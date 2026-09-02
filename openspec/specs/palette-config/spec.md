# palette-config Specification

## Purpose

命令面板（⌘K）的可配置能力：唤起热键、快速新建触发词、项目名匹配模式与指令触发词的后端持久化、读写 API 与设置页编辑，配置保存后运行时生效。

## Requirements

### Requirement: 命令面板配置存储

系统 SHALL 将命令面板配置持久化到应用数据目录下的配置文件（`<dataDir>/palette.json`）：写入 MUST 为临时文件 + 原子 rename，文件权限 MUST 为 0600。配置包含四个 camelCase 键，默认形状为 `{"hotkey":"mod+k","triggerWord":"new","matchMode":"exact-then-substring","commandTriggers":{...}}`（`commandTriggers` 形状与默认值见「命令面板配置读写 API」）。`hotkey` 的 wire 默认值为 `mod+k`（`⌘K / Ctrl+K` 仅为 UI 展示文案）；`triggerWord` 默认 `new`；`matchMode` 默认 `exact-then-substring`。

`matchMode` MUST 为以下枚举之一：
- `exact`：仅精确匹配（忽略大小写）。
- `exact-then-substring`：精确匹配优先，无精确命中时取唯一子串匹配（忽略大小写）。

`matchMode` 仅控制自动预选推断，命令面板候选列表始终使用 exact > prefix > substring 排序（不受 matchMode 影响）。

`triggerWord` 匹配是大小写不敏感的字面前缀匹配，MUST NOT 解释为正则；长度上限 32 按 Unicode code point 计数（前端与 Go 校验同一计数规则）。共享原语 `foldForMatch(s)` = ECMAScript `String.prototype.toLowerCase.call(s)` 的结果；MUST NOT 使用 `toLocaleLowerCase`、`Intl.Collator` 或 Unicode normalization。触发词解析固定为比较 `query.slice(0, triggerWord.length)` 的 fold 值与 triggerWord 的 fold 值是否 `===`；空白边界与余文切片使用原始 UTF-16 下标。空白字符集合统一定义为 ECMAScript WhiteSpace + LineTerminator 集合：U+0009–000D、U+0020、U+00A0、U+1680、U+2000–200A、U+2028、U+2029、U+202F、U+205F、U+3000、U+FEFF。触发词前缀解析、余文 trim 与 Go 触发词校验 MUST 共用该集合定义（Go 侧不得用 unicode.IsSpace 的更大集合）。

启动加载时文件不存在 MUST 视为正常未配置态（默认配置、无错误）；文件损坏/不可读/字段非法 MUST 降级为默认配置并记录告警日志，MUST NOT 拒绝启动。磁盘文件缺少 `commandTriggers` 键（旧版本写入）时，加载 MUST NOT 降级——按其余三键正常加载并复制默认词表，但与旧 `triggerWord` fold 后相同的默认项 MUST 置为空字符串（保自定义词，冲突指令暂不启用），其余默认项保留；磁盘文件含 `commandTriggers` 但非法（未知 ID/键不全/值非法/重复冲突）时按既有损坏语义降级默认配置并告警。配置变更 MUST 运行时生效，MUST NOT 要求重启服务。立即生效仅保证完成保存的当前 App 实例；其他已打开实例下次加载生效。App 在 TokenGate 期间已挂载，palette config GET 跟随认证生命周期：`authed=false` 时 MUST NOT 发起 palette config GET，使用默认快照；每次 `false → true` 转换 MUST 开启新代际并发起 GET；App 首次挂载且 `authed=true` 时 MUST 开启新代际并发起 palette config GET；`true → false` 转换 MUST 使在途 GET 失效并重置默认快照；旧代际的成功或失败 MUST NOT 覆盖重新认证后的代际。

#### Scenario: 首次启动无配置文件

- **WHEN** 服务启动且 `<dataDir>/palette.json` 不存在
- **THEN** 命令面板按默认配置运行（wire 默认热键 `mod+k`、触发词 `new`、匹配模式 `exact-then-substring`），服务正常启动

#### Scenario: 配置文件损坏降级

- **WHEN** 服务启动且 `<dataDir>/palette.json` 内容损坏或字段非法
- **THEN** 命令面板按默认配置运行，服务正常启动并记录告警日志

#### Scenario: 配置保存后运行时生效

