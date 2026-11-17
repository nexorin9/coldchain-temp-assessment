package rules

import (
	"fmt"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
)

// 本文件是越限判定的两条收口规则：
//
//	一、短时波动过滤：未达最短越限门槛的越限单独归集，不生成正式窗口；
//	二、相邻越限合并：紧邻且回落不足门槛的两段越限合成一条窗口。
//
// 两条规则解决的是同一件事：让「一次异常」在台账上只算一条，
// 同时不让短时波动混进越温窗口。
//
// 为什么必须分开做：
// 冷库开门取苗，箱内温度冲高两三度、十几分钟后落回来——这是波动。
// 为它惊动药事、报疫苗生产企业评估批次，台账上很快全是噪声，
// 没人再当真。反过来，一次断电造成的越温若被拆成三条短窗口、
// 每条都不够门槛，结果一条窗口都不成立——台账上干干净净，
// 冰箱其实已经坏了半天。
//
// 所以：够门槛的合成一条窗口并在窗口里保留分段明细，
// 不够门槛的一条条列进短时波动，让药师自己看。

// ExtractWindows 抽出这批采样里的全部正式越温窗口。
//
// 相邻的达标越限已经合并成一条：紧邻的两段越限之间若只回落了不足
// min_excursion_minutes 分钟，说明设备没有真正恢复，应当算同一次异常。
//
// 温区判定不可用时返回错误且不产出任何窗口：窗口是按温区算出来的，
// 温区不可信时算出来的窗口看着正常、实则整个是错的。
func ExtractWindows(samples []domain.TemperatureEvent, zone ZoneDecision) ([]domain.ExcursionWindow, error) {
	windows, _, err := ExtractExcursionReport(samples, zone)
	return windows, err
}

// ExtractTransientExcursions 抽出未达最短越限门槛的短时越限（波动）。
//
// 波动不生成正式窗口，但必须留痕：「没成立窗口」与「压根没越限」
// 对药师是两回事——前者要有人去看一眼，后者不用管。
func ExtractTransientExcursions(samples []domain.TemperatureEvent, zone ZoneDecision) ([]domain.TransientExcursion, error) {
	_, transients, err := ExtractExcursionReport(samples, zone)
	return transients, err
}

// ExtractExcursionReport 一次性给出正式窗口与短时波动。
//
// 两个结论出自同一遍扫描，所以「哪些算窗口、哪些算波动」不会互相打架：
// 一条越限要么进窗口、要么进波动列表，不会两头都算或两头都不算。
func ExtractExcursionReport(samples []domain.TemperatureEvent, zone ZoneDecision) ([]domain.ExcursionWindow, []domain.TransientExcursion, error) {
	scan, err := scanAll(samples, zone)
	if err != nil {
		return nil, nil, err
	}
	if len(scan.ordered) == 0 {
		return nil, nil, nil
	}
	cls := classifyExcursions(scan, zone)
	windows := make([]domain.ExcursionWindow, 0, len(cls.groups))
	for _, group := range cls.groups {
		window := buildWindow(scan, group, zone)
		if window == nil {
			continue
		}
		windows = append(windows, *window)
	}
	return windows, cls.transients, nil
}

// excursionClassification 是一次扫描后越限段的归类结果。
type excursionClassification struct {
	// groups 是合并后的正式窗口分组，每组是一段或几段紧邻的达标越限。
	groups [][]excursionSegment
	// transients 是未达门槛的短时越限。
	transients []domain.TransientExcursion
}

