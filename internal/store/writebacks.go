// 本文件落「出站写回」在本地留下的痕迹：run_writebacks 表。
//
// 为什么按目标分两行记（台账一行、通知一行）：一次写回要落两处，
// 两处的成败互不相干——台账写成了、通知没发出去，是很常见的一种状态。
// 记成一条「写回成功」就永远补不了通知，记成一条「写回失败」就会把
// 已经入账的台账再写一遍。分开记，补偿重放才敢只补没写成的那一处。
//
// 这张表同时是「不重复入账」的本地凭据：台账侧按 external_ref /
// idempotency_key 认得出重复入账（权威判据），本地记录让重放连请求都不发。
package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// writebacksSchemaSQL 是 run_writebacks 表的建表脚本。
//
// 全部 IF NOT EXISTS：重复执行即幂等，服务每次启动跑一遍即可。
const writebacksSchemaSQL = `
CREATE TABLE IF NOT EXISTS run_writebacks (
    run_id          TEXT NOT NULL,
    target          TEXT NOT NULL,
    state           TEXT NOT NULL DEFAULT 'pending',
    external_ref    TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT NOT NULL DEFAULT '',
    error_code      TEXT NOT NULL DEFAULT '',
    error_detail    TEXT NOT NULL DEFAULT '',
    written_at      TEXT,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    PRIMARY KEY (run_id, target)
);

CREATE INDEX IF NOT EXISTS idx_run_writebacks_state ON run_writebacks (state, updated_at);
`

// upsertWritebackSQL 是写回记录入库语句。
//
// ON CONFLICT DO UPDATE：同一处目标的多次尝试更新那一行，而不是插出第二行。
// 一次写回尝试一次、补偿重放再试几次，都只留一行——
// 「这一处写过几次」由 attempt 计数与审计链回答，不由行数回答。
const upsertWritebackSQL = `
INSERT INTO run_writebacks (
    run_id, target, state, external_ref, idempotency_key,
    error_code, error_detail, written_at, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(run_id, target) DO UPDATE SET
    state           = excluded.state,
    external_ref    = excluded.external_ref,
    idempotency_key = excluded.idempotency_key,
    error_code      = excluded.error_code,
    error_detail    = excluded.error_detail,
    written_at      = CASE WHEN excluded.written_at IS NULL
                           THEN run_writebacks.written_at
                           ELSE excluded.written_at END,
    updated_at      = excluded.updated_at
`

// prepareWritebacks 建 run_writebacks 表（幂等）。
func (s *Store) prepareWritebacks(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, writebacksSchemaSQL); err != nil {
		return errcode.Newf(errcode.StorageError, "建 run_writebacks 表失败：%v", err)
	}
	return nil
}

// writebackAlertsSchemaSQL 是 writeback_alerts 表的建表脚本。
//
// 与 run_writebacks 分开成两张表：前者记「这一处现在写没写成」，
// 这一张记「曾经写不进去、现在还没人管」。分开记，补偿重放补上之后
// 告警仍留着 open→resolved 的留痕，飞检问「那次怎么补的」有据可查。
const writebackAlertsSchemaSQL = `
CREATE TABLE IF NOT EXISTS writeback_alerts (
    run_id          TEXT NOT NULL,
    target          TEXT NOT NULL,
    state           TEXT NOT NULL DEFAULT 'open',
    error_code      TEXT NOT NULL DEFAULT '',
    error_detail    TEXT NOT NULL DEFAULT '',
    attempts        INTEGER NOT NULL DEFAULT 0,
    failures        INTEGER NOT NULL DEFAULT 0,
    first_failed_at TEXT,
    last_failed_at  TEXT,
    resolved_at     TEXT,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    PRIMARY KEY (run_id, target)
);

CREATE INDEX IF NOT EXISTS idx_writeback_alerts_state ON writeback_alerts (state, updated_at);
`

