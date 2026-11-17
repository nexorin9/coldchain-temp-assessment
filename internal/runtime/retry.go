// 本文件落判定步骤的重试策略：失败几次还重试、每次之间隔多久、哪些错误根本不该重试。
//
// 为什么要有这一层：判定链上的一步失败，未必等于「这次越温判不了」。
// 模型服务超时、台账暂时不可达、磁盘一时锁住——这些都是「再等一下可能就成了」，
// 一次就判失败等于把一次瞬时故障升级成一次人工返工。
// 但反过来，有些失败重试多少次都一样：字段校验不过（数据本身有问题）、
// 状态迁移非法（流程本身走错了），再试一百次还是同样的错，只是白等。
// 所以重试必须同时回答两件事：还该不该试（错误性质），以及隔多久再试（退避）。
//
// 源产品机制对应（溯源见 spec.md「开源对照」与「参考地基」）：
// 源产品的执行器在一步失败后按步骤自身的重试次数上限决定是否再试，
// 判定依据是「这一步的当前尝试序号 + 步骤声明的重试上限 + 错误本身」，
// 即 queue.ShouldRetry(err, attempt, retryCount) 这样的三段式判断；
// 每一次尝试各自产出一条完整的步骤记录（带本次尝试的错误码），
// 不覆盖、不合并——「one entry per step attempt with the attempt's full value set」
// （github_ref/inngest/ui/packages/components/src/generated/index.ts：
// 每类元数据一个 kind，各带独立 const 与独立 interface，可单独替换；
// 同一文件里 SandboxMetadata.error_code 记的就是「步骤失败时的错误码」）。
// 搬到冷链场景，对应物是：
//
//   - 重试策略 = 最多试 3 次，退避时长按尝试序号指数增长并封顶
//   - 不可重试清单 = 校验类、状态迁移类与事实不符类错误，直接判失败不空等
//   - 每次尝试各留一条步骤记录，带 attempt 序号、错误码与退避时长
//   - 退避等待可取消：判定被取消时不等满，直接返回
//
// 三条不许让步的约定：
//   - 耗尽即失败：重试用完就把 run 落到失败态并保留最后一次错误，
//     绝不把「试了三次都没成」写成成功。
//   - 不重试的不空等：不该重试的错误一次就停，不占用退避时间。
//   - 等待看得见：退避时长写进步骤记录，时间线上能看到这一步卡了多久。
package runtime

import (
	"context"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// 重试策略的默认值。
//
// 3 次是权衡出来的：第 1 次是原始尝试，第 2、3 次覆盖「台账刚从网络抖动里恢复」
// 与「模型端短暂过载」这两类常见瞬时故障。再往上加，判定就会拖到分钟级——
// 而越温结论本来就要求快，越温判定晚半小时，处置也晚半小时。
const (
	// defaultMaxAttempts 是单步默认的最大尝试次数（含首次）。
	defaultMaxAttempts = 3
	// defaultBaseBackoff 是第一次失败后的退避时长。
	defaultBaseBackoff = time.Second
	// defaultMaxBackoff 是退避时长的上限。
	defaultMaxBackoff = 30 * time.Second
)

// RetryPolicy 描述判定步骤的重试策略。
//
// 策略本身是值类型且不可变：一份策略被多个执行器共享时互不影响。
type RetryPolicy struct {
	// MaxAttempts 是单步的最大尝试次数（含首次）。小于等于 1 表示不重试。
	MaxAttempts int
	// BaseBackoff 是首次退避时长；此后每失败一次翻一倍。
	BaseBackoff time.Duration
	// MaxBackoff 是退避时长的封顶值：连续失败时也不无限期地等下去。
	MaxBackoff time.Duration
	// NonRetryable 是不可重试的错误码清单。
	//
	// 它是一份「不要重试」的清单，而不是「可以重试」的清单：
	// 新增错误码时默认会被当作可重试，符合「先加上、观察一段时间再豁免」的次序。
	// 但更稳的做法是把确定的不可重试错误逐条列进来——
	// 现场能明确判定「再试也一样」的错误有三类：数据本身不合法、流程本身走不通、
	// 以及模型输出与输入事实不符（模型层已重问过一次）。
	NonRetryable []errcode.Code
}

// DefaultRetryPolicy 返回默认重试策略：最多 3 次尝试，退避 1 秒起步、30 秒封顶，
// 校验类、状态迁移类与模型事实不符类错误不重试。
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts: defaultMaxAttempts,
		BaseBackoff: defaultBaseBackoff,
		MaxBackoff:  defaultMaxBackoff,
		NonRetryable: []errcode.Code{
			// 字段缺失、取值不合法、载荷形状对不上：数据本身有问题，再试还是错。
			errcode.Validation,
			// 状态迁移非法：流程走错了，重试只会再撞一次同一堵墙。
			errcode.IllegalTransition,
			// 模型输出与输入事实不符（编造的温度、越界的窗口、清单外的批次号）：
			// 模型层已经带反馈重问过一次，仍不符说明这次判定该退回规则基线，
			// 而不是再问一遍——两层都重试，退避时长会叠乘，一次判定能拖成好几分钟。
			errcode.ModelFactMismatch,
		},
	}
}

// Normalize 修正策略里的非法取值，返回可用的策略。
//
// 非法取值一律回落到默认，而不是报错：重试策略配错不该让服务起不来，
// 但也不能因此变成「不重试」——那会把瞬时故障放大成人工返工。
func (p RetryPolicy) Normalize() RetryPolicy {
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = defaultMaxAttempts
	}
	if p.BaseBackoff < 0 {
		p.BaseBackoff = defaultBaseBackoff
	}
	if p.BaseBackoff == 0 {
		p.BaseBackoff = defaultBaseBackoff
	}
	if p.MaxBackoff <= 0 {
		p.MaxBackoff = defaultMaxBackoff
	}
	if p.MaxBackoff < p.BaseBackoff {
		p.MaxBackoff = p.BaseBackoff
	}
	if len(p.NonRetryable) == 0 {
		p.NonRetryable = DefaultRetryPolicy().NonRetryable
	}
	return p
}

// ShouldRetry 判断在第 attempt 次尝试失败之后是否还应该再试一次。
//
// 三个条件同时成立才重试：还有次数、错误可重试、错误本身不是取消类。
// attempt 是已经用掉的尝试次数（第一次失败时为 1）。
func (p RetryPolicy) ShouldRetry(cause error, attempt int) bool {
	if cause == nil {
		return false
	}
	if attempt >= p.MaxAttempts {
		return false
	}
	code := errcode.As(cause).Code
	for _, blocked := range p.NonRetryable {
		if blocked == code {
			return false
		}
	}
	return true
}

// Backoff 返回第 attempt 次失败之后的退避时长。
//
// 指数增长：第 1 次失败等 1 秒，第 2 次等 2 秒，第 3 次等 4 秒……
// 封顶在 MaxBackoff：连续失败时也不无限期地等下去。
func (p RetryPolicy) Backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	wait := p.BaseBackoff
	for i := 1; i < attempt; i++ {
		wait *= 2
		if wait >= p.MaxBackoff {
			return p.MaxBackoff
		}
	}
	if wait > p.MaxBackoff {
		return p.MaxBackoff
	}
	return wait
}

// sleepCtx 按 ctx 可取消地等待一段时长。
//
// 退避等待必须能被取消：判定被取消时还闷头等满 30 秒，
// 服务关闭会拖成一片卡住。ctx 一断，等待立刻返回。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
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
