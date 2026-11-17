package report

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// 本文件的用例围绕周报这条产出口径：
//   - 一行一个点位，越限次数按点位汇总（在一行一窗口的表里这一列恒等于 1，没意义）；
//   - 处置结论与确认人只认人的结论，没确认的写「待确认」，不拿系统建议顶替；
//   - CSV 表头固定、带 UTF-8 BOM、行数与报表行数对得上。
//
// 取数用假读侧喂进来：报表层只回答「要哪几份数据」，
// 怎么拿是存储层的事，不必为了测一行 CSV 去造一个真库。

// 测试用点位标识（占位值，不含任何真实机构信息）。
const (
	reportPointA = "point-vax-freezer-01"
	reportPointB = "point-vax-fridge-02"
)

// fakeReader 是喂给报表层的假读侧。
type fakeReader struct {
	runs          []domain.RunRecord
	payloads      map[domain.StepPayloadKind]map[string]json.RawMessage
	confirmations map[string]domain.Confirmation
	aggs          []domain.HourlyAggregate
}

// RunsByPointInRange 回读区间内的判定 run。
func (f *fakeReader) RunsByPointInRange(_ context.Context, pointID string, from, to time.Time) ([]domain.RunRecord, error) {
	out := make([]domain.RunRecord, 0, len(f.runs))
	for _, rec := range f.runs {
		if pointID != "" && rec.Run.PointID != pointID {
			continue
		}
		if rec.Run.WindowStart == nil {
			continue
		}
		if rec.Run.WindowStart.Before(from) || rec.Run.WindowStart.After(to) {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// RunPayloadsInRange 回读区间内所有 run 的指定分码产物。
func (f *fakeReader) RunPayloadsInRange(_ context.Context, kind domain.StepPayloadKind, _, _ time.Time) (map[string]json.RawMessage, error) {
	return f.payloads[kind], nil
}

// ConfirmationsInRange 回读区间内所有 run 的确认记录。
func (f *fakeReader) ConfirmationsInRange(_ context.Context, _, _ time.Time) (map[string]domain.Confirmation, error) {
	return f.confirmations, nil
}

// HourlyAggregates 回读区间内的小时级聚合行。
func (f *fakeReader) HourlyAggregates(_ context.Context, _ string, _, _ time.Time) ([]domain.HourlyAggregate, error) {
	return f.aggs, nil
}

// sampleRun 造一条判定 run 的状态快照。
func sampleRun(runID, pointID string, start time.Time) domain.RunRecord {
	run := domain.Run{
		RunID:          runID,
		PointID:        pointID,
		State:          domain.RunStateAwaitingConfirmation,
		ExecState:      domain.ExecStateSucceeded,
		ExternalRef:    domain.ExternalRef(pointID, start),
		IdempotencyKey: domain.WindowIdempotencyKey(pointID, start),
	}
	run.BindWindow(start, start.Add(15*time.Minute))
	return domain.RunRecord{Run: run}
}

// windowPayload 造一条越限窗口产物。
func windowPayload(t *testing.T, runID string, peak float64, durationMinutes int) map[string]json.RawMessage {
	t.Helper()
	raw, err := domain.EncodePayload(domain.PayloadWindow, &domain.WindowPayload{
		Window: domain.ExcursionWindow{
			Start:           time.Date(2026, 10, 20, 3, 10, 0, 0, time.UTC),
			End:             time.Date(2026, 10, 20, 3, 25, 0, 0, time.UTC),
			DurationMinutes: durationMinutes,
			PeakC:           peak,
			PeakAt:          time.Date(2026, 10, 20, 3, 18, 0, 0, time.UTC),
			Zone:            "2~8℃",
			SampleCount:     4,
		},
	})
	if err != nil {
		t.Fatalf("编码窗口产物失败：%v", err)
	}
	return map[string]json.RawMessage{runID: raw}
}

// recordPayload 造一条成稿产物。
func recordPayload(t *testing.T, runID, deviceName string, batches ...string) map[string]json.RawMessage {
	t.Helper()
	list := make([]domain.Batch, 0, len(batches))
	for _, ref := range batches {
		list = append(list, domain.Batch{BatchRef: ref})
	}
	raw, err := domain.EncodePayload(domain.PayloadExceptionRecord, &domain.ExceptionRecordPayload{
		PointID:    reportPointA,
		DeviceName: deviceName,
		Batches:    list,
	})
	if err != nil {
		t.Fatalf("编码成稿产物失败：%v", err)
	}
	return map[string]json.RawMessage{runID: raw}
}

// TestBuildWeeklyReport_AggregatesByPoint 验证周报按点位汇总。
//
// 三条判定：点位 A 两段（一段已确认、一段没确认），点位 B 一段。
// 周报应出两行；点位 A 那一行的越限次数是 2，峰值取两段里更高的那个，
// 持续分钟数是累加，涉及批次取并集，处置结论里同时出现人的结论与「待确认」。
func TestBuildWeeklyReport_AggregatesByPoint(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 10, 20, 3, 10, 0, 0, time.UTC)
	second := time.Date(2026, 10, 20, 9, 10, 0, 0, time.UTC)

	windows := windowPayload(t, "run-a1", 12.4, 15)
	for runID, raw := range windowPayload(t, "run-a2", 9.8, 25) {
		windows[runID] = raw
	}
	for runID, raw := range windowPayload(t, "run-b1", 15.2, 40) {
		windows[runID] = raw
	}
	records := recordPayload(t, "run-a1", "疫苗冷藏箱一号", "<EXAMPLE_LOT-01>")
	for runID, raw := range recordPayload(t, "run-a2", "疫苗冷藏箱一号", "<EXAMPLE_LOT-02>") {
		records[runID] = raw
	}

	reader := &fakeReader{
		runs: []domain.RunRecord{
			sampleRun("run-a1", reportPointA, start),
			sampleRun("run-a2", reportPointA, second),
			sampleRun("run-b1", reportPointB, start),
		},
		payloads: map[domain.StepPayloadKind]map[string]json.RawMessage{
			domain.PayloadWindow:          windows,
			domain.PayloadExceptionRecord: records,
		},
		confirmations: map[string]domain.Confirmation{
			"run-a1": {
				RunID:            "run-a1",
				Action:           string(workflow.ActionConfirm),
				ConfirmedBy:      "药学部主管甲",
				ConfirmedAt:      time.Date(2026, 10, 20, 5, 0, 0, 0, time.UTC),
				Disposition:      "hold",
				DispositionLabel: "隔离待评估",
			},
		},
	}

	from := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 25, 23, 59, 59, 0, time.UTC)
	rows, err := BuildWeeklyReport(ctx, reader, from, to, "")
	if err != nil {
		t.Fatalf("生成周报失败：%v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("周报应出 2 行（两个点位），实际 %d 行", len(rows))
	}

	// 行序：越限次数降序、点位标识升序。点位 A 两段、点位 B 一段，A 在前。
	if rows[0].PointID != reportPointA || rows[1].PointID != reportPointB {
		t.Fatalf("行序不符：%s, %s", rows[0].PointID, rows[1].PointID)
	}

	rowA := rows[0]
	if rowA.ExcursionCount != 2 {
		t.Fatalf("点位 A 越限次数应为 2，实际 %d", rowA.ExcursionCount)
	}
	if rowA.PeakTemperatureC != 12.4 {
		t.Fatalf("点位 A 峰值应为 12.4℃，实际 %.2f℃", rowA.PeakTemperatureC)
	}
	if rowA.DurationMinutes != 40 {
		t.Fatalf("点位 A 累计持续应为 40 分钟，实际 %d", rowA.DurationMinutes)
	}
	if len(rowA.Batches) != 2 {
		t.Fatalf("点位 A 涉及批次应为 2 个，实际 %v", rowA.Batches)
	}
	if rowA.PendingCount != 1 {
		t.Fatalf("点位 A 待确认应为 1 条（run-a2 没人确认），实际 %d", rowA.PendingCount)
	}
	if !containsText(rowA.Dispositions, pendingDispositionLabel) {
		t.Fatalf("点位 A 处置结论应含「%s」，实际 %v", pendingDispositionLabel, rowA.Dispositions)
	}
	if !containsText(rowA.Dispositions, "隔离待评估") {
		t.Fatalf("点位 A 处置结论应含人的结论，实际 %v", rowA.Dispositions)
	}
	if !containsText(rowA.ConfirmedBy, "药学部主管甲") {
		t.Fatalf("点位 A 确认人应含做确认的主管，实际 %v", rowA.ConfirmedBy)
	}
	if rowA.DeviceName != "疫苗冷藏箱一号" {
		t.Fatalf("点位 A 设备名应取成稿里的，实际 %q", rowA.DeviceName)
	}

	rowB := rows[1]
	if rowB.ExcursionCount != 1 {
		t.Fatalf("点位 B 越限次数应为 1，实际 %d", rowB.ExcursionCount)
	}
	if rowB.PeakTemperatureC != 15.2 {
		t.Fatalf("点位 B 峰值应为 15.2℃，实际 %.2f℃", rowB.PeakTemperatureC)
	}
	if rowB.PendingCount != 1 {
		t.Fatalf("点位 B 未确认，应计 1 条待确认，实际 %d", rowB.PendingCount)
	}
}

// TestBuildWeeklyReport_RejectedRunIsNotPending 验证被主管否决的判定不再算「待确认」。
//
// 否决也是主管做过的动作。把它算成「还没人确认」，管理员会拿着周报
// 追着主管要一个已经给过的结论；而它的处置结论一栏写「已否决」，
// 才是这段没放行的事实。
func TestBuildWeeklyReport_RejectedRunIsNotPending(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 10, 20, 3, 10, 0, 0, time.UTC)

	reader := &fakeReader{
		runs: []domain.RunRecord{sampleRun("run-r1", reportPointA, start)},
		payloads: map[domain.StepPayloadKind]map[string]json.RawMessage{
			domain.PayloadWindow: windowPayload(t, "run-r1", 11.0, 30),
		},
		confirmations: map[string]domain.Confirmation{
			"run-r1": {
				RunID:       "run-r1",
				Action:      string(workflow.ActionReject),
				ConfirmedBy: "药学部主管乙",
				ConfirmedAt: time.Date(2026, 10, 20, 5, 0, 0, 0, time.UTC),
				Opinion:     "判定依据不足，暂不放行",
			},
		},
	}

	from := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 25, 23, 59, 59, 0, time.UTC)
	rows, err := BuildWeeklyReport(ctx, reader, from, to, "")
	if err != nil {
		t.Fatalf("生成周报失败：%v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("周报应出 1 行，实际 %d 行", len(rows))
	}
	if rows[0].PendingCount != 0 {
		t.Fatalf("被否决的判定不应计入待确认，实际 %d", rows[0].PendingCount)
	}
	if !containsText(rows[0].Dispositions, rejectedDispositionLabel) {
		t.Fatalf("处置结论应含「%s」，实际 %v", rejectedDispositionLabel, rows[0].Dispositions)
	}
	if !containsText(rows[0].ConfirmedBy, "药学部主管乙") {
		t.Fatalf("确认人应含做否决的主管，实际 %v", rows[0].ConfirmedBy)
	}
}

// TestBuildWeeklyReport_EmptyRangeProducesNoRows 验证区间内没有判定时出空表而不是报错。
//
// 「这周没出事」是常态：周报照出，只是没有数据行。
func TestBuildWeeklyReport_EmptyRangeProducesNoRows(t *testing.T) {
	ctx := context.Background()
	reader := &fakeReader{}
	from := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 25, 23, 59, 59, 0, time.UTC)

	rows, err := BuildWeeklyReport(ctx, reader, from, to, "")
	if err != nil {
		t.Fatalf("无判定时应出空表而不是报错：%v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("无判定时应出 0 行，实际 %d 行", len(rows))
	}
}

// TestBuildWeeklyReport_RejectsInvalidRange 验证非法区间被挡下。
func TestBuildWeeklyReport_RejectsInvalidRange(t *testing.T) {
	ctx := context.Background()
	reader := &fakeReader{}
	from := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)

	if _, err := BuildWeeklyReport(ctx, reader, from, to, ""); err == nil {
		t.Fatal("区间倒置时应报错")
	}
	if _, err := BuildWeeklyReport(ctx, reader, time.Time{}, to, ""); err == nil {
		t.Fatal("缺起点时应报错")
	}
}

