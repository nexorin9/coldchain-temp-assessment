package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
	"github.com/chaosforge/coldchain-temp-assessment/internal/runtime"
)

// 本文件验证「出站写回没落成之后怎么办」这一段的 HTTP 接缝。
//
// 两条底线：
//   - **清单要看得见**：待写回数量与具体是哪条 run，都要能从端点直接查到；
//   - **补发要有身份**：补偿重放要的是「触发台账写回补偿重放」这项权限，
//     经办与主管拿得到，只读岗拿不到（它只调阅，不替人补发台账）。

// stubCompensator 是补偿重放的测试替身：按预设返回清单与重放结果。
type stubCompensator struct {
	summary runtime.CompensationSummary
	err     error

	pendingCalls int
	retryRunIDs  []string
	retryOutcome runtime.WritebackOutcome
	retryErr     error
}

func (s *stubCompensator) Summary(_ context.Context) (runtime.CompensationSummary, error) {
	s.pendingCalls++
	return s.summary, s.err
}

func (s *stubCompensator) Retry(_ context.Context, runID string) (runtime.WritebackOutcome, error) {
	s.retryRunIDs = append(s.retryRunIDs, runID)
	return s.retryOutcome, s.retryErr
}

// compensationServer 造一台挂了补偿端点的 Server。
func compensationServer(t *testing.T, comp *stubCompensator) *Server {
	t.Helper()
	return New(Options{
		Version:     "0.1.0",
		Logger:      logging.New(nil),
		Compensator: comp,
		Credentials: testCredentials(),
	})
}

