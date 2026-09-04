# Mobile Terminal Adaptation Specification

## Purpose

为任务工作台页（TaskWorkbenchPage）提供移动端/iPad 适配能力：视口与安全区基础、响应式布局、终端触控滚动与触摸点击、触屏锁定模式、虚拟键盘视口适配、应用侧 IME 直交字符补偿（修复 xterm.js 5.5.0 在 WebKit/Safari 丢失全角标点的上游缺陷）。桌面端行为保持不变；适配范围仅限工作台页。

## Requirements

### Requirement: 视口与安全区基础
系统 SHALL 在 web 入口设置 `viewport-fit=cover`，使用 `dvh` 高度单位（含 `vh` fallback）与 `env(safe-area-inset-*)` 安全区 padding，使布局在 iOS Safari 地址栏伸缩与刘海设备上不被遮挡。

#### Scenario: iOS Safari 地址栏伸缩
- **WHEN** 用户在 iPhone/iPad Safari 打开应用并滚动导致地址栏伸缩
- **THEN** 工作台布局高度跟随可视视口正确收缩/伸展，终端与按钮不被遮挡

#### Scenario: 刘海设备横屏
- **WHEN** 用户在带刘海的 iPhone/iPad 横屏使用
- **THEN** 关键操作按钮与终端内容避开安全区，不被圆角/刘海裁切

### Requirement: 工作台响应式布局
系统 SHALL 按视口宽度分流工作台布局：`≥1025px` 保持桌面布局不变（含 iPad Pro 横屏）；`≤1024px` 重组为窄屏形态——tabstrip 横向滚动、header 操作区收窄、固定宽度侧栏（git 340px）与宽弹窗（720px）自适应。审批确认、任务进度、推理历史 SHALL 全部经由 TUI 终端 tab 完成，不新增 web 原生面板。布局分流 MUST 只看视口宽度，与触屏锁定判定（pointer）正交。

#### Scenario: iPad 竖屏打开工作台
- **WHEN** 用户在 768-1024px 视口打开任务工作台
- **THEN** TUI/shell/Git/设置 tab 可横向滚动切换，无布局溢出

#### Scenario: 手机打开工作台
- **WHEN** 用户在 ≤767px 视口打开任务工作台
- **THEN** 用户可切换到 TUI tab 查看任务进度与推理历史、进入 Git tab 查看变更，所有操作按钮可达且无溢出

#### Scenario: iPad Pro 横屏
- **WHEN** 用户在 ≥1025px 宽度的触屏设备横屏打开工作台
- **THEN** 使用桌面式布局，且终端仍按触屏设备默认锁定

#### Scenario: 桌面端零变化
- **WHEN** 用户在 ≥1025px 且 fine pointer 设备打开工作台
- **THEN** 布局与交互与适配前完全一致

### Requirement: 终端触控滚动（含 TUI）

当触控手势能力启用时（启用判定见「移动端模式启用判定」；终端锁定开启时本能力 MUST 视为开启），终端 SHALL 支持单指垂直拖拽滚动。手势路由 MUST 按每次手势时的 xterm 公开运行时状态（`term.modes.mouseTrackingMode` 与 `term.buffer.active.type`）判定，不得按终端类型静态分流：锁定状态下由锁定 overlay 接管拖拽并合成 WheelEvent 派发到 xterm 元素；解锁状态下仅当 normal buffer 且无 mouse tracking 时放行 xterm 原生触控滚动，其余（alternate buffer 或 mouse tracking active）由手势层合成 WheelEvent。合成 wheel MUST 复用 xterm 原生 wheel 路径（普通 buffer 滚 scrollback / alt buffer 转方向键 / mouse reporting 转滚轮序列），不得直接操作 xterm 内部 viewport。手势 MUST 仅在单指且垂直位移占优时接管，tap 与拖拽以位移阈值区分。触控手势能力关闭时 MUST NOT 挂载手势层，触控行为回退为 xterm 原生处理。

#### Scenario: 锁定状态滚动查看输出

