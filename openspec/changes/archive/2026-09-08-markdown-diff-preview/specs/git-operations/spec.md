# Delta Spec: git-operations（markdown-diff-preview）

## MODIFIED Requirements

### Requirement: diff 视图渲染
diff 视图 SHALL 使用 CodeMirror 6 merge 组件渲染文件两侧版本内容；markdown 文件（扩展名 `.md` / `.markdown`）例外——可按「markdown 渲染预览」需求以只读渲染预览替代 merge 视图。视图 SHALL 具有查看与编辑两种模式，默认查看模式；查看模式下视图为只读，编辑模式行为见「diff 文件直接编辑」。视图 SHALL 支持单列（unified）与并排（side-by-side）两种形态且用户 MUST 可切换（作用于源码模式）；首次打开的默认形态按视口唯一确定：>1024px MUST 默认并排，≤1024px MUST 默认单列；用户手动选择 MUST 跨文件切换与视口变化保留，直至当前 Git 面板会话结束（不持久化）。代码行默认 MUST NOT 折行，超出视口宽度的长行横向滚动；视图 SHALL 提供「换行」切换控件（作用于源码模式），用户 MUST 可在横向滚动与自动折行之间切换，两种形态（单列/并排）均 MUST 生效；换行选择的保留规则与形态切换一致（跨文件切换与视口变化保留，Git 面板会话结束丢弃，不持久化）。行号范围唯一：并排形态两侧 MUST 各显示本侧行号；单列形态 MUST 显示当前文档行号，删除块 SHALL NOT 要求展示旧侧行号。视图 MUST 仅以 `\n` 为行分隔符（`\r` 保留为文档字符），仅换行符风格差异（如 CRLF↔LF）MUST 仍呈现为可见差异而非「无变更」。系统 SHALL 按文件扩展名加载对应语法高亮，语言包 MUST 按需懒加载；未识别的扩展名 MUST 降级为纯文本渲染而非报错。旧侧存在性=false 时 MUST 渲染为全部新增视图；新侧存在性=false 时 MUST 渲染为全部删除视图；两侧存在性均为 false 时 MUST 展示「文件已不存在」状态；任一侧 mode=160000 时 MUST 展示子模块变更提示（含两侧 OID 文本），MUST NOT 渲染 merge 视图或落入「不存在/无变更」；至少一侧存在、两侧内容均为空且（两侧均存在时 mode 相同）时 MUST 展示空文件状态；两侧均存在、内容与 mode 均相同且 truncated=false 时 MUST 展示「无变更」状态而非空 merge 视图；两侧均存在且 mode 不同时 MUST 显示权限/类型变更横幅（内容相同但 mode 不同 MUST NOT 显示「无变更」）；两侧均存在、truncated=true 且返回的有界前缀相同时 MUST NOT 展示「无变更」（真实尾部可能不同），MUST 展示「截断范围内无可见差异」并显示截断横幅。渲染优先级 MUST 与「文件 diff 查看」的派生规则一致。isBinary=true 时 MUST NOT 渲染 merge 视图，MUST 显示二进制不支持提示；truncated=true 时 MUST 显示截断提示横幅。diff 渲染 MUST NOT 使用 `dangerouslySetInnerHTML`。

#### Scenario: 切换 diff 形态
- **WHEN** 用户在源码模式下点击形态切换控件
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
- **WHEN** 用户在源码模式下点击「换行」切换控件
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

