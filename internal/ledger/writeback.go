// 本文件落冷链台账的**出站写侧**：把一条已由药学部主管确认的越温偏差
// 写回院内冷链台账。
//
// 为什么写侧与读侧分开成两个客户端（client.go 是读侧）：读与写的失败语义、
// 重试策略、幂等要求都不同。读侧失败最多是「批次信息缺失，转人工核对」；
// 写侧失败是「台账里没有这条越温」——药事那边会以为一切正常。
// 把两者混在一个客户端里，重试策略就只好一刀切。
//
// 三条纪律贯穿本文件：
//
//  1. **凭据只从环境变量读，缺了就不发请求。**
//     带着半份配置硬发一次，得到的 401 会掩盖真正的问题。
//  2. **请求体带 external_ref 与 idempotency_key。**
//     台账侧按 external_ref 识别「同一次越温」，按 idempotency_key 防重复入账。
//     重放（补偿重放、接口重试）发出的是同一个键，台账认得出这是同一条，
//     不会在台账里留下两条一模一样的越温记录。
//  3. **载荷是主管确认那一刻固化的那份原文，不在写回时重算。**
//     重算就等于台账里躺着的是另一份结论——主管签的字与台账上的内容对不上。
//
// 它与源产品机制的关系：源仓库的模型单价表瘦身工具（tools/model-prices）
// 在把上游原始快照搬进产品时只投影真正用到的字段、显式剔除「看着像数据、
// 其实是占位」的条目，并且每条被剔除的条目都有注释写明为什么。
// 搬到这里的出站写侧是同一件事：**台账要什么就给什么，多余的字段不带**，
// 批次号原样引用台账给的，系统一个都不新造。
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
	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
)

const (
	// deviationsPath 是冷链台账的越温偏差写入路径。
	deviationsPath = "/api/v1/cold-chain/deviations"
	// writebackUserAgent 是出站写请求的 User-Agent。
	writebackUserAgent = "coldchain-temp-assessment/0.1 (ledger-write)"
	// DefaultWritebackTimeout 是台账写侧的单次调用时限。
	DefaultWritebackTimeout = 15 * time.Second
	// maxWriteBodyBytes 是请求体上限（字节）。
	//
	// 记录表正文可能很长（带数据质量提示、批次清单、确认小节），
	// 但它有确定的量级；越限多半是拼错了东西，挡住比整份塞进内存安全。
	maxWriteBodyBytes = 8 << 20
)

// WritebackConfig 是冷链台账写侧的配置面。
//
// 基址与凭据全部来自环境变量，不带默认值：缺项时装配即失败。
type WritebackConfig struct {
	// BaseURL 是冷链台账基址（不含路径）。
	BaseURL string
	// Token 是调用凭据，只从 COLDCHAIN_LEDGER_TOKEN 读。
	Token string
	// Timeout 是单次调用的时限；零值取 DefaultWritebackTimeout。
	Timeout time.Duration
}

// WritebackConfigFromEnv 从环境变量装配台账写侧配置面。
//
// 与读侧共用同一组台账环境变量：同一个台账、同一个身份，
// 读侧与写侧分开只是因为失败语义不同，不代表它们连的是两套系统。
func WritebackConfigFromEnv() (WritebackConfig, error) {
	cfg := WritebackConfig{
		BaseURL: strings.TrimRight(strings.TrimSpace(os.Getenv(config.EnvLedgerBaseURL)), "/"),
		Token:   strings.TrimSpace(os.Getenv(config.EnvLedgerToken)),
		Timeout: DefaultWritebackTimeout,
	}
	if raw := strings.TrimSpace(os.Getenv(config.EnvLedgerTimeout)); raw != "" {
		secs, err := strconv.Atoi(raw)
		if err != nil || secs <= 0 {
			return WritebackConfig{}, errcode.Newf(errcode.ConfigMissing,
				"%s 必须为正整数秒，当前取值 %q", config.EnvLedgerTimeout, raw)
		}
		cfg.Timeout = time.Duration(secs) * time.Second
	}
	var missing []string
	if cfg.BaseURL == "" {
		missing = append(missing, config.EnvLedgerBaseURL)
	}
	if cfg.Token == "" {
		missing = append(missing, config.EnvLedgerToken)
	}
	if len(missing) > 0 {
		return WritebackConfig{}, errcode.Newf(errcode.ConfigMissing,
			"冷链台账写侧缺少必填配置项：%s（请在环境变量或 .env 中设置，详见 .env.example）",
			strings.Join(missing, "、"))
	}
	return cfg, nil
}

