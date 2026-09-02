# Design: quick-create-shortcut-support

## Context

命令面板现状（均已核实）：

- `web/src/components/CommandPalette.tsx`：`matchCommand` 纯函数模糊过滤（:24-32，按空白分词、每词须子串命中）；命令注册表 = 6 静态入口 + 任务列表 + 2 操作（:54-97）；「新建任务」action → `onNewTask()`（:78-86）。
- `web/src/App.tsx`：⌘K/Ctrl+K 监听硬编码（:74-84，`(metaKey||ctrlKey) && key==='k'`）；`onNewTask` = `navigate('/')` + `emitPaletteFocus('new-task-name')`（:129-134）。
- `web/src/palette-focus.ts`：焦点信号为纯字符串 id（`PaletteFocusId = 'new-task-name' | 'register-project-name'`，:11），CustomEvent + pending 兜底（:17-33）。
- `web/src/pages/CommandCenterPage.tsx`：消费 `new-task-name` 信号 → `openAndFocus()`（:234-244）；父层状态/prop 在 :227/:281（`focusTaskNameNonce` 及其传入 `NewTaskPanel`）；NewTaskPanel 内部状态在 :690（`projectQuery`/`selectedProject`/`taskName` 等）。
- 设置页：5 子标签（`web/src/router.ts:49-57` + `web/src/pages/SettingsPage.tsx:17-23`）；后端配置存储先例：`internal/infrastructure/notify/store.go`（`<dataDir>/notification.json`、0600 原子写、损坏降级默认不拒绝启动）。
- 全局热键全部硬编码，无配置机制；⌘B/Ctrl+B 为侧栏折叠（`web/src/components/AppShell.tsx:60-74`）；`AppShell.tsx:135/:139` 硬编码展示 ⌘K 文案（title 与可见徽标两处硬编码，实现时 MUST 同时动态化）。
- 组合根：项目未使用 Fx，组合根为 `cmd/ocdeck-server/main.go`。路由聚合点为 `internal/api/server.go:176`（`registerRoutes`）。同名项目允许存在（`internal/infrastructure/store/migrations/0001_init.sql:17` 仅约束 path 唯一）。

约束：proposal 已冻结需求边界（见 `proposal.md`）。spec 为规范源；design 中复述的行为矩阵仅为非规范性实现摘要，冲突时以 spec 为准。D 段保留接口签名、机制与 why。同一契约跨 artifact 的关键短语逐字一致。

## Goals / Non-Goals

**Goals:**

- 触发词快速新建链路：解析 → 候选排序 → 信号 → 面板初始化（预选项目 + 聚焦任务名）。
- 匹配/候选排序抽为可复用纯函数模块，供未来任务搜索、项目选择器等场景复用。
- 命令面板三项配置（热键/触发词/匹配模式）后端持久化 + 设置页编辑，运行时生效。

**Non-Goals:**（与 proposal 非目标一致，不重述）

## Decisions

### D1 触发词解析为独立纯函数

新增 `parseQuickCreateQuery(query, triggerWord): { projectQuery: string } | null`（与 `matchCommand` 同文件或独立模块，均为纯函数）。行为规范见 spec web-ui-shell「⌘K 全局命令面板」与 spec palette-config「命令面板配置存储」。实现用共享原语 `foldForMatch(s)` = ECMAScript `String.prototype.toLowerCase.call(s)` 的结果；MUST NOT 使用 `toLocaleLowerCase`、`Intl.Collator` 或 Unicode normalization。触发词解析固定为比较 `query.slice(0, triggerWord.length)` 的 fold 值与 triggerWord 的 fold 值是否 `===`；空白边界与余文切片使用原始 UTF-16 下标。空白边界判定与余文 trim 共用 spec 定义的 ECMAScript WhiteSpace + LineTerminator 集合。`query` 匹配成功时返回余文作为项目名；否则返回 `null`。仅输入触发词无尾随空白 → `null`，面板保持既有模糊匹配行为。

理由：解析规则是快速新建的入口契约，独立纯函数可单测且与过滤逻辑解耦；固定 `toLowerCase` 与原始 UTF-16 下标切片，避免 locale/normalization 导致前缀长度漂移。备选（在 matchCommand 内特殊分支）会把两种语义耦合进一个布尔函数，放弃。

### D2 快速新建模式的面板形态

触发词模式下 `CommandPalette` 渲染：置顶动态命令「新建任务」（始终存在、始终可执行，Enter 或点击触发快速新建），其下方为按 spec web-ui-shell「⌘K 全局命令面板」排序的项目候选列表；选中候选 = 以该项目执行快速新建。候选列表 MUST 读取现有 `useProjects()` 共享快照（hooks.ts:215-241（useProjects 位于 :222）），MUST NOT 新建订阅、轮询或为打开面板额外 GET；订阅/轮询失败后保留上次成功快照（既有 store 语义）；首次尚无成功快照时按空项目列表处理、仅显示置顶命令，文本 Enter 仍可执行。项目候选副文案展示项目路径（同名项目可视觉区分）。`classifyMatch` 空查询返回 null（判定层），`rankByQuery` 空查询返回全部项目按名称确定序（列表层例外）。非空查询排除不命中项。触发词模式 `new `（空余文）下置顶命令后展示全部项目候选，但 MUST NOT 自动预选。非空查询且排序结果为空时，展示全部项目作为候选，按同一名称确定序排序，点击候选仍携带 `projectID + projectName`。项目列表为空时仅展示置顶命令。Enter 触发语义与排序规则见 spec web-ui-shell「⌘K 全局命令面板」。自由文本 Enter 只传 `projectName`，候选点击同时传 `projectID` 与 `projectName`。

