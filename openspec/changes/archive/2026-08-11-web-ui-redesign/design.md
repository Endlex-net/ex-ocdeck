# Design: web-ui-redesign

## Context

ocdeck 的 Web UI（React 18 + TS + Vite，无 UI 库，约 5500 行）当前是"项目优先"的六页结构：无全局导航壳层、单一 ad-hoc 样式表（1707 行）、管理页面无移动端适配。UX 交付物（Open Design 项目目录，下称"设计稿"）给出"亮壳驾驶舱 × 深色终端"的完整重设计：4 屏 + 全局侧栏 + ⌘K 命令面板 + 亮/暗双主题 + token 化设计系统（`ocdeck-ui.css` 566 行，覆盖全部页面组件）。

设计稿是**视觉与交互规格**，不是代码契约：其 mock 数据未归一，且部分场景（权限等待、自动重试计数）在设计时无后端数据源。后者已通过源码级调研解决——opencode serve 1.18.14（本仓库锁定契约基线）确实暴露 permission/question 的 SSE 事件与 pending 查询端点（见 D6）。

关键约束（来自探索阶段用户决策）：一次性全量实施；保留现有 React 逻辑与终端子系统；允许后端小改；注意力交互边界为"信号+跳转"。

## Goals / Non-Goals

**Goals:**

- 落地设计稿全部 4 屏：指挥中心（新首页）、任务工作台（并入壳层）、项目管理（master-detail）、设置（四合一）
- 全局壳层：侧栏导航脊柱（⌘B 折叠）、亮/暗双主题、⌘K 命令面板
- 后端注意力信号：捕获 opencode 权限/问题 pending 请求并按任务透出
- TokenGate、ServerStatusBanner 保留并适配新壳层
- 全部页面响应式（设计稿分级：>1024 双栏 / ≤1024 钻取 / ≤767 紧凑）
- 旧路由深链重定向，不 404

**Non-Goals:**

- 终端子系统逻辑（IME 补偿、输入门控、触摸锁、手势、连接状态机、外观偏好）——仅视觉适配；**终端 palette 同步桥接（theme → xterm 原地更新）明确纳入范围**
- 自定义终端配色预设（后续扩展，本期只有亮/暗两套 palette 跟随应用主题）
- Web 端直接审批权限/回答问题（不新增 reply 代理端点；审批仍在 TUI 内）
- 后端 API breaking change、DB schema/迁移
- shell 终端数量上限（设计稿的"4 上限"是纯设计约定，不实现）
- 设计稿中自标"规划中"的三项：在终端打开、配置模板、worktree 占用
- 新增前端运行时依赖

## Decisions

### D1: 保留 React 逻辑，换肤 + 壳层叠加（而非按稿重写）

设计稿 HTML 只作为布局/状态/交互规格。现有组件的行为逻辑（状态机驱动按钮、自适应轮询、409 冲突处理、乐观锁保存、生成计数器防竞态）全部保留，改动限于：JSX 结构对齐设计稿、className 映射到设计系统、抽离新壳层组件。

**替代方案**（按设计稿 vanilla HTML 重写组件）：拒绝——状态机/轮询/终端工程是重资产，重写的回归风险远大于收益。

### D2: 设计系统落地方式

- `ocdeck-ui.css` 作为唯一样式基底引入 `web/src/`，token（`--bg/--surface/--fg/--muted/--border/--accent` + ink 对 + 语义色派生）原样保留；派生色只走 `color-mix(in oklch, …)`，禁止新增 hex 字面量（设计稿品牌契约）。
- 旧 `styles.css` 删除；`terminal/mobile.css` 保留（z-order 契约：终端 < 锁遮罩 < 浮动锁钮 < 连接状态遮罩，重设计不得破坏）。
- GitPanel 的 diff2html 横向滚动所有权不变量（仅 `.d2h-wrapper` 滚动）按设计稿 `.od-diff` 样式重述，行为不变。
- 图标从文本字符（▶ ⏸ ⟳ ⋯）换为 1.6px 描边单色 SVG `currentColor`，内联 React 组件，不引图标库。

