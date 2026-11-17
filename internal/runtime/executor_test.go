package runtime

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
	"github.com/chaosforge/coldchain-temp-assessment/internal/store"
)

// excursionSamples 返回一段越温采样：03:10 起连续三条越限（峰值 12.4℃），
// 其中前两条带断电事件——归因要认的就是这个现场证据。
func excursionSamples() []domain.TemperatureEvent {
	samples := []domain.TemperatureEvent{
		sample("2026-10-20T03:10:00+08:00", 9.6),
		sample("2026-10-20T03:15:00+08:00", 12.4),
		sample("2026-10-20T03:20:00+08:00", 11.1),
	}
	samples[0].Events = []string{domain.EventPowerLoss}
	samples[1].Events = []string{domain.EventPowerLoss}
	return samples
}

// directRun 造一条不经调度器的判定 run：直接建 run 并绑定窗口。
//
// 它存在的意义是证明执行器不依赖调度器：给一条 run，执行器就能按链子跑。
func directRun(t *testing.T, samples []domain.TemperatureEvent) domain.Run {
	t.Helper()
	run := domain.NewRun(testPointID)
	run.BindWindow(samples[0].MeasuredAt, samples[len(samples)-1].MeasuredAt)
	return run
}

// quietLogger 返回写丢弃口的日志器，避免测试输出被 JSON 日志刷屏。
func quietLogger() *logging.Logger { return logging.New(io.Discard) }

// openStore 开一个内存库；测试用完即弃。
func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestExecutor_FiveStepsAllGreen 是本任务的主场景：
// 五步按序跑通，每步各留一条产物，业务状态停在待主管确认。
func TestExecutor_FiveStepsAllGreen(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	samples := excursionSamples()

	chain, err := NewChain(ChainDeps{Profile: testProfile(), Samples: samples})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	exec := NewExecutor(chain, st, quietLogger())

	result, err := exec.Run(ctx, directRun(t, samples))
	if err != nil {
		t.Fatalf("步骤链执行失败：%v", err)
	}

	if result.Run.ExecState != domain.ExecStateSucceeded {
		t.Fatalf("执行态应为 succeeded，实际 %q", result.Run.ExecState)
	}
	if result.Run.State != domain.RunStateAwaitingConfirmation {
		t.Fatalf("业务状态应停在待主管确认，实际 %q", result.Run.State)
	}
	if result.FailedStep != "" || result.ErrorCode != "" {
		t.Fatalf("全绿不应带失败步骤或错误码：failed_step=%q error_code=%q",
			result.FailedStep, result.ErrorCode)
	}

	wantOrder := ChainStepNames()
	if len(result.Steps) != len(wantOrder) {
		t.Fatalf("步骤数应为 %d，实际 %d", len(wantOrder), len(result.Steps))
	}
	for i, step := range result.Steps {
		if step.StepName != wantOrder[i] {
			t.Fatalf("第 %d 步应为 %q，实际 %q", i+1, wantOrder[i], step.StepName)
		}
		if step.State != domain.StepStateSucceeded {
			t.Fatalf("第 %d 步（%s）状态应为 succeeded，实际 %q", i+1, step.StepName, step.State)
		}
		if step.Seq != i+1 {
			t.Fatalf("第 %d 步的 seq 应为 %d，实际 %d", i+1, i+1, step.Seq)
		}
		if len(step.Payload) == 0 {
			t.Fatalf("第 %d 步（%s）没有产物", i+1, step.StepName)
		}
		if step.EndedAt == nil {
			t.Fatalf("第 %d 步（%s）没有结束时刻", i+1, step.StepName)
		}
	}

	// 落库：一次成功判定后，run_steps 表里每步各一条产物。
	count, err := st.CountRunSteps(ctx, result.Run.RunID)
	if err != nil {
		t.Fatalf("统计步骤记录失败：%v", err)
	}
	if count != len(wantOrder) {
		t.Fatalf("run_steps 应有 %d 行，实际 %d 行", len(wantOrder), count)
	}
	stored, err := st.RunStepsByRun(ctx, result.Run.RunID)
	if err != nil {
		t.Fatalf("回读步骤记录失败：%v", err)
	}
	if len(stored) != len(wantOrder) {
		t.Fatalf("回读步骤记录应有 %d 行，实际 %d 行", len(wantOrder), len(stored))
	}
	for i, step := range stored {
		if step.RunID != result.Run.RunID {
			t.Fatalf("第 %d 行记录的 run_id 不符：%s", i+1, step.RunID)
		}
		if step.StepName != wantOrder[i] {
			t.Fatalf("回读顺序不符：第 %d 行为 %q，期望 %q", i+1, step.StepName, wantOrder[i])
		}
		if len(step.Payload) == 0 {
			t.Fatalf("回读的第 %d 行（%s）没有产物", i+1, step.StepName)
		}
	}
}

