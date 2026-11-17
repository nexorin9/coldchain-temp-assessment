// 本文件落「模型输出事实校验」的记录：run_fact_checks 表。
//
// 为什么单独一张表：模型说过的每一个被拦下的数字，都是一条审计线索。
// 「这次归因结论里的峰值温度 13.7℃ 是不是现场读数」这件事，
// 事后没人能靠记忆回答——但只要库里留着那一行（哪个字段、原值多少、
// 依据是什么、第几次调用被拦的），就能查。
//
// 这张表与 run_steps 分开：run_steps 记的是「判定链走了哪几步」，
// run_fact_checks 记的是「模型被拦过几次」。两张表按 run_id 关联，
// 互不覆盖——一次判定可能一步没拦、也可能在归因那步拦两次。
//
// 三条约定：
//   - 一行一次尝试：主键 (run_id, step_name, schema_name, attempt)。
//     同一轮判定里同一步重问一次就是两行，先后次序看得见。
//   - 问题清单原样落库：被拦下的字段、原值与依据都进 issues 列，
//     不揉成一句话——揉了就看不出「拦的到底是哪个字段」。
//   - 原文片段只留截断版：够复核「模型到底说了什么」，又不至于把库撑大。
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

// factChecksSchemaSQL 是 run_fact_checks 表的建表脚本。
//
// 全部 IF NOT EXISTS：重复执行即幂等，服务每次启动跑一遍即可。
const factChecksSchemaSQL = `
CREATE TABLE IF NOT EXISTS run_fact_checks (
    run_id       TEXT NOT NULL,
    step_name    TEXT NOT NULL,
    schema_name  TEXT NOT NULL,
    attempt      INTEGER NOT NULL DEFAULT 1,
    verdict      TEXT NOT NULL,
    issues       TEXT NOT NULL DEFAULT '[]',
    raw_fragment TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    PRIMARY KEY (run_id, step_name, schema_name, attempt)
);

CREATE INDEX IF NOT EXISTS idx_run_fact_checks_run ON run_fact_checks (run_id, step_name, attempt);
`

// insertFactCheckSQL 是校验记录入库语句。
//
// ON CONFLICT DO UPDATE：同一轮同一步的同一次尝试被重放时更新那一行，
// 而不是插出第二行——重放是常态（进程重启后续跑），插两行会让
// 「问了几次」这件事对不上。
const insertFactCheckSQL = `
INSERT INTO run_fact_checks (
    run_id, step_name, schema_name, attempt,
    verdict, issues, raw_fragment, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(run_id, step_name, schema_name, attempt) DO UPDATE SET
    verdict      = excluded.verdict,
    issues       = excluded.issues,
    raw_fragment = excluded.raw_fragment,
    created_at   = excluded.created_at
`

// prepareFactChecks 建 run_fact_checks 表（幂等）。
func (s *Store) prepareFactChecks(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, factChecksSchemaSQL); err != nil {
		return errcode.Newf(errcode.StorageError, "建 run_fact_checks 表失败：%v", err)
	}
	return nil
}

// RecordFactCheck 写入（或更新）一条事实校验记录。
//
// 它满足模型层的 FactRecorder 接口：模型层不碰数据库，
// 只把「这次校验的结论」交出来，落库由存储层做。
func (s *Store) RecordFactCheck(ctx context.Context, record domain.FactCheckRecord) error {
	if strings.TrimSpace(record.RunID) == "" {
		return errcode.Newf(errcode.Validation, "事实校验记录缺少 run_id，无法入库")
	}
	if strings.TrimSpace(record.StepName) == "" {
		return errcode.Newf(errcode.Validation, "事实校验记录缺少 step_name，无法入库")
	}
	if strings.TrimSpace(record.SchemaName) == "" {
		return errcode.Newf(errcode.Validation, "事实校验记录缺少 schema_name，无法入库")
	}
	attempt := record.Attempt
	if attempt <= 0 {
		attempt = 1
	}
	issues := record.Issues
	if issues == nil {
		issues = []domain.FactIssue{}
	}
	issuesJSON, err := json.Marshal(issues)
	if err != nil {
		return errcode.Newf(errcode.StorageError, "序列化事实校验问题失败：%v", err)
	}
	createdAt := record.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	if _, err := s.db.ExecContext(ctx, insertFactCheckSQL,
		record.RunID, record.StepName, record.SchemaName, attempt,
		record.Verdict, string(issuesJSON), record.RawFragment,
		createdAt.UTC().Format(tsLayout),
	); err != nil {
		return errcode.Newf(errcode.StorageError, "写入事实校验记录失败：%v", err)
	}
	return nil
}

// FactChecksByRun 回读一条 run 的全部事实校验记录。
//
// 按步骤名、尝试序号升序返回：校验的先后就是发生的先后，复盘时不能靠调用方自己排。
func (s *Store) FactChecksByRun(ctx context.Context, runID string) ([]domain.FactCheckRecord, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, errcode.Newf(errcode.Validation, "回读事实校验记录需要 run_id")
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT run_id, step_name, schema_name, attempt, verdict, issues, raw_fragment, created_at "+
			"FROM run_fact_checks WHERE run_id = ? ORDER BY step_name ASC, attempt ASC",
		runID)
	if err != nil {
		return nil, errcode.Newf(errcode.StorageError, "回读事实校验记录失败：%v", err)
	}
	defer func() { _ = rows.Close() }()

	records := make([]domain.FactCheckRecord, 0, 4)
	for rows.Next() {
		record, err := scanFactCheck(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, errcode.Newf(errcode.StorageError, "回读事实校验记录失败：%v", err)
	}
	return records, nil
}

// CountFactChecks 统计一条 run 的事实校验记录条数。
//
// 这是「模型被问过几次、有没有被拦过」的最快核对口径。
func (s *Store) CountFactChecks(ctx context.Context, runID string) (int, error) {
	if strings.TrimSpace(runID) == "" {
		return 0, errcode.Newf(errcode.Validation, "统计事实校验记录需要 run_id")
	}
	var total int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM run_fact_checks WHERE run_id = ?", runID).Scan(&total); err != nil {
		return 0, errcode.Newf(errcode.StorageError, "统计事实校验记录失败：%v", err)
	}
	return total, nil
}

// scanFactCheck 把一行校验记录还原成领域记录。
func scanFactCheck(rows *sql.Rows) (domain.FactCheckRecord, error) {
	var (
		record    domain.FactCheckRecord
		issues    string
		createdAt string
	)
	if err := rows.Scan(
		&record.RunID, &record.StepName, &record.SchemaName, &record.Attempt,
		&record.Verdict, &issues, &record.RawFragment, &createdAt,
	); err != nil {
		return domain.FactCheckRecord{}, errcode.Newf(errcode.StorageError, "读取事实校验记录行失败：%v", err)
	}
	if strings.TrimSpace(issues) != "" {
		var list []domain.FactIssue
		if err := json.Unmarshal([]byte(issues), &list); err != nil {
			return domain.FactCheckRecord{}, errcode.Newf(errcode.StorageError,
				"还原事实校验问题失败：%v", err)
		}
		record.Issues = list
	}
	parsed, err := time.Parse(tsLayout, createdAt)
	if err != nil {
		return domain.FactCheckRecord{}, errcode.Newf(errcode.StorageError,
			"还原事实校验记录 created_at 失败：%v", err)
	}
	record.CreatedAt = parsed
	return record, nil
}
