package rules

import (
	"math"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
)

// 本文件覆盖越温窗口抽取的边界与异常用例：
// 数据缺口、时钟回拨、探头卡死、边界采样归属。
//
// 这四类不是「极端情况」，而是冷链现场最常见的几种脏数据形态。
// 每一类如果不单独认出来，都会被算成一段看起来完全正常的越温——
// 而这种越温会一路走到《温度异常情况记录表》上，等着人工去追一个不存在的异常。

// sampleAtRaw 造一条采样：时刻用 offset 分钟表示，可为负（用于构造时钟回拨）。
func sampleAtRaw(offsetMin int, temperatureC float64) domain.TemperatureEvent {
	base := time.Date(2026, 10, 20, 3, 0, 0, 0, time.FixedZone("CST", 8*3600))
	return domain.TemperatureEvent{
		PointID:      "point-vax-freezer-01",
		MeasuredAt:   base.Add(time.Duration(offsetMin) * time.Minute),
		TemperatureC: temperatureC,
	}
}

// TestInferSamplingInterval_UsesMedian 由相邻间隔的中位数推断采样周期：
// 一次长缺口不能把周期推断带偏。
func TestInferSamplingInterval_UsesMedian(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAtRaw(0, 5.0),
		sampleAtRaw(5, 5.1),
		sampleAtRaw(10, 5.2),
		sampleAtRaw(15, 5.3),
		sampleAtRaw(75, 5.4), // 中间断了 60 分钟
		sampleAtRaw(80, 5.5),
		sampleAtRaw(85, 5.6),
	}
	// 间隔：5 / 5 / 5 / 60 / 5 / 5，中位数仍是 5 分钟。
	if got, want := inferSamplingInterval(samples), 5*time.Minute; got != want {
		t.Errorf("推断出的采样周期 = %s，期望 %s（取中位数，不被一次缺口带偏）", got, want)
	}
}

// TestExtractWindow_DataGapMarkedAndNotConnected 缺口要被标出来，且不跨缺口连线：
// 缺的那段时间没有读数，不能算进越温时长，更不能拿两头的读数接成连续温升。
func TestExtractWindow_DataGapMarkedAndNotConnected(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAtRaw(0, 5.0),
		sampleAtRaw(5, 8.6),  // 越限
		sampleAtRaw(10, 9.1), // 越限
		// 断线 40 分钟（超过 3 个采样周期）
		sampleAtRaw(50, 8.8), // 越限
		sampleAtRaw(55, 5.0), // 回到温区
	}

	window, err := ExtractWindow(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望抽出越温窗口，实际报错：%v", err)
	}
	if window == nil {
		t.Fatal("期望抽出越温窗口，实际为 nil")
	}

	if got, want := window.BreakpointCount, 1; got != want {
		t.Fatalf("断点数 = %d，期望 %d（缺口必须被标出来）", got, want)
	}
	if len(window.DataGaps) != 1 {
		t.Fatalf("缺口条数 = %d，期望 1", len(window.DataGaps))
	}
	gap := window.DataGaps[0]
	if !gap.From.Equal(sampleAtRaw(10, 0).MeasuredAt) {
		t.Errorf("缺口起点 = %s，期望 %s", gap.From, sampleAtRaw(10, 0).MeasuredAt)
	}
	if !gap.To.Equal(sampleAtRaw(50, 0).MeasuredAt) {
		t.Errorf("缺口终点 = %s，期望 %s", gap.To, sampleAtRaw(50, 0).MeasuredAt)
	}
	if got, want := gap.GapMinutes, 40; got != want {
		t.Errorf("缺口时长 = %d 分钟，期望 %d 分钟", got, want)
	}
	if got, want := gap.MissingSamples, 7; got != want {
		t.Errorf("估算缺失采样条数 = %d，期望 %d（40 分钟 ÷ 5 分钟 − 1）", got, want)
	}

	// 确认时长按采样点数折算：(3-1)×5 = 10 分钟。
	// 缺口那 40 分钟不计入（按墙钟折算会把断线算成越温），
	// 但缺口本身不会被抹掉——它在窗口上单独标成断点。
	if got, want := window.DurationMinutes, 10; got != want {
		t.Errorf("持续分钟 = %d，期望 %d（缺口那段时间不计入越温时长）", got, want)
	}
	if got, want := window.SampleCount, 3; got != want {
		t.Errorf("采样点数 = %d，期望 %d", got, want)
	}
	// 窗口起止仍落在真实越限采样点上。
	if !window.Start.Equal(sampleAtRaw(5, 0).MeasuredAt) {
		t.Errorf("窗口起点 = %s，期望 %s", window.Start, sampleAtRaw(5, 0).MeasuredAt)
	}
	if !window.End.Equal(sampleAtRaw(50, 0).MeasuredAt) {
		t.Errorf("窗口终点 = %s，期望 %s", window.End, sampleAtRaw(50, 0).MeasuredAt)
	}
}

