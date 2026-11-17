package rules

import (
	"strings"
	"testing"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
)

// 本文件测的是两条收口规则：
//   - 短时波动过滤：未达最短越限门槛的越限不进正式窗口；
//   - 相邻越限合并：紧邻且回落不足门槛的两段越限合成一条窗口。
//
// 样例统一用 5 分钟一采的栅格（hystZone：温区 2~8℃、采样周期 5 分钟、
// 最短越限 10 分钟、迟滞带 1℃），对应现场常见配置。

// TestExtractWindows_TransientExcursionFilteredOut 短时波动被过滤：
// 一次 5 分钟的越限不成立越温窗口，但它确实发生过，
// 必须单独归进短时波动列表，不能说成「压根没越限」。
func TestExtractWindows_TransientExcursionFilteredOut(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAt(0, 5.0),
		sampleAt(5, 8.6),  // 越限
		sampleAt(10, 9.1), // 越限
		sampleAt(15, 5.2), // 回落，回到温区
	}

	windows, err := ExtractWindows(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望抽取成功，实际报错：%v", err)
	}
	if len(windows) != 0 {
		t.Fatalf("短时波动不该生成越温窗口，实际生成 %d 条", len(windows))
	}

	transients, err := ExtractTransientExcursions(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望抽取短时波动成功，实际报错：%v", err)
	}
	if len(transients) != 1 {
		t.Fatalf("短时波动段数 = %d，期望 %d", len(transients), 1)
	}
	tr := transients[0]
	if got, want := tr.Start, sampleAt(5, 0).MeasuredAt; !got.Equal(want) {
		t.Errorf("波动起点 = %s，期望 %s", got, want)
	}
	if got, want := tr.End, sampleAt(10, 0).MeasuredAt; !got.Equal(want) {
		t.Errorf("波动终点 = %s，期望 %s", got, want)
	}
	if got, want := tr.DurationMinutes, 5; got != want {
		t.Errorf("波动时长 = %d 分钟，期望 %d 分钟", got, want)
	}
	if got, want := tr.PeakC, 9.1; got != want {
		t.Errorf("波动峰值 = %.2f℃，期望 %.2f℃", got, want)
	}
	if got, want := tr.PeakAt, sampleAt(10, 0).MeasuredAt; !got.Equal(want) {
		t.Errorf("波动峰值时刻 = %s，期望 %s", got, want)
	}
	if got, want := tr.OutOfRangeSamples, 2; got != want {
		t.Errorf("波动越限条数 = %d，期望 %d", got, want)
	}
	if got, want := tr.Side, domain.ExcursionSideHigh; got != want {
		t.Errorf("波动侧别 = %q，期望 %q", got, want)
	}
	if !strings.Contains(tr.Reason, "10 分钟") {
		t.Errorf("波动理由应写清门槛，实际：%s", tr.Reason)
	}
}

// TestExtractWindows_TransientNotInWindow_AndWindowUnchanged 短时波动不改变正式窗口的结论：
// 一批采样里既有够门槛的越温、又有不够门槛的波动时，
// 窗口结论必须与只有那次越温时一模一样。
func TestExtractWindows_TransientNotInWindow_AndWindowUnchanged(t *testing.T) {
	withTransient := []domain.TemperatureEvent{
		sampleAt(0, 5.0),
		sampleAt(5, 8.6),
		sampleAt(10, 9.5),
		sampleAt(15, 10.0), // 够门槛的越限
		sampleAt(20, 5.2),  // 回落
		sampleAt(25, 5.0),
		sampleAt(30, 8.8), // 短时波动（单条采样，确认时长 0 分钟）
		sampleAt(35, 5.1),
	}

	windows, err := ExtractWindows(withTransient, hystZone(10))
	if err != nil {
		t.Fatalf("期望抽取成功，实际报错：%v", err)
	}
	if len(windows) != 1 {
		t.Fatalf("正式窗口数 = %d，期望 %d", len(windows), 1)
	}
	if got, want := windows[0].DurationMinutes, 10; got != want {
		t.Errorf("窗口持续 = %d 分钟，期望 %d 分钟（短时波动不该计入）", got, want)
	}
	if got, want := windows[0].OutOfRangeSamples, 3; got != want {
		t.Errorf("窗口越限条数 = %d，期望 %d（短时波动不该计入）", got, want)
	}
	if got, want := windows[0].End, sampleAt(15, 0).MeasuredAt; !got.Equal(want) {
		t.Errorf("窗口终点 = %s，期望 %s", got, want)
	}

	transients, err := ExtractTransientExcursions(withTransient, hystZone(10))
	if err != nil {
		t.Fatalf("期望抽取短时波动成功，实际报错：%v", err)
	}
	if len(transients) != 1 {
		t.Fatalf("短时波动段数 = %d，期望 %d", len(transients), 1)
	}
}

