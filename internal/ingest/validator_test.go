package ingest

import (
	"math"
	"testing"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// floatPtr 返回指向 v 的指针，便于构造带值的原始事件字段。
func floatPtr(v float64) *float64 { return &v }

// TestValidate_FahrenheitConversionKeepsRawValue 覆盖华氏度换算与原值保留。
//
// 现场有一批以华氏度显示的进口记录仪：读数要换算成摄氏度才能与温区比较，
// 但换算前的现场值不能被吃掉——事后复核「设备当时报的是多少」要用它。
func TestValidate_FahrenheitConversionKeepsRawValue(t *testing.T) {
	raw := baseEvent()
	raw.Unit = domain.UnitFahrenheit
	raw.TemperatureC = floatPtr(40.0) // 40℉ = 4.44℃
	raw.DeviceName = "疫苗冷藏库温度记录仪（华氏度显示）"

	event, err := NormalizeEvent(raw)
	if err != nil {
		t.Fatalf("华氏度事件不应失败：%v", err)
	}

	const wantC = 4.444444444444445 // (40-32)*5/9
	if math.Abs(event.TemperatureC-wantC) > 0.01 {
		t.Fatalf("换算结果应接近 %.4f℃（误差 ≤0.01℃），实际 %.4f℃", wantC, event.TemperatureC)
	}
	if event.TemperatureRaw == nil {
		t.Fatal("原始温度值 temperature_raw 不应为空")
	}
	if *event.TemperatureRaw != 40.0 {
		t.Fatalf("原始温度值应原样保留 40，实际 %v", *event.TemperatureRaw)
	}
	if event.UnitRaw != domain.UnitFahrenheit {
		t.Fatalf("原始单位应保留 %s，实际 %q", domain.UnitFahrenheit, event.UnitRaw)
	}
	if event.Unit != domain.UnitCelsius {
		t.Fatalf("规范化后温度的单位应是摄氏度，实际 %q", event.Unit)
	}
}

// TestValidate_FahrenheitOutOfRangeAfterConversion 覆盖换算后才暴露的越量程。
//
// 200℉ 合 93.3℃，在量程内；300℉ 合 148.9℃，越量程。
// 若拿原始数值直接比 [-80,120]，200℉ 会被误拒——那是一台好设备在报正常高温。
func TestValidate_FahrenheitOutOfRangeAfterConversion(t *testing.T) {
	raw := baseEvent()
	raw.Unit = domain.UnitFahrenheit
	raw.TemperatureC = floatPtr(200.0)
	if err := Validate(raw); err != nil {
		t.Fatalf("200℉（93.3℃）应通过量程校验，实际 %v", err)
	}

	raw2 := baseEvent()
	raw2.Unit = domain.UnitFahrenheit
	raw2.TemperatureC = floatPtr(300.0)
	requireCode(t, Validate(raw2), errcode.Range)
}

// TestValidate_UnknownUnitRejected 覆盖非法单位。
func TestValidate_UnknownUnitRejected(t *testing.T) {
	raw := baseEvent()
	raw.Unit = "K"
	requireCode(t, Validate(raw), errcode.Validation)

	raw2 := baseEvent()
	raw2.Unit = ""
	// 空单位按摄氏度解释（字段名已声明语义），不是错误。
	if err := Validate(raw2); err != nil {
		t.Fatalf("空单位应按摄氏度处理，实际 %v", err)
	}
}

// TestValidate_InvalidSourceRejected 覆盖 source 非法枚举与缺失。
func TestValidate_InvalidSourceRejected(t *testing.T) {
	raw := baseEvent()
	raw.Source = "email"
	requireCode(t, Validate(raw), errcode.Validation)

	raw2 := baseEvent()
	raw2.Source = ""
	requireCode(t, Validate(raw2), errcode.Validation)
}

// TestValidate_MissingDeviceNameRejected 覆盖 device_name 必填。
func TestValidate_MissingDeviceNameRejected(t *testing.T) {
	raw := baseEvent()
	raw.DeviceName = "  "
	requireCode(t, Validate(raw), errcode.Validation)

	raw2 := baseEvent()
	raw2.DeviceID = ""
	requireCode(t, Validate(raw2), errcode.Validation)
}

// TestValidate_PrecisionOverLimitAnnotated 覆盖精度超限标注。
//
// 记录仪规范上限 0.5℃：声明 1.2℃ 的读数仍进判定，但结论必须带
// 「精度不足，仅供参考」——这类读数能说明「这里出事了」，不能说明「超了 1.2℃」。
func TestValidate_PrecisionOverLimitAnnotated(t *testing.T) {
	raw := baseEvent()
	raw.AccuracyC = floatPtr(1.2)

	event, err := NormalizeEvent(raw)
	if err != nil {
		t.Fatalf("精度超限不应拒绝入站：%v", err)
	}
	if !event.Precision.Limited {
		t.Fatal("精度超限应打上 limited 标记")
	}
	if event.Precision.Tag != PrecisionLimitedTag {
		t.Fatalf("标注文案应为 %q，实际 %q", PrecisionLimitedTag, event.Precision.Tag)
	}
	if event.Precision.LimitC != domain.AccuracyLimitRecorderC {
		t.Fatalf("记录仪的规范上限应为 %.2f℃，实际 %.2f℃",
			domain.AccuracyLimitRecorderC, event.Precision.LimitC)
	}
	if event.Precision.DeclaredC == nil || *event.Precision.DeclaredC != 1.2 {
		t.Fatalf("设备声明的精度应保留 1.2，实际 %v", event.Precision.DeclaredC)
	}
	if event.Precision.Detail == "" {
		t.Fatal("精度不足应带具体数值说明")
	}
}

// TestValidate_PrecisionWithinLimitNotAnnotated 覆盖精度达标时不误标。
func TestValidate_PrecisionWithinLimitNotAnnotated(t *testing.T) {
	raw := baseEvent()
	raw.AccuracyC = floatPtr(0.3)

	event, err := NormalizeEvent(raw)
	if err != nil {
		t.Fatalf("规范化不应失败：%v", err)
	}
	if event.Precision.Limited {
		t.Fatal("精度达标不应打 limited 标记")
	}
	if event.Precision.Tag != "" {
		t.Fatalf("精度达标不应带标注文案，实际 %q", event.Precision.Tag)
	}
}

// TestValidate_ManualUploadUsesThermometerLimit 覆盖人工抄录按温度计口径取上限。
func TestValidate_ManualUploadUsesThermometerLimit(t *testing.T) {
	raw := baseEvent()
	raw.Source = domain.SourceManualUpload
	raw.AccuracyC = floatPtr(0.8)

	event, err := NormalizeEvent(raw)
	if err != nil {
		t.Fatalf("规范化不应失败：%v", err)
	}
	if event.Precision.Limited {
		t.Fatal("0.8℃ 对人工抄录（温度计口径 1.0℃）不算超限")
	}
	if event.Precision.LimitC != domain.AccuracyLimitManualUploadC {
		t.Fatalf("人工抄录的规范上限应为 %.2f℃，实际 %.2f℃",
			domain.AccuracyLimitManualUploadC, event.Precision.LimitC)
	}
}

// TestValidate_PrecisionUndeclaredKeepsLimit 覆盖设备未声明精度。
func TestValidate_PrecisionUndeclaredKeepsLimit(t *testing.T) {
	raw := baseEvent()
	raw.AccuracyC = nil

	event, err := NormalizeEvent(raw)
	if err != nil {
		t.Fatalf("规范化不应失败：%v", err)
	}
	if event.Precision.Limited {
		t.Fatal("未声明精度不应判定为超限")
	}
	if event.Precision.DeclaredC != nil {
		t.Fatal("未声明精度不应拿 0 冒充")
	}
	if event.Precision.LimitC != domain.AccuracyLimitRecorderC {
		t.Fatalf("未声明精度时也应带上规范上限，实际 %.2f℃", event.Precision.LimitC)
	}
}

// TestValidate_HumidityAndBatteryRange 覆盖湿度与电量量程。
func TestValidate_HumidityAndBatteryRange(t *testing.T) {
	raw := baseEvent()
	raw.HumidityPct = floatPtr(105.5)
	requireCode(t, Validate(raw), errcode.Range)

	raw2 := baseEvent()
	raw2.HumidityPct = floatPtr(-0.5)
	requireCode(t, Validate(raw2), errcode.Range)

	raw3 := baseEvent()
	raw3.BatteryPct = floatPtr(120.0)
	requireCode(t, Validate(raw3), errcode.Range)

	raw4 := baseEvent()
	raw4.BatteryPct = floatPtr(-3.0)
	requireCode(t, Validate(raw4), errcode.Range)

	// 边界值（0 与 100）合法。
	raw5 := baseEvent()
	raw5.HumidityPct = floatPtr(0)
	raw5.BatteryPct = floatPtr(100)
	if err := Validate(raw5); err != nil {
		t.Fatalf("湿度电量边界值应被接受，实际 %v", err)
	}
}

// TestValidate_NonPositiveAccuracyRejected 覆盖非法精度声明。
func TestValidate_NonPositiveAccuracyRejected(t *testing.T) {
	raw := baseEvent()
	raw.AccuracyC = floatPtr(0)
	requireCode(t, Validate(raw), errcode.Range)

	raw2 := baseEvent()
	raw2.AccuracyC = floatPtr(-0.5)
	requireCode(t, Validate(raw2), errcode.Range)
}

// TestValidate_UnknownEventKindStillRejected 覆盖设备事件类型仍然显式拒绝。
func TestValidate_UnknownEventKindStillRejected(t *testing.T) {
	raw := baseEvent()
	raw.Events = []string{domain.EventPowerLoss, "compressor_alarm"}
	requireCode(t, Validate(raw), errcode.Validation)
}

// TestNormalizeEvent_CelsiusKeepsRawValues 覆盖摄氏度事件的原值保留。
func TestNormalizeEvent_CelsiusKeepsRawValues(t *testing.T) {
	raw := baseEvent()

	event, err := NormalizeEvent(raw)
	if err != nil {
		t.Fatalf("规范化不应失败：%v", err)
	}
	if event.TemperatureRaw == nil || *event.TemperatureRaw != *raw.TemperatureC {
		t.Fatalf("摄氏度事件应保留原始值 %v，实际 %v", *raw.TemperatureC, event.TemperatureRaw)
	}
	if event.UnitRaw != domain.UnitCelsius {
		t.Fatalf("原始单位应保留 %s，实际 %q", domain.UnitCelsius, event.UnitRaw)
	}
	if event.DeviceID != raw.DeviceID || event.DeviceName != raw.DeviceName {
		t.Fatalf("设备标识与名称应保留：%q / %q", event.DeviceID, event.DeviceName)
	}
}
