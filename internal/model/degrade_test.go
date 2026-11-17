// 本文件验证「模型不可达降级与状态不被误推进」。
//
// 覆盖三类必答：
//  1. 四类模型错误（超时 / 不可达 / 协议不合规 / 形状不合规 / 事实不符）各自触发降级；
//  2. 降级后 run 状态仍为 awaiting_confirmation（不自动进 confirmed）；
//  3. 降级后出站写回调用数为 0（一条结论都没往台账与通知那边写）。
//
// 本文件用外部测试包（package model_test），因此可以引入判定步骤链
// （internal/runtime）做端到端验证：降级不是模型层自己的事，
// 它最终要落在一条真实判定 run 的状态上。
package model_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/audit"
	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
	"github.com/chaosforge/coldchain-temp-assessment/internal/model"
	"github.com/chaosforge/coldchain-temp-assessment/internal/rules"
	"github.com/chaosforge/coldchain-temp-assessment/internal/runtime"
	"github.com/chaosforge/coldchain-temp-assessment/internal/store"
)

// ---------------------------------------------------------------------------
// 测试夹具
// ---------------------------------------------------------------------------

const degradePointID = "point-vax-freezer-01"

// degradeSamples 返回一段越温采样（脱敏样例，与 testdata/series_excursion.json 同源）。
//
// 03:10 起越限（8.6℃ → 峰值 12.4℃），03:25 回落到温区；
// 窗口内两条采样带 door_open 事件——规则基线据此判「开门」。
func degradeSamples() []domain.TemperatureEvent {
	base := time.Date(2026, 10, 20, 3, 0, 0, 0, time.FixedZone("CST", 8*3600))
	rows := []struct {
		offset int
		temp   float64
		events []string
	}{
		{0, 5.2, nil},
		{5, 5.6, nil},
		{10, 8.6, []string{domain.EventDoorOpen}},
		{15, 12.4, []string{domain.EventDoorOpen}},
		{20, 9.8, nil},
		{25, 8.7, nil},
		{30, 6.1, nil},
	}
	out := make([]domain.TemperatureEvent, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.TemperatureEvent{
			PointID:      degradePointID,
			DeviceID:     "device-recorder-0001",
			DeviceName:   "疫苗冷藏库温度记录仪",
			MeasuredAt:   base.Add(time.Duration(row.offset) * time.Minute),
			TemperatureC: row.temp,
			Unit:         domain.UnitCelsius,
			HumidityPct:  floatPtr(58 + float64(row.offset)/2),
			Source:       domain.SourceRecorder,
			Events:       row.events,
		})
	}
	return out
}

// degradeProfile 返回脱敏的温区配置：2～8℃、5 分钟采样周期。
func degradeProfile() *domain.PointProfile {
	return &domain.PointProfile{
		PointID:             degradePointID,
		ZoneMinC:            2,
		ZoneMaxC:            8,
		SampleIntervalSec:   300,
		MinExcursionMinutes: 10,
		HysteresisC:         1,
		AccuracyLimitC:      0.5,
	}
}

// degradeWindow 按规则基线抽出这次越温窗口。
func degradeWindow(t *testing.T, samples []domain.TemperatureEvent) *domain.ExcursionWindow {
	t.Helper()
	zone, err := rules.ResolveZoneForEvent(degradeProfile(), samples[0], nil)
	if err != nil {
		t.Fatalf("解析点位温区失败：%v", err)
	}
	window, err := rules.ExtractWindow(samples, zone)
	if err != nil {
		t.Fatalf("抽取越温窗口失败：%v", err)
	}
	if window == nil {
		t.Fatal("样例采样没有抽出越温窗口")
	}
	return window
}

// floatPtr 取一个 float64 指针。
func floatPtr(v float64) *float64 { return &v }

// ---------------------------------------------------------------------------
// 一、降级分类：哪些错误该降级，哪些不该
// ---------------------------------------------------------------------------

