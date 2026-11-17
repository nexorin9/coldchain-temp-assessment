package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
	"github.com/chaosforge/coldchain-temp-assessment/internal/runtime"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// 本文件验证「确认 → 出站写回」在 HTTP 层的接缝：
// 确认成立之后会不会真的触发写回、回执里能不能看出台账与通知各自的落点。
//
// 两条底线：
//   - 确认成功就该有一次写回：主管点完确认，台账与门诊那边要真的收到东西；
//   - 写回失败不推翻这次确认：结论已经定了，缺的是台账里那一条，
//     回执必须如实说清缺哪一头，而不是笼统一句「失败」。

// stubWritebacker 是出站写回的测试替身：记录被触发的 run，按预设返回结果。
type stubWritebacker struct {
	runIDs  []string
	outcome runtime.WritebackOutcome
	err     error
}

func (s *stubWritebacker) Writeback(_ context.Context, runID string) (runtime.WritebackOutcome, error) {
	s.runIDs = append(s.runIDs, runID)
	return s.outcome, s.err
}

// writebackServer 造一台挂了确认端点与写回能力的 Server。
func writebackServer(t *testing.T, confirmer RunConfirmer, writer RunWritebacker) *Server {
	t.Helper()
	return New(Options{
		Version:     "0.1.0",
		Logger:      logging.New(io.Discard),
		Confirmer:   confirmer,
		Writebacker: writer,
		Credentials: supervisorCreds(),
	})
}

// TestConfirmRun_TriggersWriteback 覆盖主路径：
// 主管确认之后触发一次出站写回，回执带出台账与通知各自的落点。
func TestConfirmRun_TriggersWriteback(t *testing.T) {
	stub := &stubConfirmer{outcome: confirmedOutcome()}
	writer := &stubWritebacker{outcome: runtime.WritebackOutcome{
		RunID:            "run-test-0001",
		ExternalRef:      "cc-point-vax-freezer-01-20261019T191000Z",
		IdempotencyKey:   "54b35c7f136371619ea6a5313714d1ac3",
		LedgerWritten:    true,
		NotificationSent: true,
		WritebackState:   "written_back",
		Note:             "冷链台账越温记录已写入，接种门诊通知已发出",
	}}
	srv := writebackServer(t, stub, writer)

	rec := postConfirm(t, srv, "/v1/runs/run-test-0001/confirm", testSupervisorToken,
		`{"actor":"supervisor-007","disposition":"isolate"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if len(writer.runIDs) != 1 || writer.runIDs[0] != "run-test-0001" {
		t.Fatalf("写回被触发的 run = %v，期望恰好一次且是 run-test-0001", writer.runIDs)
	}

	var body confirmResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析回执失败：%v", err)
	}
	if body.Writeback == nil {
		t.Fatal("回执里应当带出站写回结果")
	}
	if !body.Writeback.LedgerWritten || !body.Writeback.NotificationSent {
		t.Errorf("回执里的写回落点不对：%+v", body.Writeback)
	}
	if body.Writeback.WritebackState != "written_back" {
		t.Errorf("回执里的写回状态 = %q，期望 written_back", body.Writeback.WritebackState)
	}
	if body.Note != "冷链台账越温记录已写入，接种门诊通知已发出" {
		t.Errorf("回执备注 = %q", body.Note)
	}
}

// TestConfirmRun_WritebackFailureDoesNotUndoConfirmation 覆盖失败路径：
// 台账没写进去时确认照样成立，回执如实说明缺哪一头。
func TestConfirmRun_WritebackFailureDoesNotUndoConfirmation(t *testing.T) {
	stub := &stubConfirmer{outcome: confirmedOutcome()}
	writer := &stubWritebacker{outcome: runtime.WritebackOutcome{
		RunID:          "run-test-0001",
		LedgerError:    "E_LEDGER_UNAVAILABLE",
		WritebackState: "pending",
		Note:           "冷链台账写入失败，本次越温尚未入账，挂起待补偿重放",
	}}
	srv := writebackServer(t, stub, writer)

	rec := postConfirm(t, srv, "/v1/runs/run-test-0001/confirm", testSupervisorToken,
		`{"actor":"supervisor-007","disposition":"isolate"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%s", rec.Code, rec.Body.String())
	}

	var body confirmResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析回执失败：%v", err)
	}
	if body.State != "confirmed" {
		t.Errorf("确认状态 = %q，期望 confirmed（写回失败不推翻确认）", body.State)
	}
	if body.Writeback == nil {
		t.Fatal("回执里应当带出站写回结果")
	}
	if body.Writeback.LedgerError != "E_LEDGER_UNAVAILABLE" {
		t.Errorf("回执里的台账错误码 = %q，期望 E_LEDGER_UNAVAILABLE", body.Writeback.LedgerError)
	}
	if body.Writeback.WritebackState != "pending" {
		t.Errorf("回执里的写回状态 = %q，期望 pending（待补偿）", body.Writeback.WritebackState)
	}
}

// TestConfirmRun_WithoutWritebackCapability 覆盖未装配写回能力的场景：
// 确认照样成立，回执里不出现写回结果（没有就是没有，不假装写过）。
func TestConfirmRun_WithoutWritebackCapability(t *testing.T) {
	stub := &stubConfirmer{outcome: confirmedOutcome()}
	srv := writebackServer(t, stub, nil)

	rec := postConfirm(t, srv, "/v1/runs/run-test-0001/confirm", testSupervisorToken,
		`{"actor":"supervisor-007","disposition":"isolate"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var body confirmResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析回执失败：%v", err)
	}
	if body.Writeback != nil {
		t.Errorf("未装配写回能力时不该出现写回结果：%+v", body.Writeback)
	}
}

// TestRejectRun_DoesNotTriggerWriteback 覆盖否决路径：
// 被否决的判定不写台账、不发通知——台账里多一条没发生过的越温更麻烦。
func TestRejectRun_DoesNotTriggerWriteback(t *testing.T) {
	stub := &stubConfirmer{outcome: runtime.ConfirmationOutcome{
		Decision: workflow.ConfirmResult{
			RunID:       "run-test-0001",
			FromState:   workflow.StateAwaitingConfirmation,
			ToState:     workflow.StateRejected,
			ConfirmedBy: "supervisor-007",
			ConfirmedAt: time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC),
			Opinion:     "这条不该这么判",
		},
	}}
	writer := &stubWritebacker{}
	srv := writebackServer(t, stub, writer)

	rec := postConfirm(t, srv, "/v1/runs/run-test-0001/reject", testSupervisorToken,
		`{"actor":"supervisor-007","opinion":"这条不该这么判"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if len(writer.runIDs) != 0 {
		t.Errorf("否决不该触发出站写回，实际触发了 %v", writer.runIDs)
	}
}
