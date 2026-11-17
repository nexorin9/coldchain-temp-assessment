package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// 测试用点位标识（占位值，不含任何真实机构信息）。
const testRunPointID = "point-vax-freezer-01"

// sampleRunRecord 造一条判定 run 的状态快照。
func sampleRunRecord(runID string, execState domain.ExecState) domain.RunRecord {
	start := time.Date(2026, 10, 20, 3, 10, 0, 0, time.UTC)
	end := time.Date(2026, 10, 20, 3, 25, 0, 0, time.UTC)
	run := domain.Run{
		RunID:          runID,
		PointID:        testRunPointID,
		State:          domain.RunStateAwaitingConfirmation,
		ExecState:      execState,
		ExternalRef:    domain.ExternalRef(testRunPointID, start),
		IdempotencyKey: domain.WindowIdempotencyKey(testRunPointID, start),
	}
	run.BindWindow(start, end)
	return domain.RunRecord{
		Run:     run,
		Profile: &domain.PointProfile{PointID: testRunPointID, ZoneMinC: 2, ZoneMaxC: 8},
	}
}

// TestUpsertRun_RoundTrips 验证 run 状态快照的落库与回读。
//
// 落库的是整条快照，回读出来的必须逐字段对得上——
// 库里那一份是判定真相的唯一来源，对不上就等于没落。
func TestUpsertRun_RoundTrips(t *testing.T) {
	ctx := context.Background()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	defer func() { _ = st.Close() }()

	rec := sampleRunRecord("run-round-trip", domain.ExecStateSucceeded)
	rec.FailedStep = ""
	rec.ErrorCode = ""
	rec.UpdatedAt = time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC)

	if err := st.UpsertRun(ctx, rec); err != nil {
		t.Fatalf("写入 run 失败：%v", err)
	}
	got, err := st.RunByID(ctx, "run-round-trip")
	if err != nil {
		t.Fatalf("回读 run 失败：%v", err)
	}
	if got.Run.RunID != rec.Run.RunID || got.Run.PointID != rec.Run.PointID {
		t.Fatalf("回读的 run 身份不符：%+v", got.Run)
	}
	if got.Run.ExecState != domain.ExecStateSucceeded {
		t.Fatalf("回读的执行态不符：%q", got.Run.ExecState)
	}
	if got.Run.State != domain.RunStateAwaitingConfirmation {
		t.Fatalf("回读的业务状态不符：%q", got.Run.State)
	}
	if got.Run.ExternalRef != rec.Run.ExternalRef {
		t.Fatalf("回读的 external_ref 不符：%q", got.Run.ExternalRef)
	}
	if got.Run.WindowStart == nil || got.Run.WindowEnd == nil {
		t.Fatal("回读的窗口起止不应为空")
	}
	if !got.Run.WindowStart.Equal(*rec.Run.WindowStart) {
		t.Fatalf("回读的窗口起点不符：%v", got.Run.WindowStart)
	}
	if got.Profile == nil {
		t.Fatal("温区配置应随 run 一起落库")
	}
	if got.Profile.ZoneMinC != 2 || got.Profile.ZoneMaxC != 8 {
		t.Fatalf("回读的温区配置不符：%v~%v", got.Profile.ZoneMinC, got.Profile.ZoneMaxC)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("落库后应有创建时刻")
	}
}

// TestUpsertRun_RejectsIncompleteRun 验证缺身份信息的 run 不入库。
func TestUpsertRun_RejectsIncompleteRun(t *testing.T) {
	ctx := context.Background()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	defer func() { _ = st.Close() }()

	if err := st.UpsertRun(ctx, domain.RunRecord{Run: domain.Run{PointID: testRunPointID}}); err == nil {
		t.Fatal("缺 run_id 的 run 不应入库")
	} else if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.Validation, code)
	}
	if err := st.UpsertRun(ctx, domain.RunRecord{Run: domain.Run{RunID: "run-x"}}); err == nil {
		t.Fatal("缺 point_id 的 run 不应入库")
	} else if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.Validation, code)
	}
}

