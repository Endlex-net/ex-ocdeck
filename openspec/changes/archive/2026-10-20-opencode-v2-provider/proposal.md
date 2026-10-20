# Proposal: opencode-v2-provider

## Why

opencode 官方已发布 v2（锁定基线 2.0.28），其 wire 契约与 v1 不兼容（API `/api` 前缀、`{data}` envelope、prompt/事件/permission 形态全部变化）。spike（`opencode-v2-contract-spike`）已实证全部核心差异并完成官方升级路径验证。ocdeck 需要新增 v2 provider 兑现既有 SPI 契约，与 v1 provider 长期并存：任务创建时钉死 providerID，存量 v1 任务由 v2 机器上的 v2 provider 承接，反向 fail-closed。

## What Changes

- **新增 v2 provider 实现**（与 `internal/infrastructure/opencode` 同级的 v2 包）：兑现 `agent-provider-spi` 的 Provider 聚合接口与 8 窄接口；wire 差异适配以 spike 差异清单为唯一契约事实起点（机制见 design D1/D7）
- **registry 双 provider 注册**：`opencode-v1` 与 `opencode-v2` 并存注册（`cmd/ocdeck-server/main.go` 装配点），v1 行为不变
- **分派语义扩展**：v2 机器承接 v1 钉扎任务（实际执行 provider 按规则选择，`tasks.provider_id` 钉扎值不变）；跨版本越界激活 fail-closed（规则见 design D4）
- **v2 runtime 托管模型**：按 A+ 方案（`docs/design/opencode-v2-a-plus-migration.md`）——全局单 `opencode serve` 进程承载多 session（语义见 design D3）
- **v2 契约治理**：新增 v2 契约锚点集与独立检查脚本；v1 契约锚点集保留不动（见 design D6）
- **承接前置补证**：spike design.md「Spike 结论」阻断项清单转化为本 change 的补证与冻结任务（见 design D8）
- 迁移数据割接采信官方升级路径（已实证 PASS），不做自研数据搬运

## Capabilities

### New Capabilities

- `opencode-v2-provider`：v2 provider 的 SPI 兑现与 wire 适配——session 生命周期、事件订阅与归一化、attention（permission/question）、prompt 提交、状态读取、终端、标题同步、runtime 托管（A+ 单 server 多 session）、v2 契约锚点治理

### Modified Capabilities

- `agent-provider-spi`：registry 分派语义扩展——双 provider 并存注册、v2 机器承接 v1 钉扎任务的执行 provider 选择规则、跨版本激活的 fail-closed 边界；新增 binding 归属校验端口（v2 强制，先于凭据派生）

## Impact

- **新增代码**：v2 provider 包（client/supervisor/events/适配器）、v2 契约锚点检查脚本、v2 testdata fixture
- **修改代码**：`cmd/ocdeck-server/main.go`（registry 双注册装配）、`internal/task/binding_dispatch.go` 与激活/恢复路径（承接分派与 fail-closed 判定）、`internal/config/config.go`（v2 版本探测扩展）
- **不变**：`internal/infrastructure/opencode/` v1 实现、v1 契约锚点、任务钉扎值（`tasks.provider_id`）、纯 v1 路径行为；承接时 ocdeck 侧 binding/归属/anchor 元数据原子转换（design D5）属本期明确变更范围
- **依赖**：spike 结论（`openspec/changes/opencode-v2-contract-spike/design.md`「Spike 结论」节）为唯一契约事实起点；待验证项按其标注在实现中补证
- **上游依赖**：opencode v2.0.28（update-API 渠道核验）；模型依赖项补证需要带模型凭据的集成环境
