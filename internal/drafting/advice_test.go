package drafting

import (
	"strings"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/model"
)

// adviceInputs 造一份脱敏的判定产物，用来算批次可用性建议。
//
// 三个在库批次刻意分成三种情形，这样「逐批次分别给建议」才看得出来：
//
//   - <EXAMPLE_LOT-01>：疫苗（脱敏样例），温区 2~8℃，峰值 9.6℃ 越出上限 → 受影响；
//   - <EXAMPLE_LOT-02>：冷藏药品（脱敏样例），温区 0~10℃，峰值 9.6℃ 未越上限 → 未受影响；
//   - <EXAMPLE_LOT-03>：制品温区未在点位配置中登记 → 判不了。
//
// 批次号、点位与制品名一律用占位标识，不含任何真实医院、真实批号或患者信息。
func adviceInputs() AdviceInputs {
	start := time.Date(2026, 10, 20, 3, 10, 0, 0, time.FixedZone("CST", 8*3600))
	end := time.Date(2026, 10, 20, 3, 25, 0, 0, time.FixedZone("CST", 8*3600))
	peak := time.Date(2026, 10, 20, 3, 15, 0, 0, time.FixedZone("CST", 8*3600))

	window := &domain.WindowPayload{
		Window: domain.ExcursionWindow{
			Start:               start,
			End:                 end,
			DurationMinutes:     15,
			PeakC:               9.6,
			PeakAt:              peak,
			Zone:                "2~8℃",
			SampleCount:         4,
			HysteresisC:         1,
			OutOfRangeSamples:   4,
			MinExcursionMinutes: 10,
			SamplingIntervalSec: 300,
			IntervalSource:      domain.IntervalSourcePointProfile,
			SegmentCount:        1,
		},
		ZoneMinC:   2,
		ZoneMaxC:   8,
		ZoneSource: domain.ZoneSourcePointProfile,
		ZoneBasis:  "点位温区配置 point-vax-freezer-01",
		Products: []domain.ZoneProductDecision{
			{
				ProductName: "疫苗（脱敏样例）",
				ZoneMinC:    2,
				ZoneMaxC:    8,
				ZoneLabel:   "2~8℃",
				ZoneSource:  domain.ZoneSourceProductManual,
				ZoneBasis:   "点位温区配置 products[] 中制品 疫苗（脱敏样例） 的条目",
				BatchRefs:   []string{"<EXAMPLE_LOT-01>"},
			},
			{
				ProductName: "冷藏药品（脱敏样例）",
				ZoneMinC:    0,
				ZoneMaxC:    10,
				ZoneLabel:   "0~10℃",
				ZoneSource:  domain.ZoneSourceProductManual,
				ZoneBasis:   "点位温区配置 products[] 中制品 冷藏药品（脱敏样例） 的条目",
				BatchRefs:   []string{"<EXAMPLE_LOT-02>"},
			},
		},
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
		PeakTemperatureC:   9.6,
		DurationMinutes:    15,
		Zone:               "2~8℃",
		CauseConclusion:    "door_open",
		Evidence:           attribution.Evidence,
		Batches: []domain.Batch{
			{PointID: "point-vax-freezer-01", BatchRef: "<EXAMPLE_LOT-01>", ProductName: "疫苗（脱敏样例）", Quantity: 120, ExpiryDate: "<EXAMPLE_EXPIRY-A>"},
			{PointID: "point-vax-freezer-01", BatchRef: "<EXAMPLE_LOT-02>", ProductName: "冷藏药品（脱敏样例）", Quantity: 48, ExpiryDate: "<EXAMPLE_EXPIRY-C>"},
			{PointID: "point-vax-freezer-01", BatchRef: "<EXAMPLE_LOT-03>", ProductName: "生物制品（温区未登记样例）", Quantity: 30, ExpiryDate: "<EXAMPLE_EXPIRY-D>"},
		},
		GeneratedAt: fixedNow,
	}

	return AdviceInputs{
		Record:      record,
		Window:      window,
		Attribution: attribution,
		PointID:     "point-vax-freezer-01",
	}
}

func advise(t *testing.T, in AdviceInputs) domain.DispositionAdvice {
	t.Helper()
	out, err := Advise(in)
	if err != nil {
		t.Fatalf("Advise 返回错误：%v", err)
	}
	return out
}

// TestAdvise_DefaultDispositionIsPendingManufacturerEvaluation 覆盖「默认处置为待企业评估」：
// 没有依据时，系统给的动作是「等着企业评估」，不是自己下结论。
func TestAdvise_DefaultDispositionIsPendingManufacturerEvaluation(t *testing.T) {
	in := adviceInputs()
	in.Record.Batches = nil

	out := advise(t, in)
	if out.Disposition != model.DispositionPendingEvaluation {
		t.Fatalf("默认处置应为 %q，实际 %q", model.DispositionPendingEvaluation, out.Disposition)
	}
	if !strings.Contains(out.Note, "未取得在库批次清单") {
		t.Errorf("处置说明未写明批次清单缺失：%s", out.Note)
	}
	if len(out.BatchAdvices) != 0 {
		t.Errorf("没有批次时不应给出逐批次建议，实际 %d 条", len(out.BatchAdvices))
	}
}

