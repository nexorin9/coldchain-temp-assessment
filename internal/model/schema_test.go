package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// 合法归因响应：原因代码、置信度、候选集与证据齐备。
const validAttribution = `{
  "cause_code": "power_loss",
  "label": "断电",
  "confidence": 0.9,
  "candidates": [
    {
      "cause_code": "power_loss",
      "label": "断电",
      "confidence": 0.9,
      "evidence": [
        {"feature": "device_event:power_loss", "value": 2, "threshold": 1, "note": "记录仪上报断电事件"}
      ]
    },
    {"cause_code": "door_open", "label": "开门", "confidence": 0.3}
  ],
  "evidence": [
    {"feature": "device_event:power_loss", "value": 2, "threshold": 1}
  ],
  "notes": ["断电事件由设备直接上报，是最硬的一条证据"]
}`

// 合法窗口响应。
const validWindow = `{
  "start": "2026-10-20T03:10:00+08:00",
  "end": "2026-10-20T03:25:00+08:00",
  "duration_minutes": 15,
  "peak_c": 12.4
}`

// 合法批次建议响应（根是数组）。
const validBatchAdvice = `[
  {"batch_ref": "<EXAMPLE_LOT-01>", "advice": "pending_manufacturer_evaluation",
   "rationale": "夜间断电数小时，质量影响待企业评估", "confidence": 0.8}
]`

// 合法异常情况记录表响应。
const validExceptionRecord = `{
  "point_id": "point-vax-freezer-01",
  "device_name": "疫苗冷藏箱-01",
  "excursion_started_at": "2026-10-20T03:10:00+08:00",
  "excursion_ended_at": "2026-10-20T03:25:00+08:00",
  "peak_temperature_c": 12.4,
  "duration_minutes": 15,
  "zone": "2~8℃",
  "batches": [{"batch_ref": "<EXAMPLE_LOT-01>", "advice": "pending_manufacturer_evaluation"}],
  "cause_conclusion": "断电",
  "evidence": [{"feature": "device_event:power_loss", "value": 2, "threshold": 1}],
  "disposition": "pending_manufacturer_evaluation",
  "disposition_note": "断电期间温升超出温区上限，批次质量影响待疫苗生产企业评估"
}`

// 合法处置说明响应。
const validDispositionNote = `{
  "disposition": "isolate",
  "note": "建议先隔离存放，勿发放",
  "rationale": "越限时长超过 15 分钟",
  "confidence": 0.7
}`

// TestSchemas_RegisteredAndNamed 断言五组 schema 都已登记且分码固定。
func TestSchemas_RegisteredAndNamed(t *testing.T) {
	want := []SchemaName{
		SchemaAttribution, SchemaWindow, SchemaBatchAdvice,
		SchemaExceptionRecord, SchemaDispositionNote,
	}
	got := SchemaNames()
	if len(got) != len(want) {
		t.Fatalf("期望登记 %d 组 schema，实际 %d 组", len(want), len(got))
	}
	for i, name := range want {
		if got[i] != name {
			t.Fatalf("第 %d 组分码期望 %q，实际 %q", i, name, got[i])
		}
		schema, ok := Lookup(name)
		if !ok {
			t.Fatalf("分码 %q 未登记", name)
		}
		if schema.Title == "" || schema.Description == "" {
			t.Fatalf("分码 %q 缺标题或说明", name)
		}
		body, err := schema.JSONSchemaJSON()
		if err != nil {
			t.Fatalf("分码 %q 的 JSON Schema 序列化失败：%v", name, err)
		}
		if !json.Valid(body) {
			t.Fatalf("分码 %q 的 JSON Schema 不是合法 JSON", name)
		}
	}
}

