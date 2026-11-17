package rules

import (
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// sampleAt 造一条采样：时刻固定用 2026-10-20 03:00:00 +08:00 起算，offset 分钟。
func sampleAt(offsetMin int, temperatureC float64) domain.TemperatureEvent {
	base := time.Date(2026, 10, 20, 3, 0, 0, 0, time.FixedZone("CST", 8*3600))
	return domain.TemperatureEvent{
		PointID:      "point-vax-freezer-01",
		MeasuredAt:   base.Add(time.Duration(offsetMin) * time.Minute),
		TemperatureC: temperatureC,
	}
}

// testZone 疫苗冷藏温区 2~8℃，不带判定门槛（门槛为 0 即不设限）。
func testZone() ZoneDecision {
	return ZoneDecision{
		ZoneMinC:   2,
		ZoneMaxC:   8,
		ZoneLabel:  ZoneLabel(2, 8),
		ZoneSource: ZoneSourcePointProfile,
	}
}

// hystZone 疫苗冷藏温区 2~8℃，采样周期 5 分钟、最短越限 10 分钟、迟滞带 1℃。
//
// 这组参数对应现场常见配置：记录仪 5 分钟记一次，连续超限 10 分钟才算越温，
// 迟滞 1℃ 用来吃掉空调启停造成的贴边抖动。
func hystZone(minExcursionMinutes int) ZoneDecision {
	zone := testZone()
	zone.SampleIntervalSec = 300
	zone.MinExcursionMinutes = minExcursionMinutes
	zone.HysteresisC = 1
	return zone
}

// TestExtractWindow_NormalExcursion 正常越限：连续超限达门槛即成立窗口，
// 窗口起止落在真实越限采样点上，峰值取偏离温区最远的读数。
func TestExtractWindow_NormalExcursion(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAt(0, 5.0),
		sampleAt(5, 5.4),
		sampleAt(10, 8.6), // 越限
		sampleAt(15, 9.1), // 越限
		sampleAt(20, 8.4), // 越限
		sampleAt(25, 5.2), // 回落，回到温区
	}

	window, err := ExtractWindow(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望抽出越温窗口，实际报错：%v", err)
	}
	if window == nil {
		t.Fatal("期望抽出越温窗口，实际为 nil")
	}
	if got, want := window.Start, sampleAt(10, 0).MeasuredAt; !got.Equal(want) {
		t.Errorf("窗口起点 = %s，期望 %s", got, want)
	}
	if got, want := window.End, sampleAt(20, 0).MeasuredAt; !got.Equal(want) {
		t.Errorf("窗口终点 = %s，期望 %s", got, want)
	}
	if got, want := window.PeakC, 9.1; got != want {
		t.Errorf("峰值 = %.2f℃，期望 %.2f℃", got, want)
	}
	if got, want := window.PeakAt, sampleAt(15, 0).MeasuredAt; !got.Equal(want) {
		t.Errorf("峰值时刻 = %s，期望 %s", got, want)
	}
	if got, want := window.DurationMinutes, 10; got != want {
		t.Errorf("持续分钟 = %d，期望 %d", got, want)
	}
	if got, want := window.SampleCount, 3; got != want {
		t.Errorf("采样点数 = %d，期望 %d", got, want)
	}
	if got, want := window.OutOfRangeSamples, 3; got != want {
		t.Errorf("越限读数条数 = %d，期望 %d", got, want)
	}
	if got, want := window.MergeCount, 0; got != want {
		t.Errorf("合并次数 = %d，期望 %d（只有一段越限，不该发生合并）", got, want)
	}
	if got, want := window.HysteresisC, 1.0; got != want {
		t.Errorf("迟滞带 = %.2f℃，期望 %.2f℃", got, want)
	}
	if got, want := window.MinExcursionMinutes, 10; got != want {
		t.Errorf("最短越限门槛 = %d 分钟，期望 %d 分钟", got, want)
	}
	if got, want := window.Zone, "2~8℃"; got != want {
		t.Errorf("温区标签 = %s，期望 %s", got, want)
	}
}

