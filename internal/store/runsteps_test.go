package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// openTestStore 开一个内存库供测试用。
func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// testStep 造一条带产物的步骤记录。
func testStep(runID, name string, seq int, attempt int, kind domain.StepPayloadKind, payload any) domain.Step {
	step := domain.Step{
		RunID:    runID,
		StepName: name,
		Seq:      seq,
		Attempt:  attempt,
		State:    domain.StepStateSucceeded,
		StartedAt: time.Date(2026, 10, 20, 3, 10, 0, 0, time.UTC).
			Add(time.Duration(seq) * time.Minute),
	}
	if payload != nil {
		if err := step.SetPayload(kind, payload); err != nil {
			panic("测试数据的产物编码失败：" + err.Error())
		}
	}
	return step
}

// TestInsertRunStep_RoundTripsPayload 验证产物按分码落库并能原样取回。
func TestInsertRunStep_RoundTripsPayload(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	attribution := &domain.AttributionPayload{
		CauseCode:  "power_loss",
		Confidence: 0.6,
		Evidence: []domain.AttributionEvidence{
			{Feature: "device_event:power_loss", Value: 2, Threshold: 1},
		},
	}
	step := testStep("run_test_001", "attribute", 3, 1, domain.PayloadAttribution, attribution)
	ended := step.StartedAt.Add(120 * time.Millisecond)
	step.EndedAt = &ended

	if err := st.InsertRunStep(ctx, step); err != nil {
		t.Fatalf("写入步骤记录失败：%v", err)
	}

	stored, err := st.RunStepsByRun(ctx, "run_test_001")
	if err != nil {
		t.Fatalf("回读步骤记录失败：%v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("应回读到 1 行，实际 %d 行", len(stored))
	}
	got := stored[0]
	if got.RunID != "run_test_001" || got.StepName != "attribute" {
		t.Fatalf("身份字段不符：%+v", got)
	}
	if got.Seq != 3 || got.Attempt != 1 {
		t.Fatalf("次序与尝试次数不符：seq=%d attempt=%d", got.Seq, got.Attempt)
	}
	if got.PayloadKind != domain.PayloadAttribution {
		t.Fatalf("产物分码不符：%q", got.PayloadKind)
	}
	if got.EndedAt == nil || !got.EndedAt.Equal(ended) {
		t.Fatalf("结束时刻未原样取回：%v", got.EndedAt)
	}

	// 产物按分码还原成对应类型。
	typed, err := domain.DecodePayloadAs[domain.AttributionPayload](domain.PayloadAttribution, got.Payload)
	if err != nil {
		t.Fatalf("取回的产物应能还原成 *AttributionPayload：%v", err)
	}
	if typed.CauseCode != "power_loss" || len(typed.Evidence) != 1 {
		t.Fatalf("产物内容不符：%+v", typed)
	}

	// 跨分码取回被拒绝。
	if _, err := domain.DecodePayload(domain.PayloadExceptionRecord, got.Payload); err == nil {
		t.Fatal("用成稿分码取回归因产物应被拒绝")
	}
}

// TestInsertRunStep_SameAttemptUpdatesRow 验证重放同一步骤是更新而不是插第二行：
// 时间线上不能出现两条一模一样的记录。
func TestInsertRunStep_SameAttemptUpdatesRow(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	step := testStep("run_test_002", "attribute", 3, 1, domain.PayloadAttribution,
		&domain.AttributionPayload{CauseCode: "unknown"})
	if err := st.InsertRunStep(ctx, step); err != nil {
		t.Fatalf("首次写入失败：%v", err)
	}

	step.State = domain.StepStateFailed
	step.ErrorCode = string(errcode.ModelUnavailable)
	step.Error = "E_MODEL_UNAVAILABLE: 模型服务不可达"
	if err := st.InsertRunStep(ctx, step); err != nil {
		t.Fatalf("重放写入失败：%v", err)
	}

	count, err := st.CountRunSteps(ctx, "run_test_002")
	if err != nil {
		t.Fatalf("统计步骤记录失败：%v", err)
	}
	if count != 1 {
		t.Fatalf("同一步骤重放后应仍为 1 行，实际 %d 行", count)
	}

	stored, err := st.RunStepsByRun(ctx, "run_test_002")
	if err != nil {
		t.Fatalf("回读步骤记录失败：%v", err)
	}
	if stored[0].State != domain.StepStateFailed {
		t.Fatalf("重放后状态应被更新为 failed，实际 %q", stored[0].State)
	}
	if stored[0].ErrorCode != string(errcode.ModelUnavailable) {
		t.Fatalf("重放后错误码不符：%q", stored[0].ErrorCode)
	}
}

// TestInsertRunStep_NextAttemptKeepsBothRows 验证换一次尝试是新增一行：
// 两次尝试的记录都要留着，排障时要看第几次失败在哪。
func TestInsertRunStep_NextAttemptKeepsBothRows(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	for attempt := 1; attempt <= 2; attempt++ {
		step := testStep("run_test_003", "attribute", 3, attempt, domain.PayloadAttribution,
			&domain.AttributionPayload{CauseCode: "unknown"})
		if attempt == 1 {
			step.State = domain.StepStateFailed
			step.ErrorCode = string(errcode.ModelTimeout)
			step.Error = "E_MODEL_TIMEOUT: 模型调用超时"
		}
		if err := st.InsertRunStep(ctx, step); err != nil {
			t.Fatalf("第 %d 次写入失败：%v", attempt, err)
		}
	}

	count, err := st.CountRunSteps(ctx, "run_test_003")
	if err != nil {
		t.Fatalf("统计步骤记录失败：%v", err)
	}
	if count != 2 {
		t.Fatalf("两次尝试应留 2 行，实际 %d 行", count)
	}

	stored, err := st.RunStepsByRun(ctx, "run_test_003")
	if err != nil {
		t.Fatalf("回读步骤记录失败：%v", err)
	}
	if stored[0].Attempt != 1 || stored[1].Attempt != 2 {
		t.Fatalf("回读应按尝试次数升序：%d、%d", stored[0].Attempt, stored[1].Attempt)
	}
	if stored[0].State != domain.StepStateFailed {
		t.Fatalf("第一次尝试应记为 failed，实际 %q", stored[0].State)
	}
	if stored[1].State != domain.StepStateSucceeded {
		t.Fatalf("第二次尝试应记为 succeeded，实际 %q", stored[1].State)
	}
}

// TestRunStepsByRun_OrderedByStepSequence 验证回读按链上次序排列。
func TestRunStepsByRun_OrderedByStepSequence(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	names := []string{"fetch_series", "extract_window", "attribute", "draft", "await_confirmation"}
	// 倒着写进去：回读时必须按 seq 排回正序。
	for i := len(names) - 1; i >= 0; i-- {
		step := testStep("run_test_004", names[i], i+1, 1, domain.PayloadSeries,
			&domain.SeriesPayload{PointID: "point-vax-freezer-01", SampleCount: 3})
		if err := st.InsertRunStep(ctx, step); err != nil {
			t.Fatalf("写入步骤记录失败：%v", err)
		}
	}

	stored, err := st.RunStepsByRun(ctx, "run_test_004")
	if err != nil {
		t.Fatalf("回读步骤记录失败：%v", err)
	}
	if len(stored) != len(names) {
		t.Fatalf("应回读到 %d 行，实际 %d 行", len(names), len(stored))
	}
	for i, step := range stored {
		if step.StepName != names[i] {
			t.Fatalf("第 %d 行应为 %q，实际 %q", i+1, names[i], step.StepName)
		}
		if step.Seq != i+1 {
			t.Fatalf("第 %d 行的 seq 应为 %d，实际 %d", i+1, i+1, step.Seq)
		}
	}
}

// TestInsertRunStep_RejectsMissingIdentity 验证缺身份字段的记录不入库。
func TestInsertRunStep_RejectsMissingIdentity(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	noRun := testStep("", "attribute", 1, 1, domain.PayloadSeries, &domain.SeriesPayload{})
	if err := st.InsertRunStep(ctx, noRun); err == nil {
		t.Fatal("缺 run_id 的步骤记录不应入库")
	} else if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.Validation, code)
	}

	noName := testStep("run_test_005", "", 1, 1, domain.PayloadSeries, &domain.SeriesPayload{})
	if err := st.InsertRunStep(ctx, noName); err == nil {
		t.Fatal("缺 step_name 的步骤记录不应入库")
	} else if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.Validation, code)
	}

	count, err := st.CountRunSteps(ctx, "run_test_005")
	if err != nil {
		t.Fatalf("统计步骤记录失败：%v", err)
	}
	if count != 0 {
		t.Fatalf("不合法的记录不应入库，实际 %d 行", count)
	}
}

