## Context

本设计覆盖 proposal 的四项变更（见 proposal.md - Why / What Changes）。塑造方案的现状态约束（均已对照代码核实）：

- **聚焦机制**：全局导航聚焦协议在 `web/src/terminal/focus-request.ts`（模块级单例，`requestTerminalFocus(taskID)` / `subscribeTerminalFocus` / TTL 5s），消费点 `web/src/terminal/TerminalView.tsx` `tryConsumeFocusRequest`（:118-133，门禁：connected + taskID 匹配 + 未锁定 + 未过期 + `isFocusRequestTargetAllowed`）；`tuiTaskIDFromWsPath`（:67-72）对 shell 路径返回 null（头注释明确 shell MUST NOT 消费导航请求）。`switchTab`（`web/src/pages/TaskWorkbenchPage.tsx:189-192`）仅 setState。TUI TerminalView 常驻挂载（隐藏不断连），`active` 变 true 仅触发 `session.connect()`（:341-346）。
- **通知层**：`internal/application/notification/triggers.go` `onAttentionChanged`（:21-68）在 pending 出现即触发（`:60` 先写 `notifiedPermissions` 去重再门禁投递）；`TaskSnapshot`/`TaskRef`（`ports.go:33-59`）无权限模式；通知层有周期 tick（`notifier.go:39` `TickEvery` 默认 10s，:148 ticker）；事件经 domain event bus（`internal/domain/event/event.go:58-60` `TypeServeRuntimeAttentionChanged`），AI 判定层（`internal/task/permit_auto.go`）与通知层无交互。overflow 对账按当前 pending 全量播种去重（`notifier.go:411-418`）。
- **ai-auto 判定**：`judgeScanAsync`（`permit_auto.go:77-146`）每次扫描实时读 DB 判 mode（:86-93）；`judgeAndReply`（:264-346）switch 中 `PermissionVerdictReject → reply="reject"`（:297-298）；`permGate`（:152-162）为纯内存门禁（不读 DB、不检查 mode）；`judgedPerms` per-runtime 去重（`manager.go:399-401`）。
- **模式持久化**：`permission_mode` 唯一写路径为 INSERT（`internal/infrastructure/store/queries.go:355-362`）；PATCH `/api/v1/tasks/{id}`（`internal/api/tasks.go:744-789`）仅支持 name/branch_slug；`application.TaskInfoUpdate`（`internal/application/ports.go:123-127`）与 `CommitTaskInfoUpdate`（`task_info_queries.go:39-90`）均无 permission_mode 列；分支改名故障恢复载荷 `renamePendingIntent`（`internal/task/task_info.go:32-36`）。`all-approve` 的 `--auto` 在激活时烧录 argv（`activate.go:1062-1069, 1089-1091`），运行中不可热改。server 重启恢复 runtime 时仅从 `env_snapshot` 还原 password/port/taskID 等会话 env（`internal/task/reconcile.go:370, :401`），无模式启动态记录。
- **继承契约**：ai-auto 判定输入的字段级提取矩阵、界值与截断语义的唯一 owner 是 archived change 2026-09-10-task-permission-mode 的 design D5 提取表（`openspec/changes/archive/2026-09-10-task-permission-mode/design.md`）。本变更不修改该契约，仅在 spec 中显式引用（避免与本设计 D5 编号歧义），不在本文档复制（防双源漂移）。

## Goals / Non-Goals

**Goals:**

- 详情页终端/shell tab 点击聚焦（含重复点击、未就绪等待、锁定/抢占门禁），不破坏既有导航聚焦协议。
- ai-auto 权限通知延迟触发：自动放行不通知、转人工立即通知、超时兜底通知；非 ai-auto 语义零变化。
- ai-auto 仅自动放行，REJECT 转人工，审计契约同步收敛。
- 权限模式创建后三档互转：有效 ai-auto 行为即时切换（切入补判、切出在途不回复）、all-approve 下次激活生效、运行态收敛安全。

**Non-Goals:**

- 不调整 LLM prompt / 判定能力本身（REJECT verdict 仍由模型输出并留痕，仅不再自动执行）。
- 不引入通知撤回机制（已发通知不撤销，靠延迟触发避免误发）。
- 不支持运行中进程的 all-approve 热切（用户已确认下次激活生效；DR1 进一步确认 `--auto` 运行进程切入 ai-auto 也不启用 AI，下次激活生效）。
- 不改变 question / idle / retry / error 类别通知语义。
- 不修改判定输入提取矩阵（继承 archived D5 提取表，见 Context）。

## Decisions

### D1：ai-auto 通知延迟触发——事件唤醒 + 快照定论 + tick 兜底

