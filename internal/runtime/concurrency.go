package runtime

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
)

// DefaultConcurrencyLimit 是同一点位默认的并发额度。
//
// 额度是 1：同一个冷库点位同时只跑一条判定。原因很直接——同一台冷柜在
// 同一时刻只可能处在一段越温里，两条判定并行跑只会得到两份互相矛盾的结论，
// 而台账上最终只能留一条。多余的判定排队等着就行，排队的这段时间本身
// 也会被记下来（见 Lease.QueueWaitMs），出问题时能看出「这条判定是被
// 同点位的其它判定压住了」还是「模型本身慢」。
//
// 不同点位之间互不占用额度：3 号冷库的门被反复打开，不该把 5 号冷柜的
// 判定堵在后面。
const DefaultConcurrencyLimit = 1

// AdmissionState 是一次入闸回执的状态。
type AdmissionState string

const (
	// AdmissionAcquired：当场拿到了额度，可以开始执行。
	AdmissionAcquired AdmissionState = "acquired"
	// AdmissionQueued：额度已满，请求已排进该点位的等待队列，等前面的判定放行。
	AdmissionQueued AdmissionState = "queued"
)

// Lease 是一次并发额度的持有凭证。
//
// 凭证必须被释放：额度不还回去，同点位的后续判定会一直排队。Release
// 是幂等的——重复调用不会把额度多放出去一次（多放一次就等于限流形同虚设，
// 那比限流本身更糟）。
type Lease struct {
	// PointID 是被限流的点位。
	PointID string
	// RunID 是占用额度的判定 run。
	RunID string
	// AdmittedAt 是拿到额度的时刻。
	AdmittedAt time.Time
	// QueueWaitMs 是在队列里等到的毫秒数。
	//
	// 它对应源产品 TimingMetadata.queue_delay_ms 的口径：
	// 「由并发限额、限流或其它用户自定义并发约束造成的滞留时间」。
	// 现场看这个数能分清两种慢——一种是模型慢，一种是同点位排队慢。
	// 直接拿到额度（没排队）时为 0。
	QueueWaitMs int64

	gate *Gate
	once sync.Once
}

// Release 归还额度，并唤醒该点位队列里的队首。
func (l *Lease) Release() {
	if l == nil || l.gate == nil {
		return
	}
	l.once.Do(func() {
		g := l.gate
		g.mu.Lock()
		g.releaseLocked(l)
		g.mu.Unlock()
		g.logger.Info("run_concurrency_released",
			"point_id", l.PointID,
			"run_id", l.RunID,
			"queue_wait_ms", l.QueueWaitMs)
	})
}

// Waiter 是排在一个点位队列里的等待请求。
//
// 拿到排队回执后必须二选一：调 Await 等轮到自己，或调 Cancel 从队列里撤掉。
// 放着不管会占住队列位次——真拿到额度却没人释放，后面的判定就再也排不上了。
type Waiter struct {
	// PointID 是等待中的点位。
	PointID string
	// RunID 是等待中的判定 run。
	RunID string
	// Seq 是入队序号，出队严格按它排序（FIFO）。
	Seq uint64
	// QueuedAt 是入队时刻。
	QueuedAt time.Time

	gate      *Gate
	ch        chan *Lease
	lease     *Lease
	cancelled bool
}

// Await 等到额度轮到自己。
//
// 上下文取消时立刻返回错误，并把这条等待从队列里摘掉、把可能已经授予的额度还回去——
// 不能让一次取消把点位的额度吃掉。
func (w *Waiter) Await(ctx context.Context) (*Lease, error) {
	if w == nil {
		return nil, errcode.Newf(errcode.Validation, "等待对象为空")
	}
	select {
	case lease := <-w.ch:
		return lease, nil
	case <-ctx.Done():
		pos := w.Position()
		w.gate.cancelWaiter(w)
		return nil, fmt.Errorf("并发闸门排队被取消（点位 %s，原队列位次 %d）：%w", w.PointID, pos, ctx.Err())
	}
}

