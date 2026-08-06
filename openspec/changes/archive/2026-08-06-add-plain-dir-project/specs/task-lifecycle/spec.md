# Delta: task-lifecycle

## MODIFIED Requirements

### Requirement: 任务创建

系统 SHALL 支持在项目下创建任务。`kind=repo` 项目的每个任务 MUST 拥有独立的 git worktree 与独立分支（从选定基线切出，缺省为项目默认分支）。`kind=dir`（纯目录）项目的任务 MUST NOT 创建 worktree 与分支：分支记录为空、`worktree_path` 记录为项目路径本身、分支命名（LLM slug 与机械 slugify）、分支校验/冲突检查、worktree 路径生成与碰撞重试、worktree add、inherit 文件继承全部跳过；init script 保留（仍按项目配置决定 `init_status` 并触发 InitRunner/自动激活）。dir 任务创建在落库前 MUST 做无副作用预检：项目路径存在且为目录，否则以 invalid_state 拒绝且 MUST NOT 落 creating 行。dir 任务创建 MUST 为零文件/git 副作用（除落库与后续激活/init 的进程副作用外），`creation_failed` 仅可能来自 lifecycle 配置读取失败或提交点失败。以下 worktree/分支义务均仅适用于 `kind=repo` 项目。

分支名 MUST 为 `ocdeck/<slug>`，slug 生成策略：当 AI 配置可用（见 ai-provider-config spec 的可用性判定）时，SHALL 调用 LLM 将任务名提炼为语义化英文 kebab-case slug（≤50 字符，匹配 `^[a-z0-9]([a-z0-9-]{0,48}[a-z0-9])?$` 且不命中无意义词表，否则视为失败）；LLM 调用失败、超时、输出非法或 AI 未配置时，MUST 回退到机械 slugify（与既有行为一致，空结果兜底 `task`）。**AI 错误本身 MUST NOT 向用户返回、MUST NOT 阻断任务创建**——命名回退后创建流程继续，但随后仍可能因既有的分支名校验/冲突等前置检查失败（该语义不变）。LLM 调用 SHALL 设置超时（≤10s）且发生在任何副作用（落库、worktree add）之前。

新建任务的 worktree 路径 MUST 为 `<dataDir>/worktrees/<projectName-slug>/<branchPathSlug>-<rand4>/`：`projectName-slug` 为项目名经规范化（小写、非 `[a-z0-9-]` 折叠为 `-`，允许为空）的结果，为空时回退 `project-<projectID前8位>`，且 MUST 截断至 ≤50 字符；`branchPathSlug` 为分支名去掉 `ocdeck/` 前缀后截断至 ≤50 字符的目录段（截断后去尾部 `-`，截空时兜底 `task`）——目录段是分支名的派生展示，**分支名本身行为不变**（机械 slugify 无长度限制），DB 落库的 `worktree_path` 为唯一事实源，MUST NOT 从目录反推分支；`rand4` 为 4 位小写字母数字随机后缀（crypto/rand）。熵失败语义：Go 1.24 起 `crypto/rand` 底层熵失败为不可恢复 fatal（进程终止，天然满足零副作用）；实现保留 error 返回路径作为可注入熵源的防御 seam，**当使用可注入熵源且其返回错误时 MUST 返回错误且零副作用**。目录碰撞检测 MUST 在落库前以无副作用的存在性检查完成：碰撞时重新生成后缀（≤3 次），3 次均碰撞 MUST 返回错误且不产生任何副作用。路径在创建时确定并落库，此后删除/挂起/激活/重试等全部生命周期操作 MUST 按 DB 记录的 `worktree_path` 执行，**MUST NOT 按新格式重算**——既有任务（含旧 `<projectID>/<taskID>` 格式路径）行为不变，不做迁移。worktree 创建在任何文件/git 副作用前 MUST 通过 `<dataDir>/worktrees` 根的包含性校验。

创建流程在 worktree 创建成功后、提交 suspended 前，SHALL 执行项目配置的 inherit 文件继承（语义见 project-lifecycle-config spec），inherit 失败 MUST NOT 阻断创建。提交 suspended 后：若项目配置了 init script，SHALL 先异步执行 init 并仅在成功后触发自动激活；init 失败 MUST NOT 触发激活，任务保持 suspended 且 init_status=failed（init 状态机见 project-lifecycle-config spec）；激活失败任务落挂起并记录 last_error，用户可手动重试激活。项目未配置 inherit/init 时，创建流程与既有行为完全一致。

