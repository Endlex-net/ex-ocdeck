# Proposal: SSE 化活跃会话推送与内部事件总线

## Why

指挥中心当前以 5 秒固定间隔轮询 `GET /api/v1/sessions/active`：每次请求除 SQLite 聚合与内存快照外，还要对每个活跃任务实时探测其 opencode serve 的 `/session/status`（并发上限 8、3 秒预算）。这带来两个问题：数据延迟最高可达一个轮询周期加水合耗时；服务端开销随活跃任务数线性增长且绝大部分轮询返回的是无变化数据。后端实际上已经通过 per-task 的 opencode SSE 订阅（`handleSSEEvent`）实时接收 session/attention 变更，但层间没有事件通知机制，API 层只能在请求到达时现查——轮询是这个缺失的补丁。引入内部事件总线 + SSE 推送可以把"变更产生"与"客户端可见"之间的延迟降到秒级以内，并把服务端开销从"按请求聚合"降为"按变更推送"。

## What Changes

- **Phase 1：内部 DDD 分层重构 + 领域事件总线 + 全部事件生产（一等交付物）**：把现有单体 `internal/task` Manager 重构为 domain/application/infrastructure/interfaces 分层——Task（聚合根）/Session（独立聚合）/ServeRuntime（内存实体，Registry 持有）领域模型、consumer-owned Repository ports、结构化提交结果；进程内、按领域 topic 发布的**领域事件**总线（不是场景脏信号）由 application 集中 commit helper 在提交成功后发布。总线 `Topic` 闭合为 `task` / `session` / `serve_runtime` / `control`（与 design/tasks/stream spec 逐字一致）；本变更闭合的领域事件为具名 `Type` + 小载荷，11 个 Type 与 design/tasks/stream spec 逐字一致：`task.created` / `task.status_changed` / `task.deleted` / `task.activity_changed` / `session.claimed` / `session.touched` / `session.deleted` / `sessions.aligned` / `serve_runtime.attention_changed` / `serve_runtime.run_status_changed` / `resync.requested`；（各事件 Payload 以 design 事件类型目录为唯一定义，此处不重复列举）。总线按领域发布、不按场景裁剪。**全部事件生产点（含 Phase 0 实测门禁与 agentStatus 事件驱动建模）在 Phase 1 完成挂接**。Phase 1 不改变任何 REST/WS 对外行为与前端轮询（唯一显式差异：幂等同值写不再推进 `updated_at`，见 task-lifecycle delta）。
- **SSE 是指挥中心场景投影，不是领域事件外发**：`GET /api/v1/sessions/active/stream` 只消费会影响活跃会话快照的领域事件（见 design 消费过滤表），对外仅 `snapshot` / `update` / `: ping`，data 为与 REST 同构的裸数组。MUST NOT 把内部 `Type` 或载荷原样送到浏览器。后续 `/projects` 等场景可另写适配器订阅同一总线，本次不接入。
- **新增 SSE 端点** `GET /api/v1/sessions/active/stream`：连接建立即推送一次全量快照（与现有 REST 响应同构），此后由事件驱动推送更新；推送采用固定 500ms 事件合并窗口（窗口内多次变更合并为一次推送；后续调整须另走规格变更）。
- **agentStatus 改为事件驱动维护**：复用现有 per-task opencode SSE 订阅中的 session 状态事件，在内存中维护每个任务的 agentStatus 快照；`GET /api/v1/sessions/active` 与 SSE 推送共享该快照读取（经独立快照访问方法），请求路径上不再对 opencode 做实时探测（该端点的水合降级语义同步调整，见 specs delta）；**既有 `Manager.AgentStatus` 实时探测链保持不变**，`/projects`、`/tasks/{id}` 等其他消费者继续走该链。
- **前端指挥中心改为 SSE 订阅**：使用 fetch + ReadableStream 手动解析 SSE 帧（可携带 `Authorization: Bearer`，认证模型不变），自实现断线重连退避；移除本页 5s 轮询，不保留轮询 fallback。
- **保留既有 REST 端点** `GET /api/v1/sessions/active`：对外契约不变（兼容与调试用途），内部实现改为读事件驱动维护的快照。

## Capabilities

### New Capabilities

- `active-sessions-stream`: 领域事件总线契约、指挥中心 SSE 场景投影（快照帧 + 更新帧 + 心跳）、事件合并与推送语义、前端订阅与重连行为。

### Modified Capabilities

- `active-sessions-overview`: 「前端活跃会话页面」要求中的固定 5 秒轮询改为 SSE 订阅驱动；「活跃会话列表 API」中 agentStatus 实时水合语义调整为读事件驱动快照（字段与降级行为保持一致）。
- `command-center`: 指挥中心数据源描述中 sessions/active 的「5s single-flight 轮询」同步为 SSE 订阅快照（双快照 join 规则不变）。
- `task-lifecycle`: 新增「幂等同值写不推进 `updated_at`」要求——`tasks.updated_at` 语义从「最近一次写尝试」收紧为「最近一次真实变更」（同值原子 no-op 的显式行为差异；canonical 对该字段的读侧断言保持成立）。

## Impact

- **新增代码**：`internal/domain/{task,session,event}`、`internal/application/{task,runtime}`、`internal/infrastructure/{sqlite,eventbus}`（Phase 1 分层与总线）；`internal/api` SSE handler 与订阅管理；`web/src` fetch streaming SSE 客户端与重连逻辑（Phase 2）。
- **修改代码**：`internal/task`（strangler 迁移为 facade，编排逻辑移入 application 层；`handleSSEEvent`、任务状态生命周期操作挂事件发布点；agentStatus 内存态维护）；`internal/store`（同值原子 no-op 与结构化提交结果，经 sqlite adapter 暴露）；`internal/api/tasks.go`（REST handler 水合改读快照）；`web/src/pages/CommandCenterPage.tsx`（轮询移除）；`web/src/api.ts`（流式客户端）。
- **无变更项**：数据库 schema 不变；认证模型不变（仍 Bearer token，SSE 经 fetch 流携带）；无新增第三方依赖；不涉及代理/压缩中间件（当前栈无压缩，SSE 无缓冲顾虑）。
- **风险**：Phase 1 为大规模内部重构（写点众多、ServeRuntime 换代与债务并发），以 design D0 的迁移顺序、不变量清单与 race 测试对冲；opencode 状态事件的确切类型字符串与可靠性需在实现期对运行中实例实测确认（Phase 0 门禁，沿用既有 fail-closed 归属校验，未命中一律忽略）；事件驱动 agentStatus 若实测不可靠，切换到 spec 预定义的模式 B（后台低频探测缓存），总线与 SSE 协议不受影响。
