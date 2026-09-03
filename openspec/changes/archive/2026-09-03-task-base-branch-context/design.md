# Design: task-base-branch-context

## Context

创建 repo 任务时，指挥中心内联面板用 `GET /api/v1/projects/{id}/branches` 的短名填充基准分支 combobox。API 返回「本地在前、远端在后」稳定序（`internal/infrastructure/git/branches.go:10-47`）。前端 `filteredBranches`（`web/src/pages/CommandCenterPage.tsx:874-877`）只做大小写不敏感子串过滤、不重排；表单 `onSubmit`（`:881-899`）把输入框当前值原样作为 `base_ref` POST。选中 repo 项目时输入框预填 `default_branch`（`:821`），因此用户不改输入直接提交时，即使列表里有 `origin/main` 也会提交 `main`。后端 `ResolveBaseRef`（`internal/infrastructure/git/base_ref.go:21-47`）heads 优先，本变更不改。

任务激活时 `layerEnvSnapshot`（`internal/task/activate.go:89-164`）注入 `OCDECK_TASK_ID/NAME/PATH` 与 `OCDECK_PROJECT_PATH`，经 `mergeEnvSnapshot` 持久化到 `tasks.env_snapshot`。opencode serve 用该 map 经 tmux `-e` 启动（`activate.go:1034-1041`）；页面 shell 经 `loadEnvSnapshot` 同一通道启动（`attach_shell.go:130-135`）。`OCDECK_*` 前缀已保留（`isReservedEnvKey`，`activate.go:52-59`）。落库 `tasks.base_ref` 为全限定 ref（`refs/heads/<name>` 或 `refs/remotes/<name>`）；`tasks.branch` 为 `ocdeck/<slug>`。dir 任务 `base_ref` 与 `branch` 皆空。

```
创建任务（repo）
  用户输入/预填 q ──► filter(includes) ──► [本变更] rank ──► 提交首项作 base_ref
                                              │
                                              ▼
                                    POST .../tasks { base_ref }
                                              │
                                              ▼
                                    resolveRepoBaseRef（不变，heads 优先）
                                              │
                                              ▼
                                    落库 tasks.base_ref / tasks.branch

激活（新激活代）
  layerEnvSnapshot ──► [本变更] 注入 BASE/HEAD 短名 ──► persist env_snapshot
                                              │
                    ┌─────────────────────────┴─────────────────────────┐
                    ▼                                                   ▼
           opencode serve (tmux -e)                            CreateShell (tmux -e)
                    │                                                   │
                    ▼                                                   ▼
           agent bash 继承                                      页面 shell 继承

自动重拉（同一激活代，D8）
  runRecoveryIncident 每轮：
    loadEnvSnapshot（permit 之前）
      ├─ 坏快照 → recoveryTerminalError → 终态补偿（AcquireRecoveryPermit=0）
      └─ 有效 map → runRecoveryAttempt（permit-first）
           仅更新 OCDECK_SERVE_PORT ──► persistEnvSnapshot ──► NewSession
  MUST NOT layerEnvSnapshot / mergeEnvSnapshot
```

## Goals / Non-Goals

**Goals:**

- 指挥中心新建任务面板：过滤后远端同名分支排在对应本地分支之前；任一提交路径用过滤排序后的第一项作为 `base_ref`。
- repo 任务激活时注入 `OCDECK_TASK_BASE_BRANCH`（基线短名）与 `OCDECK_TASK_HEAD_BRANCH`（`tasks.branch`），进入 env 快照，serve 与 shell 两条链路均可读。
- dir 任务不注入这两个键。

**Non-Goals:**

- 不改项目创建流程、不改 `GET /branches` 返回顺序、不改 `ResolveBaseRef` heads 优先。
- 不接 opencode session metadata。
- 不改 `project-lifecycle-config` spec（其「MUST 含」四变量是下限；init/pre_delete 经同一 `layerEnvSnapshot`，repo 任务脚本会自然看到新变量，dir 任务不会）。

## Decisions

### D1 排序与提交只改前端，后端列表与解析不变

