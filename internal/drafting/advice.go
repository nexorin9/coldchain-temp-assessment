// Package drafting 的这一半负责「批次可用性建议与处置说明」。
//
// 它回答的是药学部在越温之后最关心的两个问题：
//
//  1. 当时在库的那几批货，各自受没受影响？
//  2. 现在该拿这几批货怎么办，谁来定？
//
// 答案必须满足三件事：
//
//   - **按批次分别算**。同一台冷柜里可能既有 2~8℃ 的疫苗、又有 0~10℃ 的冷藏药品，
//     峰值 9℃ 对前者是越限、对后者不是。按点位温区一刀切会把没受影响的货一起拖下水。
//   - **每条建议都挂着依据**。窗口事实、原因结论、证据条数都要落到建议上，
//     没有依据的建议等于没给。
//   - **不越过自己的职责边界**。批次能不能用由疫苗生产企业评估质量影响、
//     再由药学部主管确认决定；系统给的是「建议先隔离并送企业评估」这类可核对表述，
//     不是「可以放心使用」。所以处置取值被限死在一个很小的域里，
//     越权措辞会被过滤器挡下来并留痕。
//
// 它与源产品机制的关系：源仓库的契约转换器（tools/convert-openapi）
// 在把外部契约搬进新契约时要求「每个样例值都必须能落到一个已存在的栏位，
// 落不进去就报错」（example references unknown path / unknown operation），
// 绝不凭空多出一条。搬到建议这里就是同一件事：**取值域之外的处置结论一律拒绝，
// 不自己编一个看着像的**。

package drafting

import (
	"fmt"
	"math"
	"strings"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/model"
)

// DefaultDisposition 是默认建议处置：待疫苗生产企业评估质量影响。
//
// 默认值不是「保守」两个字能解释的：批次能不能用本来就只有疫苗生产企业说了算，
// 所以系统在没有更多依据时，正确的动作是「等着」，而不是猜一个。
const DefaultDisposition = model.DispositionPendingEvaluation

// OverreachReplacement 是越权措辞被过滤后留下的标记。
//
// 它必须显眼：过滤掉一句话不能不留痕，否则复核的人看到的是一份
// 「看起来很干净」的稿子，而干净是过滤出来的，不是本来就有的。
const OverreachReplacement = "〔越权措辞已过滤〕"

// overreachPhrases 是越权措辞清单。
//
// 这些话有一个共同点：都是在替疫苗生产企业或药学部主管下结论。
// 冷链场景里「可放心使用」「质量不受影响」这类话一旦写进要递出去的表，
// 后面复核的人就会以为已经有人判过了——而事实上没有。
var overreachPhrases = []string{
	"可以放心使用",
	"可放心使用",
	"放心使用",
	"可正常使用",
	"不影响质量",
	"质量不受影响",
	"质量合格",
	"无需处理",
	"无需隔离",
	"无需送",
	"建议放行",
	"同意放行",
	"可以放行",
	"直接放行",
	"直接可用",
}

// AdviceInputs 是批次可用性建议的输入。
type AdviceInputs struct {
	// Record 是记录表载荷：在库批次清单从它上面取。
	Record *domain.ExceptionRecordPayload
	// Window 是越温窗口产物：峰值、持续分钟与逐制品温区从它上面取。
	Window *domain.WindowPayload
	// Attribution 是归因产物：原因结论与证据从它上面取。
	Attribution *domain.AttributionPayload
	// PointID 是本次判定点位。
	PointID string
	// RequestedDisposition 是上游（模型或人工）提出的处置取值；空表示未指定。
	//
	// 取值必须落在合法域内，越界一律 E_VALIDATION 拒绝；
	// 但「放行」「报损」虽在域内，属人工决定事项，系统不作为建议给出
	//（压下来的取值记在 WithheldDisposition 里，不吞掉）。
	RequestedDisposition string
	// RequestedNote 是上游给出的说明正文（如模型给出的处置说明）。
	//
	// 它会先过一遍越权措辞过滤再进稿子。
	RequestedNote string
	// Lookup 是在库批次查询结果（可选）。
	//
	// 它只影响一件事：批次清单没拿到时，说明里要写清是「没查成」
	// 还是「查到了但确实没有在库批次」。两种情形对复核的人完全不同，
	// 混成一句「未取得在库批次清单」会让人分不清该去台账补查还是该确认库里没货。
	Lookup *domain.BatchLookup
}

