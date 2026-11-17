// openapi_check 是接口契约的守门程序：拿 api/openapi.yaml 与代码里的路由表
// 逐条比对，任何对不上的地方都判失败。
//
// 它守三件事：
//
//  1. 文档里有、代码里没有 → 判失败。文档写了却没实现的端点，会让调用方
//     按文档去调一个根本不存在的路径。
//  2. 代码里有、文档里没有 → 判失败。新加了端点却忘了写文档，
//     调用方根本不知道它存在。
//  3. 端点缺 examples，或 examples 只是占位符 → 判失败。
//     没有样例的接口文档等于没有：调用方只能靠猜字段名。
//     而「猜」出来的字段名一旦和实现不一致，错误要到运行时才暴露。
//
// 占位符的判定沿用「文档产线」那套做法：样例里带 TODO 注释键、
// 或只剩注释键、或值是「（填写）/ 待补 / TBD」这类字样，都算占位符。
//
// 用法：go run ./scripts/openapi_check [-doc api/openapi.yaml]
// 退出码 0 表示契约一致，1 表示有不一致。
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/chaosforge/coldchain-temp-assessment/internal/httpapi"
)

// forbiddenPlaceholders 是样例里不允许出现的占位符字样。
//
// 判据只有一个：这份样例能不能当成真的响应体看。
// 「（填写）」「待补」「TBD」「string」「xxx」都不能。
var forbiddenPlaceholders = map[string]bool{
	"（填写）":    true,
	"(填写)":    true,
	"待补":      true,
	"TBD":     true,
	"tbd":     true,
	"TODO":    true,
	"todo":    true,
	"xxx":     true,
	"string":  true,
	"…":       true,
	"...":     true,
	"example": true,
	"示例":      true,
}

func main() {
	docPath := flag.String("doc", "api/openapi.yaml", "OpenAPI 文档路径")
	flag.Parse()

	violations, err := check(*docPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "契约检查失败：%v\n", err)
		os.Exit(1)
	}
	if len(violations) > 0 {
		fmt.Fprintf(os.Stderr, "契约检查失败：%d 处不一致\n", len(violations))
		for _, v := range violations {
			fmt.Fprintf(os.Stderr, "  - %s\n", v)
		}
		os.Exit(1)
	}
	fmt.Println("契约检查通过：文档与代码路由表一致，每个端点都带真实样例")
}

// check 执行全部比对，返回不一致清单。
func check(docPath string) ([]string, error) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile(docPath)
	if err != nil {
		return nil, fmt.Errorf("加载 %s 失败：%v", docPath, err)
	}
	if doc.OpenAPI == "" {
		return nil, fmt.Errorf("%s 缺少 openapi 版本声明", docPath)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.1") {
		return nil, fmt.Errorf("%s 声明的版本是 %q，本服务要求 OpenAPI 3.1", docPath, doc.OpenAPI)
	}

	violations := make([]string, 0, 16)

	// 代码里的路由表：与文档逐条对照。
	codeRoutes := map[string]bool{}
	for _, route := range httpapi.New(httpapi.Options{Version: "0.1.0"}).Routes() {
		codeRoutes[route.Method+" "+route.Path] = true
	}

	// 文档里的端点：路径 × 方法。
	docRoutes := map[string]bool{}
	for path, item := range doc.Paths.Map() {
		if item == nil {
			violations = append(violations, fmt.Sprintf("路径 %s 没有内容", path))
			continue
		}
		for method, op := range item.Operations() {
			key := strings.ToUpper(method) + " " + path
			docRoutes[key] = true
			if op == nil {
				violations = append(violations, fmt.Sprintf("端点 %s 没有内容", key))
				continue
			}
			violations = append(violations, checkOperation(key, op)...)
		}
	}

	for key := range codeRoutes {
		if !docRoutes[key] {
			violations = append(violations,
				fmt.Sprintf("代码里注册了端点 %s，但 %s 里没有", key, docPath))
		}
	}
	for key := range docRoutes {
		if !codeRoutes[key] {
			violations = append(violations,
				fmt.Sprintf("%s 里写了端点 %s，但代码里没有注册", docPath, key))
		}
	}

	sort.Strings(violations)
	return violations, nil
}

// checkOperation 检查单个端点的样例与说明是否齐备。
func checkOperation(key string, op *openapi3.Operation) []string {
	violations := make([]string, 0, 4)

	if strings.TrimSpace(op.Summary) == "" {
		violations = append(violations, fmt.Sprintf("端点 %s 缺 summary", key))
	}
	if op.Responses == nil || op.Responses.Len() == 0 {
		violations = append(violations, fmt.Sprintf("端点 %s 没有响应定义", key))
		return violations
	}

	for status, ref := range op.Responses.Map() {
		if ref == nil || ref.Value == nil {
			violations = append(violations, fmt.Sprintf("端点 %s 的响应 %s 没有内容", key, status))
			continue
		}
		response := ref.Value
		if len(response.Content) == 0 {
			violations = append(violations,
				fmt.Sprintf("端点 %s 的响应 %s 没有响应体定义", key, status))
			continue
		}
		for mediaType, content := range response.Content {
			if content == nil {
				violations = append(violations,
					fmt.Sprintf("端点 %s 的响应 %s（%s）没有内容", key, status, mediaType))
				continue
			}
			if len(content.Examples) == 0 {
				violations = append(violations,
					fmt.Sprintf("端点 %s 的响应 %s（%s）缺 examples", key, status, mediaType))
				continue
			}
			for name, example := range content.Examples {
				if example == nil || example.Value == nil {
					violations = append(violations,
						fmt.Sprintf("端点 %s 的响应 %s（%s）的样例 %s 是空的", key, status, mediaType, name))
					continue
				}
				if placeholder, why := isPlaceholder(example.Value.Value); placeholder {
					violations = append(violations,
						fmt.Sprintf("端点 %s 的响应 %s（%s）的样例 %s 是占位符：%s",
							key, status, mediaType, name, why))
				}
			}
		}
	}

	return violations
}

// isPlaceholder 判断一个样例值是不是占位符。
//
// 四种判据：
//   - 带 "// TODO" 键：文档产线里明确拒绝这种样例；
//   - 只剩注释键（全部以 "//" 开头）：没有真实内容；
//   - 空对象：什么都没有；
//   - 值是「（填写）/ 待补 / TBD」这类字样：写了等于没写。
func isPlaceholder(value any) (bool, string) {
	switch typed := value.(type) {
	case map[string]any:
		if _, ok := typed["// TODO"]; ok {
			return true, "带 // TODO 注释键"
		}
		if len(typed) == 0 {
			return true, "空对象"
		}
		real := 0
		for key, item := range typed {
			if strings.HasPrefix(key, "//") {
				continue
			}
			real++
			if placeholder, why := isPlaceholder(item); placeholder {
				return true, fmt.Sprintf("字段 %s %s", key, why)
			}
		}
		if real == 0 {
			return true, "只剩注释键，没有真实内容"
		}
	case []any:
		// 空数组不算占位符：「没有待补偿的越温」本身就是一份真实响应，
		// 它的值就是空列表。占位符的判据是「没写」，不是「恰好为空」。
		for _, item := range typed {
			if placeholder, why := isPlaceholder(item); placeholder {
				return true, why
			}
		}
	case string:
		trimmed := strings.TrimSpace(typed)
		if forbiddenPlaceholders[trimmed] {
			return true, fmt.Sprintf("值 %q 是占位符字样", trimmed)
		}
	}
	return false, ""
}
