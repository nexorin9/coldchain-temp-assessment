// Package model 定义冷链判定链上模型调用的结构化输出契约。
//
// 这一层只管「形状」：请求侧给出 JSON Schema，响应侧逐字段强校验。
// 它不管「事实对不对」（数值能不能在输入时序里找到，交给 factcheck），
// 也不管「模型服务通不通」（交给 gateway）。
//
// 分层是为了让失败原因分得清——三种失败在处置上完全不同：
//
//	形状不合规（E_MODEL_SCHEMA_INVALID）→ 重问一次，仍不合规则退回规则基线；
//	数值与事实对不上（E_MODEL_FACT_MISMATCH）→ 说明模型在编，重问并拦截；
//	服务不可达（E_MODEL_TIMEOUT / E_MODEL_UNAVAILABLE）→ 直接降级。
//
// 形状契约取自源产品的同构做法：上游把一份字段繁多的原始快照
// 「投影」成自己真正用到的少数几个字段（tools/model-prices 的 rawEntry →
// slimEntry），未知字段直接忽略而不报错。本包沿用同一取舍：
// 请求侧只把产品真正消费的字段写进 JSON Schema；响应侧遇到模型多说的字段
// 采取「记录、不拦截」——模型爱多说两句是常事，为这个把一份可用结论一起
// 丢掉不划算。
package model

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/chaosforge/coldchain-temp-assessment/internal/disposition"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/rules"
)

// SchemaName 是模型结构化输出的分码。
//
// 五类输出各有各的形状，互不吞并：换掉归因结论的写法不该牵动成稿记录表，
// 反之亦然。所以每类输出各有一份独立的 schema 与独立的校验入口。
type SchemaName string

// 模型结构化输出的分码取值。
const (
	// SchemaAttribution：归因结论（原因代码 + 置信度 + 候选集 + 证据）。
	SchemaAttribution SchemaName = "attribution"
	// SchemaWindow：越限窗口（起止 + 持续分钟 + 峰值）。
	SchemaWindow SchemaName = "window"
	// SchemaBatchAdvice：在库批次的可用性建议（逐批次一条）。
	SchemaBatchAdvice SchemaName = "batch_advice"
	// SchemaExceptionRecord：《温度异常情况记录表》的结构化内容。
	SchemaExceptionRecord SchemaName = "exception_record"
	// SchemaDispositionNote：处置说明（处置结论 + 说明正文）。
	SchemaDispositionNote SchemaName = "disposition_note"
)

// 处置结论取值，与 spec 的状态表一致。
//
// 注意：处置结论只有「建议」这一层含义——批次到底能不能用，
// 由疫苗生产企业评估质量影响、再由药学部主管确认决定。
//
// 取值域的真源在 internal/disposition：模型层、成稿层与确认层都要用它，
// 放在这里另抄一份迟早会分叉（模型给了一个成稿不认识的处置，台账就收下了）。
// 这里的常量是别名，保留是为了不改动既有调用方。
const (
	// DispositionIsolate：隔离。
	DispositionIsolate = disposition.Isolate
	// DispositionRelease：放行。
	DispositionRelease = disposition.Release
	// DispositionScrap：报损。
	DispositionScrap = disposition.Scrap
	// DispositionPendingEvaluation：待疫苗生产企业评估（默认取值）。
	DispositionPendingEvaluation = disposition.PendingEvaluation
)

// DispositionValues 返回全部处置结论取值。
func DispositionValues() []string {
	return disposition.Values()
}

// DispositionLabel 取处置结论的中文短标签。
//
// 取值域之外的代码返回 ok=false：调用方应当显式处理「不认识这个处置」，
// 而不是自己编一个标签——处置结论写错一个字，批次就可能被误放行。
func DispositionLabel(code string) (string, bool) {
	return disposition.Label(code)
}

// CauseCodes 返回全部归因原因代码。
//
// 取值直接引自规则基线（internal/rules），不在这里另抄一份：
// 两套原因字典一旦分叉，模型给的结论就没法跟规则降级路径的结论对齐。
func CauseCodes() []string {
	return []string{
		rules.CauseDoorOpen,
		rules.CausePowerLoss,
		rules.CauseDefrost,
		rules.CauseProbeDetached,
		rules.CauseProbeStuck,
		rules.CauseACFailure,
		rules.CauseOverstock,
		rules.CauseUnknown,
	}
}

