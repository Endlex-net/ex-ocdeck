# Design: 项目/侧栏数据面 SSE 推送与端点 task 命名对齐

## Context

前一变更 sse-active-sessions（同分支、已完成待归档）已交付：进程内领域事件总线（4 topic：`task`/`session`/`serve_runtime`/`control`，11 Type；已实现于 sse-active-sessions）、application 集中 commit helper、agentStatus 内存快照（P1.8，MODE A 事件驱动，连接代 `aligning → reconcilePending → valid` 模型，快照访问方法 `AgentStatusSnapshot(taskID)`）、共享快照组装 helper（`buildActiveSessionsSnapshot`）、SSE 端点与统一写路径（`writeSSEFrame`、`statusRecorder` FlushError/Unwrap、`http.Server.BaseContext` 关停先取消 stream 再 Shutdown）、前端 fetch streaming 订阅客户端（`web/src/sse.ts`：Bearer、分帧解析、指数退避、401 停连、`close()` 永久终态）。

当前 projects/侧栏数据面的事实（已核实）：

- `internal/api/projects.go` `handleListProjects`/`handleGetProject` 对全部 active 任务摘要做 agentStatus **实时水合**（goroutine fan-out、信号量 8、3s 预算）——本变更 Item A 移除该链，改读内存快照。
- `web/src/hooks.ts` 共享 projects store 为 5s single-flight 轮询 + trailing `refresh()`（变更操作成功后补发一次）；壳层侧栏、指挥中心与项目管理页消费同一 store（web-ui-shell spec「单一数据源」约束）。
- 项目 CRUD（创建/重命名/删除项目）与项目 env 等操作**没有领域事件**——事件目录只覆盖任务域，本变更不扩展生产侧。
- 路由基座：Go stdlib `http.ServeMux`（1.22+ 模式匹配），已注册 `GET /api/v1/projects/{id}`（projects.go:29）。
- **在树部分实现**：aborted lane 已在未提交状态下落实了 Item A（projects.go 快照化 + attention_api_test.go 改写）与 Item C（tasks.go 路由、注释与两侧测试、web 路径字符串）的大部分，并对 `openspec/changes/sse-active-sessions/*.md` 做了 4 处路径改写。tasks 以「复核」项逐文件承接，§1 复核已于 2026-08-26 执行完成（逐项 PASS）；前一变更文档的在树改写已按既定决策回退（前一变更冻结，新路径由本变更 deltas 承载）。

## Goals / Non-Goals

**Goals:**

- **Item C**：端点族 task 中心命名——`/api/v1/tasks/active`（canonical）+ `/api/v1/sessions/active`（兼容别名，同一 handler）；`/api/v1/tasks/active/stream`（旧流路径 404，无别名）；前端路径字符串与测试同步。
- **Item A**：`/projects` 的 agentStatus 改读 `AgentStatusSnapshot`，移除实时水合链；降级语义与 DTO 字节级不变；`/tasks/{id}` 保持实时探测（用户批准的显式不变项）。
- **Item B**：新增 `GET /api/v1/projects/stream` SSE 端点（快照与 `/projects` REST 完全同构；SSE 纪律与活跃会话流一致；消费过滤按全状态任务树场景定义）；SSE 循环核心从 `sessions_stream.go` 抽取共享，MUST NOT 平行复制；前端 projects store 改为流订阅 + 常驻低频兜底轮询。

**Non-Goals:**

- 不新增任何领域事件 Type/topic（project CRUD 无事件；生产侧扩展另行变更）；事件目录与 commit helper 不动。
- 不改 `/tasks/{id}` 的 agentStatus 实时探测语义；不改存量内部调用链（suspend/attach/reconcile/delete 的会话环境读取）。
- 不删除 `/api/v1/sessions/active` 兼容别名（兼容与调试用途，沿用前一变更 Non-Goals）；不为未发布的旧流路径 `/api/v1/sessions/active/stream` 提供别名或迁移。
- 不引入 WebSocket、不改认证模型、无 DB schema 变更、无新增第三方依赖。
- AppShell/侧栏/指挥中心/项目管理页组件不改（仍读共享 store）；双快照 join 规则不变。

