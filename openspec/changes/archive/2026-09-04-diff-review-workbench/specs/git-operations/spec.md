# Delta Spec: git-operations

本 delta 为 `git-operations` 增加 diff 页面的「查看/编辑」两模式与文件直接编辑能力。

## MODIFIED Requirements

### Requirement: 文件 diff 查看
系统 SHALL 提供任务 worktree 单文件的两侧版本内容查询，供前端 CodeMirror merge 视图渲染。端点与请求参数形态不变（`GET /api/v1/tasks/{id}/git/diff?ref=&path=&untracked=`）；响应承载旧侧内容、新侧内容、两侧存在性标记、两侧 git mode、二进制标记与截断标记。

内容来源语义（唯一判定）：
- `untracked=1`：旧侧存在性=false；新侧为工作区文件内容。
- `ref` 非空：ref MUST 先经 `git rev-parse --verify --end-of-options` 解析为 OID（防 option 注入）；存在性探测 MUST 使用 `:(literal)` pathspec 包裹 path 并逐条核对返回记录的路径与对象类型：仅记录路径与请求 path 精确相等的条目参与判定——regular blob（mode 100644/100755）→ 存在，内容为该 blob；symlink（mode 120000）→ 存在，内容为该 blob（符号链接目标文本）；gitlink（mode 160000）→ 存在，内容为该记录的 commit OID 文本；目录（tree）等 MUST 按存在性=false 处理。path 不存在于该 ref → 旧侧存在性=false（正常结果，非错误）。blob 内容读取 MUST 以探测取得的 blob OID 为读取对象，MUST NOT 以 `<ref>:<path>` 形式二次拼路径读取。旧侧 mode MUST 取自探测记录。
- `ref` 为空：旧侧为 git index 中 path 的 stage-0 内容；存在性探测 MUST 使用 `:(literal)` pathspec 包裹 path 并逐条核对返回记录：仅记录路径与请求 path 精确相等且 stage 为 0 的条目参与判定——mode 为 100644/100755/120000/160000 时视为存在（内容读取规则同 ref 分支：blob 经 `git show <blobOID>`、gitlink 直接以记录 OID 为内容）；记录为空或路径不匹配 → 旧侧存在性=false；path 处于未解决冲突（无 stage-0 记录但存在同 path 其他 stage 记录）→ MUST 返回 invalid_state 明确错误，MUST NOT 以「旧侧不存在」降级展示。旧侧 mode MUST 取自探测记录。
- 新侧恒为工作区侧内容：path 防御校验（拒绝绝对路径、`..` 逃逸、NUL → invalid_input，与既有 untracked 路径校验同一规则）；以 `Lstat` 判定类型——ENOENT（含并发删除竞态）→ 新侧存在性=false；regular file → 候选读取对象，执行禁锢校验（resolve 后的真实路径 MUST 仍位于该任务 worktree 根内，防中间级 symlink 逃逸；越界 → invalid_input）后有界读取，mode 依 owner 执行位（0100）为 100644/100755；symlink → 存在，内容为 `Readlink` 目标文本（读取链接文本而非跟随；MUST 先校验该链接的 resolved parent 位于 worktree 根内——防中间级 symlink 逃逸，越界 → invalid_input；链接目标本身不受禁锢），mode=120000；directory → 按 gitlink 工作区侧处理：MUST 先校验 resolved 路径位于 worktree 根内（越界 → invalid_input）再执行任何 git 命令；存在，MUST 先以 `git -C <path> rev-parse --show-toplevel` 校验其 canonical 路径与目标目录一致——输出 MUST 仅去除 git 追加的行尾换行后做 canonical（EvalSymlinks）比较，MUST NOT 整体 TrimSpace（路径自身的首尾空白合法）；真实未初始化子模块会向上发现父仓库，MUST 视为未初始化而非返回 superproject HEAD——不一致或失败 → 内容为空字符串；一致 → 内容为 `git -C <path> rev-parse HEAD` 的 commit OID 文本，且以 `git -C <path> status --porcelain` 非空判定 dirty，dirty 时内容 MUST 以稳定 `-dirty` 后缀标记（对齐旧 unified diff 的 `Subproject commit <OID>-dirty` 显示语义）；dirty 探测执行失败 → MUST 返回 git_error 并透传 stderr，MUST NOT 静默按 clean 处理；mode=160000；其他非 regular 类型 → 新侧存在性=false。
- 任一侧存在性=false 时该侧内容与 mode MUST 为空字符串，前端按全部新增/全部删除渲染。

