# Design: project-lifecycle-config

## 0. 范围与命名

项目级生命周期配置三项：**Inherit patterns**（创建时从主仓库继承 gitignored/untracked 文件）、**Init script**（创建后执行一次）、**Pre-delete script**（删除流程中执行）。

命名决策（已确认）：不使用 emdash 的 preserve/setup/teardown/run/shell-setup 词汇；不使用 `cleanup`（已被 cleanup debt 占用）。落库字段、API、UI 统一用 `inherit` / `init` / `pre_delete`。

明确不做（非目标）：Run script（ocdeck 无 dev-server 预览概念）、Shell setup（与 env 管理重叠）、每次激活都跑的 setup、脚本超时用户可配、脚本在 tmux 可见窗口中执行。

## 1. 总体结构与接线

```
┌────────────────────────────────────────────────────────────────┐
│ web: ProjectDetailPage ─ "Project Config" 区块（3 个编辑器）     │
│      TaskWorkbenchPage ─ init 状态 / 日志 / Re-run 按钮          │
│      删除失败入口 ─ pre-delete 日志查看                          │
└──────────────┬─────────────────────────────────────────────────┘
               │ GET/PUT /api/v1/projects/{id}/lifecycle-config
               │ POST /api/v1/tasks/{id}/rerun-init
               │ GET  /api/v1/tasks/{id}/init-log | pre-delete-log
┌──────────────▼─────────────────────────────────────────────────┐
│ internal/api: lifecycle_config.go（handler + DTO + 路由）        │
│   LifecycleConfigStore 接口 + store adapter（同 env 模式）       │
│   TaskBackend 接口增 RerunInit / task DTO 增 init 字段           │
└──────────────┬─────────────────────────────────────────────────┘
┌──────────────▼─────────────────────────────────────────────────┐
│ internal/task: Manager 编排状态机                                │
│   crud.go       Create/retryCreate → inherit + 异步链            │
│   activate.go   Activate init 门禁                               │
│   delete.go     deleteResume → pre-delete 步骤；Delete/Archive   │
│                 的 init 进行中门禁                                │
│   init_run.go（新）InitRunner：CAS claim → 执行 → CAS finish、    │
│                 RerunInit、Reconcile 收敛                        │
│   adapters.go   StoreAdapter 增 lifecycle 方法                   │
│   manager.go    Options 增 LifecycleRunner 依赖                  │
└──────────────┬─────────────────────────────────────────────────┘
┌──────────────▼─────────────────────────────────────────────────┐
│ internal/lifecycle（新包，纯机制，无 DB/状态机）                 │
│   RunScript(dir, env, script, timeout, logPath) error           │
│   CopyInherited(repoPath, wtPath, entries, patterns) (warnings) │
├────────────────────────────────────────────────────────────────┤
│ internal/git: ListIgnoredUntracked（新增，复用 porcelain v2      │
│   parser + boundedBuffer，-uall 文件级枚举）                     │
└──────────────┬─────────────────────────────────────────────────┘
┌──────────────▼─────────────────────────────────────────────────┐
│ internal/store: migration 0007 + CAS queries                    │
└────────────────────────────────────────────────────────────────┘
```

分层约束：`internal/lifecycle` 只做机制（跑脚本、拷文件），不感知任务状态机；git 枚举走 `internal/git` 封装（禁止 task/lifecycle 层裸执行 git）；状态迁移全部在 `internal/task` 编排，与既有"意图落库 → 副作用序列 → 提交点"哲学一致。

cmd/ocdeck-server/main.go 接线：`store` 新方法经 `StoreAdapter` 注入 `task.Manager`；Manager Options 增 `LifecycleRunner` 接口依赖——唯一注入点，同时覆盖 `RunScript` 与 `CopyInherited` 两个能力（internal/lifecycle 包实现，task 测试注入 mock）；api `Server` 增 `LifecycleConfigStore` adapter 并在 registerRoutes 注册。

## 2. 存储（migration 0007_project_lifecycle_config.sql）

