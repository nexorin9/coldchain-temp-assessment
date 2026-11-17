// Package domain 定义冷链温控判定服务的核心领域类型。
//
// 这些类型是判定主路径上各步骤共用的数据形状：
//   - 入站：TemperatureEvent（一条记录仪温度事件）、PointProfile（点位温区配置）
//   - 权威侧：在库批次 Batch（只从冷链台账取，本系统不造新号）
//   - 产出：ExcursionWindow（规则抽出的越温窗口，见 window.go）
//
// 字段名与 spec.md 的最小数据格式逐字一致。入站环节只做「校验 + 规范化」，
// 不在这里下任何处置结论：批次能不能用由疫苗生产企业评估质量影响、
// 再由药学部主管确认，判定结论由后续步骤产生。
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// 温度单位。事件体的 temperature_c 字段名已声明数值为摄氏度，
// unit 是记录仪自身的显示单位元数据；单位换算在接入加固阶段处理。
const (
	UnitCelsius    = "C"
	UnitFahrenheit = "F"
)

// 设备事件类型（events[] 取值域）。只认这三类，未知类型显式拒绝，
// 不静默丢弃——门开、断电、除霜对归因结论影响不同，吞掉一条等于伪造现场。
const (
	EventDoorOpen  = "door_open"
	EventPowerLoss = "power_loss"
	EventDefrost   = "defrost"
)

// KnownEventKinds 是 events[] 的合法取值集合。
var KnownEventKinds = []string{EventDoorOpen, EventPowerLoss, EventDefrost}

// 事件来源枚举。来源不是形式字段：人工上传的读数是拿温度计现场抄的，
// 它的可信口径与记录仪自报的口径不同，精度判定按来源分别取阈值。
const (
	// SourceRecorder 是记录仪自身上报。
	SourceRecorder = "recorder"
	// SourceEdgeGateway 是院内边缘网关转发（转发的是记录仪读数，精度口径同记录仪）。
	SourceEdgeGateway = "edge-gateway"
	// SourceManualUpload 是人工抄录上传（现场读数，精度口径同温度计）。
	SourceManualUpload = "manual-upload"
)

// KnownSources 是 source 字段的合法取值集合。
var KnownSources = []string{SourceRecorder, SourceEdgeGateway, SourceManualUpload}

// 测温精度规范上限（℃）。
//
// 数值口径来自院内冷链记录设备的通行精度口径（见 spec 最小数据格式）：
// 记录仪 >0.5℃、温度计（人工抄录）>1℃ 即视为精度不足。
// 这不是拍脑袋的阈值：一台自称 ±2℃ 的记录仪报出的 9.2℃，与真实温升 1.2℃
// 在现场根本无法区分，据此下「超了 1.2℃」的结论会直接误导批次处置。
const (
	// AccuracyLimitRecorderC 是记录仪与边缘网关转发的精度规范上限。
	AccuracyLimitRecorderC = 0.5
	// AccuracyLimitManualUploadC 是人工抄录（温度计读数）的精度规范上限。
	AccuracyLimitManualUploadC = 1.0
)

// AccuracyLimitCForSource 返回该来源适用的测温精度规范上限（℃）。
//
// 记录仪与边缘网关转发按记录仪口径（0.5℃）；人工抄录按温度计口径（1.0℃）。
// 点位温区配置里的 accuracy_limit_c 可进一步收紧该上限，那一步由温区判定承担。
//
// 这个口径放在 domain 里，是因为接入层与规则层都要用同一把尺：
// 两处各算一次，早晚会对不上，而对不上的那天没人说得清哪边是对的。
func AccuracyLimitCForSource(source string) float64 {
	if strings.TrimSpace(source) == SourceManualUpload {
		return AccuracyLimitManualUploadC
	}
	return AccuracyLimitRecorderC
}

// 记录仪温度量程（摄氏度）。越限数据不进入判定：明显越量程的读数
// 往往意味着探头故障或量程配置错误，直接拿来判越温会得出错误结论。
const (
	MinTemperatureC = -80.0
	MaxTemperatureC = 120.0
)

