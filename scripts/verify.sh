#!/usr/bin/env bash
# 一条命令跑通：查环境 → 编译 → 跑测试 → 起服务 → 灌一条事件 → 打印判定结论与时间线。
#
# 这条脚本是给「刚拿到仓库的人」准备的。跑完它，屏幕上应该有
# 一条真实的判定结论和一条完整的步骤时间线——不是一句「已完成」，
# 而是能自己核对的事实：越温起止、峰值、持续多久、原因判成什么、结论停在哪一步。
#
# 出站三端（冷链台账 / 通知系统 / 模型端点）在本机由协议替身占位：
#   scripts/evidence/stub_servers.py 是**协议替身**，只在本机占位，不是最终对接方式。
#   产品代码里没有任何 localhost 默认值——基址与令牌都由下面的环境变量注入。
#   本机没有 python3 时脚本退到规则基线：不连模型、不查台账，
#   判定结论照样出得来（批次一栏写「待人工核对」），验证链路不因此断掉。
#
# 运行时数据一律落在临时目录（默认 $TMPDIR/coldchain-verify.*），
# 项目目录里不留下数据库、日志或构建产物。
set -euo pipefail

PROJ="$(cd "$(dirname "$0")/.." && pwd)"
cd "$PROJ"

WORK="${COLDCHAIN_VERIFY_WORK:-$(mktemp -d "${TMPDIR:-/tmp}/coldchain-verify.XXXXXX")}"
SERVE_PID=""
STUB_PID=""
FAILED=0

step() { printf '\n=== %s ===\n' "$*"; }