```sql
CREATE TABLE project_lifecycle_configs (
    project_id        TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    inherit_patterns  TEXT NOT NULL DEFAULT '',
    init_script       TEXT NOT NULL DEFAULT '',
    pre_delete_script TEXT NOT NULL DEFAULT '',
    updated_at        INTEGER NOT NULL
);

ALTER TABLE tasks ADD COLUMN init_status TEXT NOT NULL DEFAULT 'none';
ALTER TABLE tasks ADD COLUMN init_error  TEXT;  -- nullable，init_status=failed 时有值
```

- 独立表而非 projects 加列：与 `project_env_vars` 解耦风格一致；无配置项目无行（读时缺行 = 三字段全空）。
- DDL 风格遵循既有约定：不用 IF NOT EXISTS，FK 依赖 `PRAGMA foreign_keys=ON`（store.Open 已开启）。
- `init_status` 域：`none | pending | running | succeeded | failed`。存量任务迁移后为 `none`。

### 2.1 store 方法（全部 CAS / 原子，禁止无条件更新）

| 方法 | SQL 语义 | 用途 |
|---|---|---|
| `GetLifecycleConfig(projectID)` | 缺行返回空配置（非错误） | API 读、执行器读快照 |
| `UpsertLifecycleConfig(projectID, patterns, init, preDelete)` | INSERT … ON CONFLICT DO UPDATE | API 写 |
| `CommitCreated(taskID, expectedStatus, initStatus)` | 单条 UPDATE：`status :expected→suspended` 与 `init_status` 原子提交；rows=0 → 提交失败 | Create（expected=`creating`）/ retryCreate（expected=`creation_failed`）共用提交点 |
| `ClaimInitRun(taskID)` | `SET init_status='running' WHERE id=? AND status='suspended' AND init_status='pending'`；rows=0 → 未获得 | 创建链 InitRunner claim |
| `ClaimInitRerun(taskID)` | 同上但 `init_status IN ('failed','succeeded')` | RerunInit claim |
| `FinishInitRun(taskID, status, err)` | `SET init_status=?, init_error=? WHERE id=? AND init_status='running'`；rows=0 → 记录警告（任务已被外部收敛） | InitRunner 完成落账 |
| `ConvergeInterruptedInitRuns()` | `SET init_status='failed', init_error='interrupted by server restart' WHERE init_status IN ('pending','running')` | Reconcile 启动收敛 |

统一约定（与既有 `UpdateTaskStatus` 语义一致）：`CommitCreated` 后置条件 MUST 含 `last_error=NULL` 与 `updated_at=now`（否则 retryCreate 成功后残留旧 creation_failed 错误）；其余 init CAS MUST 刷新 `updated_at`；`ClaimInitRerun` 置 running 时 MUST 同时清空旧 `init_error`。

## 3. init_status 状态迁移表（唯一权威）

| 事件 | 前置（CAS 条件） | 后置 | 副作用 |
|---|---|---|---|
| Create/retryCreate 提交（配 init） | status ∈ {creating, creation_failed}（CommitCreated expectedStatus） | suspended + **pending** | 启动异步 InitRunner |
| Create/retryCreate 提交（未配 init） | status ∈ {creating, creation_failed} | suspended + **none** | 直接 triggerActivate（现状） |
| InitRunner claim | suspended + pending | **running** | 执行脚本（10min 超时） |
| InitRunner 成功 | running | **succeeded**（init_error 清空） | `FinishInitRun` CAS 成功（rows=1）**之后**才 triggerActivate（锁外调用，见 §4）；DB error 或 rows=0 MUST NOT 激活 |
| InitRunner 失败 | running | **failed** + init_error | 无（MUST NOT 激活） |
| RerunInit claim | suspended + (failed\|succeeded) | **running** | 执行脚本；成功**不**自动激活 |
| 服务重启收敛 | pending\|running | **failed** + "interrupted by server restart" | 无（MUST NOT 自动重跑） |