repo 任务创建 SHALL 支持可选基线分支参数 `base_ref`：外部输入为短名（本地分支 `feature-x` 或远端分支 `origin/feature-x`），缺省（空）从项目默认分支切出（向后兼容）。系统 MUST 先对短名执行 `git check-ref-format --branch <短名>` 规范校验，再将输入按 `refs/heads/<name>` → `refs/remotes/<name>` 顺序探测（heads 优先：本地与远端同名时解析为本地分支），仅接受这两个命名空间（拒绝 tag/SHA/任意表达式），经 `git rev-parse --verify` 存在性校验；任一环节失败 MUST 返回 invalid_input。base_ref 校验为无副作用前置检查，MUST 在落 creating 行之前完成；提供非法/不存在 base_ref 时 MUST NOT 产生落库或 worktree 副作用。**解析后的全限定 ref MUST 随任务落库（`tasks.base_ref`），包括缺省创建（落库 `refs/heads/<项目默认分支>`）**；Retry 重试 MUST 使用落库的全限定 ref，MUST NOT 重读项目默认分支；repo 任务落库值为空 MUST fail-closed 报错（空值仅 dir 任务使用）。Retry 保证使用同一 ref（分支名），不保证同一 commit（分支 tip 移动后按当前 tip 重建，与既有语义一致）。任务分支（`ocdeck/<slug>`）命名逻辑与基线解耦、行为不变。`kind=dir` 项目的任务创建 MUST NOT 接受 `base_ref`（提供即 invalid_input）。

系统 SHALL 提供项目分支列表只读查询 `GET /api/v1/projects/{id}/branches`（本地+远端分支，`git branch`/`git branch -r`，不进入仓库写锁）：返回稳定排序、去重后的短名 JSON 数组（如 `["feature-x","main","origin/feature-x"]`），本地分支在前、远端分支在后，按 `%(symref)` 元数据排除远端 symbolic ref（如 `origin/HEAD`）；返回的短名 MUST 可直接作为 `base_ref` 输入。dir 项目调用该查询 MUST 返回 invalid_input。

系统 SHALL 提供远端刷新 `POST /api/v1/projects/{id}/branches/refresh`：对每个 remote 执行 `git fetch --no-tags --no-recurse-submodules --no-write-fetch-head --prune --refmap='+refs/heads/*:refs/remotes/<remote>/*' <remote> '+refs/heads/*:refs/remotes/<remote>/*'`（`--refmap` + 命令行显式 refspec 完全取代 `remote.*.fetch` 配置，保证仅写入 `refs/remotes/<remote>/*`——mirror remote、自定义 refspec、fetch.pruneTags 等配置 MUST NOT 使 fetch 触碰 `refs/heads/*` 或 tags；本机 git CLI，30s 硬上限，`GIT_TERMINAL_PROMPT=0`，子进程按进程组终止）后在同一 repo 写锁内重新枚举并返回同构短名数组。fetch 全程 MUST 持有该 repo 写锁（与 worktree add/remove 串行），同 repo 并发 refresh MUST 合并为单次 fetch（singleflight，等待者共享结果且 MUST 响应自身 context 取消），不同 repo 可并行。refresh MUST fail-closed：fetch 失败/超时/取消返回 git_error，MUST NOT 返回 200 伪装最新；dir/未知 kind MUST fail-closed。fetch MUST NOT 移动既有任务本地分支与 worktree HEAD，MUST NOT 覆盖用户 `FETCH_HEAD`。任务创建 MUST NOT 隐式 fetch（仍按本地 ref 校验）。

#### Scenario: 创建任务（repo 项目）

- **WHEN** 用户在 repo 项目下创建任务（提供任务名称）
- **THEN** 系统创建 worktree 与分支，任务进入挂起状态，随后**自动触发激活**（异步启动进程组并锚定 session）；激活失败任务落挂起并记录 last_error，用户可手动重试激活

#### Scenario: LLM 生成语义化分支名（repo 项目）

- **WHEN** AI 配置可用，用户以中文任务名（如「接入AI与worktree命名优化」）创建任务
- **THEN** 系统调用 LLM 生成英文 slug，分支名为 `ocdeck/<ai-slug>`（如 `ocdeck/ai-worktree`），worktree 目录为 `<dataDir>/worktrees/<projectName-slug>/<ai-slug>-<rand4>/`（AI 路径下目录段与分支 slug 一致）

