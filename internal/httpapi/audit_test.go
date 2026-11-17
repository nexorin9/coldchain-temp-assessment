package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/audit"
	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
	"github.com/chaosforge/coldchain-temp-assessment/internal/model"
	"github.com/chaosforge/coldchain-temp-assessment/internal/store"
)

// newAuditTestServer 造一台带判定 run 查询与审计链查询能力的 Server。
func newAuditTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	quiet := logging.New(io.Discard)
	srv := New(Options{
		Version:     "0.1.0",
		Logger:      quiet,
		IngestToken: testIngestToken,
		Credentials: testCredentials(),
		RunReader:   st,
		AuditReader: st,
	})
	return srv, st
}

// doGetAudit 发一次带凭据的 GET /v1/runs/{id}/audit。
func doGetAudit(t *testing.T, srv *Server, runID, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/runs/"+runID+"/audit", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// seedAuditChain 在库里造一条判定 run，并往它的审计链上写两条记录：
// 一次模型调用（含 token 与成本）与一次人工确认动作。
func seedAuditChain(t *testing.T, st *store.Store, modelName string, withPrice bool) string {
	t.Helper()
	ctx := context.Background()

	runID := seedRun(t, st)

	opts := []audit.Option{audit.WithSecrets("sk-hospital-secret-0123456789")}
	if withPrice {
		opts = append(opts, audit.WithPricer(audit.NewPriceTable(map[string]audit.Price{
			modelName: {InputCostPerToken: 0.0000012, OutputCostPerToken: 0.0000024},
		})))
	}
	rec := audit.NewRecorder(st, opts...)

	if err := rec.RecordModelCall(ctx, audit.ModelCall{
		RunID:            runID,
		StepName:         "attribute",
		SchemaName:       "attribution",
		Model:            modelName,
		Outcome:          domain.AuditOutcomeSucceeded,
		LatencyMS:        812,
		PromptTokens:     900,
		CompletionTokens: 120,
		TotalTokens:      1020,
		PromptDigest:     audit.Digest(`{"point_id":"point-vax-freezer-01"}`),
		PromptChars:      35,
	}); err != nil {
		t.Fatalf("记录模型调用失败：%v", err)
	}
	if err := rec.RecordConfirmation(ctx, audit.Confirmation{
		RunID:     runID,
		Actor:     "supervisor-007",
		Action:    "confirm",
		FromState: "awaiting_confirmation",
		ToState:   "confirmed",
		Reason:    "现场复核箱内温度后确认处置结论",
	}); err != nil {
		t.Fatalf("记录确认动作失败：%v", err)
	}
	return runID
}

// decodeAudit 解析审计端点的响应体。
func decodeAudit(t *testing.T, rec *httptest.ResponseRecorder) auditSummaryResponse {
	t.Helper()
	var out auditSummaryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析审计响应体失败：%v（原文：%s）", err, rec.Body.String())
	}
	return out
}

