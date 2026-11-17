// 本文件落「审计链」的记录：run_audit_entries 表。
//
// 为什么单独一张表：审计链是账本，不是流水。判定结论、步骤产物、台账写回
// 各自有各自的表，而「这次判定用了哪个模型、花了多少 token、最后谁确认的」
// 这条线是独立的——它回答的是事后复核的问题，不是业务状态本身。
//
// 三条约定：
//   - 一条记录一个序号：主键 (run_id, seq)，序号在 run 内递增。
//     模型调用与人工确认共用这一条序号线，合起来才是完整的判定过程。
//   - 只追加不改写：同一个 (run_id, seq) 的重放只更新那一行；
//     新的一次调用一定拿到新序号——两次调用就该是两条记录。
//   - 成本可算性落库：cost_status 记 known / unknown，
//     cost_amount 为空表示算不出来，绝不用 0 冒充「没花钱」。
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// auditEntriesSchemaSQL 是 run_audit_entries 表的建表脚本。
//
// 全部 IF NOT EXISTS：重复执行即幂等，服务每次启动跑一遍即可。
const auditEntriesSchemaSQL = `
CREATE TABLE IF NOT EXISTS run_audit_entries (
    run_id            TEXT NOT NULL,
    seq               INTEGER NOT NULL,
    entry_type        TEXT NOT NULL,
    step_name         TEXT NOT NULL DEFAULT '',
    schema_name       TEXT NOT NULL DEFAULT '',
    model             TEXT NOT NULL DEFAULT '',
    outcome           TEXT NOT NULL DEFAULT '',
    degraded          INTEGER NOT NULL DEFAULT 0,
    error_code        TEXT NOT NULL DEFAULT '',
    latency_ms        INTEGER NOT NULL DEFAULT 0,
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens      INTEGER NOT NULL DEFAULT 0,
    cost_amount       REAL,
    cost_currency     TEXT NOT NULL DEFAULT '',
    cost_status       TEXT NOT NULL DEFAULT 'unknown',
    actor             TEXT NOT NULL DEFAULT '',
    action            TEXT NOT NULL DEFAULT '',
    disposition       TEXT NOT NULL DEFAULT '',
    opinion           TEXT NOT NULL DEFAULT '',
    from_state        TEXT NOT NULL DEFAULT '',
    to_state          TEXT NOT NULL DEFAULT '',
    details           TEXT NOT NULL DEFAULT '',
    prompt_digest     TEXT NOT NULL DEFAULT '',
    prompt_chars      INTEGER NOT NULL DEFAULT 0,
    created_at        TEXT NOT NULL,
    PRIMARY KEY (run_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_run_audit_entries_type ON run_audit_entries (run_id, entry_type, seq);
`

// auditEntriesColumnDDL 是给存量审计表补列用的语句表。
//
// 处置结论与意见是后来才加的两列：老库里没有，补齐后老记录仍在，
// 只是这两栏是空的——空表示「当时没记」，不是「记了个空值」。
var auditEntriesColumnDDL = []struct {
	column string
	ddl    string
}{
	{"disposition", "ALTER TABLE run_audit_entries ADD COLUMN disposition TEXT NOT NULL DEFAULT ''"},
	{"opinion", "ALTER TABLE run_audit_entries ADD COLUMN opinion TEXT NOT NULL DEFAULT ''"},
}

