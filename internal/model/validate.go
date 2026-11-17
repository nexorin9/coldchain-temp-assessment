package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// maxAuditFragment 是保留在错误里的原始响应片段上限（字节）。
//
// 片段只留截断后的前若干字节：够复核「模型到底说了什么」，
// 又不至于把一大段响应塞进日志与错误响应里。
const maxAuditFragment = 512

// ValidationIssue 描述一处字段级不合规。
type ValidationIssue struct {
	// Path 是不合规字段的路径（如 candidates[0].confidence）。
	Path string
	// Kind 是不合规的类型：
	// missing_required / type_mismatch / enum_violation /
	// range_violation / format_violation / not_json。
	Kind string
	// Reason 是中文说明，直接进错误详情。
	Reason string
	// Value 是不合规的原始取值（已截断）。
	Value string
}

// SchemaError 是响应侧强校验失败的结构化错误。
//
// 错误码恒为 E_MODEL_SCHEMA_INVALID：形状不合规与「数值说错了」
// （E_MODEL_FACT_MISMATCH）是两回事，前者是协议问题，后者是模型在编。
//
// RawFragment 保留原始响应片段供审计：模型不遵守 schema 时，
// 最该被看见的是它到底返回了什么，而不是一句「校验失败」。
type SchemaError struct {
	// Base 是带错误码的底层错误。
	Base *errcode.Error
	// Schema 是校验失败的结构化输出分码。
	Schema SchemaName
	// Issue 是字段级问题。
	Issue ValidationIssue
	// RawFragment 是截断后的原始响应片段。
	RawFragment string
}

// Error 实现 error 接口。
func (e *SchemaError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s: %s（%s）", e.Base.Code, e.Base.Message, e.Base.Details)
}

// Unwrap 交出底层错误，errors.As 能取出 *errcode.Error。
func (e *SchemaError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Base
}

// newSchemaError 造一个形状不合规的错误。
func newSchemaError(name SchemaName, issue ValidationIssue, raw []byte) *SchemaError {
	base := errcode.New(errcode.ModelSchemaInvalid, issue.Reason)
	return &SchemaError{
		Base:        base,
		Schema:      name,
		Issue:       issue,
		RawFragment: truncateForAudit(raw),
	}
}

// validationContext 收集一次校验里的软问题。
type validationContext struct {
	// missing 是缺失的字段路径（含必填与选填）。
	missing []string
	// extra 是模型多给、本服务不消费的字段路径。
	extra []string
}

// parsed 是一次响应校验的通用结果。
type parsed struct {
	schema SchemaName
	// needsReview 表示模型只给了部分内容。
	needsReview bool
	// missingFields 是缺失的字段路径。
	missingFields []string
	// extraFields 是模型多给的字段路径。
	extraFields []string
}

// parse 校验一份模型响应。
//
// 硬不合规（必填缺失 / 类型错 / 枚举越界 / 数值越界 / 格式错 / 不是 JSON）
// 一律返回 E_MODEL_SCHEMA_INVALID；
// 选填字段缺失只记 missingFields，由调用方标 needs_review 处理——
// 缺一个候选集不等于这份结论不能用，但也不能当完整结论用。
func parse(name SchemaName, raw []byte) (*parsed, error) {
	schema, ok := Lookup(name)
	if !ok {
		return nil, errcode.Newf(errcode.Validation, "未登记的模型结构化输出分码：%q", name)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, newSchemaError(name, ValidationIssue{
			Kind:   "not_json",
			Reason: "模型响应内容为空",
		}, raw)
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	// 用 Number 保留数字字面量：这样「整数写成 3.5」能被认出来，
	// 而不是先被 float64 悄悄抹平。
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, newSchemaError(name, ValidationIssue{
			Kind:   "not_json",
			Reason: fmt.Sprintf("模型响应不是合法 JSON：%v", err),
			Value:  truncateForAudit(trimmed),
		}, raw)
	}
	if dec.More() {
		return nil, newSchemaError(name, ValidationIssue{
			Kind:   "not_json",
			Reason: "模型响应在首个 JSON 之后还有多余内容",
			Value:  truncateForAudit(trimmed),
		}, raw)
	}
	ctx := &validationContext{}
	if issue := validateValue(schema.Root, doc, "", ctx); issue != nil {
		return nil, newSchemaError(name, *issue, raw)
	}
	return &parsed{
		schema:        name,
		needsReview:   len(ctx.missing) > 0,
		missingFields: ctx.missing,
		extraFields:   ctx.extra,
	}, nil
}

