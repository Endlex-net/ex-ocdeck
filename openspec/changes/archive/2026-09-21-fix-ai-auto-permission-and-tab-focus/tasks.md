# Tasks

## 1. 契约与事件 Lane（domain event、共享校验、D2 收敛）

- [x] 1.1 domain/event 新增 `TypeServeRuntimePermissionVerdict` 与 `TypeServeRuntimePermissionModeChanged` 类型 + constructor（payload 复用 `ServeRuntimeTaskPayload{TaskID}`，`event.go:118`；RID=instVersion）；验证：`go build ./internal/domain/event/` 通过，新类型有构造测试
- [x] 1.2 抽取共享三值权限模式校验函数（`crud.go:224-231` 逻辑 → 共用，创建与模式端点同构，含 trim + 未知字段拒绝语义分离说明）；验证：创建路径既有测试不回归，校验函数有表驱动单测（三值通过、空/null/类型错误/非三值拒绝）
- [x] 1.3 D2：`judgeAndReply` 移除 `PermissionVerdictReject → reply="reject"` 分支（`permit_auto.go:297-298`），REJECT 并入 default（不回复、转人工），审计记 `REJECT + not_applicable`；`permAuditVerdict` 映射不变；验证：单测断言 REJECT 判定永不进入回复发送路径（reply 变量无 reject 赋值），审计记录 REJECT 结论保留，`go test ./internal/task/ -run Judge` 通过
- [x] 1.4 `application.PermissionModeView` 定于 `internal/application/dto.go`（字段 `PermissionMode string` / `EffectivePermissionMode string`）；验证：`go build ./internal/application/` 通过

## 2. 持久化与 Runtime Lane（store 原语、epoch 算法、permission_mode_at_start）

- [x] 2.1 SQL migration 新增 `tasks.permission_mode_at_start TEXT NULL` 列（仅加列，不回填）；验证：migration 应用后 `PRAGMA table_info(tasks)` 含新列，存量行值为 NULL，既有测试不回归
- [x] 2.2 store 窄端口：`BackfillPermissionModeAtStart(ctx) error`（仅 active 且 NULL 行按持久化 `permission_mode` 回填，幂等；不推进 `updated_at`、不发事件；DB 错误原样返回）、`SetPermissionModeAtStart(ctx, taskID, mode string) error`（不推进 `updated_at`、不发事件）、`GetPermissionModeAtStart(ctx, taskID string) (*string, error)`（NULL→nil）；归属 `internal/task.TaskStore`（manager.go:24），经 `task.StoreAdapter` 委托 `store.Queries`；不扩展 `TaskRow`；验证：store 单测覆盖回填幂等/仅 NULL 行/未命中/写失败/不推进 updated_at，adapter 编译断言通过
- [x] 2.3 store 原语 `UpdateTaskPermissionMode(ctx, taskID, mode string) (application.MutationResult, error)` 单列 UPDATE（`task_info_queries.go` 同文件；updated_at 按 task-lifecycle Unix 秒精度规则；`Matched=false` 可区分）；验证：单测覆盖成功更新（跨秒推进/同秒不变 Changed 语义）、未命中、DB 错误
- [x] 2.4 `taskRuntime` 增加 `permEpoch`(uint64) 与 `RuntimePermissionState{InstVersion, SavedMode, StartedWithAuto, AIAutoEnabled}`（rt.mu 同步域，可空；AI 启用唯一事实源为 `AIAutoEnabled`，无独立布尔）；三条创建/恢复路径共用 D5 统一初始化表公式；验证：编译通过，三路径初始化一致性单测（ask→ai-auto、ai-auto→ask/all-approve 重启恢复、--auto 进程保存 ai-auto 重启仍不启用 AI）
- [x] 2.5 `startRuntimeWithPortRetry`（activate.go:1132 `NewSession` 前）写入 `permission_mode_at_start`（本次 argv 实际模式值；写失败不建进程）；`attach_shell.go:144` 与 `delete.go:599` 的 `NewSession` 不写；验证：单测覆盖写入成功/写失败不建进程/shell 与 temp serve 负向不写
- [x] 2.6 `resumeActive` 与 `tryRepairRuntime`（suspend.go:323）注册 runtime 前读取校验该列：有存活进程但列 NULL 或非法 MUST 失败（不注册；非法值经 reconcile 报错拒绝开放 HTTP，同 reconcile.go:408 语义）；非 active 任务 NULL 合法；验证：单测覆盖 NULL 拒绝注册/非法值 fail-closed/合法值恢复 `StartedWithAuto`
- [x] 2.7 `judgeScan` 入口捕获 `(rt, instVersion, permEpoch)` 三元组；`judgeScanAsync` 模式判定预检一次 DB 读、此后 mode 零 DB 重读（判定输入组装与回复客户端构造为异步锁外非 mode 读）；`judgeAndReply`/`permGate` 复核条件含 epoch 一致 && `AIAutoEnabled`（复核门禁纯内存）；judged-set 契约 per-runtime/per-epoch/per-request；判定状态记录携带 epoch（登记 inflight 写当前 epoch，终态提交比较捕获 epoch，失配不写状态不发事件）；验证：epoch 屏障单测（切出后在途判定不回复、同值不开 epoch、切出再切入+旧 epoch 迟到不写状态不发事件、gate 读取 `AIAutoEnabled` 结论），`go test -race ./internal/task/` 通过
- [x] 2.8 有效模式推导单一函数（输入为 runtime 的 `RuntimePermissionState` 原子读取结果或 nil（无 runtime 时读 DB 行），输出 `application.PermissionModeView`，D5/D6）+ `Manager.PermissionModeView(ctx, taskID) (application.PermissionModeView, error)`（供详情 GET/详情 SSE/PATCH 响应/通知快照复用，禁止各自实现）；验证：推导函数表驱动单测覆盖 D6 全部分支（含 DR1 --auto 进程恒 all-approve），模式切换后 gate 与 `PermissionModeView` 读取同一 `AIAutoEnabled` 结论的断言

