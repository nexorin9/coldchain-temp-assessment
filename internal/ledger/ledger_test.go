package ledger

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// 本文件是出站写侧（台账越温偏差 + 接种门诊通知）的契约测试。
//
// 它盯住五件事：
//
//  1. 请求形状对不对：方法、路径、鉴权头、Content-Type。
//     路径写错一个字母，台账回的是 404，而系统看起来「写完了」。
//  2. 请求体字段齐不齐：spec 的出站字段规范逐项落到请求体里，
//     一个字段不缺——台账侧按栏位取值，缺一栏就是一条残缺的记录。
//  3. 幂等键在不在：external_ref 与 idempotency_key 必须随体发出，
//     台账侧靠它们防重复入账。
//  4. 失败语义分不分得开：凭据被拒、请求不合法、端点坏了、超时、已入账，
//     五种失败在处置上完全不同（改凭据 / 改代码 / 退避重试 / 认账 / 不重试）。
//  5. 批次号原样带出：台账给的批次号一个字符都不改。

// newTestDeviationWriter 造一个对着测试台账的写侧客户端。
func newTestDeviationWriter(t *testing.T, handler http.HandlerFunc) (*DeviationWriter, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	w, err := NewDeviationWriter(WritebackConfig{
		BaseURL: server.URL,
		Token:   "test-ledger-token",
		Timeout: 2 * time.Second,
	}, WithWritebackHTTPClient(server.Client()))
	if err != nil {
		t.Fatalf("装配台账写侧失败：%v", err)
	}
	return w, server
}

// newTestNotifier 造一个对着测试通知系统的客户端。
func newTestNotifier(t *testing.T, handler http.HandlerFunc) (*Notifier, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	n, err := NewNotifier(NotifyConfig{
		BaseURL: server.URL,
		Token:   "test-notify-token",
		Timeout: 2 * time.Second,
	}, WithNotifyHTTPClient(server.Client()))
	if err != nil {
		t.Fatalf("装配通知客户端失败：%v", err)
	}
	return n, server
}

// sampleDeviation 造一条脱敏的越温偏差记录。
func sampleDeviation() DeviationRecord {
	return DeviationRecord{
		ExternalRef:        "cc-point-vax-freezer-01-20261019T191000Z",
		IdempotencyKey:     "54b35c7f136371619ea6a5313714d1ac3",
		PointID:            "point-vax-freezer-01",
		DeviceName:         "疫苗冷藏箱-01号",
		ExcursionStartedAt: time.Date(2026, 10, 20, 3, 10, 0, 0, time.FixedZone("CST", 8*3600)),
		ExcursionEndedAt:   time.Date(2026, 10, 20, 3, 25, 0, 0, time.FixedZone("CST", 8*3600)),
		PeakTemperatureC:   12.4,
		DurationMinutes:    15,
		Zone:               "2~8℃",
		Batches: []domain.Batch{
			{
				PointID:     "point-vax-freezer-01",
				BatchRef:    "<EXAMPLE_LOT-01>",
				ProductName: "疫苗（脱敏样例）",
				Quantity:    120,
				ExpiryDate:  "<EXAMPLE_EXPIRY-A>",
			},
		},
		CauseConclusion: "door_open",
		Evidence: []domain.AttributionEvidence{
			{Feature: "door_open_events", Value: 2, Threshold: 1, Note: "窗口内 2 条采样上报开门"},
		},
		Disposition:     "isolate",
		ConfirmedBy:     "supervisor-007",
		ConfirmedAt:     time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC),
		ExceptionRecord: "一、基本信息\n点位：point-vax-freezer-01（疫苗冷藏箱-01号）",
	}
}

