# Opencode Orchestration Specification

## Purpose
为每个活跃任务托管独立的 `opencode serve` 与 `opencode attach` 进程组，经专属 tmux server 管理生命周期、端口分配、就绪探测、SSE 归属捕获与重启 reconciliation。

## Requirements

### Requirement: 每任务独立 serve 实例
系统 SHALL 为每个活跃任务启动独立的 `opencode serve` 进程（托管于命名 tmux 会话 `ocdeck-<taskID>-serve`），仅监听 `127.0.0.1`，工作目录为该任务的 worktree，并以随机强密码启用 Basic Auth（`OPENCODE_SERVER_PASSWORD`）。ocdeck MUST 作为该 serve 的唯一访问入口。

#### Scenario: 激活时启动 serve
- **WHEN** 任务被激活
- **THEN** 系统分配端口、生成随机密码并以 worktree 为 cwd 启动 serve 进程

### Requirement: 端口分配策略
系统 SHALL 在 DB 记录任务上次成功端口（非事实来源）。激活时 MUST 先尝试上次端口；不可用时在可配置范围内（轮转起点）选择下一端口；serve 因端口占用退出时 MUST 自动换端口重试；serve 启动后未通过健康检查（serve not ready）时，后续重试 MUST 切换到**不同**端口——分配 MUST 排除刚失败的端口；健康检查通过后才将端口写回 DB。

换端口重试 MUST 以旧 serve 会话「已确认终止」为前提：已确认终止 = 会话已不存在（轮询期间进程死亡或 KillSession 返回 snapshot_missing_degraded）或 KillSession 确认 `SessionKilled=true`。active 任务 MUST NOT 携带 retryable cleanup debt：reap_failed / snapshot_failed / kill_failed / 未知或矛盾的 KillResult 时 MUST 记录对应 cleanup notice（未知矛盾结果按 kill_failed 记录）并判定激活失败，MUST NOT 分配新端口、MUST NOT 持久化端口快照、MUST NOT 创建新会话；snapshot_missing_degraded（不可重试、已接受丢失）记录 notice 后允许继续换端口重试；KillSession infra 错误时 MUST 记录 retryable kill_failed notice 并判定激活失败。所有终态路径在返回前 MUST 先本地持久化对应 cleanup notice；本地写失败 MUST 按 pending cleanup 合同移交外层重试（见下）。

serve 会话进程在健康轮询期间已死亡时 MUST 直接换端口重试（无需再执行 KillSession）。最后一次尝试失败后 MUST NOT 再执行端口分配与快照持久化。端口分配失败时 MUST 如实返回分配错误（含范围耗尽信息），MUST NOT 被前置健康检查错误覆盖。端口快照持久化失败时 MUST 判定激活失败，MUST NOT 创建新 serve 会话。健康轮询或探测期间 ctx 取消时 MUST 直接终止激活流程，MUST NOT 执行本地 KillSession、端口分配或快照持久化（外层清理逻辑负责收尾）。cleanup notice 持久化 MUST 使用脱离请求取消的有界 context；任何 notice 本地持久化失败时 MUST 判定激活失败，MUST NOT 分配新端口、MUST NOT 持久化端口快照、MUST NOT 创建新会话，且 MUST 将 pending cleanup 数据（sessionName + cleanup tickets + reason + retryable；cause 路径经携带型错误 additionally 含 notice 错误与原错误）随错误传递，MUST NOT 静默丢弃 tickets。外层统一清理 MUST 对每个 cleanup notice intent（KillSession 非 clean disposition 及 HasSession/KillSession infra 错误直记 kill_failed）形成 cleanup observation（reason/retryable/cleanupTickets/是否已落库；clean 不形成 observation、MUST NOT 覆盖或取消既有 pending——旧未落库 tickets 仍按原 reason 回放），KillResult 分类与轮换路径共用同一 fail-closed 映射（未知/矛盾按 kill_failed）。回放 MUST 在外层统一清理结束后、最终 DB 收敛前进行：fold 输入为按发生顺序的 cause pending 与 cleanup observations，同会话归并为单条——reason/retryable 以最新 observation 为准（后续写成功且 disposition 改变时 MUST NOT 被旧 pending 覆盖回旧值），cleanupTickets 为全部未落库 observation 的 union；仅当同会话存在未落库 observation 时回放一次。回放阶段使用单个新的 detached 有界 context（整个阶段一次预算 10s，非逐项；MUST NOT 复用清理阶段 context），回放项按首次出现顺序确定性遍历、每项恰好一次额外 recordResidualNotice 调用；回放仍失败时聚合进 last_error 收敛（store 在整个激活窗口持续不可写为不可约边界）。pending 收集与回放仅用于激活失败路径（Activate 统一补偿）；其余 cleanup 调用点（锁超时收敛、主动收敛、reconcile resume 失败清理）行为不变，保持现状首写聚合语义。轮换路径终态返回的错误 MUST 聚合可得上下文（健康检查错误 + KillSession 错误或 disposition + notice 错误），MUST NOT 只返回健康检查超时文案；能力探测失败返回探测错误本身（分类保持现状），外层清理与回放结果仅聚合进 last_error、不改变返回错误。

