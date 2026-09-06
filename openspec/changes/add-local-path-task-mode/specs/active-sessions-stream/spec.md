# Delta: active-sessions-stream（add-local-path-task-mode）

## MODIFIED Requirements

### Requirement: 事件类型与载荷结构

本变更支持的事件分为两层，各自闭合，内容不同。MUST NOT 把领域事件 Type/Payload 当作 SSE 帧，也 MUST NOT 把 SSE 裸数组回写成总线事件。

**内部领域事件**结构固定为 `{Topic, Type, RID, Payload}`。`Topic` MUST 为 `task` / `session` / `serve_runtime` / `control`。`Type` MUST 为下列闭合枚举之一；**各 Type 的 Payload 字段以 design 事件类型目录为唯一定义**（本 requirement 不重复列举），Payload 仅含该 Type 规定的小字段，MUST NOT 携带 `ActiveSessionItem` 整表或 Attention 明细。`RID` MUST 为主体实体自己的主键 ID：task 事件（`task.created`/`task.status_changed`/`task.deleted`/`task.activity_changed`）为 task 主键；`session.claimed` / `session.touched` / `session.deleted` 为 session 主键（session 是独立聚合，owning task 由 Payload `task_id` 携带）；`serve_runtime.attention_changed` / `serve_runtime.run_status_changed` 为 ServeRuntime 主键 instVersion（owning task 由 Payload `task_id` 携带）；`sessions.aligned` 的主体为任务的会话集合（持久侧无独立对象），RID 为 task 主键；`resync.requested` 无主体，RID 允许空：

- `task.created`（topic `task`，RID=task 主键）
- `task.status_changed`（topic `task`，RID=task 主键）
- `task.deleted`（topic `task`，RID=task 主键）
- `task.activity_changed`（topic `task`，RID=task 主键；仅当提交未伴随 `task.status_changed` 时发布——真实状态迁移只发 `task.status_changed`，其 `updated_at` 推进由它承载；发布条件为**未伴随 status 迁移的任务行非 status 真实变更提交 `Changed=true`**——**不再要求 `updated_at` 跨秒推进**，覆盖 `notice`/`last_port`/`delete_mode`/同状态 `last_error`/`env_snapshot`（不直接透出 DTO，仅作保守失效通知）/align 事务内 notice 等全部既有提交点；`init_status`/`init_error` 系列写入（`ClaimInitRun`/`ClaimInitRerun`/`FinishInitRun`）真实变更 MUST 触发本事件（task-detail-stream 解除 P1.6.1 冻结）；校验失败、存储失败、CAS 未命中、无实际变化路径 MUST NOT 发布）
- `session.claimed` / `session.touched` / `session.deleted`（topic `session`，RID=session 主键）
- `sessions.aligned`（topic `session`，RID=task 主键）
- `serve_runtime.attention_changed`（topic `serve_runtime`，RID=ServeRuntime 主键 instVersion）
- `serve_runtime.run_status_changed`（topic `serve_runtime`，RID=ServeRuntime 主键 instVersion）
- `resync.requested`（topic `control`，RID 允许空）

未知 `Type` MUST 仍投递给订阅者，MUST NOT 使发布失败。场景适配器的消费过滤 MUST 按场景独立定义、互不影响：指挥中心 SSE 适配器 MUST 按 design 消费过滤表标脏（active-only 视图）——`task.created` 与两端都非 `active` 的 `task.status_changed`、以及 `from!=active` 的 `task.deleted` MUST NOT 标脏；其余本变更 Type 与未知 Type、以及溢出信号 MUST 标脏；projects 任务树场景（全状态任务树视图）的消费过滤见 projects-stream spec。适配器 MUST NOT 按 Payload 做增量合并。

**对外 SSE 帧**仅允许三种写出：`event: snapshot` 与 `event: update` 的 data MUST 为与 REST `GET /api/v1/tasks/active` 同构的 `ActiveSessionItem` 裸数组；心跳 MUST 为注释行 `: ping`，无 data。MUST NOT 把内部 `Type` 名用作 SSE `event:`，MUST NOT 发送领域 Payload、增量 diff、单任务补丁或 error 事件帧。

`ActiveSessionItem` 元素字段 MUST 为：`task_id`、`project_id`、`project_name`、`name`、`branch`、`worktree_path`（string，均必有）；`mode`（必有，取值 `worktree`/`local-path`，与任务持久化 `mode` 同源）；`last_active_at`（Unix 秒，number，必有）；`agentStatus`（`idle` \| `busy` \| `retry`，不可用或零 owned 时省略，MUST NOT 输出空串）；`attention`（object，必有）为 `{permissions: PermissionSignal[], questions: QuestionSignal[]}`，无 pending 时两数组为 `[]` 非 `null`。`PermissionSignal` 为 `{id, permission, patterns, since}`（`patterns` 为 string[]，`since` 为 Unix 秒）。`QuestionSignal` 为 `{id, questions: [{header, question}], since}`（`since` 为 Unix 秒）。空活跃列表 MUST 编码为 `[]` 非 `null`。

