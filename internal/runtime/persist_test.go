package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/store"
)

// openFileStore 开一个文件库（不是内存库）。
//
// 续跑场景要模拟「进程重启」：内存库随进程一起没了，重启后什么也不剩，
// 测不出任何东西。文件库才能真的关掉再打开。
func openFileStore(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("打开文件库失败：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// dbPath 给一条本轮测试专用的库路径。
func dbPath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(t.TempDir(), name+".db")
}

// storeSamples 把采样写进库：续跑的取数步骤要能从库里回读到它们。
func storeSamples(t *testing.T, st *store.Store, samples []domain.TemperatureEvent) {
	t.Helper()
	if _, err := st.InsertEvents(context.Background(), samples); err != nil {
		t.Fatalf("写入采样失败：%v", err)
	}
}

// stepCounter 统计每一步被调用了几次。
//
// 「已完成的步骤不重跑」这条不变量靠它验证：续跑之后，
// 前两步的调用次数必须还是 1，不能变成 2。
type stepCounter struct {
	calls map[string]int
}

func newStepCounter() *stepCounter { return &stepCounter{calls: map[string]int{}} }

func (c *stepCounter) inc(name string) { c.calls[name]++ }

func (c *stepCounter) count(name string) int { return c.calls[name] }

// countingChain 装一条会记录每步调用次数的判定链。
//
// draftStep 指定「在哪一步模拟进程被杀」：传空串表示不模拟。
func countingChain(t *testing.T, deps ChainDeps, counter *stepCounter, crashAt string) []StepSpec {
	t.Helper()
	base, err := NewChain(deps)
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	out := make([]StepSpec, 0, len(base))
	for _, spec := range base {
		spec := spec
		original := spec.Handler
		spec.Handler = func(ctx context.Context, in StepInput) (any, error) {
			counter.inc(spec.Name)
			if spec.Name == crashAt {
				// 模拟进程在这一步中途被杀：没有任何收尾动作，
				// 没有失败落库，没有状态迁移——只有已经落库的前序步骤产物。
				panic("模拟进程在步骤 " + spec.Name + " 中途被杀")
			}
			return original(ctx, in)
		}
		out = append(out, spec)
	}
	return out
}

// TestPersist_CrashMidRunThenResume_NoDuplicateStepRecords 是本任务的主场景：
// 判定跑到一半时进程被杀，重启后接着跑，且不产生任何重复的步骤记录。
//
// 「杀进程」用 panic 模拟：panic 不会执行任何收尾动作，
// 库里留下的正是真实崩溃时留下的东西——前两步的产物与 running 状态的 run。
func TestPersist_CrashMidRunThenResume_NoDuplicateStepRecords(t *testing.T) {
	ctx := context.Background()
	path := dbPath(t, "crash")
	samples := excursionSamples()
	profile := testProfile()

	// 第一段：跑到成稿那一步时「进程被杀」。
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

	// 崩溃现场：成稿那一步之前的三步（取数、抽窗口、归因）已落库，run 停在执行中。
	steps, err := st.RunStepsByRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("回读步骤记录失败：%v", err)
	}
	if len(steps) != 3 {
		t.Fatalf("崩溃后应只留下前 3 步的产物，实际 %d 条", len(steps))
	}
	rec, err := st.RunByID(ctx, run.RunID)
	if err != nil {
		t.Fatalf("回读 run 失败：%v", err)
	}
	if rec.Run.ExecState != domain.ExecStateRunning {
		t.Fatalf("崩溃后执行态应停在 running，实际 %q", rec.Run.ExecState)
	}
	if rec.Profile == nil {
		t.Fatal("run 必须连同温区配置一起落库，否则重启后重建不了同一条判定链")
	}
	if rec.Profile.ZoneMinC != profile.ZoneMinC || rec.Profile.ZoneMaxC != profile.ZoneMaxC {
		t.Fatalf("落库的温区配置与当初不符：%v~%v", rec.Profile.ZoneMinC, rec.Profile.ZoneMaxC)
	}

	// 「重启」：关掉库再打开，同一个文件。
	if err := st.Close(); err != nil {
		t.Fatalf("关闭库失败：%v", err)
	}
	restarted := openFileStore(t, path)

	// 重启后扫出待续跑的 run，并按已有步骤产物算出续跑点。
	resumer := NewResumer(restarted, func(p *domain.PointProfile) (*Executor, error) {
		resumeChain, err := NewChain(ChainDeps{Profile: p, Source: restarted})
		if err != nil {
			return nil, err
		}
		return NewExecutor(resumeChain, restarted, quietLogger()), nil
	}, quietLogger())

	plans, err := resumer.Scan(ctx)
	if err != nil {
		t.Fatalf("续跑扫描失败：%v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("应扫出 1 条待续跑的 run，实际 %d 条", len(plans))
	}
	plan := plans[0]
	if plan.NextStep != StepDraft {
		t.Fatalf("续跑点应是 %q，实际 %q", StepDraft, plan.NextStep)
	}
	if len(plan.CompletedSteps) != 3 {
		t.Fatalf("应认出 3 个已完成步骤，实际 %d 个", len(plan.CompletedSteps))
	}

	outcome, err := resumer.Resume(ctx, plan)
	if err != nil {
		t.Fatalf("续跑失败：%v", err)
	}
	if outcome.Outcome != ResumeOutcomeResumed {
		t.Fatalf("续跑结果应为 resumed，实际 %q（%s）", outcome.Outcome, outcome.Reason)
	}

	// 关键断言：步骤记录没有重复。
	finalSteps, err := restarted.RunStepsByRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("回读步骤记录失败：%v", err)
	}
	if len(finalSteps) != 5 {
		t.Fatalf("续跑后应有 5 条步骤记录（每步一条），实际 %d 条：%+v", len(finalSteps), finalSteps)
	}
	seen := map[string]int{}
	for _, step := range finalSteps {
		seen[step.StepName]++
		if step.State != domain.StepStateSucceeded {
			t.Fatalf("步骤 %s 状态应为 succeeded，实际 %q", step.StepName, step.State)
		}
	}
	for name, count := range seen {
		if count != 1 {
			t.Fatalf("步骤 %s 有 %d 条记录，续跑产生了重复记录", name, count)
		}
	}

	// 前两步的处理器在续跑期间没有被再调用一次。
	if counter.count(StepFetchSeries) != 1 {
		t.Fatalf("取数步骤被调用了 %d 次，续跑不应重跑已完成的步骤", counter.count(StepFetchSeries))
	}
	if counter.count(StepExtractWindow) != 1 {
		t.Fatalf("抽窗口步骤被调用了 %d 次，续跑不应重跑已完成的步骤", counter.count(StepExtractWindow))
	}

	// 续跑后的 run 状态：执行态补成终态，业务状态停在待主管确认。
	finalRec, err := restarted.RunByID(ctx, run.RunID)
	if err != nil {
		t.Fatalf("回读 run 失败：%v", err)
	}
	if finalRec.Run.ExecState != domain.ExecStateSucceeded {
		t.Fatalf("续跑后执行态应为 succeeded，实际 %q", finalRec.Run.ExecState)
	}
	if finalRec.Run.State != domain.RunStateAwaitingConfirmation {
		t.Fatalf("续跑后业务状态应停在待主管确认，实际 %q", finalRec.Run.State)
	}

	// 已完成的产物没有因为续跑被改写：窗口峰值仍是当初那一批采样算出来的 12.4℃。
	window, ok := ProductAs[domain.WindowPayload](finalSteps, domain.PayloadWindow)
	if !ok {
		t.Fatal("续跑后窗口产物应仍能按分码取回")
	}
	if window.Window.PeakC != 12.4 {
		t.Fatalf("续跑后窗口峰值应为 12.4，实际 %v", window.Window.PeakC)
	}
}

