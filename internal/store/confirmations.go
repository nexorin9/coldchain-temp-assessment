// 本文件落「人工确认」的持久化：run_confirmations 表。
//
// 为什么单独一张表：确认是**一次动作**，run 是**一条判定**。
// 一条判定可能被确认一次、被否决一次、被补偿重放多次——混在一张表里，
// 「这条判定最后一次是谁定的」就答不上来了。确认单独成行、一次动作一行，
// 飞检翻出来逐行看就是完整的处置过程。
//
// 三条约定：
//   - 载荷随确认一起固化：PayloadJSON 存的是主管确认那一刻定下的写回内容原文，
//     写回侧发出去的就是它，不在写回时重新算。
//   - 一次 run 一行：主键是 run_id，重复确认更新那一行而不是插出第二行。
//     状态机挡住了「确认两次」，这张表的唯一键是第二道保险。
//   - 缺项就是空串：意见、处置、载荷都可以没有，空串表示「没有」，
//     不用零值或空对象冒充。
package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// confirmationsSchemaSQL 是 run_confirmations 表的建表脚本。
//
// 全部 IF NOT EXISTS：重复执行即幂等，服务每次启动跑一遍即可。
const confirmationsSchemaSQL = `
CREATE TABLE IF NOT EXISTS run_confirmations (
    run_id            TEXT PRIMARY KEY,
    action            TEXT NOT NULL,
    confirmed_by      TEXT NOT NULL,
    confirmed_at      TEXT NOT NULL,
    disposition       TEXT NOT NULL DEFAULT '',
    disposition_label TEXT NOT NULL DEFAULT '',
    opinion           TEXT NOT NULL DEFAULT '',
    payload_json      TEXT NOT NULL DEFAULT '',
    created_at        TEXT NOT NULL
);
`

// insertConfirmationSQL 是确认记录入库语句。
//
// ON CONFLICT DO UPDATE：同一个 run 的重复确认更新那一行。
// 状态机已经挡住「确认两次」，这里是第二道保险——重放是常态
// （进程重启、接口重试），插出第二行会让「这条判定被确认了几次」对不上。
const insertConfirmationSQL = `
INSERT INTO run_confirmations (
    run_id, action, confirmed_by, confirmed_at,
    disposition, disposition_label, opinion, payload_json, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(run_id) DO UPDATE SET
    action            = excluded.action,
    confirmed_by      = excluded.confirmed_by,
    confirmed_at      = excluded.confirmed_at,
    disposition       = excluded.disposition,
    disposition_label = excluded.disposition_label,
    opinion           = excluded.opinion,
    payload_json      = excluded.payload_json,
    created_at        = excluded.created_at
`

// prepareConfirmations 建 run_confirmations 表（幂等）。
func (s *Store) prepareConfirmations(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, confirmationsSchemaSQL); err != nil {
		return errcode.Newf(errcode.StorageError, "建 run_confirmations 表失败：%v", err)
	}
	return nil
}

// SaveConfirmation 写入（或更新）一条确认记录。
func (s *Store) SaveConfirmation(ctx context.Context, rec domain.Confirmation) error {
	if strings.TrimSpace(rec.RunID) == "" {
		return errcode.Newf(errcode.Validation, "确认记录缺少 run_id，无法入库")
	}
	action := strings.TrimSpace(rec.Action)
	if action == "" {
		return errcode.Newf(errcode.Validation, "确认记录缺少 action，无法入库")
	}
	confirmedAt := rec.ConfirmedAt
	if confirmedAt.IsZero() {
		confirmedAt = time.Now()
	}
	createdAt := rec.CreatedAt
	if createdAt.IsZero() {
		createdAt = confirmedAt
	}
	if _, err := s.db.ExecContext(ctx, insertConfirmationSQL,
		rec.RunID, action, rec.ConfirmedBy,
		confirmedAt.UTC().Format(tsLayout),
		rec.Disposition, rec.DispositionLabel, rec.Opinion, rec.PayloadJSON,
		createdAt.UTC().Format(tsLayout),
	); err != nil {
		return errcode.Newf(errcode.StorageError, "写入确认记录失败：%v", err)
	}
	return nil
}

// ConfirmationByRun 回读一条 run 的确认记录。
//
// 找不到返回 E_NOT_FOUND：写回侧据此判断「这条判定还没人确认过」，
// 而不是拿一条空记录去写台账。
func (s *Store) ConfirmationByRun(ctx context.Context, runID string) (domain.Confirmation, error) {
	if strings.TrimSpace(runID) == "" {
		return domain.Confirmation{}, errcode.Newf(errcode.Validation, "回读确认记录需要 run_id")
	}
	var (
		rec         domain.Confirmation
		action      string
		confirmedAt string
		createdAt   string
	)
	err := s.db.QueryRowContext(ctx,
		"SELECT run_id, action, confirmed_by, confirmed_at, disposition, disposition_label, "+
			"opinion, payload_json, created_at FROM run_confirmations WHERE run_id = ?",
		runID).Scan(
		&rec.RunID, &action, &rec.ConfirmedBy, &confirmedAt,
		&rec.Disposition, &rec.DispositionLabel,
		&rec.Opinion, &rec.PayloadJSON, &createdAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return domain.Confirmation{}, errcode.Newf(errcode.NotFound,
				"判定 run %s 没有确认记录：未经药学部主管确认，不得写回台账", runID)
		}
		return domain.Confirmation{}, errcode.Newf(errcode.StorageError, "回读确认记录失败：%v", err)
	}
	rec.Action = action
	if parsed, perr := time.Parse(tsLayout, confirmedAt); perr == nil {
		rec.ConfirmedAt = parsed
	}
	if parsed, perr := time.Parse(tsLayout, createdAt); perr == nil {
		rec.CreatedAt = parsed
	}
	return rec, nil
}

// RunState 回读一条 run 的业务状态。
//
// 它是出站写回闸门（workflow.WritebackGate）要的读口：
// 写回之前先问一句「这条到底确认了没有」，问不到就一次出站调用都不发。
func (s *Store) RunState(ctx context.Context, runID string) (string, error) {
	if strings.TrimSpace(runID) == "" {
		return "", errcode.Newf(errcode.Validation, "回读判定 run 业务状态需要 run_id")
	}
	var state string
	err := s.db.QueryRowContext(ctx,
		"SELECT state FROM runs WHERE run_id = ?", runID).Scan(&state)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", errcode.Newf(errcode.NotFound, "判定 run 不存在：%s", runID)
		}
		return "", errcode.Newf(errcode.StorageError, "回读判定 run 业务状态失败：%v", err)
	}
	return state, nil
}
