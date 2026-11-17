package model

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/store"
)

// ---------------------------------------------------------------------------
// 本文件验证「模型输出事实校验」：防幻觉那道硬闸。
//
// 覆盖四类必答：温度偏差超限、窗口越界、编造批次号、重问后成功；
// 另加重问后仍失败、形状不合规重问、台账清单未取到不拦、事件条数不符、
// 时长不符、校验记录落库、以及走真实 OpenAI-compatible 协议的端到端用例。
// ---------------------------------------------------------------------------

// factSamples 是本次判定依据的温度采样（脱敏样例，与 testdata/series_excursion.json 同源）。
func factSamples() []domain.TemperatureEvent {
	base := time.Date(2026, 10, 20, 3, 0, 0, 0, time.FixedZone("CST", 8*3600))
	rows := []struct {
		offset   int
		temp     float64
		humidity float64
		events   []string
	}{
		{0, 5.2, 58.0, nil},
		{5, 5.6, 59.5, nil},
		{10, 8.6, 61.0, []string{domain.EventDoorOpen}},
		{15, 12.4, 68.5, []string{domain.EventDoorOpen}},
		{20, 9.8, 66.0, nil},
		{25, 8.7, 63.5, nil},
		{30, 6.1, 61.0, nil},
	}
	out := make([]domain.TemperatureEvent, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.TemperatureEvent{
			PointID:      "point-vax-freezer-01",
			DeviceID:     "device-recorder-0001",
			DeviceName:   "疫苗冷藏库温度记录仪",
			MeasuredAt:   base.Add(time.Duration(row.offset) * time.Minute),
			TemperatureC: row.temp,
			Unit:         domain.UnitCelsius,
			HumidityPct:  &row.humidity,
			Source:       domain.SourceRecorder,
			Events:       row.events,
		})
	}
	return out
}

// factWindow 是规则抽出的越温窗口（与规则层同口径：03:10 ~ 03:25，峰值 12.4℃）。
func factWindow() *domain.ExcursionWindow {
	base := time.Date(2026, 10, 20, 3, 0, 0, 0, time.FixedZone("CST", 8*3600))
	return &domain.ExcursionWindow{
		Start:               base.Add(10 * time.Minute),
		End:                 base.Add(25 * time.Minute),
		DurationMinutes:     15,
		PeakC:               12.4,
		PeakAt:              base.Add(15 * time.Minute),
		Zone:                "2~8℃",
		SampleCount:         4,
		SamplingIntervalSec: 300,
	}
}

// factBatches 是台账在库批次清单（占位批次号）。
func factBatches() []domain.Batch {
	return []domain.Batch{
		{PointID: "point-vax-freezer-01", BatchRef: "<EXAMPLE_LOT-01>", ProductName: "疫苗（脱敏样例）", Quantity: 120},
		{PointID: "point-vax-freezer-01", BatchRef: "<EXAMPLE_LOT-02>", ProductName: "疫苗（脱敏样例）", Quantity: 80},
	}
}

// factBasis 返回一份完整的校验依据。
func factBasis() FactBasis {
	return FactBasis{
		PointID:      "point-vax-freezer-01",
		Samples:      factSamples(),
		Window:       factWindow(),
		Batches:      factBatches(),
		BatchesKnown: true,
	}
}

// scriptedAsk 按顺序回放一串响应，并记录每次收到的反馈。
type scriptedAsk struct {
	responses []string
	calls     int
	feedbacks []string
	payloads  []any
}

// newScriptedAsk 造一个按顺序回放的提问出口。
func newScriptedAsk(responses ...string) *scriptedAsk {
	return &scriptedAsk{responses: responses}
}

// ask 实现 AskFunc。
func (s *scriptedAsk) ask(ctx context.Context, schema SchemaName, payload any, feedback string) (*Completion, error) {
	s.calls++
	s.feedbacks = append(s.feedbacks, feedback)
	s.payloads = append(s.payloads, payload)
	if s.calls > len(s.responses) {
		return nil, errcode.Newf(errcode.ModelUnavailable, "脚本已无响应可回（第 %d 次调用）", s.calls)
	}
	return &Completion{Schema: schema, Content: []byte(s.responses[s.calls-1])}, nil
}

