# Opencode Orchestration Specification

## Purpose
为每个活跃任务托管单个 `opencode --port` 进程（runtime 会话），同进程暴露 TUI 与完整 HTTP API/SSE；浏览器经 tmux attach 客户端接入同进程 TUI；任务进程异常退出后自动重拉。经专属 tmux server 管理生命周期、端口分配、就绪探测、SSE 归属捕获与重启 reconciliation。

## Requirements

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

### Requirement: serve 就绪等待与能力探测
系统 SHALL 在任务进程启动后轮询 `GET /global/health`，就绪后 MUST 做能力探测（`/session/status` 响应结构与 session 列表字段形状校验；**DELETE 响应形状 MUST NOT 做 live 探测**——不能为探测制造删除副作用，首次真实删除时校验，不符报 deletion_failed）。探测通过判定当前进程 ready——TUI 与 API 同进程，MUST NOT 再创建独立 TUI 会话；任务活跃的**唯一提交点**是「任务进程自动重拉」定义的成功提交全序列（锚定确认 → token/group/watcher 注册 → SSE 订阅 + 全量对齐 → 写回 `tasks.last_port` → CAS active）完成之后，MUST NOT 在探测通过时提前 CAS active。单端口尝试的健康检查超时或进程死亡 MUST 先按「端口分配策略」处置（旧会话清理、cleanup notice、换端口判定），仅当该策略判定允许换端口时才以不同端口重试；全部尝试耗尽后 MUST 判定激活失败，清理会话并回滚任务状态为挂起 + 记录 last_error。**任何能力探测失败**（含冷启动重试耗尽后的进程未就绪、认证失败、能力不兼容、未知错误）MUST 直接判定激活失败，MUST NOT 触发换端口重试，错误分类保持既有映射，清理会话并回滚任务状态为挂起 + 记录 last_error。opencode 版本号 MUST NOT 作为激活门禁：版本超出已验证契约区间 [ContractMinVersion, ContractBaseline] 时仅告警（日志 + UI 提示），不阻止激活。

#### Scenario: 进程启动全部尝试耗尽
- **WHEN** 所有端口尝试均未通过健康检查（或重试预算耗尽）
- **THEN** 激活失败，进程被清理，任务回到挂起并展示错误

#### Scenario: API 能力不兼容
- **WHEN** 任务进程返回的响应结构与 occlient 契约不符（版本号不一致仅告警，不触发本场景）
- **THEN** 激活被阻止并提示 opencode 契约兼容性问题，不触发换端口重试

#### Scenario: 能力探测未就绪不轮换
- **WHEN** 任务进程通过健康检查但能力探测在冷启动重试耗尽后仍返回未就绪
- **THEN** 激活失败，不触发换端口重试、不执行本地 KillSession，由外层统一清理逻辑 kill 会话并按 KillResult 在非 clean/错误时记录 cleanup notice（clean 不记），任务回退挂起并按既有分类记录错误

#### Scenario: 其他能力探测终态错误不轮换
- **WHEN** 任务进程通过健康检查但能力探测返回认证失败或未知错误
- **THEN** 激活失败，不触发换端口重试、不执行本地 KillSession，由外层统一清理逻辑收尾，任务回退挂起并按既有分类记录错误

### Requirement: 结构化状态查询
系统 SHALL 通过任务的 serve REST API（如 `GET /session/status`）获取任务 agent 运行状态，用于任务列表的状态展示。

#### Scenario: 任务列表展示运行状态
- **WHEN** 用户查看任务列表
- **THEN** 活跃任务展示来自 serve API 的 agent 状态（如 idle/busy）

### Requirement: 进程经 tmux 会话托管
系统 SHALL 以专属 tmux server（`tmux -L ocdeck`，与用户自有 tmux 隔离）托管全部任务进程：任务单进程与 shell 各自运行于命名 tmux 会话（`ocdeck-<taskID>-<suffix>`，运行期 suffix ∈ runtime / shell-&lt;n&gt;，n 为正整数）。会话命名 MUST 含 taskID 作为运行时注册表；env MUST 经 `new-session -e` argv 注入，命令 MUST 从白名单 argv 单引号转义构造。全部 tmux 会话 MUST 只能由统一的 launcher 创建并强制 canonical worktree cwd（`-c` 参数）。进程层白名单 MUST 拆分：创建（NewSession）仅接受 `runtime`/`shell-<n>` 并拒绝 legacy `serve`/`tui` 后缀；管理/清理（HasSession/KillSession/watch）同时接受 legacy 后缀（旧会话可清理、不可新建）。终止会话时 MUST 先快照 pane 子孙进程（reaper），kill-session 后对逃逸子孙按身份校验（pid+startTime）先 TERM 宽限后 KILL。不遗留孤儿进程为不变量目标；个别会话终止失败时 MUST 记录 tasks.notice 并由后台周期任务重试清理，MUST NOT 因此阻塞状态收敛。

