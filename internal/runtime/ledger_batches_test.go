package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// 本文件覆盖成稿步接上冷链台账读侧之后的四条行为：
//
//  1. 台账查到了在库批次 → 逐批次建议按台账给的批次出，批次号原样引用；
//  2. 台账没查成 → 成稿写「批次信息缺失，待人工核对」，不编批次号，
//     但判定本身不因此失败（窗口与归因结论照样成立，结论仍停在待主管确认）；
//  3. 查到了但确实没有在库批次 → 照实写「无在库批次」，不写「待人工核对」
//     （让人白跑一趟台账，比漏写还糟）；
//  4. 本地给了批次清单时不走台账（本地清单优先）。

// stubBatchReader 是在库批次读侧的测试替身。
type stubBatchReader struct {
	lookup domain.BatchLookup
	err    error
	calls  int
	point  string
	at     time.Time
}

func (s *stubBatchReader) Batches(ctx context.Context, pointID string, at time.Time) (domain.BatchLookup, error) {
	s.calls++
	s.point = pointID
	s.at = at
	return s.lookup, s.err
}

// TestChain_LedgerBatchesFeedPerBatchAdvice 覆盖台账读侧取到在库批次的情形。
func TestChain_LedgerBatchesFeedPerBatchAdvice(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	samples := excursionSamples()
	at := samples[0].MeasuredAt
	reader := &stubBatchReader{
		lookup: domain.NewBatchLookupOk([]domain.Batch{
			{
				PointID:     testPointID,
				BatchRef:    "<EXAMPLE_LOT-01>",
				ProductName: "疫苗（脱敏样例）",
				Quantity:    120,
				ExpiryDate:  "<EXAMPLE_EXPIRY-A>",
			},
		}, at),
	}

	chain, err := NewChain(ChainDeps{
		Profile:     adviceProfile(),
		Samples:     samples,
		BatchReader: reader,
	})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	exec := NewExecutor(chain, st, quietLogger())

	result, err := exec.Run(ctx, directRun(t, samples))
	if err != nil {
		t.Fatalf("步骤链执行失败：%v", err)
	}

	if reader.calls != 1 {
		t.Fatalf("台账读侧被调用 %d 次，期望 1 次", reader.calls)
	}
	if reader.point != testPointID {
		t.Errorf("查询点位 = %q，期望 %q", reader.point, testPointID)
	}
	// 查库存的时刻取越温窗口起点：那才是「这批货当时在不在库里」的时刻。
	if !reader.at.Equal(at) {
		t.Errorf("查询时刻 = %s，期望窗口起点 %s", reader.at.Format(time.RFC3339), at.Format(time.RFC3339))
	}

	record, ok := ProductAs[domain.ExceptionRecordPayload](result.Steps, domain.PayloadExceptionRecord)
	if !ok {
		t.Fatal("成稿产物应能按 exception_record 分码还原")
	}
	if len(record.Batches) != 1 || record.Batches[0].BatchRef != "<EXAMPLE_LOT-01>" {
		t.Fatalf("成稿产物应带台账给的批次，实际 %+v", record.Batches)
	}
	if record.BatchLookup == nil || record.BatchLookup.Status != domain.BatchLookupOK {
		t.Fatalf("成稿产物应记着批次查询状态，实际 %+v", record.BatchLookup)
	}
	if len(record.Advice.BatchAdvices) != 1 {
		t.Fatalf("应给出 1 条逐批次建议，实际 %d 条", len(record.Advice.BatchAdvices))
	}
	if record.Advice.BatchAdvices[0].Disposition != "isolate" {
		t.Errorf("批次建议处置应为 isolate，实际 %q", record.Advice.BatchAdvices[0].Disposition)
	}
	if len(record.MissingFields) != 1 || record.MissingFields[0] != "disposition" {
		t.Errorf("缺项列表应为 [disposition]，实际 %v", record.MissingFields)
	}
}

