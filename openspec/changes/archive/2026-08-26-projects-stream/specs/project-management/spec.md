# project-management 变更 Delta

## MODIFIED Requirements

### Requirement: 项目列表与详情

系统 SHALL 提供项目列表与单项目详情查询，详情包含该项目下的任务数量与任务状态分布。项目列表（`GET /api/v1/projects`）的每个元素 MUST 附加 `tasks` 摘要数组，摘要元素字段为 `id`、`name`、`status`、`init_status`、`branch`、`worktree_path`、`last_error`、`notice`（形状与现有任务 DTO 一致：`NoticeItem[]`，无 notice 时省略，见 `web/src/types.ts:210` `parseNotice`）、`updated_at`（Unix 秒）、`agentStatus`（活跃任务读 agentStatus 内存快照——快照维护见 active-sessions-stream spec「agent 状态事件驱动维护」；不可用时省略；命名与 `GET /api/v1/tasks/active` 一致）、`attention_count`（pending 权限 + 问题总数，见 agent-attention spec）；该字段为可加性扩展，MUST NOT 改变既有字段语义。任务摘要 MUST 覆盖该项目的全部非删除态任务（活跃、挂起、归档、过渡与失败态）；项目无任务时 `tasks` MUST 为空数组 `[]`（MUST NOT 为 `null`）。

失败与只读语义（与 `GET /api/v1/tasks/active` 对齐）：摘要查询失败 MUST 返回 500 标准错误信封且 MUST NOT 进入响应组装；`agentStatus` MUST 读内存快照，请求路径 MUST NOT 实时探测 opencode（原「并发实时水合（每请求并发上限 8、预算 3 秒）」语义自本变更起移除）；快照不可用的任务 MUST 降级为该字段省略，MUST NOT 导致整个请求失败；完整请求路径 MUST 为纯读操作（MUST NOT 写数据库、触发 align、改变任务状态机或启动/停止进程）。

行为变更披露（2026-08-26 用户批准）：`agentStatus` 由「请求时实时探测」改为「内存快照」，字段值可能滞后于对账周期（opencode SSE 状态事件与对账维护）；断流/对账失败时省略该字段，MUST NOT 展示陈旧值。`GET /api/v1/tasks/{id}` 的 `agentStatus` 不在本 requirement 范围内，MUST 保持实时探测语义不变。

#### Scenario: 查看项目列表

- **WHEN** 用户请求项目列表
- **THEN** 系统返回全部已注册项目及其任务概况，每个项目元素携带 `tasks` 摘要数组

#### Scenario: 摘要覆盖多状态任务

- **WHEN** 项目 A 有 1 个活跃任务、1 个挂起任务、1 个归档任务
- **THEN** 项目 A 的 `tasks` 摘要包含全部 3 个任务，状态字段各自正确

#### Scenario: 注意力计数

- **WHEN** 项目 A 的某活跃任务有 2 个 pending 权限请求与 1 个 pending 问题请求
- **THEN** 该任务摘要的 `attention_count` 为 3

#### Scenario: 无任务项目返回空数组

- **WHEN** 项目 B 下没有任何任务
- **THEN** 项目 B 的 `tasks` 字段为 `[]`（非 `null`）

#### Scenario: 摘要查询失败不组装

- **WHEN** 底层摘要查询返回错误
- **THEN** 返回 500 标准错误信封，不进入响应组装

#### Scenario: 快照不可用降级

- **WHEN** 某活跃任务的 agent 状态快照不可用（opencode SSE 断流、对账失败或快照尚不存在）
- **THEN** 返回 200，该任务摘要的 `agentStatus` 省略，其余任务不受影响

#### Scenario: 请求路径无实时探测

- **WHEN** 客户端连续多次请求项目列表或单项目详情
- **THEN** 所有请求的 `agentStatus` 均读内存快照，过程中不发起任何 opencode `/session/status` 调用，响应不因快照读取而阻塞等待外部探测

#### Scenario: 任务详情保持实时探测

- **WHEN** 客户端请求 `GET /api/v1/tasks/{id}`
- **THEN** 该响应的 `agentStatus` 保持既有实时探测语义，行为与本变更前一致