// TestExtractWindow_ExactlyAtMinExcursionMinutes 边界：确认时长恰好等于门槛时成立窗口，
// 差一分钟就不成立。门槛取「大于等于」而不是「大于」——
// 门槛本身就是现场约定的口径，不是数学上的开区间。
func TestExtractWindow_ExactlyAtMinExcursionMinutes(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAt(0, 5.0),
		sampleAt(5, 8.6),
		sampleAt(10, 9.1),
		sampleAt(15, 8.4),
		sampleAt(20, 5.2),
	}
	// 三条越限采样、5 分钟一个周期：确认时长 = (3-1)×5 = 10 分钟。
	window, err := ExtractWindow(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望判定成功，实际报错：%v", err)
	}
	if window == nil {
		t.Fatal("确认时长恰好等于门槛时期望成立窗口，实际为 nil")
	}
	if got, want := window.DurationMinutes, 10; got != want {
		t.Errorf("持续分钟 = %d，期望 %d", got, want)
	}

	window, err = ExtractWindow(samples, hystZone(11))
	if err != nil {
		t.Fatalf("未达门槛不是失败，实际报错：%v", err)
	}
	if window != nil {
		t.Fatalf("确认时长 10 分钟未达门槛 11 分钟，期望不成立窗口，实际得到 %+v", window)
	}
}

// TestExtractWindow_BelowMinExcursionMinutes 未达最短越限时长不成立窗口：
// 一次读数超限是波动，不是越温；不成立窗口不是失败，不报错。
func TestExtractWindow_BelowMinExcursionMinutes(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAt(0, 5.0),
		sampleAt(5, 8.6), // 只超了一条
		sampleAt(10, 5.2),
	}

	window, err := ExtractWindow(samples, hystZone(10))
	if err != nil {
		t.Fatalf("未达门槛不是失败，实际报错：%v", err)
	}
	if window != nil {
		t.Fatalf("未达门槛时期望不成立窗口，实际得到 %+v", window)
	}
}

// TestExtractWindow_HysteresisHoldsShortDip 迟滞消抖：读数落回迟滞带（8℃ 与 7℃ 之间）
// 但没跌出越限状态时，本次异常不结束，随后再次冲出去仍算同一段。
//
// 没有迟滞带的话，这次回落会把一次真实断电切成两段 5 分钟的短窗口，
// 两段都因为「不够长」判不出来——一次断电反倒什么结论都没有。
func TestExtractWindow_HysteresisHoldsShortDip(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAt(0, 5.0),
		sampleAt(5, 8.6),  // 越限
		sampleAt(10, 8.2), // 越限
		sampleAt(15, 7.8), // 落回迟滞带（> 8-1=7℃），异常未结束
		sampleAt(20, 9.0), // 又冲出去
		sampleAt(25, 5.0), // 回到温区，异常结束
	}

	window, err := ExtractWindow(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望抽出越温窗口，实际报错：%v", err)
	}
	if window == nil {
		t.Fatal("期望抽出越温窗口，实际为 nil")
	}
	if got, want := window.Start, sampleAt(5, 0).MeasuredAt; !got.Equal(want) {
		t.Errorf("窗口起点 = %s，期望 %s", got, want)
	}
	if got, want := window.End, sampleAt(20, 0).MeasuredAt; !got.Equal(want) {
		t.Errorf("窗口终点 = %s，期望 %s（迟滞带里的回落不该提前结束窗口）", got, want)
	}
	if got, want := window.SampleCount, 4; got != want {
		t.Errorf("采样点数 = %d，期望 %d", got, want)
	}
	if got, want := window.OutOfRangeSamples, 3; got != want {
		t.Errorf("越限读数条数 = %d，期望 %d（迟滞带里的回落不算越限）", got, want)
	}
	if got, want := window.MergeCount, 1; got != want {
		t.Errorf("合并次数 = %d，期望 %d（两段越限被合并成一段）", got, want)
	}
	if got, want := window.PeakC, 9.0; got != want {
		t.Errorf("峰值 = %.2f℃，期望 %.2f℃", got, want)
	}
	if got, want := window.DurationMinutes, 15; got != want {
		t.Errorf("持续分钟 = %d，期望 %d", got, want)
	}
}

// TestExtractWindow_MultipleExcursionsMerged 多次短暂越限合并：两次落回迟滞带、
// 三段越限，最后合成一条窗口，合并次数记 2。
func TestExtractWindow_MultipleExcursionsMerged(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAt(0, 8.6),  // 越限
		sampleAt(5, 7.8),  // 落回迟滞带
		sampleAt(10, 8.4), // 越限
		sampleAt(15, 7.9), // 落回迟滞带
		sampleAt(20, 9.1), // 越限
		sampleAt(25, 5.0), // 回到温区
	}

	window, err := ExtractWindow(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望抽出越温窗口，实际报错：%v", err)
	}
	if window == nil {
		t.Fatal("期望抽出越温窗口，实际为 nil")
	}
	if got, want := window.MergeCount, 2; got != want {
		t.Errorf("合并次数 = %d，期望 %d（三段越限合并成一段）", got, want)
	}
	if got, want := window.OutOfRangeSamples, 3; got != want {
		t.Errorf("越限读数条数 = %d，期望 %d", got, want)
	}
	if got, want := window.SampleCount, 5; got != want {
		t.Errorf("采样点数 = %d，期望 %d", got, want)
	}
	if got, want := window.Start, sampleAt(0, 0).MeasuredAt; !got.Equal(want) {
		t.Errorf("窗口起点 = %s，期望 %s", got, want)
	}
	if got, want := window.End, sampleAt(20, 0).MeasuredAt; !got.Equal(want) {
		t.Errorf("窗口终点 = %s，期望 %s", got, want)
	}
	if got, want := window.PeakC, 9.1; got != want {
		t.Errorf("峰值 = %.2f℃，期望 %.2f℃", got, want)
	}
}