// insertAuditEntrySQL 是审计条目入库语句。
//
// ON CONFLICT DO UPDATE：同一个 (run_id, seq) 的重放更新那一行，
// 而不是插出第二行——重放是常态（进程重启后续跑），插两行会让
// 「这次判定问了几次模型」这件事对不上。
const insertAuditEntrySQL = `
INSERT INTO run_audit_entries (
    run_id, seq, entry_type, step_name, schema_name, model, outcome, degraded,
    error_code, latency_ms, prompt_tokens, completion_tokens, total_tokens,
    cost_amount, cost_currency, cost_status, actor, action, disposition, opinion,
    from_state, to_state, details, prompt_digest, prompt_chars, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(run_id, seq) DO UPDATE SET
    entry_type        = excluded.entry_type,
    step_name         = excluded.step_name,
    schema_name       = excluded.schema_name,
    model             = excluded.model,
    outcome           = excluded.outcome,
    degraded          = excluded.degraded,
    error_code        = excluded.error_code,
    latency_ms        = excluded.latency_ms,
    prompt_tokens     = excluded.prompt_tokens,
    completion_tokens = excluded.completion_tokens,
    total_tokens      = excluded.total_tokens,
    cost_amount       = excluded.cost_amount,
    cost_currency     = excluded.cost_currency,
    cost_status       = excluded.cost_status,
    actor             = excluded.actor,
    action            = excluded.action,
    disposition       = excluded.disposition,
    opinion           = excluded.opinion,
    from_state        = excluded.from_state,
    to_state          = excluded.to_state,
    details           = excluded.details,
    prompt_digest     = excluded.prompt_digest,
    prompt_chars      = excluded.prompt_chars,
    created_at        = excluded.created_at
`

// prepareAuditEntries 建 run_audit_entries 表（幂等）。
func (s *Store) prepareAuditEntries(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, auditEntriesSchemaSQL); err != nil {
		return errcode.Newf(errcode.StorageError, "建 run_audit_entries 表失败：%v", err)
	}
	for _, item := range auditEntriesColumnDDL {
		if err := s.ensureColumn(ctx, "run_audit_entries", item.column, item.ddl); err != nil {
			return err
		}
	}
	return nil
}

// RecordAuditEntry 写入（或更新）一条审计记录。
//
// 它满足审计层的 Sink 接口：审计层不碰数据库，只把组装好的条目交出来。
//
// 序号分配：调用方没给序号时，取该 run 当前最大序号 +1。
// 取与写在同一个事务里完成，避免两次并发写入抢到同一个序号。
func (s *Store) RecordAuditEntry(ctx context.Context, entry domain.AuditEntry) error {
	runID := strings.TrimSpace(entry.RunID)
	if runID == "" {
		return errcode.Newf(errcode.Validation, "审计记录缺少 run_id，无法入库")
	}
	entryType := strings.TrimSpace(entry.EntryType)
	if entryType == "" {
		return errcode.Newf(errcode.Validation, "审计记录缺少 entry_type，无法入库")
	}
	createdAt := entry.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	costStatus := entry.CostStatus
	if costStatus == "" {
		costStatus = domain.CostStatusUnknown
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return errcode.Newf(errcode.StorageError, "开启审计记录写入事务失败：%v", err)
	}
	defer func() { _ = tx.Rollback() }()

	seq := entry.Seq
	if seq <= 0 {
		var maxSeq sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			"SELECT MAX(seq) FROM run_audit_entries WHERE run_id = ?", runID).Scan(&maxSeq); err != nil {
			return errcode.Newf(errcode.StorageError, "读取审计记录序号失败：%v", err)
		}
		seq = int(maxSeq.Int64) + 1
	}
	entryID := entry.EntryID
	if strings.TrimSpace(entryID) == "" {
		entryID = auditEntryID(runID, seq, entryType)
	}

	if _, err := tx.ExecContext(ctx, insertAuditEntrySQL,
		runID, seq, entryType,
		entry.StepName, entry.SchemaName, entry.Model, entry.Outcome, boolToInt(entry.Degraded),
		entry.ErrorCode, entry.LatencyMS, entry.PromptTokens, entry.CompletionTokens, entry.TotalTokens,
		entry.CostAmount, entry.CostCurrency, costStatus,
		entry.Actor, entry.Action, entry.Disposition, entry.Opinion,
		entry.FromState, entry.ToState,
		entry.Details, entry.PromptDigest, entry.PromptChars,
		createdAt.UTC().Format(tsLayout),
	); err != nil {
		return errcode.Newf(errcode.StorageError, "写入审计记录失败：%v", err)
	}
	if err := tx.Commit(); err != nil {
		return errcode.Newf(errcode.StorageError, "提交审计记录写入事务失败：%v", err)
	}
	return nil
}