// TestPersist_WrittenBackRunNotResumedAfterRestart 验证「不重复写回」：
// 台账已经写成的 run，重启后既不会被扫成待续跑，也不会被再写一次台账。
//
// 这是真实会发生的场景：写回成功、进程在把状态更新回去之前被杀。
// 现场看到的就是「台账里有一条，库里状态还停在老地方」——
// 这时候再写一次，台账里就会出现两条同一次越温的记录。
func TestPersist_WrittenBackRunNotResumedAfterRestart(t *testing.T) {
	ctx := context.Background()
	path := dbPath(t, "written-back")
	samples := excursionSamples()
	profile := testProfile()

	st := openFileStore(t, path)
	storeSamples(t, st, samples)

	chain, err := NewChain(ChainDeps{Profile: profile, Samples: samples})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	exec := NewExecutor(chain, st, quietLogger()).WithRunPersistence(st, profile)

	run := domain.NewRun(testPointID)
	run.BindWindow(samples[0].MeasuredAt, samples[len(samples)-1].MeasuredAt)
	if _, err := exec.Run(ctx, run); err != nil {
		t.Fatalf("判定失败：%v", err)
	}

	// 台账写入落成（这一动作由出站写回器在台账返回成功后触发）。
	if err := st.MarkRunWrittenBack(ctx, run.RunID, time.Now()); err != nil {
		t.Fatalf("标记写回状态失败：%v", err)
	}
	state, err := st.RunWritebackState(ctx, run.RunID)
	if err != nil {
		t.Fatalf("读回写回状态失败：%v", err)
	}
	if state != domain.WritebackStateWrittenBack {
		t.Fatalf("写回状态应为 %s，实际 %q", domain.WritebackStateWrittenBack, state)
	}

	// 「重启」后扫描：已写回的 run 不该出现在待续跑列表里。
	if err := st.Close(); err != nil {
		t.Fatalf("关闭库失败：%v", err)
	}
	restarted := openFileStore(t, path)

	resumable, err := restarted.ResumableRuns(ctx)
	if err != nil {
		t.Fatalf("扫描待续跑判定失败：%v", err)
	}
	for _, rec := range resumable {
		if rec.Run.RunID == run.RunID {
			t.Fatal("已写回台账的 run 不该被扫成待续跑")
		}
	}

	// 即使有人硬把这条 run 塞进续跑计划，续跑器也必须拒绝它。
	steps, err := restarted.RunStepsByRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("回读步骤记录失败：%v", err)
	}
	rec, err := restarted.RunByID(ctx, run.RunID)
	if err != nil {
		t.Fatalf("回读 run 失败：%v", err)
	}
	plan, err := PlanResume(chain, rec, steps)
	if err != nil {
		t.Fatalf("规划续跑失败：%v", err)
	}
	if !plan.Skipped() {
		t.Fatal("已写回台账的 run 不该被续跑")
	}
	if plan.SkipReason != SkipReasonWrittenBack {
		t.Fatalf("跳过理由应为 %q，实际 %q", SkipReasonWrittenBack, plan.SkipReason)
	}

	// 续跑器实际执行时也跳过它：一条台账写回记录都不会多。
	resumer := NewResumer(restarted, func(p *domain.PointProfile) (*Executor, error) {
		resumeChain, err := NewChain(ChainDeps{Profile: p, Source: restarted})
		if err != nil {
			return nil, err
		}
		return NewExecutor(resumeChain, restarted, quietLogger()), nil
	}, quietLogger())
	outcome, err := resumer.Resume(ctx, plan)
	if err != nil {
		t.Fatalf("续跑（应跳过）返回错误：%v", err)
	}
	if outcome.Outcome != ResumeOutcomeSkipped {
		t.Fatalf("续跑结果应为 skipped，实际 %q", outcome.Outcome)
	}
}

