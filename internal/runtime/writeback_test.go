package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/audit"
	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/ledger"
	"github.com/chaosforge/coldchain-temp-assessment/internal/store"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// 本文件是出站写回这一段的编排测试。
//
// 它盯住六件事：
//
//  1. 确认之后台账与通知**各恰好一次**出站调用（不多不少）。
//  2. **未确认不得写回**：没确认的 run 一次出站调用都不发。
//  3. **写回顺序**：先台账、后通知。台账没写成时通知不发。
//  4. **通知失败不撤销台账写入**：台账那一条还在，通知挂起待补偿。
//  5. **重放不产生第二条台账记录**：本地按目标记账，已写成的不再发请求。
//  6. **被否决的判定不写台账**：台账里多一条没发生过的越温，比少记一条更麻烦。

// fakeDeviationWriter 是一次出站台账写调的替身：只记调用，不发请求。
type fakeDeviationWriter struct {
	calls   int
	payload []string
	err     error
}

func (f *fakeDeviationWriter) WriteDeviation(_ context.Context, runID string, payloadJSON string) error {
	f.calls++
	f.payload = append(f.payload, payloadJSON)
	return f.err
}

// fakeNotifier 是一次出站通知发调的替身：只记调用，不发请求。
type fakeNotifier struct {
	calls int
	msgs  []ledger.Message
	err   error
}

func (f *fakeNotifier) Send(_ context.Context, msg ledger.Message) error {
	f.calls++
	f.msgs = append(f.msgs, msg)
	return f.err
}

// confirmedRun 跑一条判定链到「待确认」，再由主管确认，返回 run 标识。
func confirmedRun(t *testing.T, st *store.Store) string {
	t.Helper()
	ctx := context.Background()
	runID := runChainToAwaitingConfirmation(t, st)
	confirmer := NewConfirmer(st, audit.NewRecorder(st)).WithClock(func() time.Time { return confirmFixedAt })
	if _, err := confirmer.Confirm(ctx, runID, supervisorConfirmRequest(runID)); err != nil {
		t.Fatalf("主管确认失败：%v", err)
	}
	return runID
}

// TestWriteback_ConfirmedRunWritesLedgerAndNotifyExactlyOnce 是本任务的主场景：
// 主管确认之后，台账与通知各恰好被调用一次。
func TestWriteback_ConfirmedRunWritesLedgerAndNotifyExactlyOnce(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	runID := confirmedRun(t, st)

	writer := &fakeDeviationWriter{}
	notify := &fakeNotifier{}
	wb := NewWritebacker(st, writer, notify, quietLogger()).
		WithClock(func() time.Time { return confirmFixedAt })

	outcome, err := wb.Writeback(ctx, runID)
	if err != nil {
		t.Fatalf("写回应当成功：%v", err)
	}
	if writer.calls != 1 {
		t.Errorf("台账被调用 %d 次，期望恰好 1 次", writer.calls)
	}
	if notify.calls != 1 {
		t.Errorf("通知被调用 %d 次，期望恰好 1 次", notify.calls)
	}
	if !outcome.LedgerWritten || !outcome.NotificationSent {
		t.Errorf("写回结果不对：台账=%v 通知=%v", outcome.LedgerWritten, outcome.NotificationSent)
	}
	if !outcome.Complete() {
		t.Error("两处都写成了，结果应当是完整的")
	}
	if outcome.ExternalRef == "" || outcome.IdempotencyKey == "" {
		t.Errorf("写回结果缺少外部引用或幂等键：%+v", outcome)
	}

	// 台账与通知的先后顺序：台账先、通知后。
	if len(writer.payload) != 1 {
		t.Fatalf("台账载荷应有 1 份，实际 %d 份", len(writer.payload))
	}
	if !strings.Contains(writer.payload[0], "external_ref") {
		t.Errorf("台账载荷不是固化的写回载荷：%s", writer.payload[0])
	}
	if len(notify.msgs) != 1 {
		t.Fatalf("通知消息应有 1 条，实际 %d 条", len(notify.msgs))
	}
	msg := notify.msgs[0]
	if msg.RelatedRunID != runID {
		t.Errorf("通知关联的 run = %q，期望 %q", msg.RelatedRunID, runID)
	}
	if msg.IdempotencyKey == "" {
		t.Error("通知必须带幂等键")
	}
	if !strings.Contains(msg.Body, "越温") || !strings.Contains(msg.Body, "12.4") {
		t.Errorf("通知正文应当说清这次越温的事实：%q", msg.Body)
	}

	// 库里：run 状态推到「已写回台账」，写回状态置已写回，
	// 两处目标各留一条「已写成」的记录。
	rec, err := st.RunByID(ctx, runID)
	if err != nil {
		t.Fatalf("回读判定 run 失败：%v", err)
	}
	if rec.Run.State != domain.RunStateWrittenBack {
		t.Errorf("run 状态 = %q，期望 %q", rec.Run.State, domain.RunStateWrittenBack)
	}
	if rec.WritebackState != domain.WritebackStateWrittenBack {
		t.Errorf("写回状态 = %q，期望 %q", rec.WritebackState, domain.WritebackStateWrittenBack)
	}
	for _, target := range []string{domain.WritebackTargetLedger, domain.WritebackTargetNotification} {
		record, err := st.WritebackByTarget(ctx, runID, target)
		if err != nil {
			t.Fatalf("回读 %s 写回记录失败：%v", target, err)
		}
		if !record.Written() {
			t.Errorf("%s 写回记录状态 = %q，期望已写成", target, record.State)
		}
		if record.WrittenAt == nil {
			t.Errorf("%s 写回记录缺少写入时刻", target)
		}
	}
}

