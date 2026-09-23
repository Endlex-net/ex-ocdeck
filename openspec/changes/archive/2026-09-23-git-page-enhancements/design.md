# Design: git-page-enhancements

## Context

现状与约束（动机见 proposal.md，行为契约见 specs/git-operations delta）：

- diff 渲染：`@codemirror/merge` 6.12.2 `MergeView`（`web/src/components/diff/DiffViewer.tsx:923-937`）。已核实（库源码级）：MergeView 零 scroll 监听；纵向对齐靠单一 `.cm-mergeView` 滚动容器 + 高度 spacer 天然双 pane 同步；横向每 pane 独立 `.cm-scroller`（`overflowX:auto`），无任何同步；`MergeView.a` / `MergeView.b` 与 `EditorView.scrollDOM` 为公开 API；`@codemirror/view` 无 scroll-sync 工具。编辑器在 `DiffViewer.tsx:862` 的 effect 中按 deps 重建（destroy → 新建）。
- 单侧坍缩：纯新增/纯删除时一侧折叠为零宽（`DiffViewer.tsx:265,276,312-313` `singleSided` / `singleSidedRef`）。
- 文件列表：三组硬编码 `groupFiles`（`GitPanel.tsx:30-43`）；路径用 `direction:rtl` 省略号只露尾部（`legacy-components.css:1191-1201`）；commit 勾选为路径级 `Set<string>`（`GitPanel.tsx:61,233,421-422`）。
- 后端：任务锁 + `assertGitRepoTask` 门禁（`internal/task/gitops.go:35`）；ref 解析 `ResolveRefOID`（`content.go:87`）；ref 侧读取 `ReadRefSideContent`（`content.go:108`，ls-tree 探测 + `git show <blobOID>`）；UTF-8 规范化管线 `normalizeDiffSideContent`（`gitops.go:227`）；numstat 解析 `parseNumstatZ`（`internal/infrastructure/git/parser_numstat.go:25`）；`merge-base` 已在命令白名单（`exec.go:25`）零调用；exit code 提取 `gitExitCode`（`exec.go:60`）。
- 前端任务上下文：工作台页已加载任务详情（含 `base_ref`），`web/src/pages/TaskWorkbenchPage.tsx:764` 渲染 `<GitPanel taskID …/>`（任务 `base_ref` 已在该页加载，`:438,563`）。
- 批注身份三元组（path、ref、untracked）与编辑资格预取均在 DiffViewer/ReviewPanel 内，branch 视图按条目类型门控：已提交条目整体只读，未提交条目保持完整能力（spec「分支对比文件 diff 查看」只读条款）。

## Goals / Non-Goals

**Goals:**

- 并排横向滚动协同（含防回环、单侧豁免），不触碰既有纵向滚动/对齐/批注/编辑行为。
- 文件列表：混合三色、容器级横向滚动、flat/tree 双形态（纯前端投影，无新依赖）。
- 分支对比（three-dot）：新增两个只读端点 + 前端视图切换，three-dot 语义唯一由服务端执行。
- 设计落点显式化，实现 agent 可仅凭 artifacts 执行。

**Non-Goals:**

- proposal 非目标之外，设计层补充：不引入 OID/snapshot token 协议；不持久化任何新视图偏好（flat/tree、视图选择、base ref 均会话内保留）；不改造既有 `/git/diff` 端点参数形态；不实现任意文件阅读（仅按 D9 预留复用面）。

## Decisions

### D1：横向滚动协同——scrollDOM 双向 passive 监听 + clamp + 程序性 echo 抑制

在 `DiffViewer.tsx:862` 编辑器创建 effect 内、`new MergeView(...)` 之后安装：对 `mergeView.a.scrollDOM` 与 `mergeView.b.scrollDOM` 各挂一个 `{ passive: true }` 的 `scroll` 监听。echo 状态**按 pane 分离**且存放于组件级 ref（`pendingRef = { a: Set<number>, b: Set<number> }`）：记录本侧被程序性写入、尚在等待 echo 的偏移值（允许同侧多个在途值）；存放于 ref 而非创建 effect 闭包，使下述独立清理路径可触达。

`sync(from, to)`：`next = clamp(from.scrollLeft, 0, to.scrollWidth - to.clientWidth)`；`to.scrollLeft === next` 时短路 no-op；否则记录 `prev = to.scrollLeft`，执行 `to.scrollLeft = next`，若写入后 `to.scrollLeft !== prev`（实际产生位移，浏览器将异步派发 echo 事件）则将实际写入值加入 `to` 侧 `pending`。

handler(X) 入口**先做启用态判定**（side-by-side 且未换行且 `!singleSidedRef.current` 且 merge 渲染态；该判定 MUST 先于 pending membership 判断）：禁用态时清空两侧 `pending` 并 return（兜底清理）。启用态再判 membership：若 `pending(X)` 含 `X.scrollLeft` → 判为程序性 echo：`pending(X).clear()` 并 return（echo MUST NOT 反向改写另一侧；同值用户事件与同值 echo 无法区分，统一按 echo 消费——spec 已显式声明该同值竞态例外：可能跳过一次对侧同步，任一下一次滚动事件到达即自愈，无持久分叉）。否则判为用户事件：`pending(X).clear()`（旧的程序 echo 已被浏览器合并/覆盖——scroll 事件到达时读取的是当前 `scrollLeft`，迟到 echo 会因值不匹配落入本分支，再经 `sync` 的相等短路收敛，不回拉源侧），然后执行 `sync(X, 另一侧)`。