#### Scenario: 挂起时干净退出
- **WHEN** 任务被挂起
- **THEN** 任务进程与全部 shell 会话被完整终止（含逃逸子孙收割），无残留进程

#### Scenario: legacy 会话可清理不可新建
- **WHEN** 进程层收到创建 `ocdeck-<taskID>-serve` 或 `-tui` 会话的请求
- **THEN** NewSession 拒绝；而 HasSession/KillSession 对 legacy 后缀会话正常工作（供迁移清理）

### Requirement: 服务端关停策略（shutdownPolicy）
系统 SHALL 提供三档关停策略配置（`shutdownPolicy` ∈ `persist | kill_on_start | kill_immediate`，默认 persist）。正常退出时：persist 保留全部会话，其余两档 kill 全部会话。异常死亡（含 kill -9）时：persist 模式会话存活待重启恢复；kill_on_start 模式由下次启动 reconcile 清理；kill_immediate 模式 MUST 由单个 watchdog 子进程（服务端启动时、任何会话创建之前 spawn，轮询 ppid）在父亡时执行 `tmux -L ocdeck kill-server`。

#### Scenario: 服务端被 kill -9（persist）
- **WHEN** shutdownPolicy=persist 且 ocdeck 服务端异常死亡
- **THEN** 全部任务会话存活，服务端重启后 reattach 恢复，agent 工作不丢失

#### Scenario: 服务端被 kill -9（kill_immediate）
- **WHEN** shutdownPolicy=kill_immediate 且 ocdeck 服务端异常死亡
- **THEN** watchdog 检测到父进程消失，kill-server 终止全部 ocdeck 会话，无孤儿 oc 进程

### Requirement: 服务端启动 reconciliation
系统 SHALL 在启动时枚举 `tmux -L ocdeck` 会话并按 taskID 与 DB 对账：persist 模式下仅 **active** 任务中任务进程会话存活且健康检查通过且无 cleanup debt 的 MUST 恢复为活跃（重订阅 SSE + 全量对齐），否则落为挂起；**activating 任务 MUST 一律视为被中断的激活/恢复——执行清理并落为挂起**（单进程无锚定 bootstrap 的中间态无法经会话名区分，不支持续跑）；**suspending 任务 MUST 完成清理落为挂起**（以持久化意图为准，不得恢复活跃）；**archived/creation_failed/deletion_failed 等持久状态 MUST 保持原状**（仅清理其异常会话）；kill_on_start / kill_immediate 模式下 MUST kill 全部 ocdeck 会话并将 active/activating/suspending 任务收敛为挂起（其余持久状态保持原状）；taskID 无对应 DB 行的孤儿会话 MUST 一律清理，清理失败进入本次运行后台周期重试。旧版双进程布局的遗留会话（同 taskID 的 `-serve` 与 `-tui` 会话）MUST 一律按异常会话清理，不支持热迁移恢复。

#### Scenario: 重启后状态恢复（persist）
- **WHEN** 服务端重启（persist 模式，此前异常退出）
- **THEN** active 中任务进程健康且无 cleanup debt 的任务自动恢复活跃，用户打开终端即见 agent 当前状态；activating 任务完成清理落挂起（不续跑中断的激活/恢复）；suspending 完成清理落挂起；其余持久状态保持原状

#### Scenario: 重启后状态对齐（kill 模式）
- **WHEN** 服务端重启（kill_on_start 或 kill_immediate 模式）
- **THEN** active/activating/suspending 任务显示为挂起（archived/creation_failed/deletion_failed 保持原状），无进程残留，用户可手动激活恢复

