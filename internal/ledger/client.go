// Package ledger 是冷链台账的出站读侧客户端。
//
// 它只做两件事：按点位读温度时序、按点位与时刻读在库批次清单。
// 写侧（越温记录写回台账、通知接种门诊）在另一个任务里单独实现，
// 不与读侧混在一个客户端里——读与写的失败语义、重试策略、幂等要求都不同。
//
// 三条纪律贯穿本文件：
//
//  1. **凭据只从环境变量读，缺了就不发请求。**
//     带着半份配置硬发一次，得到的 401 会掩盖真正的问题（配置没给，而不是端点坏了）。
//  2. **失败语义显式分档。**
//     调用方要靠错误码区分「退避重试即可」（台账 5xx / 超时 / 限流）与
//     「必须人去处理」（凭据被拒 401、请求本身不合法 400）。
//     混成一句「台账不可用」，重试策略就只能瞎猜。
//  3. **批次号只接受台账给的。**
//     台账返回什么批次就是什么批次；没有批次号的条目是占位，直接剔除；
//     归属点位对不上的批次整份标歧义、转人工。本服务在任何情况下都不生成新批次号。
//
// 它与源产品机制的关系：源仓库的模型单价表瘦身工具（tools/model-prices）
// 在把上游原始快照搬进产品时，只投影真正用到的字段、显式剔除「看着像数据、
// 其实是占位」的条目（文档占位键 sample_spec、两项成本都为 0 的空壳条目），
// 并且每条被剔除的条目都有注释写明为什么。搬到这里的批次读侧就是同一件事：
// **台账给的批次号原样引用，占位条目剔除，剔除的条数与原因都要留痕**，
// 绝不因为「这一栏总得有点什么」就自己补一个看起来像批次号的值。
package ledger

import (
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
	"sync"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/config"
	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
)

const (
	// seriesPathFormat 是点位温度时序的读侧路径。
	seriesPathFormat = "/api/v1/points/%s/series"
	// batchesPath 是在库批次清单的读侧路径。
	batchesPath = "/api/v1/batches"
	// clientUserAgent 是出站请求的 User-Agent。
	clientUserAgent = "coldchain-temp-assessment/0.1 (ledger-read)"
	// maxResponseBytes 是响应体上限（字节）。
	//
	// 越限多半是端点回错了东西（比如把一个网页当 JSON 返回），
	// 挡住它比把它整份塞进内存再报错安全。
	maxResponseBytes = 4 << 20
	// DefaultTimeout 是台账读侧的单次调用时限。
	DefaultTimeout = 15 * time.Second
	// DefaultCacheTTL 是读侧缓存的默认有效期。
	//
	// 取值很小：批次清单是「此刻在库什么货」，放久了就成了过期库存。
	// 缓存只用来挡住同一次判定里的重复读，不是拿来当库存快照的。
	DefaultCacheTTL = 30 * time.Second
)

// Config 是台账读侧的配置面。
//
// 基址与凭据全部来自环境变量，不带默认值：缺项时装配即失败。
type Config struct {
	// BaseURL 是冷链台账基址（不含路径）。
	BaseURL string
	// Token 是调用凭据，只从 COLDCHAIN_LEDGER_TOKEN 读。
	Token string
	// Timeout 是单次调用的时限；零值取 DefaultTimeout。
	Timeout time.Duration
	// CacheTTL 是读侧缓存有效期；零值取 DefaultCacheTTL，负值表示关闭缓存。
	CacheTTL time.Duration
}

// ConfigFromEnv 从环境变量装配台账读侧配置面。
//
// 只校验台账侧的两项，不牵连通知与模型那几项——离线的 assess / import
// 子命令同样要能起读侧，不必先把整站配置凑齐。
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		BaseURL: strings.TrimRight(strings.TrimSpace(os.Getenv(config.EnvLedgerBaseURL)), "/"),
		Token:   strings.TrimSpace(os.Getenv(config.EnvLedgerToken)),
		Timeout: DefaultTimeout,
	}
	if raw := strings.TrimSpace(os.Getenv(config.EnvLedgerTimeout)); raw != "" {
		secs, err := strconv.Atoi(raw)
		if err != nil || secs <= 0 {
			return Config{}, errcode.Newf(errcode.ConfigMissing,
				"%s 必须为正整数秒，当前取值 %q", config.EnvLedgerTimeout, raw)
		}
		cfg.Timeout = time.Duration(secs) * time.Second
	}
	if raw := strings.TrimSpace(os.Getenv(config.EnvLedgerCacheTTL)); raw != "" {
		secs, err := strconv.Atoi(raw)
		if err != nil {
			return Config{}, errcode.Newf(errcode.ConfigMissing,
				"%s 必须为整数秒（负值表示关闭缓存），当前取值 %q", config.EnvLedgerCacheTTL, raw)
		}
		cfg.CacheTTL = time.Duration(secs) * time.Second
	}
	var missing []string
	if cfg.BaseURL == "" {
		missing = append(missing, config.EnvLedgerBaseURL)
	}
	if cfg.Token == "" {
		missing = append(missing, config.EnvLedgerToken)
	}
	if len(missing) > 0 {
		return Config{}, errcode.Newf(errcode.ConfigMissing,
			"冷链台账读侧缺少必填配置项：%s（请在环境变量或 .env 中设置，详见 .env.example）",
			strings.Join(missing, "、"))
	}
	return cfg, nil
}

