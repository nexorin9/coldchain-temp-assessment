#!/usr/bin/env bash
# 并发幂等压测：同一批温度事件并发灌入，核对两条不变量——
#
#   1. 事件不重复入库：同一批采样投多少遍，库里只该有一条一条的采样；
#   2. 判定条数不膨胀：同一段越温无论并发投多少遍，只判一条判定 run。
#
# 这两条是冷链场景的生死线：冷库夜间断电数小时，记录仪会把断电期间的每一条
# 采样都推上来，网关重试还会再推几遍。投多少遍就判多少条，主管面前就是几十条
# 同一场断电的判定记录，台账里也会留下几十条越温。
#
# 压测分两段，各压各的：
#   第一段走真实 HTTP 入口（真实进程、真实库、真实 Bearer 鉴权），核对事件不重复入库；
#   第二段在进程内并发压判定调度器，核对判定条数不膨胀。
#   之所以分段：判定 run 由调度器按「点位温区配置」建立，而温区配置的管理接口
#   尚未落地，服务进程拿不到点位配置时不会建判定——把这一段硬塞进 HTTP 流程里，
#   压的就不是产品的判定去抖逻辑了。
#
# 用法：
#   bash scripts/stress_concurrency.sh [--deliveries N] [--parallel P] [--port P] [--keep]
#
#   --deliveries N  同一批采样投递几遍（默认 20）
#   --parallel P     同时发出的请求数（默认 8）
#   --port P         服务监听端口（默认 18099）
#   --keep           保留压测临时目录（默认压完即删）
#
# 退出码：0 表示两条不变量都成立；非 0 表示压测失败并打印失败原因。
set -euo pipefail

PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$PROJECT_DIR"

DELIVERIES=20
PARALLEL=8
PORT=18099
KEEP=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --deliveries) DELIVERIES="$2"; shift 2 ;;
    --parallel) PARALLEL="$2"; shift 2 ;;
    --port) PORT="$2"; shift 2 ;;
    --keep) KEEP=1; shift ;;
    -h|--help) sed -n '2,25p' "$0"; exit 0 ;;
    *) echo "未知参数：$1" >&2; exit 2 ;;
  esac
done

command -v sqlite3 >/dev/null 2>&1 || {
  echo "缺少 sqlite3 客户端，无法核对库内行数" >&2
  exit 2
}
command -v curl >/dev/null 2>&1 || {
  echo "缺少 curl，无法发起并发投递" >&2
  exit 2
}

WORK="$(mktemp -d "${TMPDIR:-/tmp}/coldchain-stress.XXXXXX")"
SERVER_PID=""
cleanup() {
  if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  if [[ "$KEEP" -eq 1 ]]; then
    echo "压测临时目录已保留：$WORK"
  else
    rm -rf "$WORK"
  fi
}
trap cleanup EXIT

echo "=== 冷链判定并发幂等压测 ==="
echo "投递遍数：$DELIVERIES    并发数：$PARALLEL    端口：$PORT"
echo "临时目录：$WORK"

# ── 准备：编译服务端、造一批越限采样 ───────────────────────────────
mkdir -p "$WORK/bin" "$WORK/batch" "$WORK/archive"
echo "编译服务端二进制…"
go build -o "$WORK/bin/coldchain" ./cmd/coldchain

# 一段越温窗口：3 条采样，1 分钟粒度，峰值 12.4℃（温区 2~8℃）。
# 采样间隔取 1 分钟是刻意的：调度器的同窗去抖容忍 3 个采样周期，
# 这段窗口的跨度（2 分钟）稳稳落在容忍范围内——并发乱序投递也只应判一次。
cat > "$WORK/profile.json" <<'JSON'
{
  "point_id": "point-vax-freezer-01",
  "zone_min_c": 2,
  "zone_max_c": 8,
  "sample_interval_sec": 60,
  "min_excursion_minutes": 1,
  "hysteresis_c": 0.5,
  "accuracy_limit_c": 0.5
}
JSON