// PrecisionNote 是一次读数的测温精度标注。
//
// 设备自报的测温精度超出该类设备的规范上限时，读数会被打上「精度不足，仅供参考」：
// 这类读数仍可用于发现「这里发生了温度异常」，但不足以支撑「超了 1.2℃」这样的
// 量化结论，更不能单独支撑批次处置判断。标注随事件一路带下去，
// 到归因结论与成稿时必须显式出现在显眼位置。
type PrecisionNote struct {
	// Limited 表示设备声明的精度超出规范上限。
	Limited bool `json:"limited"`
	// LimitC 是本次判定采用的精度规范上限（℃）。
	LimitC float64 `json:"limit_c"`
	// DeclaredC 是设备自报的测温精度（℃）；设备未声明时为 nil（缺测不冒充 0）。
	DeclaredC *float64 `json:"declared_c,omitempty"`
	// Tag 是精度不足时的固定标注文案：「精度不足，仅供参考」。
	Tag string `json:"tag,omitempty"`
	// Detail 是精度不足时的具体数值说明，供成稿引用。
	Detail string `json:"detail,omitempty"`
}

// TemperatureEvent 是一条记录仪上报的温度事件。
//
// MeasuredAt 保留解析后的时刻（时区偏移与原始上报一致）；
// MeasuredAtRaw 保留原始时间字符串，便于回看「设备当时到底报的是什么格式」，
// 也让后续排查时区与时钟回拨问题有据可查。
//
// 温度一律以摄氏度落在 TemperatureC 里：Unit 是该数值的单位，
// TemperatureRaw 与 UnitRaw 则是设备当初报上来的原始数值与原始单位，
// 换算前的现场值不会被换算吃掉——事后核对「设备当时到底报了什么」要用它们。
//
// 可选数值字段用指针：缺测就缺测，不用 0 冒充。
type TemperatureEvent struct {
	// EventID 是该采样在库里的标识（evt_ + 幂等键前缀）。
	// 入站时尚未落库，为空；从库里回读时才有值。
	EventID        string    `json:"event_id,omitempty"`
	PointID        string    `json:"point_id"`
	DeviceID       string    `json:"device_id,omitempty"`
	DeviceName     string    `json:"device_name,omitempty"`
	MeasuredAt     time.Time `json:"measured_at"`
	MeasuredAtRaw  string    `json:"measured_at_raw,omitempty"`
	TemperatureC   float64   `json:"temperature_c"`
	Unit           string    `json:"unit,omitempty"`
	TemperatureRaw *float64  `json:"temperature_raw,omitempty"`
	UnitRaw        string    `json:"unit_raw,omitempty"`
	HumidityPct    *float64  `json:"humidity_pct,omitempty"`
	BatteryPct     *float64  `json:"battery_pct,omitempty"`
	AccuracyC      *float64  `json:"accuracy_c,omitempty"`
	Source         string    `json:"source,omitempty"`
	AlarmFlag      bool      `json:"alarm_flag,omitempty"`
	Events         []string  `json:"events,omitempty"`
	// Precision 是测温精度标注，随事件一路透传到归因结论与成稿。
	Precision PrecisionNote `json:"precision"`
	// DataNotes 是数据形态标注（编码回退、缺测字段、截断字段、多余取值）。
	// 线格式投递的正常事件不带这一组标注，批量导入的历史文件才带。
	DataNotes DataNotes `json:"data_notes,omitempty"`
}

// DataNotes 是一次读数在「数据形态」上的标注。
//
// 现场记录仪导出的文件比规范样例脏：GBK 编码、中文设备名带生僻字、可选列空着、
// 表里多出两列厂家自有的列、某个字段塞进了一整段文本。这些形态都不该让导入失败，
// 但也不该悄悄咽下去——把「这台设备导出的是 GBK」「这个字段被截断了」
// 记在事件上，后面出结论的人才知道这份数据的成色。
//
// 标注只描述「这份数据长什么样」，不参与判定：越限判定看的是温度值本身。
type DataNotes struct {
	// Encoding 是这批数据的字符编码：utf-8 或 gbk。
	Encoding string `json:"encoding,omitempty"`
	// EncodingFallback 表示原始字节不是合法 UTF-8、已回退 GBK 解析。
	//
	// 这一位必须显式留着：回退解析出来的文本看着「像是对的」，
	// 但它已经不是设备当初写下的那个字节序列了，
	// 事后核对时需要知道这一点。
	EncodingFallback bool `json:"encoding_fallback"`
	// MissingFields 是本条采样缺测的字段名（列在表头里但这一行没值）。
	// 缺测就是缺测，不用 0 冒充。
	MissingFields []string `json:"missing_fields,omitempty"`
	// Truncated 表示本条采样有字段因超长被截断。
	Truncated bool `json:"truncated"`
	// TruncatedFields 是被截断的字段名（按字段名排序）。
	TruncatedFields []string `json:"truncated_fields,omitempty"`
	// ExtraValues 是本行多出来的取值个数（行长度超过表头），已忽略。
	ExtraValues int `json:"extra_values,omitempty"`
}

