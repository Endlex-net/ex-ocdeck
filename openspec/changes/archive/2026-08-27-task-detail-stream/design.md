# Design: task-detail-stream

## Context

任务详情页（`web/src/pages/TaskWorkbenchPage.tsx:175`）以 `usePoll` 每 2s/4s 轮询 `GET /api/v1/tasks/{id}`，页面打开期间永不停止。项目已有 SSE 读模型流基建：后端共享循环核心 `runReadModelStream`（`internal/api/read_model_stream.go:59`，四 topic fan-in、先订阅再组装、500ms 合并窗口、25s 心跳、溢出自愈、统一写路径），场景差异仅 `assemble`/`eventDirty` 两个注入点；前端通用订阅器 `subscribeStream`（`web/src/sse.ts:40`，Bearer、逐行分帧、指数退避重连）。详情页全部字段变化均由领域事件驱动（`task.*`/`session.*`/`serve_runtime.*`/`control.resync.requested`，发布点收敛于 `internal/application/task/lifecycle.go` 的 commit helper）。

约束（继承现有规格）：推送路径纯读、MUST NOT 实时调用 opencode；对外帧仅 snapshot/update + 注释行心跳；领域事件与 SSE 帧解耦；无 last-event-id，以全量快照自愈。

## Goals / Non-Goals

**Goals:**

- 新增 `GET /api/v1/tasks/{id}/stream` 单实体 SSE 流，帧 data 与 `handleGetTask` 的 `taskRowDTO` 同构。
- 详情页移除 `usePoll` 轮询，改 `subscribeTask` 订阅驱动；任务删除后收敛到现有 not-found 展示。
- 复用共享流核心与通用订阅器，行为差异全部收敛为注入点/薄绑定。

**Non-Goals:**

- 不改造 `ServerStatusBanner` 30s 轮询与 projects store 30s 兜底轮询。
- 不改 `GET /api/v1/tasks/{id}` REST 端点的成功响应结构与实时探测 `AgentStatus` 行为（D3 配套 recorder 改动会使其 404 响应 message 原样保留 handler 文案，见 D3 风险披露）。
- 不引入 last-event-id / 事件重放；不新增事件类型；不改 Fx/组合根接线（`eventSubscriberAdapter` 复用）。

## Decisions

### D0 事件契约扩展：解除 P1.6.1 冻结（用户已决策）

`task.activity_changed` 发布条件从"DTO 可见非 status 字段真实变更且 `updated_at` 跨秒推进"放宽为"真实变更（Changed=true）即发布，不再要求跨秒"，并显式接入三个 init 提交点：`LifecycleService.ClaimInitRun`/`ClaimInitRerun`/`FinishInitRun`（`internal/application/task/delete_reconcile.go:57-70`，当前直通 store 不发布）。`ConvergeInterruptedInitRuns` 在 HTTP 开放前执行（零订阅者），不挂接。失败、未命中、无变化路径不发布；与 `task.status_changed` 同事务不重复发布的不变量不变。

依据：P1.6.1 的冻结理由是 scope 裁剪（`archive/2026-08-26-sse-active-sessions/design.md:429`："后续 projects 场景若需要再扩展"），详情页正是该场景。放宽仅覆盖运行期逐任务 `MutationResult` 提交；启动期 `ConvergeInterruptedInitRuns`（HTTP 开放前、零订阅者）不发布断言保留为例外。对既有两条流的影响：init 系列写入以及 delete_mode/notice/last_port/同状态 last_error/env_snapshot 等同秒提交均会多产生标脏 update 帧（合并窗口吸收），无语义变化；冻结测试 `TestP148_InitStatusWritesNeverPublish` 中运行期三提交点断言反转为发布、Converge 断言保留。另注意 `commitSessionsAligned`（`internal/application/task/sessions.go:274`）旁路了 `commitTaskMutation`、直接判 `Changed && UpdatedAtAdvanced`，放宽需同步修改该旁路条件为 `Changed`。

