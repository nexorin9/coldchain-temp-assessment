# 公开冷链数据集取数说明

> 本文件说明「离线验证底座」的取数来源、许可证与取数记录格式。
> **公开数据集不是产品对接面** —— 产品的真实对接面是院内记录仪 / 边缘网关（入站）
> 与冷链台账、通知系统（出站）；本文档里的数据只用于**离线对照基准与性能压测**。

## 取数入口

```bash
bash scripts/fetch_datasets.sh            # 下载（缺失才下载）+ 完整性校验
bash scripts/fetch_datasets.sh --check    # 只校验本地缓存，不联网
bash scripts/fetch_datasets.sh --force    # 忽略缓存强制重下
bash scripts/fetch_datasets.sh --jobs 12  # 并行分片数（默认 8）
bash scripts/fetch_datasets.sh --help     # 全部参数
```

脚本行为的三条硬约定：

| 情形 | 行为 | 退出码 |
|---|---|---|
| 校验不通过（行数不足 / 列名不符 / CSV 不可解析 / 采集点不足） | 打印具体原因并终止 | **非 0** |
| 网络不可达 | 打印「使用 testdata/ 本地样例继续」，不写 `datasets.lock` | **0** |
| 已下载且 SHA256 匹配 | 跳过下载，直接重新校验 | **0** |

> 断网降级**不会**把空数据集写进 `datasets.lock`，也不会用 0 行文件冒充下载成功。

## 数据集清单

| 数据集 | 来源 | 许可证 | 规模 / 粒度 | 在本项目中的用途 |
|---|---|---|---|---|
| **VCC-CPLD**（Vaccine Cold-Chain Cyber-Physical Logistics Dataset） | Zenodo <https://zenodo.org/records/18527963>（DOI `10.5281/zenodo.18527963`），文件 `vaccine_coldchain.zip` | CC BY 4.0 | 445,603 条 1 分钟粒度冷链时序（主 CSV 270,152,769 字节，90 列）；含内外温与设定点、滚动 min/max/std、越限计数、开门 / 断电 / 除霜事件、湿度凝露，带 `safe_to_use_flag_t_plus_H` 与 `potency_remaining_pct_t_plus_H` 标签 | 越限窗口抽取的对照基准（窗口误差 ≤1 个采样周期）、归因 Top-1 一致率、性能压测（≥40 万条 ≤30s）；方法与实测结果见 `docs/bench.md` |
| **LogTag Nepal**（Long-range freeze-preventive vaccine carriers in Nepal: thermal data） | Mendeley Data <https://data.mendeley.com/datasets/j277dvs69g>（DOI `10.17632/j277dvs69g.1`） | CC BY 4.0 | 真实 LogTag TRIX-8 记录仪导出 CSV；526 份 CSV / 1,765,814 条数据行，覆盖数据集说明中的 24+ 个尼泊尔接种点，每点 4 个测点（冻防疫苗运输箱 / 常规疫苗运输箱 / 冰箱 / 环境温），10 分钟采样 | 真实记录仪导出形态（无表头、带 BOM、列序固定）下的越限窗口切分与记录仪文本抽取基线 |

**许可与署名**：两个数据集均为 CC BY 4.0。使用本项目随附的取数脚本或离线基准时，
请在衍生材料中署名原始数据集（VCC-CPLD: Davis, E. A., Zenodo；LogTag Nepal: Mendeley Data）并保留本表中的来源链接。

### VCC-CPLD 关键列名（离线基准与规则归因直接引用）

主 CSV 表头共 90 列。下游任务（规则归因 task 25、离线基准 task 26）按下表取列，
**列名以本表为准**——标签列带 `_t_plus_H` 后缀（表示 T+ 未来时刻的预测标签），
不要按 `safe_to_use_flag` 这类不带后缀的名字去找。

| 用途 | 实际列名 | 说明 |
|---|---|---|
| 内部温度 / 环境温度 / 设定点 | `temp_internal_c` / `temp_ambient_c` / `temp_setpoint_c` | 越限窗口抽取以 `temp_internal_c` 对温区比较 |
| 滚动窗口统计 | `temp_min_30m` / `temp_max_30m` / `temp_std_30m` | 短时波动过滤与探头卡死识别参考 |
| 温升速率 | `temp_rate_change` | 温升斜率（℃/h）特征的对照值 |
| 开门事件 | `door_open_count` / `door_open_duration_s` | `door_open` 归因候选的触发特征 |
| 断电事件 | `power_outage_count` / `power_mode` | `power_loss` 归因候选的触发特征 |
| 除霜事件 | `defrost_cycle_flag` | `defrost` 归因候选的触发特征 |
| 越限计数 | `thermal_excursion_count` | 越限窗口真值对照（Top-1 一致率口径） |
| 湿度凝露 | `humidity_internal_pct` / `humidity_ambient_pct` / `dew_point_c` | 湿度同比上升幅度特征 |
| 质量标签 | `safe_to_use_flag_t_plus_H` / `potency_remaining_pct_t_plus_H` / `potency_proxy_index` | 批次可用性标签；本系统**不据此自行判定批次可用**，只作基准对照 |
| 时间戳 | `timestamp` | 1 分钟粒度，无时区偏移 |

> **口径提醒**：标签列是数据集自带的**事后预测标签**，用于评估窗口抽取与归因的准确度，
> 不是本产品的判定依据。本产品的批次处置结论仍须由疫苗生产企业评估 + 药学部主管确认。

## 完整性校验口径

