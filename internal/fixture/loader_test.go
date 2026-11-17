package fixture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/assessment"
	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/ingest"
	"github.com/chaosforge/coldchain-temp-assessment/internal/rules"
)

const (
	// sampleRecordingsPath 是脱敏记录仪样例的路径。
	sampleRecordingsPath = "../../testdata/recordings_sample.csv"
	// sampleBatchesPath 是在库批次清单样例的路径。
	sampleBatchesPath = "../../testdata/batches_sample.csv"
	// sampleProfilePath 是点位温区配置样例的路径。
	sampleProfilePath = "../../testdata/point_profile.json"
	// samplePointID 是样例里的点位标识。
	samplePointID = "point-vax-freezer-01"
	// sampleRowCount 是记录仪样例的数据行数（不含表头）。
	sampleRowCount = 84
	// sampleBatchCount 是批次清单样例的数据行数（不含表头）。
	sampleBatchCount = 4
)

// loadSample 读取记录仪样例。
func loadSample(t *testing.T) []domain.TemperatureEvent {
	t.Helper()
	events, err := LoadRecordings(sampleRecordingsPath)
	if err != nil {
		t.Fatalf("读取记录仪样例失败：%v", err)
	}
	return events
}

// TestLoadRecordings_SampleShape 校验样例的行数、点位与时间栅格。
func TestLoadRecordings_SampleShape(t *testing.T) {
	events := loadSample(t)

	if len(events) != sampleRowCount {
		t.Fatalf("样例行数不符：期望 %d 行，实际 %d 行", sampleRowCount, len(events))
	}
	for i, e := range events {
		if e.PointID != samplePointID {
			t.Fatalf("第 %d 行点位不符：期望 %s，实际 %s", i+1, samplePointID, e.PointID)
		}
		if i == 0 {
			continue
		}
		gap := e.MeasuredAt.Sub(events[i-1].MeasuredAt)
		if gap != 5*time.Minute {
			t.Fatalf("第 %d 行与上一行的间隔不是 5 分钟：%s", i+1, gap)
		}
	}
	if !events[0].MeasuredAt.Equal(time.Date(2026, 10, 20, 8, 0, 0, 0, time.FixedZone("CST", 8*3600))) {
		t.Fatalf("首行时刻不符：%s", events[0].MeasuredAt)
	}
}

// TestLoadRecordings_FieldMapping 校验列到领域字段的映射。
func TestLoadRecordings_FieldMapping(t *testing.T) {
	events := loadSample(t)
	first := events[0]

	if first.DeviceID != "device-recorder-0001" {
		t.Fatalf("device_id 映射不符：%s", first.DeviceID)
	}
	if first.DeviceName != "疫苗冷藏库温度记录仪" {
		t.Fatalf("device_name 映射不符：%s", first.DeviceName)
	}
	if first.Unit != domain.UnitCelsius {
		t.Fatalf("unit 映射不符：%s", first.Unit)
	}
	if first.Source != "recorder" {
		t.Fatalf("source 映射不符：%s", first.Source)
	}
	if first.TemperatureC != 5.0 {
		t.Fatalf("temperature_c 映射不符：%v", first.TemperatureC)
	}
	if first.HumidityPct == nil || *first.HumidityPct != 58 {
		t.Fatalf("humidity_pct 映射不符：%v", first.HumidityPct)
	}
	if first.BatteryPct == nil || *first.BatteryPct != 92 {
		t.Fatalf("battery_pct 映射不符：%v", first.BatteryPct)
	}
	if first.AccuracyC == nil || *first.AccuracyC != 0.3 {
		t.Fatalf("accuracy_c 映射不符：%v", first.AccuracyC)
	}
	if first.AlarmFlag {
		t.Fatal("首行 alarm_flag 应为 false")
	}
	// 原始时间文本与时区偏移都要留住：偏移被吃掉之后，
	// 跨院区排班时整段时序会错位，而错位后的结论看上去仍然是合理的。
	if _, offset := first.MeasuredAt.Zone(); offset != 8*3600 {
		t.Fatalf("measured_at 时区偏移不是 +08:00：%d", offset)
	}
	if first.MeasuredAtRaw != "2026-10-20T08:00:00+08:00" {
		t.Fatalf("measured_at_raw 未保留原始文本：%s", first.MeasuredAtRaw)
	}
}