- **选择**：在 `CommandCenterPage` 的 `filteredBranches` 上重排，并让 `submit` 读排序后第一项。`ListBranches` 仍本地在前；`ResolveBaseRef` 仍 heads 优先。
- **理由**：用户要的是「输入 `master` 时下拉里 `origin/master` 在前、提交用该项」。`origin/` 只是 UI 启发式标记（与 `git branch -r` 短名惯例一致），不是「真实 remote」判定。若仓库存在本地 `refs/heads/origin/main`，`ListBranches` 会因同短名去重只保留本地条目，且 `ResolveBaseRef` 对提交短名 `origin/main` 仍 heads 优先解析为 `refs/heads/origin/main`（task-lifecycle 既有场景）。本变更不保证选择真实 remote。无碰撞时提交 `origin/main` 才会解析为 `refs/remotes/origin/main`。改 API 顺序会牵动所有消费方；改解析优先级会改变未走 UI 的 API 客户端语义。
- **备选**：后端 `ListBranches` 改为远端在前 — 拒绝，破坏 task-lifecycle 既有「本地在前」契约。

### D2 过滤后排序元组（owner：command-center spec「基准分支下拉过滤与排序」）

对过滤结果按以下元组升序、稳定排序（值小优先）：

1. 同名命中：候选的本地名（去掉大小写不敏感前缀 `origin/` 后的部分；无该前缀则整名）等于 `q` → 0，否则 1。`normalizedInput = 输入框当前值.trim()`；`q = normalizedInput.toLowerCase()`。
2. 是否远端：短名大小写不敏感以 `origin/` 开头 → 0，否则 1。
3. 过滤前原顺序下标。

`origin/` 仅作 UI 远端标记（与 `git branch -r` 短名惯例一致）。其它 remote 名（`upstream/main`）不享受第 2 键加分，仍可被第 1 键命中（本地名 `upstream/main` 仅在 `q` 等于该整名时为同名命中）。

输入 `master` 且列表含 `master` 与 `origin/master`：两者第 1 键均为 0，第 2 键远端为 0 → `origin/master` 第一。

实现：从 `NewTaskPanel` 抽出纯函数（同文件或 `web/src/` 下小模块），供单测覆盖排序，不依赖 React。

### D3 任一提交路径使用过滤列表第一项（owner：command-center spec「提交时的 `base_ref`」）

- **选择**：创建按钮与表单内 Enter（含任务名框 Enter）共用 `submit`。统一原语 `normalizedInput = 输入框当前值.trim()`。repo 项目另须分支列表状态为 `ready`（D9）才可过门禁。`submit` 在现有门禁通过后，取当前 `filteredBranches[0]` 作为 `base_ref`（唯一总规则：`base_ref = filteredBranches[0]`）。`normalizedInput` 非空且不在基础候选中（大小写敏感 `Array.prototype.includes`）时，将 `normalizedInput` 作为 synthetic candidate 前置；synthetic 只参与 D2 排序，不保证第一，仅当它实际排第一时请求才提交 `normalizedInput`。过滤列表为空仅当状态 `ready` 且 branches、`default_branch`、`normalizedInput` 均为空——malformed 项目 DTO 防御路径：省略 `base_ref`，服务端沿既有契约返回 `invalid_input`，页面展示创建失败。dir 仍不传 `base_ref`。
- **理由**：用户确认「一律用过滤首项」，含预填 `main` 后直接点创建。现有 `<form onSubmit={submit}>` 已让任意字段 Enter 走同一函数，不必给分支 input 单独拦 Enter。下拉高亮与 `filteredBranches[0]` 对齐（ready 时高亮提交首项；loading/error 时为 stale 展示列表首项），不按输入框精确等值高亮，保证「所见即所提交」。
- **备选**：仅分支框 Enter 用首项、按钮用输入框值 — 拒绝，用户已否决。点击下拉某项只写入输入框；若该项不是当前过滤序第一，随后提交仍用第一项（见 Risks）。

### D4 在 `layerEnvSnapshot` 注入，不新开通道（owner：env-management spec「生命周期变量注入」）