// Option 是客户端的可选装配项。
type Option func(*Client)

// WithHTTPClient 注入自定义 HTTP 客户端（测试与运维排障用）。
func WithHTTPClient(c *http.Client) Option {
	return func(cl *Client) {
		if c != nil {
			cl.http = c
		}
	}
}

// WithLogger 注入结构化日志器。
func WithLogger(l *logging.Logger) Option {
	return func(cl *Client) {
		if l != nil {
			cl.logger = l
		}
	}
}

// WithClock 注入时钟（测试里把缓存有效期走满用）。
func WithClock(now func() time.Time) Option {
	return func(cl *Client) {
		if now != nil {
			cl.now = now
		}
	}
}

// Client 是冷链台账的出站读侧客户端。
//
// 业务层只依赖它，不直接接触 http.Client：换台账实现只改环境变量。
type Client struct {
	cfg    Config
	http   *http.Client
	logger *logging.Logger
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
}

// cacheEntry 是一条缓存记录。
//
// 只存成功取到的结果与取到的时刻。失败从不进缓存：
// 把一次失败缓存成「有效数据」，下一次读就会把过期库存当现状写进台账。
type cacheEntry struct {
	value     any
	fetchedAt time.Time
}

// NewClient 装配台账读侧客户端。
//
// 配置不合法直接返回 E_CONFIG_MISSING：宁可起不来，也不要带着半份配置
// 发出一注定失败的请求。
func NewClient(cfg Config, opts ...Option) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, errcode.Newf(errcode.ConfigMissing,
			"冷链台账基址为空（%s 未配置）", config.EnvLedgerBaseURL)
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errcode.Newf(errcode.ConfigMissing,
			"冷链台账凭据为空（%s 未配置）：不发匿名请求", config.EnvLedgerToken)
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errcode.Newf(errcode.ConfigMissing,
			"冷链台账基址不合法：%q（须为带协议与主机名的绝对地址）", cfg.BaseURL)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	cacheTTL := cfg.CacheTTL
	if cacheTTL == 0 {
		cacheTTL = DefaultCacheTTL
	}
	cl := &Client{
		cfg: Config{
			BaseURL:  base,
			Token:    cfg.Token,
			Timeout:  timeout,
			CacheTTL: cacheTTL,
		},
		logger: logging.Default(),
		now:    time.Now,
		cache:  map[string]cacheEntry{},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(cl)
		}
	}
	if cl.http == nil {
		cl.http = &http.Client{Timeout: cl.cfg.Timeout}
	}
	return cl, nil
}

// BaseURL 返回台账基址（不含凭据）。
func (c *Client) BaseURL() string { return c.cfg.BaseURL }

// seriesEnvelope 是台账时序响应信封。
//
// 台账侧可能回 {"point_id":…,"samples":[…]}，也可能直接回一个数组；
// 两种都收，但只投影产品真正用到的字段（measured_at / temperature_c /
// humidity_pct / accuracy_c），其余字段忽略而不报错。
type seriesEnvelope struct {
	PointID string         `json:"point_id"`
	Samples []ledgerSample `json:"samples"`
	Data    []ledgerSample `json:"data"`
}

// ledgerSample 是台账时序里的一条读数。
type ledgerSample struct {
	MeasuredAt   string   `json:"measured_at"`
	TemperatureC *float64 `json:"temperature_c"`
	HumidityPct  *float64 `json:"humidity_pct"`
	AccuracyC    *float64 `json:"accuracy_c"`
}

