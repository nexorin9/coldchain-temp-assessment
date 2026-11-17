package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/drafting"
)

// TestChain_DraftCarriesQualityVerdict 覆盖成稿步接上要素完整性校验：
// 校验结论随成稿产物落库，缺哪几项、哪几项挡着写回都写得清楚。
func TestChain_DraftCarriesQualityVerdict(t *testing.T) {
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
	if record.Quality == nil {
		t.Fatal("成稿产物应带要素完整性校验结论")
	}
	if !record.Quality.Qualified {
		t.Errorf("必备要素齐备时应判合格，挡写回的缺项：%v", record.Quality.BlockingFields)
	}
	if record.Quality.Complete {
		t.Error("主管尚未确认最终处置时，稿子还不该判完整")
	}
	if len(record.Quality.MissingFields) != 1 || record.Quality.MissingFields[0] != "disposition" {
		t.Errorf("缺项清单应为 [disposition]，实际 %v", record.Quality.MissingFields)
	}
	if len(record.Quality.PendingFields) != 1 || record.Quality.PendingFields[0] != "disposition" {
		t.Errorf("待补清单应为 [disposition]，实际 %v", record.Quality.PendingFields)
	}
	if len(record.Quality.BlockingFields) != 0 {
		t.Errorf("必备要素齐备时挡写回的缺项应为空，实际 %v", record.Quality.BlockingFields)
	}
	if record.Quality.CheckedAt.IsZero() {
		t.Error("校验结论应带校验时刻")
	}

	// 稿子上要写清还差哪几项，且写中文栏目名而不是字段名。
	if !strings.Contains(record.DraftText, "待补要素") {
		t.Errorf("成稿正文缺少待补要素提示\n%s", record.DraftText)
	}
	if !strings.Contains(record.DraftText, "处置建议") {
		t.Errorf("成稿正文未写明缺哪一项\n%s", record.DraftText)
	}
}

// TestChain_DraftWithoutBatchesIsStillExportable 覆盖缺项的两级语义在链路上的表现：
// 批次清单拿不到时，稿子标 needs_review、列出缺项，但**不挡**台账写回——
// 台账如实收下一条没有 batches 的越温记录，比这条记录压根不落库要好。
func TestChain_DraftWithoutBatchesIsStillExportable(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	samples := excursionSamples()

	chain, err := NewChain(ChainDeps{
		Profile: adviceProfile(),
		Samples: samples,
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
	if !record.NeedsReview {
		t.Error("缺批次与最终处置时成稿应标 needs_review")
	}
	if len(record.MissingFields) != 2 ||
		record.MissingFields[0] != "batches" || record.MissingFields[1] != "disposition" {
		t.Errorf("缺项清单应为 [batches disposition]，实际 %v", record.MissingFields)
	}
	if record.Quality == nil || !record.Quality.Qualified {
		t.Fatalf("待人工补齐的要素不该挡写回，校验结论：%+v", record.Quality)
	}

	// 写回载荷组装得出来，且批次栏是空的（不是编出来的）。
	source, err := drafting.BuildWritebackSource(record)
	if err != nil {
		t.Fatalf("缺批次不应挡写回：%v", err)
	}
	if len(source.Batches) != 0 {
		t.Errorf("没有批次清单时写回载荷的批次栏应为空，实际 %v", source.Batches)
	}
	if source.PointID != testPointID {
		t.Errorf("写回载荷点位不对：%q", source.PointID)
	}
	if source.ExceptionRecord == "" {
		t.Error("写回载荷应带上异常记录表正文")
	}
}

// TestChain_UnqualifiedDraftRefusesWritebackPayload 覆盖「不合格成稿不可直接导出」：
// 必备要素缺项时，写回载荷组装被拒，且错误信息点名缺项。
//
// 链路正常跑出来的稿子不会缺必备要素——这里直接改产物，
// 模拟一份要素不全的稿子被推到写回口。
func TestChain_UnqualifiedDraftRefusesWritebackPayload(t *testing.T) {
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

	// 温区这一栏没了：这份稿子在陈述一件说不清的事。
	record.Zone = ""
	record.Quality.Qualified = false
	record.Quality.BlockingFields = []string{domain.DraftElementZone}

	_, err = drafting.BuildWritebackSource(record)
	if err == nil {
		t.Fatal("要素不全的成稿不得组装出台账写回载荷")
	}
	if !strings.Contains(err.Error(), "zone") {
		t.Errorf("错误信息未点名缺项：%v", err)
	}
}
