# Design: fix-serve-connection-churn

## Context

2026-08-11 生产故障：17:06–18:21 所有新 task 激活失败，重启 ocdeck-server 后恢复。reconcile 日志显示对存活 serve 的健康检查 dial 报 `connect: can't assign requested address`（EADDRNOTAVAIL，loopback 临时端口耗尽）。

当前实现（均已核对原文）：

- `internal/opencode/client.go` `NewClient`：每个 Client 实例新建 `loopbackTransport := &http.Transport{Proxy: nil}`，无跨实例连接池。调用点：`activate.go:577`（健康轮询 client，500ms 间隔内复用同一 client）、`activate.go:651`（SSE client）、`reconcile.go:279`（resume 健康检查）、`agent_status.go:44`（每次 AgentStatus 查询新建）、`attention.go`（后台重试）。零值 `http.Transport` 的 `MaxIdleConns`/`IdleConnTimeout` 为 0 = 无上限/不回收；`http.DefaultTransport` 才有 `MaxIdleConns=100`、`IdleConnTimeout=90s`、DialContext 30s 超时等显式配置（net/http transport.go）。
- `internal/task/activate.go` `startServeWithPortRetry`（activate.go:564-614）：健康检查失败 → `_, _ = m.proc.KillSession(serveName)`（KillResult 与错误均丢弃）→ `allocatePort(lastPort=旧端口)` → 旧端口刚释放立即可绑定 → `newPort == port` → 终态报错 `serve not ready`，`servePortRetries=3` 第一次即退出。次生缺陷：最后一次迭代失败后仍 allocate + persist 幽灵端口；allocate 失败分支（activate.go:587）误包健康检查 err 而非 aerr；wait 返回 `ctx.Err()` 后仍继续 kill + allocate；Probe 失败处（activate.go:607）同样丢弃 KillResult——若 reap_failed，首次 tickets 丢失且外层 compensation 因会话已不存在无法重建。
- `internal/process/reaper.go` `KillSession`（reaper.go:282-334）：除会话名校验外恒返回 `nil error`，语义由 `KillResult.Disposition` 承载——`clean` / `retryable_reap_failed`（SessionKilled=true，逃逸收割未净，tickets）/ `retryable_snapshot_failed`（会话存活，未执行 kill）/ `retryable_kill_failed`（会话存活，kill 失败，tickets）/ `snapshot_missing_degraded`（会话已消失，不可重试、已接受丢失）。**"无错误"不等于"会话已终止"**。
- cleanup notice 责任在 task 层：process 层只返回 `CleanupTickets`；task 层经 `recordResidualNoticeFromDisposition`（notice.go:253-261，clean 不记；五种已知 disposition 中其余必记；**未知 disposition 由 `dispositionToNotice` 返回 ok=false、helper 直接返回 nil 不记录**——故 default/未知分支 MUST NOT 依赖该 helper，必须显式调 `recordResidualNotice` 按 kill_failed 记录）或 `recordResidualNotice`（kill infra 错误时记 retryable kill_failed）持久化。现有调用范式见 activate.go:1234-1243、delete.go:482-486、suspend.go:180-190。后台 notice 处理遇**当前 runtime 同名会话**时整项跳过（notice.go:392）——即 active 任务持有的 retryable tickets 不会被 RetryReap。
- `waitServeReadyOrDead`（activate.go:445-464）：进程死亡（`HasSession=false`）与健康超时返回不同文案的 `fmt.Errorf`，无结构化区分。
- `allocatePort`（activate.go:178-208）：lastPort 优先 + 轮转游标线性扫描，无排除机制。
- `persistEnvSnapshot` 失败 → 终态不建会话的边界已存在（activate.go:596-597），本 change 保留。
- 外层统一补偿已存在：`activateRun` 返回错误后 `runActivateFailureCompensation`（activate.go:288）以 `context.WithoutCancel` + 30s（cleanup）/10s（DB 收敛）分段预算执行 kill 已建会话 + residual notice 持久化 + 清 env 快照 + 状态回退 suspended + last_error 聚合（activate.go:331-360）。

