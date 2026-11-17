// 本文件是规则基线归因。
//
// 归因回答的是「这次越温是什么原因」——一句话，但它要写进药事台账、
// 随异常情况记录表递到疫苗生产企业手里去。所以这里的每一条结论都必须
// 扛得住复核：说得清「凭什么这么判」，也说得清「还差什么才能坐实」。
//
// 它有三重身份：
//
//  1. **模型归因的对照基线**：模型归因看得是同一批特征（见 features.go），
//     两边结论不一致时，才能把差异摆到台面上逐条对照。
//  2. **模型不可达时的降级落点**：模型挂了就退到这儿，标 degraded，
//     结论停在待主管确认，不推进业务状态。
//  3. **成稿的输入**：证据链直接进《温度异常情况记录表》。
//
// 三条纪律贯穿全文件：
//
//  1. **每条结论都带证据**：特征名 + 数值 + 阈值，一个都不能少。
//     「是断电导致的」不是证据，「设备上报断电事件 3 次，阈值 1 次」才是。
//  2. **证据不足就说不清**：宁可给 unknown 并列出还差哪条线索，
//     也不硬套一个原因。硬套出来的原因会被当成查清的事实写进台账。
//  3. **读数不可信时结论也要标**：设备精度声明超出规范上限，
//     全部结论显式标注「精度不足，仅供参考」，置信度封顶。

package rules

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// 归因原因码。
//
// 这八个取值是判定结论的稳定词汇表：规则基线与模型归因都用它们，
// 台账、成稿、报表也按它们归类。新增原因码要在这里登记，
// 不能在某一处临时起个名字——名字对不上，跨月对账就散了。
const (
	// CauseDoorOpen：柜门长时间未关，室外热空气进入箱内。
	CauseDoorOpen = "door_open"
	// CausePowerLoss：制冷设备断电或停机，箱内温度失去冷源。
	CausePowerLoss = "power_loss"
	// CauseDefrost：设备除霜周期引起箱内温度波动。
	CauseDefrost = "defrost"
	// CauseProbeDetached：温度探头脱落或接触不良，读数突变。
	CauseProbeDetached = "probe_detached"
	// CauseProbeStuck：温度探头卡死，读数恒定不动。
	CauseProbeStuck = "probe_stuck"
	// CauseACFailure：制冷能力不足（设备带电运行但箱内温度持续上行）。
	CauseACFailure = "ac_failure"
	// CauseOverstock：在库装载超出额定装载量，热容偏大、散热跟不上。
	CauseOverstock = "overstock"
	// CauseUnknown：证据不足以指向任一原因。
	CauseUnknown = "unknown"
)

// 原因码的中文短标签。
//
// 成稿与台账直接引用它：给药事看的表格里写「断电」比写 power_loss 有用得多。
var causeLabels = map[string]string{
	CauseDoorOpen:      "开门",
	CausePowerLoss:     "断电",
	CauseDefrost:       "除霜",
	CauseProbeDetached: "探头脱落",
	CauseProbeStuck:    "探头卡死",
	CauseACFailure:     "制冷异常",
	CauseOverstock:     "超量存放",
	CauseUnknown:       "原因待查",
}

// 归因判据的阈值。
//
// 阈值都写在这里并各自带一句理由：阈值本身是这套系统里最容易被
// 「就差一点」地改掉的东西，改动必须看得见理由。
const (
	// doorOpenMinMinutes：累计开门时长达到多少分钟才算「门确实开过」。
	//
	// 取 5 分钟是因为采样周期通常是 5 分钟：观察到一个带开门事件的采样，
	// 门至少开了一个采样周期。低于这个数不成立——那多半是设备误报。
	doorOpenMinMinutes = 5
	// doorOpenStrongMinutes：累计开门时长达到多少分钟算强证据。
	//
	// 取 15 分钟：三个采样周期。箱体有热容，开门十几分钟才会把温度推出温区；
	// 只开一下就超温的，多半不是开门造成的。
	doorOpenStrongMinutes = 15
	// maxDoorToRiseDelayMinutes：开门到温升起点的滞后超过多少分钟，
	// 这次开门就不能算温升的原因。
	//
	// 取 30 分钟：再久就变成「早先开过门」而不是「这次开门引起的温升」。
	// 这条是开门这一结论最容易被忽略的否定线索。
	maxDoorToRiseDelayMinutes = 30
	// humidityRiseDoorThresholdPct：湿度上升幅度达到多少个百分点，
	// 才算支持「开门引入室外湿空气」这条线索。
	humidityRiseDoorThresholdPct = 1.0
	// humidityFlatThresholdPct：湿度上升幅度不超过多少个百分点，
	// 才算「箱内没有湿空气进入」。
	//
	// 取 2 个百分点：开门时湿度跟着涨是常见现象，涨不足 2 个百分点
	// 说明热湿交换不充分，更像制冷中断而不是开门。
	humidityFlatThresholdPct = 2.0
	// minRiseRatePerHour：折算温升速率达到多少 ℃/小时，才算「箱内温度在往上走」。
	minRiseRatePerHour = 0.5
	// fastRiseRatePerHour：折算温升速率超过多少 ℃/小时，多半是读数跳变而非真实温升。
	//
	// 取 24 ℃/小时：折合 2 ℃/小时每 5 分钟。箱体有热容，
	// 真实空气温度不会这么快——这个速率更像是探头读数跳了一截。
	fastRiseRatePerHour = 24.0
	// probeStuckThresholdSamples：连续多少条恒定读数算探头卡死。
	probeStuckThresholdSamples = 3
	// acFailureMinDurationMinutes：制冷异常的温升至少持续多少分钟才值得怀疑。
	//
	// 取 60 分钟：短时间不制冷多半是刚开门或刚化霜，
	// 持续一小时的持续上行才是设备该修的信号。
	acFailureMinDurationMinutes = 60
	// acFailureMinRiseRatePerHour：制冷异常的温升速率门槛（℃/小时）。
	//
	// 取 1.0：比「箱内温度在往上走」的门槛高一档——要让结论落在制冷能力上，
	// 得是持续而明确的升温，不是贴着上界的轻微波动。
	acFailureMinRiseRatePerHour = 1.0
	// acFailureMaxDoorMinutes：制冷异常这一结论允许的累计开门时长上限（分钟）。
	//
	// 取 5 分钟：一个采样周期。开门超过这个数，热湿交换就足以解释升温，
	// 这时该归因的是开门而不是制冷能力。
	acFailureMaxDoorMinutes = doorOpenMinMinutes
)

