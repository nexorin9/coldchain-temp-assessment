// 本文件落「出站写回没落成之后怎么办」这一段：待补偿清单与补偿重放。
//
// 为什么需要这一段：台账写不进去是常态而不是例外——
// 台账侧在维护、网关在重启、网线被踢掉。写回失败一次就结束的话，
// 这次越温就永远躺在「台账里查不到」的状态里：
// 药事以为记了账，门诊不知道要隔离，账面与实物对不上。
//
// 所以写回失败不是「失败」，是「挂起」：run 落到 pending_writeback，
// 留下可见的告警记录，等人来补。
//
// 三条纪律：
//
//  1. **补偿重放不是重新确认**：走同一条主路径、同一份固化载荷、
//     同一把幂等键（external_ref / idempotency_key）。
//     重放出来的那条与当初那条是同一条越温，台账侧不会多出一条。
//  2. **只补没写成的那一处**：台账写成了、通知没发出去的，
//     重放只补通知——台账那一处连请求都不发。
//  3. **重放仍失败就继续挂起**：不静默丢弃，告警累加次数，
//     待补偿清单里这条一直都在，直到有人把它补上。
package runtime

import (
	"context"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
)

// CompensationStore 是补偿重放需要的本地读写口。
//
// 与 WritebackStore 分开声明：补偿重放要看的是「哪些还没写成」
// 这类**清单**视角，与首次写回要的「这一处写没写成」是两种取数。
type CompensationStore interface {
	// RunByID 回读一条判定 run 的状态快照。
	RunByID(ctx context.Context, runID string) (domain.RunRecord, error)
	// PendingWritebacks 回读全部还没写成的写回记录。
	PendingWritebacks(ctx context.Context) ([]domain.WritebackRecord, error)
	// CountPendingWritebacks 统计还没写成的写回记录条数。
	CountPendingWritebacks(ctx context.Context) (int, error)
	// OpenWritebackAlerts 回读全部还挂着的告警。
	OpenWritebackAlerts(ctx context.Context) ([]domain.WritebackAlert, error)
	// CountOpenWritebackAlerts 统计还挂着的告警条数。
	CountOpenWritebackAlerts(ctx context.Context) (int, error)
	// WritebackAlertByTarget 回读某条 run 在某一处目标上的告警。
	WritebackAlertByTarget(ctx context.Context, runID string, target string) (domain.WritebackAlert, error)
}

// PendingWriteback 是一条待补偿重放的写回记录（带 run 侧的定位信息）。
//
// 它是给人看的清单条目：药学部冷链管理员打开这个列表，
// 要能一眼看出「哪条判定、哪个点位、台账还是通知没写进去、
// 已经敲了多少次门」。
type PendingWriteback struct {
	// RunID 是被写回的判定 run。
	RunID string `json:"run_id"`
	// Target 是写回目标（ledger / notification）。
	Target string `json:"target"`
	// State 是这一处的写回状态（pending / failed）。
	State string `json:"state"`
	// ExternalRef 是这次越温的外部引用；台账按它识别同一次越温。
	ExternalRef string `json:"external_ref,omitempty"`
	// IdempotencyKey 是这次越温的幂等键；补偿重放用的就是它。
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// ErrorCode 是最近一次失败的错误码。
	ErrorCode string `json:"error_code,omitempty"`
	// ErrorDetail 是最近一次失败的细节。
	ErrorDetail string `json:"error_detail,omitempty"`
	// Attempts 是这一处目标累计发起的出站调用次数。
	Attempts int `json:"attempts"`
	// Failures 是这一处目标累计失败轮次。
	Failures int `json:"failures"`
	// RunState 是这条判定当前的业务状态。
	RunState string `json:"run_state,omitempty"`
	// PointID 是越温点位。
	PointID string `json:"point_id,omitempty"`
	// UpdatedAt 是最近一次落库的时刻。
	UpdatedAt time.Time `json:"updated_at"`
}

// CompensationSummary 是待补偿清单的汇总视图。
//
// 计数进运行摘要：待写回数量是「台账与账面有没有对上」的体温计，
// 平时是 0，一旦不是 0 就得有人管。
type CompensationSummary struct {
	// PendingCount 是还没写成的写回记录条数。
	PendingCount int `json:"pending_count"`
	// OpenAlertCount 是还挂着的告警条数。
	OpenAlertCount int `json:"open_alert_count"`
	// Items 是待补偿的写回记录清单。
	Items []PendingWriteback `json:"items"`
	// Alerts 是还挂着的告警清单。
	Alerts []domain.WritebackAlert `json:"alerts"`
}

// Compensator 是补偿重放的编排者。
//
// 它复用同一套 Writebacker：首次写回与补偿重放走同一条主路径，
// 差别只在状态迁移留痕上的来源（writeback / compensate）与
// 「已经写成的那一处跳过」这条边。
type Compensator struct {
	store  CompensationStore
	writer *Writebacker
	logger *logging.Logger
	now    func() time.Time
}

