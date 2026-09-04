# Delta: mobile-terminal-adaptation（移动端模式设置）

## ADDED Requirements

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

## MODIFIED Requirements

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
