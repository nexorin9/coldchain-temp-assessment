// Package observe 落「运行摘要」：服务自己在跑的这 24 小时里发生了什么。
//
// 它回答信息科运维站在服务前最常问的三句话：
//
//   - 现在有事吗？——有没有判定卡住、有没有越温还没进台账、有没有停在待确认；
//   - 哪一步慢？——哪一条在退避重试、哪一段最终失败；
//   - 钱花在哪了？——模型调了几次、降级率多少、失败几次。
//
// 三条纪律贯穿本包：
//
//   - 只读：摘要不推进任何业务状态，也不发任何出站请求。
//     它是看板，不是操作台——要推进状态走确认端点，要补写回走补偿重放。
//   - 取数不扫全表：计数走索引范围扫描，逐小时分解走小时级聚合表。
//     摘要端点被运维轮询，一次整表扫描就足以把主路径拖垮。
//   - 口径随响应给出：窗口起止、统计口径与覆盖情况都写在响应体里，
//     免得看的人对着一个数字猜它算的是哪一段、覆盖了几个小时。
package observe

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"
)

// DefaultWindow 是运行摘要的默认统计窗口（近 24 小时）。
//
// 24 小时这个口径对应院内的一班：冷链越温一般发生在夜间断电、门诊领苗
// 或设备故障时，运维白天来看昨夜到现在这一段最有意义。
const DefaultWindow = 24 * time.Hour

// Summary 是运行摘要的对外形状。
//
// 字段分四组：窗口内发生的（事件 / 判定 / 模型调用）、此刻还欠着的
// （待确认 / 待写回 / 挂着的告警）、此刻在动的（在跑 / 排队 / 重试中）、
// 以及逐小时的分解。
type Summary struct {
	// GeneratedAt 是这份摘要的生成时刻（UTC，RFC3339）。
	GeneratedAt string `json:"generated_at"`
	// WindowHours 是统计窗口长度（小时）。
	WindowHours int `json:"window_hours"`
	// WindowStart 是统计区间起点（UTC，RFC3339）。
	WindowStart string `json:"window_start"`
	// WindowEnd 是统计区间终点（UTC，RFC3339）。
	WindowEnd string `json:"window_end"`

	// Events 是窗口内入库的温度采样条数。
	Events int `json:"events"`
	// Runs 是窗口内新建的判定 run 数（判了几段越温）。
	Runs int `json:"runs"`
	// FailedRuns 是窗口内最终失败的判定 run 数（步骤重试已耗尽）。
	FailedRuns int `json:"failed_runs"`
	// DegradedRuns 是窗口内新建且结论降级的判定 run 数。
	DegradedRuns int `json:"degraded_runs"`

	// AwaitingConfirmation 是此刻停在「待药学部主管确认」的判定数。
	// 它是当前存量，不按窗口化——「现在有多少条等主管点头」是此刻的问题。
	AwaitingConfirmation int `json:"awaiting_confirmation"`
	// PendingWritebacks 是此刻还没写进冷链台账的写回记录数。
	PendingWritebacks int `json:"pending_writebacks"`
	// OpenWritebackAlerts 是此刻还挂着的写回告警数。
	OpenWritebackAlerts int `json:"open_writeback_alerts"`

	// ModelCalls 是窗口内的模型调用总次数。
	ModelCalls int `json:"model_calls"`
	// DegradedModelCalls 是其中走了降级路径的次数。
	DegradedModelCalls int `json:"degraded_model_calls"`
	// FailedModelCalls 是其中失败的次数。
	FailedModelCalls int `json:"failed_model_calls"`
	// DegradeRate 是降级率 = 降级次数 / 模型调用总次数（0~1，保留四位小数）。
	// 一次模型调用都没发生时记 0，不记 1。
	DegradeRate float64 `json:"degrade_rate"`

	// RunningRuns 是此刻正在跑步骤链的判定数。
	RunningRuns int `json:"running_runs"`
	// QueuedRuns 是此刻排队等待的判定数。
	QueuedRuns int `json:"queued_runs"`
	// RetryingRuns 是此刻正退避重试的判定数。
	RetryingRuns int `json:"retrying_runs"`

	// Hourly 是逐小时分解（跨点位合并），按小时起点升序。
	Hourly []HourlyBucket `json:"hourly"`
	// HourlyCoveredHours 是逐小时分解实际覆盖了几个小时。
	// 它小于 WindowHours 时说明小时级聚合还没覆盖完整窗口，
	// 分解可能缺桶——具体缺哪一段，看 Notes。
	HourlyCoveredHours int `json:"hourly_covered_hours"`
	// Notes 是给看摘要的人留的话：口径提醒、覆盖缺口、降级提示。
	Notes []string `json:"notes,omitempty"`
}

