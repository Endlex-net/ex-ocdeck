## MODIFIED Requirements

### Requirement: 全状态任务树消费过滤

projects 场景适配器 MUST 按下列口径判定领域事件是否标脏，标脏后 MUST 重组装全量快照重推，MUST NOT 按 Payload 做增量合并：

- `task.created`、`task.status_changed`（**任意** from/to 迁移，含两端均非 `active` 的过渡/失败/归档迁移）、`task.deleted`（**任意** `from`，含非 active 任务的删除）、`task.activity_changed`：MUST 标脏——本场景呈现全部非删除态任务，任一任务的进入、迁移、离开或可见字段变化都改变任务树投影；
- `task.user_activity`：合法事件（Topic `task` 且 Payload 为 `struct{}{}`）MUST NOT 标脏——一次性用户输入事实，不影响任务树投影；Topic/Payload 畸形按保守标脏惯例标脏；
- 全部 `session.*`（`session.claimed`/`session.touched`/`session.deleted`/`sessions.aligned`）、`serve_runtime.attention_changed`、`serve_runtime.run_status_changed`、`resync.requested`、未知 `Type`：MUST 标脏（保守标脏，避免漏推）；
- 任一路订阅溢出信号置位：MUST 先置脏再触发窗口外全量重推。

本场景与活跃会话流（active-sessions-stream spec）的消费过滤 MUST 相互独立定义、互不影响：本场景为全状态任务树视图（侧栏/项目管理页），活跃会话流为 active-only 视图（指挥中心）。项目 CRUD（创建/重命名/删除项目）不产生领域事件（本能力不新增 project 事件），该类变更不经本流感知，由前端低频兜底轮询覆盖（见「前端 projects store 订阅与兜底轮询」Requirement）。

#### Scenario: task.created 触发更新

- **WHEN** 某项目下创建新任务（`task.created` 发布）
- **THEN** 本流在合并窗口后推送 `update`（任务树新增行），而已连接的活跃会话流不因此单独推送 `update`

#### Scenario: 非 active 迁移触发更新

- **WHEN** 发生两端均非 `active` 的状态迁移（如 `suspended → archived`、`creating → creation_failed`）或 `from != active` 的 `task.deleted`
- **THEN** 本流推送 `update`（树内该任务行变化/移除），活跃会话流不因此推送

#### Scenario: 跨越 active 边界的迁移触发更新

- **WHEN** 发生 `(from==active) != (to==active)` 的 `task.status_changed`
- **THEN** 本流与活跃会话流均推送 `update`

#### Scenario: resync 与未知 Type 保守标脏

- **WHEN** 到达 `resync.requested` 或未知 `Type` 事件
- **THEN** 本流标脏并在窗口/立即重推路径推送最新全量快照

#### Scenario: 用户活动事件不触发更新

- **WHEN** 到达合法的 `task.user_activity` 事件
- **THEN** 本流不标脏，不推送 `update`