#### Scenario: AI 未配置或失败时回退（repo 项目）

- **WHEN** AI 未配置、调用失败/超时、或输出未通过清洗门禁
- **THEN** 系统回退到机械 slugify 生成分支名，AI 错误不向用户暴露；创建流程继续，随后仍遵循既有前置检查语义（如分支冲突时报错）

#### Scenario: 新路径格式的人类可读目录（repo 项目）

- **WHEN** 创建新任务
- **THEN** worktree 目录为 `<dataDir>/worktrees/<projectName-slug>/<branchPathSlug>-<rand4>/`（branchPathSlug 为分支名去 `ocdeck/` 前缀后截断 ≤50 字符的目录段，分支名本身不变），项目名与分支语义可从路径直接辨认；存量旧格式任务的目录与全部生命周期操作（含创建重试）不受影响

#### Scenario: 纯中文项目名的目录回退（repo 项目）

- **WHEN** 项目名规范化后为空（如纯中文项目名）
- **THEN** 目录第一段为 `project-<projectID前8位>`，保证非空、合法、可区分

#### Scenario: 目录碰撞重试（repo 项目）

- **WHEN** 落库前的存在性检查发现目标目录已存在
- **THEN** 系统重新生成 4 位随机后缀重试（≤3 次）；3 次均碰撞则返回错误，不产生落库或 worktree 副作用

#### Scenario: 配置 init 的项目创建任务（repo 项目）

- **WHEN** 项目配置了 init script，创建任务
- **THEN** worktree 创建 → inherit 复制 → 挂起（init_status=pending）→ init 执行成功 → 自动激活

#### Scenario: init 失败停留在挂起（repo 项目）

- **WHEN** 创建链中 init script 执行失败
- **THEN** 任务保持挂起、init_status=failed、init_error 落库，无 serve/tui 会话，用户可查看日志并 Re-run

#### Scenario: 未配置项目行为不变（repo 项目）

- **WHEN** 项目未配置 inherit patterns 与 init script，创建任务
- **THEN** 创建流程与既有行为一致：worktree 创建后直接自动激活，init_status=none

#### Scenario: Probe 冷启动重试（repo 项目）

- **WHEN** 创建后自动激活时 capability probe 因冷启动超时/网络类错误（ErrServeNotReady）失败
- **THEN** 系统保持 serve 会话并短退避重试（共 3 次尝试，退避 2s/4s）；任一次成功则激活继续；全部失败才落 suspended 并记录 last_error（语义与现状一致）。结构不兼容（ErrCapabilityMismatch）与凭据错误（ErrUnauthorized）不重试

#### Scenario: 分支名冲突（repo 项目）

- **WHEN** 生成的分支名已存在（无论由 LLM 生成还是 slugify 回退）
- **THEN** 系统报错并提示用户更换任务名称

#### Scenario: 指定基线分支创建（repo 项目）

- **WHEN** 用户创建 repo 任务并提供 `base_ref` 短名（本地分支 `feature-x` 或远端分支 `origin/feature-x`）
- **THEN** 系统校验该分支存在后，worktree 从该基线切出（任务分支命名不变），解析后的全限定 ref 落库供 Retry 使用

#### Scenario: 默认基线行为不变（repo 项目）

- **WHEN** 用户创建 repo 任务未提供 `base_ref`
- **THEN** worktree 从项目默认分支切出，与既有行为一致；落库 `refs/heads/<项目默认分支>`

#### Scenario: 同名本地与远端分支的解析优先级（repo 项目）

- **WHEN** 仓库同时存在本地分支 `origin/feature-x`（`refs/heads/origin/feature-x`）与远端分支 `origin/feature-x`（`refs/remotes/origin/feature-x`），用户提供短名 `origin/feature-x`
- **THEN** 系统按 heads 优先解析为本地 `refs/heads/origin/feature-x`（全限定 ref 落库，Retry 不受远端变化影响）

#### Scenario: 缺省基线创建后默认分支变化的 Retry（repo 项目）

- **WHEN** 缺省基线创建的任务落 creation_failed，随后项目默认分支被修改，用户 Retry
- **THEN** Retry 仍使用创建时落库的 `refs/heads/<原默认分支>` 重建 worktree，不受默认分支变化影响

