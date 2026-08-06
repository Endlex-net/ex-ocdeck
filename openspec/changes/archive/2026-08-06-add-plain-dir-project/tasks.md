# Tasks: add-plain-dir-project

实现 lane 顺序：DB/store → kind/API/DTO → session 归属隔离基础（dir 并行任务前置）→ create/delete/reconcile/gitops 分叉 → UI → repo 回归验证。

## 1. DB 与 store 层

- [x] 1.1 新增迁移 `internal/store/migrations/0008_project_kind.sql`：`ALTER TABLE projects ADD COLUMN kind TEXT NOT NULL DEFAULT 'repo'`（存量行回填 repo，零数据迁移）；`ALTER TABLE tasks ADD COLUMN base_ref TEXT NOT NULL DEFAULT ''`；存量 repo 任务回填 `base_ref = 'refs/heads/' || 项目 default_branch`（迁移时冻结，此后 repo 空值 fail-closed、空值仅 dir 使用）
- [x] 1.2 `internal/store/queries.go`：`ProjectRow` 增 `Kind` 字段、`TaskRow` 增 `BaseRef` 字段；`CreateProject`/`CreateTask` 签名与全部 SELECT/Scan 点同步
- [x] 1.3 store 层测试：kind 默认值回填、repo/dir 写入读取往返、base_ref 写入读取往返、存量 repo 回填断言

## 2. 项目注册 API 与 DTO（D1/D6/D10）

