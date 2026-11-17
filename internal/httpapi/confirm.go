// 本文件落「药学部主管确认 / 否决处置结论」这两个端点：
// POST /v1/runs/{id}/confirm 与 POST /v1/runs/{id}/reject。
//
// 这是整条链上**唯一由人推进业务状态**的入口。模型产出、规则基线、
// 降级路径都只能把判定推到「待确认」为止；再往后（已确认 / 已否决）
// 只能由持主管凭据的动作推进——这条边界写在角色闸门里，
// 也写在状态机的触发来源表里，两处各管一头。
//
// 三条约定：
//   - 权限：两个端点都要求「确认或否决处置结论」这一项权限，
//     只有药学部主管持有。缺凭据 401，身份对但权限不对 403。
//   - 顺序：先校验、再决策、后落库。任何一步没过，库里一个字节都不改——
//     状态被误推进比状态推不动危险得多。
//   - 确认与写回分两段：这两个端点只推进状态、固化载荷、记账；
//     台账写入由出站写回流程执行（runtime.Writebacker），且必须先过
//     「未确认不得写回」那道闸门。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/runtime"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// RunConfirmer 是判定确认与否决的能力边界。
//
// 只声明本包真正用到的那两个方法。编排细节（成稿在不在、要素齐不齐、
// 载荷怎么固化）留在 runtime 层，这一层只负责「把请求变成动作、
// 把结果变成回执」。
type RunConfirmer interface {
	// Confirm 确认一条待确认的判定。
	Confirm(ctx context.Context, runID string, req workflow.ConfirmRequest) (runtime.ConfirmationOutcome, error)
	// Reject 否决一条判定。
	Reject(ctx context.Context, runID string, req workflow.ConfirmRequest) (runtime.ConfirmationOutcome, error)
}

// confirmRequest 是确认 / 否决的请求体。
//
// 字段少是刻意的：确认动作只带「人的事实」——谁做的、定了什么处置、
// 说了什么意见。峰值、持续分钟、涉及批次这些都在成稿里，
// 确认时不许再改一遍：改了就等于台账里那条与刚才看到的稿子对不上。
type confirmRequest struct {
	// Actor 是执行动作的药师标识（确认人 / 否决人）。
	//
	// 它必填：处置结论要能落到具体的人身上，「药学部」三个字不算确认人。
	Actor string `json:"actor"`
	// Disposition 是处置结论取值（confirm 时必填）。
	//
	// 取值域：isolate / release / scrap / pending_manufacturer_evaluation。
	Disposition string `json:"disposition"`
	// Opinion 是意见（可选）；写进留痕与审计链。
	Opinion string `json:"opinion"`
}

// confirmResponse 是确认 / 否决的回执。
//
// 它回答的是「现在这条判定处于什么状态、由谁在什么时候定成了什么、
// 台账里将会出现的是哪一条」。最后一条最要紧：主管确认完就该知道
// 接下来冷链台账上会多出什么样的一行。
type confirmResponse struct {
	// RunID 是被确认 / 否决的判定 run。
	RunID string `json:"run_id"`
	// PreviousState 是动作前的业务状态。
	PreviousState string `json:"previous_state"`
	// State 是动作后的业务状态。
	State string `json:"state"`
	// ConfirmedBy 是执行动作的人。
	ConfirmedBy string `json:"confirmed_by"`
	// ConfirmedAt 是动作时刻。
	ConfirmedAt string `json:"confirmed_at"`
	// Disposition 是最终处置结论；否决时为空。
	Disposition string `json:"disposition,omitempty"`
	// DispositionLabel 是处置结论的中文短标签。
	DispositionLabel string `json:"disposition_label,omitempty"`
	// Opinion 是主管意见。
	Opinion string `json:"opinion,omitempty"`
	// ExternalRef 是这条判定的外部引用；台账按它识别同一次越温。
	ExternalRef string `json:"external_ref,omitempty"`
	// IdempotencyKey 是这条判定的幂等键；台账侧按它防重复入账。
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// WritebackState 是确认之后的写回状态。
	WritebackState string `json:"writeback_state,omitempty"`
	// PayloadFrozen 表示台账写回载荷是否已在确认这一刻固化。
	PayloadFrozen bool `json:"payload_frozen"`
	// Writeback 是出站写回的结果：台账写没写进去、门诊通知到没到。
	//
	// 它回答的是主管最关心的那句「这条现在到底生效了没有」：
	// 确认成功只说明结论定了，台账与通知的落点要另算。
	Writeback *writebackSummary `json:"writeback,omitempty"`
	// Note 是给调用方看的一句话：确认之后会发生什么、没发生什么。
	Note string `json:"note"`
}