## Goals / Non-Goals

**Goals:**

- 消除 serve HTTP 短连接 churn：跨 Client 实例共享 loopback transport，连接池复用且空闲连接有界回收。
- 激活重试闭环：serve 未通过健康检查时后续尝试使用不同端口；重试副作用（kill/notice → allocate → persist → NewSession）按确认门禁、预算与 ctx 排序；无幽灵端口、无单点丢失 notice/tickets（本地写失败经 pending cleanup handoff 由外层补偿重试，见 D2；store 在整个激活窗口持续不可写为不可约边界）、无未确认清理；active 任务不携带 retryable cleanup debt。
- `Proxy: nil` 作为显式不变量保留。

**Non-Goals:**

- probe/健康检查错误细节保留（`ErrServeNotReady` 吞掉底层 dial 错误）。
- 激活失败的 UI 可见性（创建接口 201 + 异步激活失败的静默问题）。delta scenarios 中「展示错误」「提示兼容性问题」为主 spec 继承基线（前端既有 last_error 展示机制），本 change 不修改、不新增 UI 验收；本 change 验收边界 = suspended + last_error + 错误分类。
- SSE 重连策略调整（无限重连保留，见 D3）。
- 创建接口的同步/异步语义；TUI `opencode attach` 独立进程的连接行为；Probe 失败的错误分类调整；除 pending handoff 接收与补偿路径 notice 写失败重试外，外层 compensation 与 notice 后台处理逻辑的既有语义不在本 change 改动范围。

## Decisions

### D1: 包级共享 loopback Transport（clone DefaultTransport）

`internal/opencode` 包级单例：`http.DefaultTransport.(*http.Transport).Clone()` 后置 `Proxy = nil`。`NewClient` 的 `httpClient` 与 `sseClient` 均绑定该单例。各 Client 保留独立 `http.Client`（不同调用点 `OpTimeout` 不同：3s/5s/10s），共享的是 Transport（连接池按 host:port 复用）。

**为什么 clone DefaultTransport 而非零值构造**：零值 `http.Transport` 的 `MaxIdleConns=0`（无上限）、`IdleConnTimeout=0`（空闲连接永不回收）、无 DialContext 超时配置；包级单例会把这些差异放大到进程整个生命周期。clone 继承经过生产验证的参数，仅覆盖 `Proxy`。

**容量模型**：`MaxIdleConnsPerHost=2`（clone 默认）× 活跃 serve 数 = idle 峰值需求。ocdeck 是单用户本地编排工具，同时 active 任务量级为数十；`MaxIdleConns=100` 覆盖 ~50 个活跃 serve 的 idle 峰值。即使极端超过（理论上限 1000 端口），后果仅是部分 idle 连接被驱逐、退化为按需新建——正确性不受影响，但 churn 回升属明示接受的残余可用性风险（见 Risks）；热点 host（正被轮询/流式的任务）始终享受复用。验收补充多 host（≥2 个 host:port）两轮访问的连接计数测试。

**为什么保留 `Proxy: nil`**：显式不变量，防 Go 版本/运行环境代理语义漂移（Go ≥1.16 的 `ProxyFromEnvironment` 已豁免 loopback；现有 client.go 中"ProxyFromEnvironment 不豁免 127.0.0.1"的注释已过时，实施时同步修正）。

**为什么包级单例而非经 ocFactory 注入**：所有 Client 均为 loopback-only，无按任务区分 transport 的需求；注入需改 `ocFactory` 签名与全部调用点，收益为零。

### D2: 重试闭环修复（端口排除 + 确认门禁 + 预算检查 + ctx 短路 + Probe 清理委托）

`allocatePort(lastPort sql.NullInt64, exclude int)`：lastPort 快路径与轮转扫描均跳过 `exclude`（0 = 不排除）。

`waitServeReadyOrDead` 失败原因结构化：进程死亡分支包装哨兵错误（如 `errServeSessionDied`），调用方用 `errors.Is` 区分「会话已死亡」与「健康超时」。

