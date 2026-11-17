package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// 本文件是台账读侧的契约测试。
//
// 它盯住四件事：
//
//  1. 请求形状对不对：方法、路径、查询参数、鉴权头。
//     路径或参数错了，台账回的是 404 或另一份数据，而系统看起来「查到了」。
//  2. 失败语义分不分得开：超时、5xx、凭据被拒、响应不合法，
//     四种失败在处置上完全不同（重试 / 改凭据 / 查台账侧 / 转人工）。
//  3. 批次权威性守没守住：批次号只认台账给的；没批次号的占位条目要剔除；
//     归属别的点位的批次要落歧义。
//  4. 缓存是不是「有效数据缓存」：读失败不能被缓存成一份可用的清单，
//     过期库存不能当现状发出去。

// newTestClient 造一个对着测试台账的客户端。
func newTestClient(t *testing.T, handler http.HandlerFunc, opts ...Option) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	base := []Option{WithHTTPClient(server.Client())}
	client, err := NewClient(Config{
		BaseURL:  server.URL,
		Token:    "test-ledger-token",
		Timeout:  2 * time.Second,
		CacheTTL: 30 * time.Second,
	}, append(base, opts...)...)
	if err != nil {
		t.Fatalf("装配台账读侧失败：%v", err)
	}
	return client, server
}

// TestSeries_RequestShapeAndDecoding 断言时序读的请求形状与解码结果。
func TestSeries_RequestShapeAndDecoding(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotQuery string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"point_id":"point-vax-freezer-01","samples":[
			{"measured_at":"2026-10-20T03:10:00+08:00","temperature_c":11.4,"humidity_pct":58.5,"accuracy_c":0.3},
			{"measured_at":"2026-10-20T03:15:00+08:00","temperature_c":12.4,"humidity_pct":60.0}
		]}`))
	})

	from := time.Date(2026, 10, 20, 3, 10, 0, 0, time.FixedZone("CST", 8*3600))
	to := from.Add(15 * time.Minute)
	samples, err := client.Series(context.Background(), "point-vax-freezer-01", from, to)
	if err != nil {
		t.Fatalf("读台账时序失败：%v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("请求方法 = %q，期望 GET", gotMethod)
	}
	if gotPath != "/api/v1/points/point-vax-freezer-01/series" {
		t.Errorf("请求路径 = %q，期望 /api/v1/points/point-vax-freezer-01/series", gotPath)
	}
	if gotAuth != "Bearer test-ledger-token" {
		t.Errorf("鉴权头 = %q，期望 Bearer test-ledger-token", gotAuth)
	}
	if !strings.Contains(gotQuery, "from=") || !strings.Contains(gotQuery, "to=") {
		t.Errorf("查询参数 = %q，期望含 from 与 to", gotQuery)
	}
	if len(samples) != 2 {
		t.Fatalf("读到 %d 条读数，期望 2 条", len(samples))
	}
	if samples[1].TemperatureC != 12.4 {
		t.Errorf("第 2 条温度 = %v，期望 12.4", samples[1].TemperatureC)
	}
	if samples[0].HumidityPct == nil || *samples[0].HumidityPct != 58.5 {
		t.Errorf("第 1 条湿度 = %v，期望 58.5", samples[0].HumidityPct)
	}
	// 缺测不得用 0 冒充：没报精度的读数，精度就是缺测。
	if samples[1].AccuracyC != nil {
		t.Errorf("第 2 条精度 = %v，期望 nil（缺测不冒充 0）", *samples[1].AccuracyC)
	}
}

// TestSeries_AcceptsBareArray 断言台账直接回数组时也认。
func TestSeries_AcceptsBareArray(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"measured_at":"2026-10-20T03:10:00+08:00","temperature_c":9.1}]`))
	})
	samples, err := client.Series(context.Background(), "point-vax-freezer-01",
		time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatalf("读台账时序失败：%v", err)
	}
	if len(samples) != 1 || samples[0].TemperatureC != 9.1 {
		t.Fatalf("读到 %d 条读数（%v），期望 1 条 9.1℃", len(samples), samples)
	}
}

