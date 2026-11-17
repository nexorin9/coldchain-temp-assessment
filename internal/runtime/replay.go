// 本文件落离线回放：把已经入库的历史温度事件按时间顺序重跑一遍，
// 重建越温窗口，并与当初在线判定的结论逐字段比对。
//
// 为什么要有这条通道：药学部与质控在飞检、内审时被问的从来不是
// 「系统判了什么」，而是「这批读数当初为什么判成那样」。
// 记录仪导出、网关补投、人工补录之后，库里那批原始读数还在，
// 规则口径却可能已经改过；只有把同一批读数重跑一遍、和当初的结论
// 逐字段对上，才能说清「当初那条结论是对的，还是现在的口径变了」。
//
// 源产品机制对应（溯源见 spec.md「开源对照」与「参考地基」）：
// 源产品的 Event API 把事件写进数据库，供历史与回放使用
// （README「Database — Persists system data and history including … Events」），
// 回放走的是与在线同一套函数与状态存储。搬到冷链场景，对应物是：
//
//   - 回放源 = 库里已入库的温度事件（按点位与时间区间回读）
//   - 重建   = 与在线判定同一套调度器 + 同一套规则 + 同一份温区配置
//   - 比对   = 拿窗口幂等键找到当初在线判的那条 run，逐字段对窗口结论
//
// 三条不许让步的约定：
//   - 结论一致才算数：回放窗口必须与在线结论逐字段对得上；
//     对不上时要说清差在哪个字段，而不是「大致相同」。
//   - 回放不改账：回放不写 runs / run_steps / events 三张表，
//     不覆盖在线判定记录，也不触发出站写回台账。
//   - 回放有独立标记：回放重建出的 run 标识带 replay_ 前缀，
//     与在线判定的 run_ 一眼可分，绝不重名。
package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
)

// ReplayRunIDPrefix 是回放重建出的判定 run 的标识前缀。
//
// 在线判定用 run_…，回放用 replay_…。两者用同一套窗口幂等键，
// 但标识绝不重名：飞检翻库时，一眼就要能分清哪条是当初判的、
// 哪条是事后复算的。
const ReplayRunIDPrefix = "replay_"

// 温区配置来源标识。
const (
	// ProfileSourceFile：温区配置来自本次回放显式指定的配置文件。
	ProfileSourceFile = "profile_file"
	// ProfileSourceOnlineRun：温区配置取自当初在线判定随 run 落库的那一份。
	//
	// 它是复算该用的口径：只有用「当初那一份」重跑，才能回答
	// 「当初那条结论对不对」；换成今天的温区配置重跑，
	// 得到的差异说明的是口径变了，不是当初判错了。
	ProfileSourceOnlineRun = "online_run"
)

// LedgerDeviation 是一条待写回冷链台账的越温偏差。
//
// 字段与 spec 的台账写入体规范对齐（外部引用、点位、起止、峰值、持续时长、温区）。
type LedgerDeviation struct {
	RunID            string
	PointID          string
	DeviceName       string
	ExternalRef      string
	IdempotencyKey   string
	StartedAt        time.Time
	EndedAt          time.Time
	PeakTemperatureC float64
	DurationMinutes  int
	Zone             string
}

// LedgerWriteback 是出站台账写回口。
//
// 它在本轮只做一件事：把「回放绝不写台账」这条纪律钉成一个可断言的事实。
// 真正的台账适配器在后续任务落地，届时它的实现会挂到**在线确认**路径上；
// 回放路径上挂的必须是拒绝实现——挂错了，回放直接拒绝执行，
// 而不是「挂上但其实不调用」：那样一条接线错误会被藏起来。
type LedgerWriteback interface {
	// WriteDeviation 把一条越温偏差写回冷链台账。
	WriteDeviation(ctx context.Context, dev LedgerDeviation) error
}