**active 任务 MUST NOT 携带 retryable cleanup debt**：后台 notice 处理遇当前 runtime 同名会话时整项跳过（notice.go:392），若 reap_failed/snapshot_failed/kill_failed 后继续激活，旧代 tickets 会在任务整个 active 期间无法 RetryReap（违反主 spec 后台周期重试语义）。故所有 **retryable** 非 clean disposition → 记 notice 后**终态**；suspended 状态下无 runtime 冲突，后台即可自由重试收割。`snapshot_missing_degraded` 不可重试（已接受丢失），不产生 debt，记 notice 后可继续。

**Probe 失败统一委托外层补偿**：`startServeWithPortRetry` 内任何 Probe 失败不再本地 kill（修复 activate.go:607 丢弃 KillResult 丢 tickets 的问题），直接返回错误；外层 `runActivateFailureCompensation`（detached ctx）统一执行 kill + notice 持久化。Probe 期间 ctx 取消与 Probe 其他失败走同一路径。**错误聚合边界**：Probe 失败返回的错误 = 探测错误本身（分类保持现状，不附加 kill/notice 上下文）；外层 compensation 的 kill/notice/回放结果只聚合进 DB last_error（activate.go:347-351），MUST NOT 改变 Activate 的返回错误（activate.go:288-295 返回原始 err）。

**轮换路径 notice 写用脱离取消的有界 ctx**：KillSession 单次可达 30s，期间请求 ctx 可能已取消；notice 持久化用 `context.WithoutCancel(ctx)` + 有界超时（**预算 10s**，实现可新增命名常量——本地写为单个有界 CAS 收敛流程，10s 足够且不为热路径引入 30s 级附加延迟）。**notice 写失败（`nerr != nil`）→ 立即终态**，MUST NOT allocate/persist/NewSession，且 MUST 经 pending cleanup handoff 移交（见下），不得静默丢弃 tickets。

**Pending cleanup handoff（tickets 不丢的闭环）**：`startServeWithPortRetry` 只返回 `(int, error)`，而外层 `cleanupActivationRuntime` 对 `HasSession=false` 的会话直接跳过（activate.go:1230-1232）——reap_failed 已杀会话后若本地 notice 写失败，逃逸进程 tickets 仅靠返回值携带，直接终态会**永久丢失**。统一类型契约：

- `pendingCleanup` = `{sessionName, cleanupTickets, reason, retryable}`（纯持久化字段）。
- `pendingCleanupError` = `{pendingCleanup, noticeErr, cause}`，实现 `Unwrap` 返回 cause；仅 cause 路径（轮换路径本地写失败）使用，noticeErr 保留首次写失败信息供聚合。

回放合同（observation 模型，两条路径统一）：

