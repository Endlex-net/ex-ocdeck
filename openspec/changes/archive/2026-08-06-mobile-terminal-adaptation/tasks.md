# Tasks: 移动端/iPad 终端工作台适配

实现顺序按 L0→L3 分层，但注意依赖：L3 的 visualViewport 依赖 L2 锁态，IME 补偿依赖 L2 统一输入门禁。桌面端（fine pointer + ≥1025px）行为全程零回归（IME 补偿修复桌面 Safari 除外，已获批准）。

## 0. Spike：opencode TUI 鼠标行为验证（先行，阴性则阻断）

- [x] 0.1 在**真实链路**（浏览器 xterm → WS → tmux → `opencode attach`）验证：TUI 是否开启 mouse reporting（xterm 运行时 `term.modes.mouseTrackingMode` 是否为非 none）、滚轮事件是否滚动推理历史、点击审批选项是否响应鼠标
- [x] 0.2 判定分流：支持 → 记录结论到 design.md「Open Questions」并继续；**不支持 → 停止实现，阻断本 change 并升级给用户决策（用户已要求触摸点击必须支持，不得静默降级为键盘验收）**

## 1. L0 视口与安全区基础

- [x] 1.1 `web/index.html` viewport meta 改为 `width=device-width, initial-scale=1.0, viewport-fit=cover`
- [x] 1.2 `styles.css` 高度链（`html/body/#root`、`.app-shell`）加 `100dvh`（`100vh` fallback 在前），外壳加 `env(safe-area-inset-*)` padding
- [x] 1.3 小屏断点（`max-width:1024px`）下松绑 `.input min-width:180px`、`.modal-wide width:720px`、`.git-side width:340px`——响应式覆盖 MUST 限定 `.workbench` 作用域（`.workbench .input` / `.workbench .modal-wide` / `.workbench .git-side`），不影响管理页（AIConfig/Projects/TokenGate 等）
- [x] 1.4 现有 `web/src/hooks.ts` 增加 `useMediaQuery(query)` hook（不新建 hooks/ 目录）
- [x] 1.5 `web/package.json`：`@xterm/xterm` 从 `^5.5.0` 钉为 `5.5.0` 精确版本；新增 devDependency vitest 与 `test` script（仅此 devDep，无运行时新依赖）

## 2. L1 工作台响应式布局

- [x] 2.1 `styles.css` + TaskWorkbenchPage：小屏断点下 tabstrip 横向滚动；header 收窄——保留返回+标题（截断）、主操作图标化、次级操作收「⋯」溢出菜单（design D3）；Git 面板纵向堆叠
- [x] 2.2 TaskWorkbenchPage 用 `useMediaQuery('(max-width: 1024px)')` 应用窄屏 class；pane 复用现有堆叠 + `.pane-hidden` 切换机制（不重写 pane 内部）
- [x] 2.3 桌面端回归：≥1025px fine pointer 下布局/交互与适配前一致（含 iPad Pro 横屏桌面布局）

## 3. L2 锁定模式、统一输入门禁与触控手势

- [x] 3.1 新增 `web/src/terminal/lock.ts`：`createLockController(host)` 按 design D8 接口实现（暴露 `overlay`、`onChange` 订阅、`lock()/unlock()/isLocked()`、`dispose()`），纯状态 + overlay DOM，无 React 依赖；锁定状态不持久化
- [x] 3.2 session.ts 统一输入门禁（design D5）：`sendInput(d, binary)` 同时接 `onData` 与 `onBinary`——onBinary 按 charCode 取原始字节（MUST NOT 用 TextEncoder）；门禁判定 `authed + WS_OPEN + !(locked && !syntheticEventInFlight)`；WS 连接建立/auth_ok（含重连）→ coarse 设备强制 LOCKED；`lock()` 先置门禁标志再 `term.blur()`
- [x] 3.3 新增 `web/src/terminal/touch-gestures.ts`：按 design D4/D8 实现——每次手势按 `term.modes.mouseTrackingMode` + `term.buffer.active.type` 运行时路由（路由表 D4.1）；**capture 阶段在 `.xterm` 上注册 touch 监听，判定接管时 `stopImmediatePropagation()` 阻断 xterm 原生 touch 监听，放行路径不阻断**（design D4 监听阶段契约）；合成事件契约（D4.2）：WheelEvent 带 clientX/Y、逐帧 deltaY、deltaMode:0、bubbles/cancelable；MouseEvent 带坐标/button/buttons，mouseup 必须 bubbles:true；自定义 tap 对原 touch 序列 preventDefault 抑制 compatibility mouse；changedTouches+identifier 单指追踪；touchcancel 复位；`getTarget()` 按锁状态返回 overlay 或 xterm 元素，锁状态变化时 rebind；合成事件经 `markSynthetic` 同步包裹（嵌套计数 + try/finally）
- [x] 3.4 session.ts 接线：coarse 判定；pointer 类型动态变化重评估（转 fine 自动解锁隐藏 UI、转 coarse 强制锁定）；对外暴露 `lock()/unlock()/isLocked/onLockChange(cb)`；**`unlock()` 必须在按钮 click 同步调用栈内完成 overlay 移除与 `term.focus()`**（iOS 只允许可信手势栈内唤起虚拟键盘）；dispose 全清理
- [x] 3.5 TerminalView 加锁定/解锁浮动按钮（仅 coarse 显示），仅从 TermSession 接口取状态投影 + 回调，不持有锁状态；z-index 层级（自底向上，与 design D5 一致）：终端画面 < 锁定 overlay < 浮动按钮 < 连接状态 overlay（连接控件必须最顶层）

