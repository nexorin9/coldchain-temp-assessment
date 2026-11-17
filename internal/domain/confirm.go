package domain

import "time"

// 本文件落「人工确认」这件事的领域形状：谁在什么时候确认的、定了什么处置、
// 当时固化下来准备写回台账的内容是什么。
//
// 为什么单独一张形状而不是塞进 Run：确认是**一次动作**，run 是**一条判定**。
// 一条判定可能被确认一次、被否决一次、被补偿重放多次——把它们混在一张表里，
// 「这条判定最后一次是谁定的」就答不上来了。确认单独成行，
// 一次动作一行，飞检翻出来逐行看就是完整的处置过程。
//
// PayloadJSON 是这里最要紧的一栏：它是主管确认那一刻**固化**下来的
// 台账写回载荷原文。写回侧发出去的就是这份内容，不在写回时重新算——
// 重新算就等于台账里躺着的是另一份结论。

// 确认动作的类型。
const (
	// ConfirmationActionConfirm：确认处置结论。
	ConfirmationActionConfirm = "confirm"
	// ConfirmationActionReject：否决这条判定。
	ConfirmationActionReject = "reject"
)

// Confirmation 是一次人工确认 / 否决的记录。
type Confirmation struct {
	// RunID 是被确认 / 否决的判定 run。
	RunID string `json:"run_id"`
	// Action 是动作类型（confirm / reject）。
	Action string `json:"action"`
	// ConfirmedBy 是执行动作的人（药学部主管标识）。
	ConfirmedBy string `json:"confirmed_by"`
	// ConfirmedAt 是动作时刻。
	ConfirmedAt time.Time `json:"confirmed_at"`
	// Disposition 是最终处置结论；否决时为空。
	Disposition string `json:"disposition,omitempty"`
	// DispositionLabel 是处置结论的中文短标签；台账与报表直接引用它。
	DispositionLabel string `json:"disposition_label,omitempty"`
	// Opinion 是主管的意见（可选）。
	//
	// 它记的是「为什么这么定」：送企业评估、隔离、暂缓——
	// 这些话事后没人记得，但飞检与内审第一个问的就是它。
	Opinion string `json:"opinion,omitempty"`
	// PayloadJSON 是确认时固化的台账写回载荷原文（JSON）。
	//
	// 否决时为空：被否决的判定不写台账。
	PayloadJSON string `json:"payload_json,omitempty"`
	// CreatedAt 是这条记录的落库时刻。
	CreatedAt time.Time `json:"created_at"`
}
