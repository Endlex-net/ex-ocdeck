# opencode-v2-provider Specification

## Purpose

为 ocdeck 提供 opencode v2 的 provider 实现：在既有 agent-provider-spi 契约下兑现 v2 wire 协议（`/api` 前缀、`{data}` envelope、v2 事件与 permission 形态），并托管 v2 runtime（A+ 全局单 server 多 session），与 v1 provider 长期并存。

## Requirements

### Requirement: v2 wire 协议适配

系统 SHALL 以 opencode v2 锁定基线（2.0.28）的实证契约与 v2 上游通信：全部 API 请求 MUST 使用 `/api` 前缀；响应 MUST 按 `{data}` envelope 解包；prompt 提交 MUST 使用 `POST /api/session/{sessionID}/prompt`——无附件 body 为 `{text, id}`，附件为同 body `files` 数组（实证形态 `FileAttachment{uri(必),name,description,mention}`，无 mime 字段；仅 uri 形态开放，inline base64 未实证不开放；映射见 design D7）；OpenAPI 契约发现 MUST 使用 `/openapi.json`（MUST NOT 依赖 `/doc`，其为公开 SPA 页面）；session ID MUST 符合 `^ses` 形态、message ID MUST 符合 `^msg_` 形态（以运行时发现的 pattern 为准）。对 wire 形状的假设 MUST NOT 硬编码绕过运行时证据；与 spike 差异清单标注为「候选/待验证」的形态相关的行为 MUST 在实证前保持保守语义。

#### Scenario: API 请求统一带 /api 前缀

- **WHEN** v2 provider 发起任意 session/event/permission API 调用
- **THEN** 请求路径以 `/api` 为前缀，且响应经 `{data}` envelope 解包后解析

#### Scenario: prompt 提交使用 v2 形态

- **WHEN** 向 v2 session 提交用户消息
- **THEN** 请求为 `POST /api/session/{sessionID}/prompt`，body 含 `text` 与符合已发现 pattern 的 `id`；仅 SPI 准入失败返回 typed error，进入适配层后的发送结果按 `PromptResult` 四分类返回（design D7），不静默重投

#### Scenario: 契约发现不依赖 /doc

- **WHEN** v2 provider 需要上游契约事实（如 ID pattern）
- **THEN** 从 `/openapi.json` 获取并解析；获取失败时该能力保持待验证，不猜默认值

### Requirement: v2 事件订阅与归一化

系统 SHALL 经 `GET /api/event` 订阅 v2 SSE 事件流，并将 v2 事件归一化为 SPI `AgentEvent`：事件载荷 MUST 从 `data.*` 键取值；session 归属 MUST 按 `data.sessionID` 校验；事件名映射 MUST 覆盖 `session.created`/`session.renamed`/`session.deleted` 及消息/permission/question 族。v2 SSE 为 volatile（断线不重放），订阅 MUST 支持断线重连并在重连后自补状态。首帧 `server.connected` MUST 作为订阅握手证据，未观测到首帧 MUST NOT 判定订阅成功。

#### Scenario: session 重命名事件归一化

- **WHEN** v2 上游发出 `session.renamed` 事件且 `data.sessionID` 归属当前任务 session
- **THEN** 归一化为 SPI 的 session 更新语义事件，标题取值自 `data.*` 载荷

#### Scenario: 断线重连自补状态

- **WHEN** SSE 订阅断开（网络错误或上游关闭）
- **THEN** 订阅方重连事件流，并在重连后主动拉取 session 快照对齐状态，不假设断线期间事件已送达

#### Scenario: 订阅握手以首帧为准

- **WHEN** 发起事件订阅
- **THEN** 仅在观测到可解析的首帧 `server.connected` 后才将订阅视为建立；进程存活不等于握手成立

#### Scenario: permission.replied 归一化为 AttentionResolved

- **WHEN** v2 上游发出 `permission.replied` 事件且 `data.requestID` 命中此前 `permission.asked` 升起的同一 permission（`replied.requestID === asked.id`），`data.sessionID` 归属当前任务 session
- **THEN** 归一化为 SPI `EventAttentionResolved`，`Attention.ID = data.requestID`（与 asked 升起时的 ID 同值，下游 `removePermLocked` 据此命中移除 pending）；`reply ∈ {once, always, reject}` 仅作诊断，批准与拒绝统一映射为 `AttentionResolved`、MUST NOT 分流独立 rejected Kind；MUST NOT 据 `permission.replied` 推导 session idle/busy 状态（idle/busy 仅由执行边界事件 `session.execution.*` 决定）

