# Design: markdown-diff-preview

## Context

ocdeck 的 git review 工作台中，`DiffViewer`（`web/src/components/diff/DiffViewer.tsx`）用 CodeMirror 6 merge view 渲染所有文本文件的 diff，markdown 文件仅有语法高亮（`loadLanguage` 映射 `.md`/`.markdown` → `@codemirror/lang-markdown`，`web/src/components/editor/language.ts:17-18`），无法阅读渲染后的排版。本变更为 markdown 文件增加只读的渲染预览模式。

现状关键结构（实现落点的代码锚点）：

- `deriveDiffState`（DiffViewer.tsx:101）是渲染优先级唯一链：binary → gitlink → 双侧不存在 → 空文件 → 无变更 → 截断范围内无可见差异（truncated 且前缀相同）→ merge 视图。仅 `state.kind === 'merge'` 时存在内容视图与工具栏（:702-731）。
- 视图形态 `mode: 'unified' | 'side-by-side'` 由 `modeOverride`/`onModeChange` props 控制（:76-77, :188-190），属 GitPanel 管理的用户偏好；换行同理（:78-79）。
- 编辑模式 `editPhase: 'view' | 'edit'`（:159）；批注手势仅在 view 模式且挂了 `onCreateAnnotation` 时可用（:228, :592-602）。
- diff 内容样式集中在 `web/src/legacy-components.css`（`.diff-view` 起于 :1085）。
- 测试基建：Vitest 2 + React 18，既有测试在 `web/src/__tests__/`（如 `diff-viewer.test.tsx`、`diff-language.test.ts`）。

**前置基线修复（canonical ↔ 实现漂移收敛）**：canonical spec「diff 视图渲染」要求代码行默认不折行（spec.md:94），但实现 `GitPanel.tsx:75` `wrapOverride` 初值为 `true`（默认换行开），`web/src/__tests__/git-panel-diff.test.tsx:157` 亦断言默认换行开启。本变更将实现收敛到 canonical：`wrapOverride` 初值改为 `false` 并同步更新该测试断言。这是既有行为修复而非本变更新增语义；Goals 中「源码模式行为完全不变」指收敛后的源码行为不再被预览改动。

## Goals / Non-Goals

**Goals:**

- `.md` / `.markdown` 文件在 diff 视图中可切换到「渲染预览」模式：左右两栏各自渲染该侧完整内容的 GFM 效果。
- 渲染安全：不使用 `dangerouslySetInnerHTML`，markdown 中的原始 HTML 不被注入渲染。
- 源码模式行为（高亮、编辑、行级批注、单列/并排/换行）完全不变。
- 预览模式支持块级批注：锚定渲染块的源码行范围，复用既有批注数据契约。
- 非 markdown 文件的 diff 展示与批注创建行为完全不变（提交 payload 的统一省略规则对所有文件类型生效，见 D9）。

**Non-Goals:**

- 渲染级的行/段落差异高亮（变更对比仍由源码模式承担）。
- 其他格式渲染预览（HTML/SVG/图片等）。
- 后端 diff 接口与 `GitDiffDTO` 契约变更。
- 无后缀文件（README、CHANGELOG 等）的 markdown 识别。
- 与 GitHub 页面像素级一致的渲染效果。

## Decisions

### D1：渲染库选型 — react-markdown + remark-gfm

选用 `react-markdown`（v10）+ `remark-gfm`。

- **安全性**：react-markdown 将 markdown 语法树构建为 React 元素树，官方明确 "safe by default (no dangerouslySetInnerHTML or XSS attacks)"；markdown 中的原始 HTML 默认不被渲染（启用需显式引入 `rehype-raw`，本设计不引入），满足 spec 对 HTML 注入的禁令。
- **GFM 覆盖**：react-markdown 基线为 CommonMark，GFM（表格、任务列表、删除线、自动链接）由 `remark-gfm` 插件提供，满足 proposal 确认的 GFM 语义。
- **备选**：`marked` + DOMPurify 方案输出 HTML 字符串、需 `dangerouslySetInnerHTML`，与 spec 禁令冲突，排除。

