# Proposal: single-process-opencode

## Why

当前每任务运行 `opencode serve` + `opencode attach` 两个 Bun 进程，实测 serve 基线 ~400MB（调优后 ~300MB）、attach 每任务 ~150-300MB（4 任务 TUI 合计 ~730MB），是 ocdeck 最大的内存开销来源之一。经源码确认（`tui.ts` external 分支），`opencode --port <p> --hostname 127.0.0.1` 启动的 TUI 会在同一进程内以与 `serve` 完全相同的 `Server.listen` 实现暴露完整 HTTP API（/session、/event SSE、/global/health、Basic Auth），因此双进程拆分对 opencode 并非必要——单进程即可同时提供 TUI 显示与 ocdeck 控制面，每任务可减少一个约 115-300MB 的 attach 进程。

## What Changes

- **BREAKING** 每任务进程模型从「serve 会话 + attach 会话」双进程改为「`opencode --port <port> --hostname 127.0.0.1` 单进程」单 tmux 会话；该进程同时承载 TUI（浏览器经 tmux attach 客户端接入）与 HTTP API/SSE 控制面（端口分配、密码注入、健康检查、能力探测、SSE 归属捕获逻辑复用现状）
- **BREAKING** 进程退出语义反转：单进程退出（TUI 内 exit / 崩溃）由「任务落挂起（serve 消失）/ 保持活跃（TUI 消失）」改为「监控检测到进程消失后自动重拉进程并恢复锚定 session」；重拉带重试预算与退避，预算耗尽才落挂起并记录 last_error；被中断的进行中的 agent turn 接受丢失（会话状态经 opencode 磁盘持久化恢复）
- 挂起路径必须区分「ocdeck 主动 kill（挂起/删除/reconcile）」与「进程自行退出」，前者不触发自动重拉
- 首次激活与每次自动重拉都 MUST 恢复本任务已解析并确认归属的锚定 session（沿用现有「预检 → 创建 → 原子 claim」路径），CLI session 选择/校验路径（`cli/tui/validate-session.ts` 等）纳入契约锚点核验
- opencode 契约锚点清单扩展：将 `tui.ts` 的 external 分支判断逻辑与 `tui/worker.ts` 的 server 启动路径纳入核验锚点（版本升级 SOP 同步核验）
- 移除 attach 专属会话与 ReopenAttach 中「仅重开 TUI」的路径（重开 = 进程在则重新 attach 客户端接入，进程不在则走自动重拉）

## Capabilities

### New Capabilities

（无）

### Modified Capabilities

- `opencode-orchestration`: 每任务独立 serve 实例、TUI attach 进程、进程退出监视等 Requirement 的行为发生 spec 级变更（双进程→单进程、TUI 消失语义、新增自动重拉要求）；端口分配、tmux 托管、SSE 归属、连接管理等 Requirement 大体保留但作用对象从 serve 进程变为单进程

## Impact

- **代码**：`internal/task`（激活/挂起/重开编排、进程监视与自动重拉）、`internal/infrastructure/process`（tmux 会话角色模型 serve/tui/shell → 单进程角色）、`internal/infrastructure/opencode`（CONTRACT.md 锚点扩展；API 客户端调用点不变，仅目标进程语义变化）
- **spec**：`openspec/specs/opencode-orchestration/spec.md` 多处 Requirement 修订
- **兼容性/部署前提**：升级前必须挂起全部任务；本变更不支持存活双进程会话的热迁移或兼容恢复，重启后旧 serve/tui 双会话一律按孤儿会话清理，任务以单进程模型重新激活
- **不变**：浏览器终端链路（xterm.js + WS + tmux attach 客户端）、shell 终端、SSE 归属捕获与原子 claim、端口分配与轮换策略、shutdownPolicy 三档、启动 reconciliation 框架