- **WHEN** 用户通过设置页保存新的触发词或热键
- **THEN** 配置原子写入配置文件，无需重启服务。保存成功事件 MUST 携带 PUT 200 返回的完整 PaletteConfig（MUST NOT 包一层 `{ config }`）；仅 PUT 200 后派发，保存失败 MUST NOT 派发。App 同步应用该值并使此前在途 GET 代际失效；保存成功后加载失败提示消失并应用新配置；如需后台重拉，只允许最新代际写入 state，重拉失败保留 PUT 返回值（不回退默认、不恢复旧 loadError）；仅初始加载失败回退默认配置。立即生效仅保证完成保存的当前 App 实例；其他已打开实例下次加载生效。

#### Scenario: 已有有效 token 刷新页面加载持久化配置

- **WHEN** 本地已有有效 token 时刷新页面
- **THEN** App 挂载后加载后端持久化配置而非永久使用默认快照。App 首次挂载且 `authed=true` 时 MUST 开启新代际并发起 palette config GET

#### Scenario: TokenGate 认证成功后加载配置

- **WHEN** 首次无 token，App 已挂载并展示 TokenGate，随后用户保存有效 token（`false → true`）
- **THEN** `authed=false` 时 MUST NOT 发起 palette config GET，使用默认快照；每次 `false → true` 转换 MUST 开启新代际并发起 GET

#### Scenario: 401 后重新认证重新加载配置

- **WHEN** 已认证会话收到 401（`true → false`），随后用户再次通过 TokenGate 认证（`false → true`）
- **THEN** `true → false` 转换 MUST 使在途 GET 失效并重置默认快照；每次 `false → true` 转换 MUST 开启新代际并发起 GET；旧代际的成功或失败 MUST NOT 覆盖重新认证后的代际

### Requirement: 命令面板配置读写 API

系统 SHALL 提供 `GET /api/v1/palette/config` 返回当前生效配置，以及 `PUT /api/v1/palette/config` 校验并保存配置。GET 与 PUT 200 均返回 `{"hotkey":"mod+k","triggerWord":"new","matchMode":"exact-then-substring","commandTriggers":{...}}`；`commandTriggers` 为对象，键为指令 ID 枚举 `command-center|projects|settings-appearance|settings-env|settings-opencode|settings-ai|settings-palette|register-project`（恰 8 键，GET 返回全部 8 键，未启用的值为空字符串），值为该指令的触发词（空字符串 = 未启用）。默认值：`command-center:'cc'`、`projects:'pro'`、`register-project:'reg'`、其余 5 键为空字符串。PUT 四键全必填（`hotkey`、`triggerWord`、`matchMode`、`commandTriggers`）；`commandTriggers` 缺失、非对象、含未知指令 ID 键、键不全（须恰含 8 键）→ 422 `invalid_input`；非空值沿用 triggerWord 字符规则（非空、禁空白字符集合、≤32 Unicode code point）；非空值之间按 `foldForMatch` 比较不可重复、不可与全局 `triggerWord` 相同（违反 → 422）；值之间前缀重叠允许（解析按最长前缀优先）。成功保存后四键值为当前生效配置。空请求体、纯空白体、JSON 语法错误、尾随第二个 JSON 值均为 400 `invalid_input`；语法合法但顶层非对象（数组/字符串/数字等）、四键缺失/null/类型错误为 422 `invalid_input`；业务非法为 422 `invalid_input`；写盘失败为 500 `internal`；未知附加键按项目惯例忽略（统一错误结构见 `internal/api/errors.go:27`，先例见 `internal/api/notification_config.go:69`）。PUT MUST 拒绝非法配置并返回错误原因，MUST NOT 部分写入。

请求体上限为 4 KiB：`<=4096` 字节继续解码、`>4096` 字节返回 400；该 400 的错误信封 code 为 `invalid_input`；超限请求 MUST NOT 进入校验或文件写路径。

固定执行序：完整解码和校验 → 临时文件写入/关闭/rename → 内存快照替换；校验或保存失败 MUST NOT 创建有效新配置、修改旧文件或旧快照。

