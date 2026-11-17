package assessment

import (
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/fixture"
)

func sample(offsetMin int, temperatureC float64) domain.TemperatureEvent {
	base := time.Date(2026, 10, 20, 3, 0, 0, 0, time.FixedZone("CST", 8*3600))
	return domain.TemperatureEvent{
		PointID:      "point-vax-freezer-01",
		MeasuredAt:   base.Add(time.Duration(offsetMin) * time.Minute),
		TemperatureC: temperatureC,
	}
}

func profile() *domain.PointProfile {
	return &domain.PointProfile{
		PointID:  "point-vax-freezer-01",
		ZoneMinC: 2,
		ZoneMaxC: 8,
	}
}

func TestAssess_ProducesRunAndWindow(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sample(0, 5.0),
		sample(5, 8.6),
		sample(10, 9.1),
		sample(15, 5.0),
	}

	result, err := Assess(samples, profile())
	if err != nil {
		t.Fatalf("期望判定成功，实际报错：%v", err)
	}
	if result.Run.State != domain.RunStateAssessed {
		t.Errorf("run 状态 = %s，期望 %s", result.Run.State, domain.RunStateAssessed)
	}
	if result.Run.RunID == "" {
		t.Error("run_id 不应为空")
	}
	if result.Run.ExternalRef == "" {
		t.Error("external_ref 不应为空")
	}
	if result.Run.IdempotencyKey == "" {
		t.Error("idempotency_key 不应为空")
	}
	if result.Run.WindowStart == nil || result.Run.WindowEnd == nil {
		t.Fatal("窗口起止不应为空")
	}
	if result.Window == nil {
		t.Fatal("期望给出越温窗口，实际为 nil")
	}
	if got, want := result.Window.PeakC, 9.1; got != want {
		t.Errorf("峰值 = %.2f℃，期望 %.2f℃", got, want)
	}
	if got, want := result.Window.DurationMinutes, 5; got != want {
		t.Errorf("持续分钟 = %d，期望 %d", got, want)
	}
}

func TestAssess_NoExcursionKeepsRunDetected(t *testing.T) {
	samples := []domain.TemperatureEvent{sample(0, 5.0), sample(5, 5.2)}

	result, err := Assess(samples, profile())
	if err != nil {
		t.Fatalf("未越限不是失败，实际报错：%v", err)
	}
	if result.Window != nil {
		t.Errorf("未越限时期望 nil 窗口，实际得到 %+v", result.Window)
	}
	if result.Run.State != domain.RunStateDetected {
		t.Errorf("run 状态 = %s，期望 %s", result.Run.State, domain.RunStateDetected)
	}
	if result.Excursions == nil {
		t.Fatal("越限汇总不该为空：没窗口时它是唯一能说明「到底越没越限」的地方")
	}
	if result.Excursions.SegmentCount != 0 {
		t.Errorf("越限段数 = %d，期望 0", result.Excursions.SegmentCount)
	}
}

// TestAssess_ShortExcursionReportsExcursionSummary 未达最短越限时长时不给窗口，
// 但必须把越限情况报出来：没成立窗口不等于没发生过越限。
func TestAssess_ShortExcursionReportsExcursionSummary(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sample(0, 5.0),
		sample(5, 8.6), // 越限
		sample(10, 5.2),
	}

	thresholdProfile := profile()
	thresholdProfile.SampleIntervalSec = 300
	thresholdProfile.MinExcursionMinutes = 10

	result, err := Assess(samples, thresholdProfile)
	if err != nil {
		t.Fatalf("未达门槛不是失败，实际报错：%v", err)
	}
	if result.Window != nil {
		t.Fatalf("未达门槛时期望 nil 窗口，实际得到 %+v", result.Window)
	}
	if result.Excursions == nil {
		t.Fatal("越限汇总不该为空")
	}
	if got, want := result.Excursions.SegmentCount, 1; got != want {
		t.Errorf("越限段数 = %d，期望 %d", got, want)
	}
	if got, want := result.Excursions.OutOfRangeSamples, 1; got != want {
		t.Errorf("越限读数条数 = %d，期望 %d", got, want)
	}
	if result.Excursions.MeetsThreshold() {
		t.Error("未达门槛的短时波动不该被判为达到门槛")
	}
}

func TestAssess_MissingProfileGoesNeedsManual(t *testing.T) {
	result, err := Assess([]domain.TemperatureEvent{sample(0, 5.0)}, nil)
	if err == nil {
		t.Fatal("缺少温区配置时期望报错")
	}
	if e := errcode.As(err); e.Code != errcode.Validation {
		t.Errorf("错误码 = %s，期望 %s", e.Code, errcode.Validation)
	}
	if result.Run.State != domain.RunStateNeedsManual {
		t.Errorf("run 状态 = %s，期望 %s", result.Run.State, domain.RunStateNeedsManual)
	}
	if result.NeedsManualReason == "" {
		t.Error("转人工时必须写明原因")
	}
	if result.Window != nil {
		t.Errorf("配置缺失时期望不产出窗口结论，实际得到 %+v", result.Window)
	}
}

