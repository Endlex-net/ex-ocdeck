# Design: add-plain-dir-project

## Context

ocdeck 当前模型：项目 MUST 是 git 仓库（`internal/api/projects.go` 注册时 `git.IsGitRepo` + `git.ResolveDefaultBranch`），任务 MUST 拥有独立 worktree + `ocdeck/<slug>` 分支（`internal/task/crud.go` Create），删除任务会移除 worktree 并删除分支（`internal/task/delete.go` deleteResume ⑤⑥）。

跨项目操作场景下，用户希望把多个仓库的上级目录（非 git 仓库）注册为项目并创建任务，agent 直接在该目录工作，git 操作由用户人工处理。

关键代码事实（设计决策的落点，已经源码核实）：

- `internal/task/crud.go` `Create`：分支命名 → `ValidateBranchName`/`BranchExists` → `newWorktreePath` → 落库(creating) → `wt.Add` → `runInherit`（读 lifecycle 配置 + 复制 gitignored 文件）→ `CommitCreated` → 自动激活。init script 决策依赖 `runInherit` 返回的配置快照。
- `internal/task/delete.go` `Delete`/`deleteResume`：`PreflightDelete`（**包含性校验要求 worktree 位于 `<dataDir>/worktrees` 根下**）→ dirty 快照 → 清理 debt → 删 oc sessions → kill 残余会话 → 二次 dirty 门禁 → pre-delete 脚本 → `wt.Remove`（含 `DeleteBranch`）→ 删 DB 行。
- `internal/task/reconcile.go`：persist reconcile 对 `creating` 任务直接落 `creation_failed`（**不调用** `VerifyWorktreeProduct`），`creation_failed` 保持原状仅清理异常会话。`VerifyWorktreeProduct` 仅在 `retryCreate` 中使用。
- `internal/task/activate.go` `alignSessions`：全量对齐按**目录**枚举 session（`GET /session?directory=<wt>&limit=1000`）并 upsert 进本任务的 `task_sessions`；`resolveAnchorSession` 从本任务最近顶层 session 选 attach 目标。`task_sessions` 仅约束 `(task_id, session_id)`，允许同一 session 归属多任务。
- `internal/task/activate.go` / `suspend.go`：仅以 `row.WorktreePath` 作为进程/会话锚定目录，无其他 git 依赖。
- `internal/task/gitops.go`：status/diff/commit/push 直接对 `row.WorktreePath` 执行 git。
- `internal/store/migrations/`：编号 SQL 迁移（当前至 0007）；`tasks.branch` 为 `TEXT NOT NULL`（允许空串，无需改约束）。
- `internal/api/tasks.go`：task DTO 当前无项目类型字段。

## Goals / Non-Goals

**Goals:**

- 项目注册支持显式 `kind: repo | dir`；dir 项目接受任意存在的目录（不要求 git 仓库）。
- dir 项目任务：无分支、无 worktree，运行目录锚定为项目路径本身；创建/删除/重试/reconcile 全生命周期按 kind 分叉。
- dir 任务删除硬不变量：**ocdeck 内建删除逻辑对用户目录零写删**（用户显式配置的 pre-delete 脚本除外），仅清理 ocdeck 自身数据（DB 行、tmux 进程组、lifecycle 日志目录）与本任务拥有的 opencode session 数据。
- session 归属隔离：任何任务只操作自己拥有的 session（见 D8）。
- dir 任务保留 init script / pre-delete script 能力（项目级用户授权配置）。
- git API 对 dir 任务明确降级报错；Web UI 隐藏 git 入口并标识项目类型。

**Non-Goals:**

- 子仓库自动探测、聚合 git 视图（原 D 方案）。
- 多仓库任务 / 任务组（1:N 模型，原 B 方案）。
- 同 dir 项目并行任务的**文件级**互斥锁或隔离（用户显式接受文件层面无隔离）。
- dir 任务的任何 git 能力（status/diff/commit/push/dirty 门禁）。
- 存量 repo 项目/任务行为的任何改变（D8 的所有权防御 guard 仅在新竞争场景生效，不改变 repo 单任务目录私有语义）。
- SSE 断流期间 dir 任务新 session 的补记（见 D8 取舍，属于已接受的降级，不做目录级兜底认领）。