// TestValidateDisposition_ValueDomain 覆盖取值域：
// 四个合法取值都放行，域外取值一律拒绝（处置写错一个字，批次就可能被误放行）。
func TestValidateDisposition_ValueDomain(t *testing.T) {
	for _, code := range model.DispositionValues() {
		if err := ValidateDisposition(code); err != nil {
			t.Errorf("合法取值 %q 被拒：%v", code, err)
		}
	}
	if err := ValidateDisposition(""); err != nil {
		t.Errorf("空取值应当视为未指定而不是报错：%v", err)
	}
	for _, code := range []string{"release_now", "放行", "PENDING", "isolate_now"} {
		err := ValidateDisposition(code)
		if err == nil {
			t.Errorf("取值域外的 %q 应当被拒绝", code)
			continue
		}
		if errcode.As(err).Code != errcode.Validation {
			t.Errorf("取值域外的 %q 错误码应为 E_VALIDATION，实际 %s", code, errcode.As(err).Code)
		}
		if !strings.Contains(err.Error(), code) {
			t.Errorf("错误信息未点名越界取值 %q：%v", code, err)
		}
	}
}

// TestAdvise_OutOfDomainDispositionRejected 覆盖「取值域越界被拒」：
// 上游给了域外的处置结论，整条建议直接报错，不落一个「看着像」的结论。
func TestAdvise_OutOfDomainDispositionRejected(t *testing.T) {
	in := adviceInputs()
	in.RequestedDisposition = "release_now"

	_, err := Advise(in)
	if err == nil {
		t.Fatal("取值域外的处置结论应当被拒绝")
	}
	if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("错误码应为 E_VALIDATION，实际 %s", code)
	}
	if !strings.Contains(err.Error(), "release_now") {
		t.Errorf("错误信息未点名越界取值：%v", err)
	}
}

// TestAdvise_OverreachWordingFiltered 覆盖「越权措辞被过滤」：
// 上游说明里的「可放心使用」「质量不受影响」被挡下来，并留下过滤痕迹。
func TestAdvise_OverreachWordingFiltered(t *testing.T) {
	in := adviceInputs()
	in.RequestedNote = "该批次可放心使用，本次温度偏离不影响质量"

	out := advise(t, in)
	for _, forbidden := range []string{"可放心使用", "不影响质量"} {
		if strings.Contains(out.Note, forbidden) {
			t.Errorf("越权措辞 %q 出现在处置说明里：%s", forbidden, out.Note)
		}
	}
	if !strings.Contains(out.Note, OverreachReplacement) {
		t.Errorf("过滤后没有留下标记：%s", out.Note)
	}
	joined := strings.Join(out.FilteredPhrases, "、")
	for _, want := range []string{"可放心使用", "不影响质量"} {
		if !strings.Contains(joined, want) {
			t.Errorf("过滤痕迹里没有 %q：%v", want, out.FilteredPhrases)
		}
	}
}

// TestFilterOverreach_FiltersAndKeepsTrace 覆盖过滤器本身：
// 命中就替换成显式标记，并报告命中了什么；没命中就原样返回。
func TestFilterOverreach_FiltersAndKeepsTrace(t *testing.T) {
	out, hits := FilterOverreach("温度正常，质量不受影响，批次可以放心使用")
	for _, forbidden := range []string{"质量不受影响", "可以放心使用"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("越权措辞 %q 未被过滤：%s", forbidden, out)
		}
	}
	if len(hits) != 2 {
		t.Fatalf("应当报告 2 处命中，实际 %d：%v", len(hits), hits)
	}
	clean, hits := FilterOverreach("建议先隔离并送疫苗生产企业评估")
	if hits != nil {
		t.Errorf("正常文本不应报命中：%v", hits)
	}
	if clean != "建议先隔离并送疫苗生产企业评估" {
		t.Errorf("正常文本被改动了：%s", clean)
	}
}