// OnlineConclusionStore 是回放比对所需的在线结论来源。
//
// 抽成接口是为了让运行时层不绑死存储实现：生产用 SQLite，测试用内存库，
// 两者口径一致。
type OnlineConclusionStore interface {
	// RunByIdempotencyKey 按窗口幂等键回读判定 run；找不到返回 E_NOT_FOUND。
	RunByIdempotencyKey(ctx context.Context, key string) (domain.RunRecord, error)
	// RunsByPointInRange 返回某点位在时间区间内的判定 run。
	RunsByPointInRange(ctx context.Context, pointID string, from, to time.Time) ([]domain.RunRecord, error)
	// StepPayloadByRunKind 回读一条 run 上某个分码的成功产物。
	StepPayloadByRunKind(ctx context.Context, runID string, kind domain.StepPayloadKind) (any, bool, error)
}

// ReplayOptions 是一次离线回放的参数。
type ReplayOptions struct {
	// PointID 是回放覆盖的点位（必填）。
	PointID string
	// From 与 To 是回放的时间区间（闭区间，必填）。
	From time.Time
	To   time.Time
	// Profile 是点位温区配置；为 nil 时从当初在线判定落库的那一份取。
	Profile *domain.PointProfile
	// Source 是历史事件来源（通常是本地库）。
	Source SampleSource
	// Online 是在线结论来源；为 nil 时本次回放只重建窗口、不做比对。
	Online OnlineConclusionStore
}

// FieldDiff 是一处字段级差异。
//
// 差异要落到字段上：「结论不一致」没法拿去问人，
// 「峰值 12.40℃ vs 13.10℃、采样 4 条 vs 6 条」才问得下去。
type FieldDiff struct {
	// Field 是差异字段名（window_start / peak_c / sample_count …）。
	Field string `json:"field"`
	// Replay 是回放重建出的取值。
	Replay string `json:"replay"`
	// Online 是在线结论的取值。
	Online string `json:"online"`
}

// ReplayWindow 是回放重建出的一条越温窗口及其与在线结论的比对结果。
type ReplayWindow struct {
	// RunID 是回放重建出的 run 标识（带 replay_ 前缀）。
	RunID string `json:"run_id"`
	// IdempotencyKey 是窗口幂等键：回放与在线判定靠它对上号。
	IdempotencyKey string `json:"idempotency_key"`
	// ExternalRef 是外部引用（点位 + 窗口起点 UTC）。
	ExternalRef string `json:"external_ref"`
	// Window 是回放重建出的越温窗口。
	Window domain.ExcursionWindow `json:"window"`
	// OnlineRunID 是当初在线判定的 run 标识；没有对应在线结论时为空。
	OnlineRunID string `json:"online_run_id,omitempty"`
	// OnlineWindow 是在线判定当时的窗口结论；没有对应在线结论时为空。
	OnlineWindow *domain.ExcursionWindow `json:"online_window,omitempty"`
	// OnlineState / OnlineExecState / OnlineWritebackState 是在线判定当时的状态快照。
	//
	// 复盘时经常要连着问一句「那条结论后来确认了没有、写回台账了没有」，
	// 一起带出来省得再翻一次库。
	OnlineState          string `json:"online_state,omitempty"`
	OnlineExecState      string `json:"online_exec_state,omitempty"`
	OnlineWritebackState string `json:"online_writeback_state,omitempty"`
	// Compared 表示这次是否真的做了逐字段比对。
	Compared bool `json:"compared"`
	// Matched 表示逐字段比对全部一致。
	Matched bool `json:"matched"`
	// CompareNote 说明没比对上的原因（在线无结论 / 只留了起止 / …）。
	CompareNote string `json:"compare_note,omitempty"`
	// Diffs 是逐字段差异；一致时为空。
	Diffs []FieldDiff `json:"diffs,omitempty"`
}

// OnlineRunRef 是一条「在线判过、但这次回放重建不出来」的判定。
//
// 它是复盘时最该看的一类记录：要么原始读数在库里缺了，
// 要么当初用的温区口径与今天重跑用的不是一份。
type OnlineRunRef struct {
	RunID          string     `json:"run_id"`
	ExternalRef    string     `json:"external_ref,omitempty"`
	IdempotencyKey string     `json:"idempotency_key,omitempty"`
	State          string     `json:"state,omitempty"`
	WritebackState string     `json:"writeback_state,omitempty"`
	WindowStart    *time.Time `json:"window_start,omitempty"`
	WindowEnd      *time.Time `json:"window_end,omitempty"`
}

