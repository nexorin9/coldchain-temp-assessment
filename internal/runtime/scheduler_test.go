package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/ingest"
)

const testPointID = "point-vax-freezer-01"

// testProfile 返回一条脱敏的温区配置：2～8℃、5 分钟采样周期。
func testProfile() *domain.PointProfile {
	return &domain.PointProfile{
		PointID:             testPointID,
		ZoneMinC:            2,
		ZoneMaxC:            8,
		SampleIntervalSec:   300,
		MinExcursionMinutes: 10,
		HysteresisC:         1,
		AccuracyLimitC:      0.5,
	}
}

// sample 造一条采样。温度与时刻由调用方给。
func sample(at string, tempC float64) domain.TemperatureEvent {
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		panic("测试数据里的时刻不可解析：" + at)
	}
	return domain.TemperatureEvent{
		PointID:      testPointID,
		DeviceID:     "device-recorder-0001",
		DeviceName:   "疫苗冷藏库温度记录仪",
		MeasuredAt:   t,
		TemperatureC: tempC,
		Unit:         domain.UnitCelsius,
		Source:       domain.SourceRecorder,
	}
}

// TestDispatch_SameWindowTenDeliveries_OneRun 是本任务的核心不变量：
// 同一次越温被重复投递 10 次，只能判一次。
func TestDispatch_SameWindowTenDeliveries_OneRun(t *testing.T) {
	s := NewScheduler(nil)
	profile := testProfile()
	ctx := context.Background()

	ev := sample("2026-10-20T03:15:00+08:00", 12.4)
	var firstRunID string
	for i := 0; i < 10; i++ {
		result, err := s.Dispatch(ctx, ev, profile)
		if err != nil {
			t.Fatalf("第 %d 次投递出错：%v", i+1, err)
		}
		if i == 0 {
			firstRunID = result.Run.RunID
			if result.Outcome != OutcomeCreated {
				t.Fatalf("首次投递的结局应是 created，实际是 %q", result.Outcome)
			}
		} else {
			if result.Run.RunID != firstRunID {
				t.Fatalf("第 %d 次投递换了 run_id：%s ≠ %s", i+1, result.Run.RunID, firstRunID)
			}
			if result.Outcome != OutcomeDuplicate {
				t.Fatalf("第 %d 次重复投递的结局应是 duplicate，实际是 %q", i+1, result.Outcome)
			}
		}
		if result.SampleCount != 1 {
			t.Fatalf("重复投递不应增加样本数，实际 %d", result.SampleCount)
		}
	}

	if got := s.RunCount(); got != 1 {
		t.Fatalf("同窗重复投递 10 次应只产生 1 条 run，实际 %d 条", got)
	}
}

// TestDispatch_SamePointDifferentWindows_TwoRuns：同一点位上两段不相邻的越温
// 必须判成两条 run——去抖只能并同一次越温，不能把后续的第二次越温也吞掉。
func TestDispatch_SamePointDifferentWindows_TwoRuns(t *testing.T) {
	s := NewScheduler(nil)
	profile := testProfile()
	ctx := context.Background()

	series := []domain.TemperatureEvent{
		sample("2026-10-20T03:00:00+08:00", 5.2),  // 温区内
		sample("2026-10-20T03:10:00+08:00", 8.6),  // 越限，窗口一
		sample("2026-10-20T03:15:00+08:00", 12.4), // 越限，窗口一
		sample("2026-10-20T03:20:00+08:00", 9.8),  // 越限，窗口一
		sample("2026-10-20T03:25:00+08:00", 6.1),  // 回到温区内，窗口一关闭
		sample("2026-10-20T05:00:00+08:00", 9.1),  // 越限，窗口二
		sample("2026-10-20T05:05:00+08:00", 10.4), // 越限，窗口二
		sample("2026-10-20T05:10:00+08:00", 6.5),  // 回到温区内，窗口二关闭
	}

	results, err := s.DispatchSeries(ctx, series, profile)
	if err != nil {
		t.Fatalf("投递整段采样出错：%v", err)
	}

	if got := s.RunCount(); got != 2 {
		t.Fatalf("同点两段越温应产生 2 条 run，实际 %d 条", got)
	}

	runIDs := map[string]bool{}
	for _, r := range results {
		if r.Outcome == OutcomeCreated {
			runIDs[r.Run.RunID] = true
		}
	}
	if len(runIDs) != 2 {
		t.Fatalf("应恰好新建 2 条 run，实际新建 %d 条", len(runIDs))
	}

	// 两条 run 的窗口起点必须不同，外部引用也不同——台账侧靠这个区分两次越温。
	seenKeys := map[string]bool{}
	for _, run := range s.Runs() {
		if seenKeys[run.IdempotencyKey] {
			t.Fatalf("两条 run 的窗口幂等键相同：%s", run.IdempotencyKey)
		}
		seenKeys[run.IdempotencyKey] = true
		if run.ExternalRef == "" {
			t.Fatalf("run %s 的 external_ref 为空", run.RunID)
		}
	}
}

