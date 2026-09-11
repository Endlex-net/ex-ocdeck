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

系统 SHALL 提供由可配置唤起热键（wire 默认值 `mod+k`；`⌘K / Ctrl+K` 仅为 UI 展示文案，见 palette-config spec「命令面板配置读写 API」）唤出的全局命令面板：条目涵盖静态入口七项（指挥中心、项目管理、设置·终端外观、设置·环境变量、设置·opencode 配置、设置·AI 配置、设置·命令面板；明确静态入口不含通知子标签）、当前任务列表（跳转工作台）与全局操作（新建任务、注册项目）；MUST 支持关键词模糊匹配（含中文关键词）、↑↓ 移动、Enter 执行、Esc 关闭。命令面板 MUST NOT 引入第三方组件库。

**触发词快速新建**：触发词匹配是大小写不敏感的字面前缀匹配，MUST NOT 解释为正则。共享原语 `foldForMatch(s)` = ECMAScript `String.prototype.toLowerCase.call(s)` 的结果；MUST NOT 使用 `toLocaleLowerCase`、`Intl.Collator` 或 Unicode normalization。触发词解析固定为比较 `query.slice(0, triggerWord.length)` 的 fold 值与 triggerWord 的 fold 值是否 `===`；空白边界与余文切片使用原始 UTF-16 下标。`classifyMatch`、`rankByQuery`、唯一匹配预选全部对 fold 后字符串执行 `===`、`startsWith`、`indexOf`。空白字符集合统一定义为 ECMAScript WhiteSpace + LineTerminator 集合：U+0009–000D、U+0020、U+00A0、U+1680、U+2000–200A、U+2028、U+2029、U+202F、U+205F、U+3000、U+FEFF。触发词前缀解析、余文 trim 与 Go 触发词校验 MUST 共用该集合定义（Go 侧不得用 unicode.IsSpace 的更大集合）。当输入以配置的快速新建触发词（默认 `new`）后随空白字符开头时，面板 MUST 进入快速新建模式，即使余文为空（空余文等价于不预选、过滤词为空）。`triggerWord + 空白` 即进入快速新建模式。触发词后的整段剩余文本 MUST 整体作为项目名（项目名可包含空格，MUST NOT 按空格拆词）；余文去除首尾空白但保留内部空白。该模式下「新建任务」命令 MUST 置顶且始终可执行；Enter 执行后 MUST 打开指挥中心新建任务面板并聚焦任务名输入（项目预选规则见 command-center spec「指挥中心内联新建任务」）。非空余文且存在命中候选时，默认键盘高亮 MUST 位于首个项目候选（按「项目候选排序」首位），Enter 即以该候选执行快速新建（携带 `projectID` 与 `projectName`）；空余文、零命中 fallback 或无项目快照时默认键盘高亮 MUST 位于置顶「新建任务」命令；余文每次变化后默认高亮按同一规则重置；↑↓ 移动与 Esc 关闭行为不变。自由文本 Enter 只传 `projectName`；快速新建模式 Enter 在余文为空时 MUST 发送 `{ projectName: '' }`；候选点击同时传 `projectID` 与 `projectName`。允许携带 projectName；候选点击同时携带 projectID 与 projectName；projectID 不单独出现。候选列表 MUST 读取现有 `useProjects()` 共享快照（hooks.ts:215-241（useProjects 位于 :222）），MUST NOT 新建订阅、轮询或为打开面板额外 GET；订阅/轮询失败后保留上次成功快照（既有 store 语义）；首次尚无成功快照时按空项目列表处理、仅显示置顶命令，文本 Enter 仍可执行。候选条目 MUST 展示项目路径作为副文案。触发词模式 `new `（空余文）下置顶命令后展示全部项目候选，但 MUST NOT 自动预选。非空查询且排序结果为空时，展示全部项目作为候选，按同一名称确定序排序，点击候选仍携带 `projectID + projectName`。仅输入触发词而无尾随空白时 MUST 保持既有模糊匹配行为不变。

