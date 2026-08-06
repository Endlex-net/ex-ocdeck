# Tasks: fix-git-diff-new-file-and-linenum

## 1. 后端：untracked new-file diff（D1）

- [x] 1.1 `internal/git/ops.go` 新增 `DiffUntracked(ctx, dir, path string) (diff string, truncated bool, err error)`：`git diff --no-index -- <os.DevNull> <path>`；防御性路径校验（拒绝绝对路径、`..` 逃逸、NUL）失败返回 sentinel `git.ErrInvalidDiffPath`（`errors.Is` 可判）；输出判定真值表（design D1）：err==nil（含 stdout 空）→ 正常；ExitCode==1 && stdout 非空 && stderr 为空 → 正常；`errors.Is(ErrOutputTruncated)` && stdout 非空 && stderr 为空 → 512KB 前缀 + truncated=true；其他非 nil 错误透传（注意 exec.go:108 溢出路径上 `errors.As(*exec.ExitError)` 断裂，必须先判 ErrOutputTruncated）；复用 isBinaryDiffOutput（二进制返回 `""` + truncated=true）
- [x] 1.2 `internal/task/gitops.go` `Manager.GitDiff` 增加 `untracked bool` 参数：untracked=true 且 path 空 → invalid_input；untracked=true 且 ref 非空 → invalid_input（在任何 git 命令前校验）；`errors.Is(err, git.ErrInvalidDiffPath)` → invalid_input（禁止字符串匹配），git 执行错误沿用 codeGitError + StderrOf；untracked 分支调用 `git.DiffUntracked`
- [x] 1.3 `internal/api/git.go` `handleGitDiff` 解析 `untracked` 查询参数（值域 absent/0/1，其他 invalid_input），透传到 Manager；同步更新 `internal/api/tasks.go:52` `TaskBackend.GitDiff` 接口签名与全部 fake/mock（`git_api_test.go` 的 `diffFn`/`mockGitBackend`）
- [x] 1.4 `internal/git/git_test.go` 新增用例：非空新文件返回含 `new file mode` 完整 diff；空新文件返回仅元数据 diff；二进制返回 truncated；路径不存在返回错误；绝对路径/`..` 逃逸/NUL 拒绝；ErrOutputTruncated 路径按真值表处理；index 不变性（调用前后 `git diff --cached` 输出一致）；已跟踪路径 `Diff` 行为不变
- [x] 1.5 `internal/task` 层测试：untracked+空 path / untracked+非空 ref 均 invalid_input 且未执行 git 命令（fake/mock 断言）
- [x] 1.6 `internal/api/git_api_test.go` 契约测试：untracked=1 透传、untracked=0 与 absent 行为不变、非法取值 invalid_input、untracked=1&path 空 invalid_input、untracked=1&ref 非空 invalid_input

## 2. 后端：untracked 行数统计（D2）

- [x] 2.1 `internal/git/ops.go` `Status()` 在 numstat 合并后对 `e.Untracked` 条目计行：`count('\n')` + 末行无换行 +1；deletions=0
- [x] 2.2 有界读取顺序（design D2）：`os.Lstat` 非 regular file 跳过 → 前 8000 字节嗅探 NUL（所有 regular file 始终执行，预算耗尽仍嗅探但不再计行；IsBinary、不计行）→ prefix 计入单文件 16MB 与累计 64MB 双预算，续读用 `io.LimitReader(min(16MB-prefixLen, 剩余累计预算)+1)`（按实际读取字节计），超限跳过行计数但保留 IsBinary 标记；IO 错误返回含路径的明确错误
- [x] 2.3 修正 `internal/git/types.go:25` 过时注释（untracked 行数估算本次落地）
- [x] 2.4 `internal/git/git_test.go` 新增用例：100 行新文件 additions=100/deletions=0；无末行换行计数正确；二进制不计行且 IsBinary；symlink 跳过；超 16MB 单文件（含 prefix 计费边界）跳过计数；累计 64MB 预算耗尽后后续文件仅嗅探（二进制仍标记、additions=0）；IO 错误返回明确错误

## 3. 前端：untracked 参数透传（D1 链路前端段）

- [x] 3.1 `web/src/api.ts` `gitDiff` 增加 `untracked` 参数（true 时 query 追加 `untracked=1`）
- [x] 3.2 `web/src/components/GitPanel.tsx` `FileGroup` 增加 untracked 标记；`openDiff` 对 untracked 组传 `untracked=true`；`web/src/types.ts` 如需要同步类型

## 4. 前端：diff 视图行号对齐（D3，@designer lane）

- [x] 4.1 `web/src/styles.css`：`.d2h-code-line/.d2h-code-side-line` 与 `.d2h-code-linenumber/.d2h-code-side-linenumber` 设相同固定 line-height（18px）+ 代码行 `white-space: nowrap`（非 pre，diff2html 模板含换行空白）+ `td{padding:0}`
- [x] 4.2 滚动 owner 收敛：`.d2h-wrapper` 改 `overflow-x: auto`（table 按内容撑宽，真实 DOM 验证）；`.git-diff` 横向关闭（`overflow-x: hidden`）；`.d2h-file-diff` 设 `overflow: visible`（非 scroll container）；禁止给 diff 容器加 transform/新定位上下文
- [x] 4.3 ≤1024px 断点：`.git-panel`（styles.css:1602-1605）显式 `overflow-x: hidden`；`.d2h-wrapper` 横向滚动 owner 身份不变
- [x] 4.4 人工验收：超长行（>200 字符）+ 多 hunk diff，宽屏与 ≤1024px 双视口滚动至中部/底部，行号逐行对齐；浏览器端点击 untracked 文件确认 new-file diff 正常渲染

## 5. 验证

- [x] 5.1 `go test ./internal/git/ ./internal/task/ ./internal/api/` 通过；`gofmt -l` 仅检查本 change 修改的 Go 文件（`internal/git/ops.go internal/git/types.go internal/git/git_test.go internal/task/gitops.go internal/api/git.go internal/api/tasks.go internal/api/git_api_test.go` 及新增文件），无输出
- [x] 5.2 `web/` 下 `npm run build` 与 `npm test` 通过
- [x] 5.3 `/opsx-verify fix-git-diff-new-file-and-linenum` 通过
