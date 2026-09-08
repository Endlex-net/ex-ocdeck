# Design: add-frontend-tool-quick-open

## Context

用户经常用 GoLand / VSCode review 代码，当前要先 pwd 拿到任务工作目录再手动打开编辑器。由于服务端可能运行在远程机器上，"在编辑器中打开"必须是纯前端行为——由本机浏览器通过 URL scheme 唤起本机编辑器。

现状相关代码：

- 设置页子标签体系：`web/src/pages/SettingsPage.tsx`（TABS 数组 + ConfigsTab 深链），`web/src/router.ts:49-62`（ConfigsTab 类型与 isConfigsTab 白名单，未知 tab 回退 appearance）。
- 前端偏好存储既有模式：`web/src/terminal/preferences.ts`（`ocdeck.*` localStorage 键、读取侧损坏只回退默认不改写、写入失败向上抛、`TERM_PREFS_CHANGED` CustomEvent 派发）；`ClipboardPolicyField`（SettingsPage.tsx:174-222）演示了开关 + storage 事件跨标签页收敛的完整模式。
- 任务详情页页头：`web/src/pages/TaskWorkbenchPage.tsx:319-428`（header 结构、窄屏 `isNarrow` 分支、`WorkbenchOverflow` disclosure 菜单模式）。
- 打开目标字段：`Task.worktree_path`（`web/src/types.ts:97`）。已核实 dir / repo local-path 任务落库 `worktree_path` = canonical 项目路径（`internal/task/crud.go:172`、`:360-364`），前端统一取该字段、不按模式分叉。

外部契约核实（@librarian，2026-09-08）：

- VSCode 官方文档化：`vscode://file/<path>/` 打开文件夹（目录带尾斜杠；Windows `c:/myProject/` 形式）。来源：https://code.visualstudio.com/docs/editor/command-line 「Opening VS Code with URLs」。
- GoLand：JetBrains 官方**未文档化**任何 URL scheme（官方仅文档化命令行 `goland <path>`）；社区通用格式 `<产品>://open?file=<path>`，目录语义 = 作为项目打开（YouTrack IDEA-204266：「`open "idea://open?file=/foo"` creates a new project in `/foo`」，工单至今仍 open）。纯前端约束排除了命令行方案，故采用 `goland://open?file=<path>`。证据强度弱于 VSCode（未见 GoLand 专属的协议注册文档），需以本机实测为准。

## Goals / Non-Goals

**Goals:**

- 设置页新增「常用工具」子标签（`tools`），VSCode/GoLand 独立开关、默认关闭、localStorage 持久化。
- 任务详情页页头提供「图标主按钮 + ⌄ 下拉」快捷打开入口，默认工具记忆，按 spec `editor-quick-open` 的交互语义实现。
- 每个工具支持可选的自定义唤起 URI 模板：默认使用内置本地模板（行为不变），远程开发机等场景由用户自行配置模板（用户 2026-09-08 决策：「仅仅支持自定义即可，默认是本地模式」）。
- 零服务端改动。

**Non-Goals:**

- 不支持 VSCode/GoLand 以外的工具（Finder、终端、其他编辑器）。
- 不在指挥中心/项目管理页提供快捷打开。
- 「常用工具」子标签不进 ⌘K 命令面板静态入口（与通知子标签一致）。
- 不检测协议未注册/唤起失败（浏览器无可靠 API），不做失败回退 UI。

## Decisions

### D1: 偏好存储模块 `web/src/editor-tools.ts`（新文件）

独立小模块承载全部存储与 URI 构造逻辑，保持纯函数可测（参照 `terminal/preferences.ts` 模式）：

