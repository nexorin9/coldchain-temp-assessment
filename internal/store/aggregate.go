package store

// 本文件落「小时级聚合表」：event_hourly_agg。
//
// 为什么要有这张表：多点位 5 分钟粒度持续上报，一天就能攒下几万条原始采样。
// 原始采样全量留在热表上，一是周报与趋势查询每次都要扫全表，
// 二是库文件随天数无限长，院内小主机放不下。
// 按小时聚合之后，报表与趋势看聚合行，原始采样按保留期搬进 gzip JSONL 归档。
//
// 四条口径约定：
//   - 归桶按本地整点：小时桶以院内本地时间切分（默认 UTC+8，可配置），
//     否则「凌晨三点那一小时」在报表上会横跨两个桶，人看不懂。
//   - 越温次数按窗口起点归桶：跨小时的同一段越温只算在它开始的那一小时。
//     各小时的越温次数加起来因此等于实际段数，周报上的总数对得上。
//   - 归档过的桶不再重算：归档桶重算会把历史峰值抹成剩余采样的峰值。
//   - 不静默改数：桶里没有采样就不写行，也不把上一轮的行删成空桶。

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// hourlyAggSchemaSQL 是小时级聚合表的建表脚本。
//
// 全部 IF NOT EXISTS：重复执行即幂等，服务每次启动跑一遍即可。
const hourlyAggSchemaSQL = `
CREATE TABLE IF NOT EXISTS event_hourly_agg (
    point_id          TEXT NOT NULL,
    hour_start        TEXT NOT NULL,
    event_count       INTEGER NOT NULL DEFAULT 0,
    excursion_count   INTEGER NOT NULL DEFAULT 0,
    peak_temperature_c REAL NOT NULL DEFAULT 0,
    avg_temperature_c REAL NOT NULL DEFAULT 0,
    min_temperature_c REAL NOT NULL DEFAULT 0,
    archived          INTEGER NOT NULL DEFAULT 0,
    updated_at        TEXT NOT NULL,
    PRIMARY KEY (point_id, hour_start)
);

CREATE INDEX IF NOT EXISTS idx_hourly_agg_point_hour ON event_hourly_agg (point_id, hour_start);
`

// upsertHourlyAggSQL 是聚合行的写入语句。
//
// ON CONFLICT DO UPDATE：同一桶重算时更新那一行，而不是插出第二行。
// 同一小时被重算多次是常态（补投采样后再刷一次），
// 插出两行会让「这一小时有几条采样」凭空翻倍。
const upsertHourlyAggSQL = `
INSERT INTO event_hourly_agg (
    point_id, hour_start, event_count, excursion_count,
    peak_temperature_c, avg_temperature_c, min_temperature_c,
    archived, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(point_id, hour_start) DO UPDATE SET
    event_count       = excluded.event_count,
    excursion_count   = excluded.excursion_count,
    peak_temperature_c = excluded.peak_temperature_c,
    avg_temperature_c  = excluded.avg_temperature_c,
    min_temperature_c  = excluded.min_temperature_c,
    updated_at        = excluded.updated_at
`

// selectHourlyAggSQL 是聚合行的回读语句骨架。
const selectHourlyAggSQL = `
SELECT point_id, hour_start, event_count, excursion_count,
       peak_temperature_c, avg_temperature_c, min_temperature_c,
       archived, updated_at
FROM event_hourly_agg
`

// prepareHourlyAggregates 建 event_hourly_agg 表（幂等）。
func (s *Store) prepareHourlyAggregates(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, hourlyAggSchemaSQL); err != nil {
		return errcode.Newf(errcode.StorageError, "建 event_hourly_agg 表失败：%v", err)
	}
	return nil
}

// FixedZoneOffset 是院内本地时区偏移的默认值（分钟）。
//
// 480 = UTC+8：中国标准时间。冷链点位在院内，跑的是院内本地时间，
// 「凌晨三点那一小时」必须是本地三点整到四点整，不是 UTC 的那一小时。
const FixedZoneOffset = 480

// bucketLocation 把时区偏移分钟数换成本地时区。
//
// 只接受整小时的偏移：小时桶按整点切分，半小时偏移会让「本地整点」
// 与 UTC 整点错开，归桶边界说不清楚，索性拒绝而不是猜一个。
func bucketLocation(tzOffsetMinutes int) (*time.Location, error) {
	if tzOffsetMinutes%60 != 0 {
		return nil, errcode.Newf(errcode.Validation,
			"时区偏移必须是整小时（分钟数能被 60 整除），收到 %d", tzOffsetMinutes)
	}
	return time.FixedZone("冷链本地时区", tzOffsetMinutes*60), nil
}

