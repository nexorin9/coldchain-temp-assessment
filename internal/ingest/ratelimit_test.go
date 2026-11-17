package ingest

import (
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// 限流闸门的用例覆盖四件事：
//   - 桶里的令牌够用时放行、用完时显式拒绝；
//   - 突发额度放得下断网补投的历史数据；
//   - 时间走过之后令牌自动补充；
//   - 点位之间互不影响。
// 时钟注入固定时间，不依赖真实等待。

func TestRateLimiter_AllowsUpToBurstThenRejects(t *testing.T) {
	limiter := NewRateLimiter(3, time.Minute, 3)

	for i := 1; i <= 3; i++ {
		if err := limiter.Allow("point-vax-freezer-01"); err != nil {
			t.Fatalf("第 %d 次投递应放行，实际报错：%v", i, err)
		}
	}
	if got := limiter.RemainingTokens("point-vax-freezer-01"); got != 0 {
		t.Fatalf("桶内剩余令牌应为 0，实际 %d", got)
	}

	err := limiter.Allow("point-vax-freezer-01")
	if err == nil {
		t.Fatal("令牌耗尽后的投递应被拒绝")
	}
	e := errcode.As(err)
	if e.Code != errcode.RateLimited {
		t.Fatalf("期望 E_RATE_LIMITED，实际 %s", e.Code)
	}
	if e.Details == "" {
		t.Fatal("拒绝原因不应为空：现场要看得懂是哪台设备在刷")
	}
}

func TestRateLimiter_BurstAllowsBacklogReplay(t *testing.T) {
	// 断网恢复后的补投：一台记录仪 5 分钟一条，一次补 72 条历史采样。
	// 这种「来得急但都是真数据」的流量不该被当成刷屏拒掉。
	limiter := NewRateLimiter(60, time.Minute, 300)

	for i := 0; i < 72; i++ {
		if err := limiter.Allow("point-vax-freezer-01"); err != nil {
			t.Fatalf("补投第 %d 条应放行：%v", i+1, err)
		}
	}
}

func TestRateLimiter_RefillsTokensOverTime(t *testing.T) {
	limiter := NewRateLimiter(60, time.Minute, 60)
	now := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	limiter.SetClock(func() time.Time { return now })

	for i := 0; i < 60; i++ {
		if err := limiter.Allow("point-vax-freezer-01"); err != nil {
			t.Fatalf("第 %d 次投递应放行：%v", i+1, err)
		}
	}
	if err := limiter.Allow("point-vax-freezer-01"); err == nil {
		t.Fatal("令牌耗尽后的投递应被拒绝")
	}

	// 过一分钟，稳态速率补回 60 个令牌。
	now = now.Add(time.Minute)
	if err := limiter.Allow("point-vax-freezer-01"); err != nil {
		t.Fatalf("令牌补充后应放行：%v", err)
	}
}

func TestRateLimiter_IsolatesPoints(t *testing.T) {
	limiter := NewRateLimiter(1, time.Minute, 1)

	if err := limiter.Allow("point-vax-freezer-01"); err != nil {
		t.Fatalf("首次投递应放行：%v", err)
	}
	if err := limiter.Allow("point-vax-freezer-01"); err == nil {
		t.Fatal("超限投递应被拒绝")
	}
	if err := limiter.Allow("point-vax-freezer-02"); err != nil {
		t.Fatalf("其它点位不应被牵连：%v", err)
	}
}

func TestRateLimiter_NonPositiveLimitMeansUnlimited(t *testing.T) {
	limiter := NewRateLimiter(0, time.Minute, 0)

	for i := 0; i < 100; i++ {
		if err := limiter.Allow("point-vax-freezer-01"); err != nil {
			t.Fatalf("不限流时不应报错：%v", err)
		}
	}
}

func TestRateLimiter_NilLimiterAllows(t *testing.T) {
	var limiter *RateLimiter
	if err := limiter.Allow("point-vax-freezer-01"); err != nil {
		t.Fatalf("空限流器应放行：%v", err)
	}
}
