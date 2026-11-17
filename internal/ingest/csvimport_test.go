package ingest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/store"
)

// 本文件覆盖记录仪导出 CSV 的批量导入。
//
// 三条主线：干净文件全量入库、含脏行时逐条给出跳过原因、干跑不落库
// 但统计与实跑一致。外加表头驱动、缺列、批内重复与库内重复几类边界。

// cleanImportCSV 是一份干净的四行导出。
//
// 点位、设备名与时间都是脱敏占位，不含患者或真实批号等标识。
const cleanImportCSV = `point_id,device_id,device_name,measured_at,temperature_c,unit,humidity_pct,battery_pct,accuracy_c,source,alarm_flag,events
point-vax-freezer-01,device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20T08:00:00+08:00,5.0,C,58,92,0.3,recorder,false,
point-vax-freezer-01,device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20T08:05:00+08:00,5.2,C,58,92,0.3,recorder,false,
point-vax-freezer-01,device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20T08:10:00+08:00,9.6,C,61,91,0.3,recorder,false,door_open
point-vax-freezer-01,device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20T08:15:00+08:00,5.1,C,58,92,0.3,recorder,false,
`

// dirtyImportCSV 在四行干净数据之间插入三类脏行：时间不可解析、温度越量程、
// 事件类型未知。导入后应剩两行干净数据入库，三行被逐条跳过。
const dirtyImportCSV = `point_id,device_id,device_name,measured_at,temperature_c,unit,humidity_pct,battery_pct,accuracy_c,source,alarm_flag,events
point-vax-freezer-01,device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20T08:00:00+08:00,5.0,C,58,92,0.3,recorder,false,
point-vax-freezer-01,device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20 08:05:00,5.2,C,58,92,0.3,recorder,false,
point-vax-freezer-01,device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20T08:10:00+08:00,150.0,C,58,92,0.3,recorder,false,
point-vax-freezer-01,device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20T08:15:00+08:00,9.6,C,61,91,0.3,recorder,false,compressor_alarm
point-vax-freezer-01,device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20T08:20:00+08:00,5.3,C,58,92,0.3,recorder,false,
`

// newImportTestStore 造一个内存库。
func newImportTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// countEvents 统计库内事件条数。
func countEvents(t *testing.T, st *store.Store) int {
	t.Helper()
	count, err := st.CountEvents(context.Background(), "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("统计事件条数失败：%v", err)
	}
	return count
}

// skipCodes 取跳过原因分类集合，便于断言「该有的原因分类都在」。
func skipCodes(reasons []SkipReason) map[string]int {
	codes := make(map[string]int, len(reasons))
	for _, r := range reasons {
		codes[r.Code]++
	}
	return codes
}

// TestImportTemperatureSeries_CleanFileImportsEveryRow 干净文件 → 全部入库。
func TestImportTemperatureSeries_CleanFileImportsEveryRow(t *testing.T) {
	st := newImportTestStore(t)

	result, err := ImportTemperatureSeries(context.Background(), st, []byte(cleanImportCSV), false)
	if err != nil {
		t.Fatalf("导入失败：%v", err)
	}

	if result.Imported != 4 {
		t.Fatalf("期望入库 4 条，实际 %d 条", result.Imported)
	}
	if result.Skipped != 0 {
		t.Fatalf("干净文件不应有跳过行，实际跳过 %d 条：%v", result.Skipped, result.SkipReasons)
	}
	if result.TotalRows != 4 {
		t.Fatalf("期望数据行 4 行，实际 %d 行", result.TotalRows)
	}
	if result.Duplicates != 0 {
		t.Fatalf("首次导入不应出现重复，实际 %d 条", result.Duplicates)
	}
	if count := countEvents(t, st); count != 4 {
		t.Fatalf("库内应有 4 条事件，实际 %d 条", count)
	}
	if len(result.SkipReasons) != 0 {
		t.Fatalf("跳过原因列表应为空，实际 %v", result.SkipReasons)
	}
}