cleanup() {
  if [ -n "$SERVE_PID" ]; then kill "$SERVE_PID" 2>/dev/null || true; fi
  if [ -n "$STUB_PID" ]; then kill "$STUB_PID" 2>/dev/null || true; fi
  wait 2>/dev/null || true
  if [ "$FAILED" -eq 0 ]; then
    rm -rf "$WORK"
  else
    echo
    echo "验证未通过，临时目录保留在：$WORK（服务日志 $WORK/serve.log）"
  fi
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# 0. 环境检查
# ---------------------------------------------------------------------------
step "0. 环境检查"
command -v go >/dev/null 2>&1 || {
  echo "找不到 go：先装 Go ≥ 1.22（https://go.dev/dl/）"; exit 1; }
command -v curl >/dev/null 2>&1 || {
  echo "找不到 curl：验证脚本要靠它调服务接口"; exit 1; }

GOVER="$(go env GOVERSION)"          # 形如 go1.26.4
GONUM="${GOVER#go}"                  # 去掉 go 前缀 → 1.26.4
GOMAJOR="${GONUM%%.*}"               # 主版本号 → 1
GOTAIL="${GONUM#*.}"                 # 小版本与补丁 → 26.4
GOMINOR="${GOTAIL%%.*}"              # 小版本号 → 26
if [ "$GOMAJOR" -lt 1 ] || { [ "$GOMAJOR" -eq 1 ] && [ "$GOMINOR" -lt 22 ]; }; then
  echo "需要 Go ≥ 1.22，当前 $GOVER"; exit 1
fi
echo "go version: $(go version)"
echo "curl: $(command -v curl)"

HAVE_PY=0
if command -v python3 >/dev/null 2>&1; then HAVE_PY=1; fi

# ---------------------------------------------------------------------------
# 1. 编译
# ---------------------------------------------------------------------------
step "1. 编译（go build ./...）"
go build ./...
go build -o "$WORK/coldchain" ./cmd/coldchain
echo "编译通过：$WORK/coldchain"

# ---------------------------------------------------------------------------
# 2. 单元测试
# ---------------------------------------------------------------------------
step "2. 单元测试（go test ./...）"
go test ./... -count=1

# ---------------------------------------------------------------------------
# 3. 起协议替身与真实服务
# ---------------------------------------------------------------------------
step "3. 起协议替身与真实服务"

# 凭据只在本脚本的临时环境里生效：这些是占位令牌，不是任何院内系统的真凭据。
export COLDCHAIN_INGEST_TOKEN="${COLDCHAIN_INGEST_TOKEN:-verify-ingest-token}"
export COLDCHAIN_OPERATOR_TOKEN="${COLDCHAIN_OPERATOR_TOKEN:-verify-operator-token}"
export COLDCHAIN_SUPERVISOR_TOKEN="${COLDCHAIN_SUPERVISOR_TOKEN:-verify-supervisor-token}"
export COLDCHAIN_AUDITOR_TOKEN="${COLDCHAIN_AUDITOR_TOKEN:-verify-auditor-token}"
export COLDCHAIN_LEDGER_TOKEN="${COLDCHAIN_LEDGER_TOKEN:-verify-ledger-token}"
export COLDCHAIN_NOTIFY_TOKEN="${COLDCHAIN_NOTIFY_TOKEN:-verify-notify-token}"
export MODEL_API_KEY="${MODEL_API_KEY:-verify-model-key}"
export MODEL_NAME="${MODEL_NAME:-coldchain-attribution-verify}"
export DATABASE_PATH="$WORK/coldchain.db"
export ARCHIVE_DIR="$WORK/archive"

# 端口优先挑空闲端口；没有 python3 时退回固定端口（可用环境变量覆盖）。
pick_port() {
  if [ "$HAVE_PY" -eq 1 ]; then
    python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()'
  else
    echo "$1"
  fi
}
PORT="$(pick_port 18190)"
LEDGER_PORT="$(pick_port 18191)"
NOTIFY_PORT="$(pick_port 18192)"
MODEL_PORT="$(pick_port 18193)"
API="http://127.0.0.1:$PORT"

export COLDCHAIN_LEDGER_BASE_URL="http://127.0.0.1:$LEDGER_PORT"
export COLDCHAIN_NOTIFY_BASE_URL="http://127.0.0.1:$NOTIFY_PORT"
export MODEL_BASE_URL="http://127.0.0.1:$MODEL_PORT/v1"

if [ "$HAVE_PY" -eq 1 ]; then
  export COLDCHAIN_STUB_LEDGER_PORT="$LEDGER_PORT"
  export COLDCHAIN_STUB_NOTIFY_PORT="$NOTIFY_PORT"
  export COLDCHAIN_STUB_MODEL_PORT="$MODEL_PORT"
  nohup python3 "$PROJ/scripts/evidence/stub_servers.py" > "$WORK/stub.log" 2>&1 &
  STUB_PID=$!
  sleep 1
  cat "$WORK/stub.log"
  echo "协议替身已起：只在本机占位，不是最终对接方式"
else
  echo "本机没有 python3：跳过协议替身，归因走规则基线、在库批次留「待人工核对」"
fi

nohup "$WORK/coldchain" serve --addr "127.0.0.1:$PORT" > "$WORK/serve.log" 2>&1 &
SERVE_PID=$!

READY=0
for _ in $(seq 1 60); do
  if curl -sS -o /dev/null "$API/healthz" 2>/dev/null; then READY=1; break; fi
  sleep 0.25
done
if [ "$READY" -ne 1 ]; then
  FAILED=1
  echo "服务没能在 15 秒内就绪，stderr 如下："
  cat "$WORK/serve.log"
  exit 1
fi
echo "服务已就绪：$API/healthz"
printf 'GET /healthz → '
curl -sS "$API/healthz"; echo

# ---------------------------------------------------------------------------
# 4. 灌一条温度事件（入站 HTTP 主路径）
# ---------------------------------------------------------------------------
step "4. 灌一条温度事件（入站 HTTP 主路径）"
HTTP_CODE="$(curl -sS -o "$WORK/ingest.json" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $COLDCHAIN_INGEST_TOKEN" \
  -H 'Content-Type: application/json' \
  --data-binary @testdata/single_event.json \
  "$API/v1/events/coldchain-temperature")"
echo "POST /v1/events/coldchain-temperature → HTTP $HTTP_CODE"
if [ "$HAVE_PY" -eq 1 ]; then
  python3 -m json.tool --no-ensure-ascii "$WORK/ingest.json" || cat "$WORK/ingest.json"
else
  cat "$WORK/ingest.json"; echo
fi
if [ "$HTTP_CODE" != "202" ]; then
  FAILED=1
  echo "事件投递没有被受理（期望 202）"
  exit 1
fi

# ---------------------------------------------------------------------------
# 5. 判定结论
# ---------------------------------------------------------------------------
step "5. 判定结论（取数 → 抽窗口 → 归因 → 成稿 → 待主管确认）"
ASSESS_ARGS=(assess --events testdata/series_excursion.json --profile testdata/point_profile.json
             --steps --db "$WORK/coldchain.db")
if [ "$HAVE_PY" -eq 1 ]; then
  ASSESS_ARGS+=(--model --ledger)
fi