cat > "$WORK/batch/01.json" <<'JSON'
{
  "point_id": "point-vax-freezer-01",
  "device_id": "device-recorder-0001",
  "device_name": "疫苗冷藏库温度记录仪",
  "measured_at": "2026-10-20T03:10:00+08:00",
  "temperature_c": 11.4,
  "unit": "C",
  "humidity_pct": 62.5,
  "battery_pct": 91,
  "accuracy_c": 0.3,
  "source": "recorder",
  "alarm_flag": true,
  "events": []
}
JSON

cat > "$WORK/batch/02.json" <<'JSON'
{
  "point_id": "point-vax-freezer-01",
  "device_id": "device-recorder-0001",
  "device_name": "疫苗冷藏库温度记录仪",
  "measured_at": "2026-10-20T03:11:00+08:00",
  "temperature_c": 12.4,
  "unit": "C",
  "humidity_pct": 64.0,
  "battery_pct": 91,
  "accuracy_c": 0.3,
  "source": "recorder",
  "alarm_flag": true,
  "events": []
}
JSON

cat > "$WORK/batch/03.json" <<'JSON'
{
  "point_id": "point-vax-freezer-01",
  "device_id": "device-recorder-0001",
  "device_name": "疫苗冷藏库温度记录仪",
  "measured_at": "2026-10-20T03:12:00+08:00",
  "temperature_c": 10.8,
  "unit": "C",
  "humidity_pct": 63.1,
  "battery_pct": 90,
  "accuracy_c": 0.3,
  "source": "recorder",
  "alarm_flag": true,
  "events": []
}
JSON

