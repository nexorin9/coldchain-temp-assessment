// 本文件落「运行摘要」的取数查询：服务自己在跑的这 24 小时里发生了什么。
//
// 为什么单独一个文件：摘要要的是**计数**，不是明细。它问的是
// 「近 24 小时判了多少段越温、失败了几段、模型调了几次、还有几条没进台账」，
// 而不是一个点位一小时有多少条采样。
//
// 三条口径贯穿本文件：
//
//   - 全部走索引：每条计数查询都带时间下界或状态等值条件，命中的是索引
//     范围扫描，不会把整张表读进内存。多点位 5 分钟粒度持续上报，一天就能
//     攒下几万条采样——摘要端点被运维轮询，扫全表会直接把主路径拖垮。
//   - 窗口口径统一：所有「近 N 小时」都按同一段 [from, to] 闭区间取，
//     不同指标之间不会各算各的一段。
//   - 逐小时分解走聚合表：event_hourly_agg 里已经是按小时归好桶的统计，
//     摘要直接读它，不对原始采样做 GROUP BY。
//
// 索引是幂等补齐的：建索引语句带 IF NOT EXISTS，存量库每次启动跑一遍即可，
// 不需要单独的迁移命令。
package store

import (
	"context"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// metricsIndexSQL 是运行摘要取数用到的索引。
//
// 逐条说明为什么需要它——不是为了「以后也许用得上」，而是每条都对应
// 摘要里的一项指标：
//
//   - idx_runs_created：近 24 小时新建了多少段判定（按创建时刻窗口化）。
//   - idx_runs_state：当前有多少条停在「待确认」等某个业务状态。
//   - idx_run_audit_entries_type_created：近 24 小时模型调了几次、
//     其中几次降级、几次失败（按类型 + 时刻窗口化）。
//
// 没有它们时，同样的查询会退化成整表扫描：runs 表按天增长，
// 审计表按判定条数增长，两者都不是能随手扫的规模。
const metricsIndexSQL = `
CREATE INDEX IF NOT EXISTS idx_runs_created ON runs (created_at);
CREATE INDEX IF NOT EXISTS idx_runs_state ON runs (state);
CREATE INDEX IF NOT EXISTS idx_run_audit_entries_type_created ON run_audit_entries (entry_type, created_at);
`

// prepareMetricsIndexes 建运行摘要用到的索引（幂等）。
func (s *Store) prepareMetricsIndexes(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, metricsIndexSQL); err != nil {
		return errcode.Newf(errcode.StorageError, "建运行摘要索引失败：%v", err)
	}
	return nil
}

// CountEventsInRange 统计区间内入库的温度采样条数。
//
// 口径：按 measured_at 落在闭区间 [from, to] 内计数。
// 命中 idx_events_measured 的索引范围扫描，不读表行。
func (s *Store) CountEventsInRange(ctx context.Context, from, to time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM events WHERE measured_at >= ? AND measured_at <= ?",
		from.UTC().Format(tsLayout), to.UTC().Format(tsLayout)).Scan(&n)
	if err != nil {
		return 0, errcode.Newf(errcode.StorageError, "统计温度采样失败：%v", err)
	}
	return n, nil
}

// CountRunsCreatedInRange 统计区间内新建的判定 run 数。
//
// 口径：按 created_at 落在闭区间内计数——「近 24 小时判了多少段越温」。
// 命中 idx_runs_created。
func (s *Store) CountRunsCreatedInRange(ctx context.Context, from, to time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM runs WHERE created_at >= ? AND created_at <= ?",
		from.UTC().Format(tsLayout), to.UTC().Format(tsLayout)).Scan(&n)
	if err != nil {
		return 0, errcode.Newf(errcode.StorageError, "统计判定 run 失败：%v", err)
	}
	return n, nil
}

