# Delta: git-operations

## ADDED Requirements

### Requirement: 纯目录项目任务的 git 操作降级
对 `kind=dir` 项目的任务，任务级 git 操作（status/diff/commit/push）SHALL 统一拒绝并返回明确错误（invalid_input，消息说明该项目为纯目录类型、非 git 仓库），MUST NOT 对任务目录执行任何 git 命令，MUST NOT 尝试探测目录内的子仓库。Web UI SHALL 对 dir 项目的任务隐藏 git 面板入口（status/diff/commit/push），不展示分支名。

#### Scenario: dir 任务请求 git 状态
- **WHEN** 对 `kind=dir` 项目的任务调用 git status/diff/commit/push 任一 API
- **THEN** 系统返回 invalid_input 错误，明确说明纯目录项目不支持 git 操作，且未执行任何 git 命令

#### Scenario: dir 任务的 UI 降级
- **WHEN** 用户在 Web UI 打开 dir 项目的任务
- **THEN** git 面板入口不可见，任务不显示分支名；项目列表/详情显示项目类型标识