调用约定：`untracked` 查询参数值域为 absent / `0` / `1`，其他值 MUST 返回 invalid_input；`untracked=1` 时 ref MUST 为空，否则 MUST 返回 invalid_input；`untracked=1` 为调用方声明的展示模式，服务端 MUST NOT 二次探测文件是否确为 untracked。path MUST 非空——空 path（历史全仓 diff 形态）MUST 返回 invalid_input。参数的词法校验（untracked 值域与组合约束、path 非空、路径防御）MUST 在任何 git 命令与文件读取之前完成；ref 的语义解析（rev-parse）在词法校验全部通过后执行。

容量与二进制：
- 单侧内容处理管线 MUST 为唯一顺序：原始字节读取 → NUL 二进制嗅探 → UTF-8 规范化（`strings.ToValidUTF8`，非法序列替换为 U+FFFD）→ 规范化结果按 UTF-8 rune 边界限制至 524288 bytes。`truncated=true` 当且仅当原始读取超出读取上限**或**规范化结果因上限被裁短（替换扩张导致的裁短同样置位）；truncated 时该侧返回规范化后的有界前缀。
- 二进制判定 MUST 对两侧各自执行（原始内容前 8000 字节含 NUL → 二进制，与 status 行计数嗅探同一口径）；任一侧为二进制 → isBinary=true 且两侧内容 MUST 为空；truncated 仅表示上述容量裁短，MUST NOT 兼任二进制含义。
- 响应八字段（oldContent/newContent/oldExists/newExists/oldMode/newMode/isBinary/truncated）MUST 始终全部返回。派生规则唯一：`isBinary` = 任一侧二进制（mode 为 120000/160000 的侧 MUST NOT 参与二进制嗅探，其内容天然为文本）；`truncated` = 任一侧按上条真值表被裁短；二进制清空两侧内容但 MUST NOT 改变 `truncated` 取值。响应内容为 UTF-8 文本契约，MUST NOT 承诺字节级 round-trip。
- 前端渲染优先级唯一：isBinary（不支持提示）→ gitlink（任一侧 mode=160000 → 子模块变更提示，展示两侧 OID 文本，MUST NOT 渲染 merge 视图，MUST NOT 落入「不存在/无变更」）→ 双侧不存在（「文件已不存在」）→ 空文件（至少一侧存在且两侧内容均为空；mode 相同条件仅在两侧均存在时参与，一侧缺失时该侧 mode 恒为空串、不参与判定）→ 无变更（两侧均存在、truncated=false、内容与 mode 均相同）→ 截断范围内无可见差异（两侧均存在、truncated=true 且返回前缀相同）→ merge 视图。mode 变更横幅独立显示：两侧均存在且 mode 不同（如 100644→100755）时 MUST 显示权限/类型变更横幅，可与 merge 视图或状态提示同时出现；内容相同但 mode 不同 MUST NOT 显示「无变更」。截断横幅独立显示，可与任一状态提示或 merge 视图同时出现，不参与优先级。

