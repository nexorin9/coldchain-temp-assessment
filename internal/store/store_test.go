package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// 本文件的用例围绕三件事：
//   - 同一事件重复投递，库里只有一行；
//   - 按点位 + 时间区间回读，顺序与边界都对；
//   - 批量入库是单事务，批内重复按幂等键忽略。
//
// 用真库（临时目录里的 SQLite 文件）而不是内存替身：
// 幂等靠的是唯一约束，WAL 与事务靠的是 SQLite 的真实行为，
// 拿替身测这些等于没测。

// newTestStore 在临时目录里开一个真实的库。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "coldchain-test.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("打开测试库失败：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// testEvent 造一条脱敏的温度事件。
//
// 点位与设备用占位标识，不含患者、批号等真实标识；
// measured_at 统一带 +08:00 偏移，与院内记录仪的写法一致。
func testEvent(pointID string, at time.Time, tempC float64) domain.TemperatureEvent {
	raw := tempC
	humidity := 55.0
	battery := 88.0
	accuracy := 0.3
	return domain.TemperatureEvent{
		PointID:        pointID,
		DeviceID:       "dev-" + pointID,
		DeviceName:     "测试冷库记录仪",
		MeasuredAt:     at,
		MeasuredAtRaw:  at.Format(time.RFC3339),
		TemperatureC:   tempC,
		Unit:           domain.UnitCelsius,
		TemperatureRaw: &raw,
		UnitRaw:        domain.UnitCelsius,
		HumidityPct:    &humidity,
		BatteryPct:     &battery,
		AccuracyC:      &accuracy,
		Source:         domain.SourceRecorder,
		Events:         []string{domain.EventDoorOpen},
	}
}

// countRows 直接查表计数：断言要落在「表里到底有几行」上，
// 而不是落在某个方法自己的返回值上。
func countRows(t *testing.T, st *Store) int {
	t.Helper()
	var total int
	if err := st.db.QueryRow("SELECT COUNT(*) FROM events").Scan(&total); err != nil {
		t.Fatalf("统计 events 表行数失败：%v", err)
	}
	return total
}

func TestOpen_UsesWALAndCreatesSchema(t *testing.T) {
	st := newTestStore(t)

	var mode string
	if err := st.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("读取 journal_mode 失败：%v", err)
	}
	if mode != "wal" {
		t.Fatalf("期望 WAL 日志模式，实际为 %q", mode)
	}

	var name string
	if err := st.db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='table' AND name='events'").Scan(&name); err != nil {
		t.Fatalf("events 表未创建：%v", err)
	}
}

func TestInsertEvent_DuplicateDeliveryKeepsOneRow(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	event := testEvent("point-vax-freezer-01", time.Date(2026, 10, 20, 3, 15, 0, 0, time.FixedZone("CST", 8*3600)), 11.4)

	var firstID string
	for i := 0; i < 10; i++ {
		outcome, err := st.InsertEvent(ctx, event)
		if err != nil {
			t.Fatalf("第 %d 次写入失败：%v", i+1, err)
		}
		if i == 0 {
			if outcome.Duplicate {
				t.Fatal("首次写入不应被判为重复")
			}
			firstID = outcome.EventID
			continue
		}
		if !outcome.Duplicate {
			t.Fatalf("第 %d 次写入应判为重复", i+1)
		}
		if outcome.EventID != firstID {
			t.Fatalf("重复投递的事件 ID 不一致：期望 %s，实际 %s", firstID, outcome.EventID)
		}
	}

	if rows := countRows(t, st); rows != 1 {
		t.Fatalf("重复投递 10 次后 events 表应为 1 行，实际 %d 行", rows)
	}

	total, err := st.CountEvents(ctx, "point-vax-freezer-01", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("统计事件失败：%v", err)
	}
	if total != 1 {
		t.Fatalf("按点位统计应为 1 条，实际 %d 条", total)
	}
}

