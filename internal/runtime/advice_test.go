package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
)

// adviceProfile 造一份带制品温区的点位配置。
//
// 制品温区是逐批次建议的输入：没有它，系统判不了某批货受没受影响，
// 只能给「待人工核对」，给不出建议。
func adviceProfile() *domain.PointProfile {
	profile := testProfile()
	profile.Products = []domain.ProductProfile{
		{
			ProductName: "疫苗（脱敏样例）",
			ZoneMinC:    2,
			ZoneMaxC:    8,
			ZoneRef:     "点位温区配置 products[] 条目（脱敏样例）",
		},
	}
	return profile
}

// adviceBatches 造一份在库批次清单（占位批次号，不含真实批号）。
func adviceBatches() []domain.Batch {
	return []domain.Batch{
		{
			PointID:     testPointID,
			BatchRef:    "<EXAMPLE_LOT-01>",
			ProductName: "疫苗（脱敏样例）",
			Quantity:    120,
			ExpiryDate:  "<EXAMPLE_EXPIRY-A>",
		},
		{
			PointID:     testPointID,
			BatchRef:    "<EXAMPLE_LOT-02>",
			ProductName: "疫苗（脱敏样例）",
			Quantity:    60,
			ExpiryDate:  "<EXAMPLE_EXPIRY-B>",
		},
	}
}

// TestChain_DraftCarriesPerBatchAdvice 覆盖成稿步接上批次清单后的行为：
// 逐批次建议落进成稿产物，正文里能看见每个批次的温区、暴露时长与建议。
func TestChain_DraftCarriesPerBatchAdvice(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	samples := excursionSamples()

	chain, err := NewChain(ChainDeps{
		Profile: adviceProfile(),
		Samples: samples,
		Batches: adviceBatches(),
	})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	exec := NewExecutor(chain, st, quietLogger())

	result, err := exec.Run(ctx, directRun(t, samples))
	if err != nil {
		t.Fatalf("步骤链执行失败：%v", err)
	}

	record, ok := ProductAs[domain.ExceptionRecordPayload](result.Steps, domain.PayloadExceptionRecord)
	if !ok {
		t.Fatal("成稿产物应能按 exception_record 分码还原")
	}
	if record.Advice == nil {
		t.Fatal("成稿产物应带批次可用性建议")
	}
	if len(record.Advice.BatchAdvices) != 2 {
		t.Fatalf("应当给出 2 条逐批次建议，实际 %d 条", len(record.Advice.BatchAdvices))
	}
	for _, ba := range record.Advice.BatchAdvices {
		if ba.Disposition != "isolate" {
			t.Errorf("批次 %s 峰值 12.4℃ 越出了 2~8℃，建议处置应为 isolate，实际 %q",
				ba.BatchRef, ba.Disposition)
		}
		if ba.DeviationC != 4.4 {
			t.Errorf("批次 %s 的偏离应为 4.4℃，实际 %v", ba.BatchRef, ba.DeviationC)
		}
		if ba.ExposureMinutes == nil || *ba.ExposureMinutes <= 0 {
			t.Errorf("批次 %s 的暴露时长应有值，实际 %v", ba.BatchRef, ba.ExposureMinutes)
		}
		if len(ba.Evidence) == 0 {
			t.Errorf("批次 %s 的建议没有证据", ba.BatchRef)
		}
	}
	if record.Disposition != "isolate" {
		t.Errorf("整体建议处置应为 isolate，实际 %q", record.Disposition)
	}
	// 批次清单已取得，「batches」不再是缺项；但最终处置仍待主管确认。
	if len(record.MissingFields) != 1 || record.MissingFields[0] != "disposition" {
		t.Errorf("缺项列表应为 [disposition]，实际 %v", record.MissingFields)
	}
	if !record.NeedsReview {
		t.Error("建议已给出但主管尚未确认，成稿仍应标 needs_review")
	}
	// 建议不推进业务状态：仍停在待主管确认。
	if result.Run.State != domain.RunStateAwaitingConfirmation {
		t.Errorf("业务状态应停在待主管确认，实际 %q", result.Run.State)
	}
	for _, want := range []string{
		"<EXAMPLE_LOT-01>",
		"该批次温区：2~8℃",
		"暴露时长：",
		"批次可用性建议：隔离（isolate）",
		"最终处置待药学部主管确认后确定",
	} {
		if !strings.Contains(record.DraftText, want) {
			t.Errorf("成稿正文缺少 %q\n%s", want, record.DraftText)
		}
	}
	for _, forbidden := range []string{"可放心使用", "建议放行", "同意放行"} {
		if strings.Contains(record.DraftText, forbidden) {
			t.Errorf("成稿正文出现越权措辞 %q\n%s", forbidden, record.DraftText)
		}
	}
}

// TestChain_DraftWithoutBatchesKeepsBatchesOutstanding 覆盖没有批次清单时的行为：
// 批次栏仍列为缺项，处置给默认建议，不编批次号。
func TestChain_DraftWithoutBatchesKeepsBatchesOutstanding(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	samples := excursionSamples()

	chain, err := NewChain(ChainDeps{Profile: adviceProfile(), Samples: samples})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	exec := NewExecutor(chain, st, quietLogger())

	result, err := exec.Run(ctx, directRun(t, samples))
	if err != nil {
		t.Fatalf("步骤链执行失败：%v", err)
	}

	record, ok := ProductAs[domain.ExceptionRecordPayload](result.Steps, domain.PayloadExceptionRecord)
	if !ok {
		t.Fatal("成稿产物应能按 exception_record 分码还原")
	}
	if record.Advice == nil || record.Advice.Disposition != "pending_manufacturer_evaluation" {
		t.Fatalf("没有批次清单时处置建议应为待疫苗生产企业评估，实际 %+v", record.Advice)
	}
	if len(record.Advice.BatchAdvices) != 0 {
		t.Errorf("没有批次清单时不应给出逐批次建议，实际 %d 条", len(record.Advice.BatchAdvices))
	}
	found := false
	for _, field := range record.MissingFields {
		if field == "batches" {
			found = true
		}
	}
	if !found {
		t.Errorf("缺项列表应含 batches，实际 %v", record.MissingFields)
	}
	if strings.Contains(record.DraftText, "<EXAMPLE_LOT") {
		t.Errorf("成稿凭空造出了批次号\n%s", record.DraftText)
	}
}