**决策**：task 层在 per-request 判定终结时**先提交判定状态（rt.mu 同步域）、后发布**轻量唤醒事件——事件契约钉死：`Topic=serve_runtime`、`Type=serve_runtime.permission_verdict`、`RID=instVersion`、`Payload=ServeRuntimeTaskPayload{TaskID}`（复用既有 payload 类型，`internal/domain/event/event.go:118`，**不含 requestID**；通知层按 task 重读权威快照并重评该任务全部 waiting 条目，不信任事件载荷）。快照扩展：`TaskRef` 增加 `EffectivePermissionMode`（**有效模式**，按 D6 推导，NOT 原始持久化值——`--auto` 运行进程保存 ai-auto 时有效模式仍为 all-approve，通知分类不得误入延迟语义，DR1）；`TaskSnapshot` 增加 per-request 判定状态映射（requestID → `{epoch, state}`，state 为下表三值之一，由 task 层 runtime 状态经 `TaskNotificationSnapshot` 组合原子读出，与 attention 同一代际；**快照仅输出当前 epoch 的状态，旧 epoch 残余状态 MUST NOT 输出**）。

**判定状态闭合枚举与分支表**（task 层 `judgeAndReply` 各终结分支的唯一映射）：

| 判定/回复终态 | 判定状态 | 通知行为 |
|---|---|---|
| APPROVE + 回复 ok | `settled_no_notify` | 不通知（即使 attention 瞬时仍 pending） |
| APPROVE + gone（请求已被了结的竞态） | `settled_no_notify` | 不通知 |
| REJECT / UNCERTAIN / FAILED | `manual_required` | 唤醒事件到达即通知 |
| APPROVE + unknown（回复结果未知） | `manual_required` | 通知（回复可能未受理，请求或仍待处理，按安全方向转人工） |
| APPROVE + unsupported / 发送前门禁失败未发送 | `manual_required` | 通知 |
| 判定进行中（已登记未终结） | `inflight` | 等待；deadline 到期仍 pending 则兜底通知 |

ai-auto（有效模式）任务的 pending permission 首次观察时不写 `notifiedPermissions`，改为登记 waiting 条目（requestID → 首次观察时间 deadline）；快照显示 `manual_required` 或（`inflight`/无状态且 deadline 到期且请求仍 pending）→ 走既有 evaluate/dispatch 通知并写去重；`settled_no_notify` 或请求消失 → 清 waiting 不通知。deadline 仅作用于未定论（`inflight`/无状态）条目；`manual_required` 不等 deadline（事件唤醒即通知）；`settled_no_notify` 永不通知。waiting/去重条目随 pending 消失剪枝（沿用上界约束）。

**超时参数**：deadline = 首次观察 + 15s；tick 周期 10s（既有 `TickEvery` 默认）。算术自检：deadline 到期后首个 tick 触发，deadline 时刻相对 tick 相位任意 → 实际触发区间为 **[15s, 25s)**（deadline 恰在 tick 后 ε 到期时等待近一个完整 tick 周期）。LLM 正常判定（秒级）由唤醒事件立即通知，不等 deadline。fake-clock 测试 MUST 覆盖 15s 与接近 25s 两边边界。

**备选**：(a) 纯事件驱动——event bus 可溢出/乱序，无权威对账来源，否决；(b) 纯固定窗口重读——实现最简但拒绝/失败也要等满窗口（转人工慢 15s+），否决；(c) task 层直接调通知渠道——侵入通知层职责且已发通知不可撤回，否决。

**不变量**：有效模式非 ai-auto 的请求首次观察即通知（现状不变）；deadline 以首次有效观察为准，重复事件/快照不延长；判定状态提交先于 verdict 事件发布（通知层任何时刻重读快照都能读到已提交状态）；**判定状态记录携带 epoch：登记 `inflight` 时写入当前 epoch；终态（`manual_required`/`settled_no_notify`）提交 MUST 比较捕获 epoch，失配 MUST NOT 写状态、MUST NOT 发 verdict 事件（旧 epoch 判定迟到不得污染新 epoch）**；发送前重读快照复核（同 runtime、仍 pending、未通知、非 `settled_no_notify`）；同一 runtime 同一 request 最多通知一次；pending 消失清理 waiting/去重状态；**overflow 对账矩阵**：仍 pending 的已通知条目保留去重；同 runtime/request 的 waiting 条目保留——按对账快照定论：当前有效模式非 ai-auto（如切出事件被 overflow 丢失）时，MUST 在完整对账成功并退出 reconciling 后立即按正常门禁投递（请求仍 pending 且未通知）；当前有效模式仍为 ai-auto 时保留原 deadline，并按快照判定状态立即重评（`manual_required` 即通知、`settled_no_notify` 清除、未定论等 deadline）；有效 ai-auto 的 pending 中既未通知也无 waiting 的 ID（attention/verdict 事件在 waiting 建立前丢失）MUST 新建 waiting、deadline 从本次对账观察时刻起算（否则兜底永远失效，方向评审 F4）；仅「此前无 waiting」的普通非 ai-auto pending 与 question 仍按既有语义仅播种去重不补发；**进程重启后 waiting 状态不恢复——已有 pending 按既有启动基线语义播种去重、不补发（见 task-notifications spec「通知抑制、启动基线与对账」），超时保证限当前进程生命周期，不新增持久化通知状态**。

### D2：ai-auto 仅放行自动回复（judgeAndReply 收敛）

**决策**：`judgeAndReply` 的 verdict switch（`permit_auto.go:293-303`）收敛为仅 `PermissionVerdictApprove → reply="once"`；`PermissionVerdictReject` 并入 default 分支（不回复、转人工），审计记 `REJECT + not_applicable`。`permAuditVerdict`（:165-174）映射不变（REJECT 仍如实记录）。审计契约同步：新记录 `reply` 字段仅可能出现 `once`。

