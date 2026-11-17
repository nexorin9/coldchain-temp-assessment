// Package runtime 是判定 run 的调度与执行层。
//
// 本文件落调度器的第一层：事件 → run 的按键入队与同窗去抖。
//
// 源产品机制对应（溯源见 spec.md「开源对照」与「参考地基」）：
// 入站事件经事件流进 Runner，Runner 按函数上配置的 flow control 把 run 入队，
// 同一个 key 的后续事件并入既有 run 而不是新建一条；队列层提供
// concurrency / throttling / prioritization / debouncing / rate limiting
// 这组流控手段。搬到冷链场景，对应物是：
//
//   - 入队键 = point_id + 越温窗口起点（窗口幂等键 sha256(point_id|window_start)）
//   - 去抖   = 同一点位的同一个越温窗口只判一次，后续采样并入既有 run
//   - 并发   = 同一 process 内按 point_id 串行化调度（按键限并发闸门在下一层落）
//
// 「一个越温窗口只判一次」是这套调度存在的全部理由。冷库夜间断电数小时，
// 记录仪会把断电期间的每一条采样都推上来，网关重试还会再推几遍。
// 若每条采样都建一条 run，药学部主管面前就是几十条同一场断电的判定记录，
// 台账里也会留下几十条越温；反过来，若把它们合并成一条，窗口的峰值、
// 持续时长与在库批次仍然能算对——这才是一条能拿出去用的结论。
package runtime

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
	"github.com/chaosforge/coldchain-temp-assessment/internal/rules"
)

// Outcome 是一次调度投递的结局。
//
// 分类不是为了好看：调用方要靠它区分「这条投递产生了一条新判定」
// 与「这条投递只是往已有判定上补了一个采样」。两种结局对台账的影响完全不同。
type Outcome string

// 调度结局取值。
const (
	// OutcomeCreated：新建了一条判定 run（此前没有同窗 run）。
	OutcomeCreated Outcome = "created"
	// OutcomeMerged：并入了既有 run 的窗口，采样数增加，run_id 不变。
	OutcomeMerged Outcome = "merged"
	// OutcomeDuplicate：命中已收过的同一条采样，样本数不增，也不新建 run。
	OutcomeDuplicate Outcome = "duplicate"
	// OutcomeClosed：采样回到温区内，当前开放窗口就此关闭，不新建 run。
	OutcomeClosed Outcome = "closed"
	// OutcomeNoWindow：采样在温区内且没有开放窗口，本就不需要判定。
	OutcomeNoWindow Outcome = "no_window"
)

const (
	// gapCycles 是「还算同一段越限」的最大采样周期数。
	//
	// 记录仪 5 分钟一个点，偶发丢一两个点很常见（网关抖动、电池换装）。
	// 缺 1～2 个点不该把一次断电拆成两次越温，所以容忍 3 个周期以内的缺口；
	// 超过 3 个周期说明中间那段数据根本不存在，跨过去连线会把两段越温
	// 算成一段「持续很久」的假窗口——那种窗口会直接误导批次处置。
	gapCycles = 3

	// defaultSampleIntervalSec 是点位未声明采样周期时使用的兜底间隔（秒）。
	//
	// 它只用于判断「两个采样是不是挨着」，不参与温区判定，也不参与窗口结论。
	// 温区绝不猜（缺配置一律转人工），采样间隔缺了只是把「挨着」的尺度
	// 取成现场通行的 5 分钟粒度，不影响越限与否的判断。
	defaultSampleIntervalSec = 300
)

