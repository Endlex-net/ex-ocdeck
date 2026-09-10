## MODIFIED Requirements

### Requirement: 内部事件总线

系统 SHALL 提供进程内、按领域 topic 发布的领域事件总线，用于将任务域事实通知任意场景订阅者。总线 MUST 支持按 topic 订阅与退订；发布操作 MUST 为非阻塞：订阅者缓冲满时允许丢弃该订阅者的本次事件并记录日志，MUST NOT 阻塞或失败发布方。订阅句柄 MUST 提供可观察的溢出信号：事件被丢弃时该信号被非阻塞置位（多次溢出至少一次可见），供订阅方触发自愈重推。Publish 与 Subscribe/Close MUST 并发安全；Close 后 MUST NOT 再收到事件。领域事件发布 MUST 发生在对应状态真实变更且落账/内存态应用成功之后；校验拒绝、存储失败、条件更新未命中（CAS 未提交）或无实际变化的路径 MUST NOT 产生领域事件。`resync.requested` 控制事件不受此前提约束（它不表达域事实），仅允许用于 D2 异常收敛矩阵规定的 (a) 提交结果不确定路径、(b) worker 撤销登记前重同步（仅提交结果仍不确定的叶节点 W②b；committed=false 已确定的 W③b 撤销登记 MUST NOT 发布）、(c) 锁等待超时且触发令牌仍有效的债务登记（含 tombstone 匹配直登 `postCleanup`）（见「不确定提交的重同步例外」Scenario），每条不确定路径至多一次。`task.user_activity` 同样不受"状态真实变更落账后发布"前提约束：它表达一次性用户输入事实，不对应任务行变更，发布 MUST NOT 以任务行变更为前提，且 MUST NOT 为满足该通则额外推进 `updated_at` 或产生 `task.activity_changed`。总线 MUST NOT 引入第三方依赖，MUST NOT 持久化事件，进程重启后以全量对齐机制收敛（不保证事件的历史重放）。本变更启用的领域 topic 为 `task`、`session`、`serve_runtime`、`control`；总线设计 MUST 允许后续新增 topic 而不修改订阅者接口。总线 MUST 按领域发布，MUST NOT 按某一 HTTP 场景裁剪发布集合。指挥中心 SSE 适配器 MUST 按 design 消费过滤表决定是否标脏，MUST NOT 把领域事件原样外发。

#### Scenario: 变更落账后发布

- **WHEN** 某任务的 session 归属/活跃时间、attention、agent 状态，或 `tasks.status` 真实改写，或任务行真实插入/删除并成功落账或应用
- **THEN** 总线收到对应领域 topic 上的具名事件（如 `session.claimed`、`task.status_changed`、`task.created`），且事件产生于变更成功之后

#### Scenario: 对账导致的 attention 变化发布

- **WHEN** 接管归并把旧缓冲写入可见集合，或随后被接受的 REST 写回（200/404/degraded）相对归并后基线再次改变外部可见 Attention 快照
- **THEN** 每个独立 accepted apply 各自发布至多一条 `serve_runtime.attention_changed`；canceled 与 epoch 失配不为 REST 写回再发布，也不得取消已经发布的接管事件

#### Scenario: 失败与无变化路径不发布

- **WHEN** 某次状态变更因校验失败、存储错误、CAS 未命中或 apply 无实际变化而未生效
- **THEN** 总线不产生对应领域事件

#### Scenario: 不确定提交的重同步例外

- **WHEN** 异常收敛路径的 CAS 结果不确定（CAS error 或状态重读 error），或 worker 在提交结果仍不确定的叶节点（W②b）撤销登记前需重同步（committed=false 已确定的 W③b 撤销登记不发布），或锁等待超时且触发令牌仍有效需登记 `preCleanup`/`postCleanup` 债务
- **THEN** 允许发布一次 `resync.requested`——它不表达任何域事实，仅要求订阅方重新拉取其场景全量；这是上一条规则针对不确定提交路径的 resync 例外（一次性用户输入事实的发布前提例外见「内部事件总线」正文 `task.user_activity` 条款），且每个不确定路径至多发布一次

