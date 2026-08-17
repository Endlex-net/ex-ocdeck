# Design: SSE 化活跃会话推送与内部事件总线

## Context

指挥中心（`web/src/pages/CommandCenterPage.tsx:157-188`）以 5s single-flight 轮询 `GET /api/v1/sessions/active`。后端 handler（`internal/api/tasks.go:520-563`）每次请求做三件事：SQLite 聚合查询（`internal/store/queries.go:390-420`）、读 attention 内存快照、对每个活跃任务**实时探测**其 opencode serve 的 `/session/status`（并发上限 8、3s 预算）。与此同时，每个活跃任务已经持有一条 ocdeck→opencode 的 SSE 订阅（`internal/task/activate.go:960-1108 startSSE` → `handleSSEEvent`），session 与 attention 的变更实时到达 task 层——但 task 层与 api 层之间没有通知机制，api 层只能在请求到达时现查。

约束事实（均已原文核实）：

- 服务端为 Go stdlib `http.ServeMux`，无压缩/CORS 中间件，`http.Server` 无写超时（`internal/api/server.go:275-280`）——对 SSE 长连接友好。
- `/api/v1/*` 统一走 Bearer 认证中间件（`internal/api/middleware.go:50-67`）；浏览器原生 `EventSource` 无法自定义请求头。
- opencode SSE 事件 envelope 为 `{type, properties}`（`internal/opencode/client.go:68`）；`session.created/updated/deleted` 带 `properties.info.directory`（可信归属），status/diff 类事件仅带 `properties.sessionID`（须反查 `task_sessions` 归属，`activate.go:1190-1203`）。agent 状态三值枚举 `idle/busy/retry`（`client.go:46-53`）。opencode 心跳注释行在 client 层被消费（`client.go:725-736`），不会进入 task 层事件回调。
- 前端 API client 统一经 `request()`（`web/src/api.ts:47-99`）注入 Bearer 头并处理 401（`clearToken()` + `UNAUTHORIZED_EVENT`）。
- 任务状态生命周期操作集中在 `task.Manager`：`Create/Activate/Suspend/Archive/Restore/Delete`（`internal/task/crud.go`、`activate.go`、`suspend.go`、`delete.go`），状态落账经 `store.UpdateTaskStatus`（`internal/store/queries.go:423`）与 `UpdateTaskStatusConditional`（`queries.go:615`）。**注意存在非常规迁移路径**：异常收敛 `convergeToSuspended` 直接 CAS `active→suspended`（`activate.go:1502`），Suspend 修复路径 CAS `suspending→active`（`suspend.go:115`）。

## Goals / Non-Goals

**Goals:**

- **Phase 1 交付物：DDD 分层重构 + 事件总线 + 全部领域事件生产**：D0 落地 Task/Session/ServeRuntime 领域模型、consumer-owned Repository ports、结构化提交结果与 application 集中 commit helper；进程内领域事件总线建成；**全部** D2 生产点（task 生命周期/session/attention/run_status/activity/异常收敛债务）在本阶段完成挂接，含 Phase 0 实测门禁与 agentStatus 事件驱动建模（D4 生产侧）。Phase 1 不改变任何 REST/WS 对外行为与前端轮询（唯一显式差异为同值写不再推进 `updated_at`，见 D0 与 task-lifecycle delta）。
- **Phase 2 交付物：消费侧 SSE 适配**：共享快照组装 helper、`GET /api/v1/sessions/active` 改读 agentStatus 内存快照、`GET /api/v1/sessions/active/stream` SSE 端点（连接即推全量快照，此后事件驱动推送，固定 500ms 合并窗口）、`statusRecorder` FlushError/BaseContext 改造与前端 fetch streaming 订阅（移除 5s 轮询，自实现重连退避）。

**Non-Goals:**

- 不改 `/projects`、`/tasks/{id}`、`/server/status` 的轮询（事件总线预留能力即可）。
- 不引入 WebSocket 替代、不改认证模型、不加压缩中间件、无 DB schema 变更。
- 不删除既有 REST 端点 `GET /api/v1/sessions/active`（保留作兼容与调试）。
- 事件总线不做跨进程/持久化/至少一次投递保证（进程内、at-most-once、以周期对齐兜底）。

## Decisions

### D0: DDD 分层与领域模型（Phase 1，先建模后 SSE）

本变更分两阶段：**Phase 1 先完成内部 DDD 分层重构、领域事件总线与全部事件生产点**（REST/WS 对外行为不变），**Phase 2 仅做消费侧 SSE 适配**（D3-D5 与前端）。重构动机：现有 `internal/task` Manager 是单体编排器，层间无端口与领域模型，事件无处产生——事件总线必须建在明确的领域模型之上，否则发布点只能是临时补丁。

**目标包布局**（迁移期间不为目录整齐一次性搬动稳定包；消除反向依赖优先于物理移动）：

```text
internal/
  domain/
    task/                 Task、Status、InitStatus、Notice、DeleteMode、状态机 guard
    session/              Session、Ownership（领域 ID = session_id）
    event/                Event、Topic/Type 常量、typed payload 与构造器
  application/
    task/                 单一 LifecycleService（用例编排，沿用按文件分用例）、consumer-owned Repository/外部端口
    runtime/              ServeRuntimeRegistry、RunStatus、Attention、收敛债务
  infrastructure/
    sqlite/               包装现有 store.DB 的 repository adapter
    eventbus/             进程内 Bus（见 D1）
  store/ opencode/ process/ worktree/   迁移期保留现有实现
  api/                    HTTP/WS interface adapter（Phase 1 完成时 MUST NOT 再 import legacy `internal/task`）
  task/                   临时兼容 facade，最终只委托 application（Phase 1 门槛前调用方收缩至零，随后删除或仅留别名转发层）
cmd/ocdeck-server/        composition root
```

依赖方向固定：`api → application → domain`；`infrastructure → application ports + domain`；`cmd` 组装全部具体实现并完成 wiring；`domain` 只依赖 stdlib。迁移期允许的临时形态是 `api → task facade → application`，但这不是终态：TaskBackend 接口签名依赖的模型类型 MUST 随迁移一并迁到 application/domain 层（见 tasks P1.4），P1.9 以 import-graph 检查验收终态（`internal/api` 不出现 `internal/task` import，`internal/domain` 仅依赖 stdlib）。

**领域模型（实体与聚合边界，已与领域所有者确认）**：

| 对象 | 身份（主键） | 职责 | 不包含 |
|---|---|---|---|
| `Task`（聚合根） | `tasks.id` | 完整 status/init_status 状态机（domain guard 表达合法流转，application 负责 CAS 提交）、delete intent、typed notice 集合、创建期不可变信息 | `[]Session`、ServeRuntime、tmux/opencode handle、env 合并算法 |
| `Session`（独立聚合） | 领域 ID = `session_id` | OwnerTaskID、created/first_seen/last_seen、parentID、claim/touch/delete 规则 | opencode session 内容、run status、Task 指针 |
| `ServeRuntime`（内存实体） | `instanceID + generation` | RuntimeToken、runtime groups、run_status、attention | 持久化 Task 副本、Repository、跨重启恢复 |
| `AttentionState`（ServeRuntime 组件，非独立实体） | —（随 ServeRuntime） | permission/question 集合、capability 状态、owner/reconcile epoch、buffer | 独立 repository、独立生命周期 |

- **Session 为什么是独立聚合**：一次 align 可处理千级行、由 opencode SSE 独立驱动，且现有对齐要求 owned 快照/claim/touch/delete/notice 在同一事务内完成（`queries.go:972`）——这是 application 级批处理事务，不表示 Session 是 Task 内部实体。
- **ServeRuntime 由 application 持有**：`ServeRuntimeRegistry` 按 taskID 索引，实体身份为 `instanceID+generation`，不持有 `*Task`；与持久状态的协作始终通过「重读 Task/CAS → 外部副作用 → Runtime apply」完成。无 Repository。
- **AttentionState 不是值对象**（可变、有 epoch/owner 并发仲裁），作为 ServeRuntime 的组件；permission/question 快照项是值对象。现有 owner/buffer/atomic epoch 模型（`attention.go:55` 起）保持不简化。
- **session_id 唯一性口径（已核实物理现状）**：领域层声明 `session_id` 全局唯一；物理层 `task_sessions` 主键为 `(task_id, session_id)` 复合主键（`internal/store/migrations/0001_init.sql:62`），无全局唯一索引——跨 task 唯一由 `ClaimTaskSession` 事务内先查其他 owner 再 upsert 保证（`queries.go:903`）。本变更不加唯一索引；读到历史重复归属时 fail-closed。`SessionRepository` MUST NOT 暴露通用 Save/Upsert，方法闭合为 Claim/TouchOwned/DeleteOwned/Align/OwnedSessions/OwnerOf（见下）。

**Repository ports（consumer-owned，定义在 application 侧，非 domain）**：`TaskRepository`、`SessionRepository`、`ProjectReader`、`EnvReader`、`CleanupDebtRepository`，加外部端口 `ProcessPort`（tmux）、`OpenCodePort`、`WorktreePort`。MUST NOT 复制现有同时含项目/任务/env/session/读模型的超宽 `TaskStore`（它目前泄漏 `sql.NullString`，`manager.go:21`）。`internal/task/adapters.go` 当前由 task 侧主动 import store/process/worktree（`adapters.go:7`）——最终这些 adapter 移到 infrastructure；物理搬目录不是 Phase 1 门禁，消除反向依赖才是。

`SessionRepository` 方法闭合为（覆盖 D2 全部 session 提交点，MUST NOT 暴露通用 Save/Upsert）：

```go
type SessionRepository interface {
    Claim(ctx context.Context, s session.Session) (ClaimResult, error)              // 事务内先查他主再 upsert；changed=新插入或 last_seen_at/parent_id 实际推进
    TouchOwned(ctx context.Context, taskID string, id session.ID, lastSeen int64) (MutationResult, error) // 值变化条件（last_seen_at < ?）+ RowsAffected 判定
    DeleteOwned(ctx context.Context, taskID string, id session.ID) (affected int, err error)               // 返回受影响行数
    Align(ctx context.Context, taskID string, observed []session.Session, notice NoticeMutation) (AlignResult, error) // 单事务批处理（仅 complete 清除分支的 notice 随事务提交）；notice expected 失配 MUST 回滚整个事务并返回 typed conflict，见下；overflow 的 session_overflow 设置 MUST 在 Align 之前经事务外 CAS 完成（见下），Align 失败 MUST NOT 回滚该 notice
    OwnedSessions(ctx context.Context, taskID string) ([]session.Session, error)    // owned 集合查询（对账交集用）
    OwnerOf(ctx context.Context, id session.ID) (taskID string, found bool, err error) // status/diff 事件归属反查（fail-closed）
}
```

