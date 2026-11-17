package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
	"github.com/chaosforge/coldchain-temp-assessment/internal/observe"
	"github.com/chaosforge/coldchain-temp-assessment/internal/store"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// 本文件验证「运行摘要」这一段的 HTTP 接缝。
//
// 三条底线：
//   - **要凭据**：摘要是业务数据（判了多少段、欠了多少条），不是服务自身状态，
//     所以它跟 run 详情端点同一把钥匙，没凭据进不来；
//   - **只读**：摘要端点不推进任何业务状态，也不发任何出站请求；
//   - **查不出来就说查不出来**：返回全 0 的摘要会被读成「今天什么事都没发生」，
//     而真相是「没人在统计」——那是两件完全不同的事。

// stubSummaryReader 是运行摘要的测试替身：按预设返回摘要或错误。
type stubSummaryReader struct {
	summary observe.Summary
	err     error
	calls   int
}

func (s *stubSummaryReader) Collect(_ context.Context) (observe.Summary, error) {
	s.calls++
	if s.err != nil {
		return observe.Summary{}, s.err
	}
	return s.summary, nil
}

// summaryServer 造一台挂了摘要端点的 Server。
func summaryServer(t *testing.T, reader SummaryReader) *Server {
	t.Helper()
	return New(Options{
		Version:       "0.1.0",
		Logger:        logging.New(nil),
		SummaryReader: reader,
		Credentials:   testCredentials(),
	})
}

// TestGetSummary_RequiresCredential 断言没凭据拿不到摘要。
func TestGetSummary_RequiresCredential(t *testing.T) {
	reader := &stubSummaryReader{summary: observe.Summary{Events: 3}}
	srv := summaryServer(t, reader)

	rec := getWithToken(t, srv, "/v1/summary", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 %d（期望 401），响应体：%s", rec.Code, rec.Body.String())
	}
	if reader.calls != 0 {
		t.Errorf("未鉴权时取数被调用了 %d 次，期望 0 次", reader.calls)
	}
}

// TestGetSummary_WrongCredentialRejected 断言错凭据也拿不到摘要。
func TestGetSummary_WrongCredentialRejected(t *testing.T) {
	srv := summaryServer(t, &stubSummaryReader{summary: observe.Summary{Events: 3}})

	rec := getWithToken(t, srv, "/v1/summary", "not-a-real-token")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 %d（期望 401），响应体：%s", rec.Code, rec.Body.String())
	}
}

// TestGetSummary_ReturnsSummary 断言摘要按形状原样返回。
func TestGetSummary_ReturnsSummary(t *testing.T) {
	reader := &stubSummaryReader{summary: observe.Summary{
		GeneratedAt:          "2026-10-22T12:00:00Z",
		WindowHours:          24,
		WindowStart:          "2026-10-21T12:00:00Z",
		WindowEnd:            "2026-10-22T12:00:00Z",
		Events:               1200,
		Runs:                 7,
		FailedRuns:           1,
		DegradedRuns:         2,
		AwaitingConfirmation: 3,
		PendingWritebacks:    2,
		OpenWritebackAlerts:  1,
		ModelCalls:           9,
		DegradedModelCalls:   3,
		FailedModelCalls:     1,
		DegradeRate:          0.3333,
		RunningRuns:          1,
		QueuedRuns:           2,
		RetryingRuns:         1,
		Hourly: []observe.HourlyBucket{{
			HourStart:        "2026-10-22T09:00:00Z",
			EventCount:       120,
			ExcursionCount:   1,
			PeakTemperatureC: 12.4,
			Points:           3,
		}},
		HourlyCoveredHours: 1,
		Notes:              []string{"有越温还没进冷链台账"},
	}}
	srv := summaryServer(t, reader)

	rec := getWithToken(t, srv, "/v1/summary", testOperatorToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d（期望 200），响应体：%s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是合法 JSON：%v", err)
	}
	for _, key := range []string{
		"generated_at", "window_hours", "window_start", "window_end",
		"events", "runs", "failed_runs", "degraded_runs",
		"awaiting_confirmation", "pending_writebacks", "open_writeback_alerts",
		"model_calls", "degraded_model_calls", "failed_model_calls", "degrade_rate",
		"running_runs", "queued_runs", "retrying_runs",
		"hourly", "hourly_covered_hours", "notes",
	} {
		if _, ok := body[key]; !ok {
			t.Errorf("响应体缺字段 %q", key)
		}
	}
	if body["events"].(float64) != 1200 {
		t.Errorf("events = %v，期望 1200", body["events"])
	}
	if body["degrade_rate"].(float64) != 0.3333 {
		t.Errorf("degrade_rate = %v，期望 0.3333", body["degrade_rate"])
	}
}

