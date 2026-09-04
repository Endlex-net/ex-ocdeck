# Tasks: add-local-path-task-mode

## 1. 持久化与模型层

- [x] 1.1 新增 migration `internal/infrastructure/store/migrations/0013_task_mode.sql`：`tasks.mode TEXT NOT NULL DEFAULT 'worktree'`，migration 内 UPDATE dir 项目的既有任务为 `local-path`（design D1）
- [x] 1.2 migration 双路径测试：从 0012 升级 + 全新建库，验证 repo→`worktree`、dir→`local-path` 回填结果（design D1）
- [x] 1.3 task 包定义 `TaskMode` 常量（`worktree`/`local-path`，与 ProjectKind 同处 types.go）；`TaskRow` 及 store/sqlite/application 各层读写映射携带 mode，逐层不得丢字段（design D7 责任表：queries.go、sqlite/adapter.go、application/dto.go、ports.go、task/adapters.go）

## 2. 有效模式解析器与任务包分流点

- [x] 2.1 实现 `resolveTaskMode(task, projKind)` 穷尽矩阵（仅 (repo,worktree)/(repo,local-path)/(dir,local-path) 合法；dir+worktree、未知 kind/mode → internal；kind-first 顺序），单点收口。解析器为纯函数；**入口错误处置分层**：未提交意图的入口（Create/Activate/Suspend/首次 Delete/Retry）在任何状态写入与副作用前拒绝、零副作用；已提交意图的重入路径（deleteResume）仅允许写 `deletion_failed + last_error`，随后 MUST NOT 执行 git/进程/session/目录/删记录副作用（design D2）
- [x] 2.2 创建链路：`Manager.Create` 改收 `CreateTaskOptions{Name, BaseRef, Mode}`；抽 `createDir` 主体为共用 `createInPlace`（repo local-path 与 dir 共用），完整继承 dir 创建语义：非空 base_ref 拒绝、无副作用目录预检（EvalSymlinks+IsDir，失败 invalid_state 不落 creating 行）、跳过分支命名（LLM slug/机械 slugify）/分支校验冲突检查/worktree 路径生成与碰撞重试/worktree add/inherit/全部 git 与文件副作用、`creation_failed` 仅可能来自 lifecycle 配置读取失败或提交点失败。**三种落库写入必须闭合**：repo 缺省或显式 worktree → `mode='worktree'`；repo local-path → `mode='local-path'`、`branch=""`、`worktree_path=canonical 项目路径`、`base_ref=""`；dir 缺省 → `mode='local-path'`（MUST NOT 落到 DB 默认值 `worktree`，否则立即成为非法组合）（design D3）
- [x] 2.3 创建重试：`retryCreate`（crud.go:529）按有效模式分流，local-path → retryCreateDir 语义（design D3）
- [x] 2.4 删除链路：删除入口（delete.go:61）、`deleteResume`（delete.go:135）、删除 Retry（crud.go:453-506）按有效模式分流；repo 序列前置 PreflightDelete/dirty 快照与 Retry 的 DirtyFiles+confirmDirty 门禁按有效模式跳过。错误处置按 2.1 分层：首次 Delete/Retry 在状态写前拒绝；deleteResume 解析失败仅落 `deletion_failed + last_error`（design D4）
- [x] 2.5 会话对齐：`alignModeForKind` 改为按有效模式解析（local-path→OwnedOnly）。调用点全覆盖：Activate（activate.go:299）、Suspend（suspend.go:40）、恢复双入口（recovery.go:295/400）、reconcile（reconcile.go:313）、attach_shell（attach_shell.go:62）；四个运行时入口（Activate、persist 重启恢复 resumeActive、挂起修复 tryRepairRuntime、自动重拉恢复 ensureRecovery）在任何状态修改或运行时副作用前完成解析（opencode-orchestration delta「session 归属捕获」）（design D5）
- [x] 2.6 生命周期变量：`layerEnvSnapshot`（activate.go:183）按有效模式注入分支变量——worktree 注入 BASE/HEAD，local-path 两键不存在；非法 kind/mode 组合 → internal error（不持久化快照、不建进程）（design D9）
- [x] 2.7 git 门禁：`assertGitRepoTask`（gitops.go:26）按有效模式拒绝 local-path：invalid_input、在任何 git 命令/文件读取/子仓库探测前拒绝；原因文案按模式区分（语义契约见 git-operations delta「纯目录项目任务的 git 操作降级」，非逐字冻结）。覆盖 status/diff/commit/push 与 diff review（diffreview_adapters.go:186）（design D8）
- [x] 2.8 reconcile/recovery：reconcile 对 local-path 任务跳过 git/产物验证并保持 creating→creation_failed 收敛语义（同 dir）；recovery 仅按 D5 分流对齐模式，锚定 claim 冲突语义不变（design D11）
- [x] 2.9 **分流点复查验收（G2）**：grep `proj.Kind` 与 `alignModeForKind` 全部调用点，对照 design Context 表 14 处分流点逐一确认已按有效模式驱动，无遗漏

## 3. API 与 DTO 传播

