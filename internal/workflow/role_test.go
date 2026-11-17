package workflow

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// TestAuthorize_OperatorCannotConfirm 验证经办（冷链管理员）不能确认处置结论。
//
// 这是整个权限矩阵里最要紧的一条：经办是天天跑判定的人，
// 如果他能自己判、自己签，那「主管审核」这一步就没有意义了，
// 出了问题也没法追责——签字的人本来就不该是干活的人。
func TestAuthorize_OperatorCannotConfirm(t *testing.T) {
	err := Authorize(RoleOperator, PermConfirm)
	if err == nil {
		t.Fatal("经办身份不应能确认处置结论，实际却放行了")
	}
	assertRoleRequired(t, err)
	if !strings.Contains(err.Error(), "药学部冷链管理员（经办）") {
		t.Fatalf("错误信息应点名当前身份，实际：%v", err)
	}
	if !strings.Contains(err.Error(), "确认或否决处置结论") {
		t.Fatalf("错误信息应点名缺失的权限，实际：%v", err)
	}
	if !strings.Contains(err.Error(), "药学部主管（审核）") {
		t.Fatalf("错误信息应指出持该权限的身份（让人知道该找谁签），实际：%v", err)
	}
}

// TestAuthorize_IngestCannotReadLedger 验证投递通道（记录仪 / 边缘网关）
// 不能读取在库批次与冷链台账。
//
// 记录仪是台面上的机器：它只管把温度读数送进来。看见了批次号，
// 它既不需要也用不上；而一旦这条通道能读库，机器凭据被抄走就等于
// 把整个库摊开——所以投递通道严格只读不写之外的东西一律不给。
func TestAuthorize_IngestCannotReadLedger(t *testing.T) {
	err := Authorize(RoleIngest, PermLedgerRead)
	if err == nil {
		t.Fatal("投递通道身份不应能读取冷链台账，实际却放行了")
	}
	assertRoleRequired(t, err)
	if !strings.Contains(err.Error(), "读取在库批次与冷链台账") {
		t.Fatalf("错误信息应点名缺失的权限，实际：%v", err)
	}

	// 投递通道唯一持有的是「投递温度事件」。
	if err := Authorize(RoleIngest, PermEventIngest); err != nil {
		t.Fatalf("投递通道应能投递温度事件，实际被拒：%v", err)
	}
}

// TestAuthorize_AuditorReadOnly 验证只读调阅岗（质控 / 院感）只读。
//
// 它能看判定依据、时间线与审计链，也能导出台账（导出是读动作）；
// 但它不投递事件、不触发判定、不改温区配置、更不能确认处置结论。
func TestAuthorize_AuditorReadOnly(t *testing.T) {
	allowed := []Permission{PermRunRead, PermLedgerRead, PermExport}
	for _, perm := range allowed {
		if err := Authorize(RoleAuditor, perm); err != nil {
			t.Fatalf("只读身份应能执行 %q，实际被拒：%v", perm, err)
		}
	}

	denied := []Permission{PermEventIngest, PermAssessTrigger, PermConfirm, PermProfileWrite}
	for _, perm := range denied {
		err := Authorize(RoleAuditor, perm)
		if err == nil {
			t.Fatalf("只读身份不应能执行 %q，实际却放行了", perm)
		}
		assertRoleRequired(t, err)
	}
}

// TestAuthorize_SupervisorCanConfirm 验证药学部主管能确认处置结论。
//
// 主管是唯一能把一条判定推进到「已生效」的身份：台账写回与接种门诊通知
// 都挂在这一步上。确认权与经办权分开，就是为了让「谁签字」这件事有意义。
func TestAuthorize_SupervisorCanConfirm(t *testing.T) {
	if err := Authorize(RoleSupervisor, PermConfirm); err != nil {
		t.Fatalf("主管身份应能确认处置结论，实际被拒：%v", err)
	}
	// 主管是四类身份里权限最全的：经办能做的他都能做。
	for _, perm := range PermissionsOf(RoleOperator) {
		if err := Authorize(RoleSupervisor, perm); err != nil {
			t.Fatalf("主管身份应持有经办的全部权限 %q，实际被拒：%v", perm, err)
		}
	}
}

