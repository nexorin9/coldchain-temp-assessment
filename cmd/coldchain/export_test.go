package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/report"
	"github.com/chaosforge/coldchain-temp-assessment/internal/store"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// 本文件的用例打的是「药学部冷链管理员每周真正敲的那两条命令」：
//   - coldchain export：导出周报 CSV 与小时级聚合 CSV；
//   - coldchain archive：把过期原始事件归档成 gzip JSONL 并从热表清理。
//
// 断言落在产物上：CSV 的表头与行数、归档文件的行数、热表里还剩多少行。
// 中间过程一律用真库真文件——这三条命令的风险全在「产物与库表对不对得上」。

// exportTestPoint 是测试用点位标识（占位值，不含任何真实机构信息）。
const exportTestPoint = "point-vax-freezer-01"

// exportTestRunID 是测试用判定 run 标识。
const exportTestRunID = "run-export-001"

// exportWindowStart 是测试判定对应的越限窗口起点。
var exportWindowStart = time.Date(2026, 10, 20, 3, 10, 0, 0, time.UTC)

// newExportTestDB 造一个带一条判定与几条采样的真库。
//
// 一条判定 = 一条 run + 窗口产物 + 成稿产物 + 一条主管确认记录，
// 外加三条原始采样（其中两条早于归档截止时刻、一条晚于）。
func newExportTestDB(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "coldchain.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("打开测试库失败：%v", err)
	}
	defer func() { _ = st.Close() }()

	// 原始采样：03:00 / 03:10 早于归档截止时刻，09:00 晚于。
	for _, at := range []time.Time{
		time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 20, 3, 10, 0, 0, time.UTC),
		time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC),
	} {
		raw := 12.4
		humidity := 55.0
		ev := domain.TemperatureEvent{
			PointID:        exportTestPoint,
			DeviceID:       "dev-" + exportTestPoint,
			DeviceName:     "疫苗冷藏箱一号",
			MeasuredAt:     at,
			MeasuredAtRaw:  at.Format(time.RFC3339),
			TemperatureC:   12.4,
			Unit:           domain.UnitCelsius,
			TemperatureRaw: &raw,
			UnitRaw:        domain.UnitCelsius,
			HumidityPct:    &humidity,
			Source:         domain.SourceRecorder,
		}
		if _, err := st.InsertEvent(ctx, ev); err != nil {
			t.Fatalf("写入采样失败：%v", err)
		}
	}

	run := domain.Run{
		RunID:          exportTestRunID,
		PointID:        exportTestPoint,
		State:          domain.RunStateAwaitingConfirmation,
		ExecState:      domain.ExecStateSucceeded,
		ExternalRef:    domain.ExternalRef(exportTestPoint, exportWindowStart),
		IdempotencyKey: domain.WindowIdempotencyKey(exportTestPoint, exportWindowStart),
	}
	run.BindWindow(exportWindowStart, exportWindowStart.Add(15*time.Minute))
	if err := st.UpsertRun(ctx, domain.RunRecord{
		Run:     run,
		Profile: &domain.PointProfile{PointID: exportTestPoint, ZoneMinC: 2, ZoneMaxC: 8},
	}); err != nil {
		t.Fatalf("写入 run 失败：%v", err)
	}

	windowRaw, err := domain.EncodePayload(domain.PayloadWindow, &domain.WindowPayload{
		Window: domain.ExcursionWindow{
			Start:           exportWindowStart,
			End:             exportWindowStart.Add(15 * time.Minute),
			DurationMinutes: 15,
			PeakC:           12.4,
			PeakAt:          exportWindowStart.Add(8 * time.Minute),
			Zone:            "2~8℃",
			SampleCount:     4,
		},
	})
	if err != nil {
		t.Fatalf("编码窗口产物失败：%v", err)
	}
	recordRaw, err := domain.EncodePayload(domain.PayloadExceptionRecord, &domain.ExceptionRecordPayload{
		PointID:    exportTestPoint,
		DeviceName: "疫苗冷藏箱一号",
		Batches:    []domain.Batch{{BatchRef: "<EXAMPLE_LOT-01>"}},
	})
	if err != nil {
		t.Fatalf("编码成稿产物失败：%v", err)
	}
	for _, step := range []struct {
		name string
		kind domain.StepPayloadKind
		seq  int
		raw  []byte
	}{
		{"抽越限窗口", domain.PayloadWindow, 2, windowRaw},
		{"成稿记录表", domain.PayloadExceptionRecord, 4, recordRaw},
	} {
		ended := exportWindowStart.Add(time.Minute)
		if err := st.InsertRunStep(ctx, domain.Step{
			RunID:       exportTestRunID,
			StepName:    step.name,
			Seq:         step.seq,
			Attempt:     1,
			State:       domain.StepStateSucceeded,
			PayloadKind: step.kind,
			Payload:     step.raw,
			StartedAt:   exportWindowStart,
			EndedAt:     &ended,
		}); err != nil {
			t.Fatalf("写入步骤产物失败：%v", err)
		}
	}

	if err := st.SaveConfirmation(ctx, domain.Confirmation{
		RunID:            exportTestRunID,
		Action:           string(workflow.ActionConfirm),
		ConfirmedBy:      "药学部主管甲",
		ConfirmedAt:      time.Date(2026, 10, 20, 5, 0, 0, 0, time.UTC),
		Disposition:      "hold",
		DispositionLabel: "隔离待评估",
	}); err != nil {
		t.Fatalf("写入确认记录失败：%v", err)
	}
	return dbPath
}

