// 本文件落通知系统的**出站写侧**：把「这次越温已经定处置了」这件事
// 通知到预防接种门诊与药事。
//
// 为什么通知与台账写分成两处目标、两次调用：台账是**账**，
// 通知是**找人**。账写成了不等于人知道了——台账里躺着一条越温记录，
// 而接种门诊照常把疫苗打出去，这是冷链事故里最常见的一种形态。
// 反过来，通知发出去了而台账没写上，账就少了一条。两件事都得做，
// 而且得分别记账（见 domain.WritebackRecord）。
//
// 三条纪律贯穿本文件：
//
//  1. **凭据只从环境变量读，缺了就不发请求。**
//  2. **消息带 idempotency_key**：通知系统据此认出「同一条越温的同一次通知」，
//     补发重发都不会让门诊收到两条一样的消息。
//  3. **正文是那张异常情况记录表**：通知正文不重新组织一遍事实，
//     直接带上确认时定稿的记录表原文——门诊与药事拿到的就是同一份东西。
package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/config"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
)

const (
	// messagesPath 是通知系统的消息写入路径。
	messagesPath = "/api/v1/messages"
	// notifyUserAgent 是出站通知请求的 User-Agent。
	notifyUserAgent = "coldchain-temp-assessment/0.1 (notify-write)"
	// DefaultNotifyTimeout 是通知侧的单次调用时限。
	DefaultNotifyTimeout = 10 * time.Second
	// DefaultNotifyChannel 是通知渠道的默认值。
	//
	// 「冷链温控越温」是院内通知系统里的一条固定渠道：收到这一类消息的人
	// 都知道是什么事。它不是「每次都填一遍的表单字段」，所以给默认值。
	DefaultNotifyChannel = "coldchain_deviation"
	// DefaultNotifyAudience 是通知对象的默认值：预防接种门诊。
	DefaultNotifyAudience = "vaccination_clinic"
)

// NotifyConfig 是通知系统的配置面。
//
// 基址与凭据全部来自环境变量，不带默认值：缺项时装配即失败。
// 渠道与通知对象可以按院内的实际约定覆盖，不给就用默认值。
type NotifyConfig struct {
	// BaseURL 是通知系统基址（不含路径）。
	BaseURL string
	// Token 是调用凭据，只从 COLDCHAIN_NOTIFY_TOKEN 读。
	Token string
	// Timeout 是单次调用的时限；零值取 DefaultNotifyTimeout。
	Timeout time.Duration
	// Channel 是通知渠道；空值取 DefaultNotifyChannel。
	Channel string
	// Audience 是通知对象；空值取 DefaultNotifyAudience。
	Audience string
}

// NotifyConfigFromEnv 从环境变量装配通知侧配置面。
func NotifyConfigFromEnv() (NotifyConfig, error) {
	cfg := NotifyConfig{
		BaseURL:  strings.TrimRight(strings.TrimSpace(os.Getenv(config.EnvNotifyBaseURL)), "/"),
		Token:    strings.TrimSpace(os.Getenv(config.EnvNotifyToken)),
		Timeout:  DefaultNotifyTimeout,
		Channel:  strings.TrimSpace(os.Getenv(config.EnvNotifyChannel)),
		Audience: strings.TrimSpace(os.Getenv(config.EnvNotifyAudience)),
	}
	if raw := strings.TrimSpace(os.Getenv(config.EnvNotifyTimeout)); raw != "" {
		secs, err := strconv.Atoi(raw)
		if err != nil || secs <= 0 {
			return NotifyConfig{}, errcode.Newf(errcode.ConfigMissing,
				"%s 必须为正整数秒，当前取值 %q", config.EnvNotifyTimeout, raw)
		}
		cfg.Timeout = time.Duration(secs) * time.Second
	}
	var missing []string
	if cfg.BaseURL == "" {
		missing = append(missing, config.EnvNotifyBaseURL)
	}
	if cfg.Token == "" {
		missing = append(missing, config.EnvNotifyToken)
	}
	if len(missing) > 0 {
		return NotifyConfig{}, errcode.Newf(errcode.ConfigMissing,
			"通知系统缺少必填配置项：%s（请在环境变量或 .env 中设置，详见 .env.example）",
			strings.Join(missing, "、"))
	}
	return cfg, nil
}

// Message 是发给通知系统的一条消息。
//
// 字段与 spec 的出站字段规范逐项对应：
// channel / audience / title / body / related_run_id / idempotency_key。
type Message struct {
	// Channel 是通知渠道；空值由客户端补成默认渠道。
	Channel string `json:"channel"`
	// Audience 是通知对象；空值由客户端补成默认通知对象。
	Audience string `json:"audience"`
	// Title 是消息标题。
	Title string `json:"title"`
	// Body 是消息正文；装的是确认时定稿的异常情况记录表。
	Body string `json:"body"`
	// RelatedRunID 是这条消息对应的判定 run。
	RelatedRunID string `json:"related_run_id"`
	// IdempotencyKey 是消息幂等键；通知系统据此认出重复消息。
	IdempotencyKey string `json:"idempotency_key"`
}

// Notifier 把一条消息发给通知系统。
//
// 业务层只依赖它，不直接接触 http.Client：换通知系统只改环境变量。
type Notifier struct {
	cfg    NotifyConfig
	http   *http.Client
	logger *logging.Logger
}

// NotifyOption 是通知客户端的可选装配项。
type NotifyOption func(*Notifier)

// WithNotifyHTTPClient 注入自定义 HTTP 客户端（测试与运维排障用）。
func WithNotifyHTTPClient(c *http.Client) NotifyOption {
	return func(n *Notifier) {
		if c != nil {
			n.http = c
		}
	}
}

