package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/ingest"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
	"github.com/chaosforge/coldchain-temp-assessment/internal/store"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// 测试用的四类身份凭据。
//
// 只存在于测试进程内，不写进任何文件、不写进任何日志。
const (
	// testIngestToken 是投递通道（记录仪 / 边缘网关）的凭据。
	testIngestToken = "test-ingest-token-3f9c"
	// testOperatorToken 是药学部冷链管理员（经办）的凭据。
	testOperatorToken = "test-operator-token-7c2a"
	// testSupervisorToken 是药学部主管（审核）的凭据。
	testSupervisorToken = "test-supervisor-token-b41e"
	// testAuditorToken 是质控 / 院感（只读调阅）的凭据。
	testAuditorToken = "test-auditor-token-5d8f"
)

// testCredentials 返回测试用的角色凭据表（一把令牌一个身份）。
func testCredentials() []Credential {
	return []Credential{
		{Role: workflow.RoleOperator, Token: testOperatorToken},
		{Role: workflow.RoleSupervisor, Token: testSupervisorToken},
		{Role: workflow.RoleAuditor, Token: testAuditorToken},
	}
}

// newTestServer 造一台指向内存库的 Server。
//
// 用的是真的存储层（SQLite 内存库）、真的接入器与真的限流闸门，
// 只把凭据与落盘位置换掉：断言「重复投递只落一行」这类行为时，
// 拿测试替身验证没有意义。
func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	return newTestServerWithLimiter(t, ingest.NewRateLimiter(1000, time.Minute, 1000))
}

// newTestServerWithLimiter 造一台带指定限流闸门的 Server。
func newTestServerWithLimiter(t *testing.T, limiter *ingest.RateLimiter) (*Server, *store.Store) {
	t.Helper()

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	quiet := logging.New(io.Discard)
	ingestor := ingest.NewIngestor(st, limiter, quiet)
	srv := New(Options{
		Version:     "0.1.0",
		Logger:      quiet,
		IngestToken: testIngestToken,
		Credentials: testCredentials(),
		Ingestor:    ingestor,
		Importer:    ingestor,
	})
	return srv, st
}

// sampleEventBody 构造一条合法的温度事件请求体。
func sampleEventBody(t *testing.T) []byte {
	t.Helper()
	body := map[string]any{
		"point_id":      "point-vax-freezer-01",
		"device_id":     "device-recorder-0001",
		"device_name":   "疫苗冷藏库温度记录仪",
		"measured_at":   "2026-10-20T03:15:00+08:00",
		"temperature_c": 11.4,
		"unit":          "C",
		"humidity_pct":  62.5,
		"battery_pct":   91,
		"accuracy_c":    0.3,
		"source":        "recorder",
		"alarm_flag":    true,
		"events":        []string{"door_open"},
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("构造测试事件失败：%v", err)
	}
	return data
}

// postEvent 向入站端点投递一次。
//
// token 为空表示不带 Authorization 头。
func postEvent(t *testing.T, srv *Server, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/events/coldchain-temperature", bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// decodeErrorBody 解析错误响应体。
func decodeErrorBody(t *testing.T, rec *httptest.ResponseRecorder) errcode.Error {
	t.Helper()
	var e errcode.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("错误响应体不是合法 JSON：%v（原文：%s）", err, rec.Body.String())
	}
	return e
}

// decodeAcceptedBody 解析受理回执。
func decodeAcceptedBody(t *testing.T, rec *httptest.ResponseRecorder) eventAcceptedResponse {
	t.Helper()
	var resp eventAcceptedResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("回执不是合法 JSON：%v（原文：%s）", err, rec.Body.String())
	}
	return resp
}

// assertNoCredentialLeak 断言响应里没有任何与凭据相关的内容。
func assertNoCredentialLeak(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if strings.Contains(rec.Body.String(), testIngestToken) {
		t.Fatalf("响应体回显了入站凭据：%s", rec.Body.String())
	}
	for name, values := range rec.Header() {
		for _, v := range values {
			if strings.Contains(v, testIngestToken) {
				t.Fatalf("响应头 %s 回显了入站凭据：%s", name, v)
			}
		}
	}
}

