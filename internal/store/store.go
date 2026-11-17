// Package store 是冷链温控判定服务的 SQLite 存储层。
//
// 本包只回答两个问题：原始温度事件落在哪儿、怎么原样查回来。
// 判定 run、步骤产物与审计记录另有各表（后续任务落地），本文件先把事件这一层做扎实：
// 事件是后面所有结论的原始依据，它一旦在库里被改写、被覆盖、被重复，
// 后面每一层算出来的东西都不可信。
//
// 四条约定贯穿本包：
//   - WAL + 单写者：SQLite 单文件部署在院内小主机上，写并发用 WAL 与单连接写队列化解，
//     不靠「多写线程拼运气」；读走同一连接，写入期间的读请求排队而不是撞锁。
//   - 幂等落库：同一事件重复投递只留一行，重复的那次返回既有事件 ID，不覆盖先到的那条。
//   - 不静默改数：入库的是设备当时报的值——原始单位、原始时间文本、精度声明一并留存，
//     回读时原样取回。
//   - 失败显式：库写不进去就说写不进去（E_STORAGE_ERROR），不假装成功。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，院内离线机无需 cgo
)

// tsLayout 是库里 measured_at 的存储格式。
//
// 固定宽度、UTC、纳秒补齐：这样按字符串排序就等于按时间排序，
// 区间回读能直接用字符串比较走索引，不必在每行上做时间解析。
// 现场写入的 +08:00 偏移不会打乱顺序——入库前统一折算成 UTC。
const tsLayout = "2006-01-02T15:04:05.000000000Z"

// schemaVersion 是当前库结构版本号，写进 schema_meta 便于日后迁移时判断存量库。
// v2：新增 run_steps 表（判定步骤产物落库）。
// v3：新增 runs 表（判定 run 状态落库，进程重启后可续跑）。
// v4：新增 run_fact_checks 表（模型输出事实校验记录落库）。
// v5：新增 run_audit_entries 表（模型调用与确认动作的审计链落库）。
// v6：runs 表增 state_trail_json 列（业务状态迁移留痕随 run 落库）。
// v7：新增 run_confirmations 表（人工确认记录与固化的写回载荷落库）；
// run_audit_entries 表增 disposition / opinion 两列。
// v8：新增 run_writebacks 表（出站写回按目标分别记账）。
// v9：新增 writeback_alerts 表（出站写回没落成的告警留痕）。
// v10：新增 point_profiles 表（点位温区配置登记簿）。
// v11：新增 event_hourly_agg 表（小时级聚合表）。
// v12：新增运行摘要取数索引（runs.created_at / runs.state / 审计表 entry_type+created_at）。
const schemaVersion = "12"

// schemaSQL 是建表脚本。全部 IF NOT EXISTS：重复执行即幂等，
// 服务每次启动跑一遍即可，不需要额外的迁移命令。
const schemaSQL = `
CREATE TABLE IF NOT EXISTS schema_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS events (
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

CREATE INDEX IF NOT EXISTS idx_events_point_measured ON events (point_id, measured_at);
CREATE INDEX IF NOT EXISTS idx_events_measured ON events (measured_at);
`

// dataNotesColumnDDL 是给存量库补列的语句。
//
// 建表脚本带 IF NOT EXISTS，对已经建好的库不会重跑；因此新增列必须另走
// 一条幂等的 ALTER：查 pragma table_info，列不在才补。
// 直接改建表脚本的结果是——新库有这个列、老库没有，两种库行为不一致，
// 而这种不一致往往在上线后才被发现。
const dataNotesColumnDDL = "ALTER TABLE events ADD COLUMN data_notes TEXT NOT NULL DEFAULT '{}'"

// insertEventSQL 是事件入库语句。
//
// ON CONFLICT(idempotency_key) DO NOTHING 是幂等的落点：重复投递不覆盖先到的那条，
// 也不报错，由调用方把「这次是重复」这件事回给投递方。
const insertEventSQL = `
INSERT INTO events (
    event_id, idempotency_key, point_id, device_id, device_name,
    measured_at, measured_at_raw, temperature_c, unit, temperature_raw, unit_raw,
    humidity_pct, battery_pct, accuracy_c, source, alarm_flag,
    device_events, precision, data_notes, received_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(idempotency_key) DO NOTHING
`

