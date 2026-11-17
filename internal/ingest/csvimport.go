package ingest

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/store"
)

// 本文件是记录仪导出 CSV 的批量导入：读入 → 逐行解析 → 汇总 → 单事务落库。
//
// 现场记录仪导出的 CSV 与线上投递的单个事件不是一回事。线上事件一次一条、
// 字段齐整，是设备或网关按本服务契约拼好的；导出文件却是几千行连着十几列，
// 中间混着空值、坏时间、超量程读数与未知事件类型，还常有不同型号设备
// 导出的列序不一样。导入要处理的就是这些。
//
// 三条底线贯穿本文件：
//   - 脏行不静默丢弃：跳过的每一行都要带行号与可读原因回到调用方。
//     「导入了 80 条、丢了 4 条」而说不出丢的是哪 4 条，等于把现场数据弄丢了；
//   - 表头驱动：按列名取值，不写死列序。换一台型号不同的记录仪那天，
//     写死列序的导入器会整批读错，而且错得看不出错——数字照样进库，
//     只是温度和湿度换了位置；
//   - 干跑与实跑只差落库：dry_run 的跳过统计与导入条数必须与实跑一致。
//     否则「先干跑看看」这个动作就没有参考价值。
//
// 与 internal/fixture 的分工：fixture 加载的是「已知良好」的仓库内样例，
// 任何一行不合法都直接报错、一行不跳；本文件面对的是真实导出文件，
// 允许脏行并逐条给出跳过原因。两者刻意不合并——把「样例坏了」和
// 「现场有脏行」混成同一种失败，现场排查时分不清是数据脏还是程序坏。

// 导入跳过原因分类。机器可读，用于现场按原因分组处理；文案另见 SkipReason.Reason。
const (
	// SkipMissingColumn：必填列在表头里有，但这一行的取值为空。
	SkipMissingColumn = "missing_column"
	// SkipUnparsableTime：measured_at 不是带时区偏移的 RFC3339 时间。
	SkipUnparsableTime = "unparsable_measured_at"
	// SkipTemperatureOutOfRange：温度换算成摄氏度后越出量程。
	SkipTemperatureOutOfRange = "temperature_out_of_range"
	// SkipUnknownEventKind：events 列含未知事件类型。
	SkipUnknownEventKind = "unknown_event_kind"
	// SkipInvalidNumber：数值列取不到数值。
	SkipInvalidNumber = "invalid_number"
	// SkipInvalidBool：布尔列取值不合法。
	SkipInvalidBool = "invalid_bool"
	// SkipInvalidUnit：温度单位不合法。
	SkipInvalidUnit = "invalid_unit"
	// SkipValueOutOfRange：数值本身合法但越量程（湿度、电量、精度声明）。
	SkipValueOutOfRange = "value_out_of_range"
	// SkipInvalidRow：无法归入上面各类的行级问题（含 CSV 语法错误）。
	SkipInvalidRow = "invalid_row"
)

// importRequiredColumns 是记录仪导出文件必须具备的列。
//
// 缺任一列 → 整个导入失败（E_VALIDATION），不降级为「部分列缺失也能导」。
// 这几列是判定的最低身份信息：没有点位归不了因，没有设备名填不了异常记录表，
// 没有时间算不出越温窗口，缺一列的读数进了库也只是查不出来的垃圾。
var importRequiredColumns = []string{
	"point_id", "device_id", "device_name", "measured_at", "temperature_c", "source",
}

// importOptionalColumns 是可选列：列可以缺、值可以空，缺即缺测（nil），
// 不用 0 冒充。列在但值非法时按行跳过并说明原因。
var importOptionalColumns = []string{
	"unit", "alarm_flag", "events", "humidity_pct", "battery_pct", "accuracy_c",
}

