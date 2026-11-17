package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/store"
)

// timelineBase 是时间线测试用的时间基准。
var timelineBase = time.Date(2026, 10, 20, 3, 10, 0, 0, time.UTC)

// stepAt 造一条步骤记录：给定次序、名称、状态与起止偏移（毫秒）。
//
// 偏移是相对 timelineBase 的：测试读起来就是「第几步、跑了多久」，
// 不用每次都写一长串时刻。
func stepAt(seq int, name, state string, attempt int, startMS, durMS int64) domain.Step {
	started := timelineBase.Add(time.Duration(startMS) * time.Millisecond)
	ended := started.Add(time.Duration(durMS) * time.Millisecond)
	return domain.Step{
		StepName:  name,
		Seq:       seq,
		Attempt:   attempt,
		State:     state,
		StartedAt: started,
		EndedAt:   &ended,
	}
}

// fiveSteps 造一条跑完五步的判定链的步骤记录。
func fiveSteps() []domain.Step {
	return []domain.Step{
		stepAt(1, StepFetchSeries, domain.StepStateSucceeded, 1, 0, 120),
		stepAt(2, StepExtractWindow, domain.StepStateSucceeded, 1, 120, 80),
		stepAt(3, StepAttribute, domain.StepStateSucceeded, 1, 200, 950),
		stepAt(4, StepDraft, domain.StepStateSucceeded, 1, 1150, 300),
		stepAt(5, StepAwaitConfirmation, domain.StepStateSucceeded, 1, 1450, 30),
	}
}

// TestBuildTimeline_StepCountMatchesEntries 验证「步骤条数与时间线条目数一致」。
//
// 这是最容易被悄悄破坏的一条：续跑、重试会让库里多出记录，
// 但一条判定链在时间线上仍然是「每步一条（重试多几次就多几条尝试）」。
func TestBuildTimeline_StepCountMatchesEntries(t *testing.T) {
	steps := fiveSteps()
	view := BuildTimeline("run-x", steps)

	if view.StepCount != len(steps) {
		t.Fatalf("step_count 应为 %d，实际 %d", len(steps), view.StepCount)
	}
	if len(view.Entries) != len(steps) {
		t.Fatalf("时间线条目数应为 %d，实际 %d", len(steps), len(view.Entries))
	}
	for i, entry := range view.Entries {
		if entry.Seq != i+1 {
			t.Fatalf("第 %d 条的 seq 应为 %d，实际 %d", i+1, i+1, entry.Seq)
		}
		if entry.StepName == "" {
			t.Fatalf("第 %d 条缺步骤名", i+1)
		}
	}
}

// TestBuildTimeline_TotalDurationCoversEveryStep 验证「总耗时与各步耗时对得上」。
//
// 总跨度必须覆盖每一步的耗时：加起来比总跨度还长，说明时间线算错了。
func TestBuildTimeline_TotalDurationCoversEveryStep(t *testing.T) {
	steps := fiveSteps()
	view := BuildTimeline("run-x", steps)

	var sum int64
	for _, entry := range view.Entries {
		sum += entry.DurationMS
	}
	if view.TotalDurationMS < sum {
		t.Fatalf("总跨度 %dms 不应小于各步耗时之和 %dms", view.TotalDurationMS, sum)
	}
	if view.RunEndedAt == nil {
		t.Fatal("跑完的判定应有终点时刻")
	}
	span := view.RunEndedAt.Sub(view.RunStartedAt).Milliseconds()
	if span != view.TotalDurationMS {
		t.Fatalf("终点减起点应等于总跨度 %dms，实际 %dms", view.TotalDurationMS, span)
	}
	if !view.RunStartedAt.Equal(timelineBase) {
		t.Fatalf("起点应是最早一步的起点 %s，实际 %s", timelineBase, view.RunStartedAt)
	}
}