// Position 返回当前在队列里的位次（1 起）；已授予额度时返回 0。
func (w *Waiter) Position() int {
	if w == nil || w.gate == nil {
		return 0
	}
	w.gate.mu.Lock()
	defer w.gate.mu.Unlock()
	if w.lease != nil {
		return 0
	}
	for i, item := range w.gate.queues[w.PointID] {
		if item == w {
			return i + 1
		}
	}
	return 0
}

// Cancel 从队列里撤掉这条等待。
//
// 撤掉之后再来的请求不受影响；被授予额度但调用方放弃的情形会把额度原路还回。
func (w *Waiter) Cancel() {
	if w == nil || w.gate == nil {
		return
	}
	w.gate.cancelWaiter(w)
}

// Admission 是一次入闸回执。
type Admission struct {
	// State 是入闸状态：拿到额度（acquired）或排进队列（queued）。
	State AdmissionState `json:"state"`
	// Lease 在拿到额度时非空。
	Lease *Lease `json:"-"`
	// Waiter 在排队时非空，调用方据此 Await 或 Cancel。
	Waiter *Waiter `json:"-"`
	// QueuePosition 是排队位次（1 起）；拿到额度时为 0。
	QueuePosition int `json:"queue_position"`
	// Limit 是该点位的并发额度。
	Limit int `json:"limit"`
}

// PointGateStats 是一个点位的并发与排队口径。
type PointGateStats struct {
	PointID string `json:"point_id"`
	// Running 是该点位当前在跑的判定数。
	Running int `json:"running"`
	// Queued 是该点位当前排队的判定数。
	Queued int `json:"queued"`
	// QueueDelayMs 是队首已经等了多久（毫秒）。
	QueueDelayMs int64 `json:"queue_delay_ms"`
}

// RuntimeSummary 是运行摘要里并发与排队那部分的形状。
//
// 「供运行摘要端点读取」指的是这一层：摘要端点只要读 Gate.Summary()
// 就能报出全院有多少条判定在跑、多少条在排队、哪个点位压得最久，
// 不用自己去遍历 run 表。
type RuntimeSummary struct {
	// ConcurrencyLimit 是单点位的并发额度。
	ConcurrencyLimit int `json:"concurrency_limit"`
	// RunningRuns 是全部点位当前在跑的判定总数。
	RunningRuns int `json:"running_runs"`
	// QueuedRuns 是全部点位当前排队的判定总数。
	QueuedRuns int `json:"queued_runs"`
	// Points 是逐点位的明细，按 point_id 升序。
	Points []PointGateStats `json:"points"`
}

// Gate 是按 point_id 的并发闸门。
//
// 源产品机制对应（溯源见 spec.md「开源对照」与「参考地基」）：
// 源产品的队列层提供 concurrency / throttling / prioritization / debouncing
// 这组流控手段，其中按 key 的并发限额在额度用满时让请求进入队列等待，
// 并把这段滞留记成 queue delay（`TimingMetadata.queue_delay_ms`，
// 注释写明是「由并发限额、限流或其它用户自定义并发约束造成的滞留时间」）。
// 搬到冷链场景，对应物是：
//
//   - 限流键 = point_id（同一点位一条判定一个额度）
//   - 满额   = 同点位的第 2 条判定排队，不并发跑
//   - 排队   = 按 FIFO 出队，额度释放即唤醒队首
//   - 滞留   = 记成 QueueWaitMs，进运行摘要
type Gate struct {
	// mu 保护下面三张表。闸门的临界区很短（改计数、挪队首），不涉及 IO。
	mu sync.Mutex
	// limit 是单个点位的并发额度。
	limit int
	// running 是逐点位当前在跑的判定数。
	running map[string]int
	// queues 是逐点位的等待队列（FIFO）。
	queues map[string][]*Waiter
	// seq 是入队序号，保证出队顺序确定。
	seq uint64
	// logger 记录入闸与放行动作；为 nil 时回落到默认日志器（写 stderr）。
	logger *logging.Logger
	// now 取当前时刻；测试可注入固定时钟。
	now func() time.Time
}

