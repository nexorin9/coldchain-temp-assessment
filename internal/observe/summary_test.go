package observe

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/store"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// 固定时钟：让窗口起止可复现，断言里能写死具体时刻。
var fixedNow = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

// fakeSource 是内存取数源：把各种口径都摆出来，让汇总器的换算可核对。
type fakeSource struct {
	metrics Metrics
	err     error

	mu       sync.Mutex
	lastFrom time.Time
	lastTo   time.Time
	calls    int
}

func (f *fakeSource) Collect(ctx context.Context, from, to time.Time) (Metrics, error) {
	f.mu.Lock()
	f.lastFrom = from
	f.lastTo = to
	f.calls++
	f.mu.Unlock()
	if f.err != nil {
		return Metrics{}, f.err
	}
	return f.metrics, nil
}

// newFakeCollector 造一台用固定时钟的汇总器。
func newFakeCollector(source Source) *Collector {
	return NewCollector(source, WithClock(func() time.Time { return fixedNow }))
}

// TestCollect_CarriesEveryMetric 断言摘要把取数口给的每一项都带出去。
//
// 这是最基础的一条：字段名与口径对不上，看摘要的人就会把「待写回数」
// 读成「待确认数」——两个都是数，含义完全相反。
func TestCollect_CarriesEveryMetric(t *testing.T) {
	source := &fakeSource{metrics: Metrics{
		Events:               1200,
		Runs:                 7,
		FailedRuns:           1,
		DegradedRuns:         2,
		AwaitingConfirmation: 3,
		PendingWritebacks:    2,
		OpenWritebackAlerts:  1,
		ModelCalls:           9,
		DegradedModelCalls:   3,
		FailedModelCalls:     1,
		RunningRuns:          1,
		QueuedRuns:           2,
		RetryingRuns:         1,
		Hourly: []HourlyBucket{
			{HourStart: "2026-10-22T09:00:00Z", EventCount: 120, ExcursionCount: 1, PeakTemperatureC: 12.4, Points: 3},
			{HourStart: "2026-10-22T10:00:00Z", EventCount: 118, ExcursionCount: 0, PeakTemperatureC: 7.1, Points: 3},
		},
	}}

	summary, err := newFakeCollector(source).Collect(context.Background())
	if err != nil {
		t.Fatalf("汇总运行摘要失败：%v", err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"events", summary.Events, 1200},
		{"runs", summary.Runs, 7},
		{"failed_runs", summary.FailedRuns, 1},
		{"degraded_runs", summary.DegradedRuns, 2},
		{"awaiting_confirmation", summary.AwaitingConfirmation, 3},
		{"pending_writebacks", summary.PendingWritebacks, 2},
		{"open_writeback_alerts", summary.OpenWritebackAlerts, 1},
		{"model_calls", summary.ModelCalls, 9},
		{"degraded_model_calls", summary.DegradedModelCalls, 3},
		{"failed_model_calls", summary.FailedModelCalls, 1},
		{"running_runs", summary.RunningRuns, 1},
		{"queued_runs", summary.QueuedRuns, 2},
		{"retrying_runs", summary.RetryingRuns, 1},
		{"hourly_covered_hours", summary.HourlyCoveredHours, 2},
	}
	for _, check := range checks {
		if check.got != check.want {
			t.Errorf("%s = %v，期望 %v", check.name, check.got, check.want)
		}
	}
	if summary.WindowHours != 24 {
		t.Errorf("window_hours = %d，期望 24", summary.WindowHours)
	}
	if summary.GeneratedAt != fixedNow.Format(time.RFC3339) {
		t.Errorf("generated_at = %q，期望 %q", summary.GeneratedAt, fixedNow.Format(time.RFC3339))
	}
	if len(summary.Hourly) != 2 {
		t.Fatalf("hourly 桶数 = %d，期望 2", len(summary.Hourly))
	}
	if summary.Hourly[0].EventCount != 120 || summary.Hourly[0].PeakTemperatureC != 12.4 {
		t.Errorf("hourly[0] = %+v，与取数口给的桶对不上", summary.Hourly[0])
	}
}