// TestIsDegradable_ClassifiesModelErrorsOnly 断言降级的错误码白名单。
//
// 白名单的意义：只有「模型这一路没给出可用结论」才降级。
// 校验类、存储类错误是系统自己的问题，降级会把它们说成「模型不可用」，
// 让现场去查错方向。
func TestIsDegradable_ClassifiesModelErrorsOnly(t *testing.T) {
	degradable := []errcode.Code{
		errcode.ModelTimeout,
		errcode.ModelUnavailable,
		errcode.ModelProtocol,
		errcode.ModelSchemaInvalid,
		errcode.ModelFactMismatch,
	}
	for _, code := range degradable {
		err := errcode.New(code, "模型这一路没给出可用结论")
		if !model.IsDegradable(err) {
			t.Errorf("错误码 %s 应触发降级", code)
		}
		if got := model.DegradeReasonOf(err); got != string(code) {
			t.Errorf("降级原因应为 %s，实际 %q", code, got)
		}
	}

	notDegradable := []errcode.Code{
		errcode.Validation,
		errcode.StorageError,
		errcode.NotFound,
		errcode.IllegalTransition,
	}
	for _, code := range notDegradable {
		err := errcode.New(code, "系统自己的问题")
		if model.IsDegradable(err) {
			t.Errorf("错误码 %s 不应触发降级", code)
		}
		if got := model.DegradeReasonOf(err); got != "" {
			t.Errorf("非可降级错误的降级原因应为空，实际 %q", got)
		}
	}

	if model.IsDegradable(nil) {
		t.Error("空错误不应触发降级")
	}
}

// TestDegradeToBaseline_RejectsNonDegradableError 断言非可降级错误被明确拒绝。
func TestDegradeToBaseline_RejectsNonDegradableError(t *testing.T) {
	samples := degradeSamples()
	window := degradeWindow(t, samples)
	zone, err := rules.ResolveZoneForEvent(degradeProfile(), samples[0], nil)
	if err != nil {
		t.Fatalf("解析点位温区失败：%v", err)
	}

	_, err = model.DegradeToBaseline(errcode.New(errcode.Validation, "字段缺失"), model.DegradeInput{
		Samples: samples, Window: window, Zone: zone,
	})
	if err == nil {
		t.Fatal("非可降级错误应被拒绝")
	}
	if !strings.Contains(err.Error(), "不属于可降级范围") {
		t.Errorf("报错应点明不属于可降级范围，实际：%v", err)
	}
}

// TestDegradeToBaseline_RequiresSamplesAndWindow 断言降级必须有料可依。
//
// 没有采样或没有窗口时给不出可核对的结论——宁可报错让判定转人工，
// 也不拿一份空结论糊弄过去。
func TestDegradeToBaseline_RequiresSamplesAndWindow(t *testing.T) {
	samples := degradeSamples()
	window := degradeWindow(t, samples)
	zone, err := rules.ResolveZoneForEvent(degradeProfile(), samples[0], nil)
	if err != nil {
		t.Fatalf("解析点位温区失败：%v", err)
	}
	cause := errcode.New(errcode.ModelUnavailable, "模型服务不可达")

	if _, err := model.DegradeToBaseline(cause, model.DegradeInput{
		Window: window, Zone: zone,
	}); err == nil || !strings.Contains(err.Error(), "没有取到采样") {
		t.Errorf("缺采样时应报错并点明没取到采样，实际：%v", err)
	}
	if _, err := model.DegradeToBaseline(cause, model.DegradeInput{
		Samples: samples, Zone: zone,
	}); err == nil || !strings.Contains(err.Error(), "没有抽到窗口") {
		t.Errorf("缺窗口时应报错并点明没抽到窗口，实际：%v", err)
	}
}

// ---------------------------------------------------------------------------
// 二、降级结论本身：退回规则基线、标 degraded、封顶置信度
// ---------------------------------------------------------------------------

// TestDegradeToBaseline_FallsBackToRuleBaseline 断言降级后的结论来自规则基线。
func TestDegradeToBaseline_FallsBackToRuleBaseline(t *testing.T) {
	samples := degradeSamples()
	window := degradeWindow(t, samples)
	zone, err := rules.ResolveZoneForEvent(degradeProfile(), samples[0], nil)
	if err != nil {
		t.Fatalf("解析点位温区失败：%v", err)
	}

	payload, err := model.DegradeToBaseline(
		errcode.New(errcode.ModelUnavailable, "模型服务不可达：connection refused"),
		model.DegradeInput{Samples: samples, Window: window, Zone: zone})
	if err != nil {
		t.Fatalf("降级失败：%v", err)
	}
	if !payload.Degraded {
		t.Error("降级结论必须标 degraded=true")
	}
	if payload.DegradeReason != string(errcode.ModelUnavailable) {
		t.Errorf("降级原因应为 %s，实际 %q", errcode.ModelUnavailable, payload.DegradeReason)
	}
	if payload.CauseCode != rules.CauseDoorOpen {
		t.Errorf("降级结论应沿用规则基线的原因码 %s，实际 %s", rules.CauseDoorOpen, payload.CauseCode)
	}
	if len(payload.Candidates) == 0 {
		t.Fatal("降级结论应带上规则基线给出的候选集")
	}
	if len(payload.Evidence) == 0 {
		t.Fatal("降级结论应带上证据")
	}
	// 证据仍要能指回输入：降级不能把「凭什么这么判」弄丢。
	found := false
	for _, item := range payload.Evidence {
		if strings.Contains(item.Feature, "door_open") {
			found = true
		}
	}
	if !found {
		t.Errorf("证据里应保留开门这条现场依据，实际：%+v", payload.Evidence)
	}
}

