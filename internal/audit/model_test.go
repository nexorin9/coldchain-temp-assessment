package audit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// memSink 是一个把审计条目收在内存里的落库替身。
//
// 它只实现 Sink 的那一个方法：审计层不碰数据库，测试因此能不落库就验审计。
type memSink struct {
	entries []domain.AuditEntry
	err     error
}

func (m *memSink) RecordAuditEntry(_ context.Context, entry domain.AuditEntry) error {
	if m.err != nil {
		return m.err
	}
	m.entries = append(m.entries, entry)
	return nil
}

// last 返回最后一条审计条目。
func (m *memSink) last(t *testing.T) domain.AuditEntry {
	t.Helper()
	if len(m.entries) == 0 {
		t.Fatalf("审计记录器没有落任何条目")
	}
	return m.entries[len(m.entries)-1]
}

// fixedClock 返回一个固定时刻的时钟。
func fixedClock() func() time.Time {
	at := time.Date(2026, 10, 26, 2, 30, 0, 0, time.UTC)
	return func() time.Time { return at }
}

// TestRecorder_ModelCallMetadataIsComplete 验证一次模型调用的审计元数据字段齐全。
//
// 内审要回答的是「用了哪个模型、花了多少 token、算下来多少钱、成没成」——
// 这几个字段缺一个，那条记录就答不了这个问题。
func TestRecorder_ModelCallMetadataIsComplete(t *testing.T) {
	sink := &memSink{}
	rec := NewRecorder(sink, WithPricer(NewPriceTable(map[string]Price{
		"院内判定模型": {InputCostPerToken: 0.0000012, OutputCostPerToken: 0.0000024},
	})), WithClock(fixedClock()))

	err := rec.RecordModelCall(context.Background(), ModelCall{
		RunID:            "run-abc123",
		StepName:         "attribute",
		SchemaName:       "attribution",
		Model:            "院内判定模型",
		Outcome:          domain.AuditOutcomeSucceeded,
		LatencyMS:        812,
		PromptTokens:     900,
		CompletionTokens: 120,
		TotalTokens:      1020,
		PromptDigest:     Digest("{\"point_id\":\"point-vax-freezer-01\"}"),
		PromptChars:      35,
	})
	if err != nil {
		t.Fatalf("记录模型调用失败：%v", err)
	}

	entry := sink.last(t)
	if entry.RunID != "run-abc123" {
		t.Errorf("run_id = %q，期望 run-abc123", entry.RunID)
	}
	if entry.EntryType != domain.AuditEntryModelCall {
		t.Errorf("entry_type = %q，期望 %q", entry.EntryType, domain.AuditEntryModelCall)
	}
	if entry.StepName != "attribute" {
		t.Errorf("step_name = %q，期望 attribute", entry.StepName)
	}
	if entry.SchemaName != "attribution" {
		t.Errorf("schema_name = %q，期望 attribution", entry.SchemaName)
	}
	if entry.Model != "院内判定模型" {
		t.Errorf("model = %q，期望 院内判定模型", entry.Model)
	}
	if entry.Outcome != domain.AuditOutcomeSucceeded {
		t.Errorf("outcome = %q，期望 %q", entry.Outcome, domain.AuditOutcomeSucceeded)
	}
	if entry.LatencyMS != 812 {
		t.Errorf("latency_ms = %d，期望 812", entry.LatencyMS)
	}
	if entry.PromptTokens != 900 || entry.CompletionTokens != 120 || entry.TotalTokens != 1020 {
		t.Errorf("token 字段 = %d/%d/%d，期望 900/120/1020",
			entry.PromptTokens, entry.CompletionTokens, entry.TotalTokens)
	}
	if entry.CostStatus != domain.CostStatusKnown {
		t.Errorf("cost_status = %q，期望 %q", entry.CostStatus, domain.CostStatusKnown)
	}
	if entry.CostAmount == nil {
		t.Fatal("cost_amount 为空：成本算得出来时必须有金额")
	}
	if entry.CostCurrency != DefaultCurrency {
		t.Errorf("cost_currency = %q，期望 %q", entry.CostCurrency, DefaultCurrency)
	}
	if entry.PromptDigest == "" {
		t.Error("prompt_digest 为空：审计链要能回答「是不是同一段提示词」")
	}
	if entry.PromptChars != 35 {
		t.Errorf("prompt_chars = %d，期望 35", entry.PromptChars)
	}
	if !entry.CreatedAt.Equal(time.Date(2026, 10, 26, 2, 30, 0, 0, time.UTC)) {
		t.Errorf("created_at = %v，期望固定时钟给的时刻", entry.CreatedAt)
	}
}

