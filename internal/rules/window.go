package rules

import (
	"sort"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// excursionSide 表示越限发生在温区的哪一侧。
type excursionSide int

const (
	// sideHigh：读数越过温区上界（偏热）。
	sideHigh excursionSide = iota
	// sideLow：读数低于温区下界（偏冷）。
	sideLow
)

// excursionSegment 是一次扫描找到的越限段。
//
// 一段越限可能由好几截连续越限拼成——中间那次短暂回落只要没跌出迟滞带，
// 就仍然算同一次异常。这种拼接的次数要记下来：合并次数是复核时
// 「这段异常为什么这么长」的答案。
type excursionSegment struct {
	// firstIdx / lastIdx 是这段越限的首末**越限采样点**在有序序列中的下标。
	firstIdx int
	lastIdx  int
	// side 是这段越限所在的一侧。
	side excursionSide
	// segments 是被合并进本段的连续越限段数（含第一段）；1 表示没有发生合并。
	segments int
	// outOfRangeCount 是窗口内实际越限的读数条数。
	outOfRangeCount int
}

// ExcursionSummary 是一次扫描后这批采样里的越限情况汇总。
//
// 它回答的是判定之后药师最先问的两句话：越限了几次、最长的一次多久。
// 没有它，只看「窗口为 null」根本分不出「压根没越限」和「越了但没够门槛」。
type ExcursionSummary struct {
	// SegmentCount 是扫出的越限段数。
	//
	// 「段」= 一次连续异常（被迟滞带托住、又被合并进来的算同一段）。
	SegmentCount int `json:"segment_count"`
	// OutOfRangeSamples 是这批采样里实际越限的读数条数。
	OutOfRangeSamples int `json:"out_of_range_samples"`
	// LongestConfirmedMinutes 是最长一段的确认时长（分钟）。
	LongestConfirmedMinutes int `json:"longest_confirmed_minutes"`
	// MinExcursionMinutes 是本次判定采用的「最短越限时长」门槛（分钟）。
	MinExcursionMinutes int `json:"min_excursion_minutes"`
	// IntervalSource 是本次折算采用的采样周期来源。
	IntervalSource string `json:"interval_source,omitempty"`
	// DataGapCount 是这批采样里的数据缺口数。
	DataGapCount int `json:"data_gap_count,omitempty"`
	// ClockRegressionCount 是这批采样里的时钟回拨次数。
	ClockRegressionCount int `json:"clock_regression_count,omitempty"`
	// ProbeStuckSampleCount 是落在探头卡死段里的采样条数。
	ProbeStuckSampleCount int `json:"probe_stuck_sample_count,omitempty"`
	// WindowCount 是这批采样里成立的正式窗口数（相邻越限已合并）。
	WindowCount int `json:"window_count"`
	// MergedWindowMinutes 是最长的一条正式窗口的持续分钟（各段越限时长之和）。
	//
	// 它与 LongestConfirmedMinutes 的差别正是「合并」这件事本身：
	// 单段窗口时两者相等；多段合并时，前者是整条窗口的时长，
	// 后者只是其中最长的一段。
	MergedWindowMinutes int `json:"merged_window_minutes"`
	// TransientSegmentCount 是未达门槛的短时波动段数。
	TransientSegmentCount int `json:"transient_segment_count,omitempty"`
	// TransientSamples 是落在短时波动里的越限读数条数。
	TransientSamples int `json:"transient_samples,omitempty"`
	// LongestTransientMinutes 是最长的一段短时波动时长（分钟）。
	LongestTransientMinutes int `json:"longest_transient_minutes,omitempty"`
}

// MeetsThreshold 判断最长的一段是否达到了「最短越限时长」门槛。
func (s ExcursionSummary) MeetsThreshold() bool {
	return s.SegmentCount > 0 && s.LongestConfirmedMinutes >= s.MinExcursionMinutes
}

// ExtractWindow 从温度采样序列中抽取越温窗口（双阈值迟滞版）。
//
// 三道阈值一起定这件事：
//
//  1. **进入阈值**：连续超限要达到 min_excursion_minutes 才成立窗口。
//     一次读数超了 0.4℃ 是波动，连着超够时长才是越温——这两件事对批次处置
//     的含义完全不同，不能混成一条结论。
//  2. **退出阈值**：要回落到 (zone_max_c - hysteresis_c) 以内才算回到温区。
//     迟滞带（比如温区 2~8℃、迟滞 1℃，即回落到 7℃ 及以下）的作用是消抖：
//     空调启停造成的贴边抖动，会把一次真实断电切成好几段短窗口，
//     而每段又都因为「不够长」判不出来——一次断电反倒什么结论都没有。
//  3. **合并规则**：读数落回迟滞带但没跌出越限状态、随后又冲出去时，
//     两段算同一个窗口，并记下合并次数。
//
// 窗口起止落在**真实越限采样点**上：第一段越限的首点、最后一段越限的末点。
// 窗口要指认的是「哪几个读数真的越限了」，复核时能对着记录仪导出的表一条条核；
// 落在迟滞带边界上的窗口核不出来。
//
// 确认时长不达标的不成立窗口——那是一段波动，不是越温
// （未达门槛的短时波动由 ExtractTransientExcursions 单独归集）。
//
// 紧邻的两段达标越限之间若只回落了不足门槛时长，会先合并成一条窗口再返回
// （合并规则见 transient.go 的 classifyExcursions）。
//
// 一次抽取只给一条窗口：按采样时刻取第一条成立的窗口。
// 要看全部窗口用 ExtractWindows，要看短时波动用 ExtractTransientExcursions。
//
// 温区判定不可用（配置缺失、点位对不上、设备归属歧义）时返回错误且不产出窗口：
// 窗口是按温区算出来的，温区不可信时算出来的窗口看着正常、实则整个是错的。
// 这条闸门放在窗口计算这一步，而不是散在各调用方的 if 里——
// 谁忘了检查 needs_manual，这里就产不出东西，而不是产出一个假的。
func ExtractWindow(samples []domain.TemperatureEvent, zone ZoneDecision) (*domain.ExcursionWindow, error) {
	windows, err := ExtractWindows(samples, zone)
	if err != nil {
		return nil, err
	}
	if len(windows) == 0 {
		return nil, nil
	}
	return &windows[0], nil
}

// SummarizeExcursions 汇总这批采样里的越限情况。
//
// 它与 ExtractWindow 用的是同一套扫描逻辑，两边对「哪几段越限」的判断永远一致；
// 区别只在于本函数连未达门槛的短段也一并统计出来——
// 那些短段是「波动」，要单独归集给药师看，不能当成「没发生过越限」。
//
// 汇总里同时给出窗口数与短时波动数：这两样一起看，
// 才知道「这批读数里到底有几件事发生过」。
func SummarizeExcursions(samples []domain.TemperatureEvent, zone ZoneDecision) (ExcursionSummary, error) {
	scan, err := scanAll(samples, zone)
	if err != nil {
		return ExcursionSummary{}, err
	}
	summary := ExcursionSummary{
		MinExcursionMinutes:  zone.MinExcursionMinutes,
		IntervalSource:       scan.intervalSource,
		DataGapCount:         len(scan.gaps),
		ClockRegressionCount: len(scan.regressions),
	}
	for _, span := range scan.stuck {
		summary.ProbeStuckSampleCount += span.SampleCount
	}
	for _, seg := range scan.segments {
		minutes := int(scan.spanMinutes(seg))
		summary.SegmentCount++
		summary.OutOfRangeSamples += seg.outOfRangeCount
		if minutes > summary.LongestConfirmedMinutes {
			summary.LongestConfirmedMinutes = minutes
		}
	}

	// 窗口数与短时波动数：与窗口抽取走同一遍分类，两边不会打架。
	cls := classifyExcursions(scan, zone)
	summary.WindowCount = len(cls.groups)
	for _, group := range cls.groups {
		minutes := 0
		for _, seg := range group {
			minutes += int(scan.spanMinutes(seg))
		}
		if minutes > summary.MergedWindowMinutes {
			summary.MergedWindowMinutes = minutes
		}
	}
	for _, transient := range cls.transients {
		summary.TransientSegmentCount++
		summary.TransientSamples += transient.OutOfRangeSamples
		if transient.DurationMinutes > summary.LongestTransientMinutes {
			summary.LongestTransientMinutes = transient.DurationMinutes
		}
	}
	return summary, nil
}

// excursionScan 是一次扫描的结果：排好序的采样、扫出的越限段、采样周期，
// 以及这批读数本身的三类异常（数据缺口、时钟回拨、探头卡死）。
type excursionScan struct {
	ordered []domain.TemperatureEvent
	// logical 是修正后的时间轴；仅在确认时钟回拨时启用，其余情况与 measured_at 一致。
	logical  []time.Time
	segments []excursionSegment
	// interval 是本次采用的采样周期；intervalSource 说明它从哪儿来；
	// inferredInterval 是由数据推断出的周期（可能与配置不一致）。
	interval         time.Duration
	intervalSource   string
	inferredInterval time.Duration
	// gaps 是数据缺口；regressions 是时钟回拨；stuck 是探头卡死段。
	gaps        []dataGap
	regressions []domain.ClockRegression
	clockBroken bool
	stuck       []domain.ProbeStuckSpan
	// stuckIdx 标出落在卡死段里的采样下标。
	stuckIdx []bool
	zone     ZoneDecision
}

// scanAll 排好序并扫出全部越限段。
func scanAll(samples []domain.TemperatureEvent, zone ZoneDecision) (excursionScan, error) {
	if !zone.Usable() {
		reason := zone.Reason
		if reason == "" {
			reason = "温区判定不可用"
		}
		return excursionScan{}, errcode.Newf(errcode.Validation,
			"温区判定不可用，不产出越温窗口：%s", reason)
	}
	if len(samples) == 0 {
		return excursionScan{}, nil
	}

	ordered := make([]domain.TemperatureEvent, len(samples))
	copy(ordered, samples)

	// 一、时钟回拨必须在**记录仪序**上认：按 measured_at 排序会把倒退藏起来。
	interval, source, inferred := resolveEffectiveInterval(zone, ordered)
	regressions, clockBroken := detectClockRegressions(ordered, interval)

	if !clockBroken {
		// 没有回拨：按采样时刻排序归位（导出文件乱序、并发投递乱序都要归位）。
		sort.SliceStable(ordered, func(i, j int) bool {
			return ordered[i].MeasuredAt.Before(ordered[j].MeasuredAt)
		})
		interval, source, inferred = resolveEffectiveInterval(zone, ordered)
	}

	scan := excursionScan{
		ordered:          ordered,
		interval:         interval,
		intervalSource:   source,
		inferredInterval: inferred,
		regressions:      regressions,
		clockBroken:      clockBroken,
		zone:             zone,
	}
	if clockBroken {
		// 确认回拨：保留记录仪序，并重建一条单调时间轴供窗口起止使用。
		scan.logical = repairTimeline(ordered, interval)
	}
	scan.gaps = detectDataGaps(ordered, interval)
	scan.stuck = detectProbeStuck(ordered, zone, interval)
	scan.stuckIdx = markStuckIndexes(ordered, scan.stuck)
	scan.segments = scanSegments(ordered, zone)
	return scan, nil
}

// markStuckIndexes 标出落在探头卡死段里的采样下标。
func markStuckIndexes(ordered []domain.TemperatureEvent, spans []domain.ProbeStuckSpan) []bool {
	flags := make([]bool, len(ordered))
	if len(spans) == 0 {
		return flags
	}
	for i := range ordered {
		for _, span := range spans {
			if ordered[i].MeasuredAt.Before(span.From) || ordered[i].MeasuredAt.After(span.To) {
				continue
			}
			flags[i] = true
			break
		}
	}
	return flags
}

// scanSegments 按采样时刻扫出全部越限段。
//
// 返回的段按发生先后排列；相邻两段之间由迟滞带判定是否算同一次异常。
func scanSegments(ordered []domain.TemperatureEvent, zone ZoneDecision) []excursionSegment {
	segments := make([]excursionSegment, 0, 4)
	i := 0
	for i < len(ordered) {
		if !zone.OutOfRange(ordered[i].TemperatureC) {
			i++
			continue
		}
		seg, next := scanSegment(ordered, i, zone)
		segments = append(segments, seg)
		if next <= i {
			next = i + 1
		}
		i = next
	}
	return segments
}

// scanSegment 从 start（一个越限采样点）起扫出一段越限。
//
// 返回该段与下一个待考察的下标：下一段越限从这里接着扫，
// 已在温区内的采样则由外层循环跳过。
func scanSegment(ordered []domain.TemperatureEvent, start int, zone ZoneDecision) (excursionSegment, int) {
	side := sideOf(ordered[start].TemperatureC, zone)
	seg := excursionSegment{
		firstIdx:        start,
		lastIdx:         start,
		side:            side,
		segments:        1,
		outOfRangeCount: 1,
	}
	// inBand 表示当前处于「落回迟滞带但越限状态还没结束」的托住状态。
	inBand := false

	j := start + 1
	for j < len(ordered) {
		temperatureC := ordered[j].TemperatureC
		switch {
		case zone.OutOfRange(temperatureC):
			if sideOf(temperatureC, zone) != side {
				// 越到了温区另一侧：本次异常到此为止，那一侧另算一段。
				return seg, j
			}
			if inBand {
				// 从迟滞带里又冲出去了：算合并进本段。
				seg.segments++
				inBand = false
			}
			seg.lastIdx = j
			seg.outOfRangeCount++
			j++
		case holdsOpen(temperatureC, side, zone):
			// 落回迟滞带但没跌出越限状态：异常还没结束，
			// 但这条读数不算越限，不计入越限条数。
			inBand = true
			j++
		default:
			// 回到温区：异常结束。
			return seg, j
		}
	}
	return seg, j
}

// sideOf 判断读数越的是温区哪一侧。
func sideOf(temperatureC float64, zone ZoneDecision) excursionSide {
	if temperatureC > zone.ZoneMaxC {
		return sideHigh
	}
	return sideLow
}

// holdsOpen 判断一条已在温区内的读数是否仍然「托住」本次异常。
//
// 上侧异常要回落到 (zone_max_c - hysteresis_c) 才算结束；
// 下侧异常要回升到 (zone_min_c + hysteresis_c) 才算结束。
// 迟滞带为 0 时，判据退回到温区边界本身，与「温区是闭区间」的口径一致。
func holdsOpen(temperatureC float64, side excursionSide, zone ZoneDecision) bool {
	switch side {
	case sideHigh:
		return temperatureC > zone.ZoneMaxC-zone.HysteresisC
	default:
		return temperatureC < zone.ZoneMinC+zone.HysteresisC
	}
}

// logicalAt 取某个下标在本次判定用的时间轴上的时刻。
//
// 确认时钟回拨时用修正后的单调时间轴；否则就是原始上报时刻。
func (s excursionScan) logicalAt(idx int) time.Time {
	if idx < 0 || idx >= len(s.ordered) {
		return time.Time{}
	}
	if s.clockBroken && idx < len(s.logical) {
		return s.logical[idx]
	}
	return s.ordered[idx].MeasuredAt
}

// gapsInRange 取一段下标范围内的数据缺口。
func (s excursionScan) gapsInRange(firstIdx, lastIdx int) []dataGap {
	out := make([]dataGap, 0, 2)
	for _, g := range s.gaps {
		if g.beforeIdx < firstIdx || g.afterIdx > lastIdx {
			continue
		}
		out = append(out, g)
	}
	return out
}

// stuckInRange 取一段下标范围内的探头卡死段。
func (s excursionScan) stuckInRange(firstIdx, lastIdx int) []domain.ProbeStuckSpan {
	out := make([]domain.ProbeStuckSpan, 0, 1)
	for _, span := range s.stuck {
		inRange := false
		for i := firstIdx; i <= lastIdx; i++ {
			if s.stuckIdx[i] {
				inRange = true
				break
			}
		}
		if inRange {
			out = append(out, span)
		}
	}
	return out
}

// regressionsInRange 取一段下标范围内的时间轴异常。
func (s excursionScan) regressionsInRange(firstIdx, lastIdx int) []domain.ClockRegression {
	out := make([]domain.ClockRegression, 0, 1)
	for _, r := range s.regressions {
		if r.Index < firstIdx || r.Index > lastIdx {
			continue
		}
		out = append(out, r)
	}
	return out
}

// peakInRange 取一段下标范围内偏离温区最远的那条读数（峰值）及其时刻。
//
// 上侧异常取最高读数，下侧异常取最低读数；同值时取最早出现的那一点，
// 保证结论可复现。
func (s excursionScan) peakInRange(firstIdx, lastIdx int, side excursionSide) (float64, time.Time) {
	if firstIdx < 0 || firstIdx >= len(s.ordered) {
		return 0, time.Time{}
	}
	peakIdx := firstIdx
	for i := firstIdx + 1; i <= lastIdx && i < len(s.ordered); i++ {
		if side == sideHigh {
			if s.ordered[i].TemperatureC > s.ordered[peakIdx].TemperatureC {
				peakIdx = i
			}
			continue
		}
		if s.ordered[i].TemperatureC < s.ordered[peakIdx].TemperatureC {
			peakIdx = i
		}
	}
	return s.ordered[peakIdx].TemperatureC, s.logicalAt(peakIdx)
}

// spanMinutes 是一段越限的确认时长（分钟）。
//
// 折算口径分三种：
//
//  1. 采样周期已解析出来：按采样点数折算，窗口内有 n 条采样就是 (n-1) 个周期。
//     少算的那一个周期是「这段读数从什么时候开始越限」的不确定度。
//     数据缺口那段时间**不计入**——按墙钟折算会把断线算成越温，
//     记录仪掉线两小时，一次断电就被算成两小时越温。
//     缺口本身不在时长里消失：它在窗口上单独标成断点，
//     缺了多久、缺了几条采样都写清楚，由复核的人判断那段时间发生了什么。
//  2. 时钟回拨已确认：时间轴本身不成立，同样按采样点数折算。
//     按墙钟折算会算出负数时长，那不是结论，是噪音。
//  3. 采样周期未解析出来：退回墙钟差值（兜底，偏宽松）。
func (s excursionScan) spanMinutes(seg excursionSegment) float64 {
	span := s.ordered[seg.firstIdx : seg.lastIdx+1]
	if len(span) <= 1 {
		return 0
	}
	if s.interval > 0 {
		return float64(len(span)-1) * s.interval.Minutes()
	}
	return span[len(span)-1].MeasuredAt.Sub(span[0].MeasuredAt).Minutes()
}

// buildWindow 由一组（相邻合并后的）越限段构造窗口。
//
// group 长度为 1 时就是普通窗口；长度大于 1 时说明这几段紧邻、
// 且中间只回落了不足门槛时长，被判为同一次异常：
//
//   - 持续分钟 = 各段越限时长之和（回落时间不算越温时间）；
//   - 回落分钟单独记在 recovery_minutes，供复核时判断设备是不是在反复启停；
//   - 峰值取整条窗口里偏离温区最远的那条读数，peak_at 指向那条真实采样；
//   - 分段明细保留在 segments 里，一段一条。
//
// 口径提醒：持续分钟 + 回落分钟 ≤ (采样条数 - 1) × 采样周期。
// 差额是「越限→回落」「回落→越限」两段过渡的不确定度——
// 现场只知道下一次采样时冰箱已经安全了，不知道它具体什么时候降回来的。
func buildWindow(scan excursionScan, group []excursionSegment, zone ZoneDecision) *domain.ExcursionWindow {
	if len(group) == 0 {
		return nil
	}
	first := group[0]
	last := group[len(group)-1]
	span := scan.ordered[first.firstIdx : last.lastIdx+1]
	gaps := scan.gapsInRange(first.firstIdx, last.lastIdx)
	window := &domain.ExcursionWindow{
		Start:               scan.logicalAt(first.firstIdx),
		End:                 scan.logicalAt(last.lastIdx),
		Zone:                zone.ZoneLabel,
		SampleCount:         len(span),
		HysteresisC:         zone.HysteresisC,
		MinExcursionMinutes: zone.MinExcursionMinutes,
		SamplingIntervalSec: int(scan.interval.Seconds()),
		IntervalSource:      scan.intervalSource,
		InferredIntervalSec: int(scan.inferredInterval.Seconds()),
		BreakpointCount:     len(gaps),
		SegmentCount:        len(group),
	}
	for _, g := range gaps {
		window.DataGaps = append(window.DataGaps, g.DataGap)
	}
	window.ClockRegressions = scan.regressionsInRange(first.firstIdx, last.lastIdx)
	window.ProbeStuckSpans = scan.stuckInRange(first.firstIdx, last.lastIdx)

	segments := make([]domain.WindowSegment, 0, len(group))
	for i, seg := range group {
		minutes := int(scan.spanMinutes(seg))
		window.DurationMinutes += minutes
		window.OutOfRangeSamples += seg.outOfRangeCount
		window.MergeCount += seg.segments - 1

		segPeakC, segPeakAt := scan.peakInRange(seg.firstIdx, seg.lastIdx, seg.side)
		recoveryAfter := 0
		if i < len(group)-1 {
			recoveryAfter = int(scan.recoveryMinutes(seg, group[i+1]))
			window.RecoveryMinutes += recoveryAfter
		}
		segments = append(segments, domain.WindowSegment{
			Start:                scan.logicalAt(seg.firstIdx),
			End:                  scan.logicalAt(seg.lastIdx),
			DurationMinutes:      minutes,
			PeakC:                segPeakC,
			PeakAt:               segPeakAt,
			SampleCount:          seg.lastIdx - seg.firstIdx + 1,
			OutOfRangeSamples:    seg.outOfRangeCount,
			RecoveryAfterMinutes: recoveryAfter,
		})
	}
	window.Segments = segments

	// 峰值取整条窗口里偏离温区最远的那条读数：上侧异常取最高读数，
	// 下侧异常取最低读数。同值时取最早出现的那一点，保证结论可复现。
	window.PeakC, window.PeakAt = scan.peakInRange(first.firstIdx, last.lastIdx, first.side)

	rate := scan.riseRate(excursionSegment{firstIdx: first.firstIdx, lastIdx: last.lastIdx, side: first.side})
	window.RiseRate = &rate
	return window
}
