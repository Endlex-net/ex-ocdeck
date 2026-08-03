web 终端中文显示为 `_` 有两层根因：① 前端 xterm.js 字体栈不含 CJK 字形；② 后端 tmux attach 客户端运行环境无 UTF-8 locale（launchd 启动无 LANG），tmux 判定客户端为非 UTF-8 终端（`client_utf8=0`）并将 CJK 输出转写为 `_`。两层都需修复才能端到端正常显示中文；同时提供终端字体/字号可配置以匹配用户本地终端（如 iTerm2）偏好。

## What Changes

- **（后端）tmux 环境注入 UTF-8 locale 默认值**：宿主 LANG/LC_ALL/LC_CTYPE 均未设置或为空时，tmux 命令（含 attach 客户端）与会话进程环境默认注入 `LANG=en_US.UTF-8`；宿主显式设置的非空 locale 原样透传尊重。
- 扩展 xterm.js 默认 `fontFamily`，在等宽字体栈后追加 CJK 系统字体回退链（PingFang SC / Sarasa Mono SC / Noto Sans Mono CJK SC / Microsoft YaHei 等），使中文在装有常见 CJK 字体的浏览器环境中正常渲染。
- 「全局配置」页新增「终端外观」配置节：终端 fontFamily 与 fontSize（整数 8–32）可自定义（浏览器端全局偏好，不放任务级设置入口）。
- 终端外观偏好存浏览器 localStorage，对当前浏览器（含同源其他标签页）内所有任务的 TUI/shell 终端生效；未设置时使用新默认字体栈；偏好变更就地应用于已打开终端，不重建终端实例、不断开 WebSocket。
- 偏好保存/变更生效为纯前端链路（localStorage + xterm options 就地更新），不新增后端 API，不做跨浏览器同步，不内置 web font。

## Capabilities

### New Capabilities

（无）

### Modified Capabilities

- `terminal-streaming`: 新增终端文本渲染要求（CJK 回退字体栈保证中文可渲染）、终端进程 UTF-8 locale 要求（无有效 locale 时注入默认值，显式配置原样透传）与终端外观偏好要求（fontFamily/fontSize 可配置、localStorage 持久化、作用于全部终端实例）。
- `env-management`: env 基线最小基础集新增 `LC_ALL`/`LC_CTYPE` 透传（仅非空值），并定义无有效 locale 时注入 `LANG=en_US.UTF-8` 的兜底行为。

## Impact

- 代码：`web/src/terminal/preferences.ts`（新增：默认值常量、类型、校验、localStorage 读写全部契约的唯一 owner）、`web/src/terminal/session.ts`（字体栈改用偏好模块解析值 + 新增 `applyPreferences()` 就地应用）、`web/src/components/TermAppearanceEditor.tsx`（新增设置组件）、`web/src/pages/ConfigsPage.tsx`（挂载设置节）、`web/src/styles.css`（配置节样式）；后端 `internal/process/process.go`（`DefaultBaseEnv` locale 透传+兜底）、`internal/task/activate.go`（env 快照基础集同规则）及对应测试。
- API / 存储 schema：无影响（无新端点、无表结构变更）。
- 依赖：无新增。
- 兼容性：未设置偏好的现有用户仅字体栈变化（中文从 `_` 变为正常显示）；宿主显式配置的非空 locale 原样尊重；attach 客户端重连（浏览器刷新/重连）即获得新 locale 生效，既有会话内已运行进程的环境在启动时固化，需重新激活任务或新建会话才获得新 locale。无破坏性行为变更。