- [x] 3.1 `createTaskReq` 增加 `Mode *string`（presence 语义）；按 task-lifecycle delta 决策表实现校验顺序 ①-⑥（值域 → kind fail-closed → dir+mode 组合拒绝）（design D6）
- [x] 3.2 `taskRowDTO` 增加 `mode` 必有字段（非 omitempty，非法值 fail-closed 不输出）；`toTaskDTO` 从任务行取值（design D7）
- [x] 3.3 项目列表任务摘要（project-management spec 字段表）与任务详情 SSE（task-detail-stream spec 字段穷举）增加 `mode` 必有字段；摘要组装遇非法 kind/mode 同样 fail-closed（500 标准错误信封，不输出缺 mode 元素）
- [x] 3.4 活跃任务概览链透传 mode：`store.ActiveTaskOverviewRow`（queries.go:390-435）→ `application.ActiveTaskOverviewRow`（dto.go:83-94）→ StoreAdapter（adapters.go:72-84）→ `activeSessionDTO`/`buildActiveSessionsSnapshot`（api/tasks.go:493-508、sessions_snapshot.go:17-35）；REST 非法 kind/mode 返回 500；SSE 初始组装 500、update 保持 dirty 重试、MUST NOT 推送缺 mode 帧（design D7）

## 4. Web

- [ ] 4.1 `web/src/types.ts` Task/TaskSummary/ActiveSessionItem 增加 `mode`；`api.createTask` 增加可选 mode 参数（design D7/D10）
- [ ] 4.2 新建任务面板（CommandCenterPage.tsx 内联面板）：双段 segmented control「隔离 worktree / 就地运行」（仅 repo 渲染、缺省 worktree）；local-path 联动（隐藏基准分支字段、绕过 ready 门禁、灰字提醒逐字文案「直接在项目目录里跑，改动就地生效。多任务共享同一目录，并行与否自己把握。」、底部 hint 换文案）；选择器状态重置规则（项目 ID 变更重置为 worktree；同项目手动切换保留分支状态；不改变项目的信号保持现状；repo 初次分支请求始终发起）（command-center delta「指挥中心内联新建任务」含「选择器状态重置规则」）
- [ ] 4.3 工作台 Git tab 与分支展示对 local-path 任务隐藏（TaskWorkbenchPage.tsx:228，同 dir 降级）；删除确认弹窗按任务 mode 出文案——dir：「仅删除任务记录与 opencode 会话数据，不会删除项目目录及其内容」；repo local-path：「仅删除任务记录与 opencode 会话数据，不会删除项目目录及其内容、不改动 git 状态」；两者共同：normal 且配置 pre_delete script 时提示该脚本仍会执行、不出现 worktree/dirty 删除确认项（task-lifecycle delta 弹窗场景）

## 5. 测试与验收

- [ ] 5.1 创建：三类新建行的持久化断言（repo 缺省/显式 worktree→`worktree`；repo local-path→`local-path`+空 branch/base_ref+项目路径；dir 缺省→`local-path`）；拒绝非空 base_ref、目录消失 invalid_state 零副作用、dir+mode 拒绝、未知 mode 拒绝、presence 决策表全组合；**createInPlace 零副作用验收**：spy/panic backend + 目录快照断言 slug 生成、分支校验与探测、worktree 路径生成、worktree add、git 调用、项目目录内文件创建均未发生；local-path 不执行 inherit 但 lifecycle 配置读取失败仍阻断创建链；配置 init 时 local-path 任务 init 以 canonical 项目路径为 cwd 执行，成功自动激活、失败保持 suspended 且 init_status=failed（project-lifecycle-config delta）；**创建重试验收**：local-path 任务 creation_failed → Retry 成功时复用 canonical 项目路径并跳过 git/worktree/inherit；Retry 时目录不存在保持 creation_failed 且零副作用；非法 kind/mode 组合在状态修改与任何副作用前拒绝
- [ ] 5.2 删除：repo local-path 走 dir 序列（normal/force/retry），内建逻辑不触碰项目目录与 git 状态；pre_delete normal 执行（cwd=项目目录）/retry 重执行/force 跳过；非法 kind/mode 组合：首次 Delete/Retry 状态写前拒绝零副作用、deleteResume 仅落 deletion_failed+last_error 无后续副作用
- [ ] 5.3 对齐与运行时入口：同 repo 两个 local-path 任务 OwnedOnly 互不认领（复用 session_isolation_test.go 模式）；四个运行时入口（Activate/resumeActive/tryRepairRuntime/ensureRecovery）+ Suspend/reconcile/attach_shell 逐一验收按有效模式解析；非法组合（dir+worktree、未知 kind、未知 mode）断言状态/runtime/SSE/align/anchor 均未变化
- [ ] 5.4 env：local-path 任务激活不注入分支变量（init/pre_delete 直接调用 layerEnvSnapshot 路径同规）；非法 kind/mode 组合 internal error（不持久化快照、不建进程）
- [ ] 5.5 git 门禁：local-path 任务 status/diff/commit/push 与 diff review → invalid_input，且 git runner 调用数、文件读取、子仓库探测均为零
- [ ] 5.6 DTO/API 传播：任务详情 REST 与 SSE、项目列表与详情摘要、活跃 REST/SSE snapshot/update 帧——mode 必有且与持久化同源；损坏数据分派全覆盖（任务 DTO/摘要 fail-closed；活跃 REST 500；SSE 初始 500、update 保持 dirty 重试）
- [ ] 5.7 Web：面板三态（默认 worktree/local-path/dir）与选择器重置规则；弹窗文案按 dir/local-path 区分与 pre-delete 提示；Git tab 降级
- [ ] 5.8 行为测试有效性证据：每个新增/修改的行为测试在旧实现下失败、新实现下通过（mutation 式验证或基线运行）
- [ ] 5.9 `openspec validate add-local-path-task-mode --strict` 通过；`go build ./...` 与相关包测试通过；web 测试通过