// TestUpsertRun_KeepsCreatedAtOnUpdate 验证创建时刻不被后续更新覆盖：
// 「这条判定是什么时候开始的」不能被后面每一步的推进改掉。
func TestUpsertRun_KeepsCreatedAtOnUpdate(t *testing.T) {
	ctx := context.Background()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	defer func() { _ = st.Close() }()

	rec := sampleRunRecord("run-created-at", domain.ExecStateRunning)
	rec.CreatedAt = time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	rec.UpdatedAt = time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	if err := st.UpsertRun(ctx, rec); err != nil {
		t.Fatalf("写入 run 失败：%v", err)
	}

	rec.Run.ExecState = domain.ExecStateSucceeded
	rec.UpdatedAt = time.Date(2026, 10, 20, 3, 30, 0, 0, time.UTC)
	if err := st.UpsertRun(ctx, rec); err != nil {
		t.Fatalf("更新 run 失败：%v", err)
	}

	got, err := st.RunByID(ctx, "run-created-at")
	if err != nil {
		t.Fatalf("回读 run 失败：%v", err)
	}
	if !got.CreatedAt.Equal(rec.CreatedAt) {
		t.Fatalf("创建时刻被覆盖了：%v ≠ %v", got.CreatedAt, rec.CreatedAt)
	}
	if !got.UpdatedAt.Equal(rec.UpdatedAt) {
		t.Fatalf("更新时刻不符：%v ≠ %v", got.UpdatedAt, rec.UpdatedAt)
	}
}

// TestMarkRunWrittenBack_MarksAndIsIdempotent 验证写回标记：
// 标记后能读回，且重复标记不报错（补偿重放可能重复调它）。
func TestMarkRunWrittenBack_MarksAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	defer func() { _ = st.Close() }()

	if err := st.UpsertRun(ctx, sampleRunRecord("run-wb", domain.ExecStateSucceeded)); err != nil {
		t.Fatalf("写入 run 失败：%v", err)
	}
	at := time.Date(2026, 10, 20, 5, 0, 0, 0, time.UTC)
	if err := st.MarkRunWrittenBack(ctx, "run-wb", at); err != nil {
		t.Fatalf("标记写回状态失败：%v", err)
	}
	state, err := st.RunWritebackState(ctx, "run-wb")
	if err != nil {
		t.Fatalf("读回写回状态失败：%v", err)
	}
	if state != domain.WritebackStateWrittenBack {
		t.Fatalf("写回状态应为 %s，实际 %q", domain.WritebackStateWrittenBack, state)
	}
	got, err := st.RunByID(ctx, "run-wb")
	if err != nil {
		t.Fatalf("回读 run 失败：%v", err)
	}
	if got.WrittenBackAt == nil {
		t.Fatal("写回时刻不应为空")
	}
	if !got.WrittenBackAt.Equal(at) {
		t.Fatalf("写回时刻不符：%v ≠ %v", got.WrittenBackAt, at)
	}

	// 重复标记不报错。
	if err := st.MarkRunWrittenBack(ctx, "run-wb", at); err != nil {
		t.Fatalf("重复标记写回状态不应报错：%v", err)
	}
}

// TestMarkRunWrittenBack_MissingRun 验证给不存在的 run 标记写回状态会明确报错。
func TestMarkRunWrittenBack_MissingRun(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	defer func() { _ = st.Close() }()

	err = st.MarkRunWrittenBack(context.Background(), "run-missing", time.Now())
	if err == nil {
		t.Fatal("给不存在的 run 标记写回状态应报错")
	}
	if code := errcode.As(err).Code; code != errcode.NotFound {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.NotFound, code)
	}
}

// TestOpen_MigratesLegacyDBWithoutRunsTable 验证存量库补表：
// 只有 events 与 run_steps（没有 runs 表）的老库被打开后，runs 表建好且能正常写入。
//
// 这是「升级不停服」的前提：老库文件不能因为新表缺失就开不起来。
func TestOpen_MigratesLegacyDBWithoutRunsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-runs.db")

	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开老库失败：%v", err)
	}
	// 造一个 v2 结构的库：有 events 与 run_steps，没有 runs。
	legacySchema := `
CREATE TABLE events (
    event_id        TEXT PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE,
    point_id        TEXT NOT NULL,
    device_id       TEXT NOT NULL,
    device_name     TEXT NOT NULL,
    measured_at     TEXT NOT NULL,
    measured_at_raw TEXT,
    temperature_c   REAL NOT NULL,
    unit            TEXT NOT NULL,
    temperature_raw REAL,
    unit_raw        TEXT,
    humidity_pct    REAL,
    battery_pct     REAL,
    accuracy_c      REAL,
    source          TEXT NOT NULL,
    alarm_flag      INTEGER NOT NULL DEFAULT 0,
    device_events   TEXT NOT NULL DEFAULT '[]',
    precision       TEXT NOT NULL DEFAULT '{}',
    data_notes      TEXT NOT NULL DEFAULT '{}',
    received_at     TEXT NOT NULL
);
CREATE TABLE run_steps (
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
`
	if _, err := legacy.Exec(legacySchema); err != nil {
		t.Fatalf("造老库失败：%v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("关闭老库失败：%v", err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("打开老库失败（补表应幂等）：%v", err)
	}
	defer func() { _ = st.Close() }()

	if err := st.UpsertRun(context.Background(), sampleRunRecord("run-legacy", domain.ExecStateRunning)); err != nil {
		t.Fatalf("在补过的老库里写 run 失败：%v", err)
	}
	got, err := st.RunByID(context.Background(), "run-legacy")
	if err != nil {
		t.Fatalf("回读 run 失败：%v", err)
	}
	if got.Run.RunID != "run-legacy" {
		t.Fatalf("回读的 run 不符：%+v", got.Run)
	}
}