**指令触发词模式**：当输入以某个已启用指令触发词（`commandTriggers` 非空值）+ 空白字符开头时，面板 MUST 进入该指令的模式：置顶该指令为唯一条目、默认键盘高亮位于该条目、Enter 或点击执行该指令（action/href 既有语义，`register-project` 含聚焦信号链路）、余文 MUST 被忽略（不参与过滤、不报错）、MUST NOT 展示项目候选或任务条目（`projects` 指令例外，见「projects 指令项目参数」）；Esc 关闭、↑↓ 行为不变。**projects 指令项目参数**：`projects` 指令触发词的余文作为项目名查询（空余文视为空查询，展示全部项目候选）——置顶「项目管理」命令下方 MUST 展示按「项目候选排序」的项目候选（复用同一排序、缩写档位、零命中 fallback 与默认键盘高亮规则：非空余文且有命中时默认高亮首个项目候选，空余文/零命中时默认高亮置顶命令）；项目候选 Enter 或点击 MUST 导航 `#/projects#<projectID>`（既有深链选中语义）；置顶「项目管理」命令 Enter 在唯一精确命中、或 `matchMode=exact-then-substring` 下唯一子串命中时 MUST 导航至该项目深链（缩写档位 MUST NOT 参与该推断），否则导航 `/projects` 且不选中。触发词解析在「全局触发词（快速新建，余文为项目名）+ 已启用指令触发词（余文忽略）」集合上按最长前缀匹配（fold 比较，`触发词 + 空白` 即进入模式）；同长度前缀不可能冲突（配置校验禁止重复值）。仅触发词无尾随空白时保持既有模糊匹配行为不变。

**项目候选排序**：快速新建模式下，置顶命令下方 MUST 展示按匹配优先级排序的项目候选列表：精确匹配（忽略大小写）优先于前缀匹配，前缀匹配优先于子串匹配，子串匹配优先于缩写匹配（exact > prefix > substring > acronym）；同档位内子串起始位置靠前者优先；完全同分按名称确定序。缩写匹配（acronym 档位）：项目名的缩写串 `acronymOf(name)` 定义为——对原始项目名按 `-`、`_`、空白字符（同一 ECMAScript WhiteSpace + LineTerminator 集合）拆段（跳过空段），每段内再按 camelCase 边界拆子段（边界为前一字符不是 ASCII 大写字母且当前字符是 ASCII 大写字母），取每个非空子段首字符逐个经 `foldForMatch` 折叠后按序拼接；分段 MUST 在原始名称上进行（MUST NOT 先 fold 再分段）。非空查询 `q` 在 `acronymOf(name)` 非空且 `acronymOf(name).startsWith(foldForMatch(q))` 时为缩写命中（首字母串前缀匹配，如 `gaaa`/`ga` 命中 `go-ai-agent-app`，`aa` 不命中）；缩写档内无位置比较，按名称确定序、再输入顺序兜底；同一项目同时命中子串与缩写时按更高档子串计。名称确定序在 `foldForMatch(name)` 的结果上执行；比较其 UTF-16 code units：首个不同 UTF-16 code unit 较小者优先；共享部分完全相同（一方为另一方前缀）时长度较短者优先（前缀规则中的长度也是 fold 后字符串的 UTF-16 `.length`）；fold 后完全相同才按输入顺序稳定兜底；MUST NOT 使用原始名称或 `localeCompare`。该基准同时适用于正常排序、空查询全部项目与零命中 fallback 三处。该排序不受 `matchMode` 影响；`matchMode` 仅控制自动预选推断。缩写档位 MUST NOT 参与 `matchMode` 的自动预选推断（预选推断仍仅精确匹配与唯一子串匹配）；缩写命中计入非空查询的命中集合（零命中 fallback 不触发、默认高亮移至首个候选的规则随之生效）。选中某项目候选 MUST 等价于以该项目执行快速新建（预选该项目并聚焦任务名输入）。`classifyMatch` 空查询返回 null（判定层），`rankByQuery` 空查询返回全部项目按名称确定序（列表层例外）。`rankByQuery(items, '')` 是「排除不命中项」的唯一例外：空查询返回全部项目并按名称确定序排序；非空查询排除不命中项。非空查询且排序结果为空时，展示全部项目作为候选，按同一名称确定序排序，点击候选仍携带 `projectID + projectName`。零命中 fallback 按同一名称确定序。触发词模式 `new `（空余文）下置顶命令后展示全部项目候选，但 MUST NOT 自动预选。项目列表为空时 MUST 仅展示置顶的「新建任务」命令。

