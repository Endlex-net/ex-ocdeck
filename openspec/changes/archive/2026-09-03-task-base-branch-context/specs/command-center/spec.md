## MODIFIED Requirements

### Requirement: 指挥中心内联新建任务

指挥中心 MUST 提供内联新建任务面板：项目选择（可过滤下拉）、任务名、基准分支选择（repo 项目）与"刷新远端分支"；纯目录项目 MUST 展示多任务共享目录警告。**提交门禁**：仅当已选中有效项目 ID 且任务名非空时 MUST 才可发起 POST（按钮禁用）；用户在选择后继续编辑项目输入导致偏离已选项时 MUST 清除已选项目 ID；`base_ref` MUST 仅对 repo 项目提交；提交在途期间 MUST 防重复提交。repo 项目另须分支列表状态为 `ready`（见下）才可提交。创建成功 MUST 跳转新任务工作台；创建失败 MUST 展示错误原因。面板行为 MUST 复用项目管理页相同的创建契约（`POST /api/v1/projects/{id}/tasks`，可选 `base_ref`）。

**基准分支列表状态（repo）**：MUST 维护 `idle | loading | ready | error`，与 `lastSuccessfulBranches` 正交。选中 repo 项目后立即进入 `loading` 并发起初次 `GET /api/v1/projects/{id}/branches`；成功（含返回空数组）进入 `ready` 并写入 `lastSuccessfulBranches`；失败进入 `error` 并保留错误文案，此时无历史数据，列表为空。点击「刷新远端分支」进入 `loading`（refresh 在途，MUST NOT 清空 `lastSuccessfulBranches`）；成功进入 `ready` 并覆盖 `lastSuccessfulBranches`；失败进入 `error`，MUST 保留最近一次 ready 数据作为 stale 列表展示，并标注「本地快照未刷新」及重试入口。`loading` 与 `error` 时 MUST 禁止提交（按钮禁用，Enter 不发起 POST）。仅 `ready` 时按下方规则计算过滤首项并允许提交；stale 列表 MUST NOT 用于 `filteredBranches[0]` 或提交。dir 项目无此状态机。切走 repo 项目时重置为 `idle` 并清空 `lastSuccessfulBranches`。

**基准分支下拉过滤与排序**：统一原语 `normalizedInput = 输入框当前值.trim()`（不改大小写）。非空判定、synthetic 成员判断与 synthetic 候选值均基于 `normalizedInput`。基础候选仅在状态 `ready` 时计算：来自 `lastSuccessfulBranches`（最近一次成功分支列表响应，含初次 `GET /api/v1/projects/{id}/branches` 与 refresh `POST /api/v1/projects/{id}/branches/refresh`；两者成功均覆盖该字段）。成功返回空数组时，若项目 `default_branch` 非空则回退为 `[default_branch]`，否则为空列表。`loading` / `error` 不得把空 `branches` 当作成功空列表去回退 `default_branch`。仅当 `normalizedInput` 非空且不在基础候选中时（大小写敏感，`Array.prototype.includes`），MUST 将 `normalizedInput` 作为 synthetic candidate 前置到候选；synthetic 只参与后续过滤与 D2 排序，不保证成为第一项。过滤 MUST 为对 `q = normalizedInput.toLowerCase()` 的大小写不敏感子串包含（`q` 为空则不过滤）。过滤后 MUST 按以下元组升序稳定排序（值小优先）：
1. 是否「同名命中」：候选的本地名（`origin/` 前缀去掉后的部分，无此前缀则整名）大小写不敏感等于 `q` 则为 0，否则 1；
2. 是否远端：短名大小写不敏感以 `origin/` 开头则为 0，否则 1；
3. 过滤前原顺序下标。

即输入 `master` 且同时存在 `origin/master` 与 `master` 时，`origin/master` MUST 排在第一。下拉展示 MUST 将过滤排序后的第一项标为当前选中（高亮），MUST NOT 按输入框值精确等值高亮；列表为空时无高亮。