- **WHEN** 触屏设备终端处于锁定状态，用户单指垂直拖动终端区域
- **THEN** shell 终端滚动画面的 scrollback 历史，TUI 终端滚动 TUI 内历史（推理历史），页面不发生冲突滚动

#### Scenario: 解锁后 shell 运行全屏程序

- **WHEN** 用户在解锁的 shell 终端中运行 vim/htop（进入 alternate buffer 或开启 mouse tracking）并单指拖动
- **THEN** 手势层接管并合成 wheel 事件，程序内滚动正常（不经 xterm 原生 touch 路径）

#### Scenario: 非终端面板滚动不受影响

- **WHEN** 用户在 Git 面板或设置区域滑动
- **THEN** 该区域内部滚动容器原生滚动，不受终端手势逻辑影响

#### Scenario: 触控手势关闭时回退原生

- **WHEN** 触控手势能力关闭（开启模式下手势子开关为关且终端锁定同为关，或模式为关闭）
- **THEN** 终端不挂手势层，触摸交互完全由 xterm 原生监听处理

### Requirement: 触摸点击 TUI

触控手势能力启用时（启用判定见「移动端模式启用判定」），解锁状态下当 xterm 运行时 mouse tracking active，终端 SHALL 支持 tap（位移小于阈值、短时长）转换为合成 mousedown/mouseup 事件，经 xterm 鼠标协议路径转为点击序列发送给 TUI，以实现触摸点击审批确认等 TUI 内交互。合成点击 MUST 对原 touch 序列 `preventDefault()` 抑制浏览器 compatibility mouse 事件，保证一次 tap 恰好产生一次点击（防审批双确认）。本需求以 spike 验证 opencode TUI 在真实 web/xterm→WS→tmux 链路开启 mouse reporting 为前提；spike 阴性时本能力不得交付（升级决策），不得静默降级验收口径。

#### Scenario: 触摸点击审批

- **WHEN** 用户在 iPad 解锁 TUI 终端并轻点 TUI 中的审批选项
- **THEN** 点击坐标转为鼠标点击序列发送给 TUI，TUI 响应点击（审批确认）

#### Scenario: 一次 tap 不双触发

- **WHEN** 用户在 TUI 中轻点任意可点击元素一次
- **THEN** TUI 恰好收到一次点击事件（compatibility mouse 事件已被抑制），不发生重复确认

#### Scenario: mouse tracking 未激活时不合成点击

- **WHEN** 终端运行时 `mouseTrackingMode` 为 none（如普通 shell 提示符）
- **THEN** tap 不合成鼠标事件，不产生任何 stdin 发送

### Requirement: 移动端模式启用判定

移动端终端能力（终端锁定、触控手势、键盘避让）的启用 SHALL 由本机「移动端模式」偏好决定（设置与存储见 `terminal-streaming` spec「移动端模式偏好」）：

- 模式为「自动」时，沿用现有设备自适配：触屏设备（`pointer: coarse`）启用终端锁定与触控手势且 MUST 使用出厂默认；桌面端（fine pointer）不启用这两项；自动模式 MUST NOT 读取任何自定义子开关存储值（读取 MUST NOT 发生，而非读取后忽略）。键盘避让在自动模式下不受本设置的开关影响：启用范围 MUST 保持既有现状（解锁且聚焦时启用，与 pointer 类型无关）；避让算法按「虚拟键盘视口适配」的阈值规则统一适用于所有模式。
- 模式为「开启」时，无论设备指针类型 MUST 按移动端终端启用三项能力，各项是否启用由对应子开关决定。
- 模式为「关闭」时，三项能力 MUST 全部不启用：不出现锁定 overlay 与锁定/解锁按钮、不在重连后强制锁定、不挂触控手势层、不做键盘避让——即使设备为触屏。
- 子开关取值仅在模式为「开启」时生效；切换模式 MUST NOT 改写或丢失子开关的已存值。
- 终端锁定开启时，触控手势 MUST 视为开启：不允许「锁定开 + 手势关」的组合（锁定时滚动依赖 overlay 上的手势层）。
- 模式或子开关变更 MUST 即时作用于当前浏览器全部已打开终端（TUI 与 shell），MUST NOT 重建终端实例、MUST NOT 断开或重连 WebSocket、MUST NOT 丢失 scrollback。能力启用状态未发生边沿变化时，变更 MUST NOT 重新锁定终端、MUST NOT 改变终端焦点（保护用户手动解锁后的会话）。运行中禁用终端锁定（边沿 true→false）MUST 立即移除锁定 overlay 且 MUST NOT 主动 focus（避免注入 focus-in 控制序列）；运行中启用终端锁定（边沿 false→true）MUST 立即进入锁定（门禁置位后 blur）。
- 每次 WS 连接建立（含一切重连、Tab 切换）为上述边沿保护的唯一例外：只要终端锁定能力当前启用，MUST 强制回锁（沿用既有语义，会话锁定状态仍不持久化）。