// ReplayResult 是一次离线回放的结果。
type ReplayResult struct {
	// PointID 是回放覆盖的点位。
	PointID string `json:"point_id"`
	// From 与 To 是回放区间。
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// ProfileSource 说明本次回放用的温区配置是哪一份。
	ProfileSource string `json:"profile_source"`
	// SampleCount 是回读到的历史采样条数。
	SampleCount int `json:"sample_count"`
	// ReplayedRunCount 是回放重建出的判定 run 条数。
	ReplayedRunCount int `json:"replayed_run_count"`
	// Compared / Matched / Mismatched / Uncompared 是比对口径的四个计数。
	Compared   int `json:"compared"`
	Matched    int `json:"matched"`
	Mismatched int `json:"mismatched"`
	Uncompared int `json:"uncompared"`
	// Windows 是逐条回放窗口及其比对结果。
	Windows []ReplayWindow `json:"windows"`
	// OnlineWithoutReplay 是「在线判过、回放重建不出来」的判定。
	OnlineWithoutReplay []OnlineRunRef `json:"online_without_replay,omitempty"`
	// WritebackCalls 是回放期间触发的台账写回次数，恒为 0。
	WritebackCalls int `json:"writeback_calls"`
	// Persisted 标明回放是否把结果落库，恒为 false。
	//
	// 回放不落库是纪律，不是还没做：落库的判定会被启动续跑扫描当成
	// 在线判定续跑，回放结论混进去会让人分不清哪条是真判的。
	Persisted bool `json:"persisted"`
}

// Replayer 是离线回放器。
type Replayer struct {
	// logger 记录回放过程；为 nil 时回落到默认日志器（写 stderr）。
	logger *logging.Logger
	// writeback 是台账写回口；回放路径上必须为 nil。
	//
	// 挂上它不是「多一个能力」，而是接线错了：回放一旦写回台账，
	// 同一次越温在台账里就会留下两条记录。挂上就直接拒绝执行。
	writeback LedgerWriteback
}

// NewReplayer 创建回放器。logger 为 nil 时回落到默认日志器。
func NewReplayer(logger *logging.Logger) *Replayer {
	if logger == nil {
		logger = logging.Default()
	}
	return &Replayer{logger: logger}
}

// WithWriteback 挂载台账写回口并返回自身，便于链式装配。
//
// 挂上之后回放会拒绝执行（见 Replay）。这是刻意的：
// 「回放不写台账」如果只靠「我们没调用它」来保证，
// 迟早会有人把真实写回器接到回放路径上而不自知。
// 做成拒绝执行，接线错误当场暴露，而不是等到台账里多了一条越温记录才发现。
func (r *Replayer) WithWriteback(wb LedgerWriteback) *Replayer {
	r.writeback = wb
	return r
}

