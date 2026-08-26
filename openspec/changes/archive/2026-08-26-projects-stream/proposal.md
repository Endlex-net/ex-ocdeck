# Proposal: 项目/侧栏数据面 SSE 推送与端点 task 命名对齐

## Why

sse-active-sessions（同分支、已完成待归档）已建成推送基础设施：进程内领域事件总线（4 topic / 11 Type）、LifecycleService commit helper、agentStatus 内存快照、共享快照组装 helper 与指挥中心 SSE 端点。但 projects/侧栏数据面仍是 5s 轮询 `GET /api/v1/projects`，且每次请求对全部 active 任务**实时探测** agentStatus（并发 8、3s 预算）——数据滞后最高一个轮询周期、服务端开销按请求线性增长。同时活跃任务端点族以 `sessions` 命名任务本体（`/api/v1/sessions/active`），与 `/api/v1/tasks/*` 既有任务端点族不一致。本变更把推送模型扩展到 projects/侧栏面，并对齐端点命名；除注明项外不改事件生产侧。

## What Changes

- **Item C（端点族 task 中心命名）**：**BREAKING**（仅影响前一变更引入、尚未发布的流路径）——`GET /api/v1/sessions/active/stream` 改为 `GET /api/v1/tasks/active/stream`，旧流路径 MUST NOT 保留别名（返回 JSON 404）。`GET /api/v1/sessions/active` 的 canonical 改为 `GET /api/v1/tasks/active`，旧路径保留为兼容别名（同一 handler、同一响应；兼容与调试用途，沿用前一变更 Non-Goals 决策）。前端 `web/src/api.ts` / `web/src/sse.ts` 路径字符串与两侧测试同步；前一变更（已完成）的 design/spec 文档不改写，新路径全部由本变更的 spec deltas 承载。
- **Item A（/projects agentStatus 由实时探测改为内存快照；用户 2026-08-26 批准的行为变更）**：项目列表/详情的任务摘要 agentStatus 水合（goroutine fan-out、并发 8、3s 预算）移除，active 摘要改读 `AgentStatusSnapshot(taskID)`（与前一变更 `/api/v1/tasks/active` 同模式）；降级语义不变（快照不可用 → `omitempty` 省略），DTO 字节级不变。`/tasks/{id}` 显式保持实时探测不变。行为差异披露：`/projects` 的 agentStatus 可能滞后于对账周期。
- **Item B（新增 `GET /api/v1/projects/stream`）**：快照帧 data 与 `GET /api/v1/projects` 响应体完全同构（projectDTO 裸数组，含任务摘要的 snapshot `agentStatus` 与 `attention_count`）；SSE 纪律与活跃会话流一致（先订阅再组装、500ms 合并窗口、溢出窗口外重推、业务帧后重置心跳、统一 `writeSSEFrame`、写失败退订退出）。消费过滤与指挥中心流**不同**——侧栏需要全状态任务树：全部 `task.*`（含 `task.created` 与不跨越 active 边界的 `task.status_changed`、任意 `from` 的 `task.deleted`）、`session.*`、`serve_runtime.*`、`resync.requested`、未知 Type 均标脏。SSE 循环骨架 MUST 从 `sessions_stream.go` 抽取共享核心，MUST NOT 平行复制。
- **前端 projects store 改造**：`web/src/hooks.ts` 共享 store 的 5s 轮询改为 projects/stream 订阅 + 常驻低频兜底轮询（固定 30s）——项目 CRUD（创建/重命名/删除）无领域事件，本变更**不新增**任何 project 事件（生产侧扩展不在范围），兜底轮询覆盖项目集合变更；`refresh()` trailing 语义保持。AppShell/侧栏组件不改（仍读 store）。

## Capabilities

### New Capabilities

- `projects-stream`: projects/侧栏场景的 SSE 投影——`GET /api/v1/projects/stream` 端点协议、全状态任务树消费过滤、SSE 循环核心复用约束、前端订阅与低频兜底轮询。

### Modified Capabilities

- `active-sessions-overview`: 「活跃会话列表 API」canonical 路径改为 `/api/v1/tasks/active`（旧路径为兼容别名、响应一致）；「水合调用链 context 收敛」中 `/projects` 从实时探测链移出、改读内存快照（`/tasks/{id}` 保持实时）。
- `active-sessions-stream`（sse-active-sessions 新建，随其归档进入主 specs）: 「活跃会话 SSE 推送端点」路径改为 `/api/v1/tasks/active/stream` 且不留旧路径别名；REST 同构引用、「agent 状态事件驱动维护」与「前端 SSE 订阅与重连」中的路径同步，并标注 `/projects` 转为快照消费者。
- `command-center`: 「指挥中心首页」数据源描述中 sessions 流路径同步为 `/api/v1/tasks/active/stream`；projects store 从「5s single-flight 轮询」改为「SSE 订阅 + 低频兜底轮询」。
- `project-management`: 「项目列表与详情」的 agentStatus 语义由「请求路径并发实时水合（上限 8 / 3s）」改为「读内存快照、请求路径不实时探测」，含滞后披露。
- `web-ui-shell`: 「全局应用壳层与侧栏导航」中侧栏数据源从「5s single-flight 轮询 store」改为「SSE 订阅 + 低频兜底轮询 store」，单一数据源约束保持。

## Impact

- **修改代码**：`internal/api/tasks.go`（路由 canonical/别名/流路径）、`internal/api/sessions_stream.go`（循环核心抽取）、`internal/api/projects.go`（快照化组装共享）、`internal/api/sessions_filter.go`（新增 projects 场景过滤，或同层新文件）、新增 projects 流 handler；`internal/application/dto.go`、`internal/infrastructure/store/queries.go`（注释路径）；`web/src/api.ts`、`web/src/sse.ts`（路径 + 客户端参数化）、`web/src/hooks.ts`（store 订阅化）。
- **测试**：`internal/api`（别名一致性、旧流路径 404、projects 快照化、projects 流全套）；web vitest（sse/hooks/store）。
- **无变更**：DB schema、认证模型、事件目录（不新增任何领域事件 Type/topic）、事件生产侧。
- **在树状态**：Item A/C 已有 aborted lane 的部分实现（未提交），tasks 以「复核」项承接且 §1 复核已于 2026-08-26 执行完成（结构/测试/前端路径逐项 PASS，无需功能性修正）；该 lane 对 `openspec/changes/sse-active-sessions/*.md` 的在树改写已按既定决策回退（前一变更文档冻结，新路径由本变更 deltas 承载）。