#### Scenario: 内部领域事件闭合且仅小载荷

- **WHEN** 任务域在任一提交点发布总线事件
- **THEN** 事件含 `Topic`/`Type`/`RID`/`Payload`，`Type` 为本 requirement 闭合枚举之一；`RID` 为主体实体主键（task 事件与 sessions.aligned 为 task 主键，session 单条事件为 session 主键，serve_runtime 事件为 instVersion，resync.requested 允许空）；`task.status_changed` 带 `{from,to}`，`task.deleted` 带 `{from}`，`serve_runtime.run_status_changed` 带 `{task_id,from,to,available}`，`serve_runtime.attention_changed` 带 `{task_id}`，`session.claimed`/`session.touched`/`session.deleted` 带 `{task_id}`，`sessions.aligned` 带计数与受影响 `session_ids`；事件 MUST NOT 携带 `ActiveSessionItem` 或 Attention 明细

#### Scenario: 对外帧与领域事件解耦

- **WHEN** 总线相继到达 `task.created`、`task.status_changed`、`session.claimed`、`serve_runtime.attention_changed`、`serve_runtime.run_status_changed` 或 `resync.requested`
- **THEN** 客户端只收到 `snapshot` 和/或 `update` 帧（若该 Type 被所订阅场景的消费过滤标脏），data 均为完整裸数组，帧的 `event` 名不出现内部 Type

#### Scenario: CreateTask 发领域事件但不驱动指挥中心 SSE

- **WHEN** `CreateTask` 插入可见行成功
- **THEN** 总线发布 `task.created`，已连接的指挥中心 SSE 不因此单独推送 `update`（projects 任务树流按其消费过滤另行推送）

#### Scenario: init 系列写入真实变更发布 activity_changed

- **WHEN** `ClaimInitRun`/`ClaimInitRerun`/`FinishInitRun` 提交且 `init_status`/`init_error` 真实变更（Changed=true）且该提交未伴随 `task.status_changed`
- **THEN** 总线发布一次 `task.activity_changed`（RID=task 主键），不要求 `updated_at` 跨秒；校验失败、存储失败、未命中、无变化路径不发布

#### Scenario: 启动期收敛不发布

- **WHEN** 进程启动期 `ConvergeInterruptedInitRuns` 在 HTTP 开放前执行（零订阅者）并收敛 stale init runs
- **THEN** 不挂接 `task.activity_changed` 发布（明确例外；订阅方以开放后首帧全量快照收敛）

#### Scenario: 数组元素字段完整

- **WHEN** 客户端收到 `snapshot` 或 `update` 且当前存在至少一个 active 任务
- **THEN** 每个元素含 `task_id`/`project_id`/`project_name`/`name`/`branch`/`mode`/`worktree_path`/`last_active_at`/`attention`；`attention.permissions` 与 `attention.questions` 为数组；可用时 `agentStatus` 为三态之一，不可用时该字段缺省

#### Scenario: 活跃流快照帧携带 mode

- **WHEN** 客户端收到 `GET /api/v1/tasks/active/stream` 的 `snapshot` 或 `update` 帧，且当前存在至少一个活跃任务
- **THEN** 每个元素携带 `mode` 字段（必有，`worktree` 或 `local-path`），与任务持久化 `mode` 一致

### Requirement: 活跃会话 SSE 推送端点

系统 SHALL 提供 `GET /api/v1/tasks/active/stream` 端点，鉴权方式与其他 `/api/v1/*` 管理 API 一致（Bearer token）。该端点为前一变更引入且尚未发布，task 中心命名生效后旧路径 `GET /api/v1/sessions/active/stream` MUST NOT 保留别名：对旧路径的请求 MUST 返回 JSON 404 标准错误信封（MUST NOT 写 SSE 响应头、MUST NOT 建立任何事件订阅）。端点 MUST 以 `text/event-stream` 推送，所有数据帧的 data MUST 为与 REST 端点响应体完全同构的活跃会话**裸数组**。建连时序 MUST 为：认证通过 → 对领域 topic `task`/`session`/`serve_runtime`/`control` 各订阅一次并 fan-in（任一路溢出视为溢出）→ 再组装初始快照；组装期间到达且通过消费过滤表的事件 MUST 置脏标记。初始快照组装失败时 MUST 四路全部退订并返回 500 标准错误信封，MUST NOT 写入 SSE 响应头。持久化 `kind`/`mode` 非法（数据损坏）时初始组装 MUST fail-closed 返回 500；update 帧组装遇同类损坏 MUST 按既有组装失败语义处理（跳过本次发送、保持脏标记、由后续事件或心跳 tick 重试），MUST NOT 推送缺 `mode` 或取值非法的帧。组装成功 MUST 写 200 与 SSE 响应头、发送完整 `event: snapshot` 帧并随即 flush（首帧 MUST 立即可达，MUST NOT 滞留到后续心跳）；若脏标记已置位 MUST 紧接着进入合并窗口补发 `event: update`。此后事件到达 MUST 经合并窗口（本变更固定 500ms；后续调整须另走规格变更）合并，窗口到期以最新全量快照发送 `update` 帧；组装失败 MUST 跳过本次发送、保持脏标记并在后续事件或心跳 tick 重试，MUST NOT 关闭连接。订阅溢出信号置位时 MUST 先置脏标记再立即触发一次窗口外全量快照重推；脏标记在组装失败时保持并由后续事件或心跳 tick 继续重试（自愈信号不得丢失）；写/flush 失败按统一写路径立即退订退出（客户端重连经 snapshot 自愈）。无事件期间 MUST 以心跳注释行维持连接（默认 25s）。所有帧（snapshot/update/溢出重推/心跳）MUST 经统一写路径写出并检查写与 flush 错误：任何一次写或 flush 失败 MUST 立即退订并退出 handler，MUST NOT 依赖后续心跳或 context 兜底。帧组装 MUST 复用与 REST 端点相同的读模型组装逻辑，元素字段为 `task_id`、`project_id`、`project_name`、`name`、`branch`、`worktree_path`、`mode`（必有，取值 `worktree`/`local-path`，与任务持久化 `mode` 同源）、`last_active_at`（Unix 秒）、`agentStatus`（`idle` | `busy` | `retry`，不可用时省略）、`attention`（同 agent-attention spec 结构）。推送路径 MUST 为纯读操作，MUST NOT 实时调用 opencode 接口，MUST NOT 产生任何写副作用。端点 MUST 在客户端断开或服务进程 context 取消时释放订阅并退出 handler；服务端关停 MUST 先取消活跃 stream 再执行 HTTP Shutdown，使关停可在其预算内完成。

