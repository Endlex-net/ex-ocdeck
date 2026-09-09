# Design: terminal-links-emoji-icons

## Context

ocdeck 终端链路：浏览器 xterm.js 6.0.0（上一 change 已升级，含 addon-fit 0.11 / addon-webgl 0.19 与 #6140 IME 补丁）→ WS → PTY → tmux attach → opencode TUI / shell。本 change 纯前端终端层增强：链接点击、emoji grapheme、Nerd Font 图标。

关键现状（已核实）：

- `web/src/terminal/session.ts:110-134`：Terminal 构造（fontFamily 经 `resolveFontFamily`）、FitAddon/WebglAddon 加载点。
- `web/src/terminal/preferences.ts:17-18`：`DEFAULT_FONT_FAMILY = '"JetBrains Mono", "SF Mono", ui-monospace, Menlo, Consolas, "PingFang SC", "Sarasa Mono SC", "Noto Sans Mono CJK SC", "Microsoft YaHei", monospace'`——无 Nerd Font 覆盖（图标豆腐块根因，用户截图实证）。
- vite 无 public 目录；构建产物 `web/dist` 经 `web/embed.go` 嵌入 Go 二进制——任何 vite 打包资产（含字体）自动随二进制分发。
- xterm 6.0.0 配套 addon 版本（6.0.0 tag 实证）：`@xterm/addon-web-links@0.12.0`、`@xterm/addon-unicode-graphemes@0.4.0`（MIT）。
- web-links 默认 handler 无修饰键门控（任意点击即打开，源码实证）；OSC 8 由 xterm 6.0 核心解析存储并经核心自动注册的 OscLinkProvider 提供（走 Terminal `linkHandler` 选项）；纯文本 URL 由 web-links addon 的 WebLinkProvider 提供——两条入口独立（D1）。

## Goals / Non-Goals

**Goals：** 三项终端增强（spec 三个 requirement）；零后端改动；不破坏既有 IME/门禁/锁定/字体偏好语义。

**Non-Goals：** 文件路径点击；Cmd+F 搜索；addon-unicode11（无宽度错配证据）；改动用户字体偏好设置 UI。

## Decisions

### D1 — 链接：两条独立接线，复用同一修饰键门控打开函数

xterm 6.0 的链接有两个**不同**入口（源码实证，`web/node_modules/@xterm/xterm` 6.0.0）：纯文本 URL 经 `WebLinkProvider`（web-links addon 注册），OSC 8 超链接经核心自动注册的 `OscLinkProvider`、走 Terminal 构造选项 `linkHandler`（`ITerminalOptions.linkHandler?: ILinkHandler`，`xterm.d.ts:163`）。两者 MUST 各自接线、复用同一打开函数：

```ts
// session.ts Terminal 构造 + addon 加载处
const openLink = (event: MouseEvent, uri: string) => {
  if (!(event.metaKey || event.ctrlKey)) return;           // 修饰键门控（iTerm 语义）
  window.open(uri, '_blank', 'noopener,noreferrer');        // 同步调用，保住用户手势
};
new Terminal({ ..., linkHandler: { activate: openLink } }); // OSC 8 入口（allowNonHttpProtocols 不设 → 仅 http(s)，非法目标不打开）
term.loadAddon(new WebLinksAddon(openLink));                // 纯文本 URL 入口
```

- **激活时机**（上游实证）：linkifier 在 `mousedown` 记录链接、`mouseup` 命中同一链接才调 `activate`；两条入口均受门控。
- **不加自定义 hover/urlRegex**；OSC 8 不加 hover tooltip。
- **浏览器表述**：`window.open` 在用户手势中同步请求当前浏览器打开新浏览上下文（标签/窗口由浏览器决定）；被阻止时 MUST NOT 异步重试、MUST NOT 转为当前页导航、MUST NOT 影响终端会话。

**与锁定/鼠标报告的事件路径闭环（N2）**：

