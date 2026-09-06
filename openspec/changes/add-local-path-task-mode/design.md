# Design: add-local-path-task-mode

## Context

现状：`kind=dir` 项目类型已完整实现「就地运行」语义。但任务行为的分流依据全部是**项目级 `proj.Kind`**，全量审计后分流点清单：

| # | 位置 | 现状行为 |
|---|---|---|
| 1 | `internal/task/crud.go:185` Create | kind → createRepo/createDir |
| 2 | `internal/task/crud.go:529` retryCreate | kind → retryCreateRepo（空 base_ref fail-closed）/retryCreateDir |
| 3 | `internal/task/delete.go:61,135` Delete/deleteResume | kind → repo/dir 删除序列 |
| 4 | `internal/task/activate.go:299` Activate | alignModeForKind |
| 5 | `internal/task/suspend.go:40` Suspend | alignModeForKind |
| 6 | `internal/task/recovery.go:295,400` 恢复 | alignModeForKind |
| 7 | `internal/task/reconcile.go:313` reconcile | alignModeForKind |
| 8 | `internal/task/attach_shell.go:62` shell | alignModeForKind |
| 9 | `internal/task/activate.go:183` layerEnvSnapshot | repo 要求 Branch/base_ref 非空并注入分支变量 |
| 10 | `internal/task/gitops.go:29` assertGitRepoTask | repo 放行 git status/diff/commit/push；本变更改按 D2 有效模式：worktree 与 repo local-path 放行（D8），dir 拒绝（文案不变） |
| 11 | `internal/task/diffreview_adapters.go:186` diff review | 复用同一 git 门禁 |
| 12 | `web/src/pages/TaskWorkbenchPage.tsx:228` | 工作台 Git tab 按 kind 渲染；本变更 isGitless 收窄为 `project_kind==='dir'`（repo local-path 显示，D8） |
| 13 | `web/src/pages/CommandCenterPage.tsx:446` 删除弹窗 | 由 project task summary 驱动（summary 无 mode）；本变更按 D7 透传 mode，弹窗按 `task.mode` 出文案 |
| 14 | `internal/task/crud.go:453-506` 删除 Retry | kind → repo 执行 DirtyFiles 快照 + confirmDirty 门禁（483-505）；dir 跳过 |

本变更把「就地运行」从项目级属性下沉为**任务级运行模式**：repo 项目创建任务时可选 worktree（默认）/ local-path。若 local-path 任务被当作 repo 任务无差别处理：#9 激活直接报错（空 branch）、#4-8 以 AlignModeRepo claim 共享目录 session（多任务互抢）。#10/#11 的 git 操作经 review 裁决**显式放行**（D8：作用于项目目录当前分支、风险由用户自担），不属于事故场景；除 git 门禁外，local-path 的生命周期序列仍与 dir 同款（D2）。

## Goals / Non-Goals

**Goals:**
- repo 项目任务创建支持任务级 `mode`（worktree 默认 / local-path）；除 D8 主动 git 能力外，创建、删除、env、对齐等生命周期语义同 dir
- 上表全部 14 个分流点改由「任务持久化 mode」驱动，非法组合 fail-closed
- Web 新建任务面板模式选择器与低可见度提醒；删除弹窗按任务 mode 出文案；工作台 Git tab 仅对 dir 项目任务隐藏（repo local-path 显示，D8）

**Non-Goals:**
- 不改变 worktree 模式任何现有行为
- 不新增项目类型；dir 项目对外语义不变（无 mode 选择器、拒绝 mode 参数）
- 不支持 local-path 任务的 base_ref / 分支能力（任务无分支概念；git 能力见 D8，作用于当前分支）
- 不收敛/迁移 dir 项目类型

## Decisions

### D1：migration `0013_task_mode.sql`（最新已占用至 0012）

`tasks.mode TEXT NOT NULL DEFAULT 'worktree'`；migration 内显式 UPDATE：dir 项目的既有任务 → `local-path`。repo 既有任务由 DEFAULT 覆盖为 `worktree`，与现状行为等价。测试必须覆盖：从 0012 升级与全新建库两条路径，并验证 repo→`worktree`、dir→`local-path` 回填结果。

