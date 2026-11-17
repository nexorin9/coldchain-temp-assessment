package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/ingest"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
	"github.com/chaosforge/coldchain-temp-assessment/internal/store"
)

// newTestServerWithRegistry 造一台带判定台账清单与温区配置登记簿的 Server。
//
// 用真的存储层：断言「配置写进去真的能被查回来」时，
// 拿一个只记在内存里的替身验证没有意义。
func newTestServerWithRegistry(t *testing.T) (*Server, *store.Store) {
	t.Helper()

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	quiet := logging.New(io.Discard)
	ingestor := ingest.NewIngestor(st, ingest.NewRateLimiter(1000, time.Minute, 1000), quiet)
	// 温区配置与判定 run 落在同一个库里：登记簿与判定记录同源，
	// 「这台冷柜现在按什么温区判」与「它判过什么」才问得出同一个答案。
	srv := New(Options{
		Version:       "0.1.0",
		Logger:        quiet,
		Ingestor:      ingestor,
		Importer:      ingestor,
		RunReader:     st,
		RunLister:     st,
		PointProfiles: st,
		Credentials:   testCredentials(),
	})
	return srv, st
}

// sampleProfileBody 构造一份合法的点位温区配置请求体。
func sampleProfileBody(t *testing.T, pointID string) string {
	t.Helper()
	body := map[string]any{
		"point_id":              pointID,
		"zone_min_c":            2,
		"zone_max_c":            8,
		"sample_interval_sec":   300,
		"min_excursion_minutes": 20,
		"hysteresis_c":          0.5,
		"accuracy_limit_c":      0.5,
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("构造温区配置请求体失败：%v", err)
	}
	return string(data)
}

// putProfile 往指定点位写一份温区配置。
func putProfile(t *testing.T, srv *Server, token, pointID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/v1/points/"+pointID+"/profile", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// getPoints 查温区配置清单。
func getPoints(t *testing.T, srv *Server, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/points", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// TestUpdatePointProfile_PersistsWholeConfig 断言整份配置写进去能原样取回。
func TestUpdatePointProfile_PersistsWholeConfig(t *testing.T) {
	srv, _ := newTestServerWithRegistry(t)

	rec := putProfile(t, srv, testOperatorToken, "point-vax-freezer-01",
		sampleProfileBody(t, "point-vax-freezer-01"))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d（期望 200），响应体：%s", rec.Code, rec.Body.String())
	}

	var response pointProfileResponse
	decodeInto(t, rec, &response)
	if response.PointID != "point-vax-freezer-01" {
		t.Errorf("回执里的 point_id 是 %q", response.PointID)
	}
	if response.Profile.ZoneMinC != 2 || response.Profile.ZoneMaxC != 8 {
		t.Errorf("回执里的温区是 %.2f~%.2f℃，期望 2~8℃", response.Profile.ZoneMinC, response.Profile.ZoneMaxC)
	}
	if response.Profile.MinExcursionMinutes != 20 || response.Profile.SampleIntervalSec != 300 {
		t.Errorf("回执里的判定门槛不对：min_excursion_minutes=%d，sample_interval_sec=%d",
			response.Profile.MinExcursionMinutes, response.Profile.SampleIntervalSec)
	}

	// 再查清单：配置真的落在库里，不是只在回执里好看。
	listRec := getPoints(t, srv, testOperatorToken)
	if listRec.Code != http.StatusOK {
		t.Fatalf("查温区配置清单状态码 %d，响应体：%s", listRec.Code, listRec.Body.String())
	}
	var list pointProfileListResponse
	decodeInto(t, listRec, &list)
	if list.Count != 1 {
		t.Fatalf("清单里有 %d 份配置，期望 1 份", list.Count)
	}
	if list.Profiles[0].PointID != "point-vax-freezer-01" {
		t.Errorf("清单里的 point_id 是 %q", list.Profiles[0].PointID)
	}
}

// TestUpdatePointProfile_ReplacesPreviousConfig 断言重复登记是覆盖而不是并存。
func TestUpdatePointProfile_ReplacesPreviousConfig(t *testing.T) {
	srv, _ := newTestServerWithRegistry(t)

	putProfile(t, srv, testOperatorToken, "point-vax-freezer-01",
		sampleProfileBody(t, "point-vax-freezer-01"))

	updated := `{"zone_min_c":2,"zone_max_c":10,"sample_interval_sec":300,` +
		`"min_excursion_minutes":30,"hysteresis_c":0.5,"accuracy_limit_c":0.5}`
	rec := putProfile(t, srv, testOperatorToken, "point-vax-freezer-01", updated)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d（期望 200），响应体：%s", rec.Code, rec.Body.String())
	}

	listRec := getPoints(t, srv, testOperatorToken)
	var list pointProfileListResponse
	decodeInto(t, listRec, &list)
	if list.Count != 1 {
		t.Fatalf("清单里有 %d 份配置，期望 1 份（重复登记是覆盖）", list.Count)
	}
	if list.Profiles[0].ZoneMaxC != 10 || list.Profiles[0].MinExcursionMinutes != 30 {
		t.Errorf("清单里的配置没被覆盖：zone_max_c=%.2f，min_excursion_minutes=%d",
			list.Profiles[0].ZoneMaxC, list.Profiles[0].MinExcursionMinutes)
	}
}