生命周期与 pending 清理（两条独立路径，均不重建 editor 以外的东西）：

1. **创建 effect cleanup**：editor 重建与 mode/wrap 切换时，先移除两个监听、清空 `pendingRef` 两侧 Set，再 `view.destroy()`；重建后按新 MergeView 重新安装监听。
2. **启用谓词 effect（同步清理）**：一个独立的小 effect 依赖完整启用谓词（mode/wrap/singleSided/merge 态——其中 singleSided 经 render 期更新的 `singleSidedRef` 反映，该 effect 依赖渲染布尔值而非 ref），谓词变 false 时在本次渲染提交后无条件清空两侧 `pending` Set——不依赖任何 scroll 事件，保证「单侧期间零滚动事件」场景下重新启用前 pending 必为空。该 effect MUST NOT 触碰 editor 实例与监听安装（单侧坍缩不触发 editor 重建：存在性不进重建 deps，`DiffViewer.tsx:312-313,973-977`）。

- **防回环 = 值相等短路 + 按 pane 分离的程序性 echo 抑制，不用布尔 guard**：相等短路处理「两侧已对齐」的 echo；两侧最大可滚动距离不同时（A 最大 1000、B 最大 500），A 滚到 900 会把 B clamp 到 500，B 的 echo 到达时若仅靠相等短路会把 A 回拉到 500——违反「源侧偏移不被反向改写」，故需 echo 消费。快速交替场景下单一标量记录会被新事件覆盖、迟到 echo 被误判为用户事件，故 MUST 按 pane 分离且允许同侧多个在途值（Set）。布尔 guard 无效：scroll 事件异步派发，echo 到达时 guard 已复位（库研究实测结论）。
- **不用 `EditorView.domEventHandlers({scroll})`**：其语义为「滚动元素或其父节点滚动」均触发，会把外层纵向滚动混入，无法干净区分轴向。
- **不用 rAF 合并**：`scrollLeft` 读写廉价，rAF 引入一帧延迟与抖动。
- **像素偏移同步，非比例同步**；两侧最大可滚动距离不同时，目标侧 clamp 到其最大可行偏移并停住，源侧保持用户设定偏移不被回拉。
- **启用条件**（handler 内判定，不进重建 deps）：`mode==='side-by-side'` 且未开启换行且 `!singleSidedRef.current` 且为 merge 渲染状态。换行时 `scrollLeft` 恒 0，监听天然 no-op；单侧坍缩时直接 return（否则空侧最大偏移 0 会把内容侧回拉，阻断纯新增/纯删除长行阅读——spec 已明确豁免）。
- 与既有逻辑兼容：`annotation-ext.ts:229` 的 `syncInlineHostWidth` 只读 `scrollLeft`，由 `DiffViewer.tsx:421` 的 `ResizeObserver` 驱动，不形成写回环；协同 listener 中 MUST NOT 调 `syncInlineHostWidth`。编辑态新侧仍为 `mergeView.b`，写 `scrollLeft` 不产生文档事务，不影响 EditSession 写回。
- 备选（否决）：布尔 guard（echo 抑制失败）；库内 syncScroll（不存在）；比例同步（宽度不同会漂移）。

### D2：分支对比 API——两个新端点，`base` 为唯一用户提供的 ref/object identity 输入

新增只读端点（`internal/api/git.go` `registerGitRoutes`）：

```text
GET /api/v1/tasks/{id}/git/branch-diff/files?base=<ref>
GET /api/v1/tasks/{id}/git/branch-diff/file?base=<ref>&path=<path>
```

file 端点另接收用户选择的 `path` 参数；files 端点不接收 `path`。除此之外无任何用户提供的 ref/OID 输入。

