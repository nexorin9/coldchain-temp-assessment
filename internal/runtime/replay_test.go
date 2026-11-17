package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/assessment"
	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// replaySamples 返回一段含一次越温的历史采样（5 分钟粒度）。
//
// 布局刻意做成「先在温区内 → 连续三条越限（峰值 12.4℃）→ 回到温区内」：
// 越温窗口是 03:10~03:20，持续 10 分钟，采样 3 条；前后那几条在温区内，
// 用来验证窗口的开与闭不会把结论带偏。
func replaySamples() []domain.TemperatureEvent {
	samples := []domain.TemperatureEvent{
		sample("2026-10-20T03:00:00+08:00", 5.0),
		sample("2026-10-20T03:05:00+08:00", 6.2),
		sample("2026-10-20T03:10:00+08:00", 9.6),
		sample("2026-10-20T03:15:00+08:00", 12.4),
		sample("2026-10-20T03:20:00+08:00", 11.1),
		sample("2026-10-20T03:25:00+08:00", 7.8),
		sample("2026-10-20T03:30:00+08:00", 5.5),
	}
	samples[2].Events = []string{domain.EventPowerLoss}
	samples[3].Events = []string{domain.EventPowerLoss}
	return samples
}

// replayRange 返回覆盖上面那段采样的回放区间（UTC）。
func replayRange() (time.Time, time.Time) {
	from, _ := time.Parse(time.RFC3339, "2026-10-19T19:00:00Z")
	to, _ := time.Parse(time.RFC3339, "2026-10-19T19:40:00Z")
	return from, to
}

// TestReplay_WindowMatchesOnlineConclusionFieldByField 是本任务的主场景：
// 同一批历史读数重跑一遍，越温窗口与当初在线判定的结论逐字段一致。
func TestReplay_WindowMatchesOnlineConclusionFieldByField(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	profile := *testProfile()
	samples := replaySamples()

	if _, err := st.InsertEvents(ctx, samples); err != nil {
		t.Fatalf("写入历史采样失败：%v", err)
	}
	// 先跑一遍在线判定，把结论落库——这就是回放要比对的基准。
	assessed, err := assessment.Assess(samples, &profile)
	if err != nil {
		t.Fatalf("在线判定失败：%v", err)
	}
	chain, err := NewChain(ChainDeps{Profile: &profile, Samples: samples})
	if err != nil {
		t.Fatalf("装配判定步骤链失败：%v", err)
	}
	exec := NewExecutor(chain, st, quietLogger()).WithRunPersistence(st, &profile)
	online, err := exec.Run(ctx, assessed.Run)
	if err != nil {
		t.Fatalf("在线判定步骤链失败：%v", err)
	}

	from, to := replayRange()
	replayer := NewReplayer(quietLogger())
	result, err := replayer.Replay(ctx, ReplayOptions{
		PointID: testPointID,
		From:    from,
		To:      to,
		Source:  st,
		Online:  st,
	})
	if err != nil {
		t.Fatalf("离线回放失败：%v", err)
	}

	if result.SampleCount != len(samples) {
		t.Fatalf("回放读到的采样条数不对：期望 %d，实际 %d", len(samples), result.SampleCount)
	}
	if result.ReplayedRunCount != 1 {
		t.Fatalf("回放重建出的判定条数不对：期望 1，实际 %d", result.ReplayedRunCount)
	}
	if result.Compared != 1 || result.Matched != 1 || result.Mismatched != 0 {
		t.Fatalf("比对计数不对：compared=%d matched=%d mismatched=%d",
			result.Compared, result.Matched, result.Mismatched)
	}

	entry := result.Windows[0]
	if !entry.Compared || !entry.Matched {
		t.Fatalf("窗口比对应一致：compared=%v matched=%v diffs=%+v", entry.Compared, entry.Matched, entry.Diffs)
	}
	if len(entry.Diffs) != 0 {
		t.Fatalf("逐字段比对不应有差异，实际有 %+v", entry.Diffs)
	}
	if entry.OnlineRunID != online.Run.RunID {
		t.Fatalf("回放没对上在线那条判定：期望 %s，实际 %s", online.Run.RunID, entry.OnlineRunID)
	}

	// 逐字段核对：起止、峰值、峰值时刻、持续分钟、温区、采样条数。
	onlineWindow, ok, err := st.StepPayloadByRunKind(ctx, online.Run.RunID, domain.PayloadWindow)
	if err != nil || !ok {
		t.Fatalf("取回在线窗口产物失败：ok=%v err=%v", ok, err)
	}
	expected := onlineWindow.(*domain.WindowPayload).Window
	got := entry.Window
	if !got.Start.Equal(expected.Start) || !got.End.Equal(expected.End) {
		t.Fatalf("窗口起止不一致：回放 %s~%s，在线 %s~%s",
			got.Start, got.End, expected.Start, expected.End)
	}
	if got.PeakC != expected.PeakC || !got.PeakAt.Equal(expected.PeakAt) {
		t.Fatalf("峰值不一致：回放 %.2f℃@%s，在线 %.2f℃@%s",
			got.PeakC, got.PeakAt, expected.PeakC, expected.PeakAt)
	}
	if got.DurationMinutes != expected.DurationMinutes {
		t.Fatalf("持续分钟不一致：回放 %d，在线 %d", got.DurationMinutes, expected.DurationMinutes)
	}
	if got.Zone != expected.Zone {
		t.Fatalf("温区不一致：回放 %s，在线 %s", got.Zone, expected.Zone)
	}
	if got.SampleCount != expected.SampleCount {
		t.Fatalf("采样条数不一致：回放 %d，在线 %d", got.SampleCount, expected.SampleCount)
	}
	// 业务键也要一致：外部引用与窗口幂等键是台账防重复入账的凭据。
	if entry.IdempotencyKey != online.Run.IdempotencyKey {
		t.Fatalf("窗口幂等键不一致：回放 %s，在线 %s", entry.IdempotencyKey, online.Run.IdempotencyKey)
	}
	if entry.ExternalRef != online.Run.ExternalRef {
		t.Fatalf("外部引用不一致：回放 %s，在线 %s", entry.ExternalRef, online.Run.ExternalRef)
	}
}

