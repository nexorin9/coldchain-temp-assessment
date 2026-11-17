// Package drafting 的成稿质量闸门：这份稿子齐不齐、差哪几项、
// 缺的这几项挡不挡台账写回。
//
// 它的落点是源仓库契约转换器的同一条纪律。源产品在把外部样例搬进契约时，
// 每一个样例都必须逐个落到**已存在**的路径 / 方法 / 状态码栏位，
// 落不进去就报错退出（`example references unknown path` /
// `unknown operation` / `unknown response code` / `example for … is a TODO placeholder`），
// 绝不凭空多出一条，也绝不把占位样例当样例收下。
//
// 搬到冷链场景，对应物就是这一份《温度异常情况记录表》：
//
//   - **每个必备要素都必须能落到一个真实领域字段**，落不进去就判不合格，
//     并把缺项列出来——不拿一个像样的值顶替（那是「凭空多出一条」）。
//   - **占位不是内容**：一栏写着「待补」的记录表不是一份完整的记录表，
//     它不能被当成完整的写回依据（那是「把占位样例当样例收下」）。
//
// 与成稿正文（record.go）的关系：正文负责把有的写清、把没有的写「待补」；
// 质量闸门负责回答「这份稿子能不能用」。两件事分开，正文不会被判定的逻辑
// 搅浑，判定也不会因为正文长了一行就改了结论。
//
// 三条纪律贯穿整个文件：
//
//  1. **缺项分两级**。必备要素缺了，这份稿子就在陈述一件说不清的事，
//     不得进写回载荷；待人工补齐的要素（批次、最终处置）缺了不挡写回——
//     那两栏本来就等着人补，台账如实收下「还没定」比不收更安全。
//  2. **有缺就有账**。缺哪几项、哪几项挡着写回，都写进结论并随产物落库。
//  3. **标注要显眼**。精度不足、结论来自规则基线，这两件事写在稿子最上面，
//     不是塞在末尾的附注里——收件人先看到标题，再看到第一条提醒。
package drafting

