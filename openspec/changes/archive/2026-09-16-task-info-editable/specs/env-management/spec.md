## MODIFIED Requirements

### Requirement: 修改后生效时机

环境变量的修改 SHALL 仅在该任务下一次"挂起后激活"时生效。系统 SHALL 在任务激活时合并 env、生成快照并持久化（`tasks.env_snapshot`）；同次激活内的 attach 重开与新建 shell MUST 复用该快照（不得重新读 DB）；**persist 模式服务端重启恢复 MUST 从 DB 读回原快照**（重启不是 env 生效点）；挂起时清除快照。系统 MUST 在 UI 提示"需重启任务（挂起后激活）生效"。

**显式例外——任务信息修改（task-lifecycle spec「任务名称修改」「任务分支改名」）**：任务信息保存 MUST 同步改写有效 env 快照中本次变更对应的生命周期键——名称变更改写 `OCDECK_TASK_NAME`；分支实际改名改写 `OCDECK_TASK_HEAD_BRANCH`（历史快照缺少该键时补写；仅名称变更时 MUST NOT 补写分支键）；其余键与快照元数据 MUST 原样保留；快照改写与业务字段在**单个事务**中原子提交。任务无快照且处于非 active 状态（挂起等正常缺失）时 MUST 只提交业务字段、快照保持 NULL。**active 任务快照缺失（NULL）时，MUST 在任何写入前返回 internal error、零副作用**——不生成快照、不调用 `layerEnvSnapshot`、不修改业务字段（快照缺失不可自愈，与既有语义一致）。任务存在损坏快照（JSON 非法 / `vars` 缺失）且需要更新时，MUST 在任何写入前返回 internal error、零副作用（与坏快照不可自愈语义一致）。该例外仅作用于持久化快照；运行中进程的环境变量为 OS 级不可变，不在同步范围内。

自动重拉（Recovery）属于同一激活代：MUST 从现有 `tasks.env_snapshot` 加载环境，仅更新 `OCDECK_SERVE_PORT` 后持久化，MUST NOT 调用 `layerEnvSnapshot`。部署前已 active 的旧快照经自动重拉后仍无 `OCDECK_TASK_BASE_BRANCH` / `OCDECK_TASK_HEAD_BRANCH`；挂起再激活后才获得新键。

快照缺失、JSON 非法、`vars` 缺失或为 null 均视为不可自愈。`loadEnvSnapshot` MUST 校验并返回普通 error，MUST 拒绝 `vars == nil`（不得返回 nil map）。`runRecoveryIncident` MUST 在进入 attempt、获取 permit 和退避之前加载快照；失败时构造 `&recoveryTerminalError{err: newOpErr(codeInternal, err)}` 并走既有终态分派。坏快照路径 MUST NOT 调用 `persistEnvSnapshot`、MUST NOT 写入更新后的 env map、MUST NOT `NewSession`、MUST NOT 调用 `AcquireRecoveryPermit`、MUST NOT backoff；既有终态补偿事务（`status`/`last_error`/`env_snapshot=NULL`）仍 MUST 执行。有效 map 再传给 permit-first 的 `runRecoveryAttempt`。

#### Scenario: 运行中修改变量
- **WHEN** 用户在任务活跃期间修改 env
- **THEN** 当前进程环境与该次激活内新建的 shell/重开的 attach 均保持激活快照不变，UI 提示需重启任务生效；任务挂起再激活后新值生效

#### Scenario: persist 重启后 env 一致
- **WHEN** persist 模式下服务端重启并恢复活跃任务
- **THEN** 该任务全部进程继续使用重启前的激活快照，不因 DB 中的新修改产生同任务两套环境

#### Scenario: 自动重拉复用同代快照且不补写新键
- **WHEN** 部署前已 active 的 repo 任务快照不含 `OCDECK_TASK_BASE_BRANCH` / `OCDECK_TASK_HEAD_BRANCH`，随后自动重拉成功
- **THEN** 新 serve 进程环境与持久化快照仍无这两个键；仅 `OCDECK_SERVE_PORT` 可更新

#### Scenario: 自动重拉保持原分支变量值
- **WHEN** 已 active 的 repo 任务快照含 `OCDECK_TASK_BASE_BRANCH=origin/main` 与 `OCDECK_TASK_HEAD_BRANCH=ocdeck/my-task`，随后自动重拉成功
- **THEN** 新 serve 进程环境与持久化快照仍为上述原值（不得因落库 `base_ref`/`branch` 或用户 env 变化而重算）

#### Scenario: 自动重拉遇到坏快照立即终态
- **WHEN** 自动重拉加载 `tasks.env_snapshot` 时快照缺失、JSON 非法、或 `vars` 缺失/为 null
- **THEN** `loadEnvSnapshot` 返回普通 error；`runRecoveryIncident` 在获取 permit 之前将其包装为 `recoveryTerminalError` 并立即终态补偿；`AcquireRecoveryPermit` 调用次数为 0、无 backoff、无 `persistEnvSnapshot`、无更新后的 env map 写入、无 `NewSession`、无 panic；既有终态补偿事务仍执行

#### Scenario: 任务改名同步快照（例外路径）

- **WHEN** 活跃任务存在有效 env 快照，用户修改任务名称（或实际修改分支）
- **THEN** 快照中 `OCDECK_TASK_NAME`（或 `OCDECK_TASK_HEAD_BRANCH`）被改写为新值并与业务字段同事务提交，其余键与元数据不变；新拉起的 shell 立即读到新值，运行中进程环境不变

#### Scenario: 挂起任务改名无快照可改

- **WHEN** 挂起任务（无 env 快照）被修改名称或分支
- **THEN** 只提交业务字段，快照保持 NULL，下次激活时按新值生成快照

#### Scenario: 损坏快照拒绝信息修改

- **WHEN** 活跃任务的 env 快照损坏（JSON 非法或 `vars` 缺失），用户修改任务名称或分支
- **THEN** 系统在任何写入前返回 internal 错误，任务名称、分支与快照均保持原值，零副作用

#### Scenario: 活跃任务快照缺失拒绝信息修改

- **WHEN** 活跃任务的 env 快照缺失（NULL），用户修改任务名称或分支
- **THEN** 系统在任何写入前返回 internal 错误（不隐式生成快照），任务名称、分支与快照均保持原值，零副作用