#### Scenario: 快捷键唤出与执行

- **WHEN** 用户在任意已认证页面按配置的唤起热键（wire 默认值 `mod+k`，UI 展示文案 `⌘K / Ctrl+K`），输入关键词后按 Enter
- **THEN** 面板打开并展示匹配条目，Enter 后执行首选项（导航或操作）并关闭面板

#### Scenario: 中文关键词匹配

- **WHEN** 用户输入"设置"或"任务"等中文关键词
- **THEN** 对应页面/任务条目被匹配并展示

#### Scenario: 触发词快速新建入口置顶

- **WHEN** 用户输入 `new 我的项目`（触发词为默认 `new`）
- **THEN** 「新建任务」命令置顶展示，其下方按匹配优先级展示项目候选列表

#### Scenario: 命中时候选默认高亮

- **WHEN** 用户输入 `new <项目名>`（非空余文）且存在命中候选
- **THEN** 默认键盘高亮位于首个命中候选，Enter 以该候选执行快速新建（携带 `projectID` 与 `projectName`）；输入为空余文或零命中时，默认键盘高亮位于置顶「新建任务」命令

#### Scenario: 缩写档位匹配

- **WHEN** 项目 `go-ai-agent-app`（或 `goAiAgentApp`，缩写均为 `gaaa`）存在，用户输入 `new gaaa` 或其前缀 `ga`
- **THEN** 该项目按缩写档位（exact > prefix > substring > acronym 第四档）进入候选列表且计入命中集合；输入 `aa` 不构成缩写命中（非首字母串前缀），按既有零命中 fallback 处理；缩写命中不参与 `matchMode` 自动预选推断

#### Scenario: 仅触发词不改变既有行为

- **WHEN** 用户仅输入 `new`（无尾随空白）
- **THEN** 面板按既有模糊匹配展示条目，不进入快速新建模式

#### Scenario: 触发词加空白即使余文为空也进入快速新建

- **WHEN** 用户输入 `new `（`triggerWord + 空白`，余文为空）
- **THEN** 面板进入快速新建模式；空余文等价于不预选、过滤词为空。快速新建模式 Enter 在余文为空时 MUST 发送 `{ projectName: '' }`

#### Scenario: 空余文展示全部项目候选

- **WHEN** 用户输入 `new `（空余文）且项目列表非空
- **THEN** 触发词模式 `new `（空余文）下置顶命令后展示全部项目候选，但 MUST NOT 自动预选；`rankByQuery(items, '')` 是「排除不命中项」的唯一例外：空查询返回全部项目并按名称确定序排序

#### Scenario: 触发词大小写不敏感字面前缀且非正则

- **WHEN** 用户输入 `NEW 我的项目`（配置触发词为 `new`）
- **THEN** 因触发词匹配是大小写不敏感的字面前缀匹配（MUST NOT 解释为正则），面板进入快速新建模式，`我的项目` 作为项目名

#### Scenario: 含空格项目名整体解析

- **WHEN** 用户输入 `new my cool project`
- **THEN** `my cool project` 整体作为项目名参与匹配，不按空格拆词；余文去除首尾空白但保留内部空白

#### Scenario: 候选点击同时传 projectID 与 projectName

- **WHEN** 用户在快速新建模式点击某项目候选
- **THEN** 发出初始化信号，同时传该候选的 `projectID` 与 `projectName`；允许携带 projectName；候选点击同时携带 projectID 与 projectName；projectID 不单独出现

#### Scenario: 非空查询零命中时展示全部项目候选

- **WHEN** 用户输入 `new zzzz` 且无任何项目命中
- **THEN** 非空查询且排序结果为空时，展示全部项目作为候选，按同一名称确定序排序，点击候选仍携带 `projectID + projectName`

#### Scenario: 自由文本 Enter 只传 projectName

