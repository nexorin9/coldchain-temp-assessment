// 本包是接口契约的守门测试。
//
// 它把 api/openapi.yaml 当成「对外承诺」，把代码里的路由表与真实响应当作
// 「实际交付」，两边逐条比对。三类不一致都要判失败：
//
//  1. 文档里有、代码里没有 → 调用方按文档去调一个不存在的路径。
//  2. 代码里有、文档里没有 → 加了端点却忘了写文档，等于没加。
//  3. 文档里的端点缺样例，或样例只是占位符 → 没有样例的接口文档等于没有，
//     字段名只能靠猜；猜错要到运行时才暴露。
//
// 第四件事是「形状对不对」：拿文档里的 schema 去校验真实请求与真实响应。
// 这一层最易躲——文档写得再漂亮，字段名与实现差一个字，调用方拿到的就是空值。
//
// 文档位置相对本包：../../api/openapi.yaml。
package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/httpapi"
	"github.com/chaosforge/coldchain-temp-assessment/internal/ingest"
	"github.com/chaosforge/coldchain-temp-assessment/internal/ledger"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
	"github.com/chaosforge/coldchain-temp-assessment/internal/runtime"
	"github.com/chaosforge/coldchain-temp-assessment/internal/store"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// 测试用的四类身份凭据：只存在于测试进程内，不写进任何文件与日志。
const (
	operatorToken   = "contract-operator-token-2b7e"
	supervisorToken = "contract-supervisor-token-9a3f"
	auditorToken    = "contract-auditor-token-4c1d"
)

// docPath 是接口契约文档的路径（相对本包目录）。
const docPath = "../../api/openapi.yaml"

// loadDoc 载入接口契约文档。
func loadDoc(t *testing.T) *openapi3.T {
	t.Helper()
	abs, err := filepath.Abs(docPath)
	if err != nil {
		t.Fatalf("解析文档路径失败：%v", err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("找不到接口契约文档 %s：%v", abs, err)
	}
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile(abs)
	if err != nil {
		t.Fatalf("加载接口契约文档失败：%v", err)
	}
	return doc
}

// newServer 造一台指向内存库的 Server。
//
// 用真的存储层与真的接入器：断言「文档里的响应形状与真实响应一致」时，
// 拿测试替身验证没有意义。
func newServer(t *testing.T) (*httpapi.Server, *store.Store) {
	t.Helper()

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	quiet := logging.New(io.Discard)
	ingestor := ingest.NewIngestor(st, ingest.NewRateLimiter(1000, time.Minute, 1000), quiet)

	// 出站两端指向一台本地替身服务：待补偿清单端点要真的从库里读，
	// 读出来的东西得来自真链路，不能拿一个空壳替身糊弄过去。
	// 清单为空时不会有任何出站调用发生。
	outbound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(outbound.Close)
	deviations, err := ledger.NewDeviationWriter(ledger.WritebackConfig{
		BaseURL: outbound.URL,
		Token:   "contract-ledger-token",
	})
	if err != nil {
		t.Fatalf("装配台账写侧失败：%v", err)
	}
	notifier, err := ledger.NewNotifier(ledger.NotifyConfig{
		BaseURL: outbound.URL,
		Token:   "contract-notify-token",
	})
	if err != nil {
		t.Fatalf("装配通知系统失败：%v", err)
	}
	writebacker := runtime.NewWritebacker(st, workflow.NewWritebackGate(deviations, st), notifier, quiet)
	compensator := runtime.NewCompensator(st, writebacker, quiet)

	srv := httpapi.New(httpapi.Options{
		Version:       "0.1.0",
		Logger:        quiet,
		Ingestor:      ingestor,
		Importer:      ingestor,
		RunReader:     st,
		RunLister:     st,
		AuditReader:   st,
		PointProfiles: st,
		Compensator:   compensator,
		Credentials: []httpapi.Credential{
			{Role: workflow.RoleOperator, Token: operatorToken},
			{Role: workflow.RoleSupervisor, Token: supervisorToken},
			{Role: workflow.RoleAuditor, Token: auditorToken},
		},
	})
	return srv, st
}

// TestDocDeclares31 是最基础的一条：文档必须声明 OpenAPI 3.1。
//
// 3.0 与 3.1 在「可空类型」的写法上不一样（3.0 用 nullable，3.1 用类型数组），
// 文档版本写错，工具链解析出来的语义就跟着错。
func TestDocDeclares31(t *testing.T) {
	doc := loadDoc(t)
	if !strings.HasPrefix(doc.OpenAPI, "3.1") {
		t.Fatalf("文档声明的版本是 %q，应为 OpenAPI 3.1", doc.OpenAPI)
	}
	if doc.Paths == nil || doc.Paths.Len() == 0 {
		t.Fatal("文档里没有任何路径")
	}
}

// TestEveryImplementedEndpointIsDocumented 断言代码里的每个端点都在文档里。
func TestEveryImplementedEndpointIsDocumented(t *testing.T) {
	doc := loadDoc(t)
	srv, _ := newServer(t)

	for _, route := range srv.Routes() {
		key := route.Method + " " + route.Path
		item := doc.Paths.Find(route.Path)
		if item == nil {
			t.Errorf("代码里注册了端点 %s，文档里没有这条路径", key)
			continue
		}
		if !hasOperation(item, route.Method) {
			t.Errorf("代码里注册了端点 %s，文档的 %s 路径下没有 %s 操作", key, route.Path, route.Method)
		}
	}
}

// TestEveryDocumentedEndpointIsImplemented 断言文档里的每个端点都在代码里。
//
// 这条比上一条更重要：文档里写了、代码里没有的端点，调用方按文档去调会拿到 404。
func TestEveryDocumentedEndpointIsImplemented(t *testing.T) {
	doc := loadDoc(t)
	srv, _ := newServer(t)

	registered := map[string]bool{}
	for _, route := range srv.Routes() {
		registered[route.Method+" "+route.Path] = true
	}

	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			key := strings.ToUpper(method) + " " + path
			if !registered[key] {
				t.Errorf("文档里写了端点 %s，代码里没有注册", key)
			}
		}
	}
}