// ValidateDisposition 校验处置结论取值。
//
// 空值视为「未指定」，由调用方取默认值；取值域之外的代码一律拒绝：
// 处置结论写错一个字，批次就可能被误放行，所以这里不「就近取值」也不「猜一个」。
func ValidateDisposition(code string) error {
	if strings.TrimSpace(code) == "" {
		return nil
	}
	for _, v := range model.DispositionValues() {
		if code == v {
			return nil
		}
	}
	return errcode.Newf(errcode.Validation,
		"处置结论取值不合法：%q（合法取值：%s）", code, strings.Join(model.DispositionValues(), "、"))
}

// FilterOverreach 把越权措辞从正文里过滤掉。
//
// 返回过滤后的正文与被过滤掉的措辞清单：过滤是显式的，不是静默改写。
func FilterOverreach(text string) (string, []string) {
	if text == "" {
		return text, nil
	}
	out := text
	var hits []string
	for _, phrase := range overreachPhrases {
		if !strings.Contains(out, phrase) {
			continue
		}
		out = strings.ReplaceAll(out, phrase, OverreachReplacement)
		hits = append(hits, phrase)
	}
	return out, hits
}

// Advise 给这次越温给出批次可用性建议与处置说明。
//
// 返回的结论只到「建议」这一层：它不推进业务状态、不写台账、不代替主管确认。
func Advise(in AdviceInputs) (domain.DispositionAdvice, error) {
	if in.Record == nil {
		return domain.DispositionAdvice{}, errcode.Newf(errcode.Validation,
			"批次建议缺少记录表载荷（ExceptionRecordPayload）")
	}
	if err := ValidateDisposition(in.RequestedDisposition); err != nil {
		return domain.DispositionAdvice{}, err
	}

	advice := domain.DispositionAdvice{Disposition: DefaultDisposition}

	// 「放行」「报损」是人工决定事项：药学部主管确认后才成立。
	// 上游要求给这两个值时，系统压下来并留痕，而不是照着写进稿子。
	if in.RequestedDisposition == model.DispositionRelease ||
		in.RequestedDisposition == model.DispositionScrap {
		advice.WithheldDisposition = in.RequestedDisposition
	}

	batches := in.Record.Batches
	advices := make([]domain.BatchAdvice, 0, len(batches))
	affected := 0
	for _, batch := range batches {
		ba := adviseBatch(batch, in)
		if ba.Affected {
			affected++
		}
		advices = append(advices, ba)
	}
	advice.BatchAdvices = advices

	// 整体处置取各批次建议里最严的一条：有批次越出了自身温区就先隔离。
	// 没有批次、或批次都没越限时，按默认的待企业评估给。
	if affected > 0 {
		advice.Disposition = model.DispositionIsolate
	}

	advice.Note = composeNote(advice, len(batches), affected, in.Lookup)
	advice.Rationale = composeRationale(in)

	// 越权措辞过滤：正文与理由都要过一遍，上游给的说明也要过一遍。
	var hits []string
	var h []string
	advice.Note, h = FilterOverreach(advice.Note)
	hits = append(hits, h...)
	advice.Rationale, h = FilterOverreach(advice.Rationale)
	hits = append(hits, h...)
	for i := range advice.BatchAdvices {
		advice.BatchAdvices[i].AdviceText, h = FilterOverreach(advice.BatchAdvices[i].AdviceText)
		hits = append(hits, h...)
		advice.BatchAdvices[i].Rationale, h = FilterOverreach(advice.BatchAdvices[i].Rationale)
		hits = append(hits, h...)
	}
	if strings.TrimSpace(in.RequestedNote) != "" {
		filtered, h := FilterOverreach(in.RequestedNote)
		hits = append(hits, h...)
		advice.Note += "；上游说明：" + filtered
	}
	advice.FilteredPhrases = uniqueStrings(hits)

	return advice, nil
}

