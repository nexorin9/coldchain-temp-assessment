package workflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/disposition"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// fixedAt 是测试用的固定时刻，让确认时刻可复现。
var fixedAt = time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)

// fakeStates 是出站写回闸门用的状态读取替身：按 run_id 返回预先摆好的业务状态。
type fakeStates struct {
	states map[string]string
	err    error
	calls  int
}

func (f *fakeStates) RunState(_ context.Context, runID string) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	state, ok := f.states[runID]
	if !ok {
		return "", errcode.Newf(errcode.NotFound, "判定 run 不存在：%s", runID)
	}
	return state, nil
}

// countingWriter 是出站写回替身：只数调用次数，并把载荷原样记下来。
//
// 它数的是「真的发出去了几次」——闸门要在调用**之前**拦住，
// 所以拦下时这个计数必须是 0，而不是「发出去了但被回滚」。
type countingWriter struct {
	calls    int
	payloads []string
	err      error
}

func (w *countingWriter) WriteDeviation(_ context.Context, _ string, payloadJSON string) error {
	if w.err != nil {
		return w.err
	}
	w.calls++
	w.payloads = append(w.payloads, payloadJSON)
	return nil
}

// supervisorRequest 造一份药学部主管的确认请求。
func supervisorRequest() ConfirmRequest {
	return ConfirmRequest{
		RunID:       "run-test-0001",
		Actor:       "supervisor-007",
		Role:        RoleSupervisor,
		Disposition: disposition.Isolate,
		Opinion:     "已联系疫苗生产企业评估，先隔离",
		At:          fixedAt,
	}
}

// TestConfirm_AdvancesToConfirmed 覆盖「确认推进到已确认」：
// 待确认 → 已确认这条边存在，处置、确认人、意见与留痕都落到位。
func TestConfirm_AdvancesToConfirmed(t *testing.T) {
	req := supervisorRequest()
	result, err := Confirm(StateAwaitingConfirmation, req)
	if err != nil {
		t.Fatalf("主管确认待确认的判定应当成功：%v", err)
	}
	if result.ToState != StateConfirmed {
		t.Fatalf("确认后状态不对：%q", result.ToState)
	}
	if result.FromState != StateAwaitingConfirmation {
		t.Errorf("确认前状态不对：%q", result.FromState)
	}
	if result.Disposition != disposition.Isolate {
		t.Errorf("处置结论不对：%q", result.Disposition)
	}
	if result.DispositionLabel != "隔离" {
		t.Errorf("处置中文标签不对：%q", result.DispositionLabel)
	}
	if result.ConfirmedBy != "supervisor-007" {
		t.Errorf("确认人不对：%q", result.ConfirmedBy)
	}
	if !result.ConfirmedAt.Equal(fixedAt) {
		t.Errorf("确认时刻不对：%v", result.ConfirmedAt)
	}
	if result.Opinion != req.Opinion {
		t.Errorf("意见没带出来：%q", result.Opinion)
	}
	// 留痕：谁、什么时候、从哪到哪、为什么，一样都不能少。
	if result.Transition.From != StateAwaitingConfirmation || result.Transition.To != StateConfirmed {
		t.Errorf("迁移留痕的起止不对：%+v", result.Transition)
	}
	if result.Transition.Action != ActionConfirm {
		t.Errorf("迁移留痕的触发来源不对：%q", result.Transition.Action)
	}
	if result.Transition.Reason != ReasonSupervisorConfirmed {
		t.Errorf("迁移留痕的原因码不对：%q", result.Transition.Reason)
	}
	if result.Transition.Actor != "supervisor-007" {
		t.Errorf("迁移留痕的执行者不对：%q", result.Transition.Actor)
	}
	if !result.Transition.At.Equal(fixedAt) {
		t.Errorf("迁移留痕的时刻不对：%v", result.Transition.At)
	}
}

// TestConfirm_OnlySupervisorMayConfirm 覆盖「经办不能替主管确认」：
// 身份不是主管时一律拒绝，且错误信息要点명当前身份与该找谁签。
func TestConfirm_OnlySupervisorMayConfirm(t *testing.T) {
	cases := []struct {
		name string
		role Role
		// known 表示身份已登记：已登记的身份被拒时，错误信息要点名该找谁签。
		known bool
	}{
		{name: "经办身份", role: RoleOperator, known: true},
		{name: "只读身份", role: RoleAuditor, known: true},
		{name: "投递通道", role: RoleIngest, known: true},
		{name: "身份缺失", role: RoleUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := supervisorRequest()
			req.Role = tc.role
			_, err := Confirm(StateAwaitingConfirmation, req)
			if err == nil {
				t.Fatal("非主管身份不得确认处置结论")
			}
			if code := errcode.As(err).Code; code != errcode.RoleRequired {
				t.Fatalf("错误码不对：%v", code)
			}
			if tc.known && !strings.Contains(err.Error(), "药学部主管（审核）") {
				t.Errorf("错误信息未写明持该权限的身份：%v", err)
			}
			if !tc.known && !strings.Contains(err.Error(), "认不准就拒") {
				t.Errorf("身份缺失时的错误信息未写明按最保守处理：%v", err)
			}
		})
	}
}

