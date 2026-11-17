package rules

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/fixture"
)

// 本文件测的是归因特征工程：
//   - 温升速率（按采样点数折算，缺口处不连线）；
//   - 峰值偏离度、湿度基线与上升幅度；
//   - 开门累计时长、开门与温升的滞后、断电与除霜；
//   - 相邻采样跳变（探头脱落的线索）；
//   - 缺测特征写 nil 并列入 missing_features，不用 0 顶替。
//
// 样例统一用 5 分钟一采的栅格（hystZone：温区 2~8℃、采样周期 5 分钟、
// 最短越限 10 分钟、迟滞带 1℃），对应现场常见配置。

// featSample 造一条带湿度与设备事件的采样。
func featSample(offsetMin int, temperatureC float64, humidityPct *float64, events ...string) domain.TemperatureEvent {
	s := sampleAt(offsetMin, temperatureC)
	s.HumidityPct = humidityPct
	s.Events = events
	return s
}

// featPct 造一个浮点指针（缺测就是 nil，不用 0 冒充）。
func featPct(v float64) *float64 { return &v }

// featZone 温区 2~8℃、采样周期 5 分钟、最短越限 10 分钟、迟滞带 1℃。
func featZone() ZoneDecision { return hystZone(10) }

// TestExtractFeatures_NormalRiseRate 正常斜率：
// 温升速率按连续观测段起点到有效峰值折算，
// 历时按采样点数算（每条采样记一个采样周期），不按墙钟。
func TestExtractFeatures_NormalRiseRate(t *testing.T) {
	samples := []domain.TemperatureEvent{
		featSample(0, 5.0, featPct(58)),
		featSample(5, 5.2, featPct(58)),
		featSample(10, 5.1, featPct(58)),
		featSample(15, 8.6, featPct(62)), // 越限
		featSample(20, 9.1, featPct(62)), // 越限
		featSample(25, 9.6, featPct(62)), // 越限，峰值
		featSample(30, 6.9, featPct(59)), // 回落
		featSample(35, 5.8, featPct(58)),
	}

	window, err := ExtractWindow(samples, featZone())
	if err != nil {
		t.Fatalf("期望抽出越温窗口，实际报错：%v", err)
	}
	if window == nil {
		t.Fatal("期望抽出越温窗口，实际为 nil")
	}

	f, err := ExtractFeatures(samples, window, featZone())
	if err != nil {
		t.Fatalf("期望提取归因特征，实际报错：%v", err)
	}

	// 起点 t=0 的 5.0℃ → 峰值 t=25 的 9.6℃，历时 5 个采样周期 = 25 分钟。
	if f.RiseRatePerHour == nil {
		t.Fatal("温升速率不该缺测")
	}
	wantRate := (9.6 - 5.0) / (25.0 / 60.0)
	if diff := *f.RiseRatePerHour - wantRate; diff > 0.001 || diff < -0.001 {
		t.Errorf("温升速率 = %.4f℃/小时，期望 %.4f℃/小时", *f.RiseRatePerHour, wantRate)
	}
	if got, want := f.RiseRateSamples, 6; got != want {
		t.Errorf("参与折算的采样条数 = %d，期望 %d", got, want)
	}
	if got, want := f.RiseRatePeakC, 9.6; got != want {
		t.Errorf("有效峰值 = %.2f℃，期望 %.2f℃", got, want)
	}
	if f.Missing(domain.FeatureRiseRatePerHour) {
		t.Error("温升速率不该出现在 missing_features 里")
	}

	// 峰值偏离度：9.6℃ 超温区上界 8℃ 达 1.6℃。
	if f.PeakDeviationC == nil {
		t.Fatal("峰值偏离度不该缺测")
	}
	if got, want := *f.PeakDeviationC, 1.6; got < want-0.001 || got > want+0.001 {
		t.Errorf("峰值偏离度 = %.2f℃，期望 %.2f℃", got, want)
	}
	if got, want := f.PeakDeviationSide, domain.ExcursionSideHigh; got != want {
		t.Errorf("偏离侧别 = %q，期望 %q", got, want)
	}

	// 湿度基线取窗口之前温区内读数的中位数（58），窗口内峰值 62，上升 4 个百分点。
	if f.HumidityRisePct == nil {
		t.Fatal("湿度上升幅度不该缺测")
	}
	if got, want := *f.HumidityRisePct, 4.0; got < want-0.001 || got > want+0.001 {
		t.Errorf("湿度上升幅度 = %.2f 个百分点，期望 %.2f", got, want)
	}
	if f.HumidityBaselinePct == nil || *f.HumidityBaselinePct != 58 {
		t.Errorf("湿度基线 = %v，期望 58", f.HumidityBaselinePct)
	}
	if got, want := f.HumidityBaselineSamples, 3; got != want {
		t.Errorf("湿度基线采样条数 = %d，期望 %d", got, want)
	}

	// 相邻采样没有超过 5℃ 的跳变。
	if f.SampleJumpCount == nil {
		t.Fatal("相邻采样跳变不该缺测")
	}
	if got, want := *f.SampleJumpCount, 0; got != want {
		t.Errorf("跳变次数 = %d，期望 %d", got, want)
	}
	if f.MaxSampleJumpC == nil || *f.MaxSampleJumpC < 3.49 || *f.MaxSampleJumpC > 3.51 {
		t.Errorf("最大跳变 = %v，期望 3.50", f.MaxSampleJumpC)
	}
}