**备选**：(a) 移除模型侧 REJECT 输出（改 prompt 为二值）——缩小 AI 表达力、丢失「AI 认为危险」的审计信号，且属 non-goal（不改 prompt），否决；(b) REJECT 延迟 N 秒后自动拒绝——与「拒绝等待用户决策」需求冲突，否决。

**不变量**：LLM 解析协议、证据降级、三道 permGate 复核、「回复结果未知不重试」边界全部不变；REJECT 判定永不进入回复发送路径（`reply` 变量无 `reject` 赋值分支）。

### D3：详情页 tab 聚焦——tab-local 聚焦 intent，与全局导航协议分离

**决策**：保留 `focus-request.ts` 为「跨导航到目标任务 TUI」专用协议（shell 消费禁令不变）。`TaskWorkbenchPage` 对「终端」/「shell N」tab 的真实点击生成 tab-local intent `{seq, ts, target}`（单调递增 seq，新 intent 覆盖旧 intent），作为 prop 传给对应 `TerminalView`；Git/设置 tab 与程序性 `switchTab` 不生成 intent（用户点击与程序性切换的分流在 tabstrip onClick 处完成，switchTab 本身不感知 intent）。**target 目标键闭合定义：`target = 'tui'`（终端 tab）或 `target = <shell tab 稳定 id>`（与 tabstrip `active={tab===id}` 判定同一键，即 shell tab 的 session/tab id——非显示序号、非数组索引，shell 增删/排序不影响身份）；点击 onClick 处即以该键生成 intent；`TerminalView` 仅当 intent.target 与自身 tab 标识匹配时消费；目标 tab 销毁/对应 TerminalView 卸载时未消费 intent 随组件生命周期取消，MUST NOT 误投递到其他 shell**。`TerminalView` 复用既有 `tryConsumeFocusRequest` 的门禁内核（connected + 未锁定 + 未过期 + 白名单），但 intent 生命周期独立：仅在 `active && connected` 时消费；`active` 变 false、卸载、过期（沿用 5s TTL）、或用户焦点已进入输入区 → 取消。tab-local 白名单仅允许 `body` 或 tabstrip 点击目标，不放宽全局导航白名单（`isFocusRequestTargetAllowed` 既有规则）。

**备选**：(a) 泛化全局 focus-request 支持 shell——混合跨页导航与同页 tab 语义，shell 可能误消费 TUI 导航请求，否决；(b) `active` 变化直接 focus——重复点击当前 tab 不触发（active 无变化）、程序性切 tab 抢焦点、未连接时丢请求，否决。

**不变量**：重复点击已激活终端 tab 生成新 seq 必须聚焦；锁定时消费但不聚焦、不等待解锁补抢；shell 永不消费导航请求；既有导航聚焦四场景（含不抢占、锁定）零回归。

### D4：权限模式变更——独立子资源端点 + 严格收敛序列

**决策**：权限模式修改为独立应用命令与专用子资源端点 `PATCH /api/v1/tasks/{id}/permission-mode`，**不进入** 既有 PATCH 任务信息通道与分支改名 saga（`renamePendingIntent` 不携带模式，saga 语义零变化）。`task.Manager.UpdateTaskPermissionMode` 为唯一协调器（持 per-task 互斥与 runtime 句柄），严格序列：

```
校验请求值（共享三值校验）→ 取 per-task 互斥 → 读任务行（不存在→404；
持久化值损坏→500 fail-closed）→ 同值→200（零副作用）→
DB 单列 UPDATE 提交（point-of-no-return）→ rt.mu 下内存收敛
（D5：有效行为切换才动 epoch；不可失败，随 Manager 生命周期 ctx）→
发布 task.activity_changed（真实变更统一发布，驱动各读模型刷新已保存模式）→
存在 runtime 时再发布 serve_runtime.permission_mode_changed 唤醒事件（D7，非阻塞）→ 200 响应
```

**失败矩阵**：请求体 >4KiB / 空体 / 缺失 / null / 类型错误 / 尾随 JSON / 未知字段 / trim 后非三值 → 422 `invalid_input` 零副作用（未知字段拒绝，与既有通用 PATCH 的忽略语义不同，为本端点显式收紧）；任务行读取 `errors.Is(err, sql.ErrNoRows)`（含预读成功但 UPDATE 未命中 `Matched=false`——行在窗口期被删）→ 404 `not_found` 零副作用；其他读错误/取消/基础设施异常 → 500 `internal` 零副作用（MUST NOT 把非 ErrNoRows 读错误包装成 404）；per-task 互斥竞争（与激活/挂起/改名等生命周期操作冲突）→ 409 `conflict`；持久化旧值损坏 → 500 `internal`（fail-closed）；DB 提交失败 → 500 且零 runtime 副作用（不触发补判、不改 epoch、不发事件）。**DB 提交为 point-of-no-return**：提交后请求取消/中断 MUST NOT 阻断后续内存收敛与事件发布（收敛在 Manager 生命周期 ctx 下执行，不随请求 ctx 取消）；进程崩溃时重启后从 DB 重建（有效模式推导不依赖本次调用的内存残余）。真实变更 MUST 推进 `updated_at`（按 task-lifecycle 的 Unix 秒精度规则：跨秒推进；同秒真实变更仍提交但数值不变，`task.activity_changed` 仍按 `Changed=true` 发布）；同值保存不推进、零副作用。

