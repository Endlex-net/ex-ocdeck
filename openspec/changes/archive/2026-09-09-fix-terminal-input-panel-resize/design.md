# Design: fix-terminal-input-panel-resize

## Context

ocdeck Web 前端（`web/src`，React + CodeMirror merge 6.12.2 / view 6.43.9，pnpm-lock.yaml 锁定；xterm.js 本 change 由 5.5.0 升级至 6.0.0，见 D2）经 WS → PTY → `tmux -L ocdeck -f /dev/null attach` 桥接 opencode TUI。6 个 bug 分两组：终端输入组（bug 1/2/3）与面板布局组（bug 4/5/6，代码库无任何可复用 splitter，从零新增）。

关键现状（均已原文核实）：

- `web/src/terminal/session.ts:497-512` 已存在 Shift+Enter → CSI `\x1b[27;2;13~`（modifyOtherKeys 格式）拦截翻译；`web/src/terminal/ime-compensator.ts` 已实现 IME 候选仲裁（fail-closed：漏发优先于双发，契约见其头注释 12-30 行与 `qualifyCandidate` 80-92 行）。
- 后端 tmux 以 `-f /dev/null` 运行（`internal/infrastructure/process/process.go:312` `tmuxArgs`），全仓库无任何 `extended-keys` 配置（grep 核实）。
- 既有 `EnsureServerOptions`（`process.go` ~:550 起）是 tmux server 选项的幂等配置入口：`NewSession` 建会话拉起 server 后调用（`process.go:513-517`），`reconcile` 在存量 server 上调用（`internal/task/reconcile.go:99-100`）；剪贴板配置有严格的版本分段、顺序与 fail-closed 补救语义，调用方一律 best-effort 记日志不阻断。
- `TerminalView.tsx` 全文无 `.focus()` 调用；侧栏任务项为纯 `<a href="#/task/:id">`（`AppShell.tsx:208-231`）；`TaskWorkbenchPage` 按 taskID key 重挂载（`App.tsx:213`）——同一任务再次点击 taskID 不变、不重挂载。
- 侧栏宽度纯 CSS 变量 `--sidebar-w: 232px`（`design-system.css:59,146`）；`.git-side` 桌面固定 340px（`legacy-components.css:921`）、窄屏 `width:100%`（`legacy-components.css:1716-1719`）；side-by-side diff 为 CodeMirror `MergeView`（`DiffViewer.tsx:808-820`，创建前异步 `loadLanguage` `:762-764`；重建依赖 `:848`）。

## Goals / Non-Goals

**Goals:**
- bug 1/2 根因修复（端到端行为正确 + 回归测试），bug 3/4/5/6 新增交互能力。
- 三处拖拽共用一套零依赖 resize 机制；宽度/比例持久化 localStorage。
- 不破坏既有契约：统一输入门禁（`web/src/terminal/input-gate.ts` `shouldSendInput`）、IME fail-closed、终端锁定零发送语义、窄视口堆叠布局、git 编辑/预览生命周期。

**Non-Goals:**
- 不引入 resize 第三方依赖；不引入 E2E；xterm.js 升级仅限 6.0.0 目标版本（不追最新），升级动机与回归面见 D2。
- 不做侧栏/git 面板/diff 布局重构，不替换 diff 渲染引擎。
- git 文件面板收起态不持久化，仅会话内保持（宽度持久化）。
- 不修改 git 数据契约与 WS/PTY 协议帧格式。

## Decisions

### D1 — bug 1 根因（已实证）：tmux `extended-keys` 默认 off

**实证记录**（2026-09-08，本机 tmux 3.7c，`/opt/homebrew/bin/tmux`，PTY 实验：pane 内 raw-mode 读取器启动时发 `CSI >4;1m` 模拟 OpenTUI 请求，PTY attach 客户端 stdin 注入 `\x1b[27;2;13~`）：

