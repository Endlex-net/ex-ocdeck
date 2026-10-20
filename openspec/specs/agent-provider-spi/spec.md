# Agent Provider SPI Specification

## Purpose

为 ocdeck 托管的各类 agent（opencode v1/v2、codex、pi 等）定义统一的 provider 接入契约：provider 身份模型、registry 分派、运行时托管与任务绑定生命周期、会话归属命名空间、typed events 与能力接口集合。opencode v1 作为首个 provider 兑现该契约，对外行为不变。

## Requirements

### Requirement: Provider 身份模型

系统 SHALL 为每个可接入的 agent 分配稳定的 provider 标识（`provider_id`，如 `opencode-v1`），并区分四个身份概念：provider 标识（产品族+协议代）、契约版本区间、二进制版本、任务激活时的绑定（TaskBinding）。任务 MUST 持久化其 provider 标识；存量任务 MUST 在迁移时回填为 `opencode-v1`。活跃任务 MUST 按激活时的绑定路由，禁止按任务实时状态重新推导 provider；任务仅允许在非活跃（suspended）状态下切换 provider。切换采用**保留业务关联的重建语义**：系统 MUST 保留任务及其既有归属行（作为旧命名空间下的历史记录），释放旧 provider runtime 与其 TaskBinding，清空旧锚定（锚定属于旧 provider 命名空间，对目标 provider 无意义）；任务切换后保持 suspended，下次激活时由目标 provider 重新建立 runtime、绑定与新锚定。切换动作本身 MUST 原子：目标 provider 未注册或旧 runtime 释放失败时切换失败，任务 provider 标识保持原值，归属与锚定不变。

#### Scenario: 存量任务回填默认 provider

- **WHEN** 系统执行 provider 持久化迁移
- **THEN** 所有未声明 provider 的存量任务被回填为 `opencode-v1`，其行为与迁移前完全一致

#### Scenario: 活跃任务禁止切换 provider

- **WHEN** 对处于活跃状态的任务请求切换 provider
- **THEN** 系统拒绝该请求且零副作用；任务先挂起后才允许切换

#### Scenario: 切换保留业务关联并重建

- **WHEN** 非活跃任务切换 provider
- **THEN** 既有归属行保留为旧命名空间历史记录，旧 runtime 与其绑定被释放，旧锚定清空；任务保持 suspended，下次激活由目标 provider 重新建立 runtime、绑定与锚定

#### Scenario: 切换失败保持原状

- **WHEN** 非活跃任务切换 provider，但目标 provider 未注册或旧 runtime 释放失败
- **THEN** 切换失败，任务 provider 标识、归属、锚定与绑定均保持原值

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

### Requirement: 运行时托管与任务绑定生命周期

系统 SHALL 经 provider 的运行时托管契约（RuntimeSupervisor）管理 agent 运行时：任务激活时获取（Acquire）运行时可用的绑定、重启或恢复时重建（Resume）绑定、任务删除或挂起时释放（Release）绑定。绑定（TaskBinding）MUST 持久化，内容至少包含：provider 标识、provider 实例标识（稳定的后端运行池/数据域标识，不得为易变的进程号）、上游 session 标识、终端引用（如有）、以及 provider 私有的连接数据（版本化）。连接凭据分两类：秘密凭据（如认证密码）MUST 经确定性派生获得（由主密钥与任务标识派生），MUST NOT 持久化于绑定或任何存储；非秘密连接数据（端点、目录等）MUST 以持久化绑定为事实来源。进程环境变量仅作为向 agent 进程注入凭据的通道，MUST NOT 作为 ocdeck 读取凭据的来源。**两个例外**：① 迁移导入桥接（见「迁移导入桥接」）对存量 runtime 的历史随机密码允许一次性读取并存入绑定（标记为 legacy，该任务的下次激活/重建起切换为派生凭据）；② 删除清理例外：删除任务且无可复用 runtime（无持久化绑定，或 binding 陈旧——对应 runtime 已终止或凭据失效；如存量 suspended 任务仍有归属会话）时，系统 SHALL 经一次性临时绑定（仅存内存、MUST NOT 持久化）连接其上游以执行归属清理；临时绑定 MUST NOT 用于锚定 bootstrap、session 创建、SSE 订阅或任务状态提交；临时 runtime 的清理失败 MUST 阻断任务删除并保留可重试的清理记录。

#### Scenario: 激活产生持久化绑定

- **WHEN** 任务激活成功
- **THEN** 系统持久化该任务的 TaskBinding，后续对任务 runtime 的全部访问按绑定路由

#### Scenario: 重启后按绑定恢复

- **WHEN** ocdeck-server 重启后恢复活跃任务
- **THEN** 系统按持久化 TaskBinding 重建与任务 runtime 的连接，而非从进程环境变量反查凭据

