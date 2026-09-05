# Delta: command-center（add-local-path-task-mode）

## MODIFIED Requirements

### Requirement: 指挥中心首页

系统 SHALL 将指挥中心作为应用首页（`#/`），按分区展示：「需要关注」（置顶）、「其余活跃任务」、「挂起与归档」。页头 MUST 提供新建任务入口与最近刷新时间指示。

数据源：指挥中心 MUST 从 App 层共享 projects store（`/api/v1/projects/stream` SSE 订阅 + 常驻低频兜底轮询，与侧栏同一数据源，见 projects-stream spec 与 web-ui-shell spec；MUST NOT 自行重复轮询 `/projects`）获取全状态任务树，并以 `GET /api/v1/tasks/active/stream` 的 SSE 订阅快照（见 active-sessions-stream spec）补充活跃任务的 `last_active_at`、`agentStatus` 与 `attention`。

双快照 join 规则（按 `task_id` 关联）：**字段级来源**——`status/init_status/branch/mode/worktree_path/last_error/notice/updated_at` 及身份字段（`name/project_id/project_name`）以 projects 快照为准；`last_active_at/agentStatus/attention` 以 tasks/active 快照为准（projects 摘要的 `agentStatus` MUST 仅在该任务缺席 tasks/active 快照时使用，即 projects-only 活跃任务）；不存在两端同字段的优先级冲突。`mode`（必有，取值 `worktree`/`local-path`）以 projects 快照为准，tasks/active 快照同样携带 `mode`（必有），两端同源、语义一致；任务行的模式相关操作（删除确认弹窗文案、Git 入口显隐）按行内 task 的 `mode` 判定。`notice` 形状与任务 DTO 一致（`NoticeItem[]`，无 notice 时省略）。「需要关注」推导的类别数据源：失败/init 失败/notice 类来自 projects 快照；等待权限/等待问题/idle 类来自 tasks/active 快照。**单侧存在的任务**：仅在 projects 快照存在 → 按 projects 状态归入对应分区（活跃任务可归「其余活跃任务」，可推导的 1/2/5 类照常进「需要关注」）；仅在 tasks/active 快照存在 → 归「其余活跃任务」并可推导 3/4/6 类。MUST NOT 在请求内做合并修复（后续推送或轮询自然收敛）。订阅异常或请求失败时 MUST 保留上次成功数据并展示错误提示，不闪现空态。sessions 首帧到达前的「连接中」与连接错误 MUST 作为独立指示呈现，MUST NOT 升级为整页 loading：若 projects 快照已有数据 MUST 继续按 join 规则渲染 projects-only 任务，MUST NOT 因 sessions 未就绪进入真空态。初次加载与真空态 MUST 区分呈现。所有任务行点击 MUST 跳转对应任务工作台 `#/task/:id`。

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

### Requirement: 指挥中心内联新建任务

指挥中心 MUST 提供内联新建任务面板：项目选择（可过滤下拉）、任务名、工作空间选择器（双段 segmented control：「worktree / local」，仅 repo 项目选中时渲染，缺省选中「worktree」；dir 项目 MUST NOT 渲染该选择器）、基准分支选择（repo 项目）与"刷新远端分支"；纯目录项目 MUST 展示多任务共享目录警告。**提交门禁**：仅当已选中有效项目 ID 且任务名非空时 MUST 才可发起 POST（按钮禁用）；用户在选择后继续编辑项目输入导致偏离已选项时 MUST 清除已选项目 ID；`base_ref` MUST 仅对 repo 项目的 worktree 模式提交；提交在途期间 MUST 防重复提交。repo 项目 worktree 模式另须分支列表状态为 `ready`（见下）才可提交；local-path 模式下 MUST NOT 等待分支列表 ready（分支列表状态不作为提交门禁）。创建成功 MUST 跳转新任务工作台；创建失败 MUST 展示错误原因。面板行为 MUST 复用项目管理页相同的创建契约（`POST /api/v1/projects/{id}/tasks`，worktree 模式可选 `base_ref`，local-path 模式提交 MUST 携带 `mode=local-path` 且 MUST NOT 携带 `base_ref`）。