- 常量：`VSCODE_KEY = 'ocdeck.editorTools.vscode'`、`GOLAND_KEY = 'ocdeck.editorTools.goland'`、`DEFAULT_KEY = 'ocdeck.editorTools.defaultTool'`、变更事件 `EDITOR_TOOLS_CHANGED = 'ocdeck-editor-tools-changed'`（window CustomEvent）。
- `loadEditorTools(): { vscode: boolean; goland: boolean }` —— 仅 `'1'` 为开，其余/缺省为关；localStorage 不可用（getItem 抛异常）时按全关处理；损坏数据只回退不改写。
- `saveEditorTool(tool, enabled)` —— 开启写 `'1'`、关闭写 `'0'`；setItem 成功才派发 `EDITOR_TOOLS_CHANGED`；失败向上抛、不派发。只写对应开关键，MUST NOT 触碰 `DEFAULT_KEY`（默认回退是派生状态，不落库）。
- `loadDefaultTool(): EditorTool | null` —— `'vscode'|'goland'` 合法，其余返回 null 不改写；getItem 抛异常时返回 null（按无记录处理），MUST NOT 抛至渲染层。
- `saveDefaultTool(tool)` —— 唯一写 `DEFAULT_KEY` 的入口（仅由下拉显式选择调用）；同事务语义：setItem 成功才派发 `EDITOR_TOOLS_CHANGED`，失败向上抛。
- `resolveDefaultTool(tools, stored): EditorTool | null` —— 纯函数：stored 合法且已启用 → stored；否则固定顺序 vscode → goland 取第一个已启用；无已启用返回 null。
- 模板键常量：`VSCODE_URI_TEMPLATE_KEY = 'ocdeck.editorTools.uriTemplate.vscode'`、`GOLAND_URI_TEMPLATE_KEY = 'ocdeck.editorTools.uriTemplate.goland'`。
- `loadEditorUriTemplate(tool): string | null` —— 缺省/空串/读取异常返回 null（按未设置处理），MUST NOT 改写 localStorage。
- `saveEditorUriTemplate(tool, template)` —— 非空写原文；空串清除对应键（removeItem）；setItem/removeItem 成功才派发 `EDITOR_TOOLS_CHANGED`，失败向上抛、不派发。
- `buildEditorUri(tool, path, template?): string` —— 纯函数：`template` 合法（spec「自定义唤起 URI 模板」合法性判定）时走模板替换分支（见 D7），否则走内置分支（见 D3）；path 为空返回空串。

理由：设置页与任务详情页两处消费同一份偏好，收敛到单一模块避免双份解析逻辑漂移；纯函数便于无 jsdom 的单元测试（项目既有测试风格）。

### D2: 唤起机制 —— 用户手势内顶层 `location.assign(uri)`

点击打开时在点击处理器调用链内对顶层页面执行 `location.assign(uri)`（自定义 scheme）。

备选：隐藏 iframe。拒绝理由：实现更复杂且无实际收益；`location.assign` 在用户点击手势内是社区标准做法。

行为边界（应用只能保证自身部分）：应用保证不调用服务端、捕获同步异常；浏览器是否弹确认框、是否拦截、协议处理器是否注册及是否实际打开编辑器，均不受应用控制且无法可靠检测（Non-Goal）。

### D3: URI 编码实现（与 spec「编辑器唤起 URI 构造」逐字同一契约）

`buildEditorUri` 内部：

1. 公共归一化：仅 `path.replace(/\\/g, '/')`，得到 `normalized`。
2. VSCode 分支：`normalized` 按 `/` 分段，每段 `encodeURIComponent`，仅 Windows 盘符段（首段匹配 `^[A-Za-z]:$`）的冒号保留字面，以 `/` 重新连接；去掉开头的 `/` 后拼到 `vscode://file/` 前缀；随后将结果路径的尾部 `/` 收敛为恰好一个（`normalized` 已带一个或多个尾斜杠时连接结果天然以 `/` 结尾，MUST NOT 再追加，多余尾斜杠收敛为一个；该收敛仅属于 VSCode 分支）。
3. GoLand 分支：`` `goland://open?file=${encodeURIComponent(normalized)}` ``——对归一化路径整体一次编码，MUST NOT 复用 VSCode 分段编码结果，MUST NOT 二次编码。

