package rules

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
)

// 本文件是越温窗口抽取的「边界与异常」处理。
//
// 现场记录仪给出来的数据不是规整的时间序列。它会：
//   - 掉线几十分钟再补传（数据缺口）；
//   - 换电池后时钟回到过去（时钟回拨）；
//   - 探头接触不良，读数一动不动（探头卡死）；
//   - 导出文件顺序乱、并发投递顺序乱（乱序）。
//
// 这四种情况有一个共同点：**都可能被当成一段真实越温**。
// 缺口两头的读数接起来是一次凭空造出来的温升，
// 回拨后的排序会把先发生的高温排到后发生的低温后面，
// 卡死的读数会让「温升多快」这个归因特征整个失真。
//
// 所以本文件把每一种都单独认出来、单独标在窗口上，
// 而不是把它们悄悄算进一个看着正常的结论里。

// gapIntervalFactor 是缺口判定倍数：相邻采样间隔超过 3 个采样周期即视为缺口。
//
// 三倍不是拍脑袋：记录仪每 5 分钟记一次，丢一两个点通常只是网关抖动，
// 丢到三倍周期（15 分钟）以上就说明这段时间确实没有数据。
// 缺口必须显式标出来，而不是拿两头的读数连线补上。
const gapIntervalFactor = 3

// stuckMinRun 是探头卡死的判定条数：连续这么多条读数数值完全相同才算卡死。
//
// 取 3 条（两个周期）是个保守口径：只卡两条时，一次重复上报就能凑够，
// 而重复上报在网关重试时非常常见。
const stuckMinRun = 3

// overlapTolerance 是时间轴重叠的判定系数。
//
// 排序后的墙钟跨度短于「按采样周期估算的覆盖长度」到这个比例以下，
// 就说明序列自身重叠了——时钟回拨过，同一段时间被报了两次。
const overlapTolerance = 0.8

// 采样周期来源常量（与 domain 同名常量对齐，规则层直接引用）。
const (
	// IntervalSourcePointProfile：采样周期取自点位温区配置的标称值。
	IntervalSourcePointProfile = domain.IntervalSourcePointProfile
	// IntervalSourceInferred：点位未配置标称周期，由相邻采样间隔中位数推断。
	IntervalSourceInferred = domain.IntervalSourceInferred
	// IntervalSourceWallClock：周期无从解析，退回墙钟折算。
	IntervalSourceWallClock = domain.IntervalSourceWallClock
)

// dataGap 是一条缺口，以及它在有序序列中的位置。
type dataGap struct {
	domain.DataGap
	// beforeIdx 是缺口前那条采样的下标；afterIdx 是缺口后那条采样的下标。
	beforeIdx int
	afterIdx  int
}

// inferSamplingInterval 由相邻采样间隔的中位数推断采样周期。
//
// 取中位数而不是均值：一次数据缺口能把均值拉长一大截，
// 5 分钟一采的记录仪只要缺一次，均值就变成 10 分钟，
// 后面「几个周期算一次缺口」全跟着错。中位数对少数离群间隔不敏感。
//
// 间隔非正（时钟回拨处）不计入：那是钟不准，不是采样节奏变了。
func inferSamplingInterval(ordered []domain.TemperatureEvent) time.Duration {
	deltas := make([]time.Duration, 0, len(ordered))
	for i := 1; i < len(ordered); i++ {
		d := ordered[i].MeasuredAt.Sub(ordered[i-1].MeasuredAt)
		if d > 0 {
			deltas = append(deltas, d)
		}
	}
	if len(deltas) == 0 {
		return 0
	}
	sort.Slice(deltas, func(i, j int) bool { return deltas[i] < deltas[j] })
	mid := len(deltas) / 2
	if len(deltas)%2 == 1 {
		return deltas[mid]
	}
	return (deltas[mid-1] + deltas[mid]) / 2
}

// resolveEffectiveInterval 取本次判定实际采用的采样周期、它的来源与推断值。
//
// 点位配置了标称周期就用配置的：那是约定，冷链台账与记录仪都按它对账。
// 没配置才由数据推断——现场点位配置常常缺这一项，
// 但「多久记一次」这件事记录仪自己知道，从数据里问出来比等配置补齐靠得住。
//
// 两个都没有时返回 0，调用方退回墙钟折算（兜底，偏宽松）。
func resolveEffectiveInterval(zone ZoneDecision, ordered []domain.TemperatureEvent) (time.Duration, string, time.Duration) {
	inferred := inferSamplingInterval(ordered)
	if zone.SampleIntervalSec > 0 {
		return time.Duration(zone.SampleIntervalSec) * time.Second, IntervalSourcePointProfile, inferred
	}
	if inferred > 0 {
		return inferred, IntervalSourceInferred, inferred
	}
	return 0, IntervalSourceWallClock, 0
}