// TestExtractFeatures_GapSegmentRiseRate 缺口段斜率折算：
// 数据缺口处不连线——缺口之后的温升历时只按该段自己的采样条数折算，
// 不能把缺口两头的读数接成一段连续温升。
func TestExtractFeatures_GapSegmentRiseRate(t *testing.T) {
	samples := []domain.TemperatureEvent{
		featSample(0, 5.0, featPct(58)),
		featSample(5, 8.6, featPct(60)),  // 越限
		featSample(10, 9.1, featPct(60)), // 越限
		// 缺 40 分钟（超过 3 个采样周期），这段时间没有读数。
		featSample(50, 11.0, featPct(60)), // 越限
		featSample(55, 12.4, featPct(60)), // 越限，峰值
	}

	window, err := ExtractWindow(samples, featZone())
	if err != nil {
		t.Fatalf("期望抽出越温窗口，实际报错：%v", err)
	}
	if window == nil {
		t.Fatal("期望抽出越温窗口，实际为 nil")
	}
	// 持续时长按采样点数折算：4 条越限采样 = 3 个周期 = 15 分钟，缺口不计入。
	if got, want := window.DurationMinutes, 15; got != want {
		t.Errorf("窗口持续 = %d 分钟，期望 %d 分钟（缺口不计入）", got, want)
	}

	f, err := ExtractFeatures(samples, window, featZone())
	if err != nil {
		t.Fatalf("期望提取归因特征，实际报错：%v", err)
	}

	// 温升只在缺口之后那一段里算：t=50 的 11.0℃ → t=55 的 12.4℃，
	// 历时 1 个采样周期 = 5 分钟 → 1.4 / (5/60) = 16.8℃/小时。
	if f.RiseRatePerHour == nil {
		t.Fatal("温升速率不该缺测")
	}
	wantRate := 1.4 / (5.0 / 60.0)
	if diff := *f.RiseRatePerHour - wantRate; diff > 0.001 || diff < -0.001 {
		t.Errorf("温升速率 = %.4f℃/小时，期望 %.4f℃/小时（缺口处不连线）", *f.RiseRatePerHour, wantRate)
	}
	if got, want := f.RiseRateSamples, 2; got != want {
		t.Errorf("参与折算的采样条数 = %d，期望 %d", got, want)
	}
	if got, want := f.RiseRateStartC, 11.0; got != want {
		t.Errorf("温升起点读数 = %.2f℃，期望 %.2f℃", got, want)
	}
	if got, want := f.RiseRateExcludedGapMinutes, 35; got != want {
		t.Errorf("因缺口未计入的分钟数 = %d，期望 %d", got, want)
	}

	// 跨缺口的相邻采样对不算跳变：缺 40 分钟，读数差 1.9℃ 不该被算成突变。
	if f.SampleJumpCount == nil {
		t.Fatal("相邻采样跳变不该缺测")
	}
	if got, want := *f.SampleJumpCount, 0; got != want {
		t.Errorf("跳变次数 = %d，期望 %d（跨缺口不计）", got, want)
	}
	if f.MaxSampleJumpC == nil || *f.MaxSampleJumpC > 3.7 {
		t.Errorf("最大跳变 = %v，期望不超过 3.6（跨缺口那一对不计）", f.MaxSampleJumpC)
	}
}

