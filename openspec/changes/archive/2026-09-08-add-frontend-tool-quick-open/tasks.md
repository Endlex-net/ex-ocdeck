# Tasks: add-frontend-tool-quick-open

## 1. 偏好存储模块（design D1）

- [x] 1.1 新建 `web/src/editor-tools.ts`：常量（`VSCODE_KEY`/`GOLAND_KEY`/`DEFAULT_KEY`/`EDITOR_TOOLS_CHANGED`）+ `loadEditorTools`/`saveEditorTool`（开写 `'1'` 关写 `'0'`，成功才派发事件，失败上抛不派发，不触碰 `DEFAULT_KEY`）+ `loadDefaultTool`（非法值/异常返回 null 不改写）/`saveDefaultTool`（DEFAULT_KEY 唯一写入口）+ `resolveDefaultTool`（stored 合法且已启用→stored，否则 vscode→goland 首个已启用，无则 null）+ `buildEditorUri`（公共归一化仅反斜杠转 `/`；VSCode 分段编码+盘符冒号保留+去开头 `/`+尾斜杠收敛恰好一个；GoLand 整体一次 encodeURIComponent；空路径返回空串）
- [x] 1.2 新建 `web/src/__tests__/editor-tools.test.ts`：覆盖 spec 全部存储与 URI 场景——缺省关闭、开启持久化、关闭写 `'0'` 且不动默认键、损坏数据回退不改写、默认工具持久化、存储默认已关闭回退、关闭后重开恢复显式选择、默认键读取异常回退、**开关写入失败向上抛且不派发变更事件**、URI 构造（含空格、已有尾斜杠、多重尾斜杠收敛、Windows 反斜杠归一、含百分号单次编码、空路径返回空串）

- [x] 1.3 模板读写与模板分支：`editor-tools.ts` 增加 `VSCODE_URI_TEMPLATE_KEY`/`GOLAND_URI_TEMPLATE_KEY` 常量、`loadEditorUriTemplate`（缺省/空/异常返回 null 不改写）、`saveEditorUriTemplate`（非空写原文、空串 removeItem 清除，成功才派发失败上抛）、模板合法性判定（含 `{path}` 且 scheme 前缀属于该工具族：vscode 允许 `vscode://`/`vscode-insiders://`，goland 允许 `goland://`）、`buildEditorUri` 第三参 template（合法→全部 `{path}` 替换为分段编码注入值[保留开头 `/`、无尾斜杠收敛、不再二次编码]；非法/缺省→内置分支）+ 测试覆盖 spec「自定义唤起 URI 模板」全部场景

## 2. 路由与设置页子标签（design D5/D6）

- [x] 2.1 `web/src/router.ts`：`ConfigsTab` 与 `CONFIGS_TABS` 增加 `'tools'`（未知 tab 回退 appearance 既有逻辑自动覆盖，不改动）
- [x] 2.2 `web/src/icons.tsx`：新增 VSCode/GoLand 内联 SVG 图标（沿用既有图标模式）
- [x] 2.3 `web/src/pages/SettingsPage.tsx`：TABS 增加 `{ key: 'tools', label: '常用工具' }`（置于「命令面板」之后）+ 新增 `EditorToolsPanel` 组件（两个开关行，od-field + checkbox 形态，每行 hint 说明「需本机已安装、使用本机路径打开」；变更走 `saveEditorTool`，面板监听 `EDITOR_TOOLS_CHANGED` + `storage` 收敛，同 ClipboardPolicyField 模式）
- [x] 2.4 测试：`#/configs#tools` 深链选中「常用工具」子标签；**未知设置 tab 回退 appearance**（web-ui-shell delta 沿用场景，可并入既有路由测试文件如 App.palette-config.test.tsx 同风格）；开关点击写入 localStorage 并派发事件；**开关写失败面板不视为生效（UI 不翻转）**
  - 落点：`resolveRoute('/configs#tools')` 与 `isConfigsTab('tools')` 断言并入既有路由测试 `web/src/__tests__/shell-contracts.test.ts`（未知 tab 回退 appearance 用例该文件已有）；SettingsPage(tools) 面板渲染、开关写入/派发/写失败不翻转见新文件 `web/src/__tests__/editor-tools-settings.test.tsx`。

- [x] 2.5 设置面板模板配置：`EditorToolsPanel` 每个工具行增加可选「自定义唤起 URI 模板」文本输入（placeholder 显示该工具内置默认模板、失焦保存、空值保存=清除恢复内置，走 `saveEditorUriTemplate`）+ 能力说明 hint（spec 原文：VSCode 可配置 `vscode://vscode-remote/ssh-remote+<主机>{path}` 打开远程开发机目录，需本机已安装 Remote-SSH 扩展且 SSH 可达，该形式为社区实测非官方文档化；GoLand 不存在通过 URL 打开远程项目的可用形式）+ 测试（保存/清除/写失败不生效/事件派发/hint 呈现）

## 3. 任务详情页快捷打开入口（design D4）

