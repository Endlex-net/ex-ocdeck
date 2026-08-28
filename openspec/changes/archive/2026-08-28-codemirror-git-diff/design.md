# Design: codemirror-git-diff

## Context

任务工作台 Git 面板的 diff 查看目前链路为：后端 `GET /api/v1/tasks/{id}/git/diff?ref=&path=&untracked=` 返回 unified diff 文本（`GitDiffDTO{Diff, Truncated}`，`internal/application/dto.go:155`），前端 `GitPanel.tsx` 用 diff2html 渲染为静态 HTML 并经 `dangerouslySetInnerHTML` 注入。diff2html 只提供 diff 级别着色，无语法高亮、无并排对照。

后续「工作区文件读/编辑」特性已选定 CodeMirror 6 作为编辑器栈。本 change 是该方向的第一步：用 CodeMirror 6 的 merge 能力替换 diff2html，统一编辑器依赖。行为规范（内容来源语义、参数校验、容量与二进制、错误映射、渲染形态与优先级）由 `specs/git-operations/spec.md` delta 的「文件 diff 查看」（MODIFIED）与「diff 视图渲染」（ADDED）两个 requirement 承载，本文档只记录机制与决策理由；涉及同一契约时按 requirement 名引用，不改写。

现状关键事实（写作前已核实）：
- git 子命令白名单（`internal/infrastructure/git/exec.go:14-30`）含 `diff`/`ls-files` 等，**不含** `show`/`ls-tree`。
- 单次 git 命令输出硬上限 `execOutputLimit = 16MB`（`exec.go:33`）；diff 文本上限 `DiffMaxBytes = 512KB`（`ops.go:308`）。
- untracked 路径防御 `validateDiffPath`（`ops.go:425`）：拒绝绝对路径/`..`/NUL。
- `@codemirror/merge@6.12.2`（npm registry latest），依赖 `@codemirror/view ^6.17.0`、`@codemirror/state ^6.0.0`、`@codemirror/language ^6.0.0`（`@codemirror/state`、`@codemirror/language` 需显式声明为直接依赖以引用其 API）。
- `@codemirror/merge` API（unpkg `dist/index.d.ts` 核实）：导出 `MergeView`、`unifiedMergeView`、`updateOriginalDoc`、`goToNextChunk/goToPreviousChunk` 等；`MergeConfig` 含 `orientation`、`revertControls`、`highlightChanges`、`gutter`（changed-line 标记，**非行号**）、`collapseUnchanged{margin,minSize}`、`diffConfig`；`UnifiedMergeConfig` 含 `original`、`highlightChanges`、`gutter`、`mergeControls`、`collapseUnchanged`、`syntaxHighlightDeletions` 等；只读无 merge 级开关，须在 `a`/`b` 的 extensions 里配 `EditorView.editable.of(false)` + `EditorState.readOnly.of(true)`；`a`/`b` 类型为 `EditorStateConfig`（构造器只读取 `doc`/`selection`/`extensions` 并自行创建 state，传预建 `EditorState` 会丢失 extensions）；`diffConfig` 默认仅 `{scanLimit: 500}`，`timeout` 为可选项。
- Go 无官方 lezer 语言包；`@codemirror/legacy-modes` 的 `mode/go.js` 导出 `go` stream parser，经 `StreamLanguage.define(go)` 接入（unpkg 核实）。
- 前端窄屏断点 ≤1024px 已存在（`web/src/legacy-components.css:1276`，工作台布局同断点）。

## Goals / Non-Goals

**Goals:**
- diff 视图切换为 CodeMirror 6 merge 渲染：语法级高亮、单列/并排可切换、只读。
- 后端 diff 端点响应从 diff 文本改为两侧版本内容（请求侧参数形态不变）。
- 移除 diff2html 依赖与 `dangerouslySetInnerHTML` 渲染路径。
- 编辑器封装（主题、只读配置、语言加载）沉淀为可复用单元，为后续文件读/编辑特性预留。

**Non-Goals:**
- 工作区文件树浏览、任意文件读/编辑、diff 视图内编辑（accept/reject chunk 不启用）。
- LSP/代码跳转。
- git status 分组、提交勾选、commit/push 交互逻辑不变。
- 新的移动端专属交互（窄屏仅做形态默认值适配）。

## Decisions

### D1：端点与请求参数不变，响应 DTO 替换为两侧内容契约

