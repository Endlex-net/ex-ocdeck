## Context

web 终端由 `web/src/terminal/session.ts` 的 `TermSession` 封装（xterm.js + FitAddon + WebglAddon），`fontFamily` 硬编码为 `"JetBrains Mono", "SF Mono", ui-monospace, Menlo, Consolas, monospace`，`fontSize` 硬编码 13。该字体栈不含任何 CJK 字形，xterm.js（含 WebGL 渲染路径）对缺失字形退化为 `_`/方块，远端浏览器打开终端时中文不可读。本地 iTerm2 显示正常是因为终端模拟器有系统级字体回退（macOS 自动落到 PingFang SC）。

现有设置入口：任务工作台「设置」tab（`TaskWorkbenchPage.tsx`），目前只有「任务级环境变量」一节（走服务端 API）。终端外观是浏览器端 UI 偏好，与服务端无关。

已核实的事实（oracle review 抽查确认）：
- xterm 5.5 `term.options` setter 支持就地修改 `fontFamily`/`fontSize`，RenderService 会清理渲染器、重新测量并全量刷新（`web/node_modules/@xterm/xterm/typings/xterm.d.ts:834-867`）。**无需重建终端实例**。
- 重建实例会丢失浏览器侧 scrollback/selection/focus，且 WS 重连会进入服务端单交互客户端替换链路（4009 竞态），本项目明确不做输出缓冲回放——故重建方案被否决。
- `web/package.json` scripts 仅有 `dev / build(tsc --noEmit && vite build) / preview`，无 lint script。

## Goals / Non-Goals

**Goals:**
- 默认字体栈追加 CJK 系统字体回退，中文在装有常见 CJK 字体的浏览器环境（macOS / Windows / 装好 Noto CJK 的 Linux）正常渲染。
- 用户可在「全局配置」页自定义终端 fontFamily 与 fontSize，偏好存 localStorage，对当前浏览器（含同源其他标签页）内所有任务的 TUI 与 shell 终端生效。
- 偏好变更就地应用于已打开终端，**不断开 WS、不重建终端实例**。

**Non-Goals:**
- 不内置 web font（CJK 字体 MB 级体积）；裸 Linux 无 CJK 字体环境不在保证范围内。
- 不新增后端 API / 服务端持久化，不做跨浏览器、跨设备同步。
- 不开放其他 xterm 选项（lineHeight、theme、cursorBlink 等保持现状）。
- 不改动 WS 协议、PTY 桥接、重连等现有终端逻辑；不引入通用设置框架或事件总线。

## Decisions

### D0：后端 UTF-8 locale 默认值（实证根因，优先级高于字体栈）

实证链（2026-08-03 在生产实例验证）：
- launchd 启动的 ocdeck-server 环境无 LANG/LC_*（`ps eww <pid>` 确认）。
- `DefaultBaseEnv`（internal/process/process.go:99）与 task 侧 `envBaselineKeys`（internal/task/activate.go:23）只透传宿主已有的 LANG → tmux 命令与任务进程均无 locale。
- 生产 TUI attach 客户端 `#{client_utf8}` = **0**（tmux 按客户端 locale 判定 UTF-8 能力）→ tmux 将网格中完好的 UTF-8 中文转写为 `_` 输出（capture-pane 验证网格内容正常）。
- 结论：前端字体修复只解决"字形渲染"，字节流层的 `_` 必须由本决策解决。

决策：
- `process.DefaultBaseEnv` 与 `task` 侧 env 快照（mergeEnvSnapshot 基础集）统一规则：
  1. **透传**：`LANG/LC_ALL/LC_CTYPE` 均加入基础集白名单，宿主非空值原样透传（oracle 复核修订：初版只把 LC_ALL/LC_CTYPE 当抑制信号不透传——`cmd.Env` 是全量替换，宿主仅设 LC_ALL 时子进程三个 locale 全缺，抑制失效，等同未尊重显式配置）。
  2. **兜底注入**：三者均未设置**或为空串**（空串视为未设置）时，注入 `LANG=en_US.UTF-8`。
  3. 宿主显式设置的非空 locale（哪怕非 UTF-8，如 LANG=C）原样尊重，不做"纠正"——显式配置优先。
