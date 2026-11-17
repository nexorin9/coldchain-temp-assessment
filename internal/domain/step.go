package domain

import (
	"encoding/json"
	"reflect"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// 判定步骤（step）的执行状态。
const (
	StepStateRunning   = "running"
	StepStateSucceeded = "succeeded"
	StepStateFailed    = "failed"
	StepStateRetrying  = "retrying"
)

// StepPayloadKind 是步骤产物的分码。
//
// 同一条判定链上，不同步骤的产物形状完全不同：归因结论、成稿记录表、
// 模型原始输出各有一套字段与取值域。分码的意义是「每类产物各自独立、单独替换」：
// 换掉归因结论的写法不会牵动成稿记录表，反之亦然；下游按分码取回产物，
// 不会把一份归因结论当成成稿记录表去解析。
//
// 分码本身也进了载荷信封：取回产物时先核对分码，对不上直接拒绝，
// 而不是「尽力解析一下」——静默串型比报错难查得多。
type StepPayloadKind string

// 步骤产物分码取值。
const (
	// PayloadAttribution：归因结论产物（原因候选集 + 证据 + 置信度 + 是否降级）。
	PayloadAttribution StepPayloadKind = "attribution"
	// PayloadExceptionRecord：成稿产物（《温度异常情况记录表》的结构化内容）。
	PayloadExceptionRecord StepPayloadKind = "exception_record"
	// PayloadModelRawOutput：模型原始输出产物（模型返回原文与调用元数据）。
	PayloadModelRawOutput StepPayloadKind = "model_raw_output"
	// PayloadSeries：取数步骤的产物（这次判定实际依据的那批温度采样）。
	//
	// 它独立分码的意义：结论凭什么算出来的，必须能原样取回。
	// 采样与结论混在一份产物里，复核时就分不清「用了哪几条读数」。
	PayloadSeries StepPayloadKind = "series"
	// PayloadWindow：越限窗口抽取步骤的产物（窗口本身 + 所用温区及其来源）。
	//
	// 温区来源必须跟着窗口一起走：温区猜错是最致命的一类错误，
	// 而「这个结论用的是点位配置还是制品说明书条目」是复核的第一个问题。
	PayloadWindow StepPayloadKind = "window"
	// PayloadConfirmationReady：待确认步骤的产物（判定已就绪、停在主管确认前）。
	//
	// 它记着这条判定还差哪些要素才够格写台账（批次、处置），
	// 让「结论已出但还不能写回」这件事有据可查，而不是靠人记得。
	PayloadConfirmationReady StepPayloadKind = "confirmation_ready"
)

// payloadKindOrder 是分码的固定顺序，保证摘要与审计输出是确定的。
//
// 顺序按判定步骤链的先后排：取数 → 抽窗口 → 归因 → 成稿 → 待确认，
// 模型原始输出排在最后（它随模型调用产生，挂在归因那一步上）。
var payloadKindOrder = []StepPayloadKind{
	PayloadSeries,
	PayloadWindow,
	PayloadAttribution,
	PayloadExceptionRecord,
	PayloadConfirmationReady,
	PayloadModelRawOutput,
}

// payloadKinds 是分码到载荷类型的映射：每个分码一个独立类型。
//
// 分码与类型是一一对应的：每类产物各自独立编码、各自可替换，
// 换掉其中一个不影响其它分码的类型与解析路径。
var payloadKinds = map[StepPayloadKind]func() any{
	PayloadAttribution:       func() any { return &AttributionPayload{} },
	PayloadExceptionRecord:   func() any { return &ExceptionRecordPayload{} },
	PayloadModelRawOutput:    func() any { return &ModelRawOutputPayload{} },
	PayloadSeries:            func() any { return &SeriesPayload{} },
	PayloadWindow:            func() any { return &WindowPayload{} },
	PayloadConfirmationReady: func() any { return &ConfirmationReadyPayload{} },
}

// PayloadKinds 返回全部分码，按固定顺序。
func PayloadKinds() []StepPayloadKind {
	out := make([]StepPayloadKind, len(payloadKindOrder))
	copy(out, payloadKindOrder)
	return out
}

// AttributionEvidence 是一条可回溯的证据。
//
// 证据必须带数值与阈值：只有「是开门导致的」而没有「门开累计 47 分钟、
// 温升起点在开门后 2 分钟」，复核的人无从判断这条结论站不站得住。
type AttributionEvidence struct {
	Feature   string  `json:"feature"`
	Value     float64 `json:"value"`
	Threshold float64 `json:"threshold"`
	Note      string  `json:"note,omitempty"`
}

// AttributionCandidate 是一个归因候选原因。
//
// 每个候选单独成条、自带证据与置信度，互不吞并：
// 复核的人看到的是「有哪几个可能、各自凭什么、多可信」，
// 而不是一段把几个原因揉在一起的话。
type AttributionCandidate struct {
	CauseCode string `json:"cause_code"`
	// Label 是原因的中文短标签（断电 / 开门 / 探头卡死…），成稿与台账直接引用。
	Label      string                `json:"label,omitempty"`
	Confidence float64               `json:"confidence"`
	Evidence   []AttributionEvidence `json:"evidence,omitempty"`
	// Note 说明这条候选是怎么来的、还差什么才能坐实。
	Note string `json:"note,omitempty"`
	// Tag 是这条候选上的显眼标注（如「精度不足，仅供参考」）。
	// 它必须跟着每一条候选走：只标在总头上，复核的人逐条看候选时会以为都没问题。
	Tag string `json:"tag,omitempty"`
}

// AttributionPayload 是归因结论步骤的产物。
//
// Degraded 表示这次结论不是模型给的、而是模型不可达时退回的规则基线；
// DegradeReason 记下退化原因的错误码。复核的人看到 degraded 就知道该重点核对什么。
type AttributionPayload struct {
	CauseCode string `json:"cause_code"`
	// CauseLabel 是结论原因的中文短标签。
	CauseLabel    string                 `json:"cause_label,omitempty"`
	Confidence    float64                `json:"confidence"`
	Candidates    []AttributionCandidate `json:"candidates,omitempty"`
	Evidence      []AttributionEvidence  `json:"evidence,omitempty"`
	Degraded      bool                   `json:"degraded,omitempty"`
	DegradeReason string                 `json:"degrade_reason,omitempty"`
	// PrecisionLimited 表示设备测温精度声明超出规范上限，结论仅供参考。
	// 它要一路带到成稿：读数本身不可信时，原因结论也得显眼地标出来。
	PrecisionLimited bool `json:"precision_limited,omitempty"`
	// PrecisionTag 是精度不足时的标注文案：「精度不足，仅供参考」。
	PrecisionTag string `json:"precision_tag,omitempty"`
	// MissingFeatures 是算不出来的特征名（缺测就是缺测，不用 0 顶替）。
	MissingFeatures []string `json:"missing_features,omitempty"`
	// MissingClues 是「还差哪条线索才能把原因判实」。
	// 原因判不出来时，它给出的是下一步该去查什么，而不是一句「无法判断」。
	MissingClues []string `json:"missing_clues,omitempty"`
	// Notes 是逐条口径说明，随结论一起给复核的人看。
	Notes []string `json:"notes,omitempty"`
}

// ExceptionRecordPayload 是成稿步骤的产物：一份《疫苗储存和运输温度异常情况记录表》
// 的结构化内容。
//
// 字段与 spec 的写回字段规范对齐：点位与设备、越限起止、峰值与持续时长、所属温区、
// 涉及批次、原因结论与证据、处置建议、数据来源与生成时间。
//
// NeedsReview 与 MissingFields 是成稿自检的结果：要素不全的稿子带标记，
// 不许直接拿去写台账。
type ExceptionRecordPayload struct {
	PointID            string    `json:"point_id"`
	DeviceName         string    `json:"device_name"`
	ExcursionStartedAt time.Time `json:"excursion_started_at"`
	ExcursionEndedAt   time.Time `json:"excursion_ended_at"`
	PeakTemperatureC   float64   `json:"peak_temperature_c"`
	DurationMinutes    int       `json:"duration_minutes"`
	Zone               string    `json:"zone"`
	Batches            []Batch   `json:"batches,omitempty"`
	// BatchLookup 记着这次在库批次查询的结果。
	//
	// 它回答的是「批次清单这一栏到底怎么回事」：查到了（status=ok）、
	// 确实没有在库批次（status=empty）、台账没查成（status=unavailable）、
	// 还是批次归属有歧义（status=ambiguous）。四种情形在成稿里要写的话不一样，
	// 混成一句「待补」的话，复核的人就分不清「这批货真不在库里」与「系统没查到」。
	BatchLookup     *BatchLookup          `json:"batch_lookup,omitempty"`
	CauseConclusion string                `json:"cause_conclusion,omitempty"`
	Evidence        []AttributionEvidence `json:"evidence,omitempty"`
	Disposition     string                `json:"disposition,omitempty"`
	DispositionNote string                `json:"disposition_note,omitempty"`
	// Advice 是批次可用性建议：逐批次的建议动作与依据。
	//
	// 它与 Disposition 分开：Disposition 是整次越温的一句话建议，
	// Advice 是逐批次展开的建议集——一批货受影响不代表另一批也受影响，
	// 按点位一刀切会让没受影响的货陪绑。
	Advice           *DispositionAdvice `json:"advice,omitempty"`
	NeedsReview      bool               `json:"needs_review,omitempty"`
	MissingFields    []string           `json:"missing_fields,omitempty"`
	PrecisionLimited bool               `json:"precision_limited,omitempty"`
	Degraded         bool               `json:"degraded,omitempty"`
	// ConfirmedDisposition 是药学部主管确认后的最终处置结论。
	//
	// 它与 Disposition 分开：Disposition 是系统给出的**建议**
	// （默认 pending_manufacturer_evaluation），ConfirmedDisposition 是**人给的结论**。
	// 成稿要素校验看的是这一栏——建议再完整也不等于处置已经定了，
	// 处置定了才有资格进台账写回载荷。
	ConfirmedDisposition string `json:"confirmed_disposition,omitempty"`
	// Quality 是成稿要素完整性校验的结论，随成稿产物一起落库。
	//
	// 它回答「这份稿子齐不齐、差哪几项、缺的这几项挡不挡写回」。
	// 确认动作发生在判定之后、甚至跨进程重启之后，答案必须还在库里。
	Quality *DraftQuality `json:"quality,omitempty"`
	// DraftText 是成稿步骤渲染出来的《温度异常情况记录表》正文。
	//
	// 它与结构化字段存在同一份产物里：结构化字段给机器读（写台账、
	// 做报表、算统计），正文给人读（递药事、递疫苗生产企业）。
	// 两份内容出自同一批数据，不会出现「表上写着 12.4℃、
	// 正文里写着 12℃」这种当场对不上的情况。
	DraftText string `json:"draft_text,omitempty"`
	// Notes 是随稿给出的口径说明（如「归因来自规则基线，需人工重点核对」）。
	//
	// 它与 MissingFields 分开：MissingFields 说的是「这张表还差哪些要素没填」，
	// Notes 说的是「已经填了的那些要素，成色如何、该怎么用」。
	Notes       []string  `json:"notes,omitempty"`
	GeneratedAt time.Time `json:"generated_at"`
}

// ModelRawOutputPayload 是模型原始输出步骤的产物。
//
// 保留模型返回原文，便于事后核对「模型到底说了什么」；调用元数据（模型名、
// token 数、耗时）随产物一起落库，成本审计与降级判定都要用它。
// 这里不存凭据与鉴权头。
type ModelRawOutputPayload struct {
	Model        string `json:"model"`
	RawResponse  string `json:"raw_response"`
	InputTokens  int    `json:"input_tokens,omitempty"`
	OutputTokens int    `json:"output_tokens,omitempty"`
	DurationMS   int64  `json:"duration_ms,omitempty"`
	Degraded     bool   `json:"degraded,omitempty"`
}

// SeriesPayload 是取数步骤的产物：这次判定实际依据的那批温度采样。
//
// 它回答复核人最关心的第一个问题：这条结论是用哪几条读数算出来的。
// From / To 是取数区间，SampleCount 是取到的条数，Samples 是原样的采样。
type SeriesPayload struct {
	PointID     string             `json:"point_id"`
	From        time.Time          `json:"from"`
	To          time.Time          `json:"to"`
	SampleCount int                `json:"sample_count"`
	Samples     []TemperatureEvent `json:"samples,omitempty"`
}

// WindowPayload 是越限窗口抽取步骤的产物。
//
// ZoneSource 记着温区是从哪儿来的（点位温区配置 / 制品说明书条目）：
// 温区猜错是最致命的一类错误，而「这份结论用的是哪份温区」是复核必问的。
//
// ZoneBasis 补上出处（依据哪份规范或哪条配置），Products 是多制品点位
// 逐制品的温区结论与各自的在库批次，Precision 是精度联动结论，
// Binding 是权威实体绑定链结论（点位 → 设备 → 批次核验到哪一步）。
// 这几样都不参与窗口计算，但它们决定这份窗口能不能拿去写台账。
type WindowPayload struct {
	Window     ExcursionWindow       `json:"window"`
	ZoneMinC   float64               `json:"zone_min_c"`
	ZoneMaxC   float64               `json:"zone_max_c"`
	ZoneSource string                `json:"zone_source"`
	ZoneBasis  string                `json:"zone_basis,omitempty"`
	Products   []ZoneProductDecision `json:"products,omitempty"`
	Precision  PrecisionVerdict      `json:"precision"`
	Binding    BindingVerdict        `json:"binding"`
}

// ConfirmationReadyPayload 是待确认步骤的产物：判定已就绪，停在主管确认之前。
//
// OutstandingFields 记着这条判定还差哪些要素才够格写台账。
// 它让「结论已出、但还不能写回」有据可查，而不是靠人记得。
type ConfirmationReadyPayload struct {
	RunID             string    `json:"run_id"`
	PointID           string    `json:"point_id"`
	ExternalRef       string    `json:"external_ref,omitempty"`
	IdempotencyKey    string    `json:"idempotency_key,omitempty"`
	AwaitingRole      string    `json:"awaiting_role"`
	OutstandingFields []string  `json:"outstanding_fields,omitempty"`
	ReadyAt           time.Time `json:"ready_at"`
}

// payloadEnvelope 是产物在库里的信封：分码 + 内容。
//
// 信封里带上分码，是为了让「取回时核对形状」成为可能：
// 只存内容的话，一份归因结论被当成成稿记录表解析时，JSON 往往能解析成功，
// 串型就变成了静默的数据错乱。
type payloadEnvelope struct {
	Kind    StepPayloadKind `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// Step 是判定 run 里的一个步骤。
//
// 步骤产物（Payload）按分码独立保存：归因结论、成稿记录表、模型原始输出
// 各用自己的载荷类型与分码，替换其中一种不影响其它步骤。
//
// 同一个类型同时是「步骤在内存中的样子」与「步骤在库里的一行」：
// RunID 与 Seq 让一行记录能独立回答「这是哪条 run 的第几步」，
// ErrorCode 让失败原因能按错误码被检索（E_MODEL_TIMEOUT 与 E_VALIDATION
// 在排障时是两回事，光看一句中文描述分不出来）。
type Step struct {
	// RunID 是这条步骤所属的判定 run。
	RunID string `json:"run_id,omitempty"`
	// StepName 是步骤名（取数 / 抽窗口 / 归因 / 成稿 / 待确认）。
	StepName string `json:"step_name"`
	// Seq 是步骤在链上的次序，从 1 起；时间线按它排序。
	Seq     int    `json:"seq,omitempty"`
	Attempt int    `json:"attempt"`
	State   string `json:"state"`
	// PayloadKind 是产物的分码，写入时同步落；读取时与信封内的分码必须一致。
	PayloadKind StepPayloadKind `json:"payload_kind,omitempty"`
	// Payload 是产物信封的 JSON（含分码与内容）。
	Payload   json.RawMessage `json:"payload,omitempty"`
	StartedAt time.Time       `json:"started_at"`
	EndedAt   *time.Time      `json:"ended_at,omitempty"`
	Error     string          `json:"error,omitempty"`
	// ErrorCode 是失败步骤的错误码；成功步骤为空。
	ErrorCode string `json:"error_code,omitempty"`
	// BackoffMS 是这次尝试失败后等待的退避毫秒数；成功或不再重试时为 0。
	//
	// 它记在失败的那一次尝试上：时间线上要看得见「这一步失败后等了多久才再试」。
	// 只留最终结果就看不出中间那段等待——而那段等待恰恰是复盘时要问的
	// 「它到底卡了多久」。
	BackoffMS int64 `json:"backoff_ms,omitempty"`
}

// NewStep 建一个待执行的步骤。
func NewStep(stepName string, startedAt time.Time) Step {
	return Step{
		StepName:  stepName,
		Attempt:   1,
		State:     StepStateRunning,
		StartedAt: startedAt,
	}
}

// EncodePayload 按分码把产物编码成信封 JSON。
//
// 分码与载荷类型必须对得上：拿归因结论的载荷往成稿分码上塞会被拒绝，
// 宁可报错也不要写出一份形状对不上的产物。
func EncodePayload(kind StepPayloadKind, payload any) (json.RawMessage, error) {
	ctor, ok := payloadKinds[kind]
	if !ok {
		return nil, errcode.Newf(errcode.Validation, "未登记的步骤产物分码：%q", kind)
	}
	if payload == nil {
		return nil, errcode.Newf(errcode.Validation, "步骤产物分码 %q 的载荷为空", kind)
	}
	want := reflect.TypeOf(ctor())
	got := reflect.TypeOf(payload)
	if got != want {
		return nil, errcode.Newf(errcode.Validation,
			"步骤产物分码 %q 的载荷类型不匹配：期望 %s，收到 %s", kind, want, got)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, errcode.Newf(errcode.Validation, "步骤产物序列化失败（分码 %q）：%v", kind, err)
	}
	envelope, err := json.Marshal(payloadEnvelope{Kind: kind, Payload: body})
	if err != nil {
		return nil, errcode.Newf(errcode.Validation, "步骤产物信封序列化失败（分码 %q）：%v", kind, err)
	}
	return json.RawMessage(envelope), nil
}

// DecodePayload 按分码把信封 JSON 还原成对应类型的产物。
//
// 分码对不上（信封里的 kind 与请求的分码不同）或分码未登记，一律返回
// E_VALIDATION：宁可拒绝，也不跨类型猜测。
func DecodePayload(kind StepPayloadKind, raw json.RawMessage) (any, error) {
	ctor, ok := payloadKinds[kind]
	if !ok {
		return nil, errcode.Newf(errcode.Validation, "未登记的步骤产物分码：%q", kind)
	}
	if len(raw) == 0 {
		return nil, errcode.Newf(errcode.Validation, "步骤产物分码 %q 没有载荷内容", kind)
	}
	var envelope payloadEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, errcode.Newf(errcode.Validation, "步骤产物信封解析失败（分码 %q）：%v", kind, err)
	}
	if envelope.Kind != kind {
		return nil, errcode.Newf(errcode.Validation,
			"步骤产物分码不匹配：请求 %q，载荷实际为 %q", kind, envelope.Kind)
	}
	if len(envelope.Payload) == 0 {
		return nil, errcode.Newf(errcode.Validation, "步骤产物信封内容为空（分码 %q）", kind)
	}
	target := ctor()
	if err := json.Unmarshal(envelope.Payload, target); err != nil {
		return nil, errcode.Newf(errcode.Validation, "步骤产物解析失败（分码 %q）：%v", kind, err)
	}
	return target, nil
}

// DecodePayloadAs 取回某类产物并断言成指定类型。
//
// 分码不符或类型不符都返回 E_VALIDATION。
func DecodePayloadAs[T any](kind StepPayloadKind, raw json.RawMessage) (*T, error) {
	value, err := DecodePayload(kind, raw)
	if err != nil {
		return nil, err
	}
	typed, ok := value.(*T)
	if !ok {
		return nil, errcode.Newf(errcode.Validation,
			"步骤产物分码 %q 的类型不匹配：期望 %s", kind, reflect.TypeOf(value))
	}
	return typed, nil
}

// SetPayload 按分码写入本步骤的产物：编码信封并同步记下分码。
func (s *Step) SetPayload(kind StepPayloadKind, payload any) error {
	encoded, err := EncodePayload(kind, payload)
	if err != nil {
		return err
	}
	s.PayloadKind = kind
	s.Payload = encoded
	return nil
}

// PayloadAs 按分码取回本步骤的产物，还原成对应类型的产物值。
//
// 分码与本步骤记录的分码不一致、或与信封内分码不一致，都返回 E_VALIDATION。
func (s *Step) PayloadAs(kind StepPayloadKind) (any, error) {
	if s.PayloadKind != kind {
		return nil, errcode.Newf(errcode.Validation,
			"步骤 %s 的产物分码是 %q，取回 %q 不匹配", s.StepName, s.PayloadKind, kind)
	}
	return DecodePayload(kind, s.Payload)
}

// HasPayload 判断本步骤是否已有该类产物。
func (s *Step) HasPayload(kind StepPayloadKind) bool {
	return s.PayloadKind == kind && len(s.Payload) > 0
}