// TestEveryOperationCarriesRealExamples 断言每个端点的每个响应都带真实样例。
//
// 占位符（TODO、待补、（填写）、空对象）不算样例：照着它调一次照样错。
func TestEveryOperationCarriesRealExamples(t *testing.T) {
	doc := loadDoc(t)

	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			key := strings.ToUpper(method) + " " + path
			if op == nil {
				t.Errorf("端点 %s 没有内容", key)
				continue
			}
			if strings.TrimSpace(op.Summary) == "" {
				t.Errorf("端点 %s 缺 summary", key)
			}
			if op.Responses == nil || op.Responses.Len() == 0 {
				t.Errorf("端点 %s 没有响应定义", key)
				continue
			}
			for status, ref := range op.Responses.Map() {
				if ref == nil || ref.Value == nil {
					t.Errorf("端点 %s 的响应 %s 没有内容", key, status)
					continue
				}
				if len(ref.Value.Content) == 0 {
					t.Errorf("端点 %s 的响应 %s 没有响应体定义", key, status)
					continue
				}
				for mediaType, content := range ref.Value.Content {
					if content == nil || len(content.Examples) == 0 {
						t.Errorf("端点 %s 的响应 %s（%s）缺 examples", key, status, mediaType)
						continue
					}
					for name, example := range content.Examples {
						if example == nil || example.Value == nil {
							t.Errorf("端点 %s 的响应 %s（%s）的样例 %s 是空的", key, status, mediaType, name)
							continue
						}
						if why, placeholder := placeholderReason(example.Value.Value); placeholder {
							t.Errorf("端点 %s 的响应 %s（%s）的样例 %s 是占位符：%s",
								key, status, mediaType, name, why)
						}
					}
				}
			}
		}
	}
}