#### Scenario: 遗留双进程会话清理
- **WHEN** 启动 reconciliation 发现旧版布局的 `-serve`/`-tui` 会话
- **THEN** 一律按孤儿/异常会话清理，对应任务按上述状态规则收敛，不尝试恢复双进程运行时

### Requirement: session 归属捕获
系统 SHALL 订阅每个活跃任务进程的 SSE 事件流（`GET /event`）：`session.created` 事件的 sessionID 位于 `properties.info.id`（已验证契约区间 [ContractMinVersion, ContractBaseline]）；同时监听 `session.updated` 刷新 `last_seen_at`。激活后 MUST 全量对齐一次该 directory 的 session 列表。SSE 断流时 MUST 指数退避重连，重连成功后 MUST 再次全量对齐。

**session 所有权规则**：一个 opencode session 至多归属一个 ocdeck 任务（该约束适用于经本变更后合法写入口产生的新归属；历史遗留的重复归属行不做启动修复，随任务删除自然清理）；任务 MUST 仅对本任务拥有的 session（`task_sessions` 中本任务的行）执行删除、attach 与对齐写回。归属写入 MUST 统一经 store 层原子 claim（单事务内"仅当 sessionID 未被其他任务拥有时插入/更新本任务行"），MUST NOT 以"先查询后 upsert"的非原子方式写归属。claim 冲突语义：SSE/对齐路径冲突 MUST 忽略该 session 并记服务端诊断日志（不阻断）；锚定创建路径冲突 MUST 使激活失败并记录 last_error，MUST NOT attach 不属本任务的 session。

`kind=dir` 项目的任务（目录可共享）MUST NOT 经目录级全量对齐认领新 session。dir 对齐 MUST 按以下顺序执行：① 按原始目录列表数量判定 complete/overflow（判定先于任何过滤）；② 候选集取"原始目录列表 ∩ 本任务当前 owned 集合"；③ complete 时在单个 store 事务内仅对候选集刷新 `last_seen_at`、仅删除本任务 owned 集合中的缺席行，并经事务内 noticeFn 清除既有 session_overflow notice；④ overflow 时不删任何缺席行，application 层 MUST 先经事务外 CAS 写入 session_overflow notice 再调对齐（对齐失败时 notice 保留，与 repo 现状逐点一致），仅刷新候选集。dir 任务的新 session 仅经本任务进程的 SSE 捕获（原子 claim）与锚定创建记录归属（SSE 断流期间经 TUI 新建的 session 不补记，为已接受的降级）。`session.updated` 事件 MUST 仅刷新本任务已归属行的 `last_seen_at`（条件更新，绝不插入新归属），未归属 session 的 updated 事件一律忽略。

kind 传播 MUST 覆盖全部四个会建立 SSE/对齐/锚定的运行时入口：Activate、persist 重启恢复（resumeActive）、挂起失败的运行时修复（tryRepairRuntime）、自动重拉恢复（ensureRecovery，含 ReopenAttach 转发而来的恢复）；四者在任何状态修改或运行时副作用前 MUST 解析并校验项目 kind，未知 kind MUST 报错且零副作用。恢复路径的锚定 claim 冲突 MUST 判定本次恢复失败并记录 last_error（计入重拉预算），MUST NOT 接入不属本任务的 session。

同目录双进程不串流是该 SSE 归属方案的前提，已经 OpenCode 源码验证（设计阶段完成）：`/event` 订阅的是进程内 listener（`server/routes/instance/httpapi/handlers/event.ts`），事件 publish 仅 notify 本进程 PubSub（`core/event.ts`），跨进程仅可经 `sync/history` 显式拉取；该架构自 v1.16.0 起连续稳定（v1.18.14 ↔ v1.18.18 锚点字节级一致；扩展区间须相邻对核验）。若未来 OpenCode 升级引入存储级事件分发，dir 任务归属 MUST 重新评审。

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

#### Scenario: 恢复路径的归属安全
- **WHEN** 任务进程自动重拉（含 ReopenAttach 转发），无锚定记录或预检 404 需创建新 session
- **THEN** 新 session 经原子 claim 归属本任务；claim 冲突时判定本次恢复失败并记录 last_error（计入重拉预算），MUST NOT 接入不属本任务的 session