`Align` 是显式的 application 级一致性操作，不是 Repository 自治写：notice 的 expected/new 变更（`NoticeMutation`）MUST 先经 Task domain 决策（typed notice 规则）算出，再由 sqlite adapter 在同一事务内与 session 行变更一并提交；`AlignResult` 携带 session 行计数与 `TaskMutation`（notice 是否变化、`updated_at` 是否真实推进），供 commit helper 分别发布 `sessions.aligned` 与 `task.activity_changed`。**notice 并发冲突闭环**：expected 与事务内最新 notice 不匹配时 MUST 回滚整个 Align 事务（不提交任何 session 行变更）并返回 typed conflict；application MUST 重读 Task、重新经 Task domain 决策 notice、有界重试（沿用既有 notice CAS 重读重试语义，上限 8 次，`notice.go:459-476`）；MUST NOT 覆盖并发新增 notice，也 MUST NOT 提交 session 行变更却遗留错误的 overflow notice。**overflow 设置与 complete 清除的事务边界不同**（沿用 canonical opencode-orchestration spec 与 `activate.go:1239-1244` 现状）：overflow（`complete=false`）时 application MUST 先经事务外 CAS 写入 `session_overflow` notice（结构化 `MutationResult`），成功后才调用 Align；该 CAS 失败 MUST 返回错误且 MUST NOT 执行 Align；Align 后续失败 MUST NOT 回滚已写入的 notice。complete 时的 notice 清除才随 Align 单事务提交（expected 失配整体回滚）。overflow 设置成功且真实变更时按 D2 updated_at 行规则发布 `task.activity_changed`。

**结构化提交结果（application 层定义，非 domain、非 sqlite 包）**：

```go
type MutationResult struct { Matched, Changed, UpdatedAtAdvanced bool }
type TransitionResult struct { MutationResult; StatusChanged bool; From, To task.Status } // StatusChanged=true 时才填充 From/To
type ClaimResult struct { Claimed, Changed bool; OwnerTaskID string }
type AlignResult struct {
    Inserted, Touched, Deleted int
    SessionIDs []session.ID
    TaskMutation MutationResult
    Conflicts []session.ID
}
```

现有 SQL 的同值写也会刷新 `updated_at`（`queries.go:422`），SQLite `RowsAffected` 不能区分值是否实际改变（notice CAS 同此，`queries.go:582`）——因此 MUST 先完成「同值原子 no-op + old/new 结构化结果捕获」（见 D2 提交点矩阵），再接事件，不得用现有 bool 猜测变化。**同值判定 MUST 覆盖该 SQL 语句写入的全部业务列**：`UpdateTaskStatus*` 同时写 `status` 与 `last_error`（`queries.go:422-427`），状态相同但 `last_error` 不同仍是真实行变更——MUST 提交、MUST NOT 发 `task.status_changed`，并按 `UpdatedAtAdvanced` 发 `task.activity_changed`。状态写方法的结构化结果 MUST 能区分 status 是否实际迁移（`TransitionResult` 的 `StatusChanged`/`From`/`To`），commit helper 据此决定是否发 `task.status_changed`。

**Phase 1 唯一显式对外行为差异（已落入 spec delta，见 `specs/task-lifecycle/spec.md`）**：同值原子 no-op 使幂等写不再推进 `updated_at`——该字段语义从「最近一次写尝试」收紧为「最近一次真实变更」。canonical spec 对 `updated_at` 的全部断言均为读侧（projects 摘要字段、指挥中心排序），读侧语义保持成立；差异仅在写侧刷新时机。除此之外 Phase 1 REST/WS 对外行为不变。

**`tasks.status` 状态机矩阵（domain guard 的完整输入；P1.1 表驱动测试逐行对应）**：

| from | to | 触发用例与提交点 | domain guard | 提交方式 | 失败补偿 | 领域事件 |
|---|---|---|---|---|---|---|
| —（无行） | `creating` | Create 意图插入（repo `crud.go:92`；dir `crud.go:184`） | 前置检查通过（项目存在、名称/分支/baseRef 合法） | INSERT | 插入失败即返回，零副作用 | `task.created` |
| `creating` | `creation_failed` | Create 补偿（repo `crud.go:103/115/131`；dir `crud.go:196/209`）；启动收敛（`reconcile.go:208`） | 无（补偿性落账总是允许） | 无条件 UPDATE | —（已处失败终态） | `task.status_changed` |
| `creating` | `suspended` | `CommitCreated`（`crud.go:128/206`） | 预期状态=`creating` | CAS | CAS error → 补偿落 `creation_failed`（repo `crud.go:131`、dir `crud.go:209`，走上行 `creating→creation_failed`）；CAS 未命中（`crud.go:135/213`）→ 本提交事实无写入、不发布，其后独立补偿见下行 | `task.status_changed` |
| 任意并发状态（或行已不存在） | `creation_failed` | 初始 Create 的 `CommitCreated` 未命中后补偿（repo `crud.go:138`；dir `crud.go:215`；保持现有补偿行为，Phase 1 不改为保留并发状态） | —（补偿性落账总是允许） | 无条件 UPDATE（结构化结果 MUST 返回实际旧状态 + affected/changed） | — | 命中且 changed 且实际 `from != creation_failed` → 按**实际** `from→creation_failed` 发 `task.status_changed`；实际 `from == creation_failed` 且 `last_error` 变化 → 走同状态规则（提交、不发 `task.status_changed`、按 `UpdatedAtAdvanced` 发 `task.activity_changed`）；全列同值或未命中（行不存在/已被并发删除）→ 不发布 |
| `creation_failed` | `creation_failed` | Retry 前置阶段失败与 CommitCreated error 补偿重复落账（`crud.go:399/407/411/417/426/439/460/466/471/479/491`；Retry 全程不回写 `creating`；CommitCreated **未命中**（`crud.go:442/494`）保留并发状态、无写入，不产生任何事件） | — | 同值原子 no-op（比较 status+`last_error` 全列） | — | `last_error` 相同 → 不发布；`last_error` 不同 → 提交但不发 `task.status_changed`，按 `UpdatedAtAdvanced` 发 `task.activity_changed` |
| `creation_failed` | `suspended` | Retry 的 `CommitCreated`（`crud.go:436/488`） | 预期状态=`creation_failed` | CAS | error → 重复落 `creation_failed`（`439/491`，走上行同值规则）；未命中 → 保留并发状态、无写入（`442/494`） | `task.status_changed` |
| `suspended` | `activating` | Activate（`activate.go:284`） | `CanActivate`：`suspended` + 无阻断 notice + init_status 门禁（五分支见 canonical task-lifecycle spec） | CAS expected=`suspended` | 未命中 → 409 冲突 | `task.status_changed` |
| `activating` | `active` | Activate 提交（`activate.go:302`）；resume（`reconcile.go:321`） | —（编排内部提交） | 无条件 UPDATE | error → 补偿 `activating→suspended` | `task.status_changed` |
| `activating` | `suspended` | Activate 失败补偿（`activate.go:372` CAS）；reconcile suspend 收敛 CAS（`reconcile.go:116`，fromStatus=`activating`）；reconcile kill 分支（`reconcile.go:239`） | — | CAS expected=`activating` / 无条件 UPDATE | 补偿失败仅记录日志 | `task.status_changed` |
| `active` | `suspending` | Suspend（`suspend.go:42`） | `CanSuspend`：`active` | CAS expected=`active` | 未命中 → 409 | `task.status_changed` |
| `suspending` | `active` | Suspend 修复回迁（`suspend.go:115`） | —（编排内部） | CAS expected=`suspending` | best-effort，失败记录 | `task.status_changed` |
| `suspending` | `suspended` | Suspend 完成（`suspend.go:206`）；reconcile 收敛（`reconcile.go:193`，case `StatusSuspending`）；reconcile kill 分支（`reconcile.go:239`） | — | 无条件 UPDATE | error 记录，后台收敛 | `task.status_changed` |
| `active` | `suspended` | 异常收敛（`activate.go:1502` CAS）；reconcile suspend 收敛 CAS（`reconcile.go:116`，fromStatus=`active`）；reconcile kill 分支（`reconcile.go:239`） | — | CAS expected=`active` / 无条件 UPDATE | 见 D2 异常收敛嵌套决策表 | `task.status_changed` |
| `active` | `active` | resume（`reconcile.go:321`，原状态已为 `active`）；ReopenAttach（`attach_shell.go:89`） | — | 同值原子 no-op（比较 status+`last_error` 全列） | — | status 不迁移：`last_error` 相同 → no-op 不发布；`last_error` 不同 → 提交但不发 `task.status_changed`，按 `UpdatedAtAdvanced` 发 `task.activity_changed` |
| `suspended` | `archived` | Archive（`crud.go:522` → `ArchiveTask`） | `CanArchive`：`status=suspended` 且 `init_status∉{pending,running}`（init 进行中拒绝，`crud.go:518-521`） | 无条件 UPDATE | error 返回调用方 | `task.status_changed` |
| `archived` | `suspended` | Restore（`crud.go:543` → `RestoreTask`） | `CanRestore`：`archived` | 无条件 UPDATE | error 返回调用方 | `task.status_changed` |
| `suspended`/`archived`/`creation_failed`/`deletion_failed` | `deleting` | `BeginDeleteIntent`（`delete.go:93`） | `CanDelete(task, mode)`：Normal 仅允许 `suspended|archived|creation_failed`；Force 额外允许 `deletion_failed`（`deleteAllowedStatus` `delete.go:654-662`）；两者均拒绝 `init_status∈{pending,running}`（`delete.go:46-49`） | CAS fromStatuses 白名单 | 未命中 → 409 | `task.status_changed` |
| `deleting` | `deletion_failed` | 删除副作用失败落账（`delete.go:129/140/151/162/177/189/210/218/228/238/244/326/334`、`crud.go:347/352`） | —（补偿性落账总是允许） | 无条件 UPDATE（finalize 用非取消 ctx，`delete.go:148-152`） | — | `task.status_changed` |
| `deletion_failed` | `deleting` | Retry 重入删除（`crud.go:326-333`，先 `SetTaskDeleteMode` 再置 deleting） | 项目 kind 校验通过 | 无条件 UPDATE | error 返回 | `task.status_changed` |
| 任意 | （行删除） | `DeleteTask` 提交点（repo 路径 `delete.go:289`、dir 路径 `delete.go:366`；Force 路径事务内级联删除剩余 session 归属行，`0001_init.sql:57`） | 前置静态检查（`delete.go:67`）+ `deleting` 状态 | DELETE（`DeleteResult` 携带级联 session ID 集合） | — | 先逐个发级联 `session.deleted{task_id}`（本操作内已逐项发布的不重复发），再发 `task.deleted{from}` |