### D3: 路由收敛与重定向

hash 路由保留（静态托管零要求的既有优势）。新路由表：

```
#/                    → 指挥中心（CommandCenterPage，新首页）
#/task/:id            → 任务工作台（保留 ?from 来源感知返回）
#/projects            → 项目管理 master-detail（#/projects#<projectID> 深链选中）
#/configs             → 设置四合一（#appearance/#env/#opencode/#ai 深链子标签）
#/active              → 重定向 #/
#/ai-config           → 重定向 #/configs#ai
#/project/:id         → 重定向 #/projects#<id>
```

App.tsx 的页面分发改为壳层内嵌：`<AppShell>{page}</AppShell>`；TokenGate 在壳层之外（未认证只见令牌页）。

工作台 `?from` 来源感知归一为单一映射（替代现有仅 `fromActive` 两分支，`TaskWorkbenchPage.tsx:21-27`）：`?from ∈ {home, projects, active}`，其中 `active` 为 legacy 别名映射到 `home`（旧 `#/task/:id?from=active` 链接不断）；未知值/缺省 → `home`。返回链接由统一纯函数解析：`home → #/`、`projects → #/projects#<projectID>`。各入口跳转时携带来源：指挥中心行 → `from=home`、项目管理任务行 → `from=projects`、侧栏任务组 → `from=home`。

### D4: 侧栏/指挥中心数据模型——扩展现有聚合 + 单一共享轮询

侧栏任务组与指挥中心都需要跨项目的"项目 → 任务"树。决策：**`GET /api/v1/projects` 响应项附加 `tasks` 摘要数组**（可加性字段，非 breaking），摘要字段：`id / name / status / init_status / branch / worktree_path / last_error / notice / updated_at（Unix 秒）/ agentStatus（活跃任务水合，可省略）/ attention_count`。字段集按指挥中心「需要关注」推导（失败态、init 失败、notice、排序时间）与侧栏呈现（状态点、注意力标记）的最小并集确定。

**单一共享轮询 store**：App 层持有一个 projects 轮询 store（5s single-flight，沿用 `usePoll` 语义），壳层侧栏、指挥中心、**项目管理页**从同一 store 消费；指挥中心另以 `GET /sessions/active`（5s single-flight）补充活跃任务的 `last_active_at / agentStatus / attention`。**MUST NOT 出现第二个 `/projects` 轮询**。store MUST 暴露 `refresh()`（trailing 语义）：任何变更操作成功后调用；若调用时已有轮询在途，MUST 在该请求结束后再补发一次——`refresh()` 承诺其结果反映调用之后的最新状态（不接受 mutation 前的在途快照）。两个快照不一致时不做请求内合并修复：各分区按各自快照呈现，下一轮轮询自然收敛（与现有快照语义一致）。

**替代方案**：新增 `GET /api/v1/sidebar` 专用聚合端点——拒绝，与现有 projects 轮询重复，多一个端点多一份一致性负担。

agent 状态注水沿用现有并发模式（`GET /sessions/active` 的 cap 8 / 3s 预算模式推广到 projects 列表的活跃任务）。

### D5: 主题系统

移植设计稿 `ocdeck-theme.js` 语义为 React hook（`useTheme`）：`localStorage['od-theme']` ∈ `system|light|dark`，默认 `system`；`<html data-theme>` 由 `index.html` 内联同步脚本设置防闪烁；设置页"终端外观"子标签内放主题分段控件。

**终端配色跟随应用主题**（用户在实现 review 阶段的明确决策，取代设计源"固定深底"条款——仅此一处偏离 brand-spec）：effective app theme → terminal palette 由统一 palette resolver 解析（CSS `--term-*` 变量与 xterm `ITheme` 同一事实来源，覆盖容器/前景/背景/光标/选区/ANSI 16 色）；主题切换（含 system 跟随 OS）时已挂载终端经 xterm `options.theme` 原地更新，不重建 TermSession/xterm、不重连 WebSocket，scrollback/焦点/锁状态/连接状态保持。自定义终端配色预设为后续扩展（本期 Non-Goal）。