- **收集**：外层补偿路径对**每个 cleanup notice intent**（KillSession 非 clean disposition、HasSession/KillSession infra 错误直记 kill_failed，activate.go:1219-1241）形成 observation = `{sessionName, reason, retryable, cleanupTickets, persisted}`——**clean 不形成 observation**（无 notice intent）；notice 写失败 → persisted=false 即 pending；写成功 → persisted=true 仍 MUST 参与归并（首次写失败、外层再次 kill 得到更新 disposition 且写成功时，最终 reason/retryable 必须取最新 observation，MUST NOT 被旧 pending 覆盖回旧值）。**clean 决策**：外层再次 kill 返回 clean 时 MUST NOT 取消或覆盖既有 pending——clean 无 notice metadata，只表明本次会话终止无新增 debt，不会回溯收割首次 kill 逃逸的进程；旧未落库 tickets 仍按原 reason 回放。collect 变体将 `[]cleanupObservation` 暴露给调用方（首次写错误按既有 noticeErrs 语义保留聚合，activate.go:1217-1248）。cause 路径（轮换路径任一终态分支）本地写失败 → 包裹 `pendingCleanupError` 返回（含 SnapshotMissingDegraded 分支，MUST NOT 继续轮换）；cause 路径本地写成功无需携带——若同会话存在待回放 observation，成功 cause 必早于它；否则 DB 中成功写入已收敛，无需进入 fold。
- **KillResult 分类共享**：五类已知 disposition + 未知/矛盾 fail-closed（→ kill_failed，retryable=true）的一致性分类 MUST 收敛为共享 helper，轮换路径 switch 与 cleanup 路径（collect 变体）共用；cleanup 路径 MUST NOT 再经 `recordResidualNoticeFromDisposition` 的 `dispositionToNotice` 静默忽略未知值（notice.go:255-260）——Probe 失败委托外层补偿时同样 fail-closed。
- **归并**：fold 输入 = 按发生顺序的 cause pendings + cleanup observations；同会话合并为**单条**：reason/retryable = **最新 observation**；cleanupTickets = 全部 persisted=false observation 的 tickets union（`recordResidualNotice` 的 DB 内 union（notice.go:179-188）只覆盖已落库 entry，救不了从未写入的旧 tickets，故必须在内存中归并）；仅当同会话存在 persisted=false observation 时产生回放项。
- **回放**：`runActivateFailureCompensation` 在 cleanup 结束后、最终 DB 收敛前回放。回放阶段使用**单个**新的 detached 有界 ctx（整个阶段一次预算，MUST NOT 逐 pending 各建 ctx；超时复用 `activateCompensationFinalizeTimeout`=10s，activate.go:315-319；MUST NOT 复用 cleanup compCtx——KillSession 不受 compCtx 约束、多会话串行可耗尽 30s 预算，activate.go:306-318）。回放项之间按首次出现顺序确定性遍历，每项额外调用 `recordResidualNotice` **恰好一次**；回放错误独立于首次写错误聚合进 last_error。
- **收敛**：回放仍失败仅存于 last_error——**store 在整个激活窗口持续不可写是不可约边界**（notice 只能随任务行持久化，DB 不可写期间任何机制都无法落库），设计目标收窄为"单次/瞬时写失败（CAS 不收敛等，DB 在回放时点可用）不丢 tickets"。
- **其余调用点行为不变**：`cleanupActivationRuntime` 既有签名（返回 error）保留；observation 收集由新增的 collect 变体承载，仅 Activate 失败路径使用（`runActivateFailureCompensation`，activate.go:335）。其余三个既有调用点——锁超时 best-effort 收敛（activate.go:1139）、运行时主动收敛（activate.go:1164）、resume 失败清理（reconcile.go:167）——继续调用原函数，保持现状首写聚合语义，不做 pending 回放（其失败模式非本 change 目标，行为显式不变）。
- default/未知矛盾分支的载荷 reason 固定为 kill_failed、retryable=true（共享分类 helper 的 fail-closed 输出，不经过 `dispositionToNotice`）。

**终态错误聚合**：轮换路径终态（wait 失败后的门禁/预算/分配/持久化终态）返回的错误 MUST 聚合可得上下文——wait 错误 + KillSession infra error 或非 clean disposition + notice 错误（如 `fmt.Errorf("serve not ready: %v; kill: %v; notice: %v", ...)` 风格），保证 last_error 可诊断，不得只返回 `health check timeout`。Probe 终态与外层 compensation 结果不在此列（边界见上）。

**未知/矛盾 KillResult fail-closed**：`CleanupDisposition` 是开放字符串类型。switch 必须有 default：未识别的 disposition、或 disposition 与 `SessionKilled` 矛盾（如 `SessionKilled=true` 配 snapshot_failed）→ 按 kill_failed 记 notice 后终态，MUST NOT 静默继续或静默终态。

`startServeWithPortRetry` 主流程（`servePortRetries=3` 为**总尝试次数**，含首次）：

