// 本文件落「进程重启后接着跑」这件事：续跑计划、启动扫描与续跑执行。
//
// 现场场景：院内小主机上的判定服务在判定跑到一半时被重启了——
// 断电、内核升级、发布、巡检。这条判定不能就这么消失：库里那批温度读数
// 明明白白越限过一次，药学部要的是「那次到底判了没有、判到哪一步」。
//
// 续跑靠什么认账：
//   - run_steps 表里已成功的步骤产物：它们就是「已经算过」的凭据，
//     已完成的步骤不重跑（重跑会用可能已经变了的温区口径把结论改写一遍）。
//   - runs 表里 run 的执行态：非终态的 run 才需要续跑。
//   - runs 表里 run 的写回状态：台账已经写成的 run 不再写一次台账。
//
// 三条不许让步的约定：
//   - 不重跑已完成的步骤：重复判定会把一条越温记成两条。
//   - 不重复写回台账：同一段越温在台账里只能有一条 deviations 记录。
//   - 认不出依据就不续跑：没有温区配置的 run 落「需人工处理」，
//     绝不拿默认值顶替温区硬跑出一个结论。
package runtime

import (
	"context"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// 续跑跳过的原因。
//
// 跳过也要有说法：现场问「这条判定为什么没接着跑」时，
// 答案必须是「因为台账已经写过」或「因为没有温区配置」，
// 而不是沉默——沉默会被读成「系统把它忘了」。
const (
	// SkipReasonWrittenBack：台账已写回，不能再写一次。
	SkipReasonWrittenBack = "台账已写回，不重复写回"
	// SkipReasonTerminal：执行态已是终态，没有可续的部分。
	SkipReasonTerminal = "执行态已是终态，无需续跑"
	// SkipReasonNoProfile：缺少点位温区配置，重建不了同一条判定链。
	SkipReasonNoProfile = "缺少点位温区配置，无法重建判定链"
	// SkipReasonAllStepsDone：全部步骤都已完成，只差把执行态补成终态。
	SkipReasonAllStepsDone = "全部步骤已完成，只差补齐执行态"
)

// ResumePlan 是一条待续跑 run 的续跑计划。
type ResumePlan struct {
	// Run 是这条 run 的当前状态快照。
	Run domain.Run
	// Profile 是这条判定所用的点位温区配置；续跑时用它重建同一条判定链。
	Profile *domain.PointProfile
	// ExistingSteps 是此前已落库的步骤记录，构成时间线的前半段。
	ExistingSteps []domain.Step
	// CompletedSteps 是已成功完成的步骤名，按链上次序。
	CompletedSteps []string
	// NextStep 是第一个未完成的步骤名；空串表示全部步骤都已完成。
	NextStep string
	// NextSeq 是第一个未完成步骤在链上的序号（从 1 起）。
	// 全部完成时取链长 + 1，表示「只剩补齐执行态这一步」。
	NextSeq int
	// SkipReason 非空表示这条 run 不该被续跑。
	SkipReason string
}

// Skipped 判断这条计划是否应当被跳过。
func (p ResumePlan) Skipped() bool { return p.SkipReason != "" }

// Products 把已成功步骤的产物还原成按分码索引的表。
//
// 只认成功步骤的产物：失败步骤没有产物（半截产物不算结论）。
// 解不开的产物直接跳过——宁可让后续步骤报「缺少前序产物」，
// 也不要拿一份形状不对的产物去算下一步。
func (p ResumePlan) Products() map[domain.StepPayloadKind]any {
	products := make(map[domain.StepPayloadKind]any, len(p.ExistingSteps))
	for _, step := range p.ExistingSteps {
		if step.State != domain.StepStateSucceeded || step.PayloadKind == "" {
			continue
		}
		value, err := domain.DecodePayload(step.PayloadKind, step.Payload)
		if err != nil {
			continue
		}
		products[step.PayloadKind] = value
	}
	return products
}

// PlanResume 按已有步骤产物判断一条 run 该从哪一步继续。
//
// 判断顺序即「先排除不该续跑的，再算续跑点」：
//  1. 写回状态已是「已写回」→ 跳过，绝不重复写台账；
//  2. 执行态已是终态 → 跳过，没有可续的部分；
//  3. 没有温区配置 → 跳过（转人工），不拿默认值顶替；
//  4. 沿着链子走一遍：已成功的步骤算完成，第一步没成功的即为续跑点。
//
// 第 4 步顺带做一次一致性检查：续跑点之后的步骤不该有成功记录。
// 有的话说明步骤记录与链上次序对不上（数据被改过或写过别的顺序），
// 这种情况宁可不续跑——按错误的口径续跑出来的结论没有人能复核。
func PlanResume(chain []StepSpec, rec domain.RunRecord, steps []domain.Step) (ResumePlan, error) {
	plan := ResumePlan{
		Run:           rec.Run,
		Profile:       rec.Profile,
		ExistingSteps: steps,
	}

	if rec.WritebackState == domain.WritebackStateWrittenBack {
		plan.SkipReason = SkipReasonWrittenBack
		return plan, nil
	}
	if domain.IsTerminalExecState(rec.Run.ExecState) {
		plan.SkipReason = SkipReasonTerminal
		return plan, nil
	}
	if rec.Profile == nil {
		plan.SkipReason = SkipReasonNoProfile
		return plan, nil
	}
	if len(chain) == 0 {
		return plan, errcode.Newf(errcode.Validation, "判定步骤链为空，无法规划续跑")
	}

	completed := make(map[string]bool, len(steps))
	for _, step := range steps {
		if step.State == domain.StepStateSucceeded {
			completed[step.StepName] = true
		}
	}

	resumeAt := 0
	for i, spec := range chain {
		if !completed[spec.Name] {
			resumeAt = i
			break
		}
		plan.CompletedSteps = append(plan.CompletedSteps, spec.Name)
	}

	if resumeAt == 0 && len(plan.CompletedSteps) == len(chain) {
		// 全部步骤都跑过了：只剩把执行态补成终态。
		plan.NextStep = ""
		plan.NextSeq = len(chain) + 1
		plan.SkipReason = SkipReasonAllStepsDone
		return plan, nil
	}

	// 一致性检查：续跑点之后不该再有成功记录。
	for _, spec := range chain[resumeAt:] {
		if completed[spec.Name] {
			return plan, errcode.Newf(errcode.Validation,
				"判定 run %s 的步骤记录与链上次序不一致：步骤 %q 尚未完成，但其后步骤 %q 已有成功记录",
				rec.Run.RunID, chain[resumeAt].Name, spec.Name)
		}
	}

	plan.NextStep = chain[resumeAt].Name
	plan.NextSeq = resumeAt + 1
	return plan, nil
}

// RunStore 是续跑所需的存储能力边界。
//
// 抽成接口是为了让运行时层不绑死存储实现：测试用内存替身，
// 生产用 SQLite，两者行为口径一致。
type RunStore interface {
	// UpsertRun 写入或更新一条判定 run 的状态快照。
	UpsertRun(ctx context.Context, rec domain.RunRecord) error
	// RunByID 回读一条判定 run 的状态快照。
	RunByID(ctx context.Context, runID string) (domain.RunRecord, error)
	// ResumableRuns 返回需要续跑的判定 run。
	ResumableRuns(ctx context.Context) ([]domain.RunRecord, error)
	// RunStepsByRun 回读一条 run 的步骤记录。
	RunStepsByRun(ctx context.Context, runID string) ([]domain.Step, error)
	// MarkRunWrittenBack 标记一条 run 的台账写入已落成。
	MarkRunWrittenBack(ctx context.Context, runID string, at time.Time) error
}

// ExecutorFactory 按温区配置造一台执行器。
//
// 续跑要用「当初那一份」温区配置重建同一条判定链，因此执行器不是现成的，
// 而是按每条 run 持久化的配置临时装配的。
type ExecutorFactory func(profile *domain.PointProfile) (*Executor, error)

// Resumer 负责启动后的续跑扫描与续跑执行。
type Resumer struct {
	// store 是 run 与步骤产物的存储口。
	store RunStore
	// factory 按温区配置装配执行器。
	factory ExecutorFactory
	// logger 记录扫描与续跑过程。
	logger *logging.Logger
	// now 是时钟；测试可注入固定时钟。
	now func() time.Time
}

// NewResumer 创建续跑器。logger 为 nil 时回落到默认日志器。
func NewResumer(st RunStore, factory ExecutorFactory, logger *logging.Logger) *Resumer {
	if logger == nil {
		logger = logging.Default()
	}
	return &Resumer{
		store:   st,
		factory: factory,
		logger:  logger,
		now:     time.Now,
	}
}

// ResumeOutcome 是一条 run 的续跑结果。
type ResumeOutcome struct {
	// RunID 是这条 run 的标识。
	RunID string `json:"run_id"`
	// Outcome 是续跑结果：resumed（跑完了）/ skipped（不该跑）/ failed（没跑成）。
	Outcome string `json:"outcome"`
	// Reason 是结果的说明；跳过与失败时说明为什么。
	Reason string `json:"reason,omitempty"`
	// NextStep 是续跑点；已续跑时为空。
	NextStep string `json:"next_step,omitempty"`
}

// 续跑结果取值。
const (
	// ResumeOutcomeResumed：这条 run 已续跑完成。
	ResumeOutcomeResumed = "resumed"
	// ResumeOutcomeSkipped：这条 run 不该被续跑。
	ResumeOutcomeSkipped = "skipped"
	// ResumeOutcomeFailed：这条 run 续跑没跑成。
	ResumeOutcomeFailed = "failed"
)

// Scan 扫出待续跑的 run 与它们的续跑计划。
//
// 扫的是「非终态且没写过台账」的 run；每条 run 再按已有步骤产物算续跑点。
func (r *Resumer) Scan(ctx context.Context) ([]ResumePlan, error) {
	records, err := r.store.ResumableRuns(ctx)
	if err != nil {
		return nil, err
	}
	plans := make([]ResumePlan, 0, len(records))
	for _, rec := range records {
		steps, err := r.store.RunStepsByRun(ctx, rec.Run.RunID)
		if err != nil {
			return nil, err
		}
		chain := r.chainFor(rec.Profile)
		if len(chain) == 0 {
			// 判定链装不起来：这条 run 没法自动续跑，落人工处理。
			plans = append(plans, ResumePlan{
				Run:        rec.Run,
				Profile:    rec.Profile,
				SkipReason: "判定步骤链装配失败，须人工处理",
			})
			continue
		}
		plan, err := PlanResume(chain, rec, steps)
		if err != nil {
			// 步骤记录与链上次序对不上：这条 run 不能自动续跑。
			// 记下来并跳过，不让它卡住整批扫描。
			r.logger.Error("resume_plan_failed",
				"run_id", rec.Run.RunID, "error", err.Error())
			plans = append(plans, ResumePlan{
				Run:        rec.Run,
				Profile:    rec.Profile,
				SkipReason: "步骤记录与链上次序不一致，须人工处理：" + errcode.As(err).Details,
			})
			continue
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

// chainFor 按温区配置装配判定步骤链。
//
// 装配不出来（配置本身有问题）时给一条空链：PlanResume 会因为链子为空
// 而拒绝出计划，那条 run 落人工处理，而不是硬跑一个空链。
func (r *Resumer) chainFor(profile *domain.PointProfile) []StepSpec {
	if r.factory == nil {
		return nil
	}
	exec, err := r.factory(profile)
	if err != nil || exec == nil {
		return nil
	}
	return exec.chain
}

// Resume 执行一条续跑计划。
//
// 计划里带跳过理由时不动这条 run：跳过也要留痕，但不碰它。
func (r *Resumer) Resume(ctx context.Context, plan ResumePlan) (ResumeOutcome, error) {
	outcome := ResumeOutcome{RunID: plan.Run.RunID, NextStep: plan.NextStep}
	if plan.Skipped() {
		outcome.Outcome = ResumeOutcomeSkipped
		outcome.Reason = plan.SkipReason
		r.logger.Info("resume_skipped", "run_id", plan.Run.RunID, "reason", plan.SkipReason)
		return outcome, nil
	}
	if r.factory == nil {
		return outcome, errcode.Newf(errcode.Validation, "未装配执行器工厂，无法续跑")
	}
	exec, err := r.factory(plan.Profile)
	if err != nil {
		outcome.Outcome = ResumeOutcomeFailed
		outcome.Reason = errcode.As(err).Details
		return outcome, err
	}
	exec.WithRunPersistence(r.store, plan.Profile)

	result, runErr := exec.Resume(ctx, plan.Run, plan)
	if runErr != nil {
		outcome.Outcome = ResumeOutcomeFailed
		outcome.Reason = errcode.As(runErr).Details
		r.logger.Error("resume_failed",
			"run_id", plan.Run.RunID, "next_step", plan.NextStep, "error", runErr.Error())
		return outcome, runErr
	}
	outcome.Outcome = ResumeOutcomeResumed
	outcome.Reason = "已从「" + plan.NextStep + "」续跑并完成"
	r.logger.Info("resume_completed",
		"run_id", plan.Run.RunID,
		"next_step", plan.NextStep,
		"exec_state", string(result.Run.ExecState))
	return outcome, nil
}

// ResumeSummary 是一次续跑扫描与执行的摘要。
type ResumeSummary struct {
	// Scanned 是扫到的待续跑 run 条数。
	Scanned int `json:"scanned"`
	// Resumed 是续跑完成的条数。
	Resumed int `json:"resumed"`
	// Skipped 是跳过的条数。
	Skipped int `json:"skipped"`
	// Failed 是续跑没跑成的条数。
	Failed int `json:"failed"`
	// Outcomes 是逐条 run 的续跑结果。
	Outcomes []ResumeOutcome `json:"outcomes"`
}

// ResumeAll 扫描并续跑全部待续跑的 run。
//
// 单条失败不打断整批：一条反复失败的判定不该把其它判定一起卡住。
// 缺温区配置的 run 在这里落「需人工处理」——系统判不了，就明说判不了，
// 而不是悄悄跳过让人以为它跑过了。
func (r *Resumer) ResumeAll(ctx context.Context) (ResumeSummary, error) {
	summary := ResumeSummary{Outcomes: []ResumeOutcome{}}
	plans, err := r.Scan(ctx)
	if err != nil {
		return summary, err
	}
	summary.Scanned = len(plans)

	for _, plan := range plans {
		if plan.SkipReason == SkipReasonNoProfile {
			// 认不出判定依据：落人工处理，让现场看到「这条没判成、为什么没判成」。
			r.markNeedsManual(ctx, plan.Run)
		}
		outcome, _ := r.Resume(ctx, plan)
		summary.Outcomes = append(summary.Outcomes, outcome)
		switch outcome.Outcome {
		case ResumeOutcomeResumed:
			summary.Resumed++
		case ResumeOutcomeSkipped:
			summary.Skipped++
		case ResumeOutcomeFailed:
			summary.Failed++
		}
	}
	return summary, nil
}

// markNeedsManual 把一条没法自动续跑的 run 落成「需人工处理」。
//
// 它是一次状态落账，不是业务结论：系统没有给出判定结论，
// 因此不推进业务状态机，只把「这条判不了」这件事记下来。
//
// 状态改写走 AdvanceState 一处（唯一入口）：推进被拒时保留原状态并记一条
// 错误日志——不硬改，也不安静地咽下去。
func (r *Resumer) markNeedsManual(ctx context.Context, run domain.Run) {
	if _, err := run.AdvanceState(workflow.Request{
		To:     workflow.StateNeedsManual,
		Action: workflow.ActionAuto,
		Reason: SkipReasonNoProfile,
		Note:   "续跑时缺少点位温区配置，系统判不了，转人工处理",
		Actor:  workflow.ActorSystem,
	}); err != nil {
		r.logger.Error("resume_mark_needs_manual_rejected",
			"run_id", run.RunID, "error_code", string(errcode.As(err).Code),
			"error", err.Error())
		return
	}
	rec := domain.RunRecord{Run: run, UpdatedAt: r.now()}
	if err := r.store.UpsertRun(ctx, rec); err != nil {
		r.logger.Error("resume_mark_needs_manual_failed",
			"run_id", run.RunID, "error", err.Error())
		return
	}
	r.logger.Warn("resume_needs_manual",
		"run_id", run.RunID, "point_id", run.PointID, "reason", SkipReasonNoProfile)
}
