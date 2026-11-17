package bench

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/rules"
)

// 基准数据集的列头：与公开数据集的列名一致，表头驱动读列。
const testHeader = "timestamp,sensor_bundle_id,packaging_type,temp_internal_c,temp_setpoint_c," +
	"humidity_internal_pct,door_open_count,power_outage_count,defrost_cycle_flag," +
	"time_above_threshold_min,time_below_threshold_min,exposure_integral"

// writeDataset 写一份小数据集到临时目录。
func writeDataset(t *testing.T, rows []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vaccine_coldchain.csv")
	body := testHeader + "\n" + strings.Join(rows, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("写测试数据集失败：%v", err)
	}
	return path
}

// 默认样例行：设定点 5℃。
//
// 第 1 行在温区内但上报过开门；第 2、3 行越限（第 2 行同时上报断电）。
// 越限行的最小偏差是 4.001℃，据此推得的标注容差为 4.00℃，
// 温区取 1~9℃——两条越限行都落在温区外，窗口边界应当与标注段严丝合缝。
func defaultRows() []string {
	return []string{
		"2025-02-25 13:17:00,407,thermal_shipper,5.10,5.0,55.0,1,0,0,0,0,0.0",
		"2025-02-25 13:18:00,407,thermal_shipper,9.001,5.0,55.0,0,1,0,1,0,1.2",
		"2025-02-25 13:19:00,407,thermal_shipper,9.60,5.0,56.0,0,0,0,1,0,1.5",
	}
}

func TestLoadDataset_SplitsStreamsBySetpoint(t *testing.T) {
	path := writeDataset(t, []string{
		"2025-02-25 13:17:00,407,thermal_shipper,5.10,5.0,55.0,0,0,0,0,0,0.0",
		"2025-02-25 13:17:00,689,reefer_pallet,-70.10,-70.0,40.0,0,0,0,0,0,0.0",
		"2025-02-25 13:18:00,407,thermal_shipper,5.20,5.0,55.0,0,0,0,0,0,0.0",
		"2025-02-25 13:18:00,689,reefer_pallet,-70.20,-70.0,40.0,0,0,0,0,0,0.0",
	})

	ds, err := LoadDataset(path, Options{})
	if err != nil {
		t.Fatalf("读数据集失败：%v", err)
	}
	if ds.Records != 4 {
		t.Fatalf("记录数不对：%d（应为 4）", ds.Records)
	}
	if len(ds.Streams) != 2 {
		t.Fatalf("可比流条数不对：%d（应为 2：设定点 5℃ 与 -70℃ 各一条）", len(ds.Streams))
	}
	cold := ds.Streams[0]
	if cold.SetpointC != -70 {
		t.Fatalf("第一条流的设定点不对：%v（应按设定点升序，-70 在前）", cold.SetpointC)
	}
	if cold.ZoneMinC != -73 || cold.ZoneMaxC != -67 {
		t.Fatalf("温区不对：%v~%v（应为设定点 ±3℃）", cold.ZoneMinC, cold.ZoneMaxC)
	}
	if cold.SamplingIntervalSec != 60 {
		t.Fatalf("采样周期不对：%d（应为 60）", cold.SamplingIntervalSec)
	}
	if ds.Streams[1].SetpointC != 5 {
		t.Fatalf("第二条流的设定点不对：%v", ds.Streams[1].SetpointC)
	}
}

func TestLoadDataset_MapsDeviceEventsAndLabels(t *testing.T) {
	path := writeDataset(t, defaultRows())
	ds, err := LoadDataset(path, Options{})
	if err != nil {
		t.Fatalf("读数据集失败：%v", err)
	}
	stream := ds.Streams[0]
	if len(stream.Samples) != 3 {
		t.Fatalf("采样条数不对：%d", len(stream.Samples))
	}
	// 第 1 行：开门；第 2 行：断电；第 3 行：无事件。
	if len(stream.Samples[0].Events) != 1 || stream.Samples[0].Events[0] != domain.EventDoorOpen {
		t.Fatalf("开门事件没映射上：%v", stream.Samples[0].Events)
	}
	if len(stream.Samples[1].Events) != 1 || stream.Samples[1].Events[0] != domain.EventPowerLoss {
		t.Fatalf("断电事件没映射上：%v", stream.Samples[1].Events)
	}
	if len(stream.Samples[2].Events) != 0 {
		t.Fatalf("第三行不该有设备事件：%v", stream.Samples[2].Events)
	}
	if !stream.Labeled[1] || !stream.Labeled[2] {
		t.Fatalf("越限标注没读上：%v", stream.Labeled)
	}
	if stream.Labeled[0] {
		t.Fatal("温区内的那行不该被标成越限")
	}
	if len(stream.Causes[1]) != 1 || stream.Causes[1][0] != causePowerLoss {
		t.Fatalf("成因标注没读上：%v", stream.Causes[1])
	}
}