```
尝试 i（i = 1..3）
  │
  ├─ NewSession(serve, port_i) 失败 → 终态报错（不轮换，同现状）
  │
  ├─ waitServeReadyOrDead 就绪 → Probe
  │     └─ 任何 Probe 失败（含 ctx 取消、冷启动重试耗尽的
  │        ErrServeNotReady、unauthorized、能力不兼容、未知）
  │        → 直接返回错误：MUST NOT 本地 kill、MUST NOT 换端口重试；
  │        错误分类保持现状；kill + notice 统一委托外层
  │        runActivateFailureCompensation（detached ctx）
  │
  └─ waitServeReadyOrDead 失败
       │
       ├─ ① ctx.Err() != nil → 直接返回 ctx 错误
       │     （行为修复：现状会继续 kill + allocate）
       │
       ├─ ② 进程已死亡（errors.Is(err, errServeSessionDied)）
       │     → 会话已不存在 = 已确认终止；无 KillSession 调用，
       │      无 disposition、无 notice → ④
       │
       ├─ ③ 健康超时（会话仍存活）→ KillSession 门禁
       │     （notice 写用 WithoutCancel + 有界 ctx）：
       │     ├─ err != nil（infra 错误）
       │     │    → 记 retryable kill_failed notice → 终态
       │     ├─ DispositionClean → ④
       │     ├─ DispositionReapFailed（SessionKilled=true，retryable tickets）
       │     │    → 记 notice → 终态（不允许 active 携带 retryable debt）
       │     ├─ DispositionSnapshotFailed / DispositionKillFailed
       │     │    （SessionKilled=false，会话仍存活，retryable）
       │     │    → 记 notice → 终态（外层 compensation 会再次尝试 kill）
       │     ├─ DispositionSnapshotMissingDegraded（会话已消失，
       │     │    non-retryable 已接受丢失）→ 记 notice → ④
       │     │    （写失败 → 终态 + handoff，MUST NOT 继续轮换）
       │     └─ default（未知/矛盾 KillResult）→ fail-closed：
       │          按 kill_failed 记 notice → 终态
       │
       ├─ ④ i == 3 → 终态 serve not ready（MUST NOT allocate/persist）
       │
       ├─ ⑤ allocatePort(lastPort=port_i, exclude=port_i)
       │     ├─ 失败 → 终态报错，MUST 包装 aerr
       │     │        （修正现 activate.go:587 误包健康检查 err）
       │     └─ 成功 → port_{i+1} ≠ port_i → ⑥
       │
       └─ ⑥ persistEnvSnapshot
            ├─ 失败 → 终态报错，MUST NOT NewSession
            │        （保留现 activate.go:596-597 边界，不得回归）
            └─ 成功 → continue
```

「已确认终止」的定义：**会话已不存在（分支② 或 SnapshotMissingDegraded）或 `SessionKilled=true`（Clean/ReapFailed）**——但 ReapFailed 因 retryable debt 规则仍终态。终态路径返回前 MUST 先本地持久化已有 disposition/tickets 的 notice；本地写失败 MUST 经 pending cleanup handoff 移交外层重试，不得静默丢弃；错误聚合全部可得上下文。

**端口相异语义**：仅排除刚失败的那一个端口（相邻尝试不同），不要求全部尝试互异——双端口可用范围内 A→B→A 合法。`exclude int` 单值参数即可，不引入集合抽象（YAGNI）。

**为什么显式 exclude 而非拨动 portCursor**：拨游标不保证排除——扫描满一圈仍会回到旧端口。

### D3（不采纳，记录理由）: SSE 重连加上限

无限重连（`ReconnectMaxTries=0`）保留：SSE 在同一 Client 内复用自身 transport，churn 贡献可忽略；设上限会让长任务在 serve flap 后静默丢失事件流，风险大于收益。explore 阶段曾列入修复项，分析后确认非根因必要项，经本文档记录后移出范围。

### 副作用边界（MUST NOT 表）