// writebackSummary 是出站写回的结果摘要。
//
// 台账与通知分两栏：两处的成败互不相干，混成一句「写回成功 / 失败」
// 会让人分不清该先修哪一头。
type writebackSummary struct {
	// ExternalRef 是这次越温在台账侧的外部引用。
	ExternalRef string `json:"external_ref,omitempty"`
	// IdempotencyKey 是这次越温的幂等键。
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// LedgerWritten 表示台账这一处本次写成了。
	LedgerWritten bool `json:"ledger_written"`
	// LedgerSkipped 表示台账这一处此前已写成，本次没有重复发请求。
	LedgerSkipped bool `json:"ledger_skipped"`
	// LedgerError 是台账写入失败的错误码。
	LedgerError string `json:"ledger_error,omitempty"`
	// NotificationSent 表示接种门诊通知本次发出去了。
	NotificationSent bool `json:"notification_sent"`
	// NotificationSkipped 表示通知此前已发出，本次没有重复发请求。
	NotificationSkipped bool `json:"notification_skipped"`
	// NotificationError 是通知发送失败的错误码。
	NotificationError string `json:"notification_error,omitempty"`
	// WritebackState 是写回之后的写回状态。
	WritebackState string `json:"writeback_state"`
	// Note 是给调用方看的一句话。
	Note string `json:"note"`
}

// handleConfirmRun 确认一条待确认的判定。
//
// 它做两件事：把结论从「待确认」推到「已确认」并把台账要写的内容固化下来；
// 确认成立之后触发那一段出站写回（写冷链台账、通知接种门诊），
// 并把两处的落点如实写进回执。
func (s *Server) handleConfirmRun(w http.ResponseWriter, r *http.Request) {
	s.handleDecision(w, r, true)
}

// handleRejectRun 否决一条判定。
//
// 被否决的判定不写冷链台账：台账里多一条没发生过的越温，
// 比少记一条更麻烦——前者会让越温统计对不上真实温控记录。
func (s *Server) handleRejectRun(w http.ResponseWriter, r *http.Request) {
	s.handleDecision(w, r, false)
}

// handleDecision 是确认与否决共用的处理流程。
//
// confirm 为真走确认，为假走否决。两条路径的分歧只有三处：
// 处置取值要不要、载荷要不要、结论写什么。
func (s *Server) handleDecision(w http.ResponseWriter, r *http.Request, confirm bool) {
	if s.confirmer == nil {
		writeError(w, errcode.Newf(errcode.StorageError,
			"判定确认能力未初始化，服务不接受确认动作"))
		return
	}

	runID := strings.TrimSpace(r.PathValue("id"))
	if runID == "" {
		writeError(w, errcode.Newf(errcode.Validation, "缺少 run_id"))
		return
	}

	body, err := s.decodeConfirmRequest(w, r)
	if err != nil {
		writeError(w, errcode.As(err))
		return
	}

	req := workflow.ConfirmRequest{
		RunID:       runID,
		Actor:       strings.TrimSpace(body.Actor),
		Role:        RoleFrom(r.Context()),
		Disposition: strings.TrimSpace(body.Disposition),
		Opinion:     body.Opinion,
	}

	var outcome runtime.ConfirmationOutcome
	if confirm {
		outcome, err = s.confirmer.Confirm(r.Context(), runID, req)
	} else {
		outcome, err = s.confirmer.Reject(r.Context(), runID, req)
	}
	if err != nil {
		e := errcode.As(err)
		if s.logger != nil {
			s.logger.Warn("confirmation_rejected",
				"error_code", string(e.Code),
				"detail", e.Details,
				"run_id", runID)
		}
		writeError(w, e)
		return
	}

	decision := outcome.Decision
	note := "结论已确认，台账写入由出站写回流程执行（未确认前不出站）"
	if !confirm {
		note = "结论已否决，本条判定不写入冷链台账"
	}

	// 确认成功之后触发出站写回：先写冷链台账的越温偏差，再发接种门诊通知。
	// 写回失败不推翻这次确认——结论已经定了，缺的是台账里那一条，
	// 它会挂到「待补偿」等人补，不会悄悄消失。
	var writeback *writebackSummary
	if confirm {
		writeback = s.runWriteback(r.Context(), runID)
		if writeback != nil {
			note = writeback.Note
		}
	}
	writeJSON(w, http.StatusOK, confirmResponse{
		RunID:            runID,
		PreviousState:    string(decision.FromState),
		State:            string(decision.ToState),
		ConfirmedBy:      decision.ConfirmedBy,
		ConfirmedAt:      formatTime(decision.ConfirmedAt),
		Disposition:      decision.Disposition,
		DispositionLabel: decision.DispositionLabel,
		Opinion:          decision.Opinion,
		ExternalRef:      outcome.ExternalRef,
		IdempotencyKey:   outcome.IdempotencyKey,
		WritebackState:   outcome.WritebackState,
		PayloadFrozen:    outcome.PayloadFrozen,
		Writeback:        writeback,
		Note:             note,
	})
}