- [x] 2.1 `internal/api/projects.go` + `server.go` + `store_adapter.go`：注册请求增 `kind`（缺省 repo，非法值 invalid_input/422）；`kind=repo` 走既有 IsGitRepo/ResolveDefaultBranch；`kind=dir` 仅校验路径存在且为目录、default_branch 落空；MUST NOT 隐式推断；项目 DTO 暴露 kind
- [x] 2.2 `internal/api/tasks.go`：task DTO 增 `project_kind`，四个响应消费点（List/Create/Get/RerunInit）均填充——Create 在任务提交前已取得项目 kind（注册/创建链路同源），不得在任务创建后再补查（避免补查失败 500）；**RerunInit 必须在启动 Rerun/claim 副作用前取得项目 kind**（现状先启动再构造 DTO，补查失败会留"脚本已跑但 API 500"的部分成功窗口）；List 复用项目详情避免 N+1
- [x] 2.3 `internal/task` 侧 `ProjectRow`/adapter（`internal/task/adapters.go`）增 `Kind`；所有按 kind 分叉的 switch 显式 repo/dir 两分支，default 在任何副作用前报错（fail-closed）
- [x] 2.4 API 测试：dir 注册成功（非 git 目录）、dir 拒绝不存在/非目录路径、repo 拒绝非仓库且不降级、非法 kind 422、DTO 含 kind/project_kind
- [x] 2.5 合并 main（cross-project-active-sessions）后的兼容性确认：`GET /api/v1/sessions/active` 对 dir 任务返回 branch="" 且前端 `branch || worktree_path` 回退显示项目路径（`activeSessionDTO` 不变更，见 design D6）；`TaskStore` 新增的 `ListActiveTaskOverview` 与本变更新增方法的 mock 合并同步
- [x] 2.6 分支列表只读 API（D10）：`GET /api/v1/projects/{id}/branches` 返回本地+远端分支列表（`git branch --format` / `git branch -r --format`，本机 git CLI 只读、不进仓库写锁）；返回稳定排序、去重后的短名数组（本地在前、远端在后，排除 `origin/HEAD` 等 symbolic HEAD）；dir 项目返回 invalid_input；API 测试覆盖排序/去重/symbolic HEAD 排除/dir 拒绝
- [x] 2.7 远端刷新 API（D10）：`POST /api/v1/projects/{id}/branches/refresh`——逐 remote（`git remote` 枚举）`git fetch --no-tags --no-recurse-submodules --no-write-fetch-head --prune --refmap='+refs/heads/*:refs/remotes/<remote>/*' <remote> '+refs/heads/*:refs/remotes/<remote>/*'`（白名单入 internal/git，`GIT_TERMINAL_PROMPT=0`，30s 硬上限始终派生，进程组终止；`--refmap` 取代 `remote.*.fetch` 配置仅写 refs/remotes/*）；`RepoLock` 升级为 context-aware 获取并迁移 Add/Remove/refresh（Push `-u` 写 .git/config 一并纳入）；fetch 与同锁内 ListBranches 串行；同 repo singleflight 合并、跨 repo 并行（等待者 done channel + select 响应自身取消，等待者取消 MUST NOT 取消领跑者仍需要的 fetch）；fail-closed git_error（失败/超时/取消不返回 200 伪装最新）；dir/未知 kind fail-closed；测试：bare remote 新分支 refresh 后可见、prune 移除已删远端分支、恶意/mirror refspec 不写 refs/heads/*、无 remote 跳过、FETCH_HEAD 不变、既有任务分支/HEAD 不动、30s 硬上限、等待者取消语义（领跑者不被误取消）、单次 fetch 计数、跨 repo 并行重叠、与 Add/Remove 临界区不重叠、进程组终止

## 3. session 归属隔离（D8，dir 并行前置）

- [x] 3.0 ~~前置验证 spike~~（已在设计阶段经 OpenCode 源码验证闭环：事件总线为进程内实例级，同目录双 serve 不串流，v1.16.0 起架构稳定；见 design D8。无实现前置任务）
- [x] 3.1 `internal/store/queries.go` + `internal/task`：task（application）层新增自有类型 `AlignMode`（未知值 fail-closed 返回错误零写入）与 `SessionObservation`（不含 opencode 依赖），StoreAdapter 转换为 store 层类型（沿用 TaskStore/SessionRow 既有解耦惯例）；store 层新增原子 `ClaimTaskSession(ctx, taskID, sessionID, ...) (claimed bool, ownerTaskID string, err error)`（单事务"仅当未被他任务拥有时插入/更新"，不加唯一索引）；新增原子 `AlignTaskSessions(ctx, taskID, mode, listed, complete, noticeFn)`（单事务读 owned → 按 mode claim/刷新 → complete 仅删 owned 缺席行 → noticeFn 事务内读写 notice：complete 清除既有 session_overflow notice）；新增条件 `TouchOwnedTaskSession(ctx, taskID, sessionID, lastSeenAt) (updated bool, err error)`（UPDATE WHERE task_id+session_id，绝不插入；updated=false 为正常路径）；`TaskStore` 接口、StoreAdapter 与全部 mock 同步
- [x] 3.2 `internal/task/activate.go`：SSE session.created、锚定创建、alignSessions 三个归属写入口统一改为 ClaimTaskSession/AlignTaskSessions；冲突语义——SSE/对齐忽略+记诊断，锚定冲突 → 激活失败（MUST NOT attach 他任务 session）；`session.updated` 改经 TouchOwnedTaskSession 仅刷新已归属行（updated=false 记 debug 不报错，store 错误传播），停用 UpsertTaskSession 在该路径的调用；overflow notice 保持现状逐点一致：overflow 时先事务外 CAS 写 notice 再调对齐（对齐失败 notice 保留），complete 时经对齐事务内 noticeFn 清除
- [x] 3.3 kind 传播四入口：`Activate`、persist 恢复路径（reconcilePersist→resumeActive→startSSE）、挂起修复路径（Suspend 分支 c→tryRepairRuntime）、TUI 重开路径（ReopenAttach，attach_shell.go）都在任何状态修改或运行时副作用前解析并校验项目 kind（未知值零副作用报错），显式传入 startSSE/alignSessions 选择对齐模式（AlignModeRepo / AlignModeOwnedOnly）；ReopenAttach 的锚定 claim 冲突 → 返回错误 + 记录 last_error，任务保持 active 不收敛，MUST NOT attach 他任务 session；dir 对齐语义经 AlignTaskSessions 单事务执行
- [x] 3.4 测试：并发 claim 唯一归属；同目录两 dir 任务各自激活/对齐互不认领；删除任务 A 不影响任务 B；complete 仅删 owned 缺席行且同事务清除 session_overflow notice；overflow 不删且事务外 CAS 写 notice（对齐失败 notice 保留，与现状一致）；foreign/unowned session 过滤；session.updated 不创建归属（TouchOwnedTaskSession updated=false 路径）；未知 kind 的 Activate/resumeActive/tryRepairRuntime/ReopenAttach 四入口零副作用；ReopenAttach claim 冲突保持 active + last_error；未知 AlignMode fail-closed；存量重复 owner 的 claim 冲突忽略+诊断；RerunInit 前置项目 kind 查询失败时 ClaimInitRerun 与脚本均未启动（零副作用）
- [x] 3.5 既有 task 测试 fixture 统一回填 `Kind="repo"`（seedProject/helper 默认值），避免新增 Kind 字段后零值 unknown 触发 fail-closed

## 4. 任务创建分叉（D2）

- [x] 4.1 `internal/task/crud.go` `Create` + `internal/api/tasks.go`：API 请求增可选 `base_ref`（经 TaskBackend.Create 传播，前端类型同步）；按 `proj.Kind` 分叉——dir 先做无副作用目录预检（存在且为目录，否则 invalid_state 且不落 creating 行），跳过分支命名/校验/冲突检查/路径生成/wt.Add/inherit 复制，wtPath=canonical 项目路径、Branch=""；仅读 lifecycle 配置决定 init_status；落库 → CommitCreated → InitRunner/自动激活。repo 路径接受可选 `base_ref`：先 `git check-ref-format --branch <短名>` 规范校验，再经 task 层端口（`WorktreeBackend`/adapter 新增解析方法，MUST NOT 在 crud.go 直接依赖 git）按 `refs/heads/`→`refs/remotes/` 顺序 rev-parse 探测（heads 优先，均在落 creating 行前的无副作用阶段），解析后的**全限定 ref** 随任务落库（`tasks.base_ref`，缺省创建落库 `refs/heads/<默认分支>`），`wt.Add` 用解析后 baseRef 替代 `proj.DefaultBranch`；dir 项目提供 base_ref → invalid_input 零副作用；mock 同步
- [x] 4.2 `retryCreate`：dir 跳过 VerifyWorktreeProduct/分支检查/wt.Add，仅校验项目目录仍存在（否则保持 creation_failed 并报错）后重读配置提交；repo 重建 worktree 时使用落库 `base_ref`，MUST NOT 重读项目默认分支
- [x] 4.3 创建测试：dir 创建零文件副作用（init/激活开始前目录树一致）、branch 空、worktree_path=项目路径、init script 仍触发 InitRunner、目录消失拒绝创建/重试语义、counting mock 证明不调用 Namer/WorktreeBackend/inherit copy；repo base_ref 本地/远端生效、同名 heads 优先、非法/不存在 invalid_input 零副作用、缺省创建落库全限定 ref、dir 提供 base_ref 拒绝、Retry 用落库 baseRef（默认分支变更不影响；空值 fail-closed）

## 5. 任务删除分叉（D3，硬不变量）

- [x] 5.1 `internal/task/delete.go`：deleteResume 入口按 kind 一次性分流为 repo/dir 序列（共享步骤抽 helper）；dir 序列跳过 PreflightDelete/DirtyFiles 双门禁/wt.Remove/DeleteBranch，confirmDirty 接受但忽略；dir normal 保留 debt 重试 → oc sessions → kill 残余会话 → pre_delete（cwd=项目目录）→ DB 删除 → 日志目录清理；dir force 与 repo force 契约一致（跳过 ③⑥，不跳过 ②）
- [x] 5.2 `Retry` 删除重入路径：dir 跳过 preflight dirty 快照，按持久化 delete_mode 重入 dir 序列
- [x] 5.3 重构不可回归点保留：非取消落账 ctx、pre-delete WG token 恰好一次释放、notice CAS 失败聚合、oc session 逐项错误聚合不短路、tickets 不随 CASCADE 丢失
- [x] 5.4 删除测试（关键）：预埋文件树，dir normal/force/retry 三路径删除后目录逐字节比对不变（无 pre-delete 配置时）；panic mock 证明 dir 路径不调用 git/WorktreeBackend；断言一次性 serve/进程创建参数不向用户目录写任何文件（temp serve cwd 仅作工作目录）；pre-delete 配置时脚本在 cwd=项目目录执行且失败落 `pre-delete:` 前缀 deletion_failed；oc session 仅删本任务拥有的 session

## 6. reconcile 与 git 降级（D4/D5）

- [x] 6.1 确认 `internal/task/reconcile.go` 的 creating/creation_failed 状态分支对 dir 任务无需 D8（resumeActive kind 传播）之外的特殊处理（creating→creation_failed、creation_failed 保持原状）；补测试钉死该行为
- [x] 6.2 `internal/task/gitops.go`：Status/Diff/Commit/Push 入口解析项目 kind，dir → invalid_input 明确报错，不执行任何 git 命令
- [x] 6.3 测试：reconcile dir 收敛语义；gitops 四入口对 dir 任务报错且零 git 调用

## 7. Web UI（D7）

- [x] 7.1 项目注册表单增类型选择（默认 git 仓库）；dir 类型不请求默认分支
- [x] 7.2 项目列表/详情显示 kind 徽标；dir 任务不显示分支名；任务详情隐藏 git tab（依据 `project_kind`）
- [x] 7.3 dir 项目 ≥2 活跃任务时显示"多任务共享同一目录、无文件隔离"提示条
- [x] 7.4 删除确认弹窗（`web/src/components/DeleteTaskModal.tsx`）按 `project_kind` 分叉：dir 任务不出现"会删除对应 worktree"类文案，明确"不会删除项目目录及其内容"；normal + 已配置 pre-delete script 时提示脚本仍会执行；不展示 dirty/worktree 确认项
- [x] 7.5 repo 项目创建任务表单增基线分支下拉（数据源 `GET /projects/{id}/branches`，默认选中项目默认分支）；dir 项目不显示该控件
- [x] 7.6 基线分支选择器 refresh 交互（D10）：首次打开自动调用 `POST .../branches/refresh` 一次 + 下拉旁显式"刷新"按钮；refresh 期间 loading 且禁止提交所选远端基线；失败保留旧列表并标注"本地快照未刷新"+ 错误与重试入口

## 8. 验证

- [x] 8.1 `go build ./... && go test ./internal/store/... ./internal/task/... ./internal/api/...` 全绿
- [x] 8.2 前端构建与既有 e2e/手测：repo 项目全流程回归（创建/激活/挂起/删除/git 面板/SSE 断流补记）确认零行为变化（前端 tsc+vite build 通过；repo 回归由全量 go test 覆盖；未做人工 UI 手测）
- [x] 8.3 `openspec validate add-plain-dir-project` 通过