// HourlyBucket 是一个小时桶的统计（跨点位合并）。
//
// 口径来自小时级聚合表：事件数按采样落库时刻归桶，越温次数按越限窗口
// 起点归桶——跨小时的同一段越温只算在它开始的那一小时，各小时加起来
// 正好等于实际段数。
type HourlyBucket struct {
	// HourStart 是这个桶的起点（UTC，RFC3339）。
	HourStart string `json:"hour_start"`
	// EventCount 是这一小时入库的采样条数。
	EventCount int `json:"event_count"`
	// ExcursionCount 是这一小时开始的越温段数。
	ExcursionCount int `json:"excursion_count"`
	// PeakTemperatureC 是这一小时的最高温度（℃）。
	PeakTemperatureC float64 `json:"peak_temperature_c"`
	// Points 是这一小时有采样的点位数。
	Points int `json:"points"`
}

// Metrics 是一次取数得到的原始计数。
//
// 它是「库里数出来的」与「对外报出去的」之间的一层：
// 汇总器只在这里做口径换算（降级率、覆盖率），不重新查库。
type Metrics struct {
	// Events 是窗口内入库的温度采样条数。
	Events int
	// Runs 是窗口内新建的判定 run 数。
	Runs int
	// FailedRuns 是窗口内最终失败的判定 run 数。
	FailedRuns int
	// DegradedRuns 是窗口内新建且结论降级的判定 run 数。
	DegradedRuns int
	// AwaitingConfirmation 是此刻停在待确认的判定数。
	AwaitingConfirmation int
	// PendingWritebacks 是此刻还没写进台账的写回记录数。
	PendingWritebacks int
	// OpenWritebackAlerts 是此刻还挂着的写回告警数。
	OpenWritebackAlerts int
	// ModelCalls 是窗口内的模型调用总次数。
	ModelCalls int
	// DegradedModelCalls 是其中走了降级路径的次数。
	DegradedModelCalls int
	// FailedModelCalls 是其中失败的次数。
	FailedModelCalls int
	// RunningRuns 是此刻正在跑步骤链的判定数。
	RunningRuns int
	// QueuedRuns 是此刻排队等待的判定数。
	QueuedRuns int
	// RetryingRuns 是此刻正退避重试的判定数。
	RetryingRuns int
	// Hourly 是逐小时分解。
	Hourly []HourlyBucket
}

// Source 是运行摘要的取数口。
//
// 抽成接口是为了让汇总器不绑死存储实现：生产用 SQLite，
// 测试可以用一份内存里的假数据把各种口径都走一遍。
type Source interface {
	// Collect 取区间内的原始计数。区间两端闭合。
	Collect(ctx context.Context, from, to time.Time) (Metrics, error)
}

// Collector 汇总一次运行摘要。
//
// 它只做两件事：定窗口、取数、算口径。不查库、不写库、不发请求。
type Collector struct {
	// source 是取数口。
	source Source
	// now 是时钟；测试可注入固定时钟，让窗口起止可复现。
	now func() time.Time
	// window 是统计窗口长度。
	window time.Duration
}

// Option 是 Collector 的可选装配项。
type Option func(*Collector)

// WithClock 注入时钟（测试用）。
func WithClock(now func() time.Time) Option {
	return func(c *Collector) {
		if now != nil {
			c.now = now
		}
	}
}

// WithWindow 设置统计窗口；非正数时按默认的近 24 小时处理。
func WithWindow(window time.Duration) Option {
	return func(c *Collector) {
		if window > 0 {
			c.window = window
		}
	}
}