**`init_status` 状态机**（`none|pending|running|succeeded|failed`，`types.go:48-57`）：`CommitCreated` 按是否配置 init 脚本落 `pending`/`none`；`pending→running` 经 `ClaimInitRun`（`queries.go:695` 区域）；`running→succeeded|failed` 经 `FinishInitRun`（`queries.go:730` 区域）；`failed|succeeded→running` 经 `ClaimInitRerun`（`queries.go:712`，要求 `status=suspended`）；启动收敛 `ConvergeInterruptedInitRuns` 将 `pending|running` 落 `failed`（`queries.go:748` 区域）。init_status 改写 MUST 同样走同值原子 no-op 与结构化结果；本变更 MUST NOT 为 init_status 发明领域事件（见 D2）。跨字段门禁（如 Activate 对 init_status 的五分支放行/拒绝）以 canonical task-lifecycle spec 为准，domain guard 函数逐一对应。

**事件产生与发布责任**：domain 决定合法变化（状态机 guard），Repository 返回已提交的结构化事实，**application 在提交成功后经集中 commit helper 同步调用非阻塞 Publisher**——如 `commitTransition` / `commitTaskMutation` / `applyRuntimeChange`，MUST NOT 让每个流程散落 `Publish`，MUST NOT 用 Repository decorator 隐式发布。不做 aggregate pending-events 队列（本系统是专用 SQL CAS + 外部副作用模型，不是 Load→Save 模型；如 Activate 是 `suspended→activating`、运行副作用、再提交 active，`activate.go:283`，聚合提前记录事件无法判断 CAS 命中与补偿终态）。规则：DB 事实在事务/CAS 成功且 `Changed=true` 后发布；Runtime 事实在唯一 apply 返回 changed 后立即发布；commit 结果不确定只发 `resync.requested`；Publish 溢出不回滚业务提交（订阅方全量重建兜底）。`Payload any` 仅作 envelope，生产代码用 typed payload 与构造器；Bus 不做 schema 校验。

**迁移顺序（strangler，MUST 按序；保留单一 LifecycleService 与 ServeRuntimeRegistry，不拆成十几个各自持锁的小 service）**：

1. 冻结 HTTP/WS DTO、错误码、SQL 行为与关键副作用 trace；建立旧 Manager facade。
2. 引入 domain 类型、Repository ports 与结构化结果；调用路径不变。
3. 抽 `ServeRuntimeRegistry`，一次性迁移 generation/instanceID/groups/tombstone/clear 责任；MUST NOT 旧 Manager 与新 Registry 双写。
4. 迁移低风险 `Get/List/Archive/Restore`，验证状态 guard、CAS 与事件 commit helper。
5. 迁移 Session claim/touch/delete/align 与 Attention apply；align 保持单事务，不拆成循环调用（RunStatus 内存态为新增能力，整体在 P1.8 建立，本步不涉及）。
6. 迁移 Create/Retry，然后 Activate；保持创建的「前置检查 → creating → 副作用 → CommitCreated → 锁外调度」顺序（`crud.go:91`）。
7. 迁移 Suspend、异常 converge 与两阶段 `preCleanup/postCleanup` 债务（并发风险最高）。
8. 最后迁移 Delete、Reconcile、background、Shutdown；Delete 静态检查必须继续先于删除意图与破坏性副作用（`delete.go:67`）。

**迁移期间必须锁定的不变量**：

- `api.TaskBackend` 契约、DTO 字段与 `task.OpError` 映射不变（`tasks.go:15`）。
- 全系统只有一个 task keyed-lock owner；状态写尽量全部改为 expected-state CAS。
- watcher/SSE callback 始终携带 generation+sessionName+instanceID；MUST NOT 像现状 watcher 预校验后丢掉 token 再进入无 token converge（`activate.go:1366`）。
- 现状锁超时做无锁 cleanup + CAS（`activate.go:1457`）是**必须被替换的行为**，Phase 1 换成令牌校验后的两阶段债务（见 D2 矩阵），不得当作需保持的现状。
- Attention 的 owner/reconcile epoch、锁外 REST、buffer 重放语义保持。
- Session claim 唯一归属、dir owned-only、overflow 基于原始列表、align 单事务保持。
- detached compensation context、runner/shutdown gate、WG join 顺序保持。
- canonical specs 是验收合同；有意改变的同值写语义必须先落入本 change 的 spec delta。
- **决策先于副作用**：每个用例阶段在首个不可逆外部副作用（tmux/opencode 调用、消息发送）之前完成该阶段全部 domain 决策；guard 拒绝叶节点 MUST 零副作用；依赖外部调用成功的事实（如同步锚点、运行时状态）仅在外部成功后才推进；关键生命周期流程用 recording fake 的调用 trace 断言副作用顺序（含 Times(0) 级别的「不得发生」断言）。

**Phase 1 最小可用闭环**：`HTTP/后台命令 → application → domain guard → repository commit / runtime apply → 结构化结果 → Publisher → Bus → 测试/诊断订阅者 → 订阅者重新查询得到与事件一致的全量状态`。Phase 1 完成门槛：Task/Session/ServeRuntime 三个模型落位；**所有**影响事件目录的生产写点（含 session/attention/run_status/activity 与异常收敛债务）都经过 application commit helper 并真实发布；结构化结果、总线、Runtime 快照、两阶段 converge 债务与 race 测试完成；**Phase 0 实测门禁完成且 agentStatus 事件驱动建模（D4 生产侧：唯一 apply、连接代阶段机、断流回调契约）落地**；现有 REST/WS 与前端 5s 轮询保持不变（唯一显式差异为同值写不再推进 `updated_at`，见上）。GitOps、日志读取、项目/env 等非本 bounded context 代码无需为「全量 DDD」重写。**Phase 2 仅做消费侧适配**：共享快照组装、SSE adapter（含消费过滤）、REST 改读 run_status 快照、`statusRecorder` Flush/BaseContext 与前端 fetch stream——Phase 2 不新增任何事件生产点。

**反模式约束（本体量明确不用）**：通用 `Repository[T]`/`BaseEntity`/反射 mapper/DI 框架；全局 UnitOfWork（操作级事务足够，`AlignTaskSessions` 保持专用事务）；Event Sourcing/CQRS 框架/Outbox/NATS/Watermill（事件是进程内 at-most-once）；Saga 框架（Create/Activate/Delete 是显式 process manager 补偿流程）；每个动作一个 Service struct；默认化的 DomainService（仅跨聚合且无自然归属的纯规则才用；Session claim 并发唯一性是 Repository 事务职责）；Factory 类（`task.New(...)`/`runtime.New(...)` 保证构造不变量即可）；Specification 模式（状态门禁用 `CanActivate`/`CanDelete`/`CanArchive` 普通函数即可）。

### D1: 事件总线形态——进程内 topic 化 pub/sub（`internal/infrastructure/eventbus`）

新包 `internal/infrastructure/eventbus`（领域事件类型定义在 `internal/domain/event`，Bus 实现属 infrastructure，见 D0 布局），核心接口：

```go
type Event struct {
    Topic  string      // 领域 topic："task" | "session" | "serve_runtime" | "control"
    Type   string      // 具名领域事件，见「事件类型目录」
    RID    string      // 主体实体自己的主键 ID：task 事件=task 主键；session 单条事件=session 主键（session 是独立实体）；serve_runtime 事件=ServeRuntime 主键 instanceID（Payload 携带 task_id）；sessions.aligned=task 主键（主体为任务的会话集合，持久侧无独立对象）；仅 resync.requested 无主体允许空
    Payload any        // 按 Type 的小载荷，见目录；MUST NOT 塞整表/整实体快照；RID 承载主体主键不重复入 Payload；Payload 只带关联实体 ID（session 单条与 serve_runtime 事件含 owning task_id；sessions.aligned 含受影响 session_ids）
}
// resync.requested 为控制事件：不表达任何域事实，仅要求订阅方重新拉取其场景全量；
// 允许范围仅三处（见 D2 异常收敛矩阵）——(a) 提交结果不确定的异常收敛路径、
// (b) worker 撤销登记前的重同步（仅提交结果仍不确定的叶节点 W②b；committed=false 已确定的 W③b 撤销登记 MUST NOT 发布）、(c) 锁等待超时且触发令牌仍有效的债务登记（含 tombstone 匹配直登 postCleanup）；
// 是"失败/无变化不发布领域事件"规则的唯一例外。

type Bus struct { /* ... */ }
func (b *Bus) Publish(ev Event)            // 非阻塞：订阅者缓冲满则丢弃该事件并置位其溢出信号、记日志
func (b *Bus) Subscribe(topic string) *Sub // 按单个 topic 订阅；多 topic 由调用方多次 Subscribe 后 fan-in

type Sub struct { /* ... */ }
func (s *Sub) C() <-chan Event
func (s *Sub) Overflow() <-chan struct{} // 缓冲溢出时被非阻塞置位（至少一次可见），供订阅方自愈
func (s *Sub) Close()
```

- **为什么自建而非引入库**：需求是单进程、单二进制、无持久化的 fan-out 通知，标准库 channel + RWMutex 订阅表足够（约百行），引入 NATS/Watermill 类依赖与项目"stdlib 优先"的依赖策略（go.mod 5 个直接第三方依赖）不符。
- **非阻塞 Publish + 可观察溢出**：发布者（`handleSSEEvent`、生命周期操作）持任务锁或在热路径上，绝不允许被慢订阅者阻塞。缓冲（如 64）满时丢弃该事件、记 warn 日志、并非阻塞置位 `Overflow()` 信号——订阅方（SSE handler）据此立即触发一次全量重推自愈（见 D3），丢事件不破坏正确性。溢出信号是可合并的：多次溢出只需至少一次可见。
- **并发语义**：Publish 与 Subscribe/Close 并发安全；Close 后不再收到事件；需 race 测试锁定。
- **备选方案**：① task 层直接持有 SSE handler 注册表回调——层间反向依赖，违反现有 api→task 单向依赖；② 定时快照 diff 推流——浪费且延迟不可控。均否决。

### 事件类型目录（两层各自闭合，内容不同）

