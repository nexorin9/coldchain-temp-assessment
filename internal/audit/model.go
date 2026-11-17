// Package audit 落「模型调用审计」与「调用成本估算」。
//
// 这层存在的理由很具体：模型归因的结论要写进药事台账、递到疫苗生产企业面前，
// 内审与飞检会追问「这条结论是怎么来的、花了多少、谁认的」。
// 靠翻日志回答不了这个问题——日志是流水，审计链是账本。
//
// 三件事在这里各归其位：
//
//	单价表   —— 按模型名索引的每 token 单价，算得出成本就算，算不出就记 unknown；
//	脱敏     —— 凭据、鉴权头、原始提示词一律不进审计记录；
//	记录器   —— 把一次模型调用（或一次人工确认）组装成一条审计条目交给落库方。
//
// 「算不出就记 unknown，而不是记 0」是本包最重要的一条约定。
// 0 会被读成「这次调用不花钱」——那是结论，不是事实；事实是「本服务不知道」。
// 同源取舍：宁可少一条价目，也不让一条编出来的 0 混进台账。
//
// 本包不碰数据库、不碰 HTTP：落库由 internal/store 做，对外查询由
// internal/httpapi 做，两者都只依赖这里定义的形状。
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// DefaultCurrency 是单价表没声明币种时的默认币种。
const DefaultCurrency = "CNY"

// redactedPlaceholder 是敏感片段被抹掉后留下的占位符。
const redactedPlaceholder = "***"

// documentationPlaceholderKey 是价目表里的文档占位键。
//
// 上游价目快照用它演示「一个条目的全部字段长什么样」，代价全是 0——
// 它不是一个真的模型。装载时显式剔除：把占位条目当成免费模型，
// 会让一次真实调用的成本悄悄记成 0。
//
// 同源取舍见 internal/audit 包的说明：宁可少一条价目，
// 也不让一条编出来的 0 混进台账。
const documentationPlaceholderKey = "sample_spec"

// Price 是一个模型的每 token 单价。
//
// 单价按「每个 token」记，与上游价目快照的口径一致：
// 一次调用的成本 = 输入 token × 输入单价 + 输出 token × 输出单价。
type Price struct {
	// InputCostPerToken 是每个输入 token 的成本。
	InputCostPerToken float64 `json:"input_cost_per_token"`
	// OutputCostPerToken 是每个输出 token 的成本。
	OutputCostPerToken float64 `json:"output_cost_per_token"`
	// Currency 是币种；留空时取 DefaultCurrency。
	Currency string `json:"currency,omitempty"`
}

// PriceTable 是按模型名索引的每 token 单价表。
//
// 键是端点回报的模型名（端点没回报时用配置里的模型名）——
// 同一张表可以同时覆盖院内自建模型与合规云端模型。
type PriceTable struct {
	prices map[string]Price
}

// LoadPriceTable 从 JSON 文件装载单价表。
//
// 文件形状与上游价目快照的精简投影一致：
//
//	{
//	  "some-model": {"input_cost_per_token": 0.0000012, "output_cost_per_token": 0.0000024}
//	}
//
// 装载时做两件事：
//   - 剔除「输入输出单价都是 0」的条目：这类条目在上游快照里几乎总是没填的
//     占位行，不是一个真的免费模型。剔除它宁可让这次调用的成本记 unknown，
//     也不让一条编出来的 0 混进台账。
//   - 负单价直接报错：负价不是「便宜」，是这份表填错了。
//
// 单价表是可选配置：路径为空时调用方应当改用 UnknownPricer，
// 而不是造一张空表——空表与「没有表」在账上长得一样，但只有后者说得清。
func LoadPriceTable(path string) (*PriceTable, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, errcode.Newf(errcode.Validation, "模型单价表路径为空")
	}
	raw, err := os.ReadFile(trimmed)
	if err != nil {
		return nil, errcode.Newf(errcode.ConfigMissing,
			"读取模型单价表失败（%s）：%v", trimmed, err)
	}
	var entries map[string]Price
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, errcode.Newf(errcode.ConfigMissing,
			"解析模型单价表失败（%s）：%v", trimmed, err)
	}
	table := &PriceTable{prices: make(map[string]Price, len(entries))}
	for model, price := range entries {
		name := strings.TrimSpace(model)
		if name == "" || name == documentationPlaceholderKey {
			continue
		}
		if price.InputCostPerToken < 0 || price.OutputCostPerToken < 0 {
			return nil, errcode.Newf(errcode.ConfigMissing,
				"模型单价表里 %q 的每 token 单价为负数（输入 %v / 输出 %v），请核对后重填",
				name, price.InputCostPerToken, price.OutputCostPerToken)
		}
		if price.InputCostPerToken == 0 && price.OutputCostPerToken == 0 {
			continue
		}
		if strings.TrimSpace(price.Currency) == "" {
			price.Currency = DefaultCurrency
		}
		table.prices[name] = price
	}
	return table, nil
}

