// Package config 从环境变量装载服务配置并做启动前校验。
//
// 设计约束（见 spec.md「部署与集成」）：所有基址与凭据只从环境变量读；
// 缺少出站台账 / 通知 / 模型端点等必填项时，serve 子命令启动即报错退出，
// 绝不回落到本地替身或 localhost 默认值——避免出现「看着跑通了、实际没接院内系统」。
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// 环境变量名。凭据与基址一律只从这里读，进程内不落盘、不写日志。
const (
	EnvIngestToken   = "COLDCHAIN_INGEST_TOKEN"
	EnvLedgerBaseURL = "COLDCHAIN_LEDGER_BASE_URL"
	EnvLedgerToken   = "COLDCHAIN_LEDGER_TOKEN"
	EnvNotifyBaseURL = "COLDCHAIN_NOTIFY_BASE_URL"
	EnvNotifyToken   = "COLDCHAIN_NOTIFY_TOKEN"
	EnvModelBaseURL  = "MODEL_BASE_URL"
	EnvModelAPIKey   = "MODEL_API_KEY"
	EnvModelName     = "MODEL_NAME"
	EnvModelTimeout  = "MODEL_TIMEOUT_SECONDS"
	// EnvOperatorToken 是药学部冷链管理员（经办）的凭据。
	//
	// 必填项：没有它，经办就看不到判定结论，整条业务链断在「事件进来了、
	// 没人看得见」。
	EnvOperatorToken = "COLDCHAIN_OPERATOR_TOKEN"
	// EnvSupervisorToken 是药学部主管（审核）的凭据。
	//
	// 必填项：确认或否决处置结论只有这个身份能做，
	// 缺它意味着没有任何人能把一条判定推进到「已生效」。
	EnvSupervisorToken = "COLDCHAIN_SUPERVISOR_TOKEN"
	// EnvAuditorToken 是质控 / 院感（只读调阅）的凭据。
	//
	// 可选项：没配就没有只读通道——按最保守处理，不是默认放行。
	EnvAuditorToken = "COLDCHAIN_AUDITOR_TOKEN"
	// EnvModelPriceTable 是模型单价表（每 token 单价）的 JSON 文件路径。
	//
	// 可选项：不配就把成本记成 unknown——「本服务不知道单价」，
	// 而不是「这次调用不花钱」。配了就按表里的单价估成本。
	EnvModelPriceTable = "MODEL_PRICE_TABLE"
	EnvDatabasePath    = "DATABASE_PATH"
	EnvArchiveDir      = "ARCHIVE_DIR"
	// EnvIngestRateLimit 是单点位的稳态投递速率（次 / 窗口）。
	EnvIngestRateLimit = "COLDCHAIN_INGEST_RATE_LIMIT"
	// EnvIngestRateWindow 是速率窗口长度（秒）。
	EnvIngestRateWindow = "COLDCHAIN_INGEST_RATE_WINDOW_SECONDS"
	// EnvIngestRateBurst 是单点位的突发额度（次）。
	// 断网恢复后补投历史数据用得上这份额度，不至于被当成洪泛拒掉。
	EnvIngestRateBurst = "COLDCHAIN_INGEST_RATE_BURST"
	// EnvLedgerTimeout 是冷链台账读侧的单次调用时限（秒）。
	//
	// 可选项：不配就用默认 15 秒。台账是院内另一个系统，
	// 慢一点不影响判定，但卡死不能拖住判定步骤。
	EnvLedgerTimeout = "COLDCHAIN_LEDGER_TIMEOUT_SECONDS"
	// EnvLedgerCacheTTL 是台账读侧缓存的有效期（秒）。
	//
	// 可选项：不配就用默认 30 秒；配成负值表示关闭缓存。
	// 取值刻意很小：在库批次是「此刻库里有什么货」，放久了就成了过期库存。
	EnvLedgerCacheTTL = "COLDCHAIN_LEDGER_CACHE_TTL_SECONDS"
	// EnvNotifyTimeout 是通知侧的单次调用时限（秒）。
	//
	// 可选项：不配就用默认 10 秒。通知比台账轻，
	// 但它是「门诊什么时候知道」的那一段，也不该无限期挂着。
	EnvNotifyTimeout = "COLDCHAIN_NOTIFY_TIMEOUT_SECONDS"
	// EnvNotifyChannel 是通知渠道。
	//
	// 可选项：不配就用默认的「冷链温控越温」渠道。
	// 院内通知系统的渠道名各家不同，按本院约定覆盖即可。
	EnvNotifyChannel = "COLDCHAIN_NOTIFY_CHANNEL"
	// EnvNotifyAudience 是通知对象。
	//
	// 可选项：不配就用默认的预防接种门诊。
	// 多个点位分属不同门诊时，按点位配置覆盖更合适——本轮先给全局默认。
	EnvNotifyAudience = "COLDCHAIN_NOTIFY_AUDIENCE"
	// EnvWritebackMaxRetries 是出站写回的额外重试次数。
	//
	// 可选项：不配就用默认 2 次（连首次共 3 次调用）。
	// 台账短时抖动值得再敲一次门，但主管点完确认要的是当场回执，
	// 不能把一次确认拖成几分钟的等待。
	EnvWritebackMaxRetries = "COLDCHAIN_WRITEBACK_MAX_RETRIES"
	// EnvWritebackBackoff 是出站写回首次退避的等待秒数。
	//
	// 可选项：不配就用默认 1 秒；之后按指数增长并封顶。
	EnvWritebackBackoff = "COLDCHAIN_WRITEBACK_BACKOFF_SECONDS"
)

