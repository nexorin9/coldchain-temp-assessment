package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// 本文件是出站退避重试的测试。
//
// 它盯住四件事：
//
//  1. **该退的退**：台账超时 → 退避后重试 → 第二次成功。
//     这一条是现场价值最高的一条：台账网关抖动一下，
//     这次越温不该就变成「账上查不到」。
//  2. **退不动就交账**：重试耗尽返回带尝试次数的错误，
//     上层据此把这条越温挂到 pending_writeback 并留告警
//     （落点在 runtime 包，见 TestCompensation_*）。
//  3. **不该退的不退**：凭据被拒、请求不合法，重发只是把同一个错再犯一遍。
//  4. **重放不重复入账**：补偿重放用的是同一把幂等键，
//     台账侧认出是同一次越温，不会多出一条记录。

// noSleep 是不真的等待的退避实现，顺带把退避序列记下来。
func noSleep(record *[]time.Duration) func(context.Context, time.Duration) error {
	return func(_ context.Context, d time.Duration) error {
		if record != nil {
			*record = append(*record, d)
		}
		return nil
	}
}

// TestRetry_TimeoutThenRetrySucceeds 覆盖主路径：
// 第一次调用超时 → 退避 → 第二次成功，同一条越温照常入账。
func TestRetry_TimeoutThenRetrySucceeds(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			// 第一次：让客户端超时（客户端时限 100ms，这里睡 400ms）。
			time.Sleep(400 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"dev-001"}`))
	}))
	t.Cleanup(server.Close)

	inner, err := NewDeviationWriter(WritebackConfig{
		BaseURL: server.URL,
		Token:   "test-ledger-token",
		Timeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("装配台账写侧失败：%v", err)
	}
	var backoffs []time.Duration
	retrying := NewRetryingDeviationWriter(inner, RetryPolicy{
		MaxRetries: 2,
		BaseDelay:  time.Second,
		MaxDelay:   8 * time.Second,
		Sleep:      noSleep(&backoffs),
	}, nil)

	if err := retrying.Write(context.Background(), sampleDeviation()); err != nil {
		t.Fatalf("超时后重试应当成功，实际失败：%v", err)
	}

	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != 2 {
		t.Fatalf("台账收到的请求数 = %d，期望 2（首次超时 + 重试成功）", gotCalls)
	}
	if len(backoffs) != 1 || backoffs[0] != time.Second {
		t.Fatalf("退避序列 = %v，期望恰好一次 1s 退避", backoffs)
	}
}

// TestRetry_ExhaustedReportsAttemptsForPendingWriteback 覆盖退避耗尽：
// 台账一直不可用 → 尝试用尽 → 返回带尝试次数的错误，
// 上层据此把这条越温挂到 pending_writeback。
//
// 挂到 pending_writeback 与留告警的落点在 runtime 包
// （internal/runtime/compensation_test.go）——ledger 包不认识业务状态，
// 它只负责把「试过几次、最后错在哪」交清楚。
func TestRetry_ExhaustedReportsAttemptsForPendingWriteback(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	inner, err := NewDeviationWriter(WritebackConfig{
		BaseURL: server.URL,
		Token:   "test-ledger-token",
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("装配台账写侧失败：%v", err)
	}
	var backoffs []time.Duration
	retrying := NewRetryingDeviationWriter(inner, RetryPolicy{
		MaxRetries: 2,
		BaseDelay:  time.Second,
		MaxDelay:   8 * time.Second,
		Sleep:      noSleep(&backoffs),
	}, nil)

	writeErr := retrying.Write(context.Background(), sampleDeviation())
	if writeErr == nil {
		t.Fatal("台账一直不可用时写回应当失败")
	}

	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != 3 {
		t.Fatalf("台账收到的请求数 = %d，期望 3（首次 + 2 次退避重试）", gotCalls)
	}
	if len(backoffs) != 2 {
		t.Fatalf("退避次数 = %d，期望 2", len(backoffs))
	}
	if backoffs[0] != time.Second || backoffs[1] != 2*time.Second {
		t.Fatalf("退避序列 = %v，期望 [1s 2s]（指数增长）", backoffs)
	}

	var exhausted *ExhaustedError
	if !errors.As(writeErr, &exhausted) {
		t.Fatalf("错误 = %v（%T），期望 *ExhaustedError", writeErr, writeErr)
	}
	if exhausted.Attempts != 3 {
		t.Errorf("耗尽时的尝试次数 = %d，期望 3", exhausted.Attempts)
	}
	if got := AttemptsOf(writeErr); got != 3 {
		t.Errorf("AttemptsOf = %d，期望 3", got)
	}
	// 错误码分档不能被「重试耗尽」盖掉：上层据此决定是挂起还是报人。
	if code := errcode.As(writeErr).Code; code != errcode.LedgerUnavailable {
		t.Errorf("错误码 = %q，期望 E_LEDGER_UNAVAILABLE", code)
	}
}

// TestRetry_NonRetryableFailureIsNotRetried 覆盖不该退的不退：
// 凭据被拒与请求不合法重发也没用，只发一次。
func TestRetry_NonRetryableFailureIsNotRetried(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{"凭据被拒不重试", http.StatusUnauthorized},
		{"请求不合法不重试", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				calls++
				mu.Unlock()
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(server.Close)

			inner, err := NewDeviationWriter(WritebackConfig{
				BaseURL: server.URL,
				Token:   "test-ledger-token",
				Timeout: 2 * time.Second,
			})
			if err != nil {
				t.Fatalf("装配台账写侧失败：%v", err)
			}
			var backoffs []time.Duration
			retrying := NewRetryingDeviationWriter(inner, RetryPolicy{
				MaxRetries: 2,
				BaseDelay:  time.Second,
				MaxDelay:   8 * time.Second,
				Sleep:      noSleep(&backoffs),
			}, nil)

			writeErr := retrying.Write(context.Background(), sampleDeviation())
			if writeErr == nil {
				t.Fatal("应当失败")
			}
			mu.Lock()
			gotCalls := calls
			mu.Unlock()
			if gotCalls != 1 {
				t.Fatalf("台账收到的请求数 = %d，期望 1（不可重试的错误不该重发）", gotCalls)
			}
			if len(backoffs) != 0 {
				t.Fatalf("退避次数 = %d，期望 0", len(backoffs))
			}
			// 不可重试的错误不套「重试耗尽」的外壳：它的错误码要原样传上去。
			var exhausted *ExhaustedError
			if errors.As(writeErr, &exhausted) {
				t.Errorf("不可重试的错误不该包成 ExhaustedError：%v", writeErr)
			}
			if got := AttemptsOf(writeErr); got != 1 {
				t.Errorf("AttemptsOf = %d，期望 1", got)
			}
		})
	}
}

// TestRetry_CompensationReplayDoesNotDuplicateLedgerRecord 覆盖补偿重放：
// 第一次写成之后，重放发的是同一把幂等键，台账侧认出是同一次越温，
// 返回 409；写侧按「已入账」处理，台账里不会多出第二条记录。
func TestRetry_CompensationReplayDoesNotDuplicateLedgerRecord(t *testing.T) {
	var mu sync.Mutex
	seenKeys := map[string]int{}
	records := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		key, _ := body["idempotency_key"].(string)
		mu.Lock()
		seenKeys[key]++
		if seenKeys[key] == 1 {
			records++
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"dev-001"}`))
			return
		}
		mu.Unlock()
		// 同一把幂等键再来了：台账认出是同一次越温。
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"duplicate external_ref"}`))
	}))
	t.Cleanup(server.Close)

	inner, err := NewDeviationWriter(WritebackConfig{
		BaseURL: server.URL,
		Token:   "test-ledger-token",
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("装配台账写侧失败：%v", err)
	}
	var backoffs []time.Duration
	retrying := NewRetryingDeviationWriter(inner, RetryPolicy{
		MaxRetries: 2,
		BaseDelay:  time.Second,
		MaxDelay:   8 * time.Second,
		Sleep:      noSleep(&backoffs),
	}, nil)

	rec := sampleDeviation()
	// 首次写回。
	if err := retrying.Write(context.Background(), rec); err != nil {
		t.Fatalf("首次写回失败：%v", err)
	}
	// 补偿重放：同一份载荷、同一把幂等键。
	if err := retrying.Write(context.Background(), rec); err != nil {
		t.Fatalf("补偿重放应当按已入账处理，实际失败：%v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if records != 1 {
		t.Errorf("台账侧实际入库的记录数 = %d，期望 1（重放不得产生第二条）", records)
	}
	if seenKeys[rec.IdempotencyKey] != 2 {
		t.Errorf("幂等键 %s 被台账收到的次数 = %d，期望 2（首次 + 重放各一次）",
			rec.IdempotencyKey, seenKeys[rec.IdempotencyKey])
	}
	if len(backoffs) != 0 {
		t.Errorf("退避次数 = %d，期望 0（409 判重不重试）", len(backoffs))
	}
}

// TestRetry_NotificationRetryAndExhaustion 覆盖通知侧的同一套语义：
// 通知发不出去也要退避重试，耗尽后交出尝试次数。
func TestRetry_NotificationRetryAndExhaustion(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)

	inner, err := NewNotifier(NotifyConfig{
		BaseURL: server.URL,
		Token:   "test-notify-token",
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("装配通知客户端失败：%v", err)
	}
	var backoffs []time.Duration
	retrying := NewRetryingNotifier(inner, RetryPolicy{
		MaxRetries: 2,
		BaseDelay:  time.Second,
		MaxDelay:   8 * time.Second,
		Sleep:      noSleep(&backoffs),
	}, nil)

	sendErr := retrying.Send(context.Background(), Message{
		Title:          "冷链温控越温通知",
		Body:           "点位 point-vax-freezer-01 越温",
		RelatedRunID:   "run-test-0001",
		IdempotencyKey: "notify-key-0001",
	})
	if sendErr == nil {
		t.Fatal("通知系统一直不可用时发送应当失败")
	}
	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != 3 {
		t.Fatalf("通知系统收到的请求数 = %d，期望 3（首次 + 2 次退避重试）", gotCalls)
	}
	if got := AttemptsOf(sendErr); got != 3 {
		t.Errorf("AttemptsOf = %d，期望 3", got)
	}
	if code := errcode.As(sendErr).Code; code != errcode.NotifyUnavailable {
		t.Errorf("错误码 = %q，期望 E_NOTIFY_UNAVAILABLE", code)
	}
}

// TestRetryPolicy_BackoffIsExponentialAndCapped 覆盖退避序列本身：
// 指数增长、单次封顶、非法取值回落默认。
func TestRetryPolicy_BackoffIsExponentialAndCapped(t *testing.T) {
	policy := RetryPolicy{MaxRetries: 2, BaseDelay: time.Second, MaxDelay: 8 * time.Second}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second}
	for i, expect := range want {
		got := policy.Backoff(i + 1)
		if got != expect {
			t.Errorf("第 %d 次退避 = %v，期望 %v", i+1, got, expect)
		}
	}
	if got := policy.Backoff(0); got != 0 {
		t.Errorf("第 0 次退避 = %v，期望 0", got)
	}

	normalized := RetryPolicy{MaxRetries: -1, BaseDelay: -1, MaxDelay: -1}.Normalize()
	if normalized.MaxRetries != DefaultMaxRetries {
		t.Errorf("非法重试次数回落为 %d，期望 %d", normalized.MaxRetries, DefaultMaxRetries)
	}
	if normalized.BaseDelay != DefaultRetryBaseDelay {
		t.Errorf("非法退避时长回落为 %v，期望 %v", normalized.BaseDelay, DefaultRetryBaseDelay)
	}
	if normalized.TotalAttempts() != DefaultMaxRetries+1 {
		t.Errorf("总尝试次数 = %d，期望 %d", normalized.TotalAttempts(), DefaultMaxRetries+1)
	}
}

// TestRetryable_ClassifiesOutboundFailures 覆盖重试判定表：
// 只有「对端暂时不可用」才退避重试。
func TestRetryable_ClassifiesOutboundFailures(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"台账不可达可重试", errcode.Newf(errcode.LedgerUnavailable, "超时"), true},
		{"通知不可达可重试", errcode.Newf(errcode.NotifyUnavailable, "超时"), true},
		{"台账协议不合规可重试", errcode.Newf(errcode.LedgerProtocol, "回了怪东西"), true},
		{"凭据被拒不重试", errcode.Newf(errcode.Unauthorized, "401"), false},
		{"参数不合法不重试", errcode.Newf(errcode.Validation, "缺字段"), false},
		{"对象不存在不重试", errcode.Newf(errcode.NotFound, "404"), false},
		{"上下文取消不重试", context.Canceled, false},
		{"空错误不重试", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Retryable(tc.err); got != tc.want {
				t.Errorf("Retryable(%v) = %v，期望 %v", tc.err, got, tc.want)
			}
		})
	}
}
