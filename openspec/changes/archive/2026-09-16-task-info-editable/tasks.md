# Tasks: task-info-editable

实现顺序遵循 design D8；核心文件（crud/ports/store）不拆多 lane 并行。每个任务的验证方式写在条目内。

## 1. 契约与持久化

- [x] 1.1 新增 migration：`tasks` 表加可空 TEXT 列 `rename_pending`（JSON，默认 NULL），迁移失败 fail-closed。验证：新库/存量库启动均成功，`PRAGMA table_info(tasks)` 含该列
- [x] 1.2 `internal/application/ports.go` 新增端口：`CommitTaskInfoUpdate(ctx, id, update TaskInfoUpdate) (MutationResult, error)`（单事务原子提交 name/branch/env 快照 + 清除 rename_pending；同值 no-op，`Changed` 仅由业务列决定，意图清除本身不推进 `updated_at`）、`SetTaskRenamePending` / `ClearTaskRenamePending`；`internal/infrastructure/store` 实现（SQL WHERE 排除同值、NULL 安全）；**内部 store row/service 映射携带 `rename_pending`（MUST NOT 进入公共 Task DTO、TaskSnapshot 或 facade 转换）**。验证：store 单测覆盖同值 no-op、跨秒/同秒 updated_at、意图写清；API DTO 回归断言 rename_pending 不外泄
- [x] 1.3 store 层测试：失败矩阵持久化侧（提交失败意图保留、意图清除失败语义）。验证：`go test ./internal/infrastructure/store/...` 通过且新测试在旧实现下失败（mutation 式验证）
- [x] 1.4 application 层提交事件链接线：`LifecycleService` 扩展 `CommitTaskInfoUpdate` 调用路径，业务提交经 `commitTaskMutation`，仅 `Changed=true` 时发布一次 `task.activity_changed`；pending-only 操作（仅设置/仅清除意图）不发事件、不推进 `updated_at`。验证：四种事务结果各有测试（仅设置 pending / 业务列变化并清除 pending / 仅清除 pending / 业务列同值并清除 pending），断言 `Changed`、事件发布次数与 `updated_at`

## 2. git 原语 + 核心改名流程 + R1 收敛

- [x] 2.1 `internal/infrastructure/git`（或 worktree 包，按既有归属）新增 `RenameBranch(ctx, worktreePath, oldName, newName)` 原语（`git -C <worktreePath> branch -m`）。验证：真实 git worktree 改名测试通过——HEAD 跟随、路径不变、reflog 保留、目标冲突/非法名报错且零副作用
- [x] 2.2 `internal/task` 新增 `UpdateTaskInfo` 用例（严格按 design D1-D3：任务锁内读取 → R1 收敛（见 2.3）→ presence/字段矩阵校验 → 含分支变更路径：以 `proj.Path` 获取 repo 写锁 → 锁内 HEAD 身份验证 + 目标复查（任一失败在意图写入前拒绝，零副作用）→ 意图写入 → `git branch -m` → `CommitTaskInfoUpdate` 单事务提交 → 失败补偿；纯名称路径：快照矩阵 → 单事务提交 → 标题同步后处理；锁覆盖 D2 整个临界区）。验证：`go test ./internal/task/...` 新增用例全通过；断言身份不匹配、锁内目标冲突、锁获取失败时 pending/业务列/git 均不变
- [x] 2.3 R1 收敛函数（按 worktree 实际 HEAD 三分支判定：已是新名→按意图补整次提交；仍是旧名→清意图；无法判定→保留+明确错误）+ 生命周期入口仲裁接线（Suspend 状态流转前、Activate `beginActivation` 前且不进清快照补偿、运行期 Recovery `ensureRecovery` CAS 前）。启动时序：启动 reconcile 在只读模式预检完成之后、生命周期恢复/清理之前执行 R1；任一 R1 未收敛错误（HEAD 不可判定/补提交失败/清意图失败）→ fail-closed 拒开 HTTP、生命周期恢复/清理不执行；R1 补提交成功后 MUST 重新读取任务列表再继续。关停路径：R1 失败不阻止既有进程终止与 goroutine join，错误与清理错误聚合、Shutdown 返回非 nil，watchdog 按既有契约继续运行。验证：仲裁表每个入口各有测试；启动四种路径分别测试（HEAD 不可判定/补提交失败/清意图失败/成功补提交后重读任务列表）；shutdown fake/process fixture 注入 R1 失败，断言进程终止与 join 仍发生、pending/status/env 保留、返回非 nil
- [x] 2.4 失败矩阵逐行映射测试（意图写入失败 / git 结果未知 / DB 提交失败 / 补偿失败 / 清意图失败 / 启动补提交失败）+ 恢复算法测试（含稳定 ID、三分支 HEAD 判定、嵌套 slug 原样重放）。验证：逐行断言新测试在旧实现下失败

## 3. 配置 + 外部 client + 创建路径

