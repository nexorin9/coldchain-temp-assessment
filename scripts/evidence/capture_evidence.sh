#!/usr/bin/env bash
# 采集 docs/screenshots/ 下的实测证据（真实服务 + 真实调用 + 真实响应）。
#
# 这条链路跑的是产品本身：
#   1. 起本地协议替身，占住「冷链台账 / 通知系统 / 模型端点」三个院外系统的位置；
#   2. 起真实的 coldchain serve；
#   3. 用 testdata 里的脱敏样例灌事件，跑完整判定链；
#   4. 主管确认 → 出站写回 → 导出台账与周报；
#   5. 把每一段真实回显落成 .txt，再由 scripts/render_terminal_shot.py 渲染成 PNG。
#
# 替身只在本机占位，产品代码里没有任何 localhost 默认值：
# 凭据与基址全部由下面的环境变量注入，且这些令牌只在本次取证环境里有效。
set -eu

PROJ="$(cd "$(dirname "$0")/../.." && pwd)"
WORK="${COLDCHAIN_SHOT_WORK:-/tmp/coldchain-evidence}"
SHOTS=$WORK/shots
PORT="${COLDCHAIN_SHOT_PORT:-18099}"
API="http://127.0.0.1:$PORT"

rm -rf "$WORK"
mkdir -p "$SHOTS"

export COLDCHAIN_INGEST_TOKEN=ingest-token-for-screenshot
export COLDCHAIN_OPERATOR_TOKEN=operator-token-for-screenshot
export COLDCHAIN_SUPERVISOR_TOKEN=supervisor-token-for-screenshot
export COLDCHAIN_AUDITOR_TOKEN=auditor-token-for-screenshot
LEDGER_PORT="${COLDCHAIN_STUB_LEDGER_PORT:-18141}"
NOTIFY_PORT="${COLDCHAIN_STUB_NOTIFY_PORT:-18142}"
MODEL_PORT="${COLDCHAIN_STUB_MODEL_PORT:-18143}"
export COLDCHAIN_LEDGER_BASE_URL=http://127.0.0.1:$LEDGER_PORT
export COLDCHAIN_LEDGER_TOKEN=ledger-token-for-screenshot
export COLDCHAIN_NOTIFY_BASE_URL=http://127.0.0.1:$NOTIFY_PORT
export COLDCHAIN_NOTIFY_TOKEN=notify-token-for-screenshot
export MODEL_BASE_URL=http://127.0.0.1:$MODEL_PORT/v1
export MODEL_API_KEY=model-key-for-screenshot
export MODEL_NAME=coldchain-attribution-stub
export DATABASE_PATH=$WORK/coldchain.db
export ARCHIVE_DIR=$WORK/archive

shot() {
  FILE=$SHOTS/$1.txt
  { echo "# $2"; echo "# 采集时间：$(date '+%Y-%m-%d %H:%M:%S %z')"; echo; } > "$FILE"
}
cmd() { FILE=$1; shift; echo "\$ $*" >> "$FILE"; "$@" >> "$FILE" 2>&1; echo >> "$FILE"; }
cmdpipe() { FILE=$1; shift; echo "\$ $*" >> "$FILE"; eval "$*" >> "$FILE" 2>&1; echo >> "$FILE"; }

cleanup() {
  [ -n "${SERVE_PID:-}" ] && kill "$SERVE_PID" 2>/dev/null || true
  [ -n "${STUB_PID:-}" ] && kill "$STUB_PID" 2>/dev/null || true
}
trap cleanup EXIT

echo "=== 0. 起协议替身（冷链台账 / 通知系统 / 模型端点） ==="
nohup python3 "$PROJ/scripts/evidence/stub_servers.py" > "$WORK/stub.log" 2>&1 &
STUB_PID=$!
sleep 1
cat "$WORK/stub.log"

echo "=== 1. 构建 ==="
cd "$PROJ"
go build -o "$WORK/coldchain" ./cmd/coldchain

echo "=== 2. ① 事件投递 → 判定 run → 判定结论 ==="
shot 01-assess-run "① 记录仪样例批量导入 → 建判定 run → 五步判定链给出结论（CLI 通道）"
cmd "$SHOTS/01-assess-run.txt" "$WORK/coldchain" import --file testdata/recordings_sample.csv --db "$WORK/coldchain.db"
echo '$ coldchain assess --events testdata/series_excursion.json --profile testdata/point_profile.json --steps --model --ledger --db <本地库>' >> "$SHOTS/01-assess-run.txt"
"$WORK/coldchain" assess --events testdata/series_excursion.json --profile testdata/point_profile.json \
  --steps --model --ledger --db "$WORK/coldchain.db" > "$WORK/assess.stdout.json" 2> "$WORK/assess.stderr.txt"
python3 "$PROJ/scripts/evidence/project_assess.py" >> "$SHOTS/01-assess-run.txt" 2>&1
cp "$WORK/assess.stdout.json" "$SHOTS/01-assess-run.full.json"
echo >> "$SHOTS/01-assess-run.txt"

RUN_ID=$(python3 -c "import sqlite3;print(sqlite3.connect('$WORK/coldchain.db').execute('select run_id from runs order by created_at desc limit 1').fetchone()[0])")