## Decisions

### D1: 端点族 task 中心命名（Item C）

- **REST**：canonical `GET /api/v1/tasks/active`，旧路径 `GET /api/v1/sessions/active` 注册为**同一 handler**（`handleListActiveSessions`）的兼容别名，响应（状态码/头/体）完全一致。别名长期保留：它是已发布路径（cross-project-active-sessions 起），兼容与调试用途。
- **SSE 流**：仅注册 `GET /api/v1/tasks/active/stream`。旧路径 `/api/v1/sessions/active/stream` 由前一变更引入且**尚未发布**，无兼容负担——不注册即落入 `jsonNotFoundHandler` 返回 JSON 404 信封（不写 SSE 头、不订阅事件，需测试锁定）。
- **命名理由**：资源本体是任务（`tasks` 表 / Task 聚合），sessions 是任务的运行投影；与 `/api/v1/tasks/{id}` 等既有任务端点族对齐后，active 列表与流都以任务为中心。`/api/v1/projects/stream`（Item B）同理以 projects 为主体（项目集合 + 全状态任务树投影）。
- **前端**：`web/src/api.ts` `listActiveSessions` 改请求 `/tasks/active`；`web/src/sse.ts` `STREAM_PATH` 改 `/api/v1/tasks/active/stream`；两侧测试同步路径字符串，并补「别名响应一致」「旧流路径 404」两类断言。
- **文档口径**：前一变更（已完成）的 design/spec 文档不改写；本变更的 spec deltas 承载全部新路径（含对前一变更 ADDED requirement 的 MODIFIED delta）。在树 aborted lane 对前一变更文档的 4 处改写 MUST 回退为 HEAD 版本。

### D2: /projects agentStatus 读内存快照（Item A）

- `handleListProjects`/`handleGetProject` 的任务摘要组装中，active 摘要同步读 `s.tasks.AgentStatusSnapshot(taskID)`（前一变更 P1.8.5 引入的独立快照访问方法），在 DTO 组装循环内内联完成——无 goroutine、无信号量、无 3s 预算；`hydrateProjectTaskAgentStatuses`/`hydrateSingleProjectAgentStatuses`/`runAgentStatusHydration` 整体删除。
- **降级语义不变**：快照不可用（断流、对账失败、尚不存在）返回空串，经 `agentStatus,omitempty` 省略；DTO 字段集合与序列化字节级不变。
- **`/tasks/{id}` 显式不变**：`handleGetTask` 继续走 `AgentStatus(ctx, taskID)` 实时探测（任务详情是单任务深链场景，实时性优先；用户 2026-08-26 批准的不变项）。
- **行为变更披露（落入 project-management delta）**：`/projects` 的 agentStatus 可能滞后于对账周期（模式 A 下由 opencode SSE 状态事件与对账维护）；断流即省略，不展示陈旧值。
- **为什么不保持实时水合**：推送化后 `/projects` 组装进入流重组装热路径（Item B 的 500ms 窗口内重组装），实时探测（每任务一次 tmux 环境读取 + opencode HTTP）在该路径不可接受；内存快照已有合规维护模型与降级语义，`/api/v1/tasks/active` 已验证同模式（前一变更 P2.2）。

### D3: 新端点 `GET /api/v1/projects/stream`（Item B）