// detectClockRegressions 检测记录仪时钟回拨。
//
// 返回回拨清单与「是否确认发生回拨」两样东西。
//
// 判据分两层：
//  1. 记录仪序里 measured_at 出现倒退——这是必要条件；
//  2. 倒退之后时间轴自身重叠（同一时刻被报了两次，
//     或排序后的墙钟跨度明显短于按采样周期估算的覆盖长度）——这是确认条件。
//
// 只有条件 1 没有条件 2 的，多半是导出文件顺序乱，不是钟坏了：
// 这类情况按 measured_at 排序归位即可，不该把「乱序」当成「时钟回拨」，
// 否则一次排序就够的事被误报成数据事故，复核的人要白跑一趟现场。
func detectClockRegressions(samples []domain.TemperatureEvent, interval time.Duration) ([]domain.ClockRegression, bool) {
	backward := make([]domain.ClockRegression, 0, 2)
	for i := 1; i < len(samples); i++ {
		prev, cur := samples[i-1].MeasuredAt, samples[i].MeasuredAt
		if !cur.Before(prev) {
			continue
		}
		backward = append(backward, domain.ClockRegression{
			Index:           i,
			At:              cur,
			PreviousAt:      prev,
			BackwardMinutes: int(prev.Sub(cur).Minutes()),
			Note: fmt.Sprintf("记录仪时钟回拨 %d 分钟：前一条采样 %s，后一条采样 %s",
				int(prev.Sub(cur).Minutes()),
				prev.Format(time.RFC3339), cur.Format(time.RFC3339)),
		})
	}
	if len(backward) == 0 {
		return nil, false
	}

	// 确认：排序之后时间轴是否自身重叠。
	sorted := make([]domain.TemperatureEvent, len(samples))
	copy(sorted, samples)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].MeasuredAt.Before(sorted[j].MeasuredAt)
	})
	for i := 1; i < len(sorted); i++ {
		if sorted[i].MeasuredAt.Equal(sorted[i-1].MeasuredAt) {
			return backward, true
		}
	}
	if interval > 0 && len(sorted) > 1 {
		span := sorted[len(sorted)-1].MeasuredAt.Sub(sorted[0].MeasuredAt)
		expected := time.Duration(len(sorted)-1) * interval
		if expected > 0 && float64(span) < float64(expected)*overlapTolerance {
			return backward, true
		}
	}
	return nil, false
}

// detectDataGaps 找出采样序列里的数据缺口。
//
// 判据是相邻间隔超过 3 个采样周期。缺口记下两头采样的真实时刻，
// 判定据此不跨缺口连线。
func detectDataGaps(ordered []domain.TemperatureEvent, interval time.Duration) []dataGap {
	if interval <= 0 || len(ordered) < 2 {
		return nil
	}
	threshold := time.Duration(gapIntervalFactor) * interval
	gaps := make([]dataGap, 0, 2)
	for i := 1; i < len(ordered); i++ {
		delta := ordered[i].MeasuredAt.Sub(ordered[i-1].MeasuredAt)
		if delta <= threshold {
			continue
		}
		missing := int(delta/interval) - 1
		if missing < 1 {
			missing = 1
		}
		gaps = append(gaps, dataGap{
			DataGap: domain.DataGap{
				From:           ordered[i-1].MeasuredAt,
				To:             ordered[i].MeasuredAt,
				GapMinutes:     int(delta.Minutes()),
				MissingSamples: missing,
				Note: fmt.Sprintf("采样中断 %d 分钟（约缺 %d 条采样），判定不跨缺口连线，该段时间不计入越温时长",
					int(delta.Minutes()), missing),
			},
			beforeIdx: i - 1,
			afterIdx:  i,
		})
	}
	return gaps
}

// detectProbeStuck 找出探头卡死段。
//
// 判据：连续 stuckMinRun 条读数的数值完全相同，且该数值不在温区中值上。
//
// 数值恒定在温区中值时，「探头不动」与「冰箱本来就稳」分不开，标了会误伤；
// 恒定在偏离中值的位置才是探头失灵的硬特征——
// 冰箱不可能长时间一动不动地停在偏离中值的地方。
//
// 数值相同的连段在数据缺口处断开：缺了那段时间就没有「连续观测」，
// 两头读数相同也可能只是碰巧，不足以说探头卡住了。
func detectProbeStuck(ordered []domain.TemperatureEvent, zone ZoneDecision, interval time.Duration) []domain.ProbeStuckSpan {
	if len(ordered) < stuckMinRun {
		return nil
	}
	mid := (zone.ZoneMinC + zone.ZoneMaxC) / 2
	spans := make([]domain.ProbeStuckSpan, 0, 1)
	for i := 0; i < len(ordered); {
		j := i + 1
		for j < len(ordered) && ordered[j].TemperatureC == ordered[i].TemperatureC {
			if interval > 0 &&
				ordered[j].MeasuredAt.Sub(ordered[j-1].MeasuredAt) > time.Duration(gapIntervalFactor)*interval {
				break
			}
			j++
		}
		runLen := j - i
		value := ordered[i].TemperatureC
		if runLen >= stuckMinRun && !nearlyEqual(value, mid) {
			spans = append(spans, domain.ProbeStuckSpan{
				From:        ordered[i].MeasuredAt,
				To:          ordered[j-1].MeasuredAt,
				SampleCount: runLen,
				Value:       value,
				Note: fmt.Sprintf("连续 %d 条读数恒为 %.2f℃，且不在温区中值 %.2f℃，判为探头卡死；该段读数不计入温升速率",
					runLen, value, mid),
			})
		}
		i = j
	}
	return spans
}