#### Scenario: 连接即收快照

- **WHEN** 已认证客户端建立 SSE 连接
- **THEN** 客户端立即收到一帧 `snapshot`，data 为当前活跃会话裸数组（无活跃任务时为 `[]` 非 `null`）

#### Scenario: 旧路径返回 JSON 404

- **WHEN** 已认证客户端请求 `GET /api/v1/sessions/active/stream`
- **THEN** 返回 JSON 404 标准错误信封（非 SSE 响应），不写入 `text/event-stream` 头，不建立任何事件订阅

#### Scenario: 订阅先于首次组装

- **WHEN** 建连过程中组装初始快照期间发生会话变更
- **THEN** snapshot 帧发送后紧接补发一帧 `update`，变更不丢失

#### Scenario: 初始组装失败

- **WHEN** 建连时初始快照组装因底层查询失败
- **THEN** 响应为 500 标准错误信封，不写入 SSE 响应头，订阅被释放

#### Scenario: 初始组装遇非法 kind/mode 返回 500

- **WHEN** 建连时初始快照组装遇到活跃任务持久化 `kind`/`mode` 非法或损坏
- **THEN** 响应为 500 标准错误信封（fail-closed），不写入 SSE 响应头，四路订阅被释放

#### Scenario: update 组装遇非法 kind/mode 保持脏标记

- **WHEN** 连接存续期间某次 update 帧组装遇到活跃任务持久化 `kind`/`mode` 非法或损坏
- **THEN** 该帧被跳过并保持脏标记，由后续事件或心跳 tick 重试；MUST NOT 推送缺 `mode` 或取值非法的帧

#### Scenario: 事件驱动更新

- **WHEN** 连接存续期间某活跃任务的 session 活跃时间变更
- **THEN** 客户端在合并窗口到期后收到一帧 `update`，data 为该变更后的最新全量裸数组

#### Scenario: 窗口内多次变更合并

- **WHEN** 500ms 合并窗口内到达多个被消费过滤表标脏的领域事件
- **THEN** 客户端仅收到一帧 `update`，data 为窗口到期时刻的全量快照

#### Scenario: 心跳维持连接

- **WHEN** 连接存续且超过心跳间隔无任何事件
- **THEN** 客户端收到心跳注释行，连接保持打开

#### Scenario: 组装失败不断连且可重试

- **WHEN** 某次 update 组装时底层读模型查询失败
- **THEN** 该帧被跳过并记录日志，脏标记保持，连接保持，后续事件或心跳 tick 触发重试

#### Scenario: 事件溢出自愈

- **WHEN** 订阅缓冲溢出导致事件被丢弃、溢出信号置位
- **THEN** 服务端立即推送一次最新全量快照，客户端状态自愈；若该次组装失败，脏标记保持并由后续事件或心跳 tick 重试至成功；若写/flush 失败，连接关闭，客户端重连后经 snapshot 自愈

#### Scenario: 未认证访问被拒

- **WHEN** 请求缺失或携带错误 token
- **THEN** 返回 401，不建立事件流，不泄露任何资源信息

#### Scenario: 推送无写副作用

- **WHEN** SSE 连接存续并发生多次推送
- **THEN** 数据库内容、任务状态机与进程集合除外部因素外不发生变化，且推送路径不发起任何 opencode 调用

#### Scenario: 服务关停及时释放

- **WHEN** SSE 连接存续期间服务进程 context 被取消
- **THEN** handler 退出、订阅释放，HTTP Shutdown 在其预算内完成，不拖到超时
