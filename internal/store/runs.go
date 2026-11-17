// 本文件落判定 run 的持久化：runs 表与「进程重启后接着跑」所需的查询。
//
// 为什么必须落库：判定服务是要常驻在院内小主机上的。断电、发布、内核升级、
// 巡检重启都会让进程在判定跑到一半时消失。如果 run 的状态只在内存里，
// 这条判定就凭空消失了——现场看到的是「温控系统说没报警」，而库里的温度记录
// 明明白白越限过一次。药学部要的是「那次到底判了没有、判到哪一步、谁确认的」。
//
// 四条约定：
//   - 随步落库：run 的状态在每一步推进后实时写盘，不等整条链跑完。
//     进程被杀掉时，库里留下的是「它走到了哪一步」，而不是一个空壳。
//   - 续跑认产物，不认记忆：重启后从 run_steps 表里已成功的步骤产物判断
//     从哪一步继续，已完成的步骤不重跑——重跑会把已经算对的窗口再算一遍，
//     而重算用的温区可能已经不是当初那一份了。
//   - 终态不续跑：执行态为 succeeded / failed 的 run 是终态，
//     不在续跑扫描的结果里。
//   - 写回只写一次：writeback_state 记着台账写入是否落成，
//     「已写回」的 run 不会被重新写一次台账。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// runsSchemaSQL 是 runs 表的建表脚本。
//
// 全部 IF NOT EXISTS：重复执行即幂等，服务每次启动跑一遍即可。
const runsSchemaSQL = `
CREATE TABLE IF NOT EXISTS runs (
    run_id          TEXT PRIMARY KEY,
    point_id        TEXT NOT NULL,
    window_start    TEXT,
    window_end      TEXT,
    state           TEXT NOT NULL DEFAULT 'detected',
    exec_state      TEXT NOT NULL DEFAULT 'queued',
    external_ref    TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT NOT NULL DEFAULT '',
    failed_step     TEXT NOT NULL DEFAULT '',
    error_code      TEXT NOT NULL DEFAULT '',
    profile_json    TEXT NOT NULL DEFAULT '',
    writeback_state TEXT NOT NULL DEFAULT '',
    written_back_at TEXT,
    degraded        INTEGER NOT NULL DEFAULT 0,
    degrade_reason  TEXT NOT NULL DEFAULT '',
    state_trail_json TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_runs_exec_state ON runs (exec_state, updated_at);
CREATE INDEX IF NOT EXISTS idx_runs_idempotency ON runs (idempotency_key);
`

// runsColumnDDL 是给存量 runs 表补列用的语句表。
//
// 建表脚本对已存在的表不会重跑，后来新增的列要单独补。
// 逐列幂等补齐，保证新老库结构一致——新老库行为分叉通常在上线后才被发现。
var runsColumnDDL = []struct {
	column string
	ddl    string
}{
	{"failed_step", "ALTER TABLE runs ADD COLUMN failed_step TEXT NOT NULL DEFAULT ''"},
	{"error_code", "ALTER TABLE runs ADD COLUMN error_code TEXT NOT NULL DEFAULT ''"},
	{"profile_json", "ALTER TABLE runs ADD COLUMN profile_json TEXT NOT NULL DEFAULT ''"},
	{"writeback_state", "ALTER TABLE runs ADD COLUMN writeback_state TEXT NOT NULL DEFAULT ''"},
	{"written_back_at", "ALTER TABLE runs ADD COLUMN written_back_at TEXT"},
	{"degraded", "ALTER TABLE runs ADD COLUMN degraded INTEGER NOT NULL DEFAULT 0"},
	{"degrade_reason", "ALTER TABLE runs ADD COLUMN degrade_reason TEXT NOT NULL DEFAULT ''"},
	{"state_trail_json", "ALTER TABLE runs ADD COLUMN state_trail_json TEXT NOT NULL DEFAULT ''"},
}