// TestSeries_MalformedSampleRejected 断言不合规的读数被显式拒绝。
func TestSeries_MalformedSampleRejected(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"samples":[{"measured_at":"2026-10-20 03:10:00","temperature_c":9.1}]}`))
	})
	_, err := client.Series(context.Background(), "point-vax-freezer-01", time.Now().Add(-time.Hour), time.Now())
	if err == nil {
		t.Fatal("measured_at 不是 RFC3339 时应报错")
	}
	if code := errcode.As(err).Code; code != errcode.LedgerProtocol {
		t.Errorf("错误码 = %q，期望 %q", code, errcode.LedgerProtocol)
	}
}

// TestBatches_RequestShapeAndDecoding 断言在库批次读的请求形状与解码结果。
func TestBatches_RequestShapeAndDecoding(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotPointID, gotAt string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotPointID = r.URL.Query().Get("point_id")
		gotAt = r.URL.Query().Get("at")
		_, _ = w.Write([]byte(`{"batches":[
			{"batch_ref":"<EXAMPLE_LOT-01>","product_name":"疫苗（脱敏样例）","quantity":120,"expiry_date":"<EXAMPLE_EXPIRY-A>"},
			{"batch_ref":"<EXAMPLE_LOT-02>","product_name":"疫苗（脱敏样例）","quantity":60,"expiry_date":"<EXAMPLE_EXPIRY-B>"}
		]}`))
	})

	at := time.Date(2026, 10, 20, 3, 10, 0, 0, time.FixedZone("CST", 8*3600))
	lookup, err := client.Batches(context.Background(), "point-vax-freezer-01", at)
	if err != nil {
		t.Fatalf("读台账在库批次失败：%v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("请求方法 = %q，期望 GET", gotMethod)
	}
	if gotPath != "/api/v1/batches" {
		t.Errorf("请求路径 = %q，期望 /api/v1/batches", gotPath)
	}
	if gotAuth != "Bearer test-ledger-token" {
		t.Errorf("鉴权头 = %q，期望 Bearer test-ledger-token", gotAuth)
	}
	if gotPointID != "point-vax-freezer-01" {
		t.Errorf("查询参数 point_id = %q，期望 point-vax-freezer-01", gotPointID)
	}
	if gotAt == "" {
		t.Error("查询参数缺少 at")
	}
	if lookup.Status != domain.BatchLookupOK {
		t.Errorf("查询状态 = %q，期望 %q", lookup.Status, domain.BatchLookupOK)
	}
	if len(lookup.Batches) != 2 {
		t.Fatalf("读到 %d 个批次，期望 2 个", len(lookup.Batches))
	}
	// 批次号原样保留：不改写、不去空格、更不生成新号。
	if lookup.Batches[0].BatchRef != "<EXAMPLE_LOT-01>" {
		t.Errorf("批次号 = %q，期望 <EXAMPLE_LOT-01>", lookup.Batches[0].BatchRef)
	}
	if lookup.Batches[0].PointID != "point-vax-freezer-01" {
		t.Errorf("批次点位 = %q，期望 point-vax-freezer-01", lookup.Batches[0].PointID)
	}
	if !lookup.Usable() {
		t.Error("查到批次时查询结果应当可用")
	}
}

// TestBatches_EmptyIsNotFailure 断言「查到了但没有在库批次」不是失败。
func TestBatches_EmptyIsNotFailure(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"batches":[]}`))
	})
	lookup, err := client.Batches(context.Background(), "point-vax-freezer-01", time.Now())
	if err != nil {
		t.Fatalf("空清单不应报错：%v", err)
	}
	if lookup.Status != domain.BatchLookupEmpty {
		t.Errorf("查询状态 = %q，期望 %q", lookup.Status, domain.BatchLookupEmpty)
	}
	if lookup.NeedsManual {
		t.Error("查到了空清单不该要求人工核对（库里确实没货，与没查成是两回事）")
	}
	if lookup.Usable() {
		t.Error("空清单不该算作可用批次清单")
	}
}