// TestJSONSchema_FieldNamesMatchSpec 断言请求侧 JSON Schema 的字段名与 spec 一致。
func TestJSONSchema_FieldNamesMatchSpec(t *testing.T) {
	cases := []struct {
		schema SchemaName
		fields []string
	}{
		{SchemaAttribution, []string{"cause_code", "confidence", "candidates", "evidence"}},
		{SchemaWindow, []string{"start", "end", "duration_minutes", "peak_c"}},
		{SchemaExceptionRecord, []string{
			"point_id", "device_name", "excursion_started_at", "excursion_ended_at",
			"peak_temperature_c", "duration_minutes", "zone", "batches",
			"cause_conclusion", "evidence", "disposition", "disposition_note",
		}},
		{SchemaDispositionNote, []string{"disposition", "note"}},
	}
	for _, tc := range cases {
		schema, ok := Lookup(tc.schema)
		if !ok {
			t.Fatalf("分码 %q 未登记", tc.schema)
		}
		doc := schema.JSONSchema()
		props, ok := doc["properties"].(map[string]any)
		if !ok {
			t.Fatalf("分码 %q 的 JSON Schema 缺 properties", tc.schema)
		}
		for _, field := range tc.fields {
			if _, ok := props[field]; !ok {
				t.Fatalf("分码 %q 的 JSON Schema 缺字段 %q", tc.schema, field)
			}
		}
	}
}

// TestJSONSchema_BatchAdviceIsArray 断言批次建议的根是数组。
func TestJSONSchema_BatchAdviceIsArray(t *testing.T) {
	schema, ok := Lookup(SchemaBatchAdvice)
	if !ok {
		t.Fatalf("分码 %q 未登记", SchemaBatchAdvice)
	}
	doc := schema.JSONSchema()
	if doc["type"] != string(kindArray) {
		t.Fatalf("批次建议的根应为 array，实际 %v", doc["type"])
	}
	items, ok := doc["items"].(map[string]any)
	if !ok {
		t.Fatal("批次建议的 JSON Schema 缺 items")
	}
	props, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatal("批次建议的 items 缺 properties")
	}
	for _, field := range []string{"batch_ref", "advice"} {
		if _, ok := props[field]; !ok {
			t.Fatalf("批次建议缺字段 %q", field)
		}
	}
}

// TestParseAttribution_ValidResponse 断言合法归因响应通过校验。
func TestParseAttribution_ValidResponse(t *testing.T) {
	out, err := ParseAttribution([]byte(validAttribution))
	if err != nil {
		t.Fatalf("合法归因响应应通过校验，实际报错：%v", err)
	}
	if out.CauseCode != "power_loss" {
		t.Fatalf("期望 cause_code=power_loss，实际 %q", out.CauseCode)
	}
	if out.Confidence != 0.9 {
		t.Fatalf("期望 confidence=0.9，实际 %v", out.Confidence)
	}
	if len(out.Candidates) != 2 {
		t.Fatalf("期望 2 个候选，实际 %d 个", len(out.Candidates))
	}
	if len(out.Evidence) != 1 {
		t.Fatalf("期望 1 条证据，实际 %d 条", len(out.Evidence))
	}
	if out.NeedsReview {
		t.Fatal("要素齐全的响应不应标 needs_review")
	}
	if len(out.MissingFields) != 0 {
		t.Fatalf("要素齐全的响应不应有缺失字段，实际 %v", out.MissingFields)
	}
}

// TestParseWindow_ValidResponse 断言合法窗口响应通过校验并解析出时刻。
func TestParseWindow_ValidResponse(t *testing.T) {
	out, err := ParseWindow([]byte(validWindow))
	if err != nil {
		t.Fatalf("合法窗口响应应通过校验，实际报错：%v", err)
	}
	if out.DurationMinutes != 15 {
		t.Fatalf("期望 duration_minutes=15，实际 %d", out.DurationMinutes)
	}
	if out.PeakC != 12.4 {
		t.Fatalf("期望 peak_c=12.4，实际 %v", out.PeakC)
	}
	if out.Start.Format(time.RFC3339) != "2026-10-20T03:10:00+08:00" {
		t.Fatalf("窗口起点解析错误：%s", out.Start.Format(time.RFC3339))
	}
}