// upsertWritebackAlertSQL 是告警入库语句。
//
// 次数是**累加**的：attempts += 本轮尝试次数，failures += 1。
// 一次失败事件 = 一轮失败；一轮失败里可能发起了 3 次出站调用（首次 + 2 次重试）。
// 累加而不是覆盖，是为了让「这条越温的台账写入一共敲了多少次门」看得见。
const upsertWritebackAlertSQL = `
INSERT INTO writeback_alerts (
    run_id, target, state, error_code, error_detail, attempts, failures,
    first_failed_at, last_failed_at, resolved_at, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(run_id, target) DO UPDATE SET
    state           = excluded.state,
    error_code      = excluded.error_code,
    error_detail    = excluded.error_detail,
    attempts        = writeback_alerts.attempts + excluded.attempts,
    failures        = writeback_alerts.failures + 1,
    first_failed_at = COALESCE(writeback_alerts.first_failed_at, excluded.first_failed_at),
    last_failed_at  = excluded.last_failed_at,
    resolved_at     = excluded.resolved_at,
    updated_at      = excluded.updated_at
`

// prepareWritebackAlerts 建 writeback_alerts 表（幂等）。
func (s *Store) prepareWritebackAlerts(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, writebackAlertsSchemaSQL); err != nil {
		return errcode.Newf(errcode.StorageError, "建 writeback_alerts 表失败：%v", err)
	}
	return nil
}

// UpsertWritebackAlert 写入（或累加）一条写回告警。
//
// rec.Attempts 是**本轮**发起的出站调用次数；库里累加之后，
// 这条告警的 attempts 就是「这一处目标一共敲了多少次门」。
func (s *Store) UpsertWritebackAlert(ctx context.Context, rec domain.WritebackAlert) error {
	if strings.TrimSpace(rec.RunID) == "" {
		return errcode.Newf(errcode.Validation, "写回告警缺少 run_id，无法入库")
	}
	target := strings.TrimSpace(rec.Target)
	if target == "" {
		return errcode.Newf(errcode.Validation, "写回告警缺少 target，无法入库")
	}
	state := strings.TrimSpace(rec.State)
	if state == "" {
		state = domain.WritebackAlertStateOpen
	}
	now := time.Now().UTC()
	createdAt := rec.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}
	updatedAt := rec.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = now
	}
	attempts := rec.Attempts
	if attempts < 0 {
		attempts = 0
	}
	// 时刻一律折算成 UTC 再落库：库里的时间戳格式带 Z 后缀，
	// 直接塞本地时刻会写出「11:28 但标着 Z」这种自相矛盾的值，
	// 待补偿清单按 updated_at 排序时也会跟着乱。
	if _, err := s.db.ExecContext(ctx, upsertWritebackAlertSQL,
		rec.RunID, target, state, rec.ErrorCode, rec.ErrorDetail, attempts, 1,
		formatOptionalStamp(rec.FirstFailedAt),
		formatOptionalStamp(rec.LastFailedAt),
		formatOptionalStamp(rec.ResolvedAt),
		createdAt.UTC().Format(tsLayout), updatedAt.UTC().Format(tsLayout),
	); err != nil {
		return errcode.Newf(errcode.StorageError, "写入写回告警失败：%v", err)
	}
	return nil
}