// Replay 按时间顺序重跑历史事件，重建越温窗口并与在线结论逐字段比对。
//
// 回放走的是与在线判定同一套代码：同一套调度器（窗口开闭、同窗去抖）、
// 同一套规则（温区比较、窗口抽取）、同一份温区配置。
// 换一套算法去「复算」，复算出来的差异说明的是算法不同，不是当初判错了。
func (r *Replayer) Replay(ctx context.Context, opts ReplayOptions) (ReplayResult, error) {
	result := ReplayResult{
		PointID: opts.PointID,
		From:    opts.From,
		To:      opts.To,
		Windows: []ReplayWindow{},
	}

	if r.writeback != nil {
		return result, errcode.Newf(errcode.Validation,
			"回放路径不允许挂载台账写回口：回放结论只用于核对，不推进业务状态、不写回冷链台账")
	}
	if strings.TrimSpace(opts.PointID) == "" {
		return result, errcode.Newf(errcode.Validation, "离线回放需要 point_id")
	}
	if opts.From.IsZero() || opts.To.IsZero() {
		return result, errcode.Newf(errcode.Validation, "离线回放需要明确的起止时刻")
	}
	if opts.To.Before(opts.From) {
		return result, errcode.Newf(errcode.Validation,
			"回放区间非法：起始时刻 %s 晚于结束时刻 %s",
			opts.From.Format(time.RFC3339), opts.To.Format(time.RFC3339))
	}
	if opts.Source == nil {
		return result, errcode.Newf(errcode.Validation, "离线回放缺少历史事件来源")
	}

	profile, profileSource, err := r.resolveProfile(ctx, opts)
	if err != nil {
		return result, err
	}
	result.ProfileSource = profileSource

	samples, err := opts.Source.EventsInRange(ctx, opts.PointID, opts.From, opts.To)
	if err != nil {
		return result, err
	}
	result.SampleCount = len(samples)
	if len(samples) == 0 {
		return result, errcode.Newf(errcode.NotFound,
			"回放区间 %s ~ %s 内没有点位 %s 的温度采样",
			opts.From.Format(time.RFC3339), opts.To.Format(time.RFC3339), opts.PointID)
	}

	// 重建窗口：与在线判定同一套调度器与规则，只有 run 标识前缀不同。
	scheduler := NewScheduler(r.logger).WithRunIDPrefix(ReplayRunIDPrefix)
	if _, err := scheduler.DispatchSeries(ctx, samples, profile); err != nil {
		return result, err
	}

	runs := scheduler.Runs()
	result.ReplayedRunCount = len(runs)
	for _, run := range runs {
		window, ok := scheduler.WindowOf(run.RunID)
		if !ok || window == nil {
			continue
		}
		entry := ReplayWindow{
			RunID:          run.RunID,
			IdempotencyKey: run.IdempotencyKey,
			ExternalRef:    run.ExternalRef,
			Window:         *window,
		}
		r.compareWithOnline(ctx, opts, &entry)
		switch {
		case !entry.Compared:
			result.Uncompared++
		default:
			result.Compared++
			if entry.Matched {
				result.Matched++
			} else {
				result.Mismatched++
			}
		}
		result.Windows = append(result.Windows, entry)
	}

	r.collectOnlineWithoutReplay(ctx, opts, &result)

	r.logger.Info("replay_completed",
		"point_id", opts.PointID,
		"from", opts.From.Format(time.RFC3339),
		"to", opts.To.Format(time.RFC3339),
		"sample_count", result.SampleCount,
		"replayed_runs", result.ReplayedRunCount,
		"compared", result.Compared,
		"matched", result.Matched,
		"mismatched", result.Mismatched,
		"uncompared", result.Uncompared,
		"writeback_calls", result.WritebackCalls)
	return result, nil
}

// resolveProfile 取本次回放所用的温区配置。
//
// 显式给了就用给的那份；没给就用当初在线判定随 run 落库的那一份——
// 「用当初那一份重跑」才是复算该有的口径。库里连一份在线判定都没有时，
// 明说取不到，请显式指定，绝不拿默认值顶替出一个结论。
func (r *Replayer) resolveProfile(ctx context.Context, opts ReplayOptions) (*domain.PointProfile, string, error) {
	if opts.Profile != nil {
		if strings.TrimSpace(opts.Profile.PointID) == "" {
			return nil, "", errcode.Newf(errcode.Validation,
				"指定的温区配置缺少 point_id，无法确认它适用于哪个点位")
		}
		if opts.Profile.PointID != opts.PointID {
			return nil, "", errcode.Newf(errcode.Validation,
				"点位不一致：回放点位 %s，温区配置点位 %s",
				opts.PointID, opts.Profile.PointID)
		}
		return opts.Profile, ProfileSourceFile, nil
	}
	if opts.Online == nil {
		return nil, "", errcode.Newf(errcode.Validation,
			"离线回放缺少点位温区配置：请显式指定，或先让该点位跑过一次在线判定（温区配置随判定落库）")
	}

	// 先在回放区间内找；区间内没有就放宽到该点位的全部在线判定。
	records, err := opts.Online.RunsByPointInRange(ctx, opts.PointID, opts.From, opts.To)
	if err != nil {
		return nil, "", err
	}
	if profile := profileFromRecords(records, opts.PointID); profile != nil {
		return profile, ProfileSourceOnlineRun, nil
	}
	records, err = opts.Online.RunsByPointInRange(ctx, opts.PointID, time.Time{}, time.Time{})
	if err != nil {
		return nil, "", err
	}
	if profile := profileFromRecords(records, opts.PointID); profile != nil {
		return profile, ProfileSourceOnlineRun, nil
	}
	return nil, "", errcode.Newf(errcode.NotFound,
		"点位 %s 没有在线判定记录，取不到当初的温区配置；请显式指定温区配置后重试",
		opts.PointID)
}

