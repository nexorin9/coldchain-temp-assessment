package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// ---------------------------------------------------------------------------
// 本文件用本地 OpenAI-compatible 协议替身验证模型网关。
//
// 替身只出现在测试数据层：它按真实兼容协议收发（真实 HTTP、真实请求头、
// 真实信封结构），但不代表产品最终对接面——生产端点是院内自建或合规云端
// 的推理服务，由 MODEL_BASE_URL 指定。真实端点未实连这件事记在 progress.txt。
// ---------------------------------------------------------------------------

// recordedRequest 是一次被网关打过来的请求记录。
type recordedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   string
}

// stub 是本地 OpenAI-compatible 协议替身。
type stub struct {
	mu       sync.Mutex
	requests []recordedRequest
	// handle 决定这次请求回什么；默认回一个合法的归因结论信封。
	handle func(w http.ResponseWriter, r *http.Request)
	srv    *httptest.Server
}

// newStub 起一个本地协议替身。
func newStub(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) *stub {
	t.Helper()
	s := &stub{handle: handle}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.requests = append(s.requests, recordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Header: r.Header.Clone(),
			Body:   string(body),
		})
		s.mu.Unlock()
		if s.handle != nil {
			s.handle(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":     "chatcmpl-stub",
			"object": "chat.completion",
			"model":  "stub-model",
			"choices": []any{map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": `{"cause_code":"power_loss","confidence":0.9}`,
				},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens":     120,
				"completion_tokens": 30,
				"total_tokens":      150,
			},
		})
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// writeJSON 回一个 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// count 返回替身收到的请求数。
func (s *stub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// last 返回替身收到的最后一个请求。
func (s *stub) last(t *testing.T) recordedRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		t.Fatal("替身没有收到任何请求")
	}
	return s.requests[len(s.requests)-1]
}

// newTestGateway 起一个指向替身的网关。
func newTestGateway(t *testing.T, s *stub, timeout time.Duration) *ModelGateway {
	t.Helper()
	g, err := NewGateway(GatewayConfig{
		BaseURL: s.srv.URL,
		APIKey:  "test-key",
		Model:   "stub-model",
		Timeout: timeout,
	})
	if err != nil {
		t.Fatalf("装配网关失败：%v", err)
	}
	return g
}

// codeOf 取一个错误的错误码。
func codeOf(t *testing.T, err error) errcode.Code {
	t.Helper()
	if err == nil {
		t.Fatal("期望出错，实际没有错误")
	}
	e := errcode.As(err)
	if e == nil {
		t.Fatalf("错误没有带错误码：%v", err)
	}
	return e.Code
}

// ---------------------------------------------------------------------------
// 配置面
// ---------------------------------------------------------------------------

func TestNewGateway_MissingConfigFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		cfg  GatewayConfig
	}{
		{"缺基址", GatewayConfig{APIKey: "k", Model: "m", Timeout: time.Second}},
		{"缺凭据", GatewayConfig{BaseURL: "https://llm.example.cn/v1", Model: "m", Timeout: time.Second}},
		{"缺模型名", GatewayConfig{BaseURL: "https://llm.example.cn/v1", APIKey: "k", Timeout: time.Second}},
		{"超时非正", GatewayConfig{BaseURL: "https://llm.example.cn/v1", APIKey: "k", Model: "m", Timeout: 0}},
		{"基址不合法", GatewayConfig{BaseURL: "llm.example.cn/v1", APIKey: "k", Model: "m", Timeout: time.Second}},
		{"基址缺协议", GatewayConfig{BaseURL: "ftp://llm.example.cn/v1", APIKey: "k", Model: "m", Timeout: time.Second}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, err := NewGateway(tc.cfg)
			if err == nil {
				t.Fatalf("期望装配失败，实际成功：%+v", g)
			}
			if code := codeOf(t, err); code != errcode.ConfigMissing {
				t.Fatalf("错误码应为 %s，实际 %s", errcode.ConfigMissing, code)
			}
		})
	}
}

