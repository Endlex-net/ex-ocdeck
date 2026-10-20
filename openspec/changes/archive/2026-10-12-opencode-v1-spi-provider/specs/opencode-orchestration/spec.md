## MODIFIED Requirements

### Requirement: session 归属捕获
系统 SHALL 订阅每个活跃任务进程的 SSE 事件流（`GET /event`）：`session.created` 事件的 sessionID 位于 `properties.info.id`（已验证契约区间 [ContractMinVersion, ContractBaseline]）；同时监听 `session.updated` 刷新 `last_seen_at`。激活后 MUST 按任务有效模式执行一次该 directory 的 session 对齐（worktree 模式任务执行目录级全量对齐，新 session 按 claim 语义补记；dir 任务与 repo 项目 local-path 模式任务执行 ownedOnly 对齐，MUST NOT 认领新 session）。SSE 断流时 MUST 指数退避重连，重连成功后 MUST 再次按同一有效模式对齐。

**session 所有权规则**：会话归属的唯一键为 SessionKey（provider 标识 + provider 实例标识 + 上游 sessionID，契约归属 agent-provider-spi「SessionKey 会话归属命名空间」）；本 capability 现有全部归属处于 `opencode-v1` 命名空间。一个 SessionKey 至多归属一个 ocdeck 任务（该约束适用于经本变更后合法写入口产生的新归属；历史遗留的重复归属行不做启动修复，随任务删除自然清理）；任务 MUST 仅对本任务拥有的 session（`task_sessions` 中本任务的行）执行删除、attach 与对齐写回。归属写入 MUST 统一经 store 层原子 claim（单事务内"仅当 SessionKey 未被其他任务拥有时插入/更新本任务行"），MUST NOT 以"先查询后 upsert"的非原子方式写归属。claim 冲突语义：SSE/对齐路径冲突 MUST 忽略该 session 并记服务端诊断日志（不阻断）；锚定创建路径冲突 MUST 使激活失败并记录 last_error，MUST NOT attach 不属本任务的 session。

`kind=dir` 项目的任务与 repo 项目 local-path 模式任务（目录可共享）MUST NOT 经目录级全量对齐认领新 session。dir 任务与 local-path 模式任务的对齐（ownedOnly）MUST 按以下顺序执行：① 按原始目录列表数量判定 complete/overflow（判定先于任何过滤）；② 候选集取"原始目录列表 ∩ 本任务当前 owned 集合"；③ complete 时在单个 store 事务内仅对候选集刷新 `last_seen_at`、仅删除本任务 owned 集合中的缺席行，并经事务内 noticeFn 清除既有 session_overflow notice；④ overflow 时不删任何缺席行，application 层 MUST 先经事务外 CAS 写入 session_overflow notice 再调对齐（对齐失败时 notice 保留，与 repo 现状逐点一致），仅刷新候选集。dir 任务与 local-path 模式任务的新 session 仅经本任务进程的 SSE 捕获（原子 claim）与锚定创建记录归属（SSE 断流期间经 TUI 新建的 session 不补记，为已接受的降级）。`session.updated` 事件 MUST 仅刷新本任务已归属行的 `last_seen_at`（条件更新，绝不插入新归属），未归属 session 的 updated 事件一律忽略。

有效模式解析 MUST 覆盖全部四个会建立 SSE/对齐/锚定的运行时入口：Activate、persist 重启恢复（resumeActive）、挂起失败的运行时修复（tryRepairRuntime）、自动重拉恢复（ensureRecovery，含 ReopenAttach 转发而来的恢复）；四者在任何状态修改或运行时副作用前 MUST 解析「项目 `kind` + 任务持久化 `mode`」得到有效模式，合法组合仅 (repo, worktree)、(repo, local-path)、(dir, local-path)，并按解析结果分流对齐模式（worktree 模式 → 目录级全量对齐；dir 任务与 repo 项目 local-path 模式任务 → ownedOnly 对齐）；dir+worktree、未知 kind、未知 mode MUST 报错且零副作用。恢复路径的锚定 claim 冲突 MUST 判定本次恢复失败并记录 last_error（计入重拉预算），MUST NOT 接入不属本任务的 session。

