// 本文件是「模型输出事实校验」相关的领域类型。
//
// 为什么要单独一组类型：模型输出的**形状**对不对，由结构化校验管
// （internal/model/validate.go）；模型输出的**数值与批次号**对不对，由事实校验管
// （internal/model/factcheck.go）。两件事的处置完全不同——形状不对是协议问题，
// 数值不对是模型在编，而「编出来的温度」是会写进药事台账、递到疫苗生产企业
// 面前的数字。
//
// 所以事实校验的每一条记录都必须能回答三件事：
//
//  1. 模型说了什么（被拦下的字段与原值）；
//  2. 依据是什么（输入时序、规则窗口、台账批次清单）；
//  3. 拦下来之后怎么办（重问一次，仍不符则退回规则基线，状态不推进）。
//
// 这些类型放在 domain 而不是 model 包：事实校验记录要落库，
// 落库那层（internal/store）不该反过来依赖模型包。
package domain

import "time"

// 事实校验的结论取值。
const (
	// FactVerdictPass：本次输出与输入事实相符。
	FactVerdictPass = "pass"
	// FactVerdictMismatch：本次输出里有与输入事实不符的内容（模型在编）。
	FactVerdictMismatch = "mismatch"
	// FactVerdictShapeInvalid：本次输出的形状没通过结构化校验。
	//
	// 它与「事实不符」分开记：形状不合规是模型没按 schema 答，
	// 事实不符是模型答了、但答的不是现场发生的事。
	FactVerdictShapeInvalid = "shape_invalid"
)

// 事实不符的类别。
const (
	// FactKindTemperatureNotFound：温度数值在输入时序里找不到。
	FactKindTemperatureNotFound = "temperature_not_found"
	// FactKindWindowOutOfRange：窗口起止落在规则抽出的窗口之外。
	FactKindWindowOutOfRange = "window_out_of_range"
	// FactKindDurationMismatch：持续时长与规则窗口对不上。
	FactKindDurationMismatch = "duration_mismatch"
	// FactKindBatchRefUnknown：批次号不在台账在库批次清单里。
	FactKindBatchRefUnknown = "batch_ref_unknown"
	// FactKindEventCountMismatch：设备事件条数与输入时序里数得出来的不一致。
	FactKindEventCountMismatch = "event_count_mismatch"
)

// FactIssue 是一处事实不符。
//
// Claimed 是模型给的值（保留原样，不改写）；Basis 是这次判定可依据的输入
// （时序里最接近的一条读数、规则窗口的起止、台账在库批次清单）；
// Reason 是人话版的判定理由，直接进错误详情与重问反馈。
type FactIssue struct {
	// Path 是不符字段的路径（如 peak_c、batches[0].batch_ref）。
	Path string `json:"path"`
	// Kind 是不符的类别（见 FactKind* 常量）。
	Kind string `json:"kind"`
	// Claimed 是模型给的值。
	Claimed string `json:"claimed"`
	// Basis 是核对用的输入依据。
	Basis string `json:"basis"`
	// Reason 是中文说明。
	Reason string `json:"reason"`
}

// FactCheckRecord 是事实校验的一条记录。
//
// 一次模型调用最多留下两条记录（首次 + 重问），每条一个结论：
// 通过、事实不符、形状不合规。记录挂在 run 与步骤上，
// 复核时能看出「这条结论问了几次、哪一次被拦下来、拦的是哪个字段」。
type FactCheckRecord struct {
	// RecordID 是这条记录的标识。
	RecordID string `json:"record_id"`
	// RunID 是所属判定 run。
	RunID string `json:"run_id"`
	// StepName 是所属步骤名（归因 / 成稿 …）。
	StepName string `json:"step_name"`
	// SchemaName 是这次调用的结构化输出分码。
	SchemaName string `json:"schema_name"`
	// Attempt 是第几次尝试（1 = 首次，2 = 重问）。
	Attempt int `json:"attempt"`
	// Verdict 是校验结论（见 FactVerdict* 常量）。
	Verdict string `json:"verdict"`
	// Issues 是被拦下的问题；通过时为空。
	Issues []FactIssue `json:"issues,omitempty"`
	// RawFragment 是模型返回原文的截断片段，供事后核对模型到底说了什么。
	RawFragment string `json:"raw_fragment,omitempty"`
	// CreatedAt 是记录时刻。
	CreatedAt time.Time `json:"created_at"`
}