理由：dir 任务持久化 mode 写 `local-path` 后，「任务有效运行模式」成为任务行自洽事实，生命周期分流只看 `task.Mode` + kind 合法性校验，不必每次按 kind 重推。备选（`branch==""` 隐式推断）否决：delete.go:111-113 明确禁止隐式信号分流。

### D2：有效模式解析器——穷尽矩阵 + 统一错误语义

新增 `resolveTaskMode(task TaskRow, projKind string)` 单点解析（task 包），kind-first 后 mode：

| proj.Kind | task.Mode | 结果 |
|---|---|---|
| repo | worktree | worktree 序列 |
| repo | local-path | local-path（=dir）序列（除 D8 git 能力外） |
| dir | local-path | local-path（=dir）序列 |
| dir | worktree | 持久化损坏 → internal，零副作用 |
| 未知 kind | 任意 | internal，零副作用 |
| repo/dir | 未知 mode | internal，零副作用 |

- 所有入口 MUST 在任何写/git/进程副作用前完成解析；`CreateShell`（attach_shell.go）同为入口——在任何 process 查询/创建前读取项目并解析（实现评审 I-F2 裁决）。
- 已提交意图的重入路径（deleteResume）：解析失败落 `deletion_failed + last_error`，MUST NOT 执行任何破坏性副作用（与 delete.go:140-144 未知 kind 现状一致）。
- 分流实现 MUST NOT 依赖 `branch` 判空等隐式信号。
- 删除失败落账统一经 `finalizeDeletionFailed`：非取消有界 ctx + `writeStatusConditional(deleting → deletion_failed)` 真实 CAS + 检查写错误与 Matched；落账失败返回 internal 并 `errors.Join` 保留原始错误与落账错误（实现评审 I-F1/C-F1 裁决）。

### D3：创建链路

`Manager.Create` 签名由 `(ctx, projectID, name, baseRef)` 改为接收 `CreateTaskOptions{Name, BaseRef, Mode}`（避免布尔/串参数位膨胀）。

- repo + worktree → `createRepo` 现状不变
- repo + local-path → 抽 `createDir` 主体为共用 `createInPlace`：base_ref 非空拒绝、无副作用目录预检、零文件副作用、creation_failed 语义全部继承；落库 `mode=local-path`、`branch=""`、`worktree_path=canonical 项目路径`、`base_ref=""`
- dir → API 层已拒 mode 参数（D6），内部落 `mode=local-path`
- retryCreate（crud.go:529）按 D2 解析分流：local-path → retryCreateDir 语义（仅校验目录存在）；worktree → 现状（空 base_ref fail-closed 保持，对 worktree 任务仍是合法不变量）

### D4：删除链路

删除入口（delete.go:61）、`deleteResume`（delete.go:135）与删除 Retry（crud.go:453-506，#14）分流从 `proj.Kind` 改为 D2 有效模式：local-path → `deleteResumeDir` 序列；worktree → repo 序列。repo 序列前置的 PreflightDelete/dirty 快照（delete.go:75-93）与 Retry 路径的 DirtyFiles + confirmDirty 门禁（crud.go:483-505）同样按有效模式跳过。硬不变量（内建逻辑不触碰用户目录、不动 git 状态）对 repo local-path 任务同等成立。

Retry（deletion_failed → deleting）重入改为专用原子意图写 `BeginRetryDeleteIntent`：单事务写 `delete_mode + status=deleting + last_error=NULL`（守卫 `status IS deletion_failed`），CAS 未命中在任何 dirty/进程/删除副作用前返回 conflict。首删 `BeginDeleteIntent` 逐字不动（不新增清 last_error 行为）（实现评审 C-F2/C-F3 裁决）。

### D5：会话对齐模式按任务模式解析