校验规则：
- `hotkey` MUST 为规范串。规范串为全小写，以 `+` 为分隔符。修饰键枚举精确为 `mod|meta|ctrl|alt|shift`，至少包含 `mod|meta|ctrl|alt` 之一，`shift` 不得单独成立；修饰键不得重复，`mod` 不得与 `meta`/`ctrl` 共存；组合串固定顺序为 mod,meta,ctrl,alt,shift，末尾恰有一个非修饰键，且该 key token 取值域限制为单个 `[a-z0-9]`（即 `mod+banana`、`mod++`、`mod+K` 均非法，非规范串 PUT 返回 422）。PUT 仅接受规范串，非规范串返回 422。`mod` 展开为 meta、ctrl、meta+ctrl 三种 modifier mask；运行时事件匹配按完整 mask 精确匹配（额外未配置修饰键按下即不匹配）。浏览器保留组合拒绝规则为「key ∈ t|w|n|q 且 mask 含 meta 或 ctrl（含 mod 展开后）」的超集拒绝。侧栏 ⌘B 冲突按当前实现语义判定：`key=b && !alt && !shift && (meta||ctrl)`（AppShell.tsx:63）。Go 校验与 TS 监听使用同一表驱动矩阵。
- `triggerWord` MUST 为非空、不含空白字符的字符串，长度上限 32 按 Unicode code point 计数（前端与 Go 校验同一计数规则）。空白字符集合统一定义为 ECMAScript WhiteSpace + LineTerminator 集合：U+0009–000D、U+0020、U+00A0、U+1680、U+2000–200A、U+2028、U+2029、U+202F、U+205F、U+3000、U+FEFF。触发词前缀解析、余文 trim 与 Go 触发词校验 MUST 共用该集合定义（Go 侧不得用 unicode.IsSpace 的更大集合）。前端对原始字符串校验非空、精确空白集合（ECMAScript WhiteSpace + LineTerminator）及 `Array.from(value).length <= 32`（Unicode code point 计数），MUST NOT trim、MUST NOT 规范化；本地校验失败 MUST NOT 调用 PUT，展示本地错误；后端 PUT 校验仍是最终裁决。触发词匹配是大小写不敏感的字面前缀匹配，MUST NOT 解释为正则。共享原语 `foldForMatch(s)` = ECMAScript `String.prototype.toLowerCase.call(s)` 的结果；MUST NOT 使用 `toLocaleLowerCase`、`Intl.Collator` 或 Unicode normalization。触发词解析固定为比较 `query.slice(0, triggerWord.length)` 的 fold 值与 triggerWord 的 fold 值是否 `===`；空白边界与余文切片使用原始 UTF-16 下标。
- `matchMode` MUST 为 `exact` 或 `exact-then-substring`。
- `commandTriggers` MUST 为恰含 8 个指令 ID 键的对象，值校验与冲突矩阵见本 requirement 上文（未知 ID/键不全/值非法/重复/与全局 `triggerWord` 相同 → 422 `invalid_input`）。

#### Scenario: 读取默认配置

- **WHEN** 未配置过时调用 `GET /api/v1/palette/config`
- **THEN** 返回 `{"hotkey":"mod+k","triggerWord":"new","matchMode":"exact-then-substring","commandTriggers":{...}}`（`commandTriggers` 全部 8 键：默认 `command-center:'cc'`、`projects:'pro'`、`register-project:'reg'`，其余 5 键为空字符串）

#### Scenario: 保存合法配置

- **WHEN** 以合法配置（规范串 `hotkey`、合法 `triggerWord`、合法 `matchMode`、合法 `commandTriggers`）调用 `PUT /api/v1/palette/config`
- **THEN** 配置持久化，PUT 200 与后续 GET 均返回同形 camelCase 四键的新配置。立即生效仅保证完成保存的当前 App 实例；其他已打开实例下次加载生效。

#### Scenario: 拒绝非法热键

- **WHEN** 以无修饰键的热键（如 `k`）、仅 `shift` 的热键、或非规范串（如 `mod+banana`、`mod++`、`mod+K`）调用 PUT
- **THEN** 请求被拒绝，返回 422 `invalid_input`，既有配置不变（MUST NOT 创建有效新配置、修改旧文件或旧快照）

#### Scenario: 拒绝浏览器保留组合与侧栏热键冲突

- **WHEN** 以「key ∈ t|w|n|q 且 mask 含 meta 或 ctrl（含 mod 展开后）」的超集拒绝命中的热键（如 `mod+t`、`meta+ctrl+t`），或以 `key=b && !alt && !shift && (meta||ctrl)` 命中侧栏冲突的热键（如 `mod+b`）调用 PUT
- **THEN** 请求被拒绝，返回 422 `invalid_input`，既有配置不变

