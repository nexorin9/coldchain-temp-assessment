package bench

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/rules"
)

// Report 是基准报告（落盘到 data/bench_baseline.json 的那份）。
type Report struct {
	// Dataset 是数据集名。
	Dataset string `json:"dataset"`
	// DatasetPath 是数据集文件路径。
	DatasetPath string `json:"dataset_path"`
	// Records 是参与基准的数据行数。
	Records int `json:"records"`
	// GeneratedAt 是本次运行的时刻（ISO 8601）。
	GeneratedAt string `json:"generated_at"`
	// Options 是本次基准的口径参数。
	Options OptionsReport `json:"options"`
	// Timings 是各阶段的实测耗时（秒）。
	Timings Timings `json:"timings"`
	// Metrics 是三项主指标。
	Metrics Metrics `json:"metrics"`
	// Streams 是逐条可比流的明细。
	Streams []StreamReport `json:"streams"`
	// Notes 是口径说明与实测发现。
	Notes []string `json:"notes"`
	// Reproducible 是重复运行时确定性指标是否逐字一致。
	// 指标本身是确定性的，耗时不是——所以这一项只管前两项。
	Reproducible *bool `json:"reproducible,omitempty"`
}

// OptionsReport 是基准口径的可读形式（便于复核时知道这轮是怎么配的）。
type OptionsReport struct {
	// ToleranceC 是温区相对设定点的容差（℃）。
	ToleranceC float64 `json:"tolerance_c"`
	// AnnotationToleranceC 是从数据集标注里读出的阈值容差（℃）。
	AnnotationToleranceC float64 `json:"annotation_tolerance_c"`
	// ContextSamples 是归因上下文的采样条数。
	ContextSamples int `json:"context_samples"`
	// MinExcursionMinutes 是最短越限时长（分钟）。
	MinExcursionMinutes int `json:"min_excursion_minutes"`
	// HysteresisC 是迟滞带（℃）。
	HysteresisC float64 `json:"hysteresis_c"`
}

// Timings 是各阶段的实测耗时（秒）。
//
// 分段记耗时的用处很实在：基准超时时，报告要能指出是哪一段慢，
// 而不是只给一个「超时了」让人自己猜。
type Timings struct {
	// LoadSeconds 是读数据集与拼流的耗时。
	LoadSeconds float64 `json:"load_seconds"`
	// WindowSeconds 是抽窗口的耗时（含按产品门槛的复算）。
	WindowSeconds float64 `json:"window_seconds"`
	// AttributionSeconds 是逐窗口跑归因基线的耗时。
	AttributionSeconds float64 `json:"attribution_seconds"`
}

// Metrics 是三项主指标。
type Metrics struct {
	// WindowErrorMaxSamples 是窗口起止相对数据集标注段的最大偏差（采样条数）。
	WindowErrorMaxSamples int `json:"window_error_max_samples"`
	// WindowErrorMeanSamples 是同一偏差的平均值（采样条数）。
	WindowErrorMeanSamples float64 `json:"window_error_mean_samples"`
	// CauseTop1AgreementRate 是规则基线归因 Top-1 与数据集成因标注的一致率。
	CauseTop1AgreementRate float64 `json:"cause_top1_agreement_rate"`
	// CauseTop1HitRate 是规则基线归因 Top-1 落在数据集成因标注集合内的比例。
	CauseTop1HitRate float64 `json:"cause_top1_hit_rate"`
	// ElapsedSeconds 是本次基准的耗时（秒）。
	ElapsedSeconds float64 `json:"elapsed_seconds"`
}