// adviseBatch 给一个在库批次算建议。
//
// 三种情形分开说，不合并成一句「请人工核对」：
//
//   - 批次登记在别的点位：本次越温没涉及它；
//   - 制品温区没登记：判不了它受没受影响；
//   - 温区清楚且越了：建议先隔离并送疫苗生产企业评估。
func adviseBatch(batch domain.Batch, in AdviceInputs) domain.BatchAdvice {
	ba := domain.BatchAdvice{
		BatchRef:    batch.BatchRef,
		ProductName: batch.ProductName,
		Quantity:    batch.Quantity,
		ExpiryDate:  batch.ExpiryDate,
		Disposition: DefaultDisposition,
	}

	zone, zoneFound := zoneOfBatch(in.Window, batch)
	if zoneFound {
		ba.Zone = zone.ZoneLabel
		ba.ZoneBasis = zone.ZoneBasis
	}

	// 暴露时长：批次登记在别的点位 = 本次越温没涉及它，记 0 分钟；
	// 本次判定没有窗口 = 不知道，记 nil。两者不能混。
	exposed := true
	if strings.TrimSpace(batch.PointID) != "" && batch.PointID != in.PointID {
		exposed = false
	}
	switch {
	case !exposed:
		zero := 0
		ba.ExposureMinutes = &zero
	case in.Window != nil && in.Window.Window.DurationMinutes > 0:
		minutes := in.Window.Window.DurationMinutes
		ba.ExposureMinutes = &minutes
	}

	// 偏离：峰值相对该批次自己温区的偏离。
	//
	// 只在本批次确实被本次越温涉及时才算：拿别的点位的峰值去对这个批次的温区，
	// 算出来的是另一个故事。温区未知时不猜，偏离记 0、判「没越」。
	ba.Exposed = exposed
	if exposed && zoneFound && in.Window != nil {
		peak := in.Window.Window.PeakC
		switch {
		case peak > zone.ZoneMaxC:
			ba.DeviationC = roundC(peak - zone.ZoneMaxC)
		case peak < zone.ZoneMinC:
			ba.DeviationC = roundC(zone.ZoneMinC - peak)
		}
	}
	ba.Affected = exposed && zoneFound && ba.DeviationC > 0

	if ba.Affected {
		ba.Disposition = model.DispositionIsolate
	}

	ba.AdviceText = batchAdviceText(ba, in)
	ba.Rationale = batchRationale(ba, in)

	// 证据：归因证据 + 本批次相关的窗口证据。
	ba.Evidence = append(ba.Evidence, attributionEvidenceOf(in.Attribution)...)
	if exposed && zoneFound {
		ba.Evidence = append(ba.Evidence, domain.AttributionEvidence{
			Feature:   "peak_deviation_from_batch_zone_c",
			Value:     ba.DeviationC,
			Threshold: 0,
			Note:      fmt.Sprintf("峰值相对该批次温区 %s 的偏离（℃）", ba.Zone),
		})
	}
	if ba.ExposureMinutes != nil {
		ba.Evidence = append(ba.Evidence, domain.AttributionEvidence{
			Feature:   "excursion_exposure_minutes",
			Value:     float64(*ba.ExposureMinutes),
			Threshold: 0,
			Note:      "该批次在本次越温窗口内的暴露时长（分钟）",
		})
	}

	return ba
}

// zoneOfBatch 取该批次自己所属制品的温区结论。
//
// 先按批次号对（台账给的批次号挂到哪个制品温区下最准），
// 对不上再按制品名对；都对不上就是温区未知，不拿点位温区顶替。
func zoneOfBatch(window *domain.WindowPayload, batch domain.Batch) (domain.ZoneProductDecision, bool) {
	if window == nil {
		return domain.ZoneProductDecision{}, false
	}
	for _, product := range window.Products {
		for _, ref := range product.BatchRefs {
			if ref == batch.BatchRef {
				return product, true
			}
		}
	}
	if strings.TrimSpace(batch.ProductName) != "" {
		for _, product := range window.Products {
			if product.ProductName == batch.ProductName {
				return product, true
			}
		}
	}
	return domain.ZoneProductDecision{}, false
}