// TestWriteWeeklyCSV_FixedHeaderAndRowCount 验证周报 CSV 的形状。
//
// 三件事：表头固定不变（贴表的人每周用同一张模板，列名一改就得改模板）、
// 行数与报表行数一致、带 UTF-8 BOM（Excel 双击打开不乱码）。
func TestWriteWeeklyCSV_FixedHeaderAndRowCount(t *testing.T) {
	rows := []WeeklyRow{
		{
			PointID:          reportPointA,
			DeviceName:       "疫苗冷藏箱一号",
			ExcursionCount:   2,
			PeakTemperatureC: 12.4,
			DurationMinutes:  40,
			Batches:          []string{"<EXAMPLE_LOT-01>", "<EXAMPLE_LOT-02>"},
			Dispositions:     []string{"隔离待评估", pendingDispositionLabel},
			ConfirmedBy:      []string{"药学部主管甲"},
			PendingCount:     1,
		},
		{
			PointID:          reportPointB,
			ExcursionCount:   1,
			PeakTemperatureC: 15.2,
			DurationMinutes:  75,
		},
	}

	var buf bytes.Buffer
	if err := WriteWeeklyCSV(&buf, rows); err != nil {
		t.Fatalf("写周报 CSV 失败：%v", err)
	}
	raw := buf.Bytes()
	if !bytes.HasPrefix(raw, []byte("\xef\xbb\xbf")) {
		t.Fatal("周报 CSV 应带 UTF-8 BOM，Excel 双击打开才不乱码")
	}

	records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(raw), "\ufeff"))).ReadAll()
	if err != nil {
		t.Fatalf("解析周报 CSV 失败：%v", err)
	}
	if len(records) != 3 {
		t.Fatalf("CSV 应有 1 行表头 + 2 行数据，实际 %d 行", len(records))
	}
	header := strings.Join(records[0], ",")
	if header != WeeklyCSVHeader {
		t.Fatalf("表头不符：期望 %q，实际 %q", WeeklyCSVHeader, header)
	}
	for _, want := range []string{"点位标识", "越限次数", "峰值温度", "累计持续", "涉及批次", "处置结论", "确认人"} {
		if !strings.Contains(header, want) {
			t.Fatalf("表头应含「%s」，实际 %q", want, header)
		}
	}

	first := records[1]
	if first[1] != reportPointA {
		t.Fatalf("首行点位不符：%q", first[1])
	}
	if first[3] != "2" {
		t.Fatalf("首行越限次数应为 2，实际 %q", first[3])
	}
	if first[4] != "12.40" {
		t.Fatalf("首行峰值应固定两位小数，实际 %q", first[4])
	}
	if !strings.Contains(first[6], "<EXAMPLE_LOT-01>") || !strings.Contains(first[6], "<EXAMPLE_LOT-02>") {
		t.Fatalf("首行涉及批次应列出两个批次，实际 %q", first[6])
	}
	if !strings.Contains(first[7], pendingDispositionLabel) {
		t.Fatalf("首行处置结论应含「%s」，实际 %q", pendingDispositionLabel, first[7])
	}
	if first[0] != "1" {
		t.Fatalf("首行序号应为 1，实际 %q", first[0])
	}
}

