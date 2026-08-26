## ADDED Requirements

### Requirement: 幂等同值写不推进 updated_at

系统 SHALL 使 `tasks` 表的字段更新（含 `updated_at` 推进类写入：`UpdateTaskEnvSnapshot`/`UpdateTaskLastPort`/`UpdateTaskNotice*`/`SetTaskDeleteMode`、align 事务内 notice 写入，以及全部 status/init_status 写方法）在字段值未发生实际变化时原子地跳过写入（no-op），MUST NOT 推进 `updated_at`。`tasks.updated_at` 的语义 MUST 为「最近一次真实变更时间」，MUST NOT 为「最近一次写尝试时间」。同值判定 MUST 覆盖该 SQL 语句写入的**全部业务列**（如 `UpdateTaskStatus*` 同时写 `status` 与 `last_error`：status 相同但 `last_error` 不同仍是真实行变更，MUST 提交；`updated_at` 按 Unix 秒精度推进——跨秒推进并返回 `UpdatedAtAdvanced=true`，同秒数值不变并返回 `UpdatedAtAdvanced=false`），且 MUST 为原子操作（SQL `WHERE` 排除同值，NULL 安全比较），MUST NOT 采用先读后写的非原子比较。条件更新（CAS）调用方 MUST 能区分「预期状态不匹配」与「同值幂等成功」（结构化结果中 `Matched` 与 `Changed` 分离），同值幂等成功 MUST NOT 被误判为竞争失败。本要求是 Phase 1 唯一的显式对外行为差异：canonical spec 对 `updated_at` 的既有断言均为读侧（projects 摘要字段、指挥中心排序），读侧语义保持成立。

#### Scenario: 同值写不推进 updated_at

- **WHEN** 对某任务执行一次字段值与当前完全相同的更新（如 notice 内容相同的 CAS 写、同值 env snapshot 写入）
- **THEN** 写入被原子跳过，`updated_at` 保持不变，结构化结果返回 `Matched=true, Changed=false`

#### Scenario: 真实变更推进 updated_at

- **WHEN** 某任务的字段发生真实变化且当前秒与上次推进不同
- **THEN** 更新落账且 `updated_at` 推进，结构化结果返回 `Matched=true, Changed=true, UpdatedAtAdvanced=true`

#### Scenario: 同秒真实变更

- **WHEN** 某任务在同一秒（Unix 秒精度）内发生真实字段变更
- **THEN** 更新落账但 `updated_at` 数值不变，结构化结果返回 `Changed=true, UpdatedAtAdvanced=false`

#### Scenario: CAS 同值幂等成功不误判

- **WHEN** CAS 写入的预期状态匹配且新值与当前值相同
- **THEN** 返回 `Matched=true, Changed=false`，调用方 MUST NOT 将其视为竞争失败而重试或报错

#### Scenario: 状态相同但 last_error 变化仍提交

- **WHEN** 某任务的状态改写中新状态与当前相同，但 `last_error` 内容不同
- **THEN** 该更新按真实变更提交，`updated_at` 按秒精度规则推进（跨秒推进、同秒数值不变），MUST NOT 被同值 no-op 吞掉新的错误信息
