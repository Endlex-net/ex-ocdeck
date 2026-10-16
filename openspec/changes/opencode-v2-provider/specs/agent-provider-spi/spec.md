# agent-provider-spi Spec (Delta)

## ADDED Requirements

### Requirement: 结构化 Question 模型

SPI 的 question 模型 SHALL 为 provider-neutral 的结构化字段模型（design D10，用户决策 2026-10-16）：`QuestionPayload` MUST 以 `Fields []QuestionField`（`Key/Label/Prompt/Type/Required/Options/Metadata`）为主视图，旧 `Questions []QuestionItem` 保留为兼容视图；回复 MUST 支持 `Values []QuestionAnswer{Key,Value}` 按字段 Key 关联，旧 `Answers []string` 按字段顺序兼容转换。消费方 MUST 优先消费 `Fields`/`Values`，缺失时回退兼容视图。`EffectiveFields`/`EffectiveAnswers` 的校验 MUST fail-closed：未知 key、重复 key、必填缺失、数量不匹配均拒绝。字段 `Type` 未知值 MUST 保留原值，MUST NOT 静默降级为 text。

#### Scenario: Fields 优先与兼容回退

- **WHEN** 消费方读取 question payload
- **THEN** `Fields` 非空时消费 `Fields`；`Fields` 为空且 `Questions` 非空时经 `EffectiveFields()` 回退为合成 key 字段视图

#### Scenario: key-based 回复与 fail-closed 校验

- **WHEN** 提交 question 回复
- **THEN** `Values` 非空时按字段 Key 关联；`Values` 为空时 `Answers` 按字段顺序转换；未知/重复 key、必填缺失、数量不匹配返回参数错误，不产生任何 wire 副作用

### Requirement: binding 归属校验端口

系统 SHALL 在 application 层提供可选窄接口 `BindingOwnershipChecker`（`CheckBindingOwnership(b, expectedTaskID, expectedDirectory) error`）：provider 解私有 binding 字段并与 task 层传入的独立任务事实比对，在任何凭据派生与网络/进程副作用之前执行。**强制范围限 `binding.ProviderID = opencode-v2`**：task 层使用 v2 binding 前 MUST 调用该校验，v2 provider 未实现该接口 MUST 拒绝使用并 fail-closed；纯 v1 binding 沿用既有校验路径，不调用该端口、不因未实现被拒绝。承接读取旧 v1 binding 时遵循历史事实校验（design D5），生成 v2 候选 binding 后按本端口完成归属校验。仅内存临时 binding（AcquireEphemeral 返回）不适用 domain `TaskBinding.Validate` 的通用四字段必填校验：其 UpstreamSessionID 允许为空、TerminalRef 取保留值，结构校验由 provider 私有校验函数承担；taskId/directory 归属比对仍 MUST 先于凭据派生；该形态 MUST NOT Encode/持久化。

#### Scenario: 归属校验先于凭据派生

- **WHEN** task 层准备使用 ProviderID 为 `opencode-v2` 的 binding 建立 runtime 连接
- **THEN** 先调 `CheckBindingOwnership`（expected 取任务行 ID 与 worktree 解析结果），通过后才允许凭据派生与网络/进程副作用；比对不符返回 `binding_invalid` 且零副作用

#### Scenario: v2 provider 未实现校验端口 fail-closed

- **WHEN** task 层使用 v2 binding 且 v2 provider 未实现 `BindingOwnershipChecker`
- **THEN** 拒绝使用该 binding 并报错，不产生任何副作用

#### Scenario: 纯 v1 binding 不受影响

- **WHEN** task 层使用 ProviderID 为 `opencode-v1` 的 binding
- **THEN** 沿用既有校验路径，不调用 `BindingOwnershipChecker`，不因未实现该接口被拒绝

### Requirement: 跨版本激活边界

系统 SHALL 在任务激活时强制跨版本兼容边界：任务钉扎 providerID 的主版本高于机器实装 opencode 主版本时，激活 MUST fail-closed——拒绝激活、任务状态与既有 binding 保持不变、错误语义映射到既有 ErrorKind 族并给出用户可感知提示。判定口径 MUST 为钉扎 providerID 主版本与机器实装主版本的比较；版本探测失败时 MUST NOT 判定为可承接高版本任务。激活/恢复准入 MUST 同时按既有 binding 的实际执行 provider 校验：已有 v2 binding 的任务，机器实装已知仅有 v1 时 MUST fail-closed（拒绝、零副作用、保留 binding/归属/锚定），该检查优先于钉扎版本比较与纯 v1 执行规则。

#### Scenario: v2 钉扎任务在 v1 机器拒绝激活

- **WHEN** 钉扎 `opencode-v2` 的任务在实装 opencode 主版本为 1 的机器上请求激活（resolver R6）
- **THEN** 激活被拒绝且零副作用：任务状态、binding、归属与锚定不变；错误如实记录并给出用户可感知提示

#### Scenario: v2 钉扎任务在 v2 机器正常激活

- **WHEN** 钉扎 `opencode-v2` 的任务在实装 opencode 主版本 ≥ 2 且承接开关开的机器上请求激活（resolver R5）
- **THEN** 由 v2 provider 正常建立 runtime 与 binding

#### Scenario: 探测失败不放行高版本钉扎

- **WHEN** 钉扎 `opencode-v2` 的任务请求激活，但机器实装版本探测失败
- **THEN** 激活被拒绝（fail-closed），不视为满足版本边界

#### Scenario: 已有 v2 binding 的任务在 v1 机器拒绝激活