- 规范归属：基础集白名单变更是 env-management capability 的行为变更，delta 见 specs/env-management/spec.md。
- 备选 A（强制 `tmux -u`）：-u 无视客户端 locale 强制 UTF-8，能修复 attach 层但不修会话内进程的 locale（shell 里 `locale` 仍为空、部分 CLI 仍按 POSIX 处理），且改变 tmux 全局调用面，否决。
- 备选 B（要求用户在 launchd plist 配 LANG）：把运维负担转嫁给部署方，否决。
- 生效范围：attach 客户端重连（浏览器刷新/重连即触发新 attach）即获得新 locale；既有会话内**已运行进程**的环境在启动时固化，需重新激活任务或新建会话才获得新 locale；既有 tmux server 无需重建。

### D1：默认字体栈追加 CJK 回退（而非内置 web font）

```
'"JetBrains Mono", "SF Mono", ui-monospace, Menlo, Consolas, "PingFang SC", "Sarasa Mono SC", "Noto Sans Mono CJK SC", "Microsoft YaHei", monospace'
```

- 备选 A（内置 Noto Sans Mono CJK web font）：任意环境确定性渲染，但 MB 级体积 + 子集化/加载策略成本高，否决。
- 备选 B（仅追加回退链）：零依赖；macOS（PingFang SC）/ Windows（雅黑）/ 主流 Linux 桌面（Noto CJK）均覆盖。选 B。
- 回退顺序：等宽拉丁字体在前（保证 ASCII 度量一致），CJK 字体只命中中文字形，不影响英文渲染与列宽度量。

### D2：偏好模块 `web/src/terminal/preferences.ts` 拥有全部契约

`session.ts` 与设置组件都从 `preferences.ts` 导入默认值与类型，**避免设置组件反向依赖含 xterm/WS 生命周期的 session.ts**。

精确契约（不得猜测）：

```ts
export interface TermPreferences {
  fontFamily?: string;  // trim 后非空才存在
  fontSize?: number;    // 整数，8–32
}
export const DEFAULT_FONT_FAMILY = '"JetBrains Mono", "SF Mono", ui-monospace, Menlo, Consolas, "PingFang SC", "Sarasa Mono SC", "Noto Sans Mono CJK SC", "Microsoft YaHei", monospace';
export const DEFAULT_FONT_SIZE = 13;
export const FONT_FAMILY_KEY = 'ocdeck.terminal.fontFamily';
export const FONT_SIZE_KEY  = 'ocdeck.terminal.fontSize';
export const TERM_PREFS_CHANGED = 'ocdeck-term-prefs-changed'; // window CustomEvent 名

export function loadTermPrefs(): TermPreferences;
export function resolveFontFamily(p: TermPreferences): string; // p.fontFamily ?? DEFAULT_FONT_FAMILY
export function resolveFontSize(p: TermPreferences): number;   // p.fontSize ?? DEFAULT_FONT_SIZE
export function validateFontSize(v: string): number | null;    // Number.isInteger && 8<=v<=32，否则 null；禁止 parseInt（会接受 "13px"）
export function saveTermPrefs(p: TermPreferences): void;       // 见副作用边界
export function clearTermPrefs(): void;                        // removeItem 两个 key
```

副作用边界：
- **先完整校验、后写存储**：`saveTermPrefs` 只在两个字段都通过校验后才触碰 localStorage；任一非法则整体不写入（不存在 fontFamily 已写而 fontSize 失败的部分更新）。
- **fontFamily trim 后为空** → 删除 `FONT_FAMILY_KEY`（回到默认栈），仍允许单独保存合法 fontSize。
- **读取不修复**：`loadTermPrefs` 遇到损坏/非法数据只回退默认值，MUST NOT 顺手改写 localStorage。
- **存储异常**：`getItem/setItem/removeItem` 可能抛 `SecurityError`/quota 错误。保存失败时 MUST NOT 派发 `TERM_PREFS_CHANGED`，由设置组件 catch 后显示错误、保留运行中终端现状；读取失败按无偏好处理（默认值兜底，终端功能可用）。

