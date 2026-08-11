# Command Center Specification

## Purpose

指挥中心是应用首页（`#/`），以"任务优先"回答用户最关心的问题——哪个 agent 需要我。它聚合「需要关注」注意力项（失败态/init 失败/等待权限/等待问题/notice/agent idle）、其余活跃任务与挂起归档任务，并提供内联新建任务入口。

## Requirements

### Requirement: 指挥中心首页

系统 SHALL 将指挥中心作为应用首页（`#/`），按分区展示：「需要关注」（置顶）、「其余活跃任务」、「挂起与归档」。页头 MUST 提供新建任务入口与最近刷新时间指示。

数据源与轮询：指挥中心 MUST 从 App 层共享 projects 轮询 store（5s single-flight，与侧栏同一数据源，MUST NOT 自行重复轮询 `/projects`）获取全状态任务树，并以 `GET /sessions/active`（5s single-flight）补充活跃任务的 `last_active_at`、`agentStatus` 与 `attention`。

双快照 join 规则（按 `task_id` 关联）：**字段级来源**——`status/init_status/branch/worktree_path/last_error/notice/updated_at` 及身份字段（`name/project_id/project_name`）以 projects 快照为准；`last_active_at/agentStatus/attention` 以 sessions/active 快照为准（projects 摘要的 `agentStatus` MUST 仅在该任务缺席 sessions/active 快照时使用，即 projects-only 活跃任务）；不存在两端同字段的优先级冲突。`notice` 形状与任务 DTO 一致（`NoticeItem[]`，无 notice 时省略）。「需要关注」推导的类别数据源：失败/init 失败/notice 类来自 projects 快照；等待权限/等待问题/idle 类来自 sessions/active 快照。**单侧存在的任务**：仅在 projects 快照存在 → 按 projects 状态归入对应分区（活跃任务可归「其余活跃任务」，可推导的 1/2/5 类照常进「需要关注」）；仅在 sessions/active 快照存在 → 归「其余活跃任务」并可推导 3/4/6 类。MUST NOT 在请求内做合并修复（下一轮轮询自然收敛）。请求失败时 MUST 保留上次成功数据并展示错误提示，不闪现空态；初次加载与真空态 MUST 区分呈现。所有任务行点击 MUST 跳转对应任务工作台 `#/task/:id`。

过渡态任务（`creating/activating/suspending/deleting`）MUST 归入「其余活跃任务」区并呈现过渡徽章，MUST NOT 进入「需要关注」（失败态除外）。归档任务 MUST 出现在「挂起与归档」区，MUST NOT 出现在侧栏任务组。

分区内排序：「其余活跃任务」MUST 按 `last_active_at` 倒序（projects-only 活跃任务无该字段时回退 `updated_at`），时间相同以任务 ID 升序 tie-break；「挂起与归档」MUST 按 `updated_at` 倒序，tie-break 相同。

#### Scenario: 首页分区呈现

- **WHEN** 用户打开 `#/` 且存在 1 个等待权限确认的活跃任务、2 个普通活跃任务、1 个挂起任务
- **THEN** 等待权限确认的任务出现在「需要关注」，其余 2 个活跃任务出现在「其余活跃任务」，挂起任务出现在「挂起与归档」

#### Scenario: 轮询不重叠与失败保留

- **WHEN** 某次轮询超过 5 秒未返回，随后下一次轮询失败
- **THEN** 在途期间不发起重叠请求；失败后页面保留上次成功数据并展示错误提示

#### Scenario: 空态引导

- **WHEN** 用户打开 `#/` 且系统内无任何任务
- **THEN** 页面展示空态与新建任务引导，而非报错或空白

### Requirement: 需要关注注意力聚合

「需要关注」分区 MUST 按以下优先级降序聚合注意力项：

1. 失败态任务（`creation_failed` / `deletion_failed`）
2. init 失败任务（`suspended` 且 `init_status=failed`）
3. 等待权限确认（任务存在 pending 权限请求）
4. 等待回答问题（任务存在 pending 问题请求）
5. 携带 notice 的任务（如残留进程待清理）
6. agent 状态为 idle 的活跃任务

**单任务去重**：一个任务在「需要关注」中 MUST 只出现一行，取最高优先级命中类别作为主呈现；其余命中类别 MUST 以次要标记同行展示。**排序**：同类内按任务最近时间倒序（活跃任务用 `last_active_at`，非活跃任务用 `updated_at`），时间相同以任务 ID 升序 tie-break（保证多次渲染间顺序稳定）。