### D6: 注意力信号（后端小改，核心外部契约）

opencode serve 1.18.14 源码确认（`packages/schema/src/v1/permission.ts`、`question.ts`，1.18.0–1.18.15 逐字节一致）：

```
SSE（GET /event，payload.type 分派，event: 字段恒为 message）：
  permission.asked   { id, sessionID, permission, patterns[], metadata, always[], tool? }
  permission.replied { sessionID, requestID, reply: once|always|reject }
  question.asked     { id, sessionID, questions[{question,header,options[{label,description}],multiple?,custom?}], tool? }
  question.replied   { sessionID, requestID, answers: string[][] }
  question.rejected  { sessionID, requestID }
  + v2 家族（permission.v2.asked/replied、question.v2.asked/replied/rejected）同形，两族都订阅
REST（per serve 实例，?directory=<worktree>）：
  GET /permission → PermissionV1.Request[]（pending 快照）
  GET /question   → QuestionV1.Request[]
注意：等待审批期间 session status 仍为 busy，不能靠 status 检测
```

ocdeck 侧设计：

- **捕获**：`internal/opencode/client.go` 的 SSE 订阅已用通用 `Event{type, properties}` envelope + Raw 透传。**事件进入 task 层的入口**：`OCClient.SubscribeEvents` 签名不变（仍回调 `opencode.Event`）；opencode 包新增纯函数 `ParseAttentionEvent(Event) (AttentionEvent, bool)` 完成 map → typed 解析（`AttentionEvent` 为 union：kind ∈ asked/replied/rejected + type ∈ permission/question + 规范化字段）；task runtime 在既有 SSE 事件 handler 中先调用该函数，命中则 `applyAttentionEvent(AttentionEvent)`。`replied/rejected` 携带的 requestID 不在 pending 集合中时 MUST 忽略（不构成错误）。
- **状态所有权与并发**：pending 集合归属**任务运行时（taskRuntime）**，与现有 runtime 锁结构同层：SSE 事件写入与 align 对账在任务锁内串行；Manager 提供只读快照方法（返回集合并拷，调用方不得持有内部引用）。SSE 写入与 API 并发读取的 data race 由"锁内写、拷贝出"保证。
- **新增接口签名**（职责分层）：
  - `internal/opencode`（typed 契约层）：`ListPermissions(ctx, dir) ([]PermissionRequest, error)`、`ListQuestions(ctx, dir) ([]QuestionRequest, error)`；404 映射为可导出的类型化错误（如 `ErrCapabilityUnsupported`，替代当前包内私有 404 实现 `client.go:186-194`），供调用方 `errors.Is` 判定。
  - task runtime（状态层）：`applyAttentionEvent(...)`、`reconcileAttention(ctx, client OCClient, dir string)`（快照对账；**client 类型为既有 `OCClient` 接口**（`internal/task/types.go:75-88`），接口新增 `ListPermissions`/`ListQuestions` 两个方法，全部 mocks/wrappers 同步扩展；client 与 dir 的来源与现有 AgentStatus 水合相同路径）、`attentionSnapshot() Attention`（拷贝）。
  - **三层类型模型（边界不混）**：