// DispatchResult 是一次调度投递的结果。
type DispatchResult struct {
	// Run 是被推进的判定 run。新建时是新 run，并入时是既有 run（run_id 不变）。
	Run domain.Run `json:"run"`
	// Window 是该 run 当前挂着的越温窗口；未越限时为 nil。
	Window *domain.ExcursionWindow `json:"window,omitempty"`
	// Outcome 是本次投递的结局分类。
	Outcome Outcome `json:"outcome"`
	// SampleCount 是该窗口目前纳入的采样数。
	SampleCount int `json:"sample_count"`
	// Backfilled 表示本次并入的采样时刻早于窗口起点（迟到补投）。
	//
	// 它不改变窗口起点——起点一动窗口幂等键就变了，同一次越温会被拆成两条 run。
	// 但窗口起点可能因此晚于真实的越温起点，这个事实必须显式带出来，
	// 不能让人以为窗口起止就是全部现场。
	Backfilled bool `json:"backfilled,omitempty"`
	// EarliestSampleAt 是窗口内最早一条采样的时刻。
	//
	// 只有当它早于窗口起点（迟到补投）时才出现：这时窗口起点低估了
	// 越温的真实起点，出结论的人要能看到这个差额。
	EarliestSampleAt *time.Time `json:"earliest_sample_at,omitempty"`
	// NeedsManual 与 NeedsManualReason 在系统判不了时出现（温区配置缺失、点位对不上）。
	NeedsManual       bool   `json:"needs_manual,omitempty"`
	NeedsManualReason string `json:"needs_manual_reason,omitempty"`
}

// Scheduler 是判定 run 的调度器：把温度事件推进成判定 run，并把同窗事件并到同一条 run 上。
//
// 并发模型：按 point_id 串行化。
//
// 同一点位的调度必须串行，因为「这个采样属于已有窗口还是新开一个窗口」
// 是一次读-改-写：两个 goroutine 同时看到「没有开放窗口」，就会各建一条 run，
// 同一次越温被判两次。不同点位之间互不阻塞——一个冷库的门被反复打开，
// 不该把全院其它冷库的判定堵在后面。
// detectWindowStepName 是「检测越温窗口」这一步在服务日志里的步骤名。
//
// 调度器按点位与越温窗口建判定 run——它做的是判定链里「抽窗口」那一段
// 的事：认出这段越温从什么时候开始、到什么时候结束。用同一个步骤名，
// 服务日志里调度器记的这几条与执行链里 extract_window 那步能对上号。
const detectWindowStepName = "extract_window"

type Scheduler struct {
	// mu 保护下面的索引表。点位锁之外的共享状态只有这几个 map。
	mu sync.Mutex
	// runs 按窗口幂等键索引：键 → 判定 run。
	runs map[string]*runEntry
	// runsByID 按 run_id 索引，便于按 run 查窗口与样本。
	runsByID map[string]*runEntry
	// points 按 point_id 索引点位状态。
	points map[string]*pointState
	// logger 记录调度事件；为 nil 时回落到默认日志器（写 stderr）。
	logger *logging.Logger
	// runIDPrefix 是新建判定 run 的标识前缀（默认为空，即 run_ 前缀）。
	//
	// 离线回放用它把回放重建出的 run 与在线判定一眼分开：在线是 run_…，
	// 回放是 replay_…。两者用同一套幂等键与窗口规则，但标识绝不重名——
	// 混进在线判定记录里，飞检时就分不清哪条是当初判的、哪条是复算的。
	runIDPrefix string
}

// pointState 是一个点位在调度器里的状态。
//
// 锁在点位上而不是调度器上：不同点位要能同时调度，同一点位要严格串行。
type pointState struct {
	mu sync.Mutex
	// entries 是该点位已知的全部越温窗口，按创建顺序排列。
	//
	// 查找归属窗口时从最新的一条往回找：一条迟到的采样最可能属于
	// 最近开的那段越温，先查它省掉大部分比较。
	entries []*runEntry
}