pointer 类型动态变化的重评估仅在模式为「自动」时按既有语义生效；模式为「开启」或「关闭」时 pointer 变化 MUST NOT 改变三项能力的启用状态。

#### Scenario: 自动模式触屏设备使用出厂默认

- **WHEN** 用户在触屏设备打开任务终端，且移动端模式为自动（含从未设置）
- **THEN** 终端锁定与触控手势按出厂默认启用；键盘避让启用范围保持既有现状（与 pointer 无关），但算法使用本次阈值启发式

#### Scenario: 自动模式桌面端不启用锁定与手势

- **WHEN** 用户在桌面浏览器（fine pointer）打开任务终端，且移动端模式为自动
- **THEN** 终端锁定与触控手势不启用，不显示锁定 UI；键盘避让启用范围保持既有现状（与 pointer 无关，算法按阈值规则统一适用）；即使用户此前在「开启」模式下修改过子开关，这些值也不被读取

#### Scenario: 开启模式强制启用并可分别配置

- **WHEN** 用户将移动端模式设为「开启」，并将「终端锁定」子开关关闭（手势、避让保持开）
- **THEN** 当前浏览器全部终端立即不锁定、不显示锁定按钮、重连后不回锁，触控手势与键盘避让仍生效

#### Scenario: 关闭模式整包停用

- **WHEN** 用户在触屏设备将移动端模式设为「关闭」
- **THEN** 终端不出现锁定 overlay 与锁定按钮、重连后不强制锁定、触控手势层不接管、弹出虚拟键盘时终端高度不收缩

#### Scenario: 关闭后回到开启恢复子开关

- **WHEN** 用户在「开启」模式下将终端锁定关闭，随后切到「关闭」，再切回「开启」
- **THEN** 终端锁定仍为关闭（上次选择被保留），其余子开关同样恢复各自上次值

#### Scenario: 开启模式下外接鼠标/触控板不改变启用状态

- **WHEN** 移动端模式为「开启」，设备 pointer 由 coarse 变为 fine（如 iPad 接入触控板）
- **THEN** 三项能力的启用状态不因此改变（仍按子开关执行）

#### Scenario: 运行中禁用锁定立即生效

- **WHEN** 终端当前处于锁定状态，用户在设置中将「终端锁定」子开关关闭（或将模式切为「关闭」）
- **THEN** 锁定 overlay 立即移除、可正常交互，WebSocket 不断开、终端实例不重建、scrollback 保留，且不主动 focus 终端

#### Scenario: 运行中启用锁定立即生效

- **WHEN** 终端当前未锁定，用户将「终端锁定」子开关开启（或模式切为「开启」且锁定子开关为开）
- **THEN** 终端立即进入锁定（门禁先置位再 blur），overlay 覆盖终端

#### Scenario: 自动模式保留 pointer 动态重评估

- **WHEN** 移动端模式为「自动」，锁定中的 iPad 接入外接鼠标/触控板（pointer 变为 fine）
- **THEN** 终端自动解锁、锁定 UI 隐藏（沿用既有语义）

### Requirement: 终端锁定模式

