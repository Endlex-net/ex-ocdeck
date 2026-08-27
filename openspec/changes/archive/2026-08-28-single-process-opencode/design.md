# Design: single-process-opencode

## Context

现状（openspec/specs/opencode-orchestration/spec.md）：每任务两个 opencode 进程——`serve`（API+SSE 控制面，~400MB 基线）与 `attach`（TUI 显示器，~150-300MB），分别托管于 `ocdeck-<taskID>-serve` / `-tui` tmux 会话。内存实测（本机 opencode 1.18.18，Bun 编译单文件二进制）：空载 serve ~400MB、调优后 ~300MB；4 个活跃任务的 TUI 合计 ~730MB。

关键已验证事实（调研于 opencode 源码，v1.18.14–1.18.18 契约区间）：

- `packages/opencode/src/cli/cmd/tui.ts`：TUI 启动时判断 `external = hasArg("--port") || hasArg("--hostname") || network.mdns`；external 为真时经 `tui/worker.ts` 调用与 `opencode serve` 相同的 `Server.listen` 启动真实 HTTP server（API 面一致：`/session`、`/event` SSE、`/global/health`、web UI）；否则走 `http://opencode.internal` 进程内 RPC（无 TCP 端口）
- `packages/opencode/src/cli/network.ts`：`--port` 默认 0（随机）、`--hostname` 默认 127.0.0.1
- `packages/opencode/src/server/auth.ts`：`OPENCODE_SERVER_PASSWORD` Basic Auth 仅 external 模式生效
- 顶层 CLI 实测支持 `-s, --session <id>` / `--continue` / `--fork`
- SSE 事件仅 notify 本进程 PubSub（任务仍各自独立进程，归属前提不变）

约束：浏览器终端链路（xterm.js + WS + tmux attach 客户端）不变；端口分配/轮换 fail-closed 语义不变；SSE 归属与原子 claim 不变；shutdownPolicy 三档与启动 reconciliation 框架不变。

## Goals / Non-Goals

**Goals:**

- 每任务从双进程降为单进程（`opencode --port <p> --hostname 127.0.0.1`），消除 attach 进程内存（每任务省 ~115-300MB）
- 进程退出（TUI exit / 崩溃）后自动重拉并恢复锚定 session，任务保持活跃；重拉带固定预算算法与退避，耗尽落挂起
- 直接替换双进程模式，不留配置开关；契约锚点扩展覆盖新依赖的 TUI 侧路径

**Non-Goals:**

- idle TUI 回收 / LRU 保留策略（单进程后 TUI 即进程本体，不再适用；如未来需要另起 change，且必须引入显式持久化意图而非 watcher 特判）
- serve 内嵌 web UI 接入浏览器（独立候选方向，本 change 不评估）
- 单 serve 多任务复用（违背 env 注入与 SSE 归属前提，已否决）
- opencode 契约区间扩展（版本门禁逻辑不变）

## Decisions

### D1 单进程命令形态

激活时在 `ocdeck-<taskID>-runtime` 会话启动 `opencode --port <port> --hostname 127.0.0.1`（有锚定时追加 `--session <anchoredID>`，见 D5），cwd=worktree，env 注入 `OPENCODE_SERVER_PASSWORD`（`new-session -e`，禁止 argv）。健康检查、能力探测、SSE 订阅、结构化状态查询的调用点全部不变——目标端口与认证语义与 serve 模式一致。

- 备选：维持 serve+attach（内存代价不可接受）；bare TUI 进程内 RPC（无 TCP 控制面，ocdeck 全部 API 依赖落空）。均否决。

### D2 tmux 角色模型与双枚举

会话命名 `ocdeck-<taskID>-<suffix>`，运行期 suffix 收敛为 `runtime | shell-<n>`（n 为正整数）。两层枚举明确拆分：