// TestDeviationWrite_RequestShapeAndBody 断言越温偏差的请求形状与请求体字段。
func TestDeviationWrite_RequestShapeAndBody(t *testing.T) {
	var (
		gotMethod, gotPath, gotAuth, gotContentType string
		gotBody                                     map[string]any
	)
	w, _ := newTestDeviationWriter(t, func(rw http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusCreated)
		_, _ = rw.Write([]byte(`{"record_id":"dev-001"}`))
	})

	if err := w.Write(context.Background(), sampleDeviation()); err != nil {
		t.Fatalf("写回越温偏差失败：%v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("请求方法 = %q，期望 POST", gotMethod)
	}
	if gotPath != "/api/v1/cold-chain/deviations" {
		t.Errorf("请求路径 = %q，期望 /api/v1/cold-chain/deviations", gotPath)
	}
	if gotAuth != "Bearer test-ledger-token" {
		t.Errorf("鉴权头 = %q，期望 Bearer test-ledger-token", gotAuth)
	}
	if !strings.HasPrefix(gotContentType, "application/json") {
		t.Errorf("Content-Type = %q，期望 application/json", gotContentType)
	}

	// spec 的出站字段规范：一项都不能少。
	for _, field := range []string{
		"external_ref", "point_id", "device_name", "excursion_started_at",
		"excursion_ended_at", "peak_temperature_c", "duration_minutes", "zone",
		"batches", "cause_conclusion", "evidence", "disposition",
		"confirmed_by", "confirmed_at", "exception_record", "idempotency_key",
	} {
		if _, ok := gotBody[field]; !ok {
			t.Errorf("请求体缺字段 %q", field)
		}
	}

	// 值要逐项对上：台账侧按这些栏位取值，错了就是一条对不上的记录。
	if gotBody["external_ref"] != "cc-point-vax-freezer-01-20261019T191000Z" {
		t.Errorf("external_ref = %v", gotBody["external_ref"])
	}
	if gotBody["idempotency_key"] != "54b35c7f136371619ea6a5313714d1ac3" {
		t.Errorf("idempotency_key = %v", gotBody["idempotency_key"])
	}
	if gotBody["peak_temperature_c"] != 12.4 {
		t.Errorf("peak_temperature_c = %v，期望 12.4", gotBody["peak_temperature_c"])
	}
	if gotBody["duration_minutes"] != float64(15) {
		t.Errorf("duration_minutes = %v，期望 15", gotBody["duration_minutes"])
	}
	if gotBody["disposition"] != "isolate" {
		t.Errorf("disposition = %v，期望 isolate", gotBody["disposition"])
	}
	if gotBody["confirmed_by"] != "supervisor-007" {
		t.Errorf("confirmed_by = %v，期望 supervisor-007", gotBody["confirmed_by"])
	}

	// 批次原样带出：一个字符都不改，系统不生成新号。
	batches, ok := gotBody["batches"].([]any)
	if !ok || len(batches) != 1 {
		t.Fatalf("batches = %v，期望 1 条", gotBody["batches"])
	}
	first, _ := batches[0].(map[string]any)
	if first["batch_ref"] != "<EXAMPLE_LOT-01>" {
		t.Errorf("批次号 = %v，期望 <EXAMPLE_LOT-01>", first["batch_ref"])
	}
}

// TestDeviationWrite_ArrayFieldsAreEmptyNotNull 断言数组字段发 [] 而不是 null：
// 「这次越温不涉及任何批次」是一个空清单，不是「这一栏没有值」——
// 后者在严格的 JSON Schema 校验下会被台账侧拒收，整条记录写不进去。
func TestDeviationWrite_ArrayFieldsAreEmptyNotNull(t *testing.T) {
	var raw string
	w, _ := newTestDeviationWriter(t, func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		raw = string(body)
		rw.WriteHeader(http.StatusOK)
	})
	rec := sampleDeviation()
	rec.Batches = nil
	rec.Evidence = nil
	if err := w.Write(context.Background(), rec); err != nil {
		t.Fatalf("写回越温偏差失败：%v", err)
	}
	if !strings.Contains(raw, `"batches":[]`) {
		t.Errorf("batches 应当是空数组，实际请求体：%s", raw)
	}
	if !strings.Contains(raw, `"evidence":[]`) {
		t.Errorf("evidence 应当是空数组，实际请求体：%s", raw)
	}
	if strings.Contains(raw, `"batches":null`) || strings.Contains(raw, `"evidence":null`) {
		t.Errorf("数组字段不得发 null，实际请求体：%s", raw)
	}
}