// StreamReport 是逐条可比流的明细。
type StreamReport struct {
	// Name 是流的标识。
	Name string `json:"name"`
	// SetpointC 是设定点（℃）。
	SetpointC float64 `json:"setpoint_c"`
	// Zone 是温区标签（如 2~8℃）。
	Zone string `json:"zone"`
	// Records 是这条流的记录数。
	Records int `json:"records"`
	// SamplingIntervalSec 是采样周期（秒）。
	SamplingIntervalSec int `json:"sampling_interval_sec"`
	// Windows 是规则层抽出的正式窗口数。
	Windows int `json:"windows"`
	// GroundTruthSegments 是数据集标注的越限段数。
	GroundTruthSegments int `json:"ground_truth_segments"`
	// MatchedSegments 是能对上窗口的标注段数。
	MatchedSegments int `json:"matched_segments"`
	// UnmatchedSegments 是没对上任何窗口的标注段数。
	UnmatchedSegments int `json:"unmatched_segments"`
	// WindowErrorMaxSamples 是该流上的最大窗口偏差（采样条数）。
	WindowErrorMaxSamples int `json:"window_error_max_samples"`
	// WindowErrorMeanSamples 是该流上的平均窗口偏差（采样条数）。
	WindowErrorMeanSamples float64 `json:"window_error_mean_samples"`
	// ExtraWindows 是没能对上标注段的窗口数。
	ExtraWindows int `json:"extra_windows"`
	// BelowThresholdWindows 是落在「低于阈值」一侧的窗口数。
	//
	// 数据集只标注了「高于阈值」（time_below_threshold_min 全为 0），
	// 这一侧没有真值可比，单列计数，不混进窗口误差。
	BelowThresholdWindows int `json:"below_threshold_windows"`
	// PolicyWindowsAtProductGate 是按产品门槛（最短越限 1 分钟）复算时成立为正式窗口的段数。
	PolicyWindowsAtProductGate int `json:"policy_windows_at_product_gate"`
	// PolicyTransientsAtProductGate 是同一门槛下属短时波动的段数。
	PolicyTransientsAtProductGate int `json:"policy_transients_at_product_gate"`
	// CauseLabeledWindows 是带数据集成因标注的窗口数。
	CauseLabeledWindows int `json:"cause_labeled_windows"`
	// CauseSingleLabelWindows 是成因标注唯一的窗口数（一致率的分母）。
	CauseSingleLabelWindows int `json:"cause_single_label_windows"`
	// CauseTop1Matched 是 Top-1 与唯一成因标注完全一致的窗口数。
	CauseTop1Matched int `json:"cause_top1_matched"`
	// CauseTop1Hit 是 Top-1 落在成因标注集合内的窗口数。
	CauseTop1Hit int `json:"cause_top1_hit"`
	// Top1Distribution 是规则基线 Top-1 原因的分布。
	Top1Distribution map[string]int `json:"top1_distribution"`
	// DatasetCauseDistribution 是数据集成因标注的分布。
	DatasetCauseDistribution map[string]int `json:"dataset_cause_distribution"`
	// WindowSeconds 是这条流上抽窗口的耗时（秒）。
	WindowSeconds float64 `json:"window_seconds"`
	// AttributionSeconds 是这条流上逐窗口归因的耗时（秒）。
	AttributionSeconds float64 `json:"attribution_seconds"`
}

// ProductGateMinutes 是产品的最短越限门槛（分钟），用于复算口径对照。
//
// 基准本身不设门槛（见 Options.MinExcursionMinutes），但报告里要说明：
// 同一批数据按产品门槛算会分成多少正式窗口、多少短时波动。
const ProductGateMinutes = 1

// indexSpan 是一段连续采样的下标区间（闭区间）。
type indexSpan struct {
	first int
	last  int
}

// RunPipeline 读数据集并跑一遍基准，返回报告。
//
// 耗时从**读文件**那一步起算：40 万条记录要在 30 秒内跑完，
// 读那 270 MB CSV 占掉大头，只算指标计算的时间等于把大头藏起来。
func RunPipeline(datasetPath string, opts Options) (*Report, error) {
	started := time.Now()
	ds, err := LoadDataset(datasetPath, opts)
	if err != nil {
		return nil, err
	}
	loadElapsed := roundSeconds(time.Since(started))
	report, err := Run(ds, opts)
	if err != nil {
		return nil, err
	}
	report.Timings.LoadSeconds = loadElapsed
	report.Metrics.ElapsedSeconds = roundSeconds(time.Since(started))
	// 口径说明里带耗时构成，读数耗时这时才刚知道——重算一遍说明。
	report.Notes = buildNotes(ds, report)
	return report, nil
}

