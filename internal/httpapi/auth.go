package httpapi

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// 本文件是入站事件的 Bearer 鉴权中间件。
//
// 入站面是院内冷链监测平台、边缘网关与记录仪直连本服务的入口，
// 一旦敞开，任何能连到这台主机的人都能往库里塞温度事件——
// 而这些事件会直接进入越温判定与批次处置建议。
// 所以鉴权这一道关口只有一条原则：认不准就拒，绝不放行。
//
// 三条边界贯穿本文件：
//   - 凭据只从环境变量 COLDCHAIN_INGEST_TOKEN 读出（由调用方传入），进程内不落盘、不写日志；
//   - 比对用常数时间比较，避免有人靠响应耗时逐位猜凭据；
//   - 失败一律 401，且响应体、响应头、日志都不回显任何与凭据相关的内容。

// bearerScheme 是入站凭据使用的鉴权方案名（大小写不敏感）。
const bearerScheme = "bearer"

// Credential 是「一种身份 + 一把凭据」。
//
// 同一个端点可以挂多把凭据：入站凭据给记录仪与边缘网关，
// 经办凭据给冷链管理员，主管凭据给药学部主管，只读凭据给质控与内审。
// 身份随凭据一起确定，不靠请求方自己声明——声明的身份是不可信的，
// 详见 roles.go。
type Credential struct {
	// Role 是持有这把凭据的身份标识（如 ingest / operator / supervisor / auditor）。
	Role workflow.Role
	// Token 是凭据原文；为空表示这把凭据没配，不参与匹配。
	Token string
}

// credentialRoleKey 是上下文键：标记本次请求命中的是哪把凭据。
type credentialRoleKey struct{}

// RoleFrom 返回本次请求命中的凭据身份；取不到返回 workflow.RoleUnknown。
func RoleFrom(ctx context.Context) workflow.Role {
	if ctx == nil {
		return workflow.RoleUnknown
	}
	if role, ok := ctx.Value(credentialRoleKey{}).(workflow.Role); ok {
		return role
	}
	return workflow.RoleUnknown
}

// BearerAuthAny 返回接受凭据表里任意一把凭据的中间件。
//
// 用法：把「这个端点允许谁看」变成一张凭据表，凭据表由装配方从环境变量读出后传入；
// 命中哪一把，就把它的身份放进请求上下文，交给角色闸门（roles.go）判断权限。
// 全部凭据都不匹配时回 401 且给出同一句话——差异化的错误提示等于告诉试探者
// 「你差得很近」或者「这台机器没配凭据」。
func BearerAuthAny(creds []Credential, logger Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			usable := make([]Credential, 0, len(creds))
			for _, cred := range creds {
				if strings.TrimSpace(cred.Token) == "" {
					continue
				}
				usable = append(usable, cred)
			}
			if len(usable) == 0 {
				writeAuthFailure(w)
				if logger != nil {
					logger.Error("auth_rejected",
						"error_code", string(errcode.ConfigMissing),
						"reason", "服务未配置可用凭据")
				}
				return
			}

			presented, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				writeAuthFailure(w)
				if logger != nil {
					logger.Warn("auth_rejected",
						"error_code", string(errcode.Unauthorized),
						"reason", "缺少或格式不正确的 Authorization 头",
						"path", r.URL.Path)
				}
				return
			}

			for _, cred := range usable {
				// 常数时间比较：长度不同也走完同样的比较流程，不给逐位试探留时间差。
				if subtle.ConstantTimeCompare([]byte(presented), []byte(cred.Token)) != 1 {
					continue
				}
				ctx := context.WithValue(r.Context(), credentialRoleKey{}, cred.Role)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			writeAuthFailure(w)
			if logger != nil {
				logger.Warn("auth_rejected",
					"error_code", string(errcode.Unauthorized),
					"reason", "凭据不匹配",
					"path", r.URL.Path)
			}
		})
	}
}

// authFailureMessage 是所有鉴权失败共用的提示文案。
//
// 三种失败——没带凭据、头部格式不对、凭据不符——必须给出同一句话。
// 提示语一旦有差别，就等于告诉试探者「你的凭据接近了」或者
// 「这台服务根本没配凭据」，反而多送出去一条信息。
const authFailureMessage = "缺少或无效的入站凭据"

// bearerToken 从 Authorization 头里取出 Bearer 凭据。
//
// 头部写法为 "Bearer <token>"：方案名大小写不敏感，凭据本身不允许含空格
// （含空格的取值在 HTTP 头部里本就不可靠，直接判为格式错误）。
func bearerToken(header string) (string, bool) {
	trimmed := strings.TrimSpace(header)
	if trimmed == "" {
		return "", false
	}
	scheme, rest, found := strings.Cut(trimmed, " ")
	if !found || !strings.EqualFold(scheme, bearerScheme) {
		return "", false
	}
	token := strings.TrimSpace(rest)
	if token == "" {
		return "", false
	}
	return token, true
}

// writeAuthFailure 写出统一的 401 响应。
//
// 响应头只回 WWW-Authenticate（按 RFC 6750 提示客户端改用 Bearer），
// 响应体只回错误码与一句通用说明，绝不回显收到的凭据。
func writeAuthFailure(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="coldchain-ingest"`)
	writeError(w, errcode.Newf(errcode.Unauthorized, "%s", authFailureMessage))
}