// TestExecutor_ProductsReadableByType 验证「按产物分码存储、读取时还原成对应类型」。
func TestExecutor_ProductsReadableByType(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	samples := excursionSamples()

	chain, err := NewChain(ChainDeps{Profile: testProfile(), Samples: samples})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	exec := NewExecutor(chain, st, quietLogger())
	result, err := exec.Run(ctx, directRun(t, samples))
	if err != nil {
		t.Fatalf("步骤链执行失败：%v", err)
	}

	// 从库里回读后按分码取回，形状必须还原成对应类型。
	stored, err := st.RunStepsByRun(ctx, result.Run.RunID)
	if err != nil {
		t.Fatalf("回读步骤记录失败：%v", err)
	}

	series, ok := ProductAs[domain.SeriesPayload](stored, domain.PayloadSeries)
	if !ok {
		t.Fatal("取数产物应能按 series 分码还原成 *SeriesPayload")
	}
	if series.SampleCount != len(samples) {
		t.Fatalf("取数产物的样本数应为 %d，实际 %d", len(samples), series.SampleCount)
	}

	window, ok := ProductAs[domain.WindowPayload](stored, domain.PayloadWindow)
	if !ok {
		t.Fatal("窗口产物应能按 window 分码还原成 *WindowPayload")
	}
	if window.Window.PeakC != 12.4 {
		t.Fatalf("窗口峰值应为 12.4，实际 %v", window.Window.PeakC)
	}
	if window.ZoneMinC != 2 || window.ZoneMaxC != 8 {
		t.Fatalf("窗口产物应带出所用温区，实际 %v~%v", window.ZoneMinC, window.ZoneMaxC)
	}
	if window.ZoneSource == "" {
		t.Fatal("窗口产物必须带出温区来源")
	}

	attribution, ok := ProductAs[domain.AttributionPayload](stored, domain.PayloadAttribution)
	if !ok {
		t.Fatal("归因产物应能按 attribution 分码还原成 *AttributionPayload")
	}
	if attribution.CauseCode != domain.EventPowerLoss {
		t.Fatalf("窗口内有断电事件上报，原因结论应为 power_loss，实际 %q", attribution.CauseCode)
	}
	if len(attribution.Evidence) == 0 {
		t.Fatal("归因结论必须带证据")
	}
	if attribution.Evidence[0].Threshold != 1 {
		t.Fatalf("证据必须带阈值，实际 %v", attribution.Evidence[0].Threshold)
	}

	record, ok := ProductAs[domain.ExceptionRecordPayload](stored, domain.PayloadExceptionRecord)
	if !ok {
		t.Fatal("成稿产物应能按 exception_record 分码还原成 *ExceptionRecordPayload")
	}
	if record.PeakTemperatureC != 12.4 || record.DurationMinutes != 10 {
		t.Fatalf("成稿产物的峰值或持续分钟不符：%v / %v",
			record.PeakTemperatureC, record.DurationMinutes)
	}
	if record.CauseConclusion != domain.EventPowerLoss {
		t.Fatalf("成稿产物应带上原因结论，实际 %q", record.CauseConclusion)
	}
	// 批次与处置此刻拿不到：必须显式列为待补，而不是填一个像样的值。
	if !record.NeedsReview {
		t.Fatal("批次与处置待补时成稿必须标 needs_review")
	}
	for _, field := range []string{"batches", "disposition"} {
		found := false
		for _, missing := range record.MissingFields {
			if missing == field {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("成稿缺项列表应含 %q，实际 %v", field, record.MissingFields)
		}
	}

	ready, ok := ProductAs[domain.ConfirmationReadyPayload](stored, domain.PayloadConfirmationReady)
	if !ok {
		t.Fatal("待确认产物应能按 confirmation_ready 分码还原成 *ConfirmationReadyPayload")
	}
	if ready.AwaitingRole != "supervisor" {
		t.Fatalf("待确认产物应指向主管，实际 %q", ready.AwaitingRole)
	}
	if ready.ExternalRef != result.Run.ExternalRef {
		t.Fatalf("待确认产物的 external_ref 与 run 不一致：%s ≠ %s",
			ready.ExternalRef, result.Run.ExternalRef)
	}

	// 跨分码取回必须被拒绝：分码是形状的唯一凭据。
	if _, ok := ProductAs[domain.SeriesPayload](stored, domain.PayloadWindow); ok {
		t.Fatal("用 series 分码取回窗口产物应被拒绝")
	}
	if _, ok := ProductAs[domain.AttributionPayload](stored, domain.PayloadExceptionRecord); ok {
		t.Fatal("用 attribution 分码取回成稿产物应被拒绝")
	}
}

// TestExecutor_StepFailureFailsRunAndKeepsLastProduct 验证失败隔离：
// 某一步失败后 run 落失败态、保留失败原因与此前各步的产物。
//
// 模型不可达属可重试错误，因此这一步会被试 3 次才判失败——
// 记录里能看到三次尝试各留一行，前两步的产物照旧保留。
func TestExecutor_StepFailureFailsRunAndKeepsLastProduct(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	samples := excursionSamples()

	chain, err := NewChain(ChainDeps{
		Profile: testProfile(),
		Samples: samples,
		Attribute: func(ctx context.Context, in StepInput) (any, error) {
			return nil, errcode.Newf(errcode.ModelUnavailable, "模型服务不可达：连接被拒绝")
		},
	})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	// 退避等待换掉：测试要的是行为，不是秒表。
	exec := NewExecutor(chain, st, quietLogger()).WithWaitFunc((&waitRecorder{}).wait)

	result, err := exec.Run(ctx, directRun(t, samples))
	if err == nil {
		t.Fatal("归因步骤失败时整条链应报错")
	}
	if code := errcode.As(err).Code; code != errcode.ModelUnavailable {
		t.Fatalf("失败错误码应为 %s，实际 %s", errcode.ModelUnavailable, code)
	}

	if result.Run.ExecState != domain.ExecStateFailed {
		t.Fatalf("执行态应落到 failed，实际 %q", result.Run.ExecState)
	}
	if result.FailedStep != StepAttribute {
		t.Fatalf("失败步骤应为 %q，实际 %q", StepAttribute, result.FailedStep)
	}
	if result.ErrorCode != errcode.ModelUnavailable {
		t.Fatalf("结果里的错误码应为 %s，实际 %s", errcode.ModelUnavailable, result.ErrorCode)
	}

	// 失败前跑完的两步保留产物；失败的那一步三次尝试都没有产物。
	if len(result.Steps) != 5 {
		t.Fatalf("应记录到失败那一步为止共 5 条（成功两步 + 失败三次尝试），实际 %d 条",
			len(result.Steps))
	}
	for _, step := range result.Steps[2:] {
		if step.State != domain.StepStateFailed {
			t.Fatalf("第 %d 次尝试状态应为 failed，实际 %q", step.Attempt, step.State)
		}
		if step.ErrorCode != string(errcode.ModelUnavailable) {
			t.Fatalf("第 %d 次尝试应记下错误码 %s，实际 %q",
				step.Attempt, errcode.ModelUnavailable, step.ErrorCode)
		}
		if !strings.Contains(step.Error, "连接被拒绝") {
			t.Fatalf("第 %d 次尝试应保留失败原因，实际 %q", step.Attempt, step.Error)
		}
		if len(step.Payload) != 0 {
			t.Fatal("失败步骤不应留下产物")
		}
	}

	// 最后一次成功产物仍在：复核要看的正是「它走到了哪一步、停在哪一步」。
	window, ok := ProductAs[domain.WindowPayload](result.Steps, domain.PayloadWindow)
	if !ok {
		t.Fatal("失败前的窗口产物应仍然可按分码取回")
	}
	if window.Window.PeakC != 12.4 {
		t.Fatalf("保留的窗口产物内容不符：峰值 %v", window.Window.PeakC)
	}
	if _, ok := ProductAs[domain.AttributionPayload](result.Steps, domain.PayloadAttribution); ok {
		t.Fatal("失败步骤不应产出归因结论")
	}

	// 落库：失败的三次尝试各留一行（带错误码），前面两行照旧。
	stored, err := st.RunStepsByRun(ctx, result.Run.RunID)
	if err != nil {
		t.Fatalf("回读步骤记录失败：%v", err)
	}
	if len(stored) != 5 {
		t.Fatalf("run_steps 应有 5 行（成功两步 + 失败三次尝试），实际 %d 行", len(stored))
	}
	if stored[2].State != domain.StepStateFailed ||
		stored[2].ErrorCode != string(errcode.ModelUnavailable) {
		t.Fatalf("落库的失败行应带状态与错误码，实际 state=%q error_code=%q",
			stored[2].State, stored[2].ErrorCode)
	}
}

// TestExecutor_StoreFailureFailsStep 验证「产物没存下来就不算数」：
// 落库失败时这一步判失败，run 不被标成成功。
func TestExecutor_StoreFailureFailsStep(t *testing.T) {
	samples := excursionSamples()
	chain, err := NewChain(ChainDeps{Profile: testProfile(), Samples: samples})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	// 退避等待换掉：落库一直失败时不必真等满退避时长。
	exec := NewExecutor(chain, brokenRecorder{}, quietLogger()).WithWaitFunc((&waitRecorder{}).wait)

	result, err := exec.Run(context.Background(), directRun(t, samples))
	if err == nil {
		t.Fatal("落库失败时步骤链应报错")
	}
	if code := errcode.As(err).Code; code != errcode.StorageError {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.StorageError, code)
	}
	if result.Run.ExecState != domain.ExecStateFailed {
		t.Fatalf("执行态应落到 failed，实际 %q", result.Run.ExecState)
	}
	if result.FailedStep != StepFetchSeries {
		t.Fatalf("失败步骤应为 %q，实际 %q", StepFetchSeries, result.FailedStep)
	}
}

