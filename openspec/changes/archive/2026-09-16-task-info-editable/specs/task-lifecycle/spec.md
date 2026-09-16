## MODIFIED Requirements

### Requirement: 任务创建

系统 SHALL 支持在项目下创建任务。`kind=repo` 项目的任务按任务级运行模式分流（模式选择与持久化见「任务运行模式选择」）：worktree 模式（缺省）任务 MUST 拥有独立的 git worktree 与独立分支（从选定基线切出，缺省为项目默认分支）；local-path 模式任务 MUST 遵循与 `kind=dir` 任务完全一致的创建语义——MUST NOT 创建 worktree 与分支：分支记录为空、`worktree_path` 记录为项目路径本身、分支命名（LLM slug 与机械 slugify）、分支校验/冲突检查、worktree 路径生成与碰撞重试、worktree add、inherit 文件继承全部跳过；init script 保留（仍按项目配置决定 `init_status` 并触发 InitRunner/自动激活）。`kind=dir`（纯目录）项目的任务 MUST NOT 创建 worktree 与分支：分支记录为空、`worktree_path` 记录为项目路径本身、分支命名（LLM slug 与机械 slugify）、分支校验/冲突检查、worktree 路径生成与碰撞重试、worktree add、inherit 文件继承全部跳过；init script 保留（仍按项目配置决定 `init_status` 并触发 InitRunner/自动激活）。dir 任务与 repo 项目 local-path 模式任务创建在落库前 MUST 做无副作用预检：项目路径存在且为目录，否则以 invalid_state 拒绝且 MUST NOT 落 creating 行。dir 任务与 local-path 模式任务创建 MUST 为零文件/git 副作用（除落库与后续激活/init 的进程副作用外），`creation_failed` 仅可能来自 lifecycle 配置读取失败或提交点失败。以下 worktree/分支义务均仅适用于 `kind=repo` 项目的 worktree 模式任务。

分支名 MUST 为 `<branch-prefix>/<slug>`，其中 `<branch-prefix>` 为创建时刻读取的全局 worktree 分支前缀配置（默认 `ocdeck`，配置读写与生效范围见 worktree-branch-prefix spec）；**创建流程 MUST 只读取一次前缀配置，并以该快照值驱动分支命名与 worktree 路径段生成**（避免配置并发更新导致派生不一致）；任务创建后其分支名不随后续前缀配置变化。slug 来源按以下优先级确定：创建请求显式提供分支 slug（trim 后非空）时，MUST 直接使用该 slug，MUST NOT 调用 LLM 命名或机械 slugify，且最终分支名 MUST 通过 `git check-ref-format --branch` 校验（非法 → invalid_input）、MUST NOT 与既有分支冲突（冲突 → **conflict**，与既有创建分支冲突错误码一致）；任一失败零副作用（MUST NOT 落 creating 行）；未提供时维持既有策略——当 AI 配置可用（见 ai-provider-config spec 的可用性判定）时，SHALL 调用 LLM 将任务名提炼为语义化英文 kebab-case slug（≤50 字符，匹配 `^[a-z0-9]([a-z0-9-]{0,48}[a-z0-9])?$` 且不命中无意义词表，否则视为失败）；LLM 调用失败、超时、输出非法或 AI 未配置时，MUST 回退到机械 slugify（与既有行为一致，空结果兜底 `task`）。**AI 错误本身 MUST NOT 向用户返回、MUST NOT 阻断任务创建**——命名回退后创建流程继续，但随后仍可能因既有的分支名校验/冲突等前置检查失败（该语义不变）。LLM 调用 SHALL 设置超时（≤10s）且发生在任何副作用（落库、worktree add）之前。`kind=dir` 项目的任务与 repo 项目 local-path 模式任务创建 MUST NOT 接受分支 slug 参数（提供 trim 后非空值即 invalid_input；缺失/null/trim 后空串视为未提供）。