**外部契约**：body `{"permission_mode": "<三值>"}`（必填，合法值 trim）；响应固定 `{"permission_mode": "<已保存值>", "effective_permission_mode": "<D6 推导值>"}`；同值也返回 200。`effective_permission_mode` 仅出现在任务详情 GET、任务详情流（SSE）与本端点响应，MUST NOT 加入创建响应、项目任务摘要、active 概览。前端：设置 tab 权限模式与名称/分支一致采用**查看态/编辑态**——查看态只读展示当前已保存模式（无常驻选择器）；进入编辑态后出现模式选择器，随编辑态「保存」提交，「取消」还原未保存选择；模式变更走独立端点 `PATCH /tasks/{id}/permission-mode`（MUST NOT 走通用 PATCH；同次编辑中名称/分支变更仍走通用 PATCH，两路径各自保留既有错误/不确定门禁）；保存期间禁用、禁止重叠请求；成功以响应收敛；网络失败/结果不确定时 GET 复验后解禁；旧响应 MUST NOT 覆盖更新选择。

**备选**：(a) 扩展通用 PATCH 通道并将模式塞入 `renamePendingIntent` 原子提交——模式与分支改名 saga 耦合，R1 故障恢复 replay 会绕过 runtime 收敛点，留下「DB 已变、runtime 未切换」状态（方向评审 F3），否决；(b) 每次发送前重读 DB 判 mode——热路径 DB IO + TOCTOU，否决。

**不变量**：校验失败/同值保存/DB 提交失败 → 零 runtime 副作用；接口返回成功前 runtime 模式状态已收敛且切入补判已调度；持久化、runtime 收敛、事件发布的唯一入口是该用例（无第二路径）。

### D5：运行态收敛——epoch 算法 + 转换矩阵 + 启动模式持久化

**算法（钉死，实现者无需自由发挥）**：`taskRuntime` 增加 `permEpoch`（uint64）与原子模式状态 `RuntimePermissionState{InstVersion, SavedMode, StartedWithAuto, AIAutoEnabled}`（可空——无 runtime 不存在），均在 rt.mu 同步域；**AI 启用事实的唯一状态源是 `RuntimePermissionState.AIAutoEnabled`，MUST NOT 另设独立布尔字段（双源会在锁下仍语义漂移）——permGate、effective 推导、通知快照统一读取该字段**；**有 runtime 时模式对（SavedMode + 有效行为）的更新与读取 MUST 在同一 rt.mu 临界区完成（消除 DB 提交与 runtime 收敛窗口期「新持久化值 + 旧 AIAutoEnabled」混合读），无 runtime 时才读 DB 持久化值**。DB 提交成功后由 `UpdateTaskPermissionMode` 在 rt.mu 下同步更新——**仅真实「有效 ai-auto 行为」切换才变更**：切入有效 ai-auto → `AIAutoEnabled=true`、`permEpoch+1`、清空 `judgedPerms`、记录 `needScan`；切出有效 ai-auto → `AIAutoEnabled=false`、`permEpoch+1`（使在途判定的 epoch 捕获失效）。**锁边界（防自锁，judgeScan 自身会获取 rt.mu，`permit_auto.go:53-64`）：状态更新在 rt.mu 临界区内完成；释放 rt.mu 之后才调用 `judgeScan(rt)`（不等下一条 attention 事件，也不等待 LLM 判定完成——接口成功仅要求补判已调度）**。`judgeScan` 入口捕获 `(rt, instVersion, permEpoch)` 三元组；`judgeScanAsync` 的模式判定预检只做一次 DB 读（任务 active + 持久化模式为 ai-auto，:86-93 同构），**模式判定路径此后零 DB 重读（消除 mode TOCTOU）**；判定输入组装（`GetProject` 项目语境，继承 archived 2026-09-10-task-permission-mode D12/8.2）与回复客户端构造（`taskOcClient`，任务行/会话 env 读）为**既有异步非 mode 读、锁外执行**，发送前由两道 permGate 把关、失败安全转人工——不属于模式判定路径，不重新引入 mode TOCTOU；`judgeAndReply`/`permGate` 的**复核**全部纯内存：当前 runtime 一致 && instVersion 一致 && epoch 一致 && `AIAutoEnabled` && ready && !stopping && !unsupported（既有三道复核同构，复核门禁零 DB IO）。judged-set 计数契约变为「per-runtime、per-epoch、per-request 一次」；同值保存不开新 epoch。

