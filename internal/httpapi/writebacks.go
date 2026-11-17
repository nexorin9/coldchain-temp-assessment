// 本文件落「出站写回没落成之后怎么办」这一段的对外接口：
// 待补偿清单与补偿重放。
//
// 为什么这两个端点要独立于确认端点：写回失败是**另一件要办的事**。
// 主管确认完当场看到「台账没写进去」，那是确认回执；
// 稍后台账恢复了要补发，那是补偿重放。两者混在一起，
// 「这次确认到底成没成」就说不清了——
// 确认早成了，欠的是台账那一条。
//
// 两条端点的口径：
//   - GET /v1/writebacks/pending：只读清单，回答「哪些越温还没进台账」；
//   - POST /v1/runs/{id}/writeback/retry：补发，回答「把这条补上」。
//
// 补发**不是重新确认**：走同一条主路径、同一份固化载荷、同一把幂等键。
// 台账侧按 external_ref / idempotency_key 认出是同一次越温，
// 不会多出一条记录。
package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/runtime"
)

// WritebackCompensator 是补偿重放的能力边界。
//
// 它与 RunWritebacker 分开声明：确认端点要的是「把这条判定写出去」，
// 补偿端点要的是「列出还没写出去的」与「把其中一条再送一遍」，
// 两种取数、两种动作，混在一个接口里迟早用错。
type WritebackCompensator interface {
	// Summary 列出待补偿的写回记录与还挂着的告警。
	Summary(ctx context.Context) (runtime.CompensationSummary, error)
	// Retry 对一条待补偿的判定触发补偿重放。
	Retry(ctx context.Context, runID string) (runtime.WritebackOutcome, error)
}

// pendingWritebacksResponse 是待补偿清单的响应体。
type pendingWritebacksResponse struct {
	// PendingCount 是还没写成的写回记录条数。
	PendingCount int `json:"pending_count"`
	// OpenAlertCount 是还挂着的告警条数。
	OpenAlertCount int `json:"open_alert_count"`
	// Items 是待补偿的写回记录清单。
	Items []runtime.PendingWriteback `json:"items"`
	// Alerts 是还挂着的告警清单。
	Alerts []domain.WritebackAlert `json:"alerts"`
}

// retryWritebackResponse 是补偿重放的回执。
type retryWritebackResponse struct {
	// RunID 是被重放的判定 run。
	RunID string `json:"run_id"`
	// ExternalRef 是这次越温的外部引用；台账按它识别同一次越温。
	ExternalRef string `json:"external_ref,omitempty"`
	// IdempotencyKey 是这次越温的幂等键；重放用的就是它。
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// LedgerWritten 表示台账这一处本次补上了。
	LedgerWritten bool `json:"ledger_written"`
	// LedgerSkipped 表示台账这一处此前已写成，本次没有重复发请求。
	LedgerSkipped bool `json:"ledger_skipped"`
	// LedgerError 是台账写入失败的错误码；成功时为空。
	LedgerError string `json:"ledger_error,omitempty"`
	// LedgerErrorDetail 是台账写入失败的细节。
	LedgerErrorDetail string `json:"ledger_error_detail,omitempty"`
	// LedgerAttempts 是台账这一处本次实际发起的出站调用次数。
	LedgerAttempts int `json:"ledger_attempts"`
	// NotificationSent 表示通知这一处本次发出去了。
	NotificationSent bool `json:"notification_sent"`
	// NotificationSkipped 表示通知这一处此前已发出，本次没有重复发请求。
	NotificationSkipped bool `json:"notification_skipped"`
	// NotificationError 是通知发送失败的错误码；成功时为空。
	NotificationError string `json:"notification_error,omitempty"`
	// NotificationErrorDetail 是通知发送失败的细节。
	NotificationErrorDetail string `json:"notification_error_detail,omitempty"`
	// NotificationAttempts 是通知这一处本次实际发起的出站调用次数。
	NotificationAttempts int `json:"notification_attempts"`
	// WritebackState 是重放之后的写回状态（待写回 / 已写回）。
	WritebackState string `json:"writeback_state"`
	// Compensation 标记本次是补偿重放。
	Compensation bool `json:"compensation"`
	// Note 是给调用方看的一句话：补上了没有、还差什么。
	Note string `json:"note"`
}

// handleGetPendingWritebacks 列出待补偿的写回记录与还挂着的告警。
//
// 它是只读端点：光看清单不改变任何业务状态。
// 未装配补偿能力时明确报错：宁可不答，也不回一个空的「一切正常」。
func (s *Server) handleGetPendingWritebacks(w http.ResponseWriter, r *http.Request) {
	if s.compensator == nil {
		writeError(w, errcode.Newf(errcode.StorageError,
			"本服务未装配补偿重放能力，无法列出待写回记录"))
		return
	}
	summary, err := s.compensator.Summary(r.Context())
	if err != nil {
		writeError(w, errcode.As(err))
		return
	}
	items := summary.Items
	if items == nil {
		items = []runtime.PendingWriteback{}
	}
	alerts := summary.Alerts
	if alerts == nil {
		alerts = []domain.WritebackAlert{}
	}
	writeJSON(w, http.StatusOK, pendingWritebacksResponse{
		PendingCount:   summary.PendingCount,
		OpenAlertCount: summary.OpenAlertCount,
		Items:          items,
		Alerts:         alerts,
	})
}

// handleRetryWriteback 对一条待补偿的判定触发补偿重放。
//
// 它补的是「上次没送到的那一份」，不重新判定、不重新确认：
// 载荷、幂等键、external_ref 全是主管确认那一刻定下的那份。
// 已经写进台账的那一处连请求都不发。
func (s *Server) handleRetryWriteback(w http.ResponseWriter, r *http.Request) {
	if s.compensator == nil {
		writeError(w, errcode.Newf(errcode.StorageError,
			"本服务未装配补偿重放能力，无法触发写回重放"))
		return
	}
	runID := strings.TrimSpace(r.PathValue("id"))
	if runID == "" {
		writeError(w, errcode.Newf(errcode.Validation, "补偿重放缺少 run_id"))
		return
	}
	outcome, err := s.compensator.Retry(r.Context(), runID)
	if err != nil {
		writeError(w, errcode.As(err))
		return
	}
	writeJSON(w, http.StatusOK, retryWritebackResponse{
		RunID:                   outcome.RunID,
		ExternalRef:             outcome.ExternalRef,
		IdempotencyKey:          outcome.IdempotencyKey,
		LedgerWritten:           outcome.LedgerWritten,
		LedgerSkipped:           outcome.LedgerSkipped,
		LedgerError:             outcome.LedgerError,
		LedgerErrorDetail:       outcome.LedgerErrorDetail,
		LedgerAttempts:          outcome.LedgerAttempts,
		NotificationSent:        outcome.NotificationSent,
		NotificationSkipped:     outcome.NotificationSkipped,
		NotificationError:       outcome.NotificationError,
		NotificationErrorDetail: outcome.NotificationErrorDetail,
		NotificationAttempts:    outcome.NotificationAttempts,
		WritebackState:          outcome.WritebackState,
		Compensation:            outcome.Compensation,
		Note:                    outcome.Note,
	})
}