// TestExtractFeatures_MissingHumidityIsNilNotZero 缺测特征写 nil：
// 设备没上报湿度时，湿度上升幅度是 nil 并列入 missing_features，
// 不能拿 0 冒充——「没上报」与「没变化」在归因时是两回事。
func TestExtractFeatures_MissingHumidityIsNilNotZero(t *testing.T) {
	samples := []domain.TemperatureEvent{
		featSample(0, 5.0, nil),
		featSample(5, 8.6, nil),  // 越限
		featSample(10, 9.1, nil), // 越限
		featSample(15, 9.6, nil), // 越限，峰值
		featSample(20, 6.0, nil), // 回落
	}

	window, err := ExtractWindow(samples, featZone())
	if err != nil {
		t.Fatalf("期望抽出越温窗口，实际报错：%v", err)
	}

	f, err := ExtractFeatures(samples, window, featZone())
	if err != nil {
		t.Fatalf("期望提取归因特征，实际报错：%v", err)
	}

	if f.HumidityRisePct != nil {
		t.Errorf("缺测的湿度上升幅度 = %v，期望 nil", *f.HumidityRisePct)
	}
	if f.HumidityPeakPct != nil {
		t.Errorf("缺测的湿度峰值 = %v，期望 nil", *f.HumidityPeakPct)
	}
	if !f.Missing(domain.FeatureHumidityRisePct) {
		t.Errorf("missing_features = %v，期望含 %s", f.MissingFeatures, domain.FeatureHumidityRisePct)
	}
	// 有温升速率就不该被算成缺测。
	if f.RiseRatePerHour == nil {
		t.Error("温升速率不该缺测")
	}
	if f.Missing(domain.FeatureRiseRatePerHour) {
		t.Error("温升速率不该出现在 missing_features 里")
	}

	// 序列化后缺测特征是 null，不是 0。
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("序列化归因特征失败：%v", err)
	}
	if !strings.Contains(string(data), `"humidity_rise_pct":null`) {
		t.Errorf("序列化结果里缺测特征应为 null：%s", string(data))
	}
	if strings.Contains(string(data), `"humidity_rise_pct":0`) {
		t.Errorf("缺测特征不得以 0 冒充：%s", string(data))
	}
}

