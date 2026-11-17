package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// confirmFixedAt 是确认记录的固定时刻，让断言可复现。
var confirmFixedAt = time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)

// testConfirmation 造一条确认记录。
func testConfirmation(runID string) domain.Confirmation {
	return domain.Confirmation{
		RunID:            runID,
		Action:           domain.ConfirmationActionConfirm,
		ConfirmedBy:      "supervisor-007",
		ConfirmedAt:      confirmFixedAt,
		Disposition:      "isolate",
		DispositionLabel: "隔离",
		Opinion:          "已联系疫苗生产企业评估，先隔离",
		PayloadJSON:      `{"external_ref":"cc-point-vax-freezer-01-20261019T191000Z","disposition":"isolate"}`,
		CreatedAt:        confirmFixedAt,
	}
}

// TestSaveConfirmation_RoundTrips 覆盖确认记录的往返：
// 谁、何时、什么处置、什么意见、固化的载荷，逐项原样取回。
func TestSaveConfirmation_RoundTrips(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	rec := testConfirmation("run-confirm-001")
	if err := st.SaveConfirmation(ctx, rec); err != nil {
		t.Fatalf("写入确认记录失败：%v", err)
	}

	got, err := st.ConfirmationByRun(ctx, "run-confirm-001")
	if err != nil {
		t.Fatalf("回读确认记录失败：%v", err)
	}
	if got.Action != domain.ConfirmationActionConfirm {
		t.Errorf("动作类型不对：%q", got.Action)
	}
	if got.ConfirmedBy != "supervisor-007" {
		t.Errorf("确认人不对：%q", got.ConfirmedBy)
	}
	if !got.ConfirmedAt.Equal(confirmFixedAt) {
		t.Errorf("确认时刻不对：%v", got.ConfirmedAt)
	}
	if got.Disposition != "isolate" || got.DispositionLabel != "隔离" {
		t.Errorf("处置结论不对：%q / %q", got.Disposition, got.DispositionLabel)
	}
	if got.Opinion != rec.Opinion {
		t.Errorf("意见不对：%q", got.Opinion)
	}
	if got.PayloadJSON != rec.PayloadJSON {
		t.Errorf("固化的载荷不对：%q", got.PayloadJSON)
	}
}

// TestSaveConfirmation_RepeatedSaveKeepsOneRow 覆盖「一次 run 一行」：
// 重复确认更新那一行，不插出第二条。
func TestSaveConfirmation_RepeatedSaveKeepsOneRow(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	for i := 0; i < 3; i++ {
		if err := st.SaveConfirmation(ctx, testConfirmation("run-confirm-002")); err != nil {
			t.Fatalf("写入确认记录失败：%v", err)
		}
	}
	var count int
	if err := st.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM run_confirmations WHERE run_id = ?", "run-confirm-002").Scan(&count); err != nil {
		t.Fatalf("统计确认记录失败：%v", err)
	}
	if count != 1 {
		t.Fatalf("重复确认后应当只有 1 行，实际 %d 行", count)
	}
}

// TestConfirmationByRun_NotFound 覆盖「查不到这条确认」：
// 返回 E_NOT_FOUND——写回侧据此判断「这条判定还没人确认过」。
func TestConfirmationByRun_NotFound(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	_, err := st.ConfirmationByRun(ctx, "run-not-exist")
	if err == nil {
		t.Fatal("查不到的确认记录应报错")
	}
	if code := errcode.As(err).Code; code != errcode.NotFound {
		t.Fatalf("错误码不对：%v", err)
	}
}

// TestSaveConfirmation_MissingIdentityRejected 覆盖落库前的身份校验：
// 缺 run_id 或缺动作类型一律拒收，不落半条记录。
func TestSaveConfirmation_MissingIdentityRejected(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	rec := testConfirmation("")
	if err := st.SaveConfirmation(ctx, rec); err == nil {
		t.Error("缺 run_id 的确认记录不得入库")
	}
	rec = testConfirmation("run-confirm-003")
	rec.Action = ""
	if err := st.SaveConfirmation(ctx, rec); err == nil {
		t.Error("缺动作类型的确认记录不得入库")
	}
}

// TestRunState_ReadsBusinessState 覆盖出站写回闸门要的读口：
// 回读得到的是 run 的业务状态；run 不存在时报 E_NOT_FOUND。
func TestRunState_ReadsBusinessState(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	run := domain.NewRun("point-vax-freezer-01")
	if err := st.UpsertRun(ctx, domain.RunRecord{Run: run, UpdatedAt: confirmFixedAt}); err != nil {
		t.Fatalf("写入判定 run 失败：%v", err)
	}
	state, err := st.RunState(ctx, run.RunID)
	if err != nil {
		t.Fatalf("回读业务状态失败：%v", err)
	}
	if state != domain.RunStateDetected {
		t.Errorf("业务状态不对：%q", state)
	}
	if _, err := st.RunState(ctx, "run-not-exist"); err == nil {
		t.Error("查不到的 run 应当报错")
	}
}

