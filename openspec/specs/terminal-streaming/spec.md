# Terminal Streaming Specification

## Purpose
经 xterm.js + WebSocket 桥接 tmux attach 客户端，为每个活跃任务提供原生 opencode TUI 终端与多个普通 shell 终端，支持断线重连与背压控制。

## Requirements

### Requirement: 浏览器终端会话
系统 SHALL 为每个活跃任务提供浏览器内的终端界面（xterm.js），通过 WebSocket 桥接 PTY 中的 tmux attach 客户端，与该任务的 tmux 会话双向通信，呈现原生 opencode TUI 体验。

#### Scenario: 打开任务终端
- **WHEN** 用户打开活跃任务的终端页
- **THEN** 浏览器建立 WebSocket 并渲染 TUI，用户可直接交互

#### Scenario: 挂起任务无终端
- **WHEN** 用户打开非活跃任务的终端页
- **THEN** 系统提示任务未运行并提供激活入口

### Requirement: 断线重连（tmux reattach）
任务进程本体 SHALL 运行于 tmux 会话中，浏览器侧 PTY 仅为 `tmux -L ocdeck attach -t <session>` 渲染客户端。WS 断开时系统 MUST 仅终止 attach 客户端 PTY（任务会话不受影响）；浏览器重连时 MUST 新建 attach 客户端 PTY 重新接入，由 tmux 推送当前正确屏幕——MUST NOT 依赖服务端输出缓冲回放。终端尺寸变化经 attach 客户端 winsize 由 tmux 自动传播到会话窗口。

#### Scenario: 刷新页面后恢复画面
- **WHEN** 用户刷新终端页或网络断开后重连
- **THEN** 新建 attach 客户端接入会话，tmux 重绘当前屏幕，随后无缝接入实时输出

#### Scenario: 断开不杀任务进程
- **WHEN** 浏览器 WS 断开（关页面/断网）
- **THEN** 仅 attach 客户端退出，opencode TUI 与任务进程在 tmux 会话中继续运行

### Requirement: 终端尺寸同步
系统 SHALL 支持浏览器终端尺寸变化时通过 JSON 控制消息同步 PTY winsize。

#### Scenario: 窗口缩放
- **WHEN** 用户调整浏览器窗口或终端面板大小
- **THEN** PTY 尺寸更新，TUI 正确重绘

### Requirement: 输出削峰与背压
系统 SHALL 对 PTY 输出做短窗口（≤16ms）批量合并后推送；每条 WebSocket 的写侧 MUST 使用独立 goroutine 与有界队列，慢客户端直接断开。

#### Scenario: 高频输出
- **WHEN** TUI 产生大量连续输出
- **THEN** 客户端收到按窗口合并的数据块，画面流畅

#### Scenario: 慢客户端
- **WHEN** 某客户端消费速度持续低于产出
- **THEN** 该连接被断开，不影响 PTY 与其他连接

### Requirement: 单交互客户端
同一终端同一时间 SHALL 只允许一个交互客户端；新连接建立时 MUST 替换（断开）旧连接。

#### Scenario: 第二个浏览器标签打开同一终端
- **WHEN** 同一终端已有活跃连接，新连接通过认证
- **THEN** 旧连接被断开，新连接接管

### Requirement: WebSocket 协议与认证
终端 WebSocket SHALL 使用二进制帧双向传输终端 IO，JSON 控制帧仅承载 auth/resize。首条消息 MUST 为 `{"type":"auth","token":"...","cols":N,"rows":M}`（认证与初始尺寸握手合一，5 秒超时，认证成功前不订阅 PTY），token MUST NOT 通过 query 参数传递。系统 MUST 校验 Origin、限制帧大小上限、使用原生 ping/pong。关闭码语义：4001 未认证、4009 被新连接替换、4010 任务已挂起、1011 服务端内部错误。

#### Scenario: 未认证连接
- **WHEN** 连接在超时内未完成首消息认证
- **THEN** 连接以 4001 关闭，未收到任何 PTY 数据

#### Scenario: 初始尺寸握手
- **WHEN** 客户端认证消息携带 cols/rows
- **THEN** 服务端以该尺寸创建 attach 客户端 PTY，tmux 将会话窗口调整到客户端尺寸，画面无尺寸跳变

### Requirement: 任务级 shell 终端
系统 SHALL 支持为每个任务创建多个普通 shell 终端（用户默认 `$SHELL`，运行于独立命名 tmux 会话 `ocdeck-<taskID>-shell-<n>`，cwd 为任务 worktree），与 opencode TUI 终端并列展示，浏览器同样经 tmux attach 客户端接入。shell 会话 MUST 注入与任务进程相同的环境（项目级 + 任务级 + 生命周期变量）。shell 终端复用与 TUI 终端相同的 PTY + WebSocket 通道。

#### Scenario: 新建 shell 终端
- **WHEN** 用户在活跃任务上新建 shell 终端
- **THEN** 系统创建 shell tmux 会话（cwd=worktree，注入任务 env）并接入 attach 客户端，浏览器出现新终端标签

#### Scenario: 多终端并存
- **WHEN** 用户为一个任务创建多个 shell 终端
- **THEN** 各终端相互独立，可分别关闭

#### Scenario: 挂起时 shell 终止
- **WHEN** 任务被挂起
- **THEN** 该任务全部 shell 终端进程一并终止，重新激活后需手动新建