错误映射（唯一）：词法非法 → invalid_input；候选 regular file 的真实路径逃逸 → invalid_input；未解决冲突 → invalid_state；ref 解析与旧侧 git 执行失败 → git_error 并透传 stderr；新侧非 ENOENT 的 IO 错误 → internal（消息含相对 path 与操作名）；ENOENT 竞态与其他非 regular 类型（fifo/socket 等）→ 新侧存在性=false（正常结果，优先于逃逸判定）；子模块 rev-parse 失败或 toplevel 校验不一致（未初始化等）→ 新侧存在性=true、内容为空、mode=160000（正常结果，非错误）；子模块 dirty 探测（status --porcelain）执行失败 → git_error 并透传 stderr。多源同时失败时 MUST 按固定执行顺序（词法校验 → ref 解析 → 旧侧读取 → 新侧读取）返回首个失败。旧侧内容读取经 git CLI 白名单子命令执行、输出有界；ref/index 侧存在性探测 MUST 使用无歧义判定（`:(literal)` 记录核对），MUST NOT 依赖 stderr 文案匹配。该端点为只读操作，MUST NOT 修改 git index 或工作区，MUST NOT 进入 repo 写锁。

#### Scenario: 查看修改文件 diff
- **WHEN** 用户选择某个已修改文件
- **THEN** 系统返回旧侧（对应 ref 或 index 版本）与新侧（工作区）内容，两侧存在性=true，前端渲染 merge 对比视图

#### Scenario: 查看未跟踪新文件 diff
- **WHEN** 用户选择 untracked 分组中的新文件
- **THEN** 旧侧存在性=false、内容为空，新侧为文件全文，前端渲染为全部新增行视图，且 git index 未被修改

#### Scenario: 查看空的新文件
- **WHEN** 用户选择 untracked 分组中的空文件
- **THEN** 旧侧存在性=false，新侧存在性=true 且内容为空，前端正常展示空文件状态

#### Scenario: 查看已删除文件 diff
- **WHEN** 用户选择的文件已在工作区删除
- **THEN** 新侧存在性=false、内容为空，旧侧为对应版本内容，前端渲染为全部删除行视图

#### Scenario: 查看冲突中的文件
- **WHEN** 用户选择的文件处于未解决冲突（index 无 stage-0 记录）且 ref 为空
- **THEN** 系统返回 invalid_state 明确错误，前端展示错误提示而非误导性 diff

#### Scenario: 非法参数组合
- **WHEN** 请求 `untracked=1` 且 ref 非空，或 untracked 取值不在值域内，或 path 为空
- **THEN** 系统返回 invalid_input 错误，且未执行任何 git 命令或文件读取

#### Scenario: 超大文件
- **WHEN** 文件任一侧内容超过 524288 bytes
- **THEN** 该侧返回规范化后的有界前缀并标记 truncated=true，前端显示截断提示，不冻结浏览器

#### Scenario: 非法 UTF-8 替换扩张裁短
- **WHEN** 文件原始读取未超上限，但非法 UTF-8 序列替换为 U+FFFD 后规范化结果超过 524288 bytes
- **THEN** 规范化结果按 rune 边界裁短并标记 truncated=true

#### Scenario: 二进制文件
- **WHEN** 文件任一侧为二进制
- **THEN** 系统返回 isBinary=true 且两侧内容为空，前端显示二进制不支持标记

#### Scenario: 权限位变更
- **WHEN** 文件仅 mode 发生变化（如 100644→100755），内容相同
- **THEN** 两侧内容与存在性正常返回、mode 不同，前端显示权限/类型变更横幅，MUST NOT 显示「无变更」

#### Scenario: 符号链接目标变更
- **WHEN** 符号链接（mode 120000）的目标路径发生变化
- **THEN** 两侧内容为链接目标文本（旧侧取 blob、新侧取 Readlink），前端以 merge 视图渲染目标差异

#### Scenario: 子模块（gitlink）变更
- **WHEN** 子模块（mode 160000）的 commit OID 发生变化（含工作区 dirty 产生的 `-dirty` 后缀变化）
- **THEN** 两侧内容为各自 commit OID 文本（新侧 dirty 时带 `-dirty` 后缀），前端显示子模块变更提示而非 merge 视图，MUST NOT 落入「不存在/无变更」