**模式转换矩阵**（DR1，用户已决）：`startedWithAuto=true` 的运行进程保存为 `ai-auto` 时**不启用 AI、不补判、不动 epoch**——当前 runtime 继续按 all-approve 运行，下次激活才按 ai-auto 启动；此期间 `effective_permission_mode` 与通知分类均为 `all-approve`。仅 `startedWithAuto=false`（或无 runtime）且有效行为真实切入 ai-auto 时才增 epoch 补判。对称地，`startedWithAuto=true` 切出 ai-auto 类操作本就不可能处于有效 ai-auto（有效恒为 all-approve），无需处理。

| 当前启动态 | 保存值变化 | epoch/补判 | 有效模式（D6） |
|---|---|---|---|
| startedWithAuto=true | → ask / ai-auto | 不动 | all-approve（直至下次激活） |
| startedWithAuto=false | → all-approve | 若原有效 ai-auto：切出（epoch+1）；否则不动 | ask（直至下次激活） |
| startedWithAuto=false | ask ↔ ai-auto | 有效切换：epoch+1，切入补判 | 新保存值（即时） |
| 无 runtime | 任意 | 不动（下次激活生效） | 新保存值 |

**启动模式持久化**：`startedWithAuto` 的唯一来源是 `tasks` 表新增 nullable 列 `permission_mode_at_start`（三值或 NULL，独立 SQL migration）。**写入/恢复三路径矩阵**：① 仅 `startRuntimeWithPortRetry` 创建受管主 runtime（`activate.go:1132` `NewSession`，含端口重试多次调用）**之前**写入本次 `runtimeCmdArgv` 实际使用的模式值——写失败 MUST NOT 创建进程（先写列后建进程，杜绝进程存活而列缺失）；**shell（`attach_shell.go:144`）与删除期临时 serve（`delete.go:599`）的 `NewSession` MUST NOT 写入该列**（否则 all-approve 启动态可能被新保存值改写，重启后违反 DR1）；② `resumeActive` 与挂起修复 `tryRepairRuntime`（`suspend.go:323`，不新建进程但重建 runtime）注册 runtime 前 MUST 读取并校验该列——任务有存活进程时列 NULL 或非法 MUST 失败（不注册、不猜测，避免错误启用 AI；非法值传播语义同 F14：reconcile 报错拒绝开放 HTTP）；非 active 任务的 NULL 合法（无进程可误表）；③ 普通模式修改 MUST NOT 改写该列。server 重启 resume 路径（`reconcile.go:370, :401`）从该列恢复 `startedWithAuto`（仅在有 runtime 时参与有效模式推导；无 runtime 时以持久化模式为准，列值可留存不清理）。存量行该列为 NULL：**迁移分层——SQL migration 只新增列（SQL 无法读取现存进程 argv）；active 行回填由应用启动阶段在 reconcile 之前、HTTP 开放之前执行**。回填事实来源确定：升级前权限模式创建后不可修改（既有 spec 约束），故任何现存进程的启动模式恒等于当前持久化 `permission_mode`——回填按该值直接写入（非"推断 argv"），仅处理 NULL 行、幂等；回填执行失败（DB 错误）MUST 记错误日志并拒绝开放 HTTP（fail-closed，与 reconcile 语义一致），MUST NOT 跳过回填直接开放（否则 active 任务 NULL 在 resume 校验时失败，语义劣化）。非 active 行保持 NULL。列值为三值之外的非法值时 MUST fail-closed：reconcile 返回错误并拒绝开放 HTTP（与既有坏 env snapshot 失败语义一致，`reconcile.go:408`），MUST NOT 猜测降级（按无 `--auto` 恢复可能错误启用 AI，违反 DR1 单引擎语义）。选 SQL 列而非 env_snapshot JSON 字段的原因：既有活动任务改名路径会反序列化后重编码 env_snapshot（`task_info.go:173`），未知顶层 JSON 字段会被丢弃，回滚旧版本后启动事实丢失；独立列对旧版本透明（旧代码按列名 SELECT/UPDATE，自然保留未知列），回滚安全（回滚操作约束见 Migration Plan）。

**`RuntimePermissionState` 统一初始化表**（三条 runtime 创建/恢复路径——startRuntimeWithPortRetry / resumeActive / tryRepairRuntime——共用同一公式，禁止各路径各自推导）：`SavedMode=当前 DB 持久化值`；`StartedWithAuto=(permission_mode_at_start == "all-approve")`；`AIAutoEnabled = !StartedWithAuto && SavedMode == "ai-auto"`；`InstVersion=当前 runtime 实例令牌`；`permEpoch` 从 0 起（每次真实有效切换 +1）。

**备选**：(a) 每次发送前重读 DB 判 mode——热路径 DB IO + 读后发送前 TOCTOU 窗口，否决；(b) 改模式即重启 runtime——违反 all-approve「下次激活生效」且中断运行任务，否决。

**不变量**：切出有效 ai-auto 后未越过最终发送门禁的旧 epoch 判定不得回复；已发出回复不可撤回（沿用「结果未知」边界）；all-approve 运行中行为只由启动 argv 决定；同值保存不开新 epoch；模式变更不清审计；`permGate` 复核与模式判定路径零 DB IO（判定输入组装与回复客户端构造的既有异步非 mode 读除外，见上算法段）。

### D6：effective_permission_mode 透出与 UI 提示

