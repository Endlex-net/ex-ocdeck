# command-center Specification (Delta)

## MODIFIED Requirements

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