// AuditEntriesByRun 回读一条 run 的全部审计记录，按序号升序。
//
// 序号即发生的先后：审计链上的顺序不能靠调用方自己排。
func (s *Store) AuditEntriesByRun(ctx context.Context, runID string) ([]domain.AuditEntry, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, errcode.Newf(errcode.Validation, "回读审计记录需要 run_id")
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT run_id, seq, entry_type, step_name, schema_name, model, outcome, degraded, "+
			"error_code, latency_ms, prompt_tokens, completion_tokens, total_tokens, "+
			"cost_amount, cost_currency, cost_status, actor, action, disposition, opinion, "+
			"from_state, to_state, details, prompt_digest, prompt_chars, created_at "+
			"FROM run_audit_entries WHERE run_id = ? ORDER BY seq ASC",
		runID)
	if err != nil {
		return nil, errcode.Newf(errcode.StorageError, "回读审计记录失败：%v", err)
	}
	defer func() { _ = rows.Close() }()

	entries := make([]domain.AuditEntry, 0, 8)
	for rows.Next() {
		entry, err := scanAuditEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, errcode.Newf(errcode.StorageError, "回读审计记录失败：%v", err)
	}
	return entries, nil
}

// CountAuditEntries 统计一条 run 的审计记录条数。
func (s *Store) CountAuditEntries(ctx context.Context, runID string) (int, error) {
	if strings.TrimSpace(runID) == "" {
		return 0, errcode.Newf(errcode.Validation, "统计审计记录需要 run_id")
	}
	var total int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM run_audit_entries WHERE run_id = ?", runID).Scan(&total); err != nil {
		return 0, errcode.Newf(errcode.StorageError, "统计审计记录失败：%v", err)
	}
	return total, nil
}

// scanAuditEntry 把一行审计记录还原成领域记录。
func scanAuditEntry(rows *sql.Rows) (domain.AuditEntry, error) {
	var (
		entry      domain.AuditEntry
		degraded   int
		costAmount sql.NullFloat64
		createdAt  string
	)
	if err := rows.Scan(
		&entry.RunID, &entry.Seq, &entry.EntryType,
		&entry.StepName, &entry.SchemaName, &entry.Model, &entry.Outcome, &degraded,
		&entry.ErrorCode, &entry.LatencyMS, &entry.PromptTokens, &entry.CompletionTokens,
		&entry.TotalTokens, &costAmount, &entry.CostCurrency, &entry.CostStatus,
		&entry.Actor, &entry.Action, &entry.Disposition, &entry.Opinion,
		&entry.FromState, &entry.ToState,
		&entry.Details, &entry.PromptDigest, &entry.PromptChars, &createdAt,
	); err != nil {
		return domain.AuditEntry{}, errcode.Newf(errcode.StorageError, "读取审计记录行失败：%v", err)
	}
	entry.Degraded = degraded != 0
	if costAmount.Valid {
		amount := costAmount.Float64
		entry.CostAmount = &amount
	}
	parsed, err := time.Parse(tsLayout, createdAt)
	if err != nil {
		return domain.AuditEntry{}, errcode.Newf(errcode.StorageError,
			"还原审计记录 created_at 失败：%v", err)
	}
	entry.CreatedAt = parsed
	entry.EntryID = auditEntryID(entry.RunID, entry.Seq, entry.EntryType)
	return entry, nil
}

// auditEntryID 按 run + 序号 + 类型生成稳定的记录标识。
//
// 内容派生而非随机：同一条记录重放时算出的标识一样，
// 便于外部系统按标识去重而不必依赖库里的自增列。
func auditEntryID(runID string, seq int, entryType string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s", runID, seq, entryType)))
	return "aud_" + hex.EncodeToString(sum[:])[:16]
}

// boolToInt 把布尔转成库里的小整数标志位。
func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