**决策**：任务详情 DTO 增加 `effective_permission_mode`，按**当前实际权限行为**推导（ask↔ai-auto 即时生效，不能取启动时模式）：无 runtime → DB 持久化模式；有 runtime → 同一 rt.mu 临界区原子读取 `RuntimePermissionState`（D5）后推导——`StartedWithAuto` → `all-approve`（argv 不可热改，运行中恒为自动批准，DR1）；未带 `--auto` 且 `SavedMode` 为 `all-approve` → `ask`（进程原生行为，平台不判定）；未带 `--auto` 且 `SavedMode` 为 `ask`/`ai-auto` → `SavedMode`。输出归一化复用 `permissionModeForOutput`（`tasks.go:178-188`，未知值 fail-closed）。透出范围见 D4 外部契约（仅详情 GET、详情 SSE、模式修改端点响应）。通知层分类（D1）与 UI 提示共用同一推导与同一原子 mode pair（单一推导函数，禁止两处各自实现；详情、SSE、PATCH 响应、通知快照读取的都是同一份 rt.mu 内状态或 DB 值）。前端 `TaskInfoCard` 权限模式选择器（独立保存，D4）：两值不一致时展示「当前运行进程仍按「X」处理，新模式将在下次激活生效」；**保存成功且响应两值不一致（无法立即生效）时另给一条瞬时确认「已保存，将在下次激活后生效。」（下一次保存开始时清除；两值转为一致后不再展示；常驻提示独立保留）**。

**备选**：保存后一次性 toast——状态持续存在（直到下次激活），toast 瞬时不抗刷新，否决。

### D7：模式变更与通知层的交互

**决策**：真实变更统一发布两类事件，**全部在 runtime 收敛（D5）完成之后**（与 D4 序列一致，无第二种时序）——① 既有 `task.activity_changed`：一切真实变更都发布，**仅承担读模型失效**（项目摘要/active 概览仅刷新已保存 `permission_mode`；详情 GET/SSE 刷新已保存值与 `effective_permission_mode`——effective 透出范围仍以 D4/D8 为准，① 不向摘要/概览引入该字段）；**通知层 MUST NOT 消费 ①**（它不携带收敛语义）。② 仅当任务存在 runtime 时追加发布 `serve_runtime.permission_mode_changed`（Topic `serve_runtime`，RID = instVersion，`Payload=ServeRuntimeTaskPayload{TaskID}`——与 D1 唤醒事件同构：事件仅唤醒，通知层重读快照定论），**通知层的 waiting 重评估 MUST 只由 ② 驱动**。无 runtime 时无 instVersion，MUST NOT 发布 ②（waiting 在无 runtime 时随非 active 门禁不触发，下次激活后由 attention 事件驱动重评估）。通知层收到 ② 后重评估该任务的 waiting 条目：任务有效模式已非 ai-auto 时按 D1 非 ai-auto 语义处理（请求仍 pending 且未通知则触发）。**消费矩阵**：两个新事件类型（`serve_runtime.permission_verdict` / `serve_runtime.permission_mode_changed`）在 topic/payload 合法时 MUST NOT 标脏任何 API 读模型流（详情/active/项目流的过滤表对未知类型默认标脏——`task_filter.go:82`、`sessions_filter.go:82`、`projects_filter.go:29`，必须显式豁免这两个类型，读模型刷新仅依赖 `task.activity_changed`）；畸形事件（topic/payload 不合法）仍保守标脏。

### D8：Implementation Map（分层落点与 mock 边界）

项目为手工组合根（无 Fx，`cmd/ocdeck-server/main.go:159` 装配），application 层不依赖 infrastructure/api。落点：