// TestExtractFeatures_DoorOpenDurationAccumulated 开门时长累计：
// 门开了三条采样就累计三个采样周期；
// 开门与温升起点的滞后按最近一次开门算，把「开门」和「温升」在时间上接起来。
func TestExtractFeatures_DoorOpenDurationAccumulated(t *testing.T) {
	samples := []domain.TemperatureEvent{
		featSample(0, 5.0, featPct(58), domain.EventDoorOpen),
		featSample(5, 5.4, featPct(58), domain.EventDoorOpen),
		featSample(10, 5.8, featPct(59), domain.EventDoorOpen),
		featSample(15, 8.6, featPct(64)), // 越限，温升起点
		featSample(20, 9.4, featPct(64)), // 越限
		featSample(25, 9.8, featPct(64)), // 越限，峰值
		featSample(30, 6.5, featPct(60)), // 回落
	}

	window, err := ExtractWindow(samples, featZone())
	if err != nil {
		t.Fatalf("期望抽出越温窗口，实际报错：%v", err)
	}
	if window == nil {
		t.Fatal("期望抽出越温窗口，实际为 nil")
	}
	if got, want := window.Start, sampleAt(15, 0).MeasuredAt; !got.Equal(want) {
		t.Fatalf("窗口起点 = %s，期望 %s", got, want)
	}

	f, err := ExtractFeatures(samples, window, featZone())
	if err != nil {
		t.Fatalf("期望提取归因特征，实际报错：%v", err)
	}

	if f.DoorOpenMinutes == nil {
		t.Fatal("开门时长不该缺测")
	}
	// 三条开门采样 × 5 分钟 = 15 分钟。
	if got, want := *f.DoorOpenMinutes, 15; got != want {
		t.Errorf("开门累计时长 = %d 分钟，期望 %d 分钟", got, want)
	}
	if got, want := *f.DoorOpenCount, 3; got != want {
		t.Errorf("开门次数 = %d，期望 %d", got, want)
	}
	if !f.DeviceEventsReported {
		t.Error("设备上报过开门事件，DeviceEventsReported 应为真")
	}
	if f.DoorOpenAt == nil {
		t.Fatal("首次开门时刻不该缺测")
	}
	if got, want := *f.DoorOpenAt, sampleAt(0, 0).MeasuredAt; !got.Equal(want) {
		t.Errorf("首次开门时刻 = %s，期望 %s", got, want)
	}
	// 最近一次开门在 t=10，温升起点在 t=15，滞后 5 分钟。
	if f.DoorToRiseDelayMinutes == nil {
		t.Fatal("开门与温升的滞后不该缺测")
	}
	if got, want := *f.DoorToRiseDelayMinutes, 5; got != want {
		t.Errorf("开门到温升的滞后 = %d 分钟，期望 %d 分钟", got, want)
	}
	// 湿空气跟着进来了：基线 58（窗口之前温区内读数），峰值 64，上升 6 个百分点。
	if f.HumidityRisePct == nil {
		t.Fatal("湿度上升幅度不该缺测")
	}
	if got, want := *f.HumidityRisePct, 6.0; got < want-0.001 || got > want+0.001 {
		t.Errorf("湿度上升幅度 = %.2f 个百分点，期望 %.2f", got, want)
	}
}

// TestExtractFeatures_DoorOpenReachesWindowStart 开门一直延续到温升起点：
// 滞后为 0（开门那一刻温度就已在上升），开门时长按观察到的条数累计。
func TestExtractFeatures_DoorOpenReachesWindowStart(t *testing.T) {
	samples := []domain.TemperatureEvent{
		featSample(0, 5.0, featPct(58), domain.EventDoorOpen),
		featSample(5, 5.4, featPct(58), domain.EventDoorOpen),
		featSample(10, 5.8, featPct(58), domain.EventDoorOpen),
		featSample(15, 8.6, featPct(64), domain.EventDoorOpen), // 越限且仍在开门
		featSample(20, 9.4, featPct(64)),
		featSample(25, 9.8, featPct(64)),
		featSample(30, 6.5, featPct(60)),
	}

	window, err := ExtractWindow(samples, featZone())
	if err != nil {
		t.Fatalf("期望抽出越温窗口，实际报错：%v", err)
	}

	f, err := ExtractFeatures(samples, window, featZone())
	if err != nil {
		t.Fatalf("期望提取归因特征，实际报错：%v", err)
	}
	if f.DoorOpenMinutes == nil || *f.DoorOpenMinutes != 20 {
		t.Errorf("开门累计时长 = %v，期望 20 分钟", f.DoorOpenMinutes)
	}
	if f.DoorToRiseDelayMinutes == nil || *f.DoorToRiseDelayMinutes != 0 {
		t.Errorf("开门到温升的滞后 = %v，期望 0 分钟", f.DoorToRiseDelayMinutes)
	}
}