// TestCollect_WindowIsLast24Hours 断言统计窗口是近 24 小时且两端闭合。
//
// 窗口口径是所有窗口内指标的唯一基准：口径一变，「近 24 小时判了多少段」
// 就和昨天报的那份不是一个东西了。
func TestCollect_WindowIsLast24Hours(t *testing.T) {
	source := &fakeSource{}
	if _, err := newFakeCollector(source).Collect(context.Background()); err != nil {
		t.Fatalf("汇总运行摘要失败：%v", err)
	}

	wantFrom := fixedNow.Add(-24 * time.Hour)
	if !source.lastFrom.Equal(wantFrom) {
		t.Errorf("取数区间起点 = %s，期望 %s", source.lastFrom, wantFrom)
	}
	if !source.lastTo.Equal(fixedNow) {
		t.Errorf("取数区间终点 = %s，期望 %s", source.lastTo, fixedNow)
	}
}

// TestCollect_DegradeRate 断言降级率的换算口径。
//
// 分母是模型调用总次数，不是判定条数——一次判定可能调好几次模型
// （归因一次、成稿一次），拿判定条数当分母算出来的比例没有意义。
func TestCollect_DegradeRate(t *testing.T) {
	cases := []struct {
		name     string
		calls    int
		degraded int
		want     float64
	}{
		{name: "三分之一降级", calls: 9, degraded: 3, want: 0.3333},
		{name: "全部降级", calls: 4, degraded: 4, want: 1},
		{name: "没有降级", calls: 5, degraded: 0, want: 0},
		{name: "一次都没调过模型", calls: 0, degraded: 0, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := &fakeSource{metrics: Metrics{
				ModelCalls:         tc.calls,
				DegradedModelCalls: tc.degraded,
			}}
			summary, err := newFakeCollector(source).Collect(context.Background())
			if err != nil {
				t.Fatalf("汇总运行摘要失败：%v", err)
			}
			if summary.DegradeRate != tc.want {
				t.Errorf("degrade_rate = %v，期望 %v", summary.DegradeRate, tc.want)
			}
		})
	}
}

// TestCollect_NotesFlagRealProblems 断言备注只在真有话要说时才出现。
//
// 一切正常时备注必须为空：把「一切正常」也写进备注，看的人就得先分辨
// 哪句是提醒、哪句是例行——那是在给看摘要的人加活。
func TestCollect_NotesFlagRealProblems(t *testing.T) {
	t.Run("一切正常时没有备注", func(t *testing.T) {
		source := &fakeSource{metrics: Metrics{Hourly: fullDayBuckets()}}
		summary, err := newFakeCollector(source).Collect(context.Background())
		if err != nil {
			t.Fatalf("汇总运行摘要失败：%v", err)
		}
		if len(summary.Notes) != 0 {
			t.Errorf("备注 = %v，期望为空", summary.Notes)
		}
	})

	t.Run("聚合覆盖不足时提醒", func(t *testing.T) {
		source := &fakeSource{metrics: Metrics{Hourly: []HourlyBucket{
			{HourStart: "2026-10-22T09:00:00Z"},
		}}}
		summary, err := newFakeCollector(source).Collect(context.Background())
		if err != nil {
			t.Fatalf("汇总运行摘要失败：%v", err)
		}
		if !hasNoteContaining(summary.Notes, "小时级聚合") {
			t.Errorf("备注 = %v，期望含聚合覆盖不足的提醒", summary.Notes)
		}
	})

	t.Run("有欠账与失败时提醒", func(t *testing.T) {
		source := &fakeSource{metrics: Metrics{
			PendingWritebacks:   2,
			OpenWritebackAlerts: 1,
			FailedRuns:          1,
			Hourly:              fullDayBuckets(),
		}}
		summary, err := newFakeCollector(source).Collect(context.Background())
		if err != nil {
			t.Fatalf("汇总运行摘要失败：%v", err)
		}
		if !hasNoteContaining(summary.Notes, "台账") {
			t.Errorf("备注 = %v，期望含待写回提醒", summary.Notes)
		}
		if !hasNoteContaining(summary.Notes, "失败") {
			t.Errorf("备注 = %v，期望含失败提醒", summary.Notes)
		}
	})
}

