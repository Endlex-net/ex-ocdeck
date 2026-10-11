// migration_runner.go 实现迁移导入 runner（opencode-v1-spi-provider tasks 4.2 /
// design D6 阶段 3 + SPI spec 启动屏障）：每次启动幂等执行——快照「持久化 active
// 任务」为本次候选集合，逐任务出「验证或导入」结论。
//
// 执行契约（design D6）：
//   - 主密钥准入门先于一切网络验证与桥接：不可用 → runner 整体失败（server 拒绝
//     进入服务就绪；候选屏障不释放、零状态修改、零 env 读取）。
//   - 已有 binding 且可解析 → 以其端点/凭据做健康检查（derived 现场派生）；
//     通过则保留（不读取旧来源、不修改）。失败落来源标记判定。
//   - 来源标记判定（唯一判据 = spawn env 非秘密标记 OCDECK_CRED_KIND，MUST NOT
//     以密码值比对代替）：标记 derived → 派生凭据 runtime，MUST NOT 读取/导入/
//     落库其密码，记 last_error 标注疑似主密钥或凭据问题，不修改 binding；
//     标记缺失 → 旧二进制随机凭据 runtime，读取端口与密码并健康检查，通过则以
//     auth.kind="legacy" 写入 binding，失败记 last_error。
//   - 防御不变量：任何路径 MUST NOT 把等于当前派生值的密码写入存储（比对仅作
//     防御断言，不作来源判据）。
//   - 单任务失败继续；中断重启续跑（幂等，无断点状态）；候选出结论前消费者
//     跳过——屏障由启动序列顺序保证（runner 完成 → Reconcile/HTTP 才启动）。
//
// binding 仅在 legacy 导入时写入（阶段 3 切换属后续段——激活提交事务写 binding
// 随事实来源切换落地）；derived 无密码落库（消费方现场派生，见 appagent 原语）。
package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
	"ocdeck/internal/infrastructure/store"
)

// RunMigrationImport 执行迁移导入（每次启动幂等）。返回 error 仅代表整体级失败
// （master gate / 快照失败）——server MUST fail-closed 拒绝就绪；单任务失败经
// last_error 落账后继续其余候选。
func (m *Manager) RunMigrationImport(ctx context.Context) error {
	// 主密钥准入门：先于一切网络验证与桥接（零 env 读取在 gate 之前）。
	master, err := appagent.ResolveMasterSecret()
	if err != nil {
		return fmt.Errorf("migration import: master secret gate: %w", err)
	}
	rows, err := m.store.ListAllTasks(ctx)
	if err != nil {
		return fmt.Errorf("migration import: snapshot active tasks: %w", err)
	}
	for _, row := range rows {
		if row.Status != StatusActive {
			continue
		}
		if terr := m.migrateImportTask(ctx, row, master); terr != nil {
			log.Printf("migration import: task %s: %v", row.ID, terr)
		}
	}
	return nil
}