// DefaultModelTimeout 是模型调用的默认超时。
//
// 导出给模型网关包复用：超时默认值只该有一处，否则两边各写一个 30 秒，
// 改了一处忘了另一处，排查起来很难看出差异。
const DefaultModelTimeout = 30 * time.Second

// 默认值只覆盖本地存储位置，不覆盖任何出站基址或凭据。
const (
	defaultModelTimeout = DefaultModelTimeout
	defaultDatabasePath = "./data/coldchain.db"
	defaultArchiveDir   = "./data/archive"
	// defaultIngestRateLimit 是单点位稳态投递速率默认值（次 / 窗口）。
	// 取值远高于记录仪 5 分钟一条的正常速率，只在真正的洪泛时才触发。
	defaultIngestRateLimit = 60
	// defaultIngestRateWindow 是速率窗口默认值。
	defaultIngestRateWindow = 60 * time.Second
	// defaultIngestRateBurst 是突发额度默认值（次）。
	// 够放下一次断网恢复后的历史补投（约 25 小时的 5 分钟粒度采样）。
	defaultIngestRateBurst = 300
	// defaultWritebackMaxRetries 是出站写回的额外重试次数默认值。
	defaultWritebackMaxRetries = 2
	// defaultWritebackBackoff 是出站写回首次退避的等待时长默认值。
	defaultWritebackBackoff = time.Second
)

// Config 是服务的运行期配置。
type Config struct {
	// 入站
	IngestToken string
	// 入站限流：单点位的稳态投递速率（次 / 窗口）与突发额度
	IngestRateLimit  int
	IngestRateWindow time.Duration
	IngestRateBurst  int

	// 角色凭据：一把令牌对应一个身份
	OperatorToken   string
	SupervisorToken string
	AuditorToken    string

	// 出站：冷链台账（读时序 / 读批次 / 写越温记录）
	LedgerBaseURL string
	LedgerToken   string

	// 出站：接种门诊 / 药事通知
	NotifyBaseURL string
	NotifyToken   string

	// 出站写回的退避重试策略：
	// 额外重试次数与首次退避等待时长（之后指数增长并封顶）
	WritebackMaxRetries int
	WritebackBackoff    time.Duration

	// 模型网关（OpenAI-compatible）
	ModelBaseURL string
	ModelAPIKey  string
	ModelName    string
	ModelTimeout time.Duration
	// ModelPriceTable 是单价表文件路径；留空表示没配单价表，成本记 unknown。
	ModelPriceTable string

	// 本地存储
	DatabasePath string
	ArchiveDir   string
}

// requiredKeys 是 serve 启动时必须齐备的配置项，顺序即报错时的展示顺序。
//
// 角色凭据里经办与主管两项是必填：缺经办没人看得见判定结论，
// 缺主管没人能把结论推进到「已生效」——这两种缺法都让服务起不来是对的，
// 因为它们让整条业务链停在半路。质控 / 院感的只读凭据是可选的：
// 没有这个岗，服务照常跑，只是没有调阅通道。
var requiredKeys = []string{
	EnvIngestToken,
	EnvOperatorToken,
	EnvSupervisorToken,
	EnvLedgerBaseURL,
	EnvLedgerToken,
	EnvNotifyBaseURL,
	EnvNotifyToken,
	EnvModelBaseURL,
	EnvModelAPIKey,
	EnvModelName,
}

