package domain

import "time"

// 本文件落「出站写回」在本地留下的痕迹。
//
// 为什么写回要按目标分开记账：一次写回要落两处——
// 冷链台账的一条越温偏差记录、接种门诊的一条通知。
// 两处的成败互不相干：台账写成了、通知没发出去，是很常见的一种状态
// （台账接口正常、通知系统正好在维护）。把它们记成一条「写回成功 / 失败」，
// 这两种真实情况就都表达不出来：
//   - 记成成功 → 通知永远不会补发，门诊不知道这批疫苗要隔离；
//   - 记成失败 → 补偿重放会把已经入账的台账再写一遍，台账里多出一条重复记录。
//
// 所以按目标分两行记：**台账写成了**与**通知发成了**各记各的。
// 补偿重放（task 40）据此只补没写成的那一处，不碰已经落成的那一处。
//
// 这一层也是「不重复入账」的本地凭据：台账侧按 external_ref / idempotency_key
// 认得出重复入账（权威判据），本地记录则让重放**连请求都不发**——
// 少发一次请求，就少一次把别人的台账写坏的机会。

// 写回目标取值。
const (
	// WritebackTargetLedger：写冷链台账的越温偏差记录。
	WritebackTargetLedger = "ledger"
	// WritebackTargetNotification：发接种门诊 / 药事的通知。
	WritebackTargetNotification = "notification"
)

// 写回目标自身的状态：这一处写没写成。
const (
	// WritebackTargetStatePending：还没写成，等着写。
	WritebackTargetStatePending = "pending"
	// WritebackTargetStateWritten：已写成，且已记下写入时刻。
	//
	// 它是「这一处不用再写了」的唯一凭据：补偿重放扫到这一行，
	// 就不会再对同一个目标发第二次请求。
	WritebackTargetStateWritten = "written"
	// WritebackTargetStateFailed：写失败（错误码与失败细节都留在这里）。
	//
	// 失败要留痕而不是静默丢弃：一条没写进台账的越温如果没人知道，
	// 药事与门诊那边就都以为一切正常。
	WritebackTargetStateFailed = "failed"
)

// WritebackRecord 是「一次写回的某一处目标」在本地留下的记录。
type WritebackRecord struct {
	// RunID 是被写回的判定 run。
	RunID string `json:"run_id"`
	// Target 是写回目标（ledger / notification）。
	Target string `json:"target"`
	// State 是这一处的状态（pending / written / failed）。
	State string `json:"state"`
	// ExternalRef 是这次越温的外部引用；台账按它识别同一次越温。
	ExternalRef string `json:"external_ref,omitempty"`
	// IdempotencyKey 是这次越温的幂等键；出站两侧都按它防重复。
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// ErrorCode 是失败时的错误码；成功时为空。
	ErrorCode string `json:"error_code,omitempty"`
	// ErrorDetail 是失败时的细节；成功时为空。
	ErrorDetail string `json:"error_detail,omitempty"`
	// WrittenAt 是这一处写成的时刻；还没写成时为空。
	WrittenAt *time.Time `json:"written_at,omitempty"`
	// CreatedAt 是这条记录第一次落库的时刻。
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt 是最近一次落库的时刻。
	UpdatedAt time.Time `json:"updated_at"`
}

// Written 判断这一处是否已经写成。
//
// 只有「写成」才算：pending 与 failed 都还要再写一次。
func (r WritebackRecord) Written() bool {
	return r.State == WritebackTargetStateWritten
}

// 写回告警的状态。
const (
	// WritebackAlertStateOpen：告警挂着，还没解决——
	// 这一处目标到现在还没写进台账 / 没通知到门诊。
	WritebackAlertStateOpen = "open"
	// WritebackAlertStateResolved：告警已解决（补偿重放补上了）。
	//
	// 它不删告警记录：飞检问「上周那次台账写不进去，后来怎么补上的」，
	// 答案就是这一条从 open 变成 resolved 的留痕。
	WritebackAlertStateResolved = "resolved"
)

// WritebackAlert 是一条「出站写回没落成」的告警记录。
//
// 为什么它与 WritebackRecord 分开成两张表：写回记录说的是
// 「这一处现在写没写成」，是当前状态；告警说的是
// 「曾经写不进去，现在还没人管」，是**要有人接手的事**。
// 两者混在一行里，补发成功后那行就变成「一切正常」，
// 「曾经差点漏掉一条越温」这件事就没人知道了。
type WritebackAlert struct {
	// RunID 是被写回的判定 run。
	RunID string `json:"run_id"`
	// Target 是写回目标（ledger / notification）。
	Target string `json:"target"`
	// State 是告警状态（open / resolved）。
	State string `json:"state"`
	// ErrorCode 是最近一次失败的错误码。
	ErrorCode string `json:"error_code,omitempty"`
	// ErrorDetail 是最近一次失败的细节。
	ErrorDetail string `json:"error_detail,omitempty"`
	// Attempts 是累计发起的出站调用次数（跨多轮补偿重放累计）。
	Attempts int `json:"attempts"`
	// Failures 是累计失败轮次（每一轮补偿重放算一轮）。
	Failures int `json:"failures"`
	// FirstFailedAt 是第一次失败的时刻。
	FirstFailedAt *time.Time `json:"first_failed_at,omitempty"`
	// LastFailedAt 是最近一次失败的时刻。
	LastFailedAt *time.Time `json:"last_failed_at,omitempty"`
	// ResolvedAt 是告警解决（补偿重放补上）的时刻；还挂着时为空。
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	// CreatedAt 是这条告警第一次落库的时刻。
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt 是最近一次落库的时刻。
	UpdatedAt time.Time `json:"updated_at"`
}

// Open 判断这条告警是否还挂着。
func (a WritebackAlert) Open() bool {
	return a.State == WritebackAlertStateOpen
}