// TestBuildTimeline_PositionsMonotonicAndNonOverlapping 验证「位置单调不重叠」。
//
// 时间线是给人读的轴：两步重叠就没有先后可言。
// 断言两件事——后一条的起点不早于前一条的终点；百分比落在 0~100 之间。
func TestBuildTimeline_PositionsMonotonicAndNonOverlapping(t *testing.T) {
	view := BuildTimeline("run-x", fiveSteps())

	for i := 1; i < len(view.Entries); i++ {
		prev := view.Entries[i-1]
		cur := view.Entries[i]
		if cur.OffsetMS < prev.EndOffsetMS {
			t.Fatalf("第 %d 条起点 %dms 早于前一条终点 %dms，位置重叠", i+1, cur.OffsetMS, prev.EndOffsetMS)
		}
		if cur.OffsetMS < prev.OffsetMS {
			t.Fatalf("第 %d 条起点 %dms 早于前一条起点 %dms，位置非单调", i+1, cur.OffsetMS, prev.OffsetMS)
		}
	}
	for i, entry := range view.Entries {
		if entry.StartPct < 0 || entry.StartPct > 100 {
			t.Fatalf("第 %d 条起点百分比越界：%f", i+1, entry.StartPct)
		}
		if entry.WidthPct < 0 || entry.WidthPct > 100 {
			t.Fatalf("第 %d 条宽度百分比越界：%f", i+1, entry.WidthPct)
		}
		if entry.StartPct+entry.WidthPct > 100.01 {
			t.Fatalf("第 %d 条起点+宽度超出轨道：%f+%f", i+1, entry.StartPct, entry.WidthPct)
		}
	}
}

// TestBuildTimeline_OutOfOrderRecordsStillOrdered 验证「乱序落库的记录也能排成时间线」。
//
// 补投的旧采样可能晚于新采样落库，时间线不能因此乱掉：
// 条目按链上次序与尝试序号排，位置坐标仍然单调。
func TestBuildTimeline_OutOfOrderRecordsStillOrdered(t *testing.T) {
	steps := fiveSteps()
	// 故意倒序存放：库里回读的顺序不该决定时间线的顺序。
	reversed := make([]domain.Step, 0, len(steps))
	for i := len(steps) - 1; i >= 0; i-- {
		reversed = append(reversed, steps[i])
	}

	view := BuildTimeline("run-x", reversed)
	for i, entry := range view.Entries {
		if entry.Seq != i+1 {
			t.Fatalf("第 %d 条的 seq 应为 %d，实际 %d", i+1, i+1, entry.Seq)
		}
	}
	if view.CurrentStep == nil || view.CurrentStep.StepName != StepAwaitConfirmation {
		t.Fatalf("当前步应是待确认，实际 %+v", view.CurrentStep)
	}
}

// TestBuildTimeline_RetryAttemptVisible 验证「重试在时间线上看得见」。
//
// 失败的那次尝试照样占一条，带错误码与退避时长；
// 下一次尝试接着它的序号走——时间线要能看出「这是重试，不是重跑」。
func TestBuildTimeline_RetryAttemptVisible(t *testing.T) {
	failed := stepAt(3, StepAttribute, domain.StepStateFailed, 1, 200, 400)
	failed.ErrorCode = "E_MODEL_TIMEOUT"
	failed.Error = "模型网关超时"
	failed.BackoffMS = 1000
	succeeded := stepAt(3, StepAttribute, domain.StepStateSucceeded, 2, 1600, 250)

	view := BuildTimeline("run-x", []domain.Step{
		stepAt(1, StepFetchSeries, domain.StepStateSucceeded, 1, 0, 120),
		stepAt(2, StepExtractWindow, domain.StepStateSucceeded, 1, 120, 80),
		failed,
		succeeded,
		stepAt(4, StepDraft, domain.StepStateSucceeded, 1, 1850, 200),
	})

	if view.StepCount != 5 {
		t.Fatalf("5 条尝试应得 5 条时间线条目，实际 %d", view.StepCount)
	}
	if view.Entries[2].Attempt != 1 || view.Entries[2].State != domain.StepStateFailed {
		t.Fatalf("第 3 条应是失败的第一次尝试，实际 %+v", view.Entries[2])
	}
	if view.Entries[2].ErrorCode != "E_MODEL_TIMEOUT" {
		t.Fatalf("失败尝试应带错误码，实际 %q", view.Entries[2].ErrorCode)
	}
	if view.Entries[2].BackoffMS != 1000 {
		t.Fatalf("失败尝试应带回避时长 1000ms，实际 %d", view.Entries[2].BackoffMS)
	}
	if view.Entries[3].Attempt != 2 || view.Entries[3].State != domain.StepStateSucceeded {
		t.Fatalf("第 4 条应是成功的第二次尝试，实际 %+v", view.Entries[3])
	}
	// 失败那次与重试那次之间有一段退避等待：后一条起点不早于前一条终点。
	if view.Entries[3].OffsetMS < view.Entries[2].EndOffsetMS {
		t.Fatalf("重试起点 %dms 早于上次终点 %dms", view.Entries[3].OffsetMS, view.Entries[2].EndOffsetMS)
	}
	if len(view.RecentErrors) != 1 {
		t.Fatalf("最近失败应有 1 条，实际 %d", len(view.RecentErrors))
	}
	if view.RecentErrors[0].ErrorCode != "E_MODEL_TIMEOUT" {
		t.Fatalf("最近失败的错误码不符：%q", view.RecentErrors[0].ErrorCode)
	}
}