// TestIngestEvent_MissingBearerTokenReturns401 未带凭据 → 401。
func TestIngestEvent_MissingBearerTokenReturns401(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := postEvent(t, srv, "", sampleEventBody(t))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401，实际 %d（响应体：%s）", rec.Code, rec.Body.String())
	}
	e := decodeErrorBody(t, rec)
	if e.Code != errcode.Unauthorized {
		t.Fatalf("期望错误码 %s，实际 %s", errcode.Unauthorized, e.Code)
	}
	assertNoCredentialLeak(t, rec)
}

// TestIngestEvent_WrongBearerTokenReturns401 凭据不对 → 401。
func TestIngestEvent_WrongBearerTokenReturns401(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := postEvent(t, srv, "wrong-token", sampleEventBody(t))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401，实际 %d（响应体：%s）", rec.Code, rec.Body.String())
	}
	e := decodeErrorBody(t, rec)
	if e.Code != errcode.Unauthorized {
		t.Fatalf("期望错误码 %s，实际 %s", errcode.Unauthorized, e.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("401 响应缺少 WWW-Authenticate 头")
	}
	assertNoCredentialLeak(t, rec)
}

// TestIngestEvent_MalformedAuthorizationHeaderReturns401 头部格式不对 → 401。
func TestIngestEvent_MalformedAuthorizationHeaderReturns401(t *testing.T) {
	srv, _ := newTestServer(t)

	for _, header := range []string{
		"Basic dXNlcjpwYXNz",
		"Bearer",
		"Bearer    ",
		testIngestToken,
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/events/coldchain-temperature", bytes.NewReader(sampleEventBody(t)))
		req.Header.Set("Authorization", header)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("头部 %q 期望 401，实际 %d（响应体：%s）", header, rec.Code, rec.Body.String())
		}
	}
}

// TestIngestEvent_ValidEventAcceptedWithEventID 合法事件 → 202 且回执带 event_id。
func TestIngestEvent_ValidEventAcceptedWithEventID(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := postEvent(t, srv, testIngestToken, sampleEventBody(t))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("期望 202，实际 %d（响应体：%s）", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("响应 Content-Type 不是 JSON：%s", ct)
	}
	resp := decodeAcceptedBody(t, rec)
	if resp.EventID == "" {
		t.Fatalf("回执缺少 event_id：%s", rec.Body.String())
	}
	if resp.Duplicate {
		t.Fatalf("首次投递不应标记为重复：%s", rec.Body.String())
	}
	if resp.PointID != "point-vax-freezer-01" {
		t.Fatalf("回执未回带 point_id：%s", rec.Body.String())
	}
	if resp.MeasuredAt != "2026-10-20T03:15:00+08:00" {
		t.Fatalf("回执未保留原始时区偏移：%s", resp.MeasuredAt)
	}
	// 判定 run 由调度器建立，此刻还没有：显式给 null，不编一个号出来。
	if resp.RunID != nil {
		t.Fatalf("判定 run 尚未建立，run_id 应为 null，实际 %q", *resp.RunID)
	}
	if resp.RunState != runStatePending {
		t.Fatalf("期望 run_state=%s，实际 %s", runStatePending, resp.RunState)
	}
	if resp.Note == "" {
		t.Fatalf("回执缺少说明文字")
	}
}

// TestIngestEvent_DuplicateDeliveryKeepsOneRow 重复投递 → 202 且库内仍只有一行。
func TestIngestEvent_DuplicateDeliveryKeepsOneRow(t *testing.T) {
	srv, st := newTestServer(t)
	body := sampleEventBody(t)

	first := postEvent(t, srv, testIngestToken, body)
	if first.Code != http.StatusAccepted {
		t.Fatalf("首次投递期望 202，实际 %d（响应体：%s）", first.Code, first.Body.String())
	}
	firstResp := decodeAcceptedBody(t, first)

	second := postEvent(t, srv, testIngestToken, body)
	if second.Code != http.StatusAccepted {
		t.Fatalf("重复投递期望 202，实际 %d（响应体：%s）", second.Code, second.Body.String())
	}
	secondResp := decodeAcceptedBody(t, second)

	if !secondResp.Duplicate {
		t.Fatalf("重复投递应标记 duplicate=true：%s", second.Body.String())
	}
	if secondResp.EventID != firstResp.EventID {
		t.Fatalf("重复投递的 event_id 应与首次一致：%s vs %s", secondResp.EventID, firstResp.EventID)
	}

	count, err := st.CountEvents(context.Background(), "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("统计事件条数失败：%v", err)
	}
	if count != 1 {
		t.Fatalf("重复投递后库内应只有 1 条事件，实际 %d 条", count)
	}
}