// TestScanSegments_SplitWhenBackInSafeBand 回到迟滞带以内的温区（不是迟滞带本身）
// 才真正结束一次异常：此时的回落会切出独立的一段，两段各自成立窗口。
func TestScanSegments_SplitWhenBackInSafeBand(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAt(0, 8.6),
		sampleAt(5, 8.4),
		sampleAt(10, 8.3),
		sampleAt(15, 6.0), // 回到温区且低于迟滞带 → 第一段结束
		sampleAt(20, 9.0),
		sampleAt(25, 9.5),
		sampleAt(30, 9.2),
		sampleAt(35, 5.0),
	}

	segments := scanSegments(samples, hystZone(10))
	if got, want := len(segments), 2; got != want {
		t.Fatalf("扫出的越限段数 = %d，期望 %d", got, want)
	}
	if got, want := segments[0].firstIdx, 0; got != want {
		t.Errorf("第一段起点下标 = %d，期望 %d", got, want)
	}
	if got, want := segments[0].lastIdx, 2; got != want {
		t.Errorf("第一段终点下标 = %d，期望 %d", got, want)
	}
	if got, want := segments[0].segments, 1; got != want {
		t.Errorf("第一段合并前段数 = %d，期望 %d", got, want)
	}
	if got, want := segments[1].firstIdx, 4; got != want {
		t.Errorf("第二段起点下标 = %d，期望 %d", got, want)
	}
	if got, want := segments[1].lastIdx, 6; got != want {
		t.Errorf("第二段终点下标 = %d，期望 %d", got, want)
	}
}

// TestExtractWindow_SortsByMeasuredAt 采样乱序到达：窗口必须按采样时刻排序后再抽取，
// 而不是按文件顺序。
func TestExtractWindow_SortsByMeasuredAt(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAt(20, 8.7),
		sampleAt(0, 8.6),
		sampleAt(10, 12.4),
		sampleAt(5, 9.4),
		sampleAt(15, 10.1),
	}

	window, err := ExtractWindow(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望抽出越温窗口，实际报错：%v", err)
	}
	if window == nil {
		t.Fatal("期望抽出越温窗口，实际为 nil")
	}
	if got, want := window.Start, sampleAt(0, 0).MeasuredAt; !got.Equal(want) {
		t.Errorf("窗口起点 = %s，期望 %s", got, want)
	}
	if got, want := window.PeakC, 12.4; got != want {
		t.Errorf("峰值 = %.2f℃，期望 %.2f℃", got, want)
	}
	if got, want := window.SampleCount, 5; got != want {
		t.Errorf("采样点数 = %d，期望 %d", got, want)
	}
}

// TestExtractWindow_SampleCountMatchesDuration 采样点数与持续分钟一致：
// 窗口内 n 条采样、5 分钟一个周期，持续分钟就是 (n-1)×5。
// 这条不变量让复核的人能拿采样条数反查时长，不用再翻原始记录。
func TestExtractWindow_SampleCountMatchesDuration(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAt(0, 5.0),
		sampleAt(5, 8.6),
		sampleAt(10, 9.1),
		sampleAt(15, 8.4),
		sampleAt(20, 8.2),
		sampleAt(25, 5.0),
	}

	window, err := ExtractWindow(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望抽出越温窗口，实际报错：%v", err)
	}
	if window == nil {
		t.Fatal("期望抽出越温窗口，实际为 nil")
	}
	if got, want := window.DurationMinutes, (window.SampleCount-1)*5; got != want {
		t.Errorf("持续分钟 = %d，期望 %d（采样点数 %d × 5 分钟周期）",
			got, want, window.SampleCount)
	}
}

