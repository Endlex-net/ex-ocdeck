# Active Sessions Stream Specification

## Purpose

活跃会话实时推送能力：进程内领域事件总线契约（按领域 topic 发布的具名事件与载荷结构）、活跃会话 SSE 场景投影（快照帧 + 更新帧 + 心跳、事件合并与推送语义）、agent 状态事件驱动快照维护，以及前端 SSE 订阅与重连行为。

## Requirements

### Requirement: 内部事件总线

系统 SHALL 提供进程内、按领域 topic 发布的领域事件总线，用于将任务域事实通知任意场景订阅者。总线 MUST 支持按 topic 订阅与退订；发布操作 MUST 为非阻塞：订阅者缓冲满时允许丢弃该订阅者的本次事件并记录日志，MUST NOT 阻塞或失败发布方。订阅句柄 MUST 提供可观察的溢出信号：事件被丢弃时该信号被非阻塞置位（多次溢出至少一次可见），供订阅方触发自愈重推。Publish 与 Subscribe/Close MUST 并发安全；Close 后 MUST NOT 再收到事件。领域事件发布 MUST 发生在对应状态真实变更且落账/内存态应用成功之后；校验拒绝、存储失败、条件更新未命中（CAS 未提交）或无实际变化的路径 MUST NOT 产生领域事件。`resync.requested` 控制事件不受此前提约束（它不表达域事实），仅允许用于 D2 异常收敛矩阵规定的 (a) 提交结果不确定路径、(b) worker 撤销登记前重同步（仅提交结果仍不确定的叶节点 W②b；committed=false 已确定的 W③b 撤销登记 MUST NOT 发布）、(c) 锁等待超时且触发令牌仍有效的债务登记（含 tombstone 匹配直登 `postCleanup`）（见「不确定提交的重同步例外」Scenario），每条不确定路径至多一次。总线 MUST NOT 引入第三方依赖，MUST NOT 持久化事件，进程重启后以全量对齐机制收敛（不保证事件的历史重放）。本变更启用的领域 topic 为 `task`、`session`、`serve_runtime`、`control`；总线设计 MUST 允许后续新增 topic 而不修改订阅者接口。总线 MUST 按领域发布，MUST NOT 按某一 HTTP 场景裁剪发布集合。指挥中心 SSE 适配器 MUST 按 design 消费过滤表决定是否标脏，MUST NOT 把领域事件原样外发。

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
- **THEN** 允许发布一次 `resync.requested`——它不表达任何域事实，仅要求订阅方重新拉取其场景全量；这是上一条规则的唯一例外，且每个不确定路径至多发布一次

#### Scenario: 慢订阅者不阻塞发布且溢出可见

- **WHEN** 某订阅者的接收缓冲已满，发布方继续发布事件
- **THEN** 发布操作立即返回，该订阅者丢弃本次事件、其溢出信号被置位并记录日志，其余订阅者不受影响

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

### Requirement: 活跃会话 SSE 推送端点