// profileFromRecords 从一批在线判定里取出该点位的温区配置。
func profileFromRecords(records []domain.RunRecord, pointID string) *domain.PointProfile {
	for _, rec := range records {
		if rec.Profile == nil || rec.Profile.PointID != pointID {
			continue
		}
		return rec.Profile
	}
	return nil
}

// compareWithOnline 拿回放窗口与在线结论逐字段比对。
func (r *Replayer) compareWithOnline(ctx context.Context, opts ReplayOptions, entry *ReplayWindow) {
	if opts.Online == nil {
		entry.CompareNote = "未提供在线结论来源，本次回放只重建窗口、不做比对"
		return
	}

	rec, err := opts.Online.RunByIdempotencyKey(ctx, entry.IdempotencyKey)
	if err != nil {
		e := errcode.As(err)
		if e.Code == errcode.NotFound {
			entry.CompareNote = "在线没有对应判定结论（该越温尚未被判过，或在线判定用的是另一份温区口径）"
			return
		}
		entry.CompareNote = "读取在线结论失败：" + e.Details
		return
	}

	entry.OnlineRunID = rec.Run.RunID
	entry.OnlineState = rec.Run.State
	entry.OnlineExecState = string(rec.Run.ExecState)
	entry.OnlineWritebackState = rec.WritebackState

	onlineWindow, partial, err := r.onlineWindow(ctx, opts, rec)
	if err != nil {
		entry.CompareNote = "读取在线窗口结论失败：" + errcode.As(err).Details
		return
	}
	if onlineWindow == nil {
		entry.CompareNote = "在线结论没有留下窗口起止，无从比对"
		return
	}

	entry.OnlineWindow = onlineWindow
	entry.Compared = true
	entry.Diffs = compareWindows(entry.Window, *onlineWindow, partial)
	entry.Matched = len(entry.Diffs) == 0
	if partial {
		entry.CompareNote = "在线结论只留了窗口起止（未落窗口产物），其余字段无从比对"
	}
	if !entry.Matched {
		r.logger.Warn("replay_window_mismatch",
			"point_id", opts.PointID,
			"replay_run_id", entry.RunID,
			"online_run_id", entry.OnlineRunID,
			"diff_count", len(entry.Diffs))
	}
}

// onlineWindow 取在线判定当时的窗口结论。
//
// 优先取窗口步骤产物（字段最全）；取不到就退到 runs 表里的窗口起止
// （最窄判定路径也会落这两列），并标明这是残缺基准，只能比起止。
func (r *Replayer) onlineWindow(ctx context.Context, opts ReplayOptions, rec domain.RunRecord) (*domain.ExcursionWindow, bool, error) {
	value, ok, err := opts.Online.StepPayloadByRunKind(ctx, rec.Run.RunID, domain.PayloadWindow)
	if err != nil {
		return nil, false, err
	}
	if ok {
		payload, typed := value.(*domain.WindowPayload)
		if !typed {
			return nil, false, errcode.Newf(errcode.Validation,
				"在线窗口产物类型不符：%T", value)
		}
		window := payload.Window
		return &window, false, nil
	}
	if rec.Run.WindowStart != nil && rec.Run.WindowEnd != nil {
		return &domain.ExcursionWindow{
			Start: *rec.Run.WindowStart,
			End:   *rec.Run.WindowEnd,
		}, true, nil
	}
	return nil, false, nil
}

