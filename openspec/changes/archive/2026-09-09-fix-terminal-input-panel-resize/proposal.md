# Proposal: fix-terminal-input-panel-resize

## Why

用户在使用 ocdeck Web 控制台时遇到 6 个明确的交互 bug，集中在两个区域：终端输入（Shift+Enter 换行失效、中文输入法切换产生重复输入、点击侧边栏任务后焦点未落到终端）与面板布局（应用侧边栏、git 评审左侧文件面板、diff 并排 new/old 边界均不支持拖拽调整，git 左侧面板不支持收起）。这些问题直接破坏终端输入的正确性与工作区布局的可用性。

## What Changes

修复 6 个 bug，分为两组：

**终端输入组（既有机制失效/遗漏路径，侧重根因修复）：**

1. **Shift+Enter 换行失效**：在终端中按 Shift+Enter 应触发 opencode 的换行快捷键（插入换行），当前行为是直接发送消息（shift 修饰键丢失）。
2. **中文输入法切换重复输入**：中文输入法输入到一半直接切换英文输入法时，已输入内容被重复发送（如输入 "nihao" 欲得「你好」，切换后终端收到 "hi haohihao" 而非 "nihao"）。预期是同一段输入恰好送达一次。
3. **点击侧边栏任务后焦点不落到终端**：点击侧边栏任务项导航到工作台后，键盘焦点停留在侧边栏，按上下方向键操作的是侧边栏而非 TUI。预期是任务导航后焦点转移到终端。

**面板布局组（从零新增能力）：**

4. **应用侧边栏宽度拖拽**：侧边栏支持拖拽调整宽度，调整后宽度持久化（刷新后保持）。
5. **Git 评审左侧文件面板拖拽/收起**：git 评审的左侧文件列表面板支持拖拽调整宽度，并支持收起/展开。
6. **Diff 并排 new/old 边界拖拽**：git 评审 side-by-side diff 视图中，new/old 两侧的边界支持拖拽调整比例。

**范围边界与非目标：**

- 覆盖六项 Web 交互修复，以及 bug 1 必需的最小后端终端运行环境配置修复；不修改 WS/PTY 协议语义与帧格式。
- 不引入 E2E 测试；验收以单元/组件测试 + 手动验证清单为准。
- 拖拽后的宽度/比例持久化，刷新后保持。
- 不做侧边栏/diff 布局的整体重构，不替换 diff 渲染引擎。

## Capabilities

### New Capabilities

（无全新 capability——6 个 bug 均落在已有 capability 的行为修正/增强上）

### Modified Capabilities

- `terminal-streaming`: Shift+Enter 换行键翻译、IME 组合输入去重两个既有终端输入行为的修正（requirement 级行为变化：修正错误行为、补全遗漏路径）。
- `web-ui-shell`: 侧栏导航新增「点击任务后焦点转移到终端」行为；应用侧边栏新增宽度拖拽调整与持久化能力。
- `git-operations`: git 评审面板新增左侧文件面板拖拽/收起、diff 并排边界拖拽的布局交互能力（仅前端布局行为，不改 git 数据契约）。

## Impact

- **前端代码**：`web/src/terminal/session.ts`、`web/src/terminal/TerminalView.tsx`、`web/src/components/AppShell.tsx`、`web/src/components/GitPanel.tsx`、`web/src/components/diff/DiffViewer.tsx`、样式文件（`design-system.css`、`legacy-components.css`）。
- **依赖**：升级 `@xterm/xterm` 至 6.0.0（连带 addon 升级），并新增一个受管理依赖补丁修复 6.0 未覆盖的输入法切换双发射缺陷（机制与交付物见 design D2）。
- **新增**：可复用的面板拖拽 resize 能力（代码库当前无可复用工具）。
- **持久化**：新增 localStorage 持久化项（侧边栏宽度、git 面板宽度、diff 分栏比例）。
- **测试**：`web/src/__tests__/` 下新增/更新组件测试（Shift+Enter 回归、IME 切换去重、焦点转移、拖拽 resize）；Go 侧新增 tmux 配置测试。
- **后端**：bug 1 根因证实需要最小后端改动——终端运行环境需启用扩展键支持（机制见 design D1）；不改 WS/PTY 协议语义。对使用者的迁移说明：修复对新建/重建的终端会话生效；发布前已在运行的存量会话需重新激活任务后生效。
