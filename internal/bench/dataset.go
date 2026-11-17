// Package bench 是公开冷链数据集上的离线对照基准。
//
// 基准回答三个问题，每个问题都对应一个可核对的数字：
//
//  1. **越限窗口抽得准不准**：窗口起止与数据集自带标注段的偏差，
//     折算成采样周期（几条采样点）后统计——这条是「窗口误差 ≤1 个采样周期」的来源。
//  2. **规则基线归因的 Top-1 与数据集自带成因标注一致多少**：
//     这条是后续模型归因（task 27–31）的对照基准，不是及格线。
//  3. **真实规模下跑不跑得动**：40 万条记录能不能在 30 秒内跑完。
//
// **数据集不是产品对接面。** 本包读的那份公开数据集里，每一行都是一次运输的
// 遥测快照（一条 shipment 记录），不存在「一台设备连续采样」这种形态——
// 同一个 sensor_bundle_id 的各行之间可能隔着几天，也可能是完全不同的货物。
// 基准据此把记录按**设定点工况**归成若干条可比流：同一条流里的记录设定点相同，
// 按原始时间戳升序拼接后喂给规则层。
//
// 这样拼出来的流只用来量算法在**真实遥测数值**上的行为
// （窗口边界落点、归因候选排序、耗时），**不代表任何真实设备的连续时序**，
// 也不能拿它去推「某台冰箱当天夜里几点升到几度」。
//
// 温区口径：以每条流自己的设定点为中心 ±ToleranceC 取温区。
// 默认容差 3℃——这个数不是拍脑袋定的，是从数据集自己的标注里读出来的：
// 被标注为「高于阈值」的记录，其相对设定点的偏差最小值恰好是 3.00℃
// （见 VerifyAnnotationTolerance）。容差取错，基准就变成自己拿尺子量自己。
package bench

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// 时间戳格式：数据集里的 timestamp 列形如 2025-02-25 13:17:00，不带时区。
const datasetTimeLayout = "2006-01-02 15:04:05"

// Options 是基准的取数与口径参数。
type Options struct {
	// ToleranceC 是温区相对设定点的容差（℃）；0 时取 DefaultToleranceC。
	ToleranceC float64
	// ContextSamples 是归因时取窗口前后多少条采样作为上下文；0 时取 DefaultContextSamples。
	//
	// 上下文的作用是让「温升速率」「湿度基线」这类特征有前后可依；
	// 取值太大则会把邻近别的异常的事件拉进来，取值太小则特征算不出来。
	ContextSamples int
	// MinExcursionMinutes 是成立正式越限窗口的最短持续时长（分钟）。
	//
	// 0 表示**不设门槛**：每一段连续越限都成立为一条正式窗口。
	// 基准之所以不设门槛：数据集的越限标注粒度就是 1 分钟
	//（被标注为「高于阈值」的记录，其偏差最小值恰好 3.00℃），
	// 而产品按「至少持续 N 分钟」把单点越限判为短时波动——
	// 门槛取 1 分钟时，1,866 段标注里只有 9 段能成立为正式窗口，
	// 其余全落进「短时波动」列表，窗口就无从比对了。
	//
	// 这不是说产品不该设门槛：门槛由点位配置决定（见 profile 的
	// min_excursion_minutes），基准要比的是**窗口抽在哪**，
	// 不是「门槛该定几分钟」。基准报告里另有一行按产品门槛复算的结果，
	// 说明同一批数据在产品门槛下会分成多少窗口、多少波动。
	MinExcursionMinutes int
	// HysteresisC 是迟滞带（℃）；0 时取 DefaultHysteresisC。
	HysteresisC float64
}

// 基准默认口径。
const (
	// DefaultToleranceC 是默认温区容差（℃）：设定点 ±3℃。
	DefaultToleranceC = 3.0
	// DefaultContextSamples 是默认归因上下文采样条数。
	DefaultContextSamples = 5
	// DefaultMinExcursionMinutes 是默认最短越限时长（分钟）：0，即不设门槛。
	//
	// 理由见 Options.MinExcursionMinutes：数据集的越限标注粒度是 1 分钟，
	// 门槛一旦设成 1 分钟，绝大多数标注段会被判成短时波动而不是窗口，
	// 窗口对齐就无从谈起。基准比的是窗口边界落点，不是门槛该定几分钟。
	DefaultMinExcursionMinutes = 0
	// DefaultHysteresisC 是默认迟滞带（℃）。
	DefaultHysteresisC = 0.5
)