// TestPersist_ResumeAllSkipsWrittenBackAndResumesPending 验证一次启动扫描里
// 两种 run 各走各的路：已写回的跳过，没跑完的接着跑。
func TestPersist_ResumeAllSkipsWrittenBackAndResumesPending(t *testing.T) {
	ctx := context.Background()
	path := dbPath(t, "mixed")
	samples := excursionSamples()
	profile := testProfile()

	st := openFileStore(t, path)
	storeSamples(t, st, samples)

	// 一条跑完并已写回台账的 run。
	doneChain, err := NewChain(ChainDeps{Profile: profile, Samples: samples})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	doneExec := NewExecutor(doneChain, st, quietLogger()).WithRunPersistence(st, profile)
	doneRun := domain.NewRun(testPointID)
	doneRun.BindWindow(samples[0].MeasuredAt, samples[len(samples)-1].MeasuredAt)
	if _, err := doneExec.Run(ctx, doneRun); err != nil {
		t.Fatalf("判定失败：%v", err)
	}
	if err := st.MarkRunWrittenBack(ctx, doneRun.RunID, time.Now()); err != nil {
		t.Fatalf("标记写回状态失败：%v", err)
	}

	// 一条停在执行中的 run：模拟进程被杀在这里。
	pendingChain, err := NewChain(ChainDeps{
		Profile: profile,
		Samples: samples,
		Draft: func(ctx context.Context, in StepInput) (any, error) {
			return nil, errcode.Newf(errcode.Validation, "模拟成稿步骤失败")
		},
	})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	pendingExec := NewExecutor(pendingChain, st, quietLogger()).WithRunPersistence(st, profile)
	pendingRun := domain.NewRun(testPointID)
	pendingRun.BindWindow(samples[0].MeasuredAt, samples[len(samples)-1].MeasuredAt)
	if _, err := pendingExec.Run(ctx, pendingRun); err == nil {
		t.Fatal("成稿步骤失败时整条链应报错")
	}

	// 把这条 run 的执行态改回「执行中」——真实崩溃时就是这个状态。
	rec, err := st.RunByID(ctx, pendingRun.RunID)
	if err != nil {
		t.Fatalf("回读 run 失败：%v", err)
	}
	rec.Run.ExecState = domain.ExecStateRunning
	if err := st.UpsertRun(ctx, rec); err != nil {
		t.Fatalf("更新 run 状态失败：%v", err)
	}

	if err := st.Close(); err != nil {
		t.Fatalf("关闭库失败：%v", err)
	}
	restarted := openFileStore(t, path)

	resumer := NewResumer(restarted, func(p *domain.PointProfile) (*Executor, error) {
		resumeChain, err := NewChain(ChainDeps{Profile: p, Source: restarted})
		if err != nil {
			return nil, err
		}
		return NewExecutor(resumeChain, restarted, quietLogger()), nil
	}, quietLogger())

	summary, err := resumer.ResumeAll(ctx)
	if err != nil {
		t.Fatalf("续跑扫描与执行失败：%v", err)
	}
	if summary.Scanned != 1 {
		t.Fatalf("应只扫出 1 条待续跑的 run，实际 %d 条", summary.Scanned)
	}
	if summary.Resumed != 1 {
		t.Fatalf("应续跑完成 1 条，实际 %d 条（跳过 %d、失败 %d）",
			summary.Resumed, summary.Skipped, summary.Failed)
	}

	// 已写回的那条不受影响：写回标记还在，判定结论没被动过。
	doneRec, err := restarted.RunByID(ctx, doneRun.RunID)
	if err != nil {
		t.Fatalf("回读 run 失败：%v", err)
	}
	if doneRec.WritebackState != domain.WritebackStateWrittenBack {
		t.Fatalf("已写回的 run 写回标记被改动，实际 %q", doneRec.WritebackState)
	}
}