#### Scenario: 绑定缺失按恢复失败处理

- **WHEN** 恢复路径发现任务缺少有效 TaskBinding 或绑定数据无法解析
- **THEN** 本次恢复判定失败并记录 last_error（计入重拉预算），不得回退到环境变量反查

#### Scenario: 无绑定任务的删除清理

- **WHEN** 删除一个无可复用 runtime 但有归属会话的任务（无持久化绑定或 binding 陈旧，如存量 suspended 任务）
- **THEN** 系统以一次性内存临时绑定连接上游，逐项删除本任务拥有的会话后终止临时 runtime；临时绑定不持久化、不建锚定、不建 SSE、不提交任务状态

#### Scenario: 临时清理失败阻断删除

- **WHEN** 删除清理中临时 runtime 的终止非净或清理记录落账失败
- **THEN** 任务 DB 删除被阻断，清理记录保留可重试；不得越过删除提交点导致清理票据随级联删除丢失

#### Scenario: 终端引用失效重建绑定

- **WHEN** 任务存在 TaskBinding 但其中的终端引用已失效（对应 tmux 会话不存在）
- **THEN** Resume 将该 runtime 视为已死亡，进入既有重拉/重建流程；新 runtime 建立成功后新绑定原子替换旧绑定；新 runtime 建立失败时旧绑定保留、本次恢复判定失败并计入恢复预算；除迁移导入桥接外，不得从旧环境变量静默恢复

### Requirement: 迁移导入桥接

系统 SHALL 在每次启动时执行迁移导入 runner，保证持久化 active 任务的 TaskBinding 可信。runner 对每个 active 任务按「验证或导入」处理：binding 存在且可解析时 MUST 以其凭据实际验证连通（健康检查通过为准），通过则保留；binding 缺失、无法解析或验证失败时，MUST 读取旧 runtime 的连接信息（进程环境变量与持久化目录）并验证连通，通过后以验证结果写入或替换 binding。旧来源读取是 runner 的迁移桥接特许，其适用条件由以下**唯一优先级分支规则**定义（「运行时托管与任务绑定生命周期」的凭据读取禁令适用于正常生命周期路径，与 runner 互不冲突）：

**主密钥准入**：runner MUST 在任何网络验证与桥接前确认主密钥可用（能完成派生计算）；主密钥不可用则本轮 runner 整体放弃，server MUST 以启动错误拒绝进入正常生命周期与服务就绪（候选屏障不释放、不耗恢复预算、不改任务状态、不读 env）。主密钥的 master secret 文件缺失时由首启自动生成（D3 修订），故「不可用」仅指显式空 env / 空文件等配置错误。

**凭据来源判定（唯一判据为非秘密来源标记，禁止以密码值比对代替）**：runtime 由本系统创建时 MUST 经会话 env 注入非秘密来源标记（`OCDECK_CRED_KIND=derived`）。runner 对验证失败的候选读取该标记：标记为 `derived` → 该 runtime 为派生凭据，MUST NOT 读取/导入/落库其密码，记 last_error 标注疑似主密钥或凭据问题，不修改 binding；标记缺失 → 确认为旧二进制产生的随机凭据 runtime（回滚窗口重拉），允许读取端口与密码并经健康检查验证后 legacy 导入。防御性不变量：任何路径 MUST NOT 把等于当前派生值的密码写入存储（比对仅作防御断言，不作来源判据）。

runner MUST 幂等：完成判定为本次启动快照的待处理集合中每个任务均出结论（保留/替换/失败），不设完成标记位，中断后重启即续跑。runner 对某任务出结论前，正常生命周期消费者 MUST NOT 处理该任务（不判恢复失败、不耗恢复预算）。单任务导入失败（旧 runtime 消失、连接失败、数据解析失败）时 MUST 记录 last_error 并继续其余任务，由既有恢复路径接管，MUST NOT 写入伪造绑定；导入失败的副作用边界为：MUST NOT 创建新进程、MUST NOT 删除旧 session、MUST NOT 修改锚定、MUST NOT 清理归属行。

#### Scenario: 已有可信绑定直接保留

- **WHEN** runner 处理 active 任务，其 binding 可解析且健康检查通过
- **THEN** 保留该 binding，不读取旧来源，任务正常进入生命周期

#### Scenario: 导入成功建立或替换绑定

- **WHEN** 任务 binding 缺失/无法解析/验证失败，旧 runtime 存活且健康检查通过
- **THEN** 以验证结果在单事务内写入或替换完整 TaskBinding，任务状态不变；回滚窗口被旧二进制变更的过时 binding 由此被识别并替换

#### Scenario: 导入失败不产生副作用