## Decisions

### D1. 项目类型显式建模：`projects.kind` 列 + 注册参数

DB 迁移（新增 `0008_project_kind.sql`）：`ALTER TABLE projects ADD COLUMN kind TEXT NOT NULL DEFAULT 'repo'`，存量行天然回填 `repo`，零数据迁移。`store.ProjectRow`、`CreateProject` 签名、API DTO 全链路增加 `kind`。

注册 API：请求字段 `kind ∈ {repo, dir}`，缺省 `repo`（向后兼容）；非法值返回 `invalid_input`（HTTP 422）。`kind=repo` 走既有校验；`kind=dir` 跳过 `IsGitRepo`/`ResolveDefaultBranch`，仅校验路径存在且为目录，`default_branch` 落空串。

**fail-closed**：所有按 kind 分叉的 switch MUST 显式处理 `repo`/`dir` 两分支，`default`（未知/损坏值）MUST 在任何副作用前返回内部错误，MUST NOT 默认落入 repo 或 dir 分支。

**备选**：靠 `IsGitRepo` 失败隐式推断 dir —— 拒绝。路径手滑会静默创建语义完全不同的项目。

### D2. dir 任务创建：绕过全部 git 环节，`worktree_path = 项目路径`

`Manager.Create` 在取得项目后按 `proj.Kind` 分叉（主流程的本地变体，非平行流程）：

```
repo（现状不变）                    dir
─────────────────────────────      ─────────────────────────────
slug/分支命名                       │ 跳过（Branch 落空串）
ValidateBranchName/BranchExists    │ 跳过
newWorktreePath 碰撞重试           │ 跳过；wtPath = canonical 项目路径
（无前置门禁）                      │ 目录存在性预检：os.Stat 存在且为目录，
                                  │ 否则 invalid_state 拒绝，MUST NOT 落 creating 行
落库 creating                       │ 落库 creating（Branch="", WorktreePath=项目路径）
wt.Add                              │ 跳过（零文件副作用）
runInherit（配置+复制）             │ 仅读 lifecycle 配置（不枚举/复制 gitignored 文件）；
                                  │ 配置读取失败 → creation_failed（与 repo 同一阻断点语义）
CommitCreated(init_status 决策)     │ 同左（init script 保留）
triggerActivate / startInitRunner   │ 同左（activate 只锚定目录，天然兼容）
```

- dir 创建自身无任何文件/git 副作用；`creation_failed` 仅可能来自配置读取失败或 CommitCreated 失败。
- "创建完成后目录零新增文件"仅限定于 **init/激活开始之前**——init script 与 agent 会话本身是用户授权行为，可以在目录内产生文件。
- `retryCreate` 对 dir 任务：`VerifyWorktreeProduct`/分支检查/`wt.Add` 全部跳过，仅校验项目目录仍存在且为目录（不存在 → 保持 creation_failed 并报错），然后读配置 → `CommitCreated`。
- 分支命名（LLM slug）完全跳过（用户已确认无命名需求）。

### D3. dir 任务删除：步骤矩阵 + 硬不变量

`Delete`/`deleteResume` 在删除序列入口按 kind **一次性分流**为 repo/dir 两条序列，共享步骤抽共用 helper。MUST NOT 依赖 `Branch == ""` 等隐式信号逐步跳过（隐式信号易在后续改动中被破坏，review 时也无法一眼确认 dir 路径绝无 Remove 调用）。

**硬不变量**：dir 删除路径上，ocdeck 内建逻辑 MUST NOT 对 `row.WorktreePath`（=用户目录）执行任何写/删 syscall。**唯一例外**是用户显式配置的 pre-delete script（用户授权操作，与 repo 语义一致，不计入不变量）。`confirmDirty` 参数对 dir 任务接受但忽略（删除不产生目录副作用，无需确认基线）。

步骤矩阵（✓=执行，✗=跳过）：