- **WHEN** 用户在快速新建模式对置顶「新建任务」按 Enter
- **THEN** 发出初始化信号，只传 `projectName`；允许携带 projectName；projectID 不单独出现

#### Scenario: 指令触发词直接执行

- **WHEN** 指令触发词 `cc` 已启用，用户输入 `cc ` 或 `cc 任意余文` 后按 Enter
- **THEN** 面板置顶「指挥中心」为唯一条目且默认高亮，Enter 跳转指挥中心，余文被忽略

#### Scenario: 最长前缀优先

- **WHEN** 指令触发词 `p` 与 `pr` 同时启用（前缀重叠允许），用户输入 `pr `
- **THEN** 进入 `pr`（项目管理）模式而非 `p` 所属指令的模式

#### Scenario: projects 指令带项目名跳转选中

- **WHEN** 指令触发词 `pr` 已启用，项目 `ocdeck` 存在，用户输入 `pr ocdeck`
- **THEN** 置顶「项目管理」命令下方按「项目候选排序」展示命中项目且默认高亮首个命中候选，候选 Enter 或点击导航 `#/projects#<projectID>` 选中该项目；置顶命令 Enter 在唯一精确或（`matchMode=exact-then-substring` 下）唯一子串命中时同样导航该项目深链，零/多命中时导航 `/projects` 不选中；`pr `（空余文）与零命中行为同快速新建（展示全部候选、默认高亮置顶命令）

### Requirement: 路由收敛与旧链重定向

系统 SHALL 将路由收敛为：`#/`（指挥中心）、`#/task/:id`（任务工作台，保留 `?from` 来源感知返回）、`#/projects`（项目管理，`#/projects#<projectID>` 深链选中项目）、`#/configs`（设置，`#appearance|#env|#opencode|#ai|#notifications|#palette|#tools` 深链子标签）。旧路由 MUST 重定向而非 404：`#/active` → `#/`；`#/ai-config` → `#/configs#ai`；`#/project/:id` → `#/projects#<id>`。非法深链 MUST 有恢复路径：`#/projects#<不存在的id>` 回退为不选中任何项目（展示项目列表与空详情占位）；`#/configs#<未知tab>` 回退为 `#appearance`；`#/task/<不存在的id>` 保留现有 notFound 页与返回列表入口。工作台 `?from` 来源感知 MUST 归一为单一映射：`?from ∈ {home, projects, active}`，其中 `active` 为 legacy 别名映射到 `home`（旧 `#/task/:id?from=active` 链接不断）；未知值/缺省 → `home`；返回链接由统一函数解析（`home → #/`、`projects → #/projects#<projectID>`）。

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

#### Scenario: 命令面板子标签深链

- **WHEN** 用户打开 `#/configs#palette`
- **THEN** 设置页打开并选中「命令面板」子标签

#### Scenario: 常用工具子标签深链

- **WHEN** 用户打开 `#/configs#tools`
- **THEN** 设置页打开并选中「常用工具」子标签

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

### Requirement: 版本未验证提示可关闭

全局「opencode 版本未验证」banner（`versionVerified === false` 触发）SHALL 提供「不再提示」关闭按钮。用户点击后系统 MUST 将该选择持久化于浏览器 localStorage（key：`ocdeck.versionNotice.dismissed`，值 `'1'`），此后该版本提示 MUST NOT 再展示——彻底关闭，opencode 版本变化后 MUST NOT 自动重新弹出，直至用户手动清除该存储项。关闭操作 MUST 仅作用于版本未验证提示，MUST NOT 影响 watchdog 降级告警的展示。localStorage 写入失败时 banner 当次仍然关闭（组件内状态），但不保证跨会话记住，且不阻塞其他功能。读取到任意非 `'1'` 值 MUST 视为未关闭；读取抛异常（如 `SecurityError`）MUST 捕获并视为未关闭，MUST NOT 向上抛出导致壳层渲染失败，banner 轮询与 watchdog 降级告警评估 MUST 照常进行。

#### Scenario: 点击不再提示

- **WHEN** 版本未验证 banner 展示中，用户点击「不再提示」
- **THEN** banner 立即消失并写入 localStorage；后续页面刷新、30s 轮询刷新、其他页面均不再展示版本未验证提示