// TestConfirm_RejectsInvalidInput 覆盖确认动作的输入校验：
// 缺 run_id、缺确认人、处置取值越界，一律拒绝且不改状态。
func TestConfirm_RejectsInvalidInput(t *testing.T) {
	t.Run("缺 run_id", func(t *testing.T) {
		req := supervisorRequest()
		req.RunID = "  "
		_, err := Confirm(StateAwaitingConfirmation, req)
		if code := errcode.As(err).Code; code != errcode.Validation {
			t.Fatalf("错误码不对：%v", err)
		}
	})

	t.Run("缺确认人", func(t *testing.T) {
		req := supervisorRequest()
		req.Actor = ""
		_, err := Confirm(StateAwaitingConfirmation, req)
		if err == nil {
			t.Fatal("没有确认人的确认动作不得放行")
		}
		if code := errcode.As(err).Code; code != errcode.Validation {
			t.Fatalf("错误码不对：%v", err)
		}
		if !strings.Contains(err.Error(), "确认人") {
			t.Errorf("错误信息未点明缺确认人：%v", err)
		}
	})

	t.Run("处置取值越界", func(t *testing.T) {
		req := supervisorRequest()
		req.Disposition = "ship_it"
		_, err := Confirm(StateAwaitingConfirmation, req)
		if err == nil {
			t.Fatal("取值域之外的处置结论不得放行")
		}
		if code := errcode.As(err).Code; code != errcode.Validation {
			t.Fatalf("错误码不对：%v", err)
		}
		if !strings.Contains(err.Error(), "ship_it") {
			t.Errorf("错误信息未点名越界取值：%v", err)
		}
	})
}

// TestConfirm_IllegalStateRejected 覆盖状态迁移的拦截：
// 还没到「待确认」的判定不得被确认，非法迁移一律 409。
func TestConfirm_IllegalStateRejected(t *testing.T) {
	cases := []State{StateDetected, StateAssessed, StateRejected, StateWrittenBack, StateFailed}
	for _, state := range cases {
		_, err := Confirm(state, supervisorRequest())
		if err == nil {
			t.Fatalf("业务状态 %q 不得被直接确认", state)
		}
		if code := errcode.As(err).Code; code != errcode.IllegalTransition {
			t.Fatalf("业务状态 %q 的错误码不对：%v", state, err)
		}
	}
}

// TestReject_AdvancesToRejected 覆盖「否决推进到已否决」：
// 待确认 → 已否决这条边存在，处置留空（被否决的判定不写台账）。
func TestReject_AdvancesToRejected(t *testing.T) {
	req := supervisorRequest()
	req.Disposition = ""
	req.Opinion = "这条读数疑似探头故障，本次不算越温"
	result, err := Reject(StateAwaitingConfirmation, req)
	if err != nil {
		t.Fatalf("主管否决待确认的判定应当成功：%v", err)
	}
	if result.ToState != StateRejected {
		t.Fatalf("否决后状态不对：%q", result.ToState)
	}
	if result.Disposition != "" {
		t.Errorf("否决不应带处置结论：%q", result.Disposition)
	}
	if result.Transition.Action != ActionReject {
		t.Errorf("迁移留痕的触发来源不对：%q", result.Transition.Action)
	}
	if result.Transition.Reason != ReasonSupervisorRejected {
		t.Errorf("迁移留痕的原因码不对：%q", result.Transition.Reason)
	}
	if result.Transition.Actor != "supervisor-007" {
		t.Errorf("迁移留痕的执行者不对：%q", result.Transition.Actor)
	}
}

// TestReject_OnlySupervisorMayReject 覆盖「经办不能替主管否决」。
func TestReject_OnlySupervisorMayReject(t *testing.T) {
	req := supervisorRequest()
	req.Role = RoleOperator
	_, err := Reject(StateAwaitingConfirmation, req)
	if err == nil {
		t.Fatal("非主管身份不得否决判定")
	}
	if code := errcode.As(err).Code; code != errcode.RoleRequired {
		t.Fatalf("错误码不对：%v", err)
	}
}

// TestReject_AllowedFromAssessed 覆盖「结论刚出就能被否决」：
// assessed 与 awaiting_confirmation 都能到 rejected。
func TestReject_AllowedFromAssessed(t *testing.T) {
	result, err := Reject(StateAssessed, supervisorRequest())
	if err != nil {
		t.Fatalf("已出结论的判定应当可以被否决：%v", err)
	}
	if result.ToState != StateRejected {
		t.Fatalf("否决后状态不对：%q", result.ToState)
	}
}