// readCSV 读一个导出的 CSV（去掉 UTF-8 BOM 后按行解析）。
func readCSV(t *testing.T, path string) [][]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取导出文件失败：%v", err)
	}
	if !strings.HasPrefix(string(raw), "\ufeff") {
		t.Fatalf("导出文件应带 UTF-8 BOM，实际开头为 %q", string(raw[:min(len(raw), 8)]))
	}
	records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(raw), "\ufeff"))).ReadAll()
	if err != nil {
		t.Fatalf("解析导出文件失败：%v", err)
	}
	return records
}

// min 取两数中的较小者。
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestExport_WeeklyCSVHasFixedHeaderAndRows 验证周报导出的产物形状。
//
// 表头固定、行数对得上、峰值固定两位小数、处置结论与确认人取的是人的结论——
// 这几项是周报能直接贴进台账的前提。
func TestExport_WeeklyCSVHasFixedHeaderAndRows(t *testing.T) {
	dbPath := newExportTestDB(t)
	outPath := filepath.Join(t.TempDir(), "weekly.csv")

	code, stderr := captureStderr(t, func() int {
		return run([]string{
			"export",
			"--db", dbPath,
			"--from", "2026-10-19T00:00:00+08:00",
			"--to", "2026-10-25T23:59:59+08:00",
			"--out", outPath,
		})
	})
	if code != exitOK {
		t.Fatalf("导出周报应成功退出，实际退出码 %d，stderr：%s", code, stderr)
	}

	records := readCSV(t, outPath)
	if len(records) != 2 {
		t.Fatalf("CSV 应有 1 行表头 + 1 行数据，实际 %d 行", len(records))
	}
	if header := strings.Join(records[0], ","); header != report.WeeklyCSVHeader {
		t.Fatalf("表头不符：期望 %q，实际 %q", report.WeeklyCSVHeader, header)
	}
	row := records[1]
	if row[1] != exportTestPoint {
		t.Fatalf("点位列不符：%q", row[1])
	}
	if row[3] != "1" {
		t.Fatalf("越限次数列应为 1，实际 %q", row[3])
	}
	if row[4] != "12.40" {
		t.Fatalf("峰值列应固定两位小数，实际 %q", row[4])
	}
	if row[5] != "15" {
		t.Fatalf("累计持续列应为 15 分钟，实际 %q", row[5])
	}
	if !strings.Contains(row[6], "<EXAMPLE_LOT-01>") {
		t.Fatalf("涉及批次列应含批次号，实际 %q", row[6])
	}
	if !strings.Contains(row[7], "隔离待评估") {
		t.Fatalf("处置结论列应含主管给的结论，实际 %q", row[7])
	}
	if !strings.Contains(row[8], "药学部主管甲") {
		t.Fatalf("确认人列应含做确认的主管，实际 %q", row[8])
	}
	if !strings.Contains(stderr, "周报已导出") {
		t.Fatalf("stderr 应说明周报已导出，实际：%s", stderr)
	}
}

