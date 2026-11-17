package drafting

import (
	"strings"

	"github.com/chaosforge/coldchain-temp-assessment/internal/disposition"
)

// 本文件落「主管确认之后，那张记录表上多出什么」。
//
// 为什么要在稿子末尾补一段，而不是重新渲染整张表：成稿正文是**草**，
// 它的每一行都对应判定链算出来的一个事实（点位、峰值、持续分钟、原因、证据）。
// 确认动作只补两样东西——最终处置结论与主管的意见——不改动任何判定事实。
// 重新渲染整张表会把「草稿」变成「定稿」，而稿子里那句
// 「处置建议：隔离（isolate）（最终处置待药学部主管确认后确定）」
// 恰恰是提醒收件人「这里还要人签字」的一句话，去掉它就等于替主管签了字。
//
// 所以做法是：原稿一字不动，末尾补一段「人工确认」，
// 由收件人自己看两段——上半段是机器判的，下半段是人定的。

// confirmationSectionTitle 是补在稿子末尾的小节标题。
//
// 序号接在成稿渲染器的「七、人工复核」之后：那张复核栏是留给主管手写的，
// 这一段是系统写下的确认结果，两者合起来才是完整的复核记录。
const confirmationSectionTitle = "八、人工确认（药学部主管）"

// AppendConfirmationBlock 在记录表正文末尾补上人工确认小节。
//
// 补的内容只有四行：确认人、确认时间、最终处置结论、主管意见。
// 处置结论带中文标签（隔离 / 放行 / 报损 / 待疫苗生产企业评估），
// 与台账里写的取值一一对应——台账按英文码统计，稿子给人看中文。
//
// 稿子为空时原样返回空串：没有稿子就没有可补的确认栏，
// 不凭空造出一张只有确认记录的表。
func AppendConfirmationBlock(draftText string, facts ConfirmFacts) string {
	if strings.TrimSpace(draftText) == "" {
		return ""
	}

	var b strings.Builder
	b.WriteString(strings.TrimRight(draftText, "\n"))
	b.WriteString("\n\n")
	b.WriteString(confirmationSectionTitle)
	b.WriteString("\n")
	b.WriteString("    确认人：" + orPending(strings.TrimSpace(facts.ConfirmedBy)))
	b.WriteString("\n")
	b.WriteString("    确认时间：" + formatTime(facts.ConfirmedAt))
	b.WriteString("\n")

	dispositionText := strings.TrimSpace(facts.Disposition)
	if label, ok := disposition.Label(dispositionText); ok {
		dispositionText = label + "（" + facts.Disposition + "）"
	}
	b.WriteString("    最终处置结论：" + orPending(dispositionText))
	b.WriteString("\n")
	b.WriteString("    主管意见：" + orPending(strings.TrimSpace(facts.Opinion)))
	b.WriteString("\n")
	return b.String()
}

// orPending 把空值写成「待补」——空栏在这张表上读起来就是「漏了」。
func orPending(value string) string {
	if strings.TrimSpace(value) == "" {
		return PendingText
	}
	return value
}