- **快照组装**：抽 `buildProjectsSnapshot(ctx) ([]projectDTO, error)`——projects 列表 + `ListProjectTaskSummaries` + 逐项目 counts + 分组 + 摘要 DTO（agentStatus 读快照），REST handler 与 SSE handler 共用，保证帧与 REST 响应体完全同构。store 失败在初始组装阶段（写 SSE 头之前）退订并返回 500 标准错误信封；无项目/无任务时为 `[]` 非 `null`。
- **路由**：以字面路径 `GET /api/v1/projects/stream` 注册。Go 1.22+ `ServeMux` 中字面段优先于 `/api/v1/projects/{id}` 通配，`stream` 不会被当作项目 ID 命中 `handleGetProject`——需路由锁定测试（正反两向：`/projects/stream` 走流、`/projects/{realID}` 走详情）。
- **SSE 纪律**：与 `/api/v1/tasks/active/stream` 完全一致（复用 D5 共享核心）：先订阅 `task`/`session`/`serve_runtime`/`control` 四 topic 并 fan-in、再组装初始快照；组装期间 drain 判脏；写 200 + SSE 头、`event: snapshot` 首帧立即 flush；固定 500ms 合并窗口；溢出先置脏再窗口外重推；心跳默认 25s 且**任一成功写出的业务帧后重置**；统一 `writeSSEFrame`（Write + `http.NewResponseController.Flush()`）；任何写/flush 失败立即退订退出；客户端断开或进程 ctx 取消释放订阅（BaseContext 方案沿用）。
- **帧协议**：对外仅 `event: snapshot` / `event: update`（data 为 projectDTO 裸数组）与 `: ping` 注释行；MUST NOT 外发内部 Type/Payload、增量 diff 或 error 帧；鉴权失败走 HTTP 401。
- **推送路径纯读**：不实时调用 opencode、无 store 写副作用（agentStatus 已是内存快照读）。

### D4: projects 场景消费过滤（与指挥中心 active-only 过滤的差异）

| 领域事件 | 指挥中心流（active-only，已实现） | projects 流（本变更，全任务树） |
|---|---|---|
| `task.created` | 否（只影响 projects 树） | **是**（树新增行） |
| `task.status_changed`（两端均非 active） | 否 | **是**（挂起↔归档、creating→creation_failed、→deleting 等过渡/失败迁移都改变树呈现） |
| `task.status_changed`（跨越 active 边界） | 是 | 是 |
| `task.deleted`（from 非 active） | 否 | **是**（删除挂起/归档任务也改树） |
| `task.activity_changed` | 是 | 是（notice/last_error/updated_at 影响摘要、排序与呈现） |
| `session.*` / `sessions.aligned` | 是 | 是（`last_active_at` 回退字段、attention_count） |
| `serve_runtime.*` | 是 | 是（agentStatus 状态点、attention_count） |
| `resync.requested` | 是 | 是（强制重拉全量） |
| 未知 `Type` | 是（保守） | 是（保守） |

- **差异理由（落入 projects-stream delta）**：侧栏/项目管理页呈现**全部非删除态任务**（含挂起、归档、过渡与失败态），任一任务的进入/迁移/离开/字段变化都改变该投影；指挥中心活跃流只关心 active 集合成员及其投影。两过滤表各自独立定义，互不覆盖。
- **实现**：新增纯函数 `eventDirtiesProjectsTaskTree(ev)`，与 `eventDirtiesActiveSessions`（sessions_filter.go）同层并列；`task.*` 闭集分支全部返回 true，default（`session.*`/`serve_runtime.*`/`resync.requested`/未知）标脏；不解读 Payload 做增量合并。标脏后一律重组装全量快照重推。
- **项目 CRUD 无事件（已记录的限制）**：项目创建/重命名/删除不产生领域事件（本变更不扩展生产侧），该类变更由前端低频兜底轮询覆盖（D6），流内不感知。

### D5: SSE 循环共享核心抽取（MUST NOT 平行复制，fb-35）

