# Tasks: task-detail-stream

依赖顺序：1（事件契约）与 2（核心扩展）可并行；3 依赖 1+2；前端实现（4.1-4.3）在 API 契约冻结后可与 1-3 并行，4.4 的页面级集成验收依赖 3 完成；5 最后。无 DB schema/migration/repository mapping 变更；仓库未使用 Uber Fx，组合根（`cmd/ocdeck-server/main.go`）接线无需改动。

## 1. 事件契约扩展：解除 P1.6.1（D0）

- [x] 1.1 `internal/application/task/delete_reconcile.go`：`ClaimInitRun`/`ClaimInitRerun`/`FinishInitRun` 三个提交点在真实变更（Changed=true）后发布 `task.activity_changed`（复用 `commitTaskMutation` 或等价集中 helper；失败/未命中/无变化不发布；删除过时的"never emit domain events (P1.6.1)"注释）
- [x] 1.2 发布条件放宽：`task.activity_changed` 不再要求 `UpdatedAtAdvanced`（未伴随 status 迁移的任务行非 status 真实变更 Changed 即发），同步更新 `event.go` 中 `TypeTaskActivityChanged` 注释与 `commitTaskMutation` 语义
- [x] 1.3 `internal/application/task/sessions.go:263-277` `commitSessionsAligned` 旁路提交点：notice 发布条件从 `res.TaskMutation.Changed && res.TaskMutation.UpdatedAtAdvanced` 改为 `res.TaskMutation.Changed`，同步更新注释（"按 updated_at 规则"表述过时）；新增 `Changed=true/UpdatedAtAdvanced=false` 仍发布、无变化不发布的测试（现有 `sessions_test.go:349-383` 无同秒用例）
- [x] 1.4 反转全部反向冻结测试并清理旧口径注释：冻结测试 `TestP148_InitStatusWritesNeverPublish` 位于 `internal/application/task/delete_reconcile_test.go:139-169`（现有用例只直接覆盖 `ClaimInitRun` 和 `FinishInitRun`，须新增 `ClaimInitRerun` 覆盖；**其中 `ConvergeInterruptedInitRuns` 不发布断言保留**——启动期零订阅者为例外，见 D0/delta spec「启动期收敛不发布」，可拆分或重命名该用例）；另含 `create_activate_test.go:95-108,160-205`、`delete_reconcile_test.go:116`；旧语义注释含 `create_activate.go:8,94`、`delete_reconcile.go:39,50,56,62,68`、`sessions.go:50,263-265`。所有 `commitTaskMutation` 调用方须覆盖"同秒 Changed 发布一次、无变化/失败不发布"；init 真实变更断言发布一次 `activity_changed`
- [x] 1.5 既有流回归：active-sessions 与 projects 流测试全量通过（放宽后仅多产生标脏 update 帧）

## 2. 后端：共享核心扩展（D3）

- [x] 2.1 `internal/api/read_model_stream.go`：`readModelStreamConfig` 新增可选 `assembleGone func(error) bool`；初始组装 gone → JSON 404 标准错误信封（`not_found`/`task not found`，不写 SSE 头，订阅经 defer 退订）；`pushUpdate` gone → 返回包级 sentinel `errStreamGone`，各调用点按退订退出处理且不记错误日志；nil 时全部路径保持现有行为；同步收敛核心注释——`assemble`/`pushUpdate` 注释中"DTO 裸数组"改为"场景完整快照、与对应 REST 响应同构"（本变更引入单对象场景，注释不得与泛型语义冲突）
- [x] 2.2 `internal/api/server.go`：`statusRecorder` 对 404/405 改为缓冲下游响应；`jsonNotFoundHandler` 对下游已写 `Content-Type: application/json` 的标准错误信封原样转发，仅重写 mux 裸文本 404/405
- [x] 2.3 核心扩展测试：初始 gone → 404 无 SSE 头；推送期 gone → 连接关闭退出；nil 钩子（既有行为）回归；recorder 保留 JSON 信封 / 重写裸文本两路径；断言既有 REST 404 message 变化的测试同步更新

## 3. 后端：详情流薄绑定（D1/D2/D4）