- **锁定态**：锁定 overlay（`mobile.css .terminal-lock-overlay`，z-index 高于终端画面）拦截全部 pointer 事件 → **锁定时链接不可点击**。spec 已按此定稿（锁定的语义就是不可交互；链接可点击仅在解锁态）。
- **解锁态 + 鼠标报告开启**：xterm 6.0 `bindMouse` 对修饰键不保证抑制报告（metaKey 不可编码进鼠标协议；是否报告及事件种类服从既有协议与强制选择规则）——Cmd+点击链接时除打开浏览器外，可能按当前鼠标协议向应用上报鼠标事件。**报告仍经既有 `onData`/`onBinary` → `sendInput` 统一门禁**（认证、WS 可写、锁定、合成标记），仅在原门禁允许时发送；本 change MUST NOT 修改或绕过该门禁。链接 handler 自身不产生键盘输入。接受该行为（opencode 中点击 scrollback 文本位置无可感副作用），手动验收⑧确认。
- 既有 `attachCustomKeyEventHandler` 与 IME 监听不受影响。

### D2 — Emoji：加载 unicode-graphemes addon

- `session.ts` 加载 `UnicodeGraphemesAddon`（`loadAddon` 即激活；上游实证：activate 将 `unicode.activeVersion` 切到 `'15-graphemes'` provider，非仅增加分段）。
- **验收门控（N6）**：若实测引入错位则**回退该 addon 且本 change 不得标记完成**（回退 = 范围未交付，须回到文档阶段重新对齐），不存在「移除 emoji 能力仍放行」的路径。
- ASCII/CJK 不变承诺的验证依据：既有终端测试全绿 + 真实 provider 下的缓冲宽度组件测试（写入 ASCII/CJK/emoji 样本断言单元格数）+ 手动验收 opencode TUI 边框/表格。
- 不装 unicode11（无宽度版本错配证据；见 proposal 非目标）。

### D3 — 图标与 Emoji 字形：内置/系统字体回退

**根因观察**：Nerd Font 图标豆腐块（用户截图实证）与 emoji 豆腐块（用户实证「终端上依旧是豆腐块」）均为渲染字形缺失。当前字体栈未显式列出 Nerd Font 或 emoji 字体族——这不足以独立证明根因（浏览器存在系统字体回退），本节显式补充两类回退并在问题环境验收效果。unicode-graphemes（D2）只管 grapheme 分簇与宽度，不提供字形；字形覆盖由本节的字体回退承载，两者正交且都需要。

- **emoji 字形（系统字体回退，无需 vendor；E1 用户定稿：彩色优先）**：字体栈引用系统 emoji 字体族 `"Apple Color Emoji"`（macOS）、`"Segoe UI Emoji"`（Windows）、`"Noto Color Emoji"`（Linux）——常见系统字体族名声明，可用时由浏览器匹配其一；**不保证安装或完整序列覆盖**（缺失平台降级豆腐块、不影响其余功能，见 spec）。不下载不打包，零资产零许可负担。当前观察到 emoji 缺字（用户实证豆腐块），本节显式补充常见系统 emoji 字体回退；具体效果以问题环境手动验收为准。
- **图标字形（内置资产）**：nerd-fonts 官方 release **v3.5.1** 的 `NerdFontsSymbolsOnly.tar.xz`（`https://github.com/ryanoasis/nerd-fonts/releases/download/v3.5.1/NerdFontsSymbolsOnly.tar.xz`）中取 `SymbolsNerdFontMono-Regular.ttf`；该目录 LICENSE 为 **MIT**（v3.4.0/v3.5.1 均实证）——vendor 时同目录随附 LICENSE 副本。不转换格式（浏览器原生支持 TTF；避免引入转换工具链）。实现任务 MUST 记录下载 URL、文件 sha256 与体积实测。
- **声明**：新增 `web/src/terminal/fonts.css`：

```css
@font-face {
  font-family: 'Symbols Nerd Font Mono';
  src: url('../assets/fonts/SymbolsNerdFontMono-Regular.ttf') format('truetype');
  font-display: block;
  /* N3：仅参与约定图标范围，杜绝正文/其他缺字误用图标字体 */
  unicode-range: U+E000-F8FF, U+F0000-FFFFD, U+100000-10FFFD;
}
```

  由 `TerminalView.tsx` import（与 `mobile.css` 同模式）。emoji 系统族名无需 @font-face。