// selectEventsSQL 是按点位 + 时间区间回读的语句骨架。
// pointID 为空表示不限点位；区间两端都是闭区间。
const selectEventsSQL = `
SELECT event_id, point_id, device_id, device_name, measured_at, measured_at_raw,
       temperature_c, unit, temperature_raw, unit_raw,
       humidity_pct, battery_pct, accuracy_c, source, alarm_flag,
       device_events, precision, data_notes
FROM events
WHERE measured_at >= ? AND measured_at <= ?
`

// Store 是冷链事件的 SQLite 存储。
type Store struct {
	db *sql.DB
}

// Open 打开（或创建）指定路径的库并完成建表。
//
// path 为 ":memory:" 时开内存库（仅测试用）；其余按文件库处理，
// 目录不存在会自动创建——院内部署常直接把数据文件指向 /var/lib 下的子目录。
func Open(path string) (*Store, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, errcode.Newf(errcode.Validation, "数据库路径为空")
	}

	if !isMemoryPath(trimmed) {
		if dir := filepath.Dir(trimmed); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, errcode.Newf(errcode.StorageError, "创建数据库目录失败：%s", dir)
			}
		}
	}

	db, err := sql.Open("sqlite", trimmed)
	if err != nil {
		return nil, errcode.Newf(errcode.StorageError, "打开数据库失败：%v", err)
	}
	// 单写者：SQLite 同一时刻只允许一个写事务，高频上报时靠连接池排队，
	// 不靠并发写换吞吐——写争用在这台小主机上只会换来 "database is locked"。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db}
	if err := s.prepare(context.Background(), trimmed); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// prepare 设置 SQLite 运行参数并建表。
func (s *Store) prepare(ctx context.Context, path string) error {
	// WAL 让读不阻塞写；内存库不支持 WAL，此时跳过并注明。
	if !isMemoryPath(path) {
		var mode string
		if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
			return errcode.Newf(errcode.StorageError, "设置 WAL 日志模式失败：%v", err)
		}
		if !strings.EqualFold(mode, "wal") {
			return errcode.Newf(errcode.StorageError, "设置 WAL 日志模式失败：当前模式为 %q", mode)
		}
	}
	for _, pragma := range []string{
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := s.db.ExecContext(ctx, pragma); err != nil {
			return errcode.Newf(errcode.StorageError, "设置数据库参数失败（%s）：%v", pragma, err)
		}
	}

	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return errcode.Newf(errcode.StorageError, "建表失败：%v", err)
	}
	// 存量库补列：建表脚本对已存在的表不会重跑，新增的列要单独补。
	if err := s.ensureDataNotesColumn(ctx); err != nil {
		return err
	}
	// 判定步骤产物表：同样按「每次启动跑一遍建表脚本」的幂等方式补齐。
	if err := s.prepareRunSteps(ctx); err != nil {
		return err
	}
	// 判定 run 状态表：进程重启后靠它认出「哪条判定没跑完」并接着跑。
	if err := s.prepareRuns(ctx); err != nil {
		return err
	}
	// 事实校验记录表：模型被拦下过的每一个数字都留在这里，供事后审计追溯。
	if err := s.prepareFactChecks(ctx); err != nil {
		return err
	}
	// 审计链表：模型调用元数据与人工确认动作落在同一条链上，供内审与飞检复核。
	if err := s.prepareAuditEntries(ctx); err != nil {
		return err
	}
	// 确认记录表：主管确认了什么、什么时候确认的、当时固化的写回载荷是什么。
	if err := s.prepareConfirmations(ctx); err != nil {
		return err
	}
	// 写回记录表：台账与通知两处目标各自写没写成，补偿重放据此只补没写成的那一处。
	if err := s.prepareWritebacks(ctx); err != nil {
		return err
	}
	// 写回告警表：出站写回没落成时留一条告警，等补偿重放把它解掉。
	if err := s.prepareWritebackAlerts(ctx); err != nil {
		return err
	}
	// 点位温区配置登记簿：判定结论的第一问「这个温区是谁定的」在这里有答案。
	if err := s.preparePointProfiles(ctx); err != nil {
		return err
	}
	// 小时级聚合表：原始采样搬进冷数据后，统计口径留在这张表里。
	if err := s.prepareHourlyAggregates(ctx); err != nil {
		return err
	}
	// 运行摘要用到的索引：没有它们，摘要的计数查询会退化成整表扫描。
	if err := s.prepareMetricsIndexes(ctx); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		"INSERT INTO schema_meta (key, value) VALUES ('schema_version', ?) "+
			"ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		schemaVersion); err != nil {
		return errcode.Newf(errcode.StorageError, "写入库结构版本失败：%v", err)
	}
	return nil
}