// TestDocumentedExamplesMatchImplementedResponses 拿文档里的样例去对真实响应。
//
// 样例是给调用方照着写的：样例里的字段名与真实响应差一个字，
// 照着写出来的人就会拿到空值，而服务看着一切正常。
func TestDocumentedExamplesMatchImplementedResponses(t *testing.T) {
	doc := loadDoc(t)
	srv, _ := newServer(t)

	cases := []struct {
		method string
		path   string
		token  string
		body   string
		status int
	}{
		{
			method: http.MethodGet,
			path:   "/healthz",
			status: http.StatusOK,
		},
		{
			method: http.MethodGet,
			path:   "/v1/version",
			status: http.StatusOK,
		},
		{
			method: http.MethodGet,
			path:   "/v1/points",
			token:  operatorToken,
			status: http.StatusOK,
		},
		{
			method: http.MethodGet,
			path:   "/v1/runs",
			token:  operatorToken,
			status: http.StatusOK,
		},
		{
			method: http.MethodGet,
			path:   "/v1/writebacks/pending",
			token:  auditorToken,
			status: http.StatusOK,
		},
		{
			method: http.MethodPost,
			path:   "/v1/events/coldchain-temperature",
			token:  operatorToken,
			body: `{"point_id":"point-vax-freezer-01","device_id":"device-recorder-0001",` +
				`"device_name":"疫苗冷藏库温度记录仪","measured_at":"2026-10-20T03:15:00+08:00",` +
				`"temperature_c":11.4,"unit":"C","humidity_pct":62.5,"battery_pct":91,` +
				`"accuracy_c":0.3,"source":"recorder","alarm_flag":true,"events":["door_open"]}`,
			status: http.StatusAccepted,
		},
	}

	for _, tc := range cases {
		name := tc.method + " " + tc.path
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("状态码 %d（期望 %d），响应体：%s", rec.Code, tc.status, rec.Body.String())
			}
			// 处理函数已经把请求体读空了，校验前放回去。
			req.Body = io.NopCloser(strings.NewReader(tc.body))
			assertMatchesDoc(t, doc, req, rec)
		})
	}
}

// assertMatchesDoc 用文档里的 schema 校验一次真实响应。
func assertMatchesDoc(t *testing.T, doc *openapi3.T, req *http.Request, rec *httptest.ResponseRecorder) {
	t.Helper()

	router, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatalf("装配文档路由器失败：%v", err)
	}
	route, params, err := router.FindRoute(req)
	if err != nil {
		t.Fatalf("文档里找不到这条路径：%v", err)
	}

	input := &openapi3filter.RequestValidationInput{
		Request:    req,
		PathParams: params,
		Route:      route,
		Options: &openapi3filter.Options{
			// 文档里没写的状态码也算失败：多出来的状态码往往说明
			// 实现与文档已经分叉了。
			IncludeResponseStatus: true,
		},
	}
	// 请求体先取出来留底：ValidateRequest 会把它读空，
	// 读空之后同一份 body 就没法再喂给别的校验了。
	var body []byte
	if req.Body != nil {
		body, err = io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("读取请求体失败：%v", err)
		}
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(body))
	}

	if err := openapi3filter.ValidateRequest(context.Background(), input); err != nil {
		t.Fatalf("请求不符合文档定义：%v", err)
	}

	req.Body = io.NopCloser(bytes.NewReader(body))
	responseInput := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: input,
		Status:                 rec.Code,
		Header:                 rec.Header(),
		Body:                   io.NopCloser(bytes.NewReader(rec.Body.Bytes())),
	}
	if err := openapi3filter.ValidateResponse(context.Background(), responseInput); err != nil {
		t.Fatalf("响应不符合文档定义：%v\n实际响应体：%s", err, rec.Body.String())
	}
}

// TestDocumentedProfileEndpointMatchesImplementedResponse 覆盖温区配置写入端点。
//
// 它是本轮唯一会改业务状态的端点，形状对不上最容易出事：
// 调用方按文档发的字段名与实现不一致，配置就会写不到点位上。
func TestDocumentedProfileEndpointMatchesImplementedResponse(t *testing.T) {
	doc := loadDoc(t)
	srv, _ := newServer(t)

	body := `{"zone_min_c":2,"zone_max_c":8,"sample_interval_sec":300,` +
		`"min_excursion_minutes":20,"hysteresis_c":0.5,"accuracy_limit_c":0.5}`
	req := httptest.NewRequest(http.MethodPut, "/v1/points/point-vax-freezer-01/profile",
		strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+operatorToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d（期望 200），响应体：%s", rec.Code, rec.Body.String())
	}
	req.Body = io.NopCloser(strings.NewReader(body))
	assertMatchesDoc(t, doc, req, rec)
}