- **WHEN** 钉扎 `opencode-v1`、binding ProviderID 为 `opencode-v2` 的任务在实装 v1 或版本探测失败的机器上请求激活或恢复
- **THEN** 激活被拒绝且零副作用：任务状态、binding、归属与锚定不变；不进入纯 v1 执行路径

## MODIFIED Requirements

### Requirement: Provider registry 分派

系统 SHALL 维护 provider registry（provider_id → provider 实现），registry MUST 支持多 provider 并存注册（至少 `opencode-v1` 与 `opencode-v2`）。task 层所有 agent 消费点（激活、attention、permission 回复、agent status、diffreview、恢复、挂起、删除）MUST 经 registry 按任务 provider 标识分派到对应 provider 实现，MUST NOT 直接实例化特定 provider 的客户端。任务 provider 未在 registry 中注册时，该任务的生命周期操作 MUST 报错且零副作用，不得回退到默认 provider 静默执行。

任务的实际执行 provider MUST 按以下规则确定：任务钉扎 providerID 为 `opencode-v2` 时执行 provider 为 v2；任务钉扎 providerID 为 `opencode-v1` 且机器实装 opencode 主版本 ≥ 2 时，执行 provider MUST 为 v2（承接），任务的钉扎 providerID 保持 `opencode-v1` 不变。机器实装版本探测失败时 MUST 按 v1 处理（保守，不承接）。

已建立 binding 的任务消费点 MUST 按 binding 的 ProviderID 路由实际执行 provider：已有 v2 binding 的任务 MUST NOT 因钉扎值仍为 `opencode-v1`、版本探测失败或承接开关关闭而自动转回 v1 执行；实际执行 provider 未在 registry 注册时按未注册语义 fail-closed，保留 binding、归属与锚定不变。共享 runtime 的 provider MUST 将连接凭据作用域限定在 runtime 实例（而非任务），同一 runtime 的全部任务连接使用同一凭据。激活/恢复准入与消费点路由 MUST 遵循 design D4 resolver 决策表（R1–R12）：已有 v2 binding 的规则先于钉扎判定；跨版本拒绝错误种类为 `capability_unavailable`、provider 未注册为 `provider_not_found`（既有 ErrorKind 枚举）；fail-closed 允许的诊断副作用仅为 last_error 记录与用户可感知提示。承接开关关闭的形态 MUST 为 v2 provider 不注册。

#### Scenario: 未注册 provider fail-closed

- **WHEN** 某任务声明的 provider 标识在 registry 中不存在
- **THEN** 对该任务的生命周期操作报错并记录 last_error（诊断性持久化为允许的副作用）；不产生任何进程/网络副作用，不修改归属、锚定、绑定与任务状态，不得回退到默认 provider 静默执行

#### Scenario: 消费点统一分派

- **WHEN** 后台对账（attention）需要构造任务的 agent 客户端
- **THEN** 客户端经 registry 按该任务的执行 provider 分派构造，而非硬编码单一 provider

#### Scenario: v2 机器承接 v1 钉扎任务

- **WHEN** 钉扎 `opencode-v1`、无 v2 binding 的任务在实装 opencode 主版本 ≥ 2 且承接开关开的机器上激活或恢复（resolver R8）
- **THEN** 由 v2 provider 执行该任务：建立 v2 runtime 与 binding（binding 的 provider 标识为 `opencode-v2`），任务行的钉扎 providerID 保持 `opencode-v1` 不变

#### Scenario: v1 机器纯 v1 执行

- **WHEN** 钉扎 `opencode-v1`、无 v2 binding 的任务在实装 opencode 主版本为 1 的机器上激活或恢复（resolver R10）
- **THEN** 由 v1 provider 执行，行为与引入 v2 provider 前完全一致

#### Scenario: 版本探测失败保守不承接

- **WHEN** 钉扎 `opencode-v1`、无 v2 binding 的任务激活，但机器实装 opencode 版本探测失败（resolver R11）
- **THEN** 按 v1 执行，不承接给 v2 provider

#### Scenario: 承接延续原 session

- **WHEN** 钉扎 `opencode-v1` 且已有历史上游 session 的任务在实装 v2 的机器上被承接
- **THEN** v2 provider 沿用官方升级保留的原 upstream session id，验证其可读且可继续使用后延续（仅重建连接、终端与 provider binding）；原 session 不可读或不可继续使用时停止承接、保留任务现状并提交用户决策，不自动新建替代会话

#### Scenario: 无历史会话的新任务承接即首次创建

- **WHEN** 钉扎 `opencode-v1`、从未建立上游会话的新任务在实装 v2 的机器上首次激活
- **THEN** 由 v2 provider 正常创建首个 session，任务钉扎值保持 `opencode-v1` 不变；该路径不得用于替代丢失或不可用的历史 session

#### Scenario: 承接后不自动转回 v1

- **WHEN** 已被 v2 承接的任务（binding ProviderID 为 `opencode-v2`、钉扎值仍为 `opencode-v1`）发生后台对账、挂起恢复或承接开关关闭
- **THEN** 消费点按 binding 的 `opencode-v2` 路由；v2 provider 未注册时 fail-closed 报错，不得自动转回 v1 执行

#### Scenario: 承接提交的原子归属转换

- **WHEN** 承接主流程进入提交阶段
- **THEN** 新 v2 binding 写入、task_sessions 命名空间转换（`opencode-v1`→`opencode-v2`，UpstreamSessionID 不变，含 parent_id 链历史子 session 行）、anchor 三元组更新与任务状态 CAS 在同一事务完成；任一失败整体回滚且旧 v1 归属原样保留；`tasks.provider_id` 不变
