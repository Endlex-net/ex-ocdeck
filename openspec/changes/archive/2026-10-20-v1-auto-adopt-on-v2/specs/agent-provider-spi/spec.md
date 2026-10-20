# agent-provider-spi Spec (Delta)

## ADDED Requirements

### Requirement: 承接开关默认值与自动承接通知

承接开关 `OCDECK_V2_ADOPTION` 未配置时 SHALL 默认为开启（v2 provider 在装配期注册）；显式配置为 `0`/`false`/`off`（大小写不敏感、容忍首尾空白）时关闭，其他未知显式值 MUST 保守视为关闭，关闭形态 MUST 仍为 v2 provider 不注册。钉扎 `opencode-v1`、无 v2 binding 的任务在实装 opencode 主版本 ≥ 2 的机器上激活或恢复时，承接 MUST 自动发生（resolver R8，无需用户逐任务确认）；承接成功时 MUST 经 last_error 渠道写入用户可感知的迁移承接通知，如实说明 v1 pending attention 不保留（MUST NOT 把空 pending 解释为「原 pending 已解决」）。

#### Scenario: 未配置开关时默认承接

- **WHEN** 机器实装 opencode 主版本 ≥ 2、未配置 `OCDECK_V2_ADOPTION`，钉扎 `opencode-v1` 且无 v2 binding 的任务请求激活或恢复
- **THEN** resolver 命中 R8（exec=opencode-v2、Adopt=true），承接流程自动执行，不因开关未配置而 fail-closed

#### Scenario: 显式关闭保留 fail-closed

- **WHEN** `OCDECK_V2_ADOPTION` 显式配置为 `0`/`false`/`off`，钉扎 `opencode-v1` 的任务在 v2 机器上请求激活或恢复
- **THEN** resolver 命中 R9 fail-closed（capability_unavailable），保留任务现状，提示承接开关处于关闭状态及恢复方式

#### Scenario: 未知显式值保守按关闭处理

- **WHEN** `OCDECK_V2_ADOPTION` 配置为 `0`/`false`/`off` 之外的未知值（如 `yes`、空串、`2`），钉扎 `opencode-v1` 的任务在 v2 机器上请求激活或恢复
- **THEN** 按关闭处理（v2 provider 不注册），resolver 命中 R9 fail-closed 语义适用

#### Scenario: 自动承接成功通知

- **WHEN** v1 钉扎任务经 R8 自动承接且历史判定矩阵命中 A 分支（已有历史 session）提交成功
- **THEN** 任务 last_error 写入迁移承接通知，说明该任务已随 opencode v2 升级自动承接、原 v1 pending attention 为进程内存态不保留；B 分支（从未建立上游会话的首建）不产生迁移通知——无历史可丢失
