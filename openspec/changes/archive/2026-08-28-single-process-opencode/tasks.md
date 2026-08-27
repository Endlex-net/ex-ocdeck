# Tasks: single-process-opencode

## 1. Spike（契约基线实测，先行）

- [x] 1.1 D5 spike：在契约基线版本实测 bare TUI external 模式——① `--session` 指向不存在 id 时 TUI 的确切行为（报错/回退/新建；正确性已由 D5 的 API 列表校验兜底，此项定 UX 表现）② external 模式下 health / session CRUD / status / SSE 首事件 / Basic Auth 与 serve 逐点一致。spike 结论与 D5 冲突时，先更新 artifacts 再继续实现

## 2. tmux 角色模型与命名收敛

- [x] 2.1 `internal/task/manager.go` runtimeGroup（L278-283）：Role 枚举从 `serve/tui/shell` 收敛为 `runtime/shell`；梳理全部 role 判断分支（activate.go、delete.go、suspend.go、reconcile.go）
- [x] 2.2 `internal/task/util.go`：新增 `runtimeSessionName`；`roleFromSessionName` 与 `taskIDFromSessionName`（reconcile.go L596-605 亦使用）识别新后缀 `runtime`/`shell-<n>` 及 legacy `serve`/`tui`（后者仅供迁移清理分组）
- [x] 2.3 `internal/infrastructure/process/process.go` sessionNameRe / ValidateSessionName（L171-187）：白名单拆分——NewSession 仅接受 `runtime`/`shell-<n>`、拒绝 legacy；HasSession/KillSession/watch 等管理清理路径同时接受 legacy 后缀

## 3. 激活路径单进程化

- [x] 3.1 `internal/task/activate.go` `startServeWithPortRetry`（L843）：启动 `opencode --port <p> --hostname 127.0.0.1`（会话名 `-runtime`，cwd=worktree，`new-session -e` 注入密码）；端口轮换、cleanup notice、回放等 fail-closed 语义逐点不变
- [x] 3.2 健康检查 + 能力探测通过判定当前进程 **ready**（不直接判定活跃）；删除 `startTUI`（L1369）与独立 TUI 会话创建；活跃的唯一提交点 = 成功提交全序列（锚定确认 → token/group/watcher 注册 → SSE 订阅+全量对齐 → 写回 tasks.last_port → CAS active）完成后
- [x] 3.4 锚定持久化契约（F-25/F-30/F-31）：schema 迁移新增 `tasks.anchor_session_id` 列并按旧确定性排序（最近顶层 owned session）回填（仅 NULL 行，含多 session 迁移测试与回滚验证）；store 层「ClaimTaskSession + 设置 anchor」单事务方法（claim 冲突时归属与 anchor 均不修改）；旧锚定条件清空分派（store error → POST 前终态；Matched → 转无锚定；CAS mismatch 复读——NULL 才继续；出现新 anchor 则先按 KillResult disposition 表确认当前 bootstrap 已终止，再按新锚定进入下一 attempt（Recovery 另占 permit、Activate 沿既有重试预算），绝不覆盖）；激活/重拉的 `--session <id>` 一律读自该列；替代 `ListTopLevelTaskSessions`（queries.go L954）推导现状
- [x] 3.3 D5 确定性 bootstrap 协议：有锚定 → `--session <anchoredID>` 启动，就绪后经 `GET /session?directory=` 校验锚定在列表中（不在则弃用转无锚定流程）；无锚定 → 不带 `--session` 启动，就绪后 `POST /session?directory=` 创建并按响应 ID 原子 claim 写锚定，再以 `--session <newID>` 重启进程（一次性双启动，仅锚定创建时）；claim 冲突判定激活/恢复失败 + last_error

## 4. 进程监视与自动重拉