- [x] 3.1 `branchprefix` Store（dataDir `branch-prefix.json`，缺省 `ocdeck`，损坏降级默认值 + 可观察错误，原子写入）+ `GET/PUT /api/v1/config/branch-prefix` handler（镜像 `internal/api/palette_config.go`；PUT 校验 `^[a-z0-9]([a-z0-9-]{0,48}[a-z0-9])?$`，非法 invalid_input 不写盘）+ composition root 装配（`cmd/ocdeck-server/main.go:154` 区域：Store 初始化与默认值加载、API 路由注册、Manager/Service 注入）。验证：API 测试覆盖缺省/保存/非法拒绝/损坏降级；服务启动后 GET/PUT branch-prefix 实际可达
- [x] 3.2 `*opencode.Client` 新增 `UpdateSessionTitle`（PATCH `/session/{id}`，404/网络/超时统一降级）+ `OCClient` 接口扩展 + 全部 mock/fake 使用方同步扩展 + CONTRACT.md 增补（标记为目标契约）+ 能力缓存（首次 PATCH 实测，按 `taskID + instVersion` 缓存，复用 diffreview_capability registry；runtime 实例更换失效）。验证：编译并运行全部 `OCClient` mock/fake 使用方；测试覆盖 `PATCH /session/{id}?directory=...` 的 PathEscape/QueryEscape、请求体 title、成功响应 ID 校验、404 缓存、网络/超时不缓存、instVersion 变化失效
- [x] 3.3 创建路径接入：`createTaskReq` 新增 `branch_slug`（presence 语义）→ Manager → `createRepo`；`crud.go:276` 前缀改为读取配置快照（单次读取）；`branchDirSlug` 参数化剥离前缀 + `normalizeSlug`；dir/local-path + 非空 slug → invalid_input。验证：创建测试覆盖显式 slug（跳过 LLM/slugify）、slug 非法/冲突（invalid_input/conflict 零副作用）、前缀变更仅影响新任务
- [x] 3.4 `internal/api/tasks.go` 新增 `PATCH /api/v1/tasks/{id}` 路由 + handler + `decodeOptionalTaskInfoPatchJSON`（空 body → `{}` 幂等 200；`{}`/null 字段 → 未提供；非法 JSON/尾随内容/类型错误 → invalid_input；`*string` 保留 presence；不改既有 `decodeJSON`）+ 错误映射（invalid_input/invalid_state/conflict/git_error/internal）。验证：API 测试覆盖字段矩阵与错误映射全部分支

## 4. Web UI（依赖标注：4.1 依赖 D1/D4 DTO 固定；4.2 依赖 PATCH response/error DTO 固定（3.4）；4.3 依赖 branch-prefix GET DTO 固定（3.1）；4.4 依赖 branch-prefix GET/PUT API 固定（3.1）；满足对应前置后可与 2-3 并行）

- [x] 4.1 `web/src/api.ts` 新增 `updateTask`（PATCH）+ `getBranchPrefix`/`putBranchPrefix`；`types.ts` 同步 DTO。验证：api 包装测试（镜像 `api-permission-mode.test.ts` 风格）
- [x] 4.2 `TaskInfoCard` 组件挂入工作台 settings pane：展示态连续字段 + 单编辑按钮 → 编辑态（name/slug 可编辑、slug 预览沿用原前缀、取消/保存）；slug 变化时保存前危险确认 modal（复用 `DeleteTaskModal` shell）；失败保持编辑态；gitless 任务无 slug 输入；**保存结果不确定（超时/网络错误，服务端可能已提交）时 MUST 先刷新任务、以当前分支重新确认目标 slug 后再允许再次提交，MUST NOT 直接重发旧请求**（嵌套 slug 重放防护，design D2 / task-lifecycle spec 场景）；保存成功后触发共享 projects store `refresh()`。验证：组件测试覆盖展示/编辑/确认/失败/gitless 矩阵 + 不确定结果场景（模拟服务端已提交但响应丢失，断言再次提交前刷新并按当前分支重新确认，不重发旧请求）
- [x] 4.3 `NewTaskPanel` 折叠高级选项 + slug 输入 + 实时预览 `<prefix>/<slug>`（前缀经 `getBranchPrefix` 拉取；GET 未成功时显示「前缀未加载」占位、MUST NOT 伪造预览；加载状态不作为提交门禁）；仅 repo+worktree 渲染；slug 不参与提交门禁、模式/项目切换保留。验证：`command-center-new-task` 测试扩展（成功加载/加载失败占位/切换项目模式 slug 保留/仅 repo+worktree 渲染）
- [x] 4.4 `SettingsPage` 新增分支前缀 tab（`ConfigsTab` + `router.ts` CONFIGS_TABS 扩展 + 深链）+ `BranchPrefixPanel`（镜像 `PaletteConfigPanel`）。验证：设置面板测试（镜像 `palette-settings.test.tsx`）

## 5. 端到端验收

- [x] 5.1 全量回归：`go test ./...` + `cd web && pnpm test` + `pnpm build` 通过；`openspec validate task-info-editable --strict` 通过
- [x] 5.2 手工验收：创建任务（默认/指定 slug/改前缀后新建）→ 改名（挂起/活跃/dir）→ 分支改名（成功/冲突/gitless 拒绝/危险确认）→ 中断恢复（kill 进程于 git 后 DB 前，重启收敛）逐项符合 spec 场景（实际范围：实时冒烟 12 步全 PASS——指定 slug 创建、改名、分支改名保留路径/HEAD、非法/冲突拒绝、前缀仅影响新任务、改名沿用原前缀、local-path invalid_state、鉴权与幂等空 PATCH；中断恢复由失败矩阵自动化测试覆盖）
