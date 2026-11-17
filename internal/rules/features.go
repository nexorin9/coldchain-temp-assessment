package rules

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// 本文件是归因特征工程。
//
// 越温窗口只回答「哪段时间、到了多少度」；它回答不了「为什么会这样」。
// 归因要看的却是另一组东西：温度升得多快、湿度有没有跟着涨、
// 门开了多久、断电事件有没有出现、相邻读数有没有突然跳变。
//
// 这批特征是规则基线归因与模型归因**共用的输入面**。
// 共用不是为了省事，是为了可比：两边看同一批数字，
// 结论不一致时才能把差异摆到台面上逐条对照，
// 而不是各说各话、谁也说服不了谁。
//
// 三条口径贯穿全文件：
//
//  1. **不跨缺口连线**：缺数据的那段时间没有读数，
//     把两头的读数接起来算出来的斜率是假的。
//  2. **卡死段不进温升**：探头不动不是温度在动。
//  3. **缺测写 nil**：算不出来的特征记进 MissingFeatures，不用 0 顶替。

// sampleJumpThresholdC 是相邻采样跳变的判定阈值（℃）。
//
// 取 5℃ 不是拍脑袋：疫苗冷藏箱有箱体热容、开门后室外热空气侵入也有缓冲，
// 箱内空气温度在**一个采样周期**（5 分钟）里跳 5℃ 基本不可能是真实变化。
// 出现这种跳变，要怀疑的是探头脱落或接触不良，而不是冰箱真升了那么快。
//
// 跨数据缺口的相邻采样对不参与判定：那两条读数在时间上并不相邻，
// 中间缺了多久都不一定，差值大是必然的，不能算成跳变。
const sampleJumpThresholdC = 5.0

// ExtractFeatures 计算一次越温窗口连同周边观测的归因特征。
//
// samples 是本次判定的**完整**采样序列（不只是窗口内那几条）：
// 特征要用到窗口之前的常态（湿度基线、开门滞后、温升起点），
// 只给窗口内的读数，算出来的斜率永远偏低、基线永远没有。
//
// window 是已成立的越温窗口。窗口为 nil 时返回错误——没有窗口就没有
// 「这次越温的特征」，硬凑一组数字出来比不给更危险。
//
// 温区判定不可用时同样返回错误：峰值偏离度、湿度基线都要按温区算，
// 温区不可信时算出来的特征看着正常、实则整个是错的。
func ExtractFeatures(samples []domain.TemperatureEvent, window *domain.ExcursionWindow, zone ZoneDecision) (domain.ExcursionFeatures, error) {
	if window == nil {
		return domain.ExcursionFeatures{}, errcode.Newf(errcode.Validation,
			"没有越温窗口，无从提取归因特征")
	}

	scan, err := scanAll(samples, zone)
	if err != nil {
		return domain.ExcursionFeatures{}, err
	}
	if len(scan.ordered) == 0 {
		return domain.ExcursionFeatures{}, errcode.Newf(errcode.Validation,
			"没有温度采样，无从提取归因特征")
	}

	first, last, ok := windowIndexRange(scan, *window)
	if !ok {
		return domain.ExcursionFeatures{}, errcode.Newf(errcode.NotFound,
			"本次采样里找不到窗口 %s ~ %s 对应的读数",
			window.Start.Format(time.RFC3339), window.End.Format(time.RFC3339))
	}

	f := domain.ExcursionFeatures{
		SampleJumpThresholdC: sampleJumpThresholdC,
		// 缺测清单显式给空数组而不是 null：
		// 「一个特征都没缺」与「这份清单算不出来」在台账上不是一回事。
		MissingFeatures: []string{},
		Notes:           []string{},
	}
	addNote(&f, "特征取自本次异常的归因观测范围；数据缺口处不连线，探头卡死段不计入温升速率")

	side := windowSide(scan, first, last, zone)
	scopeStart, scopeEnd := attributionScope(scan, first, last)

	extractRiseRate(&f, scan, excursionSegment{firstIdx: first, lastIdx: last, side: side})
	extractPeakDeviation(&f, *window, zone, side)
	extractHumidity(&f, scan, scopeStart, first, last, zone)
	extractDeviceEvents(&f, scan, scopeStart, scopeEnd, first)
	extractSampleJumps(&f, scan, scopeStart, scopeEnd)
	extractProbeStuck(&f, scan, scopeStart, scopeEnd)
	extractAccuracy(&f, zone)
	return f, nil
}

