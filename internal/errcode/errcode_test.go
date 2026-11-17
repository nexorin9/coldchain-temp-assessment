package errcode

import (
	"net/http"
	"testing"
)

func TestHTTPStatusMapping(t *testing.T) {
	cases := []struct {
		code Code
		want int
	}{
		{Unauthorized, http.StatusUnauthorized},
		{Validation, http.StatusBadRequest},
		{Range, http.StatusUnprocessableEntity},
		{IllegalTransition, http.StatusConflict},
		{RoleRequired, http.StatusForbidden},
		{NotFound, http.StatusNotFound},
		{Conflict, http.StatusConflict},
		{ModelTimeout, http.StatusGatewayTimeout},
		{ModelUnavailable, http.StatusServiceUnavailable},
		{ModelProtocol, http.StatusBadGateway},
		{ModelSchemaInvalid, http.StatusBadGateway},
		{ModelFactMismatch, http.StatusBadGateway},
		{LedgerUnavailable, http.StatusServiceUnavailable},
		{ConfigMissing, http.StatusInternalServerError},
	}
	for _, c := range cases {
		if got := HTTPStatus(c.code); got != c.want {
			t.Errorf("%s 期望状态码 %d，实际 %d", c.code, c.want, got)
		}
	}
}

func TestUnknownCodeFallsBackToInternalServerError(t *testing.T) {
	if got := HTTPStatus(Code("E_NOT_REGISTERED")); got != http.StatusInternalServerError {
		t.Fatalf("未登记错误码期望 500，实际 %d", got)
	}
	if Describe(Code("E_NOT_REGISTERED")) == "" {
		t.Fatal("未登记错误码仍应有兜底文案")
	}
}

func TestNewAndAs(t *testing.T) {
	e := Newf(Validation, "字段 %s 缺失", "point_id")
	if e.Code != Validation {
		t.Fatalf("期望错误码 %s，实际 %s", Validation, e.Code)
	}
	if e.Message != Describe(Validation) {
		t.Fatalf("期望错误消息取自错误码表，实际 %q", e.Message)
	}
	if e.Details != "字段 point_id 缺失" {
		t.Fatalf("期望细节为格式化文本，实际 %q", e.Details)
	}

	wrapped := As(e)
	if wrapped != e {
		t.Fatal("As 应原样返回 *Error")
	}
	other := As(errPlain{})
	if other.Code != Validation {
		t.Fatalf("普通错误应归一化为 E_VALIDATION，实际 %s", other.Code)
	}
	if As(nil) != nil {
		t.Fatal("nil 错误应返回 nil")
	}
}

// errPlain 是用于测试归一化逻辑的普通错误。
type errPlain struct{}

func (errPlain) Error() string { return "普通错误" }
