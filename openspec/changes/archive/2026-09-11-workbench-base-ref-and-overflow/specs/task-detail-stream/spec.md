# Delta: task-detail-stream（workbench-base-ref-and-overflow）

## MODIFIED Requirements

### Requirement: 任务详情 SSE 推送端点

系统 SHALL 提供 `GET /api/v1/tasks/{id}/stream` 端点，鉴权方式与其他 `/api/v1/*` 管理 API 一致（Bearer token）。端点 MUST 始终注册（不随事件订阅端口是否注入而变化）；事件订阅端口未注入时请求 MUST 返回 500 标准错误信封（客户端按可重试错误退避重连），使该路径的 404 唯一表示"任务不存在"。端点 MUST 以 `text/event-stream` 推送，复用共享读模型流核心（`runReadModelStream`）的建连时序、500ms 合并窗口、25s 心跳、溢出自愈与退出纪律，MUST NOT 平行复制循环逻辑。所有数据帧（`event: snapshot` 与 `event: update`）的 data MUST 为与 REST `GET /api/v1/tasks/{id}` 响应体同构的**单个任务详情对象**（非裸数组）；心跳 MUST 为注释行 `: ping`。MUST NOT 把内部事件 Type 名用作 SSE `event:`，MUST NOT 发送领域 Payload、增量 diff 或 error 事件帧。

任务详情对象字段 MUST 与 `taskRowDTO` 一致：`id`、`project_id`、`name`、`branch`、`base_ref`（必有且原样透传落库字符串，字段不省略；正常非空 worktree 任务值为 `refs/heads/<name>` 或 `refs/remotes/<name>`；非 worktree 模式任务与历史空值 worktree 任务为空串——历史空值为合法响应，MUST NOT 因此拒绝详情或回填数据）、`status`、`worktree_path`、`created_at`、`updated_at`、`init_status`、`project_kind`（必有）、`mode`（必有，取值 `worktree`/`local-path`）、`permission_mode`（必有，沿用 task-permission-mode 既有契约，本变更不引入新行为）；`last_port`、`last_error`、`notice`、`delete_mode`、`init_error`、`sessions`、`agentStatus`（按 omitempty 省略）；`attention`（必有，`{permissions[], questions[]}`，空数组非 `null`）。组装 MUST 复用与 REST 详情端点相同的 DTO 组装逻辑，唯一差异：`agentStatus` MUST 读内存快照（`AgentStatusSnapshot`），MUST NOT 实时探测（`AgentStatus`）。推送路径 MUST 为纯读操作，MUST NOT 发起任何 opencode 调用，MUST NOT 产生任何写副作用。

场景事件消费过滤 MUST 按以下决策表判定（优先级自上而下，先匹配先生效）。已知 Type 的合法 Topic/Payload 类型以 `internal/domain/event/event.go` 逐字为准：`task.created`/`task.activity_changed`/`task.user_activity`/`resync.requested` → Payload `struct{}{}`；`task.status_changed` → `TaskStatusChangedPayload`；`task.deleted` → `TaskDeletedPayload`；`session.claimed`/`touched`/`deleted` → `SessionOwnerPayload`；`sessions.aligned` → `SessionsAlignedPayload`；`serve_runtime.attention_changed` → `ServeRuntimeTaskPayload`；`serve_runtime.run_status_changed` → `ServeRuntimeRunStatusChangedPayload`。任一已知 Type 的 Topic 或 Payload 类型与此不符即为畸形事件：

| 事件形态 | 判定 |
|---|---|
| 未知 Type（任何 topic） | 标脏（保守自愈） |
| 已知 Type 但 Topic 或 Payload 类型不合法（畸形事件） | 标脏（保守自愈） |
| `task.created`/`task.status_changed`/`task.deleted`/`task.activity_changed` | RID==该 taskID → 标脏，否则不脏 |
| `task.user_activity` | 合法事件不标脏（一次性用户输入事实，不影响任务详情投影；畸形事件按上行惯例标脏） |
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

#### Scenario: 用户活动事件不触发推送

- **WHEN** 连接存续期间到达合法的 `task.user_activity` 事件（无论 RID 是否为本 taskID）
- **THEN** 不标脏，不触发重组装，不发送 `update` 帧

#### Scenario: 窗口内多次变更合并

- **WHEN** 500ms 合并窗口内到达多个关联本任务的标脏事件
- **THEN** 客户端仅收到一帧 `update`，data 为窗口到期时刻的全量快照

#### Scenario: 未认证访问被拒

- **WHEN** 请求缺失或携带错误 token
- **THEN** 返回 401，不建立事件流，不泄露任何资源信息

#### Scenario: 推送无写副作用且无实时探测

- **WHEN** SSE 连接存续并发生多次推送
- **THEN** 数据库内容、任务状态机与进程集合除外部因素外不发生变化；推送路径不发起任何 opencode 调用，`AgentStatus` 实时探测调用次数为 0

#### Scenario: worktree 任务详情携带来源分支

- **WHEN** 已认证客户端请求 base_ref 非空的 worktree 模式任务的详情（REST 或 SSE 快照）
- **THEN** 详情对象 `base_ref` 为该任务已持久化的全限定基线 ref（`refs/heads/<name>` 或 `refs/remotes/<name>`）

#### Scenario: 非 worktree 任务详情来源分支为空

- **WHEN** 已认证客户端请求 local-path / dir 任务的详情（REST 或 SSE 快照）
- **THEN** 详情对象 `base_ref` 字段存在且为空串

#### Scenario: 历史空值 worktree 任务详情正常返回

- **WHEN** 已认证客户端请求 base_ref 落库为空串的 worktree 模式任务的详情（REST 或 SSE 快照；如本特性上线前创建的历史任务）
- **THEN** 详情正常返回，`base_ref` 字段存在且为空串，不报错、不回填
