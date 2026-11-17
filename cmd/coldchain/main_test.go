package main

import (
	"os"
	"strings"
	"testing"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/runtime"
)

// captureStderr 跑一段逻辑并收集它写到 stderr 的内容。
//
// serve 的启动失败信息全在 stderr 上（stdout 留给正常输出），
// 所以断言「打印了冲突名」必须把它抓下来看。
func captureStderr(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	orig := os.Stderr
	path := t.TempDir() + "/stderr.log"
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建临时 stderr 文件失败：%v", err)
	}
	os.Stderr = f
	code := fn()
	os.Stderr = orig
	if err := f.Close(); err != nil {
		t.Fatalf("关闭临时 stderr 文件失败：%v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取临时 stderr 文件失败：%v", err)
	}
	return code, string(data)
}

// TestServe_DuplicateResultShapeExitsNonZero 验证重复登记同名结果类型时，
// serve 启动即失败：打印冲突名并以非 0 退出码退出，不监听端口。
//
// 形状冲突意味着同一个结果类型名底下挂着两种形状。带着这样的结论去写台账，
// 解析器多半不会报错，而是安静地读出半个结论——所以必须起不来。
func TestServe_DuplicateResultShapeExitsNonZero(t *testing.T) {
	orig := shapeRegistryBuilder
	t.Cleanup(func() { shapeRegistryBuilder = orig })

	shapeRegistryBuilder = func() (*runtime.ResultShapeRegistry, error) {
		// 与内置的归因结论同名：注册时即冲突。
		return runtime.BuildResultShapeRegistry(runtime.ResultShape{
			Name:   string(domain.PayloadAttribution),
			Kind:   domain.PayloadAttribution,
			New:    func() any { return &domain.AttributionPayload{} },
			Source: "test:重复登记",
		})
	}

	code, stderr := captureStderr(t, func() int {
		return run([]string{"serve"})
	})

	if code == 0 {
		t.Fatal("重复登记同名结果类型时 serve 应以非 0 退出码退出")
	}
	if !strings.Contains(stderr, string(domain.PayloadAttribution)) {
		t.Fatalf("启动失败信息应点名冲突的结果类型 %q，实际：%s", domain.PayloadAttribution, stderr)
	}
	if !strings.Contains(stderr, "重复登记") {
		t.Fatalf("启动失败信息应说明是重复登记，实际：%s", stderr)
	}
	if !strings.Contains(stderr, "判定结果形状校验失败") {
		t.Fatalf("启动失败信息应指明是结果形状校验失败，实际：%s", stderr)
	}
}

// TestServe_MissingConfigStillReportedAfterShapeCheck 验证形状校验通过后，
// 缺配置的启动失败信息照旧报出来（形状校验不能把配置校验顶掉）。
func TestServe_MissingConfigStillReportedAfterShapeCheck(t *testing.T) {
	for _, key := range []string{
		"COLDCHAIN_INGEST_TOKEN",
		"COLDCHAIN_LEDGER_BASE_URL",
		"COLDCHAIN_LEDGER_TOKEN",
		"COLDCHAIN_NOTIFY_BASE_URL",
		"COLDCHAIN_NOTIFY_TOKEN",
		"MODEL_BASE_URL",
		"MODEL_API_KEY",
		"MODEL_NAME",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("DATABASE_PATH", t.TempDir()+"/coldchain.db")

	code, stderr := captureStderr(t, func() int {
		return run([]string{"serve"})
	})

	if code == 0 {
		t.Fatal("缺必填配置时 serve 应以非 0 退出码退出")
	}
	if !strings.Contains(stderr, "配置校验失败") {
		t.Fatalf("应报出配置校验失败，实际：%s", stderr)
	}
}