### Requirement: markdown 渲染预览
对扩展名为 `.md` / `.markdown`（判定口径与语法高亮语言选择一致，复用同一扩展名提取规则）且处于 merge 渲染状态的 diff，系统 SHALL 在查看模式的工具栏提供「预览」开关，默认处于源码模式。开启预览后，视图 MUST 以只读渲染预览替代 CodeMirror merge 视图：两侧均存在时左右两栏分别渲染旧侧与新侧的完整返回内容（双侧等宽并分别标注「旧侧/新侧」），单侧存在（纯新增/纯删除）时仅渲染存在的一侧且该侧占满宽度。渲染 MUST 遵循 GFM（GitHub Flavored Markdown）语义，表格、任务列表、删除线、自动链接 MUST 正确渲染；渲染 MUST NOT 使用 `dangerouslySetInnerHTML`，markdown 源中的原始 HTML MUST NOT 被渲染。预览模式 MUST NOT 提供渲染级的行/段落差异高亮。预览选择 MUST NOT 跨文件切换保留、MUST NOT 持久化；切换文件后 MUST 回到源码模式。非 merge 渲染状态（二进制、子模块、文件已不存在、空文件、无变更、截断范围内无可见差异）MUST NOT 提供预览开关；truncated=true 的 diff MUST NOT 提供预览开关（即使派生状态为 merge）。URL 与外部请求策略：仅绝对 `http(s)` URL 有效——图片仅绝对 `http(s)` 可加载且使用 `loading="lazy"`，其他图片只显示 alt/占位且不得产生 `src`；链接仅绝对 `http(s)` 可点击并使用 `target="_blank"` + `rel="noopener noreferrer"`，相对路径、锚点、`mailto:` 等其他链接仅显示文本、不导航。外部图片请求仅在进入预览并挂载图片后发生；源码模式 MUST NOT 因 markdown 内容中的 URL 发起任何外部资源请求（应用自身的 API 请求与语言包 chunk 动态加载属既有行为，不在此限）。URL 策略检查 MUST 作用于渲染交付的最终 href/src 值（而非 markdown 原文目标）：以无 base 的 URL 解析判定协议，仅 http/https 协议通过；URL 缺失、为空或解析抛异常时 MUST 判定为不合格，判定函数 MUST NOT 向外抛错；www 前缀自动链接经 GFM 产出 http:// href，MUST 判定通过并可点击；邮箱自动链接产出 mailto: href，MUST 降级为纯文本；不合格链接 MUST 将整个后代树降级为纯文本等价物（其中图片显示 alt），任何后代 MUST NOT 导航或发起资源请求；不合格或加载失败的图片 MUST 统一渲染固定占位并显示 alt，MUST NOT 保留 img/src；图片加载失败状态 MUST 按最终 src 隔离，src 或内容变化后 MUST 允许重新加载。预览模式为只读，且满足状态不变量：预览模式 ⇒ 查看模式且无编辑会话（不存在「编辑中预览」状态）。预览模式下 MUST NOT 提供编辑入口、MUST NOT 提供源码行级批注手势、MUST NOT 展示源码行级批注标记；预览模式 SHALL 提供块级批注能力（行为契约见 diff-annotations「markdown 预览块级批注」需求）；编辑模式下 MUST NOT 提供预览开关。显式进入编辑与编辑偏好驱动的自动进入编辑 MUST 同时满足：非预览状态、merge 渲染状态、编辑门禁成立、资格为可编辑且持有有效编辑基线。预览期间完成的编辑资格预取 MUST NOT 触发进入编辑；diff 在同一视图内刷新后若编辑门禁或 merge 状态不再成立，MUST 作废缓存的资格结果与编辑基线；切回源码后若编辑偏好与资格仍成立，自动进入编辑恢复正常。进入预览 MUST 同时关闭未提交的批注草稿并清除跨侧选区提示，且 MUST NOT 清除编辑模式偏好；源码模式的批注瞬态 UI（草稿、候选高亮、跨侧选区提示）MUST 仅在非预览激活时显示；预览模式的块级批注草稿与块级徽章按 diff-annotations「markdown 预览块级批注」需求显示。预览模式不随单列/并排形态切换改变布局语义；预览态工具栏 MUST 仅保留「预览」开关，MUST NOT 显示单列/并排/换行控件；退出预览 MUST 恢复进入预览前的形态与换行偏好。预览模式 MUST 隐藏全部编辑相关控件与资格提示（含禁用编辑入口、资格检查中、重试、资格拒绝原因与进入编辑失败提示），编辑资格预取 MUST 继续在后台运行，退出预览后按当前资格恢复显示。预览资格谓词 canPreview = isMarkdown && state.kind === 'merge' && !diff.truncated && editPhase === 'view'；渲染激活谓词 previewActive = preview && canPreview。「预览」开关的出现条件 MUST 为 canPreview；开关的 aria-pressed、实际渲染分支、编辑器创建短路及相应 effect 依赖 MUST 使用 previewActive。diff 在同一视图内刷新（如同文件内容再拉取）导致资格失效时，视图 MUST 立即按源码/既有派生状态渲染并退出预览，后续资格恢复 MUST NOT 自动重进预览。权限/类型变更横幅在预览模式 MUST 照常显示（truncated=true 不得进入预览，截断横幅无预览态）。每侧预览 MUST 独立失败隔离：单侧渲染失败仅替换该侧为固定提示「渲染失败，请切回源码」，另一侧继续正常显示；切回源码再进入预览、切换文件或该侧内容变化时 MUST 重置并重试渲染。图片等网络加载失败不属于渲染失败，MUST 按固定占位与 alt 降级。任何渲染失败 MUST NOT 呈现空白视图。

#### Scenario: markdown 文件进入渲染预览
- **WHEN** 用户查看 `.md` 文件的 merge 状态 diff，点击查看模式工具栏的「预览」开关
- **THEN** 视图以左右两栏分别渲染旧侧与新侧内容的 GFM 效果（表格、任务列表、删除线、自动链接正确呈现），不渲染 merge 视图

#### Scenario: 预览模式只读
- **WHEN** diff 处于预览模式
- **THEN** 视图不提供编辑入口与源码行级批注手势，不展示源码行级批注标记（块级批注入口与徽章按 diff-annotations「markdown 预览块级批注」需求提供）

#### Scenario: 编辑与预览入口互斥
- **WHEN** diff 处于编辑模式，或 diff 处于预览模式
- **THEN** 编辑模式下不出现预览开关，预览模式下不出现编辑入口