内部总线发布的是**领域事件**（task 已创建、状态已变更、session 已认领……），按领域 topic 投递，**不按场景裁剪**。SSE 是指挥中心场景适配器：订阅领域总线后按消费过滤表决定是否把本场景快照标脏，再推自己的 `snapshot`/`update` 裸数组。两层 schema MUST NOT 混用。

#### 1. 内部领域事件（进程内，不离开服务端）

```go
type Event struct {
    Topic   string // "task" | "session" | "serve_runtime" | "control"
    Type    string // 具名事件，见下表
    RID     string // 主体实体自己的主键 ID（无主体的 resync.requested 允许空）
    Payload any    // 小载荷；未知 Type 仍投递，不得使 Publish 失败
}
```

| Type | Topic | RID | 含义 | Payload | 何时发布（摘要，细则见 D2） | 不发布 |
|---|---|---|---|---|---|---|
| `task.created` | `task` | task 主键 | 任务行已创建（`creating` 状态入库） | `{}` | `CreateTask` 插入可见行成功（repo `crud.go:92`；dir `crud.go:184`） | 插入失败 |
| `task.status_changed` | `task` | task 主键 | 任务状态机发生真实迁移（`from→to` 已落账） | `{from, to}` 均为 `tasks.status` 枚举值（`creating`/`creation_failed`/`activating`/`active`/`suspending`/`suspended`/`archived`/`deleting`/`deletion_failed`，见 `internal/task/types.go:37-45`） | 任一 `UpdateTaskStatus*` / CAS 真实改写 `tasks.status`（含 `activating`/`suspending` 过渡、`CommitCreated` 的 `creating\|creation_failed→suspended`、Archive/Restore 的 `suspended↔archived`、异常收敛 `active→suspended`） | 同值 no-op、CAS 未命中、存储失败 |
| `task.deleted` | `task` | task 主键 | 任务行已删除 | `{from}` 被删行原状态 | `DeleteTask` affected>0 | 未命中、门禁拒绝 |
| `task.activity_changed` | `task` | task 主键 | 任务可见字段（env/notice/last_port/delete_mode/同状态 `last_error` 等）真实变更且活动水位 `updated_at` 跨秒推进 | `{}` | 闭合字段集合（`env_snapshot`/`notice`/`last_port`/`delete_mode`/同状态 `last_error`/align 事务内 notice）的提交 `Changed && UpdatedAtAdvanced` **且该提交未产生 `task.status_changed`**——真实状态迁移的 `updated_at` 推进由 `task.status_changed` 承载，MUST NOT 同事务重复发布；**`init_status`/`init_error` 写入即使推进 `updated_at` 也 MUST NOT 触发本事件**（本变更不为 init 系列发布领域事件） | 同值 no-op、同秒、未命中、伴随 `task.status_changed` 的提交、init 系列写入 |
| `session.claimed` | `session` | session 主键 | 任务认领了 opencode 会话归属（或归属信息被推进） | `{task_id}`（owning task） | `ClaimTaskSession` `changed=true`（新插入或 `last_seen_at`/`parent_id` 推进） | 幂等无变化、冲突持有 |
| `session.touched` | `session` | session 主键 | owned 会话的最近活跃时间被推进 | `{task_id}` | `TouchOwnedTaskSession` 真实推进 `last_seen_at` | 值不变、未命中归属 |
| `session.deleted` | `session` | session 主键 | 一条 owned 会话归属被移除 | `{task_id}` | `DeleteTaskSession` affected>0；或 Force 删除时 `DeleteTask` 事务内级联移除（逐个发布，先于 `task.deleted`） | 删除不存在行；本操作内已逐项发布过的 session MUST NOT 重复发布 |
| `sessions.aligned` | `session` | task 主键（主体为任务的会话集合，task 的衍生产物） | 全量对账使会话归属行或活动水位发生真实变化 | `{inserted, touched, deleted int; session_ids []string}`（受影响会话 ID，由对账事务统计得出） | `alignSessions` 的 session 行计数（inserted+touched+deleted）总和 >0；对账事务内 notice/`updated_at` 的真实推进 MUST NOT 计入本事件，另发 `task.activity_changed`（见 D2 align 事务内写入行） | 全量无变化 |
| `serve_runtime.attention_changed` | `serve_runtime` | ServeRuntime 主键 instanceID（主体是 ServeRuntime 的注意力组件） | 外部可见的注意力快照（permissions/questions）发生变化 | `{task_id}` | 增量 apply / 接管归并 / REST 写回相对基线再变化 | canceled/epoch 失配仅为 REST 写回；无变化 |
| `serve_runtime.run_status_changed` | `serve_runtime` | ServeRuntime 主键 instanceID | 该 serve 实例的运行状态（opencode session 三态聚合，即对外 `agentStatus` 字段）或其可用性发生变化 | `{task_id, from, to, available}`：`from`/`to` 为聚合三态 `idle`/`busy`/`retry` 或 `""`（不可用，见 `internal/opencode/client.go:50-52`）；`available` 为变化后外部是否可用 | 唯一 apply 返回 changed（含 0↔1 owned、断流 available→unavailable） | 同值、原本不可用再断流、孤儿 status 事件 |
| `resync.requested` | `control` | 空（无主体） | 控制事件：要求订阅方重拉其场景全量；不表达任何域事实 | `{}` | 仅 D2 三处闭合例外：(a) 不确定提交 (b) worker 撤销登记前 (c) 锁等待超时且触发令牌仍有效的债务登记（含 tombstone 匹配直登 postCleanup）；每条例外路径至多一次 | 确定的成功/失败域路径；不得用它表达 session/attention/agent 事实 |

`RID` 一律为**主体实体自己的主键 ID**：task 事件（`task.created`/`task.status_changed`/`task.deleted`/`task.activity_changed`）为 task 主键；`session.claimed`/`session.touched`/`session.deleted` 为 session 主键（session 是独立聚合，owning task 转入 Payload `task_id`）；`serve_runtime.attention_changed`/`serve_runtime.run_status_changed` 为 ServeRuntime 主键 instanceID（owning task 转入 Payload `task_id`）；`sessions.aligned` 的主体是任务的会话集合（持久侧无独立对象），RID 为 task 主键；`resync.requested` 无主体允许空。Payload 只携带关联实体 ID 与已落账事实（`from`/`to`/`available`），不含 ActiveSessionItem 整行、不含 Attention 明细、不含 overview 数组。总线 MUST NOT 再引入无 Type 的粗 `Kind`。

**SSE 场景适配器消费过滤**（仅本变更接入的订阅者；其它场景以后另写）：

| 领域事件 | 指挥中心 SSE 是否标脏 |
|---|---|
| `session.claimed` / `session.touched` / `session.deleted` / `sessions.aligned` | 是 |
| `serve_runtime.attention_changed` | 是 |
| `serve_runtime.run_status_changed` | 是 |
| `resync.requested` | 是（强制重拉本场景全量） |
| `task.status_changed` 且 `(from==active) != (to==active)` | 是 |
| `task.deleted` 且 `from==active` | 是（按现有 `delete.go:93` 门禁，生产路径通常不出现） |
| `task.activity_changed` | 是（`last_active_at` 无 session 时回退 `updated_at`；多余重建可接受） |
| `task.created` | **否**（只改 projects 树） |
| `task.status_changed` 且两端都非 active（含 `suspended→activating`、`activating→suspended`、`suspending→suspended`、`CommitCreated`、Archive/Restore） | **否** |
| `task.deleted` 且 `from!=active` | **否** |
| 未知 `Type` | 是（保守标脏，避免漏推） |

适配器收到上表「是」或 `Overflow()` 后只置 dirty，按 D3 重推全量 `ActiveSessionItem[]`。MUST NOT 按领域 Payload 做增量合并，MUST NOT 把 `Type` 写成 SSE `event:`。

#### 2. 对外 SSE 帧（指挥中心场景，`GET /api/v1/sessions/active/stream`）

仅下列三种写出路径；`event:` 名闭合。data 与 REST `GET /api/v1/sessions/active` 响应体完全同构。

```
event: snapshot
data: <ActiveSessionItem 裸数组>

event: update
data: <ActiveSessionItem 裸数组>

: ping
```

| SSE `event` | data | 何时发出 | 客户端动作 |
|---|---|---|---|
| `snapshot` | JSON 裸数组（无外层对象） | 建连组装成功后的首帧；空列表为 `[]` 非 `null` | 整表替换 sessions 快照 |
| `update` | 同上，同构裸数组 | 合并窗口到期 / 建连 dirty 补帧 / Overflow 窗口外重推 | 整表替换 sessions 快照 |
| （注释行） | 无 data；原文 `: ping` | 无业务帧超过 25s | 忽略，保持连接 |

**禁止的对外事件**：任何内部 `Type`（`task.created`、`session.claimed`、`resync.requested` 等）均不得作为 SSE `event:` 名；不得把领域 Payload 原样外发；不得发增量 diff、单任务补丁、error 事件帧（鉴权失败走 HTTP 401，初始组装失败走 HTTP 500 信封，均不进入 SSE）。

`ActiveSessionItem`（数组元素，字段与 `internal/api/tasks.go` `activeSessionDTO` / `web/src/types.ts` `ActiveSessionItem` 对齐）：

```json
{
  "task_id": "tsk_...",
  "project_id": "prj_...",
  "project_name": "demo",
  "name": "fix login",
  "branch": "feat/x",
  "worktree_path": "/path/to/wt",
  "last_active_at": 1710000000,
  "agentStatus": "idle",
  "attention": {
    "permissions": [
      { "id": "p1", "permission": "edit", "patterns": ["src/**"], "since": 1710000000 }
    ],
    "questions": [
      {
        "id": "q1",
        "questions": [{ "header": "Confirm", "question": "Overwrite?" }],
        "since": 1710000001
      }
    ]
  }
}
```

| 字段 | 类型 | 约束 |
|---|---|---|
| `task_id` | string | 必有 |
| `project_id` | string | 必有 |
| `project_name` | string | 必有 |
| `name` | string | 必有 |
| `branch` | string | 必有 |
| `worktree_path` | string | 必有 |
| `last_active_at` | number | Unix **秒**；session `last_seen_at` 的 MAX，无 session 行回退 `tasks.updated_at` |
| `agentStatus` | `"idle"` \| `"busy"` \| `"retry"` | 不可用或零 owned 时 **省略**（JSON omitempty），不得输出空串或 `idle` 占位 |
| `attention` | object | 必有；无 pending 时两数组为 `[]` 非 `null` |
| `attention.permissions[]` | `{id, permission, patterns, since}` | `since` 为 Unix 秒；`patterns` 为 string[] |
| `attention.questions[]` | `{id, questions[{header, question}], since}` | `since` 为 Unix 秒 |