// jsonNumber 把置信度写成 JSON 数字文本。
func jsonNumber(v float64) string {
	body, _ := json.Marshal(v)
	return string(body)
}

// TestFactCheck_TemperatureDeviationRejected 验证温度数值必须能在输入时序里找到：
// 模型报了一个现场从来没有的峰值温度，必须被拦下。
func TestFactCheck_TemperatureDeviationRejected(t *testing.T) {
	basis := factBasis()
	parsed, err := ParseWindow([]byte(`{"start":"2026-10-20T03:10:00+08:00","end":"2026-10-20T03:25:00+08:00",` +
		`"duration_minutes":15,"peak_c":13.7}`))
	if err != nil {
		t.Fatalf("构造窗口响应失败：%v", err)
	}

	issues := CheckFacts(SchemaWindow, parsed, basis)
	if len(issues) != 1 {
		t.Fatalf("应拦下 1 处事实不符，实际 %d 处（%+v）", len(issues), issues)
	}
	issue := issues[0]
	if issue.Kind != domain.FactKindTemperatureNotFound {
		t.Fatalf("不符类别应为 %q，实际 %q", domain.FactKindTemperatureNotFound, issue.Kind)
	}
	if issue.Path != "peak_c" {
		t.Fatalf("不符字段路径应为 peak_c，实际 %q", issue.Path)
	}
	if !strings.Contains(issue.Claimed, "13.70") {
		t.Fatalf("被拦下的原值应保留模型给的数值，实际 %q", issue.Claimed)
	}
	if !strings.Contains(issue.Basis, "12.40") {
		t.Fatalf("依据里应点出最接近的实测读数，实际 %q", issue.Basis)
	}
}

// TestFactCheck_TemperatureToleranceBoundary 验证温度容差边界：
// ±0.05℃ 内算四舍五入、之外算编造。
func TestFactCheck_TemperatureToleranceBoundary(t *testing.T) {
	basis := factBasis()
	for _, tc := range []struct {
		name     string
		peak     float64
		rejected bool
	}{
		{"正好等于实测读数", 12.4, false},
		{"四舍五入（+0.04）", 12.44, false},
		{"编造（+0.06）", 12.46, true},
		{"编造（远离实测）", 20.0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := ParseWindow([]byte(`{"start":"2026-10-20T03:10:00+08:00","end":"2026-10-20T03:25:00+08:00",` +
				`"duration_minutes":15,"peak_c":` + jsonNumber(tc.peak) + `}`))
			if err != nil {
				t.Fatalf("构造窗口响应失败：%v", err)
			}
			issues := CheckFacts(SchemaWindow, parsed, basis)
			if tc.rejected && len(issues) == 0 {
				t.Fatal("该温度应被判为输入时序里找不到")
			}
			if !tc.rejected && len(issues) != 0 {
				t.Fatalf("该温度应通过校验，实际拦下 %+v", issues)
			}
		})
	}
}

// TestFactCheck_WindowOutOfRangeRejected 验证窗口起止必须落在规则抽出的窗口内。
func TestFactCheck_WindowOutOfRangeRejected(t *testing.T) {
	basis := factBasis()
	parsed, err := ParseWindow([]byte(`{"start":"2026-10-20T02:40:00+08:00","end":"2026-10-20T03:25:00+08:00",` +
		`"duration_minutes":15,"peak_c":12.4}`))
	if err != nil {
		t.Fatalf("构造窗口响应失败：%v", err)
	}
	issues := CheckFacts(SchemaWindow, parsed, basis)
	if len(issues) != 1 {
		t.Fatalf("应拦下 1 处窗口越界，实际 %d 处（%+v）", len(issues), issues)
	}
	if issues[0].Kind != domain.FactKindWindowOutOfRange {
		t.Fatalf("不符类别应为 %q，实际 %q", domain.FactKindWindowOutOfRange, issues[0].Kind)
	}
	if !strings.Contains(issues[0].Basis, "2026-10-20T03:10:00+08:00") {
		t.Fatalf("依据里应给出规则窗口的起止，实际 %q", issues[0].Basis)
	}
}