func TestLoadDataset_DerivesAnnotationTolerance(t *testing.T) {
	// 越限标注行的最小偏差是 4.001℃，向下取整到两位小数得 4.00℃。
	path := writeDataset(t, defaultRows())
	ds, err := LoadDataset(path, Options{})
	if err != nil {
		t.Fatalf("读数据集失败：%v", err)
	}
	if ds.AnnotationToleranceC != 4 {
		t.Fatalf("标注容差不对：%v（应为 4）", ds.AnnotationToleranceC)
	}
}

func TestLoadDataset_MissingFileAndMissingColumn(t *testing.T) {
	if _, err := LoadDataset(filepath.Join(t.TempDir(), "nope.csv"), Options{}); err == nil {
		t.Fatal("数据集不存在时应报错")
	} else if !strings.Contains(err.Error(), "fetch_datasets.sh") {
		t.Fatalf("缺数据集的报错应提示先取数：%v", err)
	}

	path := filepath.Join(t.TempDir(), "bad.csv")
	body := "timestamp,temp_internal_c\n2025-02-25 13:17:00,5.0\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("写文件失败：%v", err)
	}
	if _, err := LoadDataset(path, Options{}); err == nil {
		t.Fatal("缺列时应报错")
	} else if !strings.Contains(err.Error(), "缺少必需列") {
		t.Fatalf("缺列的报错应点名缺哪几列：%v", err)
	}
}

func TestRun_WindowErrorWithinOneSample(t *testing.T) {
	// 两行连续越限（9.4 / 9.8），数据集标注两行都越限。
	path := writeDataset(t, defaultRows())
	ds, err := LoadDataset(path, Options{})
	if err != nil {
		t.Fatalf("读数据集失败：%v", err)
	}
	report, err := Run(ds, Options{})
	if err != nil {
		t.Fatalf("跑基准失败：%v", err)
	}
	if report.Metrics.WindowErrorMaxSamples > 1 {
		t.Fatalf("窗口误差过大：%d 个采样周期（上限 1）", report.Metrics.WindowErrorMaxSamples)
	}
	stream := report.Streams[0]
	if stream.GroundTruthSegments == 0 {
		t.Fatal("标注段数为 0，基准没跑起来")
	}
	if stream.UnmatchedSegments != 0 {
		t.Fatalf("有 %d 段标注没对上窗口", stream.UnmatchedSegments)
	}
}

func TestRun_AttributionTop1MatchesDatasetCause(t *testing.T) {
	// 只有断电事件，且窗口内温度越限：规则基线的 Top-1 应当是 power_loss。
	path := writeDataset(t, defaultRows())
	ds, err := LoadDataset(path, Options{})
	if err != nil {
		t.Fatalf("读数据集失败：%v", err)
	}
	report, err := Run(ds, Options{})
	if err != nil {
		t.Fatalf("跑基准失败：%v", err)
	}
	stream := report.Streams[0]
	if stream.CauseSingleLabelWindows == 0 {
		t.Fatal("没有带成因标注的窗口，归因指标无从谈起")
	}
	if stream.CauseTop1Matched == 0 {
		t.Fatalf("Top-1 一致数为 0（分布 %v）", stream.Top1Distribution)
	}
}

func TestRun_ReportNotesExplainCaveats(t *testing.T) {
	path := writeDataset(t, defaultRows())
	ds, err := LoadDataset(path, Options{})
	if err != nil {
		t.Fatalf("读数据集失败：%v", err)
	}
	report, err := Run(ds, Options{})
	if err != nil {
		t.Fatalf("跑基准失败：%v", err)
	}
	joined := strings.Join(report.Notes, "\n")
	for _, want := range []string{"设定点工况", "容差", "成因标注唯一"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("口径说明里少了「%s」：%s", want, joined)
		}
	}
}

func TestDeterministicSignature_IgnoresElapsed(t *testing.T) {
	a := &Report{Metrics: Metrics{
		WindowErrorMaxSamples:  0,
		WindowErrorMeanSamples: 0,
		CauseTop1AgreementRate: 0.5,
		CauseTop1HitRate:       0.6,
		ElapsedSeconds:         1.5,
	}}
	b := &Report{Metrics: Metrics{
		WindowErrorMaxSamples:  0,
		WindowErrorMeanSamples: 0,
		CauseTop1AgreementRate: 0.5,
		CauseTop1HitRate:       0.6,
		ElapsedSeconds:         22.5,
	}}
	if DeterministicSignature(a) != DeterministicSignature(b) {
		t.Fatal("耗时不参与确定性签名，两份报告应同签名")
	}
	c := &Report{Metrics: Metrics{CauseTop1AgreementRate: 0.4}}
	if DeterministicSignature(a) == DeterministicSignature(c) {
		t.Fatal("一致率不同，签名应不同")
	}
}

