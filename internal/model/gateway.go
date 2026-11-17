// 本文件实现本服务唯一的模型调用出口：OpenAI-compatible 的 ModelGateway。
//
// 边界（与 schema.go / validate.go 分工）：
//
//	schema.go   —— 请求体长什么样、响应内容该是什么形状；
//	gateway.go  —— 请求发得到哪儿、发得出去吗、回来的东西是不是一个合法的
//	                OpenAI-compatible 响应信封；
//	validate.go —— 信封里的内容对不对（字段、类型、取值域）。
//
// 三层各自只认一类失败，处置上完全不同：
//
//	E_MODEL_PROTOCOL      —— 端点回了，但不是合法的兼容响应（连不上 JSON、
//	                        没有 choices、输出被截断）。多半是端点不对或者
//	                        模型没按结构化输出要求答，重发一次也没用。
//	E_MODEL_TIMEOUT       —— 端点没在时限内回话。可以退避重试。
//	E_MODEL_UNAVAILABLE   —— 端点连不上或回服务端错误（5xx / 429）。可退避重试。
//
// 三类互不混淆：调用方要靠它们决定「退避重试」还是「直接退回规则基线」。
//
// 两条硬约束：
//
//  1. 凭据只从环境变量读，缺失就启动失败——绝不发出匿名请求。拿不到凭据还
//     硬发一次，得到的 401 会掩盖真正的问题（配置没给，而不是端点坏了）。
//  2. 网关内部不做重试。重试由判定步骤层统一负责（步骤级重试与指数退避，
//     见 internal/runtime/retry.go）；两层都重试会让退避时长叠乘，
//     一次瞬时故障能拖成一串等待。
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/audit"
	"github.com/chaosforge/coldchain-temp-assessment/internal/config"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
)

const (
	// chatCompletionsPath 是 OpenAI-compatible 的聊天补全路径。
	chatCompletionsPath = "/chat/completions"
	// maxResponseBytes 是模型响应体上限（字节）。
	//
	// 越限不是「模型话多」，多半是端点回错了东西（比如把一个网页当 JSON
	// 返回）。挡住它比把它整份塞进内存再报错安全。
	maxResponseBytes = 1 << 20
	// gatewayUserAgent 是出站请求的 User-Agent。
	gatewayUserAgent = "coldchain-temp-assessment/0.1 (model-gateway)"
)

// GatewayConfig 是模型网关的配置面。
//
// 四个取值全部来自环境变量，不带任何默认值：基址、凭据、模型名缺一不可，
// 超时可选（默认 30 秒）。
type GatewayConfig struct {
	// BaseURL 是 OpenAI-compatible 端点基址（不含 /chat/completions）。
	BaseURL string
	// APIKey 是调用凭据，只从 MODEL_API_KEY 读。
	APIKey string
	// Model 是模型名，只从 MODEL_NAME 读。
	Model string
	// Timeout 是单次调用的时限。
	Timeout time.Duration
}

// GatewayConfigFromEnv 从环境变量装配网关配置面。
//
// 只校验模型侧的四项，不牵连台账 / 通知那几项——离线的 assess / import
// 子命令同样要能起网关，不必先把整站配置凑齐。
func GatewayConfigFromEnv() (GatewayConfig, error) {
	cfg := GatewayConfig{
		BaseURL: strings.TrimRight(strings.TrimSpace(os.Getenv(config.EnvModelBaseURL)), "/"),
		APIKey:  strings.TrimSpace(os.Getenv(config.EnvModelAPIKey)),
		Model:   strings.TrimSpace(os.Getenv(config.EnvModelName)),
		Timeout: config.DefaultModelTimeout,
	}
	if raw := strings.TrimSpace(os.Getenv(config.EnvModelTimeout)); raw != "" {
		secs, err := strconv.Atoi(raw)
		if err != nil || secs <= 0 {
			return GatewayConfig{}, errcode.Newf(errcode.ConfigMissing,
				"%s 必须为正整数秒，当前取值 %q", config.EnvModelTimeout, raw)
		}
		cfg.Timeout = time.Duration(secs) * time.Second
	}
	var missing []string
	if cfg.BaseURL == "" {
		missing = append(missing, config.EnvModelBaseURL)
	}
	if cfg.APIKey == "" {
		missing = append(missing, config.EnvModelAPIKey)
	}
	if cfg.Model == "" {
		missing = append(missing, config.EnvModelName)
	}
	if len(missing) > 0 {
		return GatewayConfig{}, errcode.Newf(errcode.ConfigMissing,
			"模型网关缺少必填配置项：%s（请在环境变量或 .env 中设置，详见 .env.example）",
			strings.Join(missing, "、"))
	}
	return cfg, nil
}