新建任务（worktree 模式）的 worktree 路径 MUST 为 `<dataDir>/worktrees/<projectName-slug>/<branchPathSlug>-<rand4>/`：`projectName-slug` 为项目名经规范化（小写、非 `[a-z0-9-]` 折叠为 `-`，允许为空）的结果，为空时回退 `project-<projectID前8位>`，且 MUST 截断至 ≤50 字符；`branchPathSlug` 为分支名去掉创建时刻读取的 `<branch-prefix>/` 前缀、再经规范化（小写、非 `[a-z0-9-]` 折叠为 `-`——显式 slug 含 `/`、大写、非 ASCII 字符时在此步折叠，如分支 `ocdeck/feature/X` 的目录段为 `feature-x`；规范化只作用于目录段，分支名本身保留 git 合法字符）后截断至 ≤50 字符的目录段（截断后去尾部 `-`，截空时兜底 `task`）——目录段是分支名的派生展示，**分支名本身行为不变**（机械 slugify 无长度限制），DB 落库的 `worktree_path` 为唯一事实源，MUST NOT 从目录反推分支；`rand4` 为 4 位小写字母数字随机后缀（crypto/rand）。熵失败语义：Go 1.24 起 `crypto/rand` 底层熵失败为不可恢复 fatal（进程终止，天然满足零副作用）；实现保留 error 返回路径作为可注入熵源的防御 seam，**当使用可注入熵源且其返回错误时 MUST 返回错误且零副作用**。目录碰撞检测 MUST 在落库前以无副作用的存在性检查完成：碰撞时重新生成后缀（≤3 次），3 次均碰撞 MUST 返回错误且不产生任何副作用。路径在创建时确定并落库，此后删除/挂起/激活/重试等全部生命周期操作 MUST 按 DB 记录的 `worktree_path` 执行，**MUST NOT 按新格式重算**——既有任务（含旧 `<projectID>/<taskID>` 格式路径）行为不变，不做迁移。worktree 创建在任何文件/git 副作用前 MUST 通过 `<dataDir>/worktrees` 根的包含性校验。

创建流程（worktree 模式）在 worktree 创建成功后、提交 suspended 前，SHALL 执行项目配置的 inherit 文件继承（语义见 project-lifecycle-config spec），inherit 失败 MUST NOT 阻断创建。提交 suspended 后：若项目配置了 init script，SHALL 先异步执行 init 并仅在成功后触发自动激活；init 失败 MUST NOT 触发激活，任务保持 suspended 且 init_status=failed（init 状态机见 project-lifecycle-config spec）；激活失败任务落挂起并记录 last_error，用户可手动重试激活。项目未配置 inherit/init 时，创建流程与既有行为完全一致。

repo 项目 worktree 模式任务创建 SHALL 支持可选基线分支参数 `base_ref`：外部输入为短名（本地分支 `feature-x` 或远端分支 `origin/feature-x`），缺省（空）从项目默认分支切出（向后兼容）。系统 MUST 先对短名执行 `git check-ref-format --branch <短名>` 规范校验，再将输入按 `refs/heads/<name>` → `refs/remotes/<name>` 顺序探测（heads 优先：本地与远端同名时解析为本地分支），仅接受这两个命名空间（拒绝 tag/SHA/任意表达式），经 `git rev-parse --verify` 存在性校验；任一环节失败 MUST 返回 invalid_input。base_ref 校验为无副作用前置检查，MUST 在落 creating 行之前完成；提供非法/不存在 base_ref 时 MUST NOT 产生落库或 worktree 副作用。**解析后的全限定 ref MUST 随任务落库（`tasks.base_ref`），包括缺省创建（落库 `refs/heads/<项目默认分支>`）**；Retry 重试 MUST 使用落库的全限定 ref，MUST NOT 重读项目默认分支；worktree 模式任务落库值为空 MUST fail-closed 报错（空值仅 dir 任务与 local-path 模式任务使用）。Retry 保证使用同一 ref（分支名），不保证同一 commit（分支 tip 移动后按当前 tip 重建，与既有语义一致）。任务分支命名逻辑与基线解耦、行为不变。`kind=dir` 项目的任务与 repo 项目 local-path 模式任务创建 MUST NOT 接受 `base_ref`（提供 trim 后非空值即 invalid_input；缺失/null/trim 后空串视为未提供）。

系统 SHALL 提供项目分支列表只读查询 `GET /api/v1/projects/{id}/branches`（本地+远端分支，`git branch`/`git branch -r`，不进入仓库写锁）：返回稳定排序、去重后的短名 JSON 数组（如 `["feature-x","main","origin/feature-x"]`），本地分支在前、远端分支在后，按 `%(symref)` 元数据排除远端 symbolic ref（如 `origin/HEAD`）；返回的短名 MUST 可直接作为 `base_ref` 输入。dir 项目调用该查询 MUST 返回 invalid_input。

