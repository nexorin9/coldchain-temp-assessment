// 本文件落「药学部主管确认 / 否决处置结论」这一段的编排：把判定链停在「待确认」
// 的那条 run 推到「已确认」，并把要写回台账的内容在确认这一刻固化下来。
//
// 为什么放在运行时层而不是状态机层：状态机（internal/workflow）只回答
// 「这条边存不存在、谁有资格走」；确认动作还要回答三件更具体的事——
// 成稿在不在、要素齐不齐、台账里最终写的是什么。这三件都要读判定产物，
// 而判定产物归运行时层管。
//
// 一条纪律贯穿全文件：**确认本身不出站**。
// 确认只做四件事——推进状态、固化载荷、记确认记录、记审计链；
// 它不写冷链台账、不发门诊通知。出站写回是另一段流程的事，
// 且必须先过 workflow.AuthorizeWriteback 那道闸门（未确认不得写回）。
// 于是有一条推得出来的结论：**未确认的 run，出站写回调用数恒为 0**。
//
// 落库顺序也有讲究：先写确认记录（连同固化的载荷），再推进 run 状态。
// 万一状态推进失败，确认记录在、状态没动——写回侧看的是状态，
// 闸门照样拦得住；反过来先推状态后写记录，一旦记录写失败，
// 就留下一条「已确认但没有载荷」的判定，只能靠人补。
package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/audit"
	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/drafting"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// ConfirmationStore 是确认动作需要的读写口。
//
// 抽成接口是为了让运行时层不绑死存储实现：生产用 SQLite，测试用内存实现，
// 两者口径一致。
type ConfirmationStore interface {
	// RunByID 回读一条判定 run 的状态快照。
	RunByID(ctx context.Context, runID string) (domain.RunRecord, error)
	// RunStepsByRun 回读一条 run 的步骤记录（含各步产物）。
	RunStepsByRun(ctx context.Context, runID string) ([]domain.Step, error)
	// UpsertRun 写入或更新一条判定 run 的状态快照。
	UpsertRun(ctx context.Context, rec domain.RunRecord) error
	// SaveConfirmation 写入（或更新）一条确认记录。
	SaveConfirmation(ctx context.Context, rec domain.Confirmation) error
	// InsertRunStep 写入或更新一条步骤记录。
	InsertRunStep(ctx context.Context, step domain.Step) error
}

// ConfirmationAuditor 是确认动作的审计落库口。
type ConfirmationAuditor interface {
	// RecordConfirmation 落一条人工确认动作的审计记录。
	RecordConfirmation(ctx context.Context, c audit.Confirmation) error
}

// ConfirmationOutcome 是一次确认 / 否决动作的完整结果。
//
// 它比决策本身多三样东西：这条判定在台账侧的外部标识与幂等键、
// 确认之后的写回状态、以及「载荷是否已固化」。
// 调用方（接口层）据此回执：主管确认完当场就能看到
// 「台账里将会出现的是哪一条、幂等键是多少、写回状态到哪一步了」。
type ConfirmationOutcome struct {
	// Decision 是这次决策本身（状态、处置、确认人、迁移留痕）。
	Decision workflow.ConfirmResult
	// ExternalRef 是这条判定的外部引用；台账按它识别同一次越温。
	ExternalRef string
	// IdempotencyKey 是这条判定的幂等键；台账侧按它防重复入账。
	IdempotencyKey string
	// WritebackState 是确认之后的写回状态（待写回）。
	WritebackState string
	// PayloadFrozen 表示台账写回载荷是否已在确认这一刻固化。
	// 否决时为 false：被否决的判定不写台账。
	PayloadFrozen bool
}

// Confirmer 是「主管确认 / 否决」这一段流程的编排者。
type Confirmer struct {
	store   ConfirmationStore
	auditor ConfirmationAuditor
	now     func() time.Time
	// logger 记录确认 / 否决动作；为 nil 时回落到默认日志器（写 stderr）。
	logger *logging.Logger
}

// NewConfirmer 装配确认编排器。
func NewConfirmer(store ConfirmationStore, auditor ConfirmationAuditor) *Confirmer {
	return &Confirmer{
		store:   store,
		auditor: auditor,
		now:     time.Now,
		logger:  logging.Default(),
	}
}

