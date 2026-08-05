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

系统 SHALL 使 agent 状态水合调用链（`AgentStatus` → tmux 环境读取 → opencode HTTP）全程遵守调用方传入的 context：取消或超时 MUST 使水合在预算内终止。`process` 层 MUST 提供 context-aware 的会话环境读取能力；既有非 context 接口的对外行为 MUST 保持不变（既有调用方不受影响）。

#### Scenario: 超时内终止水合

- **WHEN** 水合 context 已超时或取消，而底层 tmux 环境读取尚未返回
- **THEN** 水合调用在 context 到期后迅速返回，不等待底层固定超时

#### Scenario: 既有接口行为不变

- **WHEN** 存量代码路径（suspend/attach/reconcile/delete）继续调用非 context 的会话环境读取接口
- **THEN** 其行为（含超时上限与错误语义）与收敛前一致

### Requirement: 活跃会话列表 API

系统 SHALL 提供 `GET /api/v1/sessions/active` 端点，鉴权方式与其他 `/api/v1/*` 管理 API 一致（Bearer token）。响应 MUST 为 JSON 数组，元素字段为 `task_id`、`project_id`、`project_name`、`name`、`branch`、`worktree_path`、`last_active_at`（Unix 秒）、`agentStatus`（`idle` | `busy` | `retry`，不可用时省略）。端点 MUST 对每个返回任务实时水合 `agentStatus`；水合 MUST 并发执行（每请求并发上限 8）并受水合预算约束（数据库查询完成后起 3 秒）；单个任务水合失败或超时 MUST 降级为该字段缺省，MUST NOT 导致整个请求失败。数据库查询失败 MUST 返回标准错误信封 500 且 MUST NOT 开始水合。无 active 任务时 MUST 返回 200 与空数组（JSON `[]`，MUST NOT 为 `null`）。响应为查询时刻快照：查询完成后任务状态变化的 MUST NOT 触发请求内重做。

#### Scenario: 正常返回活跃列表

- **WHEN** 存在若干 active 任务，且其凭据、端口、归属 session 与 serve 状态接口均可用
- **THEN** 返回 200，数组元素包含全部字段，`agentStatus` 为 `idle`/`busy`/`retry` 之一，数组按 `last_active_at` 倒序

#### Scenario: 单个任务水合失败降级

- **WHEN** 某 active 任务的 serve 实例不可达或水合超时
- **THEN** 返回 200，该任务元素仍存在于数组中且 `agentStatus` 缺省，其余任务不受影响

#### Scenario: 水合并发上限与预算

- **WHEN** 同时水合的任务数超过 8
- **THEN** 任意时刻进行中的水合调用不超过 8 个；水合预算到期后未完成的调用终止，对应元素 `agentStatus` 缺省，请求仍正常返回

#### Scenario: 数据库查询失败

- **WHEN** 底层查询返回错误
- **THEN** 返回 500 标准错误信封，且不发起任何水合调用

#### Scenario: 客户端请求取消

- **WHEN** 客户端在水合过程中断开连接
- **THEN** 水合调用随 context 取消而终止，不产生写副作用

#### Scenario: 无活跃任务返回空数组

- **WHEN** 没有任何 active 任务
- **THEN** 返回 200 与空数组 `[]`（非 `null`）

#### Scenario: 快照语义

- **WHEN** 查询完成后、响应返回前某任务被 suspend
- **THEN** 该请求仍按查询时快照返回，由客户端下一轮轮询收敛

#### Scenario: 未认证访问被拒

- **WHEN** 请求缺失或携带错误 token
- **THEN** 返回 401，不泄露任何资源信息

### Requirement: 列表全链路只读

跨项目活跃列表的完整请求路径（数据库查询、tmux 环境读取、opencode 状态查询）MUST 为纯读操作：MUST NOT 写数据库、MUST NOT 触发会话 align、MUST NOT 改变任务状态机、MUST NOT 启动或停止任何进程。

#### Scenario: 列表请求无写副作用

- **WHEN** 连续发起多次活跃列表请求
- **THEN** 数据库内容、任务状态与进程集合除外部因素外不发生变化

### Requirement: 前端活跃会话页面

系统 SHALL 提供独立前端页面（路由 `#/active`）展示跨项目活跃任务列表：每行展示项目名称、任务名、分支、最近活跃时间、agent 状态徽标；页面 MUST 以固定 5 秒间隔轮询刷新，且 MUST single-flight（上一请求未返回时跳过本次轮询，禁止请求重叠）；点击行 MUST 跳转对应任务工作台 `#/task/:id`。页面 MUST 区分三种状态：初次加载（loading）、请求失败（错误提示并保留上次成功数据）、真正空态（请求成功且无活跃任务，展示引导文案）。项目列表页（`#/`）MUST 提供进入该页面的入口。

#### Scenario: 浏览活跃任务并跳转

- **WHEN** 用户打开 `#/active` 且存在活跃任务
- **THEN** 列表按最近活跃时间倒序展示各行；点击某行后进入该任务的工作台页面

#### Scenario: 轮询不重叠

- **WHEN** 某次轮询请求超过 5 秒仍未返回
- **THEN** 后续轮询 tick 被跳过，任意时刻至多一个在途列表请求

#### Scenario: 请求失败保留旧数据

- **WHEN** 页面已成功展示列表后某次轮询失败
- **THEN** 页面展示错误提示且保留上次成功的列表数据，不闪现空态

#### Scenario: 空态展示

- **WHEN** 用户打开 `#/active` 且请求成功返回空数组
- **THEN** 页面展示空态引导文案，而非报错或空白

#### Scenario: 从项目列表进入

- **WHEN** 用户在项目列表页点击活跃会话入口
- **THEN** 导航至 `#/active`