// GatewayOption 是网关的可选装配项。
type GatewayOption func(*ModelGateway)

// WithHTTPClient 注入自定义 HTTP 客户端（测试与运维排障用）。
func WithHTTPClient(c *http.Client) GatewayOption {
	return func(g *ModelGateway) {
		if c != nil {
			g.client = c
		}
	}
}

// WithLogger 注入结构化日志器。
func WithLogger(l *logging.Logger) GatewayOption {
	return func(g *ModelGateway) {
		if l != nil {
			g.logger = l
		}
	}
}

// ModelGateway 是本服务唯一的模型调用出口。
//
// 业务层只依赖它，不直接接触 http.Client，也没有任何厂商 SDK 依赖——
// 换端点（院内自建 / 合规云端）只改环境变量。
type ModelGateway struct {
	cfg    GatewayConfig
	client *http.Client
	logger *logging.Logger
}

// NewGateway 装配模型网关。
//
// 配置不合法直接返回 E_CONFIG_MISSING：宁可起不来，也不要带着半份配置
// 发出一注定失败的请求。
func NewGateway(cfg GatewayConfig, opts ...GatewayOption) (*ModelGateway, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, errcode.Newf(errcode.ConfigMissing,
			"模型网关基址为空（%s 未配置）", config.EnvModelBaseURL)
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errcode.Newf(errcode.ConfigMissing,
			"模型网关凭据为空（%s 未配置）：不发匿名请求", config.EnvModelAPIKey)
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errcode.Newf(errcode.ConfigMissing,
			"模型名为空（%s 未配置）", config.EnvModelName)
	}
	if cfg.Timeout <= 0 {
		return nil, errcode.Newf(errcode.ConfigMissing,
			"模型调用超时必须为正数（%s）", config.EnvModelTimeout)
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errcode.Newf(errcode.ConfigMissing,
			"模型网关基址不合法：%q（须为带协议与主机名的绝对地址）", cfg.BaseURL)
	}
	g := &ModelGateway{
		cfg: GatewayConfig{
			BaseURL: base,
			APIKey:  cfg.APIKey,
			Model:   cfg.Model,
			Timeout: cfg.Timeout,
		},
		logger: logging.Default(),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(g)
		}
	}
	if g.client == nil {
		g.client = &http.Client{Timeout: g.cfg.Timeout}
	}
	return g, nil
}

// Model 返回网关使用的模型名（供审计与日志引用）。
func (g *ModelGateway) Model() string { return g.cfg.Model }

// BaseURL 返回网关基址（不含凭据）。
func (g *ModelGateway) BaseURL() string { return g.cfg.BaseURL }

// Completion 是一次模型调用的结果。
//
// 这里刻意**不**解析内容：内容对不对由 validate.go 判定（形状不合规与
// 数值说错了是两回事，处置也不同）。网关只负责把信封拆开、把原文交出去，
// 并把用量（token 数）原样带出来——计价与审计由审计层接着做。
type Completion struct {
	// Schema 是这次调用对应的结构化输出分码。
	Schema SchemaName
	// Content 是模型返回的内容原文（未校验）。
	Content []byte
	// Model 是端点回报的模型名；端点没回报时回落为配置里的模型名。
	Model string
	// FinishReason 是端点给的结束原因。
	FinishReason string
	// PromptTokens / CompletionTokens / TotalTokens 是端点回报的用量。
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// LatencyMS 是本次调用耗时（毫秒）。
	LatencyMS int64 `json:"latency_ms"`
	// PromptDigest 是本次请求体的指纹（sha256 前 16 位十六进制）。
	//
	// 提示词原文不进审计记录，但它带着现场点位与读数上下文，
	// 事后复核要能回答「这两次判定用的是不是同一段提示词」——
	// 指纹够回答这个问题，又不把现场上下文抄进审计表。
	PromptDigest string `json:"prompt_digest"`
	// PromptChars 是本次请求体的字符数。
	PromptChars int `json:"prompt_chars"`
}