// ResolveWritebackAlert 把一条告警标记为已解决。
//
// 找不到告警时按幂等空操作处理：告警是补偿重放的副产品，
// 解决动作找不到对应告警不代表出错。
func (s *Store) ResolveWritebackAlert(ctx context.Context, runID, target string, at time.Time) error {
	if strings.TrimSpace(runID) == "" {
		return errcode.Newf(errcode.Validation, "解决写回告警需要 run_id")
	}
	if strings.TrimSpace(target) == "" {
		return errcode.Newf(errcode.Validation, "解决写回告警需要 target")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	if _, err := s.db.ExecContext(ctx,
		"UPDATE writeback_alerts SET state = ?, resolved_at = ?, updated_at = ? "+
			"WHERE run_id = ? AND target = ?",
		domain.WritebackAlertStateResolved, at.UTC().Format(tsLayout), at.UTC().Format(tsLayout),
		runID, target,
	); err != nil {
		return errcode.Newf(errcode.StorageError, "解决写回告警失败：%v", err)
	}
	return nil
}

// WritebackAlertByTarget 回读某条 run 在某一处目标上的告警。
//
// 找不到返回 E_NOT_FOUND：调用方据此判断「这一处还没告过警」。
func (s *Store) WritebackAlertByTarget(ctx context.Context, runID, target string) (domain.WritebackAlert, error) {
	if strings.TrimSpace(runID) == "" {
		return domain.WritebackAlert{}, errcode.Newf(errcode.Validation, "回读写回告警需要 run_id")
	}
	if strings.TrimSpace(target) == "" {
		return domain.WritebackAlert{}, errcode.Newf(errcode.Validation, "回读写回告警需要 target")
	}
	row := s.db.QueryRowContext(ctx,
		"SELECT run_id, target, state, error_code, error_detail, attempts, failures, "+
			"first_failed_at, last_failed_at, resolved_at, created_at, updated_at "+
			"FROM writeback_alerts WHERE run_id = ? AND target = ?",
		runID, target)
	rec, err := scanWritebackAlert(row)
	if err != nil {
		if errcode.As(err).Code == errcode.NotFound {
			return domain.WritebackAlert{}, errcode.Newf(errcode.NotFound,
				"判定 run %s 在目标 %s 上还没有告警记录", runID, target)
		}
		return domain.WritebackAlert{}, err
	}
	return rec, nil
}

// OpenWritebackAlerts 回读全部还挂着的告警（按最近更新排序）。
func (s *Store) OpenWritebackAlerts(ctx context.Context) ([]domain.WritebackAlert, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT run_id, target, state, error_code, error_detail, attempts, failures, "+
			"first_failed_at, last_failed_at, resolved_at, created_at, updated_at "+
			"FROM writeback_alerts WHERE state = ? ORDER BY updated_at ASC, run_id ASC, target ASC",
		domain.WritebackAlertStateOpen)
	if err != nil {
		return nil, errcode.Newf(errcode.StorageError, "回读写回告警失败：%v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []domain.WritebackAlert
	for rows.Next() {
		rec, err := scanWritebackAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, errcode.Newf(errcode.StorageError, "遍历写回告警失败：%v", err)
	}
	return out, nil
}

// CountOpenWritebackAlerts 统计还挂着的告警条数。
//
// 它是运行摘要里「待写回数量」这一项指标的来源。
func (s *Store) CountOpenWritebackAlerts(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM writeback_alerts WHERE state = ?",
		domain.WritebackAlertStateOpen).Scan(&n); err != nil {
		return 0, errcode.Newf(errcode.StorageError, "统计写回告警失败：%v", err)
	}
	return n, nil
}

// PendingWritebacks 回读全部还没写成的写回记录。
//
// 「还没写成」= state 不是 written：pending（还没轮到写）与 failed（写失败待补偿）
// 都算。返回顺序按最近更新时刻升序：先失败最早的那条排在最前面。
func (s *Store) PendingWritebacks(ctx context.Context) ([]domain.WritebackRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT run_id, target, state, external_ref, idempotency_key, "+
			"error_code, error_detail, written_at, created_at, updated_at "+
			"FROM run_writebacks WHERE state <> ? "+
			"ORDER BY updated_at ASC, run_id ASC, target ASC",
		domain.WritebackTargetStateWritten)
	if err != nil {
		return nil, errcode.Newf(errcode.StorageError, "回读待写回记录失败：%v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []domain.WritebackRecord
	for rows.Next() {
		var (
			rec     domain.WritebackRecord
			state   string
			written sql.NullString
			created string
			updated string
		)
		if err := rows.Scan(
			&rec.RunID, &rec.Target, &state, &rec.ExternalRef, &rec.IdempotencyKey,
			&rec.ErrorCode, &rec.ErrorDetail, &written, &created, &updated,
		); err != nil {
			return nil, errcode.Newf(errcode.StorageError, "读取待写回记录失败：%v", err)
		}
		rec.State = state
		if written.Valid && written.String != "" {
			if parsed, perr := time.Parse(tsLayout, written.String); perr == nil {
				rec.WrittenAt = &parsed
			}
		}
		if parsed, perr := time.Parse(tsLayout, created); perr == nil {
			rec.CreatedAt = parsed
		}
		if parsed, perr := time.Parse(tsLayout, updated); perr == nil {
			rec.UpdatedAt = parsed
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, errcode.Newf(errcode.StorageError, "遍历待写回记录失败：%v", err)
	}
	return out, nil
}

// CountPendingWritebacks 统计还没写成的写回记录条数。
//
// 它是运行摘要里「待写回数量」这一项指标的来源。
func (s *Store) CountPendingWritebacks(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM run_writebacks WHERE state <> ?",
		domain.WritebackTargetStateWritten).Scan(&n); err != nil {
		return 0, errcode.Newf(errcode.StorageError, "统计待写回记录失败：%v", err)
	}
	return n, nil
}

// rowScanner 是一条可扫描的查询结果（*sql.Row 与 *sql.Rows 都满足它）。
type rowScanner interface {
	Scan(dest ...any) error
}

// scanWritebackAlert 扫描一行告警记录。
func scanWritebackAlert(row rowScanner) (domain.WritebackAlert, error) {
	var (
		rec      domain.WritebackAlert
		state    string
		first    sql.NullString
		last     sql.NullString
		resolved sql.NullString
		created  string
		updated  string
	)
	if err := row.Scan(
		&rec.RunID, &rec.Target, &state, &rec.ErrorCode, &rec.ErrorDetail,
		&rec.Attempts, &rec.Failures, &first, &last, &resolved, &created, &updated,
	); err != nil {
		if err == sql.ErrNoRows {
			return domain.WritebackAlert{}, errcode.Newf(errcode.NotFound, "写回告警不存在")
		}
		return domain.WritebackAlert{}, errcode.Newf(errcode.StorageError, "读取写回告警失败：%v", err)
	}
	rec.State = state
	rec.FirstFailedAt = parseOptionalStamp(first)
	rec.LastFailedAt = parseOptionalStamp(last)
	rec.ResolvedAt = parseOptionalStamp(resolved)
	if parsed, perr := time.Parse(tsLayout, created); perr == nil {
		rec.CreatedAt = parsed
	}
	if parsed, perr := time.Parse(tsLayout, updated); perr == nil {
		rec.UpdatedAt = parsed
	}
	return rec, nil
}

// formatOptionalStamp 把可空时刻格式化成库里的时间字符串。
func formatOptionalStamp(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC().Format(tsLayout)
}

// parseOptionalStamp 把库里的可空时间字符串还原成时刻。
func parseOptionalStamp(v sql.NullString) *time.Time {
	if !v.Valid || v.String == "" {
		return nil
	}
	parsed, err := time.Parse(tsLayout, v.String)
	if err != nil {
		return nil
	}
	return &parsed
}

// UpsertWriteback 写入（或更新）一条写回记录。
func (s *Store) UpsertWriteback(ctx context.Context, rec domain.WritebackRecord) error {
	if strings.TrimSpace(rec.RunID) == "" {
		return errcode.Newf(errcode.Validation, "写回记录缺少 run_id，无法入库")
	}
	target := strings.TrimSpace(rec.Target)
	if target == "" {
		return errcode.Newf(errcode.Validation, "写回记录缺少 target，无法入库")
	}
	state := strings.TrimSpace(rec.State)
	if state == "" {
		state = domain.WritebackTargetStatePending
	}
	now := time.Now().UTC()
	createdAt := rec.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}
	updatedAt := rec.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = now
	}
	var writtenAt any
	if rec.WrittenAt != nil && !rec.WrittenAt.IsZero() {
		writtenAt = rec.WrittenAt.UTC().Format(tsLayout)
	}
	if _, err := s.db.ExecContext(ctx, upsertWritebackSQL,
		rec.RunID, target, state, rec.ExternalRef, rec.IdempotencyKey,
		rec.ErrorCode, rec.ErrorDetail, writtenAt,
		createdAt.UTC().Format(tsLayout), updatedAt.UTC().Format(tsLayout),
	); err != nil {
		return errcode.Newf(errcode.StorageError, "写入写回记录失败：%v", err)
	}
	return nil
}

