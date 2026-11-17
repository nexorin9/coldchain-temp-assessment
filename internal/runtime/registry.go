// 本文件落「判定结果形状唯一性」：一张结果类型注册表，加一道启动即失败的校验。
//
// 源产品机制对应（溯源见 spec.md「开源对照」与「参考地基」）：
// 源产品的类型收集器扫描各包中带注解的类型，收集完先做重名检查，
// 一旦同名就报「duplicate type %q: found in both %s and %s」并整体失败，
// 绝不生成半份产物
// （github_ref/inngest/cmd/tygo-collect/main.go：collectAnnotatedTypes →
// checkDuplicates，重复即 return error，main 收到错误即 exit(1)）；
// 每类元数据另有自己的 kind 常量与独立结构，kind 名即唯一标识
// （github_ref/inngest/ui/packages/components/src/generated/index.ts：
// KindInngestAI / KindInngestAISummary 各带独立 const 与独立 interface）。
// 搬到冷链场景，对应物是：
//
//   - 判定结果类型 = 采样 / 越限窗口 / 归因结论 / 异常记录表 / 待确认标记 / 模型原始输出
//   - 结果类型名唯一 = 一个结果类型名只能登记一种形状；重复登记即报冲突并点名
//   - 启动即失败 = serve 启动时校验内置步骤链的结果形状，有冲突就退出，
//     不带着一份形状对不上的判定结论去写台账
//
// 为什么值得单独做一层：
// 判定结论最终要写进冷链台账、要递到疫苗生产企业。台账与企业的解析器
// 按「结果类型名 + 字段」取数据。同一个名字下出现两种形状（改了字段却只改了一处、
// 忘了另一处），解析器往往不会报错，而是安静地读出半个结论——那比报个错坏得多。
// 所以形状冲突要在启动时就被拦下，而不是等某天夜里台账里多了一条错记录。
package runtime

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// ResultShape 是一种判定结果形状的登记条目。
//
// 一条登记说清三件事：这个结果类型叫什么（名字唯一）、它对应哪个产物分码、
// 以及它的载荷怎么构造。第三件是校验用的：注册表与 domain 侧的分码映射
// 必须对得上，否则「归因结论」名下会挂着一份采样形状的载荷。
type ResultShape struct {
	// Name 是结果类型名，注册表里的唯一键。
	Name string
	// Kind 是该形状对应的步骤产物分码。
	Kind domain.StepPayloadKind
	// New 构造该形状的空载荷；类型必须与分码在 domain 侧登记的一致。
	New func() any
	// Source 是登记来源（步骤名或文件）。冲突时用它指认「谁登记的」——
	// 只说「重复了」等于让人自己猜。
	Source string
}

// ResultShapeRegistry 是判定结果类型的注册表。
//
// 并发安全：注册表在服务启动时被装配，之后只读；
// 但装配期与运行期都可能并发访问（例如装配时校验、运行时查形状），
// 所以内部加锁，而不是靠调用方自己保证「先装配后使用」。
type ResultShapeRegistry struct {
	mu     sync.RWMutex
	shapes map[string]ResultShape
	// kinds 是分码到结果类型名的反查表：同一个分码不得挂在两个名字下。
	kinds map[domain.StepPayloadKind]string
	order []string
}

// NewResultShapeRegistry 造一张空注册表。
func NewResultShapeRegistry() *ResultShapeRegistry {
	return &ResultShapeRegistry{
		shapes: map[string]ResultShape{},
		kinds:  map[domain.StepPayloadKind]string{},
	}
}

