package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// testFactCheck 造一条事实校验记录。
func testFactCheck(runID, step string, attempt int, verdict string, issues []domain.FactIssue) domain.FactCheckRecord {
	return domain.FactCheckRecord{
		RunID:       runID,
		StepName:    step,
		SchemaName:  "attribution",
		Attempt:     attempt,
		Verdict:     verdict,
		Issues:      issues,
		RawFragment: `{"cause_code":"door_open","confidence":0.9}`,
		CreatedAt:   time.Date(2026, 10, 20, 3, 10, 0, 0, time.UTC).Add(time.Duration(attempt) * time.Minute),
	}
}

// TestRecordFactCheck_RoundTripsIssues 验证校验记录落库后能原样取回：
// 被拦下的字段、原值与依据一个都不能少。
func TestRecordFactCheck_RoundTripsIssues(t *testing.T) {
	st := openTestStore(t)
	record := testFactCheck("run-fact-001", "attribute", 1, domain.FactVerdictMismatch, []domain.FactIssue{{
		Path:    "peak_c",
		Kind:    domain.FactKindTemperatureNotFound,
		Claimed: "13.70℃",
		Basis:   "输入时序 7 条读数里没有这个温度；最接近的一条是 12.40℃",
		Reason:  "字段 peak_c 的温度 13.70℃ 在输入时序里找不到",
	}})

	if err := st.RecordFactCheck(context.Background(), record); err != nil {
		t.Fatalf("写入事实校验记录失败：%v", err)
	}

	records, err := st.FactChecksByRun(context.Background(), "run-fact-001")
	if err != nil {
		t.Fatalf("回读事实校验记录失败：%v", err)
	}
	if len(records) != 1 {
		t.Fatalf("应回读到 1 条记录，实际 %d 条", len(records))
	}
	got := records[0]
	if got.Verdict != domain.FactVerdictMismatch {
		t.Fatalf("校验结论应为 %q，实际 %q", domain.FactVerdictMismatch, got.Verdict)
	}
	if got.RunID != record.RunID || got.StepName != record.StepName ||
		got.SchemaName != record.SchemaName || got.Attempt != record.Attempt {
		t.Fatalf("记录身份字段应原样往返，实际 %+v", got)
	}
	if len(got.Issues) != 1 {
		t.Fatalf("应回读到 1 条问题，实际 %d 条", len(got.Issues))
	}
	issue := got.Issues[0]
	if issue.Path != "peak_c" || issue.Claimed != "13.70℃" ||
		issue.Kind != domain.FactKindTemperatureNotFound ||
		issue.Basis == "" || issue.Reason == "" {
		t.Fatalf("问题清单应原样往返，实际 %+v", issue)
	}
	if got.RawFragment == "" {
		t.Fatal("模型原文片段应原样保留")
	}
	if !got.CreatedAt.Equal(record.CreatedAt) {
		t.Fatalf("记录时刻应原样往返，期望 %s，实际 %s",
			record.CreatedAt.Format(time.RFC3339), got.CreatedAt.Format(time.RFC3339))
	}
}

// TestRecordFactCheck_SameAttemptUpdatesRow 验证同一次尝试被重放时更新那一行：
// 重放是常态（进程重启后续跑），插两行会让「问了几次」对不上。
func TestRecordFactCheck_SameAttemptUpdatesRow(t *testing.T) {
	st := openTestStore(t)
	record := testFactCheck("run-fact-002", "attribute", 1, domain.FactVerdictMismatch, []domain.FactIssue{{
		Path: "peak_c", Kind: domain.FactKindTemperatureNotFound, Claimed: "13.70℃",
	}})
	if err := st.RecordFactCheck(context.Background(), record); err != nil {
		t.Fatalf("首次写入失败：%v", err)
	}
	record.Verdict = domain.FactVerdictPass
	record.Issues = nil
	if err := st.RecordFactCheck(context.Background(), record); err != nil {
		t.Fatalf("重放写入失败：%v", err)
	}

	count, err := st.CountFactChecks(context.Background(), "run-fact-002")
	if err != nil {
		t.Fatalf("统计事实校验记录失败：%v", err)
	}
	if count != 1 {
		t.Fatalf("重放后应仍只有 1 行，实际 %d 行", count)
	}
	records, err := st.FactChecksByRun(context.Background(), "run-fact-002")
	if err != nil {
		t.Fatalf("回读事实校验记录失败：%v", err)
	}
	if records[0].Verdict != domain.FactVerdictPass {
		t.Fatalf("重放后结论应被更新为 %q，实际 %q", domain.FactVerdictPass, records[0].Verdict)
	}
	if len(records[0].Issues) != 0 {
		t.Fatalf("重放后问题清单应被清空，实际 %d 条", len(records[0].Issues))
	}
}