func TestGatewayConfigFromEnv_ReadsOnlyEnv(t *testing.T) {
	t.Setenv("MODEL_BASE_URL", "https://llm.internal.example-hospital.cn/v1/")
	t.Setenv("MODEL_API_KEY", "env-key")
	t.Setenv("MODEL_NAME", "env-model")
	t.Setenv("MODEL_TIMEOUT_SECONDS", "12")

	cfg, err := GatewayConfigFromEnv()
	if err != nil {
		t.Fatalf("从环境变量装配失败：%v", err)
	}
	if cfg.BaseURL != "https://llm.internal.example-hospital.cn/v1" {
		t.Fatalf("基址应去掉末尾斜杠，实际 %q", cfg.BaseURL)
	}
	if cfg.APIKey != "env-key" || cfg.Model != "env-model" {
		t.Fatalf("凭据或模型名取错：%+v", cfg)
	}
	if cfg.Timeout != 12*time.Second {
		t.Fatalf("超时应为 12s，实际 %s", cfg.Timeout)
	}
}

func TestGatewayConfigFromEnv_MissingOrInvalid(t *testing.T) {
	t.Run("缺项", func(t *testing.T) {
		t.Setenv("MODEL_BASE_URL", "")
		t.Setenv("MODEL_API_KEY", "")
		t.Setenv("MODEL_NAME", "")
		_, err := GatewayConfigFromEnv()
		if code := codeOf(t, err); code != errcode.ConfigMissing {
			t.Fatalf("错误码应为 %s，实际 %s", errcode.ConfigMissing, code)
		}
		if !strings.Contains(err.Error(), "MODEL_BASE_URL") ||
			!strings.Contains(err.Error(), "MODEL_API_KEY") ||
			!strings.Contains(err.Error(), "MODEL_NAME") {
			t.Fatalf("错误信息应点名缺失的三项，实际：%v", err)
		}
	})
	t.Run("超时非法", func(t *testing.T) {
		t.Setenv("MODEL_BASE_URL", "https://llm.example.cn/v1")
		t.Setenv("MODEL_API_KEY", "k")
		t.Setenv("MODEL_NAME", "m")
		t.Setenv("MODEL_TIMEOUT_SECONDS", "0")
		_, err := GatewayConfigFromEnv()
		if code := codeOf(t, err); code != errcode.ConfigMissing {
			t.Fatalf("错误码应为 %s，实际 %s", errcode.ConfigMissing, code)
		}
	})
	t.Run("超时缺省", func(t *testing.T) {
		t.Setenv("MODEL_BASE_URL", "https://llm.example.cn/v1")
		t.Setenv("MODEL_API_KEY", "k")
		t.Setenv("MODEL_NAME", "m")
		t.Setenv("MODEL_TIMEOUT_SECONDS", "")
		cfg, err := GatewayConfigFromEnv()
		if err != nil {
			t.Fatalf("装配失败：%v", err)
		}
		if cfg.Timeout != 30*time.Second {
			t.Fatalf("默认超时应为 30s，实际 %s", cfg.Timeout)
		}
	})
}

// ---------------------------------------------------------------------------
// 正常路径
// ---------------------------------------------------------------------------

