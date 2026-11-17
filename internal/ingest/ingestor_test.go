package ingest

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
	"github.com/chaosforge/coldchain-temp-assessment/internal/store"
)

// 接入入口的用例跑在真库上：幂等靠唯一约束，限流靠真实计数，
// 用替身测这两样等于没测。

// newTestIngestor 造一个接入入口：真库 + 指定阈值的限流闸门 + 丢弃输出的日志器。
func newTestIngestor(t *testing.T, limit int, window time.Duration, burst int) (*Ingestor, *store.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "coldchain-test.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("打开测试库失败：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// 日志写进 testing 的输出里，失败时能直接看到被判重与限流的记录。
	logger := logging.New(testWriter{t})
	return NewIngestor(st, NewRateLimiter(limit, window, burst), logger), st
}

// testWriter 把日志写进测试输出。
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

// ingestTestEvent 造一条脱敏温度事件。
func ingestTestEvent(pointID string, at time.Time, tempC float64) domain.TemperatureEvent {
	raw := tempC
	accuracy := 0.3
	return domain.TemperatureEvent{
		PointID:        pointID,
		DeviceID:       "dev-" + pointID,
		DeviceName:     "测试冷库记录仪",
		MeasuredAt:     at,
		MeasuredAtRaw:  at.Format(time.RFC3339),
		TemperatureC:   tempC,
		Unit:           domain.UnitCelsius,
		TemperatureRaw: &raw,
		UnitRaw:        domain.UnitCelsius,
		AccuracyC:      &accuracy,
		Source:         domain.SourceRecorder,
	}
}

func TestIngestor_DuplicateDeliveryKeepsOneRow(t *testing.T) {
	ingestor, st := newTestIngestor(t, 100, time.Minute, 100)
	ctx := context.Background()

	event := ingestTestEvent("point-vax-freezer-01",
		time.Date(2026, 10, 20, 3, 15, 0, 0, time.UTC), 11.4)

	var firstID string
	for i := 0; i < 10; i++ {
		result, err := ingestor.Ingest(ctx, event)
		if err != nil {
			t.Fatalf("第 %d 次投递失败：%v", i+1, err)
		}
		if i == 0 {
			if result.Duplicate {
				t.Fatal("首次投递不应判为重复")
			}
			firstID = result.EventID
			continue
		}
		if !result.Duplicate {
			t.Fatalf("第 %d 次投递应判为重复", i+1)
		}
		if result.EventID != firstID {
			t.Fatalf("重复投递的事件 ID 不一致：期望 %s，实际 %s", firstID, result.EventID)
		}
	}

	total, err := st.CountEvents(ctx, "point-vax-freezer-01", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("统计事件失败：%v", err)
	}
	if total != 1 {
		t.Fatalf("重复投递 10 次后 events 表应为 1 行，实际 %d 行", total)
	}
}

func TestIngestor_RateLimitRejectsAndRecords(t *testing.T) {
	ingestor, st := newTestIngestor(t, 2, time.Minute, 2)
	ctx := context.Background()

	base := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		if _, err := ingestor.Ingest(ctx, ingestTestEvent("point-vax-freezer-01", base.Add(time.Duration(i)*time.Minute), 5.0)); err != nil {
			t.Fatalf("第 %d 次投递应放行：%v", i+1, err)
		}
	}

	_, err := ingestor.Ingest(ctx, ingestTestEvent("point-vax-freezer-01", base.Add(2*time.Minute), 5.0))
	if err == nil {
		t.Fatal("超出限流的投递应被拒绝")
	}
	e := errcode.As(err)
	if e.Code != errcode.RateLimited {
		t.Fatalf("期望 E_RATE_LIMITED，实际 %s", e.Code)
	}

	total, err := st.CountEvents(ctx, "point-vax-freezer-01", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("统计事件失败：%v", err)
	}
	if total != 2 {
		t.Fatalf("被限流拒绝的投递不应入库，实际 %d 行", total)
	}
}

func TestIngestor_OtherPointNotBlockedByRateLimit(t *testing.T) {
	ingestor, _ := newTestIngestor(t, 1, time.Minute, 1)
	ctx := context.Background()

	at := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	if _, err := ingestor.Ingest(ctx, ingestTestEvent("point-vax-freezer-01", at, 5.0)); err != nil {
		t.Fatalf("首次投递应放行：%v", err)
	}
	if _, err := ingestor.Ingest(ctx, ingestTestEvent("point-vax-freezer-01", at.Add(time.Minute), 5.0)); err == nil {
		t.Fatal("超限投递应被拒绝")
	}
	if _, err := ingestor.Ingest(ctx, ingestTestEvent("point-vax-freezer-02", at, 5.0)); err != nil {
		t.Fatalf("其它点位不应被牵连：%v", err)
	}
}

func TestIngestor_WithoutStoreReturnsExplicitError(t *testing.T) {
	ingestor := NewIngestor(nil, NewRateLimiter(10, time.Minute, 10), logging.Default())

	_, err := ingestor.Ingest(context.Background(),
		ingestTestEvent("point-vax-freezer-01", time.Now(), 5.0))
	if err == nil {
		t.Fatal("存储未初始化时应返回显式错误")
	}
	if e := errcode.As(err); e.Code != errcode.StorageError {
		t.Fatalf("期望 E_STORAGE_ERROR，实际 %s", e.Code)
	}
}
