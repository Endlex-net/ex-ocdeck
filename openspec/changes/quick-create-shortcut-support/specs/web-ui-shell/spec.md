# web-ui-shell Specification (Delta)

## MODIFIED Requirements

### Requirement: ⌘K 全局命令面板

系统 SHALL 提供由可配置唤起热键（wire 默认值 `mod+k`；`⌘K / Ctrl+K` 仅为 UI 展示文案，见 palette-config spec「命令面板配置读写 API」）唤出的全局命令面板：条目涵盖静态入口七项（指挥中心、项目管理、设置·终端外观、设置·环境变量、设置·opencode 配置、设置·AI 配置、设置·命令面板；明确静态入口不含通知子标签）、当前任务列表（跳转工作台）与全局操作（新建任务、注册项目）；MUST 支持关键词模糊匹配（含中文关键词）、↑↓ 移动、Enter 执行、Esc 关闭。命令面板 MUST NOT 引入第三方组件库。

**触发词快速新建**：触发词匹配是大小写不敏感的字面前缀匹配，MUST NOT 解释为正则。共享原语 `foldForMatch(s)` = ECMAScript `String.prototype.toLowerCase.call(s)` 的结果；MUST NOT 使用 `toLocaleLowerCase`、`Intl.Collator` 或 Unicode normalization。触发词解析固定为比较 `query.slice(0, triggerWord.length)` 的 fold 值与 triggerWord 的 fold 值是否 `===`；空白边界与余文切片使用原始 UTF-16 下标。`classifyMatch`、`rankByQuery`、唯一匹配预选全部对 fold 后字符串执行 `===`、`startsWith`、`indexOf`。空白字符集合统一定义为 ECMAScript WhiteSpace + LineTerminator 集合：U+0009–000D、U+0020、U+00A0、U+1680、U+2000–200A、U+2028、U+2029、U+202F、U+205F、U+3000、U+FEFF。触发词前缀解析、余文 trim 与 Go 触发词校验 MUST 共用该集合定义（Go 侧不得用 unicode.IsSpace 的更大集合）。当输入以配置的快速新建触发词（默认 `new`）后随空白字符开头时，面板 MUST 进入快速新建模式，即使余文为空（空余文等价于不预选、过滤词为空）。`triggerWord + 空白` 即进入快速新建模式。触发词后的整段剩余文本 MUST 整体作为项目名（项目名可包含空格，MUST NOT 按空格拆词）；余文去除首尾空白但保留内部空白。该模式下「新建任务」命令 MUST 置顶且始终可执行；Enter 执行后 MUST 打开指挥中心新建任务面板并聚焦任务名输入（项目预选规则见 command-center spec「指挥中心内联新建任务」）。自由文本 Enter 只传 `projectName`；快速新建模式 Enter 在余文为空时 MUST 发送 `{ projectName: '' }`；候选点击同时传 `projectID` 与 `projectName`。允许携带 projectName；候选点击同时携带 projectID 与 projectName；projectID 不单独出现。候选列表 MUST 读取现有 `useProjects()` 共享快照（hooks.ts:215-241（useProjects 位于 :222）），MUST NOT 新建订阅、轮询或为打开面板额外 GET；订阅/轮询失败后保留上次成功快照（既有 store 语义）；首次尚无成功快照时按空项目列表处理、仅显示置顶命令，文本 Enter 仍可执行。候选条目 MUST 展示项目路径作为副文案。触发词模式 `new `（空余文）下置顶命令后展示全部项目候选，但 MUST NOT 自动预选。非空查询且排序结果为空时，展示全部项目作为候选，按同一名称确定序排序，点击候选仍携带 `projectID + projectName`。仅输入触发词而无尾随空白时 MUST 保持既有模糊匹配行为不变。

**项目候选排序**：快速新建模式下，置顶命令下方 MUST 展示按匹配优先级排序的项目候选列表：精确匹配（忽略大小写）优先于前缀匹配，前缀匹配优先于子串匹配（exact > prefix > substring）；同档位内子串起始位置靠前者优先；完全同分按名称确定序。名称确定序在 `foldForMatch(name)` 的结果上执行；比较其 UTF-16 code units：首个不同 UTF-16 code unit 较小者优先；共享部分完全相同（一方为另一方前缀）时长度较短者优先（前缀规则中的长度也是 fold 后字符串的 UTF-16 `.length`）；fold 后完全相同才按输入顺序稳定兜底；MUST NOT 使用原始名称或 `localeCompare`。该基准同时适用于正常排序、空查询全部项目与零命中 fallback 三处。该排序不受 `matchMode` 影响；`matchMode` 仅控制自动预选推断。选中某项目候选 MUST 等价于以该项目执行快速新建（预选该项目并聚焦任务名输入）。`classifyMatch` 空查询返回 null（判定层），`rankByQuery` 空查询返回全部项目按名称确定序（列表层例外）。`rankByQuery(items, '')` 是「排除不命中项」的唯一例外：空查询返回全部项目并按名称确定序排序；非空查询排除不命中项。非空查询且排序结果为空时，展示全部项目作为候选，按同一名称确定序排序，点击候选仍携带 `projectID + projectName`。零命中 fallback 按同一名称确定序。触发词模式 `new `（空余文）下置顶命令后展示全部项目候选，但 MUST NOT 自动预选。项目列表为空时 MUST 仅展示置顶的「新建任务」命令。

#### Scenario: 快捷键唤出与执行

- **WHEN** 用户在任意已认证页面按配置的唤起热键（wire 默认值 `mod+k`，UI 展示文案 `⌘K / Ctrl+K`），输入关键词后按 Enter
- **THEN** 面板打开并展示匹配条目，Enter 后执行首选项（导航或操作）并关闭面板

#### Scenario: 中文关键词匹配

- **WHEN** 用户输入"设置"或"任务"等中文关键词
- **THEN** 对应页面/任务条目被匹配并展示

#### Scenario: 触发词快速新建入口置顶

- **WHEN** 用户输入 `new 我的项目`（触发词为默认 `new`）
- **THEN** 「新建任务」命令置顶展示，其下方按匹配优先级展示项目候选列表

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

### Requirement: 路由收敛与旧链重定向

系统 SHALL 将路由收敛为：`#/`（指挥中心）、`#/task/:id`（任务工作台，保留 `?from` 来源感知返回）、`#/projects`（项目管理，`#/projects#<projectID>` 深链选中项目）、`#/configs`（设置，`#appearance|#env|#opencode|#ai|#notifications|#palette` 深链子标签）。旧路由 MUST 重定向而非 404：`#/active` → `#/`；`#/ai-config` → `#/configs#ai`；`#/project/:id` → `#/projects#<id>`。非法深链 MUST 有恢复路径：`#/projects#<不存在的id>` 回退为不选中任何项目（展示项目列表与空详情占位）；`#/configs#<未知tab>` 回退为 `#appearance`；`#/task/<不存在的id>` 保留现有 notFound 页与返回列表入口。工作台 `?from` 来源感知 MUST 归一为单一映射：`?from ∈ {home, projects, active}`，其中 `active` 为 legacy 别名映射到 `home`（旧 `#/task/:id?from=active` 链接不断）；未知值/缺省 → `home`；返回链接由统一函数解析（`home → #/`、`projects → #/projects#<projectID>`）。

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
