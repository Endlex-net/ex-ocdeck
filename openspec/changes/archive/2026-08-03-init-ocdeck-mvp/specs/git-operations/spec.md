# Spec: git-operations

## ADDED Requirements

### Requirement: 工作区状态查询
系统 SHALL 提供任务 worktree 的 git 状态查询（基于 `git status --porcelain=v2 -z -uall`），展示已暂存/未暂存/未跟踪文件及增删行数统计（`git diff --numstat [--cached]`）。变更文件数 MUST 有上限（默认 10000），超限返回明确错误而非截断。

#### Scenario: 查看任务改动
- **WHEN** 用户在任务的 git 面板请求状态
- **THEN** 系统返回文件级状态列表与增删行数

### Requirement: 文件 diff 查看
系统 SHALL 提供 unified diff 文本（`git diff [ref] -- <path>` 输出），供前端 diff2html 渲染。服务端 MUST 限制单次 diff 字节数与文件数；二进制或超限文件 MUST 返回截断标记而非内容。

#### Scenario: 查看单文件 diff
- **WHEN** 用户选择某个改动文件
- **THEN** 系统返回该文件的 unified diff 文本，前端渲染对比视图

#### Scenario: 超大文件
- **WHEN** 文件超过服务端限制或为二进制
- **THEN** 前端显示截断/不支持标记，不冻结浏览器

### Requirement: 提交改动
系统 SHALL 支持在任务 worktree 上执行 commit：用户选择要提交的文件（或全部）并输入 commit message。系统 MUST 使用本机 git CLI 执行，保持与用户环境一致（含 hooks、签名）。

#### Scenario: 提交全部改动
- **WHEN** 用户输入 commit message 并提交
- **THEN** 系统在 worktree 中执行 stage + commit 并返回结果

#### Scenario: commit hook 失败
- **WHEN** commit 被 git hook 拒绝
- **THEN** 系统将 git 的错误输出原样展示给用户

### Requirement: 推送分支
系统 SHALL 支持将任务分支 push 到远端（含首次 push 时设置 upstream）。系统 MUST NOT 自动 force-push。

#### Scenario: 首次推送
- **WHEN** 用户对未推送过的任务分支执行 push
- **THEN** 系统执行 `git push -u origin <branch>` 并返回结果

#### Scenario: 推送被拒绝
- **WHEN** push 被远端拒绝（non-fast-forward 等）
- **THEN** 系统展示 git 错误，不自动采取任何补救动作

### Requirement: git 操作串行化
对同一项目仓库的 worktree 增删等仓库级写操作 SHALL 经每 repo 锁串行执行，避免并发 git 锁冲突；status/diff 等只读操作 MUST NOT 进入写队列。

#### Scenario: 并发创建任务
- **WHEN** 用户同时创建多个任务
- **THEN** worktree 创建操作排队串行执行，全部成功

### Requirement: git 执行安全约束
系统 SHALL 以固定命令白名单 + argv 数组调用 git CLI，MUST NOT 拼接 shell 字符串。分支名 MUST 经 `git check-ref-format` 校验。全部 git 命令 MUST 支持 context 取消，stdout/stderr 有界读取。

#### Scenario: 非法分支名
- **WHEN** 任务名称生成的分支名不合法
- **THEN** 创建被拒绝并提示修正任务名称