// ensureDataNotesColumn 幂等地给 events 表补上 data_notes 列。
func (s *Store) ensureDataNotesColumn(ctx context.Context) error {
	return s.ensureColumn(ctx, "events", "data_notes", dataNotesColumnDDL)
}

// ensureColumn 幂等地给表补上一列：查 pragma table_info，列已在就不动；不在才 ALTER。
//
// 这样同一个库文件反复启动多少次都是同一个结构，
// 新老库的行为也一致——不会出现「新库有这列、老库没有」的分叉。
//
// table 与 column 都是包内常量（不是外部输入），拼进 PRAGMA 是安全的。
func (s *Store) ensureColumn(ctx context.Context, table, column, ddl string) error {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return errcode.Newf(errcode.StorageError, "读取%s表结构失败：%v", table, err)
	}
	defer func() { _ = rows.Close() }()

	exists := false
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
			return errcode.Newf(errcode.StorageError, "读取%s表结构失败：%v", table, err)
		}
		if name == column {
			exists = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		return errcode.Newf(errcode.StorageError, "读取%s表结构失败：%v", table, err)
	}
	if exists {
		return nil
	}

	if _, err := s.db.ExecContext(ctx, ddl); err != nil {
		return errcode.Newf(errcode.StorageError, "补建%s表%s列失败：%v", table, column, err)
	}
	return nil
}

// isMemoryPath 判断是否内存库。
func isMemoryPath(path string) bool {
	return path == ":memory:" || strings.Contains(path, "mode=memory")
}

// Close 关闭底层连接。
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// InsertOutcome 是单条事件入库的结果。
type InsertOutcome struct {
	// EventID 是这条事件在库里的标识。重复投递时返回既有那条的 ID，
	// 调用方拿它去查回读的仍是同一条数据。
	EventID string
	// IdempotencyKey 是事件幂等键（sha256(point_id + measured_at)）。
	IdempotencyKey string
	// Duplicate 表示这次投递命中了已有的同一条采样，没有新写入。
	Duplicate bool
}

// BatchOutcome 是一次批量入库的结果。
type BatchOutcome struct {
	// Inserted 是新写入的行数。
	Inserted int
	// Duplicates 是命中幂等键、没有新写入的行数。
	Duplicates int
	// EventIDs 是逐条事件 ID，顺序与入参一致。
	EventIDs []string
}

// InsertEvent 写入一条已规范化的温度事件。
//
// 重复投递不会覆盖先到的那条，也不会报错：返回既有事件 ID 并置 Duplicate=true，
// 由调用方决定回「已收」还是回「重复」。这正是记录仪重发、网关重试时应有的行为。
func (s *Store) InsertEvent(ctx context.Context, ev domain.TemperatureEvent) (InsertOutcome, error) {
	return s.insertEvent(ctx, s.db, ev)
}

