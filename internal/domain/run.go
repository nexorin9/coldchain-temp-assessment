package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// 判定 run 的业务状态。
//
// 状态的定义与合法迁移表只有一处：internal/workflow。这里保留的是同源别名，
// 方便判定产物、台账载荷与接口响应直接引用——值不再各写一份，
// 免得两处定义悄悄跑偏（写台账的状态名与状态机认的状态名对不上，
// 是那种上线三个月后才查得出来的错）。
//
// 九态：detected（已建判定）/ assessed（已出结论）/ awaiting_confirmation（待确认）/
// confirmed（主管已确认）/ pending_writeback（写回待补偿）/ written_back（已写回台账）/
// rejected（已否决）/ needs_manual（判不了，转人工）/ failed（判定失败）。
const (
	// RunStateDetected：事件已入库、判定 run 已建，尚未给出结论。
	RunStateDetected = string(workflow.StateDetected)
	// RunStateAssessed：已给出可核对的判定结论。
	// 最窄路径只到窗口层；完整链路的 assessed 还需叠加归因与成稿。
	RunStateAssessed = string(workflow.StateAssessed)
	// RunStateAwaitingConfirmation：判定结论已出，停在待药学部主管确认。
	//
	// 停在这里是刻意的：模型产出只到草稿，批次能不能用由疫苗生产企业评估
	// 质量影响、再由主管确认。系统不自己把结论推进到「已处置」。
	RunStateAwaitingConfirmation = string(workflow.StateAwaitingConfirmation)
	// RunStateConfirmed：药学部主管已确认处置结论。
	RunStateConfirmed = string(workflow.StateConfirmed)
	// RunStatePendingWriteback：已确认，但台账写回没落成，等补偿重放。
	RunStatePendingWriteback = string(workflow.StatePendingWriteback)
	// RunStateWrittenBack：台账与通知已落库。
	RunStateWrittenBack = string(workflow.StateWrittenBack)
	// RunStateRejected：药学部主管否决了这条判定。
	RunStateRejected = string(workflow.StateRejected)
	// RunStateNeedsManual：配置缺失、点位歧义等不能自动判定的情况，须人工接手。
	RunStateNeedsManual = string(workflow.StateNeedsManual)
	// RunStateFailed：判定最终失败（步骤重试已耗尽）。
	RunStateFailed = string(workflow.StateFailed)
)

// ExecState 是判定 run 的执行态，回答的是「这条判定跑到哪一步了」。
//
// 它与业务状态（Run.State：detected / assessed / …）是两回事：
// 执行态描述调度与执行器里的进度，业务状态描述结论走到了哪一步。
// 两者各走各的迁移表，谁也不能借推进另一条状态机来「顺手」改掉对方。
type ExecState string

// 执行态取值。
const (
	// ExecStateQueued：已建 run、已入队，尚未开始执行。
	ExecStateQueued ExecState = "queued"
	// ExecStateRunning：正在执行步骤链。
	ExecStateRunning ExecState = "running"
	// ExecStateSucceeded：步骤链全部执行成功。
	ExecStateSucceeded ExecState = "succeeded"
	// ExecStateFailed：步骤链最终失败（重试已耗尽）。
	ExecStateFailed ExecState = "failed"
	// ExecStateRetrying：某步失败、正在退避等待重试。
	ExecStateRetrying ExecState = "retrying"
)

// execTransitions 是执行态的合法迁移表。
//
// 用白名单而不是黑名单：没列出来的迁移一律拒绝。这样将来新增状态时，
// 不会出现「忘了补一条判断就悄悄放行」的情况——迁移表就是全部真相。
var execTransitions = map[ExecState][]ExecState{
	ExecStateQueued:    {ExecStateRunning, ExecStateFailed},
	ExecStateRunning:   {ExecStateSucceeded, ExecStateFailed, ExecStateRetrying},
	ExecStateRetrying:  {ExecStateRunning, ExecStateFailed},
	ExecStateSucceeded: {},
	ExecStateFailed:    {},
}

// execStateOrder 是执行态的固定展示顺序，保证摘要与时间线输出是确定的。
var execStateOrder = []ExecState{
	ExecStateQueued,
	ExecStateRunning,
	ExecStateRetrying,
	ExecStateSucceeded,
	ExecStateFailed,
}

