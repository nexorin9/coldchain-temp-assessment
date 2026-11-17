// Package drafting 把判定结论成稿成《疫苗储存和运输温度异常情况记录表》的文本草稿。
//
// 它只做一件事：把结构化产物（越温窗口、归因结论、记录表载荷）拼成一份人能读、
// 能递出去的表。它不判定、不推进业务状态、不写台账——批次到底能不能用，
// 由疫苗生产企业评估质量影响、再由药学部主管确认决定。
//
// 三条纪律贯穿整个文件：
//
//  1. **每个栏目都能指回领域对象**。稿子里的每个数值都来自某个产物字段；
//     产物里没有的，稿子里写「待补」，绝不填一个像样的值。
//     填错的数字比空着的栏目危险得多：空着看得见，填错看不出来。
//  2. **稿头写明用途与复核位**。这张表会被递到疫苗生产企业面前，
//     收件人必须第一眼就知道它是系统草稿、还没有人复核过。
//  3. **证据要带数值与阈值**。「是开门导致的」不是证据，
//     「累计开门 10 分钟（阈值 5 分钟）」才是。
//
// 它与源产品机制的关系：源仓库的契约转换器（tools/convert-openapi）
// 在把外部契约搬进新契约时要求「每个样例值都必须能落到一个已存在的栏位，
// 落不进去就报错」（example references unknown path / unknown operation /
// unknown response code），绝不凭空多出一条。搬到成稿这里就是同一件事：
// **每个栏目都必须能指回一个领域字段，取不到就写「待补」，不许编**。
package drafting

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/model"
)

// Title 是成稿的标题。
const Title = "疫苗储存和运输温度异常情况记录表"

// PurposeStatement 是稿头的用途声明。
//
// 这句话不能省，也不能改软：这张表要递到疫苗生产企业与药事会面前，
// 收件人必须第一眼就知道它是系统草稿、还没有人复核过，
// 以及「批次能不能用」并不由这张表回答。
const PurposeStatement = "用途声明：本表由冷链温控判定系统自动生成，属草稿；" +
	"须经药学部主管复核并提交疫苗生产企业评估质量影响后，再由药学部主管确认处置。" +
	"本表不构成批次放行结论。"

// ReviewStatement 是稿尾的人工复核位说明。
const ReviewStatement = "本表为系统生成的草稿，须经药学部主管复核并提交疫苗生产企业评估"

// PendingText 是「待补」占位文案。
//
// 它不是省略号，也不是空白：留白会被读成「这一栏不用填」，
// 而「待补」明确告诉复核的人这一栏还缺东西。
const PendingText = "待补"

// Inputs 是成稿所需的输入。
//
// 三样输入都是判定链上的步骤产物：取数产物、窗口产物、归因产物，
// 外加成稿步骤自己拼出来的记录表载荷。成稿只读它们，不改它们。
type Inputs struct {
	// Record 是记录表载荷：结构化字段（点位、起止、峰值、温区、批次、处置…）。
	Record *domain.ExceptionRecordPayload
	// Series 是取数产物：这次判定实际依据的那批温度采样。
	Series *domain.SeriesPayload
	// Window 是越温窗口产物：窗口本身、温区来源、精度与实体绑定结论。
	Window *domain.WindowPayload
	// Attribution 是归因产物：原因结论、候选集、证据与置信度。
	Attribution *domain.AttributionPayload
	// Advice 是批次可用性建议：有它时，批次栏与处置栏按逐批次建议渲染。
	//
	// 没有它时退回记录表载荷自带的批次与处置字段（成稿仍可出稿，
	// 只是批次栏只列事实、不给建议）。
	Advice *domain.DispositionAdvice
}

// Options 是成稿的可选参数。
type Options struct {
	// Now 是生成时间时钟；nil 时用 time.Now。
	//
	// 记录表载荷里已经带了生成时间时，以载荷里的为准——
	// 成稿是在判定那一刻生成的，不是渲染这一刻。
	Now func() time.Time
}