// eventSeparator 是 events 列内多个事件类型的分隔符。
//
// 与仓库内样例保持同一口径（竖线分隔），不同型号记录仪的分隔符差异
// 属于编码与脏数据形态那一层的加固范围，不在这里分叉成多套解析路径。
const eventSeparator = "|"

// SkipReason 是一条被跳过的记录及其原因。
//
// Row 是文件里的实际行号（第 1 行是表头，数据行从 2 起）：现场拿着
// 「第 37 行有问题」去核对导出文件，比拿着「第 36 条数据有问题」快得多。
type SkipReason struct {
	// Row 是被跳过记录在导出文件里的行号。
	Row int `json:"row"`
	// Code 是机器可读的原因分类。
	Code string `json:"code"`
	// Reason 是给人看的原因文本。
	Reason string `json:"reason"`
	// PointID 与 MeasuredAt 是该行自带的归属信息，能取到就带上，方便现场定位。
	PointID    string `json:"point_id,omitempty"`
	MeasuredAt string `json:"measured_at,omitempty"`
}

// ImportResult 是一次批量导入的结果。
type ImportResult struct {
	// Imported 是新写入库里的条数；干跑时是「将会写入」的条数。
	Imported int `json:"imported"`
	// Skipped 是因数据问题被跳过的条数。
	Skipped int `json:"skipped"`
	// TotalRows 是文件里的数据行总数（不含表头）。
	TotalRows int `json:"total_rows"`
	// Duplicates 是命中幂等键、没有新写入的条数。
	Duplicates int `json:"duplicates"`
	// DryRun 标记本次是否只校验未落库。
	DryRun bool `json:"dry_run"`
	// SkipReasons 是逐条跳过原因，与 Skipped 一一对应。
	SkipReasons []SkipReason `json:"skip_reasons"`
	// Encoding 是这批文件的字符编码：utf-8 或 gbk。
	Encoding string `json:"encoding"`
	// EncodingFallback 表示原始字节不是合法 UTF-8、已回退 GBK 解析。
	EncodingFallback bool `json:"encoding_fallback"`
	// OriginalBytes 是导入文件的原始字节数。
	OriginalBytes int `json:"original_bytes"`
	// ExtraColumns 是表头里多出来的列名（厂家自有列，已忽略）。
	ExtraColumns []string `json:"extra_columns,omitempty"`
	// ExtraValues 是各行多出来的取值个数合计（行长度超过表头，已忽略）。
	ExtraValues int `json:"extra_values"`
	// MissingColumns 是表头里缺失的可选列名（该列整列没有，已按缺测处理）。
	MissingColumns []string `json:"missing_columns,omitempty"`
}

// ParsedSeries 是一段记录仪导出 CSV 解析出来的结果。
type ParsedSeries struct {
	// Events 是通过校验的温度事件，按文件中的行序排列。
	Events []domain.TemperatureEvent
	// SkipReasons 是逐条跳过原因。
	SkipReasons []SkipReason
	// TotalRows 是读到的数据行总数（不含表头）。
	TotalRows int
	// Encoding 是这批文件的字符编码：utf-8 或 gbk。
	Encoding string
	// EncodingFallback 表示原始字节不是合法 UTF-8、已回退 GBK 解析。
	EncodingFallback bool
	// OriginalBytes 是导入文件的原始字节数。
	OriginalBytes int
	// ExtraColumns 是表头里多出来的列名（厂家自有列，已忽略）。
	ExtraColumns []string
	// ExtraValues 是各行多出来的取值个数合计。
	ExtraValues int
	// MissingColumns 是表头里缺失的可选列名。
	MissingColumns []string
}