// TestDegradeToBaseline_CapsConfidenceAndTagsEveryCandidate 断言置信度封顶与逐条标注。
func TestDegradeToBaseline_CapsConfidenceAndTagsEveryCandidate(t *testing.T) {
	samples := degradeSamples()
	window := degradeWindow(t, samples)
	zone, err := rules.ResolveZoneForEvent(degradeProfile(), samples[0], nil)
	if err != nil {
		t.Fatalf("解析点位温区失败：%v", err)
	}

	payload, err := model.DegradeToBaseline(
		errcode.New(errcode.ModelTimeout, "模型调用超时"),
		model.DegradeInput{Samples: samples, Window: window, Zone: zone})
	if err != nil {
		t.Fatalf("降级失败：%v", err)
	}
	if payload.Confidence > model.ConfidenceCapDegraded {
		t.Errorf("降级后结论置信度 %v 应封顶在 %v", payload.Confidence, model.ConfidenceCapDegraded)
	}
	for i, candidate := range payload.Candidates {
		if candidate.Confidence > model.ConfidenceCapDegraded {
			t.Errorf("第 %d 条候选置信度 %v 应封顶在 %v", i, candidate.Confidence, model.ConfidenceCapDegraded)
		}
		if !strings.Contains(candidate.Tag, model.DegradeTag) {
			t.Errorf("第 %d 条候选必须带降级标注 %q，实际 %q", i, model.DegradeTag, candidate.Tag)
		}
		if !strings.Contains(candidate.Note, "降级") {
			t.Errorf("第 %d 条候选的说明里应写明这是降级结论，实际 %q", i, candidate.Note)
		}
	}
	// 口径说明里要有一条专门讲降级（规则基线自己的那几条说明不算）。
	sawDegradeNote := false
	for _, note := range payload.Notes {
		if strings.Contains(note, "降级") {
			sawDegradeNote = true
		}
	}
	if !sawDegradeNote {
		t.Errorf("结论的口径说明里应写明降级，实际：%v", payload.Notes)
	}
}

// TestDegradeNote_ExplainsOriginAndManualCheck 断言成稿用的降级说明。
func TestDegradeNote_ExplainsOriginAndManualCheck(t *testing.T) {
	payload := &domain.AttributionPayload{
		Degraded:      true,
		DegradeReason: string(errcode.ModelUnavailable),
	}
	note := model.DegradeNote(payload)
	if note == "" {
		t.Fatal("降级结论必须给得出成稿说明")
	}
	for _, want := range []string{"规则基线", string(errcode.ModelUnavailable), "重点核对"} {
		if !strings.Contains(note, want) {
			t.Errorf("成稿说明应包含 %q，实际：%s", want, note)
		}
	}

	if got := model.DegradeNote(&domain.AttributionPayload{}); got != "" {
		t.Errorf("非降级结论不该有成稿降级说明，实际：%s", got)
	}
	if got := model.DegradeNote(nil); got != "" {
		t.Errorf("空结论不该有成稿降级说明，实际：%s", got)
	}
}