- [x] 4.1 `internal/domain/task/task.go`：新增 `ApplyRecoveryStart`（只表达 active→activating 及任务领域 guard，**不含 runtime token**）等恢复迁移方法，迁移矩阵仿 ApplyActivate/ApplyActivateCommit（L362-365）模式；runtime token 校验在 Manager/ensureRecovery 持锁后完成（基础设施代际概念不进领域层）
- [x] 4.2 recovery policy 组件（注入 clock，确定性测试友好）按预算协议实现：仅 Recovery 路径使用（首次 Activate 不用）；store 层新增 per-task 持久化 attempt 时间戳表；**AcquirePermit 协议**——原子写入 acquire 并返回 ordinal，写入成功后才按 ordinal 退避（ordinal 1/2/3 → 5s/15s/45s），permit 写入是 attempt 首个动作，退避取消时 permit 仍保留；滚动 5min/3 permit；成功不清零仅窗口老化；端口轮换与双启动第二次创建各占 permit；attempt 记录跨重启保留参与窗口计数
- [x] 4.3 恢复执行器按 D3 三段结构实现：一次性前序（token 校验 + CAS active→activating + 停旧 SSE/watch + 清理全部 shell 与残余 runtime，retryable cleanup debt 直接进终态补偿）→ 可重复 attempt（端口分配 → 端口持久化进 env_snapshot.vars.OCDECK_SERVE_PORT → 新密码建进程 → 健康+探测 → D5 bootstrap，无锚定分支含双启动子事务：bootstrap 占一个 permit、确认终止后才复用 `-runtime` 名称、正式进程占新 permit 并重新健康+探测+锚定校验）→ 成功提交（锚定确认 → 新 token+注册 group/watcher → SSE 订阅+全量对齐 → 写回 tasks.last_port → CAS activating→active；CAS 失配分派：复读状态+token，active 且 token 属本 attempt 视为幂等成功，否则完整反向清理且 DB 禁写范围收窄为 status/last_error/env_snapshot/anchor 四字段——KillResult disposition 表产生的 notice/debt 仍必须持久化，失败写入 tagged debt（phase=cleanup_notice））；统一终态补偿 MUST 先对可能存活的 runtime 会话执行 HasSession/KillSession 并按完整 KillResult disposition 表处置（见 design D3 表：absent/clean 直接终态、snapshot_missing_degraded 记 non-retryable notice 后终态、retryable 各分支 debt 持久化成功后终态、持久化失败走 pending/replay），再经单个条件事务 `CompleteRecoveryFailure(expected=activating)` 原子完成 status/last_error/env_snapshot 三字段（CAS 不匹配均不修改）。**pending/replay 合同扩展至 Recovery（tagged debt）**：`phase=cleanup_notice|complete`——cleanup_notice 变体字段 taskID+sessionName+tickets+reason+retryable+cause，complete 变体仅 taskID+cause；重放入口为后台周期任务+Shutdown/reconcile 按 phase 恢复（cleanup_notice 重放 notice 后执行 Complete，complete 直接执行 Complete）；CompleteRecoveryFailure 自身写失败保留 phase=complete debt 由后台驱动收敛，CAS mismatch 删除 debt 服从最新状态。**不直接复用 Activate**（其 checkNoResidualSessions 会拒绝存活 shell，activate.go L1675-1684），仅复用端口轮换/健康检查/探测/SSE 对齐子例程
- [x] 4.4 watcher 分派（D3 状态表）：active+token 匹配 → keyed mutex 内复读 + CAS；activating → no-op；suspending/deleting/token 不匹配 → no-op；与 Suspend 的锁竞争规则（先拿锁者胜，恢复先拿锁则 Suspend 收 transitional-state 拒绝；Delete 与 active 恢复无竞争——delete.go CanDelete 只接受 suspended/archived/失败态，deleting 下 watcher no-op 仅为防御）；ensureRecovery 幂等入口（watcher 与 ReopenAttach 共用）
- [x] 4.5 SSE 永久失败统一分派：`internal/task/activate.go` SubscribeEvents 永久返回处（L1106 附近 `convergeToSuspendedForGen` 直接落挂起路径）改为幂等 `ensureRecovery`——SSE EOF 先于 watcher 到达且进程仍存活时，恢复前序清理终止该失去控制面的 runtime；两个入口经同一 keyed mutex + token + 幂等保证不产生并发双恢复

