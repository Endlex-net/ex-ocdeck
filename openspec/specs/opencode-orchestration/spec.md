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
系统 SHALL 在 DB 记录任务上次成功端口（非事实来源）。激活时 MUST 先尝试上次端口；不可用时在可配置范围内（轮转起点）选择下一端口；serve 因端口占用退出时 MUST 自动换端口重试；健康检查通过后才将端口写回 DB。

#### Scenario: 端口冲突回退
- **WHEN** 候选端口被占用
- **THEN** 系统自动切换下一可用端口重试，而非激活失败

#### Scenario: 端口范围耗尽
- **WHEN** 可配置端口范围内无可用端口
- **THEN** 激活失败并返回明确的端口耗尽错误，任务回退挂起

### Requirement: TUI attach 进程
系统 SHALL 为每个活跃任务在命名 tmux 会话（`ocdeck-<taskID>-tui`）中运行 `opencode attach http://127.0.0.1:<port>`，会话工作目录 MUST 为任务 worktree；浏览器经 PTY 中的 `tmux -L ocdeck attach -t <session>` 客户端接入该会话。serve 密码 MUST 经会话 env（`new-session -e OPENCODE_SERVER_PASSWORD`）传递，MUST NOT 经 `--password` argv 传递（避免进程列表泄露）。全部 tmux 会话 MUST 只能由统一的 launcher 创建并强制 canonical worktree cwd（`-c` 参数）；对 serve 的全部 API 请求 MUST 显式携带 directory 参数。

#### Scenario: attach 连接到正确实例
- **WHEN** serve 进程就绪后
- **THEN** 系统创建 TUI tmux 会话，TUI 作用于本任务 worktree

### Requirement: serve 就绪等待与能力探测
系统 SHALL 在 serve 启动后轮询 `GET /global/health`，就绪后 MUST 做能力探测（`/session/status` 响应结构与 session 列表字段形状校验；**DELETE 响应形状 MUST NOT 做 live 探测**——不能为探测制造删除副作用，首次真实删除时校验，不符报 deletion_failed）；探测通过才启动 TUI 会话。健康检查超时或能力不兼容 MUST 判定激活失败，清理会话并回滚任务状态为挂起 + 记录 last_error。opencode 版本号 MUST NOT 作为激活门禁：版本与契约基准不一致时仅告警（日志 + UI 提示），不阻止激活。

#### Scenario: serve 启动超时
- **WHEN** serve 在超时时间内未通过健康检查
- **THEN** 激活失败，进程被清理，任务回到挂起并展示错误

#### Scenario: API 能力不兼容
- **WHEN** serve 返回的响应结构与 occlient 契约不符（版本号不一致仅告警，不触发本场景）
- **THEN** 激活被阻止并提示 opencode 契约兼容性问题

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
系统 SHALL 订阅每个活跃 serve 的 SSE 事件流（`GET /event`）：`session.created` 事件的 sessionID 位于 `properties.info.id`（OpenCode 1.18.9 契约）；同时监听 `session.updated` 刷新 `last_seen_at`。激活后 MUST 全量对齐一次该 directory 的 session 列表。SSE 断流时 MUST 指数退避重连，重连成功后 MUST 再次全量对齐。

#### Scenario: TUI 新建会话被记录
- **WHEN** 用户在 TUI 中新建会话
- **THEN** 新 sessionID 经 SSE 被捕获并与任务关联，用于后续恢复

#### Scenario: 断流后对齐
- **WHEN** SSE 连接断开并恢复
- **THEN** 系统重连后全量对齐 session 列表，断流期间错过的会话被补记

### Requirement: 进程退出监视
系统 SHALL 监视任务全部 tmux 会话的存活（has-session 轮询，serve 另有健康轮询）：serve 会话异常消失（非挂起路径）时任务 MUST 落为挂起并记录 last_error；TUI 会话消失而 serve 存活时任务保持活跃、终端标记为可重开。

#### Scenario: serve 崩溃
- **WHEN** 活跃任务的 serve 会话异常消失
- **THEN** 任务自动落为挂起，UI 展示 last_error，用户可重新激活

#### Scenario: TUI 退出
- **WHEN** 用户退出 TUI 导致其会话消失但 serve 存活
- **THEN** 任务保持活跃，重新打开终端页时重开 TUI 会话