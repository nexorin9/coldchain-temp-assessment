package store

import (
	"context"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// 本文件验证 run_writebacks 表（出站写回按目标分别记账）。
//
// 它盯住四件事：
//
//  1. 一处目标一行：台账一行、通知一行，不多不少。
//  2. 反复记账更新同一行：补偿重放试几次都只留一行。
//  3. 「已写成」与「失败」分得清：补偿重放据此只补没写成的那一处。
//  4. 写入时刻随成功落库：飞检翻台账要能看到「什么时候写进去的」。

// TestUpsertWriteback_RoundTrips 断言写回记录原样往返。
func TestUpsertWriteback_RoundTrips(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	at := time.Date(2026, 10, 20, 11, 5, 0, 0, time.UTC)
	rec := domain.WritebackRecord{
		RunID:          "run-001",
		Target:         domain.WritebackTargetLedger,
		State:          domain.WritebackTargetStateWritten,
		ExternalRef:    "cc-point-vax-freezer-01-20261019T191000Z",
		IdempotencyKey: "key-001",
		WrittenAt:      &at,
		CreatedAt:      at,
		UpdatedAt:      at,
	}
	if err := st.UpsertWriteback(ctx, rec); err != nil {
		t.Fatalf("写入写回记录失败：%v", err)
	}

	got, err := st.WritebackByTarget(ctx, "run-001", domain.WritebackTargetLedger)
	if err != nil {
		t.Fatalf("回读写回记录失败：%v", err)
	}
	if got.State != domain.WritebackTargetStateWritten {
		t.Errorf("状态 = %q，期望 %q", got.State, domain.WritebackTargetStateWritten)
	}
	if got.ExternalRef != rec.ExternalRef {
		t.Errorf("外部引用 = %q，期望 %q", got.ExternalRef, rec.ExternalRef)
	}
	if got.IdempotencyKey != rec.IdempotencyKey {
		t.Errorf("幂等键 = %q，期望 %q", got.IdempotencyKey, rec.IdempotencyKey)
	}
	if got.WrittenAt == nil {
		t.Fatal("写入时刻不应为空")
	}
	if !got.Written() {
		t.Error("已写成的记录应当报出已写成")
	}
}

// TestUpsertWriteback_TwoTargetsOneRowEach 断言台账与通知各一行。
func TestUpsertWriteback_TwoTargetsOneRowEach(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	now := time.Date(2026, 10, 20, 11, 5, 0, 0, time.UTC)
	for _, target := range []string{
		domain.WritebackTargetLedger,
		domain.WritebackTargetNotification,
	} {
		if err := st.UpsertWriteback(ctx, domain.WritebackRecord{
			RunID:     "run-001",
			Target:    target,
			State:     domain.WritebackTargetStateWritten,
			CreatedAt: now,
			UpdatedAt: now,
		}); err != nil {
			t.Fatalf("写入 %s 写回记录失败：%v", target, err)
		}
	}

	records, err := st.WritebacksByRun(ctx, "run-001")
	if err != nil {
		t.Fatalf("回读写回记录失败：%v", err)
	}
	if len(records) != 2 {
		t.Fatalf("写回记录 = %d 行，期望 2 行（台账一行、通知一行）", len(records))
	}
	if records[0].Target != domain.WritebackTargetLedger ||
		records[1].Target != domain.WritebackTargetNotification {
		t.Errorf("写回记录的目标不对：%q / %q", records[0].Target, records[1].Target)
	}
}

// TestUpsertWriteback_RetryUpdatesSameRow 断言重试更新同一行而不是插出第二行。
func TestUpsertWriteback_RetryUpdatesSameRow(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	now := time.Date(2026, 10, 20, 11, 5, 0, 0, time.UTC)
	if err := st.UpsertWriteback(ctx, domain.WritebackRecord{
		RunID:     "run-001",
		Target:    domain.WritebackTargetLedger,
		State:     domain.WritebackTargetStateFailed,
		ErrorCode: string(errcode.LedgerUnavailable),
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("写入失败记录失败：%v", err)
	}
	if err := st.UpsertWriteback(ctx, domain.WritebackRecord{
		RunID:     "run-001",
		Target:    domain.WritebackTargetLedger,
		State:     domain.WritebackTargetStateWritten,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("写入成功记录失败：%v", err)
	}

	records, err := st.WritebacksByRun(ctx, "run-001")
	if err != nil {
		t.Fatalf("回读写回记录失败：%v", err)
	}
	if len(records) != 1 {
		t.Fatalf("写回记录 = %d 行，期望 1 行（重试更新同一行）", len(records))
	}
	if !records[0].Written() {
		t.Errorf("状态 = %q，期望已写成", records[0].State)
	}
	if records[0].ErrorCode != "" {
		t.Errorf("成功记录的失败错误码应被清空，实际 %q", records[0].ErrorCode)
	}
}

// TestWritebackByTarget_NotFound 断言没有记录时报「还没写过」而不是空记录。
func TestWritebackByTarget_NotFound(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	_, err := st.WritebackByTarget(ctx, "run-404", domain.WritebackTargetLedger)
	if err == nil {
		t.Fatal("没有记录时必须报错")
	}
	if code := errcode.As(err).Code; code != errcode.NotFound {
		t.Errorf("错误码 = %q，期望 %q", code, errcode.NotFound)
	}
}

// TestUpsertWriteback_MissingIdentityRejected 断言缺 run_id 或 target 时拒收。
func TestUpsertWriteback_MissingIdentityRejected(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	if err := st.UpsertWriteback(ctx, domain.WritebackRecord{Target: domain.WritebackTargetLedger}); err == nil {
		t.Error("缺 run_id 必须报错")
	}
	if err := st.UpsertWriteback(ctx, domain.WritebackRecord{RunID: "run-001"}); err == nil {
		t.Error("缺 target 必须报错")
	}
}