echo "=== 3. 起真实服务 ==="
nohup "$WORK/coldchain" serve --addr 127.0.0.1:$PORT > "$WORK/serve.log" 2>&1 &
SERVE_PID=$!
for _ in $(seq 1 40); do curl -sS "$API/healthz" > /dev/null 2>&1 && break; sleep 0.25; done

echo "=== 4. ② 判定详情与步骤时间线 ==="
shot 02-run-timeline "② GET /v1/runs/{id} 与 GET /v1/runs/{id}/timeline 的真实响应（凭据已脱敏）"
echo "\$ curl -sS -H 'Authorization: Bearer <经办凭据>' $API/v1/runs/$RUN_ID" >> "$SHOTS/02-run-timeline.txt"
curl -sS -H "Authorization: Bearer $COLDCHAIN_OPERATOR_TOKEN" "$API/v1/runs/$RUN_ID" > "$WORK/run.json"
python3 "$PROJ/scripts/evidence/project_run.py" >> "$SHOTS/02-run-timeline.txt" 2>&1
echo >> "$SHOTS/02-run-timeline.txt"
echo "\$ curl -sS -H 'Authorization: Bearer <质控只读凭据>' $API/v1/runs/$RUN_ID/timeline" >> "$SHOTS/02-run-timeline.txt"
curl -sS -H "Authorization: Bearer $COLDCHAIN_AUDITOR_TOKEN" "$API/v1/runs/$RUN_ID/timeline" > "$WORK/timeline.json"
python3 "$PROJ/scripts/evidence/project_timeline.py" >> "$SHOTS/02-run-timeline.txt" 2>&1
echo >> "$SHOTS/02-run-timeline.txt"

echo "=== 5. ③ 主管确认 → 出站写回 ==="
shot 03-confirm-writeback "③ 主管确认 → 冷链台账写入 → 接种门诊通知（两侧收到的真实报文）"
echo "\$ curl -sS -X POST -H 'Authorization: Bearer <主管凭据>' -H 'Content-Type: application/json' -d '{\"actor\":\"supervisor-007\",\"disposition\":\"isolate\",\"opinion\":\"已联系疫苗生产企业评估质量影响，先隔离在库批次\"}' $API/v1/runs/$RUN_ID/confirm" >> "$SHOTS/03-confirm-writeback.txt"
curl -sS -X POST -H "Authorization: Bearer $COLDCHAIN_SUPERVISOR_TOKEN" -H 'Content-Type: application/json' \
  -d '{"actor":"supervisor-007","disposition":"isolate","opinion":"已联系疫苗生产企业评估质量影响，先隔离在库批次"}' \
  "$API/v1/runs/$RUN_ID/confirm" | python3 -m json.tool --no-ensure-ascii >> "$SHOTS/03-confirm-writeback.txt" 2>&1
echo >> "$SHOTS/03-confirm-writeback.txt"
echo '$ curl -sS http://127.0.0.1:'"$LEDGER_PORT"'/__counts   # 冷链台账与通知系统两端真实收到的报文' >> "$SHOTS/03-confirm-writeback.txt"
curl -sS "http://127.0.0.1:$LEDGER_PORT/__counts" | python3 "$PROJ/scripts/evidence/project_counts.py" >> "$SHOTS/03-confirm-writeback.txt" 2>&1
echo >> "$SHOTS/03-confirm-writeback.txt"

echo "=== 6. ④ 成稿记录表 + 周报 CSV ==="
shot 04-draft-and-export "④ 《疫苗储存和运输温度异常情况记录表》正文与判定周报 CSV 片段"
cmd "$SHOTS/04-draft-and-export.txt" "$WORK/coldchain" draft --run-id "$RUN_ID" --db "$WORK/coldchain.db"
cmd "$SHOTS/04-draft-and-export.txt" "$WORK/coldchain" export --kind weekly \
  --from 2026-10-19T00:00:00+08:00 --to 2026-10-26T00:00:00+08:00 --out "$WORK/weekly.csv" --db "$WORK/coldchain.db"
cmdpipe "$SHOTS/04-draft-and-export.txt" "head -6 $WORK/weekly.csv"

echo "=== 7. 渲染成 PNG ==="
cp "$SHOTS"/*.txt "$SHOTS"/01-assess-run.full.json "$PROJ/docs/screenshots/"
cd "$PROJ"
python3 scripts/render_terminal_shot.py docs/screenshots/01-assess-run.txt docs/screenshots/01-assess-run.png --title "coldchain — 事件投递 → 判定 run → 判定结论"
python3 scripts/render_terminal_shot.py docs/screenshots/02-run-timeline.txt docs/screenshots/02-run-timeline.png --title "coldchain — GET /v1/runs/{id} 与 /timeline 的真实响应"
python3 scripts/render_terminal_shot.py docs/screenshots/03-confirm-writeback.txt docs/screenshots/03-confirm-writeback.png --title "coldchain — 主管确认 → 台账写回 → 门诊通知"
python3 scripts/render_terminal_shot.py docs/screenshots/04-draft-and-export.txt docs/screenshots/04-draft-and-export.png --title "coldchain — 《温度异常情况记录表》与判定周报 CSV"

echo "done. 采集文件在 $SHOTS"