系统 SHALL 提供远端刷新 `POST /api/v1/projects/{id}/branches/refresh`：对每个 remote 执行 `git fetch --no-tags --no-recurse-submodules --no-write-fetch-head --prune --refmap='+refs/heads/*:refs/remotes/<remote>/*' <remote> '+refs/heads/*:refs/remotes/<remote>/*'`（`--refmap` + 命令行显式 refspec 完全取代 `remote.*.fetch` 配置，保证仅写入 `refs/remotes/<remote>/*`——mirror remote、自定义 refspec、fetch.pruneTags 等配置 MUST NOT 使 fetch 触碰 `refs/heads/*` 或 tags；本机 git CLI，30s 硬上限，`GIT_TERMINAL_PROMPT=0`，子进程按进程组终止）后在同一 repo 写锁内重新枚举并返回同构短名数组。fetch 全程 MUST 持有该 repo 写锁（与 worktree add/remove 串行），同 repo 并发 refresh MUST 合并为单次 fetch（singleflight，等待者共享结果且 MUST 响应自身 context 取消），不同 repo 可并行。refresh MUST fail-closed：fetch 失败/超时/取消返回 git_error，MUST NOT 返回 200 伪装最新；dir/未知 kind MUST fail-closed。fetch MUST NOT 移动既有任务本地分支与 worktree HEAD，MUST NOT 覆盖用户 `FETCH_HEAD`。任务创建 MUST NOT 隐式 fetch（仍按本地 ref 校验）。

#### Scenario: 创建任务（repo 项目）

- **WHEN** 用户在 repo 项目下创建任务（提供任务名称，未选择运行模式或选择 worktree 模式）
- **THEN** 系统创建 worktree 与分支，任务进入挂起状态，随后**自动触发激活**（异步启动进程组并锚定 session）；激活失败任务落挂起并记录 last_error，用户可手动重试激活

#### Scenario: 用户指定分支 slug 创建（repo 项目）

- **WHEN** 用户在 repo 项目下以 worktree 模式创建任务并提供合法分支 slug（如 `my-feature`）
- **THEN** 系统直接使用该 slug，不调用 LLM 命名与机械 slugify，分支名为 `<branch-prefix>/my-feature`，其余创建流程不变

#### Scenario: 用户指定 slug 非法（repo 项目）

- **WHEN** 用户在 repo 项目下以 worktree 模式创建任务，提供的分支 slug 使最终分支名未通过 `git check-ref-format --branch` 校验
- **THEN** 系统返回 invalid_input 明确错误，MUST NOT 落 creating 行、零副作用

#### Scenario: 用户指定 slug 冲突（repo 项目）

- **WHEN** 用户在 repo 项目下以 worktree 模式创建任务，提供的分支 slug 使最终分支名与既有分支冲突
- **THEN** 系统返回 conflict 明确错误（与既有创建分支冲突错误码一致），MUST NOT 落 creating 行、零副作用

#### Scenario: 默认前缀配置变化不影响创建后任务（repo 项目）

- **WHEN** 任务以前缀 `ocdeck` 创建（分支 `ocdeck/<slug>`），随后全局前缀配置被修改为其他值
- **THEN** 该任务的分支名、worktree 路径与全部生命周期操作保持原样；仅之后创建的新任务使用新前缀

#### Scenario: dir / local-path 模式拒绝分支 slug

- **WHEN** 用户对 dir 项目任务或 repo 项目 local-path 模式任务创建时提供非空分支 slug
- **THEN** 系统返回 invalid_input（无分支命名语义），零副作用，MUST NOT 落 creating 行

#### Scenario: LLM 生成语义化分支名（repo 项目）

- **WHEN** AI 配置可用，用户在 repo 项目下以 worktree 模式创建中文任务名任务（如「接入AI与worktree命名优化」），且未提供分支 slug
- **THEN** 系统调用 LLM 生成英文 slug，分支名为 `<branch-prefix>/<ai-slug>`（默认前缀下如 `ocdeck/ai-worktree`），worktree 目录为 `<dataDir>/worktrees/<projectName-slug>/<ai-slug>-<rand4>/`（AI 路径下目录段与分支 slug 一致）