// DeviationRecord 是写回冷链台账的越温偏差请求体。
//
// 字段与 spec 的出站字段规范逐项对应，一项不多一项不少：
// external_ref / point_id / device_name / excursion_started_at /
// excursion_ended_at / peak_temperature_c / duration_minutes / zone /
// batches[] / cause_conclusion / evidence[] / disposition /
// confirmed_by / confirmed_at / exception_record，外加幂等键 idempotency_key。
//
// 批次原样引用台账给的批次号：系统不生成新号，也不改写台账给的值。
type DeviationRecord struct {
	// ExternalRef 是这次越温的外部引用；台账按它识别「同一次越温」。
	ExternalRef string `json:"external_ref"`
	// IdempotencyKey 是这次越温的幂等键；台账按它防重复入账。
	IdempotencyKey string `json:"idempotency_key"`
	// PointID 是越温点位。
	PointID string `json:"point_id"`
	// DeviceName 是点位上的记录仪名称。
	DeviceName string `json:"device_name"`
	// ExcursionStartedAt 是越限起点。
	ExcursionStartedAt time.Time `json:"excursion_started_at"`
	// ExcursionEndedAt 是越限终点。
	ExcursionEndedAt time.Time `json:"excursion_ended_at"`
	// PeakTemperatureC 是窗口峰值（℃）。
	PeakTemperatureC float64 `json:"peak_temperature_c"`
	// DurationMinutes 是越限持续分钟数。
	DurationMinutes int `json:"duration_minutes"`
	// Zone 是该点位适用的温区。
	Zone string `json:"zone"`
	// Batches 是越温时在库的批次；台账查不到时为空，不编造。
	Batches []domain.Batch `json:"batches"`
	// CauseConclusion 是原因结论（原因码）。
	CauseConclusion string `json:"cause_conclusion"`
	// Evidence 是支撑原因结论的证据。
	Evidence []domain.AttributionEvidence `json:"evidence"`
	// Disposition 是主管确认后的最终处置结论。
	Disposition string `json:"disposition"`
	// ConfirmedBy 是确认人（药学部主管标识）。
	ConfirmedBy string `json:"confirmed_by"`
	// ConfirmedAt 是确认时刻。
	ConfirmedAt time.Time `json:"confirmed_at"`
	// ExceptionRecord 是《疫苗储存和运输温度异常情况记录表》正文。
	ExceptionRecord string `json:"exception_record"`
}

// DecodeDeviation 把确认时固化的载荷原文还原成出站请求体。
//
// 它认的是**已固化的载荷**，不是现算的结果——写回侧发出去的必须是主管
// 确认那一刻定下的内容。载荷缺 external_ref 或 point_id 一律拒绝：
// 台账侧靠这两栏认出「这是哪次越温、哪个点位」，缺了就是一条查不回来的记录。
func DecodeDeviation(payloadJSON string) (DeviationRecord, error) {
	trimmed := strings.TrimSpace(payloadJSON)
	if trimmed == "" {
		return DeviationRecord{}, errcode.Newf(errcode.Validation,
			"写回载荷为空：判定尚未确认或确认记录里没有固化载荷")
	}
	var rec DeviationRecord
	if err := json.Unmarshal([]byte(trimmed), &rec); err != nil {
		return DeviationRecord{}, errcode.Newf(errcode.Validation,
			"写回载荷不是合法 JSON：%v", err)
	}
	if strings.TrimSpace(rec.ExternalRef) == "" {
		return DeviationRecord{}, errcode.Newf(errcode.Validation,
			"写回载荷缺少 external_ref：台账侧无法据此识别这次越温")
	}
	if strings.TrimSpace(rec.PointID) == "" {
		return DeviationRecord{}, errcode.Newf(errcode.Validation,
			"写回载荷缺少 point_id：台账侧无法据此归到点位")
	}
	return rec, nil
}