// TestReplay_DoesNotTouchOnlineRecords 钉住回放的隔离性：
// 回放不写任何一张表，在线判定记录一字不改。
func TestReplay_DoesNotTouchOnlineRecords(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	profile := *testProfile()
	samples := replaySamples()

	if _, err := st.InsertEvents(ctx, samples); err != nil {
		t.Fatalf("写入历史采样失败：%v", err)
	}
	assessed, err := assessment.Assess(samples, &profile)
	if err != nil {
		t.Fatalf("在线判定失败：%v", err)
	}
	chain, err := NewChain(ChainDeps{Profile: &profile, Samples: samples})
	if err != nil {
		t.Fatalf("装配判定步骤链失败：%v", err)
	}
	exec := NewExecutor(chain, st, quietLogger()).WithRunPersistence(st, &profile)
	online, err := exec.Run(ctx, assessed.Run)
	if err != nil {
		t.Fatalf("在线判定步骤链失败：%v", err)
	}

	runsBefore, err := st.CountRuns(ctx, testPointID)
	if err != nil {
		t.Fatalf("统计判定条数失败：%v", err)
	}
	eventsBefore, err := st.CountEvents(ctx, testPointID, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("统计采样条数失败：%v", err)
	}
	stepsBefore, err := st.CountRunSteps(ctx, online.Run.RunID)
	if err != nil {
		t.Fatalf("统计步骤条数失败：%v", err)
	}
	recordBefore, err := st.RunByID(ctx, online.Run.RunID)
	if err != nil {
		t.Fatalf("回读在线判定失败：%v", err)
	}

	from, to := replayRange()
	if _, err := NewReplayer(quietLogger()).Replay(ctx, ReplayOptions{
		PointID: testPointID,
		From:    from,
		To:      to,
		Source:  st,
		Online:  st,
	}); err != nil {
		t.Fatalf("离线回放失败：%v", err)
	}

	runsAfter, err := st.CountRuns(ctx, testPointID)
	if err != nil {
		t.Fatalf("统计判定条数失败：%v", err)
	}
	eventsAfter, err := st.CountEvents(ctx, testPointID, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("统计采样条数失败：%v", err)
	}
	stepsAfter, err := st.CountRunSteps(ctx, online.Run.RunID)
	if err != nil {
		t.Fatalf("统计步骤条数失败：%v", err)
	}
	if runsAfter != runsBefore {
		t.Fatalf("回放改写了判定记录：回放前 %d 条，回放后 %d 条", runsBefore, runsAfter)
	}
	if eventsAfter != eventsBefore {
		t.Fatalf("回放改写了原始采样：回放前 %d 条，回放后 %d 条", eventsBefore, eventsAfter)
	}
	if stepsAfter != stepsBefore {
		t.Fatalf("回放改写了步骤产物：回放前 %d 条，回放后 %d 条", stepsBefore, stepsAfter)
	}
	recordAfter, err := st.RunByID(ctx, online.Run.RunID)
	if err != nil {
		t.Fatalf("回读在线判定失败：%v", err)
	}
	if recordAfter.Run.State != recordBefore.Run.State ||
		recordAfter.Run.ExecState != recordBefore.Run.ExecState ||
		recordAfter.Run.ExternalRef != recordBefore.Run.ExternalRef ||
		recordAfter.WritebackState != recordBefore.WritebackState {
		t.Fatalf("在线判定记录被改动：回放前 %+v，回放后 %+v", recordBefore, recordAfter)
	}
}

