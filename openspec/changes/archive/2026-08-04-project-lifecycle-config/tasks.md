# Tasks: project-lifecycle-config

## Phase 1: 存储层

- [x] 1.1 新增 migration `internal/store/migrations/0007_project_lifecycle_config.sql`：`project_lifecycle_configs` 表（project_id PK + 三 TEXT 字段 + updated_at，REFERENCES projects(id) ON DELETE CASCADE）；`tasks` 增 `init_status TEXT NOT NULL DEFAULT 'none'`、`init_error TEXT`（nullable）。DDL 不用 IF NOT EXISTS（项目迁移风格）
- [x] 1.2 `internal/store/queries.go`：`LifecycleConfigRow` + `GetLifecycleConfig`（缺行返回空配置，非错误）/ `UpsertLifecycleConfig`；`TaskRow` 增 `InitStatus`/`InitError` 并更新相关 scan/insert
- [x] 1.3 CAS/原子任务方法（design.md §2.1）：`CommitCreated(taskID, expectedStatus, initStatus)`（`:expected→suspended` 与 init_status 单条原子 UPDATE；Create 传 `creating`、retryCreate 传 `creation_failed`）、`ClaimInitRun`（WHERE status='suspended' AND init_status='pending'）、`ClaimInitRerun`（WHERE status='suspended' AND init_status IN ('failed','succeeded')）、`FinishInitRun`（WHERE init_status='running'；成功时清空 init_error）、`ConvergeInterruptedInitRuns`（pending|running→failed 批量）
- [x] 1.4 真实 SQLite migration 测试（沿用 TestP4Rereview_RealStore 模式）：0007 应用后表/列存在、CRUD 全链路、项目删除 CASCADE 生效、存量任务 init_status=none、各 CAS 方法条件不满足时 rows=0、`CommitCreated` 分别覆盖 `creating` 与 `creation_failed` 两种前置状态

## Phase 2: 机制层

- [x] 2.1 `internal/git` 新增 `ListIgnoredUntracked(ctx, repoPath)`：`git status --porcelain=v2 -z --ignored=traditional --untracked-files=all`（**必须 traditional——matching 对 ignored 目录只返回目录级记录，已实测**）；**扩展 parser：`parsePorcelainEntry` 当前丢弃 `!` 记录（parser_status.go:116）且 `FileStatus` 无 ignored 标记，需解析 `!` 并增加 `FileStatus.Ignored bool`**；复用 boundedBuffer + 参数白名单；真实 repo 测试（**ignored 目录内嵌套文件展开**、`-uall` 展开、有界输出超限）
- [x] 2.2 引入 `github.com/bmatcuk/doublestar/v4` 依赖（PUT 校验与执行共用）
- [x] 2.3 新包 `internal/lifecycle`：`RunScript(ctx, dir, env, script, logPath, timeout)`——`/bin/sh -c` 非交互、cwd=dir、stdout/stderr 写入 logPath（每次 truncate 重写，RunScript 为唯一写入者）、捕获上限 1MB 超限截断加标记、超时杀整个进程组
- [x] 2.4 `CopyInherited(ctx, repoPath, wtPath, entries, patterns) (warnings []string)`——**只返回警告不返回阻断 error（匹配/复制机制失败降级为警告；枚举在 internal/git、inherit.log 由 task 层写入，均不在本函数职责内）**；glob 过滤、排除 `.git`、保持相对路径/权限、符号链接按链接复制、**普通文件 no-clobber 原子发布（同目录临时文件完整写入 fsync+chmod 后 link(2) 到目标——EEXIST 即跳过+警告，再 unlink 临时文件；禁止 rename 覆盖并发目标）**、**destination 任一祖先为符号链接时拒绝**、路径 containment 校验（拒绝绝对路径/`..`）
- [x] 2.5 env 层叠抽取重构：从 `mergeEnvSnapshot`（activate.go:68-150）抽出不含 port 与持久化的层叠函数供脚本执行复用。**行为不变量**：serve/tui/shell 的 env 内容与顺序完全不变；既有测试全绿，矩阵补 TERM/locale 兜底/follow_host/manual/reserved key/快照持久化
- [x] 2.6 单测：脚本成功/非零/超时杀进程组/日志 truncate 重写/1MB 截断；inherit 的 `**` 嵌套、目录递归、符号链接（含 broken）、**no-clobber（并发出现的目标不被覆盖）**、**ancestor symlink 拒绝**、目标已存在跳过、containment 拒绝、`.git` 排除、tracked 文件不匹配、日志文件 0600/目录 0700

