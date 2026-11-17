package drafting

import (
	"strings"
	"testing"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// completeInputs 造一份十个要素全部齐备的判定产物。
//
// 批次号用占位值（<EXAMPLE_LOT-01>），点位与设备名用占位标识，
// 不含任何真实医院、真实批号或患者信息。
func completeInputs() Inputs {
	in := sampleInputs()
	in.Record.Batches = []domain.Batch{
		{PointID: "point-vax-freezer-01", BatchRef: "<EXAMPLE_LOT-01>", ProductName: "流感疫苗", Quantity: 100, ExpiryDate: "2026-12-31"},
	}
	in.Record.ConfirmedDisposition = "isolate"
	in.Record.Disposition = "isolate"
	return in
}

// TestCheckQuality_CompleteDraftPasses 覆盖「要素齐全通过」：
// 十个要素都在时，稿子既完整又合格，可以进写回载荷。
func TestCheckQuality_CompleteDraftPasses(t *testing.T) {
	q := CheckQuality(completeInputs(), fixedNow)

	if !q.Complete {
		t.Errorf("要素齐全时 Complete 应为 true，缺项：%v", q.MissingFields)
	}
	if !q.Qualified {
		t.Errorf("要素齐全时 Qualified 应为 true，挡写回的缺项：%v", q.BlockingFields)
	}
	if len(q.MissingFields) != 0 {
		t.Errorf("要素齐全时不应有缺项，实际：%v", q.MissingFields)
	}
	if q.NeedsReview() {
		t.Error("要素齐全时不应需要复核")
	}
	if !q.Exportable() {
		t.Error("要素齐全时应可作为台账写回载荷的来源")
	}
}

// TestCheckQuality_MissingBlockingElementListedAndRejected 覆盖「缺项被拦」：
// 必备要素缺了，缺项要列出来，这稿就不许进写回载荷。
func TestCheckQuality_MissingBlockingElementListedAndRejected(t *testing.T) {
	in := completeInputs()
	in.Record.PointID = ""

	q := CheckQuality(in, fixedNow)

	if q.Complete {
		t.Error("缺了点位就不该判完整")
	}
	if q.Qualified {
		t.Error("缺了必备要素就不该判合格")
	}
	if !containsString(q.MissingFields, domain.DraftElementPointID) {
		t.Errorf("缺项清单里没有点位，实际：%v", q.MissingFields)
	}
	if !containsString(q.BlockingFields, domain.DraftElementPointID) {
		t.Errorf("挡写回的缺项清单里没有点位，实际：%v", q.BlockingFields)
	}
	if !q.NeedsReview() {
		t.Error("有缺项就应标 needs_review")
	}
	if q.Exportable() {
		t.Error("必备要素缺项时不得可导出")
	}
}

// TestCheckQuality_PendingElementsDoNotBlockWriteback 覆盖缺项的两级语义：
// 批次与最终处置本来就是等人补的，缺了要列出来，但不挡台账写回。
//
// 这条要是反过来（也判成阻断），每一次越温记录都进不了台账——
// 台账拿不到记录，比记录里少两栏糟糕得多。
func TestCheckQuality_PendingElementsDoNotBlockWriteback(t *testing.T) {
	in := sampleInputs() // 没有批次，也没有主管确认的最终处置
	q := CheckQuality(in, fixedNow)

	if q.Complete {
		t.Error("缺了批次与最终处置就不该判完整")
	}
	if !q.Qualified {
		t.Errorf("待人工补齐的要素不该挡写回，挡写回的缺项：%v", q.BlockingFields)
	}
	if len(q.BlockingFields) != 0 {
		t.Errorf("挡写回的缺项应为空，实际：%v", q.BlockingFields)
	}
	for _, want := range []string{domain.DraftElementBatches, domain.DraftElementDisposition} {
		if !containsString(q.MissingFields, want) {
			t.Errorf("缺项清单里没有 %s，实际：%v", want, q.MissingFields)
		}
		if !containsString(q.PendingFields, want) {
			t.Errorf("待补清单里没有 %s，实际：%v", want, q.PendingFields)
		}
	}
	if !q.NeedsReview() {
		t.Error("有缺项就应标 needs_review")
	}
}

// TestCheckQuality_MissingEvidenceIsNotMissingCause 覆盖「unknown 也算有结论」：
// 证据不足时归因给 unknown，那是有结论的结论，不是缺项。
func TestCheckQuality_MissingEvidenceIsNotMissingCause(t *testing.T) {
	in := completeInputs()
	in.Attribution.CauseCode = "unknown"
	in.Attribution.CauseLabel = ""
	in.Attribution.Evidence = nil

	q := CheckQuality(in, fixedNow)

	if containsString(q.MissingFields, domain.DraftElementCauseConclusion) {
		t.Errorf("unknown 是有结论的结论，不该算缺项，缺项清单：%v", q.MissingFields)
	}
}

// TestCheckQuality_PrecisionLimitedCarriedThrough 覆盖「精度标注透传」：
// 点位精度超限时，校验结论要带上标注与超限数值。
func TestCheckQuality_PrecisionLimitedCarriedThrough(t *testing.T) {
	in := completeInputs()
	declared := 1.2
	in.Window.Precision = domain.PrecisionVerdict{
		Limited:     true,
		LimitC:      0.5,
		LimitSource: domain.PrecisionLimitDeviceClass,
		DeclaredC:   &declared,
		Detail:      "设备声明测温精度 1.20℃，超出该类设备规范上限 0.50℃",
	}

	q := CheckQuality(in, fixedNow)

	if !q.PrecisionLimited {
		t.Error("精度超限时校验结论应标 precision_limited")
	}
	if q.PrecisionTag != PrecisionTag {
		t.Errorf("精度标注文案不对：%q", q.PrecisionTag)
	}
	if !strings.Contains(q.PrecisionDetail, "1.20") || !strings.Contains(q.PrecisionDetail, "0.50") {
		t.Errorf("精度说明应含超限数值，实际：%q", q.PrecisionDetail)
	}
}

// TestCheckQuality_PrecisionDetailBuiltFromVerdict 覆盖精度说明的兜底：
// 判定没给说明文字时，两个数值都在就自己拼出来（不写「超了多少」等于让人猜）。
func TestCheckQuality_PrecisionDetailBuiltFromVerdict(t *testing.T) {
	in := completeInputs()
	declared := 1.2
	in.Window.Precision = domain.PrecisionVerdict{
		Limited:     true,
		LimitC:      0.5,
		LimitSource: domain.PrecisionLimitPointProfile,
		DeclaredC:   &declared,
	}

	q := CheckQuality(in, fixedNow)

	if !strings.Contains(q.PrecisionDetail, "1.2") || !strings.Contains(q.PrecisionDetail, "0.5") {
		t.Errorf("精度说明应含声明值与上限值，实际：%q", q.PrecisionDetail)
	}
	if !strings.Contains(q.PrecisionDetail, "点位温区配置") {
		t.Errorf("精度说明应写明上限来源，实际：%q", q.PrecisionDetail)
	}
}

// TestCheckQuality_DegradedCarriedThrough 覆盖「降级说明透传」：
// 归因来自规则基线时，校验结论要标降级并记下降级原因。
func TestCheckQuality_DegradedCarriedThrough(t *testing.T) {
	in := completeInputs()
	in.Attribution.Degraded = true
	in.Attribution.DegradeReason = "E_MODEL_UNAVAILABLE"

	q := CheckQuality(in, fixedNow)

	if !q.Degraded {
		t.Error("归因来自规则基线时应标 degraded")
	}
	if q.DegradeReason != "E_MODEL_UNAVAILABLE" {
		t.Errorf("降级原因不对：%q", q.DegradeReason)
	}
}

// TestRender_QualityNoticesAppearAtTop 覆盖「标注要显眼」：
// 精度不足与降级提示写在稿子最上面（标题之后、第一个栏目之前），
// 不是塞在末尾当附注。
func TestRender_QualityNoticesAppearAtTop(t *testing.T) {
	in := completeInputs()
	declared := 1.2
	in.Window.Precision = domain.PrecisionVerdict{
		Limited:     true,
		LimitC:      0.5,
		LimitSource: domain.PrecisionLimitDeviceClass,
		DeclaredC:   &declared,
		Detail:      "设备声明测温精度 1.20℃，超出该类设备规范上限 0.50℃",
	}
	in.Attribution.Degraded = true
	in.Attribution.DegradeReason = "E_MODEL_UNAVAILABLE"

	draft := render(t, in)
	text := draft.Text

	if !strings.Contains(text, PrecisionTag) {
		t.Errorf("成稿缺少精度标注 %q\n%s", PrecisionTag, text)
	}
	if !strings.Contains(text, "1.20℃") || !strings.Contains(text, "0.50℃") {
		t.Errorf("成稿的精度提示未写明超限数值\n%s", text)
	}
	if !strings.Contains(text, DegradeNotice) {
		t.Errorf("成稿缺少降级说明 %q\n%s", DegradeNotice, text)
	}
	if !strings.Contains(text, "E_MODEL_UNAVAILABLE") {
		t.Errorf("成稿的降级提示未写明降级原因\n%s", text)
	}

	// 位置：两条提示都要在第一个栏目之前。
	firstSection := strings.Index(text, "一、监测点位与设备")
	precisionAt := strings.Index(text, PrecisionTag)
	degradeAt := strings.Index(text, DegradeNotice)
	if firstSection < 0 {
		t.Fatalf("成稿缺少第一个栏目\n%s", text)
	}
	if precisionAt < 0 || precisionAt > firstSection {
		t.Errorf("精度提示应写在第一个栏目之前（位置 %d，栏目 %d）\n%s", precisionAt, firstSection, text)
	}
	if degradeAt < 0 || degradeAt > firstSection {
		t.Errorf("降级提示应写在第一个栏目之前（位置 %d，栏目 %d）\n%s", degradeAt, firstSection, text)
	}
}

// TestRender_MissingElementsListedOnDraft 覆盖「有缺就有账」：
// 稿子上要写清还差哪几项，缺项名要翻成中文栏目名。
func TestRender_MissingElementsListedOnDraft(t *testing.T) {
	in := sampleInputs() // 缺批次与最终处置
	draft := render(t, in)

	if !strings.Contains(draft.Text, "待补要素") {
		t.Errorf("成稿缺少待补要素提示\n%s", draft.Text)
	}
	if !strings.Contains(draft.Text, "涉及批次") || !strings.Contains(draft.Text, "处置建议") {
		t.Errorf("待补提示未写明缺哪几项\n%s", draft.Text)
	}
	if strings.Contains(draft.Text, "point_id") {
		t.Errorf("待补提示应写中文栏目名，不该露出字段名\n%s", draft.Text)
	}
}

// TestBuildWritebackSource_RefusesUnqualifiedDraft 覆盖「不合格成稿不可直接导出」：
// 必备要素缺项时，写回载荷组装被拒，且错误信息点名缺项。
func TestBuildWritebackSource_RefusesUnqualifiedDraft(t *testing.T) {
	in := completeInputs()
	in.Record.Zone = ""
	quality := CheckQuality(in, fixedNow)
	in.Record.Quality = quality.Domain()

	_, err := BuildWritebackSource(in.Record)
	if err == nil {
		t.Fatal("要素不全的成稿不得组装出台账写回载荷")
	}
	if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("错误码不对：%v", code)
	}
	if !strings.Contains(err.Error(), "zone") {
		t.Errorf("错误信息未点名缺项：%v", err)
	}
	if !strings.Contains(err.Error(), "不得作为台账写回载荷") {
		t.Errorf("错误信息未写明拒绝原因：%v", err)
	}
}