// Field 是成稿里的一个栏目。
//
// Source 是这个栏目的取值出处（领域对象路径）。它不是装饰：
// 复核的人问「这个 12.4℃ 是哪来的」，答案应当是一行可核对的事实，
// 而不是「系统算的」。
type Field struct {
	Label  string `json:"label"`
	Value  string `json:"value"`
	Source string `json:"source"`
}

// Draft 是一份成稿草稿。
type Draft struct {
	// Text 是成稿正文（可直接打印或写入附件）。
	Text string `json:"text"`
	// Fields 是正文里每个栏目的取值与出处。
	Fields []Field `json:"fields"`
}

// builder 是成稿的拼装器：一边写正文，一边记栏目出处。
type builder struct {
	lines  []string
	fields []Field
}

func (b *builder) raw(format string, args ...any) {
	b.lines = append(b.lines, fmt.Sprintf(format, args...))
}

// field 写一个有出处的栏目。
func (b *builder) field(label, value, source string) {
	if strings.TrimSpace(value) == "" {
		value = PendingText
	}
	b.lines = append(b.lines, "    "+label+"："+value)
	b.fields = append(b.fields, Field{Label: label, Value: value, Source: source})
}

// section 写一个小节标题。
func (b *builder) section(title string) {
	b.lines = append(b.lines, "", title)
}