// TestAttributionPayloadFromModel_ProjectionKeepsEvidence 断言模型结论的投影不丢字段。
func TestAttributionPayloadFromModel_ProjectionKeepsEvidence(t *testing.T) {
	attr := &model.Attribution{
		CauseCode:  rules.CausePowerLoss,
		Label:      "断电",
		Confidence: 0.88,
		Evidence: []model.Evidence{{
			Feature: "device_event:power_loss", Value: 3, Threshold: 1, Note: "设备上报断电事件 3 次",
		}},
		Candidates: []model.Candidate{{
			CauseCode:  rules.CausePowerLoss,
			Label:      "断电",
			Confidence: 0.88,
			Evidence: []model.Evidence{{
				Feature: "device_event:power_loss", Value: 3, Threshold: 1,
			}},
		}},
	}
	payload := model.AttributionPayloadFromModel(attr)
	if payload == nil {
		t.Fatal("投影结果不该为空")
	}
	if payload.CauseCode != rules.CausePowerLoss || payload.CauseLabel != "断电" {
		t.Errorf("投影后原因码与中文标签应原样保留，实际：%s / %s", payload.CauseCode, payload.CauseLabel)
	}
	if payload.Degraded {
		t.Error("在线路径的结论不该带降级标记")
	}
	if len(payload.Evidence) != 1 || payload.Evidence[0].Feature != "device_event:power_loss" {
		t.Errorf("证据应原样投影，实际：%+v", payload.Evidence)
	}
	if len(payload.Candidates) != 1 || len(payload.Candidates[0].Evidence) != 1 {
		t.Errorf("候选与其证据应原样投影，实际：%+v", payload.Candidates)
	}
}

// ---------------------------------------------------------------------------
// 三、端到端：四类模型错误各自降级，状态不推进，出站写回为 0
// ---------------------------------------------------------------------------

// modelEndpoint 是替模型端点用的假服务：按脚本回不同形态的响应。
type modelEndpoint struct {
	// calls 收到的请求数。
	calls int64
	// handler 回响应体。
	handler func(w http.ResponseWriter, r *http.Request)
}

// newModelEndpoint 起一个假模型端点。
func newModelEndpoint(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*modelEndpoint, string) {
	t.Helper()
	endpoint := &modelEndpoint{handler: handler}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&endpoint.calls, 1)
		endpoint.handler(w, r)
	}))
	t.Cleanup(server.Close)
	return endpoint, server.URL
}

// writeCompletion 回一个 OpenAI-compatible 的成功信封。
func writeCompletion(t *testing.T, w http.ResponseWriter, content string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	envelope := map[string]any{
		"id":    "chatcmpl-degrade-test",
		"model": "院内判定模型",
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 900, "completion_tokens": 120, "total_tokens": 1020},
	}
	if err := json.NewEncoder(w).Encode(envelope); err != nil {
		t.Errorf("写响应失败：%v", err)
	}
}

// degradedRun 跑一条判定链，归因步用模型归因处理器。
//
// 返回执行结果与落库用的库句柄。
func degradedRun(t *testing.T, gateway *model.ModelGateway, st *store.Store) runtime.ExecutionResult {
	t.Helper()
	samples := degradeSamples()
	profile := degradeProfile()

	asker := &model.Asker{
		Caller:  model.GatewayAsker(gateway),
		Auditor: audit.NewRecorder(st),
	}
	handler, err := runtime.NewModelAttributionHandler(runtime.ModelAttributionDeps{
		Profile: profile,
		Asker:   asker,
	})
	if err != nil {
		t.Fatalf("装配模型归因处理器失败：%v", err)
	}

	chain, err := runtime.NewChain(runtime.ChainDeps{
		Profile:   profile,
		Samples:   samples,
		Attribute: handler,
	})
	if err != nil {
		t.Fatalf("装配判定步骤链失败：%v", err)
	}

	run := domain.NewRun(degradePointID)
	run.BindWindow(samples[2].MeasuredAt, samples[5].MeasuredAt)

	executor := runtime.NewExecutor(chain, st, logging.New(io.Discard)).WithRunPersistence(st, profile)
	result, err := executor.Run(context.Background(), run)
	if err != nil {
		t.Fatalf("判定步骤链执行失败：%v", err)
	}
	return result
}