| 场景 | 结果 | 结论 |
|---|---|---|
| A：extended-keys 默认 off | pane 收到 `\r` | bug 复现：shift 丢失，与线上症状一致 |
| B：pane 先请求模式（被忽略），之后才 `set -s extended-keys on` | pane 收到 `\r` | **竞态真实存在**：配置晚于 pane 请求则无效 |
| C：先 `set -s extended-keys on`，pane 之后才请求 | pane 收到 `\x1b[27;2;13~` | 配置先于 pane 请求时修复生效 |
| D：`new-session ... \; set -s extended-keys on` 同链 | pane 收到 `\x1b[27;2;13~` | 同链配置在 pane 进程 boot 前生效（server 命令队列顺序处理） |
| E：new-session 返回后**立即**单独 exec `set -s extended-keys on`（无 sleep，pane 为真实进程启动），重复 5 次 | 5/5 pane 收到 `\x1b[27;2;13~` | 观测结果：典型环境下分离调用通常生效，但**不构成顺序保证**（竞态由场景 B 证伪） |
| F：`start-server` 单独拉起空 server | server 立即退出 | `exit-empty` 默认 on，空 server 不存活，单纯 start-server 不可用 |
| G：`start-server \; set -s exit-empty off \; set -s extended-keys on`（单次调用、纯配置链）→ new-session → `set -s exit-empty on` 恢复 | pane 收到 `\x1b[27;2;13~`；全部会话结束后 server 自动退出 | **严格顺序保证**：配置先于首个 pane 创建；恢复 exit-empty on 后 server 生命周期语义与现状一致 |

调研佐证（源码级）：tmux 客户端 stdin 解析接受 `CSI 27;m;k~` 与 `CSI k;mu` 两种格式（`tty-keys.c tty_keys_extended_key`）；pane 侧重编码按 pane 模式，扩展键模式仅在 `extended-keys` ≠ off 时接受 pane 的 `CSI >4;Nm` 请求（`input.c INPUT_CSI_MODSET`），3.x 全系列默认 off（`options-table.c`）；OpenTUI `parseKeypress` 双格式识别（`parse.keypress.ts` modifyOtherKeysRe），`input_newline` 绑定 `shift+return`（opencode `keybind.ts`）。实验同时证明：**无需配置 `terminal-features`**——attach 客户端 TERM 为通用 xterm 系、无 extkeys feature 时，3.7c 仍能解析客户端 stdin 扩展序列并送达 pane（场景 C/D 均未配置 terminal-features）。

**修复方案（固定 `on`，无备选授权；严格顺序保证）**：`set -s extended-keys on` 必须**先于首个 pane 创建**生效。NewSession 路径改为三步：

1. **server 前置初始化**（new-session 之前，单次 execTmux 调用，纯配置链不创建会话）：`start-server \; set -s exit-empty off \; set -s extended-keys on`。`start-server` 显式拉起 server；`exit-empty off` 防止空 server 立即退出（实证 F）；server 命令队列顺序处理 → 配置先于任何 pane 创建（实证 G）。该链失败（含低版本不支持 extended-keys）→ 记日志后继续走原 new-session 路径（降级为旧行为），由落点 2 幂等重试。
2. **new-session**：现状不变（独立 execTmux，错误语义不变）；此时 server 已存在且已配置。
3. **EnsureServerOptions**（NewSession 尾部 + reconcile 两个既有调用点）：**三个独立尝试的步骤**——①剪贴板配置（内部版本分段/安全顺序/fail-closed 补救一字不动）；②幂等 `set -s extended-keys on`（<3.2 跳过）；③恢复 `set -s exit-empty on`（恢复既有 server 生命周期：会话清零后 server 自动退出，实证 G 已验证）。步骤间互不跳过：除 `ErrNoTmuxServer`（确定无 server）外，①②任一失败 MUST 仍执行③；③使用独立超时执行；各步错误 errors.Join 汇总返回，调用方 best-effort 记日志不阻断。

**已在运行的旧 pane 不追溯修复**（其模式请求已被忽略且不可重放），pane 重建（任务重新激活拉起新 opencode pane）后生效——接受的迁移边界，已同步 spec delta 与 proposal。

**创建结果 × 配置结果失败矩阵**：

| 步骤 | 失败行为 |
|---|---|
| 前置初始化链 | 无任何创建；记日志后继续 new-session（降级旧行为），EnsureServerOptions 幂等重试 |
| new-session | 沿用现状：返回创建错误，无会话；**随后立即 best-effort 恢复 `exit-empty on`**（独立调用，保留原创建错误、恢复失败与之汇总）；MUST NOT kill-server 或影响已有会话。恢复成功则不留任何残留（无空 server 依赖后续 reconcile 恢复的场景） |
| new-session 成功 + 键盘 set 失败（EnsureServerOptions 内） | **保持会话创建成功**，记录配置失败日志，exit-empty 恢复步骤照常执行；MUST NOT 因配置失败清理或重建会话 |

