package rules

import (
	"strings"
	"testing"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// zoneProfile 造一份点位温区配置（疫苗冷藏 2~8℃，精度上限 0.5℃）。
func zoneProfile() *domain.PointProfile {
	return &domain.PointProfile{
		PointID:           "point-vax-freezer-01",
		ZoneMinC:          2,
		ZoneMaxC:          8,
		SampleIntervalSec: 300,
		AccuracyLimitC:    0.5,
	}
}

// zoneEvent 造一条读数：默认来自登记在册的记录仪、精度 0.3℃。
func zoneEvent(temperatureC float64) domain.TemperatureEvent {
	accuracy := 0.3
	return domain.TemperatureEvent{
		PointID:      "point-vax-freezer-01",
		DeviceID:     "device-recorder-0001",
		DeviceName:   "疫苗冷藏库温度记录仪",
		TemperatureC: temperatureC,
		AccuracyC:    &accuracy,
		Source:       domain.SourceRecorder,
	}
}

// TestResolveZone_NormalProfile 温区正常命中：温区取自点位配置，出处可回溯。
func TestResolveZone_NormalProfile(t *testing.T) {
	profile := zoneProfile()

	zone, err := ResolveZone(profile)
	if err != nil {
		t.Fatalf("期望判定成功，实际报错：%v", err)
	}
	if zone.NeedsManual {
		t.Errorf("配置齐备时期望不转人工，实际 NeedsManual=true（%s）", zone.Reason)
	}
	if got, want := zone.ZoneLabel, "2~8℃"; got != want {
		t.Errorf("温区标签 = %s，期望 %s", got, want)
	}
	if got, want := zone.ZoneSource, ZoneSourcePointProfile; got != want {
		t.Errorf("温区来源 = %s，期望 %s", got, want)
	}
	if !strings.Contains(zone.ZoneBasis, "point-vax-freezer-01") {
		t.Errorf("温区出处应写明点位，实际：%s", zone.ZoneBasis)
	}
	if zone.OutOfRange(8.0) {
		t.Error("恰好等于 zone_max_c 的读数应仍在区内")
	}
	if !zone.OutOfRange(8.1) {
		t.Error("高于 zone_max_c 的读数应判越限")
	}
	if !zone.OutOfRange(1.9) {
		t.Error("低于 zone_min_c 的读数应判越限")
	}
}

// TestResolveZone_MissingProfileGoesManual 配置缺失转人工：不猜温区、不顶替默认值。
func TestResolveZone_MissingProfileGoesManual(t *testing.T) {
	zone, err := ResolveZone(nil)
	if err == nil {
		t.Fatal("缺少温区配置时期望报错")
	}
	if e := errcode.As(err); e.Code != errcode.Validation {
		t.Errorf("错误码 = %s，期望 %s", e.Code, errcode.Validation)
	}
	if !zone.NeedsManual {
		t.Error("缺少温区配置时期望标记 needs_manual")
	}
	if zone.Reason == "" {
		t.Error("转人工时必须写明原因")
	}
	if zone.ZoneMinC != 0 || zone.ZoneMaxC != 0 {
		t.Errorf("判不了时不得给出温区数值，实际 zone=%s", zone.ZoneLabel)
	}
}

// TestResolveZone_MissingProfileProducesNoWindow 配置缺失即不判的闸门：
// 温区判不了时，窗口这一步也产不出结论。
//
// 「没越限」和「判不了」是两回事：把判不了当成没越限，
// 等于替一台没配温区的冷柜下了个「没事」的结论。
func TestResolveZone_MissingProfileProducesNoWindow(t *testing.T) {
	zone, err := ResolveZone(nil)
	if err == nil {
		t.Fatal("缺少温区配置时期望报错")
	}
	if zone.Usable() {
		t.Error("缺少温区配置的判定结果不得标为可用")
	}

	window, err := ExtractWindow([]domain.TemperatureEvent{
		sampleAt(0, 11.4),
		sampleAt(5, 12.4),
	}, zone)
	if err == nil {
		t.Fatal("温区不可用时期望报错（不产出窗口结论）")
	}
	if e := errcode.As(err); e.Code != errcode.Validation {
		t.Errorf("错误码 = %s，期望 %s", e.Code, errcode.Validation)
	}
	if window != nil {
		t.Errorf("温区不可用时期望不产出窗口，实际得到 %+v", window)
	}
}

// TestResolveZone_InvalidBoundsGoesManual 温区上下限非法时转人工。
func TestResolveZone_InvalidBoundsGoesManual(t *testing.T) {
	profile := zoneProfile()
	profile.ZoneMinC = 8
	profile.ZoneMaxC = 2

	zone, err := ResolveZone(profile)
	if err == nil {
		t.Fatal("温区上下限非法时期望报错")
	}
	if !zone.NeedsManual {
		t.Error("温区上下限非法时期望标记 needs_manual")
	}
}

// TestResolveZoneForEvent_MultiProductsJudgedSeparately 多制品分别判定：
// 每个制品有自己的温区与出处，在库批次挂到各自温区下。
func TestResolveZoneForEvent_MultiProductsJudgedSeparately(t *testing.T) {
	profile := zoneProfile()
	profile.Products = []domain.ProductProfile{
		{
			ProductName: "疫苗（脱敏样例）",
			ZoneMinC:    2,
			ZoneMaxC:    8,
			ZoneRef:     "疫苗储存和运输管理规范（2017 年版）",
		},
		{
			ProductName: "冷藏药品（脱敏样例）",
			ZoneMinC:    2,
			ZoneMaxC:    8,
		},
	}

	event := zoneEvent(11.4)
	batches := []domain.Batch{
		{PointID: "point-vax-freezer-01", BatchRef: "<EXAMPLE_LOT-01>", ProductName: "疫苗（脱敏样例）"},
		{PointID: "point-vax-freezer-01", BatchRef: "<EXAMPLE_LOT-02>", ProductName: "疫苗（脱敏样例）"},
		{PointID: "point-vax-freezer-01", BatchRef: "<EXAMPLE_LOT-03>", ProductName: "冷藏药品（脱敏样例）"},
	}

	zone, err := ResolveZoneForEvent(profile, event, batches)
	if err != nil {
		t.Fatalf("多制品点位期望判定成功，实际报错：%v", err)
	}
	if zone.NeedsManual {
		t.Errorf("制品温区齐备时期望不转人工，实际（%s）", zone.Reason)
	}
	if len(zone.Products) != 2 {
		t.Fatalf("制品结论条数 = %d，期望 2", len(zone.Products))
	}

	vaccine, ok := zone.ProductZoneFor("疫苗（脱敏样例）")
	if !ok {
		t.Fatal("取不到疫苗制品的温区结论")
	}
	if vaccine.ZoneSource != ZoneSourceProductManual {
		t.Errorf("制品温区来源 = %s，期望 %s", vaccine.ZoneSource, ZoneSourceProductManual)
	}
	if !strings.Contains(vaccine.ZoneBasis, "疫苗储存和运输管理规范") {
		t.Errorf("制品温区出处应引用登记的规范条目，实际：%s", vaccine.ZoneBasis)
	}
	if len(vaccine.BatchRefs) != 2 {
		t.Errorf("疫苗制品下的批次 = %v，期望 2 个", vaccine.BatchRefs)
	}
	if !vaccine.OutOfRange {
		t.Error("11.4℃ 超出疫苗温区 2~8℃，该制品结论应判越限")
	}

	cold, ok := zone.ProductZoneFor("冷藏药品（脱敏样例）")
	if !ok {
		t.Fatal("取不到冷藏药品的温区结论")
	}
	if len(cold.BatchRefs) != 1 || cold.BatchRefs[0] != "<EXAMPLE_LOT-03>" {
		t.Errorf("冷藏药品下的批次 = %v，期望 [<EXAMPLE_LOT-03>]", cold.BatchRefs)
	}
	if !strings.Contains(cold.ZoneBasis, "未注明出处") {
		t.Errorf("未登记出处的制品应如实说明，实际：%s", cold.ZoneBasis)
	}
}

// TestResolveZoneForEvent_UnmappedProductGoesManual 在库批次无对应温区时转人工：
// 这批货受没受过这次温升说不清，不能默认它没事。
func TestResolveZoneForEvent_UnmappedProductGoesManual(t *testing.T) {
	profile := zoneProfile()
	profile.Products = []domain.ProductProfile{
		{ProductName: "疫苗（脱敏样例）", ZoneMinC: 2, ZoneMaxC: 8},
	}

	event := zoneEvent(11.4)
	batches := []domain.Batch{
		{PointID: "point-vax-freezer-01", BatchRef: "<EXAMPLE_LOT-01>", ProductName: "疫苗（脱敏样例）"},
		{PointID: "point-vax-freezer-01", BatchRef: "<EXAMPLE_LOT-09>", ProductName: "冷藏药品（脱敏样例）"},
	}

	zone, err := ResolveZoneForEvent(profile, event, batches)
	if err == nil {
		t.Fatal("在库批次没有对应温区配置时期望报错")
	}
	if e := errcode.As(err); e.Code != errcode.Validation {
		t.Errorf("错误码 = %s，期望 %s", e.Code, errcode.Validation)
	}
	if !zone.NeedsManual {
		t.Error("批次挂不上温区时期望标记 needs_manual")
	}
	if len(zone.Binding.UnmappedProducts) != 1 {
		t.Fatalf("未映射制品 = %v，期望 1 条", zone.Binding.UnmappedProducts)
	}
	if !strings.Contains(zone.Binding.UnmappedProducts[0], "冷藏药品（脱敏样例）") {
		t.Errorf("未映射制品应点名制品名，实际：%s", zone.Binding.UnmappedProducts[0])
	}
	if !strings.Contains(errcode.As(err).Details, "<EXAMPLE_LOT-09>") {
		t.Errorf("错误细节应点名批次号，实际：%s", errcode.As(err).Details)
	}
}

// TestResolveZoneForEvent_DuplicateProductGoesManual 制品重复登记按歧义处理。
func TestResolveZoneForEvent_DuplicateProductGoesManual(t *testing.T) {
	profile := zoneProfile()
	profile.Products = []domain.ProductProfile{
		{ProductName: "疫苗（脱敏样例）", ZoneMinC: 2, ZoneMaxC: 8},
		{ProductName: "疫苗（脱敏样例）", ZoneMinC: 2, ZoneMaxC: 10},
	}

	zone, err := ResolveZoneForEvent(profile, zoneEvent(5.0), nil)
	if err == nil {
		t.Fatal("制品重复登记时期望报错")
	}
	if !zone.NeedsManual {
		t.Error("制品重复登记时期望标记 needs_manual")
	}
	if !strings.Contains(errcode.As(err).Details, "歧义") {
		t.Errorf("错误细节应点明歧义，实际：%s", errcode.As(err).Details)
	}
}

// TestResolveZoneForEvent_PrecisionOverLimit 点位收紧的精度上限与设备自报精度共同判定：
// 超限即标「精度不足，仅供参考」，且该标注要能被归因与成稿引用。
func TestResolveZoneForEvent_PrecisionOverLimit(t *testing.T) {
	profile := zoneProfile() // 点位精度上限 0.5℃
	profile.Products = []domain.ProductProfile{
		{ProductName: "疫苗（脱敏样例）", ZoneMinC: 2, ZoneMaxC: 8},
	}

	event := zoneEvent(9.2)
	accuracy := 1.2
	event.AccuracyC = &accuracy

	zone, err := ResolveZoneForEvent(profile, event, nil)
	if err != nil {
		t.Fatalf("精度不足不阻断判定，实际报错：%v", err)
	}
	if !zone.Precision.Limited {
		t.Fatal("设备自报精度 1.2℃ 超出点位上限 0.5℃，期望标记精度不足")
	}
	if zone.Precision.Tag != PrecisionLimitedTag {
		t.Errorf("精度标注 = %q，期望 %q", zone.Precision.Tag, PrecisionLimitedTag)
	}
	if zone.Precision.LimitC != 0.5 {
		t.Errorf("采用的精度上限 = %.2f℃，期望 0.50℃", zone.Precision.LimitC)
	}
	if zone.Precision.LimitSource != domain.PrecisionLimitPointProfile {
		t.Errorf("精度上限来源 = %s，期望 %s", zone.Precision.LimitSource, domain.PrecisionLimitPointProfile)
	}
	if !strings.Contains(zone.Precision.Detail, "1.20") {
		t.Errorf("精度说明应写明设备自报值，实际：%s", zone.Precision.Detail)
	}

	// 点位上限更松时，仍以设备类规范口径为准：点位不能把规范放宽。
	loose := zoneProfile()
	loose.AccuracyLimitC = 2.0
	zone2, err := ResolveZoneForEvent(loose, event, nil)
	if err != nil {
		t.Fatalf("点位上限较松时期望判定成功，实际报错：%v", err)
	}
	if !zone2.Precision.Limited {
		t.Error("点位上限较松时仍应按设备类规范判精度不足")
	}
	if zone2.Precision.LimitC != domain.AccuracyLimitRecorderC {
		t.Errorf("采用的精度上限 = %.2f℃，期望 %.2f℃",
			zone2.Precision.LimitC, domain.AccuracyLimitRecorderC)
	}

	// 精度够用时不打标。
	ok := zoneEvent(9.2)
	zone3, err := ResolveZoneForEvent(profile, ok, nil)
	if err != nil {
		t.Fatalf("精度够用时期望判定成功，实际报错：%v", err)
	}
	if zone3.Precision.Limited {
		t.Error("设备自报精度 0.3℃ 未超限时不应打精度不足标记")
	}
}

// TestResolveZoneForEvent_PointMismatchGoesManual 点位对不上时转人工，不猜点位。
func TestResolveZoneForEvent_PointMismatchGoesManual(t *testing.T) {
	profile := zoneProfile()

	event := zoneEvent(9.2)
	event.PointID = "point-vaccine-fridge-02"

	zone, err := ResolveZoneForEvent(profile, event, nil)
	if err == nil {
		t.Fatal("点位不一致时期望报错")
	}
	if e := errcode.As(err); e.Code != errcode.Validation {
		t.Errorf("错误码 = %s，期望 %s", e.Code, errcode.Validation)
	}
	if !zone.NeedsManual {
		t.Error("点位不一致时期望标记 needs_manual")
	}
	if zone.Binding.Status != domain.BindingAmbiguous {
		t.Errorf("绑定状态 = %s，期望 %s", zone.Binding.Status, domain.BindingAmbiguous)
	}
}

// TestResolveZoneForEvent_MissingPointIDGoesManual 读数缺 point_id 时转人工。
func TestResolveZoneForEvent_MissingPointIDGoesManual(t *testing.T) {
	event := zoneEvent(9.2)
	event.PointID = "  "

	zone, err := ResolveZoneForEvent(zoneProfile(), event, nil)
	if err == nil {
		t.Fatal("读数缺 point_id 时期望报错")
	}
	if !zone.NeedsManual {
		t.Error("读数缺 point_id 时期望标记 needs_manual")
	}
	if !strings.Contains(errcode.As(err).Details, "point_id") {
		t.Errorf("错误细节应点名 point_id，实际：%s", errcode.As(err).Details)
	}
}

// TestResolveZoneForEvent_DeviceMappingAmbiguous 设备映射歧义时转人工，不猜设备。
func TestResolveZoneForEvent_DeviceMappingAmbiguous(t *testing.T) {
	profile := zoneProfile()
	profile.Devices = []domain.DeviceBinding{
		{DeviceID: "device-recorder-0001", DeviceName: "疫苗冷藏库温度记录仪"},
	}

	// 一、设备不在册 → 读数归属不明。
	unknown := zoneEvent(9.2)
	unknown.DeviceID = "device-recorder-9999"
	zone, err := ResolveZoneForEvent(profile, unknown, nil)
	if err == nil {
		t.Fatal("设备不在册时期望报错")
	}
	if !zone.NeedsManual {
		t.Error("设备不在册时期望标记 needs_manual")
	}
	if !strings.Contains(errcode.As(err).Details, "device-recorder-9999") {
		t.Errorf("错误细节应点名设备编号，实际：%s", errcode.As(err).Details)
	}

	// 二、同一设备在册两次 → 设备映射歧义。
	profile.Devices = append(profile.Devices, domain.DeviceBinding{
		DeviceID: "device-recorder-0001", DeviceName: "疫苗冷藏库温度记录仪（备用）",
	})
	zone2, err := ResolveZoneForEvent(profile, zoneEvent(9.2), nil)
	if err == nil {
		t.Fatal("设备重复登记时期望报错")
	}
	if !strings.Contains(errcode.As(err).Details, "歧义") {
		t.Errorf("错误细节应点明歧义，实际：%s", errcode.As(err).Details)
	}
	if !zone2.NeedsManual {
		t.Error("设备重复登记时期望标记 needs_manual")
	}
}

// TestResolveZoneForEvent_DeviceBoundOrUnmapped 设备在册时绑定成立；
// 点位没登记设备清单时不阻断，但结论要写明设备归属未核验。
func TestResolveZoneForEvent_DeviceBoundOrUnmapped(t *testing.T) {
	profile := zoneProfile()
	profile.Devices = []domain.DeviceBinding{
		{DeviceID: "device-recorder-0001", DeviceName: "疫苗冷藏库温度记录仪"},
	}

	zone, err := ResolveZoneForEvent(profile, zoneEvent(9.2), nil)
	if err != nil {
		t.Fatalf("设备在册时期望判定成功，实际报错：%v", err)
	}
	if zone.Binding.Status != domain.BindingBound {
		t.Errorf("绑定状态 = %s，期望 %s", zone.Binding.Status, domain.BindingBound)
	}
	if zone.Binding.DeviceName != "疫苗冷藏库温度记录仪" {
		t.Errorf("绑定设备名 = %q，期望 疫苗冷藏库温度记录仪", zone.Binding.DeviceName)
	}

	unregistered := zoneProfile()
	zone2, err := ResolveZoneForEvent(unregistered, zoneEvent(9.2), nil)
	if err != nil {
		t.Fatalf("点位未登记设备清单时期望判定成功，实际报错：%v", err)
	}
	if zone2.Binding.Status != domain.BindingPointOnly {
		t.Errorf("绑定状态 = %s，期望 %s", zone2.Binding.Status, domain.BindingPointOnly)
	}
	if !strings.Contains(zone2.Binding.Reason, "未核验") {
		t.Errorf("绑定说明应写明设备归属未核验，实际：%s", zone2.Binding.Reason)
	}
}

// TestResolveZoneForEvent_BatchFromOtherPointRejected 别点位的批次不得混进本次判定。
func TestResolveZoneForEvent_BatchFromOtherPointRejected(t *testing.T) {
	profile := zoneProfile()
	profile.Products = []domain.ProductProfile{
		{ProductName: "疫苗（脱敏样例）", ZoneMinC: 2, ZoneMaxC: 8},
	}
	batches := []domain.Batch{
		{PointID: "point-vaccine-fridge-02", BatchRef: "<EXAMPLE_LOT-04>", ProductName: "疫苗（脱敏样例）"},
	}

	zone, err := ResolveZoneForEvent(profile, zoneEvent(9.2), batches)
	if err == nil {
		t.Fatal("批次点位不一致时期望报错")
	}
	if !zone.NeedsManual {
		t.Error("批次点位不一致时期望标记 needs_manual")
	}
	if !strings.Contains(errcode.As(err).Details, "point-vaccine-fridge-02") {
		t.Errorf("错误细节应点名批次所属点位，实际：%s", errcode.As(err).Details)
	}
}

func TestMatchProfile(t *testing.T) {
	profile := &domain.PointProfile{PointID: "point-vax-freezer-01"}

	if err := MatchProfile(sampleAt(0, 5.0), profile); err != nil {
		t.Errorf("点位一致时期望匹配成功，实际报错：%v", err)
	}

	other := sampleAt(0, 5.0)
	other.PointID = "point-vaccine-fridge-02"
	if err := MatchProfile(other, profile); err == nil {
		t.Error("点位不一致时期望报错（歧义须转人工）")
	}
}
