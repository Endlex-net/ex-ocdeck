# Tasks: markdown-diff-preview

## 1. 前置收敛与依赖

- [x] 1.1 换行基线收敛：`GitPanel.tsx:75` `wrapOverride` 初值 `true`→`false`（对齐 canonical「代码行默认 MUST NOT 折行」），同步更新 `git-panel-diff.test.tsx:157` 的默认换行断言（design 前置任务，见 Context 与 Migration Plan）
- [x] 1.2 新增前端依赖：`pnpm --dir web add react-markdown@^10 remark-gfm@^4`，同步提交 `web/package.json` 与 `web/pnpm-lock.yaml`（design Migration Plan）

## 2. MarkdownPreview 组件与 URL 判定层（D1/D5）

- [x] 2.1 新建 `web/src/components/diff/MarkdownPreview.tsx`：受控组件，核心 props `{ content: string }`；react-markdown v10 + `remarkPlugins={[remarkGfm]}`、`skipHtml`；不自定义 `urlTransform`，由自定义 `a`/`img` 组件对最终值判定（任务 2.2）；D8 批注能力按受控接口扩展（任务 5.2）
- [x] 2.2 URL 判定层（D1 契约句）：自定义 `a`/`img` 渲染组件，作用于最终 href/src；无 base URL 解析、仅 http/https 通过；缺失/空/解析异常判定不合格且不向外抛错；合格图片使用 `loading="lazy"`，合格链接使用 `target="_blank"` + `rel="noopener noreferrer"`；不合格链接整个后代树降级纯文本等价物（图片显示 alt）且不导航不发请求；不合格/加载失败图片固定占位 + alt、不保留 img/src；图片失败状态按最终 src 隔离、src/内容变化允许重新加载
- [x] 2.3 样式：`web/src/legacy-components.css` `.diff-view` 区块附近新增 `.md-preview` 作用域样式——GFM 元素基础排版（标题/列表/表格/代码块/引用等）、预览列 `min-width: 0`、图片 `max-width: 100%`、表格与代码块本侧栏内横向滚动；颜色沿用 design-system 变量（双侧等宽与「旧侧/新侧」标注属集成层布局，见 3.2）
- [x] 2.4 组件单测：GFM 元素渲染（表格/任务列表/删除线/自动链接）；原始 HTML 不渲染（skipHtml）；URL 策略分支（相对/空/非法/mailto/www 自动链接、嵌套图片链接不保留远程请求、合法图片 lazy 与合法链接 target/rel 属性）；图片失败按 src 隔离与同 URL 内容更新后重试

## 3. DiffViewer 预览集成（D2/D3/D6）

- [x] 3.1 预览状态与谓词：`preview: boolean`（默认 false）；`isMarkdown` MUST 由 `extractExtension(path)` 的 `.md`/`.markdown` 结果派生（复用语法高亮同一扩展名提取规则，不产生第二套判定）；`canPreview`/`previewActive` 按 D3 逐字契约；工具栏「预览」开关出现条件 = `canPreview`，`aria-pressed`/实际渲染分支/编辑器创建 effect 短路/effect 依赖 = `previewActive`；进入预览后不创建 CodeMirror 实例
- [x] 3.2 预览渲染分支：`previewActive` 时双侧各渲染 `MarkdownPreview`（双侧等宽并分别标注「旧侧/新侧」；单侧存在时仅渲染存在侧占满宽度），替代 CodeMirror 容器
- [x] 3.3 预览态工具栏边界：仅保留预览开关，不显示单列/并排/换行控件；退出预览恢复进入前的形态与换行偏好
- [x] 3.4 动态失效：同一组件 key 下 diff 刷新导致资格失效时立即按源码/既有派生状态渲染并清除 `preview`；资格恢复不自动重进预览
- [x] 3.5 逐侧失败隔离：每侧 `MarkdownPreview` 独立 ErrorBoundary 包裹；单侧失败仅替换该侧为「渲染失败，请切回源码」、另一侧正常显示；三种重试条件（切回源码再进入预览/切换文件/该侧内容变化）均重置重试；任何渲染失败不呈现空白视图；图片网络加载失败不触发渲染失败提示
- [x] 3.6 性能验收：`truncated=true` 不挂载预览；双侧 2×524288 字节输入 smoke 测试无异常
- [x] 3.7 集成单测：扩展名判定（大小写、其他后缀、无后缀文件无开关）；开关出现/隐藏（非 markdown、非 merge、truncated、编辑模式）；进入/退出预览；动态失效退出；切换文件后回到源码模式且预览选择不跨文件保留；单侧预览（纯新增/纯删除）；工具栏控件显隐；形态/换行偏好恢复；预览中 mode 变更横幅照常显示；源码模式不因 markdown 内容 URL 发起外部资源请求；逐侧失败隔离（单侧抛错时另一侧继续显示且不空白；切回源码再进入、切换文件、该侧内容变化三种条件分别重置重试；图片 `onError` 仅显示占位、不触发 ErrorBoundary）

## 4. 编辑与源码行级批注互斥（D4）

