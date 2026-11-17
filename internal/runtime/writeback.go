// 本文件落「出站写回」这一段的编排：把一条已由药学部主管确认的判定，
// 写进冷链台账、通知到接种门诊，并把两处的成败分别记账。
//
// 顺序是刻意的：**先写台账，再发通知**。反过来会出现「门诊已经知道要隔离，
// 台账里却查不到这次越温」——台账是飞检与内审对账的那一份，
// 少一条记录比多发一条通知严重得多。
//
// 「通知失败不撤销台账写入」也是刻意的：台账里那条已经是对的事实
// （这次越温确实发生过、处置确实这么定），把它撤掉等于让账目对不上；
// 通知没发出去要补发，补发用的是同一条 external_ref 与幂等键，
// 台账侧认得出是同一次越温，不会多出一条记录。
//
// 三条纪律贯穿本文件：
//
//  1. **未确认不得写回**：出站调用之前先问一次业务状态，
//     不到「已确认」一次请求都不发（workflow.AuthorizeWriteback）。
//  2. **写回的是确认时固化的载荷原文**，不在写回时重新算一遍。
//  3. **不重复入账**：本地按目标分别记账，已写成的那一处不再发第二次请求；
//     台账侧再按 external_ref / idempotency_key 兜一层。
package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/disposition"
	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/ledger"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// WritebackStore 是出站写回需要的本地读写口。
//
// 抽成接口是为了让运行时层不绑死存储实现：生产用 SQLite，测试用内存实现，
// 两者口径一致。
type WritebackStore interface {
	// RunByID 回读一条判定 run 的状态快照。
	RunByID(ctx context.Context, runID string) (domain.RunRecord, error)
	// UpsertRun 写入或更新一条判定 run 的状态快照。
	UpsertRun(ctx context.Context, rec domain.RunRecord) error
	// ConfirmationByRun 回读一条 run 的确认记录（含固化的写回载荷）。
	ConfirmationByRun(ctx context.Context, runID string) (domain.Confirmation, error)
	// UpsertWriteback 写入或更新一条写回记录。
	UpsertWriteback(ctx context.Context, rec domain.WritebackRecord) error
	// WritebackByTarget 回读一条 run 在某一处目标上的写回记录。
	WritebackByTarget(ctx context.Context, runID string, target string) (domain.WritebackRecord, error)
	// UpsertWritebackAlert 写入（或累加）一条写回告警。
	UpsertWritebackAlert(ctx context.Context, rec domain.WritebackAlert) error
	// ResolveWritebackAlert 把一条告警标记为已解决。
	ResolveWritebackAlert(ctx context.Context, runID string, target string, at time.Time) error
}

// NotificationSender 是通知系统的发送口。
type NotificationSender interface {
	// Send 发一条通知。
	Send(ctx context.Context, msg ledger.Message) error
}

// WritebackOutcome 是一次写回的结果。
//
// 它比「成功 / 失败」多几样东西，因为一次写回真的会落在两个地方：
// 台账写没写成、通知发没发成，是两件独立的事。
// 调用方（接口层）据此回执：主管确认完当场就能看到
// 「台账里写进去了没有、门诊通知到了没有、没到位的那部分怎么补」。
type WritebackOutcome struct {
	// RunID 是被写回的判定 run。
	RunID string `json:"run_id"`
	// ExternalRef 是这次越温的外部引用。
	ExternalRef string `json:"external_ref,omitempty"`
	// IdempotencyKey 是这次越温的幂等键。
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// LedgerWritten 表示台账这一处本次写成了。
	LedgerWritten bool `json:"ledger_written"`
	// LedgerSkipped 表示台账这一处此前已写成，本次没有重复发请求。
	LedgerSkipped bool `json:"ledger_skipped"`
	// LedgerError 是台账写入失败的错误码；成功时为空。
	LedgerError string `json:"ledger_error,omitempty"`
	// LedgerErrorDetail 是台账写入失败的细节。
	LedgerErrorDetail string `json:"ledger_error_detail,omitempty"`
	// NotificationSent 表示通知这一处本次发出去了。
	NotificationSent bool `json:"notification_sent"`
	// NotificationSkipped 表示通知这一处此前已发出，本次没有重复发请求。
	NotificationSkipped bool `json:"notification_skipped"`
	// NotificationError 是通知发送失败的错误码；成功时为空。
	NotificationError string `json:"notification_error,omitempty"`
	// NotificationErrorDetail 是通知发送失败的细节。
	NotificationErrorDetail string `json:"notification_error_detail,omitempty"`
	// WritebackState 是写回之后的写回状态（待写回 / 已写回）。
	WritebackState string `json:"writeback_state"`
	// LedgerAttempts 是台账这一处本次实际发起的出站调用次数。
	//
	// 1 = 一次就成（或不可重试的错，只发了那一次）；
	// 3 = 首次 + 2 次退避重试都失败了。
	// 它让「台账写不进去」这件事有了具体量级，而不是笼统一句失败。
	LedgerAttempts int `json:"ledger_attempts"`
	// NotificationAttempts 是通知这一处本次实际发起的出站调用次数。
	NotificationAttempts int `json:"notification_attempts"`
	// Compensation 标记本次是补偿重放（不是首次写回）。
	Compensation bool `json:"compensation"`
	// Note 是给调用方看的一句话：写回到了哪一步、还差什么。
	Note string `json:"note"`
}

