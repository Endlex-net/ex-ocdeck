# Delta: project-management

## MODIFIED Requirements

### Requirement: 项目注册
系统 SHALL 允许用户将本机目录注册为项目。注册时 MUST 显式指定项目类型 `kind ∈ {repo, dir}`（请求字段 `kind`，缺省 `repo`，向后兼容；非法值 MUST 返回 invalid_input / HTTP 422）。

`kind=repo`：系统 MUST 校验该路径存在且为 git 仓库（含 `.git` 或 `git rev-parse` 校验），并记录项目的默认分支。

`kind=dir`（纯目录项目）：系统 MUST NOT 要求该路径为 git 仓库，MUST 仅校验路径存在且为目录；MUST NOT 探测默认分支（`default_branch` 记录为空）。系统 MUST NOT 根据 `IsGitRepo` 失败隐式推断 `kind=dir`——类型只来自用户显式指定。

项目 DTO SHALL 暴露项目 `kind`；任务 DTO SHALL 暴露 `project_kind`（`repo|dir`），供 UI 标识与降级。

#### Scenario: 注册合法仓库
- **WHEN** 用户以 `kind=repo`（或缺省）提交一个存在的 git 仓库绝对路径与项目名称
- **THEN** 系统创建项目记录并返回项目详情（含默认分支与 `kind=repo`）

#### Scenario: 拒绝非仓库路径
- **WHEN** 用户以 `kind=repo` 提交的路径不存在或不是 git 仓库
- **THEN** 系统拒绝注册并返回明确错误原因

#### Scenario: 注册纯目录项目
- **WHEN** 用户以 `kind=dir` 提交一个存在的目录绝对路径（无论其是否为 git 仓库）
- **THEN** 系统创建项目记录，`kind=dir`、`default_branch` 为空，不执行任何 git 校验

#### Scenario: dir 拒绝不存在或非目录路径
- **WHEN** 用户以 `kind=dir` 提交的路径不存在或不是目录
- **THEN** 系统拒绝注册并返回明确错误原因

#### Scenario: 拒绝隐式推断
- **WHEN** 用户以 `kind=repo` 提交一个非 git 仓库路径
- **THEN** 系统拒绝注册（而非自动降级为 dir 项目），错误信息提示可显式选择纯目录类型

### Requirement: worktree 存放位置约定
系统 SHALL 将 `kind=repo` 项目的任务 worktree 统一创建在 ocdeck 自有数据目录下：`<dataDir>/worktrees/<projectName-slug>/<branchPathSlug>-<rand4>/`（路径格式详见 task-lifecycle spec 任务创建要求；存量旧格式目录行为不变），MUST NOT 创建在源仓库内部。路径 MUST 以 canonical path + `filepath.Rel` 语义校验包含性。

`kind=dir` 项目的任务 MUST NOT 创建 worktree：任务运行目录直接锚定为项目路径本身（`worktree_path` 记录为项目路径）。ocdeck 的任务创建内建步骤在 init/激活开始前 MUST NOT 在项目目录内创建任何文件或子目录；用户授权的 init script、pre-delete script 与 agent 会话行为除外。

#### Scenario: 创建任务时的 worktree 路径
- **WHEN** repo 项目下创建任务
- **THEN** worktree 落于 `<dataDir>/worktrees/` 下按任务创建要求生成的路径，源仓库目录不被污染

#### Scenario: dir 项目任务不创建 worktree
- **WHEN** dir 项目下创建任务
- **THEN** 任务记录的 `worktree_path` 等于项目路径；init/激活开始前磁盘上无新增目录或文件（用户授权的 init script 与 agent 会话行为除外）