不变量：
1. `pending|running` ⇒ task.status 必为 `suspended`（Claim 的 CAS 条件保证；Delete/Archive 门禁保证不被破坏，见 §6）。
2. `suspended + pending` 由 `CommitCreated` 单条 SQL 原子产生，**不存在 `suspended+none` 却应跑 init 的窗口**。
3. Activate 门禁对未知/空 init_status **fail-closed** 拒绝（invalid_state）。
4. 每次执行尝试（初次 / Re-run / 删除 Retry 的 pre-delete）**开始时读一次当前配置**；执行中不受后续配置修改影响；修改后的脚本供后续 Re-run/Retry 使用。
5. init 执行链任何步骤失败（配置读取、env 合并、日志文件创建、脚本执行）→ 一律 `FinishInitRun(failed)` 落账，MUST NOT 激活；pre-delete 执行链任何步骤失败 → 一律落 `deletion_failed`，MUST NOT 执行 `wt.Remove`。

变体路径全部归并到上表：retryCreate 走与 Create 相同的提交点（§3.1）；Re-run 与初次执行共享 InitRunner 执行体（仅 claim 条件与成功后行为不同）。

### 3.1 retryCreate 闭环

- **无论 worktree 产物是复用（VerifyWorktreeProduct 通过）还是重建，retryCreate 都重新枚举并幂等执行 inherit**（CopyInherited 对目标已存在的文件自动跳过）。理由："worktree 存在 ⇒ inherit 已执行"不成立——服务可能在 worktree.Add 后、inherit 前崩溃，或配置读取在 Add 后失败，跳过重拷会永久漏掉 `.env` 等文件；幂等重拷成本极低且语义可靠。
- 读取项目配置失败 → 落 `creation_failed` + last_error（与既有失败路径一致）。
- Create/retryCreate 共用 `CommitCreated(taskID, expectedStatus, initStatus)` 提交点：Create 传 `creating`，retryCreate 传 `creation_failed`。内部结果用二态枚举 `{directActivate, startInit}`：未配 init → directActivate（沿用现状 autoActivateWG）；配 init → startInit（启动 InitRunner）。

## 4. Create 主流程与并发锁时间线

```
Create(projectID, taskName)                    [持任务锁]
  │  前置检查（既有，无副作用）
  │  ① 插入 creating 行
  │  ② worktree.Add                     失败 → creation_failed（既有）
  │  ③ runInherit 编排（同步）：读配置（失败 → creation_failed，唯一阻断点）
  │     → ListIgnoredUntracked 枚举（失败 → 警告）
  │     → CopyInherited 匹配/复制（失败 → 逐条警告）
  │     → task 层重写 inherit.log（写失败 → 仅记服务端日志，不阻断）
  │  ④ CommitCreated 原子提交           suspended + (pending|none)
  ▼  [释放任务锁]
异步链（锁外，与 triggerActivate 同一模式）：
  none    → triggerActivate（autoActivateWG，现状）
  pending → InitRunner：
      ClaimInitRun CAS 失败 → 放弃（并发下另一执行者已 claim）
      成功 → RunScript → FinishInitRun CAS 落账
      succeeded → triggerActivate（锁外调用，避免 crud.go:93 同类自锁）
```

并发互斥矩阵：

| 并发对 | 机制 |
|---|---|
| InitRunner 成功 → triggerActivate vs 手动 Activate | 既有 keyed mutex + 状态门禁（suspended→activating CAS）天然互斥 |
| 手动 Activate vs init 进行中 | Activate 门禁：pending/running/failed → invalid_state 拒绝 |
| Delete/Archive vs init 进行中 | Delete/Archive 前置门禁：`init_status ∈ {pending,running}` → invalid_state 拒绝（保不变量 1；init 通常秒级~分钟级，用户稍候或待其失败） |
| RerunInit vs InitRunner 执行中 | Claim CAS 互斥（running 时两类 claim 均失败 → conflict） |
| RerunInit vs Activate/Delete/Archive | **RerunInit MUST 持任务 keyed mutex（tryLockTask）完成门禁检查 + Claim CAS**（脚本执行异步、不持锁）。必要性：`succeeded→running` 转移中，Activate 门禁（succeeded 放行）与 Delete/Archive 可能并发通过，形成 running+activating/deleting 非法组合；与 Activate/Delete/Archive 共用同一把任务锁后，双方门禁读取的都是串行后的最新状态 |
| 配置 PUT vs 执行中脚本 | 无互斥；执行用启动时快照（不变量 4） |
| Shutdown vs 执行中脚本 | InitRunner goroutine 登记 WaitGroup 时 MUST 经与 autoActivateWG 相同的 Shutdown 准入（shutdownGateMu + shutdownStarted 检查）：门已关则不得新登记（任务保持 pending，由下次 Reconcile 收敛），杜绝 Shutdown Wait 与新 goroutine 登记的竞态；已登记者随**独立 runnerCtx** 取消杀脚本进程组（**不得复用 `m.lifecycleCtx()` 的 signal ctx**，见 §6.1）→ FinishInitRun 尽力落账；未收敛者由下次 Reconcile 收敛 |

