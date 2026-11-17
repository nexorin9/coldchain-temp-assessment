package store

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
)

// 本文件的用例围绕容量演进这条路上的三件事：
//   - 小时级聚合的数值对得上（事件数、越温次数、峰值、平均、最低）；
//   - 归档真的把过期采样搬出了热表（热表行数与归档文件行数对得上）；
//   - 归档产物读得回来，而且读回来的采样还能当取数来源用（离线回放的前提）。
//
// 用真库（临时目录里的 SQLite 文件）与真文件（磁盘上的 gzip JSONL）：
// 归档这件事的风险全在「文件与库表之间的一致性」上，
// 拿内存替身测这些等于没测。

// 归档测试用的点位标识（占位值，不含任何真实机构信息）。
const (
	aggPointA = "point-vax-freezer-01"
	aggPointB = "point-vax-fridge-02"
)

// sampleSourceLike 是离线回放要求的取数形状。
//
// 它与 runtime.SampleSource 一致，但在这里本地声明一份：
// 存储层的测试不必依赖运行时层，也不会因为运行时层改了接口而连带改测试。
type sampleSourceLike interface {
	EventsInRange(ctx context.Context, pointID string, from, to time.Time) ([]domain.TemperatureEvent, error)
}

// 编译期断言：归档来源满足离线回放的取数形状。
var _ sampleSourceLike = (*ArchiveSource)(nil)

// insertSamples 把一批采样落库。
func insertSamples(t *testing.T, st *Store, events []domain.TemperatureEvent) {
	t.Helper()
	for _, ev := range events {
		if _, err := st.InsertEvent(context.Background(), ev); err != nil {
			t.Fatalf("写入采样失败（%s %s）：%v", ev.PointID, ev.MeasuredAt, err)
		}
	}
}

// hourlyTestSamples 造一批跨两个小时桶的采样。
//
// 口径：点位 A 在 03:00 桶里有 3 条（4.0 / 9.5 / 6.0℃），
// 04:00 桶里有 2 条（5.0 / 7.5℃）；点位 B 在 03:00 桶里有 1 条（-70.0℃）。
// 时间为 UTC，对应院内本地 11:00 与 12:00 两个桶（+08:00 偏移）。
func hourlyTestSamples() []domain.TemperatureEvent {
	base := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	second := base.Add(time.Hour)
	return []domain.TemperatureEvent{
		testEvent(aggPointA, base, 4.0),
		testEvent(aggPointA, base.Add(10*time.Minute), 9.5),
		testEvent(aggPointA, base.Add(20*time.Minute), 6.0),
		testEvent(aggPointA, second, 5.0),
		testEvent(aggPointA, second.Add(10*time.Minute), 7.5),
		testEvent(aggPointB, base, -70.0),
	}
}