// Complete 判断两处目标是否都已写成。
func (o WritebackOutcome) Complete() bool {
	return o.LedgerWritten && o.NotificationSent
}

// writebackStepName 是出站写回在服务日志里的步骤名。
//
// 写回不在判定链的五步之内——它发生在主管确认之后，是把结论搬进
// 冷链台账与接种门诊的那一段。带同一个步骤名，服务日志里按 run_id 串联时
// 「结论已经搬出去了」这一段也认得出来。
const writebackStepName = "writeback"

// Writebacker 是出站写回的编排者。
type Writebacker struct {
	store  WritebackStore
	writer workflow.OutboundWriter
	notify NotificationSender
	logger *logging.Logger
	now    func() time.Time
}

// NewWritebacker 装配出站写回编排器。
//
// writer 是台账写侧，通常由 workflow.WritebackGate 包一层——
// 那道闸门负责在发请求之前问一次业务状态（「未确认不得写回」）。
func NewWritebacker(store WritebackStore, writer workflow.OutboundWriter,
	notify NotificationSender, logger *logging.Logger) *Writebacker {
	if logger == nil {
		logger = logging.Default()
	}
	return &Writebacker{
		store:  store,
		writer: writer,
		notify: notify,
		logger: logger,
		now:    time.Now,
	}
}

// WithClock 注入时钟；测试可注入固定时钟，让写入时刻可复现。
func (w *Writebacker) WithClock(now func() time.Time) *Writebacker {
	if now != nil {
		w.now = now
	}
	return w
}

// Writeback 执行一次出站写回：先写冷链台账的越温偏差，再发接种门诊通知。
//
// 返回的 err 只在**硬失败**时非空——闸门拦下（未确认）、没有确认记录、
// 载荷不合法。这几种情况下一次出站请求都不该发出去。
//
// 出站调用本身失败不算硬失败：台账写不进去、通知发不出去，
// 错误码与细节都写进 outcome，同时把 run 挂到「待补偿」——
// 台账里那一条越温还没进去，这事不能就这么过去。
func (w *Writebacker) Writeback(ctx context.Context, runID string) (WritebackOutcome, error) {
	return w.writeback(ctx, runID, workflow.ActionWriteback, false)
}

// Replay 对一条待补偿的判定执行一次补偿重放。
//
// 它与首次写回走同一条路径、同一份固化载荷、同一把幂等键——
// 重放不是「再确认一次」，是「把上次没送到的再送一次」。
// 已经写成的那一处（台账或通知）连请求都不发，只补没写成的那一处。
func (w *Writebacker) Replay(ctx context.Context, runID string) (WritebackOutcome, error) {
	return w.writeback(ctx, runID, workflow.ActionCompensate, true)
}