// DeviationWriter 把越温偏差写回冷链台账。
//
// 业务层只依赖它，不直接接触 http.Client：换台账实现只改环境变量。
type DeviationWriter struct {
	cfg    WritebackConfig
	http   *http.Client
	logger *logging.Logger
}

// WritebackOption 是台账写侧客户端的可选装配项。
//
// 与读侧的 Option 分开声明：两个客户端的字段不同，共用一个 Option 类型
// 会逼着读侧的注入项出现在写侧（或反过来），迟早用错。
type WritebackOption func(*DeviationWriter)

// WithWritebackHTTPClient 注入自定义 HTTP 客户端（测试与运维排障用）。
func WithWritebackHTTPClient(c *http.Client) WritebackOption {
	return func(w *DeviationWriter) {
		if c != nil {
			w.http = c
		}
	}
}

// WithWritebackLogger 注入结构化日志器。
func WithWritebackLogger(l *logging.Logger) WritebackOption {
	return func(w *DeviationWriter) {
		if l != nil {
			w.logger = l
		}
	}
}

// NewDeviationWriter 装配台账写侧客户端。
//
// 配置不合法直接返回 E_CONFIG_MISSING：宁可起不来，也不要带着半份配置
// 发出一注定失败的请求——写侧的失败比读侧的失败贵得多。
func NewDeviationWriter(cfg WritebackConfig, opts ...WritebackOption) (*DeviationWriter, error) {
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
		timeout = DefaultWritebackTimeout
	}
	w := &DeviationWriter{
		cfg:    WritebackConfig{BaseURL: base, Token: cfg.Token, Timeout: timeout},
		logger: logging.Default(),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(w)
		}
	}
	if w.http == nil {
		w.http = &http.Client{Timeout: w.cfg.Timeout}
	}
	return w, nil
}

// BaseURL 返回台账基址（不含凭据）。
func (w *DeviationWriter) BaseURL() string { return w.cfg.BaseURL }