#### Scenario: AI 未配置或失败时回退（repo 项目）

- **WHEN** 用户在 repo 项目下以 worktree 模式创建任务且未提供分支 slug，AI 未配置、调用失败/超时、或输出未通过清洗门禁
- **THEN** 系统回退到机械 slugify 生成分支名，AI 错误不向用户暴露；创建流程继续，随后仍遵循既有前置检查语义（如分支冲突时报错）

#### Scenario: 新路径格式的人类可读目录（repo 项目）

- **WHEN** 用户在 repo 项目下以 worktree 模式创建新任务
- **THEN** worktree 目录为 `<dataDir>/worktrees/<projectName-slug>/<branchPathSlug>-<rand4>/`（branchPathSlug 为分支名去其 `<branch-prefix>/` 前缀后截断 ≤50 字符的目录段，分支名本身不变），项目名与分支语义可从路径直接辨认；存量旧格式任务的目录与全部生命周期操作（含创建重试）不受影响

#### Scenario: 纯中文项目名的目录回退（repo 项目）

- **WHEN** repo 项目名称规范化后为空（如纯中文项目名），用户以 worktree 模式创建任务
- **THEN** 目录第一段为 `project-<projectID前8位>`，保证非空、合法、可区分

#### Scenario: 目录碰撞重试（repo 项目）

- **WHEN** repo 项目 worktree 模式任务创建时，落库前的存在性检查发现目标目录已存在
- **THEN** 系统重新生成 4 位随机后缀重试（≤3 次）；3 次均碰撞则返回错误，不产生落库或 worktree 副作用

#### Scenario: 配置 init 的项目创建任务（repo 项目）

- **WHEN** repo 项目配置了 init script，用户以 worktree 模式创建任务
- **THEN** worktree 创建 → inherit 复制 → 挂起（init_status=pending）→ init 执行成功 → 自动激活

#### Scenario: init 失败停留在挂起（repo 项目）

- **WHEN** 创建链中 init script 执行失败
- **THEN** 任务保持挂起、init_status=failed、init_error 落库，无 serve/tui 会话，用户可查看日志并 Re-run

#### Scenario: 未配置项目行为不变（repo 项目）

- **WHEN** repo 项目未配置 inherit patterns 与 init script，用户以 worktree 模式创建任务
- **THEN** 创建流程与既有行为一致：worktree 创建后直接自动激活，init_status=none

#### Scenario: Probe 冷启动重试（repo 项目）

- **WHEN** 创建后自动激活时 capability probe 因冷启动超时/网络类错误（ErrServeNotReady）失败
- **THEN** 系统保持 serve 会话并短退避重试（共 3 次尝试，退避 2s/4s）；任一次成功则激活继续；全部失败才落 suspended 并记录 last_error（语义与现状一致）。结构不兼容（ErrCapabilityMismatch）与凭据错误（ErrUnauthorized）不重试

#### Scenario: 分支名冲突（repo 项目）

- **WHEN** repo 项目 worktree 模式任务创建时，生成的分支名已存在（无论由 LLM 生成还是 slugify 回退）
- **THEN** 系统报错并提示用户更换任务名称

#### Scenario: 指定基线分支创建（repo 项目）

- **WHEN** 用户以 worktree 模式创建 repo 任务并提供 `base_ref` 短名（本地分支 `feature-x` 或远端分支 `origin/feature-x`）
- **THEN** 系统校验该分支存在后，worktree 从该基线切出（任务分支命名不变），解析后的全限定 ref 落库供 Retry 使用

#### Scenario: 默认基线行为不变（repo 项目）

- **WHEN** 用户以 worktree 模式创建 repo 任务且未提供 `base_ref`
- **THEN** worktree 从项目默认分支切出，与既有行为一致；落库 `refs/heads/<项目默认分支>`

#### Scenario: 同名本地与远端分支的解析优先级（repo 项目）

- **WHEN** 用户以 worktree 模式创建任务并提供短名 `origin/feature-x`，仓库同时存在本地分支 `origin/feature-x`（`refs/heads/origin/feature-x`）与远端分支 `origin/feature-x`（`refs/remotes/origin/feature-x`）
- **THEN** 系统按 heads 优先解析为本地 `refs/heads/origin/feature-x`（全限定 ref 落库，Retry 不受远端变化影响）

