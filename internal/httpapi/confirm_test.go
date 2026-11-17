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

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
	"github.com/chaosforge/coldchain-temp-assessment/internal/runtime"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// 本文件验证确认 / 否决两个端点的 HTTP 层行为：
// 凭据、角色、请求体、响应形状，以及「确认本身不出站」这条纪律。
//
// 三条底线：
//   - 凭据不对进不来（401），身份不对做不了（403），经办永远不能替主管签；
//   - 请求体不合法一律 400 并说清错在哪，不半猜半放行；
//   - 确认只推进状态、固化载荷、记账，不碰冷链台账。

// stubConfirmer 是确认编排层的测试替身：记录收到的请求，按预设返回结果。
type stubConfirmer struct {
	confirmCalls []workflow.ConfirmRequest
	rejectCalls  []workflow.ConfirmRequest
	outcome      runtime.ConfirmationOutcome
	err          error
}

func (s *stubConfirmer) Confirm(_ context.Context, runID string, req workflow.ConfirmRequest) (runtime.ConfirmationOutcome, error) {
	req.RunID = runID
	s.confirmCalls = append(s.confirmCalls, req)
	return s.outcome, s.err
}

func (s *stubConfirmer) Reject(_ context.Context, runID string, req workflow.ConfirmRequest) (runtime.ConfirmationOutcome, error) {
	req.RunID = runID
	s.rejectCalls = append(s.rejectCalls, req)
	return s.outcome, s.err
}

// confirmServer 造一台挂了确认 / 否决端点的 Server。
func confirmServer(t *testing.T, confirmer RunConfirmer, creds []Credential) *Server {
	t.Helper()
	srv := New(Options{
		Version:     "0.1.0",
		Logger:      logging.New(io.Discard),
		Confirmer:   confirmer,
		Credentials: creds,
	})
	return srv
}

// supervisorCreds 造一套凭据表：经办、主管、只读各一把。
func supervisorCreds() []Credential {
	return []Credential{
		{Role: workflow.RoleOperator, Token: testOperatorToken},
		{Role: workflow.RoleSupervisor, Token: testSupervisorToken},
		{Role: workflow.RoleAuditor, Token: testAuditorToken},
	}
}

// confirmedOutcome 造一份确认成功的回执。
func confirmedOutcome() runtime.ConfirmationOutcome {
	at := time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)
	return runtime.ConfirmationOutcome{
		Decision: workflow.ConfirmResult{
			RunID:            "run-test-0001",
			FromState:        workflow.StateAwaitingConfirmation,
			ToState:          workflow.StateConfirmed,
			Disposition:      "isolate",
			DispositionLabel: "隔离",
			ConfirmedBy:      "supervisor-007",
			ConfirmedAt:      at,
			Opinion:          "已联系疫苗生产企业评估，先隔离",
		},
		ExternalRef:    "cc-point-vax-freezer-01-20261019T191000Z",
		IdempotencyKey: "54b35c7f136371619ea6a5313714d1ac3d4173e3f835bcc97eb1087e6f7dddae",
		WritebackState: "pending",
		PayloadFrozen:  true,
	}
}

