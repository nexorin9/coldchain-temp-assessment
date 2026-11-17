package store

// 本文件落「报表取数」：周报要把一段时间里的判定汇总成一行一个点位，
// 需要的不只是 run 本身，还要每条 run 的窗口产物、成稿产物与确认记录。
//
// 三条口径：
//   - 统计范围按越限窗口的起点算：window_start 落在区间内的判定才计入。
//     按「窗口与区间相交」算会把跨周的长段越温算进两周，各周加起来不等于总数。
//   - 同一步骤被重放过多次时取最后一次成功的产物：重放是常态，
//     取到哪一次决定报表上的峰值与处置结论对不对。
//   - 缺什么就是缺什么：成稿没查到批次就是没查到，不用空数组冒充「没有批次」。

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// selectRunPayloadsInRangeSQL 取区间内所有 run 的指定分码产物。
//
// JOIN runs 是为了按窗口起点筛区间；ORDER BY attempt ASC 让重放多次时
// 后写的那次覆盖前一次（取最后一次）。
const selectRunPayloadsInRangeSQL = `
SELECT rs.run_id, rs.payload
FROM run_steps rs
JOIN runs r ON r.run_id = rs.run_id
WHERE rs.payload_kind = ?
  AND rs.state = ?
  AND r.window_start IS NOT NULL AND r.window_start <> ''
  AND r.window_start >= ? AND r.window_start <= ?
ORDER BY rs.attempt ASC
`

// selectConfirmationsInRangeSQL 取区间内所有 run 的确认记录。
const selectConfirmationsInRangeSQL = `
SELECT c.run_id, c.action, c.confirmed_by, c.confirmed_at,
       c.disposition, c.disposition_label, c.opinion, c.payload_json, c.created_at
FROM run_confirmations c
JOIN runs r ON r.run_id = c.run_id
WHERE r.window_start IS NOT NULL AND r.window_start <> ''
  AND r.window_start >= ? AND r.window_start <= ?
`

// stepStateSucceeded 是「这一步成功」的步骤状态取值。
const stepStateSucceeded = domain.StepStateSucceeded

// RunPayloadsInRange 回读区间内所有 run 的指定分码产物原文。
//
// 返回 run_id → 产物信封 JSON。同一个 run 的同一步骤被重放过多次时，
// 取最后一次成功的那次。
func (s *Store) RunPayloadsInRange(ctx context.Context, kind domain.StepPayloadKind, from, to time.Time) (map[string]json.RawMessage, error) {
	if kind == "" {
		return nil, errcode.Newf(errcode.Validation, "回读步骤产物需要指定分码")
	}
	if from.IsZero() || to.IsZero() {
		return nil, errcode.Newf(errcode.Validation, "回读步骤产物需要明确的起止区间")
	}
	rows, err := s.db.QueryContext(ctx, selectRunPayloadsInRangeSQL,
		string(kind), stepStateSucceeded,
		from.UTC().Format(tsLayout), to.UTC().Format(tsLayout))
	if err != nil {
		return nil, errcode.Newf(errcode.StorageError, "回读步骤产物失败：%v", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]json.RawMessage)
	for rows.Next() {
		var (
			runID   string
			payload string
		)
		if err := rows.Scan(&runID, &payload); err != nil {
			return nil, errcode.Newf(errcode.StorageError, "读取步骤产物失败：%v", err)
		}
		if strings.TrimSpace(payload) == "" {
			continue
		}
		out[runID] = json.RawMessage(payload)
	}
	if err := rows.Err(); err != nil {
		return nil, errcode.Newf(errcode.StorageError, "回读步骤产物失败：%v", err)
	}
	return out, nil
}

// ConfirmationsInRange 回读区间内所有 run 的确认记录。
//
// 返回 run_id → 确认记录。同一 run 反复确认时库里只有一行（主键是 run_id），
// 取回的就是最后一次那次的结论。
func (s *Store) ConfirmationsInRange(ctx context.Context, from, to time.Time) (map[string]domain.Confirmation, error) {
	if from.IsZero() || to.IsZero() {
		return nil, errcode.Newf(errcode.Validation, "回读确认记录需要明确的起止区间")
	}
	rows, err := s.db.QueryContext(ctx, selectConfirmationsInRangeSQL,
		from.UTC().Format(tsLayout), to.UTC().Format(tsLayout))
	if err != nil {
		return nil, errcode.Newf(errcode.StorageError, "回读确认记录失败：%v", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]domain.Confirmation)
	for rows.Next() {
		var (
			rec         domain.Confirmation
			confirmedAt string
			createdAt   string
		)
		if err := rows.Scan(
			&rec.RunID, &rec.Action, &rec.ConfirmedBy, &confirmedAt,
			&rec.Disposition, &rec.DispositionLabel, &rec.Opinion, &rec.PayloadJSON, &createdAt,
		); err != nil {
			return nil, errcode.Newf(errcode.StorageError, "读取确认记录失败：%v", err)
		}
		if parsed, err := time.Parse(tsLayout, confirmedAt); err == nil {
			rec.ConfirmedAt = parsed
		}
		if parsed, err := time.Parse(tsLayout, createdAt); err == nil {
			rec.CreatedAt = parsed
		}
		out[rec.RunID] = rec
	}
	if err := rows.Err(); err != nil {
		return nil, errcode.Newf(errcode.StorageError, "回读确认记录失败：%v", err)
	}
	return out, nil
}
