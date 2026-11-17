// Package report 把判定结果汇总成能直接交出去的报表。
//
// 它管两件事：
//   - 周报：药学部冷链管理员每周导出一份「这一周哪个点位越了几次限、
//     最��到多少度、累计多少分钟、涉及哪些批次、结论是什么、谁确认的」。
//     一行一个点位——「越限次数」在一行一窗口的表里恒等于 1，没有意义；
//     按点位汇总，这一列才是这周这个点位到底出了几次事。
//   - 小时级聚合表：按点位与小时给出事件数、越温次数、峰值与平均温度，
//     供趋势查询与归档后的历史回看。
//
// 三条口径写死在代码里，不靠调用方自觉：
//   - 统计范围按越限窗口的起点算（window_start 落在区间内才计入）；
//   - 未确认的判定在处置结论一栏写「待确认」，不拿系统建议冒充人的结论；
//   - 缺项就是缺项：没查到批次就是没查到，不用空值冒充「没有批次」。
package report

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/disposition"
	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// Reader 是报表取数需要的读侧。
//
// 抽成接口是为了让报表层不直接依赖存储层：报表只回答「要哪几份数据」，
// 怎么拿是存储层的事。测试可以拿一份假数据喂进来，
// 不必为了测一行 CSV 去造一个真库。
type Reader interface {
	// RunsByPointInRange 回读区间内的判定 run；pointID 为空表示不限点位。
	RunsByPointInRange(ctx context.Context, pointID string, from, to time.Time) ([]domain.RunRecord, error)
	// RunPayloadsInRange 回读区间内所有 run 的指定分码产物（run_id → 信封 JSON）。
	RunPayloadsInRange(ctx context.Context, kind domain.StepPayloadKind, from, to time.Time) (map[string]json.RawMessage, error)
	// ConfirmationsInRange 回读区间内所有 run 的确认记录（run_id → 确认记录）。
	ConfirmationsInRange(ctx context.Context, from, to time.Time) (map[string]domain.Confirmation, error)
	// HourlyAggregates 回读区间内的小时级聚合行。
	HourlyAggregates(ctx context.Context, pointID string, from, to time.Time) ([]domain.HourlyAggregate, error)
}

// WeeklyRow 是一行点位周报。
type WeeklyRow struct {
	// PointID 是点位标识。
	PointID string `json:"point_id"`
	// DeviceName 是该点位登记的设备名（成稿里记的那个）；没有则留空。
	DeviceName string `json:"device_name,omitempty"`
	// ExcursionCount 是这周该点位的越限段数。
	ExcursionCount int `json:"excursion_count"`
	// PeakTemperatureC 是这周该点位出现过的最高温度（℃）。
	PeakTemperatureC float64 `json:"peak_temperature_c"`
	// DurationMinutes 是这周该点位越限的累计时长（分钟）。
	DurationMinutes int `json:"duration_minutes"`
	// Batches 是这周涉及的批次清单（去重后按出现顺序）。
	Batches []string `json:"batches,omitempty"`
	// Dispositions 是这周各段越限的处置结论（去重后按出现顺序）。
	Dispositions []string `json:"dispositions,omitempty"`
	// ConfirmedBy 是这周做过确认动作的人（去重后按出现顺序）。
	ConfirmedBy []string `json:"confirmed_by,omitempty"`
	// FirstExcursionAt 是这周第一段越限的起点。
	FirstExcursionAt time.Time `json:"first_excursion_at,omitempty"`
	// LastExcursionAt 是这周最后一段越限的起点。
	LastExcursionAt time.Time `json:"last_excursion_at,omitempty"`
	// PendingCount 是这周还没人确认的判定条数。
	PendingCount int `json:"pending_count"`
}

// 待确认的处置结论写法。
//
// 不用系统建议顶替：周报是给主管看的，看到「待确认」他才知道还有几段
// 压在经办手里；写成「待疫苗生产企业评估」会让人以为已经定了。
const pendingDispositionLabel = "待确认"