#### Scenario: 彻底关闭不随版本复活

- **WHEN** 用户已关闭版本提示，之后 opencode 升级/降级为另一个未验证版本
- **THEN** 版本未验证提示仍不展示

#### Scenario: watchdog 告警不受影响

- **WHEN** 用户已关闭版本提示，且服务端进入 watchdog 降级状态
- **THEN** watchdog 降级告警照常展示

#### Scenario: 存储读取异常

- **WHEN** localStorage 读取抛异常（如 `SecurityError`）
- **THEN** 视为未关闭：版本未验证提示照常展示，组件不抛错，watchdog 告警评估不受影响

#### Scenario: 存储不可用

- **WHEN** localStorage 写入抛异常（如 SecurityError / quota 超限）
- **THEN** 本次点击仍关闭当前展示的 banner，不报错阻塞；下次会话可能重新展示

### Requirement: 侧栏任务导航后焦点落到终端

用户通过任务导航入口（侧栏任务项、工作台任务切换器、指挥中心任务行）导航到任务工作台后，系统 SHALL 将键盘焦点转移到该任务的终端输入，使后续键盘输入（含方向键）直接作用于 TUI 而非侧栏等导航元素。焦点转移 MUST 在终端就绪（可接收输入）后生效，且 MUST NOT 抢占用户已转移的输入焦点：焦点转移前若用户焦点已进入任何输入区（编辑器、提交框、文本框等），系统 MUST 取消本次焦点转移。终端处于锁定状态（见 `terminal-streaming` spec「触屏设备终端输入锁定」）时 MUST NOT 强制聚焦。对当前已打开任务的再次点击（同任务重复导航）MUST 与跨任务导航具有一致的焦点行为。普通断线重连 MUST NOT 触发焦点转移。

#### Scenario: 侧栏点击任务后方向键作用于 TUI

- **WHEN** 用户在侧栏点击某活跃任务导航到其工作台，终端渲染就绪
- **THEN** 键盘焦点位于终端，按上/下方向键直接操作 TUI（而非侧栏导航项）

#### Scenario: 再次点击当前任务同样聚焦终端

- **WHEN** 用户已在某任务工作台且焦点在侧栏，再次点击侧栏中该同一任务项
- **THEN** 键盘焦点转移到终端

#### Scenario: 不抢占用户输入焦点

- **WHEN** 任务导航后终端尚未就绪期间，用户已将焦点移入提交框等输入区
- **THEN** 终端就绪后 MUST NOT 抢夺焦点，用户输入不被打断

#### Scenario: 终端锁定时不强制聚焦

- **WHEN** 终端锁定能力启用且终端处于锁定状态，用户点击侧栏任务项
- **THEN** 系统 MUST NOT 聚焦终端 textarea，锁定状态保持

### Requirement: 侧栏宽度拖拽调整

应用侧栏 SHALL 支持拖拽调整宽度：侧栏右缘提供拖拽把手，拖动时侧栏宽度实时跟随；宽度 MUST 约束在 [180px, 480px]（默认 232px）。调整后的宽度 MUST 持久化于 localStorage（键 `ocdeck:sidebar-width`）并在刷新后恢复。侧栏折叠（⌘B 图标轨）与宽度调整互不干扰：折叠展开后 MUST 恢复用户调整后的宽度。

#### Scenario: 拖拽调整并持久化

- **WHEN** 用户拖拽侧栏右缘调整宽度后刷新页面
- **THEN** 侧栏以调整后的宽度渲染

#### Scenario: 折叠展开恢复自定义宽度

- **WHEN** 用户已自定义侧栏宽度，按 ⌘B 折叠为图标轨后再次展开
- **THEN** 侧栏恢复用户调整后的宽度而非默认宽度

### Requirement: 工作台页头分支展示（来源分支与当前分支）

任务工作台页头分支信息区 SHALL 按以下有序规则渲染。术语约定：「来源分支」= 任务 `base_ref` 的展示短名；「当前分支」= worktree 任务的 `branch` 字段，local-path 任务的文件系统当前 checkout 分支。MUST NOT 使用"目标分支"措辞。