系统 SHALL 提供 `GET /api/v1/tasks/active/stream` 端点，鉴权方式与其他 `/api/v1/*` 管理 API 一致（Bearer token）。该端点为前一变更引入且尚未发布，task 中心命名生效后旧路径 `GET /api/v1/sessions/active/stream` MUST NOT 保留别名：对旧路径的请求 MUST 返回 JSON 404 标准错误信封（MUST NOT 写 SSE 响应头、MUST NOT 建立任何事件订阅）。端点 MUST 以 `text/event-stream` 推送，所有数据帧的 data MUST 为与 REST 端点响应体完全同构的活跃会话**裸数组**。建连时序 MUST 为：认证通过 → 对领域 topic `task`/`session`/`serve_runtime`/`control` 各订阅一次并 fan-in（任一路溢出视为溢出）→ 再组装初始快照；组装期间到达且通过消费过滤表的事件 MUST 置脏标记。初始快照组装失败时 MUST 四路全部退订并返回 500 标准错误信封，MUST NOT 写入 SSE 响应头。组装成功 MUST 写 200 与 SSE 响应头、发送完整 `event: snapshot` 帧并随即 flush（首帧 MUST 立即可达，MUST NOT 滞留到后续心跳）；若脏标记已置位 MUST 紧接着进入合并窗口补发 `event: update`。此后事件到达 MUST 经合并窗口（本变更固定 500ms；后续调整须另走规格变更）合并，窗口到期以最新全量快照发送 `update` 帧；组装失败 MUST 跳过本次发送、保持脏标记并在后续事件或心跳 tick 重试，MUST NOT 关闭连接。订阅溢出信号置位时 MUST 先置脏标记再立即触发一次窗口外全量快照重推；脏标记在组装失败时保持并由后续事件或心跳 tick 继续重试（自愈信号不得丢失）；写/flush 失败按统一写路径立即退订退出（客户端重连经 snapshot 自愈）。无事件期间 MUST 以心跳注释行维持连接（默认 25s）。所有帧（snapshot/update/溢出重推/心跳）MUST 经统一写路径写出并检查写与 flush 错误：任何一次写或 flush 失败 MUST 立即退订并退出 handler，MUST NOT 依赖后续心跳或 context 兜底。帧组装 MUST 复用与 REST 端点相同的读模型组装逻辑，元素字段为 `task_id`、`project_id`、`project_name`、`name`、`branch`、`worktree_path`、`last_active_at`（Unix 秒）、`agentStatus`（`idle` | `busy` | `retry`，不可用时省略）、`attention`（同 agent-attention spec 结构）。推送路径 MUST 为纯读操作，MUST NOT 实时调用 opencode 接口，MUST NOT 产生任何写副作用。端点 MUST 在客户端断开或服务进程 context 取消时释放订阅并退出 handler；服务端关停 MUST 先取消活跃 stream 再执行 HTTP Shutdown，使关停可在其预算内完成。

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

### Requirement: agent 状态事件驱动维护