func TestGateway_Complete_StructuredResponse(t *testing.T) {
	s := newStub(t, nil)
	g := newTestGateway(t, s, 5*time.Second)

	out, err := g.Complete(context.Background(), SchemaAttribution, RequestOptions{
		UserPayload: map[string]any{"point_id": "point-vax-freezer-01"},
	})
	if err != nil {
		t.Fatalf("调用失败：%v", err)
	}
	if out.Schema != SchemaAttribution {
		t.Fatalf("分码应为 %s，实际 %s", SchemaAttribution, out.Schema)
	}
	if string(out.Content) != `{"cause_code":"power_loss","confidence":0.9}` {
		t.Fatalf("内容原样返回不对：%s", out.Content)
	}
	if out.Model != "stub-model" {
		t.Fatalf("模型名应为 stub-model，实际 %q", out.Model)
	}
	if out.PromptTokens != 120 || out.CompletionTokens != 30 || out.TotalTokens != 150 {
		t.Fatalf("用量取回不对：%+v", out)
	}
	if out.LatencyMS < 0 {
		t.Fatalf("耗时不应为负：%d", out.LatencyMS)
	}

	// 请求本身也要对：路径、鉴权头、请求体形状。
	req := s.last(t)
	if req.Method != http.MethodPost || req.Path != "/chat/completions" {
		t.Fatalf("请求方法或路径不对：%s %s", req.Method, req.Path)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer test-key" {
		t.Fatalf("鉴权头不对：%q", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type 不对：%q", got)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(req.Body), &body); err != nil {
		t.Fatalf("请求体不是合法 JSON：%v", err)
	}
	if body["model"] != "stub-model" {
		t.Fatalf("请求体里的模型名不对：%v", body["model"])
	}
	rf, ok := body["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("请求体缺少 response_format：%v", body)
	}
	js, ok := rf["json_schema"].(map[string]any)
	if !ok || js["name"] != string(SchemaAttribution) {
		t.Fatalf("response_format.json_schema 不对：%v", rf)
	}
}

func TestGateway_Complete_EmptyContentIsEnvelopeValid(t *testing.T) {
	// 分层：空内容是「模型没给内容」，属内容形状问题（E_MODEL_SCHEMA_INVALID），
	// 不是信封问题（E_MODEL_PROTOCOL）。这里锁住这条边界。
	s := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"choices": []any{map[string]any{
				"message":       map[string]any{"role": "assistant", "content": ""},
				"finish_reason": "stop",
			}},
		})
	})
	g := newTestGateway(t, s, 5*time.Second)

	out, err := g.Complete(context.Background(), SchemaAttribution, RequestOptions{
		UserPayload: map[string]any{},
	})
	if err != nil {
		t.Fatalf("信封层不应报错：%v", err)
	}
	if len(out.Content) != 0 {
		t.Fatalf("内容应为空，实际 %q", out.Content)
	}
	if _, err := ParseAttribution(out.Content); codeOf(t, err) != errcode.ModelSchemaInvalid {
		t.Fatalf("内容层应报 %s，实际 %s", errcode.ModelSchemaInvalid, codeOf(t, err))
	}
}

// ---------------------------------------------------------------------------
// 错误语义：三类错误码互不混淆
// ---------------------------------------------------------------------------

func TestGateway_TimeoutReturnsModelTimeout(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		writeJSON(w, http.StatusOK, map[string]any{})
	})
	g := newTestGateway(t, s, 200*time.Millisecond)

	_, err := g.Complete(context.Background(), SchemaAttribution, RequestOptions{
		UserPayload: map[string]any{},
	})
	if code := codeOf(t, err); code != errcode.ModelTimeout {
		t.Fatalf("错误码应为 %s，实际 %s（错误：%v）", errcode.ModelTimeout, code, err)
	}
}

func TestGateway_ServerErrorReturnsModelUnavailable(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal"}`))
	})
	g := newTestGateway(t, s, 5*time.Second)

	_, err := g.Complete(context.Background(), SchemaAttribution, RequestOptions{
		UserPayload: map[string]any{},
	})
	if code := codeOf(t, err); code != errcode.ModelUnavailable {
		t.Fatalf("错误码应为 %s，实际 %s（错误：%v）", errcode.ModelUnavailable, code, err)
	}
}