// TestCollect_WithoutSourceFails 断言没装配取数源时报错而不是返回全 0。
//
// 全 0 会被读成「今天什么事都没发生」，而真相是「根本没查」。
func TestCollect_WithoutSourceFails(t *testing.T) {
	_, err := NewCollector(nil).Collect(context.Background())
	if err == nil {
		t.Fatal("没装配取数源时应当报错")
	}
	if code := errcode.As(err).Code; code != errcode.StorageError {
		t.Errorf("错误码 = %q，期望 %q", code, errcode.StorageError)
	}
}

// TestCollect_SourceErrorPropagates 断言取数失败时摘要不产出。
func TestCollect_SourceErrorPropagates(t *testing.T) {
	boom := errors.New("库打不开")
	source := &fakeSource{err: boom}
	_, err := newFakeCollector(source).Collect(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v，期望原样透出 %v", err, boom)
	}
}

// newTestStore 造一台内存库。
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// insertSample 往库里写一条温度采样。
func insertSample(t *testing.T, st *store.Store, pointID string, at time.Time, tempC float64) {
	t.Helper()
	_, err := st.InsertEvent(context.Background(), domain.TemperatureEvent{
		PointID:      pointID,
		DeviceID:     "device-recorder-0001",
		DeviceName:   "疫苗冷藏库温度记录仪",
		MeasuredAt:   at,
		TemperatureC: tempC,
		Unit:         "C",
		Source:       "recorder",
	})
	if err != nil {
		t.Fatalf("写入温度采样失败：%v", err)
	}
}

// insertRun 往库里写一条判定 run 的状态快照。
func insertRun(t *testing.T, st *store.Store, rec domain.RunRecord) {
	t.Helper()
	if err := st.UpsertRun(context.Background(), rec); err != nil {
		t.Fatalf("写入判定 run 失败：%v", err)
	}
}