// insertRunSQL 是 run 状态落库语句。
//
// ON CONFLICT DO UPDATE：同一条 run 被反复推进时更新那一行，而不是插出第二行。
// created_at 不在更新列里——它是这条 run 第一次落库的时刻，
// 后续每一步推进都覆盖它的话，「这条判定是什么时候开始的」就查不出来了。
//
// 写回状态与写回时刻用 CASE 保护：执行器每次推进都写整条快照，
// 快照里没有写回状态（空串），若无条件覆盖，「已写回」这个标记
// 就会被悄悄抹掉——重启后系统会以为这条还没写过台账，再写一次。
// 这里的语义是：空值表示「没有意见，保留库里原有的」。
const insertRunSQL = `
INSERT INTO runs (
    run_id, point_id, window_start, window_end, state, exec_state,
    external_ref, idempotency_key, failed_step, error_code,
    profile_json, writeback_state, written_back_at,
    degraded, degrade_reason, state_trail_json, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(run_id) DO UPDATE SET
    point_id        = excluded.point_id,
    window_start    = excluded.window_start,
    window_end      = excluded.window_end,
    state           = excluded.state,
    exec_state      = excluded.exec_state,
    external_ref    = excluded.external_ref,
    idempotency_key = excluded.idempotency_key,
    failed_step     = excluded.failed_step,
    error_code      = excluded.error_code,
    profile_json    = excluded.profile_json,
    writeback_state = CASE WHEN excluded.writeback_state = ''
                           THEN runs.writeback_state
                           ELSE excluded.writeback_state END,
    written_back_at = CASE WHEN excluded.written_back_at IS NULL
                           THEN runs.written_back_at
                           ELSE excluded.written_back_at END,
    degraded        = excluded.degraded,
    degrade_reason  = excluded.degrade_reason,
    state_trail_json = excluded.state_trail_json,
    updated_at      = excluded.updated_at
`

// selectRunsSQL 是回读 run 的语句骨架。
const selectRunsSQL = `
SELECT run_id, point_id, window_start, window_end, state, exec_state,
       external_ref, idempotency_key, failed_step, error_code,
       profile_json, writeback_state, written_back_at,
       degraded, degrade_reason, state_trail_json, created_at, updated_at
FROM runs
`

// prepareRuns 建 runs 表并给存量库补列（幂等）。
func (s *Store) prepareRuns(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, runsSchemaSQL); err != nil {
		return errcode.Newf(errcode.StorageError, "建 runs 表失败：%v", err)
	}
	for _, item := range runsColumnDDL {
		if err := s.ensureColumn(ctx, "runs", item.column, item.ddl); err != nil {
			return err
		}
	}
	return nil
}

// UpsertRun 写入（或更新）一条判定 run 的状态快照。
//
// 写的是整条快照而不是增量补丁：库里那一份永远是这条 run 的当前全貌，
// 不存在「更新了失败步骤却忘了更新执行态」这种半截状态。
func (s *Store) UpsertRun(ctx context.Context, rec domain.RunRecord) error {
	if strings.TrimSpace(rec.Run.RunID) == "" {
		return errcode.Newf(errcode.Validation, "run 记录缺少 run_id，无法入库")
	}
	if strings.TrimSpace(rec.Run.PointID) == "" {
		return errcode.Newf(errcode.Validation, "run %s 缺少 point_id，无法入库", rec.Run.RunID)
	}

	profileJSON := ""
	if rec.Profile != nil {
		encoded, err := json.Marshal(rec.Profile)
		if err != nil {
			return errcode.Newf(errcode.StorageError, "序列化点位温区配置失败：%v", err)
		}
		profileJSON = string(encoded)
	}

	// 业务状态迁移留痕：整条链序列化进 runs 表的一个 JSON 列。
	// 落库而不是只留在内存里，是因为「这条判定是怎么一步步走到今天这个状态的」
	// 必须经得起进程重启与飞检回看——只存在内存里的留痕等于没有。
	trailJSON := ""
	if len(rec.Run.StateTrail) > 0 {
		encoded, err := json.Marshal(rec.Run.StateTrail)
		if err != nil {
			return errcode.Newf(errcode.StorageError, "序列化业务状态迁移留痕失败：%v", err)
		}
		trailJSON = string(encoded)
	}

	var windowStart, windowEnd any
	if rec.Run.WindowStart != nil {
		windowStart = rec.Run.WindowStart.UTC().Format(tsLayout)
	}
	if rec.Run.WindowEnd != nil {
		windowEnd = rec.Run.WindowEnd.UTC().Format(tsLayout)
	}
	var writtenBackAt any
	if rec.WrittenBackAt != nil {
		writtenBackAt = rec.WrittenBackAt.UTC().Format(tsLayout)
	}

	now := rec.UpdatedAt
	if now.IsZero() {
		now = time.Now()
	}
	createdAt := rec.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}

	if _, err := s.db.ExecContext(ctx, insertRunSQL,
		rec.Run.RunID, rec.Run.PointID, windowStart, windowEnd,
		rec.Run.State, string(rec.Run.ExecState),
		rec.Run.ExternalRef, rec.Run.IdempotencyKey,
		rec.FailedStep, rec.ErrorCode,
		profileJSON, rec.WritebackState, writtenBackAt,
		boolToInt(rec.Run.Degraded), rec.Run.DegradeReason,
		trailJSON,
		createdAt.UTC().Format(tsLayout), now.UTC().Format(tsLayout),
	); err != nil {
		return errcode.Newf(errcode.StorageError, "写入判定 run 失败：%v", err)
	}
	return nil
}