// Run 在已读入的数据集上跑一遍基准，返回报告。
//
// 三项主指标的口径：
//   - **窗口误差**：数据集标注的每个越限段，对上规则层抽出的那条窗口，
//     窗口起点与标注段起点的偏差、终点与标注段终点的偏差，
//     各自折算成「差了几条采样点」，取两者较大者作为该段的窗口误差。
//   - **归因 Top-1 一致率**：窗口内数据集自带的成因标注唯一时，
//     规则基线 Top-1 与它相同即算一致；标注有多个时，Top-1 落在集合内即算命中。
//   - **耗时**：读数 + 抽窗口 + 逐窗口归因的总耗时。
func Run(ds *Dataset, opts Options) (*Report, error) {
	if err := opts.normalize(); err != nil {
		return nil, err
	}
	started := time.Now()

	report := &Report{
		Dataset:     "vcc-cpld",
		DatasetPath: ds.Path,
		Records:     ds.Records,
		GeneratedAt: started.Format(time.RFC3339),
		Options: OptionsReport{
			ToleranceC:           opts.ToleranceC,
			AnnotationToleranceC: ds.AnnotationToleranceC,
			ContextSamples:       opts.ContextSamples,
			MinExcursionMinutes:  opts.MinExcursionMinutes,
			HysteresisC:          opts.HysteresisC,
		},
		Streams: []StreamReport{},
		Notes:   []string{},
	}

	totalErr := 0
	singleLabel := 0
	top1Matched := 0
	top1Hit := 0
	labeledWindows := 0

	for i := range ds.Streams {
		stream := &ds.Streams[i]
		sr, err := evaluateStream(stream, opts)
		if err != nil {
			return nil, err
		}
		report.Streams = append(report.Streams, *sr)

		totalErr += sr.WindowErrorMaxSamples
		singleLabel += sr.CauseSingleLabelWindows
		top1Matched += sr.CauseTop1Matched
		top1Hit += sr.CauseTop1Hit
		labeledWindows += sr.CauseLabeledWindows
	}

	report.Metrics = Metrics{
		WindowErrorMaxSamples:  maxInt(totalErr, 0),
		WindowErrorMeanSamples: meanWindowError(report.Streams),
		CauseTop1AgreementRate: rate(top1Matched, singleLabel),
		CauseTop1HitRate:       rate(top1Hit, labeledWindows),
	}
	for _, s := range report.Streams {
		report.Timings.WindowSeconds += s.WindowSeconds
		report.Timings.AttributionSeconds += s.AttributionSeconds
	}
	report.Timings.WindowSeconds = roundTwo(report.Timings.WindowSeconds)
	report.Timings.AttributionSeconds = roundTwo(report.Timings.AttributionSeconds)
	report.Metrics.ElapsedSeconds = roundSeconds(time.Since(started))

	report.Notes = buildNotes(ds, report)
	return report, nil
}

