# Design: 移动端/iPad 终端工作台适配

## Context

现状（已核实）：

- web/src 零响应式：无 `@media` / `matchMedia` / touch 监听 / UA 检测（grep 全仓 0 命中）
- `web/index.html:5` viewport 为 `width=device-width, initial-scale=1.0`，无 `viewport-fit=cover`、无 dvh
- 布局为纯桌面 flexbox：git 侧栏固定 340px（styles.css:802）、`.modal-wide` 720px（:1377）、`.input min-width:180px`（:116）；`.workbench` 为 `height:100%; overflow:hidden`（:406-411），页面唯一滚动容器是 `.app-content`（:353-357）
- 工作台 tab 结构：TUI / 多个 shell 终端 / Git / 设置（TaskWorkbenchPage.tsx:316-419），pane absolute 堆叠 + `.pane-hidden` 切换。**审批确认、任务进度、推理历史均在外部 opencode TUI 内部，web 端无原生实现**（用户已确认：全部走 TUI，不新增原生 pane）
- header 含 main 合入的**来源感知返回按钮**（`fromActive` prop：从 #/active 进入时返回「← 活跃会话」，否则「← 任务列表」，TaskWorkbenchPage.tsx:165-177）与 **isDir 条件**（dir 任务隐藏 Git tab 与分支名徽标）——L1 header 收窄 MUST 保留 fromActive 条件分支的两个变体与 isDir 条件渲染
- 后端 resize 链路已通（FitAddon → WS resize → tmux winsize），移动端无需改后端

xterm.js 5.5.0 已核实事实（lib 调研，全部对 5.5.0 源码 spot-check）：

- **IME bug**：`Terminal.ts:1172` `_inputEvent` gate `(!ev.composed || !this._keyDownSeen)` 丢弃 input 事件；`_keyDownSeen` 在**每个 keydown（含纯修饰键）**置 true（:1003）、仅 keyup 清除（:1100）；`ev.composed` 是标准 DOM 标志，所有真实 input 事件均为 true → 按住 Shift 输入 `？《》` 时 gate 必丢。汉字走 CompositionHelper 路径不受影响。上游 #5887 / PR #6054 未合并，升级无效
- **`term.input(data, wasUserInput=true)`**（public/Terminal.ts:141 → CoreService.triggerDataEvent，CoreService.ts:58-78）：与键盘输入同一条 onData 管线，绕过 textarea/gate 全部机制——应用侧注入字符的干净公开 API
- **touch**：`.xterm` 上有原生 `touchstart`/`touchmove`（Terminal.ts:835-846），普通 buffer 拖拽滚动（Viewport.ts:384-400）；**但 `areMouseEventsActive`（TUI 开启鼠标上报）时直接 return，alt buffer 无 scrollback 可滚** → TUI 场景原生触控滚动无效。无 `touchend` 监听
- **wheel**（Terminal.ts:800-833，`.xterm` 上，`passive:false`）：(a) 普通 buffer → 滚 scrollback；(b) alt buffer 无 mouse reporting → 转成方向键序列发给应用；(c) mouse reporting → 转 SGR `\x1b[<64/65;col;rowM`。**合成 WheelEvent 可复用整条路径**
- **鼠标点击**：mouse reporting 开启时 `.xterm` 的 mousedown/up 被转换为 SGR 序列发给应用 → **合成 MouseEvent 可实现触摸点击 TUI**
- `term.blur()` 公开 API 存在（public/Terminal.ts:135）

约束：不修改/升级/patch `@xterm/xterm` 包；不新增运行时依赖（vitest 作为 devDependency 已获用户批准）；桌面端（fine pointer）行为零回归——唯一的例外是 IME 补偿全平台启用会顺带修复桌面 Safari 全角标点 bug（用户已批准）；本期只适配 TaskWorkbenchPage；dispatchEvent 同步执行，可利用同步性做无副作用的事件来源标记。

## Goals / Non-Goals

**Goals:**

1. phone（≤767px）与 iPad（768-1024px）下工作台可用；iPad 横屏（≥1025px）用桌面式布局（布局只看宽度，用户已确认）
2. 触屏设备终端默认锁定、可解锁；任何 WS 重连（含 Tab 切换）后强制回锁定（用户已确认）
3. 锁定时可触控滚动查看（含 TUI 内推理历史）；解锁后支持**触摸点击 TUI（审批确认）**（用户要求必须支持）、虚拟键盘输入
4. 应用侧 IME 补偿：全平台修复 IME 直交字符丢失；候选输入仲裁——有界保证（settle 前已观测且位于 100ms 回看窗口内的原生覆盖 MUST NOT 补发），fail-closed（漏发优先于双发）；超窗残余显式记录（见 D7 契约）
5. 核心模块纯逻辑化，vitest 单测覆盖

**Non-Goals:**