// TestDocumentedRunEndpointsMatchImplementedResponses 覆盖判定 run 的三个查询端点。
//
// 库里先放一条判定：run 详情、时间线与审计链三个端点都拿它当输入，
// 文档里的响应结构必须与真实响应逐字段对得上。
func TestDocumentedRunEndpointsMatchImplementedResponses(t *testing.T) {
	doc := loadDoc(t)
	srv, st := newServer(t)

	ctx := context.Background()
	start := time.Date(2026, 10, 20, 3, 10, 0, 0, time.FixedZone("CST", 8*3600))
	end := time.Date(2026, 10, 20, 3, 25, 0, 0, time.FixedZone("CST", 8*3600))
	rec := domain.RunRecord{
		Run: domain.Run{
			RunID:          "run_3f9c1a7d5b2e4c08",
			PointID:        "point-vax-freezer-01",
			WindowStart:    &start,
			WindowEnd:      &end,
			State:          "awaiting_confirmation",
			ExecState:      domain.ExecStateSucceeded,
			ExternalRef:    "cc-point-vax-freezer-01-20261019T191000Z",
			IdempotencyKey: "54b35c7f136371619ea6a5313714d1ac3d4173e3f835bcc97eb1087e6f7dddae",
		},
		Profile: &domain.PointProfile{
			PointID:             "point-vax-freezer-01",
			ZoneMinC:            2,
			ZoneMaxC:            8,
			SampleIntervalSec:   300,
			MinExcursionMinutes: 20,
			HysteresisC:         0.5,
			AccuracyLimitC:      0.5,
		},
		WritebackState: "pending",
		CreatedAt:      start,
		UpdatedAt:      end,
	}
	if err := st.UpsertRun(ctx, rec); err != nil {
		t.Fatalf("写入判定 run 失败：%v", err)
	}

	for _, path := range []string{
		"/v1/runs/run_3f9c1a7d5b2e4c08",
		"/v1/runs/run_3f9c1a7d5b2e4c08/timeline",
		"/v1/runs/run_3f9c1a7d5b2e4c08/audit",
		"/v1/runs?point_id=point-vax-freezer-01",
	} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer "+operatorToken)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 %d（期望 200），响应体：%s", rec.Code, rec.Body.String())
			}
			assertMatchesDoc(t, doc, req, rec)
		})
	}
}

// hasOperation 判断一个路径项下有没有某个方法的操作。
func hasOperation(item *openapi3.PathItem, method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodGet:
		return item.Get != nil
	case http.MethodPost:
		return item.Post != nil
	case http.MethodPut:
		return item.Put != nil
	case http.MethodPatch:
		return item.Patch != nil
	case http.MethodDelete:
		return item.Delete != nil
	default:
		return false
	}
}

// placeholderReason 判断样例值是不是占位符，并说明理由。
func placeholderReason(value any) (string, bool) {
	forbidden := map[string]bool{
		"（填写）": true, "(填写)": true, "待补": true, "TBD": true, "tbd": true,
		"TODO": true, "todo": true, "xxx": true, "string": true, "…": true,
		"...": true, "example": true, "示例": true,
	}
	switch typed := value.(type) {
	case map[string]any:
		if _, ok := typed["// TODO"]; ok {
			return "带 // TODO 注释键", true
		}
		if len(typed) == 0 {
			return "空对象", true
		}
		real := 0
		for key, item := range typed {
			if strings.HasPrefix(key, "//") {
				continue
			}
			real++
			if why, placeholder := placeholderReason(item); placeholder {
				return "字段 " + key + " " + why, true
			}
		}
		if real == 0 {
			return "只剩注释键，没有真实内容", true
		}
	case []any:
		for _, item := range typed {
			if why, placeholder := placeholderReason(item); placeholder {
				return why, true
			}
		}
	case string:
		if forbidden[strings.TrimSpace(typed)] {
			return "值是占位符字样", true
		}
	}
	return "", false
}

// decodeJSON 解析一段 JSON（测试辅助）。
func decodeJSON(t *testing.T, data []byte, into any) {
	t.Helper()
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatalf("解析 JSON 失败：%v（原文：%s）", err, string(data))
	}
}