空/缺失 `worktree_path`：`buildEditorUri` 返回空串，调用侧不触发唤起。

### D7: 自定义唤起 URI 模板（可选，默认内置本地模板）

模板契约的 normative 文本在 spec「自定义唤起 URI 模板」（合法性判定、模板路径注入值、替换语义、写入语义），此处仅记录机制与理由：

- 模板路径注入值复用 VSCode 分段编码逻辑（`normalized` 按 `/` 分段逐段编码、盘符首段冒号保留），与内置 VSCode 分支的差异仅在首尾斜杠处理：注入值**保留开头 `/`、不做尾斜杠收敛**——内置分支要去头补尾是因为 `vscode://file/` 前缀形态固定；模板模式下前后文由用户自行书写（如 `vscode://file{path}/`、`goland://open?file={path}`、`vscode://vscode-remote/ssh-remote+<主机>{path}` 三种写法天然兼容同一注入值）。
- 模板有效时 URI = 模板中全部 `{path}` 出现替换为注入值后的字符串，MUST NOT 对替换结果再做任何编码。
- 合法性判定（spec 原文）：模板包含至少一个 `{path}` 占位符，且以该工具允许的 scheme 前缀开头（VSCode 工具允许 `vscode://` 与 `vscode-insiders://`；GoLand 工具允许 `goland://`）。scheme 白名单同时是安全边界：防止 `javascript:` 等危险 scheme 经 `location.assign` 执行。
- 动机：GoLand scheme 未文档化、远程开发机（VSCode Remote-SSH）有真实需求；模板化让用户自行覆盖 scheme 变体与远程形式，不扩大内置工具集，也不内置远程主机/SSH 连接管理（Non-Goal）。

远程形式外部契约核实（@librarian，2026-09-08）：

- VSCode Remote-SSH：`vscode://vscode-remote/ssh-remote+[user@]host[:port]/<path>`。证据：VSCode 源码常量 `vscodeRemote = 'vscode-remote'`（`src/vs/base/common/network.ts`）+ 多个 GitHub issue 实测确认（microsoft/vscode#128309/#215496 等）；**官方文档未记载**；路径编码与本地 `file` 形式一致，authority 中 `+` 为字面分隔符不编码。前置条件：本机安装 Remote-SSH 扩展且 SSH 配置可达。
- JetBrains 侧：**不存在**一步打开远程项目的 URL 机制——`jetbrains://gateway/ssh/environment` 仅能创建/跳转 SSH 环境，指定项目路径的能力请求 TBX-14811 尚未解决；JetBrains Client 无 URL handler（GTW-9500 实测 Info.plist）。GoLand 行 hint 据此明示不支持远程。

### D4: 页头入口组件 `OpenInEditorMenu`（`web/src/components/OpenInEditorMenu.tsx`）

- 位置：页头 `header-spacer` 之后、TaskActions 之前；宽屏与窄屏（isNarrow）两处均渲染（图标形态天然紧凑，不收进「⋯」溢出菜单——打开是高频主操作，与参考项目截图一致）。
- 结构：`[工具图标按钮][⌄ 按钮]` 组合，复用 `WorkbenchOverflow` 的 disclosure 模式（Escape 关闭、backdrop/失焦关闭、打开聚焦首项）。
- 数据源与收敛：`useState(() => loadEditorTools())` + `loadDefaultTool()`；监听 `EDITOR_TOOLS_CHANGED` 与 `storage` 事件，回调内**同时重读开关与存储默认值**再经 `resolveDefaultTool` 派生当前默认（跨标签页即时生效，覆盖开关与默认值两类变更）。
- 无已启用工具 → 返回 null（完全隐藏无占位）。
- 下拉项：仅列已启用工具，默认工具带 ✓。
- 点击主流程（spec「任务详情页快捷打开入口」执行顺序的落点）：主按钮执行 ①→③；下拉选择执行 ①→②→③。
  1. 校验目标工具仍已启用、`worktree_path` 非空、`buildEditorUri` 非空串；任一失败 → 不写存储、不派发事件、不唤起、不改默认，菜单关闭。
  2. （仅下拉选择）`saveDefaultTool(tool)`；抛异常 → 组件捕获，保留原默认与 ✓，不唤起，菜单关闭。
  3. 使用第 ① 步构造的 URI 调用 `location.assign(uri)`（同一手势调用链内）；同步异常组件捕获、页面可用、已保存默认不回滚；菜单关闭。
