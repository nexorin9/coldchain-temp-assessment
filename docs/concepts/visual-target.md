# 视觉目标卡：药品与疫苗冷链温控判定系统

> 本轮交付形态是**无界面 HTTP API 服务**（OpenAPI 3.1 + curl 示例 + 契约测试），因此本卡描述的画面
> 是**真实终端会话与 HTTP 交互回显**，不是网页大屏。凡是用当前依赖（Go 标准库 + SQLite 驱动）
> 真做不出来的效果，一律不写进本卡。

## 一、三个状态

### 1. 起始态：服务刚起，还没出事

画面：终端里一行结构化启动日志，紧接着一条健康检查回显。

```
{"ts":"2026-01-16T08:10:02.113Z","level":"info","event":"serve_start","version":"0.1.0","config":"ledger=https://ledger.internal.example-hospital.cn notify=https://notify.internal.example-hospital.cn model=https://llm.internal.example-hospital.cn(...) timeout=30s db=./data/coldchain.db archive=./data/archive"}
{"status":"ok","service":"coldchain-temp-assessment","version":"0.1.0"}
```

要传达的信息：**服务起来了，且明确接了哪几个院内系统**——启动日志里出现的基址就是真实配置，
没有的基址根本起不来（配置缺失直接退出码 78）。

### 2. 首个可用瞬间：一条越温事件投进来，结论成形

画面：一条 curl 投递 + 一次判定结论查询，终端里先出现窗口三要素（起止、峰值、持续分钟），
再出现原因候选与证据，最后出现状态 `awaiting_confirmation`。

```
$ curl -sS -X POST http://127.0.0.1:8080/v1/events/coldchain-temperature -H "Authorization: Bearer $COLDCHAIN_INGEST_TOKEN" -d @testdata/single_event.json
{"event_id":"<EVENT_ID>","run_id":"<RUN_ID>","accepted":true}

$ curl -sS http://127.0.0.1:8080/v1/runs/<RUN_ID>
{"state":"awaiting_confirmation","window":{"start":"2026-01-15T22:05:00+08:00","end":"2026-01-16T03:40:00+08:00","duration_minutes":335,"peak_c":12.4},"attribution":{"cause_code":"power_loss","confidence":0.86},"batches":[{"batch_ref":"<EXAMPLE_LOT>"}]}
```

要传达的信息：**越温窗口、原因、批次一次拿全**，且状态停在「待主管确认」——此时台账与门诊
通知还没有写出去。

### 3. 成形态：主管确认后，落库与通知各一条，过程可回看

画面：确认动作 → 写回结果 → 步骤时间线三屏连读。

```
$ curl -sS -X POST http://127.0.0.1:8080/v1/runs/<RUN_ID>/confirm -H "Authorization: Bearer $COLDCHAIN_LEDGER_TOKEN" -d '{"disposition":"pending_manufacturer_evaluation","operator":"<主管工号>"}'
{"state":"written_back","external_ref":"<EXTERNAL_REF>","ledger_written":true,"notified":true}

$ curl -sS http://127.0.0.1:8080/v1/runs/<RUN_ID>/timeline
{"steps":[{"name":"fetch_series","started_at":"...","duration_ms":42},{"name":"extract_window","duration_ms":7},{"name":"attribute","duration_ms":1830},{"name":"draft","duration_ms":1120},{"name":"await_confirmation","duration_ms":0}]}
```

要传达的信息：**判定是一次可回看的过程，不是一个结论快照**——哪一步慢、哪一步重试过、
时间线位置单调不重叠，都在时间线里看得见。

## 二、画面类型与视觉语言

- **画面类型**：终端会话输出 + HTTP JSON 回显（本轮无图形界面，不画浏览器页面）。
- **配色**（仅用于终端与文档，不引入前端依赖）：
  - 主色 `#1F6FEB`（可交互动作：投递、确认、重试）
  - 成功 `#1A7F37`（已写回、已落库）
  - 警示 `#B26A00`（降级、待写回、精度不足）
  - 危险 `#C4302B`（失败、非法迁移、量程越界）
  - 中性灰阶 `#24292F` / `#57606A` / `#8B949E`（正文 / 次要信息 / 辅助说明）
- **字体族**：终端用系统等宽字体；数字（峰值、持续分钟、token 数）用等宽对齐，时间戳统一
  `YYYY-MM-DD HH:mm`。
- **信息层级**：
  1. 最显眼：窗口三要素（起止、峰值、持续分钟）——判定结论的第一眼；
  2. 次显眼：原因结论与证据；
  3. 第三层：涉及批次与处置建议；
  4. 辅助层：run_id / step_name / 时间戳 / 错误码。
- **留白密度**：一个 JSON 响应一屏放得下（约 6～10 行），长响应用 `jq` 折行展示，字段之间不留空行。

## 三、必须做得到的约束（不得画出现有技术栈做不出的效果）

- 只用 Go 标准库 `net/http` + `modernc.org/sqlite` + 终端输出，**不引入**前端框架、图表库或
  终端 UI 库（如 bubbletea / lipgloss）；终端画面就是真实 stdout/stderr 的样子。
- 不画网页仪表盘、不画 WebGL、不画「实时大屏」——本轮没有界面，视觉目标只对终端与 HTTP 交互成立。
- 所有标识用占位值（`<RUN_ID>`、`<EXAMPLE_LOT>`、`<EXAMPLE_POINT>`），不出现真实批号、住院号、
  患者化名。
- 概念图若生成，必须能画成「终端真实回显」的样子；本轮运行环境不具备图像生成能力，因此
  `docs/concepts/` 只保留本结构化目标卡，**不放任何图片**，也不用它冒充实测截图。