// InsertEvents 在一个事务里批量写入事件。
//
// 单事务：要么这批都落库，要么都不落——批处理跑到一半断电时，
// 库里不会出现「前 300 条在、后 200 条没进」的半截状态。
// 批内出现重复采样不算失败，按幂等键忽略并计入 Duplicates。
func (s *Store) InsertEvents(ctx context.Context, events []domain.TemperatureEvent) (BatchOutcome, error) {
	outcome := BatchOutcome{EventIDs: make([]string, 0, len(events))}
	if len(events) == 0 {
		return outcome, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BatchOutcome{}, errcode.Newf(errcode.StorageError, "开启入库事务失败：%v", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, ev := range events {
		result, err := s.insertEvent(ctx, tx, ev)
		if err != nil {
			return BatchOutcome{}, err
		}
		if result.Duplicate {
			outcome.Duplicates++
		} else {
			outcome.Inserted++
		}
		outcome.EventIDs = append(outcome.EventIDs, result.EventID)
	}

	if err := tx.Commit(); err != nil {
		return BatchOutcome{}, errcode.Newf(errcode.StorageError, "提交入库事务失败：%v", err)
	}
	return outcome, nil
}

// insertEvent 是单条入库的实现，q 可以是 *sql.DB 或 *sql.Tx。
func (s *Store) insertEvent(ctx context.Context, q execer, ev domain.TemperatureEvent) (InsertOutcome, error) {
	if err := checkEvent(ev); err != nil {
		return InsertOutcome{}, err
	}

	key := domain.EventIdempotencyKey(ev.PointID, ev.MeasuredAt)
	eventID := EventIDFromKey(key)

	deviceEvents := ev.Events
	if deviceEvents == nil {
		deviceEvents = []string{}
	}
	eventsJSON, err := json.Marshal(deviceEvents)
	if err != nil {
		return InsertOutcome{}, errcode.Newf(errcode.StorageError, "序列化设备事件失败：%v", err)
	}
	precisionJSON, err := json.Marshal(ev.Precision)
	if err != nil {
		return InsertOutcome{}, errcode.Newf(errcode.StorageError, "序列化精度标注失败：%v", err)
	}
	// 形态标注（编码回退、缺测字段、截断字段）随事件一起落库：
	// 事后回看这条采样时，要能知道它是从什么样的数据里来的。
	dataNotesJSON, err := json.Marshal(ev.DataNotes)
	if err != nil {
		return InsertOutcome{}, errcode.Newf(errcode.StorageError, "序列化数据形态标注失败：%v", err)
	}

	alarmFlag := 0
	if ev.AlarmFlag {
		alarmFlag = 1
	}

	res, err := q.ExecContext(ctx, insertEventSQL,
		eventID, key,
		ev.PointID, ev.DeviceID, ev.DeviceName,
		ev.MeasuredAt.UTC().Format(tsLayout), ev.MeasuredAtRaw,
		ev.TemperatureC, ev.Unit, ev.TemperatureRaw, ev.UnitRaw,
		ev.HumidityPct, ev.BatteryPct, ev.AccuracyC,
		ev.Source, alarmFlag,
		string(eventsJSON), string(precisionJSON), string(dataNotesJSON),
		time.Now().UTC().Format(tsLayout),
	)
	if err != nil {
		return InsertOutcome{}, errcode.Newf(errcode.StorageError, "写入温度事件失败：%v", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return InsertOutcome{}, errcode.Newf(errcode.StorageError, "读取入库结果失败：%v", err)
	}
	if affected > 0 {
		return InsertOutcome{EventID: eventID, IdempotencyKey: key}, nil
	}

	// 没写入 = 命中幂等键：取回既有事件 ID 返回，绝不覆盖先到的那条。
	var existingID string
	if err := q.QueryRowContext(ctx,
		"SELECT event_id FROM events WHERE idempotency_key = ?", key).Scan(&existingID); err != nil {
		return InsertOutcome{}, errcode.Newf(errcode.StorageError, "取回既有事件失败：%v", err)
	}
	return InsertOutcome{EventID: existingID, IdempotencyKey: key, Duplicate: true}, nil
}

// EventsInRange 按点位与时间区间回读事件。
//
// pointID 为空表示不限点位（回放整院某段时间用）。区间两端都是闭区间：
// from 与 to 各自对应的采样点都会被取到——边界少一条，事后复算时
// 就会对不上当初判定的窗口。
//
// 返回按 measured_at 升序排列；同一时刻的多条按事件 ID 排序，保证顺序稳定可复现。
func (s *Store) EventsInRange(ctx context.Context, pointID string, from, to time.Time) ([]domain.TemperatureEvent, error) {
	if to.Before(from) {
		return nil, errcode.Newf(errcode.Validation,
			"回读区间非法：起始时间 %s 晚于结束时间 %s",
			from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	}

	query := selectEventsSQL
	args := []any{from.UTC().Format(tsLayout), to.UTC().Format(tsLayout)}
	if trimmed := strings.TrimSpace(pointID); trimmed != "" {
		query += " AND point_id = ?"
		args = append(args, trimmed)
	}
	query += " ORDER BY measured_at ASC, event_id ASC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, errcode.Newf(errcode.StorageError, "回读温度事件失败：%v", err)
	}
	defer func() { _ = rows.Close() }()

	events := make([]domain.TemperatureEvent, 0, 64)
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, errcode.Newf(errcode.StorageError, "回读温度事件失败：%v", err)
	}
	return events, nil
}

// CountEvents 统计事件条数。
//
// pointID 为空表示不限点位；from / to 任一为零值表示该侧不设边界。
// 报表与运行摘要取数走这里，不去全表扫描。
func (s *Store) CountEvents(ctx context.Context, pointID string, from, to time.Time) (int, error) {
	query := "SELECT COUNT(*) FROM events WHERE 1=1"
	args := make([]any, 0, 3)
	if trimmed := strings.TrimSpace(pointID); trimmed != "" {
		query += " AND point_id = ?"
		args = append(args, trimmed)
	}
	if !from.IsZero() {
		query += " AND measured_at >= ?"
		args = append(args, from.UTC().Format(tsLayout))
	}
	if !to.IsZero() {
		query += " AND measured_at <= ?"
		args = append(args, to.UTC().Format(tsLayout))
	}

	var total int
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&total); err != nil {
		return 0, errcode.Newf(errcode.StorageError, "统计温度事件失败：%v", err)
	}
	return total, nil
}

// ExistingIdempotencyKeys 返回给定幂等键中已经存在于库里的那些。
//
// 批量导入的干跑要用它回答「这批里有多少条库里已经有了」：
// 不查库就报「将导入 N 条」，实跑时却只导进 M 条（N>M），
// 「先干跑看看」这个动作就没有参考价值。
//
// 只读，不写库：干跑就是干跑。
func (s *Store) ExistingIdempotencyKeys(ctx context.Context, keys []string) (map[string]bool, error) {
	found := make(map[string]bool, len(keys))
	if len(keys) == 0 {
		return found, nil
	}

	// SQLite 单条语句的绑定参数上限约 999，按批查，避免超限时整条失败。
	const chunkSize = 500
	for start := 0; start < len(keys); start += chunkSize {
		end := start + chunkSize
		if end > len(keys) {
			end = len(keys)
		}
		batch := keys[start:end]

		args := make([]any, 0, len(batch))
		placeholders := make([]string, 0, len(batch))
		for _, key := range batch {
			args = append(args, key)
			placeholders = append(placeholders, "?")
		}
		query := "SELECT idempotency_key FROM events WHERE idempotency_key IN (" +
			strings.Join(placeholders, ",") + ")"

		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, errcode.Newf(errcode.StorageError, "查询既有温度事件失败：%v", err)
		}
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				_ = rows.Close()
				return nil, errcode.Newf(errcode.StorageError, "读取温度事件幂等键失败：%v", err)
			}
			found[key] = true
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, errcode.Newf(errcode.StorageError, "查询既有温度事件失败：%v", err)
		}
		_ = rows.Close()
	}

	return found, nil
}