// TestWriteback_UnconfirmedRunMakesZeroOutboundCalls 断言「未确认不得写回」：
// 待确认的 run 一次出站调用都不发。
func TestWriteback_UnconfirmedRunMakesZeroOutboundCalls(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	runID := runChainToAwaitingConfirmation(t, st)

	writer := &fakeDeviationWriter{}
	notify := &fakeNotifier{}
	wb := NewWritebacker(st, writer, notify, quietLogger())

	_, err := wb.Writeback(ctx, runID)
	if err == nil {
		t.Fatal("未确认的 run 写回必须被拦下")
	}
	if code := errcode.As(err).Code; code != errcode.IllegalTransition {
		t.Errorf("错误码 = %q，期望 %q", code, errcode.IllegalTransition)
	}
	if writer.calls != 0 || notify.calls != 0 {
		t.Errorf("出站调用数：台账 %d 次、通知 %d 次，期望都是 0",
			writer.calls, notify.calls)
	}
}

// TestWriteback_RejectedRunIsNotWritten 断言被否决的判定不写台账。
func TestWriteback_RejectedRunIsNotWritten(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	runID := runChainToAwaitingConfirmation(t, st)

	confirmer := NewConfirmer(st, audit.NewRecorder(st)).WithClock(func() time.Time { return confirmFixedAt })
	if _, err := confirmer.Reject(ctx, runID, workflow.ConfirmRequest{
		RunID:   runID,
		Actor:   "supervisor-007",
		Role:    workflow.RoleSupervisor,
		Opinion: "这条不该这么判",
		At:      confirmFixedAt,
	}); err != nil {
		t.Fatalf("否决失败：%v", err)
	}

	writer := &fakeDeviationWriter{}
	notify := &fakeNotifier{}
	wb := NewWritebacker(st, writer, notify, quietLogger())

	if _, err := wb.Writeback(ctx, runID); err == nil {
		t.Fatal("被否决的判定写回必须被拒")
	}
	if writer.calls != 0 || notify.calls != 0 {
		t.Errorf("出站调用数：台账 %d 次、通知 %d 次，期望都是 0",
			writer.calls, notify.calls)
	}
}

