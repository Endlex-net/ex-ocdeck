# Proposal: 移动端/iPad 终端工作台适配

## Why

当前 web 端是零响应式的纯桌面布局（无任何 media query / touch 处理 / 移动端检测），在手机与 iPad 上完全不可用：页面无滚动、终端无法触控滚动、固定宽度侧栏与弹窗溢出小屏。用户需要在移动场景下查看任务进度、点击审批确认、查看推理历史，并在必要时发送指引（含中文）；终端需要可交互但默认锁定防误触。

## What Changes

- 视口与安全区基础：`viewport-fit=cover`、iOS 安全区 inset、`dvh` 高度单位、松绑 `.input min-width` 等桌面硬编码尺寸（样式改动限定 `.workbench` 作用域，不影响管理页；viewport/safe-area/dvh 为全局基础设施例外）
- 工作台响应式布局：引入断点体系（phone ≤767px / iPad 768-1024px / ≥1025px 桌面），TaskWorkbenchPage 小屏窄化（tabstrip 横向滚动、header 收窄、Git 面板堆叠）；审批确认、任务进度、推理历史均在外部 opencode TUI 内部，不新增 web 原生 pane
- 终端触控交互：触屏设备上 TUI 终端的触控滚动（合成 WheelEvent 复用 xterm wheel 路径，覆盖 scrollback/alt buffer/mouse reporting）与**触摸点击 TUI**（合成 MouseEvent 转 SGR 鼠标序列，用于审批确认）
- 终端锁定模式：触屏设备上终端默认锁定（overlay 拦截、不聚焦、不弹虚拟键盘、零意外输入），显式解锁后可交互；任何 WS 重连（含 Tab 切换）后强制回锁定；桌面端行为不变
- 虚拟键盘适配：visualViewport resize 时终端容器高度调整 + FitAddon 重排 + PTY resize 同步
- 应用侧 IME 补偿：修复 xterm.js 5.5.0 在 WebKit/Safari（含 iOS）下丢失 IME 直交字符（全角标点 `？《》` 等）的上游 bug（xtermjs/xterm.js#5887，上游 PR #6054 未合并）；不改 xterm.js 包本身，应用层候选输入仲裁（观测原生 onData 后仅补未覆盖内容）+ `term.input()` 公开 API 补发，fail-closed、有界保证（100ms 观测窗口内原生覆盖 MUST NOT 补发，超窗残余显式记录）；全平台启用，顺带修复桌面 Safari
- 范围限定：本期仅适配任务工作台页（TaskWorkbenchPage）及其终端；配置页、任务列表等管理页保持桌面端体验不做移动适配；工作台"设置"Tab 内的环境变量表单仅做最小可用（不溢出），不做精细适配

## Capabilities

### New Capabilities

- `mobile-terminal-adaptation`: 移动端/iPad 断点布局、终端触控滚动与手势仲裁、终端锁定模式、虚拟键盘适配、应用侧 IME 直交字符补偿

### Modified Capabilities

- `terminal-streaming`: 新增触屏设备上终端默认锁定与解锁交互的要求（纯新增移动场景行为，不改变桌面端既有需求）

## Impact

- **前端（web/）**：`index.html`（viewport meta）、`src/styles.css`（断点、安全区、dvh）、`src/pages/TaskWorkbenchPage.tsx`（响应式重组）、`src/terminal/session.ts`（锁定模式、touch 手势、IME 补偿）、`src/terminal/TerminalView.tsx`（锁定 UI/解锁按钮）
- **后端（internal/）**：无改动（PTY resize / WS 桥接链路已支持任意尺寸，移动端复用）
- **依赖**：无新增运行时依赖；新增 devDependency vitest（IME 补偿器单测，已获批准）；`@xterm/xterm` 钉死 5.5.0 精确版本（不升级、不 patch；IME 补偿与手势层只使用公开 API 与标准 DOM 事件，无私有字段依赖）
- **风险**：触摸点击/滚动依赖 opencode TUI 开启 mouse reporting（实现期 spike 在真实链路首验，**不支持则阻断本 change 并升级给用户决策**，不得静默降级）；合成 DOM 事件语义与 xterm 5.5.0 耦合，升级 xterm 需复核手势层与 IME 补偿