// EventIDFromKey 由幂等键派生事件 ID。
//
// 由键派生而不是随机生成：同一条采样无论投递几次，事件 ID 恒定，
// 投递方拿到的回执也就恒定。
func EventIDFromKey(key string) string {
	const prefix = "evt_"
	if len(key) <= 32 {
		return prefix + key
	}
	return prefix + key[:32]
}

// checkEvent 校验入库前的事件是否具备落库所需的最小身份信息。
//
// 这一层挡的是「库里出现一条查不回来的事件」：没有点位就归不了因，
// 没有设备名就填不了异常记录表，缺这两样的事件进了库也只是垃圾。
func checkEvent(ev domain.TemperatureEvent) error {
	if strings.TrimSpace(ev.PointID) == "" {
		return errcode.Newf(errcode.Validation, "缺少 point_id，无法入库")
	}
	if strings.TrimSpace(ev.DeviceID) == "" {
		return errcode.Newf(errcode.Validation, "缺少 device_id，无法入库")
	}
	if strings.TrimSpace(ev.DeviceName) == "" {
		return errcode.Newf(errcode.Validation, "缺少 device_name，无法入库")
	}
	if strings.TrimSpace(ev.Source) == "" {
		return errcode.Newf(errcode.Validation, "缺少 source，无法入库")
	}
	if ev.MeasuredAt.IsZero() {
		return errcode.Newf(errcode.Validation, "缺少 measured_at，无法入库")
	}
	return nil
}