// TestOpen_MigratesLegacyRunsTableWithoutNewColumns 验证存量 runs 表补列：
// 老表缺 failed_step / writeback_state 等列时，补齐后老数据仍在。
func TestOpen_MigratesLegacyRunsTableWithoutNewColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-runs-columns.db")

	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开老库失败：%v", err)
	}
	legacySchema := `
CREATE TABLE runs (
    run_id          TEXT PRIMARY KEY,
    point_id        TEXT NOT NULL,
    window_start    TEXT,
    window_end      TEXT,
    state           TEXT NOT NULL DEFAULT 'detected',
    exec_state      TEXT NOT NULL DEFAULT 'queued',
    external_ref    TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT NOT NULL DEFAULT '',
    profile_json    TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);
INSERT INTO runs (
    run_id, point_id, window_start, window_end, state, exec_state,
    external_ref, idempotency_key, profile_json, created_at, updated_at
) VALUES (
    'run-old-0001', 'point-vax-freezer-01', NULL, NULL, 'detected', 'running',
    '', '', '', '2026-10-20T00:00:00.000000000Z', '2026-10-20T00:00:00.000000000Z'
);
`
	if _, err := legacy.Exec(legacySchema); err != nil {
		t.Fatalf("造老库失败：%v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("关闭老库失败：%v", err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("打开老库失败（补列应幂等）：%v", err)
	}
	defer func() { _ = st.Close() }()

	for _, column := range []string{
		"failed_step", "error_code", "writeback_state", "written_back_at",
		"degraded", "degrade_reason", "state_trail_json",
	} {
		found := false
		rows, err := st.db.Query("PRAGMA table_info(runs)")
		if err != nil {
			t.Fatalf("读取表结构失败：%v", err)
		}
		for rows.Next() {
			var (
				cid        int
				name       string
				columnType string
				notNull    int
				defaultVal any
				primaryKey int
			)
			if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultVal, &primaryKey); err != nil {
				t.Fatalf("读取表结构失败：%v", err)
			}
			if name == column {
				found = true
			}
		}
		_ = rows.Close()
		if !found {
			t.Fatalf("存量 runs 表未补上 %s 列", column)
		}
	}

	// 老数据没丢。
	got, err := st.RunByID(context.Background(), "run-old-0001")
	if err != nil {
		t.Fatalf("回读老数据失败：%v", err)
	}
	if got.Run.RunID != "run-old-0001" {
		t.Fatalf("回读的老 run 不符：%+v", got.Run)
	}
	if got.Run.ExecState != domain.ExecStateRunning {
		t.Fatalf("老 run 的执行态不符：%q", got.Run.ExecState)
	}
	if got.WritebackState != "" {
		t.Fatalf("补出的写回状态应为空，实际 %q", got.WritebackState)
	}
}

// TestOpen_RepeatedOpenKeepsSameSchema 验证同一个库文件反复启动多少次都是同一个结构。
func TestOpen_RepeatedOpenKeepsSameSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repeat.db")
	for i := 0; i < 3; i++ {
		st, err := Open(path)
		if err != nil {
			t.Fatalf("第 %d 次打开库失败：%v", i+1, err)
		}
		if err := st.UpsertRun(context.Background(), sampleRunRecord("run-repeat", domain.ExecStateRunning)); err != nil {
			t.Fatalf("第 %d 次写入 run 失败：%v", i+1, err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("第 %d 次关闭库失败：%v", i+1, err)
		}
	}
	st, err := Open(path)
	if err != nil {
		t.Fatalf("打开库失败：%v", err)
	}
	defer func() { _ = st.Close() }()
	count, err := st.CountRuns(context.Background(), "")
	if err != nil {
		t.Fatalf("统计 run 条数失败：%v", err)
	}
	if count != 1 {
		t.Fatalf("反复开关库后应只有 1 行 run，实际 %d 行", count)
	}
}