1. dir 任务（gitless）：分支信息 span 整体不渲染，MUST NOT 新增路径或占位文本。
2. local-path 任务（repo 项目）：SHALL 展示文件系统当前 checkout 分支（单行仅当前分支，无来源行）；获取失败或分支为空（detached HEAD、git 异常）时不展示，静默降级，MUST NOT 展示占位文本。
3. worktree 任务：保留现有外层渲染条件（当前分支非空时才渲染分支信息区）。
4. worktree 且 `base_ref` 非空：两行展示——上行当前分支、下行来源分支（行序本身表达方向性，无箭头符号），来源分支视觉层级弱于当前分支（更小字号 + 更弱颜色 + 静态"派生自"图标）。
5. worktree 且 `base_ref` 为空串（历史任务或兼容性缺失）：仅展示当前分支单行，与现状完全一致，MUST NOT 展示"未知""-"等占位文本。

来源分支展示文本 SHALL 将全限定 ref 转换为短名：`refs/heads/<name>` → `<name>`；`refs/remotes/<name>` → `<name>`（保留 remote 段，如 `refs/remotes/origin/main` → `origin/main`）。「同名不去重」比较的是转换后的来源短名与当前分支名：两者相同仍正常两行各自展示，MUST NOT 做去重特判。展示分支名的完整未截断文本 SHALL 经 tooltip 可达。窄屏（页头分支信息区既有隐藏断点 ≤1024px）下分支信息随既有行为整段不展示，本期 MUST NOT 为窄屏新增展示。任务列表（指挥中心、项目管理、侧栏）MUST NOT 展示来源分支。

#### Scenario: worktree 任务两行展示当前与来源分支

- **WHEN** 打开 worktree 模式且 `base_ref` 为 `refs/heads/main`、当前分支为 `feature-x` 的任务工作台
- **THEN** 页头分支信息区两行展示：上行当前分支 `feature-x`（提亮）、下行来源分支 `main`（弱化 + 静态派生图标），tooltip 含两者完整文本

#### Scenario: 远端基线保留 remote 段

- **WHEN** 任务的 `base_ref` 为 `refs/remotes/origin/main`
- **THEN** 页头来源分支展示为 `origin/main`

#### Scenario: local-path 任务展示文件系统当前分支

- **WHEN** 打开 local-path 模式（repo 项目）任务的工作台，其路径当前 checkout 分支为 `feature-y`
- **THEN** 页头分支信息区单行展示当前分支 `feature-y`（无来源行）

#### Scenario: local-path 分支获取失败静默降级

- **WHEN** 打开 local-path 任务的工作台，当前分支获取失败（非 401 错误）或返回空串（如 detached HEAD）
- **THEN** 页头不渲染分支信息，不出现占位文本，不设置页面业务错误、不自动重试；401 响应沿用共享 API 客户端的既有认证失效流程，不受本降级语义约束

#### Scenario: dir 任务不渲染分支信息区

- **WHEN** 打开 dir 项目任务的工作台
- **THEN** 页头不渲染分支信息 span，不出现来源分支、路径替代文本或任何占位

#### Scenario: 历史任务 base_ref 空串降级

- **WHEN** 打开 worktree 模式但 `base_ref` 为空串的任务（如本特性上线前创建的历史任务）
- **THEN** 页头仅展示当前分支，与本特性上线前表现一致

### Requirement: 工作台页头分支名点击复制

任务工作台页头展示的每个分支名 SHALL 为独立的可复制控件：点击复制该控件自己显示的短名文本（所见即所得；显示被截断时仍复制完整名）。分支图标（⎇）与来源行的派生图标（↳）MUST 保持静态、不可点击。复制反馈 SHALL 经页面底部 toast 呈现：成功时 toast 含被复制的分支名（如 `已复制 feature-x`）；失败时 toast 指引 tooltip 兜底（如 `复制失败，完整分支名见悬浮提示`），MUST NOT 提供第三种失败 UI。每个分支控件 SHALL 有可发现性信号（复制指针、hover 提亮）与屏幕阅读器可辨的角色标注（aria-label 区分「复制来源分支」与「复制当前分支」）；tooltip SHALL 下沉到各分支控件并包含「点击复制」提示与完整分支文本。剪贴板 API 不可用时 SHALL 走既有降级链（execCommand 兜底）。`base_ref` 为空串时仅当前分支可复制，MUST NOT 渲染禁用态来源控件；来源与当前分支同名时两者 SHALL 照常各自独立可复制。窄屏（页头分支信息区既有隐藏断点 ≤1024px）下复制交互随展示一起不存在，本期无需处理。