#### Scenario: 端口冲突回退
- **WHEN** 端口分配探测（bind 检查）发现候选端口被占用
- **THEN** 系统跳过该端口、自动选择下一可用端口重试，而非激活失败（分配后竞争导致 serve 因端口被抢占退出的情形，按「serve 进程死亡直接换端口重试」处置）

#### Scenario: serve 健康检查超时换端口重试
- **WHEN** 健康检查超时后 KillSession 返回 clean（会话已净终止、无 cleanup tickets），且重试预算未耗尽
- **THEN** 系统在一个与刚失败端口不同的端口上重新拉起 serve 重试，而非直接判定激活失败

#### Scenario: serve 进程死亡直接换端口重试
- **WHEN** serve 会话进程在健康轮询期间死亡（会话已不存在，含分配后竞争导致 serve 因端口被抢占退出的情形），且重试预算未耗尽
- **THEN** 系统不再执行 KillSession，直接在一个不同端口上重新拉起 serve 重试

#### Scenario: 会话快照期间消失继续重试
- **WHEN** 健康检查失败后调用 KillSession，会话于快照期间消失（snapshot_missing_degraded），且 cleanup notice 持久化成功、重试预算未耗尽
- **THEN** 系统记录不可重试的 cleanup notice（已接受丢失），并在一个与刚失败端口不同的端口上重新拉起 serve 重试

#### Scenario: 逃逸收割未净终止激活
- **WHEN** 健康检查失败后 KillSession 确认会话已终止但逃逸收割未净（reap_failed），且 cleanup notice 持久化成功
- **THEN** 系统记录含 tickets 的 cleanup notice，判定激活失败并回退挂起（不携带 retryable debt 继续激活），MUST NOT 分配新端口、持久化端口快照或创建新 serve 会话；挂起状态下后台重试收割不受 runtime 同名冲突影响

#### Scenario: 未知或矛盾 KillResult 保守失败
- **WHEN** KillSession 返回未识别的 disposition，或 disposition 与 SessionKilled 矛盾，且 cleanup notice 持久化成功
- **THEN** 系统按 kill_failed 记录 cleanup notice，判定激活失败并回退挂起，MUST NOT 分配新端口、持久化端口快照或创建新 serve 会话

#### Scenario: 端口快照持久化失败
- **WHEN** 换端口后 env 快照持久化失败
- **THEN** 激活失败，MUST NOT 创建新 serve 会话

#### Scenario: 旧 serve 会话未确认终止
- **WHEN** 健康检查失败后 KillSession 返回 infra 错误，或 disposition 表明会话仍存活（snapshot_failed / kill_failed），且 cleanup notice 持久化成功
- **THEN** 系统记录对应 cleanup notice，判定激活失败并回退挂起，MUST NOT 分配新端口、持久化端口快照或创建新 serve 会话

#### Scenario: cleanup notice 持久化失败
- **WHEN** 换端口重试前的 cleanup notice 本地持久化失败（store 不可达或 CAS 不收敛）
- **THEN** 激活失败并聚合原始错误，MUST NOT 分配新端口、持久化端口快照或创建新 serve 会话；pending cleanup 数据随错误传递至外层统一清理逻辑重试持久化，MUST NOT 静默丢弃 tickets

