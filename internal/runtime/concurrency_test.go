package runtime

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestGate_SamePointSecondRunQueues：同一点位的第 2 条判定必须排队，不能并发跑。
//
// 同一台冷柜同一时刻只可能处在一段越温里，两条判定并行会得到两份互相矛盾的结论。
func TestGate_SamePointSecondRunQueues(t *testing.T) {
	g := NewGate(nil)

	first, err := g.TryAcquire("point-vax-freezer-01", "run-a")
	if err != nil {
		t.Fatalf("第一次入闸出错：%v", err)
	}
	if first.State != AdmissionAcquired {
		t.Fatalf("第一次入闸状态应为 acquired，实际是 %q", first.State)
	}

	second, err := g.TryAcquire("point-vax-freezer-01", "run-b")
	if err != nil {
		t.Fatalf("第二次入闸出错：%v", err)
	}
	if second.State != AdmissionQueued {
		t.Fatalf("第二次入闸状态应为 queued，实际是 %q", second.State)
	}
	if second.QueuePosition != 1 {
		t.Fatalf("第二次入闸的排队位次应为 1，实际是 %d", second.QueuePosition)
	}
	if second.Lease != nil {
		t.Fatalf("排队中的入闸不应带额度凭证")
	}
	if got := g.RunningCount("point-vax-freezer-01"); got != 1 {
		t.Fatalf("同点位在跑数应为 1，实际是 %d", got)
	}
	if got := g.QueuedCount("point-vax-freezer-01"); got != 1 {
		t.Fatalf("同点位排队数应为 1，实际是 %d", got)
	}

	second.Waiter.Cancel()
	first.Lease.Release()
}

// TestGate_DifferentPointsRunInParallel：不同点位互不阻塞，可以同时在跑。
func TestGate_DifferentPointsRunInParallel(t *testing.T) {
	g := NewGate(nil)

	a, err := g.TryAcquire("point-vax-freezer-01", "run-a")
	if err != nil {
		t.Fatalf("点位 A 入闸出错：%v", err)
	}
	b, err := g.TryAcquire("point-vax-fridge-02", "run-b")
	if err != nil {
		t.Fatalf("点位 B 入闸出错：%v", err)
	}
	if a.State != AdmissionAcquired || b.State != AdmissionAcquired {
		t.Fatalf("两个不同点位都应当场拿到额度，实际是 %q 与 %q", a.State, b.State)
	}
	if got := g.RunningCount("point-vax-freezer-01"); got != 1 {
		t.Fatalf("点位 A 在跑数应为 1，实际是 %d", got)
	}
	if got := g.RunningCount("point-vax-fridge-02"); got != 1 {
		t.Fatalf("点位 B 在跑数应为 1，实际是 %d", got)
	}
	if got := g.QueuedCount("point-vax-freezer-01"); got != 0 {
		t.Fatalf("不同点位不应产生排队，实际排队数 %d", got)
	}

	a.Lease.Release()
	b.Lease.Release()
}

// TestGate_ReleaseWakesQueueHeadInFIFOOrder：额度释放后按 FIFO 出队唤醒。
func TestGate_ReleaseWakesQueueHeadInFIFOOrder(t *testing.T) {
	g := NewGate(nil)

	held, err := g.TryAcquire("point-vax-freezer-01", "run-held")
	if err != nil {
		t.Fatalf("占位入闸出错：%v", err)
	}

	second, err := g.TryAcquire("point-vax-freezer-01", "run-second")
	if err != nil {
		t.Fatalf("第二条入闸出错：%v", err)
	}
	third, err := g.TryAcquire("point-vax-freezer-01", "run-third")
	if err != nil {
		t.Fatalf("第三条入闸出错：%v", err)
	}
	if second.QueuePosition != 1 || third.QueuePosition != 2 {
		t.Fatalf("排队位次应为 1 与 2，实际是 %d 与 %d", second.QueuePosition, third.QueuePosition)
	}

	held.Lease.Release()

	lease2, err := second.Waiter.Await(context.Background())
	if err != nil {
		t.Fatalf("队首等待出错：%v", err)
	}
	if lease2.RunID != "run-second" {
		t.Fatalf("队首应先拿到 run-second，实际是 %s", lease2.RunID)
	}
	if got := g.RunningCount("point-vax-freezer-01"); got != 1 {
		t.Fatalf("放行队首后在跑数应仍为 1，实际是 %d", got)
	}
	if got := third.Waiter.Position(); got != 1 {
		t.Fatalf("第三条的位次应前移为 1，实际是 %d", got)
	}

	lease2.Release()
	lease3, err := third.Waiter.Await(context.Background())
	if err != nil {
		t.Fatalf("第三条等待出错：%v", err)
	}
	if lease3.RunID != "run-third" {
		t.Fatalf("第二条放行后应轮到 run-third，实际是 %s", lease3.RunID)
	}
	lease3.Release()

	if got := g.RunningCount("point-vax-freezer-01"); got != 0 {
		t.Fatalf("全部释放后在跑数应为 0，实际是 %d", got)
	}
	if got := g.QueuedCount("point-vax-freezer-01"); got != 0 {
		t.Fatalf("全部放行后排队数应为 0，实际是 %d", got)
	}
}