| 步骤 | repo normal | repo force | dir normal | dir force |
|---|---|---|---|---|
| 前置 git 检查（包含性/dirty/分支占用） | ✓ | ✓ | ✗ | ✗ |
| ① 持久化 delete_mode + 置 deleting | ✓ | ✓ | ✓ | ✓ |
| ② RetryReap 既有 cleanup debt | ✓ | ✓ | ✓ | ✓ |
| ③ 删本任务 oc sessions | ✓ | ✗ | ✓ | ✗ |
| ④ kill 残余 tmux 会话 | ✓ | ✓ | ✓ | ✓ |
| ⑤ 二次 dirty 门禁 | ✓ | ✓ | ✗ | ✗ |
| ⑥ pre-delete script（cwd=项目目录） | ✓ | ✗ | ✓ | ✗ |
| ⑦ 删 worktree | ✓ | ✓ | ✗ | ✗ |
| ⑧ 删本地分支 | ✓ | ✓ | ✗ | ✗ |
| ⑨ 删 DB 行 | ✓ | ✓ | ✓ | ✓ |
| ⑩ 清理任务日志目录 | ✓ | ✓ | ✓ | ✓ |

Force 语义与既有契约完全一致（仅跳过 ③⑥，MUST NOT 跳过 ②）；dir 仅额外跳过 git/文件类步骤，不改变 normal/force 语义。Retry 删除重入：dir 跳过 preflight dirty 快照，按持久化 delete_mode 重入上表序列；`deleteResume` 的 `preflightDirty=nil` 路径即 dir/重入共用。

**deleteResume 重构不可回归点**（拆 helper 时必须原样保留）：deletion_failed 落账使用非取消 ctx（`finishDeletionCtxTimeout`）；pre-delete WG token 持有到 DB 提交点/落账点后恰好一次释放；notice CAS 写回失败聚合进 last_error；oc session 逐项删除错误聚合不短路；tickets 不随 CASCADE 丢失。

### D4. reconcile 与 Retry：保持最简现状

- persist reconcile 对 `creating` 任务统一落 `creation_failed`（现状），dir 任务同样适用，**无需特殊分支**；`creation_failed` 保持原状（现状）。
- dir 任务的目录存在性校验只发生在 `retryCreate`（D2）与 dir 创建前置（D2），reconcile 不新增任何 git/产物验证路径。

### D5. git API 降级：按项目 kind 拒绝

`gitops.go` 四个入口（Status/Diff/Commit/Push）在执行前解析任务所属项目，`kind=dir` → 返回 `invalid_input` 错误，消息明确"project kind is dir (not a git repository)"。不做子仓库探测。

### D6. task DTO 与 API 契约

- task DTO（`internal/api/tasks.go`）新增 `project_kind` 字段（`repo|dir`），由 API 层按 task → project 映射填充（task Manager 的 List/Get 已持 projectID，API 层批量取项目 kind，避免 N+1：List 场景一次取项目详情即可，因任务按项目列表查询）。
- UI 依赖 `project_kind` 做徽标/分支名/git tab/并行提示的全部降级判断，不自行推断。
- **已评估的新消费点**（合并 main 后，cross-project-active-sessions）：`GET /api/v1/sessions/active` 的 `activeSessionDTO` 含 branch 字段，dir 任务 branch 为空时前端已有优雅回退（`branch || worktree_path`，`web/src/pages/ActiveSessionsPage.tsx`），显示项目路径即符合 dir 语义，该 DTO MUST NOT 变更（不加 project_kind），行为天然兼容。

### D7. Web UI：类型标识 + git 入口隐藏 + 并行风险提示 + 删除确认文案

- 项目注册表单增加类型选择（默认 git 仓库）；dir 类型不请求默认分支。
- 项目列表/详情显示 kind 徽标；任务卡片对 dir 项目不显示分支名。
- 任务详情对 dir 项目隐藏 git tab。
- dir 项目下存在 ≥2 个活跃任务时，任务列表显示"多任务共享同一目录、无文件隔离"提示条（纯前端判断）。
- **删除确认弹窗（`web/src/components/DeleteTaskModal.tsx`）按 `project_kind` 分叉文案**：dir 任务 MUST NOT 出现"会删除对应 worktree，不可恢复"等暗示目录删除的文案；改为明确"仅删除任务记录与 opencode 会话数据，不会删除项目目录及其内容"；normal 模式且项目配置了 pre-delete script 时提示该脚本仍会执行；不展示 dirty/worktree 删除确认项。

### D8. session 归属隔离（任何任务只操作自己的 session）