// nearlyEqual 比较两个温度是否相同（容差 0.01℃）。
//
// 读数是浮点数，直接比相等会在「同一个值」上判出「不一样」，
// 卡死段因此被漏掉——漏判的代价是让一段假读数进了归因特征。
func nearlyEqual(a, b float64) bool {
	return math.Abs(a-b) < 0.01
}

// repairTimeline 在确认时钟回拨后重建一条单调时间轴。
//
// 做法：回拨点之后的采样按记录仪序继续走，步长取相邻采样的真实间隔；
// 回拨那一刻按标称采样周期接上——设备一直在采样，只是它的钟不准。
//
// 原始时刻原样保留在采样序列与回拨异常里，不被覆盖；
// 修正后的时间轴只用于窗口起止与持续时长的折算，
// 并且一定在窗口上标出「发生过回拨」，由人复核时决定采不采信。
func repairTimeline(ordered []domain.TemperatureEvent, interval time.Duration) []time.Time {
	logical := make([]time.Time, len(ordered))
	if len(ordered) == 0 {
		return logical
	}
	logical[0] = ordered[0].MeasuredAt
	for i := 1; i < len(ordered); i++ {
		step := ordered[i].MeasuredAt.Sub(ordered[i-1].MeasuredAt)
		if step <= 0 {
			step = interval
			if step <= 0 {
				step = time.Minute
			}
		}
		logical[i] = logical[i-1].Add(step)
	}
	return logical
}

// riseRate 计算温升速率。
//
// 口径：取「连续观测段起点 → 峰值点」的平均温升速率。
//
// 三个约束各自有理由：
//   - 只在同一段连续观测内算：数据缺口处不连线，缺的那段时间没有读数；
//   - 探头卡死的读数被剔除：它们不是温度在动；
//   - 起点回溯到连续观测段的开头：温升的起点常常在越限之前，
//     只看窗口内的读数会把起点截在门槛线上，算出来的速率偏低。
//
// 有效采样不足时返回不可用，不给一个凑出来的数字。
func (s excursionScan) riseRate(seg excursionSegment) domain.RiseRate {
	rate := domain.RiseRate{}

	// 一、剔除探头卡死的读数，并标出因此产生的断裂。
	valid := make([]int, 0, len(s.ordered))
	for i := range s.ordered {
		if s.inStuckSpan(i) {
			rate.ExcludedStuckSamples++
			continue
		}
		valid = append(valid, i)
	}
	rate.ValidSamples = len(valid)
	if len(valid) < 2 {
		rate.Note = "剔除探头卡死段后有效采样不足两条，算不出温升速率"
		return rate
	}

	// 二、峰值取窗口内的有效读数里偏离温区最远的那一条。
	// 窗口的原始峰值若落在卡死段上，就不能拿来算速率——那不是温度在动。
	peakSampleIdx, peakC := s.effectivePeak(seg)
	if peakSampleIdx < 0 {
		rate.Note = "窗口内没有可用于计算温升速率的有效读数"
		return rate
	}
	rate.PeakC = peakC

	// 三、从峰值所在的连续观测段起点回溯。
	runStart, ok := s.runStart(valid, peakSampleIdx, &rate)
	if !ok {
		rate.Note = "峰值所在的连续观测段内没有更早的有效读数，算不出温升速率"
		return rate
	}

	minutes := s.minutesBetween(runStart, peakSampleIdx)
	if minutes <= 0 {
		rate.Note = "峰值与观测段起点的有效间隔为 0，算不出温升速率"
		return rate
	}
	rate.MinutesToPeak = minutes
	rate.PerMinute = (peakC - s.ordered[runStart].TemperatureC) / float64(minutes)
	rate.Usable = true
	rate.Note = "温升速率按连续观测段起点到峰值的平均速率折算；探头卡死段与数据缺口均已剔除"
	return rate
}