当终端锁定能力启用时（启用判定见「移动端模式启用判定」），终端 SHALL 在每次 WS 连接建立（含一切重连、Tab 切换）后进入锁定状态：透明 overlay 拦截全部 pointer/touch（textarea 不聚焦、虚拟键盘不弹出），`term.blur()` 保证硬件键盘无输入目标。统一输入门禁 MUST 覆盖 xterm 的全部发送出口（`onData` 与 `onBinary`，后者承载鼠标控制序列等非 UTF-8 字节），锁定期除合成手势产生的滚动字节外零 stdin 发送。门禁状态 MUST 先于 `term.blur()` 生效，防止 focus-out 控制序列在锁定瞬间泄漏。用户 SHALL 可显式解锁交互、随时重新锁定。锁定状态 SHALL NOT 持久化。终端锁定能力关闭时 MUST 不出现锁定 overlay、MUST NOT 在重连后强制锁定、MUST NOT 显示锁定/解锁按钮。pointer 类型动态变化（如 iPad 接/拔外接鼠标或触控板）时系统 MUST 在自动模式下重评估：转入 fine 自动解锁并隐藏锁定 UI，转入 coarse 强制锁定并显示锁定 UI；开启或关闭模式下 pointer 变化不改变启用状态。锁定/解锁 MUST NOT 中断 WS/PTY 数据流或触发重连。

#### Scenario: 触屏设备默认锁定

- **WHEN** 用户在 iPad/手机打开任务终端，且终端锁定能力启用（自动模式触屏，或开启模式且锁定子开关为开）
- **THEN** 终端锁定，点击终端区域不弹键盘、不产生输入，输出正常渲染

#### Scenario: 重连强制锁定

- **WHEN** 终端锁定能力启用的设备上终端 WS 断开重连成功（含切换 tab 引起的重连）
- **THEN** 终端回到锁定状态，不论断开前是否已解锁

#### Scenario: 锁定期零意外输入

- **WHEN** 终端锁定，用户敲击外接硬件键盘、发生 IME composition 尾事件或产生鼠标/触摸控制序列
- **THEN** 无任何字符或控制序列经 stdin 发送到 PTY（onData 与 onBinary 双出口均被门禁拦截）

#### Scenario: 锁定期滚动手势放行

- **WHEN** 终端锁定，用户执行触控滚动手势
- **THEN** 手势产生的滚动控制字节（wheel/方向键序列）经同步来源标记放行，TUI 正常滚动

#### Scenario: 外接鼠标/触控板接入自动解锁（自动模式）

- **WHEN** 移动端模式为自动，锁定中的 iPad 接入外接鼠标/触控板（`pointer` 媒体特性变为 fine）
- **THEN** 终端自动解锁、锁定 UI 隐藏，用户可直接输入；拔出后恢复锁定。注：仅外接键盘（无指针设备）不改变 `pointer`，终端保持锁定，用户可手动解锁或将移动端模式设为开启后关闭终端锁定——`pointer` 描述主指针设备，无法检测键盘

#### Scenario: 解锁与重新锁定

- **WHEN** 用户点击「解锁输入」完成输入后点击「锁定」
- **THEN** 解锁期终端可聚焦、键盘唤起、输入进入 TUI；重新锁定后恢复只读，全程 WS/PTY 无重建

#### Scenario: 自动模式桌面端无锁定干扰

- **WHEN** 移动端模式为自动，用户在桌面浏览器（fine pointer）打开终端
- **THEN** 终端直接可交互，不显示锁定/解锁按钮

#### Scenario: 终端锁定关闭时无锁定干扰

- **WHEN** 终端锁定能力关闭（模式为关闭，或开启模式下锁定子开关为关）
- **THEN** 任何设备上终端均不出现锁定 overlay、不显示锁定/解锁按钮、重连后不强制锁定

### Requirement: 虚拟键盘视口适配