// TestLoadRecordings_ContainsThreeExcursionWindows 校验样例确实含 3 段越温窗口。
func TestLoadRecordings_ContainsThreeExcursionWindows(t *testing.T) {
	events := loadSample(t)
	profile, err := ingest.LoadProfileFile(sampleProfilePath)
	if err != nil {
		t.Fatalf("读取温区配置失败：%v", err)
	}
	zone, err := rules.ResolveZone(&profile)
	if err != nil {
		t.Fatalf("解析温区失败：%v", err)
	}

	type window struct {
		start string
		end   string
		peak  float64
	}
	var windows []window
	var current *window
	for _, e := range events {
		if zone.OutOfRange(e.TemperatureC) {
			if current == nil {
				current = &window{start: e.MeasuredAt.Format("15:04"), peak: e.TemperatureC}
			}
			current.end = e.MeasuredAt.Format("15:04")
			if e.TemperatureC > current.peak {
				current.peak = e.TemperatureC
			}
			continue
		}
		if current != nil {
			windows = append(windows, *current)
			current = nil
		}
	}
	if current != nil {
		windows = append(windows, *current)
	}

	if len(windows) != 3 {
		t.Fatalf("越温窗口段数不符：期望 3 段，实际 %d 段（%+v）", len(windows), windows)
	}
	expected := []window{
		{start: "09:00", end: "09:15", peak: 9.6},
		{start: "11:00", end: "12:15", peak: 15.2},
		{start: "14:00", end: "14:15", peak: 9.2},
	}
	for i, want := range expected {
		if windows[i] != want {
			t.Fatalf("第 %d 段越温窗口不符：期望 %+v，实际 %+v", i+1, want, windows[i])
		}
	}
}

// TestLoadRecordings_CarriesDeviceEvents 校验样例带上门开与断电事件行。
func TestLoadRecordings_CarriesDeviceEvents(t *testing.T) {
	events := loadSample(t)

	counts := map[string]int{}
	for _, e := range events {
		for _, kind := range e.Events {
			counts[kind]++
		}
	}
	if counts[domain.EventDoorOpen] == 0 {
		t.Fatal("样例里没有 door_open 事件行")
	}
	if counts[domain.EventPowerLoss] == 0 {
		t.Fatal("样例里没有 power_loss 事件行")
	}
	if counts[domain.EventDefrost] == 0 {
		t.Fatal("样例里没有 defrost 事件行")
	}
	for kind := range counts {
		if len(ingest.UnknownEventKinds([]string{kind})) > 0 {
			t.Fatalf("样例里出现未知事件类型：%s", kind)
		}
	}
	// 断电段记录仪自身会持续报出断电状态，所以事件行数多于一段。
	if counts[domain.EventPowerLoss] < 2 {
		t.Fatalf("power_loss 事件行数过少：%d", counts[domain.EventPowerLoss])
	}
}

// TestSampleFeedsAssessmentMainPath 校验样例可直接喂进判定主路径。
func TestSampleFeedsAssessmentMainPath(t *testing.T) {
	events := loadSample(t)
	profile, err := ingest.LoadProfileFile(sampleProfilePath)
	if err != nil {
		t.Fatalf("读取温区配置失败：%v", err)
	}

	result, err := assessment.Assess(events, &profile)
	if err != nil {
		t.Fatalf("样例走判定主路径失败：%v", err)
	}
	if result.Run.State != domain.RunStateAssessed {
		t.Fatalf("run 状态不符：期望 %s，实际 %s", domain.RunStateAssessed, result.Run.State)
	}
	if result.Window == nil {
		t.Fatal("样例应抽出越温窗口，实际为 nil")
	}
	if result.Window.PeakC != 9.6 {
		t.Fatalf("首个窗口峰值不符：期望 9.6，实际 %v", result.Window.PeakC)
	}
	if result.Window.SampleCount != 4 {
		t.Fatalf("首个窗口采样点数不符：期望 4，实际 %d", result.Window.SampleCount)
	}
	if result.Run.ExternalRef == "" || result.Run.IdempotencyKey == "" {
		t.Fatalf("run 缺少外部引用或幂等键：%+v", result.Run)
	}
}