- 版本下限：`extended-keys` 需 tmux ≥ 3.2，`tmuxVersion`/`tmuxVersionAtLeast` 复用既有工具函数；<3.2 或不可解析时**前置链与 EnsureServerOptions 步骤均跳过** extended-keys 记日志（`start-server`/`exit-empty` 为古老命令无需门禁）。
- shell pane 不变量：`on` 仅对请求扩展键的 pane 生效，shell 等未请求 pane 行为不变（实证 A→C 差异只在请求后）。
- 执行通道：一律经 `Manager.execTmux`，沿用实际 `socketName`、`-f /dev/null`、`TMUX_TMPDIR`、超时与有界错误输出；EnsureServerOptions 内失败仅汇总错误返回（errors.Join 风格），`ErrNoTmuxServer` 直接返回，调用方 best-effort 记日志不阻断。
- Go 测试 MUST 覆盖：前置链命令序列与顺序、前置链失败降级路径（new-session 照常、EnsureServerOptions 重试）、exit-empty 恢复、创建成功但键盘 set 失败（会话保留、日志记录、剪贴板步骤仍执行）、低版本跳过、存量 server reconcile 幂等。
- 前端 CSI `27;2;13~` 翻译保持不变。
- **并发约束（I1）**：同一 tmux server 的「前置配置—new-session—失败恢复 exit-empty」窗口 MUST 在 process 层串行化——Manager 增加互斥锁保护 NewSession 的该事务区间与独立的 `EnsureServerOptions`（含 reconcile 调用点），参与同一同步机制的尾部调用经不重复加锁的内部 helper 避免死锁。反例交错（必须排除）：A 前置配置完成 → B 前置配置完成并等待创建 → A 创建失败恢复 `exit-empty on` → 空 server 退出 → B 的 new-session 拉起未配置的默认 server。任务级锁（activate.go）不隔离不同任务，不能替代本约束。Go 测试 MUST 用 channel/barrier 控制两个 NewSession 交错，覆盖「一方创建失败、另一方已完成前置配置但尚未创建」证明恢复不使另一方失去预配置 server。

### D2 — bug 2 根因（已证实）：xterm.js 5.5.0 CompositionHelper 双发射

**根因**（权威来源：xterm.js 5.5.0 源码 + issue #5778 及其修复 PR #6140 + W3C UI Events 规范；调研确认非浏览器 bug）：

- 浏览器行为符合规范：输入法切换是 `compositionend` 的规范触发条件，`compositionend.data` 为 commit 文本，compositionend 本身不产生 input 事件，commit 文本经 `insertText`（isComposing=false）交付。
- 重复来自 xterm.js：`CompositionHelper.keydown` 的排除集为 {229,16,17,18}，**不含 CapsLock(20)/eisuu 等输入法切换键** → 切换键 keydown 落到 `_finalizeComposition(false)` **立即 emit 一次且不写 `_dataAlreadySent`**；随后浏览器 `compositionend` → `_finalizeComposition(true)` 的 setTimeout(0) 延迟路径因去重偏移为 0 **再次 emit**。已复现的示例中两次内容为同一 commit 文本（"hi haohihao" 类重复），但不排除 deferred 分支携带 commit 后新增字符（其读取 `substring(start)`）。
- WebKit 附加差异（#5887/#6045/#5894）：input 先于 keydown、keydown keyCode=229/keyup 真实键码、部分 IME 模式无 composition 事件——去重规则必须不依赖 keydown 顺序假设。

**事件表（Chromium，切换输入法中途结束 composition）**：

| # | 事件 | 关键字段 |
|---|---|---|
| 1 | keydown（切换键，如 CapsLock/eisuu） | keyCode ∉ {16,17,18,229}，isComposing=true |
| 2 | （xterm 内部）`_finalizeComposition(false)` → onData emit commit 文本 **第 1 次** | — |
| 3 | compositionend | data=commit 文本 |
| 4 | （xterm 内部 setTimeout 0）→ onData emit 重复内容（已复现示例为同一 commit 文本）**第 2 次** | ← 重复源 |

**修复方案（升级到 6.0.0 + 受管理的最小依赖补丁）**：