**工作空间选择器（repo）**：选中「local」时：基准分支字段（含刷新远端分支按钮）MUST 整体隐藏（非禁用）；提交 MUST NOT 携带 `base_ref`、MUST 携带 `mode=local-path`；选择器下方 MUST 显示低可见度提醒（12px 灰字、无色块无边框），文案 MUST 逐字为：「直接在项目目录里跑，改动就地生效。多任务共享同一目录，并行与否自己把握。」；底部 od-hint 文案 MUST 切换为「创建后直接在当前目录运行并进入工作台，不切分支。」切换模式 MUST NOT 清空已选分支、MUST NOT 重新请求分支列表（切回「worktree」时基准分支字段与分支列表状态恢复原样）。dir 项目的现有色块警告 MUST 原样保留，与 local-path 灰字提醒条件互斥、不得叠加。

**选择器状态重置规则**：面板挂载时选择器为缺省「worktree」；已选项目 ID 发生变更（含切换项目、清除项目选择、从 repo 切到 dir）时，选择器 MUST 重置为「worktree」——防止新选 repo 未经用户再次主动选择即以就地模式提交。仅同一项目内的手动模式切换适用上文联动规则（保留已选分支与分支列表状态）。不改变已选项目的面板信号（如无 payload 的 new / action=keep 初始化信号）MUST 保持选择器现状；若信号导致已选项目 ID 变化，按项目变更重置。repo 项目选中后的初次分支列表请求 MUST 始终发起（与现状一致，与当前模式无关），保证切回 worktree 时分支数据可用。

**基准分支列表状态（repo）**：MUST 维护 `idle | loading | ready | error`，与 `lastSuccessfulBranches` 正交。选中 repo 项目后立即进入 `loading` 并发起初次 `GET /api/v1/projects/{id}/branches`；成功（含返回空数组）进入 `ready` 并写入 `lastSuccessfulBranches`；失败进入 `error` 并保留错误文案，此时无历史数据，列表为空。点击「刷新远端分支」进入 `loading`（refresh 在途，MUST NOT 清空 `lastSuccessfulBranches`）；成功进入 `ready` 并覆盖 `lastSuccessfulBranches`；失败进入 `error`，MUST 保留最近一次 ready 数据作为 stale 列表展示，并标注「本地快照未刷新」及重试入口。`loading` 与 `error` 时 MUST 禁止提交（按钮禁用，Enter 不发起 POST；该门禁仅作用于 worktree 模式）。仅 `ready` 时按下方规则计算过滤首项并允许提交；stale 列表 MUST NOT 用于 `filteredBranches[0]` 或提交。dir 项目无此状态机。切走 repo 项目时重置为 `idle` 并清空 `lastSuccessfulBranches`。

**基准分支下拉过滤与排序**：统一原语 `normalizedInput = 输入框当前值.trim()`（不改大小写）。非空判定、synthetic 成员判断与 synthetic 候选值均基于 `normalizedInput`。基础候选仅在状态 `ready` 时计算：来自 `lastSuccessfulBranches`（最近一次成功分支列表响应，含初次 `GET /api/v1/projects/{id}/branches` 与 refresh `POST /api/v1/projects/{id}/branches/refresh`；两者成功均覆盖该字段）。成功返回空数组时，若项目 `default_branch` 非空则回退为 `[default_branch]`，否则为空列表。`loading` / `error` 不得把空 `branches` 当作成功空列表去回退 `default_branch`。仅当 `normalizedInput` 非空且不在基础候选中时（大小写敏感，`Array.prototype.includes`），MUST 将 `normalizedInput` 作为 synthetic candidate 前置到候选；synthetic 只参与后续过滤与 D2 排序，不保证成为第一项。过滤 MUST 为对 `q = normalizedInput.toLowerCase()` 的大小写不敏感子串包含（`q` 为空则不过滤）。过滤后 MUST 按以下元组升序稳定排序（值小优先）：
1. 是否「同名命中」：候选的本地名（`origin/` 前缀去掉后的部分，无此前缀则整名）大小写不敏感等于 `q` 则为 0，否则 1；
2. 是否远端：短名大小写不敏感以 `origin/` 开头则为 0，否则 1；
3. 过滤前原顺序下标。

即输入 `master` 且同时存在 `origin/master` 与 `master` 时，`origin/master` MUST 排在第一。下拉展示 MUST 将过滤排序后的第一项标为当前选中（高亮），MUST NOT 按输入框值精确等值高亮；列表为空时无高亮。