// TestBatches_PlaceholderEntriesDropped 断言没有批次号的占位条目被剔除。
func TestBatches_PlaceholderEntriesDropped(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"batches":[
			{"batch_ref":"<EXAMPLE_LOT-01>","product_name":"疫苗（脱敏样例）","quantity":120},
			{"batch_ref":"","product_name":"疫苗（脱敏样例）","quantity":0},
			{"product_name":"疫苗（脱敏样例）","quantity":10}
		]}`))
	})
	lookup, err := client.Batches(context.Background(), "point-vax-freezer-01", time.Now())
	if err != nil {
		t.Fatalf("读台账在库批次失败：%v", err)
	}
	if len(lookup.Batches) != 1 {
		t.Fatalf("保留 %d 个批次，期望 1 个（占位条目要剔除）", len(lookup.Batches))
	}
	if lookup.Batches[0].BatchRef != "<EXAMPLE_LOT-01>" {
		t.Errorf("保留的批次号 = %q，期望 <EXAMPLE_LOT-01>", lookup.Batches[0].BatchRef)
	}
}

// TestBatches_ForeignPointIsAmbiguous 断言归属别的点位的批次落歧义。
func TestBatches_ForeignPointIsAmbiguous(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"batches":[
			{"point_id":"point-vax-freezer-01","batch_ref":"<EXAMPLE_LOT-01>","product_name":"疫苗（脱敏样例）","quantity":120},
			{"point_id":"point-vax-freezer-02","batch_ref":"<EXAMPLE_LOT-77>","product_name":"疫苗（脱敏样例）","quantity":30}
		]}`))
	})
	lookup, err := client.Batches(context.Background(), "point-vax-freezer-01", time.Now())
	if err != nil {
		t.Fatalf("读台账在库批次失败：%v", err)
	}
	if lookup.Status != domain.BatchLookupAmbiguous {
		t.Errorf("查询状态 = %q，期望 %q", lookup.Status, domain.BatchLookupAmbiguous)
	}
	if !lookup.NeedsManual {
		t.Error("批次归属有歧义时必须要求人工核对")
	}
	// 别的点位的批次不许并进本次清单。
	for _, batch := range lookup.Batches {
		if batch.BatchRef == "<EXAMPLE_LOT-77>" {
			t.Error("别的点位的批次不该并入本次清单")
		}
	}
	if !strings.Contains(lookup.Detail, "point-vax-freezer-02") {
		t.Errorf("歧义说明 = %q，期望点名 point-vax-freezer-02", lookup.Detail)
	}
}

// TestBatches_TimeoutIsExplicit 断言超时是显式的、可重试的一档。
func TestBatches_TimeoutIsExplicit(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{"batches":[]}`))
	}, WithHTTPClient(&http.Client{Timeout: 50 * time.Millisecond}))

	lookup, err := client.Batches(context.Background(), "point-vax-freezer-01", time.Now())
	if err == nil {
		t.Fatal("超时必须报错")
	}
	if code := errcode.As(err).Code; code != errcode.LedgerUnavailable {
		t.Errorf("错误码 = %q，期望 %q", code, errcode.LedgerUnavailable)
	}
	if lookup.Status != domain.BatchLookupUnavailable {
		t.Errorf("查询状态 = %q，期望 %q", lookup.Status, domain.BatchLookupUnavailable)
	}
	if !lookup.NeedsManual {
		t.Error("台账没查成时必须要求人工核对")
	}
	if len(lookup.Batches) != 0 {
		t.Errorf("没查成时不得有批次，期望 0 条，实际 %d 条", len(lookup.Batches))
	}
}

// TestBatches_ServerErrorIsUnavailable 断言 5xx 归到「可退避重试」一档。
func TestBatches_ServerErrorIsUnavailable(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	})
	lookup, err := client.Batches(context.Background(), "point-vax-freezer-01", time.Now())
	if err == nil {
		t.Fatal("台账 5xx 必须报错")
	}
	if code := errcode.As(err).Code; code != errcode.LedgerUnavailable {
		t.Errorf("错误码 = %q，期望 %q", code, errcode.LedgerUnavailable)
	}
	if lookup.Status != domain.BatchLookupUnavailable || !lookup.NeedsManual {
		t.Errorf("查询状态 = %q / needs_manual=%v，期望 unavailable + true", lookup.Status, lookup.NeedsManual)
	}
}

// TestBatches_UnauthorizedIsDistinct 断言凭据被拒与「台账不可达」分得开。
func TestBatches_UnauthorizedIsDistinct(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"token invalid"}`))
	})
	_, err := client.Batches(context.Background(), "point-vax-freezer-01", time.Now())
	if err == nil {
		t.Fatal("凭据被拒必须报错")
	}
	if code := errcode.As(err).Code; code != errcode.Unauthorized {
		t.Errorf("错误码 = %q，期望 %q（凭据要去改，不是重试能解决的）", code, errcode.Unauthorized)
	}
}

