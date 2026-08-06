# terminal-streaming spec delta

## ADDED Requirements

### Requirement: 触屏设备终端输入锁定
在 `pointer: coarse` 设备上，终端流 SHALL 支持输入锁定状态：锁定时终端继续接收并渲染全部 PTY 输出，但浏览器侧除合成手势产生的滚动控制字节外不产生任何 stdin 数据（textarea 不聚焦、键盘/IME/粘贴零发送；鼠标/触摸控制序列等非 UTF-8 字节亦经统一门禁拦截）。每次 WS 连接建立（含一切重连、Tab 切换）后 MUST 回到锁定状态。锁定/解锁 MUST NOT 触发 WebSocket 重连或 PTY 重建。

#### Scenario: 锁定期间输出不中断
- **WHEN** 触屏设备终端处于锁定状态且任务持续产生输出
- **THEN** 终端正常渲染实时输出，WS/PTY 链路无任何重建

#### Scenario: 锁定期间零意外输入
- **WHEN** 触屏设备终端处于锁定状态，用户触摸终端区域、敲击外接键盘、发生 IME composition 尾事件或产生鼠标/触摸控制序列
- **THEN** 不产生任何意外 stdin 字节（含非 UTF-8 控制序列）发送到 PTY

#### Scenario: 断线重连后回到锁定
- **WHEN** 触屏设备终端 WS 断开并自动重连成功（含 Tab 切换引起的重连）
- **THEN** 终端回到锁定状态（防误触优先），输出渲染恢复
