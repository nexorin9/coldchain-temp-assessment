// 本文件落「药学部主管确认 / 否决处置结论」这一段：谁能确认、从哪个状态能确认、
// 确认后走到哪一步、留什么痕，以及——最要紧的一条——**未经确认不得写回台账**。
//
// 为什么确认这件事要单独一个文件，而不是塞进状态机：状态机只回答「这条边存不存在」，
// 确认动作要回答的是另一组问题——
//   - 这次确认是谁做的（经办不能替主管签）；
//   - 处置结论取哪个值（取值域之外一律拒绝，不自己编一个）；
//   - 确认之后台账里写的是什么（载荷必须先固化，不能边写边算）。
//
// 三条纪律贯穿本文件：
//
//  1. **只有主管能确认**。角色闸门在这里再查一遍（Authorize），不依赖调用方
//     已经查过——越权确认是把一份没人负责的结论写进药事台账，
//     这类错事后没人认领，也补不回来。
//  2. **确认只到「已确认」为止**。确认之后要不要写台账、写了什么，
//     是出站写回侧的事；本文件只把状态推到 confirmed 并把留痕留下。
//  3. **未确认不得写回**。AuthorizeWriteback 是写回侧唯一的放行口：
//     run 不到 confirmed，出一站调用都算越权。这道闸门由写回侧二次校验，
//     不靠「记得先确认」这种约定。
package workflow