// Render 把判定产物渲染成《疫苗储存和运输温度异常情况记录表》文本草稿。
//
// 缺记录表载荷时返回 E_VALIDATION：没有记录表就没有稿子，
// 这里不返回一份空壳表格——空壳会被当成「要素齐全」递出去。
func Render(in Inputs, opts Options) (Draft, error) {
	if in.Record == nil {
		return Draft{}, errcode.Newf(errcode.Validation, "成稿缺少记录表载荷（ExceptionRecordPayload）")
	}
	rec := in.Record

	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	generatedAt := rec.GeneratedAt
	if generatedAt.IsZero() {
		generatedAt = now()
	}

	b := &builder{}
	b.raw("%s（草稿）", Title)
	b.raw(strings.Repeat("=", 46))

	// 顶部三条提示：精度不足、结论来自规则基线、还有哪些要素待补。
	//
	// 放在最上面是有理由的：这张表会被递到疫苗生产企业面前，收件人打开第一眼
	// 看到的应该是「这份稿子有什么 caveats」，而不是标题下面一段用途声明。
	// 塞在末尾当附注的提示，等于没有提示。
	renderQualityNotices(b, CheckQuality(in, generatedAt))

	b.raw(PurposeStatement)

	// 一、监测点位与设备
	b.section("一、监测点位与设备")
	b.field("点位编号", rec.PointID, "domain.ExceptionRecordPayload.PointID")
	b.field("设备名称", rec.DeviceName, "domain.ExceptionRecordPayload.DeviceName")
	if deviceID := deviceIDOf(in.Series); deviceID != "" {
		b.field("记录仪标识", deviceID, "domain.SeriesPayload.Samples[].DeviceID")
	}

	// 二、温度异常情况
	b.section("二、温度异常情况")
	b.field("越限起始时间", formatTime(rec.ExcursionStartedAt), "domain.ExceptionRecordPayload.ExcursionStartedAt")
	b.field("越限结束时间", formatTime(rec.ExcursionEndedAt), "domain.ExceptionRecordPayload.ExcursionEndedAt")
	b.field("越限持续时长", formatMinutes(rec.DurationMinutes), "domain.ExceptionRecordPayload.DurationMinutes")
	b.field("峰值温度", formatTemperature(rec.PeakTemperatureC), "domain.ExceptionRecordPayload.PeakTemperatureC")
	if in.Window != nil && !in.Window.Window.PeakAt.IsZero() {
		b.field("峰值出现时刻", formatTime(in.Window.Window.PeakAt), "domain.WindowPayload.Window.PeakAt")
	}
	b.field("所属温区", rec.Zone, "domain.ExceptionRecordPayload.Zone")
	if in.Window != nil {
		if source := zoneSourceLabel(in.Window.ZoneSource); source != "" {
			value := source
			if in.Window.ZoneBasis != "" {
				value += "（依据：" + in.Window.ZoneBasis + "）"
			}
			b.field("温区来源", value, "domain.WindowPayload.ZoneSource/ZoneBasis")
		}
	}

	// 三、原因结论与判定依据
	b.section("三、原因结论与判定依据")
	b.field("原因结论", causeConclusionText(rec, in.Attribution), "domain.AttributionPayload.CauseCode/CauseLabel")
	if in.Attribution != nil {
		b.field("判定置信度", formatNumber(in.Attribution.Confidence), "domain.AttributionPayload.Confidence")
	}
	evidence := evidenceOf(rec, in.Attribution)
	if len(evidence) == 0 {
		b.field("证据摘要", PendingText+"（本次判定没有可回溯的证据）", "domain.AttributionPayload.Evidence")
	} else {
		b.lines = append(b.lines, "    证据摘要：")
		for i, ev := range evidence {
			b.lines = append(b.lines, "      "+fmt.Sprintf("%d) ", i+1)+evidenceLine(ev))
		}
		b.fields = append(b.fields, Field{
			Label:  "证据摘要",
			Value:  fmt.Sprintf("%d 条证据", len(evidence)),
			Source: "domain.AttributionPayload.Evidence",
		})
	}
	if len(rec.Notes) > 0 {
		b.lines = append(b.lines, "    口径说明：")
		for _, note := range rec.Notes {
			b.lines = append(b.lines, "      - "+note)
		}
		b.fields = append(b.fields, Field{
			Label:  "口径说明",
			Value:  strings.Join(rec.Notes, "；"),
			Source: "domain.ExceptionRecordPayload.Notes",
		})
	}

	// 四、涉及批次
	//
	// 有逐批次建议时，每一批连同「自己的温区、暴露多久、建议怎么办」一起写出来：
	// 只列批次号，复核的人还得自己去查这批货的温区，算没给出建议。
	b.section("四、涉及批次")
	if in.Advice != nil && len(in.Advice.BatchAdvices) > 0 {
		for i, ba := range in.Advice.BatchAdvices {
			value := fmt.Sprintf("批次号 %s｜品名 %s｜数量 %d｜有效期 %s",
				ba.BatchRef, ba.ProductName, ba.Quantity, ba.ExpiryDate)
			b.field(fmt.Sprintf("涉及批次 %d", i+1), value,
				fmt.Sprintf("domain.DispositionAdvice.BatchAdvices[%d]", i))
			b.lines = append(b.lines, "      该批次温区："+batchZoneText(ba))
			b.lines = append(b.lines, "      暴露时长："+batchExposureText(ba))
			b.lines = append(b.lines, "      批次可用性建议："+dispositionText(ba.Disposition)+"——"+ba.AdviceText)
		}
	} else if len(rec.Batches) == 0 {
		value, source := batchSectionValue(rec)
		b.field("涉及批次", value, source)
	} else {
		for i, batch := range rec.Batches {
			value := fmt.Sprintf("批次号 %s｜品名 %s｜数量 %d｜有效期 %s",
				batch.BatchRef, batch.ProductName, batch.Quantity, batch.ExpiryDate)
			b.field(fmt.Sprintf("涉及批次 %d", i+1), value,
				fmt.Sprintf("domain.ExceptionRecordPayload.Batches[%d]", i))
		}
	}

	// 五、处置建议
	b.section("五、处置建议")
	if in.Advice != nil {
		disposition := dispositionText(in.Advice.Disposition)
		if strings.TrimSpace(in.Advice.Disposition) == "" {
			disposition = PendingText + "（处置结论尚未给出）"
		}
		b.field("处置建议", disposition+"（最终处置待药学部主管确认后确定）",
			"domain.DispositionAdvice.Disposition")
		if strings.TrimSpace(in.Advice.Note) != "" {
			b.field("处置说明", in.Advice.Note, "domain.DispositionAdvice.Note")
		}
		if in.Advice.WithheldDisposition != "" {
			b.field("上游处置取值（未作为建议给出）",
				dispositionText(in.Advice.WithheldDisposition),
				"domain.DispositionAdvice.WithheldDisposition")
		}
		if len(in.Advice.FilteredPhrases) > 0 {
			b.field("越权措辞过滤", strings.Join(in.Advice.FilteredPhrases, "、"),
				"domain.DispositionAdvice.FilteredPhrases")
		}
	} else if strings.TrimSpace(rec.Disposition) == "" {
		b.field("处置建议",
			PendingText+"（由批次可用性建议与药学部主管确认后填写）",
			"domain.ExceptionRecordPayload.Disposition")
	} else {
		b.field("处置建议", dispositionText(rec.Disposition), "domain.ExceptionRecordPayload.Disposition")
		if strings.TrimSpace(rec.DispositionNote) != "" {
			b.field("处置说明", rec.DispositionNote, "domain.ExceptionRecordPayload.DispositionNote")
		}
	}

	// 六、数据来源与生成时间
	b.section("六、数据来源与生成时间")
	if source := sampleSourceText(in.Series); source != "" {
		b.field("数据来源", source, "domain.SeriesPayload.Samples[].Source")
	}
	if in.Series != nil {
		b.field("采样条数", fmt.Sprintf("%d 条", in.Series.SampleCount), "domain.SeriesPayload.SampleCount")
		if !in.Series.From.IsZero() || !in.Series.To.IsZero() {
			b.field("取数区间", formatRange(in.Series.From, in.Series.To), "domain.SeriesPayload.From/To")
		}
	}
	if in.Window != nil && in.Window.Window.SamplingIntervalSec > 0 {
		value := fmt.Sprintf("%d 秒", in.Window.Window.SamplingIntervalSec)
		if label := intervalSourceLabel(in.Window.Window.IntervalSource); label != "" {
			value += "（来源：" + label + "）"
		}
		b.field("采样周期口径", value, "domain.WindowPayload.Window.SamplingIntervalSec/IntervalSource")
	}
	for _, line := range dataQualityLines(in.Window) {
		b.lines = append(b.lines, "    "+line)
	}
	b.field("生成时间", formatTime(generatedAt), "domain.ExceptionRecordPayload.GeneratedAt")
	// 生成方式不是领域字段，是这份稿子自身的性质，因此不进栏目出处表。
	b.raw("    生成方式：系统自动成稿（草稿，未经人工复核）")

	// 七、人工复核位
	b.section("七、人工复核（须由药学部主管填写）")
	b.raw("    复核人（药学部主管）：____________________")
	b.raw("    复核日期：____________________")
	b.raw("    复核结论（同意 / 退回补充 / 另附说明）：____________________")
	b.raw("    送疫苗生产企业评估（企业名称 / 受理日期）：____________________")
	b.raw("")
	b.raw("    " + ReviewStatement + "。")

	return Draft{Text: strings.Join(b.lines, "\n"), Fields: b.fields}, nil
}