// TestReplay_RunIDCarriesReplayMarker 钉住回放的独立标记：
// 重建出的 run 标识带 replay_ 前缀，与在线判定一眼可分、绝不重名。
func TestReplay_RunIDCarriesReplayMarker(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	profile := *testProfile()
	samples := replaySamples()

	if _, err := st.InsertEvents(ctx, samples); err != nil {
		t.Fatalf("写入历史采样失败：%v", err)
	}
	assessed, err := assessment.Assess(samples, &profile)
	if err != nil {
		t.Fatalf("在线判定失败：%v", err)
	}
	chain, err := NewChain(ChainDeps{Profile: &profile, Samples: samples})
	if err != nil {
		t.Fatalf("装配判定步骤链失败：%v", err)
	}
	exec := NewExecutor(chain, st, quietLogger()).WithRunPersistence(st, &profile)
	online, err := exec.Run(ctx, assessed.Run)
	if err != nil {
		t.Fatalf("在线判定步骤链失败：%v", err)
	}

	from, to := replayRange()
	result, err := NewReplayer(quietLogger()).Replay(ctx, ReplayOptions{
		PointID: testPointID,
		From:    from,
		To:      to,
		Source:  st,
		Online:  st,
	})
	if err != nil {
		t.Fatalf("离线回放失败：%v", err)
	}
	entry := result.Windows[0]
	if !strings.HasPrefix(entry.RunID, ReplayRunIDPrefix) {
		t.Fatalf("回放 run 标识没有独立标记：%s", entry.RunID)
	}
	if entry.RunID == online.Run.RunID {
		t.Fatalf("回放 run 标识与在线判定重名：%s", entry.RunID)
	}
	if result.Persisted {
		t.Fatal("回放结果不应落库")
	}
	if result.WritebackCalls != 0 {
		t.Fatalf("回放的台账写回调用数应为 0，实际 %d", result.WritebackCalls)
	}
}

// countingWriteback 是一条会被计数的台账写回替身。
//
// 它存在的意义：证明写回口是一条活的接缝（真的能写出去），
// 而不是一段没人调用的死代码——回放路径上写回调用数恒为 0，
// 靠的是「挂上就拒绝执行」，不是「恰好没人调」。
type countingWriteback struct {
	calls int
}

func (w *countingWriteback) WriteDeviation(_ context.Context, _ LedgerDeviation) error {
	w.calls++
	return nil
}

// TestReplay_WritebackAttachedRefusesExecution 钉住「回放不写回台账」：
// 挂上台账写回口的回放直接拒绝执行，一个字都不写出去。
func TestReplay_WritebackAttachedRefusesExecution(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	profile := *testProfile()
	samples := replaySamples()

	if _, err := st.InsertEvents(ctx, samples); err != nil {
		t.Fatalf("写入历史采样失败：%v", err)
	}
	from, to := replayRange()
	writeback := &countingWriteback{}
	replayer := NewReplayer(quietLogger()).WithWriteback(writeback)

	_, err := replayer.Replay(ctx, ReplayOptions{
		PointID: testPointID,
		From:    from,
		To:      to,
		Profile: &profile,
		Source:  st,
		Online:  st,
	})
	if err == nil {
		t.Fatal("挂上台账写回口的回放应当拒绝执行")
	}
	e := errcode.As(err)
	if e.Code != errcode.Validation {
		t.Fatalf("错误码不对：期望 %s，实际 %s", errcode.Validation, e.Code)
	}
	if !strings.Contains(e.Details, "回放路径不允许挂载台账写回口") {
		t.Fatalf("错误说明没点明回放写回纪律：%s", e.Details)
	}
	if writeback.calls != 0 {
		t.Fatalf("回放拒绝执行时不应写出任何台账记录，实际写出 %d 次", writeback.calls)
	}
}