// attributionScope 定本次异常的归因观测范围。
//
// 范围是「上一次异常结束之后、下一次异常开始之前」的那段时间：
// 上一次异常的原因算在上一次异常头上，下一次异常里的事与本次无关，
// 夹在中间这段才是这次异常真正的原因可能发生的地方。
//
// 开门往往就发生在这段里——门开了十几分钟，温度才慢慢爬过上限，
// 那一刻温度还在温区内，算不进窗口，却正是原因。
//
// 范围不跨数据缺口：缺口那头的时间轴与这边没有连续观测关系，
// 拿两头的读数凑成一段「期间」等于凭空造出一段没发生过的现场。
func attributionScope(scan excursionScan, first, last int) (int, int) {
	runStart := contextRunStart(scan, first)

	scopeStart := runStart
	for _, seg := range scan.segments {
		if seg.lastIdx < first && seg.lastIdx+1 > scopeStart {
			scopeStart = seg.lastIdx + 1
		}
	}
	scopeEnd := len(scan.ordered) - 1
	for _, seg := range scan.segments {
		if seg.firstIdx > last {
			scopeEnd = seg.firstIdx - 1
			break
		}
	}

	// 裁到窗口所在的连续观测段内。
	for scopeStart < first && crossesGap(scan, scopeStart, scopeStart+1) {
		scopeStart++
	}
	for scopeEnd > last && crossesGap(scan, scopeEnd-1, scopeEnd) {
		scopeEnd--
	}
	return scopeStart, scopeEnd
}

// addNote 追加一条计算口径说明。
func addNote(f *domain.ExcursionFeatures, format string, args ...any) {
	f.Notes = append(f.Notes, fmt.Sprintf(format, args...))
}

// markMissing 记下某个特征算不出来。
func markMissing(f *domain.ExcursionFeatures, name string) {
	f.MissingFeatures = append(f.MissingFeatures, name)
}

// windowIndexRange 找到窗口在有序采样里的下标范围（闭区间）。
//
// 用修正后的时间轴（logicalAt）比对：确认时钟回拨时，
// 窗口起止取的是重建出来的单调时间轴上的时刻，
// 拿原始 measured_at 去比会对不上。
func windowIndexRange(scan excursionScan, window domain.ExcursionWindow) (int, int, bool) {
	first := -1
	last := -1
	for i := range scan.ordered {
		at := scan.logicalAt(i)
		if at.Before(window.Start) {
			continue
		}
		if first < 0 {
			first = i
		}
		if !at.After(window.End) {
			last = i
		}
	}
	if first < 0 || last < first {
		return 0, 0, false
	}
	return first, last, true
}

// windowSide 判定窗口的越限侧别。
//
// 看窗口里第一条越限读数在哪一侧：偏热与偏冷是两种完全不同的异常，
// 峰值偏离度、跳变幅度这些特征都按侧别算，不能混着来。
func windowSide(scan excursionScan, first, last int, zone ZoneDecision) excursionSide {
	for i := first; i <= last; i++ {
		if zone.OutOfRange(scan.ordered[i].TemperatureC) {
			return sideOf(scan.ordered[i].TemperatureC, zone)
		}
	}
	return sideHigh
}

// contextRunStart 回溯窗口起点所在连续观测段的起点下标。
//
// 观测段被数据缺口打断：缺口之前的读数与窗口之间没有连续观测关系，
// 拿它们算开门滞后或湿度基线等于把两段不相干的过程接在一起。
//
// 窗口起点本身落在卡死段里时（罕见，但会出现），
// 保守起见就把起点当段首——宁可少看一点前情，也不把不相干的读数接上来。
func contextRunStart(scan excursionScan, idx int) int {
	if idx <= 0 {
		return 0
	}
	valid := validIndexes(scan)
	start, ok := scan.runStart(valid, idx, &domain.RiseRate{})
	if !ok || start > idx {
		return idx
	}
	return start
}