// ParseTemperatureSeries 解析记录仪导出的 CSV。
//
// 表头驱动：按列名索引取值，列序随便换都能读对；重复列名与缺必需列
// 都直接报错（E_VALIDATION），因为这两种情况下「读到的是哪一列」本身
// 就不确定，继续解析只会把错值当成真值写进库。
//
// 行级问题不中断整份文件：坏行被跳掉并记下原因，好行照常解析。
// 只有 CSV 语法坏到解析器卡住（同一行反复报错）时才整体失败。
func ParseTemperatureSeries(data []byte) (*ParsedSeries, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errcode.Newf(errcode.Validation, "导入文件为空")
	}

	// 先解码：Windows 上导出的记录仪文件十有八九是 GBK，
	// 直接按 UTF-8 读会得到乱码，设备名与点位名全废。
	// 回退发生时在事件与回执上都标出来，不悄悄换编码。
	decoded, err := DecodeSeriesBytes(data)
	if err != nil {
		return nil, err
	}

	reader := csv.NewReader(strings.NewReader(decoded.Text))
	// 列数不齐的行由本层自己判并报行号，不让 csv 包先抛一个没有位置的错。
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true

	header, err := reader.Read()
	if err != nil {
		if err == io.EOF {
			return nil, errcode.Newf(errcode.Validation, "导入文件为空")
		}
		return nil, errcode.Newf(errcode.Validation, "读取导入文件表头失败：%s", err)
	}

	columns, err := headerIndex(header)
	if err != nil {
		return nil, err
	}
	if err := requireImportColumns(columns); err != nil {
		return nil, err
	}

	out := &ParsedSeries{
		Events:           make([]domain.TemperatureEvent, 0, 256),
		SkipReasons:      make([]SkipReason, 0),
		Encoding:         decoded.Encoding,
		EncodingFallback: decoded.Fallback,
		OriginalBytes:    decoded.OriginalBytes,
		ExtraColumns:     extraHeaderColumns(header, columns),
		MissingColumns:   missingOptionalColumns(columns),
	}

	// 同一行反复报错时解析器会卡在同一处，继续读只是空转：如实报错退出，
	// 不假装「导入了 0 条」是成功。
	lastBadLine := -1
	for {
		row, readErr := reader.Read()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			line := csvErrorLine(readErr)
			if line == lastBadLine {
				return nil, errcode.Newf(errcode.Validation,
					"导入文件第 %d 行无法解析且无法跳过：%s", line, readErr)
			}
			lastBadLine = line
			out.TotalRows++
			out.SkipReasons = append(out.SkipReasons, SkipReason{
				Row:    line,
				Code:   SkipInvalidRow,
				Reason: fmt.Sprintf("第 %d 行不是合法的 CSV 记录：%s", line, readErr),
			})
			continue
		}

		out.TotalRows++
		event, skip := parseImportRow(row, columns, out.TotalRows+1, out)
		if skip != nil {
			out.SkipReasons = append(out.SkipReasons, *skip)
			continue
		}
		// 多余取值逐行累计：回执里的 extra_values 是总数，
		// 便于现场判断「多出来的是整列还是零星几格」。
		out.ExtraValues += event.DataNotes.ExtraValues
		out.Events = append(out.Events, event)
	}

	return out, nil
}

// extraHeaderColumns 返回表头里不被识别的列名。
//
// 不同型号的记录仪导出的列不一样，多出来的列（厂家自有的「柜内序号」
// 「导出批次」之类）直接忽略即可——但要报出去：
// 否则现场会以为「导进来 12 列全用上了」，实际只用了 6 列。
func extraHeaderColumns(header []string, columns map[string]int) []string {
	known := knownImportColumns()
	extra := make([]string, 0)
	for _, name := range header {
		trimmed := strings.TrimSpace(name)
		if known[trimmed] {
			continue
		}
		extra = append(extra, trimmed)
	}
	if len(extra) == 0 {
		return nil
	}
	return extra
}