**所有权规则**：一个 opencode session 至多归属一个 ocdeck 任务；任务 MUST 仅对 `task_sessions` 中本任务拥有的 session 执行删除/attach/对齐写回。

**原子 claim（唯一写入口）**：store 层新增原子方法 `ClaimTaskSession(ctx, taskID, sessionID, ...) (claimed bool, ownerTaskID string, err error)`——单事务内"仅当该 sessionID 未被其他 task 拥有时插入/更新本任务行；已被他任务拥有则返回 claimed=false 与 ownerTaskID"。SQLite 单写者语义下事务无竞态，**不加跨任务唯一索引**（避免对存量数据做迁移）。session 归属的全部三个写入入口 MUST 统一经过该方法：

```
入口                repo 任务（目录私有）              dir 任务（目录可共享）
────────────────────────────────────────────────────────────────────────
SSE session.created  ClaimTaskSession；冲突           同左（每任务独立 serve）。
                     （理论上不发生）→ 忽略 +          冲突（双 serve 同目录串流时）
                     记诊断                            → 忽略 + 记诊断
锚定创建              CreateSession 后 ClaimTaskSession  同左。冲突 → 激活失败
                     （新建必不冲突）                    （MUST NOT attach 他任务 session）
全量对齐              原始目录列表逐个 claim +           仅对"原始目录列表 ∩ 本任务 owned 集合"
（alignSessions）     缺席删行（现状语义）               做存在性核对（算法见下）
```

**对齐的 store 事务契约**：store 层新增原子对齐方法（替换 application 层"先 ListTaskSessions 再 AlignSessions"的非原子组合）。沿用既有分层惯例：**task（application）层自有 `AlignMode`/`SessionObservation` 类型，StoreAdapter 转换为 store 层类型**（与 TaskStore/SessionRow 现有解耦一致），store MUST NOT 依赖 `internal/opencode`。

```go
// AlignMode 对齐模式（task 层常量，StoreAdapter 映射到 store 类型；
// 合法值仅两种，未知值 MUST 在任何写入前返回错误——fail-closed）。
type AlignMode int
const ( AlignModeRepo AlignMode = iota + 1; AlignModeOwnedOnly )

// SessionObservation 持久化中立的会话观测（application 层从 opencode DTO 转换，
// 仅含归属写回所需字段：SessionID/CreatedAt/UpdatedAt/ParentID）。
type SessionObservation struct{ ... }

// store 层（经 adapter 转换后）：单事务读 owned 集合 O → 按 mode 处理 listed →
// complete 时仅删 owned 缺席行 → noticeFn 事务内读写 notice。
// mode=repo：listed 逐个原子 claim（未被他任务拥有才插入/更新），冲突 ID 经返回值上报。
// mode=ownedOnly：仅对 listed∩O 刷新 last_seen_at，不做任何 claim。
// complete=false（overflow）：不删任何缺席行。
AlignTaskSessions(ctx, taskID string, mode AlignMode, listed []SessionObservation,
    complete bool, noticeFn func(current sql.NullString) sql.NullString) (conflicts []string, err error)
```

**overflow notice 语义 MUST 与现状逐点一致**（`internal/task/activate.go` alignSessions 现状）：overflow 时 application 层**先经事务外 CAS**（`recordSessionOverflowNotice` / `UpdateTaskNoticeCAS`）写入 session_overflow notice，**再**调用对齐（对齐失败时 notice 保留，与现状一致）；complete 时经对齐事务内 noticeFn 清除该 notice（对齐与清除原子提交，与现状一致）。MUST NOT 把 overflow 写入并入对齐事务（那会改变"对齐失败时 notice 保留"的既有语义）。

owned 快照与刷新/删除/notice MUST 在同一事务内，杜绝"事务外 O 快照期间新 claim 行被 complete 对齐误删"。`TaskStore` 接口、StoreAdapter 与全部 mock 同步。

**session.updated 的 store 契约**：新增 `TouchOwnedTaskSession(ctx, taskID, sessionID string, lastSeenAt int64) (updated bool, err error)`——条件 UPDATE `WHERE task_id=? AND session_id=?`（MUST NOT 插入）；`updated=false`（未归属行）为正常路径，记 debug 日志不报错；store 错误照常传播。现有 `UpsertTaskSession` 在 session.updated 路径 MUST 停用（其插入语义与"不创建归属"冲突）。