理由：置顶命令保证 Enter 在任何输入下都有确定行为；零命中时下方仍有按名称确定序的全部项目候选，满足用户确认的「没有匹配到时应该有个优先级列表」。不加额外匹配状态文案（用户已确认保持现有简单体验）。同名项目允许存在（path 唯一、name 不唯一），候选点击必须带 `projectID` 才能跨路由保住身份。

### D3 可复用匹配排序模块 `fuzzy-match.ts`

新增 `web/src/fuzzy-match.ts`，导出 `rankByQuery` / `classifyMatch`。候选排序与零命中 fallback 的行为规范见 spec web-ui-shell「⌘K 全局命令面板」；预选判定见 spec command-center「指挥中心内联新建任务」。实现用同一 `foldForMatch`：`classifyMatch`、`rankByQuery`、唯一匹配预选全部对 fold 后字符串执行 `===`、`startsWith`、`indexOf`。`classifyMatch` 空查询返回 null（判定层），`rankByQuery` 空查询返回全部项目按名称确定序（列表层例外）。`rankByQuery(items, '')` 是「排除不命中项」的唯一例外：空查询返回全部项目并按名称确定序排序；非空查询排除不命中项。名称确定序在 `foldForMatch(name)` 的结果上执行；比较其 UTF-16 code units：首个不同 UTF-16 code unit 较小者优先；共享部分完全相同（一方为另一方前缀）时长度较短者优先（前缀规则中的长度也是 fold 后字符串的 UTF-16 `.length`）；fold 后完全相同才按输入顺序稳定兜底；MUST NOT 使用原始名称或 `localeCompare`。该基准同时适用于正常排序、空查询全部项目与零命中 fallback 三处。

Lane A 冻结的最小 TypeScript 契约：

```ts
type MatchKind = 'exact' | 'prefix' | 'substring';
type MatchResult = { kind: MatchKind; index: number };

classifyMatch(name: string, query: string): MatchResult | null;
rankByQuery<T extends { name: string }>(items: readonly T[], query: string): T[];
```

`rankByQuery` MUST NOT 修改输入数组，返回原 item 引用的新数组。非空命中时 `index = foldForMatch(name).indexOf(foldForMatch(query))`，因此 exact/prefix 恒为 `0`、substring 为首个命中位置的 UTF-16 code-unit 下标；空查询或未命中返回 `null`（无 MatchResult）。

与现有 `matchCommand` 的关系：**并存**——`matchCommand` 继续承担普通模式的布尔过滤（含中文关键词语义），本模块承担触发词模式的项目排序与预选判定；不收敛两者（matchCommand 的多词语义与本模块的整段打分语义不同，强行统一会牺牲其一）。复用形态为纯函数模块，未来场景（任务搜索、项目选择器）直接引入即可。

### D4 焦点信号携带项目身份，预选判定在消费侧

`palette-focus.ts` 扩展 API（TypeScript）：

```ts
type PaletteFocusPayload =
  | { projectName?: undefined; projectID?: undefined }
  | { projectName: string; projectID?: string };

type PaletteFocusDetail = { id: PaletteFocusId } & PaletteFocusPayload;

emitPaletteFocus(id: PaletteFocusId, payload?: PaletteFocusPayload): void;
consumePendingPaletteFocus(expected: PaletteFocusId): PaletteFocusPayload | null;
```

冻结精确契约 `type PaletteFocusDetail = { id: PaletteFocusId } & PaletteFocusPayload`（扁平结构，MUST NOT 嵌套 `{id, payload}`）；三种合法 detail：`{id}`、`{id, projectName}`、`{id, projectName, projectID}`；pending 兜底与实时事件 MUST 共用同一 payload 归一函数。语义：pending 匹配且有 payload 返回该 payload，匹配但无 payload 返回 `{}`，无匹配返回 `null`（替换现有 boolean 返回）。既有调用方需同步迁移：`CommandCenterPage` 的 `'new-task-name'` 消费（`CommandCenterPage.tsx` 现以 boolean 调用 `consumePendingPaletteFocus('new-task-name')`）与 `ProjectsManagePage` 的 `'register-project-name'` 消费（`ProjectsManagePage.tsx` 现以 boolean 调用 `consumePendingPaletteFocus('register-project-name')`）。`projectID` MUST NOT 单独出现（合法生产路径只产生空对象、`{projectName}`、`{projectName, projectID}`）；消费侧收到仅有 `projectID` 的非法 detail 时 MUST 将其归一为 `{}`（等价无 payload），按普通无 payload `new` 语义处理（只展开聚焦、保持全部表单状态）。允许携带 projectName；候选点击同时携带 projectID 与 projectName；projectID 不单独出现。