func TestInsertEvent_SameInstantDifferentOffsetIsDuplicate(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	zone := time.FixedZone("CST", 8*3600)
	local := testEvent("point-vax-freezer-01", time.Date(2026, 10, 20, 3, 15, 0, 0, zone), 11.4)
	utc := local
	utc.MeasuredAt = local.MeasuredAt.UTC()
	utc.MeasuredAtRaw = local.MeasuredAt.UTC().Format(time.RFC3339)

	if _, err := st.InsertEvent(ctx, local); err != nil {
		t.Fatalf("写入本地时区事件失败：%v", err)
	}
	outcome, err := st.InsertEvent(ctx, utc)
	if err != nil {
		t.Fatalf("写入 UTC 事件失败：%v", err)
	}
	if !outcome.Duplicate {
		t.Fatal("同一采样写成不同偏移应判为重复")
	}
	if rows := countRows(t, st); rows != 1 {
		t.Fatalf("同一采样应只留 1 行，实际 %d 行", rows)
	}
}

func TestInsertEvent_RejectsIncompleteEvent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	event := testEvent("point-vax-freezer-01", time.Now(), 5.0)
	event.PointID = ""
	if _, err := st.InsertEvent(ctx, event); err == nil {
		t.Fatal("缺 point_id 的事件应当被拒绝")
	} else if e := errcode.As(err); e.Code != errcode.Validation {
		t.Fatalf("期望 E_VALIDATION，实际 %s", e.Code)
	}
	if rows := countRows(t, st); rows != 0 {
		t.Fatalf("被拒事件不应入库，实际 %d 行", rows)
	}
}

func TestEventsInRange_OrderedAndBoundariesInclusive(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	base := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	// 故意乱序写入：回读必须按时间排序，与写入顺序无关。
	for _, offset := range []int{4, 0, 2, 1, 3} {
		event := testEvent("point-vax-freezer-01", base.Add(time.Duration(offset)*time.Minute), 5.0+float64(offset))
		if _, err := st.InsertEvent(ctx, event); err != nil {
			t.Fatalf("写入第 %d 条事件失败：%v", offset, err)
		}
	}

	from := base.Add(1 * time.Minute)
	to := base.Add(3 * time.Minute)
	events, err := st.EventsInRange(ctx, "point-vax-freezer-01", from, to)
	if err != nil {
		t.Fatalf("回读失败：%v", err)
	}
	if len(events) != 3 {
		t.Fatalf("闭区间 [from,to] 应回读到 3 条，实际 %d 条", len(events))
	}
	for i := 1; i < len(events); i++ {
		if events[i].MeasuredAt.Before(events[i-1].MeasuredAt) {
			t.Fatalf("回读结果未按 measured_at 升序：%s 早于 %s",
				events[i].MeasuredAt, events[i-1].MeasuredAt)
		}
	}
	if !events[0].MeasuredAt.Equal(from) {
		t.Fatalf("区间下界应被包含，首条为 %s", events[0].MeasuredAt)
	}
	if !events[len(events)-1].MeasuredAt.Equal(to) {
		t.Fatalf("区间上界应被包含，末条为 %s", events[len(events)-1].MeasuredAt)
	}
}

func TestEventsInRange_FiltersByPoint(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	base := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	for _, point := range []string{"point-vax-freezer-01", "point-vax-freezer-02"} {
		event := testEvent(point, base, 6.0)
		if _, err := st.InsertEvent(ctx, event); err != nil {
			t.Fatalf("写入 %s 事件失败：%v", point, err)
		}
	}

	only, err := st.EventsInRange(ctx, "point-vax-freezer-01", base.Add(-time.Hour), base.Add(time.Hour))
	if err != nil {
		t.Fatalf("按点位回读失败：%v", err)
	}
	if len(only) != 1 || only[0].PointID != "point-vax-freezer-01" {
		t.Fatalf("按点位回读应只返回该点位的 1 条，实际 %d 条", len(only))
	}

	all, err := st.EventsInRange(ctx, "", base.Add(-time.Hour), base.Add(time.Hour))
	if err != nil {
		t.Fatalf("不限点位回读失败：%v", err)
	}
	if len(all) != 2 {
		t.Fatalf("不限点位回读应返回 2 条，实际 %d 条", len(all))
	}
}