## 5. Activate 门禁

`Activate` 前置检查区（activate.go:223-236 既有三连查之后）新增：

```
none | succeeded → 放行
pending | running → invalid_state："init in progress"
failed → invalid_state："init failed: <init_error>；修复脚本后 Re-run"
其他（未知/空值）→ invalid_state（fail-closed）
```

fail-closed 理由：init 典型内容是装依赖，绕过门禁激活出依赖不全的 agent 会话，错误更晚更难定位。escape hatch：项目方清空 init_script 后 Re-run（空脚本立即成功）等价"跳过"。

## 6. Delete 流程：Pre-delete 挂点与门禁

```
Delete/Archive 前置（新增）：init_status ∈ {pending,running} → invalid_state 拒绝

deleteResume（delete.go:88 既有序列）
  ② RetryReap debt → ③ 删 oc sessions → ④ kill 残余会话
  ⑤ 二次 dirty 门禁（既有）
  ⑤.5 Pre-delete（新增）：
      worktree 目录 os.Stat：仅 IsNotExist → 跳过（幂等，与"资源不存在视为成功"一致；
        覆盖 wt.Remove 已成功而 DB 删除失败后 Retry、creation_failed 无 worktree 两场景）；
        其他 Stat 错误 → deletion_failed（fail-closed，不当不存在处理）
      读当前配置，无 pre_delete_script → 跳过
      执行（2min 超时，pre-delete.log 覆盖写；ctx/WG 见 §6.1 runner 所有权）
      配置读取 / env 合并 / 日志创建 / 脚本执行任一失败 → deletion_failed + last_error
        （既有模式，Retry 重入并重跑脚本 → 脚本需幂等；MUST NOT 执行 wt.Remove）
        last_error MUST 以固定前缀 `pre-delete:` 开头（UI 据此稳定识别并展示日志入口）
  ⑥ wt.Remove（ForceDirty）→ ⑦ 删 DB 行
  ⑧ 日志目录 `<dataDir>/logs/<taskID>/` best-effort 删除（提交点后、忽略错误）
```

- **位置理由**：进程已死（端口/锁释放），worktree 还在；在二次 dirty 门禁之后，脚本对 worktree 的改动不触发门禁（wt.Remove 本就 ForceDirty），不破坏门禁保护用户数据的语义。
- **Force 跳过**：`DeleteForce` MUST 跳过 pre-delete——这是对既有"Force 只能跳过 ③"语义的正式扩展（见 task-lifecycle spec delta），作为脚本持续失败的逃生舱。
- 超时 2 分钟写死（v1 不可配）。

### 6.1 脚本进程所有权与 crash model

