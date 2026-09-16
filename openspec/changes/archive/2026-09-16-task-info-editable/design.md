## Context

当前状态（均已原文核实）：

- 分支名生成唯一出处：`internal/task/crud.go:276` `branch := "ocdeck/" + slug`，前缀硬编码；slug 来自 `SlugNamer`（LLM）/ `Slugify` 回退（`internal/task/naming.go`）。
- worktree 路径段推导：`crud.go:1011-1027` `branchDirSlug` = 去硬编码 `ocdeck/` 前缀 → `normalizeSlug`（小写、非 `[a-z0-9-]` 折叠）→ 截断 ≤50 → 去尾 `-` → 截空兜底 `task`；路径在创建时落库（`tasks.worktree_path`），此后全部生命周期操作按 DB 记录执行。
- DB：`tasks` 表 `name`/`branch`/`worktree_path` 均 NOT NULL；store 现有更新方法仅覆盖 status/notice/env 等（`internal/application/ports.go`），无 name/branch 更新端口。
- API：`internal/api/tasks.go` 仅有 POST create / GET / action / retry / delete 路由，无 PATCH。
- Git 原语：`ValidateBranchName`（check-ref-format）、`BranchExists`（worktree.Manager）、`AcquireRepoLock`（`internal/infrastructure/git/ops.go:360`）均已存在；**锁键按传入路径归一**（`internal/infrastructure/git/exec.go:199`），不会自动把 worktree 路径折算为公共仓库——编排层必须显式以项目公共仓库路径取锁；**无分支改名原语**。
- 激活链路：env 快照在 activate 时合并 `OCDECK_TASK_NAME = row.Name`（`activate.go:202`）并持久化；`OCDECK_TASK_HEAD_BRANCH` 取 `tasks.branch`（env-management spec「生命周期变量注入」）；**挂起时清除快照、同一激活代复用快照、坏快照视为不可自愈**（env-management spec「修改后生效时机」；`activate.go:220` 对缺失快照返回错误）。快照由 `loadEnvSnapshot`（`attach_shell.go:140`）/ `layerEnvSnapshot`（`init_run.go`）消费。
- 会话标题：仅在 `CreateSession(ctx, row.WorktreePath, row.Name)`（`activate.go:1725`）创建时传入；激活可复用旧 anchor（`activate.go:1675`），复用路径不更新标题。`OCClient`（`internal/task/types.go:80-110`）与底层 `*opencode.Client`（仅 GET/POST/DELETE `/session`）均无会话标题更新方法；`internal/infrastructure/opencode/CONTRACT.md:107+` 端点契约摘要无 `PATCH /session/{id}`（该文档声明"以 live probe 为准"）。
- 事件链：非状态真实变更经 `LifecycleService.commitTaskMutation`（`internal/application/task/lifecycle.go:83`）在 `Changed=true` 时发布 `task.activity_changed`，同值 no-op 不发布，Publish 溢出不回滚业务提交。
- 任务锁先例：`tryLockTask`（`internal/task/delete.go:29`）。启动对账先于 HTTP 开放（`internal/task/reconcile.go:17`）。
- 应用级配置先例：`internal/api/palette_config.go` + `internal/infrastructure/palette/store.go`（dataDir JSON、原子写、损坏配置降级默认值 + 可观察加载错误、GET/PUT handler + DTO + 校验）。
- 装配：显式 composition root（`cmd/ocdeck-server/main.go:154`），无 Fx。
- 前端：无任务名/分支编辑入口；表单先例 `LifecycleConfigEditor.tsx`；危险确认 modal 先例 `DeleteTaskModal.tsx`；设置页 `ConfigsTab` 为固定枚举（`web/src/router.ts:49`）+ `SettingsPage.tsx:50-57` TABS。

约束：`git branch -m` 行为已用本地 git 2.55 实测（worktree 检出分支可原子改名，HEAD 跟随、reflog 保留、路径不变；从主 repo 侧对 worktree 检出分支改名同样成功）。