- **升级**：`@xterm/xterm` 5.5.0 → **6.0.0**，连带 `@xterm/addon-fit` ^0.10.0 → **^0.11.0**、`@xterm/addon-webgl` ^0.18.0 → **^0.19.0**（5.x addon 声明 peer `^5.0.0`，与 6.0 不兼容，npm registry 元数据核实）。6.0 原生覆盖两个 IME 修复：CapsLock（keyCode 20）加入 keydown 排除集（#5282）与 Wubi 五笔 `_compositionPosition.end` 修复（#5024）。
- **升级影响面（已核实，grep `web/src`）**：代码库未使用 6.0 移除/变更的 API——无 `overviewRulerWidth`/`windowsMode`/`fastScrollModifier`/`addon-canvas`/`rendererType`，无 alt→ctrl+方向键 hack 依赖；`attachCustomKeyEventHandler`/`onData`/`term.input` 在 6.0 不变（release notes），Shift+Enter 拦截（D1 前端侧）不受影响。接受的行为变化：滚动条重做（#5096）与 `disableStdin` 语义微调——纳入手动验收回归。
- **仍需补丁**：#5778 修复 PR（#6041/#6140）未合入且晚于 6.0.0 发布，6.0 上非排除切换键（Ctrl+Space/eisuu/重映射键）路径仍双发。纯应用侧 onData 过滤无法可靠归属发射来源（立即 finalize 与 deferred finalize 从外部不可区分，文本/时序推断均有反例：composition 后 F7 序列会被误拼进记录导致仍然双发；IME 撤回后无 deferred 发射，等待态会吞掉后续合法输入）。因此对锁定的 `@xterm/xterm@6.0.0` 打最小补丁。
- **补丁内容**：逐字移植上游 PR #6140 的修复点——在 `CompositionHelper._finalizeComposition` 的立即分支（非排除键 keydown 触发的 `_finalizeComposition(false)` 路径）记录 `this._dataAlreadySent = input;`，使 deferred 分支按既有 `_dataAlreadySent.length` 偏移跳过已发送文本（6.0.0 的 CompositionHelper 与 5.5.0 结构一致，修复点同样适用）。补丁作用于 npm 包发布产物的**运行入口**（package.json `"main"` 指向的编译产物，6.0 含 ESM 入口 `lib/xterm.mjs`——两个入口产物都需覆盖），经 `pnpm patch @xterm/xterm@6.0.0` 生成 `web/patches/@xterm__xterm@6.0.0.patch` 提交入库，并在 `web/package.json` 声明 `pnpm.patchedDependencies`（`"@xterm/xterm@6.0.0": "patches/@xterm__xterm@6.0.0.patch"` 精确版本映射）。**交付物包含更新后的 `web/pnpm-lock.yaml`**（patchedDependencies 哈希入锁文件）；验收 MUST 覆盖全新安装：`pnpm install --frozen-lockfile`（CI 既有用法）通过、真实 Terminal 复现测试通过、`pnpm build` 通过。
- **版本纪律**：补丁绑定精确版本 6.0.0；后续升级 @xterm/xterm 时 pnpm 强制复核补丁匹配，届时 MUST 重新核对上游是否已合入修复（#5778 为开放 issue）。
- **应用侧 compensator 不动**：既有 IME 候选仲裁（fail-closed、occurrence 级去重、自身发射排除）保持不变；不引入任何 sentCommit/前缀剥离机制。
- **既有补偿机制与 6.0 的重叠核对**（调研实证）：compensator 防御的 xterm 缺陷 #5887（`_inputEvent` gate 丢字）/#6045（textarea diff 翻滚双发/丢失）/#6078（隐藏 textarea 累积重发）/#6089（`_isSendingComposition` 共享布尔丢字）在 6.0.0 **全部未修复**（issue 均 open 且明确在 6.0.0 复现；`_inputEvent` gate 与 `_handleAnyTextareaChanges` diff 与 5.5.0 逐字节一致）——compensator 在 6.0 下仍必要且自中和（原生已发则不补发），全部保留。6.0 原生修复（#5282 CapsLock、#5024 Wubi）不在 compensator 防御范围内，无冲突；CapsLock 路径的「原生修复 × 本补丁 × compensator」三方交互纳入手动验收（见矩阵）。
- **边界语义归属**：CapsLock 切换、F7 按键、IME 撤回 composition、commit 后新增后缀输入等边界由 6.0 原生行为 + 上游补丁语义承载（#6140 自带浏览器测试已覆盖 composition 后 F7 期望 `[COMPOSITION, F7]` 与撤回后不再发 commit 两组场景）。
- 交付前提与失败边界不变：仅当输入门禁通过（已认证、WS 可写、未锁定）时才交付；锁定零发送；任何不确定路径 MUST NOT 补发、MUST NOT 退格或重发整串修正。

