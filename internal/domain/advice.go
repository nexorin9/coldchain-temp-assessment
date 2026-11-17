// 本文件放的是「批次可用性建议」的两块形状：逐批次建议与整次越温的处置建议。
//
// 它们放在 domain 而不是 drafting 里，是因为这两块东西要跟着成稿步骤产物一起
// 落库（写台账、算统计、给内审调阅都要读它们），产物不能反过来依赖成稿包，
// 否则成稿的写法一动，库里存的历史建议就再也解不开了。
//
// 一条纪律贯穿这两个形状：**系统只给建议，不给结论**。
//
// 批次到底能不能用，由疫苗生产企业评估质量影响、再由药学部主管确认决定。
// 所以这里的每个处置取值都是「建议动作」，每个理由都挂着可核对的依据，
// 任何一句「可放心使用」「质量不受影响」都不该出现在这个结构里。

package domain

// BatchAdvice 是一条在库批次的可用性建议。
type BatchAdvice struct {
	// BatchRef 是台账给的批次号，原样引用，系统不生成新号。
	BatchRef string `json:"batch_ref"`
	// ProductName 是制品名，与台账在库清单一致。
	ProductName string `json:"product_name,omitempty"`
	// Quantity 是该批次的数量。
	Quantity int `json:"quantity,omitempty"`
	// ExpiryDate 是该批次的有效期。
	ExpiryDate string `json:"expiry_date,omitempty"`
	// Zone 是该批次自己所属制品的温区。
	//
	// 不是点位温区：同一台冷柜里可能既有 2~8℃ 的疫苗、又有 0~10℃ 的冷藏药品，
	// 按点位温区一刀切，会把温区要求更松的那批货也拖成「越了」。
	// 温区取不到时留空——不拿点位温区顶替制品温区。
	Zone string `json:"zone,omitempty"`
	// ZoneBasis 是这个温区的出处（点位配置条目名或说明书条款）。
	ZoneBasis string `json:"zone_basis,omitempty"`
	// ExposureMinutes 是该批次在本次越温窗口内的暴露时长（分钟）。
	//
	// 缺测时为 nil（不拿 0 冒充「暴露了 0 分钟」）：批次登记在别的点位时
	// 记 0（本次越温确实没涉及它），本次判定没有窗口时留 nil（不知道）。
	ExposureMinutes *int `json:"exposure_minutes,omitempty"`
	// DeviationC 是峰值相对该批次温区边界的偏离（℃）。
	//
	// 未越出该批次温区时为 0；温区未知时也是 0，但那种情况下 Affected 为 false，
	// 理由里会写明「温区未登记、判不了」，不把「判不了」说成「没越」。
	DeviationC float64 `json:"deviation_c"`
	// Exposed 表示本次越温是否涉及该批次。
	//
	// 批次登记在别的点位时为 false：不是「没受影响」，而是「这次越温压根没轮到它」，
	// 两种情况在建议里要说的话不一样。
	Exposed bool `json:"exposed"`
	// Affected 表示本次越温是否越出了该批次自己的温区。
	Affected bool `json:"affected"`
	// Disposition 是给这一批的建议处置，取值域见处置结论取值。
	Disposition string `json:"disposition"`
	// AdviceText 是建议正文：写「建议先隔离并送疫苗生产企业评估」这类可核对的话，
	// 不写「可以使用」「质量不受影响」这类系统无权下的结论。
	AdviceText string `json:"advice_text"`
	// Rationale 是这条建议的理由。
	Rationale string `json:"rationale"`
	// Evidence 是支撑这条建议的证据：归因证据 + 与本批次相关的窗口证据。
	//
	// 每条证据都带数值与阈值，复核的人不用猜「凭什么建议隔离」。
	Evidence []AttributionEvidence `json:"evidence,omitempty"`
}

// DispositionAdvice 是一次越温的处置建议结论。
//
// 它同时给整体处置与逐批次建议：整体处置是「这份表怎么处置」，
// 逐批次建议是「每一批货分别怎么办」。两者分开，是因为一批货受影响
// 不代表另一批也受影响——按点位一刀切会让没受影响的货陪绑。
type DispositionAdvice struct {
	// Disposition 是整体建议处置，取值域见处置结论取值。
	Disposition string `json:"disposition"`
	// Note 是处置说明正文：这次为什么这么建议、谁来定。
	Note string `json:"note"`
	// Rationale 是整体建议的依据（窗口事实 + 原因结论 + 证据条数）。
	Rationale string `json:"rationale"`
	// BatchAdvices 是逐批次建议。
	BatchAdvices []BatchAdvice `json:"batch_advices,omitempty"`
	// WithheldDisposition 记着被系统压下来的上游处置取值。
	//
	// 「放行」与「报损」是人工决定事项：药学部主管确认后才成立，
	// 系统不作为建议给出。压下来不吞掉——记在这里，谁都能查。
	WithheldDisposition string `json:"withheld_disposition,omitempty"`
	// FilteredPhrases 记着从正文里过滤掉的越权措辞。
	//
	// 过滤不是静默改写：改了什么、为什么改，都留痕。
	FilteredPhrases []string `json:"filtered_phrases,omitempty"`
}