// RunByID 回读一条判定 run 的状态快照。
//
// run 不存在时返回 E_NOT_FOUND：调用方据此区分「查错了 id」与「库里出问题了」。
func (s *Store) RunByID(ctx context.Context, runID string) (domain.RunRecord, error) {
	if strings.TrimSpace(runID) == "" {
		return domain.RunRecord{}, errcode.Newf(errcode.Validation, "回读判定 run 需要 run_id")
	}
	rows, err := s.db.QueryContext(ctx, selectRunsSQL+"WHERE run_id = ?", runID)
	if err != nil {
		return domain.RunRecord{}, errcode.Newf(errcode.StorageError, "回读判定 run 失败：%v", err)
	}
	defer func() { _ = rows.Close() }()

	records, err := scanRuns(rows)
	if err != nil {
		return domain.RunRecord{}, err
	}
	if len(records) == 0 {
		return domain.RunRecord{}, errcode.Newf(errcode.NotFound, "判定 run 不存在：%s", runID)
	}
	return records[0], nil
}

// RunByIdempotencyKey 按窗口幂等键回读判定 run。
//
// 它是离线回放的接头：回放重建出的窗口按同一条规则算出同一个幂等键，
// 拿这个键就能找到「当初在线判的是哪一条 run」，把两边的结论逐字段比对。
//
// 幂等键是窗口幂等键（sha256(point_id + window_start)），不含随机成分，
// 所以同一次越温无论判多少次、对上多少次，键都是同一个。
func (s *Store) RunByIdempotencyKey(ctx context.Context, key string) (domain.RunRecord, error) {
	if strings.TrimSpace(key) == "" {
		return domain.RunRecord{}, errcode.Newf(errcode.Validation, "按幂等键回读判定 run 需要 key")
	}
	rows, err := s.db.QueryContext(ctx, selectRunsSQL+"WHERE idempotency_key = ?", key)
	if err != nil {
		return domain.RunRecord{}, errcode.Newf(errcode.StorageError, "按幂等键回读判定 run 失败：%v", err)
	}
	defer func() { _ = rows.Close() }()
	records, err := scanRuns(rows)
	if err != nil {
		return domain.RunRecord{}, err
	}
	if len(records) == 0 {
		return domain.RunRecord{}, errcode.Newf(errcode.NotFound,
			"按幂等键找不到判定 run：%s", key)
	}
	return records[0], nil
}