### D1 端点与路由

`GET /api/v1/tasks/{id}/stream` 注册于 `registerTaskRoutes`（`internal/api/tasks.go`），**始终注册**（不放入 `s.eventSubscriber != nil` 条件块）；`eventSubscriber` 未注入时 handler 返回 500 标准错误信封（`event stream not configured`），使该路径的 404 唯一表示"任务不存在"（见 D3 的客户端语义）。Go 1.22 mux 模式优先级使 `{id}/stream`（含字面段 `stream`）优先于 `{id}`，无路由冲突；`/tasks/active/stream` 字面段优先于 `{id}/stream`，互不干扰。

### D2 薄绑定 + 按 taskID 的消费过滤表

新增 `internal/api/task_stream.go`：handler 从 `r.PathValue("id")` 取 taskID，构造闭包调用 `runReadModelStream`：

- `assemble`：组装该任务的 `taskRowDTO`（见 D4）。
- `eventDirty`：新增 `eventDirtiesTaskDetail(taskID)` 过滤函数（`internal/api/task_filter.go`），判定决策表（优先级自上而下）：

  | 事件形态 | 判定 |
  |---|---|
  | 未知 Type（任何 topic） | 脏（保守自愈） |
  | 已知 Type 但 Topic/Payload 类型不合法（畸形事件） | 脏（保守自愈） |
  | `task.*`（已知 Type） | `ev.RID == taskID` → 脏，否则不脏 |
  | `sessions.aligned` | `ev.RID == taskID` → 脏，否则不脏 |
  | `session.claimed`/`touched`/`deleted` | Payload `SessionOwnerPayload.TaskID == taskID` → 脏，否则不脏 |
  | `serve_runtime.attention_changed` / `run_status_changed` | Payload TaskID 字段 == taskID → 脏，否则不脏 |
  | `resync.requested` | 恒脏 |
  | 其余合法且不关联事件 | 不脏 |

备选（否决）：扩展核心支持增量 diff 或单任务 patch——违反"对外帧仅全量快照"的既有规格精神，且详情对象小，全量重推成本可忽略。

### D3 共享核心扩展：assembleGone 钩子 + 404 信封保留

`readModelStreamConfig` 新增可选字段 `assembleGone func(error) bool`（nil = 现有行为，active/projects 两场景不受影响）：

- **初始组装**：`assembleGone(err)` 为 true → 不写 SSE 头、不写 `errCopy` 500，改写 JSON 404 标准错误信封（`error.code=not_found`、`error.message=task not found`），订阅经既有 defer 全量退订。
- **推送路径**（`pushUpdate`）：`assembleGone(err)` 为 true → 返回包级 sentinel `errStreamGone`；所有 `pushUpdate` 调用点对 `errStreamGone` 按写失败同等处理（退订退出 handler），但不记错误日志（正常业务终态）。其余组装失败语义不变（保持 dirty 重试）。

详情场景注入 `assembleGone: func(err) bool { return errors.Is(err, application.ErrTaskNotFound) }`。不存在的判定 MUST 走 sentinel（`internal/application/ports.go:22`），MUST NOT 按错误文案匹配。task 层 opErr 经 `fmt.Errorf("task not found: %w", application.ErrTaskNotFound)` 包装（如 `internal/task/crud.go`），`errors.Is` 链可达。

**404 信封保留（配套改动）**：`jsonNotFoundHandler`/`statusRecorder`（`internal/api/server.go:196-249`）当前吞掉下游所有 404/405 body 并重写为 `no route for ...`。改为：`statusRecorder` 对 404/405 缓冲下游响应（状态码、Content-Type、body）；`jsonNotFoundHandler` 若下游已写标准 JSON 错误信封（Content-Type 为 `application/json`）则原样转发，否则沿用既有重写。这使本端点的 404 信封（固定文案 `task not found`）真正到达客户端；对既有 REST 404 的副作用是 message 从统一 `no route for ...` 变为各 handler 原始文案（如详情 REST 经 `crud.go:730` 包装后实际为 `task not found: task not found`），`error.code` 与 status 不变，仅断言消息的测试需同步更新；本变更 MUST NOT 顺手规范化既有 REST 错误文案。

