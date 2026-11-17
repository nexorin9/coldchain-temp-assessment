package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// 本文件验证 HTTP 层的角色闸门：凭据证明「你进来了」，
// 角色闸门证明「这件事归你管」——两道闸都过了，端点才真正执行。
//
// 三条底线：
//   - 越权一律 403 E_ROLE_REQUIRED，不给「差一点就行」的提示；
//   - 身份缺失按最保守处理：宁可端点不可用，也不默认放行；
//   - 角色声明与凭据身份对不上时拒绝：矛盾本身就是可疑信号。

// roleGateServer 造一台只挂了一个测试端点的 Server。
//
// 端点声明「确认或否决处置结论」这项权限——它是权限矩阵里最重的一项，
// 只有药学部主管持有。用它来验闸门最不容易含糊。
func roleGateServer(t *testing.T, creds []Credential) *Server {
	t.Helper()
	quiet := logging.New(io.Discard)
	srv := &Server{mux: http.NewServeMux(), logger: quiet, creds: creds}
	srv.protect(workflow.PermConfirm, "POST", "/v1/runs/x/confirm", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	return srv
}

// roleGateReadServer 造一台挂了一个只读端点（查看判定结论）的 Server。
func roleGateReadServer(t *testing.T, creds []Credential) *Server {
	t.Helper()
	quiet := logging.New(io.Discard)
	srv := &Server{mux: http.NewServeMux(), logger: quiet, creds: creds}
	srv.protect(workflow.PermRunRead, "GET", "/v1/runs/x", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	return srv
}

// callWithToken 发一次带凭据（可选带角色声明）的请求。
func callWithToken(t *testing.T, srv *Server, method, path, token, declaredRole string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if declaredRole != "" {
		req.Header.Set(RoleDeclHeader, declaredRole)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// assertRoleRejected 断言响应是 403 且错误码为 E_ROLE_REQUIRED。
func assertRoleRejected(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusForbidden {
		t.Fatalf("期望 403，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var body struct {
		ErrorCode string `json:"error_code"`
		Message   string `json:"message"`
		Details   string `json:"details"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析错误响应失败：%v（原文：%s）", err, rec.Body.String())
	}
	if body.ErrorCode != string(errcode.RoleRequired) {
		t.Fatalf("期望错误码 %s，实际：%s", errcode.RoleRequired, body.ErrorCode)
	}
	if !strings.Contains(body.Details, "无权") && !strings.Contains(body.Details, "拒绝") {
		t.Fatalf("错误详情应说明是无权或拒绝，实际：%s", body.Details)
	}
}

// TestRoleGate_OperatorCannotConfirm 验证经办不能确认处置结论（403）。
func TestRoleGate_OperatorCannotConfirm(t *testing.T) {
	srv := roleGateServer(t, []Credential{
		{Role: workflow.RoleOperator, Token: testOperatorToken},
	})

	rec := callWithToken(t, srv, http.MethodPost, "/v1/runs/x/confirm", testOperatorToken, "")
	assertRoleRejected(t, rec)
	if !strings.Contains(rec.Body.String(), "药学部主管（审核）") {
		t.Fatalf("错误信息应指出该找谁签（药学部主管），实际：%s", rec.Body.String())
	}
}

// TestRoleGate_SupervisorCanConfirm 验证主管能确认处置结论（200）。
func TestRoleGate_SupervisorCanConfirm(t *testing.T) {
	srv := roleGateServer(t, []Credential{
		{Role: workflow.RoleSupervisor, Token: testSupervisorToken},
	})

	rec := callWithToken(t, srv, http.MethodPost, "/v1/runs/x/confirm", testSupervisorToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("主管确认应放行，实际 %d：%s", rec.Code, rec.Body.String())
	}
}

// TestRoleGate_IngestCannotReadLedger 验证投递通道读不了台账（403）。
func TestRoleGate_IngestCannotReadLedger(t *testing.T) {
	srv := roleGateServer(t, []Credential{
		{Role: workflow.RoleIngest, Token: testIngestToken},
	})

	rec := callWithToken(t, srv, http.MethodPost, "/v1/runs/x/confirm", testIngestToken, "")
	assertRoleRejected(t, rec)
}

// TestRoleGate_AuditorReadOnlyPasses 验证只读调阅岗读得到、动不了。
func TestRoleGate_AuditorReadOnlyPasses(t *testing.T) {
	srv := roleGateServer(t, []Credential{
		{Role: workflow.RoleAuditor, Token: testAuditorToken},
	})

	// 只读端点（查看判定结论）放行。
	readable := roleGateReadServer(t, []Credential{
		{Role: workflow.RoleAuditor, Token: testAuditorToken},
	})
	rec := callWithToken(t, readable, http.MethodGet, "/v1/runs/x", testAuditorToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("只读身份查看判定结论应放行，实际 %d：%s", rec.Code, rec.Body.String())
	}

	// 但确认这种写动作一律拒绝。
	rec = callWithToken(t, srv, http.MethodPost, "/v1/runs/x/confirm", testAuditorToken, "")
	assertRoleRejected(t, rec)
}

// TestRoleGate_MissingRoleIsDenied 验证身份缺失按最保守处理（403）。
//
// 凭据能命中、但这把凭据没登记角色——服务端就不知道对面是谁，
// 这时不能替它挑一个身份放行。
func TestRoleGate_MissingRoleIsDenied(t *testing.T) {
	srv := roleGateServer(t, []Credential{
		{Role: workflow.RoleUnknown, Token: "token-without-role"},
	})

	rec := callWithToken(t, srv, http.MethodPost, "/v1/runs/x/confirm", "token-without-role", "")
	assertRoleRejected(t, rec)
	if !strings.Contains(rec.Body.String(), "最保守") {
		t.Fatalf("错误信息应说明按最保守处理，实际：%s", rec.Body.String())
	}
}

// TestRoleGate_DeclaredRoleMustMatchCredential 验证角色声明与凭据身份必须一致。
func TestRoleGate_DeclaredRoleMustMatchCredential(t *testing.T) {
	srv := roleGateServer(t, []Credential{
		{Role: workflow.RoleOperator, Token: testOperatorToken},
		{Role: workflow.RoleSupervisor, Token: testSupervisorToken},
	})

	// 拿主管的凭据自称主管：一致，放行。
	rec := callWithToken(t, srv, http.MethodPost, "/v1/runs/x/confirm", testSupervisorToken, "supervisor")
	if rec.Code != http.StatusOK {
		t.Fatalf("声明与凭据一致时应放行，实际 %d：%s", rec.Code, rec.Body.String())
	}

	// 拿经办的凭据自称主管：矛盾，拒绝。
	rec = callWithToken(t, srv, http.MethodPost, "/v1/runs/x/confirm", testOperatorToken, "supervisor")
	assertRoleRejected(t, rec)
	if !strings.Contains(rec.Body.String(), "不一致") {
		t.Fatalf("错误信息应说明声明与凭据不一致，实际：%s", rec.Body.String())
	}

	// 自称一个没登记过的身份：拒绝。
	rec = callWithToken(t, srv, http.MethodPost, "/v1/runs/x/confirm", testOperatorToken, "root")
	assertRoleRejected(t, rec)
	if !strings.Contains(rec.Body.String(), "未登记") {
		t.Fatalf("错误信息应说明该身份未登记，实际：%s", rec.Body.String())
	}
}

// TestGetRun_IngestTokenCannotReadConclusion 验证投递通道看不到判定结论。
//
// 真实端点上的表现：记录仪那把凭据能投递温度事件，
// 但拿它去查判定结论会被挡下——它不需要看，也无权看。
func TestGetRun_IngestTokenCannotReadConclusion(t *testing.T) {
	srv, st := newRunTestServer(t)
	runID := seedRun(t, st)

	rec := doGetRun(t, srv, runID, testIngestToken)
	assertRoleRejected(t, rec)
	if !strings.Contains(rec.Body.String(), "查看判定结论") {
		t.Fatalf("错误信息应点名缺失的权限，实际：%s", rec.Body.String())
	}
}

// TestGetRun_OperatorAndAuditorCanRead 验证经办与只读岗都能查判定结论。
func TestGetRun_OperatorAndAuditorCanRead(t *testing.T) {
	srv, st := newRunTestServer(t)
	runID := seedRun(t, st)

	for _, token := range []string{testOperatorToken, testSupervisorToken, testAuditorToken} {
		rec := doGetRun(t, srv, runID, token)
		if rec.Code != http.StatusOK {
			t.Fatalf("持 %s 的身份应能查看判定结论，实际 %d：%s", token, rec.Code, rec.Body.String())
		}
	}
}

// TestGetRunAudit_OnlyReadRolesPass 验证审计链只对只读类身份开放。
func TestGetRunAudit_OnlyReadRolesPass(t *testing.T) {
	srv, st := newAuditTestServer(t)
	runID := seedAuditChain(t, st, "院内判定模型", true)

	for _, token := range []string{testOperatorToken, testSupervisorToken, testAuditorToken} {
		rec := doGetAudit(t, srv, runID, token)
		if rec.Code != http.StatusOK {
			t.Fatalf("持 %s 的身份应能调阅审计链，实际 %d：%s", token, rec.Code, rec.Body.String())
		}
	}

	rec := doGetAudit(t, srv, runID, testIngestToken)
	assertRoleRejected(t, rec)
}

// TestIngestEvent_OperatorMayAlsoDeliver 验证经办也能补投温度事件。
//
// 手工上传一条记录仪读数是经办岗的日常动作，
// 所以经办持有「投递温度事件」这项权限。
func TestIngestEvent_OperatorMayAlsoDeliver(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := postEvent(t, srv, testOperatorToken, sampleEventBody(t))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("经办补投事件应被接受，实际 %d：%s", rec.Code, rec.Body.String())
	}
}

// TestIngestEvent_AuditorCannotDeliver 验证只读岗不能往库里塞读数。
func TestIngestEvent_AuditorCannotDeliver(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := postEvent(t, srv, testAuditorToken, sampleEventBody(t))
	assertRoleRejected(t, rec)
}

// TestRoutes_EveryProtectedRouteDeclaresPermission 验证每个端点都声明了权限要求。
//
// 缺失声明的端点按默认拒绝处理——所以声明不能漏。
// 免鉴权端点只有健康检查与版本两个，它们不碰业务数据。
func TestRoutes_EveryProtectedRouteDeclaresPermission(t *testing.T) {
	srv, _ := newTestServer(t)

	public := map[string]bool{
		"GET /healthz":    true,
		"GET /v1/version": true,
	}
	for _, route := range srv.Routes() {
		key := route.Method + " " + route.Path
		if public[key] {
			if !route.Public {
				t.Fatalf("%s 应标记为免鉴权端点", key)
			}
			continue
		}
		if route.Public {
			t.Fatalf("%s 不应标记为免鉴权端点", key)
		}
		if !workflow.KnownPermission(route.Permission) {
			t.Fatalf("%s 未声明有效权限（%q）", key, route.Permission)
		}
	}
}

// TestEffectiveCredentials_IngestTokenKeepsItsOwnRole 验证入站凭据固定对应投递身份。
//
// 凭据表里若已登记同一身份的令牌，以凭据表为准，不重复登记——
// 同一把令牌挂两个身份会让「你是谁」变得不确定。
func TestEffectiveCredentials_IngestTokenKeepsItsOwnRole(t *testing.T) {
	creds := effectiveCredentials(Options{
		IngestToken: "ingest-token",
		Credentials: []Credential{
			{Role: workflow.RoleOperator, Token: "operator-token"},
			{Role: workflow.RoleIngest, Token: "ingest-token-from-table"},
			{Role: workflow.RoleSupervisor, Token: ""},
		},
	})

	seen := map[workflow.Role]string{}
	for _, cred := range creds {
		if prev, ok := seen[cred.Role]; ok {
			t.Fatalf("身份 %q 登记了两把凭据（%q 与 %q）", cred.Role, prev, cred.Token)
		}
		seen[cred.Role] = cred.Token
	}
	if seen[workflow.RoleIngest] != "ingest-token-from-table" {
		t.Fatalf("凭据表里已登记的投递令牌应优先，实际：%q", seen[workflow.RoleIngest])
	}
	if seen[workflow.RoleOperator] != "operator-token" {
		t.Fatalf("经办凭据应保留，实际：%q", seen[workflow.RoleOperator])
	}
	if _, ok := seen[workflow.RoleSupervisor]; ok {
		t.Fatal("空凭据不应登记")
	}
}