#### Scenario: 非法或不存在的基线分支（repo 项目）

- **WHEN** 用户提供的 `base_ref` 未通过 check-ref-format、不是本地/远端分支、或分支不存在
- **THEN** 系统返回 invalid_input 明确错误，MUST NOT 落 creating 行、零副作用

#### Scenario: dir 项目拒绝 base_ref

- **WHEN** 用户对 dir 项目创建任务并提供 `base_ref`
- **THEN** 系统返回 invalid_input（纯目录项目无基线分支语义），零副作用

#### Scenario: Retry 使用落库基线（repo 项目）

- **WHEN** 以 `base_ref=origin/feature-x` 创建的任务落 creation_failed 后 Retry，期间项目默认分支已被修改
- **THEN** Retry 仍按落库的 `refs/remotes/origin/feature-x` 重建 worktree（同一 ref，tip 为当前值），不受默认分支变化影响

#### Scenario: 分支列表查询成功（repo 项目）

- **WHEN** 请求 repo 项目的分支列表
- **THEN** 返回稳定排序、去重后的短名 JSON 数组（本地分支在前、远端分支在后，排除 `origin/HEAD` 等 symbolic HEAD），每个短名可直接作为 `base_ref` 输入

#### Scenario: dir 项目拒绝分支列表查询

- **WHEN** 请求 dir 项目的分支列表
- **THEN** 系统返回 invalid_input（纯目录项目无分支语义）

#### Scenario: 远端新分支 refresh 后可见

- **WHEN** 远端新建分支后，普通 GET 列表（本地快照）尚不包含该分支，用户触发 refresh
- **THEN** 系统 fetch 后返回的列表包含该分支，且可直接作为 `base_ref` 创建任务（从最新本地 remote tip 切出）

#### Scenario: refresh 失败不伪装最新

- **WHEN** fetch 因网络/凭证/超时失败
- **THEN** 系统返回 git_error（不返回 200 伪装最新），UI 保留旧列表并标注"本地快照未刷新"+ 重试入口

#### Scenario: refresh 不移动既有任务分支

- **WHEN** 项目存在活跃任务（本地 `ocdeck/*` 分支与 worktree），执行 refresh（远端分支已推进/删除）
- **THEN** fetch 仅更新 `refs/remotes/*` 与对象库（prune 移除已删远端分支），既有任务本地分支与 worktree HEAD 不变，用户 `FETCH_HEAD` 不被覆盖

#### Scenario: 同 repo 并发 refresh 合并

- **WHEN** 同一 repo 项目的多个 refresh 请求并发到达
- **THEN** 仅执行一次 fetch，全部等待者共享同一结果；不同 repo 的 refresh 可并行

#### Scenario: dir 项目创建任务

- **WHEN** 用户在 `kind=dir` 项目下创建任务
- **THEN** 系统不创建 worktree/分支、不执行分支命名与校验、不执行 inherit；任务落库 `branch` 为空、`worktree_path` 等于项目路径，随后按项目 init 配置走 InitRunner 或直接自动激活；init/激活开始前项目目录内零新增文件

#### Scenario: dir 项目目录消失时拒绝创建

- **WHEN** 用户在 `kind=dir` 项目下创建任务，但项目路径已不存在或不再是目录
- **THEN** 系统以 invalid_state 拒绝，MUST NOT 落 creating 行，零副作用

#### Scenario: dir 项目创建重试时目录消失

- **WHEN** dir 任务处于 creation_failed，用户 Retry，但项目路径已不存在或不再是目录
- **THEN** 系统保持 creation_failed 并返回明确错误，零副作用

#### Scenario: dir 项目配置 init script

- **WHEN** dir 项目配置了 init script，创建任务
- **THEN** 任务落挂起（init_status=pending）→ init 在项目目录内执行成功 → 自动激活；init 失败保持挂起且 init_status=failed（与 repo 语义一致）

#### Scenario: dir 项目创建重试

- **WHEN** dir 任务处于 creation_failed（配置读取失败/提交点失败），用户 Retry
- **THEN** 系统跳过 worktree 产物验证与分支检查，仅校验项目目录仍存在后重读配置并提交，随后按 init 配置触发 InitRunner 或自动激活

#### Scenario: dir 项目并行多任务