**测试与放行条件**：

- **实施放行门槛**：jsdom 挂载真实 `Terminal` 的复现测试——合成 compositionstart/update + **不在 6.0 排除集 {16,17,18,20,229} 内的键**（固定用 F7，keyCode 118）keydown + compositionend 事件序列驱动真实 `CompositionHelper` 走双发射路径，断言修复后 onData 恰好收到 commit 文本一次；该测试 MUST 在未打补丁的 6.0.0 依赖下变红、打补丁后变绿——红绿对照使用**同一 6.0.0 版本**，唯一变量是补丁是否应用（实现者举证）。CapsLock（keyCode 20）路径为 6.0 原生修复，另列为独立的原生回归用例，不承担补丁红绿举证。
- 回归：`ime-compensator.test.ts`、`session-adapter.test.ts` 等既有测试全绿。
- 手动验收（Chrome + Safari，拼音输入法输入 "nihao" 中途切英文）：终端收到恰好一份 "nihao"；正常中文上屏回归不丢字。

### D3 — bug 3：导航聚焦请求信号 + 就绪门禁

**信号机制（固定实现，无二选一）**：新模块 `web/src/terminal/focus-request.ts`，模块级内存单例：

- 生产 API：`requestTerminalFocus(taskID: string)`；请求字段 `{ taskID, seq（递增）, ts }`；过期时间固定 **5s**。
- 三个生产点（导航仍走原生 href/navigate，信号为附加调用）：`AppShell` 侧栏任务项 onClick、工作台任务切换器选择、指挥中心任务行点击——覆盖同 taskID 再次点击（key 重挂载覆盖不了的场景）。
- **单例保留最新 pending 请求**；新请求覆盖旧请求。
- 唯一消费方：目标任务的 **TUI TerminalView**（以 wsPath 形态区分：`/ws/terminal/<taskID>` 为 TUI，`/ws/terminal/shell/...` 为 shell 实例，shell MUST NOT 消费）；路由不匹配（请求 taskID ≠ 当前路由任务）→ 暂不消费、保留请求。
- **订阅交接**：TerminalView 挂载订阅时同步交付当前 pending 快照（解决「发布早于挂载」）；消费/取消按 seq 比较，仅清除匹配 seq 的请求；订阅清理（卸载）MUST NOT 删除更晚发布的新请求。组件测试 MUST 覆盖「发布 B 请求 → A 卸载 → B 订阅 → B connected → 聚焦」。

**标签激活阶段（G1 修复，闭合同任务再点击）**：目标 `TaskWorkbenchPage` 收到匹配且未过期的焦点请求时，若当前 tab 不是 TUI（如 Git/设置/shell 标签），经既有 `switchTab(TUI_TAB)`（`TaskWorkbenchPage.tsx:162`）激活 TUI 标签——**只激活标签，不消费请求**；TUI TerminalView 挂载/连接后按下述状态表与门禁正常消费。标签激活仅由显式导航请求触发：普通重连、无请求的挂载 MUST NOT 切标签。组件测试 MUST 覆盖「当前 Git 标签 → 点击同任务 → TUI 标签激活 → connected 后聚焦」。

**状态表（请求 → 就绪 → 门禁 → 聚焦/取消）**：

| 请求到达时终端状态 | 行为 |
|---|---|
| 已 `connected` | 立即执行门禁检查，通过则聚焦，请求消费 |
| 首次连接中（idle/connecting/recovering） | 挂起等待；进入 `connected` 后门禁检查并消费 |
| 重连中（reconnecting） | 显式导航请求优先：挂起等待该次连接 `connected` 后门禁检查并消费（用户意图新鲜） |
| 普通重连自身 | MUST NOT 产生新请求；已消费请求不因重连重新聚焦 |

**门禁（全部满足才聚焦）**：① 请求未过期（5s）且 seq 最新；② 请求 taskID 与当前路由任务匹配；③ 终端会话 `connected` 且未锁定（锁定 MUST NOT 聚焦）；④ 焦点保护：`document.activeElement` 为 `body`、`.od-sidebar` 内元素、任务切换器或指挥中心任务行内元素；⑤ **输入元素排除优先于区域白名单**：activeElement 为 input/textarea/contenteditable 时 MUST NOT 聚焦。