// TestRunStepsByRun_RejectsMissingRunID 验证回读必须带 run_id。
func TestRunStepsByRun_RejectsMissingRunID(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	if _, err := st.RunStepsByRun(ctx, "  "); err == nil {
		t.Fatal("缺 run_id 的回读应被拒绝")
	} else if code := errcode.As(err).Code; code != errcode.Validation {
		t.Fatalf("错误码应为 %s，实际 %s", errcode.Validation, code)
	}
	if _, err := st.CountRunSteps(ctx, ""); err == nil {
		t.Fatal("缺 run_id 的统计应被拒绝")
	}
}

// TestInsertRunStep_RoundTripsBackoffMS 验证退避时长落库并能原样取回：
// 失败那一次尝试等了多少秒，时间线上要看得见。
func TestInsertRunStep_RoundTripsBackoffMS(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	failed := testStep("run_test_backoff", "attribute", 3, 1, domain.PayloadSeries, nil)
	failed.State = domain.StepStateFailed
	failed.ErrorCode = string(errcode.ModelUnavailable)
	failed.Error = "模型服务不可达"
	failed.BackoffMS = 2000
	if err := st.InsertRunStep(ctx, failed); err != nil {
		t.Fatalf("写入失败尝试记录失败：%v", err)
	}

	succeeded := testStep("run_test_backoff", "attribute", 3, 2, domain.PayloadSeries, nil)
	succeeded.State = domain.StepStateSucceeded
	succeeded.BackoffMS = 0
	if err := st.InsertRunStep(ctx, succeeded); err != nil {
		t.Fatalf("写入成功尝试记录失败：%v", err)
	}

	stored, err := st.RunStepsByRun(ctx, "run_test_backoff")
	if err != nil {
		t.Fatalf("回读步骤记录失败：%v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("应回读到 2 条尝试记录，实际 %d 条", len(stored))
	}
	if stored[0].Attempt != 1 || stored[0].BackoffMS != 2000 {
		t.Fatalf("第 1 次尝试应带退避 2000ms，实际 attempt=%d backoff_ms=%d",
			stored[0].Attempt, stored[0].BackoffMS)
	}
	if stored[1].Attempt != 2 || stored[1].BackoffMS != 0 {
		t.Fatalf("第 2 次尝试不应带退避时长，实际 attempt=%d backoff_ms=%d",
			stored[1].Attempt, stored[1].BackoffMS)
	}
}

// TestOpen_MigratesLegacyRunStepsWithoutBackoffMS 验证存量库补列：
// 老库里已经跑过的判定不该因为加一列退避时长就作废——
// 补列后老数据仍在，新写入的退避时长也落得下。
func TestOpen_MigratesLegacyRunStepsWithoutBackoffMS(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")

	// 造一个「没有 backoff_ms 列」的老库，并塞一条老记录。
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开老库失败：%v", err)
	}
	if _, err := legacy.ExecContext(ctx, `
		CREATE TABLE run_steps (
		    run_id       TEXT NOT NULL,
		    step_name    TEXT NOT NULL,
		    attempt      INTEGER NOT NULL DEFAULT 1,
		    seq          INTEGER NOT NULL DEFAULT 0,
		    state        TEXT NOT NULL,
		    payload_kind TEXT NOT NULL DEFAULT '',
		    payload      TEXT NOT NULL DEFAULT '',
		    started_at   TEXT NOT NULL,
		    ended_at     TEXT,
		    error        TEXT NOT NULL DEFAULT '',
		    error_code   TEXT NOT NULL DEFAULT '',
		    PRIMARY KEY (run_id, step_name, attempt)
		)`); err != nil {
		t.Fatalf("造老库失败：%v", err)
	}
	if _, err := legacy.ExecContext(ctx,
		"INSERT INTO run_steps (run_id, step_name, attempt, seq, state, started_at) "+
			"VALUES ('run_legacy', 'fetch_series', 1, 1, 'succeeded', "+
			"'2026-10-20T03:10:00.000000000Z')"); err != nil {
		t.Fatalf("写老记录失败：%v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("关闭老库失败：%v", err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("打开存量库失败：%v", err)
	}
	defer func() { _ = st.Close() }()

	stored, err := st.RunStepsByRun(ctx, "run_legacy")
	if err != nil {
		t.Fatalf("回读存量步骤记录失败：%v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("老记录应保留 1 行，实际 %d 行", len(stored))
	}
	if stored[0].StepName != "fetch_series" || stored[0].Attempt != 1 {
		t.Fatalf("老记录内容不符：step=%q attempt=%d", stored[0].StepName, stored[0].Attempt)
	}
	if stored[0].BackoffMS != 0 {
		t.Fatalf("老记录没有退避时长，应为 0，实际 %d", stored[0].BackoffMS)
	}

	// 补列后新写入的退避时长落得下。
	fresh := testStep("run_legacy", "attribute", 3, 1, domain.PayloadSeries, nil)
	fresh.State = domain.StepStateFailed
	fresh.BackoffMS = 1500
	if err := st.InsertRunStep(ctx, fresh); err != nil {
		t.Fatalf("补列后写入失败尝试记录失败：%v", err)
	}
	stored, err = st.RunStepsByRun(ctx, "run_legacy")
	if err != nil {
		t.Fatalf("回读步骤记录失败：%v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("应回读到 2 条记录，实际 %d 条", len(stored))
	}
	if stored[1].BackoffMS != 1500 {
		t.Fatalf("新写入的退避时长应为 1500ms，实际 %d", stored[1].BackoffMS)
	}
}