## Phase 3: task 编排

- [x] 3.1 接线：`TaskStore` 接口 + `StoreAdapter`（adapters.go）增 lifecycle 方法；**`internal/task.TaskRow` 增 `InitStatus`/`InitError` 并更新 `StoreAdapter.toTaskRow` 转换与相关 mock**（否则 Activate/Delete/UI 取不到 init 字段）；`Manager` Options（manager.go）增 `LifecycleRunner` 接口依赖（唯一注入点，含 RunScript 与 CopyInherited 两能力，internal/lifecycle 实现，测试注入 mock）
- [x] 3.2 Create 链（crud.go）：worktree.Add 后 **runInherit 编排**：读配置（**读失败是唯一阻断点 → creation_failed**）→ `ListIgnoredUntracked` 枚举（失败 → 警告）→ `CopyInherited` 匹配/复制（失败 → 逐条警告）→ task 层重写 inherit.log（每次重写、无警告删除既有文件、**1MB 上限超限截断加标记**、写失败仅记服务端日志不阻断、**inherit.log 0600 与目录 0700**）→ `CommitCreated` 原子提交（配 init → pending，否则 none）→ 锁外异步链：none 直接 triggerActivate（现状）；pending 启动 InitRunner。Create/retryCreate 内部结果改二态 `{directActivate, startInit}`
- [x] 3.3 retryCreate 闭环：**无论 worktree 产物复用还是重建都重新枚举并幂等执行 inherit**（消除 Add 后、inherit 前崩溃的漏拷窗口）；与 Create 共用 `CommitCreated`（expectedStatus=`creation_failed`）与异步链
- [x] 3.4 InitRunner（新文件 init_run.go）：**admitRunner 顺序固定：先 admission（gate 检查 + WG 登记）后 `ClaimInitRun` CAS；admission 后所有同步退出路径（任务不存在/门禁失败/store error/CAS 失败）MUST 恰好一次释放登记，异步启动后所有权移交 goroutine；gate 已关闭不得修改 init_status（保持 pending 待 Reconcile 收敛）** → 读配置快照执行（配置读取/env 合并/日志创建/脚本执行任一失败 → `FinishInitRun(failed)`）→ `FinishInitRun` CAS 落账；**仅 CAS 成功（rows=1）置 succeeded 后才锁外 triggerActivate**（避免 crud.go:93 同类自锁）；DB error 或 rows=0 MUST NOT 激活；**脚本以 Manager 持有的独立 `runnerCtx` 执行（不复用 SetLifecycleCtx 的 signal ctx——其先于 Shutdown 取消会形成反向窗口；仅 Shutdown 关 gate 后取消）；runner ctx 取消后的最终落账 MUST 用独立短超时非取消 ctx（如 5s Background），仍在 WG 内；init 与 pre-delete 共用 runner WaitGroup，WG 覆盖完整 attempt（配置/env/日志/脚本/最终落账），Done 在最终状态写库之后；Manager Shutdown 顺序：关 gate → cancel runnerCtx → wait runner WG → 关 store**
- [x] 3.5 Activate 门禁（activate.go 前置检查区）：none|succeeded 放行；pending|running → invalid_state；failed → invalid_state 含 init_error 与 Re-run 指引；未知/空值 fail-closed 拒绝
- [x] 3.6 `RerunInit(ctx, taskID)`：**持任务 keyed mutex（tryLockTask）；先 admission（gate 检查 + WG 登记）——gate 已关闭返回错误且 init_status 不变；再门禁检查 + `ClaimInitRerun` CAS**（门禁 task.status=suspended 且 init_status∈{failed,succeeded}，其余 invalid_state；竞争 conflict；**admission 后所有同步退出（门禁失败/任务不存在/store error/CAS 失败）MUST 恰好一次释放登记**）→ 异步执行（不持锁）；成功不自动激活；每次执行覆盖 init.log
- [x] 3.7 Delete/Archive 前置门禁：`init_status ∈ {pending,running}` → invalid_state 拒绝
- [x] 3.8 Reconcile 新增首步 `ConvergeInterruptedInitRuns`：MUST 先于 restoreCleanupDebts 与任务运行时恢复执行；更新失败 fail-closed 阻止 HTTP 开放（同 restoreCleanupDebts 语义）
- [x] 3.9 Delete 挂点（delete.go deleteResume）：二次 dirty 门禁后、wt.Remove 前执行 pre_delete script（2min 超时，pre-delete.log 覆盖写；**Manager 独立 runnerCtx 执行（不得调用 `m.lifecycleCtx()`）+ runner WG 登记，与 init 共用§6.1 机制；**WG 登记 MUST 持有到删除序列成功提交（DB 行删除）或 deletion_failed 落账，而非脚本返回即释放**）；**admission 失败（gate 已关闭）→ 停止删除序列、绝不执行 wt.Remove，本次操作返回错误供下次 Retry**；worktree 目录 **os.Stat 仅 IsNotExist** → 幂等跳过，其他 Stat 错误 → deletion_failed；无配置 → 跳过；配置读取/env 合并/日志创建/脚本执行任一失败 → deletion_failed + **last_error 以固定前缀 `pre-delete:` 开头** 且 MUST NOT 执行 wt.Remove；`DeleteForce` 跳过；删除成功（DB 行删除后）→ best-effort 删除 `<dataDir>/logs/<taskID>/`（忽略错误）；日志文件 0600/目录 0700
- [x] 3.10 mock 测试（沿用 mem store/mock 模式）：Create 链原子提交与成功/失败分支、retryCreate 总是幂等重跑 inherit（含"Add 后、inherit 前崩溃"场景）、`CommitCreated` 的 `creation_failed` 前置、Activate 门禁五分支、RerunInit 门禁与 CAS 竞争及成功不自动激活、**Rerun vs Activate/Delete/Archive 交叉竞态（互斥锁串行化，无 running+activating/deleting 组合）**、InitRunner `FinishInitRun` DB error/rows=0 时不激活、**admitRunner 顺序（admission 先于 claim；gate 关闭时 Rerun 返回错误且 init_status 不变）**、Shutdown 准入竞态（shutdownStarted 后新 runner 不得登记）、**Shutdown 中 pre-delete 执行（ctx 取消收敛）**、**pre-delete admission 失败（gate 关闭）→ 删除停止且 wt.Remove 未执行**、Delete/Archive init 进行中门禁、Delete pre-delete 失败（**last_error `pre-delete:` 前缀**）/Stat 非 ENOENT 错误 fail-closed/Force 跳过/Retry 重跑/worktree 不存在幂等跳过（含 wt.Remove 成功 DB 删除失败后 Retry 收敛）/删除成功日志目录清理、Reconcile 收敛顺序与 fail-closed、**重启后 succeeded 未激活任务不自动激活**、**取消后落账使用独立非取消 ctx（runner ctx 取消后仍能 FinishInitRun 落账）**、**task 层 inherit.log 写入（重写/无警告删除/1MB 上限截断/写失败仅服务端日志/inherit.log 0600 与目录 0700）**、**signal ctx（SetLifecycleCtx 注入）提前取消不影响 runnerCtx（runner 继续执行）**、**admission 后各同步退出路径均恰好一次释放登记（Shutdown wait 不挂起）**