```
internal/opencode（外部契约层，不含本地字段）：
  PermissionRequest { ID, SessionID, Permission string; Patterns []string }
  QuestionRequest   { ID, SessionID string; Questions []struct{ Header, Question string } }
  AttentionEvent    { Kind asked|replied|rejected; Type permission|question; RequestID, SessionID string;
                      Permission/Questions 负载（asked 时） }
  ParseAttentionEvent(Event) (AttentionEvent, bool)  // map → typed 解析在此完成

internal/task（本地状态层）：
  PendingPermission { PermissionRequest; Since int64 }  // Since=本地首次观察时间
  PendingQuestion   { QuestionRequest;   Since int64 }

internal/api（DTO 层）：独立 JSON 结构，只做快照拷贝 → JSON 映射
```

  task 层 MUST NOT 直接消费非类型化的 `Event.Properties` map；opencode 类型 MUST NOT 含 `Since`。
  - task Manager（对 API 的公开组装入口）：`Attention(taskID) (Attention, bool)`（任务注意力只读快照）；`ListProjectTaskSummaries(ctx) ([]ProjectTaskSummary, error)`——`/projects` handler 经此一次性获取全部任务摘要，结构 `ProjectTaskSummary{ TaskID, Name, ProjectID, Status, InitStatus, Branch, WorktreePath, LastError, Notice, UpdatedAt, AttentionCount }`，agentStatus 注水沿用既有 TaskBackend 模式在 API 层并发完成。API 层 MUST NOT 读取 runtime 内部状态。
  - `internal/api`（DTO 层）：只做快照 → JSON 映射，不触达 opencode。
- **可选能力，永不致命**：注意力是可选增强，**任何注意力路径的失败 MUST NOT 影响任务状态机与 session align 的既有 fatal 语义**（`activate.go:734-741` 首次 align 失败中止激活、`:767-770` 重连 align 失败收敛 suspended 的路径不覆盖注意力对账）。
- **与既有 align 缓冲的接入点**：注意力事件与 session 事件共享同一 SSE 流与同一 `buffered` 缓冲（`activate.go:664-728`）。并发模型按路径区分，核心原则是**互斥只覆盖"事件应用 vs 集合替换"，不覆盖 REST 网络往返**：
  ① **激活/SSE 重连路径**——对账在 `alignMu` 临界区内、session align 完成之后、`drainAndRelease` 之前执行；期间到达的事件仍在 `buffered` 中（天然阻塞应用），对账结束后由 `drainAndRelease` 按到达序统一重放。session align 失败（既有 fatal 路径）时注意力对账 MUST NOT 执行，注意力对账失败 MUST NOT 阻止 `drainAndRelease`。
  ② **后台 30s 周期路径**——`alignMu`/`buffered` 是 `startSSE` 局部状态，后台循环不可访问。taskRuntime 持有专用 `attentionMu` 与 per-type 增量缓冲：对账开始时（`attentionMu` 内）置 per-type reconciling 标记；REST 请求**在锁外**发出；往返期间该类型的 SSE 事件由应用方发现 reconciling 标记后追加到增量缓冲而非直接写集合；写回阶段（`attentionMu` 内）：校验代际未变 → 原子替换该类型集合 → 按序应用增量缓冲 → 清标记。由此 REST 往返期间的 asked/replied 不被旧快照覆盖，且 SSE 应用方最多只在置标记/追加缓冲的临界区内短暂阻塞。
- **双触发仲裁（per-type reconcile epoch + reconciling 所有权）**：同一任务同一类型同一时刻只有一个在途对账生效。taskRuntime 持有 per-type `reconcileEpoch`（atomic）与 reconciling 状态（owner epoch + 增量缓冲）：新对账发起时（`attentionMu` 内）推进 epoch 并成为 reconciling owner——**接管时 MUST 先将旧缓冲按序归并到当前（旧）集合并清空缓冲，新 owner 从空缓冲开始**（否则旧缓冲里的事件会被重放到更新的 REST 快照上：断连期间已被回复的请求会被错误复活）；**只有 owner 有权清标记与归并/丢弃缓冲**——被抢占的旧对账退出时 epoch 失配，MUST NOT 触碰 reconciling 标记与缓冲。写回时校验 epoch 未变才生效。后台 REST 在途时发生重连 align 的场景：align 路径发起新对账（epoch 推进、归并旧缓冲、接管 owner）→ 后台在途结果写回被拒且不触碰状态 → 以 align 路径的新快照为准，仅重放新 owner 启动后观察到的增量。
- **增量缓冲结果表**（任何出口 MUST 经 defer 处理，且仅 owner 可清标记/动缓冲）：