// TestPlanResume_ComputesResumePointFromStepProducts 验证续跑点的计算口径：
// 续跑点是链上第一个没有成功产物的步骤。
func TestPlanResume_ComputesResumePointFromStepProducts(t *testing.T) {
	chain, err := NewChain(ChainDeps{Profile: testProfile()})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	rec := domain.RunRecord{
		Run:     domain.Run{RunID: "run-1", PointID: testPointID, ExecState: domain.ExecStateRunning},
		Profile: testProfile(),
	}

	// 没有任何产物：续跑点就是第一步。
	plan, err := PlanResume(chain, rec, nil)
	if err != nil {
		t.Fatalf("规划续跑失败：%v", err)
	}
	if plan.NextStep != StepFetchSeries || plan.NextSeq != 1 {
		t.Fatalf("续跑点应是第一步，实际 %q（seq=%d）", plan.NextStep, plan.NextSeq)
	}

	// 前两步有成功产物：续跑点后移到第三步。
	steps := []domain.Step{
		{StepName: StepFetchSeries, Seq: 1, State: domain.StepStateSucceeded},
		{StepName: StepExtractWindow, Seq: 2, State: domain.StepStateSucceeded},
	}
	plan, err = PlanResume(chain, rec, steps)
	if err != nil {
		t.Fatalf("规划续跑失败：%v", err)
	}
	if plan.NextStep != StepAttribute || plan.NextSeq != 3 {
		t.Fatalf("续跑点应是第三步，实际 %q（seq=%d）", plan.NextStep, plan.NextSeq)
	}

	// 全部步骤都有产物：只差把执行态补成终态。
	for i, name := range ChainStepNames() {
		steps = append(steps, domain.Step{StepName: name, Seq: i + 1, State: domain.StepStateSucceeded})
	}
	plan, err = PlanResume(chain, rec, steps)
	if err != nil {
		t.Fatalf("规划续跑失败：%v", err)
	}
	if plan.NextStep != "" {
		t.Fatalf("全部完成时不应有续跑点，实际 %q", plan.NextStep)
	}
	if plan.SkipReason != SkipReasonAllStepsDone {
		t.Fatalf("跳过理由应为 %q，实际 %q", SkipReasonAllStepsDone, plan.SkipReason)
	}
}