func TestEventsInRange_RejectsInvertedInterval(t *testing.T) {
	st := newTestStore(t)
	base := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)

	_, err := st.EventsInRange(context.Background(), "point-vax-freezer-01", base, base.Add(-time.Minute))
	if err == nil {
		t.Fatal("起始晚于结束的区间应当被拒绝")
	}
	if e := errcode.As(err); e.Code != errcode.Validation {
		t.Fatalf("期望 E_VALIDATION，实际 %s", e.Code)
	}
}

func TestInsertEvents_BatchIsSingleTransaction(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	base := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	events := make([]domain.TemperatureEvent, 0, 5)
	for i := 0; i < 5; i++ {
		events = append(events, testEvent("point-vax-freezer-01", base.Add(time.Duration(i)*time.Minute), 5.0))
	}
	// 批内再放一条重复采样：应按幂等键忽略，而不是覆盖或报错。
	events = append(events, events[0])

	outcome, err := st.InsertEvents(ctx, events)
	if err != nil {
		t.Fatalf("批量入库失败：%v", err)
	}
	if outcome.Inserted != 5 {
		t.Fatalf("期望新写入 5 条，实际 %d 条", outcome.Inserted)
	}
	if outcome.Duplicates != 1 {
		t.Fatalf("期望命中幂等键 1 条，实际 %d 条", outcome.Duplicates)
	}
	if len(outcome.EventIDs) != 6 {
		t.Fatalf("期望逐条返回 6 个事件 ID，实际 %d 个", len(outcome.EventIDs))
	}
	if rows := countRows(t, st); rows != 5 {
		t.Fatalf("批量入库后 events 表应为 5 行，实际 %d 行", rows)
	}
}

func TestInsertEvents_RollsBackOnInvalidRow(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	base := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	good := testEvent("point-vax-freezer-01", base, 5.0)
	bad := testEvent("point-vax-freezer-01", base.Add(time.Minute), 6.0)
	bad.PointID = ""

	if _, err := st.InsertEvents(ctx, []domain.TemperatureEvent{good, bad}); err == nil {
		t.Fatal("含非法行的事务应当失败")
	}
	if rows := countRows(t, st); rows != 0 {
		t.Fatalf("事务回滚后 events 表应为 0 行，实际 %d 行", rows)
	}
}