// TestDispatch_MergeKeepsRunIDAndGrowsSamples：同一窗口里的新采样并入既有 run，
// run_id 不变、样本数增加、窗口边界刷新。
func TestDispatch_MergeKeepsRunIDAndGrowsSamples(t *testing.T) {
	s := NewScheduler(nil)
	profile := testProfile()
	ctx := context.Background()

	first, err := s.Dispatch(ctx, sample("2026-10-20T03:10:00+08:00", 8.6), profile)
	if err != nil {
		t.Fatalf("首次投递出错：%v", err)
	}
	if first.Outcome != OutcomeCreated || first.SampleCount != 1 {
		t.Fatalf("首次投递应是 created 且样本数 1，实际 %q / %d", first.Outcome, first.SampleCount)
	}

	second, err := s.Dispatch(ctx, sample("2026-10-20T03:15:00+08:00", 12.4), profile)
	if err != nil {
		t.Fatalf("第二次投递出错：%v", err)
	}
	if second.Outcome != OutcomeMerged {
		t.Fatalf("同窗新采样的结局应是 merged，实际是 %q", second.Outcome)
	}
	if second.Run.RunID != first.Run.RunID {
		t.Fatalf("并入后 run_id 变了：%s ≠ %s", second.Run.RunID, first.Run.RunID)
	}
	if second.SampleCount != 2 {
		t.Fatalf("并入后样本数应是 2，实际 %d", second.SampleCount)
	}
	if second.Window == nil {
		t.Fatal("并入后窗口不应为 nil")
	}
	if second.Window.End != second.Window.Start.Add(5*time.Minute) {
		t.Fatalf("窗口终点未刷新：%s", second.Window.End)
	}
	if second.Window.PeakC != 12.4 {
		t.Fatalf("峰值未刷新：%v", second.Window.PeakC)
	}
	if got := s.RunCount(); got != 1 {
		t.Fatalf("并入不应新建 run，实际 run 数 %d", got)
	}
	if got := s.SampleCount(first.Run.RunID); got != 2 {
		t.Fatalf("按 run_id 查到的样本数应是 2，实际 %d", got)
	}
}

// TestDispatch_ConcurrentSameEvent_OneRun：并发投递同一条采样也不能建出两条 run。
// 按 point_id 串行化就是为了挡这个。
func TestDispatch_ConcurrentSameEvent_OneRun(t *testing.T) {
	s := NewScheduler(nil)
	profile := testProfile()
	ctx := context.Background()
	ev := sample("2026-10-20T03:15:00+08:00", 12.4)

	const workers = 32
	var wg sync.WaitGroup
	runIDs := make([]string, workers)
	errs := make([]error, workers)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(idx int) {
			defer wg.Done()
			result, err := s.Dispatch(ctx, ev, profile)
			errs[idx] = err
			if err == nil {
				runIDs[idx] = result.Run.RunID
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个并发投递出错：%v", i, err)
		}
		if runIDs[i] != runIDs[0] {
			t.Fatalf("并发投递产生了不同 run_id：%s ≠ %s", runIDs[i], runIDs[0])
		}
	}
	if got := s.RunCount(); got != 1 {
		t.Fatalf("并发投递同一条采样应只产生 1 条 run，实际 %d 条", got)
	}
}