// TestExport_HourlyCSVRowsPerHourBucket 验证小时级聚合导出。
//
// 一行一个点位一个小时桶，小时起点按院内本地时区展示；
// 库里按 UTC 存，「凌晨三点那一小时」在报表上必须是本地三点整。
func TestExport_HourlyCSVRowsPerHourBucket(t *testing.T) {
	dbPath := newExportTestDB(t)
	outPath := filepath.Join(t.TempDir(), "hourly.csv")

	code, stderr := captureStderr(t, func() int {
		return run([]string{
			"export",
			"--kind", "hourly",
			"--db", dbPath,
			"--from", "2026-10-19T00:00:00+08:00",
			"--to", "2026-10-25T23:59:59+08:00",
			"--out", outPath,
		})
	})
	if code != exitOK {
		t.Fatalf("导出小时级聚合应成功退出，实际退出码 %d，stderr：%s", code, stderr)
	}

	records := readCSV(t, outPath)
	if len(records) != 3 {
		t.Fatalf("CSV 应有 1 行表头 + 2 行数据（03:00 与 09:00 两个桶），实际 %d 行", len(records))
	}
	if header := strings.Join(records[0], ","); header != report.HourlyCSVHeader {
		t.Fatalf("表头不符：期望 %q，实际 %q", report.HourlyCSVHeader, header)
	}
	if records[1][1] != "2026-10-20 11:00" {
		t.Fatalf("小时起点应按本地时区展示，实际 %q", records[1][1])
	}
	if records[1][2] != "2" {
		t.Fatalf("事件数列应为 2（03:00 桶里两条采样），实际 %q", records[1][2])
	}
	if records[2][1] != "2026-10-20 17:00" {
		t.Fatalf("第二个桶的小时起点不符：%q", records[2][1])
	}
	if !strings.Contains(stderr, "小时级聚合已导出") {
		t.Fatalf("stderr 应说明聚合已导出，实际：%s", stderr)
	}
}

// TestExport_RequiresRange 验证缺区间参数时报用法错误。
func TestExport_RequiresRange(t *testing.T) {
	dbPath := newExportTestDB(t)

	code, stderr := captureStderr(t, func() int {
		return run([]string{"export", "--db", dbPath, "--from", "2026-10-19T00:00:00+08:00"})
	})
	if code != exitUsage {
		t.Fatalf("缺 --to 时应以用法错误退出，实际退出码 %d", code)
	}
	if !strings.Contains(stderr, "--to") {
		t.Fatalf("应指名缺哪个参数，实际：%s", stderr)
	}
}

// TestExport_RejectsUnknownKind 验证未知的导出类型被挡下。
func TestExport_RejectsUnknownKind(t *testing.T) {
	dbPath := newExportTestDB(t)

	code, stderr := captureStderr(t, func() int {
		return run([]string{
			"export", "--kind", "monthly",
			"--db", dbPath,
			"--from", "2026-10-19T00:00:00+08:00",
			"--to", "2026-10-25T23:59:59+08:00",
		})
	})
	if code != exitUsage {
		t.Fatalf("未知 --kind 应以用法错误退出，实际退出码 %d", code)
	}
	if !strings.Contains(stderr, "weekly") {
		t.Fatalf("应列出合法取值，实际：%s", stderr)
	}
}

// TestArchive_MovesExpiredEventsToArchiveFile 验证归档命令的产物。
//
// 三件事一起断言：归档文件确实落盘、行数与移出热表的条数相等、
// 热表里剩下的条数正好是没到期的那些。
func TestArchive_MovesExpiredEventsToArchiveFile(t *testing.T) {
	dbPath := newExportTestDB(t)
	archiveDir := filepath.Join(t.TempDir(), "archive")

	code, stderr := captureStderr(t, func() int {
		return run([]string{
			"archive",
			"--db", dbPath,
			"--before", "2026-10-20T12:00:00+08:00",
			"--dir", archiveDir,
		})
	})
	if code != exitOK {
		t.Fatalf("归档应成功退出，实际退出码 %d，stderr：%s", code, stderr)
	}
	if !strings.Contains(stderr, "归档完成") {
		t.Fatalf("stderr 应说明归档完成，实际：%s", stderr)
	}

	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		t.Fatalf("读取归档目录失败：%v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("归档目录应有 1 个文件，实际 %d", len(entries))
	}
	path := filepath.Join(archiveDir, entries[0].Name())
	if !strings.Contains(entries[0].Name(), exportTestPoint) {
		t.Fatalf("归档文件名应带点位标识，实际 %q", entries[0].Name())
	}

	// 归档文件行数：两条早于截止时刻的采样。
	if lines := countGzipLines(t, path); lines != 2 {
		t.Fatalf("归档文件应有 2 行采样，实际 %d", lines)
	}

	// 热表：只剩没到期的那一条。
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("重新打开库失败：%v", err)
	}
	defer func() { _ = st.Close() }()
	remaining, err := st.CountEvents(context.Background(), exportTestPoint, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("统计热表采样失败：%v", err)
	}
	if remaining != 1 {
		t.Fatalf("热表应剩 1 条未到期采样，实际 %d", remaining)
	}
}