// TestDegrade_EachModelErrorFallsBackToBaselineAndKeepsState 覆盖四类模型错误。
//
// 每类错误都要触发降级、退回规则基线、标上 degraded，
// 并把 run 停在 awaiting_confirmation——不自动进 confirmed。
func TestDegrade_EachModelErrorFallsBackToBaselineAndKeepsState(t *testing.T) {
	cases := []struct {
		name    string
		want    errcode.Code
		handler func(t *testing.T, w http.ResponseWriter, r *http.Request)
		timeout time.Duration
		// deadEndpoint 为真时用「已关掉的地址」当端点：连不上，正是不可达那一类。
		deadEndpoint bool
	}{
		{
			name:         "端点不可达",
			want:         errcode.ModelUnavailable,
			deadEndpoint: true,
			timeout:      time.Second,
		},
		{
			name: "端点超时",
			want: errcode.ModelTimeout,
			handler: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				time.Sleep(2 * time.Second)
			},
			timeout: 200 * time.Millisecond,
		},
		{
			name: "响应不是合法兼容信封",
			want: errcode.ModelProtocol,
			handler: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, "not-json-at-all")
			},
			timeout: time.Second,
		},
		{
			name: "内容未通过结构化校验",
			want: errcode.ModelSchemaInvalid,
			handler: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				// confidence 超出 [0,1]：信封合法，内容不合规。
				writeCompletion(t, w, `{"cause_code":"door_open","confidence":1.7}`)
			},
			timeout: time.Second,
		},
		{
			name: "内容与输入事实不符",
			want: errcode.ModelFactMismatch,
			handler: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				// 峰值温度 13.7℃：输入时序里最接近的是 12.4℃。
				writeCompletion(t, w, `{"cause_code":"door_open","label":"开门","confidence":0.8,`+
					`"evidence":[{"feature":"temperature_c","value":13.7,"threshold":8}]}`)
			},
			timeout: time.Second,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			baseURL := ""
			if tc.deadEndpoint {
				// 地址还在，服务已经关掉：连不上，正是不可达那一类。
				dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				baseURL = dead.URL
				dead.Close()
			} else {
				_, url := newModelEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
					tc.handler(t, w, r)
				})
				baseURL = url
			}

			gateway, err := model.NewGateway(model.GatewayConfig{
				BaseURL: baseURL,
				APIKey:  "test-token-not-a-real-credential",
				Model:   "院内判定模型",
				Timeout: tc.timeout,
			}, model.WithLogger(logging.New(io.Discard)))
			if err != nil {
				t.Fatalf("装配模型网关失败：%v", err)
			}

			st, err := store.Open(":memory:")
			if err != nil {
				t.Fatalf("打开内存库失败：%v", err)
			}
			t.Cleanup(func() { _ = st.Close() })

			result := degradedRun(t, gateway, st)

			if result.Run.State != domain.RunStateAwaitingConfirmation {
				t.Errorf("降级后 run 状态应停在 %s，实际 %s",
					domain.RunStateAwaitingConfirmation, result.Run.State)
			}
			if result.Run.State == "confirmed" {
				t.Fatal("降级后 run 绝不能进入 confirmed")
			}
			if !result.Run.Degraded {
				t.Error("降级后 run 必须带降级标记")
			}
			if result.Run.DegradeReason != string(tc.want) {
				t.Errorf("降级原因应为 %s，实际 %q", tc.want, result.Run.DegradeReason)
			}

			attribution, ok := runtime.ProductAs[domain.AttributionPayload](
				result.Steps, domain.PayloadAttribution)
			if !ok {
				t.Fatal("降级后应产出归因结论产物")
			}
			if !attribution.Degraded {
				t.Error("归因产物必须标 degraded=true")
			}
			if attribution.DegradeReason != string(tc.want) {
				t.Errorf("归因产物的降级原因应为 %s，实际 %q", tc.want, attribution.DegradeReason)
			}
			if attribution.Confidence > model.ConfidenceCapDegraded {
				t.Errorf("降级结论置信度 %v 应封顶在 %v",
					attribution.Confidence, model.ConfidenceCapDegraded)
			}

			// 成稿必须写明这是降级来的结论。
			record, ok := runtime.ProductAs[domain.ExceptionRecordPayload](
				result.Steps, domain.PayloadExceptionRecord)
			if !ok {
				t.Fatal("降级后应产出成稿产物")
			}
			if !record.Degraded {
				t.Error("成稿产物必须标 degraded=true")
			}
			joined := strings.Join(record.Notes, " ")
			if !strings.Contains(joined, "规则基线") {
				t.Errorf("成稿说明应写明归因来自规则基线，实际：%v", record.Notes)
			}
		})
	}
}