// batchesEnvelope 是台账在库批次响应信封。
type batchesEnvelope struct {
	Batches []ledgerBatch `json:"batches"`
	Data    []ledgerBatch `json:"data"`
}

// ledgerBatch 是台账在库清单里的一条批次。
//
// PointID 可空：台账按点位查出来的清单通常不逐条带点位归属。
type ledgerBatch struct {
	PointID     string `json:"point_id"`
	BatchRef    string `json:"batch_ref"`
	ProductName string `json:"product_name"`
	Quantity    int    `json:"quantity"`
	ExpiryDate  string `json:"expiry_date"`
}

// Series 读点位温度时序。
//
// from / to 是取数区间，两端闭合。返回的读数按采样时刻升序。
//
// 失败语义：超时 / 连接失败 / 5xx / 429 → E_LEDGER_UNAVAILABLE（可退避重试）；
// 401 / 403 → E_UNAUTHORIZED（凭据要去改，重试无用）；404 → E_NOT_FOUND；
// 400 → E_VALIDATION；响应不是合法 JSON → E_LEDGER_PROTOCOL。
func (c *Client) Series(ctx context.Context, pointID string, from, to time.Time) ([]domain.TemperatureEvent, error) {
	pointID = strings.TrimSpace(pointID)
	if pointID == "" {
		return nil, errcode.Newf(errcode.Validation, "读取台账时序缺少点位标识")
	}
	key := fmt.Sprintf("series|%s|%s|%s", pointID, from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	if cached, ok := c.cacheGet(key); ok {
		if samples, ok := cached.([]domain.TemperatureEvent); ok {
			return samples, nil
		}
	}

	path := fmt.Sprintf(seriesPathFormat, url.PathEscape(pointID))
	query := url.Values{}
	query.Set("from", from.Format(time.RFC3339))
	query.Set("to", to.Format(time.RFC3339))

	raw, err := c.get(ctx, path, query)
	if err != nil {
		c.cacheInvalidate(key)
		return nil, err
	}

	samples, err := decodeSeries(raw)
	if err != nil {
		c.cacheInvalidate(key)
		return nil, err
	}
	c.cachePut(key, samples)
	return samples, nil
}

// Batches 读在库批次清单。
//
// at 是查库存的时刻（本次判定取越温窗口起点）。返回的批次原样引用台账给的批次号。
//
// 返回值与错误同时有意义：
//   - 返回值里的 Status 说明这次查询到底怎么样（查成了 / 确实为空 / 没查成 / 有歧义），
//     Detail 说明为什么；调用方据此决定成稿怎么写。
//   - 错误用于记录与分流（可重试 / 要人去改凭据）。
//
// 批次权威性约束在本方法内落地：
//   - 没有批次号的条目是占位，剔除并计数（不生成新号顶替）；
//   - 归属点位与查询点位对不上的批次不并入清单，整份结果标歧义、转人工；
//   - 台账不可达或响应不合法时返回「没查成」，绝不用空清单冒充查到了。
func (c *Client) Batches(ctx context.Context, pointID string, at time.Time) (domain.BatchLookup, error) {
	pointID = strings.TrimSpace(pointID)
	if pointID == "" {
		return domain.BatchLookup{}, errcode.Newf(errcode.Validation, "读取台账在库批次缺少点位标识")
	}
	key := fmt.Sprintf("batches|%s|%s", pointID, at.UTC().Format(time.RFC3339))
	if cached, ok := c.cacheGet(key); ok {
		if lookup, ok := cached.(domain.BatchLookup); ok {
			return lookup, nil
		}
	}

	query := url.Values{}
	query.Set("point_id", pointID)
	query.Set("at", at.Format(time.RFC3339))

	raw, err := c.get(ctx, batchesPath, query)
	if err != nil {
		c.cacheInvalidate(key)
		lookup := domain.NewBatchLookupUnavailable(failureDetail(err), at)
		c.logger.Warn("ledger_batches_unavailable",
			"point_id", pointID,
			"at", at.Format(time.RFC3339),
			"error_code", string(errcode.As(err).Code),
			"detail", lookup.Detail,
		)
		return lookup, err
	}

	rawBatches, err := decodeBatches(raw)
	if err != nil {
		c.cacheInvalidate(key)
		lookup := domain.NewBatchLookupUnavailable(failureDetail(err), at)
		return lookup, err
	}

	kept, placeholders, foreign := partitionBatches(rawBatches, pointID)
	switch {
	case len(foreign) > 0:
		lookup := domain.NewBatchLookupAmbiguous(kept, fmt.Sprintf(
			"冷链台账按点位 %s 查出的在库批次里有 %d 条登记在别的点位（%s），批次归属不可信，未并入本次清单，转人工核对",
			pointID, len(foreign), strings.Join(foreign, "、")), at)
		c.logger.Warn("ledger_batches_ambiguous",
			"point_id", pointID,
			"foreign_count", len(foreign),
			"foreign_point_ids", strings.Join(foreign, "、"),
		)
		return lookup, nil
	case placeholders > 0:
		c.logger.Warn("ledger_batches_placeholder_skipped",
			"point_id", pointID,
			"skipped", placeholders,
		)
	}

	lookup := domain.NewBatchLookupOk(kept, at)
	c.cachePut(key, lookup)
	return lookup, nil
}

// partitionBatches 把台账返回的批次分成三类：可用的、占位的（无批次号）、
// 归属别的点位的（歧义）。
//
// 批次号原样保留：既不改写大小写、也不去空格、更不生成新号。
func partitionBatches(raw []ledgerBatch, pointID string) (kept []domain.Batch, placeholders int, foreignPointIDs []string) {
	for _, item := range raw {
		if strings.TrimSpace(item.BatchRef) == "" {
			// 占位条目：台账侧没给批次号。它不是一批货，剔除并计数。
			placeholders++
			continue
		}
		if itemPoint := strings.TrimSpace(item.PointID); itemPoint != "" && itemPoint != pointID {
			foreignPointIDs = append(foreignPointIDs, itemPoint)
			continue
		}
		kept = append(kept, domain.Batch{
			PointID:     pointID,
			BatchRef:    item.BatchRef,
			ProductName: item.ProductName,
			Quantity:    item.Quantity,
			ExpiryDate:  item.ExpiryDate,
		})
	}
	return kept, placeholders, foreignPointIDs
}

// get 发一次读请求并返回响应体。
func (c *Client) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}
	target := c.cfg.BaseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, errcode.Newf(errcode.Validation, "构造台账读请求失败：%v", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("User-Agent", clientUserAgent)

	started := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.transportError(ctx, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, errcode.Newf(errcode.LedgerUnavailable,
			"读取冷链台账响应体失败（HTTP %d）：%v", resp.StatusCode, err)
	}
	if len(raw) > maxResponseBytes {
		return nil, errcode.Newf(errcode.LedgerProtocol,
			"冷链台账响应体超过上限（%d 字节），疑似端点返回了非台账内容", maxResponseBytes)
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, errcode.Newf(errcode.Unauthorized,
			"冷链台账拒绝了本服务的凭据（HTTP %d）：请核对 %s", resp.StatusCode, config.EnvLedgerToken)
	case resp.StatusCode == http.StatusNotFound:
		return nil, errcode.Newf(errcode.NotFound,
			"冷链台账上没有该对象（HTTP %d）：%s", resp.StatusCode, path)
	case resp.StatusCode == http.StatusBadRequest, resp.StatusCode == http.StatusUnprocessableEntity:
		return nil, errcode.Newf(errcode.Validation,
			"冷链台账拒绝了本次读请求（HTTP %d）：%s", resp.StatusCode, summarizeBody(raw))
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, errcode.Newf(errcode.LedgerUnavailable,
			"冷链台账限流（HTTP 429），可退避后重试")
	case resp.StatusCode >= 500:
		return nil, errcode.Newf(errcode.LedgerUnavailable,
			"冷链台账返回服务端错误（HTTP %d），可退避后重试", resp.StatusCode)
	}
	c.logger.Info("ledger_read_completed",
		"path", path,
		"status", resp.StatusCode,
		"latency_ms", time.Since(started).Milliseconds(),
	)
	return raw, nil
}