import (
	"context"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/disposition"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// 确认动作的原因码。
//
// 原因码是机器可判读的：审计与飞检问「这条判定为什么到了已确认」，
// 答案应当是一行代码，而不是一段需要人去读的中文。
const (
	// ReasonSupervisorConfirmed：药学部主管确认了处置结论。
	ReasonSupervisorConfirmed = "supervisor_confirmed"
	// ReasonSupervisorRejected：药学部主管否决了这条判定。
	ReasonSupervisorRejected = "supervisor_rejected"
)

// ConfirmRequest 是一次确认 / 否决的请求。
//
// 它只带「人的事实」：谁做的、处置是什么、意见是什么、什么时候做的。
// 载荷（写回台账的内容）不在这里——那是成稿层与确认编排层的事，
// 状态机不该知道台账的字段长什么样。
type ConfirmRequest struct {
	// RunID 是被确认 / 否决的判定 run。
	RunID string
	// Actor 是执行动作的人（药学部主管标识）。
	Actor string
	// Role 是发起人的身份；闸门据此判断能不能确认。
	Role Role
	// Disposition 是处置结论取值（确认时必填）。
	//
	// 取值域见 internal/disposition。域外取值一律拒绝：处置结论写错一个字，
	// 批次就可能被误放行，所以宁可报错也不猜。
	Disposition string
	// Opinion 是意见（可选）；写进留痕与审计链。
	Opinion string
	// At 是动作时刻；零值取当前时间。
	At time.Time
}

// ConfirmResult 是一次确认 / 否决的结果。
//
// 它回的是「这条判定现在处于什么状态、由谁在什么时候定成了什么」，
// 也就是内审与飞检要的那几行字。
type ConfirmResult struct {
	// RunID 是被确认 / 否决的判定 run。
	RunID string
	// FromState 是动作前的业务状态。
	FromState State
	// ToState 是动作后的业务状态。
	ToState State
	// Disposition 是最终处置结论；否决时为空。
	Disposition string
	// DispositionLabel 是处置结论的中文短标签。
	DispositionLabel string
	// ConfirmedBy 是执行动作的人。
	ConfirmedBy string
	// ConfirmedAt 是动作时刻。
	ConfirmedAt time.Time
	// Opinion 是意见。
	Opinion string
	// Transition 是本次状态迁移的留痕。
	Transition Transition
	// Request 是本次推进用的状态推进请求。
	//
	// 它随结果一起给出，是为了让落库方用**同一份**请求去推进 run 状态：
	// 留痕、状态与这次决策必须出自同一次判断，不能各算一遍。
	Request Request
}

// Confirm 推进到「已确认」。
//
// 五道检查，顺序有意：
//  1. 身份缺失或不是主管 → E_ROLE_REQUIRED（403）。
//     先查身份再查状态：越权请求不该知道这条 run 停在哪个状态。
//  2. run_id 为空 → E_VALIDATION。
//  3. 确认人为空 → E_VALIDATION。确认动作必须能落到具体的人身上，
//     「药学部」三个字不算确认人。
//  4. 处置取值不在域内 → E_VALIDATION。
//  5. 状态迁移合法性 → E_ILLEGAL_TRANSITION（409）。
//     迁移表说「待确认 → 已确认」这条边存在，这条边只认 confirm 来源。
func Confirm(current State, req ConfirmRequest) (ConfirmResult, error) {
	if err := Authorize(req.Role, PermConfirm); err != nil {
		return ConfirmResult{}, err
	}
	if strings.TrimSpace(req.RunID) == "" {
		return ConfirmResult{}, errcode.Newf(errcode.Validation, "确认动作缺少 run_id")
	}
	if strings.TrimSpace(req.Actor) == "" {
		return ConfirmResult{}, errcode.Newf(errcode.Validation,
			"确认动作缺少确认人：处置结论必须能落到具体的人身上，不接受空署名")
	}
	if err := disposition.Validate(req.Disposition); err != nil {
		return ConfirmResult{}, err
	}

	at := req.At
	if at.IsZero() {
		at = time.Now()
	}
	stateReq := Request{
		To:     StateConfirmed,
		Action: ActionConfirm,
		Reason: ReasonSupervisorConfirmed,
		Note:   req.Opinion,
		Actor:  req.Actor,
		At:     at,
	}
	to, transition, err := Advance(current, stateReq)
	if err != nil {
		return ConfirmResult{}, err
	}

	label, _ := disposition.Label(req.Disposition)
	return ConfirmResult{
		RunID:            req.RunID,
		FromState:        current,
		ToState:          to,
		Disposition:      req.Disposition,
		DispositionLabel: label,
		ConfirmedBy:      strings.TrimSpace(req.Actor),
		ConfirmedAt:      at,
		Opinion:          req.Opinion,
		Transition:       transition,
		Request:          stateReq,
	}, nil
}

// Reject 推进到「已否决」。
//
// 否决不需要处置结论：主管说「这条不该这么判」本身就是结论。
// 也不需要载荷——被否决的判定不写台账，写了反倒让台账多一条没发生过的越温。
//
// 迁移表里 assessed 与 awaiting_confirmation 都能到 rejected：
// 结论刚出就被否决，与成稿后被否决，都是主管说了算。
func Reject(current State, req ConfirmRequest) (ConfirmResult, error) {
	if err := Authorize(req.Role, PermConfirm); err != nil {
		return ConfirmResult{}, err
	}
	if strings.TrimSpace(req.RunID) == "" {
		return ConfirmResult{}, errcode.Newf(errcode.Validation, "否决动作缺少 run_id")
	}
	if strings.TrimSpace(req.Actor) == "" {
		return ConfirmResult{}, errcode.Newf(errcode.Validation,
			"否决动作缺少否决人：结论必须能落到具体的人身上，不接受空署名")
	}

	at := req.At
	if at.IsZero() {
		at = time.Now()
	}
	stateReq := Request{
		To:     StateRejected,
		Action: ActionReject,
		Reason: ReasonSupervisorRejected,
		Note:   req.Opinion,
		Actor:  req.Actor,
		At:     at,
	}
	to, transition, err := Advance(current, stateReq)
	if err != nil {
		return ConfirmResult{}, err
	}

	return ConfirmResult{
		RunID:       req.RunID,
		FromState:   current,
		ToState:     to,
		ConfirmedBy: strings.TrimSpace(req.Actor),
		ConfirmedAt: at,
		Opinion:     req.Opinion,
		Transition:  transition,
		Request:     stateReq,
	}, nil
}

// AuthorizeWriteback 判断这条判定 run 现在能不能触发出站写回调用。
//
// 这是「未确认不得写回」这条硬约束的判定口，也是写回侧唯一的放行口：
// run 不到 confirmed，任何出站写回调用都算越权。
//
// 放行两种状态：confirmed（刚确认，等写回）与 pending_writeback
// （写回没落成，等补偿重放）——后者是同一批货的第二次尝试，
// 不是第二次确认，重放时载荷与幂等键都照原样用。
//
// 报错时点明当前状态与它意味着什么：调用方一眼能看出「这条还没人确认」。
func AuthorizeWriteback(state State) error {
	switch state {
	case StateConfirmed, StatePendingWriteback:
		return nil
	default:
		return errcode.Newf(errcode.IllegalTransition,
			"判定 run 当前业务状态为 %q（%s），未经药学部主管确认，不得触发出站写回",
			state, Describe(state))
	}
}

// OutboundWriter 是出站写回的调用口。
//
// 载荷以固化后的 JSON 原文交付：写回侧发出去的必须是主管确认那一刻定下的内容，
// 不能在写回时重新算一遍——重新算就等于「台账里那条是另一份结论」。
type OutboundWriter interface {
	// WriteDeviation 把一条越温偏差写回冷链台账。
	WriteDeviation(ctx context.Context, runID string, payloadJSON string) error
}

// RunStateReader 读一条判定 run 的业务状态。
//
// 它回的是库里那一列的原始字符串：本包不要求存储层把状态存成 workflow.State，
// 只要求读回来的值能被本包认出来——认不出来就是脏数据，闸门会挡住。
type RunStateReader interface {
	// RunState 回读一条 run 的业务状态；run 不存在时返回 E_NOT_FOUND。
	RunState(ctx context.Context, runID string) (string, error)
}

// WritebackGate 是「未确认不得写回」这道闸门：出站写回必须经它。
//
// 它存在的理由是「约定靠不住」：约定说「先确认再写回」，可写回有重试、
// 有补偿重放、有重启后续跑——每一条路径都可能绕开人的记性。
// 把检查写进代码里，绕开它就得改代码，改代码就会有人看见。
//
// 闸门只做一件事：读出这条 run 当前的业务状态，问一句能不能写。
// 不能写就一次出站调用都不发——不是发了再补救。
type WritebackGate struct {
	writer OutboundWriter
	states RunStateReader
}

// NewWritebackGate 装配出站写回闸门。
func NewWritebackGate(writer OutboundWriter, states RunStateReader) *WritebackGate {
	return &WritebackGate{writer: writer, states: states}
}

// WriteDeviation 经闸门写回一条越温偏差。
//
// 未确认的 run 在这里被拦下，出站调用数为 0——拦在调用之前，不是调用之后。
func (g *WritebackGate) WriteDeviation(ctx context.Context, runID string, payloadJSON string) error {
	if g == nil || g.writer == nil {
		return errcode.Newf(errcode.StorageError, "出站写回未装配调用能力")
	}
	if g.states == nil {
		return errcode.Newf(errcode.StorageError, "出站写回未装配业务状态读取能力")
	}
	if strings.TrimSpace(runID) == "" {
		return errcode.Newf(errcode.Validation, "出站写回缺少 run_id")
	}
	state, err := g.states.RunState(ctx, runID)
	if err != nil {
		return err
	}
	if err := AuthorizeWriteback(State(state)); err != nil {
		return err
	}
	return g.writer.WriteDeviation(ctx, runID, payloadJSON)
}