// missingOptionalColumns 返回表头里缺失的可选列名。
//
// 整列都没有的可选列不是错误（那是「这台设备不报湿度」的真实情况），
// 但按缺测处理并在回执里点名，现场才知道这批数据没有哪几项。
func missingOptionalColumns(columns map[string]int) []string {
	missing := make([]string, 0)
	for _, name := range importOptionalColumns {
		if _, ok := columns[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return missing
}

// knownImportColumns 返回本服务识别的全部列名集合。
func knownImportColumns() map[string]bool {
	known := make(map[string]bool, len(importRequiredColumns)+len(importOptionalColumns))
	for _, name := range importRequiredColumns {
		known[name] = true
	}
	for _, name := range importOptionalColumns {
		known[name] = true
	}
	return known
}

// ImportTemperatureSeries 把一段记录仪导出的 CSV 导入存储。
//
// 落库是单事务：要么这批合法行全部入库，要么一行不入。
// 批处理跑到一半断电时，库里不会出现「前 300 条在、后 200 条没进」的半截状态，
// 而半截的时序算出来的越温窗口是错的。
//
// dryRun 为真时只校验不落库，但跳过统计与「将会导入多少条」都按实跑口径给出：
// 干跑要回答的是「这批文件能不能导」，那就得给得出与实跑同样的答案。
func ImportTemperatureSeries(ctx context.Context, st *store.Store, data []byte, dryRun bool) (*ImportResult, error) {
	if st == nil {
		return nil, errcode.Newf(errcode.StorageError, "事件存储未初始化")
	}

	parsed, err := ParseTemperatureSeries(data)
	if err != nil {
		return nil, err
	}

	result := &ImportResult{
		Skipped:          len(parsed.SkipReasons),
		TotalRows:        parsed.TotalRows,
		DryRun:           dryRun,
		SkipReasons:      parsed.SkipReasons,
		Encoding:         parsed.Encoding,
		EncodingFallback: parsed.EncodingFallback,
		OriginalBytes:    parsed.OriginalBytes,
		ExtraColumns:     parsed.ExtraColumns,
		ExtraValues:      parsed.ExtraValues,
		MissingColumns:   parsed.MissingColumns,
	}
	if result.SkipReasons == nil {
		result.SkipReasons = []SkipReason{}
	}

	// 批内重复：同一份文件里出现两次的同一次采样只算一条。
	// 现场导出常见于「导出时选了两段重叠的时间范围」。
	candidates := make([]domain.TemperatureEvent, 0, len(parsed.Events))
	keys := make([]string, 0, len(parsed.Events))
	seen := make(map[string]bool, len(parsed.Events))
	for _, ev := range parsed.Events {
		key := domain.EventIdempotencyKey(ev.PointID, ev.MeasuredAt)
		if seen[key] {
			result.Duplicates++
			continue
		}
		seen[key] = true
		candidates = append(candidates, ev)
		keys = append(keys, key)
	}

	if dryRun {
		// 干跑不写库，但「这批里有多少条库里已经有了」必须查出来：
		// 不查就报「将导入 N 条」，实跑时却只导进 M 条（N>M），
		// 「先干跑看看」就失去了参考价值。这里是只读查询。
		existing, err := st.ExistingIdempotencyKeys(ctx, keys)
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			if existing[key] {
				result.Duplicates++
			}
		}
		result.Imported = len(candidates) - result.Duplicates
		return result, nil
	}

	outcome, err := st.InsertEvents(ctx, candidates)
	if err != nil {
		return nil, err
	}
	result.Imported = outcome.Inserted
	result.Duplicates += outcome.Duplicates
	return result, nil
}

// headerIndex 由表头建立「列名 → 列序」索引。
//
// 重复列名直接报错：两列同名时，「读到的是哪一列」无法确定，
// 按名字取值会随机命中其中一列，读出来的数据看着正常、其实串了列。
func headerIndex(header []string) (map[string]int, error) {
	columns := make(map[string]int, len(header))
	for i, name := range header {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			return nil, errcode.Newf(errcode.Validation,
				"导入文件表头第 %d 列为空列名", i+1)
		}
		if _, exists := columns[trimmed]; exists {
			return nil, errcode.Newf(errcode.Validation,
				"导入文件表头含重复列名：%s", trimmed)
		}
		columns[trimmed] = i
	}
	return columns, nil
}