// TestExtractWindows_AdjacentSegmentsMergedIntoOneWindow 相邻两段越限合并成一条窗口：
// 两段各自都够门槛，中间只回落了一个采样周期（5 分钟 < 门槛 10 分钟），
// 说明设备没有真正恢复，应当算同一次异常，台账上只记一条。
func TestExtractWindows_AdjacentSegmentsMergedIntoOneWindow(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAt(0, 5.0),
		sampleAt(5, 8.6), // 第一段越限
		sampleAt(10, 9.5),
		sampleAt(15, 10.0),
		sampleAt(20, 6.0), // 回落到温区（只落一次采样就又冲出去）
		sampleAt(25, 9.0), // 第二段越限
		sampleAt(30, 9.5),
		sampleAt(35, 10.5),
		sampleAt(40, 5.0),
	}

	windows, err := ExtractWindows(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望抽取成功，实际报错：%v", err)
	}
	if len(windows) != 1 {
		t.Fatalf("合并后正式窗口数 = %d，期望 %d（相邻两段应合并成一条）", len(windows), 1)
	}
	w := windows[0]
	if got, want := w.Start, sampleAt(5, 0).MeasuredAt; !got.Equal(want) {
		t.Errorf("窗口起点 = %s，期望 %s", got, want)
	}
	if got, want := w.End, sampleAt(35, 0).MeasuredAt; !got.Equal(want) {
		t.Errorf("窗口终点 = %s，期望 %s", got, want)
	}
	if got, want := w.DurationMinutes, 20; got != want {
		t.Errorf("窗口持续 = %d 分钟，期望 %d 分钟（两段各 10 分钟相加）", got, want)
	}
	if got, want := w.RecoveryMinutes, 5; got != want {
		t.Errorf("回落分钟 = %d，期望 %d", got, want)
	}
	if got, want := w.SegmentCount, 2; got != want {
		t.Errorf("合并段数 = %d，期望 %d", got, want)
	}
	if len(w.Segments) != 2 {
		t.Fatalf("分段明细条数 = %d，期望 %d", len(w.Segments), 2)
	}
	if got, want := w.OutOfRangeSamples, 6; got != want {
		t.Errorf("窗口越限条数 = %d，期望 %d", got, want)
	}
	if got, want := w.Segments[0].RecoveryAfterMinutes, 5; got != want {
		t.Errorf("第一段之后的回落分钟 = %d，期望 %d", got, want)
	}
	if got, want := w.Segments[1].RecoveryAfterMinutes, 0; got != want {
		t.Errorf("末段之后的回落分钟 = %d，期望 %d", got, want)
	}
}