// Record 是数据集的一行原始记录。
//
// 字段只留基准要用的那些：温度、设定点、设备标识、三类设备事件、
// 数据集自带的越限标注与成因标注。
type Record struct {
	// Timestamp 是记录时刻（UTC 解析；数据集时间戳不带时区）。
	Timestamp time.Time
	// SensorID 是数据集里的 sensor_bundle_id。
	SensorID string
	// DeviceName 是设备名（取 packaging_type）。
	DeviceName string
	// TempC 是箱内温度（℃）。
	TempC float64
	// SetpointC 是该条记录对应的设定点（℃）。
	SetpointC float64
	// HumidityPct 是箱内湿度；缺测时为 nil（不拿 0 顶替）。
	HumidityPct *float64
	// DoorOpen 表示该记录上报过开门。
	DoorOpen bool
	// PowerLoss 表示该记录上报过断电。
	PowerLoss bool
	// Defrost 表示该记录处于除霜周期。
	Defrost bool
	// AboveThreshold 是数据集自带的「高于阈值」标注。
	AboveThreshold bool
	// BelowThreshold 是数据集自带的「低于阈值」标注。
	BelowThreshold bool
	// ExposureIntegral 是数据集自带的累计暴露积分。
	ExposureIntegral float64
}

// Stream 是一条可比流：同一设定点工况下的记录按时间戳升序拼接。
type Stream struct {
	// Name 是流的标识（按设定点生成，如 cold-chain-5c）。
	Name string
	// SetpointC 是这条流的设定点（℃）。
	SetpointC float64
	// ZoneMinC / ZoneMaxC 是这条流的温区上下限（℃）。
	ZoneMinC float64
	ZoneMaxC float64
	// SamplingIntervalSec 是这条流的采样周期（秒），按相邻采样间隔中位数推定。
	SamplingIntervalSec int
	// Samples 是拼接后的采样序列（按时间戳升序）。
	Samples []domain.TemperatureEvent
	// Labeled 标出每条采样是否被数据集标注为「高于阈值」。
	Labeled []bool
	// Below 标出每条采样是否被数据集标注为「低于阈值」。
	Below []bool
	// Causes 是每条采样上的数据集成因标注（0~3 个）。
	Causes [][]string
}

// Dataset 是读进来的公开数据集。
type Dataset struct {
	// Path 是数据集文件路径。
	Path string
	// Records 是读到的数据行数（不含表头）。
	Records int
	// Streams 是按设定点归出的可比流，按设定点升序排列。
	Streams []Stream
	// AnnotationToleranceC 是从数据集标注里读出的阈值容差（℃）。
	//
	// 它是「高于阈值」标注记录相对设定点偏差的最小值——
	// 数据集自己认定越限的门槛。基准配置的容差应当与它一致。
	AnnotationToleranceC float64
}

// 成因标注常量：与规则层的 cause code 同名，便于逐条比对。
const (
	causeDoorOpen  = "door_open"
	causePowerLoss = "power_loss"
	causeDefrost   = "defrost"
)

// LoadDataset 读取公开冷链数据集 CSV。
//
// 列名按表头匹配，不写死列序：换一份同结构的导出文件不用改代码。
// 缺列即报错退出——基准跑不起来时说清楚是哪一列没有，
// 比默默按列号取数取出一堆错值要好。
func LoadDataset(path string, opts Options) (*Dataset, error) {
	if err := opts.normalize(); err != nil {
		return nil, err
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errcode.Newf(errcode.NotFound,
				"数据集不存在：%s（先运行 bash scripts/fetch_datasets.sh 取数）", path)
		}
		return nil, errcode.Newf(errcode.Validation, "打开数据集失败：%s", path)
	}
	defer func() { _ = f.Close() }()

	r := csv.NewReader(f)
	r.ReuseRecord = true
	header, err := r.Read()
	if err != nil {
		return nil, errcode.Newf(errcode.Validation, "读取数据集表头失败：%s", path)
	}
	idx, err := indexColumns(header)
	if err != nil {
		return nil, err
	}

	ds := &Dataset{Path: path}
	buckets := map[float64][]Record{}
	var order []float64

	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errcode.Newf(errcode.Validation, "读取数据集第 %d 行失败：%s", ds.Records+2, err)
		}
		parsed, ok, err := parseRecord(rec, idx)
		if err != nil {
			return nil, errcode.Newf(errcode.Validation, "解析数据集第 %d 行失败：%s", ds.Records+2, err)
		}
		if !ok {
			// 坏行不静默跳过：基准报告里的行数必须与文件对得上，
			// 少算几行而不说，指标就没法复现。
			return nil, errcode.Newf(errcode.Validation,
				"数据集第 %d 行字段不完整（缺时间戳或温度）", ds.Records+2)
		}
		ds.Records++
		if _, seen := buckets[parsed.SetpointC]; !seen {
			order = append(order, parsed.SetpointC)
		}
		buckets[parsed.SetpointC] = append(buckets[parsed.SetpointC], parsed)
	}

	sort.Float64s(order)
	for _, sp := range order {
		records := buckets[sp]
		sort.SliceStable(records, func(i, j int) bool {
			return records[i].Timestamp.Before(records[j].Timestamp)
		})
		stream, err := buildStream(records, sp, opts)
		if err != nil {
			return nil, err
		}
		ds.Streams = append(ds.Streams, *stream)
	}

	ds.AnnotationToleranceC = deriveAnnotationTolerance(ds.Streams)
	return ds, nil
}