### Requirement: v2 attention 通道

系统 SHALL 经 v2 permission/form 端点兑现 SPI AttentionIO：pending permission 快照 MUST 读 session 级 `GET /api/session/{id}/permission?directory=<d>`（实证：pending 仅存于 session 级端点；全局 `/api/permission/request` 不带 `location[directory]` deepObject 过滤时返回空，MUST NOT 作为 pending 来源）；`/api/permission/saved` 仅存已保存规则，MUST NOT 并入 pending 视图。question 的等价能力为 form 族（`GET /api/session/{id}/form`，pending 判定 = `state.status="pending"`；上游无 question 端点）。permission 回复 MUST 使用 `POST /api/session/{id}/permission/{requestID}/reply?directory=<d>`（body `{decision: once|always|reject}`，204）；form 回复 MUST 使用 `POST /api/session/{id}/form/{formID}/reply`（body `{answer}`，204）。无 pending 项时 MUST 返回空视图而非错误。端点 4xx/5xx 或不可解析响应 MUST 如实报错并透出能力健康，不得吞错返回空。逐方法 wire↔SPI 映射 MUST 遵循 design D7 映射表，表中标注【待实证】的项在冻结前 MUST 保持保守语义。

#### Scenario: pending permission 列表

- **WHEN** 后台对账查询某 v2 任务的 pending attention
- **THEN** 经 session 级 `GET /api/session/{id}/permission?directory=<d>` 取回 pending 列表并归一化为 SPI Attention 项；空列表返回空视图

#### Scenario: 端点形态违约透出

- **WHEN** attention 端点返回 4xx/5xx 或不可解析响应
- **THEN** 本次查询按错误分类透出（能力健康上报），不得当作「无 pending」处理

### Requirement: v2 runtime 托管（A+ 单 server）

系统 SHALL 以全局单 `opencode serve` 进程承载全部 v2 任务 session：任务激活时 MUST 确保 server 在跑（幂等获取，未运行则以 `serve` 子命令启动——MUST NOT 使用裸 `opencode` 或 `--port` 根命令形态）；session 建立 MUST 区分历史有无——任务无历史上游 session 时经 `POST /api/session` 创建（以 `location.directory` 绑定任务 worktree），已有历史 session 时 MUST 仅恢复延续该 session，MUST NOT 创建替代 session。server 就绪判定 MUST 使用 `/api/session`（401/200 为就绪信号，MUST NOT 使用 `/global/health`——其为 SPA HTML）。任务挂起释放 MUST 仅释放运行关联（退订事件、回收终端/连接资源），MUST NOT 删除上游 session、MUST NOT 终止全局 server；上游 session 的删除 MUST 仅发生于两类入口（design D3）：任务删除流程经 SessionControl；首建失败补偿的受限回收（仅本次 attempt 新建且未提交成功的 session）。承接路径 MUST 沿用 binding 承载的既有 upstream session（官方升级保留），MUST NOT 新建替代 session。server 进程异常退出后 MUST 自动重拉，重拉后活跃任务 MUST 按持久化 binding 重建 session 关联。runtime 实例端口/pid 记录的生命周期与重启恢复矩阵 MUST 遵循 design D3「runtime 实例记录」契约（记录先于启动写入、pid 可空、pid 缺失时经确定性托管单元重新发现并验证，验证通过才复用、不得仅凭端口响应接管）。AcquireEphemeral 的仅内存临时连接 MUST NOT 创建 session；其临时 binding（UpstreamSessionID 允许为空、TerminalRef 保留值）MUST 经 provider 私有结构校验而非通用 envelope Validate，MUST NOT 持久化；对应 Release MUST 仅回收连接，不得调用 session abort/delete。

#### Scenario: 激活幂等获取全局 server

- **WHEN** v2 任务激活且全局 server 未运行
- **THEN** 以 `opencode serve --port <p> --hostname 127.0.0.1` 启动 server，经 `/api/session` 就绪判定后按历史有无创建或恢复任务 session

#### Scenario: server 已运行时复用