// TestExtractWindows_MergedWindowPeakPointsToRealPeakSample 合并后的峰值指向真实峰值采样：
// 峰值可能落在第二段上，合并后必须按整条窗口重算，
// peak_at 也要指到那条真实采样，而不是拿第一段的峰值充数。
func TestExtractWindows_MergedWindowPeakPointsToRealPeakSample(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAt(0, 5.0),
		sampleAt(5, 8.6),
		sampleAt(10, 9.5),
		sampleAt(15, 10.0), // 第一段峰值
		sampleAt(20, 6.0),
		sampleAt(25, 9.0),
		sampleAt(30, 9.5),
		sampleAt(35, 10.5), // 整条窗口的峰值
		sampleAt(40, 5.0),
	}

	windows, err := ExtractWindows(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望抽取成功，实际报错：%v", err)
	}
	if len(windows) != 1 {
		t.Fatalf("合并后正式窗口数 = %d，期望 %d", len(windows), 1)
	}
	w := windows[0]
	if got, want := w.PeakC, 10.5; got != want {
		t.Errorf("合并后峰值 = %.2f℃，期望 %.2f℃", got, want)
	}
	if got, want := w.PeakAt, sampleAt(35, 0).MeasuredAt; !got.Equal(want) {
		t.Errorf("合并后峰值时刻 = %s，期望 %s", got, want)
	}
	if got, want := w.Segments[0].PeakC, 10.0; got != want {
		t.Errorf("第一段峰值 = %.2f℃，期望 %.2f℃", got, want)
	}
	if got, want := w.Segments[1].PeakC, 10.5; got != want {
		t.Errorf("第二段峰值 = %.2f℃，期望 %.2f℃", got, want)
	}
}

// TestExtractWindows_RecoveryLongEnough_NotMerged 回落时间达到门槛时不合并：
// 冰箱真的在温区内待够了一个门槛时长，那是两次独立的异常，
// 台账上应当分开记，不能合成一条。
func TestExtractWindows_RecoveryLongEnough_NotMerged(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAt(0, 5.0),
		sampleAt(5, 8.6), // 第一段越限
		sampleAt(10, 9.5),
		sampleAt(15, 10.0),
		sampleAt(20, 6.0), // 回落到温区并持续了 4 个采样周期（20 分钟 ≥ 门槛）
		sampleAt(25, 6.5),
		sampleAt(30, 6.2),
		sampleAt(35, 6.8),
		sampleAt(40, 9.0), // 第二段越限
		sampleAt(45, 9.5),
		sampleAt(50, 10.5),
		sampleAt(55, 5.0),
	}

	windows, err := ExtractWindows(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望抽取成功，实际报错：%v", err)
	}
	if len(windows) != 2 {
		t.Fatalf("正式窗口数 = %d，期望 %d（回落达到门槛不该合并）", len(windows), 2)
	}
	for i, w := range windows {
		if got, want := w.DurationMinutes, 10; got != want {
			t.Errorf("第 %d 条窗口持续 = %d 分钟，期望 %d 分钟", i+1, got, want)
		}
		if got, want := w.SegmentCount, 1; got != want {
			t.Errorf("第 %d 条窗口合并段数 = %d，期望 %d", i+1, got, want)
		}
		if got, want := w.RecoveryMinutes, 0; got != want {
			t.Errorf("第 %d 条窗口回落分钟 = %d，期望 %d", i+1, got, want)
		}
	}
	if got, want := windows[0].Start, sampleAt(5, 0).MeasuredAt; !got.Equal(want) {
		t.Errorf("第一条窗口起点 = %s，期望 %s", got, want)
	}
	if got, want := windows[1].Start, sampleAt(40, 0).MeasuredAt; !got.Equal(want) {
		t.Errorf("第二条窗口起点 = %s，期望 %s", got, want)
	}
}

// TestExtractWindows_TransientBetweenSegmentsBreaksAdjacency 中间隔着短时波动的不合并：
// 两段越限之间夹着一次越限，说明期间又发生过事，
// 它们不是「紧邻」的两段，不能合成一条。
func TestExtractWindows_TransientBetweenSegmentsBreaksAdjacency(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAt(0, 5.0),
		sampleAt(5, 8.6), // 第一段越限（够门槛）
		sampleAt(10, 9.5),
		sampleAt(15, 10.0),
		sampleAt(20, 6.0),
		sampleAt(25, 9.0), // 中间这次越限不够门槛
		sampleAt(30, 9.3),
		sampleAt(35, 6.5),
		sampleAt(40, 9.2), // 第二段越限（够门槛）
		sampleAt(45, 9.8),
		sampleAt(50, 10.2),
		sampleAt(55, 5.0),
	}

	windows, err := ExtractWindows(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望抽取成功，实际报错：%v", err)
	}
	if len(windows) != 2 {
		t.Fatalf("正式窗口数 = %d，期望 %d（中间夹着波动的不该合并）", len(windows), 2)
	}
	transients, err := ExtractTransientExcursions(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望抽取短时波动成功，实际报错：%v", err)
	}
	if len(transients) != 1 {
		t.Fatalf("短时波动段数 = %d，期望 %d", len(transients), 1)
	}
	if got, want := transients[0].DurationMinutes, 5; got != want {
		t.Errorf("短时波动时长 = %d 分钟，期望 %d 分钟", got, want)
	}
}

