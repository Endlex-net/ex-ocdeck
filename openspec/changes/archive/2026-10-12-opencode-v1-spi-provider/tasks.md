# Tasks: opencode v1 作为 provider 接入标准 SPI

> 依据：specs（agent-provider-spi / opencode-orchestration delta）+ design.md（D1-D11、D6 五阶段、行为保持矩阵）
> **发布边界**：阶段 2（类型重构）与阶段 3（迁移导入+事实来源切换）MUST 同一 release；阶段 2 单独发布被禁止（design D6）。阶段 3 可作为独立 PR 开发与回滚准备，但不得脱离阶段 2 单独发布。依赖顺序：阶段 0 → 阶段 1 →（阶段 2 + 阶段 3）→ 阶段 4；每阶段进入条件 = 前一阶段测试全绿。

## 1. 阶段 0：Characterization Tests 与行为保持基线

- [x] 1.1 实现脚本化 fake opencode server（httptest，按 CONTRACT.md 锚点形状应答 + 可编程 SSE 事件脚本）与六观察面比较 harness（HTTP/Web DTO 逐字段 JSON、store dump 排序行集、process 调用序列、notice/last_error 精确文本、session ownership 集合、SSE 应用后内存态；`capabilities` 字段为比较例外、单独验证）。验证：harness 在重构前基线代码上跑通
- [x] 1.2 落地**基线组 fixture**（仅含重构前已存在且可在旧路径执行的行为；通过条件 = 在基线代码上通过）：
  - 激活/端口重试（端口占用、进程死亡、健康超时脚本）；anchor bootstrap 三支（有效锚定直接启动 / 失效锚定 CAS 清空转无锚定 / 无锚定双启动）
  - SSE 捕获与并发 claim（两任务并发仅一成功）；`session.updated` 不创建归属
  - 有效模式分流：repo+worktree 全量对齐 / repo+local-path ownedOnly / dir+local-path ownedOnly；非法组合（dir+worktree、未知 kind、未知 mode）在任何状态修改、SSE、对齐、锚定或进程副作用之前报错且零副作用（不写 last_error）
  - ownedOnly 对齐的 complete/overflow 前置判定顺序、session_overflow notice 的 CAS 写入与清除行为
  - attention 六家族事件（含缺字段畸形静默忽略）、diffreview 能力探测、标题同步三态（成功/404 降级缓存/网络失败不缓存）
  - suspend/delete/reconcile 正常与各失败注入；重启恢复（进程存活/消失）；历史重复行 fail-closed
- [x] 1.3 落地**契约组 fixture 骨架**（新 SPI 契约行为，基线上允许编译存在但不全通过；通过条件 = 阶段 2/3 实现后通过）：迁移导入 runner、provider registry 分派与未注册 fail-closed、provider 切换入口不可达 fail-closed（本期 registry 仅 opencode-v1；D9a 完整切换流程属后续变更）、派生凭据（derived/legacy/主密钥场景）、删除清理的临时绑定分支、回滚再升级命名空间补齐、能力健康透出。验证：骨架编译通过，阶段 2/3 完成后全绿

**阶段 0 完成条件**：1.1（harness）+ 1.2（基线组）在基线代码上全绿；1.3 仅要求 fixture 骨架可编译（契约组的行为通过条件延后至阶段 2+3 同 release 完成后，见 5.3 终验）。

## 2. 阶段 1：Schema 迁移（migration 0018）

- [x] 2.1 编写 migration 0018：tasks +`provider_id` TEXT NOT NULL DEFAULT 'opencode-v1'、+`task_binding` TEXT NULL、+`anchor_provider_id`/`anchor_provider_instance_id`（DEFAULT 'opencode-v1'/'default'）；task_sessions +`provider_id`/`provider_instance_id`（同上默认值）；主键不变。验证：迁移测试通过；旧数据库文件执行迁移后全部行命名空间正确
- [x] 2.2 实现命名空间防御性补齐启动步骤（幂等、单事务、先于任何命名空间消费者、与 binding runner 独立；覆盖 NULL/空串行，保留合法已有值）。验证：「升级→旧二进制写入→再升级」fixture 通过