// 置信度基线值。
//
// 每个原因各有一个基线，规则在此之上按证据加减。
// 基线不是概率，是「这条结论有多硬」的刻度：
// 0.9 意味着设备自己报了这件事，0.45 意味着是我们从现象反推的、须现场核实。
const (
	confidencePowerLoss     = 0.90
	confidenceDoorOpen      = 0.60
	confidenceDefrost       = 0.60
	confidenceProbeDetached = 0.70
	confidenceProbeStuck    = 0.65
	confidenceACFailure     = 0.50
	confidenceOverstock     = 0.40
	// confidenceMax 是任何一条结论的置信度上限。
	//
	// 归因是判断，不是测量：再硬的证据也只是「最可能是这个原因」，
	// 处置结论永远要由药学部主管确认。留一个天花板是提醒这件事。
	confidenceMax = 0.95
)

// ConfidenceCapPrecisionLimited 是精度不足时的置信度上限。
//
// 设备自称测温精度 1.2℃（超出 0.5℃ 规范上限）时，读数只够支撑
// 「这里发生了温度异常」，不足以支撑「超了 1.2℃」这样的量化结论；
// 原因判断同理。精度不足时结论一律封顶在本值以下，并显式标注。
const ConfidenceCapPrecisionLimited = 0.5

// PrecisionTagLimited 是精度不足时的固定标注文案。
const PrecisionTagLimited = "精度不足，仅供参考"

// LoadInfo 是在库装载情况。
//
// 它是「超量存放」这一条线索的判据：装载量超出额定装载时，
// 同样的开门时长与同样的环境温度，箱内升温会比空箱明显得多。
//
// 拿不到就当没有这条线索——不能拿「不知道装载量」去反推「没超量」。
type LoadInfo struct {
	// TotalQuantity 是当前在库总件数。
	TotalQuantity int
	// Capacity 是该点位额定装载件数；0 表示未登记容量，这条线索不可用。
	Capacity int
	// LoadPercent 是装载占额定容量的百分比（如 125 表示 125%）。
	//
	// 有些台账只登记百分比、不登记额定件数；两者给其一即可。
	LoadPercent *float64
	// BatchCount 是当前在库批次数。
	BatchCount int
}

// overstocked 判断在库装载是否超出额定装载。
//
// 两个口径给其一即可：件数（TotalQuantity / Capacity）或百分比（LoadPercent）。
// 都拿不到时返回 false——「不知道装了多少」不能反推成「没装多」。
func (l *LoadInfo) overstocked() (bool, float64, float64, string) {
	if l == nil {
		return false, 0, 0, ""
	}
	if l.LoadPercent != nil {
		return *l.LoadPercent > 100, *l.LoadPercent, 100, "load_percent"
	}
	if l.Capacity > 0 {
		return l.TotalQuantity > l.Capacity, float64(l.TotalQuantity), float64(l.Capacity), "load_quantity"
	}
	return false, 0, 0, ""
}

// AttributionOptions 是规则基线归因的额外输入。
//
// 特征（features.go）回答「这次越温长什么样」，但有两件事它答不了：
// 这次越温持续了多久（窗口）、在库装载了多少（台账）。
// 这两样各自有它的来源，不塞进特征里，特征就能保持「只看读数算得出来的」。
type AttributionOptions struct {
	// Window 是本次归因对应的越温窗口；为 nil 时只用特征。
	Window *domain.ExcursionWindow
	// Load 是在库装载情况；为 nil 时「超量存放」这条线索不可用。
	Load *LoadInfo
}

