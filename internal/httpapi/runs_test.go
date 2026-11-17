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

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
	"github.com/chaosforge/coldchain-temp-assessment/internal/store"
)

// newRunTestServer 造一台带判定 run 查询能力的 Server。
func newRunTestServer(t *testing.T) (*Server, *store.Store) {
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
	})
	return srv, st
}

// seedRun 在库里造一条跑完的判定 run（带完整步骤产物）。
func seedRun(t *testing.T, st *store.Store) string {
	t.Helper()
	ctx := context.Background()

	run := domain.NewRun("point-vax-freezer-01")
	start := time.Date(2026, 10, 20, 3, 10, 0, 0, time.UTC)
	end := time.Date(2026, 10, 20, 3, 25, 0, 0, time.UTC)
	run.BindWindow(start, end)
	run.State = domain.RunStateAwaitingConfirmation
	run.ExecState = domain.ExecStateSucceeded

	profile := &domain.PointProfile{PointID: "point-vax-freezer-01", ZoneMinC: 2, ZoneMaxC: 8}
	if err := st.UpsertRun(ctx, domain.RunRecord{Run: run, Profile: profile}); err != nil {
		t.Fatalf("写入 run 失败：%v", err)
	}

	// 两步产物：越限窗口与待确认标记。
	windowPayload, err := domain.EncodePayload(domain.PayloadWindow, &domain.WindowPayload{
		Window: domain.ExcursionWindow{
			Start: start, End: end, DurationMinutes: 15, PeakC: 12.4, SampleCount: 4,
		},
		ZoneMinC: 2, ZoneMaxC: 8, ZoneSource: "点位温区配置",
	})
	if err != nil {
		t.Fatalf("编码窗口产物失败：%v", err)
	}
	readyPayload, err := domain.EncodePayload(domain.PayloadConfirmationReady, &domain.ConfirmationReadyPayload{
		RunID: run.RunID, PointID: run.PointID, ExternalRef: run.ExternalRef,
		AwaitingRole: "supervisor", OutstandingFields: []string{"batches", "disposition"},
		ReadyAt: end,
	})
	if err != nil {
		t.Fatalf("编码待确认产物失败：%v", err)
	}

	now := time.Now().UTC()
	steps := []domain.Step{
		{
			RunID: run.RunID, StepName: "extract_window", Seq: 2, Attempt: 1,
			State: domain.StepStateSucceeded, PayloadKind: domain.PayloadWindow,
			Payload: windowPayload, StartedAt: now, EndedAt: ptrTime(now.Add(120 * time.Millisecond)),
		},
		{
			RunID: run.RunID, StepName: "await_confirmation", Seq: 5, Attempt: 1,
			State: domain.StepStateSucceeded, PayloadKind: domain.PayloadConfirmationReady,
			Payload: readyPayload, StartedAt: now, EndedAt: ptrTime(now.Add(30 * time.Millisecond)),
		},
	}
	for _, step := range steps {
		if err := st.InsertRunStep(ctx, step); err != nil {
			t.Fatalf("写入步骤记录失败：%v", err)
		}
	}
	return run.RunID
}

// ptrTime 取一个时刻的指针。
func ptrTime(t time.Time) *time.Time { return &t }