// NewCompensator 装配补偿重放编排器。
func NewCompensator(store CompensationStore, writer *Writebacker, logger *logging.Logger) *Compensator {
	if logger == nil {
		logger = logging.Default()
	}
	return &Compensator{
		store:  store,
		writer: writer,
		logger: logger,
		now:    time.Now,
	}
}

// WithClock 注入时钟；测试可注入固定时钟，让清单里的时刻可复现。
func (c *Compensator) WithClock(now func() time.Time) *Compensator {
	if now != nil {
		c.now = now
	}
	return c
}

// Summary 列出待补偿的写回记录与还挂着的告警。
//
// 清单按「最近更新时刻升序」排：最早失败的那条排在最前面，
// 因为它等得最久。
func (c *Compensator) Summary(ctx context.Context) (CompensationSummary, error) {
	if c == nil || c.store == nil {
		return CompensationSummary{}, errcode.Newf(errcode.StorageError, "补偿重放未装配本地存储")
	}
	records, err := c.store.PendingWritebacks(ctx)
	if err != nil {
		return CompensationSummary{}, err
	}
	alerts, err := c.store.OpenWritebackAlerts(ctx)
	if err != nil {
		return CompensationSummary{}, err
	}
	pendingCount, err := c.store.CountPendingWritebacks(ctx)
	if err != nil {
		return CompensationSummary{}, err
	}
	alertCount, err := c.store.CountOpenWritebackAlerts(ctx)
	if err != nil {
		return CompensationSummary{}, err
	}

	items := make([]PendingWriteback, 0, len(records))
	for _, rec := range records {
		item := PendingWriteback{
			RunID:          rec.RunID,
			Target:         rec.Target,
			State:          rec.State,
			ExternalRef:    rec.ExternalRef,
			IdempotencyKey: rec.IdempotencyKey,
			ErrorCode:      rec.ErrorCode,
			ErrorDetail:    rec.ErrorDetail,
			UpdatedAt:      rec.UpdatedAt,
		}
		// 告警里的累计次数比写回记录上的更细：它记着这一处一共敲了多少次门。
		if alert, aerr := c.store.WritebackAlertByTarget(ctx, rec.RunID, rec.Target); aerr == nil {
			item.Attempts = alert.Attempts
			item.Failures = alert.Failures
		}
		// run 侧的定位信息（点位、设备、业务状态）取不到时不臆造：
		// 库里没有这条 run 就只报 run_id，不填猜测的点位。
		if run, rerr := c.store.RunByID(ctx, rec.RunID); rerr == nil {
			item.RunState = run.Run.State
			item.PointID = run.Run.PointID
		}
		items = append(items, item)
	}

	return CompensationSummary{
		PendingCount:   pendingCount,
		OpenAlertCount: alertCount,
		Items:          items,
		Alerts:         alerts,
	}, nil
}

// Retry 对一条待补偿的判定触发补偿重放。
//
// 它做三件事：
//  1. 确认这条 run 确实有没写成的那一处——没有就不该重放（否则是空跑）；
//  2. 走与首次写回同一条主路径（同一份固化载荷、同一把幂等键）；
//  3. 把结果如实回执：补上了说补上了，没补上说没补上、还差什么。
//
// 重放成功会把 run 从 pending_writeback 推到 written_back，
// 并把这一处的告警解掉；重放仍失败则告警次数继续累加，
// 这条仍留在待补偿清单里。
func (c *Compensator) Retry(ctx context.Context, runID string) (WritebackOutcome, error) {
	if c == nil || c.store == nil {
		return WritebackOutcome{}, errcode.Newf(errcode.StorageError, "补偿重放未装配本地存储")
	}
	if c.writer == nil {
		return WritebackOutcome{}, errcode.Newf(errcode.StorageError, "补偿重放未装配出站写回")
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return WritebackOutcome{}, errcode.Newf(errcode.Validation, "补偿重放缺少 run_id")
	}

	// 重放前先看这条 run 是不是真的还有没写成的那一处。
	rec, err := c.store.RunByID(ctx, runID)
	if err != nil {
		return WritebackOutcome{}, err
	}
	pending, err := c.store.PendingWritebacks(ctx)
	if err != nil {
		return WritebackOutcome{}, err
	}
	hasPending := false
	for _, p := range pending {
		if p.RunID == runID {
			hasPending = true
			break
		}
	}
	if !hasPending {
		return WritebackOutcome{}, errcode.Newf(errcode.IllegalTransition,
			"判定 run %s 当前业务状态为 %q，没有待补偿的写回目标，无需重放", runID, rec.Run.State)
	}

	outcome, err := c.writer.Replay(ctx, runID)
	if err != nil {
		return outcome, err
	}
	c.logger.Info("writeback_compensation_replayed",
		"run_id", runID,
		"ledger_written", outcome.LedgerWritten,
		"ledger_skipped", outcome.LedgerSkipped,
		"notification_sent", outcome.NotificationSent,
		"notification_skipped", outcome.NotificationSkipped,
		"ledger_attempts", outcome.LedgerAttempts,
		"notification_attempts", outcome.NotificationAttempts,
		"writeback_state", outcome.WritebackState,
	)
	return outcome, nil
}