// TestExtractWindows_DifferentSidesNotMerged 偏热与偏冷不合并：
// 越到温区哪一侧是两回事，合成一条会让「峰值取哪一侧」说不清。
func TestExtractWindows_DifferentSidesNotMerged(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAt(0, 5.0),
		sampleAt(5, 8.6), // 偏热越限
		sampleAt(10, 9.5),
		sampleAt(15, 10.0),
		sampleAt(20, 6.0),
		sampleAt(25, 1.0), // 偏冷越限
		sampleAt(30, 0.5),
		sampleAt(35, -1.0),
		sampleAt(40, 5.0),
	}

	windows, err := ExtractWindows(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望抽取成功，实际报错：%v", err)
	}
	if len(windows) != 2 {
		t.Fatalf("正式窗口数 = %d，期望 %d（两侧不该合并）", len(windows), 2)
	}
	if got, want := windows[0].PeakC, 10.0; got != want {
		t.Errorf("偏热窗口峰值 = %.2f℃，期望 %.2f℃", got, want)
	}
	if got, want := windows[1].PeakC, -1.0; got != want {
		t.Errorf("偏冷窗口峰值 = %.2f℃，期望 %.2f℃", got, want)
	}
}

// TestSummarizeExcursions_ReportsWindowsAndTransients 汇总同时报窗口数与波动数：
// 这两样一起看，才知道这批读数里到底发生过几件事。
func TestSummarizeExcursions_ReportsWindowsAndTransients(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAt(0, 5.0),
		sampleAt(5, 8.6),
		sampleAt(10, 9.5),
		sampleAt(15, 10.0), // 够门槛
		sampleAt(20, 6.0),
		sampleAt(25, 9.0), // 相邻（回落 5 分钟）→ 合并
		sampleAt(30, 9.5),
		sampleAt(35, 10.5),
		sampleAt(40, 5.0),
		sampleAt(45, 8.7), // 短时波动
		sampleAt(50, 5.0),
	}

	summary, err := SummarizeExcursions(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望汇总成功，实际报错：%v", err)
	}
	if got, want := summary.WindowCount, 1; got != want {
		t.Errorf("正式窗口数 = %d，期望 %d", got, want)
	}
	if got, want := summary.MergedWindowMinutes, 20; got != want {
		t.Errorf("合并后窗口时长 = %d 分钟，期望 %d 分钟", got, want)
	}
	if got, want := summary.LongestConfirmedMinutes, 10; got != want {
		t.Errorf("最长单段确认时长 = %d 分钟，期望 %d 分钟", got, want)
	}
	if got, want := summary.TransientSegmentCount, 1; got != want {
		t.Errorf("短时波动段数 = %d，期望 %d", got, want)
	}
	if got, want := summary.TransientSamples, 1; got != want {
		t.Errorf("短时波动越限条数 = %d，期望 %d", got, want)
	}
	if got, want := summary.SegmentCount, 3; got != want {
		t.Errorf("越限段总数 = %d，期望 %d", got, want)
	}
}

