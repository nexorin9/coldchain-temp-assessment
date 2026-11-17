// Package runtime 是判定 run 的调度与执行层。
//
// 本文件落执行器：把判定步骤链按顺序跑一遍，每一步的产物按分码落库，
// 某一步失败时把 run 落到失败态并保留此前的产物与失败原因。
//
// 源产品机制对应（溯源见 spec.md「开源对照」与「参考地基」）：
// 源产品一次函数调用由若干步骤按序推进，每步的尝试次数、起止时刻与结果
// 各自独立记录，失败只落在那一步上、不推进整条 run；每类步骤元数据
// 各自带独立的 kind 常量与独立结构，换掉一种不影响其它种类
// （github_ref/inngest/ui/packages/components/src/generated/index.ts：
// 每类元数据一个 kind，各带独立 const 与独立 interface，可单独替换）。
// 运行详情页把一次 run 还原成按步骤排开的时间线
// （github_ref/inngest/ui/packages/components/src/RunDetailsV4/index.ts：
// TimelineBar / TimelineHeader / Timeline 与 calculateBarPosition、formatDuration）。
// 搬到冷链场景，对应物是：
//
//   - 步骤链 = 取数 → 抽越限窗口 → 归因 → 成稿记录表 → 停在待主管确认
//   - 步骤产物分码 = 采样 / 窗口 / 归因结论 / 成稿记录表 / 待确认标记，各自独立
//   - 步骤记录 = 起止时刻、尝试次数、状态、错误码，逐条落库可回看
//
// 执行器与调度器是分开的两个角色：调度器管 run 的生命周期与排队
// （同窗去抖、按点位限并发），执行器只管步骤顺序与产物。
// 执行器不持有调度器引用，也不认识「窗口幂等键」这类调度概念——
// 它拿到一条 run 就按链子跑，跑完把产物交出去。
//
// 三条不许让步的约定：
//   - 失败不推进：某步失败时 run 落失败态，绝不标成成功。
//     一条带着不存在结论的 run 去写台账，比不写坏得多。
//   - 产物不串型：产物按分码存取，取回时核对分码，对不上就报错。
//   - 失败留痕：失败的步骤照样落一行，带错误码。
//     「这步没跑成」和「这步跑成了但结论为空」是两回事，不能只记后者。
package runtime

import (
	"context"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
)

// 判定步骤名（步骤链的固定顺序）。
//
// 顺序本身是业务约定，不是实现细节：先取到读数，才谈得上抽窗口；
// 先有窗口，才谈得上归因；先有归因，才谈得上成稿；
// 成稿齐了，才谈得上「停在待主管确认」。换顺序就会得出用不上的结论。
const (
	// StepFetchSeries：取数——把这次判定依据的温度采样取到手。
	StepFetchSeries = "fetch_series"
	// StepExtractWindow：抽越限窗口——按点位温区比较温度，抽出越温窗口。
	StepExtractWindow = "extract_window"
	// StepAttribute：归因——给出原因候选与证据。
	StepAttribute = "attribute"
	// StepDraft：成稿——组装《温度异常情况记录表》的结构化内容。
	StepDraft = "draft"
	// StepAwaitConfirmation：待确认——结论就绪，停在药学部主管确认之前。
	StepAwaitConfirmation = "await_confirmation"
)

// StepHandler 是一个判定步骤的实现：拿到输入，产出该步的产物。
//
// 产物按分码返回具体类型（*domain.SeriesPayload 等）。执行器不解析业务内容，
// 只负责「跑它、按分码把它存下来、失败时不推进状态」。
type StepHandler func(ctx context.Context, in StepInput) (any, error)

// StepInput 是步骤处理器的输入。
type StepInput struct {
	// Run 是当前判定 run 的快照。
	Run domain.Run
	// Seq 是本步在链上的次序，从 1 起。
	Seq int
	// StepName 是本步的步骤名。
	StepName string
	// Products 是前序步骤已产出的产物，按分码索引。
	Products map[domain.StepPayloadKind]any
}

// Product 取回前序某一步的产物。
func (in StepInput) Product(kind domain.StepPayloadKind) (any, bool) {
	value, ok := in.Products[kind]
	return value, ok
}