func TestGateway_NonJSONResponseReturnsModelProtocol(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<html><body>login required</body></html>`))
	})
	g := newTestGateway(t, s, 5*time.Second)

	_, err := g.Complete(context.Background(), SchemaAttribution, RequestOptions{
		UserPayload: map[string]any{},
	})
	if code := codeOf(t, err); code != errcode.ModelProtocol {
		t.Fatalf("错误码应为 %s，实际 %s（错误：%v）", errcode.ModelProtocol, code, err)
	}
}

func TestGateway_ThreeErrorCodesAreDistinct(t *testing.T) {
	// 同一端点三种故障，三类错误码必须各归各的，不能互相吞并。
	timeoutStub := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	})
	timeoutGateway := newTestGateway(t, timeoutStub, 200*time.Millisecond)
	_, timeoutErr := timeoutGateway.Complete(context.Background(), SchemaAttribution,
		RequestOptions{UserPayload: map[string]any{}})

	downStub := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	downGateway := newTestGateway(t, downStub, 5*time.Second)
	_, downErr := downGateway.Complete(context.Background(), SchemaAttribution,
		RequestOptions{UserPayload: map[string]any{}})

	badStub := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not json at all`))
	})
	badGateway := newTestGateway(t, badStub, 5*time.Second)
	_, badErr := badGateway.Complete(context.Background(), SchemaAttribution,
		RequestOptions{UserPayload: map[string]any{}})

	got := []errcode.Code{
		codeOf(t, timeoutErr),
		codeOf(t, downErr),
		codeOf(t, badErr),
	}
	want := []errcode.Code{errcode.ModelTimeout, errcode.ModelUnavailable, errcode.ModelProtocol}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 类错误码应为 %s，实际 %s", i+1, want[i], got[i])
		}
	}
	if got[0] == got[1] || got[1] == got[2] || got[0] == got[2] {
		t.Fatalf("三类错误码必须互不相同，实际 %v", got)
	}
}

