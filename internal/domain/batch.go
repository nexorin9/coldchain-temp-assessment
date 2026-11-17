// 本文件放「在库批次读侧」的形状：一次查询的结果，以及结果的状态。
//
// 为什么单独一个形状：批次清单的**缺失**和批次清单的**为空**是两种完全不同的事实，
// 而「批次清单查不到」这件事本身必须被记下来。
//
// 在库批次的权威来源是冷链台账（不是记录仪、也不是本服务）：
// 台账里有什么货，本服务就说什么；台账里查不到，本服务就写「批次信息缺失，待人工核对」。
// 绝不因为「表上总得有一栏」就编一个批次号出来——编出来的批次号在药事与
// 疫苗生产企业面前查无此物，比空着危险得多。
//
// 一条纪律贯穿本文件：**查询失败不是「没有批次」，而是「没查成」**。
// 两者在成稿里要写的话不一样：
//
//   - 没查成（台账不可达 / 凭据被拒 / 响应不合法）→ 批次信息缺失，待人工核对；
//   - 查成了但确实没有在库批次 → 该点位该时刻无在库批次，照实写「无在库批次」。
//
// 混成一句话，复核的人就分不清「这批货真的不在库里」与「系统没查到」。

package domain

import "time"

// BatchLookupStatus 是一次在库批次查询的结果状态。
type BatchLookupStatus string

const (
	// BatchLookupOK：台账给出了在库批次清单，且清单与本次判定的点位一致。
	BatchLookupOK BatchLookupStatus = "ok"
	// BatchLookupEmpty：台账可达，但该点位在该时刻没有在库批次。
	//
	// 这是「查到了，确实没有」，不是「没查成」——成稿里照实写「无在库批次」，
	// 不写「待人工核对」。
	BatchLookupEmpty BatchLookupStatus = "empty"
	// BatchLookupUnavailable：台账不可达（超时 / 连接失败 / 5xx / 凭据被拒 / 响应不合法）。
	//
	// 这是「没查成」：批次信息缺失，待人工核对。
	BatchLookupUnavailable BatchLookupStatus = "unavailable"
	// BatchLookupAmbiguous：台账返回的批次与本次判定的点位对不上。
	//
	// 按点位查出来的清单里混着别的点位的批次，说明台账侧的归属字段不可信，
	// 这时把哪一批算作「本次越温涉及的批次」都是在猜——落歧义，转人工核对。
	BatchLookupAmbiguous BatchLookupStatus = "ambiguous"
)

// BatchMissingText 是批次信息缺失时写进成稿的固定文案。
//
// 它是给复核的人看的：这一栏不是漏填，是明确告诉他「系统没拿到批次清单」，
// 需要人工去冷链台账核对后补上。留白会被读成「这一栏不用填」，
// 「待补」会被读成「谁忘了填」，而这句话说的是「这件事得有人去查」。
const BatchMissingText = "批次信息缺失，待人工核对"

// BatchSourceLedger 是在库批次读侧的来源标记。
//
// 它写进产物是为了让人能回答「这批清单是哪来的」：台账读的，
// 就以台账为准；本系统不生成批次号。
const BatchSourceLedger = "cold_chain_ledger"

// BatchLookup 是一次在库批次查询的事实记录。
//
// 它回答三个问题：查到了什么（Batches）、查成没有（Status）、
// 没查成的话为什么（Detail）。三样缺一不可——只有 Batches 的话，
// 「空清单」与「没查成」分不开。
type BatchLookup struct {
	// Status 是查询结果状态。
	Status BatchLookupStatus `json:"status"`
	// Batches 是台账给出的在库批次，原样引用，系统不改写、不生成新号。
	Batches []Batch `json:"batches,omitempty"`
	// Detail 是状态说明：查成没有、为什么没查成、歧义在哪个批次上。
	Detail string `json:"detail,omitempty"`
	// Source 是数据来源标记。
	Source string `json:"source,omitempty"`
	// CheckedAt 是本次查询的时刻。
	CheckedAt time.Time `json:"checked_at,omitempty"`
	// NeedsManual 表示这次查询的结果须人工接手。
	//
	// 台账不可达、批次归属有歧义时为 true：这两种情形下，
	// 成稿不能拿「无批次」当结论用，必须有人去核对。
	NeedsManual bool `json:"needs_manual,omitempty"`
}

// Usable 判断这份查询结果能不能直接当在库批次清单用。
//
// 只有「查成了、批次与点位对得上、且确实有批次」才算可用：
// 空清单虽然查成了，但它没有批次可用（成稿要照实说「无在库批次」，
// 而不是把空清单当成一份齐全的清单）。
func (l BatchLookup) Usable() bool {
	return l.Status == BatchLookupOK && len(l.Batches) > 0
}

// Missing 表示批次信息是否缺失到需要人工补齐。
//
// 台账不可达（没查成）与批次归属歧义都算：这两种情形下成稿不能装作拿到了清单。
func (l BatchLookup) Missing() bool {
	return l.Status == BatchLookupUnavailable || l.Status == BatchLookupAmbiguous
}

// NewBatchLookupOk 造一份「查到了」的批次查询结果。
func NewBatchLookupOk(batches []Batch, at time.Time) BatchLookup {
	status := BatchLookupOK
	detail := ""
	if len(batches) == 0 {
		status = BatchLookupEmpty
		detail = "冷链台账可达，但该点位在该时刻没有在库批次"
	}
	return BatchLookup{
		Status:    status,
		Batches:   batches,
		Detail:    detail,
		Source:    BatchSourceLedger,
		CheckedAt: at,
	}
}

// NewBatchLookupUnavailable 造一份「没查成」的批次查询结果。
//
// detail 是给人看的失败原因（超时 / 连接失败 / 5xx / 响应不合法），
// 必须写清：成稿上写「批次信息缺失」却不写为什么，复核的人还得自己去查日志。
func NewBatchLookupUnavailable(detail string, at time.Time) BatchLookup {
	return BatchLookup{
		Status:      BatchLookupUnavailable,
		Detail:      detail,
		Source:      BatchSourceLedger,
		CheckedAt:   at,
		NeedsManual: true,
	}
}

// NewBatchLookupAmbiguous 造一份「批次归属有歧义」的批次查询结果。
//
// batches 只保留与本次判定点位对得上的那些；归属对不上的批次不并入清单——
// 把别的点位的批次写进本次越温的异常记录表，比不写更糟。
func NewBatchLookupAmbiguous(batches []Batch, detail string, at time.Time) BatchLookup {
	return BatchLookup{
		Status:      BatchLookupAmbiguous,
		Batches:     batches,
		Detail:      detail,
		Source:      BatchSourceLedger,
		CheckedAt:   at,
		NeedsManual: true,
	}
}
