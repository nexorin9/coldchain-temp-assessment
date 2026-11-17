// 本文件落出站调用的**退避重试**：台账写入与门诊通知在失败时按指数退避重试，
// 重试耗尽才交出错误码。
//
// 为什么出站这一段要单独有重试：判定链内部每一步都有重试（步骤级重试，
// 见 runtime/retry.go），但那重试的是「判定本身算不算得出来」；
// 这里重试的是「台账收没收到这条越温」。两者的失败语义完全不同——
// 判定算不出来可以退回规则基线，台账没收到就是账上少了一条，
// 只能靠重试与补偿把它补上。
//
// 三条纪律：
//
//  1. **只重试该重试的**：网络超时、连接失败、5xx、429 可退避重试；
//     凭据被拒（401/403）、请求本身不合法（400/422）不重试——
//     再发一百次也是同样的结果，只是把台账的日志刷满。
//  2. **重试次数有上限**：默认额外重试 2 次（连首次共 3 次调用），
//     退避时长指数增长且封顶。重试不是无限循环，它有终点。
//  3. **耗尽即交账**：重试用完仍失败，返回带尝试次数的错误（ExhaustedError），
//     上层据此把这条越温挂到「待补偿」并留下告警记录——
//     台账里少一条越温这件事，必须有人看得见。
//
// 它与源产品机制的关系：源仓库的模型单价表瘦身工具（tools/model-prices）
// 在每一步失败时只做一件事——把失败原因写进 stderr 然后以非 0 退出码结束，
// 绝不写出一份半成品。搬到这里的出站重试是同一件事的另一半：
// 该退的退、退不动的就明说退不动了，不把「重试过了」说成「写成了」。
package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
)

const (
	// DefaultMaxRetries 是出站调用默认的额外重试次数。
	//
	// 连首次共 3 次调用：这个数字是「台账短时抖动值得再敲一次门」的量级，
	// 又不至于把一次确认拖成几分钟的等待——主管点完确认要的是当场回执。
	DefaultMaxRetries = 2
	// DefaultRetryBaseDelay 是首次退避的等待时长。
	DefaultRetryBaseDelay = time.Second
	// DefaultRetryMaxDelay 是单次退避的时长上限。
	DefaultRetryMaxDelay = 8 * time.Second
)

// RetryPolicy 是出站调用的退避重试策略。
//
// 它是值类型且可注入：测试把 Sleep 换成不真的等待的假实现，
// 于是「超时 → 重试 → 成功」这条路径在毫秒内跑完，退避序列仍可断言。
type RetryPolicy struct {
	// MaxRetries 是额外重试次数；0 表示不重试（只发首次那一次）。
	MaxRetries int
	// BaseDelay 是首次退避时长。
	BaseDelay time.Duration
	// MaxDelay 是单次退避的时长上限。
	MaxDelay time.Duration
	// Sleep 是等待实现；nil 时取按 ctx 取消的默认等待。
	Sleep func(ctx context.Context, d time.Duration) error
}

// DefaultRetryPolicy 返回出站调用的默认重试策略。
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxRetries: DefaultMaxRetries,
		BaseDelay:  DefaultRetryBaseDelay,
		MaxDelay:   DefaultRetryMaxDelay,
	}
}

// Normalize 把策略里的非法取值收敛回默认档。
//
// 它不做「报错退出」：重试策略错了不该让服务起不来——
// 最坏的结果是多敲两次门，而不是整个判定链停摆。
// 取值非法时回落默认，而不是照着非法值硬跑。
func (p RetryPolicy) Normalize() RetryPolicy {
	out := p
	if out.MaxRetries < 0 {
		out.MaxRetries = DefaultMaxRetries
	}
	if out.BaseDelay <= 0 {
		out.BaseDelay = DefaultRetryBaseDelay
	}
	if out.MaxDelay <= 0 {
		out.MaxDelay = DefaultRetryMaxDelay
	}
	if out.MaxDelay < out.BaseDelay {
		out.MaxDelay = out.BaseDelay
	}
	if out.Sleep == nil {
		out.Sleep = sleepWithContext
	}
	return out
}