// normalize 补齐缺省口径并校验取值。
func (o *Options) normalize() error {
	if o.ToleranceC <= 0 {
		o.ToleranceC = DefaultToleranceC
	}
	if o.ContextSamples <= 0 {
		o.ContextSamples = DefaultContextSamples
	}
	if o.HysteresisC < 0 {
		return errcode.Newf(errcode.Validation, "迟滞带不能为负：%v", o.HysteresisC)
	}
	return nil
}

// 需要的列名。
var requiredColumns = []string{
	"timestamp",
	"sensor_bundle_id",
	"packaging_type",
	"temp_internal_c",
	"temp_setpoint_c",
	"humidity_internal_pct",
	"door_open_count",
	"power_outage_count",
	"defrost_cycle_flag",
	"time_above_threshold_min",
	"time_below_threshold_min",
	"exposure_integral",
}

// indexColumns 建立列名到列号的映射，并校验必需列是否齐备。
func indexColumns(header []string) (map[string]int, error) {
	idx := make(map[string]int, len(header))
	for i, name := range header {
		idx[strings.TrimSpace(name)] = i
	}
	missing := make([]string, 0, len(requiredColumns))
	for _, name := range requiredColumns {
		if _, ok := idx[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, errcode.Newf(errcode.Validation,
			"数据集表头缺少必需列：%s", strings.Join(missing, "、"))
	}
	return idx, nil
}

// parseRecord 把一行 CSV 解析成 Record。
//
// 第二个返回值 false 表示这行该跳过（整行为空）。
func parseRecord(rec []string, idx map[string]int) (Record, bool, error) {
	get := func(name string) string {
		i := idx[name]
		if i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}

	rawTS := get("timestamp")
	if rawTS == "" {
		return Record{}, false, nil
	}
	ts, err := time.ParseInLocation(datasetTimeLayout, rawTS, time.UTC)
	if err != nil {
		return Record{}, false, errcode.Newf(errcode.Validation,
			"时间戳 %q 不是 %s 格式", rawTS, datasetTimeLayout)
	}

	tempC, err := parseFloat(get("temp_internal_c"))
	if err != nil {
		return Record{}, false, errcode.Newf(errcode.Validation, "temp_internal_c 解析失败：%s", err)
	}
	setpointC, err := parseFloat(get("temp_setpoint_c"))
	if err != nil {
		return Record{}, false, errcode.Newf(errcode.Validation, "temp_setpoint_c 解析失败：%s", err)
	}
	exposure, err := parseFloat(get("exposure_integral"))
	if err != nil {
		return Record{}, false, errcode.Newf(errcode.Validation, "exposure_integral 解析失败：%s", err)
	}

	var humidity *float64
	if raw := get("humidity_internal_pct"); raw != "" {
		v, err := parseFloat(raw)
		if err != nil {
			return Record{}, false, errcode.Newf(errcode.Validation, "humidity_internal_pct 解析失败：%s", err)
		}
		humidity = &v
	}

	door, err := parseCount(get("door_open_count"))
	if err != nil {
		return Record{}, false, errcode.Newf(errcode.Validation, "door_open_count 解析失败：%s", err)
	}
	power, err := parseCount(get("power_outage_count"))
	if err != nil {
		return Record{}, false, errcode.Newf(errcode.Validation, "power_outage_count 解析失败：%s", err)
	}
	defrost, err := parseCount(get("defrost_cycle_flag"))
	if err != nil {
		return Record{}, false, errcode.Newf(errcode.Validation, "defrost_cycle_flag 解析失败：%s", err)
	}
	above, err := parseCount(get("time_above_threshold_min"))
	if err != nil {
		return Record{}, false, errcode.Newf(errcode.Validation, "time_above_threshold_min 解析失败：%s", err)
	}
	below, err := parseCount(get("time_below_threshold_min"))
	if err != nil {
		return Record{}, false, errcode.Newf(errcode.Validation, "time_below_threshold_min 解析失败：%s", err)
	}

	return Record{
		Timestamp:        ts,
		SensorID:         get("sensor_bundle_id"),
		DeviceName:       get("packaging_type"),
		TempC:            tempC,
		SetpointC:        setpointC,
		HumidityPct:      humidity,
		DoorOpen:         door > 0,
		PowerLoss:        power > 0,
		Defrost:          defrost > 0,
		AboveThreshold:   above > 0,
		BelowThreshold:   below > 0,
		ExposureIntegral: exposure,
	}, true, nil
}

// parseFloat 解析浮点列；空串按缺测处理（返回 0,false 由调用方决定）。
func parseFloat(raw string) (float64, error) {
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, err
	}
	return v, nil
}