// runEntry 是一条判定 run 在调度器里的运行态：run 本身 + 窗口的采样集合。
//
// 采样集合存在这里而不是 run 上，是因为 run 的形状要落库、要对外暴露，
// 而采样集合是调度过程的状态。任务 17 会把两者一起持久化。
type runEntry struct {
	run     domain.Run
	pointID string
	zone    string
	// startAt 是窗口起点，创建时钉住，之后不再变。
	//
	// 钉住的理由是窗口幂等键由它算出：起点一动，键就变，
	// 同一次越温会被拆成两条 run，台账里也会留下两条越温记录。
	// 迟到补投的采样可能早于起点——那条采样仍然收进窗口，
	// 但起点不动，Backfilled 标记会把这件事显式带出来。
	startAt time.Time
	// endAt 是窗口终点，随采样推进向后刷新。
	endAt time.Time
	// closedAt 是窗口关闭的时刻（采样回到温区内的那一刻）。
	// 零值表示窗口还开着。
	//
	// 它定的是「窗口管到哪一刻为止」：关闭之后到达的越限采样属于
	// 另一段越温，不能再并进这条窗口——中间那段温度已经回到温区内了，
	// 把两段越限连成一段，结论就成了假的。
	closedAt time.Time
	// samples 按采样时刻升序排列。
	samples []domain.TemperatureEvent
	// sampleKeys 是已收采样的事件幂等键集合，用于去重。
	sampleKeys map[string]bool
	peakC      float64
	peakAt     time.Time
}

// NewScheduler 创建调度器。logger 为 nil 时回落到默认日志器。
func NewScheduler(logger *logging.Logger) *Scheduler {
	if logger == nil {
		logger = logging.Default()
	}
	return &Scheduler{
		runs:     map[string]*runEntry{},
		runsByID: map[string]*runEntry{},
		points:   map[string]*pointState{},
		logger:   logger,
	}
}

// WithRunIDPrefix 设置新建判定 run 的标识前缀并返回自身，便于链式装配。
//
// 前缀会替换掉默认的 "run_" 段：回放用 "replay_" 之后，重建出的 run 标识
// 形如 replay_1f3c…，与在线判定的 run_1f3c… 一眼可分，且永不相撞。
// 窗口幂等键不受影响——它只由点位与窗口起点算出，与 run 标识无关。
func (s *Scheduler) WithRunIDPrefix(prefix string) *Scheduler {
	s.runIDPrefix = prefix
	return s
}

// Dispatch 把一条温度事件推进到判定 run。
//
// 三条路径：
//   - 采样越限且落在某条窗口的覆盖范围内 → 并入既有 run（run_id 不变，样本数增加）；
//   - 采样越限但不属于任何窗口 → 按窗口幂等键建 run；
//   - 采样在温区内 → 关闭该点位的开放窗口，不新建 run。
//
// 重复投递（同一点位同一采样时刻）既不新建 run 也不增加样本数：
// 网关重试、记录仪补传都会把同一条采样再送一遍，样本数跟着涨的话，
// 「这段温升持续了多久」会被算错。
//
// 温区配置缺失或点位对不上时返回 E_VALIDATION 并置 NeedsManual：
// 系统判不了就明说，绝不猜温区顶替出一个结论。
func (s *Scheduler) Dispatch(ctx context.Context, ev domain.TemperatureEvent, profile *domain.PointProfile) (DispatchResult, error) {
	if err := ctx.Err(); err != nil {
		return DispatchResult{}, err
	}

	zone, err := rules.ResolveZoneForEvent(profile, ev, nil)
	if err != nil {
		return DispatchResult{
			Outcome:           OutcomeNoWindow,
			NeedsManual:       true,
			NeedsManualReason: zone.Reason,
		}, err
	}

	pt := s.pointStateFor(ev.PointID)
	pt.mu.Lock()
	defer pt.mu.Unlock()

	tolerance := gapTolerance(profile)

	// 采样未越限：回到温区内意味着当前开放窗口到此结束。
	if !zone.OutOfRange(ev.TemperatureC) {
		closed := pt.closeWindows(ev.MeasuredAt)
		if closed == nil {
			return DispatchResult{Outcome: OutcomeNoWindow}, nil
		}
		s.logger.Info("run_window_closed",
			"step_name", detectWindowStepName,
			"point_id", ev.PointID,
			"run_id", closed.run.RunID,
			"window_start", closed.start().UTC().Format(time.RFC3339),
			"window_end", closed.end().UTC().Format(time.RFC3339),
			"sample_count", len(closed.samples))
		return s.resultFor(closed, OutcomeClosed, false), nil
	}

	// 采样越限：先看它落在哪条已有窗口里。
	if target := pt.findWindowFor(ev.MeasuredAt, tolerance); target != nil {
		backfilled := ev.MeasuredAt.Before(target.start())
		added := target.add(ev)
		target.refreshWindow()
		outcome := OutcomeMerged
		if !added {
			outcome = OutcomeDuplicate
		}
		s.logger.Info("run_dispatch_merged",
			"step_name", detectWindowStepName,
			"point_id", ev.PointID,
			"run_id", target.run.RunID,
			"window_start", target.start().UTC().Format(time.RFC3339),
			"sample_count", len(target.samples),
			"backfilled", backfilled,
			"duplicate", !added)
		return s.resultFor(target, outcome, backfilled), nil
	}

	// 不属于任何已有窗口：这是一段新的越温，建一条 run。
	entry := s.createRun(ev, zone.ZoneLabel)
	pt.entries = append(pt.entries, entry)
	s.logger.Info("run_created",
		"step_name", detectWindowStepName,
		"point_id", ev.PointID,
		"run_id", entry.run.RunID,
		"external_ref", entry.run.ExternalRef,
		"idempotency_key", entry.run.IdempotencyKey,
		"window_start", entry.start().UTC().Format(time.RFC3339),
		"peak_c", entry.peakC,
		"sample_count", len(entry.samples))
	return s.resultFor(entry, OutcomeCreated, false), nil
}