// TestIngestEvent_InvalidJSONReturns400 请求体不是 JSON → 400。
func TestIngestEvent_InvalidJSONReturns400(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := postEvent(t, srv, testIngestToken, []byte("{不是 JSON"))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d（响应体：%s）", rec.Code, rec.Body.String())
	}
	e := decodeErrorBody(t, rec)
	if e.Code != errcode.Validation {
		t.Fatalf("期望错误码 %s，实际 %s", errcode.Validation, e.Code)
	}
}

// TestIngestEvent_EmptyBodyReturns400 空请求体 → 400。
func TestIngestEvent_EmptyBodyReturns400(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := postEvent(t, srv, testIngestToken, nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d（响应体：%s）", rec.Code, rec.Body.String())
	}
	e := decodeErrorBody(t, rec)
	if !strings.Contains(e.Details, "请求体为空") {
		t.Fatalf("错误细节未点明请求体为空：%s", e.Details)
	}
}

// TestIngestEvent_TrailingContentRejected 一次投递只允许一个 JSON 对象。
func TestIngestEvent_TrailingContentRejected(t *testing.T) {
	srv, _ := newTestServer(t)

	body := append(sampleEventBody(t), []byte(`{"point_id":"第二个对象"}`)...)
	rec := postEvent(t, srv, testIngestToken, body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d（响应体：%s）", rec.Code, rec.Body.String())
	}
	e := decodeErrorBody(t, rec)
	if !strings.Contains(e.Details, "只能包含一个") {
		t.Fatalf("错误细节未点明只允许一个 JSON 对象：%s", e.Details)
	}
}

// TestIngestEvent_MissingPointIDReturns400 缺 point_id → 400。
func TestIngestEvent_MissingPointIDReturns400(t *testing.T) {
	srv, _ := newTestServer(t)

	body := map[string]any{
		"device_id":     "device-recorder-0001",
		"device_name":   "疫苗冷藏库温度记录仪",
		"measured_at":   "2026-10-20T03:15:00+08:00",
		"temperature_c": 11.4,
		"source":        "recorder",
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("构造测试事件失败：%v", err)
	}

	rec := postEvent(t, srv, testIngestToken, data)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d（响应体：%s）", rec.Code, rec.Body.String())
	}
	e := decodeErrorBody(t, rec)
	if e.Code != errcode.Validation {
		t.Fatalf("期望错误码 %s，实际 %s", errcode.Validation, e.Code)
	}
}

// TestIngestEvent_TemperatureOutOfRangeReturns422 温度越量程 → 422。
func TestIngestEvent_TemperatureOutOfRangeReturns422(t *testing.T) {
	srv, _ := newTestServer(t)

	body := map[string]any{
		"point_id":      "point-vax-freezer-01",
		"device_id":     "device-recorder-0001",
		"device_name":   "疫苗冷藏库温度记录仪",
		"measured_at":   "2026-10-20T03:15:00+08:00",
		"temperature_c": 150.0,
		"source":        "recorder",
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("构造测试事件失败：%v", err)
	}

	rec := postEvent(t, srv, testIngestToken, data)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("期望 422，实际 %d（响应体：%s）", rec.Code, rec.Body.String())
	}
	e := decodeErrorBody(t, rec)
	if e.Code != errcode.Range {
		t.Fatalf("期望错误码 %s，实际 %s", errcode.Range, e.Code)
	}
}

// TestIngestEvent_RateLimitedReturns429 超出同点限流 → 429。
func TestIngestEvent_RateLimitedReturns429(t *testing.T) {
	// 桶容量 1：第一条放行，第二条令牌耗尽 → 429。
	srv, _ := newTestServerWithLimiter(t, ingest.NewRateLimiter(1, time.Minute, 1))

	first := postEvent(t, srv, testIngestToken, sampleEventBody(t))
	if first.Code != http.StatusAccepted {
		t.Fatalf("首条投递期望 202，实际 %d（响应体：%s）", first.Code, first.Body.String())
	}

	second := postEvent(t, srv, testIngestToken, sampleEventBody(t))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("超限投递期望 429，实际 %d（响应体：%s）", second.Code, second.Body.String())
	}
	e := decodeErrorBody(t, second)
	if e.Code != errcode.RateLimited {
		t.Fatalf("期望错误码 %s，实际 %s", errcode.RateLimited, e.Code)
	}
}

