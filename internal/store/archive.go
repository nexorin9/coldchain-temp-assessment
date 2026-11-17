package store

// 本文件落「冷数据归档」：把过期的原始温度采样搬出热表，写成 gzip JSONL。
//
// 为什么要有这一步：多点位 5 分钟粒度持续上报，原始采样一天就几万条，
// 全量留在热表上，库文件很快撑爆院内那台小主机，查询也被拖慢。
// 冷链管理的规矩是「原始记录要留、但不必一直躺在热库上」——
// 原始采样搬进 gzip JSONL 归档文件，热表只留最近一段，小时级聚合表留住统计口径，
// 需要复盘时再从归档文件读回来重跑一遍判定。
//
// 三条关键约定：
//   - 先落盘、后删表：归档文件写不成功就绝不删热表里的行。
//     顺序反过来的话，删完表才发现文件没写成，这段原始读数就没了。
//   - 一行一条采样：gzip JSONL 每行是一个完整的采样对象，
//     不加表头、不加包装——这样「归档文件行数 == 移出热表的行数」是可以直接核对的。
//   - 归档可回读：归档产物就是一个 SampleSource，离线回放能直接从它读回采样重跑判定。

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// ArchiveInfo 是一次归档的结果。
type ArchiveInfo struct {
	// Path 是归档文件路径。
	Path string `json:"path"`
	// PointID 是这批采样所属的点位。
	PointID string `json:"point_id"`
	// From 与 To 是这批采样的实际时间范围（闭区间）。
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// EventCount 是写进归档文件的采样条数。
	EventCount int `json:"event_count"`
	// Bytes 是归档文件落盘后的字节数。
	Bytes int64 `json:"bytes"`
	// ArchivedAt 是归档动作发生的时刻。
	ArchivedAt time.Time `json:"archived_at"`
}

// ArchivedEvents 是从归档文件读回的一批采样。
type ArchivedEvents struct {
	// Path 是归档文件路径。
	Path string `json:"path"`
	// PointID 是这批采样所属的点位。
	PointID string `json:"point_id"`
	// From 与 To 是这批采样的时间范围（闭区间）。
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Events 是读回的采样，按时间升序。
	Events []domain.TemperatureEvent `json:"events"`
}

// archiveFileName 给归档文件起名：点位 + 时间区间。
//
// 文件名要能自己说明这是什么：一年后有人把这个目录拷出来做飞检备查，
// 只看到一串时间戳是认不出它属于哪个冷库、覆盖哪段时间的。
func archiveFileName(pointID string, from, to time.Time) string {
	safePoint := sanitizeArchiveToken(pointID)
	return fmt.Sprintf("coldchain-events-%s-%s-%s.jsonl.gz",
		safePoint,
		from.UTC().Format("20060102T150405Z"),
		to.UTC().Format("20060102T150405Z"))
}

// sanitizeArchiveToken 把点位标识清洗成能安全进文件名的字符。
func sanitizeArchiveToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	if out == "" {
		return "point"
	}
	return out
}