// TestFactCheck_ReversedWindowOrderRejected 验证起止颠倒被拦下：
// 「结束时刻早于开始时刻」在台账上会变成负时长，必须在写之前拦。
func TestFactCheck_ReversedWindowOrderRejected(t *testing.T) {
	basis := factBasis()
	parsed, err := ParseWindow([]byte(`{"start":"2026-10-20T03:25:00+08:00","end":"2026-10-20T03:10:00+08:00",` +
		`"duration_minutes":15,"peak_c":12.4}`))
	if err != nil {
		t.Fatalf("构造窗口响应失败：%v", err)
	}
	issues := CheckFacts(SchemaWindow, parsed, basis)
	if len(issues) != 1 {
		t.Fatalf("应拦下 1 处起止颠倒，实际 %d 处（%+v）", len(issues), issues)
	}
	if !strings.Contains(issues[0].Reason, "起止顺序颠倒") {
		t.Fatalf("拦下理由应点明起止颠倒，实际 %q", issues[0].Reason)
	}
}

// TestFactCheck_FabricatedBatchRefRejected 验证编造的批次号被拦下：
// 批次号只由冷链台账给，模型提到清单外的批次就是编。
func TestFactCheck_FabricatedBatchRefRejected(t *testing.T) {
	basis := factBasis()
	parsed, err := ParseBatchAdvice([]byte(`[{"batch_ref":"<EXAMPLE_LOT-01>","advice":"isolate"},` +
		`{"batch_ref":"<EXAMPLE_LOT-99>","advice":"isolate"}]`))
	if err != nil {
		t.Fatalf("构造批次建议响应失败：%v", err)
	}
	issues := CheckFacts(SchemaBatchAdvice, parsed, basis)
	if len(issues) != 1 {
		t.Fatalf("应拦下 1 处批次号不符，实际 %d 处（%+v）", len(issues), issues)
	}
	issue := issues[0]
	if issue.Kind != domain.FactKindBatchRefUnknown {
		t.Fatalf("不符类别应为 %q，实际 %q", domain.FactKindBatchRefUnknown, issue.Kind)
	}
	if issue.Path != "items[1].batch_ref" {
		t.Fatalf("不符字段路径应为 items[1].batch_ref，实际 %q", issue.Path)
	}
	if !strings.Contains(issue.Basis, "<EXAMPLE_LOT-01>") {
		t.Fatalf("依据里应列出台账清单的批次号，实际 %q", issue.Basis)
	}
}

// TestFactCheck_BatchesUnknownSkipsBatchCheck 验证台账清单未取到时不拦批次号：
// 「还没查」与「查了是空的」是两回事。
func TestFactCheck_BatchesUnknownSkipsBatchCheck(t *testing.T) {
	basis := factBasis()
	basis.BatchesKnown = false
	basis.Batches = nil
	parsed, err := ParseBatchAdvice([]byte(`[{"batch_ref":"<ANY-LOT>","advice":"isolate"}]`))
	if err != nil {
		t.Fatalf("构造批次建议响应失败：%v", err)
	}
	if issues := CheckFacts(SchemaBatchAdvice, parsed, basis); len(issues) != 0 {
		t.Fatalf("批次清单未取到时不应拦下，实际拦下 %+v", issues)
	}
}

// TestFactCheck_EmptyLedgerListRejectsAnyBatchRef 验证台账清单查了但是空的：
// 此时模型提到任何批次号都是编的。
func TestFactCheck_EmptyLedgerListRejectsAnyBatchRef(t *testing.T) {
	basis := factBasis()
	basis.Batches = nil
	basis.BatchesKnown = true
	parsed, err := ParseBatchAdvice([]byte(`[{"batch_ref":"<EXAMPLE_LOT-01>","advice":"isolate"}]`))
	if err != nil {
		t.Fatalf("构造批次建议响应失败：%v", err)
	}
	if issues := CheckFacts(SchemaBatchAdvice, parsed, basis); len(issues) != 1 {
		t.Fatalf("清单为空时任何批次号都该被拦下，实际 %+v", issues)
	}
}