自由文本 Enter 只传 `projectName`，候选点击同时传 `projectID` 与 `projectName`。消费优先级固定为：有效 `projectID` 直接选中；ID 已失效则回退文本匹配；文本匹配失败则填过滤词。推断结果不跨层传递，显式用户选择（候选点击）允许携带 `projectID`。

无有效 `projectID`（或 ID 已失效后的回退）时，预选判定在指挥中心消费侧执行：用 D3 `classifyMatch` 按 spec command-center「指挥中心内联新建任务」的匹配规则（含 `matchMode` 配置）计算预选项目；不预选时将项目名文本填入 `NewTaskPanel` 的项目过滤输入。消费侧只按信号到达时的快照判定预选，MUST NOT 因后续加载自动重试预选。`matchMode` 仅控制自动预选推断。`NewTaskPanel` 新增初始化 props（预选项目 ID / 项目过滤词初值），状态所有权仍在 `NewTaskPanel` 内部。

**已展开面板与连续信号**：父层保存带递增 nonce 的初始化意图，NewTaskPanel 对每个新 nonce 应用项目选择/过滤词并聚焦任务名；快速新建信号仅替换项目相关状态（预选或过滤词），MUST 保留用户已输入的 taskName；无参数 new（无 payload）只展开和聚焦，保持现有全部表单状态不变。

理由：同名项目允许存在，仅传项目名会在跨路由消费时丢失候选身份；`projectID` 只来自显式点击，推断结果仍留在消费侧、以打开时的项目列表快照判定。`CommandPalette` 的候选排序（D2/D3）与消费侧文本匹配使用同一模块。nonce 让已展开面板也能应用连续信号，且不擦除用户已输入的任务名。

### D5 后端配置存储平移 notify 模式

- 领域类型：`internal/domain/palette`（`Config{ Hotkey, TriggerWord, MatchMode string }` + 默认值 + 校验函数）。校验规则全文见 spec palette-config「命令面板配置读写 API」，此处不改写。
- Wire DTO：HTTP JSON 使用 camelCase 三键 `hotkey` / `triggerWord` / `matchMode`。GET 与 PUT 200 均返回 `{"hotkey":"mod+k","triggerWord":"new","matchMode":"exact-then-substring"}` 形状（camelCase 三键）；领域结构字段为 Go 导出名，adapter 负责 camelCase wire DTO 映射，领域层不以下划线 JSON tag 对外。
- 存储：`internal/infrastructure/palette/store.go`，结构平移 `internal/infrastructure/notify/store.go`：`<dataDir>/palette.json`、临时文件 + 原子 rename、0600、内存快照 + 写互斥、`LoadStore` 损坏降级默认配置 + 告警日志且不拒绝启动（spec palette-config「命令面板配置存储」）。
- 写路径执行序：完整解码和校验 → 临时文件写入/关闭/rename → 内存快照替换；校验或保存失败 MUST NOT 创建有效新配置、修改旧文件或旧快照。

理由：项目已有两套同构实现（ai、notify），平移先例比新设计风险低；配置体量小，无并发复杂度。camelCase 与前端 `api.ts` / 设置面板字段名对齐，避免前后端各转一层。

### D6 配置 API 与接线

`internal/api/palette_config.go`：`GET /api/v1/palette/config`、`PUT /api/v1/palette/config`（镜像 `internal/api/ai_config.go` / `internal/api/notification_config.go:69` 形态）。API 行为见 spec palette-config「命令面板配置读写 API」：空请求体、纯空白体、JSON 语法错误、尾随第二个 JSON 值均为 400 `invalid_input`；语法合法但顶层非对象（数组/字符串/数字等）、三键缺失/null/类型错误为 422 `invalid_input`；业务非法为 422 `invalid_input`；写盘失败为 500 `internal`；未知附加键按项目惯例忽略（统一错误结构见 `internal/api/errors.go:27`）。请求体上限为 1 KiB：`<=1024` 字节继续解码、`>1024` 字节返回 400；该 400 的错误信封 code 为 `invalid_input`；超限请求 MUST NOT 进入校验或文件写路径。PUT 仅接受规范串，非规范串返回 422。

项目未使用 Fx，组合根为 `cmd/ocdeck-server/main.go`。`internal/api/server.go:176` 为路由聚合点——新增 `Server.paletteConfig` 字段、`SetPaletteConfigStore`、`registerPaletteConfigRoutes` 与 server.go 注册调用；main.go 只负责 LoadStore 与注入，且保证在 RebuildRoutes 前完成。

理由：与 notify/ai 延迟注入 + `RebuildRoutes` 模式一致，避免在 `New()` 内隐式构造 store。

### D7 热键表示：规范化组合串 + `mod` 虚拟修饰键 + 共享纯函数

