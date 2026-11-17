package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
)

// testAuditEntry 造一条审计记录。
func testAuditEntry(runID string, seq int, entryType string) domain.AuditEntry {
	amount := 0.0024
	return domain.AuditEntry{
		RunID:            runID,
		Seq:              seq,
		EntryType:        entryType,
		StepName:         "attribute",
		SchemaName:       "attribution",
		Model:            "院内判定模型",
		Outcome:          domain.AuditOutcomeSucceeded,
		LatencyMS:        812,
		PromptTokens:     900,
		CompletionTokens: 120,
		TotalTokens:      1020,
		CostAmount:       &amount,
		CostCurrency:     "CNY",
		CostStatus:       domain.CostStatusKnown,
		PromptDigest:     "0123456789abcdef",
		PromptChars:      35,
		CreatedAt:        time.Date(2026, 10, 26, 2, 30, 0, 0, time.UTC),
	}
}

// TestRecordAuditEntry_RoundTripsAllFields 验证审计记录原样往返。
//
// 审计链是账本：写进去什么，读出来就得是什么。差一个字段，
// 复核时那条记录就答不了它本该回答的问题。
func TestRecordAuditEntry_RoundTripsAllFields(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	want := testAuditEntry("run-audit-001", 1, domain.AuditEntryModelCall)
	if err := st.RecordAuditEntry(ctx, want); err != nil {
		t.Fatalf("写入审计记录失败：%v", err)
	}

	entries, err := st.AuditEntriesByRun(ctx, "run-audit-001")
	if err != nil {
		t.Fatalf("回读审计记录失败：%v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("审计记录条数 = %d，期望 1", len(entries))
	}
	got := entries[0]
	if got.RunID != want.RunID || got.Seq != want.Seq || got.EntryType != want.EntryType {
		t.Errorf("身份字段 = %q/%d/%q，期望 %q/%d/%q",
			got.RunID, got.Seq, got.EntryType, want.RunID, want.Seq, want.EntryType)
	}
	if got.Model != want.Model || got.Outcome != want.Outcome {
		t.Errorf("模型与状态 = %q/%q，期望 %q/%q", got.Model, got.Outcome, want.Model, want.Outcome)
	}
	if got.LatencyMS != want.LatencyMS {
		t.Errorf("latency_ms = %d，期望 %d", got.LatencyMS, want.LatencyMS)
	}
	if got.PromptTokens != want.PromptTokens || got.CompletionTokens != want.CompletionTokens ||
		got.TotalTokens != want.TotalTokens {
		t.Errorf("token 字段 = %d/%d/%d，期望 %d/%d/%d",
			got.PromptTokens, got.CompletionTokens, got.TotalTokens,
			want.PromptTokens, want.CompletionTokens, want.TotalTokens)
	}
	if got.CostAmount == nil || *got.CostAmount != *want.CostAmount {
		t.Errorf("cost_amount = %v，期望 %v", got.CostAmount, *want.CostAmount)
	}
	if got.CostCurrency != "CNY" || got.CostStatus != domain.CostStatusKnown {
		t.Errorf("成本币种与状态 = %q/%q，期望 CNY/%s",
			got.CostCurrency, got.CostStatus, domain.CostStatusKnown)
	}
	if got.PromptDigest != want.PromptDigest || got.PromptChars != want.PromptChars {
		t.Errorf("提示词指纹 = %q/%d，期望 %q/%d",
			got.PromptDigest, got.PromptChars, want.PromptDigest, want.PromptChars)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("created_at = %v，期望 %v", got.CreatedAt, want.CreatedAt)
	}
}

// TestRecordAuditEntry_UnknownCostKeepsAmountEmpty 验证算不出成本时金额为空而不是 0。
func TestRecordAuditEntry_UnknownCostKeepsAmountEmpty(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	entry := testAuditEntry("run-audit-002", 1, domain.AuditEntryModelCall)
	entry.CostAmount = nil
	entry.CostStatus = domain.CostStatusUnknown
	entry.CostCurrency = ""
	if err := st.RecordAuditEntry(ctx, entry); err != nil {
		t.Fatalf("写入审计记录失败：%v", err)
	}

	entries, err := st.AuditEntriesByRun(ctx, "run-audit-002")
	if err != nil {
		t.Fatalf("回读审计记录失败：%v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("审计记录条数 = %d，期望 1", len(entries))
	}
	if entries[0].CostAmount != nil {
		t.Errorf("cost_amount = %v，期望为空（算不出就不是 0）", *entries[0].CostAmount)
	}
	if entries[0].CostStatus != domain.CostStatusUnknown {
		t.Errorf("cost_status = %q，期望 %q", entries[0].CostStatus, domain.CostStatusUnknown)
	}
}

// TestRecordAuditEntry_SeqAssignedInOrder 验证序号在 run 内递增。
//
// 模型调用与人工确认共用这一条序号线，合起来才是完整的判定过程。
func TestRecordAuditEntry_SeqAssignedInOrder(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	if err := st.RecordAuditEntry(ctx, testAuditEntry("run-audit-003", 0, domain.AuditEntryModelCall)); err != nil {
		t.Fatalf("写入模型调用记录失败：%v", err)
	}
	confirmation := testAuditEntry("run-audit-003", 0, domain.AuditEntryConfirmation)
	confirmation.Actor = "supervisor-007"
	confirmation.Action = "confirm"
	confirmation.FromState = "awaiting_confirmation"
	confirmation.ToState = "confirmed"
	if err := st.RecordAuditEntry(ctx, confirmation); err != nil {
		t.Fatalf("写入确认动作记录失败：%v", err)
	}

	entries, err := st.AuditEntriesByRun(ctx, "run-audit-003")
	if err != nil {
		t.Fatalf("回读审计记录失败：%v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("审计记录条数 = %d，期望 2", len(entries))
	}
	if entries[0].Seq != 1 || entries[1].Seq != 2 {
		t.Errorf("序号 = %d/%d，期望 1/2", entries[0].Seq, entries[1].Seq)
	}
	if entries[0].EntryType != domain.AuditEntryModelCall ||
		entries[1].EntryType != domain.AuditEntryConfirmation {
		t.Errorf("记录类型 = %q/%q，期望 %q/%q",
			entries[0].EntryType, entries[1].EntryType,
			domain.AuditEntryModelCall, domain.AuditEntryConfirmation)
	}
	if entries[1].Actor != "supervisor-007" {
		t.Errorf("确认人 = %q，期望 supervisor-007", entries[1].Actor)
	}
}

// TestRecordAuditEntry_ReplayUpdatesSameRow 验证同一个序号的重放只更新那一行。
//
// 重放是常态（进程重启后续跑）：插两行会让「这次判定问了几次模型」对不上。
func TestRecordAuditEntry_ReplayUpdatesSameRow(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	entry := testAuditEntry("run-audit-004", 1, domain.AuditEntryModelCall)
	if err := st.RecordAuditEntry(ctx, entry); err != nil {
		t.Fatalf("写入审计记录失败：%v", err)
	}
	entry.LatencyMS = 1500
	if err := st.RecordAuditEntry(ctx, entry); err != nil {
		t.Fatalf("重放审计记录失败：%v", err)
	}

	entries, err := st.AuditEntriesByRun(ctx, "run-audit-004")
	if err != nil {
		t.Fatalf("回读审计记录失败：%v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("审计记录条数 = %d，期望 1（重放只更新那一行）", len(entries))
	}
	if entries[0].LatencyMS != 1500 {
		t.Errorf("latency_ms = %d，期望 1500（重放更新了那一行）", entries[0].LatencyMS)
	}
}

// TestRecordAuditEntry_RejectsMissingIdentity 验证缺身份的记录不入库。
func TestRecordAuditEntry_RejectsMissingIdentity(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	missingRun := testAuditEntry("", 1, domain.AuditEntryModelCall)
	if err := st.RecordAuditEntry(ctx, missingRun); err == nil {
		t.Error("缺 run_id 的审计记录应被拒收")
	}
	missingType := testAuditEntry("run-audit-005", 1, "")
	if err := st.RecordAuditEntry(ctx, missingType); err == nil {
		t.Error("缺 entry_type 的审计记录应被拒收")
	}
}

// TestAuditEntriesByRun_EmptyAndMissingRun 验证查不到的 run 回空链而不是报错。
func TestAuditEntriesByRun_EmptyAndMissingRun(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	entries, err := st.AuditEntriesByRun(ctx, "run-does-not-exist")
	if err != nil {
		t.Fatalf("回读审计记录失败：%v", err)
	}
	if len(entries) != 0 {
		t.Errorf("审计记录条数 = %d，期望 0", len(entries))
	}
	if _, err := st.AuditEntriesByRun(ctx, "  "); err == nil {
		t.Error("空 run_id 应被拒收")
	}
}

// TestOpen_CreatesAuditEntriesTableForLegacyDB 验证存量库补表后老数据不丢。
func TestOpen_CreatesAuditEntriesTableForLegacyDB(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/legacy.db"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开库失败：%v", err)
	}
	if _, err := db.Exec("CREATE TABLE schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		t.Fatalf("造老库失败：%v", err)
	}
	if _, err := db.Exec("INSERT INTO schema_meta (key, value) VALUES ('schema_version', '3')"); err != nil {
		t.Fatalf("写老库版本号失败：%v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭老库失败：%v", err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("打开老库失败：%v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	if err := st.RecordAuditEntry(ctx, testAuditEntry("run-audit-006", 1, domain.AuditEntryModelCall)); err != nil {
		t.Fatalf("老库补表后写入失败：%v", err)
	}
	count, err := st.CountAuditEntries(ctx, "run-audit-006")
	if err != nil {
		t.Fatalf("统计审计记录失败：%v", err)
	}
	if count != 1 {
		t.Fatalf("老库补表后应有 1 行记录，实际 %d 行", count)
	}
}