// Attribute 按规则基线给出归因结论。
//
// 输入是归因特征（features.go 算出的那批数字）与两个额外输入，
// 输出是带候选集、证据链与置信度的归因结论产物。
//
// 返回的结论里，Candidates 按证据强度（置信度）降序排好，
// 第一个就是 CauseCode。选不出原因时给 unknown，并把「还差哪条线索」列出来。
func Attribute(features domain.ExcursionFeatures, opts AttributionOptions) (*domain.AttributionPayload, error) {
	if featuresAreEmpty(features) {
		return nil, errcode.Newf(errcode.Validation,
			"归因特征为空，无从判断越温原因（特征须先由 ExtractFeatures 算出）")
	}

	candidates := make([]domain.AttributionCandidate, 0, 8)
	for _, rule := range []func(domain.ExcursionFeatures, AttributionOptions) (domain.AttributionCandidate, bool){
		rulePowerLoss,
		ruleDoorOpen,
		ruleDefrost,
		ruleProbeDetached,
		ruleProbeStuck,
		ruleACFailure,
		ruleOverstock,
	} {
		if candidate, ok := rule(features, opts); ok {
			candidates = append(candidates, candidate)
		}
	}

	capCandidates(candidates)
	sortCandidates(candidates)

	notes := make([]string, 0, 4)
	notes = append(notes, "规则基线归因：结论由观测特征按阈值推出，每条候选均附特征名、数值与阈值")

	if len(candidates) == 0 {
		payload := unknownPayload(features, notes)
		return applyPrecisionLimit(payload, features), nil
	}

	top := candidates[0]
	payload := &domain.AttributionPayload{
		CauseCode:  top.CauseCode,
		CauseLabel: top.Label,
		Confidence: top.Confidence,
		Candidates: candidates,
		Evidence:   top.Evidence,
		Notes:      notes,
	}
	if len(candidates) > 1 {
		notes = append(notes, fmt.Sprintf("共 %d 个候选原因，按证据强度排序：%s",
			len(candidates), candidateSummary(candidates)))
		payload.Notes = notes
	}
	payload.MissingFeatures = features.MissingFeatures
	return applyPrecisionLimit(payload, features), nil
}

// AttributeSamples 从采样与窗口直接给出规则基线归因。
//
// 它是「特征 + 归因」一步到位的入口：先算特征，再归因。
// 离线对照基准（公开数据集）与模型不可达时的降级路径都走这里，
// 保证两条路用的是同一套判据。
func AttributeSamples(samples []domain.TemperatureEvent, window *domain.ExcursionWindow,
	zone ZoneDecision, opts AttributionOptions) (*domain.AttributionPayload, error) {
	features, err := ExtractFeatures(samples, window, zone)
	if err != nil {
		return nil, err
	}
	// 窗口时长是「制冷异常」这条判据的门槛之一，调用方多半已经拿着窗口，
	// 这里直接补进选项里，免得归因看不到自己正在解释的那次越温。
	if opts.Window == nil {
		opts.Window = window
	}
	return Attribute(features, opts)
}

// rulePowerLoss 判「断电」。
//
// 设备自己上报断电事件是最硬的一条证据：不是我们从温度曲线反推的，
// 是记录仪当场报的。因此基线取到 0.9。
//
// 湿度是佐证：断电时压缩机停了，箱内没有热湿交换来源，湿度基本不动；
// 开门则会把室外湿空气带进来，湿度跟着涨。
func rulePowerLoss(features domain.ExcursionFeatures, _ AttributionOptions) (domain.AttributionCandidate, bool) {
	if features.PowerLossObserved == nil || !*features.PowerLossObserved {
		return domain.AttributionCandidate{}, false
	}

	confidence := confidencePowerLoss
	evidence := []domain.AttributionEvidence{{
		Feature:   "device_event:power_loss",
		Value:     float64(features.PowerLossCount),
		Threshold: 1,
		Note: fmt.Sprintf("设备上报断电事件 %d 次（阈值 1 次）",
			features.PowerLossCount),
	}}
	notes := "断电是记录仪自己上报的现场事件，属直接证据"

	if rate := features.RiseRatePerHour; rate != nil && *rate >= minRiseRatePerHour {
		evidence = append(evidence, domain.AttributionEvidence{
			Feature:   "rise_rate_per_hour",
			Value:     *rate,
			Threshold: minRiseRatePerHour,
			Note: fmt.Sprintf("断电后制冷中断，折算温升速率 %.2f ℃/小时（阈值 %.2f）",
				*rate, minRiseRatePerHour),
		})
	}
	if rise := features.HumidityRisePct; rise != nil {
		if *rise <= humidityFlatThresholdPct {
			evidence = append(evidence, domain.AttributionEvidence{
				Feature:   "humidity_rise_pct",
				Value:     *rise,
				Threshold: humidityFlatThresholdPct,
				Note: fmt.Sprintf("箱内湿度上升 %.2f 个百分点（阈值 %.2f，即不超过该值视为无明显湿空气进入），与开门引入湿空气的特征相反",
					*rise, humidityFlatThresholdPct),
			})
		} else {
			notes = appendNote(notes, "但窗口内湿度上升 %.2f 个百分点，与「断电时箱内湿度基本不变」不符，需核对是否同时发生过开门", *rise)
		}
	}

	return domain.AttributionCandidate{
		CauseCode:  CausePowerLoss,
		Label:      causeLabels[CausePowerLoss],
		Confidence: confidence,
		Evidence:   evidence,
		Note:       notes,
	}, true
}

