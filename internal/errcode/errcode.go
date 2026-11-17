// Package errcode 定义冷链温控判定服务的结构化错误码表与 HTTP 状态映射。
//
// 错误码是本服务对外契约的一部分：入站事件接入、判定 run、台账写回与审计端点
// 统一返回 {error_code, message, details}，调用方据此区分「重试即可」
// （E_MODEL_TIMEOUT / E_LEDGER_UNAVAILABLE）与「必须人工处理」
// （E_VALIDATION / E_ILLEGAL_TRANSITION / E_ROLE_REQUIRED）。
package errcode

import (
	"errors"
	"fmt"
	"net/http"
)

// Code 是本服务的错误码，取值以 E_ 开头，全大写下划线分词。
type Code string

// 本服务的全部错误码。
const (
	// 配置类
	ConfigMissing Code = "E_CONFIG_MISSING"

	// 接入与校验类
	Unauthorized Code = "E_UNAUTHORIZED"
	Range        Code = "E_RANGE"
	Validation   Code = "E_VALIDATION"
	// RateLimited：同一点位的投递超出限流上限。
	// 单独一档而不是复用 E_CONFLICT：调用方看到它就该退避重投，
	// 而不是以为自己写错了数据、放弃这一条。
	RateLimited Code = "E_RATE_LIMITED"
	// StorageError：本地存储写入或读取失败（磁盘、锁、库损坏）。
	// 与 E_VALIDATION 区分：校验错是数据的问题，存储错是设备的问题，重投无用。
	StorageError Code = "E_STORAGE_ERROR"

	// 业务状态类
	IllegalTransition Code = "E_ILLEGAL_TRANSITION"
	RoleRequired      Code = "E_ROLE_REQUIRED"
	NotFound          Code = "E_NOT_FOUND"
	Conflict          Code = "E_CONFLICT"

	// 模型网关类
	ModelTimeout       Code = "E_MODEL_TIMEOUT"
	ModelUnavailable   Code = "E_MODEL_UNAVAILABLE"
	ModelProtocol      Code = "E_MODEL_PROTOCOL"
	ModelSchemaInvalid Code = "E_MODEL_SCHEMA_INVALID"
	ModelFactMismatch  Code = "E_MODEL_FACT_MISMATCH"

	// 出站台账类
	LedgerUnavailable Code = "E_LEDGER_UNAVAILABLE"
	// LedgerProtocol：台账回了，但回的不是合法的台账响应
	// （连不上 JSON、缺必填字段、字段类型不对）。
	//
	// 与 LedgerUnavailable 分开：前者是端点坏了或网络不通（可退避重试），
	// 这一档是「对端在说另一种话」——重发一次多半还是这样，要人去查台账侧。
	LedgerProtocol Code = "E_LEDGER_PROTOCOL"

	// 出站通知类
	// NotifyUnavailable：通知系统不可达（超时 / 连接失败 / 5xx / 限流）。
	//
	// 与 LedgerUnavailable 分开：两者处置不同——台账写不成是「账里没有这条」，
	// 通知发不出是「门诊还不知道」。混成一句，复核的人分不清该先修哪一头。
	NotifyUnavailable Code = "E_NOTIFY_UNAVAILABLE"
	// NotifyProtocol：通知系统回了，但回的不是合法的通知响应。
	NotifyProtocol Code = "E_NOTIFY_PROTOCOL"
)

// descriptions 是错误码的中文含义，用于日志与错误响应文案。
var descriptions = map[Code]string{
	ConfigMissing:      "配置缺失或非法，服务无法启动",
	Unauthorized:       "缺少凭据或凭据不正确",
	Range:              "数值超出允许量程",
	Validation:         "字段缺失或取值不合法",
	RateLimited:        "同一点位投递超出限流上限",
	StorageError:       "本地存储读写失败",
	IllegalTransition:  "业务状态不允许该迁移",
	RoleRequired:       "当前身份无权执行该操作",
	NotFound:           "对象不存在",
	Conflict:           "与既有数据冲突",
	ModelTimeout:       "模型调用超时",
	ModelUnavailable:   "模型服务不可达",
	ModelProtocol:      "模型响应不符合 OpenAI-compatible 协议",
	ModelSchemaInvalid: "模型输出未通过结构化校验",
	ModelFactMismatch:  "模型输出与输入事实不符",
	LedgerUnavailable:  "冷链台账不可达",
	LedgerProtocol:     "冷链台账响应不符合协议",
	NotifyUnavailable:  "通知系统不可达",
	NotifyProtocol:     "通知系统响应不符合协议",
}

// httpStatuses 是错误码到 HTTP 状态码的映射。
var httpStatuses = map[Code]int{
	ConfigMissing:      http.StatusInternalServerError,
	Unauthorized:       http.StatusUnauthorized,
	Range:              http.StatusUnprocessableEntity,
	Validation:         http.StatusBadRequest,
	RateLimited:        http.StatusTooManyRequests,
	StorageError:       http.StatusInternalServerError,
	IllegalTransition:  http.StatusConflict,
	RoleRequired:       http.StatusForbidden,
	NotFound:           http.StatusNotFound,
	Conflict:           http.StatusConflict,
	ModelTimeout:       http.StatusGatewayTimeout,
	ModelUnavailable:   http.StatusServiceUnavailable,
	ModelProtocol:      http.StatusBadGateway,
	ModelSchemaInvalid: http.StatusBadGateway,
	ModelFactMismatch:  http.StatusBadGateway,
	LedgerUnavailable:  http.StatusServiceUnavailable,
	LedgerProtocol:     http.StatusBadGateway,
	NotifyUnavailable:  http.StatusServiceUnavailable,
	NotifyProtocol:     http.StatusBadGateway,
}

// Describe 返回错误码的中文含义；未登记的错误码返回兜底文案。
func Describe(code Code) string {
	if s, ok := descriptions[code]; ok {
		return s
	}
	return "未登记的错误码"
}

// HTTPStatus 返回错误码对应的 HTTP 状态码；未登记的错误码按 500 处理。
func HTTPStatus(code Code) int {
	if s, ok := httpStatuses[code]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// Error 是带错误码的运行时错误，可直接序列化为对外错误响应体。
type Error struct {
	Code    Code   `json:"error_code"`
	Message string `json:"message"`
	Details string `json:"details,omitempty"`
}

// Error 实现 error 接口。
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Details == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s（%s）", e.Code, e.Message, e.Details)
}

// New 构造一个带错误码的错误；details 可为空。
func New(code Code, details string) *Error {
	return &Error{Code: code, Message: Describe(code), Details: details}
}

// Newf 构造一个带错误码与格式化细节的错误。
func Newf(code Code, format string, args ...any) *Error {
	return New(code, fmt.Sprintf(format, args...))
}

// As 把任意 error 归一化为 *Error；非 *Error 一律按 E_VALIDATION 处理。
//
// 它走 errors.As 而不是直接类型断言：带包装的错误（如模型层的
// *model.SchemaError）必须能取回它内层的错误码，否则 E_MODEL_SCHEMA_INVALID
// 会被降级成 E_VALIDATION——两者在处置上完全不同（前者可重问，后者要人看）。
func As(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return New(Validation, err.Error())
}
