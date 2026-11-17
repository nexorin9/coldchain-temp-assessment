package store

import (
	"context"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// 本文件验证 writeback_alerts 表（出站写回没落成的告警）与待补偿清单。
//
// 它盯住四件事：
//
//  1. 告警原样往返：run、目标、错误码、尝试次数、失败时刻都能读回来。
//  2. 次数是**累加**的：补发仍失败 → 尝试次数与失败轮次都往上加，
//     「这条越温的台账写入一共敲了多少次门」看得见。
//  3. 解掉不等于删掉：resolved 的告警仍在库里，留痕可回溯。
//  4. 待补偿清单只列还没写成的：已写成的那一处不再出现在清单里。

// TestUpsertWritebackAlert_RoundTrips 断言告警原样往返。
func TestUpsertWritebackAlert_RoundTrips(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	at := time.Date(2026, 10, 20, 11, 5, 0, 0, time.UTC)
	rec := domain.WritebackAlert{
		RunID:         "run-001",
		Target:        domain.WritebackTargetLedger,
		State:         domain.WritebackAlertStateOpen,
		ErrorCode:     string(errcode.LedgerUnavailable),
		ErrorDetail:   "冷链台账不可达",
		Attempts:      3,
		Failures:      1,
		FirstFailedAt: &at,
		LastFailedAt:  &at,
		CreatedAt:     at,
		UpdatedAt:     at,
	}
	if err := st.UpsertWritebackAlert(ctx, rec); err != nil {
		t.Fatalf("写入告警失败：%v", err)
	}

	got, err := st.WritebackAlertByTarget(ctx, "run-001", domain.WritebackTargetLedger)
	if err != nil {
		t.Fatalf("回读告警失败：%v", err)
	}
	if got.RunID != "run-001" || got.Target != domain.WritebackTargetLedger {
		t.Errorf("告警定位不对：%+v", got)
	}
	if !got.Open() {
		t.Errorf("告警状态 = %q，期望 open", got.State)
	}
	if got.ErrorCode != string(errcode.LedgerUnavailable) {
		t.Errorf("告警错误码 = %q，期望 E_LEDGER_UNAVAILABLE", got.ErrorCode)
	}
	if got.Attempts != 3 || got.Failures != 1 {
		t.Errorf("告警次数不对：attempts=%d failures=%d", got.Attempts, got.Failures)
	}
	if got.FirstFailedAt == nil || got.LastFailedAt == nil {
		t.Fatal("告警的失败时刻应当能取回")
	}
	if !got.FirstFailedAt.Equal(at) {
		t.Errorf("首次失败时刻 = %v，期望 %v", got.FirstFailedAt, at)
	}
	if got.ResolvedAt != nil {
		t.Errorf("未解决的告警不该带解决时刻：%v", got.ResolvedAt)
	}
}

// TestUpsertWritebackAlert_AccumulatesAttempts 断言次数累加：
// 同一处目标反复失败，尝试次数与失败轮次都往上加。
func TestUpsertWritebackAlert_AccumulatesAttempts(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	at := time.Date(2026, 10, 20, 11, 5, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		rec := domain.WritebackAlert{
			RunID:         "run-002",
			Target:        domain.WritebackTargetNotification,
			State:         domain.WritebackAlertStateOpen,
			ErrorCode:     string(errcode.NotifyUnavailable),
			Attempts:      3,
			FirstFailedAt: &at,
			LastFailedAt:  &at,
			CreatedAt:     at,
			UpdatedAt:     at,
		}
		if err := st.UpsertWritebackAlert(ctx, rec); err != nil {
			t.Fatalf("写入告警失败：%v", err)
		}
	}

	got, err := st.WritebackAlertByTarget(ctx, "run-002", domain.WritebackTargetNotification)
	if err != nil {
		t.Fatalf("回读告警失败：%v", err)
	}
	if got.Attempts != 9 {
		t.Errorf("累计尝试次数 = %d，期望 9（三轮各 3 次）", got.Attempts)
	}
	if got.Failures != 3 {
		t.Errorf("累计失败轮次 = %d，期望 3", got.Failures)
	}
}

// TestResolveWritebackAlert_KeepsRecord 断言解掉不等于删掉：
// 记录仍在库里，只是状态变成 resolved 且带上解决时刻。
func TestResolveWritebackAlert_KeepsRecord(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	at := time.Date(2026, 10, 20, 11, 5, 0, 0, time.UTC)
	resolvedAt := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err := st.UpsertWritebackAlert(ctx, domain.WritebackAlert{
		RunID:         "run-003",
		Target:        domain.WritebackTargetLedger,
		State:         domain.WritebackAlertStateOpen,
		ErrorCode:     string(errcode.LedgerUnavailable),
		Attempts:      3,
		FirstFailedAt: &at,
		LastFailedAt:  &at,
		CreatedAt:     at,
		UpdatedAt:     at,
	}); err != nil {
		t.Fatalf("写入告警失败：%v", err)
	}
	if err := st.ResolveWritebackAlert(ctx, "run-003", domain.WritebackTargetLedger, resolvedAt); err != nil {
		t.Fatalf("解决告警失败：%v", err)
	}

	got, err := st.WritebackAlertByTarget(ctx, "run-003", domain.WritebackTargetLedger)
	if err != nil {
		t.Fatalf("回读告警失败：%v", err)
	}
	if got.Open() {
		t.Error("解决之后告警不该还开着")
	}
	if got.State != domain.WritebackAlertStateResolved {
		t.Errorf("告警状态 = %q，期望 resolved", got.State)
	}
	if got.ResolvedAt == nil || !got.ResolvedAt.Equal(resolvedAt) {
		t.Errorf("解决时刻 = %v，期望 %v", got.ResolvedAt, resolvedAt)
	}

	// 开着的不再进清单，解掉的也不进。
	open, err := st.OpenWritebackAlerts(ctx)
	if err != nil {
		t.Fatalf("回读告警清单失败：%v", err)
	}
	if len(open) != 0 {
		t.Errorf("还挂着的告警数 = %d，期望 0", len(open))
	}
	count, err := st.CountOpenWritebackAlerts(ctx)
	if err != nil {
		t.Fatalf("统计告警失败：%v", err)
	}
	if count != 0 {
		t.Errorf("还挂着的告警数 = %d，期望 0", count)
	}
}

// TestPendingWritebacks_ListsOnlyNotWritten 断言待补偿清单：
// 已写成的那一处不再出现，没写成的一条不少。
func TestPendingWritebacks_ListsOnlyNotWritten(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	at := time.Date(2026, 10, 20, 11, 5, 0, 0, time.UTC)
	if err := st.UpsertWriteback(ctx, domain.WritebackRecord{
		RunID:     "run-004",
		Target:    domain.WritebackTargetLedger,
		State:     domain.WritebackTargetStateFailed,
		ErrorCode: string(errcode.LedgerUnavailable),
		CreatedAt: at,
		UpdatedAt: at,
	}); err != nil {
		t.Fatalf("写入写回记录失败：%v", err)
	}
	if err := st.UpsertWriteback(ctx, domain.WritebackRecord{
		RunID:     "run-004",
		Target:    domain.WritebackTargetNotification,
		State:     domain.WritebackTargetStateWritten,
		WrittenAt: &at,
		CreatedAt: at,
		UpdatedAt: at,
	}); err != nil {
		t.Fatalf("写入写回记录失败：%v", err)
	}

	pending, err := st.PendingWritebacks(ctx)
	if err != nil {
		t.Fatalf("回读待写回记录失败：%v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("待写回记录数 = %d，期望 1（已写成的那一处不列）", len(pending))
	}
	if pending[0].Target != domain.WritebackTargetLedger {
		t.Errorf("待写回目标 = %q，期望 ledger", pending[0].Target)
	}
	count, err := st.CountPendingWritebacks(ctx)
	if err != nil {
		t.Fatalf("统计待写回记录失败：%v", err)
	}
	if count != 1 {
		t.Errorf("待写回记录数 = %d，期望 1", count)
	}
}

// TestUpsertWritebackAlert_RejectsIncompleteRecord 断言坏记录被挡下：
// 缺 run_id 或 target 一律拒收，不入库。
func TestUpsertWritebackAlert_RejectsIncompleteRecord(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	if err := st.UpsertWritebackAlert(ctx, domain.WritebackAlert{Target: domain.WritebackTargetLedger}); err == nil {
		t.Error("缺 run_id 的告警应当被拒")
	}
	if err := st.UpsertWritebackAlert(ctx, domain.WritebackAlert{RunID: "run-005"}); err == nil {
		t.Error("缺 target 的告警应当被拒")
	}
	if _, err := st.WritebackAlertByTarget(ctx, "run-005", domain.WritebackTargetLedger); err == nil {
		t.Error("不存在的告警应当报 E_NOT_FOUND")
	}
}

// TestOpenWritebackAlerts_OrdersByUpdateTime 断言清单顺序：
// 最早失败的那条排在最前面（它等得最久）。
func TestOpenWritebackAlerts_OrdersByUpdateTime(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	early := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	late := time.Date(2026, 10, 20, 18, 0, 0, 0, time.UTC)
	for _, item := range []struct {
		runID string
		at    time.Time
	}{
		{"run-late", late},
		{"run-early", early},
	} {
		if err := st.UpsertWritebackAlert(ctx, domain.WritebackAlert{
			RunID:         item.runID,
			Target:        domain.WritebackTargetLedger,
			State:         domain.WritebackAlertStateOpen,
			ErrorCode:     string(errcode.LedgerUnavailable),
			Attempts:      3,
			FirstFailedAt: &item.at,
			LastFailedAt:  &item.at,
			CreatedAt:     item.at,
			UpdatedAt:     item.at,
		}); err != nil {
			t.Fatalf("写入告警失败：%v", err)
		}
	}

	alerts, err := st.OpenWritebackAlerts(ctx)
	if err != nil {
		t.Fatalf("回读告警清单失败：%v", err)
	}
	if len(alerts) != 2 {
		t.Fatalf("告警数 = %d，期望 2", len(alerts))
	}
	if alerts[0].RunID != "run-early" {
		t.Errorf("清单首条 = %q，期望 run-early（最早失败的排最前）", alerts[0].RunID)
	}
	count, err := st.CountOpenWritebackAlerts(ctx)
	if err != nil {
		t.Fatalf("统计告警失败：%v", err)
	}
	if count != 2 {
		t.Errorf("还挂着的告警数 = %d，期望 2", count)
	}
}