// DispatchSeries 按采样时刻顺序投递一整段采样。
//
// 顺序必须按时刻而不是按到达顺序：记录仪补投、网关乱序到达都会打乱投递顺序，
// 而窗口的起止与峰值只认时刻。返回值与投递顺序一致，调用方可逐条核对。
func (s *Scheduler) DispatchSeries(ctx context.Context, samples []domain.TemperatureEvent, profile *domain.PointProfile) ([]DispatchResult, error) {
	ordered := make([]domain.TemperatureEvent, len(samples))
	copy(ordered, samples)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].MeasuredAt.Before(ordered[j].MeasuredAt)
	})

	results := make([]DispatchResult, 0, len(ordered))
	for _, ev := range ordered {
		result, err := s.Dispatch(ctx, ev, profile)
		if err != nil {
			return results, err
		}
		results = append(results, result)
	}
	return results, nil
}

// RunCount 返回当前已建立的判定 run 数量。
//
// 这是「判定条数与投递条数解耦」的核对口径：投了多少条采样，
// 不该等于产生多少条判定。
func (s *Scheduler) RunCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}

// Runs 返回全部判定 run，按 run_id 排序，保证输出顺序确定。
func (s *Scheduler) Runs() []domain.Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.Run, 0, len(s.runs))
	for _, entry := range s.runs {
		out = append(out, entry.run)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunID < out[j].RunID })
	return out
}

// RunByKey 按窗口幂等键取判定 run。
func (s *Scheduler) RunByKey(key string) (domain.Run, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.runs[key]
	if !ok {
		return domain.Run{}, false
	}
	return entry.run, true
}

// SampleCount 返回某条 run 当前纳入的采样数。
func (s *Scheduler) SampleCount(runID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.runsByID[runID]
	if !ok {
		return 0
	}
	return len(entry.samples)
}

// WindowOf 返回某条 run 当前挂着的越温窗口。
func (s *Scheduler) WindowOf(runID string) (*domain.ExcursionWindow, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.runsByID[runID]
	if !ok {
		return nil, false
	}
	return entry.window(), true
}