保留 `GET /api/v1/tasks/{id}/git/diff?ref=&path=&untracked=`；`GitDiffDTO` 字段从 `{Diff, Truncated}` 替换为 `{OldContent, NewContent, OldExists, NewExists, OldMode, NewMode, IsBinary, Truncated}`（JSON：`oldContent`/`newContent`/`oldExists`/`newExists`/`oldMode`/`newMode`/`isBinary`/`truncated`；mode 为 git 八进制 mode 文本如 `100644`/`100755`/`120000`/`160000`，不存在侧为空串）。八字段的派生规则与前端渲染优先级以 spec「文件 diff 查看」的响应派生规则段为唯一 normative 来源，本文档不改写。mode 字段的引入原因：mode-only 变更（chmod）与非 regular 类型（symlink/gitlink）在旧 unified diff 下有可见输出，新契约必须保留其用户可见语义（I1，用户裁决「保留旧语义」）。

- 备选 A（新增 `/git/file-content` 端点、保留旧端点）：双端点并存导致两套语义维护，且旧端点立即成为死代码（GitPanel 是唯一调用方）——否决。
- 备选 B（响应仍带 diff 文本 + 两侧内容双承载）：冗余且必然漂移——否决。

### D2：path 必填，空 path 返回 invalid_input

merge 视图按单文件渲染，历史「空 path=全仓 diff」语义无对应承载；GitPanel 恒传 path（`GitPanel.tsx:94`）。属请求侧边缘收紧，唯一调用方无感知。spec「文件 diff 查看」的「非法参数组合」scenario 已覆盖。

### D3：旧侧内容获取——literal pathspec 探测 + blob OID 读取，禁止 stderr 文案匹配

- `ref` 非空：`rev-parse --verify --end-of-options` 解析 OID（沿用现状，`ops.go:321`）→ `git ls-tree -z <oid> -- ":(literal)<path>"` 探测：逐条解析 `-z` 记录并核对记录的路径与对象 type/mode，仅记录路径与请求 path 精确相等的条目参与判定——regular blob（mode 100644/100755）→ 存在，`git show <blobOID>` 读取；symlink（120000）→ 存在，blob 内容即链接目标文本（同 `git show <blobOID>`）；gitlink（160000）→ 存在，内容直接取记录中的 commit OID 文本（无需 git show）；目录（tree）等 → 不存在。旧侧 mode 取自探测记录。
- `ref` 为空：`git ls-files -z --stage -- ":(literal)<path>"` 探测：逐条解析记录并核对记录路径与请求 path **精确相等**（`:(literal)` 不等于精确匹配——实测 `git ls-files --stage -- ':(literal)<目录>'` 会返回该目录下全部子路径记录）、stage 为 0；mode 100644/100755/120000 → 存在并经 `git show <blobOID>` 读取，160000 → 存在且内容取记录 OID；无任何匹配记录 → 旧侧不存在；存在同 path 记录但无 stage-0（仅其他 stage）→ invalid_state 冲突错误。旧侧 mode 取自探测记录。
- 探测 MUST 用 `:(literal)` 包裹：裸 path 会被 pathspec magic 扩大匹配（实测 `git ls-files ... ':(exclude)...'` 类输入可返回整个 index）。
- 内容读取 MUST 以探测取得的 blob OID 为对象，MUST NOT 以 `<ref>:<path>` 二次拼路径（实测 `git show HEAD:<目录>` 输出 tree listing 而非报错，会把目录误当文件内容）。
- 白名单新增 `show`、`ls-tree`（`ls-files`/`rev-parse` 已在列）。
- `git show` 输出经既有 `run()` 有界读取（16MB 硬上限）；blob 超上限走 `ErrOutputTruncated` 真值表（沿用 `DiffUntracked` 模式：stdout 非空且 stderr 空 → 前缀 + truncated，否则透传错误）。
- 备选（`git show` 失败 stderr 匹配 "does not exist" 类文案判定不存在）：git 版本间文案不稳定，spec 已明确禁止——否决。
- 测试锚点：含冒号/magic 字符的 path、目录 path、symlink、gitlink（submodule）、冲突 stage 组合。

### D4：新侧内容获取——受限文件系统读取