- **RuntimeGroup.Role ∈ {runtime, shell}**（internal/task/manager.go 的 runtimeGroup 结构）
- **进程层名字白名单拆分**（internal/infrastructure/process/process.go 的 sessionNameRe / ValidateSessionName）：`NewSession` 只接受 `runtime | shell-<n>`，拒绝 legacy `serve/tui`；`HasSession`/`KillSession`/watch 等管理/清理路径同时接受 legacy 后缀（旧会话必须可清理、不可新建）
- `internal/task/util.go` 的 `roleFromSessionName`/`taskIDFromSessionName` 解析器同时识别新后缀与 legacy `serve/tui` 后缀（后者仅供 reconcile 遗留清理分组）

### D3 自动重拉：状态机 + 固定预算算法 + attempt 事务

**状态机**（DR-01：恢复期间状态 = activating，用户已决策）：

| 触发 | 条件 | 动作 |
|---|---|---|
| watcher 发现 runtime 会话消失 | 任务 active 且 runtime token 匹配当前代 | keyed mutex 内复读状态，CAS `active→activating`，启动恢复执行器 |
| watcher 发现 runtime 会话消失 | 任务 activating | no-op——由当前恢复执行器的健康轮询/端口轮换路径处理，监视器不重复介入 |
| watcher 发现 runtime 会话消失 | suspending/deleting 或 token 不匹配 | no-op（主动 kill 或陈旧回调；deleting 分支仅为防御——Delete 只接受 suspended/archived/失败态，与 active 恢复无竞争） |
| Suspend 与恢复竞争锁 | Suspend 先拿锁 | 恢复放弃（token 失效即停） |
| 同上 | 恢复先拿锁（已进入 activating） | Suspend 收到 transitional-state 拒绝 |
| 恢复成功 | — | CAS `activating→active` |
| 预算耗尽 | — | CAS `activating→suspended` + last_error |

**预算协议**（固定，不再是开放参数）：

- 适用面：仅 Recovery（自动重拉）路径使用恢复预算；首次 Activate 不使用恢复预算（无退避、不消耗 permit，沿用现有激活端口轮换重试预算）
- 记录落点：store 层新增 per-task 持久化 attempt 时间戳表；每 attempt 开始时（首个动作，先于端口分配与任何进程副作用）原子写入一条记录即消耗一个 permit；写入失败属 store 不可约边界 → 终态补偿（last_error 记录尝试）
- 窗口：滚动 5 分钟内最多 3 个 permit；过期记录随窗口老化不参与计数（惰性裁剪）
- **AcquirePermit 协议**：permit 经原子写入 acquire 并返回 ordinal（窗口内第几个）；写入成功后才按 ordinal 退避（ordinal 1/2/3 → 5s/15s/45s）；退避取消（离开 activating/Shutdown/token 失效）时 permit 仍保留（已消耗，不清除）
- 计次：每次 `NewSession(runtime)` 必须对应一个已写入的 permit（含双启动子事务的第二次、端口轮换的新进程）；窗口不足以取得 permit → 终态补偿（D5 无锚定分支已 claim 的锚定保留）
- **permit 子协议仅适用 Recovery**：首次 Activate（含其无锚定双启动）MUST NOT 消耗恢复 permit、不执行恢复退避，沿用现有激活端口轮换重试预算
- 取消：退避 timer 在任务离开 activating、Shutdown 或 token 失效时取消
- 跨重启：attempt 记录持久化，服务端重启后仍参与窗口计数；恢复成功不立即清零，仅随窗口老化
- 实现：注入 clock 的 recovery policy 组件，确定性测试覆盖窗口老化与计时取消

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
| 成功提交各步（token/group/watcher 注册、SSE 建立、对齐、last_port 写回） | 任一步失败 | 本次 attempt 失败（先做 F-21 反向清理），退避重试 | 是 |
| CAS activating→active | 失配 | 成功提交段的 CAS 失配分派 | 是 |
| token 失效 | 任意阶段 | 只退出：零清理、零状态写入（固定会话名清理可能误杀新代 runtime） | 不定 |
| ctx 取消 / Shutdown | 任意阶段 | 仅当本 attempt token 仍拥有该 runtime 时执行反向清理，否则跳过；不执行状态写入（persist Shutdown 下存活进程由 reconcile 收敛） | 不定 |