### D3：偏好变更就地应用——`TermSession.applyPreferences()`，不重建、不重连

xterm 5.5 `term.options` setter 原生支持修改 `fontFamily/fontSize`（RenderService 自动清理渲染器、重新测量、全量刷新，WebGL 路径同样适用）。

变更生效主流程（同页与跨标签页是同一主流程的两个触发源）：

```
设置组件 save/clear
  → 校验通过且 localStorage 写入成功
  → window.dispatchEvent(new CustomEvent(TERM_PREFS_CHANGED))   [同页触发源]
                                                       │
其他标签页 localStorage 写入 ─→ window 'storage' 事件   [跨标签页触发源]
                                                       │
        TerminalView 监听两个事件 ─────────────────────┘
          → loadTermPrefs() 读取 resolved prefs
          → session.applyPreferences(prefs)
              term.options.fontFamily / fontSize 就地更新
              下一帧 requestAnimationFrame → fitNow() + 同步 winsize
```

- 每个 `TerminalView` 自行监听并应用，同页所有 TUI/shell 终端实例同时生效；`storage` 事件天然只在**其他**标签页触发，与同页 CustomEvent 互补、无重复应用问题（重复应用亦幂等）。
- 隐藏（inactive）终端实例同样就地更新字体；其 fit 由现有 `active` effect 在下次激活时执行，无需特殊分支。
- 被否决的备选：dispose/new 重建实例——丢 scrollback/selection/focus、触发 WS 重连与 4009 替换竞态（见 Context 已核实事实）。

### D4：设置 UI——「全局配置」页新增「终端外观」节

- 位置：`ConfigsPage.tsx`（项目列表页「全局配置」入口进入），在「全局环境变量」节之上新增一节，组件 `web/src/components/TermAppearanceEditor.tsx`。
- 决策依据（人工 review 反馈修订）：终端外观是 localStorage 全局偏好，与任务无关；放任务工作台「设置」tab 会造成"任务级配置"的语义误导，故放全局配置页。原方案（工作台设置 tab）已否决。
- 表单语义：
  - 初始值：`loadTermPrefs()` 的已存值；未设置的字段留空，placeholder 显示当前生效的默认栈 / `13`。
  - 保存：两字段整体校验（D2）；成功后派发事件并将表单值归一为"已存值"显示；存储异常显示错误且不生效。
  - fontSize 输入框留空 = 未设置（恢复该项默认）。
  - 恢复默认：`clearTermPrefs()` + 派发事件 + 表单两字段清空。
- 提示文案：自定义字体栈需自行包含 CJK 字体（如 PingFang SC / 更纱黑体），否则中文无法显示。

### D5：宽字符度量

xterm.js 按 Unicode East Asian Width 处理宽字符（中文占 2 列），与 tmux/PTY 侧一致，本变更只解决字形缺失，不动度量逻辑。

## Risks / Trade-offs

- [裸 Linux 桌面无 CJK 字体仍显示异常] → 设置节提示安装 Noto Sans CJK 或指定已装 CJK 字体；本方案明确不保此场景。
- [用户自定义字体栈不含 CJK 字体，中文又变 `_`] → 提示文案 + 「恢复默认」一键回退。
- [两个 localStorage key 无事务性] → 先完整校验后写（D2）；写中途崩溃只影响偏好数据，读取侧损坏回退默认，无级联影响。
- [localStorage 被清空/不可用] → 默认栈已含 CJK 回退，读取异常按无偏好处理，无回归。
- [同源多标签页偏好瞬时一致依赖 storage 事件] → 为浏览器标准行为；极端情况下（事件被扩展拦截）刷新页面即可收敛，可接受。
