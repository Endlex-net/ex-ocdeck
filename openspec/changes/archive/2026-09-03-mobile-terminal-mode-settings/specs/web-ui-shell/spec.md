# Delta: web-ui-shell（版本未验证提示可关闭）

## ADDED Requirements

### Requirement: 版本未验证提示可关闭

全局「opencode 版本未验证」banner（`versionVerified === false` 触发）SHALL 提供「不再提示」关闭按钮。用户点击后系统 MUST 将该选择持久化于浏览器 localStorage（key：`ocdeck.versionNotice.dismissed`，值 `'1'`），此后该版本提示 MUST NOT 再展示——彻底关闭，opencode 版本变化后 MUST NOT 自动重新弹出，直至用户手动清除该存储项。关闭操作 MUST 仅作用于版本未验证提示，MUST NOT 影响 watchdog 降级告警的展示。localStorage 写入失败时 banner 当次仍然关闭（组件内状态），但不保证跨会话记住，且不阻塞其他功能。读取到任意非 `'1'` 值 MUST 视为未关闭；读取抛异常（如 `SecurityError`）MUST 捕获并视为未关闭，MUST NOT 向上抛出导致壳层渲染失败，banner 轮询与 watchdog 降级告警评估 MUST 照常进行。

#### Scenario: 点击不再提示

- **WHEN** 版本未验证 banner 展示中，用户点击「不再提示」
- **THEN** banner 立即消失并写入 localStorage；后续页面刷新、30s 轮询刷新、其他页面均不再展示版本未验证提示

#### Scenario: 彻底关闭不随版本复活

- **WHEN** 用户已关闭版本提示，之后 opencode 升级/降级为另一个未验证版本
- **THEN** 版本未验证提示仍不展示

#### Scenario: watchdog 告警不受影响

- **WHEN** 用户已关闭版本提示，且服务端进入 watchdog 降级状态
- **THEN** watchdog 降级告警照常展示

#### Scenario: 存储读取异常

- **WHEN** localStorage 读取抛异常（如 `SecurityError`）
- **THEN** 视为未关闭：版本未验证提示照常展示，组件不抛错，watchdog 告警评估不受影响

#### Scenario: 存储不可用

- **WHEN** localStorage 写入抛异常（如 SecurityError / quota 超限）
- **THEN** 本次点击仍关闭当前展示的 banner，不报错阻塞；下次会话可能重新展示