## Goals / Non-Goals

**Goals:**

- 三个相互独立的能力：任务名修改、任务分支改名（slug 输入）、创建时指定分支 slug；外加全局分支前缀配置。
- 全部写路径满足零副作用前置校验、两类失败结果（确定未生效 / 待恢复）显式分离、同值幂等写不推进 `updated_at`。
- 所有机制复用项目既有先例（palette.Store 配置、repo 写锁、capability 降级、commitTaskMutation 事件链），不引入新框架/新依赖。

**Non-Goals:**

- 不修改 worktree 磁盘路径、不迁移存量任务。
- 不修改运行中进程的环境变量（OS 级不可变，物理边界）。
- 不做任务名唯一性强制、不改动状态机与生命周期钩子行为。
- 不为 serve 端发明会话标题更新协议之外的能力（不支持即降级）。
- 不引入通用 saga/编排框架——单意图记录 + 单一收敛函数足够。

## Decisions

### D1: API 形态 — 单端点 `PATCH /api/v1/tasks/{id}` + 唯一请求处理顺序

请求体 `{"name"?: string, "branch_slug"?: string}`，字段缺失（JSON 中不存在或为 null）= 不修改该项。响应返回更新后的任务 DTO。

**请求处理顺序（唯一）：**请求分为两个阶段——**阶段一「历史意图收敛」**（存在 `rename_pending` 时执行，允许独立产生恢复提交，不受本次修改的零副作用承诺约束）与**阶段二「本次修改」**（"任一环节失败零副作用、整次拒绝"仅约束本阶段的新业务写入）。

1. JSON 解码（**新增专用 helper `decodeOptionalTaskInfoPatchJSON`**——既有 `decodeJSON`（projects.go:447）对空 body/解码失败返回 invalid_input，与本端点空 body 幂等语义冲突，MUST NOT 直接复用或修改既有 helper 影响其他调用方）：空 body → 视为 `{}`（200 幂等）；合法 JSON `{}` 或字段为 null → presence 区分（字段未提供）；非法 JSON / 尾随内容 / 类型错误 → invalid_input；`name`/`branch_slug` presence 由 `*string` 指针字段保留。
2. 任务锁（`tryLockTask`）内读取任务；**阶段一**：存在未收敛 `rename_pending` → 执行收敛算法 `R1`（见 D2）；收敛成功 → 进入阶段二（重新读取任务当前值）；不可收敛 → conflict（本次修改不执行）。无 pending → 直接进阶段二。
3. 字段校验：`name` 提供时沿创建入口语义——trim 判空（空 → invalid_input）、**原值存储**（保留首尾空白）；`branch_slug` trim 后为空视为未提供。
4. 与当前值比较，逐字段判定"实际变更"：`branch_slug` 换算出的新分支名等于当前 `branch` → 该字段同值跳过（不进入分支改名路径，不触发状态门禁与 git 操作）。
5. 仅当分支实际变更时：状态门禁（∈ {active, suspended, archived}，否则 invalid_state）、gitless（`branch` 为空）拒绝（invalid_state）、`ValidateBranchName`（非法 → invalid_input）+ `BranchExists`（冲突 → conflict）。名称修改不受状态门禁（全状态允许，由任务锁串行）。
6. 预计算 env 快照目标值（D3 快照矩阵）；快照损坏且需要更新时在任何写入前返回 internal。
7. 分支实际变更 → D2 git 序列；纯名称变更 → 直接进下一步。
8. `CommitTaskInfoUpdate` 单事务提交（含意图清除）。
9. 本地提交成功后，会话标题 best-effort 后处理（D3）。

**字段×状态矩阵（响应契约）：**