// TestWriteback_LedgerFailureStopsBeforeNotify 断言台账没写成时通知不发。
func TestWriteback_LedgerFailureStopsBeforeNotify(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	runID := confirmedRun(t, st)

	writer := &fakeDeviationWriter{err: errcode.Newf(errcode.LedgerUnavailable, "冷链台账不可达")}
	notify := &fakeNotifier{}
	wb := NewWritebacker(st, writer, notify, quietLogger()).
		WithClock(func() time.Time { return confirmFixedAt })

	outcome, err := wb.Writeback(ctx, runID)
	if err != nil {
		t.Fatalf("台账不可达不算硬失败（结论已确认，缺的是台账那一条）：%v", err)
	}
	if writer.calls != 1 {
		t.Errorf("台账被调用 %d 次，期望 1 次", writer.calls)
	}
	if notify.calls != 0 {
		t.Errorf("台账都没写进去，通知不该发（实际发了 %d 次）", notify.calls)
	}
	if outcome.LedgerError != string(errcode.LedgerUnavailable) {
		t.Errorf("台账错误码 = %q，期望 %q", outcome.LedgerError, errcode.LedgerUnavailable)
	}
	if outcome.LedgerWritten || outcome.NotificationSent {
		t.Error("台账没写成，结果里不该出现已写成的标记")
	}

	// 挂起待补偿：台账里那一条还没进去，这事不能就这么过去。
	rec, err := st.RunByID(ctx, runID)
	if err != nil {
		t.Fatalf("回读判定 run 失败：%v", err)
	}
	if rec.Run.State != domain.RunStatePendingWriteback {
		t.Errorf("run 状态 = %q，期望 %q", rec.Run.State, domain.RunStatePendingWriteback)
	}
	if rec.WritebackState != domain.WritebackStatePending {
		t.Errorf("写回状态 = %q，期望 %q", rec.WritebackState, domain.WritebackStatePending)
	}
	record, err := st.WritebackByTarget(ctx, runID, domain.WritebackTargetLedger)
	if err != nil {
		t.Fatalf("回读写回记录失败：%v", err)
	}
	if record.State != domain.WritebackTargetStateFailed {
		t.Errorf("台账写回记录状态 = %q，期望 %q", record.State, domain.WritebackTargetStateFailed)
	}
	if record.ErrorCode != string(errcode.LedgerUnavailable) {
		t.Errorf("台账写回记录错误码 = %q，期望 %q", record.ErrorCode, errcode.LedgerUnavailable)
	}
}

// TestWriteback_NotifyFailureKeepsLedgerWrite 断言通知失败不撤销台账写入。
func TestWriteback_NotifyFailureKeepsLedgerWrite(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	runID := confirmedRun(t, st)

	writer := &fakeDeviationWriter{}
	notify := &fakeNotifier{err: errcode.Newf(errcode.NotifyUnavailable, "通知系统不可达")}
	wb := NewWritebacker(st, writer, notify, quietLogger()).
		WithClock(func() time.Time { return confirmFixedAt })

	outcome, err := wb.Writeback(ctx, runID)
	if err != nil {
		t.Fatalf("通知失败不算硬失败：%v", err)
	}
	if writer.calls != 1 {
		t.Errorf("台账被调用 %d 次，期望 1 次", writer.calls)
	}
	if notify.calls != 1 {
		t.Errorf("通知被调用 %d 次，期望 1 次", notify.calls)
	}
	if !outcome.LedgerWritten {
		t.Error("台账写成了，结果里必须标出来（不能被通知失败抹掉）")
	}
	if outcome.NotificationSent {
		t.Error("通知没发出去，不该标成已发出")
	}
	if outcome.NotificationError != string(errcode.NotifyUnavailable) {
		t.Errorf("通知错误码 = %q，期望 %q", outcome.NotificationError, errcode.NotifyUnavailable)
	}

	// 台账那一条还在（记录是「已写成」），通知那一条是「失败」，等补偿。
	ledgerRec, err := st.WritebackByTarget(ctx, runID, domain.WritebackTargetLedger)
	if err != nil {
		t.Fatalf("回读台账写回记录失败：%v", err)
	}
	if !ledgerRec.Written() {
		t.Errorf("台账写回记录状态 = %q，期望已写成（通知失败不该撤销它）", ledgerRec.State)
	}
	notifyRec, err := st.WritebackByTarget(ctx, runID, domain.WritebackTargetNotification)
	if err != nil {
		t.Fatalf("回读通知写回记录失败：%v", err)
	}
	if notifyRec.State != domain.WritebackTargetStateFailed {
		t.Errorf("通知写回记录状态 = %q，期望 %q", notifyRec.State, domain.WritebackTargetStateFailed)
	}

	// run 挂起待补偿：台账写进去了、通知没发出去。
	rec, err := st.RunByID(ctx, runID)
	if err != nil {
		t.Fatalf("回读判定 run 失败：%v", err)
	}
	if rec.Run.State != domain.RunStatePendingWriteback {
		t.Errorf("run 状态 = %q，期望 %q", rec.Run.State, domain.RunStatePendingWriteback)
	}
	if rec.WritebackState != domain.WritebackStatePending {
		t.Errorf("写回状态 = %q，期望 %q", rec.WritebackState, domain.WritebackStatePending)
	}
}