// TestDeviationWrite_ConflictMeansAlreadyRecorded 断言台账判重时按成功记账。
func TestDeviationWrite_ConflictMeansAlreadyRecorded(t *testing.T) {
	var calls int32
	w, _ := newTestDeviationWriter(t, func(rw http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		rw.WriteHeader(http.StatusConflict)
		_, _ = rw.Write([]byte(`{"error":"duplicate external_ref"}`))
	})
	if err := w.Write(context.Background(), sampleDeviation()); err != nil {
		t.Fatalf("台账判重时不应报错：%v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("台账被调用 %d 次，期望 1 次", got)
	}
}

// TestDeviationWrite_FailureSemantics 断言四种失败各自归到正确的错误码。
func TestDeviationWrite_FailureSemantics(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   errcode.Code
	}{
		{"凭据被拒", http.StatusUnauthorized, errcode.Unauthorized},
		{"请求不合法", http.StatusBadRequest, errcode.Validation},
		{"点位不存在", http.StatusNotFound, errcode.NotFound},
		{"端点故障", http.StatusInternalServerError, errcode.LedgerUnavailable},
		{"限流", http.StatusTooManyRequests, errcode.LedgerUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := newTestDeviationWriter(t, func(rw http.ResponseWriter, r *http.Request) {
				rw.WriteHeader(tc.status)
				_, _ = rw.Write([]byte(`{"error":"boom"}`))
			})
			err := w.Write(context.Background(), sampleDeviation())
			if err == nil {
				t.Fatal("失败必须报错")
			}
			if code := errcode.As(err).Code; code != tc.want {
				t.Errorf("错误码 = %q，期望 %q", code, tc.want)
			}
		})
	}
}

// TestDeviationWrite_TimeoutIsRetryable 断言超时归到可退避重试的一档。
func TestDeviationWrite_TimeoutIsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = rw.Write([]byte(`{}`))
	}))
	defer server.Close()
	w, err := NewDeviationWriter(WritebackConfig{
		BaseURL: server.URL,
		Token:   "t",
		Timeout: 50 * time.Millisecond,
	}, WithWritebackHTTPClient(&http.Client{Timeout: 50 * time.Millisecond}))
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	writeErr := w.Write(context.Background(), sampleDeviation())
	if writeErr == nil {
		t.Fatal("超时必须报错")
	}
	if code := errcode.As(writeErr).Code; code != errcode.LedgerUnavailable {
		t.Errorf("错误码 = %q，期望 %q", code, errcode.LedgerUnavailable)
	}
}

// TestDeviationWrite_MissingIdentityRejected 断言缺 external_ref 或 point_id 时
// 根本不发请求：台账侧靠这两栏认出「这是哪次越温、哪个点位」。
func TestDeviationWrite_MissingIdentityRejected(t *testing.T) {
	var calls int32
	w, _ := newTestDeviationWriter(t, func(rw http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		rw.WriteHeader(http.StatusOK)
	})
	rec := sampleDeviation()
	rec.ExternalRef = ""
	if err := w.Write(context.Background(), rec); err == nil {
		t.Fatal("缺 external_ref 必须报错")
	}
	rec = sampleDeviation()
	rec.PointID = "  "
	if err := w.Write(context.Background(), rec); err == nil {
		t.Fatal("缺 point_id 必须报错")
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("台账被调用 %d 次，期望 0 次（校验没过就不该发请求）", got)
	}
}

// TestWriteDeviation_UsesFrozenPayload 断言 workflow.OutboundWriter 约定那条路
// 走的是「确认时固化的载荷原文」，而不是现算的结果。
func TestWriteDeviation_UsesFrozenPayload(t *testing.T) {
	var gotBody map[string]any
	w, _ := newTestDeviationWriter(t, func(rw http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		rw.WriteHeader(http.StatusOK)
	})

	frozen := `{"external_ref":"cc-frozen-01","idempotency_key":"key-frozen",` +
		`"point_id":"point-vax-freezer-01","device_name":"疫苗冷藏箱-01号",` +
		`"excursion_started_at":"2026-10-20T03:10:00+08:00",` +
		`"excursion_ended_at":"2026-10-20T03:25:00+08:00",` +
		`"peak_temperature_c":12.4,"duration_minutes":15,"zone":"2~8℃",` +
		`"batches":[],"cause_conclusion":"door_open","evidence":[],` +
		`"disposition":"isolate","confirmed_by":"supervisor-007",` +
		`"confirmed_at":"2026-10-20T11:00:00Z","exception_record":"记录表正文"}`

	if err := w.WriteDeviation(context.Background(), "run-001", frozen); err != nil {
		t.Fatalf("按固化载荷写回失败：%v", err)
	}
	if gotBody["external_ref"] != "cc-frozen-01" {
		t.Errorf("external_ref = %v，期望 cc-frozen-01（固化载荷原样发出）", gotBody["external_ref"])
	}
	if gotBody["exception_record"] != "记录表正文" {
		t.Errorf("exception_record = %v，期望 记录表正文", gotBody["exception_record"])
	}
}