#### Scenario: 外层清理获得更新终态时回放不覆盖
- **WHEN** 轮换路径 cleanup notice 写失败形成 pending，外层统一清理再次 kill 同一会话得到更新的 disposition（如 kill_failed → snapshot_missing_degraded）且写成功
- **THEN** 回放后 notice 的 reason/retryable 以最新 disposition 为准，MUST NOT 被旧 pending 覆盖回旧值；cleanupTickets 完整包含首次写失败的 tickets

#### Scenario: 外层清理 notice 写失败归并回放
- **WHEN** 外层统一清理路径的 cleanup notice 持久化失败
- **THEN** 系统收集该会话 cleanup observation，归并后在最终 DB 收敛前回放一次；回放仍失败时聚合进 last_error

#### Scenario: 端口范围耗尽
- **WHEN** 可配置端口范围内（排除本次重试需避开的端口后）无可用端口
- **THEN** 激活失败并返回明确的端口耗尽错误（分配错误本身），任务回退挂起

#### Scenario: 轮询期间 ctx 取消
- **WHEN** 健康轮询或能力探测期间请求 ctx 被取消
- **THEN** 激活流程直接终止，不执行本地 KillSession、端口分配或快照持久化；完整 Activate 路径由外层统一清理逻辑执行清理 kill 并收尾

### Requirement: TUI attach 进程
系统 SHALL 为每个活跃任务在命名 tmux 会话（`ocdeck-<taskID>-tui`）中运行 `opencode attach http://127.0.0.1:<port>`，会话工作目录 MUST 为任务 worktree；浏览器经 PTY 中的 `tmux -L ocdeck attach -t <session>` 客户端接入该会话。serve 密码 MUST 经会话 env（`new-session -e OPENCODE_SERVER_PASSWORD`）传递，MUST NOT 经 `--password` argv 传递（避免进程列表泄露）。全部 tmux 会话 MUST 只能由统一的 launcher 创建并强制 canonical worktree cwd（`-c` 参数）；对 serve 的全部 API 请求 MUST 显式携带 directory 参数。

#### Scenario: attach 连接到正确实例
- **WHEN** serve 进程就绪后
- **THEN** 系统创建 TUI tmux 会话，TUI 作用于本任务 worktree

### Requirement: serve 就绪等待与能力探测
系统 SHALL 在 serve 启动后轮询 `GET /global/health`，就绪后 MUST 做能力探测（`/session/status` 响应结构与 session 列表字段形状校验；**DELETE 响应形状 MUST NOT 做 live 探测**——不能为探测制造删除副作用，首次真实删除时校验，不符报 deletion_failed）；探测通过才启动 TUI 会话。单端口尝试的健康检查超时或 serve 进程死亡 MUST 先按「端口分配策略」处置（旧会话清理、cleanup notice、换端口判定），仅当该策略判定允许换端口时才以不同端口重试；全部尝试耗尽后 MUST 判定激活失败，清理会话并回滚任务状态为挂起 + 记录 last_error。**任何能力探测失败**（含冷启动重试耗尽后的 serve 未就绪、认证失败、能力不兼容、未知错误）MUST 直接判定激活失败，MUST NOT 触发换端口重试，错误分类保持既有映射，清理会话并回滚任务状态为挂起 + 记录 last_error。opencode 版本号 MUST NOT 作为激活门禁：版本超出已验证契约区间 [ContractMinVersion, ContractBaseline] 时仅告警（日志 + UI 提示），不阻止激活。

#### Scenario: serve 启动全部尝试耗尽
- **WHEN** 所有端口尝试均未通过健康检查（或重试预算耗尽）
- **THEN** 激活失败，进程被清理，任务回到挂起并展示错误

#### Scenario: API 能力不兼容
- **WHEN** serve 返回的响应结构与 occlient 契约不符（版本号不一致仅告警，不触发本场景）
- **THEN** 激活被阻止并提示 opencode 契约兼容性问题，不触发换端口重试

#### Scenario: 能力探测未就绪不轮换
- **WHEN** serve 通过健康检查但能力探测在冷启动重试耗尽后仍返回未就绪
- **THEN** 激活失败，不触发换端口重试、不执行本地 KillSession，由外层统一清理逻辑 kill 会话并按 KillResult 在非 clean/错误时记录 cleanup notice（clean 不记），任务回退挂起并按既有分类记录错误