// parseCount 解析整数列。
func parseCount(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		f, ferr := strconv.ParseFloat(raw, 64)
		if ferr != nil {
			return 0, err
		}
		return int(f), nil
	}
	return v, nil
}

// buildStream 把同一设定点工况的记录拼成一条可比流。
func buildStream(records []Record, setpointC float64, opts Options) (*Stream, error) {
	stream := &Stream{
		Name:      streamName(setpointC),
		SetpointC: setpointC,
		ZoneMinC:  setpointC - opts.ToleranceC,
		ZoneMaxC:  setpointC + opts.ToleranceC,
		Samples:   make([]domain.TemperatureEvent, 0, len(records)),
		Labeled:   make([]bool, 0, len(records)),
		Below:     make([]bool, 0, len(records)),
		Causes:    make([][]string, 0, len(records)),
	}

	for _, rec := range records {
		events := make([]string, 0, 3)
		causes := make([]string, 0, 3)
		if rec.DoorOpen {
			events = append(events, domain.EventDoorOpen)
			causes = append(causes, causeDoorOpen)
		}
		if rec.PowerLoss {
			events = append(events, domain.EventPowerLoss)
			causes = append(causes, causePowerLoss)
		}
		if rec.Defrost {
			events = append(events, domain.EventDefrost)
			causes = append(causes, causeDefrost)
		}

		stream.Samples = append(stream.Samples, domain.TemperatureEvent{
			PointID:       "vcc-cpld-" + stream.Name,
			DeviceID:      rec.SensorID,
			DeviceName:    rec.DeviceName,
			MeasuredAt:    rec.Timestamp,
			MeasuredAtRaw: rec.Timestamp.Format(datasetTimeLayout),
			TemperatureC:  rec.TempC,
			Unit:          "C",
			HumidityPct:   rec.HumidityPct,
			Source:        "recorder",
			Events:        events,
		})
		stream.Labeled = append(stream.Labeled, rec.AboveThreshold)
		stream.Below = append(stream.Below, rec.BelowThreshold)
		stream.Causes = append(stream.Causes, causes)
	}

	stream.SamplingIntervalSec = medianIntervalSeconds(stream.Samples)
	return stream, nil
}

// streamName 按设定点给流起名。
func streamName(setpointC float64) string {
	if setpointC == math.Trunc(setpointC) {
		return fmt.Sprintf("setpoint-%dc", int(setpointC))
	}
	return fmt.Sprintf("setpoint-%gc", setpointC)
}

// medianIntervalSeconds 按相邻采样间隔的中位数推定采样周期（秒）。
//
// 取中位数而不是平均值：一条流里偶尔缺几个采样，平均值会被缺口拉长，
// 拿被拉长的周期去折算越限时长，窗口时长就会虚高。
func medianIntervalSeconds(samples []domain.TemperatureEvent) int {
	if len(samples) < 2 {
		return 0
	}
	gaps := make([]int, 0, len(samples)-1)
	for i := 1; i < len(samples); i++ {
		d := samples[i].MeasuredAt.Sub(samples[i-1].MeasuredAt)
		if d <= 0 {
			continue
		}
		gaps = append(gaps, int(d.Seconds()))
	}
	if len(gaps) == 0 {
		return 0
	}
	sort.Ints(gaps)
	return gaps[len(gaps)/2]
}

// deriveAnnotationTolerance 从数据集标注里读出它自己认定的阈值容差（℃）。
//
// 取法：被标注为「高于阈值」的记录，其相对设定点的偏差最小值。
// 这就是数据集认定「这算越限」的门槛——基准配置的容差应当与它一致，
// 不一致时基准就没有可比性。
func deriveAnnotationTolerance(streams []Stream) float64 {
	best := math.Inf(1)
	for _, s := range streams {
		for i, rec := range s.Samples {
			if !s.Labeled[i] {
				continue
			}
			dev := math.Abs(rec.TemperatureC - s.SetpointC)
			if dev < best {
				best = dev
			}
		}
	}
	if math.IsInf(best, 1) {
		return 0
	}
	// 向下取整到两位小数，不四舍五入。
	//
	// 四舍五入可能把门槛抬高到越限行之上：最小偏差 3.005℃ 时，
	// 四舍五入得 3.01℃，温区上界落到 8.01℃，那条 8.005℃ 的越限行
	// 就被算成「还在温区内」——数据集标了越限、算法说没越限，
	// 基准上凭空多出一条 1 个采样周期的误差。向下取整保证
	// 「被数据集标为越限的行，一定在温区外」。
	return math.Floor(best*100) / 100
}