// TestDispatch_InRangeSample_NoRun：温区内的采样不该凭空生成判定。
func TestDispatch_InRangeSample_NoRun(t *testing.T) {
	s := NewScheduler(nil)
	profile := testProfile()

	result, err := s.Dispatch(context.Background(), sample("2026-10-20T03:00:00+08:00", 5.2), profile)
	if err != nil {
		t.Fatalf("投递温区内采样出错：%v", err)
	}
	if result.Outcome != OutcomeNoWindow {
		t.Fatalf("温区内采样的结局应是 no_window，实际 %q", result.Outcome)
	}
	if result.Window != nil {
		t.Fatalf("温区内采样不应带窗口，实际 %+v", result.Window)
	}
	if got := s.RunCount(); got != 0 {
		t.Fatalf("温区内采样不该建 run，实际 %d 条", got)
	}
}

// TestDispatch_WindowClosedWhenBackInRange：越限之后回到温区内，窗口关闭，
// run 仍留在索引里可回查，但不再吸收新采样。
func TestDispatch_WindowClosedWhenBackInRange(t *testing.T) {
	s := NewScheduler(nil)
	profile := testProfile()
	ctx := context.Background()

	if _, err := s.Dispatch(ctx, sample("2026-10-20T03:10:00+08:00", 8.6), profile); err != nil {
		t.Fatalf("投递越限采样出错：%v", err)
	}
	closed, err := s.Dispatch(ctx, sample("2026-10-20T03:15:00+08:00", 5.0), profile)
	if err != nil {
		t.Fatalf("投递回温采样出错：%v", err)
	}
	if closed.Outcome != OutcomeClosed {
		t.Fatalf("回到温区内的结局应是 closed，实际 %q", closed.Outcome)
	}
	if got := s.RunCount(); got != 1 {
		t.Fatalf("关闭窗口不应改变 run 数，实际 %d", got)
	}
	if closed.Window == nil || closed.Window.PeakC != 8.6 {
		t.Fatalf("关闭时窗口应保留峰值 8.6，实际 %+v", closed.Window)
	}
}

// TestDispatch_LateDeliveryAfterClose_MergesIntoExistingRun：
// 窗口关闭后同一条采样再送一次（网关重试），不得另起一条 run。
func TestDispatch_LateDeliveryAfterClose_MergesIntoExistingRun(t *testing.T) {
	s := NewScheduler(nil)
	profile := testProfile()
	ctx := context.Background()

	ev := sample("2026-10-20T03:10:00+08:00", 8.6)
	if _, err := s.Dispatch(ctx, ev, profile); err != nil {
		t.Fatalf("首次投递出错：%v", err)
	}
	if _, err := s.Dispatch(ctx, sample("2026-10-20T03:15:00+08:00", 5.0), profile); err != nil {
		t.Fatalf("投递回温采样出错：%v", err)
	}

	again, err := s.Dispatch(ctx, ev, profile)
	if err != nil {
		t.Fatalf("迟到重投出错：%v", err)
	}
	if again.Outcome != OutcomeDuplicate {
		t.Fatalf("迟到重投的结局应是 duplicate，实际 %q", again.Outcome)
	}
	if got := s.RunCount(); got != 1 {
		t.Fatalf("迟到重投不得新建 run，实际 %d 条", got)
	}
	if again.SampleCount != 1 {
		t.Fatalf("迟到重投不应增加样本数，实际 %d", again.SampleCount)
	}
}

// TestDispatch_BackfilledSample_KeepsOneRun：迟到补投的采样早于窗口起点时，
// 仍并入同一条 run（不拆成两条），并显式标出 backfilled。
func TestDispatch_BackfilledSample_KeepsOneRun(t *testing.T) {
	s := NewScheduler(nil)
	profile := testProfile()
	ctx := context.Background()

	first, err := s.Dispatch(ctx, sample("2026-10-20T03:15:00+08:00", 12.4), profile)
	if err != nil {
		t.Fatalf("首次投递出错：%v", err)
	}

	backfilled, err := s.Dispatch(ctx, sample("2026-10-20T03:10:00+08:00", 8.6), profile)
	if err != nil {
		t.Fatalf("补投采样出错：%v", err)
	}
	if !backfilled.Backfilled {
		t.Fatal("早于窗口起点的补投应标记 backfilled")
	}
	if backfilled.Run.RunID != first.Run.RunID {
		t.Fatalf("补投不得换 run_id：%s ≠ %s", backfilled.Run.RunID, first.Run.RunID)
	}
	if got := s.RunCount(); got != 1 {
		t.Fatalf("补投不得新建 run，实际 %d 条", got)
	}
	// 窗口起点不动：动了窗口幂等键就变了，同一次越温会被拆成两条。
	if !backfilled.Window.Start.Equal(first.Window.Start) {
		t.Fatalf("窗口起点被改动：%s ≠ %s", backfilled.Window.Start, first.Window.Start)
	}
	if backfilled.SampleCount != 2 {
		t.Fatalf("补投后样本数应是 2，实际 %d", backfilled.SampleCount)
	}
}