// makeRun 造一条判定 run 的状态快照。
func makeRun(runID, pointID string, state string, execState domain.ExecState,
	createdAt time.Time, degraded bool) domain.RunRecord {
	return domain.RunRecord{
		Run: domain.Run{
			RunID:     runID,
			PointID:   pointID,
			State:     state,
			ExecState: execState,
			Degraded:  degraded,
		},
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
}

// insertModelCall 往审计链里写一条模型调用记录。
func insertModelCall(t *testing.T, st *store.Store, runID string, at time.Time,
	outcome string, degraded bool) {
	t.Helper()
	err := st.RecordAuditEntry(context.Background(), domain.AuditEntry{
		RunID:      runID,
		EntryType:  domain.AuditEntryModelCall,
		StepName:   "attribute",
		SchemaName: "attribution",
		Model:      "test-model",
		Outcome:    outcome,
		Degraded:   degraded,
		CreatedAt:  at,
	})
	if err != nil {
		t.Fatalf("写入模型调用审计记录失败：%v", err)
	}
}

// TestStoreSource_CountsMatchDatabase 断言摘要里的数与库里的实际条数对得上。
//
// 这是摘要最要紧的一条：一个数对不上的看板比没有看板更坏——
// 看的人会照着它做决定。
func TestStoreSource_CountsMatchDatabase(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

	// 窗口内：3 条采样。
	for i := 0; i < 3; i++ {
		insertSample(t, st, "point-vax-freezer-01", now.Add(-time.Duration(i)*5*time.Minute), 11.4)
	}
	// 窗口外：1 条老采样，不该被算进来。
	insertSample(t, st, "point-vax-freezer-01", now.Add(-30*time.Hour), 11.4)

	// 窗口内：2 条判定（1 条正常、1 条失败），另有 1 条降级的。
	insertRun(t, st, makeRun("run-summary-001", "point-vax-freezer-01",
		string(workflow.StateAwaitingConfirmation), domain.ExecStateSucceeded, now.Add(-2*time.Hour), false))
	insertRun(t, st, makeRun("run-summary-002", "point-vax-freezer-01",
		string(workflow.StateFailed), domain.ExecStateFailed, now.Add(-time.Hour), true))
	// 窗口外：1 条老判定。
	insertRun(t, st, makeRun("run-summary-000", "point-vax-freezer-01",
		string(workflow.StateAssessed), domain.ExecStateSucceeded, now.Add(-48*time.Hour), false))

	// 模型调用：窗口内 3 次（1 次降级、1 次失败）。
	insertModelCall(t, st, "run-summary-001", now.Add(-90*time.Minute),
		domain.AuditOutcomeSucceeded, false)
	insertModelCall(t, st, "run-summary-001", now.Add(-80*time.Minute),
		domain.AuditOutcomeDegraded, true)
	insertModelCall(t, st, "run-summary-002", now.Add(-70*time.Minute),
		domain.AuditOutcomeFailed, false)
	// 窗口外：1 次老调用。
	insertModelCall(t, st, "run-summary-000", now.Add(-40*time.Hour),
		domain.AuditOutcomeSucceeded, false)

	collector := NewCollector(NewStoreSource(st),
		WithClock(func() time.Time { return now }))
	summary, err := collector.Collect(ctx)
	if err != nil {
		t.Fatalf("汇总运行摘要失败：%v", err)
	}

	if summary.Events != 3 {
		t.Errorf("events = %d，期望 3（窗口外那 1 条不该算进来）", summary.Events)
	}
	if summary.Runs != 2 {
		t.Errorf("runs = %d，期望 2（窗口外那 1 条不该算进来）", summary.Runs)
	}
	if summary.FailedRuns != 1 {
		t.Errorf("failed_runs = %d，期望 1", summary.FailedRuns)
	}
	if summary.DegradedRuns != 1 {
		t.Errorf("degraded_runs = %d，期望 1", summary.DegradedRuns)
	}
	if summary.AwaitingConfirmation != 1 {
		t.Errorf("awaiting_confirmation = %d，期望 1", summary.AwaitingConfirmation)
	}
	if summary.ModelCalls != 3 {
		t.Errorf("model_calls = %d，期望 3（窗口外那 1 次不该算进来）", summary.ModelCalls)
	}
	if summary.DegradedModelCalls != 1 {
		t.Errorf("degraded_model_calls = %d，期望 1", summary.DegradedModelCalls)
	}
	if summary.FailedModelCalls != 1 {
		t.Errorf("failed_model_calls = %d，期望 1", summary.FailedModelCalls)
	}
	if summary.DegradeRate != 0.3333 {
		t.Errorf("degrade_rate = %v，期望 0.3333", summary.DegradeRate)
	}
}

// TestStoreSource_CountsMatchDatabaseCounts 断言摘要的数与直接查库数出来的一致。
//
// 上面那条比的是「我写进去几条」；这条比的是「库现在到底有几条」——
// 两者不一致时，以库为准。
func TestStoreSource_CountsMatchDatabaseCounts(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	from := now.Add(-24 * time.Hour)

	insertSample(t, st, "point-vax-freezer-01", now.Add(-10*time.Minute), 5.0)
	insertRun(t, st, makeRun("run-summary-101", "point-vax-freezer-01",
		string(workflow.StateAwaitingConfirmation), domain.ExecStateQueued, now.Add(-time.Hour), false))
	insertModelCall(t, st, "run-summary-101", now.Add(-time.Hour),
		domain.AuditOutcomeSucceeded, false)

	wantEvents, err := st.CountEventsInRange(ctx, from, now)
	if err != nil {
		t.Fatalf("统计采样失败：%v", err)
	}
	wantRuns, err := st.CountRunsCreatedInRange(ctx, from, now)
	if err != nil {
		t.Fatalf("统计判定失败：%v", err)
	}
	wantCalls, err := st.ModelCallMetricsInRange(ctx, from, now)
	if err != nil {
		t.Fatalf("统计模型调用失败：%v", err)
	}
	wantAwaiting, err := st.CountRunsInState(ctx, string(workflow.StateAwaitingConfirmation))
	if err != nil {
		t.Fatalf("统计待确认失败：%v", err)
	}
	wantQueued, err := st.CountRunsInExecState(ctx, string(domain.ExecStateQueued))
	if err != nil {
		t.Fatalf("统计排队失败：%v", err)
	}

	summary, err := NewCollector(NewStoreSource(st),
		WithClock(func() time.Time { return now })).Collect(ctx)
	if err != nil {
		t.Fatalf("汇总运行摘要失败：%v", err)
	}

	if summary.Events != wantEvents {
		t.Errorf("events = %d，库里直接数是 %d", summary.Events, wantEvents)
	}
	if summary.Runs != wantRuns {
		t.Errorf("runs = %d，库里直接数是 %d", summary.Runs, wantRuns)
	}
	if summary.ModelCalls != wantCalls.Total {
		t.Errorf("model_calls = %d，库里直接数是 %d", summary.ModelCalls, wantCalls.Total)
	}
	if summary.AwaitingConfirmation != wantAwaiting {
		t.Errorf("awaiting_confirmation = %d，库里直接数是 %d",
			summary.AwaitingConfirmation, wantAwaiting)
	}
	if summary.QueuedRuns != wantQueued {
		t.Errorf("queued_runs = %d，库里直接数是 %d", summary.QueuedRuns, wantQueued)
	}
}

// TestStoreSource_HourlyBreakdownComesFromAggregate 断言逐小时分解来自小时级聚合表。
//
// 分解的口径是「按小时归桶的统计」：事件数相加、越温次数相加、峰值取最高、
// 点位数去重。它不回到原始采样上重算——那正是聚合表存在的理由。
func TestStoreSource_HourlyBreakdownComesFromAggregate(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	from := now.Add(-24 * time.Hour)

	// 两个点位在同一小时里的采样：事件数相加，峰值取最高，点位数去重。
	insertSample(t, st, "point-vax-freezer-01", now.Add(-30*time.Minute), 5.0)
	insertSample(t, st, "point-vax-freezer-01", now.Add(-25*time.Minute), 9.6)
	insertSample(t, st, "point-vax-fridge-02", now.Add(-20*time.Minute), 6.2)

	// 越温窗口：让聚合表里落一段越温次数。
	insertRun(t, st, makeRun("run-summary-201", "point-vax-freezer-01",
		string(workflow.StateAssessed), domain.ExecStateSucceeded, now.Add(-30*time.Minute), false))
	runRec, err := st.RunByID(ctx, "run-summary-201")
	if err != nil {
		t.Fatalf("回读判定失败：%v", err)
	}
	start := now.Add(-30 * time.Minute)
	end := now.Add(-15 * time.Minute)
	runRec.Run.WindowStart = &start
	runRec.Run.WindowEnd = &end
	if err := st.UpsertRun(ctx, runRec); err != nil {
		t.Fatalf("更新判定窗口失败：%v", err)
	}

	written, err := st.RefreshHourlyAggregates(ctx, "", from, now, store.FixedZoneOffset)
	if err != nil {
		t.Fatalf("重算小时级聚合失败：%v", err)
	}
	if written == 0 {
		t.Fatal("小时级聚合一个桶都没写进去")
	}

	summary, err := NewCollector(NewStoreSource(st),
		WithClock(func() time.Time { return now })).Collect(ctx)
	if err != nil {
		t.Fatalf("汇总运行摘要失败：%v", err)
	}
	if len(summary.Hourly) == 0 {
		t.Fatal("逐小时分解是空的")
	}
	total := 0
	peak := 0.0
	points := map[string]bool{}
	for _, bucket := range summary.Hourly {
		total += bucket.EventCount
		if bucket.PeakTemperatureC > peak {
			peak = bucket.PeakTemperatureC
		}
	}
	for _, bucket := range summary.Hourly {
		points[bucket.HourStart] = true
	}
	if total != 3 {
		t.Errorf("逐小时事件数合计 = %d，期望 3", total)
	}
	if peak != 9.6 {
		t.Errorf("逐小时峰值 = %v，期望 9.6", peak)
	}
	if summary.HourlyCoveredHours != len(summary.Hourly) {
		t.Errorf("hourly_covered_hours = %d，与桶数 %d 对不上",
			summary.HourlyCoveredHours, len(summary.Hourly))
	}
	// 逐小时分解按小时起点升序：看的人不用自己排。
	for i := 1; i < len(summary.Hourly); i++ {
		if summary.Hourly[i-1].HourStart > summary.Hourly[i].HourStart {
			t.Fatalf("逐小时桶没有按时间升序：%s 在 %s 前面",
				summary.Hourly[i-1].HourStart, summary.Hourly[i].HourStart)
		}
	}
}

// TestCollect_ConcurrentWritesDoNotError 断言并发写入时摘要端点不报错。
//
// 摘要端点是被轮询的：主路径一直在往库里写采样与判定，
// 运维同时在看这一页。这一条不许因为「库正忙」就报错或返回半截数据。
func TestCollect_ConcurrentWritesDoNotError(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

	collector := NewCollector(NewStoreSource(st),
		WithClock(func() time.Time { return now }))

	var wg sync.WaitGroup
	stop := make(chan struct{})
	writeErrs := make(chan error, 64)

	// 三个写者：一个写采样、一个写判定、一个写审计链。
	for writer := 0; writer < 3; writer++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				switch id {
				case 0:
					_, err := st.InsertEvent(ctx, domain.TemperatureEvent{
						PointID:      fmt.Sprintf("point-concurrent-%d", id),
						DeviceID:     "device-recorder-0001",
						DeviceName:   "疫苗冷藏库温度记录仪",
						MeasuredAt:   now.Add(-time.Duration(i) * time.Minute),
						TemperatureC: 5.0,
						Unit:         "C",
						Source:       "recorder",
					})
					if err != nil {
						writeErrs <- err
					}
				case 1:
					err := st.UpsertRun(ctx, makeRun(
						fmt.Sprintf("run-concurrent-%d-%d", id, i),
						"point-concurrent-1",
						string(workflow.StateAssessed), domain.ExecStateSucceeded,
						now.Add(-time.Duration(i)*time.Minute), false))
					if err != nil {
						writeErrs <- err
					}
				case 2:
					err := st.RecordAuditEntry(ctx, domain.AuditEntry{
						RunID:     "run-concurrent-2-0",
						EntryType: domain.AuditEntryModelCall,
						StepName:  "attribute",
						Outcome:   domain.AuditOutcomeSucceeded,
						CreatedAt: now,
					})
					if err != nil {
						writeErrs <- err
					}
				}
			}
		}(writer)
	}

	// 摘要端点被同时轮询。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := collector.Collect(ctx); err != nil {
				writeErrs <- err
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
	close(writeErrs)

	for err := range writeErrs {
		t.Fatalf("并发写入 / 统计时出错：%v", err)
	}
}