// fieldKind 是字段的取值类型。
type fieldKind string

// 字段取值类型。
const (
	kindString  fieldKind = "string"
	kindNumber  fieldKind = "number"
	kindInteger fieldKind = "integer"
	kindBoolean fieldKind = "boolean"
	kindArray   fieldKind = "array"
	kindObject  fieldKind = "object"
	// kindTimestamp 在 JSON Schema 侧是 string + format: date-time，
	// 校验侧额外要求能按 RFC3339 解析出来。
	kindTimestamp fieldKind = "timestamp"
)

// jsonType 返回字段在 JSON Schema 里的类型名。
func (k fieldKind) jsonType() string {
	if k == kindTimestamp {
		return "string"
	}
	return string(k)
}

// fieldSpec 描述一个字段的形状约束。
//
// 一份 spec 同时喂两处：请求侧生成 JSON Schema（给模型看），
// 响应侧逐字段强校验（给产品用）。两侧共用一份定义，
// 才不会出现「按 A 生成的 schema」与「按 B 校验的规则」对不上的情况。
type fieldSpec struct {
	name     string
	kind     fieldKind
	required bool
	// material 标记「选填但要素性」的字段：缺了不影响解析通过，
	// 但整份结论要素不全，要标 needs_review。
	//
	// 区分它与普通选填字段（如结论的中文标签、口径说明）是为了不把
	// 「模型多说一句少一句」当成「这份结论不能用」。
	material bool
	// enum 是枚举取值域；非空时取值必须落在其中。
	enum []string
	// min / max 是数值取值域（含端点）。
	min *float64
	max *float64
	// minItems / maxItems 是数组长度取值域。
	minItems int
	maxItems int
	// items 是数组的元素形状。
	items *fieldSpec
	// fields 是对象的字段集。
	fields []fieldSpec
	// format 是补充格式标记（如 date-time）。
	format string
	// desc 是给模型看的字段说明。
	desc string
}

func str(name string, required bool, desc string) fieldSpec {
	return fieldSpec{name: name, kind: kindString, required: required, desc: desc}
}

// matStr 是一个「选填但要素性」的字符串字段：缺了不判不合规，但结论要素不全。
func matStr(name string, desc string) fieldSpec {
	return fieldSpec{name: name, kind: kindString, required: false, material: true, desc: desc}
}

// matArr 是一个「选填但要素性」的数组字段：缺了不判不合规，但结论要素不全。
func matArr(name string, items fieldSpec, desc string) fieldSpec {
	return fieldSpec{name: name, kind: kindArray, required: false, material: true,
		items: &items, desc: desc}
}

func enumStr(name string, required bool, values []string, desc string) fieldSpec {
	return fieldSpec{name: name, kind: kindString, required: required, enum: values, desc: desc}
}

// matEnumStr 是一个「选填但要素性」的枚举字段。
func matEnumStr(name string, values []string, desc string) fieldSpec {
	return fieldSpec{name: name, kind: kindString, required: false, material: true,
		enum: values, desc: desc}
}

func num(name string, required bool, desc string) fieldSpec {
	return fieldSpec{name: name, kind: kindNumber, required: required, desc: desc}
}

func bounded(name string, required bool, low, high float64, desc string) fieldSpec {
	return fieldSpec{name: name, kind: kindNumber, required: required,
		min: &low, max: &high, desc: desc}
}

func integer(name string, required bool, low float64, desc string) fieldSpec {
	return fieldSpec{name: name, kind: kindInteger, required: required, min: &low, desc: desc}
}

func timestamp(name string, required bool, desc string) fieldSpec {
	return fieldSpec{name: name, kind: kindTimestamp, required: required,
		format: "date-time", desc: desc}
}

func arr(name string, required bool, items fieldSpec, desc string) fieldSpec {
	return fieldSpec{name: name, kind: kindArray, required: required,
		items: &items, desc: desc}
}