#### Scenario: 缺省基线创建后默认分支变化的 Retry（repo 项目）

- **WHEN** 以 worktree 模式缺省基线创建的任务落 creation_failed，随后项目默认分支被修改，用户 Retry
- **THEN** Retry 仍使用创建时落库的 `refs/heads/<原默认分支>` 重建 worktree，不受默认分支变化影响

#### Scenario: 非法或不存在的基线分支（repo 项目）

- **WHEN** 用户以 worktree 模式创建 repo 任务，提供的 `base_ref` 未通过 check-ref-format、不是本地/远端分支、或分支不存在
- **THEN** 系统返回 invalid_input 明确错误，MUST NOT 落 creating 行、零副作用

#### Scenario: dir 项目拒绝 base_ref

- **WHEN** 用户对 dir 项目创建任务并提供非空 `base_ref`
- **THEN** 系统返回 invalid_input（纯目录项目无基线分支语义），零副作用

#### Scenario: Retry 使用落库基线（repo 项目）

- **WHEN** 以 worktree 模式、`base_ref=origin/feature-x` 创建的任务落 creation_failed 后 Retry，期间项目默认分支已被修改
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

#### Scenario: local-path 模式创建任务（repo 项目）

- **WHEN** 用户在 repo 项目下以 local-path 模式创建任务
- **THEN** 系统不创建 worktree/分支、不执行分支命名与校验、不执行 inherit；任务落库 `branch` 为空、`worktree_path` 等于项目路径、`base_ref` 为空、`mode=local-path` 持久化，随后按项目 init 配置走 InitRunner 或直接自动激活；init/激活开始前项目目录内零新增文件（允许 dirty working tree，不做任何 git 状态检查）

#### Scenario: local-path 模式拒绝 base_ref

- **WHEN** 用户对 repo 项目以 local-path 模式创建任务并提供非空 `base_ref`
- **THEN** 系统返回 invalid_input（就地运行无基线分支语义），零副作用，MUST NOT 落 creating 行

#### Scenario: local-path 模式目录消失时拒绝创建

- **WHEN** 用户以 local-path 模式创建任务，但项目路径已不存在或不再是目录
- **THEN** 系统以 invalid_state 拒绝，MUST NOT 落 creating 行，零副作用

#### Scenario: local-path 模式创建重试

- **WHEN** local-path 模式任务处于 creation_failed（配置读取失败/提交点失败），用户 Retry
- **THEN** 系统跳过 worktree 产物验证与分支检查，仅校验项目目录仍存在后重读配置并提交，随后按 init 配置触发 InitRunner 或自动激活（与 dir 任务重试语义一致）

#### Scenario: local-path 模式并行多任务

- **WHEN** 同一 repo 项目下已存在活跃的 local-path 模式任务，用户再以 local-path 模式创建/激活新任务
- **THEN** 系统允许并行（无互斥锁，符合无人工并发配额语义），创建面板的低可见度提醒已在模式选中时展示「多任务共享同一目录」语义

## ADDED Requirements

### Requirement: 任务名称修改

系统 SHALL 支持修改已创建任务的名称。修改任务名 MUST 为独立操作，MUST NOT 联动修改分支名、worktree 路径或其他字段。任务名校验沿创建入口语义：trim 后为空即 invalid_input（零副作用），合法时**原值存储**（保留首尾空白），不强制唯一。名称修改不受任务状态门禁（任意状态允许，由任务锁串行）。名称修改 MUST 持久化到任务记录；名称、分支与环境快照的持久化变更 MUST 在单个事务中原子提交（任一失败整体零部分写入）。对当前已激活的任务，新名称 MUST 同步到运行时环境快照（仅改写 `OCDECK_TASK_NAME` 键，其余键与快照元数据原样保留；新拉起的终端/init 进程立即使用新名；已在运行的进程环境变量为 OS 级不可变，不在同步范围内），并 best-effort 同步已有会话标题——serve 提供会话标题更新端点时立即更新，端点不存在或调用失败时 MUST 降级（不阻断改名、记录提示），标题在后续新建会话时使用新名；未激活任务在下次激活时生效：新建会话以新名创建标题，锚定复用已有会话时 best-effort 同步标题（不支持则降级）。名称修改对全部任务形态（worktree / local-path / dir）可用。修改失败 MUST 保持原名称不变（零部分写入）——"原名称"以两阶段语义（见「任务分支改名」）中阶段一收敛成功并重读后的状态为基准。