**恢复流程分三段**（副作用边界与持久化顺序逐字段固定）：

*一次性前序（prelude，每个恢复 incident 只执行一次）*：持锁后做 runtime token 校验（基础设施代际概念，不进领域层）；CAS `active→activating`；停止旧 SSE/watch；清理该任务全部 shell 会话与残余 runtime 会话——出现 retryable cleanup debt 直接进入终态补偿，MUST NOT 创建进程。

*可重复进程 attempt（预算内每次执行）*：分配端口（先上次端口）→ 将新端口持久化进 `tasks.env_snapshot.vars.OCDECK_SERVE_PORT`（持久化失败判定本次 attempt 失败，MUST NOT 创建会话）→ 以新随机密码 `NewSession(runtime)` → 健康检查 + 能力探测 → 执行 D5 锚定 bootstrap（含双启动子事务，见 D5）→ 成功提交。

*双启动子事务（仅 D5 无锚定分支、仅 Recovery 路径，作为 attempt 内的子协议；首次 Activate 的双启动不占恢复 permit、不执行恢复退避）*：① bootstrap 进程占用一个预算 permit 并完成健康检查 + 能力探测；② `POST /session` + claim 后，MUST 按既有 KillResult/cleanup notice 规则确认 bootstrap 进程已终止，才可复用 `-runtime` 名称与端口；③ 正式进程占用**新的**预算 permit 并执行对应退避，端口复用已持久化值、密码重新生成（每次 NewSession 新密码不变）；④ 正式进程 MUST 重新执行健康检查 + 能力探测 + 锚定存在校验（`GET /session` 列表含新锚定）；⑤ 全部通过才进入成功提交；⑥ 预算窗口不足以取得第二个 permit 时，已 claim 的锚定保留，本次 attempt 进入终态补偿。

*成功提交（标记活跃前 MUST 按序全部完成）*：正式进程 ready → 创建新 runtime token、注册 runtime group 与 watcher → 建立 SSE 订阅 + 全量对齐 → 健康检查通过后写回 `tasks.last_port` → CAS `activating→active`。token/group/watcher 注册后的任何失败 MUST 先 cancel/join SSE 与 watch、清理 runtime registry，再执行 HasSession/KillSession 与本次 attempt 失败/终态补偿分派。**CAS 失配分派**：CAS mismatch 后 MUST 复读任务状态与本 attempt token——最新状态为 active 且 token 仍属本 attempt → 视为幂等成功，结束执行器；否则 MUST 执行完整反向清理（cancel/join SSE 与 watch、清理 runtime registry、HasSession/KillSession）。此分支的 DB 禁写范围 MUST 收窄为 `status`/`last_error`/`env_snapshot`/`anchor` 四字段（以 DB 最新状态为准）；反向清理的 KillResult 按完整 disposition 表产生的 notice/debt 仍 MUST 持久化，持久化失败进入 tagged debt（`phase=cleanup_notice`）。

*统一终态补偿（所有终态失败共用：预算耗尽 / retryable cleanup debt / claim 冲突 / 探测失败 / 提交失败）*：① **先执行运行时清理**——对该任务可能仍存活的 runtime 会话执行 HasSession/KillSession，按既有 fail-closed 映射的完整 disposition 表处置：

| KillResult disposition | SessionKilled 一致性 | notice reason | retryable | 是否执行终态事务 |
|---|---|---|---|---|
| 会话已不存在（absent） | — | 不记 notice | — | 执行 |
| clean | true | 不记 notice | — | 执行 |
| snapshot_missing_degraded | 会话消失 | snapshot_missing_degraded | 否（已接受丢失） | 执行 |
| reap_failed / snapshot_failed / kill_failed | 不得矛盾 | 对应 reason | 是 | **debt/notice 持久化成功后**执行，残余进程由后台周期任务接管 |
| KillSession infra 错误 | — | kill_failed | 是 | **debt/notice 持久化成功后**执行，残余进程由后台接管 |
| 未知/矛盾 disposition | 矛盾 | kill_failed | 是 | **debt/notice 持久化成功后**执行 |
| notice/debt 持久化失败（任意分支） | — | — | — | **不得执行** `CompleteRecoveryFailure`，按既有 pending cleanup/replay 合同处理 |