// execer 是 *sql.DB 与 *sql.Tx 的共同最小接口，让单条入库逻辑能同时用于事务内外。
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// scanEvent 把一行记录还原成领域事件。
//
// 原始单位、原始时间文本与精度声明都从库里原样取回：
// 现场复核「设备当时到底报了什么」靠的就是这几列。
func scanEvent(rows *sql.Rows) (domain.TemperatureEvent, error) {
	var (
		ev             domain.TemperatureEvent
		measuredAt     string
		measuredAtRaw  sql.NullString
		unit           string
		temperatureRaw sql.NullFloat64
		unitRaw        sql.NullString
		humidity       sql.NullFloat64
		battery        sql.NullFloat64
		accuracy       sql.NullFloat64
		alarmFlag      int
		deviceEvents   string
		precisionJSON  string
		dataNotesJSON  string
	)

	err := rows.Scan(
		&ev.EventID, &ev.PointID, &ev.DeviceID, &ev.DeviceName, &measuredAt, &measuredAtRaw,
		&ev.TemperatureC, &unit, &temperatureRaw, &unitRaw,
		&humidity, &battery, &accuracy, &ev.Source, &alarmFlag,
		&deviceEvents, &precisionJSON, &dataNotesJSON,
	)
	if err != nil {
		return domain.TemperatureEvent{}, errcode.Newf(errcode.StorageError, "读取温度事件行失败：%v", err)
	}

	// 优先用原始时间文本还原时刻：它带着设备当时用的时区偏移，
	// 回读出来才和现场记录的是同一个写法。原始文本缺失时才退回库里的 UTC 列。
	if measuredAtRaw.Valid && strings.TrimSpace(measuredAtRaw.String) != "" {
		parsed, err := time.Parse(time.RFC3339, measuredAtRaw.String)
		if err != nil {
			return domain.TemperatureEvent{}, errcode.Newf(errcode.StorageError,
				"还原 measured_at 原始时间失败：%v", err)
		}
		ev.MeasuredAt = parsed
		ev.MeasuredAtRaw = measuredAtRaw.String
	} else {
		parsed, err := time.Parse(tsLayout, measuredAt)
		if err != nil {
			return domain.TemperatureEvent{}, errcode.Newf(errcode.StorageError,
				"还原 measured_at 失败：%v", err)
		}
		ev.MeasuredAt = parsed
	}

	ev.Unit = unit
	if temperatureRaw.Valid {
		value := temperatureRaw.Float64
		ev.TemperatureRaw = &value
	}
	if unitRaw.Valid {
		ev.UnitRaw = unitRaw.String
	}
	if humidity.Valid {
		value := humidity.Float64
		ev.HumidityPct = &value
	}
	if battery.Valid {
		value := battery.Float64
		ev.BatteryPct = &value
	}
	if accuracy.Valid {
		value := accuracy.Float64
		ev.AccuracyC = &value
	}
	ev.AlarmFlag = alarmFlag != 0

	if strings.TrimSpace(deviceEvents) != "" {
		var list []string
		if err := json.Unmarshal([]byte(deviceEvents), &list); err != nil {
			return domain.TemperatureEvent{}, errcode.Newf(errcode.StorageError,
				"还原设备事件失败：%v", err)
		}
		ev.Events = list
	}
	if strings.TrimSpace(precisionJSON) != "" {
		if err := json.Unmarshal([]byte(precisionJSON), &ev.Precision); err != nil {
			return domain.TemperatureEvent{}, errcode.Newf(errcode.StorageError,
				"还原精度标注失败：%v", err)
		}
	}
	if strings.TrimSpace(dataNotesJSON) != "" {
		if err := json.Unmarshal([]byte(dataNotesJSON), &ev.DataNotes); err != nil {
			return domain.TemperatureEvent{}, errcode.Newf(errcode.StorageError,
				"还原数据形态标注失败：%v", err)
		}
	}
	return ev, nil
}
