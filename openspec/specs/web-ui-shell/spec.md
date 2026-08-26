# Web UI Shell Specification

## Purpose

全局应用壳层能力：侧栏导航脊柱、亮/暗双主题体系（含终端配色跟随主题）、⌘K 全局命令面板、路由收敛与旧链重定向、设计系统与全站响应式。这是 Web UI 的导航与视觉基座，所有管理页面运行于该壳层之内。

## Requirements

### Requirement: 全局应用壳层与侧栏导航

系统 SHALL 提供全局应用壳层包裹全部管理页面：左侧导航脊柱包含品牌区、指挥中心顶层项、按项目分组的任务组（任务项带 agent 状态点与注意力标记）、管理组（项目管理、设置）与底栏（⌘K 入口、本地地址、主题切换）。壳层 MUST 支持 ⌘B 快捷键与底栏按钮折叠为图标轨，折叠状态 MUST 持久化于 localStorage 并在下次打开时恢复。未认证时 MUST NOT 渲染壳层（仅呈现令牌门）。ServerStatusBanner MUST 在壳层内所有页面可见。

侧栏任务组数据 MUST 来自 App 层共享 projects store（`/api/v1/projects/stream` SSE 订阅 + 常驻低频兜底轮询，见 projects-stream spec）：壳层侧栏、指挥中心与项目管理页消费同一数据源，应用内 MUST NOT 存在第二个 `/projects` 轮询或第二条 projects 流订阅（兜底轮询属于 store 内部、single-flight 语义保持）。store MUST 暴露 `refresh()`（trailing 语义）：任何变更操作成功后调用；若调用时已有加载在途，MUST 在该请求结束后再补发一次——`refresh()` 承诺其结果反映调用之后的最新状态（MUST NOT 以 mutation 前的在途快照交差）。流订阅或兜底轮询失败时侧栏 MUST 保留上次成功数据静默展示（错误由 ServerStatusBanner 与页面级错误提示承担，侧栏本身不闪空态）。

**移动端任务入口**：≤767px 视口侧栏任务组隐藏时，任务切换入口 MUST 由工作台页头的任务切换器承担（执行与侧栏相同的 hash 导航 + 按 taskID 重挂载），移动端 MUST NOT 失去任务间直达能力。

#### Scenario: 跨页面导航

- **WHEN** 用户在任意页面点击侧栏的指挥中心/项目管理/设置
- **THEN** 应用切换到对应页面且侧栏保持渲染（不整页重载）

#### Scenario: 折叠持久化

- **WHEN** 用户按 ⌘B 折叠侧栏后刷新页面
- **THEN** 侧栏保持折叠的图标轨形态

#### Scenario: 侧栏任务组状态呈现

- **WHEN** 存在活跃或挂起任务
- **THEN** 侧栏按项目分组展示这些任务，活跃任务显示 agent 状态点（idle/busy/retry），有待处理注意力项的任务显示注意力标记；归档任务 MUST NOT 出现在侧栏

#### Scenario: projects 数据流驱动更新

- **WHEN** store 订阅存续期间收到 projects 流的 `snapshot` 或 `update` 帧
- **THEN** store 以帧内裸数组整表替换快照，侧栏任务组随之收敛，期间不存在对 `/api/v1/projects` 的固定 5 秒轮询请求

#### Scenario: 侧栏切换任务

- **WHEN** 用户点击侧栏任务组中的某任务
- **THEN** 应用导航至该任务工作台 `#/task/:id`

### Requirement: 亮/暗双主题体系

