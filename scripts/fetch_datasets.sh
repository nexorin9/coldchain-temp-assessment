#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# fetch_datasets.sh —— 公开冷链数据集的下载、完整性校验与断网降级
#
# 本脚本是「离线验证底座」的取数入口，不是产品对接面：
#   * VCC-CPLD  : 疫苗冷链「赛博-物理」数据集（Zenodo，CC BY 4.0）
#                  445,603 条 1 分钟粒度冷链时序，带 safe_to_use_flag 标签
#                  → 用于越限窗口抽取的对照基准与归因 Top-1 一致率
#   * LogTag NP : 尼泊尔长程冻防疫苗运输箱热数据（Mendeley Data，CC BY 4.0）
#                  真实 LogTag TRIX-8 记录仪导出 CSV，24+ 个接种点
#                  → 用于真实记录仪文本/CSV 形态的窗口切分基线
#
# 三个硬性行为约定：
#   1. 校验不通过 → 非 0 退出（行数不足、列名不符、CSV 不可解析、采集点不足）
#   2. 断网 → 打印「使用 testdata/ 本地样例继续」并以退出码 0 结束，
#      且绝不把空数据集写进 datasets.lock 冒充成功
#   3. 幂等重跑 → 已下载且 SHA256 匹配时跳过下载直接校验，结果一致
#
# 用法：
#   bash scripts/fetch_datasets.sh                 # 下载（缺失才下载）+ 校验
#   bash scripts/fetch_datasets.sh --check        # 只校验本地缓存，不联网
#   bash scripts/fetch_datasets.sh --force        # 忽略缓存强制重下
#   bash scripts/fetch_datasets.sh --jobs 12      # 并行分片数（默认 8）
#   bash scripts/fetch_datasets.sh --max-logtag-files 60   # LogTag 最多下 N 个文件
#
# 字段说明见 docs/datasets.md；取数记录写入 data/datasets.lock（TSV）。
# ---------------------------------------------------------------------------
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ---- 默认参数 ---------------------------------------------------------------
DATA_DIR="${REPO_ROOT}/data/datasets"
LOCK_FILE="${REPO_ROOT}/data/datasets.lock"
CHECK_ONLY=0
FORCE=0
JOBS=8
TIMEOUT=120
MAX_LOGTAG_FILES=0        # 0 = 全部
SKIP_VCC=0
SKIP_LOGTAG=0
PROBE_TIMEOUT=15

# ---- 数据集登记（来源 URL / 许可证 / 校验口径）------------------------------
# 以下取值均可用环境变量覆盖（便于离线演练与镜像源切换）
VCC_NAME="vcc-cpld"
VCC_PAGE="${COLDCHAIN_VCC_PAGE_URL:-https://zenodo.org/records/18527963}"
VCC_API="${COLDCHAIN_VCC_API_URL:-https://zenodo.org/api/records/18527963}"
VCC_FILE_URL="${COLDCHAIN_VCC_FILE_URL:-https://zenodo.org/api/records/18527963/files/vaccine_coldchain.zip/content}"
VCC_LICENSE="CC BY 4.0"
VCC_MIN_ROWS="${COLDCHAIN_VCC_MIN_ROWS:-400000}"
# 期望列名（VCC-CPLD 表头，逗号分隔）：任一列名缺失即判定列名不匹配
# 注：按「子串」匹配——数据集里的标签列实际名为 safe_to_use_flag_t_plus_H（T+ 时刻预测标签），
#     以 safe_to_use_flag 作为期望名可同时容纳两种命名。详见 docs/datasets.md。
VCC_REQUIRED_COLUMNS="${COLDCHAIN_VCC_REQUIRED_COLUMNS:-safe_to_use_flag}"

LOGTAG_NAME="logtag-nepal"
LOGTAG_PAGE="${COLDCHAIN_LOGTAG_PAGE_URL:-https://data.mendeley.com/datasets/j277dvs69g}"
LOGTAG_API="${COLDCHAIN_LOGTAG_API_URL:-https://data.mendeley.com/public-api/datasets/j277dvs69g}"
LOGTAG_LICENSE="CC BY 4.0"
LOGTAG_MIN_POINTS="${COLDCHAIN_LOGTAG_MIN_POINTS:-24}"
# 置 1 可强制走「断网降级」路径（离线演练用）
FORCE_OFFLINE="${COLDCHAIN_DATASETS_OFFLINE:-0}"