| 请求 | 任务状态 | 结果 |
|---|---|---|
| `{}` / 全 null，**无 pending** | 任意 | 200 幂等成功，零变更（跳过快照校验、提交与标题后处理） |
| `{}` / 全 null，**有 pending** | 任意 | 先执行 R1 收敛：成功 → 200 返回当前任务 DTO；不可收敛 → conflict |
| 全部字段同值（无实际变更） | 任意 | 200 幂等成功，零变更（同上跳过后续步骤） |
| 仅 name（合法，含过渡/失败状态） | 任意 | 改名成功（无状态门禁） |
| name trim 后为空 | 任意 | invalid_input，零副作用 |
| slug 同值（换算后 = 当前 branch） | 任意（含 suspended 之外状态） | 按纯名称保存处理，不进 git 路径 |
| slug 实际变更 | active/suspended/archived 且 branch 非空 | 走 D2 |
| slug 实际变更 | 过渡/失败状态，或 gitless | invalid_state，零副作用 |
| slug 非法（check-ref-format 失败） | — | invalid_input，零副作用 |
| slug 冲突（目标分支已存在） | — | conflict，零副作用 |

- **为什么单端点**：UI 单卡片单保存按钮（web-ui-shell spec），双字段一次提交天然匹配原子语义。
- 错误映射沿用既有 handler 模式：invalid_input / invalid_state / git_error / conflict / internal。

### D2: 任务分支改名 — 锁序 + HEAD 身份验证 + 恢复意图 + 失败矩阵

**临界区序列（编排层持锁，git 原语不重复加锁；恢复路径复用同一锁键与身份验证）：**

1. 任务锁（`tryLockTask`）。
2. 以**项目公共仓库路径**（`proj.Path`，非 worktree 路径——锁键不会自动折算）`AcquireRepoLock`。
3. HEAD 身份验证（零副作用）：共用检查仅限仓库归属（`git worktree list` 含 `row.WorktreePath`）与 symbolic HEAD 读取；**普通改名**额外要求 HEAD == `refs/heads/<row.Branch>`（不匹配、detached HEAD、路径缺失 → invalid_state，意图写入前拒绝）。
4. 锁内复查目标本地 ref 不存在（`BranchExists(新名)`，防锁外竞态）。
5. 写恢复意图：`rename_pending` = 同次保存完整目标值 JSON `{"name": <新名或 null>, "branch_old": ..., "branch_new": ..., "env_snapshot": <预计算快照或 null>}`——恢复单元 = 提交单元，仅凭意图即可重建整次本地提交。
6. `git -C <worktreePath> branch -m <旧名> <新名>`（单命令原子：HEAD 跟随、reflog 保留、路径不变——仅指单条 git 命令；跨 git/DB 恢复由意图承担）。
7. git 成功 → `CommitTaskInfoUpdate` 单事务提交（name/branch/快照 + 清除意图）。
8. 失败处置见失败矩阵。

**失败矩阵（两类结果：「确定未生效」= git 与 DB 均保持原状、意图已清；「待恢复」= 意图保留、结果未收敛，由恢复路径最终收敛，不再承诺即时原状）：**

| 失败点 | 结果分类 | 处置 |
|---|---|---|
| 意图写入失败 | 确定未生效 | 返回 internal，零 git/DB 业务变更 |
| repo 锁获取失败 / 请求取消（意图写入前） | 确定未生效 | 返回错误，零副作用 |
| git 明确报错且分支未改 | 确定未生效 | 清除意图，返回 git_error |
| git 结果未知（进程被杀 / context 取消 / 超时） | 待恢复 | 保留意图，返回 git_error（结果未收敛，提示稍后重试或重启收敛） |
| DB 提交失败 → 补偿改回成功 | 确定未生效 | 清除意图，返回 git_error |
| DB 提交失败 → 补偿失败 | 待恢复 | 保留意图，返回 git_error（携带当前真实分支名） |
| 清除意图失败 | 待恢复 | 意图残留，由下次收敛清理 |
| 恢复补提交失败 | 待恢复 | 保留意图；启动路径任一 R1 未收敛错误（含补提交/清意图/HEAD 无法判定）→ fail-closed 拒开 HTTP（对账先于 HTTP 开放，reconcile.go:17），需人工修复 worktree 后重启；运行期重试/其他生命周期入口按仲裁表任务级处置 |