// TestImportTemperatureSeries_DirtyRowsSkippedWithReasons 脏行逐条给出原因。
func TestImportTemperatureSeries_DirtyRowsSkippedWithReasons(t *testing.T) {
	st := newImportTestStore(t)

	result, err := ImportTemperatureSeries(context.Background(), st, []byte(dirtyImportCSV), false)
	if err != nil {
		t.Fatalf("导入失败：%v", err)
	}

	if result.Imported != 2 {
		t.Fatalf("期望入库 2 条，实际 %d 条", result.Imported)
	}
	if result.Skipped != 3 {
		t.Fatalf("期望跳过 3 条，实际 %d 条：%v", result.Skipped, result.SkipReasons)
	}
	if result.TotalRows != 5 {
		t.Fatalf("期望数据行 5 行，实际 %d 行", result.TotalRows)
	}
	if count := countEvents(t, st); count != 2 {
		t.Fatalf("库内应有 2 条事件（只入干净行），实际 %d 条", count)
	}

	codes := skipCodes(result.SkipReasons)
	for _, want := range []string{
		SkipUnparsableTime,
		SkipTemperatureOutOfRange,
		SkipUnknownEventKind,
	} {
		if codes[want] != 1 {
			t.Fatalf("跳过原因缺少分类 %s（实际分布：%v）", want, codes)
		}
	}

	// 每条原因都要带行号与可读文案：现场拿着行号去核对导出文件。
	for _, reason := range result.SkipReasons {
		if reason.Row < 2 {
			t.Fatalf("跳过原因未带行号：%+v", reason)
		}
		if !strings.Contains(reason.Reason, "第") {
			t.Fatalf("跳过原因文案未说明位置：%+v", reason)
		}
	}
}

// TestImportTemperatureSeries_DryRunDoesNotWrite 干跑不落库。
func TestImportTemperatureSeries_DryRunDoesNotWrite(t *testing.T) {
	st := newImportTestStore(t)

	result, err := ImportTemperatureSeries(context.Background(), st, []byte(dirtyImportCSV), true)
	if err != nil {
		t.Fatalf("干跑失败：%v", err)
	}

	if !result.DryRun {
		t.Fatalf("干跑结果应带 dry_run=true")
	}
	if result.Imported != 2 {
		t.Fatalf("干跑应报出将入库 2 条，实际 %d 条", result.Imported)
	}
	if result.Skipped != 3 {
		t.Fatalf("干跑应报出跳过 3 条，实际 %d 条", result.Skipped)
	}
	if count := countEvents(t, st); count != 0 {
		t.Fatalf("干跑不得落库，库内实际有 %d 条事件", count)
	}
}

// TestImportTemperatureSeries_DryRunMatchesRealRun 干跑与实跑统计一致。
//
// 这是「先干跑看看」这个动作的前提：两边的跳过条数、跳过原因与导入条数
// 必须完全一样，否则干跑给出的结论在实跑时不成立。
func TestImportTemperatureSeries_DryRunMatchesRealRun(t *testing.T) {
	dryStore := newImportTestStore(t)
	realStore := newImportTestStore(t)

	dry, err := ImportTemperatureSeries(context.Background(), dryStore, []byte(dirtyImportCSV), true)
	if err != nil {
		t.Fatalf("干跑失败：%v", err)
	}
	real, err := ImportTemperatureSeries(context.Background(), realStore, []byte(dirtyImportCSV), false)
	if err != nil {
		t.Fatalf("实跑失败：%v", err)
	}

	if dry.Imported != real.Imported || dry.Skipped != real.Skipped ||
		dry.TotalRows != real.TotalRows || dry.Duplicates != real.Duplicates {
		t.Fatalf("干跑与实跑统计不一致：干跑 %+v，实跑 %+v", dry, real)
	}

	dryJSON, _ := json.Marshal(dry.SkipReasons)
	realJSON, _ := json.Marshal(real.SkipReasons)
	if string(dryJSON) != string(realJSON) {
		t.Fatalf("干跑与实跑的跳过原因不一致：\n干跑 %s\n实跑 %s", dryJSON, realJSON)
	}
}

