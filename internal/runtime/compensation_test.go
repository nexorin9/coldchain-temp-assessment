package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/ledger"
	"github.com/chaosforge/coldchain-temp-assessment/internal/store"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// 本文件是「出站写回没落成之后怎么办」这一段的编排测试：
// 待补偿清单、可见告警与补偿重放。
//
// 它盯住四件事：
//
//  1. **退避重试耗尽 → 挂起待补偿**：run 落到 pending_writeback，
//     待补偿清单里能查到它（不多不少，就这一条）。
//  2. **挂起不静默**：同时留下一条告警记录（状态 open），
//     记着这一处目标累计敲了多少次门、最后错在哪。
//  3. **补偿重放补得上**：台账恢复后重放，这条越温补进台账，
//     run 推进到 written_back，告警解掉。
//  4. **重放不重复入账**：重放用的是同一把幂等键与同一份固化载荷；
//     已写成的那一处（台账或通知）连请求都不发。

// pendingCompensationRun 造一条「台账写不进去、已挂起待补偿」的判定。
//
// 走的是真实主路径：判定链 → 主管确认 → 出站写回（台账侧失败）。
func pendingCompensationRun(t *testing.T, st *store.Store) (string, *Writebacker, *fakeDeviationWriter) {
	t.Helper()
	ctx := context.Background()
	runID := confirmedRun(t, st)

	writer := &fakeDeviationWriter{err: errcode.Newf(errcode.LedgerUnavailable, "冷链台账不可达")}
	notify := &fakeNotifier{}
	wb := NewWritebacker(st, writer, notify, quietLogger()).
		WithClock(func() time.Time { return confirmFixedAt })

	outcome, err := wb.Writeback(ctx, runID)
	if err != nil {
		t.Fatalf("写回应当以「挂起待补偿」收场而不是报错：%v", err)
	}
	if outcome.WritebackState != domain.WritebackStatePending {
		t.Fatalf("写回状态 = %q，期望 pending（待补偿）", outcome.WritebackState)
	}
	return runID, wb, writer
}

// TestCompensation_RetryExhaustedGoesPendingAndRaisesAlert 覆盖退避耗尽的落点：
// run 落到 pending_writeback，待补偿清单里含这条 run，
// 且告警记录存在（状态 open、记着尝试次数与错误码）。
func TestCompensation_RetryExhaustedGoesPendingAndRaisesAlert(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	runID, wb, _ := pendingCompensationRun(t, st)

	// 业务状态：落到「已确认，台账写回待补偿重放」。
	rec, err := st.RunByID(ctx, runID)
	if err != nil {
		t.Fatalf("回读 run 失败：%v", err)
	}
	if rec.Run.State != string(workflow.StatePendingWriteback) {
		t.Fatalf("run 业务状态 = %q，期望 pending_writeback", rec.Run.State)
	}
	if rec.WritebackState != domain.WritebackStatePending {
		t.Fatalf("run 写回状态 = %q，期望 pending", rec.WritebackState)
	}

	// 待补偿清单：含这条 run，且不多不少。
	compensator := NewCompensator(st, wb, quietLogger())
	summary, err := compensator.Summary(ctx)
	if err != nil {
		t.Fatalf("读待补偿清单失败：%v", err)
	}
	if summary.PendingCount != 1 {
		t.Fatalf("待写回记录数 = %d，期望 1", summary.PendingCount)
	}
	if len(summary.Items) != 1 {
		t.Fatalf("待补偿清单条目数 = %d，期望 1", len(summary.Items))
	}
	item := summary.Items[0]
	if item.RunID != runID {
		t.Errorf("待补偿清单里的 run = %q，期望 %q", item.RunID, runID)
	}
	if item.Target != domain.WritebackTargetLedger {
		t.Errorf("待补偿目标 = %q，期望 ledger（台账那一处没写成）", item.Target)
	}
	if item.State != domain.WritebackTargetStateFailed {
		t.Errorf("待补偿条目的状态 = %q，期望 failed", item.State)
	}
	if item.RunState != string(workflow.StatePendingWriteback) {
		t.Errorf("待补偿条目里的 run 状态 = %q，期望 pending_writeback", item.RunState)
	}
	if item.PointID == "" {
		t.Error("待补偿条目应当带出越温点位")
	}

	// 告警记录：存在、开着、记着尝试次数与错误码。
	if summary.OpenAlertCount != 1 {
		t.Fatalf("还挂着的告警数 = %d，期望 1", summary.OpenAlertCount)
	}
	if len(summary.Alerts) != 1 {
		t.Fatalf("告警清单条目数 = %d，期望 1", len(summary.Alerts))
	}
	alert := summary.Alerts[0]
	if alert.RunID != runID || alert.Target != domain.WritebackTargetLedger {
		t.Errorf("告警定位不对：%+v", alert)
	}
	if !alert.Open() {
		t.Errorf("告警状态 = %q，期望 open", alert.State)
	}
	if alert.ErrorCode != string(errcode.LedgerUnavailable) {
		t.Errorf("告警错误码 = %q，期望 E_LEDGER_UNAVAILABLE", alert.ErrorCode)
	}
	if alert.Attempts <= 0 {
		t.Errorf("告警累计尝试次数 = %d，期望大于 0", alert.Attempts)
	}
	if alert.Failures != 1 {
		t.Errorf("告警累计失败轮次 = %d，期望 1", alert.Failures)
	}
	if alert.FirstFailedAt == nil || alert.LastFailedAt == nil {
		t.Error("告警应当记着第一次与最近一次失败的时刻")
	}
}

