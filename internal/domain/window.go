package domain

import "time"

// 本文件是越温窗口及其「数据异常」的类型。
//
// 窗口不只是「哪一段时间越限」：它还带着这批读数本身的可信度信息——
// 有没有数据缺口、记录仪的钟有没有回拨、探头有没有卡死。
//
// 这些不写进窗口，结论就只剩一个时间区间和一个峰值数字，
// 复核的人无从判断这段结论是「确实如此」还是「数据本身就不可靠」。
// 冷链场景里这两种情况对批次处置的含义完全不同：
// 前者要报药事并评估，后者要先修设备、补数据。

// 采样周期来源常量。
//
// 周期从哪儿来必须写进结论：折算口径不同，同一段读数算出来的持续时长就不同，
// 而持续时长又直接决定「这次越限算不算数」。
const (
	// IntervalSourcePointProfile：采样周期取自点位温区配置的标称值。
	IntervalSourcePointProfile = "point_profile"
	// IntervalSourceInferred：点位未配置标称周期，由相邻采样间隔的中位数推断。
	IntervalSourceInferred = "inferred"
	// IntervalSourceWallClock：周期无从解析，只能退回墙钟折算（兜底，偏宽松）。
	IntervalSourceWallClock = "wall_clock"
)

// ExcursionWindow 是一个越温窗口。
//
// 窗口由「双阈值迟滞」规则抽出：进入越限要连续超限达到 min_excursion_minutes，
// 退出越限要回落到 (zone_max_c - hysteresis_c) 以内才算回到温区。
// 迟滞带的作用是消抖——空调启停造成的贴边抖动不该拆成好几段窗口，
// 更不该让每段都因为「不够长」而一个都判不出来。
//
// 窗口起止落在**真实越限采样点**上（第一段越限的首点、最后一段越限的末点），
// 不是落在迟滞带的边界上：窗口要指认的是「哪几个读数真的越限了」，
// 复核时能一条条对着记录仪导出的表核。
//
// 这几个字段都要能回溯：HysteresisC 是本次抽取用的迟滞带（℃），
// MergeCount 是被迟滞合并成同一窗口的越限段数（0 表示只有一段、没有合并），
// OutOfRangeSamples 是窗口内实际越限的读数条数（与 SampleCount 的差值
// 就是迟滞带里「托住」窗口的那些回落读数），
// MinExcursionMinutes 是本次判定采用的「最短越限时长」门槛（分钟）。
//
// 后半组字段说的是这批读数本身的可信度：
// SamplingIntervalSec / IntervalSource / InferredIntervalSec 是折算口径；
// DataGaps / BreakpointCount 是数据缺口（缺口处不连线）；
// ClockRegressions 是记录仪时钟回拨（时间轴不可信）；
// ProbeStuckSpans 是探头卡死（读数不动不等于温度不动）；
// RiseRate 是温升速率，卡死段已被剔除。
type ExcursionWindow struct {
	Start           time.Time `json:"start"`
	End             time.Time `json:"end"`
	DurationMinutes int       `json:"duration_minutes"`
	PeakC           float64   `json:"peak_c"`
	PeakAt          time.Time `json:"peak_at"`
	Zone            string    `json:"zone"`
	SampleCount     int       `json:"sample_count"`
	// HysteresisC 是本次抽取采用的迟滞带（℃）。
	HysteresisC float64 `json:"hysteresis_c"`
	// MergeCount 是被迟滞合并进本窗口的越限段数；0 表示没有发生合并。
	MergeCount int `json:"merge_count"`
	// OutOfRangeSamples 是窗口内实际越限的读数条数。
	OutOfRangeSamples int `json:"out_of_range_samples"`
	// MinExcursionMinutes 是本次判定采用的最短越限时长门槛（分钟）。
	MinExcursionMinutes int `json:"min_excursion_minutes"`
	// SamplingIntervalSec 是本次判定实际采用的采样周期（秒）。
	SamplingIntervalSec int `json:"sampling_interval_sec,omitempty"`
	// IntervalSource 是采样周期来源：point_profile / inferred / wall_clock。
	IntervalSource string `json:"interval_source,omitempty"`
	// InferredIntervalSec 是由相邻采样间隔中位数推断出的周期（秒）。
	//
	// 它与 SamplingIntervalSec 可能不一致：点位配置的标称周期是约定，
	// 设备实际怎么采是事实。两者对不上时，事实写在窗口里，供复核时判断
	// 是不是点位配置过期了。
	InferredIntervalSec int `json:"inferred_interval_sec,omitempty"`
	// DataGaps 是窗口内的数据缺口。
	DataGaps []DataGap `json:"data_gaps,omitempty"`
	// BreakpointCount 是窗口内的数据缺口数（断点数）。
	//
	// 缺口处不连线：缺的那段时间没有读数，不能算进越温时长，
	// 也不能拿两头的读数接成一段不存在的连续温升。
	BreakpointCount int `json:"breakpoint_count,omitempty"`
	// ClockRegressions 是窗口内的时间轴异常（记录仪时钟回拨）。
	ClockRegressions []ClockRegression `json:"clock_regressions,omitempty"`
	// ProbeStuckSpans 是窗口内的探头卡死段。
	ProbeStuckSpans []ProbeStuckSpan `json:"probe_stuck_spans,omitempty"`
	// RiseRate 是温升速率特征；卡死段已从中剔除。
	RiseRate *RiseRate `json:"rise_rate,omitempty"`
	// Segments 是本窗口由几段越限合并而来的分段明细。
	//
	// 单段窗口时长度为 1。合并的目的是让「一次异常」在台账上只算一条，
	// 但分段明细不能丢：复核时要能看出这次异常是连续一段，
	// 还是「冲出去—回落一下—又冲出去」（后者往往是设备在反复启停）。
	Segments []WindowSegment `json:"segments,omitempty"`
	// SegmentCount 是本窗口合并而来的越限段数；1 表示只有一段、没有合并。
	SegmentCount int `json:"segment_count"`
	// RecoveryMinutes 是合并时确认回落到温区的时长（分钟）。
	//
	// 它不计入越温时长：冰箱确实在那一刻回到了安全温度。
	// 但它必须写出来——「一次异常中间回落到温区 5 分钟」与
	// 「一次异常全程没回落」对药师的判断不是一回事：
	// 前者说明设备在反复启停，该查的是设备而不是那批疫苗。
	//
	// 口径：只数两段之间确实落在温区内的采样，按采样点数折算。
	// 「越限→回落」「回落→越限」两段过渡的不确定度不计入。
	RecoveryMinutes int `json:"recovery_minutes,omitempty"`
}

