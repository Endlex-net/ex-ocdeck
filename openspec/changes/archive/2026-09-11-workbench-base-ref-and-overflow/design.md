# Design: workbench-base-ref-and-overflow

## Context

见 proposal.md - Why。现状关键事实（均已核实代码原文）：

- `base_ref` 数据已存在：读模型 `application.TaskRow.BaseRef`（`internal/application/dto.go:72`）。新建 worktree 任务在创建时落库——`createRepo`（worktree）经 `resolveRepoBaseRef` 解析为全限定 ref（空输入缺省 `refs/heads/<defaultBranch>`；非空经校验解析为 `refs/heads/<name>` 或 `refs/remotes/<name>`，`internal/task/crud.go:429-435`）；`createInPlace`（local-path/dir）恒落库空串且拒绝非空 base_ref（`internal/task/crud.go:351-378`）。存量任务的非空 `base_ref` 不一定来自创建时记录：migration 0008 曾按迁移时的项目 `default_branch` 回填存量任务（`internal/infrastructure/store/migrations/0008_project_kind.sql:9`），本期不重新推断或修正历史值，只原样透传。
- 详情 DTO `taskRowDTO`（`internal/api/tasks.go:523-544`）与组装函数 `toTaskDTO`（`:623-654`）不透出 `BaseRef`；SSE 详情流复用同一组装逻辑，改一处即双端生效。
- 前端 `Task` 类型（`web/src/types.ts:103-133`）无 `base_ref`；页头分支展示在 `TaskWorkbenchPage.tsx:420`；溢出菜单 `WorkbenchOverflow`（`:38-135`）恒渲染按钮、菜单项条件渲染（init 日志项仅 `init_status==='failed'`，删除项仅 `status!=='active'`），无空态隐藏；两处 call site（宽屏 `:444`、窄屏 `:457`）共用同一组件。
- 仓库空态先例：`OpenInEditorMenu` 无可用工具时组件级 `return null`（`web/src/components/OpenInEditorMenu.tsx:94`）；删除按钮活跃态直接隐藏（`WorkbenchOverflow` `:117-118`，design D9）。
- 后端存在未导出的短名转换 `baseBranchShortName`（定义于 `internal/task/activate.go:91`，`:126` 为调用点），仅供激活流程内部使用。
- 页头分支信息区 `.header-meta` 在 ≤1024px 断点整段 `display:none`（`web/src/legacy-components.css:1809`），窄屏本就不展示分支。

## Goals / Non-Goals

**Goals:**

- 任务详情对象（REST + SSE）透出 `base_ref`，前端工作台页头展示来源分支。
- 溢出菜单无可见菜单项时整个入口不渲染。
- 两处改动均抽取可单测的纯逻辑，符合仓库"纯逻辑 helper 入 .ts"惯例。

**Non-Goals:**

- 列表/侧栏不展示来源分支；不新建独立详情页；不往页头加其他元信息（见 proposal Non-goals）。
- 不改动菜单项自身的既有显示条件与删除/日志行为。
- 不改动窄屏分支信息缺口（本期接受，见 D3）。

## Decisions

### D1：`base_ref` 为必有字段（非 omitempty），值为落库全限定 ref

`taskRowDTO` 增加 `BaseRef string \`json:"base_ref"\``（无 omitempty），`toTaskDTO` 直接映射 `t.BaseRef`。worktree 任务为全限定 ref；非 worktree 任务与历史空值任务为空串。