// writeback 是首次写回与补偿重放共用的主路径。
//
// action 决定状态迁移的留痕来源：首次写回记 ActionWriteback，
// 补偿重放记 ActionCompensate——审计链上要能分清「这次是补的」。
func (w *Writebacker) writeback(ctx context.Context, runID string,
	action workflow.Action, compensation bool) (WritebackOutcome, error) {
	if w == nil || w.store == nil {
		return WritebackOutcome{}, errcode.Newf(errcode.StorageError, "出站写回未装配本地存储")
	}
	if w.writer == nil {
		return WritebackOutcome{}, errcode.Newf(errcode.StorageError, "出站写回未装配台账写侧")
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return WritebackOutcome{}, errcode.Newf(errcode.Validation, "出站写回缺少 run_id")
	}

	rec, err := w.store.RunByID(ctx, runID)
	if err != nil {
		return WritebackOutcome{}, err
	}

	// 本地按目标分别记账：两处各自写没写成。
	ledgerRec, ledgerErr := w.writebackRecord(ctx, runID, domain.WritebackTargetLedger)
	notifyRec, notifyErr := w.writebackRecord(ctx, runID, domain.WritebackTargetNotification)

	// 已经写回过的 run：再写一次是空操作（幂等重放），一次出站请求都不发。
	// 这条边单独处理，不走闸门——闸门管的是「未确认不得写回」，
	// 而写回过的 run 早过了确认那一关。
	if workflow.State(rec.Run.State) == workflow.StateWrittenBack {
		if ledgerRec.Written() && notifyRec.Written() {
			outcome := WritebackOutcome{
				RunID:               runID,
				ExternalRef:         ledgerRec.ExternalRef,
				IdempotencyKey:      ledgerRec.IdempotencyKey,
				LedgerSkipped:       true,
				NotificationSkipped: true,
				WritebackState:      domain.WritebackStateWrittenBack,
				Compensation:        compensation,
				Note:                "该判定此前已写回冷链台账并通知接种门诊，本次未重复发出",
			}
			w.logger.Info("writeback_replay_noop",
				"run_id", runID,
				"step_name", writebackStepName,
				"external_ref", ledgerRec.ExternalRef)
			return outcome, nil
		}
		return WritebackOutcome{}, errcode.Newf(errcode.IllegalTransition,
			"判定 run %s 已是「已写回台账」，但本地写回记录不完整，不能再写一次", runID)
	}

	// 闸门：不到「已确认」一次出站调用都不发。
	if err := workflow.AuthorizeWriteback(workflow.State(rec.Run.State)); err != nil {
		return WritebackOutcome{}, err
	}

	confirmation, err := w.store.ConfirmationByRun(ctx, runID)
	if err != nil {
		return WritebackOutcome{}, err
	}
	if confirmation.Action != domain.ConfirmationActionConfirm {
		return WritebackOutcome{}, errcode.Newf(errcode.Validation,
			"判定 run %s 的确认动作是 %q（被否决的判定不写台账）", runID, confirmation.Action)
	}
	deviation, err := ledger.DecodeDeviation(confirmation.PayloadJSON)
	if err != nil {
		return WritebackOutcome{}, err
	}

	outcome := WritebackOutcome{
		RunID:          runID,
		ExternalRef:    deviation.ExternalRef,
		IdempotencyKey: deviation.IdempotencyKey,
		Compensation:   compensation,
	}

	// 1. 台账：先写这一处。已写成的跳过——重放不再发第二次请求。
	ledgerDone, err := w.writeLedger(ctx, rec, confirmation, ledgerRec, ledgerErr, &outcome)
	if err != nil {
		// 台账没写进去：挂起待补偿，这次写回到此为止（通知也不发了）。
		w.markPending(ctx, rec, action,
			"冷链台账写入失败，本次越温尚未入账，挂起待补偿重放")
		outcome.WritebackState = domain.WritebackStatePending
		return outcome, nil
	}
	if !ledgerDone {
		outcome.WritebackState = domain.WritebackStatePending
		return outcome, nil
	}

	// 2. 通知：台账写成了再发。通知失败不撤销台账写入，只标记待补偿。
	if !w.sendNotification(ctx, runID, deviation, notifyRec, notifyErr, &outcome) {
		w.markPending(ctx, rec, action,
			"冷链台账已写入，接种门诊通知未发出，挂起待补偿重放")
		outcome.WritebackState = domain.WritebackStatePending
		return outcome, nil
	}

	// 3. 两处都写成：状态推到「已写回台账」，写回状态置已写回。
	rec.WritebackState = domain.WritebackStateWrittenBack
	rec.UpdatedAt = w.now()
	if err := w.advanceState(ctx, &rec, action, workflow.StateWrittenBack,
		workflow.ReasonLedgerWrittenBack,
		"冷链台账越温记录与接种门诊通知均已发出"); err != nil {
		w.logger.Warn("writeback_state_advance_failed",
			"run_id", runID, "error", err.Error())
	}
	if err := w.store.UpsertRun(ctx, rec); err != nil {
		return outcome, err
	}
	outcome.WritebackState = domain.WritebackStateWrittenBack
	outcome.Note = "冷链台账越温记录已写入，接种门诊通知已发出"
	return outcome, nil
}