// batchAdviceText 写给这一批的建议正文。
//
// 措辞只有一种口径：建议动作 + 谁来定。没有「可以使用」「质量不受影响」。
func batchAdviceText(ba domain.BatchAdvice, in AdviceInputs) string {
	who := "批次是否可用由疫苗生产企业评估质量影响后、再由药学部主管确认决定"
	switch {
	case ba.Affected:
		return fmt.Sprintf(
			"建议先隔离该批次（批次号 %s，制品 %s，数量 %d），并送疫苗生产企业评估本次温度偏离的质量影响；%s",
			ba.BatchRef, ba.ProductName, ba.Quantity, who)
	case !ba.Exposed:
		return fmt.Sprintf(
			"该批次（批次号 %s）台账登记在点位 %s，与本次判定点位 %s 不同，本次温度偏离未涉及该批次；批次可用性仍以冷链台账记录与药学部主管确认为准",
			ba.BatchRef, batchPointOf(in, ba), in.PointID)
	case ba.Zone == "":
		return fmt.Sprintf(
			"该批次（批次号 %s，制品 %s）的制品温区未在点位温区配置中登记，无法判断本次越温是否影响该批次；按待疫苗生产企业评估给出建议，由药学部主管核对后确认",
			ba.BatchRef, ba.ProductName)
	default:
		return fmt.Sprintf(
			"该批次（批次号 %s，制品 %s）温区 %s，本次越温峰值 %s 未越出该批次温区；按待疫苗生产企业评估给出建议，%s",
			ba.BatchRef, ba.ProductName, ba.Zone, peakText(in), who)
	}
}

// batchRationale 写给这一批的建议理由。
func batchRationale(ba domain.BatchAdvice, in AdviceInputs) string {
	cause := causeText(in)
	switch {
	case ba.Affected:
		return fmt.Sprintf(
			"本次越温峰值 %s 越出该批次温区 %s（超出 %s ℃），该批次在窗口内暴露 %d 分钟；原因结论：%s",
			peakText(in), ba.Zone, formatNumber(ba.DeviationC), exposureMinutes(ba), cause)
	case !ba.Exposed:
		return fmt.Sprintf("该批次不在本次判定点位 %s 上，本次温度偏离未涉及它；原因结论：%s", in.PointID, cause)
	case ba.Zone == "":
		return fmt.Sprintf("该批次的制品温区未在点位温区配置中登记，越限影响判不了；本次越温峰值 %s；原因结论：%s",
			peakText(in), cause)
	default:
		return fmt.Sprintf(
			"本次越温峰值 %s 未越出该批次温区 %s，该批次在窗口内暴露 %d 分钟；原因结论：%s",
			peakText(in), ba.Zone, exposureMinutes(ba), cause)
	}
}