**中断恢复（算法 ID：** **`R1`** **；启动 reconcile 与改名入口共用同一收敛函数，任务锁 + repo 锁 + 共用检查（仓库归属 + HEAD 读取）下执行；恢复 MUST NOT 先要求 HEAD=`row.Branch`，而是按 HEAD 当前值三分支判定）：**

- worktree HEAD 已是 `refs/heads/<branch_new>` → 用意图内容经 `CommitTaskInfoUpdate` 原子补做整次提交（含名称与快照）并清除意图。
- worktree HEAD 仍是 `refs/heads/<branch_old>` → 清除意图（git 未发生或已回滚，整次保存视为未生效）。
- HEAD 指向其他分支、detached、worktree 缺失等无法明确判定 → 保留意图，返回明确错误。
- 意图未收敛期间：新的任务信息修改请求先收敛、不可收敛则 conflict；任务删除 conflict 拒绝（PreflightDelete 依赖 `row.Branch`，`internal/task/delete.go:74-77`）。

**pending × 生命周期入口仲裁表（防止意图快照被后续生命周期动作过期化；统一规则：任何将改变任务状态或 env 快照的入口，在执行首个状态/快照变更步骤前 MUST 先对当前任务执行 `R1` 收敛）：**

| 入口 | pending 存在时行为 |
|---|---|
| `PATCH /tasks/{id}` 信息修改 | 先执行 R1；成功 → 继续本次修改（重读任务当前值）；不可收敛 → conflict，本次修改不执行 |
| 任务删除 | conflict 拒绝（不自动收敛——删除与改名意图语义冲突，要求先经 PATCH 收敛或人工修复 worktree） |
| Suspend（挂起，清除快照） | 先执行 R1（在 `active→suspending` 状态流转之前）；成功 → 继续挂起（快照按既有语义清除）；不可收敛 → conflict 拒绝挂起（防止清除后恢复补提交复活快照） |
| Activate / 创建后自动激活（生成新快照） | 先执行 R1（位置固定：suspended 前置校验之后、`beginActivation` 之前）；成功 → 继续激活；不可收敛 → 保持 suspended、仅记录 last_error，**MUST NOT 进入通常会清理快照的激活失败补偿路径** |
| 自动 Recovery（运行期 serve 异常重拉，`runRecoveryIncident` 路径） | 先执行 R1（位置固定：`ensureRecovery` 的状态 CAS **之前**，而非进入 incident 之后）；成功 → 继续；不可收敛 → 该任务本次 recovery 跳过并记录 last_error（fail-closed 到任务级，不阻断其他任务） |
| 关停清理（kill 模式 Shutdown） | **状态/快照写入与进程终止分离**：先执行 R1；不可收敛 → 保留 pending、任务状态与快照不变，但**既有的进程终止与 goroutine join 仍照常执行**（kill 模式终止全部任务进程的既有义务不因 R1 失败豁免）；R1 错误与清理错误聚合后以非 nil 返回 Shutdown——调用方按既有契约保留 watchdog 兜底（main.go:371：Shutdown 错误时 watchdog 继续运行），本次退出 MUST NOT 报告为干净成功 |
| 启动 reconcile | 收敛执行位置固定：**只读模式预检之后、生命周期恢复/清理之前**；逐任务执行 R1；**R1 任一未收敛错误（HEAD 无法判定 / 补提交失败 / 清意图失败）均使 Reconcile 返回错误、拒开 HTTP、不进入后续生命周期恢复**（沿用既有对账 fail-closed，reconcile.go:17），需人工修复后重启；收敛产生补提交后 MUST 重新读取任务列表再进入生命周期恢复 |

**替代方案**：作废旧分支 + 建新分支（两步非原子）——拒绝；普通重试改名（DB 旧名 git 新名时无法收敛）——拒绝，必须有意图记录。

