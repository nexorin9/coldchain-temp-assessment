package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// ---------------------------------------------------------------------------
// 一、合法迁移边
// ---------------------------------------------------------------------------

func TestExecState_LegalTransitions(t *testing.T) {
	for _, from := range AllExecStates() {
		for _, to := range ExecStateTransitions(from) {
			run := NewRun("point-vax-freezer-01")
			run.ExecState = from

			if err := run.Transition(to); err != nil {
				t.Fatalf("合法迁移 %s → %s 被拒：%v", from, to, err)
			}
			if run.ExecState != to {
				t.Fatalf("迁移 %s → %s 后状态未更新，当前 %s", from, to, run.ExecState)
			}
		}
	}
}

func TestExecState_TerminalStatesHaveNoOutgoingEdge(t *testing.T) {
	for _, terminal := range []ExecState{ExecStateSucceeded, ExecStateFailed} {
		if next := ExecStateTransitions(terminal); len(next) != 0 {
			t.Fatalf("终态 %s 不应有出边，实际 %v", terminal, next)
		}
	}
}

func TestExecState_NewRunStartsQueued(t *testing.T) {
	run := NewRun("point-vax-freezer-01")
	if run.ExecState != ExecStateQueued {
		t.Fatalf("新建 run 的执行态应为 queued，实际 %s", run.ExecState)
	}
	if run.State != RunStateDetected {
		t.Fatalf("新建 run 的业务状态应为 detected，实际 %s", run.State)
	}
	if !strings.HasPrefix(run.RunID, "run_") || len(run.RunID) <= 4 {
		t.Fatalf("run_id 应由服务端生成且形如 run_<hex>，实际 %q", run.RunID)
	}
}

// ---------------------------------------------------------------------------
// 二、非法迁移边
// ---------------------------------------------------------------------------

func TestExecState_IllegalTransitionRejected(t *testing.T) {
	cases := []struct {
		name string
		from ExecState
		to   ExecState
	}{
		{"排队未开始不能直接成功", ExecStateQueued, ExecStateSucceeded},
		{"执行中不能退回排队", ExecStateRunning, ExecStateQueued},
		{"成功是终态不能再重试", ExecStateSucceeded, ExecStateRetrying},
		{"失败是终态不能再跑", ExecStateFailed, ExecStateRunning},
		{"重试中不能直接成功", ExecStateRetrying, ExecStateSucceeded},
		{"未知目标态", ExecStateRunning, ExecState("archived")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := NewRun("point-vax-freezer-01")
			run.ExecState = tc.from

			err := run.Transition(tc.to)
			if err == nil {
				t.Fatalf("非法迁移 %s → %s 应被拒绝", tc.from, tc.to)
			}
			code := errcode.As(err).Code
			if code != errcode.IllegalTransition {
				t.Fatalf("非法迁移应返回 %s，实际 %s", errcode.IllegalTransition, code)
			}
			// 状态不被误推进：非法迁移不得改动当前执行态。
			if run.ExecState != tc.from {
				t.Fatalf("非法迁移后执行态被改动：%s → %s（期望仍为 %s）", tc.from, run.ExecState, tc.from)
			}
		})
	}
}

func TestExecState_UninitializedStateRejected(t *testing.T) {
	run := Run{RunID: "run_test", PointID: "point-vax-freezer-01"}

	err := run.Transition(ExecStateRunning)
	if err == nil {
		t.Fatal("空执行态的 run 不应被允许迁移")
	}
	if code := errcode.As(err).Code; code != errcode.IllegalTransition {
		t.Fatalf("空执行态迁移应返回 %s，实际 %s", errcode.IllegalTransition, code)
	}
	if run.ExecState != "" {
		t.Fatalf("非法迁移后执行态被改动：%q", run.ExecState)
	}
}

// ---------------------------------------------------------------------------
// 三、external_ref 同窗口稳定
// ---------------------------------------------------------------------------

