package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/audit"
	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/store"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// confirmFixedAt 是确认动作的固定时刻，让断言可复现。
var confirmFixedAt = time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)

// supervisorConfirmRequest 造一份药学部主管的确认请求。
func supervisorConfirmRequest(runID string) workflow.ConfirmRequest {
	return workflow.ConfirmRequest{
		RunID:       runID,
		Actor:       "supervisor-007",
		Role:        workflow.RoleSupervisor,
		Disposition: "isolate",
		Opinion:     "已联系疫苗生产企业评估，先隔离",
		At:          confirmFixedAt,
	}
}

// runChainToAwaitingConfirmation 跑一条完整判定链并落库，
// 返回停在「待确认」的 run 标识。
func runChainToAwaitingConfirmation(t *testing.T, st *store.Store) string {
	t.Helper()
	ctx := context.Background()
	samples := excursionSamples()
	profile := adviceProfile()

	chain, err := NewChain(ChainDeps{
		Profile: profile,
		Samples: samples,
		Batches: adviceBatches(),
	})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	exec := NewExecutor(chain, st, quietLogger()).WithRunPersistence(st, profile)
	result, err := exec.Run(ctx, directRun(t, samples))
	if err != nil {
		t.Fatalf("步骤链执行失败：%v", err)
	}
	if result.Run.State != domain.RunStateAwaitingConfirmation {
		t.Fatalf("判定应当停在待确认，实际 %q", result.Run.State)
	}
	return result.Run.RunID
}

// TestConfirmer_ConfirmAdvancesRunAndFreezesPayload 覆盖确认动作的主场景：
// 状态推到已确认、写回载荷在确认这一刻固化、确认记录与审计链都留下。
func TestConfirmer_ConfirmAdvancesRunAndFreezesPayload(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	runID := runChainToAwaitingConfirmation(t, st)

	recorder := audit.NewRecorder(st)
	confirmer := NewConfirmer(st, recorder).WithClock(func() time.Time { return confirmFixedAt })

	outcome, err := confirmer.Confirm(ctx, runID, supervisorConfirmRequest(runID))
	if err != nil {
		t.Fatalf("主管确认应当成功：%v", err)
	}
	if outcome.Decision.ToState != workflow.StateConfirmed {
		t.Errorf("确认后状态不对：%q", outcome.Decision.ToState)
	}
	if !outcome.PayloadFrozen {
		t.Error("确认后写回载荷应当已固化")
	}
	if outcome.WritebackState != domain.WritebackStatePending {
		t.Errorf("确认后写回状态不对：%q", outcome.WritebackState)
	}

	// run 状态与迁移留痕落库。
	rec, err := st.RunByID(ctx, runID)
	if err != nil {
		t.Fatalf("回读判定 run 失败：%v", err)
	}
	if rec.Run.State != domain.RunStateConfirmed {
		t.Errorf("库里 run 状态不对：%q", rec.Run.State)
	}
	if rec.WritebackState != domain.WritebackStatePending {
		t.Errorf("库里写回状态不对：%q", rec.WritebackState)
	}
	if len(rec.Run.StateTrail) == 0 {
		t.Fatal("确认应当留下迁移留痕")
	}
	last := rec.Run.StateTrail[len(rec.Run.StateTrail)-1]
	if last.To != workflow.StateConfirmed || last.Action != workflow.ActionConfirm {
		t.Errorf("迁移留痕不对：%+v", last)
	}
	if last.Actor != "supervisor-007" {
		t.Errorf("迁移留痕的执行者不对：%q", last.Actor)
	}

	// 确认记录落库：谁、何时、什么处置、什么意见，以及固化下来的载荷。
	confirmation, err := st.ConfirmationByRun(ctx, runID)
	if err != nil {
		t.Fatalf("回读确认记录失败：%v", err)
	}
	if confirmation.Action != domain.ConfirmationActionConfirm {
		t.Errorf("确认动作类型不对：%q", confirmation.Action)
	}
	if confirmation.ConfirmedBy != "supervisor-007" {
		t.Errorf("确认人不对：%q", confirmation.ConfirmedBy)
	}
	if confirmation.Disposition != "isolate" {
		t.Errorf("处置结论不对：%q", confirmation.Disposition)
	}
	if confirmation.DispositionLabel != "隔离" {
		t.Errorf("处置中文标签不对：%q", confirmation.DispositionLabel)
	}
	if confirmation.Opinion != "已联系疫苗生产企业评估，先隔离" {
		t.Errorf("意见不对：%q", confirmation.Opinion)
	}

	// 固化的载荷：字段与 spec 的出站字段规范逐项对应。
	var payload map[string]any
	if err := json.Unmarshal([]byte(confirmation.PayloadJSON), &payload); err != nil {
		t.Fatalf("固化的载荷不是合法 JSON：%v", err)
	}
	for _, field := range []string{
		"external_ref", "point_id", "device_name", "excursion_started_at",
		"excursion_ended_at", "peak_temperature_c", "duration_minutes", "zone",
		"batches", "cause_conclusion", "evidence", "disposition",
		"confirmed_by", "confirmed_at", "exception_record", "idempotency_key",
	} {
		if _, ok := payload[field]; !ok {
			t.Errorf("固化的载荷缺字段 %q", field)
		}
	}
	if payload["disposition"] != "isolate" {
		t.Errorf("载荷里的处置结论不对：%v", payload["disposition"])
	}
	if payload["confirmed_by"] != "supervisor-007" {
		t.Errorf("载荷里的确认人不对：%v", payload["confirmed_by"])
	}
	if payload["external_ref"] != rec.Run.ExternalRef {
		t.Errorf("载荷里的外部引用与 run 不一致：%v", payload["external_ref"])
	}
	if payload["idempotency_key"] != rec.Run.IdempotencyKey {
		t.Errorf("载荷里的幂等键与 run 不一致：%v", payload["idempotency_key"])
	}
	batches, ok := payload["batches"].([]any)
	if !ok || len(batches) == 0 {
		t.Fatalf("固化的载荷应带出台账给的批次：%v", payload["batches"])
	}
	firstBatch, ok := batches[0].(map[string]any)
	if !ok || firstBatch["batch_ref"] != "<EXAMPLE_LOT-01>" {
		t.Errorf("批次号应原样带出：%v", batches[0])
	}

	// 记录表正文末尾补上了人工确认小节：谁、何时、定了什么处置、什么意见。
	exceptionRecord, _ := payload["exception_record"].(string)
	for _, want := range []string{
		"八、人工确认（药学部主管）",
		"确认人：supervisor-007",
		"最终处置结论：隔离（isolate）",
		"主管意见：已联系疫苗生产企业评估，先隔离",
	} {
		if !strings.Contains(exceptionRecord, want) {
			t.Errorf("记录表正文缺确认小节内容 %q\n%s", want, exceptionRecord)
		}
	}

	// 审计链：确认动作与模型调用同一条链，处置与意见各自成列。
	entries, err := st.AuditEntriesByRun(ctx, runID)
	if err != nil {
		t.Fatalf("回读审计链失败：%v", err)
	}
	var confirmEntry *domain.AuditEntry
	for i := range entries {
		if entries[i].EntryType == domain.AuditEntryConfirmation {
			confirmEntry = &entries[i]
		}
	}
	if confirmEntry == nil {
		t.Fatal("审计链里应有确认动作记录")
	}
	if confirmEntry.Actor != "supervisor-007" {
		t.Errorf("审计记录的执行者不对：%q", confirmEntry.Actor)
	}
	if confirmEntry.Disposition != "isolate" {
		t.Errorf("审计记录的处置结论不对：%q", confirmEntry.Disposition)
	}
	if confirmEntry.Opinion != "已联系疫苗生产企业评估，先隔离" {
		t.Errorf("审计记录的意见不对：%q", confirmEntry.Opinion)
	}
	if confirmEntry.FromState != domain.RunStateAwaitingConfirmation ||
		confirmEntry.ToState != domain.RunStateConfirmed {
		t.Errorf("审计记录的状态迁移不对：%s → %s", confirmEntry.FromState, confirmEntry.ToState)
	}
}