```
对账结果                集合操作              增量缓冲
200                     原子替换该类型集合     按序重放到新集合
非404失败（→degraded）   保留旧集合            按序重放到保留集合（事件真实发生过，MUST NOT 丢）
404（→unsupported）      清空该类型集合         丢弃（unsupported 后该类型事件本就忽略）
canceled（仍是 owner）   不写回                清标记、丢弃缓冲（集合由挂起/删除流程清空）
epoch 失配（非 owner）   不写回                丢弃自身结果，不动共享标记与缓冲
```

```
激活/重连时序（alignMu 临界区内，每任务串行）：
  1. session align（既有，fatal 语义不变）；失败 → 走既有 fatal 路径，不执行 2
  2. attention reconcile（新增，非 fatal，permission 与 question 各自独立）：
     a. GET /permission + GET /question 并发请求（期间 SSE 事件仍在 buffered）
     b. 每类型独立完整解析到临时集合
     c. 按结果分三种路径（每类型独立）：
        - 200：任务锁内原子替换该类型集合
        - 非404失败：该类型迁 degraded，保留现有集合
        - 404：该类型迁 unsupported，清空该类型集合
        任一类型路径 MUST NOT 影响另一类型集合
  3. drainAndRelease（既有）：按到达序重放 buffered 全部事件（session + 注意力）；
     unsupported 类型的注意力事件在重放时被忽略
```

- **degraded 恢复调度**：除激活/SSE 重连两个对账时机外，`degraded` 状态的任务 MUST 挂入既有 30s 后台循环（`manager.go:595` backgroundLoop，新增一个 retryable 处理项，沿用其既有模式）做周期对账重试，迁回 `available` 即移出；不新增 goroutine/定时器。挂起/删除时从周期重试中移除。
- **runtime 生命周期仲裁**：taskRuntime 携带**独立的 `attentionEpoch atomic.Uint64`**（不复用现有 activation `generation`——后者是 registry 三元组回调校验的一部分，`manager.go:261-289`，改造它会破坏现有隔离不变量）；推进 MUST NOT 依赖 `attentionMu`（否则挂起会被在途对账阻塞）。挂起/删除时先推进 `attentionEpoch`、再清空 pending 集合（短暂持 `attentionMu`）。对账写回 MUST 在 `attentionMu` 内校验 `attentionEpoch` 未变才生效——后台循环持有旧 runtime 发起的在途对账在挂起/删除后 MUST NOT 写回状态（防止清理后重写入，`suspend.go:65` 竞态）。teardown/shutdown 导致的 `context.Canceled` 为中性结果：MUST NOT 触发能力状态迁移（不迁 `degraded`）、MUST NOT 写回集合。

- **能力状态机（per 任务 × per 类型，permission 与 question 各自独立）**：

```
unknown    ──200──────▶ available   （正常对账/透出）
unknown    ──404──────▶ unsupported （该类型能力关闭，透出空数组）
unknown    ──非404错误─▶ degraded    （无旧值可留，透出空数组，继续重试）
available  ──非404错误─▶ degraded    （保留最后成功集合，透出旧值，继续重试）
degraded   ──200──────▶ available   （恢复）
available/degraded ──404──▶ unsupported （运行期实例降级/端点消失，透出转空数组）
```

  失败语义表：404 → `unsupported`；401/5xx/超时/坏 JSON → `degraded`；permission 与 question 一端失败另一端成功 → 各自独立迁移，互不影响。REST 对账开关：`available/degraded` 在每个对账时机执行对账；`unsupported` 停止对账。SSE 事件语义按能力状态区分：`available/degraded` 照常登记（degraded 的透出 = 最后成功快照 + 其后 SSE 增量）；`unsupported` 忽略该类型 SSE 事件（该实例本就不具备此能力），透出恒为空数组且不计入 `attention_count`。`replied/rejected` 事件的枚举值（reply/answers）不做校验：任何 replied/rejected 一律按"该 requestID 已了结"从集合移除。
- **不落库**：opencode 实例的 pending 本就是内存态，重启即失，与任务挂起/重启语义天然一致。挂起/删除时清空集合。
- **typed 契约（opencode 层结构，仅取所需字段，未知额外字段忽略；本地 Since 只存在于 task 层 Pending 类型，见三层类型模型）**：