| 数据集 | 校验项 | 阈值 | 实测值 |
|---|---|---|---|
| VCC-CPLD | 主 CSV 数据行数（不含表头） | ≥ 400,000 | 445,603 |
| VCC-CPLD | 表头列名匹配（含 `safe_to_use_flag`） | 缺列即判失败并打印实际表头 | `safe_to_use_flag_t_plus_H` 存在 |
| LogTag Nepal | 每份 CSV 可解析（第 1 列为序号、第 4 列可转浮点温度） | 任一份失败即判失败并指出文件名 | 526 / 526 份通过 |
| LogTag Nepal | 采集点覆盖数（文件名中的点位 / 测点编号去重） | ≥ 24 | 150 个去重标识 |
| LogTag Nepal | 数据行合计 | — | 1,765,814 |

> **采集点口径**：文件名里的 ≥6 位数字串（如 `1000820985`）视为点位 / 测点编号，
> 无数字编号的取前导点位代码（如 `PA1PA`）并加 `TXT:` 前缀去重。
> 该口径数的是**去重标识数**（150），数据集官方说明的接种点口径是 24+ 个，两者是不同口径，勿混用。

## `data/datasets.lock` 字段说明

取数脚本成功后写入 `data/datasets.lock`（TSV，每行一条数据集记录；文件头部保留注释块）。
`data/` 属于运行时目录，不入库；本表是字段的权威说明。

| 字段 | 含义 | 示例 |
|---|---|---|
| `dataset` | 数据集名 | `vcc-cpld` |
| `source_url` | 来源 URL（数据集主页） | `https://zenodo.org/records/18527963` |
| `license` | 许可证 | `CC BY 4.0` |
| `records` | 记录行数（CSV 数据行，不含表头） | `445603` |
| `sha256` | 本地主产物的 SHA256（幂等重跑据此判定命中缓存） | `e3b0c442…` |
| `fetched_at` | 获取时间（ISO 8601，本地时区） | `2026-10-25T19:12:03+0800` |
| `local_path` | 本地主产物相对仓库根的路径 | `data/datasets/vcc-cpld/vaccine_coldchain.csv` |

字段作用一览：

- `records` —— 离线基准报告（窗口误差 / 一致率 / 耗时）里标注的样本规模，保证指标可复现。
- `sha256` —— 幂等依据：命中缓存则跳过下载、重新校验，结果一致；不匹配则重新下载并告警。
- `fetched_at` —— 记录何时取的数，便于判断基准是否需要重跑。

## 离线目录结构

```
data/datasets/
├── vaccine_coldchain.zip          # VCC-CPLD 原始压缩包（缓存下载分片）
├── vcc-cpld/                      # 解压结果；主 CSV 为行数最多的那份
├── logtag-nepal/                  # 526 份真实记录仪 CSV（文件名已安全化）
│   └── manifest.tsv               # 逐文件：文件名 / 期望 SHA256 / 实际 SHA256 / 行数 / 字节数
└── .logtag_api.json               # Mendeley 公开 API 快照（重跑幂等依据）
```

## 取数脚本实测记录

四条路径全部在本地跑过，退出码与输出如下（`COLDCHAIN_*` 环境变量用于切换阈值做反向验证）：

| 场景 | 命令 | 退出码 | 关键输出 |
|---|---|---|---|
| 只校验本地缓存 | `bash scripts/fetch_datasets.sh --check` | **0** | `vcc-cpld 校验通过：445603 行`；`logtag-nepal 校验通过：1765814 行 / 150 个采集点` |
| 幂等重跑（连跑两次） | `bash scripts/fetch_datasets.sh` × 2 | **0 / 0** | 两次均打印「已缓存且 SHA256 匹配，跳过下载」；两次非日志行输出逐字节一致 |
| 网络不可达降级 | `COLDCHAIN_DATASETS_OFFLINE=1 bash scripts/fetch_datasets.sh --data-dir /tmp/xxx --force` | **0** | 「网络不可用：使用 testdata/ 本地样例继续（未写入 datasets.lock，不以空数据集冒充成功）」；`datasets.lock` 校验和不变，目标目录 0 个文件 |
| 校验不通过（行数不足） | `COLDCHAIN_VCC_MIN_ROWS=999999999 bash scripts/fetch_datasets.sh --check` | **1** | 「VCC-CPLD 行数不足：445603 < 999999999」+「完整性校验未通过」 |
| 校验不通过（列名不匹配） | `COLDCHAIN_VCC_REQUIRED_COLUMNS=definitely_not_a_column bash scripts/fetch_datasets.sh --check` | **1** | 「列名不匹配，缺少期望字段」+ 打印实际 90 列表头 |
| 校验不通过（采集点不足） | `COLDCHAIN_LOGTAG_MIN_POINTS=99999 bash scripts/fetch_datasets.sh --check` | **1** | 「LogTag 采集点不足：150 < 99999」 |

> 三条失败路径均**只读校验、绝不改写** `data/datasets.lock`，也不产出空文件占位。

### 断网降级后怎么办

断网时数据集拿不到，**离线基准与性能压测（task 26）就无从谈起**——此时只能跑
`testdata/` 下的仓库内脱敏样例（task 5 落盘），并在基准报告里注明「数据集未取到，指标未复现」。
**禁止**用样例行数冒充 40 万条规模、也禁止用样例指标冒充数据集指标。

### 离线对照基准的实测结果

数据集取到后，跑 `bash scripts/bench_window.sh` 得到本轮基准的三项指标
（窗口误差 0 个采样周期、归因 Top-1 一致率 0.48、耗时 15.99 秒 / 445,603 条）。
指标口径、拼接流的边界、成因标注的映射差异与「结论怎么读」见 **`docs/bench.md`**。
