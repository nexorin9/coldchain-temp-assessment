// 本文件是**降级路径**：模型归因不可用时，退回规则基线归因，结论停在待主管确认。
//
// 为什么必须有这条路径：模型端点是院内自建或合规云端都可能临时不可用的东西。
// 冷库夜里断电，疫苗已经越温了——判定不能因为「模型此刻连不上」就做不出来。
// 但降级不等于放行：降级出来的结论照样只是草稿，一样要主管确认，
// 一样不写台账。这条路径的产出必须比在线路径**更显眼地标出自己不可靠**。
//
// 四类失败触发降级：
//
//	E_MODEL_TIMEOUT        —— 端点没在时限内回话；
//	E_MODEL_UNAVAILABLE    —— 端点连不上，或回 5xx / 429；
//	E_MODEL_PROTOCOL       —— 回的不是一个合法的 OpenAI-compatible 信封；
//	E_MODEL_SCHEMA_INVALID —— 信封合法，但内容没通过结构化校验（重问一次仍不过）；
//	E_MODEL_FACT_MISMATCH  —— 内容结构对，但数值与本次输入事实对不上。
//
// 后两类也降级，是因为它们同样说明「这次的模型结论不可用」：形状不合规是
// 模型没按要求答，事实不符是模型答了但答的不是现场发生的事。两者都拿不到
// 可用的归因结论，就都退回规则基线。
//
// 三条不许让步的约定：
//
//  1. **降级不推进业务状态**：降级产出的是一份带 degraded 标记的归因结论，
//     run 停在 awaiting_confirmation。不自动进 confirmed，不触发出站写回。
//     批次能不能用始终由疫苗生产企业评估质量影响、再由药学部主管确认。
//  2. **降级要封顶置信度**：降级结论只有规则基线一条路撑着，未经模型交叉印证，
//     置信度一律封顶在 ConfidenceCapDegraded，不许看起来像已经查实了。
//  3. **降级要在 run 与审计链上都看得见**：归因产物带 degraded 与降级原因，
//     run 快照带同样的标记，审计链记一条 outcome=degraded 的条目。
//     复核的人必须能一眼看出「这条结论是降级来的」，不能靠猜。
//
// 源产品机制对应（溯源见 spec.md「开源对照」与「参考地基」）：
// 源产品的 model-prices 主路径在投影时**显式剔除「看着像数据、其实是占位」的条目**
// ——文档占位键 sample_spec、两项成本都为 0 的空壳条目，注释写明
// 「excluding it avoids a real model's usage silently costing nothing」。
// 搬到冷链场景，对应物就是这条降级路径：
// **模型给不出可用结论时，宁可退回可复算的规则基线，也不让一条编出来的结论
// 悄悄混进产物里；而且退回来的那条必须显式标出它是退回来的。**
package model

import (
	"fmt"
	"strings"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/rules"
)

const (
	// ConfidenceCapDegraded 是降级结论的置信度上限。
	//
	// 取 0.60：规则基线里「开门」这类由现象反推的结论基线就是 0.60，
	// 而「断电」这类设备直接上报的证据基线是 0.90。降级意味着**模型这一路
	// 没有参与交叉印证**——只有一条路撑着的结论不该看起来比两条路互相印证过
	// 的结论更硬，所以一律压到 0.60。
	//
	// 精度不足时的封顶（0.50）比它更低，两个封顶取更严的那个：
	// 读数本身不可信时，规则基线也不该给出比 0.50 更高的置信度。
	ConfidenceCapDegraded = 0.60

	// DegradeTag 是降级结论的显眼标注。
	//
	// 它要跟着**每一条候选**走，不只标在总头上：复核的人常常是逐条候选看的，
	// 标注漏在一边就等于没有标注（与精度不足标注同一纪律）。
	DegradeTag = "降级结论（规则基线）"
)