- **选择**：在 `layerEnvSnapshot` 现有四变量赋值处（`activate.go:160-163`）按项目 kind 三分支：`repo` 注入两键（先校验 D5）；`dir` 强制不注入两键（即使脏数据有 `base_ref`/`branch`）；其它 kind 返回 internal error，不得按 dir 静默缺键。`mergeEnvSnapshot` 持久化快照；serve 与 `CreateShell` 无需改签名。Activate 入口已有 `alignModeForKind` 未知 kind 门禁（`activate.go:253-263`），`layerEnvSnapshot` 仍 MUST 自检 kind，因为 init 与（仅当实际执行时的）pre_delete 直接调用它、不经过该门禁。
- **理由**：agent bash 与页面 shell 都已吃这份 map。`OCDECK_*` 前缀已保留，用户 env 覆盖不了新键。不必把新键写入 `envReservedKeys` 字面量（前缀规则已覆盖）。生命周期变量计数：`layerEnvSnapshot` 在 repo 为 6 个（原 4 个 + `OCDECK_TASK_BASE_BRANCH` + `OCDECK_TASK_HEAD_BRANCH`），dir 仍为 4 个；`mergeEnvSnapshot` 在 repo 为 7 个、dir 仍为 5 个（`OCDECK_SERVE_PORT` 只由 merge 调用方加入，`activate.go:72-77`）。未知 kind fail-closed 与 `types.go:167-184` 既有约定一致。
- **备选**：task 级用户 env API 写入 — 拒绝，可被用户改、非生命周期。opencode metadata — 非目标。未知 kind 当 dir — 拒绝，会掩盖 DB 损坏。

### D5 短名由落库全限定 ref 去前缀得到（owner：env-management spec 短名规则）

| 落库 `tasks.base_ref` | `OCDECK_TASK_BASE_BRANCH` |
| --- | --- |
| `refs/heads/<name>`（`<name>` 非空） | `<name>`（如 `main`） |
| `refs/remotes/<name>`（`<name>` 非空） | `<name>`（如 `origin/main`） |
| 其它前缀、空、或前缀后为空 | `layerEnvSnapshot` 对 repo 返回 internal error |

`OCDECK_TASK_HEAD_BRANCH` = `row.Branch`（`ocdeck/<slug>`）。转换是纯字符串前缀剥除，不调 git。dir：kind=dir 时两键都不写（即使脏数据有值）。repo 且 `row.Branch` 为空、或 `base_ref` 不匹配上表两行：`layerEnvSnapshot` 返回 internal error——MUST NOT 持久化新快照、MUST NOT 创建进程。init 沿既有「layer env snapshot 失败」路径落账（`init_status=failed`），且 MUST NOT 触发后续副作用。pre-delete 仅在既有执行路径上调用 `layerEnvSnapshot`：`DeleteNormal`、worktree 目录存在、配置了非空 `pre_delete_script`（`delete.go:253-260`、`init_run.go:184-192`）。Force、目录不存在、空脚本均沿既有契约跳过，MUST NOT 为校验新变量而改变这些跳过路径。当该路径确实调用 `layerEnvSnapshot` 且失败时，沿既有「pre-delete:」前缀落 `deletion_failed`，MUST NOT `wt.Remove`。

### D8 Recovery 复用同代快照，不重分层（owner：env-management spec「修改后生效时机」）

- **选择**：`loadEnvSnapshot` 只校验并返回普通 error（拒绝 `vars == nil`）。`runRecoveryIncident`（`recovery.go:507-520` 循环）每轮在调用 `runRecoveryAttempt` 之前加载快照；失败时构造 `&recoveryTerminalError{err: newOpErr(codeInternal, err)}` 并走既有终态分派（`recovery.go:542-548`）。有效 map 再传入 permit-first 的 `runRecoveryAttempt`：该函数仍以 `AcquireRecoveryPermit` 为首动作（`recovery.go:686-694`，canonical 预算协议），内部用传入 map 仅覆盖 `OCDECK_SERVE_PORT` 后 `persistEnvSnapshot`，MUST NOT 调用 `layerEnvSnapshot` / `mergeEnvSnapshot`。坏快照路径 MUST NOT 调用 `persistEnvSnapshot`、MUST NOT 写入更新后的 env map、MUST NOT `NewSession`、MUST NOT `AcquireRecoveryPermit`、MUST NOT backoff；既有终态补偿事务（`status`/`last_error`/`env_snapshot=NULL`）仍 MUST 执行。端口轮换路径（`activate.go:1024-1027`）与 persist 重启 `resumeActive`（`reconcile.go:355-360`）不改。
- **理由**：自动重拉属于同一激活代。现实现走 `mergeEnvSnapshot` 会重算用户 env 并补写新键。坏快照不可自愈；若在 `runRecoveryAttempt` 内 permit 之后才加载，会消耗恢复预算并违反「立即终态」。pre-attempt 校验兑现零预算消耗，且不破坏「permit 是 attempt 首动作」。
- **备选**：让 Recovery 重分层以尽快补键 — 拒绝。坏快照在 attempt 内加载 — 拒绝，会先耗 permit。`loadEnvSnapshot` 直接返回 `recoveryTerminalError` — 拒绝，该类型属于 Recovery 分派，共享加载函数不得耦合。

