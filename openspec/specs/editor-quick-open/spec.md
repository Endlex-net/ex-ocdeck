# editor-quick-open

## Purpose

前端本机编辑器快捷打开能力：设置页「常用工具」子标签提供 VSCode/GoLand/Cursor 本机编辑器启用开关（默认关闭、本设备 localStorage 持久化）、可选的自定义唤起 URI 模板（默认内置本地模板）与自定义工具（名称 + URI 模板）；任务详情页页头提供「默认工具图标主按钮 + 下拉」快捷打开入口，按任务工作目录经本机浏览器 URL scheme 唤起本机编辑器。纯前端能力，服务端零改动。

## Requirements

### Requirement: 常用工具开关设置

系统 SHALL 在设置页提供「常用工具」子标签（tab 值为 `tools`，深链 `#/configs#tools`，标签文案「常用工具」），内含 VSCode、GoLand 与 Cursor 三个本机编辑器的启用开关。每个开关独立开启/关闭，**缺省（无存储记录）均为关闭**。开关状态 MUST 仅持久化于本设备浏览器 localStorage，MUST NOT 写入服务端或随任何 API 请求发送。

localStorage 契约：键 `ocdeck.editorTools.vscode`、`ocdeck.editorTools.goland` 与 `ocdeck.editorTools.cursor`。写入语义 MUST 为：开启写 `'1'`、关闭写 `'0'`；读取语义 MUST 为：仅 `'1'` 视为开启，其余任何值或缺省视为关闭。读取侧遇到损坏/非法数据 MUST 只回退默认值（关闭），MUST NOT 改写 localStorage；写入侧失败 MUST 向上抛出且不派发变更事件、不视为已生效。开关变更 MUST 即时生效于当前已打开页面的快捷打开入口显隐（无需刷新页面）。

Cursor 的模板 scheme 白名单为 `cursor://`；内置唤起分支与 VSCode 同构（`cursor://file/<分段编码路径>/`，尾部斜杠收敛为恰好一个）。设置面板 SHALL 为 Cursor 提供与 VSCode 同构的可选自定义唤起 URI 模板配置（placeholder `cursor://file{path}/`）。

#### Scenario: 缺省关闭

- **WHEN** 用户首次（无 localStorage 记录）打开设置页「常用工具」子标签
- **THEN** VSCode 与 GoLand 两个开关均呈现为关闭状态

#### Scenario: 开启开关并持久化

- **WHEN** 用户开启 VSCode 开关后刷新页面
- **THEN** VSCode 开关保持开启，GoLand 开关仍为关闭

#### Scenario: 关闭序列化

- **WHEN** 用户关闭已开启的 VSCode 开关
- **THEN** `ocdeck.editorTools.vscode` 被写入 `'0'`，且 `ocdeck.editorTools.defaultTool` 不被清除或改写

#### Scenario: 损坏数据回退

- **WHEN** localStorage 中 `ocdeck.editorTools.vscode` 的值为非法内容（非 `'1'`）
- **THEN** 该开关按关闭处理，且 localStorage 中的值不被改写

### Requirement: 默认编辑器记忆

系统 SHALL 记忆用户的默认编辑器（用于快捷打开主按钮）。localStorage 键 `ocdeck.editorTools.defaultTool`，合法值为内置名 `'vscode'`/`'goland'`/`'cursor'` 与自定义工具引用 `'custom:<id>'`（id 非空）。默认编辑器的解析规则 MUST 为：存储值合法且对应工具当前存在（内置已启用 / 自定义 id 存在）→ 以存储值为默认；否则回退为固定顺序（VSCode → GoLand → Cursor）中第一个已启用的内置工具；再无则取自定义工具列表（存储顺序）中的第一个；仍无则不存在默认编辑器。

默认工具键的唯一写入口 MUST 为快捷打开下拉中的显式选择；关闭/开启工具开关 MUST 只更新对应开关键，MUST NOT 清除或改写默认工具键（开关关闭后的默认回退仅为派生状态，不落库）。读取侧遇到非法值 MUST 只按上述回退规则处理、MUST NOT 改写 localStorage；读取时 localStorage 抛异常（如不可用）MUST 按无记录处理并返回回退结果，MUST NOT 抛至渲染层。存储的默认值变更 MUST 与开关变更一样经由变更事件/storage 事件即时收敛到已打开的快捷打开入口。