// migrateImportTask 对单个候选出「验证或导入」结论。返回值仅供诊断日志——
// 单任务失败 MUST NOT 中断其余任务（结论经 last_error 落账）。
func (m *Manager) migrateImportTask(ctx context.Context, row TaskRow, master string) error {
	// ① binding 存在且可解析 → 健康验证；通过则保留（零修改）。
	// C5 门禁修复（design D6 阶段 3 判定链 1→2）：store 读取失败（行缺失/DB 故障）
	// 与 envelope 解析失败分派——前者属数据层不可达，fail-closed 结论（不进入
	// 来源判定：marker 读取亦无从谈起任务的 runtime 事实）；后者落 ② 来源判定
	//（specs「迁移导入桥接」：binding 无法解析时 MUST 读取旧来源验证或导入）。
	binding, has, berr := m.store.GetTaskBinding(ctx, row.ID)
	if berr != nil {
		// C5 门禁修复（mutation 证据：分派判定旁路 `|| true`——store 读取失败
		// 滑入来源判定 legacy 路径写 binding → 测试 binding_read_store_failure_
		// stays_failclosed 红）。
		if bindingCorruptErr(berr) {
			// envelope 解析失败 → 落 ② 来源标记判定。
		} else {
			return m.concludeImportFailure(ctx, row, fmt.Errorf("binding 读取失败: %v", berr))
		}
	}
	if has && berr == nil {
		if port, pw, pok, perr := m.bindingCredential(ctx, binding, master); perr == nil && pok {
			oc := m.ocFactory(binding.ProviderID, port, pw, appagent.Options{HealthTimeout: 2 * time.Second, OpTimeout: 5 * time.Second})
			if h, herr := oc.Health(ctx); herr == nil {
				// C5 门禁修复（mutation 证据：删 Healthy 检查即 `if true || h.Healthy`
				// 降级 err-only 判定 → 测试 health_protocol_violation_not_preserved 红
				//——binding 被当作「验证通过」保留、last_error 空）。
				if h.Healthy {
					return nil // 验证通过 → 保留，不读取旧来源、不修改。
				}
			}
		}
		// 凭据不可用/健康失败 → 落来源标记判定（与缺失同分支，D6 第 2 句）。
	}

	// ② 来源标记判定（唯一判据 = OCDECK_CRED_KIND）。标记读取错误与 derived
	// 显式区分：ShowSessionEnvContext 失败时不得当作「标记缺失」滑入 legacy
	// 导入（C5 门禁修复——缺失才允许 legacy；未知值 fail-closed 不读密码）。
	name := runtimeSessionName(row.ID)
	marker, markerErr := m.markerForImport(ctx, name)
	// C5 门禁修复（mutation 证据：markerErr 静默降级为空 marker——读取失败被当作
	// 缺失滑入 legacy 判定 → 测试 marker_read_error_and_unknown_value_failclosed
	// 红，last_error 变「会话 env 不可读」）。
	if markerErr != nil {
		return m.concludeImportFailure(ctx, row,
			fmt.Errorf("来源标记读取失败: %v", markerErr))
	}
	switch {
	case marker == appagent.DerivedCredKind:
		// 派生凭据 runtime：MUST NOT 读取/导入/落库其密码；不修改 binding；
		// 记 last_error 标注疑似主密钥或凭据问题（fail-closed 结论）。
		return m.concludeImportFailure(ctx, row,
			fmt.Errorf("runtime 使用派生凭据（标记 %s）；疑似主密钥或凭据问题，禁止读取/导入/落库密码，binding 保持不变", appagent.DerivedCredKind))
	case marker == "":
		// 标记缺失 → 旧二进制随机凭据 runtime（确认 legacy），读取端口与密码做健康验证。
	default:
		// 未知标记值 fail-closed：不读密码、不写 binding（D5/D6 唯一判据语义）。
		return m.concludeImportFailure(ctx, row,
			fmt.Errorf("未知来源标记 %q；fail-closed 不读取密码", marker))
	}
	pw, _ := m.proc.ShowSessionEnvContext(ctx, name, "OPENCODE_SERVER_PASSWORD")
	portStr, _ := m.proc.ShowSessionEnvContext(ctx, name, "OCDECK_SERVE_PORT")
	if pw == "" || portStr == "" {
		return m.concludeImportFailure(ctx, row,
			fmt.Errorf("runtime 会话 env 不可读（密码/端口缺失，marker=%q）", marker))
	}
	port, ok := parsePort(portStr)
	if !ok {
		return m.concludeImportFailure(ctx, row, fmt.Errorf("invalid serve port %q", portStr))
	}
	oc := m.ocFactory(store.ProviderIDV1, port, pw, appagent.Options{HealthTimeout: 2 * time.Second, OpTimeout: 5 * time.Second})
	// C5 残余（review attempt 2）：legacy 分支同样要求 herr==nil && h.Healthy——
	// 与 ① derived 验证分支对称（协议应答 200 + healthy=false 的违约不是「验证
	// 通过」，MUST NOT 导入 binding）。mutation 证据（已验证 → 红 → 还原）：恢复
	// err-only 判定（`herr != nil` 单条件）→ 测试
	// TestCharxContract_MigrationImportRunner/legacy_health_protocol_violation 红。
	if h, herr := oc.Health(ctx); herr != nil || !h.Healthy {
		return m.concludeImportFailure(ctx, row, fmt.Errorf("legacy runtime 健康检查失败: %v", herr))
	}
	// 防御不变量：等于当前派生值的密码 MUST NOT 落库（比对仅作防御断言）。
	derived := appagent.DeriveRuntimePassword(master, store.ProviderIDV1, row.ID)
	if pw == derived {
		return m.concludeImportFailure(ctx, row, fmt.Errorf("密码等于当前派生值；导入跳过（防御断言）"))
	}
	// ④ legacy 导入：binding envelope（D3 形状；envelope data 为 v1 私有 JSON——
	// 逐字按 D3 示例构造 {"port","directory","auth":{"kind":"legacy","password"}}），
	// UpstreamSessionID 取锚定（empty → 无法构造合法 envelope，fail-closed 结论）。
	if !row.AnchorSessionID.Valid || row.AnchorSessionID.String == "" {
		return m.concludeImportFailure(ctx, row, fmt.Errorf("任务无锚定 session；无法构造 binding envelope"))
	}
	data, merr := json.Marshal(map[string]interface{}{
		"port":      port,
		"directory": row.WorktreePath,
		// C6 残余（review attempt 2）：legacy data 同样携带 taskId（健康观测身份键
		// 的显式上下文——禁止以 upstream session ID 顶替 task ID）。
		"taskId": row.ID,
		"auth":   map[string]string{"kind": appagent.LegacyCredKind, "password": pw},
	})
	if merr != nil {
		return m.concludeImportFailure(ctx, row, fmt.Errorf("marshal binding data: %v", merr))
	}
	legacy := domain.TaskBinding{
		Version:            domain.TaskBindingVersion,
		ProviderID:         store.ProviderIDV1,
		ProviderInstanceID: store.ProviderInstanceV1Default,
		UpstreamSessionID:  row.AnchorSessionID.String,
		TerminalRef:        name,
		Data:               data,
	}
	if serr := m.store.SetTaskBinding(ctx, row.ID, legacy); serr != nil {
		return m.concludeImportFailure(ctx, row, fmt.Errorf("写入 binding: %v", serr))
	}
	log.Printf("migration import: task %s: legacy binding imported (port=%d, anchor=%s)", row.ID, port, row.AnchorSessionID.String)
	return nil
}