// TestAuthorizeWriteback_OnlyAfterConfirmation 覆盖「未确认不得写回」这条硬约束：
// 只有已确认与待补偿两种状态放行，其余一律拒绝。
func TestAuthorizeWriteback_OnlyAfterConfirmation(t *testing.T) {
	allowed := []State{StateConfirmed, StatePendingWriteback}
	for _, state := range allowed {
		if err := AuthorizeWriteback(state); err != nil {
			t.Errorf("业务状态 %q 应当放行写回：%v", state, err)
		}
	}
	blocked := []State{
		StateDetected, StateAssessed, StateAwaitingConfirmation,
		StateRejected, StateNeedsManual, StateFailed,
	}
	for _, state := range blocked {
		err := AuthorizeWriteback(state)
		if err == nil {
			t.Fatalf("业务状态 %q 不得触发出站写回", state)
		}
		if code := errcode.As(err).Code; code != errcode.IllegalTransition {
			t.Errorf("业务状态 %q 的错误码不对：%v", state, err)
		}
		if !strings.Contains(err.Error(), "未经药学部主管确认") {
			t.Errorf("业务状态 %q 的错误信息未写明未确认：%v", state, err)
		}
	}
}

// TestWritebackGate_UnconfirmedRunMakesZeroOutboundCalls 覆盖任务要求的第四条：
// **未确认的 run，出站写回调用次数为 0**。
//
// 这条断言分两段：未确认时闸门在调用之前就把出站拦住（计数 0），
// 确认之后同一道闸门放行（计数 1）——证明拦的是「状态没到位」，
// 而不是「闸门坏了所以谁都不许写」。
func TestWritebackGate_UnconfirmedRunMakesZeroOutboundCalls(t *testing.T) {
	const runID = "run-test-0001"
	states := &fakeStates{states: map[string]string{runID: string(StateAwaitingConfirmation)}}
	writer := &countingWriter{}
	gate := NewWritebackGate(writer, states)

	// 未确认：闸门拦住，一次出站调用都不发。
	err := gate.WriteDeviation(context.Background(), runID, `{"external_ref":"cc-point-01"}`)
	if err == nil {
		t.Fatal("未确认的判定不得触发出站写回")
	}
	if code := errcode.As(err).Code; code != errcode.IllegalTransition {
		t.Fatalf("错误码不对：%v", err)
	}
	if writer.calls != 0 {
		t.Fatalf("未确认的 run 出站写回调用次数应为 0，实际 %d", writer.calls)
	}

	// 主管确认之后，状态到「已确认」，同一道闸门放行。
	result, err := Confirm(StateAwaitingConfirmation, supervisorRequest())
	if err != nil {
		t.Fatalf("主管确认应当成功：%v", err)
	}
	states.states[runID] = string(result.ToState)

	if err := gate.WriteDeviation(context.Background(), runID, `{"external_ref":"cc-point-01"}`); err != nil {
		t.Fatalf("已确认的判定应当放行写回：%v", err)
	}
	if writer.calls != 1 {
		t.Fatalf("已确认的 run 出站写回调用次数应为 1，实际 %d", writer.calls)
	}
	if len(writer.payloads) != 1 || writer.payloads[0] != `{"external_ref":"cc-point-01"}` {
		t.Errorf("载荷未原样送出：%+v", writer.payloads)
	}
}

// TestWritebackGate_MissingAssemblyFailsClosed 覆盖闸门自身的装配校验：
// 没装写回口或没装状态读取能力时一律失败关闭，不放行。
func TestWritebackGate_MissingAssemblyFailsClosed(t *testing.T) {
	gate := NewWritebackGate(nil, &fakeStates{})
	if err := gate.WriteDeviation(context.Background(), "run-x", "{}"); err == nil {
		t.Fatal("未装配写回口时不得放行")
	}
	gate = NewWritebackGate(&countingWriter{}, nil)
	if err := gate.WriteDeviation(context.Background(), "run-x", "{}"); err == nil {
		t.Fatal("未装配状态读取能力时不得放行")
	}
}

// TestWritebackGate_RunNotFoundBlocksOutbound 覆盖「查不到这条 run」的情形：
// 查不到就不写——宁可少写一条，也别把越温记到不存在的判定上。
func TestWritebackGate_RunNotFoundBlocksOutbound(t *testing.T) {
	writer := &countingWriter{}
	gate := NewWritebackGate(writer, &fakeStates{states: map[string]string{}})
	err := gate.WriteDeviation(context.Background(), "run-missing", "{}")
	if err == nil {
		t.Fatal("查不到的 run 不得触发出站写回")
	}
	if code := errcode.As(err).Code; code != errcode.NotFound {
		t.Fatalf("错误码不对：%v", err)
	}
	if writer.calls != 0 {
		t.Fatalf("出站写回调用次数应为 0，实际 %d", writer.calls)
	}
}
