package model

import (
	"context"
	"testing"

	"github.com/chaosforge/coldchain-temp-assessment/internal/audit"
	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// memAuditor 是一个把模型调用审计记录收在内存里的落库替身。
type memAuditor struct {
	calls []audit.ModelCall
	err   error
}

func (m *memAuditor) RecordModelCall(_ context.Context, call audit.ModelCall) error {
	if m.err != nil {
		return m.err
	}
	m.calls = append(m.calls, call)
	return nil
}

// auditedAsk 是一个带用量回报的提问出口。
//
// 用量（token）与模型名是真实端点会回报的东西：审计记录里要有的就是它们。
type auditedAsk struct {
	responses []string
	calls     int
	failWith  error
}

func (a *auditedAsk) ask(_ context.Context, schema SchemaName, _ any, _ string) (*Completion, error) {
	a.calls++
	if a.failWith != nil {
		return nil, a.failWith
	}
	if a.calls > len(a.responses) {
		return nil, errcode.Newf(errcode.ModelUnavailable, "脚本已无响应可回（第 %d 次调用）", a.calls)
	}
	return &Completion{
		Schema:           schema,
		Content:          []byte(a.responses[a.calls-1]),
		Model:            "院内判定模型",
		PromptTokens:     900,
		CompletionTokens: 120,
		TotalTokens:      1020,
		LatencyMS:        812,
		PromptDigest:     audit.Digest(`{"point_id":"point-vax-freezer-01"}`),
		PromptChars:      35,
	}, nil
}

// TestAsker_RecordsSuccessfulCallWithTokens 验证一次通过的调用留下完整元数据。
//
// 内审要回答「用了哪个模型、花了多少 token、算下来多少钱」——
// 审计记录里这几个字段缺一个，那条记录就答不了这个问题。
func TestAsker_RecordsSuccessfulCallWithTokens(t *testing.T) {
	caller := &auditedAsk{responses: []string{
		`{"cause_code":"door_open","confidence":0.9}`,
	}}
	auditor := &memAuditor{}
	asker := &Asker{
		Caller:   caller.ask,
		Basis:    factBasis(),
		RunID:    "run-audit-001",
		StepName: "attribute",
		Auditor:  auditor,
	}

	_, result, err := asker.Ask(context.Background(), SchemaAttribution, nil)
	if err != nil {
		t.Fatalf("模型调用失败：%v", err)
	}
	if result.Attempts != 1 {
		t.Fatalf("调用次数 = %d，期望 1", result.Attempts)
	}
	if len(auditor.calls) != 1 {
		t.Fatalf("审计记录条数 = %d，期望 1", len(auditor.calls))
	}
	call := auditor.calls[0]
	if call.RunID != "run-audit-001" || call.StepName != "attribute" {
		t.Errorf("调用身份 = %q/%q，期望 run-audit-001/attribute", call.RunID, call.StepName)
	}
	if call.SchemaName != string(SchemaAttribution) {
		t.Errorf("schema_name = %q，期望 %q", call.SchemaName, SchemaAttribution)
	}
	if call.Model != "院内判定模型" {
		t.Errorf("model = %q，期望 院内判定模型", call.Model)
	}
	if call.PromptTokens != 900 || call.CompletionTokens != 120 || call.TotalTokens != 1020 {
		t.Errorf("token 字段 = %d/%d/%d，期望 900/120/1020",
			call.PromptTokens, call.CompletionTokens, call.TotalTokens)
	}
	if call.LatencyMS != 812 {
		t.Errorf("latency_ms = %d，期望 812", call.LatencyMS)
	}
	if call.Outcome != domain.AuditOutcomeSucceeded {
		t.Errorf("outcome = %q，期望 %q", call.Outcome, domain.AuditOutcomeSucceeded)
	}
	if call.ErrorCode != "" {
		t.Errorf("error_code = %q，期望空", call.ErrorCode)
	}
	if call.PromptDigest == "" || call.PromptChars == 0 {
		t.Errorf("提示词指纹 = %q/%d，期望非空", call.PromptDigest, call.PromptChars)
	}
}

// TestAsker_RecordsFactMismatchAsFailed 验证事实不符记「失败」而不是「成功」。
//
// HTTP 200 只说明端点回话了；答的数与现场对不上，这次调用就没成。
// 审计链要如实这么写，否则内审看到的是一条「成功」记录，
// 而结论其实早就被拦下了。
func TestAsker_RecordsFactMismatchAsFailed(t *testing.T) {
	// 两次都答同一个编造的温度：重问一次后仍不符，才是事实不符的终态。
	caller := &auditedAsk{responses: []string{
		`{"cause_code":"door_open","confidence":0.9,"evidence":[{"feature":"temperature_c","value":13.7,"threshold":8}]}`,
		`{"cause_code":"door_open","confidence":0.9,"evidence":[{"feature":"temperature_c","value":13.7,"threshold":8}]}`,
	}}
	auditor := &memAuditor{}
	asker := &Asker{
		Caller:   caller.ask,
		Basis:    factBasis(),
		RunID:    "run-audit-002",
		StepName: "attribute",
		Auditor:  auditor,
	}

	_, _, err := asker.Ask(context.Background(), SchemaAttribution, nil)
	if err == nil {
		t.Fatal("编造的温度应被事实校验拦下")
	}
	if !IsFactMismatch(err) {
		t.Fatalf("错误 = %v，期望事实不符", err)
	}
	if len(auditor.calls) != 2 {
		t.Fatalf("审计记录条数 = %d，期望 2（两次调用各一条）", len(auditor.calls))
	}
	for i, call := range auditor.calls {
		if call.Outcome != domain.AuditOutcomeFailed {
			t.Errorf("第 %d 条 outcome = %q，期望 %q", i+1, call.Outcome, domain.AuditOutcomeFailed)
		}
		if call.ErrorCode != string(errcode.ModelFactMismatch) {
			t.Errorf("第 %d 条 error_code = %q，期望 %q",
				i+1, call.ErrorCode, errcode.ModelFactMismatch)
		}
	}
}

// TestAsker_RecordsShapeInvalidAsFailed 验证形状不合规记「失败」并带对应错误码。
func TestAsker_RecordsShapeInvalidAsFailed(t *testing.T) {
	// 两次都答不合规的形状：重问一次后仍不合规。
	caller := &auditedAsk{responses: []string{
		`{"cause_code":"不存在的原因"}`,
		`{"cause_code":"不存在的原因"}`,
	}}
	auditor := &memAuditor{}
	asker := &Asker{
		Caller:   caller.ask,
		Basis:    factBasis(),
		RunID:    "run-audit-003",
		StepName: "attribute",
		Auditor:  auditor,
	}

	_, _, err := asker.Ask(context.Background(), SchemaAttribution, nil)
	if err == nil {
		t.Fatal("形状不合规的响应应被拒收")
	}
	if len(auditor.calls) != 2 {
		t.Fatalf("审计记录条数 = %d，期望 2", len(auditor.calls))
	}
	for i, call := range auditor.calls {
		if call.Outcome != domain.AuditOutcomeFailed {
			t.Errorf("第 %d 条 outcome = %q，期望 %q", i+1, call.Outcome, domain.AuditOutcomeFailed)
		}
		if call.ErrorCode != string(errcode.ModelSchemaInvalid) {
			t.Errorf("第 %d 条 error_code = %q，期望 %q",
				i+1, call.ErrorCode, errcode.ModelSchemaInvalid)
		}
	}
}

// TestAsker_RecordsTransportFailure 验证端点不可达也留一条审计记录。
//
// 没发成功的调用也是一次调用：它卡在哪一档、要不要退避重试，
// 端点那边与运维那边都要看得见。
func TestAsker_RecordsTransportFailure(t *testing.T) {
	caller := &auditedAsk{failWith: errcode.Newf(errcode.ModelTimeout, "模型调用超时")}
	auditor := &memAuditor{}
	asker := &Asker{
		Caller:   caller.ask,
		Basis:    factBasis(),
		RunID:    "run-audit-004",
		StepName: "attribute",
		Auditor:  auditor,
	}

	_, _, err := asker.Ask(context.Background(), SchemaAttribution, nil)
	if err == nil {
		t.Fatal("端点不可达时应返回错误")
	}
	if len(auditor.calls) != 1 {
		t.Fatalf("审计记录条数 = %d，期望 1", len(auditor.calls))
	}
	call := auditor.calls[0]
	if call.Outcome != domain.AuditOutcomeFailed {
		t.Errorf("outcome = %q，期望 %q", call.Outcome, domain.AuditOutcomeFailed)
	}
	if call.ErrorCode != string(errcode.ModelTimeout) {
		t.Errorf("error_code = %q，期望 %q", call.ErrorCode, errcode.ModelTimeout)
	}
}

// TestAsker_CallContextSuppliesRunAndStep 验证调用身份可从上下文取。
//
// 网关是唯一知道「发起了这次调用」的地方，但它不知道调用属于哪条判定——
// 那是调用方的上下文。用上下文传递，审计记录就不用靠全局变量追来追去。
func TestAsker_CallContextSuppliesRunAndStep(t *testing.T) {
	caller := &auditedAsk{responses: []string{
		`{"cause_code":"door_open","confidence":0.9}`,
	}}
	auditor := &memAuditor{}
	asker := &Asker{
		Caller:  caller.ask,
		Basis:   factBasis(),
		Auditor: auditor,
	}

	ctx := audit.WithCallContext(context.Background(), "run-audit-005", "draft")
	_, _, err := asker.Ask(ctx, SchemaAttribution, nil)
	if err != nil {
		t.Fatalf("模型调用失败：%v", err)
	}
	if len(auditor.calls) != 1 {
		t.Fatalf("审计记录条数 = %d，期望 1", len(auditor.calls))
	}
	call := auditor.calls[0]
	if call.RunID != "run-audit-005" || call.StepName != "draft" {
		t.Errorf("调用身份 = %q/%q，期望 run-audit-005/draft", call.RunID, call.StepName)
	}
}

// TestAsker_WithoutAuditorStillWorks 验证没装审计记录器时判定照常。
//
// 审计是账本，不是判定的必要条件：账本写不进去，不该让一次本来通过的
// 判定变成失败。
func TestAsker_WithoutAuditorStillWorks(t *testing.T) {
	caller := &auditedAsk{responses: []string{
		`{"cause_code":"door_open","confidence":0.9}`,
	}}
	asker := &Asker{
		Caller:   caller.ask,
		Basis:    factBasis(),
		RunID:    "run-audit-006",
		StepName: "attribute",
	}

	parsed, result, err := asker.Ask(context.Background(), SchemaAttribution, nil)
	if err != nil {
		t.Fatalf("模型调用失败：%v", err)
	}
	if parsed == nil {
		t.Fatal("解析结果为空")
	}
	if result.Attempts != 1 {
		t.Errorf("调用次数 = %d，期望 1", result.Attempts)
	}
}

// TestAsker_AuditFailureDoesNotBreakAssessment 验证审计写不进去时判定照常。
func TestAsker_AuditFailureDoesNotBreakAssessment(t *testing.T) {
	caller := &auditedAsk{responses: []string{
		`{"cause_code":"door_open","confidence":0.9}`,
	}}
	auditor := &memAuditor{err: errcode.Newf(errcode.StorageError, "审计表写不进去")}
	asker := &Asker{
		Caller:   caller.ask,
		Basis:    factBasis(),
		RunID:    "run-audit-007",
		StepName: "attribute",
		Auditor:  auditor,
	}

	parsed, _, err := asker.Ask(context.Background(), SchemaAttribution, nil)
	if err != nil {
		t.Fatalf("审计写失败不该影响判定结论：%v", err)
	}
	if parsed == nil {
		t.Fatal("解析结果为空")
	}
}