备选（否决）：`eventDirty` 对 `task.deleted` 直接关流——`eventDirty` 签名只返回 bool，改动面更大且与"删除 → 重组装未命中 → 关流"的自然链路重复；删除事件只需标脏，由重组装的 not-found 统一关流，竞态（事件丢失/乱序）也由该路径兜底。

### D4 详情快照组装：抽取共享 helper，REST 与流同源

先收敛再新增：从 `handleGetTask`（`tasks.go:238-261`）抽取共享 helper `buildTaskDetailDTOBase(ctx, t, sessions)`（`internal/api/tasks.go`），集中执行 `requireProjectKind`、`toTaskDTO`、`toSessionDTOs(sessions)`、`Attention` 填充等纯 DTO 映射。**查询策略不进 helper、由两侧各自持有**：

- REST：`Get` 失败按 `mapTaskErr` 返回；`ListTaskSessions` 失败沿用既有降级语义忽略（行为不变）。
- 流（`assembleTaskDetail`）：`Get` 失败直接返回 err（not-found 由 D3 处理，其余走核心通用重试语义）；`ListTaskSessions` 失败 MUST 作为组装失败返回 err（保持 dirty、跳帧重试）——纯 SSE 模式下吞错省略 `sessions` 可能长期不自愈，故与 REST 降级语义有意不同。

最后**唯一值差异点**分别填充：

- REST：`dto.AgentStatus = s.tasks.AgentStatus(ctx, taskID)`（实时探测，行为不变）
- 流：`dto.AgentStatus = s.tasks.AgentStatusSnapshot(taskID)`（内存快照，推送路径 MUST NOT 实时探测）

`requireProjectKind` 返回的 `*ApiError`（含项目不存在 `CodeNotFound`）不在 `ErrTaskNotFound` 链上，流侧按通用组装失败（保持 dirty 重试）处理——比误判关流更保守，可接受。补 REST/stream 逐字段同构测试：同一任务两种入口的 DTO 仅允许 `agentStatus` 值不同。

备选（否决）：把 REST `handleGetTask` 也改为 `AgentStatusSnapshot` 以消除两处差异——扩大变更面且改变既有 REST 行为语义，属非目标。

### D5 前端：subscribeStream 增加 onGone 终态 + subscribeTask 场景绑定

- `subscribeStream` 的 `SubscribeStreamOptions` 新增可选 `onGone?: () => void`：`loop` 中 `res.status === 404` 且提供了 `onGone` 时，调用 `onGone()`、置 closed、不回调 onError、不安排重连（永久终态，与 401 同级的终止语义）。未提供 `onGone` 的既有调用方（active/projects）行为不变（404 仍走 onError + 退避）。
- `SubscribeStreamOptions` 另增可选 `reportEndAsError?: boolean`（默认 false，仅 `subscribeTask` 开启）：ReadableStream 正常 EOF（`outcome === 'ended'`）时先 `onError(`${errorLabel}连接中断`)` 再退避重连——服务端关流/断流必须展示错误提示（spec「推送/订阅异常期间保留旧数据并展示错误提示」），既有订阅方行为不变。删除链路：EOF 错误提示 → 重连 → 404 → onGone，期间短暂提示可接受。
- 新增 `subscribeTask(taskID, opts)`（`web/src/sse.ts`）：路径 `/api/v1/tasks/${taskID}/stream`，`errorLabel: '任务详情'`；帧 data 为单对象，自定义 `validate` 谓词固定为 `typeof data === 'object' && data !== null && !Array.isArray(data)`（仅校验信封形状，不做字段级 schema 校验），通过后包装 `[data as Task]` 复用 `subscribeStream<Task>` 的数组通道，`onData(items)` 取 `items[0]` 回调给页面（对核心零侵入）。`onGone` 由页面传入。validate 覆盖 null / 数组 / primitive / 对象四类用例。

