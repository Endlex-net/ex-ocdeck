# Delta Spec: project-management

## MODIFIED Requirements

### Requirement: 项目注册
系统 SHALL 允许用户将本机目录注册为项目。注册时 MUST 显式指定项目类型 `kind ∈ {repo, dir}`（请求字段 `kind`，缺省 `repo`，向后兼容；非法值 MUST 返回 invalid_input / HTTP 422）。

`kind=repo`：系统 MUST 校验该路径存在且为 git 仓库（含 `.git` 或 `git rev-parse` 校验），并记录项目的默认分支。

`kind=dir`（纯目录项目）：系统 MUST NOT 要求该路径为 git 仓库，MUST 仅校验路径存在且为目录；MUST NOT 探测默认分支（`default_branch` 记录为空）。系统 MUST NOT 根据 `IsGitRepo` 失败隐式推断 `kind=dir`——类型只来自用户显式指定。

系统 SHALL 允许同一 canonical path 注册多个项目（`kind=repo` 与 `kind=dir` 均允许）：注册时 MUST NOT 仅因 path 与已有项目重复而拒绝，MUST NOT 返回 409 conflict；当请求通过全部既有校验（字段合法性、路径存在性与类型、kind 校验）且持久化与回读成功时，创建照常成功并返回 201 与项目详情。重复 path MUST NOT 免除任何既有校验：非 git 仓库路径以 `kind=repo` 提交仍 MUST 拒绝，非法 kind 仍 MUST 返回 422。path 的 canonical 归一（绝对化 + symlink 解析）语义 MUST 保持不变，存储的仍为归一后路径。项目唯一标识 MUST 仅为项目 id，path 不再承担唯一性。

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

#### Scenario: 重复 path 注册成功
- **WHEN** 用户提交的路径通过全部既有校验（字段、路径、kind），且 canonical 归一后与任一已注册项目的 path 相同
- **THEN** 系统照常创建项目并返回 201 与项目详情，不返回 409，新项目与已有项目各自独立（不同 id）

#### Scenario: 不同 kind 共用 path
- **WHEN** 某路径本身为合法 git 仓库且已注册为 `kind=repo` 项目，用户以 `kind=dir` 注册同一路径（或反之）
- **THEN** 系统照常创建项目并返回 201，两项目 kind 各自独立

#### Scenario: 重复 path 不免除既有校验
- **WHEN** 路径与某已注册项目重复，但请求本身不满足既有校验（如非 git 仓库路径以 `kind=repo` 提交、非法 kind、路径不存在）
- **THEN** 系统按既有规则拒绝并返回对应错误（422/invalid_input 等），MUST NOT 创建项目记录

## ADDED Requirements

### Requirement: 注册表单重复路径提醒
注册项目表单 SHALL 在用户输入/修改路径时实时检查该路径是否与任一已注册项目的 path 重复，重复时 MUST 即时展示提醒（告知该路径已注册过项目）；路径变为不重复时提醒 MUST 消失。该提醒 MUST NOT 阻塞表单提交——用户在提醒展示中仍可完成创建。

查重数据源 MUST 为前端已加载的项目列表，MUST NOT 为此新增后端查重接口。匹配规则：候选值为输入路径经 `trim()` 去除前后空白后的字符串；候选值为空时不匹配；否则对已加载列表逐项目执行 `p.path === candidate`（字符串全等），不按 kind 过滤；除 trim 外 MUST NOT 引入尾斜杠、相对路径、大小写或 symlink 等额外归一化逻辑，系统接受因此导致的漏检（如 `/tmp` 与 `/private/tmp` 指向同一目录）。

#### Scenario: 输入重复路径即时提醒
- **WHEN** 用户在注册表单输入的路径与已加载项目列表中某项目的 path 相同
- **THEN** 表单即时展示重复提醒，告知该路径已注册过项目，提交按钮保持可用

#### Scenario: 路径修正后提醒消失
- **WHEN** 提醒展示中，用户将路径修改为不与任何已注册项目重复的值
- **THEN** 提醒即时消失

#### Scenario: 提醒不阻塞创建
- **WHEN** 重复提醒展示中，用户提交注册表单
- **THEN** 系统照常创建项目（对应「项目注册」的重复 path 注册成功语义）