- **store**：migration 仅新增 `tasks.permission_mode_at_start TEXT NULL` 列（SQL 不回填——SQL 无法判断现存进程语义）；启动回填经独立窄方法 `BackfillPermissionModeAtStart(ctx) error`（仅将 active 且 NULL 的行按持久化 `permission_mode` 回填该列，幂等；**MUST NOT 推进 `updated_at`、MUST NOT 发布任何事件**；任何 DB 错误原样返回）；`task_info_queries.go` 同文件新增 `UpdateTaskPermissionMode(ctx, taskID, mode string) (application.MutationResult, error)` 单列 UPDATE 原语（presence 不需要——必填单列；`MutationResult` 含 `Matched/Changed/UpdatedAtAdvanced`，updated_at 推进按 task-lifecycle Unix 秒精度规则；预读成功后 UPDATE `Matched=false` 按 D4 失败矩阵映射 404）；启动事实窄端口（签名钉死）：`SetPermissionModeAtStart(ctx, taskID string, mode string) error`——仅 `startRuntimeWithPortRetry` 建主 runtime 前调用（写失败不建进程；**MUST NOT 推进 `updated_at`、MUST NOT 发布任何事件**，内部启动事实非用户可见变更）与 `GetPermissionModeAtStart(ctx, taskID string) (*string, error)`——nullable 映射为 `*string`（nil=NULL；`resumeActive`/`tryRepairRuntime` 注册 runtime 前读取校验，D5 三路径矩阵）；不扩展 `TaskRow`（启动事实不进通用读路径）；**两方法归属 `internal/task.TaskStore`（manager.go:24 既有闭合接口）**——由 `task.StoreAdapter`（adapters.go:17）委托 `store.Queries` 实现，不新增 `application.TaskRepository` 方法、不动 sqlite adapter、无新增装配（三个消费点均在 Manager 内经既有 `m.store` 调用）；仅 `TaskStore` mocks 同步，补未命中（NULL）与写失败测试。
- **task（application 实现方）**：`Manager.BackfillPermissionModeAtStart(ctx) error` 启动回填入口（调用 store 窄方法，错误原样传播）；`Manager.UpdateTaskPermissionMode(ctx, taskID, mode string) (application.PermissionModeView, error)` 唯一协调器（D4 序列，签名钉死无省略）；`Manager.PermissionModeView(ctx, taskID) (application.PermissionModeView, error)` 供详情 GET/详情 SSE 复用；`application.PermissionModeView` 定于 `internal/application/dto.go`（字段 `PermissionMode string` / `EffectivePermissionMode string`——api 层经 import_graph_test 禁止依赖 `internal/task`，Manager 与 TaskBackend 两方法均逐字返回该 application 类型）；有效模式推导单一函数（输入为 runtime 的 `RuntimePermissionState` 原子读取结果或 nil（无 runtime 时读 DB 行），D5/D6）输出 `application.PermissionModeView`——详情 GET/详情 SSE/PATCH 响应/通知快照全部复用（禁止各自实现）；`permEpoch`/`RuntimePermissionState` runtime 状态与 epoch 算法（D5，AI 启用唯一事实源为 `RuntimePermissionState.AIAutoEnabled`，无独立布尔）；判定状态记录携带 epoch（D1/F12）；`judgeAndReply` REJECT 收敛（D2）；verdict/mode-changed/activity_changed 事件发布；`TaskNotificationSnapshot` 扩展（EffectivePermissionMode + 当前 epoch 判定状态映射）。
- **domain/event**：新增 `TypeServeRuntimePermissionVerdict` 与 `TypeServeRuntimePermissionModeChanged` 类型 + constructor（payload 复用既有 `ServeRuntimeTaskPayload{TaskID}`，`event.go:118`；RID=instVersion，D1/D7 事件契约）。
- **api**：路由 + handler（D4 契约表），`TaskBackend` 接口增 `UpdateTaskPermissionMode` 与 `PermissionModeView`；详情 DTO/SSE 映射 `effective_permission_mode`（经 `PermissionModeView`，输出归一化复用 `permissionModeForOutput`）；读模型流过滤表显式豁免两个新事件类型（D7 消费矩阵）。
- **notification**：waiting 状态机（D1）、verdict/mode-changed 事件订阅、overflow 矩阵、tick 到期检查。
- **web**：tab-local intent（D3，`TaskWorkbenchPage` + `TerminalView`）；`TaskInfoCard` 模式选择器独立保存 + effective 提示（D4/D6）；`types.ts`/`api.ts` 扩展——**`Task` 与 `TaskDetail` 类型分离：`effective_permission_mode` 仅存在于 `TaskDetail`（详情 GET/详情 SSE/模式端点响应，强制字段），通用 `Task`（列表/摘要/通用 PATCH 响应）不含该字段；通用任务信息 PATCH 回写页面任务时 MUST 合并保留当前 effective 值（不得用不含该字段的响应整体替换导致提示丢失）；模式端点响应仅合并两个模式字段**。
- **mock/测试边界**：`TaskBackend` fake、store repository adapter 编译断言、notification snapshot mocks 必须同步更新；无 Fx 装配变更（复用 main.go 已注入 bus/lifecycle）。**启动顺序钉死（组合根 main.go）：SQL add column → `Manager.BackfillPermissionModeAtStart` → `Reconcile` → HTTP open——回填任何 DB 错误传播到启动流程并阻止 HTTP 开放（fail-closed，与 D5 一致）；无新增依赖注入（复用既有 Manager/store 装配）**。
- **先收敛的既有逻辑**：共享三值校验函数抽取（crud.go:224-231 → 共用）；REJECT 回复分支移除（D2）；triggers.go:60 权限去重先记语义拆分（ai-auto 走 waiting，非 ai-auto 保持）；tabstrip 用户点击与程序性 switchTab 分流（D3）。

### D9：实施分期与测试策略

**主流程状态图**：

```
permission 通知（ai-auto 有效模式）：
  pending 出现 ──> waiting(inflight, deadline=+15s)
                     |-- verdict 事件(manual_required) --> 立即通知
                     |-- verdict 事件(settled_no_notify) -> 不通知,清除
                     |-- tick 到期仍 pending ------------> 兜底通知
                     |-- pending 消失 -------------------> 清除,不通知
  overflow 对账: 无 waiting 且未通知 -> 新建 waiting(对账时刻起算)
  进程重启: waiting 不恢复,按启动基线播种不补发

模式变更（D4/D5）：
  校验 -> per-task 锁 -> 读行 -> 同值?->200 : DB commit(PONR)
       -> rt.mu 收敛(有效切换才 epoch+1/补判)
       -> task.activity_changed -> permission_mode_changed -> 200
```