#### Scenario: 默认工具持久化

- **WHEN** 用户在快捷打开下拉中选择 GoLand（两个工具均已启用），随后刷新页面重新打开任务详情页
- **THEN** 主按钮图标为 GoLand，下拉中 GoLand 带 ✓ 标识

#### Scenario: 存储的默认工具已被关闭

- **WHEN** 存储的默认工具为 GoLand，用户在设置页关闭 GoLand 开关（VSCode 保持开启），随后打开任务详情页
- **THEN** 主按钮图标回退为 VSCode

#### Scenario: 关闭后重新开启恢复显式选择

- **WHEN** 存储的默认工具为 GoLand，用户关闭 GoLand 开关后又重新开启（期间未在下拉中显式选择）
- **THEN** 默认编辑器恢复为 GoLand（`ocdeck.editorTools.defaultTool` 始终保持 `'goland'` 未被改写）

#### Scenario: 默认键读取异常回退

- **WHEN** 读取 `ocdeck.editorTools.defaultTool` 时 localStorage 抛异常，且仅 VSCode 已启用
- **THEN** 默认编辑器按回退规则解析为 VSCode，渲染不抛错，存储不被改写

### Requirement: 任务详情页快捷打开入口

系统 SHALL 在任务详情页（任务工作台）页头提供「在编辑器中打开」入口，形态为「默认工具图标主按钮 + 下拉触发按钮」的组合（即使仅启用一个工具也保持该组合结构）。仅当至少一个可用工具存在（内置已启用或自定义工具名称与模板齐全）时 MUST 呈现该入口；无任何可用工具时 MUST 完全隐藏、不留占位。打开目标 MUST 为任务详情接口返回的 `worktree_path` 字段（worktree 模式任务为 worktree 目录；dir 项目任务与 repo 项目 local-path 模式任务该字段为项目路径），前端 MUST NOT 按任务模式自行拼接或改写路径。

交互语义 MUST 为：点击主按钮直接以当前默认编辑器打开；下拉菜单列出全部可用工具（顺序：已启用内置按 VSCode → GoLand → Cursor，其后为自定义工具按存储顺序；名称为空的自定义行不展示）并以 ✓ 标识当前默认工具；点击下拉中某工具即用它打开，并将其设为默认编辑器（持久化，自定义工具写 `custom:<id>`）。下拉为普通 disclosure 模式：Escape 关闭、点击外部关闭。

执行顺序 MUST 为单一主流程：主按钮执行 ①→③，下拉选择执行 ①→②→③：

1. 校验目标工具当前已启用、`worktree_path` 非空，并完成 URI 构造；任一失败 MUST 不写存储、不派发事件、不唤起、不改变默认编辑器。
2. （仅下拉选择）调用默认工具键写入；写入失败 MUST 由组件捕获，保留原默认编辑器与 ✓ 标识，MUST NOT 执行唤起。
3. 第 ① 步成功后，主按钮跳过第 ② 步进入本步；下拉选择仅在第 ② 步写入成功后进入本步。在同一用户手势调用链内同步执行唤起，MUST NOT 插入异步等待。

同步唤起抛异常时 MUST 由组件捕获、页面保持可用；已成功保存的默认编辑器 MUST NOT 回滚；协议处理器是否实际打开不可观测，MUST NOT 作为任何状态变更的条件。菜单关闭规则 MUST 为：选择成功（完成唤起调用）或失败均关闭菜单。`worktree_path` 为空或缺失时 MUST 保留组合入口但禁用主按钮与下拉触发按钮，MUST NOT 触发唤起。

唤起动作 MUST 为纯前端行为（本机浏览器触发 URL scheme），MUST NOT 依赖服务端执行任何命令或新增任何 API。协议未注册（本机未安装对应编辑器）时无法可靠检测失败，入口 MUST NOT 因此阻断页面其他功能。

#### Scenario: 两个工具均关闭时入口隐藏