#### Scenario: 修改挂起任务名称

- **WHEN** 用户将一个挂起任务从「旧名字」改名为「新名字」
- **THEN** 任务记录名称更新为「新名字」，任务列表与工作台展示新名字，下次激活时运行时环境与会话标题使用新名字

#### Scenario: 修改已激活任务名称立即生效

- **WHEN** 用户修改一个处于活跃状态的任务名称
- **THEN** 任务记录与运行时环境快照立即更新为新名称（新拉起进程生效）；已有会话标题经 serve 会话更新端点 best-effort 同步，serve 不支持或调用失败时改名仍成功、标题在后续新建会话时使用新名

#### Scenario: serve 不支持会话标题更新时降级

- **WHEN** 用户修改活跃任务名称，目标 serve 无会话标题更新端点或调用返回不支持
- **THEN** 改名整体成功，任务记录与环境快照已更新，系统记录提示（标题未同步），不报错、不回滚

#### Scenario: dir / local-path 任务改名

- **WHEN** 用户对 dir 项目任务或 local-path 模式任务修改名称
- **THEN** 名称修改正常生效（无分支/worktree 语义牵扯）

#### Scenario: 空名称拒绝

- **WHEN** 用户提交 trim 后为空的任务名称
- **THEN** 系统返回 invalid_input，任务名称保持原值，零副作用

### Requirement: 任务分支改名

系统 SHALL 支持对 `kind=repo` 项目 worktree 模式任务单独修改分支 slug（与任务名称修改相互独立）。仅稳定状态（active / suspended / archived）且分支记录非空的任务可执行；过渡状态、失败状态（creating / creation_failed / activating / suspending / deleting / deletion_failed）与 dir / local-path 任务 MUST 拒绝（invalid_state），零副作用。输入为 slug（trim 后非空，trim 后为空视为未提供、不进入分支改名路径）；新分支名 MUST 沿用该任务当前分支的原有前缀（当前分支名最后一个 `/` 之前的部分；当前分支无 `/` 时新分支名即为 slug 本身），MUST NOT 使用全局前缀配置的当前值。输入 slug 换算出的新分支名与当前分支名相同（同值）时 MUST 跳过分支改名路径（不触发状态门禁与 git 操作），整次保存按其余字段处理。新分支名 MUST 通过 `git check-ref-format --branch` 校验（非法 → invalid_input）、MUST NOT 与既有本地分支冲突；远端追踪分支不参与冲突判定，也不得被修改。（冲突 → conflict）；任一失败零副作用。改名前 MUST 在 repo 写锁内完成 HEAD 身份验证：目标 worktree 的 symbolic HEAD 指向任务记录的当前分支，不匹配（含 detached HEAD、路径缺失）即 invalid_state 拒绝，零副作用。改名结果 MUST 区分两类：「确定未生效」——git 与任务记录均保持原状、恢复意图已清除；「待恢复」——git 结果未知或记录提交失败且补偿失败时，恢复意图保留、结果未收敛，由恢复路径最终收敛，此时不承诺即时恢复原分支名。改名成功 MUST 原子完成：worktree 磁盘路径不变、worktree HEAD 指向的提交不变、任务记录的分支字段更新为新分支名。改名 MUST NOT 触碰远端分支，MUST NOT 触发任务状态流转。改名过程被中断（如进程在 git 改名与记录提交之间退出）时，系统 MUST 通过持久化的恢复意图（覆盖同次保存的名称、分支与环境快照目标值）在重启或重试时收敛：git 侧已是新名则以意图内容原子补做整次提交，git 侧仍是旧名则清除意图（整次保存视为未生效）；无法明确判定时 MUST 保留意图并返回明确错误。恢复意图未收敛期间，新的任务信息修改请求 MUST 先完成收敛、无法收敛则以 conflict 拒绝；依赖分支的破坏性操作（任务删除）MUST 以 conflict 拒绝。**两阶段语义**：任务信息修改请求分两阶段——阶段一「历史意图收敛」（存在未收敛恢复意图时执行，其恢复提交独立生效，不受本次修改的零副作用承诺约束）、阶段二「本次修改」。本 capability 中全部"保持原值 / 原分支名不变 / 零副作用"断言 MUST 以**阶段一收敛成功并重新读取后的状态**为基准：阶段一恢复提交产生的新值不被视为对"原值"承诺的违反。**任何将改变任务状态或 env 快照的生命周期入口（挂起、激活/创建后自动激活、运行期自动重拉、关停清理），在执行首个状态/快照变更步骤前 MUST 先执行同一收敛**：收敛成功则按既有语义继续；不可收敛时——挂起 MUST 以 conflict 拒绝（防止清除快照后恢复补提交将其复活）、激活 MUST 保持 suspended 并仅记录 last_error（不进入清理快照的激活失败补偿）、运行期自动重拉 MUST 跳过该任务并记录 last_error（不阻断其他任务）。**启动 reconcile MUST 在只读模式预检之后、生命周期恢复/清理之前执行收敛；R1 任一未收敛错误（HEAD 无法判定 / 补提交失败 / 清意图失败）MUST 使 reconcile 返回错误、拒开 HTTP、不进入后续生命周期恢复**（沿用既有对账 fail-closed 语义）。**关停清理 MUST 将状态/快照写入与进程终止分离**：收敛不可收敛时保留意图、任务状态与快照，但既有进程终止与 goroutine 清理照常执行（kill 模式终止全部任务进程的既有义务不被豁免），收敛与清理错误聚合后以非 nil 返回 Shutdown（watchdog 按既有契约保留兜底，本次退出不报告为干净成功）。补提交后 MUST 重新读取任务列表再进入后续生命周期处理。**已知限制**：slug 含 `/` 时原请求重放非幂等（如 `ocdeck/old` 提交 `feature/X` → `ocdeck/feature/X`，原样重发会继续改名）；系统 MUST NOT 承诺原请求安全重放，UI 在保存结果不确定时 MUST 先刷新任务、以当前分支重新确认目标 slug 后再提交。