// TestUpsertRun_RoundTripsStateTrail 验证业务状态迁移留痕的落库与回读。
//
// 留痕落库的意义：进程重启后仍能回答「这条判定是怎么一步步走到今天这个状态的」。
// 只存在内存里的留痕，等于没有留痕。
func TestUpsertRun_RoundTripsStateTrail(t *testing.T) {
	ctx := context.Background()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	defer func() { _ = st.Close() }()

	rec := sampleRunRecord("run-trail", domain.ExecStateSucceeded)
	at := time.Date(2026, 10, 20, 3, 30, 0, 0, time.UTC)
	rec.Run.StateTrail = []workflow.Transition{
		{
			From:   workflow.StateDetected,
			To:     workflow.StateAssessed,
			Action: workflow.ActionAuto,
			Reason: "excursion_window_extracted",
			Note:   "越温窗口已抽出，给出可核对的判定结论",
			Actor:  workflow.ActorSystem,
			At:     at,
		},
		{
			From:   workflow.StateAssessed,
			To:     workflow.StateAwaitingConfirmation,
			Action: workflow.ActionAuto,
			Reason: "draft_ready_for_confirmation",
			Actor:  workflow.ActorSystem,
			At:     at.Add(time.Minute),
		},
	}

	if err := st.UpsertRun(ctx, rec); err != nil {
		t.Fatalf("写入 run 失败：%v", err)
	}
	got, err := st.RunByID(ctx, "run-trail")
	if err != nil {
		t.Fatalf("回读 run 失败：%v", err)
	}
	if len(got.Run.StateTrail) != 2 {
		t.Fatalf("迁移留痕条数不符：实际 %d，期望 2", len(got.Run.StateTrail))
	}
	first := got.Run.StateTrail[0]
	if first.From != workflow.StateDetected || first.To != workflow.StateAssessed {
		t.Fatalf("首条留痕起止状态不符：%s → %s", first.From, first.To)
	}
	if first.Reason != "excursion_window_extracted" {
		t.Fatalf("首条留痕原因码不符：%s", first.Reason)
	}
	if first.Note != "越温窗口已抽出，给出可核对的判定结论" {
		t.Fatalf("首条留痕补注不符：%s", first.Note)
	}
	if !first.At.Equal(at) {
		t.Fatalf("首条留痕时刻不符：%s", first.At)
	}
	second := got.Run.StateTrail[1]
	if second.To != workflow.StateAwaitingConfirmation || second.Reason != "draft_ready_for_confirmation" {
		t.Fatalf("次条留痕不符：%s → %s（%s）", second.From, second.To, second.Reason)
	}
}

// TestUpsertRun_NoTrailLeavesColumnEmpty 验证没有留痕的 run 不写假数据。
//
// 留痕列存的是 JSON 数组：空数组与「没留痕」不是一回事，
// 库里统一存空串，回读时也不硬凑一条空记录。
func TestUpsertRun_NoTrailLeavesColumnEmpty(t *testing.T) {
	ctx := context.Background()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	defer func() { _ = st.Close() }()

	rec := sampleRunRecord("run-no-trail", domain.ExecStateSucceeded)
	if err := st.UpsertRun(ctx, rec); err != nil {
		t.Fatalf("写入 run 失败：%v", err)
	}
	got, err := st.RunByID(ctx, "run-no-trail")
	if err != nil {
		t.Fatalf("回读 run 失败：%v", err)
	}
	if len(got.Run.StateTrail) != 0 {
		t.Fatalf("没有留痕的 run 回读出 %d 条留痕", len(got.Run.StateTrail))
	}
	var raw string
	if err := st.db.QueryRowContext(ctx,
		"SELECT state_trail_json FROM runs WHERE run_id = ?", "run-no-trail").Scan(&raw); err != nil {
		t.Fatalf("直查留痕列失败：%v", err)
	}
	if raw != "" {
		t.Fatalf("没有留痕的 run 留痕列应为空串，实际 %q", raw)
	}
}