func obj(name string, required bool, fields []fieldSpec, desc string) fieldSpec {
	return fieldSpec{name: name, kind: kindObject, required: required, fields: fields, desc: desc}
}

// Schema 是一份结构化输出的契约。
type Schema struct {
	// Name 是分码。
	Name SchemaName
	// Title 是中文标题（给模型看的说明用）。
	Title string
	// Description 是这份输出的用途说明。
	Description string
	// Root 是根形状：对象或数组。
	Root fieldSpec
}

// jsonSchema 把字段约束翻译成 JSON Schema。
//
// 对象节点一律带 additionalProperties: false（结构化输出的严格模式要求），
// 但这只约束「模型该输出什么」，不约束「我们怎么收」——
// 响应侧对多出来的字段是记录而不拦截，见包注释。
func (f fieldSpec) jsonSchema() map[string]any {
	out := map[string]any{"type": f.kind.jsonType()}
	if f.format != "" {
		out["format"] = f.format
	}
	if len(f.enum) > 0 {
		out["enum"] = f.enum
	}
	if f.min != nil {
		out["minimum"] = *f.min
	}
	if f.max != nil {
		out["maximum"] = *f.max
	}
	switch f.kind {
	case kindArray:
		if f.items != nil {
			out["items"] = f.items.jsonSchema()
		}
		if f.minItems > 0 {
			out["minItems"] = f.minItems
		}
		if f.maxItems > 0 {
			out["maxItems"] = f.maxItems
		}
	case kindObject:
		props := map[string]any{}
		required := []string{}
		for _, sub := range f.fields {
			props[sub.name] = sub.jsonSchema()
			if sub.required {
				required = append(required, sub.name)
			}
		}
		out["properties"] = props
		if len(required) > 0 {
			out["required"] = required
		}
		out["additionalProperties"] = false
	}
	if f.desc != "" {
		out["description"] = f.desc
	}
	return out
}

// JSONSchema 返回这份契约对应的 JSON Schema 文档。
func (s Schema) JSONSchema() map[string]any {
	return s.Root.jsonSchema()
}

// JSONSchemaJSON 返回 JSON Schema 的 JSON 文本。
func (s Schema) JSONSchemaJSON() ([]byte, error) {
	body, err := json.Marshal(s.JSONSchema())
	if err != nil {
		return nil, errcode.Newf(errcode.Validation, "JSON Schema 序列化失败（分码 %q）：%v", s.Name, err)
	}
	return body, nil
}

// 复用形状定义的辅助构造函数。
var (
	// evidenceObject 是一条证据的形状。
	//
	// 证据必须带特征名、实测值与阈值：只有「是开门导致的」而没有
	// 「门开累计 47 分钟、温升起点在开门后 2 分钟」，复核的人无从判断
	// 这条结论站不站得住。
	evidenceObject = obj("", false, []fieldSpec{
		str("feature", true, "证据对应的特征名（如 device_event:door_open）"),
		num("value", true, "该特征的实测值"),
		num("threshold", true, "该特征用于判定的阈值"),
		str("note", false, "口径说明"),
	}, "一条证据")

	// candidateObject 是一个归因候选的形状。
	candidateObject = obj("", false, []fieldSpec{
		enumStr("cause_code", true, CauseCodes(), "归因原因代码，须落在系统原因字典内"),
		str("label", false, "原因的中文短标签（如「断电」）"),
		bounded("confidence", true, 0, 1, "该候选的置信度，取值 0～1"),
		arr("evidence", false, evidenceObject, "支撑该候选的证据"),
		str("note", false, "这条候选还差什么才能坐实"),
	}, "一个归因候选")

	// batchAdviceObject 是一条批次建议的形状。
	//
	// batch_ref 是台账给的批次号：模型只引用，不新造。
	batchAdviceObject = obj("", false, []fieldSpec{
		str("batch_ref", true, "在库批次号（只由冷链台账给，不得新造）"),
		enumStr("advice", true, DispositionValues(), "对该批次的建议动作"),
		str("rationale", false, "给出该建议的理由"),
		bounded("confidence", false, 0, 1, "该建议的置信度，取值 0～1"),
		str("note", false, "补充说明"),
	}, "一条批次建议")
)