#### Scenario: 改名中断后重试收敛（git 已是新名）

- **WHEN** 一次同时修改名称与分支的保存在 git 成功但记录提交前被中断，用户重试或服务端重启
- **THEN** 系统识别 worktree 实际检出分支已是新名，按恢复意图原子补做整次提交（含新名称与环境快照）并清除意图，任务记录与 git 状态一致，无部分成功

#### Scenario: 改名中断后重试收敛（git 仍是旧名）

- **WHEN** 分支改名在意图写入后、git 执行前被中断（或 git 失败已补偿回滚）
- **THEN** 系统识别 worktree 实际检出分支仍是旧名，清除改名意图，任务保持原状可再次发起改名

#### Scenario: 改名意图未收敛时拒绝删除

- **WHEN** 任务存在未收敛的改名意图，用户执行删除
- **THEN** 系统以 conflict 拒绝删除并提示先完成改名恢复，任务与 worktree 保持原状

#### Scenario: 改名意图未收敛时挂起先收敛

- **WHEN** 活跃任务存在未收敛的改名意图，用户执行挂起
- **THEN** 系统先执行收敛：收敛成功（补提交或清除意图）后继续挂起流程；不可收敛则以 conflict 拒绝挂起，任务与快照保持原状

#### Scenario: 运行期自动重拉前先收敛

- **WHEN** 任务 serve 进程异常退出触发运行期自动重拉（非启动对账），且该任务存在未收敛的改名意图
- **THEN** 系统在自动重拉的状态变更前（ensureRecovery 的状态 CAS 之前）先执行收敛；收敛成功后按重新读取的任务记录继续重拉；不可收敛则跳过该任务本次重拉并记录 last_error，不阻断其他任务

#### Scenario: persist 启动对账遇未收敛意图 fail-closed

- **WHEN** 服务端重启（persist 模式）启动对账时发现任务存在未收敛的改名意图，且收敛出现任一错误（HEAD 无法判定 / 补提交失败 / 清除意图失败）
- **THEN** 对账返回错误、HTTP 拒开、不进入后续生命周期恢复/清理；错误信息指向需人工修复对应 worktree 后重启