// Contains 判断一个时刻是否落在窗口覆盖范围内（闭区间）。
//
// 闭区间是有讲究的：窗口的起止就是那两条真实越限采样点的时刻，
// 它们本身算在窗口内。边界归属一旦含糊，同一条读数就会被算进两个窗口，
// 「这次越限持续了多久」当场对不上账。
func (w ExcursionWindow) Contains(t time.Time) bool {
	return !t.Before(w.Start) && !t.After(w.End)
}

// DataGap 是一处采样缺口。
//
// 缺口指相邻两条采样之间中断了三个采样周期以上的时间。
// 它不是故障，只是「这段时间没有数据」——但它必须显式记下来：
// 不记，缺口两头的读数就会被连成一段连续温升，
// 凭空造出一段现场并不存在的越温。
type DataGap struct {
	// From 是缺口前最后一条采样的时刻。
	From time.Time `json:"from"`
	// To 是缺口后第一条采样的时刻。
	To time.Time `json:"to"`
	// GapMinutes 是缺口时长（分钟）。
	GapMinutes int `json:"gap_minutes"`
	// MissingSamples 是按采样周期估算缺失的采样条数。
	MissingSamples int `json:"missing_samples"`
	// Note 是给复核人看的说明。
	Note string `json:"note,omitempty"`
}