系统 SHALL 提供应用级亮/暗主题：`<html data-theme="light|dark">` 驱动全部 token 翻转；主题偏好 MUST 持久化于 localStorage（`od-theme` ∈ `system|light|dark`，缺省 `system` 跟随系统偏好）；主题设置 MUST 在首次绘制前同步应用（index.html 内联脚本）避免闪烁；设置页 MUST 提供跟随系统/浅色/深色分段控件。**终端配色 MUST 跟随应用主题**：暗色主题 MUST 使用深色终端 palette、亮色主题 MUST 使用浅色终端 palette；palette 覆盖容器背景、默认前景/背景、光标、选区与 ANSI 16 色；主题切换（含 `system` 模式下操作系统主题变化）时所有已挂载终端 MUST 原地更新配色，MUST NOT 重建 TermSession/xterm 实例、MUST NOT 重连 WebSocket，scrollback、焦点、触摸锁状态与连接状态 MUST 保持。CSS `--term-*` 变量与 xterm `ITheme` MUST 来自同一 light/dark palette resolver（单一事实来源）。组件级规则中语义色 MUST 由品牌 token 经 `color-mix(in oklch, …)` 派生，界面 MUST NOT 引入 token 之外的硬编码色值；token 定义层（`--success/--warn/--danger/--term-*` 等）允许直接 oklch 定义（见设计系统要求的派生边界）。自定义终端配色预设为后续扩展，本期 MUST NOT 出现在设置页。

#### Scenario: 切换主题立即生效

- **WHEN** 用户在设置页将主题从跟随系统切换为深色
- **THEN** 全部页面立即应用深色 token，刷新后保持深色

#### Scenario: 默认跟随系统

- **WHEN** 用户从未设置过主题
- **THEN** 界面按系统 prefers-color-scheme 渲染，系统主题变化时界面跟随

#### Scenario: 防闪烁

- **WHEN** 用户偏好深色并刷新页面
- **THEN** 首帧即为深色，不出现先亮后暗的闪烁

#### Scenario: 终端配色随主题翻转

- **WHEN** 已挂载终端的任务工作台处于亮色主题，用户切换为深色主题
- **THEN** 终端画布原地切换为深色 palette（容器/前景/背景/光标/选区/ANSI 16 色），不重建终端实例、不重连 WebSocket，scrollback 与输入焦点保持

#### Scenario: system 模式跟随 OS 变化

- **WHEN** 主题偏好为 `system` 且操作系统从浅色切到深色
- **THEN** 已挂载终端与全部页面同步切换为深色 palette

### Requirement: ⌘K 全局命令面板

系统 SHALL 提供 ⌘K 或 Ctrl+K 唤出的全局命令面板：条目涵盖静态入口（指挥中心、项目管理、设置四子标签深链，共 6 项，与 `docs/design/assets/ocdeck-palette.js` 注册表一致）、当前任务列表（跳转工作台）与全局操作（新建任务、注册项目）；MUST 支持关键词模糊匹配（含中文关键词）、↑↓ 移动、Enter 执行、Esc 关闭。命令面板 MUST NOT 引入第三方组件库。

#### Scenario: 快捷键唤出与执行

- **WHEN** 用户在任意已认证页面按 ⌘K 或 Ctrl+K，输入关键词后按 Enter
- **THEN** 面板打开并展示匹配条目，Enter 后执行首选项（导航或操作）并关闭面板

#### Scenario: 中文关键词匹配

- **WHEN** 用户输入"设置"或"任务"等中文关键词
- **THEN** 对应页面/任务条目被匹配并展示

### Requirement: 路由收敛与旧链重定向

系统 SHALL 将路由收敛为：`#/`（指挥中心）、`#/task/:id`（任务工作台，保留 `?from` 来源感知返回）、`#/projects`（项目管理，`#/projects#<projectID>` 深链选中项目）、`#/configs`（设置，`#appearance|#env|#opencode|#ai` 深链子标签）。旧路由 MUST 重定向而非 404：`#/active` → `#/`；`#/ai-config` → `#/configs#ai`；`#/project/:id` → `#/projects#<id>`。非法深链 MUST 有恢复路径：`#/projects#<不存在的id>` 回退为不选中任何项目（展示项目列表与空详情占位）；`#/configs#<未知tab>` 回退为 `#appearance`；`#/task/<不存在的id>` 保留现有 notFound 页与返回列表入口。工作台 `?from` 来源感知 MUST 归一为单一映射：`?from ∈ {home, projects, active}`，其中 `active` 为 legacy 别名映射到 `home`（旧 `#/task/:id?from=active` 链接不断）；未知值/缺省 → `home`；返回链接由统一函数解析（`home → #/`、`projects → #/projects#<projectID>`）。