// TestBatches_NotFoundIsDistinct 断言「台账上没有这个点位」与「台账挂了」分得开。
func TestBatches_NotFoundIsDistinct(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"point not registered"}`))
	})
	_, err := client.Batches(context.Background(), "point-vax-freezer-01", time.Now())
	if err == nil {
		t.Fatal("点位未登记必须报错")
	}
	if code := errcode.As(err).Code; code != errcode.NotFound {
		t.Errorf("错误码 = %q，期望 %q", code, errcode.NotFound)
	}
}

// TestBatches_MalformedBodyIsProtocolError 断言响应不是合法 JSON 时归协议错。
func TestBatches_MalformedBodyIsProtocolError(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>台账维护中</html>`))
	})
	lookup, err := client.Batches(context.Background(), "point-vax-freezer-01", time.Now())
	if err == nil {
		t.Fatal("非 JSON 响应必须报错")
	}
	if code := errcode.As(err).Code; code != errcode.LedgerProtocol {
		t.Errorf("错误码 = %q，期望 %q", code, errcode.LedgerProtocol)
	}
	if lookup.Status != domain.BatchLookupUnavailable {
		t.Errorf("查询状态 = %q，期望 %q", lookup.Status, domain.BatchLookupUnavailable)
	}
}

// TestBatches_SuccessIsCached 断言同一份查询在有效期内只打一次台账。
func TestBatches_SuccessIsCached(t *testing.T) {
	var calls int32
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"batches":[{"batch_ref":"<EXAMPLE_LOT-01>","product_name":"疫苗（脱敏样例）","quantity":120}]}`))
	})
	at := time.Date(2026, 10, 20, 3, 10, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if _, err := client.Batches(context.Background(), "point-vax-freezer-01", at); err != nil {
			t.Fatalf("第 %d 次读失败：%v", i+1, err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("台账被调用 %d 次，期望 1 次（有效期内只读一次）", got)
	}
}

// TestBatches_FailureIsNotCachedAsData 断言读失败不会被缓存成一份可用的清单。
func TestBatches_FailureIsNotCachedAsData(t *testing.T) {
	var calls int32
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		_, _ = w.Write([]byte(`{"batches":[{"batch_ref":"<EXAMPLE_LOT-01>","product_name":"疫苗（脱敏样例）","quantity":120}]}`))
	})
	at := time.Date(2026, 10, 20, 3, 10, 0, 0, time.UTC)

	lookup, err := client.Batches(context.Background(), "point-vax-freezer-01", at)
	if err == nil {
		t.Fatal("首次读应失败")
	}
	if len(lookup.Batches) != 0 {
		t.Error("失败结果不得带批次")
	}
	// 第二次读必须真的再打一次台账：失败不能被当成有效数据缓存住。
	lookup, err = client.Batches(context.Background(), "point-vax-freezer-01", at)
	if err != nil {
		t.Fatalf("第二次读失败：%v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("台账被调用 %d 次，期望 2 次（失败不缓存）", got)
	}
	if len(lookup.Batches) != 1 {
		t.Errorf("第二次读到 %d 个批次，期望 1 个", len(lookup.Batches))
	}
}

// TestBatches_ExpiredCacheIsNotServed 断言过期缓存不当作现状发出去。
func TestBatches_ExpiredCacheIsNotServed(t *testing.T) {
	var calls int32
	now := time.Date(2026, 10, 20, 3, 10, 0, 0, time.UTC)
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"batches":[{"batch_ref":"<EXAMPLE_LOT-01>","product_name":"疫苗（脱敏样例）","quantity":120}]}`))
	}, WithClock(func() time.Time { return now }))

	at := now
	if _, err := client.Batches(context.Background(), "point-vax-freezer-01", at); err != nil {
		t.Fatalf("首次读失败：%v", err)
	}
	// 时钟走过缓存有效期后再读：必须重新打台账，不能拿过期库存当现状。
	now = now.Add(2 * time.Minute)
	if _, err := client.Batches(context.Background(), "point-vax-freezer-01", at); err != nil {
		t.Fatalf("第二次读失败：%v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("台账被调用 %d 次，期望 2 次（过期缓存要重新取）", got)
	}
}

