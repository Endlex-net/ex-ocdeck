# Agent Attention Specification

## Purpose

注意力信号能力：从每个活跃任务的 opencode 实例捕获权限/问题 pending 请求（SSE 事件 + REST 快照对账），按任务维护内存态集合并经 API 透出，支撑指挥中心「需要关注」分区的"等待权限确认/等待回答问题"信号。透出只读——不提供 Web 端审批，审批动作在任务 TUI 内完成。

## Requirements

### Requirement: opencode 权限/问题事件捕获

系统 SHALL 在每个活跃任务的 opencode SSE 订阅中捕获权限与问题事件（v1 家族 `permission.asked` / `permission.replied` / `question.asked` / `question.replied` / `question.rejected`，及同形 v2 家族 `permission.v2.asked` / `permission.v2.replied` / `question.v2.asked` / `question.v2.replied` / `question.v2.rejected`），并按任务维护 pending 请求集合：`asked` 事件按 `id` 登记，`replied`/`rejected` 事件按 `requestID` 移除；`replied`/`rejected` 携带的 requestID 不在集合中时 MUST 忽略（不构成错误）。事件按 payload 的 `type` 字段分派（SSE `event:` 字段恒为 `message`，MUST NOT 用于分派）。未识别的事件类型或字段缺失的事件 MUST 被忽略且不产生错误。SSE 事件写入 MUST 在任务锁内完成，与 REST 对账替换串行化。

#### Scenario: 捕获权限请求

- **WHEN** 某活跃任务的 opencode 实例发出 `permission.asked`（`id=per_123`、`permission=edit`、`patterns=["web/src/a.ts"]`）
- **THEN** 该任务的 pending 集合新增对应权限请求记录

#### Scenario: 回复后移除

- **WHEN** pending 集合中存在 `per_123`，随后收到 `permission.replied`（`requestID=per_123`）
- **THEN** `per_123` 从 pending 集合移除

#### Scenario: v2 家族等价处理

- **WHEN** 收到 `question.v2.asked` 事件
- **THEN** 按与 `question.asked` 相同的规则登记 pending 问题请求

#### Scenario: 未知事件静默忽略

- **WHEN** 收到未知类型或缺少关键字段的事件
- **THEN** 事件被忽略，任务状态与 pending 集合不变，不产生错误日志以外的副作用

#### Scenario: 未知 requestID 的回复事件忽略

- **WHEN** 收到 `permission.replied`，其 `requestID` 不在该任务 pending 集合中
- **THEN** 事件被忽略，pending 集合不变，不构成错误

#### Scenario: 关键字段定义

- **WHEN** 解析 asked 事件或 REST pending 条目
- **THEN** 关键字段为 `id`、`sessionID`（permission 请求另含 `permission`；question 请求另含非空 `questions` 数组，元素取 `header`/`question`）；其余字段（`metadata`/`always`/`tool` 等）可选且未知字段忽略

#### Scenario: REST 200 但错形整体失败

- **WHEN** 对账时 `GET /permission` 返回 200 但 body 为 null、非数组或数组含非法元素
- **THEN** 该类型整体视为对账失败迁 `degraded`，MUST NOT 部分采纳任何元素

#### Scenario: 字段级校验细则

- **WHEN** 解析 asked/replied/rejected 事件或 REST 条目
- **THEN** 以下情况按非法处理（事件忽略 / REST 整体失败）：replied/rejected 缺非空 `sessionID` 或 `requestID`；question 元素缺非空 `header` 或 `question`；`patterns` 非字符串数组

#### Scenario: teardown 取消不迁 degraded

- **WHEN** 对账因任务挂起/服务关停导致 `context.Canceled`
- **THEN** 能力状态不迁移、集合不写回，视为中性结果

#### Scenario: 挂起后在途对账不写回

- **WHEN** 后台循环对某任务发起的对账在 REST 在途期间任务被挂起（代际推进、集合清空）
- **THEN** 对账返回后写回被代际校验拒绝，pending 集合保持清空

### Requirement: pending 状态生命周期与对账

任务的 pending 权限/问题集合 MUST 为内存态，MUST NOT 持久化到数据库。任务挂起/删除时 MUST 清空该任务的 pending 集合。