// NewGate 创建并发闸门，额度取默认值 1。logger 为 nil 时回落到默认日志器。
func NewGate(logger *logging.Logger) *Gate {
	return NewGateWithLimit(DefaultConcurrencyLimit, logger)
}

// NewGateWithLimit 创建并发闸门并指定单点位额度。额度小于 1 时按 1 处理。
//
// 生产上用 1：同一台冷柜同一时刻只能有一段越温，两个判定并行只会互相打架。
func NewGateWithLimit(limit int, logger *logging.Logger) *Gate {
	if limit < 1 {
		limit = 1
	}
	if logger == nil {
		logger = logging.Default()
	}
	return &Gate{
		limit:   limit,
		running: map[string]int{},
		queues:  map[string][]*Waiter{},
		logger:  logger,
		now:     time.Now,
	}
}

// Limit 返回单点位的并发额度。
func (g *Gate) Limit() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.limit
}

// TryAcquire 立刻尝试拿到额度，拿不到就排进队列。
//
// 满额时返回 AdmissionQueued 与排队位次，**不**并发执行——调用方拿到的是
// 「排队状态」而不是「已经跑起来了」。队列位次与后续放行顺序都由 FIFO 决定。
//
// 排队回执里的 Waiter 必须被 Await 或 Cancel 收尾，否则它会一直占着队列位次。
func (g *Gate) TryAcquire(pointID, runID string) (*Admission, error) {
	if pointID == "" {
		return nil, errcode.Newf(errcode.Validation, "并发闸门需要点位标识：point_id 不能为空")
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.running[pointID] < g.limit {
		g.running[pointID]++
		now := g.now()
		lease := &Lease{
			PointID:    pointID,
			RunID:      runID,
			AdmittedAt: now,
			gate:       g,
		}
		g.logger.Info("run_concurrency_acquired",
			"point_id", pointID,
			"run_id", runID,
			"running", g.running[pointID],
			"queue_wait_ms", int64(0))
		return &Admission{State: AdmissionAcquired, Lease: lease, Limit: g.limit}, nil
	}

	w := g.enqueueLocked(pointID, runID)
	pos := len(g.queues[pointID])
	g.logger.Info("run_concurrency_queued",
		"point_id", pointID,
		"run_id", runID,
		"queue_position", pos,
		"running", g.running[pointID])
	return &Admission{State: AdmissionQueued, Waiter: w, QueuePosition: pos, Limit: g.limit}, nil
}

// Acquire 拿到额度，额度已满则排进队列等待轮到它。
//
// 等待严格按 FIFO：先到的先跑。ctx 取消时返回错误，这条等待会从队列里摘掉，
// 点位的额度不会被一次取消吃掉。
func (g *Gate) Acquire(ctx context.Context, pointID, runID string) (*Lease, error) {
	if pointID == "" {
		return nil, errcode.Newf(errcode.Validation, "并发闸门需要点位标识：point_id 不能为空")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("进入并发闸门前上下文已取消：%w", err)
	}
	g.mu.Lock()
	w := g.enqueueLocked(pointID, runID)
	// 有空额度就当场放行；没有就等着前面的判定还额度。
	g.grantLocked(pointID)
	g.mu.Unlock()
	return w.Await(ctx)
}

// Release 归还某个点位的额度。
//
// 这是给不持有 Lease 的调用方用的（例如按点位统一清理）；持有凭证的路径
// 应当直接调 Lease.Release()。
func (g *Gate) Release(pointID string) {
	g.mu.Lock()
	if g.running[pointID] > 0 {
		g.running[pointID]--
	}
	if g.running[pointID] <= 0 {
		delete(g.running, pointID)
	}
	g.grantLocked(pointID)
	g.mu.Unlock()
}

// RunningCount 返回某点位当前在跑的判定数。
//
// 这条数不该超过额度：超过就说明闸门漏了，同点位的判定会并发跑起来。
func (g *Gate) RunningCount(pointID string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.running[pointID]
}

// QueuedCount 返回某点位当前排队的判定数。
func (g *Gate) QueuedCount(pointID string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.queues[pointID])
}

