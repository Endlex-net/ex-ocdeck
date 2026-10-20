# Tasks

## 1. 判定矩阵扩展

- [ ] 1.1 `internal/task/v2_adopt.go` classifyAdoption：新增 legacy-A 变体——`!hasBinding && anchorID != "" && anchor 命名空间 v1/default && anchor ∈ v1 归属集合` → branch A（target=anchorID）；其余守卫不变（v2/未知命名空间归属行、v2 binding、部分缺失仍 stop）
- [ ] 1.2 判定矩阵单测：legacy-A 命中 + 三个拒绝变体（anchor 不在归属、anchor 空、归属含 v2 行）

## 2. 承接流程与事务

- [ ] 2.1 adoptTaskV2 分支 A：hasBinding=false 时合成内存态 v1 binding（ProviderID=opencode-v1、ProviderInstanceID=default、UpstreamSessionID=anchorID）供 Resume 与提交使用，不持久化
- [ ] 2.2 `internal/infrastructure/store/adoption_tx.go`：CommitAdoption 接受旧 binding 空形态——代际守卫为 `task_binding IS NULL`（事务内复验）；既有 v1 binding 路径不变。可新增兄弟方法或扩签名，保持既有调用方编译行为不变或同步更新
- [ ] 2.3 adoptionStore 窄接口（v2_adopt.go:47-53）同步扩展
- [ ] 2.4 store 测试：NULL binding CAS 成功 + 并发写入后失配拒绝 + 锚定/归属转换正确

## 3. 通知与端到端

- [ ] 3.1 stage4 承接测试补 legacy 形态用例（无 binding + anchor + 归属 → 承接成功 + 通知写入）
- [ ] 3.2 `go build ./...` + `go test -count=1 ./internal/task/... ./internal/infrastructure/store/...` 全绿
- [ ] 3.3 `openspec validate v1-legacy-anchor-adopt --strict` 通过
- [ ] 3.4 Docker 模拟容器实证：v1 任务置空 task_binding → 升级 v2 → 激活自动承接成功（orchestrator 执行）