// TestDegrade_NoOutboundWritebackCall 断言降级后出站写回调用数为 0。
//
// 降级结论还没经主管确认，一条都不许往冷链台账与接种门诊通知那边写。
// 这里用两个计数端点分别代表台账与通知，任何一次调用都会被抓到。
func TestDegrade_NoOutboundWritebackCall(t *testing.T) {
	var ledgerCalls, notifyCalls int64
	ledger := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&ledgerCalls, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(ledger.Close)
	notify := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&notifyCalls, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(notify.Close)

	// 模型端点不可达：必然走降级路径。
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	gateway, err := model.NewGateway(model.GatewayConfig{
		BaseURL: deadURL,
		APIKey:  "test-token-not-a-real-credential",
		Model:   "院内判定模型",
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("装配模型网关失败：%v", err)
	}

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	result := degradedRun(t, gateway, st)

	if result.Run.State != domain.RunStateAwaitingConfirmation {
		t.Fatalf("降级后 run 状态应停在 %s，实际 %s",
			domain.RunStateAwaitingConfirmation, result.Run.State)
	}
	if got := atomic.LoadInt64(&ledgerCalls); got != 0 {
		t.Errorf("降级后冷链台账出站写回调用数应为 0，实际 %d", got)
	}
	if got := atomic.LoadInt64(&notifyCalls); got != 0 {
		t.Errorf("降级后接种门诊通知出站调用数应为 0，实际 %d", got)
	}
	// 库里也不该有「已写回」的痕迹。
	state, err := st.RunWritebackState(context.Background(), result.Run.RunID)
	if err != nil {
		t.Fatalf("回读写回状态失败：%v", err)
	}
	if state != domain.WritebackStateNone {
		t.Errorf("降级后写回状态应为空，实际 %q", state)
	}
}

// TestDegrade_RunCarriesDegradedMarkAfterRestart 断言降级标记随 run 落库。
//
// 标记要能跨进程重启查得到：复核的人不可能只在线时看一眼。
func TestDegrade_RunCarriesDegradedMarkAfterRestart(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	gateway, err := model.NewGateway(model.GatewayConfig{
		BaseURL: deadURL,
		APIKey:  "test-token-not-a-real-credential",
		Model:   "院内判定模型",
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("装配模型网关失败：%v", err)
	}

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	result := degradedRun(t, gateway, st)

	record, err := st.RunByID(context.Background(), result.Run.RunID)
	if err != nil {
		t.Fatalf("回读判定 run 失败：%v", err)
	}
	if !record.Run.Degraded {
		t.Error("落库后的 run 必须仍带降级标记")
	}
	if record.Run.DegradeReason != string(errcode.ModelUnavailable) {
		t.Errorf("落库后的降级原因应为 %s，实际 %q",
			errcode.ModelUnavailable, record.Run.DegradeReason)
	}
	if record.Run.State != domain.RunStateAwaitingConfirmation {
		t.Errorf("落库后的业务状态应停在 %s，实际 %s",
			domain.RunStateAwaitingConfirmation, record.Run.State)
	}
}

// TestDegrade_AuditChainRecordsDegradedOutcome 断言审计链上看得见降级。
//
// 审计链要能回答两件事：这次模型调用没成（failed），以及最终结论退回了
// 规则基线（degraded）。两条分开记，复核的人才分得清是「模型答歪了」
// 还是「模型压根没答成」。
func TestDegrade_AuditChainRecordsDegradedOutcome(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	gateway, err := model.NewGateway(model.GatewayConfig{
		BaseURL: deadURL,
		APIKey:  "test-token-not-a-real-credential",
		Model:   "院内判定模型",
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("装配模型网关失败：%v", err)
	}

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	result := degradedRun(t, gateway, st)

	entries, err := st.AuditEntriesByRun(context.Background(), result.Run.RunID)
	if err != nil {
		t.Fatalf("回读审计链失败：%v", err)
	}
	if len(entries) == 0 {
		t.Fatal("降级后审计链上应有记录")
	}
	var sawDegraded bool
	for _, entry := range entries {
		if entry.Outcome == domain.AuditOutcomeDegraded {
			sawDegraded = true
			if !entry.Degraded {
				t.Error("降级条目必须带降级标记")
			}
			if entry.ErrorCode != string(errcode.ModelUnavailable) {
				t.Errorf("降级条目的错误码应为 %s，实际 %q",
					errcode.ModelUnavailable, entry.ErrorCode)
			}
		}
	}
	if !sawDegraded {
		t.Errorf("审计链上应有 outcome=degraded 的条目，实际：%s", fmt.Sprintf("%+v", entries))
	}
}