// fakeOnlineStore 是在线结论的内存替身。
//
// 真实库里不容易造出「在线结论与回放不一致」的情形（在线结论就是那批数据算出来的），
// 要验证逐字段差异报告，就得能喂一份对不上的在线结论进来。
type fakeOnlineStore struct {
	samples  []domain.TemperatureEvent
	runs     []domain.RunRecord
	payloads map[string]any
}

// EventsInRange 按点位与区间回读采样（区间闭合，按时刻升序）。
func (f *fakeOnlineStore) EventsInRange(_ context.Context, pointID string, from, to time.Time) ([]domain.TemperatureEvent, error) {
	out := make([]domain.TemperatureEvent, 0, len(f.samples))
	for _, s := range f.samples {
		if s.PointID != pointID || s.MeasuredAt.Before(from) || s.MeasuredAt.After(to) {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// RunByIdempotencyKey 按窗口幂等键回读判定。
func (f *fakeOnlineStore) RunByIdempotencyKey(_ context.Context, key string) (domain.RunRecord, error) {
	for _, rec := range f.runs {
		if rec.Run.IdempotencyKey == key {
			return rec, nil
		}
	}
	return domain.RunRecord{}, errcode.Newf(errcode.NotFound, "按幂等键找不到判定 run：%s", key)
}

// RunsByPointInRange 返回点位在区间内的判定。
func (f *fakeOnlineStore) RunsByPointInRange(_ context.Context, pointID string, from, to time.Time) ([]domain.RunRecord, error) {
	out := make([]domain.RunRecord, 0, len(f.runs))
	for _, rec := range f.runs {
		if rec.Run.PointID != pointID {
			continue
		}
		if rec.Run.WindowStart == nil {
			continue
		}
		if !from.IsZero() && rec.Run.WindowStart.Before(from) {
			continue
		}
		if !to.IsZero() && rec.Run.WindowStart.After(to) {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// StepPayloadByRunKind 回读某分码的成功产物。
func (f *fakeOnlineStore) StepPayloadByRunKind(_ context.Context, runID string, kind domain.StepPayloadKind) (any, bool, error) {
	if kind != domain.PayloadWindow {
		return nil, false, nil
	}
	value, ok := f.payloads[runID]
	if !ok {
		return nil, false, nil
	}
	return value, true, nil
}

// TestReplay_MismatchReportedFieldByField 验证差异报告落到字段上：
// 「结论不一致」没法拿去问人，「峰值差 0.7℃、采样少 2 条」才问得下去。
func TestReplay_MismatchReportedFieldByField(t *testing.T) {
	ctx := context.Background()
	profile := *testProfile()
	samples := replaySamples()

	// 先算出当初在线判定的窗口。
	assessed, err := assessment.Assess(samples, &profile)
	if err != nil {
		t.Fatalf("在线判定失败：%v", err)
	}
	onlineWindow := assessed.Window

	// 造一份对不上的在线结论：峰值与采样条数都动了，起止与温区保持一致。
	doctored := *onlineWindow
	doctored.PeakC = 13.1
	doctored.SampleCount = 5

	fake := &fakeOnlineStore{
		samples: samples,
		runs: []domain.RunRecord{{
			Run: domain.Run{
				RunID:          "run_online_0001",
				PointID:        testPointID,
				State:          domain.RunStateAwaitingConfirmation,
				ExecState:      domain.ExecStateSucceeded,
				ExternalRef:    assessed.Run.ExternalRef,
				IdempotencyKey: assessed.Run.IdempotencyKey,
				WindowStart:    assessed.Run.WindowStart,
				WindowEnd:      assessed.Run.WindowEnd,
			},
			Profile:        &profile,
			WritebackState: domain.WritebackStateNone,
		}},
		payloads: map[string]any{
			"run_online_0001": &domain.WindowPayload{
				Window:     doctored,
				ZoneMinC:   profile.ZoneMinC,
				ZoneMaxC:   profile.ZoneMaxC,
				ZoneSource: "point_profile",
			},
		},
	}

	from, to := replayRange()
	result, err := NewReplayer(quietLogger()).Replay(ctx, ReplayOptions{
		PointID: testPointID,
		From:    from,
		To:      to,
		Profile: &profile,
		Source:  fake,
		Online:  fake,
	})
	if err != nil {
		t.Fatalf("离线回放失败：%v", err)
	}
	if result.Mismatched != 1 || result.Matched != 0 {
		t.Fatalf("应报出 1 处不一致：mismatched=%d matched=%d", result.Mismatched, result.Matched)
	}
	entry := result.Windows[0]
	if entry.Compared != true || entry.Matched {
		t.Fatalf("比对状态不对：compared=%v matched=%v", entry.Compared, entry.Matched)
	}
	fields := map[string]FieldDiff{}
	for _, diff := range entry.Diffs {
		fields[diff.Field] = diff
	}
	if _, ok := fields["peak_c"]; !ok {
		t.Fatalf("差异报告里没有 peak_c：%+v", entry.Diffs)
	}
	if _, ok := fields["sample_count"]; !ok {
		t.Fatalf("差异报告里没有 sample_count：%+v", entry.Diffs)
	}
	if fields["peak_c"].Replay != "12.40" || fields["peak_c"].Online != "13.10" {
		t.Fatalf("峰值差异取值不对：%+v", fields["peak_c"])
	}
	// 起止与温区没动过，不该被报成差异。
	if _, ok := fields["window_start"]; ok {
		t.Fatalf("窗口起点没变，不该报差异：%+v", entry.Diffs)
	}
	if _, ok := fields["zone"]; ok {
		t.Fatalf("温区没变，不该报差异：%+v", entry.Diffs)
	}
}

// TestReplay_OnlineRunWithoutSamplesReported 覆盖复盘时最该看的一类：
// 在线判过，但库里原始读数缺了，回放重建不出那一段。
func TestReplay_OnlineRunWithoutSamplesReported(t *testing.T) {
	ctx := context.Background()
	profile := *testProfile()
	samples := replaySamples()

	assessed, err := assessment.Assess(samples, &profile)
	if err != nil {
		t.Fatalf("在线判定失败：%v", err)
	}

	fake := &fakeOnlineStore{
		// 只留后半段读数：越温窗口的头一条读数缺失，窗口起点会往后挪，
		// 于是这次回放重建出的窗口与当初那条在线判定对不上号。
		samples: samples[3:],
		runs: []domain.RunRecord{{
			Run: domain.Run{
				RunID:          "run_online_0002",
				PointID:        testPointID,
				State:          domain.RunStateAwaitingConfirmation,
				ExecState:      domain.ExecStateSucceeded,
				ExternalRef:    assessed.Run.ExternalRef,
				IdempotencyKey: assessed.Run.IdempotencyKey,
				WindowStart:    assessed.Run.WindowStart,
				WindowEnd:      assessed.Run.WindowEnd,
			},
			Profile: &profile,
		}},
		payloads: map[string]any{},
	}

	from, to := replayRange()
	result, err := NewReplayer(quietLogger()).Replay(ctx, ReplayOptions{
		PointID: testPointID,
		From:    from,
		To:      to,
		Profile: &profile,
		Source:  fake,
		Online:  fake,
	})
	if err != nil {
		t.Fatalf("离线回放失败：%v", err)
	}
	if len(result.OnlineWithoutReplay) != 1 {
		t.Fatalf("应报出 1 条「在线有、回放重建不出」：%+v", result.OnlineWithoutReplay)
	}
	if result.OnlineWithoutReplay[0].RunID != "run_online_0002" {
		t.Fatalf("报出的在线判定不对：%+v", result.OnlineWithoutReplay)
	}
}

// TestReplay_ProfileSourceResolution 验证温区配置来源的两条口径：
// 显式指定就用指定的；没指定就用当初在线判定落库的那一份。
func TestReplay_ProfileSourceResolution(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	profile := *testProfile()
	samples := replaySamples()

	if _, err := st.InsertEvents(ctx, samples); err != nil {
		t.Fatalf("写入历史采样失败：%v", err)
	}
	assessed, err := assessment.Assess(samples, &profile)
	if err != nil {
		t.Fatalf("在线判定失败：%v", err)
	}
	chain, err := NewChain(ChainDeps{Profile: &profile, Samples: samples})
	if err != nil {
		t.Fatalf("装配判定步骤链失败：%v", err)
	}
	exec := NewExecutor(chain, st, quietLogger()).WithRunPersistence(st, &profile)
	if _, err := exec.Run(ctx, assessed.Run); err != nil {
		t.Fatalf("在线判定步骤链失败：%v", err)
	}

	from, to := replayRange()

	// 不给配置：应当取到当初在线判定落库的那一份。
	result, err := NewReplayer(quietLogger()).Replay(ctx, ReplayOptions{
		PointID: testPointID,
		From:    from,
		To:      to,
		Source:  st,
		Online:  st,
	})
	if err != nil {
		t.Fatalf("离线回放失败：%v", err)
	}
	if result.ProfileSource != ProfileSourceOnlineRun {
		t.Fatalf("温区配置来源不对：期望 %s，实际 %s", ProfileSourceOnlineRun, result.ProfileSource)
	}

	// 显式给配置：应当以给的为准，并在结果里说明来源。
	result, err = NewReplayer(quietLogger()).Replay(ctx, ReplayOptions{
		PointID: testPointID,
		From:    from,
		To:      to,
		Profile: &profile,
		Source:  st,
		Online:  st,
	})
	if err != nil {
		t.Fatalf("离线回放失败：%v", err)
	}
	if result.ProfileSource != ProfileSourceFile {
		t.Fatalf("温区配置来源不对：期望 %s，实际 %s", ProfileSourceFile, result.ProfileSource)
	}
}

// TestReplay_NoOnlineConclusionReportsUncompared 验证一种常见情形：
// 这段读数还没被判过（在线没有结论），回放照跑，但要如实说「无结论可比」。
func TestReplay_NoOnlineConclusionReportsUncompared(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	profile := *testProfile()
	samples := replaySamples()

	if _, err := st.InsertEvents(ctx, samples); err != nil {
		t.Fatalf("写入历史采样失败：%v", err)
	}
	from, to := replayRange()
	result, err := NewReplayer(quietLogger()).Replay(ctx, ReplayOptions{
		PointID: testPointID,
		From:    from,
		To:      to,
		Profile: &profile,
		Source:  st,
		Online:  st,
	})
	if err != nil {
		t.Fatalf("离线回放失败：%v", err)
	}
	if result.Compared != 0 || result.Uncompared != 1 {
		t.Fatalf("无在线结论时不应做比对：compared=%d uncompared=%d", result.Compared, result.Uncompared)
	}
	if result.Windows[0].CompareNote == "" {
		t.Fatal("无在线结论时应说明原因")
	}
}

// TestReplay_NoSamplesInRange 验证回放区间内没有读数时如实报错。
func TestReplay_NoSamplesInRange(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	profile := *testProfile()

	from, to := replayRange()
	_, err := NewReplayer(quietLogger()).Replay(ctx, ReplayOptions{
		PointID: testPointID,
		From:    from,
		To:      to,
		Profile: &profile,
		Source:  st,
		Online:  st,
	})
	if err == nil {
		t.Fatal("区间内没有采样时应报错")
	}
	e := errcode.As(err)
	if e.Code != errcode.NotFound {
		t.Fatalf("错误码不对：期望 %s，实际 %s", errcode.NotFound, e.Code)
	}
}

// TestReplay_InvalidArgumentsRejected 验证参数校验：点位缺失、区间倒置都要被挡下。
func TestReplay_InvalidArgumentsRejected(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	from, to := replayRange()

	cases := []struct {
		name string
		opts ReplayOptions
	}{
		{"缺点位", ReplayOptions{From: from, To: to, Source: st}},
		{"缺起止", ReplayOptions{PointID: testPointID, Source: st}},
		{"区间倒置", ReplayOptions{PointID: testPointID, From: to, To: from, Source: st}},
		{"缺来源", ReplayOptions{PointID: testPointID, From: from, To: to}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewReplayer(quietLogger()).Replay(ctx, tc.opts); err == nil {
				t.Fatalf("%s 应当被挡下", tc.name)
			}
		})
	}
}

// TestReplay_PointMismatchProfileRejected 验证点位与温区配置对不上时被挡下：
// 拿 A 冷库的温区去判 B 冷库的读数，结论从第一步就全错。
func TestReplay_PointMismatchProfileRejected(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	profile := *testProfile()
	profile.PointID = "point-other-fridge-02"
	from, to := replayRange()

	_, err := NewReplayer(quietLogger()).Replay(ctx, ReplayOptions{
		PointID: testPointID,
		From:    from,
		To:      to,
		Profile: &profile,
		Source:  st,
	})
	if err == nil {
		t.Fatal("点位与温区配置对不上时应当被挡下")
	}
	e := errcode.As(err)
	if !strings.Contains(e.Details, "点位不一致") {
		t.Fatalf("错误说明没点明点位不一致：%s", e.Details)
	}
}