// Complete 按结构化输出分码发起一次模型调用。
//
// opts.Model 为空时用配置里的模型名；opts.SystemPrompt 为空时用该分码的
// 默认系统提示词。返回的 Completion.Content 是**未经校验**的原文，
// 调用方须再经 Parse* 系列校验后才可进入业务结论。
func (g *ModelGateway) Complete(ctx context.Context, name SchemaName, opts RequestOptions) (*Completion, error) {
	if opts.Model == "" {
		opts.Model = g.cfg.Model
	}
	if opts.SystemPrompt == "" {
		opts.SystemPrompt = DefaultSystemPrompt(name)
	}
	body, err := BuildRequest(name, opts)
	if err != nil {
		return nil, err
	}
	return g.complete(ctx, name, body)
}

// chatCompletion 是 OpenAI-compatible 的响应信封。
//
// 只取本服务真正用到的字段：内容、结束原因与用量。其余字段（id、created、
// system_fingerprint 等）不消费——同源取舍：未知字段忽略而不报错。
type chatCompletion struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role string `json:"role"`
			// Content 用指针：字段整个缺失（信封不合规）与模型答了空串
			// （合法的信封、模型没给内容）要分开处置。
			Content *string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// complete 发出请求并拆信封。
//
// 日志级别分两档，这是本服务的一条通用纪律：
//   - 端点不可达 / 超时（E_MODEL_UNAVAILABLE / E_MODEL_TIMEOUT）记 warn：
//     它们是瞬时故障，步骤级退避重试多半能救回来，不必当成事故；
//   - 协议不合规 / 结构化校验不过（E_MODEL_PROTOCOL 等）记 error：
//     重发一次还是同样的结果，那是要人去查端点的事。
//
// 正常流转（调用成功）只在 model_call_completed 里记一条 info。
func (g *ModelGateway) complete(ctx context.Context, name SchemaName, body []byte) (out *Completion, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	callCtx := audit.CallContextFrom(ctx)
	defer func() {
		if err == nil {
			return
		}
		e := errcode.As(err)
		fields := []any{
			"run_id", callCtx.RunID,
			"step_name", callCtx.StepName,
			"schema", string(name),
			"error_code", string(e.Code),
			"error", e.Error(),
		}
		switch e.Code {
		case errcode.ModelTimeout, errcode.ModelUnavailable:
			g.logger.Warn("model_call_failed", fields...)
		default:
			g.logger.Error("model_call_failed", fields...)
		}
	}()
	// 先看一眼取消状态：调用方已经放弃的请求不发出去，
	// 免得白耗端点配额。
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		g.cfg.BaseURL+chatCompletionsPath, bytes.NewReader(body))
	if err != nil {
		return nil, errcode.Newf(errcode.Validation, "构造模型请求失败：%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+g.cfg.APIKey)
	req.Header.Set("User-Agent", gatewayUserAgent)

	started := time.Now()
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, g.transportError(ctx, err)
	}
	defer func() {
		// 读完剩余部分再关，让连接能进复用池。
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	latency := time.Since(started)
	if err != nil {
		return nil, errcode.Newf(errcode.ModelUnavailable,
			"读取模型响应体失败（HTTP %d）：%v", resp.StatusCode, err)
	}
	if len(raw) > maxResponseBytes {
		return nil, errcode.Newf(errcode.ModelProtocol,
			"模型响应体超过上限（%d 字节），疑似端点返回了非模型内容", maxResponseBytes)
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, errcode.Newf(errcode.ModelUnavailable,
			"模型端点限流（HTTP 429），可退避后重试")
	case resp.StatusCode >= 500:
		return nil, errcode.Newf(errcode.ModelUnavailable,
			"模型端点返回服务端错误（HTTP %d）", resp.StatusCode)
	case resp.StatusCode >= 400:
		// 4xx 是「端点收到了请求但不接受」——服务是活的，响应也不是一个
		// 兼容的成功信封，按协议问题记。
		return nil, errcode.Newf(errcode.ModelProtocol,
			"模型端点拒绝请求（HTTP %d）：%s", resp.StatusCode, summarizeBody(raw))
	}

	completion, err := g.parseCompletion(ctx, name, raw, latency)
	if err != nil {
		return nil, err
	}
	return attachPromptFingerprint(completion, body), nil
}

// attachPromptFingerprint 给一次调用补上提示词指纹与长度。
//
// 指纹在**发出去之前**就算好：审计层从头到尾拿不到提示词原文，
// 只有「同一段提示词会不会算出同一个指纹」这一条线索。
func attachPromptFingerprint(out *Completion, body []byte) *Completion {
	if out == nil {
		return nil
	}
	out.PromptDigest = audit.Digest(string(body))
	out.PromptChars = len([]rune(string(body)))
	return out
}

// parseCompletion 拆解响应信封。
//
// ctx 用来取回「这次调用属于哪条判定、哪一步」：网关是唯一知道
// 「发起了这次调用」的地方，但它不知道调用属于哪条判定——那是调用方的上下文。
// 带上 run_id 与 step_name，一次模型调用就能在服务日志里与判定链对上号，
// 不用再去翻审计表。
func (g *ModelGateway) parseCompletion(ctx context.Context, name SchemaName, raw []byte, latency time.Duration) (*Completion, error) {
	var env chatCompletion
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, errcode.Newf(errcode.ModelProtocol,
			"模型响应不是合法的 OpenAI-compatible 格式（JSON 解析失败）：%v；原始片段：%s",
			err, truncateForAudit(raw))
	}
	if len(env.Choices) == 0 {
		return nil, errcode.Newf(errcode.ModelProtocol,
			"模型响应缺少 choices（分码 %q）；原始片段：%s", name, truncateForAudit(raw))
	}
	choice := env.Choices[0]
	if choice.Message.Content == nil {
		return nil, errcode.Newf(errcode.ModelProtocol,
			"模型响应缺少 choices[0].message.content（分码 %q）；原始片段：%s",
			name, truncateForAudit(raw))
	}
	// 只接受「正常结束」。length 表示内容被长度上限截断（多半不是完整 JSON），
	// content_filter 表示被内容策略拦下——两种情况都不是一份可用结论。
	if choice.FinishReason != "" && choice.FinishReason != "stop" {
		return nil, errcode.Newf(errcode.ModelProtocol,
			"模型输出未正常结束（finish_reason=%q，分码 %q）：内容不完整",
			choice.FinishReason, name)
	}

	model := env.Model
	if model == "" {
		model = g.cfg.Model
	}
	out := &Completion{
		Schema:           name,
		Content:          []byte(*choice.Message.Content),
		Model:            model,
		FinishReason:     choice.FinishReason,
		PromptTokens:     env.Usage.PromptTokens,
		CompletionTokens: env.Usage.CompletionTokens,
		TotalTokens:      env.Usage.TotalTokens,
		LatencyMS:        latency.Milliseconds(),
	}
	callCtx := audit.CallContextFrom(ctx)
	g.logger.Info("model_call_completed",
		"run_id", callCtx.RunID,
		"step_name", callCtx.StepName,
		"schema", string(name),
		"model", model,
		"finish_reason", choice.FinishReason,
		"latency_ms", out.LatencyMS,
		"prompt_tokens", out.PromptTokens,
		"completion_tokens", out.CompletionTokens,
		"total_tokens", out.TotalTokens,
	)
	return out, nil
}