- 理由：与 `mode`/`init_status` 等必有字段惯例一致（`taskRowDTO` 注释 `:519-522`）；前端判定简单（空串即不展示）；spec（task-detail-stream delta）已规定"必有"。
- 备选：omitempty 省略空值——拒绝，与 spec"必有"冲突，且空串/缺字段双态增加前端判定复杂度。
- 前端类型为 `base_ref: string`（必有）；对旧服务端运行时字段缺失的兼容按空串降级处理，不因此把新 API 契约写成可选。
- 影响面：`toTaskDTO` 被 REST 详情（`handleGetTask:322` → `buildTaskDetailDTOBase:340`）、SSE 流组装（复用同一 helper）、任务列表（`:244`）、创建响应（`:304`）与 rerun-init 响应（`:444`）复用，这些响应的 JSON 都会新增该字段；列表 UI 不展示来源分支，不为此另拆 DTO。
- 历史 worktree 空值：本特性上线前创建的任务 `base_ref` 可能为空串，属合法响应，不拒绝详情、不回填（见 Risks）。
- 非 worktree 任务的页头当前分支展示不经 `base_ref`（恒空串），数据源与降级见 D3。
- fail-closed 边界：`toTaskDTO` 现有 kind/mode 校验不变；`base_ref` 为纯透传，不新增校验（持久化值由创建路径保证合法，`resolveRepoBaseRef` 解析失败即创建失败）。

### D2：DTO 透出全限定 ref，短名转换在前端展示层完成

后端透出 `refs/heads/main` / `refs/remotes/origin/main` 原值；前端展示时 strip：`refs/heads/<name>` → `<name>`，`refs/remotes/<name>` → `<name>`（保留 remote 段，如 `origin/main`）。

- 理由：API 契约保留完整信息（remote 段是信息，丢掉有损）；短名是展示关注，归属 UI 层；`taskRowDTO` 其他字段均为落库原值，保持一致。后端 `baseBranchShortName` 未导出且服务激活流程，不为展示需求改变其可见性。
- 备选：后端直接透出短名——拒绝，信息有损且让 API 契约为单一展示场景服务。

### D3：页头分支呈现（designer 决策 A1；术语按 review 批注定为「来源分支 / 当前分支」，不使用"目标分支"）

`TaskWorkbenchPage.tsx:420` 的 `header-meta` span 原位扩展，按任务模式分三种呈现（最终形态经人工 review 两轮迭代确认：两行、无箭头）：

```
⎇ ocdeck/task-detail-support-more    ← 上行：当前分支（10px，正文色 --fg）
  ↳ origin/main                      ← 下行：来源分支（9px，muted·0.7，↳ 图标静态）
```

- **worktree 且 `base_ref` 非空**：两行——上行当前分支、下行来源分支（行序本身表达方向性，无箭头符号）
- **local-path（repo 项目）**：单行展示当前文件系统分支（仅上行，无来源行）
- **dir（gitless）**：整段不渲染（现状不变）

术语约定：「来源分支」= `base_ref` 短名；「当前分支」= worktree 任务取 `task.branch`，local-path 任务取文件系统当前 checkout 分支。