// TestAuthorize_MissingRoleIsMostConservative 验证身份缺失时按最保守处理。
//
// 「不知道你是谁」不等于「你是自己人」。凭据命中了但没登记角色、
// 或者传入一个没登记过的身份，都按没有任何权限处理——
// 这条守则的代价只是多问一句，收益是不给任何含糊留缝。
func TestAuthorize_MissingRoleIsMostConservative(t *testing.T) {
	for _, role := range []Role{RoleUnknown, Role("admin"), Role("SUPERVISOR"), Role(" ")} {
		for _, perm := range Permissions() {
			if err := Authorize(role, perm); err == nil {
				t.Fatalf("身份 %q 不应持有任何权限（%q），实际却放行了", role, perm)
			} else {
				assertRoleRequired(t, err)
			}
		}
	}
}

// TestAuthorize_UnregisteredPermissionIsRejected 验证未登记的权限一律拒绝。
//
// 端点漏写权限声明时，按默认拒绝处理：宁可端点暂时不可用，
// 也不能因为「没写要求」就默认放行。
func TestAuthorize_UnregisteredPermissionIsRejected(t *testing.T) {
	err := Authorize(RoleSupervisor, Permission("delete_everything"))
	if err == nil {
		t.Fatal("未登记的权限不应放行")
	}
	assertRoleRequired(t, err)
	if !strings.Contains(err.Error(), "未登记") {
		t.Fatalf("错误信息应说明权限未登记，实际：%v", err)
	}
}

// TestAuthorize_ErrorMapsToHTTP403 验证越权一律映射为 403。
//
// 403 与 401 分开：401 是「你是谁我不知道」，403 是「知道你是谁，
// 但这件事不归你管」。一线人员看到 403 就知道该找主管签，而不是去改密码。
func TestAuthorize_ErrorMapsToHTTP403(t *testing.T) {
	err := Authorize(RoleOperator, PermConfirm)
	if err == nil {
		t.Fatal("期望越权被拒")
	}
	var coded *errcode.Error
	if !errors.As(err, &coded) {
		t.Fatalf("期望 *errcode.Error，实际：%T", err)
	}
	if coded.Code != errcode.RoleRequired {
		t.Fatalf("期望错误码 %s，实际：%s", errcode.RoleRequired, coded.Code)
	}
	if got := errcode.HTTPStatus(coded.Code); got != http.StatusForbidden {
		t.Fatalf("期望 HTTP %d，实际：%d", http.StatusForbidden, got)
	}
}

// TestParseRole_OnlyRegisteredRolesAreAccepted 验证角色声明只能落到登记过的身份。
//
// 请求头里的角色声明是调用方自报的，它不能自证身份，更不能造一个新身份。
func TestParseRole_OnlyRegisteredRolesAreAccepted(t *testing.T) {
	cases := map[string]Role{
		"ingest":     RoleIngest,
		" operator ": RoleOperator,
		"supervisor": RoleSupervisor,
		"AUDITOR":    RoleUnknown,
		"admin":      RoleUnknown,
		"":           RoleUnknown,
	}
	for raw, want := range cases {
		if got := ParseRole(raw); got != want {
			t.Fatalf("ParseRole(%q) 应为 %q，实际：%q", raw, want, got)
		}
	}
}

// TestPermissionMatrix_EveryPermissionHasAHolder 验证每项权限都有身份持有。
//
// 没有持有者的权限等于没有实现的能力：要么是矩阵写漏了，
// 要么是端点声明了一个谁也做不了的操作——后者会让现场点了没反应。
func TestPermissionMatrix_EveryPermissionHasAHolder(t *testing.T) {
	for _, perm := range Permissions() {
		holders := RolesWith(perm)
		if len(holders) == 0 {
			t.Fatalf("权限 %q 没有任何身份持有", perm)
		}
	}
}

// TestPermissionMatrix_EveryRoleHasAtLeastOnePermission 验证每类身份都有事可做。
func TestPermissionMatrix_EveryRoleHasAtLeastOnePermission(t *testing.T) {
	for _, role := range Roles() {
		if len(PermissionsOf(role)) == 0 {
			t.Fatalf("身份 %q 没有任何权限", role)
		}
	}
}

// TestPermissionMatrix_IngestIsWriteOnly 验证投递通道是写通道，不是读通道。
func TestPermissionMatrix_IngestIsWriteOnly(t *testing.T) {
	perms := PermissionsOf(RoleIngest)
	if len(perms) != 1 || perms[0] != PermEventIngest {
		t.Fatalf("投递通道应只持有「投递温度事件」，实际：%v", perms)
	}
}