// TestDispatch_OutOfRangeAfterClose_StartsNewWindow：窗口被回温采样关闭之后，
// 再来的越限采样属于另一段越温，必须另起一条 run。
//
// 这条容易做错：关闭后 10 分钟又越限，若还并进上一条 run，
// 结论就会变成「一次持续很久的断电」——而现场其实是两段。
func TestDispatch_OutOfRangeAfterClose_StartsNewWindow(t *testing.T) {
	s := NewScheduler(nil)
	profile := testProfile()
	ctx := context.Background()

	if _, err := s.Dispatch(ctx, sample("2026-10-20T03:10:00+08:00", 8.6), profile); err != nil {
		t.Fatalf("首次投递出错：%v", err)
	}
	if _, err := s.Dispatch(ctx, sample("2026-10-20T03:15:00+08:00", 5.0), profile); err != nil {
		t.Fatalf("投递回温采样出错：%v", err)
	}

	second, err := s.Dispatch(ctx, sample("2026-10-20T03:25:00+08:00", 9.4), profile)
	if err != nil {
		t.Fatalf("关闭后越限投递出错：%v", err)
	}
	if second.Outcome != OutcomeCreated {
		t.Fatalf("关闭后的越限应是新建 run，实际 %q", second.Outcome)
	}
	if got := s.RunCount(); got != 2 {
		t.Fatalf("关闭后的越限应产生第 2 条 run，实际 %d 条", got)
	}
}

// TestDispatch_FarGapStartsNewWindow：缺口超过容差时不得把两段越温连成一段。
func TestDispatch_FarGapStartsNewWindow(t *testing.T) {
	s := NewScheduler(nil)
	profile := testProfile()
	ctx := context.Background()

	if _, err := s.Dispatch(ctx, sample("2026-10-20T03:10:00+08:00", 8.6), profile); err != nil {
		t.Fatalf("首次投递出错：%v", err)
	}
	// 1 小时后：超出 3 个采样周期（15 分钟）的容差。
	second, err := s.Dispatch(ctx, sample("2026-10-20T04:10:00+08:00", 9.2), profile)
	if err != nil {
		t.Fatalf("第二次投递出错：%v", err)
	}
	if second.Outcome != OutcomeCreated {
		t.Fatalf("大缺口后的越限应是新建 run，实际 %q", second.Outcome)
	}
	if got := s.RunCount(); got != 2 {
		t.Fatalf("大缺口应产生 2 条 run，实际 %d", got)
	}
}

// TestDispatch_MissingProfile_NeedsManual：温区配置缺失一律转人工，不猜温区。
func TestDispatch_MissingProfile_NeedsManual(t *testing.T) {
	s := NewScheduler(nil)

	result, err := s.Dispatch(context.Background(), sample("2026-10-20T03:15:00+08:00", 12.4), nil)
	if err == nil {
		t.Fatal("缺少温区配置时应返回错误")
	}
	if got := errcode.As(err).Code; got != errcode.Validation {
		t.Fatalf("错误码应是 E_VALIDATION，实际 %s", got)
	}
	if !result.NeedsManual || result.NeedsManualReason == "" {
		t.Fatalf("应显式转人工并说明原因，实际 %+v", result)
	}
	if got := s.RunCount(); got != 0 {
		t.Fatalf("转人工时不应建 run，实际 %d 条", got)
	}
}

// TestDispatch_ProfilePointMismatch_NeedsManual：点位与温区配置对不上时转人工。
func TestDispatch_ProfilePointMismatch_NeedsManual(t *testing.T) {
	s := NewScheduler(nil)
	profile := testProfile()
	profile.PointID = "point-other-freezer-02"

	result, err := s.Dispatch(context.Background(), sample("2026-10-20T03:15:00+08:00", 12.4), profile)
	if err == nil {
		t.Fatal("点位不一致时应返回错误")
	}
	if got := errcode.As(err).Code; got != errcode.Validation {
		t.Fatalf("错误码应是 E_VALIDATION，实际 %s", got)
	}
	if !result.NeedsManual {
		t.Fatalf("点位不一致应转人工，实际 %+v", result)
	}
}