用法形态：`<ReactMarkdown remarkPlugins={[remarkGfm]} skipHtml>{content}</ReactMarkdown>`（`skipHtml` 显式忽略原始 HTML，双保险）。

**URL 与外部请求策略（DR-1，用户已确认；行为规范见 spec「markdown 渲染预览」，逐字一致）**：仅绝对 `http(s)` URL 有效——图片仅绝对 `http(s)` 可加载且使用 `loading="lazy"`，其他图片只显示 alt/占位且不得产生 `src`；链接仅绝对 `http(s)` 可点击并使用 `target="_blank"` + `rel="noopener noreferrer"`，相对路径、锚点、`mailto:` 等其他链接仅显示文本、不导航。外部图片请求仅在进入预览并挂载图片后发生；源码模式 MUST NOT 因 markdown 内容中的 URL 发起任何外部资源请求（应用自身的 API 请求与语言包 chunk 动态加载属既有行为，不在此限）。已接受风险：远程图片加载会使第三方获知客户端 IP 等网络信息。

**URL 判定层（唯一口径；以下契约句与 spec「markdown 渲染预览」逐字一致）**：URL 策略检查 MUST 作用于渲染交付的最终 href/src 值（而非 markdown 原文目标）：以无 base 的 URL 解析判定协议，仅 http/https 协议通过；URL 缺失、为空或解析抛异常时 MUST 判定为不合格，判定函数 MUST NOT 向外抛错；www 前缀自动链接经 GFM 产出 http:// href，MUST 判定通过并可点击；邮箱自动链接产出 mailto: href，MUST 降级为纯文本；不合格链接 MUST 将整个后代树降级为纯文本等价物（其中图片显示 alt），任何后代 MUST NOT 导航或发起资源请求；不合格或加载失败的图片 MUST 统一渲染固定占位并显示 alt，MUST NOT 保留 img/src；图片加载失败状态 MUST 按最终 src 隔离，src 或内容变化后 MUST 允许重新加载。机制：由自定义 `a`/`img` 渲染组件执行该策略（react-markdown `components` 覆盖）。测试要求：相对 URL、空 URL、非法 URL、嵌套图片链接（`[![alt](https://host/img)](./relative)` 不得保留远程图片请求）、图片 src 变化后重试。

### D2：markdown 判定复用 `extractExtension`

复用 `web/src/components/editor/language.ts:9` 的 `extractExtension(path)`：返回值 ∈ `{'.md', '.markdown'}` 即为 markdown 文件。与语法高亮的文件识别口径一致，不引入第二套判定。

### D3：预览模式是 diff 视图的第三种展示形态，独立于单列/并排

`DiffViewer` 新增本地状态 `preview: boolean` 与两个派生谓词（唯一口径，各处逐字一致）：

- 谓词契约（与 spec「markdown 渲染预览」逐字一致）：预览资格谓词 canPreview = isMarkdown && state.kind === 'merge' && !diff.truncated && editPhase === 'view'；渲染激活谓词 previewActive = preview && canPreview。「预览」开关的出现条件 MUST 为 canPreview；开关的 aria-pressed、实际渲染分支、编辑器创建短路及相应 effect 依赖 MUST 使用 previewActive。（资格否决见 D6）

「预览」开关按钮 `aria-pressed` 语义与现有按钮一致：

```
                  ┌────────────── 源码模式（默认）──────────────┐
  .md/.markdown   │  单列 / 并排（既有 modeOverride 语义不变）   │
  且 merge 状态 ──┤                                            │
                  └────────────── 预览模式（previewActive）──────┘
                                    │
              ┌─────────────────────┼─────────────────────┐
              ▼                     ▼                     ▼
        两侧均存在              单侧存在               （非 merge 状态
        左:渲染 oldContent      仅渲染存在侧            不出现预览开关，
        右:渲染 newContent      （新增→new，删除→old）   维持既有提示）
```