② 随后经单个条件事务 `CompleteRecoveryFailure(expected=activating)` **原子**完成 `status=suspended` + `last_error=<cause>` + `env_snapshot=NULL`——CAS 不匹配（并发状态已变）时三个字段 MUST 均不修改，以 DB 最新状态为准。

**pending/replay 合同扩展至 Recovery**：既有 pending cleanup 机制（原仅用于激活失败路径）扩展覆盖恢复路径，pending 改为 tagged debt：`phase=cleanup_notice|complete`。`cleanup_notice` 变体字段：taskID + sessionName + cleanup tickets + reason + retryable + cause；`complete` 变体（无 cleanup notice 但 `CompleteRecoveryFailure` 未执行）仅字段：taskID + cause。重放入口：后台周期任务 + Shutdown/reconcile，按 phase 恢复：`cleanup_notice` → 重放 notice 持久化成功后 MUST 继续执行 `CompleteRecoveryFailure`；`complete` → 直接执行 `CompleteRecoveryFailure`。`CompleteRecoveryFailure` 自身写库失败 → 保留 debt（phase=complete），任务停留 activating 由后台重试驱动收敛（MUST NOT 静默）；CAS mismatch → 删除对应 debt，服从 DB 最新状态。

领域层新增 `ApplyRecoveryStart`（只表达 active→activating 及任务领域 guard，不含 runtime token——token 校验在 Manager/ensureRecovery 持锁后完成）等方法；迁移矩阵以 internal/domain/task/task.go 现有 ApplyActivate/ApplyActivateCommit 模式为准。

### D4 主动 kill 与异常消失的区分

挂起/删除/kill 模式 reconcile 在 kill 前已持久化意图（suspending 状态、删除标记、reconcile 模式）；watcher 仅在任务 active 且无进行中生命周期操作、token 匹配时把会话消失判定为「异常消失」（D3 状态表）。

**SSE 永久失败的统一分派**：SSE 流永久返回（非主动 cancel，如进程退出导致 EOF）与 watcher 发现会话消失 MUST 走同一入口——幂等 `ensureRecovery`（统一 runtime failure 分派），替代现有 `convergeToSuspendedForGen` 直接落挂起路径（internal/task/activate.go SubscribeEvents 永久返回处）。SSE EOF 可能先于 tmux watcher 到达；若进程仍存活但已失去控制面，ensureRecovery 前序清理会终止该 runtime。两个入口经同一 keyed mutex + token + 幂等保证不产生并发双恢复。

### D5 锚定 session bootstrap（确定性顺序协议，无 SSE 竞态）

单进程未启动时 API 不存在，无法「先经 API 创建 session 再带 id 启动」。锚定恢复 MUST NOT 依赖启动期 SSE `session.created` 事件（不可重放、与订阅建立存在竞态、无法从并发事件中识别 bootstrap 事件），统一采用 API 可观测的顺序协议：

1. **启动**：有锚定 → 命令携带 `--session <anchoredID>`；无锚定 → 不带 `--session`
2. **就绪**：健康检查 + 能力探测通过后，`GET /session?directory=` 取列表
3. **校验/创建**：
   - 有锚定且锚定在列表中 → 锚定确认，转步骤 5
   - 有锚定但不在列表（已被删除/失效）→ 弃用旧锚定，转无锚定流程
   - 无锚定 → `POST /session?directory=` 创建新 session，**按响应 ID 原子 claim** 并写入锚定
4. **落到锚定**（仅新建锚定时）：以 `--session <newID>` 重启进程（一次性双启动，仅锚定创建时发生；重拉/后续激活均直达步骤 1 的有锚定分支）
5. **提交**：SSE 订阅 + 全量对齐，任务标记活跃

