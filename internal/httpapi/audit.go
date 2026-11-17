// 本文件落审计链的查询端点：GET /v1/runs/{id}/audit。
//
// 它回答的是内审与飞检最爱问的那句话：「这条判定当时用了模型吗？
// 用了哪个模型、花了多少 token、算下来多少钱、最后是谁确认的」。
//
// 端点回两段：
//   - summary：这一条判定的调用与确认汇总（调用次数、token 合计、成本可算性）；
//   - entries：审计链明细，按 run 内序号递增——模型调用与人工确认同一条链，
//     合起来才是「机器怎么判的、人怎么认的」这段完整过程。
//
// 三条约定：
//   - 找不到就说找不到：run 不存在返回 404 与 E_NOT_FOUND，不回空壳；
//   - 只读：这个端点只回账，不改账（确认动作走独立的确认端点）；
//   - 不回敏感内容：审计链里本来就没有凭据与提示词原文，
//     端点也只把链上有的东西原样端出来。
package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// AuditReader 是审计链的读取能力边界。
//
// 只声明本包真正用到的那一个方法。落库实现（SQLite）与部署形态无关，
// 这一层不关心审计表是从哪儿来的。
type AuditReader interface {
	// AuditEntriesByRun 回读一条 run 的全部审计记录，按序号升序。
	AuditEntriesByRun(ctx context.Context, runID string) ([]domain.AuditEntry, error)
}

// auditEntryView 是审计链上一条记录的对外形状。
//
// 它是链上一条记录的原样投影：模型调用那类填 token 与成本，
// 人工确认那类填执行人与状态迁移，另一类的字段留空——
// 空值必须真的空着，不用零值冒充。
type auditEntryView struct {
	Seq              int      `json:"seq"`
	EntryType        string   `json:"entry_type"`
	StepName         string   `json:"step_name,omitempty"`
	SchemaName       string   `json:"schema_name,omitempty"`
	Model            string   `json:"model,omitempty"`
	Outcome          string   `json:"outcome,omitempty"`
	Degraded         bool     `json:"degraded"`
	ErrorCode        string   `json:"error_code,omitempty"`
	LatencyMS        int64    `json:"latency_ms"`
	PromptTokens     int      `json:"prompt_tokens"`
	CompletionTokens int      `json:"completion_tokens"`
	TotalTokens      int      `json:"total_tokens"`
	CostAmount       *float64 `json:"cost_amount,omitempty"`
	CostCurrency     string   `json:"cost_currency,omitempty"`
	CostStatus       string   `json:"cost_status"`
	Actor            string   `json:"actor,omitempty"`
	Action           string   `json:"action,omitempty"`
	Disposition      string   `json:"disposition,omitempty"`
	Opinion          string   `json:"opinion,omitempty"`
	FromState        string   `json:"from_state,omitempty"`
	ToState          string   `json:"to_state,omitempty"`
	Details          string   `json:"details,omitempty"`
	PromptDigest     string   `json:"prompt_digest,omitempty"`
	PromptChars      int      `json:"prompt_chars"`
	CreatedAt        string   `json:"created_at"`
}

// auditSummaryResponse 是 GET /v1/runs/{id}/audit 的响应体。
type auditSummaryResponse struct {
	// RunID 是这条判定 run 的标识。
	RunID string `json:"run_id"`
	// EntryCount 是审计链上的记录条数。
	EntryCount int `json:"entry_count"`
	// ModelCallCount 是其中的模型调用条数。
	ModelCallCount int `json:"model_call_count"`
	// ConfirmationCount 是其中的人工确认条数。
	ConfirmationCount int `json:"confirmation_count"`
	// TotalPromptTokens 是全部模型调用的输入 token 合计。
	TotalPromptTokens int `json:"total_prompt_tokens"`
	// TotalCompletionTokens 是全部模型调用的输出 token 合计。
	TotalCompletionTokens int `json:"total_completion_tokens"`
	// TotalTokens 是全部模型调用的 token 合计。
	TotalTokens int `json:"total_tokens"`
	// KnownCostAmount 是算得出成本的那部分调用的成本合计；
	// 全部算不出时为 nil（不是 0）。
	KnownCostAmount *float64 `json:"known_cost_amount,omitempty"`
	// CostCurrency 是成本币种。
	CostCurrency string `json:"cost_currency,omitempty"`
	// CostStatus 是成本可算性：known（全算得出）/ partial（部分算得出）/
	// unknown（全算不出）。
	CostStatus string `json:"cost_status"`
	// UnknownCostCount 是算不出成本的调用条数。
	UnknownCostCount int `json:"unknown_cost_count"`
	// Entries 是审计链明细，按序号升序。
	Entries []auditEntryView `json:"entries"`
}

