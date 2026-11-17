// 本文件落判定 run 的查询端点：GET /v1/runs/{id} 与 GET /v1/runs/{id}/timeline。
//
// 它回答的是现场最常问的一句话：「那次越温判了没有、判到哪一步、谁确认的」。
// 答案不能靠翻日志：判定 run 的状态、每一步的起止与产物都落在库里，
// 这个端点就是把库里的真相原样端出来。
//
// 端点回四段：
//   - run：判定本身的总体状态（业务状态、执行态、窗口起止、外部引用、写回状态）；
//   - window：越温窗口摘要（起止、峰值、持续分钟、温区来源）；
//   - steps：步骤时间线，每步的序号、名称、状态、尝试次数、起止、耗时与位置坐标；
//   - products：按分码取回的结论产物（越限窗口、归因结论、成稿草稿）。
//
// 时间线端点（/timeline）是同一条判定的另一种看法：只讲时间，
// 不带产物正文——正文按分码在详情端点取，两处各回一半，凑一起才完整。
//
// 三条约定：
//   - 找不到就说找不到：run 不存在返回 404 与 E_NOT_FOUND，不回空壳。
//   - 产物按分码取回：不同形状的产物各走各的分码，不做尽力解析。
//   - 鉴权在中间件：凭据不对的请求进不到这里，也拿不到任何判定内容。
package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/runtime"
)

// RunReader 是判定 run 的读取能力边界。
//
// 只声明本包真正用到的两个方法：取一条 run 的状态快照、取它的步骤记录。
// 存储实现（SQLite）与部署形态无关，这一层不关心它从哪儿来。
type RunReader interface {
	// RunByID 回读一条判定 run 的状态快照。
	RunByID(ctx context.Context, runID string) (domain.RunRecord, error)
	// RunStepsByRun 回读一条 run 的步骤记录。
	RunStepsByRun(ctx context.Context, runID string) ([]domain.Step, error)
}

// runView 是 run 状态快照的对外形状。
//
// 它与库里的列一一对应，但不把整张快照原样倒出去：
// 温区配置这类内部口径不进响应体——现场看的是「判了什么、停在哪」。
type runView struct {
	RunID          string `json:"run_id"`
	PointID        string `json:"point_id"`
	State          string `json:"state"`
	ExecState      string `json:"exec_state"`
	WindowStart    string `json:"window_start,omitempty"`
	WindowEnd      string `json:"window_end,omitempty"`
	ExternalRef    string `json:"external_ref,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	FailedStep     string `json:"failed_step,omitempty"`
	ErrorCode      string `json:"error_code,omitempty"`
	WritebackState string `json:"writeback_state,omitempty"`
	WrittenBackAt  string `json:"written_back_at,omitempty"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
	Zone           string `json:"zone,omitempty"`
	ProfilePresent bool   `json:"profile_present"`
	// Degraded 标记这条判定的归因结论是降级得来的（模型归因不可用，
	// 结论由规则基线单路给出）。DegradeReason 是降级原因的错误码。
	Degraded      bool   `json:"degraded"`
	DegradeReason string `json:"degrade_reason,omitempty"`
}

// windowSummary 是越温窗口的对外摘要。
//
// 现场看窗口只关心四件事：什么时候开始、什么时候结束、峰值多少、持续多久。
// 温区来源一并带出：这份结论用的是点位配置还是制品说明书条目，
// 是复核时的第一个问题。
type windowSummary struct {
	WindowStart      string   `json:"window_start,omitempty"`
	WindowEnd        string   `json:"window_end,omitempty"`
	PeakTemperatureC *float64 `json:"peak_temperature_c,omitempty"`
	DurationMinutes  *int     `json:"duration_minutes,omitempty"`
	SampleCount      *int     `json:"sample_count,omitempty"`
	Zone             string   `json:"zone,omitempty"`
	ZoneSource       string   `json:"zone_source,omitempty"`
	PeakAt           string   `json:"peak_at,omitempty"`
}