// TestAdvise_PerBatchAdviceByOwnZoneAndExposure 覆盖「批次逐条建议」：
// 三个批次按各自的温区与暴露时长分别给建议，不是一刀切。
func TestAdvise_PerBatchAdviceByOwnZoneAndExposure(t *testing.T) {
	out := advise(t, adviceInputs())
	if len(out.BatchAdvices) != 3 {
		t.Fatalf("应当给出 3 条逐批次建议，实际 %d 条", len(out.BatchAdvices))
	}

	byRef := map[string]domain.BatchAdvice{}
	for _, ba := range out.BatchAdvices {
		byRef[ba.BatchRef] = ba
	}

	// 一、疫苗（脱敏样例）：温区 2~8℃，峰值 9.6℃ 越出上限 → 建议隔离。
	vaccine := byRef["<EXAMPLE_LOT-01>"]
	if !vaccine.Affected {
		t.Error("疫苗批次的峰值 9.6℃ 越出了 2~8℃，应当判为受影响")
	}
	if vaccine.Disposition != model.DispositionIsolate {
		t.Errorf("受影响批次的建议处置应为 isolate，实际 %q", vaccine.Disposition)
	}
	if vaccine.DeviationC != 1.6 {
		t.Errorf("偏离应为 1.6℃，实际 %v", vaccine.DeviationC)
	}
	if vaccine.ExposureMinutes == nil || *vaccine.ExposureMinutes != 15 {
		t.Errorf("暴露时长应为 15 分钟，实际 %v", vaccine.ExposureMinutes)
	}
	if vaccine.Zone != "2~8℃" {
		t.Errorf("批次温区应为该批次自己的 2~8℃，实际 %q", vaccine.Zone)
	}
	if !strings.Contains(vaccine.AdviceText, "建议先隔离") {
		t.Errorf("受影响批次的建议正文应为「建议先隔离…」，实际 %q", vaccine.AdviceText)
	}
	if !strings.Contains(vaccine.AdviceText, "疫苗生产企业") {
		t.Errorf("建议正文应写明由疫苗生产企业评估，实际 %q", vaccine.AdviceText)
	}

	// 二、冷藏药品（脱敏样例）：温区 0~10℃，峰值 9.6℃ 未越上限 → 不受影响。
	drug := byRef["<EXAMPLE_LOT-02>"]
	if drug.Affected {
		t.Error("冷藏药品批次的峰值 9.6℃ 未越出 0~10℃，不应判为受影响")
	}
	if drug.Disposition != model.DispositionPendingEvaluation {
		t.Errorf("未受影响批次的建议处置应为待企业评估，实际 %q", drug.Disposition)
	}
	if drug.DeviationC != 0 {
		t.Errorf("未越限批次的偏离应为 0，实际 %v", drug.DeviationC)
	}
	if drug.Zone != "0~10℃" {
		t.Errorf("批次温区应为该批次自己的 0~10℃，实际 %q", drug.Zone)
	}

	// 三、温区未登记的批次：判不了 → 待企业评估，且说清为什么判不了。
	unknown := byRef["<EXAMPLE_LOT-03>"]
	if unknown.Affected {
		t.Error("温区未登记的批次不应判为受影响")
	}
	if unknown.Zone != "" {
		t.Errorf("温区未登记的批次不应带温区，实际 %q", unknown.Zone)
	}
	if !strings.Contains(unknown.AdviceText, "温区未在点位温区配置中登记") {
		t.Errorf("温区未登记时建议正文应写明判不了，实际 %q", unknown.AdviceText)
	}

	// 整体处置取最严的一条：有批次越限就先隔离。
	if out.Disposition != model.DispositionIsolate {
		t.Errorf("整体处置应为 isolate，实际 %q", out.Disposition)
	}
}

// TestAdvise_EveryAdviceCitesWindowAndAttributionEvidence 覆盖「证据引用」：
// 每条建议都要挂到窗口事实与归因证据上，不给无依据的结论。
func TestAdvise_EveryAdviceCitesWindowAndAttributionEvidence(t *testing.T) {
	out := advise(t, adviceInputs())
	for _, ba := range out.BatchAdvices {
		if len(ba.Evidence) == 0 {
			t.Errorf("批次 %s 的建议没有任何证据", ba.BatchRef)
			continue
		}
		features := map[string]bool{}
		for _, ev := range ba.Evidence {
			features[ev.Feature] = true
		}
		for _, want := range []string{"door_open_minutes", "excursion_exposure_minutes"} {
			if !features[want] {
				t.Errorf("批次 %s 的建议缺少证据 %q", ba.BatchRef, want)
			}
		}
		// 偏离证据只在「温区清楚且本次越温确实涉及该批次」时才有：
		// 温区没登记时算不出偏离，凭空补一条等于编数。
		wantDeviation := ba.Exposed && ba.Zone != ""
		if wantDeviation && !features["peak_deviation_from_batch_zone_c"] {
			t.Errorf("批次 %s 的建议缺少证据 %q", ba.BatchRef, "peak_deviation_from_batch_zone_c")
		}
		if !wantDeviation && features["peak_deviation_from_batch_zone_c"] {
			t.Errorf("批次 %s 不该带偏离证据（温区未知或未涉及本次越温）", ba.BatchRef)
		}
	}
	if !strings.Contains(out.Rationale, "越温窗口") || !strings.Contains(out.Rationale, "开门（door_open）") {
		t.Errorf("整体依据应引用窗口事实与原因结论，实际 %q", out.Rationale)
	}
}