// Backoff 返回第 retryIndex 次重试前的等待时长（retryIndex 从 1 起）。
//
// 指数增长：1 次重试等 BaseDelay，2 次等两倍，以此类推，
// 但单次不超过 MaxDelay——退避不是无限增长的，第三次和第四次之间
// 差别不该比第一次和第二次更大。
func (p RetryPolicy) Backoff(retryIndex int) time.Duration {
	policy := p.Normalize()
	if retryIndex < 1 {
		return 0
	}
	delay := policy.BaseDelay
	for i := 1; i < retryIndex; i++ {
		delay *= 2
		if delay >= policy.MaxDelay {
			return policy.MaxDelay
		}
	}
	if delay > policy.MaxDelay {
		return policy.MaxDelay
	}
	return delay
}

// TotalAttempts 返回一次出站调用最多会发起几次请求（含首次）。
func (p RetryPolicy) TotalAttempts() int {
	return p.Normalize().MaxRetries + 1
}

// sleepWithContext 是默认的等待实现：等满时长，或等 ctx 被取消。
//
// 等待期间必须能被取消：主管在界面上点了「取消」，这次写回就该立刻停手，
// 而不是把两次退避都睡完再去发那两个注定失败的请求。
func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ExhaustedError 是「退避重试已耗尽」的错误。
//
// 它包着最后一次失败的原因（Unwrap），所以上层取错误码时
// 仍然拿得到 E_LEDGER_UNAVAILABLE 这类分档信息——
// 「重试耗尽」是附加说明，不是把真实原因盖掉。
type ExhaustedError struct {
	// Attempts 是实际发起的调用次数（含首次）。
	Attempts int
	// Last 是最后一次失败的原因。
	Last error
}

// Error 实现 error 接口。
func (e *ExhaustedError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("出站调用重试耗尽（共发起 %d 次，最后一次失败）：%v", e.Attempts, e.Last)
}

// Unwrap 返回最后一次失败的原因，让 errors.As 能取到内层的错误码。
func (e *ExhaustedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Last
}

// AttemptsOf 返回一次出站调用实际发起的次数。
//
// nil（成功）按 1 次计：成功那一次也是发出去过的。
// 非「重试耗尽」的错误按 1 次计：只发了那一次就失败了（不可重试的错误）。
func AttemptsOf(err error) int {
	if err == nil {
		return 1
	}
	var ex *ExhaustedError
	if errors.As(err, &ex) && ex.Attempts > 0 {
		return ex.Attempts
	}
	return 1
}

// Retryable 判断一个出站错误值不值得退避重试。
//
// 只有「对端暂时不可用」这一类才重试：
//   - E_LEDGER_UNAVAILABLE / E_NOTIFY_UNAVAILABLE：超时、连接失败、5xx、429；
//   - E_LEDGER_PROTOCOL / E_NOTIFY_PROTOCOL：对端在说另一种话，重发多半一样。
//
// 不可重试的：凭据被拒（E_UNAUTHORIZED）、请求本身不合法（E_VALIDATION）、
// 对象不存在（E_NOT_FOUND）、上下文取消——重发只是把同一个错再犯一遍。
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	switch errcode.As(err).Code {
	case errcode.LedgerUnavailable, errcode.NotifyUnavailable,
		errcode.LedgerProtocol, errcode.NotifyProtocol:
		return true
	default:
		return false
	}
}

// RetryingDeviationWriter 是带退避重试的冷链台账写侧。
//
// 它包在 DeviationWriter 外面，业务层拿到的接口不变（WriteDeviation），
// 换句话说：重试是出站这一层的内部细节，判定链不需要知道它。
type RetryingDeviationWriter struct {
	inner  *DeviationWriter
	policy RetryPolicy
	logger *logging.Logger
}

// NewRetryingDeviationWriter 装配带退避重试的台账写侧。
func NewRetryingDeviationWriter(inner *DeviationWriter, policy RetryPolicy, logger *logging.Logger) *RetryingDeviationWriter {
	if logger == nil {
		logger = logging.Default()
	}
	return &RetryingDeviationWriter{inner: inner, policy: policy.Normalize(), logger: logger}
}

// BaseURL 返回台账基址（不含凭据）。
func (w *RetryingDeviationWriter) BaseURL() string {
	if w == nil || w.inner == nil {
		return ""
	}
	return w.inner.BaseURL()
}