- **WHEN** VSCode 与 GoLand 开关均关闭，用户打开任务详情页
- **THEN** 页头不呈现「在编辑器中打开」入口，也无占位元素

#### Scenario: 主按钮直接打开

- **WHEN** 仅 VSCode 已启用，用户点击页头编辑器主按钮
- **THEN** 浏览器触发打开该任务 `worktree_path` 的 VSCode URL scheme

#### Scenario: 下拉选择即打开并设为默认

- **WHEN** 两个工具均已启用、当前默认为 VSCode，用户在下拉中点击 GoLand
- **THEN** 浏览器触发以 GoLand 打开该任务 `worktree_path`，且 GoLand 被设为默认编辑器（下拉 ✓ 移到 GoLand，刷新后仍保持）

#### Scenario: 空路径禁用入口

- **WHEN** 任务详情接口返回的 `worktree_path` 为空，且至少一个工具已启用
- **THEN** 组合入口保留呈现但主按钮与下拉触发按钮均为禁用态，点击不触发任何唤起或存储写入

#### Scenario: 主按钮前置检查失败零副作用

- **WHEN** 用户点击主按钮，但第 ① 步校验失败（目标工具已不处于启用状态，或 `worktree_path` 为空，或 URI 构造失败）
- **THEN** 不写存储、不派发变更事件、不触发唤起、默认编辑器不变，菜单关闭

#### Scenario: 默认工具写失败不唤起

- **WHEN** 用户在下拉中点击 GoLand，但 `ocdeck.editorTools.defaultTool` 写入失败（localStorage 抛异常）
- **THEN** 不触发唤起，默认编辑器与 ✓ 标识保持原值，页面保持可用，菜单关闭

#### Scenario: 同步唤起异常页面可用

- **WHEN** 唤起调用本身同步抛异常（默认工具键已写入成功）
- **THEN** 异常被捕获、页面保持可用，已保存的默认编辑器不回滚

#### Scenario: 开关变更即时反映入口显隐

- **WHEN** 任务详情页已打开且入口隐藏，用户在另一标签页的设置页开启 VSCode 开关后回到任务详情页
- **THEN** 入口呈现（跨标签页经由 storage 事件收敛，无需手动刷新）

### Requirement: 编辑器唤起 URI 构造

系统 SHALL 按「公共归一化 + 两个互斥分支」构造唤起 URI：

公共归一化 MUST 为：仅将路径中的反斜杠 `\` 统一转为 `/`，得到归一化路径。

VSCode 分支 MUST 为：对归一化路径按 `/` 分段，每段做 URI 组件编码（空格、`#`、`?`、`%`、`&` 等保留字符必须编码），Windows 盘符段的冒号保留字面（如 `C:`），以 `/` 重新连接后去掉开头的 `/`，拼到 `vscode://file/` 前缀之后；随后将结果路径的尾部 `/` 收敛为恰好一个（归一化路径已带一个或多个尾斜杠时 MUST NOT 重复追加，多余尾斜杠收敛为一个）。该尾斜杠收敛仅属于 VSCode 分支。

GoLand 分支 MUST 为：`goland://open?file=` 后接**对归一化路径整体做一次查询值编码**（`/` 编码为 `%2F`、盘符冒号编码为 `%3A`、空格编码为 `%20`、`%` 编码为 `%25`）；MUST NOT 复用 VSCode 分支的分段编码结果，MUST NOT 二次编码。

Cursor 分支 MUST 与 VSCode 分支同构：`cursor://file/` 前缀 + 分段编码路径（盘符段冒号保留字面）+ 尾部 `/` 收敛为恰好一个。

`worktree_path` 为空或缺失时 MUST NOT 构造 URI、MUST NOT 触发唤起（入口禁用态见「任务详情页快捷打开入口」）。

#### Scenario: 含空格路径编码（VSCode）

- **WHEN** 任务 `worktree_path` 为 `/Users/me/my project` 且用户以 VSCode 打开
- **THEN** 触发的 URI 为 `vscode://file/Users/me/my%20project/`

#### Scenario: 已有尾斜杠不重复追加（VSCode）

- **WHEN** 任务 `worktree_path` 为 `/Users/me/proj/` 且用户以 VSCode 打开
- **THEN** 触发的 URI 为 `vscode://file/Users/me/proj/`

