package rules

import (
	"strings"
	"testing"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// 本文件测的是规则基线归因：
//   - 断电、长时间开门、探头卡死、探头脱落、制冷异常、超量存放六类候选的证据与置信度；
//   - 湿度无关温升与开门带湿空气的区别（同一批数字，两种结论）；
//   - 证据不足时给 unknown 并列出「还差哪条线索」，不硬套原因；
//   - 精度不足联动：每条候选都带标注、置信度封顶；
//   - 候选排序与「每条非 unknown 候选至少一条含数值与阈值的证据」这条硬约束。
//
// 场景样例统一用 5 分钟一采的栅格（featZone：温区 2~8℃、采样周期 5 分钟、
// 最短越限 10 分钟、迟滞带 1℃），对应现场常见配置。

// attrPct 造一个浮点指针（缺测就是 nil，不用 0 冒充）。
func attrPct(v float64) *float64 { return &v }

// attrInt 造一个整数指针。
func attrInt(v int) *int { return &v }

// attrBool 造一个布尔指针。
func attrBool(v bool) *bool { return &v }

// baseFeatures 造一份「算过了但什么都没发生」的特征底子。
//
// 温升速率 3.2℃/小时、湿度持平、设备报过一次开门又报过断电与除霜，
// 各项特征都齐备，测试在此基础上替换其中一项来构造场景。
func baseFeatures() domain.ExcursionFeatures {
	return domain.ExcursionFeatures{
		RiseRatePerHour:      attrPct(3.2),
		RiseRateHours:        1.0,
		RiseRateSamples:      4,
		HumidityBaselinePct:  attrPct(58),
		HumidityPeakPct:      attrPct(58.4),
		HumidityRisePct:      attrPct(0.4),
		DoorOpenMinutes:      attrInt(0),
		DoorOpenCount:        attrInt(0),
		PowerLossObserved:    attrBool(false),
		PowerLossCount:       0,
		DefrostObserved:      attrBool(false),
		DefrostCount:         0,
		MaxSampleJumpC:       attrPct(1.2),
		SampleJumpCount:      attrInt(0),
		SampleJumpThresholdC: 5.0,
		AccuracyLimitC:       0.5,
		DeviceEventsReported: true,
		MissingFeatures:      []string{},
		Notes:                []string{},
	}
}

// findCandidate 按原因码取候选。
func findCandidate(t *testing.T, payload *domain.AttributionPayload, causeCode string) (domain.AttributionCandidate, bool) {
	t.Helper()
	for _, candidate := range payload.Candidates {
		if candidate.CauseCode == causeCode {
			return candidate, true
		}
	}
	return domain.AttributionCandidate{}, false
}

// hasEvidence 判断候选里有没有某条特征的证据。
func hasEvidence(candidate domain.AttributionCandidate, feature string) (domain.AttributionEvidence, bool) {
	for _, evidence := range candidate.Evidence {
		if evidence.Feature == feature {
			return evidence, true
		}
	}
	return domain.AttributionEvidence{}, false
}

// assertEvidenceNumeric 断言候选的每条证据都带特征名、阈值与说明文字。
//
// 这是本任务最硬的一条约束：没有数值与阈值的证据，复核的人无从判断
// 这条结论站不站得住。
//
// 数值允许为 0：「观测范围内断电事件 0 次」是一条实打实的读数，
// 把它当成「没测」就丢了信息。阈值不允许为 0——零阈值等于没有判据。
func assertEvidenceNumeric(t *testing.T, candidate domain.AttributionCandidate) {
	t.Helper()
	if len(candidate.Evidence) == 0 {
		t.Fatalf("候选 %s 没有任何证据", candidate.CauseCode)
	}
	for _, evidence := range candidate.Evidence {
		if strings.TrimSpace(evidence.Feature) == "" {
			t.Errorf("候选 %s 的证据缺少特征名", candidate.CauseCode)
		}
		if evidence.Threshold == 0 {
			t.Errorf("候选 %s 的证据 %s 缺阈值", candidate.CauseCode, evidence.Feature)
		}
		if strings.TrimSpace(evidence.Note) == "" {
			t.Errorf("候选 %s 的证据 %s 缺说明文字", candidate.CauseCode, evidence.Feature)
		}
	}
}

// TestAttribute_PowerLoss 断电：设备自己上报断电事件，是最硬的一条证据，
// 结论应落在 power_loss，证据带事件次数与阈值，置信度明显高于其它候选。
func TestAttribute_PowerLoss(t *testing.T) {
	features := baseFeatures()
	features.PowerLossObserved = attrBool(true)
	features.PowerLossCount = 3
	features.HumidityRisePct = attrPct(0.2)

	payload, err := Attribute(features, AttributionOptions{})
	if err != nil {
		t.Fatalf("归因失败：%v", err)
	}
	if payload.CauseCode != CausePowerLoss {
		t.Fatalf("原因结论 = %q，期望 %q", payload.CauseCode, CausePowerLoss)
	}
	if payload.CauseLabel != "断电" {
		t.Errorf("原因中文标签 = %q，期望「断电」", payload.CauseLabel)
	}
	if payload.Confidence < 0.85 {
		t.Errorf("断电置信度 = %.2f，期望不低于 0.85（设备直接上报的事件）", payload.Confidence)
	}

	candidate, ok := findCandidate(t, payload, CausePowerLoss)
	if !ok {
		t.Fatal("候选集里应有 power_loss")
	}
	assertEvidenceNumeric(t, candidate)

	evidence, ok := hasEvidence(candidate, "device_event:power_loss")
	if !ok {
		t.Fatal("断电候选应带设备事件证据")
	}
	if evidence.Value != 3 || evidence.Threshold != 1 {
		t.Errorf("断电证据数值/阈值 = %v/%v，期望 3/1", evidence.Value, evidence.Threshold)
	}

	// 湿度持平是断电的佐证：断电时压缩机停了，箱内没有湿空气进来。
	if _, ok := hasEvidence(candidate, "humidity_rise_pct"); !ok {
		t.Error("断电候选应带湿度持平的佐证")
	}
}

// TestAttribute_DoorOpenLongTime 长时间开门：开门时长够、开门在温升之前、
// 湿度跟着涨——三条都成立时结论应落在 door_open。
func TestAttribute_DoorOpenLongTime(t *testing.T) {
	features := baseFeatures()
	features.DoorOpenMinutes = attrInt(30)
	features.DoorOpenCount = attrInt(6)
	features.DoorToRiseDelayMinutes = attrInt(5)
	features.HumidityRisePct = attrPct(6.0)

	payload, err := Attribute(features, AttributionOptions{})
	if err != nil {
		t.Fatalf("归因失败：%v", err)
	}
	if payload.CauseCode != CauseDoorOpen {
		t.Fatalf("原因结论 = %q，期望 %q", payload.CauseCode, CauseDoorOpen)
	}

	candidate, ok := findCandidate(t, payload, CauseDoorOpen)
	if !ok {
		t.Fatal("候选集里应有 door_open")
	}
	assertEvidenceNumeric(t, candidate)

	open, ok := hasEvidence(candidate, "door_open_minutes")
	if !ok {
		t.Fatal("开门候选应带开门时长证据")
	}
	if open.Value != 30 {
		t.Errorf("开门时长证据数值 = %v，期望 30", open.Value)
	}
	if _, ok := hasEvidence(candidate, "door_to_rise_delay_minutes"); !ok {
		t.Error("开门候选应带开门与温升的滞后证据")
	}
	if payload.Confidence <= 0.8 {
		t.Errorf("长时间开门置信度 = %.2f，期望高于 0.8", payload.Confidence)
	}
}

// TestAttribute_DoorOpenExcludedWhenDoorAfterRise 开门发生在温升之后：
// 这一开门不是本次温升的原因，候选不该成立。
func TestAttribute_DoorOpenExcludedWhenDoorAfterRise(t *testing.T) {
	features := baseFeatures()
	features.DoorOpenMinutes = attrInt(20)
	features.DoorOpenCount = attrInt(4)
	features.DoorToRiseDelayMinutes = attrInt(-5)

	payload, err := Attribute(features, AttributionOptions{})
	if err != nil {
		t.Fatalf("归因失败：%v", err)
	}
	if _, ok := findCandidate(t, payload, CauseDoorOpen); ok {
		t.Fatal("开门发生在温升之后，不该产生 door_open 候选")
	}
}

// TestAttribute_ProbeStuck 探头卡死：读数恒定不动，越限可能只是读数假象，
// 结论应落在 probe_stuck 并说明须现场确认箱内温度。
func TestAttribute_ProbeStuck(t *testing.T) {
	features := baseFeatures()
	features.ProbeStuckSampleCount = 4
	features.RiseRatePerHour = nil
	features.RiseRateSamples = 0

	payload, err := Attribute(features, AttributionOptions{})
	if err != nil {
		t.Fatalf("归因失败：%v", err)
	}
	if payload.CauseCode != CauseProbeStuck {
		t.Fatalf("原因结论 = %q，期望 %q", payload.CauseCode, CauseProbeStuck)
	}

	candidate, ok := findCandidate(t, payload, CauseProbeStuck)
	if !ok {
		t.Fatal("候选集里应有 probe_stuck")
	}
	assertEvidenceNumeric(t, candidate)

	evidence, ok := hasEvidence(candidate, "probe_stuck_sample_count")
	if !ok {
		t.Fatal("探头卡死候选应带卡死采样条数证据")
	}
	if evidence.Value != 4 || evidence.Threshold != 3 {
		t.Errorf("卡死证据数值/阈值 = %v/%v，期望 4/3", evidence.Value, evidence.Threshold)
	}
	if !strings.Contains(candidate.Note, "现场确认") {
		t.Errorf("探头卡死候选应提示现场确认箱内温度，实际 %q", candidate.Note)
	}
}

// TestAttribute_ProbeDetached 探头脱落：相邻采样跳变超阈值，箱体热容不支持这种变化速度。
func TestAttribute_ProbeDetached(t *testing.T) {
	features := baseFeatures()
	features.SampleJumpCount = attrInt(2)
	features.MaxSampleJumpC = attrPct(6.4)
	features.RiseRatePerHour = attrPct(30.0)

	payload, err := Attribute(features, AttributionOptions{})
	if err != nil {
		t.Fatalf("归因失败：%v", err)
	}
	if payload.CauseCode != CauseProbeDetached {
		t.Fatalf("原因结论 = %q，期望 %q", payload.CauseCode, CauseProbeDetached)
	}

	candidate, _ := findCandidate(t, payload, CauseProbeDetached)
	assertEvidenceNumeric(t, candidate)
	jump, ok := hasEvidence(candidate, "max_sample_jump_c")
	if !ok {
		t.Fatal("探头脱落候选应带最大跳变证据")
	}
	if jump.Value != 6.4 || jump.Threshold != 5.0 {
		t.Errorf("跳变证据数值/阈值 = %v/%v，期望 6.4/5", jump.Value, jump.Threshold)
	}
}

// TestAttribute_HumidityIndependentRise 湿度无关温升：
// 带电运行、柜门没开、没有除霜、湿度纹丝不动，箱内温度却持续上行一小时以上——
// 结论应落在制冷异常，并带上「湿度持平」这条佐证。
func TestAttribute_HumidityIndependentRise(t *testing.T) {
	features := baseFeatures()
	features.RiseRatePerHour = attrPct(7.5)
	features.HumidityRisePct = attrPct(0.1)
	features.DoorOpenMinutes = attrInt(5)

	window := &domain.ExcursionWindow{DurationMinutes: 65}
	payload, err := Attribute(features, AttributionOptions{Window: window})
	if err != nil {
		t.Fatalf("归因失败：%v", err)
	}
	if payload.CauseCode != CauseACFailure {
		t.Fatalf("原因结论 = %q，期望 %q", payload.CauseCode, CauseACFailure)
	}

	candidate, ok := findCandidate(t, payload, CauseACFailure)
	if !ok {
		t.Fatal("候选集里应有 ac_failure")
	}
	assertEvidenceNumeric(t, candidate)

	if _, ok := hasEvidence(candidate, "excursion_duration_minutes"); !ok {
		t.Error("制冷异常候选应带越温持续时长证据")
	}
	if _, ok := hasEvidence(candidate, "humidity_rise_pct"); !ok {
		t.Error("制冷异常候选应带湿度持平证据")
	}
	if candidate.Confidence > 0.6 {
		t.Errorf("制冷异常置信度 = %.2f，期望不超过 0.6（由现象反推，须现场核实）", candidate.Confidence)
	}
}

// TestAttribute_ACKnowledgeNeedsDeviceEvents 设备不上报事件时，
// 断电与制冷异常分不开，制冷异常这一结论不该硬成立。
func TestAttribute_ACKnowledgeNeedsDeviceEvents(t *testing.T) {
	features := baseFeatures()
	features.DeviceEventsReported = false
	features.PowerLossObserved = nil
	features.DefrostObserved = nil
	features.DoorOpenMinutes = nil

	window := &domain.ExcursionWindow{DurationMinutes: 120}
	payload, err := Attribute(features, AttributionOptions{Window: window})
	if err != nil {
		t.Fatalf("归因失败：%v", err)
	}
	if _, ok := findCandidate(t, payload, CauseACFailure); ok {
		t.Fatal("设备不上报事件时不该产生 ac_failure 候选")
	}
	if payload.CauseCode != CauseUnknown {
		t.Fatalf("原因结论 = %q，期望 %q（证据不足就说不出原因）", payload.CauseCode, CauseUnknown)
	}
}

// TestAttribute_InsufficientEvidence 证据不足：没有设备事件、没有读数异常，
// 结论应是 unknown，并把「还差哪条线索」列出来。
func TestAttribute_InsufficientEvidence(t *testing.T) {
	features := baseFeatures()
	features.DeviceEventsReported = false
	features.PowerLossObserved = nil
	features.DefrostObserved = nil
	features.DoorOpenMinutes = nil
	features.DoorToRiseDelayMinutes = nil
	features.HumidityRisePct = nil
	features.SampleJumpCount = nil

	payload, err := Attribute(features, AttributionOptions{})
	if err != nil {
		t.Fatalf("归因失败：%v", err)
	}
	if payload.CauseCode != CauseUnknown {
		t.Fatalf("原因结论 = %q，期望 %q", payload.CauseCode, CauseUnknown)
	}
	if payload.Confidence != 0 {
		t.Errorf("原因待查的置信度 = %.2f，期望 0", payload.Confidence)
	}
	if len(payload.MissingClues) == 0 {
		t.Fatal("原因判不出来时必须列出还差哪条线索")
	}
	joined := strings.Join(payload.MissingClues, "；")
	if !strings.Contains(joined, "设备事件") {
		t.Errorf("缺失线索里应点名设备不上报事件这一条，实际 %q", joined)
	}
	if !strings.Contains(joined, "湿度") {
		t.Errorf("缺失线索里应点名湿度线索缺失，实际 %q", joined)
	}
}

// TestAttribute_PrecisionLimitedCapsConfidenceAndTagsEveryCandidate 精度不足联动：
// 读数本身不可信时，每一条候选都要带「精度不足，仅供参考」的标注，置信度一律封顶。
func TestAttribute_PrecisionLimitedCapsConfidenceAndTagsEveryCandidate(t *testing.T) {
	features := baseFeatures()
	features.PowerLossObserved = attrBool(true)
	features.PowerLossCount = 3
	features.PrecisionLimited = true
	features.PrecisionTag = "精度不足，仅供参考"
	features.AccuracyC = attrPct(1.2)

	payload, err := Attribute(features, AttributionOptions{})
	if err != nil {
		t.Fatalf("归因失败：%v", err)
	}
	if !payload.PrecisionLimited {
		t.Fatal("精度不足时结论必须标记 precision_limited")
	}
	if payload.PrecisionTag != "精度不足，仅供参考" {
		t.Errorf("精度标注文案 = %q，期望「精度不足，仅供参考」", payload.PrecisionTag)
	}
	if payload.Confidence > ConfidenceCapPrecisionLimited {
		t.Errorf("结论置信度 = %.2f，期望封顶在 %.2f", payload.Confidence, ConfidenceCapPrecisionLimited)
	}
	if len(payload.Candidates) == 0 {
		t.Fatal("候选集不该为空")
	}
	for _, candidate := range payload.Candidates {
		if candidate.Tag != "精度不足，仅供参考" {
			t.Errorf("候选 %s 缺精度不足标注", candidate.CauseCode)
		}
		if candidate.Confidence > ConfidenceCapPrecisionLimited {
			t.Errorf("候选 %s 置信度 = %.2f，期望封顶在 %.2f",
				candidate.CauseCode, candidate.Confidence, ConfidenceCapPrecisionLimited)
		}
	}
}

// TestAttribute_CandidatesSortedByEvidenceStrength 候选排序：按证据强度（置信度）降序。
func TestAttribute_CandidatesSortedByEvidenceStrength(t *testing.T) {
	features := baseFeatures()
	features.PowerLossObserved = attrBool(true)
	features.PowerLossCount = 2
	features.DoorOpenMinutes = attrInt(20)
	features.DoorOpenCount = attrInt(4)
	features.DoorToRiseDelayMinutes = attrInt(45)
	features.HumidityRisePct = attrPct(0.2)

	payload, err := Attribute(features, AttributionOptions{})
	if err != nil {
		t.Fatalf("归因失败：%v", err)
	}
	if len(payload.Candidates) < 2 {
		t.Fatalf("候选数 = %d，期望至少 2 条（断电与开门）", len(payload.Candidates))
	}
	for i := 1; i < len(payload.Candidates); i++ {
		if payload.Candidates[i-1].Confidence < payload.Candidates[i].Confidence {
			t.Fatalf("候选未按证据强度降序：第 %d 条 %.2f < 第 %d 条 %.2f",
				i, payload.Candidates[i-1].Confidence, i+1, payload.Candidates[i].Confidence)
		}
	}
	// 每条非 unknown 候选都要带数值与阈值齐全的证据。
	for _, candidate := range payload.Candidates {
		if candidate.CauseCode == CauseUnknown {
			continue
		}
		assertEvidenceNumeric(t, candidate)
	}
}

// TestAttribute_Overstock 超量存放：在库装载超出额定装载时给出候选；
// 没有装载数据时不得凭空给出这一条。
func TestAttribute_Overstock(t *testing.T) {
	features := baseFeatures()

	// 没有装载数据：不该有超量存放候选。
	payload, err := Attribute(features, AttributionOptions{})
	if err != nil {
		t.Fatalf("归因失败：%v", err)
	}
	if _, ok := findCandidate(t, payload, CauseOverstock); ok {
		t.Fatal("没有装载数据时不该产生 overstock 候选")
	}

	// 装载 125%：应给出候选，证据带百分比与 100% 阈值。
	percent := 125.0
	payload, err = Attribute(features, AttributionOptions{Load: &LoadInfo{LoadPercent: &percent, BatchCount: 3}})
	if err != nil {
		t.Fatalf("归因失败：%v", err)
	}
	candidate, ok := findCandidate(t, payload, CauseOverstock)
	if !ok {
		t.Fatal("装载超额定时应产生 overstock 候选")
	}
	assertEvidenceNumeric(t, candidate)
	evidence, ok := hasEvidence(candidate, "load_percent")
	if !ok {
		t.Fatal("超量存放候选应带装载占比证据")
	}
	if evidence.Value != 125 || evidence.Threshold != 100 {
		t.Errorf("装载占比证据数值/阈值 = %v/%v，期望 125/100", evidence.Value, evidence.Threshold)
	}
}

// TestAttribute_EmptyFeaturesRejected 特征根本没算过时直接报错：
// 「没算过」与「算了但什么都没发生」必须分开。
func TestAttribute_EmptyFeaturesRejected(t *testing.T) {
	_, err := Attribute(domain.ExcursionFeatures{}, AttributionOptions{})
	if err == nil {
		t.Fatal("空特征应报错")
	}
	if code := errcode.As(err).Code; code != errcode.Validation {
		t.Errorf("错误码 = %q，期望 %q", code, errcode.Validation)
	}
}

// TestAttributeSamples_PowerLossEndToEnd 端到端：真实采样序列 → 特征 → 归因。
// 记录仪自己报了断电，结论就该落在断电。
func TestAttributeSamples_PowerLossEndToEnd(t *testing.T) {
	samples := []domain.TemperatureEvent{
		featSample(0, 5.0, attrPct(58)),
		featSample(5, 5.1, attrPct(58)),
		featSample(10, 8.6, attrPct(58), domain.EventPowerLoss),
		featSample(15, 9.6, attrPct(58), domain.EventPowerLoss),
		featSample(20, 10.4, attrPct(58), domain.EventPowerLoss),
		featSample(25, 6.0, attrPct(58)),
	}

	window, err := ExtractWindow(samples, featZone())
	if err != nil {
		t.Fatalf("抽窗口失败：%v", err)
	}
	if window == nil {
		t.Fatal("期望抽出越温窗口")
	}

	payload, err := AttributeSamples(samples, window, featZone(), AttributionOptions{})
	if err != nil {
		t.Fatalf("归因失败：%v", err)
	}
	if payload.CauseCode != CausePowerLoss {
		t.Fatalf("原因结论 = %q，期望 %q", payload.CauseCode, CausePowerLoss)
	}
	if len(payload.Evidence) == 0 {
		t.Fatal("结论必须带证据")
	}
}

// TestAttributeSamples_DoorOpenEndToEnd 端到端：长时间开门 + 湿度跟着涨 → 开门。
func TestAttributeSamples_DoorOpenEndToEnd(t *testing.T) {
	samples := []domain.TemperatureEvent{
		featSample(0, 5.0, attrPct(58)),
		featSample(5, 5.1, attrPct(58), domain.EventDoorOpen),
		featSample(10, 5.2, attrPct(58), domain.EventDoorOpen),
		featSample(15, 5.3, attrPct(59), domain.EventDoorOpen),
		featSample(20, 8.6, attrPct(62), domain.EventDoorOpen),
		featSample(25, 9.2, attrPct(63), domain.EventDoorOpen),
		featSample(30, 9.6, attrPct(64), domain.EventDoorOpen),
		featSample(35, 6.0, attrPct(60)),
	}

	window, err := ExtractWindow(samples, featZone())
	if err != nil {
		t.Fatalf("抽窗口失败：%v", err)
	}
	payload, err := AttributeSamples(samples, window, featZone(), AttributionOptions{})
	if err != nil {
		t.Fatalf("归因失败：%v", err)
	}
	if payload.CauseCode != CauseDoorOpen {
		t.Fatalf("原因结论 = %q，期望 %q", payload.CauseCode, CauseDoorOpen)
	}
	candidate, _ := findCandidate(t, payload, CauseDoorOpen)
	assertEvidenceNumeric(t, candidate)
}

// TestAttributeSamples_ProbeStuckEndToEnd 端到端：连续恒定读数 → 探头卡死。
func TestAttributeSamples_ProbeStuckEndToEnd(t *testing.T) {
	samples := []domain.TemperatureEvent{
		featSample(0, 5.0, attrPct(58)),
		featSample(5, 5.1, attrPct(58)),
		featSample(10, 9.4, attrPct(58)),
		featSample(15, 9.4, attrPct(58)),
		featSample(20, 9.4, attrPct(58)),
		featSample(25, 9.4, attrPct(58)),
		featSample(30, 5.5, attrPct(58)),
	}

	window, err := ExtractWindow(samples, featZone())
	if err != nil {
		t.Fatalf("抽窗口失败：%v", err)
	}
	payload, err := AttributeSamples(samples, window, featZone(), AttributionOptions{})
	if err != nil {
		t.Fatalf("归因失败：%v", err)
	}
	if payload.CauseCode != CauseProbeStuck {
		t.Fatalf("原因结论 = %q，期望 %q", payload.CauseCode, CauseProbeStuck)
	}
}

// TestAttributeSamples_ACKnowledgeEndToEnd 端到端：带电运行、柜门基本没开、
// 湿度纹丝不动，温度持续上行一小时 → 制冷异常。
func TestAttributeSamples_ACKnowledgeEndToEnd(t *testing.T) {
	samples := []domain.TemperatureEvent{
		featSample(0, 5.0, attrPct(58), domain.EventDoorOpen),
		featSample(5, 5.1, attrPct(58)),
		featSample(10, 5.2, attrPct(58)),
		featSample(15, 8.4, attrPct(58)),
		featSample(20, 8.9, attrPct(58)),
		featSample(25, 9.4, attrPct(58)),
		featSample(30, 9.9, attrPct(58)),
		featSample(35, 10.4, attrPct(58)),
		featSample(40, 10.9, attrPct(58)),
		featSample(45, 11.4, attrPct(58)),
		featSample(50, 11.9, attrPct(58)),
		featSample(55, 12.4, attrPct(58)),
		featSample(60, 12.9, attrPct(58)),
		featSample(65, 13.4, attrPct(58)),
		featSample(70, 13.9, attrPct(58)),
		featSample(75, 14.4, attrPct(58)),
		// 回落是逐渐的：一步掉回温区会被当成探头跳变，
		// 那就变成另一条线索了。
		featSample(80, 11.9, attrPct(58)),
		featSample(85, 10.4, attrPct(58)),
		featSample(90, 9.2, attrPct(58)),
		featSample(95, 8.0, attrPct(58)),
		featSample(100, 7.0, attrPct(58)),
	}

	window, err := ExtractWindow(samples, featZone())
	if err != nil {
		t.Fatalf("抽窗口失败：%v", err)
	}
	if window.DurationMinutes < acFailureMinDurationMinutes {
		t.Fatalf("窗口持续 %d 分钟，短于制冷异常的门槛 %d 分钟，场景构造有问题",
			window.DurationMinutes, acFailureMinDurationMinutes)
	}

	payload, err := AttributeSamples(samples, window, featZone(), AttributionOptions{})
	if err != nil {
		t.Fatalf("归因失败：%v", err)
	}
	if payload.CauseCode != CauseACFailure {
		t.Fatalf("原因结论 = %q，期望 %q", payload.CauseCode, CauseACFailure)
	}
	candidate, _ := findCandidate(t, payload, CauseACFailure)
	assertEvidenceNumeric(t, candidate)
}

// TestAttributeSamples_InsufficientEvidenceEndToEnd 端到端：设备什么事件都不报，
// 现场特征也齐不起来 → 原因待查，并给出下一步该查什么。
func TestAttributeSamples_InsufficientEvidenceEndToEnd(t *testing.T) {
	samples := []domain.TemperatureEvent{
		featSample(0, 5.0, attrPct(58)),
		featSample(5, 5.1, attrPct(58)),
		featSample(10, 8.6, attrPct(58)),
		featSample(15, 9.1, attrPct(58)),
		featSample(20, 9.6, attrPct(58)),
		featSample(25, 6.0, attrPct(58)),
	}

	window, err := ExtractWindow(samples, featZone())
	if err != nil {
		t.Fatalf("抽窗口失败：%v", err)
	}
	payload, err := AttributeSamples(samples, window, featZone(), AttributionOptions{})
	if err != nil {
		t.Fatalf("归因失败：%v", err)
	}
	if payload.CauseCode != CauseUnknown {
		t.Fatalf("原因结论 = %q，期望 %q", payload.CauseCode, CauseUnknown)
	}
	if len(payload.MissingClues) == 0 {
		t.Fatal("原因待查时必须列出还差哪条线索")
	}
}