正确性兜底来自 API 会话列表（可重放、可校验），与 TUI 端实际打开了哪个 session 解耦；claim 冲突 MUST 判定本次激活/恢复失败并记录 last_error（恢复场景计入重拉预算），MUST NOT 接入不属本任务的 session；dir 项目 ownedOnly 对齐语义不变。

`--session` 失效的确定性分派（不留给实现判断）：

- 进程在 HTTP server 就绪前退出（如 CLI 校验报错即退出）→ 健康轮询判死/超时 → 按本次 attempt 失败处理（既有错误分类），随后按预算退避重试或终态补偿
- 进程就绪 → 列表校验是唯一正确性判据：锚定在列表中且 `--session` 为有效 id → validate-session 正常路径必然选中它（TUI 选中其他 session 的唯一前提是锚定已失效，而失效必被列表校验发现）；锚定不在列表 → 弃用重建

**锚定持久化契约**：新增 `tasks.anchor_session_id` 列（schema 迁移），替代现状「最近顶层 owned session 推导」（store `ListTopLevelTaskSessions`，queries.go）。规则：「ClaimTaskSession + 设置 anchor」MUST 为单事务——claim 成功后 newID 立即成为权威锚定，跨 attempt/重启保留；claim 冲突时归属与 anchor 均不修改。`--session <id>` 的 id 一律读自 `tasks.anchor_session_id`。

旧锚定条件清空的分派（列表校验缺席时执行条件清空 `anchor_session_id=NULL WHERE task_id=? AND anchor_session_id=<old>`）：store error → POST 前终态补偿；清空 Matched → 转无锚定流程继续；CAS mismatch（0 行匹配）→ MUST 复读：为 NULL 才继续无锚定流程；已出现新 anchor → 进入下一 attempt 前 MUST 按 KillResult disposition 表确认当前 bootstrap 进程已终止（retryable debt 或 notice 写失败转终态/pending，不得复用 `-runtime` 名称与端口），然后按新锚定进入下一 attempt（Recovery 另占 permit；Activate 沿其既有重试预算），MUST NOT 覆盖新 anchor。

既有数据回填：schema 迁移时（或首次读取时惰性）按旧确定性排序（最近顶层 owned session，ListTopLevelTaskSessions 同款排序）回填 `anchor_session_id`，仅处理 NULL 行；配套多 session 数据迁移测试与回滚验证。

**Spike 先行**（tasks 第 1 节首项），验证两项契约行为并把结论写回 CONTRACT.md：① `--session` 指向不存在 id 时 TUI 的确切行为（报错退出 / 静默回退 / 新建）——对应上述两条分派分支的实际触发路径 ② external 模式下 health / session CRUD / status / SSE 首事件 / Basic Auth 与 serve 的逐点一致性。spike 结论与本协议冲突时，回本 change 更新文档再实现。

### D6 契约锚点扩展

CONTRACT.md 锚点清单**新增 2 个** TUI 侧文件（最终 23 个）：`cli/cmd/tui.ts`（external 分支判断）、`cli/tui/worker.ts`（server 启动路径）。`cli/tui/validate-session.ts` 原已在锚点清单内，显式标注其单进程相关性（锚点数量不变）。`scripts/check-opencode-contract.sh` 的 ANCHORS 数组与数量断言同步更新；live probe checklist 改为 D5 spike 验证过的 bare TUI external 启动流程（含 Basic Auth、health、session CRUD、status、SSE 首事件、`--session` 恢复与校验失败语义）。external 分支行为变化（默认不启动真实 server、API 面分叉、auth 语义变化）MUST 阻断区间扩展。

### D7 部署迁移：不支持热迁移

升级前挂起全部任务；新版启动时 reconcile 将旧版 `-serve`/`-tui` 会话一律按异常会话清理（无论其 taskID 是否有 DB 行），任务以单进程模型重新激活。不做双进程运行时的兼容恢复。

