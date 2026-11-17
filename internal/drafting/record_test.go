package drafting

import (
	"strings"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

var fixedNow = time.Date(2026, 10, 26, 5, 20, 0, 0, time.FixedZone("CST", 8*3600))

// sampleInputs 造一份脱敏的判定产物：越温窗口 + 开门归因 + 在库批次。
//
// 批次号用占位值（<EXAMPLE_LOT-…>），点位与设备名也用占位标识，
// 不含任何真实医院、真实批号或患者信息。
func sampleInputs() Inputs {
	start := time.Date(2026, 10, 20, 3, 10, 0, 0, time.FixedZone("CST", 8*3600))
	end := time.Date(2026, 10, 20, 3, 25, 0, 0, time.FixedZone("CST", 8*3600))
	peak := time.Date(2026, 10, 20, 3, 15, 0, 0, time.FixedZone("CST", 8*3600))

	samples := []domain.TemperatureEvent{
		{
			PointID:      "point-vax-freezer-01",
			DeviceID:     "recorder-vax-01",
			DeviceName:   "疫苗冷藏箱-01号",
			MeasuredAt:   start,
			TemperatureC: 10.2,
			Source:       "recorder",
			Events:       []string{"door_open"},
		},
		{
			PointID:      "point-vax-freezer-01",
			DeviceID:     "recorder-vax-01",
			DeviceName:   "疫苗冷藏箱-01号",
			MeasuredAt:   peak,
			TemperatureC: 12.4,
			Source:       "recorder",
		},
		{
			PointID:      "point-vax-freezer-01",
			DeviceID:     "recorder-vax-01",
			DeviceName:   "疫苗冷藏箱-01号",
			MeasuredAt:   end,
			TemperatureC: 9.8,
			Source:       "recorder",
		},
	}

	window := domain.ExcursionWindow{
		Start:               start,
		End:                 end,
		DurationMinutes:     15,
		PeakC:               12.4,
		PeakAt:              peak,
		Zone:                "2~8℃",
		SampleCount:         4,
		HysteresisC:         1,
		OutOfRangeSamples:   4,
		MinExcursionMinutes: 10,
		SamplingIntervalSec: 300,
		IntervalSource:      domain.IntervalSourcePointProfile,
		InferredIntervalSec: 300,
		SegmentCount:        1,
	}

	attribution := &domain.AttributionPayload{
		CauseCode:  "door_open",
		CauseLabel: "开门",
		Confidence: 0.9,
		Evidence: []domain.AttributionEvidence{
			{Feature: "door_open_minutes", Value: 10, Threshold: 5, Note: "累计开门时长"},
			{Feature: "door_to_rise_delay_minutes", Value: 0, Threshold: 30, Note: "开门到温升起点滞后"},
		},
	}

	record := &domain.ExceptionRecordPayload{
		PointID:            "point-vax-freezer-01",
		DeviceName:         "疫苗冷藏箱-01号",
		ExcursionStartedAt: start,
		ExcursionEndedAt:   end,
		PeakTemperatureC:   12.4,
		DurationMinutes:    15,
		Zone:               "2~8℃",
		CauseConclusion:    "door_open",
		Evidence:           attribution.Evidence,
		NeedsReview:        true,
		MissingFields:      []string{"batches", "disposition"},
		GeneratedAt:        fixedNow,
	}

	return Inputs{
		Record: record,
		Series: &domain.SeriesPayload{
			PointID:     "point-vax-freezer-01",
			From:        start.Add(-5 * time.Minute),
			To:          end.Add(5 * time.Minute),
			SampleCount: 4,
			Samples:     samples,
		},
		Window: &domain.WindowPayload{
			Window:     window,
			ZoneMinC:   2,
			ZoneMaxC:   8,
			ZoneSource: domain.ZoneSourcePointProfile,
			ZoneBasis:  "点位温区配置 point-vax-freezer-01",
		},
		Attribution: attribution,
	}
}

func render(t *testing.T, in Inputs) Draft {
	t.Helper()
	draft, err := Render(in, Options{Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatalf("Render 返回错误：%v", err)
	}
	return draft
}

// TestRender_NormalDraftHoldsFourKeyElements 覆盖「正常成稿」：
// 点位、峰值、持续分钟、原因结论四项关键要素都要在稿子里。
func TestRender_NormalDraftHoldsFourKeyElements(t *testing.T) {
	draft := render(t, sampleInputs())
	text := draft.Text

	want := []string{
		"point-vax-freezer-01",
		"12.4 ℃",
		"15 分钟",
		"开门",
	}
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("成稿缺少关键要素 %q\n成稿正文：\n%s", w, text)
		}
	}
	if !strings.Contains(text, "2026-10-20 03:10（+08:00）") {
		t.Errorf("成稿缺少越限起始时间（应为 YYYY-MM-DD HH:mm 加时区偏移）\n%s", text)
	}
	if !strings.Contains(text, "2~8℃") {
		t.Errorf("成稿缺少所属温区\n%s", text)
	}
}

