// Package rules 是冷链温控判定的规则基线。
//
// 规则基线承担三件事：温区比较、越限窗口抽取、规则归因候选。
// 它是模型归因的对照基线，也是模型不可达时的降级落点——
// 模型不可用时，系统仍然必须能给出一个可核对的数字结论，而不是一片空白。
//
// 本文件是温区闸门：判定一份读数属于哪个温区、这个温区是谁定的、
// 这条读数绑的是哪个点位哪台设备、精度够不够支撑量化结论。
//
// 闸门的立场只有一句：判不了就明说判不了，不猜、不顶替。
// 把「疫苗冷藏 2~8℃」硬编码成所有冷链的温区，
// 是这类系统最常见也最致命的错误——冷冻柜按 2~8℃ 判，结论全错，
// 而且看起来完全正常。
package rules

import (
	"fmt"
	"strings"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// 温区来源常量（与 domain 同名常量对齐，便于规则层直接引用）。
const (
	// ZoneSourcePointProfile：温区取自点位温区配置。
	ZoneSourcePointProfile = domain.ZoneSourcePointProfile
	// ZoneSourceProductManual：温区取自制品说明书条目。
	ZoneSourceProductManual = domain.ZoneSourceProductManual
)

// PrecisionLimitedTag 是精度不足时的固定标注文案。
//
// 固定文案不是偷懒：同一件事在接入层、归因结论、成稿三处各写一遍，
// 现场就会看到三种说法，复核的人根本不知道说的是同一件事。
const PrecisionLimitedTag = "精度不足，仅供参考"

// ZoneDecision 是一次温区判定结果。
//
// NeedsManual 为真表示「系统判不了、须人工接手」，此时 Reason 说明原因；
// 调用方据此把 run 落 needs_manual，而不是给出一个凑出来的结论。
//
// 一份判定结果同时带着三样可回溯的东西：
//   - ZoneSource / ZoneBasis：温区是谁定的、依据哪一条；
//   - Products：多制品点位逐制品的结论与各自的在库批次；
//   - Precision / Binding：精度够不够、设备归属核验到哪一步。
type ZoneDecision struct {
	// ZoneMinC / ZoneMaxC 是点位温区上下限（℃）。
	ZoneMinC float64 `json:"zone_min_c"`
	ZoneMaxC float64 `json:"zone_max_c"`
	// ZoneLabel 是温区的可读标签，如「2~8℃」。
	ZoneLabel string `json:"zone_label"`
	// ZoneSource 是温区来源：点位配置或制品说明书条目。
	ZoneSource string `json:"zone_source"`
	// ZoneBasis 是温区出处说明，回溯时给复核的人看。
	ZoneBasis string `json:"zone_basis"`
	// NeedsManual 表示系统判不了，须人工接手。
	NeedsManual bool `json:"needs_manual"`
	// Reason 是「判不了」或「判得了但有保留」的原因。
	Reason string `json:"reason,omitempty"`
	// SampleIntervalSec 是点位配置的标称采样周期（秒）；0 表示未配置。
	//
	// 它决定「连续超限多少分钟」的折算口径：按采样点数折算比按墙钟折算保守，
	// 采样中断的那段时间不会被算进越限时长里。
	SampleIntervalSec int `json:"sample_interval_sec,omitempty"`
	// MinExcursionMinutes 是成立正式越温窗口的最短持续时长（分钟）；0 表示不设门槛。
	//
	// 门槛的意义是分清「越温」与「波动」：取一次数超了 0.4℃ 是波动，
	// 连着超了二十分钟才是越温。两者对批次处置的含义完全不同。
	MinExcursionMinutes int `json:"min_excursion_minutes,omitempty"`
	// HysteresisC 是迟滞带（℃）：越限后要回落到 (zone_max_c - hysteresis_c)
	// 以内才算回到温区。
	//
	// 没有迟滞带时，贴着上界抖动的读数会把一次真实越温切成好几段短窗口，
	// 每段又都因为「不够长」判不出来——一次断电反而什么结论都没有。
	HysteresisC float64 `json:"hysteresis_c,omitempty"`
	// Products 是多制品点位逐制品的温区结论。
	Products []domain.ZoneProductDecision `json:"products,omitempty"`
	// Precision 是精度联动结论。
	Precision domain.PrecisionVerdict `json:"precision"`
	// Binding 是权威实体绑定链结论。
	Binding domain.BindingVerdict `json:"binding"`
}

// ResolveZone 解析点位温区配置。
//
// 只看配置，不看读数：这一步回答的是「这个点位按什么温区判」，
// 多制品点位逐制品给出各自的温区与出处。
//
// 配置缺失或不可信时，返回 needs_manual 为真的判定结果与 E_VALIDATION 错误：
// 不猜温区、不用默认值顶替。
func ResolveZone(profile *domain.PointProfile) (ZoneDecision, error) {
	if profile == nil {
		return manualDecision("缺少点位温区配置，无法判定越限"), errcode.Newf(errcode.Validation,
			"缺少点位温区配置（--profile），系统不猜测温区、不用默认值顶替")
	}
	if strings.TrimSpace(profile.PointID) == "" {
		return manualDecision("点位温区配置缺少 point_id"), errcode.Newf(errcode.Validation,
			"点位温区配置缺少 point_id，无法确认这份配置适用于哪个点位")
	}
	if profile.ZoneMaxC <= profile.ZoneMinC {
		return manualDecision(fmt.Sprintf("温区上下限非法：zone_min_c=%.2f 不小于 zone_max_c=%.2f",
				profile.ZoneMinC, profile.ZoneMaxC)), errcode.Newf(errcode.Validation,
				"点位 %s 的温区上下限非法：zone_min_c=%.2f，zone_max_c=%.2f",
				profile.PointID, profile.ZoneMinC, profile.ZoneMaxC)
	}
	if profile.ZoneMinC < domain.MinTemperatureC || profile.ZoneMaxC > domain.MaxTemperatureC {
		return manualDecision(fmt.Sprintf("温区超出记录仪量程：zone_min_c=%.2f，zone_max_c=%.2f",
				profile.ZoneMinC, profile.ZoneMaxC)), errcode.Newf(errcode.Validation,
				"点位 %s 的温区超出记录仪量程 [%.0f, %.0f]：zone_min_c=%.2f，zone_max_c=%.2f",
				profile.PointID, domain.MinTemperatureC, domain.MaxTemperatureC,
				profile.ZoneMinC, profile.ZoneMaxC)
	}

	products, err := resolveProductZones(profile)
	if err != nil {
		return manualDecision("点位温区配置里的制品温区不合法"), err
	}

	// 判定门槛三项（采样周期 / 最短越限时长 / 迟滞带）也要校验：
	// 它们直接决定「这一段算不算越温」，配错了结论就整个是错的。
	if err := validateExcursionCriteria(profile); err != nil {
		return manualDecision("点位温区配置的判定门槛不合法"), err
	}

	return ZoneDecision{
		ZoneMinC:   profile.ZoneMinC,
		ZoneMaxC:   profile.ZoneMaxC,
		ZoneLabel:  ZoneLabel(profile.ZoneMinC, profile.ZoneMaxC),
		ZoneSource: ZoneSourcePointProfile,
		ZoneBasis: fmt.Sprintf("点位温区配置 point_id=%s 的 zone_min_c/zone_max_c",
			profile.PointID),
		SampleIntervalSec:   profile.SampleIntervalSec,
		MinExcursionMinutes: profile.MinExcursionMinutes,
		HysteresisC:         profile.HysteresisC,
		Products:            products,
		Binding: domain.BindingVerdict{
			Status:  domain.BindingPointOnly,
			PointID: profile.PointID,
			Reason:  "点位已确认；该点位未登记设备清单，设备归属未核验",
		},
	}, nil
}

// validateExcursionCriteria 校验判定门槛三项。
//
// 门槛配错有三种典型：采样周期与最短越限时长为负（明显是配置写错）、
// 迟滞带为负（等于把退出阈值抬到上界之上，越限窗口永远关不掉）、
// 迟滞带不小于温区宽度（迟滞带盖住了整个温区，越限窗口同样关不掉）。
// 这三种都会让「这一段算不算越温」变得没法回答，一律转人工。
func validateExcursionCriteria(profile *domain.PointProfile) error {
	if profile.SampleIntervalSec < 0 {
		return errcode.Newf(errcode.Validation,
			"点位 %s 的 sample_interval_sec 为负：%d", profile.PointID, profile.SampleIntervalSec)
	}
	if profile.MinExcursionMinutes < 0 {
		return errcode.Newf(errcode.Validation,
			"点位 %s 的 min_excursion_minutes 为负：%d", profile.PointID, profile.MinExcursionMinutes)
	}
	if profile.HysteresisC < 0 {
		return errcode.Newf(errcode.Validation,
			"点位 %s 的 hysteresis_c 为负：%.2f", profile.PointID, profile.HysteresisC)
	}
	if width := profile.ZoneMaxC - profile.ZoneMinC; profile.HysteresisC >= width {
		return errcode.Newf(errcode.Validation,
			"点位 %s 的 hysteresis_c=%.2f 不小于温区宽度 %.2f℃，迟滞带会盖住整个温区",
			profile.PointID, profile.HysteresisC, width)
	}
	return nil
}

// resolveProductZones 把点位配置里的 products[] 解析成逐制品的温区结论。
//
// 制品名重复登记按歧义处理：同一个制品在配置里出现两条温区，
// 就没法说「这批货按哪条温区判」，也就没法说它在不在温区里。
func resolveProductZones(profile *domain.PointProfile) ([]domain.ZoneProductDecision, error) {
	if len(profile.Products) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(profile.Products))
	out := make([]domain.ZoneProductDecision, 0, len(profile.Products))
	for _, p := range profile.Products {
		name := strings.TrimSpace(p.ProductName)
		if name == "" {
			return nil, errcode.Newf(errcode.Validation,
				"点位 %s 的 products[] 里有制品没写 product_name", profile.PointID)
		}
		if seen[name] {
			return nil, errcode.Newf(errcode.Validation,
				"点位 %s 的 products[] 里制品 %q 重复登记，制品温区映射歧义",
				profile.PointID, name)
		}
		seen[name] = true

		if p.ZoneMaxC <= p.ZoneMinC {
			return nil, errcode.Newf(errcode.Validation,
				"点位 %s 的制品 %s 温区上下限非法：zone_min_c=%.2f，zone_max_c=%.2f",
				profile.PointID, name, p.ZoneMinC, p.ZoneMaxC)
		}
		if p.ZoneMinC < domain.MinTemperatureC || p.ZoneMaxC > domain.MaxTemperatureC {
			return nil, errcode.Newf(errcode.Validation,
				"点位 %s 的制品 %s 温区超出记录仪量程 [%.0f, %.0f]：zone_min_c=%.2f，zone_max_c=%.2f",
				profile.PointID, name, domain.MinTemperatureC, domain.MaxTemperatureC,
				p.ZoneMinC, p.ZoneMaxC)
		}

		basis := strings.TrimSpace(p.ZoneRef)
		if basis == "" {
			basis = fmt.Sprintf("点位温区配置 products[] 中制品 %s 的条目（未注明出处）", name)
		}

		out = append(out, domain.ZoneProductDecision{
			ProductName: name,
			ZoneMinC:    p.ZoneMinC,
			ZoneMaxC:    p.ZoneMaxC,
			ZoneLabel:   ZoneLabel(p.ZoneMinC, p.ZoneMaxC),
			ZoneSource:  ZoneSourceProductManual,
			ZoneBasis:   basis,
		})
	}
	return out, nil
}

