// 本文件落判定步骤产物的持久化：run_steps 表。
//
// 为什么要单独一张表：判定结论的价值一半在结论本身，一半在「这个结论怎么来的」。
// 窗口结论、归因结论、成稿草稿都是产物；产物挂在步骤上，步骤记着起止时刻、
// 尝试次数、失败原因与错误码。有了这张表，飞检时翻一条 run 就能把
// 「取了哪些读数 → 抽出的窗口 → 归因结论 → 成稿草稿」整条链复盘出来，
// 而不是只剩一个结论数字。
//
// 三条约定：
//   - 一行一步：主键 (run_id, step_name, attempt)。同一步重试时 attempt 递增，
//     换一轮判定时 run_id 不同，两种「再来一次」互不覆盖。
//   - 产物按分码存：payload_kind 与 payload 成对落库，取回时按分码还原成
//     对应类型；跨分码取回被拒绝，不做「尽力解析」。
//   - 失败留痕：失败步骤照样落一行，带 error 与 error_code。
//     「这步没跑成」和「这步跑成了但结论是空」是两回事，不能只记后者。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// runStepsSchemaSQL 是 run_steps 表的建表脚本。
//
// 全部 IF NOT EXISTS：重复执行即幂等，服务每次启动跑一遍即可。
// 与 events 表同一套约定——建表脚本负责新库，存量库靠 prepareRunSteps 补齐。
const runStepsSchemaSQL = `
CREATE TABLE IF NOT EXISTS run_steps (
    run_id       TEXT NOT NULL,
    step_name    TEXT NOT NULL,
    attempt      INTEGER NOT NULL DEFAULT 1,
    seq          INTEGER NOT NULL DEFAULT 0,
    state        TEXT NOT NULL,
    payload_kind TEXT NOT NULL DEFAULT '',
    payload      TEXT NOT NULL DEFAULT '',
    started_at   TEXT NOT NULL,
    ended_at     TEXT,
    error        TEXT NOT NULL DEFAULT '',
    error_code   TEXT NOT NULL DEFAULT '',
    backoff_ms   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (run_id, step_name, attempt)
);

CREATE INDEX IF NOT EXISTS idx_run_steps_run ON run_steps (run_id, seq, attempt);
`

// runStepsBackoffColumnDDL 是给存量 run_steps 表补列的语句。
//
// 补列而不是改表：老库里已经跑过的判定不该因为加一列退避时长就作废。
const runStepsBackoffColumnDDL = "ALTER TABLE run_steps ADD COLUMN backoff_ms INTEGER NOT NULL DEFAULT 0"

// insertRunStepSQL 是步骤落库语句。
//
// ON CONFLICT DO UPDATE：同一步骤在同一轮里被重放时更新那一行，
// 而不是插出第二行。重放是常态（进程重启后继续跑同一步），
// 插出第二行会让时间线上出现两条一模一样的记录，复核时看不出哪条是真的。
const insertRunStepSQL = `
INSERT INTO run_steps (
    run_id, step_name, attempt, seq, state,
    payload_kind, payload, started_at, ended_at, error, error_code, backoff_ms
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(run_id, step_name, attempt) DO UPDATE SET
    seq          = excluded.seq,
    state        = excluded.state,
    payload_kind = excluded.payload_kind,
    payload      = excluded.payload,
    started_at   = excluded.started_at,
    ended_at     = excluded.ended_at,
    error        = excluded.error,
    error_code   = excluded.error_code,
    backoff_ms   = excluded.backoff_ms
`

// prepareRunSteps 建 run_steps 表（幂等）。
func (s *Store) prepareRunSteps(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, runStepsSchemaSQL); err != nil {
		return errcode.Newf(errcode.StorageError, "建 run_steps 表失败：%v", err)
	}
	// 存量库补列：退避时长是后来才加的，老表没有这一列。
	if err := s.ensureColumn(ctx, "run_steps", "backoff_ms", runStepsBackoffColumnDDL); err != nil {
		return err
	}
	return nil
}

// InsertRunStep 写入（或更新）一条步骤记录。
//
// 产物以信封 JSON 原样落库：分码在信封里，也在 payload_kind 列上，
// 两处都要在——列上那一份是为了按分码检索，信封里那一份是为了取回时核对形状。
func (s *Store) InsertRunStep(ctx context.Context, step domain.Step) error {
	if strings.TrimSpace(step.RunID) == "" {
		return errcode.Newf(errcode.Validation, "步骤记录缺少 run_id，无法入库")
	}
	if strings.TrimSpace(step.StepName) == "" {
		return errcode.Newf(errcode.Validation, "步骤记录缺少 step_name，无法入库")
	}
	attempt := step.Attempt
	if attempt <= 0 {
		attempt = 1
	}

	var endedAt any
	if step.EndedAt != nil {
		endedAt = step.EndedAt.UTC().Format(tsLayout)
	}

	if _, err := s.db.ExecContext(ctx, insertRunStepSQL,
		step.RunID, step.StepName, attempt, step.Seq, step.State,
		string(step.PayloadKind), string(step.Payload),
		step.StartedAt.UTC().Format(tsLayout), endedAt,
		step.Error, step.ErrorCode, step.BackoffMS,
	); err != nil {
		return errcode.Newf(errcode.StorageError, "写入步骤记录失败：%v", err)
	}
	return nil
}