// transportError 把传输层失败映射到错误码。
func (c *Client) transportError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return contextError(ctxErr)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return errcode.Newf(errcode.LedgerUnavailable,
			"冷链台账读侧调用超时（超过 %s）：%v", c.cfg.Timeout, err)
	}
	return errcode.Newf(errcode.LedgerUnavailable, "冷链台账不可达：%v", err)
}

// contextError 把调用方取消或 deadline 到期包成错误。
//
// 错误码仍是 E_LEDGER_UNAVAILABLE：这次调用没拿到结果，
// 处置上与「台账不可达」一致（读侧是批次信息缺失转人工核对，
// 写侧是这条越温还没入账、挂起待补偿）。
func contextError(ctxErr error) error {
	base := errcode.New(errcode.LedgerUnavailable, "冷链台账调用被调用方取消")
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		base = errcode.New(errcode.LedgerUnavailable, "冷链台账调用超时（调用方 deadline 已到）")
	}
	return fmt.Errorf("%w: %w", base, ctxErr)
}

// decodeSeries 拆台账的时序响应，只投影产品真正用到的字段。
func decodeSeries(raw []byte) ([]domain.TemperatureEvent, error) {
	var envelope seriesEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		// 台账可能直接回一个数组；两种形状都收。
		var list []ledgerSample
		if err2 := json.Unmarshal(raw, &list); err2 != nil {
			return nil, errcode.Newf(errcode.LedgerProtocol,
				"冷链台账时序响应不是合法 JSON：%v", err)
		}
		envelope.Samples = list
	}
	items := envelope.Samples
	if items == nil {
		items = envelope.Data
	}
	samples := make([]domain.TemperatureEvent, 0, len(items))
	for i, item := range items {
		if item.TemperatureC == nil {
			return nil, errcode.Newf(errcode.LedgerProtocol,
				"冷链台账时序第 %d 条读数缺少 temperature_c", i+1)
		}
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(item.MeasuredAt))
		if err != nil {
			return nil, errcode.Newf(errcode.LedgerProtocol,
				"冷链台账时序第 %d 条读数的 measured_at 不是 RFC3339 时间：%q", i+1, item.MeasuredAt)
		}
		event := domain.TemperatureEvent{
			MeasuredAt:   at,
			TemperatureC: *item.TemperatureC,
			Source:       "recorder",
		}
		if item.HumidityPct != nil {
			humidity := *item.HumidityPct
			event.HumidityPct = &humidity
		}
		if item.AccuracyC != nil {
			accuracy := *item.AccuracyC
			event.AccuracyC = &accuracy
		}
		samples = append(samples, event)
	}
	return samples, nil
}