### Requirement: 进程退出监视
系统 SHALL 监视任务全部 tmux 会话的存活（has-session 轮询，任务进程另有健康轮询）：任务进程会话异常消失（active 且 token 匹配、无进行中生命周期操作）时 MUST 按「任务进程自动重拉」处置，MUST NOT 直接落挂起；shell 会话消失不影响任务状态。任务进程与 TUI 同生共死，不再存在「TUI 消失而 serve 存活」的分支。SSE 流永久失败（非主动 cancel）MUST 与进程会话消失走同一幂等 ensureRecovery 入口（统一 runtime failure 分派），MUST NOT 直接落挂起；SSE EOF 先于 watcher 到达且进程仍存活（失去控制面）时，恢复前序清理 MUST 终止该 runtime。

#### Scenario: 任务进程异常消失触发重拉
- **WHEN** 活跃任务的进程会话异常消失（含用户 TUI exit 与崩溃）
- **THEN** 系统进入自动重拉流程；重拉成功任务保持活跃，预算耗尽才落挂起并展示 last_error

#### Scenario: SSE 永久失败触发重拉
- **WHEN** 活跃任务的 SSE 流永久返回（非主动 cancel，如进程退出导致 EOF 先于 tmux watcher 到达）
- **THEN** 系统进入同一幂等 ensureRecovery 流程（必要时先终止仍存活但失去控制面的 runtime），而非直接落挂起

#### Scenario: shell 会话退出
- **WHEN** 某个 shell 终端会话消失而任务进程存活
- **THEN** 任务保持活跃，仅该 shell 终端标记为已关闭

### Requirement: serve HTTP 客户端连接管理
ocdeck-server 进程内经 `internal/infrastructure/opencode` Client 发起的对任务进程的全部 HTTP 请求（健康检查、能力探测、状态查询、后台重试、SSE）SHALL 复用共享的 loopback HTTP transport，跨 Client 实例按 host:port 连接池复用，空闲连接 MUST 有回收上限；该 transport MUST 显式禁用代理（`Proxy: nil`）。对任务进程的全部 directory-scoped API 请求 MUST 显式携带 directory 参数。

#### Scenario: 新建 client 实例共享连接池
- **WHEN** 系统为状态查询/后台重试等路径创建新的 occlient 实例访问同一任务进程
- **THEN** 新实例复用共享 transport 的空闲连接，而非各自新建独立连接池

#### Scenario: 空闲连接有界回收
- **WHEN** 共享 transport 中的连接进入空闲状态
- **THEN** 空闲连接受 MaxIdleConns/IdleConnTimeout 上限约束并被按期回收，而非无限驻留

#### Scenario: 代理禁用语义保持
- **WHEN** 宿主环境设置了 HTTP_PROXY/HTTPS_PROXY
- **THEN** occlient 对任务进程的 loopback 请求仍直连 127.0.0.1，不经过代理

### Requirement: 每任务单进程实例
系统 SHALL 为每个活跃任务启动单个 opencode 进程：`opencode --port <port> --hostname 127.0.0.1`（托管于命名 tmux 会话 `ocdeck-<taskID>-runtime`），工作目录为该任务 worktree，并以随机强密码经会话 env（`new-session -e OPENCODE_SERVER_PASSWORD`）启用 Basic Auth。该进程经 `--port`/`--hostname` 进入 external 模式，在同一进程内同时提供 TUI（浏览器经 tmux attach 客户端接入）与完整 HTTP API/SSE 控制面（与 `opencode serve` 相同的 `Server.listen` 实现）。ocdeck MUST 作为该进程 API 的唯一访问入口；密码 MUST NOT 经 argv 传递。

锚定 session bootstrap MUST 采用确定性顺序协议（MUST NOT 依赖启动期 SSE `session.created` 事件——不可重放、与订阅建立有竞态、无法从并发事件中识别 bootstrap 事件）：

