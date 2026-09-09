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
### Requirement: 移动端模式偏好

系统 SHALL 在设置页「终端外观」提供「移动端模式」设置（该偏好为浏览器端本机偏好，不跟账号、不放在任务级设置入口），控制终端锁定、触控手势、键盘避让三项移动端终端能力的启用（各能力语义见 `mobile-terminal-adaptation` spec）。

偏好 MUST 存于浏览器 localStorage，共两个 key：模式 key `ocdeck.terminal.mobileMode`（取值 `auto` | `on` | `off`，缺省 `auto`）；子开关记录 key `ocdeck.terminal.mobileCaps`（值为带 `version: 1` 的 JSON 对象，含 `lock` / `gestures` / `keyboardAvoid` 三个布尔字段，缺省全 `true`）。子开关仅在模式为「开启」（`on`）时展示并可编辑；模式为「自动」或「关闭」时 MUST NOT 展示子开关，且 MUST NOT 读取 `mobileCaps` 存储值（读取 MUST NOT 发生，而非读取后忽略）；模式为「关闭」时 MUST 保留 `mobileCaps` 存储值不丢失。模式切换 MUST 只写 `mobileMode` key；子开关变更 MUST 一次性写入完整 `mobileCaps` JSON——终端锁定子开关开启时若手势为关，MUST 在同一次写入中置 `gestures: true`，且终端锁定开启时触控手势子开关 MUST 展示为开且不可关闭（避免「锁定开 + 手势关」的不可滚动组合）。

缺省（无任何存储项）行为 MUST 与设置项引入前一致：自动模式 + 出厂默认。读取容错：非法 mode 值 MUST 回退 `auto`；`mobileCaps` JSON 解析失败、缺字段、字段类型错误或 `version` 未知 MUST 整项回退默认（三字段全 `true`）；读取抛异常（如 `SecurityError`）MUST 按默认值返回；任何读取失败场景 MUST NOT 改写 localStorage。存储写入失败时 MUST 向用户显示错误、MUST NOT 派发变更事件、已打开终端 MUST 保持现状（不得出现部分 key 已生效的半更新状态）。

偏好变更 MUST 即时应用到当前页所有已打开的终端实例（TUI 与 shell），并 MUST 应用到同源其他浏览器标签页中的终端实例（与终端外观字体偏好同一变更通道）；应用过程 MUST NOT 重建终端实例、MUST NOT 断开或重连 WebSocket，浏览器侧 scrollback 与连接状态 MUST 保留。锁定能力未发生启用/禁用边沿变化时，变更 MUST NOT 重新锁定终端、MUST NOT 改变终端焦点（保护用户手动解锁后的会话）；仅在锁定能力发生边沿变化时才允许相应的锁定/解锁与焦点副作用（见 `mobile-terminal-adaptation` spec「移动端模式启用判定」）。「恢复默认」操作 MUST 在清除字体偏好的同时尝试清除 `mobileMode` 与 `mobileCaps` 两个存储项。恢复默认是多 key 删除，无法原子化，采用 best-effort：逐个尝试清除全部目标 key 并收集失败；只要有任一删除成功 MUST 派发变更事件、UI MUST 从实际存储重载收敛；存在失败时 MUST 显示「部分偏好未清除」并允许重试。

#### Scenario: 选择移动端模式立即生效

- **WHEN** 用户在设置页将移动端模式从「自动」切换为「关闭」
- **THEN** 当前页所有已打开终端立即停用锁定/手势/键盘避让，WebSocket 与终端实例不受影响；刷新或其他标签页打开后同样生效

#### Scenario: 开启模式展开子开关

- **WHEN** 用户将移动端模式切换为「开启」
- **THEN** 设置页展示终端锁定、触控手势、键盘避让三项开关（默认开），可分别编辑

#### Scenario: 自动模式不展示不读取子开关

