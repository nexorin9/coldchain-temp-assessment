package config

import (
	"strings"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// setEnv 批量设置环境变量；t.Setenv 会在用例结束后自动恢复原值。
func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestLoadMissingRequiredReturnsConfigMissing(t *testing.T) {
	setEnv(t, map[string]string{
		EnvIngestToken:   "",
		EnvLedgerBaseURL: "",
		EnvLedgerToken:   "",
		EnvNotifyBaseURL: "",
		EnvNotifyToken:   "",
		EnvModelBaseURL:  "",
		EnvModelAPIKey:   "",
		EnvModelName:     "",
	})

	_, err := Load()
	if err == nil {
		t.Fatal("期望配置校验失败，实际返回 nil")
	}
	if !strings.Contains(err.Error(), string(errcode.ConfigMissing)) {
		t.Fatalf("期望 E_CONFIG_MISSING，实际：%v", err)
	}
	if !strings.Contains(err.Error(), EnvLedgerBaseURL) {
		t.Fatalf("期望错误信息包含缺失项名 %s，实际：%v", EnvLedgerBaseURL, err)
	}
}

func TestLoadOkReadsAllRequired(t *testing.T) {
	setEnv(t, map[string]string{
		EnvIngestToken:     "ingest-token",
		EnvOperatorToken:   "operator-token",
		EnvSupervisorToken: "supervisor-token",
		EnvLedgerBaseURL:   "https://ledger.internal.example-hospital.cn/",
		EnvLedgerToken:     "ledger-token",
		EnvNotifyBaseURL:   "https://notify.internal.example-hospital.cn",
		EnvNotifyToken:     "notify-token",
		EnvModelBaseURL:    "https://llm.internal.example-hospital.cn/v1",
		EnvModelAPIKey:     "model-key",
		EnvModelName:       "hospital-llm",
		EnvModelTimeout:    "45",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("期望加载成功，实际：%v", err)
	}
	if cfg.LedgerBaseURL != "https://ledger.internal.example-hospital.cn" {
		t.Fatalf("基址末尾斜杠应被去掉，实际：%q", cfg.LedgerBaseURL)
	}
	if cfg.ModelTimeout.Seconds() != 45 {
		t.Fatalf("模型超时应为 45 秒，实际：%v", cfg.ModelTimeout)
	}
	if cfg.DatabasePath != defaultDatabasePath {
		t.Fatalf("数据库路径应取默认值，实际：%q", cfg.DatabasePath)
	}
}

func TestLoadRejectsNonHTTPScheme(t *testing.T) {
	setEnv(t, map[string]string{
		EnvIngestToken:     "ingest-token",
		EnvOperatorToken:   "operator-token",
		EnvSupervisorToken: "supervisor-token",
		EnvLedgerBaseURL:   "ledger.internal.example-hospital.cn",
		EnvLedgerToken:     "ledger-token",
		EnvNotifyBaseURL:   "https://notify.internal.example-hospital.cn",
		EnvNotifyToken:     "notify-token",
		EnvModelBaseURL:    "https://llm.internal.example-hospital.cn/v1",
		EnvModelAPIKey:     "model-key",
		EnvModelName:       "hospital-llm",
	})

	if _, err := Load(); err == nil {
		t.Fatal("期望缺少协议前缀的基址被拒，实际通过")
	}
}

func TestLoadRejectsNonPositiveTimeout(t *testing.T) {
	setEnv(t, map[string]string{
		EnvIngestToken:     "ingest-token",
		EnvOperatorToken:   "operator-token",
		EnvSupervisorToken: "supervisor-token",
		EnvLedgerBaseURL:   "https://ledger.internal.example-hospital.cn",
		EnvLedgerToken:     "ledger-token",
		EnvNotifyBaseURL:   "https://notify.internal.example-hospital.cn",
		EnvNotifyToken:     "notify-token",
		EnvModelBaseURL:    "https://llm.internal.example-hospital.cn/v1",
		EnvModelAPIKey:     "model-key",
		EnvModelName:       "hospital-llm",
		EnvModelTimeout:    "0",
	})

	if _, err := Load(); err == nil {
		t.Fatal("期望非正数的模型超时被拒，实际通过")
	}
}

func TestLoadRateLimitDefaultsWhenUnset(t *testing.T) {
	setEnv(t, map[string]string{
		EnvIngestToken:     "ingest-token",
		EnvOperatorToken:   "operator-token",
		EnvSupervisorToken: "supervisor-token",
		EnvLedgerBaseURL:   "https://ledger.internal.example-hospital.cn",
		EnvLedgerToken:     "ledger-token",
		EnvNotifyBaseURL:   "https://notify.internal.example-hospital.cn",
		EnvNotifyToken:     "notify-token",
		EnvModelBaseURL:    "https://llm.internal.example-hospital.cn/v1",
		EnvModelAPIKey:     "model-key",
		EnvModelName:       "hospital-llm",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("期望加载成功，实际：%v", err)
	}
	if cfg.IngestRateLimit != defaultIngestRateLimit {
		t.Fatalf("限流速率应取默认值 %d，实际 %d", defaultIngestRateLimit, cfg.IngestRateLimit)
	}
	if cfg.IngestRateWindow != defaultIngestRateWindow {
		t.Fatalf("限流窗口应取默认值 %s，实际 %s", defaultIngestRateWindow, cfg.IngestRateWindow)
	}
	if cfg.IngestRateBurst != defaultIngestRateBurst {
		t.Fatalf("突发额度应取默认值 %d，实际 %d", defaultIngestRateBurst, cfg.IngestRateBurst)
	}
}

func TestLoadRejectsInvalidRateLimitValues(t *testing.T) {
	base := map[string]string{
		EnvIngestToken:     "ingest-token",
		EnvOperatorToken:   "operator-token",
		EnvSupervisorToken: "supervisor-token",
		EnvLedgerBaseURL:   "https://ledger.internal.example-hospital.cn",
		EnvLedgerToken:     "ledger-token",
		EnvNotifyBaseURL:   "https://notify.internal.example-hospital.cn",
		EnvNotifyToken:     "notify-token",
		EnvModelBaseURL:    "https://llm.internal.example-hospital.cn/v1",
		EnvModelAPIKey:     "model-key",
		EnvModelName:       "hospital-llm",
	}

	for _, item := range []struct {
		key string
		val string
	}{
		{EnvIngestRateLimit, "0"},
		{EnvIngestRateLimit, "abc"},
		{EnvIngestRateWindow, "0"},
		{EnvIngestRateWindow, "-5"},
		{EnvIngestRateBurst, "0"},
		{EnvIngestRateBurst, "not-a-number"},
	} {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		// 三个限流项每一轮都显式写全：t.Setenv 在整个用例内持续生效，
		// 上一轮留下的非法值会盖住这一轮要验的那一项。
		env[EnvIngestRateLimit] = "60"
		env[EnvIngestRateWindow] = "60"
		env[EnvIngestRateBurst] = "300"
		env[item.key] = item.val
		setEnv(t, env)

		if _, err := Load(); err == nil {
			t.Fatalf("期望 %s=%q 被拒，实际通过", item.key, item.val)
		} else if !strings.Contains(err.Error(), item.key) {
			t.Fatalf("错误信息应点名 %s，实际：%v", item.key, err)
		}
	}
}

func TestDescribeDoesNotLeakCredentials(t *testing.T) {
	cfg := &Config{
		LedgerBaseURL: "https://ledger.internal.example-hospital.cn",
		NotifyBaseURL: "https://notify.internal.example-hospital.cn",
		ModelBaseURL:  "https://llm.internal.example-hospital.cn/v1",
		ModelName:     "hospital-llm",
		LedgerToken:   "super-secret",
	}
	got := cfg.Describe()
	if strings.Contains(got, "super-secret") {
		t.Fatalf("配置摘要不应包含凭据，实际：%s", got)
	}
}

// TestLoadRequiresOperatorAndSupervisorTokens 验证经办与主管凭据缺一即启动失败。
//
// 缺经办没人看得见判定结论，缺主管没人能把结论推进到「已生效」——
// 这两种缺法都让整条业务链停在半路，所以宁可在启动时报错，
// 也不要服务起来了却没人能干活。
func TestLoadRequiresOperatorAndSupervisorTokens(t *testing.T) {
	for _, missing := range []string{EnvOperatorToken, EnvSupervisorToken} {
		env := map[string]string{
			EnvIngestToken:     "ingest-token",
			EnvOperatorToken:   "operator-token",
			EnvSupervisorToken: "supervisor-token",
			EnvLedgerBaseURL:   "https://ledger.internal.example-hospital.cn",
			EnvLedgerToken:     "ledger-token",
			EnvNotifyBaseURL:   "https://notify.internal.example-hospital.cn",
			EnvNotifyToken:     "notify-token",
			EnvModelBaseURL:    "https://llm.internal.example-hospital.cn/v1",
			EnvModelAPIKey:     "model-key",
			EnvModelName:       "hospital-llm",
		}
		env[missing] = ""
		setEnv(t, env)

		_, err := Load()
		if err == nil {
			t.Fatalf("缺 %s 时应启动失败，实际通过", missing)
		}
		if !strings.Contains(err.Error(), missing) {
			t.Fatalf("错误信息应点名缺失项 %s，实际：%v", missing, err)
		}
	}
}

// TestLoadAuditorTokenIsOptional 验证只读调阅凭据缺了不影响启动。
func TestLoadAuditorTokenIsOptional(t *testing.T) {
	setEnv(t, map[string]string{
		EnvIngestToken:     "ingest-token",
		EnvOperatorToken:   "operator-token",
		EnvSupervisorToken: "supervisor-token",
		EnvLedgerBaseURL:   "https://ledger.internal.example-hospital.cn",
		EnvLedgerToken:     "ledger-token",
		EnvNotifyBaseURL:   "https://notify.internal.example-hospital.cn",
		EnvNotifyToken:     "notify-token",
		EnvModelBaseURL:    "https://llm.internal.example-hospital.cn/v1",
		EnvModelAPIKey:     "model-key",
		EnvModelName:       "hospital-llm",
		EnvAuditorToken:    "",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("缺只读调阅凭据时应照常加载，实际：%v", err)
	}
	if cfg.AuditorToken != "" {
		t.Fatalf("只读调阅凭据应为空，实际：%q", cfg.AuditorToken)
	}
}

// TestDescribeListsConfiguredRoles 验证启动摘要只列身份、不列凭据。
func TestDescribeListsConfiguredRoles(t *testing.T) {
	cfg := &Config{
		IngestToken:     "ingest-secret",
		OperatorToken:   "operator-secret",
		SupervisorToken: "supervisor-secret",
	}
	got := cfg.Describe()
	for _, secret := range []string{"ingest-secret", "operator-secret", "supervisor-secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("配置摘要不应包含凭据 %q，实际：%s", secret, got)
		}
	}
	for _, role := range []string{"ingest", "operator", "supervisor"} {
		if !strings.Contains(got, role) {
			t.Fatalf("配置摘要应列出已配置的身份 %q，实际：%s", role, got)
		}
	}
}

// TestLoadWritebackRetryDefaultsWhenUnset 断言出站重试的默认取值：
// 不配就用默认 2 次重试与 1 秒首次退避。
func TestLoadWritebackRetryDefaultsWhenUnset(t *testing.T) {
	setEnv(t, map[string]string{
		EnvIngestToken:     "ingest-token",
		EnvOperatorToken:   "operator-token",
		EnvSupervisorToken: "supervisor-token",
		EnvLedgerBaseURL:   "https://ledger.internal.example-hospital.cn",
		EnvLedgerToken:     "ledger-token",
		EnvNotifyBaseURL:   "https://notify.internal.example-hospital.cn",
		EnvNotifyToken:     "notify-token",
		EnvModelBaseURL:    "https://llm.internal.example-hospital.cn/v1",
		EnvModelAPIKey:     "model-key",
		EnvModelName:       "hospital-llm",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("期望加载成功，实际：%v", err)
	}
	if cfg.WritebackMaxRetries != defaultWritebackMaxRetries {
		t.Fatalf("写回重试次数应取默认值 %d，实际 %d",
			defaultWritebackMaxRetries, cfg.WritebackMaxRetries)
	}
	if cfg.WritebackBackoff != defaultWritebackBackoff {
		t.Fatalf("写回退避时长应取默认值 %s，实际 %s",
			defaultWritebackBackoff, cfg.WritebackBackoff)
	}
}

// TestLoadWritebackRetryReadsOverrides 断言显式配置生效：
// 重试次数可为 0（只发首次那一次），退避时长按配置走。
func TestLoadWritebackRetryReadsOverrides(t *testing.T) {
	setEnv(t, map[string]string{
		EnvIngestToken:         "ingest-token",
		EnvOperatorToken:       "operator-token",
		EnvSupervisorToken:     "supervisor-token",
		EnvLedgerBaseURL:       "https://ledger.internal.example-hospital.cn",
		EnvLedgerToken:         "ledger-token",
		EnvNotifyBaseURL:       "https://notify.internal.example-hospital.cn",
		EnvNotifyToken:         "notify-token",
		EnvModelBaseURL:        "https://llm.internal.example-hospital.cn/v1",
		EnvModelAPIKey:         "model-key",
		EnvModelName:           "hospital-llm",
		EnvWritebackMaxRetries: "0",
		EnvWritebackBackoff:    "3",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("期望加载成功，实际：%v", err)
	}
	if cfg.WritebackMaxRetries != 0 {
		t.Fatalf("写回重试次数 = %d，期望 0", cfg.WritebackMaxRetries)
	}
	if cfg.WritebackBackoff != 3*time.Second {
		t.Fatalf("写回退避时长 = %s，期望 3s", cfg.WritebackBackoff)
	}
}

// TestLoadRejectsInvalidWritebackRetryValues 断言非法取值启动即报错：
// 重试次数为负、退避时长非正都拒。
func TestLoadRejectsInvalidWritebackRetryValues(t *testing.T) {
	base := map[string]string{
		EnvIngestToken:     "ingest-token",
		EnvOperatorToken:   "operator-token",
		EnvSupervisorToken: "supervisor-token",
		EnvLedgerBaseURL:   "https://ledger.internal.example-hospital.cn",
		EnvLedgerToken:     "ledger-token",
		EnvNotifyBaseURL:   "https://notify.internal.example-hospital.cn",
		EnvNotifyToken:     "notify-token",
		EnvModelBaseURL:    "https://llm.internal.example-hospital.cn/v1",
		EnvModelAPIKey:     "model-key",
		EnvModelName:       "hospital-llm",
	}
	for _, item := range []struct {
		key string
		val string
	}{
		{EnvWritebackMaxRetries, "-1"},
		{EnvWritebackMaxRetries, "abc"},
		{EnvWritebackBackoff, "0"},
		{EnvWritebackBackoff, "abc"},
	} {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		env[item.key] = item.val
		setEnv(t, env)

		if _, err := Load(); err == nil {
			t.Fatalf("%s=%q 期望被拒，实际通过", item.key, item.val)
		}
	}
}
