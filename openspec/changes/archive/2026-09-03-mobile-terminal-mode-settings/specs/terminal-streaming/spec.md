# Delta: terminal-streaming（移动端模式偏好 + 触屏锁定需求门禁化）

## ADDED Requirements

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

## MODIFIED Requirements

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