// TestExtractWindow_NoIntervalConfigured_InfersIntervalFromData 采样周期未配置时
// 由相邻采样间隔的中位数推断周期，再按采样点数折算时长。
//
// 这里刻意不再退回墙钟折算：墙钟会把采样点之间的空档算进越温时长，
// 断线越久、结论里的时长越离谱——把断线算成越温，正是这套规则要防的事。
// 现场点位配置常常缺 sample_interval_sec，从数据里把周期问出来更靠得住。
func TestExtractWindow_NoIntervalConfigured_InfersIntervalFromData(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAt(0, 5.0),
		sampleAt(5, 8.6),
		sampleAt(15, 9.1),
		sampleAt(20, 5.0),
	}

	zone := testZone()
	window, err := ExtractWindow(samples, zone)
	if err != nil {
		t.Fatalf("期望抽出越温窗口，实际报错：%v", err)
	}
	if window == nil {
		t.Fatal("期望抽出越温窗口，实际为 nil")
	}
	// 相邻间隔 5 / 10 / 5 分钟，中位数为 5 分钟。
	if got, want := window.IntervalSource, IntervalSourceInferred; got != want {
		t.Errorf("采样周期来源 = %s，期望 %s", got, want)
	}
	if got, want := window.SamplingIntervalSec, 300; got != want {
		t.Errorf("采样周期 = %d 秒，期望 %d 秒（由相邻间隔中位数推断）", got, want)
	}
	// 越限段两条采样：确认时长 = (2-1)×5 = 5 分钟。
	if got, want := window.DurationMinutes, 5; got != want {
		t.Errorf("持续分钟 = %d，期望 %d（按推断出的采样周期折算）", got, want)
	}
}

// TestExtractWindow_NoIntervalInferable_ReportsWallClockSource 只有一条采样、
// 推断不出周期时，如实把来源标成 wall_clock，而不是假装有周期。
func TestExtractWindow_NoIntervalInferable_ReportsWallClockSource(t *testing.T) {
	zone := testZone()
	zone.SampleIntervalSec = 0

	summary, err := SummarizeExcursions([]domain.TemperatureEvent{sampleAt(0, 5.0)}, zone)
	if err != nil {
		t.Fatalf("期望汇总成功，实际报错：%v", err)
	}
	if got, want := summary.IntervalSource, IntervalSourceWallClock; got != want {
		t.Errorf("采样周期来源 = %s，期望 %s", got, want)
	}
}

// TestExtractWindow_LowSideExcursion 下侧越限（偏冷）：峰值取偏离温区最远的读数，
// 也就是这段窗口里最低的那条；窗口起止同样落在真实越限采样点上。
func TestExtractWindow_LowSideExcursion(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAt(0, 4.0),
		sampleAt(5, 1.0),   // 低于下界 2℃
		sampleAt(10, -3.0), // 更低
		sampleAt(15, 0.5),
		sampleAt(20, 4.0),
	}

	window, err := ExtractWindow(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望抽出越温窗口，实际报错：%v", err)
	}
	if window == nil {
		t.Fatal("期望抽出越温窗口，实际为 nil")
	}
	if got, want := window.PeakC, -3.0; got != want {
		t.Errorf("峰值 = %.2f℃，期望 %.2f℃（下侧越限取最低读数）", got, want)
	}
	if got, want := window.PeakAt, sampleAt(10, 0).MeasuredAt; !got.Equal(want) {
		t.Errorf("峰值时刻 = %s，期望 %s", got, want)
	}
	if got, want := window.Start, sampleAt(5, 0).MeasuredAt; !got.Equal(want) {
		t.Errorf("窗口起点 = %s，期望 %s", got, want)
	}
	if got, want := window.End, sampleAt(15, 0).MeasuredAt; !got.Equal(want) {
		t.Errorf("窗口终点 = %s，期望 %s", got, want)
	}
}

// TestExtractWindow_NoExcursion 全部落在温区内：没有越温窗口，但不是失败。
func TestExtractWindow_NoExcursion(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAt(0, 5.0),
		sampleAt(5, 8.0),  // 恰好等于 zone_max_c，仍在区内（闭区间）
		sampleAt(10, 2.0), // 恰好等于 zone_min_c，仍在区内
		sampleAt(15, 5.2),
	}

	window, err := ExtractWindow(samples, hystZone(10))
	if err != nil {
		t.Fatalf("未越限时期望无错误，实际报错：%v", err)
	}
	if window != nil {
		t.Fatalf("未越限时期望 nil 窗口，实际得到 %+v", window)
	}
}