- **WHEN** 移动端模式为「自动」
- **THEN** 设置页不展示三项子开关；即使 localStorage 中存在此前保存的 `mobileCaps`，其值不被读取，终端行为按出厂默认执行

#### Scenario: 锁定开启时手势强制开启

- **WHEN** 模式为「开启」且终端锁定开关为开
- **THEN** 触控手势开关处于开且不可关闭；用户开启终端锁定时若手势为关，手势在同一次存储写入中被自动置开

#### Scenario: 修改非锁定子项不得重新锁定

- **WHEN** 终端锁定能力保持启用、用户已手动解锁终端，随后修改触控手势或键盘避让子开关（或修改字体等无关偏好）
- **THEN** 终端保持解锁状态，不被重新锁定，焦点不被改变

#### Scenario: 跨标签页同步

- **WHEN** 用户在标签页 A 修改移动端模式或子开关
- **THEN** 同源标签页 B 中已打开的终端即时按新偏好调整，无需刷新

#### Scenario: 损坏的持久化数据

- **WHEN** localStorage 中 `mobileMode` 为非法值，或 `mobileCaps` JSON 解析失败/缺字段/类型错误/version 未知
- **THEN** 对应项按默认值（模式 `auto` / 子开关全 `true`）生效，且读取过程不改写 localStorage

#### Scenario: 恢复默认

- **WHEN** 用户在终端外观点击「恢复默认」
- **THEN** `mobileMode` 与 `mobileCaps` 存储项与字体偏好一并清除，终端行为回到自动模式出厂默认

#### Scenario: 恢复默认部分失败

- **WHEN** 恢复默认过程中部分 key 删除抛异常
- **THEN** 已删除的 key 生效并派发变更事件、UI 按实际存储收敛，同时显示「部分偏好未清除」并允许重试；无任何 key 删除成功时不派发事件

### Requirement: 触屏设备终端输入锁定

当终端锁定能力启用时（启用判定见 `mobile-terminal-adaptation` spec「移动端模式启用判定」），终端流 SHALL 支持输入锁定状态：锁定时终端继续接收并渲染全部 PTY 输出，但浏览器侧除合成手势产生的滚动控制字节外不产生任何 stdin 数据（textarea 不聚焦、键盘/IME/粘贴零发送；鼠标/触摸控制序列等非 UTF-8 字节亦经统一门禁拦截）。每次 WS 连接建立（含一切重连、Tab 切换）后 MUST 回到锁定状态。锁定/解锁 MUST NOT 触发 WebSocket 重连或 PTY 重建。终端锁定能力关闭时 MUST NOT 在连接建立后进入锁定状态。

#### Scenario: 锁定期间输出不中断

- **WHEN** 终端锁定能力启用且终端处于锁定状态，任务持续产生输出
- **THEN** 终端正常渲染实时输出，WS/PTY 链路无任何重建

#### Scenario: 锁定期间零意外输入

- **WHEN** 终端锁定能力启用且终端处于锁定状态，用户触摸终端区域、敲击外接键盘、发生 IME composition 尾事件或产生鼠标/触摸控制序列
- **THEN** 不产生任何意外 stdin 字节（含非 UTF-8 控制序列）发送到 PTY

#### Scenario: 断线重连后回到锁定

- **WHEN** 终端锁定能力启用，终端 WS 断开并自动重连成功（含 Tab 切换引起的重连）
- **THEN** 终端回到锁定状态（防误触优先），输出渲染恢复

#### Scenario: 锁定能力关闭时重连不回锁

- **WHEN** 终端锁定能力关闭（移动端模式为关闭，或开启模式下锁定子开关为关），终端 WS 断开并重连成功
- **THEN** 终端不进入锁定状态，保持可交互

### Requirement: Shift+Enter 换行输入