// TestAdvise_BatchAtAnotherPointIsNotExposed 覆盖「未涉及本次越温的批次」：
// 批次登记在别的点位时，暴露时长记 0、不判受影响，但也不说「可以放心使用」。
func TestAdvise_BatchAtAnotherPointIsNotExposed(t *testing.T) {
	in := adviceInputs()
	in.Record.Batches = []domain.Batch{
		{PointID: "point-cold-room-02", BatchRef: "<EXAMPLE_LOT-09>", ProductName: "疫苗（脱敏样例）", Quantity: 10, ExpiryDate: "<EXAMPLE_EXPIRY-Z>"},
	}
	out := advise(t, in)
	if len(out.BatchAdvices) != 1 {
		t.Fatalf("应当给出 1 条逐批次建议，实际 %d 条", len(out.BatchAdvices))
	}
	ba := out.BatchAdvices[0]
	if ba.Affected {
		t.Error("登记在别的点位的批次不应判为受影响")
	}
	if ba.ExposureMinutes == nil || *ba.ExposureMinutes != 0 {
		t.Errorf("未涉及本次越温的批次暴露时长应为 0 分钟，实际 %v", ba.ExposureMinutes)
	}
	if !strings.Contains(ba.AdviceText, "未涉及该批次") {
		t.Errorf("建议正文应写明未涉及该批次，实际 %q", ba.AdviceText)
	}
	for _, forbidden := range []string{"可放心使用", "可以放行", "不影响质量"} {
		if strings.Contains(ba.AdviceText, forbidden) {
			t.Errorf("建议正文出现越权措辞 %q：%s", forbidden, ba.AdviceText)
		}
	}
}

// TestAdvise_ReleaseRequestIsWithheld 覆盖「系统不自己下结论」：
// 上游要求给「放行」时，系统压下来并留痕，不照着写进稿子。
func TestAdvise_ReleaseRequestIsWithheld(t *testing.T) {
	in := adviceInputs()
	in.RequestedDisposition = model.DispositionRelease

	out := advise(t, in)
	if out.Disposition == model.DispositionRelease {
		t.Fatal("系统不得把「放行」作为建议给出")
	}
	if out.WithheldDisposition != model.DispositionRelease {
		t.Errorf("被压下的上游处置取值应当留痕，实际 %q", out.WithheldDisposition)
	}
	if !strings.Contains(out.Note, "属人工决定事项") {
		t.Errorf("处置说明应写明为何压下上游取值：%s", out.Note)
	}
}

// TestAdvise_MissingRecordRejected 覆盖失败路径：
// 没有记录表载荷就没有批次清单，不给建议，更不编一个。
func TestAdvise_MissingRecordRejected(t *testing.T) {
	_, err := Advise(AdviceInputs{})
	if err == nil {
		t.Fatal("缺少记录表载荷时应当报错")
	}
	if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("错误码应为 E_VALIDATION，实际 %s", code)
	}
}

// TestRender_WithAdviceShowsPerBatchAdviceAndNoReleaseWording 覆盖成稿：
// 逐批次建议写进稿子，稿子里没有一句直接放行的话。
func TestRender_WithAdviceShowsPerBatchAdviceAndNoReleaseWording(t *testing.T) {
	in := adviceInputs()
	advice := advise(t, in)

	draft, err := Render(Inputs{
		Record:      in.Record,
		Window:      in.Window,
		Attribution: in.Attribution,
		Advice:      &advice,
	}, Options{Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatalf("Render 返回错误：%v", err)
	}
	text := draft.Text

	for _, want := range []string{
		"<EXAMPLE_LOT-01>",
		"<EXAMPLE_LOT-02>",
		"<EXAMPLE_LOT-03>",
		"该批次温区：2~8℃",
		"该批次温区：0~10℃",
		"暴露时长：15 分钟",
		"隔离（isolate）",
		"待疫苗生产企业评估（pending_manufacturer_evaluation）",
		"最终处置待药学部主管确认后确定",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("成稿缺少 %q\n%s", want, text)
		}
	}
	for _, forbidden := range []string{"可放心使用", "建议放行", "同意放行", "质量不受影响", "可以直接使用"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("成稿出现直接放行措辞 %q\n%s", forbidden, text)
		}
	}
	// 每条逐批次建议都要有出处，稿子里的建议才不是凭空来的。
	for _, f := range draft.Fields {
		if strings.TrimSpace(f.Source) == "" {
			t.Errorf("栏目 %q 没有取值出处", f.Label)
		}
	}
}