// WithLogger 注入结构化日志器并返回自身，便于链式装配。
func (c *Confirmer) WithLogger(l *logging.Logger) *Confirmer {
	if l != nil {
		c.logger = l
	}
	return c
}

// 确认动作在日志里的步骤名。
//
// 确认不是判定链上的五步之一（取数 / 抽窗口 / 归因 / 成稿 / 待确认），
// 但它同样是「这条判定走到了哪一步」的一部分——
// 服务日志里按 run_id 串起来时，主管点头这一下也该有个名字。
const (
	confirmStepName = "confirm"
	rejectStepName  = "reject"
)

// WithClock 注入时钟；测试可注入固定时钟，让确认时刻可复现。
func (c *Confirmer) WithClock(now func() time.Time) *Confirmer {
	if now != nil {
		c.now = now
	}
	return c
}

// Confirm 确认一条待确认的判定：推进状态、固化写回载荷、记确认记录与审计链。
//
// 四步，顺序不能换：
//  1. 决策：角色、当前状态、处置取值域都在这一步把关（internal/workflow）。
//  2. 组装载荷：成稿在不在、要素齐不齐、批次带没带，都在这一步把关。
//  3. 落确认记录：连同固化下来的写回载荷一起落库。
//  4. 推进状态并记账：run 状态、迁移留痕、成稿产物里的最终处置、审计链。
//
// 任一步失败都直接返回错误，**不改**已经落库的东西——状态被误推进比
// 状态推不动危险得多。
func (c *Confirmer) Confirm(ctx context.Context, runID string, req workflow.ConfirmRequest) (ConfirmationOutcome, error) {
	if c == nil || c.store == nil {
		return ConfirmationOutcome{}, errcode.Newf(errcode.StorageError, "确认能力未装配")
	}
	if c.auditor == nil {
		return ConfirmationOutcome{}, errcode.Newf(errcode.StorageError,
			"确认能力未装配审计落库：没有审计链的确认不予放行")
	}
	req.RunID = strings.TrimSpace(runID)
	if req.At.IsZero() {
		req.At = c.now()
	}

	rec, steps, err := c.load(ctx, req.RunID)
	if err != nil {
		return ConfirmationOutcome{}, err
	}

	// 1. 决策：谁能确认、从哪能确认、处置取值合不合法。
	result, err := workflow.Confirm(workflow.State(rec.Run.State), req)
	if err != nil {
		return ConfirmationOutcome{}, err
	}

	// 2. 组装写回载荷：成稿在不在、要素齐不齐，这一层说了算。
	payload, record, err := c.buildPayload(steps, rec, result)
	if err != nil {
		return ConfirmationOutcome{}, err
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return ConfirmationOutcome{}, errcode.Newf(errcode.StorageError,
			"序列化台账写回载荷失败：%v", err)
	}

	// 3. 落确认记录：载荷在这一刻固化，之后写回侧原样发出。
	confirmation := domain.Confirmation{
		RunID:            req.RunID,
		Action:           domain.ConfirmationActionConfirm,
		ConfirmedBy:      result.ConfirmedBy,
		ConfirmedAt:      result.ConfirmedAt,
		Disposition:      result.Disposition,
		DispositionLabel: result.DispositionLabel,
		Opinion:          result.Opinion,
		PayloadJSON:      string(payloadJSON),
		CreatedAt:        result.ConfirmedAt,
	}
	if err := c.store.SaveConfirmation(ctx, confirmation); err != nil {
		return ConfirmationOutcome{}, err
	}

	// 4. 推进状态并记账。
	if err := c.applyDecision(ctx, rec, steps, record, result); err != nil {
		c.logger.Error("run_confirm_failed",
			"run_id", req.RunID,
			"step_name", confirmStepName,
			"error_code", string(errcode.As(err).Code),
			"error", err.Error())
		return ConfirmationOutcome{}, err
	}
	c.logger.Info("run_confirmed",
		"run_id", req.RunID,
		"step_name", confirmStepName,
		"actor", result.ConfirmedBy,
		"disposition", result.Disposition,
		"from_state", string(result.FromState),
		"to_state", string(result.ToState))
	return ConfirmationOutcome{
		Decision:       result,
		ExternalRef:    rec.Run.ExternalRef,
		IdempotencyKey: rec.Run.IdempotencyKey,
		WritebackState: domain.WritebackStatePending,
		PayloadFrozen:  true,
	}, nil
}