// NewPriceTable 直接用一张内存里的价目表装配（测试与内嵌默认价目用）。
func NewPriceTable(prices map[string]Price) *PriceTable {
	table := &PriceTable{prices: make(map[string]Price, len(prices))}
	for model, price := range prices {
		name := strings.TrimSpace(model)
		if name == "" || name == documentationPlaceholderKey {
			continue
		}
		if price.InputCostPerToken == 0 && price.OutputCostPerToken == 0 {
			continue
		}
		if strings.TrimSpace(price.Currency) == "" {
			price.Currency = DefaultCurrency
		}
		table.prices[name] = price
	}
	return table
}

// PriceFor 查一个模型的单价；查不到时返回 false。
func (t *PriceTable) PriceFor(model string) (Price, bool) {
	if t == nil || t.prices == nil {
		return Price{}, false
	}
	price, ok := t.prices[strings.TrimSpace(model)]
	return price, ok
}

// Len 返回表内有效条目数（被剔除的占位条目不计入）。
func (t *PriceTable) Len() int {
	if t == nil {
		return 0
	}
	return len(t.prices)
}

// Estimate 按本表给一次模型调用算成本；表里没有这个模型时成本记 unknown。
//
// 单价表自己就是计价器，不必再包一层——包一层只是为了给「计价器不是表」
// 的场景留个位置（比如按合约价单独算）。
func (t *PriceTable) Estimate(model string, promptTokens, completionTokens int) Cost {
	if t == nil || t.prices == nil {
		return Cost{Known: false}
	}
	price, ok := t.PriceFor(model)
	if !ok {
		return Cost{Known: false}
	}
	amount := float64(promptTokens)*price.InputCostPerToken +
		float64(completionTokens)*price.OutputCostPerToken
	return Cost{Amount: amount, Currency: price.Currency, Known: true}
}

// Cost 是一次模型调用的估算成本。
//
// Known 为 false 时 Amount 与 Currency 都没有意义——那不是「花了 0 元」，
// 而是「本服务算不出来」。
type Cost struct {
	// Amount 是估算成本金额。
	Amount float64 `json:"amount"`
	// Currency 是币种。
	Currency string `json:"currency"`
	// Known 标记这个成本是否算得出来。
	Known bool `json:"known"`
}

// Pricer 估算一次模型调用的成本。
type Pricer interface {
	// Estimate 按模型名与 token 数给出成本估算。
	Estimate(model string, promptTokens, completionTokens int) Cost
}

// TablePricer 按单价表估算成本。
type TablePricer struct {
	table *PriceTable
}

// NewTablePricer 装配一个按单价表估价的计价器。
func NewTablePricer(table *PriceTable) *TablePricer {
	return &TablePricer{table: table}
}

// Estimate 查单价表并算出成本；表里没有这个模型时成本记 unknown。
func (p *TablePricer) Estimate(model string, promptTokens, completionTokens int) Cost {
	if p == nil {
		return Cost{Known: false}
	}
	return p.table.Estimate(model, promptTokens, completionTokens)
}