## 3. 阶段 2：SPI 契约包与类型重构（不可发布中间态，与阶段 3 同 release）

- [x] 3.1 新建 `internal/domain/agentprovider`：SessionKey、TaskBinding（versioned envelope）、AgentSession、AgentEvent（逐 Kind 单负载不变量）、AgentStatus/StatusDetail（retry 明细、NextAt epoch 毫秒）、SessionErrorDetail（StatusCode/IsRetryable 可空指针）、Attention/Payload、EventKind、ListResult（Overflow 语义）。验证：单元测试覆盖不变量（AgentEvent 每 Kind 恰好一个非 nil 负载）
- [x] 3.2 新建 `internal/application/agentprovider`：Provider/RuntimeSupervisor（Acquire/Resume(ResumePolicy)/AcquireEphemeral/Release）/SessionControl/EventSource/AttentionIO/AsyncPrompt/StatusReader/TerminalIO/TitleUpdater 窄接口、EventStream、Error+ErrorKind sentinel（errors.Is 按 Kind 匹配）、CapabilityHealth、registry（未注册 fail-closed）。验证：接口编译期断言 + registry fail-closed 测试通过
- [x] 3.3 v1 适配器迁入：`internal/infrastructure/opencode` 实现 Provider 全部窄接口；SSE 解析迁入 typed events（D8 字段级映射逐字继承现状：六家族校验、session.error 字段路径、directory 同源校验、未知枚举双路径语义）；HTTP 错误默认映射 + 操作级例外（Reply 按 `_tag`、Title 404→CapabilityUnavailable、Prompt 四分类通道规则）。验证：行为保持矩阵 attention/permission/status/diffreview/标题 fixture 在新路径通过
- [x] 3.4 连接管理保持（orchestration spec「serve HTTP 客户端连接管理」）：provider adapter 统一注入共享 loopback transport（跨实例按 host:port 连接池复用、MaxIdleConns/IdleConnTimeout 有界回收、`Proxy: nil` 显式禁用代理、directory-scoped 请求显式携带 directory）。验证：新 client 复用连接池测试、空闲回收测试、代理环境下直连 127.0.0.1 测试通过
- [x] 3.5 store 层：SessionKey 列读写、TaskBinding 列读写（版本化解析、未知版本 fail-closed）、claim 迁移为 BEGIN IMMEDIATE + 三列谓词（D4 算法）；既有 store_test.go / anchor_recovery_test.go 断言随类型迁移扩展至完整 SessionKey 形状（单命名空间下行为等价）。验证：SQLite 并发 claim 测试（两任务并发仅一个成功）；OwnerOf 历史重复行 fail-closed 测试通过；扩展后的既有测试通过
- [x] 3.6 task 层消费点迁移（D5 清单：activate.go 6 处、reconcile.go 2 处、suspend/delete/agent_status 各 1 处直调点 + attention/permit_auto 经 taskOcClient 分派）；`internal/application/dto.go` 去除 `opencode.*` 嵌入（PendingPermission/PendingQuestion 改嵌 provider-neutral payload，字段形状不变）。验证：`grep -rn "infrastructure/opencode" internal/task internal/application` 零命中 + DTO 序列化字段形状测试通过；全量测试通过
- [x] 3.7 `cmd/ocdeck-server` 手工装配 registry（v1 provider 注册，缺失即启动失败）。验证：启动冒烟 + 未注册 provider fixture 通过
- [x] 3.8 capability 健康透出：内存按 runtime 代际维护（Acquire/Resume 重建换代、server 重启归零 unknown），日志 warn + `GET /api/v1/server/status` 新增 `capabilities` 只读字段。HTTP JSON 精确形状（逐字段断言）：`{"primitive": string, "scope": "provider"|"runtime"|"session", "epoch": number, "state": "unknown"|"available"|"unsupported"|"degraded", "reason": string, "taskId": string, "session": null | {"providerId": string, "providerInstanceId": string, "upstreamId": string}}`。验证：状态迁移矩阵测试（unknown→available、协议违约→degraded、明确不支持→unsupported、epoch 递增与重启归零）；上述 JSON 字段名与 session 可空形状的精确断言；日志 warn 断言