// ResolveZoneForEvent 把温区配置与一条读数绑定起来，给出完整的温区判定结论。
//
// 它在 ResolveZone 的基础上补三件事：
//   - 权威实体绑定链：point_id → device_id → batch_ref 三环对上没有；
//   - 精度联动：点位配置收紧的精度上限与设备自报精度共同决定结论可不可信；
//   - 批次映射：在库批次挂到各自的制品温区下。
//
// batches 可以为空（台账还没接上、或本次判定不涉批次）。
// 一旦给了批次，链路上有对不上的环节就转人工：批次在库却查不到温区，
// 意味着这批货受没受过这次温升说不清，而「说不清」不能写成「没事」。
func ResolveZoneForEvent(profile *domain.PointProfile, event domain.TemperatureEvent, batches []domain.Batch) (ZoneDecision, error) {
	zone, err := ResolveZone(profile)
	if err != nil {
		return zone, err
	}

	// 一、点位绑定：读数必须来自这份配置对应的点位。
	if err := MatchProfile(event, profile); err != nil {
		zone.NeedsManual = true
		zone.Reason = "点位与温区配置不匹配"
		zone.Binding = domain.BindingVerdict{
			Status:  domain.BindingAmbiguous,
			PointID: event.PointID,
			Reason:  err.Error(),
		}
		return zone, err
	}
	zone.Binding.PointID = profile.PointID
	zone.Binding.DeviceID = event.DeviceID

	// 二、设备绑定：点位登记了设备清单时，读数必须来自清单里的某台设备。
	deviceName, bindingStatus, bindErr := resolveDevice(profile, event)
	if bindErr != nil {
		zone.NeedsManual = true
		zone.Reason = "读数来源设备与点位登记清单对不上"
		zone.Binding.Status = domain.BindingAmbiguous
		zone.Binding.Reason = bindErr.Error()
		return zone, bindErr
	}
	zone.Binding.Status = bindingStatus
	zone.Binding.DeviceName = deviceName
	if bindingStatus == domain.BindingPointOnly {
		zone.Binding.Reason = fmt.Sprintf("点位 %s 未登记设备清单，读数设备 %q 的归属未核验",
			profile.PointID, event.DeviceID)
	}

	// 三、批次映射：在库批次挂到各自的制品温区下。
	if err := resolveBatches(&zone, profile, batches); err != nil {
		zone.NeedsManual = true
		zone.Reason = "在库批次与点位温区配置对不上"
		zone.Binding.Status = domain.BindingAmbiguous
		zone.Binding.Reason = err.Error()
		return zone, err
	}

	// 四、精度联动：点位上限与设备类规范取更严的一条。
	zone.Precision = resolvePrecision(profile, event)

	// 五、逐制品判当前读数是否越限。
	for i := range zone.Products {
		zone.Products[i].OutOfRange = event.TemperatureC < zone.Products[i].ZoneMinC ||
			event.TemperatureC > zone.Products[i].ZoneMaxC
	}

	return zone, nil
}

