# Task Detail Stream Specification

## Purpose

任务详情实时推送能力：`GET /api/v1/tasks/{id}/stream` 单实体 SSE 读模型流（按 taskID 过滤事件、snapshot + update 全量帧、删除关流语义），共享流核心的"主体消失"扩展，以及前端任务详情页由轮询切换为 SSE 订阅的行为。

## ADDED Requirements

### Requirement: 任务详情 SSE 推送端点

系统 SHALL 提供 `GET /api/v1/tasks/{id}/stream` 端点，鉴权方式与其他 `/api/v1/*` 管理 API 一致（Bearer token）。端点 MUST 始终注册（不随事件订阅端口是否注入而变化）；事件订阅端口未注入时请求 MUST 返回 500 标准错误信封（客户端按可重试错误退避重连），使该路径的 404 唯一表示"任务不存在"。端点 MUST 以 `text/event-stream` 推送，复用共享读模型流核心（`runReadModelStream`）的建连时序、500ms 合并窗口、25s 心跳、溢出自愈与退出纪律，MUST NOT 平行复制循环逻辑。所有数据帧（`event: snapshot` 与 `event: update`）的 data MUST 为与 REST `GET /api/v1/tasks/{id}` 响应体同构的**单个任务详情对象**（非裸数组）；心跳 MUST 为注释行 `: ping`。MUST NOT 把内部事件 Type 名用作 SSE `event:`，MUST NOT 发送领域 Payload、增量 diff 或 error 事件帧。

任务详情对象字段 MUST 与 `taskRowDTO` 一致：`id`、`project_id`、`name`、`branch`、`status`、`worktree_path`、`created_at`、`updated_at`、`init_status`、`project_kind`（必有）；`last_port`、`last_error`、`notice`、`delete_mode`、`init_error`、`sessions`、`agentStatus`（按 omitempty 省略）；`attention`（必有，`{permissions[], questions[]}`，空数组非 `null`）。组装 MUST 复用与 REST 详情端点相同的 DTO 组装逻辑，唯一差异：`agentStatus` MUST 读内存快照（`AgentStatusSnapshot`），MUST NOT 实时探测（`AgentStatus`）。推送路径 MUST 为纯读操作，MUST NOT 发起任何 opencode 调用，MUST NOT 产生任何写副作用。

场景事件消费过滤 MUST 按以下决策表判定（优先级自上而下，先匹配先生效）。已知 Type 的合法 Topic/Payload 类型以 `internal/domain/event/event.go` 逐字为准：`task.created`/`task.activity_changed`/`resync.requested` → Payload `struct{}{}`；`task.status_changed` → `TaskStatusChangedPayload`；`task.deleted` → `TaskDeletedPayload`；`session.claimed`/`touched`/`deleted` → `SessionOwnerPayload`；`sessions.aligned` → `SessionsAlignedPayload`；`serve_runtime.attention_changed` → `ServeRuntimeTaskPayload`；`serve_runtime.run_status_changed` → `ServeRuntimeRunStatusChangedPayload`。任一已知 Type 的 Topic 或 Payload 类型与此不符即为畸形事件：

| 事件形态 | 判定 |
|---|---|
| 未知 Type（任何 topic） | 标脏（保守自愈） |
| 已知 Type 但 Topic 或 Payload 类型不合法（畸形事件） | 标脏（保守自愈） |
| `task.created`/`task.status_changed`/`task.deleted`/`task.activity_changed` | RID==该 taskID → 标脏，否则不脏 |
| `sessions.aligned` | RID==该 taskID → 标脏，否则不脏 |
| `session.claimed`/`session.touched`/`session.deleted` | Payload `SessionOwnerPayload.TaskID`==该 taskID → 标脏，否则不脏 |
| `serve_runtime.attention_changed` | Payload `ServeRuntimeTaskPayload.TaskID`==该 taskID → 标脏，否则不脏 |
| `serve_runtime.run_status_changed` | Payload `ServeRuntimeRunStatusChangedPayload.TaskID`==该 taskID → 标脏，否则不脏 |
| `resync.requested` | 一律标脏 |

合法且不关联本 taskID 的事件 MUST NOT 标脏、MUST NOT 触发重组装（"不关联"判定不适用于未知/畸形事件——它们恒标脏）。

#### Scenario: 连接即收快照

- **WHEN** 已认证客户端对已存在的任务建立 SSE 连接
- **THEN** 客户端立即收到一帧 `snapshot`，data 为该任务详情对象（字段与 REST 详情响应同构）

#### Scenario: 事件驱动更新

- **WHEN** 连接存续期间该任务发生状态迁移、可见字段变更（含 `init_status`/`init_error` 经扩展后的 `task.activity_changed`）、session 归属变化或 attention/agentStatus 变化
- **THEN** 客户端在合并窗口到期后收到一帧 `update`，data 为变更后的最新任务详情对象

#### Scenario: 他任务事件不触发推送

- **WHEN** 连接存续期间到达的事件均为合法已知 Type 且 RID/Payload 均不关联本 taskID
- **THEN** 不触发重组装，不发送 `update` 帧

#### Scenario: 窗口内多次变更合并

- **WHEN** 500ms 合并窗口内到达多个关联本任务的标脏事件
- **THEN** 客户端仅收到一帧 `update`，data 为窗口到期时刻的全量快照

#### Scenario: 未认证访问被拒

- **WHEN** 请求缺失或携带错误 token
- **THEN** 返回 401，不建立事件流，不泄露任何资源信息

#### Scenario: 推送无写副作用且无实时探测