// WritebackByTarget 回读某条 run 在某一处目标上的写回记录。
//
// 找不到返回 E_NOT_FOUND：写回侧据此判断「这一处还没写过」，
// 而不是拿一条空记录当成「已经写过了」。
func (s *Store) WritebackByTarget(ctx context.Context, runID, target string) (domain.WritebackRecord, error) {
	if strings.TrimSpace(runID) == "" {
		return domain.WritebackRecord{}, errcode.Newf(errcode.Validation, "回读写回记录需要 run_id")
	}
	if strings.TrimSpace(target) == "" {
		return domain.WritebackRecord{}, errcode.Newf(errcode.Validation, "回读写回记录需要 target")
	}
	var (
		rec     domain.WritebackRecord
		state   string
		written sql.NullString
		created string
		updated string
	)
	err := s.db.QueryRowContext(ctx,
		"SELECT run_id, target, state, external_ref, idempotency_key, "+
			"error_code, error_detail, written_at, created_at, updated_at "+
			"FROM run_writebacks WHERE run_id = ? AND target = ?",
		runID, target).Scan(
		&rec.RunID, &rec.Target, &state, &rec.ExternalRef, &rec.IdempotencyKey,
		&rec.ErrorCode, &rec.ErrorDetail, &written, &created, &updated,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return domain.WritebackRecord{}, errcode.Newf(errcode.NotFound,
				"判定 run %s 在目标 %s 上还没有写回记录", runID, target)
		}
		return domain.WritebackRecord{}, errcode.Newf(errcode.StorageError, "回读写回记录失败：%v", err)
	}
	rec.State = state
	if written.Valid && written.String != "" {
		if parsed, perr := time.Parse(tsLayout, written.String); perr == nil {
			rec.WrittenAt = &parsed
		}
	}
	if parsed, perr := time.Parse(tsLayout, created); perr == nil {
		rec.CreatedAt = parsed
	}
	if parsed, perr := time.Parse(tsLayout, updated); perr == nil {
		rec.UpdatedAt = parsed
	}
	return rec, nil
}