usage() {
  sed -n '2,35p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
  exit 0
}

while [ $# -gt 0 ]; do
  case "$1" in
    --check)             CHECK_ONLY=1 ;;
    --force)             FORCE=1 ;;
    --jobs)              JOBS="${2:-8}"; shift ;;
    --data-dir)          DATA_DIR="${2:-$DATA_DIR}"; shift ;;
    --timeout)           TIMEOUT="${2:-120}"; shift ;;
    --max-logtag-files)  MAX_LOGTAG_FILES="${2:-0}"; shift ;;
    --skip-vcc)          SKIP_VCC=1 ;;
    --skip-logtag)       SKIP_LOGTAG=1 ;;
    -h|--help)           usage ;;
    *) echo "未知参数：$1（--help 查看用法）" >&2; exit 2 ;;
  esac
  shift
done

mkdir -p "${DATA_DIR}"

# ---- 输出helpers ------------------------------------------------------------
log()  { printf '[datasets] %s\n' "$*"; }
warn() { printf '[datasets] 警告：%s\n' "$*" >&2; }
err()  { printf '[datasets] 错误：%s\n' "$*" >&2; }

sha256_of() {
  if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}';
  else sha256sum "$1" | awk '{print $1}'; fi
}

now_iso() { date '+%Y-%m-%dT%H:%M:%S%z'; }

# ---- 网络可达性探针 ---------------------------------------------------------
host_reachable() {
  # host_reachable <url> → 0 可达 / 1 不可达
  if [ "${FORCE_OFFLINE}" = "1" ]; then return 1; fi
  local code
  code="$(curl -sS -o /dev/null -m "${PROBE_TIMEOUT}" -w '%{http_code}' "$1" 2>/dev/null)"
  [ -n "${code}" ] && [ "${code}" != "000" ] && [ "${code}" != "0000" ]
}

http_total_size() {
  # 通过 Range 探针取资源总字节数（比 HEAD 更可靠）
  local hdr
  hdr="$(curl -sS -m "${PROBE_TIMEOUT}" -D - -o /dev/null -r 0-0 "$1" 2>/dev/null \
         | tr -d '\r' | grep -i '^content-range:' | awk -F'/' '{print $2}' | tr -d ' ')"
  case "${hdr}" in
    ''|*[!0-9]*) return 1 ;;
    *) printf '%s' "${hdr}" ;;
  esac
}

# ---- 下载 -------------------------------------------------------------------
download_plain() {
  # download_plain <url> <out>
  curl -fsSL --retry 3 --retry-delay 3 --connect-timeout 15 --max-time "${TIMEOUT}" \
       -o "$2" "$1"
}

fetch_range_chunk() {
  # fetch_range_chunk <url> <part_file> <start> <end> <hdr_file>
  # 单个分片的可续传下载：已下载部分保留，从断点继续，直到拿满区间或超出重试上限
  local url="$1" part="$2" start="$3" end="$4" hdr="$5"
  local expected=$(( end - start + 1 ))
  local attempt=0 have=0 abs=0
  while [ "${attempt}" -lt 40 ]; do
    have=0
    [ -f "${part}" ] && have="$(wc -c < "${part}" | tr -d ' ')"
    if [ "${have}" -ge "${expected}" ]; then return 0; fi
    if [ "${have}" -gt "${expected}" ]; then
      warn "分片 $(basename "${part}") 字节数异常（${have} > ${expected}），丢弃重来"
      rm -f "${part}"
      continue
    fi
    abs=$(( start + have ))
    rm -f "${hdr}.body"
    curl -fsSL --speed-limit 1024 --speed-time 120 --connect-timeout 20 \
         --max-time 1800 -r "${abs}-${end}" -D "${hdr}" -o "${hdr}.body" "$url" 2>/dev/null
    if grep -qi "content-range: bytes ${abs}-" "${hdr}" 2>/dev/null; then
      if [ -s "${hdr}.body" ]; then
        cat "${hdr}.body" >> "${part}"
      fi
    else
      warn "分片 $(basename "${part}") 返回的区间不是 ${abs}-${end}，丢弃本次响应"
    fi
    attempt=$(( attempt + 1 ))
  done
  have=0
  [ -f "${part}" ] && have="$(wc -c < "${part}" | tr -d ' ')"
  [ "${have}" -ge "${expected}" ]
}