// TestLoadRecordings_MissingFile 校验样例文件不存在时给 E_NOT_FOUND。
func TestLoadRecordings_MissingFile(t *testing.T) {
	_, err := LoadRecordings(filepath.Join(t.TempDir(), "nope.csv"))
	if err == nil {
		t.Fatal("样例文件不存在时应报错")
	}
	if code := errcode.As(err).Code; code != errcode.NotFound {
		t.Fatalf("错误码不符：期望 %s，实际 %s", errcode.NotFound, code)
	}
}

// TestLoadRecordings_MissingRequiredColumn 校验缺列时给 E_VALIDATION 并点名缺哪列。
func TestLoadRecordings_MissingRequiredColumn(t *testing.T) {
	path := writeTempCSV(t, "point_id,device_id,device_name,measured_at,unit,source,alarm_flag,events\n"+
		samplePointID+",device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20T08:00:00+08:00,C,recorder,false,\n")

	_, err := LoadRecordings(path)
	if err == nil {
		t.Fatal("缺少必需列时应报错")
	}
	if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("错误码不符：期望 %s，实际 %s", errcode.Validation, code)
	}
	if !strings.Contains(errcode.As(err).Details, "temperature_c") {
		t.Fatalf("错误细节未点名缺失列：%s", errcode.As(err).Details)
	}
}

// TestLoadRecordings_InvalidRow 校验坏行报错时带行号，且坏样例不被静默跳过。
func TestLoadRecordings_InvalidRow(t *testing.T) {
	cases := []struct {
		name    string
		content string
		expect  string
	}{
		{
			name:    "温度不是数值",
			content: samplePointID + ",device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20T08:00:00+08:00,abc,C,58,92,0.3,recorder,false,\n",
			expect:  "temperature_c 不是数值",
		},
		{
			name:    "布尔值非法",
			content: samplePointID + ",device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20T08:00:00+08:00,5.0,C,58,92,0.3,recorder,maybe,\n",
			expect:  "alarm_flag 不是布尔值",
		},
		{
			name:    "未知事件类型",
			content: samplePointID + ",device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20T08:00:00+08:00,5.0,C,58,92,0.3,recorder,false,compressor_alarm\n",
			expect:  "未知事件类型",
		},
		{
			name:    "时间不可解析",
			content: samplePointID + ",device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20 08:00:00,5.0,C,58,92,0.3,recorder,false,\n",
			expect:  "measured_at 不是带时区偏移的 RFC3339 时间",
		},
		{
			name:    "温度越量程",
			content: samplePointID + ",device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20T08:00:00+08:00,150.0,C,58,92,0.3,recorder,false,\n",
			expect:  "超出量程",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempCSV(t, "point_id,device_id,device_name,measured_at,temperature_c,unit,humidity_pct,battery_pct,accuracy_c,source,alarm_flag,events\n"+tc.content)
			_, err := LoadRecordings(path)
			if err == nil {
				t.Fatal("坏行应报错")
			}
			if code := errcode.As(err).Code; code != errcode.Validation && code != errcode.Range {
				t.Fatalf("错误码不符：期望 %s 或 %s，实际 %s", errcode.Validation, errcode.Range, code)
			}
			details := errcode.As(err).Details
			if !strings.Contains(details, tc.expect) {
				t.Fatalf("错误细节不符：期望包含 %q，实际 %q", tc.expect, details)
			}
			if !strings.Contains(details, "第 2 行") {
				t.Fatalf("错误细节未带行号：%q", details)
			}
		})
	}
}

// TestLoadBatches_SampleShape 校验批次清单的行数与字段映射。
func TestLoadBatches_SampleShape(t *testing.T) {
	batches, err := LoadBatches(sampleBatchesPath)
	if err != nil {
		t.Fatalf("读取批次清单样例失败：%v", err)
	}
	if len(batches) != sampleBatchCount {
		t.Fatalf("样例行数不符：期望 %d 行，实际 %d 行", sampleBatchCount, len(batches))
	}

	first := batches[0]
	if first.PointID != samplePointID {
		t.Fatalf("point_id 映射不符：%s", first.PointID)
	}
	if first.BatchRef != "<EXAMPLE_LOT-01>" {
		t.Fatalf("batch_ref 映射不符：%s", first.BatchRef)
	}
	if first.ProductName != "疫苗（脱敏样例）" {
		t.Fatalf("product_name 映射不符：%s", first.ProductName)
	}
	if first.Quantity != 120 {
		t.Fatalf("quantity 映射不符：%d", first.Quantity)
	}
	if first.ExpiryDate != "<EXAMPLE_EXPIRY-A>" {
		t.Fatalf("expiry_date 映射不符：%s", first.ExpiryDate)
	}
}