// StepSpec 描述步骤链上的一个步骤。
type StepSpec struct {
	// Name 是步骤名。
	Name string
	// PayloadKind 是该步产物的分码。
	//
	// 同一分码只允许登记一种结果形状：重复登记在启动校验里被拦下，
	// 防止判定结果结构随实现漂移，下游解析随之失效。
	PayloadKind domain.StepPayloadKind
	// Handler 是这一步的实现。
	Handler StepHandler
	// OnSuccess 在该步成功后推进 run 的业务状态。
	//
	// 业务状态只在这里被推进，且只到「已评估」「待确认」为止：
	// 再往后（已确认 / 已写回）只能由主管的确认动作推进。
	//
	// 推进被拒（迁移不合法）时返回错误，这一步按失败处理：
	// 状态被误推进比状态推不动危险得多，宁可让这一步红着，
	// 也不让一条 run 带着一份说不清来路的结论继续往下走。
	OnSuccess func(run *domain.Run, product any) error
}

// StepRecorder 是步骤产物的落库口。
//
// 抽成接口是为了让执行器不绑死存储实现：库写不进去时执行器要把这一步判失败
// （产物没存下来就不算数），但「产物存到哪儿」是部署形态的事。
type StepRecorder interface {
	// InsertRunStep 写入或更新一条步骤记录。
	InsertRunStep(ctx context.Context, step domain.Step) error
	// RunStepsByRun 回读一条 run 的步骤记录。
	RunStepsByRun(ctx context.Context, runID string) ([]domain.Step, error)
}

// RunRecorder 是判定 run 状态的落库口。
//
// 它与 StepRecorder 分开声明：步骤产物是「这一步算出了什么」，
// run 状态是「这条判定走到哪一步了」。两者推进频率不同（每步一次 vs 每次状态变化），
// 落库口径也不同（run 状态是整条快照，不是增量补丁）。
type RunRecorder interface {
	// UpsertRun 写入或更新一条判定 run 的状态快照。
	UpsertRun(ctx context.Context, rec domain.RunRecord) error
}

// Executor 是判定步骤链的执行器。
type Executor struct {
	// chain 是按序执行的步骤。
	chain []StepSpec
	// recorder 是步骤产物的落库口；为 nil 时只在内存里走一遍（不落库）。
	recorder StepRecorder
	// logger 记录步骤起止与失败；为 nil 时回落到默认日志器（写 stderr）。
	logger *logging.Logger
	// now 是时钟；测试可注入固定时钟，让步骤耗时可复现。
	now func() time.Time
	// retry 是步骤级重试策略：失败几次还重试、隔多久再试、哪些错误不该重试。
	retry RetryPolicy
	// wait 是退避等待；测试注入一个立即返回的函数，就能断言退避时长序列
	// 而不必真的睡。生产用按 ctx 可取消的等待。
	wait func(ctx context.Context, d time.Duration) error
	// shapes 是结果形状注册表：每一步的产物都要对得上它登记的形状，
	// 对不上就判这一步失败，不让形状存疑的产物进库。
	shapes *ResultShapeRegistry
	// runRecorder 是 run 状态的落库口；为 nil 时不落库（只在内存里走一遍）。
	runRecorder RunRecorder
	// profile 是这条判定所用的点位温区配置。
	//
	// 它随 run 一起落库：进程重启后要接着跑，靠的就是这份配置重建同一条判定链。
	// 温区换了口径，重跑出来的结论就不是同一条判定了。
	profile *domain.PointProfile
}

// NewExecutor 创建执行器。logger 为 nil 时回落到默认日志器。
//
// 重试策略取默认：最多 3 次尝试、退避指数增长、校验类与状态迁移类错误不重试。
func NewExecutor(chain []StepSpec, recorder StepRecorder, logger *logging.Logger) *Executor {
	if logger == nil {
		logger = logging.Default()
	}
	return &Executor{
		chain:    chain,
		recorder: recorder,
		logger:   logger,
		now:      time.Now,
		retry:    DefaultRetryPolicy(),
		wait:     sleepCtx,
		shapes:   defaultShapes(),
	}
}

// WithShapeRegistry 设置结果形状注册表并返回自身，便于链式装配。
//
// 传入 nil 表示不做形状自检（只在确有必要时用；默认是查内置注册表）。
func (e *Executor) WithShapeRegistry(reg *ResultShapeRegistry) *Executor {
	e.shapes = reg
	return e
}