1. **启动**：有锚定 → 命令 MUST 携带 `--session <anchoredSessionID>`；无锚定 → MUST NOT 携带 `--session`
2. **就绪校验**：健康检查 + 能力探测通过后 MUST `GET /session?directory=` 取列表：有锚定且在列表中 → 锚定确认；有锚定但不在列表（已失效）→ 弃用旧锚定转无锚定流程
3. **无锚定创建**：MUST `POST /session?directory=` 创建新 session，**按响应 ID 原子 claim** 并写入锚定；claim 冲突 MUST 判定本次激活/恢复失败并记录 last_error（恢复场景计入重拉预算），MUST NOT 接入不属本任务的 session
4. **落到锚定（双启动子事务，仅新建锚定时；permit 子协议仅适用 Recovery 路径——首次 Activate 的双启动 MUST NOT 消耗恢复 permit、不执行恢复退避）**：① bootstrap 进程占用一个重拉预算 permit 并完成健康检查 + 能力探测；② `POST /session` + claim 后 MUST 按既有 KillResult/cleanup notice 规则确认 bootstrap 进程已终止，才可复用 `-runtime` 名称与端口；③ 正式进程占用**新的**预算 permit 并执行对应退避，端口复用已持久化值、密码重新生成；④ 正式进程 MUST 重新执行健康检查 + 能力探测 + 锚定存在校验；⑤ 全部通过才进入成功提交；⑥ 预算窗口不足以取得第二个 permit 时，已 claim 的锚定 MUST 保留，本次尝试进入终态补偿
5. dir 项目 MUST NOT 经目录级对齐认领（ownedOnly 语义不变）

`--session` 失效的确定性分派：进程在 HTTP server 就绪前退出 → 健康轮询判死/超时 → 按本次尝试失败处理（既有错误分类）；进程就绪 → 列表校验是唯一正确性判据（锚定在列表中且 id 有效则 CLI 校验正常路径必然选中；不在则弃用重建）。

锚定持久化契约：锚定 MUST 存于 `tasks.anchor_session_id` 显式列（替代「最近顶层 owned session 推导」现状）；「ClaimTaskSession + 设置 anchor」MUST 为单事务——claim 成功后 newID 立即成为权威锚定，跨 attempt/重启保留；claim 冲突时归属与 anchor 均不修改。`--session <id>` 的 id 一律读自 `tasks.anchor_session_id`。旧锚定条件清空（列表校验缺席时执行 `anchor_session_id=NULL WHERE task_id=? AND anchor_session_id=<old>`）的分派：store error → POST 前终态补偿；清空 Matched → 转无锚定流程继续；CAS mismatch（0 行匹配）→ MUST 复读：为 NULL 才继续，已出现新 anchor → 终止本次 bootstrap、按新锚定进入下一 attempt，MUST NOT 覆盖。既有数据 MUST 回填：schema 迁移时（或首次读取时惰性）按旧确定性排序（最近顶层 owned session）回填 `anchor_session_id`，仅处理 NULL 行。

术语继承：本 capability 其余 Requirement 中的「serve 进程」「serve 会话」「serve」一律指本 Requirement 定义的任务单进程及其 tmux 会话；端口分配、健康检查、能力探测、SSE 归属、连接管理等既有语义不变。

#### Scenario: 激活时启动单进程
- **WHEN** 任务被激活
- **THEN** 系统分配端口、生成随机密码并以 worktree 为 cwd 启动单进程会话，TUI 画面与 API 在同一进程就绪

#### Scenario: 同进程提供 API 与 TUI
- **WHEN** 单进程通过健康检查与能力探测
- **THEN** ocdeck-server 经 `http://127.0.0.1:<port>` 访问全部契约端点（/session、/event、/global/health 等），浏览器经 tmux attach 客户端接入同进程 TUI，两者互不影响

#### Scenario: 有锚定恢复指定会话
- **WHEN** 激活或重拉时任务存在已确认归属的锚定 session
- **THEN** 启动命令携带 `--session <anchoredSessionID>`；健康检查与能力探测通过后经会话列表校验锚定存在则确认，不存在则弃用旧锚定转无锚定流程

#### Scenario: 无锚定经 API 创建锚定
- **WHEN** 激活或重拉时任务无有效锚定
- **THEN** 进程就绪后经 `POST /session` 创建新 session，按响应 ID 原子 claim 为锚定；确认 bootstrap 进程终止后，正式进程启动（Recovery 路径另占预算 permit；Activate 路径沿其既有重试预算、不耗恢复 permit）并重新通过健康检查、能力探测与锚定存在校验，才标记活跃；claim 冲突时本次激活/恢复失败并记录 last_error