// CountRunsFailedInRange 统计区间内最终失败的判定 run 数。
//
// 口径：执行态为 failed（步骤重试已耗尽）且最近更新时刻落在闭区间内。
// 命中 idx_runs_exec_state（exec_state, updated_at）。
func (s *Store) CountRunsFailedInRange(ctx context.Context, from, to time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM runs WHERE exec_state = ? AND updated_at >= ? AND updated_at <= ?",
		string(domain.ExecStateFailed), from.UTC().Format(tsLayout), to.UTC().Format(tsLayout)).Scan(&n)
	if err != nil {
		return 0, errcode.Newf(errcode.StorageError, "统计失败判定失败：%v", err)
	}
	return n, nil
}

// CountRunsDegradedInRange 统计区间内新建且结论降级的判定 run 数。
//
// 口径：degraded = 1（归因结论退回了规则基线）且创建时刻落在闭区间内。
func (s *Store) CountRunsDegradedInRange(ctx context.Context, from, to time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM runs WHERE degraded = 1 AND created_at >= ? AND created_at <= ?",
		from.UTC().Format(tsLayout), to.UTC().Format(tsLayout)).Scan(&n)
	if err != nil {
		return 0, errcode.Newf(errcode.StorageError, "统计降级判定失败：%v", err)
	}
	return n, nil
}

// CountRunsInState 统计当前处于某业务状态的判定 run 数。
//
// 口径：这是**当前值**，不按时间窗口化——「现在有多少条停在待确认」
// 是一个此刻的存量问题，跟近 24 小时判了多少段是两回事。
// 命中 idx_runs_state。
func (s *Store) CountRunsInState(ctx context.Context, state string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM runs WHERE state = ?", state).Scan(&n)
	if err != nil {
		return 0, errcode.Newf(errcode.StorageError, "统计业务状态失败：%v", err)
	}
	return n, nil
}

// CountRunsInExecState 统计当前处于某执行态的判定 run 数。
//
// 口径：同样是当前值。running / queued / retrying 三个执行态合起来
// 就是「此刻有多少条判定在跑、多少条在排队、多少条在退避重试」。
func (s *Store) CountRunsInExecState(ctx context.Context, execState string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM runs WHERE exec_state = ?", execState).Scan(&n)
	if err != nil {
		return 0, errcode.Newf(errcode.StorageError, "统计执行态失败：%v", err)
	}
	return n, nil
}

// ModelCallMetrics 是模型调用侧的计数。
type ModelCallMetrics struct {
	// Total 是区间内的模型调用总次数。
	Total int
	// Degraded 是其中走了降级路径的次数（模型不可达、结论退回了规则基线）。
	Degraded int
	// Failed 是其中失败的次数（超时、协议不合规、结论校验不过）。
	Failed int
}

// ModelCallMetricsInRange 统计区间内的模型调用次数、降级次数与失败次数。
//
// 三项在一条 SQL 里用条件聚合一次算出：审计表按判定条数增长，
// 分三次查就等于把索引范围扫三遍。命中 idx_run_audit_entries_type_created。
func (s *Store) ModelCallMetricsInRange(ctx context.Context, from, to time.Time) (ModelCallMetrics, error) {
	var metrics ModelCallMetrics
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*), "+
			"COALESCE(SUM(CASE WHEN degraded = 1 THEN 1 ELSE 0 END), 0), "+
			"COALESCE(SUM(CASE WHEN outcome = ? THEN 1 ELSE 0 END), 0) "+
			"FROM run_audit_entries WHERE entry_type = ? AND created_at >= ? AND created_at <= ?",
		domain.AuditOutcomeFailed, domain.AuditEntryModelCall,
		from.UTC().Format(tsLayout), to.UTC().Format(tsLayout)).
		Scan(&metrics.Total, &metrics.Degraded, &metrics.Failed)
	if err != nil {
		return ModelCallMetrics{}, errcode.Newf(errcode.StorageError, "统计模型调用失败：%v", err)
	}
	return metrics, nil
}