// TestFactCheck_EventCountMismatchRejected 验证设备事件条数与时序里数得出来的不符时拦下。
func TestFactCheck_EventCountMismatchRejected(t *testing.T) {
	basis := factBasis()
	parsed, err := ParseAttribution([]byte(`{"cause_code":"door_open","confidence":0.9,` +
		`"evidence":[{"feature":"device_event:door_open","value":5,"threshold":1}]}`))
	if err != nil {
		t.Fatalf("构造归因响应失败：%v", err)
	}
	issues := CheckFacts(SchemaAttribution, parsed, basis)
	if len(issues) != 1 {
		t.Fatalf("应拦下 1 处事件条数不符，实际 %d 处（%+v）", len(issues), issues)
	}
	if issues[0].Kind != domain.FactKindEventCountMismatch {
		t.Fatalf("不符类别应为 %q，实际 %q", domain.FactKindEventCountMismatch, issues[0].Kind)
	}
	if !strings.Contains(issues[0].Basis, "出现 2 次") {
		t.Fatalf("依据里应点出时序里数出来的次数，实际 %q", issues[0].Basis)
	}
}

// TestFactCheck_EventCountMatchingPasses 验证条数对得上时不拦。
func TestFactCheck_EventCountMatchingPasses(t *testing.T) {
	basis := factBasis()
	parsed, err := ParseAttribution([]byte(`{"cause_code":"door_open","confidence":0.9,` +
		`"evidence":[{"feature":"device_event:door_open","value":2,"threshold":1}]}`))
	if err != nil {
		t.Fatalf("构造归因响应失败：%v", err)
	}
	if issues := CheckFacts(SchemaAttribution, parsed, basis); len(issues) != 0 {
		t.Fatalf("条数对得上时不应拦下，实际 %+v", issues)
	}
}

// TestFactCheck_DurationMismatchRejected 验证持续时长与规则窗口对不上时拦下。
func TestFactCheck_DurationMismatchRejected(t *testing.T) {
	basis := factBasis()
	parsed, err := ParseWindow([]byte(`{"start":"2026-10-20T03:10:00+08:00","end":"2026-10-20T03:25:00+08:00",` +
		`"duration_minutes":999,"peak_c":12.4}`))
	if err != nil {
		t.Fatalf("构造窗口响应失败：%v", err)
	}
	issues := CheckFacts(SchemaWindow, parsed, basis)
	if len(issues) != 1 {
		t.Fatalf("应拦下 1 处时长不符，实际 %d 处（%+v）", len(issues), issues)
	}
	if issues[0].Kind != domain.FactKindDurationMismatch {
		t.Fatalf("不符类别应为 %q，实际 %q", domain.FactKindDurationMismatch, issues[0].Kind)
	}
}

// TestFactCheck_RetryOnceThenSucceed 验证重问一次后通过：
// 首次编了一个温度，被拦下；带反馈重问后改对了，结论可用，首轮问题留档。
func TestFactCheck_RetryOnceThenSucceed(t *testing.T) {
	script := newScriptedAsk(
		`{"cause_code":"door_open","confidence":0.9,"evidence":[{"feature":"temperature_c","value":13.7,"threshold":8}]}`,
		`{"cause_code":"door_open","confidence":0.9,"evidence":[{"feature":"temperature_c","value":12.4,"threshold":8}]}`,
	)
	asker := &Asker{
		Caller:   script.ask,
		Basis:    factBasis(),
		RunID:    "run-fact-001",
		StepName: "attribute",
	}
	parsed, result, err := asker.Ask(context.Background(), SchemaAttribution, nil)
	if err != nil {
		t.Fatalf("重问后应通过校验，实际报错：%v", err)
	}
	if script.calls != 2 {
		t.Fatalf("应调用模型 2 次（首次 + 重问），实际 %d 次", script.calls)
	}
	if result.Attempts != 2 {
		t.Fatalf("尝试次数应为 2，实际 %d", result.Attempts)
	}
	if len(result.Issues) != 1 {
		t.Fatalf("首轮被拦下的问题应留在结果里，实际 %d 条", len(result.Issues))
	}
	if result.Issues[0].Claimed == "" || result.Issues[0].Basis == "" {
		t.Fatalf("留档的问题应带原值与依据，实际 %+v", result.Issues[0])
	}
	attr, ok := parsed.(*Attribution)
	if !ok {
		t.Fatalf("应返回归因结论，实际 %T", parsed)
	}
	if attr.CauseCode != "door_open" {
		t.Fatalf("归因原因应为 door_open，实际 %q", attr.CauseCode)
	}
	if len(result.Records) != 2 {
		t.Fatalf("应留 2 条校验记录，实际 %d 条", len(result.Records))
	}
	if result.Records[0].Verdict != domain.FactVerdictMismatch {
		t.Fatalf("首轮记录结论应为 %q，实际 %q", domain.FactVerdictMismatch, result.Records[0].Verdict)
	}
	if result.Records[1].Verdict != domain.FactVerdictPass {
		t.Fatalf("重问记录结论应为 %q，实际 %q", domain.FactVerdictPass, result.Records[1].Verdict)
	}
	// 重问时必须把「错在哪」带回去：模型得看得见依据才知道该改成什么。
	if strings.TrimSpace(script.feedbacks[0]) != "" {
		t.Fatal("首次调用不应带反馈")
	}
	feedback := script.feedbacks[1]
	if !strings.Contains(feedback, "13.70") || !strings.Contains(feedback, "输入时序") {
		t.Fatalf("重问反馈应写明被拦下的数值与依据，实际 %q", feedback)
	}
}

