# Tasks: 项目/侧栏数据面 SSE 推送与端点 task 命名对齐

> 在树状态说明：aborted lane 已在未提交工作树中留下 Item A / Item C 的部分实现（`internal/api/projects.go`、`internal/api/tasks.go`、`internal/api/sessions_stream.go`、`internal/application/dto.go`、`internal/infrastructure/store/queries.go`、相关测试、`web/src/api.ts`、`web/src/sse.ts`、`web/src/__tests__/sse.test.ts`，以及对前一变更 4 个文档的在树改写）。这些改动未经审查，下列 §1 以「复核」项逐文件承接，MUST NOT 视为已完成。

## 1. 在树部分实现复核（Item A / Item C 遗留，逐项核对而非盲信）

- [x] 1.1 复核 Item C 后端路由与引用：`internal/api/tasks.go` 路由注册（canonical `GET /api/v1/tasks/active` 与别名 `GET /api/v1/sessions/active` 同 handler `handleListActiveSessions`；SSE 仅注册 `GET /api/v1/tasks/active/stream`）与注释；`internal/api/sessions_stream.go`、`internal/application/dto.go`、`internal/infrastructure/store/queries.go` 中端点路径注释引用一致性；`go build ./...` 通过
- [x] 1.2 复核 Item C 测试面：`internal/api/active_sessions_api_test.go`（canonical 路径断言 + `TestListActiveSessions_LegacyAliasSameResponse`）、`sessions_stream_test.go`（路径断言 + `TestActiveSessionsStream_LegacyPathJSON404`：JSON 404 信封、不写 SSE 头、`liveSubs()==0`）、`sessions_snapshot_test.go`、`attention_api_test.go` 路径；`web/src/__tests__/sse.test.ts` 断言路径为 `/api/v1/tasks/active/stream`
- [x] 1.3 复核 Item C 前端路径：`web/src/api.ts` `listActiveSessions` → `/tasks/active`、`web/src/sse.ts` `STREAM_PATH` → `/api/v1/tasks/active/stream`；全仓 `rg 'sessions/active'`（web 与 internal）核对无残留调用点（文档/规格中的历史引用除外）
- [x] 1.4 回退 aborted lane 对前一变更文档的在树改写：`openspec/changes/sse-active-sessions/design.md` 与 `specs/{active-sessions-overview,active-sessions-stream,command-center}/spec.md` 恢复 HEAD 版本（前一变更冻结；新路径由本变更 deltas 承载）
- [x] 1.5 复核 Item A 在树实现：`internal/api/projects.go`——`hydrateProjectTaskAgentStatuses`/`hydrateSingleProjectAgentStatuses`/`runAgentStatusHydration` 已删除、`toProjectTaskSummaryDTOs` 为 Server 方法且 active 摘要读 `s.tasks.AgentStatusSnapshot(sm.TaskID)`、`handleGetProject` 同步改用、无 goroutine/信号量/3s 预算残留；`internal/api/attention_api_test.go` 改写面（`projectSummaryBackend.agentStatusSnapshot` 注入、`AgentStatus` 仅记录调用、快照降级测试、实时探测零调用断言）
- [x] 1.6 把在树代码注释中的 stale lane 编号（P2.10/P2.11）改写为本变更（projects-stream）引用；`/tasks/{id}` `handleGetTask` 实时探测（`AgentStatus(ctx, ...)`）保持并跑既有回归测试确认不变
- [x] 1.7 §1 复核完成门槛：`go build ./... && go test ./internal/api/... -race` 与 web `tsc`/vitest 全绿（在树部分实现经复核后的基线）

## 2. /projects 组装共享与 projects 场景消费过滤

- [x] 2.1 抽 `buildProjectsSnapshot(ctx) ([]projectDTO, error)` 共享组装 helper（projects 列表 + `ListProjectTaskSummaries` + counts + 分组 + 摘要 DTO（agentStatus 读快照））；`handleListProjects` 改调 helper 后写响应，DTO 字节级不变（既有 projects 测试回归）；`handleGetProject` 详情路径保持单项目组装
- [x] 2.2 新增 projects 场景消费过滤纯函数 `eventDirtiesProjectsTaskTree(ev)`（与 `eventDirtiesActiveSessions` 同层；全部 `task.*`——含 `task.created`、任意 from/to 的 `task.status_changed`、任意 from 的 `task.deleted`、`task.activity_changed`——与 default 分支全部标脏：`session.*`/`serve_runtime.*`/`resync.requested`/未知 Type；不解读 Payload 做增量合并）
- [x] 2.3 消费过滤单测：逐行表驱动覆盖全部已知 Type（重点差异行：`task.created`、两端均非 active 的 `task.status_changed`、`from!=active` 的 `task.deleted` 在本场景标脏）与未知 Type；与 `eventDirtiesActiveSessions` 的差异对照断言