// Register 登记一个结果类型。
//
// 两类冲突都直接报错，不静默覆盖：
//   - 同名重复登记：同一个结果类型名只能有一种形状；
//   - 同一分码挂到两个名字下：一个分码只能有一种形状。
//
// 冲突时返回 E_CONFLICT，并在错误详情里点名冲突的结果类型与两处登记来源。
func (r *ResultShapeRegistry) Register(shape ResultShape) error {
	if strings.TrimSpace(shape.Name) == "" {
		return errcode.Newf(errcode.Validation, "结果类型名不能为空")
	}
	if shape.Kind == "" {
		return errcode.Newf(errcode.Validation, "结果类型 %q 未给出产物分码", shape.Name)
	}
	if shape.New == nil {
		return errcode.Newf(errcode.Validation, "结果类型 %q 未给出载荷构造函数", shape.Name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if prev, ok := r.shapes[shape.Name]; ok {
		return errcode.Newf(errcode.Conflict,
			"结果类型 %q 重复登记：先前登记于 %s，本次登记来自 %s（同一结果类型名只能有一种形状）",
			shape.Name, sourceOf(prev), sourceOf(shape))
	}
	if prevName, ok := r.kinds[shape.Kind]; ok {
		return errcode.Newf(errcode.Conflict,
			"结果分码 %q 被两个结果类型同时登记：%q 与 %q（同一结果形状只能有一处）",
			shape.Kind, prevName, shape.Name)
	}

	r.shapes[shape.Name] = shape
	r.kinds[shape.Kind] = shape.Name
	r.order = append(r.order, shape.Name)
	return nil
}

// Lookup 按结果类型名取登记条目。
func (r *ResultShapeRegistry) Lookup(name string) (ResultShape, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	shape, ok := r.shapes[name]
	return shape, ok
}

// ShapeForKind 按产物分码取登记条目。
func (r *ResultShapeRegistry) ShapeForKind(kind domain.StepPayloadKind) (ResultShape, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	name, ok := r.kinds[kind]
	if !ok {
		return ResultShape{}, false
	}
	shape, ok := r.shapes[name]
	return shape, ok
}

// Names 返回已登记的结果类型名，按登记顺序。
func (r *ResultShapeRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Shapes 返回已登记的结果类型条目，按登记顺序。
func (r *ResultShapeRegistry) Shapes() []ResultShape {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ResultShape, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.shapes[name])
	}
	return out
}

// Verify 校验注册表与步骤产物分码的一致性。
//
// 逐条检查：已登记类型的载荷必须能按它的分码编码成功，并且能解回同一种类型。
// 这一步把「注册表」与 domain 侧的分码映射钉在一起——
// 两者一旦漂移，产物就会被写成一种形状、按另一种形状去读。
func (r *ResultShapeRegistry) Verify() error {
	for _, shape := range r.Shapes() {
		encoded, err := domain.EncodePayload(shape.Kind, shape.New())
		if err != nil {
			return errcode.Newf(errcode.Validation,
				"结果类型 %q 与产物分码 %q 不一致：%s",
				shape.Name, shape.Kind, errcode.As(err).Details)
		}
		decoded, err := r.Decode(shape.Name, encoded)
		if err != nil {
			return errcode.Newf(errcode.Validation,
				"结果类型 %q 的载荷无法解回同一种类型：%s", shape.Name, errcode.As(err).Details)
		}
		if reflect.TypeOf(decoded) != reflect.TypeOf(shape.New()) {
			return errcode.Newf(errcode.Validation,
				"结果类型 %q 解回的类型与登记类型不符：期望 %s，实际 %s",
				shape.Name, reflect.TypeOf(shape.New()), reflect.TypeOf(decoded))
		}
	}
	return nil
}

// CheckProduct 校验一步的产物确实属于该分码登记的结果形状。
//
// 未登记的分码明确拒绝：宁可这一步判失败，也不让一份没人认领形状的产物进库。
// 登记了但类型对不上同样拒绝——那说明处理器还错了，返回了另一种形状。
func (r *ResultShapeRegistry) CheckProduct(kind domain.StepPayloadKind, product any) error {
	shape, ok := r.ShapeForKind(kind)
	if !ok {
		return errcode.Newf(errcode.Validation,
			"结果分码 %q 未登记结果形状（已登记：%s）", kind, strings.Join(r.Names(), "、"))
	}
	if product == nil {
		return errcode.Newf(errcode.Validation, "结果类型 %q 的产物为空", shape.Name)
	}
	want := reflect.TypeOf(shape.New())
	got := reflect.TypeOf(product)
	if want != got {
		return errcode.Newf(errcode.Validation,
			"结果类型 %q 的产物类型不匹配：期望 %s，收到 %s", shape.Name, want, got)
	}
	return nil
}

// Decode 按结果类型名把载荷信封还原成对应类型的产物。
//
// 未登记的结果类型明确拒绝（E_VALIDATION），不猜、不跨类型解析：
// 一份归因结论被当成异常记录表读，多半能解析成功，然后安静地读出半个结论。
func (r *ResultShapeRegistry) Decode(name string, raw json.RawMessage) (any, error) {
	shape, ok := r.Lookup(name)
	if !ok {
		return nil, errcode.Newf(errcode.Validation,
			"未登记的结果类型：%q（已登记：%s）", name, strings.Join(r.Names(), "、"))
	}
	if len(raw) == 0 {
		return nil, errcode.Newf(errcode.Validation, "结果类型 %q 没有载荷内容", name)
	}

	var envelope struct {
		Kind    domain.StepPayloadKind `json:"kind"`
		Payload json.RawMessage        `json:"payload"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, errcode.Newf(errcode.Validation,
			"结果类型 %q 的载荷信封解析失败：%v", name, err)
	}
	if envelope.Kind != shape.Kind {
		return nil, errcode.Newf(errcode.Validation,
			"结果类型 %q 的分码与载荷实际分码不符：登记 %q，载荷 %q",
			name, shape.Kind, envelope.Kind)
	}
	if len(envelope.Payload) == 0 {
		return nil, errcode.Newf(errcode.Validation, "结果类型 %q 的载荷信封内容为空", name)
	}

	target := shape.New()
	if err := json.Unmarshal(envelope.Payload, target); err != nil {
		return nil, errcode.Newf(errcode.Validation,
			"结果类型 %q 的载荷解析失败：%v", name, err)
	}
	return target, nil
}

// sourceOf 取登记来源；没写来源时回落到一个占位说法。
func sourceOf(shape ResultShape) string {
	if shape.Source == "" {
		return "未标注来源"
	}
	return shape.Source
}

// ChainShapeSpec 是步骤链上一步的结果形状声明。
type ChainShapeSpec struct {
	// StepName 是步骤名。
	StepName string
	// Kind 是该步产物的分码。
	Kind domain.StepPayloadKind
}

// BuiltinChainSpecs 返回内置判定步骤链的结果形状声明（步骤名 + 产物分码）。
//
// 它与 chain.go 装配出的链子一一对应：链子负责「这一步做什么」，
// 这里负责「这一步的结果叫什么、属于哪种形状」。
func BuiltinChainSpecs() []ChainShapeSpec {
	return []ChainShapeSpec{
		{StepName: StepFetchSeries, Kind: domain.PayloadSeries},
		{StepName: StepExtractWindow, Kind: domain.PayloadWindow},
		{StepName: StepAttribute, Kind: domain.PayloadAttribution},
		{StepName: StepDraft, Kind: domain.PayloadExceptionRecord},
		{StepName: StepAwaitConfirmation, Kind: domain.PayloadConfirmationReady},
	}
}

// BuiltinShapeSpecs 返回内置结果类型的登记条目。
//
// 模型原始输出也在其中：它由模型调用产生、挂在归因那一步上，
// 形状与其它产物一样独立可替换，所以一并登记、一起被唯一性校验盯着。
func BuiltinShapeSpecs() []ResultShape {
	return []ResultShape{
		{
			Name:   string(domain.PayloadSeries),
			Kind:   domain.PayloadSeries,
			New:    func() any { return &domain.SeriesPayload{} },
			Source: "builtin:fetch_series",
		},
		{
			Name:   string(domain.PayloadWindow),
			Kind:   domain.PayloadWindow,
			New:    func() any { return &domain.WindowPayload{} },
			Source: "builtin:extract_window",
		},
		{
			Name:   string(domain.PayloadAttribution),
			Kind:   domain.PayloadAttribution,
			New:    func() any { return &domain.AttributionPayload{} },
			Source: "builtin:attribute",
		},
		{
			Name:   string(domain.PayloadExceptionRecord),
			Kind:   domain.PayloadExceptionRecord,
			New:    func() any { return &domain.ExceptionRecordPayload{} },
			Source: "builtin:draft",
		},
		{
			Name:   string(domain.PayloadConfirmationReady),
			Kind:   domain.PayloadConfirmationReady,
			New:    func() any { return &domain.ConfirmationReadyPayload{} },
			Source: "builtin:await_confirmation",
		},
		{
			Name:   string(domain.PayloadModelRawOutput),
			Kind:   domain.PayloadModelRawOutput,
			New:    func() any { return &domain.ModelRawOutputPayload{} },
			Source: "builtin:model_call",
		},
	}
}

// BuildResultShapeRegistry 装配结果形状注册表：先登记内置形状，再登记扩展形状。
//
// 扩展形状与内置形状同名时直接返回冲突错误，并点名冲突的结果类型：
// 这正是「同一个结果类型名只能有一种形状」的落点。
func BuildResultShapeRegistry(extra ...ResultShape) (*ResultShapeRegistry, error) {
	reg := NewResultShapeRegistry()
	for _, shape := range append(BuiltinShapeSpecs(), extra...) {
		if err := reg.Register(shape); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

// DefaultResultShapeRegistry 装配只含内置形状的结果形状注册表。
func DefaultResultShapeRegistry() (*ResultShapeRegistry, error) {
	return BuildResultShapeRegistry()
}

// ValidateChainShapeSpecs 校验一条步骤链的结果形状声明。
//
// 两件事：
//   - 每一步的结果分码都必须已登记：未登记的类型明确拒绝，不让它混进判定链；
//   - 同一个结果分码不得被两步共用：同一结果形状只能有一处产出，
//     否则下游按分码取产物时不知道该取哪一份。
func ValidateChainShapeSpecs(specs []ChainShapeSpec, reg *ResultShapeRegistry) error {
	if reg == nil {
		return errcode.Newf(errcode.Validation, "结果形状注册表为空，无法校验步骤链")
	}
	seen := map[domain.StepPayloadKind]string{}
	for _, spec := range specs {
		shape, ok := reg.ShapeForKind(spec.Kind)
		if !ok {
			return errcode.Newf(errcode.Validation,
				"步骤 %q 的结果分码 %q 未登记结果形状", spec.StepName, spec.Kind)
		}
		if prev, dup := seen[spec.Kind]; dup {
			return errcode.Newf(errcode.Conflict,
				"结果形状 %q 被步骤 %q 与 %q 同时使用（同一结果形状只能有一处产出）",
				shape.Name, prev, spec.StepName)
		}
		seen[spec.Kind] = spec.StepName
	}
	return nil
}

// ValidateChainShapes 校验一条已装配步骤链的结果形状。
func ValidateChainShapes(chain []StepSpec, reg *ResultShapeRegistry) error {
	specs := make([]ChainShapeSpec, 0, len(chain))
	for _, spec := range chain {
		specs = append(specs, ChainShapeSpec{StepName: spec.Name, Kind: spec.PayloadKind})
	}
	return ValidateChainShapeSpecs(specs, reg)
}

// ValidateStartupShapes 校验启动时的结果形状唯一性。
//
// 三件事：
//  1. 注册表自检：每个已登记类型的载荷都能按分码编码并解回同一种类型；
//  2. 内置步骤链的每一步都有已登记的结果形状；
//  3. 内置步骤链里同一个结果分码不被两步共用。
//
// 任何一条不满足都返回错误，由 serve 打印冲突名并以非 0 退出码退出：
// 形状对不上的判定服务，宁可起不来。
func ValidateStartupShapes(reg *ResultShapeRegistry) error {
	if reg == nil {
		return errcode.Newf(errcode.Validation, "结果形状注册表为空，无法做启动校验")
	}
	if err := reg.Verify(); err != nil {
		return err
	}
	return ValidateChainShapeSpecs(BuiltinChainSpecs(), reg)
}