// TestPlanResume_RejectsInconsistentStepOrder 验证一致性检查：
// 前面的步骤没跑成、后面的步骤却有成功记录时，不续跑。
//
// 这种数据说明步骤记录被改过或写错了顺序。按错误的口径续跑，
// 出来的结论没有人能复核——宁可不跑。
func TestPlanResume_RejectsInconsistentStepOrder(t *testing.T) {
	chain, err := NewChain(ChainDeps{Profile: testProfile()})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	rec := domain.RunRecord{
		Run:     domain.Run{RunID: "run-2", PointID: testPointID, ExecState: domain.ExecStateRunning},
		Profile: testProfile(),
	}
	steps := []domain.Step{
		{StepName: StepExtractWindow, Seq: 2, State: domain.StepStateSucceeded},
		{StepName: StepAttribute, Seq: 3, State: domain.StepStateSucceeded},
	}
	if _, err := PlanResume(chain, rec, steps); err == nil {
		t.Fatal("步骤记录与链上次序不一致时不应给出续跑计划")
	} else if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.Validation, code)
	}
}

// TestPlanResume_MissingProfileIsSkipped 验证「认不出依据就不续跑」：
// 没有温区配置的 run 不续跑，也不拿默认值顶替。
func TestPlanResume_MissingProfileIsSkipped(t *testing.T) {
	chain, err := NewChain(ChainDeps{Profile: testProfile()})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	rec := domain.RunRecord{
		Run:     domain.Run{RunID: "run-3", PointID: testPointID, ExecState: domain.ExecStateRunning},
		Profile: nil,
	}
	plan, err := PlanResume(chain, rec, nil)
	if err != nil {
		t.Fatalf("规划续跑失败：%v", err)
	}
	if plan.SkipReason != SkipReasonNoProfile {
		t.Fatalf("跳过理由应为 %q，实际 %q", SkipReasonNoProfile, plan.SkipReason)
	}
	if plan.NextStep != "" {
		t.Fatalf("没有温区配置时不应给出续跑点，实际 %q", plan.NextStep)
	}
}

// TestPersist_RunStatePersistedOnEachStep 验证「随每步推进实时落库」：
// 每推进一步，runs 表里那条 run 的状态就跟着更新一次。
func TestPersist_RunStatePersistedOnEachStep(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	samples := excursionSamples()
	profile := testProfile()

	chain, err := NewChain(ChainDeps{Profile: profile, Samples: samples})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	exec := NewExecutor(chain, st, quietLogger()).WithRunPersistence(st, profile)

	run := domain.NewRun(testPointID)
	run.BindWindow(samples[0].MeasuredAt, samples[len(samples)-1].MeasuredAt)
	result, err := exec.Run(ctx, run)
	if err != nil {
		t.Fatalf("判定失败：%v", err)
	}

	rec, err := st.RunByID(ctx, run.RunID)
	if err != nil {
		t.Fatalf("回读 run 失败：%v", err)
	}
	if rec.Run.ExecState != domain.ExecStateSucceeded {
		t.Fatalf("落库的执行态应为 succeeded，实际 %q", rec.Run.ExecState)
	}
	if rec.Run.State != result.Run.State {
		t.Fatalf("落库的业务状态应与执行结果一致：%q ≠ %q", rec.Run.State, result.Run.State)
	}
	if rec.Run.ExternalRef != run.ExternalRef {
		t.Fatalf("落库的 external_ref 应与 run 一致：%q ≠ %q", rec.Run.ExternalRef, run.ExternalRef)
	}
	if rec.CreatedAt.IsZero() || rec.UpdatedAt.IsZero() {
		t.Fatal("run 落库后应有创建时刻与更新时刻")
	}

	// 反复落库不产生第二行：更新的是同一行。
	for i := 0; i < 3; i++ {
		if err := st.UpsertRun(ctx, rec); err != nil {
			t.Fatalf("第 %d 次落库失败：%v", i+1, err)
		}
	}
	count, err := st.CountRuns(ctx, "")
	if err != nil {
		t.Fatalf("统计 run 条数失败：%v", err)
	}
	if count != 1 {
		t.Fatalf("反复落库后 run 表应只有 1 行，实际 %d 行", count)
	}
}