#### Scenario: 用户输入事实即时发布

- **WHEN** 识别到某任务的合法用户主动操作（task-notifications「用户主动操作识别」）
- **THEN** 总线收到 `task.user_activity`（RID=该 task 主键），发布不以任务行变更为前提，且不推进 `updated_at`、不产生 `task.activity_changed`

#### Scenario: 慢订阅者不阻塞发布且溢出可见

- **WHEN** 某订阅者的接收缓冲已满，发布方继续发布事件
- **THEN** 发布操作立即返回，该订阅者丢弃本次事件、其溢出信号被置位并记录日志，其余订阅者不受影响

### Requirement: 事件类型与载荷结构

本变更支持的事件分为两层，各自闭合，内容不同。MUST NOT 把领域事件 Type/Payload 当作 SSE 帧，也 MUST NOT 把 SSE 裸数组回写成总线事件。

**内部领域事件**结构固定为 `{Topic, Type, RID, Payload}`。`Topic` MUST 为 `task` / `session` / `serve_runtime` / `control`。`Type` MUST 为下列闭合枚举之一；**各 Type 的 Payload 字段以 design 事件类型目录为唯一定义**（本 requirement 不重复列举），Payload 仅含该 Type 规定的小字段，MUST NOT 携带 `ActiveSessionItem` 整表或 Attention 明细。`RID` MUST 为主体实体自己的主键 ID：task 事件（`task.created`/`task.status_changed`/`task.deleted`/`task.activity_changed`/`task.user_activity`）为 task 主键；`session.claimed` / `session.touched` / `session.deleted` 为 session 主键（session 是独立聚合，owning task 由 Payload `task_id` 携带）；`serve_runtime.attention_changed` / `serve_runtime.run_status_changed` 为 ServeRuntime 主键 instVersion（owning task 由 Payload `task_id` 携带）；`sessions.aligned` 的主体为任务的会话集合（持久侧无独立对象），RID 为 task 主键；`resync.requested` 无主体，RID 允许空：

- `task.created`（topic `task`，RID=task 主键）
- `task.status_changed`（topic `task`，RID=task 主键）
- `task.deleted`（topic `task`，RID=task 主键）
- `task.activity_changed`（topic `task`，RID=task 主键；仅当提交未伴随 `task.status_changed` 时发布——真实状态迁移只发 `task.status_changed`，其 `updated_at` 推进由它承载；发布条件为**未伴随 status 迁移的任务行非 status 真实变更提交 `Changed=true`**——**不再要求 `updated_at` 跨秒推进**，覆盖 `notice`/`last_port`/`delete_mode`/同状态 `last_error`/`env_snapshot`（不直接透出 DTO，仅作保守失效通知）/align 事务内 notice 等全部既有提交点；`init_status`/`init_error` 系列写入（`ClaimInitRun`/`ClaimInitRerun`/`FinishInitRun`）真实变更 MUST 触发本事件（task-detail-stream 解除 P1.6.1 冻结）；校验失败、存储失败、CAS 未命中、无实际变化路径 MUST NOT 发布）
- `task.user_activity`（topic `task`，RID=task 主键；一次性用户输入事实，仅作 idle 通知计时取消信号，Payload 为 `struct{}{}`；不受"状态真实变更后发布"前提约束，见「内部事件总线」）
- `session.claimed` / `session.touched` / `session.deleted`（topic `session`，RID=session 主键）
- `sessions.aligned`（topic `session`，RID=task 主键）
- `serve_runtime.attention_changed`（topic `serve_runtime`，RID=ServeRuntime 主键 instVersion）
- `serve_runtime.run_status_changed`（topic `serve_runtime`，RID=ServeRuntime 主键 instVersion）
- `resync.requested`（topic `control`，RID 允许空）

