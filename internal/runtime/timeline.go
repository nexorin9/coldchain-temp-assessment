// 本文件落「一次判定的时间线」：把步骤记录还原成一条可核对的时间线。
//
// 现场问的是一句大白话：「那次越温判了没有、判到哪一步、卡了多久」。
// 答案不该靠翻日志——库里每一步的起止、状态、尝试序号与退避时长都在，
// 这条时间线就是把它们原样摆出来，包括「这一步失败后等了多久才再试」。
//
// 源产品机制对应（溯源见 spec.md「开源对照」与「参考地基」）：
// 运行详情页把一次函数调用还原成按步骤排开的时间线，每步有自己的位置与宽度
// （github_ref/inngest/ui/packages/components/src/RunDetailsV4/index.ts：
// TimelineBar / TimelineHeader / Timeline，calculateBarPosition 算位置、
// calculateDuration 算耗时、TIMELINE_CONSTANTS 常量）。
// 搬到冷链场景，对应物是：
//
//   - 一次判定 = 一条判定 run（取数 → 抽窗口 → 归因 → 成稿 → 待确认）；
//   - 每一步 = 一个带起止、耗时、状态、尝试序号与退避时长的条目；
//   - 位置 = 相对整条判定起点的偏移，以及在轨道上的起点百分比与宽度百分比。
//
// 三条不许让步的约定：
//   - 位置单调不重叠：时间线是给人读的轴，两步重叠就没有先后可言。
//     展示坐标按链上次序单调推进；真实起止时刻原样保留在 started_at / ended_at，
//     展示坐标不覆盖事实。
//   - 失败留痕：失败的尝试照样占一条，带错误码与退避时长。
//   - 续跑不重复：重启后续跑不产生第二条同一步骤记录，时间线上每步只有一条。
package runtime

import (
	"encoding/json"
	"math"
	"sort"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
)

// recentErrorLimit 是时间线里保留的最近失败条数。
//
// 留 5 条是够用的：一次判定最多五步，每步最多重试三轮，
// 现场要看的失败不会超过这个量级；再多的历史失败属于审计明细，不属于时间线。
const recentErrorLimit = 5