- `worktree_path` 为空/缺失：组合入口保留但两个按钮禁用。
- 图标：新增 VSCode/GoLand 简笔图标到 `web/src/icons.tsx`（现有图标均为内联 SVG，沿用该模式）。

### D5: 设置页「常用工具」子标签

- `router.ts`：ConfigsTab 与 CONFIGS_TABS 增加 `'tools'`。
- `SettingsPage.tsx`：TABS 增加 `{ key: 'tools', label: '常用工具' }`（置于「命令面板」之后）；新增 `EditorToolsPanel` 组件——两个开关行（VSCode / GoLand），复用 `od-field` + checkbox 形态（与移动端子开关一致），每行附 hint（如「在任务详情页页头显示 VSCode 快捷打开；需本机已安装 VSCode，打开使用本机路径」）。
- 每个工具行下附可选「自定义唤起 URI 模板」文本输入：placeholder 显示该工具内置默认模板，失焦保存，空值保存=清除恢复内置；保存走 `saveEditorUriTemplate`（事务语义同开关）。
- 模板输入处附能力说明 hint（spec「自定义唤起 URI 模板」末段文案）：VSCode 可配置 `vscode://vscode-remote/ssh-remote+<主机>{path}` 形式打开远程开发机目录（需本机已安装 Remote-SSH 扩展且 SSH 可达；该形式为社区实测、非官方文档化）；GoLand 不存在通过 URL 打开远程项目的可用形式。
- 开关变更走 `saveEditorTool`（事务语义），面板自身监听 `EDITOR_TOOLS_CHANGED` + `storage` 收敛外部变更（同 ClipboardPolicyField 模式）。

### D6: 路由与面板联动

`resolveRoute` 的未知 tab 回退逻辑（`isConfigsTab` 白名单）自动覆盖新值，无需改动；web-ui-shell delta 已将 `#tools` 纳入深链枚举。

## Risks / Trade-offs

- [GoLand URL scheme 非官方文档化（证据为 YouTrack IDEA-204266 社区格式，未见 GoLand 专属注册文档），行为可能随版本变化或特定安装方式下未注册] → 设置页 hint 文案说明「需本机已安装」；实现后在本机实测 GoLand 唤起作为兼容性验证；用户可关闭 GoLand 开关。
- [浏览器拦截自定义 scheme（企业策略/首次确认弹窗）或协议未注册] → 不受应用控制且无法可靠检测（D2 行为边界）；不做检测与提示（Non-Goal），设置页 hint 说明「使用本机路径打开」避免远程路径误解。
- [`location.assign` 对未注册 scheme 的实际表现因浏览器/OS 而异（静默、原生提示或极少见错误页）] → 应用侧捕获同步异常、不做跨页面状态依赖；该行为本身属于浏览器/OS 边界，见 D2。
- [跨标签页开关/默认值状态不一致] → `storage` 事件 + `EDITOR_TOOLS_CHANGED` 双通道收敛（D4/D5），且入口同时重读开关与默认值两类键，与剪贴板策略既有模式一致。
- [VSCode Remote-SSH URL 形式非官方文档化（证据为源码常量 + GitHub issue 实测），行为可能随版本变化] → 仅作为用户自定义模板的能力说明提供、不内置；hint 注明前置条件与证据强度。
- [用户配置的模板非法（无 `{path}` 或 scheme 不符）导致唤起 URI 异常] → 合法性判定不满足即按未设置回退内置分支，不阻断唤起、不改写存储（spec「自定义唤起 URI 模板」）。

## Open Questions

无。