当键盘避让能力开启时（启用判定见「移动端模式启用判定」），系统 SHALL 在解锁且终端聚焦时监听 `visualViewport` 的 resize 与 scroll 事件（Safari 为保持输入点可见会平移 visual viewport），且仅当可视视口被明显压缩（虚拟键盘弹出量级，具体阈值见 design）时，才按 `max(0, visualViewport.offsetTop + visualViewport.height - 容器 top)` 调整终端外层容器（承载终端画面、锁定 overlay 与浮动按钮的 wrap）高度并触发 FitAddon 重排与 PTY resize 同步，保证输入区与浮动锁定按钮不被虚拟键盘遮挡；blur/锁定后恢复原高度。浏览器工具栏伸缩等轻微压缩 MUST NOT 触发高度调整。键盘避让能力关闭时 MUST NOT 监听 visualViewport、MUST NOT 调整终端高度。visualViewport API 缺失时 SHALL 跳过（可接受遮挡降级）。

#### Scenario: 解锁弹出键盘

- **WHEN** 键盘避让开启，用户在 iPad/手机解锁终端并弹出虚拟键盘
- **THEN** 终端可视高度收缩，光标行保持可见，PTY cols/rows 同步更新

#### Scenario: 收起键盘恢复

- **WHEN** 用户收起虚拟键盘或重新锁定
- **THEN** 终端恢复全高布局并重排

#### Scenario: 浏览器工具栏伸缩不触发收缩

- **WHEN** 键盘避让开启，终端聚焦且未弹出虚拟键盘，Safari 地址栏/工具栏伸缩导致可视视口轻微变化
- **THEN** 终端高度不做收缩调整

#### Scenario: 键盘避让关闭时不收缩

- **WHEN** 键盘避让能力关闭，用户解锁终端并弹出虚拟键盘
- **THEN** 终端高度保持不变（键盘可能遮挡输入区，为用户显式选择的降级）

### Requirement: 应用侧 IME 直交字符补偿
系统 SHALL 在应用层补偿 xterm.js 5.5.0 丢失 IME 直交字符（全角标点 `？《》` 等）的上游缺陷（xtermjs/xterm.js#5887），实现方式为**候选输入仲裁**：对终端 textarea 的 capture 阶段事件镜像做候选资格判决（仅当 xterm gate 必然丢弃该事件时才成为候选），候选挂起一个宏任务后结算——观测 xterm 原生 `onData` 实际发出的内容，仅对未被原生发送覆盖的候选经 `term.input()` 公开 API 补发，全平台启用。MUST NOT 修改、升级或 patch `@xterm/xterm` 依赖包，MUST NOT 依赖 xterm 私有字段；应用层补偿代码 MUST NOT 修改 textarea.value（composition/229 路径的 deferred diff 依赖其内容；xterm 自身的清空行为不受此限）。**补偿契约为有界保证**：对 settle 前已观测且位于 100ms 回看窗口内的原生覆盖 MUST NOT 补发；判决 MUST fail-closed——资格判决任一条件不确定、或原生覆盖因裁剪/容量溢出不可证明时，宁可漏发不可双发；原生发射观测 MUST 时间有界（惰性裁剪 MUST NOT 丢弃落在 pending 候选回看窗口内的记录 + 硬容量上限 + **历史缺口水位**：容量溢出 MUST 记录缺口产生时刻，新候选回看窗口与缺口重叠时立即标记不可证明）且按文本出现次数消耗（occurrence 级：消耗一次文本出现而非整条记录，聚合原生发射如 `onData("？？")` 可依次覆盖多个相同候选；多条记录可匹配时取时间最早者（时间戳相同取插入序）、耗尽记录立即移出）；**候选 MUST 限定单 Unicode code point**（`[...ev.data].length === 1`，补偿目标为单个全角标点；多 code point 的 ev.data 无法做精确 occurrence 分配 → fail-closed 丢弃不进队列），不得形成长期残留的状态守卫；**在候选资格与覆盖历史均可证明的前提下**，除下述显式残余外，所有既有输入路径（普通按键、composition 提交、粘贴、iOS 预测/听写）MUST 保持恰好单次发送。**显式记录的残余风险**：（a）xterm 5.5.0 自身在「迟到重复 commit 且无按键按住」时存在原生双发缺陷，无补偿器时同样存在，不属于本补偿契约范围；（b）超出 100ms 观测窗口的迟到重复 commit 可能补发重复内容——在「不改包」约束下公开事件仲裁无法区分超窗迟到重复与用户再次输入相同字符，产品决策接受该残余换取目标字符可靠补发；（c）100ms 窗口内与候选同文本的无关原生发射可能误抑制候选造成漏发——方向为 fail-closed，用户重按即可恢复，作为已接受 trade-off 记录。