终端 SHALL 将 Shift+Enter 按键翻译为 opencode TUI 可识别的换行输入，而不是普通回车提交。xterm.js 对 Enter 与 Shift+Enter 均发送 `\r`（shift 修饰键在终端数据层丢失），浏览器侧 MUST 在键盘事件层拦截 Shift+Enter 并向终端输入流发送 TUI 可区分的换行序列；普通 Enter（无修饰键）行为 MUST 保持不变。IME 组合进行中（composition 或 IME process key 事件）MUST NOT 触发该拦截翻译。生效范围与迁移边界：修复对新建与重建的终端会话生效；修复发布前已在运行且未重建的存量会话可维持旧行为，任务重新激活（会话重建）后生效。

#### Scenario: Shift+Enter 插入换行

- **WHEN** 终端聚焦且非 IME 组合状态，用户按下 Shift+Enter
- **THEN** opencode TUI 收到换行输入（在输入框中插入换行），MUST NOT 提交发送消息

#### Scenario: 普通 Enter 提交行为不变

- **WHEN** 终端聚焦，用户按下无修饰键的 Enter
- **THEN** 行为与修复前一致（opencode TUI 收到普通回车）

#### Scenario: IME 组合中不拦截

- **WHEN** IME composition 进行中或收到 IME process key 事件，用户按下 Shift+Enter
- **THEN** 系统 MUST NOT 发送换行翻译序列，事件交由 IME 流程处理

### Requirement: IME 组合输入去重

终端 IME 输入补偿机制 SHALL 保证同一段用户输入只发送一次。交付前提：仅当输入门禁通过（已认证、连接可写、终端未锁定）时 IME 内容才可交付；终端锁定状态 MUST 保持零发送（锁定契约优先于恰好一次）。恰好一次的适用范围为可复现且可证明的路径：正常上屏 MUST 交付 composition 提交文本恰好一次；中文输入法 composition 进行中切换输入法等异常结束路径，已敲内容 MUST 按输入法实际 commit 的文本恰好交付一次，MUST NOT 出现片段重复（如输入 "nihao" 欲得「你好」，切换后终端不得收到 "hi haohihao" 类重复）；不可证明的候选 MUST fail-closed 丢弃（漏发优先于双发）。任何失败或不确定路径 MUST NOT 补发，MUST NOT 通过退格或重发整串修正已发送内容。

#### Scenario: 组合中途切换输入法不重复

- **WHEN** 用户使用中文输入法输入拼音（如 "nihao"）尚未上屏，直接切换到英文输入法导致 composition 结束
- **THEN** 终端最终收到输入法实际 commit 的文本恰好一份（如 "nihao"），无任何片段重复

#### Scenario: 正常上屏不重复

- **WHEN** 用户完成中文输入并正常上屏（compositionend 正常触发）
- **THEN** 上屏文本恰好发送一次

### Requirement: 终端链接修饰键点击打开

终端 SHALL 识别输出中的链接并支持修饰键点击打开：纯文本 `http(s)://` URL 与程序输出的 OSC 8 超链接均 MUST 可识别；Cmd+点击（macOS）或 Ctrl+点击（其他平台）MUST 在用户手势中同步请求当前浏览器以新标签页/窗口打开该链接（具体呈现由浏览器决定；被阻止时 MUST NOT 异步重试、MUST NOT 转为当前页导航、MUST NOT 影响终端会话）；不带修饰键的普通点击 MUST NOT 触发打开。终端锁定状态（见「触屏设备终端输入锁定」）下 overlay 拦截全部 pointer 事件，链接**不可点击**；解锁后可点击。修饰键点击链接 MUST NOT 产生键盘输入数据（鼠标报告行为按 xterm 既有语义，机制见 `openspec/changes/archive/2026-09-09-terminal-links-emoji-icons/design.md` D1）。链接打开 MUST NOT 影响终端会话、输入门禁与既有键盘/IME 行为。

#### Scenario: Cmd+点击打开纯文本 URL

- **WHEN** 终端解锁且输出包含纯文本 `https://example.com/path`，用户 Cmd+点击该 URL 文本
- **THEN** 请求当前浏览器以新标签页/窗口打开该 URL，终端无键盘输入数据产生

