# Tasks: codemirror-git-diff

## 1. 后端：infrastructure git 内容读取能力

- [x] 1.1 `internal/infrastructure/git/exec.go` 白名单新增 `show`、`ls-tree`（design D3）
- [x] 1.2 新增新侧工作区文件读取函数（design D4）：`validateDiffPath` 同源校验 → `os.Lstat` 类型分支（ENOENT → 不存在，优先返回；regular → `EvalSymlinks` + `filepath.Rel` 禁锢后有界读取（512KB+1 判定截断）+ 前 8000 字节 NUL 嗅探；symlink/directory 分支由 6.1/6.5 扩展；其他非 regular → 不存在）
- [x] 1.3 新增旧侧读取能力（design D3）：ref 分支 `ls-tree -z` 探测（记录路径/mode 核对，初版仅 100644/100755 为存在，symlink/gitlink 分支由 6.1 扩展）+ `git show <blobOID>`；index 分支 `ls-files -z --stage` 探测（路径精确相等 + stage/mode 核对，无 stage-0 有其他 stage → 冲突哨兵错误）+ `git show <blobOID>`；`ErrOutputTruncated` 真值表沿用
- [x] 1.4 常量与 helper 调整：`DiffMaxBytes` 重命名为内容上限语义（512KB 值不变）；NUL 嗅探 helper 提取复用（design D5/D9）
- [x] 1.5 Go 单测（`internal/infrastructure/git`）：单侧读取（ref/index/工作区三分支）、存在性判定（目录/symlink/gitlink/冲突 stage 组合）、`:(literal)` 防 pathspec magic（含冒号/magic 字符 path、目录 path）、底层 sentinel 错误、git index 不被修改、512KB 截断、单侧 NUL 嗅探

## 2. 后端：DTO、编排与 API

- [x] 2.1 `internal/application/dto.go` `GitDiffDTO` 字段替换（最终为八字段：`oldContent`/`newContent`/`oldExists`/`newExists`/`oldMode`/`newMode`/`isBinary`/`truncated`；mode 字段由 6.1 补充落地）（design D1）
- [x] 2.2 `internal/task/gitops.go` `Manager.GitDiff` 按固定六阶段重编排（词法校验 → task/worktree → rev-parse → 旧侧 → 新侧 → DTO 组装），各层错误包装为对应 `OpError` 码（invalid_input/invalid_state/git_error/internal），多源失败返回首个（design D10、spec 错误映射矩阵）
- [x] 2.3 `internal/api/git.go` handler 逻辑不变，更新注释与 DTO 别名表述；`internal/api/tasks.go` TaskBackend 接口签名联动（如 DTO 引用变化）
- [x] 2.4 API 测试（`internal/api`）：八字段精确 JSON 契约；非法/空值/重复 `untracked` 参数断言 TaskBackend 零调用；注入 Manager `OpError` 验证 HTTP 错误映射（design Verification Strategy）
- [x] 2.5 删除旧 unified-diff 实现（`Diff`/`DiffUntracked`/`finalizeDiffTruncatable`/`isBinaryDiffOutput`/`DiffMaxFiles`），迁移或删除旧专属测试（`git_test.go` 中 `Diff`/`DiffUntracked` 用例、`git_api_test.go` diff 用例；task 层以空 path 调用 `GitDiff` 的回归测试——`oracle_p0p1_test.go:521/544`、`dir_delete_gitops_test.go` 的 `TestGitops_Dir_StatusDiffCommitPush_InvalidInput`（:377）与 `TestGitops_UnknownKind_FailClosed`（:410）——改用合法单文件 path 保留原断言目的：词法校验前置后空 path 会先返回 invalid_input，无法继续验证 dir kind / unknown kind 语义）（design D9）
- [x] 2.6 `internal/task` 层编排测试：①固定失败顺序——构造多源同时失败，断言按词法 → ref 解析 → 旧侧 → 新侧返回首个失败；②八字段派生真值表与错误矩阵——isBinary 任一侧置位后清空两侧、truncated 任一侧置位、invalid_input/invalid_state/git_error（stderr 透传）/internal（含路径与操作名）；③词法校验零调用——空 path、绝对路径、`..`、NUL、`untracked=1`+ref 五类，在失败型假 worktree/git 执行环境下断言均在任何 git/FS 访问前返回 invalid_input