- local-path 当前分支的数据来源：复用既有 `api.gitStatus`（`GET /api/v1/tasks/:id/git/status`，`web/src/api.ts:215`）响应的 `branch` 字段（`GitStatusDTO.Branch`，`internal/application/dto.go:177-180`；后端语义：`git.CurrentBranch(worktreePath)` 失败时分支清空为空串，随后的 `git.Status` 失败则端点整体返回错误，`internal/task/gitops.go:61` 起）。请求条件 MUST 为 `task.project_kind === 'repo' && task.mode === 'local-path'`，MUST NOT 用 `!isGitlessTask(...)` 门控该请求或整个页头分支区（`isGitlessTask` 同时覆盖 dir 与 local-path，`web/src/types.ts:560-562`）。前端页头在任务详情就绪后获取一次（按任务身份隔离结果，随 taskID 切换重新获取，不依赖整个 SSE task 对象作为 effect 依赖以免重复请求），不轮询、不为页头向 SSE 详情流新增字段（保持流路径纯读、不调 git 的既有约束）。降级语义：非 401 请求失败及成功但分支为空串 → 仅不展示页头分支，不设置页面业务错误、不自动重试；401 沿用共享 API 客户端的既有认证失效流程（清 token + 全局未授权事件，`web/src/api.ts:90`），不属于本特性的降级范围。dir 项目由上述请求条件先行排除，不发起请求。
- 结构与层级：`.header-meta-branches`（inline-flex，align-items:flex-start，`overflow: visible` 以免裁切按钮焦点环）→ `BranchIcon`（对齐首行）+ `.branch-rows`（列方向 flex）→ 上行当前分支 button（10px、`--fg`）、下行 `.branch-src-row`（9px；来源分支 button 为 muted·0.7，`SourceRefIcon` ↳ 拐角引出图标为静态 muted 图标）。层级为三维信号：大小（10px/9px）+ 明暗（--fg/muted·0.7）+ 图标（⎇/↳），"哪个是当前分支"零歧义；9px 为 mono 可读性经验下限。两行总高 ≈26.6px，页头垂直平衡不受影响。不加"来源："文字标签（meta 级元素，标签会撑成标题级信息）。
- 截断与宽度下限：分支 button 各自独立省略号截断且 `min-width: 6ch`（ch 随各自字号缩放）；容器 `.header-meta-branches` `min-width: 10ch`（单/双分支共用，两行后横向压力减半，无双档 pair 下限）；页头紧张时由 `.workbench .page-title` 全断点收缩截断吸收；截断时 tooltip 是完整文本出口。
- 判定顺序（边界态）：
  1. dir 任务（gitless）→ 整段分支信息 span 不渲染，无改动；
  2. local-path 任务 → 获取文件系统当前分支：非空展示单行（仅当前分支，无来源行），空串/获取失败不展示；
  3. worktree：保留现有当前分支非空的外层渲染条件（`:420` 由 `task?.branch && !isBranchless` 控制），分支区已渲染且 `base_ref` 非空 → 两行展示（上当前/下来源短名）；
  4. worktree 但 `base_ref` 为空（历史任务）→ 只显示当前分支单行，与现状一致，不渲染占位；
  5. `base_ref` 转换后的短名与当前分支同名 → 两行正常各自展示 `main`/`main`，不做去重特判（比较的是转换后来源短名与 `branch`）。
- 窄屏（≤1024px）：`.header-meta` 已整段隐藏，来源分支随之不展示；本期不为窄屏新增展示（designer 明确接受的缺口；未来如需集中元信息走 Settings tab 独立需求）。
- 备选 A2（Settings tab 信息区块）：拒绝单独采用——来源分支是 merge/rebase 前的操作型信息，藏在第三个 tab 会打断工作流。

**点击复制（review 批注新增需求，designer 决策）**：页头展示的每个分支名 MUST 为独立的可复制 `<button type="button">`；`BranchIcon` 与 `SourceRefIcon` 保持静态不可点。

- 视觉归零：透明背景、无边框、padding 0，继承各自行的字号与颜色层级（上行 10px/--fg、下行 9px/muted·0.7），渲染结果与纯展示文本肉眼无差别；可点性信号为 `cursor: copy` + hover 提亮一档 + `text-decoration: underline dotted`，不新增图标；按钮视觉归零时 `:focus-visible` 焦点环 MUST 保留。
- 复制内容 = 各 button 自己显示的短名（所见即所得；取数据非 DOM 文本，截断显示不影响复制完整名）；全限定 ref 不进剪贴板，仅在 tooltip。
- 反馈复用 `.od-toast`（`web/src/design-system.css:538`，fixed 底部居中、`role="status"`、2000ms，参照 `TerminalView.tsx:49,135-139` 的 `TOAST_MS` + `clearTimeout` 重显模式）：成功文案 `已复制 <分支名>`（mono，比终端裸"已复制"多带分支名——双分支态下确认复制了哪一个是必要信息）；失败文案 `复制失败，完整分支名见悬浮提示`（button 化文本不可拖选，tooltip 是真实兜底出口）。手动点击 MUST NOT 套用 `takeToastSlot` 节流（该节流为远程程序自动复制设计，用户手势每次都值得确认）；连续点击重置计时器重显，不排队不叠加。
- aria-label：双分支态 `复制来源分支 <name>` / `复制当前分支 <name>`；单分支（local-path）态 `复制当前分支 <name>`。
- tooltip 从容器 span 下沉到各 button（职责合并：完整文本出口 + 复制可发现性），唯一模板集：来源 button——原值与短名不同时 `来源分支：<短名>（<全限定 ref>）（点击复制）`，相同时 `来源分支：<短名>（点击复制）`；当前 button——`当前分支：<name>（点击复制）`。容器 span 不再挂 title。
- 边界态：local-path 单分支 button 行为一致（无来源行）；`base_ref` 空（历史任务）仅当前分支 button，不渲染禁用态来源按钮；同名双分支照常各自独立可复制；窄屏 ≤1024px 整段隐藏、复制交互随之不存在（无需处理）；剪贴板 API 不可用时走共享 helper 的 execCommand 降级，再失败走失败 toast + tooltip 兜底。