// ClockRegression 是一次记录仪时钟回拨。
//
// 时钟回拨（换电池、记录仪被断电重启、网关重发历史数据）会让 measured_at 倒退。
// 一旦倒退，按时刻排序就会把回拨前后的读数搅在一起——
// 先发生的高温会被排到后发生的低温后面，凭空造出一段不存在的升降温。
//
// 所以确认回拨后，判定按**记录仪序**重排：设备一直在采样，只是它的钟不准。
type ClockRegression struct {
	// Index 是回拨发生在记录仪序里的位置（该采样是倒退后的第一条）。
	Index int `json:"index"`
	// At 是倒退后的那条采样的原始时刻。
	At time.Time `json:"at"`
	// PreviousAt 是倒退前那条采样的原始时刻。
	PreviousAt time.Time `json:"previous_at"`
	// BackwardMinutes 是回拨了多少分钟（正数）。
	BackwardMinutes int `json:"backward_minutes"`
	// Note 是给复核人看的说明，含窗口起止采用修正时间轴的口径。
	Note string `json:"note,omitempty"`
}

// ProbeStuckSpan 是一段探头卡死。
//
// 判据：连续若干条读数的数值完全相同，且该数值不在温区中值上。
//
// 数值恒定在温区中值时，「探头不动」与「冰箱本来就稳」分不开，
// 标成卡死会误伤；恒定在偏离中值的位置（一直是 12.4℃ 这种）才是探头失灵的硬特征。
//
// 卡死段的读数不进温升速率：它们不是温度在动，是探头不动了。
// 把它们算进斜率，等于让一段假读数把「温升多快」这个特征带偏。
type ProbeStuckSpan struct {
	// From 是卡死段首条采样的时刻。
	From time.Time `json:"from"`
	// To 是卡死段末条采样的时刻。
	To time.Time `json:"to"`
	// SampleCount 是这一段里的采样条数。
	SampleCount int `json:"sample_count"`
	// Value 是这一段的恒定读数（℃）。
	Value float64 `json:"value"`
	// Note 是给复核人看的说明。
	Note string `json:"note,omitempty"`
}

// RiseRate 是温升速率特征。
//
// 它回答的是「温度升得有多快」——单位 ℃/分钟。
// 温升速率是归因的关键特征之一：缓慢爬升与陡升指向不同的原因，
// 而「升得快」和「探头卡死」这两件事必须分开看。
type RiseRate struct {
	// PerMinute 是从连续观测段起点到峰值点的平均温升速率（℃/分钟）；正数为升温。
	PerMinute float64 `json:"per_minute"`
	// MinutesToPeak 是起点到峰值经历的有效分钟数。
	MinutesToPeak int `json:"minutes_to_peak"`
	// PeakC 是参与计算的峰值（℃）。
	//
	// 它可能与窗口的 peak_c 不同：窗口峰值是「记录到的最高读数」，
	// 卡死读数也可能在里面；这里的峰值只取有效读数——
	// 拿一段不动的读数当峰值，等于让探头故障去定义这次温升有多高。
	PeakC float64 `json:"peak_c"`
	// ValidSamples 是参与计算的有效采样条数（已剔除探头卡死段）。
	ValidSamples int `json:"valid_samples"`
	// ExcludedStuckSamples 是因探头卡死被剔除的采样条数。
	ExcludedStuckSamples int `json:"excluded_stuck_samples"`
	// ExcludedGapMinutes 是因数据缺口未计入的分钟数。
	ExcludedGapMinutes int `json:"excluded_gap_minutes"`
	// Usable 表示有效采样不足，速率不成立。
	Usable bool `json:"usable"`
	// Note 是计算口径说明。
	Note string `json:"note,omitempty"`
}
