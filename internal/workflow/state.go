// Package workflow 落判定 run 的业务状态机：九个业务状态、合法迁移表、
// 状态推进的唯一入口与迁移留痕。
//
// 为什么单独一个包：一条判定 run 身上有两条状态线——
//   - 执行态（跑到哪一步了）：在 domain 里，调度、限并发与重启续跑要用它；
//   - 业务状态（结论走到哪一步了）：在这里。
//
// 两条线各走各的迁移表，谁也不能借推进另一条状态机来「顺手」改掉对方。
// 业务状态单独拎出来，是因为它的推进者只有人：模型产出最多到「待确认」，
// 再往后（已确认 / 已写回）只能由药学部主管的确认动作推进。
//
// 三条纪律：
//   - 白名单迁移：合法边写死在一张表里，没列出来的迁移一律拒绝。
//     状态被误推进比状态推不动危险得多——被判失败的 run 若被悄悄标成成功，
//     后面就会带着一份不存在的结论去写冷链台账。
//   - 唯一入口：业务状态只经 Advance 改写，每次迁移都必须带原因码与备注，
//     留痕可回溯（「谁在什么时候把这条判定推到了哪一步、为什么」）。
//   - 终态保护：rejected / needs_manual / failed / written_back 是终态，
//     进入后拒绝任何迁移——一条判定的结论一旦落定就不再改动。
package workflow