同目录双进程不串流是该 SSE 归属方案的前提，已经 OpenCode 源码验证（设计阶段完成）：`/event` 订阅的是进程内 listener（`server/routes/instance/httpapi/handlers/event.ts`），事件 publish 仅 notify 本进程 PubSub（`core/event.ts`），跨进程仅可经 `sync/history` 显式拉取；该架构自 v1.16.0 起连续稳定（v1.18.14 ↔ v1.18.18 锚点字节级一致；扩展区间须相邻对核验）。若未来 OpenCode 升级引入存储级事件分发，dir 任务与 repo 项目 local-path 模式任务归属 MUST 重新评审。

#### Scenario: TUI 新建会话被记录
- **WHEN** 用户在 TUI 中新建会话
- **THEN** 新 sessionID 经 SSE 被捕获并按 SessionKey 原子 claim 至本任务，用于后续恢复；若已被其他任务拥有则忽略并记诊断日志

#### Scenario: 断流后对齐
- **WHEN** repo 项目 worktree 模式任务 SSE 连接断开并恢复
- **THEN** 系统重连后全量对齐 session 列表，断流期间错过的会话被补记

#### Scenario: 同目录 dir 任务互不认领
- **WHEN** 同一 dir 项目下两个活跃任务 A/B（同一目录）各自执行全量对齐
- **THEN** 任务 A 的对齐仅核对自身 owned session，不认领任务 B 拥有的 session，反之亦然；目录中不属于任何任务的 session（如用户手工运行 opencode 产生）不被任何任务认领

#### Scenario: 同目录 dir 任务删除隔离
- **WHEN** 删除同一 dir 项目下的任务 A（任务 B 仍活跃）
- **THEN** 系统仅删除任务 A 拥有的 session；任务 B 的 session、锚定与对话状态不受影响

#### Scenario: dir 任务与 local-path 模式任务断流降级

- **WHEN** dir 任务或 repo 项目 local-path 模式任务 SSE 断流期间用户在 TUI 新建会话，随后重连并全量对齐
- **THEN** 该新会话不被补记进任务归属（与"他人/手工创建"无法区分），任务既有 session 的存在性核对与缺席清理语义不变

#### Scenario: 并发 claim 唯一归属
- **WHEN** 两个任务（如 SSE 与对齐并发）同时 claim 同一 SessionKey（同一上游 sessionID）
- **THEN** 原子 claim 仅一个成功，该 session 归属唯一任务；失败方按路径语义忽略/记诊断

#### Scenario: session.updated 不创建归属
- **WHEN** 任务收到未归属 session 的 session.updated 事件
- **THEN** 系统忽略该事件（条件更新未命中，不插入归属行、不报错）；已归属 session 的 updated 事件仅刷新 last_seen_at

#### Scenario: 挂起修复路径的有效模式解析

- **WHEN** dir 任务或 repo 项目 local-path 模式任务挂起失败后进入运行时修复（重建 SSE/对齐/锚定），或任务出现非法 kind/mode 组合（dir+worktree、未知 kind、未知 mode）
- **THEN** 修复路径在任何状态修改或运行时副作用前完成有效模式解析：dir 任务与 local-path 模式任务按 ownedOnly 模式对齐（不认领同目录他任务 session）；非法组合报错且零副作用

#### Scenario: 恢复路径的归属安全
- **WHEN** 任务进程自动重拉（含 ReopenAttach 转发），无锚定记录或预检 404 需创建新 session
- **THEN** 新 session 经原子 claim 归属本任务；claim 冲突时判定本次恢复失败并记录 last_error（计入重拉预算），MUST NOT 接入不属本任务的 session

#### Scenario: 同目录 local-path 任务互不认领

- **WHEN** 同一 repo 项目下两个 local-path 模式活跃任务 A/B（同一项目目录）各自执行全量对齐
- **THEN** 任务 A 的对齐仅核对自身 owned session，不认领任务 B 拥有的 session，反之亦然；项目目录中不属于任何任务的 session（如用户手工运行 opencode 产生）不被任何任务认领

#### Scenario: 同目录 local-path 任务删除隔离