// ArchiveEvents 把早于 before 的原始采样归档，并从热表清理。
//
// pointID 为空表示归档全部点位（按点位分别成文件）。
// dir 是归档目录，不存在会自动创建。
//
// 返回每个点位一份归档结果；没有可归档的采样时返回空切片，不报错——
// 「这段时间没有过期数据」是常态，不是失败。
//
// 顺序固定为：读热表 → 写归档文件 → 删热表 → 标记小时桶已归档。
// 文件写不成就停在第一步，热表原封不动；删表失败时归档文件仍在，
// 数据不丢（重复归档时按幂等键去重即可）。
func (s *Store) ArchiveEvents(ctx context.Context, pointID string, before time.Time, dir string, tzOffsetMinutes int) ([]ArchiveInfo, error) {
	if before.IsZero() {
		return nil, errcode.Newf(errcode.Validation, "归档需要明确的截止时刻（早于该时刻的采样被归档）")
	}
	if strings.TrimSpace(dir) == "" {
		return nil, errcode.Newf(errcode.Validation, "归档需要指定归档目录")
	}
	loc, err := bucketLocation(tzOffsetMinutes)
	if err != nil {
		return nil, err
	}

	events, err := s.EventsInRange(ctx, pointID, time.Time{}, before)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, nil
	}

	// 按点位分组：不同点位的采样写进各自的归档文件，
	// 文件名里的点位与时间区间才对得上，回放时也能按点位取。
	byPoint := make(map[string][]domain.TemperatureEvent)
	for _, ev := range events {
		byPoint[ev.PointID] = append(byPoint[ev.PointID], ev)
	}
	points := make([]string, 0, len(byPoint))
	for p := range byPoint {
		points = append(points, p)
	}
	sort.Strings(points)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, errcode.Newf(errcode.StorageError, "创建归档目录失败：%s", dir)
	}

	var infos []ArchiveInfo
	for _, point := range points {
		group := byPoint[point]
		sort.Slice(group, func(i, j int) bool {
			return group[i].MeasuredAt.Before(group[j].MeasuredAt)
		})
		from := group[0].MeasuredAt
		to := group[len(group)-1].MeasuredAt

		name := archiveFileName(point, from, to)
		path := filepath.Join(dir, name)
		size, err := writeArchiveFile(path, group)
		if err != nil {
			return infos, err
		}

		removed, err := s.deleteEventsByID(ctx, group)
		if err != nil {
			return infos, err
		}
		if removed != len(group) {
			return infos, errcode.Newf(errcode.StorageError,
				"归档后热表清理条数对不上：归档文件写入 %d 条，热表只清掉 %d 条（归档文件 %s 仍保有全部数据）",
				len(group), removed, path)
		}

		// 采样出热表后，这些小时桶的统计值就此定格：后面重算聚合时跳过它们，
		// 免得历史峰值被剩下的几条采样改写。
		buckets := make([]domain.HourlyAggregate, 0, len(group))
		seen := make(map[string]bool)
		for _, ev := range group {
			key := ev.PointID + "|" + hourBucket(ev.MeasuredAt, loc).UTC().Format(tsLayout)
			if seen[key] {
				continue
			}
			seen[key] = true
			buckets = append(buckets, domain.HourlyAggregate{
				PointID:   ev.PointID,
				HourStart: hourBucket(ev.MeasuredAt, loc),
			})
		}
		if _, err := s.MarkHourlyBucketsArchived(ctx, buckets); err != nil {
			return infos, err
		}

		infos = append(infos, ArchiveInfo{
			Path:       path,
			PointID:    point,
			From:       from,
			To:         to,
			EventCount: len(group),
			Bytes:      size,
			ArchivedAt: time.Now(),
		})
	}
	return infos, nil
}