```
PermissionRequest { ID string; SessionID string; Permission string; Patterns []string }
QuestionRequest   { ID string; SessionID string; Questions []struct{ Header, Question string } }
```

  关键字段（缺失即事件/条目非法）：`id`、`sessionID`；permission 请求的 `permission`；question 请求的 `questions`（非空数组，元素取 `header`/`question`）。**错形语义（对账与事件解析同一规则）**：HTTP 200 但 body 为 null/非数组/数组含非法元素 → 该类型整体视为对账失败 → 迁 `degraded`（MUST NOT 部分采纳）；SSE 事件关键字段缺失/错形 → 忽略该事件。`patterns`/`metadata`/`always`/`tool` 等其余字段一律可选，仅 `patterns` 透出到 API。
- **透出**：`GET /api/v1/tasks/{id}` 与 `GET /api/v1/sessions/active` 响应附加 `attention` 字段：
  ```json
  { "permissions": [{ "id", "permission", "patterns", "since" }],
    "questions":  [{ "id", "questions": [{ "header", "question" }], "since" }] }
  ```
  `since` 为 Unix 秒：SSE `asked` 事件到达时刻；REST 快照对账时同 ID 保留原 `since`，新出现的 ID 取对账时刻（快照本身无时间字段）。无 pending 时两数组为空 `[]`（非 null、不省略）。`GET /api/v1/projects` 的任务摘要只带 `attention_count`。
- **降级**：SSE 事件解析失败/字段缺失 → 忽略该事件不报错；透出永远可用（内存集合读取），能力状态决定透出内容（`unsupported` 空数组 / `degraded` 最后成功快照 + SSE 增量）。遵守既有"能力探测、版本仅告警"策略。

### D7: 指挥中心"需要关注"定义与排序

注意力项（全部有数据源，无虚构）：

```
1. deletion_failed / creation_failed     → 操作集见下（按代码现实）
2. init failed（suspended + init_status=failed）→ 操作：查看日志/重跑初始化
3. 等待权限确认（attention.permissions 非空）→ 点击跳转工作台 TUI
4. 等待回答问题（attention.questions 非空）→ 点击跳转工作台 TUI
5. notice 残留（如残留进程待清理）
6. agent idle 的活跃任务（干完了等指令）
```

- **单任务去重**：一个任务在「需要关注」中 MUST 只出现一行，取最高优先级类别作为主呈现；其余命中类别以次要标记同行展示（如"另有权限请求待确认"）。
- **失败态操作集（按代码现实，`TaskActions.tsx:26-39`、`DeleteTaskModal.tsx:32`）**：`creation_failed` → 重试 + 普通删除 + 行内展示 `last_error`（无强制删除选项、无独立日志端点）；`deletion_failed` → 重试 + 强制删除 + pre-delete 日志查看。
- **排序**：类别优先级降序；同类内按任务最近时间倒序（活跃任务用 `last_active_at`，非活跃任务用 `updated_at`）。
- **过渡态分区**：`creating/activating/suspending/deleting` 任务归「其余活跃任务」区并呈现过渡徽章，不进入「需要关注」（失败态除外——失败态总是进「需要关注」）。

1/2/5/6 纯前端从现有数据推导；3/4 依赖 D6。设计稿的"自动重试第 N 次"场景不实现（retry 状态已由 AgentStatusBadge 覆盖）。

### D8: 工作台任务切换——保留 key-remount

设计稿的"就地切换、零跳转"简化为：侧栏任务点击 → `navigate(#/task/:id)` → workbench 按 `key={taskID}` 重挂载（现状不变）。终端重连由 tmux 恢复画面，用户感知为"切换"。

**替代方案**（多工作台实例 keep-alive）：拒绝——多终端 WS 并发 + 状态驻留的复杂度高，且"单交互客户端"语义下收益有限。

### D9: 删除任务交互统一为后端语义