- 配置页、任务列表页等管理页移动适配；工作台内"设置"Tab 里的 EnvEditor 表单仅做最小可用（不溢出），不做精细适配
- 新增 web 原生审批/历史面板
- opencode TUI 自身的鼠标支持改造（不在本仓库；若 spike 证明 TUI 未开 mouse reporting，本 change 阻断并升级给用户，不静默降级）
- xterm 上游 bug 完整修复（rollover #6089、Windows TSF #6049 等）
- pinch-zoom、惯性物理精确复刻

## Decisions

### D1: 断点与触屏检测

- 断点：`≤767px` phone、`768-1024px` iPad、`≥1025px` 桌面布局。结构性切换用 JS `matchMedia`，纯样式收缩用 CSS media query
- 触屏 = `matchMedia('(pointer: coarse)')`，与宽度**正交**：iPad Pro 横屏桌面布局 + 仍默认锁定
- `useMediaQuery` hook 加入**现有** `web/src/hooks.ts`（不新建 hooks/ 目录）

### D2: 视口与安全区（L0）

- viewport 改 `width=device-width, initial-scale=1.0, viewport-fit=cover`
- 高度链 `100vh` fallback + `100dvh`；外壳 `env(safe-area-inset-*)` padding
- 小屏断点下松绑 `.input min-width`、`.modal-wide width`、`.git-side width`——这些 class 是全局的（`.input` 还用于管理页表单），响应式覆盖 MUST 限定 `.workbench` 作用域（`.workbench .input` 等），不影响管理页

### D3: 工作台响应式布局（L1）

- `≤1024px`：单列 + 现有 tabstrip 即天然 Tab 导航（TUI/shell/Git/设置已是 tab 结构），做窄屏化：tabstrip 横向滚动、Git 面板堆叠、pane 复用现有堆叠切换机制
- header 收窄规则（≤1024px）：保留「← 任务列表」返回与任务标题（截断）；主操作（激活/恢复等状态相关主按钮）保留为图标按钮；次级操作（删除、其他）收进「⋯」溢出菜单；badge 计数保留但缩小
- 用户场景映射（全部经 TUI tab）：任务进度=TUI 画面；审批确认=TUI 内触摸点击（D4/D5）；推理历史=TUI 内触控滚动（D4）；发送指引=解锁+虚拟键盘（D6/D7）

### D4: 终端触控手势层（L2，核心）

原则：**不与 xterm 原生 touch 监听打架；手势路由只看每次手势时的公开运行时状态**（`term.modes.mouseTrackingMode` 与 `term.buffer.active.type`），不按 wsPath/终端类型静态判定——TUI 可能动态开关 mouse mode，shell 里也可能跑 vim/htop。

#### D4.1 手势路由表（每次 touchstart 时求值）

| 锁状态 | `buffer.active.type` | `mouseTrackingMode` | 手势处理 |
|---|---|---|---|
| LOCKED | 任意 | 任意 | overlay 完全遮挡终端 → xterm 原生监听收不到事件；overlay 上垂直拖 → **合成 WheelEvent**；tap → 无操作 |
| UNLOCKED | normal | none | 不接管，xterm 原生 touch 拖拽滚动 + 兼容鼠标事件聚焦 |
| UNLOCKED | normal | active（如 shell 里跑 htop） | 手势层接管：垂直拖 → 合成 WheelEvent；tap → 合成 mousedown+mouseup |
| UNLOCKED | alternate | 任意 | 原生无 scrollback 可滚 → 手势层接管：垂直拖 → 合成 WheelEvent（xterm 自动转方向键或 SGR 64/65）；mouse active 时 tap → 合成点击 |

- 滚动统一走合成 WheelEvent 复用 xterm wheel 三路径（scrollback / alt buffer 转方向键 / mouse reporting 转 SGR 64/65），应用层不直接碰 scrollTop
- **监听阶段契约**：xterm 在 `term.open()` 时已向 `.xterm` 注册 touch 监听（非 capture：事件目标为 `.xterm` 自身时走 target 阶段、目标为其子孙元素时走 bubble 阶段——二者均晚于 capture 阶段）。手势层 MUST 以 **capture 阶段**在 `.xterm` 上注册 touch 监听（捕获下降先于 target/bubble 阶段），判定「接管」（D4.1 路由表）时调用 `stopImmediatePropagation()` 阻断 xterm 原生 touch 监听；判定「放行」（normal buffer + mouseTrackingMode none）时不阻断，事件继续到 xterm 原生监听。仅 `preventDefault()` 不足以阻断同元素已注册的原生监听
- 合成事件来源标记：`dispatchEvent` 同步执行 → `markSynthetic(() => el.dispatchEvent(ev))`；实现为**嵌套计数 + try/finally**（`depth++; try { fn() } finally { depth-- }`，`syntheticEventInFlight = depth > 0`）——裸 boolean 在回调抛异常时会永久放开锁定门禁

#### D4.2 合成事件契约（构造参数必须完整，否则 xterm 取坐标/按键字段出错）

