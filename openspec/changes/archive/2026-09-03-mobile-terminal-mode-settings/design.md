# Design: 移动端模式设置（锁定/手势/键盘避让可配置）

## Context

现状（已核实）：

- 终端锁定与触控手势硬绑 `pointer: coarse`：构造期 `session.ts:167-172`（coarse → `lockController.lock()` + `attachGestures()`）、auth_ok 回锁 `session.ts:222`（传 `this.pointerCoarse`）、pointer 动态重评估 `session.ts:554-557`、锁定按钮可见性 `TerminalView.tsx:40`（`useMediaQuery('(pointer: coarse)')`）。
- 键盘避让与 pointer **无关**：`session.ts:490-508` 在 UNLOCKED 且 textarea 聚焦时无条件监听 visualViewport（含桌面 fine pointer），`fitForViewport`（`session.ts:535-542`）无条件收缩。canonical spec（mobile-terminal-adaptation「虚拟键盘视口适配」）同样无 coarse 限定。因此「自动模式」下避让的**启用范围** MUST 保持全平台现状（不随 fine pointer 关闭）；本期同时把避让**算法**升级为 D4 阈值启发式（显式行为变更，proposal 已列），对所有模式统一生效。
- 锁定状态机与顺序契约由 `session-coordination.ts:60-92` 承载（lock-before-blur、unlock=unlockSilently+focus、onAuthOk、onPointerChange）；本变更不改该模块语义，只在其调用方收口启用判定。注意 `lock()` 每次调用都会 blur——重复调用有焦点副作用，启用判定必须按边沿迁移（见 D3）。
- 本机偏好已有模式：`preferences.ts` localStorage `ocdeck.terminal.*` + `TERM_PREFS_CHANGED` CustomEvent + 同源 storage 事件即时生效（`TerminalView.tsx:72-82`）；`saveTermPrefs` 只触碰 fontFamily/fontSize 两个 key（`preferences.ts:61-83`）。
- 版本未验证 banner `ServerStatusBanner.tsx:43-50` 无条件渲染、30s 轮询（:17-24）、无关闭手段；`.od-alert-actions` 按钮槽位样式已存在（`design-system.css:363`）。

约束：iPadOS 无法可靠检测外接键盘，「自动」不做键盘识别；不改 xterm 包；桌面既有行为零回归（未配置 = 自动 + 出厂默认，含避让全平台现状）；锁定会话状态仍不持久化。

## Goals / Non-Goals

**Goals:**

1. 本机「移动端模式」三态偏好（auto/on/off）+ 开启模式下三个布尔子开关（锁定/手势/避让，默认开），即时生效于全部已打开终端与同源标签页。
2. 锁定开时手势强制开（避免只能看不能滚）；自动模式不展示、不读取子开关。
3. 键盘避让「开」改为启发式：仅视口明显压缩才收缩，工具栏抖动不误伤；自动模式下避让启用范围与现状一致（全平台），算法统一为启发式。
4. 版本未验证 banner 支持「不再提示」彻底关闭。
5. 启用判定、状态边沿、存储容错、阈值行为全部单测覆盖。

**Non-Goals:**

- IME 补偿（已全平台，不进开关）；工作台窄屏布局（按宽度，正交）。
- 服务端持久化 / 跟账号同步（策略是设备相关的，存 localStorage 是正确性要求而非省事）。
- 检测外接键盘；会话锁定状态持久化；「避让」独立三态（开=启发式，见 D4）。
- 终端锁定 overlay/手势层/门禁/watchdog 告警本身的实现变更。
- 版本提示关闭后的设置页恢复入口（恢复方式 = 用户清 localStorage）。

## Decisions

### D1: 偏好模型与存储（`preferences.ts` 扩展）

**两个原子 key**（localStorage 无多 key 事务，「开锁定同时强制手势开」必须一次写入）：

```ts
export type MobileMode = 'auto' | 'on' | 'off';
// key: ocdeck.terminal.mobileMode，值 'auto'|'on'|'off'，缺省 auto
export interface MobileCaps { version: 1; lock: boolean; gestures: boolean; keyboardAvoid: boolean; }
// key: ocdeck.terminal.mobileCaps，值 JSON.stringify(MobileCaps)，缺省 {version:1,lock:true,gestures:true,keyboardAvoid:true}
```