- **WHEN** SSE 连接存续并发生多次推送
- **THEN** 数据库内容、任务状态机与进程集合除外部因素外不发生变化；推送路径不发起任何 opencode 调用，`AgentStatus` 实时探测调用次数为 0

### Requirement: 任务不存在与删除语义

初始组装时任务不存在，端点 MUST 返回 JSON 404 标准错误信封（`error.code=not_found`、`error.message=task not found`；MUST NOT 写 SSE 响应头、MUST NOT 建立事件流——订阅在组装前建立时 MUST 退订释放）。API 层的 404/405 统一改写（`jsonNotFoundHandler`）MUST 保留 handler 已写入的标准 JSON 错误信封原样转发，仅对 mux 默认裸文本 404/405 重写。连接存续期间任务被删除（`task.deleted` RID==该 taskID 标脏后重组装未命中，或任何一次重组装返回任务不存在）时，服务端 MUST 关闭该 SSE 连接（正常退出 handler、释放订阅），MUST NOT 保持脏标记无限重试，MUST NOT 发送 error 帧。除任务不存在外的组装失败 MUST 沿用共享核心语义：跳过本次发送、保持脏标记、由后续事件或心跳 tick 重试，连接保持。**流侧组装失败范围含 `Get` 与 `ListTaskSessions` 查询失败**（与 REST 详情端点有意不同：REST 对 `ListTaskSessions` 失败降级忽略，流侧 MUST 作为组装失败处理——纯订阅模式下吞错省略 `sessions` 可能长期不自愈）。

#### Scenario: 初始不存在返回 JSON 404

- **WHEN** 已认证客户端对不存在的 taskID 请求 `GET /api/v1/tasks/{id}/stream`
- **THEN** 返回 JSON 404 标准错误信封（`error.code=not_found`、`error.message=task not found`，非 SSE 响应），不写入 `text/event-stream` 头，事件订阅被释放

#### Scenario: 流期间任务被删除

- **WHEN** 连接存续期间该任务被删除
- **THEN** 服务端关闭 SSE 连接并释放订阅，不发送 error 帧，不无限重试组装

#### Scenario: 非不存在类组装失败保持连接

- **WHEN** 某次 update 组装因底层查询失败（非任务不存在）
- **THEN** 该帧被跳过并记录日志，脏标记保持，连接保持，后续事件或心跳 tick 触发重试

### Requirement: 前端任务详情订阅与重连

任务详情页 MUST 通过 fetch + ReadableStream 的 SSE 订阅（携带 `Authorization: Bearer`）获取任务详情，MUST NOT 再对 `GET /api/v1/tasks/{id}` 做固定间隔轮询（现有 2s/4s `usePoll` 移除）。订阅 MUST 随页面挂载建立、卸载关闭（含在途退避计时器取消），taskID 变化时 MUST 关闭旧订阅并按新 taskID 重建。`snapshot` 与 `update` 帧 MUST 走同一解析路径并以帧内任务对象替换页面任务状态；帧 data 校验谓词 MUST 为 `typeof data === 'object' && data !== null && !Array.isArray(data)`（仅校验信封形状，不做字段级 schema 校验；null、数组、primitive 均判协议错误），协议错误沿用通用订阅器语义：保留旧数据、终止当前连接、退避重连且不重置退避。首帧到达前（任务状态为 null 且未进入 not-found）MUST 展示固定「连接中…」状态，MUST NOT 闪现空态；首次连接失败 MUST 展示错误提示并继续退避重连；推送/订阅异常期间 MUST 保留上次成功数据并展示错误提示。断线或非 401/404 错误 MUST 按既有指数退避自动重连（1s 起步、上限 30s、首个有效帧重置）；401 沿用清 token + 广播未授权事件且不再重连。HTTP 404 响应 MUST 视为永久终态：进入现有 not-found 展示、关闭连接、MUST NOT 再重连或轮询（任务删除后服务端关流 → 客户端重连 → 404 → not-found 是该语义的完整链路）。任务操作成功后的刷新 MUST 由流推送承接，MUST NOT 保留操作后手动 `load()` 的补偿性 REST 调用；`GET /api/v1/tasks/{id}` REST 端点本身保留不变（供非流场景使用）。

#### Scenario: 订阅替代轮询

- **WHEN** 用户打开任务详情页
- **THEN** 页面建立一条 SSE 订阅，首帧后展示任务详情，不存在对 `GET /api/v1/tasks/{id}` 的定时轮询请求

#### Scenario: 断线自动重连

- **WHEN** SSE 连接因网络或服务端原因断开（任务仍存在）
- **THEN** 页面按指数退避自动重连，重连成功收到 `snapshot` 后收敛，期间保留上次成功数据并提示错误

#### Scenario: 任务删除进入 not-found

- **WHEN** 连接存续期间任务被删除（服务端关流），客户端按退避重连收到 HTTP 404
- **THEN** 页面进入 not-found 展示，关闭订阅且不再发起任何请求

#### Scenario: 初始 404 进入 not-found

- **WHEN** 用户打开一个不存在任务的详情页
- **THEN** 首次请求收到 HTTP 404，页面进入 not-found 展示，不进入退避重连

#### Scenario: 卸载释放连接

- **WHEN** 用户离开任务详情页（含正处于重连退避等待期间）
- **THEN** SSE 连接被关闭、在途退避计时器被取消、不再发起任何 fetch，服务端订阅随之释放

#### Scenario: 切换任务重建订阅

- **WHEN** 用户在详情页切换到另一 taskID（App 路由以 `key={taskID}` 重挂载页面组件，组件状态整体重置）
- **THEN** 旧订阅随卸载被关闭，按新 taskID 建立新订阅并以新快照收敛
