package workflow

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// legalEdge 是本测试独立钉死的合法迁移边。
//
// 这里刻意不复用包内的迁移表：测试照抄实现，等于什么都没测。
// 迁移表改了而这张表没改，测试就该红——它就是那份「业务事实」的存档。
type legalEdge struct {
	from   State
	to     State
	action Action
}

// expectedEdges 是九个业务状态的完整合法迁移边（共 10 条）。
var expectedEdges = []legalEdge{
	{StateDetected, StateAssessed, ActionAuto},
	{StateDetected, StateNeedsManual, ActionAuto},
	{StateDetected, StateFailed, ActionAuto},
	{StateAssessed, StateAwaitingConfirmation, ActionAuto},
	{StateAssessed, StateRejected, ActionReject},
	{StateAwaitingConfirmation, StateConfirmed, ActionConfirm},
	{StateAwaitingConfirmation, StateRejected, ActionReject},
	{StateConfirmed, StateWrittenBack, ActionWriteback},
	{StateConfirmed, StatePendingWriteback, ActionWriteback},
	{StatePendingWriteback, StateWrittenBack, ActionCompensate},
}

// actionForTarget 给出推进到某个目标状态时该用的触发来源。
//
// 用它来构造非法边的用例：非法边要验的是「迁移表里没有这条边」，
// 而不是「这条边存在但来源不对」——后者另有专门的用例。
func actionForTarget(to State) Action {
	switch to {
	case StateConfirmed:
		return ActionConfirm
	case StateRejected:
		return ActionReject
	case StateWrittenBack, StatePendingWriteback:
		return ActionWriteback
	default:
		return ActionAuto
	}
}

// ---------------------------------------------------------------------------
// 一、合法迁移边
// ---------------------------------------------------------------------------

func TestAdvance_AllLegalEdgesAccepted(t *testing.T) {
	for _, edge := range expectedEdges {
		to, transition, err := Advance(edge.from, Request{
			To:     edge.to,
			Action: edge.action,
			Reason: "test_reason",
			Note:   "测试用迁移",
			Actor:  "tester",
		})
		if err != nil {
			t.Fatalf("合法迁移 %s → %s（%s）被拒：%v", edge.from, edge.to, edge.action, err)
		}
		if to != edge.to {
			t.Fatalf("迁移 %s → %s 后目标状态不符：实际 %s", edge.from, edge.to, to)
		}
		if transition.From != edge.from || transition.To != edge.to {
			t.Fatalf("迁移留痕的起止状态不符：%s → %s", transition.From, transition.To)
		}
		if transition.Action != edge.action {
			t.Fatalf("迁移留痕的触发来源不符：实际 %s，期望 %s", transition.Action, edge.action)
		}
	}
}

func TestLegalTargets_MatchesExpectedEdges(t *testing.T) {
	expected := map[State][]State{}
	for _, edge := range expectedEdges {
		expected[edge.from] = append(expected[edge.from], edge.to)
	}
	for from, targets := range expected {
		got := LegalTargets(from)
		if len(got) != len(targets) {
			t.Fatalf("状态 %s 的合法目标数不符：实际 %d，期望 %d", from, len(got), len(targets))
		}
		for i := range targets {
			if got[i] != targets[i] {
				t.Fatalf("状态 %s 的合法目标第 %d 项不符：实际 %s，期望 %s", from, i, got[i], targets[i])
			}
		}
	}
	// 终态没有出边。
	for _, terminal := range []State{StateWrittenBack, StateRejected, StateNeedsManual, StateFailed} {
		if len(LegalTargets(terminal)) != 0 {
			t.Fatalf("终态 %s 不应有出边，实际 %v", terminal, LegalTargets(terminal))
		}
	}
}

func TestLegalTargets_ReturnsCopy(t *testing.T) {
	targets := LegalTargets(StateDetected)
	if len(targets) == 0 {
		t.Fatal("detected 应有合法目标")
	}
	targets[0] = State("tampered")
	if LegalTargets(StateDetected)[0] == State("tampered") {
		t.Fatal("LegalTargets 返回的应是副本：改动它不应影响迁移表")
	}
}

// ---------------------------------------------------------------------------
// 二、非法迁移边
// ---------------------------------------------------------------------------

func TestAdvance_EveryOtherEdgeRejected(t *testing.T) {
	legal := map[[2]State]bool{}
	for _, edge := range expectedEdges {
		legal[[2]State{edge.from, edge.to}] = true
	}
	for _, from := range AllStates() {
		for _, to := range AllStates() {
			if legal[[2]State{from, to}] {
				continue
			}
			_, _, err := Advance(from, Request{
				To:     to,
				Action: actionForTarget(to),
				Reason: "test_reason",
				Actor:  "tester",
			})
			if err == nil {
				t.Fatalf("非法迁移 %s → %s 被放行", from, to)
			}
			code := errcode.As(err).Code
			if code != errcode.IllegalTransition {
				t.Fatalf("非法迁移 %s → %s 的错误码不符：实际 %s，期望 %s",
					from, to, code, errcode.IllegalTransition)
			}
		}
	}
}