- **判别式读取**（满足 spec「自动模式 MUST NOT 读取子开关」）：`loadMobileMode(): MobileMode` 只读 mode key；`loadMobileCaps(): MobileCaps` 只在 mode 为 `on` 时被调用方调用。运行侧 `resolveMobileCaps` 与设置页都遵守：auto/off 路径 MUST NOT 对 `mobileCaps` key 调 `getItem`。
- 容错（沿用 `loadTermPrefs` 契约）：非法 mode 值 → `auto`；caps JSON 解析失败 / 缺字段 / 字段类型错误 / `version` 未知 → 整项回默认；读取抛 `SecurityError` → 按默认值返回；**任何读取失败 MUST NOT 改写 localStorage**。
- **写入**：「无半更新」限定于单 key 原子写——模式切换只写 mode key；子开关变更一次性写完整 caps JSON（含「开锁定 → 手势强制开」的同事务提交）。`setItem` 成功后才更新 UI state 并派发 `TERM_PREFS_CHANGED`；任一写失败 MUST NOT 派发事件、已打开终端保持现状、UI 显示错误。
- `clearTermPrefs()` 扩展为同时清除 `mobileMode` 与 `mobileCaps` 两个 key（spec「恢复默认」）。恢复默认是多 key 删除，**无法原子化**，采用 best-effort：逐个尝试清除全部目标 key 并收集失败；只要有任一删除成功就派发变更事件、UI 从实际存储重载收敛；存在失败时显示「部分偏好未清除」并允许重试。`saveTermPrefs` 保持不变。
- 运行时每次需要判定时重新读取（localStorage 同步读取廉价，且保证跨标签页 storage 事件后的新鲜度），TermSession **不缓存** MobilePrefs 字段。

### D2: 启用判定唯一入口

纯函数（`terminal/mobile-mode.ts` 新文件，不 import xterm/DOM，对齐 `session-coordination.ts` 测试缝风格）：

```ts
export interface EffectiveCaps { lock: boolean; gestures: boolean; keyboardAvoid: boolean; }

export function resolveMobileCaps(mode: MobileMode, caps: MobileCaps, coarse: boolean): EffectiveCaps {
  if (mode === 'off') return { lock: false, gestures: false, keyboardAvoid: false };
  if (mode === 'auto') {
    // 锁定/手势沿用 coarse 自适配；避让启用范围保持全平台既有现状（与 pointer 无关，算法统一为 D4 启发式）
    return { lock: coarse, gestures: coarse, keyboardAvoid: true };
  }
  // on：子开关生效；锁定开 → 手势强制开
  return { lock: caps.lock, gestures: caps.lock || caps.gestures, keyboardAvoid: caps.keyboardAvoid };
}
```

调用方约定：`resolveMobileCaps(mode, …)` 的 `caps` 参数仅在 `mode === 'on'` 时来自 `loadMobileCaps()`；auto/off 传默认 caps 占位（函数不读它），调用方负责不发起读取。

### D3: TermSession 状态边沿迁移（不动 lock/手势/门禁内部）

TermSession 新增 `appliedCaps: EffectiveCaps | null`（初始 null），唯一迁移入口：

```ts
private applyMobileCaps(): void {
  const mode = loadMobileMode();
  const caps = mode === 'on' ? loadMobileCaps() : DEFAULT_CAPS;
  const next = resolveMobileCaps(mode, caps, this.pointerCoarse);
  const prev = this.appliedCaps;
  this.appliedCaps = next;
  if (prev === null) {
    // 构造首调：按 next 落地初始状态（等价现状构造 session.ts:167-172）
    if (next.lock) lockOrchestrator.lock();        // 门禁先置位再 blur
    if (next.gestures) attachGestures();
    updateVisualViewportListener();                // shouldListen 追加 next.keyboardAvoid
    return;
  }
  if (sameCaps(prev, next)) return;                // 无变化：MUST NOT 触碰锁定状态与焦点
  // —— 边沿迁移表 ——
  // lock false→true：lockOrchestrator.lock()（门禁先置位再 blur，顺序契约不变）
  // lock true→false：unlockSilently（MUST NOT focus，同 pointer 转 fine 语义）
  // lock true→true / false→false：MUST NOT 触碰锁定状态与焦点（保护用户手动解锁）
  // gestures false→true：attachGestures()；true→false：detachGestures()；同值不动
  // keyboardAvoid false→true：updateVisualViewportListener() 走既有 shouldListen 判定
  // keyboardAvoid true→false：detachVisualViewportListener() + 清 wrap maxHeight + refit
}
```

