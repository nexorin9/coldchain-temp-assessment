#!/usr/bin/env bash
# 接口契约检查：拿 api/openapi.yaml 与代码里的路由表逐条比对。
#
# 守三件事，任何一件不成立都判失败并给非 0 退出码：
#   - 文档里有、代码里没有 → 失败：调用方按文档去调一个不存在的路径。
#   - 代码里有、文档里没有 → 失败：加了端点却没写文档，等于没加。
#   - 端点缺 examples，或样例只是占位符 → 失败：没有样例的接口文档等于没有，
#     字段名只能靠猜；猜错要到运行时才暴露。
#
# 用法：
#   bash scripts/check_openapi.sh              # 跑一遍契约检查
#   bash scripts/check_openapi.sh --doc <路径>  # 指定文档路径
#   bash scripts/check_openapi.sh --help
#
# 退出码：0 契约一致；1 存在不一致或文档读不出来。

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

DOC="api/openapi.yaml"

usage() {
  sed -n '2,20p' "$0"
  exit "${1:-0}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --doc)
      DOC="${2:-}"
      if [[ -z "$DOC" ]]; then
        echo "错误：--doc 需要一个路径" >&2
        exit 2
      fi
      shift 2
      ;;
    --help|-h)
      usage 0
      ;;
    *)
      echo "未知参数：$1" >&2
      usage 2
      ;;
  esac
done

if [[ ! -f "$DOC" ]]; then
  echo "契约检查失败：找不到 OpenAPI 文档 $DOC" >&2
  exit 1
fi

# 文档头部的三行自检：版本号、标题、路径表。
# 这三样缺一，文档就不能用——先在这里说清是哪一样缺了，
# 免得后面解析器报一堆看不懂的错。
if ! grep -q '^openapi: 3\.1' "$DOC"; then
  echo "契约检查失败：$DOC 没有声明 OpenAPI 3.1" >&2
  exit 1
fi
if ! grep -q '^  /v1/' "$DOC"; then
  echo "契约检查失败：$DOC 里没有任何 /v1/ 路径" >&2
  exit 1
fi
if ! grep -q '^info:' "$DOC"; then
  echo "契约检查失败：$DOC 缺少 info 段" >&2
  exit 1
fi

# 逐条比对交给 Go 程序：它认识 OpenAPI 结构，能把 $ref 解开看真实内容，
# 也能拿到代码里的路由表。shell 只做文档存在性与头部自检。
go run ./scripts/openapi_check --doc "$DOC"