// evaluateStream 在一条可比流上跑窗口抽取与归因基线。
func evaluateStream(stream *Stream, opts Options) (*StreamReport, error) {
	zone := buildZone(stream, opts)

	windowStart := time.Now()
	windows, err := rules.ExtractWindows(stream.Samples, zone)
	if err != nil {
		return nil, err
	}

	sr := &StreamReport{
		Name:                     stream.Name,
		SetpointC:                stream.SetpointC,
		Zone:                     rules.ZoneLabel(stream.ZoneMinC, stream.ZoneMaxC),
		Records:                  len(stream.Samples),
		SamplingIntervalSec:      stream.SamplingIntervalSec,
		Windows:                  len(windows),
		Top1Distribution:         map[string]int{},
		DatasetCauseDistribution: map[string]int{},
	}

	positions := indexPositions(stream.Samples)
	segments := labeledSpans(stream.Labeled)
	sr.GroundTruthSegments = len(segments)

	// 窗口与标注段的配对：按重叠最多的那条窗口配对。
	//
	// 不用「窗口是否覆盖标注段首条」来配对：边界差一条采样时窗口会整体挪开一格，
	// 首条就落在窗口外面了——那是 1 个采样周期的偏差，不是「没对上」。
	// 按重叠配对，边界差一格照样算对上，偏差记 1。
	errSum := 0
	for _, seg := range segments {
		idx, ok := findWindowByOverlap(windows, positions, seg)
		if !ok {
			sr.UnmatchedSegments++
			continue
		}
		sr.MatchedSegments++
		startErr := absInt(indexOf(positions, windows[idx].Start) - seg.first)
		endErr := absInt(indexOf(positions, windows[idx].End) - seg.last)
		segErr := maxInt(startErr, endErr)
		if segErr > sr.WindowErrorMaxSamples {
			sr.WindowErrorMaxSamples = segErr
		}
		errSum += segErr
	}
	if sr.MatchedSegments > 0 {
		sr.WindowErrorMeanSamples = roundTwo(float64(errSum) / float64(sr.MatchedSegments))
	}
	sr.ExtraWindows = len(windows) - sr.MatchedSegments

	// 按产品门槛复算一次：同一批数据在「最短越限 1 分钟」下会分成多少正式窗口、
	// 多少短时波动。这不是基准指标，是给读报告的人看的一段口径对照——
	// 数据集的标注粒度是 1 分钟，与产品的门槛口径差着一个量级，不说清楚容易误读。
	policyZone := zone
	policyZone.MinExcursionMinutes = ProductGateMinutes
	policyWindows, err := rules.ExtractWindows(stream.Samples, policyZone)
	if err != nil {
		return nil, err
	}
	transients, err := rules.ExtractTransientExcursions(stream.Samples, policyZone)
	if err != nil {
		return nil, err
	}
	sr.PolicyWindowsAtProductGate = len(policyWindows)
	sr.PolicyTransientsAtProductGate = len(transients)
	sr.WindowSeconds = roundSeconds(time.Since(windowStart))

	// 逐窗口归因，并与数据集的成因标注比对。
	attributionStart := time.Now()
	for idx := range windows {
		window := windows[idx]
		startIdx, okStart := indexOfOK(positions, window.Start)
		endIdx, okEnd := indexOfOK(positions, window.End)
		if !okStart || !okEnd {
			continue
		}
		labels := windowCauseLabels(stream, startIdx, endIdx)
		for _, cause := range labels {
			sr.DatasetCauseDistribution[cause]++
		}
		// 低于阈值一侧：数据集没有这一侧的标注，只能单列计数。
		// 判据看峰值落在温区哪一侧——窗口峰值低于温区下界即为偏冷一侧。
		if window.PeakC < stream.ZoneMinC {
			sr.BelowThresholdWindows++
		}
		if len(labels) == 0 {
			continue
		}
		sr.CauseLabeledWindows++
		if len(labels) == 1 {
			sr.CauseSingleLabelWindows++
		}

		payload, err := rules.AttributeSamples(localSamples(stream, startIdx, endIdx, opts.ContextSamples),
			&window, zone, rules.AttributionOptions{})
		if err != nil {
			return nil, err
		}
		sr.Top1Distribution[payload.CauseCode]++
		if len(labels) == 1 && payload.CauseCode == labels[0] {
			sr.CauseTop1Matched++
		}
		if containsString(labels, payload.CauseCode) {
			sr.CauseTop1Hit++
		}
	}
	sr.AttributionSeconds = roundSeconds(time.Since(attributionStart))

	return sr, nil
}

// buildZone 按设定点与容差装配温区判定。
func buildZone(stream *Stream, opts Options) rules.ZoneDecision {
	return rules.ZoneDecision{
		ZoneMinC:            stream.ZoneMinC,
		ZoneMaxC:            stream.ZoneMaxC,
		ZoneLabel:           rules.ZoneLabel(stream.ZoneMinC, stream.ZoneMaxC),
		ZoneSource:          "离线基准口径",
		ZoneBasis:           fmt.Sprintf("公开数据集设定点 %.1f℃ ± %.1f℃", stream.SetpointC, opts.ToleranceC),
		SampleIntervalSec:   stream.SamplingIntervalSec,
		MinExcursionMinutes: opts.MinExcursionMinutes,
		HysteresisC:         opts.HysteresisC,
		Precision: domain.PrecisionVerdict{
			LimitC:      0.5,
			LimitSource: "记录仪规范口径",
		},
	}
}