## Phase 4: API

- [x] 4.1 新文件 `internal/api/lifecycle_config.go`：`LifecycleConfigStore` 接口 + store adapter（同 env 模式）+ `GET/PUT /api/v1/projects/{id}/lifecycle-config` handler（GET 缺行空配置；PUT 整体替换，非法 glob → invalid_input+行号，脚本 ≤64KB、**inherit_patterns 整体 ≤16KB**）+ `server.go` 注册 + `cmd/ocdeck-server/main.go` 注入
- [x] 4.2 `TaskBackend` 接口增 **`RerunInit(ctx, taskID) (task.TaskRow, error)`、`ReadInitLog(ctx, taskID) (string, error)`、`ReadPreDeleteLog(ctx, taskID) (string, error)`**；路由 `POST /api/v1/tasks/{id}/rerun-init` **独立 handler（非 handleTaskAction——既有 helper 返回 204；成功 200 + 任务 DTO）**（invalid_state/conflict 映射）、`GET /api/v1/tasks/{id}/init-log`（text/plain，inherit 警告节 + init.log 拼接，tail ≤64KB，**任务不存在 → not_found，先验证任务存在再用可信 taskID 构造路径**，无文件 200 空 body）、`GET /api/v1/tasks/{id}/pre-delete-log`（同上不含 inherit 节）；任务序列化增 `init_status`/`init_error`——**实际序列化路径是 `taskRowDTO` + `toTaskDTO`（tasks.go:252 附近），勿只改导出的 `TaskRowDTO`**
- [x] 4.3 handler 测试：GET 空配置、PUT 非法 glob 422+行号、**inherit_patterns >16KB → 422、项目不存在 GET/PUT → 404**、rerun-init 门禁 422/竞争 409/**成功 200 + 任务 DTO**、两类日志 200 空 body/**任务不存在 → 404**

## Phase 5: UI

- [x] 5.1 `web/src/api.ts` + `types.ts`：lifecycle-config GET/PUT、rerun-init、init-log/pre-delete-log；Task 类型增 init 字段
- [x] 5.2 ProjectDetailPage：新增 "Project Config" 区块——Inherit patterns / Init script / Pre-delete script 三个编辑器 + 保存；文案：inherit"仅复制 gitignored/untracked 文件"、**init"Re-run 或异常崩溃后脚本可能重复/并行执行，需幂等"**、pre-delete"删除重试时会重复执行，需幂等"、三者附"脚本输出会落盘，勿打印敏感凭据"
- [x] 5.3 任务列表/TaskWorkbenchPage：`pending|running` 显示"init 进行中"徽标（**轮询条件含 init_status∈{pending,running}，不只看 task.status**）；`failed` 徽标 + 日志查看；`none|succeeded` 不显示徽标；**Re-run 入口仅 `status=suspended 且 init_status ∈ {failed,succeeded}` 时渲染**（archived+failed 不出现必然 422 的按钮）；init 非 none|succeeded 时**全部激活入口禁用**（**TaskActions 与 Workbench 内联 ActivateButton 两处**）并提示原因；**任务详情始终提供 init 日志查看入口**（inherit 警告经此可见，空日志显示空态）；deletion_failed 且 **last_error 以 `pre-delete:` 前缀识别**时提供 pre-delete 日志查看入口；**DeleteTaskModal 的 Force 选项文案说明同时跳过 pre-delete 脚本**；**失败展示以 init_error/last_error 为主信息（权威），日志仅辅助**（脚本启动前失败时日志可能为旧内容）
- [x] 5.4 前端构建通过 + 手动验证：配置保存 → 创建任务看 init 链 → 失败 Re-run → 配置 pre-delete 删除任务

## 验收

- [x] A.1 `go build ./...` 与 `go test ./...` 全绿
- [x] A.2 `openspec validate project-lifecycle-config` 通过；并用 `openspec show project-lifecycle-config --json` 抽检关键 SHALL 已进入 requirement delta text（空行分段不得导致正文丢失）
- [x] A.3 端到端手动验证：配置三项 → 创建任务（.env 被继承、init 自动执行并激活）→ init 失败场景（阻塞激活、日志、Re-run）→ 删除任务（pre-delete 执行）→ pre-delete 失败（deletion_failed + `pre-delete:` 前缀 → Retry / Force）
- [x] A.4 并发与关停验收：Rerun vs Activate/Delete 交叉竞态测试、Shutdown 中 pre-delete 执行收敛测试、ignored 目录嵌套文件继承真实 repo 测试通过