// degradableCodes 是触发降级的错误码。
//
// 白名单而不是黑名单：只有这几类错误明确意味着「模型这一路没给出可用结论」。
// 校验类错误（E_VALIDATION）、存储错误（E_STORAGE_ERROR）不在名单里——
// 它们不是模型的锅，降级会把「系统自己出了问题」掩盖成「模型不可用」，
// 那会让现场去查错方向。
var degradableCodes = map[errcode.Code]bool{
	errcode.ModelTimeout:       true,
	errcode.ModelUnavailable:   true,
	errcode.ModelProtocol:      true,
	errcode.ModelSchemaInvalid: true,
	errcode.ModelFactMismatch:  true,
}

// IsDegradable 判断一个错误是否触发降级。
//
// 调用方据此决定「退回规则基线」还是「照常报错让判定失败」。
func IsDegradable(err error) bool {
	if err == nil {
		return false
	}
	return degradableCodes[errcode.As(err).Code]
}

// DegradeReasonOf 取出降级原因的错误码；不属于可降级范围时返回空串。
func DegradeReasonOf(err error) string {
	if err == nil {
		return ""
	}
	code := errcode.As(err).Code
	if !degradableCodes[code] {
		return ""
	}
	return string(code)
}

// DegradeInput 是降级到规则基线所需的输入。
//
// 它与在线路径共用同一批依据：同一份温度采样、同一个越温窗口、同一份温区结论。
// 两条路看的不是同一份数据的话，降级出来的结论就没法与在线结论对照，
// 「模型不可达时退到哪儿」也就失去了意义。
type DegradeInput struct {
	// Samples 是这次判定依据的温度采样（规则基线的唯一读数来源）。
	Samples []domain.TemperatureEvent
	// Window 是规则抽出的越温窗口；归因的时长判据要用它。
	Window *domain.ExcursionWindow
	// Zone 是点位温区结论（特征里的峰值偏离度、湿度基线都按温区算）。
	Zone rules.ZoneDecision
	// Load 是在库装载情况；为 nil 时「超量存放」这条线索不可用。
	Load *rules.LoadInfo
}

// DegradeToBaseline 把一次失败的模型调用降级成规则基线归因结论。
//
// 返回的结论带 degraded=true、降级原因错误码与封顶后的置信度。
// cause 不属于可降级范围时直接报错——不把「系统自己的问题」说成「模型不可用」。
func DegradeToBaseline(cause error, in DegradeInput) (*domain.AttributionPayload, error) {
	if cause == nil {
		return nil, errcode.Newf(errcode.Validation, "降级没有触发原因，无法退回规则基线")
	}
	reason := DegradeReasonOf(cause)
	if reason == "" {
		return nil, errcode.Newf(errcode.Validation,
			"错误 %s 不属于可降级范围（模型超时 / 不可达 / 协议不合规 / 形状不合规 / 事实不符）",
			errcode.As(cause).Code)
	}
	if len(in.Samples) == 0 {
		return nil, errcode.Newf(errcode.Validation,
			"降级到规则基线需要温度采样，本次没有取到采样，无法给出可核对的结论")
	}
	if in.Window == nil {
		return nil, errcode.Newf(errcode.Validation,
			"降级到规则基线需要越温窗口，本次没有抽到窗口，无法给出可核对的结论")
	}

	payload, err := rules.AttributeSamples(in.Samples, in.Window, in.Zone,
		rules.AttributionOptions{Window: in.Window, Load: in.Load})
	if err != nil {
		return nil, errcode.Newf(errcode.Validation,
			"规则基线归因失败，无法完成降级：%s", errcode.As(err).Details)
	}
	markDegraded(payload, reason, cause)
	return payload, nil
}