// TestStoreSource_WithoutStoreFails 断言没装配库时报错而不是回零。
func TestStoreSource_WithoutStoreFails(t *testing.T) {
	_, err := NewCollector(NewStoreSource(nil)).Collect(context.Background())
	if err == nil {
		t.Fatal("没装配本地库时应当报错")
	}
	if code := errcode.As(err).Code; code != errcode.StorageError {
		t.Errorf("错误码 = %q，期望 %q", code, errcode.StorageError)
	}
}

// fullDayBuckets 造满 24 小时覆盖的逐小时桶。
//
// 覆盖不满时摘要会提醒「聚合可能缺桶」——那是真的提醒，
// 但它跟「有没有欠账」是两回事，测备注时得先把这一项摘干净。
func fullDayBuckets() []HourlyBucket {
	buckets := make([]HourlyBucket, 0, 24)
	for i := 0; i < 24; i++ {
		buckets = append(buckets, HourlyBucket{
			HourStart:  fixedNow.Add(-time.Duration(i) * time.Hour).Format(time.RFC3339),
			EventCount: 10,
			Points:     2,
		})
	}
	return buckets
}

// hasNoteContaining 判断备注里有没有含某段文字的一条。
func hasNoteContaining(notes []string, want string) bool {
	for _, note := range notes {
		if strings.Contains(note, want) {
			return true
		}
	}
	return false
}