- **WheelEvent**：`new WheelEvent('wheel', { clientX, clientY（取触点坐标）, deltaY（逐帧增量，非累计）, deltaMode: 0（像素）, bubbles: true, cancelable: true })`，派发到 `.xterm` 元素。**deltaY 方向钉死**：`deltaY = previousClientY - currentClientY`（手指上滑 → 正值 → 向缓冲底部滚）——与 xterm 5.5.0 原生 `Viewport.handleTouchMove`（`deltaY = lastTouchY - pageY; scrollTop += deltaY`）符号约定完全一致，避免反向滚动
- **MouseEvent**：mousedown/up 均带 `{ clientX, clientY, button: 0, buttons（down=1/up=0）, bubbles: true, cancelable: true }`；**mouseup 必须 bubbles:true**——xterm 的 mouseup 监听动态挂在 `document` 上
- **compatibility mouse 抑制**：自定义 tap 必须对原 touch 序列 `preventDefault()`（touchstart 即调，需 `passive:false`），否则 Safari 会再生成兼容 mousedown/up → 同一审批点触发两次 SGR 点击（双确认风险）
- **touch 追踪**：用 `changedTouches` + `touch.identifier` 锁定单指；`touchend.touches.length===0` 时从 changedTouches 取坐标；`touchcancel` 复位手势状态；仅 `touches.length===1` 且 |Δy|>|Δx| 接管滚动，多指/水平忽略
- tap 判定：位移 <8px 且时长 <300ms；tap 仅在 `mouseTrackingMode` active 时合成点击，否则忽略（normal buffer 的聚焦交给 xterm 原生 mousedown）
- 惯性：v1 不做惯性，跟手滚（避免与 xterm 内部 scrollTop 管理竞争）
- 已知边界：若 opencode TUI 未开启 mouse reporting，合成点击不产生效果、wheel 退化为方向键——**spike 任务在真实 web/xterm→WS→tmux→opencode 链路上先行验证；spike 阴性 = 阻断本 change 并升级给用户**（用户已决策触摸点击必须支持，不得静默降级为键盘验收）

### D5: 锁定模式与统一输入门禁（L2）

```
              WS 连接建立/auth_ok（含一切重连、Tab 切换）
                        │
                        ▼
                ┌──────────────┐   点「解锁输入」   ┌──────────────┐
   (coarse) ──▶ │   LOCKED     │ ────────────────▶ │  UNLOCKED    │
                │ overlay 遮挡  │                    │ 手势层接管    │
                │ blur textarea│ ◀──────────────── │ 可聚焦/键盘   │
                │ 仅滚动手势    │   点「锁定」/任意    └──────────────┘
                └──────────────┘   WS 重连
                fine pointer：恒 UNLOCKED，无锁定 UI
```

- 状态唯一所有者：**TermSession**。`lock.ts` 提供纯状态 + overlay DOM 操作函数，由 TermSession 持有与调用；TerminalView 只经 props 渲染浮动按钮、回调 TermSession（不多状态源）
- **`lock()` 顺序敏感**：先置门禁标志（拒绝新输入），再 `term.blur()`——若先 blur，focus-out 控制序列（`\x1b[O`，sendFocus 模式）可能在门禁生效前发出
- 锁定时 overlay 拦截 + textarea blur → textarea 不可能聚焦，硬件键盘/composition 尾事件无目标
- **统一输入门禁**：xterm 有两个发送出口——`onData`（键盘/IME/`term.input`，UTF-8 字符串）与 `onBinary`（默认鼠标编码等非 UTF-8 控制序列）。两者 MUST 汇入同一门禁；`onBinary` 按 charCode 逐字符取原始字节（`String.fromCharCode` → charCodeAt & 0xFF），MUST NOT 用 UTF-8 TextEncoder（会破坏 >127 的编码字节）：

```ts
private sendInput = (d: string, binary: boolean) => {
  if (!this.authed || this.ws?.readyState !== WebSocket.OPEN) return;
  if (this.locked && !this.syntheticEventInFlight) return;  // 锁定期只有合成手势产生的字节放行
  if (binary) {
    const bytes = new Uint8Array(d.length);
    for (let i = 0; i < d.length; i++) bytes[i] = d.charCodeAt(i) & 0xFF;
    this.ws.send(bytes);
  } else {
    this.ws.send(encoder.encode(d));
  }
};
this.term.onData((d) => this.sendInput(d, false));
this.term.onBinary((d) => this.sendInput(d, true));
```