- 预览态工具栏契约（与 spec 逐字一致）：预览态工具栏 MUST 仅保留「预览」开关，MUST NOT 显示单列/并排/换行控件；退出预览 MUST 恢复进入预览前的形态与换行偏好。机制：单列/并排/换行语义仅存在于源码模式，两者偏好本就由 GitPanel 持有，天然保留。预览模式下左右两栏为各自独立的渲染结果，**不**跟随单列/并排形态切换。
- 单侧存在（纯新增/纯删除）时只渲染存在的一侧。
- 预览开关不持久化、不提升为 prop：切文件（key 变化）后回默认源码模式。理由：预览是临时阅读动作，且批注/编辑等主交互都在源码模式。

### D4：预览模式为只读，编辑与源码行级批注不进入预览

**状态不变量**：`preview === true` ⇒ `editPhase === 'view'` 且 `sessionRef.current === null`（不存在「编辑中预览」状态组合）。仅靠隐藏按钮不足以保证该不变量（F2 证据：DiffViewer.tsx:401-407 的自动进入编辑 effect 在 `editModePreferred && eligibility === 'eligible'` 时异步触发，GitPanel.tsx:76 跨文件保留编辑偏好，资格请求可能在用户进入预览后才完成）：

- 自动进入编辑 effect 与 `enterEdit()` 均 MUST 以 `preview === false` 为前置条件；完整前置集（契约句与 spec 逐字一致）：显式进入编辑与编辑偏好驱动的自动进入编辑 MUST 同时满足：非预览状态、merge 渲染状态、编辑门禁成立、资格为可编辑且持有有效编辑基线。机制映射（非契约句）：非预览状态=`preview === false`、merge 渲染状态=`state.kind === 'merge'`、编辑门禁成立=`gate.ok`、资格可编辑=`eligibility === 'eligible'`、有效编辑基线=`firstReadRef` 非空。自动 effect 依赖至少包含 `preview`、`gate.ok`、`state.kind`（证据：DiffViewer.tsx:375-383 门禁不成立时直接返回、不清空旧 `eligibility`/`firstReadRef`；:386-388 的 `enterEdit()` 不校验 `gate.ok`/`state.kind`；:401-407 的自动 effect 不依赖 `preview`）。
- diff 刷新后若编辑门禁或 merge 状态不再成立，MUST 作废缓存资格与基线，唯一动作：作废请求代际（`eligibilitySeq.current++`）、`firstReadRef.current = null`、`setEligibility('checking')`，并清空 `deniedReason`/`enterError`；不得新增 `eligibility` 枚举取值（MUST NOT 沿用旧资格进入编辑）。
- 进入预览仅当 `editPhase === 'view'` 可用（含于 `canPreview`）；编辑模式下不出现预览开关；预览模式中「编辑」按钮不渲染。
- 预览态隐藏全部编辑相关控件与资格提示（禁用「编辑」、「检查中…」、「重试」、资格拒绝原因条、进入编辑失败条），但资格预取继续在后台运行；退出预览后按当前资格状态恢复显示。
- 进入预览 MUST 同时执行 `closeDraft()` 与 `setCrossSideHint('')`（证据：DiffViewer.tsx:219-225 的 `closeDraft()` 不清除 `crossSideHint`，:156,834-837 的提示在查看模式持续显示）；全部源码模式批注瞬态 UI（草稿、候选高亮、跨侧选区提示）受 `!previewActive` 约束；MUST NOT 清除 `editModePreferred`。
- 切回源码后，若 `editModePreferred` 与资格仍成立，既有自动进入编辑 effect 自然恢复（无需额外状态）。
- 测试要求：「预览中资格请求完成不得进入编辑」「预览中 eligible → 刷新为 truncated/非 merge → 不进入编辑且资格缓存作废」「进入预览清除批注瞬态 UI」三个竞态/切换场景。

预览模式不创建 CodeMirror 编辑器实例，因此源码行级批注手势、gutter 标记、内联批注区在预览中不可用；预览模式提供块级批注（D8），锚定渲染块的源码行范围。切回源码模式后全部批注标记照常显示。

### D5：渲染组件与样式落点