// classifyExcursions 把扫出的越限段分成「正式窗口段」与「短时波动段」。
//
// 分档只看一件事：确认时长够不够最短越限门槛。
// 够门槛的进正式窗口，不够的进短时波动列表——不进窗口，但保留给药师看。
//
// 正式窗口段之间还要判「相邻」：
//
//   - 紧邻（中间没有别的越限段）、同侧、且两段之间确认回落的时间
//     不足 min_excursion_minutes 分钟 → 合并成一条窗口。
//   - 回落时间达到门槛 → 不合并：那说明设备真的恢复过，
//     是两次独立的异常，台账上应当分开记。
//   - 侧别不同 → 永远不合并：偏热与偏冷是两回事，
//     合成一条窗口会让「峰值取哪一侧」变得说不清。
//
// 为什么用「确认回落的时长」而不是「两段之间隔了多久」：
// 回落时长只数确实落在温区内的采样。冰箱在越限与回落之间何时降回安全温度，
// 现场只知道「下一次采样时它已经安全了」——所以确认回落时长是下界，
// 拿它跟门槛比才是保守口径：一段只被观察到一次安全采样的回落，
// 说明设备很可能只是抖了一下，还没真正恢复，合成一条更贴近现场实况。
func classifyExcursions(scan excursionScan, zone ZoneDecision) excursionClassification {
	cls := excursionClassification{}
	threshold := zone.MinExcursionMinutes
	var group []excursionSegment

	flush := func() {
		if len(group) > 0 {
			cls.groups = append(cls.groups, group)
			group = nil
		}
	}

	for _, seg := range scan.segments {
		minutes := scan.spanMinutes(seg)
		if threshold > 0 && minutes < float64(threshold) {
			// 未达门槛：这是一段波动，单独归集，不生成正式窗口。
			// 它同时打断了相邻关系：中间夹着一次越限，两段就不算「相邻」。
			cls.transients = append(cls.transients, buildTransient(scan, seg, zone, minutes))
			flush()
			continue
		}
		if len(group) > 0 {
			prev := group[len(group)-1]
			if prev.side == seg.side && scan.recoveryMinutes(prev, seg) < float64(threshold) {
				group = append(group, seg)
				continue
			}
		}
		flush()
		group = []excursionSegment{seg}
	}
	flush()
	return cls
}

// buildTransient 由一段未达门槛的越限构造短时波动记录。
func buildTransient(scan excursionScan, seg excursionSegment, zone ZoneDecision, minutes float64) domain.TransientExcursion {
	peakC, peakAt := scan.peakInRange(seg.firstIdx, seg.lastIdx, seg.side)
	return domain.TransientExcursion{
		Start:             scan.logicalAt(seg.firstIdx),
		End:               scan.logicalAt(seg.lastIdx),
		DurationMinutes:   int(minutes),
		PeakC:             peakC,
		PeakAt:            peakAt,
		SampleCount:       seg.lastIdx - seg.firstIdx + 1,
		OutOfRangeSamples: seg.outOfRangeCount,
		Side:              sideName(seg.side),
		Reason: fmt.Sprintf("确认时长 %d 分钟未达最短越限门槛 %d 分钟，按短时波动归集，不生成越温窗口",
			int(minutes), zone.MinExcursionMinutes),
	}
}

// recoveryMinutes 算两段越限之间确认回落到温区的时长（分钟）。
//
// 只数两段之间确实落在温区内的采样，按采样点数折算。
//
// 按采样点数而不是墙钟：墙钟会把断线那段时间算进来，
// 而断线期间冰箱到底回没回到温区，没人知道——
// 那段时间不能算回落，也不能算越限，只能是不知道。
func (s excursionScan) recoveryMinutes(prev, next excursionSegment) float64 {
	inZone := s.inZoneSamplesBetween(prev, next)
	if s.interval > 0 {
		return float64(inZone) * s.interval.Minutes()
	}
	if inZone == 0 {
		return 0
	}
	return s.ordered[next.firstIdx].MeasuredAt.Sub(s.ordered[prev.lastIdx].MeasuredAt).Minutes()
}

// inZoneSamplesBetween 数两段越限之间落在温区内的采样条数。
func (s excursionScan) inZoneSamplesBetween(prev, next excursionSegment) int {
	if next.firstIdx <= prev.lastIdx+1 {
		return 0
	}
	return next.firstIdx - prev.lastIdx - 1
}

// sideName 把侧别翻译成可写进结论的词。
func sideName(side excursionSide) string {
	if side == sideHigh {
		return domain.ExcursionSideHigh
	}
	return domain.ExcursionSideLow
}