- [x] 3.1 `internal/api/tasks.go`：从 `handleGetTask` 抽取共享 helper `buildTaskDetailDTOBase(ctx, t, sessions)`（requireProjectKind + toTaskDTO + toSessionDTOs + Attention 纯 DTO 映射；查询策略不进 helper）。REST：`Get` 失败走 mapTaskErr、`ListTaskSessions` 失败沿用既有降级忽略（行为不变）；流：`Get`/`ListTaskSessions` 失败均作为组装错误返回（not-found 由 assembleGone 处理，其余保持 dirty 重试）。REST 填 `AgentStatus`（实时探测不变）
- [x] 3.2 `internal/api/task_filter.go`：新增 `eventDirtiesTaskDetail(taskID)`，按 design D2 决策表实现（未知/畸形保守标脏；task.*/sessions.aligned 比 RID；session 单条与 serve_runtime 比 Payload TaskID；resync 恒脏）
- [x] 3.3 `internal/api/task_stream.go`：新增 `assembleTaskDetail`（Get → base → `AgentStatusSnapshot`）与 `handleTaskStream` 薄绑定（`assembleGone: errors.Is(err, application.ErrTaskNotFound)`）；`registerTaskRoutes` 始终注册 `GET /api/v1/tasks/{id}/stream`，`eventSubscriber == nil` 时返回 500 `event stream not configured`
- [x] 3.4 流端点测试：连接即收 snapshot（与 taskRowDTO 同构）；关联事件 → update（含 init 事件）；他任务合法事件不触发；500ms 窗口合并；初始不存在 → JSON 404（`error.message=task not found`）；流期间删除 → 关流；非 not-found 组装失败保持连接重试（含 `ListTaskSessions` 失败跳帧保持 dirty，REST 同场景仍降级忽略）；推送路径 `AgentStatus` 实时探测调用 0 次；401 拒绝；subscriber 未注入 → 500
- [x] 3.5 过滤表单测：design D2 决策表全组合（含未知 Type、畸形 Payload、不关联事件）
- [x] 3.6 REST/stream 逐字段同构测试：同一任务两种入口 DTO 仅允许 `agentStatus` 值不同

## 4. 前端：订阅器与页面改造（D5/D6/D7）

- [x] 4.1 `web/src/sse.ts`：`SubscribeStreamOptions` 新增可选 `onGone?: () => void`；`loop` 中 `res.status === 404` 且有 `onGone` → 调用之、置 closed、不 onError、不重连；未提供的调用方行为不变
- [x] 4.2 `web/src/sse.ts`：新增 `subscribeTask(taskID, opts)`（路径 `/api/v1/tasks/${taskID}/stream`，errorLabel「任务详情」，validate 谓词 `typeof data === 'object' && data !== null && !Array.isArray(data)` 通过后包装 `[data as Task]`，onData 取 `items[0]`，透出 onGone）
- [x] 4.3 `web/src/pages/TaskWorkbenchPage.tsx`：移除 `usePoll` 轮询与 `initActive` 频率逻辑；`useEffect([taskID])` 建立 `subscribeTask`（onData→setTask/清 error，onError→setError 保留旧数据，onGone→setNotFound），cleanup close；新增 `task === null && !notFound` 时的「连接中…」固定展示；`onTaskActionDone` 移除手动 `load()` 保留 `refreshShared()`；删除 `load()` 与本页 `api.getTask` 使用
- [x] 4.4 前端测试：subscribeStream 404+onGone 终态（不重连不 onError）；无 onGone 时 404 旧行为回归；subscribeTask validate 四类用例（null/数组/primitive/对象）与 onData 解包；页面级：订阅替代轮询、「连接中…」展示、删除 → 404 → not-found、卸载清理、taskID 切换经 App 路由（`key={taskID}` 重挂载，App.tsx:71）重建订阅

## 5. 验证

- [x] 5.1 `go build ./... && go test ./...`（全量，含既有流与事件生产侧测试回归）
- [x] 5.2 前端 `npm test`（web/ 下 vitest）与 `npm run build`
- [x] 5.3 新增行为测试有效性证据：关键新测试在旧实现下失败、新实现下通过（基线验证或等价 mutation 验证，如临时注释 init 发布点确认测试变红）