// schemas 是全部结构化输出的契约表。
var schemas = map[SchemaName]Schema{
	SchemaAttribution:     attributionSchema(),
	SchemaWindow:          windowSchema(),
	SchemaBatchAdvice:     batchAdviceSchema(),
	SchemaExceptionRecord: exceptionRecordSchema(),
	SchemaDispositionNote: dispositionNoteSchema(),
}

// attributionSchema 是归因结论的契约。
//
// 必填只有 cause_code 与 confidence 两项：模型只给一个原因、不给候选集
// 也是可用结论（needs_review 会标出来），但连「什么原因、多可信」都没有
// 就不是结论了。
func attributionSchema() Schema {
	return Schema{
		Name:        SchemaAttribution,
		Title:       "越温原因归因结论",
		Description: "对一次越温给出原因判断、置信度、候选原因与支撑证据",
		Root: obj("", true, []fieldSpec{
			enumStr("cause_code", true, CauseCodes(), "最可能的原因代码"),
			str("label", false, "原因的中文短标签"),
			bounded("confidence", true, 0, 1, "结论置信度，取值 0～1"),
			matArr("candidates", candidateObject, "其它候选原因，按证据强度排序"),
			matArr("evidence", evidenceObject, "支撑结论的证据"),
			arr("notes", false, str("", false, "口径说明"), "逐条口径说明"),
		}, "归因结论"),
	}
}

// windowSchema 是越限窗口的契约。
func windowSchema() Schema {
	return Schema{
		Name:        SchemaWindow,
		Title:       "越限窗口",
		Description: "一次越温的起止时刻、持续分钟与峰值温度",
		Root: obj("", true, []fieldSpec{
			timestamp("start", true, "越限开始时刻，RFC3339 带时区偏移"),
			timestamp("end", true, "越限结束时刻，RFC3339 带时区偏移"),
			integer("duration_minutes", true, 0, "越限持续分钟数"),
			num("peak_c", true, "窗口内峰值温度（摄氏度）"),
			arr("notes", false, str("", false, "口径说明"), "逐条口径说明"),
		}, "越限窗口"),
	}
}

// batchAdviceSchema 是在库批次建议的契约（根是数组）。
func batchAdviceSchema() Schema {
	return Schema{
		Name:        SchemaBatchAdvice,
		Title:       "在库批次可用性建议",
		Description: "逐个在库批次给出建议动作与理由",
		Root:        arr("", true, batchAdviceObject, "批次建议列表"),
	}
}

// exceptionRecordSchema 是《温度异常情况记录表》的契约。
//
// 结构性事实（点位、设备、起止、峰值、时长、温区）是必填；
// 判断性内容（原因、证据、处置、批次）是选填——缺了就标 needs_review，
// 由成稿自检去点出「这份稿子还差什么」，不许拿默认值顶替。
func exceptionRecordSchema() Schema {
	return Schema{
		Name:        SchemaExceptionRecord,
		Title:       "疫苗储存和运输温度异常情况记录表",
		Description: "可直接报药事与疫苗生产企业的温度异常情况记录表内容",
		Root: obj("", true, []fieldSpec{
			str("point_id", true, "温控点位标识"),
			str("device_name", true, "记录仪名称"),
			timestamp("excursion_started_at", true, "越限开始时刻，RFC3339 带时区偏移"),
			timestamp("excursion_ended_at", true, "越限结束时刻，RFC3339 带时区偏移"),
			num("peak_temperature_c", true, "越限峰值温度（摄氏度）"),
			integer("duration_minutes", true, 0, "越限持续分钟数"),
			str("zone", true, "所属温区描述（如 2~8℃）"),
			matArr("batches", batchAdviceObject, "涉及的在库批次与建议"),
			matStr("cause_conclusion", "原因结论"),
			matArr("evidence", evidenceObject, "支撑结论的证据"),
			matEnumStr("disposition", DispositionValues(), "建议的处置结论"),
			matStr("disposition_note", "处置说明正文"),
			arr("notes", false, str("", false, "口径说明"), "逐条口径说明"),
		}, "异常情况记录表"),
	}
}