// TestCompensation_RetryReplaysWithoutDuplicatingLedger 覆盖补偿重放：
// 台账恢复后重放一次，这条越温补进台账，run 推进到 written_back，
// 告警解掉；已经写成的那一处不被重复发请求。
func TestCompensation_RetryReplaysWithoutDuplicatingLedger(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	runID, wb, writer := pendingCompensationRun(t, st)
	callsBefore := writer.calls

	// 台账恢复：这一次写回不再报错。
	writer.err = nil
	compensator := NewCompensator(st, wb, quietLogger())

	outcome, err := compensator.Retry(ctx, runID)
	if err != nil {
		t.Fatalf("补偿重放失败：%v", err)
	}
	if !outcome.LedgerWritten {
		t.Errorf("重放后台账应当写成：%+v", outcome)
	}
	if !outcome.NotificationSent {
		t.Errorf("重放后通知应当发出：%+v", outcome)
	}
	if !outcome.Compensation {
		t.Error("回执应当标明本次是补偿重放")
	}
	if outcome.WritebackState != domain.WritebackStateWrittenBack {
		t.Errorf("重放后的写回状态 = %q，期望 written_back", outcome.WritebackState)
	}
	if writer.calls != callsBefore+1 {
		t.Errorf("台账调用数 = %d，期望 %d（重放只补没写成的那一处）",
			writer.calls, callsBefore+1)
	}

	// 重放发的是同一份固化载荷：幂等键与 external_ref 都没变。
	if len(writer.payload) == 0 {
		t.Fatal("重放应当发出过一次台账请求")
	}
	first, err := ledger.DecodeDeviation(writer.payload[0])
	if err != nil {
		t.Fatalf("解析固化载荷失败：%v", err)
	}
	last, err := ledger.DecodeDeviation(writer.payload[len(writer.payload)-1])
	if err != nil {
		t.Fatalf("解析固化载荷失败：%v", err)
	}
	if first.ExternalRef != last.ExternalRef || first.IdempotencyKey != last.IdempotencyKey {
		t.Errorf("重放的幂等键变了：首次 %s/%s，重放 %s/%s",
			first.ExternalRef, first.IdempotencyKey, last.ExternalRef, last.IdempotencyKey)
	}

	// run 推进到已写回，告警解掉。
	rec, err := st.RunByID(ctx, runID)
	if err != nil {
		t.Fatalf("回读 run 失败：%v", err)
	}
	if rec.Run.State != string(workflow.StateWrittenBack) {
		t.Fatalf("重放后的 run 状态 = %q，期望 written_back", rec.Run.State)
	}
	summary, err := compensator.Summary(ctx)
	if err != nil {
		t.Fatalf("读待补偿清单失败：%v", err)
	}
	if summary.PendingCount != 0 {
		t.Errorf("补上之后待写回记录数 = %d，期望 0", summary.PendingCount)
	}
	if summary.OpenAlertCount != 0 {
		t.Errorf("补上之后还挂着的告警数 = %d，期望 0", summary.OpenAlertCount)
	}

	// 告警不删：open→resolved 的留痕还在库里。
	alert, err := st.WritebackAlertByTarget(ctx, runID, domain.WritebackTargetLedger)
	if err != nil {
		t.Fatalf("回读告警失败：%v", err)
	}
	if alert.Open() {
		t.Error("补上之后告警应当已解掉")
	}
	if alert.ResolvedAt == nil {
		t.Error("解掉的告警应当记着解决时刻")
	}
	if alert.Failures != 1 {
		t.Errorf("告警累计失败轮次 = %d，期望 1（补上那轮不算失败）", alert.Failures)
	}
}

