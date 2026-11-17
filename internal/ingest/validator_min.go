// Package ingest 负责入站温度事件的校验与规范化。
//
// 本包对应源产品主路径里的「事件接入」一段：读入事件 → 逐字段校验 →
// 产出规范化事件。校验失败一律带明确错误码返回（E_VALIDATION / E_RANGE），
// 绝不静默丢字段、绝不猜值：入站这一步丢了什么，后面整条判定链就基于
// 一个不存在的现场做结论。
//
// 校验分两层：
//   - validator_min.go：最小校验，保证「这条数据能被读懂」；
//   - validator.go：完整字段校验 + 单位换算 + 测温精度标注，
//     保证「这条数据可以拿去做判定」。
//
// 两层共用同一条规范化入口 NormalizeEvent，不存在「走完整校验」与
// 「只走最小校验」两条互不相干的解析路径——线上事件与本地样例走的是同一段代码。
package ingest

import (
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// RawEvent 是入站温度事件的线格式（wire format）。
//
// 与领域类型的差别在于「原始」两处：MeasuredAt 保持字符串（保留设备上报的
// 原始写法与时区偏移），TemperatureC 用指针以区分「没报」与「报了 0℃」。
type RawEvent struct {
	PointID      string   `json:"point_id"`
	DeviceID     string   `json:"device_id"`
	DeviceName   string   `json:"device_name"`
	MeasuredAt   string   `json:"measured_at"`
	TemperatureC *float64 `json:"temperature_c"`
	Unit         string   `json:"unit"`
	HumidityPct  *float64 `json:"humidity_pct"`
	BatteryPct   *float64 `json:"battery_pct"`
	AccuracyC    *float64 `json:"accuracy_c"`
	Source       string   `json:"source"`
	AlarmFlag    bool     `json:"alarm_flag"`
	Events       []string `json:"events"`
}

// ParseMeasuredAt 解析 RFC3339 且带时区偏移的时间字符串。
//
// 必须带偏移：只给本地时刻的记录仪上报在多院区、多时区排班下无法对齐，
// 这类「猜本地时间」的写法在现场经常造成整段时序错位，故显式拒绝。
// 解析成功后的 time.Time 保留原始偏移量。
func ParseMeasuredAt(raw string) (time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, errcode.Newf(errcode.Validation, "缺少 measured_at")
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, errcode.Newf(errcode.Validation,
			"measured_at 不是带时区偏移的 RFC3339 时间：%q", raw)
	}
	return t, nil
}

// UnknownEventKinds 返回 events[] 中不属于合法取值域的类型。
func UnknownEventKinds(events []string) []string {
	known := make(map[string]bool, len(domain.KnownEventKinds))
	for _, k := range domain.KnownEventKinds {
		known[k] = true
	}
	var unknown []string
	for _, e := range events {
		if !known[e] {
			unknown = append(unknown, e)
		}
	}
	return unknown
}

// ValidateMinimal 做最小字段校验：点位非空、时间可解析、温度在量程内、
// 设备事件类型合法。字段缺失与取值非法分别归到 E_VALIDATION 与 E_RANGE，
// 调用方据此区分「补齐即可重投」与「读数本身不可信」。
//
// 量程按摄氏度判定：先按 unit 换算再比量程。否则一台以华氏度上报的记录仪
// 报 200℉（合 93.3℃）会被误判成越量程——换算错了的读数比被拒的读数更危险。
func ValidateMinimal(raw RawEvent) error {
	if strings.TrimSpace(raw.PointID) == "" {
		return errcode.Newf(errcode.Validation, "缺少 point_id")
	}
	if _, err := ParseMeasuredAt(raw.MeasuredAt); err != nil {
		return err
	}
	if raw.TemperatureC == nil {
		return errcode.Newf(errcode.Validation, "缺少 temperature_c")
	}

	unit, err := NormalizeUnit(raw.Unit)
	if err != nil {
		return err
	}
	celsius, err := ConvertToCelsius(*raw.TemperatureC, unit)
	if err != nil {
		return err
	}
	if celsius < domain.MinTemperatureC || celsius > domain.MaxTemperatureC {
		if unit == domain.UnitCelsius {
			return errcode.Newf(errcode.Range,
				"temperature_c=%.2f 超出量程 [%.0f, %.0f]",
				celsius, domain.MinTemperatureC, domain.MaxTemperatureC)
		}
		return errcode.Newf(errcode.Range,
			"temperature_c=%.2f（单位 %s）换算后 %.2f℃，超出量程 [%.0f, %.0f]",
			*raw.TemperatureC, unit, celsius,
			domain.MinTemperatureC, domain.MaxTemperatureC)
	}

	if unknown := UnknownEventKinds(raw.Events); len(unknown) > 0 {
		return errcode.Newf(errcode.Validation,
			"events[] 含未知事件类型：%s（合法取值：%s）",
			strings.Join(unknown, "、"), strings.Join(domain.KnownEventKinds, "、"))
	}
	return nil
}