// TestRefreshHourlyAggregates_CountsAndExtremesAreCorrect 验证小时级聚合的数值。
//
// 断言落在「桶里的数字对不对」上：事件数、越温次数、峰值、平均、最低。
// 口径三条：越温次数按窗口起点归桶（跨小时的同一段只算在开始的那一小时），
// 峰值取桶内最高读数，平均按桶内采样算术平均。
func TestRefreshHourlyAggregates_CountsAndExtremesAreCorrect(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	insertSamples(t, st, hourlyTestSamples())

	// 一段越温窗口：起点落在 03:00 桶（本地 11:00），跨到 03:30。
	// 它只算在 03:00 这一小时，不在 04:00 桶里再算一次。
	start := time.Date(2026, 10, 20, 3, 10, 0, 0, time.UTC)
	rec := sampleRunRecord("run-agg-1", domain.ExecStateSucceeded)
	rec.Run.RunID = "run-agg-1"
	rec.Run.PointID = aggPointA
	rec.Run.WindowStart = &start
	end := start.Add(20 * time.Minute)
	rec.Run.WindowEnd = &end
	if err := st.UpsertRun(ctx, rec); err != nil {
		t.Fatalf("写入 run 失败：%v", err)
	}

	from := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 20, 5, 0, 0, 0, time.UTC)
	written, err := st.RefreshHourlyAggregates(ctx, "", from, to, FixedZoneOffset)
	if err != nil {
		t.Fatalf("重算小时级聚合失败：%v", err)
	}
	if written != 3 {
		t.Fatalf("期望写出 3 个小时桶（点位 A 两个 + 点位 B 一个），实际 %d", written)
	}

	aggs, err := st.HourlyAggregates(ctx, aggPointA, from, to)
	if err != nil {
		t.Fatalf("回读小时级聚合失败：%v", err)
	}
	if len(aggs) != 2 {
		t.Fatalf("点位 A 应有 2 个小时桶，实际 %d", len(aggs))
	}

	first := aggs[0]
	if first.EventCount != 3 {
		t.Fatalf("03:00 桶事件数应为 3，实际 %d", first.EventCount)
	}
	if first.ExcursionCount != 1 {
		t.Fatalf("03:00 桶越温次数应为 1（窗口起点落在这一小时），实际 %d", first.ExcursionCount)
	}
	if first.PeakTemperatureC != 9.5 {
		t.Fatalf("03:00 桶峰值应为 9.5℃，实际 %.2f℃", first.PeakTemperatureC)
	}
	if first.MinTemperatureC != 4.0 {
		t.Fatalf("03:00 桶最低温应为 4.0℃，实际 %.2f℃", first.MinTemperatureC)
	}
	wantAvg := (4.0 + 9.5 + 6.0) / 3
	if diff := first.AvgTemperatureC - wantAvg; diff > 0.001 || diff < -0.001 {
		t.Fatalf("03:00 桶平均温应为 %.4f℃，实际 %.4f℃", wantAvg, first.AvgTemperatureC)
	}

	second := aggs[1]
	if second.EventCount != 2 {
		t.Fatalf("04:00 桶事件数应为 2，实际 %d", second.EventCount)
	}
	if second.ExcursionCount != 0 {
		t.Fatalf("04:00 桶越温次数应为 0（上一段越温起点不在这一小时），实际 %d", second.ExcursionCount)
	}
	if second.PeakTemperatureC != 7.5 {
		t.Fatalf("04:00 桶峰值应为 7.5℃，实际 %.2f℃", second.PeakTemperatureC)
	}
	if second.AvgTemperatureC != 6.25 {
		t.Fatalf("04:00 桶平均温应为 6.25℃，实际 %.4f℃", second.AvgTemperatureC)
	}
}

// TestRefreshHourlyAggregates_RecomputeDoesNotDuplicate 验证重算是幂等的。
//
// 补投采样后重刷一次是常态（重算必须能跑）。若重算插出第二行，
// 「这一小时有几条采样」会凭空翻倍，趋势图上就会出现一段假的尖峰。
func TestRefreshHourlyAggregates_RecomputeDoesNotDuplicate(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	insertSamples(t, st, hourlyTestSamples())

	from := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 20, 5, 0, 0, 0, time.UTC)
	if _, err := st.RefreshHourlyAggregates(ctx, "", from, to, FixedZoneOffset); err != nil {
		t.Fatalf("首次重算失败：%v", err)
	}
	if _, err := st.RefreshHourlyAggregates(ctx, "", from, to, FixedZoneOffset); err != nil {
		t.Fatalf("二次重算失败：%v", err)
	}

	var rows int
	if err := st.db.QueryRow("SELECT COUNT(*) FROM event_hourly_agg").Scan(&rows); err != nil {
		t.Fatalf("统计聚合表行数失败：%v", err)
	}
	if rows != 3 {
		t.Fatalf("重算两次后聚合表应仍是 3 行，实际 %d", rows)
	}

	aggs, err := st.HourlyAggregates(ctx, aggPointA, from, to)
	if err != nil {
		t.Fatalf("回读小时级聚合失败：%v", err)
	}
	if aggs[0].EventCount != 3 {
		t.Fatalf("重算后事件数应仍为 3，实际 %d", aggs[0].EventCount)
	}
}