// renderQualityNotices 把要紧的三条提示写在稿子最上面。
//
// 三条分别是：
//
//   - **精度不足**：读数本身不可信时，后面的峰值、持续分钟都只是线索；
//   - **结论来自规则基线**：结论做了，但没被模型交叉印证过；
//   - **还有哪些要素待补**：让人一眼知道这份稿子还差什么、差的那几项
//     要不要先补齐再递出去。
//
// 三条都用 b.field 写，因此每条都带着取值出处——
// 「精度不足，仅供参考」这句话同样得说得清是从哪儿来的。
func renderQualityNotices(b *builder, q Quality) {
	if q.PrecisionLimited {
		value := "【" + q.PrecisionTag + "】"
		if strings.TrimSpace(q.PrecisionDetail) != "" {
			value += q.PrecisionDetail
		} else {
			value += "本表中的温度数值仅供参考，须以现场实测复核。"
		}
		b.field("精度提示", value, "domain.WindowPayload.Precision")
	}

	if q.Degraded {
		value := "【" + DegradeNotice + "】"
		if strings.TrimSpace(q.DegradeReason) != "" {
			value += "（模型归因不可用：" + q.DegradeReason + "）"
		}
		b.field("归因来源提示", value, "domain.AttributionPayload.Degraded")
	}

	if len(q.MissingFields) > 0 {
		value := "本表要素未齐备，待补：" + strings.Join(elementLabels(q.MissingFields), "、")
		if len(q.BlockingFields) > 0 {
			// 挡写回的那几项要点名：写清楚哪几项没补齐就不能进台账。
			value += "（其中 " + strings.Join(elementLabels(q.BlockingFields), "、") +
				" 未补齐前本表不得作为台账写回依据）"
		} else {
			// 只是待人工补齐的那几项：不挡写回，写清楚是「等人补」而不是「不能用」。
			value += "（须经药学部主管复核补齐；这几项补齐前不影响本表记入冷链台账）"
		}
		b.field("待补要素", value, "domain.ExceptionRecordPayload.MissingFields")
	}
}