// TestExecutor_PayloadKindMismatchFailsStep 验证产物分码与载荷类型必须对得上：
// 对不上时这一步判失败，不硬写进库。
func TestExecutor_PayloadKindMismatchFailsStep(t *testing.T) {
	chain := []StepSpec{{
		Name:        StepFetchSeries,
		PayloadKind: domain.PayloadSeries,
		Handler: func(ctx context.Context, in StepInput) (any, error) {
			// 归因结论的载荷挂到取数分码上：形状对不上。
			return &domain.AttributionPayload{CauseCode: "power_loss"}, nil
		},
	}}
	exec := NewExecutor(chain, nil, quietLogger())

	result, err := exec.Run(context.Background(), directRun(t, excursionSamples()))
	if err == nil {
		t.Fatal("产物分码与类型不匹配时应报错")
	}
	if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.Validation, code)
	}
	if result.Run.ExecState != domain.ExecStateFailed {
		t.Fatalf("执行态应落到 failed，实际 %q", result.Run.ExecState)
	}
	if len(result.Steps) != 1 || len(result.Steps[0].Payload) != 0 {
		t.Fatal("类型不匹配的产物不应被写入")
	}
}

// TestExecutor_IllegalExecTransitionRejected 验证执行态迁移受白名单约束。
func TestExecutor_IllegalExecTransitionRejected(t *testing.T) {
	chain, err := NewChain(ChainDeps{Profile: testProfile(), Samples: excursionSamples()})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	exec := NewExecutor(chain, nil, quietLogger())

	run := directRun(t, excursionSamples())
	run.ExecState = domain.ExecStateSucceeded

	if _, err := exec.Run(context.Background(), run); err == nil {
		t.Fatal("已完成的 run 不应被再次执行")
	} else if code := errcode.As(err).Code; code != errcode.IllegalTransition {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.IllegalTransition, code)
	}
}