| 条件 | MUST NOT 发生的副作用 |
|---|---|
| wait 失败后 ctx.Err() != nil | 本地 KillSession、allocatePort、persistEnvSnapshot、NewSession |
| 任何 Probe 失败（含 ctx 取消） | 本地 KillSession、换端口重试 |
| KillSession err != nil | allocatePort、persistEnvSnapshot、NewSession |
| DispositionReapFailed / SnapshotFailed / KillFailed / 未知矛盾 KillResult | allocatePort、persistEnvSnapshot、NewSession |
| 任何 notice 持久化失败（nerr != nil） | allocatePort、persistEnvSnapshot、NewSession |
| 最后一次尝试（i == servePortRetries）wait 失败 | allocatePort、persistEnvSnapshot |
| persistEnvSnapshot 失败 | NewSession |

轮换路径终态在返回前 MUST 先本地持久化对应 disposition 的 notice（写操作用 WithoutCancel + 有界 ctx 10s）；本地写失败 MUST 经 pending cleanup handoff 将 tickets 移交外层补偿重试持久化，不得静默丢弃（store 持续不可写为不可约边界，见 D2）。轮换路径终态返回错误 MUST 聚合 wait 错误 + kill/disposition + notice 错误的全部可得上下文；Probe 终态不在此列——返回探测错误本身，外层 compensation 结果仅聚合进 last_error（边界见 D2「错误聚合边界」）。

## Risks / Trade-offs

- [共享 transport 复用到对端已重启的半死连接] → Go transport 对幂等 GET 自动重试；DialContext 30s 超时兜底；健康检查失败路径本就有轮询重试，行为不回归。
- [包级单例在测试中跨用例共享连接池] → 测试用 listener/DialContext 计数验证复用语义，不对单例内部状态做断言。
- [活跃 serve 数持续超出共享池容量（项目不设并发配额、端口范围理论上限 1000）时 idle 驱逐] → 目标负载为单用户本地编排的数十个活跃任务；持续超容量时退化回按请求新建连接——churn 接近修复前水平，受请求速率约束而非无界，属**残余可用性风险**（接受并在此明示），不承诺零影响；热点 host（正被轮询/流式的任务）仍始终享受复用（见 D1 容量模型）。
- [排除旧端口后范围仅剩旧端口可用（极端：范围内唯一空闲）] → 报端口耗尽错误（包装 aerr）并挂起；50000-50999 共 1000 端口，实际不可达。
- [reap_failed 等 retryable disposition 导致激活失败而非"带病继续"] → 有意收紧：active 携带 retryable debt 会被 notice.go:392 的同名跳过逻辑搁置；suspended 后后台重试立即生效，恢复路径是用户重新激活，语义闭合。
- [轮询观测到进程死亡（分支②）不记 notice，与 SnapshotMissingDegraded 混淆] → 两者是不同分支：②是 wait 内 HasSession 观测（无 kill 调用、无 disposition、无 notice）；SnapshotMissingDegraded 仅在调用 KillSession 后会话于快照期间消失时产生（记 notice）。实现与测试不得混用。
- [换端口重试对"serve 进程本身挂起"类故障无帮助（端口无辜）] → 重试仍会换全新 serve 进程，对冷启动慢/进程异常均有容错收益；不试图在本 change 区分根因类别。

## Migration Plan

纯代码变更，无配置/DB 迁移。部署 = 重新构建 + 重启 ocdeck-server；重启后 reconcile 的 resume 路径即使用共享 transport。回滚 = revert 并重启。

## Open Questions

（无。评审决策点已定：servePortRetries=3 为总尝试次数；active 任务不携带 retryable cleanup debt——ReapFailed/SnapshotFailed/KillFailed/未知矛盾 KillResult 记 notice 后终态，SnapshotMissingDegraded 记 notice 后继续；notice 本地写失败 → 终态 + pending cleanup handoff 由外层补偿重试持久化（store 持续不可写为不可约边界）；轮换路径终态错误聚合 wait + kill/disposition + notice 全部上下文（Probe 终态返回探测错误本身，外层 compensation 结果仅入 last_error）；persistEnvSnapshot 失败 → 终态不建会话；任何 Probe 失败不轮换、不本地 kill，委托外层 compensation；wait 失败后 ctx 取消直接短路；共享池容量按单用户数十任务建模，超限为明示接受的残余可用性风险。）