// validateValue 校验一个值是否符合 spec。
//
// 返回第一处硬不合规；软问题（选填缺失、多余字段）记进 ctx 后继续。
func validateValue(spec fieldSpec, value any, path string, ctx *validationContext) *ValidationIssue {
	switch spec.kind {
	case kindString, kindTimestamp:
		s, ok := value.(string)
		if !ok {
			return typeIssue(path, "string", value)
		}
		if spec.kind == kindTimestamp {
			if _, err := time.Parse(time.RFC3339, s); err != nil {
				return &ValidationIssue{
					Path:   path,
					Kind:   "format_violation",
					Reason: fmt.Sprintf("字段 %s 格式不符：%q（须为 RFC3339 带时区偏移的时间戳）", pathLabel(path), truncateString(s)),
					Value:  truncateString(s),
				}
			}
		}
		if len(spec.enum) > 0 && !contains(spec.enum, s) {
			return &ValidationIssue{
				Path: path,
				Kind: "enum_violation",
				Reason: fmt.Sprintf("字段 %s 取值不在允许集合内：%q（允许：%s）",
					pathLabel(path), truncateString(s), strings.Join(spec.enum, "、")),
				Value: truncateString(s),
			}
		}
	case kindNumber:
		n, ok := value.(json.Number)
		if !ok {
			return typeIssue(path, "number", value)
		}
		f, err := n.Float64()
		if err != nil {
			return typeIssue(path, "number", value)
		}
		if spec.min != nil && f < *spec.min {
			return rangeIssue(path, n.String(), *spec.min, *spec.max)
		}
		if spec.max != nil && f > *spec.max {
			return rangeIssue(path, n.String(), *spec.min, *spec.max)
		}
	case kindInteger:
		n, ok := value.(json.Number)
		if !ok {
			return typeIssue(path, "integer", value)
		}
		i, err := n.Int64()
		if err != nil {
			return &ValidationIssue{
				Path:   path,
				Kind:   "type_mismatch",
				Reason: fmt.Sprintf("字段 %s 类型不符：期望 integer，实际 %s", pathLabel(path), n.String()),
				Value:  n.String(),
			}
		}
		if spec.min != nil && float64(i) < *spec.min {
			return rangeIssue(path, n.String(), *spec.min, *spec.max)
		}
	case kindBoolean:
		if _, ok := value.(bool); !ok {
			return typeIssue(path, "boolean", value)
		}
	case kindArray:
		items, ok := value.([]any)
		if !ok {
			return typeIssue(path, "array", value)
		}
		if spec.minItems > 0 && len(items) < spec.minItems {
			return &ValidationIssue{
				Path:   path,
				Kind:   "range_violation",
				Reason: fmt.Sprintf("字段 %s 条数不足：%d（下限 %d）", pathLabel(path), len(items), spec.minItems),
				Value:  strconv.Itoa(len(items)),
			}
		}
		if spec.maxItems > 0 && len(items) > spec.maxItems {
			return &ValidationIssue{
				Path:   path,
				Kind:   "range_violation",
				Reason: fmt.Sprintf("字段 %s 条数超限：%d（上限 %d）", pathLabel(path), len(items), spec.maxItems),
				Value:  strconv.Itoa(len(items)),
			}
		}
		if spec.items == nil {
			return nil
		}
		for i, item := range items {
			if issue := validateValue(*spec.items, item, indexPath(path, i), ctx); issue != nil {
				return issue
			}
		}
	case kindObject:
		obj, ok := value.(map[string]any)
		if !ok {
			return typeIssue(path, "object", value)
		}
		known := make(map[string]bool, len(spec.fields))
		for _, sub := range spec.fields {
			known[sub.name] = true
			v, present := obj[sub.name]
			// null 按缺失处理：模型用 null 表达「给不出」，
			// 那它给的就是没给，不该被当成一个真值。
			if !present || v == nil {
				subPath := joinPath(path, sub.name)
				if sub.required {
					return &ValidationIssue{
						Path:   subPath,
						Kind:   "missing_required",
						Reason: fmt.Sprintf("字段 %s 缺失（必填）", pathLabel(subPath)),
					}
				}
				// 要素缺失只按根层判定：根层缺的是整份结论的要件，
				// 候选里少一条证据是候选自己的成色，不该把整份结论打成待复核。
				if sub.material && path == "" {
					ctx.missing = append(ctx.missing, subPath)
				}
				continue
			}
			if issue := validateValue(sub, v, joinPath(path, sub.name), ctx); issue != nil {
				return issue
			}
		}
		// 多出来的字段只记录不拦截：模型多说两句是常事，
		// 为这个把一份可用结论一起丢掉不划算。记录下来供审计核对。
		for k := range obj {
			if !known[k] {
				ctx.extra = append(ctx.extra, joinPath(path, k))
			}
		}
	}
	return nil
}

