# active-sessions-overview 变更 Delta

## MODIFIED Requirements

### Requirement: 水合调用链 context 收敛

系统 SHALL 使下列两条 agent 状态调用链全程遵守调用方传入的 context：取消或超时 MUST 使调用在预算内终止。

1. **既有实时探测链**（`Manager.AgentStatus` → tmux 环境读取 → opencode HTTP）：canonical 对该链的 context 约束保持不变。`/tasks/{id}` 等范围外消费者 MUST 继续走该链，其对外行为 MUST 保持不变（实时探测语义显式保持）。`/projects` 自本变更起不再走该链：项目列表/详情任务摘要的 `agentStatus` 改读 ServeRuntime 内存快照（快照维护见 active-sessions-stream spec「agent 状态事件驱动维护」），快照不可用降级省略、可能滞后于对账周期（见 project-management spec delta）。
2. **快照对账/探测链**（连接代对账触发 → tmux 环境读取 → opencode HTTP）：对账/探测 MUST 遵守调用方 context。该链的触发时机按模式区分，MUST NOT 混用（见 active-sessions-stream spec）：模式 A 仅在 opencode SSE 首次连接/重连的全量对齐完成后发起首次对账，`valid` 阶段 MUST NOT 周期探测，仅 `reconcilePending` 由后台按 30s 周期持续重试，直至成功或该连接代失效（每次尝试受超时限制）；模式 B 在全量对齐成功进入 `reconcilePending` 后立即首次探测，此后由既有 `backgroundLoop` 以固定 30s 周期探测 `valid`/`reconcilePending` 的 active runtime（`aligning` 跳过）。

两条链均 MUST NOT 出现在 `GET /api/v1/tasks/active`（canonical；兼容别名 `GET /api/v1/sessions/active` 行为一致）、`GET /api/v1/tasks/active/stream`、`GET /api/v1/projects` 或 `GET /api/v1/projects/stream` 的请求/推送路径上。`process` 层 MUST 提供 context-aware 的会话环境读取能力；既有非 context 接口的对外行为 MUST 保持不变（既有调用方不受影响）。

#### Scenario: 超时内终止对账

- **WHEN** 对账或实时探测的 context 已超时或取消，而底层 tmux 环境读取尚未返回
- **THEN** 该调用在 context 到期后迅速返回，不等待底层固定超时

#### Scenario: 既有接口行为不变

- **WHEN** 存量代码路径（suspend/attach/reconcile/delete）继续调用非 context 的会话环境读取接口，或 `/tasks/{id}` 继续调用 `Manager.AgentStatus`
- **THEN** 其行为（含超时上限、错误语义与实时探测）与本变更前一致

#### Scenario: 请求路径无水合

- **WHEN** 客户端请求 `GET /api/v1/tasks/active`（或兼容别名 `GET /api/v1/sessions/active`）、`GET /api/v1/projects`，或订阅 `GET /api/v1/tasks/active/stream`、`GET /api/v1/projects/stream`
- **THEN** 处理/推送过程中不发起 tmux 环境读取或 opencode HTTP 调用，`agentStatus` 读内存快照

### Requirement: 活跃会话列表 API

系统 SHALL 提供 `GET /api/v1/tasks/active` 端点（canonical 路径），旧路径 `GET /api/v1/sessions/active` MUST 保留为兼容别名：两条路径 MUST 路由到同一 handler、鉴权方式一致（与其他 `/api/v1/*` 管理 API 一致，Bearer token），响应（状态码、头与体）MUST 完全一致（兼容与调试用途）。响应 MUST 为 JSON 数组，元素字段为 `task_id`、`project_id`、`project_name`、`name`、`branch`、`worktree_path`、`last_active_at`（Unix 秒）、`agentStatus`（`idle` | `busy` | `retry`，不可用时省略）、`attention`（注意力摘要，结构见 agent-attention spec：`{permissions[], questions[]}`，无 pending 时两数组为空）。`agentStatus` MUST 来自任务运行时内存态中由 active-sessions-stream 所选合规模式（模式 A 事件驱动 / 模式 B 后台探测缓存）维护的 ServeRuntime 快照（见 active-sessions-stream spec），MUST NOT 在请求路径上实时探测 opencode；快照不可用（连接代无效或尚不存在）的任务 MUST 降级为省略该字段，MUST NOT 导致整个请求失败。`attention` 数据 MUST 来自任务内存态 pending 集合，MUST NOT 引入新的 opencode 调用。数据库查询失败 MUST 返回标准错误信封 500。无 active 任务时 MUST 返回 200 与空数组（JSON `[]`，MUST NOT 为 `null`）。响应为查询时刻快照：查询完成后任务状态变化的 MUST NOT 触发请求内重做。

#### Scenario: 正常返回活跃列表

- **WHEN** 存在若干 active 任务，且其 agent 状态快照可用（当前连接代有效）
- **THEN** 返回 200，数组元素包含全部字段，`agentStatus` 为 `idle`/`busy`/`retry` 之一，`attention` 字段存在，数组按 `last_active_at` 倒序

#### Scenario: 兼容别名响应一致

- **WHEN** 客户端分别请求 `GET /api/v1/tasks/active` 与 `GET /api/v1/sessions/active`（相同数据状态）
- **THEN** 两条路径返回相同的响应：同一鉴权语义、相同的 Content-Type 与完全一致的响应体

#### Scenario: 快照不可用降级

- **WHEN** 某 active 任务的 agent 状态快照不可用（opencode SSE 断流、对账失败或快照尚不存在）
- **THEN** 返回 200，该任务元素仍存在于数组中且 `agentStatus` 缺省，其余任务不受影响

#### Scenario: 请求路径无实时探测

- **WHEN** 客户端连续多次请求该端点
- **THEN** 所有请求的 `agentStatus` 均读内存快照，过程中不发起任何 opencode `/session/status` 调用

#### Scenario: 数据库查询失败

- **WHEN** 底层查询返回错误
- **THEN** 返回 500 标准错误信封

#### Scenario: 无活跃任务返回空数组

- **WHEN** 没有任何 active 任务
- **THEN** 返回 200 与空数组 `[]`（非 `null`）

#### Scenario: 快照语义

- **WHEN** 查询完成后、响应返回前某任务被 suspend
- **THEN** 该请求仍按查询时快照返回，由客户端订阅或后续请求收敛

#### Scenario: 未认证访问被拒

- **WHEN** 请求缺失或携带错误 token
- **THEN** 返回 401，不泄露任何资源信息
