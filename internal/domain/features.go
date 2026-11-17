// 本文件是「归因特征」的类型。
//
// 特征是规则基线归因与模型归因**共用的输入面**：两边看的是同一批数字。
// 不共用就会出现「规则说是因为断电、模型说是因为开门」，
// 而复核的人手上只有两份互相打架的文字，谁也说不清该信谁。
//
// 特征分三组：
//
//  1. **温度形态**：温升速率、峰值偏离度、相邻采样的跳变幅度与次数；
//  2. **环境与设备事件**：湿度上升幅度、开门累计时长与滞后、断电与除霜；
//  3. **可信度**：设备自报的测温精度、探头卡死采样条数。
//
// 缺测的特征一律写 nil，并把名字记进 MissingFeatures，不用 0 顶替：
// 「设备没上报湿度」和「湿度上升 0 个百分点」在归因时是两回事——
// 前者是这条线索根本不可用，后者是开门时湿度确实没变。
// 两者混成一个 0，规则归因就会把「查不到」当成「查过了，没有」。

package domain

import "time"

// 特征名常量。
//
// 算不出来的特征把名字记进 ExcursionFeatures.MissingFeatures。
// 名字写成稳定的英文短横线形式：它们会进模型请求体的特征清单，
// 也进台账里的审计记录，事后要能逐字对上。
const (
	// FeatureRiseRatePerHour：温升速率（℃/小时）。
	FeatureRiseRatePerHour = "rise_rate_per_hour"
	// FeaturePeakDeviationC：峰值偏离温区边界的幅度（℃）。
	FeaturePeakDeviationC = "peak_deviation_c"
	// FeatureHumidityRisePct：窗口内湿度相对基线的上升幅度（百分点）。
	FeatureHumidityRisePct = "humidity_rise_pct"
	// FeatureDoorOpenMinutes：开门累计时长（分钟）。
	FeatureDoorOpenMinutes = "door_open_minutes"
	// FeatureDoorToRiseDelayMinutes：开门到温升起点的滞后（分钟）。
	FeatureDoorToRiseDelayMinutes = "door_to_rise_delay_minutes"
	// FeaturePowerLossObserved：观测段内是否出现过断电事件。
	FeaturePowerLossObserved = "power_loss_observed"
	// FeatureDefrostObserved：观测段内是否出现过除霜事件。
	FeatureDefrostObserved = "defrost_observed"
	// FeatureSampleJump：相邻采样的跳变幅度与跳变次数。
	FeatureSampleJump = "sample_jump"
	// FeatureAccuracyC：设备自报的测温精度（℃）。
	FeatureAccuracyC = "accuracy_c"
)