#### Scenario: 复制来源分支成功

- **WHEN** 打开 `base_ref` 为 `refs/heads/main`、当前分支为 `feature-x` 的 worktree 任务工作台，点击来源分支 `main`
- **THEN** 剪贴板写入 `main`，页面底部 toast 显示「已复制 main」

#### Scenario: 复制当前分支成功

- **WHEN** 点击页头当前分支 `feature-x`
- **THEN** 剪贴板写入 `feature-x`，页面底部 toast 显示「已复制 feature-x」

#### Scenario: 复制失败反馈

- **WHEN** 点击分支名时剪贴板写入失败（Clipboard API 可用但写入被拒绝，或 API 不可用且 execCommand 降级失败）
- **THEN** 页面底部 toast 提示复制失败并指引 tooltip 兜底，tooltip 内含完整分支文本

#### Scenario: local-path 单分支复制

- **WHEN** 打开 local-path 任务的工作台（页头仅展示当前分支），点击该分支名
- **THEN** 剪贴板写入当前分支名，toast 反馈与双分支态一致

#### Scenario: 同名来源与当前分支各自复制

- **WHEN** 任务的来源短名与当前分支同名（如均为 `main`），分别点击两个分支控件
- **THEN** 两次均正常复制并各自 toast 反馈，不做去重特判

### Requirement: 工作台溢出菜单空态隐藏

任务工作台页头的「更多操作」溢出菜单入口 SHALL 仅在存在至少一个**可见**菜单项时渲染；当所有菜单项的既有显示条件均不满足时，整个菜单入口（按钮及其容器）MUST NOT 渲染，MUST NOT 以置灰占位项替代。菜单项的**可见性**仅由其显示条件决定（init 日志项仅 `init_status = failed` 时可见；删除项仅 `status ≠ active` 时可见，顺序保持"日志→删除"）；菜单项的禁用状态（如过渡状态下删除项 disabled）MUST NOT 参与入口显隐判定。可见菜单项集合随任务状态变化时，入口的显隐 MUST 随之动态更新（同一订阅数据驱动，无额外请求）。菜单项由非空变为空导致入口隐藏时 MUST 同时关闭展开状态；后续菜单项恢复时仅显示关闭的触发器，MUST NOT 自动展开菜单或调用任何操作回调。

#### Scenario: 无可见菜单项时入口隐藏

- **WHEN** 打开 `status = active` 且 `init_status ≠ failed` 的任务工作台
- **THEN** 页头不渲染「更多操作」溢出菜单入口

#### Scenario: 存在可见菜单项时入口出现

- **WHEN** 打开 `init_status = failed` 或 `status ≠ active` 的任务工作台
- **THEN** 页头渲染「更多操作」溢出菜单入口，点开可见满足条件的菜单项

#### Scenario: 过渡状态禁用项保留入口

- **WHEN** 任务处于过渡状态（`isTransitional` 集合：`creating`/`activating`/`suspending`/`deleting`）且 `init_status ≠ failed`
- **THEN** 「更多操作」入口保留渲染，菜单内删除项显示且处于禁用状态

#### Scenario: 状态迁移驱动入口动态显隐

- **WHEN** 当前任务由 `active` 迁移为 `suspended`（删除项变为可见）
- **THEN** 「更多操作」入口随订阅推送的数据更新而出现，无需手动刷新页面

#### Scenario: 展开中入口隐藏后恢复不自动展开

- **WHEN** 溢出菜单处于展开状态，期间可见菜单项变为空（入口隐藏），随后菜单项恢复（入口重新出现）
- **THEN** 恢复后的入口为关闭状态，菜单不自动展开，不调用任何操作回调