// TestRecorder_TotalTokensFilledWhenEndpointOmitsIt 验证端点没回报总数时按两者补齐。
//
// 总数为 0 会让汇总里「这次调用花了多少」看着像没花钱。
func TestRecorder_TotalTokensFilledWhenEndpointOmitsIt(t *testing.T) {
	sink := &memSink{}
	rec := NewRecorder(sink)

	err := rec.RecordModelCall(context.Background(), ModelCall{
		RunID:            "run-abc123",
		StepName:         "attribute",
		Model:            "院内判定模型",
		PromptTokens:     300,
		CompletionTokens: 40,
	})
	if err != nil {
		t.Fatalf("记录模型调用失败：%v", err)
	}
	if got := sink.last(t).TotalTokens; got != 340 {
		t.Errorf("total_tokens = %d，期望 340（按输入 + 输出补齐）", got)
	}
}

// TestRecorder_ConfirmationJoinsTheSameChain 验证人工确认动作落在同一条审计链上。
//
// 内审要看的正是「机器判了什么、人认了什么」这两段的接缝——
// 两类记录分开存，审计时就得跨表拼。
func TestRecorder_ConfirmationJoinsTheSameChain(t *testing.T) {
	sink := &memSink{}
	rec := NewRecorder(sink, WithClock(fixedClock()))

	if err := rec.RecordModelCall(context.Background(), ModelCall{
		RunID: "run-abc123", StepName: "attribute", Model: "院内判定模型",
	}); err != nil {
		t.Fatalf("记录模型调用失败：%v", err)
	}
	if err := rec.RecordConfirmation(context.Background(), Confirmation{
		RunID:     "run-abc123",
		Actor:     "supervisor-007",
		Action:    "confirm",
		FromState: "awaiting_confirmation",
		ToState:   "confirmed",
		Reason:    "现场复核箱内温度后确认处置结论",
	}); err != nil {
		t.Fatalf("记录确认动作失败：%v", err)
	}

	if len(sink.entries) != 2 {
		t.Fatalf("审计条目数 = %d，期望 2", len(sink.entries))
	}
	entry := sink.last(t)
	if entry.EntryType != domain.AuditEntryConfirmation {
		t.Errorf("entry_type = %q，期望 %q", entry.EntryType, domain.AuditEntryConfirmation)
	}
	if entry.Actor != "supervisor-007" || entry.Action != "confirm" {
		t.Errorf("actor/action = %q/%q，期望 supervisor-007/confirm", entry.Actor, entry.Action)
	}
	if entry.FromState != "awaiting_confirmation" || entry.ToState != "confirmed" {
		t.Errorf("状态迁移 = %q→%q，期望 awaiting_confirmation→confirmed",
			entry.FromState, entry.ToState)
	}
	if entry.Details == "" {
		t.Error("details 为空：确认理由要留档")
	}
}

// TestRecorder_MissingRunIDRejected 验证没有 run 标识的调用不入审计链。
//
// 审计链是挂在某条判定上的：认不出 run 的调用无处可挂，
// 与其记一条挂不上的账，不如明确拒收。
func TestRecorder_MissingRunIDRejected(t *testing.T) {
	sink := &memSink{}
	rec := NewRecorder(sink)

	err := rec.RecordModelCall(context.Background(), ModelCall{StepName: "attribute"})
	if err == nil {
		t.Fatal("缺 run_id 的模型调用应被拒收")
	}
	if code := errcode.As(err).Code; code != errcode.Validation {
		t.Errorf("错误码 = %q，期望 %q", code, errcode.Validation)
	}
	if len(sink.entries) != 0 {
		t.Errorf("审计条目数 = %d，期望 0（被拒收的调用不入库）", len(sink.entries))
	}
}