**提交时的 `base_ref`**：repo 项目 worktree 模式任一提交路径（创建按钮或表单 Enter，含任务名框 Enter）MUST 使用排序后过滤列表的第一项作为 `base_ref` 提交（唯一总规则：`base_ref = filteredBranches[0]`）。synthetic candidate 值为 `normalizedInput`，只参与 D2 排序，不保证第一；仅当它实际排第一时请求才提交 `normalizedInput`。过滤列表为空仅当状态 `ready` 且 branches、`default_branch`、`normalizedInput` 均为空——malformed 项目 DTO 的防御路径：前端 MUST 省略 `base_ref`；服务端沿既有契约返回 `invalid_input`（`resolveRepoBaseRef` 在缺省且 `default_branch` 为空时失败，映射 `invalid_input`）；页面 MUST 展示创建失败。dir 项目 MUST NOT 提交 `base_ref`。

**命令面板初始化信号**：面板 MUST 支持来自命令面板快速新建的初始化信号。允许携带 projectName；候选点击同时携带 projectID 与 projectName；projectID 不单独出现。自由文本 Enter 只传 projectName，候选点击同时传 projectID 与 projectName。消费侧收到仅有 `projectID` 的非法 detail 时 MUST 将其归一为 `{}`（等价无 payload），按普通无 payload `new` 语义处理（只展开聚焦、保持全部表单状态）。快速新建模式 Enter 在余文为空时 MUST 发送 `{ projectName: '' }`，消费侧清空项目选择/过滤词但保留 taskName；无 payload 的普通 `new` 保持全部表单状态（既有语义）。消费优先级固定为：有效 `projectID` 直接选中；ID 已失效则回退文本匹配；文本匹配失败则填过滤词。推断结果不跨层传递，显式用户选择（候选点击）允许携带 `projectID`。`classifyMatch(name, '') = null`（空串不匹配任何项目，禁止 startsWith/includes 空串全命中）。

消费信号时面板 MUST 展开并聚焦任务名输入。无有效 `projectID`（或 `projectID` 已失效后的回退）时，项目预选 MUST 遵循匹配规则。共享原语 `foldForMatch(s)` = ECMAScript `String.prototype.toLowerCase.call(s)` 的结果；MUST NOT 使用 `toLocaleLowerCase`、`Intl.Collator` 或 Unicode normalization。`classifyMatch`、`rankByQuery`、唯一匹配预选全部对 fold 后字符串执行 `===`、`startsWith`、`indexOf`。fold 后精确匹配（`===`）唯一命中 → 预选该项目；无精确命中且匹配模式为 `exact-then-substring` 时子串匹配（`indexOf`）恰好命中一个项目 → 预选该项目；其余情况（零命中或多命中，或匹配模式为 `exact` 且无精确命中）MUST NOT 预选项目，且 MUST 将信号中的项目名文本填入项目过滤输入以便用户手动续选。`matchMode` 仅控制自动预选推断。消费侧只按信号到达时的快照判定预选，MUST NOT 因后续加载自动重试预选。初始化信号 MUST NOT 绕过提交门禁（预选项目不改变"任务名非空才可提交"的约束）。信号跨路由到达时 MUST 有兜底消费机制（目标页未挂载监听时不丢失）。

**已展开面板与连续信号**：父层保存带递增 nonce 的初始化意图，NewTaskPanel 对每个新 nonce 应用项目选择/过滤词并聚焦任务名；快速新建信号仅替换项目相关状态（预选或过滤词），MUST 保留用户已输入的 taskName；无参数 new（无 payload）只展开和聚焦，保持现有全部表单状态不变。

#### Scenario: 内联创建并跳转

- **WHEN** 用户在指挥中心选择项目、输入任务名并提交
- **THEN** 任务创建成功后应用跳转至新任务工作台

#### Scenario: 切换项目时模式选择器重置

- **WHEN** 用户在 repo 项目 A 下选中「local」，随后切换选中项目（含切到 dir 项目或清除选择后重选）
- **THEN** 工作空间选择器 MUST 重置为「worktree」，不继承项目 A 的就地运行选择

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

#### Scenario: 输入 master 时 origin/master 排在第一

- **WHEN** repo 项目 worktree 模式下，分支列表含 `master` 与 `origin/master`，用户在基准分支输入框输入 `master`
- **THEN** 下拉过滤结果第一项为 `origin/master`，第二项为 `master`

#### Scenario: 下拉高亮过滤排序第一项

- **WHEN** repo 项目 worktree 模式下，用户在基准分支输入框输入 `sprint-macr-5063`，且列表同时含本地 `sprint-macr-5063` 与远端 `origin/sprint-macr-5063`
- **THEN** 下拉过滤结果第一项为 `origin/sprint-macr-5063`，高亮（选中态）标在该首项上，而非与输入框精确等值的本地项