各触发点收口：

- **构造**（替换 `session.ts:167-172`）：`applyMobileCaps()`。
- **auth_ok**（`session.ts:222`）：唯一例外——只要当前 `appliedCaps.lock === true` 就强制回锁（沿用 `onAuthOk` 语义，入参由 `this.pointerCoarse` 改为该布尔），不受边沿保护。
- **pointer 变化**（`session.ts:554-557`）：更新 `pointerCoarse` 后调 `applyMobileCaps()`；on/off 模式下 caps 与 coarse 无关，天然幂等不再触发迁移（等效于不再委托 `lockOrchestrator.onPointerChange`）；auto 模式迁移结果与现状逐点等价。
- **偏好变更**：`applyPreferences` 追加 `applyMobileCaps()`（TERM_PREFS_CHANGED 监听已存在）。修改手势/避让子项或无关偏好时 lock 边沿不变 → MUST NOT 重锁、MUST NOT 动焦点。
- **锁定按钮可见性**（`TerminalView.tsx:40`）：由 coarse media query 改为「caps.lock 启用」——本地 state，由 prefs 变更事件 + pointer media query 共同刷新，复用 `resolveMobileCaps` 同一事实来源。

### D4: 键盘避让启发式（开=视口明显压缩才收）

阈值判定在 `fitForViewport`（`session.ts:535-542`），**基线必须取自然布局，避免被自身写入的 maxHeight 污染**：

```ts
// 每次 visualViewport 事件：
const prevMax = wrap.style.maxHeight;
wrap.style.maxHeight = '';                        // 先恢复自然布局再测量
const shrink = wrap.getBoundingClientRect().bottom - (vv.offsetTop + vv.height);
const target = shrink >= KEYBOARD_SHRINK_THRESHOLD ? `${Math.max(0, vv.offsetTop + vv.height - wrap.getBoundingClientRect().top)}px` : '';
wrap.style.maxHeight = target;
if (target !== prevMax) fitNow();                 // 仅目标值变化时 refit
```

- `KEYBOARD_SHRINK_THRESHOLD = 100`（CSS px）：iOS/iPadOS 虚拟键盘 250px+，Safari 工具栏/地址栏伸缩通常 <100px。写死常量，不做设置项。
- 未达阈值时 `target = ''`（全高），与「工具栏抖动不误伤」「键盘收起恢复」共用同一路径，无独立分支。
- 避让关闭时 `updateVisualViewportListener` 的 shouldListen 追加 `caps.keyboardAvoid` 条件为 false（不注册监听；若曾在听，边沿 true→false 时 detach + 清 maxHeight + refit，见 D3）。

### D5: 设置页 UI（`SettingsPage.tsx` AppearancePanel）

- 「移动端模式」分段控件复用主题 `.seg` 模式（`SettingsPage.tsx:144-166`），选项 自动/开启/关闭，点击即写 mode key + 派发 `TERM_PREFS_CHANGED`。
- 子开关三个 checkbox 仅在 `mobileMode === 'on'` 渲染（auto/off 不渲染、不预读 caps key）。勾选「终端锁定」时同事务置 `gestures: true`（同一次 caps JSON 写入）；锁定为开时手势 checkbox `disabled` + hint。UI 层保证「锁定开 → 手势开」不可违。
- hint：自动=「触屏设备自动启用锁定/手势，键盘避让保持默认」；关闭=「完全按桌面终端处理（含不收缩避让键盘）」；锁定子开关 hint 含「接外接键盘时关闭可避免锁定遮挡输入」。
- 写失败（localStorage 异常）显示错误行且不派发事件（沿用 TermAppearanceEditor 错误展示模式）。

### D6: 兼容与迁移

- 存量用户无两个 key → 全默认 → auto + 出厂默认：能力启用矩阵与现状等价；唯一用户可见差异为键盘避让统一改用 D4 阈值启发式（显式行为变更，proposal 已列）。
- 无服务端迁移、无数据修复；回滚即删除相关代码，存量 key 残留无副作用。

### D7: 版本未验证 banner 可关闭（`ServerStatusBanner.tsx`）