// TestParseBatchAdvice_ValidResponse 断言合法批次建议通过校验。
func TestParseBatchAdvice_ValidResponse(t *testing.T) {
	out, err := ParseBatchAdvice([]byte(validBatchAdvice))
	if err != nil {
		t.Fatalf("合法批次建议应通过校验，实际报错：%v", err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("期望 1 条批次建议，实际 %d 条", len(out.Items))
	}
	if out.Items[0].BatchRef != "<EXAMPLE_LOT-01>" {
		t.Fatalf("批次号应原样保留，实际 %q", out.Items[0].BatchRef)
	}
	if out.Items[0].Advice != DispositionPendingEvaluation {
		t.Fatalf("期望建议 %q，实际 %q", DispositionPendingEvaluation, out.Items[0].Advice)
	}
}

// TestParseExceptionRecord_ValidResponse 断言合法记录表响应通过校验。
func TestParseExceptionRecord_ValidResponse(t *testing.T) {
	out, err := ParseExceptionRecord([]byte(validExceptionRecord))
	if err != nil {
		t.Fatalf("合法记录表响应应通过校验，实际报错：%v", err)
	}
	if out.Zone != "2~8℃" {
		t.Fatalf("期望 zone=2~8℃，实际 %q", out.Zone)
	}
	if out.Disposition != DispositionPendingEvaluation {
		t.Fatalf("期望处置结论 %q，实际 %q", DispositionPendingEvaluation, out.Disposition)
	}
	if len(out.Batches) != 1 {
		t.Fatalf("期望 1 个批次，实际 %d 个", len(out.Batches))
	}
}

// TestParseDispositionNote_ValidResponse 断言合法处置说明通过校验。
func TestParseDispositionNote_ValidResponse(t *testing.T) {
	out, err := ParseDispositionNote([]byte(validDispositionNote))
	if err != nil {
		t.Fatalf("合法处置说明应通过校验，实际报错：%v", err)
	}
	if out.Disposition != DispositionIsolate {
		t.Fatalf("期望处置 %q，实际 %q", DispositionIsolate, out.Disposition)
	}
}

// TestParse_MissingRequiredFieldRejected 断言必填字段缺失一律不合规。
func TestParse_MissingRequiredFieldRejected(t *testing.T) {
	cases := []struct {
		name    string
		schema  SchemaName
		raw     string
		wantSub string
	}{
		{
			name:    "归因缺 cause_code",
			schema:  SchemaAttribution,
			raw:     `{"confidence": 0.8}`,
			wantSub: "cause_code",
		},
		{
			name:    "归因缺 confidence",
			schema:  SchemaAttribution,
			raw:     `{"cause_code": "power_loss"}`,
			wantSub: "confidence",
		},
		{
			name:    "窗口缺 duration_minutes",
			schema:  SchemaWindow,
			raw:     `{"start": "2026-10-20T03:10:00+08:00", "end": "2026-10-20T03:25:00+08:00", "peak_c": 12.4}`,
			wantSub: "duration_minutes",
		},
		{
			name:    "记录表缺 point_id",
			schema:  SchemaExceptionRecord,
			raw:     `{"device_name": "疫苗冷藏箱-01"}`,
			wantSub: "point_id",
		},
		{
			name:    "处置说明缺 note",
			schema:  SchemaDispositionNote,
			raw:     `{"disposition": "isolate"}`,
			wantSub: "note",
		},
		{
			name:    "批次建议缺 batch_ref",
			schema:  SchemaBatchAdvice,
			raw:     `[{"advice": "isolate"}]`,
			wantSub: "batch_ref",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(tc.schema, []byte(tc.raw))
			if err == nil {
				t.Fatal("必填字段缺失应判不合规")
			}
			if code := errcode.As(err).Code; code != errcode.ModelSchemaInvalid {
				t.Fatalf("期望错误码 %s，实际 %s", errcode.ModelSchemaInvalid, code)
			}
			var schemaErr *SchemaError
			if !errors.As(err, &schemaErr) {
				t.Fatalf("期望 *SchemaError，实际 %T", err)
			}
			if schemaErr.Issue.Kind != "missing_required" {
				t.Fatalf("期望问题类型 missing_required，实际 %q", schemaErr.Issue.Kind)
			}
			if !strings.Contains(schemaErr.Issue.Path, tc.wantSub) {
				t.Fatalf("期望问题字段含 %q，实际 %q", tc.wantSub, schemaErr.Issue.Path)
			}
		})
	}
}