#### Scenario: 拒绝非法触发词

- **WHEN** 以空串、含空白字符（含 NBSP、U+3000、U+FEFF）或超过 32 Unicode code point 的触发词调用 PUT
- **THEN** 请求被拒绝，返回 422 `invalid_input`，既有配置不变

#### Scenario: 拒绝缺失 null 或类型错误

- **WHEN** PUT 四键缺失、为 null、或类型错误
- **THEN** 请求被拒绝，返回 422 `invalid_input`，既有配置不变

#### Scenario: 拒绝空体纯空白语法错误或尾随值

- **WHEN** PUT 请求体为空请求体、纯空白体、JSON 语法错误、或尾随第二个 JSON 值，且字节数 `<=4096`
- **THEN** 返回 400 `invalid_input`，既有配置不变

#### Scenario: 拒绝顶层非对象

- **WHEN** PUT 请求体语法合法但顶层非对象（数组/字符串/数字等）
- **THEN** 返回 422 `invalid_input`，既有配置不变

#### Scenario: 拒绝畸形 JSON

- **WHEN** PUT 请求体为畸形 JSON 且字节数 `<=4096`
- **THEN** 请求被拒绝，返回 400 `invalid_input`，既有配置不变

#### Scenario: 拒绝超限请求体

- **WHEN** PUT 请求体 `>4096` 字节
- **THEN** 返回 400，错误信封 code 为 `invalid_input`；超限请求 MUST NOT 进入校验或文件写路径，既有配置不变

#### Scenario: 写盘失败不改旧快照

- **WHEN** 校验通过但写盘失败
- **THEN** 返回 500 `internal`，MUST NOT 创建有效新配置、修改旧文件或旧快照

#### Scenario: 未知附加键忽略

- **WHEN** PUT 请求体在 camelCase 四键之外携带未知附加键
- **THEN** 未知附加键被忽略；其余字段合法时保存成功并返回 camelCase 四键形状

### Requirement: 设置页命令面板配置子标签

系统 SHALL 在设置页提供「命令面板」子标签（深链 `#/configs#palette`），展示并编辑唤起热键、快速新建触发词、项目名匹配模式与指令触发词。设置页「命令面板」面板增加「指令触发词」小节：固定 8 行（指令名称只读 + 触发词输入框，空 = 未启用）；本地预校验规则与 PUT 校验同源（空字符串 = 未启用；仅非空值参与字符校验、fold 去重及与全局 `triggerWord` 的冲突比较），冲突时行内提示且 MUST NOT 调用 PUT；加载成功后 draft 以 canonical 值初始化（含全部 8 键）。采用 App 单一数据源：App 保存 `{ config, loadState/loadError }` 并经 props 传给 SettingsPage/PaletteConfigPanel；PaletteConfigPanel MUST NOT 独立发起 GET，只持有表单 draft；加载中禁止保存；加载成功后以 canonical 值初始化 draft；加载失败使用默认配置渲染并提示。文本输入框始终保存和展示 raw/canonical 文本（加载后填入 canonical 规范串，如 `mod+k`）；`formatHotkey` 仅用于输入框旁的只读预览与 AppShell 文案展示。默认只读预览与 AppShell 文案为 `⌘K / Ctrl+K`（对应 wire 默认值 `mod+k`）。设置页前端预校验 = `normalizeHotkey` + `validateCanonicalHotkey`，本地校验失败 MUST NOT 调用 PUT；后端 PUT 校验仍是最终裁决。前端对原始字符串校验非空、精确空白集合（ECMAScript WhiteSpace + LineTerminator）及 `Array.from(value).length <= 32`（Unicode code point 计数），MUST NOT trim、MUST NOT 规范化；本地校验失败 MUST NOT 调用 PUT，展示本地错误。保存失败 MUST 展示后端返回的错误原因；保存成功后立即生效仅保证完成保存的当前 App 实例；其他已打开实例下次加载生效。

#### Scenario: 深链直达命令面板子标签

- **WHEN** 用户打开 `#/configs#palette`
- **THEN** 设置页打开并直接选中「命令面板」子标签

#### Scenario: 修改触发词并保存

- **WHEN** 用户在「命令面板」子标签将触发词改为 `newtask` 并保存
- **THEN** 保存成功后命令面板以 `newtask` 作为快速新建触发词，原 `new` 触发词不再生效