func TestAssess_PointMismatchGoesNeedsManual(t *testing.T) {
	other := sample(0, 5.0)
	other.PointID = "point-vaccine-fridge-02"

	result, err := Assess([]domain.TemperatureEvent{other}, profile())
	if err == nil {
		t.Fatal("点位不一致时期望报错")
	}
	if result.Run.State != domain.RunStateNeedsManual {
		t.Errorf("run 状态 = %s，期望 %s", result.Run.State, domain.RunStateNeedsManual)
	}
	if result.Window != nil {
		t.Errorf("点位不一致时期望不产出窗口结论，实际得到 %+v", result.Window)
	}
}

func TestAssess_MixedPointsGoesNeedsManual(t *testing.T) {
	other := sample(5, 5.2)
	other.PointID = "point-vaccine-fridge-02"

	result, err := Assess([]domain.TemperatureEvent{sample(0, 5.0), other}, profile())
	if err == nil {
		t.Fatal("同一批采样混点位时期望报错")
	}
	if result.Run.State != domain.RunStateNeedsManual {
		t.Errorf("run 状态 = %s，期望 %s", result.Run.State, domain.RunStateNeedsManual)
	}
}

func TestAssess_EmptySamplesRejected(t *testing.T) {
	if _, err := Assess(nil, profile()); err == nil {
		t.Fatal("空采样时期望报错")
	}
}

// gatedProfile 带判定门槛的点位配置：5 分钟一采、连续超限 10 分钟才算越温、迟滞 1℃。
func gatedProfile() *domain.PointProfile {
	p := profile()
	p.SampleIntervalSec = 300
	p.MinExcursionMinutes = 10
	p.HysteresisC = 1
	return p
}

// TestAssess_TransientExcursionsListedSeparately 未达门槛的越限不进窗口，
// 但必须出现在结论的 transient_excursions 段里：
// 「没成立窗口」与「压根没越限」对药师是两回事。
func TestAssess_TransientExcursionsListedSeparately(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sample(0, 5.0),
		sample(5, 8.6),
		sample(10, 9.1),
		sample(15, 5.2),
	}

	result, err := Assess(samples, gatedProfile())
	if err != nil {
		t.Fatalf("期望判定成功，实际报错：%v", err)
	}
	if result.Window != nil {
		t.Fatal("短时波动不该生成越温窗口")
	}
	if len(result.Windows) != 0 {
		t.Errorf("正式窗口数 = %d，期望 0", len(result.Windows))
	}
	if len(result.TransientExcursions) != 1 {
		t.Fatalf("短时波动段数 = %d，期望 1", len(result.TransientExcursions))
	}
	if got, want := result.TransientExcursions[0].DurationMinutes, 5; got != want {
		t.Errorf("短时波动时长 = %d 分钟，期望 %d 分钟", got, want)
	}
	if result.Excursions == nil {
		t.Fatal("越限汇总不应为空")
	}
	if got, want := result.Excursions.TransientSegmentCount, 1; got != want {
		t.Errorf("汇总里的短时波动段数 = %d，期望 %d", got, want)
	}
	if got, want := result.Excursions.WindowCount, 0; got != want {
		t.Errorf("汇总里的正式窗口数 = %d，期望 %d", got, want)
	}
}

// TestAssess_AdjacentExcursionsMergedIntoSingleWindow 相邻两段越限合成一条窗口：
// 两段各自够门槛，中间只回落了一个采样周期，说明设备没真正恢复。
func TestAssess_AdjacentExcursionsMergedIntoSingleWindow(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sample(0, 5.0),
		sample(5, 8.6),
		sample(10, 9.5),
		sample(15, 10.0),
		sample(20, 6.0),
		sample(25, 9.0),
		sample(30, 9.5),
		sample(35, 10.5),
		sample(40, 5.0),
	}

	result, err := Assess(samples, gatedProfile())
	if err != nil {
		t.Fatalf("期望判定成功，实际报错：%v", err)
	}
	if len(result.Windows) != 1 {
		t.Fatalf("合并后正式窗口数 = %d，期望 1", len(result.Windows))
	}
	if result.Window == nil {
		t.Fatal("合并后的窗口不应为空")
	}
	if got, want := result.Window.SegmentCount, 2; got != want {
		t.Errorf("合并段数 = %d，期望 %d", got, want)
	}
	if got, want := result.Window.RecoveryMinutes, 5; got != want {
		t.Errorf("回落分钟 = %d，期望 %d", got, want)
	}
	if got, want := result.Window.DurationMinutes, 20; got != want {
		t.Errorf("窗口持续 = %d 分钟，期望 %d 分钟", got, want)
	}
	if got, want := result.Window.PeakC, 10.5; got != want {
		t.Errorf("合并后峰值 = %.2f℃，期望 %.2f℃", got, want)
	}
	if got, want := result.Window.PeakAt, sample(35, 0).MeasuredAt; !got.Equal(want) {
		t.Errorf("合并后峰值时刻 = %s，期望 %s", got, want)
	}
}