// TestRender_EvidenceCitesFeatureValueAndThreshold 覆盖「证据引用完整」：
// 每条证据都要带特征名、数值与阈值，缺一个就不成立。
func TestRender_EvidenceCitesFeatureValueAndThreshold(t *testing.T) {
	draft := render(t, sampleInputs())
	text := draft.Text

	if !strings.Contains(text, "door_open_minutes = 10（阈值 5）") {
		t.Errorf("证据未按「特征名 = 数值（阈值 阈值值）」引用\n%s", text)
	}
	if !strings.Contains(text, "door_to_rise_delay_minutes = 0（阈值 30）") {
		t.Errorf("证据缺少第二条（数值与阈值都要在）\n%s", text)
	}
	if !strings.Contains(text, "累计开门时长") {
		t.Errorf("证据说明未随证据一起呈现\n%s", text)
	}
}

// TestRender_HeaderDeclaresDraftPurposeAndReviewSlot 覆盖「头部复核声明存在」：
// 稿头写明用途，稿尾留人工复核位。
func TestRender_HeaderDeclaresDraftPurposeAndReviewSlot(t *testing.T) {
	draft := render(t, sampleInputs())
	text := draft.Text

	for _, w := range []string{
		"疫苗储存和运输温度异常情况记录表",
		"（草稿）",
		"属草稿",
		"须经药学部主管复核并提交疫苗生产企业评估",
		"本表不构成批次放行结论",
		"人工复核（须由药学部主管填写）",
		"复核人（药学部主管）",
		"复核日期",
		"送疫苗生产企业评估",
	} {
		if !strings.Contains(text, w) {
			t.Errorf("成稿缺少必需声明 %q\n%s", w, text)
		}
	}
}

// TestRender_EveryFieldTracesToDomainObject 覆盖「模板字段与数据一一对应」：
// 每个栏目都要有取值出处，且出处必须指得进领域对象。
func TestRender_EveryFieldTracesToDomainObject(t *testing.T) {
	draft := render(t, sampleInputs())
	if len(draft.Fields) == 0 {
		t.Fatal("成稿没有给出任何栏目的取值出处")
	}
	for _, f := range draft.Fields {
		if strings.TrimSpace(f.Source) == "" {
			t.Errorf("栏目 %q 没有取值出处", f.Label)
		}
		if !strings.Contains(f.Source, "domain.") {
			t.Errorf("栏目 %q 的取值出处 %q 不是领域对象路径", f.Label, f.Source)
		}
		if strings.TrimSpace(f.Value) == "" {
			t.Errorf("栏目 %q 的取值为空", f.Label)
		}
	}
	// 出处集合里必须覆盖到四类关键栏目。
	sources := map[string]bool{}
	for _, f := range draft.Fields {
		sources[f.Source] = true
	}
	for _, want := range []string{
		"domain.ExceptionRecordPayload.PointID",
		"domain.ExceptionRecordPayload.PeakTemperatureC",
		"domain.ExceptionRecordPayload.DurationMinutes",
		"domain.AttributionPayload.CauseCode/CauseLabel",
	} {
		if !sources[want] {
			t.Errorf("成稿没有 %q 这个栏目的出处", want)
		}
	}
}

// TestRender_MissingBatchesAndDispositionAreNotFabricated 覆盖「不出现无来源的数值」：
// 批次与处置拿不到时写「待补」，绝不编一个像样的值。
func TestRender_MissingBatchesAndDispositionAreNotFabricated(t *testing.T) {
	in := sampleInputs()
	draft := render(t, in)
	text := draft.Text

	if !strings.Contains(text, PendingText) {
		t.Errorf("缺项没有写「%s」\n%s", PendingText, text)
	}
	if !strings.Contains(text, "批次号只能由冷链台账提供，系统不生成批次号") {
		t.Errorf("缺批次时未写明批次号的来源约束\n%s", text)
	}
	if strings.Contains(text, "<EXAMPLE_LOT") {
		t.Errorf("成稿凭空造出了批次号\n%s", text)
	}
	for _, forbidden := range []string{"可放心使用", "建议放行", "同意放行"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("成稿出现越权措辞 %q\n%s", forbidden, text)
		}
	}
}