// resolveDevice 确认读数来自点位登记的哪台设备。
//
// 返回登记的设备名、绑定状态与错误。三种情况：
//   - 点位没登记设备清单 → point_only：点位是确定的，设备归属未核验，不阻断；
//   - 设备在册且唯一 → bound；
//   - 设备不在册或在册多次 → ambiguous：转人工。
func resolveDevice(profile *domain.PointProfile, event domain.TemperatureEvent) (string, string, error) {
	if len(profile.Devices) == 0 {
		return "", domain.BindingPointOnly, nil
	}
	if strings.TrimSpace(event.DeviceID) == "" {
		return "", domain.BindingAmbiguous, errcode.Newf(errcode.Validation,
			"温度事件缺少 device_id，无法确认读数来自哪台设备（点位 %s 登记了 %d 台设备）",
			profile.PointID, len(profile.Devices))
	}

	matches := 0
	name := ""
	for _, d := range profile.Devices {
		if strings.TrimSpace(d.DeviceID) == event.DeviceID {
			matches++
			name = d.DeviceName
		}
	}
	switch {
	case matches == 0:
		return "", domain.BindingAmbiguous, errcode.Newf(errcode.Validation,
			"设备 %s 不在点位 %s 的登记设备清单内，读数归属不明",
			event.DeviceID, profile.PointID)
	case matches > 1:
		return "", domain.BindingAmbiguous, errcode.Newf(errcode.Validation,
			"设备 %s 在点位 %s 登记了 %d 条，设备映射歧义",
			event.DeviceID, profile.PointID, matches)
	}
	return name, domain.BindingBound, nil
}