**取消**：用户主动进入任何输入区、路由离开目标任务、请求过期、就绪时终端锁定 → 请求作废，不留待聚焦状态。

**聚焦**：经 `TermSession` 暴露的 `focus()` 方法（封装 `term.focus()`），不直接触 DOM。

### D4 — 共享零依赖 resize 原语（bug 4/5/6 共用）

新模块 `web/src/components/resize.tsx`：

- `ResizeHandle` 组件：6px 热区分割条，`role="separator"`、`aria-orientation="vertical"`、`tabIndex={0}`、`aria-valuenow/min/max` 反映当前尺寸。
- **拖拽状态机**（闭合 pointerup/lostpointercapture 竞态）：`idle → dragging → committed | cancelled → idle`。
  - pointerdown（仅 `e.isPrimary && e.button === 0`）：`setPointerCapture`，**冻结起始值**，进入 dragging。
  - pointermove（dragging 中）：`delta` = 相对 pointerdown 的累计位移；向右拖动增加左侧/上一侧尺寸；实时更新内存 state。
  - pointerup（dragging 中）：**先标记 committed、清除活动拖拽事务**，再保存存储一次，最后 release capture；此后到达的 lostpointercapture 无活动事务 → 仅清理，MUST NOT 回滚、MUST NOT 再写。
  - 仅 dragging 中收到 pointercancel 或意外 lostpointercapture → cancelled：恢复起始值且不写存储。组件卸载 MUST 清理监听与拖拽态。
  - **拖拽中失效（G4 修复，统一结局）**：活动拖拽（dragging 未提交）期间，handle 卸载、`enabled` 变 false、承载实例销毁或布局进入不允许 resize 的形态（折叠/断点切换/unified/预览等）→ 一律按 **cancelled** 结束：恢复存活持有者的起始内存值、不写存储、清理事务；此后迟到的 pointerup/lostpointercapture MUST NOT 提交。失效处理先于 handle 移除/模式失效执行。组件测试 MUST 覆盖「拖动中 → 切 unified/跨断点 → 返回」：断言恢复起始值、未写存储、返回后显示起始值。
- **单位适配**：px 模式回调 `onDrag(nextPx)`；比例模式 `ratio = clamp(startRatio + deltaPx / containerWidth)`，容器零宽 MUST NOT 启动拖拽；px 值统一取整后 clamp。
- **键盘**：方向键步进 10px（比例模式换算为 10px/容器宽度），每次有效调整后立即保存。
- `usePersistedSize(key, default, min, max, enabled)` hook：
  - **enabled 契约**（服务 D7 条件启用）：`enabled=false` 时 MUST NOT 读写存储，返回默认值；首次翻转为 true 时读取一次，之后内存保留。
  - **编解码**：存储值为 JSON number 文本。读取：`JSON.parse`；解析失败、非有限 number、空串、`null`、字符串型数字 → 回退默认值；px 模式下合法有限数值但非整数 → 回退默认值（不取整）；合法数值越界 → clamp 到 [min,max]；读取 MUST NOT 写存储；存储访问异常 MUST NOT 导致组件崩溃（回退默认）。
  - **写入**：拖动中实时更新内存 state（驱动渲染）；committed 保存一次；cancelled 不写；键盘每次有效调整保存；写失败（QuotaExceeded 等）捕获异常、保留本次内存布局。
  - 组件测试 MUST 覆盖「pointerup → lostpointercapture」序列：断言最终尺寸不回退且只写一次。

### D5 — bug 4：应用侧栏宽度

- `AppShell` 持有 `sidebarW` state（键/值域见文末持久化表），内联 `style={{ '--sidebar-w': ... }}` 设置在 `.od-shell` 容器上覆盖样式表变量（折叠态 `body.od-side-collapsed` 的 60px 规则天然优先，互不干扰）。
- `ResizeHandle` 渲染在 `.od-sidebar` 右缘；折叠（图标轨）时不渲染 handle；≤767px 视口（顶栏形态，侧栏任务组隐藏）不渲染 handle。
- ⌘B 展开后恢复持久化宽度（折叠只切 class，不动 width state）。

### D6 — bug 5：git 评审左侧文件面板拖拽/收起