- **执行 ctx：Manager 持有的独立 `runnerCtx/runnerCancel`，不归属 HTTP 请求，也不得复用 `SetLifecycleCtx` 注入的 signal ctx**——signal ctx 在信号到达时先取消（main.go:67/109），而 HTTP drain 最多 5 秒后才调 `Manager.Shutdown`（server.go:281、main.go:146）；复用会形成"先 cancel、后关 gate"的反向窗口（脚本在准入关闭前被取消）。Manager MUST 自建 runnerCtx；**仅 Shutdown 在关 gate 后取消它**。
- **统一 runner 准入与等待组（admitRunner 协议）**：init 与 pre-delete 共用一套 runner WaitGroup + Shutdown 准入（shutdownGateMu + shutdownStarted，与 autoActivateWG 同机制）。**顺序固定：先 admission（gate 内检查 + 登记 WG）后 Claim CAS；CAS 失败立即释放登记；gate 已关闭时不得修改 init_status**——杜绝"CAS 已置 running 但 runner 无法登记 → 任务无执行者"的窗口。**admission 返回 exactly-once release token：admission 后所有同步退出路径（任务不存在 / 门禁失败 / store error / CAS 失败）MUST 恰好一次释放；异步 goroutine 成功启动后所有权移交 goroutine，由其在 attempt 完成后释放**——任一遗漏会导致 Shutdown 永久等待。对 RerunInit：admission 失败（Shutdown 进行中）→ 返回错误且 init_status 不变。
- **WG 覆盖完整执行尝试**：登记的一次 attempt 覆盖 配置读取 / env 合并 / 日志准备 / 脚本执行 / 最终状态落账 全过程，**Done 在最终状态写库之后**——保证 Shutdown wait 结束时所有 attempt 均已落账（或已尽力落账），不会在落账前关闭 store。**pre-delete 的登记 MUST 持有到删除序列成功提交（DB 行删除）或 deletion_failed 落账**，而非脚本返回即释放——否则 deleteResume 仍在执行 wt.Remove/DB 删除时 WG=0，Shutdown 会提前关闭 store。**runner ctx 取消后的最终落账 MUST 使用独立短超时非取消 context**（复用已取消的 runner ctx 调 FinishInitRun/UpdateTaskStatus 会立即返回 context canceled）：如 `context.WithTimeout(context.Background(), 5s)`，该落账仍在 WG 覆盖内。
- **pre-delete admission 失败的后置条件**：gate 已关闭时 MUST 停止删除序列、**绝不执行 wt.Remove**；本次删除操作返回错误（任务保持 deleting 或 best-effort 落 deletion_failed），供下次 Retry 重新执行脚本。
- **关停顺序**：关 gate → cancel runner ctx（杀在跑脚本进程组）→ wait runner WG → 关闭 store。保证在跑脚本已终止（或已尽力落账）后才关 DB。
- **Crash model（明示，v1 不提供严格 exactly-once）**：优雅关停可取消并收敛在跑脚本；**SIGKILL/崩溃可能遗留仍在运行的脚本进程**，Reconcile 只收敛 DB（running/pending→failed），无法定位并回收旧进程。用户 Re-run 可能与旧脚本并行——**init 与 pre-delete 脚本都 MUST 幂等**（与 pre-delete Retry 重跑的要求一致，扩展至 init）。
- **At-most-once 激活调度限制**：init 成功落库后、`triggerActivate` 前崩溃会留下 `suspended+succeeded`，重启后 MUST NOT 自动激活（Reconcile 不重放调度），由用户手动激活。`FinishInitRun` DB 失败会留下 running，在线期间 Activate/Delete/Rerun 均被门禁拒绝，重启后由 Reconcile 收敛。

## 7. 执行机制

### 7.1 internal/lifecycle

包以 `LifecycleRunner` 接口形式注入 task.Manager（Options 字段，task 测试注入 mock）：

```go
// LifecycleRunner 生命周期脚本与文件继承机制（internal/lifecycle 实现，task 测试 mock）。
type LifecycleRunner interface {
    // RunScript 在 dir 下以 env 执行 script（/bin/sh -c），stdout+stderr 写入 logPath
    //（每次执行 truncate 重写；RunScript 是该日志文件的唯一写入者）。
    // 捕获输出上限 1MB，超限截断并追加 "[log truncated at 1MB]" 标记。
    // timeout 到期杀整个进程组（避免孙子进程泄漏）返回超时错误。exit 0 返回 nil。
    RunScript(ctx context.Context, dir string, env map[string]string, script, logPath string, timeout time.Duration) error

    // CopyInherited 将 entries（来自 internal/git 的文件级枚举）中匹配 patterns 的
    // 文件从 repoPath 复制进 wtPath，保持相对路径与权限；符号链接按链接复制；
    // 普通文件 MUST no-clobber 原子发布：同目录临时文件完整写入（fsync+chmod）后
    // link(2) 到目标（EEXIST → 目标已存在，跳过+警告），再 unlink 临时文件——
    // rename 会覆盖并发出现的目标，禁止直接 rename；destination 路径任一祖先
    // 为符号链接时 MUST 拒绝（防逃逸/防覆写他处）；
    // 路径 containment 校验（拒绝绝对路径/.. 逃逸）。
    // 匹配与复制机制失败一律降级为逐条警告返回，不返回阻断性 error。
    //（枚举由 internal/git.ListIgnoredUntracked 完成、inherit.log 由 task 层写入，
    //  两者失败的降级处理见 §4 runInherit 编排；Create 链唯一可阻断的前置是配置读取失败。）
    CopyInherited(ctx context.Context, repoPath, wtPath string, entries []git.FileStatus, patterns []string) (warnings []string)
}
```

