// 本文件放的是「温区判定结论」的几块形状：制品温区结论、精度联动结论、
// 权威实体绑定链结论。它们放在 domain 而不是 rules 里，是因为判定步骤产物
//（越限窗口产物）也要带着它们落库——产物不能反过来依赖规则包，
// 否则规则的实现一动，库里存的历史产物就再也解不开了。

package domain

// 温区来源。
//
// 复核一条越温结论时，第一个问题永远是「这个温区是哪儿来的」。
// 只有两种来源，不允许第三种：点位温区配置，或制品说明书条目。
const (
	// ZoneSourcePointProfile：温区取自点位温区配置的 zone_min_c / zone_max_c。
	ZoneSourcePointProfile = "point_profile"
	// ZoneSourceProductManual：温区取自制品说明书条目（点位配置的 products[] 中登记）。
	ZoneSourceProductManual = "product_manual"
)

// 权威实体绑定链的状态。
const (
	// BindingBound：点位 → 设备 → 批次 三环都确认上了。
	BindingBound = "bound"
	// BindingPointOnly：只确认了点位，设备映射未登记、设备归属未核验。
	//
	// 它不是歧义：点位没登记设备清单时「设备是谁」无从谈起，
	// 但点位本身是确定的。结论照出，同时把这一处未核验写在明面上。
	BindingPointOnly = "point_only"
	// BindingAmbiguous：链条上有环节对不上（点位不一致、设备不在册或重复登记）。
	//
	// 这一档必须转人工：设备归属错了，这份读数就不是这台冷柜的读数，
	// 据此判出来的越温窗口与在库批次全是错的。
	BindingAmbiguous = "ambiguous"
)

// 精度上限来源。
const (
	// PrecisionLimitDeviceClass：精度上限取自该类设备的规范口径。
	PrecisionLimitDeviceClass = "device_class_spec"
	// PrecisionLimitPointProfile：精度上限取自点位温区配置的 accuracy_limit_c。
	PrecisionLimitPointProfile = "point_profile"
)

// ZoneProductDecision 是一个制品的温区判定结论。
//
// 同一台冰箱可能同时存疫苗与冷藏药品，两者温区不同：按点位温区一刀切，
// 会把温区要求更严的那批货判成「还在温区内」。所以多制品点位逐制品判定，
// 各自给结论。
type ZoneProductDecision struct {
	// ProductName 是制品名（与在库批次的 product_name 对应）。
	ProductName string `json:"product_name"`
	// ZoneMinC / ZoneMaxC 是该制品的温区上下限（℃）。
	ZoneMinC float64 `json:"zone_min_c"`
	ZoneMaxC float64 `json:"zone_max_c"`
	// ZoneLabel 是温区的可读标签，如「2~8℃」。
	ZoneLabel string `json:"zone_label"`
	// ZoneSource 是温区来源：制品说明书条目。
	ZoneSource string `json:"zone_source"`
	// ZoneBasis 是温区出处：说明书条款、规范条目或点位配置条目名。
	//
	// 出处要能回溯到一份可查的文件——「温区是 2~8℃」这句话本身没有约束力，
	// 「温区是 2~8℃，依据疫苗储存和运输管理规范（2017 年版）」才有。
	ZoneBasis string `json:"zone_basis"`
	// BatchRefs 是落在该制品温区下的在库批次号（只由冷链台账给出）。
	BatchRefs []string `json:"batch_refs,omitempty"`
	// OutOfRange 表示当前读数是否越出该制品的温区。
	OutOfRange bool `json:"out_of_range"`
}

// PrecisionVerdict 是精度联动结论。
//
// 设备自报的测温精度超出本次判定采用的精度上限时，读数只能用来发现
// 「这里发生了温度异常」，不足以支撑「超了 1.2℃」这样的量化结论。
// 标注必须一路带到归因与成稿，不能只停在接入环节。
type PrecisionVerdict struct {
	// Limited 表示精度不足，结论仅供参考。
	Limited bool `json:"limited"`
	// LimitC 是本次判定采用的精度上限（℃）。
	LimitC float64 `json:"limit_c"`
	// LimitSource 是上限来源：设备类规范口径或点位配置。
	LimitSource string `json:"limit_source"`
	// DeclaredC 是设备自报的测温精度（℃）；未声明时为 nil（缺测不冒充 0）。
	DeclaredC *float64 `json:"declared_c,omitempty"`
	// Tag 是精度不足时的固定标注文案：「精度不足，仅供参考」。
	Tag string `json:"tag,omitempty"`
	// Detail 是精度不足时的具体数值说明，供成稿引用。
	Detail string `json:"detail,omitempty"`
}

// BindingVerdict 是权威实体绑定链的结论。
//
// 绑定链是 point_id → device_id → batch_ref：
// 先确认这条读数来自哪个温控点，再确认它来自哪台设备，最后确认
// 哪些在库批次挂在那个点上。三环里任何一环对不上，
// 后面算出来的窗口与批次都挂不到实处。
type BindingVerdict struct {
	// Status 是绑定状态：bound / point_only / ambiguous。
	Status string `json:"status"`
	// PointID 是本次判定绑定的温控点。
	PointID string `json:"point_id,omitempty"`
	// DeviceID 是读数来源设备的标识。
	DeviceID string `json:"device_id,omitempty"`
	// DeviceName 是设备登记名。
	DeviceName string `json:"device_name,omitempty"`
	// BatchRefs 是绑定到的在库批次号。
	BatchRefs []string `json:"batch_refs,omitempty"`
	// UnmappedProducts 是没有对应温区配置的制品名。
	//
	// 它们被单独列出来而不是丢掉：在库有货、温区却没登记，
	// 这批货到底受没受过这次温升，说不清就得说「说不清」。
	UnmappedProducts []string `json:"unmapped_products,omitempty"`
	// Reason 说明绑定状态是怎么来的（未核验项或歧义原因）。
	Reason string `json:"reason,omitempty"`
}