#### 3. 两层对照

```
task 提交点 ──Publish──► Event{Topic 领域, Type, RID, Payload}
                              │
                              ▼
              SSE 适配器按消费过滤表决定是否标脏
              （CreateTask / 非 active 迁移被忽略）
                              │
                    500ms 窗口 / 首帧 / Overflow
                              ▼
              snapshot | update  （裸数组 ActiveSessionItem[]）
                              │
                              ▼
                     前端整表替换 sessions 快照
```

### D2: 发布点——以"真实状态变更提交"为准（提交点矩阵）

统一规则：**领域事件发布的前提是状态真实变更且已落账/应用**；校验拒绝、存储失败、CAS 未命中（`committed=false`）、无实际变化的 apply MUST NOT 产生领域事件。`resync.requested` 不受此前提约束（不表达域事实），仅允许用于 D2 异常收敛矩阵规定的 (a) 提交结果不确定路径、(b) worker 撤销登记前重同步（仅叶节点 W②b；committed=false 已确定的 W③b 撤销登记 MUST NOT 发布）、(c) 锁等待超时且触发令牌仍有效的债务登记（含 tombstone 匹配直登 `postCleanup`），且每条不确定路径至多一次。发布点不按公开方法返回粗粒度判断，而按下列提交点矩阵逐点挂接。**总线按领域发布**：`CreateTask` 发 `task.created`，任意真实 `status` 改写发 `task.status_changed{from,to}`，真实删除发 `task.deleted{from}`——不再用「是否改变 active overview」裁剪发布。指挥中心 SSE 用消费过滤表决定是否标脏。

