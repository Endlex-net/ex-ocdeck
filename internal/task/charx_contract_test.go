package task

// charx_contract_test.go 落地 opencode-v1-spi-provider 阶段 0 的契约组 fixture
// 骨架（tasks.md 1.3）：新 SPI 契约行为——迁移导入 runner、provider registry
// 分派与未注册 fail-closed、provider 切换入口不可达 fail-closed（D9a）、派生凭据、
// 删除清理的临时绑定分支、回滚再升级命名空间补齐、能力健康透出。
//
// 阶段 0 仅要求骨架可编译：每个 fixture 登记固定输入与断言面（注释内契约描述
// 引用 design.md / specs），行为断言以 t.Skip 挂起，待阶段 2/3 实现后在同一
// fixture 上补齐（通过条件 = 阶段 2+3 同 release 完成后全绿，见 tasks.md 5.3）。
//
// 骨架与基线组共用 charx harness（fake opencode server + 六观察面快照），
// 保证阶段 2/3 落地时 fixture 无需重建观察基建。

import "testing"

// TestCharxContract_MigrationImportRunner 迁移导入 runner（design D6 阶段 3）。
//
// 固定输入：持久化 active 任务集（binding 存在且健康 / binding 缺失+标记缺失
// （legacy 随机凭据 runtime）/ binding 缺失+OCDECK_CRED_KIND=derived（密钥疑似变化）/
// 健康检查失败）；master secret 可用与不可用两环境。
// 断言面（待阶段 3）：
//   - 主密钥不可用 → server 拒绝进入服务就绪、任务状态不变、屏障不释放；
//   - legacy 分支：binding 以 auth.kind="legacy" 写入或替换，幂等重跑收敛；
//   - derived 标记分支：MUST NOT 读取/导入/落库密码，记 last_error，不修改 binding；
//   - 启动屏障：候选任务结论前消费者跳过（resumeActive/重拉/attention 不介入）；
//   - 副作用边界：不建进程、不删 session、不改锚定与归属。
func TestCharxContract_MigrationImportRunner(t *testing.T) {
	t.Skip("TODO(stage-3/tasks 4.2): 迁移导入 runner 实现后落行为断言（含密钥改变禁止落库 / 回滚序列 legacy 收敛 / 中断续跑 fixture）")
}

// TestCharxContract_RegistryDispatchAndUnregisteredFailClosed provider registry
// 分派与未注册 fail-closed（design D5 / specs「Provider registry 分派」）。
//
// 固定输入：任务 provider_id=opencode-v1（已注册，正常分派）；任务 provider_id
// 被篡改为未注册值（fixture 注入）。
// 断言面（待阶段 2/3）：
//   - 已注册：生命周期操作经 registry 分派到 v1 适配器，六观察面与基线组等价；
//   - 未注册：生命周期操作 fail-closed（零进程/网络副作用），诊断性 last_error
//     为允许的持久化副作用（区别于非法组合的零 last_error）。
func TestCharxContract_RegistryDispatchAndUnregisteredFailClosed(t *testing.T) {
	t.Skip("TODO(stage-2/tasks 3.2+3.7): registry 与 v1 注册落地后断言分派与 fail-closed")
}

// TestCharxContract_ProviderSwitchUnreachableFailClosed provider 切换入口
// 不可达 fail-closed（design D9a：本期 registry 仅 opencode-v1）。
//
// 固定输入：suspended 任务 + 目标 provider 未注册（本期唯一可达形态）。
// 断言面（待阶段 2/3）：切换请求零副作用报错（诊断性 last_error 允许）；
// provider_id/binding/锚定均不变。
func TestCharxContract_ProviderSwitchUnreachableFailClosed(t *testing.T) {
	t.Skip("TODO(stage-2): D9a 完整切换流程属后续变更；本期仅断言未注册目标 fail-closed")
}

// TestCharxContract_DerivedCredentials 派生凭据（design D3 固定公式）。
//
// 固定输入：master secret 测试向量（固定字节）+ provider_id/task_id 组合；
// 新任务激活（derived，spawn env 含 OPENCODE_SERVER_PASSWORD=派生值与
// OCDECK_CRED_KIND=derived）；存量任务导入（legacy）→ 下次激活切换 derived。
// 断言面（待阶段 3）：
//   - 派生公式固定测试向量：base64url_nopad(HMAC-SHA256(master,"ocdeck-agent-v1"\x00
//     provider\x00 task))[:32]；
//   - proc NewSession env 观察面：密码值精确等于派生值、来源标记存在；
//   - 同任务跨重启密码恒定；DB 全文检索：derived 任务无密码、legacy 仅标记字段。
func TestCharxContract_DerivedCredentials(t *testing.T) {
	t.Skip("TODO(stage-3/tasks 4.1): 派生凭据实现后断言固定测试向量与 env 注入")
}

// TestCharxContract_DeleteCleanupEphemeralBinding 删除清理的临时绑定分支
// （design D9 表删除清理行 / D10 例外行）。
//
// 固定输入：suspended + owned sessions + NULL binding（走 AcquireEphemeral）；
// 陈旧 binding（runtime 已终止，MUST NOT 走普通 Resume）；临时 runtime 清理失败
// 重试。
// 断言面（待阶段 3）：
//   - 分支①复用存活 runtime；分支②临时绑定不持久化、不建锚定/SSE、不提交状态；
//   - 逐项删除 owned（现有 deleteOCSessions 行为保持）；
//   - 清理失败阻断删除提交且票据可重试（deletion_failed 语义保持）。
func TestCharxContract_DeleteCleanupEphemeralBinding(t *testing.T) {
	t.Skip("TODO(stage-3/tasks 4.3): AcquireEphemeral 分支落地后断言临时绑定不持久化")
}

// TestCharxContract_RollbackUpgradeNamespaceBackfill 回滚再升级命名空间补齐
// （design D6 阶段 1 验证 fixture）。
//
// 固定输入：「升级（migration 0018）→ 旧二进制写入（新任务/归属/锚定，命名空间
// 列取 DDL 默认值）→ 再升级」序列；含旧二进制显式写空值的边界行。
// 断言面（待阶段 1/2）：
//   - 再升级后全部行命名空间正确（provider_id='opencode-v1'、
//     provider_instance_id='default'、anchor 两列同理）；
//   - 防御性补齐幂等（合法已有值保留）；归属判定结果与现状一致。
func TestCharxContract_RollbackUpgradeNamespaceBackfill(t *testing.T) {
	t.Skip("TODO(stage-1/tasks 2.1-2.2): migration 0018 与补齐步骤落地后断言命名空间收敛")
}

// TestCharxContract_CapabilityHealthSurfacing 能力健康透出（design D7）。
//
// 固定输入：runtime 代际（Acquire/Resume 重建换代、server 重启归零）；协议违约
// 响应脚本（degraded）；上游明确不支持脚本（unsupported）。
// 断言面（待阶段 2/tasks 3.8）：
//   - GET /api/v1/server/status 新增 capabilities 只读字段的 HTTP JSON 精确形状
//     （primitive/scope/epoch/state/reason/taskId/session 逐字段，session 可空）；
//   - 状态迁移矩阵：unknown→available、协议违约→degraded、明确不支持→unsupported、
//     epoch 递增与重启归零；
//   - 日志 warn 断言；capabilities 字段为比较例外（基线无对应，单独按新契约验证）。
func TestCharxContract_CapabilityHealthSurfacing(t *testing.T) {
	t.Skip("TODO(stage-2/tasks 3.8): capability 健康透出落地后断言 JSON 形状与状态矩阵")
}