// NewCollector 创建汇总器。source 为 nil 时取数必然失败，不静默回零。
func NewCollector(source Source, opts ...Option) *Collector {
	c := &Collector{
		source: source,
		now:    time.Now,
		window: DefaultWindow,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

// Collect 汇总一次运行摘要。
//
// 窗口取 [now - window, now] 闭区间：所有窗口内指标共用这一段区间，
// 不同指标之间不会各算各的。取数失败时返回错误，不返回一份「全是 0」的摘要
// ——全 0 会被读成「今天什么事都没发生」，而真相是「没查出来」。
func (c *Collector) Collect(ctx context.Context) (Summary, error) {
	if c == nil || c.source == nil {
		return Summary{}, errSourceNotReady()
	}
	now := c.now().UTC()
	to := now
	from := now.Add(-c.window)

	metrics, err := c.source.Collect(ctx, from, to)
	if err != nil {
		return Summary{}, err
	}

	summary := Summary{
		GeneratedAt:          now.Format(time.RFC3339),
		WindowHours:          int(c.window / time.Hour),
		WindowStart:          from.Format(time.RFC3339),
		WindowEnd:            to.Format(time.RFC3339),
		Events:               metrics.Events,
		Runs:                 metrics.Runs,
		FailedRuns:           metrics.FailedRuns,
		DegradedRuns:         metrics.DegradedRuns,
		AwaitingConfirmation: metrics.AwaitingConfirmation,
		PendingWritebacks:    metrics.PendingWritebacks,
		OpenWritebackAlerts:  metrics.OpenWritebackAlerts,
		ModelCalls:           metrics.ModelCalls,
		DegradedModelCalls:   metrics.DegradedModelCalls,
		FailedModelCalls:     metrics.FailedModelCalls,
		RunningRuns:          metrics.RunningRuns,
		QueuedRuns:           metrics.QueuedRuns,
		RetryingRuns:         metrics.RetryingRuns,
		Hourly:               metrics.Hourly,
	}
	if metrics.ModelCalls > 0 {
		summary.DegradeRate = roundTo(
			float64(metrics.DegradedModelCalls)/float64(metrics.ModelCalls), 4)
	}
	summary.HourlyCoveredHours = countHours(metrics.Hourly)
	summary.Notes = buildNotes(summary)
	return summary, nil
}

// buildNotes 按摘要的实际状态补上给看的人留的话。
//
// 只在真有话要说时才写：一切正常时备注为空，不拿「一切正常」刷屏。
func buildNotes(summary Summary) []string {
	var notes []string
	if summary.HourlyCoveredHours < summary.WindowHours {
		notes = append(notes, fmt.Sprintf(
			"小时级聚合只覆盖了 %d/%d 个小时，逐小时分解可能缺桶；"+
				"可用 coldchain export --kind hourly --refresh-aggregates 补齐",
			summary.HourlyCoveredHours, summary.WindowHours))
	}
	if summary.PendingWritebacks > 0 || summary.OpenWritebackAlerts > 0 {
		notes = append(notes, "有越温还没进冷链台账：先看 GET /v1/writebacks/pending 的待补偿清单")
	}
	if summary.FailedRuns > 0 {
		notes = append(notes, "有判定最终失败：看失败那几步的错误码与步骤时间线，必要时重跑")
	}
	if summary.ModelCalls > 0 && summary.DegradeRate > 0 {
		notes = append(notes, "部分结论退回了规则基线：降级不等于失败，但模型端点值得查一下")
	}
	return notes
}

// countHours 数逐小时分解覆盖了几个小时。
func countHours(buckets []HourlyBucket) int {
	seen := make(map[string]bool, len(buckets))
	for _, bucket := range buckets {
		seen[bucket.HourStart] = true
	}
	return len(seen)
}

// roundTo 把一个比率保留到指定小数位。
//
// 比率是要给人看的：0.3333333333 看着像精确值，其实是除不尽的中间量。
// 四位小数够判断「基本没降级」与「三成调用在降级」的差别。
func roundTo(value float64, digits int) float64 {
	factor := math.Pow(10, float64(digits))
	return math.Round(value*factor) / factor
}

// sortHourly 把逐小时分解按小时起点升序排好。
//
// 顺序由取数方保证；这里再排一次是为了让「桶」的顺序不依赖 SQL 的返回顺序。
func sortHourly(buckets []HourlyBucket) {
	sort.Slice(buckets, func(i, j int) bool {
		return buckets[i].HourStart < buckets[j].HourStart
	})
}