// RunsByPointInRange 返回某点位在时间区间内的判定 run，按窗口起点升序。
//
// 区间按窗口起点落在 [from, to] 内来取；窗口起点为空的 run 不在结果里
// （没有窗口就没有可复盘的越温）。pointID 为空表示不限点位。
//
// 离线回放用它回答「这段时间这个点位一共判过几段越温」，
// 并找出「在线判过、但回放重建不出来」的那些段——那正是原始读数缺失或
// 温区口径变了的信号。
func (s *Store) RunsByPointInRange(ctx context.Context, pointID string, from, to time.Time) ([]domain.RunRecord, error) {
	query := selectRunsSQL + "WHERE window_start IS NOT NULL AND window_start <> ''"
	args := make([]any, 0, 3)
	if trimmed := strings.TrimSpace(pointID); trimmed != "" {
		query += " AND point_id = ?"
		args = append(args, trimmed)
	}
	if !from.IsZero() {
		query += " AND window_start >= ?"
		args = append(args, from.UTC().Format(tsLayout))
	}
	if !to.IsZero() {
		query += " AND window_start <= ?"
		args = append(args, to.UTC().Format(tsLayout))
	}
	query += " ORDER BY window_start ASC, run_id ASC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, errcode.Newf(errcode.StorageError, "回读区间内判定 run 失败：%v", err)
	}
	defer func() { _ = rows.Close() }()
	return scanRuns(rows)
}

// ResumableRuns 返回需要续跑的判定 run。
//
// 筛选条件有三条，缺一不可：
//   - 执行态不是终态：已经跑完或已经判失败的 run 没有可续的部分；
//   - 写回状态不是「已写回」：台账已经写成的 run 不该被重新写一次；
//   - 按更新时间升序：先处理停得最早的，避免一条反复失败的判定永远排在后面。
func (s *Store) ResumableRuns(ctx context.Context) ([]domain.RunRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		selectRunsSQL+
			"WHERE exec_state NOT IN (?, ?) AND writeback_state <> ? "+
			"ORDER BY updated_at ASC, run_id ASC",
		string(domain.ExecStateSucceeded), string(domain.ExecStateFailed),
		domain.WritebackStateWrittenBack)
	if err != nil {
		return nil, errcode.Newf(errcode.StorageError, "扫描待续跑判定失败：%v", err)
	}
	defer func() { _ = rows.Close() }()
	return scanRuns(rows)
}

// MarkRunWrittenBack 标记一条 run 的台账写入已落成。
//
// 它是「不重复写回」的落点：写回器在台账返回成功后调它，
// 进程重启后扫到的 run 带 written_back 标记，就不会再写第二次。
func (s *Store) MarkRunWrittenBack(ctx context.Context, runID string, at time.Time) error {
	if strings.TrimSpace(runID) == "" {
		return errcode.Newf(errcode.Validation, "标记写回状态需要 run_id")
	}
	if at.IsZero() {
		at = time.Now()
	}
	result, err := s.db.ExecContext(ctx,
		"UPDATE runs SET writeback_state = ?, written_back_at = ?, updated_at = ? WHERE run_id = ?",
		domain.WritebackStateWrittenBack, at.UTC().Format(tsLayout),
		time.Now().UTC().Format(tsLayout), runID)
	if err != nil {
		return errcode.Newf(errcode.StorageError, "标记写回状态失败：%v", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return errcode.Newf(errcode.StorageError, "读取标记结果失败：%v", err)
	}
	if affected == 0 {
		return errcode.Newf(errcode.NotFound, "判定 run 不存在，无法标记写回状态：%s", runID)
	}
	return nil
}

// RunWritebackState 读回一条 run 的写回状态。
//
// 写回器与补偿重放都用它判断「这条是不是已经写过台账了」。
func (s *Store) RunWritebackState(ctx context.Context, runID string) (string, error) {
	if strings.TrimSpace(runID) == "" {
		return "", errcode.Newf(errcode.Validation, "读回写回状态需要 run_id")
	}
	var state string
	err := s.db.QueryRowContext(ctx,
		"SELECT writeback_state FROM runs WHERE run_id = ?", runID).Scan(&state)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", errcode.Newf(errcode.NotFound, "判定 run 不存在：%s", runID)
		}
		return "", errcode.Newf(errcode.StorageError, "读回写回状态失败：%v", err)
	}
	return state, nil
}