**提交时的 `base_ref`**：repo 项目任一提交路径（创建按钮或表单 Enter，含任务名框 Enter）MUST 使用排序后过滤列表的第一项作为 `base_ref` 提交（唯一总规则：`base_ref = filteredBranches[0]`）。synthetic candidate 值为 `normalizedInput`，只参与 D2 排序，不保证第一；仅当它实际排第一时请求才提交 `normalizedInput`。过滤列表为空仅当状态 `ready` 且 branches、`default_branch`、`normalizedInput` 均为空——malformed 项目 DTO 的防御路径：前端 MUST 省略 `base_ref`；服务端沿既有契约返回 `invalid_input`（`resolveRepoBaseRef` 在缺省且 `default_branch` 为空时失败，映射 `invalid_input`）；页面 MUST 展示创建失败。dir 项目 MUST NOT 提交 `base_ref`。

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

#### Scenario: 输入 master 时 origin/master 排在第一

- **WHEN** 分支列表含 `master` 与 `origin/master`，用户在基准分支输入框输入 `master`
- **THEN** 下拉过滤结果第一项为 `origin/master`，第二项为 `master`

#### Scenario: 下拉高亮过滤排序第一项

- **WHEN** 用户在基准分支输入框输入 `sprint-macr-5063`，且列表同时含本地 `sprint-macr-5063` 与远端 `origin/sprint-macr-5063`
- **THEN** 下拉过滤结果第一项为 `origin/sprint-macr-5063`，高亮（选中态）标在该首项上，而非与输入框精确等值的本地项

#### Scenario: 提交使用过滤列表第一项

- **WHEN** 用户已选 repo 项目、任务名非空，过滤排序后第一项为 `origin/main`，输入框值为 `main`，用户点击创建或在表单内按 Enter
- **THEN** `POST /api/v1/projects/{id}/tasks` 的 `base_ref` 为 `origin/main`

#### Scenario: 无同名远端命中时 synthetic 排第一并提交

- **WHEN** 用户已选 repo 项目、任务名非空，基础候选为 `["main","develop"]`，基准分支输入为 `  feature-x  `（`normalizedInput=feature-x` 不在基础候选中）
- **THEN** 过滤排序后第一项为 `feature-x`，`POST /api/v1/projects/{id}/tasks` 的 `base_ref` 为 `feature-x`

#### Scenario: synthetic 不保证第一

- **WHEN** 用户已选 repo 项目、任务名非空，基础候选为 `["origin/main"]`，`normalizedInput=main`（`main` 不在基础候选中，作为 synthetic 前置）
- **THEN** 过滤排序后第一项为 `origin/main`，`POST /api/v1/projects/{id}/tasks` 的 `base_ref` 为 `origin/main`

#### Scenario: 候选与输入皆空时省略 base_ref 并由服务端拒绝

- **WHEN** 用户已选 repo 项目、任务名非空，分支列表状态为 `ready`，成功返回的 branches 为空、项目 `default_branch` 为空、`normalizedInput` 为空并提交
- **THEN** 请求省略 `base_ref` 字段；服务端返回 `invalid_input`；页面展示创建失败

#### Scenario: 初次加载在途禁止提交

- **WHEN** 用户已选 repo 项目、任务名非空，初次 `GET /branches` 仍在途（状态 `loading`）
- **THEN** 提交按钮禁用，点击创建或表单 Enter 均不发起 POST

#### Scenario: 加载完成后提交过滤首项

- **WHEN** 用户已选 repo 项目、任务名非空，初次 `GET /branches` 成功返回含 `origin/main` 与 `main`，状态进入 `ready`，输入框预填 `main`
- **THEN** 允许提交，`base_ref` 为 `origin/main`

#### Scenario: 加载失败禁止提交

- **WHEN** 用户已选 repo 项目、任务名非空，初次 `GET /branches` 失败（状态 `error`）且无历史数据
- **THEN** 列表为空，保留错误文案，提交按钮禁用，不发起 POST

#### Scenario: 刷新失败保留旧列表且禁止提交

- **WHEN** repo 项目分支列表已 `ready`（含 `origin/main` 与 `main`），随后「刷新远端分支」失败进入 `error`
- **THEN** 仍展示最近一次成功列表，标注「本地快照未刷新」及重试入口；提交按钮禁用，stale 列表不得作为 `base_ref` 提交

#### Scenario: 刷新成功后恢复提交

- **WHEN** repo 项目分支列表曾为 `error` 或 `loading`，随后「刷新远端分支」成功进入 `ready`
- **THEN** 提交按钮按既有门禁恢复可点，提交使用刷新后的过滤首项