### Requirement: 任务进程自动重拉
系统 SHALL 在监视中发现任务进程会话异常消失（非 ocdeck 主动 kill 的挂起/删除/reconcile 路径，且 runtime token 匹配当前代）时自动重拉该任务进程。重拉 MUST 经 keyed mutex 内复读状态并 CAS `active→activating` 后执行；成功 CAS `activating→active`。被中断的进行中的 agent turn 接受丢失，会话状态经 opencode 磁盘持久化恢复。

**预算协议（固定）**：仅 Recovery 路径使用恢复预算——首次 Activate MUST NOT 使用恢复预算（无退避、不消耗 permit，沿用现有激活端口轮换重试预算）。预算记录 MUST 持久化于 store 层 per-task attempt 时间戳表。**AcquirePermit 协议**：permit 经原子写入 acquire 并返回 ordinal（窗口内第几个），写入成功后才按 ordinal 退避（ordinal 1/2/3 → 5s/15s/45s）；permit 写入 MUST 是 attempt 的首个动作（先于端口分配与任何进程副作用），写入失败属 store 不可约边界 → 终态补偿；退避取消（离开 activating/Shutdown/token 失效）时 permit 仍保留（已消耗，不清除）。滚动 5 分钟窗口内最多 3 个 permit，过期记录随窗口老化不参与计数；每次进程创建 MUST 对应一个已写入的 permit（含双启动子事务的第二次与端口轮换的新进程）；恢复成功 MUST NOT 立即清零记录；attempt 记录跨服务端重启保留并参与窗口计数。预算耗尽 MUST 进入终态补偿，MUST NOT 无限重试。

**逐阶段失败分派表**（attempt 内每个阶段的唯一定局；last_error 统一规则：终态补偿时聚合可得上下文——当前阶段错误 + 前序 attempt 末次错误，沿用激活路径聚合语义；「外层统一清理」指 TaskManager.Activate / ensureRecovery 的 defer 清理路径，与激活路径同一实现）：

| 阶段 | 失败 | 分派 | permit 已耗 |
|---|---|---|---|
| permit 写入 | store 不可写 | 终态补偿（store 不可约边界） | 否 |
| 端口分配 | 范围耗尽 | 终态补偿，cause=分配错误（last_error 最终按聚合规则生成） | 是 |
| AcquirePermit 窗口已满 | 预算耗尽 | 终态补偿，cause=`recovery budget exhausted` | 否（未新增 permit） |
| env 快照端口持久化 | 写失败 | 本次 attempt 失败（不创建进程），退避重试 | 是 |
| NewSession | 启动错误 | 本次 attempt 失败，退避重试 | 是 |
| 健康轮询 | 进程死亡/超时 | 按端口轮换策略（既有 fail-closed：清理+notice+换端口）；轮换新进程另耗 permit | 是 |
| 能力探测 | 任何失败 | 终态补偿（沿用「探测失败不轮换」） | 是 |
| bootstrap claim | 冲突/写失败 | 终态补偿 | 是 |
| 成功提交各步（token/group/watcher 注册、SSE 建立、对齐、last_port 写回） | 任一步失败 | 本次 attempt 失败（先做反向清理），退避重试 | 是 |
| CAS activating→active | 失配 | 成功提交段的 CAS 失配分派 | 是 |
| token 失效 | 任意阶段 | 只退出：零清理、零状态写入（固定会话名清理可能误杀新代 runtime） | 不定 |
| ctx 取消 / Shutdown | 任意阶段 | 仅当本 attempt token 仍拥有该 runtime 时执行反向清理，否则跳过；不执行状态写入（persist Shutdown 下存活进程由 reconcile 收敛） | 不定 |

**恢复流程分三段**（副作用边界与持久化顺序固定）：

*一次性前序（每个恢复 incident 执行一次）*：持锁后做 runtime token 校验 → CAS `active→activating` → 停止旧 SSE/watch → 清理该任务全部 shell 会话与残余 runtime 会话——出现 retryable cleanup debt MUST 直接进入终态补偿，MUST NOT 创建进程。

*可重复进程 attempt（预算内每次执行）*：分配端口（先上次端口）→ 将新端口持久化进 `tasks.env_snapshot.vars.OCDECK_SERVE_PORT`（持久化失败判定本次 attempt 失败，MUST NOT 创建会话）→ 以新随机密码创建进程 → 健康检查 + 能力探测 → 执行锚定 bootstrap（无锚定分支含双启动子事务，见「每任务单进程实例」）→ 成功提交。