`hotkey` 存规范化小写串。wire 默认值为 `mod+k`（`⌘K / Ctrl+K` 仅为 UI 展示文案）。规范串为全小写，以 `+` 为分隔符。修饰键枚举精确为 `mod|meta|ctrl|alt|shift`，至少包含 `mod|meta|ctrl|alt` 之一，`shift` 不得单独成立；修饰键不得重复，`mod` 不得与 `meta`/`ctrl` 共存；组合串固定顺序为 mod,meta,ctrl,alt,shift，末尾恰有一个非修饰键，且该 key token 取值域限制为单个 `[a-z0-9]`（即 `mod+banana`、`mod++`、`mod+K` 均非法，非规范串 PUT 返回 422）。PUT 仅接受规范串，非规范串返回 422。`mod` 展开为 meta、ctrl、meta+ctrl 三种 modifier mask；运行时事件匹配按完整 mask 精确匹配（额外未配置修饰键按下即不匹配）。浏览器保留组合拒绝规则为「key ∈ t|w|n|q 且 mask 含 meta 或 ctrl（含 mod 展开后）」的超集拒绝；有限拒绝表为 ⌘T/⌘W/⌘N/⌘Q 及其 Ctrl 形式。侧栏 ⌘B/Ctrl+B 冲突按当前实现语义判定：`key=b && !alt && !shift && (meta||ctrl)`（`AppShell.tsx:63`）。Go 校验与 TS 监听使用同一表驱动矩阵。完整校验见 spec palette-config「命令面板配置读写 API」。

热键纯函数落点为共享 `web/src/hotkey.ts`（normalizeHotkey/validateCanonicalHotkey/matchHotkey/formatHotkey），供 App 和配置面板共同使用。`normalizeHotkey(raw: string): string | null` 只负责语法规范化：`normalizeHotkey` 的"无法规范化"判定集合精确为：按字面 `+` 分段、各段 ECMAScript trim、ASCII A-Z 小写后，存在空段（如 `mod++k`）或非修饰 token 数不等于 1（如 `k+x` 两个非修饰 token、`mod+` 零个）时返回 `null`；否则保留重复修饰键（去重与冲突检查不在本函数）、按固定顺序重排、追加唯一非修饰 token 返回规范串。`mod+banana` normalize 成功返回 `mod+banana`、validate 拒绝（key token 域）。`normalizeHotkey` MUST NOT 检查 key token 域（[a-z0-9]）、必需修饰键、重复修饰键、mod 与 meta/ctrl 冲突、保留组合或侧栏冲突——这些全部由 `validateCanonicalHotkey` 拒绝。另设 `validateCanonicalHotkey(canonical: string): string | null`（null 表示合法，否则返回错误原因）承担完整冲突矩阵（保留组合、⌘B 冲突、修饰键约束等 spec palette-config 校验规则）。设置页前端预校验 = `normalizeHotkey` + `validateCanonicalHotkey`，本地校验失败 MUST NOT 调用 PUT；后端 PUT 校验仍是最终裁决。另定义 `formatHotkey(canonical: string): string`：按规范串 token 顺序拼接展示文本。修饰键 token 映射表为 mod → `⌘ / Ctrl`、meta → `⌘`、ctrl → `Ctrl`、alt → `⌥` / `Alt`、shift → `⇧` / `Shift`；key token 大写展示。含 `mod` 时展示双平台形式（Mac 符号 / 文字）。不含 `mod` 时：含 meta 的用 Mac 符号，仅 ctrl/alt/shift 组合用文字形式。确定示例：`mod+k` → `⌘K / Ctrl+K`、`mod+shift+1` → `⌘⇧1 / Ctrl+Shift+1`、`meta+alt+k` → `⌘⌥K`、`alt+k` → `Alt+K`、`ctrl+alt+k` → `Ctrl+Alt+K`、`mod+alt+shift+k` → `⌘⌥⇧K / Ctrl+Alt+Shift+K`、`meta+ctrl+k` → `⌘Ctrl+K`。文本输入框始终保存和展示 raw/canonical 文本（加载后填入 canonical 规范串，如 `mod+k`）；`formatHotkey` 仅用于输入框旁的只读预览与 AppShell 文案展示。`App.tsx:74-84` 的硬编码判断替换为 `matchHotkey(e, hotkey)`。`matchHotkey` 从键盘事件提取 `eventToken` 后再与规范串末尾 key token 比较：字母取 `event.key.toLowerCase()` 的 ASCII 小写；数字优先从 `event.code` 的 `Digit0..Digit9` 提取，`event.code` 缺失时回退单个数字字符的 `event.key`；其他 key token 不匹配。Numpad 语义明确：`Numpad1` 不匹配 digit `1`。

理由：`mod` 让默认配置同时覆盖 Mac/Win 用户而无需存两份；规范化在保存时完成，运行时零解析歧义；共享模块避免 App 监听与设置页预校验各写一套规则。

### D8 前端配置加载与运行时生效

