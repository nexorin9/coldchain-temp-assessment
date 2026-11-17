// 本文件把「每一次模型调用」都变成一条可审计的记录。
//
// 为什么要有这一层：模型归因的结论会写进药事台账、递到疫苗生产企业面前，
// 内审与飞检会追问「这条结论是怎么来的、花了多少、谁认的」。
// 靠翻日志回答不了——日志是流水，审计链是账本。
//
// 记录什么：
//
//	模型名、这次调用属于哪条判定哪一步、耗时、输入 / 输出 token、
//	估算成本、结果状态（成功 / 失败 / 降级）、失败时的错误码。
//
// 三条硬约束：
//
//  1. 凭据与鉴权头不进记录（登记的原文由审计层抹掉）；
//  2. 提示词原文不进记录——只留指纹与字符数；
//  3. 成本算不出来时记 unknown，不记 0。
//
// 第 3 条最要紧：0 会被读成「这次调用不花钱」，那是结论，不是事实；
// 事实是「本服务不知道单价」。
//
// 记录发生在**事实校验之后**：一次调用答得不合规、或答的数与现场对不上，
// 都是「这次调用失败了」——审计链要如实这么写，不能因为 HTTP 200 就记成功。
package model

import (
	"context"
	"strings"

	"github.com/chaosforge/coldchain-temp-assessment/internal/audit"
	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
)

// AuditRecorder 是模型调用审计的落库出口。
//
// 定义在 internal/audit 里（那边是审计词汇的归属地）：
// 由该包的 Recorder 实现。模型层只认这一个方法——
// 落库是 SQLite 还是别的库、审计表怎么建，都不关模型层的事。
type AuditRecorder = audit.AuditRecorder

// logAuditPersistFailure 记下「审计记录没写进库」这件事。
//
// 审计写不进去不该篡改业务结论——一次本来通过的判定不能因为审计表写不进去
// 就变成失败。但也不能装作没发生：记进服务日志，复核时至少知道这条记录缺了。
func logAuditPersistFailure(err error) {
	if err == nil {
		return
	}
	logging.Default().Error("model_audit_persist_failed", "error", err.Error())
}

// callIdentity 是一次调用在判定链里的身份。
//
// 取值顺序：Asker 上写死的字段优先，其次是调用链上下文里带的那一份
// （`audit.WithCallContext`）——上下文那份让「这次调用属于哪条判定」
// 不用靠全局变量追来追去。
func (a *Asker) callIdentity(ctx context.Context) (runID, stepName string) {
	if a != nil {
		runID = strings.TrimSpace(a.RunID)
		stepName = strings.TrimSpace(a.StepName)
	}
	if runID == "" || stepName == "" {
		callCtx := audit.CallContextFrom(ctx)
		if runID == "" {
			runID = callCtx.RunID
		}
		if stepName == "" {
			stepName = callCtx.StepName
		}
	}
	return runID, stepName
}

// recordCall 把一次模型调用记进审计链。
//
// outcome 由调用方给：调用层只管「发出去没有、回没回来」，
// 「回来说的是不是真的」由事实校验判定——两边合起来才是这次调用的结果。
//
// 记不记由两件事决定：
//   - 有没有装配审计记录器（没装配就静默跳过，不影响判定）；
//   - 有没有 run 标识（审计链是挂在某条判定上的，认不出 run 的调用无处可挂）。
func (a *Asker) recordCall(ctx context.Context, schema SchemaName, completion *Completion,
	outcome, errorCode string, degraded bool) {
	if a == nil || a.Auditor == nil {
		return
	}
	runID, stepName := a.callIdentity(ctx)
	if runID == "" {
		return
	}
	call := audit.ModelCall{
		RunID:      runID,
		StepName:   stepName,
		SchemaName: string(schema),
		Outcome:    outcome,
		Degraded:   degraded,
		ErrorCode:  errorCode,
	}
	if completion != nil {
		call.Model = completion.Model
		call.LatencyMS = completion.LatencyMS
		call.PromptTokens = completion.PromptTokens
		call.CompletionTokens = completion.CompletionTokens
		call.TotalTokens = completion.TotalTokens
		call.PromptDigest = completion.PromptDigest
		call.PromptChars = completion.PromptChars
	}
	if err := a.Auditor.RecordModelCall(ctx, call); err != nil {
		logAuditPersistFailure(err)
	}
}

// auditErrorCode 把错误映射成错误码字符串；没有错误时留空。
func auditErrorCode(err error) string {
	if err == nil {
		return ""
	}
	e := errcode.As(err)
	if e == nil {
		return ""
	}
	return string(e.Code)
}

// RecordDegrade 把一次降级记进模型调用审计链。
//
// 降级在审计链上是**独立的一条**：前面的「调用失败」记录说的是「这次调用没成」，
// 这一条说的是「所以最终结论退回了规则基线」。两条分开记，复核的人才分得清
// 「模型答了但不合规」与「模型没答成、我们退回了」——处置上后者还多一件事：
// 要去查模型端点为什么不可用。
//
// 没装配审计记录器时静默跳过：审计写不进去不该让一次本来能完成的判定变成失败。
func (a *Asker) RecordDegrade(ctx context.Context, schema SchemaName, cause error) {
	if a == nil {
		return
	}
	a.recordCall(ctx, schema, nil, auditOutcomeDegraded, auditErrorCode(cause), true)
}

// 审计链的结果状态取值直接复用领域常量：
// 口径只有一处定义，端点读的时候不必对照两份枚举。
const (
	auditOutcomeSucceeded = domain.AuditOutcomeSucceeded
	auditOutcomeFailed    = domain.AuditOutcomeFailed
	auditOutcomeDegraded  = domain.AuditOutcomeDegraded
)