// TestRefreshHourlyAggregates_SkipsArchivedBuckets 验证归档过的桶不再重算。
//
// 归档后原始采样已不在热表里；这时再重算，历史峰值会被「剩下的几条采样」
// 改写，趋势图上凭空多出一个下坠的尖峰——那比留着旧值危险得多。
func TestRefreshHourlyAggregates_SkipsArchivedBuckets(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	insertSamples(t, st, hourlyTestSamples())

	from := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 20, 5, 0, 0, 0, time.UTC)
	if _, err := st.RefreshHourlyAggregates(ctx, "", from, to, FixedZoneOffset); err != nil {
		t.Fatalf("首次重算失败：%v", err)
	}

	// 把点位 A 的第一个桶标成已归档，并把峰值改成别的值，
	// 模拟「原始采样已搬走、桶里只留着统计口径」。
	if _, err := st.db.Exec(
		"UPDATE event_hourly_agg SET archived = 1, peak_temperature_c = -99 "+
			"WHERE point_id = ? AND hour_start = ?",
		aggPointA, from.UTC().Format(tsLayout)); err != nil {
		t.Fatalf("标记归档状态失败：%v", err)
	}

	// 删掉这一桶的原始采样，再重算：桶已被标归档，不该被改写。
	if _, err := st.db.Exec(
		"DELETE FROM events WHERE point_id = ? AND measured_at >= ? AND measured_at < ?",
		aggPointA, from.UTC().Format(tsLayout), from.Add(time.Hour).UTC().Format(tsLayout)); err != nil {
		t.Fatalf("删除原始采样失败：%v", err)
	}
	if _, err := st.RefreshHourlyAggregates(ctx, "", from, to, FixedZoneOffset); err != nil {
		t.Fatalf("重算失败：%v", err)
	}

	aggs, err := st.HourlyAggregates(ctx, aggPointA, from, to)
	if err != nil {
		t.Fatalf("回读小时级聚合失败：%v", err)
	}
	if len(aggs) != 2 {
		t.Fatalf("点位 A 应仍有 2 个小时桶，实际 %d", len(aggs))
	}
	if aggs[0].PeakTemperatureC != -99 {
		t.Fatalf("已归档桶的峰值不应被重算改写，实际 %.2f℃", aggs[0].PeakTemperatureC)
	}
	if !aggs[0].Archived {
		t.Fatal("已归档桶应仍带归档标记")
	}
}

// TestArchiveEvents_MovesRowsOutOfHotTable 验证归档把过期采样搬出热表。
//
// 三件事一起断言：热表行数确实少了、归档文件行数与移出条数相等、
// 没到期的采样仍然留在热表里。归档最怕的是「删了但没存下来」或
// 「连没到期的也一起删了」，所以这三条必须同时成立。
func TestArchiveEvents_MovesRowsOutOfHotTable(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	insertSamples(t, st, hourlyTestSamples())

	dir := filepath.Join(t.TempDir(), "archive")
	// 截止时刻取 03:50：03:00 桶的 3 条早于它，04:00 桶的 2 条晚于它。
	before := time.Date(2026, 10, 20, 3, 50, 0, 0, time.UTC)

	infos, err := st.ArchiveEvents(ctx, aggPointA, before, dir, FixedZoneOffset)
	if err != nil {
		t.Fatalf("归档失败：%v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("点位 A 应产出 1 个归档文件，实际 %d", len(infos))
	}
	info := infos[0]
	if info.PointID != aggPointA {
		t.Fatalf("归档文件点位不符：%q", info.PointID)
	}
	if info.EventCount != 3 {
		t.Fatalf("归档条数应为 3（03:00 桶的 3 条早于截止时刻），实际 %d", info.EventCount)
	}

	// 热表：点位 A 只剩 04:00 桶的 2 条；点位 B 一条未动。
	remaining, err := st.CountEvents(ctx, aggPointA, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("统计热表采样失败：%v", err)
	}
	if remaining != 2 {
		t.Fatalf("热表应剩 2 条（未到期的），实际 %d", remaining)
	}
	other, err := st.CountEvents(ctx, aggPointB, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("统计其它点位采样失败：%v", err)
	}
	if other != 1 {
		t.Fatalf("未指定点位时只应归档点位 A，点位 B 应仍有 1 条，实际 %d", other)
	}

	// 归档文件行数与移出条数相等：这是「没丢数据」最直接的一条。
	lines := countArchiveLines(t, info.Path)
	if lines != info.EventCount {
		t.Fatalf("归档文件行数 %d 与移出热表条数 %d 对不上", lines, info.EventCount)
	}

	// 文件名带点位与时间区间：一年后有人把这个目录拷出来备查，
	// 只看到一串时间戳是认不出它属于哪个冷库、覆盖哪段时间的。
	if !strings.Contains(info.Path, aggPointA) {
		t.Fatalf("归档文件名应带点位标识，实际 %q", filepath.Base(info.Path))
	}
	if !strings.HasSuffix(info.Path, ".jsonl.gz") {
		t.Fatalf("归档文件应是 gzip JSONL，实际 %q", filepath.Base(info.Path))
	}
}

// countArchiveLines 数归档文件里的行数。
//
// 直接数压缩流里的行，而不是数解压后解析出的对象数：
// 「文件行数 == 移出热表的条数」这条对账必须落在字节上。
func countArchiveLines(t *testing.T, path string) int {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开归档文件失败：%v", err)
	}
	defer func() { _ = file.Close() }()
	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatalf("解压归档文件失败：%v", err)
	}
	defer func() { _ = reader.Close() }()

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lines := 0
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			lines++
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("读取归档文件失败：%v", err)
	}
	return lines
}

