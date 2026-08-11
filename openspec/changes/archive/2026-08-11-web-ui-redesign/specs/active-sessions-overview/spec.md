## MODIFIED Requirements

### Requirement: 活跃会话列表 API

系统 SHALL 提供 `GET /api/v1/sessions/active` 端点，鉴权方式与其他 `/api/v1/*` 管理 API 一致（Bearer token）。响应 MUST 为 JSON 数组，元素字段为 `task_id`、`project_id`、`project_name`、`name`、`branch`、`worktree_path`、`last_active_at`（Unix 秒）、`agentStatus`（`idle` | `busy` | `retry`，不可用时省略）、`attention`（注意力摘要，结构见 agent-attention spec：`{permissions[], questions[]}`，无 pending 时两数组为空）。端点 MUST 对每个返回任务实时水合 `agentStatus`；水合 MUST 并发执行（每请求并发上限 8）并受水合预算约束（数据库查询完成后起 3 秒）；单个任务水合失败或超时 MUST 降级为该字段缺省，MUST NOT 导致整个请求失败。`attention` 数据 MUST 来自任务内存态 pending 集合，MUST NOT 引入新的 opencode 调用。数据库查询失败 MUST 返回标准错误信封 500 且 MUST NOT 开始水合。无 active 任务时 MUST 返回 200 与空数组（JSON `[]`，MUST NOT 为 `null`）。响应为查询时刻快照：查询完成后任务状态变化的 MUST NOT 触发请求内重做。

#### Scenario: 正常返回活跃列表

- **WHEN** 存在若干 active 任务，且其凭据、端口、归属 session 与 serve 状态接口均可用
- **THEN** 返回 200，数组元素包含全部字段，`agentStatus` 为 `idle`/`busy`/`retry` 之一，`attention` 字段存在，数组按 `last_active_at` 倒序

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

### Requirement: 前端活跃会话页面

跨项目活跃任务列表 MUST 并入指挥中心首页（`#/active` 重定向至 `#/`，见 web-ui-shell spec 路由收敛要求），MUST NOT 保留独立页面。指挥中心 MUST 以固定 5 秒间隔 single-flight 轮询活跃任务数据（上一请求未返回时跳过本次轮询，禁止请求重叠），活跃任务行 MUST 展示项目名称、任务名、分支、最近活跃时间、agent 状态徽标与注意力标记；点击行 MUST 跳转对应任务工作台 `#/task/:id`。页面 MUST 区分三种状态：初次加载（loading）、请求失败（错误提示并保留上次成功数据）、真正空态（请求成功且无任务，展示引导文案）。侧栏 MUST 提供进入指挥中心的顶层导航项。

#### Scenario: 浏览活跃任务并跳转

- **WHEN** 用户打开指挥中心且存在活跃任务
- **THEN** 「其余活跃任务」分区按最近活跃时间倒序展示（「需要关注」分区的排序规则见 command-center spec，不受本要求约束）；点击某行后进入该任务的工作台页面

#### Scenario: 轮询不重叠

- **WHEN** 某次轮询请求超过 5 秒仍未返回
- **THEN** 后续轮询 tick 被跳过，任意时刻至多一个在途列表请求

#### Scenario: 请求失败保留旧数据

- **WHEN** 页面已成功展示列表后某次轮询失败
- **THEN** 页面展示错误提示且保留上次成功的列表数据，不闪现空态

#### Scenario: 空态展示

- **WHEN** 用户打开指挥中心且请求成功返回空数据
- **THEN** 页面展示空态引导文案，而非报错或空白

#### Scenario: 旧路由重定向

- **WHEN** 用户打开 `#/active`
- **THEN** 应用重定向至 `#/` 指挥中心