**已知限制（嵌套 slug 重放非幂等）**：slug 含 `/` 时，构造规则（前缀 = 当前分支最后一个 `/` 之前部分）使"原样重发同一请求"不等价于同值保存（`ocdeck/old` + `feature/X` → `ocdeck/feature/X`；重放 → `ocdeck/feature/feature/X`）。本设计不承诺原请求安全重放；UI 在保存结果不确定（超时/网络错误）时 MUST 先刷新任务、以当前分支重新确认目标 slug 后再提交。

### D3: 任务名修改 — 快照矩阵 + 标题同步协议 + 统一后处理

**env 快照处理矩阵（env-management spec「修改后生效时机」的显式例外，见该 spec delta）：**

| 任务状态 | 快照状态 | 处置 |
|---|---|---|
| 非 active（挂起等正常无快照） | NULL | 只提交业务字段，快照保持 NULL（activate.go:220 的缺失报错仅适用激活路径） |
| active | **NULL（缺失）** | **在任何写入前返回 internal，零副作用**——不生成快照、不调用 `layerEnvSnapshot`、不修改业务字段（快照缺失不可自愈，与 env-management spec 一致） |
| active | 有效快照 | 仅改写本次变更对应键：name 变更 → `OCDECK_TASK_NAME`；分支实际改名 → `OCDECK_TASK_HEAD_BRANCH`（历史快照缺该键时补写）；其余键与快照元数据原样保留；随业务提交同事务持久化 |
| active | 损坏快照（JSON 非法 / vars 缺失） | 在任何写入前返回 internal，零副作用（不可自愈语义与 env-management spec 一致） |
| 仅名称变更 | 有效快照 | 只改 `OCDECK_TASK_NAME`，不补分支键 |

**会话标题同步协议（外部契约闭合）：**

- 调用链：任务 ID → 活跃任务 runtime（`manager.go:241` `runtimes map[string]*taskRuntime`；serve 连接参数与 `instVersion runtime.InstVersion` 由 `taskRuntime`（manager.go:348-352）持有）→ 锚定会话 `row.AnchorSessionID`（activate.go:1675-1676）→ `PATCH /session/{id}?directory=<url.QueryEscape(worktreePath)>`（id 经 `url.PathEscape`）→ 请求体 `{"title": <新名>}` → 成功 200 + Session JSON（校验顶层 `id` 与请求一致）。
- 端点契约来源：实现任务中将向 `internal/infrastructure/opencode/CONTRACT.md` 端点表增补 `PATCH /session/{id}?directory=` 行（请求 `{"title": string}`；成功 200 + Session JSON；404 → 不支持/不可用）——**截至本设计撰写时该行尚未落盘，本行为目标契约声明而非现状断言**；CONTRACT.md、client 方法、API DTO、传递链代码作为同一变更闭环提交，落盘前实现 agent 以本设计为唯一契约来源。
- **能力判定方案（固定，不做 `/doc` 预探测）**：首次直接发送 PATCH，按响应判定——404 → `ErrSessionTitleUnsupported`，该结果按既有 capability registry 模式缓存：key = `taskID + runtime instance version`（`internal/task/diffreview_capability.go:34-41` 先例），runtime 替换（instVersion 变化）时缓存失效重新判定（`diffreview_capability.go:68-76` 先例）；同一 runtime 被多任务使用时按任务键各自判定（代价为每任务至多一次 404）；并发首调 singleflight 合并（同 registry 先例）；网络错误/超时 → best-effort 失败（记 notice），**不缓存**，下次重试。**404 二分取消**：外部契约无法可靠区分路由级 404（端点缺失）与会话级 404（会话已删）——当前 client 的 GET 404 即统一归一为 `ErrSessionNotFound`（client.go:433-445），故 PATCH 404 一律按"不支持/不可用"降级（记 notice），不区分两种语义。
- 其他错误（5xx/网络/超时）→ best-effort 失败 → 记录 notice（固定 code `task_title_sync_failed`，按任务去重）。**notice 清除固定为：本地事务提交成功且标题同步成功后执行**；清除失败仅记录日志，不产生新 notice；提示写入失败不回滚名称。
- **统一后处理入口**：纯名称提交、名称+分支提交、恢复补提交三条路径在本地提交成功后均进入同一 `postCommitTitleSync`；仅当名称实际变更时调用。防旧值覆盖：后处理执行时**重新读取任务当前 name** 再调用（保存由任务锁串行，后处理读到的总是最新名称，最终一致）。
- 无 runtime（非 active）/ 无 anchor → 不启动进程、不产生提示；激活锚定复用已有会话的路径（activate.go:1675）同样走该后处理（best-effort 同步 DB name 到 session title），修正"复用旧 session 标题永不更新"的缺口；新建会话路径（activate.go:1725）天然用新名，无需改动。