// TestChain_LedgerUnavailableMarksBatchMissing 覆盖台账没查成的情形。
func TestChain_LedgerUnavailableMarksBatchMissing(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	samples := excursionSamples()
	reader := &stubBatchReader{
		err: errcode.Newf(errcode.LedgerUnavailable, "冷链台账读侧调用超时（超过 15s）"),
	}

	chain, err := NewChain(ChainDeps{
		Profile:     adviceProfile(),
		Samples:     samples,
		BatchReader: reader,
	})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	exec := NewExecutor(chain, st, quietLogger())

	result, err := exec.Run(ctx, directRun(t, samples))
	if err != nil {
		t.Fatalf("台账没查成不应让判定失败：%v", err)
	}

	record, ok := ProductAs[domain.ExceptionRecordPayload](result.Steps, domain.PayloadExceptionRecord)
	if !ok {
		t.Fatal("成稿产物应能按 exception_record 分码还原")
	}
	if record.BatchLookup == nil {
		t.Fatal("成稿产物应记着批次查询状态")
	}
	if record.BatchLookup.Status != domain.BatchLookupUnavailable {
		t.Errorf("批次查询状态 = %q，期望 %q", record.BatchLookup.Status, domain.BatchLookupUnavailable)
	}
	if !record.BatchLookup.NeedsManual {
		t.Error("台账没查成时批次查询结果应标 needs_manual")
	}
	if len(record.Batches) != 0 {
		t.Errorf("台账没查成时不得有批次，实际 %d 条", len(record.Batches))
	}
	if !strings.Contains(record.DraftText, domain.BatchMissingText) {
		t.Errorf("成稿正文缺少「%s」\n%s", domain.BatchMissingText, record.DraftText)
	}
	// 一个批次号都不许出现：编出来的批次号在药事面前查无此物。
	if strings.Contains(record.DraftText, "LOT") {
		t.Errorf("成稿凭空造出了批次号\n%s", record.DraftText)
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
	// 判定本身不因查不到库存而停摆：结论仍停在待主管确认。
	if result.Run.State != domain.RunStateAwaitingConfirmation {
		t.Errorf("业务状态应停在待主管确认，实际 %q", result.Run.State)
	}
}

// TestChain_LedgerEmptySaysNoInStockBatch 覆盖「查到了但确实没有在库批次」。
func TestChain_LedgerEmptySaysNoInStockBatch(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	samples := excursionSamples()
	reader := &stubBatchReader{
		lookup: domain.NewBatchLookupOk(nil, samples[0].MeasuredAt),
	}

	chain, err := NewChain(ChainDeps{
		Profile:     adviceProfile(),
		Samples:     samples,
		BatchReader: reader,
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
	if record.BatchLookup == nil || record.BatchLookup.Status != domain.BatchLookupEmpty {
		t.Fatalf("批次查询状态 = %+v，期望 empty", record.BatchLookup)
	}
	if record.BatchLookup.NeedsManual {
		t.Error("查到了空清单不该要求人工核对")
	}
	if !strings.Contains(record.DraftText, "无在库批次") {
		t.Errorf("成稿正文应写明无在库批次\n%s", record.DraftText)
	}
	if strings.Contains(record.DraftText, domain.BatchMissingText) {
		t.Errorf("查到了空清单不该写成「%s」\n%s", domain.BatchMissingText, record.DraftText)
	}
}

// TestChain_LedgerErrorWithoutStatusStillMarked 覆盖读侧只回错误、不回状态的情形。
func TestChain_LedgerErrorWithoutStatusStillMarked(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	samples := excursionSamples()
	reader := &stubBatchReader{err: errors.New("台账连接被拒绝")}

	chain, err := NewChain(ChainDeps{
		Profile:     adviceProfile(),
		Samples:     samples,
		BatchReader: reader,
	})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	exec := NewExecutor(chain, st, quietLogger())

	result, err := exec.Run(ctx, directRun(t, samples))
	if err != nil {
		t.Fatalf("台账没查成不应让判定失败：%v", err)
	}
	record, ok := ProductAs[domain.ExceptionRecordPayload](result.Steps, domain.PayloadExceptionRecord)
	if !ok {
		t.Fatal("成稿产物应能按 exception_record 分码还原")
	}
	if record.BatchLookup == nil || record.BatchLookup.Status != domain.BatchLookupUnavailable {
		t.Fatalf("批次查询状态 = %+v，期望 unavailable", record.BatchLookup)
	}
	if !record.BatchLookup.NeedsManual {
		t.Error("台账没查成时批次查询结果应标 needs_manual")
	}
	if strings.TrimSpace(record.BatchLookup.Detail) == "" {
		t.Error("批次查询结果应写明失败原因")
	}
}

// TestChain_LocalBatchesWinOverLedger 覆盖本地清单优先的情形。
func TestChain_LocalBatchesWinOverLedger(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	samples := excursionSamples()
	reader := &stubBatchReader{
		lookup: domain.NewBatchLookupOk([]domain.Batch{
			{PointID: testPointID, BatchRef: "<EXAMPLE_LOT-99>", ProductName: "疫苗（脱敏样例）"},
		}, samples[0].MeasuredAt),
	}

	chain, err := NewChain(ChainDeps{
		Profile:     adviceProfile(),
		Samples:     samples,
		Batches:     adviceBatches(),
		BatchReader: reader,
	})
	if err != nil {
		t.Fatalf("装配步骤链失败：%v", err)
	}
	exec := NewExecutor(chain, st, quietLogger())

	result, err := exec.Run(ctx, directRun(t, samples))
	if err != nil {
		t.Fatalf("步骤链执行失败：%v", err)
	}
	if reader.calls != 0 {
		t.Errorf("本地给了清单时不该再打台账，实际调用 %d 次", reader.calls)
	}
	record, ok := ProductAs[domain.ExceptionRecordPayload](result.Steps, domain.PayloadExceptionRecord)
	if !ok {
		t.Fatal("成稿产物应能按 exception_record 分码还原")
	}
	if len(record.Batches) != 2 {
		t.Fatalf("成稿产物应带本地清单的 2 个批次，实际 %d 个", len(record.Batches))
	}
	if record.BatchLookup != nil {
		t.Error("本地清单不是台账实时查来的，不该标成台账查询状态")
	}
}