// TestArchiveEvents_ArchiveIsReadableBack 验证归档产物读得回来。
//
// 归档如果读不回来，等于把数据弄丢了：飞检要复盘「这一段为什么判成越温」时，
// 原始读数必须还在。断言三件事：条数对得上、读回来的采样原样（温度与点位）、
// 按点位与区间回读时过滤正确。
func TestArchiveEvents_ArchiveIsReadableBack(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	insertSamples(t, st, hourlyTestSamples())

	dir := filepath.Join(t.TempDir(), "archive")
	before := time.Date(2026, 10, 20, 3, 50, 0, 0, time.UTC)
	infos, err := st.ArchiveEvents(ctx, aggPointA, before, dir, FixedZoneOffset)
	if err != nil {
		t.Fatalf("归档失败：%v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("应有 1 个归档文件，实际 %d", len(infos))
	}

	archived, err := ReadArchiveFile(infos[0].Path)
	if err != nil {
		t.Fatalf("读回归档文件失败：%v", err)
	}
	if len(archived.Events) != 3 {
		t.Fatalf("读回的采样条数应为 3，实际 %d", len(archived.Events))
	}
	if archived.PointID != aggPointA {
		t.Fatalf("读回的点位不符：%q", archived.PointID)
	}
	for _, ev := range archived.Events {
		if ev.PointID != aggPointA {
			t.Fatalf("读回的采样点位不符：%q", ev.PointID)
		}
		if ev.TemperatureC != 4.0 && ev.TemperatureC != 9.5 && ev.TemperatureC != 6.0 {
			t.Fatalf("读回的采样温度被改写了：%.2f℃", ev.TemperatureC)
		}
	}
	// 按时间升序：回放重建窗口时靠的就是这个顺序。
	for i := 1; i < len(archived.Events); i++ {
		if archived.Events[i].MeasuredAt.Before(archived.Events[i-1].MeasuredAt) {
			t.Fatal("读回的采样应按时间升序")
		}
	}

	// 归档来源要能当取数来源用：这是离线回放读回归档的前提。
	source, err := NewArchiveSource(infos[0].Path)
	if err != nil {
		t.Fatalf("打开归档来源失败：%v", err)
	}
	from := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 20, 3, 15, 0, 0, time.UTC)
	got, err := source.EventsInRange(ctx, aggPointA, from, to)
	if err != nil {
		t.Fatalf("按区间回读归档采样失败：%v", err)
	}
	if len(got) != 2 {
		t.Fatalf("03:00~03:15 区间内应有 2 条采样，实际 %d", len(got))
	}
	// 换一个点位：归档里没有的采样不该被读出来。
	none, err := source.EventsInRange(ctx, aggPointB, from, to)
	if err != nil {
		t.Fatalf("按点位回读归档采样失败：%v", err)
	}
	if len(none) != 0 {
		t.Fatalf("归档里没有点位 B 的采样，回读却给出 %d 条", len(none))
	}
}