// runDetailResponse 是 GET /v1/runs/{id} 的响应体。
type runDetailResponse struct {
	// Run 是判定本身的总体状态。
	Run runView `json:"run"`
	// Window 是越温窗口摘要；还没抽窗口时为空。
	Window *windowSummary `json:"window,omitempty"`
	// Steps 是步骤时间线：每步的序号、名称、状态、尝试次数、起止与耗时。
	Steps []runtime.StepTimeline `json:"steps"`
	// StepCount 是步骤条数；用它一眼核对「这条判定跑到第几步」。
	StepCount int `json:"step_count"`
	// CurrentStep 是判定当前停住的那一步。
	CurrentStep *runtime.TimelineStep `json:"current_step,omitempty"`
	// RecentErrors 是最近的失败，按时间倒序。
	RecentErrors []runtime.TimelineError `json:"recent_errors"`
	// Products 是按分码取回的结论产物。
	Products map[string]any `json:"products"`
}

// timelineResponse 是 GET /v1/runs/{id}/timeline 的响应体。
//
// 它只讲一件事：这次判定的时间线。步骤产物不在这里回——
// 产物按分码在 run 详情端点取，两处各回一半，凑一起才是完整的判定。
type timelineResponse struct {
	RunID           string                  `json:"run_id"`
	RunStartedAt    string                  `json:"run_started_at"`
	RunEndedAt      string                  `json:"run_ended_at,omitempty"`
	TotalDurationMS int64                   `json:"total_duration_ms"`
	StepCount       int                     `json:"step_count"`
	CurrentStep     *runtime.TimelineStep   `json:"current_step,omitempty"`
	Entries         []runtime.StepTimeline  `json:"entries"`
	RecentErrors    []runtime.TimelineError `json:"recent_errors"`
}

// handleGetRun 返回一条判定的总体状态、窗口摘要与步骤时间线。
//
// 端点不区分「这条判定刚跑完」还是「进程重启后续跑完」：
// 库里留的是什么，这里就回什么——这正是查它存在的意义。
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	if s.runReader == nil {
		writeError(w, errcode.Newf(errcode.NotFound, "本服务未装配判定 run 查询能力"))
		return
	}
	runID := strings.TrimSpace(r.PathValue("id"))
	if runID == "" {
		writeError(w, errcode.Newf(errcode.Validation, "缺少 run_id"))
		return
	}

	rec, err := s.runReader.RunByID(r.Context(), runID)
	if err != nil {
		writeError(w, errcode.As(err))
		return
	}
	steps, err := s.runReader.RunStepsByRun(r.Context(), runID)
	if err != nil {
		writeError(w, errcode.As(err))
		return
	}

	view := runtime.BuildTimeline(runID, steps)
	response := runDetailResponse{
		Run:          buildRunView(rec),
		Window:       buildWindowSummary(rec, steps),
		Steps:        view.Entries,
		StepCount:    view.StepCount,
		CurrentStep:  view.CurrentStep,
		RecentErrors: view.RecentErrors,
		Products:     map[string]any{},
	}
	for _, kind := range domain.PayloadKinds() {
		if value, ok := runtime.ProductFrom(steps, kind); ok {
			response.Products[string(kind)] = value
		}
	}
	writeJSON(w, http.StatusOK, response)
}

// handleGetRunTimeline 返回一次判定的运行时间线。
//
// 时间线回答「哪一步慢、哪一步失败重试过几次、现在停在哪」：
// 每一步的起止、耗时、状态、尝试序号与退避时长都在里面，
// 位置坐标按链上次序单调推进，两步不会重叠。
func (s *Server) handleGetRunTimeline(w http.ResponseWriter, r *http.Request) {
	if s.runReader == nil {
		writeError(w, errcode.Newf(errcode.NotFound, "本服务未装配判定 run 查询能力"))
		return
	}
	runID := strings.TrimSpace(r.PathValue("id"))
	if runID == "" {
		writeError(w, errcode.Newf(errcode.Validation, "缺少 run_id"))
		return
	}

	// 先确认这条 run 存在：查不到就说查不到，不回一条空时间线。
	if _, err := s.runReader.RunByID(r.Context(), runID); err != nil {
		writeError(w, errcode.As(err))
		return
	}
	steps, err := s.runReader.RunStepsByRun(r.Context(), runID)
	if err != nil {
		writeError(w, errcode.As(err))
		return
	}

	view := runtime.BuildTimeline(runID, steps)
	// 时间线不带产物正文：正文按分码在 run 详情端点取，
	// 这里再塞一份只会让响应体大一倍。
	entries := view.Entries
	for i := range entries {
		entries[i].Payload = nil
	}

	response := timelineResponse{
		RunID:           view.RunID,
		RunStartedAt:    formatTime(view.RunStartedAt),
		RunEndedAt:      formatTimePtr(view.RunEndedAt),
		TotalDurationMS: view.TotalDurationMS,
		StepCount:       view.StepCount,
		CurrentStep:     view.CurrentStep,
		Entries:         entries,
		RecentErrors:    view.RecentErrors,
	}
	if response.RecentErrors == nil {
		response.RecentErrors = []runtime.TimelineError{}
	}
	writeJSON(w, http.StatusOK, response)
}