// markDegraded 把降级标记、置信度封顶与降级说明落到结论上。
//
// 置信度封顶作用在结论头与**每一条候选**上：复核的人逐条看候选时，
// 每一条都得看得出它是降级来的、且它的置信度没有越过封顶线。
func markDegraded(payload *domain.AttributionPayload, reason string, cause error) {
	if payload == nil {
		return
	}
	payload.Degraded = true
	payload.DegradeReason = reason

	if payload.Confidence > ConfidenceCapDegraded {
		payload.Confidence = ConfidenceCapDegraded
	}
	for i := range payload.Candidates {
		candidate := &payload.Candidates[i]
		if candidate.Confidence > ConfidenceCapDegraded {
			candidate.Confidence = ConfidenceCapDegraded
		}
		if candidate.Tag == "" {
			candidate.Tag = DegradeTag
		} else {
			candidate.Tag = candidate.Tag + "；" + DegradeTag
		}
		candidate.Note = appendDegradeNote(candidate.Note, degradeNote(reason, cause))
	}
	payload.Notes = append(payload.Notes, degradeNote(reason, cause))
}

// degradeNote 拼出降级说明。
//
// 措辞分三段：为什么降（错误码与原因）、结论来自哪儿（规则基线单路）、
// 要人做什么（重点核对）。第三段不能省——降级结论照样要递到药事与企业面前，
// 收件人必须知道这条结论的成色。
func degradeNote(reason string, cause error) string {
	msg := fmt.Sprintf("降级：模型归因不可用（%s）", reason)
	if cause != nil {
		if detail := strings.TrimSpace(errcode.As(cause).Details); detail != "" {
			msg += "：" + detail
		}
	}
	return msg + "；本次结论由规则基线单路给出，未经模型交叉印证，需人工重点核对原因判定"
}

// appendDegradeNote 往候选说明里追加一句降级说明。
func appendDegradeNote(existing, note string) string {
	if strings.TrimSpace(existing) == "" {
		return note
	}
	return existing + "；" + note
}

// DegradeNote 返回成稿要写进《温度异常情况记录表》的降级说明。
//
// 不是可选项：一张写着「开门导致」却没写「这是模型不可达时退回来的」的表，
// 递到疫苗生产企业那里会被当成一条查实过的结论。降级这件事必须写在纸上。
func DegradeNote(payload *domain.AttributionPayload) string {
	if payload == nil || !payload.Degraded {
		return ""
	}
	reason := payload.DegradeReason
	if strings.TrimSpace(reason) == "" {
		reason = "模型归因不可用"
	}
	return fmt.Sprintf(
		"本次归因结论来自规则基线（模型归因不可用：%s），未经模型交叉印证；"+
			"须由药学部主管重点核对原因判定后，再决定批次处置结论", reason)
}

// AttributionPayloadFromModel 把一份通过校验的模型归因结论投影成归因步骤产物。
//
// 在线路径与降级路径落到**同一个产物分码**上，用同一套字段。
// 这不是偷懒：成稿、台账写回、报表都按这个分码取产物，两条路径的字段口径
// 必须一致——否则降级一次，下游就得为「降级产物长什么样」再写一套解析，
// 而那套解析永远没人测过。
func AttributionPayloadFromModel(attr *Attribution) *domain.AttributionPayload {
	if attr == nil {
		return nil
	}
	payload := &domain.AttributionPayload{
		CauseCode:  attr.CauseCode,
		CauseLabel: attr.Label,
		Confidence: attr.Confidence,
		Evidence:   convertEvidence(attr.Evidence),
	}
	payload.Notes = append(payload.Notes, attr.Notes...)
	for _, candidate := range attr.Candidates {
		payload.Candidates = append(payload.Candidates, domain.AttributionCandidate{
			CauseCode:  candidate.CauseCode,
			Label:      candidate.Label,
			Confidence: candidate.Confidence,
			Evidence:   convertEvidence(candidate.Evidence),
			Note:       candidate.Note,
		})
	}
	return payload
}

// convertEvidence 把模型侧证据投影成领域证据。
//
// 逐条转、不揉成一段：证据的数值与阈值必须原样可回溯。
func convertEvidence(items []Evidence) []domain.AttributionEvidence {
	if len(items) == 0 {
		return nil
	}
	out := make([]domain.AttributionEvidence, 0, len(items))
	for _, item := range items {
		out = append(out, domain.AttributionEvidence{
			Feature:   item.Feature,
			Value:     item.Value,
			Threshold: item.Threshold,
			Note:      item.Note,
		})
	}
	return out
}