// elementLabels 把要素名翻成中文栏目名。
//
// 缺项清单要给人看：「point_id」对复核的人没有意义，「点位编号」才有。
// 翻不出来的名字原样返回——不认识的要素名说明清单与要素表对不上，
// 这种错要看得见，不该被悄悄吞掉。
func elementLabels(names []string) []string {
	labels := make([]string, 0, len(names))
	for _, name := range names {
		label := ""
		for _, element := range Elements {
			if element.Name == name {
				label = element.Label
				break
			}
		}
		if label == "" {
			label = name
		}
		labels = append(labels, label)
	}
	return labels
}

// batchSectionValue 渲染「涉及批次」这一栏在批次清单拿不到时该写什么。
//
// 三种情形写三句话，因为它们对复核的人意味着三件不同的事：
//
//   - 没查成（台账不可达 / 凭据被拒 / 批次归属有歧义）：
//     写「批次信息缺失，待人工核对」并说明原因——
//     这一栏得有人去冷链台账查了补上，不是「不用填」。
//   - 查成了但确实没有在库批次：照实写「无在库批次」。
//   - 压根没接过台账读侧：沿用「待补」，并说清批次号只能由台账给。
//
// 把后两种混成第一种（或反过来）都会出事：把「库里确实没货」写成「待人工核对」，
// 会让人白跑一趟台账；把「没查成」写成「无在库批次」，
// 则等于把一次查不到说成了一次查到了空的结果。
func batchSectionValue(rec *domain.ExceptionRecordPayload) (string, string) {
	lookup := rec.BatchLookup
	if lookup == nil {
		return PendingText + "（批次号只能由冷链台账提供，系统不生成批次号）",
			"domain.ExceptionRecordPayload.Batches"
	}
	switch lookup.Status {
	case domain.BatchLookupUnavailable:
		value := domain.BatchMissingText
		if detail := strings.TrimSpace(lookup.Detail); detail != "" {
			value += "（" + detail + "）"
		}
		return value, "domain.ExceptionRecordPayload.BatchLookup"
	case domain.BatchLookupAmbiguous:
		value := domain.BatchMissingText
		if detail := strings.TrimSpace(lookup.Detail); detail != "" {
			value += "（" + detail + "）"
		}
		return value, "domain.ExceptionRecordPayload.BatchLookup"
	case domain.BatchLookupEmpty:
		return "该点位该时刻无在库批次（冷链台账查询结果）",
			"domain.ExceptionRecordPayload.BatchLookup"
	default:
		return PendingText + "（批次号只能由冷链台账提供，系统不生成批次号）",
			"domain.ExceptionRecordPayload.Batches"
	}
}