// TestUpdatePointProfile_InvalidConfigNotPersisted 断言非法配置不落库。
//
// 这是本端点最要紧的一条：配置写坏了却进了登记簿，
// 下一次判定就会拿一份错温区算出「一切正常」的结论。
func TestUpdatePointProfile_InvalidConfigNotPersisted(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "温区上下限颠倒",
			body: `{"zone_min_c":8,"zone_max_c":2,"sample_interval_sec":300,"min_excursion_minutes":20}`,
		},
		{
			name: "温区上下限相等",
			body: `{"zone_min_c":5,"zone_max_c":5,"sample_interval_sec":300,"min_excursion_minutes":20}`,
		},
		{
			name: "最短越限时长不是正数",
			body: `{"zone_min_c":2,"zone_max_c":8,"sample_interval_sec":300,"min_excursion_minutes":0}`,
		},
		{
			name: "最短越限时长为负",
			body: `{"zone_min_c":2,"zone_max_c":8,"sample_interval_sec":300,"min_excursion_minutes":-5}`,
		},
		{
			name: "采样周期不是正数",
			body: `{"zone_min_c":2,"zone_max_c":8,"sample_interval_sec":0,"min_excursion_minutes":20}`,
		},
		{
			name: "温区超出记录仪量程",
			body: `{"zone_min_c":-200,"zone_max_c":8,"sample_interval_sec":300,"min_excursion_minutes":20}`,
		},
		{
			name: "迟滞带盖住整个温区",
			body: `{"zone_min_c":2,"zone_max_c":8,"sample_interval_sec":300,"min_excursion_minutes":20,"hysteresis_c":6}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newTestServerWithRegistry(t)

			// 先登记一份能用的配置，用来验证「非法写入没有动它」。
			putProfile(t, srv, testOperatorToken, "point-vax-freezer-01",
				sampleProfileBody(t, "point-vax-freezer-01"))

			rec := putProfile(t, srv, testOperatorToken, "point-vax-freezer-01", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("状态码 %d（期望 400），响应体：%s", rec.Code, rec.Body.String())
			}
			e := decodeErrorBody(t, rec)
			if e.Code != errcode.Validation {
				t.Errorf("错误码是 %q，期望 %q", e.Code, errcode.Validation)
			}

			// 库里那份配置必须原样还在。
			listRec := getPoints(t, srv, testOperatorToken)
			var list pointProfileListResponse
			decodeInto(t, listRec, &list)
			if list.Count != 1 {
				t.Fatalf("清单里有 %d 份配置，期望 1 份", list.Count)
			}
			if list.Profiles[0].ZoneMinC != 2 || list.Profiles[0].ZoneMaxC != 8 {
				t.Errorf("非法写入把库里那份配置改掉了：%.2f~%.2f℃", list.Profiles[0].ZoneMinC, list.Profiles[0].ZoneMaxC)
			}
		})
	}
}

// TestUpdatePointProfile_PointIDMismatchRejected 断言两处 point_id 不一致时拒绝。
func TestUpdatePointProfile_PointIDMismatchRejected(t *testing.T) {
	srv, _ := newTestServerWithRegistry(t)

	rec := putProfile(t, srv, testOperatorToken, "point-vax-freezer-01",
		sampleProfileBody(t, "point-fridge-cold-02"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 %d（期望 400），响应体：%s", rec.Code, rec.Body.String())
	}
	e := decodeErrorBody(t, rec)
	if e.Code != errcode.Validation {
		t.Errorf("错误码是 %q，期望 %q", e.Code, errcode.Validation)
	}
	if !strings.Contains(e.Details, "不一致") {
		t.Errorf("错误细节里没有说明两处不一致：%s", e.Details)
	}
}

// TestUpdatePointProfile_EmptyBodyRejected 断言空体被拒。
func TestUpdatePointProfile_EmptyBodyRejected(t *testing.T) {
	srv, _ := newTestServerWithRegistry(t)

	rec := putProfile(t, srv, testOperatorToken, "point-vax-freezer-01", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 %d（期望 400），响应体：%s", rec.Code, rec.Body.String())
	}
}

// TestUpdatePointProfile_RoleGate 断言只有经办与主管能改温区配置。
func TestUpdatePointProfile_RoleGate(t *testing.T) {
	srv, _ := newTestServerWithRegistry(t)
	body := sampleProfileBody(t, "point-vax-freezer-01")

	// 调阅岗只读：它不能改判定口径。
	rec := putProfile(t, srv, testAuditorToken, "point-vax-freezer-01", body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("调阅岗改配置的状态码 %d（期望 403），响应体：%s", rec.Code, rec.Body.String())
	}
	e := decodeErrorBody(t, rec)
	if e.Code != errcode.RoleRequired {
		t.Errorf("错误码是 %q，期望 %q", e.Code, errcode.RoleRequired)
	}

	// 主管可以改。
	if rec := putProfile(t, srv, testSupervisorToken, "point-vax-freezer-01", body); rec.Code != http.StatusOK {
		t.Fatalf("主管改配置的状态码 %d（期望 200），响应体：%s", rec.Code, rec.Body.String())
	}
}

// TestPointProfileEndpoints_RequireCredential 断言两个温区端点都要凭据。
func TestPointProfileEndpoints_RequireCredential(t *testing.T) {
	srv, _ := newTestServerWithRegistry(t)

	if rec := getPoints(t, srv, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("不带凭据查配置清单的状态码 %d（期望 401）", rec.Code)
	}
	rec := putProfile(t, srv, "", "point-vax-freezer-01", sampleProfileBody(t, "point-vax-freezer-01"))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("不带凭据改配置的状态码 %d（期望 401）", rec.Code)
	}
}

// TestListRuns_ReturnsRunsInWindowOrder 断言判定台账清单按窗口起点升序。
func TestListRuns_ReturnsRunsInWindowOrder(t *testing.T) {
	srv, st := newTestServerWithRegistry(t)
	ctx := context.Background()

	base := time.Date(2026, 10, 20, 3, 0, 0, 0, time.FixedZone("CST", 8*3600))
	for i, offset := range []int{20, 5, 40} {
		start := base.Add(time.Duration(offset) * time.Minute)
		end := start.Add(15 * time.Minute)
		err := st.UpsertRun(ctx, domain.RunRecord{
			Run: domain.Run{
				RunID:       fmt.Sprintf("run_%d", i),
				PointID:     "point-vax-freezer-01",
				WindowStart: &start,
				WindowEnd:   &end,
				State:       "awaiting_confirmation",
				ExecState:   domain.ExecStateSucceeded,
			},
			CreatedAt: start,
			UpdatedAt: end,
		})
		if err != nil {
			t.Fatalf("写入判定 run 失败：%v", err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/runs?point_id=point-vax-freezer-01", nil)
	req.Header.Set("Authorization", "Bearer "+testOperatorToken)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d（期望 200），响应体：%s", rec.Code, rec.Body.String())
	}

	var list runListResponse
	decodeInto(t, rec, &list)
	if list.Count != 3 {
		t.Fatalf("清单里有 %d 条判定，期望 3 条", list.Count)
	}
	for i := 1; i < len(list.Runs); i++ {
		if list.Runs[i-1].WindowStart > list.Runs[i].WindowStart {
			t.Fatalf("清单没按窗口起点升序：%s 在 %s 之前", list.Runs[i-1].WindowStart, list.Runs[i].WindowStart)
		}
	}
}

// TestListRuns_FiltersByPointAndRange 断言点位与区间过滤都生效。
func TestListRuns_FiltersByPointAndRange(t *testing.T) {
	srv, st := newTestServerWithRegistry(t)
	ctx := context.Background()

	start := time.Date(2026, 10, 20, 3, 10, 0, 0, time.FixedZone("CST", 8*3600))
	end := start.Add(15 * time.Minute)
	if err := st.UpsertRun(ctx, domain.RunRecord{
		Run: domain.Run{
			RunID:       "run_in_range",
			PointID:     "point-vax-freezer-01",
			WindowStart: &start,
			WindowEnd:   &end,
			State:       "awaiting_confirmation",
		},
		CreatedAt: start,
		UpdatedAt: end,
	}); err != nil {
		t.Fatalf("写入判定 run 失败：%v", err)
	}

	// 另一个点位的判定：按点位过滤时不该出现。
	other := start.Add(30 * time.Minute)
	if err := st.UpsertRun(ctx, domain.RunRecord{
		Run: domain.Run{
			RunID:       "run_other_point",
			PointID:     "point-fridge-cold-02",
			WindowStart: &other,
			WindowEnd:   &other,
			State:       "awaiting_confirmation",
		},
		CreatedAt: other,
		UpdatedAt: other,
	}); err != nil {
		t.Fatalf("写入判定 run 失败：%v", err)
	}

	cases := []struct {
		name  string
		query string
		want  int
	}{
		{name: "不限条件", query: "", want: 2},
		{name: "限点位", query: "?point_id=point-vax-freezer-01", want: 1},
		{name: "限区间", query: "?from=2026-10-20T03:00:00%2B08:00&to=2026-10-20T03:20:00%2B08:00", want: 1},
		{name: "区间外", query: "?from=2026-10-21T00:00:00%2B08:00&to=2026-10-22T00:00:00%2B08:00", want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/runs"+tc.query, nil)
			req.Header.Set("Authorization", "Bearer "+testOperatorToken)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 %d（期望 200），响应体：%s", rec.Code, rec.Body.String())
			}
			var list runListResponse
			decodeInto(t, rec, &list)
			if list.Count != tc.want {
				t.Errorf("清单里有 %d 条判定，期望 %d 条", list.Count, tc.want)
			}
		})
	}
}

// TestListRuns_BadRangeRejected 断言区间参数格式错时明确拒绝，不猜。
func TestListRuns_BadRangeRejected(t *testing.T) {
	srv, _ := newTestServerWithRegistry(t)

	cases := []string{
		"?from=2026-10-20", // 只给日期，没有时区
		"?to=not-a-time",   // 根本不是时刻
		"?from=2026-10-20T00:00:00%2B08:00&to=2026-10-19T00:00:00%2B08:00", // 终点早于起点
	}
	for _, query := range cases {
		req := httptest.NewRequest(http.MethodGet, "/v1/runs"+query, nil)
		req.Header.Set("Authorization", "Bearer "+testOperatorToken)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("查询 %s 的状态码 %d（期望 400），响应体：%s", query, rec.Code, rec.Body.String())
		}
	}
}

// TestListRuns_RequiresCredential 断言清单端点要凭据。
func TestListRuns_RequiresCredential(t *testing.T) {
	srv, _ := newTestServerWithRegistry(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 %d（期望 401）", rec.Code)
	}
}

// decodeInto 解析响应体。
func decodeInto(t *testing.T, rec *httptest.ResponseRecorder, into any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
		t.Fatalf("解析响应体失败：%v（原文：%s）", err, rec.Body.String())
	}
}