// resolveBatches 把在库批次挂到各自的制品温区下。
//
// 批次号只由冷链台账给出，本系统不造新号。批次挂不上温区时按歧义处理：
// 宁可让操作员去补配置，也不能让这批货在结论里悄悄消失。
func resolveBatches(zone *ZoneDecision, profile *domain.PointProfile, batches []domain.Batch) error {
	if len(batches) == 0 {
		return nil
	}

	unmapped := make([]string, 0)
	for _, b := range batches {
		if strings.TrimSpace(b.BatchRef) == "" {
			return errcode.Newf(errcode.Validation,
				"在库批次缺少 batch_ref，无法确认这批货挂在哪个点位上")
		}
		if strings.TrimSpace(b.PointID) != "" && b.PointID != profile.PointID {
			return errcode.Newf(errcode.Validation,
				"在库批次 %s 属于点位 %s，与本次判定点位 %s 不一致",
				b.BatchRef, b.PointID, profile.PointID)
		}

		matched := false
		for i := range zone.Products {
			if zone.Products[i].ProductName == b.ProductName {
				zone.Products[i].BatchRefs = append(zone.Products[i].BatchRefs, b.BatchRef)
				matched = true
				break
			}
		}
		if !matched {
			unmapped = append(unmapped, fmt.Sprintf("%s（批次 %s）", b.ProductName, b.BatchRef))
			continue
		}
		zone.Binding.BatchRefs = append(zone.Binding.BatchRefs, b.BatchRef)
	}

	if len(unmapped) > 0 {
		zone.Binding.UnmappedProducts = unmapped
		return errcode.Newf(errcode.Validation,
			"在库批次有 %d 个制品没有对应温区配置，系统不猜温区：%s",
			len(unmapped), strings.Join(unmapped, "、"))
	}
	return nil
}

