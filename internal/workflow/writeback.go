// 本文件落「出站写回」在业务状态机里的两条边与它们的原因码。
//
// 为什么写回要占状态机里的两个状态：台账写回有两种真实结局，
// 而这两种结局对现场的意义完全不同——
//   - **已写回台账**：台账里有这条越温、门诊也通知到了，事情做完了；
//   - **写回待补偿**：主管确认过了，但台账还没写进去（或通知还没发出去），
//     这条越温在账上还看不见。挂在这个状态上，是为了让「台账里少了一条」
//     这件事有人管，而不是悄悄消失。
//
// 两条边各带一个原因码：状态推进必须可归因，
// 「为什么这条判定到了已写回 / 待补偿」是飞检与内审第一个问的问题。
package workflow

import "time"

// 写回相关的原因码。
const (
	// ReasonLedgerWrittenBack：冷链台账的越温记录与接种门诊通知都已发出。
	ReasonLedgerWrittenBack = "ledger_written_back"
	// ReasonWritebackPending：写回没落成，挂起待补偿重放。
	ReasonWritebackPending = "writeback_pending_compensation"
)

// MarkWrittenBackRequest 组装一次「写回落成」的状态推进请求。
//
// 触发来源是 ActionWriteback：写回由系统发起，不由人发起。
func MarkWrittenBackRequest(at time.Time, note string) Request {
	return Request{
		To:     StateWrittenBack,
		Action: ActionWriteback,
		Reason: ReasonLedgerWrittenBack,
		Note:   note,
		Actor:  "system",
		At:     at,
	}
}

// MarkPendingWritebackRequest 组装一次「写回待补偿」的状态推进请求。
func MarkPendingWritebackRequest(at time.Time, note string) Request {
	return Request{
		To:     StatePendingWriteback,
		Action: ActionWriteback,
		Reason: ReasonWritebackPending,
		Note:   note,
		Actor:  "system",
		At:     at,
	}
}
