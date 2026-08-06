# Proposal: add-plain-dir-project

## Why

跨多个项目操作的场景中，用户希望把工作目录放在这些项目的上级目录（该目录本身不是 git 仓库）。当前 ocdeck 强制项目路径必须是 git 仓库（注册时 `IsGitRepo` 校验），任务模型绑定 "1 任务 = 1 仓库 worktree + 1 分支"，导致这类纯目录无法注册为项目、无法创建任务。

## What Changes

- 项目注册支持显式类型 `kind: repo | dir`：dir 项目跳过 git 仓库校验与默认分支探测，允许注册任意存在的目录。**不允许** 靠 `IsGitRepo` 失败隐式推断为 dir，避免路径手滑静默创建语义不同的项目。
- dir 项目下创建任务：不创建 worktree、不创建分支、不做分支命名（LLM slug 与机械 slugify 全部跳过）；任务的运行目录直接锚定为项目路径本身。
- dir 任务删除语义硬不变量：ocdeck 内建删除逻辑 MUST NOT 对用户目录及其内容执行任何写/删操作；仅清理 ocdeck 自身数据（DB 记录、进程组、任务日志目录）与 opencode session 数据。**例外**：项目配置的 pre-delete script 为用户显式授权的操作，仍在项目目录执行（与 repo 语义一致），不计入上述不变量。
- session 归属隔离：任何任务 MUST 只操作自己拥有的 opencode session——全量对齐 MUST NOT 认领已被其他任务拥有的 session；共享目录的 dir 任务 MUST NOT 经目录级全量对齐认领新 session（仅经本任务 serve 的 SSE 捕获与锚定创建记录）；删除任务 MUST 仅删除本任务拥有的 session。
- 同一 dir 项目允许并行多个活跃任务（用户显式接受**文件层面**无隔离；UI 提示风险，不加互斥锁；session 层面按上条隔离）。
- dir 任务跳过 inherit 文件继承（无语义来源）；init script 与 pre-delete script 保留可用。
- git API（status/diff/commit/push）对 dir 任务返回明确的"非 git 项目"错误；Web UI 对 dir 项目/任务隐藏 git 相关入口并标识项目类型。
- repo 项目创建任务支持选择基线分支：任务创建 API 增加可选 `base_ref` 参数（本地或远端分支），缺省仍从项目默认分支切出（向后兼容）；提供项目分支列表查询（本地+远端）供 UI 下拉选择。dir 项目任务不接受 `base_ref`（提供即 invalid_input）。

## Capabilities

### New Capabilities

（无）

### Modified Capabilities

- `project-management`: 项目注册从"必须是 git 仓库"扩展为显式 `repo | dir` 两种类型；dir 类型跳过 git 校验与默认分支探测；worktree 存放位置约定仅适用于 repo 项目。
- `task-lifecycle`: 任务创建对 dir 项目分叉——无 worktree/无分支/无分支命名，运行目录锚定项目路径；任务删除对 dir 项目分叉——内建逻辑零目录副作用（pre-delete 用户脚本除外），仅清理 ocdeck 与 opencode session 数据；同 dir 项目允许多活跃任务并行；repo 任务创建支持可选基线分支 `base_ref`（本地/远端分支）。
- `git-operations`: 任务级 git 操作对 dir 项目的任务统一返回明确降级错误。
- `opencode-orchestration`: session 归属捕获增加所有权语义——对齐不认领他任务 session、共享目录 dir 任务不经目录对齐认领新 session、删除仅作用于本任务 session。
- `project-lifecycle-config`: Inherit 文件继承限定为 repo 项目任务；明确 dir 项目 init/pre-delete 脚本以项目目录为 cwd 且属用户授权副作用。

## Impact

- **代码**：`internal/api/projects.go`（注册校验分叉）、`internal/store`（projects 表增 `kind` 列及迁移）、`internal/task`（create/delete/suspend/reconcile 按 kind 分叉，gitops 降级）、`internal/api/git.go`（dir 任务拒绝）、`web/`（项目类型标识、git tab 隐藏、dir 项目并行风险提示）。
- **API**：项目注册请求增加 `kind` 字段（默认 `repo`，向后兼容）；项目/任务 DTO 暴露类型；任务创建请求增加可选 `base_ref`；新增只读 `GET /api/v1/projects/{id}/branches`。
- **DB**：projects 表新增 `kind` 列（存量行回填 `repo`）；tasks 表新增 `base_ref` 列（存量 repo 任务回填 `refs/heads/<default_branch>`）；dir 项目任务的 `branch` 为空、`worktree_path` 等于项目路径。
- **明确排除**（非目标）：子仓库自动探测/聚合 git 视图、多仓库任务（1:N）、同目录并发任务互斥锁、dir 任务的任何 git 能力。
