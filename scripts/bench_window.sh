#!/usr/bin/env bash
# 离线对照基准：在公开冷链数据集（VCC-CPLD）上跑窗口抽取与规则基线归因，
# 输出三项可核对的指标——窗口误差、归因 Top-1 一致率、耗时——并落盘到
# data/bench_baseline.json，供后续模型归因逐条对照。
#
# 三条不许让步的约定：
#   - 数据集没取到就报错退出：不拿 testdata/ 的样例行数冒充 40 万条规模，
#     也不拿样例上的指标冒充数据集上的指标。
#   - 行数不足 40 万条就报错退出：基准的价值在于真实规模，
#     拿一小撮数据跑出来的数字没有可比性。
#   - 指标不达标给非 0 退出码：窗口误差超过 1 个采样周期、或耗时超过 30 秒，
#     都让跑批调度看得见，而不是在日志里显示成一次成功。
#
# 用法：
#   bash scripts/bench_window.sh                     # 跑一遍基准（默认重复 2 次核对可复现）
#   bash scripts/bench_window.sh --repeat 1          # 只跑一遍
#   bash scripts/bench_window.sh --check             # 只校验已落盘的报告，不重跑
#   bash scripts/bench_window.sh --help
#
# 环境变量：
#   COLDCHAIN_BENCH_DATASET  数据集路径（默认 data/datasets/vcc-cpld/vaccine_coldchain.csv）
#   COLDCHAIN_BENCH_OUT      报告落盘路径（默认 data/bench_baseline.json）
#   COLDCHAIN_BENCH_REPEAT   重复运行次数（默认 2）
#   COLDCHAIN_BENCH_MIN_ROWS 数据集最小行数（默认 400000）
#   COLDCHAIN_BENCH_MAX_ERROR 窗口误差上限（采样条数，默认 1）
#   COLDCHAIN_BENCH_MAX_SECONDS 耗时上限（秒，默认 30）

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

DATASET="${COLDCHAIN_BENCH_DATASET:-data/datasets/vcc-cpld/vaccine_coldchain.csv}"
OUT="${COLDCHAIN_BENCH_OUT:-data/bench_baseline.json}"
REPEAT="${COLDCHAIN_BENCH_REPEAT:-2}"
MIN_ROWS="${COLDCHAIN_BENCH_MIN_ROWS:-400000}"
MAX_ERROR="${COLDCHAIN_BENCH_MAX_ERROR:-1}"
MAX_SECONDS="${COLDCHAIN_BENCH_MAX_SECONDS:-30}"

usage() {
  sed -n '2,30p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

CHECK_ONLY=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --repeat) REPEAT="$2"; shift 2 ;;
    --out) OUT="$2"; shift 2 ;;
    --dataset) DATASET="$2"; shift 2 ;;
    --check) CHECK_ONLY=1; shift ;;
    --help|-h) usage; exit 0 ;;
    *) echo "未知参数：$1" >&2; usage >&2; exit 2 ;;
  esac
done

fail() {
  echo "基准未执行：$1" >&2
  exit 1
}

# 一、数据集必须在位，且行数达标。
if [ ! -f "$DATASET" ]; then
  fail "公开冷链数据集不存在：$DATASET
请先取数：bash scripts/fetch_datasets.sh
（不拿 testdata/ 下的仓库内样例冒充 40 万条规模——样例只能证明链路通，不能证明基准成立）"
fi

ROWS=$(( $(wc -l < "$DATASET") - 1 ))
if [ "$ROWS" -lt "$MIN_ROWS" ]; then
  fail "数据集行数不足：$ROWS < ${MIN_ROWS}（基准要求真实规模，小样本跑出来的数字没有可比性）"
fi
echo "数据集就位：${DATASET}（$ROWS 行）"

# 二、只校验已落盘报告时，不重跑。
if [ "$CHECK_ONLY" -eq 1 ]; then
  [ -f "$OUT" ] || fail "基准报告不存在：${OUT}（先跑一次基准）"
  python3 - "$OUT" <<'PY'
import json, sys
report = json.load(open(sys.argv[1], encoding="utf-8"))
metrics = report["metrics"]
print(f"基准报告校验：{sys.argv[1]}")
print(f"  数据集 {report['dataset']}（{report['records']} 行）· 运行时刻 {report['generated_at']}")
print(f"  指标一 · 窗口误差：最大 {metrics['window_error_max_samples']} 个采样周期（平均 {metrics['window_error_mean_samples']}）")
print(f"  指标二 · 归因 Top-1 一致率：{metrics['cause_top1_agreement_rate']}（命中口径 {metrics['cause_top1_hit_rate']}）")
print(f"  指标三 · 耗时：{metrics['elapsed_seconds']} 秒")
PY
  exit 0
fi

# 三、编译并跑基准。
mkdir -p bin
go build -o bin/coldchain ./cmd/coldchain

echo "开始跑基准（重复 $REPEAT 次核对可复现）……"
if ! bin/coldchain bench \
    --dataset "$DATASET" \
    --out "$OUT" \
    --repeat "$REPEAT" \
    --max-error-samples "$MAX_ERROR" \
    --max-seconds "$MAX_SECONDS" >/dev/null; then
  fail "基准未达标（详见上方输出）；报告已落盘：$OUT"
fi

# 四、把三项指标摆到台面上。
python3 - "$OUT" <<'PY'
import json, sys
report = json.load(open(sys.argv[1], encoding="utf-8"))
metrics = report["metrics"]
print()
print(f"基准数据集：{report['dataset']}（{report['records']} 行，来源 {report['dataset_path']}）")
print(f"基准时刻：{report['generated_at']}　口径容差：{report['options']['tolerance_c']}℃"
      f"（数据集自带阈值 {report['options']['annotation_tolerance_c']}℃）")
print()
print(f"指标一 · 窗口误差：最大 {metrics['window_error_max_samples']} 个采样周期"
      f"，平均 {metrics['window_error_mean_samples']} 个采样周期（上限 1）")
print(f"指标二 · 归因 Top-1 一致率：{metrics['cause_top1_agreement_rate']}"
      f"（命中口径 {metrics['cause_top1_hit_rate']}）")
print(f"指标三 · 耗时：{metrics['elapsed_seconds']} 秒（上限 30）")
print()
for stream in report["streams"]:
    print(f"  流 {stream['name']}：{stream['records']} 条，采样周期 {stream['sampling_interval_sec']}s，"
          f"温区 {stream['zone']}；窗口 {stream['windows']}，数据集标注段 {stream['ground_truth_segments']}"
          f"（对上 {stream['matched_segments']}、未对上 {stream['unmatched_segments']}），"
          f"偏差最大 {stream['window_error_max_samples']} 个采样周期")
    print(f"        归因：带成因标注窗口 {stream['cause_labeled_windows']} 条"
          f"（成因唯一 {stream['cause_single_label_windows']} 条，Top-1 一致 {stream['cause_top1_matched']} 条）")
print()
for note in report["notes"]:
    print(f"  · {note}")
print()
print(f"报告已写入：{sys.argv[1]}")
PY