download_parallel() {
  # download_parallel <url> <out> <total_size> —— 分片并行 + 断点续传 + 按序拼接
  local url="$1" out="$2" total="$3"
  local chunk_size=$(( (total + JOBS - 1) / JOBS ))
  local tmpdir="${out}.parts"
  local plan="${tmpdir}/plan"
  if [ -f "${plan}" ] && [ "$(cat "${plan}")" = "${total}:${JOBS}" ] && [ "${FORCE}" -eq 0 ]; then
    log "复用已有分片（断点续传）"
  else
    rm -rf "${tmpdir}"; mkdir -p "${tmpdir}"
    printf '%s:%s' "${total}" "${JOBS}" > "${plan}"
  fi

  log "分片下载（${JOBS} 片，每片约 $(( chunk_size / 1024 )) KiB，可断点续传）…"
  local i start end
  i=0
  while [ "${i}" -lt "${JOBS}" ]; do
    start=$(( i * chunk_size ))
    end=$(( start + chunk_size - 1 ))
    [ "${end}" -ge "${total}" ] && end=$(( total - 1 ))
    if [ "${start}" -gt $(( total - 1 )) ]; then break; fi
    fetch_range_chunk "${url}" "${tmpdir}/part.${i}" "${start}" "${end}" "${tmpdir}/hdr.${i}" &
    i=$(( i + 1 ))
  done
  wait

  : > "${out}.tmp"
  i=0
  while [ "${i}" -lt "${JOBS}" ]; do
    if [ -f "${tmpdir}/part.${i}" ]; then
      cat "${tmpdir}/part.${i}" >> "${out}.tmp"
    fi
    i=$(( i + 1 ))
  done
  local got
  got="$(wc -c < "${out}.tmp" | tr -d ' ')"
  if [ "${got}" != "${total}" ]; then
    rm -f "${out}.tmp"
    err "分片下载不完整：期望 ${total} 字节，实得 ${got} 字节（可重跑本脚本续传）"
    return 1
  fi
  mv "${out}.tmp" "${out}"
  rm -rf "${tmpdir}"
  return 0
}

# ---- datasets.lock 读写 -----------------------------------------------------
lock_get() {
  # lock_get <dataset> <field> —— 锁文件是按列排列的 TSV，字段名映射到列号
  [ -f "${LOCK_FILE}" ] || return 1
  local idx
  case "$2" in
    source_url) idx=2 ;;
    license)    idx=3 ;;
    records)    idx=4 ;;
    sha256)     idx=5 ;;
    fetched_at) idx=6 ;;
    local_path) idx=7 ;;
    *)          idx=1 ;;
  esac
  awk -F'\t' -v ds="$1" -v i="${idx}" '$1 == ds { print $i; exit }' "${LOCK_FILE}"
}