- `GitPanel` 持有 `gitSideW`（持久化，键/值域见表）+ `gitSideCollapsed`（会话内 useState，不持久化）。
- **宽度经 CSS 自定义属性传递**：内联仅设置 `style={{ '--git-side-w': ... }}`；桌面规则改为 `width: var(--git-side-w, 340px)`，窄屏规则保持 `width:100%` 不动——内联固定值 MUST NOT 直接写 width。
- 收起入口：`.git-side` 工具栏加收起按钮（chevron）；收起时 `.git-side` 渲染为 **36px** 窄条（仅纵向展开按钮），diff 区 `flex:1` 占满；展开恢复收起前宽度（width state 未动）；收起态隐藏 resize handle。
- **断点状态表**：

| 视口 | 文件面板 | resize handle | 收起按钮 |
|---|---|---|---|
| >1024px | 自定义宽度（CSS 变量） | 显示（未收起时） | 显示 |
| ≤1024px（堆叠） | 强制完整显示 `width:100%`（收起偏好保留但不在堆叠态生效），返回桌面恢复 | 隐藏 | 隐藏 |

- 与 D7 区分：窄屏手动开启 side-by-side 时 diff 分栏 handle 仍可用（D7 handle 跟随 side-by-side 形态而非视口）。

### D7 — bug 6：diff 并排分栏比例

- `MergeView` 无公开 splitter API。机制：局部适配函数 `applySplitRatio(view, ratio)`：在 MergeView **实际创建完成后**（`loadLanguage` await 之后、`destroyed` 检查通过）及比例变化时调用；经 **`view.a.dom` / `view.b.dom`**（@codemirror/view 6.43.9 `EditorView.dom` 公开属性，pnpm-lock 锁定版本；`editorDOM` 不存在）定位编辑器节点，向上校验其父/包装节点确为 `.cm-mergeViewEditor` flex item（a=old、b=new，以构造参数 `a:{doc: oldContent}`/`b:{doc: newContent}` 为准），应用内联 `flex: 0 0 <pct>%`；实例已销毁或**节点关系校验失败** → 安全退出：禁用 handle、`console.warn`，MUST NOT 写布局或比例；锁定版本（merge 6.12.2）主路径结构验证失败须回设计修订。
- 比例 state：持久化浮点比例（键/值域见表），经 `usePersistedSize` 的 `enabled` 契约管理生命周期——**仅「源码模式 + merge 渲染状态 + side-by-side + 双侧文件均存在（`oldExists && newExists`）」时 enabled=true**（G2 修复：纯新增/纯删除的单侧坍缩路径 MUST NOT 应用比例——既有 `.diff-collapse-a/b` 空侧 `flex: 0 0 0` 规则（`legacy-components.css:1156-1160`）优先，内联比例会覆盖该规则产生不可见占位区；单侧形态下不应用比例、不显示 handle、不访问比例存储，已加载的内存偏好保留供返回双侧时恢复）：未启用不访问存储返回默认；首次启用读取一次；之后内存保留；异步 MergeView 创建完成时应用最新值。**比例变化 MUST NOT 加入编辑器重建依赖**（`DiffViewer.tsx:848` deps 不含比例），MUST NOT 触发 docChanged/文件保存/diff 重取/编辑会话清除。组件测试 MUST 覆盖双侧↔纯新增/纯删除切换：单侧坍缩不被比例破坏、返回双侧恢复比例。
- `ResizeHandle` 绝对定位覆盖在中缝（容器 `position:relative`，left = ratio × 有效容器宽度；有效容器宽度 = `.diff-editor` 容器 contentBox 宽度）；unified、markdown 预览（既有契约要求预览双侧等宽）、非 merge 状态提示下隐藏 handle（enabled=false，不读写存储）。

### 实施阶段与验收矩阵

取证已闭合并回填本设计：tmux 时序实证（D1 场景表）、IME 事件表与根因（D2，xterm.js #5778）、MergeView 锁定版本与公开 DOM 属性（D7）。实施阶段：

1. **共享 resize 原语**：D4 模块 + 单测（含 pointerup→lostpointercapture 序列）。
2. **接线**（依赖 1）：后端键盘配置（D1）、IME 补丁（D2）、焦点请求（D3）、三处布局（D5/D6/D7）。D1/D2/D3 共同落点 `session.ts`/`TerminalView.tsx` 区域——统一实现所有者，顺序集成，禁止并行改同一文件。
3. **集成回归与手动验收**。

