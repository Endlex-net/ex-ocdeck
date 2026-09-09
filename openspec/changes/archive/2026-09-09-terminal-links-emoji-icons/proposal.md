# Proposal: terminal-links-emoji-icons

## Why

ocdeck 的浏览器终端在三个方面落后于本地终端（iTerm）体验，影响日常使用：

1. **链接不可点击**：终端输出的 URL（opencode markdown 链接、日志中的地址）在 iTerm 中可以 Cmd+点击直接打开，在 ocdeck 中只能手动复制。
2. **Emoji 序列支持**：需要支持组合 emoji 的 grapheme cluster 宽度计算与渲染（ZWJ 序列、肤色修饰、旗帜等不拆散、不错位）。
3. **图标显示为豆腐块**：shell 主题（starship 类）输出的 Nerd Font 图标（文件夹、分支、箭头等私有区字形）在浏览器终端显示为 □（用户截图实证）；根因已确认：终端字体栈不含 Nerd Font 字形覆盖。

## What Changes

**终端增强（三项，均为前端终端层新增能力）：**

1. **Cmd+点击打开链接**：终端识别链接并支持 Cmd+点击（macOS；Ctrl+点击兼容）请求当前浏览器以新标签页/窗口打开；普通点击不误触发。覆盖纯文本 http(s) URL 与程序主动输出的超链接（OSC 8）两类。
2. **Emoji 正确渲染**：修复 emoji 豆腐块，支持组合 emoji 正确显示与占位（机制与保证口径见 design D2/D3）。
3. **Nerd Font 图标内置**：终端内置图标字体回退，Nerd Font 私有区图标（文件夹/分支/提示符符号等）开箱即可显示，无需用户本地安装字体；正文字体与既有字体偏好设置行为不变。

**范围边界与非目标：**

- 仅前端终端层；后端、WS/PTY 协议零改动。
- 不做文件路径点击打开（纯文本路径不自动成链）。
- 不做终端搜索（Cmd+F）。
- 不引入 Unicode 11 宽度表切换（无宽度错配证据，避免引入新错位风险）。
- 不改动既有 IME/输入门禁/锁定语义。

## Capabilities

### New Capabilities

（无全新 capability——三项均为终端渲染/交互既有 capability 的增强）

### Modified Capabilities

- `terminal-streaming`: 终端渲染与交互增强——链接识别与修饰键点击打开、grapheme cluster 宽度/渲染、图标字形内置回退（requirement 级新增行为）。

## Impact

- **前端代码**：`web/src/terminal/session.ts`（addon 加载与字体栈）、`web/src/terminal/preferences.ts`（默认字体栈）、样式/字体资源声明。
- **依赖**：新增 `@xterm/addon-web-links`、`@xterm/addon-unicode-graphemes`（版本与 xterm 6.0.0 配套，详见 design）；新增内置图标字体资源文件。
- **测试**：`web/src/__tests__/` 新增组件/单元测试（链接识别与修饰键门控、字体栈回退声明、addon 加载）；验收含手动验证（真实浏览器中 Cmd+点击、emoji 序列、Nerd Font 图标渲染）。
