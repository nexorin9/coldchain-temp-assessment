// 本文件落「从库里取运行摘要指标」这一段。
//
// 它是汇总器与存储层之间唯一的接头：汇总器只认 Source 接口，
// 不知道库是 SQLite 还是别的；这一层把库的计数口径翻译成 Metrics。
//
// 取数口径的分工（为什么不都在这一层做 GROUP BY）：
//
//   - 计数走索引范围扫描。events 按 measured_at 计数、runs 按 created_at
//     与 exec_state 计数、审计表按 entry_type + created_at 计数——
//     三张表都有对应索引，命中的是范围扫描，不读表行。
//   - 逐小时分解走小时级聚合表。event_hourly_agg 里已经是按小时归好桶的
//     统计，直接读它；对着原始采样做 GROUP BY 等于把几十天前攒下的
//     采样再扫一遍，摘要端点每分钟被轮询一次，那样拖垮主路径。
//
// 汇总器是只读的：这一层不推进任何业务状态，也不发任何出站请求。
package observe

import (
	"context"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/store"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// StoreSource 是从 SQLite 库取运行摘要指标的实现。
type StoreSource struct {
	// store 是本地库。
	store *store.Store
}

// NewStoreSource 创建库取数源。st 为 nil 时取数必然失败，不静默回零。
func NewStoreSource(st *store.Store) *StoreSource {
	return &StoreSource{store: st}
}

// Collect 取区间内的运行摘要原始计数。
//
// 区间两端闭合。所有窗口内指标共用这一段区间；「此刻还欠着的」与
// 「此刻在动的」是当前存量，按当前状态数，不按区间数。
func (s *StoreSource) Collect(ctx context.Context, from, to time.Time) (Metrics, error) {
	if s == nil || s.store == nil {
		return Metrics{}, errSourceNotReady()
	}

	var (
		metrics Metrics
		err     error
	)
	if metrics.Events, err = s.store.CountEventsInRange(ctx, from, to); err != nil {
		return Metrics{}, err
	}
	if metrics.Runs, err = s.store.CountRunsCreatedInRange(ctx, from, to); err != nil {
		return Metrics{}, err
	}
	if metrics.FailedRuns, err = s.store.CountRunsFailedInRange(ctx, from, to); err != nil {
		return Metrics{}, err
	}
	if metrics.DegradedRuns, err = s.store.CountRunsDegradedInRange(ctx, from, to); err != nil {
		return Metrics{}, err
	}
	if metrics.AwaitingConfirmation, err = s.store.CountRunsInState(ctx,
		string(workflow.StateAwaitingConfirmation)); err != nil {
		return Metrics{}, err
	}
	if metrics.RunningRuns, err = s.store.CountRunsInExecState(ctx,
		string(domain.ExecStateRunning)); err != nil {
		return Metrics{}, err
	}
	if metrics.QueuedRuns, err = s.store.CountRunsInExecState(ctx,
		string(domain.ExecStateQueued)); err != nil {
		return Metrics{}, err
	}
	if metrics.RetryingRuns, err = s.store.CountRunsInExecState(ctx,
		string(domain.ExecStateRetrying)); err != nil {
		return Metrics{}, err
	}
	if metrics.PendingWritebacks, err = s.store.CountPendingWritebacks(ctx); err != nil {
		return Metrics{}, err
	}
	if metrics.OpenWritebackAlerts, err = s.store.CountOpenWritebackAlerts(ctx); err != nil {
		return Metrics{}, err
	}

	modelMetrics, err := s.store.ModelCallMetricsInRange(ctx, from, to)
	if err != nil {
		return Metrics{}, err
	}
	metrics.ModelCalls = modelMetrics.Total
	metrics.DegradedModelCalls = modelMetrics.Degraded
	metrics.FailedModelCalls = modelMetrics.Failed

	aggregates, err := s.store.HourlyAggregates(ctx, "", from, to)
	if err != nil {
		return Metrics{}, err
	}
	metrics.Hourly = mergeHourly(aggregates)
	return metrics, nil
}

// mergeHourly 把逐点位的聚合行合并成逐小时的桶。
//
// 合并口径：事件数与越温次数相加，峰值取最高，点位数按出现过该小时的
// 点位去重计。合并完按小时起点升序排好——
// 顺序不依赖 SQL 的返回顺序，看摘要的人不用自己排。
func mergeHourly(aggregates []domain.HourlyAggregate) []HourlyBucket {
	type acc struct {
		eventCount     int
		excursionCount int
		peak           float64
		points         map[string]bool
	}
	byHour := make(map[string]*acc, len(aggregates))
	for _, agg := range aggregates {
		key := agg.HourStart.UTC().Format(time.RFC3339)
		item, ok := byHour[key]
		if !ok {
			item = &acc{peak: agg.PeakTemperatureC, points: map[string]bool{}}
			byHour[key] = item
		}
		item.eventCount += agg.EventCount
		item.excursionCount += agg.ExcursionCount
		if agg.PeakTemperatureC > item.peak {
			item.peak = agg.PeakTemperatureC
		}
		item.points[agg.PointID] = true
	}

	out := make([]HourlyBucket, 0, len(byHour))
	for hour, item := range byHour {
		out = append(out, HourlyBucket{
			HourStart:        hour,
			EventCount:       item.eventCount,
			ExcursionCount:   item.excursionCount,
			PeakTemperatureC: item.peak,
			Points:           len(item.points),
		})
	}
	sortHourly(out)
	return out
}

// errSourceNotReady 是取数源没装配好时的错误。
//
// 它是显式错误而不是「返回全 0」：全 0 会被读成「今天什么事都没发生」，
// 而真相是「根本没查」。两者混起来，运维会以为服务是安静的，其实是瞎的。
func errSourceNotReady() error {
	return errcode.Newf(errcode.StorageError, "运行摘要取数源未装配本地库，无法统计")
}