### D4：溢出菜单空态隐藏（designer 决策 B1）

`WorkbenchOverflow` 将**可见**菜单项收集为列表（当前两项显示条件不变：`init_status==='failed'` → init 日志；`status!=='active'` → 删除；顺序保持"日志→删除"），列表为空则整个 `.header-overflow` `return null`（放在所有 hook 之后，同 `OpenInEditorMenu:94` 写法）。宽屏/窄屏两处 call site 共用组件内判定，无需分别处理。

- **"可见"与"可点击"的区分**：入口显隐仅依据菜单项的显示条件（可见性）；删除项的 `disabled` 状态（`isTransitional(task.status)`，`TaskWorkbenchPage.tsx:121`）不参与入口显隐——过渡状态、无 init 失败时入口保留，删除项显示且禁用，与原逻辑一致。
- **展开状态恢复**：菜单项由非空变为空（组件将 `return null`）时 MUST 同时关闭展开状态；后续菜单项恢复时仅显示关闭的触发器，不自动展开、不调用任何操作回调（`menuOpen` 为组件内 state，`:47`；实现时需覆盖该交互而非只测列表计算）。
- 保留行为：失焦/Escape 关闭、焦点恢复、backdrop、点击回调与禁用条件均不变。
- 理由：空菜单是断掉的 affordance（点击成本已付、回报为零）；与仓库已确立的"不可用即不出现"哲学一致（OpenInEditorMenu、删除按钮活跃态隐藏）；未来新增菜单项只需 push 列表项，显隐逻辑自动成立。
- 备选 B2（置灰"暂无更多操作"占位项）：拒绝——非操作项不提供行动路径，且 disclosure 模式打开时焦点移入"首个可用项"的查询在空菜单下返回 null，置灰占位只是在修补本不该出现的状态。

### D5：前端纯逻辑抽取为可测 helper

新增/扩展纯逻辑到 `.ts` 文件（仓库惯例，参照 `workbench-overflow.ts`、`command-center-selector.ts`）：

- `baseRefShortName(ref: string): string`——D2 的 strip 规则，完整行为：空串返回空串；两种已知前缀（`refs/heads/`、`refs/remotes/`）仅移除开头前缀；其余输入（未匹配前缀、前缀后为空等异常形态）原样返回，不抛错、不做新增查询。
- 溢出菜单项可见性计算（输入 `init_status`/`status`，输出菜单项列表）——D4 的显隐判定核心，组件只负责渲染。
- `writeTextToClipboard` 与私有 `copyViaExecCommand` 一并从 `TerminalView.tsx:61-79` 提取为共享 util（如 `web/src/clipboard.ts`），保持 `Promise<void>` 成功/失败语义与既有降级链不变：①`navigator.clipboard.writeText` 可用 → 调用一次，resolve 即成功、reject 即失败（MUST NOT 再尝试 execCommand）；②API 不可用 → 走 execCommand，返回 `true` 才成功，返回 `false` 或抛错均失败。TerminalView 与页头分支区共用；页头侧自行管理 `.od-toast` 展示（不套用 `takeToastSlot` 节流，见 D3），终端保留现有策略与反馈。

两者均由 vitest 单测覆盖（`web/src/__tests__/`）。

### D6：新建任务面板权限模式缺省改为 `ai-auto`

`CommandCenterPage` 新建任务面板的权限模式选择器缺省选中从 `ask` 改为 `ai-auto`（"AI 自动识别"）；面板内切换/清除/改选项目时的重置目标同步从 `ask` 改为 `ai-auto`。