- **默认栈字面量更新**：`DEFAULT_FONT_FAMILY`（preferences.ts:17）改为把 emoji 族放在所列 CJK 字体与末尾 generic `monospace` **之前**（彩色优先——emoji 族只含 emoji 码点，不拦截 CJK；先于可能含黑白双形态字形的 CJK 字体；前面的 `ui-monospace` 为 generic，实际彩色效果受下方验收门禁约束）：
  `'"JetBrains Mono", "SF Mono", ui-monospace, Menlo, Consolas, "Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", "PingFang SC", "Sarasa Mono SC", "Noto Sans Mono CJK SC", "Microsoft YaHei", monospace, "Symbols Nerd Font Mono"'`
- **有效字体栈变换规则（唯一规则，幂等；E4 定稿：两类回退族分开处理）**：`resolveFontFamily` 对任意输入栈执行：① **按 CSS family 列表语义解析**（正确处理单/双引号包裹与转义，保留非目标 family 的含义、引号形态和相对顺序；MUST NOT 用裸 `split(',')` 改写名字内合法含逗号的 family）并 trim；② **emoji 族**（`"Apple Color Emoji"`、`"Segoe UI Emoji"`、`"Noto Color Emoji"`）逐一检查：已存在则**保留原位置不动**，缺失则按此固定顺序追加到末尾；③ **图标族** `"Symbols Nerd Font Mono"` 规范化：移除所有已存在项（无论位置/引号形态），统一追加一次到**绝对末尾**（保证「仅此前全部字体缺字时命中」契约）。默认栈四族已内置 → 规则对其恒等。只改运行时有效栈，MUST NOT 写回用户偏好存储。初始化与 `applyPreferences` 共用同一 resolveFontFamily。
- **输入域约束（P9 定稿）**：解析器契约的输入域为**无注释的 CSS family 列表**。字体偏好保存入口 MUST 校验拒绝包含 `/*` 或 `*/` 的字符串（注释不参与；该输入只可能来自设置输入框，无合法用例）。localStorage 中经手工编辑混入注释的残留场景不保证去重正确性（不产生异常、不影响终端可用性），接受该残余风险。
- **彩色保证口径（E1 用户定稿：彩色优先；E6 分类定稿，按 Unicode 属性而非码点区间）**：
  - **彩色渲染范围**：默认 emoji 呈现字符（`Emoji_Presentation=Yes`，如 😀 U+1F600、⚡ U+26A1）、含 VS16（U+FE0F）的 emoji 呈现序列（如 ❤️ = U+2764+FE0F）、有效 ZWJ/旗帜等组合序列（如 👨‍👩‍👧、🇨🇳）——在前序字体（含用户自定义栈中的字体）未覆盖该码点/序列时命中彩色族。
  - **正文优先例外**：默认文本呈现且未请求 emoji 呈现的字符（如裸 ❤ U+2764、© U+00A9、U+1F321 类）以前序正文字体为准（可黑白），对默认栈与自定义栈同样适用。
  - **不建立字体文件级推论**：`ui-monospace` 等 generic 族随平台/浏览器解析，无法凭检查固定字体文件建立跨环境保证——彩色承诺以**验收环境（macOS + Chrome/Safari）手动验收实测**为准；分类边界验收样本：`⚡`（Emoji_Presentation=Yes 须彩色）、`❤`（裸字符正文优先）、`❤️`（VS16 须彩色）、`🇨🇳`（区域指示符序列须彩色）。验证不通过（默认栈下这四类样本未达预期）→ 本方案不得标记完成，回设计修订字体选择机制。
  - 「末尾追加 + unicode-range 双重保证」仅约束图标字体（emoji 族无 unicode-range）。
- **测试用例**：默认栈恒等、单字体自定义、含 generic 自定义、emoji 族已含部分（保留原位）、`"Symbols Nerd Font Mono", "Fira Code"`（Symbols 归末尾）、Symbols 原在末尾但缺 emoji 族（emoji 追加后 Symbols 仍最末）、单引号包裹的回退族、名字内含逗号的 family（如 `"Example, Mono"`）。

- **加载生命周期（N5）**：不阻塞终端 open。终端打开后调用 `document.fonts.load('13px "Symbols Nerd Font Mono"', '\u{E0A0}')`（电源线分支符码点采样）触发加载；成功 settle 后对所有存活 TermSession：webgl 渲染器 `clearTextureAtlas()` + `term.refresh(0, rows-1)` 消除已缓存的缺字结果（DOM renderer 回退路径 refresh 即可）；**单次尝试不重试**，失败记 `console.warn` 后终端继续可用（图标仍为豆腐块的降级态）；异步完成 MUST NOT 重建终端、重连 WS、修改偏好或改变锁定状态；终端已销毁则跳过该实例。emoji 系统字体不经 fonts.load（系统族名即时可用，无下载）。