// TestRecorder_CostCalculatedFromPerTokenPrice 验证成本按每 token 单价算对。
func TestRecorder_CostCalculatedFromPerTokenPrice(t *testing.T) {
	sink := &memSink{}
	rec := NewRecorder(sink, WithPricer(NewPriceTable(map[string]Price{
		"院内判定模型": {InputCostPerToken: 0.0000012, OutputCostPerToken: 0.0000024},
	})))

	err := rec.RecordModelCall(context.Background(), ModelCall{
		RunID:            "run-abc123",
		StepName:         "attribute",
		Model:            "院内判定模型",
		PromptTokens:     1000,
		CompletionTokens: 500,
	})
	if err != nil {
		t.Fatalf("记录模型调用失败：%v", err)
	}

	entry := sink.last(t)
	// 1000 × 0.0000012 + 500 × 0.0000024 = 0.0012 + 0.0012 = 0.0024
	want := 0.0024
	if entry.CostAmount == nil {
		t.Fatal("cost_amount 为空")
	}
	if diff := *entry.CostAmount - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("cost_amount = %v，期望 %v", *entry.CostAmount, want)
	}
}

// TestRecorder_UnknownModelGivesUnknownNotZero 验证单价表里没有这个模型时成本记 unknown。
//
// 0 会被读成「这次调用不花钱」——那是结论，不是事实。
// 事实是「本服务不知道这个模型的单价」。
func TestRecorder_UnknownModelGivesUnknownNotZero(t *testing.T) {
	sink := &memSink{}
	rec := NewRecorder(sink, WithPricer(NewPriceTable(map[string]Price{
		"别的模型": {InputCostPerToken: 0.0000012, OutputCostPerToken: 0.0000024},
	})))

	err := rec.RecordModelCall(context.Background(), ModelCall{
		RunID:            "run-abc123",
		StepName:         "attribute",
		Model:            "院内判定模型",
		PromptTokens:     1000,
		CompletionTokens: 500,
	})
	if err != nil {
		t.Fatalf("记录模型调用失败：%v", err)
	}

	entry := sink.last(t)
	if entry.CostStatus != domain.CostStatusUnknown {
		t.Errorf("cost_status = %q，期望 %q", entry.CostStatus, domain.CostStatusUnknown)
	}
	if entry.CostAmount != nil {
		t.Errorf("cost_amount = %v，期望为空（算不出就不是 0）", *entry.CostAmount)
	}
}

// TestRecorder_NoPriceTableGivesUnknownNotZero 验证没配单价表时成本一律记 unknown。
func TestRecorder_NoPriceTableGivesUnknownNotZero(t *testing.T) {
	sink := &memSink{}
	rec := NewRecorder(sink)

	err := rec.RecordModelCall(context.Background(), ModelCall{
		RunID: "run-abc123", StepName: "attribute", Model: "院内判定模型",
		PromptTokens: 1000, CompletionTokens: 500,
	})
	if err != nil {
		t.Fatalf("记录模型调用失败：%v", err)
	}
	entry := sink.last(t)
	if entry.CostStatus != domain.CostStatusUnknown {
		t.Errorf("cost_status = %q，期望 %q", entry.CostStatus, domain.CostStatusUnknown)
	}
	if entry.CostAmount != nil {
		t.Errorf("cost_amount = %v，期望为空", *entry.CostAmount)
	}
}

