# Proposal: fix-git-diff-new-file-and-linenum

## Why

任务界面的 git 面板存在两个缺陷：

1. **untracked 新文件无法查看 diff**：后端只用普通 `git diff`（`internal/git/ops.go:168`），git 对 untracked 文件永远输出空，用户点击新文件只能看到"该文件暂无可展示的 diff"。
2. **窄屏/横向滚动时行号错乱**：diff2html 大 table 直接渲染，`≤1024px` 时 `.git-diff` 切为 `overflow:visible`（`web/src/styles.css:1619-1622`）且 `.d2h-wrapper{overflow:hidden}` 裁切横向溢出，gutter 行号与代码行错位。

此外 untracked 文件在文件列表中 additions/deletions 恒为 0/0（numstat 不含 untracked），用户无法感知新文件体量。

## What Changes

- 后端新增 `git.DiffUntracked` 并由 `Manager.GitDiff` 分流：对 untracked 路径用 `git diff --no-index -- /dev/null <path>` 合成 new-file unified diff（只读、不改 index），输出天然带 `new file mode` 头，前端 diff2html 直接渲染为全新增视图。
- 后端 `git.Status` numstat 统计扩展：untracked 文件按实际行数计入 additions（二进制文件不计行数）。
- 前端 diff 视图布局修复：禁止代码折行（`white-space:nowrap`）、统一行号 gutter 与代码行行高、gutter 绝对定位锚定进滚动内容（`.d2h-code-wrapper{position:relative}`）、横向滚动唯一 owner 为 `.d2h-wrapper`，修复窄屏（≤1024px）断点下滚动容器切换导致的错位。
- 更新 `git-operations` spec：文件 diff 查看需求补充 untracked 支持、状态查询需求补充 untracked 行数统计、新增 diff 渲染对齐需求。

非目标：不引入虚拟滚动库；不改 commit/push 流程；不改已跟踪文件的 diff 行为；不改 DDD/分层结构。

## Capabilities

### New Capabilities

（无）

### Modified Capabilities

- `git-operations`: 「文件 diff 查看」需求扩展支持 untracked 新文件；「工作区状态查询」需求补充 untracked 文件行数统计；新增 diff 视图渲染对齐（行号与代码行同步）需求。

## Impact

- **后端（Go）**：
  - `internal/git/ops.go`：新增 `DiffUntracked()`（`git diff --no-index`，只读、不改 index）、`Status()` 对 untracked 计行（16MB 单文件 + 64MB 累计容量保护）。`git.Diff` 既有签名不变。
  - `internal/git/exec.go`：无需改动（`diff` 子命令已在白名单内；溢出路径的 error 链差异由 DiffUntracked 判定逻辑兼容）。
  - `internal/task/gitops.go`：`Manager.GitDiff` 增加 untracked 参数与用例不变量校验。
  - `internal/api/git.go`：`handleGitDiff` 解析 `untracked` 查询参数（值域 absent/0/1）。
  - `internal/git/git_test.go`、`internal/task`、`internal/api/git_api_test.go`：新增 untracked diff / numstat / 参数校验用例。
- **前端（React + CSS）**：
  - `web/src/api.ts`、`web/src/components/GitPanel.tsx`、`web/src/types.ts`：`untracked` 参数从文件分组透传到 API 请求。
  - `web/src/styles.css`：diff 视图 line-height/white-space/overflow 修复（横向滚动 owner 固定为 `.d2h-wrapper`），含 ≤1024px 断点。
- **API 契约**：`GitDiffDTO`/`GitStatusDTO` 结构不变；`GET /tasks/{id}/git/diff` 新增可选 `untracked` 查询参数（absent/0/1，缺省行为不变）；untracked 文件的 diff 内容与 numstat 数值语义变化。
- **Spec**：`openspec/specs/git-operations/spec.md` 经 delta 同步更新。