#### Scenario: 普通点击不触发

- **WHEN** 用户不带修饰键点击终端中的链接文本
- **THEN** 不打开浏览器，保持既有点击/选择行为

#### Scenario: OSC 8 超链接可点击

- **WHEN** 终端解锁且程序输出 OSC 8 超链接（如 opencode markdown 链接），用户 Cmd+点击链接文本
- **THEN** 请求当前浏览器以新标签页/窗口打开链接目标 URL

### Requirement: Emoji grapheme cluster 渲染

终端 SHALL 正确渲染 emoji：**字形覆盖（彩色优先）**——终端字体栈 MUST 包含系统 emoji 字体回退（`Apple Color Emoji` / `Segoe UI Emoji` / `Noto Color Emoji`）；在字体可用、字体支持目标序列且浏览器支持彩色渲染时：默认 emoji 呈现字符（`Emoji_Presentation=Yes`）、含 VS16 的 emoji 呈现序列、有效 ZWJ/旗帜组合序列 MUST 渲染为可见彩色图形而非豆腐块；默认文本呈现且未请求 emoji 呈现的字符（如裸 ❤、©）以前序正文字体为准（对默认栈与自定义栈同样适用）；emoji 码点被用户自定义栈中字体覆盖时以用户字体为准。**宽度与分段**——组合字符按 grapheme cluster 计算：ZWJ 组合 emoji（如 👨‍👩‍👧）、肤色修饰符、旗帜 emoji、组合附加符号 MUST 作为单个簇渲染与占位，MUST NOT 拆散为多个字符或产生宽度错位。普通 ASCII/CJK 文本的既有宽度行为 MUST NOT 改变。emoji 系统族名为声明式引用（不下载不打包），其缺失平台（无对应系统字体）降级为豆腐块、不影响终端其余功能。

#### Scenario: 组合 emoji 单簇渲染

- **WHEN** 终端程序输出 ZWJ 组合 emoji 序列（如 👨‍👩‍👧），且系统 emoji 字体可用、支持该序列、浏览器支持彩色渲染
- **THEN** 终端将其作为单个 grapheme cluster 渲染为可见彩色图形，光标与后续字符按单簇宽度推进

#### Scenario: 连续 emoji 不粘连

- **WHEN** 终端程序输出连续多个 emoji（如 😀😀😀，或 ZWJ 序列与普通 emoji 混合）
- **THEN** 每个 emoji 独立占据正确的单元格宽度，互不重叠/粘连（用户实证：修复前 TUI 中连续 emoji 粘在一起）

#### Scenario: 普通文本宽度不变

- **WHEN** 终端输出纯 ASCII 或 CJK 文本
- **THEN** 宽度计算与换行行为与修复前一致

### Requirement: Nerd Font 图标字形内置

终端 SHALL 内置图标字体回退，使 Nerd Font 私有区（PUA）字形（文件夹、分支、提示符符号等）无需用户本地安装字体即可渲染。图标字体 MUST 追加在有效字体栈的**绝对末尾**（含 generic 字体之后），且其 `@font-face` 声明 MUST 以 `unicode-range` 限定为 Nerd Font 图标码点范围：正文与 CJK 字符的字体选择（含用户自定义字体偏好，见终端外观偏好）MUST NOT 被改变，仅当栈中所有字体均不含某字形且码点落在约定图标范围时才使用图标字体。该变换只作用于运行时有效栈，MUST NOT 写回用户偏好存储。

#### Scenario: shell 主题图标正常显示

- **WHEN** 终端程序输出 Nerd Font PUA 图标（如 starship 主题的文件夹/分支图标）
- **THEN** 图标以图标字体渲染为可见图形，不显示为豆腐块（□）

#### Scenario: 用户自定义字体优先级保持

- **WHEN** 用户已设置自定义终端字体偏好
- **THEN** 正文/CJK 字符仍按用户字体渲染，仅用户字体栈缺失的图标字形走内置回退
