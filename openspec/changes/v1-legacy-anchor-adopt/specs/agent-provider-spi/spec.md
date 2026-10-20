# agent-provider-spi Spec (Delta)

## ADDED Requirements

### Requirement: 无 binding 老任务的 anchor 承接（legacy-A）

钉扎 `opencode-v1`、`task_binding` 为空、anchor 三元组为 v1 命名空间（`anchor_provider_id=opencode-v1`、`anchor_provider_instance_id=default`）且 `anchor_session_id` 存在于该任务 v1 归属集合的任务，承接准入 SHALL 判为 A 分支（承接目标 = `anchor_session_id`），承接流程以合成的内存态 v1 binding（不持久化）完成原 session 验证。承接事务的旧 binding 代际守卫 MUST 相应复验 `task_binding` 仍为空（NULL）：提交时若 `task_binding` 已被并发写入非空值 MUST 拒绝（ErrAdoptionConflict），零写入。anchor 不在归属集合、anchor 命名空间非 v1、或归属行含非 v1 命名空间时 MUST 维持停止承接（stop），不得猜测。

#### Scenario: 无 binding 老任务承接成功

- **WHEN** 钉扎 v1、`task_binding` 为 NULL、anchor 为 v1 且 anchor ∈ v1 归属集合的任务在 v2 机器上激活或恢复
- **THEN** 判定矩阵命中 legacy-A，以 anchor_session_id 为承接目标完成承接，binding 翻转为 v2、anchor 三元组翻转为 v2、归属行转换命名空间，写入迁移承接通知

#### Scenario: 并发写入 binding 后承接拒绝

- **WHEN** legacy-A 承接提交时，该任务的 `task_binding` 已被并发路径写入非空值
- **THEN** 承接事务 CAS 失配拒绝（ErrAdoptionConflict），整体回滚零写入，按冲突重试语义处理

#### Scenario: anchor 不在归属集合维持停止

- **WHEN** 钉扎 v1、`task_binding` 为 NULL，但 anchor_session_id 不在该任务 v1 归属集合中（或 anchor 为空、或命名空间非 v1）
- **THEN** 判定矩阵维持 stop：停止承接、保留现状、last_error 如实告知，待用户决策
