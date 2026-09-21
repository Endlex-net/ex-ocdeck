// permission_mode.go：任务权限模式运行态（task-permission-mode D5/D6/D8）。
//
// 职责边界：RuntimePermissionState/permEpoch 的定义与全部变更入口（D5 算法钉死：
// AI 启用唯一事实源是 AIAutoEnabled，MUST NOT 另设独立布尔）、三条注册路径共用的
// 启动事实读校验与统一初始化、模式保存后的 runtime 收敛、有效模式推导单一函数与
// Manager.PermissionModeView。判定/回复流程见 permit_auto.go；store 窄端口见
// TaskStore（manager.go）与 store 同名方法。
package task

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"ocdeck/internal/application"
	appnotif "ocdeck/internal/application/notification"
	"ocdeck/internal/application/runtime"
	ocdeckevent "ocdeck/internal/domain/event"
)

// RuntimePermissionState 是权限判定所需的启动事实快照（task-permission-mode D5）。
// 实例不可变：初始化（initPermState）与保存收敛（convergePermissionModeSave）均以
// 新指针原子替换（rt.mu 同步域），读者拷贝指针后可在锁外安全读取。
type RuntimePermissionState struct {
	InstVersion runtime.InstVersion
	// SavedMode 持久化权限模式（经 resolvePermissionMode 校验后的 DB 行值）。
	SavedMode string
	// StartedWithAuto 本进程是否以 --auto 启动（permission_mode_at_start=="all-approve"）。
	StartedWithAuto bool
	// AIAutoEnabled AI 自动判定是否启用：!StartedWithAuto && SavedMode=="ai-auto"。
	// 唯一事实源（spec DR1：--auto 进程保存 ai-auto 后重启不启用 AI）。
	AIAutoEnabled bool
}

// permVerdictRecord 是单条判定的状态记录（D1 三值：inflight/manual_required/settled_no_notify）。
// 携带捕获 epoch：终态提交时与 rt.permEpoch 比较，失配不写（迟到判定丢弃）。
type permVerdictRecord struct {
	epoch uint64
	state string
}

// 判定状态值（D1）：与快照出口的 appnotif 导出枚举同源（本包沿用短名）。
const (
	permVerdictInflight        = appnotif.VerdictInflight
	permVerdictManualRequired  = appnotif.VerdictManualRequired
	permVerdictSettledNoNotify = appnotif.VerdictSettledNoNotify
)

// validateStartFact 校验启动事实列值（D5 三路径矩阵②）：列值由本进程写入，必须恰为
// 三值之一；NULL 由调用方先行判空处理，非法值 fail-closed 拒绝注册 runtime。
func validateStartFact(mode string) error {
	switch mode {
	case PermissionModeAsk, PermissionModeAllApprove, PermissionModeAIAuto:
		return nil
	default:
		return fmt.Errorf("invalid permission_mode_at_start %q", mode)
	}
}

// initPermState 按 D5 统一初始化表构造并挂载 runtime 权限状态（startRuntimeWithPortRetry
// 提交、resumeActive、tryRepairRuntime 三条注册路径共用，禁止各路径各自推导）。
// startMode 为已通过 validateStartFact 校验的 permission_mode_at_start 值；调用时机
// MUST 在 runtime 发布（setRuntime）之前。持久化模式非法 fail-closed 返回错误。
func initPermState(rt *taskRuntime, row TaskRow, startMode string) error {
	saved, err := resolvePermissionMode(row)
	if err != nil {
		return err
	}
	startedWithAuto := startMode == PermissionModeAllApprove
	rt.permState = &RuntimePermissionState{
		InstVersion:     rt.instVersion,
		SavedMode:       saved,
		StartedWithAuto: startedWithAuto,
		AIAutoEnabled:   !startedWithAuto && saved == PermissionModeAIAuto,
	}
	return nil
}

// readStartFactForRegister 注册前读校验启动事实（D5 三路径矩阵②）：有存活进程待注册
// 时启动事实必须存在且合法；NULL/读失败/非法值 fail-closed 返回错误，调用方
// MUST NOT 注册 runtime（非 active 任务不经过此路径，NULL 即异常）。
func (m *Manager) readStartFactForRegister(ctx context.Context, taskID string) (string, error) {
	startPtr, err := m.store.GetPermissionModeAtStart(ctx, taskID)
	if err != nil {
		return "", fmt.Errorf("read permission_mode_at_start: %w", err)
	}
	if startPtr == nil {
		return "", fmt.Errorf("task %s: permission_mode_at_start missing", taskID)
	}
	if err := validateStartFact(*startPtr); err != nil {
		return "", fmt.Errorf("task %s: %w", taskID, err)
	}
	return *startPtr, nil
}