**claim 冲突语义**：SSE/对齐路径冲突 → 忽略该 session 并记服务端诊断日志（不阻断）；锚定路径冲突 → 激活失败并落 last_error，MUST NOT attach 不属本任务的 session。**存量重复 owner**（历史遗留同一 sessionID 归属多任务的行）：不加唯一索引、不做启动修复；claim 遇到"已被他任务拥有"即冲突处理（忽略+诊断），存量行随任务删除自然清理。

**session.updated 语义**：仅经 `TouchOwnedTaskSession` 刷新本任务已拥有行的 `last_seen_at`，MUST NOT 创建归属或 claim；未归属 session 的 updated 事件一律忽略（repo/dir 一致）。

**kind 传播的四个运行时入口**：`Activate`、persist 恢复路径（`reconcilePersist → resumeActive → startSSE`）、挂起修复路径（`Suspend` 分支 c → `tryRepairRuntime`）、TUI 重开路径（`ReopenAttach`，`internal/task/attach_shell.go`，无锚定记录或预检 404 时会创建并 claim 新 session）都 MUST 在任何状态修改或运行时副作用前解析并校验项目 kind（显式 repo/dir，未知值报错零副作用），并显式传入 startSSE/alignSessions 选择对齐模式；MUST NOT 只覆盖 Activate 单入口。`ReopenAttach` 的 claim 冲突（锚定创建的新 session 已被他任务拥有，理论边界）→ 返回错误并记录 last_error，任务保持 active 不收敛（TUI 重开失败可重试），MUST NOT attach 不属本任务的 session。

**dir 对齐算法（与缺席删行/overflow 语义兼容的精确顺序）**：

1. 取原始目录列表 L（`ListSessions(dir, 1000)`），按 `len(L)` 判定 `complete/overflow`（判定 MUST 在过滤之前，基于原始列表）；
2. 取本任务当前 owned 集合 O（`task_sessions` 本任务行）；
3. 候选集 C = L ∩ O（dir 任务 MUST NOT 对 L 中未归属本任务的 session 做 upsert/claim）；
4. `complete=true`：对 C 中 session 刷新 `last_seen_at`；删除 O 中不在 L 内的缺席行（仅删本任务 owned 行）；
5. `overflow`：不删任何缺席行，仅刷新 C 的 `last_seen_at`，按现状写 session_overflow notice（基于原始目录列表）。

repo 任务对齐保持现状（目录私有，L 即本任务全部 session），仅把 upsert 换为 claim 作为防御 guard——单任务场景 guard 永不命中，行为不变。

**SSE 归属前提（设计阶段已验证）**：OpenCode 事件总线为进程内实例级——`/event` 订阅进程内 listener，publish 仅 notify 本进程 PubSub，跨进程仅可经 `sync/history` 显式拉取（源码：`server/routes/instance/httpapi/handlers/event.ts`、`core/event.ts`；v1.16.0 起架构稳定，v1.18.9 ↔ 最新 dev 字节级一致）。故同目录双 serve 不串流，dir 任务经本任务 serve 的 SSE claim 归属是安全的。若未来 OpenCode 升级引入存储级事件分发，dir 归属设计 MUST 重新评审。

**Activate 早期 kind 门禁**：`Activate` 在任何副作用（serve 创建等）前 MUST 读取并校验项目 kind（显式 repo/dir，未知值报错零副作用），随后把 kind 显式传入对齐/SSE 逻辑，MUST NOT 在 serve 已启动后才于 alignSessions 内部发现未知 kind。

**已接受的降级**：dir 任务 SSE 断流期间经 TUI 新建的 session 在重连后不被补记（无法与"他人/手工创建"区分）；repo 任务无此限制（目录私有，断流补记语义不变）。

### D9. 项目删除语义不变

删除项目本就不触碰磁盘（spec 现状），dir 项目同样仅删 DB 记录；含任务拒绝删除的门禁不变。

### D10. repo 任务创建可选基线分支 `base_ref`

任务创建 API 增加可选参数 `base_ref`（仅 `kind=repo` 项目接受；dir 项目提供即 `invalid_input`）。缺省（空）= 项目默认分支，向后兼容。