// TestBuildTimeline_CurrentStepIsWhereItStopped 验证「当前步就是它停住的地方」。
//
// 判定失败在归因那一步，时间线的当前步就该是归因——
// 现场问「它停在哪」，答案不该是「最后一条」。
func TestBuildTimeline_CurrentStepIsWhereItStopped(t *testing.T) {
	failed := stepAt(3, StepAttribute, domain.StepStateFailed, 1, 200, 400)
	failed.ErrorCode = "E_MODEL_UNAVAILABLE"
	failed.Error = "模型网关不可达"

	view := BuildTimeline("run-x", []domain.Step{
		stepAt(1, StepFetchSeries, domain.StepStateSucceeded, 1, 0, 120),
		stepAt(2, StepExtractWindow, domain.StepStateSucceeded, 1, 120, 80),
		failed,
	})

	if view.CurrentStep == nil {
		t.Fatal("必须有当前步")
	}
	if view.CurrentStep.StepName != StepAttribute {
		t.Fatalf("当前步应是归因，实际 %q", view.CurrentStep.StepName)
	}
	if view.CurrentStep.State != domain.StepStateFailed {
		t.Fatalf("当前步状态应是 failed，实际 %q", view.CurrentStep.State)
	}
	if !view.Entries[len(view.Entries)-1].IsCurrent {
		t.Fatal("最后一条应标为当前步")
	}
	if view.Entries[0].IsCurrent {
		t.Fatal("非最后一条不应标为当前步")
	}
}

// TestBuildTimeline_RecentErrorsMostRecentFirst 验证「最近失败按时间倒序且有上限」。
//
// 现场看的是最近的错，不是最早的错；条数有上限，是为了不让一条
// 反复重试的判定把整条时间线的注意力吃光。
func TestBuildTimeline_RecentErrorsMostRecentFirst(t *testing.T) {
	var steps []domain.Step
	for i := 1; i <= 8; i++ {
		failed := stepAt(i, StepAttribute, domain.StepStateFailed, 1, int64(i*100), 50)
		failed.ErrorCode = "E_MODEL_TIMEOUT"
		failed.Error = "模型网关超时"
		steps = append(steps, failed)
	}

	view := BuildTimeline("run-x", steps)
	if len(view.RecentErrors) != recentErrorLimit {
		t.Fatalf("最近失败应保留 %d 条，实际 %d", recentErrorLimit, len(view.RecentErrors))
	}
	for i := 1; i < len(view.RecentErrors); i++ {
		if view.RecentErrors[i].At.After(view.RecentErrors[i-1].At) {
			t.Fatalf("最近失败应按时间倒序，第 %d 条比前一条更晚", i+1)
		}
	}
}

// TestBuildTimeline_EmptySteps 验证「一条记录都没有时不编造时间线」。
func TestBuildTimeline_EmptySteps(t *testing.T) {
	view := BuildTimeline("run-x", nil)

	if view.StepCount != 0 || len(view.Entries) != 0 {
		t.Fatalf("空时间线不应有条目：%+v", view)
	}
	if view.CurrentStep != nil {
		t.Fatalf("空时间线不应有当前步：%+v", view.CurrentStep)
	}
	if view.TotalDurationMS != 0 {
		t.Fatalf("空时间线总跨度应为 0，实际 %d", view.TotalDurationMS)
	}
}