// TestPersist_RunWritebackMarkerSurvivesStateUpdates 验证写回标记不被状态更新抹掉：
// 执行器每次推进都写整条快照，若快照里没有写回状态，
// 「已写回」这个标记就会被悄悄抹掉——重启后就会重复写台账。
func TestPersist_RunWritebackMarkerSurvivesStateUpdates(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	rec := domain.RunRecord{
		Run: domain.Run{
			RunID:     "run-4",
			PointID:   testPointID,
			State:     domain.RunStateAwaitingConfirmation,
			ExecState: domain.ExecStateSucceeded,
		},
	}
	if err := st.UpsertRun(ctx, rec); err != nil {
		t.Fatalf("写入 run 失败：%v", err)
	}
	if err := st.MarkRunWrittenBack(ctx, "run-4", time.Now()); err != nil {
		t.Fatalf("标记写回状态失败：%v", err)
	}

	// 模拟执行器推进时写的整条快照（不带写回状态）。
	stored, err := st.RunByID(ctx, "run-4")
	if err != nil {
		t.Fatalf("回读 run 失败：%v", err)
	}
	stored.Run.State = domain.RunStateAwaitingConfirmation
	stored.WritebackState = ""
	if err := st.UpsertRun(ctx, stored); err != nil {
		t.Fatalf("更新 run 失败：%v", err)
	}

	after, err := st.RunByID(ctx, "run-4")
	if err != nil {
		t.Fatalf("回读 run 失败：%v", err)
	}
	if after.WritebackState != domain.WritebackStateWrittenBack {
		t.Fatalf("写回标记被状态更新抹掉了，实际 %q", after.WritebackState)
	}
	if after.WrittenBackAt == nil {
		t.Fatal("写回时刻不应被状态更新抹掉")
	}
}

// TestPersist_RunByIDMissingReturnsNotFound 验证查不到就说查不到：
// run 不存在时返回 E_NOT_FOUND，不回一个空壳。
func TestPersist_RunByIDMissingReturnsNotFound(t *testing.T) {
	st := openStore(t)
	_, err := st.RunByID(context.Background(), "run-does-not-exist")
	if err == nil {
		t.Fatal("run 不存在时应报错")
	}
	if code := errcode.As(err).Code; code != errcode.NotFound {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.NotFound, code)
	}
}

// TestPersist_ResumableRunsOrderingAndFilter 验证待续跑列表的筛选与排序：
// 终态与已写回的都被排除，其余按停住时间升序。
func TestPersist_ResumableRunsOrderingAndFilter(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)

	mk := func(runID string, execState domain.ExecState, writeback string, updated time.Time) {
		t.Helper()
		rec := domain.RunRecord{
			Run: domain.Run{
				RunID:     runID,
				PointID:   testPointID,
				State:     domain.RunStateDetected,
				ExecState: execState,
			},
			WritebackState: writeback,
			UpdatedAt:      updated,
		}
		if err := st.UpsertRun(ctx, rec); err != nil {
			t.Fatalf("写入 run 失败：%v", err)
		}
	}

	base := time.Now().Add(-time.Hour)
	mk("run-late", domain.ExecStateRunning, "", base.Add(10*time.Minute))
	mk("run-early", domain.ExecStateRunning, "", base)
	mk("run-done", domain.ExecStateSucceeded, "", base)
	mk("run-failed", domain.ExecStateFailed, "", base)
	mk("run-written", domain.ExecStateRunning, domain.WritebackStateWrittenBack, base)

	resumable, err := st.ResumableRuns(ctx)
	if err != nil {
		t.Fatalf("扫描待续跑判定失败：%v", err)
	}
	if len(resumable) != 2 {
		t.Fatalf("应扫出 2 条待续跑的 run，实际 %d 条", len(resumable))
	}
	if resumable[0].Run.RunID != "run-early" {
		t.Fatalf("待续跑列表应按停住时间升序，首条应是 run-early，实际 %q", resumable[0].Run.RunID)
	}
	for _, rec := range resumable {
		if rec.Run.RunID == "run-written" {
			t.Fatal("已写回台账的 run 不该出现在待续跑列表里")
		}
	}
}