#### Scenario: kill 模式关停遇未收敛意图

- **WHEN** kill 模式服务端关停清理时任务存在未收敛的改名意图且收敛失败
- **THEN** 该任务的意图、状态与快照保留不变，但既有进程终止与 goroutine 清理照常执行；收敛与清理错误聚合后 Shutdown 返回非 nil，watchdog 按既有契约保留继续运行，本次退出不报告为干净成功

#### Scenario: 历史收敛成功但本次字段非法

- **WHEN** 请求进入时任务存在未收敛意图（如 git 已是新名、名称目标 B 未落库），本次请求携带非法字段（如空名称）
- **THEN** 阶段一收敛先完成历史恢复提交（B 等新值生效），阶段二校验本次字段返回 invalid_input、本次字段不落账——"原值"基准为收敛后重读的状态，历史恢复提交不视为违反零副作用承诺

#### Scenario: 空保存请求触发收敛

- **WHEN** 任务存在未收敛的改名意图，用户提交不含任何字段修改的保存请求
- **THEN** 系统执行收敛：成功则返回当前任务信息（本次请求零新业务写入）；不可收敛则以 conflict 拒绝

#### Scenario: 嵌套 slug 原样重放非幂等

- **WHEN** 分支为 `ocdeck/old` 的任务以 slug `feature/X` 改名成功（新分支 `ocdeck/feature/X`），随后原样重发同一保存请求
- **THEN** 系统按当前分支前缀重新构造目标（`ocdeck/feature/feature/X`）——重放不被视为同值保存；UI 在保存结果不确定时须先刷新任务再重新确认目标 slug

#### Scenario: 挂起任务分支改名成功

- **WHEN** 用户将分支为 `ocdeck/old-slug` 的挂起 worktree 任务的分支 slug 改为 `new-slug`
- **THEN** 本地分支原子改名为 `ocdeck/new-slug`，worktree 路径不变、HEAD 提交不变，任务记录分支字段更新为 `ocdeck/new-slug`，工作台展示新分支名

#### Scenario: 沿用原分支前缀

- **WHEN** 全局前缀配置已从 `ocdeck` 修改为 `team`，用户对分支为 `ocdeck/old-slug` 的存量任务执行分支改名（slug 输入 `new-slug`）
- **THEN** 新分支名为 `ocdeck/new-slug`（沿用该任务原有前缀），而非 `team/new-slug`

#### Scenario: 无分支任务拒绝分支改名

- **WHEN** 用户对 dir 项目任务或 local-path 模式任务执行分支改名
- **THEN** 系统返回 invalid_state，零副作用

#### Scenario: 过渡/失败状态拒绝分支改名

- **WHEN** 任务处于 creating、creation_failed、activating、suspending、deleting 或 deletion_failed 状态，用户执行分支改名
- **THEN** 系统返回 invalid_state，分支名与任务记录保持原状

#### Scenario: 新分支名非法

- **WHEN** 用户输入的 slug 使新分支名未通过 check-ref-format 校验
- **THEN** 系统返回 invalid_input 明确错误，原分支名不变，零副作用

#### Scenario: 新分支名冲突

- **WHEN** 用户输入的 slug 使新分支名与既有本地分支冲突
- **THEN** 系统返回 conflict 明确错误，原分支名不变，零副作用

#### Scenario: 改名失败保持原状

- **WHEN** 分支改名的 git 操作明确失败且分支未被修改（或记录提交失败但补偿回滚成功）
- **THEN** 结果为「确定未生效」：系统返回错误，原分支名与任务记录分支字段保持不变，恢复意图已清除，worktree 路径不变

#### Scenario: 改名结果未收敛时保留恢复意图

- **WHEN** 改名的 git 结果未知（进程被杀/请求取消）或记录提交失败且补偿回滚失败
- **THEN** 结果为「待恢复」：系统返回错误并提示结果未收敛，恢复意图保留，由重启或重试时的恢复路径最终收敛

#### Scenario: 改名不改变提交内容

- **WHEN** worktree 任务存在未提交改动或已提交但未推送的本地提交，用户执行分支改名
- **THEN** 改名后 worktree 工作区内容与 HEAD 提交保持不变（仅分支引用名变化）