// TestGetRunAudit_ReturnsModelCallAndConfirmationRecords 验证审计端点把
// 「机器判了什么、人认了什么」这两段放在同一条链上端出来。
func TestGetRunAudit_ReturnsModelCallAndConfirmationRecords(t *testing.T) {
	srv, st := newAuditTestServer(t)
	runID := seedAuditChain(t, st, "院内判定模型", true)

	rec := doGetAudit(t, srv, runID, testOperatorToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP 状态 = %d，期望 200（响应体：%s）", rec.Code, rec.Body.String())
	}
	out := decodeAudit(t, rec)

	if out.RunID != runID {
		t.Errorf("run_id = %q，期望 %q", out.RunID, runID)
	}
	if out.EntryCount != 2 {
		t.Fatalf("entry_count = %d，期望 2", out.EntryCount)
	}
	if out.ModelCallCount != 1 {
		t.Errorf("model_call_count = %d，期望 1", out.ModelCallCount)
	}
	if out.ConfirmationCount != 1 {
		t.Errorf("confirmation_count = %d，期望 1", out.ConfirmationCount)
	}
	if out.TotalPromptTokens != 900 || out.TotalCompletionTokens != 120 || out.TotalTokens != 1020 {
		t.Errorf("token 合计 = %d/%d/%d，期望 900/120/1020",
			out.TotalPromptTokens, out.TotalCompletionTokens, out.TotalTokens)
	}
	if out.CostStatus != domain.CostStatusKnown {
		t.Errorf("cost_status = %q，期望 %q", out.CostStatus, domain.CostStatusKnown)
	}
	if out.KnownCostAmount == nil {
		t.Fatal("known_cost_amount 为空：成本算得出来时必须有金额")
	}
	if len(out.Entries) != 2 {
		t.Fatalf("entries 条数 = %d，期望 2", len(out.Entries))
	}
	if out.Entries[0].Seq != 1 || out.Entries[1].Seq != 2 {
		t.Errorf("entries 序号 = %d/%d，期望 1/2（按 run 内序号递增）",
			out.Entries[0].Seq, out.Entries[1].Seq)
	}
	if out.Entries[0].EntryType != domain.AuditEntryModelCall {
		t.Errorf("第 1 条 entry_type = %q，期望 %q",
			out.Entries[0].EntryType, domain.AuditEntryModelCall)
	}
	if out.Entries[0].Model != "院内判定模型" {
		t.Errorf("第 1 条 model = %q，期望 院内判定模型", out.Entries[0].Model)
	}
	if out.Entries[0].LatencyMS != 812 {
		t.Errorf("第 1 条 latency_ms = %d，期望 812", out.Entries[0].LatencyMS)
	}
	if out.Entries[0].PromptDigest == "" {
		t.Error("第 1 条 prompt_digest 为空")
	}
	if out.Entries[1].EntryType != domain.AuditEntryConfirmation {
		t.Errorf("第 2 条 entry_type = %q，期望 %q",
			out.Entries[1].EntryType, domain.AuditEntryConfirmation)
	}
	if out.Entries[1].Actor != "supervisor-007" {
		t.Errorf("第 2 条 actor = %q，期望 supervisor-007", out.Entries[1].Actor)
	}
}

// TestGetRunAudit_UnknownCostIsNotZero 验证没配单价表时成本记 unknown 而不是 0。
func TestGetRunAudit_UnknownCostIsNotZero(t *testing.T) {
	srv, st := newAuditTestServer(t)
	runID := seedAuditChain(t, st, "院内判定模型", false)

	rec := doGetAudit(t, srv, runID, testOperatorToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP 状态 = %d，期望 200", rec.Code)
	}
	out := decodeAudit(t, rec)

	if out.CostStatus != domain.CostStatusUnknown {
		t.Errorf("cost_status = %q，期望 %q", out.CostStatus, domain.CostStatusUnknown)
	}
	if out.KnownCostAmount != nil {
		t.Errorf("known_cost_amount = %v，期望为空（算不出就不是 0）", *out.KnownCostAmount)
	}
	if out.UnknownCostCount != 1 {
		t.Errorf("unknown_cost_count = %d，期望 1", out.UnknownCostCount)
	}
}