// TestPriceTable_ZeroPricedEntryDropped 验证零成本占位条目被剔除。
//
// 上游价目快照里「输入输出两项成本都是 0」的条目几乎总是没填的占位行，
// 不是真的免费模型。把它当成免费模型，一次真实调用的成本就悄悄记成 0。
func TestPriceTable_ZeroPricedEntryDropped(t *testing.T) {
	table := NewPriceTable(map[string]Price{
		"真模型":  {InputCostPerToken: 0.0000012, OutputCostPerToken: 0.0000024},
		"占位空壳": {InputCostPerToken: 0, OutputCostPerToken: 0},
	})
	if table.Len() != 1 {
		t.Fatalf("单价表条目数 = %d，期望 1（零成本占位条目应被剔除）", table.Len())
	}
	if _, ok := table.PriceFor("占位空壳"); ok {
		t.Error("零成本占位条目仍在表里：它会让真实调用的成本悄悄记成 0")
	}
	if _, ok := table.PriceFor("真模型"); !ok {
		t.Error("真模型不在表里")
	}
}

// TestPriceTable_DocumentationPlaceholderDropped 验证文档占位键被显式剔除。
func TestPriceTable_DocumentationPlaceholderDropped(t *testing.T) {
	table := NewPriceTable(map[string]Price{
		documentationPlaceholderKey: {InputCostPerToken: 0, OutputCostPerToken: 0},
		"真模型":                       {InputCostPerToken: 0.0000012, OutputCostPerToken: 0.0000024},
	})
	if _, ok := table.PriceFor(documentationPlaceholderKey); ok {
		t.Errorf("文档占位键 %q 仍在表里", documentationPlaceholderKey)
	}
	if table.Len() != 1 {
		t.Errorf("单价表条目数 = %d，期望 1", table.Len())
	}
}

// TestPriceTable_NegativePriceRejected 验证负单价直接报错。
//
// 负价不是「便宜」，是这份表填错了。
func TestPriceTable_NegativePriceRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prices.json")
	body := `{"坏模型": {"input_cost_per_token": -1, "output_cost_per_token": 0.0000024}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写单价表失败：%v", err)
	}
	_, err := LoadPriceTable(path)
	if err == nil {
		t.Fatal("负单价应被拒收")
	}
	if code := errcode.As(err).Code; code != errcode.ConfigMissing {
		t.Errorf("错误码 = %q，期望 %q", code, errcode.ConfigMissing)
	}
	if !strings.Contains(err.Error(), "坏模型") {
		t.Errorf("错误信息 = %q，期望点名出问题的模型名", err.Error())
	}
}

// TestPriceTable_LoadedFromFile 验证单价表能从文件装载并算出成本。
func TestPriceTable_LoadedFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prices.json")
	body := `{"院内判定模型": {"input_cost_per_token": 0.0000012, "output_cost_per_token": 0.0000024, "currency": "CNY"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写单价表失败：%v", err)
	}

	table, err := LoadPriceTable(path)
	if err != nil {
		t.Fatalf("装载单价表失败：%v", err)
	}
	pricer := NewTablePricer(table)
	cost := pricer.Estimate("院内判定模型", 1000, 500)
	if !cost.Known {
		t.Fatal("成本应算得出来")
	}
	if diff := cost.Amount - 0.0024; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("成本 = %v，期望 0.0024", cost.Amount)
	}
	if cost.Currency != "CNY" {
		t.Errorf("币种 = %q，期望 CNY", cost.Currency)
	}
}

// TestPriceTable_MissingFileAndMalformedRejected 验证单价表读不到或格式不对时报错。
func TestPriceTable_MissingFileAndMalformedRejected(t *testing.T) {
	if _, err := LoadPriceTable(filepath.Join(t.TempDir(), "没有这个文件.json")); err == nil {
		t.Error("单价表文件不存在时应报错")
	}
	if _, err := LoadPriceTable("   "); err == nil {
		t.Error("单价表路径为空时应报错")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("{不是 JSON"), 0o600); err != nil {
		t.Fatalf("写单价表失败：%v", err)
	}
	if _, err := LoadPriceTable(path); err == nil {
		t.Error("单价表格式非法时应报错")
	}
}