未知 `Type` MUST 仍投递给订阅者，MUST NOT 使发布失败。场景适配器的消费过滤 MUST 按场景独立定义、互不影响：指挥中心 SSE 适配器 MUST 按 design 消费过滤表标脏（active-only 视图）——`task.created` 与两端都非 `active` 的 `task.status_changed`、以及 `from!=active` 的 `task.deleted` MUST NOT 标脏；合法 `task.user_activity`（Topic `task` 且 Payload 为 `struct{}{}`）MUST NOT 标脏（一次性用户输入事实，不影响 active sessions 投影；Topic/Payload 畸形按保守标脏惯例标脏）；其余本变更 Type 与未知 Type、以及溢出信号 MUST 标脏；projects 任务树场景（全状态任务树视图）的消费过滤见 projects-stream spec。适配器 MUST NOT 按 Payload 做增量合并。

**对外 SSE 帧**仅允许三种写出：`event: snapshot` 与 `event: update` 的 data MUST 为与 REST `GET /api/v1/tasks/active` 同构的 `ActiveSessionItem` 裸数组；心跳 MUST 为注释行 `: ping`，无 data。MUST NOT 把内部 `Type` 名用作 SSE `event:`，MUST NOT 发送领域 Payload、增量 diff、单任务补丁或 error 事件帧。

`ActiveSessionItem` 元素字段 MUST 为：`task_id`、`project_id`、`project_name`、`name`、`branch`、`worktree_path`（string，均必有）；`mode`（必有，取值 `worktree`/`local-path`，与任务持久化 `mode` 同源）；`last_active_at`（Unix 秒，number，必有）；`agentStatus`（`idle` \| `busy` \| `retry`，不可用或零 owned 时省略，MUST NOT 输出空串）；`attention`（object，必有）为 `{permissions: PermissionSignal[], questions: QuestionSignal[]}`，无 pending 时两数组为 `[]` 非 `null`。`PermissionSignal` 为 `{id, permission, patterns, since}`（`patterns` 为 string[]，`since` 为 Unix 秒）。`QuestionSignal` 为 `{id, questions: [{header, question}], since}`（`since` 为 Unix 秒）。空活跃列表 MUST 编码为 `[]` 非 `null`。

#### Scenario: 内部领域事件闭合且仅小载荷

- **WHEN** 任务域在任一提交点发布总线事件
- **THEN** 事件含 `Topic`/`Type`/`RID`/`Payload`，`Type` 为本 requirement 闭合枚举之一；`RID` 为主体实体主键（task 事件与 sessions.aligned 为 task 主键，session 单条事件为 session 主键，serve_runtime 事件为 instVersion，resync.requested 允许空）；`task.status_changed` 带 `{from,to}`，`task.deleted` 带 `{from}`，`task.user_activity` 无载荷字段，`serve_runtime.run_status_changed` 带 `{task_id,from,to,available}`，`serve_runtime.attention_changed` 带 `{task_id}`，`session.claimed`/`session.touched`/`session.deleted` 带 `{task_id}`，`sessions.aligned` 带计数与受影响 `session_ids`；事件 MUST NOT 携带 `ActiveSessionItem` 或 Attention 明细

#### Scenario: 对外帧与领域事件解耦

- **WHEN** 总线相继到达 `task.created`、`task.status_changed`、`session.claimed`、`serve_runtime.attention_changed`、`serve_runtime.run_status_changed`、`task.user_activity` 或 `resync.requested`
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

#### Scenario: 用户活动事件不驱动指挥中心推送

- **WHEN** 总线到达合法的 `task.user_activity` 事件
- **THEN** 指挥中心 SSE 适配器不标脏，不推送 `update`

#### Scenario: 数组元素字段完整

- **WHEN** 客户端收到 `snapshot` 或 `update` 且当前存在至少一个 active 任务
- **THEN** 每个元素含 `task_id`/`project_id`/`project_name`/`name`/`branch`/`mode`/`worktree_path`/`last_active_at`/`attention`；`attention.permissions` 与 `attention.questions` 为数组；可用时 `agentStatus` 为三态之一，不可用时该字段缺省

#### Scenario: 活跃流快照帧携带 mode

- **WHEN** 客户端收到 `GET /api/v1/tasks/active/stream` 的 `snapshot` 或 `update` 帧，且当前存在至少一个活跃任务
- **THEN** 每个元素携带 `mode` 字段（必有，`worktree` 或 `local-path`），与任务持久化 `mode` 一致