### D9 repo 分支列表 `idle|loading|ready|error`（owner：command-center spec「基准分支列表状态」）

- **选择**：`NewTaskPanel` 为 repo 项目维护显式状态 `idle|loading|ready|error`，与 `lastSuccessfulBranches` 正交。选中 repo → `loading` + 初次 GET；成功（含空数组）→ `ready` 并写入 `lastSuccessfulBranches`；失败 → `error`，无历史则列表为空。刷新远端进入 `loading` 时 MUST NOT 清空 `lastSuccessfulBranches`；成功覆盖之并回 `ready`；失败进入 `error`，保留最近一次 ready 数据作 stale 展示，标注「本地快照未刷新」及重试入口。初次 GET 与 refresh POST 成功均覆盖 `lastSuccessfulBranches`。`ready` 时基础候选来自该字段。`canSubmit` 对 repo 另须 `ready`。stale 列表 MUST NOT 用于 `filteredBranches[0]` 或提交。`loading`/`error` 不得把空 `branches` 回退为 `default_branch`。dir 无此状态机。
- **理由**：现实现初次 GET 在途时 `branches=[]` 会回退 `default_branch`，用户可在发现 `origin/main` 前提交 `main`，绕过「远端优先且提交首项」。canonical task-lifecycle「refresh 失败不伪装最新」要求保留旧列表并标注「本地快照未刷新」；本变更把 stale 展示与提交解耦，避免用未刷新快照当 `base_ref`。
- **备选**：允许在途提交预填值 — 拒绝，与用户确认的「一律用过滤首项」冲突。refresh 失败清空列表 — 拒绝，违反 canonical 展示契约。refresh 失败仍允许用 stale 提交 — 拒绝，与「仅 ready 提交过滤首项」冲突。

### D6 实现落点与分层

| 落点 | 职责 |
| --- | --- |
| `web/src/pages/CommandCenterPage.tsx` 分支列表状态 | D9：`idle\|loading\|ready\|error`；`canSubmit` 对 repo 另须 `ready` |
| 同文件 `filteredBranches` | 仅 `ready` 时按 D2 排序 |
| 同文件抽出的纯函数（建议 `rankBranchOptions(options, query)`） | D2 元组排序，单测入口 |
| 同文件 `submit` | D3：repo 用 `filteredBranches[0]`；`ready` 且空列表时省略 `base_ref` |
| `internal/task/activate.go` `layerEnvSnapshot` | D4/D5 注入 |
| 同包小函数（建议 `baseBranchShortName(fullRef string) (string, bool)`） | D5 前缀剥除 |
| `internal/task/recovery.go` `runRecoveryIncident` | D8：每轮 permit 之前 `loadEnvSnapshot`；失败包装 `recoveryTerminalError` |
| `internal/task/recovery.go` `runRecoveryAttempt` | D8：接收已校验 map；permit-first；仅更新 `OCDECK_SERVE_PORT`；MUST NOT `layerEnvSnapshot`/`mergeEnvSnapshot` |
| `internal/task/activate.go` `loadEnvSnapshot` | D8：拒绝 `vars == nil`；缺失/非法 JSON/`vars` null 返回普通 error |

不改 API、store schema、process/tmux、opencode client。无 Fx、无持久化迁移。

不需要先重构再加行为：排序是 `useMemo` 增量；注入是赋值增量。

### D7 任务拆分与验收矩阵（两段、无并行写冲突）

1. 后端注入 + Go 测试。
2. 前端排序 + 提交 + 既有 `command-center-new-task` 测试补场景。

两段可顺序由同一实现者完成；无跨模块接口变更，不需多 lane。无需新增 mock 接口。