import (
	"fmt"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// 精度不足时的固定标注文案。
//
// 它是给收件人看的第一句话：读数本身不可信时，后面那些峰值、持续分钟
// 都只能当「这里发生了温度异常」的线索，不能当量化结论。
const PrecisionTag = "精度不足，仅供参考"

// 降级时的固定提示文案。
//
// 措辞是「原因判断来自规则基线」，不是「原因判断未做」：结论确实做了，
// 只是不是模型做的、没被模型交叉印证过。请重点核对，不是不必看。
const DegradeNotice = "本次原因判断来自规则基线，请重点核对"

// Element 是成稿的一个要素。
type Element struct {
	// Name 是要素名（缺项清单里写的就是它）。
	Name string
	// Label 是中文栏目名，写在稿子上给人看。
	Label string
	// Blocking 表示这一项缺了会不会挡着台账写回。
	Blocking bool
}

// Elements 是成稿的十个必备栏目，顺序固定。
//
// 顺序固定是为了让缺项清单每次长得一样：复核的人看两稿子的缺项清单，
// 能直接逐行对，不用自己去比顺序。
var Elements = []Element{
	{Name: domain.DraftElementPointID, Label: "点位编号", Blocking: true},
	{Name: domain.DraftElementDeviceName, Label: "设备名称", Blocking: true},
	{Name: domain.DraftElementExcursionStartedAt, Label: "越限起始时间", Blocking: true},
	{Name: domain.DraftElementExcursionEndedAt, Label: "越限结束时间", Blocking: true},
	{Name: domain.DraftElementPeakTemperatureC, Label: "峰值温度", Blocking: true},
	{Name: domain.DraftElementDurationMinutes, Label: "越限持续时长", Blocking: true},
	{Name: domain.DraftElementZone, Label: "所属温区", Blocking: true},
	{Name: domain.DraftElementCauseConclusion, Label: "原因结论", Blocking: true},
	{Name: domain.DraftElementBatches, Label: "涉及批次", Blocking: false},
	{Name: domain.DraftElementDisposition, Label: "处置建议", Blocking: false},
	{Name: domain.DraftElementGeneratedAt, Label: "生成时间", Blocking: true},
}

// Quality 是成稿要素完整性校验的结论。
type Quality struct {
	// Complete 表示十个要素全部齐备。
	Complete bool
	// Qualified 表示必备要素齐备，可作为台账写回载荷的来源。
	Qualified bool
	// MissingFields 是全部缺项。
	MissingFields []string
	// BlockingFields 是挡着写回的缺项。
	BlockingFields []string
	// PendingFields 是待人工补齐的缺项。
	PendingFields []string
	// PrecisionLimited 表示设备测温精度声明超出规范上限。
	PrecisionLimited bool
	// PrecisionTag 是精度不足时的标注文案。
	PrecisionTag string
	// PrecisionDetail 是精度不足的具体数值说明。
	PrecisionDetail string
	// Degraded 表示本次原因判断来自规则基线。
	Degraded bool
	// DegradeReason 是降级原因的错误码。
	DegradeReason string
	// CheckedAt 是校验时刻。
	CheckedAt time.Time
}

// NeedsReview 表示还有待人工处理的事（只要有缺项就是）。
func (q Quality) NeedsReview() bool {
	return len(q.MissingFields) > 0
}

// Exportable 表示这份稿子能不能作为台账写回载荷的来源。
func (q Quality) Exportable() bool {
	return q.Qualified
}

// Domain 把校验结论转成可落库的形状。
func (q Quality) Domain() *domain.DraftQuality {
	return &domain.DraftQuality{
		Complete:         q.Complete,
		Qualified:        q.Qualified,
		MissingFields:    q.MissingFields,
		BlockingFields:   q.BlockingFields,
		PendingFields:    q.PendingFields,
		PrecisionLimited: q.PrecisionLimited,
		PrecisionTag:     q.PrecisionTag,
		PrecisionDetail:  q.PrecisionDetail,
		Degraded:         q.Degraded,
		DegradeReason:    q.DegradeReason,
		CheckedAt:        q.CheckedAt,
	}
}

// CheckQuality 校验一份成稿的要素完整性。
//
// 输入是成稿用的那几份产物（记录表载荷、取数、窗口、归因），不是单看记录表载荷。
// 原因有两个：
//
//   - 峰值温度与持续分钟这两个数值，0 都是合法读数（0℃ 是常规冷藏温度，
//     0 分钟的单点越限也真实存在）。单看一个 float 没法区分「就是 0」与
//     「没人填」，所以它们有没有，取决于**产生它们的那份窗口产物在不在**。
//   - 原因结论要看归因产物：原因码可能是「unknown」，那也是有结论的结论
//     （证据不足，如实写 unknown），不是缺项。
//
// now 是校验时刻；传零值时用时间零（便于测试断言）。
func CheckQuality(in Inputs, now time.Time) Quality {
	q := Quality{CheckedAt: now}

	present := elementPresence(in)
	for _, element := range Elements {
		if present[element.Name] {
			continue
		}
		q.MissingFields = append(q.MissingFields, element.Name)
		if element.Blocking {
			q.BlockingFields = append(q.BlockingFields, element.Name)
		} else {
			q.PendingFields = append(q.PendingFields, element.Name)
		}
	}

	q.Complete = len(q.MissingFields) == 0
	// 必备要素齐备即可用：待人工补齐的要素不挡写回。
	q.Qualified = len(q.BlockingFields) == 0

	// 精度不足：优先取窗口产物上的精度结论（它带着声明值与上限值），
	// 没有窗口产物时退回记录表载荷上的布尔标记。
	precision := precisionVerdict(in)
	q.PrecisionLimited = precision.Limited
	if precision.Limited {
		q.PrecisionTag = precision.Tag
		if q.PrecisionTag == "" {
			q.PrecisionTag = PrecisionTag
		}
		q.PrecisionDetail = precision.Detail
	}

	// 降级：结论来自规则基线时标出来，并记下降级原因。
	if in.Attribution != nil && in.Attribution.Degraded {
		q.Degraded = true
		q.DegradeReason = in.Attribution.DegradeReason
	}
	if q.Degraded && q.DegradeReason == "" && in.Record != nil {
		q.DegradeReason = in.Record.Quality.DegradeReason
	}

	return q
}

// elementPresence 判断每个要素在不在。
//
// 判据只有两类：**取值非空**（字符串、时间）与**来源产物在不在**（数值型字段）。
// 数值型字段不走「非零」判据，因为 0 本身是合法读数。
func elementPresence(in Inputs) map[string]bool {
	present := map[string]bool{}

	rec := in.Record
	if rec == nil {
		return present
	}

	present[domain.DraftElementPointID] = strings.TrimSpace(rec.PointID) != ""
	present[domain.DraftElementDeviceName] = strings.TrimSpace(rec.DeviceName) != ""
	present[domain.DraftElementExcursionStartedAt] = !rec.ExcursionStartedAt.IsZero()
	present[domain.DraftElementExcursionEndedAt] = !rec.ExcursionEndedAt.IsZero()
	present[domain.DraftElementZone] = strings.TrimSpace(rec.Zone) != ""
	present[domain.DraftElementGeneratedAt] = !rec.GeneratedAt.IsZero()

	// 峰值温度与持续分钟：有没有取决于窗口产物在不在（0℃、0 分钟都是合法读数）。
	present[domain.DraftElementPeakTemperatureC] = in.Window != nil
	present[domain.DraftElementDurationMinutes] = in.Window != nil

	// 原因结论：归因产物在、且原因码非空。
	// 原因码可以是 unknown——证据不足时如实给 unknown 是有结论的结论，不是缺项。
	present[domain.DraftElementCauseConclusion] = in.Attribution != nil &&
		strings.TrimSpace(firstNonEmpty(in.Attribution.CauseCode, in.Attribution.CauseLabel)) != ""

	// 涉及批次：批次清单拿不到时这一栏是缺的（批次号只能由冷链台账给）。
	present[domain.DraftElementBatches] = len(rec.Batches) > 0

	// 处置建议：看的是**主管确认后的最终处置**，不是系统给出的建议。
	// 建议再完整也不等于处置定了；处置没定，这一栏就是缺的。
	present[domain.DraftElementDisposition] = strings.TrimSpace(rec.ConfirmedDisposition) != ""

	return present
}

// precisionVerdict 取出本次成稿的精度结论。
//
// 优先用窗口产物上的精度判定：它带着「声明精度」与「采用上限」两个数值，
// 稿子上要写的就是这两个数。窗口产物不在时退回记录表载荷上的布尔标记
// （此时拿不到具体数值，就只写「精度不足，仅供参考」这句，不编数值）。
func precisionVerdict(in Inputs) domain.PrecisionVerdict {
	if in.Window != nil && in.Window.Precision.Limited {
		verdict := in.Window.Precision
		if verdict.Tag == "" {
			verdict.Tag = PrecisionTag
		}
		if strings.TrimSpace(verdict.Detail) == "" {
			verdict.Detail = precisionDetailText(verdict)
		}
		return verdict
	}
	if in.Record != nil && in.Record.PrecisionLimited {
		verdict := domain.PrecisionVerdict{Limited: true, Tag: PrecisionTag}
		if in.Record.Quality != nil {
			if in.Record.Quality.PrecisionTag != "" {
				verdict.Tag = in.Record.Quality.PrecisionTag
			}
			verdict.Detail = in.Record.Quality.PrecisionDetail
		}
		return verdict
	}
	return domain.PrecisionVerdict{}
}

// precisionDetailText 在判定没有给出说明文字时，自己把两个数值拼出来。
//
// 只在两个数都拿得到时拼：写「超出上限」却不写超了多少，
// 等于让人自己猜差多少。
func precisionDetailText(verdict domain.PrecisionVerdict) string {
	if verdict.DeclaredC == nil {
		return ""
	}
	declared := formatNumber(*verdict.DeclaredC)
	if verdict.LimitC <= 0 {
		return fmt.Sprintf("设备声明测温精度 %s ℃，超出本次采用的精度上限", declared)
	}
	detail := fmt.Sprintf("设备声明测温精度 %s ℃，超出本次采用的精度上限 %s ℃",
		declared, formatNumber(verdict.LimitC))
	if source := precisionLimitSourceLabel(verdict.LimitSource); source != "" {
		detail += "（上限来源：" + source + "）"
	}
	return detail
}

// precisionLimitSourceLabel 把精度上限来源代码译成现场说法。
func precisionLimitSourceLabel(source string) string {
	switch source {
	case domain.PrecisionLimitDeviceClass:
		return "该类设备规范口径"
	case domain.PrecisionLimitPointProfile:
		return "点位温区配置"
	default:
		return source
	}
}

// firstNonEmpty 取第一个非空字符串。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// WritebackSource 是台账写回载荷的来源字段。
//
// 字段名与 spec 的出站字段规范一一对应（external_ref / point_id / device_name /
// excursion_started_at / excursion_ended_at / peak_temperature_c / duration_minutes /
// zone / batches[] / cause_conclusion / evidence[] / disposition / exception_record），
// 外加确认人与确认时刻（confirmed_by / confirmed_at）与幂等键（idempotency_key）。
//
// 前半段由成稿产物给出（机器算出来的部分），后半段由确认动作给出
// （人的部分与这次判定的外部标识）——两者在确认那一刻合成一份完整的载荷。
type WritebackSource struct {
	ExternalRef        string                       `json:"external_ref"`
	PointID            string                       `json:"point_id"`
	DeviceName         string                       `json:"device_name"`
	ExcursionStartedAt time.Time                    `json:"excursion_started_at"`
	ExcursionEndedAt   time.Time                    `json:"excursion_ended_at"`
	PeakTemperatureC   float64                      `json:"peak_temperature_c"`
	DurationMinutes    int                          `json:"duration_minutes"`
	Zone               string                       `json:"zone"`
	Batches            []domain.Batch               `json:"batches"`
	CauseConclusion    string                       `json:"cause_conclusion"`
	Evidence           []domain.AttributionEvidence `json:"evidence"`
	Disposition        string                       `json:"disposition"`
	ExceptionRecord    string                       `json:"exception_record"`
	// ConfirmedBy 是确认人（药学部主管标识）；台账按它核对「这条谁签的」。
	ConfirmedBy string `json:"confirmed_by,omitempty"`
	// ConfirmedAt 是确认时刻。
	ConfirmedAt time.Time `json:"confirmed_at,omitempty"`
	// IdempotencyKey 是这条越温的幂等键；台账侧按它识别重复入账。
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// BuildWritebackSource 从成稿产物组装台账写回载荷的来源字段。
//
// 必备要素缺项时返回 E_VALIDATION 并点名缺项——这份稿子不得进入写回载荷。
// 待人工补齐的要素不挡：批次清单拿不到时台账如实收下一条没有 batches 的
// 越温记录，比这条记录压根不落库要好。
//
// 外部标识（external_ref / idempotency_key）与确认信息（confirmed_by / confirmed_at）
// 不在这里：前者由 run 给出（可重放、稳定），后者由确认动作给出。
func BuildWritebackSource(record *domain.ExceptionRecordPayload) (WritebackSource, error) {
	if record == nil {
		return WritebackSource{}, errcode.Newf(errcode.Validation, "缺少记录表载荷，无法组装台账写回载荷")
	}

	quality := record.Quality
	if quality == nil {
		quality = &domain.DraftQuality{Complete: true, Qualified: true}
	}
	if !quality.Exportable() {
		return WritebackSource{}, errcode.Newf(errcode.Validation,
			"成稿要素不全，不得作为台账写回载荷：缺少 %s",
			strings.Join(quality.BlockingFields, "、"))
	}

	source := WritebackSource{
		PointID:            record.PointID,
		DeviceName:         record.DeviceName,
		ExcursionStartedAt: record.ExcursionStartedAt,
		ExcursionEndedAt:   record.ExcursionEndedAt,
		PeakTemperatureC:   record.PeakTemperatureC,
		DurationMinutes:    record.DurationMinutes,
		Zone:               record.Zone,
		Batches:            record.Batches,
		CauseConclusion:    record.CauseConclusion,
		Evidence:           record.Evidence,
		ExceptionRecord:    record.DraftText,
	}
	// 处置优先取主管确认后的最终结论；还没确认时退回系统给出的建议
	// （默认 pending_manufacturer_evaluation，如实写明「还没定」）。
	source.Disposition = firstNonEmpty(record.ConfirmedDisposition, record.Disposition)
	return source, nil
}

// ConfirmFacts 是确认动作带来的事实：谁确认的、什么时候、处置定了什么、
// 这条判定在台账侧的外部标识与幂等键。
//
// 它与成稿产物分开放：成稿是机器算的，确认是人定的，两者的责任人不一样。
type ConfirmFacts struct {
	// Disposition 是主管确认后的最终处置结论。
	Disposition string
	// ConfirmedBy 是确认人。
	ConfirmedBy string
	// ConfirmedAt 是确认时刻。
	ConfirmedAt time.Time
	// Opinion 是主管的意见（可选）；写进记录表正文末尾的人工确认小节。
	Opinion string
	// ExternalRef 是这条判定的外部引用（由 run 给出，可重放、稳定）。
	ExternalRef string
	// IdempotencyKey 是这条判定的幂等键（由 run 给出）。
	IdempotencyKey string
}

// BuildConfirmedWriteback 从成稿产物与确认事实组装完整的台账写回载荷。
//
// 它是「确认时固化载荷内容」那条规则的落点：主管确认的那一刻，
// 台账要写什么就此定下，之后写回侧原样发出，不在写回时重新算一遍。
//
// 处置以确认时的结论为准：成稿里那个 pending_manufacturer_evaluation 是
// 「还没定」，确认动作把它定下来；确认时说「隔离」，台账里就该写「隔离」。
func BuildConfirmedWriteback(record *domain.ExceptionRecordPayload, facts ConfirmFacts) (WritebackSource, error) {
	source, err := BuildWritebackSource(record)
	if err != nil {
		return WritebackSource{}, err
	}
	if trimmed := strings.TrimSpace(facts.Disposition); trimmed != "" {
		source.Disposition = trimmed
	}
	source.ConfirmedBy = strings.TrimSpace(facts.ConfirmedBy)
	source.ConfirmedAt = facts.ConfirmedAt
	source.ExternalRef = strings.TrimSpace(facts.ExternalRef)
	source.IdempotencyKey = strings.TrimSpace(facts.IdempotencyKey)
	return source, nil
}