- 语义澄清：锁定期"零意外输入"= 键盘/IME/粘贴/意外触摸/鼠标事件零发送；**刻意的 TUI 滚动手势会产生 wheel/方向键字节**（这是 TUI 滚动的唯一机制），属预期行为，经 syntheticEventInFlight 同步标记放行
- 锁定状态不持久化：每次连接建立一律 LOCKED（coarse），用户已确认重连（含 Tab 切换）强制锁定
- **pointer 类型动态变化**（coarse↔fine，如 iPad 接/拔鼠标或触控板、桌面触屏本；仅外接键盘不改变 pointer 语义——`pointer` 描述主指针设备）：matchMedia change 时重评估——转入 fine：若当前 LOCKED 则自动解锁并隐藏锁定 UI；转入 coarse：强制 LOCKED 并显示锁定 UI； overlay 与浮动按钮与 session 锁状态始终同步（状态源唯一，无 UI/session 分叉）
- **层叠关系**（z-index 自底向上）：终端画面 < 锁定 overlay < 浮动锁定/解锁按钮 < 连接状态 overlay（`.terminal-overlay`，断线提示/重新连接/接管按钮）。连接状态 overlay 必须最顶层——其上是既有操作控件，锁 overlay 不得遮挡
- **`unlock()` 调用栈敏感**：必须在可信用户手势（解锁按钮 click）调用栈内**同步**执行「移除 overlay → `term.focus()`」——iOS Safari 只允许在真实用户手势同步栈内 focus 唤起虚拟键盘，异步（setTimeout/Promise）会被拒绝
- 浮动锁定/解锁按钮锚定在 `.terminal-wrap` 内右下角（absolute）；D6 键盘弹出时对 **`.terminal-wrap` 整体**施加 maxHeight（见 D6，按钮与连接 overlay 同处 wrap 内、随其收缩上移），保持可见可点（满足「随时重新锁定」）

### D6: 虚拟键盘与 visualViewport（L3）

- iOS Safari 弹键盘不触发 layout viewport 变化（dvh 不变），只缩/平移 visualViewport → 必须显式处理
- 算法：UNLOCKED 且 textarea 聚焦时监听 `visualViewport` 的 `resize` **与 `scroll`** 事件（Safari 为保持输入点可见会平移 visual viewport，offsetTop 变化只触发 scroll；rAF 去抖）：

```ts
const vv = window.visualViewport;
const top = wrap.getBoundingClientRect().top;            // layout viewport 坐标
const visibleBottom = vv.offsetTop + vv.height;          // 可视底边（含平移）
wrap.style.maxHeight = Math.max(0, visibleBottom - top) + 'px';  // clamp 防负值
fitNow();  // → 既有 WS resize 链路透传
```

- 尺寸所有者：**`.terminal-wrap`**（absolute inset 0；对其设 `maxHeight` 时 over-constrained 的 `bottom` 被忽略，高度从顶部收缩）——wrap 同时承载 `.terminal-host`、锁定 overlay、浮动按钮与连接 overlay，**收缩 wrap 才能让浮动按钮与连接控件随可视区上移**（只收缩 `.terminal-host` 会把锚定 wrap 底部的按钮留在键盘后方）；blur/锁定/卸载时移除内联样式并 refit
- API 缺失降级：跳过（键盘可能遮挡输入区，可接受，记录）
- 隐藏 pane（clientWidth===0）跳过 fit（沿用现有 session.ts:246 逻辑）

### D7: 应用侧 IME 补偿（L3，候选输入仲裁，fail-closed）

**模型**：候选字符不立即补发，先挂起一个宏任务，观测 xterm 原生 `onData` 实际发出了什么，结算时只补「原生没发」的内容。这把补偿器与 CompositionHelper 两个生产者的竞争从「猜时序」变成「观测事实」，且全部使用公开 API（`term.onData` / `term.input`）与标准 DOM 事件，**零 xterm 私有字段依赖**。

TermSession 内对 `term.textarea` 注册 capture 监听，镜像事件流：

- `anyKeyDown`：任意 keydown 置 true / 任意 keyup 置 false（镜像 xterm `_keyDownSeen` 的置位/清除规则）
- `nonModKeyDown`：仅非修饰键 keydown 置 true / 任意 keyup 置 false（镜像 PR #6054 修正语义）。**不置位集合**：纯修饰键按 `KeyboardEvent.key` 判定 `{'Shift', 'Control', 'Alt', 'Meta', 'CapsLock'}`；**IME 处理键按 `key === 'Process'` 或 `keyCode === 229` 双条件判定**（xterm 5.5.0 自身按 keyCode===229 判定；Safari 可能产出 `key:'Unidentified', keyCode:229` 的变体，只判 key 会漏）——229 不置位的理由：Safari 对 IME 消费键的 keyup 可能缺失或延迟（WebKit 170369/169209 系列时序变体；macOS 拼音按住 Shift 连打两个全角标点的实测失败即此变体），若 229 置位且无 keyup 清除，`nonModKeyDown` 会卡在 true 导致后续标点候选被 fail-closed 误丢；排除 229 后 Chrome 229 路径改由仲裁的「观测原生覆盖」抑制（见不双发论证第 3 条），结果等价仍单发；其余全部 keydown 置位
- `compositionActive`：compositionstart → true / compositionend → false

**候选资格判决（capture `input`，target===textarea，全平台启用）**——与 xterm gate 精确互补，保证「只有 xterm 必丢的事件才进入仲裁」：