- 单文件端点响应复用 `GitDiffDTO` 八字段契约，逐字段不变。
- files 响应：`{ baseRef: <用户输入原值>, files: [{ path, additions, deletions, isBinary }] }`。响应 MUST NOT 携带 `headOID`/`mergeBaseOID` 等裸 OID（与 Goals/Non-Goals「不引入 OID/snapshot token 协议」及 D8「MUST NOT 暴露裸 OID 给前端」一致）；OID 仅为服务端内部值。
- **拒绝裸 OID 入参（old/new）**：会把 three-dot 语义与对象身份交给前端，扩大信任边界；如需强一致快照未来引入服务端签发的 opaque snapshot token（本期不做）。
- **拒绝扩展既有 `/git/diff`**：触碰其 spec「端点与请求参数形态不变」硬约束，且来源语义不同。
- 两 endpoint 共用服务端 helper（`internal/task/gitops.go`，task 包级私有函数，完整签名）：`func resolveBranchDiffRefs(ctx context.Context, worktree, base string) (baseOID, headOID, mergeBaseOID string, err error)`——**helper 仅接收 base，不接收 path**；词法校验分工：base 非空由两个端点各自校验（先于 helper），path 仅 file 端点在调用 helper 前经 `git.ValidateDiffPath` 校验（files 端点无 path 参数，MUST NOT 对其执行 path 校验）。helper 内先 `ResolveRefOID(base)` 与 HEAD 解析各得 OID，最后以**两个 OID** 执行 merge-base（避免 ref 在请求期间移动）。HEAD 解析 MUST 封装为 infrastructure 原语 `git.ResolveHeadOID(ctx, dir) (string, error)`：执行 `git rev-parse --verify --quiet --end-of-options HEAD`（已实测：仓库有效但 HEAD 无任何提交 → exit 1 无输出，产生 typed `ErrUnbornHead`；exit 0 → 输出 OID；exit 128 等致命失败如仓库损坏 → 透传 stderr）。merge-base 的 Git CLI 执行 MUST 封装为 infrastructure 原语 `git.MergeBase(ctx, dir, baseOID, headOID) (string, error)`（`internal/infrastructure/git/`）：仅 exit 1 经 `gitExitCode` 判定产生 typed `ErrNoMergeBase`，其他非零 exit 透传 stderr；task 层只做任务门禁、调用顺序与错误映射，MUST NOT 在 task 层直接编排 git CLI。`BranchDiffFiles` 完整签名：`func BranchDiffFiles(ctx context.Context, dir, mbOID, headOID string) ([]BranchDiffFile, error)`，infra 返回类型 `git.BranchDiffFile{ Path string; Additions int; Deletions int; IsBinary bool }`（新增于 `internal/infrastructure/git/types.go`），**infrastructure MUST NOT 依赖 application DTO**——task 层负责 `git.BranchDiffFile` → `application.GitBranchDiffFileEntry` 的逐字段映射。同层另外两个原语完整签名：`func ResolveHeadOID(ctx context.Context, dir string) (string, error)`、`func MergeBase(ctx context.Context, dir, baseOID, headOID string) (string, error)`。此为 three-dot 语义唯一实现点；两次 HTTP 请求各自独立计算（每次 merge-base 实测约 6.6ms 量级，可接受）。
- 错误映射（不新增 API 错误码，复用 `application/operror.go` 集合）：base 为空（词法失败）→ `invalid_input` 且零 git 调用；base 非空但无法 rev-parse → `invalid_input`（仅执行这一次 `ResolveRefOID` 解析调用，其后 MUST NOT 执行 `ResolveHeadOID`/`MergeBase`/`BranchDiffFiles`/blob 读取）；HEAD 无提交 → `invalid_state`（消息明确；唯一判定来源为 `git.ResolveHeadOID` 的 typed `ErrUnbornHead`，MUST NOT 匹配 stderr 文案）；无共同祖先 → `invalid_state`（消息明确）；merge-base 其他执行失败 → `git_error` 透传 stderr。**无共同祖先 MUST 以 exit code 判定**（已实测：`git merge-base` 有共同祖先 exit 0、无共同祖先 exit 1、OID 不存在等执行失败 exit 128 等非零）：typed sentinel `ErrNoMergeBase` 由 `git.MergeBase` 原语产生（仅 exit 1），MUST NOT 匹配 stderr 文案。
- Manager/TaskBackend 精确签名（参数顺序固定）：`GitBranchDiffFiles(ctx context.Context, taskID, base string) (application.GitBranchDiffFilesDTO, error)`；`GitBranchDiffFile(ctx context.Context, taskID, base, path string) (application.GitDiffDTO, error)`。`GitBranchDiffFilesDTO{ BaseRef string \`json:"baseRef"\`; Files []GitBranchDiffFileEntry \`json:"files"\` }`，`GitBranchDiffFileEntry{ Path string \`json:"path"\`; Additions int \`json:"additions"\`; Deletions int \`json:"deletions"\`; IsBinary bool \`json:"isBinary"\` }`（json tag 风格与 `GitFileDTO` 一致，全小写/camelCase 混合按既有 DTO 口径）。
- 调用链与既有 GitStatus/GitDiff 一致：Manager 方法 `GitBranchDiffFiles` / `GitBranchDiffFile` 先词法校验 → `tryLockTask` → task/kind 校验（`assertGitRepoTask`，local-path 放行、dir 拒绝）→ helper → 读取。只读，不进 repo 写锁。

后端调用时序与失败出口（两个端点同一主流程，`GitBranchDiffFile` 多出归属校验一跳）：

```text
HTTP -> api handler -> Manager.GitBranchDiff{Files|File}
  |
  v
词法校验: base 非空(两端点各自校验) --fail--> invalid_input (零 git 读)
          仅 file 端点: path 经 ValidateDiffPath --fail--> invalid_input (零 git 读)
          (files 端点无 path 参数, 不执行 path 校验)
  |
  v
tryLockTask            --fail--> conflict (任务忙)
store.GetTask          --fail--> not_found (任务不存在)
  |
  v
worktree_path 为空      --fail--> invalid_state (gitops.go:72-74 现状口径)
  |
  v
assertGitRepoTask:
  project lookup       --fail--> not_found (project 不存在, gitops.go:36-39)
  非法 kind/mode        ------> internal (gitops.go:43,53 现状口径)
  dir 项目              ------> invalid_input (gitops.go:49 现状口径)
  |
  v
resolveBranchDiffRefs(base)  (path 非 helper 输入):
  ResolveRefOID(base)   --fail--> invalid_input (零后续读)
  ResolveHeadOID        --ErrUnbornHead--> invalid_state
                        --其他 exit------> git_error (透传 stderr)
  MergeBase(baseOID, headOID)
                        --ErrNoMergeBase---> invalid_state (无共同祖先)
                        --其他非零 exit----> git_error (透传 stderr)
  |
  v
files 端点: BranchDiffFiles(mbOID, headOID) -> 排序 -> DTO
file 端点:  BranchDiffFiles 归属校验 --path 不在集合--> invalid_input (零 blob 读)
            -> ReadRefSideContent x2 -> normalize -> GitDiffDTO
```

