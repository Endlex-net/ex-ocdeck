# Project Management Specification

## Purpose
将本机 git 仓库注册为项目并提供列表、详情与删除管理，约定任务 worktree 统一存放于 ocdeck 数据目录下，避免污染源仓库。

## Requirements

### Requirement: 项目注册
系统 SHALL 允许用户将本机 git 仓库目录注册为项目。注册时系统 MUST 校验该路径存在且为 git 仓库（含 `.git` 或 `git rev-parse` 校验），并记录项目的默认分支。

#### Scenario: 注册合法仓库
- **WHEN** 用户提交一个存在的 git 仓库绝对路径与项目名称
- **THEN** 系统创建项目记录并返回项目详情

#### Scenario: 拒绝非仓库路径
- **WHEN** 用户提交的路径不存在或不是 git 仓库
- **THEN** 系统拒绝注册并返回明确错误原因

### Requirement: 项目列表与详情
系统 SHALL 提供项目列表与单项目详情查询，详情包含该项目下的任务数量与任务状态分布。

#### Scenario: 查看项目列表
- **WHEN** 用户请求项目列表
- **THEN** 系统返回全部已注册项目及其任务概况

### Requirement: 项目删除
系统 SHALL 允许删除项目。当项目下仍存在未删除的任务时，系统 MUST 拒绝删除并提示先处理任务。

#### Scenario: 删除含任务的项目
- **WHEN** 用户删除一个仍有任务的项目
- **THEN** 系统拒绝并提示存在活跃/挂起/归档任务

#### Scenario: 删除空项目
- **WHEN** 用户删除一个无任何任务的项目
- **THEN** 系统删除项目记录（不触碰磁盘上的仓库）

### Requirement: worktree 存放位置约定
系统 SHALL 将任务 worktree 统一创建在 ocdeck 自有数据目录下：`<dataDir>/worktrees/<projectID>/<taskID>/`，MUST NOT 创建在源仓库内部。路径 MUST 以 canonical path + `filepath.Rel` 语义校验包含性。

#### Scenario: 创建任务时的 worktree 路径
- **WHEN** 项目下创建任务
- **THEN** worktree 落于 `<dataDir>/worktrees/<projectID>/<taskID>/`，源仓库目录不被污染