package domain

import "time"

// 本文件是「短时波动」与「合并窗口分段明细」的类型。
//
// 越限与越温不是一回事：冷库开门取苗，箱内温度冲高几度、十几分钟后落回来，
// 那是波动；连着超限半小时，那才是越温。两者对批次可用性的含义完全不同——
// 波动每天都在发生，为它惊动药事、报疫苗生产企业评估批次，台账上很快全是噪声；
// 越温漏判则是一批疫苗去向不明。
//
// 所以这两样必须分开归集：够门槛的合成正式越温窗口，
// 不够门槛的一条条列成短时波动，交给药师自己看。

// 越限侧别常量。
const (
	// ExcursionSideHigh：读数越过温区上界（偏热）。
	ExcursionSideHigh = "high"
	// ExcursionSideLow：读数低于温区下界（偏冷）。
	ExcursionSideLow = "low"
)

// TransientExcursion 是一段未达最短越限门槛的短时越限（波动）。
//
// 它保留起止、峰值与持续时长，是为了让人能拿它跟记录仪导出的表一条条核：
// 「9 月 3 日 10:15 那次开门冲高到 9.6℃」是可以对得上的事实，
// 「有过若干次波动」不是。
type TransientExcursion struct {
	// Start / End 是这段波动的起止时刻，落在真实越限采样点上。
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	// DurationMinutes 是这段波动的确认时长（分钟），按采样点数折算。
	DurationMinutes int `json:"duration_minutes"`
	// PeakC 是这段波动里偏离温区最远的读数（℃）。
	PeakC float64 `json:"peak_c"`
	// PeakAt 是峰值所在采样的时刻。
	PeakAt time.Time `json:"peak_at"`
	// SampleCount 是这段波动覆盖的采样条数。
	SampleCount int `json:"sample_count"`
	// OutOfRangeSamples 是其中实际越限的读数条数。
	OutOfRangeSamples int `json:"out_of_range_samples"`
	// Side 是越限所在的一侧：high（偏热）/ low（偏冷）。
	Side string `json:"side"`
	// Reason 是归集为波动的理由，写清「差多少才算越温」。
	Reason string `json:"reason"`
	// Note 是给复核人看的补充说明。
	Note string `json:"note,omitempty"`
}

// WindowSegment 是合并窗口里保留的一段越限明细。
//
// 合并是为了让「一次异常」在台账上只算一条，但分段明细不能丢：
// 复核时要能看出这次异常是「连续一段」还是「冲出去又回来一次」——
// 后者往往意味着设备在反复启停，而不是一次干净的故障，
// 两者的处置方式不一样。
type WindowSegment struct {
	// Start / End 是这一段的起止时刻，落在真实越限采样点上。
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	// DurationMinutes 是这一段的越限时长（分钟），按采样点数折算。
	DurationMinutes int `json:"duration_minutes"`
	// PeakC 是这一段的峰值（℃）。
	PeakC float64 `json:"peak_c"`
	// PeakAt 是这一段峰值所在采样的时刻。
	PeakAt time.Time `json:"peak_at"`
	// SampleCount 是这一段覆盖的采样条数。
	SampleCount int `json:"sample_count"`
	// OutOfRangeSamples 是这一段里实际越限的读数条数。
	OutOfRangeSamples int `json:"out_of_range_samples"`
	// RecoveryAfterMinutes 是本段之后确认回落到温区的时长（分钟）；末段为 0。
	//
	// 它解释了「为什么两段会被并成一条」：回落不足门槛时长，说明设备没有真正恢复。
	RecoveryAfterMinutes int `json:"recovery_after_minutes,omitempty"`
}