## 4. L3 虚拟键盘适配与 IME 补偿

- [x] 4.1 session.ts：UNLOCKED 且聚焦时监听 `visualViewport` 的 `resize` 与 `scroll`（rAF 去抖），按 `max(0, vv.offsetTop + vv.height - wrap.getBoundingClientRect().top)` 设置 **`.terminal-wrap`** maxHeight（wrap 承载终端画面/锁定 overlay/浮动按钮/连接 overlay，整体收缩保证按钮与连接控件随可视区上移）→ fitNow → WS resize；blur/锁定/卸载移除内联样式并 refit；API 缺失跳过
- [x] 4.2 新增 `web/src/terminal/ime-compensator.ts`：候选输入仲裁纯逻辑模块（design D7/D8 接口：时钟/调度器/emit 注入——**emit MUST 同步调用 term.input，生产 scheduler = 同 Window `setTimeout(fn, delayMs ?? 0)`**，`schedule(fn, delayMs?)` 返回可取消 handle，`dispose()` 取消 pending settle 并清空状态）——镜像 `anyKeyDown`/`nonModKeyDown`（**不置位集合**：修饰键 `{Shift, Control, Alt, Meta, CapsLock}`（按 key）+ **IME 处理键双条件判定 `key === 'Process'` 或 `keyCode === 229`**——含 `key:'Unidentified'` 变体；229 不置位因 Safari 对 IME 消费键的 keyup 可能缺失/延迟会卡死镜像；任意 keyup 清 false）/`compositionActive`；**候选 pending 期间收到 229 keydown 时 MUST 取消并重新调度 settle**（落在 xterm 该 keydown 注册的 diff timer 之后结算，闭合 input-before-229 批量竞争窗口）；候选资格判决含 `ev.isTrusted` 与 `ev.composed === true` 双闸 + **单 Unicode code point 限制 `[...ev.data].length === 1`**（多 code point fail-closed 不进队列）；单调毫秒时钟（生产 `performance.now()`），`lastCapacityEvictionAt` 初值 `undefined`；候选挂起一个宏任务后 settle（**待结算批次在完成全部匹配前仍视为 pending**，裁剪以完整 pending 集计算保护窗口），观测原生发射历史 `recentNative`（记录 `{text, at, remainingText}`，**occurrence 级消耗**——匹配时删除一次文本出现，多条可匹配取**时间最早者（时间戳相同取 ring 插入序）**，耗尽记录**立即移出 ring**，聚合原生发射 `onData("？？")` 可依次覆盖多个候选；**惰性裁剪 MUST NOT 丢弃落在 pending 候选回看窗口内的记录**；硬容量 32 条溢出丢最旧——溢出落在 pending 窗口内时该候选标记不可证明，且溢出 MUST 记 `lastCapacityEvictionAt = now()` **历史缺口水位**，新候选回看窗口与缺口重叠时立即标记不可证明 → fail-closed 不补发；补偿器自身 emit 经嵌套计数标记不记录），仅对未覆盖候选 `term.input(data, true)` 补发；fail-closed；不修改 textarea.value
- [x] 4.3 vitest 单测（ime-compensator，fake timers）：合成事件序列覆盖 design D7 判决表全部 12 条路径，**逐路径断言原生 + 补偿的总发送次数**（而非只断言补偿器 emit 次数）。必须含：核心竞争序列 `compositionstart → commit → compositionend → Shift keydown → input('？')` 候选在 finalize 前到达——模拟原生随后发出「你好？」，断言仲裁抑制补发、总计各一次；finalize 已发「你好」后 Shift+？——断言补发恰好一次；快速连按两次 Shift+？——断言两次补发不被自身发射记录误杀；**按住 Shift 连打两个标点且两次提交之间无任何 keyup（229 keyup 缺失/延迟变体）——断言两次提交均成为候选、原生+补偿总发送恰好两次（229 触发 settle 重排、diff 先结算，仲裁只补未覆盖候选）**；**`key:'Unidentified', keyCode:229` 变体同样不置位 nonModKeyDown 的回归测试**；**Chrome 229 路径语义更新——keydown 229 先于 input 时该 input 成为候选，settle 观测到 xterm deferred diff 的原生覆盖（FIFO 先结算）而抑制，断言总发送恰好一次**；**忠实模拟 xterm textarea/diff timer 的定时器队列测试（共享 fake timer 队列按注册序 flush），至少覆盖：Chrome `229 → input`、Safari `input → 229`、两次输入在 timer 执行前批量到达（`input1 → 229#1 → input2 → 229#2`，断言 229 重排 settle 后原生+补偿总发送恰好两次）、keyup 正常/缺失两变体**；**聚合原生发射 `onData("？？")` 一条记录依次覆盖两个「？」候选——断言两个候选均被抑制、总发送恰好两次**；迟到重复 commit（原生已发出相同内容）——**100ms 窗口内**断言抑制、**超窗**断言按契约补发（残余风险的行为锁定）；**settle 被主线程暂停推迟 >150ms——回看窗口内记录 MUST NOT 被惰性裁剪，断言仍正确抑制**；**容量 32 溢出落在 pending 窗口内——断言该候选 fail-closed 不补发**；**先溢出后候选——t=0 observeNative 记录被后续 32 条容量溢出删除（pending 为空），t=50 新候选入队且回看窗口与 `lastCapacityEvictionAt` 缺口重叠——断言该候选立即标记不可证明、settle fail-closed 不补发**；**多字符 ev.data（如 `"？？"`）候选资格不成立——fail-closed 不进队列**；多条记录同时可匹配——断言取时间最早记录（同时间戳取插入序）并删除第一次出现、耗尽记录立即移出 ring；资格判决任一条件不确定的事件——fail-closed 丢弃不进队列；合格候选无原生覆盖——补发恰好一次；粘贴路径不经补偿器；**窗口嵌套——较早候选回看窗口覆盖旧/新两条原生记录、较晚候选窗口只覆盖新记录，断言 earliest-first 把新记录留给较晚候选、两候选均被覆盖**
- [x] 4.4 vitest 单测（touch-gestures 判决与事件构造）：运行时路由表全分支、tap/拖拽阈值判定、WheelEvent/MouseEventInit 参数完整性（deltaMode、bubbles、button/buttons）、compatibility mouse 抑制调用；**监听阶段契约——接管路径调用 `stopImmediatePropagation()`、放行路径不调用（capture 注册先于 xterm 目标阶段监听）**
- [x] 4.5 vitest 单测（统一门禁 + 锁状态机）：onData/onBinary 双出口 adapter 级真实调用链（onBinary 经统一门禁 sendInput、按 charCode 原始字节发送含 >127、locked 拒绝）+ 纯函数发送矩阵；onBinary 字节按 charCode 原始值发送（含 >127 字节不被 UTF-8 破坏）；`lock()` 顺序断言（门禁先生效再 blur）；**`term.blur()` 清空 textarea 触发 pending deferred-diff 发送 DEL 时门禁已先锁定（blur-DEL 不泄漏）**；锁状态机 adapter 级——auth_ok（含重连）强制 LOCKED、coarse↔fine 动态变化（转 fine 自动解锁、转 coarse 强制锁定）、手动 lock()/unlock()、onLockChange 订阅投影（adapter 级，lock/unlock/auth_ok/pointer 变化后回调收到正确 locked 值）；TerminalView 按钮可见性/图标文案投影为视图层规则，经人工验收（5.4）覆盖
- [x] 4.6 session.ts 接线 ime-compensator（全平台启用，capture 监听 textarea，onData tap 接 observeNative，dispose 清理）