persist reconcile 的恢复边界收紧：仅 **active** 任务的存活健康进程可原地 resume；**activating** 一律视为被中断的激活/恢复——执行清理并落 suspended。原因：单进程无锚定 bootstrap 的中间态（bootstrap 进程未锚定、双启动未完成）无法经 tmux 会话名区分，原地 resume 可能把未锚定进程错误提升为 active；不持久化 phase 字段，统一走「清理 + 挂起 + 用户重新激活」的最简路径。

- 备选：热迁移/兼容恢复旧双会话（用户已否：「不用处理，我会提前全部关掉」）；持久化 phase/permit 状态按子事务续跑（复杂度不值，个人规模重启极少）。

### D8 ReopenAttach 重定义（幂等恢复入口）

`internal/task/attach_shell.go` 的 ReopenAttach 不再创建独立 TUI 会话（attach 客户端由 WS 层 `AttachPty` 创建，internal/api/ws_terminal.go）：

| runtime 会话状态 | 任务状态 | 行为 |
|---|---|---|
| 存活 | active | 返回 `-runtime` 会话的 terminal ID，WS 接入即可 |
| 缺失 | active | 调用幂等 `ensureRecovery`（与 watcher 路径共用同一恢复执行器入口，重复调用不产生第二个恢复流程），返回 typed `recovering` |
| 任意 | activating | 返回同一 typed `recovering`，不重复启动 |
| 任意 | 其他状态 | 返回 invalid state 错误 |

WS 层把 `recovering` 映射为固定契约：application 错误码 `recovering`，HTTP 状态 409，WebSocket close code **1013**（Try Again Later）；该错误 MUST NOT 映射为 suspended 或既有的非重试关闭码。前端识别 close code 1013 → 轮询任务状态，回到 active 后重连终端。恢复期 UI 统一显示「进程启动中」，不新增原因字段。

## Risks / Trade-offs

- [用户在 TUI exit 杀死进行中的 agent turn] → 自动重拉恢复会话（用户已接受 turn 丢失）
- [external 分支是 TUI 启动的次要代码路径，上游重构风险高于 serve 命令] → 契约锚点 + 相邻对核验阻断区间扩展；能力探测作为激活门禁兜底
- [无 `--session` 分支可能打开最近 session 而非新建（spike 未跑前不确定）] → D5 spike 先行，结论冲突则改流程并更新文档
- [自动重拉热循环（配置损坏/端口全占用）] → D3 固定预算算法 + 耗尽落挂起
- [重拉与端口占用竞争] → 复用既有端口轮换 fail-closed 语义
- [web UI（GET /）随单进程暴露在端口上] → 仅监听 127.0.0.1 + 随机强密码 Basic Auth，ocdeck 唯一入口原则不变
- [watcher 与 WS 入口同时发现 runtime 缺失启动两套恢复] → ensureRecovery 幂等 + keyed mutex + token 匹配

## Migration Plan

1. 升级前：挂起全部任务（部署前提，写入 release note）
2. 部署新版服务端
3. 启动 reconcile 清理旧版 `-serve`/`-tui` 会话（含 persist 模式存活会话）
4. 任务手动激活，进入单进程模型
5. 回滚：先挂起全部任务 → 停止新版 ocdeck-server → 清理专属 socket → 再部署旧版。旧版对 `-runtime` 未知 role 不按孤儿清理（tasks 6.2）；生产 socket 在 `TMUX_TMPDIR=<dataDir>/tmux`（默认 dataDir=`$HOME/.ocdeck`，自定义来源 `OCDECK_DATA_DIR`），命令为 `TMUX_TMPDIR="${OCDECK_DATA_DIR:-$HOME/.ocdeck}/tmux" tmux -L ocdeck -f /dev/null kill-server`。服务仍运行时 kill-server 会触发 watcher 自动重拉，不得跳过前两步。

## Open Questions

- D5 spike 的两个待验证行为（`--session` 失效的 TUI 表现、external 模式 API 逐点一致性）；其余设计决策均已固化