// runWriteback 触发一次出站写回并把结果压成回执摘要。
//
// 写回能力没装配时返回 nil：确认照样成立，只是这一轮没有出站调用。
// 出站调用失败也不在这里报错——确认已经生效，报错会让调用方以为
// 「整条判定没成」，而真实情况是「台账里那一条还没写进去」。
func (s *Server) runWriteback(ctx context.Context, runID string) *writebackSummary {
	if s.writebacker == nil {
		return nil
	}
	outcome, err := s.writebacker.Writeback(ctx, runID)
	if err != nil {
		if s.logger != nil {
			e := errcode.As(err)
			s.logger.Warn("writeback_trigger_failed",
				"error_code", string(e.Code),
				"detail", e.Details,
				"run_id", runID)
		}
		return &writebackSummary{
			ExternalRef:    outcome.ExternalRef,
			IdempotencyKey: outcome.IdempotencyKey,
			WritebackState: domain.WritebackStatePending,
			Note:           "出站写回未执行：" + errcode.As(err).Message,
		}
	}
	summary := &writebackSummary{
		ExternalRef:         outcome.ExternalRef,
		IdempotencyKey:      outcome.IdempotencyKey,
		LedgerWritten:       outcome.LedgerWritten,
		LedgerSkipped:       outcome.LedgerSkipped,
		LedgerError:         outcome.LedgerError,
		NotificationSent:    outcome.NotificationSent,
		NotificationSkipped: outcome.NotificationSkipped,
		NotificationError:   outcome.NotificationError,
		WritebackState:      outcome.WritebackState,
		Note:                outcome.Note,
	}
	if summary.WritebackState == "" {
		summary.WritebackState = domain.WritebackStatePending
	}
	return summary
}

// decodeConfirmRequest 读入并解析确认 / 否决的请求体。
//
// 三类失败各有各的错误码：请求体过大 → E_VALIDATION，
// 请求体不是合法 JSON → E_VALIDATION，尾部多出内容 → E_VALIDATION。
// 与入站事件端点同一套口径：调用方要知道自己错在哪，才能改对。
func (s *Server) decodeConfirmRequest(w http.ResponseWriter, r *http.Request) (confirmRequest, error) {
	limited := http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	defer func() { _ = limited.Close() }()

	decoder := json.NewDecoder(limited)
	var body confirmRequest
	if err := decoder.Decode(&body); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			return confirmRequest{}, errcode.Newf(errcode.Validation,
				"请求体超过上限 %d 字节", s.maxBodyBytes)
		case errors.Is(err, io.EOF):
			return confirmRequest{}, errcode.Newf(errcode.Validation, "请求体为空")
		default:
			return confirmRequest{}, errcode.Newf(errcode.Validation,
				"请求体不是合法的确认动作 JSON：%v", err)
		}
	}

	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return confirmRequest{}, errcode.Newf(errcode.Validation,
			"请求体只能包含一个确认动作 JSON 对象")
	}
	return body, nil
}