// evidenceLine 渲染一条证据：特征名 + 数值 + 阈值（+ 说明）。
//
// 三个要素一个都不能少：只有特征名看不出判据，只有数值看不出门槛，
// 两者都没有就只是「系统这么说」。
func evidenceLine(ev domain.AttributionEvidence) string {
	line := fmt.Sprintf("%s = %s（阈值 %s）", ev.Feature, formatNumber(ev.Value), formatNumber(ev.Threshold))
	if strings.TrimSpace(ev.Note) != "" {
		line += "—— " + ev.Note
	}
	return line
}

// causeConclusionText 渲染原因结论：中文短标签 + 原因代码。
func causeConclusionText(rec *domain.ExceptionRecordPayload, attribution *domain.AttributionPayload) string {
	code := strings.TrimSpace(rec.CauseConclusion)
	label := ""
	if attribution != nil {
		label = strings.TrimSpace(attribution.CauseLabel)
		if code == "" {
			code = strings.TrimSpace(attribution.CauseCode)
		}
	}
	switch {
	case label != "" && code != "":
		return label + "（" + code + "）"
	case label != "":
		return label
	case code != "":
		return code
	default:
		return ""
	}
}

// evidenceOf 取本次要写进稿子的证据：优先取归因产物上的，
// 没有归因产物时退回记录表载荷自带的。
func evidenceOf(rec *domain.ExceptionRecordPayload, attribution *domain.AttributionPayload) []domain.AttributionEvidence {
	if attribution != nil && len(attribution.Evidence) > 0 {
		return attribution.Evidence
	}
	return rec.Evidence
}

// dispositionText 渲染处置建议：中文短标签 + 取值代码。
func dispositionText(code string) string {
	if label, ok := model.DispositionLabel(code); ok {
		return label + "（" + code + "）"
	}
	return code
}

// batchZoneText 渲染该批次自己的温区与出处。
//
// 温区取不到时写「待补」并说明原因：拿点位温区顶替制品温区，
// 等于把温区要求更松的那批货也判成越限。
func batchZoneText(ba domain.BatchAdvice) string {
	if strings.TrimSpace(ba.Zone) == "" {
		return PendingText + "（该批次的制品温区未在点位温区配置中登记，系统不拿点位温区顶替）"
	}
	if strings.TrimSpace(ba.ZoneBasis) != "" {
		return ba.Zone + "（依据：" + ba.ZoneBasis + "）"
	}
	return ba.Zone
}

// batchExposureText 渲染该批次在本次越温窗口内的暴露时长与峰值偏离。
func batchExposureText(ba domain.BatchAdvice) string {
	exposure := "未知"
	if ba.ExposureMinutes != nil {
		exposure = fmt.Sprintf("%d 分钟", *ba.ExposureMinutes)
	}
	deviation := fmt.Sprintf("峰值相对该批次温区偏离 %s ℃", formatNumber(ba.DeviationC))
	if ba.Affected {
		deviation += "（已越出该批次温区）"
	}
	return exposure + "；" + deviation
}

// deviceIDOf 取采样里的记录仪标识；没有就留空，不拿点位标识顶替。
func deviceIDOf(series *domain.SeriesPayload) string {
	if series == nil {
		return ""
	}
	for _, sample := range series.Samples {
		if sample.DeviceID != "" {
			return sample.DeviceID
		}
	}
	return ""
}