// WritebacksByRun 回读一条 run 在所有目标上的写回记录。
//
// 补偿重放与查询端点用它还原「这一条判定写到哪儿了」。
func (s *Store) WritebacksByRun(ctx context.Context, runID string) ([]domain.WritebackRecord, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, errcode.Newf(errcode.Validation, "回读写回记录需要 run_id")
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT run_id, target, state, external_ref, idempotency_key, "+
			"error_code, error_detail, written_at, created_at, updated_at "+
			"FROM run_writebacks WHERE run_id = ? ORDER BY target ASC",
		runID)
	if err != nil {
		return nil, errcode.Newf(errcode.StorageError, "回读写回记录失败：%v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []domain.WritebackRecord
	for rows.Next() {
		var (
			rec     domain.WritebackRecord
			state   string
			written sql.NullString
			created string
			updated string
		)
		if err := rows.Scan(
			&rec.RunID, &rec.Target, &state, &rec.ExternalRef, &rec.IdempotencyKey,
			&rec.ErrorCode, &rec.ErrorDetail, &written, &created, &updated,
		); err != nil {
			return nil, errcode.Newf(errcode.StorageError, "读取写回记录失败：%v", err)
		}
		rec.State = state
		if written.Valid && written.String != "" {
			if parsed, perr := time.Parse(tsLayout, written.String); perr == nil {
				rec.WrittenAt = &parsed
			}
		}
		if parsed, perr := time.Parse(tsLayout, created); perr == nil {
			rec.CreatedAt = parsed
		}
		if parsed, perr := time.Parse(tsLayout, updated); perr == nil {
			rec.UpdatedAt = parsed
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, errcode.Newf(errcode.StorageError, "遍历写回记录失败：%v", err)
	}
	return out, nil
}