// Write 把一条越温偏差写回冷链台账，失败时按退避策略重试。
//
// 重试用尽仍失败时返回 *ExhaustedError：内层仍带最后一次的错误码分档。
func (w *RetryingDeviationWriter) Write(ctx context.Context, rec DeviationRecord) error {
	if w == nil || w.inner == nil {
		return errcode.Newf(errcode.StorageError, "出站写回未装配台账写侧")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	policy := w.policy.Normalize()
	total := policy.TotalAttempts()

	var lastErr error
	for attempt := 1; attempt <= total; attempt++ {
		if err := ctx.Err(); err != nil {
			return contextError(err)
		}
		lastErr = w.inner.Write(ctx, rec)
		if lastErr == nil {
			return nil
		}
		if !Retryable(lastErr) {
			// 不可重试：原样交出去，不套「重试耗尽」的外壳。
			return lastErr
		}
		if attempt == total {
			break
		}
		delay := policy.Backoff(attempt)
		w.logger.Warn("ledger_writeback_retry",
			"external_ref", rec.ExternalRef,
			"attempt", attempt,
			"backoff_ms", delay.Milliseconds(),
			"error_code", string(errcode.As(lastErr).Code),
			"detail", errcode.As(lastErr).Details,
		)
		if err := policy.Sleep(ctx, delay); err != nil {
			return contextError(err)
		}
	}

	w.logger.Error("ledger_writeback_retries_exhausted",
		"external_ref", rec.ExternalRef,
		"attempts", total,
		"error_code", string(errcode.As(lastErr).Code),
		"detail", errcode.As(lastErr).Details,
	)
	return &ExhaustedError{Attempts: total, Last: lastErr}
}

// WriteDeviation 按 workflow.OutboundWriter 的约定写回一条越温偏差。
func (w *RetryingDeviationWriter) WriteDeviation(ctx context.Context, runID string, payloadJSON string) error {
	rec, err := DecodeDeviation(payloadJSON)
	if err != nil {
		return err
	}
	return w.Write(ctx, rec)
}

// RetryingNotifier 是带退避重试的接种门诊通知发送端。
//
// 它与台账写侧分开成一个类型：两者的失败分档不同（E_LEDGER_UNAVAILABLE
// 与 E_NOTIFY_UNAVAILABLE），共用一个类型会逼着其中一个去猜自己撞上了哪种错。
type RetryingNotifier struct {
	inner  *Notifier
	policy RetryPolicy
	logger *logging.Logger
}

// NewRetryingNotifier 装配带退避重试的通知发送端。
func NewRetryingNotifier(inner *Notifier, policy RetryPolicy, logger *logging.Logger) *RetryingNotifier {
	if logger == nil {
		logger = logging.Default()
	}
	return &RetryingNotifier{inner: inner, policy: policy.Normalize(), logger: logger}
}

// Channel 返回通知渠道。
func (n *RetryingNotifier) Channel() string {
	if n == nil || n.inner == nil {
		return ""
	}
	return n.inner.Channel()
}

// Send 发一条通知，失败时按退避策略重试。
func (n *RetryingNotifier) Send(ctx context.Context, msg Message) error {
	if n == nil || n.inner == nil {
		return errcode.Newf(errcode.StorageError, "出站写回未装配通知系统")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	policy := n.policy.Normalize()
	total := policy.TotalAttempts()

	var lastErr error
	for attempt := 1; attempt <= total; attempt++ {
		if err := ctx.Err(); err != nil {
			return contextError(err)
		}
		lastErr = n.inner.Send(ctx, msg)
		if lastErr == nil {
			return nil
		}
		if !Retryable(lastErr) {
			return lastErr
		}
		if attempt == total {
			break
		}
		delay := policy.Backoff(attempt)
		n.logger.Warn("notification_send_retry",
			"related_run_id", msg.RelatedRunID,
			"attempt", attempt,
			"backoff_ms", delay.Milliseconds(),
			"error_code", string(errcode.As(lastErr).Code),
			"detail", errcode.As(lastErr).Details,
		)
		if err := policy.Sleep(ctx, delay); err != nil {
			return contextError(err)
		}
	}

	n.logger.Error("notification_send_retries_exhausted",
		"related_run_id", msg.RelatedRunID,
		"attempts", total,
		"error_code", string(errcode.As(lastErr).Code),
		"detail", errcode.As(lastErr).Details,
	)
	return &ExhaustedError{Attempts: total, Last: lastErr}
}