// TestExtractWindow_GapBelowThreshold_NoBreakpoint 缺口判定有门槛：
// 丢一两个点属网关抖动，不算缺口，也不打断观测段。
func TestExtractWindow_GapBelowThreshold_NoBreakpoint(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAtRaw(0, 5.0),
		sampleAtRaw(5, 8.6),
		sampleAtRaw(20, 8.8), // 间隔 15 分钟 = 3 个周期，未超过 3 倍
		sampleAtRaw(25, 8.9),
		sampleAtRaw(30, 5.0),
	}

	window, err := ExtractWindow(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望抽出越温窗口，实际报错：%v", err)
	}
	if window == nil {
		t.Fatal("期望抽出越温窗口，实际为 nil")
	}
	if got, want := window.BreakpointCount, 0; got != want {
		t.Errorf("断点数 = %d，期望 %d（15 分钟未超过 3 个周期，不算缺口）", got, want)
	}
	if got, want := window.DurationMinutes, 10; got != want {
		t.Errorf("持续分钟 = %d，期望 %d（三条越限读数按采样点数折算）", got, want)
	}
}

// TestExtractWindow_ClockRegressionKeepsRecorderOrder 时钟回拨：按记录仪序重排，
// 不按时刻排序。排序会把回拨前后的读数搅在一起——先发生的高温被排到
// 后发生的低温后面，凭空造出一段不存在的升降温。
func TestExtractWindow_ClockRegressionKeepsRecorderOrder(t *testing.T) {
	// 记录仪序：10:00 → 10:05 → 10:10，然后时钟回到 09:55（换电池），
	// 继续报 09:55 / 10:00 / 10:05。排序后 10:00 与 10:05 各出现两次，
	// 时间轴自身重叠，回拨被确认。
	samples := []domain.TemperatureEvent{
		sampleAtRaw(60, 5.0),  // 10:00
		sampleAtRaw(65, 8.6),  // 10:05 越限
		sampleAtRaw(70, 12.4), // 10:10 越限（峰值）
		sampleAtRaw(55, 8.5),  // 09:55 时钟回拨（越限）
		sampleAtRaw(60, 9.0),  // 10:00 越限
		sampleAtRaw(65, 7.0),  // 10:05 回到温区且低于迟滞带，异常结束
	}

	window, err := ExtractWindow(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望抽出越温窗口，实际报错：%v", err)
	}
	if window == nil {
		t.Fatal("期望抽出越温窗口，实际为 nil")
	}

	if len(window.ClockRegressions) != 1 {
		t.Fatalf("时钟回拨条数 = %d，期望 1", len(window.ClockRegressions))
	}
	reg := window.ClockRegressions[0]
	if got, want := reg.BackwardMinutes, 15; got != want {
		t.Errorf("回拨分钟 = %d，期望 %d", got, want)
	}
	if !reg.At.Equal(sampleAtRaw(55, 0).MeasuredAt) {
		t.Errorf("回拨时刻 = %s，期望 %s", reg.At, sampleAtRaw(55, 0).MeasuredAt)
	}
	if !reg.PreviousAt.Equal(sampleAtRaw(70, 0).MeasuredAt) {
		t.Errorf("回拨前时刻 = %s，期望 %s", reg.PreviousAt, sampleAtRaw(70, 0).MeasuredAt)
	}

	// 按记录仪序：起点是 10:05 那条越限读数，不是排序后的 09:55。
	if !window.Start.Equal(sampleAtRaw(65, 0).MeasuredAt) {
		t.Errorf("窗口起点 = %s，期望 %s（按记录仪序，不按排序后的时刻）",
			window.Start, sampleAtRaw(65, 0).MeasuredAt)
	}
	// 峰值仍是 10:10 的 12.4℃。
	if got, want := window.PeakC, 12.4; got != want {
		t.Errorf("峰值 = %.2f℃，期望 %.2f℃", got, want)
	}
	// 回拨区间的时长按采样点数折算，不按墙钟（墙钟会算出负数）。
	if got, want := window.DurationMinutes, 15; got != want {
		t.Errorf("持续分钟 = %d，期望 %d（回拨区间按采样点数折算）", got, want)
	}
	if window.End.Before(window.Start) {
		t.Errorf("窗口终点 %s 早于起点 %s，窗口区间被倒过来了", window.End, window.Start)
	}
}