// TestParse_TypeMismatchRejected 断言字段类型错一律不合规。
func TestParse_TypeMismatchRejected(t *testing.T) {
	cases := []struct {
		name   string
		schema SchemaName
		raw    string
	}{
		{"confidence 写成字符串", SchemaAttribution,
			`{"cause_code": "power_loss", "confidence": "0.9"}`},
		{"cause_code 写成数字", SchemaAttribution,
			`{"cause_code": 7, "confidence": 0.9}`},
		{"duration_minutes 写成小数", SchemaWindow,
			`{"start": "2026-10-20T03:10:00+08:00", "end": "2026-10-20T03:25:00+08:00", "duration_minutes": 15.5, "peak_c": 12.4}`},
		{"candidates 写成对象", SchemaAttribution,
			`{"cause_code": "power_loss", "confidence": 0.9, "candidates": {"cause_code": "door_open"}}`},
		{"根节点是字符串", SchemaAttribution, `"power_loss"`},
		{"批次建议根是对象", SchemaBatchAdvice, `{"batch_ref": "<EXAMPLE_LOT-01>"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(tc.schema, []byte(tc.raw))
			if err == nil {
				t.Fatal("类型不符应判不合规")
			}
			if code := errcode.As(err).Code; code != errcode.ModelSchemaInvalid {
				t.Fatalf("期望错误码 %s，实际 %s", errcode.ModelSchemaInvalid, code)
			}
			var schemaErr *SchemaError
			if !errors.As(err, &schemaErr) {
				t.Fatalf("期望 *SchemaError，实际 %T", err)
			}
			if schemaErr.Issue.Kind != "type_mismatch" {
				t.Fatalf("期望问题类型 type_mismatch，实际 %q", schemaErr.Issue.Kind)
			}
		})
	}
}

// TestParse_EnumOutOfRangeRejected 断言枚举越界一律不合规。
func TestParse_EnumOutOfRangeRejected(t *testing.T) {
	cases := []struct {
		name   string
		schema SchemaName
		raw    string
	}{
		{"cause_code 不在原因字典内", SchemaAttribution,
			`{"cause_code": "sunshine", "confidence": 0.9}`},
		{"候选 cause_code 不在原因字典内", SchemaAttribution,
			`{"cause_code": "power_loss", "confidence": 0.9, "candidates": [{"cause_code": "sunshine", "confidence": 0.5}]}`},
		{"advice 不在处置字典内", SchemaBatchAdvice,
			`[{"batch_ref": "<EXAMPLE_LOT-01>", "advice": "ship_it"}]`},
		{"disposition 不在处置字典内", SchemaDispositionNote,
			`{"disposition": "ship_it", "note": "直接放行"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(tc.schema, []byte(tc.raw))
			if err == nil {
				t.Fatal("枚举越界应判不合规")
			}
			if code := errcode.As(err).Code; code != errcode.ModelSchemaInvalid {
				t.Fatalf("期望错误码 %s，实际 %s", errcode.ModelSchemaInvalid, code)
			}
			var schemaErr *SchemaError
			if !errors.As(err, &schemaErr) {
				t.Fatalf("期望 *SchemaError，实际 %T", err)
			}
			if schemaErr.Issue.Kind != "enum_violation" {
				t.Fatalf("期望问题类型 enum_violation，实际 %q", schemaErr.Issue.Kind)
			}
		})
	}
}