- **输入格式与解析**：外部输入为短名（`feature-x` / `origin/feature-x`）。先执行 `git check-ref-format --branch <短名>` 规范校验（拒绝 `..`/控制字符等非法输入），再按 `refs/heads/<name>` → `refs/remotes/<name>` 顺序经 `git rev-parse --verify` 探测（**heads 优先**：本地与远端同名时解析为本地分支）；仅接受这两个命名空间（拒绝 tag/SHA/任意表达式）；任一环节失败 → `invalid_input` 明确报错。解析 MUST 经 task 层端口（`WorktreeBackend`/adapter 分层新增方法），MUST NOT 在 `crud.go` 直接依赖 git 实现。
- **落库为唯一事实源**：解析后的**全限定 ref** MUST 随任务落库（`tasks.base_ref`），**缺省创建也落库 `refs/heads/<项目默认分支>`**。`tasks` 表新增 `base_ref` 列（迁移 0008 一并处理），**0008 同时将存量 repo 任务回填为 `refs/heads/<其项目 default_branch>`**（迁移时冻结）；此后 repo 任务空值 MUST fail-closed，空值仅 dir 任务使用。
- **生效点**：`Manager.Create` 的 `wt.Add(ctx, repoPath, wtPath, branch, baseRef)`——`baseRef` 由 `proj.DefaultBranch` 改为落库的全限定 ref。任务分支（`ocdeck/<slug>`）命名逻辑不变，与基线解耦。
- **失败语义**：base_ref 解析校验属于无副作用前置检查，MUST 在落 creating 行之前完成（与分支名校验/冲突检查同一阶段）；worktree add 因基线问题失败仍走既有 `creation_failed` 路径。
- **Retry 语义**：`retryCreate` 使用落库的全限定 ref，MUST NOT 重读项目默认分支；repo 任务落库值为空 MUST fail-closed 报错。保证**同一 ref（分支名）**，不保证同一 commit——分支 tip 移动后按当前 tip 重建（与既有"重读默认分支"语义一致，仅确定化 ref 本身）。
- **分支列表查询（本地快照）**：`GET /api/v1/projects/{id}/branches` 保持本地只读快照（`git branch --format` / `git branch -r --format`，不进仓库写锁），返回稳定排序、去重后的短名 JSON 数组（本地在前、远端在后，按 `%(symref)` 元数据排除 symbolic ref）；返回短名 MUST 可直接作为 `base_ref` 输入。dir 项目调用返回 `invalid_input`。
- **远端刷新（refresh）**：新增 `POST /api/v1/projects/{id}/branches/refresh`，对每个 remote（`git remote` 枚举）执行 `git fetch --no-tags --no-recurse-submodules --no-write-fetch-head --prune --refmap='+refs/heads/*:refs/remotes/<remote>/*' <remote> '+refs/heads/*:refs/remotes/<remote>/*'`（**`--refmap` + 命令行显式 refspec 完全取代 `remote.*.fetch` 配置**，保证仅写入 `refs/remotes/<remote>/*`——mirror remote、自定义 refspec、`fetch.pruneTags` 等配置不得使 fetch 触碰 `refs/heads/*` 或 tags；本机 git CLI，继承用户 git config/credential helper/SSH agent，`GIT_TERMINAL_PROMPT=0`；**30s 为硬上限**：始终派生 `context.WithTimeout(ctx, 30s)`，父 context 更短 deadline 自然优先；子进程按进程组终止）后在**同一 repo 写锁内**重新枚举并返回同构短名数组。fetch 全程 MUST 持有 canonical repo 的写锁（与 `worktree.Add/Remove` 串行；`RepoLock` 升级为 context-aware 获取，迁移 Add/Remove/refresh 使用，Push 的 `-u` 写共享 `.git/config` 亦纳入）；同 repo 并发 refresh 经 singleflight 合并（只 fetch 一次，等待者共享结果；**等待者 MUST 响应自身 context 取消/更短 deadline**——done channel + select 等待，ctx 先到即返回，无其他等待者时可取消底层 fetch），不同 repo 并行。refresh 服务端 fail-closed：fetch 失败/超时/取消返回 `git_error`，MUST NOT 返回 200 伪装最新；dir/未知 kind fail-closed。fetch 仅更新对象库与 `refs/remotes/*`（可 prune），MUST NOT 移动既有任务本地分支与 worktree HEAD，MUST NOT 覆盖用户 `FETCH_HEAD`。
- **UI**：普通打开基线选择器先显示本地快照；**首次打开时自动 refresh 一次**，下拉旁保留显式"刷新"按钮；refresh 期间显示 loading 并禁止提交所选远端基线；refresh 失败保留旧列表并明确标注"本地快照未刷新"+ 错误与重试入口（显式降级，不静默冒充最新）。
- **Create 不隐式 fetch**：创建仍按本地 ref 校验；基线 ref 已被 prune/并发删除时按既有语义在落库前拒绝或 Add 阶段落 `creation_failed`。refresh 后 ref tip 变化时 Retry 使用该 ref 当前本地 tip（与"同 ref、非同 commit"语义一致）。
- **UI**：repo 项目创建任务表单增加基线分支下拉（默认选中项目默认分支，支持搜索过滤）；dir 项目不显示该控件。