`filepath.Join(worktree, path)` → `validateDiffPath` 同源校验（绝对路径/`..`/NUL）→ `os.Lstat` 类型分支：
- ENOENT → 新侧存在性=false，**优先返回，不再执行后续校验**。
- regular file → 禁锢校验：对 worktree 根与目标分别 `filepath.EvalSymlinks` 得到 rootReal/targetReal，以 `filepath.Rel(rootReal, targetReal)` 判定——结果为绝对路径、`..` 或以 `../` 开头即越界（invalid_input）；MUST NOT 用 `strings.HasPrefix` 字符串前缀判定（会把 `/worktree-other` 误判为 `/worktree` 内部）→ 以校验后的 targetReal 打开并有界读取（512KB+1 判定截断）；mode 依 owner 执行位（0100）为 `100644`/`100755`。
- symlink → 存在，`os.Readlink` 目标文本为内容（读取链接文本而非跟随；MUST 先校验该链接的 resolved parent 位于 root 内——防中间级 symlink 逃逸，越界 invalid_input；链接目标本身不受禁锢），mode=`120000`。
- directory → 按 gitlink 工作区侧处理（能进入 diff 查询的 directory 即 submodule 工作目录）：MUST 先校验 resolved 路径位于 root 内（越界 invalid_input）再执行任何 git 命令；存在；`git -C <path> rev-parse --show-toplevel` 输出仅去除行尾换行、EvalSymlinks 归一后 MUST 与目标目录 canonical 一致（MUST NOT 整体 TrimSpace——路径自身首尾空白合法，I10）——真实未初始化子模块会向上发现父仓库，不一致/失败视为未初始化（内容为空，非错误）——一致后 `rev-parse HEAD` 取 OID，`status --porcelain` 非空时追加稳定 `-dirty` 后缀；status 执行失败 MUST 返回错误（Manager 映射 git_error 透传 stderr，I11 用户裁决），MUST NOT 静默按 clean 处理；mode=`160000`。
- 其他非 regular 类型（fifo/socket 等）→ 新侧存在性=false。

已知残余风险：EvalSymlinks 与 open 之间存在 TOCTOU 窗口（见 Risks）。

### D5：容量与二进制口径沿用既有常量

单侧内容上限 512KB（复用 `DiffMaxBytes` 值，重命名为内容上限语义）；二进制嗅探为内容前 8000 字节含 NUL（与 status 行计数同口径），仅对 regular blob 侧判定——mode 为 120000（链接目标文本）/160000（commit OID）的侧 MUST NOT 参与嗅探（其内容天然为文本）。`isBinary` 独立成字段、`truncated` 只表大小截断——现状用 `truncated=true` 兼任二进制含义（`ops.go:364-365`），新契约拆开，前端据 `isBinary` 显示「不支持」标记，用户可见语义不变。派生规则细节按 D1 约定引用 spec，不改写。

### D6：前端 DiffViewer 组件——merge 双形态 + 共享编辑器工厂