以项目页设计稿为准（脏 worktree 勾选确认 + `deletion_failed` 时的强制删除选项，`DeleteTaskModal.tsx:32`）；工作台稿的"删除需先挂起"提示改为状态机自然表达（活跃态不出现删除按钮，与现状 `actionsFor` 一致）。DeleteTaskModal 逻辑不变，仅换肤。pre-delete 日志入口仅在 `deletion_failed` 且 `last_error` 以 `pre-delete:` 前缀时出现（与 `TaskWorkbenchPage.tsx:157` 现状一致）。

### D10: ⌘K 命令面板自实现

按设计稿 `ocdeck-palette.js` 行为规格移植为 React 组件：动态注册表与源一致——6 个静态入口（指挥中心、项目管理、设置四子标签深链，`docs/design/assets/ocdeck-palette.js:12` 注册表）+ 侧栏任务 + "新建任务/注册项目"操作；关键词模糊匹配（含中文）、↑↓/Enter/Esc。快捷键 ⌘K 与 Ctrl+K 均触发。不引入 cmdk 等第三方库。

### D11: 测试策略

- **后端**：`internal/opencode` 新增事件解析单测（5+5 事件类型 fixture）；task runtime pending 集合的生命周期测试（asked→replied 移除、未知 requestID 忽略、原子替换、挂起清空、404→unsupported、非 404→degraded 保留旧值、per 类型独立迁移）；API 透出字段测试；并发读写测试（SSE 写入 × API 快照读取）必须 `-race` 通过。沿用既有 Go 测试模式。
- **前端**：项目无 jsdom/Testing Library（`web/package.json`），且本 change 不新增任何依赖——可测逻辑 MUST 抽取为纯函数单测：注意力推导/排序 selector、路由解析与重定向映射、主题 resolver（`system|light|dark` → 实际主题）、命令面板匹配器。组件换肤不写快照测试。
- **验证**（按序，前端构建产物是 Go 编译前提——`web/embed.go` embed dist）：`pnpm --dir web install --frozen-lockfile` → `pnpm --dir web test` → `pnpm --dir web build` → 仓库根 `go test -race ./...` → `go vet ./...` + 手工四屏走查清单（tasks.md 列出具体检查项）。

### D12: 设计源固化与入口裁决

- **设计源 vendor 进仓库**：已固化入 `docs/design/`：根目录 `brand-spec.md`（品牌契约）；`assets/`（`ocdeck-ui.css` 样式基底、`ocdeck-palette.js` ⌘K 行为规格、`ocdeck-theme.js` 主题行为规格、`ocdeck-sidebar.js` 侧栏折叠行为规格）；`screens/`（4 个 canonical 页面稿：command-center / task-workbench / projects / configs，布局/状态/交互规格，资源引用 `../assets/*` 可解析、可直接渲染）。各文件头注明来源项目 ID 与固化日期；**mock 数据与文案不具契约效力，行为以本 change 的 normative spec 为准**。
- **设计源冲突裁决：`brand-spec.md` 为权威**。vendored `ocdeck-ui.css` 与品牌契约冲突处已收编并在文件头注明：`color:#fff` → `var(--on-ink)`；暗色 6 品牌 token + `--term-bg` 对齐 brand-spec 暗色表；触屏目标 ≥44px（`.od-term-lock-btn` 40→44、移动端 `.od-btn-sm` 36→44、移动端输入控件 34→44，对齐 web-ui-shell spec）。**唯一的权威覆盖例外**：终端配色——用户在实现 review 阶段决策"终端跟随应用主题"，取代 brand-spec 的"终端固定深底"条款（brand-spec.md 文件头已注明 supersede）；`--ink/--on-ink` 仍用于品牌标记、toast 等固定墨色组件。
- **两条进一步裁决**：
  1. brand-spec 的"语义色一律 color-mix 派生、不引入新色相字面量"条款适用于**组件级规则**；token 定义层（`:root`/暗色块的 `--success/--warn/--danger/--term-*`）是色彩值的唯一事实来源，允许直接 oklch 定义（`ocdeck-ui.css:32-35,50-54`）。理由：从 `--fg/--accent` 色相差派生绿/红在色彩学上不成立，brand-spec 该条款为理想化表述。
  2. 44px 触屏目标约束适用于**实现层**（共享设计系统的移动端规则已对齐）；页面稿内联 mock 样式（如 `docs/design/screens/configs.html` 页内 30-36px 控件）仅为演示，不作实现依据。行内密集辅助控件（tab 关闭/添加、行内图标按钮）与密集列表控件（tab 条目、命令面板条目）豁免 44px，移动端 ≥32px 并以间距隔离——避免密集布局被触屏目标撑破（已在 vendored CSS 移动端规则落地）。
  3. **移动端任务直达**：≤767px 侧栏任务组隐藏（`docs/design/assets/ocdeck-ui.css:495`），任务切换入口由工作台页头的任务切换器承担（设计源 `task-workbench.html:229`），切换器执行与侧栏相同的 hash 导航 + `key={taskID}` 重挂载。