// sampleSourceText 汇总这批读数的来源。
func sampleSourceText(series *domain.SeriesPayload) string {
	if series == nil {
		return ""
	}
	seen := map[string]bool{}
	var labels []string
	for _, sample := range series.Samples {
		if sample.Source == "" || seen[sample.Source] {
			continue
		}
		seen[sample.Source] = true
		labels = append(labels, sampleSourceLabel(sample.Source))
	}
	return strings.Join(labels, "、")
}

// sampleSourceLabel 把来源代码译成现场说法。
func sampleSourceLabel(source string) string {
	switch source {
	case "recorder":
		return "记录仪上报"
	case "edge-gateway":
		return "院内边缘网关上报"
	case "manual-upload":
		return "人工上传"
	default:
		return source
	}
}

// zoneSourceLabel 把温区来源代码译成现场说法。
func zoneSourceLabel(source string) string {
	switch source {
	case domain.ZoneSourcePointProfile:
		return "点位温区配置"
	case domain.ZoneSourceProductManual:
		return "制品说明书条目"
	default:
		return source
	}
}

// intervalSourceLabel 把采样周期来源代码译成现场说法。
func intervalSourceLabel(source string) string {
	switch source {
	case domain.IntervalSourcePointProfile:
		return "点位温区配置的标称值"
	case domain.IntervalSourceInferred:
		return "由相邻采样间隔中位数推断"
	case domain.IntervalSourceWallClock:
		return "墙钟折算（点位未配置标称周期）"
	default:
		return source
	}
}

// dataQualityLines 渲染这批读数本身的质量提示。
//
// 这些提示不是「附注」而是「事实」：读数缺了一段、记录仪钟回拨过、
// 探头卡死过——这三种情况都会让「峰值 12.4℃ 持续 15 分钟」这句话
// 的可信度打折，递到企业面前之前必须写在纸上。
func dataQualityLines(window *domain.WindowPayload) []string {
	if window == nil {
		return nil
	}
	w := window.Window
	var lines []string
	if w.BreakpointCount > 0 {
		minutes := 0
		for _, gap := range w.DataGaps {
			minutes += gap.GapMinutes
		}
		lines = append(lines, fmt.Sprintf("数据质量提示：窗口内有 %d 处数据缺口（约 %d 分钟），缺口处未连线", w.BreakpointCount, minutes))
	}
	if len(w.ClockRegressions) > 0 {
		lines = append(lines, fmt.Sprintf("数据质量提示：检测到记录仪时钟回拨 %d 次，已按记录仪序重排", len(w.ClockRegressions)))
	}
	if len(w.ProbeStuckSpans) > 0 {
		samples := 0
		for _, span := range w.ProbeStuckSpans {
			samples += span.SampleCount
		}
		lines = append(lines, fmt.Sprintf("数据质量提示：检测到探头卡死 %d 条采样，卡死读数未计入温升速率", samples))
	}
	return lines
}

// formatTime 渲染时间：本地时区的「年-月-日 时:分」加时区偏移。
//
// 偏移必须带：冷链记录仪的钟不一定准，现场写的「03:15」到底是哪一时区，
// 不写清楚，跨地市互认时就得靠猜。
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04") + "（" + t.Format("-07:00") + "）"
}

// formatRange 渲染时间区间。
func formatRange(from, to time.Time) string {
	if from.IsZero() && to.IsZero() {
		return ""
	}
	return formatTime(from) + " ~ " + formatTime(to)
}

// formatMinutes 渲染持续分钟数。
func formatMinutes(minutes int) string {
	if minutes <= 0 {
		return ""
	}
	return fmt.Sprintf("%d 分钟", minutes)
}

// formatTemperature 渲染温度值（摄氏度）。
func formatTemperature(c float64) string {
	return formatNumber(c) + " ℃"
}

// formatNumber 渲染数值：去掉多余的尾零。
//
// 12.4 就是 12.4，0.9 就是 0.9——写成 12.400000 或 0.900000，
// 在递出去的表里像多了一层没必要的权威感。
func formatNumber(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