```
ev.inputType === 'insertText' && ev.data
  && ev.isTrusted                ← 排除脚本合成事件（composed 可被合成事件伪造）
  && ev.composed === true        ← 只处理真实 UA 事件（isTrusted 之上的第二道闸）
  && [...ev.data].length === 1   ← 单 Unicode code point 候选（补偿目标为单个全角标点；
                                   多字符 ev.data 无法做精确 occurrence 分配 → fail-closed）
  && anyKeyDown                  ← xterm gate 必丢（_keyDownSeen=true）
  && !nonModKeyDown              ← 不是普通按键的重复投递
  && !ev.isComposing && !compositionActive
     ↓ 全部满足（任一不确定 → fail-closed 直接丢弃，不进队列）
  pending.push({ text: ev.data, at: now() });  调度 settle（setTimeout 0，共享单定时器）
```

**时钟**：`now()` 为单调毫秒时钟（生产 = `performance.now()`，测试注入 fake clock）；`lastCapacityEvictionAt` 初值 `undefined`（不可用 `0`）。

**结算（settle）**：待结算批次在完成全部匹配前仍视为 pending（裁剪以完整 pending 集计算保护窗口——MUST NOT 先排出再裁剪）；排出队列，按序对每个候选查「原生发射历史」`recentNative`（ring buffer，记录 `{ text, at, remainingText }`，occurrence 级消耗模型）。**候选均为单 Unicode code point**（资格判决已限制），每个候选恰好消耗一处该字符出现——同字符 occurrence 可互换，贪心取最早记录即为精确分配（无多字符候选的争抢问题）：

```
候选 c 被覆盖 ⟺ 存在原生发射 e ∈ recentNative（多条可匹配时取时间最早者；
  时间戳相同取 ring 插入序最早者）：
  e.at ∈ [c.at - 100ms, settleNow]  且  e.remainingText 包含 c.text
被覆盖 → 丢弃（原生已发），并从 e.remainingText 中删除 c.text 的一处出现
       （occurrence 级消耗：消耗的是一次文本出现而非整条记录——
         原生聚合发射 onData("？？") 的一条记录可依次覆盖两个 "？" 候选；
         remainingText 耗尽后立即移出 ring，不再参与匹配）
未覆盖 → term.input(c.text, true) 补发
不可证明（见下）→ fail-closed 丢弃，不补发
```

- **裁剪安全规则**：惰性裁剪（仅在 `observeNative`/`settle` 操作时执行，不挂主动定时器）只允许丢弃「早于全部 pending 候选回看窗口起点（`min(c.at) - 100ms`）且超过 150ms」的记录——**MUST NOT 裁剪任何落在 pending 候选回看窗口内的记录**，否则 settle 会基于被裁掉的历史错误补发（主线程暂停 >150ms 的反例）
- **容量溢出规则**：硬容量上限 32 条，溢出丢最旧；若被丢弃的记录落在某 pending 候选的回看窗口内 → 该候选标记「不可证明」，settle 时 fail-closed 丢弃（宁可漏发，不基于残缺历史补发）。**历史缺口水位**：容量溢出还 MUST 记录 `lastCapacityEvictionAt = now()`（缺口产生时刻）；新候选入队时若 `lastCapacityEvictionAt >= c.at - 100ms`（候选回看窗口与历史缺口重叠）→ 该候选立即标记「不可证明」——覆盖「先溢出、后候选」的时序（被溢出的窗口内原生记录无法被后续候选观测）。水位超过 100ms 后自然失效（后续候选窗口不再与之重叠），无需主动清除
- **匹配顺序（确定性）**：多条记录同时可匹配一个候选时，固定选**时间最早**的可匹配记录（时间戳相同取 ring 插入序最早者），删除其中第一次出现；`remainingText` 耗尽的记录立即移出 ring（避免占用容量触发不必要的溢出 fail-closed）。候选限定单 Unicode code point 后，每个候选只消耗一处该字符出现，同字符 occurrence 可互换——贪心最早匹配无分配漏洞
- `recentNative` 通过独立的 `term.onData` tap 记录；**补偿器自己经 `term.input` 发出的内容不记录**（`term.input` 同步触发 onData，调用栈内用嵌套计数标记排除）——否则快速连按两次 `？` 时第二次会被自己第一次的补发误杀
- 补发延迟 ≤ 一个宏任务（通常约 0-4ms；主线程阻塞或后台节流时可能更长），仅作用于被 xterm 丢弃的字符，可忽略
- **MUST NOT 修改 textarea.value**：composition/229 路径的 deferred diff 依赖 textarea 内容，清空会让 pending 的 `_handleAnyTextareaChanges` 定时器发出 `DEL`（源码核实）。此禁令仅约束应用层代码；xterm 自身（blur/Enter）清空 textarea 的行为不受影响