// Summary 返回运行摘要需要的并发与排队口径。
//
// 逐点位明细按 point_id 升序，输出顺序确定，便于端点与测试逐字段核对。
func (g *Gate) Summary() RuntimeSummary {
	g.mu.Lock()
	defer g.mu.Unlock()

	summary := RuntimeSummary{
		ConcurrencyLimit: g.limit,
		Points:           []PointGateStats{},
	}

	perPoint := map[string]*PointGateStats{}
	for pointID, n := range g.running {
		perPoint[pointID] = &PointGateStats{PointID: pointID, Running: n}
		summary.RunningRuns += n
	}
	for pointID, queue := range g.queues {
		stat, ok := perPoint[pointID]
		if !ok {
			stat = &PointGateStats{PointID: pointID}
			perPoint[pointID] = stat
		}
		stat.Queued = len(queue)
		summary.QueuedRuns += len(queue)
		if len(queue) > 0 {
			// 队首等得最久，拿它当该点位当前的排队滞留。
			stat.QueueDelayMs = g.now().Sub(queue[0].QueuedAt).Milliseconds()
		}
	}

	for _, stat := range perPoint {
		summary.Points = append(summary.Points, *stat)
	}
	sort.Slice(summary.Points, func(i, j int) bool {
		return summary.Points[i].PointID < summary.Points[j].PointID
	})
	return summary
}

// enqueueLocked 把一条请求排进点位队列（调用方须持有 g.mu）。
func (g *Gate) enqueueLocked(pointID, runID string) *Waiter {
	g.seq++
	w := &Waiter{
		PointID:  pointID,
		RunID:    runID,
		Seq:      g.seq,
		QueuedAt: g.now(),
		gate:     g,
		ch:       make(chan *Lease, 1),
	}
	g.queues[pointID] = append(g.queues[pointID], w)
	return w
}

// grantLocked 把空闲额度交给该点位的队首（调用方须持有 g.mu）。
//
// 一次可能放行多条（额度大于 1 时）；队列里已取消的请求直接跳过。
// 授予与投递在同一个临界区内完成：等待方一定能在 w.ch 上拿到额度。
func (g *Gate) grantLocked(pointID string) {
	for g.running[pointID] < g.limit {
		queue := g.queues[pointID]
		if len(queue) == 0 {
			break
		}
		w := queue[0]
		g.queues[pointID] = queue[1:]
		if w.cancelled {
			continue
		}
		g.running[pointID]++
		now := g.now()
		lease := &Lease{
			PointID:     pointID,
			RunID:       w.RunID,
			AdmittedAt:  now,
			QueueWaitMs: now.Sub(w.QueuedAt).Milliseconds(),
			gate:        g,
		}
		w.lease = lease
		w.ch <- lease
		g.logger.Info("run_concurrency_admitted",
			"point_id", pointID,
			"run_id", w.RunID,
			"running", g.running[pointID],
			"queue_wait_ms", lease.QueueWaitMs)
	}
	if len(g.queues[pointID]) == 0 {
		delete(g.queues, pointID)
	}
}

// releaseLocked 归还额度并放行队首（调用方须持有 g.mu）。
func (g *Gate) releaseLocked(l *Lease) {
	if g.running[l.PointID] > 0 {
		g.running[l.PointID]--
	}
	if g.running[l.PointID] <= 0 {
		delete(g.running, l.PointID)
	}
	g.grantLocked(l.PointID)
}

// cancelWaiter 把一条等待从队列里摘掉。
//
// 已授予额度但调用方放弃时（上下文取消撞上放行），把额度原路还回去并唤醒下一位——
// 否则这次取消会让点位的额度凭空少一个，后续判定永远排不上。
func (g *Gate) cancelWaiter(w *Waiter) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if w.lease != nil {
		select {
		case lease := <-w.ch:
			g.releaseLocked(lease)
		default:
		}
		w.lease = nil
		return
	}

	queue := g.queues[w.PointID]
	for i, item := range queue {
		if item == w {
			g.queues[w.PointID] = append(queue[:i], queue[i+1:]...)
			w.cancelled = true
			break
		}
	}
	if len(g.queues[w.PointID]) == 0 {
		delete(g.queues, w.PointID)
	}
}