// TestImportTemperatureSeries_HeaderDriven 列序变化不影响解析结果。
//
// 现场不同型号的记录仪导出的列序不一样。写死列序的导入器在换设备那天
// 会整批读错——数字照样进库，只是温度和湿度换了位置。
func TestImportTemperatureSeries_HeaderDriven(t *testing.T) {
	reordered := `source,events,alarm_flag,accuracy_c,battery_pct,humidity_pct,unit,temperature_c,measured_at,device_name,device_id,point_id
recorder,,false,0.3,92,58,C,5.0,2026-10-20T08:00:00+08:00,疫苗冷藏库温度记录仪,device-recorder-0001,point-vax-freezer-01
`

	parsed, err := ParseTemperatureSeries([]byte(reordered))
	if err != nil {
		t.Fatalf("解析乱序列序文件失败：%v", err)
	}
	if len(parsed.Events) != 1 {
		t.Fatalf("期望解析出 1 条事件，实际 %d 条", len(parsed.Events))
	}
	if len(parsed.SkipReasons) != 0 {
		t.Fatalf("乱序列序不应产生跳过原因，实际 %v", parsed.SkipReasons)
	}

	event := parsed.Events[0]
	if event.PointID != "point-vax-freezer-01" {
		t.Fatalf("点位读错：%q", event.PointID)
	}
	if event.DeviceName != "疫苗冷藏库温度记录仪" {
		t.Fatalf("设备名读错：%q", event.DeviceName)
	}
	if event.TemperatureC != 5.0 {
		t.Fatalf("温度读错：%v", event.TemperatureC)
	}
	if event.MeasuredAtRaw != "2026-10-20T08:00:00+08:00" {
		t.Fatalf("原始时间文本丢失：%q", event.MeasuredAtRaw)
	}
	if event.HumidityPct == nil || *event.HumidityPct != 58 {
		t.Fatalf("湿度读错：%v", event.HumidityPct)
	}
}

// TestImportTemperatureSeries_MissingRequiredColumnFails 缺必需列 → 整份失败。
func TestImportTemperatureSeries_MissingRequiredColumnFails(t *testing.T) {
	st := newImportTestStore(t)

	missing := `point_id,device_id,measured_at,temperature_c,source
point-vax-freezer-01,device-recorder-0001,2026-10-20T08:00:00+08:00,5.0,recorder
`

	_, err := ImportTemperatureSeries(context.Background(), st, []byte(missing), false)
	if err == nil {
		t.Fatalf("缺必需列应当报错")
	}
	e := errcode.As(err)
	if e.Code != errcode.Validation {
		t.Fatalf("期望错误码 %s，实际 %s", errcode.Validation, e.Code)
	}
	if !strings.Contains(e.Details, "device_name") {
		t.Fatalf("错误细节未点名缺失列：%s", e.Details)
	}
	if count := countEvents(t, st); count != 0 {
		t.Fatalf("缺列的导入不应落库，库内实际有 %d 条", count)
	}
}

// TestImportTemperatureSeries_DuplicateFileImportedOnce 同一份文件导两次不重复入库。
func TestImportTemperatureSeries_DuplicateFileImportedOnce(t *testing.T) {
	st := newImportTestStore(t)

	first, err := ImportTemperatureSeries(context.Background(), st, []byte(cleanImportCSV), false)
	if err != nil {
		t.Fatalf("首次导入失败：%v", err)
	}
	if first.Imported != 4 {
		t.Fatalf("首次导入期望入库 4 条，实际 %d 条", first.Imported)
	}

	second, err := ImportTemperatureSeries(context.Background(), st, []byte(cleanImportCSV), false)
	if err != nil {
		t.Fatalf("二次导入失败：%v", err)
	}
	if second.Imported != 0 {
		t.Fatalf("二次导入不应新增记录，实际新增 %d 条", second.Imported)
	}
	if second.Duplicates != 4 {
		t.Fatalf("二次导入期望 4 条重复，实际 %d 条", second.Duplicates)
	}
	if count := countEvents(t, st); count != 4 {
		t.Fatalf("库内应始终只有 4 条事件，实际 %d 条", count)
	}
}

// TestImportTemperatureSeries_WithinFileDuplicate 同一份文件内重复采样只入一条。
func TestImportTemperatureSeries_WithinFileDuplicate(t *testing.T) {
	st := newImportTestStore(t)

	// 第 2 行与第 1 行是同一次采样（现场导出选了两段重叠的时间范围）。
	withinFile := `point_id,device_id,device_name,measured_at,temperature_c,unit,source,alarm_flag,events
point-vax-freezer-01,device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20T08:00:00+08:00,5.0,C,recorder,false,
point-vax-freezer-01,device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20T08:00:00+08:00,5.0,C,recorder,false,
`

	result, err := ImportTemperatureSeries(context.Background(), st, []byte(withinFile), false)
	if err != nil {
		t.Fatalf("导入失败：%v", err)
	}
	if result.Imported != 1 {
		t.Fatalf("批内重复期望入库 1 条，实际 %d 条", result.Imported)
	}
	if result.Duplicates != 1 {
		t.Fatalf("批内重复期望计 1 条重复，实际 %d 条", result.Duplicates)
	}
	if result.Skipped != 0 {
		t.Fatalf("批内重复不是脏行，不应计入跳过：%v", result.SkipReasons)
	}
	if count := countEvents(t, st); count != 1 {
		t.Fatalf("库内应只有 1 条事件，实际 %d 条", count)
	}
}