// TestBuildTimeline_SubMillisecondStepsKeepDistinctPositions 验证「毫秒以下的步骤仍有分辨力」。
//
// 判定链上多数步骤在毫秒级以下：一旦按毫秒算坐标，五步会全压在同一个点，
// 时间线就退化成一根柱子。坐标内部按微秒算，毫秒只是给人看的截断值。
func TestBuildTimeline_SubMillisecondStepsKeepDistinctPositions(t *testing.T) {
	steps := []domain.Step{
		stepAt(1, StepFetchSeries, domain.StepStateSucceeded, 1, 0, 0),
		stepAt(2, StepExtractWindow, domain.StepStateSucceeded, 1, 120, 0),
		stepAt(3, StepAttribute, domain.StepStateSucceeded, 1, 260, 0),
	}
	// 把毫秒偏移换成微秒级：同一步只隔几百微秒。
	for i := range steps {
		started := timelineBase.Add(time.Duration(i*400) * time.Microsecond)
		ended := started.Add(150 * time.Microsecond)
		steps[i].StartedAt = started
		steps[i].EndedAt = &ended
	}

	view := BuildTimeline("run-x", steps)
	for i := 1; i < len(view.Entries); i++ {
		if view.Entries[i].OffsetUS <= view.Entries[i-1].OffsetUS {
			t.Fatalf("第 %d 条的微秒偏移应大于前一条：%d vs %d",
				i+1, view.Entries[i].OffsetUS, view.Entries[i-1].OffsetUS)
		}
		if view.Entries[i].StartPct <= view.Entries[i-1].StartPct {
			t.Fatalf("第 %d 条的起点百分比应大于前一条：%v vs %v",
				i+1, view.Entries[i].StartPct, view.Entries[i-1].StartPct)
		}
	}
	if view.Entries[2].StartPct == 0 {
		t.Fatal("最后一步的起点百分比不该是 0（坐标在毫秒级以下也要分得开）")
	}
}

// TestBuildTimeline_ResumeAfterRestartKeepsOneEntryPerStep 验证「重启续跑后时间线不重复」。
//
// 这是本任务最要紧的一条：进程被杀后重启续跑，已完成的步骤不重跑，
// 时间线上每一步仍然只有一条——重复的步骤记录会让「跑了多久」变成假的。
func TestBuildTimeline_ResumeAfterRestartKeepsOneEntryPerStep(t *testing.T) {
	ctx := context.Background()
	path := dbPath(t, "timeline-resume")
	samples := excursionSamples()
	profile := testProfile()

	// 第一段：跑到成稿那一步时进程被杀。
	st := openFileStore(t, path)
	storeSamples(t, st, samples)
	counter := newStepCounter()
	chain := countingChain(t, ChainDeps{Profile: profile, Samples: samples}, counter, StepDraft)

	run := domain.NewRun(testPointID)
	run.BindWindow(samples[0].MeasuredAt, samples[len(samples)-1].MeasuredAt)

	exec := NewExecutor(chain, st, quietLogger()).WithRunPersistence(st, profile)
	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("模拟的崩溃没有发生，测试失去意义")
			}
		}()
		_, _ = exec.Run(ctx, run)
	}()

	// 「重启」：关掉库再打开，同一个文件。
	if err := st.Close(); err != nil {
		t.Fatalf("关闭库失败：%v", err)
	}
	restarted := openFileStore(t, path)

	rec, err := restarted.RunByID(ctx, run.RunID)
	if err != nil {
		t.Fatalf("重启后回读 run 失败：%v", err)
	}
	// 续跑用一条重新装配的链：崩掉的那一步这次要真的跑完。
	resumeChain, err := NewChain(ChainDeps{Profile: profile, Source: restarted})
	if err != nil {
		t.Fatalf("装配续跑链失败：%v", err)
	}
	plan, err := PlanResume(resumeChain, rec, mustRunSteps(t, restarted, run.RunID))
	if err != nil {
		t.Fatalf("规划续跑失败：%v", err)
	}
	if plan.Skipped() {
		t.Fatalf("这条 run 应该续跑，跳过理由却是 %q", plan.SkipReason)
	}
	resumeExec := NewExecutor(resumeChain, restarted, quietLogger()).WithRunPersistence(restarted, profile)
	if _, err := resumeExec.Resume(ctx, rec.Run, plan); err != nil {
		t.Fatalf("续跑失败：%v", err)
	}

	steps := mustRunSteps(t, restarted, run.RunID)
	view := BuildTimeline(run.RunID, steps)

	if view.StepCount != 5 {
		t.Fatalf("续跑后应有 5 条步骤记录，实际 %d 条", view.StepCount)
	}
	seen := map[string]int{}
	for _, entry := range view.Entries {
		seen[entry.StepName]++
	}
	for _, name := range []string{StepFetchSeries, StepExtractWindow, StepAttribute, StepDraft, StepAwaitConfirmation} {
		if seen[name] != 1 {
			t.Fatalf("步骤 %q 在时间线上应只出现 1 次，实际 %d 次", name, seen[name])
		}
	}
	// 续跑不重跑已完成步骤：前两步的处理器调用次数仍是 1。
	if counter.count(StepFetchSeries) != 1 || counter.count(StepExtractWindow) != 1 {
		t.Fatalf("续跑重跑了已完成的步骤：取数 %d 次、抽窗口 %d 次",
			counter.count(StepFetchSeries), counter.count(StepExtractWindow))
	}
	if view.CurrentStep == nil || view.CurrentStep.StepName != StepAwaitConfirmation {
		t.Fatalf("续跑完成后当前步应是待确认，实际 %+v", view.CurrentStep)
	}
}