// writebackRecord 回读某处目标的本地写回记录。
//
// 还没有记录时返回一条空记录（不是错误）：「没写过」是正常情形，
// 只有读库本身出错才算异常。
func (w *Writebacker) writebackRecord(ctx context.Context, runID, target string) (domain.WritebackRecord, error) {
	rec, err := w.store.WritebackByTarget(ctx, runID, target)
	if err == nil {
		return rec, nil
	}
	if errcode.As(err).Code == errcode.NotFound {
		return domain.WritebackRecord{RunID: runID, Target: target}, nil
	}
	return domain.WritebackRecord{}, err
}

// writeLedger 写冷链台账这一处目标。
//
// 返回值写没写成；出错时错误码与细节已经写进 outcome。
func (w *Writebacker) writeLedger(ctx context.Context, rec domain.RunRecord,
	confirmation domain.Confirmation, existing domain.WritebackRecord,
	readErr error, outcome *WritebackOutcome) (bool, error) {
	runID := rec.Run.RunID

	switch {
	case readErr != nil:
		e := errcode.As(readErr)
		outcome.LedgerError = string(e.Code)
		outcome.LedgerErrorDetail = e.Details
		w.logger.Warn("ledger_writeback_failed",
			"run_id", runID,
			"step_name", writebackStepName,
			"error_code", string(e.Code),
			"detail", e.Details,
			"external_ref", outcome.ExternalRef)
		return false, readErr
	case existing.Written():
		// 已经写过台账：不重复发请求。
		outcome.LedgerSkipped = true
		w.logger.Info("ledger_writeback_skipped",
			"run_id", runID,
			"step_name", writebackStepName,
			"external_ref", existing.ExternalRef,
			"written_at", formatStamp(existing.WrittenAt))
		return true, nil
	}

	writeErr := w.writer.WriteDeviation(ctx, runID, confirmation.PayloadJSON)
	// 本次实际发起了几次出站调用：1 = 一次就成或不可重试；3 = 退避重试用尽。
	outcome.LedgerAttempts = ledger.AttemptsOf(writeErr)
	if writeErr != nil {
		e := errcode.As(writeErr)
		outcome.LedgerError = string(e.Code)
		outcome.LedgerErrorDetail = e.Details
		w.logger.Warn("ledger_writeback_failed",
			"run_id", runID,
			"step_name", writebackStepName,
			"error_code", string(e.Code),
			"detail", e.Details,
			"attempts", outcome.LedgerAttempts,
			"external_ref", outcome.ExternalRef)
		w.recordWriteback(ctx, runID, domain.WritebackTargetLedger, domain.WritebackTargetStateFailed,
			outcome.ExternalRef, outcome.IdempotencyKey, string(e.Code), e.Details)
		w.raiseAlert(ctx, runID, domain.WritebackTargetLedger,
			string(e.Code), e.Details, outcome.LedgerAttempts)
		return false, writeErr
	}

	w.recordWriteback(ctx, runID, domain.WritebackTargetLedger, domain.WritebackTargetStateWritten,
		outcome.ExternalRef, outcome.IdempotencyKey, "", "")
	w.resolveAlert(ctx, runID, domain.WritebackTargetLedger)
	outcome.LedgerWritten = true
	w.logger.Info("ledger_writeback_succeeded",
		"run_id", runID,
		"step_name", writebackStepName,
		"external_ref", outcome.ExternalRef,
		"idempotency_key", outcome.IdempotencyKey,
		"attempts", outcome.LedgerAttempts)
	return true, nil
}