// AllExecStates 返回全部执行态，按固定顺序。
func AllExecStates() []ExecState {
	out := make([]ExecState, len(execStateOrder))
	copy(out, execStateOrder)
	return out
}

// ExecStateTransitions 返回 from 允许迁移到的状态列表（副本，改它不影响迁移表）。
func ExecStateTransitions(from ExecState) []ExecState {
	next := execTransitions[from]
	out := make([]ExecState, len(next))
	copy(out, next)
	return out
}

// CanExecTransition 判断从 from 迁移到 to 是否合法。
func CanExecTransition(from, to ExecState) bool {
	for _, allowed := range execTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// 写回状态：一条 run 的台账写入走到哪一步了。
//
// 单独一档而不是塞进业务状态机：写回是「出站调用有没有落成」的记账，
// 业务状态是「结论走到哪一步了」，两者的推进者不同（前者由写回器推进，
// 后者由主管确认推进）。混在一起就会出现「业务状态已确认但台账没写进去」
// 这种说不清的状态。
const (
	// WritebackStateNone：还没有走到写回这一步。
	WritebackStateNone = ""
	// WritebackStatePending：已确认、等待写回（含补偿重放待处理）。
	WritebackStatePending = "pending"
	// WritebackStateWrittenBack：台账写入已落成，且已记下写入时刻。
	//
	// 它是「不重复写回」的唯一凭据：进程重启后扫到这条记录，
	// 就不会再对同一个 external_ref 再写一次台账。
	WritebackStateWrittenBack = "written_back"
	// WritebackStateFailed：写回尝试失败且已耗尽重试，等人工或补偿重放。
	WritebackStateFailed = "failed"
)

// IsTerminalExecState 判断执行态是否为终态。
//
// 终态的 run 不再续跑：要么步骤链跑完了，要么最终失败了。
// 「跑完了」不代表「事情做完了」——它停在待主管确认，等的是人，不是进程。
func IsTerminalExecState(state ExecState) bool {
	return state == ExecStateSucceeded || state == ExecStateFailed
}

// Run 是一次越温判定。
//
// 一条温度事件推进一条 run：run 是判定过程的容器，窗口结论、归因结论、
// 成稿草稿、确认动作与写回记录都挂在这条 run 上。
//
// 字段名的取舍：run_id 由服务端生成、不可由调用方指定；external_ref 稳定可重放
// （同一点位 + 同一窗口起点 → 同一个值），供出站台账按业务主键识别重复入账；
// idempotency_key 是窗口幂等键，同窗去抖时用它判断「这条事件是不是同一个窗口」。
type Run struct {
	RunID       string     `json:"run_id"`
	PointID     string     `json:"point_id"`
	WindowStart *time.Time `json:"window_start,omitempty"`
	WindowEnd   *time.Time `json:"window_end,omitempty"`
	// State 是业务状态（见 RunState* 常量）。
	//
	// 只能经 AdvanceState 改写：合法迁移表、终态保护与迁移留痕都在那儿，
	// 绕开它直接赋值等于把「谁在什么时候把这条判定推到了哪一步」弄丢了。
	State          string `json:"state"`
	ExternalRef    string `json:"external_ref,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// ExecState 是执行态。空值表示 run 还没入队，Transition 会拒绝把它当
	// 任何已知状态处理——空执行态不是「queued」的别名。
	ExecState ExecState `json:"exec_state,omitempty"`
	// Degraded 表示这条判定的归因结论是降级得来的：模型归因不可用，
	// 结论由规则基线单路给出，未经模型交叉印证。
	//
	// 它挂在 run 上而不只挂在归因产物上，是为了让人不必翻步骤产物就能看出
	// 「这条结论的成色」——查一条判定的人第一眼看的就是 run 的状态。
	// DegradeReason 是降级原因的错误码（如 E_MODEL_UNAVAILABLE）。
	Degraded bool `json:"degraded,omitempty"`
	// DegradeReason 是降级原因的错误码；未降级时为空。
	DegradeReason string `json:"degrade_reason,omitempty"`
	// StateTrail 是业务状态迁移留痕：每次状态推进留一条（从哪到哪、由什么触发、
	// 为什么、谁做的、什么时候）。
	//
	// 它回答的是「这条判定是怎么一步步走到今天这个状态的」——
	// 飞检与内审问的「谁确认的、什么时候确认的」，答案就在这条链上。
	StateTrail []workflow.Transition `json:"state_trail,omitempty"`
}

// AdvanceState 推进判定 run 的业务状态——业务状态唯一的写入口。
//
// 规则（合法迁移表、终态保护、触发来源限制）都在 internal/workflow 里，
// 这里只负责把结果落到 run 上并留下痕：任何绕过 AdvanceState 直接改
// State 的写法，都会让这条留痕断掉——「状态推进了却查不到为什么推进」
// 是比状态推不动更难查的错。
//
// 推进被拒时**不改**状态，并原样带回错误（E_ILLEGAL_TRANSITION / E_VALIDATION）。
func (r *Run) AdvanceState(req workflow.Request) (workflow.Transition, error) {
	to, transition, err := workflow.Advance(workflow.State(r.State), req)
	if err != nil {
		return workflow.Transition{}, err
	}
	r.State = string(to)
	r.StateTrail = append(r.StateTrail, transition)
	return transition, nil
}

// EnsureState 把 run 推进到目标状态；已经在该状态时按幂等空操作处理。
//
// 它解决的是「同一步被跑了两遍」这件事：窄判定路径先给出结论（assessed），
// 完整步骤链随后又把抽窗口那一步跑了一遍——结论一样，状态也已经在目标态，
// 再推进一次就成了「从 assessed 到 assessed」。那不是非法迁移，是同一步重做了一遍；
// 把它当非法迁移报错，等于让「先判后跑链」这条正常用法直接失败。
//
// 空操作不留痕：状态没变，迁移链上就不该多出一条记录。
func (r *Run) EnsureState(req workflow.Request) (workflow.Transition, error) {
	if workflow.State(r.State) == req.To {
		return workflow.Transition{}, nil
	}
	return r.AdvanceState(req)
}

// NewRun 建一条判定 run：run_id 由服务端生成，执行态落在 queued。
//
// run_id 不由调用方指定：它是服务内部的追溯锚点，调用方拿着它查 run、看时间线、
// 做确认与写回补偿。
func NewRun(pointID string) Run {
	return Run{
		RunID:     NewRunID(),
		PointID:   pointID,
		State:     RunStateDetected,
		ExecState: ExecStateQueued,
	}
}

// NewRunID 生成服务端侧唯一的判定 run 标识。
func NewRunID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// 极低概率的熵源失败：退到时间戳，仍保证可区分，不复用。
		return fmt.Sprintf("run_%d", time.Now().UnixNano())
	}
	return "run_" + hex.EncodeToString(buf)
}

// WindowIdempotencyKey 计算窗口幂等键：sha256(point_id + window_start)。
//
// 同一点位的同一个越温窗口只应被判一次；这个键是「是不是同一个窗口」的唯一判据，
// 不能含随机成分——重复投递同一段采样必须算出同一个键。
func WindowIdempotencyKey(pointID string, windowStart time.Time) string {
	sum := sha256.Sum256([]byte(pointID + "|" + windowStart.UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(sum[:])
}

// ExternalRef 生成稳定可重放的外部引用：同一点位 + 同一窗口起点 → 同一个值。
//
// 出站台账按 external_ref 识别「同一次越温」，用它防重复入账；
// 人也看得懂：点位 + 窗口起点（UTC）直接写在里面，飞检翻台账时能对上。
func ExternalRef(pointID string, windowStart time.Time) string {
	return "cc-" + sanitizeRefToken(pointID) + "-" + windowStart.UTC().Format("20060102T150405Z")
}

// BindWindow 把窗口绑定到 run：起止时间、外部引用与幂等键一次性写齐。
//
// 三个值必须同源同算。分开写就可能出现「窗口起点换了、外部引用没换」的错位，
// 台账侧会把两次越温认成同一次，重复入账；反过来窗口没换而引用变了，
// 同一次越温会在台账里留下两条记录。
func (r *Run) BindWindow(start, end time.Time) {
	s := start
	e := end
	r.WindowStart = &s
	r.WindowEnd = &e
	r.ExternalRef = ExternalRef(r.PointID, start)
	r.IdempotencyKey = WindowIdempotencyKey(r.PointID, start)
}

// ResumeTransition 把一条中断的 run 重新置为执行中。
//
// 续跑不是一次普通的状态推进：进程被杀掉时 run 可能停在 running 或 retrying，
// 而迁移表里没有 running→running 这条边——加上它会让「状态被误推进」变得看不出来。
// 续跑是一次显式的恢复动作，因此单独给一个入口，并且只接受非终态：
// 已经跑完（succeeded）或已经判失败（failed）的 run 都不该被拉起来重跑。
func (r *Run) ResumeTransition() error {
	switch r.ExecState {
	case ExecStateQueued, ExecStateRunning, ExecStateRetrying:
		r.ExecState = ExecStateRunning
		return nil
	case ExecStateSucceeded, ExecStateFailed:
		return errcode.Newf(errcode.IllegalTransition,
			"run %s 的执行态已是终态 %q，不能续跑", r.RunID, r.ExecState)
	case "":
		return errcode.Newf(errcode.IllegalTransition,
			"run %s 的执行态未初始化，不能续跑", r.RunID)
	default:
		return errcode.Newf(errcode.IllegalTransition,
			"run %s 的执行态 %q 未登记，不能续跑", r.RunID, r.ExecState)
	}
}

// RunRecord 是判定 run 的持久化快照。
//
// 它比 Run 多三样东西，都是「只有落盘之后才答得上来的问题」：
//   - Profile：这条判定用的点位温区配置。续跑时靠它重建同一条判定链——
//     温区变了，重跑出来的结论就换了口径，等于把一条判定改成另一条。
//   - FailedStep / ErrorCode：停在哪一步、为什么停。
//   - WritebackState / WrittenBackAt：台账写回走到哪一步、什么时候写成的。
//     写回是「不重复入账」的唯一凭据，进程重启后靠它认出「这条已经写过台账了」。
type RunRecord struct {
	// Run 是 run 本体。
	Run Run `json:"run"`
	// Profile 是这条判定所用的点位温区配置；为 nil 表示没有可复原的判定依据。
	Profile *PointProfile `json:"profile,omitempty"`
	// FailedStep 是最终失败的那一步；全部成功时为空。
	FailedStep string `json:"failed_step,omitempty"`
	// ErrorCode 是失败步骤的错误码；全部成功时为空。
	ErrorCode string `json:"error_code,omitempty"`
	// WritebackState 是台账写回状态（未写 / 待写 / 已写回 / 写回失败）。
	WritebackState string `json:"writeback_state,omitempty"`
	// WrittenBackAt 是台账写入落成的时刻。
	WrittenBackAt *time.Time `json:"written_back_at,omitempty"`
	// CreatedAt 是这条 run 第一次落库的时刻；后续更新不覆盖它。
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt 是最近一次落库的时刻。
	UpdatedAt time.Time `json:"updated_at"`
}

// Transition 把 run 的执行态推进到下一态。
//
// 非法迁移一律返回 E_ILLEGAL_TRANSITION，并且**不改**当前状态：
// 状态被误推进比状态推不动危险得多——被判失败的 run 若被悄悄标成成功，
// 后面就会带着一份不存在的结论去写台账。
func (r *Run) Transition(to ExecState) error {
	from := r.ExecState
	if from == "" {
		return errcode.Newf(errcode.IllegalTransition,
			"run %s 的执行态未初始化，不能迁移到 %q", r.RunID, to)
	}
	if _, known := execTransitions[from]; !known {
		return errcode.Newf(errcode.IllegalTransition,
			"run %s 的执行态 %q 未登记，不能迁移到 %q", r.RunID, from, to)
	}
	if !CanExecTransition(from, to) {
		return errcode.Newf(errcode.IllegalTransition,
			"run %s 不能从执行态 %q 迁移到 %q（合法目标：%v）", r.RunID, from, to, ExecStateTransitions(from))
	}
	r.ExecState = to
	return nil
}

// sanitizeRefToken 把点位标识清洗成可放进外部引用的字符集。
// 只保留字母、数字、下划线与连字符，其余一律换成短横线，避免台账侧因特殊字符拒收。
func sanitizeRefToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	if out == "" {
		return "point"
	}
	return out
}