// doGetRun 发一次带凭据的 GET /v1/runs/{id}。
func doGetRun(t *testing.T, srv *Server, runID, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/runs/"+runID, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// TestGetRun_ReturnsFullTimelineAfterRestart 验证「重启后仍能查到完整时间线」。
//
// 端点读的是库里的真相：进程重启、判定续跑完成之后，
// 这条 run 的总体状态与每一步的起止、耗时都还在。
func TestGetRun_ReturnsFullTimelineAfterRestart(t *testing.T) {
	srv, st := newRunTestServer(t)
	runID := seedRun(t, st)

	rec := doGetRun(t, srv, runID, testOperatorToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("查询 run 应返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Run struct {
			RunID     string `json:"run_id"`
			PointID   string `json:"point_id"`
			State     string `json:"state"`
			ExecState string `json:"exec_state"`
			Zone      string `json:"zone"`
		} `json:"run"`
		Steps []struct {
			Seq        int    `json:"seq"`
			StepName   string `json:"step_name"`
			State      string `json:"state"`
			Attempt    int    `json:"attempt"`
			DurationMS int64  `json:"duration_ms"`
		} `json:"steps"`
		StepCount int            `json:"step_count"`
		Products  map[string]any `json:"products"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应体失败：%v", err)
	}

	if body.Run.RunID != runID {
		t.Fatalf("响应里的 run_id 不符：%q", body.Run.RunID)
	}
	if body.Run.ExecState != string(domain.ExecStateSucceeded) {
		t.Fatalf("响应里的执行态不符：%q", body.Run.ExecState)
	}
	if body.Run.State != domain.RunStateAwaitingConfirmation {
		t.Fatalf("响应里的业务状态不符：%q", body.Run.State)
	}
	if body.Run.Zone != "2~8℃" {
		t.Fatalf("响应里的温区不符：%q", body.Run.Zone)
	}
	if body.StepCount != 2 || len(body.Steps) != 2 {
		t.Fatalf("时间线应有 2 条，实际 %d 条（step_count=%d）", len(body.Steps), body.StepCount)
	}
	for i, step := range body.Steps {
		if step.DurationMS < 0 {
			t.Fatalf("第 %d 步耗时为负：%d", i+1, step.DurationMS)
		}
		if step.StepName == "" || step.State == "" {
			t.Fatalf("第 %d 步缺名称或状态：%+v", i+1, step)
		}
	}
	if _, ok := body.Products[string(domain.PayloadWindow)]; !ok {
		t.Fatalf("产物里应含 %s 分码，实际 %v", domain.PayloadWindow, body.Products)
	}
}

// TestGetRun_MissingTokenRejected 验证判定结论不是匿名可读的。
func TestGetRun_MissingTokenRejected(t *testing.T) {
	srv, st := newRunTestServer(t)
	runID := seedRun(t, st)

	rec := doGetRun(t, srv, runID, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("缺凭据应返回 401，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), string(errcode.Unauthorized)) {
		t.Fatalf("响应体应含错误码 %s，实际 %s", errcode.Unauthorized, rec.Body.String())
	}
}

// TestGetRun_UnknownRunReturns404 验证查不到就说查不到。
func TestGetRun_UnknownRunReturns404(t *testing.T) {
	srv, _ := newRunTestServer(t)

	rec := doGetRun(t, srv, "run-does-not-exist", testOperatorToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("run 不存在应返回 404，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), string(errcode.NotFound)) {
		t.Fatalf("响应体应含错误码 %s，实际 %s", errcode.NotFound, rec.Body.String())
	}
}

// TestGetRun_WithoutRunReaderFailsClosed 验证没装配查询能力时端点拒绝服务，
// 而不是回一个空壳让人以为「这条 run 什么也没有」。
func TestGetRun_WithoutRunReaderFailsClosed(t *testing.T) {
	quiet := logging.New(io.Discard)
	srv := New(Options{Version: "0.1.0", Logger: quiet, IngestToken: testIngestToken,
		Credentials: testCredentials()})

	rec := doGetRun(t, srv, "run-any", testOperatorToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未装配查询能力时应返回 404，实际 %d：%s", rec.Code, rec.Body.String())
	}
}

// TestGetRunDetail_IncludesWindowSummaryAndCurrentStep 验证 run 详情端点
// 除了总体状态，还给出窗口摘要与「当前停在哪一步」。
//
// 现场问的是两句大白话：「哪段越温、峰值多少」和「它现在停在哪」。
// 两句都得在一次调用里答完，不该让人自己去翻产物。
func TestGetRunDetail_IncludesWindowSummaryAndCurrentStep(t *testing.T) {
	srv, st := newRunTestServer(t)
	runID := seedRun(t, st)

	rec := doGetRun(t, srv, runID, testOperatorToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("查询 run 应返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Window struct {
			WindowStart      string   `json:"window_start"`
			WindowEnd        string   `json:"window_end"`
			PeakTemperatureC *float64 `json:"peak_temperature_c"`
			DurationMinutes  *int     `json:"duration_minutes"`
			SampleCount      *int     `json:"sample_count"`
			Zone             string   `json:"zone"`
			ZoneSource       string   `json:"zone_source"`
		} `json:"window"`
		CurrentStep struct {
			StepName string `json:"step_name"`
			State    string `json:"state"`
			Seq      int    `json:"seq"`
		} `json:"current_step"`
		RecentErrors []struct {
			ErrorCode string `json:"error_code"`
		} `json:"recent_errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应体失败：%v", err)
	}

	if body.Window.PeakTemperatureC == nil || *body.Window.PeakTemperatureC != 12.4 {
		t.Fatalf("窗口摘要的峰值不符：%+v", body.Window.PeakTemperatureC)
	}
	if body.Window.DurationMinutes == nil || *body.Window.DurationMinutes != 15 {
		t.Fatalf("窗口摘要的持续分钟不符：%+v", body.Window.DurationMinutes)
	}
	if body.Window.SampleCount == nil || *body.Window.SampleCount != 4 {
		t.Fatalf("窗口摘要的采样数不符：%+v", body.Window.SampleCount)
	}
	if body.Window.ZoneSource == "" {
		t.Fatal("窗口摘要应带温区来源")
	}
	if body.CurrentStep.StepName != "await_confirmation" {
		t.Fatalf("当前步应是 await_confirmation，实际 %q", body.CurrentStep.StepName)
	}
	if body.CurrentStep.State != domain.StepStateSucceeded {
		t.Fatalf("当前步状态不符：%q", body.CurrentStep.State)
	}
	if body.RecentErrors == nil {
		t.Fatal("recent_errors 不应为 null")
	}
}