// transportError 把传输层失败映射到三类错误码之一。
func (g *ModelGateway) transportError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return contextError(ctxErr)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return errcode.Newf(errcode.ModelTimeout,
			"模型调用超时（超过 %s）：%v", g.cfg.Timeout, err)
	}
	return errcode.Newf(errcode.ModelUnavailable, "模型服务不可达：%v", err)
}

// contextError 把调用方取消或 deadline 到期包成错误。
//
// 同时包住 context 错误：调用方用 errors.Is(err, context.Canceled) 就能
// 分辨「是我自己取消的」与「端点慢」，而错误码仍是 E_MODEL_TIMEOUT
// （这次调用没拿到结果，处置上与超时一致：退回规则基线，状态不推进）。
func contextError(ctxErr error) error {
	base := errcode.New(errcode.ModelTimeout, "模型调用被调用方取消")
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		base = errcode.New(errcode.ModelTimeout, "模型调用超时（调用方 deadline 已到）")
	}
	return fmt.Errorf("%w: %w", base, ctxErr)
}

// summarizeBody 把响应体压成一行摘要，用于 4xx 的错误详情。
//
// 只留前若干字节并压掉换行：错误详情会进日志与接口响应，不能把整份响应
// 原文塞进去。
func summarizeBody(raw []byte) string {
	text := strings.TrimSpace(string(truncateForAudit(raw)))
	text = strings.ReplaceAll(text, "\n", " ")
	if len(text) > 200 {
		text = text[:200] + "…"
	}
	if text == "" {
		return "（响应体为空）"
	}
	return text
}