// TestGetRunAudit_MissingCredentialReturns401 验证缺凭据进不到审计链。
func TestGetRunAudit_MissingCredentialReturns401(t *testing.T) {
	srv, st := newAuditTestServer(t)
	runID := seedAuditChain(t, st, "院内判定模型", true)

	rec := doGetAudit(t, srv, runID, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("HTTP 状态 = %d，期望 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), string(errcode.Unauthorized)) {
		t.Errorf("响应体 = %s，期望含未授权错误码", rec.Body.String())
	}
}

// TestGetRunAudit_UnknownRunReturns404 验证查不到的 run 不回空审计链。
func TestGetRunAudit_UnknownRunReturns404(t *testing.T) {
	srv, _ := newAuditTestServer(t)

	rec := doGetAudit(t, srv, "run-does-not-exist", testOperatorToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("HTTP 状态 = %d，期望 404（响应体：%s）", rec.Code, rec.Body.String())
	}
}

// TestGetRunAudit_WithoutAuditReaderFailsClosed 验证没装审计查询能力时端点拒服务。
func TestGetRunAudit_WithoutAuditReaderFailsClosed(t *testing.T) {
	quiet := logging.New(io.Discard)
	srv := New(Options{
		Version:     "0.1.0",
		Logger:      quiet,
		IngestToken: testIngestToken,
		Credentials: testCredentials(),
	})

	rec := doGetAudit(t, srv, "run-anything", testOperatorToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("HTTP 状态 = %d，期望 404", rec.Code)
	}
}

// TestGetRunAudit_NeverLeaksCredentialsOrPrompt 验证审计响应里没有凭据与提示词原文。
func TestGetRunAudit_NeverLeaksCredentialsOrPrompt(t *testing.T) {
	srv, st := newAuditTestServer(t)
	runID := seedAuditChain(t, st, "院内判定模型", true)

	rec := doGetAudit(t, srv, runID, testOperatorToken)
	body := rec.Body.String()
	for _, secret := range []string{"sk-hospital-secret-0123456789", "point-vax-freezer-01"} {
		if strings.Contains(body, secret) {
			t.Errorf("审计响应里出现了敏感内容 %q：%s", secret, body)
		}
	}
}

// TestGetRunAudit_EndToEndThroughRealGateway 端到端验证：真实 OpenAI-compatible
// 协议的一次模型调用，其审计元数据能从审计端点原样取回。
//
// 走的是真链路：真网关（真协议、真 Bearer 头）→ 真事实校验 → 真审计记录器
// → 真 SQLite 审计表 → 真 HTTP 审计端点。
// 端点一侧是协议替身（测试数据层），不是产品对接面。
func TestGetRunAudit_EndToEndThroughRealGateway(t *testing.T) {
	const apiKey = "sk-hospital-secret-0123456789"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+apiKey {
			t.Errorf("上游收到的鉴权头 = %q，期望 Bearer %s", got, apiKey)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-audit-001",
			"object": "chat.completion",
			"model": "院内判定模型",
			"choices": [{"index": 0, "message": {"role": "assistant",
				"content": "{\"cause_code\":\"door_open\",\"confidence\":0.9}"},
				"finish_reason": "stop"}],
			"usage": {"prompt_tokens": 900, "completion_tokens": 120, "total_tokens": 1020}
		}`))
	}))
	defer upstream.Close()

	srv, st := newAuditTestServer(t)
	runID := seedRun(t, st)

	gateway, err := model.NewGateway(model.GatewayConfig{
		BaseURL: upstream.URL,
		APIKey:  apiKey,
		Model:   "院内判定模型",
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("装配模型网关失败：%v", err)
	}

	recorder := audit.NewRecorder(st,
		audit.WithPricer(audit.NewPriceTable(map[string]audit.Price{
			"院内判定模型": {InputCostPerToken: 0.0000012, OutputCostPerToken: 0.0000024},
		})),
		audit.WithSecrets(apiKey),
	)

	asker := &model.Asker{
		Caller:   model.GatewayAsker(gateway),
		RunID:    runID,
		StepName: "attribute",
		Basis:    model.FactBasis{PointID: "point-vax-freezer-01"},
		Auditor:  recorder,
	}

	_, result, err := asker.Ask(context.Background(), model.SchemaAttribution, nil)
	if err != nil {
		t.Fatalf("模型调用失败：%v", err)
	}
	if result.Attempts != 1 {
		t.Fatalf("调用次数 = %d，期望 1", result.Attempts)
	}

	// 审计端点取回这一次调用的元数据。
	rec := doGetAudit(t, srv, runID, testOperatorToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP 状态 = %d，期望 200（响应体：%s）", rec.Code, rec.Body.String())
	}
	out := decodeAudit(t, rec)
	if out.ModelCallCount != 1 {
		t.Fatalf("model_call_count = %d，期望 1", out.ModelCallCount)
	}
	entry := out.Entries[0]
	if entry.Model != "院内判定模型" {
		t.Errorf("model = %q，期望 院内判定模型", entry.Model)
	}
	if entry.PromptTokens != 900 || entry.CompletionTokens != 120 || entry.TotalTokens != 1020 {
		t.Errorf("token 字段 = %d/%d/%d，期望 900/120/1020",
			entry.PromptTokens, entry.CompletionTokens, entry.TotalTokens)
	}
	if entry.Outcome != domain.AuditOutcomeSucceeded {
		t.Errorf("outcome = %q，期望 %q", entry.Outcome, domain.AuditOutcomeSucceeded)
	}
	if entry.SchemaName != string(model.SchemaAttribution) {
		t.Errorf("schema_name = %q，期望 %q", entry.SchemaName, model.SchemaAttribution)
	}
	if entry.CostStatus != domain.CostStatusKnown {
		t.Errorf("cost_status = %q，期望 %q", entry.CostStatus, domain.CostStatusKnown)
	}
	if entry.PromptDigest == "" {
		t.Error("prompt_digest 为空：审计链要能回答「是不是同一段提示词」")
	}
	if entry.PromptChars <= 0 {
		t.Errorf("prompt_chars = %d，期望大于 0", entry.PromptChars)
	}
	if strings.Contains(rec.Body.String(), apiKey) {
		t.Errorf("审计响应里出现了 api_key：%s", rec.Body.String())
	}
}
