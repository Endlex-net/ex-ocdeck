# Proposal: opencode v2 实机契约 spike

## Why

ocdeck 已锁定「v1/v2 双 provider 长期并存」的版本适配方向（任务创建时钉死 providerID，存量任务全部视为 v1，分派按机器实装版本，v2 provider 承接 v1 任务），但 v2 迁移设计（`docs/design/opencode-v2-a-plus-migration.md`）基于 opencode 2.0.23 草案，目标版本与协议契约均未经过实机验收；v1 characterization 全绿不能证明 v2 行为兼容。在投入 v2 provider 实现之前，需要一次实机契约 spike 锁定目标版本、盘点 v1↔v2 协议差异、并设计 v1 binding 被 v2 provider 承接的语义，把后续实现 change 建立在已验证的契约事实上。同时，opencode 升级会将数据从 v1 单向自动迁移到 v2（v2 环境可打开迁移前的数据，v1 环境不承诺打开 v2 数据），该单向迁移契约是整个版本适配模型的基础假设，必须在 spike 中实测验证并记录。

## What Changes

- 在用户提供、与开发机隔离的验证环境中安装并锁定一个具体 opencode v2 目标版本（独立 XDG 目录，不触碰开发机现有 v1 数据与配置）；spike 全程在该验证环境执行，不在开发机本机执行。
- 产出可重跑的契约探测脚本集：围绕 SPI 能力面逐项探测 v2 行为——启动参数与认证、健康检查、session/location/environment、事件流与权限模型、prompt 发送与结果、终端 attach，以及现有 diffreview 依赖的操作。
- 产出 v1↔v2 协议差异清单：逐项标注 兼容 / 不兼容（含替代形态）/ 待验证，作为 v2 provider 实现 change 的契约事实来源。
- 实测验证单向数据迁移契约：v1 数据可自动迁移至 v2、v2 环境可打开迁移前数据、v1 环境不承诺打开 v2 数据；迁移实现本身属于后续 change，不在本 change 内完成。
- 验证并记录 v1 binding 在 v2 机器上的 provider 承接契约，以及 v2 任务在 v1 机器上的跨版本兼容边界；具体命名空间、归属、判定规则与失败语义写入 design.md。
- 产出 go/no-go 建议与 v2 provider 实现 change 的范围输入。
- 非目标（本 change 不做）：v2 provider 实现；C19 生产 fallback 关闭、F3 TaskRef.Mode 填充等代码修复；事件路由与共享 server 生命周期改造；codex / pi 接入；开发机升级 v2；任何线上运行时行为变更。

## Capabilities

### New Capabilities

（无——本 change 为契约 spike，不引入运行时行为。）

### Modified Capabilities

（无——本 change 不修改任何 spec-level 行为。spike 结论将作为后续 v2 provider 实现 change 的输入，由该 change 携带 spec deltas。本 change 在 `.openspec.yaml` 设置 `skip_specs: true`。）

## Impact

- **代码**：新增 `scripts/spike-v2/` 下可重跑的 v2 契约探测脚本（工具性质，不改变运行时路径）；无既有代码改动。
- **环境**：spike 全程在用户提供、与开发机隔离的验证环境执行（其内以独立 XDG 目录安装 opencode v2 目标版本）；不触碰开发机现有 v1 数据与配置，不升级开发机。
- **文档**：spike 结论沉淀于本 change 的 design.md（协议差异清单、单向迁移契约验证结论、承接契约与兼容边界），并回写 `docs/design/opencode-v2-a-plus-migration.md` 的版本锁定记录与结论引用。
- **API / 对外行为**：无变更。
- **后续**：v2 provider 实现 change 的 proposal 将引用本 spike 的协议差异清单、单向迁移契约验证结论与承接契约结论。