新增 `web/src/components/diff/DiffViewer.tsx`：
- 并排：`new MergeView({ parent, a: { doc: oldContent, extensions: [...] }, b: { doc: newContent, extensions: [...] }, highlightChanges: true, gutter: true, collapseUnchanged: { margin: 3, minSize: 4 }, diffConfig: { scanLimit: 500, timeout: 500 } })`；`revertControls` 直接省略（缺省即无 revert 控件）。
- 单列：`new EditorView({ doc: newContent, extensions: [ ...shared, unifiedMergeView({ original: Text.of(oldContent.split('\n')), mergeControls: false, collapseUnchanged: { margin: 3, minSize: 4 }, diffConfig: { scanLimit: 500, timeout: 500 } }) ], parent })`。注意 `original` MUST 以 `Text.of(oldContent.split('\n'))` 传入（`Text` 来自 `@codemirror/state`）：merge 对 string 类型的 original 会自行 `split(/\r?\n/)` 规范化（`@codemirror/merge@6.12.2` 源码），吞掉旧侧 CRLF，导致「旧侧 CRLF、新侧 LF」无可见 diff。
- 共享 extensions（并排两侧与单列当前文档）：只读（`EditorView.editable.of(false)` + `EditorState.readOnly.of(true)`）、`lineNumbers()`、`EditorState.lineSeparator.of('\n')`（`\r` 保留为文档字符——默认 state 创建会把 `\r\n`/`\r` 统一为 `\n`，吞掉纯行尾变更）、`syntaxHighlighting(classHighlighter)`（`@codemirror/language` 导出样式挂接、`@lezer/highlight` 导出 `classHighlighter`——语言包只产生语法树，不显式挂高亮样式表则无可见 token 着色；CSS 为稳定的 `.tok-*` 类配置设计 token）、主题、语言扩展。
- 形态默认值唯一：>1024px 并排、≤1024px 单列（`matchMedia('(max-width: 1024px)')`，与既有工作台断点一致）；`modeOverride: 'unified' | 'side-by-side' | null` 表达用户选择，非 null 时优先于默认值，跨文件切换与 resize 保留，GitPanel 卸载即丢弃（不持久化）。（DR1）
- 行号范围：并排两侧各自显示本侧行号；单列仅当前文档行号，删除块不做旧侧行号（DR2），不实现自定义 old-line gutter。
- 换行开关（用户人工 review 新增需求）：工具栏「换行」切换控件，默认关（横向滚动，保持既有验收）；开启时通过 `EditorView.lineWrapping` 作用于全部编辑器实例（并排 a/b 两侧与单列），行号与折行行对齐由 CM6 内部机制保证；状态 `wrapOverride` 由 GitPanel 持有（与 `modeOverride` 同生命周期：跨文件切换与视口变化保留、GitPanel 卸载丢弃、不持久化），经 props 传入 DiffViewer；切换时重建编辑器（与形态切换同一销毁-重建路径）。
- mode/gitlink 呈现（I1 契约扩展）：任一侧 mode=160000 → 子模块变更提示（展示两侧 OID 文本，不渲染 merge 视图）；两侧均存在且 mode 不同 → 权限/类型变更横幅独立叠加；「无变更」判定含 mode 相同条件。呈现规则以 spec 派生规则段为唯一来源，本节不改写。
- 组件卸载与形态切换 MUST 调 `destroy()` 释放编辑器实例。
- `diffConfig.timeout: 500` 为显式配置（merge 默认仅 `scanLimit: 500`、无 timeout），是最坏输入不冻结的实际兜底。
- 演进预留边界（YAGNI）：共享编辑器工厂仅限主题、只读 extensions 与语言加载函数，本 change 不抽象通用可编辑文档模型。

### D7：语法高亮按扩展名懒加载，未识别降级纯文本

扩展名 → 语言 loader 静态映射表。扩展名提取为浏览器端纯函数（MUST NOT 引入 Node `path` polyfill——`vite.config.ts` 与 `package.json` 均无对应实现）：`name` = git path 最后一个 `/` 之后的文件名；`dot = name.lastIndexOf('.')`；返回值 = `dot > 0 && dot < name.length - 1 ? name.slice(dot).toLowerCase() : ''`——**返回值包含前导点**，与映射表键（`.go`/`.ts` 等）一致；dotfile（如 `.gitignore`，dot=0）、无后缀（dot=-1）、末尾点（`name.`，dot 为末位）均返回空串 → 纯文本。测试锚点断言：`.GO → ".go"`（大小写归一）、`.gitignore → ""`（dotfile）、`dir.with.dot/a.ts → ".ts"`（目录名含点不影响）、`name. → ""`（末尾点）。命中后经动态 `import()` 按需加载对应语言包。映射唯一如下：

| 扩展名（小写化后） | loader |
|---|---|
| `.md` | `@codemirror/lang-markdown` `markdown()` |
| `.json` | `@codemirror/lang-json` `json()` |
| `.yaml` / `.yml` | `@codemirror/lang-yaml` `yaml()` |
| `.go` | `@codemirror/legacy-modes/mode/go` → `StreamLanguage.define(go)`（无官方 lezer 包） |
| `.js` | `@codemirror/lang-javascript` `javascript()` |
| `.jsx` | `javascript({ jsx: true })` |
| `.ts` | `javascript({ typescript: true })` |
| `.tsx` | `javascript({ typescript: true, jsx: true })` |
| `.py` | `@codemirror/lang-python` `python()` |
| `.html` | `@codemirror/lang-html` `html()` |
| `.css` | `@codemirror/lang-css` `css()` |
| 其余 | 无高亮纯文本，不报错 |

