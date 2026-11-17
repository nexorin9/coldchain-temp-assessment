package disposition

import (
	"strings"
	"testing"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// TestValues_CoverTheWholeDomain 覆盖取值域本身：
// 四个取值不多不少，标签都能对上。
func TestValues_CoverTheWholeDomain(t *testing.T) {
	values := Values()
	if len(values) != 4 {
		t.Fatalf("处置结论取值应为 4 个，实际 %d 个：%v", len(values), values)
	}
	for _, code := range values {
		label, ok := Label(code)
		if !ok {
			t.Errorf("取值 %q 没有中文标签", code)
		}
		if strings.TrimSpace(label) == "" {
			t.Errorf("取值 %q 的标签是空的", code)
		}
	}
}

// TestValidate_RejectsOutOfDomain 覆盖取值域之外的取值：
// 一律拒绝，且错误信息把合法取值一并写出来。
func TestValidate_RejectsOutOfDomain(t *testing.T) {
	err := Validate("ship_it")
	if err == nil {
		t.Fatal("取值域之外的处置结论不得放行")
	}
	if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("错误码不对：%v", err)
	}
	if !strings.Contains(err.Error(), "ship_it") {
		t.Errorf("错误信息未点名越界取值：%v", err)
	}
	for _, code := range Values() {
		if !strings.Contains(err.Error(), code) {
			t.Errorf("错误信息未列出合法取值 %q：%v", code, err)
		}
	}
}

// TestValidate_AcceptsDomainValues 覆盖合法取值一律放行。
func TestValidate_AcceptsDomainValues(t *testing.T) {
	for _, code := range Values() {
		if err := Validate(code); err != nil {
			t.Errorf("合法取值 %q 不该被拒：%v", code, err)
		}
	}
}

// TestIsManualOnly 覆盖「放行与报损是人工决定事项」：
// 系统作为建议给出时压下来并留痕，但取值域里仍然留着它们。
func TestIsManualOnly(t *testing.T) {
	if !IsManualOnly(Release) || !IsManualOnly(Scrap) {
		t.Error("放行与报损应当属于人工决定事项")
	}
	if IsManualOnly(Isolate) || IsManualOnly(PendingEvaluation) {
		t.Error("隔离与待企业评估不是人工专属取值")
	}
}