#### Scenario: 旧来源参数兼容

- **WHEN** 用户打开 `#/task/abc?from=active`
- **THEN** 工作台正常打开，返回链接指向 `#/`（指挥中心）

#### Scenario: 未知来源参数回退

- **WHEN** 用户打开 `#/task/abc?from=foobar`（任务 abc 存在）
- **THEN** 工作台正常打开，返回链接指向 `#/`

#### Scenario: 旧活跃会话链接重定向

- **WHEN** 用户打开历史书签 `#/active`
- **THEN** 应用重定向至 `#/`（指挥中心）

#### Scenario: 旧 AI 配置链接重定向

- **WHEN** 用户打开 `#/ai-config`
- **THEN** 应用重定向至 `#/configs#ai` 并选中 AI 子标签

#### Scenario: 旧项目详情链接重定向

- **WHEN** 用户打开 `#/project/abc`
- **THEN** 应用重定向至 `#/projects#abc` 并在项目管理页选中项目 abc

#### Scenario: 非法项目深链回退

- **WHEN** 用户打开 `#/projects#不存在的id`
- **THEN** 项目管理页正常打开，不选中任何项目，展示项目列表与空详情占位，不报错

#### Scenario: 非法设置子标签回退

- **WHEN** 用户打开 `#/configs#foobar`
- **THEN** 设置页打开并回退选中 `#appearance` 子标签，不报错

### Requirement: 设计系统与全站响应式

系统 SHALL 以单一设计系统样式表（token + 组件类）替换原 ad-hoc 全局样式：6 个品牌 token（`--bg/--surface/--fg/--muted/--border/--accent`）+ 固定墨色对（`--ink/--on-ink`）+ 语义色派生；徽章、按钮、告警条、表单、模态、行列表 MUST 使用设计系统组件类。**token 派生规则的边界**：组件级规则 MUST 经 `color-mix` 派生、MUST NOT 引入新色相字面量；token 定义层（`:root`/暗色块中的 `--success/--warn/--danger/--term-*` 等）是色彩值的唯一事实来源，允许直接 oklch 定义。全部页面 MUST 按断点适配：>1024px 完整布局、≤1024px 钻取/堆叠、≤767px 紧凑布局。触屏目标：主要操作控件（按钮/输入/锁钮/导航项）移动端 MUST ≥44px；行内密集辅助控件（tab 关闭/添加、行内图标按钮）与密集列表控件（tab 条目、命令面板条目）豁免，移动端 ≥32px 并以间距隔离。终端工程契约 MUST 保持不变：mobile.css 的 z 轴层级（终端 < 锁遮罩 < 浮动锁钮 < 连接状态遮罩）、IME 补偿、触摸锁与手势逻辑 MUST NOT 因换肤改变；Git diff 视图横向滚动所有权 MUST 仅属于 diff 容器。

#### Scenario: 移动端管理页可用

- **WHEN** 用户在 ≤767px 视口打开项目管理或设置页
- **THEN** 页面以紧凑布局完整可用，不出现桌面固定宽度的横向溢出

#### Scenario: 终端契约不受换肤影响

- **WHEN** 触屏设备打开任务工作台终端
- **THEN** 终端默认锁定、浮动锁钮与连接状态遮罩的层级与行为与改版前一致

#### Scenario: 无硬编码色值

- **WHEN** 审查改版后的样式表与组件
- **THEN** 除 token 定义外不存在新增硬编码 hex 色值，派生色均经 color-mix 生成
