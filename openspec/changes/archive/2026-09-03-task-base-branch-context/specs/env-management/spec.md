## MODIFIED Requirements

### Requirement: 生命周期变量注入
系统 SHALL 在任务进程启动时注入系统生命周期变量，至少包括：OCDECK_TASK_ID、OCDECK_TASK_NAME、OCDECK_TASK_PATH（worktree 绝对路径）、OCDECK_PROJECT_PATH、OCDECK_SERVE_PORT。

repo 任务 MUST 额外注入：
- `OCDECK_TASK_BASE_BRANCH`：基线分支短名（用户所见形态）。值由落库的全限定 `tasks.base_ref` 去掉前缀得到：`refs/heads/<name>` → `<name>`；`refs/remotes/<name>` → `<name>`（`<name>` 含远端名，如 `origin/main`）。不得注入全限定 ref。
- `OCDECK_TASK_HEAD_BRANCH`：任务自身分支名，取值 `tasks.branch`（`ocdeck/<slug>`）。

dir 任务 MUST NOT 注入 `OCDECK_TASK_BASE_BRANCH` 与 `OCDECK_TASK_HEAD_BRANCH`（键不存在，不得注入空串）。

repo 任务的 `tasks.base_ref` MUST 匹配 `refs/heads/<non-empty>` 或 `refs/remotes/<non-empty>`，且 `tasks.branch` MUST 非空。任一不满足时 `layerEnvSnapshot` MUST 返回 internal error：MUST NOT 持久化新快照、MUST NOT 创建进程。init/pre_delete 沿既有「layer env snapshot 失败」路径落账（init → `init_status=failed`；pre-delete → `deletion_failed`），且 MUST NOT 触发后续副作用。未知项目 kind（既非 `repo` 也非 `dir`）MUST 同样返回 internal error，不得按 dir 静默缺键。

上述变量与既有 `OCDECK_*` 同属生命周期层，用户 env MUST NOT 覆盖。注入 MUST 进入激活时持久化的 `tasks.env_snapshot`，因此 opencode serve（agent bash 子进程继承）与页面打开的 shell（`CreateShell` 读快照）均可读取。自动重拉属于同一激活代，MUST NOT 经 `layerEnvSnapshot` 补写这两个键（见「修改后生效时机」）。

#### Scenario: 进程内读取生命周期变量
- **WHEN** 任务进程启动
- **THEN** 进程环境中存在全部生命周期变量且值为系统生成

#### Scenario: repo 任务注入基线与任务分支短名
- **WHEN** repo 任务激活，落库 `tasks.base_ref=refs/remotes/origin/main` 且 `tasks.branch=ocdeck/my-task`
- **THEN** 任务进程环境（含持久化快照）含 `OCDECK_TASK_BASE_BRANCH=origin/main` 与 `OCDECK_TASK_HEAD_BRANCH=ocdeck/my-task`

#### Scenario: repo 本地基线去掉 refs/heads 前缀
- **WHEN** repo 任务激活，落库 `tasks.base_ref=refs/heads/main`
- **THEN** `OCDECK_TASK_BASE_BRANCH=main`

#### Scenario: dir 任务不注入分支变量
- **WHEN** dir 任务激活
- **THEN** 任务进程环境与 env 快照中不存在键 `OCDECK_TASK_BASE_BRANCH` 与 `OCDECK_TASK_HEAD_BRANCH`

#### Scenario: agent bash 与页面 shell 均可读取
- **WHEN** repo 任务已激活且已注入上述两个变量
- **THEN** opencode serve 进程环境与随后新建的页面 shell 进程环境均含相同的 `OCDECK_TASK_BASE_BRANCH` 与 `OCDECK_TASK_HEAD_BRANCH` 值

#### Scenario: repo 异常 base_ref 拒绝注入
- **WHEN** repo 任务的 `tasks.base_ref` 为空、或既非 `refs/heads/<non-empty>` 也非 `refs/remotes/<non-empty>`
- **THEN** `layerEnvSnapshot` 返回 internal error，不持久化新快照、不创建进程

#### Scenario: repo 空 branch 拒绝注入
- **WHEN** repo 任务的 `tasks.branch` 为空
- **THEN** `layerEnvSnapshot` 返回 internal error，不持久化新快照、不创建进程

#### Scenario: 未知项目 kind 拒绝注入
- **WHEN** 任务所属项目 kind 既非 `repo` 也非 `dir`
- **THEN** `layerEnvSnapshot` 返回 internal error，不持久化新快照、不创建进程

### Requirement: 修改后生效时机
环境变量的修改 SHALL 仅在该任务下一次"挂起后激活"时生效。系统 SHALL 在任务激活时合并 env、生成快照并持久化（`tasks.env_snapshot`）；同次激活内的 attach 重开与新建 shell MUST 复用该快照（不得重新读 DB）；**persist 模式服务端重启恢复 MUST 从 DB 读回原快照**（重启不是 env 生效点）；挂起时清除快照。系统 MUST 在 UI 提示"需重启任务（挂起后激活）生效"。

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