系统 SHALL 在任务运行时内存态中维护每个活跃任务的 agent 状态快照，并 MUST 从以下两个预定义合规模式中选择其一，由 Phase 0 实测门禁按预定义判据决定，MUST NOT 在实施中临时变更规格或混搭两种模式：**模式 A（事件驱动，默认）**——以任务已有的 opencode SSE 订阅中的 session 状态事件为更新来源，按 session 归属（`properties.sessionID` 反查 `task_sessions`，未命中一律忽略）更新 session 级状态；`valid` 阶段 MUST NOT 周期探测，仅 `reconcilePending` 阶段由后台重试对账。**模式 B（后台低频探测缓存）**——MUST 忽略状态事件（不解析不 apply）；runtime 建立时仅初始化为不可用；首次连接或重连的全量对齐成功并进入 `reconcilePending` 后立即首次探测写入缓存（不等待后台周期），此后由既有 `backgroundLoop` 对全部 active runtime 按固定 **30s** 周期探测（`aligning` 阶段一律跳过），探测失败置该任务不可用。模式选择是实现期常量（Phase 0 写入 design.md 后编译期固定），MUST NOT 做成运行时配置。两种模式均 MUST：按 busy > retry > idle 聚合为任务级状态（未记录状态的 session 按 idle 参与聚合）；状态来源（事件或对账/探测结果，目录级状态 map）MUST 与本任务 owned session 集合取交集，共享目录下他任务 session 的状态 MUST 忽略；仅在外部可见聚合状态或可用性变化时发布事件；推送与 REST 读路径 MUST NOT 实时调用 opencode。所有快照写入 MUST 收敛到运行时对象的唯一 apply 方法，该方法返回任务级聚合状态或可用性是否真实变化，供事件发布点使用。快照可用性 MUST 遵循连接代模型：连接代 MUST 独立于 runtime 激活代，每次 opencode SSE 连接建立生成新连接代并标记 connected；每个连接代 MUST 显式区分阶段 `aligning → reconcilePending → valid`（断流即终止该连接代）——连接建立进入 `aligning`，全量对齐成功进入 `reconcilePending`，对账/探测成功才进入 `valid`（快照可用）；`valid` 期间的探测/对账失败 MUST 使该连接代退回 `reconcilePending`（保持同一 connected epoch，仅发布一次可用→不可用），后台重试成功后 MUST 回到 `valid`（恢复路径唯一）。外部 `agentStatus` 可用的前提是当前连接代 `valid` **且** owned session 集合非空；零 owned 时 MUST 省略字段（返回空串），MUST NOT 输出 `idle`。所有改变 owned 成员的提交（claim——含激活锚定 `resolveAnchorSession` 直接 claim、delete、align 插入/删除）MUST 经同一 apply 路径维护默认 idle、重聚合与可用性：0→1 在连接代已 `valid` 时按默认 idle 变为可用；1→0 立即变为不可用。opencode SSE 首次连接/重连的全量对齐完成后 MUST 执行一次全量对账（模式 A 为 REST `/session/status`，模式 B 即周期探测的当次结果）写入内存态；对账结果写入 MUST 校验 runtime 激活代身份与捕获时连接代（epoch 匹配、仍 connected 且阶段允许），陈旧对账结果 MUST NOT 恢复可用性；对账成功但 owned 仍为空时 MUST 保持不可用，待后续 claim 经同一 apply 路径把 0→1 变为可用。对账/探测失败 MUST 保持不可用、记录日志、MUST NOT 使任务生命周期操作失败，并由既有后台循环按 30s 周期持续重试（直至成功或该连接代失效；每次尝试受 context/client timeout 限制）；后台重试 MUST 仅处理 `reconcilePending` 阶段的连接代，`aligning` 中或 align 失败的连接代 MUST NOT 经后台重试恢复可用。opencode SSE 断流（已建立连接终止且非主动取消）时 MUST 立即标记该任务当前 runtime 快照不可用；仅当可用性由可用转为不可用时 MUST 发布变更事件（原本不可用 MUST NOT 重复发布）；主动 context 取消 MUST NOT 触发该降级。不可用任务 MUST 降级为省略 `agentStatus` 字段，其余任务照常组装，MUST NOT 提前终止整批组装。`session.deleted` 或对齐移除 session 行时 MUST 同步清理对应状态条目并重聚合。既有活跃任务列表端点 `GET /api/v1/tasks/active`（canonical 路径，2026-08-26 起；兼容别名 `GET /api/v1/sessions/active`，见 active-sessions-overview spec）MUST 读该内存快照替代请求路径上的实时水合，响应字段与降级行为保持不变；快照读取 MUST 经独立的快照访问方法（不影响 `Manager.AgentStatus` 既有实时探测语义——`/tasks/{id}` 等其他消费者不在快照化范围内、行为 MUST 保持不变）；`/projects` 与 `GET /api/v1/projects/stream` 自 projects-stream 变更起同为该快照的消费者（项目摘要/帧中的 `agentStatus`，见 project-management 与 projects-stream spec），其请求/推送路径同样 MUST NOT 实时探测。

#### Scenario: 状态事件更新快照

- **WHEN** 任务已归属 session 的状态事件到达且聚合后任务级状态发生变化
- **THEN** 该任务的内存快照更新，随后发布 `serve_runtime.run_status_changed`

#### Scenario: 状态无变化不发布

- **WHEN** 状态事件到达但聚合后任务级状态与可用性均无变化
- **THEN** 内存态照常更新，不触发事件

#### Scenario: 孤儿状态事件被忽略

- **WHEN** 状态事件的 sessionID 在 `task_sessions` 中反查不到归属
- **THEN** 事件被忽略，内存态不变，不触发推送

#### Scenario: 连接建立后全量对账进入 valid

- **WHEN** 某任务的 opencode SSE 首次连接或重连，完成全量对齐且对账/探测成功
- **THEN** 该连接代进入 `valid`，内存态与对账结果一致；仅当 owned session 集合非空时外部 `agentStatus` 才可用，owned 为空则继续省略该字段

#### Scenario: 断流降级

- **WHEN** 某任务的 opencode SSE 断流
- **THEN** 该任务快照立即标记不可用；若此前可用则触发推送，后续帧中该任务 `agentStatus` 字段省略，其余任务不受影响

#### Scenario: 重连对齐在途不被后台提前恢复

- **WHEN** 某任务重连建立新连接代但全量对齐尚未完成，后台对账重试 tick 到达
- **THEN** 后台不对该连接代发起对账、不恢复其可用性；对齐失败的连接代同样不得经后台恢复

#### Scenario: 对账失败保持不可用并周期重试

