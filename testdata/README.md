# testdata —— 脱敏样例（测试数据层）

本目录是本仓库**唯一的测试数据层**。里面的文件全部是脱敏替身，只服务于单元测试与
本地端到端验证，**不是产品的对接面**：现场的真实数据来自记录仪或院内边缘网关的入站
接口，在库批次清单来自冷链台账的查询接口，两者都不在本目录里。

> **边界声明**：本目录样例不得被当作「系统已经接上了台账 / 记录仪」的依据。
> 真实接入面的协议、字段与凭据位见项目 README 的「对接现有系统」一节与 `spec.md`。

## 文件清单

| 文件 | 行数 | 用途 |
|------|------|------|
| `recordings_sample.csv` | 85（表头 1 + 数据 84） | 脱敏记录仪导出样例：5 分钟粒度温度序列，含 3 段越温窗口与门开 / 断电 / 除霜事件行 |
| `batches_sample.csv` | 5（表头 1 + 数据 4） | 在库批次清单样例：批次号与效期均为占位值 |
| `point_profile.json` | 16 | 点位温区配置样例（2～8℃、采样周期 300 秒、最短越限 10 分钟、迟滞 1℃、精度上限 0.5℃） |
| `single_event.json` | 14 | 单条温度事件样例（JSON 线格式），用于 `coldchain assess --events` 的最小输入 |
| `series_excursion.json` | 100 | 一段越温序列样例（JSON 数组），用于 `coldchain assess --events --profile` |
| `series_data_gap.json` | 72 | 脏数据形态样例：03:10 后中断 40 分钟、03:50 补上（数据缺口） |
| `series_clock_regression.json` | 86 | 脏数据形态样例：10:10 之后时钟回到 09:55（时钟回拨），排序后时间轴自身重叠 |
| `series_probe_stuck.json` | 100 | 脏数据形态样例：04:10 起连续 4 条读数恒为 9.4℃（探头卡死） |
| `series_merged.json` | 100 | 相邻越限合并样例：两段各 10 分钟的越限，中间只回落 5 分钟（低于门槛 10 分钟）→ 合成一条窗口 |
| `series_transient.json` | 100 | 短时波动样例：两段各 5 分钟的越限（未达门槛 10 分钟）→ 不生成窗口，单列为短时波动 |

后三份样例对应现场最常见的三种脏数据形态，用来验证「脏数据不被算成越温」：

```bash
go run ./cmd/coldchain assess --events testdata/series_data_gap.json         --profile testdata/point_profile.json
go run ./cmd/coldchain assess --events testdata/series_clock_regression.json --profile testdata/point_profile.json
go run ./cmd/coldchain assess --events testdata/series_probe_stuck.json       --profile testdata/point_profile.json
```

预期分别看到：缺口标 `breakpoint_count=1` 且断线时间不计入持续分钟；
回拨标 `clock_regressions` 且窗口按记录仪序起止；卡死标 `probe_stuck_spans`
且该段读数不进温升速率。三份样例的点位与 `point_profile.json` 一致。

### 短时波动与相邻合并样例

这两份样例验证的是「一次异常只记一条、波动单独列出」这条口径：

```bash
go run ./cmd/coldchain assess --events testdata/series_merged.json     --profile testdata/point_profile.json
go run ./cmd/coldchain assess --events testdata/series_transient.json  --profile testdata/point_profile.json
```

| 样例 | 编排 | 预期结论 |
|------|------|----------|
| `series_merged.json` | 03:05–03:15 越限（峰值 10.0℃）、03:20 回落到 6.0℃、03:25–03:35 再越限（峰值 10.5℃）；两段各 10 分钟，中间只回落 5 分钟 | 只出 **1 条**窗口：起止 03:05~03:35、持续 20 分钟、峰值 10.5℃@03:35、`segment_count=2`、`recovery_minutes=5`，分段明细在 `segments` 里 |
| `series_transient.json` | 03:10–03:15 越限（峰值 9.1℃）、03:30–03:35 越限（峰值 8.8℃）；两段各 5 分钟 | `window` 为 `null`；`transient_excursions` 列出 2 段波动，命令行摘要逐条打印，不说成「压根没越限」 |

两份样例的点位与 `point_profile.json` 一致（`point-vax-freezer-01`），
温区 2~8℃、最短越限 10 分钟、迟滞 1℃。

`recordings_sample.csv` 与 `point_profile.json` 的点位一致（`point-vax-freezer-01`），
两者可以拼在一起喂进判定主路径：

- **命令行**：当前 `coldchain assess` 读的是 JSON 线格式样例，已可直接复跑：

  ```bash
  go run ./cmd/coldchain assess \
    --events testdata/series_excursion.json \
    --profile testdata/point_profile.json
  ```

- **单测 / 程序内**：`internal/fixture` 的加载器读 CSV 样例，`go test ./internal/fixture/...`
  会把这两份样例拼起来跑一遍判定主路径，并断言抽出 3 段越温窗口。

记录仪 CSV 的批量导入入口（`POST /v1/imports/temperature-series`）在后续任务落地；
在那之前，CSV 样例由 `internal/fixture` 读取，不走 HTTP 入站。

## 脱敏方式

| 处理 | 做法 |
|------|------|
| 点位 / 设备标识 | 一律用占位名（`point-vax-freezer-01`、`device-recorder-0001`），不对应任何真实冷库或记录仪 |
| 设备名称 | 写通用中文名「疫苗冷藏库温度记录仪」，不含院区、科室、房间号 |
| 批次号 | 占位值 `<EXAMPLE_LOT-01>` ～ `<EXAMPLE_LOT-04>`，不造任何像真的批号 |
| 效期 | 占位值 `<EXAMPLE_EXPIRY-A>` ～ `<EXAMPLE_EXPIRY-D>`，不用具体日期，避免被误读成某个真实批次的真实效期 |
| 制品名 | 「疫苗（脱敏样例）」，与 `point_profile.json` 的 `products[].product_name` 一致 |
| 患者信息 | **完全不存在**：本产品只处理温度、点位、批次，不接触任何患者明细 |

