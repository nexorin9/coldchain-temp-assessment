// 本文件落 HTTP 层的角色闸门：请求进来时先验凭据，再验这个凭据对应的身份
// 有没有资格执行这个端点声明的权限。
//
// 为什么鉴权之外还要再闸一道：凭据只证明「你进来了」，不证明「你能做这件事」。
// 记录仪的投递凭据能进这台机器，但它不该看见在库批次；冷链管理员能看结论，
// 但他不该替主管签处置结论。这两件事在业务上都是责任问题，不是技术问题。
//
// 身份有两个来源，二者必须一致：
//   - 令牌：服务端把 Bearer 令牌与已登记的凭据比对，命中哪一把就是哪个身份
//     （凭据表在 auth.go，身份随凭据一起登记，不靠请求方自称）；
//   - 请求头里的角色声明（X-Coldchain-Role）：调用方自报身份。
//
// 自报身份不能自证身份：只有声明没有可核对的令牌角色时，一律按最保守处理；
// 声明与令牌角色对不上时按拒绝处理——两者不一致本身就是一条可疑信号，
// 宁可先停下来问一句，也不替它挑一个身份放行。
package httpapi

import (
	"net/http"
	"strings"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// RoleDeclHeader 是请求头里的角色声明。
//
// 它是给人看、给排障用的：一眼就能看出「这台机器现在拿的是谁的凭据」。
// 但它不产生权限——权限永远由令牌对应的凭据决定。
const RoleDeclHeader = "X-Coldchain-Role"

// RequirePermission 返回校验角色权限的中间件。
//
// perm 是这个端点声明的权限；空值表示「端点没声明权限要求」，
// 此时一律拒绝——缺失声明的端点不开放，是默认拒绝而不是默认放行。
//
// 三种拒绝情形都回 403 与 E_ROLE_REQUIRED：
//   - 身份缺失或未登记（凭据没登记角色）；
//   - 角色声明与令牌角色不一致；
//   - 身份成立但没有这项权限。
//
// 401 与 403 分开是刻意的：401 是「你是谁我不知道」，403 是「知道你是谁，
// 但这件事不归你管」。一线人员看到 403 就知道该找主管签，而不是去查密码。
func RequirePermission(perm workflow.Permission, logger Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			role, err := resolveRole(r)
			if err != nil {
				writeError(w, errcode.As(err))
				if logger != nil {
					logger.Warn("role_rejected",
						"error_code", string(errcode.RoleRequired),
						"reason", "身份未登记或角色声明与凭据不一致",
						"path", r.URL.Path)
				}
				return
			}

			if err := workflow.Authorize(role, perm); err != nil {
				writeError(w, errcode.As(err))
				if logger != nil {
					logger.Warn("role_rejected",
						"error_code", string(errcode.RoleRequired),
						"role", string(role),
						"permission", string(perm),
						"path", r.URL.Path)
				}
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// resolveRole 判定本次请求的身份。
//
// 三条规则，顺序有意：
//  1. 角色声明带了但不是登记过的身份 → 直接拒绝。声明成别的身份说明调用方
//     对权限模型的理解与服务端不一致，放行等于替它猜一个身份。
//  2. 声明与令牌角色都在且不一致 → 拒绝。一份凭据一个身份，对不上就是矛盾。
//  3. 只有令牌角色 → 以令牌角色为准；令牌角色缺失 → 身份缺失，按最保守处理。
func resolveRole(r *http.Request) (workflow.Role, error) {
	credentialRole := RoleFrom(r.Context())

	rawDeclared := strings.TrimSpace(r.Header.Get(RoleDeclHeader))
	if rawDeclared != "" {
		declared := workflow.ParseRole(rawDeclared)
		if !workflow.KnownRole(declared) {
			return workflow.RoleUnknown, errcode.Newf(errcode.RoleRequired,
				"请求声明的身份 %q 未登记，拒绝执行", rawDeclared)
		}
		if !workflow.KnownRole(credentialRole) {
			return workflow.RoleUnknown, errcode.Newf(errcode.RoleRequired,
				"请求声明的身份是 %q，但所用凭据没有登记身份，拒绝执行", string(declared))
		}
		if declared != credentialRole {
			return workflow.RoleUnknown, errcode.Newf(errcode.RoleRequired,
				"请求声明的身份 %q 与凭据登记的身份 %q 不一致，拒绝执行",
				string(declared), string(credentialRole))
		}
		return credentialRole, nil
	}

	if !workflow.KnownRole(credentialRole) {
		return workflow.RoleUnknown, errcode.Newf(errcode.RoleRequired,
			"本次请求没有可核对的身份（凭据未登记角色），按最保守处理拒绝执行")
	}
	return credentialRole, nil
}