## 5. ReopenAttach 与终端链路

- [x] 5.1 `internal/task/attach_shell.go` `ReopenAttach`（L22）重定义（D8 表）：runtime 存活 → 返回 `-runtime` terminal ID；active 但缺失 → ensureRecovery + typed `recovering`；activating → 同一 typed `recovering` 不重复启动；其他状态 → invalid state。错误码落点：`internal/application/operror.go` 新增/映射 `recovering` application 错误码、`internal/api/errors.go` HTTP 409 映射。删除「仅重开 TUI」旧路径与 TUI 会话预检分支
- [x] 5.2 `internal/api/ws_terminal.go`（AttachPty 调用点 L55-59）：`recovering` 映射为 WS close code `1013`（Try Again Later），MUST NOT 映射为 suspended 或既有非重试关闭码；web 前端（`web/src/terminal/session.ts` 等）识别 1013 后轮询任务状态、回到 active 后重连；恢复期统一显示「进程启动中」，不新增原因字段
- [x] 5.3 shell 终端创建/关闭路径适配新 role 模型；web 前端原「TUI 可重开」标记语义改为「进程在不在」

## 6. Reconcile 与迁移

- [x] 6.1 `internal/task/reconcile.go`：persist 恢复判定从「serve 会话存活+健康」改为「runtime 会话存活+健康」，且仅 **active** 任务可原地 resume；**activating 一律视为被中断的激活/恢复——执行清理并落挂起**（bootstrap 中间态无法经会话名区分，不续跑）；旧版 `-serve`/`-tui` 会话一律按异常会话清理（不支持热迁移）
- [x] 6.2 回滚路径验证：确认旧版代码对 `-runtime` 未知 role 会话按孤儿清理；不满足则在 release note 文档化回滚顺序（挂起全部任务 → 停止新版 ocdeck-server → 清理 socket → 部署旧版）与正确命令 `TMUX_TMPDIR="${OCDECK_DATA_DIR:-$HOME/.ocdeck}/tmux" tmux -L ocdeck -f /dev/null kill-server`

## 7. 契约与文档

- [x] 7.1 `internal/infrastructure/opencode/CONTRACT.md`：锚点清单新增 `cli/cmd/tui.ts`、`cli/tui/worker.ts`（新增 2 个，最终 23 个）；`cli/tui/validate-session.ts` 标注单进程相关性；升级 SOP 步骤同步（external 分支行为变化阻断区间扩展）
- [x] 7.2 `scripts/check-opencode-contract.sh`：ANCHORS 数组与数量断言更新为 23；live probe 改为 spike 验证过的 bare TUI external 启动流程（Basic Auth / health / session CRUD / status / SSE 首事件 / `--session` 恢复与校验失败语义）
- [x] 7.3 release note：部署前提「升级前挂起全部任务」+ 回滚步骤；归档时同步更新主 spec Purpose 为单进程模型，统一术语（runtime 会话 / tmux attach 客户端）

## 8. 测试