### 7.2 internal/git 枚举（新增）

`ListIgnoredUntracked(ctx, repoPath) ([]FileStatus, error)`：`git status --porcelain=v2 -z --ignored=traditional --untracked-files=all`。MUST 用 `--ignored=traditional`（已实测：`--ignored=matching` 对 ignored 目录只返回 `! dir/`，不展开内部文件；traditional + `-uall` 才返回文件级记录）。`-uall` 展开未跟踪目录，不会把目录内已跟踪文件卷入；返回仅含 untracked(`?`) 与 ignored(`!`) 两类。

**parser 扩展（既有代码不兼容，必须改）**：`parsePorcelainEntry`（parser_status.go:116）当前把 `!` 条目当错误丢弃，且 `FileStatus` 无 ignored 标记。需扩展：解析 `!` 记录并给 `FileStatus` 增加 `Ignored bool` 字段；既有 `Status` 调用方行为不变（默认不包含 ignored 条目，新枚举函数单独入口）。复用 `boundedBuffer`（有界输出）+ 参数白名单（无选项注入面）。glob 过滤用 `github.com/bmatcuk/doublestar/v4`（支持 `**`；PUT 校验与执行共用同一库）。`.git` 条目 MUST 排除。

### 7.3 env 层叠（重构点）

从 `mergeEnvSnapshot`（activate.go:68-150）抽出可复用的层叠函数（baseline → global → project → task → 生命周期变量，不含 port 参数与持久化），mergeEnvSnapshot 改为调用它。

**行为不变量**：serve/tui/shell 的 env 内容与顺序完全不变（既有测试全绿 + 矩阵覆盖 TERM/locale 兜底/follow_host/manual/reserved key 过滤/快照持久化）。

脚本执行 env = 层叠结果（含 `OCDECK_TASK_ID/TASK_NAME/TASK_PATH/PROJECT_PATH`），**不含 `OCDECK_SERVE_PORT`**（无 serve），不持久化快照。

### 7.4 日志契约（单一写入者）

| 文件（`<dataDir>/logs/<taskID>/`） | 写入者 | 写时机 |
|---|---|---|
| `inherit.log` | Create 链 inherit 步骤 | 每次执行 inherit 时重写；本次无警告则删除既有文件（避免陈旧警告随 init-log API 继续返回）；写入失败仅丢警告、不阻断创建 |
| `init.log` | RunScript（init 执行） | 每次 init 执行 truncate 重写 |
| `pre-delete.log` | RunScript（pre-delete 执行） | 每次 pre-delete 执行 truncate 重写 |

- 三个文件各有唯一写入者，无"预写警告被 truncate 覆盖"冲突。
- **1MB 上限统一适用于三个日志文件**：RunScript 捕获的脚本输出超限截断加标记；inherit 警告写入 inherit.log 同样以 1MB 为上限（超限丢弃后续警告并追加截断标记）。
- **权限**：日志文件 MUST 0600、日志目录 MUST 0700（与 dataDir 0700 一致；日志可能含用户脚本打印的敏感信息）。
- 执行链在脚本启动前失败（配置读/env 合并/日志创建）时，日志文件可能仍是上一次执行的旧内容——**UI 以 `init_error`/`last_error` 为本次失败的权威信息**，日志仅供参考。
- `GET init-log` 响应 = inherit.log（若有，冠以 `[inherit warnings]` 节）+ init.log 拼接，tail ≤64KB。
- 日志红线（对齐 env-management spec §明文存储与日志红线）：ocdeck 自身 MUST NOT 向日志写 env 值；脚本输出由用户控制，UI 编辑器旁 MUST 提示"脚本输出会落盘到 `<dataDir>/logs/`，勿在脚本中打印敏感凭据"。