// TestExtractFeatures_PowerLossAndDefrostObserved 断电与除霜事件：
// 两类事件的存在性、首次发生时点与次数都要能从观测段里读出来。
func TestExtractFeatures_PowerLossAndDefrostObserved(t *testing.T) {
	samples := []domain.TemperatureEvent{
		featSample(0, 5.0, featPct(57)),
		featSample(5, 5.1, featPct(57)),
		featSample(10, 8.2, featPct(56), domain.EventPowerLoss),
		featSample(15, 9.8, featPct(56), domain.EventPowerLoss),
		featSample(20, 11.4, featPct(56), domain.EventPowerLoss),
		featSample(25, 12.1, featPct(56)),
		featSample(30, 6.0, featPct(57)),
	}

	window, err := ExtractWindow(samples, featZone())
	if err != nil {
		t.Fatalf("期望抽出越温窗口，实际报错：%v", err)
	}

	f, err := ExtractFeatures(samples, window, featZone())
	if err != nil {
		t.Fatalf("期望提取归因特征，实际报错：%v", err)
	}

	if f.PowerLossObserved == nil || !*f.PowerLossObserved {
		t.Errorf("断电事件存在性 = %v，期望为真", f.PowerLossObserved)
	}
	if got, want := f.PowerLossCount, 3; got != want {
		t.Errorf("断电事件次数 = %d，期望 %d", got, want)
	}
	if f.PowerLossAt == nil {
		t.Fatal("首次断电时刻不该缺测")
	}
	if got, want := *f.PowerLossAt, sampleAt(10, 0).MeasuredAt; !got.Equal(want) {
		t.Errorf("首次断电时刻 = %s，期望 %s", got, want)
	}
	// 上报过事件却一次都没报除霜：那是「确实没除霜」，不是「不知道」。
	if f.DefrostObserved == nil || *f.DefrostObserved {
		t.Errorf("除霜事件存在性 = %v，期望为假（设备上报过事件）", f.DefrostObserved)
	}
	// 没有开门事件 → 开门时长是观察到的 0，不是缺测。
	if f.DoorOpenMinutes == nil || *f.DoorOpenMinutes != 0 {
		t.Errorf("开门累计时长 = %v，期望 0（设备上报过事件，属观察到的零值）", f.DoorOpenMinutes)
	}
	if f.Missing(domain.FeatureDoorOpenMinutes) {
		t.Error("设备上报过事件时，开门时长不该被算成缺测")
	}
	// 断电时压缩机停了，箱内湿度纹丝不动：基线 57，峰值 56，上升为负。
	if f.HumidityRisePct == nil {
		t.Fatal("湿度上升幅度不该缺测")
	}
	if got, want := *f.HumidityRisePct, -1.0; got < want-0.001 || got > want+0.001 {
		t.Errorf("湿度上升幅度 = %.2f 个百分点，期望 %.2f", got, want)
	}
}

// TestExtractFeatures_NoDeviceEventsReportedIsMissing 设备不上报事件：
// 分不出「没开门」与「这台设备根本不上报开门」，这一项只能记缺测，
// 不能写成 0——0 会被当成「查过了，确实没开」。
func TestExtractFeatures_NoDeviceEventsReportedIsMissing(t *testing.T) {
	samples := []domain.TemperatureEvent{
		featSample(0, 5.0, featPct(58)),
		featSample(5, 8.6, featPct(58)),
		featSample(10, 9.1, featPct(58)),
		featSample(15, 9.6, featPct(58)),
		featSample(20, 6.0, featPct(58)),
	}

	window, err := ExtractWindow(samples, featZone())
	if err != nil {
		t.Fatalf("期望抽出越温窗口，实际报错：%v", err)
	}

	f, err := ExtractFeatures(samples, window, featZone())
	if err != nil {
		t.Fatalf("期望提取归因特征，实际报错：%v", err)
	}

	if f.DeviceEventsReported {
		t.Error("这台设备没有上报过任何事件，DeviceEventsReported 应为假")
	}
	if f.DoorOpenMinutes != nil {
		t.Errorf("缺测的开门时长 = %v，期望 nil", *f.DoorOpenMinutes)
	}
	if f.PowerLossObserved != nil {
		t.Errorf("缺测的断电存在性 = %v，期望 nil", *f.PowerLossObserved)
	}
	if f.DefrostObserved != nil {
		t.Errorf("缺测的除霜存在性 = %v，期望 nil", *f.DefrostObserved)
	}
	if f.DoorToRiseDelayMinutes != nil {
		t.Errorf("缺测的开门滞后 = %v，期望 nil", *f.DoorToRiseDelayMinutes)
	}
	for _, name := range []string{
		domain.FeatureDoorOpenMinutes,
		domain.FeaturePowerLossObserved,
		domain.FeatureDefrostObserved,
		domain.FeatureDoorToRiseDelayMinutes,
	} {
		if !f.Missing(name) {
			t.Errorf("missing_features = %v，期望含 %s", f.MissingFeatures, name)
		}
	}
}