// TestExtractWindow_EmptySeries 空序列：无窗口、无错误。
func TestExtractWindow_EmptySeries(t *testing.T) {
	window, err := ExtractWindow(nil, hystZone(10))
	if err != nil {
		t.Fatalf("空序列期望无错误，实际报错：%v", err)
	}
	if window != nil {
		t.Fatalf("空序列期望 nil 窗口，实际得到 %+v", window)
	}
}

// TestExtractWindow_ZoneUnusableFailsClosed 温区判不了时既不产出窗口也不给结论：
// 「判不了」不等于「没越限」，不能拿零值温区硬比。
func TestExtractWindow_ZoneUnusableFailsClosed(t *testing.T) {
	zone := manualDecision("缺少点位温区配置，无法判定越限")

	window, err := ExtractWindow([]domain.TemperatureEvent{sampleAt(0, 11.4)}, zone)
	if err == nil {
		t.Fatal("温区不可用时期望报错")
	}
	if e := errcode.As(err); e == nil || e.Code != errcode.Validation {
		t.Errorf("错误码 = %v，期望 %s", e, errcode.Validation)
	}
	if window != nil {
		t.Errorf("温区不可用时期望不产出窗口，实际得到 %+v", window)
	}
}

// TestSummarizeExcursions_ShortExcursionBelowThreshold 未达门槛的短时波动也要统计出来：
// 「没成立窗口」不等于「没发生过越限」，这两种情况对药师是两回事。
func TestSummarizeExcursions_ShortExcursionBelowThreshold(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAt(0, 5.0),
		sampleAt(5, 8.6), // 越限
		sampleAt(10, 5.2),
	}

	summary, err := SummarizeExcursions(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望汇总成功，实际报错：%v", err)
	}
	if got, want := summary.SegmentCount, 1; got != want {
		t.Errorf("越限段数 = %d，期望 %d", got, want)
	}
	if got, want := summary.OutOfRangeSamples, 1; got != want {
		t.Errorf("越限读数条数 = %d，期望 %d", got, want)
	}
	if got, want := summary.LongestConfirmedMinutes, 0; got != want {
		t.Errorf("最长确认时长 = %d 分钟，期望 %d 分钟", got, want)
	}
	if summary.MeetsThreshold() {
		t.Error("未达门槛的短时波动不该被判为达到门槛")
	}
}

// TestSummarizeExcursions_ReportsLongestSegment 汇总取最长的一段：
// 有窗口时，汇总里的最长确认时长必须与窗口的持续分钟一致。
func TestSummarizeExcursions_ReportsLongestSegment(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAt(0, 5.0),
		sampleAt(5, 8.6),
		sampleAt(10, 9.1),
		sampleAt(15, 8.4),
		sampleAt(20, 5.2),
	}

	summary, err := SummarizeExcursions(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望汇总成功，实际报错：%v", err)
	}
	if got, want := summary.SegmentCount, 1; got != want {
		t.Errorf("越限段数 = %d，期望 %d", got, want)
	}
	if got, want := summary.OutOfRangeSamples, 3; got != want {
		t.Errorf("越限读数条数 = %d，期望 %d", got, want)
	}
	if got, want := summary.LongestConfirmedMinutes, 10; got != want {
		t.Errorf("最长确认时长 = %d 分钟，期望 %d 分钟", got, want)
	}
	if !summary.MeetsThreshold() {
		t.Error("确认时长 10 分钟达到门槛 10 分钟，应当判为达到门槛")
	}

	window, err := ExtractWindow(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望抽出越温窗口，实际报错：%v", err)
	}
	if window == nil {
		t.Fatal("期望抽出越温窗口，实际为 nil")
	}
	if window.DurationMinutes != summary.LongestConfirmedMinutes {
		t.Errorf("窗口持续 %d 分钟，与汇总的最长确认时长 %d 分钟对不上",
			window.DurationMinutes, summary.LongestConfirmedMinutes)
	}
}

// TestSummarizeExcursions_ZoneUnusableFailsClosed 温区判不了时汇总也不给：
// 拿零值温区去比出来的「越限情况」是假的。
func TestSummarizeExcursions_ZoneUnusableFailsClosed(t *testing.T) {
	zone := manualDecision("缺少点位温区配置，无法判定越限")

	summary, err := SummarizeExcursions([]domain.TemperatureEvent{sampleAt(0, 11.4)}, zone)
	if err == nil {
		t.Fatal("温区不可用时期望报错")
	}
	if e := errcode.As(err); e == nil || e.Code != errcode.Validation {
		t.Errorf("错误码 = %v，期望 %s", e, errcode.Validation)
	}
	if summary.SegmentCount != 0 {
		t.Errorf("温区不可用时期望不给出越限段，实际得到 %d 段", summary.SegmentCount)
	}
}
