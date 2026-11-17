package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// TestRegistry_BuiltinShapesRegistered 验证内置结果类型都登记在册：
// 按名字查得到、按分码反查得到，两条路指向同一条登记。
func TestRegistry_BuiltinShapesRegistered(t *testing.T) {
	reg, err := DefaultResultShapeRegistry()
	if err != nil {
		t.Fatalf("装配内置结果形状注册表失败：%v", err)
	}

	wantKinds := []domain.StepPayloadKind{
		domain.PayloadSeries,
		domain.PayloadWindow,
		domain.PayloadAttribution,
		domain.PayloadExceptionRecord,
		domain.PayloadConfirmationReady,
		domain.PayloadModelRawOutput,
	}
	for _, kind := range wantKinds {
		shape, ok := reg.ShapeForKind(kind)
		if !ok {
			t.Fatalf("分码 %q 未登记结果形状", kind)
		}
		if shape.Kind != kind {
			t.Fatalf("分码 %q 反查到的登记分码应为 %q，实际 %q", kind, kind, shape.Kind)
		}
		byName, ok := reg.Lookup(shape.Name)
		if !ok {
			t.Fatalf("结果类型 %q 按名查不到", shape.Name)
		}
		if byName.Kind != shape.Kind {
			t.Fatalf("结果类型 %q 按名查到的分码应为 %q，实际 %q", shape.Name, shape.Kind, byName.Kind)
		}
	}
	if got := len(reg.Names()); got != len(wantKinds) {
		t.Fatalf("登记的结果类型数应为 %d，实际 %d", len(wantKinds), got)
	}
}

// TestRegistry_DuplicateNameRejected 验证同名重复登记直接报冲突，并点名冲突的类型。
//
// 这是本任务的核心断言：重复登记同名结果类型必须失败，
// 不能「后来者覆盖前者」——覆盖掉的那份形状就此失联，
// 下游按名字取产物时会拿到另一种形状。
func TestRegistry_DuplicateNameRejected(t *testing.T) {
	reg := NewResultShapeRegistry()

	first := ResultShape{
		Name:   "attribution",
		Kind:   domain.PayloadAttribution,
		New:    func() any { return &domain.AttributionPayload{} },
		Source: "test:首次登记",
	}
	if err := reg.Register(first); err != nil {
		t.Fatalf("首次登记应成功：%v", err)
	}

	second := first
	second.Source = "test:重复登记"

	err := reg.Register(second)
	if err == nil {
		t.Fatal("重复登记同名结果类型必须报错")
	}
	if code := errcode.As(err).Code; code != errcode.Conflict {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.Conflict, code)
	}
	details := errcode.As(err).Details
	if !strings.Contains(details, "attribution") {
		t.Fatalf("错误详情应点名冲突的结果类型，实际：%s", details)
	}
	if !strings.Contains(details, "重复登记") {
		t.Fatalf("错误详情应说明是重复登记，实际：%s", details)
	}
	// 冲突不能改动既有登记：第一次登记的那份必须还在。
	shape, ok := reg.Lookup("attribution")
	if !ok {
		t.Fatal("冲突后原有登记不应消失")
	}
	if shape.Source != "test:首次登记" {
		t.Fatalf("冲突后原有登记的来源不应被改写，实际 %q", shape.Source)
	}
}

// TestRegistry_SameKindUnderTwoNamesRejected 验证同一分码不得挂在两个结果类型名下。
//
// 一个分码只能有一种形状：否则下游按分码取产物时不知道该取哪一份。
func TestRegistry_SameKindUnderTwoNamesRejected(t *testing.T) {
	reg := NewResultShapeRegistry()

	if err := reg.Register(ResultShape{
		Name:   "attribution_v2",
		Kind:   domain.PayloadAttribution,
		New:    func() any { return &domain.AttributionPayload{} },
		Source: "test:首次登记",
	}); err != nil {
		t.Fatalf("首次登记应成功：%v", err)
	}

	err := reg.Register(ResultShape{
		Name:   "attribution_v3",
		Kind:   domain.PayloadAttribution,
		New:    func() any { return &domain.AttributionPayload{} },
		Source: "test:另一个名字",
	})
	if err == nil {
		t.Fatal("同一分码挂到两个结果类型名下必须报错")
	}
	if code := errcode.As(err).Code; code != errcode.Conflict {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.Conflict, code)
	}
	if !strings.Contains(errcode.As(err).Details, "attribution_v2") {
		t.Fatalf("错误详情应点名两个冲突的结果类型，实际：%s", errcode.As(err).Details)
	}
}

