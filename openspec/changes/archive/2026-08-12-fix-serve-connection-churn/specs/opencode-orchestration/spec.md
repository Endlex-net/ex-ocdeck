# Delta: opencode-orchestration

## MODIFIED Requirements

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
系统 SHALL 在 serve 启动后轮询 `GET /global/health`，就绪后 MUST 做能力探测（`/session/status` 响应结构与 session 列表字段形状校验；**DELETE 响应形状 MUST NOT 做 live 探测**——不能为探测制造删除副作用，首次真实删除时校验，不符报 deletion_failed）；探测通过才启动 TUI 会话。单端口尝试的健康检查超时或 serve 进程死亡 MUST 先按「端口分配策略」处置（旧会话清理、cleanup notice、换端口判定），仅当该策略判定允许换端口时才以不同端口重试；全部尝试耗尽后 MUST 判定激活失败，清理会话并回滚任务状态为挂起 + 记录 last_error。**任何能力探测失败**（含冷启动重试耗尽后的 serve 未就绪、认证失败、能力不兼容、未知错误）MUST 直接判定激活失败，MUST NOT 触发换端口重试，错误分类保持既有映射，清理会话并回滚任务状态为挂起 + 记录 last_error。opencode 版本号 MUST NOT 作为激活门禁：版本与契约基准不一致时仅告警（日志 + UI 提示），不阻止激活。

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

## ADDED Requirements

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