// TestExtractWindow_ShuffledArrivalIsNotClockRegression 导出文件顺序乱不是时钟回拨：
// 时间轴自身不重叠就按时刻排序归位，不报数据事故。
func TestExtractWindow_ShuffledArrivalIsNotClockRegression(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAtRaw(20, 8.7),
		sampleAtRaw(0, 8.6),
		sampleAtRaw(10, 12.4),
		sampleAtRaw(5, 9.4),
		sampleAtRaw(15, 10.1),
	}

	window, err := ExtractWindow(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望抽出越温窗口，实际报错：%v", err)
	}
	if window == nil {
		t.Fatal("期望抽出越温窗口，实际为 nil")
	}
	if len(window.ClockRegressions) != 0 {
		t.Errorf("时钟回拨条数 = %d，期望 0（文件乱序不是时钟回拨）", len(window.ClockRegressions))
	}
	if !window.Start.Equal(sampleAtRaw(0, 0).MeasuredAt) {
		t.Errorf("窗口起点 = %s，期望 %s（乱序文件按时刻排序归位）",
			window.Start, sampleAtRaw(0, 0).MeasuredAt)
	}
}

// TestExtractWindow_ProbeStuckFlaggedAndExcludedFromRiseRate 探头卡死要被标出来，
// 且这一段读数不进温升速率：读数不动不等于温度不动。
func TestExtractWindow_ProbeStuckFlaggedAndExcludedFromRiseRate(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAtRaw(0, 5.0),
		sampleAtRaw(5, 9.0),   // 越限
		sampleAtRaw(10, 10.0), // 越限，探头从此不动
		sampleAtRaw(15, 10.0), // 卡死
		sampleAtRaw(20, 10.0), // 卡死
		sampleAtRaw(25, 10.0), // 卡死
		sampleAtRaw(30, 5.0),  // 回到温区
	}

	window, err := ExtractWindow(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望抽出越温窗口，实际报错：%v", err)
	}
	if window == nil {
		t.Fatal("期望抽出越温窗口，实际为 nil")
	}

	if len(window.ProbeStuckSpans) != 1 {
		t.Fatalf("探头卡死段数 = %d，期望 1", len(window.ProbeStuckSpans))
	}
	span := window.ProbeStuckSpans[0]
	if got, want := span.SampleCount, 4; got != want {
		t.Errorf("卡死段采样条数 = %d，期望 %d", got, want)
	}
	if got, want := span.Value, 10.0; got != want {
		t.Errorf("卡死段恒定读数 = %.2f℃，期望 %.2f℃", got, want)
	}
	if !span.From.Equal(sampleAtRaw(10, 0).MeasuredAt) {
		t.Errorf("卡死段起点 = %s，期望 %s", span.From, sampleAtRaw(10, 0).MeasuredAt)
	}

	// 温升速率只用有效读数：5.0 → 9.0，5 分钟，0.8℃/分钟。
	// 若把卡死段算进来，会得到 (10.0-5.0)/10 = 0.5℃/分钟——那是探头在动，不是温度在动。
	if window.RiseRate == nil {
		t.Fatal("窗口缺温升速率")
	}
	rate := window.RiseRate
	if !rate.Usable {
		t.Fatalf("温升速率应可用，实际不可用：%s", rate.Note)
	}
	if got, want := rate.ExcludedStuckSamples, 4; got != want {
		t.Errorf("因卡死剔除的采样条数 = %d，期望 %d", got, want)
	}
	if got, want := rate.PeakC, 9.0; got != want {
		t.Errorf("参与计算的峰值 = %.2f℃，期望 %.2f℃（卡死读数不算温升）", got, want)
	}
	if got, want := rate.PerMinute, 0.8; math.Abs(got-want) > 1e-9 {
		t.Errorf("温升速率 = %.4f℃/分钟，期望 %.4f℃/分钟", got, want)
	}
	if got, want := rate.MinutesToPeak, 5; got != want {
		t.Errorf("起点到峰值的分钟 = %d，期望 %d", got, want)
	}
}