#### Scenario: 其他能力探测终态错误不轮换
- **WHEN** serve 通过健康检查但能力探测返回认证失败或未知错误
- **THEN** 激活失败，不触发换端口重试、不执行本地 KillSession，由外层统一清理逻辑收尾，任务回退挂起并按既有分类记录错误

### Requirement: 结构化状态查询
系统 SHALL 通过任务的 serve REST API（如 `GET /session/status`）获取任务 agent 运行状态，用于任务列表的状态展示。

#### Scenario: 任务列表展示运行状态
- **WHEN** 用户查看任务列表
- **THEN** 活跃任务展示来自 serve API 的 agent 状态（如 idle/busy）

### Requirement: 进程经 tmux 会话托管
系统 SHALL 以专属 tmux server（`tmux -L ocdeck`，与用户自有 tmux 隔离）托管全部任务进程：serve、TUI（opencode attach）、shell 各自运行于命名 tmux 会话（`ocdeck-<taskID>-<role>`）。会话命名 MUST 含 taskID 作为运行时注册表；env MUST 经 `new-session -e` argv 注入，命令 MUST 从白名单 argv 单引号转义构造。终止会话时 MUST 先快照 pane 子孙进程（reaper），kill-session 后对逃逸子孙按身份校验（pid+startTime）先 TERM 宽限后 KILL。不遗留孤儿进程为不变量目标；个别会话终止失败时 MUST 记录 tasks.notice 并由后台周期任务重试清理，MUST NOT 因此阻塞状态收敛。

#### Scenario: 挂起时干净退出
- **WHEN** 任务被挂起
- **THEN** serve、TUI 与全部 shell 会话被完整终止（含逃逸子孙收割），无残留进程

### Requirement: 服务端关停策略（shutdownPolicy）
系统 SHALL 提供三档关停策略配置（`shutdownPolicy` ∈ `persist | kill_on_start | kill_immediate`，默认 persist）。正常退出时：persist 保留全部会话，其余两档 kill 全部会话。异常死亡（含 kill -9）时：persist 模式会话存活待重启恢复；kill_on_start 模式由下次启动 reconcile 清理；kill_immediate 模式 MUST 由单个 watchdog 子进程（服务端启动时、任何会话创建之前 spawn，轮询 ppid）在父亡时执行 `tmux -L ocdeck kill-server`。

#### Scenario: 服务端被 kill -9（persist）
- **WHEN** shutdownPolicy=persist 且 ocdeck 服务端异常死亡
- **THEN** 全部任务会话存活，服务端重启后 reattach 恢复，agent 工作不丢失

#### Scenario: 服务端被 kill -9（kill_immediate）
- **WHEN** shutdownPolicy=kill_immediate 且 ocdeck 服务端异常死亡
- **THEN** watchdog 检测到父进程消失，kill-server 终止全部 ocdeck 会话，无孤儿 oc 进程

### Requirement: 服务端启动 reconciliation
系统 SHALL 在启动时枚举 `tmux -L ocdeck` 会话并按 taskID 与 DB 对账：persist 模式下 **active/activating** 任务中 serve 会话存活且健康检查通过且无 cleanup debt 的 MUST 恢复为活跃（重订阅 SSE + 全量对齐），否则落为挂起；**suspending 任务 MUST 完成清理落为挂起**（以持久化意图为准，不得恢复活跃）；**archived/creation_failed/deletion_failed 等持久状态 MUST 保持原状**（仅清理其异常会话）；kill_on_start / kill_immediate 模式下 MUST kill 全部 ocdeck 会话并将 active/activating/suspending 任务收敛为挂起（其余持久状态保持原状）；taskID 无对应 DB 行的孤儿会话 MUST 一律清理，清理失败进入本次运行后台周期重试。

#### Scenario: 重启后状态恢复（persist）
- **WHEN** 服务端重启（persist 模式，此前异常退出）
- **THEN** active/activating 中 serve 健康且无 cleanup debt 的任务自动恢复活跃，用户打开终端即见 agent 当前状态；suspending 完成清理落挂起；其余持久状态保持原状

