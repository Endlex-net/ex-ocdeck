# Proposal: v1-legacy-anchor-adopt

## Why

承接开关默认开启后（v1-auto-adopt-on-v2 已合并），真实环境暴露：binding 持久化机制（迁移 0018）之前创建的 v1 老任务，其形态为「`task_binding` 为空 + anchor 三元组完整（v1）+ anchor ∈ v1 归属集合」。承接判定矩阵（design D5）的 A 分支要求既有 v1 binding、B 分支要求三者全空，该合法老形态落入「其余 → stop」，报「承接准入判定不一致」——老任务依然打不开。线上实证（只读核查）：三台任务均为此形态，数据完整一致、非损坏。

## What Changes

- **承接判定矩阵新增 legacy-A 变体**：钉扎 v1、`task_binding` 为空、anchor 三元组为 v1 命名空间且 anchor ∈ v1 归属集合 → 判 A 分支，承接目标 = `anchor_session_id`；anchor 不在归属集合或命名空间非 v1 时维持 stop
- **承接流程**：无 binding 时合成内存态 v1 binding（仅 `{ProviderID: opencode-v1, ProviderInstanceID: default, UpstreamSessionID: anchorID}`）供 Resume 使用（v2 supervisor 对非 v2 binding 只做 upstream 非空校验 + 自有 serve 查会话，已有语义兼容）
- **承接事务**：`CommitAdoption` 扩展接受「旧 binding 为空」形态——CAS 代际守卫相应为 `task_binding IS NULL`（并发写入 binding 后提交拒绝，守卫语义等价）；既有 v1 binding 路径行为不变
- 承接成功通知沿用既有 `adoptionPendingLossNotice`（A 分支语义不变）

## Capabilities

### Modified Capabilities

- `agent-provider-spi`：承接判定矩阵覆盖无 binding 老任务形态（legacy-A）

## Impact

- **修改代码**：`internal/task/v2_adopt.go`（classifyAdoption + adoptTaskV2 合成 binding）、`internal/infrastructure/store/adoption_tx.go`（CommitAdoption 旧 binding 空形态 CAS）
- **修改测试**：判定矩阵单测、承接事务测试（NULL binding CAS + 并发守卫）、stage4 承接流程测试
- **不受影响**：既有 v1 binding 的 A 分支、B 分支、stop 分支语义；Resume/握手/提交/退役其余步骤
- **验证**：Docker 模拟容器复现该形态（v1 任务置空 task_binding → 升级 → 激活承接成功）