### Requirement: diff 视图渲染
diff 视图 SHALL 使用 CodeMirror 6 merge 组件渲染文件两侧版本内容。视图 SHALL 具有查看与编辑两种模式，默认查看模式；查看模式下视图为只读，编辑模式行为见「diff 文件直接编辑」。视图 SHALL 支持单列（unified）与并排（side-by-side）两种形态且用户 MUST 可切换；首次打开的默认形态按视口唯一确定：>1024px MUST 默认并排，≤1024px MUST 默认单列；用户手动选择 MUST 跨文件切换与视口变化保留，直至当前 Git 面板会话结束（不持久化）。代码行默认 MUST NOT 折行，超出视口宽度的长行横向滚动；视图 SHALL 提供「换行」切换控件，用户 MUST 可在横向滚动与自动折行之间切换，两种形态（单列/并排）均 MUST 生效；换行选择的保留规则与形态切换一致（跨文件切换与视口变化保留，Git 面板会话结束丢弃，不持久化）。行号范围唯一：并排形态两侧 MUST 各显示本侧行号；单列形态 MUST 显示当前文档行号，删除块 SHALL NOT 要求展示旧侧行号。视图 MUST 仅以 `\n` 为行分隔符（`\r` 保留为文档字符），仅换行符风格差异（如 CRLF↔LF）MUST 仍呈现为可见差异而非「无变更」。系统 SHALL 按文件扩展名加载对应语法高亮，语言包 MUST 按需懒加载；未识别的扩展名 MUST 降级为纯文本渲染而非报错。旧侧存在性=false 时 MUST 渲染为全部新增视图；新侧存在性=false 时 MUST 渲染为全部删除视图；两侧存在性均为 false 时 MUST 展示「文件已不存在」状态；任一侧 mode=160000 时 MUST 展示子模块变更提示（含两侧 OID 文本），MUST NOT 渲染 merge 视图或落入「不存在/无变更」；至少一侧存在、两侧内容均为空且（两侧均存在时 mode 相同）时 MUST 展示空文件状态；两侧均存在、内容与 mode 均相同且 truncated=false 时 MUST 展示「无变更」状态而非空 merge 视图；两侧均存在且 mode 不同时 MUST 显示权限/类型变更横幅（内容相同但 mode 不同 MUST NOT 显示「无变更」）；两侧均存在、truncated=true 且返回的有界前缀相同时 MUST NOT 展示「无变更」（真实尾部可能不同），MUST 展示「截断范围内无可见差异」并显示截断横幅。渲染优先级 MUST 与「文件 diff 查看」的派生规则一致。isBinary=true 时 MUST NOT 渲染 merge 视图，MUST 显示二进制不支持提示；truncated=true 时 MUST 显示截断提示横幅。diff 渲染 MUST NOT 使用 `dangerouslySetInnerHTML`。

#### Scenario: 切换 diff 形态
- **WHEN** 用户点击形态切换控件
- **THEN** 视图在单列与并排之间切换，diff 内容与可读性保持正常

#### Scenario: 窄屏默认单列
- **WHEN** 用户在视口宽度 ≤1024px 下打开文件 diff
- **THEN** 视图默认单列形态，且仍允许手动切换为并排

#### Scenario: 语法高亮渲染
- **WHEN** 用户打开已知扩展名（如 .go）文件的 diff
- **THEN** 代码按对应语言语法高亮，增删区域标记叠加生效

#### Scenario: 未识别文件类型
- **WHEN** 用户打开无已知扩展名文件的 diff
- **THEN** 视图按纯文本渲染，无报错

#### Scenario: 长行横向滚动
- **WHEN** 默认（未开启换行）状态下 diff 包含超视口宽度的长代码行，用户横向滚动
- **THEN** 行不折行，行号与代码行保持逐行对齐，无错位

#### Scenario: 切换换行展示
- **WHEN** 用户点击「换行」切换控件
- **THEN** 长代码行在当前形态（单列或并排）下自动折行展示，再次点击恢复横向滚动；切换查看其他文件或调整视口后选择保留