// TestParse_ConfidenceOutOfRangeRejected 断言置信度越界不合规。
func TestParse_ConfidenceOutOfRangeRejected(t *testing.T) {
	for _, raw := range []string{
		`{"cause_code": "power_loss", "confidence": 1.4}`,
		`{"cause_code": "power_loss", "confidence": -0.1}`,
	} {
		_, err := parse(SchemaAttribution, []byte(raw))
		if err == nil {
			t.Fatalf("置信度越界应判不合规：%s", raw)
		}
		if code := errcode.As(err).Code; code != errcode.ModelSchemaInvalid {
			t.Fatalf("期望错误码 %s，实际 %s", errcode.ModelSchemaInvalid, code)
		}
		var schemaErr *SchemaError
		if !errors.As(err, &schemaErr) {
			t.Fatalf("期望 *SchemaError，实际 %T", err)
		}
		if schemaErr.Issue.Kind != "range_violation" {
			t.Fatalf("期望问题类型 range_violation，实际 %q", schemaErr.Issue.Kind)
		}
	}
}

// TestParse_NonJSONResponseRejected 断言非 JSON 响应一律不合规。
func TestParse_NonJSONResponseRejected(t *testing.T) {
	cases := []string{
		"这不是 JSON",
		"",
		"   ",
		"{cause_code: power_loss}",
		`{"cause_code": "power_loss", "confidence": 0.9}{"cause_code": "door_open"}`,
		"```json\n{\"cause_code\": \"power_loss\"}\n```",
	}
	for _, raw := range cases {
		_, err := parse(SchemaAttribution, []byte(raw))
		if err == nil {
			t.Fatalf("非 JSON 响应应判不合规：%q", raw)
		}
		if code := errcode.As(err).Code; code != errcode.ModelSchemaInvalid {
			t.Fatalf("期望错误码 %s，实际 %s", errcode.ModelSchemaInvalid, code)
		}
		var schemaErr *SchemaError
		if !errors.As(err, &schemaErr) {
			t.Fatalf("期望 *SchemaError，实际 %T", err)
		}
		if schemaErr.Issue.Kind != "not_json" {
			t.Fatalf("期望问题类型 not_json，实际 %q", schemaErr.Issue.Kind)
		}
	}
}

// TestParse_BadTimestampRejected 断言时间戳格式不符不合规。
func TestParse_BadTimestampRejected(t *testing.T) {
	for _, raw := range []string{
		`{"start": "2026-10-20 03:10:00", "end": "2026-10-20T03:25:00+08:00", "duration_minutes": 15, "peak_c": 12.4}`,
		`{"start": "2026-10-20T03:10:00", "end": "2026-10-20T03:25:00+08:00", "duration_minutes": 15, "peak_c": 12.4}`,
	} {
		_, err := parse(SchemaWindow, []byte(raw))
		if err == nil {
			t.Fatalf("时间戳格式不符应判不合规：%s", raw)
		}
		var schemaErr *SchemaError
		if !errors.As(err, &schemaErr) {
			t.Fatalf("期望 *SchemaError，实际 %T", err)
		}
		if schemaErr.Issue.Kind != "format_violation" {
			t.Fatalf("期望问题类型 format_violation，实际 %q", schemaErr.Issue.Kind)
		}
	}
}