- 新增 `web/src/components/diff/MarkdownPreview.tsx`：受控组件，核心 props 为 `{ content: string }`，内部以 D1 用法渲染；批注能力按 D8 的受控接口扩展。组件本身不感知 diff 上下文（path/ref/untracked 由 owner 侧处理），便于单测。
- `DiffViewer` 在 `previewActive` 分支渲染两个（或单侧一个）`MarkdownPreview`，替代 CodeMirror 容器（`containerRef` 分支不渲染，编辑器创建 effect 已有 `state.kind !== 'merge'` 短路，需同步加 `previewActive` 短路且依赖数组包含 `previewActive`）。
- 样式加在 `web/src/legacy-components.css` 既有 `.diff-view` 区块附近，限定于 `.md-preview` 作用域（标题/列表/表格/代码块/引用等 GFM 元素的基础排版），颜色沿用 design-system 变量，不引入新主题。
- 布局断言：双侧预览等宽并分别标注「旧侧/新侧」，单侧存在时该侧占满宽度；预览列使用 `min-width: 0` 防撑破 flex 布局；图片 `max-width: 100%`；表格与代码块在本侧栏内横向滚动，不撑破工作台。

### D6：性能、降级与失败隔离

- **预览资格否决**：`truncated=true` 的 diff MUST NOT 提供预览开关（F3：canonical spec 要求超大文件「不冻结浏览器」，spec.md:69；两侧截断前缀合计可接近 1 MiB，同步解析加 DOM 膨胀有冻结风险）。不设未定义指标的性能门槛；验收以可确定的结构测试为准：`truncated=true` 时永不挂载 `MarkdownPreview`；双侧各 524288 bytes 输入渲染无异常的 smoke test。
- **统一资格判定与动态失效**：唯一派生谓词见 D3（`canPreview` / `previewActive`）。diff 可在同一组件 key 下刷新（GitPanel.tsx:391 的 key 仅含三元组），因此已处于预览时若刷新结果使资格失效（如变为 truncated），MUST 立即按源码/既有派生状态渲染并清除 `preview`；后续资格恢复 MUST NOT 自动重进预览。
- **逐侧失败隔离（契约句与 spec 逐字一致）**：每侧预览 MUST 独立失败隔离：单侧渲染失败仅替换该侧为固定提示「渲染失败，请切回源码」，另一侧继续正常显示；切回源码再进入预览、切换文件或该侧内容变化时 MUST 重置并重试渲染。图片等网络加载失败不属于渲染失败，MUST 按固定占位与 alt 降级。任何渲染失败 MUST NOT 呈现空白视图。机制：每侧由独立 ErrorBoundary 包裹（`try` 无法捕获 React 子树渲染异常）。

### D8：预览模式块级批注