// resolvePrecision 判定这条读数的精度够不够支撑量化结论。
//
// 两条上限同时生效：设备类规范口径（记录仪 0.5℃ / 温度计 1.0℃）与
// 点位配置里的 accuracy_limit_c。点位可以比规范更严，不能比规范更松；
// 两者一样严时取点位配置这条（它更具体，是这个点位自己定的）。
// 设备自报精度超限时，结论标「精度不足，仅供参考」。
func resolvePrecision(profile *domain.PointProfile, event domain.TemperatureEvent) domain.PrecisionVerdict {
	limit := domain.AccuracyLimitCForSource(event.Source)
	limitSource := domain.PrecisionLimitDeviceClass
	if profile.AccuracyLimitC > 0 && profile.AccuracyLimitC <= limit {
		limit = profile.AccuracyLimitC
		limitSource = domain.PrecisionLimitPointProfile
	}

	verdict := domain.PrecisionVerdict{
		LimitC:      limit,
		LimitSource: limitSource,
		DeclaredC:   event.AccuracyC,
	}

	// 接入层已按设备类规范标注过精度不足：取更严的那条，不重复标注。
	if event.Precision.Limited {
		verdict.Limited = true
		verdict.LimitC = event.Precision.LimitC
		verdict.LimitSource = domain.PrecisionLimitDeviceClass
		verdict.Tag = event.Precision.Tag
		verdict.Detail = event.Precision.Detail
		return verdict
	}

	if event.AccuracyC != nil && *event.AccuracyC > limit {
		verdict.Limited = true
		verdict.Tag = PrecisionLimitedTag
		verdict.Detail = fmt.Sprintf(
			"设备声明测温精度 %.2f℃，超出本次判定采用的精度上限 %.2f℃（%s）",
			*event.AccuracyC, limit, precisionLimitSourceText(limitSource))
	}
	return verdict
}

// precisionLimitSourceText 把精度上限来源翻成可读文案。
func precisionLimitSourceText(source string) string {
	switch source {
	case domain.PrecisionLimitPointProfile:
		return "点位温区配置的 accuracy_limit_c"
	default:
		return "该类设备的精度规范上限"
	}
}

// Usable 判断这份温区判定能不能拿来算越限窗口。
//
// 配置缺失、点位对不上、设备归属歧义、批次挂不上温区时，判定结果为
// needs_manual：那份温区上下限要么是零值、要么根本不是这个点位的温区，
// 拿它去比较温度会算出一个看起来正常的假窗口。
//
// 这个闸门放在判定结论自己身上，而不是散在各调用方的 if 里：
// 谁忘了检查 needs_manual，窗口这一步就产不出东西，而不是产出一个假的。
func (z ZoneDecision) Usable() bool {
	return !z.NeedsManual
}

// OutOfRange 判断温度是否越出温区。
//
// 温区是闭区间：恰好等于 zone_min_c 或 zone_max_c 的读数仍在区内。
//
// 判定不可用（needs_manual）时本方法没有意义——拿一份零值温区去比较，
// 等于替配置缺失的现场下结论。调用方须先过 ExtractWindow 的闸门。
func (z ZoneDecision) OutOfRange(temperatureC float64) bool {
	return temperatureC < z.ZoneMinC || temperatureC > z.ZoneMaxC
}

// ProductZoneFor 取某个制品的温区结论。
func (z ZoneDecision) ProductZoneFor(productName string) (domain.ZoneProductDecision, bool) {
	for _, p := range z.Products {
		if p.ProductName == productName {
			return p, true
		}
	}
	return domain.ZoneProductDecision{}, false
}

// MatchProfile 校验事件点位与温区配置点位一致。
//
// 点位是温控点的唯一主键：拿 A 冷库的温区去判 B 冷库的读数，结论从第一步就全错。
// 不一致时按歧义处理、转人工，而不是取其中一方凑合。
func MatchProfile(event domain.TemperatureEvent, profile *domain.PointProfile) error {
	if profile == nil {
		return errcode.Newf(errcode.Validation, "缺少点位温区配置，无法确认点位对应关系")
	}
	if strings.TrimSpace(event.PointID) == "" {
		return errcode.Newf(errcode.Validation, "温度事件缺少 point_id，无法与温区配置对应")
	}
	if event.PointID != profile.PointID {
		return errcode.Newf(errcode.Validation,
			"点位不一致：事件 point_id=%s，温区配置 point_id=%s",
			event.PointID, profile.PointID)
	}
	return nil
}

// ZoneLabel 生成温区的可读标签，如「2~8℃」。
func ZoneLabel(minC, maxC float64) string {
	return fmt.Sprintf("%g~%g℃", minC, maxC)
}

// manualDecision 构造一个「须转人工」的温区判定结果。
func manualDecision(reason string) ZoneDecision {
	return ZoneDecision{
		NeedsManual: true,
		Reason:      reason,
		ZoneSource:  ZoneSourcePointProfile,
	}
}