// bindingCredential 解析既有 binding 的健康验证凭据（derived → 现场派生；
// legacy → 存量导入值；解析失败 → !pok）。
func (m *Manager) bindingCredential(ctx context.Context, b domain.TaskBinding, master string) (port int, password string, ok bool, err error) {
	var data struct {
		Port      int    `json:"port"`
		Directory string `json:"directory"`
		TaskID    string `json:"taskId"`
		Auth      struct {
			Kind     string `json:"kind"`
			Password string `json:"password"`
		} `json:"auth"`
	}
	if uerr := json.Unmarshal(b.Data, &data); uerr != nil {
		return 0, "", false, uerr
	}
	switch data.Auth.Kind {
	case appagent.DerivedCredKind:
		// derived：密码现场派生（master + provider + taskId——taskId 随 data 携带，
		// 与 supervisor buildBinding 的写入形状一致）。
		if data.TaskID == "" {
			return 0, "", false, fmt.Errorf("derived binding 缺少 taskId")
		}
		return data.Port, appagent.DeriveRuntimePassword(master, store.ProviderIDV1, data.TaskID), true, nil
	case appagent.LegacyCredKind:
		if data.Auth.Password == "" {
			return 0, "", false, fmt.Errorf("legacy binding 缺少导入密码")
		}
		return data.Port, data.Auth.Password, true, nil
	default:
		return 0, "", false, fmt.Errorf("未知 auth.kind %q", data.Auth.Kind)
	}
}

// concludeImportFailure 记 last_error（状态保持不变——runner 不做状态迁移）
// 并返回诊断错误。
func (m *Manager) concludeImportFailure(ctx context.Context, row TaskRow, cause error) error {
	le := sql.NullString{String: fmt.Sprintf("migration import: %v", cause), Valid: true}
	if _, werr := m.writeStatus(ctx, row.ID, row.Status, le); werr != nil {
		return fmt.Errorf("%v; record last_error: %v", cause, werr)
	}
	return cause
}

// markerForImport 读取来源标记（specs「迁移导入桥接」唯一判据）。不存在返回
// ("", nil)——仅「标记缺失」允许 legacy 导入；tmux 命令失败返回 error（C5：
// 读取错误 MUST NOT 被当作缺失滑入 legacy 路径）。
func (m *Manager) markerForImport(ctx context.Context, name string) (string, error) {
	return m.proc.ShowSessionEnvContext(ctx, name, appagent.CredKindEnvKey)
}

// bindingCorruptErr 判定 GetTaskBinding 错误是否属 envelope 解析/校验失败（落 ②
// 来源判定），与 store 读取失败（行缺失 sql.ErrNoRows / DB 故障）区分（C5 门禁
// 修复：判定锚 domain.ErrBindingEnvelopeCorrupt 哨兵——GetTaskBinding 与
// DecodeTaskBinding 均以 %w 包装）。
func bindingCorruptErr(err error) bool {
	return errors.Is(err, domain.ErrBindingEnvelopeCorrupt)
}