// validIndexes 列出剔除探头卡死段后的采样下标。
func validIndexes(scan excursionScan) []int {
	valid := make([]int, 0, len(scan.ordered))
	for i := range scan.ordered {
		if scan.inStuckSpan(i) {
			continue
		}
		valid = append(valid, i)
	}
	return valid
}

// extractRiseRate 算温升速率（℃/小时）。
//
// 口径：连续观测段起点 → 有效峰值，按**采样点数**折算历时。
// 三个约束各自有理由：
//   - 只在同一段连续观测内算：数据缺口处不连线；
//   - 探头卡死段的读数剔除：它们不是温度在动；
//   - 历时按采样点数折算：一条采样记一个采样周期，
//     墙钟差值会把采样节奏不规整误当成温升更慢。
func extractRiseRate(f *domain.ExcursionFeatures, scan excursionScan, seg excursionSegment) {
	valid := validIndexes(scan)
	peakIdx, peakC := scan.effectivePeak(seg)
	if peakIdx < 0 {
		markMissing(f, domain.FeatureRiseRatePerHour)
		addNote(f, "窗口内没有可用于计算温升速率的有效读数")
		return
	}

	rate := domain.RiseRate{}
	runStart, ok := scan.runStart(valid, peakIdx, &rate)
	if !ok {
		markMissing(f, domain.FeatureRiseRatePerHour)
		addNote(f, "峰值所在的连续观测段内没有更早的有效读数，算不出温升速率")
		return
	}

	intervals := peakIdx - runStart
	hours := 0.0
	if scan.interval > 0 {
		hours = float64(intervals) * scan.interval.Hours()
	} else {
		hours = scan.logicalAt(peakIdx).Sub(scan.logicalAt(runStart)).Hours()
	}
	if hours <= 0 {
		markMissing(f, domain.FeatureRiseRatePerHour)
		addNote(f, "温升历时为 0，算不出温升速率")
		return
	}

	startC := scan.ordered[runStart].TemperatureC
	slope := (peakC - startC) / hours
	f.RiseRatePerHour = &slope
	f.RiseRateHours = hours
	f.RiseRateSamples = intervals + 1
	f.RiseRateStartC = startC
	f.RiseRatePeakC = peakC
	f.RiseRateExcludedGapMinutes = rate.ExcludedGapMinutes
	addNote(f, "温升速率 %.2f℃/小时：连续观测段起点 %.2f℃ → 有效峰值 %.2f℃，历时 %.2f 小时（按采样点数折算，共 %d 条采样）",
		slope, startC, peakC, hours, intervals+1)
	if rate.ExcludedGapMinutes > 0 {
		addNote(f, "因数据缺口未计入温升历时 %d 分钟；缺口处不连线，缺口两头的读数没被接成一段连续温升",
			rate.ExcludedGapMinutes)
	}
}

// extractPeakDeviation 算峰值偏离温区边界的幅度。
//
// 它回答的是「超了多少钱」：同样叫越温，超 0.4℃ 与超 7℃
// 对批次处置的指向完全不同。
func extractPeakDeviation(f *domain.ExcursionFeatures, window domain.ExcursionWindow, zone ZoneDecision, side excursionSide) {
	var deviation float64
	switch side {
	case sideHigh:
		deviation = window.PeakC - zone.ZoneMaxC
	case sideLow:
		deviation = zone.ZoneMinC - window.PeakC
	default:
		markMissing(f, domain.FeaturePeakDeviationC)
		addNote(f, "窗口侧别不明，算不出峰值偏离幅度")
		return
	}
	if deviation <= 0 {
		markMissing(f, domain.FeaturePeakDeviationC)
		addNote(f, "窗口峰值落在温区内，算不出偏离幅度")
		return
	}
	f.PeakDeviationC = &deviation
	f.PeakDeviationSide = sideName(side)
	addNote(f, "峰值偏离温区 %s 边界 %.2f℃（温区 %s，峰值 %.2f℃）",
		f.PeakDeviationSide, deviation, zone.ZoneLabel, window.PeakC)
}

