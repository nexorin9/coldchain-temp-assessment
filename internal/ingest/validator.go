package ingest

import (
	"fmt"
	"strings"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// 本文件是入站温度事件的完整校验层：字段规则、单位换算与测温精度标注。
//
// 最小校验（validator_min.go）只保证「这条数据能被读懂」；
// 完整校验保证「这条数据可以拿去做判定」——设备与来源说得清、温度单位统一、
// 湿度电量在量程内、设备自报的精度够不够支撑结论，都在这一层落定。
//
// 三条底线贯穿本文件：
//   - 不猜：来源、单位、精度缺了就说缺了，不拿默认值顶替；
//   - 不丢：换算前的原始值与原始单位一并保留，换算不覆盖现场；
//   - 不改数：不合法的读数显式报错，不悄悄修正成「看起来对」的值。

// NormalizeUnit 归一化温度单位，返回本次判定实际使用的单位。
//
// 空值按摄氏度处理：字段名 temperature_c 已经声明了它的语义，
// 而现场设备不报单位时按摄氏度解释比报错更贴近真实。
// 其余取值只认 C 与 F，认不出的显式拒绝——把 K 或 R 当成 C 会得到
// 一个看起来正常的错误温度。
//
// 全角字母（「Ｃ」）折成半角再判：中文 Windows 上输入法手滑出来的全角单位
// 是同一个意思，为它丢掉整行数据不值当。换算前的原始单位仍由事件体的
// unit_raw 保留。
func NormalizeUnit(raw string) (string, error) {
	switch strings.ToUpper(normalizeLatinCell(raw)) {
	case "":
		return domain.UnitCelsius, nil
	case domain.UnitCelsius:
		return domain.UnitCelsius, nil
	case domain.UnitFahrenheit:
		return domain.UnitFahrenheit, nil
	default:
		return "", errcode.Newf(errcode.Validation,
			"unit 取值不合法：%q（合法取值：%s、%s）",
			raw, domain.UnitCelsius, domain.UnitFahrenheit)
	}
}

// ConvertToCelsius 把带单位的读数换算为摄氏度。
//
// 华氏换算按 (F-32)*5/9。返回值不做四舍五入：换算引入的误差应当留在数据里，
// 让下游的比较与统计看到真实数值，而不是被悄悄磨平的数。
func ConvertToCelsius(value float64, unit string) (float64, error) {
	switch unit {
	case domain.UnitCelsius:
		return value, nil
	case domain.UnitFahrenheit:
		return (value - 32.0) * 5.0 / 9.0, nil
	default:
		return 0, errcode.Newf(errcode.Validation,
			"未知温度单位：%q（合法取值：%s、%s）",
			unit, domain.UnitCelsius, domain.UnitFahrenheit)
	}
}

// AccuracyLimitCFor 返回该来源适用的测温精度规范上限（℃）。
//
// 记录仪与边缘网关转发按记录仪口径（0.5℃）；人工抄录按温度计口径（1.0℃）。
// 点位温区配置里的 accuracy_limit_c 可进一步收紧该上限，那一步由温区判定承担。
//
// 口径本身放在 domain：接入层与规则层用的是同一把尺，两处各算一次，
// 早晚会对不上，而对不上的那天没人说得清哪边是对的。
func AccuracyLimitCFor(source string) float64 {
	return domain.AccuracyLimitCForSource(strings.TrimSpace(source))
}

// KnownSource 判断来源是否属于合法取值域。
//
// 全角字母先折半角再比：来源是固定几个词的枚举，全半角之争只可能是
// 输入法手滑，不该为它丢掉一条采样。
func KnownSource(source string) bool {
	canonical := normalizeLatinCell(source)
	for _, s := range domain.KnownSources {
		if s == canonical {
			return true
		}
	}
	return false
}

// Validate 做完整字段校验。
//
// 校验分两层，错误码不同：
//   - 字段缺失与取值不合法 → E_VALIDATION（补齐或改正即可重投）；
//   - 数值越界（湿度、电量、精度、温度量程）→ E_RANGE（读数本身不可信）。
//
// 区分这两类不是形式：前者是「你漏填了」，后者是「这个设备在说谎」，
// 现场处置方式完全不同。
func Validate(raw RawEvent) error {
	if err := ValidateMinimal(raw); err != nil {
		return err
	}

	// 设备标识与名称必填：地方冷链口径要求记录设备名称与编码，
	// 异常记录表要送药事与企业，没有设备名就追不到是哪台冰箱。
	if strings.TrimSpace(raw.DeviceID) == "" {
		return errcode.Newf(errcode.Validation, "缺少 device_id")
	}
	if strings.TrimSpace(raw.DeviceName) == "" {
		return errcode.Newf(errcode.Validation, "缺少 device_name")
	}

	source := canonicalSource(raw.Source)
	if source == "" {
		return errcode.Newf(errcode.Validation,
			"缺少 source（合法取值：%s）", strings.Join(domain.KnownSources, "、"))
	}
	if !KnownSource(source) {
		return errcode.Newf(errcode.Validation,
			"source 取值不合法：%q（合法取值：%s）",
			raw.Source, strings.Join(domain.KnownSources, "、"))
	}

	if raw.HumidityPct != nil {
		if h := *raw.HumidityPct; h < 0 || h > 100 {
			return errcode.Newf(errcode.Range,
				"humidity_pct=%.2f 超出量程 [0, 100]", h)
		}
	}
	if raw.BatteryPct != nil {
		if b := *raw.BatteryPct; b < 0 || b > 100 {
			return errcode.Newf(errcode.Range,
				"battery_pct=%.2f 超出量程 [0, 100]", b)
		}
	}
	if raw.AccuracyC != nil {
		// 精度声明只能是正数。声明「我的精度是 0℃」或负数，
		// 说明这台设备没正确填这一项，而不是它真的那么准。
		if a := *raw.AccuracyC; a <= 0 {
			return errcode.Newf(errcode.Range,
				"accuracy_c=%.2f 不是合法的测温精度声明（必须为正数）", a)
		}
	}

	return nil
}

// AnnotatePrecision 按来源的精度规范上限给读数打标注。
//
// 标注是「参考性说明」，不是「数据不合格」：读数照常进入判定，
// 但结论与成稿必须带上它，让人知道这个温度能信到什么程度。
func AnnotatePrecision(declared *float64, source string) domain.PrecisionNote {
	limit := AccuracyLimitCFor(source)
	note := domain.PrecisionNote{LimitC: limit, DeclaredC: declared}

	if declared == nil {
		note.Detail = "设备未声明测温精度，按该类设备规范上限判定"
		return note
	}
	if *declared > limit {
		note.Limited = true
		note.Tag = PrecisionLimitedTag
		note.Detail = fmt.Sprintf("设备声明测温精度 %.2f℃，超出该类设备规范上限 %.2f℃",
			*declared, limit)
	}
	return note
}

// PrecisionLimitedTag 是精度不足时的固定标注文案。
//
// 措辞是给现场看的：明确说明结论只能参考，不夸大、不替设备背书，
// 也不写成「数据无效」——数据本身仍是设备当时的读数。
const PrecisionLimitedTag = "精度不足，仅供参考"

// NormalizeEvent 把线格式事件校验后转成领域事件，即「规范化判定输入」。
//
// 这一步做三件事：
//  1. 完整字段校验（缺失、枚举、量程），不合法即报错；
//  2. 单位换算：温度一律归到摄氏度，同时保留原始数值与原始单位；
//  3. 精度标注：按来源取规范上限，超限即打「精度不足，仅供参考」。
//
// 产出是判定主路径第一段的可核对结果：字段名与 spec 最小数据格式一致，
// 时间保留原始偏移量与原始字符串，换算前的现场值查得到，
// 精度够不够也从事件本身看得出来。
func NormalizeEvent(raw RawEvent) (domain.TemperatureEvent, error) {
	if err := Validate(raw); err != nil {
		return domain.TemperatureEvent{}, err
	}
	measuredAt, err := ParseMeasuredAt(raw.MeasuredAt)
	if err != nil {
		return domain.TemperatureEvent{}, err
	}

	unitRaw := strings.TrimSpace(raw.Unit)
	unit, err := NormalizeUnit(raw.Unit)
	if err != nil {
		return domain.TemperatureEvent{}, err
	}
	celsius, err := ConvertToCelsius(*raw.TemperatureC, unit)
	if err != nil {
		return domain.TemperatureEvent{}, err
	}

	// 原始数值与原始单位一并保留：换算是给判定用的，
	// 现场复核「设备当时报的是多少」要用换算前的那一份。
	rawValue := *raw.TemperatureC

	events := make([]string, 0, len(raw.Events))
	events = append(events, raw.Events...)

	source := canonicalSource(raw.Source)

	return domain.TemperatureEvent{
		PointID:        strings.TrimSpace(raw.PointID),
		DeviceID:       strings.TrimSpace(raw.DeviceID),
		DeviceName:     strings.TrimSpace(raw.DeviceName),
		MeasuredAt:     measuredAt,
		MeasuredAtRaw:  strings.TrimSpace(raw.MeasuredAt),
		TemperatureC:   celsius,
		Unit:           domain.UnitCelsius,
		TemperatureRaw: &rawValue,
		UnitRaw:        unitRaw,
		HumidityPct:    raw.HumidityPct,
		BatteryPct:     raw.BatteryPct,
		AccuracyC:      raw.AccuracyC,
		Source:         source,
		AlarmFlag:      raw.AlarmFlag,
		Events:         events,
		Precision:      AnnotatePrecision(raw.AccuracyC, source),
	}, nil
}