## 8. API

统一错误码映射（errors.go）：`invalid_input`/`invalid_state` → 422，`conflict` → 409，`not_found` → 404。

| 路由 | 语义 |
|---|---|
| `GET /api/v1/projects/{id}/lifecycle-config` | 缺行返回三字段空串（200）；项目不存在 → not_found |
| `PUT /api/v1/projects/{id}/lifecycle-config` | 整体替换 upsert；非法 glob → invalid_input + 行号；脚本 >64KB 或 inherit_patterns 整体 >16KB → invalid_input |
| `POST /api/v1/tasks/{id}/rerun-init` | **独立 handler**（非 handleTaskAction——既有 helper 返回 204）；门禁不满足 → invalid_state；CAS 竞争 → conflict；成功 **200 + 任务 DTO**（异步执行已登记，非同步完成） |
| `GET /api/v1/tasks/{id}/init-log` | text/plain；**任务不存在 → not_found（先验证任务存在，再用可信 taskID 构造路径）**；无日志文件 → 200 空 body；tail ≤64KB |
| `GET /api/v1/tasks/{id}/pre-delete-log` | 同上 |

TaskBackend 增 `RerunInit(ctx, taskID) (task.TaskRow, error)`、`ReadInitLog(ctx, taskID) (string, error)`、`ReadPreDeleteLog(ctx, taskID) (string, error)`。任务序列化（`taskRowDTO`/`toTaskDTO`）增 `init_status`、`init_error`。

配置变更生效：每次执行尝试开始时读一次（不变量 4）；进行中的执行不受影响。

## 9. UI

- **ProjectDetailPage**：新增 "Project Config" 可折叠区块（Env 区块旁），三个 textarea（Inherit patterns / Init script / Pre-delete script）+ 保存。文案：inherit 说明"仅复制 gitignored/untracked 文件"；init 说明"Re-run 或异常崩溃后脚本可能重复/并行执行，需幂等"；pre-delete 说明"删除重试时会重复执行，需幂等"；三者附日志红线提示。
- **任务列表 / TaskWorkbenchPage**：`pending|running` 显示"init 进行中"徽标；`failed` 显示失败徽标 + 查看日志；Re-run 入口仅 `status=suspended 且 init_status ∈ {failed,succeeded}` 时提供（与后端门禁一致）；`none|succeeded` 不显示徽标（无干扰）；激活入口在非 `none|succeeded` 时禁用并 tooltip 原因。**接线点覆盖全部激活入口**：TaskActions 与 Workbench 内联 `ActivateButton`（TaskWorkbenchPage.tsx:267 附近）两处。轮询条件 MUST 把 `init_status ∈ {pending,running}` 视为活跃（不能只轮询 task.status，否则 init 状态更新滞后）。
- **生命周期日志入口**：任务详情 MUST 始终提供 init 日志查看入口（覆盖 inherit 警告——init=none 时 inherit.log 是唯一可见性渠道；无日志时显示空态），删除因 pre-delete 失败（`last_error` 以 `pre-delete:` 前缀稳定识别）时提供 pre-delete 日志查看入口。
- **DeleteTaskModal**：Force 选项文案 MUST 说明"同时跳过 pre-delete 脚本"（DeleteTaskModal.tsx:67 附近）。

## 10. 错误处理汇总

| 失败点 | 语义 | 用户出口 |
|---|---|---|
| inherit 复制（单文件/枚举） | 警告写 inherit.log，不阻断 | 看日志，手动拷 |
| 配置读失败（Create 链） | creation_failed（既有） | Retry |
| init 执行（非零/超时） | failed，阻塞激活（invalid_state） | 看日志 → 改脚本/环境 → Re-run（或清空脚本 Re-run 跳过） |
| init 被服务重启打断 | 重启收敛为 failed | Re-run |
| init 进行中 Delete/Archive/Activate | invalid_state 拒绝 | 等执行结束 |
| pre-delete 执行 | deletion_failed（既有） | 看 pre-delete 日志 → Retry 重跑 / Force 跳过 |
| PUT 非法 glob / 超长 | invalid_input + 行号 | 改配置 |
| RerunInit 状态不满足 / CAS 竞争 | invalid_state / conflict | 按状态提示操作 |