// RunStepsByRun 回读一条 run 的全部步骤记录。
//
// 按链上次序（seq）升序、同一步按尝试次数升序返回：
// 时间线的先后就是判定实际发生的先后，复盘时不能靠调用方自己排。
func (s *Store) RunStepsByRun(ctx context.Context, runID string) ([]domain.Step, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, errcode.Newf(errcode.Validation, "回读步骤记录需要 run_id")
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT run_id, step_name, attempt, seq, state, payload_kind, payload, "+
			"started_at, ended_at, error, error_code, backoff_ms "+
			"FROM run_steps WHERE run_id = ? ORDER BY seq ASC, attempt ASC, step_name ASC",
		runID)
	if err != nil {
		return nil, errcode.Newf(errcode.StorageError, "回读步骤记录失败：%v", err)
	}
	defer func() { _ = rows.Close() }()

	steps := make([]domain.Step, 0, 8)
	for rows.Next() {
		step, err := scanStep(rows)
		if err != nil {
			return nil, err
		}
		steps = append(steps, step)
	}
	if err := rows.Err(); err != nil {
		return nil, errcode.Newf(errcode.StorageError, "回读步骤记录失败：%v", err)
	}
	return steps, nil
}

// StepPayloadByRunKind 回读一条 run 上某个分码的成功产物。
//
// 只认成功步骤的产物：失败步骤没有产物（半截产物不算结论）。
// 同一分码有多条成功记录时取链上次序最靠前的那条——重试成功的那条 attempt
// 更大、seq 相同，按 seq/attempt 排序后第一条就是这次判定真正用的那份产物。
//
// 离线回放用它取回「当初在线判定的窗口产物」，作为逐字段比对的基准。
func (s *Store) StepPayloadByRunKind(ctx context.Context, runID string, kind domain.StepPayloadKind) (any, bool, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, false, errcode.Newf(errcode.Validation, "回读步骤产物需要 run_id")
	}
	if strings.TrimSpace(string(kind)) == "" {
		return nil, false, errcode.Newf(errcode.Validation, "回读步骤产物需要产物分码")
	}
	var payload string
	err := s.db.QueryRowContext(ctx,
		"SELECT payload FROM run_steps WHERE run_id = ? AND payload_kind = ? AND state = ? "+
			"ORDER BY seq ASC, attempt ASC LIMIT 1",
		runID, string(kind), domain.StepStateSucceeded).Scan(&payload)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, false, nil
		}
		return nil, false, errcode.Newf(errcode.StorageError, "回读步骤产物失败：%v", err)
	}
	if strings.TrimSpace(payload) == "" {
		return nil, false, nil
	}
	value, err := domain.DecodePayload(kind, json.RawMessage(payload))
	if err != nil {
		return nil, false, err
	}
	return value, true, nil
}

// CountRunSteps 统计一条 run 的步骤记录条数。
//
// 这是「判定跑完了几步」的最快核对口径：时间线端点与周报都用它，
// 不必把整条记录拉出来数。
func (s *Store) CountRunSteps(ctx context.Context, runID string) (int, error) {
	if strings.TrimSpace(runID) == "" {
		return 0, errcode.Newf(errcode.Validation, "统计步骤记录需要 run_id")
	}
	var total int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM run_steps WHERE run_id = ?", runID).Scan(&total); err != nil {
		return 0, errcode.Newf(errcode.StorageError, "统计步骤记录失败：%v", err)
	}
	return total, nil
}

// scanStep 把一行步骤记录还原成领域步骤。
func scanStep(rows *sql.Rows) (domain.Step, error) {
	var (
		step      domain.Step
		startedAt string
		endedAt   sql.NullString
		payload   string
	)
	if err := rows.Scan(
		&step.RunID, &step.StepName, &step.Attempt, &step.Seq, &step.State,
		&step.PayloadKind, &payload,
		&startedAt, &endedAt, &step.Error, &step.ErrorCode, &step.BackoffMS,
	); err != nil {
		return domain.Step{}, errcode.Newf(errcode.StorageError, "读取步骤记录行失败：%v", err)
	}

	parsed, err := time.Parse(tsLayout, startedAt)
	if err != nil {
		return domain.Step{}, errcode.Newf(errcode.StorageError, "还原步骤 started_at 失败：%v", err)
	}
	step.StartedAt = parsed
	if endedAt.Valid && strings.TrimSpace(endedAt.String) != "" {
		parsedEnd, err := time.Parse(tsLayout, endedAt.String)
		if err != nil {
			return domain.Step{}, errcode.Newf(errcode.StorageError, "还原步骤 ended_at 失败：%v", err)
		}
		step.EndedAt = &parsedEnd
	}
	if strings.TrimSpace(payload) != "" {
		step.Payload = json.RawMessage(payload)
	}
	return step, nil
}