#### Scenario: 预览中资格预取完成不进入编辑
- **WHEN** 用户已开启编辑模式偏好，在编辑资格预取在途期间进入预览，随后预取完成且结果为可编辑
- **THEN** 视图保持预览模式，不自动进入编辑；切回源码后资格与偏好仍成立时可正常进入编辑

#### Scenario: 单侧存在的 markdown 预览
- **WHEN** 用户预览一个纯新增（或纯删除）的 markdown 文件
- **THEN** 仅渲染存在的一侧（新增渲染新侧，删除渲染旧侧），该侧占满宽度

#### Scenario: 非 markdown 文件无预览开关
- **WHEN** 用户查看非 `.md` / `.markdown` 扩展名文件的 diff
- **THEN** 工具栏不出现预览开关，视图行为与既有源码模式一致

#### Scenario: 非 merge 状态无预览开关
- **WHEN** markdown 文件 diff 处于二进制、子模块、文件已不存在、空文件、无变更或截断范围内无可见差异状态
- **THEN** 不出现预览开关，维持既有状态提示

#### Scenario: 截断文件无预览开关
- **WHEN** markdown 文件 diff 的 truncated=true（无论派生状态是否为 merge）
- **THEN** 不出现预览开关，视图保持 canonical 源码状态与截断横幅；若派生状态为 merge，则显示源码 merge 视图

#### Scenario: 预览中资格失效退出预览
- **WHEN** 用户处于预览模式，diff 在同一视图内刷新后资格失效（如变为 truncated=true）
- **THEN** 视图立即按源码/既有派生状态渲染并退出预览；后续资格恢复时不自动重进预览

#### Scenario: 预览不跨文件保留
- **WHEN** 用户在预览模式下切换查看其他文件
- **THEN** 新文件 diff 回到源码模式

#### Scenario: 切回源码保留形态选择
- **WHEN** 用户从预览模式切回源码模式
- **THEN** 视图按用户原有的单列/并排形态与换行选择渲染 merge 视图

#### Scenario: 预览态工具栏仅保留预览开关
- **WHEN** diff 处于预览模式
- **THEN** 工具栏仅显示「预览」开关，不显示单列/并排/换行控件及编辑相关控件与资格提示；退出预览后恢复进入预览前的形态与换行偏好

#### Scenario: 自动链接 URL 判定
- **WHEN** 预览内容包含 www 前缀自动链接（如 www.example.com）与邮箱自动链接
- **THEN** www 自动链接按 http:// 判定通过、可点击并以新标签页安全打开；邮箱自动链接降级为纯文本、不导航

#### Scenario: markdown 中的原始 HTML 不渲染
- **WHEN** 预览的 markdown 内容包含原始 HTML（如 `<script>` 或内联标签）
- **THEN** 原始 HTML 不被渲染注入，视图无脚本执行风险

#### Scenario: 图片与链接 URL 策略
- **WHEN** 预览内容包含绝对 http(s) 图片、相对路径图片、绝对 http(s) 链接、相对路径或 mailto 链接
- **THEN** 绝对 http(s) 图片以 lazy loading 加载；相对路径图片仅显示 alt/占位且不产生 src 请求；绝对 http(s) 链接可点击并以新标签页安全打开；相对路径与 mailto 等链接仅显示文本、不导航

#### Scenario: 源码模式无外部请求
- **WHEN** markdown diff 处于源码模式（从未进入预览）
- **THEN** 视图不因 markdown 内容中的 URL 发起任何外部资源请求

#### Scenario: 单侧渲染失败隔离
- **WHEN** 预览模式下一侧内容渲染失败（解析异常）
- **THEN** 仅该侧替换为「渲染失败，请切回源码」提示，另一侧继续正常显示；切回源码再进入预览、切换文件或该侧内容变化后重试渲染

#### Scenario: 非法 URL 降级不抛错
- **WHEN** 预览内容包含空 URL、相对路径或解析抛异常的链接/图片
- **THEN** 判定为不合格且判定函数不向外抛错：链接仅显示文本，图片显示固定占位与 alt，整侧预览正常渲染不失败

#### Scenario: 图片加载失败按 src 隔离可重试
- **WHEN** 预览中的图片网络加载失败
- **THEN** 该图片按固定占位与 alt 降级显示，不触发渲染失败提示；该图片的 src 变化或内容更新后允许重新加载

#### Scenario: 编辑门禁失效不进入编辑
- **WHEN** 预览期间编辑资格预取完成且结果为可编辑，随后 diff 在同一视图内刷新使编辑门禁或 merge 状态不再成立
- **THEN** 视图不进入编辑模式，缓存的资格结果与编辑基线作废

#### Scenario: 进入预览清除批注瞬态 UI
- **WHEN** 查看模式存在跨侧选区提示或未提交的批注草稿，用户进入预览
- **THEN** 草稿关闭且跨侧选区提示清除，预览中不显示任何源码模式批注瞬态 UI

#### Scenario: 预览模式横幅保持
- **WHEN** markdown diff 两侧 mode 不同（权限/类型变更），用户进入预览模式
- **THEN** 权限/类型变更横幅照常显示