// TestPersist_ResumeAfterCrashKeepsAttemptNumbering 验证续跑时的尝试序号接着走：
// 上次 attempt=1 失败，重启后这一轮是 attempt=2。
//
// 序号从头开始会让时间线看起来像「第一次尝试」，
// 复核的人就看不出这已经是第二次了。
func TestPersist_ResumeAfterCrashKeepsAttemptNumbering(t *testing.T) {
	ctx := context.Background()
	path := dbPath(t, "attempt")
	samples := excursionSamples()
	profile := testProfile()

	st := openFileStore(t, path)
	storeSamples(t, st, samples)

	// 归因步骤第一次失败（可重试错误 → 试满 3 次后整条链失败）。
	failing := func(ctx context.Context, in StepInput) (any, error) {
		return nil, errcode.Newf(errcode.ModelUnavailable, "模型服务不可达")
	}
	chain, err := NewChain(ChainDeps{Profile: profile, Samples: samples, Attribute: failing})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	exec := NewExecutor(chain, st, quietLogger()).WithRunPersistence(st, profile).
		WithWaitFunc((&waitRecorder{}).wait)

	run := domain.NewRun(testPointID)
	run.BindWindow(samples[0].MeasuredAt, samples[len(samples)-1].MeasuredAt)
	if _, err := exec.Run(ctx, run); err == nil {
		t.Fatal("归因步骤一直失败时整条链应报错")
	}

	steps, err := st.RunStepsByRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("回读步骤记录失败：%v", err)
	}
	attempts := 0
	for _, step := range steps {
		if step.StepName == StepAttribute {
			attempts = step.Attempt
		}
	}
	if attempts != 3 {
		t.Fatalf("失败步骤应留下 3 次尝试，实际 %d 次", attempts)
	}

	// 把执行态改回执行中（真实崩溃时就是这个状态），然后续跑。
	rec, err := st.RunByID(ctx, run.RunID)
	if err != nil {
		t.Fatalf("回读 run 失败：%v", err)
	}
	rec.Run.ExecState = domain.ExecStateRunning
	if err := st.UpsertRun(ctx, rec); err != nil {
		t.Fatalf("更新 run 状态失败：%v", err)
	}

	okChain, err := NewChain(ChainDeps{Profile: profile, Source: st})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	plan, err := PlanResume(okChain, rec, steps)
	if err != nil {
		t.Fatalf("规划续跑失败：%v", err)
	}
	if plan.NextStep != StepAttribute {
		t.Fatalf("续跑点应是归因步骤，实际 %q", plan.NextStep)
	}

	resumeExec := NewExecutor(okChain, st, quietLogger()).WithRunPersistence(st, profile)
	if _, err := resumeExec.Resume(ctx, rec.Run, plan); err != nil {
		t.Fatalf("续跑失败：%v", err)
	}

	after, err := st.RunStepsByRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("回读步骤记录失败：%v", err)
	}
	sawAttempt4 := false
	for _, step := range after {
		if step.StepName == StepAttribute && step.Attempt == 4 && step.State == domain.StepStateSucceeded {
			sawAttempt4 = true
		}
	}
	if !sawAttempt4 {
		t.Fatalf("续跑后的归因步骤应是第 4 次尝试且成功，实际记录：%+v", after)
	}
}

// 编译期确认存储实现同时满足步骤产物口与 run 状态口。
var (
	_ RunRecorder = (*store.Store)(nil)
	_ RunStore    = (*store.Store)(nil)
)