### D3：分支对比文件列表——`git diff --numstat -z <mbOID> <headOID>` 复用既有解析

git 层新增 `BranchDiffFiles(ctx, dir, mbOID, headOID)`：执行 `git diff --numstat -z <mbOID> <headOID>`，复用 `parseNumstatZ` 解析；二进制条目（`-` 计数）按既有 numstat 口径标记 IsBinary 且不计行；**结果 MUST 由服务端按路径字典序（byte-wise 字符串比较）排序后返回**——`parseNumstatZ` 返回 map（`parser_numstat.go:23-27`），map 遍历无序，MUST NOT 直接遍历输出；条目数超 `MaxStatusFiles`（默认 10000）返回 `ErrTooManyFilesChanged`（与「工作区状态查询」上限语义一致）。`ErrTooManyFilesChanged` 与 `git diff --numstat` 执行失败统一映射 `git_error` 透传消息（与工作区 status 既有口径一致，`gitops.go:84-86`）；file 端点归属校验（`BranchDiffFiles` 调用）失败时 MUST NOT 执行后续 blob 读取。空结果（merge-base == HEAD）返回空列表（200，非错误）。MUST NOT 包含工作区/index 变更（命令本身只读两棵 tree，天然满足）。

### D4：分支对比单文件内容——两侧 `ReadRefSideContent` + 既有管线

`GitBranchDiffFile` 在 `resolveBranchDiffRefs` 后：**先做变更集合归属校验**——经 `BranchDiffFiles(ctx, dir, mergeBaseOID, headOID)` 取得变更路径集合，`path` 不在集合中（rename 按列表展示的新路径判定）时返回 `invalid_input`，MUST NOT 执行任何后续内容读取（防止借该端点读取未变更的任意已提交文件，守住「任意文件阅读为非目标」边界）；归属通过后：旧侧 `ReadRefSideContent(ctx, dir, mergeBaseOID, path)`、新侧 `ReadRefSideContent(ctx, dir, headOID, path)`；两侧经 `normalizeDiffSideContent`；按既有 DTO 组装规则（isBinary 清空两侧内容、truncated 或运算）产出 `GitDiffDTO`。存在性、symlink/gitlink/目录按 `ReadRefSideContent` 既有判定（tree → 不存在；gitlink → OID 文本）。渲染派生状态与优先级前端零改动复用。

**内容读取失败语义（继承既有 diff 旧侧错误矩阵，`gitops.go:170-179` 口径）**：旧侧先读；`ReadRefSideContent` 失败即返回 `git_error` 并透传 stderr（`ErrUnmergedPath` 在 branch 场景不可能出现——merge-base/HEAD 两侧均为已提交 tree，无 unmerged 概念），**旧侧失败时 MUST NOT 执行新侧读取**；新侧读取失败同样映射 `git_error` 透传 stderr；`context` 取消/超限等行为与既有 diff 一致（上层 net/http 语义，不单独映射）。**rename 语义（用户已裁决）**：rename 列表项的两侧内容均按列表展示的新路径读取——旧侧（mergeBaseOID）该新路径不存在时按纯新增展示；MUST NOT 向 API 参数或内部内容读取链传递 oldPath。

### D5：混合文件列表——展示条目规范化（纯前端）

`GitPanel.tsx` 以「展示条目」替代 `groupFiles`：每个 `GitFileDTO` 展开为 1-2 条 `{ path, status: 'staged'|'unstaged'|'untracked', ref, untracked }`（staged → `ref='HEAD'`；unstaged && !untracked → `ref=''`；untracked → `untracked=true`；staged+unstaged 同路径出两条）。排序：**路径按 UTF-8 byte-wise 字典序**（与 D3 服务端排序同一 comparator；MUST NOT 用 `localeCompare`），同路径 staged 先于 unstaged。文件名颜色三色区分，映射固定（用户裁决，三次裁决更新）：staged → 成功绿 `var(--success)`、unstaged → teal `rgb(128,203,196)`、untracked → 琥珀 `var(--warn)`，并附 `title`/`aria-label` 非颜色状态信息。**条目布局（用户裁决，二次裁决更新）**：flat 形态先展示文件名、随后展示目录路径（次要视觉层级），替代单一路径字符串；tree 形态文件节点仅展示文件名（目录层级由树结构表达，二次裁决：文件名后 MUST NOT 跟目录路径）；两形态条目的 `title`/`aria-label` 均保留完整路径。commit 勾选维持路径级 `Set<string>`——同路径双条目共享勾选状态（与提交 API 路径级语义一致，不变更）。点击条目打开 diff 的来源语义与既有一致。