// TestParse_PartialResponseMarkedNeedsReview 断言部分返回的降级约定：
// 缺失字段显式记下并标 needs_review，不凭空补值。
func TestParse_PartialResponseMarkedNeedsReview(t *testing.T) {
	// 模型只给了原因与置信度，没给候选集与证据。
	out, err := ParseAttribution([]byte(`{"cause_code": "power_loss", "confidence": 0.6}`))
	if err != nil {
		t.Fatalf("部分返回不应判不合规，实际报错：%v", err)
	}
	if !out.NeedsReview {
		t.Fatal("缺候选集与证据的结论应标 needs_review")
	}
	for _, want := range []string{"candidates", "evidence"} {
		found := false
		for _, field := range out.MissingFields {
			if field == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("缺失字段清单应含 %q，实际 %v", want, out.MissingFields)
		}
	}
	if out.Candidates != nil {
		t.Fatalf("缺失的候选集不应被凭空补值，实际 %v", out.Candidates)
	}
	if out.Evidence != nil {
		t.Fatalf("缺失的证据不应被凭空补值，实际 %v", out.Evidence)
	}
}

// TestParse_RecordMissingJudgmentFieldsMarkedNeedsReview 断言记录表缺判断性内容时
// 点名缺失的字段（与成稿自检的 missing_fields 口径一致）。
func TestParse_RecordMissingJudgmentFieldsMarkedNeedsReview(t *testing.T) {
	raw := `{
	  "point_id": "point-vax-freezer-01",
	  "device_name": "疫苗冷藏箱-01",
	  "excursion_started_at": "2026-10-20T03:10:00+08:00",
	  "excursion_ended_at": "2026-10-20T03:25:00+08:00",
	  "peak_temperature_c": 12.4,
	  "duration_minutes": 15,
	  "zone": "2~8℃"
	}`
	out, err := ParseExceptionRecord([]byte(raw))
	if err != nil {
		t.Fatalf("缺判断性内容不应判不合规，实际报错：%v", err)
	}
	if !out.NeedsReview {
		t.Fatal("缺批次与处置的稿子应标 needs_review")
	}
	for _, want := range []string{"batches", "disposition"} {
		found := false
		for _, field := range out.MissingFields {
			if field == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("缺失字段清单应含 %q，实际 %v", want, out.MissingFields)
		}
	}
	if out.Disposition != "" {
		t.Fatalf("缺失的处置结论不应被凭空补值，实际 %q", out.Disposition)
	}
}

// TestParse_ExtraFieldsRecordedNotRejected 断言模型多说的字段只记录不拦截。
func TestParse_ExtraFieldsRecordedNotRejected(t *testing.T) {
	out, err := ParseAttribution([]byte(`{
	  "cause_code": "power_loss",
	  "confidence": 0.9,
	  "evidence": [],
	  "内部备注": "模型多说的字段",
	  "candidates": [{"cause_code": "door_open", "confidence": 0.3, "extra_hint": "多说的候选字段"}]
	}`))
	if err != nil {
		t.Fatalf("多余字段不应导致不合规，实际报错：%v", err)
	}
	if len(out.ExtraFields) != 2 {
		t.Fatalf("期望记录 2 个多余字段，实际 %v", out.ExtraFields)
	}
	if out.NeedsReview {
		t.Fatalf("多余字段不构成要素缺失，不应标 needs_review，实际缺 %v", out.MissingFields)
	}
}

// TestSchemaError_KeepsRawFragment 断言不合规时保留原始响应片段供审计。
func TestSchemaError_KeepsRawFragment(t *testing.T) {
	raw := `{"cause_code": "sunshine", "confidence": 0.9}`
	_, err := parse(SchemaAttribution, []byte(raw))
	if err == nil {
		t.Fatal("枚举越界应判不合规")
	}
	var schemaErr *SchemaError
	if !errors.As(err, &schemaErr) {
		t.Fatalf("期望 *SchemaError，实际 %T", err)
	}
	if schemaErr.RawFragment != raw {
		t.Fatalf("期望保留原始响应片段 %q，实际 %q", raw, schemaErr.RawFragment)
	}
	if schemaErr.Schema != SchemaAttribution {
		t.Fatalf("期望错误记录分码 %q，实际 %q", SchemaAttribution, schemaErr.Schema)
	}
	if !strings.Contains(schemaErr.Base.Details, "cause_code") {
		t.Fatalf("错误详情应点名问题字段，实际 %q", schemaErr.Base.Details)
	}
}