// ruleDoorOpen 判「开门」。
//
// 开门是现场最常见也最容易被冤枉的原因：柜门开过不一定是这次越温的原因
// （可能是两小时前开过门），开门时间点与温升起点对不上时更要当心。
// 所以这一条同时看三样：开门时长、温升滞后、湿度变化。
func ruleDoorOpen(features domain.ExcursionFeatures, opts AttributionOptions) (domain.AttributionCandidate, bool) {
	if features.DoorOpenMinutes == nil {
		return domain.AttributionCandidate{}, false
	}
	minutes := *features.DoorOpenMinutes
	if minutes < doorOpenMinMinutes {
		return domain.AttributionCandidate{}, false
	}

	// 开门发生在温升之后：这次开门不是本次温升的原因，候选直接不成立。
	if delay := features.DoorToRiseDelayMinutes; delay != nil && *delay < 0 {
		return domain.AttributionCandidate{}, false
	}

	confidence := confidenceDoorOpen
	evidence := []domain.AttributionEvidence{{
		Feature:   "door_open_minutes",
		Value:     float64(minutes),
		Threshold: doorOpenMinMinutes,
		Note: fmt.Sprintf("观测范围内开门累计 %d 分钟（阈值 %d 分钟；按观察到的开门采样条数折算，为下界）",
			minutes, doorOpenMinMinutes),
	}}
	notes := ""

	// 开门时长要能覆盖这次越温，才谈得上是原因。
	covers, coverThreshold := doorOpenCoversExcursion(minutes, opts.Window)
	switch {
	case minutes >= doorOpenStrongMinutes:
		confidence += 0.15
		evidence = append(evidence, domain.AttributionEvidence{
			Feature:   "door_open_minutes",
			Value:     float64(minutes),
			Threshold: doorOpenStrongMinutes,
			Note: fmt.Sprintf("累计开门 %d 分钟，超过 %d 分钟强证据门槛",
				minutes, doorOpenStrongMinutes),
		})
	case covers:
		// 门开的时间虽然没有到固定门槛，但占了这次越温时长的一半以上：
		// 门几乎全程开着，这解释得通。
		confidence += 0.15
		evidence = append(evidence, domain.AttributionEvidence{
			Feature:   "door_open_minutes",
			Value:     float64(minutes),
			Threshold: coverThreshold,
			Note: fmt.Sprintf("累计开门 %d 分钟，超过本次越温时长的一半（阈值 %.2f 分钟）",
				minutes, coverThreshold),
		})
	}

	if delay := features.DoorToRiseDelayMinutes; delay != nil {
		switch {
		case *delay <= maxDoorToRiseDelayMinutes && covers:
			confidence += 0.10
			evidence = append(evidence, domain.AttributionEvidence{
				Feature:   "door_to_rise_delay_minutes",
				Value:     float64(*delay),
				Threshold: maxDoorToRiseDelayMinutes,
				Note: fmt.Sprintf("最近一次开门在温升起点前 %d 分钟（阈值 %d 分钟），时间上对得上",
					*delay, maxDoorToRiseDelayMinutes),
			})
		case *delay <= maxDoorToRiseDelayMinutes:
			// 时间上挨得近，但开门时长不足以解释这么长的温升：
			// 门开了几分钟，温度随后一小时慢慢爬——多半不是门的事。
			notes = appendNote(notes, "最近一次开门在温升起点前 %d 分钟，但开门累计仅 %d 分钟，不足以解释这次温升",
				*delay, minutes)
		default:
			// 开门发生过，但时间上对不上这次温升：这是「早先开过门」，
			// 不是「这次开门引起的温升」。置信度要压下去，
			// 压到比「由现象反推的制冷异常」还低一档。
			confidence = 0.30
			evidence = append(evidence, domain.AttributionEvidence{
				Feature:   "door_to_rise_delay_minutes",
				Value:     float64(*delay),
				Threshold: maxDoorToRiseDelayMinutes,
				Note: fmt.Sprintf("最近一次开门在温升起点前 %d 分钟，超过 %d 分钟，时间上对得远，只作弱线索",
					*delay, maxDoorToRiseDelayMinutes),
			})
			notes = appendNote(notes, "最近一次开门在温升起点前 %d 分钟，超过 %d 分钟，这一开门多半不是本次温升的原因",
				*delay, maxDoorToRiseDelayMinutes)
		}
	} else {
		notes = appendNote(notes, "开门与温升的先后关系未知（设备未上报开门事件或窗口之前没有开门记录），本条只作弱线索")
	}

	if rise := features.HumidityRisePct; rise != nil {
		if *rise >= humidityRiseDoorThresholdPct {
			confidence += 0.05
			evidence = append(evidence, domain.AttributionEvidence{
				Feature:   "humidity_rise_pct",
				Value:     *rise,
				Threshold: humidityRiseDoorThresholdPct,
				Note: fmt.Sprintf("窗口内湿度上升 %.2f 个百分点（阈值 %.2f），符合开门引入室外湿空气的特征",
					*rise, humidityRiseDoorThresholdPct),
			})
		} else {
			// 开门把室外湿空气带进来，湿度通常跟着涨；湿度纹丝不动，
			// 说明这次升温不是开门带进来的热湿交换——这条要压开门这一结论的权重。
			confidence -= 0.15
			notes = appendNote(notes, "窗口内湿度仅上升 %.2f 个百分点，不支持开门引入湿空气这一条", *rise)
		}
	}

	return domain.AttributionCandidate{
		CauseCode:  CauseDoorOpen,
		Label:      causeLabels[CauseDoorOpen],
		Confidence: confidence,
		Evidence:   evidence,
		Note:       notes,
	}, true
}