**为什么不依赖 `compositionend.data`**：xterm 源码注释明确其在 Chromium 不可靠；仲裁模型观测的是「原生实际发出了什么」而非「compositionend 声称提交了什么」，该不可靠字段完全不参与判决。

**不双发论证（枚举 xterm 全部三个原生生产路径）：**

1. **`_inputEvent` 路径**：补发 ⇒ 候选满足 `composed=true 且 anyKeyDown=true` ⇒ xterm gate（`!ev.composed || !_keyDownSeen`）对该事件必然关闭 ⇒ xterm 不经此路径发送 ⇒ 无竞争（充要，镜像同一事件流）
2. **CompositionHelper `_finalizeComposition(true)` 路径**（compositionend 后 setTimeout(0) 延迟读 textarea 发出）：候选晚于 compositionend 到达时，xterm 的 finalize 定时器先于我们的 settle 定时器注册，setTimeout(0) FIFO ⇒ settle 一定在 finalize 之后执行 ⇒ 「汉字后立即 Shift+？」（？抢先进入 textarea）时 finalize 原生发出「你好？」，settle 观测到覆盖 ⇒ 抑制补发。这是**调度顺序保证**，不是时间猜测
3. **`_handleAnyTextareaChanges` 路径**（keydown 229 deferred diff）：229 不置位 `nonModKeyDown`（见上），故 229 相关 input 可成为候选。两种时序均无竞争：
   - **Chrome 时序**（keydown 229 先于 input）：xterm 的 diff 定时器在 keydown 时已注册、先于我们的 settle 定时器（setTimeout(0) FIFO）⇒ settle 时 diff 已原生发出 ⇒ 观测到覆盖 ⇒ 抑制补发
   - **Safari 时序**（input 先于 keydown 229，可能连续批量到达）：存在「input1 → 229#1 → input2 → 229#2 且定时器批量执行」的竞争窗口——settle 若先于 diff#1 执行会补发两个、diff#1 再发一个（共 3 次）。**闭合机制：候选 pending 期间收到 229 keydown 时，MUST 取消并重新调度 settle**（scheduler 契约已有可取消 handle）——重排的 settle 落在该 keydown 注册的 diff timer 之后 ⇒ diff 先结算、原生发出后进 recentNative ⇒ settle 观测覆盖、只补未覆盖候选。单次输入时 229 重排无行为差异（diff 空读不写）

**残余向量（显式记录，不属于补偿器契约）：**

- **xterm 原生双发**：迟到重复 commit 到达时若无任何按键按住（`anyKeyDown=false`），xterm gate 自行通过并把 finalize 已发的内容再发一次——这是 xterm 5.5.0 自身缺陷，无补偿器时同样存在，应用侧不改包无法阻止。经验证据：现今目标浏览器（macOS/iOS Safari 拼音）无此双发报告
- **lookback 窗口启发式**：「迟到重复 commit + 按住修饰键」依赖 100ms 回看窗口抑制；超出窗口会补发重复。失败方向为双发，但要求同一文本刚被原生发出且事件满足全部资格交集，经验上不可达（同上）；窗口本身是 fail-safe 的（抑制=安全方向），且自清除无长期残留
- **窗口内误抑制（漏发方向）**：100ms 回看窗口内若出现与候选同文本的无关原生发射，候选会被误抑制造成漏发——这是 occurrence 匹配的固有代价，方向为 fail-closed（用户重按即可恢复），作为已接受 trade-off 显式记录

**契约（有界保证）**：补偿器自身 MUST NOT 引入双发——对 settle 前已观测且位于 100ms 回看窗口内的原生覆盖 MUST NOT 补发；漏发优先于双发（漏发用户重按即可，双发会向 TUI 注入错误指令）；**原生覆盖因裁剪/容量溢出不可证明时 MUST fail-closed 丢弃候选，不得基于残缺历史补发**。**超出观测窗口的迟到重复 commit 可能补发重复内容，是已接受并显式记录的残余风险**（见上「lookback 窗口启发式」：在「不改包」约束下，公开事件仲裁无法区分「超窗迟到重复」与「用户再次输入相同字符」，绝对零双发与该约束不可兼得——产品决策：接受超窗残余，换取目标字符可靠补发）。