// TestSchemaError_LongResponseTruncated 断言超长响应片段被截断而非整段塞进错误。
func TestSchemaError_LongResponseTruncated(t *testing.T) {
	raw := `{"cause_code": "sunshine", "confidence": 0.9, "pad": "` + strings.Repeat("x", 2000) + `"}`
	_, err := parse(SchemaAttribution, []byte(raw))
	if err == nil {
		t.Fatal("枚举越界应判不合规")
	}
	var schemaErr *SchemaError
	if !errors.As(err, &schemaErr) {
		t.Fatalf("期望 *SchemaError，实际 %T", err)
	}
	if len(schemaErr.RawFragment) > maxAuditFragment+32 {
		t.Fatalf("原始片段应被截断，实际长度 %d", len(schemaErr.RawFragment))
	}
	if !strings.Contains(schemaErr.RawFragment, "已截断") {
		t.Fatalf("截断后的片段应标注已截断，实际 %q", schemaErr.RawFragment[len(schemaErr.RawFragment)-20:])
	}
}

// TestBuildRequest_StructuredShape 断言请求体带 response_format.json_schema，
// 且字段名与响应侧解析一致。
func TestBuildRequest_StructuredShape(t *testing.T) {
	body, err := BuildRequest(SchemaAttribution, RequestOptions{
		Model:        "test-model",
		SystemPrompt: DefaultSystemPrompt(SchemaAttribution),
		UserPayload: map[string]any{
			"point_id":    "point-vax-freezer-01",
			"samples":     []map[string]any{{"measured_at": "2026-10-20T03:15:00+08:00", "temperature_c": 11.4}},
			"zone":        "2~8℃",
			"attribution": "归因越温原因",
		},
	})
	if err != nil {
		t.Fatalf("构造结构化请求失败：%v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("请求体不是合法 JSON：%v", err)
	}
	if doc["model"] != "test-model" {
		t.Fatalf("请求体缺模型名：%v", doc["model"])
	}
	rf, ok := doc["response_format"].(map[string]any)
	if !ok {
		t.Fatal("请求体缺 response_format")
	}
	if rf["type"] != "json_schema" {
		t.Fatalf("response_format.type 期望 json_schema，实际 %v", rf["type"])
	}
	js, ok := rf["json_schema"].(map[string]any)
	if !ok {
		t.Fatal("response_format 缺 json_schema")
	}
	if js["name"] != string(SchemaAttribution) {
		t.Fatalf("json_schema.name 期望 %q，实际 %v", SchemaAttribution, js["name"])
	}
	if js["strict"] != true {
		t.Fatal("json_schema.strict 应为 true")
	}
	inner, ok := js["schema"].(map[string]any)
	if !ok {
		t.Fatal("json_schema 缺 schema")
	}
	props, ok := inner["properties"].(map[string]any)
	if !ok {
		t.Fatal("json_schema.schema 缺 properties")
	}
	for _, field := range []string{"cause_code", "confidence", "candidates", "evidence"} {
		if _, ok := props[field]; !ok {
			t.Fatalf("请求侧 schema 缺字段 %q", field)
		}
	}
	messages, ok := doc["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("期望 system + user 两条消息，实际 %v", doc["messages"])
	}
	first, _ := messages[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("首条消息应为 system，实际 %v", first["role"])
	}
	second, _ := messages[1].(map[string]any)
	if second["role"] != "user" {
		t.Fatalf("第二条消息应为 user，实际 %v", second["role"])
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(second["content"].(string)), &payload); err != nil {
		t.Fatalf("用户消息内容应为 JSON 载荷：%v", err)
	}
	if payload["point_id"] != "point-vax-freezer-01" {
		t.Fatalf("载荷字段被改写：%v", payload["point_id"])
	}
}

// TestBuildRequest_UnknownSchemaRejected 断言未登记分码被拒。
func TestBuildRequest_UnknownSchemaRejected(t *testing.T) {
	_, err := BuildRequest(SchemaName("not_registered"), RequestOptions{Model: "test-model"})
	if err == nil {
		t.Fatal("未登记分码应报错")
	}
	if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("期望错误码 %s，实际 %s", errcode.Validation, code)
	}
}