// TestImportTemperatureSeries_DryRunSeesExistingRows 干跑能看出库里已有的重复。
//
// 干跑不落库，但「这批里有多少条库里已经有了」必须查得出来：
// 不查就报「将导入 N 条」，实跑时却只导进 M 条，「先干跑看看」就失去了参考价值。
func TestImportTemperatureSeries_DryRunSeesExistingRows(t *testing.T) {
	st := newImportTestStore(t)

	if _, err := ImportTemperatureSeries(context.Background(), st, []byte(cleanImportCSV), false); err != nil {
		t.Fatalf("首次导入失败：%v", err)
	}

	dry, err := ImportTemperatureSeries(context.Background(), st, []byte(cleanImportCSV), true)
	if err != nil {
		t.Fatalf("干跑失败：%v", err)
	}
	if dry.Imported != 0 {
		t.Fatalf("库里已有的行干跑时应报将导入 0 条，实际 %d 条", dry.Imported)
	}
	if dry.Duplicates != 4 {
		t.Fatalf("库里已有的行干跑时应计 4 条重复，实际 %d 条", dry.Duplicates)
	}
}

// TestImportTemperatureSeries_SampleFileRowCountMatches 仓库内样例的行数与导入条数对得上。
//
// 样例是脱敏测试数据层，用它验证「文件行数 = 导入条数 + 跳过条数」这条恒等式。
func TestImportTemperatureSeries_SampleFileRowCountMatches(t *testing.T) {
	st := newImportTestStore(t)

	path := filepath.Join("..", "..", "testdata", "recordings_sample.csv")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取样例失败：%v", err)
	}

	result, err := ImportTemperatureSeries(context.Background(), st, data, false)
	if err != nil {
		t.Fatalf("导入样例失败：%v", err)
	}
	if result.Skipped != 0 {
		t.Fatalf("样例不应有跳过行，实际跳过 %d 条：%v", result.Skipped, result.SkipReasons)
	}
	if result.Imported != result.TotalRows {
		t.Fatalf("导入条数应等于数据行数：导入 %d，行数 %d", result.Imported, result.TotalRows)
	}
	if result.TotalRows != 84 {
		t.Fatalf("样例应为 84 行数据，实际 %d 行", result.TotalRows)
	}
	if count := countEvents(t, st); count != result.Imported {
		t.Fatalf("库内条数与回执不一致：库内 %d，回执 %d", count, result.Imported)
	}
}

// TestImportTemperatureSeries_EmptyBodyRejected 空文件 → 报错。
func TestImportTemperatureSeries_EmptyBodyRejected(t *testing.T) {
	st := newImportTestStore(t)

	_, err := ImportTemperatureSeries(context.Background(), st, []byte("   \n"), false)
	if err == nil {
		t.Fatalf("空文件应当报错")
	}
	e := errcode.As(err)
	if e.Code != errcode.Validation {
		t.Fatalf("期望错误码 %s，实际 %s", errcode.Validation, e.Code)
	}
}

// TestImportTemperatureSeries_MissingValueRowSkipped 必填列有列名但取值为空 → 逐行跳过。
func TestImportTemperatureSeries_MissingValueRowSkipped(t *testing.T) {
	st := newImportTestStore(t)

	blankValue := `point_id,device_id,device_name,measured_at,temperature_c,unit,source,alarm_flag,events
point-vax-freezer-01,device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20T08:00:00+08:00,5.0,C,recorder,false,
,device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20T08:05:00+08:00,5.2,C,recorder,false,
`

	result, err := ImportTemperatureSeries(context.Background(), st, []byte(blankValue), false)
	if err != nil {
		t.Fatalf("导入失败：%v", err)
	}
	if result.Imported != 1 {
		t.Fatalf("期望入库 1 条，实际 %d 条", result.Imported)
	}
	if result.Skipped != 1 {
		t.Fatalf("期望跳过 1 条，实际 %d 条", result.Skipped)
	}
	codes := skipCodes(result.SkipReasons)
	if codes[SkipMissingColumn] != 1 {
		t.Fatalf("期望跳过原因分类 missing_column，实际 %v", codes)
	}
	if !strings.Contains(result.SkipReasons[0].Reason, "point_id") {
		t.Fatalf("跳过原因未点名缺失的列：%s", result.SkipReasons[0].Reason)
	}
}
