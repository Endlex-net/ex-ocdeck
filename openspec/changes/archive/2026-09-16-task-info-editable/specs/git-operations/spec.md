## ADDED Requirements

### Requirement: 本地分支改名

系统 SHALL 支持将 worktree 当前检出的本地分支原子改名（`git branch -m` 语义）：改名后 worktree HEAD 自动跟随新分支名，分支 reflog 与分支级配置保留，worktree 磁盘路径与工作区内容不变。改名 MUST 在该 repo 写锁内执行（与 worktree add/remove、远端 refresh 等写操作串行）；repo 写锁由编排层以**项目公共仓库路径**获取（锁键按传入路径归一，不会自动把 worktree 路径折算为公共仓库），git 原语本身 MUST NOT 重复加锁。改名前 MUST 在写锁内完成 HEAD 身份验证：目标 worktree 的 symbolic HEAD 指向待改分支；HEAD 不匹配、detached HEAD 或 worktree 路径缺失 MUST 拒绝且零副作用。改名前 MUST 完成无副作用校验：新分支名通过 `git check-ref-format --branch` 规范校验，且目标本地分支不存在（写锁内复查，防锁外竞态）；校验失败 MUST 返回错误且零副作用。MUST NOT 对远端分支执行任何写操作。改名操作失败时 MUST 保持原分支名不变（`git branch -m` 单命令原子性保证无中间态；跨 git 与任务记录的恢复语义见 task-lifecycle spec「任务分支改名」）。

#### Scenario: worktree 检出分支改名成功

- **WHEN** 对 worktree 中检出的本地分支 `ocdeck/old` 执行改名为 `ocdeck/new`
- **THEN** 分支引用原子更名，worktree HEAD 指向 `ocdeck/new` 且提交不变，worktree 路径不变，reflog 保留

#### Scenario: 新分支名非法

- **WHEN** 改名目标分支名未通过 check-ref-format 校验
- **THEN** 返回错误，不执行任何 git 写操作，原分支名不变

#### Scenario: 新分支名已存在

- **WHEN** 改名目标分支名与既有本地分支冲突
- **THEN** 返回错误，不执行任何 git 写操作，原分支名不变

#### Scenario: 改名与写操作串行

- **WHEN** 改名请求与同 repo 的 worktree add/remove 或远端 refresh 并发到达
- **THEN** 改名在该 repo 写锁内与其他写操作串行执行，互不交错

#### Scenario: HEAD 身份不匹配拒绝

- **WHEN** 目标 worktree 的 symbolic HEAD 不指向待改分支（detached HEAD、HEAD 指向其他分支、或 worktree 路径缺失）
- **THEN** 系统拒绝改名（invalid_state），不执行任何 git 写操作，原分支名不变