func TestExternalRef_StableForSameWindow(t *testing.T) {
	start := time.Date(2026, 10, 20, 3, 10, 0, 0, time.FixedZone("CST", 8*3600))
	end := start.Add(15 * time.Minute)

	first := NewRun("point-vax-freezer-01")
	first.BindWindow(start, end)

	for i := 0; i < 100; i++ {
		again := NewRun("point-vax-freezer-01")
		again.BindWindow(start, end)

		if again.ExternalRef != first.ExternalRef {
			t.Fatalf("同一点位同一窗口的 external_ref 必须稳定：第 %d 次得到 %q，首次为 %q",
				i, again.ExternalRef, first.ExternalRef)
		}
		if again.IdempotencyKey != first.IdempotencyKey {
			t.Fatalf("同一点位同一窗口的幂等键必须稳定：第 %d 次得到 %q，首次为 %q",
				i, again.IdempotencyKey, first.IdempotencyKey)
		}
	}

	if !strings.HasPrefix(first.ExternalRef, "cc-point-vax-freezer-01-") {
		t.Fatalf("external_ref 应含点位标识，实际 %q", first.ExternalRef)
	}
	if first.WindowStart == nil || first.WindowEnd == nil {
		t.Fatal("BindWindow 后窗口起止不应为空")
	}
	if !first.WindowStart.Equal(start) || !first.WindowEnd.Equal(end) {
		t.Fatalf("BindWindow 写回的窗口起止不符：%v ~ %v", first.WindowStart, first.WindowEnd)
	}
}

func TestExternalRef_DiffersAcrossPointsAndWindows(t *testing.T) {
	start := time.Date(2026, 10, 20, 3, 10, 0, 0, time.UTC)

	base := ExternalRef("point-vax-freezer-01", start)
	if other := ExternalRef("point-vax-freezer-02", start); other == base {
		t.Fatalf("不同点位的 external_ref 应不同：%q", other)
	}
	if other := ExternalRef("point-vax-freezer-01", start.Add(5*time.Minute)); other == base {
		t.Fatalf("不同窗口的 external_ref 应不同：%q", other)
	}

	// 同一时刻不同偏移写法仍指向同一个窗口起点 → 同一个 external_ref。
	same := start.In(time.FixedZone("CST", 8*3600))
	if other := ExternalRef("point-vax-freezer-01", same); other != base {
		t.Fatalf("同一窗口起点（不同偏移写法）的 external_ref 应一致：%q vs %q", other, base)
	}
}

func TestNewRunID_Unique(t *testing.T) {
	seen := make(map[string]bool, 200)
	for i := 0; i < 200; i++ {
		id := NewRunID()
		if seen[id] {
			t.Fatalf("run_id 重复：%q", id)
		}
		seen[id] = true
	}
}

// ---------------------------------------------------------------------------
// 四、产物分码互不串型
// ---------------------------------------------------------------------------

func TestStepPayload_KindsDoNotBleedIntoEachOther(t *testing.T) {
	attribution := &AttributionPayload{
		CauseCode:  "power_loss",
		Confidence: 0.82,
		Evidence: []AttributionEvidence{
			{Feature: "door_open_minutes", Value: 0, Threshold: 5, Note: "全程无开门记录"},
			{Feature: "power_loss_present", Value: 1, Threshold: 1},
		},
	}
	record := &ExceptionRecordPayload{
		PointID:            "point-vax-freezer-01",
		DeviceName:         "疫苗冷藏箱-01",
		ExcursionStartedAt: time.Date(2026, 10, 20, 3, 10, 0, 0, time.UTC),
		ExcursionEndedAt:   time.Date(2026, 10, 20, 3, 25, 0, 0, time.UTC),
		PeakTemperatureC:   12.4,
		DurationMinutes:    15,
		Zone:               "2~8℃",
		Disposition:        "pending_manufacturer_evaluation",
	}

	encodedAttribution, err := EncodePayload(PayloadAttribution, attribution)
	if err != nil {
		t.Fatalf("归因结论编码失败：%v", err)
	}
	encodedRecord, err := EncodePayload(PayloadExceptionRecord, record)
	if err != nil {
		t.Fatalf("成稿记录表编码失败：%v", err)
	}

	// 按本分码取回：还原成对应类型。
	gotAttribution, err := DecodePayload(PayloadAttribution, encodedAttribution)
	if err != nil {
		t.Fatalf("按归因分码取回失败：%v", err)
	}
	typedAttribution, ok := gotAttribution.(*AttributionPayload)
	if !ok {
		t.Fatalf("归因分码应还原成 *AttributionPayload，实际 %T", gotAttribution)
	}
	if typedAttribution.CauseCode != "power_loss" || len(typedAttribution.Evidence) != 2 {
		t.Fatalf("归因产物内容不符：%+v", typedAttribution)
	}

	gotRecord, err := DecodePayload(PayloadExceptionRecord, encodedRecord)
	if err != nil {
		t.Fatalf("按成稿分码取回失败：%v", err)
	}
	typedRecord, ok := gotRecord.(*ExceptionRecordPayload)
	if !ok {
		t.Fatalf("成稿分码应还原成 *ExceptionRecordPayload，实际 %T", gotRecord)
	}
	if typedRecord.PeakTemperatureC != 12.4 || typedRecord.DurationMinutes != 15 {
		t.Fatalf("成稿产物内容不符：%+v", typedRecord)
	}

	// 跨分码取回：必须明确拒绝，不做「尽力解析」。
	if _, err := DecodePayload(PayloadExceptionRecord, encodedAttribution); err == nil {
		t.Fatal("用成稿分码取回归因产物应被拒绝")
	} else if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("跨分码取回应返回 %s，实际 %s", errcode.Validation, code)
	}

	if _, err := DecodePayload(PayloadAttribution, encodedRecord); err == nil {
		t.Fatal("用归因分码取回成稿产物应被拒绝")
	} else if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("跨分码取回应返回 %s，实际 %s", errcode.Validation, code)
	}
}