// decodeBatches 拆台账的在库批次响应。
func decodeBatches(raw []byte) ([]ledgerBatch, error) {
	var envelope batchesEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		var list []ledgerBatch
		if err2 := json.Unmarshal(raw, &list); err2 != nil {
			return nil, errcode.Newf(errcode.LedgerProtocol,
				"冷链台账在库批次响应不是合法 JSON：%v", err)
		}
		envelope.Batches = list
	}
	items := envelope.Batches
	if items == nil {
		items = envelope.Data
	}
	return items, nil
}

// failureDetail 从错误里取一句能写进成稿的失败原因。
//
// 成稿上要写「批次信息缺失」并说清为什么：只写「缺失」而不写原因，
// 复核的人还得自己去翻日志才知道是台账挂了还是凭据过期了。
func failureDetail(err error) string {
	if err == nil {
		return "冷链台账读侧未返回结果"
	}
	e := errcode.As(err)
	if e.Details == "" {
		return e.Message
	}
	// 细节里常常已经把错误码的中文含义带上了（"冷链台账不可达：…"），
	// 这时再拼一遍就成了「冷链台账不可达：冷链台账不可达：…」。
	if strings.HasPrefix(e.Details, e.Message) {
		return e.Details
	}
	return e.Message + "：" + e.Details
}

// summarizeBody 把响应体压成一行摘要，用于 4xx 的错误详情。
func summarizeBody(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	text = strings.ReplaceAll(text, "\n", " ")
	if len(text) > 200 {
		text = text[:200] + "…"
	}
	if text == "" {
		return "（响应体为空）"
	}
	return text
}

// cacheGet 取缓存里没过期的结果。
func (c *Client) cacheGet(key string) (any, bool) {
	if c.cfg.CacheTTL <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.cache[key]
	if !ok {
		return nil, false
	}
	if c.now().Sub(entry.fetchedAt) > c.cfg.CacheTTL {
		// 过期即失效：库存清单放久了就是过期库存，不能当现状用。
		delete(c.cache, key)
		return nil, false
	}
	return entry.value, true
}

// cachePut 存一次成功取到的结果。
func (c *Client) cachePut(key string, value any) {
	if c.cfg.CacheTTL <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache[key] = cacheEntry{value: value, fetchedAt: c.now()}
}

// cacheInvalidate 作废一条缓存。
//
// 读失败时调用：一次失败之后，缓存里那条「上一次读到的清单」也不能再当现状用，
// 否则会把过期库存写进台账。失败本身从不入缓存。
func (c *Client) cacheInvalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cache, key)
}