// CountRuns 统计判定 run 的条数；pointID 为空表示不限点位。
//
// 运行摘要与台账查询用它核对口径，不必把整表拉出来数。
func (s *Store) CountRuns(ctx context.Context, pointID string) (int, error) {
	query := "SELECT COUNT(*) FROM runs WHERE 1=1"
	args := make([]any, 0, 1)
	if trimmed := strings.TrimSpace(pointID); trimmed != "" {
		query += " AND point_id = ?"
		args = append(args, trimmed)
	}
	var total int
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&total); err != nil {
		return 0, errcode.Newf(errcode.StorageError, "统计判定 run 失败：%v", err)
	}
	return total, nil
}

// scanRuns 把查询结果还原成 run 状态快照。
func scanRuns(rows *sql.Rows) ([]domain.RunRecord, error) {
	records := make([]domain.RunRecord, 0, 8)
	for rows.Next() {
		var (
			rec           domain.RunRecord
			windowStart   sql.NullString
			windowEnd     sql.NullString
			profileJSON   string
			writtenBackAt sql.NullString
			createdAt     string
			updatedAt     string
			execState     string
			degraded      int
			trailJSON     string
		)
		if err := rows.Scan(
			&rec.Run.RunID, &rec.Run.PointID, &windowStart, &windowEnd,
			&rec.Run.State, &execState,
			&rec.Run.ExternalRef, &rec.Run.IdempotencyKey,
			&rec.FailedStep, &rec.ErrorCode,
			&profileJSON, &rec.WritebackState, &writtenBackAt,
			&degraded, &rec.Run.DegradeReason, &trailJSON,
			&createdAt, &updatedAt,
		); err != nil {
			return nil, errcode.Newf(errcode.StorageError, "读取判定 run 行失败：%v", err)
		}
		rec.Run.ExecState = domain.ExecState(execState)
		rec.Run.Degraded = degraded != 0

		// 迁移留痕：存的是一整条链，读回来也是一整条链；
		// 存量库里这一列是空串（迁移补列时给的是默认值），当作没有留痕。
		if strings.TrimSpace(trailJSON) != "" {
			var trail []workflow.Transition
			if err := json.Unmarshal([]byte(trailJSON), &trail); err != nil {
				return nil, errcode.Newf(errcode.StorageError, "还原业务状态迁移留痕失败：%v", err)
			}
			rec.Run.StateTrail = trail
		}

		if windowStart.Valid && strings.TrimSpace(windowStart.String) != "" {
			parsed, err := time.Parse(tsLayout, windowStart.String)
			if err != nil {
				return nil, errcode.Newf(errcode.StorageError, "还原 window_start 失败：%v", err)
			}
			rec.Run.WindowStart = &parsed
		}
		if windowEnd.Valid && strings.TrimSpace(windowEnd.String) != "" {
			parsed, err := time.Parse(tsLayout, windowEnd.String)
			if err != nil {
				return nil, errcode.Newf(errcode.StorageError, "还原 window_end 失败：%v", err)
			}
			rec.Run.WindowEnd = &parsed
		}
		if strings.TrimSpace(profileJSON) != "" {
			var profile domain.PointProfile
			if err := json.Unmarshal([]byte(profileJSON), &profile); err != nil {
				return nil, errcode.Newf(errcode.StorageError, "还原点位温区配置失败：%v", err)
			}
			rec.Profile = &profile
		}
		if writtenBackAt.Valid && strings.TrimSpace(writtenBackAt.String) != "" {
			parsed, err := time.Parse(tsLayout, writtenBackAt.String)
			if err != nil {
				return nil, errcode.Newf(errcode.StorageError, "还原 written_back_at 失败：%v", err)
			}
			rec.WrittenBackAt = &parsed
		}
		if parsed, err := time.Parse(tsLayout, createdAt); err == nil {
			rec.CreatedAt = parsed
		}
		if parsed, err := time.Parse(tsLayout, updatedAt); err == nil {
			rec.UpdatedAt = parsed
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, errcode.Newf(errcode.StorageError, "回读判定 run 失败：%v", err)
	}
	return records, nil
}