- **为什么 best-effort**：serve 端支持面无法在编译期确证，与既有 capability 降级模式（`ProbePromptAsyncCapability`）一致。

### D4: 前缀配置 — 固定端点/DTO/存储语义（镜像 palette 先例）

- 端点：`GET/PUT /api/v1/config/branch-prefix`；DTO `{"prefix": string}`；成功 200 + 当前生效值。
- 存储：`internal/infrastructure/branchprefix` 包，dataDir 下 `branch-prefix.json`；语义逐条镜像 `palette/store.go`：原子文件写入（临时文件 + rename）、写失败不替换内存值、**配置文件损坏（JSON 非法）→ 降级返回默认值 `ocdeck` + 可观察加载错误（日志）**、未配置文件 → 默认值。
- 校验：前缀**原值**匹配 `^[a-z0-9]([a-z0-9-]{0,48}[a-z0-9])?$`（含首尾空白即非法，不做 trim 容错）；非法 → invalid_input，不写盘、内存值不变。
- 前端 tab 固定：`ConfigsTab` 新增 `'branch-prefix'`，深链 `#/configs#branch-prefix`。

### D5: 分支名生成与目录段推导接入前缀（单次读取快照）

- 创建流程在分支命名前**读取一次**前缀配置，以该快照值驱动命名与路径生成（避免配置并发更新导致分支名与目录段派生不一致）。
- `crud.go:276`：`"ocdeck/" + slug` → `<prefixSnapshot>/<slug>`。
- `branchDirSlug`（crud.go:1011-1027）改造：`branchDirSlug(branch, prefix)`——去 `<prefixSnapshot>/` 前缀 → **既有 `normalizeSlug` 原样保留**（小写、非 `[a-z0-9-]` 折叠——显式 slug 含 `/`、大写、非 ASCII 字符时在此步折叠为合法目录段，如 `feature/X` → `feature-x`）→ 截断 ≤50 → 去尾 `-` → 兜底 `task`。显式 slug 的分支值本身保留 git 合法字符，规范化只作用于目录段。
- 存量任务路径已落库不受影响（spec「前缀仅对新任务生效」）。
- 创建请求 `branch_slug` presence 语义（缺失/null/trim 空 = 未提供）；提供时跳过 LLM/slugify。**字段传递链**：`createTaskReq.BranchSlug *string`（API DTO，`internal/api/tasks.go:124-135` 现有 `Name/BaseRef/Mode/PermissionMode` 同位新增；JSON 解码沿既有请求语义——未知字段忽略、null 视为未提供、重复字段按既有 decoder 行为，不新增特殊处理）→ handler 原样透传（trim/presence 判定在 Manager 侧统一执行）→ `application.CreateTaskOptions.BranchSlug *string`（或等价结构）→ Manager `Create` → `createRepo`。**创建校验顺序与既有入口对齐**（canonical 任务运行模式选择的 ①-⑥ 顺序不变），`branch_slug` 插入位置固定为：`JSON 解码 → name → mode → permission_mode → project kind → kind/mode 组合 → branch_slug 与 base_ref 组合拒绝（dir/local-path + 非空 slug → invalid_input，与 base_ref 组合校验同位）→ 读取一次 prefix 快照 → slug/分支校验（check-ref-format 非法 → invalid_input；分支已存在冲突 → **conflict**，与既有创建分支冲突错误码一致，crud.go:287 先例）→ base_ref 解析 → 路径预检 → 落 creating`。任一环节失败零副作用、MUST NOT 落 creating 行。

