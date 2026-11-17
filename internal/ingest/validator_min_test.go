package ingest

import (
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// baseEvent 返回一条各字段都合法的线格式事件，测试在此基础上逐项破坏。
func baseEvent() RawEvent {
	t := 11.4
	h := 62.5
	b := 91.0
	a := 0.3
	return RawEvent{
		PointID:      "point-vax-freezer-01",
		DeviceID:     "device-recorder-0001",
		DeviceName:   "疫苗冷藏库温度记录仪",
		MeasuredAt:   "2026-10-20T03:15:00+08:00",
		TemperatureC: &t,
		Unit:         domain.UnitCelsius,
		HumidityPct:  &h,
		BatteryPct:   &b,
		AccuracyC:    &a,
		Source:       "recorder",
		AlarmFlag:    true,
		Events:       []string{domain.EventDoorOpen},
	}
}

// requireCode 断言错误码。
func requireCode(t *testing.T, err error, want errcode.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望错误码 %s，实际没有错误", want)
	}
	if got := errcode.As(err).Code; got != want {
		t.Fatalf("期望错误码 %s，实际 %s（%v）", want, got, err)
	}
}

func TestValidateMinimal_MissingPointID(t *testing.T) {
	raw := baseEvent()
	raw.PointID = "   "
	requireCode(t, ValidateMinimal(raw), errcode.Validation)
}

func TestValidateMinimal_UnparsableMeasuredAt(t *testing.T) {
	raw := baseEvent()
	raw.MeasuredAt = "2026-10-20 03:15:00" // 无 RFC3339 分隔符与偏移
	requireCode(t, ValidateMinimal(raw), errcode.Validation)

	raw2 := baseEvent()
	raw2.MeasuredAt = "2026-10-20T03:15:00" // 缺时区偏移
	requireCode(t, ValidateMinimal(raw2), errcode.Validation)
}

func TestValidateMinimal_TemperatureOutOfRange(t *testing.T) {
	high := 125.0
	raw := baseEvent()
	raw.TemperatureC = &high
	requireCode(t, ValidateMinimal(raw), errcode.Range)

	low := -95.5
	raw2 := baseEvent()
	raw2.TemperatureC = &low
	requireCode(t, ValidateMinimal(raw2), errcode.Range)
}

func TestValidateMinimal_UnknownEventKind(t *testing.T) {
	raw := baseEvent()
	raw.Events = []string{domain.EventDoorOpen, "compressor_alarm"}
	requireCode(t, ValidateMinimal(raw), errcode.Validation)
}

func TestValidateMinimal_MissingTemperature(t *testing.T) {
	raw := baseEvent()
	raw.TemperatureC = nil
	requireCode(t, ValidateMinimal(raw), errcode.Validation)
}

func TestValidateMinimal_RangeBoundariesAccepted(t *testing.T) {
	low := domain.MinTemperatureC
	raw := baseEvent()
	raw.TemperatureC = &low
	if err := ValidateMinimal(raw); err != nil {
		t.Fatalf("量程下界应被接受，实际 %v", err)
	}

	high := domain.MaxTemperatureC
	raw2 := baseEvent()
	raw2.TemperatureC = &high
	if err := ValidateMinimal(raw2); err != nil {
		t.Fatalf("量程上界应被接受，实际 %v", err)
	}
}

func TestNormalizeEvent_PreservesOffsetAndRawText(t *testing.T) {
	raw := baseEvent()
	ev, err := NormalizeEvent(raw)
	if err != nil {
		t.Fatalf("规范化不应失败：%v", err)
	}
	if ev.PointID != raw.PointID {
		t.Fatalf("point_id 应原样保留：%q", ev.PointID)
	}
	if ev.TemperatureC != *raw.TemperatureC {
		t.Fatalf("temperature_c 应原样保留：%v", ev.TemperatureC)
	}
	if ev.MeasuredAtRaw != raw.MeasuredAt {
		t.Fatalf("原始时间字符串应保留：%q", ev.MeasuredAtRaw)
	}
	_, offset := ev.MeasuredAt.Zone()
	if offset != 8*3600 {
		t.Fatalf("时区偏移应保留 +08:00，实际 %d 秒", offset)
	}
	if got := ev.MeasuredAt.Format(time.RFC3339); got != raw.MeasuredAt {
		t.Fatalf("格式化后应与原始时间一致：%q", got)
	}
	if len(ev.Events) != 1 || ev.Events[0] != domain.EventDoorOpen {
		t.Fatalf("设备事件应原样保留：%v", ev.Events)
	}
}

func TestNormalizeEvent_PropagatesError(t *testing.T) {
	raw := baseEvent()
	raw.PointID = ""
	_, err := NormalizeEvent(raw)
	requireCode(t, err, errcode.Validation)
}