- **锚定来源（行号映射唯一口径）**：react-markdown 自定义组件收到 hast `node`（官方 README Appendix B「Every component will receive a `node`」；`passNode: true` 为 react-markdown 硬编码行为）；`node.position` 默认保留（`mdast-util-to-hast` 的 `patch()` 无条件复制 mdast position）。`line`/`column` 为 1-based；`offset` 为 0-based UTF-16 code-unit offset，公式中的 offset MUST 直接传给 JS `slice`，不得加减一；unist 规范中 end point 为排他位置（解析区域之后第一个字符）。**行模型唯一口径**：MUST NOT 直接使用解析器行号（micromark 将裸 `\r` 视为行结束，与 git-operations「diff 视图渲染」仅以 `\n` 分行的行模型漂移，实测 `"a\rb"` 段落解析器行范围跨 2 行而批注行模型仅 1 行）；行号 MUST 由 offset 映射得出——`lineAt(o) = 1 + count('\n', content.slice(0, o))`，`startLine = lineAt(start.offset)`，`endLine = lineAt(max(start.offset, end.offset - 1))`（end point 排他，取其前一个字符所在行；`max` 防御零宽范围）。块级元素（`p`、`h1`-`h6`、`li`、`ul`/`ol`、`table`（整块）、`pre`、`blockquote`、`hr`）的 handler 均调用 `patch`，position 可靠携带；任务列表 checkbox 是生成节点（无 position）但 `li` 本身 position 不受影响；本设计不引入 `rehype-raw`，不存在无 position 的原始 HTML 块。防御规则：position 或 offset 缺失、或范围零宽的节点 MUST NOT 提供批注入口；徽章匹配 MUST 复用同一 offset 映射。
- **可批注块集合**：`p`、`h1`-`h6`、`li`、`table`（整块）、`pre`、`blockquote`。容器（`ul`/`ol` 等）与 `hr` 不可批注。嵌套时批注目标为指针位置**最内层**可批注块（事件目标向上找最近的集合内元素；如列表项内段落 → 该 `p`，表格单元格内文本 → `table`）。
- **接口与状态 owner**：批注数据与草稿状态的 owner MUST 为 `DiffViewer`（与源码批注同一 owner，评论输入状态同样由 owner 持有）；`MarkdownPreview` 保持受控，两侧各自实例化、天然按侧隔离，批注相关 props：`side: 'old' | 'new'`、`annotations: Annotation[]`（owner 已按视图身份三元组过滤的本文件批注）、`draft: { startLine: number; endLine: number; comment: string } | null`（本侧块级草稿锚点与评论文本）、`onAnnotateBlock(range)`、`onDraftCommentChange(comment)`、`onSubmitDraft()`、`onCancelDraft()`、`onLocateAnnotations(ids: string[])`。
- **手势与草稿**：点击块上的批注入口（块边缘弱存在感按钮）进入块级草稿态——目标块高亮 + 评论输入；提交创建批注：`side` = 所在栏（左栏 old / 右栏 new；单侧预览按其存在侧），`startLine`/`endLine` = 按锚定来源条目 offset 映射得出的块源码行闭区间，快照窗口照常 ±3 行（`buildSnapshot`，review-utils.ts:52-64），stale 与 ReviewPanel 行为完全不变。Esc/空评论关闭即丢弃（与既有「空评论不留存」一致）。全局同时最多一个草稿（源码草稿与块级草稿互斥）；退出预览或切换文件 MUST 丢弃块级草稿；同一组件 key 下 diff 刷新时任一侧内容或存在性变化，MUST 在新预览交互前清除该侧块级草稿（草稿锚定的 AST position 属于旧内容，继续提交会构造错误快照）。
- **徽章显示**：预览模式仅显示与某可批注块 position 范围**精确相等**且侧别匹配的批注的块级徽章（无论其创建于预览还是源码模式）；范围不精确匹配任何块的批注不在预览中显示标记（切回源码后照常可见）。徽章匹配顺序固定（契约句与 diff-annotations delta 逐字一致）：徽章的批注集合 MUST 先按视图身份三元组（path、ref、untracked）过滤，再按侧别与精确范围匹配；同一范围同时匹配嵌套块时 MUST 仅归属最内层匹配块；同一块有多条匹配批注时 MUST 聚合为一个带数量的徽章，悬停按（createdAt、id）顺序显示各条评论摘要，点击在批注列表定位全部匹配条目；已漂移批注的徽章 MUST 显示漂移标识。
- **与 D4 不变量关系**：进入预览仍关闭源码草稿并清跨侧提示；源码模式批注瞬态 UI 仍受 `!previewActive` 约束；块级草稿与徽章是预览模式自身的批注 UI，不受该约束。
- **测试要求**：行范围映射（段落/表格/含 checkbox 的任务列表项/代码块，含裸 `\r` 与 CRLF 内容用例）、嵌套时最内层块选择、徽章精确匹配规则（范围相等且侧别匹配才显示）、徽章分支（视图身份三元组隔离、同范围嵌套归属最内层、同块多条聚合与排序及点击传递全部 ID、漂移标识）、块级草稿生命周期（提交创建/空评论丢弃/退出预览清除）、同 URL 图片在父级内容更新后重新加载（收尾评审遗留 NON-BLOCKING 项，并入本节测试）。