## 3. SSE 循环共享核心抽取（fb-35：MUST NOT 平行复制循环）

- [x] 3.1 从 `internal/api/sessions_stream.go` 抽共享循环核心（四 topic 订阅/defer 退订、初始组装失败 500（写头前）、组装期 drain 判脏、snapshot 首帧立即 flush、`pushUpdate` 语义（组装/序列化失败保持 dirty 记日志不闭连、仅成功写出并 flush 后清 dirty）、溢出置脏 + 窗口外重推、窗口 tick、心跳业务帧重置与 tick 兼 dirty 重试、`ctx.Done()` 退出）；场景注入点收敛为快照组装函数 + 判脏函数（+ 日志前缀/500 文案）；`writeSSEFrame`/`sseIntervals` 保持共享
- [x] 3.2 `handleActiveSessionsStream` 改为薄封装（组装器 `buildActiveSessionsSnapshot` + 过滤 `eventDirtiesActiveSessions`）；重构门槛：既有 `internal/api/sessions_stream_test.go` 全套原样通过（零行为变化）

## 4. 新端点 GET /api/v1/projects/stream

- [x] 4.1 新增 handler `handleProjectsStream`（注入 `buildProjectsSnapshot` + `eventDirtiesProjectsTaskTree`）与字面路由 `GET /api/v1/projects/stream`；路由锁定测试：`/projects/stream` 走 SSE（`text/event-stream`）、`/projects/{realID}` 仍走详情，互不串扰
- [x] 4.2 Go 测试镜像 active 流关键面 + 差异面：首帧快照与 REST `/projects` 逐字段同构（含 `tasks` 摘要 `agentStatus` omitempty、`attention_count`；空为 `[]` 非 `null`）、订阅先于组装补 update、初始组装 500 不写 SSE 头、事件驱动 update（`task.created`/非 active 迁移/非 active 删除触发）、500ms 窗口合并、组装失败保持 dirty 由窗口/心跳重试不闭连、溢出先置脏再窗口外重推、心跳与业务帧重置、401 拒绝、客户端断开/进程 ctx 取消退订归零且 Shutdown 预算内退出、推送路径无 opencode 调用与无写副作用断言、帧 `event:` 仅 snapshot/update（内部 Type/Payload 不外发）

## 5. 前端 projects store 订阅化

- [x] 5.1 `web/src/sse.ts` 参数化复用：把 fetch/分帧解析/退避/401/`close()` 永久终态机制抽为按「订阅路径 + 帧数据解析校验」参数化的通用入口；`subscribeActiveSessions` 保持既有行为（回归既有 sse 测试），新增 projects 订阅入口，不复制解析/退避循环
- [x] 5.2 `web/src/hooks.ts` 共享 store：创建时建立 `/api/v1/projects/stream` 订阅（`snapshot`/`update` 整表替换 store 快照；失败保留旧数据进 error 通道；401 走既有未授权流程）；定时代价 5s → 30s 常驻兜底轮询（single-flight `pollOnce` 保持）；`refresh()` trailing 语义与 waiter 队列保持不变；AppShell/侧栏/指挥中心/项目管理页组件不改
- [x] 5.3 web 测试：store 帧驱动整表替换、不存在固定 5s 轮询断言（兜底周期 30s）、`refresh()` trailing 补发语义回归、订阅断线退避重连且首帧重置、401 停止重连、卸载/单例生命周期清理；sse 参数化后既有 sessions 订阅测试全绿

## 6. 规格收尾与全量验证

- [x] 6.1 旧路径族回归：`/api/v1/sessions/active/stream` JSON 404（不订阅）；`/api/v1/sessions/active` 别名与 `/api/v1/tasks/active` 响应一致；`/api/v1/tasks/active/stream` 与 `/api/v1/projects/stream` 正常工作
- [x] 6.2 全量：`go build ./... && go test ./... -race` 全绿；web `tsc`/vitest 全绿
- [x] 6.3 e2e smoke（对齐 sse-active-sessions P2.8 口径）：启动服务端——订阅 `/api/v1/projects/stream` 收首帧 snapshot（与 REST `/projects` 同构）；触发任务生命周期事件（创建任务、挂起、恢复、归档）后在 500ms 合并窗口后收 update；外部注册一个新项目后在一个兜底周期（30s）内经前端 store 收敛；杀连接观察前端退避重连；关停服务观察两条 SSE 连接及时释放、订阅归零
- [x] 6.4 openspec 收尾：`openspec validate --changes` 与 `openspec status --change projects-stream`（全 artifact done）；确认前一变更文档保持 HEAD 版本（未被本实现改写）