// TestCompensation_ReplayFailureKeepsPendingAndAccumulates 覆盖重放仍失败：
// 告警次数继续累加，这条仍留在待补偿清单里——不静默丢弃。
func TestCompensation_ReplayFailureKeepsPendingAndAccumulates(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	runID, wb, _ := pendingCompensationRun(t, st)

	compensator := NewCompensator(st, wb, quietLogger())
	if _, err := compensator.Retry(ctx, runID); err != nil {
		t.Fatalf("补偿重放失败：%v", err)
	}

	summary, err := compensator.Summary(ctx)
	if err != nil {
		t.Fatalf("读待补偿清单失败：%v", err)
	}
	if summary.PendingCount != 1 {
		t.Fatalf("重放仍失败时待写回记录数 = %d，期望 1（仍在清单里）", summary.PendingCount)
	}
	if summary.OpenAlertCount != 1 {
		t.Fatalf("重放仍失败时还挂着的告警数 = %d，期望 1", summary.OpenAlertCount)
	}
	alert := summary.Alerts[0]
	if alert.Failures != 2 {
		t.Errorf("告警累计失败轮次 = %d，期望 2（首次写回 + 一次重放）", alert.Failures)
	}
	if alert.Attempts <= 0 {
		t.Errorf("告警累计尝试次数 = %d，期望大于 0", alert.Attempts)
	}
}

// TestCompensation_ReplayWithoutPendingTargetIsRejected 覆盖空跑防护：
// 没有待补偿目标的 run 不该被重放（否则是空跑，还会平白推进状态）。
func TestCompensation_ReplayWithoutPendingTargetIsRejected(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	runID, wb, _ := pendingCompensationRun(t, st)

	// 先把它补上：待补偿清单清空。
	compensator := NewCompensator(st, wb, quietLogger())
	// 直接把写回记录标成已写成，模拟「已经补好了」。
	if err := st.UpsertWriteback(ctx, domain.WritebackRecord{
		RunID:       runID,
		Target:      domain.WritebackTargetLedger,
		State:       domain.WritebackTargetStateWritten,
		ExternalRef: "cc-point-vax-freezer-01-20261019T191000Z",
		CreatedAt:   confirmFixedAt,
		UpdatedAt:   confirmFixedAt,
	}); err != nil {
		t.Fatalf("更新写回记录失败：%v", err)
	}

	_, err := compensator.Retry(ctx, runID)
	if err == nil {
		t.Fatal("没有待补偿目标时重放应当被拒")
	}
	if code := errcode.As(err).Code; code != errcode.IllegalTransition {
		t.Errorf("错误码 = %q，期望 E_ILLEGAL_TRANSITION", code)
	}
}

// TestCompensation_SummaryReportsRunLocation 覆盖清单的定位信息：
// 每条待补偿记录都要能定位到具体那条判定与那个点位。
func TestCompensation_SummaryReportsRunLocation(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	runID, wb, _ := pendingCompensationRun(t, st)

	compensator := NewCompensator(st, wb, quietLogger())
	summary, err := compensator.Summary(ctx)
	if err != nil {
		t.Fatalf("读待补偿清单失败：%v", err)
	}
	if len(summary.Items) != 1 {
		t.Fatalf("清单条目数 = %d，期望 1", len(summary.Items))
	}
	item := summary.Items[0]
	if item.RunID != runID {
		t.Errorf("清单里的 run = %q，期望 %q", item.RunID, runID)
	}
	if item.ExternalRef == "" {
		t.Error("清单条目应当带出 external_ref（补偿重放靠它保持同一把幂等键）")
	}
	if item.IdempotencyKey == "" {
		t.Error("清单条目应当带出幂等键")
	}
	if item.ErrorCode != string(errcode.LedgerUnavailable) {
		t.Errorf("清单条目里的错误码 = %q，期望 E_LEDGER_UNAVAILABLE", item.ErrorCode)
	}
	if item.UpdatedAt.IsZero() {
		t.Error("清单条目应当带出最近更新时间")
	}
}

// TestCompensation_RetryUnknownRunRejected 覆盖边界：run 不存在时明确报错。
func TestCompensation_RetryUnknownRunRejected(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	wb := NewWritebacker(st, &fakeDeviationWriter{}, &fakeNotifier{}, quietLogger())
	compensator := NewCompensator(st, wb, quietLogger())

	if _, err := compensator.Retry(ctx, "run-not-exists"); err == nil {
		t.Fatal("不存在的 run 应当报错")
	}
	if _, err := compensator.Retry(ctx, "  "); err == nil {
		t.Fatal("空 run_id 应当报错")
	}
}

// 确认 store 实现了补偿重放所需的读写口（编译期兜底）。
var _ CompensationStore = (*store.Store)(nil)