#### Scenario: 重启后状态对齐（kill 模式）
- **WHEN** 服务端重启（kill_on_start 或 kill_immediate 模式）
- **THEN** active/activating/suspending 任务显示为挂起（archived/creation_failed/deletion_failed 保持原状），无进程残留，用户可手动激活恢复

### Requirement: session 归属捕获
系统 SHALL 订阅每个活跃 serve 的 SSE 事件流（`GET /event`）：`session.created` 事件的 sessionID 位于 `properties.info.id`（已验证契约区间 [ContractMinVersion, ContractBaseline]）；同时监听 `session.updated` 刷新 `last_seen_at`。激活后 MUST 全量对齐一次该 directory 的 session 列表。SSE 断流时 MUST 指数退避重连，重连成功后 MUST 再次全量对齐。

**session 所有权规则**：一个 opencode session 至多归属一个 ocdeck 任务（该约束适用于经本变更后合法写入口产生的新归属；历史遗留的重复归属行不做启动修复，随任务删除自然清理）；任务 MUST 仅对本任务拥有的 session（`task_sessions` 中本任务的行）执行删除、attach 与对齐写回。归属写入 MUST 统一经 store 层原子 claim（单事务内"仅当 sessionID 未被其他任务拥有时插入/更新本任务行"），MUST NOT 以"先查询后 upsert"的非原子方式写归属。claim 冲突语义：SSE/对齐路径冲突 MUST 忽略该 session 并记服务端诊断日志（不阻断）；锚定创建路径冲突 MUST 使激活失败并记录 last_error，MUST NOT attach 不属本任务的 session。

`kind=dir` 项目的任务（目录可共享）MUST NOT 经目录级全量对齐认领新 session。dir 对齐 MUST 按以下顺序执行：① 按原始目录列表数量判定 complete/overflow（判定先于任何过滤）；② 候选集取"原始目录列表 ∩ 本任务当前 owned 集合"；③ complete 时在单个 store 事务内仅对候选集刷新 `last_seen_at`、仅删除本任务 owned 集合中的缺席行，并经事务内 noticeFn 清除既有 session_overflow notice；④ overflow 时不删任何缺席行，application 层 MUST 先经事务外 CAS 写入 session_overflow notice 再调对齐（对齐失败时 notice 保留，与 repo 现状逐点一致），仅刷新候选集。dir 任务的新 session 仅经本任务 serve 的 SSE 捕获（原子 claim）与锚定创建记录归属（SSE 断流期间经 TUI 新建的 session 不补记，为已接受的降级）。`session.updated` 事件 MUST 仅刷新本任务已归属行的 `last_seen_at`（条件更新，绝不插入新归属），未归属 session 的 updated 事件一律忽略。

kind 传播 MUST 覆盖全部四个会建立 SSE/对齐/锚定的运行时入口：Activate、persist 重启恢复（resumeActive）、挂起失败的运行时修复（tryRepairRuntime）、TUI 重开（ReopenAttach）；四者在任何状态修改或运行时副作用前 MUST 解析并校验项目 kind，未知 kind MUST 报错且零副作用。ReopenAttach 的锚定 claim 冲突 MUST 返回错误并记录 last_error，任务保持 active 不收敛，MUST NOT attach 不属本任务的 session。

同目录双 serve 不串流是该 SSE 归属方案的前提，已经 OpenCode 源码验证（设计阶段完成）：`/event` 订阅的是进程内 listener（`server/routes/instance/httpapi/handlers/event.ts`），事件 publish 仅 notify 本进程 PubSub（`core/event.ts`），跨进程仅可经 `sync/history` 显式拉取；该架构自 v1.16.0 起连续稳定（v1.18.14 ↔ v1.18.18 锚点字节级一致；扩展区间须相邻对核验）。若未来 OpenCode 升级引入存储级事件分发，dir 任务归属 MUST 重新评审。

#### Scenario: TUI 新建会话被记录
- **WHEN** 用户在 TUI 中新建会话
- **THEN** 新 sessionID 经 SSE 被捕获并原子 claim 至本任务，用于后续恢复；若已被其他任务拥有则忽略并记诊断日志

#### Scenario: 断流后对齐
- **WHEN** SSE 连接断开并恢复（repo 任务）
- **THEN** 系统重连后全量对齐 session 列表，断流期间错过的会话被补记