// sendNotification 发接种门诊通知这一处目标。
//
// 返回值是发没发成；失败时错误码与细节已经写进 outcome。
func (w *Writebacker) sendNotification(ctx context.Context, runID string,
	deviation ledger.DeviationRecord, existing domain.WritebackRecord,
	readErr error, outcome *WritebackOutcome) bool {
	if w.notify == nil {
		outcome.NotificationError = string(errcode.StorageError)
		outcome.NotificationErrorDetail = "出站写回未装配通知系统"
		w.logger.Warn("notification_writeback_skipped",
			"run_id", runID, "step_name", writebackStepName, "reason", "未装配通知系统")
		return false
	}

	switch {
	case readErr != nil:
		outcome.NotificationError = string(errcode.StorageError)
		outcome.NotificationErrorDetail = errcode.As(readErr).Details
		return false
	case existing.Written():
		outcome.NotificationSkipped = true
		return true
	}

	msg := buildNotification(deviation, runID)
	sendErr := w.notify.Send(ctx, msg)
	outcome.NotificationAttempts = ledger.AttemptsOf(sendErr)
	if sendErr != nil {
		e := errcode.As(sendErr)
		outcome.NotificationError = string(e.Code)
		outcome.NotificationErrorDetail = e.Details
		w.logger.Warn("notification_writeback_failed",
			"run_id", runID,
			"step_name", writebackStepName,
			"error_code", string(e.Code),
			"detail", e.Details,
			"attempts", outcome.NotificationAttempts,
			"idempotency_key", msg.IdempotencyKey)
		w.recordWriteback(ctx, runID, domain.WritebackTargetNotification,
			domain.WritebackTargetStateFailed, deviation.ExternalRef, msg.IdempotencyKey,
			string(e.Code), e.Details)
		w.raiseAlert(ctx, runID, domain.WritebackTargetNotification,
			string(e.Code), e.Details, outcome.NotificationAttempts)
		return false
	}

	w.recordWriteback(ctx, runID, domain.WritebackTargetNotification,
		domain.WritebackTargetStateWritten, deviation.ExternalRef, msg.IdempotencyKey, "", "")
	w.resolveAlert(ctx, runID, domain.WritebackTargetNotification)
	outcome.NotificationSent = true
	w.logger.Info("notification_writeback_succeeded",
		"run_id", runID,
		"step_name", writebackStepName,
		"external_ref", deviation.ExternalRef,
		"idempotency_key", msg.IdempotencyKey,
		"attempts", outcome.NotificationAttempts)
	return true
}

// markPending 把 run 挂到「待补偿」：写回状态置待写回，业务状态推进到待补偿。
//
// 挂起而不是丢弃：台账里那一条越温还没进去，这事不能就这么过去。
// 补偿重放（task 40）扫的就是这一档。
func (w *Writebacker) markPending(ctx context.Context, rec domain.RunRecord,
	action workflow.Action, note string) {
	rec.WritebackState = domain.WritebackStatePending
	rec.UpdatedAt = w.now()
	if err := w.advanceState(ctx, &rec, action, workflow.StatePendingWriteback,
		workflow.ReasonWritebackPending, note); err != nil {
		w.logger.Warn("writeback_pending_state_advance_failed",
			"run_id", rec.Run.RunID, "error", err.Error())
	}
	if err := w.store.UpsertRun(ctx, rec); err != nil {
		w.logger.Warn("writeback_pending_persist_failed",
			"run_id", rec.Run.RunID, "error", err.Error())
	}
}

// advanceState 推进业务状态；状态已在目标态时按幂等空操作处理。
func (w *Writebacker) advanceState(ctx context.Context, rec *domain.RunRecord,
	action workflow.Action, to workflow.State, reason, note string) error {
	if workflow.State(rec.Run.State) == to {
		return nil
	}
	req := workflow.Request{
		To:     to,
		Action: action,
		Reason: reason,
		Note:   note,
		Actor:  "system",
		At:     w.now(),
	}
	_, err := rec.Run.AdvanceState(req)
	return err
}