- [x] 4.1 编辑进入前置集加固：`enterEdit()` 与自动进入编辑 effect 同时满足非预览、merge 渲染状态、`gate.ok`、`eligibility === 'eligible'`、`firstReadRef` 非空；自动 effect 依赖至少含 `preview`、`gate.ok`、`state.kind`
- [x] 4.2 资格作废唯一动作：diff 刷新后门禁或 merge 状态不成立时 `eligibilitySeq.current++`、`firstReadRef.current = null`、`setEligibility('checking')`、清空 `deniedReason`/`enterError`；不新增枚举值
- [x] 4.3 进入预览执行 `closeDraft()` + `setCrossSideHint('')`；全部源码模式批注瞬态 UI（草稿、候选高亮、跨侧选区提示）受 `!previewActive` 约束；预览态隐藏全部编辑相关控件与资格提示（资格预取后台继续，退出后按当前资格恢复）；MUST NOT 清除 `editModePreferred`
- [x] 4.4 互斥测试（D4 测试要求）：「预览中资格请求完成不进入编辑」「预览中 eligible → 刷新为 truncated/非 merge → 不进入编辑且资格缓存作废」「进入预览清除批注瞬态 UI」

## 5. 预览块级批注（D8，diff-annotations delta）

- [x] 5.1 行号映射与块归属：`lineAt(o) = 1 + count('\n', content.slice(0, o))`（offset 0-based 直接传 slice）；`startLine`/`endLine` 映射公式含 `max(start.offset, end.offset - 1)`；position/offset 缺失或零宽不提供入口；可批注块集合 `p`、`h1`-`h6`、`li`、`table`、`pre`、`blockquote`，容器与 `hr` 不可批注；嵌套取指针位置最内层块
- [x] 5.2 `MarkdownPreview` 批注接口与块级草稿 UI：受控 props `side`/`annotations`/`draft {startLine,endLine,comment}`/`onAnnotateBlock`/`onDraftCommentChange`/`onSubmitDraft`/`onCancelDraft`/`onLocateAnnotations`；块边缘批注入口按钮、目标块高亮、评论输入
- [x] 5.3 `DiffViewer` 块级草稿生命周期：owner 为 DiffViewer（与源码批注同一 owner）；全局单草稿互斥；退出预览/切换文件丢弃；同 key 刷新任一侧内容或存在性变化时新预览交互前清除该侧草稿；提交创建批注（side=所在栏、±3 快照窗口照常 `buildSnapshot`、stale 与 ReviewPanel 行为不变）
- [x] 5.4 徽章显示：先按（path, ref, untracked）过滤，再按侧别与精确范围匹配；嵌套同范围仅归属最内层块；同块多条聚合为带数量徽章、悬停按（createdAt, id）排序显示摘要、点击在批注列表定位全部匹配条目；漂移批注徽章显示漂移标识；范围不精确匹配的批注不在预览显示
- [x] 5.5 块级批注测试（D8 测试要求）：行范围映射（段落/表格/含 checkbox 任务列表项/代码块，含裸 `\r` 与 CRLF 用例）；position/offset 缺失或零宽不提供入口；最内层块选择；徽章分支（精确范围相等且侧别匹配才显示、范围不匹配不显示、三元组隔离/嵌套归属/聚合排序与全部定位/漂移标识）；草稿生命周期（提交创建——断言侧别、闭区间范围与 ±3 快照窗口、空评论丢弃、Esc 丢弃、退出预览清除、切换文件清除、同 key 刷新清除）

## 6. 后端提交省略规则（D9，diff-annotations delta）

- [x] 6.1 `internal/application/diffreview/payload.go` `buildAnnotationBlock` 增加省略分支：每条批注先无条件校验窗口索引 `lo = startLine - snapshotStartLine`、`hi = endLine - snapshotStartLine + 1`（`0 <= lo <= hi <= len(Split(snapshot, "\n"))`），越界返回既有 `ErrInvalidSnapshotWindow`（API 映射 `invalid_input`）；错误 MUST 沿组装链传播——`buildAnnotationBlock` → `buildAnnotationSection` → `assemblePayload`/`assemblePayloadFromAnnotations` → `CreateSubmission`，发生在 core 大小准入与提交记录创建之前；校验通过后切取范围文本、`utf8.RuneCountInString` 计数；行数 > 5 或字符数 > 300 时仅输出批注头与评论（逐字不变，仅删 fence 段）；否则维持既有 fence+快照窗口全文
- [x] 6.2 既有 fixture 处置：payload_test.go `:43` 6 行范围 fixture 改为自洽窗口并断言无 fence；`:44` 与 `:198` 仅调整 `SnapshotStartLine`/`SnapshotLineCount` 使窗口自洽，不得改变 header、snapshot 文本或 golden payload 字节；`TestPayloadConcatenationGolden` 与短范围 dynamic-fence golden 逐字保持不变
- [x] 6.3 省略规则测试：行数恰 5/6、字符数恰 300/301 边界；省略输出无 fence 且批注头逐字不变；未省略 fence 照旧；范围文本保留 CRLF 行尾；1 行与 6 行损坏窗口断言 `invalid_input` 且 repository create 未被调用；失败时零副作用断言（批注保持原样、未调用消息发送接口）；省略仅影响 payload 文本、提交条目仍持久化完整快照窗口的断言

## 7. 验证与收尾

- [x] 7.1 前端验证：`pnpm --dir web test` 与 `pnpm --dir web build` 通过
- [x] 7.2 后端验证：`go test ./internal/application/diffreview/... ./internal/api/...` 通过
- [x] 7.3 测试有效性证据（评审用；来源：ex-workflow Section 7「实现者自检（交付评审前强制）」）：每个新增/修改的行为测试提供旧实现下失败、新实现下通过的证据（基线 revision 运行或等效 mutation 验证）
- [x] 7.4 `openspec validate markdown-diff-preview --strict` 通过，tasks 勾选状态与实现一致