// effectivePeak 取窗口内参与速率计算的有效峰值。
//
// 窗口的原始峰值落在卡死段上时，改用窗口内其余有效读数里偏离温区最远的一条。
func (s excursionScan) effectivePeak(seg excursionSegment) (int, float64) {
	best := -1
	bestC := 0.0
	for i := seg.firstIdx; i <= seg.lastIdx; i++ {
		if s.inStuckSpan(i) {
			continue
		}
		if best < 0 {
			best, bestC = i, s.ordered[i].TemperatureC
			continue
		}
		if seg.side == sideHigh && s.ordered[i].TemperatureC > bestC {
			best, bestC = i, s.ordered[i].TemperatureC
		}
		if seg.side != sideHigh && s.ordered[i].TemperatureC < bestC {
			best, bestC = i, s.ordered[i].TemperatureC
		}
	}
	return best, bestC
}

// runStart 回溯峰值所在连续观测段的起点下标。
//
// 观测段被两类东西打断：数据缺口（超过 3 个采样周期）与探头卡死段的剔除。
// 两者都不连线——跨过去算出来的斜率是假的。
func (s excursionScan) runStart(valid []int, peakIdx int, rate *domain.RiseRate) (int, bool) {
	runStart := -1
	prev := -1
	for _, idx := range valid {
		if idx == peakIdx {
			break
		}
		if prev < 0 {
			// 第一条有效采样就是本观测段的起点。
			runStart = idx
		} else if s.breaksBetween(prev, idx) {
			// 断点之后的那条是新观测段的起点，前面的读数不能用来算这段的斜率。
			runStart = idx
			rate.ExcludedGapMinutes += s.gapMinutesBetween(prev, idx)
		}
		prev = idx
	}
	if runStart < 0 {
		return -1, false
	}
	return runStart, true
}

// gapMinutesBetween 算两个下标之间因断裂而未计入的分钟数。
func (s excursionScan) gapMinutesBetween(prev, next int) int {
	if s.interval <= 0 {
		return 0
	}
	delta := s.ordered[next].MeasuredAt.Sub(s.ordered[prev].MeasuredAt)
	observed := float64(next-prev) * s.interval.Minutes()
	missing := delta.Minutes() - observed
	if missing < 0 {
		return 0
	}
	return int(missing)
}

// breaksBetween 判断两个下标之间是否有断裂（数据缺口或卡死剔除）。
func (s excursionScan) breaksBetween(prev, next int) bool {
	// 中间被剔除的卡死采样本身就是断裂。
	for i := prev + 1; i < next; i++ {
		if s.inStuckSpan(i) {
			return true
		}
	}
	if s.interval <= 0 {
		return false
	}
	delta := s.ordered[next].MeasuredAt.Sub(s.ordered[prev].MeasuredAt)
	return delta > time.Duration(gapIntervalFactor)*s.interval
}

// minutesBetween 算两个下标之间的有效分钟数。
//
// 时间轴可信时按墙钟差值；时钟回拨已确认时按采样点数折算——
// 那时墙钟本身就不成立，按它折算会算出负数时长。
func (s excursionScan) minutesBetween(from, to int) int {
	if s.clockBroken {
		return (to - from) * int(s.interval.Minutes())
	}
	return int(s.logicalAt(to).Sub(s.logicalAt(from)).Minutes())
}

// inStuckSpan 判断某个下标是否落在探头卡死段里。
func (s excursionScan) inStuckSpan(idx int) bool {
	if idx < 0 || idx >= len(s.stuckIdx) {
		return false
	}
	return s.stuckIdx[idx]
}

// ComputeRiseRate 对外暴露的温升速率计算入口。
//
// 给定一批采样与温区，算出温升速率特征（℃/分钟）。
// 窗口产物里的温升速率就是它算出来的。
func ComputeRiseRate(samples []domain.TemperatureEvent, zone ZoneDecision) (domain.RiseRate, error) {
	scan, err := scanAll(samples, zone)
	if err != nil {
		return domain.RiseRate{}, err
	}
	if len(scan.ordered) == 0 {
		return domain.RiseRate{Note: "没有采样，算不出温升速率"}, nil
	}

	// 侧别由越限读数决定：先高后低是两种完全不同的异常，不能混着算速率。
	side := sideHigh
	hasOutOfRange := false
	for _, s := range scan.ordered {
		if zone.OutOfRange(s.TemperatureC) {
			side = sideOf(s.TemperatureC, zone)
			hasOutOfRange = true
			break
		}
	}
	if !hasOutOfRange {
		return domain.RiseRate{
			Note: "采样全在温区内，没有温升可算",
		}, nil
	}

	seg := excursionSegment{firstIdx: 0, lastIdx: len(scan.ordered) - 1, side: side}
	return scan.riseRate(seg), nil
}