**测试策略**：

- Go：复用 `execTmuxFn` 注入记录命令序列/注入失败，按 D1 三步流程断言调用顺序与失败隔离：前置初始化链（`start-server` + `exit-empty off` + `extended-keys on`）先于 new-session 完成、new-session 独立调用、EnsureServerOptions 三独立步骤（剪贴板/extended-keys/exit-empty 恢复）互不跳过；覆盖前置链失败降级（new-session 照常）、创建失败后立即恢复 exit-empty（原创建错误保留、恢复失败汇总）、剪贴板失败仍恢复、extended-keys 失败仍恢复、恢复本身失败的错误记录、`ErrNoTmuxServer` 直通、低版本跳过、reconcile 幂等；既有剪贴板安全矩阵测试保持绿。
- IME：D2 放行门槛的 jsdom 真实 Terminal 复现测试（未打补丁变红、打补丁变绿，实现者自检举证）；既有 IME/session 测试全绿。
- 组件：焦点请求取消路径（锁定/输入区/过期/同任务再点击/重连不产生请求）、存储读取异常回退、拖动取消恢复起始值、异步 MergeView 创建后比例应用、resize 不触发保存/rebuild。

**手动验收矩阵**（均含通过条件）：

| 场景 | 通过条件 |
|---|---|
| 新建任务 tmux + Shift+Enter | TUI 输入框插入换行，不提交 |
| ocdeck 重启后存量任务（pane 重建）+ Shift+Enter | 同上 |
| 普通 Enter / shell 终端 | 行为与修复前一致 |
| 真实 IME 输入 "nihao" 中途切英文（Chrome + Safari） | 终端收到恰好一份 "nihao"，无重复 |
| CapsLock 切换输入法路径（macOS，原生修复×补丁×compensator 三方交互） | 不重复、不丢字 |
| 终端渲染/滚动回归（6.0 滚动条重做 #5096 行为变化） | 渲染、滚动、选区行为正常 |
| 三处拖拽后刷新 | 宽度/比例恢复 |
| 宽↔窄断点往返 | 堆叠/桌面形态各自正确，偏好不丢 |
| 编辑态 resize | 不触发保存、不清除编辑会话 |

### 持久化键一览（契约，单一表述来源）

| 键 | 值域 | 默认 | 归属 |
|---|---|---|---|
| `ocdeck:sidebar-width` | int px ∈ [180, 480] | 232 | D5 |
| `ocdeck:git-side-width` | int px ∈ [240, 600] | 340 | D6 |
| `ocdeck:diff-split-ratio` | float ∈ [0.2, 0.8] | 0.5 | D7 |

（spec delta 只规定「持久化于 localStorage」；键名/取值域以本表为唯一来源，tasks 逐字引用。）

## Risks / Trade-offs

- **tmux < 3.2**：跳过键盘配置记日志（D1 已闭合）。
- **旧 pane 不追溯**：server 存活且 pane 未重建的旧会话 Shift+Enter 维持旧行为，pane 重建/新会话生效——接受该迁移边界（重建是常态路径），换取不动 activate 流程。
- **OpenTUI kitty 检测**：tmux 把 kitty push 误解析为 RCP，OpenTUI 在 tmux 内走 modifyOtherKeys 分支——正是依赖的分支；未来 tmux 支持 kitty 不受影响（双格式识别）。
- **D2 升级与补丁维护成本**：xterm 5.5.0→6.0.0 连带 addon 升级，滚动条重做（#5096）为接受的行为变化；补丁绑定 6.0.0，后续升级依赖时需复核上游 #5778 是否合入；pnpm patchedDependencies 使该复核在 install 期强制发生。边界语义（F7/撤回/后缀输入）由上游修复语义承载，应用侧无新增推断逻辑。
- **D1 初始化窗口**：前置链与 exit-empty 恢复之间存在 exit-empty off 的短暂窗口；new-session 失败路径已含立即 best-effort 恢复（失败矩阵），仅当恢复本身也失败时才残留空 server（纯内存进程、无会话、无害），此时记录日志，由下次 NewSession 的前置链/EnsureServerOptions 收敛。
- **MergeView DOM 依赖**：D7 用公开属性 + 关系校验 + 安全退出；升级 @codemirror/merge 需重核。
- **自动聚焦误判风险**（D3）：白名单 + 输入元素排除优先的保守策略，宁可不聚焦不抢焦点。