// WithNotifyLogger 注入结构化日志器。
func WithNotifyLogger(l *logging.Logger) NotifyOption {
	return func(n *Notifier) {
		if l != nil {
			n.logger = l
		}
	}
}

// NewNotifier 装配通知客户端。
func NewNotifier(cfg NotifyConfig, opts ...NotifyOption) (*Notifier, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, errcode.Newf(errcode.ConfigMissing,
			"通知系统基址为空（%s 未配置）", config.EnvNotifyBaseURL)
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errcode.Newf(errcode.ConfigMissing,
			"通知系统凭据为空（%s 未配置）：不发匿名请求", config.EnvNotifyToken)
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errcode.Newf(errcode.ConfigMissing,
			"通知系统基址不合法：%q（须为带协议与主机名的绝对地址）", cfg.BaseURL)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultNotifyTimeout
	}
	channel := strings.TrimSpace(cfg.Channel)
	if channel == "" {
		channel = DefaultNotifyChannel
	}
	audience := strings.TrimSpace(cfg.Audience)
	if audience == "" {
		audience = DefaultNotifyAudience
	}
	n := &Notifier{
		cfg: NotifyConfig{
			BaseURL:  base,
			Token:    cfg.Token,
			Timeout:  timeout,
			Channel:  channel,
			Audience: audience,
		},
		logger: logging.Default(),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(n)
		}
	}
	if n.http == nil {
		n.http = &http.Client{Timeout: n.cfg.Timeout}
	}
	return n, nil
}

// BaseURL 返回通知系统基址（不含凭据）。
func (n *Notifier) BaseURL() string { return n.cfg.BaseURL }

// Channel 返回本客户端使用的通知渠道。
func (n *Notifier) Channel() string { return n.cfg.Channel }

// Audience 返回本客户端使用的通知对象。
func (n *Notifier) Audience() string { return n.cfg.Audience }

// Send 发一条消息。
//
// 失败语义与台账写侧同一套分档：
//   - 超时 / 连接失败 / 5xx / 429 → E_NOTIFY_UNAVAILABLE（可退避重试）；
//   - 401 / 403 → E_UNAUTHORIZED（凭据要去改，重试无用）；
//   - 400 / 422 → E_VALIDATION（消息本身不合法，要人看）。
//
// 例外一档：**409 视为「通知系统已认定这条消息发过了」**，按成功返回。
// 与台账侧同一个道理：判重成功的重复消息不该让补偿重放一遍又一遍。
func (n *Notifier) Send(ctx context.Context, msg Message) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return contextError(err)
	}
	if strings.TrimSpace(msg.Title) == "" {
		return errcode.Newf(errcode.Validation, "通知缺少标题")
	}
	if strings.TrimSpace(msg.IdempotencyKey) == "" {
		return errcode.Newf(errcode.Validation, "通知缺少 idempotency_key")
	}
	if strings.TrimSpace(msg.Channel) == "" {
		msg.Channel = n.cfg.Channel
	}
	if strings.TrimSpace(msg.Audience) == "" {
		msg.Audience = n.cfg.Audience
	}

	body, err := json.Marshal(msg)
	if err != nil {
		return errcode.Newf(errcode.Validation, "序列化通知消息失败：%v", err)
	}
	if len(body) > maxWriteBodyBytes {
		return errcode.Newf(errcode.Validation, "通知消息体超过上限（%d 字节）", maxWriteBodyBytes)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.cfg.BaseURL+messagesPath, bytes.NewReader(body))
	if err != nil {
		return errcode.Newf(errcode.Validation, "构造通知请求失败：%v", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+n.cfg.Token)
	req.Header.Set("User-Agent", notifyUserAgent)

	started := time.Now()
	resp, err := n.http.Do(req)
	if err != nil {
		return n.transportError(ctx, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxWriteBodyBytes+1))
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return errcode.Newf(errcode.Unauthorized,
			"通知系统拒绝了本服务的凭据（HTTP %d）：请核对 %s", resp.StatusCode, config.EnvNotifyToken)
	case resp.StatusCode == http.StatusBadRequest, resp.StatusCode == http.StatusUnprocessableEntity:
		return errcode.Newf(errcode.Validation,
			"通知系统拒绝了本次消息（HTTP %d）：%s", resp.StatusCode, summarizeBody(raw))
	case resp.StatusCode == http.StatusConflict:
		n.logger.Warn("notify_message_duplicate",
			"related_run_id", msg.RelatedRunID,
			"idempotency_key", msg.IdempotencyKey,
			"status", resp.StatusCode,
		)
		return nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return errcode.Newf(errcode.NotifyUnavailable,
			"通知系统限流（HTTP 429），可退避后重试")
	case resp.StatusCode >= 500:
		return errcode.Newf(errcode.NotifyUnavailable,
			"通知系统返回服务端错误（HTTP %d），可退避后重试", resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return errcode.Newf(errcode.NotifyProtocol,
			"通知系统对消息写入返回了预期外的状态码（HTTP %d）：%s",
			resp.StatusCode, summarizeBody(raw))
	}

	n.logger.Info("notify_message_sent",
		"channel", msg.Channel,
		"audience", msg.Audience,
		"related_run_id", msg.RelatedRunID,
		"idempotency_key", msg.IdempotencyKey,
		"status", resp.StatusCode,
		"latency_ms", time.Since(started).Milliseconds(),
	)
	return nil
}

// transportError 把传输层失败映射到错误码。
func (n *Notifier) transportError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return contextError(ctxErr)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return errcode.Newf(errcode.NotifyUnavailable,
			"通知系统调用超时（超过 %s）：%v", n.cfg.Timeout, err)
	}
	return errcode.Newf(errcode.NotifyUnavailable, "通知系统不可达：%v", err)
}