// doorOpenCoversExcursion 判断开门时长是否足以覆盖这次越温。
//
// 固定门槛（15 分钟）之外还有一条相对判据：开门时长至少要占越温时长的一半。
// 门开了 10 分钟、这次越温 15 分钟——门几乎全程开着，这解释得通；
// 门开了 5 分钟、这次越温 90 分钟——门开的那一会儿不可能把箱温顶一小时。
//
// 返回的第二个值是本次采用的阈值，写进证据里供复核时对照。
func doorOpenCoversExcursion(minutes int, window *domain.ExcursionWindow) (bool, float64) {
	if minutes >= doorOpenStrongMinutes {
		return true, doorOpenStrongMinutes
	}
	if window == nil || window.DurationMinutes <= 0 {
		return false, doorOpenStrongMinutes
	}
	half := float64(window.DurationMinutes) / 2
	return float64(minutes) >= half, half
}

// ruleDefrost 判「除霜」。
//
// 除霜是设备自身的周期性行为，多半伴随开门与温度波动，
// 往往是伴随现象而不是起因。所以基线压在 0.6：它成立，但排在断电与开门之后。
func ruleDefrost(features domain.ExcursionFeatures, _ AttributionOptions) (domain.AttributionCandidate, bool) {
	if features.DefrostObserved == nil || !*features.DefrostObserved {
		return domain.AttributionCandidate{}, false
	}

	confidence := confidenceDefrost
	evidence := []domain.AttributionEvidence{{
		Feature:   "device_event:defrost",
		Value:     float64(features.DefrostCount),
		Threshold: 1,
		Note: fmt.Sprintf("设备上报除霜事件 %d 次（阈值 1 次）",
			features.DefrostCount),
	}}
	notes := "除霜是设备自身行为，常与开门同时发生，多为伴随现象"

	if features.DoorOpenMinutes != nil && *features.DoorOpenMinutes >= doorOpenMinMinutes {
		notes = appendNote(notes, "同期还有开门累计 %d 分钟，起因更可能是开门", *features.DoorOpenMinutes)
		evidence = append(evidence, domain.AttributionEvidence{
			Feature:   "door_open_minutes",
			Value:     float64(*features.DoorOpenMinutes),
			Threshold: doorOpenMinMinutes,
			Note: fmt.Sprintf("同期开门累计 %d 分钟（阈值 %d 分钟），除霜多与开门同时发生",
				*features.DoorOpenMinutes, doorOpenMinMinutes),
		})
	}

	return domain.AttributionCandidate{
		CauseCode:  CauseDefrost,
		Label:      causeLabels[CauseDefrost],
		Confidence: confidence,
		Evidence:   evidence,
		Note:       notes,
	}, true
}

// ruleProbeDetached 判「探头脱落」。
//
// 探头脱落或接触不良时，读数会在相邻两个采样之间跳一大截：
// 箱体有热容，箱内空气温度不可能在 5 分钟里跳 5℃。
// 这条同时看跳变次数与跳变幅度——只跳一次可能是别的因素，
// 连着跳才说明读数本身不可信。
func ruleProbeDetached(features domain.ExcursionFeatures, _ AttributionOptions) (domain.AttributionCandidate, bool) {
	if features.SampleJumpCount == nil || features.MaxSampleJumpC == nil {
		return domain.AttributionCandidate{}, false
	}
	if *features.SampleJumpCount < 1 || *features.MaxSampleJumpC < features.SampleJumpThresholdC {
		return domain.AttributionCandidate{}, false
	}

	confidence := confidenceProbeDetached
	evidence := []domain.AttributionEvidence{{
		Feature:   "sample_jump_count",
		Value:     float64(*features.SampleJumpCount),
		Threshold: 1,
		Note: fmt.Sprintf("相邻采样出现 %d 次跳变（阈值 1 次）",
			*features.SampleJumpCount),
	}, {
		Feature:   "max_sample_jump_c",
		Value:     *features.MaxSampleJumpC,
		Threshold: features.SampleJumpThresholdC,
		Note: fmt.Sprintf("相邻采样最大跳变 %.2f℃，超过 %.2f℃ 阈值",
			*features.MaxSampleJumpC, features.SampleJumpThresholdC),
	}}
	notes := "探头脱落或接触不良时读数会突变，箱体热容不支持这种变化速度"

	if rate := features.RiseRatePerHour; rate != nil && *rate >= fastRiseRatePerHour {
		evidence = append(evidence, domain.AttributionEvidence{
			Feature:   "rise_rate_per_hour",
			Value:     *rate,
			Threshold: fastRiseRatePerHour,
			Note: fmt.Sprintf("折算温升速率 %.2f ℃/小时（阈值 %.2f），超出箱体热容可解释的范围",
				*rate, fastRiseRatePerHour),
		})
	}

	return domain.AttributionCandidate{
		CauseCode:  CauseProbeDetached,
		Label:      causeLabels[CauseProbeDetached],
		Confidence: confidence,
		Evidence:   evidence,
		Note:       notes,
	}, true
}