### D9：提交内容的统一省略规则（用户决策，对所有文件类型生效）

- **规则（行为规范见 diff-annotations delta，逐字一致）**：组装提交 payload 时，单条批注的引用范围行数 = endLine - startLine + 1（1-based 闭区间），引用范围字符数 = 从快照窗口切取的范围文本 rune 数；行数 > 5 或字符数 > 300 时，该批注块仅输出批注头（含文件、行范围、侧别与来源）与评论，MUST NOT 包含快照窗口全文；否则 MUST 包含快照窗口全文。
- **逐字公式（用户已确认保留侧别来源）**：省略分支输出 = 既有完整批注头 `### 批注 i — Q(path):range (side，来源 source)` + `"\n评论：" + comment`（仅删除 fence 段，头与评论逐字不变）。范围文本切取：`Split(snapshot, "\n")[startLine-snapshotStartLine : endLine-snapshotStartLine+1]` 以 `"\n"` Join，再 `utf8.RuneCountInString` 计 rune；内部 `\n` 与 CRLF 的 `\r` 均计入，末行后不额外计分隔符。执行顺序：每条批注 MUST 先无条件计算并校验切取窗口索引 `lo = startLine - snapshotStartLine`、`hi = endLine - snapshotStartLine + 1`，满足 `0 <= lo <= hi <= len(Split(snapshot, "\n"))`；越界统一返回既有 `ErrInvalidSnapshotWindow`（API 映射为 `invalid_input`），且 MUST 发生在 core 大小准入与提交记录创建之前；校验通过后再计算 rune 数并判断阈值；先逐条执行省略，再对最终 core 做 65536-byte 准入。
- **机制**：`buildAnnotationBlock`（internal/application/diffreview/payload.go:110-118）增加分支。省略判定在组装期完成，无 API/DB 契约变更；落库 payload 即最终文本。
- **stale 影响**：省略内容时不再提供快照全文作定位依据；canonical diff-annotations「批注锚定状态」的漂移定位表述随之修订（省略时以行范围为定位依据）。接收方仍有 file part 与固定指令「修复前先阅读相关代码」。
- **测试要求**：行数恰 5/6、字符数恰 300/301 的边界用例；省略时输出无 fence 且批注头逐字不变；未省略时 fence 段照旧；范围文本切取保留 CRLF 行尾；1 行与 6 行范围的损坏窗口（行偏移越界）用例，断言返回 `invalid_input` 且 repository create 未被调用。既有 fixture/golden 处置：payload_test.go 的 6 行范围 fixture（:43）改为自洽窗口并断言无 fence；:44 与 :198 两个窗口不自洽 fixture 仅调整其 `SnapshotStartLine`/`SnapshotLineCount` 使窗口自洽，MUST NOT 改变 header、snapshot 文本或 golden payload 字节；`TestPayloadConcatenationGolden` 与短范围 dynamic-fence golden MUST 逐字保持不变。

- [react-markdown 为新增依赖，增加 bundle 体积（含 remark 生态传递依赖）] → DiffViewer 本身已走 lazy 挂载（GitPanel 的 `DiffViewerLazy`），影响面限定在 diff 查看场景；接受。
- [预览模式无变更对比，用户可能误以为两栏渲染结果相同即无改动] → 默认仍进源码模式，变更对比由源码模式承担。
- [远程图片加载泄露客户端 IP 等网络信息] → 用户已确认接受（DR-1）；图片 lazy 加载且仅绝对 http(s)，源码模式不产生外部请求。
- [大文档渲染卡顿] → `truncated=true` 否决预览资格（D6），输入规模被既有容量上限约束。

## Migration Plan

前端增量：`pnpm --dir web add react-markdown@^10 remark-gfm@^4`（同步提交 `web/package.json` 与 `web/pnpm-lock.yaml`）+ 新组件 + DiffViewer 分支 + CSS + 单测。后端增量：`payload.go` 省略规则分支 + 单测（无 API/DB 契约变更、无迁移）。回滚 = revert 提交并移除依赖。

## Open Questions

（无）