// TestWriteHourlyCSV_FixedHeaderAndLocalHour 验证小时级聚合 CSV。
//
// 小时起点按本地时区展示：库里按 UTC 存，报表给人看，
// 「凌晨三点那一小时」必须是本地三点整。
func TestWriteHourlyCSV_FixedHeaderAndLocalHour(t *testing.T) {
	hour := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	aggs := []domain.HourlyAggregate{
		{
			PointID:          reportPointA,
			HourStart:        hour,
			EventCount:       12,
			ExcursionCount:   1,
			PeakTemperatureC: 12.4,
			AvgTemperatureC:  5.25,
			MinTemperatureC:  3.1,
		},
		{
			PointID:          reportPointB,
			HourStart:        hour.Add(time.Hour),
			EventCount:       12,
			ExcursionCount:   0,
			PeakTemperatureC: 7.5,
			AvgTemperatureC:  5.0,
			MinTemperatureC:  4.2,
			Archived:         true,
		},
	}

	var buf bytes.Buffer
	loc := time.FixedZone("冷链本地时区", 480*60)
	if err := WriteHourlyCSV(&buf, aggs, loc); err != nil {
		t.Fatalf("写聚合 CSV 失败：%v", err)
	}
	raw := buf.Bytes()
	if !bytes.HasPrefix(raw, []byte("\xef\xbb\xbf")) {
		t.Fatal("聚合 CSV 应带 UTF-8 BOM")
	}

	records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(raw), "\ufeff"))).ReadAll()
	if err != nil {
		t.Fatalf("解析聚合 CSV 失败：%v", err)
	}
	if len(records) != 3 {
		t.Fatalf("CSV 应有 1 行表头 + 2 行数据，实际 %d 行", len(records))
	}
	if header := strings.Join(records[0], ","); header != HourlyCSVHeader {
		t.Fatalf("表头不符：期望 %q，实际 %q", HourlyCSVHeader, header)
	}
	if records[1][1] != "2026-10-20 11:00" {
		t.Fatalf("小时起点应按本地时区展示，实际 %q", records[1][1])
	}
	if records[1][2] != "12" || records[1][3] != "1" {
		t.Fatalf("事件数与越温次数不符：%q / %q", records[1][2], records[1][3])
	}
	if records[2][7] != "是" {
		t.Fatalf("已归档的桶应标「是」，实际 %q", records[2][7])
	}
}