**对账并发模型（两条触发路径）**：互斥 MUST 只覆盖"SSE 事件应用 vs 集合替换"，MUST NOT 覆盖 REST 网络往返（否则挂起会被在途对账阻塞）。① **激活/SSE 重连路径**：对账在既有 `alignMu` 临界区内、session align 完成之后、`drainAndRelease` 之前执行（期间到达的事件仍在既有 `buffered` 中，对账结束后按到达序统一重放）；session align 失败（既有 fatal 路径）时 MUST NOT 执行注意力对账；注意力对账失败 MUST NOT 阻止 `drainAndRelease`。② **后台 30s 周期路径**：任务运行时持有专用注意力互斥与 per-type 增量缓冲——对账开始（互斥内）置 per-type reconciling 标记；REST 请求在锁外发出；往返期间该类型 SSE 事件追加到增量缓冲而非直接写集合；写回（互斥内）：校验代际与 epoch（见下）→ 原子替换该类型集合 → 按序应用增量缓冲 → 清标记。

**双触发仲裁**：同一任务同一类型同一时刻只有一个在途对账生效——per-type `reconcileEpoch`（atomic）+ reconciling 状态（owner epoch + 增量缓冲）：新对账发起时（互斥内）推进 epoch 并成为 reconciling owner——**接管时 MUST 先将旧缓冲按序归并到当前（旧）集合并清空缓冲，新 owner 从空缓冲开始**（否则断连期间已被回复的旧请求会被旧缓冲错误复活到新快照上）；**只有 owner 有权清标记与归并/丢弃缓冲**——被抢占的旧对账退出时 epoch 失配，MUST NOT 触碰 reconciling 标记与缓冲。写回时校验 epoch 未变才生效（后台 REST 在途时发生重连 align，以 align 路径的新快照为准，仅重放新 owner 启动后观察到的增量，后台结果被拒且不触碰状态）。

**增量缓冲结果表**（任何出口 MUST 经 defer 处理，且仅 owner 可清标记/动缓冲）：200 → 替换后按序重放到新集合；非 404 失败（→`degraded`）→ 保留旧集合并按序重放到保留集合（事件真实发生过，MUST NOT 丢）；404（→`unsupported`）→ 清空集合并丢弃缓冲；canceled（仍是 owner）→ 不写回、清标记、丢弃缓冲（集合由挂起/删除流程清空）；epoch 失配（非 owner）→ 不写回，丢弃自身结果、不动共享标记与缓冲。由此 REST 往返期间到达的 asked/replied MUST NOT 被旧快照覆盖。

**对账流程**：`GET /permission` 与 `GET /question`（携带任务 `directory` 参数）并发请求，**每类型独立**完整解析到临时集合；**200** → 按上述模型替换该类型自身集合；**非 404 失败** → 该类型迁 `degraded`，保留现有集合；**404** → 该类型迁 `unsupported`，清空该类型集合。任一类型路径 MUST NOT 修改另一类型的集合。注意力对账 MUST 独立于 session align 的成败语义：任何注意力对账失败 MUST NOT 中止激活、MUST NOT 触发任务收敛为 suspended、MUST NOT 改变任务状态机。

**runtime 生命周期仲裁**：任务运行时 MUST 携带独立的 `attentionEpoch`（atomic Uint64，不复用现有 activation generation——后者参与 registry 三元组回调校验，不可改造）；推进 MUST NOT 依赖注意力互斥；挂起/删除时先推进 `attentionEpoch`、再清空 pending 集合。对账写回 MUST 在校验 `attentionEpoch` 未变后才生效——后台循环持有旧 runtime 发起的在途对账在挂起/删除后 MUST NOT 写回状态。teardown/shutdown 导致的 `context.Canceled` 为中性结果：MUST NOT 触发能力状态迁移（不迁 `degraded`）、MUST NOT 写回集合。

**对账时机** MUST 覆盖三处：任务激活、SSE 重连 align、以及 `degraded` 状态的周期重试——`degraded` 任务 MUST 挂入既有 30s 后台循环（Manager backgroundLoop 新增 retryable 处理项）周期对账，迁回 `available` 即移出，MUST NOT 新增 goroutine/定时器；挂起/删除时从周期重试移除。

系统 SHALL 按任务 × 类型（permission 与 question 各自独立）维护能力状态机：`unknown` → 200 迁为 `available`、404 迁为 `unsupported`、非 404 错误迁为 `degraded`；`available` 遇非 404 错误（401/5xx/超时/坏 JSON）迁为 `degraded`；`degraded` 对账 200 迁回 `available`；**任一非 `unsupported` 状态收到 404 MUST 迁为 `unsupported`**（运行期实例降级/端点消失）。语义：`unsupported` 该类型停止 REST 对账、忽略该类型 SSE 事件、透出恒为空数组且不计入 `attention_count`；`degraded` 保留最后成功集合、SSE 事件照常登记（透出 = 最后成功快照 + 其后 SSE 增量）、继续在每个对账时机重试；`unknown + degraded` 无旧值时透出空数组。两类型状态 MUST 独立迁移。`replied/rejected` 事件的枚举值（reply/answers）MUST NOT 校验：一律按"该 requestID 已了结"从集合移除。