// TestExecutor_RunsOnRunBuiltWithoutScheduler 验证执行器与调度器解耦：
// 不经过调度器、直接建出来的 run 同样能跑完整条步骤链。
func TestExecutor_RunsOnRunBuiltWithoutScheduler(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	samples := excursionSamples()

	// 直接建 run：不经过调度器，也不带窗口幂等键以外的调度概念。
	run := domain.NewRun(testPointID)
	run.BindWindow(samples[0].MeasuredAt, samples[len(samples)-1].MeasuredAt)

	chain, err := NewChain(ChainDeps{Profile: testProfile(), Samples: samples})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	exec := NewExecutor(chain, st, quietLogger())

	result, err := exec.Run(ctx, run)
	if err != nil {
		t.Fatalf("步骤链执行失败：%v", err)
	}
	if result.Run.ExecState != domain.ExecStateSucceeded {
		t.Fatalf("执行态应为 succeeded，实际 %q", result.Run.ExecState)
	}
	if result.Run.RunID != run.RunID {
		t.Fatalf("执行器不应换掉 run_id：%s ≠ %s", result.Run.RunID, run.RunID)
	}
}

// TestExecutor_AfterSchedulerRun 验证调度器建出的 run 能接着交给执行器跑，
// 且两者互不越界：调度器只管 run 生命周期与排队，执行器只管步骤顺序与产物。
func TestExecutor_AfterSchedulerRun(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	samples := excursionSamples()
	profile := testProfile()

	scheduler := NewScheduler(nil)
	var run domain.Run
	for _, ev := range samples {
		dispatch, err := scheduler.Dispatch(ctx, ev, profile)
		if err != nil {
			t.Fatalf("调度投递失败：%v", err)
		}
		run = dispatch.Run
	}
	if run.RunID == "" {
		t.Fatal("调度器应建出一条 run")
	}

	chain, err := NewChain(ChainDeps{Profile: profile, Samples: samples})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	exec := NewExecutor(chain, st, quietLogger())

	result, err := exec.Run(ctx, run)
	if err != nil {
		t.Fatalf("步骤链执行失败：%v", err)
	}
	if result.Run.RunID != run.RunID {
		t.Fatalf("执行器应沿用调度器建的 run_id：%s ≠ %s", result.Run.RunID, run.RunID)
	}
	if result.Run.ExecState != domain.ExecStateSucceeded {
		t.Fatalf("执行态应为 succeeded，实际 %q", result.Run.ExecState)
	}
}