// ExcursionFeatures 是一次越温的归因特征集。
//
// 它描述的是「这次越温长什么样」，不是「这次越温是什么原因」——
// 原因由归因步骤（规则基线或模型）从这批特征里推出来，
// 并且必须附上用了哪几条特征作为依据。
//
// 时间口径：特征取自越温窗口所在的**连续观测段**（窗口起点之前不跨数据缺口）
// 与窗口本身。开门往往在越限前就开始——门开了十五分钟，温度才爬过上限——
// 只看窗口内会把开门时长算短，还会得出「开门时温度已经越限了，
// 所以不是开门造成的」这种颠倒因果的结论。
type ExcursionFeatures struct {
	// RiseRatePerHour 是温升速率（℃/小时），正数为升温。
	//
	// 历时按**采样点数**折算，不按墙钟：一条采样记一个采样周期。
	// 墙钟差值会把「记录仪采样节奏不规整」误当成「温升更慢」，
	// 也会在网关补投历史数据时把历时拉长。
	RiseRatePerHour *float64 `json:"rise_rate_per_hour"`
	// RiseRateHours 是折算出的温升历时（小时）。
	RiseRateHours float64 `json:"rise_rate_hours"`
	// RiseRateSamples 是参与折算的采样条数（含起点与峰值两条）。
	RiseRateSamples int `json:"rise_rate_samples"`
	// RiseRateStartC 是温升起点的读数（℃）。
	RiseRateStartC float64 `json:"rise_rate_start_c"`
	// RiseRatePeakC 是参与折算的有效峰值（℃）。
	//
	// 它可能与窗口的 peak_c 不同：探头卡死段的读数不参与折算，
	// 拿一段不动的读数当峰值，等于让探头故障去定义这次温升有多高。
	RiseRatePeakC float64 `json:"rise_rate_peak_c"`
	// RiseRateExcludedGapMinutes 是因数据缺口未计入的分钟数。
	RiseRateExcludedGapMinutes int `json:"rise_rate_excluded_gap_minutes,omitempty"`

	// PeakDeviationC 是峰值偏离温区边界的幅度（℃）。
	PeakDeviationC *float64 `json:"peak_deviation_c"`
	// PeakDeviationSide 是偏离侧别：high（偏热）/ low（偏冷）。
	PeakDeviationSide string `json:"peak_deviation_side,omitempty"`

	// HumidityBaselinePct 是窗口之前的湿度基线（%）。
	HumidityBaselinePct *float64 `json:"humidity_baseline_pct"`
	// HumidityBaselineSamples 是基线用到的采样条数。
	HumidityBaselineSamples int `json:"humidity_baseline_samples,omitempty"`
	// HumidityPeakPct 是窗口内的湿度峰值（%）。
	HumidityPeakPct *float64 `json:"humidity_peak_pct"`
	// HumidityRisePct 是窗口内湿度相对基线的上升幅度（百分点）。
	//
	// 它是「开门」与「断电」分得开的要紧线索：开门把室外湿空气带进来，
	// 湿度跟着温度一起涨；断电时压缩机停了，箱内湿度通常纹丝不动。
	HumidityRisePct *float64 `json:"humidity_rise_pct"`

	// DoorOpenMinutes 是观测段内开门的累计时长（分钟）。
	DoorOpenMinutes *int `json:"door_open_minutes"`
	// DoorOpenCount 是观测段内开门事件的出现次数。
	DoorOpenCount *int `json:"door_open_count"`
	// DoorOpenAt 是与温升起点对应的最近一次开门时刻。
	DoorOpenAt *time.Time `json:"door_open_at"`
	// DoorToRiseDelayMinutes 是开门到温升起点的滞后（分钟）。
	//
	// 它把「开门」和「温升」在时间上接起来：滞后越短，开门越像是原因；
	// 滞后为负（开门发生在温升之后）则说明开门多半不是这次越温的原因。
	DoorToRiseDelayMinutes *int `json:"door_to_rise_delay_minutes"`

	// PowerLossObserved 是观测段内是否出现过断电事件。
	PowerLossObserved *bool `json:"power_loss_observed"`
	// PowerLossAt 是首次断电事件的时刻。
	PowerLossAt *time.Time `json:"power_loss_at"`
	// PowerLossCount 是断电事件的出现次数。
	PowerLossCount int `json:"power_loss_count"`

	// DefrostObserved 是观测段内是否出现过除霜事件。
	DefrostObserved *bool `json:"defrost_observed"`
	// DefrostAt 是首次除霜事件的时刻。
	DefrostAt *time.Time `json:"defrost_at"`
	// DefrostCount 是除霜事件的出现次数。
	DefrostCount int `json:"defrost_count"`

	// MaxSampleJumpC 是相邻采样之间的最大温度跳变（℃）。
	MaxSampleJumpC *float64 `json:"max_sample_jump_c"`
	// SampleJumpCount 是跳变幅度超过阈值的相邻采样对数。
	//
	// 它与探头卡死互为反面：卡死是读数一动不动，
	// 突变是读数在相邻两个采样之间跳了一大截——
	// 后者更像探头脱落或接触不良，而不是箱内空气真的这么快变了。
	SampleJumpCount *int `json:"sample_jump_count"`
	// SampleJumpThresholdC 是本次采用的跳变判定阈值（℃）。
	SampleJumpThresholdC float64 `json:"sample_jump_threshold_c"`
	// ProbeStuckSampleCount 是落在探头卡死段里的采样条数。
	ProbeStuckSampleCount int `json:"probe_stuck_sample_count,omitempty"`

	// AccuracyC 是设备自报的测温精度（℃）；设备未声明时为 nil。
	AccuracyC *float64 `json:"accuracy_c"`
	// AccuracyLimitC 是本次判定采用的精度上限（℃）。
	AccuracyLimitC float64 `json:"accuracy_limit_c"`
	// PrecisionLimited 表示设备精度声明超出规范上限，结论仅供参考。
	PrecisionLimited bool `json:"precision_limited"`
	// PrecisionTag 是精度不足时的标注文案。
	PrecisionTag string `json:"precision_tag,omitempty"`

	// DeviceEventsReported 表示这台设备是否上报过设备事件。
	//
	// 它是「没开门」与「这台设备根本不上报开门」的分界：
	// 上报过事件却一次都没报开门，才轮得到「门确实没开过」这个结论。
	DeviceEventsReported bool `json:"device_events_reported"`

	// MissingFeatures 是算不出来的特征名。
	MissingFeatures []string `json:"missing_features"`
	// Notes 是逐条特征的计算口径说明，随结论一起给复核的人看。
	Notes []string `json:"notes"`
}

// Missing 判断某个特征是否算不出来。
func (f ExcursionFeatures) Missing(name string) bool {
	for _, n := range f.MissingFeatures {
		if n == name {
			return true
		}
	}
	return false
}