// UnknownPricer 是没有配置单价表时用的计价器：所有调用的成本都记 unknown。
//
// 它不是「零成本」：单价表缺失时唯一诚实的说法是「本服务算不出来」。
type UnknownPricer struct{}

// Estimate 恒返回 unknown 成本。
func (UnknownPricer) Estimate(string, int, int) Cost { return Cost{Known: false} }

// 敏感片段的正则：鉴权头与 Bearer 凭据。
var (
	authorizationPattern = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)("[^"]*"|[^\s,;]+)`)
	bearerPattern        = regexp.MustCompile(`(?i)(bearer\s+)([A-Za-z0-9._~+/=-]+)`)
)

// Redact 从文本里抹掉敏感片段。
//
// 抹三样东西：调用方登记的凭据原文（api key、token）、鉴权头、Bearer 凭据。
// 审计记录会被内审与飞检翻出来给人看，也可能被导成 CSV 往外发——
// 里面出现一把能直接调模型的 key，等于把钥匙和锁一起寄出去。
//
// 脱敏只处理文本，不判断「这段文字该不该出现」：原文不进审计表这条规矩
// 由记录器保证（提示词只留指纹），脱敏是最后一道兜底。
//
// 顺序上先抹 Bearer 凭据、再抹鉴权头：反过来会漏。
// 「Authorization: Bearer <凭据>」里，鉴权头的取值部分只吃到第一个空格
// 为止（也就是只吃到 "Bearer"），凭据本身留在后头；
// 先抹鉴权头会把 "Bearer" 这个词抹掉，后面的凭据规则就再也匹配不到了。
func Redact(text string, secrets []string) string {
	if text == "" {
		return ""
	}
	out := text
	for _, secret := range secrets {
		s := strings.TrimSpace(secret)
		if s == "" {
			continue
		}
		out = strings.ReplaceAll(out, s, redactedPlaceholder)
	}
	out = bearerPattern.ReplaceAllString(out, "${1}"+redactedPlaceholder)
	out = authorizationPattern.ReplaceAllString(out, "${1}"+redactedPlaceholder)
	return out
}

// Digest 给出文本的稳定摘要（sha256 前 16 位十六进制）。
//
// 只留指纹不留原文：复核时要能回答「这两次判定用的是不是同一段提示词」，
// 但不该把提示词原文抄进审计表——提示词里往往带着现场点位与批次上下文。
func Digest(text string) string {
	if text == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:16]
}

// ModelCall 是一次模型调用的元数据。
//
// 这里的字段是「这次调用发生了什么」，不含任何结论内容——
// 结论归步骤产物管，审计链只管调用本身。
type ModelCall struct {
	// RunID 是所属判定 run。
	RunID string
	// StepName 是所属判定步骤名（归因 / 成稿 …）。
	StepName string
	// SchemaName 是这次调用的结构化输出分码。
	SchemaName string
	// Model 是端点回报的模型名；端点没回报时用配置里的模型名。
	Model string
	// Outcome 是调用结果状态；留空按成功处理。
	Outcome string
	// Degraded 标记这次调用是否走了降级路径。
	Degraded bool
	// ErrorCode 是失败时的错误码。
	ErrorCode string
	// LatencyMS 是本次调用耗时（毫秒）。
	LatencyMS int64
	// PromptTokens 是端点回报的输入 token 数。
	PromptTokens int
	// CompletionTokens 是端点回报的输出 token 数。
	CompletionTokens int
	// TotalTokens 是端点回报的总 token 数。
	TotalTokens int
	// Details 是补充说明（落库前脱敏）。
	Details string
	// PromptDigest 是这次调用的提示词指纹（sha256 前 16 位十六进制）。
	//
	// 提示词原文永不进审计记录：它带着现场点位、批次与读数上下文，
	// 而审计表会被导出成 CSV 往外发。指纹够用来回答
	// 「这两次判定用的是不是同一段提示词」，又不会把现场上下文抄出去。
	//
	// 调用方用 Digest() 算好再传进来——审计层从头到尾没见过原文。
	PromptDigest string
	// PromptChars 是提示词字符数；用来核对「这次判定用的提示词有多长」。
	PromptChars int
}