// TestExtractFeatures_SampleJumpMarksProbeDetachment 相邻采样跳变：
// 一个采样周期里跳 9℃ 在疫苗冷藏箱里不可能是真实变化，
// 它是探头脱落或接触不良的线索，与探头卡死（读数恒定）互为反面。
func TestExtractFeatures_SampleJumpMarksProbeDetachment(t *testing.T) {
	samples := []domain.TemperatureEvent{
		featSample(0, 5.0, featPct(58)),
		featSample(5, 14.0, featPct(58)), // 一个周期跳 9℃
		featSample(10, 13.5, featPct(58)),
		featSample(15, 13.0, featPct(58)),
		featSample(20, 12.0, featPct(58)),
		featSample(25, 7.5, featPct(58)), // 回落（迟滞带内，窗口仍托住）
	}

	window, err := ExtractWindow(samples, featZone())
	if err != nil {
		t.Fatalf("期望抽出越温窗口，实际报错：%v", err)
	}

	f, err := ExtractFeatures(samples, window, featZone())
	if err != nil {
		t.Fatalf("期望提取归因特征，实际报错：%v", err)
	}

	if f.MaxSampleJumpC == nil {
		t.Fatal("最大跳变不该缺测")
	}
	if got, want := *f.MaxSampleJumpC, 9.0; got < want-0.001 || got > want+0.001 {
		t.Errorf("最大跳变 = %.2f℃，期望 %.2f℃", got, want)
	}
	if f.SampleJumpCount == nil {
		t.Fatal("跳变次数不该缺测")
	}
	if got, want := *f.SampleJumpCount, 1; got != want {
		t.Errorf("跳变次数 = %d，期望 %d", got, want)
	}
	if got, want := f.SampleJumpThresholdC, sampleJumpThresholdC; got != want {
		t.Errorf("跳变阈值 = %.2f℃，期望 %.2f℃", got, want)
	}
	// 读数没有恒定段 → 不是探头卡死。
	if got := f.ProbeStuckSampleCount; got != 0 {
		t.Errorf("探头卡死采样条数 = %d，期望 0", got)
	}
}