## 3. Notifier Lane（D1 waiting 状态机）

- [x] 3.1 `TaskNotificationSnapshot`/`TaskRef` 扩展：`EffectivePermissionMode`（D6 推导值，非原始持久化值）+ per-request 判定状态映射（requestID → `{epoch, state}`，三值枚举 `inflight/manual_required/settled_no_notify`，仅当前 epoch，与 attention 同代际原子读出）；task 层组合快照输出（复用 2.8 推导函数）；验证：snapshot 单测覆盖映射生成、旧 epoch 残余不输出、快照 EffectivePermissionMode 与 `PermissionModeView` 同源一致
- [x] 3.2 task 层 `judgeAndReply` 各终结分支按 D1 分支表提交判定状态（rt.mu 内先提交后发布），随后发布 `serve_runtime.permission_verdict` 唤醒事件（Topic=serve_runtime、RID=instVersion、Payload=ServeRuntimeTaskPayload{TaskID}）；验证：分支表全枚举单测（六分支映射）、状态提交先于事件发布的时序断言
- [x] 3.3 通知层 waiting 状态机：ai-auto（有效模式）首次观察不写 `notifiedPermissions`、登记 waiting（requestID → deadline=首次观察+15s）；快照 `manual_required` 或（`inflight`/无状态且 deadline 到期且仍 pending）→ 走既有 evaluate/dispatch 并写去重；`settled_no_notify` 或请求消失 → 清 waiting；triggers.go:60 先记去重语义拆分（ai-auto 走 waiting，非 ai-auto 保持）；验证：fake-clock 单测覆盖 15s 与近 25s 边界、手动放行不通知、转人工立即通知（不等 deadline）、pending 消失清理
- [x] 3.4 overflow 对账矩阵：仍 pending 已通知条目保留去重；waiting 条目保留并按对账快照定论（有效模式非 ai-auto → 对账成功退出 reconciling 后立即按正常门禁投递；仍 ai-auto → 保留原 deadline 按判定状态重评）；既未通知也无 waiting 的 ai-auto pending MUST 新建 waiting（deadline 从对账观察时刻起算）；进程重启 waiting 不恢复、按启动基线播种不补发；验证：对账单测覆盖全部矩阵行（含 mode-changed 事件 overflow 丢失后既有 waiting 立即投递）
- [x] 3.5 通知层订阅 `serve_runtime.permission_verdict` 与 `serve_runtime.permission_mode_changed`（事件仅唤醒、重读快照定论，不信任载荷）；发送前重读快照复核（同 runtime、仍 pending、未通知、非 settled_no_notify）；验证：事件驱动重评估单测、复核否决不发送单测

## 4. API Lane（模式端点、详情透出、读模型豁免）