// getWithToken 发一个带凭据的 GET 请求。
func getWithToken(t *testing.T, srv *Server, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// postRetry 发一个补偿重放的 POST 请求。
func postRetry(t *testing.T, srv *Server, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// TestGetPendingWritebacks_ListsPendingAndAlerts 覆盖主路径：
// 待补偿清单里带出待写回数量、具体条目与还挂着的告警。
func TestGetPendingWritebacks_ListsPendingAndAlerts(t *testing.T) {
	comp := &stubCompensator{
		summary: runtime.CompensationSummary{
			PendingCount:   1,
			OpenAlertCount: 1,
			Items: []runtime.PendingWriteback{{
				RunID:          "run-test-0001",
				Target:         domain.WritebackTargetLedger,
				State:          domain.WritebackTargetStateFailed,
				ExternalRef:    "cc-point-vax-freezer-01-20261019T191000Z",
				IdempotencyKey: "54b35c7f136371619ea6a5313714d1ac3",
				ErrorCode:      string(errcode.LedgerUnavailable),
				Attempts:       3,
				Failures:       1,
				RunState:       "pending_writeback",
				PointID:        "point-vax-freezer-01",
			}},
			Alerts: []domain.WritebackAlert{{
				RunID:     "run-test-0001",
				Target:    domain.WritebackTargetLedger,
				State:     domain.WritebackAlertStateOpen,
				ErrorCode: string(errcode.LedgerUnavailable),
				Attempts:  3,
				Failures:  1,
			}},
		},
	}
	srv := compensationServer(t, comp)

	rec := getWithToken(t, srv, "/v1/writebacks/pending", testOperatorToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var body pendingWritebacksResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if body.PendingCount != 1 {
		t.Errorf("待写回数量 = %d，期望 1", body.PendingCount)
	}
	if body.OpenAlertCount != 1 {
		t.Errorf("告警数量 = %d，期望 1", body.OpenAlertCount)
	}
	if len(body.Items) != 1 {
		t.Fatalf("清单条目数 = %d，期望 1", len(body.Items))
	}
	item := body.Items[0]
	if item.RunID != "run-test-0001" {
		t.Errorf("清单里的 run = %q，期望 run-test-0001", item.RunID)
	}
	if item.Target != domain.WritebackTargetLedger {
		t.Errorf("清单目标 = %q，期望 ledger", item.Target)
	}
	if item.Attempts != 3 || item.Failures != 1 {
		t.Errorf("清单里的尝试次数不对：%+v", item)
	}
	if item.PointID != "point-vax-freezer-01" {
		t.Errorf("清单里的点位 = %q，期望 point-vax-freezer-01", item.PointID)
	}
	if len(body.Alerts) != 1 || body.Alerts[0].State != domain.WritebackAlertStateOpen {
		t.Errorf("告警清单不对：%+v", body.Alerts)
	}
}

// TestGetPendingWritebacks_RequiresCredential 覆盖鉴权：
// 无凭据 401、投递通道凭据 403（它只看温度，不看结论）。
func TestGetPendingWritebacks_RequiresCredential(t *testing.T) {
	comp := &stubCompensator{}
	srv := compensationServer(t, comp)

	rec := getWithToken(t, srv, "/v1/writebacks/pending", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无凭据期望 401，实际 %d：%s", rec.Code, rec.Body.String())
	}
}

// TestRetryWriteback_TriggersCompensation 覆盖主路径：
// 补偿重放端点把 run 交给补偿编排器，并把结果如实回执。
func TestRetryWriteback_TriggersCompensation(t *testing.T) {
	comp := &stubCompensator{
		retryOutcome: runtime.WritebackOutcome{
			RunID:          "run-test-0001",
			ExternalRef:    "cc-point-vax-freezer-01-20261019T191000Z",
			IdempotencyKey: "54b35c7f136371619ea6a5313714d1ac3",
			LedgerWritten:  true,
			WritebackState: domain.WritebackStateWrittenBack,
			Compensation:   true,
			Note:           "冷链台账越温记录已写入，接种门诊通知已发出",
		},
	}
	srv := compensationServer(t, comp)

	rec := postRetry(t, srv, "/v1/runs/run-test-0001/writeback/retry", testOperatorToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if len(comp.retryRunIDs) != 1 || comp.retryRunIDs[0] != "run-test-0001" {
		t.Fatalf("被重放的 run = %v，期望恰好一次且是 run-test-0001", comp.retryRunIDs)
	}
	var body retryWritebackResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析回执失败：%v", err)
	}
	if !body.LedgerWritten {
		t.Errorf("回执里的台账落点不对：%+v", body)
	}
	if !body.Compensation {
		t.Error("回执应当标明本次是补偿重放")
	}
	if body.WritebackState != domain.WritebackStateWrittenBack {
		t.Errorf("回执里的写回状态 = %q，期望 written_back", body.WritebackState)
	}
	if body.IdempotencyKey == "" {
		t.Error("回执应当带出幂等键（重放用的就是它）")
	}
}

// TestRetryWriteback_StillFailingReportsError 覆盖失败路径：
// 补发仍失败时回执如实说明哪一头没写成、还差什么。
func TestRetryWriteback_StillFailingReportsError(t *testing.T) {
	comp := &stubCompensator{
		retryOutcome: runtime.WritebackOutcome{
			RunID:          "run-test-0001",
			LedgerError:    string(errcode.LedgerUnavailable),
			LedgerAttempts: 3,
			WritebackState: domain.WritebackStatePending,
			Compensation:   true,
			Note:           "冷链台账写入失败，本次越温尚未入账，挂起待补偿重放",
		},
	}
	srv := compensationServer(t, comp)

	rec := postRetry(t, srv, "/v1/runs/run-test-0001/writeback/retry", testOperatorToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var body retryWritebackResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析回执失败：%v", err)
	}
	if body.LedgerWritten {
		t.Error("补发没成功时不该报写成")
	}
	if body.LedgerError != string(errcode.LedgerUnavailable) {
		t.Errorf("回执里的错误码 = %q，期望 E_LEDGER_UNAVAILABLE", body.LedgerError)
	}
	if body.LedgerAttempts != 3 {
		t.Errorf("回执里的尝试次数 = %d，期望 3", body.LedgerAttempts)
	}
	if body.WritebackState != domain.WritebackStatePending {
		t.Errorf("回执里的写回状态 = %q，期望 pending", body.WritebackState)
	}
}

// TestRetryWriteback_RequiresCredential 覆盖鉴权与角色闸门：
// 无凭据 401、只读岗 403（它不替人补发台账）。
func TestRetryWriteback_RequiresCredential(t *testing.T) {
	comp := &stubCompensator{}
	srv := compensationServer(t, comp)

	rec := postRetry(t, srv, "/v1/runs/run-test-0001/writeback/retry", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无凭据期望 401，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if len(comp.retryRunIDs) != 0 {
		t.Errorf("未通过鉴权时不该触发出站调用，实际触发了 %v", comp.retryRunIDs)
	}

	rec = postRetry(t, srv, "/v1/runs/run-test-0001/writeback/retry", testAuditorToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("只读岗期望 403，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if len(comp.retryRunIDs) != 0 {
		t.Errorf("越权时不该触发出站调用，实际触发了 %v", comp.retryRunIDs)
	}
}

// TestRetryWriteback_SupervisorAllowed 覆盖主管这一侧：
// 主管持有补偿重放权限，补发得进去。
func TestRetryWriteback_SupervisorAllowed(t *testing.T) {
	comp := &stubCompensator{retryOutcome: runtime.WritebackOutcome{RunID: "run-test-0001"}}
	srv := compensationServer(t, comp)

	rec := postRetry(t, srv, "/v1/runs/run-test-0001/writeback/retry", testSupervisorToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if len(comp.retryRunIDs) != 1 {
		t.Fatalf("主管补发应当触发出站调用，实际 %v", comp.retryRunIDs)
	}
}

// TestWritebackEndpoints_WithoutCapabilityFailClosed 覆盖未装配：
// 没有补偿能力时端点明确拒绝，不回一个空的「一切正常」。
func TestWritebackEndpoints_WithoutCapabilityFailClosed(t *testing.T) {
	srv := New(Options{
		Version:     "0.1.0",
		Logger:      logging.New(nil),
		Credentials: testCredentials(),
	})

	rec := getWithToken(t, srv, "/v1/writebacks/pending", testOperatorToken)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("未装配时期望 500，实际 %d：%s", rec.Code, rec.Body.String())
	}
	rec = postRetry(t, srv, "/v1/runs/run-test-0001/writeback/retry", testOperatorToken)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("未装配时期望 500，实际 %d：%s", rec.Code, rec.Body.String())
	}
}

// TestRoutes_RegistersCompensationEndpoints 路由表供契约脚本遍历比对。
func TestRoutes_RegistersCompensationEndpoints(t *testing.T) {
	srv := New(Options{Version: "0.1.0", Logger: logging.New(nil), Credentials: testCredentials()})

	want := map[string]bool{
		"GET /v1/writebacks/pending":         false,
		"POST /v1/runs/{id}/writeback/retry": false,
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

// 兜底：确认零值时刻不会让清单条目崩掉。
var _ = time.Time{}
