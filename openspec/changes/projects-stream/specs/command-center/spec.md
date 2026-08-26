# command-center 变更 Delta

## MODIFIED Requirements

### Requirement: 指挥中心首页

系统 SHALL 将指挥中心作为应用首页（`#/`），按分区展示：「需要关注」（置顶）、「其余活跃任务」、「挂起与归档」。页头 MUST 提供新建任务入口与最近刷新时间指示。

数据源：指挥中心 MUST 从 App 层共享 projects store（`/api/v1/projects/stream` SSE 订阅 + 常驻低频兜底轮询，与侧栏同一数据源，见 projects-stream spec 与 web-ui-shell spec；MUST NOT 自行重复轮询 `/projects`）获取全状态任务树，并以 `GET /api/v1/tasks/active/stream` 的 SSE 订阅快照（见 active-sessions-stream spec）补充活跃任务的 `last_active_at`、`agentStatus` 与 `attention`。

双快照 join 规则（按 `task_id` 关联）：**字段级来源**——`status/init_status/branch/worktree_path/last_error/notice/updated_at` 及身份字段（`name/project_id/project_name`）以 projects 快照为准；`last_active_at/agentStatus/attention` 以 tasks/active 快照为准（projects 摘要的 `agentStatus` MUST 仅在该任务缺席 tasks/active 快照时使用，即 projects-only 活跃任务）；不存在两端同字段的优先级冲突。`notice` 形状与任务 DTO 一致（`NoticeItem[]`，无 notice 时省略）。「需要关注」推导的类别数据源：失败/init 失败/notice 类来自 projects 快照；等待权限/等待问题/idle 类来自 tasks/active 快照。**单侧存在的任务**：仅在 projects 快照存在 → 按 projects 状态归入对应分区（活跃任务可归「其余活跃任务」，可推导的 1/2/5 类照常进「需要关注」）；仅在 tasks/active 快照存在 → 归「其余活跃任务」并可推导 3/4/6 类。MUST NOT 在请求内做合并修复（后续推送或轮询自然收敛）。订阅异常或请求失败时 MUST 保留上次成功数据并展示错误提示，不闪现空态。sessions 首帧到达前的「连接中」与连接错误 MUST 作为独立指示呈现，MUST NOT 升级为整页 loading：若 projects 快照已有数据 MUST 继续按 join 规则渲染 projects-only 任务，MUST NOT 因 sessions 未就绪进入真空态。初次加载与真空态 MUST 区分呈现。所有任务行点击 MUST 跳转对应任务工作台 `#/task/:id`。

过渡态任务（`creating/activating/suspending/deleting`）MUST 归入「其余活跃任务」区并呈现过渡徽章，MUST NOT 进入「需要关注」（失败态除外）。归档任务 MUST 出现在「挂起与归档」区，MUST NOT 出现在侧栏任务组。

分区内排序：「其余活跃任务」MUST 按 `last_active_at` 倒序（projects-only 活跃任务无该字段时回退 `updated_at`），时间相同以任务 ID 升序 tie-break；「挂起与归档」MUST 按 `updated_at` 倒序，tie-break 相同。

#### Scenario: 首页分区呈现

- **WHEN** 用户打开 `#/` 且存在 1 个等待权限确认的活跃任务、2 个普通活跃任务、1 个挂起任务
- **THEN** 等待权限确认的任务出现在「需要关注」，其余 2 个活跃任务出现在「其余活跃任务」，挂起任务出现在「挂起与归档」

#### Scenario: SSE 快照驱动收敛

- **WHEN** 指挥中心已展示数据，随后收到 tasks/active 的 update 帧
- **THEN** 页面按 join 规则以新快照收敛对应分区，不发起对 tasks/active 的轮询请求

#### Scenario: 订阅异常保留旧数据

- **WHEN** tasks/active 的 SSE 订阅断开且重连尚未成功，或 projects 兜底轮询失败
- **THEN** 页面保留上次成功数据并展示错误提示，不闪现空态

#### Scenario: projects 兜底轮询不重叠与失败保留

- **WHEN** 指挥中心共享 projects store 的某次兜底轮询超过轮询周期未返回，随后下一次兜底轮询失败
- **THEN** 后续轮询不与在途请求重叠；页面保留上次成功的 projects 快照并展示错误提示

#### Scenario: 空态引导

- **WHEN** 用户打开 `#/` 且系统内无任何任务
- **THEN** 页面展示空态与新建任务引导，而非报错或空白