// localSamples 取窗口前后若干条采样作为归因上下文。
func localSamples(stream *Stream, startIdx, endIdx, context int) []domain.TemperatureEvent {
	from := startIdx - context
	if from < 0 {
		from = 0
	}
	to := endIdx + context
	if to > len(stream.Samples)-1 {
		to = len(stream.Samples) - 1
	}
	return stream.Samples[from : to+1]
}

// windowCauseLabels 取窗口内数据集自带的成因标注（去重）。
func windowCauseLabels(stream *Stream, startIdx, endIdx int) []string {
	seen := map[string]bool{}
	labels := make([]string, 0, 3)
	for i := startIdx; i <= endIdx && i < len(stream.Causes); i++ {
		for _, cause := range stream.Causes[i] {
			if seen[cause] {
				continue
			}
			seen[cause] = true
			labels = append(labels, cause)
		}
	}
	sort.Strings(labels)
	return labels
}

// indexPositions 建立采样时刻到下标的映射。
func indexPositions(samples []domain.TemperatureEvent) map[time.Time]int {
	positions := make(map[time.Time]int, len(samples))
	for i := range samples {
		positions[samples[i].MeasuredAt] = i
	}
	return positions
}

// indexOf 取某个时刻对应的下标；取不到时返回 -1。
func indexOf(positions map[time.Time]int, at time.Time) int {
	if idx, ok := positions[at]; ok {
		return idx
	}
	return -1
}

// indexOfOK 取某个时刻对应的下标，并报告是否取到。
func indexOfOK(positions map[time.Time]int, at time.Time) (int, bool) {
	idx, ok := positions[at]
	return idx, ok
}

// labeledSpans 把标注位串切成连续段（闭区间下标）。
func labeledSpans(flags []bool) []indexSpan {
	spans := make([]indexSpan, 0, len(flags))
	for i := 0; i < len(flags); {
		if !flags[i] {
			i++
			continue
		}
		j := i
		for j+1 < len(flags) && flags[j+1] {
			j++
		}
		spans = append(spans, indexSpan{first: i, last: j})
		i = j + 1
	}
	return spans
}

// findWindowByOverlap 找出与标注段重叠最多的那条窗口。
//
// 重叠为 0 时视为没对上：没有窗口覆盖这条标注段，说明算法把这段越限整个漏了。
// 一段标注可能落在同一条窗口里（迟滞带把两段并成一条），
// 那时两条标注段对的是同一条窗口，误差按各自的边界算。
func findWindowByOverlap(windows []domain.ExcursionWindow, positions map[time.Time]int, seg indexSpan) (int, bool) {
	bestIdx := -1
	bestOverlap := 0
	for i := range windows {
		start, okStart := indexOfOK(positions, windows[i].Start)
		end, okEnd := indexOfOK(positions, windows[i].End)
		if !okStart || !okEnd {
			continue
		}
		overlap := minInt(end, seg.last) - maxInt(start, seg.first) + 1
		if overlap > bestOverlap {
			bestOverlap = overlap
			bestIdx = i
		}
	}
	if bestIdx < 0 {
		return 0, false
	}
	return bestIdx, true
}