// TestArchiveEvents_MarksBucketsArchived 验证归档后相关小时桶被标成已归档。
//
// 不标的话，后面重算聚合会把历史峰值抹成「剩下几条采样」的峰值。
func TestArchiveEvents_MarksBucketsArchived(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	insertSamples(t, st, hourlyTestSamples())

	from := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 20, 5, 0, 0, 0, time.UTC)
	if _, err := st.RefreshHourlyAggregates(ctx, "", from, to, FixedZoneOffset); err != nil {
		t.Fatalf("重算小时级聚合失败：%v", err)
	}

	dir := filepath.Join(t.TempDir(), "archive")
	before := time.Date(2026, 10, 20, 3, 50, 0, 0, time.UTC)
	if _, err := st.ArchiveEvents(ctx, aggPointA, before, dir, FixedZoneOffset); err != nil {
		t.Fatalf("归档失败：%v", err)
	}

	aggs, err := st.HourlyAggregates(ctx, aggPointA, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("回读小时级聚合失败：%v", err)
	}
	if len(aggs) != 2 {
		t.Fatalf("归档后应仍能回读到 2 个小时桶的统计口径，实际 %d", len(aggs))
	}
	// 采样被搬走的那一桶标成已归档；采样还在热表里的那一桶不该被标——
	// 它后面还要重算，标了反而会让统计口径停住不动。
	for _, agg := range aggs {
		hour := agg.HourStart.UTC().Hour()
		if hour == 3 && !agg.Archived {
			t.Fatalf("桶 %s 的采样已归档，应被标成已归档", agg.HourStart.Format(time.RFC3339))
		}
		if hour == 4 && agg.Archived {
			t.Fatalf("桶 %s 的采样仍在热表，不应被标成已归档", agg.HourStart.Format(time.RFC3339))
		}
	}
}

// TestArchiveEvents_NothingToArchiveIsNotAnError 验证没有过期数据时不算失败。
//
// 「这段时间没有过期数据」是常态，不是错误——批处理调度里它应当是成功退出。
func TestArchiveEvents_NothingToArchiveIsNotAnError(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	insertSamples(t, st, hourlyTestSamples())

	dir := filepath.Join(t.TempDir(), "archive")
	before := time.Date(2026, 10, 20, 2, 0, 0, 0, time.UTC)
	infos, err := st.ArchiveEvents(ctx, "", before, dir, FixedZoneOffset)
	if err != nil {
		t.Fatalf("没有过期数据时不应报错：%v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("没有过期数据时不应产出归档文件，实际 %d", len(infos))
	}
	if countRows(t, st) != 6 {
		t.Fatalf("没有过期数据时热表应原样保留 6 条，实际 %d", countRows(t, st))
	}
}

// TestArchiveEvents_ArchiveFileIsOneObjectPerLine 验证归档文件一行一个采样对象。
//
// 不加表头、不加包装——这样「归档文件行数 == 移出热表的行数」可以直接核对，
// 复盘时也能按行读、按行找。
func TestArchiveEvents_ArchiveFileIsOneObjectPerLine(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	insertSamples(t, st, hourlyTestSamples())

	dir := filepath.Join(t.TempDir(), "archive")
	before := time.Date(2026, 10, 20, 3, 50, 0, 0, time.UTC)
	infos, err := st.ArchiveEvents(ctx, aggPointA, before, dir, FixedZoneOffset)
	if err != nil {
		t.Fatalf("归档失败：%v", err)
	}

	file, err := os.Open(infos[0].Path)
	if err != nil {
		t.Fatalf("打开归档文件失败：%v", err)
	}
	defer func() { _ = file.Close() }()
	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatalf("解压归档文件失败：%v", err)
	}
	defer func() { _ = reader.Close() }()

	scanner := bufio.NewScanner(reader)
	decoded := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev domain.TemperatureEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("归档行不是合法的采样 JSON：%v", err)
		}
		if ev.PointID != aggPointA {
			t.Fatalf("归档行的点位不符：%q", ev.PointID)
		}
		decoded++
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("读取归档文件失败：%v", err)
	}
	if decoded != 3 {
		t.Fatalf("归档文件应有 3 行采样，实际 %d", decoded)
	}
}
