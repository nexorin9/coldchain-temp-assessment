// Package disposition 放「处置结论」的取值域与中文标签。
//
// 为什么它自己站一个包：处置结论的取值域是业务词汇，同时被三处用到——
// 模型层的结构化输出（枚举约束）、成稿层的建议与写回载荷、确认层的取值校验。
// 这三处都在同一条业务链上，但谁也不该是谁的下游：模型层不该知道成稿怎么写，
// 成稿层不该知道确认怎么校验。
//
// 放在 workflow 里会让模型层反过来依赖状态机；放在 domain 里会让 workflow
// 反过来依赖 domain（domain 依赖 workflow，会成环）。所以它自己站一个叶子包：
// 谁都能引，环也构不起来，取值域也只有这一份。
//
// 一条纪律：**系统只给建议，不给结论**。
// 「放行」与「报损」是人工决定事项——它们在取值域里，但只能由药学部主管
// 在确认动作里给出；系统作为建议给出时会被压下来并留痕。
package disposition

import (
	"strings"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// 处置结论的取值域。
//
// 取值是四个固定字符串，写进台账、出现在接口响应与审计链里，
// 不做大小写变体、不自造新值——台账侧按这些值统计处置分布，
// 多一个写法就多一份对不上账的记录。
const (
	// Isolate：隔离。
	//
	// 越温后最常见的处置：先把货隔离，等疫苗生产企业评估质量影响。
	Isolate = "isolate"
	// Release：放行。
	//
	// 只有药学部主管在确认动作里给出才算成立；系统不作为建议给出。
	Release = "release"
	// Scrap：报损。
	//
	// 同样属于人工决定事项：报损是不可逆的处置，不能由系统建议触发。
	Scrap = "scrap"
	// PendingEvaluation：待疫苗生产企业评估质量影响（默认取值）。
	//
	// 它是默认值而不是「暂缺」：批次到底能不能用由疫苗生产企业评估质量影响、
	// 再由药学部主管确认决定，在那之前如实写明「还没定」。
	PendingEvaluation = "pending_manufacturer_evaluation"
)

// labels 是取值到中文短标签的映射。
var labels = map[string]string{
	Isolate:           "隔离",
	Release:           "放行",
	Scrap:             "报损",
	PendingEvaluation: "待疫苗生产企业评估",
}

// order 是取值的固定展示顺序：从严到宽，与「先隔离、再评估」的业务顺序一致。
var order = []string{Isolate, PendingEvaluation, Scrap, Release}

// Values 返回全部处置结论取值，按固定顺序。
func Values() []string {
	out := make([]string, len(order))
	copy(out, order)
	return out
}

// Label 取处置结论的中文短标签；未登记的取值返回 false。
func Label(code string) (string, bool) {
	label, ok := labels[strings.TrimSpace(code)]
	return label, ok
}

// Validate 校验处置结论取值。
//
// 取值域之外一律拒绝（E_VALIDATION），并且把合法取值一并写进错误信息：
// 调用方看到「不合法：release_now（合法取值：isolate、…）」就知道该填什么，
// 不用回去翻文档。
func Validate(code string) error {
	trimmed := strings.TrimSpace(code)
	if _, ok := labels[trimmed]; ok {
		return nil
	}
	return errcode.Newf(errcode.Validation,
		"处置结论取值不合法：%q（合法取值：%s）", code, strings.Join(Values(), "、"))
}

// IsManualOnly 判断某个取值是否只允许由人工确认给出。
//
// 「放行」与「报损」是人工决定事项：系统给这两条建议时会被压下来并留痕
// （见成稿层的处置建议）。确认动作不受此限——主管确认时给什么就是什么。
func IsManualOnly(code string) bool {
	switch strings.TrimSpace(code) {
	case Release, Scrap:
		return true
	default:
		return false
	}
}
