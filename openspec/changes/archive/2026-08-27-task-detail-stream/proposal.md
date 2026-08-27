# Proposal: task-detail-stream

## Why

任务详情页（`TaskWorkbenchPage`）目前通过 `usePoll` 每 2s（过渡态/init 运行中）或 4s（稳定态）持续轮询 `GET /api/v1/tasks/{id}`，只要页面打开就永不停止，产生源源不断的请求。项目已完成一版 SSE 改造（active-sessions-stream、projects-stream），共享流核心 `runReadModelStream` 与前端通用订阅器 `subscribeStream` 均已就绪。详情页字段变化中 status/activity/session/attention/agentStatus 均由领域事件驱动；init_status/init_error 写入当前被 P1.6.1 冻结为不发布事件（当时是 scope 裁剪，设计已留"后续场景需要再扩展"的口子），本变更扩展事件契约覆盖之。

## What Changes

- **事件契约扩展（推翻 P1.6.1）**：`task.activity_changed` 扩展为**所有未伴随 status 迁移的任务行非 status 真实变更**（运行期逐任务 `MutationResult` 提交，Changed=true）后发布，不再要求 `updated_at` 跨秒推进，并显式接入 `ClaimInitRun`/`ClaimInitRerun`/`FinishInitRun` 三个 init 提交点；`env_snapshot` 等不直接透出 DTO 的字段仅作保守失效通知；失败、未命中、无变化路径 MUST NOT 发布；仍 MUST NOT 与 `task.status_changed` 同事务重复发布。启动期 `ConvergeInterruptedInitRuns`（HTTP 开放前执行、零订阅者）保持不发布，属明确例外。
- 新增 SSE 读模型流端点 `GET /api/v1/tasks/{id}/stream`：单实体投影，按事件目录分别以 RID 或 Payload `task_id` 关联本 task 标脏（未知/畸形事件与 `resync.requested` 恒脏，决策表见 design D2），复用 `runReadModelStream` 共享核心（500ms 合并窗口、25s 心跳、snapshot + update 全量帧、溢出自愈）。
- 快照内容与现有 `handleGetTask` 的 `taskRowDTO` 同构（单个对象而非裸数组）：含 status / init_status / notice / last_error / sessions / agentStatus / attention 等全部详情字段。
- 任务不存在或被删除时服务端关闭流：初始组装时任务不存在返回 404 标准错误信封（非 SSE）；流期间收到关联 `task.deleted` 标脏后重组装未命中（not-found）时关闭连接。
- 前端 `TaskWorkbenchPage` 新增 `subscribeTask(id)`（复用 `subscribeStream`），替换 `usePoll` 轮询；断线期间依赖既有指数退避重连，重连后 snapshot 全量自愈，不加兜底轮询；流关闭且任务已删除时走现有 notFound 逻辑。
- 移除详情页 `usePoll` 对 `api.getTask` 的周期性调用；`load()` 的一次性调用（初始加载/操作后刷新）语义由 snapshot/update 帧承接。

非目标（明确不做）：

- 不改造 `ServerStatusBanner` 的 30s 轮询（无事件源，低频探测合理保留）。
- 不动 projects store 的 30s 兜底轮询（规格强制，项目 CRUD 无领域事件）。
- 不引入 last-event-id / 事件重放（沿用现有流"全量快照自愈"语义）。
- 不改 `runReadModelStream` 共享核心的行为（仅新增场景注入）。

## Capabilities

### New Capabilities

- `task-detail-stream`: 任务详情 SSE 读模型流——按 taskID 过滤的单实体实时投影，含协议帧格式、事件过滤、删除/不存在语义与前端订阅行为。

### Modified Capabilities

- `active-sessions-stream`: 「事件类型与载荷结构」中 `task.activity_changed` 的发布条件变更——移除"init_status/init_error 写入 MUST NOT 触发"的排除条款，发布条件从 `Changed && UpdatedAtAdvanced` 放宽为**运行期逐任务 `MutationResult` 提交中未伴随 status 迁移的非 status 真实变更 `Changed` 即发布**（推翻 P1.6.1 冻结；启动期 `ConvergeInterruptedInitRuns` 不发布断言保留为例外）。「内部事件总线」「活跃会话 SSE 推送端点」等其余 requirement 行为不变；既有两条流会多收到标脏 update 帧，来源含 init 系列写入以及 delete_mode/notice/last_port/同状态 last_error/env_snapshot 等同秒提交（合并窗口吸收，无语义变化）。

## Impact

- 后端：`internal/api/`（新增 `task_stream.go` 薄绑定 + `task_filter.go` 按 taskID 的消费过滤表；`read_model_stream.go` 共享核心新增可选 assembleGone 钩子；`server.go` jsonNotFoundHandler 保留 handler 已写的 JSON 404 错误信封；路由注册于 `tasks.go`）；`internal/application/task/`（init 三个提交点挂接事件发布，解冻 P1.6.1 冻结测试）。
- 前端：`web/src/sse.ts`（subscribeStream 新增 onGone 终态；新增 `subscribeTask`）、`web/src/pages/TaskWorkbenchPage.tsx`（移除 `usePoll`，改订阅驱动）。
- API：新增 `GET /api/v1/tasks/{id}/stream`（`text/event-stream`，始终注册，事件订阅端口未注入时返回 500）；`GET /api/v1/tasks/{id}` 保留不变；API 层 404 响应消息从统一 `no route for ...` 变为保留 handler 标准错误信封（对仅判 status 的客户端无影响）。
- 事件：扩展 `task.activity_changed` 发布条件（见 Modified Capabilities），无新事件类型；`eventSubscriberAdapter`/`SetEventSubscriber` 接线无需改动。