// pointStateFor 取（必要时建）点位状态。
func (s *Scheduler) pointStateFor(pointID string) *pointState {
	s.mu.Lock()
	defer s.mu.Unlock()
	pt, ok := s.points[pointID]
	if !ok {
		pt = &pointState{}
		s.points[pointID] = pt
	}
	return pt
}

// createRun 为一条新窗口建判定 run 并登记到索引。
func (s *Scheduler) createRun(ev domain.TemperatureEvent, zoneLabel string) *runEntry {
	run := domain.NewRun(ev.PointID)
	run.RunID = s.stampRunID(run.RunID)
	entry := &runEntry{
		run:        run,
		pointID:    ev.PointID,
		zone:       zoneLabel,
		sampleKeys: map[string]bool{},
		startAt:    ev.MeasuredAt,
		endAt:      ev.MeasuredAt,
	}
	entry.add(ev)
	entry.refreshWindow()

	s.mu.Lock()
	s.runs[entry.run.IdempotencyKey] = entry
	s.runsByID[entry.run.RunID] = entry
	s.mu.Unlock()
	return entry
}

// stampRunID 给 run 标识打上调度器的前缀。
//
// 前缀替换默认的 "run_" 段而不是叠在前面：回放的标识因此是 replay_1f3c…，
// 而不是 replay_run_1f3c…——后者读起来像「另一种 run」，前者读起来就是
// 「这条 run 是回放出来的」。
func (s *Scheduler) stampRunID(runID string) string {
	if s.runIDPrefix == "" {
		return runID
	}
	return s.runIDPrefix + strings.TrimPrefix(runID, "run_")
}

// resultFor 组装一次投递的回执。
func (s *Scheduler) resultFor(entry *runEntry, outcome Outcome, backfilled bool) DispatchResult {
	result := DispatchResult{
		Run:         entry.run,
		Window:      entry.window(),
		Outcome:     outcome,
		SampleCount: len(entry.samples),
		Backfilled:  backfilled,
	}
	if len(entry.samples) > 0 {
		earliest := entry.earliestSample()
		if earliest.Before(entry.startAt) {
			result.EarliestSampleAt = &earliest
		}
	}
	return result
}

// findWindowFor 找出这条采样应归属的窗口。
//
// 判定条件两条，缺一不可：
//   - covers：采样时刻落在窗口跨度 ± 容差内（容忍偶发丢点，不跨大缺口连线）；
//   - admits：窗口还开着，或者采样发生在窗口关闭之前（否则是另一段越温）。
//
// 从最新一条往回找：迟到的采样最可能属于最近开的那段越温。
func (p *pointState) findWindowFor(t time.Time, tolerance time.Duration) *runEntry {
	for i := len(p.entries) - 1; i >= 0; i-- {
		w := p.entries[i]
		if w.covers(t, tolerance) && w.admits(t) {
			return w
		}
	}
	return nil
}

// closeWindows 关闭该点位在时刻 c 之前开启、且尚未关闭的窗口。
//
// 返回被关闭的最后一条窗口（没有则返回 nil）。
func (p *pointState) closeWindows(c time.Time) *runEntry {
	var last *runEntry
	for _, w := range p.entries {
		if !w.isOpen() {
			continue
		}
		// 起点在 c 之后的窗口不动：那是一条乱序到达的采样，
		// 它不能把一条还没开始的越温关上。
		if w.startAt.After(c) {
			continue
		}
		w.close(c)
		last = w
	}
	return last
}

// start 返回窗口起点（创建时钉住的那一时刻）。
func (e *runEntry) start() time.Time {
	return e.startAt
}

// end 返回窗口终点（最晚一条采样的时刻）。
func (e *runEntry) end() time.Time {
	return e.endAt
}

// earliestSample 返回窗口内最早一条采样的时刻。
//
// 它可能早于窗口起点——迟到补投时会出现这种情况。
// 窗口起点不动是为了保住窗口幂等键，但「这次越温实际可能更早就开始了」
// 这件事不能被钉死的起点吃掉，所以单独留一个口径。
func (e *runEntry) earliestSample() time.Time {
	return e.samples[0].MeasuredAt
}

