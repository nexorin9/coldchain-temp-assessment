package drafting

import (
	"strings"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// confirmTestAt 是确认小节用的固定时刻。
var confirmTestAt = time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)

// TestAppendConfirmationBlock_AddsFinalDisposition 覆盖确认小节：
// 稿子末尾补上确认人、确认时间、最终处置结论与主管意见，
// 原稿一字不动——草稿里那句「最终处置待药学部主管确认后确定」仍在。
func TestAppendConfirmationBlock_AddsFinalDisposition(t *testing.T) {
	draft := "疫苗储存和运输温度异常情况记录表（草稿）\n\n一、监测点位与设备\n    点位编号：point-vax-freezer-01\n\n七、人工复核（须由药学部主管填写）\n    复核人（药学部主管）：____________________"

	out := AppendConfirmationBlock(draft, ConfirmFacts{
		Disposition: "isolate",
		ConfirmedBy: "supervisor-007",
		ConfirmedAt: confirmTestAt,
		Opinion:     "已联系疫苗生产企业评估，先隔离",
	})

	for _, want := range []string{
		"七、人工复核（须由药学部主管填写）",
		"八、人工确认（药学部主管）",
		"确认人：supervisor-007",
		"确认时间：2026-10-20 11:00",
		"最终处置结论：隔离（isolate）",
		"主管意见：已联系疫苗生产企业评估，先隔离",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("确认小节缺少 %q\n%s", want, out)
		}
	}
	// 原稿一字不动：草稿是草稿，确认只是补一段。
	if !strings.Contains(out, "复核人（药学部主管）：____________________") {
		t.Error("确认小节不应改动原稿的人工复核栏")
	}
}

// TestAppendConfirmationBlock_EmptyDraftStaysEmpty 覆盖「没有稿子就没有可补的栏」：
// 空稿原样返回，不凭空造出一张只有确认记录的表。
func TestAppendConfirmationBlock_EmptyDraftStaysEmpty(t *testing.T) {
	if out := AppendConfirmationBlock("  ", ConfirmFacts{Disposition: "isolate"}); out != "" {
		t.Errorf("空稿不应被补出确认小节，实际：%q", out)
	}
}

// TestAppendConfirmationBlock_MissingValuesShowPending 覆盖缺项：
// 确认人或意见没给时写「待补」——空栏在这张表上读起来就是「漏了」。
func TestAppendConfirmationBlock_MissingValuesShowPending(t *testing.T) {
	out := AppendConfirmationBlock("正文", ConfirmFacts{Disposition: "isolate"})
	if !strings.Contains(out, "确认人："+PendingText) {
		t.Errorf("缺确认人时应写「%s」\n%s", PendingText, out)
	}
	if !strings.Contains(out, "主管意见："+PendingText) {
		t.Errorf("缺意见时应写「%s」\n%s", PendingText, out)
	}
}

// TestBuildConfirmedWriteback_CarriesConfirmFacts 覆盖确认载荷的组装：
// 处置以确认时的结论为准，外部标识与幂等键由 run 给出。
func TestBuildConfirmedWriteback_CarriesConfirmFacts(t *testing.T) {
	in := completeInputs()
	in.Record.DraftText = render(t, in).Text
	quality := CheckQuality(in, confirmTestAt)
	in.Record.Quality = quality.Domain()

	source, err := BuildConfirmedWriteback(in.Record, ConfirmFacts{
		Disposition:    "isolate",
		ConfirmedBy:    "supervisor-007",
		ConfirmedAt:    confirmTestAt,
		ExternalRef:    "cc-point-vax-freezer-01-20261019T191000Z",
		IdempotencyKey: "54b35c7f136371619ea6a5313714d1ac3d4173e3f835bcc97eb1087e6f7dddae",
	})
	if err != nil {
		t.Fatalf("确认后应当能组装出台账写回载荷：%v", err)
	}
	if source.Disposition != "isolate" {
		t.Errorf("载荷里的处置结论不对：%q", source.Disposition)
	}
	if source.ConfirmedBy != "supervisor-007" {
		t.Errorf("载荷里的确认人不对：%q", source.ConfirmedBy)
	}
	if !source.ConfirmedAt.Equal(confirmTestAt) {
		t.Errorf("载荷里的确认时刻不对：%v", source.ConfirmedAt)
	}
	if source.ExternalRef != "cc-point-vax-freezer-01-20261019T191000Z" {
		t.Errorf("载荷里的外部引用不对：%q", source.ExternalRef)
	}
	if source.IdempotencyKey == "" {
		t.Error("载荷里应当带幂等键")
	}
	if source.PointID != in.Record.PointID || source.PeakTemperatureC != in.Record.PeakTemperatureC {
		t.Errorf("载荷里的判定事实被改动了：%+v", source)
	}
}

// TestBuildConfirmedWriteback_RefusesUnqualifiedDraft 覆盖「不合格成稿不得进写回载荷」：
// 必备要素缺项时确认也不放行——确认不是绕过质量闸门的口子。
func TestBuildConfirmedWriteback_RefusesUnqualifiedDraft(t *testing.T) {
	in := completeInputs()
	in.Record.Zone = ""
	quality := CheckQuality(in, confirmTestAt)
	in.Record.Quality = quality.Domain()

	_, err := BuildConfirmedWriteback(in.Record, ConfirmFacts{Disposition: "isolate"})
	if err == nil {
		t.Fatal("要素不全的成稿不得组装出台账写回载荷")
	}
	if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("错误码不对：%v", err)
	}
	if !strings.Contains(err.Error(), "zone") {
		t.Errorf("错误信息未点名缺项：%v", err)
	}
}