// writeArchiveFile 把采样写成 gzip JSONL 并落盘。
//
// 先写临时文件再改名：改名前文件是完整的，改名是原子的。
// 中途断电不会留下半截的归档文件被当成完整归档读回来。
func writeArchiveFile(path string, events []domain.TemperatureEvent) (int64, error) {
	tmp := path + ".tmp"
	file, err := os.Create(tmp)
	if err != nil {
		return 0, errcode.Newf(errcode.StorageError, "创建归档临时文件失败：%s", err)
	}
	writer := gzip.NewWriter(file)
	encoder := json.NewEncoder(writer)
	for _, ev := range events {
		if err := encoder.Encode(ev); err != nil {
			_ = writer.Close()
			_ = file.Close()
			_ = os.Remove(tmp)
			return 0, errcode.Newf(errcode.StorageError, "写入归档文件失败：%v", err)
		}
	}
	if err := writer.Close(); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return 0, errcode.Newf(errcode.StorageError, "压缩归档文件失败：%v", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return 0, errcode.Newf(errcode.StorageError, "落盘归档文件失败：%v", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return 0, errcode.Newf(errcode.StorageError, "关闭归档文件失败：%v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return 0, errcode.Newf(errcode.StorageError, "归档文件改名失败：%v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, errcode.Newf(errcode.StorageError, "读取归档文件信息失败：%v", err)
	}
	return info.Size(), nil
}

// deleteEventsByID 从热表里删掉指定的事件，返回实际删除的行数。
func (s *Store) deleteEventsByID(ctx context.Context, events []domain.TemperatureEvent) (int, error) {
	if len(events) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, errcode.Newf(errcode.StorageError, "开启归档清理事务失败：%v", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, "DELETE FROM events WHERE event_id = ?")
	if err != nil {
		return 0, errcode.Newf(errcode.StorageError, "准备归档清理语句失败：%v", err)
	}
	defer func() { _ = stmt.Close() }()

	removed := 0
	for _, ev := range events {
		result, err := stmt.ExecContext(ctx, ev.EventID)
		if err != nil {
			return removed, errcode.Newf(errcode.StorageError, "清理热表采样失败：%v", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return removed, errcode.Newf(errcode.StorageError, "读取清理结果失败：%v", err)
		}
		removed += int(affected)
	}
	if err := tx.Commit(); err != nil {
		return removed, errcode.Newf(errcode.StorageError, "提交归档清理事务失败：%v", err)
	}
	return removed, nil
}

// ReadArchiveFile 读回一个归档文件。
//
// 读回来的采样按时间升序；每行一个采样对象，行数即采样条数。
func ReadArchiveFile(path string) (*ArchivedEvents, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errcode.Newf(errcode.Validation, "读回归档文件需要指定路径")
	}
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errcode.Newf(errcode.NotFound, "归档文件不存在：%s", path)
		}
		return nil, errcode.Newf(errcode.StorageError, "打开归档文件失败：%s", path)
	}
	defer func() { _ = file.Close() }()

	reader, err := gzip.NewReader(file)
	if err != nil {
		return nil, errcode.Newf(errcode.StorageError, "解压归档文件失败（%s）：%v", path, err)
	}
	defer func() { _ = reader.Close() }()

	decoder := json.NewDecoder(reader)
	out := &ArchivedEvents{Path: path}
	for {
		var ev domain.TemperatureEvent
		if err := decoder.Decode(&ev); err != nil {
			if err == io.EOF {
				break
			}
			return nil, errcode.Newf(errcode.StorageError, "解析归档文件失败（%s）：%v", path, err)
		}
		out.Events = append(out.Events, ev)
	}
	if len(out.Events) == 0 {
		return nil, errcode.Newf(errcode.Validation, "归档文件里没有任何采样：%s", path)
	}
	sort.Slice(out.Events, func(i, j int) bool {
		return out.Events[i].MeasuredAt.Before(out.Events[j].MeasuredAt)
	})
	out.PointID = out.Events[0].PointID
	out.From = out.Events[0].MeasuredAt
	out.To = out.Events[len(out.Events)-1].MeasuredAt
	return out, nil
}

// ArchiveSource 是从归档文件读采样的来源。
//
// 它满足离线回放要的 SampleSource 形状（按点位与时间区间回读采样），
// 所以原始采样搬出热表之后，回放仍然能从归档文件里把判定重跑一遍——
// 「归档之后还能不能复现当时的结论」，是归档这件事能不能上线的关键一问。
type ArchiveSource struct {
	files []*ArchivedEvents
}

// NewArchiveSource 按给定路径打开归档文件。
func NewArchiveSource(paths ...string) (*ArchiveSource, error) {
	source := &ArchiveSource{}
	for _, path := range paths {
		archived, err := ReadArchiveFile(path)
		if err != nil {
			return nil, err
		}
		source.files = append(source.files, archived)
	}
	if len(source.files) == 0 {
		return nil, errcode.Newf(errcode.Validation, "归档回放至少需要指定一个归档文件")
	}
	return source, nil
}

// Files 返回已打开的归档文件。
func (a *ArchiveSource) Files() []*ArchivedEvents {
	if a == nil {
		return nil
	}
	return a.files
}

// EventsInRange 按点位与时间区间回读归档里的采样，区间两端闭合。
//
// pointID 为空表示不限点位。返回结果按采样时刻升序。
func (a *ArchiveSource) EventsInRange(ctx context.Context, pointID string, from, to time.Time) ([]domain.TemperatureEvent, error) {
	if a == nil {
		return nil, errcode.Newf(errcode.Validation, "归档来源未初始化")
	}
	trimmed := strings.TrimSpace(pointID)
	var out []domain.TemperatureEvent
	for _, file := range a.files {
		for _, ev := range file.Events {
			if trimmed != "" && ev.PointID != trimmed {
				continue
			}
			if !from.IsZero() && ev.MeasuredAt.Before(from) {
				continue
			}
			if !to.IsZero() && ev.MeasuredAt.After(to) {
				continue
			}
			out = append(out, ev)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MeasuredAt.Equal(out[j].MeasuredAt) {
			return out[i].EventID < out[j].EventID
		}
		return out[i].MeasuredAt.Before(out[j].MeasuredAt)
	})
	return out, nil
}