// rejectedDispositionLabel 是被主管否决的判定在周报上的写法。
const rejectedDispositionLabel = "已否决"

// BuildWeeklyReport 汇总区间内的判定，产出一行一个点位的周报。
//
// pointID 为空表示汇总全部点位。行按「越限次数降序、点位标识升序」排列：
// 出了事最多的点位排在最前面，管理员打开表格第一眼看到的就是要紧的那个。
func BuildWeeklyReport(ctx context.Context, reader Reader, from, to time.Time, pointID string) ([]WeeklyRow, error) {
	if reader == nil {
		return nil, errcode.Newf(errcode.Validation, "生成周报需要取数来源")
	}
	if from.IsZero() || to.IsZero() {
		return nil, errcode.Newf(errcode.Validation, "生成周报需要明确的起止区间")
	}
	if to.Before(from) {
		return nil, errcode.Newf(errcode.Validation,
			"生成周报的区间无效：起 %s 晚于止 %s",
			from.Format(time.RFC3339), to.Format(time.RFC3339))
	}

	runs, err := reader.RunsByPointInRange(ctx, pointID, from, to)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, nil
	}
	windows, err := reader.RunPayloadsInRange(ctx, domain.PayloadWindow, from, to)
	if err != nil {
		return nil, err
	}
	records, err := reader.RunPayloadsInRange(ctx, domain.PayloadExceptionRecord, from, to)
	if err != nil {
		return nil, err
	}
	confirmations, err := reader.ConfirmationsInRange(ctx, from, to)
	if err != nil {
		return nil, err
	}

	type pointAccumulator struct {
		row       WeeklyRow
		batchSeen map[string]bool
		dispSeen  map[string]bool
		confSeen  map[string]bool
	}
	accs := make(map[string]*pointAccumulator)

	for _, rec := range runs {
		point := rec.Run.PointID
		acc, ok := accs[point]
		if !ok {
			acc = &pointAccumulator{
				row:       WeeklyRow{PointID: point},
				batchSeen: make(map[string]bool),
				dispSeen:  make(map[string]bool),
				confSeen:  make(map[string]bool),
			}
			accs[point] = acc
		}

		acc.row.ExcursionCount++
		if rec.Run.WindowStart != nil {
			start := *rec.Run.WindowStart
			if acc.row.FirstExcursionAt.IsZero() || start.Before(acc.row.FirstExcursionAt) {
				acc.row.FirstExcursionAt = start
			}
			if start.After(acc.row.LastExcursionAt) {
				acc.row.LastExcursionAt = start
			}
		}

		// 窗口产物给峰值与时长：没有窗口产物就用 run 自身的窗口信息兜底，
		// 兜不住的字段留零值，不拿别的段的数来凑。
		if raw, ok := windows[rec.Run.RunID]; ok {
			payload, err := domain.DecodePayload(domain.PayloadWindow, raw)
			if err != nil {
				return nil, err
			}
			window, ok := payload.(*domain.WindowPayload)
			if !ok {
				return nil, errcode.Newf(errcode.Validation,
					"越限窗口产物形状不对：run %s", rec.Run.RunID)
			}
			if window.Window.PeakC > acc.row.PeakTemperatureC {
				acc.row.PeakTemperatureC = window.Window.PeakC
			}
			acc.row.DurationMinutes += window.Window.DurationMinutes
		}

		// 成稿产物给批次与设备名。
		if raw, ok := records[rec.Run.RunID]; ok {
			payload, err := domain.DecodePayload(domain.PayloadExceptionRecord, raw)
			if err != nil {
				return nil, err
			}
			record, ok := payload.(*domain.ExceptionRecordPayload)
			if !ok {
				return nil, errcode.Newf(errcode.Validation,
					"成稿产物形状不对：run %s", rec.Run.RunID)
			}
			if acc.row.DeviceName == "" {
				acc.row.DeviceName = record.DeviceName
			}
			for _, batch := range record.Batches {
				ref := strings.TrimSpace(batch.BatchRef)
				if ref == "" || acc.batchSeen[ref] {
					continue
				}
				acc.batchSeen[ref] = true
				acc.row.Batches = append(acc.row.Batches, ref)
			}
		}

		// 处置结论与确认人只认人的结论。
		confirmation, confirmed := confirmations[rec.Run.RunID]
		switch {
		case confirmed && confirmation.Action == string(workflow.ActionReject):
			// 否决也是主管做过的动作：这周它不再「待确认」，
			// 但它不是可用结论——周报上写「已否决」，让人一眼看出这段没放行。
			appendUnique(acc.dispSeen, &acc.row.Dispositions, rejectedDispositionLabel)
			appendUnique(acc.confSeen, &acc.row.ConfirmedBy, confirmation.ConfirmedBy)
		case confirmed && confirmation.Disposition != "":
			label := confirmation.DispositionLabel
			if label == "" {
				if resolved, ok := disposition.Label(confirmation.Disposition); ok {
					label = resolved
				} else {
					label = confirmation.Disposition
				}
			}
			appendUnique(acc.dispSeen, &acc.row.Dispositions, label)
			appendUnique(acc.confSeen, &acc.row.ConfirmedBy, confirmation.ConfirmedBy)
		default:
			acc.row.PendingCount++
			appendUnique(acc.dispSeen, &acc.row.Dispositions, pendingDispositionLabel)
		}
	}

	rows := make([]WeeklyRow, 0, len(accs))
	for _, acc := range accs {
		rows = append(rows, acc.row)
	}
	sortWeeklyRows(rows)
	return rows, nil
}

