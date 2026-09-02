# Command Center Specification

## Purpose

指挥中心是应用首页（`#/`），以"任务优先"回答用户最关心的问题——哪个 agent 需要我。它聚合「需要关注」注意力项（失败态/init 失败/等待权限/等待问题/notice/agent idle）、其余活跃任务与挂起归档任务，并提供内联新建任务入口。

## Requirements

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

**命令面板初始化信号**：面板 MUST 支持来自命令面板快速新建的初始化信号。允许携带 projectName；候选点击同时携带 projectID 与 projectName；projectID 不单独出现。自由文本 Enter 只传 `projectName`，候选点击同时传 `projectID` 与 `projectName`。消费侧收到仅有 `projectID` 的非法 detail 时 MUST 将其归一为 `{}`（等价无 payload），按普通无 payload `new` 语义处理（只展开聚焦、保持全部表单状态）。快速新建模式 Enter 在余文为空时 MUST 发送 `{ projectName: '' }`，消费侧清空项目选择/过滤词但保留 taskName；无 payload 的普通 `new` 保持全部表单状态（既有语义）。消费优先级固定为：有效 `projectID` 直接选中；ID 已失效则回退文本匹配；文本匹配失败则填过滤词。推断结果不跨层传递，显式用户选择（候选点击）允许携带 `projectID`。`classifyMatch(name, '') = null`（空串不匹配任何项目，禁止 startsWith/includes 空串全命中）。

消费信号时面板 MUST 展开并聚焦任务名输入。无有效 `projectID`（或 `projectID` 已失效后的回退）时，项目预选 MUST 遵循匹配规则。共享原语 `foldForMatch(s)` = ECMAScript `String.prototype.toLowerCase.call(s)` 的结果；MUST NOT 使用 `toLocaleLowerCase`、`Intl.Collator` 或 Unicode normalization。`classifyMatch`、`rankByQuery`、唯一匹配预选全部对 fold 后字符串执行 `===`、`startsWith`、`indexOf`。fold 后精确匹配（`===`）唯一命中 → 预选该项目；无精确命中且匹配模式为 `exact-then-substring` 时子串匹配（`indexOf`）恰好命中一个项目 → 预选该项目；其余情况（零命中或多命中，或匹配模式为 `exact` 且无精确命中）MUST NOT 预选项目，且 MUST 将信号中的项目名文本填入项目过滤输入以便用户手动续选。`matchMode` 仅控制自动预选推断。消费侧只按信号到达时的快照判定预选，MUST NOT 因后续加载自动重试预选。初始化信号 MUST NOT 绕过提交门禁（预选项目不改变"任务名非空才可提交"的约束）。信号跨路由到达时 MUST 有兜底消费机制（目标页未挂载监听时不丢失）。

**已展开面板与连续信号**：父层保存带递增 nonce 的初始化意图，NewTaskPanel 对每个新 nonce 应用项目选择/过滤词并聚焦任务名；快速新建信号仅替换项目相关状态（预选或过滤词），MUST 保留用户已输入的 taskName；无参数 new（无 payload）只展开和聚焦，保持现有全部表单状态不变。

#### Scenario: 内联创建并跳转

- **WHEN** 用户在指挥中心选择项目、输入任务名并提交
- **THEN** 任务创建成功后应用跳转至新任务工作台

#### Scenario: 纯目录项目警告

- **WHEN** 用户在内联面板中选择 kind=dir 的项目
- **THEN** 面板展示该目录多任务并行无文件隔离的警告文案

#### Scenario: 快速新建唯一匹配预选项目

- **WHEN** 命令面板快速新建信号携带的项目名精确匹配（或唯一子串匹配，`exact-then-substring` 模式下）某项目
- **THEN** 面板展开、预选该项目、任务名输入获得焦点

#### Scenario: 快速新建零命中或多命中不预选

- **WHEN** 命令面板快速新建信号携带的项目名零命中或多命中
- **THEN** 面板展开、不预选项目、项目过滤输入填入该项目名文本、任务名输入获得焦点，用户可手动续选项目

#### Scenario: 候选点击携带 projectID 直接选中

- **WHEN** 用户点击命令面板项目候选，信号同时携带该候选的 `projectID` 与 `projectName`（projectID 不单独出现），且该 `projectID` 在当前项目列表中有效
- **THEN** 面板展开、直接选中该 `projectID` 对应项目（不走文本匹配推断）、任务名输入获得焦点

#### Scenario: 失效 projectID 回退文本匹配

- **WHEN** 信号携带的 `projectID` 在当前项目列表中已失效，且 `projectName` 按匹配规则唯一命中另一项目
- **THEN** 面板展开、按文本匹配预选命中项目、任务名输入获得焦点

#### Scenario: 失效 projectID 且文本匹配失败则填过滤词

- **WHEN** 信号携带的 `projectID` 已失效，且 `projectName` 文本匹配失败（零命中或多命中，或 `exact` 模式下无精确命中）
- **THEN** 面板展开、不预选项目、项目过滤输入填入该 `projectName` 文本、任务名输入获得焦点

#### Scenario: 自由文本 Enter 只传 projectName

- **WHEN** 用户在快速新建模式对置顶「新建任务」按 Enter（自由文本，未点候选）
- **THEN** 信号只传 `projectName`、不传 `projectID`，消费侧按文本匹配规则预选或填过滤词

#### Scenario: 已展开面板连续信号按新 nonce 应用项目状态并保留 taskName

- **WHEN** 新建任务面板已展开且用户已输入 taskName，随后到达新的快速新建信号（父层递增 nonce）
- **THEN** NewTaskPanel 对该新 nonce 应用项目选择或过滤词并聚焦任务名；快速新建信号仅替换项目相关状态（预选或过滤词），MUST 保留用户已输入的 taskName

#### Scenario: 空余文 Enter 清空项目状态并保留 taskName

- **WHEN** 快速新建模式 Enter 在余文为空时发送 `{ projectName: '' }`
- **THEN** 消费侧清空项目选择/过滤词但保留 taskName

#### Scenario: 无参数 new 只展开和聚焦

- **WHEN** 到达无 payload 的普通 `new` 信号（无 `projectName`、无 `projectID`）
- **THEN** 面板只展开和聚焦，无 payload 的普通 `new` 保持全部表单状态（既有语义）

#### Scenario: 快速新建不绕过提交门禁

- **WHEN** 快速新建信号已预选项目但任务名为空
- **THEN** 提交按钮保持禁用，不发起创建请求
