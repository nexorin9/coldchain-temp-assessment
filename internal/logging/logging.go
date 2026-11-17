// Package logging 提供结构化 JSON 行日志。
//
// 日志只写 stderr、不落文件：服务跑在院内内网的小主机上，文件落盘属于运行期数据，
// 发布前需要清理。每行一条 JSON，便于信息科用 journald / 文件采集统一收口，
// 也便于把「一次判定 run 的哪一步慢、哪一步失败重试过」按 run_id 与步骤名串起来。
package logging

import (
	"encoding/json"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Level 是日志级别。
type Level string

// 日志级别。
const (
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// sensitiveKeys 是禁止写进日志的键名（大小写不敏感）。
var sensitiveKeys = map[string]bool{
	"api_key":       true,
	"apikey":        true,
	"token":         true,
	"authorization": true,
	"password":      true,
	"secret":        true,
	"ingest_token":  true,
	"ledger_token":  true,
	"notify_token":  true,
	"model_api_key": true,
	"access_token":  true,
	"bearer":        true,
	"credential":    true,
	"credentials":   true,
}

// Logger 是并发安全的结构化日志器。
type Logger struct {
	mu    sync.Mutex
	out   io.Writer
	base  map[string]any
	nowFn func() time.Time
}

// New 创建一个写向 out 的日志器。
func New(out io.Writer) *Logger {
	if out == nil {
		out = os.Stderr
	}
	return &Logger{out: out, base: map[string]any{}, nowFn: time.Now}
}

// Default 返回写 stderr 的默认日志器。
func Default() *Logger { return New(os.Stderr) }

// With 派生一个带附加字段的子日志器（如带 run_id / step_name）。
// 键值必须成对出现，键为字符串；奇数长度时忽略最后一个孤立键。
func (l *Logger) With(kv ...any) *Logger {
	child := &Logger{out: l.out, nowFn: l.nowFn, base: map[string]any{}}
	for k, v := range l.base {
		child.base[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		key, ok := kv[i].(string)
		if !ok {
			continue
		}
		child.base[key] = sanitize(key, kv[i+1])
	}
	return child
}

// Info 记录一条 info 级日志。
func (l *Logger) Info(event string, kv ...any) { l.log(LevelInfo, event, kv...) }

// Warn 记录一条 warn 级日志。
func (l *Logger) Warn(event string, kv ...any) { l.log(LevelWarn, event, kv...) }

// Error 记录一条 error 级日志。
func (l *Logger) Error(event string, kv ...any) { l.log(LevelError, event, kv...) }

// log 写出一行 JSON。
func (l *Logger) log(level Level, event string, kv ...any) {
	rec := map[string]any{
		"ts":    l.nowFn().UTC().Format(time.RFC3339Nano),
		"level": string(level),
		"event": event,
	}
	for k, v := range l.base {
		rec[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		key, ok := kv[i].(string)
		if !ok {
			continue
		}
		rec[key] = sanitize(key, kv[i+1])
	}

	buf, err := json.Marshal(rec)
	if err != nil {
		buf = []byte(`{"level":"error","event":"log_marshal_failed"}`)
	}
	buf = append(buf, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.out.Write(buf)
}

// sanitize 对敏感字段做脱敏；键名大小写不敏感。
func sanitize(key string, value any) any {
	if sensitiveKeys[strings.ToLower(key)] {
		return "[REDACTED]"
	}
	return value
}

// SortedKeys 返回排序后的键名列表，便于测试与调试输出。
func SortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