// TestFactChecksByRun_KeepsBothAttempts 验证首次与重问各留一行：
// 「问了几次、哪一次被拦」是事后审计最常问的两件事。
func TestFactChecksByRun_KeepsBothAttempts(t *testing.T) {
	st := openTestStore(t)
	first := testFactCheck("run-fact-003", "attribute", 1, domain.FactVerdictMismatch, []domain.FactIssue{{
		Path: "peak_c", Kind: domain.FactKindTemperatureNotFound, Claimed: "13.70℃",
	}})
	second := testFactCheck("run-fact-003", "attribute", 2, domain.FactVerdictPass, nil)
	if err := st.RecordFactCheck(context.Background(), first); err != nil {
		t.Fatalf("写入首次记录失败：%v", err)
	}
	if err := st.RecordFactCheck(context.Background(), second); err != nil {
		t.Fatalf("写入重问记录失败：%v", err)
	}

	records, err := st.FactChecksByRun(context.Background(), "run-fact-003")
	if err != nil {
		t.Fatalf("回读事实校验记录失败：%v", err)
	}
	if len(records) != 2 {
		t.Fatalf("应回读到 2 条记录，实际 %d 条", len(records))
	}
	if records[0].Attempt != 1 || records[1].Attempt != 2 {
		t.Fatalf("记录应按尝试序号升序，实际 %d、%d", records[0].Attempt, records[1].Attempt)
	}
	if records[0].Verdict != domain.FactVerdictMismatch || records[1].Verdict != domain.FactVerdictPass {
		t.Fatalf("两次结论应分别是 %q 与 %q，实际 %q、%q",
			domain.FactVerdictMismatch, domain.FactVerdictPass,
			records[0].Verdict, records[1].Verdict)
	}
}

// TestRecordFactCheck_RejectsMissingIdentity 验证缺身份的记录不入库：
// 挂不上 run 的校验记录等于没记。
func TestRecordFactCheck_RejectsMissingIdentity(t *testing.T) {
	st := openTestStore(t)
	for name, mutate := range map[string]func(domain.FactCheckRecord) domain.FactCheckRecord{
		"缺 run_id": func(r domain.FactCheckRecord) domain.FactCheckRecord {
			r.RunID = ""
			return r
		},
		"缺 step_name": func(r domain.FactCheckRecord) domain.FactCheckRecord {
			r.StepName = "  "
			return r
		},
		"缺 schema_name": func(r domain.FactCheckRecord) domain.FactCheckRecord {
			r.SchemaName = ""
			return r
		},
	} {
		t.Run(name, func(t *testing.T) {
			record := mutate(testFactCheck("run-fact-004", "attribute", 1, domain.FactVerdictPass, nil))
			err := st.RecordFactCheck(context.Background(), record)
			if err == nil {
				t.Fatal("缺身份的记录应被拒绝")
			}
			if code := errcode.As(err).Code; code != errcode.Validation {
				t.Fatalf("错误码应为 %s，实际 %s", errcode.Validation, code)
			}
		})
	}
	count, err := st.CountFactChecks(context.Background(), "run-fact-004")
	if err != nil {
		t.Fatalf("统计事实校验记录失败：%v", err)
	}
	if count != 0 {
		t.Fatalf("缺身份的记录不应入库，实际 %d 行", count)
	}
}

// TestFactChecksByRun_RejectsMissingRunID 验证回读必须带 run_id。
func TestFactChecksByRun_RejectsMissingRunID(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.FactChecksByRun(context.Background(), "  "); err == nil {
		t.Fatal("缺 run_id 时应报错")
	}
	if _, err := st.CountFactChecks(context.Background(), ""); err == nil {
		t.Fatal("缺 run_id 时统计应报错")
	}
}

// TestOpen_CreatesFactChecksTableForLegacyDB 验证存量库打开时补齐新表：
// 老库里没有这张表时，服务要能照常起（老数据不能因为加表而作废）。
func TestOpen_CreatesFactChecksTableForLegacyDB(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/legacy.db"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开库失败：%v", err)
	}
	if _, err := db.Exec("CREATE TABLE schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		t.Fatalf("造老库失败：%v", err)
	}
	if _, err := db.Exec("INSERT INTO schema_meta (key, value) VALUES ('schema_version', '3')"); err != nil {
		t.Fatalf("写老库版本号失败：%v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭老库失败：%v", err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("打开老库失败：%v", err)
	}
	defer func() { _ = st.Close() }()

	if err := st.RecordFactCheck(context.Background(),
		testFactCheck("run-fact-005", "attribute", 1, domain.FactVerdictPass, nil)); err != nil {
		t.Fatalf("老库补表后写入失败：%v", err)
	}
	count, err := st.CountFactChecks(context.Background(), "run-fact-005")
	if err != nil {
		t.Fatalf("统计事实校验记录失败：%v", err)
	}
	if count != 1 {
		t.Fatalf("老库补表后应有 1 行记录，实际 %d 行", count)
	}

	var version string
	if err := st.db.QueryRow("SELECT value FROM schema_meta WHERE key = 'schema_version'").Scan(&version); err != nil {
		t.Fatalf("读取库结构版本号失败：%v", err)
	}
	// 断言对着当前的库结构版本号，而不是写死某个数字：
	// 库结构每加一张表（事实校验表、审计链表）版本号就往上走一格，
	// 写死的数字会在这张表之后再次失效。
	if !strings.HasPrefix(version, schemaVersion) {
		t.Fatalf("库结构版本号应升到 %s，实际 %q", schemaVersion, version)
	}
}