#### Scenario: Safari 输入全角标点
- **WHEN** 用户在 macOS/iOS Safari 通过按住 Shift 输入 `？《》` 等全角标点，且候选资格与覆盖历史均可证明
- **THEN** 标点字符经仲裁补发进入 TUI 输入框，且仅发送一次（多字符一次性提交等不可证明情形 fail-closed 不补发，用户重按即可恢复）

#### Scenario: 按住 Shift 连续输入两个全角标点
- **WHEN** 用户在 Safari 按住 Shift 不放连续输入两个全角标点（如 `？？`），且 IME 消费键（`key === 'Process'` 或 `keyCode === 229`，含 `key:'Unidentified'` 变体）的 keyup 缺失或延迟（两次提交之间无任何 keyup）
- **THEN** 两个标点各恰好发送一次——229 不置位 `nonModKeyDown`，两次提交均成为候选；候选 pending 期间收到 229 keydown 时 settle MUST 取消并重排（落在 xterm 该 keydown 注册的 diff timer 之后结算），仲裁观测原生覆盖后只补未覆盖候选（该 keyup 缺失/延迟变体下 xterm 自身 gate 对提交均丢弃或经 deferred diff 发出，合计均不产生重复）

#### Scenario: 输入汉字后立即输入全角标点
- **WHEN** 用户通过拼音 composition 输入汉字（如"你好"）提交后，立即按住 Shift 输入全角标点 `？`，且候选资格与覆盖历史均可证明
- **THEN** 无论 `？` 是否抢在 xterm 延迟 finalize 之前进入 textarea，汉字与标点合计各恰好发送一次（finalize 已含标点时仲裁观测到原生覆盖并抑制补发）

#### Scenario: 中文汉字输入不双发
- **WHEN** 用户通过拼音输入法 composition 输入汉字（如"你好"）
- **THEN** 汉字经 xterm CompositionHelper 路径正常提交，补偿逻辑不补发

#### Scenario: 迟到的 composition 提交不双发（100ms 观测窗口内）
- **WHEN** IME 变体的 commit input 事件晚于 compositionend 到达，xterm 延迟 finalize 已原生发出相同内容，且该原生发射位于候选的 100ms 回看窗口内
- **THEN** 仲裁在结算时观测到原生发射覆盖该候选，抑制补发（无论 compositionend.data 是否可靠）

#### Scenario: 超出观测窗口的迟到提交（显式残余）
- **WHEN** 迟到的重复 commit 事件满足候选资格，但对应原生发射已超出 100ms 回看窗口（或被裁剪/容量溢出导致不可证明）
- **THEN** 超窗有原生记录时按契约可能补发重复内容（显式记录的残余风险 b）；不可证明时 fail-closed 丢弃不补发

#### Scenario: 普通英文按键不双发
- **WHEN** 用户直接敲击普通 ASCII 字符
- **THEN** 字符经 xterm keydown 路径单次发送，补偿逻辑跳过

#### Scenario: iOS 预测文本/听写输入不双发
- **WHEN** 用户在 iOS 通过预测条或听写提交文本（无物理按键事件）
- **THEN** 文本经 xterm 原生 input 路径单次发送，补偿逻辑跳过

#### Scenario: 未知时序 fail-closed
- **WHEN** IME 事件时序无法匹配候选资格条件
- **THEN** 补偿逻辑不补发（用户可重按），补偿器自身不产生双发

### Requirement: 适配范围限定
本期移动适配 SHALL 仅覆盖任务工作台页（TaskWorkbenchPage）及其终端面板；配置管理页、任务列表等管理页保持桌面端体验；工作台"设置"Tab 内的环境变量表单仅做最小可用（不溢出），不做精细移动适配。

#### Scenario: 管理页不承诺移动体验
- **WHEN** 用户在手机浏览器打开配置管理页
- **THEN** 页面保持桌面布局（可缩放查看），不作为本期验收范围