## 11. 测试策略

- `internal/git`：ListIgnoredUntracked 真实 repo 测试（untracked/ignored/tracked 混合、`-uall` 展开、**ignored 目录内嵌套文件展开**、有界输出超限）。
- `internal/lifecycle` 单测（真实临时 git repo）：脚本成功/非零/超时杀进程组/日志 truncate 重写/1MB 截断；inherit 的 `**` 嵌套、目录递归、符号链接（含 broken）、**no-clobber（并发目标不被覆盖）**、**ancestor symlink 拒绝**、目标已存在跳过、containment 拒绝、`.git` 排除、tracked 文件不匹配、日志 0600/目录 0700。
- `internal/task` mock 测试（沿用 mem store/mock 模式）：
  - Create 链：CommitCreated 原子性（suspended+pending 单事务、last_error 清空、updated_at 刷新）；init 成功→自动激活；失败→不激活+落账；retryCreate **复用与重建均调用 inherit**（幂等，断言 CopyInherited 两次均被调用）；配置读失败→creation_failed；
  - Activate 门禁五分支（含未知值 fail-closed）；
  - RerunInit：门禁（非 suspended / running 中 / CAS 竞争）、成功后不自动激活；**Rerun vs Activate/Delete/Archive 交叉竞态（互斥锁串行化，无 running+activating/deleting 组合）**；**admitRunner 顺序（admission 先于 claim；gate 关闭时 Rerun 返回错误且 init_status 不变）**；
  - Delete/Archive 的 init 进行中门禁；
  - Delete：pre-delete 失败→deletion_failed；**pre-delete admission 失败（gate 关闭）→ 删除停止且 wt.Remove 未执行**；Force→跳过；Retry→重跑；worktree 不存在→幂等跳过（含 wt.Remove 成功 DB 删除失败后 Retry 收敛）；Stat 非 ENOENT 错误 fail-closed；
  - Reconcile：收敛在 restoreCleanupDebts/ListAllTasks 之前执行、更新失败 fail-closed 阻止 HTTP 开放；**重启后 succeeded 未激活任务不自动激活**；
  - **Shutdown：运行中脚本被取消并尽力落账后才关 store（WG 覆盖完整 attempt）；signal ctx 提前取消不影响 runnerCtx；admission 后各同步退出路径恰好一次释放登记**；task 层 inherit.log 写入（重写/无警告删除/1MB 截断/写失败仅服务端日志/0600 与目录 0700）。
- env 抽取重构：既有测试全绿 + 矩阵补 TERM/locale/follow_host/reserved key/不持久化快照。
- `internal/store` 真实 SQLite：0007 应用、CRUD、CASCADE、存量 init_status=none、各 CAS 方法条件不满足时 rows=0。
- API handler：GET 空配置、PUT 非法 glob 422+行号、**inherit_patterns >16KB → 422、项目不存在 GET/PUT → 404**、rerun-init 门禁 422/竞争 409/**成功 200+任务 DTO**、两类日志 200 空 body/**任务不存在 → 404**。
- 前端：构建 + 手动验证流程。

## 12. 实施分期（tasks.md 展开）

1. **存储层**：migration 0007 + CAS queries + 测试。
2. **机制层**：internal/git 枚举 + doublestar 依赖 + internal/lifecycle + env 层叠抽取重构 + 测试。
3. **task 编排**：Create 链 / 门禁 / RerunInit / Reconcile / Delete 挂点 / StoreAdapter+Options 接线 + 测试。
4. **API**：接口 + adapter + 路由 + main.go 接线 + handler 测试。
5. **UI**：两页面 + 删除失败日志入口 + 构建与手动验证。

依赖顺序严格 1→2→3→4→5；每期独立可验证。