// TestNewChain_StepOrderAndKindsAreFixed 把步骤链的顺序与产物分码钉死：
// 顺序是业务约定（先取数才谈得上抽窗口），不能被实现悄悄改掉。
func TestNewChain_StepOrderAndKindsAreFixed(t *testing.T) {
	chain, err := NewChain(ChainDeps{Profile: testProfile()})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}

	wantNames := []string{
		StepFetchSeries, StepExtractWindow, StepAttribute, StepDraft, StepAwaitConfirmation,
	}
	wantKinds := []domain.StepPayloadKind{
		domain.PayloadSeries, domain.PayloadWindow, domain.PayloadAttribution,
		domain.PayloadExceptionRecord, domain.PayloadConfirmationReady,
	}
	if len(chain) != len(wantNames) {
		t.Fatalf("步骤数应为 %d，实际 %d", len(wantNames), len(chain))
	}
	for i, spec := range chain {
		if spec.Name != wantNames[i] {
			t.Fatalf("第 %d 步应为 %q，实际 %q", i+1, wantNames[i], spec.Name)
		}
		if spec.PayloadKind != wantKinds[i] {
			t.Fatalf("第 %d 步的产物分码应为 %q，实际 %q", i+1, wantKinds[i], spec.PayloadKind)
		}
		if spec.Handler == nil {
			t.Fatalf("第 %d 步（%s）没有处理器", i+1, spec.Name)
		}
	}
	got := ChainStepNames()
	if len(got) != len(wantNames) {
		t.Fatalf("步骤名清单长度应为 %d，实际 %d", len(wantNames), len(got))
	}
	for i := range wantNames {
		if got[i] != wantNames[i] {
			t.Fatalf("步骤名清单第 %d 项应为 %q，实际 %q", i+1, wantNames[i], got[i])
		}
	}
}