// raiseAlert 留一条「出站写回没落成」的告警。
//
// 告警是**可见**的：它进库、进端点、进运行摘要的待写回计数。
// 「台账写不进去」如果只留一句日志，没人会看见——
// 药事那边会以为这次越温已经入账，账面与实物就对不上了。
func (w *Writebacker) raiseAlert(ctx context.Context, runID, target,
	errorCode, errorDetail string, attempts int) {
	now := w.now()
	at := now
	rec := domain.WritebackAlert{
		RunID:         runID,
		Target:        target,
		State:         domain.WritebackAlertStateOpen,
		ErrorCode:     errorCode,
		ErrorDetail:   errorDetail,
		Attempts:      attempts,
		Failures:      1,
		FirstFailedAt: &at,
		LastFailedAt:  &at,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := w.store.UpsertWritebackAlert(ctx, rec); err != nil {
		w.logger.Warn("writeback_alert_persist_failed",
			"run_id", runID, "target", target, "error", err.Error())
		return
	}
	w.logger.Error("writeback_alert_raised",
		"run_id", runID,
		"target", target,
		"error_code", errorCode,
		"attempts", attempts,
		"detail", errorDetail,
	)
}

// resolveAlert 把一条告警标记为已解决。
//
// 补上了就把告警解掉——但**不删**：open→resolved 的留痕留在库里，
// 飞检问「那次台账写不进去、后来怎么补的」时答得出来。
func (w *Writebacker) resolveAlert(ctx context.Context, runID, target string) {
	if err := w.store.ResolveWritebackAlert(ctx, runID, target, w.now()); err != nil {
		w.logger.Warn("writeback_alert_resolve_failed",
			"run_id", runID, "target", target, "error", err.Error())
		return
	}
	w.logger.Info("writeback_alert_resolved", "run_id", runID, "target", target)
}

// recordWriteback 落一条写回记录。
//
// 落库失败只记日志：写回记录是「不重复入账」的本地凭据，
// 但它不能反过来把一次已经成功的台账写入变成失败——
// 台账里那条记录已经在了，账目比本地痕迹重要。
func (w *Writebacker) recordWriteback(ctx context.Context, runID, target, state,
	externalRef, idempotencyKey, errorCode, errorDetail string) {
	now := w.now()
	rec := domain.WritebackRecord{
		RunID:          runID,
		Target:         target,
		State:          state,
		ExternalRef:    externalRef,
		IdempotencyKey: idempotencyKey,
		ErrorCode:      errorCode,
		ErrorDetail:    errorDetail,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if state == domain.WritebackTargetStateWritten {
		at := now
		rec.WrittenAt = &at
	}
	if err := w.store.UpsertWriteback(ctx, rec); err != nil {
		w.logger.Warn("writeback_record_persist_failed",
			"run_id", runID, "target", target, "state", state, "error", err.Error())
	}
}

// buildNotification 组装发给接种门诊的通知。
//
// 正文直接带确认时定稿的异常情况记录表原文——门诊与药事拿到的
// 就是主管签过字的那一份，不在这里重新组织一遍事实。
func buildNotification(deviation ledger.DeviationRecord, runID string) ledger.Message {
	var b strings.Builder
	b.WriteString("点位 ")
	b.WriteString(deviation.PointID)
	if deviation.DeviceName != "" {
		b.WriteString("（")
		b.WriteString(deviation.DeviceName)
		b.WriteString("）")
	}
	b.WriteString(" 于 ")
	b.WriteString(formatStamp(&deviation.ExcursionStartedAt))
	b.WriteString(" ~ ")
	b.WriteString(formatStamp(&deviation.ExcursionEndedAt))
	b.WriteString(" 越温，峰值 ")
	b.WriteString(formatFloat(deviation.PeakTemperatureC))
	b.WriteString(" ℃，持续 ")
	b.WriteString(strconv.Itoa(deviation.DurationMinutes))
	b.WriteString(" 分钟")
	if deviation.Zone != "" {
		b.WriteString("（温区 ")
		b.WriteString(deviation.Zone)
		b.WriteString("）")
	}
	b.WriteString("。\n")

	dispositionText := deviation.Disposition
	if label, ok := disposition.Label(dispositionText); ok {
		dispositionText = label + "（" + deviation.Disposition + "）"
	}
	if dispositionText != "" {
		b.WriteString("最终处置结论：")
		b.WriteString(dispositionText)
		b.WriteString("。")
	}
	if deviation.ConfirmedBy != "" {
		b.WriteString("确认人：")
		b.WriteString(deviation.ConfirmedBy)
		b.WriteString("，确认时间：")
		b.WriteString(formatStamp(&deviation.ConfirmedAt))
		b.WriteString("。")
	}
	if deviation.ExceptionRecord != "" {
		b.WriteString("\n\n—— 以下为《疫苗储存和运输温度异常情况记录表》——\n")
		b.WriteString(deviation.ExceptionRecord)
	}

	return ledger.Message{
		Title:          "冷链温控越温通知",
		Body:           b.String(),
		RelatedRunID:   runID,
		IdempotencyKey: notificationKey(deviation.IdempotencyKey),
	}
}

// notificationKey 派生通知侧的幂等键。
//
// 它与台账侧的幂等键不同源：两个系统各自判重，键撞在一起会让其中一头
// 把另一头的消息当成重复而丢掉。按「通知 + 台账键」派生，两边各判各的。
func notificationKey(ledgerKey string) string {
	sum := sha256.Sum256([]byte("notification|" + ledgerKey))
	return hex.EncodeToString(sum[:])
}

// formatStamp 渲染时刻：本地时区的「年-月-日 时:分」加时区偏移。
//
// 偏移必须带：现场写的「03:15」到底是哪一时区，写不清楚就只能靠猜。
func formatStamp(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04") + "（" + t.Format("-07:00") + "）"
}