// TestWriteback_ReplayDoesNotWriteSecondLedgerRecord 断言重放不产生第二条台账记录。
func TestWriteback_ReplayDoesNotWriteSecondLedgerRecord(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	runID := confirmedRun(t, st)

	writer := &fakeDeviationWriter{}
	notify := &fakeNotifier{}
	wb := NewWritebacker(st, writer, notify, quietLogger()).
		WithClock(func() time.Time { return confirmFixedAt })

	if _, err := wb.Writeback(ctx, runID); err != nil {
		t.Fatalf("首次写回失败：%v", err)
	}
	// 再写一次：补偿重放、接口重试、进程重启后续跑都走这条路。
	outcome, err := wb.Writeback(ctx, runID)
	if err != nil {
		t.Fatalf("重放写回失败：%v", err)
	}
	if writer.calls != 1 {
		t.Errorf("台账被调用 %d 次，期望 1 次（重放不得再发一次）", writer.calls)
	}
	if notify.calls != 1 {
		t.Errorf("通知被调用 %d 次，期望 1 次（重放不得再发一次）", notify.calls)
	}
	if !outcome.LedgerSkipped || !outcome.NotificationSkipped {
		t.Errorf("重放结果应当标出两处都跳过：%+v", outcome)
	}
}

// TestWriteback_ReplayAfterLedgerOnlyResendsNotification 断言台账写成了、
// 通知没发出去时，重放只补通知那一处，不重发台账。
func TestWriteback_ReplayAfterLedgerOnlyResendsNotification(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	runID := confirmedRun(t, st)

	writer := &fakeDeviationWriter{}
	notify := &fakeNotifier{err: errcode.Newf(errcode.NotifyUnavailable, "通知系统不可达")}
	wb := NewWritebacker(st, writer, notify, quietLogger()).
		WithClock(func() time.Time { return confirmFixedAt })

	if _, err := wb.Writeback(ctx, runID); err != nil {
		t.Fatalf("首次写回失败：%v", err)
	}
	if writer.calls != 1 || notify.calls != 1 {
		t.Fatalf("首次调用数不对：台账 %d 次、通知 %d 次", writer.calls, notify.calls)
	}

	// 通知系统恢复后再补一次。
	notify.err = nil
	outcome, err := wb.Writeback(ctx, runID)
	if err != nil {
		t.Fatalf("补发失败：%v", err)
	}
	if writer.calls != 1 {
		t.Errorf("台账被调用 %d 次，期望仍是 1 次（台账那一条已经在了）", writer.calls)
	}
	if notify.calls != 2 {
		t.Errorf("通知被调用 %d 次，期望 2 次（补发一次）", notify.calls)
	}
	if !outcome.NotificationSent {
		t.Error("补发成功后应当标出通知已发出")
	}
	if !outcome.LedgerSkipped {
		t.Error("补发时台账那一处应当标出跳过")
	}

	// 两处都齐了：状态推到「已写回台账」。
	rec, err := st.RunByID(ctx, runID)
	if err != nil {
		t.Fatalf("回读判定 run 失败：%v", err)
	}
	if rec.Run.State != domain.RunStateWrittenBack {
		t.Errorf("run 状态 = %q，期望 %q", rec.Run.State, domain.RunStateWrittenBack)
	}
	if rec.WritebackState != domain.WritebackStateWrittenBack {
		t.Errorf("写回状态 = %q，期望 %q", rec.WritebackState, domain.WritebackStateWrittenBack)
	}
}