## Risks / Trade-offs

- [误删用户目录] → D3 入口硬分流 + 不变量写入 spec；实现配 panic/counting mock 测试证明 dir 路径不调用 Namer/git/WorktreeBackend/inherit copy；目录树前后比对作为补充（注意：比对只能证最终态一致，不能证零写删，故 mock 断言为主）。
- [pre-delete 例外打开缺口] → 用户显式决策（"用户授权的操作"）；spec 明确例外边界为"项目配置的脚本"，ocdeck 内建逻辑无任何例外。
- [会话串用/误删] → D8 所有权规则 + 对齐 guard + 交叉归属测试（两 dir 任务同目录：激活/恢复/删除互不影响）。
- [worktree_path 指向 dataDir 之外，破坏既有包含性假设] → 全仓 `WorktreePath` 消费点已枚举（create/delete/retry/reconcile/activate/suspend/gitops/oc serve），每处明确 dir 行为；`PreflightDelete` 包含性保护仅服务 repo 任务。
- [无文件隔离并行任务互相踩踏] → 用户显式接受；UI 提示（D7）；文档写明。
- [`Branch` 空串被既有代码误用] → 全部分叉按 `proj.Kind` 判定；`tasks.branch` 为 `TEXT NOT NULL`，空串合法，无需 DDL。
- [未知/损坏 kind 落入错误分支] → D1 fail-closed：所有 switch 显式 repo/dir，default 副作用前报错。
- [init script 在用户真实目录执行] → 与任务激活行为一致（用户授权），"零新增文件"承诺限定在 init/激活前（D2）。

## Migration Plan

1. `0008_project_kind.sql`：`ALTER TABLE projects ADD COLUMN kind TEXT NOT NULL DEFAULT 'repo'`；`ALTER TABLE tasks ADD COLUMN base_ref TEXT NOT NULL DEFAULT ''`；存量 repo 任务回填 `base_ref = 'refs/heads/' || 项目 default_branch`（迁移时冻结，此后空值仅 dir 任务使用）。
2. store/queries/adapter/API DTO 全链路增 `kind`/`project_kind`/`base_ref`（向后兼容：API 缺省 repo、base_ref 缺省默认分支）。
3. session 归属隔离基础（D8：对齐 guard + dir 对齐变体）——dir 并行任务的前置。
4. task 包按 D2/D3/D4/D10 分叉；gitops 按 D5 降级；分支列表只读 API。
5. Web UI 按 D6/D7/D10。
6. 回滚：本变更纯增量。**代码回滚前置条件**：回滚窗口内未创建新 repo 任务；若回滚窗口内旧代码创建过 repo 任务（会以默认空值写 `base_ref`），再次前滚前 MUST 先将这些空值行回填为 `refs/heads/<其项目 default_branch>`，否则新代码的 fail-closed 会阻塞这些任务的 Retry。除该窗口期写入外，回滚代码即可，新增列留存无害（缺省语义不变）。

## Open Questions

（无——探索与评审决议已收敛：pre-delete 保留为用户授权例外；Force 语义不变；session 归属隔离纳入范围；reconcile 保持最简现状。）
