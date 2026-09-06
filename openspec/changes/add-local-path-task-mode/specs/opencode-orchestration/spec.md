# Delta: opencode-orchestration（add-local-path-task-mode）

## MODIFIED Requirements

### Requirement: session 归属捕获

系统 SHALL 订阅每个活跃任务进程的 SSE 事件流（`GET /event`）：`session.created` 事件的 sessionID 位于 `properties.info.id`（已验证契约区间 [ContractMinVersion, ContractBaseline]）；同时监听 `session.updated` 刷新 `last_seen_at`。激活后 MUST 按任务有效模式执行一次该 directory 的 session 对齐（worktree 模式任务执行目录级全量对齐，新 session 按 claim 语义补记；dir 任务与 repo 项目 local-path 模式任务执行 ownedOnly 对齐，MUST NOT 认领新 session）。SSE 断流时 MUST 指数退避重连，重连成功后 MUST 再次按同一有效模式对齐。

**session 所有权规则**：一个 opencode session 至多归属一个 ocdeck 任务（该约束适用于经本变更后合法写入口产生的新归属；历史遗留的重复归属行不做启动修复，随任务删除自然清理）；任务 MUST 仅对本任务拥有的 session（`task_sessions` 中本任务的行）执行删除、attach 与对齐写回。归属写入 MUST 统一经 store 层原子 claim（单事务内"仅当 sessionID 未被其他任务拥有时插入/更新本任务行"），MUST NOT 以"先查询后 upsert"的非原子方式写归属。claim 冲突语义：SSE/对齐路径冲突 MUST 忽略该 session 并记服务端诊断日志（不阻断）；锚定创建路径冲突 MUST 使激活失败并记录 last_error，MUST NOT attach 不属本任务的 session。

`kind=dir` 项目的任务与 repo 项目 local-path 模式任务（目录可共享）MUST NOT 经目录级全量对齐认领新 session。dir 任务与 local-path 模式任务的对齐（ownedOnly）MUST 按以下顺序执行：① 按原始目录列表数量判定 complete/overflow（判定先于任何过滤）；② 候选集取"原始目录列表 ∩ 本任务当前 owned 集合"；③ complete 时在单个 store 事务内仅对候选集刷新 `last_seen_at`、仅删除本任务 owned 集合中的缺席行，并经事务内 noticeFn 清除既有 session_overflow notice；④ overflow 时不删任何缺席行，application 层 MUST 先经事务外 CAS 写入 session_overflow notice 再调对齐（对齐失败时 notice 保留，与 repo 现状逐点一致），仅刷新候选集。dir 任务与 local-path 模式任务的新 session 仅经本任务进程的 SSE 捕获（原子 claim）与锚定创建记录归属（SSE 断流期间经 TUI 新建的 session 不补记，为已接受的降级）。`session.updated` 事件 MUST 仅刷新本任务已归属行的 `last_seen_at`（条件更新，绝不插入新归属），未归属 session 的 updated 事件一律忽略。

有效模式解析 MUST 覆盖全部四个会建立 SSE/对齐/锚定的运行时入口：Activate、persist 重启恢复（resumeActive）、挂起失败的运行时修复（tryRepairRuntime）、自动重拉恢复（ensureRecovery，含 ReopenAttach 转发而来的恢复）；四者在任何状态修改或运行时副作用前 MUST 解析「项目 `kind` + 任务持久化 `mode`」得到有效模式，合法组合仅 (repo, worktree)、(repo, local-path)、(dir, local-path)，并按解析结果分流对齐模式（worktree 模式 → 目录级全量对齐；dir 任务与 repo 项目 local-path 模式任务 → ownedOnly 对齐）；dir+worktree、未知 kind、未知 mode MUST 报错且零副作用。恢复路径的锚定 claim 冲突 MUST 判定本次恢复失败并记录 last_error（计入重拉预算），MUST NOT 接入不属本任务的 session。

同目录双进程不串流是该 SSE 归属方案的前提，已经 OpenCode 源码验证（设计阶段完成）：`/event` 订阅的是进程内 listener（`server/routes/instance/httpapi/handlers/event.ts`），事件 publish 仅 notify 本进程 PubSub（`core/event.ts`），跨进程仅可经 `sync/history` 显式拉取；该架构自 v1.16.0 起连续稳定（v1.18.14 ↔ v1.18.18 锚点字节级一致；扩展区间须相邻对核验）。若未来 OpenCode 升级引入存储级事件分发，dir 任务与 repo 项目 local-path 模式任务归属 MUST 重新评审。

#### Scenario: TUI 新建会话被记录

- **WHEN** 用户在 TUI 中新建会话
- **THEN** 新 sessionID 经 SSE 被捕获并原子 claim 至本任务，用于后续恢复；若已被其他任务拥有则忽略并记诊断日志

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

- **WHEN** 两个任务（如 SSE 与对齐并发）同时 claim 同一 sessionID
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

### Requirement: 每任务单进程实例

系统 SHALL 为每个活跃任务启动单个 opencode 进程：`opencode --port <port> --hostname 127.0.0.1`（托管于命名 tmux 会话 `ocdeck-<taskID>-runtime`），工作目录为该任务的「任务运行目录」（worktree 模式任务为其持久化 `worktree_path`；`kind=dir` 任务与 repo 项目 local-path 模式任务为其 canonical 项目路径），并以随机强密码经会话 env（`new-session -e OPENCODE_SERVER_PASSWORD`）启用 Basic Auth。该进程经 `--port`/`--hostname` 进入 external 模式，在同一进程内同时提供 TUI（浏览器经 tmux attach 客户端接入）与完整 HTTP API/SSE 控制面（与 `opencode serve` 相同的 `Server.listen` 实现）。ocdeck MUST 作为该进程 API 的唯一访问入口；密码 MUST NOT 经 argv 传递。