- **WHEN** 同一 dir 项目下已存在活跃任务，用户再创建/激活新任务
- **THEN** 系统允许并行（无互斥锁，符合无人工并发配额语义），UI 显示"多任务共享同一目录、无文件隔离"提示

### Requirement: 任务删除清理

系统 SHALL 在删除任务前完成全部前置检查（dirty/untracked 确认、分支被其他 worktree 占用检查、路径包含性校验），**全部通过后才允许任何副作用**。此外，任务 init 进行中（`init_status ∈ {pending,running}`）时 MUST 拒绝删除与归档（invalid_state，提示 init 进行中）。删除副作用 MUST 按序执行：① 持久化 delete_mode + 置 deleting ② **RetryReap 既有 cleanup debt**（remaining 非空则落 deletion_failed，不得继续）③ 删 oc session 数据（逐个，404 幂等视为成功）④ kill 残余 tmux 会话（若有）⑤ 二次 dirty 门禁 ⑥ pre_delete script（项目配置时；worktree 不存在则幂等跳过；语义见 project-lifecycle-config spec）⑦ 删 worktree ⑧ 删本地分支 ⑨ 删 DB 记录 ⑩ best-effort 清理任务日志目录（忽略错误）。远端分支 MUST NOT 被删除。**Force 模式只能跳过 ③ 与 ⑥，MUST NOT 跳过 ② 进程收割**。

`kind=dir` 项目的任务删除 MUST 按以下变体执行。**硬不变量：ocdeck 内建删除逻辑 MUST NOT 对用户目录（`worktree_path` = 项目路径）及其内容执行任何写/删操作；唯一例外是用户显式配置的 pre_delete script（用户授权操作，不计入不变量）**。dir 任务 MUST NOT 执行任何 git/文件类步骤：跳过全部前置 git 检查（dirty 确认、分支占用、包含性校验）、⑤ 二次 dirty 门禁、⑦ 删 worktree、⑧ 删本地分支；`confirmDirty` 参数接受但忽略。dir normal 序列：① → ② → ③ → ④ → ⑥（pre_delete script 以项目目录为 cwd，用户授权）→ ⑨ → ⑩；dir force 序列与 repo force 契约一致（跳过 ③ 与 ⑥，MUST NOT 跳过 ②）：① → ② → ④ → ⑨ → ⑩。即 dir 删除仅清理 ocdeck 自身数据（DB 记录、tmux 进程组、任务日志目录）与本任务拥有的 opencode session 数据。实现 MUST 在删除序列入口按项目 `kind` 一次性分流为 repo/dir 两条序列（共享步骤抽共用 helper），MUST NOT 依赖 `branch` 判空等隐式信号逐步跳过；删除重试（Retry）按持久化 delete_mode 重入同一 dir 序列。

#### Scenario: 删除挂起任务（repo 项目）

- **WHEN** 用户删除一个 repo 项目的挂起任务并完成 dirty 确认（如有）
- **THEN** 系统按序完成全部清理（含 worktree 与本地分支移除），任务记录移除

#### Scenario: 进程已死时删除

- **WHEN** 删除任务时其 opencode 进程不存在（如服务端崩溃后）
- **THEN** 系统临时启动一次性 serve 完成 session 删除（不直接操作 opencode DB），其余清理照常

#### Scenario: 删除中途失败

- **WHEN** 删除任一步骤失败
- **THEN** 任务进入 deletion_failed 状态并记录 last_error，允许用户重试（幂等，从失败步骤继续）或选择"强制删除"

#### Scenario: 强制删除

- **WHEN** 用户对删除失败的任务选择强制删除
- **THEN** 系统跳过 oc session 删除（保留 session 数据并提示残留）与 pre_delete script，完成其余清理

#### Scenario: init 进行中拒绝删除

- **WHEN** 任务 init_status 为 pending 或 running，用户执行删除或归档
- **THEN** 系统拒绝并提示 init 进行中，任务与 worktree 保持原状

#### Scenario: 配置 pre-delete 的删除顺序（repo 项目）

- **WHEN** repo 项目配置了 pre_delete script，删除任务
- **THEN** pre_delete script 在 kill 残余会话与二次 dirty 门禁之后、worktree 移除之前执行；脚本失败落 deletion_failed，可重试或强制删除

#### Scenario: dirty worktree 删除确认（repo 项目）