// TestConfirmer_ConfirmUpdatesDraftProduct 覆盖「确认后查到的处置栏
// 与台账里写的是同一个值」：成稿产物里的最终处置被更新，
// 但成稿这一步还是那一步——时间线上不多出第二条记录。
func TestConfirmer_ConfirmUpdatesDraftProduct(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	runID := runChainToAwaitingConfirmation(t, st)

	confirmer := NewConfirmer(st, audit.NewRecorder(st)).WithClock(func() time.Time { return confirmFixedAt })
	if _, err := confirmer.Confirm(ctx, runID, supervisorConfirmRequest(runID)); err != nil {
		t.Fatalf("主管确认应当成功：%v", err)
	}

	steps, err := st.RunStepsByRun(ctx, runID)
	if err != nil {
		t.Fatalf("回读步骤记录失败：%v", err)
	}
	before := countDraftSteps(steps)
	record, ok := ProductAs[domain.ExceptionRecordPayload](steps, domain.PayloadExceptionRecord)
	if !ok {
		t.Fatal("成稿产物应能按 exception_record 分码还原")
	}
	if record.ConfirmedDisposition != "isolate" {
		t.Errorf("成稿产物里的最终处置不对：%q", record.ConfirmedDisposition)
	}
	// 成稿这一步仍然只有一条：确认改的是稿子上的处置栏，不是多跑了一步。
	after := countDraftSteps(steps)
	if after != before {
		t.Errorf("确认后成稿步骤条数变了：%d → %d", before, after)
	}
}