| 提交点 | 位置 | 发布条件 | Type |
|---|---|---|---|
| session claim（全部生产点） | `handleSSEEvent`（`activate.go:1158`）；**`resolveAnchorSession`（`activate.go:1354`，经 `ReopenAttach`/`attach_shell.go:83` 与激活锚定路径直接 claim，可先于 SSE 事件落账）** | 仅真实变更：当前 `ClaimTaskSession` 对本任务已归属行的幂等 upsert 也返回 `claimed=true`（`queries.go:909,941-952`），本变更须让其额外返回 `changed`（新插入或 `last_seen_at`/`parent_id` 实际推进），各 Manager 编排点仅 `changed=true` 发布；冲突（他任务持有）不发布 | `session.claimed` |
| session touch | `handleSSEEvent`（`activate.go:1143`） | 仅值变化：当前 `TouchOwnedTaskSession` 用 `MAX(?, last_seen_at)` 更新，命中归属行即 `updated=true`，值不变时同样返回 true（`queries.go:957-969`），本变更须改为值变化条件（如 `WHERE ... AND last_seen_at < ?`，以 RowsAffected 判定 changed），仅真实推进发布 | `session.touched` |
| session delete（全部生产点） | `handleSSEEvent`（`activate.go:1178`）；**`Delete` 流程直接删归属行（`delete.go:469`）**；**Force 删除的级联移除（`delete.go:323-326` 跳过逐项删除，`delete.go:289/366` `DeleteTask` 事务内级联删全部剩余归属行，`0001_init.sql:57`）** | 仅真实删除行：当前 `DeleteTaskSession` 只返回 error、删除不存在行同样成功（`queries.go:575-580`），本变更须让其返回受影响行数，各 Manager 编排点仅 affected>0 发布；Force 路径由 `DeleteTask` 结构化结果携带级联移除的 session ID 集合（见「任务删除」行），提交成功后逐个发布 | `session.deleted` |
| attention 事件变更 | `rt.applyAttentionEvent`（`activate.go:1186`） | apply 返回 `changed=true`（需让 apply 显式返回是否变化；无变化不发布） | `serve_runtime.attention_changed` |
| attention 对账变更（全部对账点） | align 对账（`attention.go:606/610`，`reconcilePermAlign`/`reconcileQuestAlign`）与 degraded 后台对账（`attention.go:659/666`，`reconcilePermBackground`/`reconcileQuestBackground`） | **两个独立 accepted apply，各自按「该 apply 前后完整外部可见 Attention 快照是否变化」判定**：① **接管归并**（成为新 owner 时把旧缓冲写入旧集合，`attention.go:285-289/328-333/465-469`）是独立原子 apply——归并当时即可改变外部快照，MUST 立即计算 `changed` 并在锁外发布；随后 REST 无论 200/404/degraded/canceled/被新 owner 抢占，都不得回滚或抑制这次发布。② **REST 写回**（写回校验通过：仍是 owner 且 `attentionEpoch` 未变）从归并后的基线独立判断：200 替换+缓冲重放（`attention.go:317/375/454/506`）、404→`unsupported` 清空（`attention.go:357-363/491-496`）、非 404→`degraded` 缓冲重放（`attention.go:364-371/497-502`）仅在相对归并后基线又变化时再发布一次。canceled（仍是 owner，`attention.go:295-303`）与 epoch 失配/被抢占 MUST NOT 为 REST 写回再发布；它们也不取消①已经发布的接管事件 | `serve_runtime.attention_changed` |
| agent 状态变更 | 新增 runtime apply 方法（D4 唯一写入口） | 聚合后任务级状态或可用性真实变化；Payload=`{task_id,from,to,available}`（`task_id` 为 owning task 主键，`from`/`to` 为聚合三态或 `""` 表不可用，`available` 为变化后外部可用性） | `serve_runtime.run_status_changed` |
| align 全量对齐 | `alignSessions` 成功返回处（`activate.go:1234`） | 仅 session 行真实变更：当前 `DeleteAbsentSessions` 不返回受影响行数（`queries.go:759-776`）、`AlignTaskSessions` 事务内 notice/updated_at 写入无计数（`queries.go:1043-1053`），本变更须让 align 返回结构化提交结果（实际插入数、时间推进数、删除行数、notice 是否变化、`updated_at` 是否真实推进）；**`sessions.aligned` 仅以 session 行计数（inserted+touched+deleted）总和 >0 判定**；notice/`updated_at` 真实推进不计入本事件，另发 `task.activity_changed`；**overflow（complete=false）时 `session_overflow` notice MUST 在 Align 之前经事务外 CAS 写入（失败则 Align 不执行；Align 失败不回滚该 notice），其真实变更按 updated_at 行规则另发 `task.activity_changed`** | `sessions.aligned` |
| 任务创建 | `CreateTask`（repo `crud.go:92`；dir `crud.go:184`） | 插入可见行成功 | `task.created` |
| 任务状态改写（**全部**真实 `tasks.status` 提交，穷举如下） | Create 补偿（repo `crud.go:103/115/131`；dir `crud.go:196/209`，`creating→creation_failed`）；初始 Create 的 `CommitCreated` 未命中后补偿（repo `crud.go:138`；dir `crud.go:215`）按实际旧状态发布，见下行；Retry 补偿（`crud.go:399/407/411/417/426/439/460/466/471/479/491`，`creation_failed→creation_failed`——Retry 全程不回写 `creating`，仅 `CommitCreated` 成功才离开 `creation_failed`）；启动收敛 `reconcile.go:208`（`creating→creation_failed`）；提交点 `CommitCreated`：`crud.go:128/206/436/488`（`creating\|creation_failed→suspended`，`queries.go:673-682`）；Activate：`activate.go:284`（`suspended→activating` CAS）、`:302`（`activating→active`）、`:372`（`activating→suspended` 补偿 CAS）；Suspend：`suspend.go:42`（`active→suspending` CAS）、`:115`（`suspending→active` 修复回迁 CAS）、`:206`（`suspending→suspended`）；Reconcile：`reconcile.go:116`（CAS，fromStatus∈{`active`,`activating`}→`suspended`）、`:193`（case `suspending→suspended` 无条件写）、`:208`（`creating→creation_failed`）、`:239`（kill 分支 `active\|activating\|suspending→suspended`）、`:321`（resume `activating→active`；原状态已为 `active` 时同值不发）；Archive/Restore：`crud.go:522/543`（`ArchiveTask`/`RestoreTask`，`suspended↔archived`，`queries.go:460/468`）；删除意图：`delete.go:93` `BeginDeleteIntent`（CAS →`deleting`）；删除失败落账：`delete.go:129/140/151/162/177/189/210/218/228/238/244/326/334` 与 `crud.go:347/352`（`deleting→deletion_failed`）；Retry 重入：`crud.go:331`（`deletion_failed→deleting`）；异常收敛：`activate.go:1502`（`active→suspended` CAS；锁超时无锁 CAS `:1466` 被本变更替换删除）；原状态已为 `active` 的 resume（`reconcile.go:321`）与 ReopenAttach（`attach_shell.go:89`）为 active→active 写：status 不迁移，MUST NOT 发 `task.status_changed`；全业务列相同才 no-op，`last_error` 不同则提交并按 `UpdatedAtAdvanced` 发 `task.activity_changed`（见下行 updated_at 规则）。初始 Create 的 `CommitCreated` 未命中（`crud.go:135/213`）本提交事实无写入不发布；其后独立补偿（`crud.go:138/215`）命中并发状态 X 且 X != `creation_failed` 时按实际 `X→creation_failed` 发布、X == `creation_failed` 且 `last_error` 不同走同状态规则（提交、不发 `task.status_changed`、按 `UpdatedAtAdvanced` 发 `task.activity_changed`）、X == `creation_failed` 全列同值或行不存在则不发布 | 仅真实迁移且 `changed=true`；Payload=`{from,to}`。同值改写（如 `creation_failed→creation_failed` 且 `last_error` 相同的重复落账）经同值原子 no-op 后 MUST NOT 发布；status 相同但 `last_error` 不同仍是真实变更——MUST 提交、不发 `task.status_changed`、按 `UpdatedAtAdvanced` 发 `task.activity_changed`。**领域层一律发布**，不再按 overview 裁剪。指挥中心 SSE 仅当 `(from==active)!=(to==active)` 时标脏（见消费过滤表） | `task.status_changed` |
| 任务删除 | `DeleteTask`（本变更须返回 `DeleteResult{Affected, From, CascadedSessionIDs}`：受影响行数 + 被删行原状态 + 事务内级联移除的剩余 session ID 集合，sqlite adapter 在同一事务内先捕获再删除） | affected>0；先逐个发布级联移除的 `session.deleted{task_id}`（本操作内已经 `DeleteTaskSession` 逐项删除并发布的 session MUST NOT 重复发布——Normal 路径逐项删除失败即落 `deletion_failed` 不会走到 `DeleteTask`，故级联集合只含未逐项处理的残余行；Force 路径跳过逐项删除，级联集合即全部剩余归属行），再发 `task.deleted{from}`。SSE 仅 `from==active` 标脏；按 `delete.go:93` 门禁普通 Delete 的 `from` 为 non-active | `task.deleted` + 级联 `session.deleted` |
| updated_at 推进类写入 | Manager 编排层中调用 `UpdateTaskEnvSnapshot`/`UpdateTaskLastPort`/`UpdateTaskNotice*`/`SetTaskDeleteMode` 的成功点（均推进 `tasks.updated_at`，`queries.go:430-456`），及 `ReopenAttach` 的 active→active `UpdateTaskStatus`（`attach_shell.go:87-90`，若 status 未变则只走本行） | **同值写矛盾的唯一解**：当前这些 SQL 无论字段值是否变化都无条件推进 `updated_at`（SQLite RowsAffected 按 WHERE 匹配计数而非值变化），跨秒同值写会真实改变 overview 排序字段——本变更 MUST 将这些 store 更新统一改为**字段同值时原子 no-op**（SQL `WHERE` 排除同值，SQLite `IS NOT` 做 NULL 安全比较），并返回结构化结果 `MutationResult{Matched, Changed, UpdatedAtAdvanced}`：`Matched` 供 CAS 调用方收敛判定（现有 `UpdateTaskNoticeCAS` 的单一 bool 被 `notice.go:196-205,228-250` 当作竞争失败重试，同值幂等成功不得被误判为不匹配）；领域发布条件 = `Changed && UpdatedAtAdvanced`。同值、同秒、任务不存在 MUST NOT 发布。**排他规则：同一提交若已产生 `task.status_changed`（status 真实迁移，含 `last_error` 随迁移同写），MUST NOT 另发 `task.activity_changed`——迁移的 `updated_at` 推进由 `task.status_changed` 承载；本事件仅用于未伴随状态迁移的真实字段变更**（如 env/notice/last_port/delete_mode 写入、`active→active` 同状态但 `last_error` 不同的提交）。指挥中心 SSE 对所有 `task.activity_changed` 标脏（无 session 时 `last_active_at` 回退该字段；有 session 时重建是多余但正确） | `task.activity_changed` |
| align 事务内 notice/updated_at 写入 | `AlignTaskSessions` 事务内 noticeFn 分支直接 `UPDATE tasks SET notice, updated_at`（`queries.go:1043-1053`） | 与上行同一 no-op 规则：notice 相同时 MUST 原子跳过（不推进 `updated_at`）；本变更须让 `AlignTaskSessions` 返回结构化提交结果（session 插入/时间推进/删除计数 + notice 是否变化 + `updated_at` 是否真实推进）。session 行计数 >0 → `sessions.aligned`；notice/`updated_at` 真实推进另发 `task.activity_changed` | `sessions.aligned` 和/或 `task.activity_changed` |
| 异常收敛迁移（持锁主路径） | `convergeToSuspended` 持锁主路径（`activate.go:1502`） | 仅 `committed=true`（并发已转走/未命中不发布）；Payload=`{from:"active", to:"suspended"}` | `task.status_changed` |
| 异常收敛的清理先于提交（持锁主路径） | `convergeToSuspended` 先 `cleanupActivationRuntime`（`activate.go:1488`，经 `clearRuntime` 清 attention 并停 SSE/runtime，`manager.go:511-522`）后 CAS（`activate.go:1502-1508`） | **结构化结果矩阵（嵌套决策表，叶节点唯一）**：清理前捕获债务令牌与外部可见状态（当前 agentStatus/attention 快照）。第一层按 CAS 结果分叉：①`committed=true` → 发布 `task.status_changed{from:active,to:suspended}`，不登记；②CAS error → 保守发布实际已发生的可见失效（`serve_runtime.run_status_changed`/`serve_runtime.attention_changed`），然后按状态重读结果分叉：②a 重读仍 active → 发布一次 `resync.requested` 并登记 **`postCleanup` 债务**；②b 重读为非 active/不存在 → 仅发布一次 `resync.requested`，不登记；②c 重读 error → 发布一次 `resync.requested` 并登记 **`postCleanup` 债务**；③`committed=false` → 按重读结果分叉：③a 仍 active → 发布可见失效并登记 **`postCleanup` 债务**；③b 非 active/不存在 → 不发布不登记（该并发转换由其对应提交点发布）；③c 重读 error → 同 ②c 处理（保守登记 `postCleanup` + resync.requested）。worker 语义见下行「债务两阶段」。重试注册表语义：**注册项携带触发时的 runtime `generation`/`instanceID` 作债务令牌，并携带阶段 `preCleanup` \| `postCleanup`**；Manager 侧维护**最新 runtime 令牌 tombstone**（创建 runtime 时更新、清理后保留；`lastGen` 已有持久代先例，`manager.go:204-208`/`notice.go:68-75`）；注册表由明确 mutex 保护；登记按 taskID 去重（新代登记原子替换旧注册项）；执行入口接入既有 `backgroundLoop`（`manager.go:603-622`）新增 tick 分支；任务离开 active 时同步撤销登记；移除 MUST 为 compare-and-delete（防旧 worker 误删新代登记） | `serve_runtime.run_status_changed` / `serve_runtime.attention_changed` / `task.status_changed` / `resync.requested` |
| 锁等待超时（禁止无锁破坏性清理） | 当前 `lockTaskForConverge` 失败后无锁 `cleanupActivationRuntime` + best-effort CAS（`activate.go:1458-1467`）；serve_exit watcher 回调预校验后丢弃触发令牌（`activate.go:1375-1391` → `handleServeExit:1438-1440` 无令牌入口） | **令牌贯穿**：所有异常收敛入口（serve_exit watcher、`handleInfraError`、SSE `convergeToSuspendedForGen`）MUST 统一携带触发 `RuntimeToken{generation, instanceID}`，watcher 路径不得再经无令牌入口（消除等锁期间换代后误清新代）。**本变更 MUST 取消无锁破坏性清理**：锁等待超时 MUST NOT 再调用 `cleanupActivationRuntime` 或无锁 CAS。超时路径仅允许：(1) 以**触发令牌**做登记前过期判定——当前 runtime 令牌等于触发令牌 → 捕获外部可见快照并继续 (2)-(4)；当前 runtime 为 nil 且 tombstone 等于触发令牌 → cleanup 已发生，跳过 (2) 直接登记 **`postCleanup`** 并发布一次 `resync.requested`；两者均不匹配 → 视为旧代 stale callback，记录日志并退出，MUST NOT 登记/清理。超时路径 MUST NOT 读取等锁结束时刻的当前 runtime 令牌代替触发令牌；(2) 若快照当前可用则经唯一 apply 发布一次可见失效（`serve_runtime.run_status_changed`/`serve_runtime.attention_changed`，无可见字段则不发领域事件）；(3) 发布一次 `resync.requested`；(4) 登记 **`preCleanup` 债务**（令牌 = 触发令牌，与持锁矩阵同一注册表/tombstone）。**原子登记**：过期判定、可见失效决策与登记 MUST 在与 runtime 安装/tombstone 更新同一互斥锁域内序列化完成（`registerDebtIfCurrent` 语义）——锁域内按精确触发令牌重新校验，校验通过才执行失效发布与登记；旧令牌 MUST NOT 覆盖较新令牌的既有登记；可见失效 apply 同样在该锁域内按触发令牌再校验（防止把新代快照标记失效）。**债务两阶段（消除 runtime=nil 与「超时后才清 runtime」的冲突）**：worker 取得任务锁后先按阶段分叉前置校验——`preCleanup`：①注册表令牌等于快照；②任务仍 active；③当前 runtime 令牌等于债务令牌（runtime 允许非空）；④tombstone 等于债务令牌。通过后持锁执行 cleanup，将同一登记原子推进为 `postCleanup`（令牌不变），再走持锁 CAS 矩阵。`postCleanup`：①注册表令牌等于快照；②任务仍 active；③当前 runtime 为空；④tombstone 等于债务令牌。通过后只重试 CAS。任一阶段 tombstone/令牌不匹配 → compare-and-delete 撤销旧债务，不 cleanup。**注册表阶段并发**：同令牌重复登记 MUST 单调合并取更高阶段（`preCleanup`→`postCleanup`），MUST NOT 出现 `postCleanup→preCleanup` 回退；阶段推进 MUST 为精确 CAS（匹配 taskID+令牌+当前阶段 `preCleanup` 才置 `postCleanup`）；推进 CAS 失败 MUST 重读：同令牌已为 `postCleanup` → 视为推进成功继续 CAS；令牌已更新（新代）→ 直接退出且 MUST NOT 删除注册项；记录缺失但任务仍 active 且 runtime 已空 → 重新登记 `postCleanup` 债务。worker 自身重试 CAS 的结果 MUST 再套同一叶节点，且每个叶节点写明注册表动作：W① `committed=true` → 先发布 `task.status_changed{from:active,to:suspended}` 再 compare-and-delete；W②a CAS error+重读 active → 保留 `postCleanup` 登记（已在本轮发布过 `resync.requested` 则本叶不再重复）；W②b CAS error+重读非 active/不存在 → 先 `resync.requested` 再 compare-and-delete；W②c CAS error+重读 error → 保留 `postCleanup` 登记；W③a `committed=false`+重读 active → 保留 `postCleanup` 登记；W③b `committed=false`+重读非 active/不存在 → compare-and-delete、不发布；W③c `committed=false`+重读 error → 同 W②c。worker 不得在未持任务锁时清理 runtime | `serve_runtime.run_status_changed` / `serve_runtime.attention_changed` / `task.status_changed` / `resync.requested` |
| Suspend 修复回迁 | `suspend.go:115` | 仅 `committed=true`；Payload=`{from,to}`（通常 `suspending→active`） | `task.status_changed` |

`UpdateTaskStatus`/`UpdateTaskStatusConditional` 等 store 层调用不感知总线——发布统一在 application 层 LifecycleService 的集中 commit helper（D0）提交点之后，store/sqlite adapter 保持纯持久化职责。领域事件按上表全量挂接；指挥中心 SSE 再用消费过滤表投影。`ClaimInitRun`/`ClaimInitRerun`/`FinishInitRun`/`ConvergeInterruptedInitRuns` 只改 `init_status`、不改 `tasks.status`，本变更 **MUST NOT** 为它们发明领域事件（含不触发 `task.activity_changed`——init 写入即使推进 `updated_at` 也不发任何领域事件，后续 projects 场景若需要再扩展）。启动期 `Reconcile` 在 HTTP 开放前执行（`main.go` 顺序），其发布按设计发往零订阅者，正确性依赖开放后首帧全量快照与不变的 projects 轮询。实现时以该路径清单逐项核对。**总线不提供跨 topic 顺序保证**（如 Force 删除的级联 `session.deleted` 与 `task.deleted` 分属不同 topic，订阅方 fan-in 的到达顺序不得作为事实依据）；指挥中心 SSE 适配器仅据事件标脏后重组装全量快照，不依赖事件顺序。

