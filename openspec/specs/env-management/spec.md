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
系统 SHALL 支持维护一组跨项目生效的全局级 key-value 环境变量（CRUD），在全部任务的进程启动时注入。每个全局变量 MUST 具有两种模式之一：`follow_host`（激活合并时从宿主环境解析当前值，宿主未设置则该变量跳过不注入）或 `manual`（使用存储的显式值）。激活快照持久化解析后的最终值。

follow_host 的宿主解析源 MUST 按序为：① ocdeck 服务端进程环境（未设置或为空视为未命中）；② 用户 login shell 捕获环境（见「login shell 环境捕获」）。两侧均未命中时该变量跳过不注入，MUST NOT 注入空值。

基础层从宿主取值时 MUST 仅查询服务端进程环境（基础集、locale 判断、登录 shell 选择均不经 login shell 捕获兜底）；既有固定值、默认值及后续层叠覆盖规则保持不变。

#### Scenario: 手动配置全局变量
- **WHEN** 用户以 manual 模式添加全局变量 KEY=VALUE
- **THEN** 全部项目后续激活的任务进程环境中包含 KEY=VALUE

#### Scenario: 跟随宿主变量（进程环境命中）
- **WHEN** 用户以 follow_host 模式添加全局变量 KEY，且 ocdeck 服务端进程环境中 KEY 已设置非空
- **THEN** 激活的任务进程环境中 KEY 取服务端进程环境中的当前值

#### Scenario: 跟随宿主变量（login shell 捕获兜底）
- **WHEN** follow_host 模式的 KEY 在服务端进程环境中未设置或为空，但 login shell 捕获环境中存在非空 KEY
- **THEN** 激活的任务进程环境中 KEY 取 login shell 捕获环境中的值

#### Scenario: 跟随宿主但宿主未设置
- **WHEN** follow_host 模式的 KEY 在服务端进程环境与 login shell 捕获环境中均不存在或为空
- **THEN** 该变量不注入任务进程（跳过，不注入空值）

### Requirement: login shell 环境捕获
系统 SHALL 支持捕获用户 login shell 环境，作为 follow_host 解析的兜底源与系统环境变量展示的数据源之一。捕获 MUST 为懒加载（首次兜底解析未命中或首次列举/刷新请求时触发），server 启动与基础集构造 MUST NOT 触发捕获。

捕获缓存 MUST 具有三态：未加载 / 成功缓存 / 失败缓存。未加载状态下普通读取触发一次捕获并按结果迁移；失败缓存状态下普通读取 MUST 按空集处理且 MUST NOT 自动重试，仅显式刷新可触发重新捕获。显式刷新成功 MUST 原子替换缓存（未加载态首次刷新成功 MUST 迁移为成功缓存）。已有缓存（成功态或失败态）下刷新失败 MUST 保留旧缓存并向调用方返回错误；未加载态首次刷新失败 MUST 迁移为失败缓存并返回错误，后续普通读取按失败缓存服务、不再次捕获，仅下一次显式刷新可重试。捕获或刷新失败 MUST NOT 发布部分结果或"成功空结果"，MUST NOT 将任何捕获内容（键或值）写入 ocdeck 自身生成的日志与错误信息。

捕获成功 MUST 同时满足：未超时、shell 退出码为零、输出含完整有效帧、帧内条目可解析。捕获命令 MUST 保证环境导出失败时结束标记不输出且退出码非零。帧语法 MUST 为字节级：BEGIN 标记独立成行（取首个出现）；其后内容按 NUL 切分为完整记录；结束标记为**独立的 NUL 终止控制记录**——仅当某条完整 NUL 记录精确等于结束标记 token 时帧结束。合法环境记录必含 `=`，与该无 `=` 控制记录无歧义；键或值中含完整标记文本的记录不构成帧边界，MUST 原样保留。控制记录后的尾部内容忽略。缺结束控制记录、缺终止 NUL、输出截断均 MUST 判失败，不发布缓存。

捕获缓存 MUST 保留合法的 `KEY=` 空值条目（键存在性用于列举来源计算）；仅 follow_host 解析的有效值判断将空串视为未命中，MUST NOT 在捕获解析阶段删除空值键。条目合法性 MUST 为：无 `=` 或键为空的记录忽略；非空键的 `KEY=` 空值条目保留；重复键取首条。**忽略后零个合法条目时本次捕获 MUST 判失败**（MUST NOT 发布"成功空缓存"）。