// defaultShapes 返回内置结果形状注册表。
//
// 内置形状的名字与分码都写死在代码里，彼此不会冲突；万一真冲突了
// （改了形状忘了同步），这里返回一张空表——随后的产物自检会把
// 「未登记」明确报出来，而不是默默放行。
func defaultShapes() *ResultShapeRegistry {
	reg, err := DefaultResultShapeRegistry()
	if err != nil {
		return NewResultShapeRegistry()
	}
	return reg
}

// WithRetryPolicy 设置步骤级重试策略并返回自身，便于链式装配。
//
// 传零值策略等于用默认策略（Normalize 会把非法取值修回默认）。
func (e *Executor) WithRetryPolicy(policy RetryPolicy) *Executor {
	e.retry = policy.Normalize()
	return e
}

// WithRunPersistence 打开 run 状态持久化并返回自身，便于链式装配。
//
// rec 是 run 状态的落库口；profile 是这条判定所用的温区配置，
// 随 run 一起落库，供进程重启后重建同一条判定链。
//
// rec 为 nil 时不落库：只落步骤产物、不落 run 状态，判定仍然跑得完，
// 但重启后无法续跑——这是部署形态的选择，不是静默降级。
func (e *Executor) WithRunPersistence(rec RunRecorder, profile *domain.PointProfile) *Executor {
	e.runRecorder = rec
	e.profile = profile
	return e
}

// WithWaitFunc 设置退避等待函数并返回自身。
//
// 这是重试的「可注入时钟」：测试注入一个记录时长后立即返回的函数，
// 既能断言退避序列是指数增长的，又不用真的把测试跑慢几秒。
func (e *Executor) WithWaitFunc(wait func(ctx context.Context, d time.Duration) error) *Executor {
	if wait != nil {
		e.wait = wait
	}
	return e
}

// ExecutionResult 是一次步骤链执行的结果。
type ExecutionResult struct {
	// Run 是执行后的 run：执行态与业务状态都推进到了该到的位置。
	Run domain.Run `json:"run"`
	// Steps 是各步的记录（含产物信封），按链上次序排列。
	Steps []domain.Step `json:"steps"`
	// FailedStep 是失败的那一步；全部成功时为空。
	FailedStep string `json:"failed_step,omitempty"`
	// ErrorCode 与 ErrorMessage 是失败原因；全部成功时为空。
	ErrorCode    errcode.Code `json:"error_code,omitempty"`
	ErrorMessage string       `json:"error_message,omitempty"`
}

// Product 按分码取回某类产物，还原成对应类型。
func (r *ExecutionResult) Product(kind domain.StepPayloadKind) (any, bool) {
	return ProductFrom(r.Steps, kind)
}

// ProductFrom 从步骤记录里按分码取回产物，还原成对应类型。
//
// 只认成功步骤的产物：失败步骤没有产物（半截产物不算结论）。
func ProductFrom(steps []domain.Step, kind domain.StepPayloadKind) (any, bool) {
	for i := range steps {
		if steps[i].State != domain.StepStateSucceeded || steps[i].PayloadKind != kind {
			continue
		}
		value, err := domain.DecodePayload(kind, steps[i].Payload)
		if err != nil {
			return nil, false
		}
		return value, true
	}
	return nil, false
}

// ProductAs 从步骤记录里按分码取回产物并断言成指定类型。
//
// 分码对不上、类型对不上或该步没成功，都返回 false：宁可说「没有」，
// 也不要给出一份形状不对的产物。
func ProductAs[T any](steps []domain.Step, kind domain.StepPayloadKind) (*T, bool) {
	for i := range steps {
		if steps[i].State != domain.StepStateSucceeded || steps[i].PayloadKind != kind {
			continue
		}
		typed, err := domain.DecodePayloadAs[T](kind, steps[i].Payload)
		if err != nil {
			return nil, false
		}
		return typed, true
	}
	return nil, false
}

// Run 按顺序执行步骤链。
//
// 步骤链全部成功时 run 的执行态落到 succeeded；任一步失败时落 failed，
// 并在结果里带上失败步骤与错误码，此前各步的产物原样保留——
// 复核时要看的正是「它走到了哪一步、在哪一步停住」。
//
// 执行态（ExecState）与业务状态（Run.State）是两条状态机：
// 执行态描述「跑到哪一步了」，业务状态描述「结论走到哪一步了」。
// 业务状态只由步骤的 OnSuccess 推进，且只到「待确认」为止。
func (e *Executor) Run(ctx context.Context, run domain.Run) (ExecutionResult, error) {
	return e.execute(ctx, run, 1, nil, nil)
}