// hourBucket 把一个时刻归到它所在的小时桶起点。
func hourBucket(t time.Time, loc *time.Location) time.Time {
	return t.In(loc).Truncate(time.Hour)
}

// RefreshHourlyAggregates 重算区间内的小时级聚合并落库。
//
// 返回实际写入的桶数。它按点位分别重算：pointID 为空表示重算全部点位。
//
// 口径：
//   - 事件数、峰值、平均、最低来自 events 表的原始采样；
//   - 越温次数来自 runs 表里越限窗口的起点（不是越限采样条数）；
//   - 已经归档过的桶（archived=1）跳过不动——原始采样不在热表里了，
//     重算只会把历史峰值抹掉。
//
// 区间两端闭合，与其它回读接口保持同一口径。
func (s *Store) RefreshHourlyAggregates(ctx context.Context, pointID string, from, to time.Time, tzOffsetMinutes int) (int, error) {
	loc, err := bucketLocation(tzOffsetMinutes)
	if err != nil {
		return 0, err
	}
	if from.IsZero() || to.IsZero() {
		return 0, errcode.Newf(errcode.Validation, "重算小时级聚合需要明确的起止区间")
	}
	if to.Before(from) {
		return 0, errcode.Newf(errcode.Validation,
			"重算小时级聚合的区间无效：起 %s 晚于止 %s",
			from.Format(time.RFC3339), to.Format(time.RFC3339))
	}

	events, err := s.EventsInRange(ctx, pointID, from, to)
	if err != nil {
		return 0, err
	}
	starts, err := s.excursionStartsInRange(ctx, pointID, from, to)
	if err != nil {
		return 0, err
	}

	type bucket struct {
		count      int
		excursions int
		peak       float64
		min        float64
		sum        float64
		hasSample  bool
	}
	buckets := make(map[string]*bucket)

	for _, ev := range events {
		key := ev.PointID + "|" + hourBucket(ev.MeasuredAt, loc).UTC().Format(tsLayout)
		b, ok := buckets[key]
		if !ok {
			b = &bucket{peak: ev.TemperatureC, min: ev.TemperatureC}
			buckets[key] = b
		}
		b.count++
		b.sum += ev.TemperatureC
		if ev.TemperatureC > b.peak {
			b.peak = ev.TemperatureC
		}
		if ev.TemperatureC < b.min {
			b.min = ev.TemperatureC
		}
		b.hasSample = true
	}

	for _, start := range starts {
		key := start.PointID + "|" + hourBucket(start.At, loc).UTC().Format(tsLayout)
		b, ok := buckets[key]
		if !ok {
			// 越温窗口所在的这一小时可能没有任何原始采样落库
			//（例如补投了结论但没补采样）。这时也要留一行，
			// 否则「这一小时越温过一次」这件事在趋势图上就凭空消失了。
			b = &bucket{}
			buckets[key] = b
		}
		b.excursions++
	}

	written := 0
	now := time.Now().UTC().Format(tsLayout)
	for key, b := range buckets {
		parts := strings.SplitN(key, "|", 2)
		if len(parts) != 2 {
			continue
		}
		bucketPoint, bucketStart := parts[0], parts[1]

		archived, err := s.hourlyBucketArchived(ctx, bucketPoint, bucketStart)
		if err != nil {
			return written, err
		}
		if archived {
			continue
		}

		avg := 0.0
		if b.count > 0 {
			avg = b.sum / float64(b.count)
		}
		if _, err := s.db.ExecContext(ctx, upsertHourlyAggSQL,
			bucketPoint, bucketStart, b.count, b.excursions,
			b.peak, avg, b.min, 0, now,
		); err != nil {
			return written, errcode.Newf(errcode.StorageError, "写入小时级聚合失败：%v", err)
		}
		written++
	}
	return written, nil
}

// excursionStart 是一条越限窗口的起点。
type excursionStart struct {
	PointID string
	At      time.Time
}

