# active-sessions-stream Delta (task-detail-stream)

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

`ActiveSessionItem` 元素字段 MUST 为：`task_id`、`project_id`、`project_name`、`name`、`branch`、`worktree_path`（string，均必有）；`last_active_at`（Unix 秒，number，必有）；`agentStatus`（`idle` \| `busy` \| `retry`，不可用或零 owned 时省略，MUST NOT 输出空串）；`attention`（object，必有）为 `{permissions: PermissionSignal[], questions: QuestionSignal[]}`，无 pending 时两数组为 `[]` 非 `null`。`PermissionSignal` 为 `{id, permission, patterns, since}`（`patterns` 为 string[]，`since` 为 Unix 秒）。`QuestionSignal` 为 `{id, questions: [{header, question}], since}`（`since` 为 Unix 秒）。空活跃列表 MUST 编码为 `[]` 非 `null`。

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
- **THEN** 每个元素含 `task_id`/`project_id`/`project_name`/`name`/`branch`/`worktree_path`/`last_active_at`/`attention`；`attention.permissions` 与 `attention.questions` 为数组；可用时 `agentStatus` 为三态之一，不可用时该字段缺省