// TestDispatch_ExternalRefAndKeyStableAcrossDeliveries：同一窗口重复投递时，
// 外部引用与窗口幂等键必须一模一样——台账靠它防重复入账。
func TestDispatch_ExternalRefAndKeyStableAcrossDeliveries(t *testing.T) {
	s := NewScheduler(nil)
	profile := testProfile()
	ctx := context.Background()

	first, err := s.Dispatch(ctx, sample("2026-10-20T03:10:00+08:00", 8.6), profile)
	if err != nil {
		t.Fatalf("首次投递出错：%v", err)
	}
	second, err := s.Dispatch(ctx, sample("2026-10-20T03:15:00+08:00", 12.4), profile)
	if err != nil {
		t.Fatalf("第二次投递出错：%v", err)
	}

	if first.Run.ExternalRef != second.Run.ExternalRef {
		t.Fatalf("外部引用不一致：%s ≠ %s", first.Run.ExternalRef, second.Run.ExternalRef)
	}
	if first.Run.IdempotencyKey != second.Run.IdempotencyKey {
		t.Fatalf("窗口幂等键不一致：%s ≠ %s", first.Run.IdempotencyKey, second.Run.IdempotencyKey)
	}
	if _, ok := s.RunByKey(first.Run.IdempotencyKey); !ok {
		t.Fatal("按窗口幂等键应能查回该 run")
	}
	// 窗口幂等键必须是窗口起点与点位的函数：换一个起点就换一把钥匙。
	otherKey := domain.WindowIdempotencyKey(testPointID, time.Now())
	if otherKey == first.Run.IdempotencyKey {
		t.Fatal("不同窗口起点不应算出同一个幂等键")
	}
}

// TestDispatch_SeriesOrderIndependentOfArrivalOrder：补投打乱到达顺序时，
// 按时刻投递的结果应当一致——窗口只认时刻，不认到达顺序。
func TestDispatch_SeriesOrderIndependentOfArrivalOrder(t *testing.T) {
	profile := testProfile()
	ctx := context.Background()
	series := []domain.TemperatureEvent{
		sample("2026-10-20T03:10:00+08:00", 8.6),
		sample("2026-10-20T03:15:00+08:00", 12.4),
		sample("2026-10-20T03:20:00+08:00", 9.8),
	}

	forward := NewScheduler(nil)
	if _, err := forward.DispatchSeries(ctx, series, profile); err != nil {
		t.Fatalf("正序投递出错：%v", err)
	}

	backward := NewScheduler(nil)
	reversed := []domain.TemperatureEvent{series[2], series[0], series[1]}
	if _, err := backward.DispatchSeries(ctx, reversed, profile); err != nil {
		t.Fatalf("乱序投递出错：%v", err)
	}

	if forward.RunCount() != 1 || backward.RunCount() != 1 {
		t.Fatalf("正序与乱序都应只判一次，实际 %d / %d", forward.RunCount(), backward.RunCount())
	}
	fRun := forward.Runs()[0]
	bRun := backward.Runs()[0]
	if fRun.IdempotencyKey != bRun.IdempotencyKey {
		t.Fatalf("正序与乱序的窗口幂等键不一致：%s ≠ %s", fRun.IdempotencyKey, bRun.IdempotencyKey)
	}
	if fRun.ExternalRef != bRun.ExternalRef {
		t.Fatalf("正序与乱序的外部引用不一致：%s ≠ %s", fRun.ExternalRef, bRun.ExternalRef)
	}
	if got := forward.SampleCount(fRun.RunID); got != 3 {
		t.Fatalf("正序投递后样本数应是 3，实际 %d", got)
	}
}

// TestDispatch_ZoneBoundarySample_IsInRange：恰好等于温区上界的读数仍在区内。
func TestDispatch_ZoneBoundarySample_IsInRange(t *testing.T) {
	s := NewScheduler(nil)
	profile := testProfile()

	result, err := s.Dispatch(context.Background(), sample("2026-10-20T03:10:00+08:00", 8.0), profile)
	if err != nil {
		t.Fatalf("投递边界采样出错：%v", err)
	}
	if result.Outcome != OutcomeNoWindow {
		t.Fatalf("等于 zone_max_c 的采样不该越限，实际 %q", result.Outcome)
	}
	if got := s.RunCount(); got != 0 {
		t.Fatalf("边界采样不该建 run，实际 %d 条", got)
	}
}