## 3. 前端：编辑器基础设施

- [x] 3.1 `web/package.json`：移除 `diff2html`；按 design D7 依赖闭合清单新增 runtime 依赖（`@codemirror/merge`、`@codemirror/view`、`@codemirror/state`、`@codemirror/language`、`@codemirror/legacy-modes`、`@lezer/highlight`、七个语言包）与 devDependency `jsdom`；重新生成 `pnpm-lock.yaml`
- [x] 3.2 新增共享编辑器工厂 `web/src/components/editor/`（design D6/D7）：主题（跟随现有设计变量）、只读 extensions（`EditorView.editable.of(false)` + `EditorState.readOnly.of(true)` + `lineNumbers()` + `EditorState.lineSeparator.of('\n')` + `syntaxHighlighting(classHighlighter)`）、扩展名提取纯函数（按 D7 伪码）、扩展名→语言 loader 静态映射表（动态 `import()`）
- [x] 3.3 新增 `web/src/components/diff/DiffViewer.tsx`（design D6）：并排 `MergeView`（`a`/`b` 传 EditorStateConfig，`revertControls` 省略）与单列 `EditorView` + `unifiedMergeView`（`original: Text.of(oldContent.split('\n'))`，`mergeControls: false`）；`collapseUnchanged{margin:3,minSize:4}`、`diffConfig{scanLimit:500,timeout:500}`；形态默认值（>1024px 并排 / ≤1024px 单列，`matchMedia`）与 `modeOverride` 语义；卸载与切换时 `destroy()`；组件整 chunk 动态 `import()` 懒加载（design D8）

## 4. 前端：GitPanel 集成

- [x] 4.1 `web/src/api.ts` `gitDiff` 返回类型与 `web/src/types.ts` `GitDiffResult` 同步替换为八字段契约（mode 字段由 6.3 补充落地）
- [x] 4.2 `GitPanel.tsx` diff 区域替换为 DiffViewer：按 spec 渲染优先级完整链实现状态展示（isBinary 提示 → gitlink 提示（6.3 补充）→ 双侧不存在「文件已不存在」→ 空文件 → 无变更 → 截断范围内无可见差异 → merge 视图），截断横幅与 mode 变更横幅（6.3 补充）独立显示；形态切换控件；`modeOverride` 状态由 GitPanel 持有（切换文件与 resize 不重置，GitPanel 卸载才丢弃），经 `modeOverride`/`onModeChange` props 传给懒加载的 DiffViewer（design D6、DR1）
- [x] 4.3 删除 `renderDiffHtml`、`diff2html.min.css` 引入与 `dangerouslySetInnerHTML` 渲染路径；清理 `legacy-components.css` 中仅服务 `.d2h-*` 的规则；新增 `.tok-*` 高亮样式（design D6/D9、G2）
- [x] 4.4 前端测试（vitest + jsdom）：表驱动渲染优先级完整链（含截断横幅共存、全部新增/全部删除）；行尾方向性三组（CRLF→LF、LF→CRLF、末尾换行变化均可见差异）；syntaxHighlighting 生效（关键字 token 类与增删 decoration 共存）；形态默认值与 `modeOverride`；扩展名提取四锚点（`.GO`/`.gitignore`/`dir.with.dot/a.ts`/`name.`）；DiffViewer 挂载/只读/destroy

## 6. I1 契约扩展（mode/symlink/gitlink）与评审修复

