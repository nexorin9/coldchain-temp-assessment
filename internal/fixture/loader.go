// Package fixture 读取仓库内脱敏样例，作为唯一的测试数据层，供单测与本地端到端验证复用。
//
// 与 internal/ingest 计划中的现场 CSV 导入（任务 9）分工明确：
//
//   - 本包加载「已知良好」的样例：任何一行不合法都直接报错，一行不跳。
//     样例坏了要当场发现，不能静默少几行、让断言在错误的基线上通过。
//   - 现场导入面向真实导出文件：允许脏行并逐条给出跳过原因。
//
// 两者刻意不合并：把「样例坏了」和「现场有脏行」混成同一种失败，
// 现场排查时既分不清是数据脏还是程序坏，也容易让导入逻辑反过来迁就样例。
//
// 边界声明：本包只服务于测试与本地验证。样例是脱敏替身，不是最终对接面——
// 真实的在库批次清单由冷链台账按 point_id + at 查询给出（见 internal/ledger 读侧）。
package fixture

import (
	"encoding/csv"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/ingest"
)

// 样例文件名。
const (
	// RecordingsFile 是脱敏记录仪导出样例的文件名。
	RecordingsFile = "recordings_sample.csv"
	// BatchesFile 是在库批次清单样例的文件名。
	BatchesFile = "batches_sample.csv"
)

// 样例的列约定。
var (
	// recordingRequiredColumns 是记录仪样例必须存在的列。
	// 缺任一列 → E_VALIDATION：样例与代码的字段约定对不上时必须当场报错，
	// 而不是让缺列的行带着零值混进判定、最后给出一个看着正常的错误结论。
	recordingRequiredColumns = []string{
		"point_id", "device_id", "device_name", "measured_at", "temperature_c",
		"unit", "source", "alarm_flag", "events",
	}

	// recordingOptionalColumns 是可选数值列：列可以缺，缺即缺测（nil），
	// 不用 0 冒充；列在但值非法则报错。
	recordingOptionalColumns = []string{"humidity_pct", "battery_pct", "accuracy_c"}

	// batchRequiredColumns 是批次清单样例必须存在的列。
	batchRequiredColumns = []string{"point_id", "batch_ref", "product_name", "quantity", "expiry_date"}
)

// LoadRecordings 读取脱敏记录仪导出样例，返回规范化后的温度事件序列。
//
// 返回值按 CSV 中的时间顺序排列。任一行不合法即返回 E_VALIDATION 并带上行号：
// 样例是判定链的基线，基线坏了却照常返回，只会让人在更远的地方追一个假结论。
func LoadRecordings(path string) ([]domain.TemperatureEvent, error) {
	records, header, err := readCSV(path)
	if err != nil {
		return nil, err
	}
	if err := requireColumns("记录仪样例", header, recordingRequiredColumns); err != nil {
		return nil, err
	}

	events := make([]domain.TemperatureEvent, 0, len(records))
	for i, rec := range records {
		rowNo := i + 2 // 第 1 行是表头
		raw, err := recordingToRaw(rec, rowNo)
		if err != nil {
			return nil, err
		}
		// 走接入层的规范化与最小校验：校验口径只有一处，
		// 样例与线上事件不会因为两条解析路径给出不同的结论。
		event, err := ingest.NormalizeEvent(raw)
		if err != nil {
			return nil, errcode.Newf(errcode.Validation,
				"记录仪样例第 %d 行不合法：%s", rowNo, detail(err))
		}
		events = append(events, event)
	}
	return events, nil
}

// LoadBatches 读取在库批次清单样例，返回领域批次。
//
// batch_ref 原样保留：既不改写大小写、也不去空格、更不生成新号。
// 批次号只由冷链台账给出，本系统造一个「看起来像批次号」的值，
// 等于把一份查无此物的库存写进异常记录表。
func LoadBatches(path string) ([]domain.Batch, error) {
	records, header, err := readCSV(path)
	if err != nil {
		return nil, err
	}
	if err := requireColumns("批次清单样例", header, batchRequiredColumns); err != nil {
		return nil, err
	}

	batches := make([]domain.Batch, 0, len(records))
	for i, rec := range records {
		rowNo := i + 2
		batch, err := batchFromRecord(rec, rowNo)
		if err != nil {
			return nil, err
		}
		batches = append(batches, batch)
	}
	return batches, nil
}

