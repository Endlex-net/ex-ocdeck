# Tasks: 移动端模式设置（锁定/手势/键盘避让可配置）

## 1. 偏好存储与判定纯函数

- [x] 1.1 新建 `web/src/terminal/mobile-mode.ts`：`MobileMode`（`auto|on|off`）、`MobileCaps`（`{version:1, lock, gestures, keyboardAvoid}`）、`EffectiveCaps` 类型与 `DEFAULT_CAPS`；纯函数 `resolveMobileCaps(mode, caps, coarse): EffectiveCaps`——off=全 false；auto=`{lock:coarse, gestures:coarse, keyboardAvoid:true}`；on=子开关生效且 `lock` 开时强制 `gestures: true`（design D2）
- [x] 1.2 扩展 `web/src/terminal/preferences.ts`：新增 key `ocdeck.terminal.mobileMode` 与 `ocdeck.terminal.mobileCaps`；`loadMobileMode(): MobileMode` 只读 mode key、`loadMobileCaps(): MobileCaps` 只读 caps key（两个 loader 各自独立解析与容错；「auto/off 不得读 caps key」是调用方约束，不由 loader 保证）；非法值/JSON 损坏/缺字段/version 未知/读取异常回退默认且不改写存储（design D1）
- [x] 1.3 `preferences.ts` 写入路径：`saveMobileMode()`（只写 mode key）、`saveMobileCaps()`（一次性写完整 JSON）；`setItem` 全成功才派发 `TERM_PREFS_CHANGED`，失败抛错且不派发。`clearTermPrefs()` 重构为 best-effort 四 key（fontFamily/fontSize/mobileMode/mobileCaps）逐项 `try removeItem`、收集失败，返回 `{ failedKeys: string[] }`；至少一项删除成功时由该函数恰好派发一次 `TERM_PREFS_CHANGED`，全部失败不派发；`saveTermPrefs` 行为不变（design D1）

## 2. 单测：判定与存储（仅存储层，不含 UI）

- [x] 2.1 新建 `web/src/__tests__/mobile-mode.test.ts`：`resolveMobileCaps` 全组合（3 模式 × coarse × 子开关，含锁定开强制手势开）
- [x] 2.2 loader 独立解析与容错：非法 mode / 损坏 JSON / 缺字段 / version 未知 / `SecurityError` 各回退默认且不改写 localStorage；两个 loader 各自只触碰自己的 key
- [x] 2.3 写失败三互斥组（design D8.3，存储层断言：抛错、事件派发次数、`clearTermPrefs` 返回的 failedKeys 内容）：a. mode/caps `setItem` 失败→抛错且不派发事件；b. 恢复默认四 key 全部删除失败→不派发、failedKeys 含全部四项；c. 恢复默认部分成功→恰好派发一次、failedKeys 仅含失败项

## 3. TermSession 状态边沿迁移

- [x] 3.1 `web/src/terminal/session.ts` 新增 `appliedCaps: EffectiveCaps | null` 与 `applyMobileCaps()`：每次判定重新 `loadMobileMode()`（仅 on 时才调 `loadMobileCaps()`，auto/off 传 `DEFAULT_CAPS` 占位），`resolveMobileCaps` 得 next；构造首调（prev===null）按 next 落地（lock=true 走 orchestrator lock 路径、gestures=true attach）；`sameCaps` 时 no-op 不触碰锁定状态与焦点（design D3）
- [x] 3.2 边沿迁移表实现：lock false→true → `lockOrchestrator.lock()`（门禁先置位再 blur）；true→false → `this.lockController.unlock()`（即 silent unlock 语义，MUST NOT focus；不改 `session-coordination.ts` 接口）；gestures 边沿 attach/detach；keyboardAvoid false→true 走既有 shouldListen 判定；true→false → detach listener + 清 `wrap.style.maxHeight` + refit（design D3）
- [x] 3.3 触发点收口：构造期替换原 `pointerCoarse` 直判（现 `session.ts:167-172`）；auth_ok 入参由 `this.pointerCoarse` 改为 `appliedCaps.lock`（唯一例外强制回锁）；pointer 变化更新 `pointerCoarse` 后调 `applyMobileCaps()`（**替换** `lockOrchestrator.onPointerChange` 委托，不得并存两条迁移路径；auto 模式逐点等价）；`applyPreferences` 追加 `applyMobileCaps()` 调用（design D3）

## 4. 键盘避让阈值启发式

- [x] 4.1 `session.ts` `fitForViewport` 改为阈值算法：`KEYBOARD_SHRINK_THRESHOLD = 100`；每次先清 inline `maxHeight` 测自然布局 rect，`shrink = rect.bottom - (vv.offsetTop + vv.height)`；`shrink >= 100` 才写回目标 maxHeight，否则 `''`；仅目标值变化时 `fitNow()`（design D4）
- [x] 4.2 `updateVisualViewportListener` 的 `shouldListen` 追加 `appliedCaps?.keyboardAvoid` 条件（避让关闭时不注册监听）