// compareWindows 逐字段比对两个越温窗口，返回差异列表（一致时为空）。
//
// partial 为真表示在线基准只有窗口起止，其余字段无从比对——
// 这时只比起止，不拿「在线没记」当成「回放错了」。
func compareWindows(replay, online domain.ExcursionWindow, partial bool) []FieldDiff {
	diffs := make([]FieldDiff, 0, 4)
	if !replay.Start.Equal(online.Start) {
		diffs = append(diffs, FieldDiff{
			Field:  "window_start",
			Replay: replay.Start.Format(time.RFC3339Nano),
			Online: online.Start.Format(time.RFC3339Nano),
		})
	}
	if !replay.End.Equal(online.End) {
		diffs = append(diffs, FieldDiff{
			Field:  "window_end",
			Replay: replay.End.Format(time.RFC3339Nano),
			Online: online.End.Format(time.RFC3339Nano),
		})
	}
	if partial {
		return diffs
	}
	if replay.PeakC != online.PeakC {
		diffs = append(diffs, FieldDiff{
			Field:  "peak_c",
			Replay: formatFloat(replay.PeakC),
			Online: formatFloat(online.PeakC),
		})
	}
	if !replay.PeakAt.Equal(online.PeakAt) {
		diffs = append(diffs, FieldDiff{
			Field:  "peak_at",
			Replay: replay.PeakAt.Format(time.RFC3339Nano),
			Online: online.PeakAt.Format(time.RFC3339Nano),
		})
	}
	if replay.DurationMinutes != online.DurationMinutes {
		diffs = append(diffs, FieldDiff{
			Field:  "duration_minutes",
			Replay: fmt.Sprintf("%d", replay.DurationMinutes),
			Online: fmt.Sprintf("%d", online.DurationMinutes),
		})
	}
	if replay.Zone != online.Zone {
		diffs = append(diffs, FieldDiff{
			Field:  "zone",
			Replay: replay.Zone,
			Online: online.Zone,
		})
	}
	if replay.SampleCount != online.SampleCount {
		diffs = append(diffs, FieldDiff{
			Field:  "sample_count",
			Replay: fmt.Sprintf("%d", replay.SampleCount),
			Online: fmt.Sprintf("%d", online.SampleCount),
		})
	}
	return diffs
}

// collectOnlineWithoutReplay 找出「在线判过、但这次回放重建不出来」的判定。
//
// 这类记录是复盘时最该看的一类：要么原始读数在库里缺了，
// 要么当初用的温区口径与本次重跑用的不是一份——
// 两种情况的处置完全不同，不能混在「都对得上」里看不见。
func (r *Replayer) collectOnlineWithoutReplay(ctx context.Context, opts ReplayOptions, result *ReplayResult) {
	if opts.Online == nil {
		return
	}
	records, err := opts.Online.RunsByPointInRange(ctx, opts.PointID, opts.From, opts.To)
	if err != nil {
		r.logger.Warn("replay_online_scan_failed", "error", err.Error())
		return
	}
	replayed := make(map[string]bool, len(result.Windows))
	for _, entry := range result.Windows {
		if entry.OnlineRunID != "" {
			replayed[entry.OnlineRunID] = true
		}
	}
	for _, rec := range records {
		if replayed[rec.Run.RunID] {
			continue
		}
		result.OnlineWithoutReplay = append(result.OnlineWithoutReplay, OnlineRunRef{
			RunID:          rec.Run.RunID,
			ExternalRef:    rec.Run.ExternalRef,
			IdempotencyKey: rec.Run.IdempotencyKey,
			State:          rec.Run.State,
			WritebackState: rec.WritebackState,
			WindowStart:    rec.Run.WindowStart,
			WindowEnd:      rec.Run.WindowEnd,
		})
	}
}

// formatFloat 把温度格式化到两位小数，保证差异报告里两边的写法一致。
func formatFloat(v float64) string {
	return fmt.Sprintf("%.2f", v)
}
