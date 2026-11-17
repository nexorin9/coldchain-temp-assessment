package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/ingest"
)

// 本文件覆盖记录仪导出 CSV 的批量导入端点。
//
// 重点是三件事：凭据不对进不来；回执里的条数与文件行数对得上；
// 干跑只差落库这一件事。

// cleanImportCSV 是一份干净的三行导出。
const cleanImportCSV = `point_id,device_id,device_name,measured_at,temperature_c,unit,humidity_pct,battery_pct,accuracy_c,source,alarm_flag,events
point-vax-freezer-01,device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20T08:00:00+08:00,5.0,C,58,92,0.3,recorder,false,
point-vax-freezer-01,device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20T08:05:00+08:00,5.2,C,58,92,0.3,recorder,false,
point-vax-freezer-01,device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20T08:10:00+08:00,9.6,C,61,91,0.3,recorder,false,door_open
`

// dirtyImportCSV 含两类脏行：时间不可解析与事件类型未知。
const dirtyImportCSV = `point_id,device_id,device_name,measured_at,temperature_c,unit,source,alarm_flag,events
point-vax-freezer-01,device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20T08:00:00+08:00,5.0,C,recorder,false,
point-vax-freezer-01,device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20 08:05:00,5.2,C,recorder,false,
point-vax-freezer-01,device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20T08:10:00+08:00,9.6,C,recorder,false,compressor_alarm
`

// postImport 向批量导入端点投递一次。
//
// token 为空表示不带 Authorization 头；query 附加在路径后。
func postImport(t *testing.T, srv *Server, token, query string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	target := "/v1/imports/temperature-series"
	if query != "" {
		target += "?" + query
	}
	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	req.Header.Set("Content-Type", "text/csv")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// decodeImportBody 解析导入回执。
func decodeImportBody(t *testing.T, rec *httptest.ResponseRecorder) importResponse {
	t.Helper()
	var resp importResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("导入回执不是合法 JSON：%v（原文：%s）", err, rec.Body.String())
	}
	return resp
}

// TestImportSeries_MissingBearerTokenReturns401 未带凭据 → 401。
func TestImportSeries_MissingBearerTokenReturns401(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := postImport(t, srv, "", "", []byte(cleanImportCSV))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401，实际 %d（响应体：%s）", rec.Code, rec.Body.String())
	}
	e := decodeErrorBody(t, rec)
	if e.Code != errcode.Unauthorized {
		t.Fatalf("期望错误码 %s，实际 %s", errcode.Unauthorized, e.Code)
	}
	assertNoCredentialLeak(t, rec)
}

// TestImportSeries_WrongBearerTokenReturns401 凭据不对 → 401。
func TestImportSeries_WrongBearerTokenReturns401(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := postImport(t, srv, "wrong-token", "", []byte(cleanImportCSV))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401，实际 %d（响应体：%s）", rec.Code, rec.Body.String())
	}
	assertNoCredentialLeak(t, rec)
}

// TestImportSeries_CleanCSVReturnsCounts 干净文件 → 条数与文件行数对得上。
func TestImportSeries_CleanCSVReturnsCounts(t *testing.T) {
	srv, st := newTestServer(t)

	rec := postImport(t, srv, testIngestToken, "", []byte(cleanImportCSV))

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d（响应体：%s）", rec.Code, rec.Body.String())
	}
	resp := decodeImportBody(t, rec)
	if resp.Imported != 3 {
		t.Fatalf("期望导入 3 条，实际 %d 条", resp.Imported)
	}
	if resp.Skipped != 0 {
		t.Fatalf("干净文件不应有跳过行，实际 %d 条：%v", resp.Skipped, resp.SkipReasons)
	}
	if resp.TotalRows != 3 {
		t.Fatalf("期望数据行 3 行，实际 %d 行", resp.TotalRows)
	}
	if resp.DryRun {
		t.Fatalf("默认实跑不应带 dry_run=true")
	}
	if resp.Note == "" {
		t.Fatalf("回执缺少说明文字")
	}

	count, err := st.CountEvents(context.Background(), "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("统计事件条数失败：%v", err)
	}
	if count != 3 {
		t.Fatalf("库内应有 3 条事件，实际 %d 条", count)
	}
}