// TestBuildTimeline_ResumeAfterRestartPositionStaysMonotonic 验证「续跑后位置仍单调」。
//
// 前半段来自崩掉的进程、后半段来自重启后的进程，两段拼起来仍是一条单调的轴。
func TestBuildTimeline_ResumeAfterRestartPositionStaysMonotonic(t *testing.T) {
	ctx := context.Background()
	path := dbPath(t, "timeline-resume-pos")
	samples := excursionSamples()
	profile := testProfile()

	st := openFileStore(t, path)
	storeSamples(t, st, samples)
	counter := newStepCounter()
	chain := countingChain(t, ChainDeps{Profile: profile, Samples: samples}, counter, StepDraft)

	run := domain.NewRun(testPointID)
	run.BindWindow(samples[0].MeasuredAt, samples[len(samples)-1].MeasuredAt)

	exec := NewExecutor(chain, st, quietLogger()).WithRunPersistence(st, profile)
	func() {
		defer func() { _ = recover() }()
		_, _ = exec.Run(ctx, run)
	}()
	if err := st.Close(); err != nil {
		t.Fatalf("关闭库失败：%v", err)
	}
	restarted := openFileStore(t, path)

	rec, err := restarted.RunByID(ctx, run.RunID)
	if err != nil {
		t.Fatalf("重启后回读 run 失败：%v", err)
	}
	resumeChain, err := NewChain(ChainDeps{Profile: profile, Source: restarted})
	if err != nil {
		t.Fatalf("装配续跑链失败：%v", err)
	}
	plan, err := PlanResume(resumeChain, rec, mustRunSteps(t, restarted, run.RunID))
	if err != nil {
		t.Fatalf("规划续跑失败：%v", err)
	}
	resumeExec := NewExecutor(resumeChain, restarted, quietLogger()).WithRunPersistence(restarted, profile)
	if _, err := resumeExec.Resume(ctx, rec.Run, plan); err != nil {
		t.Fatalf("续跑失败：%v", err)
	}

	view := BuildTimeline(run.RunID, mustRunSteps(t, restarted, run.RunID))
	for i := 1; i < len(view.Entries); i++ {
		if view.Entries[i].OffsetMS < view.Entries[i-1].EndOffsetMS {
			t.Fatalf("续跑后第 %d 条与前一条位置重叠", i+1)
		}
	}
}

// mustRunSteps 回读一条 run 的步骤记录，失败即测试失败。
func mustRunSteps(t *testing.T, st *store.Store, runID string) []domain.Step {
	t.Helper()
	steps, err := st.RunStepsByRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("回读步骤记录失败：%v", err)
	}
	return steps
}
