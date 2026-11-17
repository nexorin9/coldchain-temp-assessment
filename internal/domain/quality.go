// 本文件放「成稿要素完整性校验」的形状：一份稿子齐不齐、差哪几项、
// 缺的这几项挡不挡写回。
//
// 为什么要有这个形状：一份《温度异常情况记录表》会被递到药事会与疫苗生产企业面前。
// 递出去的东西有两种坏法——
//
//   - **缺了一半**：点位有了、峰值有了，批次与处置空着，收件人却以为那就是全部；
//   - **多了一半**：缺的地方被系统填了个像样的值，看着齐全，其实没有依据。
//
// 第二种更坏，所以本系统遇到缺项一律写「待补」并把缺项列出来（见 drafting 包），
// 但「写一句待补」只是礼貌，不算管控。**缺项得有个账**：谁去看、哪一项挡着写回、
// 这一份稿子能不能拿去当台账写回的依据——都由这个形状回答。
//
// 缺项分两级，这是本文件最要紧的一条语义：
//
//   - **必备要素（blocking）**：点位、设备、越限起止、峰值温度、持续分钟、所属温区、
//     原因结论、生成时间。缺任何一项，这份稿子就在陈述一件说不清的事，
//     **不得作为台账写回载荷的来源**。
//   - **待人工补齐要素（pending）**：涉及批次、处置建议。这两项按业务定义本来就
//     要人来补——批次号只能由冷链台账给（台账不可达时确实拿不到），
//     最终处置由药学部主管确认（确认之前就是还没定）。缺它们时稿子带
//     needs_review 并列进缺项清单，但**不挡写回**：台账如实收下
//     「这一栏还没定、由主管定了」这个事实，比不收更安全。
//
// 把这两级混成一级会有两种坏结果：全判成阻断，则没有任何一份稿子能写回台账
// （越温记录反而记不下来）；全判成不阻断，则要素全缺的空壳稿也能进台账。

package domain

import "time"

// 成稿要素名。
//
// 这些名字就是缺项清单里写的那几个名字（missing_fields 里的取值），
// 定在 domain 包里，是为了让 drafting、runtime、workflow 三处用的是同一套名字——
// 名字不一致的话，「这一稿还差什么」在三个地方会给出三个答案。
const (
	// DraftElementPointID 是点位编号。
	DraftElementPointID = "point_id"
	// DraftElementDeviceName 是设备名称。
	DraftElementDeviceName = "device_name"
	// DraftElementExcursionStartedAt 是越限起始时间。
	DraftElementExcursionStartedAt = "excursion_started_at"
	// DraftElementExcursionEndedAt 是越限结束时间。
	DraftElementExcursionEndedAt = "excursion_ended_at"
	// DraftElementPeakTemperatureC 是峰值温度。
	DraftElementPeakTemperatureC = "peak_temperature_c"
	// DraftElementDurationMinutes 是越限持续分钟。
	DraftElementDurationMinutes = "duration_minutes"
	// DraftElementZone 是所属温区。
	DraftElementZone = "zone"
	// DraftElementCauseConclusion 是原因结论。
	DraftElementCauseConclusion = "cause_conclusion"
	// DraftElementBatches 是涉及批次。
	DraftElementBatches = "batches"
	// DraftElementDisposition 是处置建议（最终处置）。
	DraftElementDisposition = "disposition"
	// DraftElementGeneratedAt 是生成时间。
	DraftElementGeneratedAt = "generated_at"
)

// DraftQuality 是成稿要素完整性校验的结论。
//
// 它随成稿产物一起落库：确认动作发生在判定之后、进程重启之后，
// 复核的人问「这份稿子当时齐不齐、差哪几项」，答案必须还在库里，
// 而不是要靠人记得当时屏幕上出现过什么。
type DraftQuality struct {
	// Complete 表示十个要素全部齐备。
	//
	// 注意它在确认之前几乎总是 false：处置建议这一栏要到药学部主管
	// 确认之后才算齐。这是事实，不是缺陷。
	Complete bool `json:"complete"`
	// Qualified 表示必备要素齐备——这份稿子可以作为台账写回载荷的来源。
	//
	// 待人工补齐的要素（批次、处置）不影响这个结论。
	Qualified bool `json:"qualified"`
	// MissingFields 是全部缺项，按固定的要素顺序排列。
	MissingFields []string `json:"missing_fields,omitempty"`
	// BlockingFields 是挡着写回的缺项（必备要素缺了哪几个）。
	BlockingFields []string `json:"blocking_fields,omitempty"`
	// PendingFields 是待人工补齐的缺项（不挡写回，但要有人去补）。
	PendingFields []string `json:"pending_fields,omitempty"`
	// PrecisionLimited 表示设备测温精度声明超出规范上限，本稿数值仅供参考。
	PrecisionLimited bool `json:"precision_limited,omitempty"`
	// PrecisionTag 是精度不足时的标注文案：「精度不足，仅供参考」。
	PrecisionTag string `json:"precision_tag,omitempty"`
	// PrecisionDetail 是精度不足的具体数值说明（声明值与上限值）。
	PrecisionDetail string `json:"precision_detail,omitempty"`
	// Degraded 表示本次原因判断来自规则基线（模型归因不可达时退回）。
	Degraded bool `json:"degraded,omitempty"`
	// DegradeReason 是降级原因的错误码。
	DegradeReason string `json:"degrade_reason,omitempty"`
	// CheckedAt 是校验时刻。
	CheckedAt time.Time `json:"checked_at"`
}

// NeedsReview 表示这份稿子还有待人工处理的事。
//
// 只要有缺项就为 true：缺一项就是有人得去补，补完了才算齐。
func (q *DraftQuality) NeedsReview() bool {
	return q != nil && len(q.MissingFields) > 0
}

// Exportable 表示这份稿子能不能作为台账写回载荷的来源。
//
// 判据只有一条：必备要素齐备（BlockingFields 为空）。
// 待人工补齐的要素不挡写回——台账如实收下「还没定」这个事实即可。
func (q *DraftQuality) Exportable() bool {
	return q != nil && q.Qualified
}