// ruleProbeStuck 判「探头卡死」。
//
// 卡死是读数一动不动，与探头脱落的突变正好相反，两者的处置完全不同：
// 卡死要换探头或重新校准，脱落要重新固定探头。
// 注意它的含义：读数恒定时，越限这件事本身可能只是读数假象，
// 箱内温度到底多少要靠另一支温度计确认。
func ruleProbeStuck(features domain.ExcursionFeatures, _ AttributionOptions) (domain.AttributionCandidate, bool) {
	if features.ProbeStuckSampleCount < probeStuckThresholdSamples {
		return domain.AttributionCandidate{}, false
	}

	evidence := []domain.AttributionEvidence{{
		Feature:   "probe_stuck_sample_count",
		Value:     float64(features.ProbeStuckSampleCount),
		Threshold: probeStuckThresholdSamples,
		Note: fmt.Sprintf("观测段内 %d 条读数恒定不动（阈值 %d 条连续恒定读数）",
			features.ProbeStuckSampleCount, probeStuckThresholdSamples),
	}}
	notes := "读数恒定不动时越限可能只是读数假象，须用另一支温度计现场确认箱内温度"

	return domain.AttributionCandidate{
		CauseCode:  CauseProbeStuck,
		Label:      causeLabels[CauseProbeStuck],
		Confidence: confidenceProbeStuck,
		Evidence:   evidence,
		Note:       notes,
	}, true
}

// ruleACFailure 判「制冷异常」。
//
// 这一条全靠现象反推：设备带电运行（没有断电事件）、柜门没开过、
// 没有除霜、湿度没有明显变化，箱内温度却持续上行——那就不是外部扰动，
// 是制冷能力本身出了问题。
//
// 前提是「没发生」这件事是**知道的**，而不是「不知道」：
// 设备不上报事件时本条不成立——那种情况下断电与制冷故障分不开，
// 硬挑一个就是在编结论。
//
// 它没有直接证据，基线只给 0.45：结论要落到「请设备工程师检查制冷与风道」，
// 而不是「已经查明是制冷故障」。
func ruleACFailure(features domain.ExcursionFeatures, opts AttributionOptions) (domain.AttributionCandidate, bool) {
	// 设备不上报设备事件时，断电与制冷故障分不开，这条不成立。
	if !features.DeviceEventsReported {
		return domain.AttributionCandidate{}, false
	}
	// 三类设备事件都要能确认「没发生」，否则这条结论没有立足点。
	if features.PowerLossObserved == nil || *features.PowerLossObserved {
		return domain.AttributionCandidate{}, false
	}
	if features.DefrostObserved == nil || *features.DefrostObserved {
		return domain.AttributionCandidate{}, false
	}
	if features.DoorOpenMinutes == nil || *features.DoorOpenMinutes > acFailureMaxDoorMinutes {
		return domain.AttributionCandidate{}, false
	}
	rate := features.RiseRatePerHour
	if rate == nil || *rate < acFailureMinRiseRatePerHour {
		return domain.AttributionCandidate{}, false
	}
	if opts.Window == nil || opts.Window.DurationMinutes < acFailureMinDurationMinutes {
		return domain.AttributionCandidate{}, false
	}

	confidence := confidenceACFailure
	evidence := []domain.AttributionEvidence{{
		Feature:   "rise_rate_per_hour",
		Value:     *rate,
		Threshold: acFailureMinRiseRatePerHour,
		Note: fmt.Sprintf("箱内温度持续上行，折算温升速率 %.2f ℃/小时（阈值 %.2f）",
			*rate, acFailureMinRiseRatePerHour),
	}, {
		Feature:   "excursion_duration_minutes",
		Value:     float64(opts.Window.DurationMinutes),
		Threshold: acFailureMinDurationMinutes,
		Note: fmt.Sprintf("越温持续 %d 分钟（阈值 %d 分钟），长时间不回落",
			opts.Window.DurationMinutes, acFailureMinDurationMinutes),
	}, {
		Feature:   "device_event:power_loss_absent",
		Value:     0,
		Threshold: 1,
		Note:      "观测范围内未上报断电事件，制冷设备带电运行",
	}, {
		Feature:   "door_open_minutes",
		Value:     float64(*features.DoorOpenMinutes),
		Threshold: acFailureMaxDoorMinutes,
		Note: fmt.Sprintf("观测范围内开门累计 %d 分钟（阈值 %d 分钟），柜门基本未开",
			*features.DoorOpenMinutes, acFailureMaxDoorMinutes),
	}}
	notes := "本条由现象反推，须由设备工程师现场确认制冷与风道"

	if rise := features.HumidityRisePct; rise != nil {
		if *rise <= humidityFlatThresholdPct {
			evidence = append(evidence, domain.AttributionEvidence{
				Feature:   "humidity_rise_pct",
				Value:     *rise,
				Threshold: humidityFlatThresholdPct,
				Note: fmt.Sprintf("箱内湿度上升 %.2f 个百分点（阈值 %.2f），无湿空气进入迹象",
					*rise, humidityFlatThresholdPct),
			})
		} else {
			notes = appendNote(notes, "但窗口内湿度上升 %.2f 个百分点，存在湿空气进入的可能，请一并核对柜门与风道", *rise)
		}
	}

	return domain.AttributionCandidate{
		CauseCode:  CauseACFailure,
		Label:      causeLabels[CauseACFailure],
		Confidence: confidence,
		Evidence:   evidence,
		Note:       notes,
	}, true
}