### D6：tree 形态——前端投影，键含 path+status

后端保持扁平 DTO，tree 由前端按路径分段构建。目录节点按目录路径唯一、可折叠/展开，仅渲染含变更文件的目录；**key 规则固定**：目录节点 key 为目录路径本身；文件叶子 key 为 `path + "\0" + <身份后缀>`——uncommitted 条目后缀为 `staged|unstaged|untracked`（同路径双条目不去重），branch 已提交条目后缀固定为字面量 `branch`（已提交条目无暂存状态概念，MUST NOT 新增第四个状态枚举值，tree 行为一致但不套三色）。**tree 文件节点布局（用户二次裁决，覆盖初裁「信息集与 flat 一致」）**：仅展示文件名——目录层级由树结构表达，文件名后 MUST NOT 跟目录路径；`title`/`aria-label` 保留完整路径（与 flat 一致）。**tree 同级节点排序**：按路径段 UTF-8 byte-wise 字典序（与 D5/D3 同一 comparator）；同路径条目固定顺序与 flat 一致：已提交 → 已暂存 → 未暂存 → 未跟踪（D8 混合列表同一次序）。flat/tree 切换为显式控件，默认 flat，会话内保留、不持久化；目录折叠状态同样会话级。active diff 身份按视图区分：uncommitted 条目保持 `path + ref + untracked`（批注三元组不变，两视图共享），branch 已提交条目身份见 D8。

### D7：列表横向滚动——容器级，移除 RTL 省略号

`.git-files` 容器内增加 `min-width: max-content` 内容层，行 `min-width: 100%; width: max-content`，横向滚动唯一归属列表容器（不做每行独立滚动：嵌套滚动区域多、tree 缩进错位）。移除 `.git-file-path` 的 `direction:rtl` 技巧，改正常 LTR 全路径、无 ellipsis（`legacy-components.css:1191-1201`）。不影响 `.git-side` 外侧的宽度拖拽把手（「评审文件面板尺寸调整与收起」不变）。

### D8：视图切换、只读门控与 branch 状态生命周期

GitPanel 增加视图状态 `'uncommitted' | 'branch'`（默认 uncommitted，会话内保留）：base ref 输入默认值来自任务详情 `base_ref`——TaskWorkbenchPage 已加载任务详情（`TaskWorkbenchPage.tsx:436-438`），本期**新增** `GitPanel` 的 `baseRef` prop（现有 props 仅 `taskID`/`active`/`agentBusy`，`GitPanel.tsx:47-56`；渲染处 `TaskWorkbenchPage.tsx:762-768` 传入 `task?.base_ref ?? ''`），为空时留白待手填。

**base 输入语义（用户已裁决）**：输入框区分 draft（编辑中值）与 applied（已生效值）；生效事件 MUST 为失焦或 Enter（MUST NOT 每次按键即触发请求）；生效前 trim；trim 后为空时回落任务 `base_ref` 默认值重新加载；**任务无 `base_ref` 且生效值为空的完整状态转移**：applied 置空、generation 递增（使全部在途旧响应失效丢弃）、清空已提交条目列表/已提交选中文件/其 diff/error 与 stale 标志（**未提交条目及其共享状态不受影响**）、已提交列表区域显示「请输入基础 ref」提示、对已提交部分零 API 请求（未提交条目展示不依赖基础 ref，照常展示）。会话内手工输入优先：组件持有 manual 标记（用户首次手改置位），`baseRef` prop 后续变化仅在 `!manual` 时更新 draft/applied，MUST NOT 覆盖手工值。

**branch 视图能力门控按条目类型判定（用户裁决：未提交条目在分支视图可编辑/批注/提交）**：已提交条目（branch-diff 来源）只读——其 DiffViewer 不传 annotations、批注回调、`editIO`、编辑资格刷新与离开守卫；未提交条目（status 来源）在 branch 视图为完整能力——DiffViewer 传齐 annotations/批注回调/editIO/编辑资格刷新/离开守卫，与 uncommitted 视图逐字一致（批注身份三元组 `path + ref + untracked` 不变）。branch 视图渲染 ReviewPanel（展示任务全部活动批注——与既有 diff-annotations 规范一致，stale 批注可见可提交，不做 status 过滤）与 commit 勾选/提交输入区（仅作用于未提交条目，语义与 uncommitted 视图一致）。uncommitted 视图行为与既有完全一致。

**视图切换离开事务**：切换视图前 MUST 先执行当前编辑会话的现有离开守卫（`GitPanel.tsx:518-535` 传入 DiffViewer 的 leave guard，仅未提交条目编辑会话存在该守卫）；守卫拒绝（在途写入失败或用户取消）时取消切换、停留在当前视图；通过后再切换。branch 视图中已提交条目的 DiffViewer 不注册离开守卫（无可写状态）；未提交条目注册（与 uncommitted 视图一致）。

**branch 视图列表合成（混合单列表）**：branch 视图文件列表 = 已提交条目（`gitBranchDiffFiles` 结果，身份后缀 `branch`）+ 未提交条目（既有 status 结果按 D5 展开规则）按路径 UTF-8 byte-wise 统一排序合并；同一路径的已提交条目与未提交条目为相互独立条目（不去重），**同路径多条目顺序固定为：已提交 → 已暂存 → 未暂存 → 未跟踪**（flat 与 tree 共用）；已提交条目视觉可区分（颜色由 designer 定）且无暂存状态；点击已提交条目走 `gitBranchDiffFile`（three-dot），点击未提交条目走既有 `gitDiff` 来源。