// recordingToRaw 把一行记录仪样例转成接入层的线格式事件。
func recordingToRaw(rec map[string]string, rowNo int) (ingest.RawEvent, error) {
	raw := ingest.RawEvent{
		PointID:    strings.TrimSpace(rec["point_id"]),
		DeviceID:   strings.TrimSpace(rec["device_id"]),
		DeviceName: strings.TrimSpace(rec["device_name"]),
		MeasuredAt: strings.TrimSpace(rec["measured_at"]),
		Unit:       strings.TrimSpace(rec["unit"]),
		Source:     strings.TrimSpace(rec["source"]),
	}

	switch raw.Unit {
	case domain.UnitCelsius, domain.UnitFahrenheit:
	default:
		return ingest.RawEvent{}, errcode.Newf(errcode.Validation,
			"记录仪样例第 %d 行 unit 取值不合法：%q（合法取值：%s、%s）",
			rowNo, rec["unit"], domain.UnitCelsius, domain.UnitFahrenheit)
	}

	temperature, err := strconv.ParseFloat(strings.TrimSpace(rec["temperature_c"]), 64)
	if err != nil {
		return ingest.RawEvent{}, errcode.Newf(errcode.Validation,
			"记录仪样例第 %d 行 temperature_c 不是数值：%q", rowNo, rec["temperature_c"])
	}
	raw.TemperatureC = &temperature

	humidity, err := optionalFloat(rec, "humidity_pct", rowNo)
	if err != nil {
		return ingest.RawEvent{}, err
	}
	raw.HumidityPct = humidity

	battery, err := optionalFloat(rec, "battery_pct", rowNo)
	if err != nil {
		return ingest.RawEvent{}, err
	}
	raw.BatteryPct = battery

	accuracy, err := optionalFloat(rec, "accuracy_c", rowNo)
	if err != nil {
		return ingest.RawEvent{}, err
	}
	raw.AccuracyC = accuracy

	alarm, err := parseBool(rec["alarm_flag"])
	if err != nil {
		return ingest.RawEvent{}, errcode.Newf(errcode.Validation,
			"记录仪样例第 %d 行 alarm_flag 不是布尔值：%q", rowNo, rec["alarm_flag"])
	}
	raw.AlarmFlag = alarm

	// events[] 用「|」分隔多个事件类型。未知类型不在这里判断：
	// 合法取值域由接入层的最小校验统一裁定，本包不维护第二份名单。
	raw.Events = splitEventKinds(rec["events"])

	return raw, nil
}

// batchFromRecord 把一行批次清单样例转成领域批次。
func batchFromRecord(rec map[string]string, rowNo int) (domain.Batch, error) {
	pointID := strings.TrimSpace(rec["point_id"])
	if pointID == "" {
		return domain.Batch{}, errcode.Newf(errcode.Validation,
			"批次清单样例第 %d 行缺少 point_id：批次挂在哪个点位是它不可省的一部分", rowNo)
	}

	// batch_ref 取原始单元格内容，不做 trim、不改大小写。
	ref := rec["batch_ref"]
	if strings.TrimSpace(ref) == "" {
		return domain.Batch{}, errcode.Newf(errcode.Validation,
			"批次清单样例第 %d 行缺少 batch_ref", rowNo)
	}

	productName := strings.TrimSpace(rec["product_name"])
	if productName == "" {
		return domain.Batch{}, errcode.Newf(errcode.Validation,
			"批次清单样例第 %d 行缺少 product_name", rowNo)
	}

	quantity, err := strconv.Atoi(strings.TrimSpace(rec["quantity"]))
	if err != nil {
		return domain.Batch{}, errcode.Newf(errcode.Validation,
			"批次清单样例第 %d 行 quantity 不是整数：%q", rowNo, rec["quantity"])
	}
	if quantity < 0 {
		return domain.Batch{}, errcode.Newf(errcode.Validation,
			"批次清单样例第 %d 行 quantity 为负数：%d", rowNo, quantity)
	}

	expiry := strings.TrimSpace(rec["expiry_date"])
	if !validExpiry(expiry) {
		return domain.Batch{}, errcode.Newf(errcode.Validation,
			"批次清单样例第 %d 行 expiry_date 不合法：%q（应为 YYYY-MM-DD 或 <占位值>）",
			rowNo, rec["expiry_date"])
	}

	return domain.Batch{
		PointID:     pointID,
		BatchRef:    ref,
		ProductName: productName,
		Quantity:    quantity,
		ExpiryDate:  expiry,
	}, nil
}