`api.ts` 增加 `getPaletteConfig` / `putPaletteConfig`。App 持有唯一配置快照，以 `matchMode` prop 传给 CommandCenterPage（同 CommandPalette/热键监听）；配置加载完成前使用默认值。不引入新 Context/全局状态抽象。采用 App 单一数据源：App 保存 `{ config, loadState/loadError }` 并经 props 传给 SettingsPage/PaletteConfigPanel；PaletteConfigPanel MUST NOT 独立发起 GET，只持有表单 draft；加载中禁止保存；加载成功后以 canonical 值初始化 draft；加载失败使用默认配置渲染并提示。`matchMode` 仅控制自动预选推断，命令面板候选列表始终使用 exact > prefix > substring 排序（不受 matchMode 影响）。App 在 TokenGate 期间已挂载（`App.tsx:52,:93`；`TokenGate.tsx:19`；`App.tsx:53` `useState(() => getToken() !== '')`），palette config GET 跟随认证生命周期：`authed=false` 时 MUST NOT 发起 palette config GET，使用默认快照；每次 `false → true` 转换 MUST 开启新代际并发起 GET；App 首次挂载且 `authed=true` 时 MUST 开启新代际并发起 palette config GET；`true → false` 转换 MUST 使在途 GET 失效并重置默认快照；旧代际的成功或失败 MUST NOT 覆盖重新认证后的代际。

设置页保存成功后：立即生效仅保证完成保存的当前 App 实例；其他已打开实例下次加载生效。机制：参照 `TERM_PREFS_CHANGED` 先例（`web/src/terminal/preferences.ts`），仅 PUT 200 后派发 `window.dispatchEvent(new CustomEvent('od:palette-config-changed', { detail }))`，保存失败 MUST NOT 派发。`detail` 直接为 PUT 200 返回的完整 `PaletteConfig` 对象（`type PaletteMatchMode = 'exact' | 'exact-then-substring'`；`type PaletteConfig = { hotkey: string; triggerWord: string; matchMode: PaletteMatchMode }` 与 wire 形状一致），MUST NOT 包一层 `{ config }`。PUT 200 成功事件 MUST 原子执行：应用 `event.detail` 为当前配置、使在途 GET 代际失效、将 loadState 置为成功/ready 并清空 loadError；后续后台重拉失败 MUST 保留该成功状态与 PUT 值（不回退默认、不恢复旧 loadError）。仅初始加载失败回退默认配置。事件名常量与 `PaletteConfig`/`PaletteMatchMode`/detail 类型属 Lane A 共享契约，并用于组件 props 与匹配函数签名。

理由：CustomEvent 先例已在项目内存在，避免引入新的全局状态库；保存成功以 PUT 200 返回值为准同步应用，后台重拉只允许最新代际写入，避免在途 GET 覆盖新配置。候选排序与预选推断分离，避免 `matchMode=exact` 把候选列表裁成只剩精确命中。

### D9 设置页子标签接入

- `web/src/router.ts:49-57`：`ConfigsTab` 与 `CONFIGS_TABS` 增加 `'palette'`。
- `web/src/pages/SettingsPage.tsx:17-23`：`TABS` 增加 `{ key: 'palette', label: '命令面板', ... }`，:79-108 区域增加对应 `tabpanel`。
- 新组件 `web/src/components/PaletteConfigPanel.tsx`：结构镜像 `NotificationConfigPanel`（受控表单 + 保存 + 错误展示）。采用 App 单一数据源：App 保存 `{ config, loadState/loadError }` 并经 props 传给 SettingsPage/PaletteConfigPanel；PaletteConfigPanel MUST NOT 独立发起 GET，只持有表单 draft；加载中禁止保存；加载成功后以 canonical 值初始化 draft；加载失败使用默认配置渲染并提示。三项配置：热键（文本录入 + 输入框旁只读预览 + 前端预校验，复用 `web/src/hotkey.ts`；文本输入框始终保存和展示 raw/canonical 文本（加载后填入 canonical 规范串，如 `mod+k`）；`formatHotkey` 仅用于输入框旁的只读预览与 AppShell 文案展示；设置页前端预校验 = `normalizeHotkey` + `validateCanonicalHotkey`，本地校验失败 MUST NOT 调用 PUT；后端 PUT 校验仍是最终裁决）、触发词（前端对原始字符串校验非空、精确空白集合（ECMAScript WhiteSpace + LineTerminator）及 `Array.from(value).length <= 32`（Unicode code point 计数），MUST NOT trim、MUST NOT 规范化；本地校验失败 MUST NOT 调用 PUT，展示本地错误；后端 PUT 校验仍是最终裁决）、匹配模式（二选 radio：`exact` / `exact-then-substring`）。
- `CommandPalette.tsx:55-62` 静态入口增加「设置 · 命令面板」`href: '/configs#palette'`，静态入口七项为指挥中心、项目管理、设置·终端外观、设置·环境变量、设置·opencode 配置、设置·AI 配置、设置·命令面板（明确静态入口不含通知子标签）。
- `AppShell.tsx:135/:139` 硬编码展示 ⌘K 文案（title 与可见徽标两处硬编码，实现时 MUST 同时动态化），展示文本 MUST 共用 `formatHotkey`。