lock_write() {
  # lock_write <dataset> <source_url> <license> <records> <sha256> <fetched_at> <local_path>
  local ds="$1" url="$2" lic="$3" rec="$4" sum="$5" at="$6" path="$7"
  mkdir -p "$(dirname "${LOCK_FILE}")"
  if [ ! -f "${LOCK_FILE}" ]; then
    {
      echo "# coldchain-temp-assessment 公开冷链数据集取数记录（由 scripts/fetch_datasets.sh 自动生成，勿手改）"
      echo "#"
      echo "# 字段说明（每行一条记录，制表符分隔）："
      echo "#   dataset      数据集名（vcc-cpld / logtag-nepal）"
      echo "#   source_url   来源 URL（数据集主页）"
      echo "#   license      许可证（CC BY 4.0 等，随数据集发布许可）"
      echo "#   records      记录行数（CSV 数据行，不含表头）"
      echo "#   sha256       本地主产物文件的 SHA256（幂等重跑据此判定命中缓存）"
      echo "#   fetched_at   获取时间（ISO 8601，本地时区）"
      echo "#   local_path   本地主产物相对仓库根的路径"
      echo "#"
      echo "# 列顺序：dataset<TAB>source_url<TAB>license<TAB>records<TAB>sha256<TAB>fetched_at<TAB>local_path"
    } > "${LOCK_FILE}"
  fi
  # 覆盖同数据集旧记录（先删后写，保证每数据集只有一行）
  awk -F'\t' -v ds="${ds}" 'BEGIN{OFS="\t"} $1 != ds && $0 !~ /^#/ {print}' "${LOCK_FILE}" > "${LOCK_FILE}.new"
  mv "${LOCK_FILE}.new" "${LOCK_FILE}"
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "${ds}" "${url}" "${lic}" "${rec}" "${sum}" "${at}" "${path}" >> "${LOCK_FILE}"
}

# ---- VCC-CPLD 校验 ----------------------------------------------------------
validate_vcc_csv() {
  # validate_vcc_csv <csv> → 打印行数；不通过返回 1
  local csv="$1" rows header missing col
  if [ ! -s "${csv}" ]; then err "VCC-CPLD 主 CSV 为空：${csv}"; return 1; fi
  rows="$(awk 'END{print NR-1}' "${csv}" | tr -d ' ')"
  header="$(head -n 1 "${csv}" | tr -d '\r')"
  missing=""
  local IFS_OLD="${IFS}"
  IFS=','
  for col in ${VCC_REQUIRED_COLUMNS}; do
    if ! printf '%s' "${header}" | grep -qi "${col}"; then missing="${missing}${col} "; fi
  done
  IFS="${IFS_OLD}"
  if [ -n "${missing}" ]; then
    err "VCC-CPLD 列名不匹配，缺少期望字段：${missing}"
    err "  实际表头：${header}"
    return 1
  fi
  if [ "${rows}" -lt "${VCC_MIN_ROWS}" ]; then
    err "VCC-CPLD 行数不足：${rows} < ${VCC_MIN_ROWS}"
    return 1
  fi
  printf '%s' "${rows}"
  return 0
}

# ---- LogTag 校验 ------------------------------------------------------------
download_one_logtag() {
  # download_one_logtag <job> <dir>；job 形如「安全文件名|URL|期望 SHA256」
  local job="$1" dir="$2"
  local safe url sha have
  safe="${job%%|*}"
  job="${job#*|}"
  url="${job%%|*}"
  sha="${job#*|}"
  if [ -z "${url}" ]; then return 1; fi
  if [ -f "${dir}/${safe}" ] && [ -n "${sha}" ]; then
    have="$(sha256_of "${dir}/${safe}")"
    if [ "${have}" = "${sha}" ]; then return 0; fi
  fi
  if ! curl -fsSL --retry 3 --retry-delay 2 --connect-timeout 15 --max-time 300 \
       -o "${dir}/${safe}" "${url}"; then
    rm -f "${dir}/${safe}"
    return 1
  fi
  return 0
}