func TestEventsInRange_RoundTripsFieldValues(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	zone := time.FixedZone("CST", 8*3600)
	at := time.Date(2026, 10, 20, 3, 15, 0, 0, zone)
	event := testEvent("point-vax-freezer-01", at, 11.4)
	event.TemperatureRaw = nil // 设备只报了摄氏度，没有换算前的原始值
	event.UnitRaw = ""
	event.HumidityPct = nil
	event.BatteryPct = nil
	event.AccuracyC = nil
	event.Precision = domain.PrecisionNote{Limited: true, LimitC: 0.5, Tag: "精度不足，仅供参考"}

	if _, err := st.InsertEvent(ctx, event); err != nil {
		t.Fatalf("写入事件失败：%v", err)
	}

	events, err := st.EventsInRange(ctx, "point-vax-freezer-01", at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil {
		t.Fatalf("回读失败：%v", err)
	}
	if len(events) != 1 {
		t.Fatalf("期望回读到 1 条，实际 %d 条", len(events))
	}
	got := events[0]

	if !got.MeasuredAt.Equal(at) {
		t.Fatalf("回读的测量时刻与写入不一致：期望 %s，实际 %s", at, got.MeasuredAt)
	}
	if got.MeasuredAtRaw != at.Format(time.RFC3339) {
		t.Fatalf("原始时间文本未被原样保留：%q", got.MeasuredAtRaw)
	}
	if got.TemperatureC != 11.4 {
		t.Fatalf("温度回读不一致：期望 11.4，实际 %v", got.TemperatureC)
	}
	if got.TemperatureRaw != nil || got.UnitRaw != "" {
		t.Fatal("缺测的原始值不应被回读成零值")
	}
	if got.HumidityPct != nil || got.BatteryPct != nil || got.AccuracyC != nil {
		t.Fatal("缺测的可选数值不应被回读成零值")
	}
	if len(got.Events) != 1 || got.Events[0] != domain.EventDoorOpen {
		t.Fatalf("设备事件未被原样保留：%v", got.Events)
	}
	if !got.Precision.Limited || got.Precision.Tag != "精度不足，仅供参考" {
		t.Fatalf("精度标注未被原样保留：%+v", got.Precision)
	}
	if got.Source != domain.SourceRecorder {
		t.Fatalf("来源未被原样保留：%q", got.Source)
	}
}

// TestOpen_MigratesLegacyDBWithoutDataNotes 存量库补列。
//
// 建表脚本带 IF NOT EXISTS，对已建好的库不会重跑；新增的列必须另走
// 一条幂等的 ALTER。这里造一个「老结构」的库文件（events 表没有
// data_notes 列），再用 Open 打开它，验证列被补上、且不影响原有数据。
func TestOpen_MigratesLegacyDBWithoutDataNotes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开老库失败：%v", err)
	}
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
    received_at     TEXT NOT NULL
);
INSERT INTO events (
    event_id, idempotency_key, point_id, device_id, device_name,
    measured_at, measured_at_raw, temperature_c, unit, temperature_raw, unit_raw,
    humidity_pct, battery_pct, accuracy_c, source, alarm_flag,
    device_events, precision, received_at
) VALUES (
    'evt_legacy0001', 'legacy-key-0001', 'point-vax-freezer-01', 'device-recorder-0001',
    '疫苗冷藏库温度记录仪', '2026-10-20T00:00:00.000000000Z', '2026-10-20T08:00:00+08:00',
    5.0, 'C', 5.0, 'C', 58, 92, 0.3, 'recorder', 0, '[]', '{}',
    '2026-10-20T00:00:01.000000000Z'
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

	// 列已补上。
	rows, err := st.db.Query("PRAGMA table_info(events)")
	if err != nil {
		t.Fatalf("读取表结构失败：%v", err)
	}
	defer func() { _ = rows.Close() }()
	hasDataNotes := false
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
		if name == "data_notes" {
			hasDataNotes = true
		}
	}
	if !hasDataNotes {
		t.Fatal("存量库未补上 data_notes 列")
	}

	// 老数据没丢，且补出来的列有默认值。
	events, err := st.EventsInRange(context.Background(), "point-vax-freezer-01",
		time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("回读老数据失败：%v", err)
	}
	if len(events) != 1 {
		t.Fatalf("老库数据应仍在，回读到 %d 条", len(events))
	}
	if events[0].DeviceName != "疫苗冷藏库温度记录仪" {
		t.Fatalf("老库数据被改动了：%q", events[0].DeviceName)
	}
	if events[0].DataNotes.Encoding != "" {
		t.Fatalf("老库行的形态标注应为空：%+v", events[0].DataNotes)
	}
}

// TestInsertEvent_RoundTripsDataNotes 形态标注随事件落库并可原样回读。
func TestInsertEvent_RoundTripsDataNotes(t *testing.T) {
	st := newTestStore(t)

	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	ev := testEvent("point-vax-freezer-01", at, 9.6)
	ev.DataNotes = domain.DataNotes{
		Encoding:         "gbk",
		EncodingFallback: true,
		MissingFields:    []string{"humidity_pct"},
		Truncated:        true,
		TruncatedFields:  []string{"device_name"},
		ExtraValues:      2,
	}

	if _, err := st.InsertEvent(context.Background(), ev); err != nil {
		t.Fatalf("写入事件失败：%v", err)
	}

	events, err := st.EventsInRange(context.Background(), "point-vax-freezer-01", at, at)
	if err != nil {
		t.Fatalf("回读事件失败：%v", err)
	}
	if len(events) != 1 {
		t.Fatalf("期望回读到 1 条，实际 %d 条", len(events))
	}

	got := events[0].DataNotes
	if got.Encoding != "gbk" || !got.EncodingFallback {
		t.Fatalf("编码标注未原样保留：%+v", got)
	}
	if !got.Truncated || len(got.TruncatedFields) != 1 || got.TruncatedFields[0] != "device_name" {
		t.Fatalf("截断标注未原样保留：%+v", got)
	}
	if len(got.MissingFields) != 1 || got.MissingFields[0] != "humidity_pct" {
		t.Fatalf("缺测标注未原样保留：%+v", got)
	}
	if got.ExtraValues != 2 {
		t.Fatalf("多余取值计数未原样保留：%+v", got)
	}
}
