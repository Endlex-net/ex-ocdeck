# Delta: git-operations

## MODIFIED Requirements

### Requirement: 工作区状态查询
系统 SHALL 提供任务 worktree 的 git 状态查询（基于 `git status --porcelain=v2 -z -uall`），展示已暂存/未暂存/未跟踪文件及增删行数统计（`git diff --numstat [--cached]`）。变更文件数 MUST 有上限（默认 10000），超限返回明确错误而非截断。

未跟踪（untracked）文件的行数统计 SHALL 按文件实际行数计入 additions（deletions=0）：行计数语义与 git 一致（`count('\n')`，末行无换行符时 +1）；所有 regular file MUST 始终执行二进制嗅探（前 8000 字节含 NUL → 标记 IsBinary 且不计行），嗅探不受行计数预算约束；非 regular file（symlink/fifo 等）MUST 跳过；行计数读取受容量保护——单文件超过 16MB 或全部 untracked 文本读取累计超过 64MB（按实际读取字节计）时 MUST 跳过相应文件的行计数（additions 保持 0，IsBinary 仍正确标记，视为容量保护而非失败）；文件读取 IO 错误 MUST 返回明确错误（含文件路径与操作上下文），MUST NOT 静默置零；唯一例外：status 快照后文件被并发删除（ENOENT）属正常竞态，MUST 跳过该文件计数且不视为错误（代码中须注释区分 ENOENT 竞态跳过与其他 IO 错误）。

#### Scenario: 查看任务改动
- **WHEN** 用户在任务的 git 面板请求状态
- **THEN** 系统返回文件级状态列表与增删行数

#### Scenario: 未跟踪新文件显示行数
- **WHEN** worktree 中新增一个 100 行的未跟踪文本文件
- **THEN** 状态列表中该文件 additions=100、deletions=0

#### Scenario: 未跟踪二进制文件
- **WHEN** worktree 中新增一个未跟踪的二进制文件
- **THEN** 该文件标记 IsBinary，不计入行数统计

### Requirement: 文件 diff 查看
系统 SHALL 提供 unified diff 文本（`git diff [ref] -- <path>` 输出），供前端 diff2html 渲染。服务端 MUST 限制单次 diff 字节数与文件数；二进制文件 MUST 返回空内容 + 截断标记，超限文本文件 MUST 返回有界前缀（512KB）+ 截断标记。

对未跟踪（untracked）文件，系统 SHALL 支持以只读方式合成 new-file unified diff（`git diff --no-index -- /dev/null <path>`，空设备路径经 `os.DevNull` 隔离），输出含 `new file mode` 头，供前端渲染为全部新增视图。该路径 MUST NOT 修改 git index（禁止 intent-to-add）。

调用约定：`untracked` 查询参数值域为 absent / `0` / `1`，其他值 MUST 返回 invalid_input；`untracked=1` 时 path MUST 非空且 ref MUST 为空，否则 MUST 返回 invalid_input（在任何 git 命令前校验）。`untracked=1` 为调用方声明的展示模式，服务端 MUST NOT 二次探测文件是否确为 untracked。untracked 分支 MUST NOT 使用 `:(literal)` pathspec 包裹（`--no-index` 按文件系统路径比较），但 MUST 防御性拒绝绝对路径、`..` 逃逸与 NUL 字符。

输出判定 SHALL 依据 `git diff --no-index` 实测契约（exit=1 同时覆盖"有差异""空新文件""路径不存在"三种情况），判定矩阵唯一如下：err 为 nil（含 stdout 空）→ 正常返回；exit=1 且 stdout 非空且 stderr 为空 → 正常 diff 输出；输出截断保护（ErrOutputTruncated）且 stdout 非空且 stderr 为空 → 返回 512KB 前缀并标记 truncated；其他任何非 nil 错误（exit>1、stdout 为空、或 stderr 非空）MUST 按错误透传。错误码映射：路径防御校验失败（绝对路径/`..`/NUL）MUST 映射 invalid_input；git 执行失败（路径不存在、读取失败）MUST 映射 git_error 并透传 stderr。空新文件返回仅含 `new file mode` 元数据（无 hunk）的 diff。二进制 untracked 文件 MUST 返回空 diff + 截断标记。

#### Scenario: 查看单文件 diff
- **WHEN** 用户选择某个改动文件
- **THEN** 系统返回该文件的 unified diff 文本，前端渲染对比视图

#### Scenario: 查看未跟踪新文件 diff
- **WHEN** 用户选择 untracked 分组中的新文件
- **THEN** 系统返回以 `/dev/null` 为旧侧的 new-file unified diff，前端渲染为全部新增行视图，且 git index 未被修改

#### Scenario: 查看空的新文件
- **WHEN** 用户选择 untracked 分组中的空文件
- **THEN** 系统返回仅含文件元数据（new file mode，无 hunk）的 diff，前端正常渲染文件头

#### Scenario: 非法参数组合
- **WHEN** 请求 `untracked=1` 且 path 为空或 ref 非空，或 untracked 取值不在值域内
- **THEN** 系统返回 invalid_input 错误，且未执行任何 git 命令

#### Scenario: 超大文件
- **WHEN** 文件超过服务端限制或为二进制
- **THEN** 前端显示截断/不支持标记，不冻结浏览器

## ADDED Requirements

### Requirement: diff 视图渲染对齐
diff 视图 SHALL 在任意视口宽度与滚动位置下保持行号（gutter）与代码行一一对齐：行号与代码 MUST 位于同一表格行且行高一致（相同固定 line-height）；代码行 MUST NOT 折行（`white-space: nowrap`，长行横向滚动；不得用 `pre`——diff2html 模板含换行空白会致行高爆炸）；横向滚动的唯一 owner MUST 是 `.d2h-wrapper`（其余容器 `.git-diff`/`.git-panel`/`.d2h-file-diff` MUST 关闭横向滚动），且 diff 表格按内容撑宽（`width: max-content; min-width: 100%`）；diff2html gutter 为 `position:absolute` 的 td，MUST 通过 `.d2h-code-wrapper{position:relative}` 锚定进滚动内容内部，MUST NOT 给 `.d2h-file-diff`/`.d2h-diff-table` 添加 transform/filter 或新定位上下文，MUST NOT 覆盖 diff2html 内置 gutter padding。窄屏（≤1024px）断点下 MUST 保持上述不变量。

#### Scenario: 长行横向滚动
- **WHEN** diff 包含超视口宽度的长代码行，用户横向滚动
- **THEN** 行号与代码行保持逐行对齐，无错位

#### Scenario: 窄屏多行滚动
- **WHEN** 视口宽度 ≤1024px 且 diff 跨多个 hunk，用户纵向滚动到中部/底部
- **THEN** 行号与代码行保持逐行对齐，无错位