#### Scenario: 宽屏默认并排
- **WHEN** 用户在视口宽度 >1024px 下首次打开文件 diff
- **THEN** 视图默认并排形态，且仍允许手动切换为单列

#### Scenario: 用户形态选择保留
- **WHEN** 用户手动切换形态后，切换查看其他文件或调整视口宽度
- **THEN** 视图保持用户所选形态，直至 Git 面板会话结束

#### Scenario: 仅换行符差异可见
- **WHEN** 文件仅换行符风格发生变化（CRLF↔LF 或末尾换行变化）
- **THEN** 视图呈现可见差异，不显示「无变更」

#### Scenario: 两侧内容相同
- **WHEN** 两侧均存在、内容与 mode 均相同且 truncated=false
- **THEN** 视图显示「无变更」状态而非空 merge 视图

#### Scenario: 截断前缀相同
- **WHEN** 两侧均存在、返回的有界前缀相同但 truncated=true
- **THEN** 视图显示「截断范围内无可见差异」与截断横幅，MUST NOT 显示「无变更」

#### Scenario: 双侧不存在
- **WHEN** 两侧存在性均为 false（如查询期间文件被并发删除）
- **THEN** 视图显示「文件已不存在」状态

#### Scenario: 内容为空的已删除文件
- **WHEN** 用户选择工作区已删除且旧侧内容为空的文件（oldExists=true、newExists=false、两侧内容均为空）
- **THEN** 视图展示空文件状态（至少一侧存在、两侧内容均为空），不显示「无变更」

#### Scenario: 默认查看模式只读
- **WHEN** 用户打开文件 diff 且未显式进入编辑模式
- **THEN** 视图为查看模式，内容只读

## ADDED Requirements

### Requirement: diff 文件直接编辑
diff 视图 SHALL 提供查看与编辑两种模式，默认查看模式；编辑模式 MUST 由用户显式进入。编辑模式下用户 MUST 可直接修改新侧（工作区）内容；修改 SHALL 实时生效（无保存按钮、无脏状态概念），写回工作区文件。查看模式承担批注手势；编辑模式 MUST NOT 提供批注手势。

编辑能力 SHALL 仅对满足编辑读取契约的文件开放：服务端对文件做可编辑判定（regular file、有界大小、有效 UTF-8、换行风格统一为 LF 或 CRLF），并向客户端返回内容、检测出的换行风格与 BOM 有无指示；判定不通过的文件 MUST NOT 提供编辑入口，并显示明确原因。

写回保真：写回 MUST 按编辑会话冻结的换行风格（首次编辑读取检测值，CRLF 文件保持 CRLF）重建换行，并保持原文件的 BOM 有无与**完整权限位（含 setuid/setgid/sticky 特殊位，一个 bit 都不变）**；末尾换行状态以用户编辑内容为准（属可编辑内容，未触碰时自然保持原值）；MUST NOT 引入用户编辑之外的换行风格、编码或权限变化。写回请求内容 MUST 仅以 `\n` 为换行字符，含任何 `\r` 时 MUST 拒绝写回并返回 invalid_input（由编辑读取契约保证客户端提交的内容天然满足）。

冲突保护：写回 MUST 携带加载时的内容校验基线；写回时若文件相对基线已在外部变化（内容、权限 mode 或换行风格，如 agent 会话修改），系统 MUST 在写盘生效前（含临时文件写入后、原子 rename 前的最终复检）拒绝写回、保留用户编辑器内容并提示冲突，MUST NOT 静默覆盖外部修改或丢弃用户内容。最终复检与 rename 之间的极小残余窗口为已接受的残留风险。

agent 会话忙时 MUST 仍允许进入编辑模式，但编辑区 MUST 显示醒目警告横幅（提示 agent 正在修改代码、保存可能冲突被拒）。

