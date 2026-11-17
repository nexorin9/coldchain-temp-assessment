package ingest

import (
	"sync"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// 本文件是入站侧的同点限流闸门。
//
// 为什么需要它：院内的冷链监测平台经常在断网恢复后补投历史数据，
// 一台网关可能一口气推来几千条；另有一种情况是记录仪固件出问题了，
// 把同一个采样反复推。两种情况都会在几秒内把同一点位的入库压力顶满，
// 拖慢其它点位的正常投递，也会把判定队列挤爆。
//
// 为什么是令牌桶而不是固定窗口计数：
//
// 冷链现场有两种截然不同的流量形态，用同一个阈值卡不住两种：
//   - 常态：记录仪 5 分钟一条，慢而稳；
//   - 突发：断网恢复后补投历史数据，一个网关一口气推来上百条历史采样。
//
// 固定窗口计数会把补投也当成洪泛拒掉——数据其实都在，只是来得急，
// 拒掉等于让现场以为那段历史没入库。令牌桶把两者分开：
// 稳态速率限制持续刷屏，桶容量（突发额度）给补投留出余量。
//
// 三条底线：
//   - 不静默丢弃：超限的投递返回显式错误（E_RATE_LIMITED），由调用方记录并上报，
//     不能悄悄扔掉——扔掉意味着现场以为数据送到了；
//   - 按点位隔离：一个点位刷屏，不影响其它点位；
//   - 阈值可调：默认值远高于正常采样率，只在真正的失控时才触发。

// DefaultRateLimit 是默认的单点位稳态投递速率（次 / 窗口）。
//
// 取 60 次 / 60 秒：正常 5 分钟粒度是每分钟 0.2 条，60 次相当于正常速率的 300 倍。
const DefaultRateLimit = 60

// DefaultRateWindow 是默认的速率窗口。
const DefaultRateWindow = time.Minute

// DefaultRateBurst 是默认的桶容量（突发额度）。
//
// 取 300 次：够放下一次断网恢复后的补投（一台记录仪 5 分钟一条，
// 300 条约合 25 小时的量），又不至于大到让真正的失控无限刷。
const DefaultRateBurst = 300

// RateLimiter 是按点位的投递限流闸门（令牌桶）。
//
// 每个点位一个桶：常态下按 refill 速率匀速补充，令牌用完即拒绝；
// 桶里攒下的令牌允许一次性突发（补投历史数据用得上）。
type RateLimiter struct {
	mu sync.Mutex
	// capacity 是桶容量：允许的最大突发投递次数。
	capacity int
	// refillPerSecond 是每秒补充的令牌数 = limit / window。
	refillPerSecond float64
	// limit 是窗口内允许的稳态次数；小于等于 0 表示不限流。
	limit int
	now   func() time.Time
	// buckets 保存各点位当前的令牌数与上次补充时刻。
	buckets map[string]*rateBucket
}

// rateBucket 是某个点位当前的令牌状态。
type rateBucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter 创建一个限流闸门。
//
// limit 是窗口内允许的稳态投递次数，window 是该窗口长度，
// burst 是桶容量（允许的突发投递次数上限）。
//
// limit 小于等于 0 表示不限流（Allow 恒放行）；
// window 小于等于 0 时回落到默认窗口；burst 小于等于 0 时取 limit（不给额外突发额度）。
func NewRateLimiter(limit int, window time.Duration, burst int) *RateLimiter {
	if window <= 0 {
		window = DefaultRateWindow
	}
	if burst <= 0 {
		burst = limit
	}
	if burst < limit {
		// 桶容量不得小于稳态速率：容量小于补充速率时令牌会被瞬间补满，
		// 限流形同虚设。以稳态速率作为容量下限。
		burst = limit
	}
	return &RateLimiter{
		capacity:        burst,
		refillPerSecond: float64(limit) / window.Seconds(),
		limit:           limit,
		now:             time.Now,
		buckets:         make(map[string]*rateBucket),
	}
}

// Allow 判断该点位此刻的投递是否放行。
//
// 放行时扣一个令牌；桶空时返回 E_RATE_LIMITED 错误，错误文本带上点位、
// 窗口与桶容量，便于现场直接看出是哪台设备在刷。
func (r *RateLimiter) Allow(pointID string) error {
	if r == nil || r.limit <= 0 {
		return nil
	}

	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()

	bucket, ok := r.buckets[pointID]
	if !ok {
		bucket = &rateBucket{tokens: float64(r.capacity), last: now}
		r.buckets[pointID] = bucket
	}

	// 按经过的时间补充令牌：桶最多积到容量，不无限涨。
	if elapsed := now.Sub(bucket.last).Seconds(); elapsed > 0 {
		bucket.tokens += elapsed * r.refillPerSecond
		if bucket.tokens > float64(r.capacity) {
			bucket.tokens = float64(r.capacity)
		}
		bucket.last = now
	}

	if bucket.tokens < 1 {
		return errcode.Newf(errcode.RateLimited,
			"点位 %s 的投递超出限流上限（稳态 %d 次/窗口，突发上限 %d 次，当前令牌已耗尽），本次投递已被拒绝并记录",
			pointID, r.limit, r.capacity)
	}
	bucket.tokens--
	return nil
}

// RemainingTokens 返回该点位当前剩余的令牌数。
//
// 给运行摘要与告警用：限流是「发生了什么」的信号，
// 只报错不留数的话，运维只能靠翻日志知道现场在刷屏。
func (r *RateLimiter) RemainingTokens(pointID string) int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	bucket, ok := r.buckets[pointID]
	if !ok {
		return r.capacity
	}
	return int(bucket.tokens)
}

// SetClock 替换闸门使用的时钟，供测试注入确定性时间。
func (r *RateLimiter) SetClock(now func() time.Time) {
	if r == nil || now == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = now
}