// Reject 否决一条判定：推进到「已否决」，不产生任何写回载荷。
//
// 被否决的判定不写冷链台账——台账里多一条没发生过的越温，
// 比少记一条更麻烦：前者会让台账的越温统计对不上真实的温控记录。
func (c *Confirmer) Reject(ctx context.Context, runID string, req workflow.ConfirmRequest) (ConfirmationOutcome, error) {
	if c == nil || c.store == nil {
		return ConfirmationOutcome{}, errcode.Newf(errcode.StorageError, "确认能力未装配")
	}
	if c.auditor == nil {
		return ConfirmationOutcome{}, errcode.Newf(errcode.StorageError,
			"确认能力未装配审计落库：没有审计链的确认不予放行")
	}
	req.RunID = strings.TrimSpace(runID)
	if req.At.IsZero() {
		req.At = c.now()
	}

	rec, _, err := c.load(ctx, req.RunID)
	if err != nil {
		return ConfirmationOutcome{}, err
	}

	result, err := workflow.Reject(workflow.State(rec.Run.State), req)
	if err != nil {
		return ConfirmationOutcome{}, err
	}

	confirmation := domain.Confirmation{
		RunID:       req.RunID,
		Action:      domain.ConfirmationActionReject,
		ConfirmedBy: result.ConfirmedBy,
		ConfirmedAt: result.ConfirmedAt,
		Opinion:     result.Opinion,
		CreatedAt:   result.ConfirmedAt,
	}
	if err := c.store.SaveConfirmation(ctx, confirmation); err != nil {
		return ConfirmationOutcome{}, err
	}
	if err := c.applyDecision(ctx, rec, nil, nil, result); err != nil {
		c.logger.Error("run_reject_failed",
			"run_id", req.RunID,
			"step_name", rejectStepName,
			"error_code", string(errcode.As(err).Code),
			"error", err.Error())
		return ConfirmationOutcome{}, err
	}
	c.logger.Info("run_rejected",
		"run_id", req.RunID,
		"step_name", rejectStepName,
		"actor", result.ConfirmedBy,
		"from_state", string(result.FromState),
		"to_state", string(result.ToState))
	return ConfirmationOutcome{
		Decision:       result,
		ExternalRef:    rec.Run.ExternalRef,
		IdempotencyKey: rec.Run.IdempotencyKey,
		WritebackState: rec.WritebackState,
		PayloadFrozen:  false,
	}, nil
}

// load 读出这条 run 的状态快照与步骤记录。
func (c *Confirmer) load(ctx context.Context, runID string) (domain.RunRecord, []domain.Step, error) {
	if strings.TrimSpace(runID) == "" {
		return domain.RunRecord{}, nil, errcode.Newf(errcode.Validation, "确认动作缺少 run_id")
	}
	rec, err := c.store.RunByID(ctx, runID)
	if err != nil {
		return domain.RunRecord{}, nil, err
	}
	steps, err := c.store.RunStepsByRun(ctx, runID)
	if err != nil {
		return domain.RunRecord{}, nil, err
	}
	return rec, steps, nil
}