// isOpen 判断窗口是否还开着。
func (e *runEntry) isOpen() bool {
	return e.closedAt.IsZero()
}

// close 在时刻 c 关闭窗口。
func (e *runEntry) close(c time.Time) {
	if e.closedAt.IsZero() {
		e.closedAt = c
	}
}

// admits 判断某个采样时刻是否还能并进这条窗口。
//
// 窗口开着就都能并；窗口关了就只认关闭时刻之前（含）的采样。
func (e *runEntry) admits(t time.Time) bool {
	if e.isOpen() {
		return true
	}
	return !t.After(e.closedAt)
}

// add 把一条采样并入窗口，返回 false 表示这条采样已收过（重复投递）。
//
// 去重按事件幂等键（point_id + 采样时刻）：同一条采样无论投递几次、
// 无论写成 +08:00 还是 UTC，都算同一条。
func (e *runEntry) add(ev domain.TemperatureEvent) bool {
	key := domain.EventIdempotencyKey(ev.PointID, ev.MeasuredAt)
	if e.sampleKeys[key] {
		return false
	}
	e.sampleKeys[key] = true
	e.samples = append(e.samples, ev)
	sort.SliceStable(e.samples, func(i, j int) bool {
		return e.samples[i].MeasuredAt.Before(e.samples[j].MeasuredAt)
	})
	if ev.MeasuredAt.After(e.endAt) {
		e.endAt = ev.MeasuredAt
	}
	return true
}

// refreshWindow 按当前采样集合刷新窗口终点与峰值。
//
// 窗口起点**不动**：起点是窗口幂等键的输入，动它等于给同一次越温换一把钥匙，
// 台账侧会把一次断电认成两次。迟到补投的采样早于起点时，只更新样本集合与峰值。
func (e *runEntry) refreshWindow() {
	if len(e.samples) == 0 {
		return
	}
	peakC := e.samples[0].TemperatureC
	peakAt := e.samples[0].MeasuredAt
	for _, s := range e.samples[1:] {
		// 同值时取最早出现的那一点，保证结论可复现。
		if s.TemperatureC > peakC {
			peakC = s.TemperatureC
			peakAt = s.MeasuredAt
		}
	}
	e.peakC = peakC
	e.peakAt = peakAt
	e.run.BindWindow(e.startAt, e.endAt)
}

// window 把当前采样集合还原成一条越温窗口。
func (e *runEntry) window() *domain.ExcursionWindow {
	if len(e.samples) == 0 {
		return nil
	}
	start := e.startAt
	end := e.endAt
	return &domain.ExcursionWindow{
		Start:           start,
		End:             end,
		DurationMinutes: int(end.Sub(start).Minutes()),
		PeakC:           e.peakC,
		PeakAt:          e.peakAt,
		Zone:            e.zone,
		SampleCount:     len(e.samples),
	}
}

// covers 判断某个采样时刻是否落在这条窗口的覆盖范围内。
//
// 覆盖范围 = [起点 - 容差, 终点 + 容差]。容差容忍偶发丢点，
// 但不许跨过大段数据缺口把两段越温连成一段。
func (e *runEntry) covers(t time.Time, tolerance time.Duration) bool {
	if len(e.samples) == 0 {
		return false
	}
	if t.Before(e.startAt.Add(-tolerance)) {
		return false
	}
	if t.After(e.endAt.Add(tolerance)) {
		return false
	}
	return true
}

// gapTolerance 算出「两个采样是否挨着」的容差。
func gapTolerance(profile *domain.PointProfile) time.Duration {
	interval := defaultSampleIntervalSec
	if profile != nil && profile.SampleIntervalSec > 0 {
		interval = profile.SampleIntervalSec
	}
	return time.Duration(interval) * time.Second * gapCycles
}