// Confirmation 是一次人工确认动作。
type Confirmation struct {
	// RunID 是所属判定 run。
	RunID string
	// Actor 是执行动作的人（经办 / 主管标识）。
	Actor string
	// Action 是动作名（confirm / reject / …）。
	Action string
	// FromState 是动作前的业务状态。
	FromState string
	// ToState 是动作后的业务状态。
	ToState string
	// Disposition 是确认动作定下的处置结论；否决时为空。
	Disposition string
	// Opinion 是主管的意见（可选，落库前脱敏）。
	Opinion string
	// Reason 是动作理由（落库前脱敏）。
	Reason string
}

// Sink 是审计条目的落库边界。
//
// 记录器只认这一个方法：它把组装好的条目交出去，落库怎么做（SQLite、别的库、
// 转发到日志系统）不关它的事。
type Sink interface {
	RecordAuditEntry(ctx context.Context, entry domain.AuditEntry) error
}

// AuditRecorder 是模型调用审计的落库出口。
//
// 业务层（模型网关、判定步骤链）只认这一个方法：
// 审计链怎么建、落在哪儿，不关它们的事。
type AuditRecorder interface {
	// RecordModelCall 落一条模型调用审计记录。
	RecordModelCall(ctx context.Context, call ModelCall) error
}

// Recorder 组装审计条目并写进 Sink。
type Recorder struct {
	sink    Sink
	pricer  Pricer
	secrets []string
	now     func() time.Time
}

// Option 是 Recorder 的可选装配项。
type Option func(*Recorder)

// WithPricer 指定计价器；不指定时成本一律记 unknown。
func WithPricer(p Pricer) Option {
	return func(r *Recorder) {
		if p != nil {
			r.pricer = p
		}
	}
}

// WithSecrets 登记需要在审计记录里抹掉的凭据原文。
func WithSecrets(secrets ...string) Option {
	return func(r *Recorder) {
		for _, s := range secrets {
			if trimmed := strings.TrimSpace(s); trimmed != "" {
				r.secrets = append(r.secrets, trimmed)
			}
		}
	}
}

// WithClock 注入时钟；测试可注入固定时钟。
func WithClock(now func() time.Time) Option {
	return func(r *Recorder) {
		if now != nil {
			r.now = now
		}
	}
}

// NewRecorder 装配审计记录器。
func NewRecorder(sink Sink, opts ...Option) *Recorder {
	r := &Recorder{
		sink:   sink,
		pricer: UnknownPricer{},
		now:    time.Now,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(r)
		}
	}
	return r
}

// RecordModelCall 记录一次模型调用。
//
// 记录里一定有：模型名、耗时、输入 / 输出 token、结果状态、成本可算性。
// 成本算不出来时 Amount 为空、状态为 unknown——不写 0。
//
// 提示词只留指纹与长度：原文不进审计表。
func (r *Recorder) RecordModelCall(ctx context.Context, call ModelCall) error {
	if r == nil || r.sink == nil {
		return errcode.Newf(errcode.Validation, "审计记录器未装配落库能力")
	}
	runID := strings.TrimSpace(call.RunID)
	if runID == "" {
		return errcode.Newf(errcode.Validation, "模型调用审计记录缺少 run_id，无法入库")
	}
	stepName := strings.TrimSpace(call.StepName)
	if stepName == "" {
		// 还没接进判定链的调用（没有步骤名）按结构化分码归位：
		// 「哪一步发起的」查不到，但「发的是哪种输出」查得到。
		stepName = strings.TrimSpace(call.SchemaName)
	}
	outcome := strings.TrimSpace(call.Outcome)
	if outcome == "" {
		outcome = domain.AuditOutcomeSucceeded
	}
	cost := r.estimate(call.Model, call.PromptTokens, call.CompletionTokens)

	entry := domain.AuditEntry{
		RunID:            runID,
		EntryType:        domain.AuditEntryModelCall,
		StepName:         stepName,
		SchemaName:       strings.TrimSpace(call.SchemaName),
		Model:            strings.TrimSpace(call.Model),
		Outcome:          outcome,
		Degraded:         call.Degraded,
		ErrorCode:        strings.TrimSpace(call.ErrorCode),
		LatencyMS:        call.LatencyMS,
		PromptTokens:     call.PromptTokens,
		CompletionTokens: call.CompletionTokens,
		TotalTokens:      call.TotalTokens,
		CostStatus:       domain.CostStatusUnknown,
		Details:          Redact(call.Details, r.secrets),
		PromptDigest:     strings.TrimSpace(call.PromptDigest),
		PromptChars:      call.PromptChars,
		CreatedAt:        r.now(),
	}
	// 端点只回报输入 / 输出 token 时，总数按两者补齐——
	// 总数为 0 会让「这次调用花了多少」在汇总里看着像没花钱。
	if entry.TotalTokens <= 0 {
		entry.TotalTokens = entry.PromptTokens + entry.CompletionTokens
	}
	if cost.Known {
		amount := cost.Amount
		entry.CostAmount = &amount
		entry.CostStatus = domain.CostStatusKnown
		entry.CostCurrency = cost.Currency
	}
	return r.sink.RecordAuditEntry(ctx, entry)
}