判定输入里不出现患者、处方、住院号这类标识，是这类产品的硬边界，不是脱敏的巧思：
越温判定的对象是「某个点位上的某批货」，不是「某个人」。

## `recordings_sample.csv` 字段含义

| 列 | 含义 |
|----|------|
| `point_id` | 冷链点位标识（温控点主键），决定用哪条温区配置去判 |
| `device_id` | 记录仪标识 |
| `device_name` | 记录仪名称 |
| `measured_at` | 采样时刻，RFC3339 带时区偏移（本样例统一 `+08:00`） |
| `temperature_c` | 摄氏温度，量程 −80～120，越量程的数据不进入判定 |
| `unit` | 记录仪显示单位，`C` 或 `F`；`F` 需按 `(F−32)×5/9` 换算并保留原值与原单位 |
| `humidity_pct` | 相对湿度（%），可缺测；缺测即缺，不用 0 冒充 |
| `battery_pct` | 电池电量（%） |
| `accuracy_c` | 记录仪自身精度声明（℃）；超出点位 `accuracy_limit_c` 时结论标注「精度不足，仅供参考」 |
| `source` | 数据来源：`recorder` / `edge-gateway` / `manual-upload` |
| `alarm_flag` | 记录仪自身告警位 |
| `events` | 设备事件，用 `\|` 分隔，取值 `door_open` / `power_loss` / `defrost`；未知类型显式拒绝，不静默丢弃 |

### `events` 的语义

事件列表示**该采样点上事件仍然成立**，不是「事件恰好在这一秒发生」：

- 门开期间每个采样点都带 `door_open` → 可以据此累计开门时长；
- 断电期间每个采样点都带 `power_loss` → 可以据此取断电发生时点与持续区间；
- 除霜周期内每个采样点都带 `defrost`。

这样记录是现场记录仪的实际行为，也是后面归因要用的口径（累计时长取首末出现时刻），
所以样例按这个口径编排。

## 样例编排（`recordings_sample.csv`）

时间轴为 2026-10-20，`+08:00`，采样间隔 5 分钟，共 84 行数据，温区 2～8℃：

| 时段 | 行数 | 温度形态 | 事件 | 告警位 | 湿度 | 用途 |
|------|------|----------|------|--------|------|------|
| 08:00–08:55 | 12 | 4.8～5.4℃ 基线 | — | false | 58% | 常态段 |
| 09:00–09:15 | 4 | 越限，峰值 9.6℃ | `door_open` | false | 62% | 短时开门越限（15 分钟） |
| 09:20–10:55 | 20 | 回落到 4.8～6.9℃ | — | false | 57% | 回落段 + 常态段 |
| 11:00–12:20 | 17 | 峰值 15.2℃，其中 11:00–12:15 连续 16 行越限（75 分钟） | `power_loss` | true | 56% | 断电长时越限 |
| 12:25–13:55 | 19 | 4.8～5.4℃ 基线 | — | false | 56% | 常态段 |
| 14:00–14:15 | 4 | 越限，峰值 9.2℃ | `defrost` | true | 66% | 除霜越限（15 分钟） |
| 14:20–14:55 | 8 | 回落到 5.2～7.5℃ | — | false | 59% | 回落段 |

三段越温窗口的起因各不相同（开门 / 断电 / 除霜），湿度特征也不同：

- **开门段**温度小幅抬升、湿度从 57% 抬到 62%（+5 个百分点）；
- **断电段**温度持续爬升到 15.2℃、**湿度全程恒定 56%** —— 归因时这是「不是开门 / 不是除霜」的判别依据；
- **除霜段**温度小幅越限、**湿度抬到 66%（+10 个百分点）** —— 与开门段的区别就在湿度抬升幅度上。

断电段最后一行（12:20，7.6℃）温度已回到温区内，但断电事件与告警位仍在：
市电未恢复、冷藏箱靠自身余冷降温，此时读数回落不代表故障结束。

湿度恒定与湿度抬升这两条，正是后面「规则基线归因 vs 模型归因」对照基准需要用到的特征。

## `batches_sample.csv` 字段含义

| 列 | 含义 |
|----|------|
| `point_id` | 该批次所在的冷链点位（在库批次是按点位查的） |
| `batch_ref` | 批次号，**只由冷链台账给出**，本系统不生成、不改写、不去空格 |
| `product_name` | 制品名 |
| `quantity` | 在库数量 |
| `expiry_date` | 效期，ISO 日期或 `<占位值>` |

批次号的原样保留是刻意约束：加载器拿到什么就存什么。系统若把批次号 trim 一下、
改个大小写或「规范化」一遍，写回台账时就对不上原批次，等于凭空造了一个查无此物的号。

## 维护约定

- 样例只增不删：新增行要保持既有列序与点位，破坏既有断言的改动必须同时改测试。
- 样例必须能被 `internal/fixture` 的加载器读通：`go test ./internal/fixture/...` 一行坏行都不放过。
- 本目录里的文件**不进** `.gitignore`（它要随仓库发布，读者才能复跑验证步骤）。
- 真实数据集不在本目录：公开冷链数据集由 `scripts/fetch_datasets.sh` 脚本化下载到 `data/`，
  校验与断网降级规则见 `docs/datasets.md`。