// TestGate_AcquireBlocksUntilLeaseReleased：Acquire 在额度满时阻塞等待。
func TestGate_AcquireBlocksUntilLeaseReleased(t *testing.T) {
	g := NewGate(nil)

	held, err := g.TryAcquire("point-vax-freezer-01", "run-held")
	if err != nil {
		t.Fatalf("占位入闸出错：%v", err)
	}

	type result struct {
		lease *Lease
		err   error
	}
	done := make(chan result, 1)
	go func() {
		lease, err := g.Acquire(context.Background(), "point-vax-freezer-01", "run-waiting")
		done <- result{lease: lease, err: err}
	}()

	select {
	case r := <-done:
		t.Fatalf("额度未释放时不应拿到额度：%v", r.err)
	case <-time.After(50 * time.Millisecond):
	}

	held.Lease.Release()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("等待方应拿到额度，实际出错：%v", r.err)
		}
		if r.lease.RunID != "run-waiting" {
			t.Fatalf("等待方拿到的 run 不对：%s", r.lease.RunID)
		}
		r.lease.Release()
	case <-time.After(2 * time.Second):
		t.Fatalf("额度释放后等待方未被唤醒")
	}
}

// TestGate_AcquireContextCancelRemovesWaiter：等待被取消时不得吃掉点位额度。
func TestGate_AcquireContextCancelRemovesWaiter(t *testing.T) {
	g := NewGate(nil)

	held, err := g.TryAcquire("point-vax-freezer-01", "run-held")
	if err != nil {
		t.Fatalf("占位入闸出错：%v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := g.Acquire(ctx, "point-vax-freezer-01", "run-cancelled")
		errCh <- err
	}()

	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("取消后应返回错误")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("取消后等待方应立即返回")
	}

	if got := g.QueuedCount("point-vax-freezer-01"); got != 0 {
		t.Fatalf("取消后排队数应回到 0，实际是 %d", got)
	}

	// 额度必须完好：放行原持有者后，下一位应能立刻拿到额度。
	held.Lease.Release()
	lease, err := g.Acquire(context.Background(), "point-vax-freezer-01", "run-next")
	if err != nil {
		t.Fatalf("取消不应影响后续入闸：%v", err)
	}
	if lease.RunID != "run-next" {
		t.Fatalf("后续拿到的 run 不对：%s", lease.RunID)
	}
	lease.Release()
}

// TestGate_LeaseReleaseIsIdempotent：重复释放额度不会多放一次。
func TestGate_LeaseReleaseIsIdempotent(t *testing.T) {
	g := NewGate(nil)

	lease, err := g.TryAcquire("point-vax-freezer-01", "run-a")
	if err != nil {
		t.Fatalf("入闸出错：%v", err)
	}
	lease.Lease.Release()
	lease.Lease.Release()
	lease.Lease.Release()

	if got := g.RunningCount("point-vax-freezer-01"); got != 0 {
		t.Fatalf("重复释放后在跑数应为 0，实际是 %d", got)
	}

	// 多放一次额度会让限流形同虚设：并发拿到的额度必须仍然只有一个。
	a, err := g.TryAcquire("point-vax-freezer-01", "run-a2")
	if err != nil {
		t.Fatalf("入闸出错：%v", err)
	}
	b, err := g.TryAcquire("point-vax-freezer-01", "run-b2")
	if err != nil {
		t.Fatalf("入闸出错：%v", err)
	}
	if a.State != AdmissionAcquired || b.State != AdmissionQueued {
		t.Fatalf("额度只应放出一个，实际是 %q 与 %q", a.State, b.State)
	}
	b.Waiter.Cancel()
	a.Lease.Release()
}