// TestExtractWindows_MergedWindowArithmetic 合并窗口的口径不变量：
// 持续分钟 = 各段越限时长之和；回落分钟只数确实落在温区内的采样；
// 两者之和不超过 (采样条数 - 1) × 采样周期，差额是过渡区间的不确定度。
func TestExtractWindows_MergedWindowArithmetic(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAt(0, 5.0),
		sampleAt(5, 8.6),
		sampleAt(10, 9.5),
		sampleAt(15, 10.0),
		sampleAt(20, 6.0),
		sampleAt(25, 9.0),
		sampleAt(30, 9.5),
		sampleAt(35, 10.5),
		sampleAt(40, 5.0),
	}

	windows, err := ExtractWindows(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望抽取成功，实际报错：%v", err)
	}
	if len(windows) != 1 {
		t.Fatalf("正式窗口数 = %d，期望 %d", len(windows), 1)
	}
	w := windows[0]

	sum := 0
	for _, seg := range w.Segments {
		sum += seg.DurationMinutes
	}
	if sum != w.DurationMinutes {
		t.Errorf("各段时长之和 = %d 分钟，与窗口持续 %d 分钟对不上", sum, w.DurationMinutes)
	}
	span := (w.SampleCount - 1) * 5
	if w.DurationMinutes+w.RecoveryMinutes > span {
		t.Errorf("持续 %d + 回落 %d 分钟超过跨度 %d 分钟", w.DurationMinutes, w.RecoveryMinutes, span)
	}
	if w.DurationMinutes+w.RecoveryMinutes < span-10 {
		t.Errorf("持续 %d + 回落 %d 分钟离跨度 %d 分钟差得太多，折叠口径可能算错了",
			w.DurationMinutes, w.RecoveryMinutes, span)
	}
}

// TestExtractWindows_SingleSegmentUnchanged 单段窗口的口径不受合并规则影响：
// 没有相邻段可合并时，结论与既有规则完全一致。
func TestExtractWindows_SingleSegmentUnchanged(t *testing.T) {
	samples := []domain.TemperatureEvent{
		sampleAt(0, 5.0),
		sampleAt(5, 8.6),
		sampleAt(10, 9.1),
		sampleAt(15, 8.4),
		sampleAt(20, 5.2),
	}

	windows, err := ExtractWindows(samples, hystZone(10))
	if err != nil {
		t.Fatalf("期望抽取成功，实际报错：%v", err)
	}
	if len(windows) != 1 {
		t.Fatalf("正式窗口数 = %d，期望 %d", len(windows), 1)
	}
	w := windows[0]
	if got, want := w.SegmentCount, 1; got != want {
		t.Errorf("合并段数 = %d，期望 %d", got, want)
	}
	if got, want := w.RecoveryMinutes, 0; got != want {
		t.Errorf("回落分钟 = %d，期望 %d", got, want)
	}
	if got, want := w.DurationMinutes, 10; got != want {
		t.Errorf("持续 = %d 分钟，期望 %d 分钟", got, want)
	}
	if got, want := w.SampleCount, 3; got != want {
		t.Errorf("采样条数 = %d，期望 %d", got, want)
	}
	if len(w.Segments) != 1 {
		t.Fatalf("分段明细条数 = %d，期望 %d", len(w.Segments), 1)
	}
}

// TestExtractWindows_ZoneUnusableFailsClosed 温区判不了时既不产出窗口也不产出波动：
// 拿零值温区去比出来的「越限情况」是假的。
func TestExtractWindows_ZoneUnusableFailsClosed(t *testing.T) {
	zone := manualDecision("缺少点位温区配置，无法判定越限")
	samples := []domain.TemperatureEvent{
		sampleAt(0, 5.0),
		sampleAt(5, 8.6),
		sampleAt(10, 9.1),
	}

	windows, err := ExtractWindows(samples, zone)
	if err == nil {
		t.Fatal("温区判不了时应当报错")
	}
	if windows != nil {
		t.Errorf("温区判不了时不该产出窗口，实际产出 %d 条", len(windows))
	}

	transients, err := ExtractTransientExcursions(samples, zone)
	if err == nil {
		t.Fatal("温区判不了时抽取短时波动应当报错")
	}
	if transients != nil {
		t.Errorf("温区判不了时不该产出短时波动，实际产出 %d 段", len(transients))
	}
}