// buildWindowSummary 组装越温窗口摘要。
//
// 优先取窗口步骤的产物（里面有峰值、持续分钟与温区来源）；
// 产物不在（还没抽窗口、或步骤失败）时退回 run 快照上的起止时刻。
func buildWindowSummary(rec domain.RunRecord, steps []domain.Step) *windowSummary {
	summary := &windowSummary{}
	if rec.Run.WindowStart != nil {
		summary.WindowStart = formatTime(*rec.Run.WindowStart)
	}
	if rec.Run.WindowEnd != nil {
		summary.WindowEnd = formatTime(*rec.Run.WindowEnd)
	}
	if rec.Profile != nil {
		summary.Zone = formatZone(rec.Profile.ZoneMinC, rec.Profile.ZoneMaxC)
	}

	payload, ok := runtime.ProductAs[domain.WindowPayload](steps, domain.PayloadWindow)
	if !ok {
		if summary.WindowStart == "" && summary.WindowEnd == "" && summary.Zone == "" {
			return nil
		}
		return summary
	}
	peak := payload.Window.PeakC
	duration := payload.Window.DurationMinutes
	samples := payload.Window.SampleCount
	summary.PeakTemperatureC = &peak
	summary.DurationMinutes = &duration
	summary.SampleCount = &samples
	if payload.ZoneSource != "" {
		summary.ZoneSource = payload.ZoneSource
	}
	if !payload.Window.PeakAt.IsZero() {
		summary.PeakAt = formatTime(payload.Window.PeakAt)
	}
	return summary
}

// formatTime 把时刻格式化成 RFC3339（带偏移）。
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02T15:04:05Z07:00")
}

// formatTimePtr 把时刻指针格式化成 RFC3339（带偏移）；空指针回空串。
func formatTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return formatTime(*t)
}

// buildRunView 把状态快照转成对外形状。
func buildRunView(rec domain.RunRecord) runView {
	view := runView{
		RunID:          rec.Run.RunID,
		PointID:        rec.Run.PointID,
		State:          rec.Run.State,
		ExecState:      string(rec.Run.ExecState),
		ExternalRef:    rec.Run.ExternalRef,
		IdempotencyKey: rec.Run.IdempotencyKey,
		FailedStep:     rec.FailedStep,
		ErrorCode:      rec.ErrorCode,
		WritebackState: rec.WritebackState,
		ProfilePresent: rec.Profile != nil,
		Degraded:       rec.Run.Degraded,
		DegradeReason:  rec.Run.DegradeReason,
	}
	if rec.Run.WindowStart != nil {
		view.WindowStart = rec.Run.WindowStart.Format("2006-01-02T15:04:05Z07:00")
	}
	if rec.Run.WindowEnd != nil {
		view.WindowEnd = rec.Run.WindowEnd.Format("2006-01-02T15:04:05Z07:00")
	}
	if rec.WrittenBackAt != nil {
		view.WrittenBackAt = rec.WrittenBackAt.Format("2006-01-02T15:04:05Z07:00")
	}
	if !rec.CreatedAt.IsZero() {
		view.CreatedAt = rec.CreatedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	if !rec.UpdatedAt.IsZero() {
		view.UpdatedAt = rec.UpdatedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	if rec.Profile != nil {
		view.Zone = formatZone(rec.Profile.ZoneMinC, rec.Profile.ZoneMaxC)
	}
	return view
}

// formatZone 把温区上下限格式化成「2~8℃」。
func formatZone(minC, maxC float64) string {
	return trimFloat(minC) + "~" + trimFloat(maxC) + "℃"
}

// trimFloat 把浮点数格式化成不带多余小数位的短文本。
func trimFloat(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}