// TestDetectProbeStuck_ConstantAtZoneMidpointNotFlagged 读数恒定在温区中值时
// 不标卡死：「探头不动」与「冰箱本来就稳」分不开，标了会误伤。
func TestDetectProbeStuck_ConstantAtZoneMidpointNotFlagged(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAtRaw(0, 5.0),
		sampleAtRaw(5, 5.0),
		sampleAtRaw(10, 5.0),
		sampleAtRaw(15, 5.0),
	}

	if spans := detectProbeStuck(samples, hystZone(10), 5*time.Minute); len(spans) != 0 {
		t.Errorf("温区中值上的恒定读数被误判为卡死：%+v", spans)
	}
}

// TestExtractWindow_BoundarySampleAssignedOnce 边界采样点归属明确且可复现：
// 窗口覆盖范围按闭区间算，窗口终点那条读数计入窗口，且只计入一次。
func TestExtractWindow_BoundarySampleAssignedOnce(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAtRaw(0, 8.6),
		sampleAtRaw(5, 9.0),
		sampleAtRaw(10, 5.0), // 恰好回到温区内，结束第一段
		sampleAtRaw(15, 9.0),
		sampleAtRaw(20, 9.5),
		sampleAtRaw(25, 9.2),
		sampleAtRaw(30, 5.0),
	}

	window, err := ExtractWindow(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望抽出越温窗口，实际报错：%v", err)
	}
	if window == nil {
		t.Fatal("期望抽出越温窗口，实际为 nil")
	}
	// 第一段只有两条越限读数：确认时长 (2-1)×5 = 5 分钟，未达门槛 10 分钟，
	// 因此窗口落在第二段。第二段三条越限读数：10 分钟，恰好达标。
	if !window.Start.Equal(sampleAtRaw(15, 0).MeasuredAt) {
		t.Errorf("窗口起点 = %s，期望 %s", window.Start, sampleAtRaw(15, 0).MeasuredAt)
	}
	if !window.End.Equal(sampleAtRaw(25, 0).MeasuredAt) {
		t.Errorf("窗口终点 = %s，期望 %s", window.End, sampleAtRaw(25, 0).MeasuredAt)
	}
	if got, want := window.SampleCount, 3; got != want {
		t.Errorf("采样点数 = %d，期望 %d", got, want)
	}
	if got, want := window.OutOfRangeSamples, 3; got != want {
		t.Errorf("越限读数条数 = %d，期望 %d（边界读数只被算一次）", got, want)
	}

	// 窗口覆盖范围按闭区间：起止两条采样点都在窗口内。
	if !window.Contains(window.Start) {
		t.Error("窗口起点应落在窗口覆盖范围内")
	}
	if !window.Contains(window.End) {
		t.Error("窗口终点应落在窗口覆盖范围内")
	}
	if window.Contains(window.End.Add(time.Minute)) {
		t.Error("窗口终点之后一分钟不应落在窗口覆盖范围内")
	}
}