// countDraftSteps 数出成稿这一步的记录条数。
func countDraftSteps(steps []domain.Step) int {
	count := 0
	for _, step := range steps {
		if step.PayloadKind == domain.PayloadExceptionRecord {
			count++
		}
	}
	return count
}

// TestConfirmer_RejectDoesNotProducePayload 覆盖否决：
// 状态推到已否决，不产生任何写回载荷——被否决的判定不写台账。
func TestConfirmer_RejectDoesNotProducePayload(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	runID := runChainToAwaitingConfirmation(t, st)

	req := supervisorConfirmRequest(runID)
	req.Disposition = ""
	req.Opinion = "本次读数疑似探头故障，不算越温"

	confirmer := NewConfirmer(st, audit.NewRecorder(st)).WithClock(func() time.Time { return confirmFixedAt })
	outcome, err := confirmer.Reject(ctx, runID, req)
	if err != nil {
		t.Fatalf("主管否决应当成功：%v", err)
	}
	if outcome.Decision.ToState != workflow.StateRejected {
		t.Errorf("否决后状态不对：%q", outcome.Decision.ToState)
	}
	if outcome.PayloadFrozen {
		t.Error("被否决的判定不应当固化写回载荷")
	}

	confirmation, err := st.ConfirmationByRun(ctx, runID)
	if err != nil {
		t.Fatalf("回读确认记录失败：%v", err)
	}
	if confirmation.Action != domain.ConfirmationActionReject {
		t.Errorf("确认动作类型不对：%q", confirmation.Action)
	}
	if confirmation.PayloadJSON != "" {
		t.Errorf("否决记录不应带写回载荷：%q", confirmation.PayloadJSON)
	}
	if confirmation.Disposition != "" {
		t.Errorf("否决记录不应带处置结论：%q", confirmation.Disposition)
	}

	rec, err := st.RunByID(ctx, runID)
	if err != nil {
		t.Fatalf("回读判定 run 失败：%v", err)
	}
	if rec.Run.State != domain.RunStateRejected {
		t.Errorf("库里 run 状态不对：%q", rec.Run.State)
	}
}

// TestConfirmer_OperatorCannotConfirm 覆盖越权：
// 经办身份确认被拒，且库里一个字节都没改。
func TestConfirmer_OperatorCannotConfirm(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	runID := runChainToAwaitingConfirmation(t, st)

	req := supervisorConfirmRequest(runID)
	req.Role = workflow.RoleOperator

	confirmer := NewConfirmer(st, audit.NewRecorder(st)).WithClock(func() time.Time { return confirmFixedAt })
	_, err := confirmer.Confirm(ctx, runID, req)
	if err == nil {
		t.Fatal("经办身份不得确认处置结论")
	}
	if code := errcode.As(err).Code; code != errcode.RoleRequired {
		t.Fatalf("错误码不对：%v", err)
	}

	rec, err := st.RunByID(ctx, runID)
	if err != nil {
		t.Fatalf("回读判定 run 失败：%v", err)
	}
	if rec.Run.State != domain.RunStateAwaitingConfirmation {
		t.Errorf("被拒的确认不得改状态，实际 %q", rec.Run.State)
	}
	if _, err := st.ConfirmationByRun(ctx, runID); err == nil {
		t.Error("被拒的确认不得留下确认记录")
	}
}

// TestConfirmer_InvalidDispositionRejected 覆盖处置取值域：
// 域外取值被拒，且状态不动。
func TestConfirmer_InvalidDispositionRejected(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	runID := runChainToAwaitingConfirmation(t, st)

	req := supervisorConfirmRequest(runID)
	req.Disposition = "ship_it"

	confirmer := NewConfirmer(st, audit.NewRecorder(st)).WithClock(func() time.Time { return confirmFixedAt })
	_, err := confirmer.Confirm(ctx, runID, req)
	if err == nil {
		t.Fatal("取值域之外的处置结论不得放行")
	}
	if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("错误码不对：%v", err)
	}
}

// TestConfirmer_WithoutAuditFailsClosed 覆盖「没有审计链的确认不予放行」：
// 审计落库口没装配时拒绝确认，库里不留痕。
func TestConfirmer_WithoutAuditFailsClosed(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	runID := runChainToAwaitingConfirmation(t, st)

	confirmer := NewConfirmer(st, nil)
	_, err := confirmer.Confirm(ctx, runID, supervisorConfirmRequest(runID))
	if err == nil {
		t.Fatal("未装配审计落库时不得放行确认")
	}
	rec, err := st.RunByID(ctx, runID)
	if err != nil {
		t.Fatalf("回读判定 run 失败：%v", err)
	}
	if rec.Run.State != domain.RunStateAwaitingConfirmation {
		t.Errorf("被拒的确认不得改状态，实际 %q", rec.Run.State)
	}
}