// TestImportSeries_DirtyRowsReportedInSkipReasons 脏行逐条回到调用方。
func TestImportSeries_DirtyRowsReportedInSkipReasons(t *testing.T) {
	srv, st := newTestServer(t)

	rec := postImport(t, srv, testIngestToken, "", []byte(dirtyImportCSV))

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d（响应体：%s）", rec.Code, rec.Body.String())
	}
	resp := decodeImportBody(t, rec)
	if resp.Imported != 1 {
		t.Fatalf("期望导入 1 条，实际 %d 条", resp.Imported)
	}
	if resp.Skipped != 2 {
		t.Fatalf("期望跳过 2 条，实际 %d 条", resp.Skipped)
	}
	if len(resp.SkipReasons) != 2 {
		t.Fatalf("跳过原因应逐条给出，实际 %d 条", len(resp.SkipReasons))
	}

	codes := map[string]bool{}
	for _, reason := range resp.SkipReasons {
		codes[reason.Code] = true
		if reason.Row < 2 {
			t.Fatalf("跳过原因未带行号：%+v", reason)
		}
	}
	if !codes[ingest.SkipUnparsableTime] {
		t.Fatalf("跳过原因缺少时间不可解析分类：%v", codes)
	}
	if !codes[ingest.SkipUnknownEventKind] {
		t.Fatalf("跳过原因缺少事件类型未知分类：%v", codes)
	}

	count, err := st.CountEvents(context.Background(), "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("统计事件条数失败：%v", err)
	}
	if count != 1 {
		t.Fatalf("库内应只有 1 条干净事件，实际 %d 条", count)
	}
}

// TestImportSeries_DryRunDoesNotWrite 干跑不落库。
func TestImportSeries_DryRunDoesNotWrite(t *testing.T) {
	srv, st := newTestServer(t)

	rec := postImport(t, srv, testIngestToken, "dry_run=true", []byte(cleanImportCSV))

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d（响应体：%s）", rec.Code, rec.Body.String())
	}
	resp := decodeImportBody(t, rec)
	if !resp.DryRun {
		t.Fatalf("干跑回执应带 dry_run=true：%s", rec.Body.String())
	}
	if resp.Imported != 3 {
		t.Fatalf("干跑应报出将导入 3 条，实际 %d 条", resp.Imported)
	}

	count, err := st.CountEvents(context.Background(), "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("统计事件条数失败：%v", err)
	}
	if count != 0 {
		t.Fatalf("干跑不得落库，库内实际有 %d 条事件", count)
	}
}

// TestImportSeries_DryRunMatchesRealRun 干跑与实跑统计一致。
func TestImportSeries_DryRunMatchesRealRun(t *testing.T) {
	srv, _ := newTestServer(t)

	dry := decodeImportBody(t, postImport(t, srv, testIngestToken, "dry_run=true", []byte(dirtyImportCSV)))
	real := decodeImportBody(t, postImport(t, srv, testIngestToken, "", []byte(dirtyImportCSV)))

	if dry.Imported != real.Imported || dry.Skipped != real.Skipped ||
		dry.TotalRows != real.TotalRows || dry.Duplicates != real.Duplicates {
		t.Fatalf("干跑与实跑统计不一致：干跑 %+v，实跑 %+v", dry, real)
	}
	if len(dry.SkipReasons) != len(real.SkipReasons) {
		t.Fatalf("干跑与实跑的跳过条数不一致：干跑 %d，实跑 %d",
			len(dry.SkipReasons), len(real.SkipReasons))
	}
	for i := range dry.SkipReasons {
		if dry.SkipReasons[i].Code != real.SkipReasons[i].Code ||
			dry.SkipReasons[i].Row != real.SkipReasons[i].Row {
			t.Fatalf("第 %d 条跳过原因不一致：干跑 %+v，实跑 %+v",
				i, dry.SkipReasons[i], real.SkipReasons[i])
		}
	}
}

// TestImportSeries_InvalidDryRunParamRejected dry_run 取值非法 → 400。
//
// 把非法取值当成 false 悄悄跑一次实导入，比导入本身更糟。
func TestImportSeries_InvalidDryRunParamRejected(t *testing.T) {
	srv, st := newTestServer(t)

	rec := postImport(t, srv, testIngestToken, "dry_run=maybe", []byte(cleanImportCSV))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d（响应体：%s）", rec.Code, rec.Body.String())
	}
	e := decodeErrorBody(t, rec)
	if e.Code != errcode.Validation {
		t.Fatalf("期望错误码 %s，实际 %s", errcode.Validation, e.Code)
	}
	count, err := st.CountEvents(context.Background(), "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("统计事件条数失败：%v", err)
	}
	if count != 0 {
		t.Fatalf("参数非法时不得落库，库内实际有 %d 条", count)
	}
}