- [x] 3.1 新建 `web/src/components/OpenInEditorMenu.tsx`：「默认工具图标主按钮 + ⌄ 下拉」组合；数据源 `loadEditorTools()`+`loadDefaultTool()`，监听 `EDITOR_TOOLS_CHANGED`+`storage` 同时重读两类键再 `resolveDefaultTool`；无已启用工具返回 null；`worktree_path` 空/缺失时保留组合但双按钮禁用；下拉仅列已启用工具、默认带 ✓；disclosure 模式（Escape 关闭、backdrop/失焦关闭、打开聚焦首项，参照 WorkbenchOverflow）
- [x] 3.2 点击主流程（spec 执行顺序）：① 校验工具仍启用+路径非空+URI 非空（失败零副作用、菜单关闭）→ ② 仅下拉：`saveDefaultTool`（失败捕获保留原默认不唤起）→ ③ 同一手势调用链 `location.assign(uri)`（同步异常捕获、页面可用、已保存默认不回滚、菜单关闭）；主按钮执行 ①→③
- [x] 3.3 `web/src/pages/TaskWorkbenchPage.tsx`：页头 `header-spacer` 之后、TaskActions 之前渲染 `OpenInEditorMenu`，宽屏与窄屏（isNarrow）两处均接入
- [x] 3.4 测试 `web/src/__tests__/open-in-editor-menu.test.tsx`：入口显隐（双关隐藏/单开呈现）、主按钮触发正确 URI、下拉选择即打开并写默认键（✓ 跟随）、默认键写失败不唤起保留原默认、**`location.assign` 同步抛异常页面可用且已保存默认不回滚、菜单关闭**、空路径禁用、**EDITOR_TOOLS_CHANGED 自定义事件与 storage 事件分别覆盖开关与默认键两类变更的收敛**、**Escape 与外部点击关闭菜单**、主按钮前置检查失败零副作用
  - 评审修正（Round 1）：前置检查失败零副作用改由「点击重读存储（底层关闭未派发事件 → 主按钮/下拉项均零副作用）」与「URI 构造同步异常（孤立代理对）→ 零副作用、②不执行」两例真实覆盖；空路径用例定位修正为仅验证禁用态呈现（disabled 按钮不进入处理器）；EDITOR_TOOLS_CHANGED 默认键收敛用例改为两工具均启用、默认 VSCode → 写 GoLand，断言主按钮与 ✓ 实际变化。

- [x] 3.5 入口接入模板：`OpenInEditorMenu` 第 ① 步同时读取目标工具的自定义模板传入 `buildEditorUri`（模板变更经同一 `EDITOR_TOOLS_CHANGED`/`storage` 通道收敛，点击时读取最新值）+ 测试（自定义模板生效、非法模板回退内置、模板修改后下次点击即时生效）

## 4. 验证

- [x] 4.1 `pnpm --dir web test` 全量测试通过（首次 57 文件 990 测试全绿；模板增量+评审修复后最终 58 文件 1012 测试全绿，orchestrator 多轮独立复跑确认）
- [x] 4.2 `pnpm --dir web build`（tsc --noEmit + vite build）通过
- [x] 4.3 新增行为测试有效性自检：对关键新增测试（URI 构造、入口显隐、下拉写默认）做等价验证——临时改动被测实现（如注释掉尾斜杠收敛或显隐判断）确认对应测试变红，恢复后变绿；在 tasks 完成记录中说明每个行为测试的证据
  - 证据 1（URI 构造）：移除 `buildEditorUri` 的尾斜杠收敛 → `editor-tools.test.ts` 3 例变红（含空格尾斜杠/多重尾斜杠收敛/Windows 盘符），恢复后全绿。
  - 证据 2（入口显隐）：移除 `OpenInEditorMenu` 双关返回 null 判断 → `open-in-editor-menu.test.tsx` 3 例变红（双关隐藏、EDITOR_TOOLS_CHANGED 开关显隐收敛、storage 开关显隐收敛），恢复后全绿。
  - 证据 3（下拉写默认）：移除下拉分支 `saveDefaultTool` 写入 → `open-in-editor-menu.test.tsx` 3 例变红（下拉写默认 ✓ 跟随、默认键写失败保留原默认、唤起异常默认不回滚），恢复后全绿。
- [x] 4.4 `openspec validate add-frontend-tool-quick-open` 通过
- [x] 4.5 GoLand 本机唤起兼容性手工验证（design Risks 要求）：在已安装 GoLand 的本机启用开关，对具有有效本机 `worktree_path` 的任务点击快捷入口，检查 GoLand 是否打开目标项目；完成记录注明 OS、浏览器、GoLand 版本、操作与实际结果；环境不具备时明确标记未完成及原因。仅记录兼容性实测，不增加协议检测或失败回退 UI —— **完成记录：用户于 2026-09-08 人工 review gate 确认已实测通过（goland:// 唤起成功打开目标项目）**
- [x] 4.6 模板增量验证：`pnpm --dir web test` / `build` 复跑通过；模板分支 mutation 自检（如去掉 `{path}` 合法性校验或注入编码步骤，确认对应测试变红，恢复后变绿）；`openspec validate add-frontend-tool-quick-open` 复跑通过
  - 证据 1（`{path}` 合法性校验）：`isValidUriTemplate` 去掉 `includes('{path}')` → 3 例变红（合法性判定、模块级缺占位符回退、菜单级缺占位符回退），恢复后全绿。
  - 证据 2（注入编码步骤）：`encodeTemplatePath` 改为返回归一化原文（去掉分段编码）→ 4 例变红（模板生效本地算例、保留开头 `/`/全部 `{path}` 替换、盘符冒号/空格分段编码、菜单级自定义模板生效），恢复后全绿。