*成功提交（标记活跃前 MUST 按序全部完成）*：正式进程 ready → 创建新 runtime token、注册 runtime group 与 watcher → 建立 SSE 订阅 + 全量对齐 → 健康检查通过后写回 `tasks.last_port` → CAS `activating→active`。token/group/watcher 注册后的任何失败 MUST 先 cancel/join SSE 与 watch、清理 runtime registry，再执行 HasSession/KillSession 与本次 attempt 失败/终态补偿分派。**CAS 失配分派**：CAS mismatch 后 MUST 复读任务状态与本 attempt token——最新状态为 active 且 token 仍属本 attempt → 视为幂等成功，结束执行器；否则 MUST 执行完整反向清理（cancel/join SSE 与 watch、清理 runtime registry、HasSession/KillSession）。此分支的 DB 禁写范围 MUST 收窄为 `status`/`last_error`/`env_snapshot`/`anchor` 四字段（以 DB 最新状态为准）；反向清理的 KillResult 按完整 disposition 表产生的 notice/debt 仍 MUST 持久化，持久化失败进入 tagged debt（`phase=cleanup_notice`）。

*统一终态补偿（预算耗尽 / retryable cleanup debt / claim 冲突 / 探测失败 / 提交失败共用）*：① MUST 先对该任务可能仍存活的 runtime 会话执行 HasSession/KillSession，按既有 fail-closed 映射处置：会话不存在或 clean → 直接执行终态事务；`snapshot_missing_degraded` → 记录 non-retryable notice 后执行终态事务；`reap_failed`/`snapshot_failed`/`kill_failed`/infra 错误/未知矛盾值 → 对应 retryable debt/notice **持久化成功后**执行终态事务，残余进程由后台周期任务接管；notice/debt 持久化失败 → MUST NOT 执行终态事务，按既有 pending cleanup/replay 合同处理；② MUST 经单个条件事务 `CompleteRecoveryFailure(expected=activating)` **原子**完成 `status=suspended` + `last_error=<cause>` + `env_snapshot=NULL`——CAS 不匹配（并发状态已变）时三个字段 MUST 均不修改，以 DB 最新状态为准。

*pending/replay 合同扩展至 Recovery*：既有 pending cleanup 机制（原仅用于激活失败路径）扩展覆盖恢复路径，pending 改为 tagged debt：`phase=cleanup_notice|complete`。`cleanup_notice` 变体字段：taskID + sessionName + cleanup tickets + reason + retryable + cause；`complete` 变体（无 cleanup notice 但 `CompleteRecoveryFailure` 未执行）仅字段：taskID + cause。重放入口：后台周期任务 + Shutdown/reconcile，按 phase 恢复：`cleanup_notice` → 重放 notice 持久化成功后 MUST 继续执行 `CompleteRecoveryFailure`；`complete` → 直接执行 `CompleteRecoveryFailure`。`CompleteRecoveryFailure` 自身写库失败 → 保留 debt（phase=complete），任务停留 activating 由后台重试驱动收敛（MUST NOT 静默）；CAS mismatch → 删除对应 debt，服从 DB 最新状态。

主动 kill（挂起、删除、kill 模式 reconcile）MUST NOT 触发自动重拉；activating 中的会话消失 MUST 由当前恢复执行器的健康轮询/端口轮换路径处理，监视器 MUST NOT 重复介入；Suspend 与恢复竞争锁时，先拿锁者胜——恢复先拿锁则 Suspend 收到 transitional-state 拒绝，Suspend 先拿锁则恢复放弃（Delete 只接受 suspended/archived/失败态，与 active 恢复无竞争，deleting 下 watcher no-op 仅为陈旧回调防御）。

#### Scenario: 用户退出 TUI 后自动重拉
- **WHEN** 用户在 TUI 中 exit/q 导致任务进程退出，监视检测到会话消失且任务为 active
- **THEN** 系统 CAS 进入 activating 并自动重拉，恢复锚定 session 后回到 active，重开终端即见会话当前状态