// TestOpen_CreatesConfirmationsTableForLegacyDB 验证存量库补表后老数据不丢：
// 老库里没有 run_confirmations 表，打开后补齐并可正常写入。
func TestOpen_CreatesConfirmationsTableForLegacyDB(t *testing.T) {
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
	if err := st.SaveConfirmation(ctx, testConfirmation("run-confirm-legacy")); err != nil {
		t.Fatalf("老库补表后写入确认记录失败：%v", err)
	}
	got, err := st.ConfirmationByRun(ctx, "run-confirm-legacy")
	if err != nil {
		t.Fatalf("老库补表后回读确认记录失败：%v", err)
	}
	if got.ConfirmedBy != "supervisor-007" {
		t.Errorf("老库补表后确认人不对：%q", got.ConfirmedBy)
	}
}

// TestOpen_CreatesAuditDispositionColumnsForLegacyDB 验证存量审计表补列：
// 老库里没有 disposition / opinion 两列，补齐后老记录仍在、新记录写得下。
func TestOpen_CreatesAuditDispositionColumnsForLegacyDB(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/legacy-audit.db"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开库失败：%v", err)
	}
	if _, err := db.Exec("CREATE TABLE schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		t.Fatalf("造老库失败：%v", err)
	}
	// 老审计表照 v5 的形状造：那时候还没有 disposition / opinion 两列。
	if _, err := db.Exec(`CREATE TABLE run_audit_entries (
		run_id TEXT NOT NULL, seq INTEGER NOT NULL, entry_type TEXT NOT NULL,
		step_name TEXT NOT NULL DEFAULT '', schema_name TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '', outcome TEXT NOT NULL DEFAULT '',
		degraded INTEGER NOT NULL DEFAULT 0, error_code TEXT NOT NULL DEFAULT '',
		latency_ms INTEGER NOT NULL DEFAULT 0, prompt_tokens INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0, total_tokens INTEGER NOT NULL DEFAULT 0,
		cost_amount REAL, cost_currency TEXT NOT NULL DEFAULT '',
		cost_status TEXT NOT NULL DEFAULT 'unknown',
		actor TEXT NOT NULL DEFAULT '', action TEXT NOT NULL DEFAULT '',
		from_state TEXT NOT NULL DEFAULT '', to_state TEXT NOT NULL DEFAULT '',
		details TEXT NOT NULL DEFAULT '', prompt_digest TEXT NOT NULL DEFAULT '',
		prompt_chars INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL,
		PRIMARY KEY (run_id, seq))`); err != nil {
		t.Fatalf("造老审计表失败：%v", err)
	}
	if _, err := db.Exec(
		"INSERT INTO run_audit_entries (run_id, seq, entry_type, actor, action, created_at) " +
			"VALUES ('run-old', 1, 'confirmation', 'supervisor-001', 'confirm', '2026-10-20T03:00:00.000000000Z')"); err != nil {
		t.Fatalf("写老审计记录失败：%v", err)
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
	entries, err := st.AuditEntriesByRun(ctx, "run-old")
	if err != nil {
		t.Fatalf("回读老审计记录失败：%v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("老审计记录应当仍在，实际 %d 条", len(entries))
	}
	if entries[0].Actor != "supervisor-001" {
		t.Errorf("老记录的执行者不对：%q", entries[0].Actor)
	}
	// 新列对老记录是空的：空表示「当时没记」，不是「记了个空值」。
	if entries[0].Disposition != "" || entries[0].Opinion != "" {
		t.Errorf("老记录的新列应为空：%q / %q", entries[0].Disposition, entries[0].Opinion)
	}

	// 新记录写得下这两列。
	if err := st.RecordAuditEntry(ctx, domain.AuditEntry{
		RunID:       "run-old",
		Seq:         2,
		EntryType:   domain.AuditEntryConfirmation,
		Actor:       "supervisor-007",
		Action:      "confirm",
		Disposition: "isolate",
		Opinion:     "先隔离",
		CreatedAt:   confirmFixedAt,
	}); err != nil {
		t.Fatalf("补列后写入新审计记录失败：%v", err)
	}
	entries, err = st.AuditEntriesByRun(ctx, "run-old")
	if err != nil {
		t.Fatalf("回读审计记录失败：%v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("补列后应有 2 条记录，实际 %d 条", len(entries))
	}
	if entries[1].Disposition != "isolate" || entries[1].Opinion != "先隔离" {
		t.Errorf("新记录的处置与意见不对：%q / %q", entries[1].Disposition, entries[1].Opinion)
	}
}