- [x] 4.1 `PATCH /api/v1/tasks/{id}/permission-mode` 路由 + handler：body `{"permission_mode": "<三值>"}`（>4KiB/空体/缺失/null/类型错误/尾随 JSON/未知字段/非三值 → 422 `invalid_input` 零副作用；`errors.Is(err, sql.ErrNoRows)` 或 UPDATE 未命中 → 404；非 ErrNoRows 读错误 → 500 不误包 404；per-task 互斥竞争 → 409；DB 提交失败 → 500 零 runtime 副作用）；响应固定 `{"permission_mode", "effective_permission_mode"}`；`TaskBackend` 接口增 `UpdateTaskPermissionMode` 与 `PermissionModeView`（逐字返回 `application.PermissionModeView`），**接口扩展同时同步 `fakeTaskBackend` 两方法**（否则 API 测试不可编译）；验证：handler 表驱动测试覆盖错误矩阵全行 + 同值 200 零副作用，`go test ./internal/api/` 通过
- [x] 4.2 `Manager.UpdateTaskPermissionMode`（唯一协调器，D4 严格序列：校验 → per-task 互斥 → 读行（损坏 500 fail-closed）→ 同值 200 零副作用 → DB commit（PONR）→ rt.mu 收敛（仅有效切换动 epoch/补判，锁内更新、释放后调 judgeScan）→ `task.activity_changed` → 有 runtime 时 `serve_runtime.permission_mode_changed` → 200；提交后收敛在 Manager 生命周期 ctx 执行）；验证：协调器单测覆盖序列顺序、转换矩阵全行（DR1 --auto 切 ai-auto 不动 epoch、切入补判、切出 epoch+1、无 runtime 仅持久化）、无 runtime 只发 activity_changed、**PONR 后请求取消不阻断内存收敛与事件发布**
- [x] 4.3 任务详情 GET / 详情 SSE 透出 `effective_permission_mode`（经 `PermissionModeView`，输出归一化复用 `permissionModeForOutput`）；MUST NOT 加入创建响应、项目任务摘要、active 概览；验证：详情响应含两字段且一致、摘要/概览/创建响应不含该字段的负向断言
- [x] 4.4 读模型流过滤表显式豁免两个新事件类型（`task_filter.go:82`、`sessions_filter.go:82`、`projects_filter.go:29`），畸形事件仍保守标脏；验证：单测断言两新事件不标脏任何读模型流（unrelated-task 不推帧）、畸形事件标脏
- [x] 4.5 组合根 main.go 启动顺序：SQL add column → `Manager.BackfillPermissionModeAtStart` → `Reconcile` → HTTP open（回填 DB 错误传播阻止 HTTP 开放）；无新增依赖注入；验证：启动顺序测试（回填失败拒绝开放 HTTP、回填先于 reconcile）
- [x] 4.6 mock/fake 同步：store adapter 编译断言、notification snapshot mocks 扩展（`TaskBackend` fake 已在 4.1 同步）；验证：`go build ./...` 与 `go vet ./...` 通过

## 5. Web Lane（tab 聚焦 D3、模式选择器 D4/D6）

- [x] 5.1 tab-local 聚焦 intent：`TaskWorkbenchPage` tabstrip onClick 处对「终端」/「shell N」真实点击生成 `{seq, ts, target}`（target='tui' 或 shell tab 稳定 id——与 `active={tab===id}` 同一键，非显示序号/数组索引）；Git/设置 tab 与程序性 `switchTab` 不生成 intent；intent 作为 prop 传给对应 TerminalView；验证：jsdom 测试覆盖点击生成 intent、程序性切换不生成、Git/设置不生成
- [x] 5.2 `TerminalView` 消费 tab-local intent：仅 intent.target 匹配自身标识时消费；复用 `tryConsumeFocusRequest` 门禁内核（connected + 未锁定 + 未过期 5s + 白名单）；仅 `active && connected` 时消费；active 变 false/卸载/过期/用户焦点已进入输入区 → 取消；目标销毁不误投递其他 shell；tab-local 白名单仅允许 body 或 tabstrip 点击目标；shell 消费导航请求禁令不变；验证：jsdom 测试覆盖点击/重复点击（新 seq 仍聚焦）/锁定消费不聚焦/不抢占/切走取消/未就绪等待/销毁不误投，既有导航聚焦四场景零回归（`navigation-focus.test.tsx` 全绿）
- [x] 5.3 `types.ts`/`api.ts`：`effective_permission_mode` 仅存在于 `TaskDetail`（详情 GET/详情 SSE/模式端点响应，强制字段），通用 `Task` 不含；模式端点 client 方法；通用任务信息 PATCH 回写页面任务时合并保留当前 effective 值（不整体替换）；模式端点响应仅合并两个模式字段；验证：类型编译通过，合并保留单测（模式不一致后保存任务名称、SSE 不推帧时提示仍存在）
- [x] 5.4 `TaskInfoCard` 权限模式选择器（与名称/分支一致的查看态/编辑态：查看态只读、编辑态草稿随统一保存走专用端点）：保存期间禁用、禁止重叠请求；成功以响应收敛；网络失败/结果不确定时 GET 复验后解禁；旧响应 MUST NOT 覆盖更新选择；两值不一致时展示「当前运行进程仍按「X」处理，新模式将在下次激活生效」+ 保存后瞬时确认；验证：jsdom 测试覆盖查看/编辑态切换、保存竞态（禁用/重叠/旧响应覆盖/失败复验）、不一致提示与瞬时确认展示
- [x] 5.5 前端全量验证：`cd web && npm test` 与 `npm run build`（或项目既有等价命令）通过

## 6. 全量验证与收尾

- [x] 6.1 `go vet ./...` 与 `go build ./...` 通过（仓库无既有 golangci-lint 入口，`go vet` 为必过验证）
- [x] 6.2 `go test -race ./...` 全量通过（含 D9 测试矩阵：fake clock 边界、overflow 矩阵、epoch 屏障、初始化表三路径、事件时序、重启恢复、store/API 错误表、读模型过滤、判定状态分支表全枚举）
- [x] 6.3 `openspec validate "fix-ai-auto-permission-and-tab-focus" --strict` 通过，tasks 勾选状态与实现一致