### D3: SSE 端点协议、建连状态机与推送语义

路由 `GET /api/v1/sessions/active/stream` 挂入 api 子 mux（与其他 `/api/v1/*` 同享 Bearer 中间件）。帧格式（data 统一为**裸数组**，与 REST 响应体完全同构）：

```
event: snapshot
data: [{...activeSessionDTO...}, ...]

event: update
data: [{...activeSessionDTO...}, ...]

: ping            ← 心跳注释行，默认 25s
```

**建连状态机（消除订阅竞态与首帧失败悬挂）**：

```
                ┌──────────────────────────────────────────┐
                ▼                                          │
   认证通过 → 对 task/session/serve_runtime/control 各 Subscribe 一次并 fan-in → 组装初始快照 ──失败──► 全部退订 + 返回 500（尚未写 SSE headers）
                                   │ 成功（组装期间收到事件 → 置 dirty）
                                   ▼
                     写 200 + headers，发送完整 snapshot 帧后 Flush（首帧立即可达）
                                   │
                    dirty? ──是──► 立即进入合并窗口补一帧 update
                                   │ 否
                                   ▼
              事件循环：事件到达 → 置 dirty，合并窗口（本变更固定 500ms）到期
                        → 重新组装全量快照 → 成功发 update；失败保持 dirty 并记日志，
                          下一个事件或心跳 tick 重试（不闭连）
              Overflow() 置位 → 先置 dirty=true 再立即窗口外重推一次全量快照（自愈）；
                        组装失败保持 dirty，由后续事件或心跳 tick 继续重试（不丢失自愈信号）；
                        写/flush 失败按统一写路径立即退订退出（客户端重连经 snapshot 自愈）
              心跳定时器 → 无帧时发 ": ping"；兼作写错误探测，写失败 → 关闭连接
              客户端断开或服务关停 → 退订、释放资源、退出 handler
```

- **先订阅再组装初始快照**：对四个领域 topic（`task`/`session`/`serve_runtime`/`control`）各 `Subscribe` 一次，将 `C()` fan-in 到同一事件循环，并将四路 `Overflow()` 合并为同一 dirty 信号（任一路溢出即先置 dirty 再窗口外重推）。订阅与首次组装之间的变更会因 dirty 标记在 snapshot 后补一帧 update 收敛，杜绝"查询与订阅之间的变更永久漏掉"。退订时四路 Sub 全部 Close。
- **首次组装失败**：发生在写 SSE headers 之前，退订并返回 500 标准错误信封——客户端按普通失败处理并重连，不留悬挂连接。
- **为什么 update 也发全量快照而非增量 diff**：活跃任务数量为个位数到几十个，全量报文小；增量 diff 引入客户端合并与乱序/丢帧正确性问题，收益不成比例。丢事件自愈也依赖全量语义。
- **心跳与统一写路径**：所有帧（snapshot/update/overflow 重推/心跳）MUST 经统一的 `writeSSEFrame` 写出：先 `Write` 完整帧，再以 **`http.NewResponseController(w).Flush()`**（Go 1.20+，返回 error）刷新；任何一次 Write 或 Flush 失败 MUST 立即退订并退出 handler（连接已坏，不等心跳或 ctx 兜底）；心跳注释行默认 25s，兼作存活性探测。
- **服务关停**：`http.Server.Shutdown`（`server.go:295-300`，5s 预算）不会主动终止长连接 handler。本变更 MUST 设置 `http.Server.BaseContext` 为服务进程 context，SSE handler 同时监听该 context：进程关停先取消 stream（handler 退出、订阅归零），再让 Shutdown 在预算内完成。需配"打开 SSE 后取消服务 context，预算内退出且订阅数归零"测试。
- **长连接与中间件（已核实，必须改造）**：顶层 `jsonNotFoundHandler` 用 `statusRecorder` 包装下游 ResponseWriter（`server.go:194-239`），而 `statusRecorder` **未实现 `http.Flusher`**——SSE handler 经类型断言取 Flusher 会失败，帧无法实时下推。本变更 MUST 给 `statusRecorder` 增加 `Unwrap()` 与 **`FlushError() error`**（委托 `http.NewResponseController(r.ResponseWriter).Flush()`）：`http.ResponseController` 优先匹配包装器的 `FlushError`，仅实现无返回值 `Flush()` 会吞掉底层 flush 错误（可保留 `Flush()` 兼容包装）；200 状态码本身在首写时透传（`server.go:217-228` 仅拦截 404/405），无其他干扰。
- **备选**：WebSocket（已有 `internal/api/ws.go` 基础设施）——需求是单向推送，WS 引入额外帧协议与首帧 auth 握手复杂度，SSE 语义更贴合；否决。

### D4: agentStatus 事件驱动维护——连接代（generation）有效性模型

在 ServeRuntime（领域名；代码侧现状为 `taskRuntime`，`internal/task/manager.go:267-279`，Phase 1 迁移至 `application/runtime` 的 ServeRuntimeRegistry，见 D0）新增 agentStatus 内存态：`map[sessionID]SessionStatusType` + busy>retry>idle 聚合值 + **可用性标记（连接代）**。所有写入收敛到 ServeRuntime 的**唯一 apply 方法**（输入 sessionID+状态/对账全量，内部更新 map、重聚合、返回任务级状态或可用性是否变化，供 D2 发布点使用）。**apply 在同一锁域内捕获外部投影的 before/after，并返回不可变 typed delta（含事件 Payload 所需的 from/to/available）；发布方解锁后直接使用该 delta，MUST NOT 重新读取 runtime 组装事件**（防止并发 apply 下事件 Payload 与实际状态串线）。

**连接代须与激活代区分（已核实）**：现有 `taskRuntime.generation` 是**激活代**（`manager.go:269`，用于回调身份校验），同一 ServeRuntime 实例内多次 SSE 断流/重连共享它，无法防止"旧连接的迟到对账写回已断流 runtime"。本变更在 agentStatus 内存态中维护独立的单调 `connectionEpoch` 与 `connected` 标记：

- 每次 opencode SSE 连接建立（首次/重连，onReady/onReconnect 时机）生成新 epoch 并置 `connected=true`；
- 断流回调（见下）仅使**匹配的 epoch** 失效（置 `connected=false`），不影响已由重连建立的新 epoch；
- 对账在开始时捕获当时 epoch，成功结果仅在"runtime 激活代身份 + epoch 均匹配且仍 `connected`"时写入并标记有效——陈旧对账结果（断流后才返回）MUST NOT 恢复可用性；
- **epoch 阶段门控（align 屏障）**：每个 epoch 显式区分阶段 `aligning → reconcilePending → valid`（断流即终止该 epoch）：连接建立进入 `aligning`；`alignSessions` 成功后进入 `reconcilePending`（可发起首次对账）；对账成功才进入 `valid`（快照可用）。**`valid` 期间的周期探测失败（仅模式 B；模式 A 的 `valid` 阶段不做任何周期探测/对账）使该 epoch 退回 `reconcilePending`（保持同一 connected epoch），仅发布一次 available→unavailable；后台重试成功后回到 `valid`**——恢复路径唯一。既有后台循环（`manager.go:603-622`，与 reconnect align 的串行执行路径 `activate.go:1047-1070` 相互独立）的对账重试 MUST 仅处理处于 `reconcilePending` 的 epoch；`aligning` 中或 align 失败的 epoch MUST NOT 经后台重试发起对账或恢复可用（防止基于旧 session 归属的提前对账把新 epoch 误标有效）。写回判定在阶段校验之外仍须叠加激活代 + epoch + connected 三重校验。

- **更新来源**：`handleSSEEvent` default 分支中识别 opencode session 状态事件（status 事件经 `properties.sessionID` 归属反查，沿用 `activate.go:1190-1203` 既有 fail-closed 路径，未命中一律忽略）。
- **有效性模型（替代固定时长新鲜度）**：固定 30s 过期会让状态稳定的健康任务被误判不可用（心跳在 client 层消费，稳定期间无任何事件到达 task 层），故改为连接代语义：
  - opencode SSE 首次连接/重连的 `alignSessions` 成功后，执行一次 REST `/session/status` 全量对账（复用现有探测逻辑，遵守 context 语义）：**对账结果 MUST 与对齐后本任务 owned session 集合取交集**——opencode 返回的是目录级状态 map，共享目录（dir 项目）下同目录其他任务的 session 状态 MUST 忽略（canonical opencode-orchestration spec 的共享目录隔离约束），owned 但状态缺失的 session 按 idle 参与聚合（既有契约，`agent_status.go:38`）——整批写入内存态；**对账成功才允许当前连接代进入 `valid`；若当时 owned 仍为空，外部快照保持不可用（省略字段），待后续 claim 经同一 apply 路径把 0→1 变为可用 idle；对账失败保持不可用、记录日志、不影响任务生命周期**，由既有后台循环按 30s 周期持续重试（直至成功或该连接代失效；每次尝试受 context/client timeout 限制）；
  - **断流感知（需 opencode client 新增契约，已核实现状）**：当前 `Options` 仅有 `OnReady/OnMalformed`（`client.go:230-248`），`SubscribeEvents` 回调仅 `onEvent/onReconnect` 且 `onReconnect` 在新连接建立后才触发（`client.go:545-592`），无法在断流当时降级。本变更 MUST 在 opencode client 增加断流回调契约：已建立连接（established=true）终止后、进入重连退避前同步调用一次；**主动 context 取消 MUST NOT 触发该回调**（区分网络断流与正常关停）。task 层在该回调中经唯一 apply 路径的 `applyDisconnect(epoch)` 立即标记本任务当前 runtime 快照不可用（须校验 runtime 代身份与 epoch，防止回调打到已重建的新 runtime）；`applyDisconnect` 返回 `changed`——**仅当可用性由 available 转为 unavailable 时才经 D2 发布 `serve_runtime.run_status_changed`**（快照原本不可用时不重复发布，与 D2「无实际变化不发布」一致），使推送侧尽快降级；
  - 不可用时 `AgentStatus` 返回空串（`omitempty` 省略），其余行照常组装，MUST NOT 提前终止整批组装。