// TestWriteHourlyCSV_BucketWithoutSamplesLeavesTemperatureBlank 验证没有原始采样的桶温度列留空。
//
// 0.00℃ 是一个读数，「这一小时没有读数」不是读数。
// 冷库峰值写成 0.00℃，看表的人会以为柜子冷到零度，或者以为设备停摆——
// 两件事都不是事实。这种桶照样要占一行（越温次数是 1，得看得见），
// 只是温度三列空着。
func TestWriteHourlyCSV_BucketWithoutSamplesLeavesTemperatureBlank(t *testing.T) {
	hour := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	aggs := []domain.HourlyAggregate{
		{
			PointID:          reportPointA,
			HourStart:        hour,
			EventCount:       0,
			ExcursionCount:   1,
			PeakTemperatureC: 0,
			AvgTemperatureC:  0,
			MinTemperatureC:  0,
		},
	}

	var buf bytes.Buffer
	loc := time.FixedZone("冷链本地时区", 480*60)
	if err := WriteHourlyCSV(&buf, aggs, loc); err != nil {
		t.Fatalf("写聚合 CSV 失败：%v", err)
	}

	records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(buf.String(), "\ufeff"))).ReadAll()
	if err != nil {
		t.Fatalf("解析聚合 CSV 失败：%v", err)
	}
	if len(records) != 2 {
		t.Fatalf("CSV 应有 1 行表头 + 1 行数据，实际 %d 行", len(records))
	}
	row := records[1]
	if row[2] != "0" || row[3] != "1" {
		t.Fatalf("事件数与越温次数不符：%q / %q", row[2], row[3])
	}
	for idx, name := range map[int]string{4: "峰值", 5: "平均", 6: "最低"} {
		if row[idx] != "" {
			t.Fatalf("没有原始采样时%s列应留空，实际 %q", name, row[idx])
		}
	}
}

// TestSummarizeWeekly_CountsRunsAndPending 验证摘要里的三个数。
func TestSummarizeWeekly_CountsRunsAndPending(t *testing.T) {
	rows := []WeeklyRow{
		{PointID: reportPointA, ExcursionCount: 2, PendingCount: 1},
		{PointID: reportPointB, ExcursionCount: 1, PendingCount: 0},
	}
	from := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 25, 23, 59, 59, 0, time.UTC)

	summary := SummarizeWeekly(rows, from, to)
	if summary.RowCount != 2 {
		t.Fatalf("摘要行数应为 2，实际 %d", summary.RowCount)
	}
	if summary.ExcursionCount != 3 {
		t.Fatalf("摘要越限次数应为 3，实际 %d", summary.ExcursionCount)
	}
	if summary.PendingCount != 1 {
		t.Fatalf("摘要待确认数应为 1，实际 %d", summary.PendingCount)
	}
	if !strings.Contains(summary.String(), "越限 3 次") {
		t.Fatalf("摘要应写明越限次数，实际 %q", summary.String())
	}
}

// containsText 判断列表里是否出现过某个值。
func containsText(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