- **WHEN** v2 任务激活且全局 server 已在跑
- **THEN** 不重复启动进程，直接在其上创建（无历史）或恢复（有历史）任务 session

#### Scenario: 恢复路径不新建替代会话

- **WHEN** v2 任务恢复或重复激活且 binding 承载历史 upstream session
- **THEN** 经 `GET /api/session/{id}` 验证后仅恢复该 session；session 缺失或不可读时停止并提交用户决策，任何 ResumePolicy 不得授权创建替代 session

#### Scenario: 挂起释放保留上游 session

- **WHEN** v2 任务挂起释放 binding
- **THEN** 先经 `POST /api/session/{id}/interrupt` 停止该任务在 server 上的执行（停止 wire 已实证冻结：`/abort` 不存在，interrupt 返回 `{interrupted: bool}`），确认停止后释放运行关联（事件订阅、终端/连接资源）；上游 session 保留；全局 server 进程保持运行；停止操作不得影响同 server 其他 session。状态收敛 MUST 遵循 design D4 生命周期状态矩阵：停止失败/超时收敛回 active、保留运行关联、记 last_error，MUST NOT 提交 suspended、MUST NOT 停留 suspending、MUST NOT 进入 v1 的强杀后 finishSuspend 分支；停止已成功但清理或最终 CAS 失败时收敛 suspended + last_error

#### Scenario: 任务删除才删除上游 session

- **WHEN** v2 任务经任务删除流程删除
- **THEN** 经 SessionControl 删除该任务的上游 session；全局 server 进程保持运行

#### Scenario: server 崩溃后重建关联

- **WHEN** 全局 server 进程异常退出并被重拉
- **THEN** 活跃 v2 任务按持久化 binding 重建 upstream session 关联；重建失败的任务按既有恢复失败语义处理

#### Scenario: 操作者发起重启全局 serve

- **WHEN** 操作者经 `POST /api/v1/server/agent/restart` 请求重启全局共享 serve
- **THEN** 系统 MUST 杀掉当前 serve 并以**同一固定端口**重新拉起（端口固定不轮换）；重拉成功后 MUST 经既有 `OnServerRestarted` / `RebuildV2Associations` 恢复活跃 v2 任务在原 session 上的关联（MUST NOT 创建替代 session、MUST NOT 自行重建关联）；监控循环 MUST 保持运行；MUST NOT 使用 `ShutdownServer`（其为终态关停——置 `shuttingDown` 并停止监控，不可用于重启）；supervisor 处于 `shuttingDown` 时 MUST 拒绝重启；v2 provider 未注册或未实现 `ServerRestarter` 窄接口时 MUST fail-closed（用户可见能力，不得 no-op）
- **AND** 影响范围 = 该 server 承载的全部 v2 任务：在途 prompt 丢失、SSE 连接断开，按既有崩溃重拉语义恢复

### Requirement: v2 状态读取保守语义

status 等价能力已实证通过（run 20261016T090000Z-0bb6ac6c）。系统 SHALL 按冻结映射实现 StatusReader：SSE `session.execution.started` → busy；`session.execution.succeeded` / `session.execution.failed` / `session.execution.interrupted` 均为执行终态 → idle（实证：run 20261014T040930Z-1bc764c7 `live/event/stream.ndjson` failed 带 `data.error`、run 20261016T090000Z-0bb6ac6c `live/lifecycle/p1b-frames-1.jsonl` interrupted 带 `reason=user`；真实中断/失败流可能不跟随 succeeded，不映射则状态永停 busy）；`Session.Message.Idle.outcome ∈ {succeeded, failed, interrupted}` MUST 仅作完成诊断，MUST NOT 映射为用户可见 idle/busy；事件 `data` 的 `error`/`reason` 等载荷同样仅诊断、MUST NOT 参与状态推导。能力健康透出 MUST 上报 `CapAvailable`。事件断线时 MUST 重连并以 session 快照自补状态。

#### Scenario: 执行边界事件映射状态

- **WHEN** v2 SSE 流出现 `session.execution.started` / `session.execution.succeeded` / `session.execution.failed` / `session.execution.interrupted` 事件
- **THEN** started 归一化为 busy；succeeded/failed/interrupted 为执行终态，归一化为 idle；事件断线后重连并以 session 快照自补