// TestExtractFeatures_AccuracyDeclaration 精度声明：
// 设备自报的精度、抗规范上限的标注都要随特征带出去，
// 未声明时写 nil 并列入 missing_features。
func TestExtractFeatures_AccuracyDeclaration(t *testing.T) {
	samples := []domain.TemperatureEvent{
		featSample(0, 5.0, featPct(58)),
		featSample(5, 8.6, featPct(58)),
		featSample(10, 9.1, featPct(58)),
		featSample(15, 9.6, featPct(58)),
		featSample(20, 6.0, featPct(58)),
	}

	zone := featZone()
	zone.Precision = domain.PrecisionVerdict{
		Limited:     false,
		LimitC:      0.5,
		LimitSource: domain.PrecisionLimitDeviceClass,
		DeclaredC:   featPct(0.3),
	}

	window, err := ExtractWindow(samples, zone)
	if err != nil {
		t.Fatalf("期望抽出越温窗口，实际报错：%v", err)
	}
	f, err := ExtractFeatures(samples, window, zone)
	if err != nil {
		t.Fatalf("期望提取归因特征，实际报错：%v", err)
	}
	if f.AccuracyC == nil || *f.AccuracyC != 0.3 {
		t.Errorf("设备自报精度 = %v，期望 0.3", f.AccuracyC)
	}
	if got, want := f.AccuracyLimitC, 0.5; got != want {
		t.Errorf("精度上限 = %.2f℃，期望 %.2f℃", got, want)
	}
	if f.PrecisionLimited {
		t.Error("精度 0.3℃ 未超上限，PrecisionLimited 应为假")
	}
	if f.Missing(domain.FeatureAccuracyC) {
		t.Error("设备已声明精度，不该出现在 missing_features 里")
	}

	// 未声明精度 → nil + 缺测。
	zone.Precision.DeclaredC = nil
	f2, err := ExtractFeatures(samples, window, zone)
	if err != nil {
		t.Fatalf("期望提取归因特征，实际报错：%v", err)
	}
	if f2.AccuracyC != nil {
		t.Errorf("未声明的精度 = %v，期望 nil", *f2.AccuracyC)
	}
	if !f2.Missing(domain.FeatureAccuracyC) {
		t.Errorf("missing_features = %v，期望含 %s", f2.MissingFeatures, domain.FeatureAccuracyC)
	}

	// 精度不足 → 标注随特征带出去。
	zone.Precision = domain.PrecisionVerdict{
		Limited:     true,
		LimitC:      0.5,
		LimitSource: domain.PrecisionLimitPointProfile,
		DeclaredC:   featPct(1.2),
		Tag:         PrecisionLimitedTag,
		Detail:      "设备声明测温精度 1.20℃，超出该类设备规范上限 0.50℃",
	}
	f3, err := ExtractFeatures(samples, window, zone)
	if err != nil {
		t.Fatalf("期望提取归因特征，实际报错：%v", err)
	}
	if !f3.PrecisionLimited {
		t.Error("精度超上限时 PrecisionLimited 应为真")
	}
	if got, want := f3.PrecisionTag, PrecisionLimitedTag; got != want {
		t.Errorf("精度标注文案 = %q，期望 %q", got, want)
	}
}

// TestExtractFeatures_NoWindowRejected 没有窗口就没有特征：
// 硬凑一组特征出来比不给更危险。
func TestExtractFeatures_NoWindowRejected(t *testing.T) {
	samples := []domain.TemperatureEvent{featSample(0, 5.0, featPct(58))}

	_, err := ExtractFeatures(samples, nil, featZone())
	if err == nil {
		t.Fatal("期望报错，实际未报错")
	}
	if code := errcode.As(err).Code; code != errcode.Validation {
		t.Errorf("错误码 = %s，期望 %s", code, errcode.Validation)
	}
}

// TestExtractFeatures_UnusableZoneRejected 温区判不了就不出特征：
// 峰值偏离度与湿度基线都按温区算，温区不可信时算出来的特征整个是错的。
func TestExtractFeatures_UnusableZoneRejected(t *testing.T) {
	samples := []domain.TemperatureEvent{
		featSample(0, 5.0, featPct(58)),
		featSample(5, 8.6, featPct(58)),
		featSample(10, 9.1, featPct(58)),
	}

	zone := featZone()
	zone.NeedsManual = true
	zone.Reason = "缺少点位温区配置"

	_, err := ExtractFeatures(samples, &domain.ExcursionWindow{Start: samples[1].MeasuredAt, End: samples[2].MeasuredAt}, zone)
	if err == nil {
		t.Fatal("期望报错，实际未报错")
	}
	if code := errcode.As(err).Code; code != errcode.Validation {
		t.Errorf("错误码 = %s，期望 %s", code, errcode.Validation)
	}
	if !strings.Contains(err.Error(), "温区判定不可用") {
		t.Errorf("错误信息应写明温区判定不可用，实际：%v", err)
	}
}

// TestExtractFeatures_WindowNotInSamples 采样里找不到窗口：
// 明确报错，不给一组对不上的特征。
func TestExtractFeatures_WindowNotInSamples(t *testing.T) {
	samples := []domain.TemperatureEvent{
		featSample(0, 5.0, featPct(58)),
		featSample(5, 5.1, featPct(58)),
	}

	stale := domain.ExcursionWindow{
		Start: time.Date(2026, 10, 19, 3, 0, 0, 0, time.FixedZone("CST", 8*3600)),
		End:   time.Date(2026, 10, 19, 3, 30, 0, 0, time.FixedZone("CST", 8*3600)),
	}

	_, err := ExtractFeatures(samples, &stale, featZone())
	if err == nil {
		t.Fatal("期望报错，实际未报错")
	}
	if code := errcode.As(err).Code; code != errcode.NotFound {
		t.Errorf("错误码 = %s，期望 %s", code, errcode.NotFound)
	}
}