logtag_rows() {
  # logtag_rows <file> → 该 CSV 的数据行数
  # 真实记录仪导出有多种形态，本项目在同一批文件里实际观察到：
  #   A) 无表头直接数据行：1,2019-04-30,11:07:59,31.2,          （部分带 BOM、CRLF）
  #   B) 带说明行再跟数据行：首行 “Note: All Date-Time values …”   （同样可能带 BOM/CRLF）
  #   C) 带表头 + 引号 + 12 小时制 + M/D/YYYY：Index,Date,Time,Readings (°C),Type
  # 因此只认「不变式」：第 1 列是序号、第 4 列是温度；日期与时刻的写法不参与判定。
  # 注意：部分文件是 GBK 编码（含中文点位名），必须 LC_ALL=C 让 awk 按字节处理，
  # 否则 awk 会因多字节转换失败直接中断，该文件的数据行数将统计不出来。
  LC_ALL=C awk -F',' '
    { gsub(/\r/, "", $4); gsub(/"/, "", $4) }
    $1 ~ /^[0-9]+$/ && $4 ~ /^-?[0-9]+(\.[0-9]+)?$/ { n++ }
    END { print n+0 }
  ' "$1"
}

validate_logtag_dir() {
  # validate_logtag_dir <dir> → 打印 "行数 采集点数"；不通过返回 1
  local dir="$1"
  local files total_rows points
  files="$(find "${dir}" -maxdepth 1 -name '*.csv' -type f | sort)"
  if [ -z "${files}" ]; then err "LogTag 目录内没有任何 CSV：${dir}"; return 1; fi
  total_rows=0
  # 采集点口径：文件名里长度 ≥6 的数字串即接种点编号（如 1000820985）；
  # 没有编号的用前导点位代码（如 PA1PA / DA6DMahabharat）。
  # 不用「前导数字」是因为有 138 份文件以字母代码开头（且部分编号在文件名中段）。
  points="$(printf '%s\n' "${files}" | LC_ALL=C awk '
    { n = $0; sub(/^.*\//, "", n); sub(/\.csv$/, "", n);
      if (match(n, /[0-9][0-9][0-9][0-9][0-9][0-9]+/)) { print substr(n, RSTART, RLENGTH) }
      else { t = n; sub(/_.*$/, "", t); sub(/Started$/, "", t); sub(/Finished$/, "", t); sub(/-+$/, "", t);
             if (t == "") t = n; print "TXT:" t } }
  ' | sort -u | wc -l | tr -d ' ')"
  local f n
  for f in ${files}; do
    # 每份 CSV 必须可解析：至少能识别出一行「序号,日期,时刻,温度」形态的数据行
    n="$(logtag_rows "${f}")"
    if [ "${n}" -lt 1 ]; then
      err "LogTag CSV 不可解析（未找到「序号,日期,时刻,温度」数据行）：$(basename "${f}")"
      return 1
    fi
    total_rows=$(( total_rows + n ))
  done
  if [ "${points}" -lt "${LOGTAG_MIN_POINTS}" ]; then
    err "LogTag 采集点不足：${points} < ${LOGTAG_MIN_POINTS}"
    return 1
  fi
  printf '%s %s' "${total_rows}" "${points}"
  return 0
}

# =============================================================================
# 主流程
# =============================================================================
OFFLINE=0
FAILED=0

# ---------------------------- VCC-CPLD ----------------------------------------
fetch_vcc() {
  local zip_path="${DATA_DIR}/vaccine_coldchain.zip"
  local out_dir="${DATA_DIR}/vcc-cpld"
  local main_csv="" cached_sum locked_sum

  if [ "${SKIP_VCC}" -eq 1 ]; then log "跳过 ${VCC_NAME}（--skip-vcc）"; return 0; fi

  # 幂等：锁定的主产物仍在且 SHA256 匹配 → 跳过下载，直接校验
  locked_sum="$(lock_get "${VCC_NAME}" sha256)"
  if [ "${CHECK_ONLY}" -eq 0 ] && [ "${FORCE}" -eq 0 ] && [ -n "${locked_sum}" ]; then
    main_csv="$(lock_get "${VCC_NAME}" local_path)"
    if [ -f "${REPO_ROOT}/${main_csv}" ]; then
      cached_sum="$(sha256_of "${REPO_ROOT}/${main_csv}")"
      if [ "${cached_sum}" = "${locked_sum}" ]; then
        log "${VCC_NAME} 已缓存且 SHA256 匹配，跳过下载"
        local rows
        rows="$(validate_vcc_csv "${REPO_ROOT}/${main_csv}")" || { FAILED=1; return 1; }
        log "${VCC_NAME} 校验通过：${rows} 行（主产物 ${main_csv}）"
        return 0
      fi
      warn "${VCC_NAME} 缓存 SHA256 不匹配（期望 ${locked_sum:0:12}… 实得 ${cached_sum:0:12}…），将重新下载"
    fi
  fi

  if [ "${CHECK_ONLY}" -eq 1 ]; then
    main_csv="$(lock_get "${VCC_NAME}" local_path)"
    if [ -z "${main_csv}" ] || [ ! -f "${REPO_ROOT}/${main_csv}" ]; then
      err "--check 模式：本地没有 ${VCC_NAME} 主产物，请先运行不带 --check 的下载"
      FAILED=1
      return 1
    fi
    local rows
    rows="$(validate_vcc_csv "${REPO_ROOT}/${main_csv}")" || { FAILED=1; return 1; }
    log "${VCC_NAME} 校验通过：${rows} 行"
    return 0
  fi

  if ! host_reachable "${VCC_API}"; then
    warn "${VCC_NAME} 来源不可达（网络不可用）"
    OFFLINE=1
    return 0
  fi

  log "下载 ${VCC_NAME}（${VCC_PAGE}，${VCC_LICENSE}）…"
  local total
  if ! total="$(http_total_size "${VCC_FILE_URL}")"; then
    warn "取不到 ${VCC_NAME} 文件总大小，改用单流下载"
    if ! download_plain "${VCC_FILE_URL}" "${zip_path}.tmp"; then
      err "${VCC_NAME} 下载失败"
      FAILED=1
      return 1
    fi
    mv "${zip_path}.tmp" "${zip_path}"
  else
    log "文件总大小 ${total} 字节"
    local cached_size=0
    [ -f "${zip_path}" ] && cached_size="$(wc -c < "${zip_path}" | tr -d ' ')"
    if [ "${FORCE}" -eq 0 ] && [ "${cached_size}" = "${total}" ]; then
      log "压缩包已缓存且大小一致（${total} 字节），跳过下载"
    elif ! download_parallel "${VCC_FILE_URL}" "${zip_path}" "${total}"; then
      err "${VCC_NAME} 分片下载失败"
      FAILED=1
      return 1
    fi
  fi

  rm -rf "${out_dir}"; mkdir -p "${out_dir}"
  if ! unzip -o -q "${zip_path}" -d "${out_dir}"; then
    err "${VCC_NAME} 解压失败（文件可能不完整）"
    FAILED=1
    return 1
  fi
  # 主 CSV = 解压目录内行数最多的 .csv
  main_csv="$(find "${out_dir}" -name '*.csv' -type f -exec wc -l {} \; | sort -rn | head -n 1 | awk '{print $2}')"
  if [ -z "${main_csv}" ]; then
    err "${VCC_NAME} 解压后未找到 CSV"
    FAILED=1
    return 1
  fi
  local rel rows
  rel="$(cd "${REPO_ROOT}" && echo "${main_csv}" | sed "s#^${REPO_ROOT}/##")"
  rows="$(validate_vcc_csv "${main_csv}")" || { FAILED=1; return 1; }
  lock_write "${VCC_NAME}" "${VCC_PAGE}" "${VCC_LICENSE}" "${rows}" "$(sha256_of "${main_csv}")" "$(now_iso)" "${rel}"
  log "${VCC_NAME} 下载并校验通过：${rows} 行（${rel}）"
  return 0
}

# ---------------------------- LogTag -----------------------------------------
fetch_logtag() {
  local dir="${DATA_DIR}/logtag-nepal"
  local manifest="${dir}/manifest.tsv"
  local cached_sum locked_sum

  if [ "${SKIP_LOGTAG}" -eq 1 ]; then log "跳过 ${LOGTAG_NAME}（--skip-logtag）"; return 0; fi

  locked_sum="$(lock_get "${LOGTAG_NAME}" sha256)"
  if [ "${CHECK_ONLY}" -eq 0 ] && [ "${FORCE}" -eq 0 ] && [ -n "${locked_sum}" ] && [ -f "${manifest}" ]; then
    cached_sum="$(sha256_of "${manifest}")"
    if [ "${cached_sum}" = "${locked_sum}" ]; then
      log "${LOGTAG_NAME} 已缓存且 SHA256 匹配，跳过下载"
      local res rows points
      res="$(validate_logtag_dir "${dir}")" || { FAILED=1; return 1; }
      rows="${res%% *}"; points="${res##* }"
      log "${LOGTAG_NAME} 校验通过：${rows} 行 / ${points} 个采集点"
      return 0
    fi
    warn "${LOGTAG_NAME} 清单 SHA256 不匹配，将重新下载"
  fi

  if [ "${CHECK_ONLY}" -eq 1 ]; then
    if [ ! -d "${dir}" ]; then
      err "--check 模式：本地没有 ${LOGTAG_NAME} 目录，请先运行不带 --check 的下载"
      FAILED=1
      return 1
    fi
    local res rows points
    res="$(validate_logtag_dir "${dir}")" || { FAILED=1; return 1; }
    rows="${res%% *}"; points="${res##* }"
    log "${LOGTAG_NAME} 校验通过：${rows} 行 / ${points} 个采集点"
    return 0
  fi

  if ! host_reachable "${LOGTAG_API}"; then
    warn "${LOGTAG_NAME} 来源不可达（网络不可用）"
    OFFLINE=1
    return 0
  fi

  log "拉取 ${LOGTAG_NAME} 文件清单（${LOGTAG_PAGE}，${LOGTAG_LICENSE}）…"
  local api_json="${DATA_DIR}/.logtag_api.json"
  if ! curl -fsSL --retry 2 --connect-timeout 15 --max-time "${TIMEOUT}" -o "${api_json}" "${LOGTAG_API}"; then
    err "${LOGTAG_NAME} 文件清单拉取失败"
    FAILED=1
    return 1
  fi

  # 保留已下载文件以便续传（逐文件按 SHA256 判定是否需要重下）；--force 才清空
  if [ "${FORCE}" -eq 1 ]; then rm -rf "${dir}"; fi
  mkdir -p "${dir}"
  # 清单：filename<TAB>download_url<TAB>sha256（来自 Mendeley 公开 API 的 content_details）
  local listing="${DATA_DIR}/.logtag_listing.tsv"
  if command -v jq >/dev/null 2>&1; then
    jq -r '.files[] | select(.filename | endswith(".csv")) | [.filename, .content_details.download_url, .content_details.sha256_hash] | @tsv' \
      "${api_json}" > "${listing}"
  else
    python3 - "${api_json}" > "${listing}" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
for f in d.get("files") or []:
    if not f.get("filename", "").lower().endswith(".csv"):
        continue
    cd = f.get("content_details") or {}
    print("\t".join([f["filename"], cd.get("download_url", ""), cd.get("sha256_hash", "")]))
PY
  fi
  local total_files
  total_files="$(wc -l < "${listing}" | tr -d ' ')"
  if [ "${MAX_LOGTAG_FILES}" -gt 0 ] && [ "${total_files}" -gt "${MAX_LOGTAG_FILES}" ]; then
    head -n "${MAX_LOGTAG_FILES}" "${listing}" > "${listing}.cut"
    mv "${listing}.cut" "${listing}"
    total_files="${MAX_LOGTAG_FILES}"
    log "按 --max-logtag-files 限制为 ${total_files} 个文件"
  fi
  log "共 ${total_files} 个 LogTag CSV 待下载（并行 ${JOBS}）…"

  # 并行下载：文件名做安全化处理，避免空格与逗号
  local safe_list="${DATA_DIR}/.logtag_safe.tsv"
  : > "${safe_list}"
  local fname furl fsha safe
  while IFS="$(printf '\t')" read -r fname furl fsha; do
    [ -z "${fname}" ] && continue
    safe="$(printf '%s' "${fname}" | tr -cs 'A-Za-z0-9._-' '_')"
    printf '%s\t%s\t%s\t%s\n' "${safe}" "${furl}" "${fsha}" "${fname}" >> "${safe_list}"
  done < "${listing}"

  # 交给 xargs 的行放「安全文件名 | URL | 期望 SHA256」三段。
  # 分隔符用竖线：BSD xargs 的 -I 模式会把输入行里的制表符折成空格，竖线不会被折。
  local jobs_file="${DATA_DIR}/.logtag_jobs"
  : > "${jobs_file}"
  local fname furl fsha safe
  while IFS="$(printf '\t')" read -r fname furl fsha; do
    [ -z "${fname}" ] && continue
    safe="$(printf '%s' "${fname}" | tr -cs 'A-Za-z0-9._-' '_')"
    printf '%s|%s|%s\n' "${safe}" "${furl}" "${fsha}" >> "${jobs_file}"
  done < "${listing}"

  # 已有且 SHA256 与 Mendeley 公布值一致的文件直接跳过（断点续传，重跑不重复下载）
  # 用 bash 后台作业分批并发，不用 xargs：BSD xargs 在 -I 模式下组装长参数列表会报
  # “command line cannot be assembled, too long” 并中断剩余任务。
  local batch=0
  while IFS= read -r job; do
    [ -z "${job}" ] && continue
    download_one_logtag "${job}" "${dir}" &
    batch=$(( batch + 1 ))
    if [ $(( batch % JOBS )) -eq 0 ]; then wait; fi
  done < "${jobs_file}"
  wait

  # 逐文件核对 Mendeley 提供的 SHA256，生成清单
  local manifest_tmp="${dir}/.manifest.tmp"
  printf 'file\tsha256_expected\tsha256_actual\trows\tbytes\n' > "${manifest_tmp}"
  local ok=0 bad=0 rows bytes
  while IFS="$(printf '\t')" read -r safe furl fsha orig; do
    [ -z "${safe}" ] && continue
    if [ ! -f "${dir}/${safe}" ]; then bad=$(( bad + 1 )); continue; fi
    bytes="$(wc -c < "${dir}/${safe}" | tr -d ' ')"
    rows="$(logtag_rows "${dir}/${safe}")"
    local actual
    actual="$(sha256_of "${dir}/${safe}")"
    if [ -n "${fsha}" ] && [ "${actual}" != "${fsha}" ]; then
      warn "LogTag 文件 SHA256 不匹配：${safe}"
      bad=$(( bad + 1 ))
      continue
    fi
    printf '%s\t%s\t%s\t%s\t%s\n' "${safe}" "${fsha}" "${actual}" "${rows}" "${bytes}" >> "${manifest_tmp}"
    ok=$(( ok + 1 ))
  done < "${safe_list}"
  mv "${manifest_tmp}" "${manifest}"
  log "下载完成：${ok} 个文件校验通过，${bad} 个失败"

  local res rows points
  res="$(validate_logtag_dir "${dir}")" || { FAILED=1; return 1; }
  rows="${res%% *}"; points="${res##* }"
  local rel
  rel="$(cd "${REPO_ROOT}" && echo "${manifest}" | sed "s#^${REPO_ROOT}/##")"
  lock_write "${LOGTAG_NAME}" "${LOGTAG_PAGE}" "${LOGTAG_LICENSE}" "${rows}" "$(sha256_of "${manifest}")" "$(now_iso)" "${rel}"
  log "${LOGTAG_NAME} 下载并校验通过：${rows} 行 / ${points} 个采集点"
  return 0
}

log "数据目录：${DATA_DIR}"
log "取数记录：${LOCK_FILE}"

fetch_vcc
fetch_logtag

echo
if [ "${OFFLINE}" -eq 1 ]; then
  warn "网络不可用：使用 testdata/ 本地样例继续（未写入 datasets.lock，不以空数据集冒充成功）"
  log "断网降级完成"
  exit 0
fi

if [ "${FAILED}" -ne 0 ]; then
  err "完整性校验未通过（详见上方错误信息）"
  exit 1
fi

if [ -f "${LOCK_FILE}" ]; then
  echo "---- ${LOCK_FILE} ----"
  grep -v '^#' "${LOCK_FILE}"
  echo "---------------------"
fi
log "全部数据集就绪"
exit 0