- **WHEN** 删除的 repo 任务 worktree 存在未提交或未跟踪文件
- **THEN** 系统提示变更内容并要求显式确认后才继续

#### Scenario: 分支被占用（repo 项目）

- **WHEN** repo 任务分支被其他 worktree 使用中
- **THEN** 系统拒绝删除并说明占用方

#### Scenario: 删除 dir 项目任务（normal）

- **WHEN** 用户以 normal 模式删除 dir 项目的任务（无需 dirty 确认）
- **THEN** 系统按 dir normal 序列完成清理：debt 重试 → 删本任务 oc session 数据 → kill 残余会话 → pre_delete script（如配置，用户授权）→ 删 DB 记录 → 清理任务日志目录

#### Scenario: 强制删除 dir 项目任务

- **WHEN** 用户对 dir 项目任务选择强制删除（Force）
- **THEN** 系统按 dir force 序列执行：跳过 oc session 删除与 pre_delete script（与 repo force 契约一致），完成 debt 重试 → kill 残余会话 → 删 DB 记录 → 清理任务日志目录

#### Scenario: dir 删除内建逻辑不触碰用户目录

- **WHEN** dir 任务删除执行完成（含 normal、force、retry 任一路径，且未配置 pre_delete script）
- **THEN** 项目目录的文件树与内容无任何增删改（ocdeck 内建逻辑零写/删 syscall），仅 ocdeck 数据与本任务拥有的 opencode session 数据被清理

#### Scenario: dir 任务删除中途失败

- **WHEN** dir 删除序列任一步骤失败（如 oc session 删除失败、残余会话清理非 clean）
- **THEN** 任务落 deletion_failed 并记录 last_error，可重试（幂等，按持久化 delete_mode 重入同一 dir 序列）或强制删除；任何失败路径下 ocdeck 内建逻辑均不触碰用户目录

#### Scenario: dir 任务删除确认弹窗文案

- **WHEN** 用户在 Web UI 删除 dir 项目的任务
- **THEN** 删除确认弹窗明确"仅删除任务记录与 opencode 会话数据，不会删除项目目录及其内容"，不出现 worktree/dirty 删除确认项；normal 模式且项目配置了 pre_delete script 时提示该脚本仍会执行

### Requirement: 服务端重启语义
服务端进程的生命周期语义 SHALL 由关停策略（shutdownPolicy）决定：persist 模式下任务进程托管于 tmux 会话、不随服务端退出而终止，服务端重启后 MUST 对 **active/activating** 中会话存活且无 cleanup debt 的任务恢复活跃（重订阅 SSE + 全量对齐），会话已消失的任务落为挂起；**suspending 任务 MUST 完成清理落为挂起**（以持久化意图为准，不得恢复活跃）；**archived/creation_failed/deletion_failed 等持久状态 MUST 保持原状**（仅清理其异常会话）；kill_on_start / kill_immediate 模式下服务端退出则全部任务进程被终止（立即或下次启动清理），重启后 active/activating/suspending 任务 MUST 收敛为挂起（其余持久状态保持原状），由用户手动激活恢复。`kind=dir` 项目的任务 reconcile 语义与 repo 完全一致（creating 落 creation_failed、creation_failed 保持原状），reconcile MUST NOT 对 dir 任务执行任何 git/产物验证；dir 任务的目录存在性 MUST 仅在创建前置与 Retry 时校验。

#### Scenario: 服务端重启后（persist）
- **WHEN** 服务端重启（persist 模式，此前有活跃任务）
- **THEN** active/activating 中 serve 健康且无 cleanup debt 的任务自动恢复活跃，用户打开终端即见 agent 当前状态；suspending 完成清理落挂起；其余持久状态保持原状

#### Scenario: 服务端重启后（kill 模式）
- **WHEN** 服务端重启（kill_on_start 或 kill_immediate 模式）
- **THEN** active/activating/suspending 任务显示为挂起（archived/creation_failed/deletion_failed 保持原状），用户可逐个激活恢复会话

#### Scenario: reconcile 处理 dir 任务

- **WHEN** 服务端重启后 reconcile 遇到 dir 项目的 creating/creation_failed 任务
- **THEN** 与 repo 任务语义完全一致：creating 落 creation_failed（可 Retry），creation_failed 保持原状；reconcile 不执行任何 git/产物验证，dir 任务的目录存在性仅在创建前置与 Retry 时校验