// TestExtractFeatures_PowerLossWindowOnRealSample 真实脱敏样例上的断电窗口：
// 用仓库里的记录仪导出样例跑一遍——断电造成的那段越温，箱内湿度没有跟着涨，
// 而开门那次湿度跟着涨。这是把「开门」与「断电」分得开的关键特征，
// 必须在真实数据形态上成立，不能只在人造样例里对。
func TestExtractFeatures_PowerLossWindowOnRealSample(t *testing.T) {
	samples, err := fixture.LoadRecordings("../../testdata/recordings_sample.csv")
	if err != nil {
		t.Fatalf("读取脱敏样例失败：%v", err)
	}

	windows, err := ExtractWindows(samples, featZone())
	if err != nil {
		t.Fatalf("期望抽取窗口成功，实际报错：%v", err)
	}

	// 样例里有三段越限：开门（峰值 9.6℃）、断电（峰值 15.2℃）、除霜（峰值 9.2℃）。
	var powerLoss *domain.ExcursionWindow
	var doorOpen *domain.ExcursionWindow
	for i := range windows {
		switch windows[i].PeakC {
		case 15.2:
			powerLoss = &windows[i]
		case 9.6:
			doorOpen = &windows[i]
		}
	}
	if powerLoss == nil || doorOpen == nil {
		t.Fatalf("样例里应同时存在断电窗口与开门窗口，实际窗口数 %d", len(windows))
	}

	// 断电窗口：断电事件在观测范围内，湿度没有跟着涨，开门事件不在本次范围内。
	f, err := ExtractFeatures(samples, powerLoss, featZone())
	if err != nil {
		t.Fatalf("期望提取断电窗口特征，实际报错：%v", err)
	}
	if f.PowerLossObserved == nil || !*f.PowerLossObserved {
		t.Errorf("断电事件存在性 = %v，期望为真", f.PowerLossObserved)
	}
	if f.PowerLossCount == 0 {
		t.Error("断电事件次数不该为 0")
	}
	if f.HumidityRisePct == nil {
		t.Fatal("湿度上升幅度不该缺测")
	}
	if *f.HumidityRisePct > 1.0 {
		t.Errorf("断电时湿度上升幅度 = %.2f 个百分点，期望不超过 1.0（断电不带进湿空气）", *f.HumidityRisePct)
	}
	if f.DoorOpenMinutes == nil || *f.DoorOpenMinutes != 0 {
		t.Errorf("断电窗口的开门时长 = %v，期望 0（开门那次属上一次异常）", f.DoorOpenMinutes)
	}
	if f.DoorToRiseDelayMinutes == nil || *f.DoorToRiseDelayMinutes < 60 {
		t.Errorf("断电窗口的开门滞后 = %v，期望至少 60 分钟（开门远早于本次温升）", f.DoorToRiseDelayMinutes)
	}

	// 开门窗口：开门在观测范围内，湿空气跟着进来了。
	f2, err := ExtractFeatures(samples, doorOpen, featZone())
	if err != nil {
		t.Fatalf("期望提取开门窗口特征，实际报错：%v", err)
	}
	if f2.DoorOpenMinutes == nil || *f2.DoorOpenMinutes <= 0 {
		t.Errorf("开门窗口的开门时长 = %v，期望大于 0", f2.DoorOpenMinutes)
	}
	if f2.PowerLossObserved == nil || *f2.PowerLossObserved {
		t.Errorf("开门窗口的断电存在性 = %v，期望为假（断电属下一次异常）", f2.PowerLossObserved)
	}
	if f2.HumidityRisePct == nil {
		t.Fatal("湿度上升幅度不该缺测")
	}
	if *f2.HumidityRisePct < 3.0 {
		t.Errorf("开门时湿度上升幅度 = %.2f 个百分点，期望至少 3.0（开门把湿空气带进来了）", *f2.HumidityRisePct)
	}
}