// TestBuildRequest_EmptyModelRejected 断言模型名缺失被拒（不发出无名请求）。
func TestBuildRequest_EmptyModelRejected(t *testing.T) {
	_, err := BuildRequest(SchemaAttribution, RequestOptions{})
	if err == nil {
		t.Fatal("模型名为空应报错")
	}
	if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("期望错误码 %s，实际 %s", errcode.Validation, code)
	}
}

// TestErrcodeAs_KeepsSchemaInvalidCode 断言包装后错误码不被降级成 E_VALIDATION。
func TestErrcodeAs_KeepsSchemaInvalidCode(t *testing.T) {
	_, err := parse(SchemaAttribution, []byte(`{"cause_code": "sunshine", "confidence": 0.9}`))
	if err == nil {
		t.Fatal("枚举越界应判不合规")
	}
	if code := errcode.As(err).Code; code != errcode.ModelSchemaInvalid {
		t.Fatalf("包装后错误码应仍为 %s，实际 %s", errcode.ModelSchemaInvalid, code)
	}
}

// ExampleParseAttribution 展示一份合法归因响应被解析成什么。
//
// 它带 Output 注释，所以 go test 会真的跑一遍并比对输出——
// 这段输出就是「模型返回什么、我们拿到什么」的可核对画面。
func ExampleParseAttribution() {
	out, err := ParseAttribution([]byte(`{
	  "cause_code": "power_loss",
	  "label": "断电",
	  "confidence": 0.9,
	  "candidates": [
	    {"cause_code": "power_loss", "confidence": 0.9},
	    {"cause_code": "door_open", "confidence": 0.3}
	  ],
	  "evidence": [{"feature": "device_event:power_loss", "value": 2, "threshold": 1}]
	}`))
	if err != nil {
		fmt.Println("校验失败：", errcode.As(err).Code)
		return
	}
	fmt.Printf("cause_code=%s confidence=%.1f candidates=%d evidence=%d needs_review=%t\n",
		out.CauseCode, out.Confidence, len(out.Candidates), len(out.Evidence), out.NeedsReview)

	// Output:
	// cause_code=power_loss confidence=0.9 candidates=2 evidence=1 needs_review=false
}

// ExampleParseAttribution_invalid 展示不合规响应会被拦下，并给出可核对的错误码。
func ExampleParseAttribution_invalid() {
	_, err := ParseAttribution([]byte(`{"cause_code": "sunshine", "confidence": 0.9}`))
	if err == nil {
		fmt.Println("本该拦下，却通过了")
		return
	}
	e := errcode.As(err)
	fmt.Printf("%s：%s\n", e.Code, e.Details)

	// Output:
	// E_MODEL_SCHEMA_INVALID：字段 cause_code 取值不在允许集合内："sunshine"（允许：door_open、power_loss、defrost、probe_detached、probe_stuck、ac_failure、overstock、unknown）
}

// ExampleParseAttribution_partial 展示「只给部分候选」时的降级约定：
// 缺失字段显式记下并标 needs_review，不凭空补值。
func ExampleParseAttribution_partial() {
	out, err := ParseAttribution([]byte(`{"cause_code": "power_loss", "confidence": 0.6}`))
	if err != nil {
		fmt.Println("校验失败：", errcode.As(err).Code)
		return
	}
	fmt.Printf("needs_review=%t missing_fields=%v candidates=%v\n",
		out.NeedsReview, out.MissingFields, out.Candidates)

	// Output:
	// needs_review=true missing_fields=[candidates evidence] candidates=[]
}