**Lane 排序（依赖序）**：① 契约/事件 lane（domain event 类型、共享校验、D2 REJECT 收敛）→ ② 持久化/runtime lane（store 原语、epoch 算法、permission_mode_at_start）→ ③ Notifier lane（D1 waiting 状态机）→ ④ API/Web lane（端点、详情透出、TaskInfoCard、tab 聚焦 D3，聚焦部分与其余 lane 完全独立可并行）。

**测试矩阵**：fake clock 覆盖 deadline 15s 与近 25s 边界；overflow 对账矩阵（waiting 保留/新建/播种、**mode-changed 事件因 overflow 丢失后既有 waiting 在对账成功退出 reconciling 后立即投递**）；epoch 屏障（切出后在途判定不回复、切入补判、同值不开 epoch、**切出再切入+旧 epoch 判定迟到不写状态不发事件**、**模式切换后 gate/DTO/通知快照读取同一 AIAutoEnabled 结论**）；**`RuntimePermissionState` 初始化表三路径一致**（ask→ai-auto、ai-auto→ask/all-approve 后 server 重启恢复、DR1 的 --auto 进程保存 ai-auto 后重启恢复仍不启用 AI）；**事件时序**（activity_changed 与 mode-changed 均在 runtime 收敛后发布；事件到达时详情 SSE 读到新 effective 值）；重启恢复 `permission_mode_at_start`（列回填——**启动顺序 SQL add column → BackfillPermissionModeAtStart → Reconcile → HTTP open，回填 DB 错误阻止 HTTP 开放**，NULL 语义、非法值 reconcile 报错拒绝开放 HTTP、`startRuntimeWithPortRetry` 写入失败不建进程、**shell/temp serve 的 NewSession 不写入该列（负向）**、`resumeActive`/`tryRepairRuntime` 有存活进程但列 NULL/非法时拒绝注册、store 窄端口未命中/写失败、启动事实写入不推进 updated_at 不发事件）；store/API 错误表（422 含未知字段/404 含 ErrNoRows 与 UPDATE 未命中/409/500 含非 ErrNoRows 读错误不误包 404、PONR 后取消）；无 runtime 模式变更（仅发 `task.activity_changed`，无 mode-changed）；读模型过滤（effective 仅详情 GET/SSE/本端点响应；**两个新事件类型不标脏读模型流——unrelated-task 不推帧**，畸形事件保守标脏）；**前端 effective 保留**（模式不一致后保存任务名称、SSE 不推帧时「下次激活生效」提示仍存在——通用 PATCH 回写合并保留 effective）；jsdom tab 聚焦（点击/重复点击/锁定/抢占/切走取消/程序性切换不聚焦）；统一保存竞态（禁用、重叠、旧响应覆盖）；判定状态分支表全枚举（D1 六分支）；`go test -race` 全量。

## Risks / Trade-offs

- [LLM 判定慢于预期时用户收到通知最晚 <25s] → 正常判定由唤醒事件即时通知；15s 窗口仅兜住 hang/慢调用；参数集中在 design 单点（D1），后续可调。
- [event bus 溢出丢失唤醒事件] → deadline + tick 兜底 + overflow 新建 waiting（D1 矩阵）保证最终通知。
- [all-approve 变更后窗口期运行进程仍按旧模式批准；DR1 下 `--auto` 进程切 ai-auto 同样下次激活才启用 AI] → effective 模式透出 + UI 常驻提示（D6）；用户已在探索阶段与 DR1 确认该语义。
- [epoch/permGate 并发复杂度] → 全部状态在 rt.mu 同步域、算法钉死（D5）、切入补判复用 judgeScan 单一入口，无第二判定路径。
- [REJECT 不再自动执行属行为语义 BREAKING] → 审计留痕保留 REJECT 结论供回溯；发布说明需提示存量 ai-auto 任务行为变化。

## Migration Plan

**持久化形态变化（本变更唯一）**：SQL migration 新增 `tasks.permission_mode_at_start TEXT NULL` 列（仅加列）；active 行回填由应用启动阶段在 reconcile 之前、HTTP 开放之前执行——升级前模式不可变，现存进程启动模式恒等于持久化 `permission_mode`，按该值直接回填 NULL 行（幂等；非 active 行保持 NULL）；回填失败 MUST 拒绝开放 HTTP（D5）。`permission_mode` 既有列与存量值不变。部署即生效：ai-auto REJECT 转人工与通知延迟触发对存量 ai-auto 任务即时生效；模式变更入口为新增独立子资源端点（`PATCH /api/v1/tasks/{id}/permission-mode`，D4），旧客户端不受影响。**回滚约束（操作要求，非热回滚保证）**：旧版本运行期间创建的进程不会维护 `permission_mode_at_start` 列，且 migration 对已记录版本不会重复执行——热回滚会使存活进程的启动事实失真（再次升级无法判断真实 argv）。因此回滚旧版本之前 MUST 先挂起全部 runtime；再次升级新版本之前同样 MUST 先挂起全部 runtime（挂起后无存活进程，列值不再参与有效模式推导，升级后激活时按新写入恢复正确语义）。不支持保留存活进程的热回滚。