#### Scenario: 多重尾斜杠收敛（VSCode）

- **WHEN** 任务 `worktree_path` 为 `/Users/me/proj//` 且用户以 VSCode 打开
- **THEN** 触发的 URI 为 `vscode://file/Users/me/proj/`

#### Scenario: Windows 路径归一（GoLand）

- **WHEN** 任务 `worktree_path` 为 `C:\work\my proj` 且用户以 GoLand 打开
- **THEN** 触发的 URI 为 `goland://open?file=C%3A%2Fwork%2Fmy%20proj`

#### Scenario: 含百分号路径单次编码（GoLand）

- **WHEN** 任务 `worktree_path` 为 `/tmp/a b%c` 且用户以 GoLand 打开
- **THEN** 触发的 URI 为 `goland://open?file=%2Ftmp%2Fa%20b%25c`（`%` 编码为 `%25`，不出现双重编码）

### Requirement: 自定义唤起 URI 模板

系统 SHALL 允许用户为每个工具配置可选的自定义唤起 URI 模板，localStorage 键为 `ocdeck.editorTools.uriTemplate.vscode` 与 `ocdeck.editorTools.uriTemplate.goland`。未配置（缺省或空）MUST 使用「编辑器唤起 URI 构造」规定的内置分支；内置分支行为 MUST NOT 因模板能力的引入而改变。

模板合法性的判定 MUST 为：模板包含至少一个 `{path}` 占位符，且以该工具允许的 scheme 前缀开头（VSCode 工具允许 `vscode://` 与 `vscode-insiders://`；GoLand 工具允许 `goland://` 与 `jetbrains://`）。不满足任一条件的模板 MUST 按未设置处理（回退内置分支），MUST NOT 改写 localStorage，MUST NOT 因此阻断唤起。

模板有效时，唤起的 URI MUST 为：将模板中全部 `{path}` 出现替换为「模板路径注入值」后的字符串，MUST NOT 对替换结果再做任何编码。模板路径注入值 MUST 为：归一化路径（仅反斜杠转 `/`）按 `/` 分段、每段做 URI 组件编码（Windows 盘符段冒号保留字面）、以 `/` 重新连接、保留开头的 `/`、不做尾斜杠收敛。

写入语义 MUST 为：保存非空模板写入原文；保存空模板清除对应键；写入失败向上抛出且不派发变更事件。模板变更 MUST 与开关变更一样经变更事件/storage 事件即时收敛，快捷打开入口 MUST 在点击时读取最新模板参与 URI 构造。

设置面板 MUST 在模板配置处提供能力说明：VSCode 可通过 `vscode://vscode-remote/ssh-remote+<主机>{path}` 形式的模板打开远程开发机目录（需本机已安装 Remote-SSH 扩展且 SSH 可达；该形式为社区实测、非官方文档化）；GoLand 不存在通过 URL 打开远程项目的可用形式。

#### Scenario: 自定义模板生效（本地）

- **WHEN** VSCode 工具配置模板 `vscode://file{path}`（无尾斜杠），任务 `worktree_path` 为 `/Users/me/my project`，用户以 VSCode 打开
- **THEN** 触发的 URI 为 `vscode://file/Users/me/my%20project`

#### Scenario: 远程 SSH 模板（VSCode）

- **WHEN** VSCode 工具配置模板 `vscode://vscode-remote/ssh-remote+devbox{path}`，任务 `worktree_path` 为 `/home/me/proj`，用户以 VSCode 打开
- **THEN** 触发的 URI 为 `vscode://vscode-remote/ssh-remote+devbox/home/me/proj`

#### Scenario: jetbrains scheme 模板生效（GoLand）

- **WHEN** GoLand 工具配置模板 `jetbrains://goland/navigate/reference?project={path}`，任务 `worktree_path` 为 `/Users/me/my project`，用户以 GoLand 打开
- **THEN** 触发的 URI 为 `jetbrains://goland/navigate/reference?project=/Users/me/my%20project`

#### Scenario: 缺省模板走内置分支