// ruleOverstock 判「超量存放」。
//
// 装载量超出额定装载时，同样的开门时长、同样的环境温度，
// 箱内升温会比空箱明显得多——热容大，散不掉。
//
// 判据只有一个：在库件数与额定装载容量。两者缺一，这条就不成立：
// 「可能装多了」不是证据，「在库 480 件、额定 300 件」才是。
func ruleOverstock(features domain.ExcursionFeatures, opts AttributionOptions) (domain.AttributionCandidate, bool) {
	over, value, threshold, feature := opts.Load.overstocked()
	if !over {
		return domain.AttributionCandidate{}, false
	}
	rate := features.RiseRatePerHour
	if rate == nil || *rate < minRiseRatePerHour {
		return domain.AttributionCandidate{}, false
	}

	loadNote := fmt.Sprintf("在库装载 %.0f%%（阈值 100%%），超出额定装载", value)
	if feature == "load_quantity" {
		loadNote = fmt.Sprintf("在库 %d 件，超出额定装载 %d 件（阈值 %d 件）",
			int(value), int(value-threshold), int(threshold))
	}

	confidence := confidenceOverstock
	evidence := []domain.AttributionEvidence{{
		Feature:   feature,
		Value:     value,
		Threshold: threshold,
		Note:      loadNote,
	}, {
		Feature:   "rise_rate_per_hour",
		Value:     *rate,
		Threshold: minRiseRatePerHour,
		Note: fmt.Sprintf("折算温升速率 %.2f ℃/小时（阈值 %.2f），满载时散热更慢",
			*rate, minRiseRatePerHour),
	}}
	notes := "超量存放会抬高箱内热容、减慢散热，须与实际装载核对后再定"

	return domain.AttributionCandidate{
		CauseCode:  CauseOverstock,
		Label:      causeLabels[CauseOverstock],
		Confidence: confidence,
		Evidence:   evidence,
		Note:       notes,
	}, true
}

// unknownPayload 造「判不出原因」时的结论。
//
// 它不是失败，是结论：**证据不足以指向任一原因**。
// 随它一起给的是「还差哪条线索」——下一步该去查什么，
// 而不是一句「无法判断」把人堵在原地。
func unknownPayload(features domain.ExcursionFeatures, notes []string) *domain.AttributionPayload {
	clues := missingClues(features)
	evidence := []domain.AttributionEvidence{{
		Feature:   "attribution_evidence_count",
		Value:     0,
		Threshold: 1,
		Note:      "本次观测没有出现可指向任一原因的现场特征（开门 / 断电 / 除霜 / 读数突变 / 读数恒定均未触发）",
	}}

	return &domain.AttributionPayload{
		CauseCode:  CauseUnknown,
		CauseLabel: causeLabels[CauseUnknown],
		Confidence: 0,
		Candidates: []domain.AttributionCandidate{{
			CauseCode:  CauseUnknown,
			Label:      causeLabels[CauseUnknown],
			Confidence: 0,
			Evidence:   evidence,
			Note:       "证据不足以指向任一原因，请补齐下列线索后重判",
		}},
		Evidence:        evidence,
		MissingFeatures: features.MissingFeatures,
		MissingClues:    clues,
		Notes:           notes,
	}
}

// missingClues 列出「还差哪条线索才能把原因判实」。
//
// 这份清单直接决定下一步该做什么：查设备日志、补湿度通道、
// 核对装载量——每一条都是现场做得到的动作。
func missingClues(features domain.ExcursionFeatures) []string {
	clues := make([]string, 0, 5)
	if !features.DeviceEventsReported {
		clues = append(clues,
			"设备不上报设备事件（开门 / 断电 / 除霜），无法区分开门、断电与除霜；须查记录仪日志或柜体门磁")
	}
	if features.DoorOpenMinutes == nil {
		clues = append(clues, "开门时长未知，须补设备事件上报或现场值守记录")
	}
	if features.HumidityRisePct == nil {
		clues = append(clues, "湿度上升幅度未知，无法区分开门引入湿空气与制冷中断；须确认湿度通道是否启用")
	}
	if features.RiseRatePerHour == nil {
		clues = append(clues, "温升速率未知，无从判断这次越温是持续上行还是瞬时跳变")
	}
	if features.SampleJumpCount == nil {
		clues = append(clues, "相邻采样跳变未评估，无法判断探头是否脱落")
	}
	if features.ProbeStuckSampleCount < probeStuckThresholdSamples {
		clues = append(clues, "读数恒定段不成立，无法判断探头是否卡死")
	}
	if len(clues) == 0 {
		clues = append(clues,
			"现场特征齐备但都未触发任何判据；须核对温区配置与采样节奏是否与现场实际一致")
	}
	return clues
}