任务激活路径上，项目/任务形态校验（kind/mode/base_ref/branch）MUST 先于任何可能触发捕获的宿主解析。非法任务拒绝路径 MUST NOT 触发捕获或刷新、MUST NOT 启动 login shell、MUST NOT 发布捕获缓存（不持久化新快照、不创建进程为既有要求）。

捕获与刷新 MUST NOT 影响已激活任务的 env 快照（快照语义沿用「修改后生效时机」：仅下一次挂起后激活生效），MUST NOT 更新进程管理器/watchdog/tmux 的基础环境。

#### Scenario: 懒加载捕获并缓存
- **WHEN** follow_host 解析或宿主环境列举首次需要 login shell 捕获环境
- **THEN** 系统执行一次捕获并缓存结果；未刷新前后续读取复用缓存，不重复执行捕获

#### Scenario: 捕获失败不阻断主流程
- **WHEN** 首次懒加载捕获失败（超时、shell 不可用、帧无效）
- **THEN** 缓存迁移为失败态并按空集服务；follow_host 兜底按未命中处理（变量跳过不注入），任务激活流程正常继续；日志不含任何捕获内容

#### Scenario: 手动刷新生效
- **WHEN** 用户在系统环境变量展示区触发刷新，且自上次捕获后用户修改了 shell 配置
- **THEN** 系统重新捕获并原子替换缓存，展示区与后续激活的 follow_host 解析使用新捕获结果

#### Scenario: 已有成功缓存时刷新失败
- **WHEN** 缓存处于成功态，手动刷新时捕获执行失败
- **THEN** 系统保留刷新前的成功缓存继续服务解析与列举，并向调用方返回错误；日志不含任何捕获内容

#### Scenario: 失败态下普通读取不重试
- **WHEN** 缓存处于失败态，发生普通 follow_host 解析或列举请求（非显式刷新）
- **THEN** 系统按空集服务且不重新执行捕获

#### Scenario: 首次刷新失败后普通读取不重试
- **WHEN** 缓存处于未加载态，用户首次操作即触发刷新且捕获失败
- **THEN** 缓存迁移为失败态并返回错误；后续普通 follow_host 解析或列举按失败缓存（空集）服务，不重新执行捕获，仅下一次显式刷新可重试

#### Scenario: 值内含完整标记行仍解析成功
- **WHEN** 某变量值中包含独立成行的结束标记文本
- **THEN** 该值被原样保留在捕获结果中，帧判定不受影响，捕获成功

#### Scenario: 键含完整结束标记文本仍完整保留
- **WHEN** 某条环境记录的键中包含独立成行的结束标记文本
- **THEN** 该记录被完整解析保留（键存在性与值均完整），帧不在该记录中间结束

#### Scenario: 帧截断判失败
- **WHEN** 捕获输出缺少终止 NUL 或结束标记（如进程被杀、输出截断）
- **THEN** 本次捕获判失败，不发布部分结果或空结果缓存

#### Scenario: 整帧无合法条目判失败
- **WHEN** 捕获帧完整但全部记录均为无 `=` 或空键的垃圾记录
- **THEN** 本次捕获判失败，不发布"成功空缓存"

#### Scenario: 非法任务不触发捕获
- **WHEN** 任务因 kind/mode/base_ref/branch 校验将被拒绝而进入激活
- **THEN** 形态校验先于宿主解析执行；该路径不触发 login shell 捕获或刷新、不启动 login shell、不发布捕获缓存

### Requirement: 宿主环境变量列举
系统 SHALL 提供宿主环境变量列举能力，返回服务端进程环境与 login shell 捕获环境的合并视图。key 集 MUST 为两侧键的并集；每个变量 MUST 按两侧键存在性标注来源（`process` / `shell` / `both`）。变量值 MUST 依次取：进程环境非空值 → 捕获环境非空值 → 空串（与 follow_host"空视为未命中"的解析顺序一致；`source=both` 时值不一定来自进程）。列举 MUST 为只读：允许填充捕获缓存，MUST NOT 修改数据库、任务 env 快照或注入行为。

无可用成功缓存且捕获失败时，列举 MUST 正常返回仅含进程环境变量的视图（不报错）。