#### Scenario: 提交使用过滤列表第一项

- **WHEN** 用户已选 repo 项目且运行模式为 worktree、任务名非空，过滤排序后第一项为 `origin/main`，输入框值为 `main`，用户点击创建或在表单内按 Enter
- **THEN** `POST /api/v1/projects/{id}/tasks` 的 `base_ref` 为 `origin/main`

#### Scenario: 无同名远端命中时 synthetic 排第一并提交

- **WHEN** 用户已选 repo 项目且运行模式为 worktree、任务名非空，基础候选为 `["main","develop"]`，基准分支输入为 `  feature-x  `（`normalizedInput=feature-x` 不在基础候选中）
- **THEN** 过滤排序后第一项为 `feature-x`，`POST /api/v1/projects/{id}/tasks` 的 `base_ref` 为 `feature-x`

#### Scenario: synthetic 不保证第一

- **WHEN** 用户已选 repo 项目且运行模式为 worktree、任务名非空，基础候选为 `["origin/main"]`，`normalizedInput=main`（`main` 不在基础候选中，作为 synthetic 前置）
- **THEN** 过滤排序后第一项为 `origin/main`，`POST /api/v1/projects/{id}/tasks` 的 `base_ref` 为 `origin/main`

#### Scenario: 候选与输入皆空时省略 base_ref 并由服务端拒绝

- **WHEN** 用户已选 repo 项目且运行模式为 worktree、任务名非空，分支列表状态为 `ready`，成功返回的 branches 为空、项目 `default_branch` 为空、`normalizedInput` 为空并提交
- **THEN** 请求省略 `base_ref` 字段；服务端返回 `invalid_input`；页面展示创建失败

#### Scenario: 初次加载在途禁止提交

- **WHEN** 用户已选 repo 项目且运行模式为 worktree、任务名非空，初次 `GET /branches` 仍在途（状态 `loading`）
- **THEN** 提交按钮禁用，点击创建或表单 Enter 均不发起 POST

#### Scenario: 加载完成后提交过滤首项

- **WHEN** 用户已选 repo 项目且运行模式为 worktree、任务名非空，初次 `GET /branches` 成功返回含 `origin/main` 与 `main`，状态进入 `ready`，输入框预填 `main`
- **THEN** 允许提交，`base_ref` 为 `origin/main`

#### Scenario: 加载失败禁止提交

- **WHEN** 用户已选 repo 项目且运行模式为 worktree、任务名非空，初次 `GET /branches` 失败（状态 `error`）且无历史数据
- **THEN** 列表为空，保留错误文案，提交按钮禁用，不发起 POST

#### Scenario: 刷新失败保留旧列表且禁止提交

- **WHEN** repo 项目 worktree 模式分支列表已 `ready`（含 `origin/main` 与 `main`），随后「刷新远端分支」失败进入 `error`
- **THEN** 仍展示最近一次成功列表，标注「本地快照未刷新」及重试入口；提交按钮禁用，stale 列表不得作为 `base_ref` 提交

#### Scenario: 刷新成功后恢复提交

- **WHEN** repo 项目 worktree 模式分支列表曾为 `error` 或 `loading`，随后「刷新远端分支」成功进入 `ready`
- **THEN** 提交按钮按既有门禁恢复可点，提交使用刷新后的过滤首项

#### Scenario: 选择就地运行创建任务

- **WHEN** 用户在 repo 项目新建任务面板选中「local」并提交
- **THEN** `POST /api/v1/projects/{id}/tasks` 请求携带 `mode=local-path` 且不携带 `base_ref`，创建成功后跳转新任务工作台

#### Scenario: local-path 模式下分支列表未 ready 仍可提交

- **WHEN** 用户在 repo 项目选中「local」，分支列表状态为 `loading`、`error` 或尚未发起，任务名非空并提交
- **THEN** 提交不被分支列表状态阻断，请求正常发起

#### Scenario: dir 项目不渲染模式选择器

- **WHEN** 用户在内联面板中选中 kind=dir 的项目
- **THEN** 面板不渲染工作空间选择器，现有「纯目录项目无文件隔离」色块警告原样保留，不出现 local-path 灰字提醒

#### Scenario: 模式切换保留已选分支

- **WHEN** 用户在 repo 项目下已选基准分支，随后在「worktree」与「local」间切换运行模式
- **THEN** 已选分支不被清空、不重新发起 `GET /api/v1/projects/{id}/branches`；切回「worktree」时基准分支字段恢复展示且分支列表状态保持原样