// TestDecodeDeviation_RejectsIncompletePayload 断言残缺载荷被显式拒绝。
func TestDecodeDeviation_RejectsIncompletePayload(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"空载荷", ""},
		{"不是 JSON", "not-json"},
		{"缺 external_ref", `{"point_id":"point-vax-freezer-01"}`},
		{"缺 point_id", `{"external_ref":"cc-01"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeDeviation(tc.payload); err == nil {
				t.Fatal("残缺载荷必须被拒绝")
			}
		})
	}
}

// TestNotifySend_RequestShapeAndBody 断言通知的请求形状与请求体字段。
func TestNotifySend_RequestShapeAndBody(t *testing.T) {
	var (
		gotMethod, gotPath, gotAuth string
		gotBody                     map[string]any
	)
	n, _ := newTestNotifier(t, func(rw http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		rw.WriteHeader(http.StatusAccepted)
		_, _ = rw.Write([]byte(`{"message_id":"msg-001"}`))
	})

	msg := Message{
		Title:          "冷链温控越温通知",
		Body:           "点位 point-vax-freezer-01 越温，峰值 12.4 ℃。",
		RelatedRunID:   "run-001",
		IdempotencyKey: "ntf-abc123",
	}
	if err := n.Send(context.Background(), msg); err != nil {
		t.Fatalf("发送通知失败：%v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("请求方法 = %q，期望 POST", gotMethod)
	}
	if gotPath != "/api/v1/messages" {
		t.Errorf("请求路径 = %q，期望 /api/v1/messages", gotPath)
	}
	if gotAuth != "Bearer test-notify-token" {
		t.Errorf("鉴权头 = %q，期望 Bearer test-notify-token", gotAuth)
	}
	for _, field := range []string{
		"channel", "audience", "title", "body", "related_run_id", "idempotency_key",
	} {
		if _, ok := gotBody[field]; !ok {
			t.Errorf("通知请求体缺字段 %q", field)
		}
	}
	if gotBody["related_run_id"] != "run-001" {
		t.Errorf("related_run_id = %v", gotBody["related_run_id"])
	}
	if gotBody["idempotency_key"] != "ntf-abc123" {
		t.Errorf("idempotency_key = %v", gotBody["idempotency_key"])
	}
	// 渠道与通知对象没显式给时由客户端补默认值——不该让调用方每次都填。
	if gotBody["channel"] != "coldchain_deviation" {
		t.Errorf("channel = %v，期望默认渠道 coldchain_deviation", gotBody["channel"])
	}
	if gotBody["audience"] != "vaccination_clinic" {
		t.Errorf("audience = %v，期望默认通知对象 vaccination_clinic", gotBody["audience"])
	}
}

// TestNotifySend_ConfiguredChannelAndAudience 断言渠道与通知对象可按院内约定覆盖。
func TestNotifySend_ConfiguredChannelAndAudience(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		rw.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	n, err := NewNotifier(NotifyConfig{
		BaseURL:  server.URL,
		Token:    "t",
		Timeout:  time.Second,
		Channel:  "院内冷链告警",
		Audience: "vaccination_clinic_02",
	}, WithNotifyHTTPClient(server.Client()))
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	if err := n.Send(context.Background(), Message{
		Title:          "冷链温控越温通知",
		IdempotencyKey: "k",
	}); err != nil {
		t.Fatalf("发送失败：%v", err)
	}
	if gotBody["channel"] != "院内冷链告警" {
		t.Errorf("channel = %v，期望 院内冷链告警", gotBody["channel"])
	}
	if gotBody["audience"] != "vaccination_clinic_02" {
		t.Errorf("audience = %v，期望 vaccination_clinic_02", gotBody["audience"])
	}
}

// TestNotifySend_FailureSemantics 断言通知侧四种失败各自归到正确的错误码。
func TestNotifySend_FailureSemantics(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   errcode.Code
	}{
		{"凭据被拒", http.StatusUnauthorized, errcode.Unauthorized},
		{"消息不合法", http.StatusBadRequest, errcode.Validation},
		{"端点故障", http.StatusInternalServerError, errcode.NotifyUnavailable},
		{"限流", http.StatusTooManyRequests, errcode.NotifyUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, _ := newTestNotifier(t, func(rw http.ResponseWriter, r *http.Request) {
				rw.WriteHeader(tc.status)
				_, _ = rw.Write([]byte(`{"error":"boom"}`))
			})
			err := n.Send(context.Background(), Message{
				Title:          "冷链温控越温通知",
				IdempotencyKey: "k",
			})
			if err == nil {
				t.Fatal("失败必须报错")
			}
			if code := errcode.As(err).Code; code != tc.want {
				t.Errorf("错误码 = %q，期望 %q", code, tc.want)
			}
		})
	}
}

// TestNotifySend_ConflictMeansAlreadySent 断言通知判重时按成功记账。
func TestNotifySend_ConflictMeansAlreadySent(t *testing.T) {
	n, _ := newTestNotifier(t, func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusConflict)
		_, _ = rw.Write([]byte(`{"error":"duplicate"}`))
	})
	if err := n.Send(context.Background(), Message{
		Title:          "冷链温控越温通知",
		IdempotencyKey: "k",
	}); err != nil {
		t.Fatalf("通知判重时不应报错：%v", err)
	}
}

// TestNotifySend_MissingTitleOrKeyRejected 断言缺标题或幂等键时不发请求。
func TestNotifySend_MissingTitleOrKeyRejected(t *testing.T) {
	var calls int32
	n, _ := newTestNotifier(t, func(rw http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		rw.WriteHeader(http.StatusOK)
	})
	if err := n.Send(context.Background(), Message{IdempotencyKey: "k"}); err == nil {
		t.Fatal("缺标题必须报错")
	}
	if err := n.Send(context.Background(), Message{Title: "t"}); err == nil {
		t.Fatal("缺幂等键必须报错")
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("通知系统被调用 %d 次，期望 0 次", got)
	}
}

// TestWritebackConfigFromEnv_MissingItems 断言缺必填环境变量时报出缺哪几项。
func TestWritebackConfigFromEnv_MissingItems(t *testing.T) {
	t.Setenv("COLDCHAIN_LEDGER_BASE_URL", "")
	t.Setenv("COLDCHAIN_LEDGER_TOKEN", "")
	_, err := WritebackConfigFromEnv()
	if err == nil {
		t.Fatal("缺必填环境变量时装配必须失败")
	}
	e := errcode.As(err)
	if !strings.Contains(e.Details, "COLDCHAIN_LEDGER_BASE_URL") ||
		!strings.Contains(e.Details, "COLDCHAIN_LEDGER_TOKEN") {
		t.Errorf("错误详情 = %q，期望点名两个必填项", e.Details)
	}
}

// TestNotifyConfigFromEnv_MissingItems 断言通知侧缺项时报出缺哪几项。
func TestNotifyConfigFromEnv_MissingItems(t *testing.T) {
	t.Setenv("COLDCHAIN_NOTIFY_BASE_URL", "")
	t.Setenv("COLDCHAIN_NOTIFY_TOKEN", "")
	_, err := NotifyConfigFromEnv()
	if err == nil {
		t.Fatal("缺必填环境变量时装配必须失败")
	}
	e := errcode.As(err)
	if !strings.Contains(e.Details, "COLDCHAIN_NOTIFY_BASE_URL") ||
		!strings.Contains(e.Details, "COLDCHAIN_NOTIFY_TOKEN") {
		t.Errorf("错误详情 = %q，期望点名两个必填项", e.Details)
	}
}

// TestNewDeviationWriter_ConfigErrors 断言缺基址或缺凭据时装配即失败。
func TestNewDeviationWriter_ConfigErrors(t *testing.T) {
	cases := []struct {
		name string
		cfg  WritebackConfig
	}{
		{"缺基址", WritebackConfig{Token: "t"}},
		{"缺凭据", WritebackConfig{BaseURL: "https://ledger.example.internal"}},
		{"基址不合法", WritebackConfig{BaseURL: "ledger.example.internal", Token: "t"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewDeviationWriter(tc.cfg); err == nil {
				t.Fatal("配置不合法时装配必须失败")
			}
		})
	}
}