// handleGetRunAudit 返回一条判定的模型调用与确认动作审计链。
//
// 端点只回账不改账：它连内部状态的边都碰不到。
func (s *Server) handleGetRunAudit(w http.ResponseWriter, r *http.Request) {
	if s.auditReader == nil {
		writeError(w, errcode.Newf(errcode.NotFound, "本服务未装配审计链查询能力"))
		return
	}
	runID := strings.TrimSpace(r.PathValue("id"))
	if runID == "" {
		writeError(w, errcode.Newf(errcode.Validation, "缺少 run_id"))
		return
	}
	// 先确认这条 run 存在：查不到就说查不到，不回一条空审计链。
	if s.runReader != nil {
		if _, err := s.runReader.RunByID(r.Context(), runID); err != nil {
			writeError(w, errcode.As(err))
			return
		}
	}

	entries, err := s.auditReader.AuditEntriesByRun(r.Context(), runID)
	if err != nil {
		writeError(w, errcode.As(err))
		return
	}

	response := buildAuditSummary(runID, entries)
	writeJSON(w, http.StatusOK, response)
}

// buildAuditSummary 把审计链组装成「汇总 + 明细」两段。
//
// 成本汇总的口径要紧：算得出成本的那部分求和，算不出的一部分**不并进来**，
// 另用条数与 cost_status 标出来。把「不知道」按 0 并进合计，
// 报出来的总成本会比真实值小——那是账实不符，不是省事。
func buildAuditSummary(runID string, entries []domain.AuditEntry) auditSummaryResponse {
	summary := auditSummaryResponse{
		RunID:      runID,
		EntryCount: len(entries),
		Entries:    make([]auditEntryView, 0, len(entries)),
		CostStatus: domain.CostStatusUnknown,
	}

	knownSum := 0.0
	knownCount := 0
	currency := ""
	for _, entry := range entries {
		view := buildAuditEntryView(entry)
		summary.Entries = append(summary.Entries, view)

		switch entry.EntryType {
		case domain.AuditEntryModelCall:
			summary.ModelCallCount++
			summary.TotalPromptTokens += entry.PromptTokens
			summary.TotalCompletionTokens += entry.CompletionTokens
			summary.TotalTokens += entry.TotalTokens
			if entry.CostStatus == domain.CostStatusKnown && entry.CostAmount != nil {
				knownSum += *entry.CostAmount
				knownCount++
				if currency == "" {
					currency = entry.CostCurrency
				}
				continue
			}
			summary.UnknownCostCount++
		case domain.AuditEntryConfirmation:
			summary.ConfirmationCount++
		}
	}

	switch {
	case knownCount > 0 && summary.UnknownCostCount > 0:
		summary.CostStatus = domain.CostStatusPartial
	case knownCount > 0:
		summary.CostStatus = domain.CostStatusKnown
	default:
		summary.CostStatus = domain.CostStatusUnknown
	}
	if knownCount > 0 {
		amount := knownSum
		summary.KnownCostAmount = &amount
		summary.CostCurrency = currency
	}
	return summary
}

// buildAuditEntryView 把一条审计记录转成对外形状。
func buildAuditEntryView(entry domain.AuditEntry) auditEntryView {
	view := auditEntryView{
		Seq:              entry.Seq,
		EntryType:        entry.EntryType,
		StepName:         entry.StepName,
		SchemaName:       entry.SchemaName,
		Model:            entry.Model,
		Outcome:          entry.Outcome,
		Degraded:         entry.Degraded,
		ErrorCode:        entry.ErrorCode,
		LatencyMS:        entry.LatencyMS,
		PromptTokens:     entry.PromptTokens,
		CompletionTokens: entry.CompletionTokens,
		TotalTokens:      entry.TotalTokens,
		CostAmount:       entry.CostAmount,
		CostCurrency:     entry.CostCurrency,
		CostStatus:       entry.CostStatus,
		Actor:            entry.Actor,
		Action:           entry.Action,
		Disposition:      entry.Disposition,
		Opinion:          entry.Opinion,
		FromState:        entry.FromState,
		ToState:          entry.ToState,
		Details:          entry.Details,
		PromptDigest:     entry.PromptDigest,
		PromptChars:      entry.PromptChars,
		CreatedAt:        formatTime(entry.CreatedAt),
	}
	if view.CostStatus == "" {
		view.CostStatus = domain.CostStatusUnknown
	}
	return view
}