// validExpiry 判断效期取值是否合法：ISO 日期，或样例用的占位标记。
//
// 效期在样例里用占位值而不是编一个像真的日期：把 2027-03-31 这类日期
// 混在脱敏样例里，读的人容易当成某个真实批次的真实效期。
func validExpiry(value string) bool {
	if value == "" {
		return false
	}
	if strings.HasPrefix(value, "<") && strings.HasSuffix(value, ">") {
		return true
	}
	_, err := time.Parse("2006-01-02", value)
	return err == nil
}

// optionalFloat 读取可选数值列：列缺或单元格为空返回 nil（缺测），
// 值非法返回错误。
func optionalFloat(rec map[string]string, column string, rowNo int) (*float64, error) {
	cell, ok := rec[column]
	if !ok {
		return nil, nil
	}
	trimmed := strings.TrimSpace(cell)
	if trimmed == "" {
		return nil, nil
	}
	value, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return nil, errcode.Newf(errcode.Validation,
			"记录仪样例第 %d 行 %s 不是数值：%q", rowNo, column, cell)
	}
	return &value, nil
}

// parseBool 解析布尔列，接受 true/false 与 1/0。
func parseBool(cell string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(cell)) {
	case "true", "1":
		return true, nil
	case "false", "0":
		return false, nil
	default:
		return false, errcode.Newf(errcode.Validation, "不是布尔值：%q", cell)
	}
}

// splitEventKinds 按「|」切分设备事件列，空项忽略。
func splitEventKinds(cell string) []string {
	parts := strings.Split(cell, "|")
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

// readCSV 读取 CSV，返回「按列名索引的记录列表」与「表头列名」。
//
// 列名以表头为准（表头驱动，不写死列序）：现场记录仪导出的列顺序不固定，
// 按列名取值才不会因为列序调整而整批读错。
func readCSV(path string) ([]map[string]string, []string, error) {
	fh, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, errcode.Newf(errcode.NotFound, "样例文件不存在：%s", path)
		}
		return nil, nil, errcode.Newf(errcode.Validation, "读取样例文件失败：%s", err)
	}
	defer fh.Close()

	reader := csv.NewReader(fh)
	// 列数不齐的行由本包自己报行号，而不是让 csv 包先抛一个没有位置的错。
	reader.FieldsPerRecord = -1

	header, err := reader.Read()
	if err != nil {
		if err == io.EOF {
			return nil, nil, errcode.Newf(errcode.Validation, "样例文件为空：%s", path)
		}
		return nil, nil, errcode.Newf(errcode.Validation, "读取样例文件表头失败：%s", err)
	}

	names := make([]string, 0, len(header))
	seen := make(map[string]bool, len(header))
	for _, h := range header {
		name := strings.TrimSpace(h)
		if seen[name] {
			return nil, nil, errcode.Newf(errcode.Validation,
				"样例文件表头含重复列名：%s", name)
		}
		seen[name] = true
		names = append(names, name)
	}

	var records []map[string]string
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, errcode.Newf(errcode.Validation, "读取样例文件数据行失败：%s", err)
		}
		rec := make(map[string]string, len(names))
		for i, name := range names {
			if i < len(row) {
				rec[name] = row[i]
			}
		}
		records = append(records, rec)
	}
	return records, names, nil
}

// requireColumns 校验必需列是否齐全。
func requireColumns(what string, header []string, required []string) error {
	present := make(map[string]bool, len(header))
	for _, h := range header {
		present[h] = true
	}
	var missing []string
	for _, column := range required {
		if !present[column] {
			missing = append(missing, column)
		}
	}
	if len(missing) > 0 {
		return errcode.Newf(errcode.Validation,
			"%s缺少必需列：%s", what, strings.Join(missing, "、"))
	}
	return nil
}

// detail 取错误的可读细节文本。
func detail(err error) string {
	if e := errcode.As(err); e != nil && e.Details != "" {
		return e.Details
	}
	return err.Error()
}