- [x] 8.1 bootstrap：有锚定 `--session` 恢复且列表校验通过、锚定失效（不在列表）转无锚定流程、无锚定经 `POST /session` 按响应 ID claim 并双启动落到锚定、claim 冲突激活/恢复失败；双启动子事务（permit 子协议仅适用 Recovery——首次 Activate 的双启动不占恢复 permit、不执行恢复退避）：bootstrap 进程确认终止后才复用 `-runtime` 名称、两次创建各占预算 permit 并执行对应退避、正式进程重新健康+探测+锚定校验、第二次启动失败的补偿路径、窗口不足第二个 permit 时锚定保留并进终态补偿
- [x] 8.2 状态机：active→activating→active / →suspended；watcher 与 Suspend 两种拿锁顺序（Delete 竞争分支不存在——Delete 只接受 suspended/archived/失败态）；旧 token 回调 no-op；activating 中消失不重复介入；SSE EOF 先于 WatchExit 到达的竞态（同一 ensureRecovery 幂等入口）；token/group/watcher 注册后失败的反向清理（cancel/join SSE+watch、清 runtime registry，再 KillSession+终态事务）双向测试
- [x] 8.3 预算：窗口滚动老化、成功不清零、端口轮换计次、退避 timer 取消（离开 activating/Shutdown/token 失效）
- [x] 8.4 attempt 事务：清理先于创建、retryable cleanup debt 阻断恢复、env 快照同代复用仅改端口、每次新密码、健康通过才写 tasks.last_port；终态补偿按 disposition 表做表驱动测试（absent/clean 直接终态、snapshot_missing_degraded 记 non-retryable notice 后终态、retryable 非 clean/infra/未知矛盾在 debt 持久化成功后仍完成 activating→suspended 且后台接管、notice/debt 持久化失败禁止状态提交），`CompleteRecoveryFailure(expected=activating)` CAS 匹配时三字段（status/last_error/env_snapshot）同时生效、不匹配时均不修改
- [x] 8.5 ReopenAttach 与 watcher 去重（ensureRecovery 幂等）、WS recovering 可重试关闭语义、各状态分支（存活/缺失/activating/其他）
- [x] 8.6 legacy `serve`/`tui` 会话可清理不可新建；reconcile 遗留双会话清理；persist 恢复判定改为 runtime
- [x] 8.7 激活路径现有测试改写：单进程命令断言、探测通过仅 ready（完整成功提交序列后才活跃）、不再创建 TUI 会话（activate*_test.go、probe_coldstart_test.go、start_tui_anchor_test.go 相应改写/删除）
- [x] 8.8 每个新增/修改行为测试提供有效性证据（旧实现下失败、新实现下通过，或对基线 revision 运行新增测试确认变红）
- [x] 8.9 全量验证通过：`go build ./... && go test ./...`；web 前端 `cd web && pnpm build`（含 tsc 类型检查；有前端测试则一并跑 `pnpm test`）；REST handler 对 `recovering` 409 的映射测试

## 8.8 证据记录

范围：`git log de1d0b1..HEAD`（`de1d0b1` runtime 角色/锚定/permit store → `5c2b71c` 激活 bootstrap → `cdcbcba`/`c0d09d1` 恢复执行器 → `cdabc9f` ReopenAttach/WS/reconcile）以及本工作区未提交的 Phase 5 / Gate 5 测试。

标注约定：

- **实测变红**：对更早 revision 跑过该测试并确认失败（本 Gate 未重跑 `de1d0b1`；下列「实测变红」仅引用提交说明/既有 Gate 记录中已声明的变红）。
- **等价 mutation**：新测试针对旧实现的已知缺口构造反例；按旧分支语义（raw `Disposition != clean`、`dispositionToNotice` 吞 `ok=false`、漏 kill 同名覆盖、无第二轮 list 校验）会失败，未在本 Gate 对基线二进制实测。

