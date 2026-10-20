# p1b 运行报告：模型依赖项运行时实证（opencode v2.0.28）

- run_id: `20261016T090000Z-0bb6ac6c`
- 执行方式：scratch harness 副本（`/var/folders/yk/.../T/opencode/p1b-harness/`，git 自含）+ Docker 容器（`--read-only` rootfs，tmpfs /tmp 256m，SPIKE_BOUNDARY_MECHANISM=container，canary EROFS REJECTED）
- 版本：v1=1.18.26（sha256 90b99cc2…）、v2=2.0.28（sha256 58fd4154…），均与 manifest official_sha256 核对一致
- 模型接线：provider `archi`（npm @ai-sdk/anthropic），baseURL=`https://archi.marscore.net/v1`（关键：必须带 `/v1` 后缀，裸域名会返回 HTTP 200 HTML 落地页 → `provider.invalid-output`）；凭据仅经容器 env 注入（副本 spikelib `_upstream_env` 透传 ANTHROPIC_BASE_URL/ANTHROPIC_AUTH_TOKEN，日志脱敏集合覆盖）；全部证据经脱敏扫描（明文凭据零命中）
- 仓库边界：`scripts/spike-v2/` 与仓库既有文件零修改；harness 副本相对原 harness 的全部改动见 `harness-diff/`（3 项）

## 逐项结论

| 项 | exec_status | 结论 | 证据 |
|---|---|---|---|
| event.02d.status_changed | PASS（results.jsonl） | 真实模型 turn 的状态事件实际形态：`session.execution.started`/`session.execution.succeeded`（无 v1 式 session.status idle/busy 事件）；事件结构 `{id:evt_*,created,type,location:{directory},data:{sessionID,...},durable:{aggregateID,seq,version}}` | live/event/p1b-frames.jsonl |
| event.02e.attention_raised | PASS（results.jsonl） | permission 升起事件 `permission.asked`，data 含 per_ ID、sessionID、action=shell、resource | live/event/perm-frames.jsonl |
| event.02f.attention_resolved | PASS（results.jsonl） | reply once（204）→ pending 清空 + execution 边界事件 | live/event/perm-after-reply-frames.jsonl |
| attention.02 | PASS（results.jsonl） | once 204+清空；重复回复 404 PermissionNotFoundError；幽灵 per_ ID 404；reject 独立 fixture 清空。pending 存续期：GET /api/permission/request?location[directory]= 返回 {location,data:[]}（pending 仅在 session 级端点，global request 列表不含）；GET /api/permission/saved?projectID= 返回 {data:[]} | live/attention/once-facts.json、reject-facts.json、pending-observation.json |
| prompt.03 | PASS（results.jsonl） | file part `{files:[{uri:"file://…",name:…}]}` 接受且 agent 读出内容（assistant text 命中 FILE_MARK） | live/prompt/assistant-texts.json |
| p1b.interrupt（1.1 挂起停止 wire） | PASS（extra） | 停止端点为 `POST /api/session/{id}/interrupt`（`/abort` 404 不存在）→ 200 `{"interrupted":bool}`；执行中 interrupt=true、outcome=interrupted、之后继续 prompt 正常恢复 | live/extra/p1b_interrupt.json、live/lifecycle/interrupt-facts-1.json |
| p1b.status_seq（1.2 status 等价） | PASS（extra） | busy=execution.started、idle=execution.succeeded 边界事件序列 + Session.Info.outcome（succeeded/failed/interrupted）；无独立 idle/busy status 事件 | live/extra/p1b_status_seq.json、live/event/status-seq.json |
| p1b.environment（1.3 注入隔离） | PASS（extra） | PUT /api/session/{sid}/environment {variables:{…}} 204 替换式；session shell API A=alpha/B=beta 互不串用；模型 bash 触发 permission（auto-reply once）后模型侧 alpha_event=true、beta_seen=false | live/extra/p1b_environment.json、live/env/* |
| migration.forward（pending 保留性） | FAIL（extra，有效负面实证） | v1.18.26 pending permission 为进程内存态：样本 per_140d7767a001… 停服后不存在于数据树任何持久化存储（SQLite permission 表 0 行、事件表无记录、全树 grep 无命中）；v2 打开同数据树：session 可见、消息历史保留（3 条）、pending 列表空 → 官方升级路径丢弃 pending | live/extra/p1b_migration_pending.json、live/migration/* |

## 关键 wire 事实（供冻结）

1. v2 permission config 触发形态：顶层 `"permissions":[{"action":"shell","resource":"*","effect":"ask"}]`（v2 归一化 bash→shell/write→edit/task→subagent；`action:"bash"` 不生效）。v1 形态为 `"permission":{"bash":"ask"}` 对象，且 v1 拒绝 v2 数组形态（ConfigInvalidError）。
2. v1 pending 读取端点=GET `/permission?directory=<enc>`（根路径无 /api；v1 `/session/{id}/permission` 是 SPA fallback）。
3. shell 输出须经 SSE `session.shell.ended` 载荷 `output.output` 读取（GET /shell/{id}/output 对已完成 shell 返回空）。
4. v2 wire：POST /api/session/{sid}/prompt body `{"id":"msg_*","text":…}`；事件流 GET /api/event?directory=（basic auth）。

## 假设与边界

- `interrupt` attempt 1 为"prompt 已提交但模型未开始产出"窗口（busy_event=false 下 interrupt=true）——即 idle 前的短窗口也可 interrupt；未覆盖"模型流式产出中"的长窗口 interrupt（语义同端点，风险低）。
- migration pending 结论基于单样本（touch /tmp/* 触发 external_directory 类型）；其他 permission 类型未逐一验证，但"内存态、数据树零持久化"是存储层事实，类型无关。
- 模型为代理端点（archi.marscore.net，deepseek-v4.1-flash），非 Anthropic 官方；provider.invalid-output 类模型故障已排除（pong 零 retry）。
- 环境/状态结论均为 v2.0.28 二进制（构建 044d8b7d）实测，非源码推断。

## harness 副本 diff 清单（harness-diff/）

1. `spikelib.py.diff`（+5 行）：`_upstream_env` 透传 ANTHROPIC_BASE_URL/ANTHROPIC_AUTH_TOKEN（模型凭据仅经 env）。
2. `live-probe.py.diff`（36 处 +/-，全部为端口偏移）：29711-29724→31711-31724、4096→6096（+2000 隔离）。
3. `p1b-live-probe.py.new`（新增 1125 行）：模型驱动 probe 扩展（importlib 复用 live-probe Ctx；probe_id 用封闭集合 5 项落 results.jsonl + 新增 p1b.* 4 项落 live/extra/）。

## 验证状态

- 已验证：9/9 项探测执行并落证；canary EROFS REJECTED；结果引用闭合校验（results.jsonl + extra evidence 引用全部存在）；证据脱敏深扫（token 片段、sk- 模式）零命中；副本树与仓库拷贝 diff -rq 一致。
- 未验证：interrupt 长窗口（流式产出中）变体；migration 其他 permission 类型。