// TestRegistry_ValidateStartupShapesPasses 验证内置步骤链的启动校验通过：
// 每一步都有已登记的结果形状，且没有两步共用同一形状。
func TestRegistry_ValidateStartupShapesPasses(t *testing.T) {
	reg, err := DefaultResultShapeRegistry()
	if err != nil {
		t.Fatalf("装配内置结果形状注册表失败：%v", err)
	}
	if err := ValidateStartupShapes(reg); err != nil {
		t.Fatalf("内置步骤链的启动校验应通过：%v", err)
	}
	if err := reg.Verify(); err != nil {
		t.Fatalf("内置注册表自检应通过：%v", err)
	}
}

// TestRegistry_ChainReusingOneShapeRejected 验证同一个结果分码被两步共用时报冲突。
//
// 这正是「同一结果形状唯一」要拦的情况：两步产出同一种分码，
// 下游按分码取产物时无从判断该取哪一份。
func TestRegistry_ChainReusingOneShapeRejected(t *testing.T) {
	reg, err := DefaultResultShapeRegistry()
	if err != nil {
		t.Fatalf("装配内置结果形状注册表失败：%v", err)
	}

	specs := []ChainShapeSpec{
		{StepName: StepAttribute, Kind: domain.PayloadAttribution},
		{StepName: StepDraft, Kind: domain.PayloadAttribution},
	}
	err = ValidateChainShapeSpecs(specs, reg)
	if err == nil {
		t.Fatal("两步共用同一结果形状必须报错")
	}
	if code := errcode.As(err).Code; code != errcode.Conflict {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.Conflict, code)
	}
	details := errcode.As(err).Details
	if !strings.Contains(details, string(domain.PayloadAttribution)) {
		t.Fatalf("错误详情应点名冲突的结果形状，实际：%s", details)
	}
	if !strings.Contains(details, StepDraft) {
		t.Fatalf("错误详情应点名冲突的步骤，实际：%s", details)
	}
}

// TestRegistry_UnregisteredKindRejected 验证未登记的结果分码被明确拒绝。
func TestRegistry_UnregisteredKindRejected(t *testing.T) {
	reg, err := DefaultResultShapeRegistry()
	if err != nil {
		t.Fatalf("装配内置结果形状注册表失败：%v", err)
	}

	unknown := domain.StepPayloadKind("custom_shape")
	err = ValidateChainShapeSpecs([]ChainShapeSpec{{StepName: "custom", Kind: unknown}}, reg)
	if err == nil {
		t.Fatal("未登记的结果分码必须报错")
	}
	if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.Validation, code)
	}
	if !strings.Contains(errcode.As(err).Details, string(unknown)) {
		t.Fatalf("错误详情应点名未登记的分码，实际：%s", errcode.As(err).Details)
	}
}

// TestRegistry_DecodeRegisteredPayload 验证已登记类型的载荷能解回同一种类型。
func TestRegistry_DecodeRegisteredPayload(t *testing.T) {
	reg, err := DefaultResultShapeRegistry()
	if err != nil {
		t.Fatalf("装配内置结果形状注册表失败：%v", err)
	}

	original := &domain.AttributionPayload{CauseCode: "power_loss", Confidence: 0.6}
	encoded, err := domain.EncodePayload(domain.PayloadAttribution, original)
	if err != nil {
		t.Fatalf("编码归因结论失败：%v", err)
	}

	decoded, err := reg.Decode(string(domain.PayloadAttribution), encoded)
	if err != nil {
		t.Fatalf("解回归因结论失败：%v", err)
	}
	typed, ok := decoded.(*domain.AttributionPayload)
	if !ok {
		t.Fatalf("解回的类型应为 *domain.AttributionPayload，实际 %T", decoded)
	}
	if typed.CauseCode != original.CauseCode || typed.Confidence != original.Confidence {
		t.Fatalf("解回的载荷与原载荷不一致：%+v", typed)
	}
}