// TestFactCheck_RetryOnceStillFails_NoFurtherRetry 验证重问后仍不符就不再重试：
// 返回 E_MODEL_FACT_MISMATCH，调用次数恰好 2 次。
func TestFactCheck_RetryOnceStillFails_NoFurtherRetry(t *testing.T) {
	bad := `{"cause_code":"door_open","confidence":0.9,"evidence":[{"feature":"temperature_c","value":13.7,"threshold":8}]}`
	script := newScriptedAsk(bad, bad)
	asker := &Asker{Caller: script.ask, Basis: factBasis(), RunID: "run-fact-002", StepName: "attribute"}

	parsed, result, err := asker.Ask(context.Background(), SchemaAttribution, nil)
	if err == nil {
		t.Fatal("两次都编造时应报错")
	}
	if parsed != nil {
		t.Fatalf("校验没过时不应返回结论，实际 %+v", parsed)
	}
	if code := errcode.As(err).Code; code != errcode.ModelFactMismatch {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.ModelFactMismatch, code)
	}
	if !IsFactMismatch(err) {
		t.Fatal("错误应能被 IsFactMismatch 识别为事实不符")
	}
	if script.calls != 2 {
		t.Fatalf("应只调用 2 次（首次 + 重问一次），实际 %d 次", script.calls)
	}
	if result.Attempts != 2 {
		t.Fatalf("尝试次数应为 2，实际 %d", result.Attempts)
	}
	var mismatch *FactMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatal("错误应带 FactMismatchError，便于降级路径取用")
	}
	if mismatch.Attempts != 2 || len(mismatch.Issues) != 1 {
		t.Fatalf("事实不符错误应带尝试次数与问题清单，实际 %+v", mismatch)
	}
	if mismatch.RawFragment == "" {
		t.Fatal("事实不符错误应保留模型原文片段供审计")
	}
	if len(result.Records) != 2 {
		t.Fatalf("应留 2 条校验记录，实际 %d 条", len(result.Records))
	}
}

// TestFactCheck_ShapeInvalidRetriedOnce 验证形状不合规也重问一次：
// 告诉模型「哪个字段不合规」，第二次往往就答对了。
func TestFactCheck_ShapeInvalidRetriedOnce(t *testing.T) {
	script := newScriptedAsk(
		`{"cause_code":"door_open"}`,
		`{"cause_code":"door_open","confidence":0.9}`,
	)
	asker := &Asker{Caller: script.ask, Basis: factBasis(), RunID: "run-fact-003", StepName: "attribute"}
	parsed, result, err := asker.Ask(context.Background(), SchemaAttribution, nil)
	if err != nil {
		t.Fatalf("重问后应通过校验，实际报错：%v", err)
	}
	if script.calls != 2 {
		t.Fatalf("应调用模型 2 次，实际 %d 次", script.calls)
	}
	if _, ok := parsed.(*Attribution); !ok {
		t.Fatalf("应返回归因结论，实际 %T", parsed)
	}
	if len(result.Records) != 2 {
		t.Fatalf("应留 2 条校验记录，实际 %d 条", len(result.Records))
	}
	if result.Records[0].Verdict != domain.FactVerdictShapeInvalid {
		t.Fatalf("首轮记录结论应为 %q，实际 %q", domain.FactVerdictShapeInvalid, result.Records[0].Verdict)
	}
	if !strings.Contains(script.feedbacks[1], "结构化校验") {
		t.Fatalf("重问反馈应说明形状不合规，实际 %q", script.feedbacks[1])
	}
}