// extractHumidity 算湿度基线、峰值与上升幅度。
//
// 基线取**窗口之前、温区内**的湿度中位数：那是箱内的常态。
// 越温期间的湿度峰值减去常态，就是「湿空气被带进来了多少」的量化说法。
//
// 取中位数而不是均值：偶发的一条异常湿度读数会把均值拉偏一截，
// 而归因结论是要写进药事台账的，不能被一条坏读数牵着走。
func extractHumidity(f *domain.ExcursionFeatures, scan excursionScan, scopeStart, first, last int, zone ZoneDecision) {
	baseline := inZoneHumidityBaseline(scan, scopeStart, first, zone)

	peak := math.Inf(-1)
	peakFound := false
	for i := first; i <= last; i++ {
		h := scan.ordered[i].HumidityPct
		if h == nil {
			continue
		}
		if *h > peak {
			peak = *h
			peakFound = true
		}
	}

	if !peakFound {
		markMissing(f, domain.FeatureHumidityRisePct)
		addNote(f, "窗口内没有湿度读数，算不出湿度上升幅度")
		return
	}
	peakValue := peak
	f.HumidityPeakPct = &peakValue

	if len(baseline) == 0 {
		markMissing(f, domain.FeatureHumidityRisePct)
		addNote(f, "窗口之前没有可作基线的湿度读数，算不出湿度上升幅度")
		return
	}

	base := median(baseline)
	rise := peakValue - base
	f.HumidityBaselinePct = &base
	f.HumidityBaselineSamples = len(baseline)
	f.HumidityRisePct = &rise
	addNote(f, "湿度基线 %.1f%%（窗口之前 %d 条温区内读数的中位数），窗口内峰值 %.1f%%，上升 %.1f 个百分点",
		base, len(baseline), peakValue, rise)
}

// inZoneHumidityBaseline 取窗口之前的湿度基线样本。
//
// 先取温区内的读数（箱内常态）；一条温区内读数都没有时，
// 退回到窗口之前全部带湿度的读数——总比没有基线强，
// 只是这条基线不干净，note 里会写明。
func inZoneHumidityBaseline(scan excursionScan, scopeStart, first int, zone ZoneDecision) []float64 {
	inZone := make([]float64, 0, 16)
	all := make([]float64, 0, 16)
	for i := scopeStart; i < first; i++ {
		h := scan.ordered[i].HumidityPct
		if h == nil {
			continue
		}
		all = append(all, *h)
		if !zone.OutOfRange(scan.ordered[i].TemperatureC) {
			inZone = append(inZone, *h)
		}
	}
	if len(inZone) > 0 {
		return inZone
	}
	return all
}

// median 取一组数的中位数。
func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