// TestPermissionMatrix_ConfirmBelongsToSupervisorOnly 验证确认权只属于主管。
func TestPermissionMatrix_ConfirmBelongsToSupervisorOnly(t *testing.T) {
	for _, role := range Roles() {
		allowed := Allows(role, PermConfirm)
		if role == RoleSupervisor && !allowed {
			t.Fatal("主管应持有确认权")
		}
		if role != RoleSupervisor && allowed {
			t.Fatalf("身份 %q 不应持有确认权", role)
		}
	}
}

// TestRolesAndPermissions_AreStableCopies 验证返回的是副本且顺序固定。
//
// 顺序固定是为了让接口输出与文档表格逐字可比；
// 副本是为了调用方改不动内部状态机。
func TestRolesAndPermissions_AreStableCopies(t *testing.T) {
	roles := Roles()
	if len(roles) != 4 {
		t.Fatalf("应登记四类身份，实际：%d", len(roles))
	}
	roles[0] = Role("tampered")
	if Roles()[0] != RoleIngest {
		t.Fatal("Roles 应返回副本")
	}

	perms := Permissions()
	perms[0] = Permission("tampered")
	if Permissions()[0] != PermEventIngest {
		t.Fatal("Permissions 应返回副本")
	}

	matrix := PermissionMatrix()
	if len(matrix) != 4 {
		t.Fatalf("权限矩阵应有四行，实际：%d", len(matrix))
	}
	if len(matrix[string(RoleIngest)]) != 1 {
		t.Fatalf("投递通道在矩阵里应只有一项权限，实际：%v", matrix[string(RoleIngest)])
	}
}

// assertRoleRequired 断言错误是 E_ROLE_REQUIRED。
func assertRoleRequired(t *testing.T, err error) {
	t.Helper()
	var coded *errcode.Error
	if !errors.As(err, &coded) {
		t.Fatalf("期望 *errcode.Error，实际：%T（%v）", err, err)
	}
	if coded.Code != errcode.RoleRequired {
		t.Fatalf("期望错误码 %s，实际：%s", errcode.RoleRequired, coded.Code)
	}
}

// TestAuthorize_WritebackRetryBelongsToOperatorAndSupervisor 验证补偿重放权限的归属：
// 经办（日常跑判定的人）与主管都能补发待补偿写回；投递通道与只读岗拿不到。
//
// 它与确认权分开：补发不产生新的结论——载荷、幂等键、external_ref
// 全是主管确认那一刻定下的那份，补发的人只是把没送到的那一份再送一遍。
func TestAuthorize_WritebackRetryBelongsToOperatorAndSupervisor(t *testing.T) {
	cases := []struct {
		role    Role
		allowed bool
	}{
		{RoleOperator, true},
		{RoleSupervisor, true},
		{RoleAuditor, false},
		{RoleIngest, false},
		{RoleUnknown, false},
	}
	for _, tc := range cases {
		err := Authorize(tc.role, PermWritebackRetry)
		if tc.allowed && err != nil {
			t.Errorf("身份 %q 应能触发补偿重放，实际被拒：%v", tc.role, err)
		}
		if !tc.allowed {
			if err == nil {
				t.Errorf("身份 %q 不应能触发补偿重放，实际放行了", tc.role)
				continue
			}
			if code := errcode.As(err).Code; code != errcode.RoleRequired {
				t.Errorf("身份 %q 的错误码 = %q，期望 E_ROLE_REQUIRED", tc.role, code)
			}
		}
	}
}

// TestAuthorize_WritebackRetryIsNotConfirmPower 验证补发权不等于确认权：
// 拿到补偿重放权限的身份，仍然拿不到确认权。
func TestAuthorize_WritebackRetryIsNotConfirmPower(t *testing.T) {
	if Allows(RoleOperator, PermWritebackRetry) != true {
		t.Fatal("经办应持有补偿重放权限")
	}
	if Allows(RoleOperator, PermConfirm) != false {
		t.Fatal("经办不应因持有补偿重放权限而获得确认权")
	}
	if Allows(RoleAuditor, PermWritebackRetry) != false {
		t.Fatal("只读岗不应持有补偿重放权限")
	}
}