// Resume 从已有产物继续跑未完成的步骤。
//
// 续跑的三条纪律：
//   - 已完成的步骤不重跑：它们的产物从库里取回，直接喂给后面的步骤；
//   - 已失败的步骤接着往下试：尝试序号接着上次走，时间线上看得见
//     「上次 attempt=1 失败、重启后 attempt=2 成功」；
//   - 终态不续跑：已经跑完或已经判失败的 run 由 ResumeTransition 拒绝。
func (e *Executor) Resume(ctx context.Context, run domain.Run, plan ResumePlan) (ExecutionResult, error) {
	return e.execute(ctx, run, plan.NextSeq, plan.Products(), plan.ExistingSteps)
}

// execute 是 Run 与 Resume 共用的执行体。
//
// startSeq 是链上第一个要跑的步骤序号（从 1 起）；products 是前序步骤已产出的产物；
// existing 是此前已落库的步骤记录——续跑时它们构成完整时间线的前半段。
func (e *Executor) execute(
	ctx context.Context,
	run domain.Run,
	startSeq int,
	products map[domain.StepPayloadKind]any,
	existing []domain.Step,
) (ExecutionResult, error) {
	result := ExecutionResult{Run: run, Steps: existing}
	if products == nil {
		products = map[domain.StepPayloadKind]any{}
	}
	if startSeq <= 1 {
		if result.Run.ExecState == "" {
			// 执行态空值不是「queued」的别名，但也没必要因此拒绝一次合法判定：
			// 刚建出来、还没入队的 run 就是要被执行。
			result.Run.ExecState = domain.ExecStateQueued
		}
		if err := result.Run.Transition(domain.ExecStateRunning); err != nil {
			return result, err
		}
	} else if err := result.Run.ResumeTransition(); err != nil {
		return result, err
	}
	e.logger.Info("run_step_chain_started",
		"run_id", result.Run.RunID,
		"point_id", result.Run.PointID,
		"step_count", len(e.chain),
		"start_seq", startSeq)
	// run 状态随链子推进实时落库：进程被杀掉时，库里留下的是
	// 「它走到了哪一步」，而不是一个空壳 run。
	if err := e.persistRun(ctx, &result); err != nil {
		return result, e.failRun(&result, "", err)
	}

	attempts := lastAttempts(result.Steps)
	for i, spec := range e.chain {
		seq := i + 1
		if seq < startSeq {
			// 续跑：这一步已经跑过并落库了，不重跑。
			continue
		}
		if err := ctx.Err(); err != nil {
			return result, e.failRun(&result, spec.Name,
				errcode.Newf(errcode.Validation, "判定被取消：%v", err))
		}
		product, err := e.runStep(ctx, &result, spec, seq, products, attempts[spec.Name])
		if err != nil {
			return result, e.failRun(&result, spec.Name, err)
		}
		products[spec.PayloadKind] = product
	}

	if err := result.Run.Transition(domain.ExecStateSucceeded); err != nil {
		return result, err
	}
	if err := e.persistRun(ctx, &result); err != nil {
		return result, e.failRun(&result, "", err)
	}
	e.logger.Info("run_step_chain_succeeded",
		"run_id", result.Run.RunID,
		"step_count", len(e.chain))
	return result, nil
}

// lastAttempts 统计每一步已经尝试到第几轮。
//
// 续跑时尝试序号接着上次走：上一次 attempt=1 失败，重启后这一轮是 attempt=2。
// 序号接着走而不是从头开始，时间线上才看得出「这是重试，不是重跑」。
func lastAttempts(steps []domain.Step) map[string]int {
	attempts := make(map[string]int, len(steps))
	for _, step := range steps {
		if step.Attempt > attempts[step.StepName] {
			attempts[step.StepName] = step.Attempt
		}
	}
	return attempts
}