// RecordConfirmation 记录一次人工确认动作。
//
// 确认动作与模型调用落在同一条审计链上：内审要看的正是
// 「机器判了什么、人认了什么」这两段的接缝。
//
// 处置结论与意见各自成列，不塞进 Details：台账与报表要按处置结论做分布统计，
// 从一段中文说明里抠取值既不牢靠也不可查。意见同样脱敏后落库。
func (r *Recorder) RecordConfirmation(ctx context.Context, c Confirmation) error {
	if r == nil || r.sink == nil {
		return errcode.Newf(errcode.Validation, "审计记录器未装配落库能力")
	}
	runID := strings.TrimSpace(c.RunID)
	if runID == "" {
		return errcode.Newf(errcode.Validation, "确认动作审计记录缺少 run_id，无法入库")
	}
	return r.sink.RecordAuditEntry(ctx, domain.AuditEntry{
		RunID:       runID,
		EntryType:   domain.AuditEntryConfirmation,
		Actor:       strings.TrimSpace(c.Actor),
		Action:      strings.TrimSpace(c.Action),
		Disposition: strings.TrimSpace(c.Disposition),
		Opinion:     Redact(c.Opinion, r.secrets),
		FromState:   strings.TrimSpace(c.FromState),
		ToState:     strings.TrimSpace(c.ToState),
		Details:     Redact(c.Reason, r.secrets),
		CostStatus:  domain.CostStatusUnknown,
		CreatedAt:   r.now(),
	})
}

// estimate 走计价器算成本；没有计价器时成本记 unknown。
func (r *Recorder) estimate(model string, promptTokens, completionTokens int) Cost {
	if r.pricer == nil {
		return Cost{Known: false}
	}
	return r.pricer.Estimate(model, promptTokens, completionTokens)
}

// CallContext 是一次模型调用在判定链里的身份。
type CallContext struct {
	// RunID 是所属判定 run。
	RunID string
	// StepName 是发起调用的判定步骤名。
	StepName string
}

// callContextKey 是上下文键；用私有类型避免与别的包撞键。
type callContextKey struct{}

// WithCallContext 把「这次调用属于哪条 run、哪一步」放进上下文。
//
// 网关是唯一知道「发起了这次调用」的地方，但它不知道调用属于哪条判定——
// 那是调用方的上下文。用上下文传递，审计记录就不用靠全局变量追来追去。
func WithCallContext(ctx context.Context, runID, stepName string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, callContextKey{}, CallContext{
		RunID:    strings.TrimSpace(runID),
		StepName: strings.TrimSpace(stepName),
	})
}

// CallContextFrom 从上下文里取回调用身份；取不到返回零值。
func CallContextFrom(ctx context.Context) CallContext {
	if ctx == nil {
		return CallContext{}
	}
	if v, ok := ctx.Value(callContextKey{}).(CallContext); ok {
		return v
	}
	return CallContext{}
}
