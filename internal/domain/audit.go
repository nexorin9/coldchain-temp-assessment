// 本文件是「审计链」相关的领域类型。
//
// 审计链回答的是飞检与内审最爱问的那句话：「这条判定当时用了模型吗？
// 用了哪个模型、花了多少 token、算下来多少钱、最后是谁确认的」。
//
// 链上的两类记录：
//   - 模型调用记录（model_call）：每次模型调用一行——模型名、耗时、输入 / 输出
//     token、估算成本、结果状态、是否走了降级；
//   - 人工确认记录（confirmation）：每次确认 / 否决一行——谁、什么时候、
//     从哪个状态改到哪个状态、理由是什么。
//
// 两类记录放在同一条链上（run 内序号递增），因为合起来才是一条完整的判定过程：
// 机器怎么判的、人怎么认的。拆成两张表，「把两次调用的记录和一次确认拼起来看」
// 这件事就得跨表拼，审计时反而更难核对。
//
// 三条约定贯穿本文件：
//   - 只追加不改写：审计记录落库后不改写；同一个序号的重放只更新那一行。
//   - 成本可算性显式：单价表里查不到这个模型，成本记 unknown 而不是 0——
//     0 是一个会被读成「这次调用不花钱」的数字，比「算不出来」危险得多。
//   - 不落敏感内容：凭据、鉴权头与原始提示词一律不进审计记录；
//     提示词只留摘要（指纹）与长度。
package domain

import "time"

// 审计条目类型。
const (
	// AuditEntryModelCall：一次模型调用。
	AuditEntryModelCall = "model_call"
	// AuditEntryConfirmation：一次人工确认动作。
	AuditEntryConfirmation = "confirmation"
)

// 模型调用的结果状态。
const (
	// AuditOutcomeSucceeded：调用成功且结论通过校验。
	AuditOutcomeSucceeded = "succeeded"
	// AuditOutcomeFailed：调用失败（超时、不可达、协议不合规、校验不过）。
	AuditOutcomeFailed = "failed"
	// AuditOutcomeDegraded：调用没成，最终结论退回了规则基线。
	AuditOutcomeDegraded = "degraded"
)

// 成本可算性。
const (
	// CostStatusKnown：单价表里有这个模型，成本算得出来。
	CostStatusKnown = "known"
	// CostStatusUnknown：单价表里没有这个模型（或没配单价表），成本算不出来。
	CostStatusUnknown = "unknown"
	// CostStatusPartial：一批记录里有一部分算得出来、一部分算不出来。
	//
	// 只在汇总口径下出现：单条记录只有「算得出」与「算不出」两种，
	// 汇总时把两者混成一个数会把「不知道」藏起来，所以另立一档。
	CostStatusPartial = "partial"
)

// AuditEntry 是审计链上的一条记录。
//
// 字段按「两类记录各自需要的部分」并置：模型调用那类填 token 与成本，
// 人工确认那类填 actor 与状态迁移，另一类的字段留空——
// 一张表两种形状比两张表好查，但空值必须真的空着，不用零值冒充。
type AuditEntry struct {
	// EntryID 是这条记录的标识（由落库方生成）。
	EntryID string `json:"entry_id,omitempty"`
	// RunID 是所属判定 run。
	RunID string `json:"run_id"`
	// Seq 是这条记录在 run 内审计链上的序号（从 1 起递增）。
	Seq int `json:"seq"`
	// EntryType 是记录类型（见 AuditEntry* 常量）。
	EntryType string `json:"entry_type"`

	// StepName 是所属判定步骤名（取数 / 抽窗口 / 归因 / 成稿 …）。
	StepName string `json:"step_name,omitempty"`
	// SchemaName 是这次调用的结构化输出分码（attribution / window / …）。
	SchemaName string `json:"schema_name,omitempty"`
	// Model 是端点回报的模型名；端点没回报时用配置里的模型名。
	Model string `json:"model,omitempty"`

	// Outcome 是调用结果状态（见 AuditOutcome* 常量）。
	Outcome string `json:"outcome,omitempty"`
	// Degraded 标记这次调用是否走了降级路径（模型不可达、结论退回了规则基线）。
	Degraded bool `json:"degraded"`
	// ErrorCode 是失败时的错误码。
	ErrorCode string `json:"error_code,omitempty"`

	// LatencyMS 是本次调用耗时（毫秒）。
	LatencyMS int64 `json:"latency_ms"`
	// PromptTokens 是端点回报的输入 token 数。
	PromptTokens int `json:"prompt_tokens"`
	// CompletionTokens 是端点回报的输出 token 数。
	CompletionTokens int `json:"completion_tokens"`
	// TotalTokens 是端点回报的总 token 数。
	TotalTokens int `json:"total_tokens"`

	// CostAmount 是估算成本；为 nil 表示算不出来（不是 0）。
	CostAmount *float64 `json:"cost_amount,omitempty"`
	// CostCurrency 是估算成本的币种。
	CostCurrency string `json:"cost_currency,omitempty"`
	// CostStatus 是成本可算性（见 CostStatus* 常量）。
	CostStatus string `json:"cost_status"`

	// Actor 是执行确认动作的人（经办 / 主管标识）。
	Actor string `json:"actor,omitempty"`
	// Action 是确认动作（confirm / reject / …）。
	Action string `json:"action,omitempty"`
	// Disposition 是确认动作定下的处置结论；否决时为空。
	//
	// 它单独成列而不塞进 Details：台账按处置结论统计处置分布，
	// 从一段中文说明里抠取值既不牢靠也不可查。
	Disposition string `json:"disposition,omitempty"`
	// Opinion 是主管的意见（可选）。
	//
	// 它回答的是「为什么这么定」：送企业评估、先隔离、暂缓——
	// 这些话事后没人记得，但飞检与内审第一个问的就是它。
	Opinion string `json:"opinion,omitempty"`
	// FromState 是确认前的业务状态。
	FromState string `json:"from_state,omitempty"`
	// ToState 是确认后的业务状态。
	ToState string `json:"to_state,omitempty"`

	// Details 是补充说明（已脱敏）。
	Details string `json:"details,omitempty"`
	// PromptDigest 是提示词的指纹（sha256 前 16 位）；提示词原文不落库。
	PromptDigest string `json:"prompt_digest,omitempty"`
	// PromptChars 是提示词字符数；用来核对「这次判定用的提示词有多长」。
	PromptChars int `json:"prompt_chars"`

	// CreatedAt 是记录时刻。
	CreatedAt time.Time `json:"created_at"`
}