// TestComputeRiseRate_ExcludesStuckSamples 温升速率不把探头卡死段算进来：
// 卡死段不是温度在动，是探头不动了。
func TestComputeRiseRate_ExcludesStuckSamples(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAtRaw(0, 5.0),
		sampleAtRaw(5, 8.0),
		sampleAtRaw(10, 12.0),
		sampleAtRaw(15, 12.0), // 卡死
		sampleAtRaw(20, 12.0), // 卡死
		sampleAtRaw(25, 12.0), // 卡死
	}

	rate, err := ComputeRiseRate(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望算出温升速率，实际报错：%v", err)
	}
	if !rate.Usable {
		t.Fatalf("温升速率应可用，实际不可用：%s", rate.Note)
	}
	if got, want := rate.ExcludedStuckSamples, 4; got != want {
		t.Errorf("因卡死剔除的采样条数 = %d，期望 %d", got, want)
	}
	// 有效读数只有 5.0 → 8.0：速率 = (8.0-5.0)/5 = 0.6℃/分钟。
	// 若把卡死段算进来，会得到 (12.0-5.0)/10 = 0.7℃/分钟——那是探头在动。
	if got, want := rate.PerMinute, 0.6; math.Abs(got-want) > 1e-9 {
		t.Errorf("温升速率 = %.4f℃/分钟，期望 %.4f℃/分钟", got, want)
	}
	if got, want := rate.PeakC, 8.0; got != want {
		t.Errorf("参与计算的峰值 = %.2f℃，期望 %.2f℃（卡死读数不算温升）", got, want)
	}
}

// TestComputeRiseRate_DoesNotSpanDataGap 温升速率不跨数据缺口连线：
// 缺了那段时间就没有「连续观测」，跨过去算出来的斜率是假的。
func TestComputeRiseRate_DoesNotSpanDataGap(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAtRaw(0, 5.0),
		sampleAtRaw(5, 8.0),
		// 断线 55 分钟
		sampleAtRaw(60, 12.0),
		sampleAtRaw(65, 13.0),
	}

	rate, err := ComputeRiseRate(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望算出温升速率，实际报错：%v", err)
	}
	if !rate.Usable {
		t.Fatalf("温升速率应可用，实际不可用：%s", rate.Note)
	}
	// 峰值 13.0 所在的连续观测段是 [12.0@60, 13.0@65]，
	// 速率 = (13.0-12.0)/5 = 0.2℃/分钟。
	// 若跨缺口连线，会得到 (13.0-5.0)/65 = 0.12℃/分钟——那是把断线算成了缓升。
	if got, want := rate.PerMinute, 0.2; math.Abs(got-want) > 1e-9 {
		t.Errorf("温升速率 = %.4f℃/分钟，期望 %.4f℃/分钟", got, want)
	}
	if rate.ExcludedGapMinutes <= 0 {
		t.Errorf("因缺口未计入的分钟 = %d，期望大于 0", rate.ExcludedGapMinutes)
	}
}

// TestSummarizeExcursions_ReportsDataQualityFacts 汇总里要带上这批读数的
// 数据质量事实：缺口数、回拨数、卡死条数、折算口径来源。
func TestSummarizeExcursions_ReportsDataQualityFacts(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAtRaw(0, 5.0),
		sampleAtRaw(5, 8.6),
		sampleAtRaw(10, 9.1),
		sampleAtRaw(60, 8.8), // 缺口
		sampleAtRaw(65, 10.0),
		sampleAtRaw(70, 10.0),
		sampleAtRaw(75, 10.0), // 卡死
		sampleAtRaw(80, 5.0),
	}

	summary, err := SummarizeExcursions(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望汇总成功，实际报错：%v", err)
	}
	if got, want := summary.IntervalSource, IntervalSourcePointProfile; got != want {
		t.Errorf("采样周期来源 = %s，期望 %s", got, want)
	}
	if got, want := summary.DataGapCount, 1; got != want {
		t.Errorf("数据缺口数 = %d，期望 %d", got, want)
	}
	if got, want := summary.ProbeStuckSampleCount, 3; got != want {
		t.Errorf("卡死段采样条数 = %d，期望 %d", got, want)
	}
}