// TestIngestEvent_BodyTooLargeReturns400 请求体超上限 → 400。
func TestIngestEvent_BodyTooLargeReturns400(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.maxBodyBytes = 64

	rec := postEvent(t, srv, testIngestToken, sampleEventBody(t))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d（响应体：%s）", rec.Code, rec.Body.String())
	}
	e := decodeErrorBody(t, rec)
	if !strings.Contains(e.Details, "超过上限") {
		t.Fatalf("错误细节未点明请求体超限：%s", e.Details)
	}
}

// TestIngestEvent_PrecisionLimitedSurfacedInResponse 精度不足在回执里当场可见。
func TestIngestEvent_PrecisionLimitedSurfacedInResponse(t *testing.T) {
	srv, _ := newTestServer(t)

	body := map[string]any{
		"point_id":      "point-vax-freezer-01",
		"device_id":     "device-recorder-0001",
		"device_name":   "疫苗冷藏库温度记录仪",
		"measured_at":   "2026-10-20T03:15:00+08:00",
		"temperature_c": 11.4,
		"accuracy_c":    1.2,
		"source":        "recorder",
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("构造测试事件失败：%v", err)
	}

	rec := postEvent(t, srv, testIngestToken, data)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("期望 202，实际 %d（响应体：%s）", rec.Code, rec.Body.String())
	}
	resp := decodeAcceptedBody(t, rec)
	if !resp.Precision.Limited {
		t.Fatalf("精度超限事件应在回执里标记 limited=true：%s", rec.Body.String())
	}
	if resp.Precision.Tag != ingest.PrecisionLimitedTag {
		t.Fatalf("回执里的精度标注文案不对：%q", resp.Precision.Tag)
	}
}

// TestIngestEvent_WithoutIngestorFailsClosed 未装配接入器 → 500，不假装收到。
func TestIngestEvent_WithoutIngestorFailsClosed(t *testing.T) {
	srv := New(Options{Version: "0.1.0", IngestToken: testIngestToken})

	rec := postEvent(t, srv, testIngestToken, sampleEventBody(t))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("期望 500，实际 %d（响应体：%s）", rec.Code, rec.Body.String())
	}
	e := decodeErrorBody(t, rec)
	if e.Code != errcode.StorageError {
		t.Fatalf("期望错误码 %s，实际 %s", errcode.StorageError, e.Code)
	}
}

// TestHealthzAndVersion_NoCredentialRequired 健康检查与版本端点免鉴权，且不泄漏凭据。
func TestHealthzAndVersion_NoCredentialRequired(t *testing.T) {
	srv, _ := newTestServer(t)

	for _, path := range []string{"/healthz", "/v1/version"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s 期望 200，实际 %d（响应体：%s）", path, rec.Code, rec.Body.String())
		}
		assertNoCredentialLeak(t, rec)
	}

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	var health map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatalf("健康检查响应不是合法 JSON：%v", err)
	}
	if health["status"] != "ok" || health["service"] != "coldchain-temp-assessment" {
		t.Fatalf("健康检查内容不对：%s", rec.Body.String())
	}
	if _, leaked := health["token"]; leaked {
		t.Fatalf("健康检查响应里出现了 token 字段：%s", rec.Body.String())
	}
}

// TestWrongMethodRejected 入站端点只接受 POST。
func TestWrongMethodRejected(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/events/coldchain-temperature", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("期望 405，实际 %d（响应体：%s）", rec.Code, rec.Body.String())
	}
}

// TestRoutes_ListsRegisteredEndpoints 路由表供契约脚本遍历比对。
func TestRoutes_ListsRegisteredEndpoints(t *testing.T) {
	srv, _ := newTestServer(t)

	want := map[string]bool{
		"GET /healthz":                          false,
		"GET /v1/version":                       false,
		"POST /v1/events/coldchain-temperature": false,
		"POST /v1/imports/temperature-series":   false,
	}
	for _, route := range srv.Routes() {
		key := route.Method + " " + route.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for key, found := range want {
		if !found {
			t.Fatalf("路由表缺少 %s（实际：%v）", key, srv.Routes())
		}
	}
}