// postConfirm 发一次确认 / 否决请求。
func postConfirm(t *testing.T, srv *Server, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// TestConfirmRun_SupervisorAccepted 覆盖主路径：
// 主管凭据确认一条待确认判定，回执带状态、处置、确认人与写回状态。
func TestConfirmRun_SupervisorAccepted(t *testing.T) {
	stub := &stubConfirmer{outcome: confirmedOutcome()}
	srv := confirmServer(t, stub, supervisorCreds())

	rec := postConfirm(t, srv, "/v1/runs/run-test-0001/confirm", testSupervisorToken,
		`{"actor":"supervisor-007","disposition":"isolate","opinion":"先隔离，送企业评估"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%s", rec.Code, rec.Body.String())
	}

	var body confirmResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析回执失败：%v（原文：%s）", err, rec.Body.String())
	}
	if body.RunID != "run-test-0001" {
		t.Errorf("回执的 run_id 不对：%q", body.RunID)
	}
	if body.PreviousState != string(workflow.StateAwaitingConfirmation) {
		t.Errorf("回执的确认前状态不对：%q", body.PreviousState)
	}
	if body.State != string(workflow.StateConfirmed) {
		t.Errorf("回执的确认后状态不对：%q", body.State)
	}
	if body.Disposition != "isolate" || body.DispositionLabel != "隔离" {
		t.Errorf("回执的处置结论不对：%q / %q", body.Disposition, body.DispositionLabel)
	}
	if body.ConfirmedBy != "supervisor-007" {
		t.Errorf("回执的确认人不对：%q", body.ConfirmedBy)
	}
	if body.ExternalRef != "cc-point-vax-freezer-01-20261019T191000Z" {
		t.Errorf("回执的外部引用不对：%q", body.ExternalRef)
	}
	if body.IdempotencyKey == "" {
		t.Error("回执应带幂等键")
	}
	if body.WritebackState != "pending" {
		t.Errorf("回执的写回状态不对：%q", body.WritebackState)
	}
	if !body.PayloadFrozen {
		t.Error("回执应标明写回载荷已固化")
	}

	// 编排层拿到的请求：处置、确认人、意见、身份都传到了。
	if len(stub.confirmCalls) != 1 {
		t.Fatalf("确认编排应当被调用 1 次，实际 %d", len(stub.confirmCalls))
	}
	call := stub.confirmCalls[0]
	if call.Role != workflow.RoleSupervisor {
		t.Errorf("编排层拿到的身份不对：%q", call.Role)
	}
	if call.Actor != "supervisor-007" {
		t.Errorf("编排层拿到的确认人不对：%q", call.Actor)
	}
	if call.Disposition != "isolate" {
		t.Errorf("编排层拿到的处置结论不对：%q", call.Disposition)
	}
	if call.Opinion != "先隔离，送企业评估" {
		t.Errorf("编排层拿到的意见不对：%q", call.Opinion)
	}
}

// TestConfirmRun_OperatorRejected 覆盖越权：经办身份确认被拒（403）。
func TestConfirmRun_OperatorRejected(t *testing.T) {
	stub := &stubConfirmer{outcome: confirmedOutcome()}
	srv := confirmServer(t, stub, supervisorCreds())

	rec := postConfirm(t, srv, "/v1/runs/run-test-0001/confirm", testOperatorToken,
		`{"actor":"operator-001","disposition":"isolate"}`)
	assertRoleRejected(t, rec)
	if len(stub.confirmCalls) != 0 {
		t.Error("越权请求不得进入确认编排")
	}
}

// TestConfirmRun_AuditorRejected 覆盖只读身份：质控 / 院感只能调阅，不能确认。
func TestConfirmRun_AuditorRejected(t *testing.T) {
	stub := &stubConfirmer{outcome: confirmedOutcome()}
	srv := confirmServer(t, stub, supervisorCreds())

	rec := postConfirm(t, srv, "/v1/runs/run-test-0001/confirm", testAuditorToken,
		`{"actor":"auditor-001","disposition":"isolate"}`)
	assertRoleRejected(t, rec)
}

// TestConfirmRun_MissingTokenRejected 覆盖没带凭据（401）。
func TestConfirmRun_MissingTokenRejected(t *testing.T) {
	stub := &stubConfirmer{outcome: confirmedOutcome()}
	srv := confirmServer(t, stub, supervisorCreds())

	rec := postConfirm(t, srv, "/v1/runs/run-test-0001/confirm", "",
		`{"actor":"supervisor-007","disposition":"isolate"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if len(stub.confirmCalls) != 0 {
		t.Error("没带凭据的请求不得进入确认编排")
	}
}

// TestConfirmRun_BadRequestBodyRejected 覆盖请求体不合法：
// 空体、坏 JSON、尾部多出内容，一律 400。
func TestConfirmRun_BadRequestBodyRejected(t *testing.T) {
	stub := &stubConfirmer{outcome: confirmedOutcome()}
	srv := confirmServer(t, stub, supervisorCreds())

	cases := []struct {
		name string
		body string
	}{
		{"空请求体", ""},
		{"不是 JSON", "actor=supervisor-007"},
		{"尾部多出内容", `{"actor":"supervisor-007","disposition":"isolate"}{"actor":"other"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postConfirm(t, srv, "/v1/runs/run-test-0001/confirm", testSupervisorToken, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("期望 400，实际 %d：%s", rec.Code, rec.Body.String())
			}
			var body struct {
				ErrorCode string `json:"error_code"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("解析错误响应失败：%v", err)
			}
			if body.ErrorCode != string(errcode.Validation) {
				t.Errorf("错误码不对：%s", body.ErrorCode)
			}
		})
	}
	if len(stub.confirmCalls) != 0 {
		t.Error("请求体不合法时不得进入确认编排")
	}
}

// TestConfirmRun_NotFoundPropagated 覆盖「查不到这条 run」：
// 编排层报 E_NOT_FOUND，端点原样回 404，不回一条空回执。
func TestConfirmRun_NotFoundPropagated(t *testing.T) {
	stub := &stubConfirmer{err: errcode.Newf(errcode.NotFound, "判定 run 不存在：run-missing")}
	srv := confirmServer(t, stub, supervisorCreds())

	rec := postConfirm(t, srv, "/v1/runs/run-missing/confirm", testSupervisorToken,
		`{"actor":"supervisor-007","disposition":"isolate"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("期望 404，实际 %d：%s", rec.Code, rec.Body.String())
	}
}

// TestRejectRun_SupervisorAccepted 覆盖否决端点：状态落到已否决，不带处置结论。
func TestRejectRun_SupervisorAccepted(t *testing.T) {
	outcome := confirmedOutcome()
	outcome.Decision.ToState = workflow.StateRejected
	outcome.Decision.Disposition = ""
	outcome.Decision.DispositionLabel = ""
	outcome.PayloadFrozen = false
	stub := &stubConfirmer{outcome: outcome}
	srv := confirmServer(t, stub, supervisorCreds())

	rec := postConfirm(t, srv, "/v1/runs/run-test-0001/reject", testSupervisorToken,
		`{"actor":"supervisor-007","opinion":"本次读数疑似探头故障，不算越温"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var body confirmResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析回执失败：%v", err)
	}
	if body.State != string(workflow.StateRejected) {
		t.Errorf("回执的否决后状态不对：%q", body.State)
	}
	if body.Disposition != "" {
		t.Errorf("否决的回执不应带处置结论：%q", body.Disposition)
	}
	if body.PayloadFrozen {
		t.Error("否决不应固化写回载荷")
	}
	if len(stub.rejectCalls) != 1 {
		t.Fatalf("否决编排应当被调用 1 次，实际 %d", len(stub.rejectCalls))
	}
	if stub.rejectCalls[0].Opinion != "本次读数疑似探头故障，不算越温" {
		t.Errorf("否决编排拿到的意见不对：%q", stub.rejectCalls[0].Opinion)
	}
}

// TestConfirmRun_WithoutConfirmerFailsClosed 覆盖没装确认能力的情形：
// 服务起得来但确认能力没装配时，端点如实报出来，不装作能确认。
func TestConfirmRun_WithoutConfirmerFailsClosed(t *testing.T) {
	srv := confirmServer(t, nil, supervisorCreds())
	rec := postConfirm(t, srv, "/v1/runs/run-test-0001/confirm", testSupervisorToken,
		`{"actor":"supervisor-007","disposition":"isolate"}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("未装配确认能力时不得回成功：%s", rec.Body.String())
	}
}

// TestRoutes_ConfirmEndpointsDeclareConfirmPermission 覆盖路由表：
// 两个端点都声明了「确认或否决处置结论」这项权限——
// 没声明权限的端点按默认拒绝处理，声明错了就等于把确认权开给了所有人。
func TestRoutes_ConfirmEndpointsDeclareConfirmPermission(t *testing.T) {
	srv := confirmServer(t, &stubConfirmer{}, supervisorCreds())
	found := map[string]bool{}
	for _, route := range srv.Routes() {
		switch route.Path {
		case "/v1/runs/{id}/confirm", "/v1/runs/{id}/reject":
			if route.Method != http.MethodPost {
				t.Errorf("端点 %s 的方法不对：%q", route.Path, route.Method)
			}
			if route.Permission != workflow.PermConfirm {
				t.Errorf("端点 %s 声明的权限不对：%q", route.Path, route.Permission)
			}
			found[route.Path] = true
		}
	}
	if !found["/v1/runs/{id}/confirm"] || !found["/v1/runs/{id}/reject"] {
		t.Errorf("确认 / 否决端点未注册：%+v", srv.Routes())
	}
}