## 5. 验证

- [x] 5.1 `npm run test`（vitest）通过；`npm run build` / typecheck 通过，无新 lint 错误
- [x] 5.2 桌面端人工回归（Chrome + macOS Safari）：终端输入（含中文 IME 汉字、`？《》` 验证补偿顺带修复）、滚动、Git 面板、TUI 操作不变 —— 用户已实测（含发现并回归验证 Safari 连打标点修复）
- [x] 5.3 响应式验收：375px / 768px / 1024px / 1194px 四档宽度（模拟器）布局无溢出、tab 可切换；**小屏管理页作用域回归——375px 下 AIConfig / Projects / TokenGate 等管理页不受 `.workbench` 作用域样式影响（`.input` 等保持桌面样式）** —— 375/768 由 P2 截图核验 + iPhone 真机覆盖 375 档；1024/1194 档未单独实测（接受风险：布局断点逻辑简单、375/768 已验证）
- [x] 5.4 真机验收（iPad Safari）：默认锁定 → 触控滚动查看推理历史 → 解锁 → 触摸点击审批（一次 tap 恰好一次确认）→ 虚拟键盘重排 → 发送中文指引（含全角标点，含「输入汉字后立即 Shift+标点」序列）→ 切 Tab 来回验证强制锁定 → 外接鼠标/触控板插拔验证自动解锁/锁定（仅外接键盘不触发，保持锁定可手动解锁）；iPhone Safari 同步过一遍主流程 —— iPhone Safari 主流程用户已实测通过；iPad 未单独实测（iPhone 与 iPad 同为 coarse pointer + WebKit，主路径一致）