- **session 生命周期联动与空 owned 语义**：外部 `agentStatus` 可用的前提是「当前连接代 `valid` **且** owned session 集合非空」。零 owned 时 MUST 省略字段（返回空串），与既有 `Manager.AgentStatus` 契约一致（`agent_status.go:39-41`：`len(sessions)==0` 返回空串，不是 idle）。所有改变 owned 成员的提交——claim（含 `resolveAnchorSession` 直接 claim，`activate.go:1354`）、delete、align 插入/删除——MUST 经同一 apply 路径维护 map 默认 idle、重聚合与可用性：0→1 在连接代已 `valid` 时按默认 idle 聚合并发布（若可用性或聚合值变化）；1→0 立即变为不可用并发布 available→unavailable。首次激活顺序是先 align/对账、再 `resolveAnchorSession` claim 首个 session（`activate.go:1354`），因此对账成功时 owned 仍可能为空——此时保持不可用，待 claim 经同一 apply 路径把 0→1 变为可用 idle，MUST NOT 把空集合标成 idle。
- **REST 端点同步改造（不动其他消费者）**：`Manager.AgentStatus` 被 `/projects` 水合（`projects.go:192/210`）与 `/tasks/{id}`（`tasks.go:189,241`）共享，这两个接口不在本次范围、其 canonical spec（project-management / opencode-orchestration）仍要求请求路径实时水合，故**既有 `AgentStatus` 实时探测语义保持不变**。本变更新增独立的快照读取方法（如 `Manager.AgentStatusSnapshot(taskID) string`，读内存快照、不可用返回空串），仅供 `handleListActiveSessions` 与 SSE 组装 helper 使用：`handleListActiveSessions` 的实时水合（`tasks.go:541-559`，并发 8/3s 预算）移除，改调快照方法，响应字段与降级行为不变；原"水合调用链 context 收敛"的水合语义转移到对账路径（对账调用链仍全程遵守调用方 context）。
- **模式执行矩阵（Phase 0 二选一后严格执行，MUST NOT 混搭）**：模式 A——解析并 apply 状态事件，`valid` 阶段不做周期探测，仅 `reconcilePending` 阶段由后台重试对账；模式 B——忽略状态事件（不解析不 apply），后台循环按固定 **30s**（复用既有 `backgroundLoop` ticker，`manager.go:606`）探测 `valid` 与 `reconcilePending` 阶段的 active runtime（`aligning` 一律跳过）。模式选择是实现期常量（Phase 0 写入 design.md 后编译期固定），MUST NOT 做成运行时配置。两分支分别配独立测试。
- **风险对冲（fallback，预定义的合规模型 B）**：spec 预先定义两个合规模式——模式 A（事件驱动，默认）与模式 B（后台低频探测缓存），Phase 0 门禁按预定义判据二选一，MUST NOT 实施中临时改规格。模式 B 语义：runtime 建立时仅初始化为不可用（此时无有效 epoch、owned 集合未完成 align，MUST NOT 提前探测或 apply）；首次连接或重连的 `alignSessions` 成功并进入 `reconcilePending` 后立即执行首次探测（不等待后台周期），此后才进入 30s 周期探测节奏；此后复用既有后台循环对**全部 active runtime** 周期探测（task 层只持窄 Publisher、无法也不应感知订阅者状态，故不设"订阅者活跃"前置条件）；探测结果同样 MUST 与 owned session 集合取交集后整批 apply；探测失败置该 runtime 不可用（沿用连接代降级语义）；仅在外部可见聚合值或可用性变化时发布事件；连接代/断流语义与模式 A 一致（断流即不可用，重连对齐后探测恢复）。推送/REST 读路径两种模式下均禁止实时调用。总线与 SSE 协议不受影响——agentStatus 来源是可替换的实现细节。

### D5: 前端 fetch streaming 订阅

新增 `web/src/sse.ts`（或并入 `api.ts`，实现期按内聚度定）：

```ts
function subscribeActiveSessions(handlers: {
  onData(sessions: ActiveSessionItem[]): void; // snapshot 与 update 同构，均为裸数组
  onError(msg: string): void;
}): { close(): void }
```

- 用 `fetch` + `Authorization: Bearer`（复用 `getToken()`）发起，`res.body` ReadableStream 手动解析 SSE 帧（`event:`/`data:`/注释行，`TextDecoder` 流式拼接按 `\n\n` 分帧，处理跨 chunk 粘包）；snapshot/update 帧 data 均为裸数组，同一解析路径。
- **畸形帧策略**：注释行与未知 event 类型 MUST 忽略；`snapshot`/`update` 帧的协议错误（非法 JSON、data 非数组）MUST 触发 `onError`、保留旧数据、终止当前连接并退避重连，且 MUST NOT 因此重置 backoff（防止坏数据导致的快速重连风暴）。
- **401 处理与 `request()` 对齐**：收到 401 → `clearToken()` + `UNAUTHORIZED_EVENT`，**停止重连**。
- **重连退避**：断线/非 401 错误后指数退避（1s 起步 ×2，上限 30s，**首个有效帧到达后重置**）；沿用"断线自动重连即可、无轮询 fallback"的既定方向。`close()` MUST 同时置永久 closed 标志、中断当前 fetch（AbortController）并取消在途退避计时器——卸载发生在 backoff 等待期间时也 MUST NOT 再发起 fetch。
- **CommandCenterPage 改造**：移除 `usePoll(pollSessions, 5000)`，mount 时订阅、unmount 时 `close()`；`onData` 直接 `setSnap`。保留 `initialized/attempted/error/lastSuccessAt` 并**修改 `resolveSessionsBootstrap`（`CommandCenterPage.tsx:83-125`）的判定**：**整页 loading 仅在 projects 自身未初始化时进入；sessions 未首帧一律为独立「连接中」状态，MUST NOT 升级为整页 loading，且抑制全局与分区空态**——现有实现在 `projectsInit=true 且 projectsLen=0 且 sessions 未 attempted` 时返回整页 loading（`:109-110`，被 `command-center.test.ts:510-513` 锁定），本变更须消除该分支：projects 已有数据时 MUST 继续渲染 projects-only 任务（`hasAnyData` 为真，`:96-110`）；projects 为空且 sessions 未首帧时展示连接中；首次连接错误走现有 `sessionsAttempted && !sessionsInitialized && sessionsError → error`（`:103-106`），不与连接中并存。双快照 join（`command-center-selector.ts:90-148`）不变。
- **备选**：`@microsoft/fetch-event-source` 库——引入依赖仅为解析几十行协议，自建可控且与现有 `request()` 错误语义对齐；否决。

### D6: 组装逻辑复用、总线所有权与依赖方向

快照组装（overview 查询 + attention + agentStatus 快照 → DTO 数组）抽为 api 层共享 helper，REST handler 与 SSE handler 共用，保证两端点响应同构。

**总线所有权**：`cmd/ocdeck-server/main.go`（composition root）创建单例 bus，按窄接口注入（沿用项目现有 wiring 模式：`api.New` + setter + `RebuildRoutes`，见 `server.go:93-121`、`main.go:130-142`）：

```
main.go
  └─ bus := eventbus.New()
       ├─ application task LifecycleService（迁移期经 task facade 的 task.Options{Publisher: bus}）——应用层只见窄 Publisher（Publish）
       └─ srv.SetEventSubscriber(bus); srv.RebuildRoutes() // api 层只见窄 Subscriber（Subscribe），须在 RebuildRoutes 前注入
```

- api 层读取业务数据仍只经 `TaskBackend`（`tasks.go:17-71`）——总线订阅能力是**传输层关注点**，不混入业务 backend 接口；store/sqlite adapter 不 import eventbus。
- 关闭责任：bus 生命周期随 main ctx；SSE handler 退订自身 Sub（D3）；Publish 方无需关闭操作。

## Risks / Trade-offs

- [状态事件契约不完全确定（确切 type 字符串与字段路径待实测）] → tasks 设 Phase 0 实测门禁：捕获 idle/busy/retry 三类真实 fixture 并固化解析契约测试；未命中 fail-closed 忽略；门禁失败启用 D4 fallback（低频探测缓存）。
- [DDD 重构中漏事件或状态语义漂移（写点众多）] → 以全部 store 写调用点形成 D2 提交矩阵；每迁移一个用例做正向/CAS 未命中/store error/同值 no-op 测试；旧 facade 只允许单写路径。
- [ServeRuntime 换代、回调等锁、teardown 与债务 worker 竞态] → 单一 Registry 锁域 + typed RuntimeToken + tombstone + compare-and-delete + preCleanup→postCleanup 单调状态机；`go test -race` 加可控 barrier 测试。
- [DB、Runtime、Publish 三者无原子事务；session 物理键弱于领域模型] → publish-after-commit；不确定结果发 resync；Overflow+全量快照兜底；SessionRepository 方法闭合为 Claim/TouchOwned/DeleteOwned/Align/OwnedSessions/OwnerOf（禁通用 Save/Upsert）；历史重复归属读到时 fail-closed。
- [事件丢失（缓冲溢出/进程内 at-most-once）] → D1 `Overflow()` 可观察溢出 + D3 全量快照语义与溢出即重推 + 心跳写探测，正确性不依赖单事件送达。
- [合并窗口引入最长 ~500ms 额外延迟] → 窗口值本变更固定 500ms（后续调整须另走规格变更）；相较 5s 轮询仍是一个数量级改善。
- [长连接资源占用（每浏览器标签页一条连接）] → 本地单用户工具，连接数个位数；`http.Server` 无写超时，ReadHeaderTimeout 不影响已建立连接。
- [statusRecorder 未实现 http.Flusher，SSE 无法经现有中间件栈 flush（server.go:211-239 已核实）] → 本变更显式包含该改造：statusRecorder 增加 `Unwrap()` 与 `FlushError() error` 委托（仅无返回值 `Flush()` 会吞底层 flush 错误），加测试锁定透传与错误传播行为。
- [Shutdown 不终止长连接，正常关停拖到 5s 预算超时] → D3 BaseContext + handler 监听服务 ctx，先取消 stream 再 Shutdown。
- [agentStatus 由"每次请求实时"变为"内存快照"，断流期间降级省略] → 用户已确认该取舍；连接代模型保证可用性语义与 opencode SSE 通道真实状态一致，不劣于现状。

## Open Questions

- opencode session 状态事件的确切 `type` 字符串与 status 字段路径（Phase 0 实测，见 D4/门禁）。

（合并窗口固定为 500ms——本变更的确定值；后续如需调整另走规格变更，不在实施中临时修改。）
