# Env Management Specification

## Purpose
管理全局级、项目级、任务级三层环境变量，按优先级合并并在任务进程启动时注入，与宿主 env 隔离，激活时生成快照持久化。
## Requirements
### Requirement: env 基线与宿主隔离
任务进程环境 MUST NOT 继承 ocdeck 服务端宿主 env。注入基线 SHALL 仅为最小基础集：`TERM/COLORTERM/HOME/USER/PATH/SHELL/LANG/LC_ALL/LC_CTYPE/TMPDIR/SSH_AUTH_SOCK` 与代理变量（`HTTP_PROXY/HTTPS_PROXY/NO_PROXY`，若宿主存在）。locale 变量（`LANG/LC_ALL/LC_CTYPE`）仅透传宿主非空值；三者均未设置或为空时系统 MUST 注入默认 `LANG=en_US.UTF-8`（保证 tmux attach 客户端 `client_utf8=1`，CJK 输出不被转写为 `_`）。执行 tmux 命令时 MUST 以清洗后的 env 调用，防止宿主 env 经 tmux server 全局环境隐式流入会话。provider 凭据主通道为 opencode 自身 auth store；需要 env 凭据的场景由用户经项目级/任务级 env 显式配置。

#### Scenario: 宿主变量不流入
- **WHEN** ocdeck 服务端 env 中存在 AWS_SECRET_ACCESS_KEY 等未配置变量，任务激活
- **THEN** 任务进程环境中不存在该变量（基础集除外）

#### Scenario: 高位 locale 变量原样透传
- **WHEN** 宿主设置了非空 LC_ALL 或 LC_CTYPE（LANG 未设置）
- **THEN** 该变量原样出现在任务进程与 tmux 命令环境中，系统不再注入默认 LANG

#### Scenario: 无有效 locale 时注入默认
- **WHEN** 宿主 LANG/LC_ALL/LC_CTYPE 均未设置或为空（如 launchd 启动）
- **THEN** 任务进程与 tmux 命令环境注入 `LANG=en_US.UTF-8`

### Requirement: 项目级环境变量
系统 SHALL 支持为每个项目维护一组 key-value 环境变量（CRUD），在该项目所有任务的 opencode 进程启动时注入。

#### Scenario: 设置项目级变量
- **WHEN** 用户为项目添加环境变量 KEY=VALUE
- **THEN** 该项目后续激活的所有任务进程环境中包含该变量

### Requirement: 任务级环境变量
系统 SHALL 支持为单个任务维护一组 key-value 环境变量（CRUD），仅在该任务的进程启动时注入。

#### Scenario: 设置任务级变量
- **WHEN** 用户为任务添加环境变量
- **THEN** 该任务下次激活时进程环境中包含该变量

### Requirement: 全局级环境变量
系统 SHALL 支持维护一组跨项目生效的全局级 key-value 环境变量（CRUD），在全部任务的进程启动时注入。每个全局变量 MUST 具有两种模式之一：`follow_host`（激活合并时从 ocdeck 服务端进程环境解析当前值，宿主未设置则该变量跳过不注入）或 `manual`（使用存储的显式值）。激活快照持久化解析后的最终值。

#### Scenario: 手动配置全局变量
- **WHEN** 用户以 manual 模式添加全局变量 KEY=VALUE
- **THEN** 全部项目后续激活的任务进程环境中包含 KEY=VALUE

#### Scenario: 跟随宿主变量
- **WHEN** 用户以 follow_host 模式添加全局变量 KEY，且 ocdeck 服务端进程环境中 KEY 已设置
- **THEN** 激活的任务进程环境中 KEY 取服务端进程环境中的当前值

#### Scenario: 跟随宿主但宿主未设置
- **WHEN** follow_host 模式的 KEY 在 ocdeck 服务端进程环境中不存在
- **THEN** 该变量不注入任务进程（跳过，不注入空值）

### Requirement: 任务级覆盖项目级
环境变量合并优先级 MUST 为：基础集 < 全局级 < 项目级 < 任务级 < 生命周期变量(OCDECK_*) < 系统内部变量。生命周期与内部变量 MUST NOT 可被用户 env 覆盖。系统内部变量按进程类型注入：`OPENCODE_SERVER_PASSWORD` MUST 仅注入 serve 与 attach 进程，MUST NOT 注入 shell 终端。

#### Scenario: 同名变量覆盖
- **WHEN** 全局级、项目级与任务级均定义 KEY，任务激活
- **THEN** 进程环境中 KEY 取任务级值（任务级 > 项目级 > 全局级）

#### Scenario: 用户变量不覆盖内部变量
- **WHEN** 用户在 env 中定义 OPENCODE_SERVER_PASSWORD 或 OCDECK_* 变量
- **THEN** 系统生成值生效，用户值被忽略并提示

#### Scenario: shell 不携带 serve 密码
- **WHEN** 用户新建 shell 终端
- **THEN** shell 进程环境中不存在 OPENCODE_SERVER_PASSWORD

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

### Requirement: 明文存储与日志红线

环境变量在 SQLite 中明文存储（个人自用场景），DB 文件权限 MUST 为 0600。env 值 MUST NOT 出现在 **ocdeck 自身生成的日志与错误信息** 中（含服务端日志、API 错误响应、notice、last_error）。用户生命周期脚本（init / pre_delete）的 stdout/stderr 属于用户可控输出，ocdeck 按原样捕获落盘（见 project-lifecycle-config spec 的生命周期日志要求），不以 env 红线过滤，但系统 UI MUST 在脚本编辑器旁提示"脚本输出会落盘，勿打印敏感凭据"。系统 UI SHALL 提示用户勿存放高敏感凭据。

#### Scenario: 敏感值提示

- **WHEN** 用户保存环境变量
- **THEN** 界面提示明文存储风险

#### Scenario: ocdeck 自身日志不含 env 值

- **WHEN** env 相关的服务端日志、API 错误或 notice 被生成
- **THEN** 其中不出现任何 env 值（键名可出现）

#### Scenario: 用户脚本输出按原样捕获

- **WHEN** 用户 init script 执行 `echo $FOO`（FOO 为已配置 env）
- **THEN** init.log 含脚本输出的值；此为用户可控输出，不视为违反红线

