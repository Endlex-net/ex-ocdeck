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

### Requirement: 终端进程 UTF-8 locale
宿主无有效 locale 配置时，系统 SHALL 保证 tmux 命令（含 attach 客户端）与任务会话进程运行在 UTF-8 locale 下：LANG/LC_ALL/LC_CTYPE 均未设置或为空时，系统 MUST 注入默认 `LANG=en_US.UTF-8`，此时 tmux attach 客户端的 `client_utf8` flag MUST 为 1，CJK 输出 MUST NOT 被转写为 `_` 或其他替代符号。宿主显式设置的非空 locale 变量（LANG/LC_ALL/LC_CTYPE，含非 UTF-8 值）MUST 原样透传到子进程环境，不得覆盖、不得纠正；此场景下终端 UTF-8 行为以用户配置为准。空串值（如 `LANG=`）MUST 视为未设置。

#### Scenario: 宿主无 locale 时注入默认
- **WHEN** ocdeck-server 进程环境未设置 LANG/LC_ALL/LC_CTYPE（如 launchd 启动），创建会话或 attach 客户端
- **THEN** 进程环境含 `LANG=en_US.UTF-8`，attach 客户端 `client_utf8=1`，中文原样输出

#### Scenario: 宿主显式 locale 被尊重
- **WHEN** 宿主显式设置了非空 LANG（任意非空值，含非 UTF-8）
- **THEN** 系统透传该值，不注入默认值、不覆盖

#### Scenario: 高位 locale 变量存在时不注入且原样透传
- **WHEN** 宿主未设 LANG 但已设非空 LC_ALL 或 LC_CTYPE
- **THEN** 系统不注入 LANG 默认值，且该高位变量 MUST 原样出现在子进程环境中

#### Scenario: 空串 locale 视为未设置
- **WHEN** 宿主 LANG 为空串且 LC_ALL/LC_CTYPE 未设置或为空
- **THEN** 系统注入默认 `LANG=en_US.UTF-8`

### Requirement: 终端文本 CJK 渲染
系统 SHALL 为浏览器终端（xterm.js）配置包含 CJK 回退字体的默认字体栈：等宽拉丁字体（JetBrains Mono / SF Mono / ui-monospace / Menlo / Consolas）之后 MUST 追加常见 CJK 系统字体（至少含 PingFang SC、Noto Sans Mono CJK SC、Microsoft YaHei 中的回退声明），使中文及其他 CJK 字符在装有对应字体的浏览器环境中正常渲染，而非退化为替代符号。

#### Scenario: 默认字体栈渲染中文
- **WHEN** 浏览器环境装有任一常见 CJK 系统字体，终端输出包含中文
- **THEN** 中文以 CJK 字体正常渲染，占 2 列宽度，不显示为 `_` 或方块

#### Scenario: 拉丁字符渲染不受回退链影响
- **WHEN** 终端输出仅含 ASCII/拉丁字符
- **THEN** 仍由字体栈前部的等宽拉丁字体渲染，列宽度量与现状一致

### Requirement: 终端外观偏好
系统 SHALL 在「全局配置」页提供「终端外观」配置（该偏好为浏览器端全局偏好，不放在任务级设置入口），允许用户自定义终端 fontFamily 与 fontSize（整数，合法范围 8–32）。偏好 MUST 存于浏览器 localStorage（key：`ocdeck.terminal.fontFamily` / `ocdeck.terminal.fontSize`），对当前浏览器所有任务的 TUI 与 shell 终端生效；未设置时 MUST 使用含 CJK 回退的默认字体栈与默认字号 13。系统 MUST 提供「恢复默认」操作（清除 localStorage 对应项）。保存时 MUST 先完整校验两个字段，全部合法后才写入 localStorage；任一字段非法 MUST NOT 修改任何存储项并提示用户。fontFamily 去除首尾空白后为空视为未设置（删除对应存储项，回到默认栈），允许单独保存合法 fontSize。

#### Scenario: 保存自定义字体
- **WHEN** 用户输入自定义 fontFamily 与合法 fontSize 并保存
- **THEN** 偏好写入 localStorage，当前页所有已打开终端即时按新偏好渲染

#### Scenario: 未设置偏好
- **WHEN** localStorage 无终端外观偏好
- **THEN** 终端使用含 CJK 回退的默认字体栈与字号 13

#### Scenario: 恢复默认
- **WHEN** 用户点击「恢复默认」
- **THEN** localStorage 对应项被清除，终端回到默认字体栈与字号

#### Scenario: 非法字号
- **WHEN** 用户输入越界、非整数或非数字 fontSize 并保存
- **THEN** 系统拒绝保存并提示，localStorage 中已有偏好（含 fontFamily）不被修改

#### Scenario: 空白字体栈
- **WHEN** 用户将 fontFamily 清空或仅输入空白并保存（fontSize 合法）
- **THEN** fontFamily 存储项被删除回到默认栈，fontSize 偏好正常保存

#### Scenario: 损坏的持久化数据
- **WHEN** localStorage 中偏好数据损坏或非法
- **THEN** 终端按默认值渲染，且读取过程不得改写 localStorage

#### Scenario: 存储不可用
- **WHEN** localStorage 读写抛出异常（如 SecurityError / quota 超限）
- **THEN** 读取失败时终端按默认值正常可用；保存失败时不派发变更事件、已打开终端保持现状，并向用户显示错误

### Requirement: 偏好变更即时生效
偏好保存或清除成功后，系统 MUST 将变更即时应用到当前页所有已打开的终端实例（TUI 与 shell），并 MUST 应用到同源其他浏览器标签页中的终端实例。应用方式 MUST 为就地更新 xterm `fontFamily`/`fontSize` 选项并重新 fit/同步尺寸，MUST NOT 重建终端实例、MUST NOT 断开或重连 WebSocket，浏览器侧 scrollback 与选择状态 MUST 保留。

#### Scenario: 同页全部终端即时生效
- **WHEN** 用户保存新偏好且当前页有多个已打开终端
- **THEN** 所有终端实例 WS 连接保持不断，就地切换到新字体/字号，终端尺寸重新同步，scrollback 保留

#### Scenario: 跨标签页生效
- **WHEN** 用户在标签页 A 保存偏好，同源标签页 B 也开着终端
- **THEN** 标签页 B 的终端通过 storage 事件即时应用新偏好

#### Scenario: 隐藏终端后续激活
- **WHEN** 偏好变更时某终端处于隐藏（inactive）标签
- **THEN** 其字体选项就地更新，下次激活时由现有 fit 逻辑完成尺寸适配，无异常