锚定 session bootstrap MUST 采用确定性顺序协议（MUST NOT 依赖启动期 SSE `session.created` 事件——不可重放、与订阅建立有竞态、无法从并发事件中识别 bootstrap 事件）：

1. **启动**：有锚定 → 命令 MUST 携带 `--session <anchoredSessionID>`；无锚定 → MUST NOT 携带 `--session`
2. **就绪校验**：健康检查 + 能力探测通过后 MUST `GET /session?directory=` 取列表：有锚定且在列表中 → 锚定确认；有锚定但不在列表（已失效）→ 弃用旧锚定转无锚定流程
3. **无锚定创建**：MUST `POST /session?directory=` 创建新 session，**按响应 ID 原子 claim** 并写入锚定；claim 冲突 MUST 判定本次激活/恢复失败并记录 last_error（恢复场景计入重拉预算），MUST NOT 接入不属本任务的 session
4. **落到锚定（双启动子事务，仅新建锚定时；permit 子协议仅适用 Recovery 路径——首次 Activate 的双启动 MUST NOT 消耗恢复 permit、不执行恢复退避）**：① bootstrap 进程占用一个重拉预算 permit 并完成健康检查 + 能力探测；② `POST /session` + claim 后 MUST 按既有 KillResult/cleanup notice 规则确认 bootstrap 进程已终止，才可复用 `-runtime` 名称与端口；③ 正式进程占用**新的**预算 permit 并执行对应退避，端口复用已持久化值、密码重新生成；④ 正式进程 MUST 重新执行健康检查 + 能力探测 + 锚定存在校验；⑤ 全部通过才进入成功提交；⑥ 预算窗口不足以取得第二个 permit 时，已 claim 的锚定 MUST 保留，本次尝试进入终态补偿
5. dir 项目任务与 repo 项目 local-path 模式任务 MUST NOT 经目录级对齐认领（ownedOnly 语义不变）

`--session` 失效的确定性分派：进程在 HTTP server 就绪前退出 → 健康轮询判死/超时 → 按本次尝试失败处理（既有错误分类）；进程就绪 → 列表校验是唯一正确性判据（锚定在列表中且 id 有效则 CLI 校验正常路径必然选中；不在则弃用重建）。

锚定持久化契约：锚定 MUST 存于 `tasks.anchor_session_id` 显式列（替代「最近顶层 owned session 推导」现状）；「ClaimTaskSession + 设置 anchor」MUST 为单事务——claim 成功后 newID 立即成为权威锚定，跨 attempt/重启保留；claim 冲突时归属与 anchor 均不修改。`--session <id>` 的 id 一律读自 `tasks.anchor_session_id`。旧锚定条件清空（列表校验缺席时执行 `anchor_session_id=NULL WHERE task_id=? AND anchor_session_id=<old>`）的分派：store error → POST 前终态补偿；清空 Matched → 转无锚定流程继续；CAS mismatch（0 行匹配）→ MUST 复读：为 NULL 才继续，已出现新 anchor → 终止本次 bootstrap、按新锚定进入下一 attempt，MUST NOT 覆盖。既有数据 MUST 回填：schema 迁移时（或首次读取时惰性）按旧确定性排序（最近顶层 owned session）回填 `anchor_session_id`，仅处理 NULL 行。

术语继承：本 capability 其余 Requirement 中的「serve 进程」「serve 会话」「serve」一律指本 Requirement 定义的任务单进程及其 tmux 会话；端口分配、健康检查、能力探测、SSE 归属、连接管理等既有语义不变。

#### Scenario: 激活时启动单进程

- **WHEN** 任务被激活
- **THEN** 系统分配端口、生成随机密码并以任务运行目录为 cwd 启动单进程会话（worktree 模式任务→持久化 worktree 路径；dir 任务与 repo 项目 local-path 模式任务→项目路径），TUI 画面与 API 在同一进程就绪

#### Scenario: 同进程提供 API 与 TUI

- **WHEN** 单进程通过健康检查与能力探测
- **THEN** ocdeck-server 经 `http://127.0.0.1:<port>` 访问全部契约端点（/session、/event、/global/health 等），浏览器经 tmux attach 客户端接入同进程 TUI，两者互不影响

#### Scenario: 有锚定恢复指定会话

- **WHEN** 激活或重拉时任务存在已确认归属的锚定 session
- **THEN** 启动命令携带 `--session <anchoredSessionID>`；健康检查与能力探测通过后经会话列表校验锚定存在则确认，不存在则弃用旧锚定转无锚定流程

#### Scenario: 无锚定经 API 创建锚定

- **WHEN** 激活或重拉时任务无有效锚定
- **THEN** 进程就绪后经 `POST /session` 创建新 session，按响应 ID 原子 claim 为锚定；确认 bootstrap 进程终止后，正式进程启动（Recovery 路径另占预算 permit；Activate 路径沿其既有重试预算、不耗恢复 permit）并重新通过健康检查、能力探测与锚定存在校验，才标记活跃；claim 冲突时本次激活/恢复失败并记录 last_error