// buildPayload 从成稿产物与确认事实组装台账写回载荷。
//
// 它返回组装好的载荷与更新后的记录表载荷：确认后的最终处置要写回记录表产物，
// 这样查判定结论（GET /v1/runs/{id}）时看到的处置栏与台账里写的是同一个值。
func (c *Confirmer) buildPayload(steps []domain.Step, rec domain.RunRecord,
	result workflow.ConfirmResult) (drafting.WritebackSource, *domain.ExceptionRecordPayload, error) {
	record, ok := ProductAs[domain.ExceptionRecordPayload](steps, domain.PayloadExceptionRecord)
	if !ok {
		return drafting.WritebackSource{}, nil, errcode.Newf(errcode.Validation,
			"判定 run %s 尚未成稿（没有成稿产物），不能确认", rec.Run.RunID)
	}

	// 确认后的最终处置写回记录表载荷：要素校验看的正是「最终处置」这一栏，
	// 确认前它必然是缺的（系统只给建议），确认后这一栏才真的填上了。
	record.ConfirmedDisposition = result.Disposition

	window, _ := ProductAs[domain.WindowPayload](steps, domain.PayloadWindow)
	attribution, _ := ProductAs[domain.AttributionPayload](steps, domain.PayloadAttribution)
	quality := drafting.CheckQuality(drafting.Inputs{
		Record:      record,
		Window:      window,
		Attribution: attribution,
	}, result.ConfirmedAt)
	if !quality.Exportable() {
		return drafting.WritebackSource{}, nil, errcode.Newf(errcode.Validation,
			"成稿要素不全，不得确认并写回冷链台账：缺少 %s",
			strings.Join(quality.BlockingFields, "、"))
	}
	record.Quality = quality.Domain()

	facts := drafting.ConfirmFacts{
		Disposition:    result.Disposition,
		ConfirmedBy:    result.ConfirmedBy,
		ConfirmedAt:    result.ConfirmedAt,
		Opinion:        result.Opinion,
		ExternalRef:    rec.Run.ExternalRef,
		IdempotencyKey: rec.Run.IdempotencyKey,
	}
	// 记录表正文末尾补上人工确认小节：原稿一字不动，
	// 只在末尾加「谁在什么时候定了什么处置、说了什么意见」。
	record.DraftText = drafting.AppendConfirmationBlock(record.DraftText, facts)

	payload, err := drafting.BuildConfirmedWriteback(record, facts)
	if err != nil {
		return drafting.WritebackSource{}, nil, err
	}
	return payload, record, nil
}

// applyDecision 把决策落到 run 状态、迁移留痕、成稿产物与审计链上。
func (c *Confirmer) applyDecision(ctx context.Context, rec domain.RunRecord,
	steps []domain.Step, record *domain.ExceptionRecordPayload,
	result workflow.ConfirmResult) error {
	// run 状态推进 + 迁移留痕：留痕、状态与这次决策出自同一份请求。
	if _, err := rec.Run.AdvanceState(result.Request); err != nil {
		return err
	}
	rec.WritebackState = domain.WritebackStatePending
	rec.UpdatedAt = result.ConfirmedAt
	if err := c.store.UpsertRun(ctx, rec); err != nil {
		return err
	}

	// 成稿产物里的最终处置：查判定结论时看到的处置栏与台账里写的是同一个值。
	if record != nil {
		if err := c.updateDraftProduct(ctx, steps, record); err != nil {
			return err
		}
	}

	// 审计链：确认动作与模型调用落在同一条链上，内审看的是两者的接缝。
	return c.auditor.RecordConfirmation(ctx, audit.Confirmation{
		RunID:       result.RunID,
		Actor:       result.ConfirmedBy,
		Action:      string(result.Request.Action),
		FromState:   string(result.FromState),
		ToState:     string(result.ToState),
		Disposition: result.Disposition,
		Opinion:     result.Opinion,
		Reason:      result.Request.Reason,
	})
}

// updateDraftProduct 把确认后的最终处置写回成稿产物。
//
// 它更新的是**同一步的同一次尝试**（run_id + step_name + attempt 不变），
// 时间线上不会多出第二条成稿记录：这一步还是那一步，只是稿子上的处置栏
// 从「待补」变成了主管定下的结论。
func (c *Confirmer) updateDraftProduct(ctx context.Context, steps []domain.Step,
	record *domain.ExceptionRecordPayload) error {
	for i := range steps {
		if steps[i].PayloadKind != domain.PayloadExceptionRecord {
			continue
		}
		if steps[i].State != domain.StepStateSucceeded {
			continue
		}
		if err := steps[i].SetPayload(domain.PayloadExceptionRecord, record); err != nil {
			return err
		}
		return c.store.InsertRunStep(ctx, steps[i])
	}
	return nil
}
