# Proposal: v1-auto-adopt-on-v2

## Why

opencode 升级到 v2 后，官方升级路径自动迁移存量 session 到 v2 存储（ID 保留、内容完整，已在 Docker 中实证：v1.18.26 → v2.0.28 原地替换二进制后同一 sessionID 可读、消息完整）。但 ocdeck 的承接开关 `OCDECK_V2_ADOPTION` 默认关，导致机器升级 v2 后所有 v1 钉扎任务命中 resolver R9 fail-closed（"需开启承接开关"）——用户视角即「老任务全部打不开」，而 session 数据实际已在 v2 中可用。这是默认值的 rollout 保守决策与现实升级路径脱节，而非机制缺失（R8 承接路径已存在且完整）。

## What Changes

- **承接开关默认值翻转为开**：未配置 `OCDECK_V2_ADOPTION` 时默认开启承接（v2 provider 在装配期注册）；显式 `=0`/`false`/`off` 仍可关闭（关闭形态不变：v2 provider 不注册，R9 fail-closed 语义保留给显式关闭场景）
- **R9 用户提示文案更新**：开关默认开后，R9 仅在用户显式关闭时触发，文案相应调整（说明是显式关闭状态及如何恢复）
- **自动承接通知**：v1 钉扎任务在 v2 机器上激活/恢复时经既有 R8 路径自动承接，承接成功的如实告知通知（pending attention 丢失说明，既有 `adoptionPendingLossNotice` last_error 渠道）保留并微调措辞，明确这是 v2 升级迁移承接

## Capabilities

### Modified Capabilities

- `agent-provider-spi`：承接开关默认值语义（默认开、显式关闭可逆）与自动承接的用户可感知通知要求

## Impact

- **修改代码**：`internal/config/config.go`（`v2AdoptionEnabled` 默认值与注释）、`internal/task/resolver.go`（R9 DenyReason 文案）
- **修改测试**：config 开关解析测试、resolver R9/R10 相关断言
- **行为变化**：v2 机器上 v1 钉扎任务激活/恢复从 fail-closed 变为自动承接（单向，binding 翻转 v2、钉扎值不变）；v1 机器行为不变（R10 纯 v1）
- **不受影响**：R1–R8、R10–R12 决策语义；承接事务与回滚矩阵（D5）；v2 supervisor/契约锚点
- **验证**：Docker 模拟容器（ocdeck-upsim，已具备 v1 会话 + v2 二进制状态）做端到端实证