// composeNote 写整体处置说明。
func composeNote(advice domain.DispositionAdvice, total, affected int, lookup *domain.BatchLookup) string {
	var note string
	switch {
	case total == 0:
		note = "本次判定未取得在库批次清单，批次可用性无从判断，按待疫苗生产企业评估给出建议；批次清单取得后须重新生成本表。"
		// 批次清单没拿到时，把「没查成」与「查到了但确实没货」分开说：
		// 前者要有人去台账补查，后者只需要在表上确认一句。
		if lookup != nil {
			switch lookup.Status {
			case domain.BatchLookupUnavailable, domain.BatchLookupAmbiguous:
				note = "本次判定" + domain.BatchMissingText + "（" + lookup.Detail +
					"），在库批次清单未取得，批次可用性无从判断，按待疫苗生产企业评估给出建议；批次清单取得后须重新生成本表。"
			case domain.BatchLookupEmpty:
				note = "本次判定在 " + lookup.CheckedAt.Format("2006-01-02 15:04") +
					" 查询冷链台账，该点位无在库批次，批次可用性无从判断，按待疫苗生产企业评估给出建议。"
			}
		}
	case affected == 0:
		note = fmt.Sprintf(
			"本次越温涉及 %d 个在库批次，均未越出各自制品温区，按待疫苗生产企业评估给出建议；批次是否可用由疫苗生产企业评估质量影响后、再由药学部主管确认，系统不给出放行结论。",
			total)
	default:
		note = fmt.Sprintf(
			"本次越温涉及 %d 个在库批次，其中 %d 个越出各自制品温区，建议先隔离并送疫苗生产企业评估质量影响；批次是否可用由疫苗生产企业评估质量影响后、再由药学部主管确认，系统不给出放行结论。",
			total, affected)
	}
	if advice.WithheldDisposition != "" {
		label, ok := model.DispositionLabel(advice.WithheldDisposition)
		if !ok {
			label = advice.WithheldDisposition
		}
		note += fmt.Sprintf("（上游提出的处置结论「%s」属人工决定事项，系统不作为建议给出，本次按默认建议给出。）", label)
	}
	return note
}

// composeRationale 写整体建议依据：窗口事实 + 原因结论 + 证据条数。
func composeRationale(in AdviceInputs) string {
	cause := causeText(in)
	evidence := attributionEvidenceOf(in.Attribution)
	if in.Window == nil {
		return fmt.Sprintf("本次判定没有越温窗口；原因结论：%s；归因证据 %d 条", cause, len(evidence))
	}
	w := in.Window.Window
	return fmt.Sprintf(
		"越温窗口 %s ~ %s，峰值 %s，持续 %d 分钟；原因结论：%s；归因证据 %d 条",
		formatTime(w.Start), formatTime(w.End), peakText(in), w.DurationMinutes, cause, len(evidence))
}

// attributionEvidenceOf 取归因证据：优先取归因产物上的，没有就取记录表载荷自带的。
func attributionEvidenceOf(attribution *domain.AttributionPayload) []domain.AttributionEvidence {
	if attribution != nil && len(attribution.Evidence) > 0 {
		return attribution.Evidence
	}
	return nil
}

// causeText 取原因结论的可读表述：中文短标签 + 原因代码。
func causeText(in AdviceInputs) string {
	if in.Attribution == nil {
		return "未给出"
	}
	code := strings.TrimSpace(in.Attribution.CauseCode)
	label := strings.TrimSpace(in.Attribution.CauseLabel)
	switch {
	case label != "" && code != "":
		return label + "（" + code + "）"
	case label != "":
		return label
	case code != "":
		return code
	default:
		return "未给出"
	}
}

// peakText 取本次越温峰值文本。
func peakText(in AdviceInputs) string {
	if in.Window == nil {
		return "未知"
	}
	return formatTemperature(in.Window.Window.PeakC)
}

// exposureMinutes 取该批次的暴露分钟数文本（缺测写「未知」）。
func exposureMinutes(ba domain.BatchAdvice) int {
	if ba.ExposureMinutes == nil {
		return 0
	}
	return *ba.ExposureMinutes
}

// batchPointOf 取该批次登记的点位标识。
//
// 批次自身带点位标识时用它；不带的（台账按点位查出来的清单通常不带）
// 就说明它来自本次判定点位，不留空字符串进正文。
func batchPointOf(in AdviceInputs, ba domain.BatchAdvice) string {
	for _, batch := range in.Record.Batches {
		if batch.BatchRef == ba.BatchRef && strings.TrimSpace(batch.PointID) != "" {
			return batch.PointID
		}
	}
	return in.PointID
}

// roundC 把偏离值收敛到两位小数。
//
// 浮点减法会留下 1.5999999999999996 这种尾巴，落进要递出去的表里很难看，
// 复核的人还得先怀疑是不是算错了。
func roundC(v float64) float64 {
	return math.Round(v*100) / 100
}

// uniqueStrings 去重并保持顺序。
func uniqueStrings(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		if seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}