#### Scenario: 进入编辑模式并修改
- **WHEN** 用户在查看模式下显式进入编辑模式并修改新侧内容
- **THEN** 修改实时写回工作区文件，diff 随之反映最新改动

#### Scenario: 写回保真
- **WHEN** 用户对 CRLF 换行的文件做局部编辑
- **THEN** 写回后文件保持 CRLF 风格与 BOM 状态，仅用户编辑的内容（含末尾换行，若用户改动）发生变化

#### Scenario: 外部变化拒绝覆盖
- **WHEN** diff 加载后文件被外部修改，用户编辑触发写回
- **THEN** 系统拒绝写回、保留用户编辑器内容并提示冲突，外部修改未被覆盖（最终复检与 rename 之间的极小残余窗口除外，为已接受的残留风险）

#### Scenario: 保存结果未知的恢复确认
- **WHEN** 写回请求因网络或服务内部错误导致结果未知（写可能已生效）
- **THEN** 客户端重新读取该文件：返回内容与该请求实际发送的内容相等、BOM 有无与编辑会话冻结值相等、（发送内容含换行时）换行风格与冻结值相等、权限 mode 与首次编辑读取值相等——全部相等才视为保存成功并采用新校验基线，任一不一致则保持冲突阻塞态；两种分支均不丢弃用户编辑器内容

#### Scenario: busy 时编辑带警告
- **WHEN** agent 会话正在执行任务时用户进入编辑模式
- **THEN** 编辑模式可用，编辑区显示醒目警告横幅

#### Scenario: 非 UTF-8 文件不可编辑
- **WHEN** 用户查看的文件不是有效 UTF-8 或换行风格混杂（含仅 CR 换行）
- **THEN** 系统不提供编辑模式入口，并显示明确原因

#### Scenario: 只读文件不可编辑
- **WHEN** 用户查看的文件无 owner 写位（如 mode 0444）
- **THEN** 系统不提供编辑模式入口，并显示明确原因

#### Scenario: 无换行文件可编辑
- **WHEN** 用户查看的文件内容不含任何换行符（单行文件）
- **THEN** 该文件按 LF 风格判定为可编辑，编辑模式入口正常提供

### Requirement: 编辑的特殊文件边界
编辑模式 SHALL 仅对满足全部条件的文件可用：渲染了 merge 视图、新侧存在、非 binary、非 truncated、新侧为 regular file（非 symlink/gitlink）、内容满足编辑读取契约（有效 UTF-8 且换行风格统一）、**文件带 owner 写位**。binary、truncated、gitlink、symlink、新侧不存在、非 UTF-8、换行风格混杂或**只读（无 owner 写位）**的文件 MUST NOT 提供编辑模式入口。

#### Scenario: 截断文件不可编辑
- **WHEN** 用户查看 truncated=true 的文件
- **THEN** 系统不提供编辑模式入口

#### Scenario: 符号链接不可编辑
- **WHEN** 用户查看 symlink（mode 120000）文件的 diff
- **THEN** 系统不提供编辑模式入口

#### Scenario: 已删除文件不可编辑
- **WHEN** 用户查看新侧不存在的已删除文件
- **THEN** 系统不提供编辑模式入口

### Requirement: 编辑还原入口
编辑模式下系统 SHALL 提供「还原」入口，将文件内容恢复为用户本次编辑会话开始前的内容快照。还原 MUST 经用户明确确认后执行。还原仅回退本次编辑会话内用户产生的改动，MUST NOT 影响该文件在编辑会话开始前已存在的改动。还原写回 MUST 遵守与「diff 文件直接编辑」相同的写回保真与冲突保护约束。

#### Scenario: 还原编辑会话改动
- **WHEN** 用户在编辑会话中做了若干修改后，确认执行还原
- **THEN** 文件恢复为编辑会话开始前的内容，编辑会话开始前的既有改动不受影响

#### Scenario: 还原需确认
- **WHEN** 用户点击还原入口
- **THEN** 系统先请求用户明确确认，确认前不执行还原