备选（否决）：为单对象重构 `subscribeStream` 泛型通道——改动面与回归风险大于 validate 包装一层。

### D6 详情页改造

`TaskWorkbenchPage`：

- 移除 `usePoll(() => void load(), ...)` 与 `initActive` 轮询频率逻辑。
- `useEffect`（deps `[taskID]`）建立 `subscribeTask(taskID, { onData: setTask + 清 error, onError: setError, onGone: () => setNotFound(true) })`，cleanup `sub.close()`。
- 首帧前（`task === null && !notFound`）展示固定「连接中…」状态（现状无该分支，仅标题省略号，本变更新增）；首次连接失败展示错误提示并继续退避重连；已有数据后 `onError` 保留旧数据展示错误（现有 error state 通道）。
- `onTaskActionDone` 移除手动 `void load()`（推送承接），保留 `refreshShared()`（侧栏/指挥中心共享 store 同步仍需要）；`load()` 函数删除，`api.getTask` 在本页不再使用（`ApiError` 404 → notFound 的初始路径由 `onGone` 承接）。
- 现有 `notFound` 渲染分支不变。页面接线补源码契约测试（订阅替代轮询、卸载清理、onGone → notFound）。
- **taskID 切换不变量**：`App.tsx:71` 以 `key={res.taskID}` 重挂载 `TaskWorkbenchPage`，组件内 state（task/notFound/error）随重挂载整体重置；组件 MUST NOT 被要求独立承接同实例 taskID prop 切换（若未来去掉 key 重挂载，须先补 `setTask(null)`/`setNotFound(false)`/清错误的状态重置）。

备选（否决）：保留 4s 低频兜底轮询——用户已明确纯 SSE 重连；snapshot 自愈 + resync.requested 兜底已覆盖收敛性。

### D7 前端 404 与"删除关流"的完整链路

服务端流期间关流（D3）→ 客户端 `readStream` 返回 `'ended'` → 退避重连 → 服务端初始组装 not-found → JSON 404 → `onGone` → notFound。链路无新增协议帧，所有终态判定收敛在 HTTP 层，不引入 `gone` 事件帧（保持对外帧闭合枚举 snapshot/update）。

## Risks / Trade-offs

- [D3 核心改动影响既有两条流] → `assembleGone` 为可选注入、nil 时逐路径保持现有行为；既有流测试（sessions_stream_test / projects 流测试）全量回归。
- [D3 404 信封保留改变既有 REST 404 响应消息] → `error.code` 与 status 不变，仅 message 从 `no route for ...` 变为 handler 原始文案；前端均按 status 判定，断言消息的测试同步更新。
- [D0 事件契约放宽增加事件量] → 放宽覆盖运行期逐任务提交的全部非 status 同秒变更（init/delete_mode/notice/last_port/同状态 last_error/env_snapshot），均为低频操作；500ms 合并窗口吸收突发；既有两条流仅多收标脏 update 帧，无语义变化；冻结测试反转（Converge 例外保留）+ 两条流回归覆盖。
- [按 taskID 过滤遗漏某类关联事件导致详情不更新] → 未知/畸形 Type 保守标脏 + `resync.requested` 全量重拉 + 溢出自愈三重兜底；过滤表随事件类型目录评审。
- [删除与重组装竞态：删除事件在订阅建立前发生] → 先订阅再组装的核心时序 + 初始组装 not-found → 404 已覆盖；事件丢失由溢出自愈/重连 snapshot 收敛。
- [单连接每客户端每任务一条订阅，多标签页打开同一任务产生 N 条连接各自重组装] → 重组装为纯读（一次 Get + sessions 查询 + 内存快照；`AgentStatusSnapshot` 内部有一次任务查询，仍为纯读且不调用 opencode），事件驱动、频率远低于既有 2s/4s 轮询。