// ProductProfile 是一个制品在该点位上的温区要求。
// 同一台冰箱可能同时存疫苗与冷藏药品，两者温区不同，故按制品分别匹配。
type ProductProfile struct {
	ProductName string  `json:"product_name"`
	ZoneMinC    float64 `json:"zone_min_c"`
	ZoneMaxC    float64 `json:"zone_max_c"`
	// ZoneRef 是这条温区要求的出处（说明书条款、规范条目或配置条目名）。
	//
	// 它是给人回溯用的：温区写对了但说不出依据，
	// 复核时就得靠猜，而猜出来的温区是最容易出事的温区。
	ZoneRef string `json:"zone_ref,omitempty"`
}

// DeviceBinding 是点位登记的一条设备。
//
// 设备清单不是装饰：冷链台账里「哪个记录仪挂在哪个冷柜」经常对不上，
// 设备编码重复登记更是常见。读数来自哪台设备认不出来时，
// 这条读数就不能算作那个温控点的读数。
type DeviceBinding struct {
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name,omitempty"`
}

// PointProfile 是点位温区配置。
//
// 配置缺失时判定必须转人工（needs_manual），绝不猜温区、绝不顶替默认值：
// 把 2～8℃ 硬编码成所有冷链的温区，是这类系统最常见的致命错误。
type PointProfile struct {
	PointID             string           `json:"point_id"`
	ZoneMinC            float64          `json:"zone_min_c"`
	ZoneMaxC            float64          `json:"zone_max_c"`
	SampleIntervalSec   int              `json:"sample_interval_sec"`
	MinExcursionMinutes int              `json:"min_excursion_minutes"`
	HysteresisC         float64          `json:"hysteresis_c"`
	AccuracyLimitC      float64          `json:"accuracy_limit_c"`
	Products            []ProductProfile `json:"products,omitempty"`
	// Devices 是该点位登记的设备清单。
	//
	// 清单为空表示「没登记」而不是「有歧义」：此时点位本身仍是确定的，
	// 只是设备归属无从核验，结论会显式标注这一点。
	// 清单非空时，读数的 device_id 必须在册且唯一，否则按歧义转人工。
	Devices []DeviceBinding `json:"devices,omitempty"`
}

// Batch 是一个在库批次。batch_ref 只由冷链台账给出，本系统不生成新批次号。
//
// PointID 记录该批次所在的冷链点位：在库批次清单是按点位查的
// （台账侧按 point_id + at 查询），批次挂在哪个点位是它不可省的一部分，
// 少了它就没法判断「这批货当时在不在那个越温的冰箱里」。
type Batch struct {
	PointID     string `json:"point_id,omitempty"`
	BatchRef    string `json:"batch_ref"`
	ProductName string `json:"product_name"`
	Quantity    int    `json:"quantity"`
	ExpiryDate  string `json:"expiry_date"`
}

// EventIdempotencyKey 计算事件幂等键：sha256(point_id + measured_at)。
//
// 同一点位的同一次采样只该入库一行：记录仪重发、边缘网关重试、院内网络抖动，
// 都会让同一条数据到两次以上；不拦住就会在时序里留下重复点，
// 既撑大库表，也让「这段温升持续了多久」算错。
//
// 键里不含随机成分，也不含时间文本的原始写法：同一采样无论投递几次、
// 无论现场写成 +08:00 还是 UTC，都必须算出同一个键，否则去重就成了摆设。
func EventIdempotencyKey(pointID string, measuredAt time.Time) string {
	sum := sha256.Sum256([]byte(pointID + "|" + measuredAt.UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(sum[:])
}

// ExcursionWindow 定义在 window.go：越温窗口及其数据异常类型。