| 路径 | 仲裁结果 | xterm 侧 | 总发送 |
|---|---|---|---|
| 普通 ASCII 按键（nonMod 已按下） | 资格不成立 | keydown 路径发 | 1 ✓ |
| Safari/iOS Shift+？（无 composition 历史） | 补发（无原生覆盖） | gate 丢弃 | 1 ✓ |
| 输入汉字后**立即** Shift+？（？抢在 finalize timer 前进 textarea） | 候选 → settle 观测到原生「你好？」覆盖 → 抑制 | finalize 发「你好？」 | 你好+？各 1 ✓ |
| 输入汉字后稍后 Shift+？（finalize 已发「你好」） | 补发（原生历史不含 ？） | gate 丢弃 | 你好+？各 1 ✓ |
| 快速连按 Shift+？两次 | 各补发一次（自身补发不进 recentNative） | gate 均丢弃 | 2 ✓ |
| Chrome IME 直交（composition 内提交） | 资格不成立（isComposing） | CompositionHelper 发 | 1 ✓ |
| Chrome 229 路径 | 候选 → settle 观测到 deferred diff 原生覆盖 → 抑制（FIFO） | deferred diff 发 | 1 ✓ |
| Safari 按住 Shift 连打两个标点（229 keyup 缺失/延迟变体） | 两次均为候选（229 不置位 nonMod）；229 keydown 触发 settle 重排 → diff 先结算 → 仲裁观测原生覆盖后只补未覆盖候选 | gate 均丢弃（keyup 缺失时 `_keyDownSeen` 同样卡 true）或经 deferred diff 发出 | 2 ✓ |
| 汉字 composition 提交（commit input 在 end 前） | 资格不成立（compositionActive） | CompositionHelper 发 | 1 ✓ |
| 迟到重复 commit（按住修饰键，finalize 已原生发出） | settle 观测 lookback 覆盖 → 抑制 | finalize 已发 | 1 ✓ |
| iOS 预测/听写（无按键 → anyKeyDown=false） | 资格不成立 | gate 通过自发 | 1 ✓ |
| 未知时序变体 | fail-closed 丢弃 | — | 宁可漏发 |

- 补偿器为纯逻辑模块：事件序列 + 注入时钟/调度器/原生发射流 → 输出补发动作，vitest fake timers 单测覆盖上表全路径，**断言原生 + 补偿的总发送次数**（而非只断言补偿器 emit 次数）
- 顺带效果：修复桌面 Safari 全角标点（原 wontfix 解除，用户已批准）

### D8: 落点与模块接口

- `web/src/hooks.ts`（改）：+ `useMediaQuery`
- `web/src/terminal/lock.ts`（新）：

```ts
createLockController(host: HTMLElement): {
  readonly overlay: HTMLElement;                    // 手势层挂载目标（LOCKED 时）
  lock(): void;                                     // 先置内部 locked 再操作 DOM（配合 D5 顺序）
  unlock(): void;
  isLocked(): boolean;
  onChange(cb: (locked: boolean) => void): () => void;  // 状态订阅（TerminalView 渲染按钮、gestures 切换挂载点）
  dispose(): void;
}
```

- `web/src/terminal/touch-gestures.ts`（新）：

```ts
attachTouchGestures(opts: {
  term: Terminal;                                   // 读 modes.mouseTrackingMode / buffer.active.type 做运行时路由
  getTarget(): HTMLElement;                         // LOCKED=lock.overlay / UNLOCKED=xterm 元素（onChange 时重挂）
  markSynthetic<T>(fn: () => T): T;                 // syntheticFlag 同步包裹（回调进 TermSession）
  wheelCtor?: typeof WheelEvent;                    // DOM 事件构造器注入（默认全局；Vitest Node 无 DOM 时注入 mock）
  mouseCtor?: typeof MouseEvent;
}): { rebind(): void; dispose(): void }
```

- `web/src/terminal/ime-compensator.ts`（新）：候选仲裁纯逻辑模块（D7）——

```ts
createImeCompensator(opts: {
  emit: (data: string) => void;                     // 接 term.input——MUST 同步调用（FIFO 与自身发射排除论证依赖同步性）；调用栈内嵌套计数标记，排除自身补发进 recentNative
  now: () => number;                                // 时钟注入（测试 fake timers）
  schedule: (fn: () => void, delayMs?: number) => { cancel(): void };  // 调度器注入，返回可取消 handle；生产实现 = 同 Window 的 setTimeout(fn, delayMs ?? 0)
}): {
  handleKeyDown(e): void; handleKeyUp(e): void;
  handleCompositionStart(): void; handleCompositionEnd(): void;
  handleInput(e): void;                             // capture input → 资格判决 → 挂起候选
  observeNative(text: string): void;                // 接 session 的 onData tap
  dispose(): void;                                  // 取消 pending settle、清空状态（session 卸载时调用）
}
```