- [x] 6.1 后端 mode/symlink/gitlink 支持（spec「文件 diff 查看」已更新、design D1/D3/D4/D5）：ref/index 探测记录按 mode 分支（100644/100755 blob 经 `git show <blobOID>`；120000 blob 即链接目标文本；160000 内容直接取记录 commit OID；tree → 不存在）；新侧 Lstat 分支扩展（symlink → `os.Readlink` 目标文本、mode=120000（禁锢前置由 6.5 补充）；directory → gitlink（toplevel 校验与 dirty 后缀由 6.5 补充）；其他非 regular → 不存在）；`GitDiffDTO` 增加 `oldMode`/`newMode`（不存在侧为空串）；mode 120000/160000 侧不参与 NUL 嗅探
- [x] 6.2 后端测试：chmod-only（内容相同 mode 不同）、symlink 目标变更（blob vs Readlink）、gitlink OID 变更、未初始化子模块（rev-parse 失败 → 存在+空内容）；API 层八字段契约断言更新；修复 `git_api_test.go` 的 `diff git_error` 用例——请求补 `path=a.txt`（I4：生产 Manager 词法校验先行，空 path 不可达 git_error）
- [x] 6.3 前端 mode/gitlink 渲染与评审修复：渲染优先级链按 spec 更新（gitlink 提示紧随 isBinary；无变更/空文件判定含 mode 相同条件；mode 变更横幅独立叠加）；`api.ts`/`types.ts` 八字段契约同步；GitPanel 请求乱序防护（I2：递增 request ID 或 AbortController，仅当前请求可写 diff 状态）；语言 chunk 加载失败降级纯文本（I3：捕获 loader rejection，保持销毁检查）
- [x] 6.4 前端测试：渲染优先级表驱动补 mode/gitlink 用例（chmod-only 显示横幅、symlink merge 目标差异、gitlink 提示不渲染 merge）；请求乱序完成测试（deferred Promise）；loader rejection 降级测试

- [x] 6.5 后端 I5/I6/I7/I8 修复（spec/design 已修订）：directory 分支先 `rev-parse --show-toplevel` 校验 canonical 路径与目标一致（防父仓库发现，I5）；dirty 子模块内容追加稳定 `-dirty` 后缀（`status --porcelain` 判定，I6）；symlink 的 resolved parent 与 directory 的 resolved target 禁锢校验前置到 Readlink/git 执行之前（越界 → invalid_input，I7）；regular mode 改判 owner 执行位 0100（I8）；`internal/api/tasks.go:59` 与 `internal/task/gitops_diff_test.go` 注释六字段→八字段
- [x] 6.6 前端 I6 断言：gitlink 提示对 `-dirty` 后缀 OID 文本的展示断言（补测试，组件逻辑预计无需变更）

- [x] 6.7 后端 I10/I11 修复（spec/design 已修订）：toplevel 输出仅去行尾换行 + EvalSymlinks 归一比较（禁整体 TrimSpace，补尾空格路径测试）；`readGitlinkSide` 对 dirty 探测（status --porcelain）失败返回错误、Manager 映射 git_error 透传 stderr（补损坏子模块 index 的确定性失败测试）

- [x] 6.8 前端换行开关（用户人工 review 新增；spec「diff 视图渲染」与 scenario「切换换行展示」、design D6 换行开关 bullet）：DiffViewer 工具栏「换行」切换控件（默认关=横向滚动）；开启时 `EditorView.lineWrapping` 作用于并排 a/b 与单列全部实例；`wrapOverride` 由 GitPanel 持有经 props 传递（生命周期同 modeOverride）；测试：默认不折行断言、切换后折行断言（jsdom 可断言 `.cm-lineWrapping` 类存在性）、两形态均生效、跨文件切换保留

## 5. 验收与收敛

- [x] 5.1 人工验收清单（人工 review gate 执行：E2E 实证暗色折叠配色/宽屏默认并排/语法高亮；换行开关与断点临界、时耗等子项经用户人工验收通过）
- [x] 5.2 收敛验收：`web/`、`internal/` 下搜索 `diff2html|d2h-|renderDiffHtml|dangerouslySetInnerHTML|diff\.diff|func Diff\(|DiffUntracked|finalizeDiffTruncatable|isBinaryDiffOutput|DiffMaxBytes|DiffMaxFiles|json:"diff"` 零残留（design D9/H2）
- [x] 5.3 全量验证：`go build ./...`、`go test ./internal/...`、`pnpm build`、`pnpm test`（vitest）全部通过