// requireImportColumns 校验必需列是否齐全。
func requireImportColumns(columns map[string]int) error {
	var missing []string
	for _, name := range importRequiredColumns {
		if _, ok := columns[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return errcode.Newf(errcode.Validation,
			"导入文件缺少必需列：%s（必需列：%s；可选列：%s）",
			strings.Join(missing, "、"),
			strings.Join(importRequiredColumns, "、"),
			strings.Join(importOptionalColumns, "、"))
	}
	return nil
}

// parseImportRow 把一行导出记录解析成温度事件；不合法时返回跳过原因。
//
// 校验顺序刻意从「能不能读懂」走到「能不能拿去判定」：
// 先看必填列齐不齐、时间解不解析得开，再看数值与事件类型，最后交给
// 接入层的 NormalizeEvent 做规范化（单位换算、精度标注）。
// 规范化走的是线上事件同一条路径，导出文件与线上投递不会给出两套结论。
func parseImportRow(row []string, columns map[string]int, rowNo int, series *ParsedSeries) (domain.TemperatureEvent, *SkipReason) {
	notes := &rowNotes{}

	// 表头里整列就没有的可选列：这一列对本次导入全程缺测。
	for _, name := range series.MissingColumns {
		notes.markMissing(name)
	}
	// 行长度超过表头：多出来的取值没有任何列名对应，忽略但计数。
	if extra := len(row) - len(columns); extra > 0 {
		notes.extra = extra
	}
	// 行长度不足表头：靠后的列在这一行没有取值，按缺测处理。
	for name, idx := range columns {
		if idx >= len(row) {
			notes.markMissing(name)
		}
	}
	// 可选列的单元格为空：这一项是缺测。保持 nil，不拿 0 冒充，
	// 并把字段名记进 missing_fields。
	for _, name := range importOptionalColumns {
		idx, ok := columns[name]
		if !ok || idx >= len(row) {
			continue
		}
		if strings.TrimSpace(row[idx]) == "" {
			notes.markMissing(name)
		}
	}

	for _, name := range importRequiredColumns {
		if strings.TrimSpace(cellOf(row, columns, name)) == "" {
			return domain.TemperatureEvent{}, &SkipReason{
				Row:    rowNo,
				Code:   SkipMissingColumn,
				Reason: fmt.Sprintf("第 %d 行缺少必填列 %s 的取值", rowNo, name),
			}
		}
	}

	pointID := cellText(row, columns, "point_id", maxPointIDRunes, notes)
	measuredRaw := cellText(row, columns, "measured_at", 0, notes)

	unitRaw := cellText(row, columns, "unit", maxUnitRunes, notes)
	unit, err := NormalizeUnit(unitRaw)
	if err != nil {
		return domain.TemperatureEvent{}, &SkipReason{
			Row:        rowNo,
			Code:       SkipInvalidUnit,
			Reason:     fmt.Sprintf("第 %d 行 unit 取值不合法：%q", rowNo, unitRaw),
			PointID:    pointID,
			MeasuredAt: measuredRaw,
		}
	}

	if _, err := ParseMeasuredAt(measuredRaw); err != nil {
		return domain.TemperatureEvent{}, &SkipReason{
			Row:        rowNo,
			Code:       SkipUnparsableTime,
			Reason:     fmt.Sprintf("第 %d 行 measured_at 不可解析：%q（需带时区偏移的 RFC3339 时间）", rowNo, measuredRaw),
			PointID:    pointID,
			MeasuredAt: measuredRaw,
		}
	}

	// 数值列做全角转半角：中文 Windows 的仪器上位机会把温度写成全角，
	// 不转的话整批数据会被当成坏行跳过。
	tempRaw := cellNumber(row, columns, "temperature_c", notes)
	temperature, err := strconv.ParseFloat(tempRaw, 64)
	if err != nil {
		return domain.TemperatureEvent{}, &SkipReason{
			Row:        rowNo,
			Code:       SkipInvalidNumber,
			Reason:     fmt.Sprintf("第 %d 行 temperature_c 不是数值：%q", rowNo, tempRaw),
			PointID:    pointID,
			MeasuredAt: measuredRaw,
		}
	}

	// 量程按摄氏度判定：先换算再比。否则一台以华氏度上报的记录仪
	// 报 200℉（合 93.3℃）会被误判成越量程。
	celsius, err := ConvertToCelsius(temperature, unit)
	if err != nil {
		return domain.TemperatureEvent{}, &SkipReason{
			Row:        rowNo,
			Code:       SkipInvalidRow,
			Reason:     fmt.Sprintf("第 %d 行温度无法换算：%s", rowNo, err),
			PointID:    pointID,
			MeasuredAt: measuredRaw,
		}
	}
	if celsius < domain.MinTemperatureC || celsius > domain.MaxTemperatureC {
		return domain.TemperatureEvent{}, &SkipReason{
			Row:  rowNo,
			Code: SkipTemperatureOutOfRange,
			Reason: fmt.Sprintf("第 %d 行温度越量程：%s（换算后 %.2f℃，允许量程 [%.0f, %.0f]）",
				rowNo, tempRaw, celsius, domain.MinTemperatureC, domain.MaxTemperatureC),
			PointID:    pointID,
			MeasuredAt: measuredRaw,
		}
	}

	humidity, skip := importOptionalNumber(row, columns, "humidity_pct", rowNo, 0, 100, notes)
	if skip != nil {
		skip.PointID, skip.MeasuredAt = pointID, measuredRaw
		return domain.TemperatureEvent{}, skip
	}
	battery, skip := importOptionalNumber(row, columns, "battery_pct", rowNo, 0, 100, notes)
	if skip != nil {
		skip.PointID, skip.MeasuredAt = pointID, measuredRaw
		return domain.TemperatureEvent{}, skip
	}
	// 精度声明只能是正数：声明「我的精度是 0℃」说明这台设备没填这一项，
	// 而不是它真的那么准。
	accuracy, skip := importOptionalNumber(row, columns, "accuracy_c", rowNo, 0, 0, notes)
	if skip != nil {
		skip.PointID, skip.MeasuredAt = pointID, measuredRaw
		return domain.TemperatureEvent{}, skip
	}
	if accuracy != nil && *accuracy <= 0 {
		return domain.TemperatureEvent{}, &SkipReason{
			Row:        rowNo,
			Code:       SkipValueOutOfRange,
			Reason:     fmt.Sprintf("第 %d 行 accuracy_c=%.2f 不是合法的测温精度声明（必须为正数）", rowNo, *accuracy),
			PointID:    pointID,
			MeasuredAt: measuredRaw,
		}
	}

	alarm := false
	if alarmRaw := cellText(row, columns, "alarm_flag", 0, notes); alarmRaw != "" {
		parsed, err := parseImportBool(alarmRaw)
		if err != nil {
			return domain.TemperatureEvent{}, &SkipReason{
				Row:        rowNo,
				Code:       SkipInvalidBool,
				Reason:     fmt.Sprintf("第 %d 行 alarm_flag 不是布尔值：%q", rowNo, alarmRaw),
				PointID:    pointID,
				MeasuredAt: measuredRaw,
			}
		}
		alarm = parsed
	}

	eventCell := cellText(row, columns, "events", maxEventsRunes, notes)
	eventKinds := splitImportEvents(eventCell)
	if unknown := UnknownEventKinds(eventKinds); len(unknown) > 0 {
		return domain.TemperatureEvent{}, &SkipReason{
			Row:  rowNo,
			Code: SkipUnknownEventKind,
			Reason: fmt.Sprintf("第 %d 行 events 含未知事件类型：%s（合法取值：%s）",
				rowNo, strings.Join(unknown, "、"), strings.Join(domain.KnownEventKinds, "、")),
			PointID:    pointID,
			MeasuredAt: measuredRaw,
		}
	}

	raw := RawEvent{
		PointID:      pointID,
		DeviceID:     cellText(row, columns, "device_id", maxDeviceIDRunes, notes),
		DeviceName:   cellText(row, columns, "device_name", maxDeviceNameRunes, notes),
		MeasuredAt:   measuredRaw,
		TemperatureC: &temperature,
		Unit:         unitRaw,
		HumidityPct:  humidity,
		BatteryPct:   battery,
		AccuracyC:    accuracy,
		Source:       cellText(row, columns, "source", maxSourceRunes, notes),
		AlarmFlag:    alarm,
		Events:       eventKinds,
	}

	event, err := NormalizeEvent(raw)
	if err != nil {
		return domain.TemperatureEvent{}, &SkipReason{
			Row:        rowNo,
			Code:       SkipInvalidRow,
			Reason:     fmt.Sprintf("第 %d 行未通过接入校验：%s", rowNo, errorDetail(err)),
			PointID:    pointID,
			MeasuredAt: measuredRaw,
		}
	}

	// 形态标注随事件落库：后面出结论的人需要知道这份数据是什么成色。
	event.DataNotes = notes.summary(series.Encoding, series.EncodingFallback)
	return event, nil
}

// importOptionalNumber 读取可选数值列。
//
// 列缺或单元格为空 → nil（缺测），不拿 0 冒充。
// min/max 都为 0 时表示不做量程检查（精度声明走单独判定）。
func importOptionalNumber(row []string, columns map[string]int, name string, rowNo int, min, max float64, notes *rowNotes) (*float64, *SkipReason) {
	cell := cellNumber(row, columns, name, notes)
	if cell == "" {
		return nil, nil
	}
	value, err := strconv.ParseFloat(cell, 64)
	if err != nil {
		return nil, &SkipReason{
			Row:    rowNo,
			Code:   SkipInvalidNumber,
			Reason: fmt.Sprintf("第 %d 行 %s 不是数值：%q", rowNo, name, cell),
		}
	}
	if max > min && (value < min || value > max) {
		return nil, &SkipReason{
			Row:    rowNo,
			Code:   SkipValueOutOfRange,
			Reason: fmt.Sprintf("第 %d 行 %s=%.2f 超出量程 [%.0f, %.0f]", rowNo, name, value, min, max),
		}
	}
	return &value, nil
}

// cellOf 按列名取单元格取值；列不存在或该行长度不足时返回空串。
func cellOf(row []string, columns map[string]int, name string) string {
	idx, ok := columns[name]
	if !ok || idx >= len(row) {
		return ""
	}
	return row[idx]
}

// parseImportBool 解析布尔列，接受 true/false 与 1/0。
func parseImportBool(cell string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(cell)) {
	case "true", "1":
		return true, nil
	case "false", "0":
		return false, nil
	default:
		return false, fmt.Errorf("不是布尔值：%q", cell)
	}
}

// splitImportEvents 按分隔符切分设备事件列，空项忽略。
func splitImportEvents(cell string) []string {
	parts := strings.Split(cell, eventSeparator)
	events := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		events = append(events, p)
	}
	if len(events) == 0 {
		return nil
	}
	return events
}

// csvErrorLine 从 csv 解析错误里取出行号；取不到时返回 0。
func csvErrorLine(err error) int {
	var parseErr *csv.ParseError
	if errors.As(err, &parseErr) {
		return parseErr.Line
	}
	return 0
}

// errorDetail 取错误的可读细节文本。
func errorDetail(err error) string {
	if e := errcode.As(err); e != nil && e.Details != "" {
		return e.Details
	}
	return err.Error()
}