// TestRender_BatchesRenderedVerbatim 覆盖「涉及批次」：
// 台账给的批次号原样出现在稿子里，不被改写。
func TestRender_BatchesRenderedVerbatim(t *testing.T) {
	in := sampleInputs()
	in.Record.Batches = []domain.Batch{
		{PointID: "point-vax-freezer-01", BatchRef: "<EXAMPLE_LOT-01>", ProductName: "流感疫苗", Quantity: 100, ExpiryDate: "2026-12-31"},
	}
	draft := render(t, in)
	text := draft.Text

	if !strings.Contains(text, "<EXAMPLE_LOT-01>") {
		t.Errorf("成稿未原样写出批次号\n%s", text)
	}
	if !strings.Contains(text, "流感疫苗") || !strings.Contains(text, "2026-12-31") {
		t.Errorf("成稿缺少批次品名或有效期\n%s", text)
	}
}

// TestRender_DispositionRenderedWithLabel 覆盖「处置建议」：
// 有处置取值时写出中文短标签与取值代码。
func TestRender_DispositionRenderedWithLabel(t *testing.T) {
	in := sampleInputs()
	in.Record.Disposition = "pending_manufacturer_evaluation"
	in.Record.DispositionNote = "建议隔离并送疫苗生产企业评估"
	draft := render(t, in)
	text := draft.Text

	if !strings.Contains(text, "待疫苗生产企业评估（pending_manufacturer_evaluation）") {
		t.Errorf("成稿未写出处置结论的中文标签与取值\n%s", text)
	}
	if !strings.Contains(text, "建议隔离并送疫苗生产企业评估") {
		t.Errorf("成稿缺少处置说明\n%s", text)
	}
}

// TestRender_NotesCarriedThrough 覆盖「口径说明」：
// 记录表载荷上的口径说明（如降级说明）要出现在稿子里。
func TestRender_NotesCarriedThrough(t *testing.T) {
	in := sampleInputs()
	in.Record.Notes = []string{"本次归因结论来自规则基线，请重点核对"}
	draft := render(t, in)
	text := draft.Text

	if !strings.Contains(text, "口径说明") {
		t.Errorf("成稿缺少口径说明小节\n%s", text)
	}
	if !strings.Contains(text, "本次归因结论来自规则基线，请重点核对") {
		t.Errorf("成稿未写出口径说明正文\n%s", text)
	}
}

// TestRender_DataQualityFactsDisclosed 覆盖「数据质量提示」：
// 缺口、回拨、探头卡死这些事实要在稿子上说清楚。
func TestRender_DataQualityFactsDisclosed(t *testing.T) {
	in := sampleInputs()
	in.Window.Window.BreakpointCount = 1
	in.Window.Window.DataGaps = []domain.DataGap{
		{GapMinutes: 40, MissingSamples: 7},
	}
	draft := render(t, in)
	text := draft.Text

	if !strings.Contains(text, "数据质量提示") {
		t.Errorf("成稿缺少数据质量提示\n%s", text)
	}
	if !strings.Contains(text, "1 处数据缺口") {
		t.Errorf("成稿未写出数据缺口数量\n%s", text)
	}
}

// TestRender_MissingRecordRejected 覆盖失败路径：
// 没有记录表载荷就不给稿子，不返回空壳表格。
func TestRender_MissingRecordRejected(t *testing.T) {
	_, err := Render(Inputs{}, Options{})
	if err == nil {
		t.Fatal("缺少记录表载荷时应当报错")
	}
	if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("错误码不对：%v", code)
	}
	if !strings.Contains(err.Error(), "记录表载荷") {
		t.Errorf("错误信息未点名缺什么：%v", err)
	}
}

// TestRender_GeneratedAtComesFromPayload 覆盖生成时间口径：
// 载荷里已有生成时间时以载荷为准，不拿渲染这一刻顶替。
func TestRender_GeneratedAtComesFromPayload(t *testing.T) {
	in := sampleInputs()
	in.Record.GeneratedAt = time.Date(2026, 10, 21, 9, 5, 0, 0, time.FixedZone("CST", 8*3600))
	draft := render(t, in)

	if !strings.Contains(draft.Text, "2026-10-21 09:05（+08:00）") {
		t.Errorf("生成时间未取自记录表载荷\n%s", draft.Text)
	}
	if strings.Contains(draft.Text, "2026-10-26 05:20") {
		t.Errorf("生成时间被渲染时钟顶替了\n%s", draft.Text)
	}
}
