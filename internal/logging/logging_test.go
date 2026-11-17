package logging

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestWritesOneJSONLinePerEvent(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf)

	log.Info("serve_start", "version", "0.1.0")
	log.Error("serve_failed", "error", "配置缺失")

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("期望 2 行日志，实际 %d 行：%s", len(lines), buf.String())
	}

	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("首行不是合法 JSON：%v", err)
	}
	if rec["event"] != "serve_start" {
		t.Fatalf("期望 event=serve_start，实际 %v", rec["event"])
	}
	if rec["level"] != "info" {
		t.Fatalf("期望 level=info，实际 %v", rec["level"])
	}
	if _, ok := rec["ts"]; !ok {
		t.Fatal("日志应包含时间戳字段 ts")
	}
}

func TestWithAddsContextFields(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf).With("run_id", "<RUN_ID>", "step_name", "attribute")

	log.Info("step_start")

	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &rec); err != nil {
		t.Fatalf("日志不是合法 JSON：%v", err)
	}
	if rec["run_id"] != "<RUN_ID>" || rec["step_name"] != "attribute" {
		t.Fatalf("期望继承 run_id 与 step_name，实际：%v", rec)
	}
}

func TestSensitiveFieldsAreRedacted(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf)

	log.Info("model_call", "api_key", "super-secret", "MODEL_API_KEY", "another-secret", "model", "hospital-llm")

	out := buf.String()
	if strings.Contains(out, "super-secret") || strings.Contains(out, "another-secret") {
		t.Fatalf("敏感字段应被脱敏，实际输出：%s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("脱敏后应保留占位标记，实际输出：%s", out)
	}
	if !strings.Contains(out, "hospital-llm") {
		t.Fatalf("非敏感字段应原样保留，实际输出：%s", out)
	}
}