### D10 测试策略

- 解析与排序
  - 纯函数单测：`parseQuickCreateQuery`（含空格项目名、仅触发词、非触发词前缀、大小写不敏感字面前缀、`triggerWord + 空白` 空余文、Unicode code point 长度、`foldForMatch`：非 ASCII 大小写如 `Ä`/`ä`、中文不影响、长度变化映射如 `İ`（U+0130，fold 后为 `i\u0307`，UTF-16 长度 1→2）不导致原始下标切片越界/误取余文）、`rankByQuery`/`classifyMatch`（档位优先级、同分确定性、大小写、候选排序不受 matchMode 影响、非 ASCII UTF-16 code-unit 顺序（如验证 `ä < 中 < 😀`）；`rankByQuery` MUST NOT 修改输入数组，返回原 item 引用的新数组；`classifyMatch` 空查询返回 null（判定层），`rankByQuery` 空查询返回全部项目按名称确定序（列表层例外）；非空查询排除不命中项；名称确定序在 `foldForMatch(name)` 的结果上执行；比较其 UTF-16 code units：首个不同 UTF-16 code unit 较小者优先；共享部分完全相同（一方为另一方前缀）时长度较短者优先（前缀规则中的长度也是 fold 后字符串的 UTF-16 `.length`）；fold 后完全相同才按输入顺序稳定兜底；MUST NOT 使用原始名称或 `localeCompare`；含 `foo` vs `foobar` 顺序与反序输入一致性，以及 `Afoo` / `aBar` 顺序与反序输入一致性（覆盖正常排序、空查询、fallback 三处的同一比较器）；`classifyMatch`、`rankByQuery`、唯一匹配预选全部对 fold 后字符串执行 `===`、`startsWith`、`indexOf`；非空命中时 `index = foldForMatch(name).indexOf(foldForMatch(query))`，因此 exact/prefix 恒为 `0`、substring 为首个命中位置的 UTF-16 code-unit 下标；空查询或未命中返回 `null`（无 MatchResult））、零命中 fallback 的排序（非空查询且排序结果为空时，展示全部项目作为候选，按同一名称确定序排序，点击候选仍携带 `projectID + projectName`）
  - jsdom 组件测试：`CommandPalette` 触发词模式（置顶命令、候选排序、Enter 执行、候选点击携带 projectID、`new zzzz` 无任何命中时展示全部项目候选、空余文展示全部项目候选且 MUST NOT 自动预选）
  - 补充矩阵：同名项目候选 projectID 传递
- 热键
  - 纯函数单测：`normalizeHotkey`/`validateCanonicalHotkey`/`matchHotkey`/`formatHotkey`（`normalizeHotkey(raw): string | null` 只负责语法规范化：`normalizeHotkey` 的"无法规范化"判定集合精确为：按字面 `+` 分段、各段 ECMAScript trim、ASCII A-Z 小写后，存在空段（如 `mod++k`）或非修饰 token 数不等于 1（如 `k+x` 两个非修饰 token、`mod+` 零个）时返回 `null`；否则保留重复修饰键（去重与冲突检查不在本函数）、按固定顺序重排、追加唯一非修饰 token 返回规范串；`normalizeHotkey` MUST NOT 检查 key token 域（[a-z0-9]）、必需修饰键、重复修饰键、mod 与 meta/ctrl 冲突、保留组合或侧栏冲突——这些全部由 `validateCanonicalHotkey` 拒绝；边界用例：`mod++k`/`mod+`/`k+x` normalize 返回 null；`mod+banana` normalize 成功返回 `mod+banana`、validate 拒绝（key token 域）；重复修饰键如 `mod+mod+k` normalize 成功但 validate 拒绝；`validateCanonicalHotkey(canonical: string): string | null`（null 表示合法，否则返回错误原因）承担完整冲突矩阵；设置页前端预校验 = `normalizeHotkey` + `validateCanonicalHotkey`，本地校验失败 MUST NOT 调用 PUT；后端 PUT 校验仍是最终裁决；「加载后不修改直接保存」往返（canonical 入框、原样 PUT 仍为合法规范串）与「乱序自由文本规范化后 PUT」（如输入 `K+Shift+Mod` 规范化为 `mod+shift+k`）；`formatHotkey` 仅用于输入框旁的只读预览与 AppShell 文案展示，示例：`mod+k` → `⌘K / Ctrl+K`、`mod+shift+1` → `⌘⇧1 / Ctrl+Shift+1`、`meta+alt+k` → `⌘⌥K`、`alt+k` → `Alt+K`、`ctrl+alt+k` → `Ctrl+Alt+K`、`mod+alt+shift+k` → `⌘⌥⇧K / Ctrl+Alt+Shift+K`、`meta+ctrl+k` → `⌘Ctrl+K`；mod 三 mask 展开与完整 mask 精确匹配、非法组合、key token 域、eventToken：字母取 `event.key.toLowerCase()` 的 ASCII 小写、数字优先从 `event.code` 的 `Digit0..Digit9` 提取且 `event.code` 缺失时回退单个数字字符的 `event.key`、其他 key token 不匹配、`mod+shift+1`（event.key='!' 时经 code 命中）、字母大小写、Numpad 边界：`Numpad1` 不匹配 digit `1`、有限拒绝表超集判定与 ⌘B 谓词判定，区分两算法的用例：`meta+shift+t` 拒绝、`meta+shift+b` 允许、`meta+ctrl+b` 拒绝）。
  - 补充矩阵：hotkey 表驱动枚举测试（含 `mod+t`、`meta+ctrl+t`、`mod+b`、`meta+shift+t` 拒绝、`meta+shift+b` 允许、`meta+ctrl+b` 拒绝、`mod+shift+1`（event.key='!' 时经 code 命中）、字母大小写、`Numpad1` 不匹配 digit `1`）
