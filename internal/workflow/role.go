// 本文件落「谁在什么岗位能做什么」——四类身份与它们各自的权限矩阵。
//
// 为什么权限要和状态机（state.go）分开：状态机管的是「这条判定走到哪一步」，
// 权限管的是「谁能让它走到那一步」。两者是正交的两件事：
//   - 状态机回答「已确认」这一步存在吗、能不能从「待确认」走过去；
//   - 权限回答「眼前这个人有没有资格走这一步」。
//
// 把两件事混在一张表里，最常见的后果是「判定链自己把状态推到已确认」——
// 于是「已确认」这个名字还在，责任却没了。
//
// 三条纪律贯穿本文件：
//   - 白名单：权限写在矩阵里，没写进去的组合一律拒绝。
//   - 认不准就拒：身份缺失、身份未登记、权限未声明，三种情况都不放行。
//     「先放行再看日志」在处置结论这种有法定责任的地方是不能接受的。
//   - 越权即拒绝，且说清缺什么：403 的错误信息要点名当前身份、缺哪项权限、
//     以及哪几个身份有这项权限——一线人员据此知道该找谁签，不用来回猜。
package workflow

import (
	"sort"
	"strings"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// Role 是调用方的身份。
//
// 取值是四个固定字符串，与 spec「干系人与角色」表一一对应：
// 写进审计留痕与错误文案，出现在接口响应里，不做大小写变体。
type Role string

// 四类身份，外加一个「身份缺失」。
const (
	// RoleIngest：记录仪 / 院内边缘网关。
	//
	// 只可写入：投递温度事件、批量导入记录仪导出。它是唯一被允许往库里
	// 塞温度读数的角色——读数进来之后会直接进入越温判定与批次处置建议，
	// 所以这条通道只给机器，不给自然人。
	RoleIngest Role = "ingest"
	// RoleOperator：药学部冷链管理员（经办）。
	//
	// 日常跑判定的人：触发判定、查看窗口与归因、读在库批次、导出台账、
	// 维护点位温区配置。他不能确认处置结论——确认是主管的法定责任。
	RoleOperator Role = "operator"
	// RoleSupervisor：药学部主管（审核）。
	//
	// 唯一能确认或否决处置结论的身份。模型产出只到「待确认」为止，
	// 再往后必须由这个身份的动作推进。
	RoleSupervisor Role = "supervisor"
	// RoleAuditor：质控 / 院感（调阅）。
	//
	// 只读：查判定依据、步骤时间线、模型调用与确认动作记录。
	// 它不投递事件、不触发判定、不改配置、不确认。
	RoleAuditor Role = "auditor"
	// RoleUnknown：身份缺失或未登记。
	//
	// 它没有任何权限：按最保守处理，认不准就拒。
	RoleUnknown Role = ""
)

// Permission 是可授予的权限。
//
// 权限名按「能做什么动作」取，不按「哪个页面」取：
// 同一个动作在界面上可能在三个位置出现，权限只该有一份。
type Permission string

// 本服务的全部权限。
const (
	// PermEventIngest：投递温度事件 / 批量导入记录仪导出。
	PermEventIngest Permission = "event_ingest"
	// PermAssessTrigger：触发判定、发起重跑（离线回放）。
	PermAssessTrigger Permission = "assess_trigger"
	// PermRunRead：查判定结论、越温窗口、步骤时间线与审计链。
	PermRunRead Permission = "run_read"
	// PermLedgerRead：读冷链台账的在库批次与点位时序。
	//
	// 它与 PermRunRead 分开：批次号只由台账给，读批次与读判定结论
	// 是两件不同的事，权限也该分开写。
	PermLedgerRead Permission = "ledger_read"
	// PermConfirm：确认或否决处置结论。
	//
	// 只有药学部主管有这一项。它是整个权限矩阵里最重的一项：
	// 确认之后台账写回、接种门诊通知都会跟着落库。
	PermConfirm Permission = "confirm"
	// PermExport：导出判定台账与周报。
	PermExport Permission = "export"
	// PermProfileWrite：更新点位温区配置。
	PermProfileWrite Permission = "profile_write"
	// PermWritebackRetry：触发台账写回的补偿重放。
	//
	// 它与 PermConfirm 分开：确认是**判**一条越温的性质结论，
	// 补偿重放只是**把同一条结论再送一遍**——载荷、幂等键、external_ref
	// 全是上次那份，重放的人并没有对内容做任何新决定。
	// 所以经办（日常跑判定的人）拿得到它，而确认权仍然只在主管手上。
	PermWritebackRetry Permission = "writeback_retry"
)

// roleDescriptions 是四类身份的中文含义与岗位，用于错误文案与接口输出。
var roleDescriptions = map[Role]string{
	RoleIngest:     "记录仪 / 院内边缘网关",
	RoleOperator:   "药学部冷链管理员（经办）",
	RoleSupervisor: "药学部主管（审核）",
	RoleAuditor:    "质控 / 院感（调阅）",
}

// permissionDescriptions 是权限的中文含义。
var permissionDescriptions = map[Permission]string{
	PermEventIngest:    "投递温度事件",
	PermAssessTrigger:  "触发判定与重跑",
	PermRunRead:        "查看判定结论、窗口、时间线与审计链",
	PermLedgerRead:     "读取在库批次与冷链台账",
	PermConfirm:        "确认或否决处置结论",
	PermExport:         "导出判定台账与周报",
	PermProfileWrite:   "更新点位温区配置",
	PermWritebackRetry: "触发台账写回补偿重放",
}

// roleOrder 是身份的固定展示顺序（按权限由轻到重排，不按字母序）。
var roleOrder = []Role{
	RoleIngest,
	RoleOperator,
	RoleSupervisor,
	RoleAuditor,
}

// permissionOrder 是权限的固定展示顺序（按业务链路从前到后排）。
var permissionOrder = []Permission{
	PermEventIngest,
	PermAssessTrigger,
	PermRunRead,
	PermLedgerRead,
	PermConfirm,
	PermExport,
	PermProfileWrite,
	PermWritebackRetry,
}

// rolePermissions 是权限矩阵（白名单）。
//
// 四行四列都写在代码里而不是从配置读，是刻意的：
// 权限表是「谁负什么责任」的约定，不是运行期可调的东西。
// 把它做成配置等于让部署的人随手就能给自己开一条确认权——
// 那这条权限就没有意义了。
//
//   - ingest（机器）：只投递。其余一律没有：它不该看见在库批次，
//     更不该看见处置结论。
//   - operator（经办）：跑判定、读结论与台账、导出、改温区、补发待补偿写回；
//     唯独不能确认——确认权与经办权分开，避免「自己判自己签」。
//   - supervisor（主管）：经办能做的都能做，外加确认 / 否决。
//   - auditor（调阅）：只读。不投递、不触发、不改配置、不确认、不补发；
//     但能导出——导出是读动作，质控与院感要的就是这份东西。
var rolePermissions = map[Role]map[Permission]bool{
	RoleIngest: {
		PermEventIngest: true,
	},
	RoleOperator: {
		PermEventIngest:    true,
		PermAssessTrigger:  true,
		PermRunRead:        true,
		PermLedgerRead:     true,
		PermExport:         true,
		PermProfileWrite:   true,
		PermWritebackRetry: true,
	},
	RoleSupervisor: {
		PermEventIngest:    true,
		PermAssessTrigger:  true,
		PermRunRead:        true,
		PermLedgerRead:     true,
		PermConfirm:        true,
		PermExport:         true,
		PermProfileWrite:   true,
		PermWritebackRetry: true,
	},
	RoleAuditor: {
		PermRunRead:    true,
		PermLedgerRead: true,
		PermExport:     true,
	},
}

// Roles 返回全部四类身份，按固定顺序。
func Roles() []Role {
	out := make([]Role, len(roleOrder))
	copy(out, roleOrder)
	return out
}

// Permissions 返回全部权限，按固定顺序。
func Permissions() []Permission {
	out := make([]Permission, len(permissionOrder))
	copy(out, permissionOrder)
	return out
}

// KnownRole 判断身份是否已登记。
func KnownRole(role Role) bool {
	_, ok := roleDescriptions[role]
	return ok
}

// DescribeRole 返回身份的中文含义与岗位；未登记的身份返回兜底文案。
func DescribeRole(role Role) string {
	if desc, ok := roleDescriptions[role]; ok {
		return desc
	}
	return "身份未登记"
}

// KnownPermission 判断权限是否已登记。
func KnownPermission(perm Permission) bool {
	_, ok := permissionDescriptions[perm]
	return ok
}

// DescribePermission 返回权限的中文含义；未登记的返回兜底文案。
func DescribePermission(perm Permission) string {
	if desc, ok := permissionDescriptions[perm]; ok {
		return desc
	}
	return "权限未登记"
}

// ParseRole 解析一个身份字符串；未登记的取值返回 RoleUnknown。
//
// 它给「请求头里的角色声明」用：声明值必须能落到四类身份之一，
// 否则按身份缺失处理——声明不能自证身份，更不能自己造一个身份。
func ParseRole(raw string) Role {
	candidate := Role(strings.TrimSpace(raw))
	if KnownRole(candidate) {
		return candidate
	}
	return RoleUnknown
}

// Allows 判断某个身份是否持有某项权限。
//
// 身份缺失（RoleUnknown）或权限未登记时一律返回 false：
// 「不知道」不等于「有」，也不等于「没有」——按没有处理。
func Allows(role Role, perm Permission) bool {
	if !KnownRole(role) || !KnownPermission(perm) {
		return false
	}
	return rolePermissions[role][perm]
}

// PermissionsOf 返回某个身份持有的权限列表（副本，按固定顺序）。
func PermissionsOf(role Role) []Permission {
	out := make([]Permission, 0, len(permissionOrder))
	for _, perm := range permissionOrder {
		if Allows(role, perm) {
			out = append(out, perm)
		}
	}
	return out
}

// RoleHolder 是「身份 + 中文岗位」的展示对，用于错误文案。
type RoleHolder struct {
	// Role 是身份标识。
	Role Role `json:"role"`
	// Label 是身份的中文岗位。
	Label string `json:"label"`
}

// RolesWith 返回持有某项权限的身份列表（副本，按固定顺序）。
func RolesWith(perm Permission) []RoleHolder {
	out := make([]RoleHolder, 0, len(roleOrder))
	for _, role := range roleOrder {
		if Allows(role, perm) {
			out = append(out, RoleHolder{Role: role, Label: roleDescriptions[role]})
		}
	}
	return out
}

// Authorize 判断某个身份能否执行某项权限，不能则返回 E_ROLE_REQUIRED。
//
// 这是权限闸门唯一的判定入口：HTTP 层、CLI 层将来都调它，
// 判定口径只有一处，不会出现「接口挡了、批处理没挡」的缝。
//
// 错误文案要点名三样东西：当前身份是谁、缺哪项权限、哪几个身份有这项权限。
// 一线人员看到「只有 药学部主管（审核） 可执行」就知道该找谁签，
// 不用回去翻文档。
func Authorize(role Role, perm Permission) error {
	if Allows(role, perm) {
		return nil
	}

	permLabel := DescribePermission(perm)
	if !KnownPermission(perm) {
		return errcode.Newf(errcode.RoleRequired,
			"该操作要求的权限 %q 未登记，按最保守处理一律拒绝", string(perm))
	}
	if !KnownRole(role) {
		return errcode.Newf(errcode.RoleRequired,
			"当前身份未登记（%q），无权执行「%s」：认不准就拒，该操作需要由已登记身份发起",
			string(role), permLabel)
	}

	holders := RolesWith(perm)
	labels := make([]string, 0, len(holders))
	for _, holder := range holders {
		labels = append(labels, holder.Label)
	}
	holder := "无"
	if len(labels) > 0 {
		holder = strings.Join(labels, "、")
	}
	return errcode.Newf(errcode.RoleRequired,
		"当前身份「%s」无权执行「%s」：该操作需要「%s」权限，持该权限的身份为 %s",
		roleDescriptions[role], permLabel, permLabel, holder)
}

// PermissionMatrix 返回权限矩阵的快照，供接口与文档展示。
//
// 键是身份，值是该身份持有的权限标识列表（按固定顺序）。
func PermissionMatrix() map[string][]string {
	out := make(map[string][]string, len(roleOrder))
	for _, role := range roleOrder {
		perms := PermissionsOf(role)
		names := make([]string, 0, len(perms))
		for _, perm := range perms {
			names = append(names, string(perm))
		}
		out[string(role)] = names
	}
	return out
}

// SortedRoleNames 返回排序后的身份标识列表，用于稳定输出与测试比对。
func SortedRoleNames() []string {
	names := make([]string, 0, len(roleOrder))
	for _, role := range roleOrder {
		names = append(names, string(role))
	}
	sort.Strings(names)
	return names
}