时钟与调度器注入（测试用 fake timers）；不触碰 DOM/xterm/textarea.value
- `web/src/terminal/session.ts`（改）：统一 sendInput 门禁（onData+onBinary）、coarse 判定、对外暴露 `lock() / unlock() / isLocked / onLockChange(cb)`（TerminalView 的唯一接口）、接线 lock/gestures/ime-compensator/visualViewport，dispose 全清理。**构造签名扩展为接收 `{ host, wrap }` 两个 DOM ref**（D6 需要直接操作 `.terminal-wrap` maxHeight，MUST NOT 依赖 `parentElement` 或 class 查询推断 wrap）
- **测试缝（Vitest 跑在 Node，无 DOM，不加 jsdom）**：门禁判定抽纯函数 `shouldSendInput(state: { authed, wsOpen, locked, syntheticInFlight }): boolean`；二进制编码抽纯函数 `encodeBinaryInput(d: string): Uint8Array`（charCode & 0xFF）；touch-gestures 的路由/阈值判定与事件参数构造抽纯函数（`resolveGestureRoute(mode, bufferType)`、`buildWheelInit(...)`/`buildMouseInit(...)` 返回 plain object），DOM 事件构造器以参数注入（默认全局 `WheelEvent`/`MouseEvent`，测试注入 mock）——纯逻辑单测不需要 DOM 事件类；session 接线层另以 vi.mock 依赖的 adapter 测试实例化 TermSession 锁定委托关系（非纯函数层）
- `web/src/terminal/TerminalView.tsx`（改）：仅从 TermSession 上述接口取状态投影 + 回调；锁定/解锁浮动按钮——**按钮可见性由 TerminalView 侧 `useMediaQuery('(pointer: coarse)')` 控制**（session 只暴露锁状态，不复制 pointer 状态；两者正交组合：按钮显隐=coarse，按钮图标/文案=locked 投影）
- `web/src/terminal/mobile.css`（新）：锁定 overlay、浮动按钮等终端侧移动样式——从 styles.css 落点调整为 terminal 专属文件（TerminalView 引入），原因：P2（布局）与 P3（终端）并行实施，避免同一 styles.css 的并行写冲突
- `web/src/pages/TaskWorkbenchPage.tsx` + `styles.css`（改）：D1-D3
- `web/index.html`（改）：D2 viewport
- `web/package.json`（改）：`@xterm/xterm` 钉精确版本 `5.5.0`（去 `^`）、+ devDependency vitest、`test` script

## Risks / Trade-offs

- [opencode TUI 未开 mouse reporting → 触摸点击/滚轮滚动失效] → spike 任务 0.1/0.2 在真实 web/xterm→WS→tmux→opencode 链路首验；**spike 阴性 = 阻断本 change 并升级给用户决策**（用户已要求触摸点击必须支持，不允许静默降级）；另注意 iOS 虚拟键盘无完整方向键，键盘降级本身也不完备
- [非 SGR 鼠标编码下点击字节经 onBinary 发送，遗漏会丢点击或破坏锁定] → D5 统一门禁同时接 onData/onBinary，onBinary 按 charCode 原始字节发送
- [Safari compatibility mouse events 与合成点击叠加 → 审批双确认] → D4.2 契约：自定义 tap 对原 touch 序列 preventDefault 抑制兼容鼠标事件
- [IME 补偿在未知 IME 时序变体下漏发（fail-closed 代价）；极端窗口外时序存在理论双发残余] → D7 候选仲裁：补发决策基于对原生 onData 的观测而非时序猜测；「汉字后立即标点」竞争由 setTimeout(0) FIFO 调度顺序保证闭合；残余向量显式记录——(a) xterm 原生双发、(b) 超窗迟到重复（经验不可达）、(c) 窗口内同文本无关原生发射误抑制候选（漏发方向，fail-closed，用户重按即可恢复，已接受 trade-off）；判决表全路径 fake-timers 单测（断言原生+补偿总次数）；真机 macOS Safari + iOS Safari 拼音验证
- [合成事件依赖 xterm 5.5.0 的 DOM 事件语义，升级 xterm 可能漂移] → package.json `@xterm/xterm` 改精确版本 `5.5.0`（去 `^`）+ frozen lockfile，建立升级复核点（本设计 D4/D7 两节）
- [重连强制锁定在频繁切 Tab 时打扰] → 用户已明确选择该策略；解锁一次点击成本低
- [dvh/safe-area 旧浏览器不支持] → vh fallback 在前，env() 不支持时为 0，优雅降级
- [visualViewport 平移后遮挡] → D6 监听 resize+scroll、公式含 offsetTop、clamp 防负值

## Migration Plan

纯前端增量，无数据迁移。按 L0→L3 任务组提交，各组独立可验收可回滚；桌面端路径（fine pointer + ≥1025px）除 IME 补偿修复外不变。回滚=revert 对应 commit。

## Open Questions

- ~~opencode TUI 的 mouse reporting / 滚轮行为~~ → **spike 已回答（阳性）**：tmux→opencode 真实链路捕获到 TUI 启动时发出 `ESC[?1000h`+`?1002h`+`?1003h`+`?1006h`（完整鼠标追踪 + SGR 编码）与 `?1049h`（alternate buffer）。结论：合成 mousedown/up 经 xterm 鼠标协议转 SGR 点击序列可送达 TUI（触摸审批可行）、合成 wheel 转 SGR 64/65（推理历史滚动可行）；alt buffer + mouse reporting 同时证实原生 touch 滚动必然失效、手势接管为必需。浏览器全链路（xterm→WS）段落由 lib 源码核实（合成事件进入 xterm mouse reporting 路径、WS 字节透传）+ 实现期 P3/真机验收 5.4 兜底确认