**branch 状态生命周期**：active diff 身份为 `条目类型 + 视图内身份`——未提交条目维持 `path + ref + untracked`（批注三元组不变），已提交条目为 `baseRef + generation + path`（generation 为**已提交缓存失效代次**：base ref 变更与提交成功均使其递增，见视图动作表「commit 成功」行）。**未提交相关状态（status 数据、勾选、未提交条目选中与 diff、编辑会话）为两视图共享的同一份 GitPanel 状态**，视图切换与 base 变更 MUST NOT 清空它们；base ref 变更时 generation 递增、清空当前选中的已提交条目 diff、重新获取已提交列表（未提交条目不受影响）。已提交列表/文件请求各自捕获发起时的 `baseRef + generation + seq`——序号模型统一为：组件级**全局单调递增 `requestSeq`**，并维护 `latestListSeq`/`latestFileSeq` 两个游标；每次发起已提交列表（或已提交文件）请求时分配新的全局序号、更新对应 latest 游标并随闭包捕获（文件请求另捕获 `path`）；已提交列表响应按 `latestListSeq` 独立校验，已提交文件响应按 `latestFileSeq` 独立校验，并另按当前已提交条目的 `baseRef + generation + path` 校验（文件请求 MUST NOT 使列表响应失效，反之亦然；未提交视图的 status/diff 请求沿用既有机制，不进本序号模型）；状态图中的 `listSeq`/`fileSeq` 即对应请求捕获的全局序号值。generation 与 requestSeq 均为纯客户端闭包状态：随请求上下文/回调闭包保存，MUST NOT 序列化为 query、body 或响应字段；服务端 HTTP 契约仅 D2 定义的 `base` 与 `path`。响应写入条件（已提交列表与已提交文件各自独立判定）：generation 与当前值一致 **且** requestSeq 等于该类请求的最新序号才允许写状态；已提交文件响应另需**当前选中条目为已提交条目且其 `baseRef + generation + path` 与该请求捕获值全匹配**（仅比较 path 不足以防同路径竞态：同一路径可同时存在已提交与未提交条目，用户先点已提交条目、请求在途、再点同路径未提交条目时，旧已提交响应 MUST 被丢弃）；不满足 MUST 丢弃（防同代乱序/旧路径响应覆盖新状态）；**stale 失败响应同样 MUST NOT 覆盖当前状态**（同样按写入条件判定丢弃）。一致性保证收窄为可执行边界：已提交文件 diff 请求**失败**时（不区分「合法删除」与「快照漂移」，无 snapshot token 下服务端无法为两次请求提供同一快照身份，二者不可区分）统一显示 diff 区失败/stale 状态并提供手动重试入口（MUST NOT 自动重试；用户点击重试时才分配新序号并重新请求），MUST NOT 静默忽略失败；**成功**响应按文件请求时的独立快照如实展示——客户端无法也无需判断其是否与列表请求同代（最终一致语义下的可接受错配窗口，用户刷新列表即收敛）。不承诺快照漂移自动检测（未来如需强一致，引入服务端签发的 opaque snapshot token，MUST NOT 暴露裸 OID 给前端）。已提交列表/文件的全部状态操作 MUST NOT 触碰未提交相关共享状态（status、勾选、未提交选中与 diff、编辑会话）。

视图切换与 branch 生命周期（状态图）：

```text
uncommitted 视图 <--------------------> branch 视图
       |  切换前: 当前编辑会话 leaveGuard()      |
       |    拒绝/取消 -> 停留当前视图            | 列表合成:
       |    通过 -> 切换视图                    |   已提交条目(branch-diff)
       v                                       |   + 未提交条目(status, 共享)
    (编辑守卫仅挂在未提交条目的 DiffViewer)      |   按路径 byte-wise 混合排序
                                              |
  未提交状态(status/勾选/选中/编辑会话)          | base 变更:
  跨两视图共享同一份, 切换与 base 变更均不清空    |   generation++
                                              |   清空已提交条目选中与 diff
                                              |   重新拉取已提交列表
                                              v
                                      branch 已提交请求生命周期:
                                        列表请求捕获 (baseRef, generation, listSeq)
                                        文件请求捕获 (baseRef, generation, fileSeq, path)
                                        写入条件: generation 一致 且 seq 为该类最新
                                                  (文件请求另需: 当前选中为已提交条目
                                                   且 baseRef + generation + path 全匹配)
                                        不满足 -> 丢弃 (成功与失败响应同样判定)
                                        列表失败且满足写入条件 -> 列表 stale + 手动重载入口
                                        文件失败且满足写入条件 -> diff stale + 手动重试入口
                                        (失败响应仅设置 stale/error, MUST NOT 自动重试;
                                         用户点击重载/重试时才分配新序号并发起请求)
                                        (不满足写入条件的成功/失败响应均丢弃,
                                         MUST NOT 改变当前错误状态或重载标志)
                                        文件成功 -> 按各自快照展示 (最终一致)
```