func TestGateway_ProtocolViolationsRejected(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"缺 choices", http.StatusOK, `{"id":"x","model":"m"}`},
		{"choices 为空", http.StatusOK, `{"choices":[]}`},
		{"缺 content 字段", http.StatusOK, `{"choices":[{"message":{"role":"assistant"},"finish_reason":"stop"}]}`},
		{"输出被截断", http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":"{}"},"finish_reason":"length"}]}`},
		{"被内容策略拦下", http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":"{}"},"finish_reason":"content_filter"}]}`},
		{"客户端错误", http.StatusBadRequest, `{"error":{"message":"bad request"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			g := newTestGateway(t, s, 5*time.Second)
			_, err := g.Complete(context.Background(), SchemaAttribution, RequestOptions{
				UserPayload: map[string]any{},
			})
			if code := codeOf(t, err); code != errcode.ModelProtocol {
				t.Fatalf("错误码应为 %s，实际 %s（错误：%v）", errcode.ModelProtocol, code, err)
			}
		})
	}
}

func TestGateway_RateLimitedIsUnavailable(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	g := newTestGateway(t, s, 5*time.Second)
	_, err := g.Complete(context.Background(), SchemaAttribution, RequestOptions{
		UserPayload: map[string]any{},
	})
	if code := codeOf(t, err); code != errcode.ModelUnavailable {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.ModelUnavailable, code)
	}
}

func TestGateway_ConnectionRefusedIsUnavailable(t *testing.T) {
	// 端点没起：连不上属服务不可达，不是协议问题。
	g, err := NewGateway(GatewayConfig{
		BaseURL: "http://127.0.0.1:1/v1",
		APIKey:  "k",
		Model:   "m",
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	_, err = g.Complete(context.Background(), SchemaAttribution, RequestOptions{
		UserPayload: map[string]any{},
	})
	if code := codeOf(t, err); code != errcode.ModelUnavailable {
		t.Fatalf("错误码应为 %s，实际 %s（错误：%v）", errcode.ModelUnavailable, code, err)
	}
}

func TestGateway_OversizedResponseRejected(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("x", maxResponseBytes+1024)))
	})
	g := newTestGateway(t, s, 5*time.Second)
	_, err := g.Complete(context.Background(), SchemaAttribution, RequestOptions{
		UserPayload: map[string]any{},
	})
	if code := codeOf(t, err); code != errcode.ModelProtocol {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.ModelProtocol, code)
	}
}

// ---------------------------------------------------------------------------
// 取消与重试
// ---------------------------------------------------------------------------

func TestGateway_ContextCanceledBeforeCallSendsNoRequest(t *testing.T) {
	s := newStub(t, nil)
	g := newTestGateway(t, s, 5*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := g.Complete(ctx, SchemaAttribution, RequestOptions{UserPayload: map[string]any{}})
	if err == nil {
		t.Fatal("期望出错，实际成功")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("错误应能被 errors.Is 认成 context.Canceled，实际：%v", err)
	}
	if code := codeOf(t, err); code != errcode.ModelTimeout {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.ModelTimeout, code)
	}
	if s.count() != 0 {
		t.Fatalf("调用方已取消，请求不该发出去，实际发出 %d 次", s.count())
	}
}

func TestGateway_ContextCancelDuringCallReturnsPromptly(t *testing.T) {
	release := make(chan struct{})
	s := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
		writeJSON(w, http.StatusOK, map[string]any{})
	})
	t.Cleanup(func() { close(release) })
	g := newTestGateway(t, s, 10*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := g.Complete(ctx, SchemaAttribution, RequestOptions{UserPayload: map[string]any{}})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("期望出错，实际成功")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("错误应能被 errors.Is 认成 context.Canceled，实际：%v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("取消后应立即返回，实际耗时 %s", elapsed)
	}
}

func TestGateway_ContextDeadlineExceededIsTimeout(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	})
	g := newTestGateway(t, s, 10*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := g.Complete(ctx, SchemaAttribution, RequestOptions{UserPayload: map[string]any{}})
	if code := codeOf(t, err); code != errcode.ModelTimeout {
		t.Fatalf("错误码应为 %s，实际 %s（错误：%v）", errcode.ModelTimeout, code, err)
	}
}

func TestGateway_DoesNotRetryInternally(t *testing.T) {
	// 重试由判定步骤层统一负责（步骤级重试 + 指数退避）；
	// 网关再自己重试一次，退避时长就叠乘了。
	s := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	g := newTestGateway(t, s, 5*time.Second)
	_, err := g.Complete(context.Background(), SchemaAttribution, RequestOptions{
		UserPayload: map[string]any{},
	})
	if codeOf(t, err) != errcode.ModelUnavailable {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.ModelUnavailable, codeOf(t, err))
	}
	if s.count() != 1 {
		t.Fatalf("网关不应自行重试，实际请求 %d 次", s.count())
	}
}

// ---------------------------------------------------------------------------
// 凭据卫生
// ---------------------------------------------------------------------------

func TestGateway_ErrorNeverLeaksCredential(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	})
	g := newTestGateway(t, s, 5*time.Second)
	_, err := g.Complete(context.Background(), SchemaAttribution, RequestOptions{
		UserPayload: map[string]any{},
	})
	if err == nil {
		t.Fatal("期望出错，实际成功")
	}
	if strings.Contains(err.Error(), "test-key") {
		t.Fatalf("错误信息里出现了凭据：%v", err)
	}
}

func TestGateway_MissingSchemaRejectedBeforeCall(t *testing.T) {
	s := newStub(t, nil)
	g := newTestGateway(t, s, 5*time.Second)
	_, err := g.Complete(context.Background(), SchemaName("not_registered"), RequestOptions{
		UserPayload: map[string]any{},
	})
	if code := codeOf(t, err); code != errcode.Validation {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.Validation, code)
	}
	if s.count() != 0 {
		t.Fatalf("分码未登记时不该发请求，实际发出 %d 次", s.count())
	}
}
