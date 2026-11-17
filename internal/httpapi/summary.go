// 本文件落「运行摘要」端点：GET /v1/summary。
//
// 它回答的是运维站在服务前最常问的那几句：现在有事吗、哪一步慢、
// 还有多少条没进台账、模型端点是不是在掉链子。
//
// 它与 run 详情端点是同源的两份真相：详情回答「这一条判定怎么了」，
// 摘要回答「这些判定总体怎么样」。两者读同一份库，口径不会打架。
//
// 三条约定：
//   - 只读：摘要端点不推进任何业务状态，也不发任何出站请求。
//     要推进状态走确认端点，要补写回走补偿重放。
//   - 口径随响应给出：窗口起止、统计口径与覆盖情况都写在响应体里，
//     免得看的人对着一个数字猜它算的是哪一段。
//   - 取数失败就说失败：查不出来时返回错误码，不返回一份「全是 0」的摘要——
//     全 0 会被读成「今天什么事都没发生」，而真相是「没查出来」。
package httpapi

import (
	"context"
	"net/http"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/observe"
)

// SummaryReader 是运行摘要的取数能力边界。
//
// 只声明本包真正用到的那一个方法。取数实现（库取数、内存假数据）
// 都不关 HTTP 层的事——这一层只负责「把请求变成一份摘要、把结果变成响应」。
type SummaryReader interface {
	// Collect 汇总一次运行摘要。
	Collect(ctx context.Context) (observe.Summary, error)
}

// handleSummary 返回一份运行摘要。
//
// 没装配取数能力时按拒绝服务处理（503）：返回一个全 0 的摘要，
// 等于告诉运维「今天什么事都没发生」——而真相是「没人在统计」。
func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	if s.summaryReader == nil {
		writeError(w, errcode.Newf(errcode.StorageError,
			"运行摘要未装配取数能力，无法统计"))
		return
	}
	summary, err := s.summaryReader.Collect(r.Context())
	if err != nil {
		e := errcode.As(err)
		writeError(w, e)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}