- 信号与面板
  - jsdom 组件测试：`NewTaskPanel` 初始化（预选 / 不预选填过滤词 / 提交门禁不绕过 / 已展开面板连续信号 nonce / 保留 taskName / 无 payload 只展开聚焦 / 实时事件与 pending 两条非法 detail（仅有 `projectID`）MUST 归一为 `{}` 并按普通无 payload `new` 语义处理；三种合法 detail `{id}` / `{id, projectName}` / `{id, projectName, projectID}` 与非法 `{id, projectID}`，镜像现有 command-center 测试模式）、`PaletteConfigPanel`（镜像 `web/src/__tests__/notification-settings.test.tsx`：mock `../api`，断言 `#panel-palette` 渲染与保存流；采用 App 单一数据源：面板不独立 GET、加载中禁保存、加载成功后以 canonical 值初始化 draft；热键文本输入框始终保存和展示 raw/canonical 文本（加载后填入 canonical 规范串，如 `mod+k`）；设置页前端预校验 = `normalizeHotkey` + `validateCanonicalHotkey`，本地校验失败 MUST NOT 调用 PUT；「加载后不修改直接保存」往返与「乱序自由文本规范化后 PUT」（如输入 `K+Shift+Mod` 规范化为 `mod+shift+k`）；triggerWord 前端校验：空串、NBSP、U+FEFF、32/33 code point，MUST NOT trim、MUST NOT 规范化）。
  - 补充矩阵：面板已展开/连续信号（nonce）、pending 实时与跨路由消费、register-project-name 链路回归
- API/store
  - Go 测试：`internal/infrastructure/palette` store 测试（不存在→默认、损坏→降级、原子写、落盘 JSON 精确键名测试（camelCase 三键，禁止 PascalCase 键）、`os.Stat(path).Mode().Perm()==0600` 断言）与 `internal/api` 校验拒绝测试，`t.TempDir()` 模式（先例：`internal/api/notification_config_test.go`）。
  - 补充矩阵：API 必填/null/422/500/`>1024` 字节 400 `invalid_input`/旧快照不变矩阵、空请求体/纯空白体/JSON 语法错误/尾随第二个 JSON 值为 400 `invalid_input`、语法合法但顶层非对象为 422 `invalid_input`、合法 JSON 补空白至恰好 1024 字节返回 200、1025 字节返回 400 且不进入校验/写盘
- App 生命周期
  - 补充矩阵：App 配置重载事件（`od:palette-config-changed` 的 `detail` 直接为 PUT 200 完整 PaletteConfig、MUST NOT 包一层 `{ config }`、仅 PUT 200 后派发、保存失败 MUST NOT 派发、App 直接消费 `event.detail`、deferred Promise 乱序返回只允许最新代际写入 state、保存后重拉失败保留 PUT 返回值（不回退默认、不恢复旧 loadError）、PUT 200 成功事件 MUST 原子执行：应用 `event.detail` 为当前配置、使在途 GET 代际失效、将 loadState 置为成功/ready 并清空 loadError、「GET 失败 → 默认配置提示 → PUT 成功 → 清除提示并应用 canonical 配置」）、认证生命周期（「首次无 token → TokenGate 保存 → 加载后端配置」：`authed=false` 时 MUST NOT 发起 palette config GET，使用默认快照，每次 `false → true` 转换 MUST 开启新代际并发起 GET；「已有有效 token 刷新页面加载持久化配置」：App 首次挂载且 `authed=true` 时 MUST 开启新代际并发起 palette config GET；「401 后重新认证重新加载」：`true → false` 转换 MUST 使在途 GET 失效并重置默认快照，旧代际的成功或失败 MUST NOT 覆盖重新认证后的代际）、SetPaletteConfigStore + RebuildRoutes 接线 smoke test
- 行为测试有效性：新增行为测试须在旧实现下失败、新实现下通过（实现者自检要求，见工作流）。

### 实施顺序（分 lane）