// runStep 执行一步：计时、跑处理器、按分码编码产物、落库；失败时按策略重试。
//
// 重试的语义在三个地方同时成立，缺一不可：
//   - 每次尝试各留一条步骤记录（attempt 序号不同），失败的那次也留，带错误码；
//   - 退避时长写进失败那条记录，时间线上看得见这一步等了多久；
//   - 重试用尽就把错误原样抛回，让 run 落失败态——绝不把「试了三次都没成」
//     写成成功。
//
// 重试只发生在这一步内部：不换链上位置、不重跑前面已成功的步骤。
// 前序产物在重试期间原样保留——重试的是这一步，不是整条链。
//
// priorAttempt 是这一步此前已尝试到的序号（续跑时非零）：本轮从它后面接着试。
func (e *Executor) runStep(
	ctx context.Context,
	result *ExecutionResult,
	spec StepSpec,
	seq int,
	products map[domain.StepPayloadKind]any,
	priorAttempt int,
) (any, error) {
	first := priorAttempt + 1
	if first < 1 {
		first = 1
	}
	for attempt := first; ; attempt++ {
		step, product, err := e.attempt(ctx, result, spec, seq, products, attempt)
		if err == nil {
			result.Steps = append(result.Steps, step)
			return product, nil
		}

		if !e.retry.ShouldRetry(err, attempt) {
			// 不该再试：这一次就是最终结论，失败原因原样抛回。
			result.Steps = append(result.Steps, step)
			return nil, err
		}

		backoff := e.retry.Backoff(attempt)
		// 退避时长补记到这条失败记录上：同一条记录更新一次即可
		// （主键是 run_id + step_name + attempt，不会多出第二行）。
		step.BackoffMS = backoff.Milliseconds()
		if perr := e.persist(ctx, step); perr != nil {
			e.logger.Error("run_step_persist_failed",
				"run_id", step.RunID, "step_name", step.StepName,
				"attempt", attempt, "error", perr.Error())
		}
		result.Steps = append(result.Steps, step)

		e.logger.Warn("run_step_retry_scheduled",
			"run_id", step.RunID, "step_name", spec.Name,
			"attempt", attempt, "max_attempts", e.retry.MaxAttempts,
			"error_code", string(errcode.As(err).Code), "backoff_ms", step.BackoffMS)

		// 执行态走到「重试中」，再走回「执行中」：状态机如实反映这条判定在等。
		if terr := result.Run.Transition(domain.ExecStateRetrying); terr != nil {
			e.logger.Error("run_state_transition_failed",
				"run_id", result.Run.RunID, "error", terr.Error())
			return nil, err
		}
		if werr := e.wait(ctx, backoff); werr != nil {
			return nil, errcode.Newf(errcode.Validation,
				"判定在退避等待中被取消：%v", werr)
		}
		if terr := result.Run.Transition(domain.ExecStateRunning); terr != nil {
			e.logger.Error("run_state_transition_failed",
				"run_id", result.Run.RunID, "error", terr.Error())
			return nil, err
		}
	}
}

// attempt 执行一步的一次尝试：计时、跑处理器、按分码编码产物、落库。
//
// 失败时把这一步落成失败态并返回错误（半截产物不算结论）；
// 调用方决定要不要按重试策略再来一次。
func (e *Executor) attempt(
	ctx context.Context,
	result *ExecutionResult,
	spec StepSpec,
	seq int,
	products map[domain.StepPayloadKind]any,
	attempt int,
) (domain.Step, any, error) {
	started := e.now()
	step := domain.Step{
		RunID:     result.Run.RunID,
		StepName:  spec.Name,
		Seq:       seq,
		Attempt:   attempt,
		State:     domain.StepStateRunning,
		StartedAt: started,
	}
	e.logger.Info("run_step_started",
		"run_id", step.RunID, "step_name", spec.Name, "seq", seq, "attempt", attempt)

	product, err := spec.Handler(ctx, StepInput{
		Run:      result.Run,
		Seq:      seq,
		StepName: spec.Name,
		Products: products,
	})
	ended := e.now()
	step.EndedAt = &ended

	if err != nil {
		return e.failStep(ctx, step, err)
	}
	// 形状自检：这一步的结果分码必须已登记，载荷类型就是登记的那种。
	// 没登记的结果类型明确拒绝，不让它混进库里——下游按分码取产物时，
	// 一份没人认领形状的产物只会读出半个结论。
	if e.shapes != nil {
		if err := e.shapes.CheckProduct(spec.PayloadKind, product); err != nil {
			return e.failStep(ctx, step, err)
		}
	}
	// 产物与分码对不上时，这一步的产出不可信：按失败处理，不硬写进库。
	if err := step.SetPayload(spec.PayloadKind, product); err != nil {
		return e.failStep(ctx, step, err)
	}
	step.State = domain.StepStateSucceeded
	if err := e.persist(ctx, step); err != nil {
		return e.failStep(ctx, step, err)
	}
	if spec.OnSuccess != nil {
		// 状态推进被拒就是这一步没成：把错误原样抛回，让 run 落失败态，
		// 绝不「推进失败就当没这回事」地继续往下走。
		if err := spec.OnSuccess(&result.Run, product); err != nil {
			return e.failStep(ctx, step, err)
		}
	}
	// run 状态随每一步推进实时落库：状态不落库，进程重启后就认不出
	// 「这条判定跑到哪一步了」，续跑也就无从谈起。
	if err := e.persistRun(ctx, result); err != nil {
		return e.failStep(ctx, step, err)
	}
	return step, product, nil
}