// TestNewClient_ConfigErrors 断言缺基址或缺凭据时装配即失败。
func TestNewClient_ConfigErrors(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"缺基址", Config{Token: "t"}},
		{"缺凭据", Config{BaseURL: "https://ledger.example.internal"}},
		{"基址不合法", Config{BaseURL: "ledger.example.internal", Token: "t"}},
		{"基址无主机名", Config{BaseURL: "https://", Token: "t"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewClient(tc.cfg)
			if err == nil {
				t.Fatal("配置不合法时装配必须失败")
			}
			if code := errcode.As(err).Code; code != errcode.ConfigMissing {
				t.Errorf("错误码 = %q，期望 %q", code, errcode.ConfigMissing)
			}
		})
	}
}

// TestConfigFromEnv_MissingItems 断言环境变量缺项时报出缺哪几项。
func TestConfigFromEnv_MissingItems(t *testing.T) {
	t.Setenv("COLDCHAIN_LEDGER_BASE_URL", "")
	t.Setenv("COLDCHAIN_LEDGER_TOKEN", "")
	_, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("缺必填环境变量时装配必须失败")
	}
	e := errcode.As(err)
	if !strings.Contains(e.Details, "COLDCHAIN_LEDGER_BASE_URL") ||
		!strings.Contains(e.Details, "COLDCHAIN_LEDGER_TOKEN") {
		t.Errorf("错误详情 = %q，期望点名两个必填项", e.Details)
	}
}

// TestBatches_EmptyPointIDRejected 断言缺点位标识时报错而不是去查台账。
func TestBatches_EmptyPointIDRejected(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("点位为空时不该打台账")
	})
	if _, err := client.Batches(context.Background(), "  ", time.Now()); err == nil {
		t.Fatal("点位为空必须报错")
	}
}

// TestBatches_CallerCancelIsWrapped 断言调用方取消时错误可被 errors.Is 识别。
func TestBatches_CallerCancelIsWrapped(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`{"batches":[]}`))
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := client.Batches(ctx, "point-vax-freezer-01", time.Now())
	if err == nil {
		t.Fatal("调用方取消必须报错")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("错误 = %v，期望 errors.Is(err, context.Canceled) 为真", err)
	}
}

// TestBatches_LookupSerializesWithStatus 断言查询结果序列化后状态可读。
func TestBatches_LookupSerializesWithStatus(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"batches":[{"batch_ref":"<EXAMPLE_LOT-01>","product_name":"疫苗（脱敏样例）","quantity":120}]}`))
	})
	lookup, err := client.Batches(context.Background(), "point-vax-freezer-01", time.Now())
	if err != nil {
		t.Fatalf("读台账在库批次失败：%v", err)
	}
	raw, err := json.Marshal(lookup)
	if err != nil {
		t.Fatalf("序列化查询结果失败：%v", err)
	}
	if !strings.Contains(string(raw), `"status":"ok"`) {
		t.Errorf("序列化结果 = %s，期望含 status=ok", raw)
	}
	if !strings.Contains(string(raw), `"source":"cold_chain_ledger"`) {
		t.Errorf("序列化结果 = %s，期望含数据来源标记", raw)
	}
}
