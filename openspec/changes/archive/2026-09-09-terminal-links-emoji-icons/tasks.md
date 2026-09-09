# Tasks: terminal-links-emoji-icons

执行依赖：`1.1 → 2.1/3.1 → 各自测试`；`1.2 → 4.1 → 4.3`；`4.2/4.3 → 4.4`；全部实现及专项测试完成后进入 5。`session.ts` 由 2.1/3.1 共同修改，归同一实现负责人协调、顺序提交。

## 1. 依赖与资产

- [x] 1.1 安装 `@xterm/addon-web-links@^0.12.0` 与 `@xterm/addon-unicode-graphemes@^0.4.0`（与 xterm 6.0.0 配套），更新 `web/package.json` + `web/pnpm-lock.yaml`；`pnpm install --frozen-lockfile` 与 `pnpm build` 通过
- [x] 1.2 vendor 字体资产：下载 nerd-fonts v3.5.1 `NerdFontsSymbolsOnly.tar.xz`（design D3 记录完整 URL），取 `SymbolsNerdFontMono-Regular.ttf` + LICENSE（MIT）放入 `web/src/assets/fonts/`；在 tasks 完成记录中写明下载 URL、文件 sha256 与实测体积
  - 完成记录：下载 URL = `https://github.com/ryanoasis/nerd-fonts/releases/download/v3.5.1/NerdFontsSymbolsOnly.tar.xz`；`web/src/assets/fonts/SymbolsNerdFontMono-Regular.ttf` sha256 = `fe471e538392f51910faab985fa8e192a39dd3426125edd15b71b3680df0e749`，实测体积 = 2,610,012 字节（约 2.49 MB）；`web/src/assets/fonts/LICENSE`（MIT, 1,082 字节）取自同 release 压缩包随附 LICENSE。

## 2. D1 链接接线（bug: Cmd+点击打开链接）

- [x] 2.1 `web/src/terminal/session.ts`：Terminal 构造加 `linkHandler: { activate: openLink }`（不设 allowNonHttpProtocols）；加载 `WebLinksAddon(openLink)`；`openLink` = 修饰键门控（`metaKey || ctrlKey` 否则 return）+ 同步 `window.open(uri, '_blank', 'noopener,noreferrer')`；被阻止不重试、不转当前页导航、不影响终端会话
- [x] 2.2 测试：门控纯函数三路径（metaKey/ctrlKey/无修饰，window.open 桩）；adapter 测试断言 linkHandler 选项与 WebLinksAddon 接线复用同一函数；真实 Terminal 写入固定 OSC 8 序列驱动激活路径（修饰键 mouseup 触发一次）

## 3. D2 Emoji graphemes

- [x] 3.1 `session.ts` 加载 `UnicodeGraphemesAddon`（loadAddon 即激活 '15-graphemes' provider）
- [x] 3.2 测试：真实 provider 缓冲宽度组件测试——ASCII/CJK 不变；样本序列（👨‍👩‍👧、🏳️‍🌈、👍🏽、🇨🇳、é=e+U+0301、连续 😀😀😀）单簇/正确占位；既有终端测试（session-adapter/term-recovering/session-coordination）全绿。**失败处置门禁：若 graphemes 引入宽度/布局错位，回退该 addon；本 change 不得标记完成，返回文档阶段重新对齐，不得以移除 emoji 能力放行**

## 4. D3 字体回退（图标 + emoji 字形）

- [x] 4.1 新增 `web/src/terminal/fonts.css`（`@font-face` Symbols Nerd Font Mono + `unicode-range: U+E000-F8FF, U+F0000-FFFFD, U+100000-10FFFD` + `font-display: block`），`TerminalView.tsx` import
- [x] 4.2 `preferences.ts`：`DEFAULT_FONT_FAMILY` 字面量按 design D3 更新（emoji 族在 CJK 字体与末尾 `monospace` 之前、Symbols 最末）；`resolveFontFamily` 实现幂等变换规则——CSS family 列表语义解析（引号/转义/名字含逗号合法 family 不改写）、emoji 族已存在保留原位/缺失按固定顺序末尾追加、Symbols 移除全部已存在项后统一追加绝对末尾；不写回偏好存储；**初始化与 `applyPreferences` 共用同一变换函数**
- [x] 4.3 加载生命周期：终端打开后 `document.fonts.load('13px "Symbols Nerd Font Mono"', '\u{E0A0}')` 单次尝试；成功后存活实例 `clearTextureAtlas()` + `refresh(0, rows-1)`；失败 `console.warn` 降级；不重建终端/不重连/不改偏好/不动锁定；已销毁实例跳过
- [x] 4.4 测试：`resolveFontFamily` 八例全集（design D3 列出：默认栈恒等/单字体自定义/含 generic/emoji 族保留原位/Symbols 归末尾/Symbols 原在末尾缺 emoji/单引号/名字含逗号）；@font-face 声明与 fonts.load 触发、成功后清 atlas+refresh、失败降级、销毁跳过的组件测试；**偏好即时更新通道组件验收（`TERM_PREFS_CHANGED → applyPreferences → resolveFontFamily`）**：有效栈更新、不写回偏好、隐藏后激活与重连场景终端实例身份保持（不重建）

## 5. 集成回归与手动验收

- [x] 5.1 全量回归：`cd web && pnpm test && pnpm build` 全绿；`go build ./... && go test ./...` 全绿（后端零改动，确认无破坏）
- [x] 5.2 手动验收矩阵逐条执行（design「手动验收矩阵」九条 + 彩色分类边界样本 ⚡/❤/❤️/🇨🇳 验收门禁）——用户已确认通过
