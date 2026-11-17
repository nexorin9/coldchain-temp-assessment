package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// sampleProfile 造一份点位温区配置。
func sampleProfile(pointID string) domain.PointProfile {
	return domain.PointProfile{
		PointID:             pointID,
		ZoneMinC:            2,
		ZoneMaxC:            8,
		SampleIntervalSec:   300,
		MinExcursionMinutes: 20,
		HysteresisC:         0.5,
		AccuracyLimitC:      0.5,
	}
}

// TestUpsertPointProfile_RoundTrips 断言配置写进去能原样取回。
func TestUpsertPointProfile_RoundTrips(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	profile := sampleProfile("point-vax-freezer-01")
	profile.Products = []domain.ProductProfile{
		{ProductName: "某疫苗制品", ZoneMinC: 2, ZoneMaxC: 8, ZoneRef: "疫苗储存和运输管理规范（2017 年版）"},
	}
	if err := st.UpsertPointProfile(ctx, profile); err != nil {
		t.Fatalf("写入点位温区配置失败：%v", err)
	}

	got, err := st.PointProfileByID(ctx, "point-vax-freezer-01")
	if err != nil {
		t.Fatalf("回读点位温区配置失败：%v", err)
	}
	if got.ZoneMinC != 2 || got.ZoneMaxC != 8 {
		t.Errorf("温区取回是 %.2f~%.2f℃，期望 2~8℃", got.ZoneMinC, got.ZoneMaxC)
	}
	if got.MinExcursionMinutes != 20 || got.SampleIntervalSec != 300 {
		t.Errorf("判定门槛取回不对：min_excursion_minutes=%d，sample_interval_sec=%d",
			got.MinExcursionMinutes, got.SampleIntervalSec)
	}
	if len(got.Products) != 1 || got.Products[0].ProductName != "某疫苗制品" {
		t.Errorf("制品温区条目取回不对：%+v", got.Products)
	}
}

// TestUpsertPointProfile_ReplacesSameRow 断言重复登记是覆盖同一行。
func TestUpsertPointProfile_ReplacesSameRow(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	if err := st.UpsertPointProfile(ctx, sampleProfile("point-vax-freezer-01")); err != nil {
		t.Fatalf("写入点位温区配置失败：%v", err)
	}

	updated := sampleProfile("point-vax-freezer-01")
	updated.ZoneMaxC = 10
	if err := st.UpsertPointProfile(ctx, updated); err != nil {
		t.Fatalf("覆盖点位温区配置失败：%v", err)
	}

	all, err := st.PointProfiles(ctx)
	if err != nil {
		t.Fatalf("回读点位温区配置清单失败：%v", err)
	}
	if len(all) != 1 {
		t.Fatalf("清单里有 %d 份配置，期望 1 份（重复登记是覆盖）", len(all))
	}
	if all[0].ZoneMaxC != 10 {
		t.Errorf("zone_max_c 是 %.2f℃，期望 10℃（覆盖没生效）", all[0].ZoneMaxC)
	}
}

// TestPointProfileByID_NotFound 断言没登记的点位明确说找不到。
func TestPointProfileByID_NotFound(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	defer func() { _ = st.Close() }()

	_, err = st.PointProfileByID(context.Background(), "point-not-registered")
	if err == nil {
		t.Fatal("回读未登记的点位应当报错")
	}
	e := errcode.As(err)
	if e.Code != errcode.NotFound {
		t.Errorf("错误码是 %q，期望 %q", e.Code, errcode.NotFound)
	}
}

// TestPointProfiles_OrderedByPointID 断言清单按 point_id 升序。
func TestPointProfiles_OrderedByPointID(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	for _, pointID := range []string{"point-z", "point-a", "point-m"} {
		if err := st.UpsertPointProfile(ctx, sampleProfile(pointID)); err != nil {
			t.Fatalf("写入点位温区配置失败：%v", err)
		}
	}

	all, err := st.PointProfiles(ctx)
	if err != nil {
		t.Fatalf("回读点位温区配置清单失败：%v", err)
	}
	if len(all) != 3 {
		t.Fatalf("清单里有 %d 份配置，期望 3 份", len(all))
	}
	want := []string{"point-a", "point-m", "point-z"}
	for i, pointID := range want {
		if all[i].PointID != pointID {
			t.Errorf("第 %d 条是 %q，期望 %q", i, all[i].PointID, pointID)
		}
	}
}

// TestUpsertPointProfile_RejectsMissingPointID 断言没有点位标识的配置不入库。
func TestUpsertPointProfile_RejectsMissingPointID(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	defer func() { _ = st.Close() }()

	err = st.UpsertPointProfile(context.Background(), sampleProfile("  "))
	if err == nil {
		t.Fatal("写入缺 point_id 的配置应当报错")
	}
	e := errcode.As(err)
	if e.Code != errcode.Validation {
		t.Errorf("错误码是 %q，期望 %q", e.Code, errcode.Validation)
	}

	all, err := st.PointProfiles(context.Background())
	if err != nil {
		t.Fatalf("回读点位温区配置清单失败：%v", err)
	}
	if len(all) != 0 {
		t.Errorf("清单里有 %d 份配置，期望 0 份", len(all))
	}
}

// TestOpen_MigratesLegacyDBWithoutPointProfiles 断言存量库补表后老数据不丢。
func TestOpen_MigratesLegacyDBWithoutPointProfiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	// 先造一个「没有 point_profiles 表」的存量库：直接用底层连接建表。
	st, err := Open(path)
	if err != nil {
		t.Fatalf("打开库失败：%v", err)
	}
	if _, err := st.db.ExecContext(context.Background(), "DROP TABLE point_profiles"); err != nil {
		t.Fatalf("删掉 point_profiles 表失败：%v", err)
	}
	if _, err := st.db.ExecContext(context.Background(),
		"INSERT INTO events (event_id, idempotency_key, point_id, device_id, device_name, "+
			"measured_at, temperature_c, unit, source, received_at) VALUES "+
			"('evt_legacy', 'key_legacy', 'point-vax-freezer-01', 'device-1', '旧记录仪', "+
			"'2026-10-01T00:00:00.000000000Z', 5.0, 'C', 'recorder', '2026-10-01T00:00:01.000000000Z')",
	); err != nil {
		t.Fatalf("写入存量事件失败：%v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("关库失败：%v", err)
	}

	// 重新打开：表要补上，老事件要在。
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("重开存量库失败：%v", err)
	}
	defer func() { _ = reopened.Close() }()

	ctx := context.Background()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	events, err := reopened.EventsInRange(ctx, "point-vax-freezer-01",
		base.Add(-time.Hour), base.Add(time.Hour))
	if err != nil {
		t.Fatalf("回读存量事件失败：%v", err)
	}
	if len(events) != 1 {
		t.Fatalf("回读到 %d 条存量事件，期望 1 条（老数据不能丢）", len(events))
	}

	if err := reopened.UpsertPointProfile(ctx, sampleProfile("point-vax-freezer-01")); err != nil {
		t.Fatalf("补表后写配置失败：%v", err)
	}
	if _, err := reopened.PointProfileByID(ctx, "point-vax-freezer-01"); err != nil {
		t.Fatalf("补表后回读配置失败：%v", err)
	}
}