// TestGetRunTimeline_ReturnsPositionsAndCurrentStep 验证时间线端点回每步的
// 名称、起止、耗时与位置坐标，并指出当前停在哪一步。
func TestGetRunTimeline_ReturnsPositionsAndCurrentStep(t *testing.T) {
	srv, st := newRunTestServer(t)
	runID := seedRun(t, st)

	rec := doGetRunTimeline(t, srv, runID, testOperatorToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("查询时间线应返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}

	var body struct {
		RunID           string `json:"run_id"`
		RunStartedAt    string `json:"run_started_at"`
		TotalDurationMS int64  `json:"total_duration_ms"`
		StepCount       int    `json:"step_count"`
		CurrentStep     struct {
			StepName string `json:"step_name"`
			State    string `json:"state"`
		} `json:"current_step"`
		Entries []struct {
			Seq        int     `json:"seq"`
			StepName   string  `json:"step_name"`
			State      string  `json:"state"`
			StartedAt  string  `json:"started_at"`
			EndedAt    string  `json:"ended_at"`
			DurationMS int64   `json:"duration_ms"`
			OffsetMS   int64   `json:"offset_ms"`
			EndOffset  int64   `json:"end_offset_ms"`
			StartPct   float64 `json:"start_pct"`
			WidthPct   float64 `json:"width_pct"`
			Payload    any     `json:"payload"`
		} `json:"entries"`
		RecentErrors []any `json:"recent_errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应体失败：%v", err)
	}

	if body.RunID != runID {
		t.Fatalf("时间线的 run_id 不符：%q", body.RunID)
	}
	if body.StepCount != 2 || len(body.Entries) != 2 {
		t.Fatalf("时间线应有 2 条，实际 %d 条（step_count=%d）", len(body.Entries), body.StepCount)
	}
	if body.RunStartedAt == "" {
		t.Fatal("时间线应有起点时刻")
	}
	for i, entry := range body.Entries {
		if entry.StepName == "" || entry.State == "" {
			t.Fatalf("第 %d 条缺名称或状态：%+v", i+1, entry)
		}
		if entry.StartedAt == "" || entry.EndedAt == "" {
			t.Fatalf("第 %d 条缺起止时刻：%+v", i+1, entry)
		}
		if entry.DurationMS < 0 {
			t.Fatalf("第 %d 条耗时为负：%d", i+1, entry.DurationMS)
		}
		if entry.Payload != nil {
			t.Fatalf("第 %d 条不应带产物正文", i+1)
		}
	}
	// 位置单调不重叠：后一条的起点不早于前一条的终点。
	for i := 1; i < len(body.Entries); i++ {
		if body.Entries[i].OffsetMS < body.Entries[i-1].EndOffset {
			t.Fatalf("第 %d 条位置与前一条重叠", i+1)
		}
	}
	if body.CurrentStep.StepName != "await_confirmation" {
		t.Fatalf("当前步应是 await_confirmation，实际 %q", body.CurrentStep.StepName)
	}
}

// TestGetRunTimeline_MissingTokenRejected 验证时间线同样要凭据。
func TestGetRunTimeline_MissingTokenRejected(t *testing.T) {
	srv, st := newRunTestServer(t)
	runID := seedRun(t, st)

	rec := doGetRunTimeline(t, srv, runID, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("缺凭据应返回 401，实际 %d", rec.Code)
	}
}

// TestGetRunTimeline_UnknownRunReturns404 验证查不到就说查不到。
func TestGetRunTimeline_UnknownRunReturns404(t *testing.T) {
	srv, _ := newRunTestServer(t)

	rec := doGetRunTimeline(t, srv, "run-does-not-exist", testOperatorToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("run 不存在应返回 404，实际 %d：%s", rec.Code, rec.Body.String())
	}
}

// TestGetRunTimeline_WithoutRunReaderFailsClosed 验证没装配查询能力时端点拒绝服务。
func TestGetRunTimeline_WithoutRunReaderFailsClosed(t *testing.T) {
	quiet := logging.New(io.Discard)
	srv := New(Options{Version: "0.1.0", Logger: quiet, IngestToken: testIngestToken,
		Credentials: testCredentials()})

	rec := doGetRunTimeline(t, srv, "run-any", testOperatorToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未装配查询能力时应返回 404，实际 %d：%s", rec.Code, rec.Body.String())
	}
}

// doGetRunTimeline 发一次带凭据的 GET /v1/runs/{id}/timeline。
func doGetRunTimeline(t *testing.T, srv *Server, runID, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/runs/"+runID+"/timeline", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}