// appendUnique 往目标列表里追加一个还没出现过的值。
func appendUnique(seen map[string]bool, target *[]string, value string) {
	value = strings.TrimSpace(value)
	if value == "" || seen[value] {
		return
	}
	seen[value] = true
	*target = append(*target, value)
}

// sortWeeklyRows 按「越限次数降序、点位标识升序」排。
func sortWeeklyRows(rows []WeeklyRow) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0; j-- {
			left, right := rows[j-1], rows[j]
			if right.ExcursionCount > left.ExcursionCount ||
				(right.ExcursionCount == left.ExcursionCount && right.PointID < left.PointID) {
				rows[j-1], rows[j] = right, left
				continue
			}
			break
		}
	}
}

// WeeklyCSVHeader 是周报 CSV 的固定表头。
//
// 表头固定是刻意的：药事与信息科每周拿同一张表往台账里贴，
// 列名与列序变了，贴表的人就得改模板。中文表头让不熟系统的人也能直接读懂。
const WeeklyCSVHeader = "序号,点位标识,设备名称,越限次数,峰值温度(℃),累计持续(分钟),涉及批次,处置结论,确认人"

// HourlyCSVHeader 是小时级聚合 CSV 的固定表头。
const HourlyCSVHeader = "点位标识,小时起,事件数,越温次数,峰值温度(℃),平均温度(℃),最低温度(℃),原始采样是否已归档"