func TestAdvance_SelfLoopRejected(t *testing.T) {
	for _, state := range AllStates() {
		_, _, err := Advance(state, Request{
			To:     state,
			Action: actionForTarget(state),
			Reason: "test_reason",
			Actor:  "tester",
		})
		if err == nil {
			t.Fatalf("状态 %s 迁移到自身被放行", state)
		}
		if code := errcode.As(err).Code; code != errcode.IllegalTransition {
			t.Fatalf("状态 %s 自迁移的错误码不符：实际 %s", state, code)
		}
	}
}

// ---------------------------------------------------------------------------
// 三、终态保护
// ---------------------------------------------------------------------------

func TestIsTerminal_TerminalStates(t *testing.T) {
	terminals := map[State]bool{
		StateWrittenBack: true,
		StateRejected:    true,
		StateNeedsManual: true,
		StateFailed:      true,
	}
	for _, state := range AllStates() {
		if got := IsTerminal(state); got != terminals[state] {
			t.Fatalf("状态 %s 的终态判定不符：实际 %v，期望 %v", state, got, terminals[state])
		}
	}
}

func TestAdvance_TerminalStatesRejectEveryTransition(t *testing.T) {
	for _, from := range []State{StateWrittenBack, StateRejected, StateNeedsManual, StateFailed} {
		for _, to := range AllStates() {
			_, _, err := Advance(from, Request{
				To:     to,
				Action: actionForTarget(to),
				Reason: "test_reason",
				Actor:  "tester",
			})
			if err == nil {
				t.Fatalf("终态 %s → %s 被放行", from, to)
			}
			if code := errcode.As(err).Code; code != errcode.IllegalTransition {
				t.Fatalf("终态 %s → %s 的错误码不符：实际 %s", from, to, code)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 四、触发来源限制：模型产出不得直接推进业务状态
// ---------------------------------------------------------------------------

func TestAdvance_ModelOutputCannotDecideDisposition(t *testing.T) {
	// 模型产出最多把判定推到「待确认」；已确认 / 已否决 / 已写回
	// 三类状态只认人的确认动作与台账写回。
	//
	// 两条路径都要拦住：边存在但来源不对（模型想确认），
	// 以及边压根不存在（模型想写回台账）。
	for _, to := range []State{StateConfirmed, StateRejected, StateWrittenBack, StatePendingWriteback} {
		from := StateAwaitingConfirmation
		if to == StateRejected {
			from = StateAssessed
		}
		_, _, err := Advance(from, Request{
			To:     to,
			Action: ActionAuto,
			Reason: "model_output",
			Actor:  ActorSystem,
		})
		if err == nil {
			t.Fatalf("自动推进 %s → %s 被放行", from, to)
		}
		if code := errcode.As(err).Code; code != errcode.IllegalTransition {
			t.Fatalf("自动推进 %s → %s 的错误码不符：实际 %s", from, to, code)
		}
	}

	// 边存在、来源不对的情形：迁移表里有这条边，但模型不能走它，
	// 错误信息要点名是哪个来源被拦下。
	for _, edge := range []struct {
		from State
		to   State
	}{
		{StateAssessed, StateRejected},
		{StateAwaitingConfirmation, StateConfirmed},
	} {
		_, _, err := Advance(edge.from, Request{
			To:     edge.to,
			Action: ActionAuto,
			Reason: "model_output",
			Actor:  ActorSystem,
		})
		if err == nil {
			t.Fatalf("自动推进 %s → %s 被放行", edge.from, edge.to)
		}
		if code := errcode.As(err).Code; code != errcode.IllegalTransition {
			t.Fatalf("自动推进 %s → %s 的错误码不符：实际 %s", edge.from, edge.to, code)
		}
		if !contains(errcode.As(err).Details, string(ActionAuto)) {
			t.Fatalf("自动推进 %s → %s 的错误信息未点名触发来源：%s",
				edge.from, edge.to, errcode.As(err).Details)
		}
	}
}

func TestActionsFor_EachTarget(t *testing.T) {
	cases := map[State][]Action{
		StateAssessed:             {ActionAuto},
		StateAwaitingConfirmation: {ActionAuto},
		StateNeedsManual:          {ActionAuto},
		StateFailed:               {ActionAuto},
		StateConfirmed:            {ActionConfirm},
		StateRejected:             {ActionReject},
		StateWrittenBack:          {ActionWriteback, ActionCompensate},
		StatePendingWriteback:     {ActionWriteback, ActionCompensate},
	}
	for to, want := range cases {
		got := ActionsFor(to)
		if len(got) != len(want) {
			t.Fatalf("目标状态 %s 的允许来源数不符：实际 %v，期望 %v", to, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("目标状态 %s 的允许来源第 %d 项不符：实际 %s，期望 %s", to, i, got[i], want[i])
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 五、原因码留痕
// ---------------------------------------------------------------------------

func TestAdvance_TransitionCarriesReasonNoteActorAndTime(t *testing.T) {
	at := time.Date(2026, 10, 26, 9, 30, 0, 0, time.UTC)
	_, transition, err := Advance(StateAwaitingConfirmation, Request{
		To:     StateConfirmed,
		Action: ActionConfirm,
		Reason: "supervisor_confirmed",
		Note:   "主管确认：隔离待疫苗生产企业评估",
		Actor:  "supervisor-07",
		At:     at,
	})
	if err != nil {
		t.Fatalf("确认迁移被拒：%v", err)
	}
	if transition.Reason != "supervisor_confirmed" {
		t.Fatalf("原因码不符：%s", transition.Reason)
	}
	if transition.Note != "主管确认：隔离待疫苗生产企业评估" {
		t.Fatalf("补注不符：%s", transition.Note)
	}
	if transition.Actor != "supervisor-07" {
		t.Fatalf("执行者不符：%s", transition.Actor)
	}
	if !transition.At.Equal(at) {
		t.Fatalf("迁移时刻不符：实际 %s，期望 %s", transition.At, at)
	}
}

func TestAdvance_ReasonCodeRequired(t *testing.T) {
	for _, reason := range []string{"", "   "} {
		_, _, err := Advance(StateAwaitingConfirmation, Request{
			To:     StateConfirmed,
			Action: ActionConfirm,
			Reason: reason,
			Actor:  "supervisor-07",
		})
		if err == nil {
			t.Fatalf("没有原因码的迁移被放行（reason=%q）", reason)
		}
		if code := errcode.As(err).Code; code != errcode.Validation {
			t.Fatalf("没有原因码的错误码不符：实际 %s，期望 %s", code, errcode.Validation)
		}
	}
}

func TestAdvance_UnregisteredStatesRejected(t *testing.T) {
	// 目标状态未登记：调用方传错了，是校验问题不是迁移问题。
	_, _, err := Advance(StateDetected, Request{
		To:     State("archived_by_mistake"),
		Action: ActionAuto,
		Reason: "test_reason",
		Actor:  "tester",
	})
	if err == nil {
		t.Fatal("未登记的目标状态被放行")
	}
	if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("未登记目标状态的错误码不符：实际 %s，期望 %s", code, errcode.Validation)
	}

	// 当前状态未登记：库里读出脏数据，不给它继续走。
	_, _, err = Advance(State(""), Request{
		To:     StateAssessed,
		Action: ActionAuto,
		Reason: "test_reason",
		Actor:  "tester",
	})
	if err == nil {
		t.Fatal("未登记的当前状态被放行")
	}
	if code := errcode.As(err).Code; code != errcode.IllegalTransition {
		t.Fatalf("未登记当前状态的错误码不符：实际 %s，期望 %s", code, errcode.IllegalTransition)
	}
}

func TestAdvance_UnregisteredActionRejected(t *testing.T) {
	_, _, err := Advance(StateAwaitingConfirmation, Request{
		To:     StateConfirmed,
		Action: Action("model_vibes"),
		Reason: "test_reason",
		Actor:  "tester",
	})
	if err == nil {
		t.Fatal("未登记的触发来源被放行")
	}
	if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("未登记触发来源的错误码不符：实际 %s，期望 %s", code, errcode.Validation)
	}
}

// ---------------------------------------------------------------------------
// 六、对外契约：非法迁移回 409
// ---------------------------------------------------------------------------

func TestIllegalTransitionMapsToHTTP409(t *testing.T) {
	_, _, err := Advance(StateAwaitingConfirmation, Request{
		To:     StateWrittenBack,
		Action: ActionWriteback,
		Reason: "test_reason",
		Actor:  "tester",
	})
	if err == nil {
		t.Fatal("未确认就写回台账的迁移被放行")
	}
	code := errcode.As(err).Code
	if code != errcode.IllegalTransition {
		t.Fatalf("错误码不符：实际 %s", code)
	}
	if status := errcode.HTTPStatus(code); status != http.StatusConflict {
		t.Fatalf("E_ILLEGAL_TRANSITION 的 HTTP 状态不符：实际 %d，期望 %d", status, http.StatusConflict)
	}
}

// ---------------------------------------------------------------------------
// 七、状态清单
// ---------------------------------------------------------------------------

func TestAllStates_NineStatesInChainOrder(t *testing.T) {
	states := AllStates()
	if len(states) != 9 {
		t.Fatalf("业务状态数不符：实际 %d，期望 9", len(states))
	}
	seen := map[State]bool{}
	for _, state := range states {
		if seen[state] {
			t.Fatalf("状态 %s 重复登记", state)
		}
		seen[state] = true
		if !Known(state) {
			t.Fatalf("状态 %s 未登记", state)
		}
		if Describe(state) == "" {
			t.Fatalf("状态 %s 缺中文说明", state)
		}
	}
	if states[0] != StateDetected {
		t.Fatalf("状态顺序应以 detected 起头，实际 %s", states[0])
	}
	if Known(State("never_registered")) {
		t.Fatal("未登记的状态不应被 Known 认领")
	}
}

// contains 判断子串是否在文本中。
func contains(text, sub string) bool {
	return strings.Contains(text, sub)
}