- **WHEN** 删除同一 repo 项目下的 local-path 模式任务 A（同项目目录其他任务 B 仍活跃）
- **THEN** 系统仅删除任务 A 拥有的 session；任务 B 的 session、锚定与对话状态不受影响

#### Scenario: dir+worktree 非法组合 fail-closed

- **WHEN** 任一运行时入口（Activate、resumeActive、tryRepairRuntime、ensureRecovery）遇到持久化组合为 dir+worktree、未知 kind 或未知 mode 的任务
- **THEN** 该入口在任何状态修改或运行时副作用前报错且零副作用（不建立 SSE、不执行对齐、不做锚定、不修改任务状态）

### Requirement: serve HTTP 客户端连接管理
ocdeck-server 进程内经任务 provider 适配层 HTTP Client 发起的对任务进程的全部 HTTP 请求（健康检查、能力探测、状态查询、后台重试、SSE）SHALL 复用共享的 loopback HTTP transport，跨 Client 实例按 host:port 连接池复用，空闲连接 MUST 有回收上限；该 transport MUST 显式禁用代理（`Proxy: nil`）。对任务进程的全部 directory-scoped API 请求 MUST 显式携带 directory 参数。任务进程的连接端点与凭据 MUST 以持久化 TaskBinding 为事实来源解析（契约归属 agent-provider-spi「运行时托管与任务绑定生命周期」），MUST NOT 从进程环境变量反查。

#### Scenario: 新建 client 实例共享连接池
- **WHEN** 系统为状态查询/后台重试等路径创建新的 client 实例访问同一任务进程
- **THEN** 新实例复用共享 transport 的空闲连接，而非各自新建独立连接池

#### Scenario: 空闲连接有界回收
- **WHEN** 共享 transport 中的连接进入空闲状态
- **THEN** 空闲连接受 MaxIdleConns/IdleConnTimeout 上限约束并被按期回收，而非无限驻留

#### Scenario: 代理禁用语义保持
- **WHEN** 宿主环境设置了 HTTP_PROXY/HTTPS_PROXY
- **THEN** client 对任务进程的 loopback 请求仍直连 127.0.0.1，不经过代理

#### Scenario: 连接凭据按绑定解析
- **WHEN** 后台路径（attention 对账、permission 回复、agent status 等）需要连接任务进程
- **THEN** 连接端点读自该任务的持久化 TaskBinding，认证密码经确定性派生计算（规范归 agent-provider-spi「运行时托管与任务绑定生命周期」），而非读取 tmux 会话环境变量

### Requirement: 每任务单进程实例
系统 SHALL 为每个活跃任务启动单个 opencode 进程：`opencode --port <port> --hostname 127.0.0.1`（托管于命名 tmux 会话 `ocdeck-<taskID>-runtime`），工作目录为该任务的「任务运行目录」（worktree 模式任务为其持久化 `worktree_path`；`kind=dir` 任务与 repo 项目 local-path 模式任务为其 canonical 项目路径），并以确定性派生强密码（KDF(master_secret, provider_id, task_id)，同任务恒定；规范归 agent-provider-spi「运行时托管与任务绑定生命周期」）经会话 env（`new-session -e OPENCODE_SERVER_PASSWORD`）启用 Basic Auth，同时注入非秘密凭据来源标记（`new-session -e OCDECK_CRED_KIND=derived`，供迁移导入的来源判定，归 agent-provider-spi「迁移导入桥接」）。该进程经 `--port`/`--hostname` 进入 external 模式，在同一进程内同时提供 TUI（浏览器经 tmux attach 客户端接入）与完整 HTTP API/SSE 控制面（与 `opencode serve` 相同的 `Server.listen` 实现）。ocdeck MUST 作为该进程 API 的唯一访问入口；密码 MUST NOT 经 argv 传递。

锚定 session bootstrap MUST 采用确定性顺序协议（MUST NOT 依赖启动期 SSE `session.created` 事件——不可重放、与订阅建立有竞态、无法从并发事件中识别 bootstrap 事件）：

