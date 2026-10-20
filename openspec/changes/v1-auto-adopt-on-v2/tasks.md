# Tasks

## 1. 开关默认值翻转

- [ ] 1.1 `internal/config/config.go`：`v2AdoptionEnabled` 默认值翻转为开——nil lookup 或未配置返回 true；显式 `0`/`false`/`off`（大小写不敏感、容忍首尾空白）返回 false；`1`/`true`/`on` 仍返回 true（其他未知值保守视为关闭并保留现状语义，与既有 default 分支一致）
- [ ] 1.2 更新 `V2AdoptionEnv` 与 `V2Adoption` 字段注释（默认关 → 默认开；关闭形态不变）
- [ ] 1.3 config 测试：新增/更新开关解析断言（未配置=开、显式关=关、显式开=开、未知值=关）

## 2. R9 文案与通知措辞

- [ ] 2.1 `internal/task/resolver.go`：R9 DenyReason 更新为「承接开关已显式关闭（OCDECK_V2_ADOPTION=0）」语义，附恢复方式说明
- [ ] 2.2 `internal/task/v2_adopt.go`：`adoptionPendingLossNotice` 措辞微调，明确「该任务已随 opencode v2 升级自动迁移承接」前缀（保留 pending 不保留的如实告知）
- [ ] 2.3 resolver 测试：R9 断言随文案更新

## 3. 验证

- [ ] 3.1 `go build ./...` + `go test ./internal/config/... ./internal/task/...` 全绿
- [ ] 3.2 `openspec validate v1-auto-adopt-on-v2 --strict` 通过
- [ ] 3.3 Docker 模拟容器端到端实证：v1 会话 + v2 二进制 + 新构建 ocdeck（默认开关）→ 激活 v1 钉扎任务自动承接、会话延续、通知可见