func TestEncodePayload_RejectsMismatchedType(t *testing.T) {
	// 归因载荷挂到成稿分码上 → 拒绝。
	if _, err := EncodePayload(PayloadExceptionRecord, &AttributionPayload{CauseCode: "door_open"}); err == nil {
		t.Fatal("载荷类型与分码不匹配时应拒绝编码")
	} else if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("类型不匹配应返回 %s，实际 %s", errcode.Validation, code)
	}

	// 未登记的分码 → 拒绝。
	if _, err := EncodePayload(StepPayloadKind("unknown_kind"), &AttributionPayload{}); err == nil {
		t.Fatal("未登记的分码应被拒绝")
	}
}

func TestStep_SetPayload_ReplacementIsolatesOtherSteps(t *testing.T) {
	started := time.Date(2026, 10, 20, 3, 10, 0, 0, time.UTC)

	attributeStep := NewStep("attribute", started)
	recordStep := NewStep("draft", started)

	if err := attributeStep.SetPayload(PayloadAttribution, &AttributionPayload{
		CauseCode:  "door_open",
		Confidence: 0.7,
	}); err != nil {
		t.Fatalf("写入归因产物失败：%v", err)
	}
	if err := recordStep.SetPayload(PayloadExceptionRecord, &ExceptionRecordPayload{
		PointID:          "point-vax-freezer-01",
		PeakTemperatureC: 12.4,
	}); err != nil {
		t.Fatalf("写成稿产物失败：%v", err)
	}

	recordBefore := string(recordStep.Payload)

	// 换掉归因产物：成稿步骤的产物不得受影响。
	if err := attributeStep.SetPayload(PayloadAttribution, &AttributionPayload{
		CauseCode:  "power_loss",
		Confidence: 0.9,
	}); err != nil {
		t.Fatalf("替换归因产物失败：%v", err)
	}

	if string(recordStep.Payload) != recordBefore {
		t.Fatal("替换归因产物不得改动其它步骤的产物")
	}
	if recordStep.PayloadKind != PayloadExceptionRecord {
		t.Fatalf("成稿步骤的分码被改动：%q", recordStep.PayloadKind)
	}
	if attributeStep.PayloadKind != PayloadAttribution {
		t.Fatalf("归因步骤的分码被改动：%q", attributeStep.PayloadKind)
	}

	// 用错分码取回本步骤产物 → 拒绝。
	if _, err := recordStep.PayloadAs(PayloadAttribution); err == nil {
		t.Fatal("用错分码取回步骤产物应被拒绝")
	} else if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("错分码取回应返回 %s，实际 %s", errcode.Validation, code)
	}

	// 正确分码取回，内容为替换后的那一份。
	got, err := attributeStep.PayloadAs(PayloadAttribution)
	if err != nil {
		t.Fatalf("按分码取回归因产物失败：%v", err)
	}
	typed := got.(*AttributionPayload)
	if typed.CauseCode != "power_loss" || typed.Confidence != 0.9 {
		t.Fatalf("替换后的归因产物不符：%+v", typed)
	}
}