**视图动作表与失败状态契约**（branch 已提交条目的状态操作 MUST NOT 写入未提交共享状态）：

| 动作 | uncommitted 视图 | branch 视图 |
|---|---|---|
| 面板 refresh | 既有 `refreshAll` 语义不变（`GitPanel.tsx:281-289`） | 重拉已提交列表（`listSeq` 递增）+ 若有选中已提交条目则同时重拉其 diff（`fileSeq` 递增）+ 同时执行既有 `loadStatus`（未提交条目为共享状态，两视图都需刷新） |
| push 成功 | 既有行为不变（成功后 `loadStatus`，`GitPanel.tsx:314-327`） | 同 uncommitted：执行既有 `loadStatus`（未提交条目共享）；push 不改变已提交对比结果，已提交列表与选中状态保持不变 |
| commit 成功 | 既有行为不变（`loadStatus`） | 执行既有 `loadStatus`，且已提交列表失效（generation 递增、清空已提交列表/选中/diff/error/stale）；当前处于 branch 视图时立即自动重载已提交列表 |
| branch 已提交列表失败（满足写入条件） | 不适用 | 列表区域 stale 状态 + 手动重载入口（MUST NOT 自动重试；用户点击重载时分配新序号并重新请求） |
| branch 已提交文件失败（满足写入条件） | 不适用 | diff 区域失败/stale 状态 + 手动重试入口（MUST NOT 自动重试；用户点击重试时分配新序号并重新请求） |
| 已提交列表重载后已选已提交路径消失 | 不适用 | 清空已提交条目选中与 diff 视图（与 base 变更清空语义一致），MUST NOT 报错 |

不满足写入条件的响应（成功或失败）一律丢弃，MUST NOT 改变当前错误状态或重载标志。

### D9：文件阅读能力的扩展点（不实现）

SideContent 读取原语（`ReadRefSideContent`/工作区读取）+ `normalizeDiffSideContent` 管线 + 前端 merge/查看器渲染管线构成未来「任意文件只读浏览」的复用面：未来能力可新增端点复用同一管线与安全约束（路径防御、有界读取、二进制嗅探）。本期不为其新增抽象层，仅保持上述原语不被改造为分支对比专用形态。

## Wiring / Test Plan

改动 seam 清单（每层责任唯一）：

| 层 | 落点 | 内容 |
|---|---|---|
| application | `internal/application/dto.go`（GitDiffDTO 于 :210-222） | 新增 branch-diff files 响应 DTO（`{ baseRef, files[] }`，不含 OID）；GitDiffDTO 八字段复用不改 |
| api | `internal/api/tasks.go:19` `TaskBackend` interface | 新增 `GitBranchDiffFiles` / `GitBranchDiffFile` 两个方法签名（精确签名见 D2） |
| api | `internal/api/git.go` `registerGitRoutes`（:18-32） | 注册两个新端点，handler 命名沿用既有风格 `handleGitBranchDiffFiles` / `handleGitBranchDiffFile`；错误映射复用既有 opError 路径（git_api_test.go:307-353 同款断言模式） |
| api 测试 | `internal/api/git_api_test.go:14-59` `mockGitBackend`、`internal/api/p3_review4_fixes_test.go:191-201` `fakeTaskBackend` | `fakeTaskBackend` 增加 branch-diff 两方法的 no-op 默认实现（所有嵌入该 fake 的测试 fixture 自动满足扩展后的 TaskBackend 接口）；`mockGitBackend` 再以 statusFn/diffFn 同款注入模式覆盖为可注入函数，覆盖成功 + 各错误码映射 |
| 组合根 | `cmd/ocdeck-server/main.go`（`SetTaskBackend` :260 / `RebuildRoutes` :310） | 接口方法由 task.Manager 实现满足，无新 wiring；路由注册在 `RebuildRoutes` 前生效的既有顺序不变 |
| frontend | `web/src/api.ts`（git 客户端 :298-342）、`web/src/types.ts`（git 类型 :211-243）、`web/src/pages/TaskWorkbenchPage.tsx`（:762-768 渲染处）、`web/src/components/GitPanel.tsx`（props :47-56） | 前端精确签名：types.ts 新增 `GitBranchDiffFileEntry{ path: string; additions: number; deletions: number; isBinary: boolean }` 与 `GitBranchDiffFilesResult{ baseRef: string; files: GitBranchDiffFileEntry[] }`（无 OID 字段）；api.ts 新增 `gitBranchDiffFiles(taskID: string, base: string) => request<GitBranchDiffFilesResult>('GET', `/tasks/${taskID}/git/branch-diff/files`, undefined, { base })` 与 `gitBranchDiffFile(taskID: string, base: string, path: string) => request<GitDiffResult>('GET', `/tasks/${taskID}/git/branch-diff/file`, undefined, { base, path })`；新增 `GitPanel` 的 `baseRef` prop，由 TaskWorkbenchPage 传入 `task?.base_ref ?? ''` |
| infrastructure | `internal/infrastructure/git/` | 新增 `MergeBase`、`ResolveHeadOID`、`BranchDiffFiles` 原语 + `ErrNoMergeBase`/`ErrUnbornHead` sentinel |

必测错误矩阵：