// TestNewChain_MissingProfileRejected 验证温区配置缺失时装配即被拒：
// 系统不猜温区、不用默认值顶替。
func TestNewChain_MissingProfileRejected(t *testing.T) {
	if _, err := NewChain(ChainDeps{}); err == nil {
		t.Fatal("缺温区配置时步骤链不应装配成功")
	} else if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.Validation, code)
	}
}

// TestExecutor_TimelineReflectsSteps 验证时间线按链上次序还原每一步的起止与耗时。
func TestExecutor_TimelineReflectsSteps(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	samples := excursionSamples()

	chain, err := NewChain(ChainDeps{Profile: testProfile(), Samples: samples})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	exec := NewExecutor(chain, st, quietLogger())
	result, err := exec.Run(ctx, directRun(t, samples))
	if err != nil {
		t.Fatalf("步骤链执行失败：%v", err)
	}

	timeline := Timeline(result.Steps)
	if len(timeline) != 5 {
		t.Fatalf("时间线应有 5 条，实际 %d 条", len(timeline))
	}
	for i, entry := range timeline {
		if entry.Seq != i+1 {
			t.Fatalf("时间线第 %d 条的 seq 应为 %d，实际 %d", i+1, i+1, entry.Seq)
		}
		if entry.EndedAt == nil {
			t.Fatalf("时间线第 %d 条没有结束时刻", i+1)
		}
		if entry.DurationMS < 0 {
			t.Fatalf("时间线第 %d 条耗时为负：%d", i+1, entry.DurationMS)
		}
	}
}

// brokenRecorder 是一个始终失败的落库口，用来验证「产物没存下来就不算数」。
type brokenRecorder struct{}

func (brokenRecorder) InsertRunStep(ctx context.Context, step domain.Step) error {
	return errcode.Newf(errcode.StorageError, "模拟落库失败：%s", step.StepName)
}

func (brokenRecorder) RunStepsByRun(ctx context.Context, runID string) ([]domain.Step, error) {
	return nil, errcode.Newf(errcode.StorageError, "模拟落库失败：%s", runID)
}

// 编译期确认测试用到的类型都实现了接口。
var (
	_ StepRecorder = (*store.Store)(nil)
	_ StepRecorder = brokenRecorder{}
)