// Load 严格装载配置：任一必填项缺失或非法即返回 *errcode.Error（E_CONFIG_MISSING）。
func Load() (*Config, error) {
	cfg, missing := loadRaw()
	if len(missing) > 0 {
		return nil, errcode.Newf(errcode.ConfigMissing,
			"缺少必填配置项：%s（请在环境变量或 .env 中设置，详见 .env.example）",
			strings.Join(missing, "、"))
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadLenient 宽容装载配置：只填能填上的必填项，返回仍然可用的配置与缺失项清单。
// 供 assess / import / replay 等不依赖出站系统的子命令使用。
func LoadLenient() (*Config, []string) {
	cfg, missing := loadRaw()
	return cfg, missing
}

// loadRaw 读取环境变量并做类型解析，返回配置与缺失的必填项清单。
func loadRaw() (*Config, []string) {
	get := func(key string) string { return strings.TrimSpace(os.Getenv(key)) }

	cfg := &Config{
		IngestToken:     get(EnvIngestToken),
		OperatorToken:   get(EnvOperatorToken),
		SupervisorToken: get(EnvSupervisorToken),
		AuditorToken:    get(EnvAuditorToken),
		LedgerBaseURL:   trimTrailingSlash(get(EnvLedgerBaseURL)),
		LedgerToken:     get(EnvLedgerToken),
		NotifyBaseURL:   trimTrailingSlash(get(EnvNotifyBaseURL)),
		NotifyToken:     get(EnvNotifyToken),
		ModelBaseURL:    trimTrailingSlash(get(EnvModelBaseURL)),
		ModelAPIKey:     get(EnvModelAPIKey),
		ModelName:       get(EnvModelName),
		ModelPriceTable: get(EnvModelPriceTable),
		DatabasePath:    get(EnvDatabasePath),
		ArchiveDir:      get(EnvArchiveDir),
	}

	if cfg.DatabasePath == "" {
		cfg.DatabasePath = defaultDatabasePath
	}
	if cfg.ArchiveDir == "" {
		cfg.ArchiveDir = defaultArchiveDir
	}

	// 限流阈值是可选项：没配就用默认值，配了必须是正整数。
	// 取值非法时记 -1，由 validate 阶段以显式错误暴露给 serve。
	cfg.IngestRateLimit = defaultIngestRateLimit
	if raw := get(EnvIngestRateLimit); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit <= 0 {
			cfg.IngestRateLimit = -1
		} else {
			cfg.IngestRateLimit = limit
		}
	}
	cfg.IngestRateWindow = defaultIngestRateWindow
	if raw := get(EnvIngestRateWindow); raw != "" {
		secs, err := strconv.Atoi(raw)
		if err != nil || secs <= 0 {
			cfg.IngestRateWindow = -1
		} else {
			cfg.IngestRateWindow = time.Duration(secs) * time.Second
		}
	}
	cfg.IngestRateBurst = defaultIngestRateBurst
	if raw := get(EnvIngestRateBurst); raw != "" {
		burst, err := strconv.Atoi(raw)
		if err != nil || burst <= 0 {
			cfg.IngestRateBurst = -1
		} else {
			cfg.IngestRateBurst = burst
		}
	}

	cfg.ModelTimeout = defaultModelTimeout
	if raw := get(EnvModelTimeout); raw != "" {
		secs, err := strconv.Atoi(raw)
		if err != nil || secs <= 0 {
			// 类型非法时保留默认值，但由 validate 阶段以显式错误暴露给 serve。
			cfg.ModelTimeout = -1
		} else {
			cfg.ModelTimeout = time.Duration(secs) * time.Second
		}
	}

	// 出站写回的退避重试：重试次数可为 0（只发首次那一次），
	// 退避时长必须是正数。
	cfg.WritebackMaxRetries = defaultWritebackMaxRetries
	if raw := get(EnvWritebackMaxRetries); raw != "" {
		retries, err := strconv.Atoi(raw)
		if err != nil || retries < 0 {
			cfg.WritebackMaxRetries = -1
		} else {
			cfg.WritebackMaxRetries = retries
		}
	}
	cfg.WritebackBackoff = defaultWritebackBackoff
	if raw := get(EnvWritebackBackoff); raw != "" {
		secs, err := strconv.Atoi(raw)
		if err != nil || secs <= 0 {
			cfg.WritebackBackoff = -1
		} else {
			cfg.WritebackBackoff = time.Duration(secs) * time.Second
		}
	}

	var missing []string
	for _, key := range requiredKeys {
		if get(key) == "" {
			missing = append(missing, key)
		}
	}
	return cfg, missing
}

// validate 校验配置取值是否合法。
func (c *Config) validate() error {
	if c.ModelTimeout <= 0 {
		return errcode.Newf(errcode.ConfigMissing,
			"%s 必须为正整数秒，当前取值非法", EnvModelTimeout)
	}
	if c.IngestRateLimit <= 0 {
		return errcode.Newf(errcode.ConfigMissing,
			"%s 必须为正整数次，当前取值非法", EnvIngestRateLimit)
	}
	if c.IngestRateWindow <= 0 {
		return errcode.Newf(errcode.ConfigMissing,
			"%s 必须为正整数秒，当前取值非法", EnvIngestRateWindow)
	}
	if c.IngestRateBurst <= 0 {
		return errcode.Newf(errcode.ConfigMissing,
			"%s 必须为正整数次，当前取值非法", EnvIngestRateBurst)
	}
	if c.WritebackMaxRetries < 0 {
		return errcode.Newf(errcode.ConfigMissing,
			"%s 必须为非负整数次（0 表示不重试），当前取值非法", EnvWritebackMaxRetries)
	}
	if c.WritebackBackoff <= 0 {
		return errcode.Newf(errcode.ConfigMissing,
			"%s 必须为正整数秒，当前取值非法", EnvWritebackBackoff)
	}
	for _, item := range []struct {
		name string
		raw  string
	}{
		{EnvLedgerBaseURL, c.LedgerBaseURL},
		{EnvNotifyBaseURL, c.NotifyBaseURL},
		{EnvModelBaseURL, c.ModelBaseURL},
	} {
		if err := validateBaseURL(item.name, item.raw); err != nil {
			return err
		}
	}
	return nil
}

// validateBaseURL 校验出站基址：必须是带 http/https 协议与主机名的绝对地址。
func validateBaseURL(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errcode.Newf(errcode.ConfigMissing, "%s 不是合法 URL：%v", name, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errcode.Newf(errcode.ConfigMissing,
			"%s 必须以 http:// 或 https:// 开头，当前为 %q", name, u.Scheme)
	}
	if u.Host == "" {
		return errcode.Newf(errcode.ConfigMissing, "%s 缺少主机名", name)
	}
	return nil
}

// trimTrailingSlash 去掉基址末尾斜杠，便于后续路径拼接。
func trimTrailingSlash(s string) string {
	return strings.TrimRight(s, "/")
}

// Describe 返回一份不含凭据的配置摘要，供启动日志使用。
func (c *Config) Describe() string {
	return fmt.Sprintf("ledger=%s notify=%s model=%s(%s) timeout=%s db=%s archive=%s roles=%s",
		redactURL(c.LedgerBaseURL), redactURL(c.NotifyBaseURL),
		redactURL(c.ModelBaseURL), c.ModelName, c.ModelTimeout,
		c.DatabasePath, c.ArchiveDir, c.rolesSummary())
}

// rolesSummary 列出已配置的角色凭据（只列身份，不列凭据本身）。
//
// 它是运维最关心的一行：哪几个岗位现在能进这台服务，一眼就能核对。
func (c *Config) rolesSummary() string {
	roles := make([]string, 0, 4)
	if c.IngestToken != "" {
		roles = append(roles, "ingest")
	}
	if c.OperatorToken != "" {
		roles = append(roles, "operator")
	}
	if c.SupervisorToken != "" {
		roles = append(roles, "supervisor")
	}
	if c.AuditorToken != "" {
		roles = append(roles, "auditor")
	}
	if len(roles) == 0 {
		return "(无)"
	}
	return strings.Join(roles, ",")
}

// redactURL 只保留基址的协议与主机名，避免把路径里的敏感信息写进日志。
func redactURL(raw string) string {
	if raw == "" {
		return "(未设置)"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(未设置)"
	}
	return u.Scheme + "://" + u.Host
}