// Write 把一条越温偏差写回冷链台账。
//
// 失败语义与读侧同一套分档：
//   - 超时 / 连接失败 / 5xx / 429 → E_LEDGER_UNAVAILABLE（可退避重试）；
//   - 401 / 403 → E_UNAUTHORIZED（凭据要去改，重试无用）；
//   - 400 / 422 → E_VALIDATION（请求本身不合法，要人看）；
//   - 404 → E_NOT_FOUND（台账上没有这个点位）。
//
// 例外一档：**409 视为「台账侧已认定这条越温入过账」**，按成功返回。
// 台账按 external_ref / idempotency_key 判重，冲突就是它认出了重复；
// 这时再报失败只会让补偿重放一遍又一遍，而台账里始终是同一条记录。
func (w *DeviationWriter) Write(ctx context.Context, rec DeviationRecord) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return contextError(err)
	}
	if strings.TrimSpace(rec.ExternalRef) == "" {
		return errcode.Newf(errcode.Validation, "写回越温偏差缺少 external_ref")
	}
	if strings.TrimSpace(rec.PointID) == "" {
		return errcode.Newf(errcode.Validation, "写回越温偏差缺少 point_id")
	}

	// 数组字段一律发 []，不发 null。
	//
	// 台账侧按栏位取值，「这次越温不涉及任何批次」是一个**空清单**，
	// 不是「这一栏没有值」——后者在严格的 JSON Schema 校验下会被拒收，
	// 整条越温记录就写不进去了。
	if rec.Batches == nil {
		rec.Batches = []domain.Batch{}
	}
	if rec.Evidence == nil {
		rec.Evidence = []domain.AttributionEvidence{}
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return errcode.Newf(errcode.Validation, "序列化越温偏差失败：%v", err)
	}
	if len(body) > maxWriteBodyBytes {
		return errcode.Newf(errcode.Validation,
			"越温偏差请求体超过上限（%d 字节）", maxWriteBodyBytes)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.BaseURL+deviationsPath, bytes.NewReader(body))
	if err != nil {
		return errcode.Newf(errcode.Validation, "构造台账写请求失败：%v", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+w.cfg.Token)
	req.Header.Set("User-Agent", writebackUserAgent)

	started := time.Now()
	resp, err := w.http.Do(req)
	if err != nil {
		return w.transportError(ctx, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxWriteBodyBytes+1))
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return errcode.Newf(errcode.Unauthorized,
			"冷链台账拒绝了本服务的凭据（HTTP %d）：请核对 %s", resp.StatusCode, config.EnvLedgerToken)
	case resp.StatusCode == http.StatusNotFound:
		return errcode.Newf(errcode.NotFound,
			"冷链台账上没有该点位或该记录（HTTP %d）：%s", resp.StatusCode, deviationsPath)
	case resp.StatusCode == http.StatusBadRequest, resp.StatusCode == http.StatusUnprocessableEntity:
		return errcode.Newf(errcode.Validation,
			"冷链台账拒绝了本次越温记录（HTTP %d）：%s", resp.StatusCode, summarizeBody(raw))
	case resp.StatusCode == http.StatusConflict:
		// 台账按 external_ref / idempotency_key 认出了重复：这一条已经入过账。
		// 报成功，让本地记账与台账保持一致，不再重发。
		w.logger.Warn("ledger_deviation_duplicate",
			"external_ref", rec.ExternalRef,
			"idempotency_key", rec.IdempotencyKey,
			"status", resp.StatusCode,
		)
		return nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return errcode.Newf(errcode.LedgerUnavailable,
			"冷链台账限流（HTTP 429），可退避后重试")
	case resp.StatusCode >= 500:
		return errcode.Newf(errcode.LedgerUnavailable,
			"冷链台账返回服务端错误（HTTP %d），可退避后重试", resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return errcode.Newf(errcode.LedgerProtocol,
			"冷链台账对越温记录写入返回了预期外的状态码（HTTP %d）：%s",
			resp.StatusCode, summarizeBody(raw))
	}

	w.logger.Info("ledger_deviation_written",
		"external_ref", rec.ExternalRef,
		"point_id", rec.PointID,
		"peak_temperature_c", rec.PeakTemperatureC,
		"duration_minutes", rec.DurationMinutes,
		"disposition", rec.Disposition,
		"status", resp.StatusCode,
		"latency_ms", time.Since(started).Milliseconds(),
	)
	return nil
}

// WriteDeviation 按 workflow.OutboundWriter 的约定写回一条越温偏差。
//
// 它接受的是**确认时固化的载荷原文**：写回侧发出去的必须是主管确认那一刻
// 定下的内容，不能在写回时重新算一遍——重新算就等于「台账里那条是另一份结论」。
func (w *DeviationWriter) WriteDeviation(ctx context.Context, runID string, payloadJSON string) error {
	rec, err := DecodeDeviation(payloadJSON)
	if err != nil {
		return err
	}
	if err := w.Write(ctx, rec); err != nil {
		return err
	}
	w.logger.Info("ledger_deviation_writeback_completed", "run_id", runID,
		"external_ref", rec.ExternalRef)
	return nil
}

// transportError 把传输层失败映射到错误码。
func (w *DeviationWriter) transportError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return contextError(ctxErr)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return errcode.Newf(errcode.LedgerUnavailable,
			"冷链台账写侧调用超时（超过 %s）：%v", w.cfg.Timeout, err)
	}
	return errcode.Newf(errcode.LedgerUnavailable, "冷链台账不可达：%v", err)
}