- Lane A：冻结 domain/wire contract 与纯函数（fuzzy-match.ts、hotkey.ts、domain/palette）；事件常量 `od:palette-config-changed` 与 `PaletteConfig`/`PaletteMatchMode`/detail 类型属 Lane A 冻结范围（`type PaletteMatchMode = 'exact' | 'exact-then-substring'`，并用于组件 props 与匹配函数签名）；`MatchKind` / `MatchResult` / `classifyMatch` / `rankByQuery` 签名（D3）属 Lane A 冻结范围。
- Lane B：后端 store/API/server/main。
- Lane C：前端 api/types/设置面板，可与 B 并行。
- Lane D：单一集成 lane 负责 App、CommandPalette、palette-focus、CommandCenterPage/NewTaskPanel、AppShell、`web/src/pages/ProjectsManagePage.tsx`（`register-project-name` 消费方迁移）。
- Lane E：回归与跨层测试；明确更新现有 boolean 返回断言的回归测试。

依赖：B/C 依赖 A，D 依赖 A/C，E 依赖 B/D。

## Risks / Trade-offs

- [触发词与正常搜索互相干扰] → 仅「触发词 + 空白」前缀才进入快速新建模式；触发词校验禁止含空白与超长（spec palette-config）。
- [面板候选排序与面板预选判定不一致] → 两侧复用同一 `fuzzy-match.ts` 模块（D3/D4）；候选列表始终 exact > prefix > substring，`matchMode` 仅控制自动预选推断。
- [配置加载失败导致面板不可用] → 前端默认配置降级 + 后端损坏降级默认（spec palette-config「命令面板配置存储」）。
- [palette-focus payload 扩展影响既有 `register-project-name` 链路] → `consumePendingPaletteFocus` 由 boolean 改为 `PaletteFocusPayload | null`；调用方迁移点见 D4（`CommandCenterPage` 的 `'new-task-name'` 与 `ProjectsManagePage` 的 `'register-project-name'`）；测试覆盖既有链路回归。
- [同名项目跨路由丢失身份] → 候选点击同时传 projectID 与 projectName；消费优先级见 D4 / spec command-center。
- [立即生效仅保证完成保存的当前 App 实例；其他已打开实例下次加载生效] → CustomEvent 方案不同步其他已打开浏览器标签页。
- [AppShell 硬编码 ⌘K] → `AppShell.tsx:135/:139` title 与可见徽标两处硬编码，实现时 MUST 同时动态化，展示文本 MUST 共用 `formatHotkey`（D7/D9）。
- [路由 spec 既有漂移（`#/configs` 深链枚举缺 `#notifications`）] → 本 change 的 MODIFIED 需求一并修正为六项枚举（spec web-ui-shell「路由收敛与旧链重定向」）。

## 主要实现落点

| 模块 | 文件 | 职责 |
| --- | --- | --- |
| 触发词解析 | `web/src/components/CommandPalette.tsx` | `parseQuickCreateQuery`、快速新建模式渲染与执行 |
| 匹配排序 | `web/src/fuzzy-match.ts`（新增） | `rankByQuery` / `classifyMatch` 可复用纯函数 |
| 热键纯函数 | `web/src/hotkey.ts`（新增） | `normalizeHotkey` / `validateCanonicalHotkey` / `matchHotkey` / `formatHotkey`，供 App 与配置面板共用 |
| 焦点信号 | `web/src/palette-focus.ts` | `PaletteFocusDetail` 扁平结构；`projectID` MUST NOT 单独出现 |
| 面板初始化 | `web/src/pages/CommandCenterPage.tsx` | 消费 payload、预选判定、nonce、`NewTaskPanel` 初始化 props；`matchMode` 由 App 以 prop 传入；只按信号到达时的快照判定预选 |
| 注册项目消费 | `web/src/pages/ProjectsManagePage.tsx` | `'register-project-name'` 消费方迁移（boolean → payload） |
| 热键监听 | `web/src/App.tsx` | 持有唯一配置快照 `{ config, loadState/loadError }`；监听改读配置；将配置经 props 传给 CommandPalette / CommandCenterPage / AppShell / SettingsPage / PaletteConfigPanel |
| 侧栏文案 | `web/src/components/AppShell.tsx` | `:135/:139` title 与可见徽标两处硬编码改为 `formatHotkey` 展示文本 |
| 配置面板 | `web/src/components/PaletteConfigPanel.tsx`（新增）+ `SettingsPage.tsx` + `router.ts` | 设置页子标签；MUST NOT 独立发起 GET，只持有表单 draft |
| 后端存储 | `internal/domain/palette` + `internal/infrastructure/palette`（新增） | Config 类型/校验 + 文件存储 |
| 后端 API | `internal/api/palette_config.go`（新增） | GET/PUT 与 camelCase wire DTO |
| 路由聚合 | `internal/api/server.go:176` | `Server.paletteConfig`、`SetPaletteConfigStore`、`registerPaletteConfigRoutes` 与注册调用 |
| 组合根 | `cmd/ocdeck-server/main.go` | 只负责 LoadStore 与注入，且保证在 RebuildRoutes 前完成 |

## Open Questions

无。