#### Scenario: outcome 不作状态来源

- **WHEN** 读取 session 消息流末条 `Session.Message.Idle.outcome`
- **THEN** 仅用于完成诊断（succeeded/failed/interrupted），不映射为用户可见 idle/busy 状态

### Requirement: v2 终端连接

系统 SHALL 兑现 v2 TerminalIO：attach 前 MUST 经 REST 验证 binding 的 upstream session 存在且归属本任务（前置条件）；attach MUST 自动连接共享 server 并选中 binding 的原 session（用户已定：自动定位）；自动选中不可实现时 MUST 返回明确失败，MUST NOT 退化为手动定位、MUST NOT 创建替代 session。session 不存在或归属不符 MUST 返回明确失败（not_found / binding_invalid）。TerminalRef 为交互 TUI 托管单元标识，独立于后台 serve 管理句柄。

#### Scenario: attach 前验证并自动选中绑定 session

- **WHEN** 用户请求 attach 某 v2 任务终端
- **THEN** 先经 `GET /api/session/{id}` 验证绑定 session 存在且归属本任务，验证通过后产出自动选中该 session 的 TUI attach argv；验证失败或自动选中不可实现时返回明确失败且不创建 session

### Requirement: v2 契约锚点治理

系统 SHALL 为 v2 维护独立契约锚点集：锚定版本为 2.0.28、源码锚点 commit `026b106b`（与二进制构建 commit `044d8b7d` 分别记录，不视为同一基线）。契约检查 MUST 覆盖 spike 实证的关键 wire 事实（`/api` 前缀、envelope、prompt 形态、事件名与归属键、permission 双端点、`/openapi.json`、就绪判定端点、serve argv 形态）。v1 契约锚点集 MUST 保持不动。

#### Scenario: v2 契约漂移检测

- **WHEN** 执行 v2 契约检查脚本
- **THEN** 逐锚点与锁定 commit 的上游源码比对，差异如实报告；v1 锚点集不受影响

### Requirement: v2 凭据与 binding 私有数据

系统 SHALL 以 v2 provider 私有 data 形状持久化连接事实（端点/目录/session 来源等）；session 来源 MUST 记录为 `origin`（`created`=ocdeck 首建 / `adopted`=承接延续 / `ephemeral`=仅内存临时连接），作为恢复配置策略的识别事实：承接延续的 session（`adopted`）在后续恢复中 MUST NOT 被覆盖既有环境/权限配置。认证密码 MUST 经确定性派生获得（主密钥 + 稳定的 v2 runtime 实例标识；连接同一全局 server 的全部任务 MUST 使用同一派生凭据，任务身份与目录归属独立校验），MUST NOT 持久化于 binding 或任何存储。v2 私有 data 的编解码 MUST fail-closed：畸形或缺字段时拒绝使用并报错。认证注入 MUST 使用 `OPENCODE_SERVER_PASSWORD` env 注入 serve 进程；未设置密码时上游打印随机密码的行为 MUST NOT 被依赖（自管 server 必须显式注入）。v2 共享 serve 与 v2 TUI 的进程环境 SHALL 在派生密码之外叠加宿主环境并集（进程 env ∪ login-shell 捕获；非法 KEY / `OCDECK_*` / 宿主 `OPENCODE_SERVER_PASSWORD` 除外；`TERM` 强制 `xterm-256color`），使插件与 `{env:VAR}` 能读取宿主机变量。项目级/任务级 env MUST NOT 写入共享 serve 进程，仍仅经 session `PUT /environment` 应用。

#### Scenario: binding 私有数据 fail-closed

- **WHEN** 解码 v2 binding 私有 data 时字段缺失或畸形
- **THEN** 拒绝使用该 binding 并按 ErrorKind 语义报错，不产生网络副作用

#### Scenario: 凭据不落库

- **WHEN** 建立或重建 v2 runtime 连接
- **THEN** 密码由主密钥与 v2 runtime 实例标识确定性派生注入，binding 与数据库中不出现密码明文

#### Scenario: 承接 session 恢复不覆盖配置

- **WHEN** `origin=adopted` 的承接 session 发生恢复（resolver R1）
- **THEN** 恢复仅重建连接/终端/订阅，MUST NOT 覆盖该 session 的既有环境/权限配置
