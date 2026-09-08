# Delta: git-operations（add-local-path-task-mode）

## ADDED Requirements

### Requirement: repo 项目 local-path 模式任务的 git 能力

repo 项目 local-path 模式任务 SHALL 开放任务级 git 管理：status / diff / diff review / commit / push / diff 视图内文件编辑读写，操作对象为项目目录（`worktree_path` 即项目路径）当前 checkout 的**当前分支**。`assertGitRepoTask` 门禁按 D2 有效模式放行（repo + local-path 通过），非法 kind/mode 组合仍 internal fail-closed。diff 来源钉死为现有 GitPanel 三组（仅未提交部分，对比 HEAD/index）：`ref=HEAD`（已暂存，工作区 vs HEAD）、`ref=''`（未暂存，工作区 vs index）、`untracked`（未跟踪文件）。MUST NOT 展示当前分支相对 upstream 或 base 的已提交 commit 差异（分支变更视图留待后续迭代）。commit SHALL 提交到当前分支 HEAD；push SHALL 为 `git push -u origin <当前分支>` 且 MUST NOT force-push。diff 视图内文件编辑读写 SHALL 经 /git/file 接线与 diffreview_fileedit.go 同一门禁放行：读（ReadRaw）落点为项目目录，编辑写回落点为项目目录当前 checkout。风险归属（显式契约）：操作对象是用户主仓库当前分支，改动就地生效、提交/推送/文件编辑写回直接作用于用户分支与工作区；dirty 混杂与直接修改/提交/推送主仓库的风险由用户自担（与 local 模式共享目录语义一致）。

#### Scenario: local-path 模式任务 git 操作放行且作用于项目目录

- **WHEN** 对 repo 项目 local-path 模式任务调用 git status/diff/commit/push 任一 API
- **THEN** 系统放行并在项目目录当前 checkout 上执行（`worktree_path` 即项目路径），无 base_ref 依赖；commit 落到当前分支 HEAD，push 执行 `git push -u origin <当前分支>` 且 MUST NOT force-push

#### Scenario: local-path 模式任务 diff 来源为 GitPanel 三组（仅未提交部分）

- **WHEN** repo 项目 local-path 模式任务在 GitPanel 查看 diff
- **THEN** diff 来源为三组真值：`ref=HEAD`（已暂存，工作区 vs HEAD）、`ref=''`（未暂存，工作区 vs index）、`untracked`（未跟踪文件），对比 HEAD/index；MUST NOT 展示当前分支相对 upstream 或 base 的已提交 commit 差异（分支变更视图留待后续迭代）

#### Scenario: local-path 模式任务 diff review 放行

- **WHEN** repo 项目 local-path 模式任务发起 diff review（DiffSourcePortAdapter.ReadLocked 同一门禁，来源 `(ref, path, untracked)` 由 UI 传入）
- **THEN** 门禁放行，review 基于项目目录当前 checkout 的指定来源执行

#### Scenario: local-path 模式任务 diff 视图内文件编辑读写放行

- **WHEN** repo 项目 local-path 模式任务在 GitPanel diff 视图内读取（ReadRaw）或编辑写回文件（/git/file 接线，经 diffreview_fileedit.go 同一门禁）
- **THEN** 读取与写回落点均为项目目录当前 checkout（`worktree_path` 即项目路径），改动就地生效；风险由用户自担

#### Scenario: detached HEAD 边界

- **WHEN** 项目目录当前分支被外部切为 detached HEAD 后，用户查看 status 或执行 push
- **THEN** status 的分支显示留空、不阻断（gitops.go:74 现状语义）；push 失败并透传 git 错误（worktree.go:236 现状语义），MUST NOT 伪装成功

#### Scenario: local-path 模式任务的 UI 展示

- **WHEN** 用户在 Web UI 打开 repo 项目 local-path 模式任务
- **THEN** Git tab 与 git 面板入口可见（isGitless 仅对 dir 项目成立）；工作台页头不展示任务分支名（`task.branch` 恒为空），GitPanel 内展示实时分支（status.branch）

#### Scenario: 任务行分支显示维持隐藏

- **WHEN** 用户在指挥中心或项目页查看 repo 项目 local-path 模式任务行
- **THEN** 任务行不展示分支名（维持 gitless 隐藏不变）

## MODIFIED Requirements

### Requirement: 纯目录项目任务的 git 操作降级

对 `kind=dir` 项目的任务，任务级 git 操作（status/diff/commit/push）SHALL 统一拒绝并返回明确错误（invalid_input，消息说明该项目为纯目录类型、非 git 仓库），MUST NOT 对任务目录执行任何 git 命令，MUST NOT 尝试探测目录内的子仓库。Web UI SHALL 对 dir 项目的任务隐藏 git 面板入口（status/diff/commit/push），不展示分支名。repo 项目 local-path 模式任务不适用本降级（见「repo 项目 local-path 模式任务的 git 能力」）。

#### Scenario: dir 任务请求 git 状态

- **WHEN** 对 `kind=dir` 项目的任务调用 git status/diff/commit/push 任一 API
- **THEN** 系统返回 invalid_input 错误，明确说明纯目录项目不支持 git 操作，且未执行任何 git 命令

#### Scenario: dir 任务的 UI 降级

- **WHEN** 用户在 Web UI 打开 dir 项目的任务
- **THEN** git 面板入口不可见，任务不显示分支名；项目列表/详情显示项目类型标识