- **新建任务入口收敛**：遵循设计稿（`screens/projects.html:398` "新建任务已移至指挥中心"）——指挥中心为唯一新建入口；项目管理单页概览不提供创建表单，改为"前往指挥中心新建任务"提示链接。

## Risks / Trade-offs

- [experimental API 漂移：permission/question 端点不在 opencode 官方文档与 /doc OpenAPI] → 契约基线锁 1.18.14（源码逐字节确认 1.18.0–1.18.15 一致）；404 → `unsupported` 透出空数组，其余错误 → `degraded` 透出旧值（D6 状态机）；能力探测策略不变。
- [v1/v2 双事件家族，若只订阅 v1 在 v2 runner 启用时漏信号] → 两族都订阅，同一归一化处理。
- [一次性全量 = 大 diff，review 与回归成本高] → tasks.md 分组为显式依赖 DAG（样式基底/后端信号可并行 → 壳层 → 指挥中心/项目管理/设置 → 工作台收尾与旧代码删除），组内连续、组间可停；终端子系统不动作为最大风险隔离。
- [换肤误伤终端工程契约（z-order、IME、触摸锁、diff 滚动）] → mobile.css 不改、终端组件只换颜色/字号 token、现有 vitest 套件必须全绿。
- [`GET /projects` 附加 tasks 摘要增加单次响应体积] → 摘要字段按最小并集（10 个非水合摘要字段 + 1 个 API 水合字段 agentStatus，见 D4；attention_count 来自 runtime 内存集合而非 store 查询）；个人工具任务量级（<50）无实际压力。
- [注意力对账失败误伤核心生命周期] → 注意力 reconcile 与 session align 分离（D6 时序），任何注意力失败只影响自身能力状态机，MUST NOT 触发激活中止或收敛 suspended。
- [壳层与指挥中心双轮询 `/projects` 导致重复请求与视图不一致] → App 层单一共享轮询 store（D4），页面只消费。
- [SSE 写 pending 与 API 并发读的 data race] → 任务锁内写、快照拷贝出（D6 并发所有权），`-race` 测试覆盖。
- [暗色主题为新增表面，徽章/告警硬编码色值可能漏改] → 硬编码色值全部收编为 token 派生，tasks.md 单列审计任务。
- [pending 注意力为双内存态（opencode 实例 + ocdeck），存在漂移窗口] → SSE 重连 align 时用 pending 快照对账；挂起时清空。

## Migration Plan

纯前端替换 + 后端兼容扩展，无数据迁移：

1. 后端注意力字段为可加性扩展，旧前端（若有缓存页面）忽略新字段不破坏。
2. 前端一次性替换；旧 localStorage key（`ocdeck.token`、终端外观 prefs）全部保留不变；新增 `od-theme`、`ocdeck:side-collapsed`。
3. 回滚 = git revert；无状态依赖。

## Open Questions

无。侧栏归档任务（不显示）、新建任务入口（指挥中心唯一）、creation_failed 操作集（重试 + 普通删除 + last_error 行内）均已裁决并写入 normative spec（web-ui-shell / command-center / project-management）。