`package.json` 依赖闭合（唯一清单）：runtime 直接依赖 `@codemirror/merge`、`@codemirror/view`、`@codemirror/state`、`@codemirror/language`、`@codemirror/legacy-modes`、`@lezer/highlight`（`classHighlighter`）及上表七个语言包（`@codemirror/lang-markdown`/`lang-json`/`lang-yaml`/`lang-javascript`/`lang-python`/`lang-html`/`lang-css`）；devDependencies 新增 `jsdom`（vitest 组件测试用）；MUST NOT 引入 `codemirror` umbrella 包（其类型只重导出 `EditorView`，不覆盖 `lineNumbers`/`EditorState`，且带入不需要的功能面）。

### D8：编辑器代码整 chunk 懒加载

`DiffViewer` 经动态 `import()` 加载，CodeMirror 相关依赖与主 bundle 分离，避免拖累非 git 页面首屏（项目现状无任何编辑器/高亮依赖，`web/package.json`）。

### D9：diff2html 与旧 unified-diff 链路的收敛范围

- 前端：`web/package.json` 移除 `diff2html`；`GitPanel.tsx` 删除 `renderDiffHtml` 与 `diff2html.min.css` 引入、`dangerouslySetInnerHTML` 渲染路径；清理样式表中仅服务 `.d2h-*` 的规则。highlight.js 为 diff2html 传递依赖，随之一并消失，无需单独处理。
- 后端：新链路测试就位后，删除无调用方的 unified-diff 实现（`Diff`/`DiffUntracked`/`finalizeDiffTruncatable`/`isBinaryDiffOutput`）及其专属常量 `DiffMaxFiles`；保留并复用 `validateDiffPath`、512KB 常量（按 D5 重命名）与 NUL 嗅探 helper；不允许新旧 DTO/实现并存（`Manager.GitDiff` 只保留新链路）。
- 收敛验收：`pnpm-lock.yaml` 随依赖移除重新生成；旧 unified-diff 专属测试（`internal/infrastructure/git/git_test.go` 中 `Diff`/`DiffUntracked` 用例、`internal/api/git_api_test.go` diff 用例）迁移或删除；清理仅引用旧链路的注释与样式（`internal/api/git.go`、`internal/application/dto.go` 中「unified diff 文本」表述、`legacy-components.css` 中 `.d2h-*` 规则）；以 `web/`、`internal/` 路径下搜索 `diff2html|d2h-|renderDiffHtml|dangerouslySetInnerHTML|diff\.diff|func Diff\(|DiffUntracked|finalizeDiffTruncatable|isBinaryDiffOutput|DiffMaxBytes|DiffMaxFiles|json:"diff"` 零残留作为收敛验收（`openspec/` 目录保留需求与设计记录，不在验收范围；`DiffMaxBytes` 按 D5 重命名后旧名亦应零残留）。

### D10：分层落点与固定执行顺序

- `internal/infrastructure/git/`：新增「按版本读取文件内容」能力（D3 探测+读取），whitelist 扩展 `show`/`ls-tree`；新侧 FS 读取函数（D4）放 infrastructure 层，worktree 根由 Manager 传入。
- `internal/task/gitops.go` `Manager.GitDiff`：按固定阶段编排——①纯词法校验（untracked 组合、path 非空、绝对路径/`..`/NUL，在任何 git 命令与文件读取前完成）→ ②task 存在性与 worktree 校验 → ③ref `rev-parse` 解析 → ④旧侧探测+读取 → ⑤新侧 metadata/containment/内容读取 → ⑥DTO 组装；多源失败返回首个失败。Manager MUST 将各层错误包装为对应 `OpError` 码（invalid_input/invalid_state/git_error/internal，矩阵以 spec「文件 diff 查看」错误映射段为唯一来源）；未经 `OpError` 包装的错误会在 api 层降为 generic internal（`internal/api/tasks.go:561`），违反 spec 错误矩阵。
- `internal/application/dto.go`：`GitDiffDTO` 字段替换；`internal/api/git.go` 仅注释与 DTO 别名联动，handler 逻辑不变。
- 前端：`api.ts` `gitDiff` 返回类型、`types.ts` `GitDiffResult` 同步替换；`GitPanel.tsx` diff 区域换 DiffViewer。

## Risks / Trade-offs