// applyPrecisionLimit 把精度不足的标注与封顶落到结论上。
//
// 精度不足时：**每一条**候选都要带标注、置信度一律封顶。
// 只标在总头上是不够的——复核的人常常是逐条候选看的，
// 标注漏在一边，就等于没有标注。
func applyPrecisionLimit(payload *domain.AttributionPayload, features domain.ExcursionFeatures) *domain.AttributionPayload {
	if !features.PrecisionLimited {
		return payload
	}

	tag := features.PrecisionTag
	if tag == "" {
		tag = PrecisionTagLimited
	}
	payload.PrecisionLimited = true
	payload.PrecisionTag = tag

	for i := range payload.Candidates {
		candidate := &payload.Candidates[i]
		if candidate.Confidence > ConfidenceCapPrecisionLimited {
			candidate.Confidence = ConfidenceCapPrecisionLimited
		}
		candidate.Tag = tag
		if candidate.Note != "" {
			candidate.Note += "；" + tag
		} else {
			candidate.Note = tag
		}
	}
	if payload.Confidence > ConfidenceCapPrecisionLimited {
		payload.Confidence = ConfidenceCapPrecisionLimited
	}
	payload.Notes = append(payload.Notes, fmt.Sprintf(
		"%s：设备声明测温精度 %s，已将全部候选置信度封顶在 %.2f",
		tag, accuracyText(features), ConfidenceCapPrecisionLimited))
	return payload
}

// accuracyText 把精度声明拼成一句人话。
func accuracyText(features domain.ExcursionFeatures) string {
	if features.AccuracyC == nil {
		return "未声明"
	}
	return fmt.Sprintf("%.2f℃（规范上限 %.2f℃）", *features.AccuracyC, features.AccuracyLimitC)
}

// appendNote 往候选说明里追加一句。
//
// 前一句为空时直接接上，否则用分号隔开——说明文字开头挂一个分号，
// 读起来像半句话被截断了。
func appendNote(notes string, format string, args ...any) string {
	text := fmt.Sprintf(format, args...)
	if strings.TrimSpace(notes) == "" {
		return text
	}
	return notes + "；" + text
}

// capCandidates 给每条候选的置信度封顶。
//
// 归因是判断不是测量：再硬的证据也只是「最可能是这个原因」。
// 处置结论永远由药学部主管确认，置信度不该看起来像已经确认过了。
func capCandidates(candidates []domain.AttributionCandidate) {
	for i := range candidates {
		if candidates[i].Confidence > confidenceMax {
			candidates[i].Confidence = confidenceMax
		}
	}
}

// sortCandidates 按证据强度给候选排序。
//
// 「证据强度」在这里就是置信度：每条候选的置信度由它满足的阈值条数算出来，
// 满足的阈值越多、且越是直接证据，置信度越高。
//
// 同分时按证据条数、再按固定的原因优先级排：
// 断电 > 开门 > 除霜 > 探头脱落 > 探头卡死 > 制冷异常 > 超量存放。
// 这个顺序不是偏好，是现场处置的顺序——断电要查配电，开门要查值守，
// 探头问题要查设备，最后才是装载量。
func sortCandidates(candidates []domain.AttributionCandidate) {
	priority := map[string]int{
		CausePowerLoss:     0,
		CauseDoorOpen:      1,
		CauseDefrost:       2,
		CauseProbeDetached: 3,
		CauseProbeStuck:    4,
		CauseACFailure:     5,
		CauseOverstock:     6,
		CauseUnknown:       7,
	}
	// 先把置信度收敛到两位小数再排序：浮点尾数会让 0.90 与 0.8999999999999999
	// 被当成两档，排序结果偶尔会翻面。
	for i := range candidates {
		candidates[i].Confidence = round2(candidates[i].Confidence)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if left.Confidence != right.Confidence {
			return left.Confidence > right.Confidence
		}
		if len(left.Evidence) != len(right.Evidence) {
			return len(left.Evidence) > len(right.Evidence)
		}
		return priority[left.CauseCode] < priority[right.CauseCode]
	})
}

// candidateSummary 把候选集拼成一句话，给结论的 Notes 用。
func candidateSummary(candidates []domain.AttributionCandidate) string {
	parts := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		parts = append(parts, fmt.Sprintf("%s(%.2f)", candidate.CauseCode, candidate.Confidence))
	}
	return strings.Join(parts, "、")
}

// round2 把置信度收敛到两位小数。
//
// 浮点尾数（0.8999999999999999）写进要递出去的表格里只会让人以为
// 这套判据算得比实际更精细。
func round2(value float64) float64 {
	return math.Round(value*100) / 100
}

// featuresAreEmpty 判断特征集是否根本没算过。
//
// 「没算过」与「算了但什么都没发生」必须分开：
// 前者是调用方的问题（该报错），后者是正常结论（给 unknown）。
func featuresAreEmpty(features domain.ExcursionFeatures) bool {
	return features.RiseRateSamples == 0 &&
		features.ProbeStuckSampleCount == 0 &&
		features.DoorOpenMinutes == nil &&
		features.PowerLossObserved == nil &&
		features.DefrostObserved == nil &&
		features.HumidityRisePct == nil &&
		features.SampleJumpCount == nil &&
		features.PeakDeviationC == nil &&
		!features.DeviceEventsReported
}