| task | 测试（文件 / 函数） | 旧实现失败原因 / 基线 | 标注 |
|---|---|---|---|
| 2.x 角色模型 | `g2_gate_fixes_test.go` `TestKillResidualSessions_*`；`start_tui_anchor_test.go` argv 断言 | 旧 serve/tui 命名与双进程 argv；基线 `de1d0b1` 无 `-runtime` 激活路径 | 等价 mutation（同提交引入于 `5c2b71c`） |
| 3.3 / 8.1 Activate bootstrap | `start_tui_anchor_test.go`：`TestActivate_UnanchoredCreatesClaimAndDualStarts` / `TestActivate_AnchoredSessionPresentNoCreate` / `TestActivate_StaleAnchorClearsAndCreates` / `TestActivate_ClaimConflictFails` | 无 `--session` / 无列表校验 / 无 claim 冲突终态 | 等价 mutation（`5c2b71c`） |
| 4.2–4.3 / 8.3–8.4 预算与终态 | `recovery_test.go`：`TestRecoveryNoAnchor_DualStartPermits`、`TestRecoveryNoAnchor_SecondPermitWindowExhaustedKeepsAnchor`、`TestCompleteRecoveryFailure_Dispositions`、`TestRecoveryRotation_RetryableDebtBlocksNewSession` | 无 permit 子协议；disposition 表不完整 | 等价 mutation（`cdcbcba`/`c0d09d1`）；终态表含未知/矛盾 fail-closed |
| 4.4–4.5 / 8.2 / 8.5 | `phase4_reopen_reconcile_test.go`；`p1_gate_test.go` `TestReopenAttach_RecreatesMissingTUI` | ReopenAttach 重建独立 TUI；SSE 直落挂起 | 等价 mutation（`cdabc9f`） |
| 5.2 WS 1013 | `web/src/__tests__/term-recovering.test.ts`；`internal/api` recovering 409 | 无 1013 / recovering 映射 | 等价 mutation（`cdabc9f` + 未提交 web 收紧） |
| 6.1 reconcile | `phase4_reopen_reconcile_test.go` persist/runtime 判定 | persist 按 serve 存活 resume；activating 可续跑 | 等价 mutation（`cdabc9f`） |
| 8.1 Recovery 锚定 e2e（G5-3） | `phase5_recovery_bootstrap_test.go`：`TestRecovery_AnchoredSessionArgvAndListCheck`、`TestRecovery_StaleAnchorRebuilds`、`TestRecovery_ClaimConflictFails`、`TestRecovery_ConfirmTerminatedBeforeRuntimeReuse`、`TestRecovery_FormalProcessSecondRoundHealthProbeAnchor`、`TestRecovery_SecondStartFailureCompensates` | Recovery 仅覆盖无锚定 permit 计数；漏 kill 时 mock 同名覆盖；无第二轮 health/probe/list；第二次 NewSession 失败无补偿断言 | 等价 mutation（本 Gate 新增；`strictReuseProc` 使漏 kill 变红） |
| 8.4 密码/permit（Phase 5 矩阵） | `phase5_matrix_test.go`：`TestActivate_DualStartConsumesNoRecoveryPermit`、`TestRecovery_FreshPasswordPerCreate` | Activate 误耗 permit；双启动复用密码 | 等价 mutation（未提交 Phase 5） |
| G5-2 调用链 fail-closed | `phase5_matrix_test.go`：`TestDelete_ContradictoryCleanDoesNotCommit`、`TestShutdown_UnknownDispositionRetainsRetryableDebt`、`TestRetryTaskNotices_UnknownDispositionKeepsRetryable`、`TestRetryTaskNotices_ContradictoryCleanKeepsRetryable`、`TestRetryOrphanSessions_ContradictoryCleanRetainsDebt`；helper 表 `TestRecordResidualNoticeFromDisposition_FailClosedTable` | `delete.go`/`manager.go`/`notice.go` raw `Disposition != clean` 把矛盾 clean 当成功；`dispositionToNotice` 对未知值 `ok=false` 被忽略 → 空 reason / 非 retryable / 清债 | 等价 mutation（本 Gate；旧分支下 Delete 会 `DeleteTask`、retry 会清 notice） |
| 7.3 / 6.2 回滚命令 | `docs/release-notes/single-process-opencode.md`；`design.md` Migration Plan；`tasks.md` 6.2 | 裸 `tmux -L ocdeck kill-server` 连默认 socket，且未先挂起/停服务 | 文档等价（非测试变红） |

未在本 Gate 对 `de1d0b1` 检出后跑新增测试确认变红；若需实测，应对 `de1d0b1` cherry-pick 仅测试文件后执行上表 G5-2 / G5-3 函数。