### 测试策略（N7：契约—验证层—预期分层映射）

| 契约 | 验证层 | 预期 |
|---|---|---|
| 修饰键门控（metaKey/ctrlKey/无修饰三路径） | 纯函数单测（window.open 桩） | 仅修饰键路径调用 open；同步调用；被阻止不重试/不导航 |
| 两条链接入口接线（linkHandler 选项 + WebLinksAddon） | adapter 组件测试（mock Terminal 断言构造选项与 loadAddon） | 两入口均接同一门控函数 |
| OSC 8 真实解析与激活路径 | 真实 Terminal 组件测试（写入固定 OSC 8 序列 `\x1b]8;;https://example.com\x07text\x1b]8;;\x07`，模拟鼠标事件流） | 修饰键 mouseup 触发 openLink 一次 |
| grapheme 宽度（真实 provider） | 真实 Terminal 组件测试（写入样本断言缓冲单元格数/光标列） | ASCII/CJK 不变；样本序列单簇占位 |
| 字体栈变换 | `resolveFontFamily` 纯函数单测（默认栈恒等/单字体自定义/含 generic/emoji 族保留原位/Symbols 归末尾/Symbols 在原末尾缺 emoji/单引号/名字含逗号——D3 列出的完整用例集） | 幂等规则输出（emoji 位置保持 + Symbols 末尾规范化） |
| @font-face 与加载生命周期 | 组件测试（声明存在、fonts.load 触发、成功后 clearTextureAtlas+refresh、失败降级 warn） | 不重建终端/不重连/不改偏好/不动锁定 |
| 真实渲染与手势 | 手动验收（见下） | — |

固定样本：OSC 8 序列如上；图标码点 U+E0A0（powerline 分支）、U+E0B0、U+F07B（文件夹）；emoji/组合字符样本 👨‍👩‍👧、🏳️‍🌈、👍🏽、🇨🇳（regional-indicator 配对）、é（e+U+0301 组合附加符号）。

既有回归：session-adapter / term-recovering / session-coordination 等全绿（覆盖 IME/门禁/锁定不被破坏）；终端偏好即时更新通道（TERM_PREFS_CHANGED → applyPreferences → resolveFontFamily）含字体栈变换的行为由组件测试覆盖（隐藏后激活、重连场景不重建实例）。

**手动验收矩阵**（真实浏览器，TUI 与 shell 两种终端）：① 纯文本 URL Cmd+点击打开、普通点击不打开 ② OSC 8 链接（opencode markdown）Cmd+点击打开 ③ starship 类 shell 图标渲染（上述码点，非豆腐块）④ 组合 emoji 样本单簇渲染、光标列正确；**连续多个 emoji（😀😀😀 及 ZWJ 混合序列）不粘连**（用户实证的修复前缺陷） ⑤ opencode TUI 边框/表格无错位 ⑥ 自定义字体偏好下正文不变、图标仍显示 ⑦ 字体慢加载/失败（DevTools 网络节流/屏蔽）下终端可用且成功后图标补渲染 ⑧ 鼠标报告开启时（opencode TUI）Cmd+点击链接打开浏览器且 TUI 无可感误操作 ⑨ 锁定态链接不可点、解锁后恢复可点。

## Risks / Trade-offs

- **graphemes 宽度行为变化**：见 D2 护栏，可独立回退。
- **字体资产体积**：Symbols Nerd Font Mono TTF 约 1–2MB 级（实现时实测记录），进入 embed 二进制；`font-display: block` + 就绪后 refresh 消除首绘豆腐块。
- **OSC 8 与纯文本双入口**：两条独立接线复用同一门控函数（D1）；OSC 8 默认仅 http(s)（不设 allowNonHttpProtocols，防 XSS）。
- **鼠标报告**：修饰键点击可能按既有鼠标协议产生报告，具体行为及统一门禁约束见 D1；接受该行为，手动验收⑧确认。