// TestImportSeries_EmptyBodyRejected 空请求体 → 400。
func TestImportSeries_EmptyBodyRejected(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := postImport(t, srv, testIngestToken, "", nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d（响应体：%s）", rec.Code, rec.Body.String())
	}
	e := decodeErrorBody(t, rec)
	if !strings.Contains(e.Details, "请求体为空") {
		t.Fatalf("错误细节未点明请求体为空：%s", e.Details)
	}
}

// TestImportSeries_MissingRequiredColumnReturns400 缺必需列 → 400。
func TestImportSeries_MissingRequiredColumnReturns400(t *testing.T) {
	srv, _ := newTestServer(t)

	missing := "point_id,device_id,measured_at,temperature_c,source\n" +
		"point-vax-freezer-01,device-recorder-0001,2026-10-20T08:00:00+08:00,5.0,recorder\n"

	rec := postImport(t, srv, testIngestToken, "", []byte(missing))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d（响应体：%s）", rec.Code, rec.Body.String())
	}
	e := decodeErrorBody(t, rec)
	if !strings.Contains(e.Details, "device_name") {
		t.Fatalf("错误细节未点名缺失列：%s", e.Details)
	}
}

// TestImportSeries_SampleFileCountsMatchRows 导入仓库内样例，条数与文件行数对得上。
func TestImportSeries_SampleFileCountsMatchRows(t *testing.T) {
	srv, st := newTestServer(t)

	path := filepath.Join("..", "..", "testdata", "recordings_sample.csv")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取样例失败：%v", err)
	}
	// 样例有 85 行：1 行表头 + 84 行数据。
	lines := strings.Count(strings.TrimRight(string(data), "\n"), "\n") + 1

	rec := postImport(t, srv, testIngestToken, "", data)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d（响应体：%s）", rec.Code, rec.Body.String())
	}
	resp := decodeImportBody(t, rec)
	if resp.TotalRows != lines-1 {
		t.Fatalf("回执数据行数 %d 与文件行数 %d 对不上", resp.TotalRows, lines-1)
	}
	if resp.Imported+resp.Skipped != resp.TotalRows {
		t.Fatalf("导入 %d 条 + 跳过 %d 条应等于数据行数 %d",
			resp.Imported, resp.Skipped, resp.TotalRows)
	}
	if resp.Skipped != 0 {
		t.Fatalf("样例不应有跳过行，实际 %d 条：%v", resp.Skipped, resp.SkipReasons)
	}

	count, err := st.CountEvents(context.Background(), "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("统计事件条数失败：%v", err)
	}
	if count != resp.Imported {
		t.Fatalf("库内条数 %d 与回执 %d 不一致", count, resp.Imported)
	}
}

// TestImportSeries_WithoutImporterFailsClosed 未装配导入器 → 500，不假装导入过。
func TestImportSeries_WithoutImporterFailsClosed(t *testing.T) {
	srv := New(Options{Version: "0.1.0", IngestToken: testIngestToken})

	rec := postImport(t, srv, testIngestToken, "", []byte(cleanImportCSV))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("期望 500，实际 %d（响应体：%s）", rec.Code, rec.Body.String())
	}
	e := decodeErrorBody(t, rec)
	if e.Code != errcode.StorageError {
		t.Fatalf("期望错误码 %s，实际 %s", errcode.StorageError, e.Code)
	}
}

// TestImportSeries_SecondImportDoesNotDuplicate 同一份文件导两次不产生第二份数据。
func TestImportSeries_SecondImportDoesNotDuplicate(t *testing.T) {
	srv, st := newTestServer(t)

	first := decodeImportBody(t, postImport(t, srv, testIngestToken, "", []byte(cleanImportCSV)))
	if first.Imported != 3 {
		t.Fatalf("首次导入期望 3 条，实际 %d 条", first.Imported)
	}

	second := decodeImportBody(t, postImport(t, srv, testIngestToken, "", []byte(cleanImportCSV)))
	if second.Imported != 0 {
		t.Fatalf("二次导入不应新增记录，实际新增 %d 条", second.Imported)
	}
	if second.Duplicates != 3 {
		t.Fatalf("二次导入期望 3 条重复，实际 %d 条", second.Duplicates)
	}

	count, err := st.CountEvents(context.Background(), "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("统计事件条数失败：%v", err)
	}
	if count != 3 {
		t.Fatalf("库内应始终只有 3 条事件，实际 %d 条", count)
	}
}