// TestLoadBatches_PreservesBatchRefVerbatim 校验占位批次号原样保留、不被改写。
//
// 批次号只能来自冷链台账。本系统若把它 trim 掉、改大小写或「规范化」一下，
// 写回台账时就对不上原批次，等于凭空造了一个查无此物的批次号。
func TestLoadBatches_PreservesBatchRefVerbatim(t *testing.T) {
	content := "point_id,batch_ref,product_name,quantity,expiry_date\n" +
		samplePointID + ", <EXAMPLE_LOT-09> ,疫苗（脱敏样例）,10,<EXAMPLE_EXPIRY-Z>\n"
	path := writeTempCSV(t, content)

	batches, err := LoadBatches(path)
	if err != nil {
		t.Fatalf("读取批次清单失败：%v", err)
	}
	if len(batches) != 1 {
		t.Fatalf("期望 1 条批次，实际 %d 条", len(batches))
	}
	if batches[0].BatchRef != " <EXAMPLE_LOT-09> " {
		t.Fatalf("batch_ref 被改写：期望 %q，实际 %q", " <EXAMPLE_LOT-09> ", batches[0].BatchRef)
	}
}

// TestLoadBatches_InvalidRow 校验坏批次行报错且不被静默跳过。
func TestLoadBatches_InvalidRow(t *testing.T) {
	cases := []struct {
		name    string
		content string
		expect  string
	}{
		{
			name:    "缺点位",
			content: ",<EXAMPLE_LOT-01>,疫苗（脱敏样例）,120,<EXAMPLE_EXPIRY-A>\n",
			expect:  "缺少 point_id",
		},
		{
			name:    "缺批次号",
			content: samplePointID + ",,疫苗（脱敏样例）,120,<EXAMPLE_EXPIRY-A>\n",
			expect:  "缺少 batch_ref",
		},
		{
			name:    "数量不是整数",
			content: samplePointID + ",<EXAMPLE_LOT-01>,疫苗（脱敏样例）,十,<EXAMPLE_EXPIRY-A>\n",
			expect:  "quantity 不是整数",
		},
		{
			name:    "数量为负",
			content: samplePointID + ",<EXAMPLE_LOT-01>,疫苗（脱敏样例）, -3 ,<EXAMPLE_EXPIRY-A>\n",
			expect:  "quantity 为负数",
		},
		{
			name:    "效期不合法",
			content: samplePointID + ",<EXAMPLE_LOT-01>,疫苗（脱敏样例）,120,2027/03/31\n",
			expect:  "expiry_date 不合法",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempCSV(t, "point_id,batch_ref,product_name,quantity,expiry_date\n"+tc.content)
			_, err := LoadBatches(path)
			if err == nil {
				t.Fatal("坏行应报错")
			}
			if code := errcode.As(err).Code; code != errcode.Validation {
				t.Fatalf("错误码不符：期望 %s，实际 %s", errcode.Validation, code)
			}
			details := errcode.As(err).Details
			if !strings.Contains(details, tc.expect) {
				t.Fatalf("错误细节不符：期望包含 %q，实际 %q", tc.expect, details)
			}
			if !strings.Contains(details, "第 2 行") {
				t.Fatalf("错误细节未带行号：%q", details)
			}
		})
	}
}

// TestLoadBatches_MissingRequiredColumn 校验批次清单缺列时报错。
func TestLoadBatches_MissingRequiredColumn(t *testing.T) {
	path := writeTempCSV(t, "point_id,batch_ref,product_name,quantity\n"+
		samplePointID+",<EXAMPLE_LOT-01>,疫苗（脱敏样例）,120\n")

	_, err := LoadBatches(path)
	if err == nil {
		t.Fatal("缺少必需列时应报错")
	}
	if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("错误码不符：期望 %s，实际 %s", errcode.Validation, code)
	}
	if !strings.Contains(errcode.As(err).Details, "expiry_date") {
		t.Fatalf("错误细节未点名缺失列：%s", errcode.As(err).Details)
	}
}

// writeTempCSV 把内容写到临时文件并返回路径。
func writeTempCSV(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sample.csv")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写临时样例失败：%v", err)
	}
	return path
}