// TestGate_SummaryExposesRunningAndQueued：运行摘要能读到在跑数与排队数。
func TestGate_SummaryExposesRunningAndQueued(t *testing.T) {
	g := NewGate(nil)

	a, err := g.TryAcquire("point-vax-freezer-01", "run-a")
	if err != nil {
		t.Fatalf("点位 A 入闸出错：%v", err)
	}
	b, err := g.TryAcquire("point-vax-fridge-02", "run-b")
	if err != nil {
		t.Fatalf("点位 B 入闸出错：%v", err)
	}
	queued, err := g.TryAcquire("point-vax-freezer-01", "run-c")
	if err != nil {
		t.Fatalf("排队入闸出错：%v", err)
	}
	if queued.State != AdmissionQueued {
		t.Fatalf("第三条应排队，实际是 %q", queued.State)
	}

	summary := g.Summary()
	if summary.ConcurrencyLimit != DefaultConcurrencyLimit {
		t.Fatalf("并发额度应为 %d，实际是 %d", DefaultConcurrencyLimit, summary.ConcurrencyLimit)
	}
	if summary.RunningRuns != 2 {
		t.Fatalf("在跑总数应为 2，实际是 %d", summary.RunningRuns)
	}
	if summary.QueuedRuns != 1 {
		t.Fatalf("排队总数应为 1，实际是 %d", summary.QueuedRuns)
	}
	if len(summary.Points) != 2 {
		t.Fatalf("逐点位明细应有 2 条，实际是 %d", len(summary.Points))
	}
	if summary.Points[0].PointID != "point-vax-freezer-01" || summary.Points[1].PointID != "point-vax-fridge-02" {
		t.Fatalf("逐点位明细应按 point_id 升序，实际是 %s 与 %s",
			summary.Points[0].PointID, summary.Points[1].PointID)
	}
	if summary.Points[0].Running != 1 || summary.Points[0].Queued != 1 {
		t.Fatalf("点位 01 的在跑数与排队数应为 1 与 1，实际是 %d 与 %d",
			summary.Points[0].Running, summary.Points[0].Queued)
	}

	queued.Waiter.Cancel()
	a.Lease.Release()
	b.Lease.Release()
}

// TestGate_QueueWaitMsRecordsQueueDelay：排队滞留时间被记下来（对应源产品的 queue_delay_ms）。
func TestGate_QueueWaitMsRecordsQueueDelay(t *testing.T) {
	g := NewGate(nil)

	held, err := g.TryAcquire("point-vax-freezer-01", "run-held")
	if err != nil {
		t.Fatalf("占位入闸出错：%v", err)
	}
	waiting, err := g.TryAcquire("point-vax-freezer-01", "run-waiting")
	if err != nil {
		t.Fatalf("排队入闸出错：%v", err)
	}

	time.Sleep(30 * time.Millisecond)
	held.Lease.Release()

	lease, err := waiting.Waiter.Await(context.Background())
	if err != nil {
		t.Fatalf("等待方出错：%v", err)
	}
	if lease.QueueWaitMs <= 0 {
		t.Fatalf("排队滞留应大于 0 毫秒，实际是 %d", lease.QueueWaitMs)
	}
	lease.Release()
}

// TestGate_ConcurrentAcquireSamePointNeverExceedsLimit：并发涌入时同点位在跑数不超过额度。
func TestGate_ConcurrentAcquireSamePointNeverExceedsLimit(t *testing.T) {
	g := NewGate(nil)

	const workers = 32
	var running int64
	var maxRunning int64
	var wg sync.WaitGroup
	errCh := make(chan error, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			lease, err := g.Acquire(context.Background(), "point-vax-freezer-01", fmt.Sprintf("run-%02d", idx))
			if err != nil {
				errCh <- err
				return
			}
			cur := atomic.AddInt64(&running, 1)
			for {
				old := atomic.LoadInt64(&maxRunning)
				if cur <= old || atomic.CompareAndSwapInt64(&maxRunning, old, cur) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			atomic.AddInt64(&running, -1)
			lease.Release()
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("并发入闸出错：%v", err)
	}
	if maxRunning != 1 {
		t.Fatalf("同点位并发在跑数不应超过 1，实际峰值是 %d", maxRunning)
	}
	if got := g.RunningCount("point-vax-freezer-01"); got != 0 {
		t.Fatalf("全部释放后在跑数应为 0，实际是 %d", got)
	}
	if got := g.QueuedCount("point-vax-freezer-01"); got != 0 {
		t.Fatalf("全部放行后排队数应为 0，实际是 %d", got)
	}
}

// TestGate_TryAcquireQueuedWaiterCanCancel：排队回执可以撤掉，不占位次。
func TestGate_TryAcquireQueuedWaiterCanCancel(t *testing.T) {
	g := NewGate(nil)

	held, err := g.TryAcquire("point-vax-freezer-01", "run-held")
	if err != nil {
		t.Fatalf("占位入闸出错：%v", err)
	}
	queued, err := g.TryAcquire("point-vax-freezer-01", "run-queued")
	if err != nil {
		t.Fatalf("排队入闸出错：%v", err)
	}
	if queued.Waiter.Position() != 1 {
		t.Fatalf("排队位次应为 1，实际是 %d", queued.Waiter.Position())
	}

	queued.Waiter.Cancel()
	if got := g.QueuedCount("point-vax-freezer-01"); got != 0 {
		t.Fatalf("撤销后排队数应为 0，实际是 %d", got)
	}

	held.Lease.Release()
}

// TestGate_EmptyPointIDRejected：点位标识为空时明确报错，不静默放行。
func TestGate_EmptyPointIDRejected(t *testing.T) {
	g := NewGate(nil)

	if _, err := g.TryAcquire("", "run-a"); err == nil {
		t.Fatalf("空 point_id 应报错")
	}
	if _, err := g.Acquire(context.Background(), "", "run-a"); err == nil {
		t.Fatalf("空 point_id 应报错")
	}
}