#### Scenario: 重连后对账

- **WHEN** SSE 断连期间用户在 TUI 内回复了一个权限请求，随后 SSE 重连 align
- **THEN** 注意力对账以 `GET /permission` 快照原子替换 pending 集合，已回复的请求不再出现

#### Scenario: 挂起清空

- **WHEN** 某任务存在 pending 权限请求，随后被挂起
- **THEN** 该任务的 pending 集合被清空；再次激活时以快照对账重建

#### Scenario: 老版本 opencode 降级

- **WHEN** 某任务的 opencode 实例对 `GET /permission` 返回 404
- **THEN** 该任务 permission 能力置为 `unsupported`，后续不再请求该端点，透出空数组，任务其余功能不受影响

#### Scenario: 瞬时错误保留旧值

- **WHEN** 某任务 permission 能力为 `available` 且有 1 个 pending 请求，对账时 `GET /permission` 返回 500
- **THEN** 能力状态迁为 `degraded`，pending 集合保留旧值照常透出，下次对账时机重试

#### Scenario: degraded 期间 SSE 照常增量

- **WHEN** 某任务 permission 能力为 `degraded`（最后成功快照含 1 个请求），随后收到新的 `permission.asked`
- **THEN** 新请求照常登记，透出 = 旧快照 + 新事件增量

#### Scenario: unsupported 忽略该类型事件

- **WHEN** 某任务 permission 能力为 `unsupported`，随后收到 `permission.asked`
- **THEN** 事件被忽略，透出恒为空数组，`attention_count` 不计该请求

#### Scenario: unknown 瞬时错误

- **WHEN** 某任务 question 能力为 `unknown`（从未对账），首次对账返回 500
- **THEN** 能力状态迁为 `degraded`，透出空数组，下次对账时机重试

#### Scenario: degraded 周期重试恢复

- **WHEN** 某任务 permission 能力为 `degraded` 且长时间无 SSE 断连
- **THEN** 既有 30s 后台循环周期执行对账重试，对账 200 后迁回 `available` 并移出周期重试

#### Scenario: 两类型独立迁移

- **WHEN** 对账时 `GET /permission` 返回 404 而 `GET /question` 返回 200（含 1 个 pending 问题）
- **THEN** permission 置为 `unsupported`（透出空数组），question 置为 `available`（透出该 pending 问题），互不影响

#### Scenario: 对账失败不伤任务

- **WHEN** 任务激活过程中注意力对账以任意错误失败
- **THEN** 激活流程按既有语义完成，任务状态机不受任何影响

### Requirement: 注意力信号 API 透出

系统 SHALL 在 `GET /api/v1/tasks/{id}` 响应中附加 `attention` 字段：`{ "permissions": [{ "id", "permission", "patterns", "since" }], "questions": [{ "id", "questions": [{ "header", "question" }], "since" }] }`。`since` MUST 为 Unix 秒的**本地首次观察时间**：pending 条目首次进入 ocdeck 集合的时刻（SSE `asked` 到达或对账快照中作为新 ID 出现）；对账替换时，新旧集合中都存在的 ID 保留原 `since`。**注意**：`since` 是 ocdeck 本地观察时间而非 opencode 权威时间——opencode 1.18.14 允许调用方自定义请求 ID，不保证 ID 单调或不复用；极端情况下（复用 ID 且错过 replied 事件）`since` 可能偏旧，UI 仅用于相对时间展示，可接受。无 pending 请求时两个数组为空（MUST NOT 省略字段或返回 null）；能力状态为 `unsupported` 时对应类型透出空数组，`degraded` 时透出最后成功集合叠加其后 SSE 增量。`GET /api/v1/sessions/active` 的每个元素 MUST 附加相同结构的 `attention` 摘要。`GET /api/v1/projects` 的任务摘要 MUST 附加 `attention_count`（pending 权限 + 问题总数）。注意力透出 MUST 为只读：MUST NOT 提供权限回复或问题回答的代理端点。注意力查询 MUST NOT 引入新的 opencode 调用（数据来自内存集合的快照拷贝）。

#### Scenario: 任务详情携带注意力

- **WHEN** 某任务有 1 个 pending 权限请求与 1 个 pending 问题请求
- **THEN** `GET /api/v1/tasks/{id}` 响应的 `attention.permissions` 与 `attention.questions` 各含 1 个元素，字段完整

#### Scenario: 无 pending 时空数组

- **WHEN** 某任务无任何 pending 请求
- **THEN** `attention` 字段存在且两个数组均为 `[]`

#### Scenario: 无回复代理端点

- **WHEN** 客户端尝试通过 ocdeck API 回复权限请求
- **THEN** 不存在此类端点（请求返回 404/405）