**行内操作集**：`creation_failed` MUST 提供重试与普通删除，并行内展示 `last_error`（MUST NOT 提供强制删除选项，无独立日志端点）；`deletion_failed` MUST 提供重试与强制删除，且 MUST 仅当 `last_error` 以 `pre-delete:` 前缀时提供 pre-delete 日志查看；init 失败 MUST 提供查看日志与重跑初始化。等待权限确认与等待回答问题的注意力项点击 MUST 跳转对应任务工作台（审批/回答在 TUI 内完成）；本页面 MUST NOT 提供权限回复或问题回答操作。注意力能力为 `unsupported` 时 MUST 仅缺少第 3/4 类条目，其余类 MUST 正常呈现，页面 MUST NOT 报错；能力为 `degraded` 时第 3/4 类 MUST 照常呈现（数据可能滞后，不标注不报错）。

#### Scenario: 失败态置顶

- **WHEN** 存在 1 个 deletion_failed 任务（last_error 以 `pre-delete:` 前缀）与 1 个 agent idle 的活跃任务
- **THEN** deletion_failed 任务排在「需要关注」内 idle 任务之前，且行内展示重试/强制删除/pre-delete 日志操作

#### Scenario: 单任务多原因去重

- **WHEN** 某任务同时为 init 失败且携带 notice
- **THEN** 该任务在「需要关注」只出现一行，以 init 失败为主呈现（含其操作集），notice 以次要标记同行展示

#### Scenario: creation_failed 操作集

- **WHEN** 存在 creation_failed 任务
- **THEN** 行内提供重试与普通删除操作并展示 last_error，不出现强制删除选项

#### Scenario: 权限等待跳转

- **WHEN** 某活跃任务存在 pending 权限请求
- **THEN** 该任务以「等待权限确认」出现在「需要关注」，点击进入该任务工作台

#### Scenario: 信号降级不影响其余类别

- **WHEN** 后端注意力能力为 `unsupported`（opencode 不支持 permission/question 端点）
- **THEN** 「需要关注」仍正常呈现失败态/init 失败/notice/idle 条目，仅缺少第 3/4 类，不出现错误提示

#### Scenario: degraded 照常呈现

- **WHEN** 后端注意力能力为 `degraded`（对账瞬时失败，保留旧快照）
- **THEN** 第 3/4 类按旧快照照常呈现，页面不标注不报错

#### Scenario: 过渡态不进需要关注

- **WHEN** 某任务处于 activating 过渡态
- **THEN** 该任务出现在「其余活跃任务」区并呈现过渡徽章，不出现在「需要关注」

#### Scenario: 其余活跃任务排序

- **WHEN** 「其余活跃任务」区有 3 个任务，last_active_at 分别为 100/300/200
- **THEN** 按 last_active_at 倒序呈现（300 → 200 → 100）

#### Scenario: projects-only 活跃任务排序回退

- **WHEN** 某活跃任务仅存在于 projects 快照（无 last_active_at）
- **THEN** 其排序时间取 `updated_at`，与其余任务按同一规则倒序

### Requirement: 挂起与归档区操作

「挂起与归档」分区的任务行 MUST 提供与状态机一致的操作（复用现有 API 与组件行为，无新增端点）：挂起任务提供激活、归档、删除；归档任务提供恢复、删除；操作后的确认/脏确认/强制删除语义与项目管理页一致。

#### Scenario: 挂起任务激活

- **WHEN** 用户在指挥中心点击某挂起任务的激活
- **THEN** 调用既有激活 API，任务进入激活过渡态并在下一轮轮询收敛

#### Scenario: 归档任务恢复

- **WHEN** 用户点击某归档任务的恢复
- **THEN** 调用既有恢复 API，任务回到挂起态

### Requirement: 指挥中心内联新建任务

指挥中心 MUST 提供内联新建任务面板：项目选择（可过滤下拉）、任务名、基准分支选择（repo 项目）与"刷新远端分支"；纯目录项目 MUST 展示多任务共享目录警告。**提交门禁**：仅当已选中有效项目 ID 且任务名非空时 MUST 才可发起 POST（按钮禁用）；用户在选择后继续编辑项目输入导致偏离已选项时 MUST 清除已选项目 ID；`base_ref` MUST 仅对 repo 项目提交；提交在途期间 MUST 防重复提交。创建成功 MUST 跳转新任务工作台；创建失败 MUST 展示错误原因。面板行为 MUST 复用项目管理页相同的创建契约（`POST /api/v1/projects/{id}/tasks`，可选 `base_ref`）。

#### Scenario: 内联创建并跳转

- **WHEN** 用户在指挥中心选择项目、输入任务名并提交
- **THEN** 任务创建成功后应用跳转至新任务工作台

#### Scenario: 纯目录项目警告

- **WHEN** 用户在内联面板中选择 kind=dir 的项目
- **THEN** 面板展示该目录多任务并行无文件隔离的警告文案