#### Scenario: 合并视图与来源标注
- **WHEN** 客户端请求宿主环境变量列举
- **THEN** 返回进程环境与 login shell 捕获环境的键并集，每项含 key、值与来源标注

#### Scenario: 同键进程非空值优先
- **WHEN** 同一 key 在进程环境与捕获环境中均存在，且进程环境值非空
- **THEN** 列举结果中该 key 取进程环境值，来源标注为 both

#### Scenario: 进程为空串时取捕获值
- **WHEN** 同一 key 两侧均存在，进程环境值为空串、捕获环境值非空
- **THEN** 列举结果中该 key 取捕获环境值，来源标注为 both

#### Scenario: 两侧均为空串
- **WHEN** 同一 key 两侧均存在且值均为空串
- **THEN** 列举结果中该 key 值为空串，来源标注为 both

#### Scenario: 捕获不可用时降级为进程环境视图
- **WHEN** 无可用成功捕获缓存（捕获失败或缓存处于失败态）且客户端请求宿主环境变量列举
- **THEN** 返回结果仅含进程环境变量（来源标注为 process），请求正常返回不报错

#### Scenario: 仅捕获环境存在空值键
- **WHEN** 某 key 仅存在于 login shell 捕获环境且值为空串
- **THEN** 列举结果包含该 key，值为空串，来源标注为 shell

#### Scenario: 列举不触发注入变更
- **WHEN** 客户端请求宿主环境变量列举
- **THEN** 所有任务的 env 快照与进程注入行为保持不变

### Requirement: 系统环境变量展示区
设置页「环境变量」tab SHALL 在全局环境变量列表下方提供「系统环境变量」展示区，呈现「宿主环境变量列举」的合并视图。变量值 MUST 默认以定长掩码展示（不显示完整明文、不泄露值长度），用户点击后 SHALL 可切换查看该行明文。展示区 SHALL 支持按 key 搜索过滤；SHALL 提供刷新按钮，触发手动刷新，刷新成功 SHALL 展示最新结果，刷新失败 SHALL 保留原列表并展示错误提示。

展示区 SHALL 支持将变量一键添加为 follow_host 模式的全局变量，但**仅限符合既有全局变量 key 校验、非系统保留键（`OCDECK_*` 前缀与 `OPENCODE_SERVER_PASSWORD`）、且尚未配置的变量**。保留键、非法 key、已配置的 key 对应行 MUST 正常展示（不违反键并集契约），但添加操作 MUST 禁用并分别标注原因；按钮状态优先级 MUST 为：已配置 → 系统保留 → 非法 key → 可添加。

#### Scenario: 默认掩码与点击显示
- **WHEN** 用户打开系统环境变量展示区
- **THEN** 所有变量值以定长掩码形式展示；用户点击某变量后该变量值显示明文

#### Scenario: 搜索过滤
- **WHEN** 用户在展示区输入搜索词
- **THEN** 列表仅显示 key 匹配搜索词的变量

#### Scenario: 一键添加为 follow_host
- **WHEN** 用户对某个未配置、符合全局变量 key 校验且非系统保留键的系统变量执行添加操作
- **THEN** 系统创建以该 key 为名、follow_host 模式的全局变量，全局环境变量列表同步更新

#### Scenario: 已配置变量不可重复添加
- **WHEN** 某 key 已配置为全局变量
- **THEN** 该系统变量的添加操作被禁用并标注已配置

#### Scenario: 保留键与非法 key 禁用添加
- **WHEN** 系统变量 key 为系统保留键（OCDECK_* 前缀或 OPENCODE_SERVER_PASSWORD）或不符合全局变量 key 命名规则
- **THEN** 该行正常展示，但添加操作被禁用并按上述状态优先级标注原因（「系统保留」/「非法 key」）

#### Scenario: 刷新成功展示最新结果
- **WHEN** 用户点击展示区的刷新按钮且后端重新捕获成功
- **THEN** 展示区替换为最新捕获的宿主环境变量合并视图，同时全局环境变量列表重新加载以更新 resolvedValue；刷新失败则不触发该联动

#### Scenario: 刷新失败保留原列表
- **WHEN** 用户点击刷新按钮且后端重新捕获失败
- **THEN** 展示区保留原列表并显示错误提示

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