// TestDispatch_NewRunCarriesQueuedExecState：新建的 run 应处于 queued 执行态，
// 等后续步骤链接手；业务状态是 detected（越温已检出、结论未出）。
func TestDispatch_NewRunCarriesQueuedExecState(t *testing.T) {
	s := NewScheduler(nil)

	result, err := s.Dispatch(context.Background(), sample("2026-10-20T03:15:00+08:00", 12.4), testProfile())
	if err != nil {
		t.Fatalf("投递出错：%v", err)
	}
	if result.Run.ExecState != domain.ExecStateQueued {
		t.Fatalf("新建 run 的执行态应是 queued，实际 %q", result.Run.ExecState)
	}
	if result.Run.State != domain.RunStateDetected {
		t.Fatalf("新建 run 的业务状态应是 detected，实际 %q", result.Run.State)
	}
	if result.Run.RunID == "" {
		t.Fatal("新建 run 必须有 run_id")
	}
	if result.Run.PointID != testPointID {
		t.Fatalf("run 的点位应是 %s，实际 %s", testPointID, result.Run.PointID)
	}
}

// TestDispatch_RealSampleFile_SingleRunPerExcursion 用仓库里真实的脱敏样例文件跑一遍：
// 7 条采样（其中 4 条越限、1 条回温）必须只判出 1 条 run，窗口与规则层一致。
//
// 这一条不是重复前面的合成用例：它走的是真实的入站文件 → 规范化 → 调度链路，
// 证明调度器接得上真实的现场数据形态，而不是只在自己的合成数据上自洽。
func TestDispatch_RealSampleFile_SingleRunPerExcursion(t *testing.T) {
	raws, err := ingest.LoadEventSeries("../../testdata/series_excursion.json")
	if err != nil {
		t.Fatalf("读入样例序列失败：%v", err)
	}
	profile, err := ingest.LoadProfileFile("../../testdata/point_profile.json")
	if err != nil {
		t.Fatalf("读入温区配置失败：%v", err)
	}

	samples := make([]domain.TemperatureEvent, 0, len(raws))
	for _, raw := range raws {
		ev, err := ingest.NormalizeEvent(raw)
		if err != nil {
			t.Fatalf("规范化样例事件失败：%v", err)
		}
		samples = append(samples, ev)
	}

	s := NewScheduler(nil)
	results, err := s.DispatchSeries(context.Background(), samples, &profile)
	if err != nil {
		t.Fatalf("投递样例序列失败：%v", err)
	}
	if len(results) != len(samples) {
		t.Fatalf("回执条数应与采样条数一致：%d ≠ %d", len(results), len(samples))
	}

	if got := s.RunCount(); got != 1 {
		t.Fatalf("样例序列里只有一段越温，应只判出 1 条 run，实际 %d 条", got)
	}

	run := s.Runs()[0]
	window, ok := s.WindowOf(run.RunID)
	if !ok {
		t.Fatal("应能取回该 run 的窗口")
	}
	if window.SampleCount != 4 {
		t.Fatalf("窗口应纳入 4 条越限采样，实际 %d", window.SampleCount)
	}
	if window.PeakC != 12.4 {
		t.Fatalf("窗口峰值应是 12.4，实际 %v", window.PeakC)
	}
	if window.DurationMinutes != 15 {
		t.Fatalf("窗口持续分钟应是 15，实际 %d", window.DurationMinutes)
	}
	// 外部引用与窗口幂等键必须与规则层最窄路径算出来的一致——
	// 两处算出不同的键，台账侧就会把同一次越温认成两次。
	expectedRef := domain.ExternalRef(run.PointID, window.Start)
	if run.ExternalRef != expectedRef {
		t.Fatalf("外部引用与窗口起点不同源：%s ≠ %s", run.ExternalRef, expectedRef)
	}
	expectedKey := domain.WindowIdempotencyKey(run.PointID, window.Start)
	if run.IdempotencyKey != expectedKey {
		t.Fatalf("窗口幂等键与窗口起点不同源：%s ≠ %s", run.IdempotencyKey, expectedKey)
	}
}
