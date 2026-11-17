package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// waitRecorder 记录退避等待的次数与每次等待的时长。
//
// 它是重试的「可注入时钟」：把真实等待换掉，既能断言退避序列是指数增长的，
// 又不用把测试真的跑慢几秒。
type waitRecorder struct {
	durations []time.Duration
}

func (w *waitRecorder) wait(ctx context.Context, d time.Duration) error {
	w.durations = append(w.durations, d)
	return ctx.Err()
}

// attempts 返回记录的等待次数。
func (w *waitRecorder) attempts() int { return len(w.durations) }

// TestRetry_SecondAttemptSucceeds 是本任务的主场景：
// 归因步骤第一次失败（模型服务瞬时不可达），退避后第二次成功，
// 判定照常走完并停在待主管确认。
func TestRetry_SecondAttemptSucceeds(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	samples := excursionSamples()

	calls := 0
	chain, err := NewChain(ChainDeps{
		Profile: testProfile(),
		Samples: samples,
		Attribute: func(ctx context.Context, in StepInput) (any, error) {
			calls++
			if calls == 1 {
				return nil, errcode.Newf(errcode.ModelUnavailable, "模型服务不可达：连接被拒绝")
			}
			return &domain.AttributionPayload{
				CauseCode:  "power_loss",
				Confidence: 0.8,
				Evidence: []domain.AttributionEvidence{{
					Feature: "device_event:power_loss", Value: 2, Threshold: 1,
					Note: "越温窗口内有 2 条采样上报 power_loss 事件",
				}},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	waits := &waitRecorder{}
	exec := NewExecutor(chain, st, quietLogger()).WithWaitFunc(waits.wait)

	result, err := exec.Run(ctx, directRun(t, samples))
	if err != nil {
		t.Fatalf("第二次尝试成功后整条链应跑通：%v", err)
	}
	if calls != 2 {
		t.Fatalf("归因步骤应被调用 2 次，实际 %d 次", calls)
	}
	if result.Run.ExecState != domain.ExecStateSucceeded {
		t.Fatalf("执行态应为 succeeded，实际 %q", result.Run.ExecState)
	}
	if result.Run.State != domain.RunStateAwaitingConfirmation {
		t.Fatalf("业务状态应停在待主管确认，实际 %q", result.Run.State)
	}

	// 退避：只等过一次，时长是首次失败的退避（1 秒起步）。
	if waits.attempts() != 1 {
		t.Fatalf("应退避等待 1 次，实际 %d 次", waits.attempts())
	}
	if waits.durations[0] != time.Second {
		t.Fatalf("首次退避应为 1s，实际 %v", waits.durations[0])
	}

	// 时间线上看得见：归因这一步有两条记录，第 1 次失败、第 2 次成功。
	var attrEntries []StepTimeline
	for _, entry := range Timeline(result.Steps) {
		if entry.StepName == StepAttribute {
			attrEntries = append(attrEntries, entry)
		}
	}
	if len(attrEntries) != 2 {
		t.Fatalf("归因步骤应在时间线上留 2 条记录，实际 %d 条", len(attrEntries))
	}
	if attrEntries[0].Attempt != 1 || attrEntries[0].State != domain.StepStateFailed {
		t.Fatalf("第 1 次尝试应为 attempt=1 且 failed，实际 attempt=%d state=%q",
			attrEntries[0].Attempt, attrEntries[0].State)
	}
	if attrEntries[0].ErrorCode != string(errcode.ModelUnavailable) {
		t.Fatalf("失败尝试应记下错误码 %s，实际 %q",
			errcode.ModelUnavailable, attrEntries[0].ErrorCode)
	}
	if attrEntries[0].BackoffMS != 1000 {
		t.Fatalf("失败尝试应记下退避 1000ms，实际 %d", attrEntries[0].BackoffMS)
	}
	if len(attrEntries[0].Payload) != 0 {
		t.Fatal("失败尝试不应留下产物")
	}
	if attrEntries[1].Attempt != 2 || attrEntries[1].State != domain.StepStateSucceeded {
		t.Fatalf("第 2 次尝试应为 attempt=2 且 succeeded，实际 attempt=%d state=%q",
			attrEntries[1].Attempt, attrEntries[1].State)
	}
	if attrEntries[1].BackoffMS != 0 {
		t.Fatalf("成功尝试不应带退避时长，实际 %d", attrEntries[1].BackoffMS)
	}

	// 前序步骤不因重试而重跑：取数与抽窗口各只跑一次。
	seen := map[string]int{}
	for _, entry := range Timeline(result.Steps) {
		seen[entry.StepName]++
	}
	if seen[StepFetchSeries] != 1 || seen[StepExtractWindow] != 1 {
		t.Fatalf("重试不应重跑前序步骤，实际记录数：%v", seen)
	}

	// 归因结论按分码可取回，且取回的是成功那次尝试的产物。
	attribution, ok := ProductAs[domain.AttributionPayload](result.Steps, domain.PayloadAttribution)
	if !ok {
		t.Fatal("重试成功后归因产物应可按分码取回")
	}
	if attribution.CauseCode != "power_loss" {
		t.Fatalf("归因结论不符：%q", attribution.CauseCode)
	}

	// 落库：两次尝试各一行，退避时长也落库。
	stored, err := st.RunStepsByRun(ctx, result.Run.RunID)
	if err != nil {
		t.Fatalf("回读步骤记录失败：%v", err)
	}
	var storedAttempts []int
	for _, step := range stored {
		if step.StepName == StepAttribute {
			storedAttempts = append(storedAttempts, step.Attempt)
			if step.Attempt == 1 && step.BackoffMS != 1000 {
				t.Fatalf("落库的失败尝试应带退避 1000ms，实际 %d", step.BackoffMS)
			}
		}
	}
	if len(storedAttempts) != 2 || storedAttempts[0] != 1 || storedAttempts[1] != 2 {
		t.Fatalf("落库的归因尝试应为 attempt 1 与 2，实际 %v", storedAttempts)
	}
}

// TestRetry_ExhaustedFailsRun 验证重试耗尽的落点：
// 三次都失败时 run 落失败态并保留最后一次错误，绝不标成成功。
func TestRetry_ExhaustedFailsRun(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	samples := excursionSamples()

	calls := 0
	chain, err := NewChain(ChainDeps{
		Profile: testProfile(),
		Samples: samples,
		Attribute: func(ctx context.Context, in StepInput) (any, error) {
			calls++
			return nil, errcode.Newf(errcode.ModelUnavailable, "模型服务不可达：第 %d 次", calls)
		},
	})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	waits := &waitRecorder{}
	exec := NewExecutor(chain, st, quietLogger()).WithWaitFunc(waits.wait)

	result, err := exec.Run(ctx, directRun(t, samples))
	if err == nil {
		t.Fatal("重试耗尽后整条链应报错")
	}
	if code := errcode.As(err).Code; code != errcode.ModelUnavailable {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.ModelUnavailable, code)
	}
	if calls != 3 {
		t.Fatalf("应尝试 3 次，实际 %d 次", calls)
	}
	if result.Run.ExecState != domain.ExecStateFailed {
		t.Fatalf("执行态应落到 failed，实际 %q", result.Run.ExecState)
	}
	if result.Run.State == domain.RunStateAwaitingConfirmation {
		t.Fatal("重试耗尽时业务状态不得推进到待主管确认")
	}
	if result.FailedStep != StepAttribute {
		t.Fatalf("失败步骤应为 %q，实际 %q", StepAttribute, result.FailedStep)
	}
	if result.ErrorCode != errcode.ModelUnavailable {
		t.Fatalf("结果里的错误码应为 %s，实际 %s", errcode.ModelUnavailable, result.ErrorCode)
	}

	// 退避序列指数增长：1s、2s（第三次失败后不再等）。
	if waits.attempts() != 2 {
		t.Fatalf("应退避等待 2 次，实际 %d 次", waits.attempts())
	}
	if waits.durations[0] != time.Second || waits.durations[1] != 2*time.Second {
		t.Fatalf("退避序列应为 [1s 2s]，实际 %v", waits.durations)
	}

	// 三次尝试各留一行：失败留痕，不静默改数据。
	var attrAttempts []int
	for _, entry := range Timeline(result.Steps) {
		if entry.StepName == StepAttribute {
			attrAttempts = append(attrAttempts, entry.Attempt)
			if entry.State != domain.StepStateFailed {
				t.Fatalf("第 %d 次尝试状态应为 failed，实际 %q", entry.Attempt, entry.State)
			}
			if entry.ErrorCode != string(errcode.ModelUnavailable) {
				t.Fatalf("第 %d 次尝试应带错误码，实际 %q", entry.Attempt, entry.ErrorCode)
			}
		}
	}
	if len(attrAttempts) != 3 {
		t.Fatalf("归因步骤应留 3 条尝试记录，实际 %d 条", len(attrAttempts))
	}

	// 最后一次失败没有退避时长：后面不再等了。
	last := Timeline(result.Steps)
	for _, entry := range last {
		if entry.StepName == StepAttribute && entry.Attempt == 3 && entry.BackoffMS != 0 {
			t.Fatalf("最后一次失败不应带退避时长，实际 %d", entry.BackoffMS)
		}
	}

	// 落库同样三条。
	stored, err := st.RunStepsByRun(ctx, result.Run.RunID)
	if err != nil {
		t.Fatalf("回读步骤记录失败：%v", err)
	}
	count := 0
	for _, step := range stored {
		if step.StepName == StepAttribute {
			count++
		}
	}
	if count != 3 {
		t.Fatalf("落库的归因尝试应为 3 条，实际 %d 条", count)
	}
}

// TestRetry_NonRetryableFailsImmediately 验证不可重试的错误一次就停：
// 校验类与状态迁移类错误再试也是一样的错，不占退避时间。
func TestRetry_NonRetryableFailsImmediately(t *testing.T) {
	for _, blocked := range []errcode.Code{errcode.Validation, errcode.IllegalTransition} {
		t.Run(string(blocked), func(t *testing.T) {
			ctx := context.Background()
			st := openStore(t)
			samples := excursionSamples()

			calls := 0
			chain, err := NewChain(ChainDeps{
				Profile: testProfile(),
				Samples: samples,
				Attribute: func(ctx context.Context, in StepInput) (any, error) {
					calls++
					return nil, errcode.Newf(blocked, "%s：重试也不会变", blocked)
				},
			})
			if err != nil {
				t.Fatalf("装配步骤链失败：%v", err)
			}
			waits := &waitRecorder{}
			exec := NewExecutor(chain, st, quietLogger()).WithWaitFunc(waits.wait)

			result, err := exec.Run(ctx, directRun(t, samples))
			if err == nil {
				t.Fatalf("%s 应直接判失败", blocked)
			}
			if code := errcode.As(err).Code; code != blocked {
				t.Fatalf("错误码应为 %s，实际 %s", blocked, code)
			}
			if calls != 1 {
				t.Fatalf("%s 只应尝试 1 次，实际 %d 次", blocked, calls)
			}
			if waits.attempts() != 0 {
				t.Fatalf("%s 不应触发退避等待，实际等了 %d 次", blocked, waits.attempts())
			}
			if result.Run.ExecState != domain.ExecStateFailed {
				t.Fatalf("执行态应落到 failed，实际 %q", result.Run.ExecState)
			}
			// 只留一条记录：没有第二次尝试，也就没有第二次尝试的记录。
			count := 0
			for _, entry := range Timeline(result.Steps) {
				if entry.StepName == StepAttribute {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("%s 应只留 1 条尝试记录，实际 %d 条", blocked, count)
			}
		})
	}
}

// TestRetry_ContextCancelDuringBackoff 验证退避等待可被取消：
// 判定被取消时不闷头等满，直接把取消原因交回去。
func TestRetry_ContextCancelDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	st := openStore(t)
	samples := excursionSamples()

	chain, err := NewChain(ChainDeps{
		Profile: testProfile(),
		Samples: samples,
		Attribute: func(ctx context.Context, in StepInput) (any, error) {
			return nil, errcode.Newf(errcode.ModelUnavailable, "模型服务不可达")
		},
	})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	// 等待函数里直接取消：模拟「退避刚开始就被取消」。
	exec := NewExecutor(chain, st, quietLogger()).WithWaitFunc(
		func(ctx context.Context, d time.Duration) error {
			cancel()
			return ctx.Err()
		})

	result, err := exec.Run(ctx, directRun(t, samples))
	if err == nil {
		t.Fatal("退避等待被取消时整条链应报错")
	}
	if !errors.Is(err, context.Canceled) && result.ErrorCode != errcode.Validation {
		t.Fatalf("应报出取消原因，实际 %v（错误码 %s）", err, result.ErrorCode)
	}
	if result.Run.ExecState != domain.ExecStateFailed {
		t.Fatalf("执行态应落到 failed，实际 %q", result.Run.ExecState)
	}
}

// TestRetryPolicy_ShouldRetry 验证重试判定表：
// 还有次数 + 错误可重试 → 重试；次数用完 → 不重试；不可重试码 → 不重试。
func TestRetryPolicy_ShouldRetry(t *testing.T) {
	policy := DefaultRetryPolicy()

	if policy.ShouldRetry(nil, 1) {
		t.Fatal("没有错误时不应重试")
	}
	if !policy.ShouldRetry(errcode.Newf(errcode.ModelTimeout, "超时"), 1) {
		t.Fatal("模型超时属可重试错误")
	}
	if !policy.ShouldRetry(errcode.Newf(errcode.LedgerUnavailable, "台账不可达"), 1) {
		t.Fatal("台账不可达属可重试错误")
	}
	if policy.ShouldRetry(errcode.Newf(errcode.ModelTimeout, "超时"), policy.MaxAttempts) {
		t.Fatal("次数用完不应再重试")
	}
	if policy.ShouldRetry(errcode.Newf(errcode.Validation, "字段不合法"), 1) {
		t.Fatal("校验类错误不应重试")
	}
	if policy.ShouldRetry(errcode.Newf(errcode.IllegalTransition, "迁移非法"), 1) {
		t.Fatal("状态迁移类错误不应重试")
	}
	// 非结构化错误按 E_VALIDATION 处理，同样不重试。
	if policy.ShouldRetry(errors.New("普通错误"), 1) {
		t.Fatal("非结构化错误按校验类处理，不应重试")
	}
}

// TestRetry_FactMismatchNotRetried 验证模型事实不符不再重试：
// 模型层已经带反馈重问过一次，仍不符说明该退回规则基线；
// 步骤层再问一遍只会让退避时长叠乘，一次判定拖成好几分钟。
func TestRetry_FactMismatchNotRetried(t *testing.T) {
	policy := DefaultRetryPolicy()
	if policy.ShouldRetry(errcode.Newf(errcode.ModelFactMismatch, "峰值温度 13.7℃ 在输入时序里找不到"), 1) {
		t.Fatal("模型事实不符不应在步骤层重试")
	}

	ctx := context.Background()
	st := openStore(t)
	samples := excursionSamples()

	calls := 0
	chain, err := NewChain(ChainDeps{
		Profile: testProfile(),
		Samples: samples,
		Attribute: func(ctx context.Context, in StepInput) (any, error) {
			calls++
			return nil, errcode.Newf(errcode.ModelFactMismatch, "峰值温度 13.7℃ 在输入时序里找不到")
		},
	})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	waits := &waitRecorder{}
	exec := NewExecutor(chain, st, quietLogger()).WithWaitFunc(waits.wait)

	result, err := exec.Run(ctx, directRun(t, samples))
	if err == nil {
		t.Fatal("事实不符应直接判失败")
	}
	if code := errcode.As(err).Code; code != errcode.ModelFactMismatch {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.ModelFactMismatch, code)
	}
	if calls != 1 {
		t.Fatalf("事实不符只应尝试 1 次，实际 %d 次", calls)
	}
	if waits.attempts() != 0 {
		t.Fatalf("事实不符不应触发退避等待，实际等了 %d 次", waits.attempts())
	}
	if result.Run.ExecState != domain.ExecStateFailed {
		t.Fatalf("执行态应落到 failed，实际 %q", result.Run.ExecState)
	}
}

// TestRetryPolicy_BackoffExponentialAndCapped 验证退避时长指数增长并封顶。
func TestRetryPolicy_BackoffExponentialAndCapped(t *testing.T) {
	policy := RetryPolicy{
		MaxAttempts: 6,
		BaseBackoff: time.Second,
		MaxBackoff:  5 * time.Second,
	}.Normalize()

	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second}
	for i, expect := range want {
		got := policy.Backoff(i + 1)
		if got != expect {
			t.Fatalf("第 %d 次失败后的退避应为 %v，实际 %v", i+1, expect, got)
		}
	}
	if got := policy.Backoff(0); got != time.Second {
		t.Fatalf("退避序号非法时应收敛到首次退避，实际 %v", got)
	}
}

// TestRetryPolicy_Normalize 验证非法取值被修回默认，而不是变成「不重试」。
func TestRetryPolicy_Normalize(t *testing.T) {
	normalized := RetryPolicy{}.Normalize()
	if normalized.MaxAttempts != defaultMaxAttempts {
		t.Fatalf("最大尝试次数应回落默认 %d，实际 %d", defaultMaxAttempts, normalized.MaxAttempts)
	}
	if normalized.BaseBackoff != defaultBaseBackoff {
		t.Fatalf("退避起步应回落默认 %v，实际 %v", defaultBaseBackoff, normalized.BaseBackoff)
	}
	if normalized.MaxBackoff != defaultMaxBackoff {
		t.Fatalf("退避上限应回落默认 %v，实际 %v", defaultMaxBackoff, normalized.MaxBackoff)
	}
	if len(normalized.NonRetryable) == 0 {
		t.Fatal("不可重试清单不应为空")
	}

	// 上限小于起步时，上限被抬到起步值：退避不能是负数或倒挂。
	odd := RetryPolicy{MaxAttempts: 2, BaseBackoff: 10 * time.Second, MaxBackoff: time.Second}.Normalize()
	if odd.MaxBackoff != 10*time.Second {
		t.Fatalf("退避上限应抬到与起步一致，实际 %v", odd.MaxBackoff)
	}
}

// TestRetry_NoRetryWhenPolicyDisabled 验证关掉重试时行为回到「一次就失败」：
// MaxAttempts=1 时不该有任何退避等待，也不该留下第二次尝试记录。
func TestRetry_NoRetryWhenPolicyDisabled(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	samples := excursionSamples()

	calls := 0
	chain, err := NewChain(ChainDeps{
		Profile: testProfile(),
		Samples: samples,
		Attribute: func(ctx context.Context, in StepInput) (any, error) {
			calls++
			return nil, errcode.Newf(errcode.ModelUnavailable, "模型服务不可达")
		},
	})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	waits := &waitRecorder{}
	exec := NewExecutor(chain, st, quietLogger()).
		WithRetryPolicy(RetryPolicy{MaxAttempts: 1}).
		WithWaitFunc(waits.wait)

	if _, err := exec.Run(ctx, directRun(t, samples)); err == nil {
		t.Fatal("关闭重试后归因失败应直接报错")
	}
	if calls != 1 {
		t.Fatalf("关闭重试后只应尝试 1 次，实际 %d 次", calls)
	}
	if waits.attempts() != 0 {
		t.Fatalf("关闭重试后不应退避等待，实际等了 %d 次", waits.attempts())
	}
}
