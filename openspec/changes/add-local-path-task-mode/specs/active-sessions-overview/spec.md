# Delta: active-sessions-overview（add-local-path-task-mode）

## MODIFIED Requirements

### Requirement: 活跃会话列表 API

系统 SHALL 提供 `GET /api/v1/tasks/active` 端点（canonical 路径），旧路径 `GET /api/v1/sessions/active` MUST 保留为兼容别名：两条路径 MUST 路由到同一 handler、鉴权方式一致（与其他 `/api/v1/*` 管理 API 一致，Bearer token），响应（状态码、头与体）MUST 完全一致（兼容与调试用途）。响应 MUST 为 JSON 数组，元素字段为 `task_id`、`project_id`、`project_name`、`name`、`branch`、`worktree_path`、`mode`（必有，取值 `worktree`/`local-path`，与任务持久化 `mode` 同源）、`last_active_at`（Unix 秒）、`agentStatus`（`idle` | `busy` | `retry`，不可用时省略）、`attention`（注意力摘要，结构见 agent-attention spec：`{permissions[], questions[]}`，无 pending 时两数组为空）。`agentStatus` MUST 来自任务运行时内存态中由 active-sessions-stream 所选合规模式（模式 A 事件驱动 / 模式 B 后台探测缓存）维护的 ServeRuntime 快照（见 active-sessions-stream spec），MUST NOT 在请求路径上实时探测 opencode；快照不可用（连接代无效或尚不存在）的任务 MUST 降级为省略该字段，MUST NOT 导致整个请求失败。`attention` 数据 MUST 来自任务内存态 pending 集合，MUST NOT 引入新的 opencode 调用。数据库查询失败 MUST 返回标准错误信封 500。持久化 `kind`/`mode` 非法（数据损坏）时 MUST fail-closed 返回 500 标准错误信封，MUST NOT 输出 `mode` 缺失或取值非法的元素。无 active 任务时 MUST 返回 200 与空数组（JSON `[]`，MUST NOT 为 `null`）。响应为查询时刻快照：查询完成后任务状态变化的 MUST NOT 触发请求内重做。

#### Scenario: 正常返回活跃列表

- **WHEN** 存在若干 active 任务，且其 agent 状态快照可用（当前连接代有效）
- **THEN** 返回 200，数组元素包含全部字段（含 `mode`，`worktree`/`local-path`），`agentStatus` 为 `idle`/`busy`/`retry` 之一，`attention` 字段存在，数组按 `last_active_at` 倒序

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

#### Scenario: 持久化 kind/mode 非法 fail-closed

- **WHEN** 某 active 任务持久化的 `kind`/`mode` 非法或损坏（如 repo 项目任务 `mode` 为未知取值）
- **THEN** 返回 500 标准错误信封（fail-closed），MUST NOT 输出该元素的残缺或非法 `mode`

#### Scenario: 无活跃任务返回空数组

- **WHEN** 没有任何 active 任务
- **THEN** 返回 200 与空数组 `[]`（非 `null`）

#### Scenario: 快照语义

- **WHEN** 查询完成后、响应返回前某任务被 suspend
- **THEN** 该请求仍按查询时快照返回，由客户端订阅或后续请求收敛

#### Scenario: 未认证访问被拒

- **WHEN** 请求缺失或携带错误 token
- **THEN** 返回 401，不泄露任何资源信息