## 4. 阶段 3：迁移导入 runner 与事实来源切换（与阶段 2 同 release；独立 PR 开发，可独立回滚）

- [x] 4.1 派生凭据：实现 D3 固定公式（base64url_nopad(HMAC-SHA256(master_secret, "ocdeck-agent-v1"‖0x00‖provider_id‖0x00‖task_id))[:32]）、master secret 解析（env OCDECK_MASTER_SECRET 优先于 ~/.config/ocdeck/master_secret 0600 文件，缺失 fail-closed）、spawn 注入派生密码 + 非秘密来源标记 `OCDECK_CRED_KIND=derived`。验证：固定测试向量通过
- [x] 4.2 迁移导入 runner：每次启动幂等执行（主密钥准入门→全部 active 候选「验证或导入」→来源标记判定分支→单事务写入/替换）；启动屏障（出结论前消费者跳过候选任务）；中断重启续跑；derived 标记任务禁止读取/落库密码。验证：迁移导入全部 fixture（密钥改变禁止落库 / 密钥未变回滚序列 legacy 收敛 / 主密钥不可用拒绝就绪 / 中断续跑）
- [x] 4.3 事实来源切换：task 层全部 runtime 连接按持久化 binding 路由（binding 提交协议：supervisor 只返回不持久化，task 层成功提交事务写入，失败零持久化副作用）；删除清理接入 AcquireEphemeral 分支规则（复用/临时绑定、失败阻断删除提交）。验证：行为保持矩阵重启恢复/删除清理 fixture 通过
- [x] 4.4 恢复状态机接线：ResumePolicy 三入口映射（resumeActive/tryRepairRuntime/ensureRecovery）与 BeforeLaunch 闸门（先于端口分配）；keyed mutex + `active→activating→active` CAS、持久化 permit（ordinal/5 分钟窗口/退避 5s/15s/45s、permit 写入先于端口与进程副作用）、token 失效处理（零清理零状态写入）。验证（逐项对应 orchestration spec「任务进程自动重拉」scenario）：「用户退出 TUI 后自动重拉」「进程崩溃后自动重拉」「重拉预算耗尽落挂起」「主动 kill 不触发重拉」「activating 中消失不重复介入」「恢复与挂起竞争」——每场景固定输入与断言（CAS 序列、permit 计数、last_error 文本、最终状态）
- [x] 4.5 失败补偿与 cleanup debt：终态补偿（统一 HasSession/KillSession fail-closed 映射 + `CompleteRecoveryFailure` 条件事务原子写 status/last_error/env_snapshot）、pending/tagged debt replay（phase=cleanup_notice|complete）、迁移导入 runner 与恢复预算的隔离（屏障期不耗预算）。验证（固定输入与结果）：cleanup notice 写失败→pending 形成与重放成功后续 `CompleteRecoveryFailure`；`complete` debt 直接重放；`CompleteRecoveryFailure` CAS mismatch→删除 debt 且三字段不改；notice/debt 持久化失败→不执行终态事务

## 5. 阶段 4：清理与收尾

- [x] 5.1 移除正常生命周期路径的 tmux env 反查、`last_port` 恢复依据、`env_snapshot` runtime 解释（保留迁移导入 runner 与删除清理的特许例外；reconcile persist 恢复的 `OCDECK_TASK_ID` 会话 env 复检为安全校验特许保留——仅核对会话 env 任务标识与 DB 任务一致，不读取凭据或连接端点）；旧字段保留于 schema。验证：`grep -rn "ShowSessionEnv" internal/task` 仅剩特许路径；全量测试通过
- [x] 5.2 敏感数据审查：legacy `auth.password` 与 master secret 不进入 API DTO/日志/事件/错误文本。验证：DB 全文检索断言（derived 任务无密码、legacy 仅标记字段）+ DTO 序列化审查测试通过
- [x] 5.3 终验：行为保持矩阵新旧路径全量等价（六观察面，`capabilities` 字段按比较例外单独验证）；`openspec validate opencode-v1-spi-provider --strict` 通过；`/opsx-verify` 通过