func TestVerifyAnnotationTolerance_RejectsMismatch(t *testing.T) {
	path := writeDataset(t, defaultRows())
	ds, err := LoadDataset(path, Options{})
	if err != nil {
		t.Fatalf("读数据集失败：%v", err)
	}
	if err := VerifyAnnotationTolerance(ds, Options{ToleranceC: 4}); err != nil {
		t.Fatalf("容差一致时不该报错：%v", err)
	}
	err = VerifyAnnotationTolerance(ds, Options{ToleranceC: 3})
	if err == nil {
		t.Fatal("容差与数据集标注不一致时应报错")
	}
	if !strings.Contains(err.Error(), "不可比") {
		t.Fatalf("报错应说明窗口误差不可比：%v", err)
	}
}

func TestOptionsNormalize_DefaultsAndRejectsNegativeHysteresis(t *testing.T) {
	opts := Options{}
	if err := opts.normalize(); err != nil {
		t.Fatalf("补默认值不该报错：%v", err)
	}
	if opts.ToleranceC != DefaultToleranceC {
		t.Fatalf("容差默认值不对：%v", opts.ToleranceC)
	}
	if opts.ContextSamples != DefaultContextSamples {
		t.Fatalf("上下文默认值不对：%v", opts.ContextSamples)
	}
	if opts.MinExcursionMinutes != 0 {
		t.Fatalf("最短越限门槛默认值应为 0（不设门槛）：%v", opts.MinExcursionMinutes)
	}
	if err := (&Options{HysteresisC: -1}).normalize(); err == nil {
		t.Fatal("负的迟滞带应报错")
	}
}

func TestLabeledSpans_SplitsContiguousRuns(t *testing.T) {
	spans := labeledSpans([]bool{false, true, true, false, true})
	if len(spans) != 2 {
		t.Fatalf("段数不对：%d（应为 2）", len(spans))
	}
	if spans[0].first != 1 || spans[0].last != 2 {
		t.Fatalf("第一段区间不对：%+v", spans[0])
	}
	if spans[1].first != 4 || spans[1].last != 4 {
		t.Fatalf("第二段区间不对：%+v", spans[1])
	}
}

func TestWindowCauseLabels_UnionWithinWindow(t *testing.T) {
	stream := &Stream{Causes: [][]string{
		{causeDoorOpen},
		{},
		{causePowerLoss, causeDefrost},
	}}
	labels := windowCauseLabels(stream, 0, 2)
	if len(labels) != 3 {
		t.Fatalf("成因标注不对：%v", labels)
	}
	if labels[0] != causeDefrost || labels[1] != causeDoorOpen || labels[2] != causePowerLoss {
		t.Fatalf("成因标注应按字典序排好：%v", labels)
	}
}

// TestRunOnRealDataset_IfPresent 在本地已取到公开数据集时跑一遍真实规模基准。
//
// 数据集不在（未执行取数脚本）就跳过——单测不依赖网络与 270 MB 数据文件。
func TestRunOnRealDataset_IfPresent(t *testing.T) {
	path := filepath.Join("..", "..", "data", "datasets", "vcc-cpld", "vaccine_coldchain.csv")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("公开数据集未取到，跳过真实规模基准：%s", path)
	}
	report, err := RunPipeline(path, Options{})
	if err != nil {
		t.Fatalf("真实规模基准失败：%v", err)
	}
	if report.Records < 400000 {
		t.Fatalf("真实规模基准的记录数不足：%d", report.Records)
	}
	if report.Metrics.WindowErrorMaxSamples > 1 {
		t.Fatalf("窗口误差过大：%d 个采样周期", report.Metrics.WindowErrorMaxSamples)
	}
	if report.Metrics.ElapsedSeconds > 30 {
		t.Fatalf("基准耗时过长：%.2fs", report.Metrics.ElapsedSeconds)
	}
	t.Logf("真实规模基准：%d 行，窗口误差 %d，归因一致率 %.2f，耗时 %.2fs",
		report.Records, report.Metrics.WindowErrorMaxSamples,
		report.Metrics.CauseTop1AgreementRate, report.Metrics.ElapsedSeconds)
}

// TestZoneMatchesProductProfile_KeepsWindowSemantics 确认基准用的温区口径
// 与产品点位配置里的 2~8℃ 冷藏温区是同一个口径。
func TestZoneMatchesProductProfile_KeepsWindowSemantics(t *testing.T) {
	profile := domain.PointProfile{
		PointID:             "point-vax-freezer-01",
		ZoneMinC:            2,
		ZoneMaxC:            8,
		SampleIntervalSec:   60,
		MinExcursionMinutes: 1,
		HysteresisC:         0.5,
	}
	zone, err := rules.ResolveZone(&profile)
	if err != nil {
		t.Fatalf("解析点位温区失败：%v", err)
	}
	stream := &Stream{SetpointC: 5, ZoneMinC: 2, ZoneMaxC: 8}
	benchZone := buildZone(stream, Options{ToleranceC: 3, ContextSamples: 5, HysteresisC: 0.5})
	if benchZone.ZoneMinC != zone.ZoneMinC || benchZone.ZoneMaxC != zone.ZoneMaxC {
		t.Fatalf("基准温区与点位配置对不上：%v~%v vs %v~%v",
			benchZone.ZoneMinC, benchZone.ZoneMaxC, zone.ZoneMinC, zone.ZoneMaxC)
	}
}