#### Scenario: 进程崩溃后自动重拉
- **WHEN** 任务进程异常崩溃，监视检测到会话消失
- **THEN** 系统按同一自动重拉路径恢复，「直接落挂起」的旧行为被移除

#### Scenario: 重拉预算耗尽落挂起
- **WHEN** 滚动窗口内进程创建尝试达到 3 次且均未成功
- **THEN** 任务 CAS 落为挂起并记录 last_error，不再重试

#### Scenario: 主动 kill 不触发重拉
- **WHEN** ocdeck 因挂起/删除/reconcile 主动 kill 任务进程会话
- **THEN** 监视不得将该消失判定为异常消失（状态非 active 或 token 不匹配），不触发自动重拉

#### Scenario: activating 中消失不重复介入
- **WHEN** 恢复执行器运行期间（任务 activating）watcher 再次发现会话消失
- **THEN** watcher 不做任何处理，由恢复执行器的健康轮询/端口轮换路径判定

#### Scenario: 恢复与挂起竞争
- **WHEN** watcher 触发重拉与用户发起挂起竞争同一任务锁
- **THEN** 挂起先拿锁则恢复因状态非 active 放弃；恢复先拿锁（已进入 activating）则挂起收到 transitional-state 拒绝

### Requirement: 终端重开与恢复中语义
ReopenAttach MUST NOT 创建独立 TUI 会话，MUST 按任务进程会话与任务状态分派：进程存活且任务 active → 返回 `-runtime` 会话的 terminal ID（attach 客户端由 WS 层 AttachPty 创建）；进程缺失且任务 active → 调用幂等 ensureRecovery（与 watcher 路径共用同一恢复执行器入口，重复调用 MUST NOT 产生第二个恢复流程）并返回 typed recovering；任务 activating → 返回同一 typed recovering，不重复启动；其他状态 → 返回 invalid state 错误。recovering 的错误契约固定为：application 错误码 `recovering`、HTTP 状态 409、WebSocket close code `1013`（Try Again Later）；MUST NOT 映射为 suspended 或既有非重试关闭码。前端识别 close code 1013 后轮询任务状态，回到 active 后重连终端；恢复期 UI 统一显示「进程启动中」。

#### Scenario: 进程存活时重开终端
- **WHEN** 用户打开终端页且任务进程存活、任务 active
- **THEN** 返回 `-runtime` 会话的 terminal ID，WS 接入即见 TUI 当前画面

#### Scenario: 进程缺失时重开触发幂等恢复
- **WHEN** 用户打开终端页且任务 active 但进程会话缺失
- **THEN** 触发幂等 ensureRecovery 并返回 typed recovering（HTTP 409）；WS 建连收到 close code 1013

#### Scenario: 恢复中重复请求不重复启动
- **WHEN** 任务处于 activating（恢复进行中）时 ReopenAttach 被再次调用（含 watcher 已触发恢复的情形）
- **THEN** 返回同一 typed recovering，不产生第二个恢复流程

#### Scenario: 非活跃状态拒绝重开
- **WHEN** 任务处于 suspended 等非活跃状态时调用 ReopenAttach
- **THEN** 返回 invalid state 错误，不触发恢复

### Requirement: 契约锚点扩展
opencode 契约锚点清单 SHALL 新增 2 个 TUI 侧文件（最终 23 个锚点）：`packages/opencode/src/cli/cmd/tui.ts`（external 分支判断逻辑）、`packages/opencode/src/cli/tui/worker.ts`（server 启动路径）。原已在清单内的 `cli/tui/validate-session.ts` MUST 显式标注其单进程相关性。`scripts/check-opencode-contract.sh` 的锚点数组与数量断言 MUST 同步更新；live probe MUST 改为 bare TUI external 启动流程（Basic Auth、health、session CRUD、status、SSE 首事件、`--session` 恢复与校验失败语义）。版本升级 SOP MUST 对新增锚点执行相同的相邻对 diff 核验；external 分支行为变化（如默认不再启动真实 server、API 面分叉、auth 语义变化）MUST 阻断区间扩展。

#### Scenario: 版本升级核验新锚点
- **WHEN** 执行契约区间扩展 SOP 跑锚点 diff
- **THEN** 全部 23 个锚点参与相邻对核验，有 DIFF 时按既有流程评估影响；live probe 以单进程 external 模式启动验证