// TestBuildWritebackSource_NilRecordRejected 覆盖失败路径：
// 没有记录表载荷就没有写回载荷。
func TestBuildWritebackSource_NilRecordRejected(t *testing.T) {
	_, err := BuildWritebackSource(nil)
	if err == nil {
		t.Fatal("缺记录表载荷时应当报错")
	}
	if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("错误码不对：%v", code)
	}
}

// TestBuildWritebackSource_FieldsFollowSpec 覆盖写回来源字段：
// 字段与 spec 的出站字段规范一一对应，批次号原样带出，不改写。
func TestBuildWritebackSource_FieldsFollowSpec(t *testing.T) {
	in := completeInputs()
	// 走一遍真实流程：先成稿（正文落在记录表载荷上），再校验，再组装写回载荷。
	in.Record.DraftText = render(t, in).Text
	quality := CheckQuality(in, fixedNow)
	in.Record.Quality = quality.Domain()

	source, err := BuildWritebackSource(in.Record)
	if err != nil {
		t.Fatalf("要素齐全的成稿应当能组装写回载荷：%v", err)
	}

	if source.PointID != "point-vax-freezer-01" {
		t.Errorf("点位不对：%q", source.PointID)
	}
	if source.DeviceName != "疫苗冷藏箱-01号" {
		t.Errorf("设备名称不对：%q", source.DeviceName)
	}
	if source.PeakTemperatureC != 12.4 || source.DurationMinutes != 15 {
		t.Errorf("峰值或持续分钟不对：%v / %v", source.PeakTemperatureC, source.DurationMinutes)
	}
	if source.Zone != "2~8℃" {
		t.Errorf("温区不对：%q", source.Zone)
	}
	if source.CauseConclusion != "door_open" {
		t.Errorf("原因结论不对：%q", source.CauseConclusion)
	}
	if len(source.Batches) != 1 || source.Batches[0].BatchRef != "<EXAMPLE_LOT-01>" {
		t.Errorf("批次号应原样带出：%+v", source.Batches)
	}
	if len(source.Evidence) != 2 {
		t.Errorf("证据条数不对：%d", len(source.Evidence))
	}
	if source.Disposition != "isolate" {
		t.Errorf("处置应取主管确认后的最终结论，实际：%q", source.Disposition)
	}
	if source.ExceptionRecord == "" {
		t.Error("写回载荷应带上异常记录表正文")
	}
}

// TestBuildWritebackSource_PendingDispositionFallsBackToAdvice 覆盖处置取值来源：
// 主管还没确认时，写回载荷里的处置取系统给出的建议（如实写明「还没定」）。
func TestBuildWritebackSource_PendingDispositionFallsBackToAdvice(t *testing.T) {
	in := sampleInputs()
	in.Record.Disposition = "pending_manufacturer_evaluation"
	quality := CheckQuality(in, fixedNow)
	in.Record.Quality = quality.Domain()

	source, err := BuildWritebackSource(in.Record)
	if err != nil {
		t.Fatalf("待补项不挡写回：%v", err)
	}
	if source.Disposition != "pending_manufacturer_evaluation" {
		t.Errorf("未确认时应退回建议处置，实际：%q", source.Disposition)
	}
}

// containsString 判断字符串切片里有没有某个值。
func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