// excursionStartsInRange 回读区间内越限窗口的起点。
//
// 取的是 runs 表里的 window_start：那是判定流程认定的「这段越温从什么时候开始」，
// 比「这一小时有几条采样越了限」更贴近周报要说的「越限次数」——
// 一段持续三小时的越温是 1 次，不是几十次。
func (s *Store) excursionStartsInRange(ctx context.Context, pointID string, from, to time.Time) ([]excursionStart, error) {
	query := "SELECT point_id, window_start FROM runs WHERE window_start IS NOT NULL AND window_start <> ''"
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
	query += " ORDER BY window_start ASC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, errcode.Newf(errcode.StorageError, "回读越限窗口起点失败：%v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []excursionStart
	for rows.Next() {
		var (
			point     string
			startText string
		)
		if err := rows.Scan(&point, &startText); err != nil {
			return nil, errcode.Newf(errcode.StorageError, "读取越限窗口起点失败：%v", err)
		}
		at, err := time.Parse(tsLayout, startText)
		if err != nil {
			return nil, errcode.Newf(errcode.StorageError,
				"越限窗口起点时间无法解析（%q）：%v", startText, err)
		}
		out = append(out, excursionStart{PointID: point, At: at})
	}
	if err := rows.Err(); err != nil {
		return nil, errcode.Newf(errcode.StorageError, "回读越限窗口起点失败：%v", err)
	}
	return out, nil
}

// hourlyBucketArchived 判断某个小时桶是否已归档。
func (s *Store) hourlyBucketArchived(ctx context.Context, pointID, hourStart string) (bool, error) {
	var archived int
	err := s.db.QueryRowContext(ctx,
		"SELECT archived FROM event_hourly_agg WHERE point_id = ? AND hour_start = ?",
		pointID, hourStart).Scan(&archived)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, errcode.Newf(errcode.StorageError, "读取小时级聚合归档标记失败：%v", err)
	}
	return archived != 0, nil
}

// MarkHourlyBucketsArchived 把给定的小时桶标记为已归档。
//
// 归档原始采样时调用：这些桶的统计值就此定格，之后不再用剩余采样重算。
func (s *Store) MarkHourlyBucketsArchived(ctx context.Context, buckets []domain.HourlyAggregate) (int, error) {
	if len(buckets) == 0 {
		return 0, nil
	}
	now := time.Now().UTC().Format(tsLayout)
	marked := 0
	for _, bucket := range buckets {
		result, err := s.db.ExecContext(ctx,
			"UPDATE event_hourly_agg SET archived = 1, updated_at = ? WHERE point_id = ? AND hour_start = ?",
			now, bucket.PointID, bucket.HourStart.UTC().Format(tsLayout))
		if err != nil {
			return marked, errcode.Newf(errcode.StorageError, "标记小时级聚合归档状态失败：%v", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return marked, errcode.Newf(errcode.StorageError, "读取标记结果失败：%v", err)
		}
		marked += int(affected)
	}
	return marked, nil
}

// HourlyAggregates 回读区间内的小时级聚合行。
//
// pointID 为空表示不限点位；区间两端闭合。行按小时起点升序返回。
func (s *Store) HourlyAggregates(ctx context.Context, pointID string, from, to time.Time) ([]domain.HourlyAggregate, error) {
	query := selectHourlyAggSQL + "WHERE 1 = 1"
	args := make([]any, 0, 3)
	if trimmed := strings.TrimSpace(pointID); trimmed != "" {
		query += " AND point_id = ?"
		args = append(args, trimmed)
	}
	if !from.IsZero() {
		query += " AND hour_start >= ?"
		args = append(args, from.UTC().Format(tsLayout))
	}
	if !to.IsZero() {
		query += " AND hour_start <= ?"
		args = append(args, to.UTC().Format(tsLayout))
	}
	query += " ORDER BY point_id ASC, hour_start ASC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, errcode.Newf(errcode.StorageError, "回读小时级聚合失败：%v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []domain.HourlyAggregate
	for rows.Next() {
		var (
			agg       domain.HourlyAggregate
			hourStart string
			archived  int
			updatedAt string
		)
		if err := rows.Scan(
			&agg.PointID, &hourStart, &agg.EventCount, &agg.ExcursionCount,
			&agg.PeakTemperatureC, &agg.AvgTemperatureC, &agg.MinTemperatureC,
			&archived, &updatedAt,
		); err != nil {
			return nil, errcode.Newf(errcode.StorageError, "读取小时级聚合失败：%v", err)
		}
		parsed, err := time.Parse(tsLayout, hourStart)
		if err != nil {
			return nil, errcode.Newf(errcode.StorageError,
				"小时级聚合起点时间无法解析（%q）：%v", hourStart, err)
		}
		agg.HourStart = parsed
		agg.Archived = archived != 0
		agg.UpdatedAt, _ = time.Parse(tsLayout, updatedAt)
		out = append(out, agg)
	}
	if err := rows.Err(); err != nil {
		return nil, errcode.Newf(errcode.StorageError, "回读小时级聚合失败：%v", err)
	}
	return out, nil
}