// typeIssue 造一处类型不符的问题。
func typeIssue(path, expected string, value any) *ValidationIssue {
	return &ValidationIssue{
		Path:   path,
		Kind:   "type_mismatch",
		Reason: fmt.Sprintf("字段 %s 类型不符：期望 %s，实际 %s", pathLabel(path), expected, jsonTypeName(value)),
		Value:  truncateString(fmt.Sprintf("%v", value)),
	}
}

// rangeIssue 造一处数值越界的问题。
func rangeIssue(path, value string, low, high float64) *ValidationIssue {
	bound := fmt.Sprintf("[%g, %g]", low, high)
	if high == 0 && low == 0 {
		bound = "[0, 0]"
	}
	return &ValidationIssue{
		Path:   path,
		Kind:   "range_violation",
		Reason: fmt.Sprintf("字段 %s 数值超出允许范围：%s（允许区间 %s）", pathLabel(path), value, bound),
		Value:  value,
	}
}

// pathLabel 把字段路径写成可读标签；根节点写作 <根>。
func pathLabel(path string) string {
	if path == "" {
		return "<根>"
	}
	return path
}

// joinPath 拼接字段路径。
func joinPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// indexPath 拼接数组下标路径。
func indexPath(prefix string, index int) string {
	if prefix == "" {
		return fmt.Sprintf("[%d]", index)
	}
	return fmt.Sprintf("%s[%d]", prefix, index)
}

// jsonTypeName 给出 JSON 值的类型名，用于错误文案。
func jsonTypeName(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		_ = v
		return "unknown"
	}
}