- **WHEN** 导入时旧 runtime 已消失或健康检查失败
- **THEN** 记录 last_error 并继续其余任务；不写入绑定、不创建进程、不删除 session、不修改锚定与归属

#### Scenario: 启动屏障隔离候选任务

- **WHEN** runner 尚未对某 active 任务出结论
- **THEN** 正常生命周期消费者（恢复、重拉、对账）跳过该任务，不判恢复失败、不消耗恢复预算

### Requirement: SessionKey 会话归属命名空间

系统 SHALL 以 SessionKey（provider 标识 + provider 实例标识 + 上游 session 标识）作为会话归属的唯一键。归属唯一性约束（一个会话至多归属一个任务）MUST 作用于完整 SessionKey；不同 provider 或不同 provider 实例产生的相同上游 session 标识 MUST NOT 相互冲突。父子会话的 SessionKey MUST 处于同一命名空间。存量归属数据 MUST 确定性回填到 `opencode-v1` 命名空间。

#### Scenario: 跨 provider 同 ID 不冲突

- **WHEN** 两个不同 provider（或同一 provider 的不同实例）产生相同的上游 session 标识
- **THEN** 二者在归属判定中被视为不同会话，各自可独立归属不同任务

#### Scenario: 存量归属回填

- **WHEN** 系统执行 SessionKey 持久化迁移
- **THEN** 既有 `task_sessions` 归属行与锚定 session 全部回填为 `opencode-v1` 命名空间，迁移前后归属判定结果一致

### Requirement: typed events 契约

系统 SHALL 以带类型事件作为 task 层消费 agent 运行信息的唯一事件形态，事件类型集合为：`SessionCreated`、`SessionUpdated`、`SessionDeleted`、`StatusChanged`、`AttentionRaised`、`AttentionResolved`、`SessionError`。provider 适配层 MUST 将上游协议事件（SSE payload、终端输出模式等）在适配层内部翻译为带类型事件；task 层 MUST NOT 解析 provider 原始事件格式。事件流语义为 volatile：断流期间的事件允许丢失，消费者 MUST 容忍断流缺口，并在重连后主动拉取状态对齐。

#### Scenario: 原始事件不出适配层

- **WHEN** provider 上游协议新增或变更事件字段
- **THEN** 影响面限制在该 provider 的适配层内，task 层消费的事件类型集合与语义不变

#### Scenario: 断流后主动对齐

- **WHEN** 事件流断开并重新建立
- **THEN** 消费者不假设断流期间事件可重放，主动拉取当前状态完成对齐（与既有 SSE 重连对齐语义一致）

### Requirement: SPI 能力接口与健康状态透出

系统 SHALL 将 SPI 能力组织为按消费者划分的接口集合：SessionControl（会话枚举/创建/删除/层级）、EventSource（事件订阅）、AttentionIO（permission/question 读取与回复）、AsyncPrompt（异步 prompt 注入）、StatusReader（运行状态读取）、TerminalIO（终端输入/输出/attach）、TitleUpdater（标题同步）。每项能力对每个 provider 均为契约必需；适配层自选兑现通道（API 或 TUI），通道选择对 SPI 消费者透明。能力运行时健康状态 MUST 按（能力原语、状态、作用域、原因、代际）透出，状态取值至少含 `unknown` / `available` / `unsupported` / `degraded`；`unsupported`/`degraded` 表示适配层故障或上游变更导致的违约，MUST 可被发现并告警，不得静默降级。透出通道为日志与服务状态端点的 `capabilities` 只读字段；该字段为纯增量，「v1 迁移行为保持」的等价比较仅覆盖既有字段，`capabilities` 单独按本节契约验证。

#### Scenario: 能力故障透出

- **WHEN** 某 provider 的某项能力因上游变更失效
- **THEN** 该能力的健康状态转为 `unsupported` 或 `degraded` 并带原因透出，而非表现为任务静默无响应

#### Scenario: 通道选择对消费者透明

- **WHEN** 某 provider 的 AsyncPrompt 能力经 TUI 通道而非 API 通道兑现
- **THEN** task 层调用方式与语义不变，无需感知兑现通道差异

### Requirement: v1 迁移行为保持

opencode v1 作为首个 provider 迁入 SPI 后，系统对外的全部既有行为 MUST 保持不变：进程托管形态、端口分配与健康检查、SSE 归属捕获语义、锚定 bootstrap 协议、attention/permission/status/diffreview 全链路行为、恢复与挂起语义。本 change 的全部既有自动化测试 MUST 在重构后保持通过，且新增的 characterization tests MUST 在重构前后均通过。

#### Scenario: 保护网前后一致

- **WHEN** characterization tests 在重构前建立并运行
- **THEN** 同一套测试在 v1 迁入 SPI 后不做行为断言修改即可全部通过

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