// convergePermissionModeSave 模式保存后的 runtime 收敛（D5 算法/转换矩阵，DB 提交成功
// 后由 UpdateTaskPermissionMode 协调器调用，本 lane 仅提供原语）。rt.mu 内以新指针原子
// 替换 RuntimePermissionState（SavedMode + AIAutoEnabled 成对更新），仅真实「有效
// ai-auto 行为」切换才 permEpoch+1 使在途判定 epoch 捕获失效：切入（false→true）另清空
// judgedPerms 并返回 needScan=true（补判由调用方在锁外发起 judgeScan）；切出仅失效在途。
// DR1：startedWithAuto 的 runtime 有效恒为 all-approve，保存值变化不动 epoch、不补判。
// 锁边界（防自锁）：本函数返回即已释放 rt.mu，调用方此后才可调用 judgeScan。
// 无 runtime / 状态未初始化时 no-op 返回 false。
func (m *Manager) convergePermissionModeSave(taskID, mode string) bool {
	rt := m.getRuntime(taskID)
	if rt == nil {
		return false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.permState == nil {
		return false
	}
	next := *rt.permState
	next.SavedMode = mode
	next.AIAutoEnabled = !next.StartedWithAuto && mode == PermissionModeAIAuto
	switched := next.AIAutoEnabled != rt.permState.AIAutoEnabled
	rt.permState = &next
	if !switched {
		return false
	}
	rt.permEpoch++
	if next.AIAutoEnabled {
		rt.judgedPerms = map[string]struct{}{}
		return true
	}
	return false
}

// permStateSnapshot 原子读取 runtime 权限状态（rt.mu 内拷贝指针；实例不可变，
// 锁外读取安全）。无 runtime 或状态未初始化返回 nil（调用方读 DB 行）。
func (m *Manager) permStateSnapshot(taskID string) *RuntimePermissionState {
	rt := m.getRuntime(taskID)
	if rt == nil {
		return nil
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.permState == nil {
		return nil
	}
	state := *rt.permState
	return &state
}

// derivePermissionModeView 有效模式推导单一函数（task-permission-mode D6/D8：详情
// GET/详情 SSE/PATCH 响应/通知快照统一复用，禁止各自实现）。输入为 RuntimePermissionState
// 原子读取结果或 nil（nil=无 runtime，读 DB 持久化行 dbRow）；推导规则（D5 转换矩阵）：
// StartedWithAuto→all-approve（DR1，--auto 进程保存 ai-auto 有效仍 all-approve）；
// 未带 --auto 且 SavedMode==all-approve→ask（保存 all-approve 不改写启动事实，
// 直至下次激活）；否则 SavedMode。
func derivePermissionModeView(state *RuntimePermissionState, dbRow TaskRow) (application.PermissionModeView, error) {
	if state == nil {
		mode, err := resolvePermissionMode(dbRow)
		if err != nil {
			return application.PermissionModeView{}, err
		}
		return application.PermissionModeView{PermissionMode: mode, EffectivePermissionMode: mode}, nil
	}
	effective := state.SavedMode
	switch {
	case state.StartedWithAuto:
		effective = PermissionModeAllApprove
	case state.SavedMode == PermissionModeAllApprove:
		effective = PermissionModeAsk
	}
	return application.PermissionModeView{PermissionMode: state.SavedMode, EffectivePermissionMode: effective}, nil
}

// PermissionModeView 返回任务权限模式视图（D8，签名钉死）：有 runtime 时在同一 rt.mu
// 同步域原子读取 RuntimePermissionState；无 runtime 时读 DB 持久化行。
func (m *Manager) PermissionModeView(ctx context.Context, taskID string) (application.PermissionModeView, error) {
	if state := m.permStateSnapshot(taskID); state != nil {
		return derivePermissionModeView(state, TaskRow{})
	}
	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return application.PermissionModeView{}, err
	}
	return derivePermissionModeView(nil, row)
}

// BackfillPermissionModeAtStart 进程启动回填入口（D8）：存量 active 且启动事实为 NULL
// 的行按持久化 permission_mode 回填；错误原样传播（调用方 MUST 据此阻止 HTTP 开放）。
func (m *Manager) BackfillPermissionModeAtStart(ctx context.Context) error {
	return m.store.BackfillPermissionModeAtStart(ctx)
}

// UpdateTaskPermissionMode 模式保存唯一协调器（task-permission-mode D4/tasks 4.2，
// 签名钉死）。严格序列：校验请求值（共享三值校验，trim 后非法 → invalid_input）→
// per-task 互斥（与激活/挂起等同一把，竞争 → conflict）→ 读任务行（不存在 →
// not_found；其他读错误 → internal 且 MUST NOT 包装成 404）→ 同值 → 当前视图零副作用
// 200（不收敛、不发事件）→ DB 单列 UPDATE 提交（PONR；提交失败 → internal 零 runtime
// 副作用；未命中 → not_found）→ rt.mu 收敛（convergePermissionModeSave，仅真实有效
// ai-auto 行为切换动 epoch；needScan 在锁释放后补判）→ 发布 task.activity_changed →
// 存在 runtime 时追加发布 serve_runtime.permission_mode_changed（D7：收敛完成之后）。
// PONR：DB 提交成功后请求取消/中断 MUST NOT 阻断收敛与发布——本阶段全部改用
// Manager 生命周期 ctx（judgeScan 自派生 judgeCtx，最终视图读取用 lifeCtx）。
// 同秒真实变更 res.Changed 仍为 true（数值不变语义由 store 保证）；并发窗口内他人已
// 写入同值（Matched+!Changed）按同值零副作用收敛。
func (m *Manager) UpdateTaskPermissionMode(ctx context.Context, taskID, mode string) (application.PermissionModeView, error) {
	normalized, nerr := normalizePermissionModeInput(mode)
	if nerr != nil {
		return application.PermissionModeView{}, newOpErr(codeInvalidInput, nerr)
	}
	unlock, lerr := m.tryLockTask(taskID)
	if lerr != nil {
		return application.PermissionModeView{}, lerr // conflict（已包 opErr）
	}

	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		unlock()
		if errors.Is(err, sql.ErrNoRows) {
			return application.PermissionModeView{}, newOpErr(codeNotFound, err)
		}
		return application.PermissionModeView{}, newOpErr(codeInternal, fmt.Errorf("read task: %w", err))
	}
	saved, perr := resolvePermissionMode(row)
	if perr != nil {
		unlock()
		// 持久化旧值损坏 fail-closed（D4 失败矩阵），internal 而非 404/422。
		return application.PermissionModeView{}, newOpErr(codeInternal, perr)
	}
	if normalized == saved {
		// 同值保存零副作用：不推进 updated_at、不收敛、不发事件，返回当前视图。
		unlock()
		return m.currentPermissionModeView(taskID, row)
	}

	res, err := m.store.UpdateTaskPermissionMode(ctx, taskID, normalized)
	if err != nil {
		unlock()
		return application.PermissionModeView{}, newOpErr(codeInternal, fmt.Errorf("commit permission_mode: %w", err))
	}
	unlock()
	if !res.Matched {
		return application.PermissionModeView{}, newOpErr(codeNotFound, fmt.Errorf("task %s not found", taskID))
	}
	if !res.Changed {
		// 并发窗口内他人已写入同值：净效果同值，零副作用（D4 同值语义）。
		return m.currentPermissionModeView(taskID, row)
	}

	// PONR 之后的收敛与发布随 Manager 生命周期执行，不随请求 ctx 取消。
	needScan := m.convergePermissionModeSave(taskID, normalized)
	if needScan {
		if rt := m.getRuntime(taskID); rt != nil {
			m.judgeScan(rt) // converge 返回即已释放 rt.mu（锁边界，D5）
		}
	}
	if m.publish != nil {
		m.publish.Publish(ocdeckevent.NewTaskActivityChanged(taskID))
		if inst := m.currentRuntimeInstVersion(taskID); inst != "" {
			m.publish.Publish(ocdeckevent.NewServeRuntimePermissionModeChanged(string(inst), taskID))
		}
	}
	return m.PermissionModeView(m.lifecycleCtx(), taskID)
}

// currentPermissionModeView 以已读任务行 + runtime 状态快照推导当前视图（同值路径
// 专用，避免第三次 store 读）。
func (m *Manager) currentPermissionModeView(taskID string, row TaskRow) (application.PermissionModeView, error) {
	view, err := derivePermissionModeView(m.permStateSnapshot(taskID), row)
	if err != nil {
		return application.PermissionModeView{}, newOpErr(codeInternal, err)
	}
	return view, nil
}

// currentRuntimeInstVersion 原子读取当前 runtime 实例令牌；无 runtime 返回空串
// （D7：无 runtime MUST NOT 发布 mode-changed）。
func (m *Manager) currentRuntimeInstVersion(taskID string) runtime.InstVersion {
	rt := m.getRuntime(taskID)
	if rt == nil {
		return ""
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.instVersion
}