// WriteWeeklyCSV 把周报写成 CSV。
//
// 带 UTF-8 BOM：院内看表的人多半用 Excel 直接双击打开，
// 没有 BOM 时中文会显示成乱码，看表的人第一反应是「这表坏了」。
func WriteWeeklyCSV(w io.Writer, rows []WeeklyRow) error {
	writer := csv.NewWriter(w)
	if _, err := w.Write([]byte("\xef\xbb\xbf")); err != nil {
		return errcode.Newf(errcode.StorageError, "写入周报 CSV 编码标记失败：%v", err)
	}
	if err := writer.Write(strings.Split(WeeklyCSVHeader, ",")); err != nil {
		return errcode.Newf(errcode.StorageError, "写入周报 CSV 表头失败：%v", err)
	}
	for i, row := range rows {
		record := []string{
			strconv.Itoa(i + 1),
			row.PointID,
			row.DeviceName,
			strconv.Itoa(row.ExcursionCount),
			formatTemperature(row.PeakTemperatureC),
			strconv.Itoa(row.DurationMinutes),
			strings.Join(row.Batches, "、"),
			strings.Join(row.Dispositions, "、"),
			strings.Join(row.ConfirmedBy, "、"),
		}
		if err := writer.Write(record); err != nil {
			return errcode.Newf(errcode.StorageError, "写入周报 CSV 行失败：%v", err)
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return errcode.Newf(errcode.StorageError, "周报 CSV 写出失败：%v", err)
	}
	return nil
}

// WriteHourlyCSV 把小时级聚合成 CSV。
//
// 小时起点按传入的本地时区展示：库里按 UTC 存，报表给人看，
// 「凌晨三点那一小时」必须是本地三点整。
func WriteHourlyCSV(w io.Writer, aggs []domain.HourlyAggregate, loc *time.Location) error {
	if loc == nil {
		loc = time.UTC
	}
	writer := csv.NewWriter(w)
	if _, err := w.Write([]byte("\xef\xbb\xbf")); err != nil {
		return errcode.Newf(errcode.StorageError, "写入聚合 CSV 编码标记失败：%v", err)
	}
	if err := writer.Write(strings.Split(HourlyCSVHeader, ",")); err != nil {
		return errcode.Newf(errcode.StorageError, "写入聚合 CSV 表头失败：%v", err)
	}
	for _, agg := range aggs {
		archived := "否"
		if agg.Archived {
			archived = "是"
		}
		// 这一小时热表里没有原始采样时，温度三列留空，不写 0.00。
		//
		// 0.00℃ 是一个读数；「这一小时没有读数」不是读数。
		// 冷库峰值写成 0.00℃，看表的人会以为柜子冷到零度，
		// 或者以为设备停摆——两件事都不是事实。
		peak, avg, min := "", "", ""
		if agg.EventCount > 0 {
			peak = formatTemperature(agg.PeakTemperatureC)
			avg = formatTemperature(agg.AvgTemperatureC)
			min = formatTemperature(agg.MinTemperatureC)
		}
		record := []string{
			agg.PointID,
			agg.HourStart.In(loc).Format("2006-01-02 15:00"),
			strconv.Itoa(agg.EventCount),
			strconv.Itoa(agg.ExcursionCount),
			peak,
			avg,
			min,
			archived,
		}
		if err := writer.Write(record); err != nil {
			return errcode.Newf(errcode.StorageError, "写入聚合 CSV 行失败：%v", err)
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return errcode.Newf(errcode.StorageError, "聚合 CSV 写出失败：%v", err)
	}
	return nil
}

// formatTemperature 把温度格式化成两位小数。
//
// 固定两位：温控台账上的温度列要能直接对齐比较，
// 一行 12.4℃ 下一行 12.4000001℃ 是同一个温度，读表的人却要多看一眼。
func formatTemperature(v float64) string {
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// Summary 是给命令行用的报表摘要。
type Summary struct {
	// RangeFrom 与 RangeTo 是统计区间。
	RangeFrom time.Time `json:"range_from"`
	RangeTo   time.Time `json:"range_to"`
	// RowCount 是报表行数（周报按点位计，聚合按小时桶计）。
	RowCount int `json:"row_count"`
	// ExcursionCount 是区间内的越限段数合计。
	ExcursionCount int `json:"excursion_count"`
	// PendingCount 是区间内还没人确认的判定条数。
	PendingCount int `json:"pending_count"`
}

// SummarizeWeekly 给周报算一句摘要。
func SummarizeWeekly(rows []WeeklyRow, from, to time.Time) Summary {
	summary := Summary{RangeFrom: from, RangeTo: to, RowCount: len(rows)}
	for _, row := range rows {
		summary.ExcursionCount += row.ExcursionCount
		summary.PendingCount += row.PendingCount
	}
	return summary
}

// String 把摘要写成人话。
func (s Summary) String() string {
	return fmt.Sprintf("区间 %s ~ %s，共 %d 个点位，越限 %d 次，其中 %d 次尚未确认",
		s.RangeFrom.Format(time.RFC3339),
		s.RangeTo.Format(time.RFC3339),
		s.RowCount, s.ExcursionCount, s.PendingCount)
}