// contains 判断取值是否落在集合内。
func contains(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

// truncateString 截断过长文本。
func truncateString(s string) string {
	if len(s) <= maxAuditFragment {
		return s
	}
	return s[:maxAuditFragment] + "…（已截断）"
}

// truncateForAudit 截断原始响应片段（按字节切，避免切坏 UTF-8）。
func truncateForAudit(raw []byte) string {
	if len(raw) <= maxAuditFragment {
		return string(raw)
	}
	cut := raw[:maxAuditFragment]
	// 回退到合法的 UTF-8 边界，免得日志里出现半个字。
	for len(cut) > 0 && !json.Valid(cut) {
		cut = cut[:len(cut)-1]
	}
	return string(cut) + "…（已截断）"
}

// Evidence 是一条可回溯的证据。
//
// 证据必须带特征名、实测值与阈值：只有「是开门导致的」而没有
// 「门开累计 47 分钟、温升起点在开门后 2 分钟」，复核的人无从判断
// 这条结论站不站得住。
type Evidence struct {
	Feature   string  `json:"feature"`
	Value     float64 `json:"value"`
	Threshold float64 `json:"threshold"`
	Note      string  `json:"note,omitempty"`
}

// Candidate 是一个归因候选原因。
type Candidate struct {
	CauseCode  string     `json:"cause_code"`
	Label      string     `json:"label,omitempty"`
	Confidence float64    `json:"confidence"`
	Evidence   []Evidence `json:"evidence,omitempty"`
	Note       string     `json:"note,omitempty"`
}

// Attribution 是归因结论响应。
//
// NeedsReview 表示这份结论要素不全（模型少给了字段）。
// 缺失的字段以显式缺省表达，不凭空补值：缺什么就记在 MissingFields 里，
// 由成稿自检点出「这份稿子还差什么」，而不是拿 0 或空串顶替。
type Attribution struct {
	CauseCode  string      `json:"cause_code"`
	Label      string      `json:"label,omitempty"`
	Confidence float64     `json:"confidence"`
	Candidates []Candidate `json:"candidates,omitempty"`
	Evidence   []Evidence  `json:"evidence,omitempty"`
	Notes      []string    `json:"notes,omitempty"`

	NeedsReview   bool     `json:"needs_review"`
	MissingFields []string `json:"missing_fields,omitempty"`
	ExtraFields   []string `json:"extra_fields,omitempty"`
}

// Window 是越限窗口响应。
type Window struct {
	Start           time.Time `json:"start"`
	End             time.Time `json:"end"`
	DurationMinutes int       `json:"duration_minutes"`
	PeakC           float64   `json:"peak_c"`
	Notes           []string  `json:"notes,omitempty"`

	NeedsReview   bool     `json:"needs_review"`
	MissingFields []string `json:"missing_fields,omitempty"`
	ExtraFields   []string `json:"extra_fields,omitempty"`
}

// BatchAdviceItem 是一条批次建议。
type BatchAdviceItem struct {
	BatchRef   string  `json:"batch_ref"`
	Advice     string  `json:"advice"`
	Rationale  string  `json:"rationale,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	Note       string  `json:"note,omitempty"`
}

// BatchAdvice 是在库批次建议响应（根是数组）。
type BatchAdvice struct {
	Items []BatchAdviceItem `json:"items"`

	NeedsReview   bool     `json:"needs_review"`
	MissingFields []string `json:"missing_fields,omitempty"`
	ExtraFields   []string `json:"extra_fields,omitempty"`
}

// ExceptionRecord 是《温度异常情况记录表》响应。
type ExceptionRecord struct {
	PointID            string            `json:"point_id"`
	DeviceName         string            `json:"device_name"`
	ExcursionStartedAt time.Time         `json:"excursion_started_at"`
	ExcursionEndedAt   time.Time         `json:"excursion_ended_at"`
	PeakTemperatureC   float64           `json:"peak_temperature_c"`
	DurationMinutes    int               `json:"duration_minutes"`
	Zone               string            `json:"zone"`
	Batches            []BatchAdviceItem `json:"batches,omitempty"`
	CauseConclusion    string            `json:"cause_conclusion,omitempty"`
	Evidence           []Evidence        `json:"evidence,omitempty"`
	Disposition        string            `json:"disposition,omitempty"`
	DispositionNote    string            `json:"disposition_note,omitempty"`
	Notes              []string          `json:"notes,omitempty"`

	NeedsReview   bool     `json:"needs_review"`
	MissingFields []string `json:"missing_fields,omitempty"`
	ExtraFields   []string `json:"extra_fields,omitempty"`
}

// DispositionNote 是处置说明响应。
type DispositionNote struct {
	Disposition string  `json:"disposition"`
	Note        string  `json:"note"`
	Rationale   string  `json:"rationale,omitempty"`
	Confidence  float64 `json:"confidence,omitempty"`

	NeedsReview   bool     `json:"needs_review"`
	MissingFields []string `json:"missing_fields,omitempty"`
	ExtraFields   []string `json:"extra_fields,omitempty"`
}

// ParseAttribution 校验并解析一份归因结论响应。
func ParseAttribution(raw []byte) (*Attribution, error) {
	p, err := parse(SchemaAttribution, raw)
	if err != nil {
		return nil, err
	}
	var out Attribution
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, newSchemaError(SchemaAttribution, ValidationIssue{
			Kind:   "type_mismatch",
			Reason: fmt.Sprintf("响应结构与归因结论不一致：%v", err),
			Value:  truncateForAudit(raw),
		}, raw)
	}
	out.NeedsReview = p.needsReview
	out.MissingFields = p.missingFields
	out.ExtraFields = p.extraFields
	return &out, nil
}

// ParseWindow 校验并解析一份越限窗口响应。
func ParseWindow(raw []byte) (*Window, error) {
	p, err := parse(SchemaWindow, raw)
	if err != nil {
		return nil, err
	}
	var out Window
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, newSchemaError(SchemaWindow, ValidationIssue{
			Kind:   "type_mismatch",
			Reason: fmt.Sprintf("响应结构与越限窗口不一致：%v", err),
			Value:  truncateForAudit(raw),
		}, raw)
	}
	out.NeedsReview = p.needsReview
	out.MissingFields = p.missingFields
	out.ExtraFields = p.extraFields
	return &out, nil
}

// ParseBatchAdvice 校验并解析一份批次建议响应。
func ParseBatchAdvice(raw []byte) (*BatchAdvice, error) {
	p, err := parse(SchemaBatchAdvice, raw)
	if err != nil {
		return nil, err
	}
	var items []BatchAdviceItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, newSchemaError(SchemaBatchAdvice, ValidationIssue{
			Kind:   "type_mismatch",
			Reason: fmt.Sprintf("响应结构与批次建议不一致：%v", err),
			Value:  truncateForAudit(raw),
		}, raw)
	}
	return &BatchAdvice{
		Items:         items,
		NeedsReview:   p.needsReview,
		MissingFields: p.missingFields,
		ExtraFields:   p.extraFields,
	}, nil
}

// ParseExceptionRecord 校验并解析一份异常情况记录表响应。
func ParseExceptionRecord(raw []byte) (*ExceptionRecord, error) {
	p, err := parse(SchemaExceptionRecord, raw)
	if err != nil {
		return nil, err
	}
	var out ExceptionRecord
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, newSchemaError(SchemaExceptionRecord, ValidationIssue{
			Kind:   "type_mismatch",
			Reason: fmt.Sprintf("响应结构与异常情况记录表不一致：%v", err),
			Value:  truncateForAudit(raw),
		}, raw)
	}
	out.NeedsReview = p.needsReview
	out.MissingFields = p.missingFields
	out.ExtraFields = p.extraFields
	return &out, nil
}

// ParseDispositionNote 校验并解析一份处置说明响应。
func ParseDispositionNote(raw []byte) (*DispositionNote, error) {
	p, err := parse(SchemaDispositionNote, raw)
	if err != nil {
		return nil, err
	}
	var out DispositionNote
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, newSchemaError(SchemaDispositionNote, ValidationIssue{
			Kind:   "type_mismatch",
			Reason: fmt.Sprintf("响应结构与处置说明不一致：%v", err),
			Value:  truncateForAudit(raw),
		}, raw)
	}
	out.NeedsReview = p.needsReview
	out.MissingFields = p.missingFields
	out.ExtraFields = p.extraFields
	return &out, nil
}