- **WHEN** 全量对齐完成但 `/session/status` 对账失败
- **THEN** 快照保持不可用、记录日志、任务生命周期不受影响，后续由后台循环按 30s 周期持续重试对账，直至成功或该连接代失效（每次尝试受超时限制）

#### Scenario: REST 端点读快照

- **WHEN** 客户端请求 `GET /api/v1/tasks/active`（或其兼容别名 `GET /api/v1/sessions/active`）
- **THEN** 响应的 `agentStatus` 来自内存快照，该请求不发起任何 opencode `/session/status` 调用

#### Scenario: 零 owned 省略字段

- **WHEN** 当前连接代已对账成功进入 `valid`，但该任务 owned session 集合为空
- **THEN** 该任务 `agentStatus` 省略（空串），MUST NOT 输出 `idle`

#### Scenario: owned 从 0 变为 1

- **WHEN** 连接代已 `valid` 且 owned 为空，随后 `resolveAnchorSession` 或其它 claim 路径真实插入首个 session 行
- **THEN** 经唯一 apply 路径按默认 idle 聚合为可用，发布一次 `serve_runtime.run_status_changed`，后续帧该任务 `agentStatus` 为 `idle`

#### Scenario: owned 从 1 变为 0

- **WHEN** 快照当前可用且 owned 仅剩一行，该行被 delete 或 align 移除
- **THEN** 经唯一 apply 路径立即变为不可用并发布一次 available→unavailable，后续帧该任务 `agentStatus` 省略

### Requirement: 前端 SSE 订阅与重连

指挥中心 MUST 通过 fetch + ReadableStream 的 SSE 订阅获取活跃会话数据（携带 `Authorization: Bearer`，认证模型与既有 REST 一致），MUST NOT 再对 `/api/v1/tasks/active` 做固定间隔轮询。订阅 MUST 随指挥中心页面挂载建立、卸载关闭。`snapshot` 与 `update` 帧 MUST 走同一解析路径并以帧内裸数组更新页面快照状态。sessions 首帧到达前 MUST 独立展示「连接中」，MUST NOT 因此进入真空态，也 MUST NOT 把门禁升级为整页 loading；若 projects 快照已有数据 MUST 继续按 join 规则渲染 projects-only 任务。首次连接错误 MUST 展示错误提示且不与「连接中」并存。连接断开或非 401 错误 MUST 按指数退避自动重连（起步 1s、上限 30s、首个有效帧到达后重置）；收到 401 MUST 按既有约定清除 token 并广播未授权事件且 MUST NOT 继续重连。帧解析 MUST 忽略注释行与未知 event 类型；`snapshot`/`update` 帧协议错误（非法 JSON 或 data 非数组）MUST 触发错误提示、保留旧数据、终止当前连接并退避重连，且 MUST NOT 因此重置退避。推送/订阅异常期间 MUST 保留上次成功数据并展示错误提示，不闪现空态。与 projects 快照的双快照 join 规则 MUST 保持不变。

#### Scenario: 订阅替代轮询

- **WHEN** 用户打开指挥中心
- **THEN** 页面建立一条 SSE 订阅，首帧后展示活跃任务数据，不存在对 tasks/active 的定时轮询请求

#### Scenario: 首帧前保留 projects-only 渲染

- **WHEN** 用户打开指挥中心时 projects 快照已有任务，sessions SSE 首帧尚未到达
- **THEN** 页面按 join 规则继续渲染 projects-only 任务，独立展示「连接中」，不进入整页 loading，也不因 sessions 未就绪展示真空态

#### Scenario: 断线自动重连

- **WHEN** SSE 连接因网络或服务端原因断开
- **THEN** 页面按指数退避自动重连，重连成功收到有效帧后以新快照收敛，期间保留上次成功数据并提示错误

#### Scenario: 401 停止重连

- **WHEN** SSE 连接收到 401
- **THEN** 页面清除 token、进入未授权流程，且不再发起重连

#### Scenario: 卸载释放连接

- **WHEN** 用户离开指挥中心页面（含正处于重连退避等待期间）
- **THEN** SSE 连接被关闭、在途退避计时器被取消、不再发起任何 fetch，服务端订阅随之释放
