# Worktree Branch Prefix Specification

## Purpose

管理 worktree 任务分支名的默认前缀配置（应用级设置，dataDir JSON 存储），供新任务创建时构成分支名，并在全局设置页提供读取与保存入口。

## Requirements

### Requirement: 分支前缀配置存储

系统 SHALL 将 worktree 分支默认前缀作为应用级配置持久化（dataDir 下 `branch-prefix.json`，结构 `{"prefix": "<string>"}`，与 palette 等既有应用配置同构：原子文件写入——临时文件 + rename，写失败 MUST NOT 替换内存值）。未配置时缺省值 MUST 为 `ocdeck`；**配置文件损坏（JSON 非法）时 MUST 降级返回缺省值 `ocdeck` 并产生可观察的加载错误（日志），MUST NOT 启动失败或返回错误响应**。保存时 MUST 校验前缀合法性：**原值**匹配 `^[a-z0-9]([a-z0-9-]{0,48}[a-z0-9])?$`（含首尾空白即非法，不做 trim 容错），非法值 MUST 拒绝保存并返回 invalid_input，原配置（文件与内存值）不变。前缀配置仅包含前缀字符串本身，不含 `/` 分隔符。

#### Scenario: 缺省前缀

- **WHEN** 从未保存过分支前缀配置，读取配置
- **THEN** 返回缺省值 `ocdeck`

#### Scenario: 保存合法前缀

- **WHEN** 用户保存前缀 `team-x`
- **THEN** 配置原子写盘并更新内存值，之后创建的新任务分支名为 `team-x/<slug>`

#### Scenario: 拒绝非法前缀

- **WHEN** 用户保存含非法字符（大写、斜杠、空格、首尾空白）或空的前缀
- **THEN** 保存被拒绝并返回 invalid_input，配置文件与内存值保持原样

#### Scenario: 配置文件损坏降级

- **WHEN** `branch-prefix.json` 内容损坏（JSON 非法），读取配置
- **THEN** 返回缺省值 `ocdeck`，记录可观察的加载错误，服务正常启动与运行

### Requirement: 分支前缀读写 API

系统 SHALL 提供固定端点 `GET /api/v1/config/branch-prefix` 与 `PUT /api/v1/config/branch-prefix`，请求与响应 DTO 均为 `{"prefix": "<string>"}`，成功响应 200 返回当前生效值。PUT MUST 在持久化前完成合法性校验，校验失败返回 invalid_input 且 MUST NOT 写盘。

#### Scenario: 读取当前前缀

- **WHEN** 前端 `GET /api/v1/config/branch-prefix`
- **THEN** 返回 200 + `{"prefix": "<当前生效值>"}`（未配置或配置损坏时为 `ocdeck`）

#### Scenario: 保存后立即可读

- **WHEN** 用户 `PUT` 新前缀成功后再次 `GET`
- **THEN** 返回新保存的前缀值

### Requirement: 前缀仅对新任务生效

分支前缀配置的修改 MUST 仅影响之后创建的任务的分支命名；已创建任务的分支名、worktree 路径与生命周期行为 MUST NOT 受影响，已创建任务的分支改名操作沿用其原分支前缀（见 task-lifecycle spec「任务分支改名」）。

#### Scenario: 修改前缀不影响存量任务

- **WHEN** 全局前缀由 `ocdeck` 修改为 `team`，系统中存在此前创建的 `ocdeck/*` 分支任务
- **THEN** 存量任务的分支名与 worktree 路径不变，仅新创建任务使用 `team/<slug>`

### Requirement: 设置页分支前缀入口

系统 SHALL 在全局设置页（`#/configs`）提供分支前缀配置子标签：`ConfigsTab` 枚举新增固定值 `'branch-prefix'`，深链 `#/configs#branch-prefix` 直达。子标签面板（与 palette 等既有面板同构：GET→ready→编辑→保存、加载/保存错误分离）展示当前前缀、允许编辑保存、非法输入就地展示校验错误。

#### Scenario: 设置页查看与保存前缀

- **WHEN** 用户打开设置页分支前缀子标签，修改前缀并保存
- **THEN** 合法值保存成功并展示当前生效值；非法值展示校验错误且不保存

#### Scenario: 深链直达

- **WHEN** 用户打开 `#/configs#branch-prefix`
- **THEN** 设置页打开并直接选中分支前缀子标签