// extractDeviceEvents 提取开门、断电与除霜三类设备事件特征。
//
// 统计范围是本次异常的**归因观测范围**（见 attributionScope）：
// 上一次异常结束之后、下一次异常开始之前的那段时间。
// 上一次异常里的开门算上一次异常头上，下一次异常里的事与本次无关。
//
// 设备事件是**逐条采样**报上来的：一条采样带一个事件，
// 事件时刻就是这条采样的时刻。开门时长因此按「观察到的开门采样条数 × 采样周期」
// 折算——门在两次采样之间开过又关了，设备是看不见的，
// 这条时长是下界，不是全量。
func extractDeviceEvents(f *domain.ExcursionFeatures, scan excursionScan, scopeStart, scopeEnd, onsetIdx int) {
	// 这台设备到底会不会上报事件：全序列里出现过任何一类事件即视为会上报。
	// 分不出「没开门」与「这台设备根本不上报开门」，后面的零值就没有意义。
	for i := range scan.ordered {
		if len(scan.ordered[i].Events) > 0 {
			f.DeviceEventsReported = true
			break
		}
	}

	doorCount := 0
	var doorFirst, doorLast *time.Time
	powerCount := 0
	var powerFirst *time.Time
	defrostCount := 0
	var defrostFirst *time.Time

	for i := scopeStart; i <= scopeEnd && i < len(scan.ordered); i++ {
		s := scan.ordered[i]
		for _, kind := range s.Events {
			switch kind {
			case domain.EventDoorOpen:
				doorCount++
				if doorFirst == nil {
					at := s.MeasuredAt
					doorFirst = &at
				}
				at := s.MeasuredAt
				doorLast = &at
			case domain.EventPowerLoss:
				powerCount++
				if powerFirst == nil {
					at := s.MeasuredAt
					powerFirst = &at
				}
			case domain.EventDefrost:
				defrostCount++
				if defrostFirst == nil {
					at := s.MeasuredAt
					defrostFirst = &at
				}
			}
		}
	}

	// 开门时长
	switch {
	case !f.DeviceEventsReported:
		markMissing(f, domain.FeatureDoorOpenMinutes)
		addNote(f, "这台设备没有上报过任何设备事件，无法判断是否开过门")
	case doorCount == 0:
		zero := 0
		f.DoorOpenMinutes = &zero
		f.DoorOpenCount = &zero
		addNote(f, "本次异常的观测范围内设备上报过事件，但没有开门事件")
	default:
		minutes := doorOpenMinutes(scan, doorCount, doorFirst, doorLast)
		f.DoorOpenMinutes = &minutes
		f.DoorOpenCount = &doorCount
		f.DoorOpenAt = doorFirst
		addNote(f, "本次异常的观测范围内开门 %d 次，累计 %d 分钟（按观察到的开门采样条数 × 采样周期折算，为下界）",
			doorCount, minutes)
	}

	// 断电与除霜
	if !f.DeviceEventsReported {
		markMissing(f, domain.FeaturePowerLossObserved)
		markMissing(f, domain.FeatureDefrostObserved)
	} else {
		powerObserved := powerCount > 0
		f.PowerLossObserved = &powerObserved
		f.PowerLossCount = powerCount
		f.PowerLossAt = powerFirst
		defrostObserved := defrostCount > 0
		f.DefrostObserved = &defrostObserved
		f.DefrostCount = defrostCount
		f.DefrostAt = defrostFirst
		if powerObserved {
			addNote(f, "本次异常的观测范围内出现断电事件 %d 次，首次在 %s", powerCount, powerFirst.Format(time.RFC3339))
		}
		if defrostObserved {
			addNote(f, "本次异常的观测范围内出现除霜事件 %d 次，首次在 %s", defrostCount, defrostFirst.Format(time.RFC3339))
		}
	}

	// 开门到温升起点的滞后
	extractDoorToRiseDelay(f, scan, onsetIdx)
}

// doorOpenMinutes 折算开门累计时长（分钟）。
//
// 采样周期已知时按条数折算：一条带开门的采样代表一个采样周期里门是开的。
// 周期无从解析时退回墙钟跨度（兜底，偏宽松）。
func doorOpenMinutes(scan excursionScan, count int, first, last *time.Time) int {
	if scan.interval > 0 {
		return count * int(scan.interval.Minutes())
	}
	if first == nil || last == nil {
		return 0
	}
	return int(last.Sub(*first).Minutes())
}

// extractDoorToRiseDelay 算开门到温升起点的滞后（分钟）。
//
// 取温升起点（窗口第一条越限读数）之前**最近**的一次开门：
// 那一次开门才是这次温升最可能对应的开门。
// 更早的开门已经被后面这一次覆盖了。
//
// 检索范围是窗口所在的整段连续观测，不限于归因观测范围：
// 这次异常之前很久开过门，这件事本身是重要的否定线索——
// 归因结论若说「是因为开门」，而最近一次开门在两小时前，
// 复核的人一眼就能看出这个结论站不住。
//
// 起点之前没有开门事件时，这一项记为缺测：
// 「没开门」与「开了门但设备没报」是两回事，不能混。
func extractDoorToRiseDelay(f *domain.ExcursionFeatures, scan excursionScan, onsetIdx int) {
	if !f.DeviceEventsReported {
		markMissing(f, domain.FeatureDoorToRiseDelayMinutes)
		addNote(f, "设备不上报设备事件，算不出开门与温升的先后关系")
		return
	}

	runStart := contextRunStart(scan, onsetIdx)
	doorIdx := -1
	for i := runStart; i <= onsetIdx && i < len(scan.ordered); i++ {
		for _, kind := range scan.ordered[i].Events {
			if kind == domain.EventDoorOpen {
				doorIdx = i
				break
			}
		}
	}
	if doorIdx < 0 {
		markMissing(f, domain.FeatureDoorToRiseDelayMinutes)
		addNote(f, "温升起点之前没有开门事件，算不出开门与温升的滞后")
		return
	}

	onset := scan.logicalAt(onsetIdx)
	opened := scan.logicalAt(doorIdx)
	delay := int(onset.Sub(opened).Minutes())
	f.DoorToRiseDelayMinutes = &delay
	addNote(f, "最近一次开门在 %s，温升起点在 %s，滞后 %d 分钟",
		opened.Format(time.RFC3339), onset.Format(time.RFC3339), delay)
}