// dispositionNoteSchema 是处置说明的契约。
func dispositionNoteSchema() Schema {
	return Schema{
		Name:        SchemaDispositionNote,
		Title:       "处置说明",
		Description: "给出建议处置结论及其说明正文",
		Root: obj("", true, []fieldSpec{
			enumStr("disposition", true, DispositionValues(), "建议的处置结论"),
			str("note", true, "处置说明正文"),
			matStr("rationale", "给出该结论的理由"),
			bounded("confidence", false, 0, 1, "该结论的置信度，取值 0～1"),
		}, "处置说明"),
	}
}

// Lookup 取一份契约。
func Lookup(name SchemaName) (Schema, bool) {
	s, ok := schemas[name]
	return s, ok
}

// SchemaNames 返回全部分码，按固定顺序。
func SchemaNames() []SchemaName {
	return []SchemaName{
		SchemaAttribution,
		SchemaWindow,
		SchemaBatchAdvice,
		SchemaExceptionRecord,
		SchemaDispositionNote,
	}
}

// Message 是聊天请求里的一条消息。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// RequestOptions 是构造结构化请求的参数。
type RequestOptions struct {
	// Model 是模型名（来自环境变量 MODEL_NAME）。
	Model string
	// SystemPrompt 是系统提示词；为空时不带 system 消息。
	SystemPrompt string
	// UserPayload 是用户消息的载荷（会被序列化成 JSON 文本）。
	UserPayload any
	// Temperature 是采样温度；默认 0（同一输入应得同一结论）。
	Temperature float64
	// MaxTokens 大于 0 时写入 max_tokens。
	MaxTokens int
}

// BuildRequest 按 schema 生成 OpenAI-compatible 的结构化请求体。
//
// 请求体固定带 response_format.json_schema：模型被要求按本包的 schema 输出，
// 返回侧再按同一份 schema 强校验——两侧同源，才谈得上「形状一致」。
func BuildRequest(name SchemaName, opts RequestOptions) ([]byte, error) {
	schema, ok := Lookup(name)
	if !ok {
		return nil, errcode.Newf(errcode.Validation, "未登记的模型结构化输出分码：%q", name)
	}
	if opts.Model == "" {
		return nil, errcode.Newf(errcode.Validation, "模型名不能为空（MODEL_NAME 未配置）")
	}
	messages := make([]Message, 0, 2)
	if opts.SystemPrompt != "" {
		messages = append(messages, Message{Role: "system", Content: opts.SystemPrompt})
	}
	payload, err := json.Marshal(opts.UserPayload)
	if err != nil {
		return nil, errcode.Newf(errcode.Validation,
			"请求载荷序列化失败（分码 %q）：%v", name, err)
	}
	messages = append(messages, Message{Role: "user", Content: string(payload)})

	body := map[string]any{
		"model":       opts.Model,
		"messages":    messages,
		"temperature": opts.Temperature,
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   string(schema.Name),
				"strict": true,
				"schema": schema.JSONSchema(),
			},
		},
	}
	if opts.MaxTokens > 0 {
		body["max_tokens"] = opts.MaxTokens
	}
	out, err := json.Marshal(body)
	if err != nil {
		return nil, errcode.Newf(errcode.Validation,
			"结构化请求序列化失败（分码 %q）：%v", name, err)
	}
	return out, nil
}

// DefaultSystemPrompt 给出该分码的系统提示词。
//
// 它只做两件事：说清这份输出要回答什么问题、要求只输出符合 schema 的 JSON。
// 业务措辞与责任边界由成稿层负责，不在这里堆砌。
func DefaultSystemPrompt(name SchemaName) string {
	schema, ok := Lookup(name)
	if !ok {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "你是医院药学部冷链温控判定系统的协办，只回答 %s（%s）。\n", schema.Title, schema.Description)
	b.WriteString("输出要求：只输出一个 JSON 对象或数组，不要输出解释性文字、不要用 Markdown 代码块包裹。\n")
	b.WriteString("字段严格按给定 JSON Schema：数值不得凭空编造，不确定的地方留空并说明缺什么。\n")
	return b.String()
}