- 提交链路自动适配：现有 `permArg = permMode === 'ask' ? undefined : permMode`（`CommandCenterPage.tsx:1000`）——未改动默认（`ai-auto`）时显式携带 `permission_mode: 'ai-auto'`；用户手选 `ask` 时沿用既有"非缺省才传"惯例省略字段，由后端缺省补 `ask`。后端契约不变。
- 范围边界：仅 Web 表单缺省与重置行为变化；API 层「未提供 → `ask`」的缺省语义、`task-permission-mode` 既有值域校验与持久化契约均不变。
- 可见性契约：权限模式选择器在面板打开后即渲染，与是否已选项目无关（为实现"初始 state 可被观察/测试"移除原 `selectedProject` 门控）；未选项目时仍不可提交（`canSubmit` 门禁不变），项目选择变化只按既有规则重置其值。
- 理由：用户明确要求"新建任务中，将 AI 识别设置为默认"（仅面板默认选中，不改后端缺省——已在探索确认）。

## 映射链与验证

`base_ref` 值传递链已逐跳核实（本变更不新增映射，仅在末端 DTO 透出）：

1. 创建落库：`createRepo` → `resolveRepoBaseRef`（`internal/task/crud.go:429-435`）；INSERT 携带 `BaseRef`（`internal/infrastructure/store/queries.go:359`）。
2. 读模型：`TaskRow.BaseRef` SELECT/Scan 保留（`queries.go:1565`；`internal/task/adapters.go:402、502、530`）。
3. DTO 组装：`toTaskDTO`（`internal/api/tasks.go:623`）——本次新增的透出点；REST 详情（`handleGetTask:322` → `buildTaskDetailDTOBase:340`）、SSE 流组装（`internal/api/task_stream.go` 复用该 helper）、列表（`:244`）、创建响应（`:304`）、rerun-init 响应（`:444`）共用。

无领域端口、Fx wiring、持久化 schema、mock 接口变更；`base_ref` 透传与 SSE 详情组装不新增 Git/opencode 调用；repo local-path 页头按 D3 额外调用既有 `git/status`，沿用该端点的任务锁、Git 查询及错误语义。不回填历史数据；SSE 推送保持纯读（既有 spec 约束不变）。

**验证要求（实现侧最小集）**：

- 后端：DTO JSON 键存在性与空串输出、两类全限定 ref 原样透出、REST 与 SSE 同任务字段一致、历史空值任务详情不报错。
- 前端：`baseRefShortName` 各前缀/空值/异常前缀单测；页头渲染矩阵（dir 不渲染 / local-path 展示文件系统当前分支、获取失败或空串静默降级 / worktree 有 base_ref 展示来源 / 空 base_ref 仅当前分支 / 同名不去重）；分支名点击复制（复制内容=显示短名、成功/失败 toast、aria-label、tooltip 下沉、local-path 单分支）；共享 clipboard util 提取后 TerminalView 既有行为不回归；溢出菜单可见项矩阵（含过渡状态禁用项保留入口）与"展开→隐藏→恢复"交互。

## Risks / Trade-offs

- [历史任务 base_ref 为空，来源分支不可见] → 按 D3 边界态 4 降级为现状展示；不回填历史数据（创建后基线可能已漂移，回填反而误导）。
- [local-path 当前分支依赖运行时 git 调用，可能失败或滞后（页头一次性获取，不轮询）] → 非 401 失败/空串静默降级为不展示（D3 边界态 2）；`git/status` 持任务锁，与生命周期操作竞争时返回 409，一次性获取遇到该错误后本次页面停留期间可能一直不显示分支，本期接受（不为此新增端点或重构锁）；滞后缺口本期接受，与 Git 面板"激活时加载一次、不自动轮询"的既有取舍一致。
- [窄屏用户看不到来源分支] → 已确认的接受缺口；完整信息未来经 Settings tab 元信息区块解决（独立需求）。
- [页头信息密度增加] → 分支区为两行 meta 级呈现（上行当前 10px/--fg、下行来源 9px/muted·0.7 + ↳ 静态图标），两行总高 ≈26.6px 不显著增加页头高度；按钮 `min-width: 6ch` + 独立省略号截断，容器 `min-width: 10ch`，页头紧张时由任务标题收缩吸收，tooltip 兜底完整文本。