### Requirement: 承接开关默认值与自动承接通知

承接开关 `OCDECK_V2_ADOPTION` 未配置时 SHALL 默认为开启（v2 provider 在装配期注册）；显式配置为 `0`/`false`/`off`（大小写不敏感、容忍首尾空白）时关闭，其他未知显式值 MUST 保守视为关闭，关闭形态 MUST 仍为 v2 provider 不注册。钉扎 `opencode-v1`、无 v2 binding 的任务在实装 opencode 主版本 ≥ 2 的机器上激活或恢复时，承接 MUST 自动发生（resolver R8，无需用户逐任务确认）；承接成功时 MUST 经 last_error 渠道写入用户可感知的迁移承接通知，如实说明 v1 pending attention 不保留（MUST NOT 把空 pending 解释为「原 pending 已解决」）。

#### Scenario: 未配置开关时默认承接

- **WHEN** 机器实装 opencode 主版本 ≥ 2、未配置 `OCDECK_V2_ADOPTION`，钉扎 `opencode-v1` 且无 v2 binding 的任务请求激活或恢复
- **THEN** resolver 命中 R8（exec=opencode-v2、Adopt=true），承接流程自动执行，不因开关未配置而 fail-closed

#### Scenario: 显式关闭保留 fail-closed

- **WHEN** `OCDECK_V2_ADOPTION` 显式配置为 `0`/`false`/`off`，钉扎 `opencode-v1` 的任务在 v2 机器上请求激活或恢复
- **THEN** resolver 命中 R9 fail-closed（capability_unavailable），保留任务现状，提示承接开关处于关闭状态及恢复方式

#### Scenario: 未知显式值保守按关闭处理

- **WHEN** `OCDECK_V2_ADOPTION` 配置为 `0`/`false`/`off` 之外的未知值（如 `yes`、空串、`2`），钉扎 `opencode-v1` 的任务在 v2 机器上请求激活或恢复
- **THEN** 按关闭处理（v2 provider 不注册），resolver 命中 R9 fail-closed 语义适用

#### Scenario: 自动承接成功通知

- **WHEN** v1 钉扎任务经 R8 自动承接且历史判定矩阵命中 A 分支（已有历史 session）提交成功
- **THEN** 任务 last_error 写入迁移承接通知，说明该任务已随 opencode v2 升级自动承接、原 v1 pending attention 为进程内存态不保留；B 分支（从未建立上游会话的首建）不产生迁移通知——无历史可丢失

### Requirement: 无 binding 老任务的 anchor 承接（legacy-A）

钉扎 `opencode-v1`、`task_binding` 为空、anchor 三元组为 v1 命名空间（`anchor_provider_id=opencode-v1`、`anchor_provider_instance_id=default`）且 `anchor_session_id` 存在于该任务 v1 归属集合的任务，承接准入 SHALL 判为 A 分支（承接目标 = `anchor_session_id`），承接流程以合成的内存态 v1 binding（不持久化）完成原 session 验证。承接事务的旧 binding 代际守卫 MUST 相应复验 `task_binding` 仍为空（NULL）：提交时若 `task_binding` 已被并发写入非空值 MUST 拒绝（ErrAdoptionConflict），零写入。anchor 不在归属集合、anchor 命名空间非 v1、或归属行含非 v1 命名空间时 MUST 维持停止承接（stop），不得猜测。

#### Scenario: 无 binding 老任务承接成功

- **WHEN** 钉扎 v1、`task_binding` 为 NULL、anchor 为 v1 且 anchor ∈ v1 归属集合的任务在 v2 机器上激活或恢复
- **THEN** 判定矩阵命中 legacy-A，以 anchor_session_id 为承接目标完成承接，binding 翻转为 v2、anchor 三元组翻转为 v2、归属行转换命名空间，写入迁移承接通知

#### Scenario: 并发写入 binding 后承接拒绝

- **WHEN** legacy-A 承接提交时，该任务的 `task_binding` 已被并发路径写入非空值
- **THEN** 承接事务 CAS 失配拒绝（ErrAdoptionConflict），整体回滚零写入，按冲突重试语义处理

#### Scenario: 存在不完整历史时维持停止

- **WHEN** 钉扎 v1、`task_binding` 为 NULL，且任务存在不完整历史——anchor_session_id 为空但存在 v1 归属行、或 anchor_session_id 不在该任务 v1 归属集合中、或 anchor 命名空间非 v1/default、或归属行含非 v1 命名空间
- **THEN** 判定矩阵维持 stop：停止承接、保留现状、last_error 如实告知，待用户决策；三者全空（无 binding、无 anchor、零归属行）继续走 B 分支（v2 首建），不属本停止场景