- 方案：纯前端，不碰后端契约。`loadVersionNoticeDismissed()` 读 `localStorage['ocdeck.versionNotice.dismissed']`，值 `'1'` 视为已关闭；**读取抛异常（如 `SecurityError`）MUST 捕获并视为未关闭，MUST NOT 向上抛出**（组件在壳层，异常会拖垮整页与 watchdog 告警）。组件用惰性 state 初始化读取一次；dismissed 为 true 时 MUST NOT 提前跳过 `usePoll`（watchdog 告警独立评估）。`unverified` alert 内加「不再提示」按钮（复用 `.od-alert-actions` 槽位样式），点击写 key + 本地 state 立即隐藏。watchdog 降级分支（:34-42）不受影响。
- 彻底关闭（用户已确认）：不记录版本号、版本变化不复活；读取到任意非 `'1'` 值视为未关闭。
- 写入失败（隐私模式/quota）：当次隐藏生效、不持久，吞异常不阻塞。
- 不进设置页（用户已确认「只要 banner 按钮」），与移动端模式设置无耦合。

### D8: Test Strategy

单测（vitest，复用 `__tests__` 现有 mock 体系）：

1. `mobile-mode.test.ts`：`resolveMobileCaps` 全组合（3 模式 × coarse × 3 子开关，含锁定开强制手势开）；判别式加载——auto/off 路径断言未对 caps key 发起 `getItem`；非法值/JSON 损坏/缺字段/version 未知/SecurityError 各回退默认且不改写存储。
2. `session-adapter.test.ts` 扩展：四个触发入口（构造/auth_ok/pointer 变化/prefs 变更）× 边沿迁移表——重点：构造四初态（auto+coarse 全落地、auto+fine 全 no-op、on+lock off+gestures on 只挂手势、off 全 no-op）；lock true→true 不重锁不 blur、修改非锁定子项不重锁、true→false silent unlock 不 focus、auth_ok 在 lock 启用时无条件回锁、auto 模式与现状等价（既有用例不退化）。
3. 写失败副作用（三组，互斥无歧义）：
   a. mode `setItem` 失败或 caps 原子写失败（含锁定+手势同事务）：不派发 `TERM_PREFS_CHANGED`、UI 保持旧值。
   b. 恢复默认全部删除调用失败（成功数为零）：不派发、UI 保持旧值。
   c. 恢复默认至少一个删除成功且至少一个失败：派发一次事件、UI 从实际存储重载收敛、显示「部分偏好未清除」。
4. 避让阈值：shrink 99px 不收 / 100px 收 / 已收缩后重复 resize 不翻转（自然基线）/ offsetTop 变化 / 键盘收起恢复 / 避让关闭时无监听无 maxHeight。
5. `ServerStatusBanner`：未关闭展示、点击后写入+隐藏、已关闭不渲染、`getItem` 抛异常时视为未关闭且组件不抛错、watchdog 分支不受影响、localStorage 写失败当次隐藏不抛错。
6. 跨标签页：storage 事件后终端按新偏好迁移（复用 TERM_PREFS_CHANGED 通道断言）。
7. 组件级接线（防 UI 接线回归）：auto/off 不渲染子开关且不调用 `loadMobileCaps`；lock 开时手势 checkbox 显示开且 disabled；切换 mode/caps 写失败保持旧 UI；`TerminalView` 锁定按钮按 effective `caps.lock` 随 mode、pointer、storage event 更新可见性。

每个新增行为测试须验证在旧实现下失败（mutation 式自检），防止摆设断言。

## Risks / Trade-offs

- [on 模式下桌面也被默认锁定（每次重连回锁），用户可能困惑] → 这是「开启=强制当移动端」的显式语义；hint 与 spec scenario 均已写明，按钮始终可解锁。
- [阈值启发式假阴性：分屏等场景视口压缩超 100px 但无虚拟键盘] → 收缩无害（仅高度+refit）；反向（浮动键盘 <100px）不收属可接受降级。
- [auto 模式忽略 caps 存储值，用户从「开启」切回「自动」后自定义"看似丢失"] → spec 显式语义（自动永远出厂默认），存储值保留、切回开启即恢复；判别式加载使该行为可测。
- [版本提示彻底关闭后用户找不到恢复入口] → 用户已确认取舍；恢复 = 清 localStorage key。

## Open Questions

- 无。阈值 100px 为经验值，实机若误触发可上调，属常量调整不改结构。