`alignModeForKind(kind)` → 按 D2 有效模式解析：local-path → `AlignModeOwnedOnly`（仅刷新 owned，绝不 claim）；worktree → `AlignModeRepo`。调用点 #4-8（activate/suspend/recovery×2/reconcile/attach_shell）全部改传任务有效模式。理由：共享目录下 AlignModeRepo 的 claim 语义会让多个 local-path 任务互抢 session 归属。

### D6：API 请求契约（presence 语义 + 校验顺序）

`createTaskReq` 增加 `Mode *string`（指针保留 presence：null=缺省，非 null=显式提供）。行为规范（决策表）的 normative owner 为 task-lifecycle delta spec「任务运行模式选择」要求，此处不复制。

校验顺序：① JSON decode → ② name 非空 → ③ mode 值域校验（显式提供时 trim 后为空或未知值 → invalid_input）→ ④ `requireProjectKind` fail-closed → ⑤ 组合校验（dir + mode 显式提供 → invalid_input）→ ⑥ 调 `Manager.Create`（base_ref 与模式的组合校验在 task 层 createInPlace/createRepo 入口，与现状 base_ref 校验位置一致）。

### D7：mode 传播链与分层责任表

删除弹窗（CommandCenterPage.tsx:446）按任务 mode 出文案，选择**全链路透传**方案（不采用「弹窗前再取详情」——增加一次往返且 summary 驱动的列表态删除入口同样需要 mode）；工作台 Git tab 显隐改按 `project_kind==='dir'` 判定（D8），不依赖 mode：

| 层 | 类型/位置 | 责任 |
|---|---|---|
| 持久化 | `tasks.mode` 列（0013） | 唯一事实源 |
| store | `internal/infrastructure/store/queries.go` TaskRow 读写 | 读写映射，扫描/插入不得丢列 |
| sqlite | `internal/infrastructure/sqlite/adapter.go:187` 附近 | store↔domain 映射 |
| application | `internal/application/dto.go:51`、`ports.go:79` | TaskSnapshot/DTO 携带 mode |
| task 包 | `internal/task/types.go` TaskRow + `adapters.go:489` | domain 映射；`TaskMode` 常量（`worktree`/`local-path`）定义于 task 包（与 ProjectKind 同处） |
| api | `taskRowDTO` + 项目列表 task summary（project-management spec 字段表） | 必有字段（非 omitempty），unknown 值 fail-closed 不输出 |
| SSE | task-detail-stream spec 字段穷举 | 同 taskRowDTO |
| web | `web/src/types.ts` Task/TaskSummary + CommandCenterPage 弹窗 + TaskWorkbenchPage Git tab | 弹窗按 `task.mode` 出文案；Git tab 显隐按 `project_kind==='dir'`（D8） |

活跃任务概览链（REST `/tasks/active` 与 SSE `/tasks/active/stream` 同构）同样透传 mode：`store.ActiveTaskOverviewRow`（queries.go:390-435）→ `application.ActiveTaskOverviewRow`（dto.go:83-94）→ task StoreAdapter（adapters.go:72-84）→ `activeSessionDTO`/`buildActiveSessionsSnapshot`（api/tasks.go:493-508、sessions_snapshot.go:17-35）→ web `ActiveSessionItem`（types.ts:114-126）。mode 为必有字段；非法 kind/mode 时 REST 返回 500；SSE 初始组装返回 500，update 组装按既有「保持 dirty 并重试」处理。

### D8：Git/diff 能力门禁（review 裁决：repo 项目 local-path 开放完整 git 能力）

`assertGitRepoTask`（gitops.go:29）改按 D2 有效模式：**repo 项目 local-path 模式任务放行全部 git 操作（status / diff / diff review / commit / push / diff 视图内文件编辑读写）**；dir 项目任务维持 `codeInvalidInput` 拒绝（文案不变：「纯目录项目非 git 仓库」）；非法 kind/mode 组合仍 internal fail-closed。diff review 经 DiffSourcePortAdapter.ReadLocked（diffreview_adapters.go:186 同一门禁），来源 `(ref, path, untracked)` 由 UI 传入；diff 视图内文件编辑读写经 diffreview_fileedit.go 同一门禁（GitPanel 经 /git/file 接线），读（ReadRaw）与编辑写回落点均为项目目录当前 checkout。