- [双侧 512KB 内容的客户端 diff 计算开销] → 上限有界（总计 ≤1MB）；显式配置 `diffConfig {scanLimit: 500, timeout: 500}`（D6）；`collapseUnchanged` 折叠未变区域；最坏输入（双 512KB 高差异）浏览器实测纳入验收；截断时显示横幅，语义与现状「有界前缀+截断标记」等价。
- [未解决冲突文件从「降级展示」变为 invalid_state 错误] → 现状 `git diff` 对 unmerged path 输出 `* Unmerged path` 本就无法构成有效 diff；明确错误优于误导性展示。spec 已立 scenario。
- [`git show` 读取大 blob 占用内存（≤16MB）] → 沿用 `run()` 有界读取与 `ErrOutputTruncated` 真值表；512KB 业务上限远低，超限路径只截断不失败。
- [AI agent 并发修改同一 worktree 导致 diff 快照过期] → 与现状一致（快照语义 + 用户手动刷新）；文件监听/自动刷新属后续文件管理特性范畴。
- [形态切换重建编辑器丢滚动位置] → 切换时重建为可接受代价（diff 视图无编辑状态）；滚动位置不保证保留。
- [NUL 嗅探不覆盖无 NUL 的非法 UTF-8，Go JSON 编码会替换非法字节] → 契约定位为 UTF-8 文本（spec 派生规则段已注明），不接受字节级 round-trip 需求。
- [EvalSymlinks 与 open 之间存在 TOCTOU 窗口] → 威胁模型为单用户本机工具，不含恶意本地并发进程；若未来威胁模型变化，需升级为 openat/no-follow 方案。

## Verification Strategy

- Go 单测（`internal/infrastructure/git`、`internal/task`）：内容来源三分支、存在性判定（目录/symlink/gitlink/冲突 stage）、`:(literal)` 防 pathspec magic、错误码矩阵、固定失败顺序（在 `internal/task` 层构造多源同时失败，断言按词法 → ref 解析 → 旧侧 → 新侧返回首个失败——API 层 mock 无法观察 Manager 内部阶段顺序）、git index 不被修改（操作前后 index 状态不变）、512KB 截断与二进制清空两侧内容；I1 扩展锚点：chmod-only（内容相同 mode 不同）、symlink 目标变更（blob vs Readlink）、gitlink OID 变更、真实未初始化子模块（toplevel 校验拦截父仓库发现 → 存在+空内容+mode=160000）、dirty 子模块（`-dirty` 后缀）、中间级 symlink 逃逸（symlink resolved parent / directory resolved 禁锢越界 → invalid_input）、权限位口径（0100 owner 执行位）；I10/I11 锚点：尾空格路径（toplevel 比较不 TrimSpace）、dirty 探测失败 → git_error（损坏子模块 index 构造确定性失败）。
- API 测试（`internal/api`）：八字段精确 JSON 契约；API 层仅对非法/空值/重复的 `untracked` 参数断言 TaskBackend 零调用（handler 内解析即拒绝）；path 非空、组合约束与路径防御的「零 git/FS 调用」断言放 `internal/task` 测试（这些校验在 Manager 内，必然经过 facade 调用）；API 层对后类错误注入 Manager `OpError` 验证 HTTP 错误映射。
- 前端测试（vitest + jsdom）：表驱动覆盖渲染优先级完整链（isBinary、gitlink、双侧不存在、空文件、无变更、mode 变更横幅、截断前缀相同、merge 视图、全部新增/全部删除，及截断横幅与各状态提示的共存）；行尾方向性（CRLF→LF、LF→CRLF、末尾换行变化均呈现可见差异，覆盖 string original 被 merge 规范化的回归）；`syntaxHighlighting(classHighlighter)` 生效（已知语言文件的关键字 token 类与增删 decoration 同时存在）；形态默认值与 `modeOverride` 语义；扩展名提取伪码的四锚点；DiffViewer 挂载/只读/destroy 生命周期；GitPanel 请求乱序防护（deferred Promise 乱序完成）与语言 chunk 加载失败降级。
- 布局与时耗（1024/1025 断点、长行横向滚动对齐、双 512KB 最坏输入耗时、换行开关四种 mode/wrap 组合的真实折行/gutter 对齐/折叠横条叠加表现）：jsdom 测量能力受限，以人工验收清单承载（写入 tasks 验收项），本 change 不为只读 diff 视图引入浏览器测试基础设施。

## Open Questions

- 无。语言首版集合已经用户确认（md/json/yaml/go/js-ts/python/html/css，见 D7）。