// TestGetSummary_AllRolesWithRunRead 断言三把钥匙都拿得到摘要。
//
// 摘要与 run 详情、审计链同属「调阅判定依据」这一类，只读岗（质控 / 院感）
// 也该看得到——它复核越温结论时第一眼问的就是「服务现在在忙什么」。
func TestGetSummary_AllRolesWithRunRead(t *testing.T) {
	for _, token := range []string{testOperatorToken, testSupervisorToken, testAuditorToken} {
		srv := summaryServer(t, &stubSummaryReader{summary: observe.Summary{Events: 1}})
		rec := getWithToken(t, srv, "/v1/summary", token)
		if rec.Code != http.StatusOK {
			t.Errorf("凭据 %s 拿到状态码 %d（期望 200）", token, rec.Code)
		}
	}
}

// TestGetSummary_WithoutReaderFailsClosed 断言没装配取数能力时拒绝服务。
//
// 返回一份全 0 的摘要等于告诉运维「今天什么事都没发生」——
// 而真相是「没人在统计」。这两种情况必须区分得开。
func TestGetSummary_WithoutReaderFailsClosed(t *testing.T) {
	srv := New(Options{
		Version:     "0.1.0",
		Logger:      logging.New(nil),
		Credentials: testCredentials(),
	})

	rec := getWithToken(t, srv, "/v1/summary", testOperatorToken)
	if rec.Code == http.StatusOK {
		t.Fatalf("没装配取数能力时不应返回 200，响应体：%s", rec.Body.String())
	}
	var body struct {
		ErrorCode string `json:"error_code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是合法 JSON：%v", err)
	}
	if body.ErrorCode == "" {
		t.Error("错误响应体缺 error_code")
	}
}

// TestGetSummary_ReaderErrorPropagates 断言取数失败时报错而不是回全 0。
func TestGetSummary_ReaderErrorPropagates(t *testing.T) {
	boom := errcode.Newf(errcode.StorageError, "库打不开")
	srv := summaryServer(t, &stubSummaryReader{err: boom})

	rec := getWithToken(t, srv, "/v1/summary", testOperatorToken)
	if rec.Code == http.StatusOK {
		t.Fatalf("取数失败时不应返回 200，响应体：%s", rec.Body.String())
	}
	var body struct {
		ErrorCode string `json:"error_code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是合法 JSON：%v", err)
	}
	if body.ErrorCode != string(errcode.StorageError) {
		t.Errorf("error_code = %q，期望 %q", body.ErrorCode, errcode.StorageError)
	}
}

// TestGetSummary_CountsMatchDatabase 断言端点回的数字与库里的实际条数一致。
//
// 这是最要紧的一条：一个数对不上的看板比没有看板更坏——
// 看的人会照着它做决定。
func TestGetSummary_CountsMatchDatabase(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := st.InsertEvent(ctx, domain.TemperatureEvent{
		PointID:      "point-vax-freezer-01",
		DeviceID:     "device-recorder-0001",
		DeviceName:   "疫苗冷藏库温度记录仪",
		MeasuredAt:   now.Add(-5 * time.Minute),
		TemperatureC: 11.4,
		Unit:         "C",
		Source:       "recorder",
	}); err != nil {
		t.Fatalf("写入温度采样失败：%v", err)
	}
	if err := st.UpsertRun(ctx, domain.RunRecord{
		Run: domain.Run{
			RunID:     "run-summary-http-001",
			PointID:   "point-vax-freezer-01",
			State:     string(workflow.StateAwaitingConfirmation),
			ExecState: domain.ExecStateSucceeded,
		},
		CreatedAt: now.Add(-time.Hour),
		UpdatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("写入判定 run 失败：%v", err)
	}

	srv := New(Options{
		Version:       "0.1.0",
		Logger:        logging.New(nil),
		SummaryReader: observe.NewCollector(observe.NewStoreSource(st)),
		Credentials:   testCredentials(),
	})

	rec := getWithToken(t, srv, "/v1/summary", testAuditorToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d（期望 200），响应体：%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Events               int `json:"events"`
		Runs                 int `json:"runs"`
		AwaitingConfirmation int `json:"awaiting_confirmation"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是合法 JSON：%v", err)
	}
	if body.Events != 1 {
		t.Errorf("events = %d，库里实际 1 条", body.Events)
	}
	if body.Runs != 1 {
		t.Errorf("runs = %d，库里实际 1 条", body.Runs)
	}
	if body.AwaitingConfirmation != 1 {
		t.Errorf("awaiting_confirmation = %d，库里实际 1 条", body.AwaitingConfirmation)
	}
}