func TestStepPayload_TypedHelper(t *testing.T) {
	raw, err := EncodePayload(PayloadModelRawOutput, &ModelRawOutputPayload{
		Model:        "model-a",
		RawResponse:  `{"cause_code":"door_open"}`,
		InputTokens:  128,
		OutputTokens: 64,
	})
	if err != nil {
		t.Fatalf("编码模型原始输出失败：%v", err)
	}

	typed, err := DecodePayloadAs[ModelRawOutputPayload](PayloadModelRawOutput, raw)
	if err != nil {
		t.Fatalf("按类型取回失败：%v", err)
	}
	if typed.InputTokens != 128 || typed.Model != "model-a" {
		t.Fatalf("模型原始输出内容不符：%+v", typed)
	}

	// 类型断言不符时返回错误，而不是悄悄返回零值。
	if _, err := DecodePayloadAs[AttributionPayload](PayloadModelRawOutput, raw); err == nil {
		t.Fatal("类型断言不符时应返回错误")
	}
}

func TestPayloadKinds_RegisteredAndOrdered(t *testing.T) {
	kinds := PayloadKinds()
	// 分码随判定步骤链扩充：取数与窗口两步接入后，采样与窗口也各自独立分码。
	if len(kinds) != 6 {
		t.Fatalf("应登记六类步骤产物分码，实际 %d 类：%v", len(kinds), kinds)
	}
	want := []StepPayloadKind{
		PayloadSeries, PayloadWindow, PayloadAttribution,
		PayloadExceptionRecord, PayloadConfirmationReady, PayloadModelRawOutput,
	}
	for i, kind := range want {
		if kinds[i] != kind {
			t.Fatalf("分码顺序不符：第 %d 项为 %q，期望 %q", i, kinds[i], kind)
		}
	}

	// 分码顺序固定：多次调用结果一致（摘要与审计输出要可复现）。
	for i := 0; i < 10; i++ {
		again := PayloadKinds()
		for j := range again {
			if again[j] != kinds[j] {
				t.Fatalf("分码顺序不稳定：%v vs %v", again, kinds)
			}
		}
	}
}