- 从 `sessions_stream.go` 的 handler 循环（订阅/defer 退订 → 初始组装失败 500（写头前）→ 组装期 drain 判脏 → snapshot 首帧 → 事件循环）抽共享核心（如 `runReadModelStream(w, r, deps)`，命名实现期定）。**场景参数**仅三个：快照组装函数、判脏函数、日志前缀/500 文案；**固定**部分：四 topic 订阅与 fan-in、`pushUpdate` 语义（组装/序列化失败保持 dirty 记日志不闭连；仅成功写出并 flush 后清 dirty）、溢出置脏 + 窗口外重推、窗口 tick、心跳语义（业务帧后重置、tick 兼 dirty 重试、静默期 `: ping`）、`ctx.Done()` 退出。`writeSSEFrame`/`sseIntervals` 已是共享件，保持。
- 两个 handler 变薄封装：`handleActiveSessionsStream = core(buildActiveSessionsSnapshot, eventDirtiesActiveSessions)`；`handleProjectsStream = core(buildProjectsSnapshot, eventDirtiesProjectsTaskTree)`。
- **重构门槛**：既有 `sessions_stream_test.go` 全套（建连状态机、窗口、溢出、心跳、写失败、断连/关停）原样通过，零行为变化。
- **备选（否决）**：复制循环后改常量——两处心跳/溢出/退订纪律必然漂移，违反 fb-35（平行重复收敛为共享核心）。

### D6: 前端 projects store 订阅 + 常驻低频兜底轮询

- **sse.ts 参数化**：把 `subscribeActiveSessions` 的 fetch/分帧/退避/401/`close()` 永久终态机制参数化（订阅路径 + 帧数据解析校验），sessions 与 projects 订阅共用同一实现，不复制解析循环。
- **hooks.ts store**：store 创建时建立 `/api/v1/projects/stream` 订阅（单例 store 生命周期 = 应用生命周期），`snapshot`/`update` 帧整表替换 store 快照；保留 `pollOnce` single-flight 机制但定时间隔 5s → **30s**（常驻兜底轮询）；`refresh()` trailing 语义不变（用户主动 CRUD 项目后立即收敛）；订阅与兜底轮询失败保留上次成功数据并进入 store error 通道（沿既有轮询失败语义，侧栏不闪空态）；401 走既有未授权流程。
- **兜底周期取 30s**（用户给定 30-60s 区间下限）：项目集合属低频外部变更，30s 保证可见性上限；任务级变更（`task.*`/`session.*`/`serve_runtime.*`）经流亚秒收敛，不受兜底周期影响；用户主动操作经 `refresh()` 立即收敛，兜底轮询只覆盖流外变更。
- **消费方不改**：AppShell/侧栏、指挥中心、项目管理页继续读同一 store；「应用内不得存在第二个 `/projects` 轮询」约束保持（兜底轮询属于 store 内部，single-flight 不变）。

## Risks / Trade-offs

- [在树部分实现未经审查（aborted lane 遗留）] → tasks 以「复核」项逐文件核对（路由/别名/404/路径字符串/快照化/测试改写），并回退前一变更文档的在树改写；复核后才进入新开发项。
- [/projects agentStatus 滞后于对账周期（用户已批准的行为变更）] → spec 披露；断流即省略、不展示陈旧值；`/tasks/{id}` 保持实时，单任务深链场景不受影响。
- [项目 CRUD 无事件，依赖兜底轮询（最长 ~30s 延迟）] → 用户主动操作经 `refresh()` 立即收敛；兜底周期 30s；后续如引入 project 领域事件另走规格变更。
- [每页两条 SSE 长连接（tasks/active/stream + projects/stream）] → 本地单用户工具、连接个位数；`http.Server` 无写超时；关停先取消 stream 再 Shutdown（BaseContext 方案沿用），预算内释放。
- [字面 `/projects/stream` 与 `/projects/{id}` 通配并存] → ServeMux 字面段优先；路由锁定测试（正反两向）。
- [共享核心抽取的重构回归] → 既有 sessions stream 测试全套原样通过为门槛；新流测试镜像关键面 + 过滤差异行。

## Open Questions

- 无——设计已由用户 2026-08-26 定案；实现期仅剩共享核心函数命名、过滤函数文件归属等机械决策。