### D6: 分层落点与提交/事件/读映射链

| 层 | 落点 | 职责 |
|---|---|---|
| domain/task | 无结构变更 | — |
| application/ports.go | 新增 `CommitTaskInfoUpdate(ctx, id, fields TaskInfoUpdate) (MutationResult, error)`；`SetTaskRenamePending(ctx, id, intent string) error`；`ClearTaskRenamePending(ctx, id) error` | 端口 |
| internal/application/task | `LifecycleService` 扩展：业务提交经 `commitTaskMutation` 先例——`Changed=true` 发布 `task.activity_changed`，同值 no-op 不发布，Publish 溢出不回滚 | 事件 |
| infrastructure/store + sqlite adapter | `CommitTaskInfoUpdate` 单事务实现：`TaskInfoUpdate{Name *string; Branch *string; EnvSnapshot *string}`（nil = 不修改该字段），同事务清除意图；同值写 SQL `WHERE` 排除同值（NULL 安全），返回 `Matched/Changed`；迁移新增 `tasks.rename_pending` 可空 TEXT 列 | 持久化 |
| internal/task（Manager / service） | `UpdateTaskInfo` 用例（D1 顺序编排、D2 git 序列、D3 快照矩阵与标题后处理）；创建路径接 slug/前缀快照；恢复收敛函数（启动 reconcile + 改名入口共用） | 用例编排 |
| infrastructure/git | 新增 `RenameBranch(ctx, worktreePath, oldName, newName)` 原语（不加锁，锁由编排层持有） | git 执行 |
| infrastructure/opencode | `UpdateSessionTitle` client 方法（D3 协议表）+ `OCClient` 接口扩展 + CONTRACT.md 增补 `PATCH /session/{id}` 行 + 标题能力缓存（`taskID + instVersion` key，复用既有 capability registry） | 外部契约 |
| infrastructure/branchprefix | 前缀配置 Store（D4） | 配置 |
| internal/api | `PATCH /tasks/{id}` handler + `decodeOptionalTaskInfoPatchJSON`（D1）；`GET/PUT /config/branch-prefix` handler | HTTP 边界 |
| web | `api.ts` 新增 `updateTask` / `getBranchPrefix` / `putBranchPrefix`；`TaskInfoCard`（settings pane）；`NewTaskPanel` 高级选项；`BranchPrefixPanel` + `SettingsPage` tab | UI |

**意图元数据的事件/时间戳语义（对基线规则的显式例外）**：`Changed` 只由业务列 `name`、`branch`、`env_snapshot` 的真实变化决定，`rename_pending` 永不参与 `Changed` 计算；`updated_at` 同样只由业务列真实变化推进。四类事务结果：

| 事务内容 | Changed | task.activity_changed | updated_at |
|---|---|---|---|
| 仅设置 pending | false | 不发布 | 不推进 |
| 业务列真实变化 + 清除 pending | true | 提交成功后发布一次 | 推进 |
| 仅清除 pending（恢复发现 git 仍是旧名） | false | 不发布 | 不推进 |
| 业务列同值 + 清除 pending | false | 不发布 | 不推进 |

（对齐 active-sessions-stream spec 的 `Changed=true` 发布规则。）
**读映射**：`rename_pending` 仅在 store row 与 service 内部消费，MUST NOT 进入公共任务 DTO / TaskSnapshot / facade 转换（任务读路径沿既有链不新增字段）。UI 保存成功后调用共享 projects store 的 `refresh()` 刷新读模型。

### D7: Web UI 组件落点