// TestAssess_FeaturesFromRealSampleFile 真实脱敏样例上的归因特征：
// 用仓库里的记录仪导出样例跑一遍判定，特征面必须把三段异常各自的
// 特征分开：开门那次湿度跟着涨、断电那次湿度没动、除霜那次湿度也涨。
//
// 这条用例守的是「特征确实算在真实数据形态上」——
// 只在人造样例里对的算法，换成真实导出文件就会露馅。
func TestAssess_FeaturesFromRealSampleFile(t *testing.T) {
	samples, err := fixture.LoadRecordings("../../testdata/recordings_sample.csv")
	if err != nil {
		t.Fatalf("读取脱敏样例失败：%v", err)
	}
	if len(samples) == 0 {
		t.Fatal("脱敏样例为空")
	}

	prof := &domain.PointProfile{
		PointID:             "point-vax-freezer-01",
		ZoneMinC:            2,
		ZoneMaxC:            8,
		SampleIntervalSec:   300,
		MinExcursionMinutes: 10,
		HysteresisC:         1,
		AccuracyLimitC:      0.5,
	}

	result, err := Assess(samples, prof)
	if err != nil {
		t.Fatalf("期望判定成功，实际报错：%v", err)
	}
	if result.Features == nil {
		t.Fatalf("期望给出归因特征，实际为 nil（原因：%s）", result.FeaturesReason)
	}
	f := result.Features

	// 第一条窗口是 09:00 那次开门造成的越温（峰值 9.6℃、持续 15 分钟）。
	if result.Window == nil {
		t.Fatal("期望抽出越温窗口，实际为 nil")
	}
	if got, want := result.Window.PeakC, 9.6; got != want {
		t.Errorf("窗口峰值 = %.2f℃，期望 %.2f℃", got, want)
	}

	// 这台设备上报过事件（开门、断电、除霜都在）。
	if !f.DeviceEventsReported {
		t.Error("样例里设备上报过事件，DeviceEventsReported 应为真")
	}

	// 开门事件必须落在这次异常的观测范围内：4 条开门采样 = 20 分钟。
	if f.DoorOpenMinutes == nil {
		t.Fatal("开门时长不该缺测")
	}
	if got, want := *f.DoorOpenMinutes, 20; got != want {
		t.Errorf("开门累计时长 = %d 分钟，期望 %d 分钟", got, want)
	}
	if got, want := *f.DoorOpenCount, 4; got != want {
		t.Errorf("开门次数 = %d，期望 %d", got, want)
	}
	// 开门那一刻温度就已在上升，滞后为 0。
	if f.DoorToRiseDelayMinutes == nil || *f.DoorToRiseDelayMinutes != 0 {
		t.Errorf("开门到温升的滞后 = %v，期望 0 分钟", f.DoorToRiseDelayMinutes)
	}

	// 断电与除霜属后面那两次异常，不该算进这一次的观测范围。
	if f.PowerLossObserved == nil || *f.PowerLossObserved {
		t.Errorf("断电事件存在性 = %v，期望为假（断电属下一次异常）", f.PowerLossObserved)
	}
	if f.DefrostObserved == nil || *f.DefrostObserved {
		t.Errorf("除霜事件存在性 = %v，期望为假（除霜属再下一次异常）", f.DefrostObserved)
	}

	// 开门那次湿空气跟着进来了：基线 58、峰值 62，上升 4 个百分点。
	if f.HumidityRisePct == nil {
		t.Fatal("湿度上升幅度不该缺测")
	}
	if got, want := *f.HumidityRisePct, 4.0; got < want-0.001 || got > want+0.001 {
		t.Errorf("湿度上升幅度 = %.2f 个百分点，期望 %.2f", got, want)
	}

	// 温升速率、峰值偏离度都得有。
	if f.RiseRatePerHour == nil {
		t.Fatal("温升速率不该缺测")
	}
	if *f.RiseRatePerHour <= 0 {
		t.Errorf("开门后的温升速率 = %.4f℃/小时，期望为正", *f.RiseRatePerHour)
	}
	if f.PeakDeviationC == nil {
		t.Fatal("峰值偏离度不该缺测")
	}
	if got, want := *f.PeakDeviationC, 1.6; got < want-0.001 || got > want+0.001 {
		t.Errorf("峰值偏离度 = %.2f℃，期望 %.2f℃", got, want)
	}
}