# 批次合集（进程内驱动器用同一份数据）：
# 把上面三份采样对象拼成一个 JSON 数组，不额外引入工具依赖。
{
  printf '['
  first=1
  for f in "$WORK"/batch/*.json; do
    if [[ "$first" -eq 1 ]]; then first=0; else printf ','; fi
    tr -d '\n' < "$f"
  done
  printf ']'
} > "$WORK/batch.json"

SAMPLE_COUNT=$(find "$WORK/batch" -name '*.json' | wc -l | tr -d ' ')
TOTAL=$((DELIVERIES * SAMPLE_COUNT))
echo "采样批次：$SAMPLE_COUNT 条（同一段越温窗口），计划投递 $TOTAL 次"

# ── 起服务：真实进程、真实 HTTP 入口、真实本地库 ────────────────────
export COLDCHAIN_INGEST_TOKEN="stress-ingest-token"
export COLDCHAIN_LEDGER_BASE_URL="https://ledger.internal.example-hospital.cn"
export COLDCHAIN_LEDGER_TOKEN="stress-ledger-token"
export COLDCHAIN_NOTIFY_BASE_URL="https://notify.internal.example-hospital.cn"
export COLDCHAIN_NOTIFY_TOKEN="stress-notify-token"
export MODEL_BASE_URL="https://llm.internal.example-hospital.cn/v1"
export MODEL_API_KEY="stress-model-key"
export MODEL_NAME="stress-model"
export MODEL_TIMEOUT_SECONDS=30
export DATABASE_PATH="$WORK/stress.db"
export ARCHIVE_DIR="$WORK/archive"
# 压测要压的是幂等，不是限流：把限流闸门放到足够高，
# 免得请求被限流挡掉、误当成「去重生效」。
export COLDCHAIN_INGEST_RATE_LIMIT=1000000
export COLDCHAIN_INGEST_RATE_WINDOW_SECONDS=60
export COLDCHAIN_INGEST_RATE_BURST=1000000

"$WORK/bin/coldchain" serve --addr "127.0.0.1:$PORT" > "$WORK/serve.log" 2>&1 &
SERVER_PID=$!

BASE_URL="http://127.0.0.1:$PORT"
READY=0
for _ in $(seq 1 60); do
  if curl -fsS "$BASE_URL/healthz" >/dev/null 2>&1; then
    READY=1
    break
  fi
  sleep 0.25
done
if [[ "$READY" -ne 1 ]]; then
  echo "服务未在预期时间内就绪，服务日志：" >&2
  cat "$WORK/serve.log" >&2
  exit 1
fi
echo "服务已就绪：$BASE_URL"

# ── 第一段：真实 HTTP 并发投递，核对事件不重复入库 ──────────────────
: > "$WORK/jobs.txt"
for _ in $(seq 1 "$DELIVERIES"); do
  for f in "$WORK"/batch/*.json; do
    printf '%s\n' "$f" >> "$WORK/jobs.txt"
  done
done

export STRESS_TOKEN="$COLDCHAIN_INGEST_TOKEN"
export STRESS_BASE="$BASE_URL"
xargs -P "$PARALLEL" -n 1 sh -c \
  'curl -sS -o /dev/null -w "%{http_code}\n" -X POST -H "Authorization: Bearer $STRESS_TOKEN" -H "Content-Type: application/json" --data-binary @"$1" "$STRESS_BASE/v1/events/coldchain-temperature"' \
  _ < "$WORK/jobs.txt" > "$WORK/status.txt"

ACCEPTED=$(grep -c '^202$' "$WORK/status.txt" || true)
OTHER=$(grep -vc '^202$' "$WORK/status.txt" || true)
ROWS=$(sqlite3 "$WORK/stress.db" "SELECT COUNT(*) FROM events;")
DISTINCT=$(sqlite3 "$WORK/stress.db" "SELECT COUNT(DISTINCT idempotency_key) FROM events;")
POINTS=$(sqlite3 "$WORK/stress.db" "SELECT COUNT(DISTINCT point_id) FROM events;")

echo "--- 第一段：HTTP 并发投递 ---"
echo "投递数：${TOTAL}（已被服务接受 $ACCEPTED 次，非 202 响应 $OTHER 次）"
echo "库内 events 表行数：${ROWS}（唯一幂等键 ${DISTINCT}，点位 ${POINTS}）"

FAIL=0
if [[ "$ACCEPTED" -ne "$TOTAL" ]]; then
  echo "失败：投递未全部被接受（接受 $ACCEPTED / 应为 ${TOTAL}）" >&2
  FAIL=1
fi
if [[ "$ROWS" -ne "$SAMPLE_COUNT" ]]; then
  echo "失败：库内事件行数 ${ROWS}，应为唯一采样数 ${SAMPLE_COUNT}（重复投递未被去重）" >&2
  FAIL=1
fi
if [[ "$DISTINCT" -ne "$SAMPLE_COUNT" ]]; then
  echo "失败：唯一幂等键 ${DISTINCT}，应为 $SAMPLE_COUNT" >&2
  FAIL=1
fi
if [[ "$ROWS" -ge "$TOTAL" ]]; then
  echo "失败：投递 $TOTAL 次后库内 $ROWS 行，重复投递没有生效" >&2
  FAIL=1
fi

# ── 第二段：并发推进判定调度器，核对判定条数不膨胀 ─────────────────
# 判定 run 由调度器按点位与越温窗口建立：同一段越温只判一次，
# 后续采样并入既有判定。这一段在进程内并发压，避开 HTTP 投递顺序的影响。
echo "--- 第二段：调度器并发去抖 ---"
STRESS_OUT="$WORK/scheduler.out"
if go run ./scripts/stress --events "$WORK/batch.json" --profile "$WORK/profile.json" --deliveries "$DELIVERIES" > "$STRESS_OUT" 2>&1; then
  cat "$STRESS_OUT"
else
  cat "$STRESS_OUT" >&2
  echo "失败：调度器并发去抖不变量破了" >&2
  FAIL=1
fi

RUN_COUNT=$(grep -o '判定 run 数：[0-9]*' "$STRESS_OUT" | head -1 | grep -o '[0-9]*' || echo "")
RUN_COUNT="${RUN_COUNT:-0}"

echo "=== 压测结论 ==="
echo "投递数：$TOTAL"
echo "入库事件行数：${ROWS}（唯一采样 $SAMPLE_COUNT 条）"
echo "判定 run 数：$RUN_COUNT"
if [[ "$FAIL" -eq 0 ]]; then
  echo "两条不变量成立：事件不重复入库 ✓　判定条数不膨胀 ✓"
  exit 0
fi
echo "压测失败：至少一条不变量不成立" >&2
exit 1