// TestFactCheck_ShapeInvalidTwiceReturnsSchemaError 验证形状两次都不合规：
// 返回 E_MODEL_SCHEMA_INVALID，且不再重试。
func TestFactCheck_ShapeInvalidTwiceReturnsSchemaError(t *testing.T) {
	script := newScriptedAsk(`{"cause_code":"door_open"}`, `{"cause_code":"door_open"}`)
	asker := &Asker{Caller: script.ask, Basis: factBasis(), RunID: "run-fact-004", StepName: "attribute"}
	_, result, err := asker.Ask(context.Background(), SchemaAttribution, nil)
	if err == nil {
		t.Fatal("两次形状都不合规时应报错")
	}
	if code := errcode.As(err).Code; code != errcode.ModelSchemaInvalid {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.ModelSchemaInvalid, code)
	}
	if script.calls != 2 {
		t.Fatalf("应只调用 2 次，实际 %d 次", script.calls)
	}
	if len(result.Records) != 2 {
		t.Fatalf("应留 2 条校验记录，实际 %d 条", len(result.Records))
	}
}

// TestFactCheck_RecordsPersistedToStore 验证事实校验元数据落库：
// 被拦下的字段、原值与依据都留得住，事后可追溯。
func TestFactCheck_RecordsPersistedToStore(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开库失败：%v", err)
	}
	defer func() { _ = st.Close() }()

	bad := `{"cause_code":"door_open","confidence":0.9,"evidence":[{"feature":"temperature_c","value":13.7,"threshold":8}]}`
	good := `{"cause_code":"door_open","confidence":0.9,"evidence":[{"feature":"temperature_c","value":12.4,"threshold":8}]}`
	script := newScriptedAsk(bad, good)
	asker := &Asker{
		Caller:   script.ask,
		Basis:    factBasis(),
		RunID:    "run-fact-005",
		StepName: "attribute",
		Recorder: st,
	}
	if _, _, err := asker.Ask(context.Background(), SchemaAttribution, nil); err != nil {
		t.Fatalf("重问后应通过校验，实际报错：%v", err)
	}

	records, err := st.FactChecksByRun(context.Background(), "run-fact-005")
	if err != nil {
		t.Fatalf("回读校验记录失败：%v", err)
	}
	if len(records) != 2 {
		t.Fatalf("库里应有 2 条校验记录，实际 %d 条", len(records))
	}
	if records[0].Verdict != domain.FactVerdictMismatch {
		t.Fatalf("首轮记录结论应为 %q，实际 %q", domain.FactVerdictMismatch, records[0].Verdict)
	}
	if len(records[0].Issues) != 1 {
		t.Fatalf("首轮记录应带 1 条问题，实际 %d 条", len(records[0].Issues))
	}
	issue := records[0].Issues[0]
	if issue.Claimed == "" || issue.Basis == "" || issue.Reason == "" {
		t.Fatalf("落库的问题应带原值、依据与理由，实际 %+v", issue)
	}
	if records[0].RawFragment == "" {
		t.Fatal("落库记录应保留模型原文片段")
	}
	if records[1].Verdict != domain.FactVerdictPass {
		t.Fatalf("重问记录结论应为 %q，实际 %q", domain.FactVerdictPass, records[1].Verdict)
	}
	count, err := st.CountFactChecks(context.Background(), "run-fact-005")
	if err != nil {
		t.Fatalf("统计校验记录失败：%v", err)
	}
	if count != 2 {
		t.Fatalf("统计条数应为 2，实际 %d", count)
	}
}