// TestRecorder_RedactsSensitiveFragments 验证凭据与鉴权头被抹掉。
//
// 审计记录会被内审与飞检翻出来给人看，也可能被导成 CSV 往外发——
// 里面出现一把能直接调模型的 key，等于把钥匙和锁一起寄出去。
func TestRecorder_RedactsSensitiveFragments(t *testing.T) {
	const apiKey = "sk-hospital-secret-0123456789"
	sink := &memSink{}
	rec := NewRecorder(sink, WithSecrets(apiKey))

	err := rec.RecordModelCall(context.Background(), ModelCall{
		RunID:    "run-abc123",
		StepName: "attribute",
		Model:    "院内判定模型",
		Details: "端点返回 Authorization: Bearer sk-hospital-secret-0123456789，" +
			"本次调用用 key sk-hospital-secret-0123456789 认证",
	})
	if err != nil {
		t.Fatalf("记录模型调用失败：%v", err)
	}

	details := sink.last(t).Details
	if strings.Contains(details, apiKey) {
		t.Errorf("details 里仍含 api_key 原文：%q", details)
	}
	if !strings.Contains(details, redactedPlaceholder) {
		t.Errorf("details = %q，期望被抹掉的片段留下占位符", details)
	}
}

// TestRecorder_RedactHelper 验证脱敏函数单独可用。
func TestRecorder_RedactHelper(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		secrets []string
	}{
		{
			name:    "登记的凭据原文",
			text:    "调用用的 key 是 sk-secret-123",
			secrets: []string{"sk-secret-123"},
		},
		{
			name: "鉴权头",
			text: "Authorization: Bearer abcdef123456",
		},
		{
			name: "Bearer 凭据",
			text: "bearer abcdef123456",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Redact(tc.text, tc.secrets)
			if strings.Contains(got, "sk-secret-123") ||
				strings.Contains(got, "abcdef123456") {
				t.Errorf("脱敏后仍含凭据原文：%q", got)
			}
		})
	}
	if got := Redact("", nil); got != "" {
		t.Errorf("空文本脱敏后 = %q，期望空串", got)
	}
}

// TestRecorder_PromptRawTextNeverStored 验证提示词原文不进审计记录。
//
// 提示词带着现场点位、批次与读数上下文，而审计表会被导成 CSV 往外发。
// 审计链只留指纹与长度。
func TestRecorder_PromptRawTextNeverStored(t *testing.T) {
	const rawPrompt = `{"point_id":"point-vax-freezer-01","samples":[{"temperature_c":12.4}]}`
	sink := &memSink{}
	rec := NewRecorder(sink)

	err := rec.RecordModelCall(context.Background(), ModelCall{
		RunID:        "run-abc123",
		StepName:     "attribute",
		Model:        "院内判定模型",
		PromptDigest: Digest(rawPrompt),
		PromptChars:  len([]rune(rawPrompt)),
	})
	if err != nil {
		t.Fatalf("记录模型调用失败：%v", err)
	}

	entry := sink.last(t)
	if entry.PromptDigest != Digest(rawPrompt) {
		t.Errorf("prompt_digest = %q，期望 %q", entry.PromptDigest, Digest(rawPrompt))
	}
	if entry.PromptChars != len([]rune(rawPrompt)) {
		t.Errorf("prompt_chars = %d，期望 %d", entry.PromptChars, len([]rune(rawPrompt)))
	}

	// 整条记录序列化后不得出现提示词原文的任何片段。
	blob, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("序列化审计记录失败：%v", err)
	}
	for _, fragment := range []string{"point-vax-freezer-01", "12.4", "samples"} {
		if strings.Contains(string(blob), fragment) {
			t.Errorf("审计记录里出现了提示词片段 %q：%s", fragment, string(blob))
		}
	}
}

// TestDigest_StableAndDistinct 验证指纹稳定且不同提示词算出不同指纹。
func TestDigest_StableAndDistinct(t *testing.T) {
	a := Digest("提示词一")
	if a != Digest("提示词一") {
		t.Error("同一段提示词应算出同一个指纹")
	}
	if a == Digest("提示词二") {
		t.Error("不同提示词不应算出同一个指纹")
	}
	if len(a) != 16 {
		t.Errorf("指纹长度 = %d，期望 16", len(a))
	}
	if Digest("") != "" {
		t.Error("空提示词的指纹应为空串")
	}
}