// extractSampleJumps 算相邻采样的最大跳变幅度与跳变次数。
//
// 它与探头卡死互为反面：卡死是读数一动不动，突变是相邻两个采样之间跳了一大截。
// 探头脱落的典型表现是后者——箱内空气不可能在 5 分钟里跳 5℃。
//
// 跨数据缺口的采样对不参与：中间缺了多久都不知道，差值大是必然的，
// 把它算成跳变，等于每次断线都报一次「探头疑似脱落」。
func extractSampleJumps(f *domain.ExcursionFeatures, scan excursionScan, ctxStart, last int) {
	if last <= ctxStart {
		markMissing(f, domain.FeatureSampleJump)
		addNote(f, "观测段内不足两条采样，算不出相邻读数跳变")
		return
	}

	maxJump := 0.0
	jumpCount := 0
	compared := 0
	for i := ctxStart + 1; i <= last && i < len(scan.ordered); i++ {
		if crossesGap(scan, i-1, i) {
			continue
		}
		compared++
		delta := math.Abs(scan.ordered[i].TemperatureC - scan.ordered[i-1].TemperatureC)
		if delta > maxJump {
			maxJump = delta
		}
		if delta > sampleJumpThresholdC {
			jumpCount++
		}
	}
	if compared == 0 {
		markMissing(f, domain.FeatureSampleJump)
		addNote(f, "观测段内没有可比较的相邻采样，算不出相邻读数跳变")
		return
	}
	// 跳变幅度取两位小数：设备精度本身就有 0.3～0.5℃，
	// 浮点尾数（3.8000000000000007）写进结论只会让人以为这数字有多准。
	maxJump = math.Round(maxJump*100) / 100

	f.MaxSampleJumpC = &maxJump
	f.SampleJumpCount = &jumpCount
	addNote(f, "相邻采样最大跳变 %.2f℃，超过 %.2f℃ 阈值的跳变 %d 次（比较了 %d 对相邻采样，跨缺口的不计）",
		maxJump, sampleJumpThresholdC, jumpCount, compared)
}

// crossesGap 判断两个下标之间是否隔着一个数据缺口。
func crossesGap(scan excursionScan, prev, next int) bool {
	if scan.interval <= 0 {
		return false
	}
	delta := scan.ordered[next].MeasuredAt.Sub(scan.ordered[prev].MeasuredAt)
	return delta > time.Duration(gapIntervalFactor)*scan.interval
}

// extractProbeStuck 数一数观测段里有多少条读数落在探头卡死段。
//
// 它是「探头卡死」那一侧的证据，与跳变特征配对使用：
// 卡死是读数恒定，脱落是读数突变，两者的处置方式不一样。
func extractProbeStuck(f *domain.ExcursionFeatures, scan excursionScan, ctxStart, last int) {
	count := 0
	for i := ctxStart; i <= last && i < len(scan.ordered); i++ {
		if scan.inStuckSpan(i) {
			count++
		}
	}
	f.ProbeStuckSampleCount = count
	if count > 0 {
		addNote(f, "观测段内 %d 条读数落在探头卡死段里（读数恒定不动）", count)
	}
}

// extractAccuracy 带上设备自报的测温精度声明。
//
// 精度不足的设备报出的「超了 1.2℃」未必真超了 1.2℃，
// 所以精度声明必须跟着特征一起走，归因时才知道哪些数字扛得住复核。
func extractAccuracy(f *domain.ExcursionFeatures, zone ZoneDecision) {
	f.AccuracyLimitC = zone.Precision.LimitC
	f.PrecisionLimited = zone.Precision.Limited
	f.PrecisionTag = zone.Precision.Tag
	if zone.Precision.DeclaredC != nil {
		declared := *zone.Precision.DeclaredC
		f.AccuracyC = &declared
		return
	}
	markMissing(f, domain.FeatureAccuracyC)
	addNote(f, "设备未声明测温精度，精度上限 %.2f℃ 按该类设备规范口径取", zone.Precision.LimitC)
}