// countGzipLines 数 gzip 文件里的非空行数。
func countGzipLines(t *testing.T, path string) int {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开归档文件失败：%v", err)
	}
	defer func() { _ = file.Close() }()
	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatalf("解压归档文件失败：%v", err)
	}
	defer func() { _ = reader.Close() }()

	scanner := bufio.NewScanner(reader)
	lines := 0
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			lines++
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("读取归档文件失败：%v", err)
	}
	return lines
}

// TestArchive_DryRunLeavesHotTableUntouched 验证干跑不落盘也不清热表。
//
// 运维习惯先干跑看一眼要搬走多少条；干跑动了数据就没人敢先试了。
func TestArchive_DryRunLeavesHotTableUntouched(t *testing.T) {
	dbPath := newExportTestDB(t)
	archiveDir := filepath.Join(t.TempDir(), "archive")

	code, stderr := captureStderr(t, func() int {
		return run([]string{
			"archive",
			"--db", dbPath,
			"--before", "2026-10-20T12:00:00+08:00",
			"--dir", archiveDir,
			"--dry-run",
		})
	})
	if code != exitOK {
		t.Fatalf("干跑应成功退出，实际退出码 %d，stderr：%s", code, stderr)
	}
	if !strings.Contains(stderr, "干跑") {
		t.Fatalf("stderr 应说明这是干跑，实际：%s", stderr)
	}
	if !strings.Contains(stderr, "2 条") {
		t.Fatalf("stderr 应说明将被归档的条数，实际：%s", stderr)
	}

	if _, err := os.Stat(archiveDir); !os.IsNotExist(err) {
		t.Fatalf("干跑不应创建归档目录，实际：%v", err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("重新打开库失败：%v", err)
	}
	defer func() { _ = st.Close() }()
	remaining, err := st.CountEvents(context.Background(), exportTestPoint, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("统计热表采样失败：%v", err)
	}
	if remaining != 3 {
		t.Fatalf("干跑后热表应仍是 3 条，实际 %d", remaining)
	}
}

// TestArchive_RequiresBefore 验证缺截止时刻时报用法错误。
func TestArchive_RequiresBefore(t *testing.T) {
	dbPath := newExportTestDB(t)

	code, stderr := captureStderr(t, func() int {
		return run([]string{"archive", "--db", dbPath, "--dir", filepath.Join(t.TempDir(), "archive")})
	})
	if code != exitUsage {
		t.Fatalf("缺 --before 时应以用法错误退出，实际退出码 %d", code)
	}
	if !strings.Contains(stderr, "--before") {
		t.Fatalf("应指名缺哪个参数，实际：%s", stderr)
	}
}

// TestArchive_NothingExpiredIsNotAFailure 验证没有过期数据时不算失败。
//
// 「这段时间没有过期数据」是常态：批处理调度里它应当是成功退出。
func TestArchive_NothingExpiredIsNotAFailure(t *testing.T) {
	dbPath := newExportTestDB(t)
	archiveDir := filepath.Join(t.TempDir(), "archive")

	code, stderr := captureStderr(t, func() int {
		return run([]string{
			"archive",
			"--db", dbPath,
			"--before", "2026-10-01T00:00:00+08:00",
			"--dir", archiveDir,
		})
	})
	if code != exitOK {
		t.Fatalf("没有过期数据时应成功退出，实际退出码 %d，stderr：%s", code, stderr)
	}
	if !strings.Contains(stderr, "无需归档") {
		t.Fatalf("stderr 应说明无需归档，实际：%s", stderr)
	}
}