- `TaskInfoCard`：新组件，挂在 `TaskWorkbenchPage` settings pane（`:731-753`）；展示态字段列表 + 单「编辑」按钮 → 编辑态（name/slug 可编辑、slug 实时预览最终分支名——沿用该任务当前分支前缀、无需请求配置）；slug 实际变更时保存前先弹确认 modal（复用 `DeleteTaskModal` shell 与 `.btn-danger`，文案"旧分支名将废弃、worktree 路径与提交内容不变"）；slug 同值不算变更、不触发确认。
- `NewTaskPanel`（`CommandCenterPage.tsx:782+`）：折叠高级选项区 + slug 输入 + 实时预览 `<prefix>/<slug>`（前缀经 `getBranchPrefix` 拉取；**GET 未成功时预览显示「前缀未加载」占位，不伪造准确预览、不构成提交门禁**）；仅 repo + worktree 模式渲染；slug 不参与提交门禁。
- `BranchPrefixPanel`：新组件镜像 `PaletteConfigPanel`（GET→ready→edit→save、load/save 错误分离），挂 `SettingsPage` 新 tab `'branch-prefix'`。

### D8: 实施顺序建议（tasks.md 细化）

1. 契约与持久化：migration（`rename_pending` 列）、端口、store 实现、读映射。
2. git 原语 + `UpdateTaskInfo` 主流程 + R1 收敛函数（含生命周期入口仲裁接线）。
3. 前缀 Store/装配/API + `UpdateSessionTitle` client（CONTRACT.md 增补）+ 创建路径接入。
4. Web UI 三处 + 端到端验收。

前端 lane 可在 DTO（D1/D4）固定后与后端 2-3 并行；核心文件（crud/ports/store）不宜拆成多 lane 同时改。测试规划要求（tasks.md 逐项映射）：store / worktree / OCClient 的 mock/fake 更新；失败矩阵逐行映射测试（意图写入失败、git 结果未知、DB 提交失败、补偿失败、清意图失败、启动补提交失败）；真实 git worktree 改名测试；标题 404 缓存与 runtime 实例更换失效测试。

## Risks / Trade-offs

- [git 改名成功但 DB 提交失败（或进程中途退出）产生 git/DB 分叉] → 持久化恢复意图 + 启动/重试收敛（补提交或清除意图）；意图未收敛期间删除/新修改 conflict 拒绝；失败矩阵显式区分「确定未生效」与「待恢复」（D2）。
- [serve 不支持会话标题更新，"立即生效"在标题维度降级] → spec 已固化为 best-effort + notice；能力判定固定为首次 PATCH + 404 按 `taskID + instVersion` 缓存（D3）。
- [活跃任务分支改名期间用户外部脚本引用旧分支名] → UI 危险确认显式说明旧名废弃；git 层面 `branch -m` 保留 reflog 可追溯。
- [前缀配置损坏/非法值破坏新任务创建] → PUT 严格校验 + 损坏降级默认值 + 可观察错误（D4）。
- [恢复失败阻塞启动] → 沿既有对账 fail-closed 规则（reconcile.go:17），错误信息明确需人工修复 worktree 后重启；可接受——分叉状态继续运行风险更高。
- [存量任务目录段含旧前缀推导痕迹] → 目录段仅是派生展示，DB `worktree_path` 为唯一事实源，无兼容风险。

## Migration Plan

- 新增 `tasks.rename_pending` 可空 TEXT 列（默认 NULL，JSON 内容），经既有迁移机制执行；迁移失败 → 服务启动 fail-closed（与既有迁移行为一致）。
- 新增 dataDir 配置文件 `branch-prefix.json`（首次保存时创建，未配置 = 默认 `ocdeck`）。
- **回滚前提**：回滚到不识别 `rename_pending` 的旧版本之前，MUST 先收敛全部 pending 意图并核对 git/DB 一致；存在未收敛意图时不得回滚（旧版本不读该列，意图会丢失，git/DB 分叉无人恢复）。列与配置文件本身对旧版本无害（旧代码只读既有列），回滚后二者留存——**不宣称"无残留"**。
