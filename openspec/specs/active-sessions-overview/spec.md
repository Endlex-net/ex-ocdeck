## Purpose

跨项目活跃会话总览：为用户提供单实例内所有项目的活跃任务聚合视图（独立 API 与前端页面），用于在多项目多 worktree 并行工作时快速跟进各任务状态并跳转回任务工作台。

## Requirements

### Requirement: 跨项目活跃任务查询

系统 SHALL 提供跨项目活跃任务聚合查询：返回所有 `tasks.status='active'` 的任务，每行包含任务 ID、所属项目 ID 与名称、任务名、分支、worktree 路径、最近活跃时间。最近活跃时间 MUST 取该任务 `task_sessions.last_seen_at` 的最大值；任务无任何会话行时 MUST 回退为 `tasks.updated_at`。`last_seen_at` 直接持久化自 opencode `time.updated`（毫秒），与秒级 `tasks.updated_at` 在存量库中混存；查询 MUST 在取最大值与排序前将毫秒值（≥ 1e11）归一化为 Unix 秒，保证混合单位数据的排序与输出一致。结果 MUST 按最近活跃时间倒序排列，时间相同则以任务 ID 升序作 tie-breaker。该查询 MUST NOT 引起数据库 schema 变更。

#### Scenario: 多项目活跃任务聚合

- **WHEN** 项目 A 有 2 个 active 任务、项目 B 有 1 个 active 任务、项目 C 无 active 任务
- **THEN** 查询返回 3 行，每行携带正确的项目名称，且按最近活跃时间倒序

#### Scenario: 混合单位数据排序正确

- **WHEN** 任务甲有毫秒级 `last_seen_at`（如 1785797826297，实际时间较早），任务乙无会话行且 `updated_at`（秒）实际时间较晚
- **THEN** 归一化后任务乙排在任务甲之前，两者输出的最近活跃时间均为 Unix 秒

#### Scenario: 无会话行的任务回退时间

- **WHEN** 某 active 任务没有任何 `task_sessions` 行
- **THEN** 该行的最近活跃时间为 `tasks.updated_at`

#### Scenario: 非 active 任务不出现

- **WHEN** 存在 suspended / archived / creating 等非 active 状态任务
- **THEN** 查询结果中不包含这些任务

#### Scenario: 排序 tie-breaker

- **WHEN** 两个 active 任务的最近活跃时间相同
- **THEN** 任务 ID 较小者排在前面，结果顺序在多次查询间稳定

### Requirement: 水合调用链 context 收敛

系统 SHALL 使下列两条 agent 状态调用链全程遵守调用方传入的 context：取消或超时 MUST 使调用在预算内终止。

1. **既有实时探测链**（`Manager.AgentStatus` → tmux 环境读取 → opencode HTTP）：canonical 对该链的 context 约束保持不变。`/tasks/{id}` 等范围外消费者 MUST 继续走该链，其对外行为 MUST 保持不变（实时探测语义显式保持）。`/projects` 自本变更起不再走该链：项目列表/详情任务摘要的 `agentStatus` 改读 ServeRuntime 内存快照（快照维护见 active-sessions-stream spec「agent 状态事件驱动维护」），快照不可用降级省略、可能滞后于对账周期（见 project-management spec）。
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

### Requirement: 列表全链路只读

跨项目活跃列表的完整请求路径（数据库查询、内存快照读取）MUST 为纯读操作：MUST NOT 写数据库、MUST NOT 触发会话 align、MUST NOT 改变任务状态机、MUST NOT 启动或停止任何进程、MUST NOT 发起 tmux 或 opencode 调用。

#### Scenario: 列表请求无写副作用

- **WHEN** 连续发起多次活跃列表请求
- **THEN** 数据库内容、任务状态与进程集合除外部因素外不发生变化，且不产生任何 tmux/opencode 出站调用

### Requirement: 前端活跃会话页面

跨项目活跃任务列表 MUST 并入指挥中心首页（`#/active` 重定向至 `#/`，见 web-ui-shell spec 路由收敛要求），MUST NOT 保留独立页面。指挥中心 MUST 以 SSE 订阅（见 active-sessions-stream spec）获取活跃任务数据，MUST NOT 采用固定间隔轮询；活跃任务行 MUST 展示项目名称、任务名、分支、最近活跃时间、agent 状态徽标与注意力标记；点击行 MUST 跳转对应任务工作台 `#/task/:id`。sessions 快照未就绪（首帧到达前）MUST 独立展示「连接中」，MUST NOT 因此进入真空态，也 MUST NOT 把门禁升级为整页 loading；若 projects 快照已有数据 MUST 继续按 join 规则渲染 projects-only 任务。订阅异常 MUST 展示错误提示并保留上次成功的 sessions 数据。真正空态仅当两侧均已成功初始化且无任务时展示引导文案。侧栏 MUST 提供进入指挥中心的顶层导航项。

#### Scenario: 浏览活跃任务并跳转

- **WHEN** 用户打开指挥中心且存在活跃任务
- **THEN** 「其余活跃任务」分区按最近活跃时间倒序展示（「需要关注」分区的排序规则见 command-center spec，不受本要求约束）；点击某行后进入该任务的工作台页面

#### Scenario: SSE 帧驱动更新

- **WHEN** 订阅存续期间收到 snapshot 或 update 帧
- **THEN** 页面以帧内容更新活跃任务列表，不发起轮询请求

#### Scenario: 首帧前保留 projects-only 渲染

- **WHEN** 用户打开指挥中心时 projects 快照已有任务，sessions SSE 首帧尚未到达
- **THEN** 页面按 join 规则继续渲染 projects-only 任务，独立展示「连接中」，不进入整页 loading，也不因 sessions 未就绪展示真空态

#### Scenario: projects 为空且 sessions 未首帧展示连接中

- **WHEN** 用户打开指挥中心时 projects 快照已成功初始化但无任务，sessions SSE 首帧尚未到达
- **THEN** 页面独立展示「连接中」，不进入整页 loading，也不展示真空态

#### Scenario: 订阅异常保留旧数据

- **WHEN** 页面已成功展示列表后 SSE 连接断开且重连尚未成功
- **THEN** 页面展示错误提示且保留上次成功的列表数据，不闪现空态

#### Scenario: 空态展示

- **WHEN** 用户打开指挥中心，projects 与 sessions 两侧均成功初始化，且双快照 join 后「需要关注」「其余活跃任务」「挂起与归档」三区均无任务
- **THEN** 页面展示空态引导文案，而非报错或空白

#### Scenario: 旧路由重定向

- **WHEN** 用户打开 `#/active`
- **THEN** 应用重定向至 `#/` 指挥中心