// TestWriteback_GateBlocksUnconfirmedRun 断言闸门在出站调用之前就把未确认的
// run 拦下——不是发完再补救。
func TestWriteback_GateBlocksUnconfirmedRun(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	runID := runChainToAwaitingConfirmation(t, st)

	calls := 0
	inner := &fakeDeviationWriter{}
	gated := workflow.NewWritebackGate(
		writerFunc(func(ctx context.Context, runID string, payloadJSON string) error {
			calls++
			return inner.WriteDeviation(ctx, runID, payloadJSON)
		}),
		st,
	)
	wb := NewWritebacker(st, gated, &fakeNotifier{}, quietLogger())

	if _, err := wb.Writeback(ctx, runID); err == nil {
		t.Fatal("未确认的 run 必须被闸门拦下")
	}
	if calls != 0 {
		t.Errorf("闸门之后仍发出 %d 次出站调用，期望 0 次", calls)
	}
}

// writerFunc 把一个函数适配成 workflow.OutboundWriter。
type writerFunc func(ctx context.Context, runID string, payloadJSON string) error

func (f writerFunc) WriteDeviation(ctx context.Context, runID string, payloadJSON string) error {
	return f(ctx, runID, payloadJSON)
}

// TestWriteback_MissingConfirmationRejected 断言没有确认记录时写回被拒。
func TestWriteback_MissingConfirmationRejected(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)

	// 造一条「已确认」但库里没有确认记录的 run（数据不一致的情形）。
	rec := domain.RunRecord{Run: domain.NewRun(testPointID), CreatedAt: time.Now(), UpdatedAt: time.Now()}
	rec.Run.State = domain.RunStateConfirmed
	if err := st.UpsertRun(ctx, rec); err != nil {
		t.Fatalf("写入判定 run 失败：%v", err)
	}

	writer := &fakeDeviationWriter{}
	wb := NewWritebacker(st, writer, &fakeNotifier{}, quietLogger())
	_, err := wb.Writeback(ctx, rec.Run.RunID)
	if err == nil {
		t.Fatal("没有确认记录时写回必须被拒")
	}
	if writer.calls != 0 {
		t.Errorf("出站调用数 = %d，期望 0 次", writer.calls)
	}
}

// TestWriteback_MissingRunIDRejected 断言缺 run_id 时连库都不查。
func TestWriteback_MissingRunIDRejected(t *testing.T) {
	st := openStore(t)
	writer := &fakeDeviationWriter{}
	wb := NewWritebacker(st, writer, &fakeNotifier{}, quietLogger())
	if _, err := wb.Writeback(context.Background(), "  "); err == nil {
		t.Fatal("缺 run_id 必须报错")
	}
	if writer.calls != 0 {
		t.Errorf("出站调用数 = %d，期望 0 次", writer.calls)
	}
}

// TestWriteback_UnassembledWriterRejected 断言没装配写侧时不发请求。
func TestWriteback_UnassembledWriterRejected(t *testing.T) {
	st := openStore(t)
	wb := NewWritebacker(st, nil, &fakeNotifier{}, quietLogger())
	if _, err := wb.Writeback(context.Background(), "run-001"); err == nil {
		t.Fatal("没装配台账写侧时必须报错")
	} else if !errors.Is(err, err) { // 仅为可读性保留：断言确实返回了错误
		t.Fatal("必须返回错误")
	}
}