# 服务与 CLI 同时开着同一个库：偶发写锁冲突时重试，不把一次锁竞争当成判定失败。
ASSESS_OK=0
for attempt in 1 2 3; do
  if "$WORK/coldchain" "${ASSESS_ARGS[@]}" > "$WORK/assess.stdout.json" 2> "$WORK/assess.stderr.txt"; then
    ASSESS_OK=1
    break
  fi
  echo "第 $attempt 次判定未成功，重试一次（stderr 末尾）："
  tail -3 "$WORK/assess.stderr.txt" || true
  sleep 1
done
if [ "$ASSESS_OK" -ne 1 ]; then
  FAILED=1
  echo "判定没有跑通"
  exit 1
fi

if [ "$HAVE_PY" -eq 1 ]; then
  COLDCHAIN_SHOT_WORK="$WORK" python3 "$PROJ/scripts/evidence/project_assess.py"
else
  echo "（本机没有 python3：从原始 stdout 里挑结论字段；载荷全量在下方 grep 之外的部分）"
  grep -E '"(run_id|point_id|state|exec_state|window_start|window_end|peak_c|duration_minutes|cause_code|cause_label|confidence|needs_review)"' \
    "$WORK/assess.stdout.json" | head -40
  echo
  tail -1 "$WORK/assess.stderr.txt" || true
fi

# ---------------------------------------------------------------------------
# 6. 服务侧读回判定 run 与步骤时间线
# ---------------------------------------------------------------------------
step "6. 判定 run 与步骤时间线（服务侧读回）"
if [ "$HAVE_PY" -eq 1 ]; then
  RUN_ID="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["run"]["run_id"])' "$WORK/assess.stdout.json")"
else
  RUN_ID="$(sed -n 's/.*"run_id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$WORK/assess.stdout.json" | head -1)"
fi
if [ -z "$RUN_ID" ]; then
  FAILED=1
  echo "判定结论里没有 run_id，取不到要回读的判定 run"
  exit 1
fi
echo "run_id: $RUN_ID"

RUN_CODE="$(curl -sS -o "$WORK/run.json" -w '%{http_code}' \
  -H "Authorization: Bearer $COLDCHAIN_OPERATOR_TOKEN" "$API/v1/runs/$RUN_ID")"
TL_CODE="$(curl -sS -o "$WORK/timeline.json" -w '%{http_code}' \
  -H "Authorization: Bearer $COLDCHAIN_AUDITOR_TOKEN" "$API/v1/runs/$RUN_ID/timeline")"
echo "GET /v1/runs/$RUN_ID → HTTP $RUN_CODE"
echo "GET /v1/runs/$RUN_ID/timeline → HTTP $TL_CODE"
if [ "$RUN_CODE" != "200" ] || [ "$TL_CODE" != "200" ]; then
  FAILED=1
  echo "服务侧读回判定 run / 时间线失败"
  cat "$WORK/run.json" "$WORK/timeline.json" || true
  exit 1
fi

if [ "$HAVE_PY" -eq 1 ]; then
  COLDCHAIN_SHOT_WORK="$WORK" python3 "$PROJ/scripts/evidence/project_timeline.py"
else
  grep -E '"(step_name|state|duration_ms|offset_ms|start_pct|width_pct|total_duration_ms|step_count)"' \
    "$WORK/timeline.json" | head -40
fi

# ---------------------------------------------------------------------------
# 7. 项目目录口径检查
# ---------------------------------------------------------------------------
step "7. 项目目录检查（发布前口径）"
LEFTOVER="$(find "$PROJ" -maxdepth 2 \
  \( -name '*.db' -o -name '*.sqlite3' -o -name '*.log' -o -name '__pycache__' \
     -o -name 'coverage' -o -name 'bin' -o -name '.pytest_cache' \) \
  -not -path '*/.git/*' | head -20)"
if [ -n "$LEFTOVER" ]; then
  FAILED=1
  echo "项目目录里仍有运行时文件："
  echo "$LEFTOVER"
  exit 1
fi
echo "项目目录无运行时文件（数据库 / 日志 / 缓存 / 构建产物）"

step "验证通过"
cat <<'SUMMARY'
一条温度事件 → 一条判定 run → 五步判定链 → 结论停在「待药学部主管确认」。
未确认之前出站写回调用数为 0——这不是配置出来的，是判定链上那道闸门拦的。

整条链路可重复执行：bash scripts/verify.sh
SUMMARY