#### Scenario: 同目录 dir 任务互不认领
- **WHEN** 同一 dir 项目下两个活跃任务 A/B（同一目录）各自执行全量对齐
- **THEN** 任务 A 的对齐仅核对自身 owned session，不认领任务 B 拥有的 session，反之亦然；目录中不属于任何任务的 session（如用户手工运行 opencode 产生）不被任何任务认领

#### Scenario: 同目录 dir 任务删除隔离
- **WHEN** 删除同一 dir 项目下的任务 A（任务 B 仍活跃）
- **THEN** 系统仅删除任务 A 拥有的 session；任务 B 的 session、锚定与对话状态不受影响

#### Scenario: dir 任务断流降级
- **WHEN** dir 任务 SSE 断流期间用户在 TUI 新建会话，随后重连并全量对齐
- **THEN** 该新会话不被补记进任务归属（与"他人/手工创建"无法区分），任务既有 session 的存在性核对与缺席清理语义不变

#### Scenario: 并发 claim 唯一归属
- **WHEN** 两个任务（如 SSE 与对齐并发）同时 claim 同一 sessionID
- **THEN** 原子 claim 仅一个成功，该 session 归属唯一任务；失败方按路径语义忽略/记诊断

#### Scenario: session.updated 不创建归属
- **WHEN** 任务收到未归属 session 的 session.updated 事件
- **THEN** 系统忽略该事件（条件更新未命中，不插入归属行、不报错）；已归属 session 的 updated 事件仅刷新 last_seen_at

#### Scenario: 挂起修复路径的 kind 校验
- **WHEN** dir 任务挂起失败后进入运行时修复（重建 SSE/对齐/锚定），或任务所属项目 kind 为未知值
- **THEN** 修复路径在任何状态修改或运行时副作用前校验 kind：dir 任务按 ownedOnly 模式对齐（不认领同目录他任务 session）；未知 kind 报错且零副作用

#### Scenario: TUI 重开路径的归属安全
- **WHEN** dir 任务 TUI 消失后重开（ReopenAttach），无锚定记录或预检 404 需创建新 session
- **THEN** 新 session 经原子 claim 归属本任务；claim 冲突时返回错误并记录 last_error，任务保持 active，MUST NOT attach 不属本任务的 session

### Requirement: 进程退出监视
系统 SHALL 监视任务全部 tmux 会话的存活（has-session 轮询，serve 另有健康轮询）：serve 会话异常消失（非挂起路径）时任务 MUST 落为挂起并记录 last_error；TUI 会话消失而 serve 存活时任务保持活跃、终端标记为可重开。

#### Scenario: serve 崩溃
- **WHEN** 活跃任务的 serve 会话异常消失
- **THEN** 任务自动落为挂起，UI 展示 last_error，用户可重新激活

#### Scenario: TUI 退出
- **WHEN** 用户退出 TUI 导致其会话消失但 serve 存活
- **THEN** 任务保持活跃，重新打开终端页时重开 TUI 会话

### Requirement: serve HTTP 客户端连接管理
ocdeck-server 进程内经 `internal/opencode` Client 发起的对任务 serve 的全部 HTTP 请求（健康检查、能力探测、状态查询、后台重试、SSE）SHALL 复用共享的 loopback HTTP transport，跨 Client 实例按 host:port 连接池复用，空闲连接 MUST 有回收上限；该 transport MUST 显式禁用代理（`Proxy: nil`）。TUI `opencode attach` 独立进程的连接行为不在本要求范围。

#### Scenario: 新建 client 实例共享连接池
- **WHEN** 系统为状态查询/后台重试等路径创建新的 occlient 实例访问同一 serve
- **THEN** 新实例复用共享 transport 的空闲连接，而非各自新建独立连接池

#### Scenario: 空闲连接有界回收
- **WHEN** 共享 transport 中的连接进入空闲状态
- **THEN** 空闲连接受 MaxIdleConns/IdleConnTimeout 上限约束并被按期回收，而非无限驻留

#### Scenario: 代理禁用语义保持
- **WHEN** 宿主环境设置了 HTTP_PROXY/HTTPS_PROXY
- **THEN** occlient 对 serve 的 loopback 请求仍直连 127.0.0.1，不经过代理