// TestRegistry_DecodeUnregisteredTypeRejected 验证未登记的结果类型被明确拒绝，
// 不跨类型猜测解析。
func TestRegistry_DecodeUnregisteredTypeRejected(t *testing.T) {
	reg, err := DefaultResultShapeRegistry()
	if err != nil {
		t.Fatalf("装配内置结果形状注册表失败：%v", err)
	}

	encoded, err := domain.EncodePayload(domain.PayloadAttribution, &domain.AttributionPayload{})
	if err != nil {
		t.Fatalf("编码归因结论失败：%v", err)
	}

	_, err = reg.Decode("not_registered", encoded)
	if err == nil {
		t.Fatal("未登记的结果类型必须报错")
	}
	if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.Validation, code)
	}
	if !strings.Contains(errcode.As(err).Details, "not_registered") {
		t.Fatalf("错误详情应点名未登记的结果类型，实际：%s", errcode.As(err).Details)
	}
}

// TestRegistry_VerifyDetectsKindMismatch 验证注册表与分码映射不一致时自检报错。
//
// 登记的构造函数类型与分码在 domain 侧的映射对不上（比如把采样形状
// 挂在归因分码下），自检必须把它点出来，而不是放行。
func TestRegistry_VerifyDetectsKindMismatch(t *testing.T) {
	reg := NewResultShapeRegistry()
	if err := reg.Register(ResultShape{
		Name:   "broken",
		Kind:   domain.PayloadAttribution,
		New:    func() any { return &domain.SeriesPayload{} },
		Source: "test:故意错配",
	}); err != nil {
		t.Fatalf("登记应成功：%v", err)
	}

	err := reg.Verify()
	if err == nil {
		t.Fatal("形状与分码错配时自检必须报错")
	}
	if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.Validation, code)
	}
	if !strings.Contains(errcode.As(err).Details, "broken") {
		t.Fatalf("错误详情应点名错配的结果类型，实际：%s", errcode.As(err).Details)
	}
}

// TestRegistry_CheckProductRejectsWrongType 验证产物类型与登记形状对不上时被拒绝。
func TestRegistry_CheckProductRejectsWrongType(t *testing.T) {
	reg, err := DefaultResultShapeRegistry()
	if err != nil {
		t.Fatalf("装配内置结果形状注册表失败：%v", err)
	}

	if err := reg.CheckProduct(domain.PayloadAttribution, &domain.AttributionPayload{}); err != nil {
		t.Fatalf("类型正确的产物应通过形状自检：%v", err)
	}

	err = reg.CheckProduct(domain.PayloadAttribution, &domain.SeriesPayload{})
	if err == nil {
		t.Fatal("类型对不上的产物必须被拒绝")
	}
	if !strings.Contains(errcode.As(err).Details, string(domain.PayloadAttribution)) {
		t.Fatalf("错误详情应点名结果类型，实际：%s", errcode.As(err).Details)
	}

	err = reg.CheckProduct(domain.StepPayloadKind("custom_shape"), &domain.SeriesPayload{})
	if err == nil {
		t.Fatal("未登记分码的产物必须被拒绝")
	}
	if !strings.Contains(errcode.As(err).Details, "custom_shape") {
		t.Fatalf("错误详情应点名未登记的分码，实际：%s", errcode.As(err).Details)
	}
}

// TestExecutor_UnregisteredShapeFailsStep 验证未登记结果形状的产物不会进库：
// 这一步判失败、错误码明确、产物不落库。
func TestExecutor_UnregisteredShapeFailsStep(t *testing.T) {
	st := openStore(t)
	chain := []StepSpec{{
		Name:        "custom_step",
		PayloadKind: domain.StepPayloadKind("custom_shape"),
		Handler: func(ctx context.Context, in StepInput) (any, error) {
			return &domain.SeriesPayload{PointID: testPointID}, nil
		},
	}}
	exec := NewExecutor(chain, st, quietLogger())

	result, err := exec.Run(context.Background(), directRun(t, excursionSamples()))
	if err == nil {
		t.Fatal("未登记结果形状的产物应判这一步失败")
	}
	if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.Validation, code)
	}
	if result.Run.ExecState != domain.ExecStateFailed {
		t.Fatalf("执行态应落到 failed，实际 %q", result.Run.ExecState)
	}
	if len(result.Steps) != 1 || len(result.Steps[0].Payload) != 0 {
		t.Fatal("形状未登记的产物不应被写入")
	}
	stored, err := st.RunStepsByRun(context.Background(), result.Run.RunID)
	if err != nil {
		t.Fatalf("回读步骤记录失败：%v", err)
	}
	for _, step := range stored {
		if len(step.Payload) != 0 {
			t.Fatalf("步骤 %s 不应留下产物载荷", step.StepName)
		}
	}
}