- infra 层：`MergeBase` exit 0/1/128 → OID/`ErrNoMergeBase`/透传；`ResolveHeadOID` exit 0/1/128 → OID/`ErrUnbornHead`/透传（exit code 语义已 /tmp 实测）；`BranchDiffFiles` 的 numstat 解析、byte-wise 排序、10000 上限、空结果（mb==HEAD）。
- task 层：任务不存在（not_found）；worktree_path 为空（invalid_state，`gitops.go:72-74` 现状口径）；project 不存在（not_found，assertGitRepoTask 内 `gitops.go:36-39`）；dir 任务拒绝（invalid_input，`gitops.go:49` 现状口径）；任务忙（conflict）透传；base 为空 → invalid_input 且零 git 调用；base 非空但无法解析 → invalid_input（仅一次 `ResolveRefOID`/rev-parse 调用，其后 MUST NOT 执行 `ResolveHeadOID`/`MergeBase`/`BranchDiffFiles`/blob 读取，按子命令分别断言调用计数）；`ErrNoMergeBase`/`ErrUnbornHead` → invalid_state；`BranchDiffFiles` 超限或执行失败 → git_error 透传（status 既有口径 `gitops.go:84-86`），file 端点归属校验失败时零 blob 读；`path` 不在变更集合 → invalid_input 且零 blob 读。
- API 层：路由接通 + 错误码 → HTTP 映射（沿用 git_api_test.go 既有断言风格）；files 响应 MUST 断言精确 JSON key 集（存在 `baseRef` 与 `files`，不存在任何 OID 字段）。
- 前端：滚动协同用合成 scroll 事件（jsdom 无布局，`Object.defineProperty` 注入 `scrollLeft`，沿用 `diff-split-ratio.test.tsx` 风格）——必测用例含：两侧最大可滚动距离不同时 clamp 写入目标侧、其 echo 不得回拉源侧；交错序列「A→B 程序写入 → B 在 echo 到达前被用户拖动 → B 旧 echo 迟到到达」断言源侧不回拉、最终收敛；双侧→单侧→双侧（内容与 path 不变、无重建、单侧期间零滚动事件）恢复后首次滚动不消费旧 pending（启用谓词 effect 同步清理）；同值例外自愈：pending 含当前偏移时派发同值用户事件允许本次不同步，下一次滚动事件到达断言另一侧立即追上且无持续事件循环；快速交替拖动最终收敛无振荡；混合列表三色/排序/同路径双条目；tree 投影与折叠；branch 视图按条目类型门控（已提交条目只读不传批注/编辑 props；未提交条目完整能力含 ReviewPanel/commit 区）；`baseRef` prop 默认值填入与空值留白；base 变更 generation 丢弃旧响应；同代乱序（同 baseRef/generation 下快速切换文件 A→B，或连续刷新列表）旧响应不得覆盖新状态；stale 失败响应不得覆盖当前状态；两视图状态隔离；混合列表与 tree 的 UTF-8 byte-wise 排序（含非 ASCII 路径用例，MUST NOT 用 localeCompare）；tree 同级节点按路径段排序与同路径完整次序（已提交 → 已暂存 → 未暂存 → 未跟踪，flat/tree 共用，含同路径四条目断言）；视图切换 leave guard 拒绝时停留。

## 实现阶段划分（tasks 按此拆分）

1. **infra 原语**：`MergeBase` / `ResolveHeadOID` / `BranchDiffFiles` + sentinel + 单元测试。
2. **task + API + wiring**：helper、Manager 方法、TaskBackend 接口、路由、DTO、API 测试（依赖阶段 1）。
3. **前端文件列表与视图**：api.ts/types.ts、混合列表、横向滚动、tree、branch 视图切换与状态生命周期（依赖阶段 2 的契约；UI 结构可与阶段 2 部分并行，联调收口在后）。
4. **DiffViewer 滚动协同**：独立改动面，可与阶段 2/3 并行，最后做集成回归。

## Risks / Trade-offs

- [两侧内容宽度不同时滚动拉扯] → clamp + 程序性 echo 抑制：目标侧停在其最大可行偏移，源侧保持用户偏移不被回拉，无振荡。
- [jsdom 无布局，`scrollLeft`/scroll 事件不可真实触发] → 前端测试用 `Object.defineProperty` 注入 `scrollLeft` + 派发合成 `scroll` 事件断言对侧偏移（沿用 `web/src/__tests__/diff-split-ratio.test.tsx` 的 DOM 检查风格）。
- [混合列表改变用户既有分组心智] → 三色 + 非颜色提示（title/aria）+ 排序稳定；规格已经用户确认。
- [base ref 任意输入的攻击面] → 服务端 `rev-parse --verify --end-of-options` 解析（spec 已定），前端不做信任假设。
- [merge-base 每点击一次进程调用（~6.6ms）] → 可接受；拒绝裸 OID 优化方案（信任边界恶化，见 D2）。
- [branch 视图下 DiffViewer 复用引入只读遗漏风险] → D8 按条目类型门控：已提交条目不传批注/编辑 props，未提交条目传齐完整能力；门控清单逐项在 tasks 中对应验收。

## Migration Plan

无持久化/数据迁移。新端点为纯增量；既有端点与契约不变，可整体回滚（删除新端点 + 前端视图切换代码）。