## 5. 单测：TermSession 与避让

- [x] 5.1 `session-adapter.test.ts` 扩展与改写：构造四初态（auto+coarse 全落地 / auto+fine no-op / on+lock off+gestures on 只挂手势 / off 全 no-op）；lock true→true 不重锁不 blur；修改非锁定子项不重锁；true→false 调 `lockController.unlock()` 不 focus；auth_ok 在 lock 启用时无条件回锁；**auto/off 下构造与偏好重评估对 `loadMobileCaps` 零调用**（判别式加载的运行侧证据）；**改写**现有断言 pointer change 委托 `lockOrchestrator.onPointerChange` 的用例（`session-adapter.test.ts:440` 一带），改为断言 `applyMobileCaps` 边沿结果，旧委托断言必须移除；其余既有用例不退化（design D8.2）
- [x] 5.2 避让阈值用例：shrink 99px 不收 / 100px 收 / 已收缩后重复 resize 不翻转（自然基线）/ offsetTop 变化 / 键盘收起恢复 / 避让关闭时无监听无 maxHeight（design D8.4）
- [x] 5.3 跨标签页：storage 事件后终端按新偏好迁移（design D8.6）

## 6. 设置页 UI

- [x] 6.1 `web/src/pages/SettingsPage.tsx` AppearancePanel 加「移动端模式」分段控件（复用主题 `.seg` 模式）：自动/开启/关闭，点击即 `saveMobileMode()`；子开关三个 checkbox 仅在 `on` 时渲染（auto/off 不渲染、不预读 caps key）；锁定开时手势 checkbox `disabled`；勾选锁定且手势为关时同事务置 `gestures: true`；hint 文案按 design D5；写失败显示错误行。**AppearancePanel 须监听 `TERM_PREFS_CHANGED` 并判别式重载 mode/caps**（恢复默认等外部变更后按实际存储收敛，如恢复前为 on 时子开关随之消失）（design D5）
- [x] 6.2 `web/src/components/TermAppearanceEditor.tsx`「恢复默认」接 `clearTermPrefs()` 新返回契约：消费 `{ failedKeys }` 并按实际存储重载 UI；`failedKeys` 非空显示「部分偏好未清除」并允许重试；**移除调用方自己的 `TERM_PREFS_CHANGED` 派发**（现 `TermAppearanceEditor.tsx:72`，事件由 `clearTermPrefs` 内部按成功情况派发）
- [x] 6.3 `web/src/terminal/TerminalView.tsx` 锁定按钮可见性由 coarse media query（现 `TerminalView.tsx:40`）改为本地 effective `caps.lock` state：由 `resolveMobileCaps` 计算，随 mode 变化、pointer media query、`TERM_PREFS_CHANGED` 与同源 storage 事件刷新；auto/off 路径 MUST NOT 调用 `loadMobileCaps`（判别式读取的 UI 侧遵守）

## 7. 版本未验证 banner 可关闭

- [x] 7.1 `web/src/components/ServerStatusBanner.tsx`：`loadVersionNoticeDismissed()` 读 `ocdeck.versionNotice.dismissed`，`'1'` 视为关闭，读取异常捕获视为未关闭不上抛；惰性 state 初始化；dismissed 时不跳过 `usePoll`（watchdog 独立）；`unverified` alert 内加「不再提示」按钮（`.od-alert-actions` 槽位），点击写 key + 立即隐藏；写失败当次隐藏不持久（design D7）

## 8. 组件级接线测试与收尾

- [x] 8.1 组件测试（UI 接线，含全部「UI 保持旧值」断言）：auto/off 不渲染子开关且不调用 `loadMobileCaps`；lock 开时手势 checkbox 显示开且 disabled；mode/caps 写失败保持旧 UI；恢复默认后 AppearancePanel 按实际存储重载收敛、部分失败提示；`TerminalView` 锁定按钮按 effective `caps.lock` 随 mode/pointer/storage event 更新可见性（design D8.7；本项只负责测试，实现见 6.1/6.3）
- [x] 8.2 banner 测试：未关闭展示 / 点击后写入+隐藏 / 已关闭不渲染 / `getItem` 抛异常视为未关闭且组件不抛错 / watchdog 分支不受影响 / 写失败当次隐藏（design D8.5）
- [x] 8.3 全部新增行为测试做 mutation 式自检（在旧实现下失败）；`openspec validate mobile-terminal-mode-settings --strict` 通过；`web` 下 vitest 全量与 `tsc` 类型检查通过