**Go 验收**：所有会到达 `layerEnvSnapshot` 的正常 repo fixture MUST 默认带 `Branch` 与 `BaseRef=refs/heads/main`，至少更新 `seedSuspendedTask` 与 `seedActiveTask`（`manager_test.go:17-30` 现缺该字段），并审计手写 `TaskRow`（如 `lifecycle_phase3_test.go:409` 的 pre-delete 失败用例现缺这两字段）。异常路径测试再显式覆盖为空或非法前缀。表驱动覆盖：
- heads → `OCDECK_TASK_BASE_BRANCH=main`；remotes → `origin/main`；
- dir 脏值（有 `base_ref`/`branch`）仍缺两键；
- repo 异常 ref / 空 branch / 未知 kind → `layerEnvSnapshot` error，不持久化；
- 快照含两键；用现有 mock process 断言 serve 与 `CreateShell` 环境含相同值。
编排覆盖：
- Activate 异常行回到 suspended、无 `NewSession`、无新快照；
- init 落 `init_status=failed` 且不运行脚本/不激活；
- pre-delete（仅 `DeleteNormal` + 目录存在 + 非空脚本）落 `deletion_failed` 且不运行脚本、不删除 worktree/任务行；既有脚本失败测试（如 `TestPreDelete_ScriptFails_NoWorktreeRemove`）MUST 补齐 `Branch`/`BaseRef`，并断言 `RunScript` 调用次数仍为 1（失败原因仍是脚本错误，不得因缺 `BaseRef` 提前在 `layerEnvSnapshot` 失败）；Force / 目录不存在 / 空脚本跳过路径 MUST NOT 因本变更而调用 `layerEnvSnapshot`；
- Recovery 同代快照：旧快照无新键则重拉后仍无；已有新键则保持原值，仅 `OCDECK_SERVE_PORT` 可更新；MUST NOT 调用 `layerEnvSnapshot`；
- Recovery 坏快照（缺失、非法 JSON、`{"vars":null}`）→ `runRecoveryIncident` 在 permit 之前包装 `recoveryTerminalError`；`AcquireRecoveryPermit` 调用次数为 0、无 backoff、无 `persistEnvSnapshot`、无更新后的 env map 写入、无 `NewSession`、无 panic；既有终态补偿事务仍执行。

**前端验收**：大小写不敏感同名命中；稳定 tie-break（原顺序下标）；`upstream/*` 不加第 2 键；预填 `main` 且存在 `origin/main` 时点创建提交 `origin/main`；任务名框 Enter 同路径；基础候选 `["main","develop"]` 且输入 `  feature-x  ` 时提交 `feature-x`；基础候选 `["origin/main"]` 且 `normalizedInput=main` 时提交 `origin/main`（synthetic 不保证第一）；dir 省略 `base_ref`；初次加载在途不发 POST；加载完成后提交 `origin/main`；初次加载失败列表为空且不提交；刷新失败保留旧列表、标注「本地快照未刷新」、禁止提交；刷新成功恢复提交。

## Risks / Trade-offs

- [Risk] 用户点击下拉里的本地 `main` 后再点创建，提交仍可能是 `origin/main`（D3）。→ 这是用户选定的「一律用首项」语义；design 接受。若后续要「点击即钉死」，再加 pinned 状态。
- [Risk] 预填 `default_branch=main` 且存在 `origin/main` 时，一键创建从切本地 `main` 变为切远端 `origin/main`。→ 符合本变更目标；后端仍按提交短名解析。
- [Risk] 存量 repo 任务再次激活才写入新变量；已 active 的任务要挂起再激活。自动重拉属于同一激活代，不补写新键（D8）。→ 与既有 env「修改后生效时机」一致，不另做热更新。
- [Risk] repo 异常 `base_ref`/`branch` 使激活/init/pre_delete 失败。→ 与 task-lifecycle「repo 落库值为空 MUST fail-closed」一致；正常 Create 只写两种前缀且 branch 非空。
- [Risk] 排序把 `origin/` 写死，非 origin remote 不加分。→ 与用户示例一致；D2 已说明。
- [Risk] 本地分支短名恰好为 `origin/main`（`refs/heads/origin/main`）时，UI 会把它当远端加分，后端仍 heads 优先解析为本地。→ 已知边界，本变更不修 ListBranches 去重与解析；见 D1。

## Migration Plan

- 无 schema / API 迁移。部署后新创建与下一次激活生效；已 active 任务经自动重拉不补写新键，须挂起再激活。
- 回滚：撤前端排序/提交/状态机改动即恢复「提交输入框值」与在途可提交；撤注入与 D8 则新激活不再带两键，Recovery 恢复为 `mergeEnvSnapshot`；旧快照仍含已写入的键直到下次挂起清除。

## Open Questions

无。探索阶段已关闭：Key 名、短名形态、Enter=提交首项、两条消费链路、dir 不注入、预填后点创建也用首项。