1. **启动**：有锚定 → 命令 MUST 携带 `--session <anchoredSessionID>`；无锚定 → MUST NOT 携带 `--session`
2. **就绪校验**：健康检查 + 能力探测通过后 MUST `GET /session?directory=` 取列表：有锚定且在列表中 → 锚定确认；有锚定但不在列表（已失效）→ 弃用旧锚定转无锚定流程
3. **无锚定创建**：MUST `POST /session?directory=` 创建新 session，**按响应 ID 原子 claim** 并写入锚定；claim 冲突 MUST 判定本次激活/恢复失败并记录 last_error（恢复场景计入重拉预算），MUST NOT 接入不属本任务的 session
4. **落到锚定（双启动子事务，仅新建锚定时；permit 子协议仅适用 Recovery 路径——首次 Activate 的双启动 MUST NOT 消耗恢复 permit、不执行恢复退避）**：① bootstrap 进程占用一个重拉预算 permit 并完成健康检查 + 能力探测；② `POST /session` + claim 后 MUST 按既有 KillResult/cleanup notice 规则确认 bootstrap 进程已终止，才可复用 `-runtime` 名称与端口；③ 正式进程占用**新的**预算 permit 并执行对应退避，端口复用已持久化值、密码按派生规则计算（同任务恒定）；④ 正式进程 MUST 重新执行健康检查 + 能力探测 + 锚定存在校验；⑤ 全部通过才进入成功提交；⑥ 预算窗口不足以取得第二个 permit 时，已 claim 的锚定 MUST 保留，本次尝试进入终态补偿
5. dir 项目任务与 repo 项目 local-path 模式任务 MUST NOT 经目录级对齐认领（ownedOnly 语义不变）

`--session` 失效的确定性分派：进程在 HTTP server 就绪前退出 → 健康轮询判死/超时 → 按本次尝试失败处理（既有错误分类）；进程就绪 → 列表校验是唯一正确性判据（锚定在列表中且 id 有效则 CLI 校验正常路径必然选中；不在则弃用重建）。

锚定持久化契约：锚定 MUST 存于 tasks 的锚定三元组显式列（`anchor_provider_id`、`anchor_provider_instance_id`、`anchor_session_id`，命名空间迁移归 agent-provider-spi 契约文档）；「ClaimTaskSession + 设置 anchor」MUST 为单事务——claim 成功后 newID 立即成为权威锚定，跨 attempt/重启保留；claim 冲突时归属与 anchor 均不修改。`--session <id>` 的 id 一律读自 `tasks.anchor_session_id`。旧锚定条件清空（列表校验缺席时执行锚定三元组清空（`...=NULL WHERE task_id=? AND anchor_session_id=<old>`））的分派：store error → POST 前终态补偿；清空 Matched → 转无锚定流程继续；CAS mismatch（0 行匹配）→ MUST 复读：为 NULL 才继续，已出现新 anchor → 终止本次 bootstrap、按新锚定进入下一 attempt，MUST NOT 覆盖。既有数据 MUST 回填：schema 迁移时按统一 v1 命名空间回填锚定三元组（归 agent-provider-spi「SessionKey 会话归属命名空间」），仅处理未回填行。

术语继承：本 capability 其余 Requirement 中的「serve 进程」「serve 会话」「serve」一律指本 Requirement 定义的任务单进程及其 tmux 会话；端口分配、健康检查、能力探测、SSE 归属、连接管理等既有语义不变。

#### Scenario: 激活时启动单进程
- **WHEN** 任务被激活
- **THEN** 系统分配端口、以派生密码生成凭据并以任务运行目录为 cwd 启动单进程会话（worktree 模式任务→持久化 worktree 路径；dir 任务与 repo 项目 local-path 模式任务→项目路径），TUI 画面与 API 在同一进程就绪

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

*可重复进程 attempt（预算内每次执行）*：分配端口（先上次端口）→ 将新端口持久化进 `tasks.env_snapshot.vars.OCDECK_SERVE_PORT`（持久化失败判定本次 attempt 失败，MUST NOT 创建会话）→ 以派生密码（同任务恒定，规范归 agent-provider-spi）创建进程 → 健康检查 + 能力探测 → 执行锚定 bootstrap（无锚定分支含双启动子事务，见「每任务单进程实例」）→ 成功提交。

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
