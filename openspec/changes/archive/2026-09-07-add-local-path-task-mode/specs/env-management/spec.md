# Delta: env-management（add-local-path-task-mode）

## MODIFIED Requirements

### Requirement: 生命周期变量注入

系统 SHALL 在任务进程启动时注入系统生命周期变量，至少包括：OCDECK_TASK_ID、OCDECK_TASK_NAME、OCDECK_TASK_PATH（worktree 绝对路径）、OCDECK_PROJECT_PATH、OCDECK_SERVE_PORT。

repo 项目 worktree 模式任务 MUST 额外注入：
- `OCDECK_TASK_BASE_BRANCH`：基线分支短名（用户所见形态）。值由落库的全限定 `tasks.base_ref` 去掉前缀得到：`refs/heads/<name>` → `<name>`；`refs/remotes/<name>` → `<name>`（`<name>` 含远端名，如 `origin/main`）。不得注入全限定 ref。
- `OCDECK_TASK_HEAD_BRANCH`：任务自身分支名，取值 `tasks.branch`（`ocdeck/<slug>`）。

dir 任务与 repo 项目 local-path 模式任务 MUST NOT 注入 `OCDECK_TASK_BASE_BRANCH` 与 `OCDECK_TASK_HEAD_BRANCH`（键不存在，不得注入空串）。

repo 项目 worktree 模式任务的 `tasks.base_ref` MUST 匹配 `refs/heads/<non-empty>` 或 `refs/remotes/<non-empty>`，且 `tasks.branch` MUST 非空。任一不满足时 `layerEnvSnapshot` MUST 返回 internal error：MUST NOT 持久化新快照、MUST NOT 创建进程。init/pre_delete 沿既有「layer env snapshot 失败」路径落账（init → `init_status=failed`；pre-delete → `deletion_failed`），且 MUST NOT 触发后续副作用。未知项目 kind（既非 `repo` 也非 `dir`）MUST 同样返回 internal error，不得按 dir 静默缺键。非法 kind/mode 组合（`kind=dir` 且 mode=`worktree`、未知 `mode`）MUST 同样返回 internal error，零副作用。

上述变量与既有 `OCDECK_*` 同属生命周期层，用户 env MUST NOT 覆盖。注入 MUST 进入激活时持久化的 `tasks.env_snapshot`，因此 opencode serve（agent bash 子进程继承）与页面打开的 shell（`CreateShell` 读快照）均可读取。自动重拉属于同一激活代，MUST NOT 经 `layerEnvSnapshot` 补写这两个键（见「修改后生效时机」）。

#### Scenario: 进程内读取生命周期变量

- **WHEN** 任务进程启动
- **THEN** 进程环境中存在全部生命周期变量且值为系统生成

#### Scenario: repo 任务注入基线与任务分支短名

- **WHEN** repo 项目 worktree 模式任务激活，落库 `tasks.base_ref=refs/remotes/origin/main` 且 `tasks.branch=ocdeck/my-task`
- **THEN** 任务进程环境（含持久化快照）含 `OCDECK_TASK_BASE_BRANCH=origin/main` 与 `OCDECK_TASK_HEAD_BRANCH=ocdeck/my-task`

#### Scenario: repo 本地基线去掉 refs/heads 前缀

- **WHEN** repo 项目 worktree 模式任务激活，落库 `tasks.base_ref=refs/heads/main`
- **THEN** `OCDECK_TASK_BASE_BRANCH=main`

#### Scenario: dir 任务不注入分支变量

- **WHEN** dir 任务激活
- **THEN** 任务进程环境与 env 快照中不存在键 `OCDECK_TASK_BASE_BRANCH` 与 `OCDECK_TASK_HEAD_BRANCH`

#### Scenario: local-path 模式任务激活不注入分支变量

- **WHEN** repo 项目 local-path 模式任务激活
- **THEN** 任务进程环境与 env 快照中不存在键 `OCDECK_TASK_BASE_BRANCH` 与 `OCDECK_TASK_HEAD_BRANCH`（与 dir 任务一致：键不存在、不注入空串）

#### Scenario: agent bash 与页面 shell 均可读取

- **WHEN** repo 项目 worktree 模式任务已激活且已注入上述两个变量
- **THEN** opencode serve 进程环境与随后新建的页面 shell 进程环境均含相同的 `OCDECK_TASK_BASE_BRANCH` 与 `OCDECK_TASK_HEAD_BRANCH` 值

#### Scenario: repo 异常 base_ref 拒绝注入

- **WHEN** repo 项目 worktree 模式任务的 `tasks.base_ref` 为空、或既非 `refs/heads/<non-empty>` 也非 `refs/remotes/<non-empty>`
- **THEN** `layerEnvSnapshot` 返回 internal error，不持久化新快照、不创建进程

#### Scenario: repo 空 branch 拒绝注入

- **WHEN** repo 项目 worktree 模式任务的 `tasks.branch` 为空
- **THEN** `layerEnvSnapshot` 返回 internal error，不持久化新快照、不创建进程

#### Scenario: 未知项目 kind 拒绝注入

- **WHEN** 任务所属项目 kind 既非 `repo` 也非 `dir`
- **THEN** `layerEnvSnapshot` 返回 internal error，不持久化新快照、不创建进程

#### Scenario: 非法 kind/mode 组合拒绝注入

- **WHEN** 任务持久化组合为 dir+worktree 或未知 `mode`，激活进入 `layerEnvSnapshot`
- **THEN** `layerEnvSnapshot` 返回 internal error，不持久化新快照、不创建进程