// StepTimeline 是步骤链的一条时间线条目。
//
// 耗时由起止时刻现算，不另存一列：存两份就会有两份对不上的真相。
type StepTimeline struct {
	// Seq 是步骤在链上的次序，从 1 起。
	Seq int `json:"seq"`
	// StepName 是步骤名（取数 / 抽窗口 / 归因 / 成稿 / 待确认）。
	StepName string `json:"step_name"`
	// State 是这一步的状态（running / succeeded / failed / retrying）。
	State string `json:"state"`
	// Attempt 是尝试序号：同一步重试时递增，续跑时接着上次走。
	Attempt int `json:"attempt"`
	// StartedAt 与 EndedAt 是这次尝试的起止时刻；未结束时 EndedAt 为空。
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	// DurationMS 是这次尝试的耗时（毫秒）；仍在执行中时按当前时刻算。
	DurationMS int64 `json:"duration_ms"`
	// DurationUS 是这次尝试的耗时（微秒）。
	//
	// 毫秒与微秒各给一份：判定链上的多数步骤在毫秒级以下是「一眨眼」，
	// 只给毫秒会全部显示成 0，时间线的位置坐标也就失去了分辨力。
	// 两份值都从同一对起止时刻现算，不是两份独立的真相。
	DurationUS int64 `json:"duration_us"`
	// OffsetMS 是本步起点相对整条判定起点的偏移（毫秒）。
	OffsetMS int64 `json:"offset_ms"`
	// OffsetUS 是本步起点相对整条判定起点的偏移（微秒）；位置坐标按它算。
	OffsetUS int64 `json:"offset_us"`
	// EndOffsetMS 是本步终点相对整条判定起点的偏移（毫秒）。
	EndOffsetMS int64 `json:"end_offset_ms"`
	// EndOffsetUS 是本步终点相对整条判定起点的偏移（微秒）。
	EndOffsetUS int64 `json:"end_offset_us"`
	// StartPct 是本步在时间线轨道上的起点百分比（0~100）。
	StartPct float64 `json:"start_pct"`
	// WidthPct 是本步在时间线轨道上的宽度百分比（0~100）。
	WidthPct float64 `json:"width_pct"`
	// Ongoing 表示这一步还没跑完（仍在执行中，耗时随时钟增长）。
	Ongoing bool `json:"ongoing,omitempty"`
	// IsCurrent 表示这一步是判定当前停住的那一步。
	IsCurrent bool `json:"is_current,omitempty"`
	// PayloadKind 是这一步产物的分码；没有产物时为空。
	PayloadKind domain.StepPayloadKind `json:"payload_kind,omitempty"`
	// Payload 是产物信封的 JSON；时间线端点不回载荷正文（正文在 run 详情端点）。
	Payload json.RawMessage `json:"payload,omitempty"`
	// Error 与 ErrorCode 是失败原因与错误码；成功步骤为空。
	Error     string `json:"error,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
	// BackoffMS 是这次尝试失败后等待的退避毫秒数；成功或不再重试时为 0。
	//
	// 它让「这一步卡了多久」看得见：失败的那次尝试带着这段等待，
	// 下一次尝试的起点就是它等完之后。
	BackoffMS int64 `json:"backoff_ms,omitempty"`
}

// TimelineStep 是「当前停在哪一步」的答案。
type TimelineStep struct {
	// Seq 是这一步在链上的次序。
	Seq int `json:"seq"`
	// StepName 是步骤名。
	StepName string `json:"step_name"`
	// State 是这一步的状态。
	State string `json:"state"`
	// Attempt 是尝试序号。
	Attempt int `json:"attempt"`
	// StartedAt 是这次尝试的起点。
	StartedAt time.Time `json:"started_at"`
	// DurationMS 是这次尝试的耗时（毫秒）。
	DurationMS int64 `json:"duration_ms"`
}

// TimelineError 是时间线上的一次失败。
type TimelineError struct {
	// StepName 是失败的步骤名。
	StepName string `json:"step_name"`
	// Seq 是该步骤在链上的次序。
	Seq int `json:"seq"`
	// Attempt 是失败的那次尝试序号。
	Attempt int `json:"attempt"`
	// ErrorCode 是错误码；它是检索失败原因的唯一可靠依据。
	ErrorCode string `json:"error_code"`
	// Message 是失败原因的中文说明。
	Message string `json:"message"`
	// At 是这次失败的时刻。
	At time.Time `json:"at"`
}

// TimelineView 是一次判定的运行时间线。
type TimelineView struct {
	// RunID 是这条判定的标识。
	RunID string `json:"run_id"`
	// RunStartedAt 是整条判定的起点（最早一步的起点）。
	RunStartedAt time.Time `json:"run_started_at"`
	// RunEndedAt 是整条判定的终点；还在执行中时为空。
	RunEndedAt *time.Time `json:"run_ended_at,omitempty"`
	// TotalDurationMS 是整条判定从起点到终点的跨度（毫秒）。
	TotalDurationMS int64 `json:"total_duration_ms"`
	// StepCount 是时间线条目数：一次尝试算一条。
	StepCount int `json:"step_count"`
	// CurrentStep 是判定当前停住的那一步；一条都没跑时为空。
	CurrentStep *TimelineStep `json:"current_step,omitempty"`
	// Entries 是逐条时间线条目，按链上次序与尝试序号排列。
	Entries []StepTimeline `json:"entries"`
	// RecentErrors 是最近的失败，按时间倒序，最多 recentErrorLimit 条。
	RecentErrors []TimelineError `json:"recent_errors"`
}

// Timeline 把步骤记录转成时间线条目。
//
// 顺序按链上次序（seq）升序；同一步的多次尝试按 attempt 升序。
// 位置坐标按该顺序单调推进，因此条目之间不重叠。
func Timeline(steps []domain.Step) []StepTimeline {
	entries, _, _, _ := layoutTimeline(steps, time.Now())
	return entries
}

// BuildTimeline 把步骤记录还原成一条完整的运行时间线。
//
// now 只用于给「仍在执行中」的步骤算当前耗时；传当前时刻即可。
func BuildTimeline(runID string, steps []domain.Step) TimelineView {
	return BuildTimelineAt(runID, steps, time.Now())
}

// BuildTimelineAt 在指定时刻还原运行时间线。
//
// 单独留一个带时刻的版本，是为了让「还在跑的那一步耗时多少」这件事
// 可复现：测试注入固定时刻，得到的坐标每次都一样。
func BuildTimelineAt(runID string, steps []domain.Step, now time.Time) TimelineView {
	entries, runStart, runEnd, total := layoutTimeline(steps, now)

	view := TimelineView{
		RunID:           runID,
		RunStartedAt:    runStart,
		RunEndedAt:      runEnd,
		TotalDurationMS: total,
		StepCount:       len(entries),
		Entries:         entries,
		RecentErrors:    []TimelineError{},
	}
	if len(entries) == 0 {
		return view
	}

	// 当前步 = 最后一条：判定是顺序推进的，最后一条就是它停住的地方。
	last := entries[len(entries)-1]
	last.IsCurrent = true
	view.Entries[len(entries)-1] = last
	view.CurrentStep = &TimelineStep{
		Seq:        last.Seq,
		StepName:   last.StepName,
		State:      last.State,
		Attempt:    last.Attempt,
		StartedAt:  last.StartedAt,
		DurationMS: last.DurationMS,
	}

	for i := len(entries) - 1; i >= 0 && len(view.RecentErrors) < recentErrorLimit; i-- {
		entry := entries[i]
		if entry.ErrorCode == "" && entry.Error == "" {
			continue
		}
		at := entry.StartedAt
		if entry.EndedAt != nil {
			at = *entry.EndedAt
		}
		view.RecentErrors = append(view.RecentErrors, TimelineError{
			StepName:  entry.StepName,
			Seq:       entry.Seq,
			Attempt:   entry.Attempt,
			ErrorCode: entry.ErrorCode,
			Message:   entry.Error,
			At:        at,
		})
	}
	return view
}

// layoutTimeline 算出时间线条目与整条判定的时间跨度。
//
// 三件事一次算完：条目顺序、每步耗时、以及每步在轨道上的位置。
//
// 位置坐标内部按微秒算：判定链上多数步骤在毫秒级以下是「一眨眼」，
// 只按毫秒算坐标会把它们全压成同一个点，时间线就失去了分辨力。
// 对外的毫秒值是微秒值截断来的，不是另一份独立的真相。
//
// 位置坐标的算法只有一条规矩：后一条的起点不早于前一条的终点。
// 步骤链是顺序推进的，正常情况下本来就满足；出现交错（时钟回拨、
// 补投的旧采样排在后面）时，展示坐标仍然单调不重叠——
// 坐标是给人看的轴，事实时刻仍以 started_at / ended_at 为准。
func layoutTimeline(steps []domain.Step, now time.Time) ([]StepTimeline, time.Time, *time.Time, int64) {
	ordered := make([]domain.Step, len(steps))
	copy(ordered, steps)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Seq != ordered[j].Seq {
			return ordered[i].Seq < ordered[j].Seq
		}
		if ordered[i].Attempt != ordered[j].Attempt {
			return ordered[i].Attempt < ordered[j].Attempt
		}
		return ordered[i].StepName < ordered[j].StepName
	})
	if len(ordered) == 0 {
		return []StepTimeline{}, time.Time{}, nil, 0
	}

	runStart := ordered[0].StartedAt
	for _, step := range ordered {
		if step.StartedAt.Before(runStart) {
			runStart = step.StartedAt
		}
	}

	entries := make([]StepTimeline, 0, len(ordered))
	prevEnd := int64(0)
	for _, step := range ordered {
		entry := StepTimeline{
			Seq:         step.Seq,
			StepName:    step.StepName,
			State:       step.State,
			Attempt:     step.Attempt,
			StartedAt:   step.StartedAt,
			EndedAt:     step.EndedAt,
			PayloadKind: step.PayloadKind,
			Payload:     step.Payload,
			Error:       step.Error,
			ErrorCode:   step.ErrorCode,
			BackoffMS:   step.BackoffMS,
		}

		var duration int64
		if step.EndedAt != nil {
			duration = step.EndedAt.Sub(step.StartedAt).Microseconds()
		} else {
			// 还没结束：这一步仍在跑，耗时按当前时刻算，并标出来。
			entry.Ongoing = true
			duration = now.Sub(step.StartedAt).Microseconds()
		}
		if duration < 0 {
			duration = 0
		}

		offset := step.StartedAt.Sub(runStart).Microseconds()
		if offset < 0 {
			offset = 0
		}
		if offset < prevEnd {
			offset = prevEnd
		}
		entry.DurationUS = duration
		entry.DurationMS = duration / 1000
		entry.OffsetUS = offset
		entry.OffsetMS = offset / 1000
		entry.EndOffsetUS = offset + duration
		entry.EndOffsetMS = entry.EndOffsetUS / 1000
		prevEnd = entry.EndOffsetUS
		entries = append(entries, entry)
	}

	total := prevEnd
	for i := range entries {
		if total > 0 {
			entries[i].StartPct = round2(float64(entries[i].OffsetUS) / float64(total) * 100)
			entries[i].WidthPct = round2(float64(entries[i].DurationUS) / float64(total) * 100)
		}
	}

	runEnd := runStart.Add(time.Duration(total) * time.Microsecond)
	var runEndPtr *time.Time
	if len(entries) == 0 || !entries[len(entries)-1].Ongoing {
		runEndPtr = &runEnd
	}
	return entries, runStart, runEndPtr, total / 1000
}

// round2 把百分比保留两位小数，避免响应体里出现一长串浮点尾巴。
func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
