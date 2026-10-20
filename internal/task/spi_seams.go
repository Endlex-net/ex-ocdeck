// spi_seams.go 将 Manager 的启动旁路副作用暴露为 SPI supervisor 装配 seam
// （opencode-v1-spi-provider tasks 4.x 生产装配；design D11：v1 适配器的
// runtime 生命周期旁路——env_snapshot 端口持久化、permission_mode_at_start、
// env 组装——委托 task 层现状实现，与 startRuntimeWithPortRetry 内部步骤
// 逐字一致，保证行为等价）。端口分配 seam 同此。
package task

import (
	"context"
	"database/sql"
	"fmt"
)

// SupervisorSideEffectsAdapter 实现 opencode.SupervisorSideEffects 的 task 层
// 委托（行为与 startRuntimeWithPortRetry 建进程前的三步逐字一致）。
type SupervisorSideEffectsAdapter struct {
	m *Manager
}

// NewSupervisorSideEffects 构造旁路副作用 seam（生产装配用）。
func NewSupervisorSideEffects(m *Manager) *SupervisorSideEffectsAdapter {
	return &SupervisorSideEffectsAdapter{m: m}
}

// ResolvePermissionMode 解析任务有效权限模式并写入启动事实（现状 =
// resolvePermissionMode + SetPermissionModeAtStart；任何失败 fail-closed
// 不建进程）。
func (a *SupervisorSideEffectsAdapter) ResolvePermissionMode(ctx context.Context, taskID string) (string, error) {
	row, err := a.m.store.GetTask(ctx, taskID)
	if err != nil {
		return "", fmt.Errorf("get task: %w", err)
	}
	mode, err := resolvePermissionMode(row)
	if err != nil {
		return "", err
	}
	if serr := a.m.store.SetPermissionModeAtStart(ctx, taskID, mode); serr != nil {
		return "", fmt.Errorf("set permission_mode_at_start: %w", serr)
	}
	return mode, nil
}

// PersistServePort 持久化轮换后的 serve env 快照（现状 = persistEnvSnapshot；
// 失败 → 本次 attempt 失败，MUST NOT 建会话）。
func (a *SupervisorSideEffectsAdapter) PersistServePort(ctx context.Context, taskID string, env map[string]string) error {
	return a.m.persistEnvSnapshot(ctx, taskID, env)
}

// BuildServeEnv 组装 serve env 快照。C22 同代快照边界（orchestration spec :145
// 「可重复进程 attempt」：恢复 attempt 的 env 来源 MUST 是已持久化的同代快照，
// 不经 layerEnvSnapshot 重算）：激活入口 mergeEnvSnapshot、恢复入口
// runRecoveryAttempt 都先持久化同代快照（含 OCDECK_SERVE_PORT 本代 ownership
// 端口），快照在 attempt 层是唯一 env 事实源——持久化后新增的全局/项目/任务级
// env 配置不混入本代 runtime进程，其生效点是下一次快照持久化（新 attempt/新代
// 激活写入时刻；用户配置变更在既定生效点应用，不在代际 mid-flight 漂移）。
// 快照缺失/坏 JSON fail-closed（不可自愈，spec：持久化失败判定 attempt 失败，
// MUST NOT 建会话）。
func (a *SupervisorSideEffectsAdapter) BuildServeEnv(ctx context.Context, taskID string) (map[string]string, error) {
	row, err := a.m.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	snap, err := a.m.loadEnvSnapshot(row)
	if err != nil {
		return nil, fmt.Errorf("load env snapshot: %w", err)
	}
	return snap, nil
}

var _ = sql.NullInt64{} // AllocateServePort 使用（保持 import 语义清晰，见下）

// AllocateServePort 是 SPI supervisor 的端口分配 seam（现状 = allocatePort；
// lastPort 复用提示 + exclude 排除项）。Manager 的内部分配器以 sql.NullInt64
// 承载复用提示，此处做形状转换。
func (m *Manager) AllocateServePort(lastPort int, lastValid bool, exclude int) (int, error) {
	return m.allocatePort(sql.NullInt64{Int64: int64(lastPort), Valid: lastValid}, exclude)
}