// buildNotes 汇总本轮基准的口径说明与实测发现。
func buildNotes(ds *Dataset, report *Report) []string {
	notes := []string{
		"数据集每行是一次运输的遥测快照，不存在单设备连续时序；基准按设定点工况归成可比流后按时间戳拼接，只用于量算法在真实遥测数值上的行为。",
		fmt.Sprintf("数据集自带的越限阈值容差为 %.2f℃，基准配置的温区容差为 %.2f℃；两者一致时窗口才可比。",
			ds.AnnotationToleranceC, report.Options.ToleranceC),
	}
	if ds.AnnotationToleranceC > 0 && math.Abs(ds.AnnotationToleranceC-report.Options.ToleranceC) > 0.001 {
		notes = append(notes,
			"注意：配置的容差与数据集标注的阈值不一致，窗口误差指标的可比性下降——请核对 --tolerance-c 取值。")
	}

	below := 0
	for _, s := range report.Streams {
		below += s.BelowThresholdWindows
	}
	if below > 0 {
		notes = append(notes,
			fmt.Sprintf("另有 %d 条窗口落在「低于阈值」一侧：数据集未提供该侧标注（time_below_threshold_min 全为 0），单列计数，不计入窗口误差。", below))
	}

	single := 0
	for _, s := range report.Streams {
		single += s.CauseSingleLabelWindows
	}
	notes = append(notes,
		fmt.Sprintf("归因一致率的分母是 %d 条「成因标注唯一」的窗口；数据集的开门/断电/除霜是逐记录的累计口径，而规则层的开门判据要求累计开门 ≥5 分钟，两套口径不同，该指标应作为模型归因的对照基线读，不作为及格线。", single))

	// 耗时构成：抽窗口占了大头时，要说清是哪一段占的。
	if report.Timings.WindowSeconds > 0 && report.Metrics.ElapsedSeconds > 0 {
		share := report.Timings.WindowSeconds / report.Metrics.ElapsedSeconds * 100
		notes = append(notes,
			fmt.Sprintf("耗时构成：读数 %.2fs、抽窗口 %.2fs（占 %.0f%%）、逐窗口归因 %.2fs。窗口抽取为每条窗口重算一遍整段温升速率，规模大时该项随「窗口数 × 采样数」增长——离线批量复算要留意。",
				report.Timings.LoadSeconds, report.Timings.WindowSeconds, share, report.Timings.AttributionSeconds))
	}
	return notes
}

// meanWindowError 按匹配段数加权平均各流的窗口误差。
func meanWindowError(streams []StreamReport) float64 {
	sum := 0.0
	count := 0
	for _, s := range streams {
		sum += s.WindowErrorMeanSamples * float64(s.MatchedSegments)
		count += s.MatchedSegments
	}
	if count == 0 {
		return 0
	}
	return roundTwo(sum / float64(count))
}

// rate 计算比例；分母为 0 时返回 0（并在报告里由分母字段说明样本量）。
func rate(part, total int) float64 {
	if total == 0 {
		return 0
	}
	return roundTwo(float64(part) / float64(total))
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func roundTwo(v float64) float64 {
	return math.Round(v*100) / 100
}

func roundSeconds(d time.Duration) float64 {
	return math.Round(d.Seconds()*100) / 100
}

// DeterministicSignature 取出报告里的确定性指标，用于重复运行比对。
//
// 耗时不参与：机器快慢不同，耗时本就不该一致；
// 窗口误差与归因一致率才是「同一数据集重复运行必须一致」的那部分。
func DeterministicSignature(report *Report) string {
	return fmt.Sprintf("window_error_max=%d|window_error_mean=%.2f|top1_agreement=%.2f|top1_hit=%.2f",
		report.Metrics.WindowErrorMaxSamples,
		report.Metrics.WindowErrorMeanSamples,
		report.Metrics.CauseTop1AgreementRate,
		report.Metrics.CauseTop1HitRate)
}

// VerifyAnnotationTolerance 校验配置的容差与数据集自带的阈值容差是否一致。
//
// 不一致时基准仍然能跑，但窗口误差就失去了可比性——
// 这时要显式说出来，而不是让人以为「误差 0 就是准」。
func VerifyAnnotationTolerance(ds *Dataset, opts Options) error {
	if ds.AnnotationToleranceC <= 0 {
		return errcode.Newf(errcode.Validation, "数据集未提供可用的越限阈值标注，无法核对容差口径")
	}
	if math.Abs(ds.AnnotationToleranceC-opts.ToleranceC) > 0.001 {
		return errcode.Newf(errcode.Validation,
			"温区容差 %.2f℃ 与数据集自带的越限阈值容差 %.2f℃ 不一致，窗口误差不可比",
			opts.ToleranceC, ds.AnnotationToleranceC)
	}
	return nil
}