import (
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// State 是判定 run 的业务状态。
//
// 取值是九个固定字符串：写进冷链台账、出现在接口响应与审计链里，
// 不用枚举序号、不用大小写混写的变体——飞检时对着台账逐字核。
type State string

// 九个业务状态。
const (
	// StateDetected：温度事件已入库、判定 run 已建，尚未给出结论。
	StateDetected State = "detected"
	// StateAssessed：已给出可核对的判定结论（越温窗口与原因结论都在手）。
	StateAssessed State = "assessed"
	// StateAwaitingConfirmation：结论已成稿，等待药学部主管确认。
	//
	// 停在这里是刻意的：模型产出只到草稿，批次能不能用由疫苗生产企业评估
	// 质量影响、再由主管确认。系统不自己把结论推进到「已处置」。
	StateAwaitingConfirmation State = "awaiting_confirmation"
	// StateConfirmed：药学部主管已确认处置结论。
	StateConfirmed State = "confirmed"
	// StatePendingWriteback：已确认，但台账写回没落成，等补偿重放。
	StatePendingWriteback State = "pending_writeback"
	// StateWrittenBack：台账与通知已落库。
	StateWrittenBack State = "written_back"
	// StateRejected：药学部主管否决了这条判定。
	StateRejected State = "rejected"
	// StateNeedsManual：温区缺失、点位歧义等不能自动判定的情况，须人工接手。
	StateNeedsManual State = "needs_manual"
	// StateFailed：判定最终失败（步骤重试已耗尽）。
	StateFailed State = "failed"
)

// ActorSystem 是自动推进（判定链、规则基线）时记在留痕里的执行者标识。
//
// 自动推进也要留痕：飞检问「这条结论什么时候算出来的、谁算的」，
// 答「系统自动判定」比答空着强。
const ActorSystem = "system"

// stateDescriptions 是九个业务状态的中文含义，用于错误文案与接口输出。
var stateDescriptions = map[State]string{
	StateDetected:             "事件已入库、判定已建，尚未给出结论",
	StateAssessed:             "已给出可核对的判定结论",
	StateAwaitingConfirmation: "结论已成稿，等待药学部主管确认",
	StateConfirmed:            "主管已确认处置结论",
	StatePendingWriteback:     "已确认，台账写回待补偿重放",
	StateWrittenBack:          "台账与通知已落库",
	StateRejected:             "主管已否决这条判定",
	StateNeedsManual:          "无法自动判定，须人工接手",
	StateFailed:               "判定最终失败",
}

// stateOrder 是九个状态的固定展示顺序：按判定链路从前往后排，
// 而不是按字母序——回看一条判定时，人想知道的是它走到哪一步了。
var stateOrder = []State{
	StateDetected,
	StateAssessed,
	StateAwaitingConfirmation,
	StateConfirmed,
	StatePendingWriteback,
	StateWrittenBack,
	StateRejected,
	StateNeedsManual,
	StateFailed,
}

// legalTransitions 是业务状态的合法迁移表（白名单）。
//
// 用白名单而不是黑名单：没列出来的迁移一律拒绝。这样将来新增状态时，
// 不会出现「忘了补一条判断就悄悄放行」的情况——迁移表就是全部真相。
//
// 迁移表的形状来自业务事实，不是随手画的：
//   - detected：刚建 run，还没出结论；判不出来就转人工，跑不动就落失败。
//   - assessed：结论已出但还没成稿待确认；此阶段也可以直接被否决。
//   - awaiting_confirmation：等人确认；确认或否决，没有第三条路。
//   - confirmed：人已经认了；接下来只有台账写回这一件事，写不成就挂起等补偿。
//   - pending_writeback：写回没落成，补偿重放成功后补上 written_back。
//   - 其余四个是终态，不再接受任何迁移。
var legalTransitions = map[State][]State{
	StateDetected:             {StateAssessed, StateNeedsManual, StateFailed},
	StateAssessed:             {StateAwaitingConfirmation, StateRejected},
	StateAwaitingConfirmation: {StateConfirmed, StateRejected},
	StateConfirmed:            {StateWrittenBack, StatePendingWriteback},
	StatePendingWriteback:     {StateWrittenBack},
	StateWrittenBack:          {},
	StateRejected:             {},
	StateNeedsManual:          {},
	StateFailed:               {},
}

// terminalStates 是终态集合：进入后拒绝任何迁移。
//
// 与 legalTransitions 里空目标列表重复登记一次，是为了让「什么是终态」
// 这件事有个独立的名字可查——回看代码时不必去数空切片。
var terminalStates = map[State]bool{
	StateWrittenBack: true,
	StateRejected:    true,
	StateNeedsManual: true,
	StateFailed:      true,
}

// Action 是一次状态推进的触发来源。
//
// 它回答的是「这次迁移是谁发起的」，不是「谁有权限发起」——
// 权限那层是身份与角色的事（见角色闸门）。两者分开，是因为
// 「模型产出不能自己确认」和「经办不能替主管确认」是两个问题。
type Action string

// 状态推进的触发来源。
const (
	// ActionAuto：判定链自动推进（模型产出、规则结论、温区闸门转人工）。
	//
	// 它最多只能把判定推到「待确认」为止：模型说批次能用不算数，
	// 批次能不能用由疫苗生产企业评估质量影响、再由主管确认决定。
	ActionAuto Action = "auto"
	// ActionConfirm：药学部主管确认处置结论。
	ActionConfirm Action = "confirm"
	// ActionReject：药学部主管否决这条判定。
	ActionReject Action = "reject"
	// ActionWriteback：台账写回落成，或写回失败后挂起待补偿。
	ActionWriteback Action = "writeback"
	// ActionCompensate：台账写回补偿重放。
	ActionCompensate Action = "compensate"
)

// actionDescriptions 是触发来源的中文含义。
var actionDescriptions = map[Action]string{
	ActionAuto:       "判定链自动推进",
	ActionConfirm:    "药学部主管确认",
	ActionReject:     "药学部主管否决",
	ActionWriteback:  "台账写回",
	ActionCompensate: "台账写回补偿重放",
}

// actionOrder 是触发来源的固定展示顺序。
var actionOrder = []Action{
	ActionAuto,
	ActionConfirm,
	ActionReject,
	ActionWriteback,
	ActionCompensate,
}

// allowedTargets 是每个触发来源允许推进到的目标状态。
//
// 这层限制比迁移表更严：迁移表说「哪些边存在」，这一层说「这条边由谁走」。
// 模型产出（ActionAuto）永远走不到 confirmed / rejected / written_back——
// 那三个状态只认人的确认与台账的写回。
var allowedTargets = map[Action][]State{
	ActionAuto:       {StateAssessed, StateAwaitingConfirmation, StateNeedsManual, StateFailed},
	ActionConfirm:    {StateConfirmed},
	ActionReject:     {StateRejected},
	ActionWriteback:  {StateWrittenBack, StatePendingWriteback},
	ActionCompensate: {StateWrittenBack, StatePendingWriteback},
}

// Transition 是一次业务状态迁移的留痕。
//
// 每次迁移留一条：谁（Actor）、什么时候（At）、从哪到哪（From/To）、
// 由什么触发（Action）、为什么（Reason）、补注（Note）。
// 复核一条判定时，这串留痕就是「这条结论是怎么一步步走过来的」的答案。
type Transition struct {
	// From 是迁移前的业务状态。
	From State `json:"from"`
	// To 是迁移后的业务状态。
	To State `json:"to"`
	// Action 是触发来源（auto / confirm / reject / writeback / compensate）。
	Action Action `json:"action"`
	// Reason 是原因码：机器可判读，如 excursion_window_extracted、supervisor_confirmed。
	Reason string `json:"reason"`
	// Note 是补注：人看得懂的一句话说明。
	Note string `json:"note,omitempty"`
	// Actor 是执行者标识（自动推进记 system，人工动作记经办 / 主管标识）。
	Actor string `json:"actor,omitempty"`
	// At 是迁移时刻。
	At time.Time `json:"at"`
}

// Request 是一次状态推进的请求。
type Request struct {
	// To 是目标状态。
	To State
	// Action 是触发来源。
	Action Action
	// Reason 是原因码；必填——没有原因码的迁移不许发生。
	Reason string
	// Note 是补注；可空。
	Note string
	// Actor 是执行者标识；可空。
	Actor string
	// At 是迁移时刻；零值取当前时间（测试可注入固定时刻）。
	At time.Time
}

// Advance 推进判定 run 的业务状态——业务状态唯一的写入口。
//
// 返回新的状态与本次迁移的留痕；不合法时返回错误，且**不改**任何状态：
// 状态被误推进比状态推不动危险得多。
//
// 四道检查，顺序有意：
//  1. 当前状态未登记 → E_ILLEGAL_TRANSITION（库里读出脏数据，不给它继续走）；
//  2. 目标状态未登记 → E_VALIDATION（调用方传错了，不是迁移问题）；
//  3. 没带原因码 → E_VALIDATION（迁移必须可归因）；
//  4. 终态 → 迁移表无出边 → 触发来源不允许，三道关依次拦下。
func Advance(from State, req Request) (State, Transition, error) {
	if !Known(from) {
		return from, Transition{}, errcode.Newf(errcode.IllegalTransition,
			"判定 run 的当前业务状态未登记（%q），不能推进状态", from)
	}
	if !Known(req.To) {
		return from, Transition{}, errcode.Newf(errcode.Validation,
			"目标业务状态未登记：%q", req.To)
	}
	if strings.TrimSpace(req.Reason) == "" {
		return from, Transition{}, errcode.Newf(errcode.Validation,
			"状态推进必须带原因码：没有原因码的迁移无法归因，不予放行")
	}
	if !KnownAction(req.Action) {
		return from, Transition{}, errcode.Newf(errcode.Validation,
			"状态推进来源未登记：%q", req.Action)
	}
	if IsTerminal(from) {
		return from, Transition{}, errcode.Newf(errcode.IllegalTransition,
			"判定 run 的业务状态 %q（%s）是终态，不再接受状态迁移", from, Describe(from))
	}
	if !CanTransition(from, req.To) {
		return from, Transition{}, errcode.Newf(errcode.IllegalTransition,
			"判定 run 不能从业务状态 %q 迁移到 %q（合法目标：%v）",
			from, req.To, LegalTargets(from))
	}
	if !ActionAllows(req.Action, req.To) {
		return from, Transition{}, errcode.Newf(errcode.IllegalTransition,
			"状态推进来源 %q（%s）不能把判定 run 推进到 %q（该状态只接受：%v）",
			req.Action, DescribeAction(req.Action), req.To, ActionsFor(req.To))
	}

	at := req.At
	if at.IsZero() {
		at = time.Now()
	}
	transition := Transition{
		From:   from,
		To:     req.To,
		Action: req.Action,
		Reason: req.Reason,
		Note:   req.Note,
		Actor:  req.Actor,
		At:     at,
	}
	return req.To, transition, nil
}

// AllStates 返回全部九个业务状态，按判定链路顺序。
func AllStates() []State {
	out := make([]State, len(stateOrder))
	copy(out, stateOrder)
	return out
}

// Known 判断状态是否已登记。
func Known(state State) bool {
	_, ok := stateDescriptions[state]
	return ok
}

// Describe 返回状态的中文含义；未登记的状态返回兜底文案。
func Describe(state State) string {
	if desc, ok := stateDescriptions[state]; ok {
		return desc
	}
	return "未登记的业务状态"
}

// LegalTargets 返回 from 允许迁移到的目标状态列表（副本，改它不影响迁移表）。
func LegalTargets(from State) []State {
	next := legalTransitions[from]
	out := make([]State, len(next))
	copy(out, next)
	return out
}

// CanTransition 判断从 from 迁移到 to 是否在合法迁移表里。
//
// 只看迁移表，不看触发来源：由谁来走这条边由 ActionAllows 单独把关。
func CanTransition(from, to State) bool {
	for _, allowed := range legalTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// IsTerminal 判断状态是否为终态。
//
// 终态的判定不再改动：否决了、判不了、跑挂了、也写回台账了，
// 四种都意味着这条判定这件事已经结束了。
func IsTerminal(state State) bool {
	return terminalStates[state]
}

// AllActions 返回全部触发来源，按固定顺序。
func AllActions() []Action {
	out := make([]Action, len(actionOrder))
	copy(out, actionOrder)
	return out
}

// KnownAction 判断触发来源是否已登记。
func KnownAction(action Action) bool {
	_, ok := actionDescriptions[action]
	return ok
}

// DescribeAction 返回触发来源的中文含义；未登记的返回兜底文案。
func DescribeAction(action Action) string {
	if desc, ok := actionDescriptions[action]; ok {
		return desc
	}
	return "未登记的状态推进来源"
}

// ActionAllows 判断某个触发来源能否把判定推进到目标状态。
func ActionAllows(action Action, to State) bool {
	for _, allowed := range allowedTargets[action] {
		if allowed == to {
			return true
		}
	}
	return false
}

// ActionsFor 返回允许推进到目标状态的触发来源列表（副本）。
func ActionsFor(to State) []Action {
	out := make([]Action, 0, len(actionOrder))
	for _, action := range actionOrder {
		if ActionAllows(action, to) {
			out = append(out, action)
		}
	}
	return out
}