// TestFactCheck_PayloadIsSameAsBasis 验证送给模型的输入就是校验依据本身：
// 模型能引用的数字与批次号，恰好就是事实校验允许的那些。
func TestFactCheck_PayloadIsSameAsBasis(t *testing.T) {
	basis := factBasis()
	script := newScriptedAsk(`{"cause_code":"door_open","confidence":0.9}`)
	asker := &Asker{Caller: script.ask, Basis: basis}
	if _, _, err := asker.Ask(context.Background(), SchemaAttribution, nil); err != nil {
		t.Fatalf("合规输出应通过校验，实际报错：%v", err)
	}
	if len(script.payloads) != 1 {
		t.Fatalf("应调用 1 次，实际 %d 次", len(script.payloads))
	}
	payload, ok := script.payloads[0].(map[string]any)
	if !ok {
		t.Fatalf("载荷应是 map，实际 %T", script.payloads[0])
	}
	samples, ok := payload["samples"].([]slimSample)
	if !ok || len(samples) != len(basis.Samples) {
		t.Fatalf("载荷里的采样条数应为 %d，实际 %v", len(basis.Samples), payload["samples"])
	}
	if payload["point_id"] != basis.PointID {
		t.Fatalf("载荷里的点位应为 %q，实际 %v", basis.PointID, payload["point_id"])
	}
	batches, ok := payload["batches"].([]map[string]any)
	if !ok || len(batches) != len(basis.Batches) {
		t.Fatalf("载荷里的批次条数应为 %d，实际 %v", len(basis.Batches), payload["batches"])
	}
	if _, hasWindow := payload["window"]; !hasWindow {
		t.Fatal("载荷里应带规则窗口")
	}
}

// TestFactCheck_GatewayEndToEnd_RetryCarriesFeedback 端到端走真实 OpenAI-compatible 协议：
// 首次编造温度被拦下 → 带反馈重问 → 改对后通过，且反馈里带着被拦下的数值。
func TestFactCheck_GatewayEndToEnd_RetryCarriesFeedback(t *testing.T) {
	var stub *stub
	stub = newStub(t, func(w http.ResponseWriter, r *http.Request) {
		// 第一次答一个现场没有的峰值温度，第二次改成实测值。
		content := `{"start":"2026-10-20T03:10:00+08:00","end":"2026-10-20T03:25:00+08:00",` +
			`"duration_minutes":15,"peak_c":13.7}`
		if stub.count() > 1 {
			content = `{"start":"2026-10-20T03:10:00+08:00","end":"2026-10-20T03:25:00+08:00",` +
				`"duration_minutes":15,"peak_c":12.4}`
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":     "chatcmpl-fact",
			"object": "chat.completion",
			"model":  "stub-model",
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": content},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120},
		})
	})
	gateway := newTestGateway(t, stub, 5*time.Second)

	asker := &Asker{
		Caller:   GatewayAsker(gateway),
		Basis:    factBasis(),
		RunID:    "run-fact-006",
		StepName: "attribute",
	}
	parsed, result, err := asker.Ask(context.Background(), SchemaWindow, nil)
	if err != nil {
		t.Fatalf("重问后应通过校验，实际报错：%v", err)
	}
	if stub.count() != 2 {
		t.Fatalf("替身应收到 2 次请求，实际 %d 次", stub.count())
	}
	win, ok := parsed.(*Window)
	if !ok {
		t.Fatalf("应返回越限窗口，实际 %T", parsed)
	}
	if win.PeakC != 12.4 {
		t.Fatalf("重问后的峰值温度应为 12.4，实际 %v", win.PeakC)
	}
	if result.Attempts != 2 || len(result.Records) != 2 {
		t.Fatalf("应记录 2 次尝试，实际 attempts=%d records=%d", result.Attempts, len(result.Records))
	}
	// 第二次请求里必须带着反馈：模型看得见自己错在哪才改得对。
	body := stub.last(t).Body
	if !strings.Contains(body, "13.70") {
		t.Fatalf("重问请求体应带被拦下的数值，实际 %s", body)
	}
	if !strings.Contains(body, "输入时序") {
		t.Fatalf("重问请求体应带输入依据摘要，实际 %s", body)
	}
	// 请求体里同时带上了原始输入：模型能引用的数字就是校验允许的那些。
	if !strings.Contains(body, "12.4") {
		t.Fatalf("重问请求体应带输入时序里的实测温度，实际 %s", body)
	}
}