- **WHEN** 未配置任何模板，任务 `worktree_path` 为 `/Users/me/proj`，用户以 VSCode 打开
- **THEN** 触发的 URI 为 `vscode://file/Users/me/proj/`（与内置 VSCode 分支一致）

#### Scenario: 非法模板回退内置（缺占位符）

- **WHEN** VSCode 工具配置的模板不含 `{path}`（如 `vscode://file/`），用户以 VSCode 打开 `/Users/me/proj`
- **THEN** 按内置 VSCode 分支构造 URI（`vscode://file/Users/me/proj/`），且 localStorage 中的模板值不被改写

#### Scenario: scheme 不符回退内置

- **WHEN** GoLand 工具配置的模板为 `vscode://file{path}`（scheme 不属于 GoLand 允许前缀），用户以 GoLand 打开 `/Users/me/proj`
- **THEN** 按内置 GoLand 分支构造 URI（`goland://open?file=%2FUsers%2Fme%2Fproj`）

#### Scenario: 保存空模板清除恢复内置

- **WHEN** VSCode 工具已配置自定义模板，用户在设置面板将模板清空并保存
- **THEN** `ocdeck.editorTools.uriTemplate.vscode` 键被清除，后续唤起走内置分支

#### Scenario: 模板写失败不生效

- **WHEN** 用户保存模板时 localStorage 抛异常
- **THEN** 错误向上抛出、不派发变更事件，快捷打开入口仍按旧模板或内置分支构造 URI

### Requirement: 自定义编辑器工具

系统 SHALL 允许用户在设置页「常用工具」子标签维护自定义编辑器工具（名称 + 唤起 URI 模板），localStorage 键 `ocdeck.editorTools.customTools`，值为 JSON 数组文本（元素 `{ id, name, template }`）。读取侧遇到 JSON 损坏、非数组或元素缺字段 MUST 跳过该元素或整项回空数组，MUST NOT 改写 localStorage。保存成功 MUST 派发变更事件（跨标签页经 storage 事件即时收敛）；写入失败 MUST 向上抛出且不派发。

自定义工具模板的安全边界 MUST 为：模板包含至少一个 `{path}` 占位符，且 scheme 匹配通用 URI scheme 语法（`^[a-zA-Z][a-zA-Z0-9+.-]*:`）且 scheme（大小写不敏感）不属于危险名单 `javascript:`、`data:`、`vbscript:`、`file:`。不满足条件的模板 MUST 按不可用处理：该工具不出现在快捷打开列表的可用集合中，设置面板保留原文（便于草稿修正）并给出提示；存储值 MUST NOT 因此被改写。

自定义工具的唤起 URI MUST 为：模板中全部 `{path}` 替换为模板路径注入值（与内置模板注入同一编码语义）；自定义工具 MUST NOT 回退到任何内置分支——不可用时点击即零副作用（不唤起、不写默认、仅关闭菜单）。

设置面板 SHALL 提供自定义工具的添加、逐行名称/模板编辑（失焦保存）、删除能力；名称为空或模板非法的行 MUST 给出提示（该行不进入快捷打开可用列表）。

#### Scenario: 自定义工具出现在快捷打开下拉

- **WHEN** 用户添加自定义工具（名称 `My Editor`、模板 `myapp://open?path={path}`）且名称模板齐全
- **THEN** 任务详情页下拉在内置工具之后按存储顺序列出 `My Editor`

#### Scenario: 下拉选择自定义工具并设为默认

- **WHEN** 用户在下拉中点击自定义工具 `My Editor`
- **THEN** 按其模板构造 URI 并同步唤起，`ocdeck.editorTools.defaultTool` 写入 `custom:<id>`，刷新后默认仍为该工具

#### Scenario: 非法自定义模板不唤起

- **WHEN** 自定义工具模板为 `javascript:alert(1){path}`（危险 scheme），用户在下拉中点击该工具
- **THEN** 不触发唤起、不写默认键，菜单关闭

#### Scenario: 自定义工具被删除后默认回退

- **WHEN** 存储默认为 `custom:<id>`，该自定义工具在另一标签页被删除
- **THEN** 主按钮按默认解析规则回退（内置启用顺序或其余自定义工具）