操作语义：全部作用于项目目录当前 checkout 的**当前分支**——GitStatus/GitDiff/GitCommit/GitPush 均基于 `row.WorktreePath`（local-path 任务即项目路径），无 base_ref 依赖。commit 提交到当前分支 HEAD；push 为 `git push -u origin <当前分支>` 且 MUST NOT force-push。diff 来源钉死为现有 GitPanel 三组（仅未提交部分，对比 HEAD/index）：`ref=HEAD`（已暂存，工作区 vs HEAD）、`ref=''`（未暂存，工作区 vs index）、`untracked`（未跟踪文件）；MUST NOT 展示当前分支相对 upstream 或 base 的已提交 commit 差异（分支变更视图留待后续迭代）。

风险归属（显式契约）：操作对象是用户主仓库当前分支，改动就地生效、提交与推送直接作用于用户分支；dirty 混杂与直接提交/推送主仓库分支的风险由用户自担（与 local 模式共享目录语义一致）。

Web：repo 项目 local-path 任务显示 Git tab 与 git 面板入口（TaskWorkbenchPage isGitless 收窄为 `project_kind==='dir'`）；工作台页头任务分支名保持隐藏（`task.branch` 恒为空，GitPanel 内有实时分支显示 status.branch）；指挥中心/项目页任务行分支显示维持 gitless 隐藏不变。env：local-path 仍不注入分支变量（D9 不变）；删除/pre-delete/对齐等其余模式分流不变。

### D9：layerEnvSnapshot 分支变量

activate.go:183 的 kind switch 改按 D2 有效模式：worktree → 注入 `OCDECK_TASK_BASE_BRANCH`/`OCDECK_TASK_HEAD_BRANCH`（现状不变）；local-path → 两键不存在（同 dir，不注入空串）。init/pre_delete 直接调用本函数，不经过 Activate 入口门禁，故此处 MUST 自检有效模式（保持既有自检原则）。

### D10：Web 新建任务面板

按 proposal「UI 设计说明与框图」节实现（segmented control 默认「worktree」、local-path 联动、灰字提醒逐字文案、dir 不渲染选择器）。`api.createTask` 增加可选 mode 参数；local-path 提交不携带 `base_ref` 且绕过分支 ready 门禁。

### D11：reconcile / recovery

reconcile 对 local-path 任务不做任何 git/产物验证（同 dir），creating → creation_failed 语义一致；目录存在性仅在创建前置与 Retry 校验。恢复路径 align 走 D5，锚定 claim 冲突语义不变。

## Risks / Trade-offs

- [分流点遗漏 → 对用户主仓库执行 git 副作用] → Context 表为全量审计结果（14 处）；tasks.md 将包含 `proj.Kind`/`alignModeForKind` 调用点复查 grep 验收项
- [migration 回填错误 → 存量任务行为漂移] → D1 双路径测试（0012 升级 + 全新建库）
- [共享目录 session 串扰] → D5 OwnedOnly 与 dir 已验证隔离模型一致（session_isolation_test.go 用例模式复用）
- [多任务并行 dirty 干扰] → 产品决策不限制并发 + 低可见度提醒，系统不加锁
- [回滚兼容] → 见下

## Migration Plan

1. migration 0013 加列 + 回填（存量任务语义不变）
2. 任务包分流点改造（D2-D5、D8、D9、D11）→ API（D6、D7）→ Web（D7、D10）
3. **回滚前提**：schema 层无需 down migration（旧代码不读 mode 列），但**行为回滚仅在没有 local-path 任务行时安全**——旧二进制会把 local-path 行按 repo worktree 任务处理（retryCreateRepo 空 base_ref fail-closed、layerEnvSnapshot 报错、删除走 repo 序列对用户目录执行 git 检查）。若已存在 local-path 任务，须先删除/处理这些任务再回滚。禁止把「无需 down migration」等同于「任意时点可安全回滚」。