// failStep 把失败步骤落库并返回错误。
//
// 失败步骤照样落一行：落库本身也失败时，把存储错误并进步骤记录里，
// 但绝不因为「记不下来」就把这步的错误吞掉。
func (e *Executor) failStep(ctx context.Context, step domain.Step, cause error) (domain.Step, any, error) {
	ecode := errcode.As(cause)
	step.State = domain.StepStateFailed
	step.ErrorCode = string(ecode.Code)
	step.Error = ecode.Error()
	step.PayloadKind = ""
	step.Payload = nil

	if err := e.persist(ctx, step); err != nil {
		step.Error = step.Error + "；落库失败：" + err.Error()
		e.logger.Error("run_step_persist_failed",
			"run_id", step.RunID, "step_name", step.StepName, "error", err.Error())
	}
	e.logger.Error("run_step_failed",
		"run_id", step.RunID, "step_name", step.StepName,
		"error_code", string(ecode.Code), "error", ecode.Details)
	return step, nil, cause
}

// failRun 把 run 落到失败态，并记下失败步骤与错误码。
func (e *Executor) failRun(result *ExecutionResult, stepName string, cause error) error {
	if err := result.Run.Transition(domain.ExecStateFailed); err != nil {
		e.logger.Error("run_state_transition_failed",
			"run_id", result.Run.RunID, "error", err.Error())
	}
	ecode := errcode.As(cause)
	result.FailedStep = stepName
	result.ErrorCode = ecode.Code
	result.ErrorMessage = ecode.Error()
	// 失败态也要落库：重启后扫到这条 run 时，要能一眼看出「它停在哪一步、
	// 为什么停」，而不是看见一条还在排队的活。
	if err := e.persistRun(context.Background(), result); err != nil {
		e.logger.Error("run_state_persist_failed",
			"run_id", result.Run.RunID, "error", err.Error())
	}
	e.logger.Error("run_step_chain_failed",
		"run_id", result.Run.RunID, "step_name", stepName,
		"error_code", string(ecode.Code), "error", ecode.Details)
	return cause
}

// persistRun 把 run 的当前状态落库。
//
// 写的是整条快照：库里那一份永远是这条 run 的当前全貌，
// 不存在「步骤推进了但执行态没跟上」这种半截状态。
func (e *Executor) persistRun(ctx context.Context, result *ExecutionResult) error {
	if e.runRecorder == nil {
		return nil
	}
	rec := domain.RunRecord{
		Run:        result.Run,
		Profile:    e.profile,
		FailedStep: result.FailedStep,
		ErrorCode:  string(result.ErrorCode),
		UpdatedAt:  e.now(),
	}
	if err := e.runRecorder.UpsertRun(ctx, rec); err != nil {
		ecode := errcode.As(err)
		return errcode.Newf(ecode.Code, "判定 run 状态落库失败：%s", ecode.Details)
	}
	return nil
}

// persist 把步骤记录落库。
func (e *Executor) persist(ctx context.Context, step domain.Step) error {
	if e.recorder == nil {
		return nil
	}
	if err := e.recorder.InsertRunStep(ctx, step); err != nil {
		ecode := errcode.As(err)
		return errcode.Newf(ecode.Code,
			"步骤产物落库失败（步骤 %s）：%s", step.StepName, ecode.Details)
	}
	return nil
}