func TestStepPayload_EnvelopeCarriesKind(t *testing.T) {
	raw, err := EncodePayload(PayloadAttribution, &AttributionPayload{CauseCode: "defrost"})
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}

	var envelope struct {
		Kind    StepPayloadKind `json:"kind"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("信封解析失败：%v", err)
	}
	if envelope.Kind != PayloadAttribution {
		t.Fatalf("信封内分码不符：%q", envelope.Kind)
	}
	if !strings.Contains(string(envelope.Payload), "defrost") {
		t.Fatalf("信封内容不符：%s", envelope.Payload)
	}
}

// ---------------------------------------------------------------------------
// 八、业务状态机：唯一写入口与迁移留痕
// ---------------------------------------------------------------------------

func TestRunState_ConstantsMatchWorkflowStates(t *testing.T) {
	// domain 里的九个状态是 workflow 状态机的同源别名：
	// 值只在 workflow 定义一次，避免两处定义悄悄跑偏。
	cases := map[string]workflow.State{
		RunStateDetected:             workflow.StateDetected,
		RunStateAssessed:             workflow.StateAssessed,
		RunStateAwaitingConfirmation: workflow.StateAwaitingConfirmation,
		RunStateConfirmed:            workflow.StateConfirmed,
		RunStatePendingWriteback:     workflow.StatePendingWriteback,
		RunStateWrittenBack:          workflow.StateWrittenBack,
		RunStateRejected:             workflow.StateRejected,
		RunStateNeedsManual:          workflow.StateNeedsManual,
		RunStateFailed:               workflow.StateFailed,
	}
	if len(cases) != 9 {
		t.Fatalf("业务状态数不符：实际 %d，期望 9", len(cases))
	}
	for alias, canonical := range cases {
		if alias != string(canonical) {
			t.Fatalf("状态别名与状态机取值不一致：%q vs %q", alias, canonical)
		}
	}
}

func TestRunState_AdvanceStateRecordsTrail(t *testing.T) {
	run := NewRun("point-vax-freezer-01")
	if run.State != RunStateDetected {
		t.Fatalf("新建 run 的业务状态不符：%s", run.State)
	}

	transition, err := run.AdvanceState(workflow.Request{
		To:     workflow.StateAssessed,
		Action: workflow.ActionAuto,
		Reason: "excursion_window_extracted",
		Note:   "越温窗口已抽出，给出可核对的判定结论",
		Actor:  workflow.ActorSystem,
	})
	if err != nil {
		t.Fatalf("状态推进被拒：%v", err)
	}
	if run.State != RunStateAssessed {
		t.Fatalf("推进后业务状态不符：%s", run.State)
	}
	if len(run.StateTrail) != 1 {
		t.Fatalf("迁移留痕条数不符：实际 %d，期望 1", len(run.StateTrail))
	}
	trail := run.StateTrail[0]
	if trail.From != workflow.StateDetected || trail.To != workflow.StateAssessed {
		t.Fatalf("留痕的起止状态不符：%s → %s", trail.From, trail.To)
	}
	if trail.Reason != "excursion_window_extracted" {
		t.Fatalf("留痕的原因码不符：%s", trail.Reason)
	}
	if trail.Action != workflow.ActionAuto {
		t.Fatalf("留痕的触发来源不符：%s", trail.Action)
	}
	if transition != trail {
		t.Fatalf("返回的迁移记录与留痕不一致：%+v vs %+v", transition, trail)
	}

	// 第二次推进：留痕只追加，不覆盖。
	if _, err := run.AdvanceState(workflow.Request{
		To:     workflow.StateAwaitingConfirmation,
		Action: workflow.ActionAuto,
		Reason: "draft_ready_for_confirmation",
		Actor:  workflow.ActorSystem,
	}); err != nil {
		t.Fatalf("第二次状态推进被拒：%v", err)
	}
	if len(run.StateTrail) != 2 {
		t.Fatalf("迁移留痕条数不符：实际 %d，期望 2", len(run.StateTrail))
	}
	if run.StateTrail[0].Reason != "excursion_window_extracted" {
		t.Fatalf("首条留痕被改写：%s", run.StateTrail[0].Reason)
	}
}

func TestRunState_AdvanceStateRejected_KeepsStateAndTrail(t *testing.T) {
	run := NewRun("point-vax-freezer-01")
	// 模型产出不得直接把判定推进到「已确认」。
	_, err := run.AdvanceState(workflow.Request{
		To:     workflow.StateConfirmed,
		Action: workflow.ActionAuto,
		Reason: "model_output",
		Actor:  workflow.ActorSystem,
	})
	if err == nil {
		t.Fatal("未确认的判定被推进到已确认")
	}
	if code := errcode.As(err).Code; code != errcode.IllegalTransition {
		t.Fatalf("错误码不符：实际 %s，期望 %s", code, errcode.IllegalTransition)
	}
	if run.State != RunStateDetected {
		t.Fatalf("推进被拒后业务状态被改写：%s", run.State)
	}
	if len(run.StateTrail) != 0 {
		t.Fatalf("推进被拒后仍留下迁移痕：%+v", run.StateTrail)
	}
}

func TestRunState_TerminalRunRejectsFurtherAdvance(t *testing.T) {
	run := NewRun("point-vax-freezer-01")
	if _, err := run.AdvanceState(workflow.Request{
		To:     workflow.StateNeedsManual,
		Action: workflow.ActionAuto,
		Reason: "zone_unusable",
		Actor:  workflow.ActorSystem,
	}); err != nil {
		t.Fatalf("转人工推进被拒：%v", err)
	}
	if _, err := run.AdvanceState(workflow.Request{
		To:     workflow.StateAssessed,
		Action: workflow.ActionAuto,
		Reason: "late_window",
		Actor:  workflow.ActorSystem,
	}); err == nil {
		t.Fatal("终态 run 仍被推进")
	}
	if run.State != RunStateNeedsManual {
		t.Fatalf("终态 run 的业务状态被改写：%s", run.State)
	}
}
