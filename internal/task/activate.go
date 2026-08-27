package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"ocdeck/internal/application/runtime"
	apptask "ocdeck/internal/application/task"
	ocdecksess "ocdeck/internal/domain/session"
	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
	"ocdeck/internal/infrastructure/store"
)

// envBaselineKeys 是 design.md §2 规定的最小基础集 env key（不含 OCDECK_*/密码）。
// TERM 不在透传列：必须强制为 terminfo 认识的 xterm-256color（见 mergeEnvSnapshot），
// 宿主 TERM（如 xterm-ghostty）会导致 tmux 报 "missing or unsuitable terminal"。
var envBaselineKeys = []string{
	"COLORTERM", "HOME", "USER", "PATH", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "TMPDIR", "SSH_AUTH_SOCK",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
}

// envReservedKeys 是 design.md §2 规定的内部/生命周期变量，不可被用户 env 覆盖，
// 也不进持久化快照与 shell env（内部变量仅注入 serve/TUI）。OPENCODE_SERVER_PASSWORD
// 为 role-specific secret，MUST NOT 进入持久化快照与 shell env。
//
// env-management spec 要求注入五个生命周期变量：OCDECK_TASK_ID、OCDECK_TASK_NAME、
// OCDECK_TASK_PATH、OCDECK_PROJECT_PATH、OCDECK_SERVE_PORT。保留命名空间覆盖全部
// OCDECK_* 前缀（用户 env 注入任何 OCDECK_* 均被忽略并提示，不只个别 key）。
var envReservedKeys = map[string]bool{
	"OPENCODE_SERVER_PASSWORD": true,
	"OCDECK_SERVE_PORT":        true,
	"OCDECK_TASK_ID":           true,
	"OCDECK_TASK_NAME":         true,
	"OCDECK_TASK_PATH":         true,
	"OCDECK_PROJECT_PATH":      true,
}

// ocdeckEnvNamespace 为生命周期变量保留前缀（design.md §2：用户 env 注入任何 OCDECK_*
// 均被忽略并提示，保留命名空间覆盖全部前缀，不只个别 key）。
const ocdeckEnvNamespace = "OCDECK_"

// isReservedEnvKey 判断 key 是否为内部/生命周期保留变量（用户 env 不得覆盖）。
// 保留命名空间覆盖全部 OCDECK_* 前缀 + 显式枚举的内部 secret（OPENCODE_SERVER_PASSWORD）。
func isReservedEnvKey(k string) bool {
	if envReservedKeys[k] {
		return true
	}
	return strings.HasPrefix(k, ocdeckEnvNamespace)
}

// envSnapshot 是持久化到 tasks.env_snapshot 的 env 合并快照（design.md §2）。
// 内容 = 基础集 + 项目级 + 任务级 + 通用生命周期变量(OCDECK_*)；
// MUST NOT 含进程类型内部变量（OPENCODE_SERVER_PASSWORD 等 role-specific secret）。
type envSnapshot struct {
	Vars map[string]string `json:"vars"`
}

// mergeEnvSnapshot 合并 env 快照并持久化（design.md §2 合并优先级）。
// 基础集 < 全局级 < 项目级 < 任务级 < 生命周期变量(OCDECK_*) < 内部变量（内部变量不进快照）。
// 全局级 manual → 存值；follow_host → 从服务端进程 env 解析，宿主未设置/空则跳过该变量。
// 返回合并后的 env map（含生命周期变量，不含密码），供 serve/tui/shell 注入时叠加密码。
func (m *Manager) mergeEnvSnapshot(ctx context.Context, row TaskRow, port int) (map[string]string, error) {
	merged, err := m.layerEnvSnapshot(ctx, row)
	if err != nil {
		return nil, err
	}
	merged["OCDECK_SERVE_PORT"] = strconv.Itoa(port)
	// 持久化快照（不含密码）。
	if err := m.persistEnvSnapshot(ctx, row.ID, merged); err != nil {
		return nil, &persistEnvSnapshotError{err: err}
	}
	return merged, nil
}

// layerEnvSnapshot 合并 env 层叠快照（design.md §2 合并优先级 + §7.3 抽取点）。
// 基础集 < 全局级 < 项目级 < 任务级 < 生命周期变量(OCDECK_*，不含 OCDECK_SERVE_PORT，由调用方按场景注入)。
// 不含 port 参数与持久化——供 serve/tui/shell 与脚本执行复用同一层叠逻辑。
// 行为不变量：serve/tui/shell 的 env 内容与顺序与抽取前完全一致（既有测试全绿）。
func (m *Manager) layerEnvSnapshot(ctx context.Context, row TaskRow) (map[string]string, error) {
	merged := map[string]string{}
	// TERM 强制为 terminfo 认识的规范值（xterm.js 客户端即 xterm-256color），
	// MUST NOT 继承宿主 TERM（如 xterm-ghostty 会致 tmux "missing or unsuitable terminal"）。
	merged["TERM"] = "xterm-256color"
	// 基础集：从宿主环境取（ocdeck 不继承全部宿主 env，仅最小基础集）。
	for _, k := range envBaselineKeys {
		if v, ok := hostEnv(k); ok && v != "" {
			merged[k] = v
		}
	}
	// Locale 兜底（design.md D0）：LANG/LC_ALL/LC_CTYPE 三者均未设置或为空串时注入默认
	// LANG=en_US.UTF-8，保证 serve/TUI/shell 会话进程拿到 UTF-8 locale（shell 内
	// locale 敏感的 CLI 依赖它）。任一高位变量已设置非空值则原样透传（已在上方基础集循环），
	// 不覆盖、不纠正；空串视为未设置，不抑制注入。
	langVal, _ := hostEnv("LANG")
	lcAllVal, _ := hostEnv("LC_ALL")
	lcCtypeVal, _ := hostEnv("LC_CTYPE")
	if langVal == "" && lcAllVal == "" && lcCtypeVal == "" {
		merged["LANG"] = "en_US.UTF-8"
	}
	// 全局级 env（design.md §2：基础集 < 全局级 < 项目级 < 任务级）。
	// manual → 存值；follow_host → 从服务端进程 env 解析，宿主未设置/空则跳过该变量（不注入空值）。
	// reserved key 与项目/任务级一致：用户值忽略并跳过（不进快照、不覆盖内部变量）。
	globalVars, err := m.store.ListGlobalEnvVars(ctx)
	if err != nil {
		return nil, fmt.Errorf("list global env: %w", err)
	}
	for _, e := range globalVars {
		if isReservedEnvKey(e.Key) {
			continue
		}
		switch e.Mode {
		case "manual":
			merged[e.Key] = e.Value
		case "follow_host":
			if v, ok := hostEnv(e.Key); ok && v != "" {
				merged[e.Key] = v
			}
			// 宿主未设置/空 → 跳过该变量（不注入空值，design.md §2）。
		}
	}
	// 项目级 env。
	projVars, err := m.store.ListProjectEnvVars(ctx, row.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("list project env: %w", err)
	}
	for _, e := range projVars {
		if isReservedEnvKey(e.Key) {
			// B7a：内部/生命周期变量不可被用户 env 覆盖，不进持久化快照与 shell env。
			continue
		}
		merged[e.Key] = e.Value
	}
	// 任务级 env（覆盖项目级）。
	taskVars, err := m.store.ListTaskEnvVars(ctx, row.ID)
	if err != nil {
		return nil, fmt.Errorf("list task env: %w", err)
	}
	for _, e := range taskVars {
		if isReservedEnvKey(e.Key) {
			continue
		}
		merged[e.Key] = e.Value
	}
	// 生命周期变量 OCDECK_*（env-management spec：注入 OCDECK_TASK_ID、OCDECK_TASK_NAME、
	// OCDECK_TASK_PATH、OCDECK_PROJECT_PATH；OCDECK_SERVE_PORT 由调用方按场景注入）。
	proj, perr := m.store.GetProject(ctx, row.ProjectID)
	if perr != nil {
		return nil, fmt.Errorf("get project for lifecycle env: %w", perr)
	}
	merged["OCDECK_TASK_ID"] = row.ID
	merged["OCDECK_TASK_NAME"] = row.Name
	merged["OCDECK_TASK_PATH"] = row.WorktreePath
	merged["OCDECK_PROJECT_PATH"] = proj.Path
	return merged, nil
}

// loadEnvSnapshot 从 DB 读回持久化的 env 快照（persist 恢复用，design.md §2）。
func (m *Manager) loadEnvSnapshot(row TaskRow) (map[string]string, error) {
	if !row.EnvSnapshot.Valid || row.EnvSnapshot.String == "" {
		return nil, fmt.Errorf("env snapshot missing for task %s", row.ID)
	}
	var snap envSnapshot
	if err := json.Unmarshal([]byte(row.EnvSnapshot.String), &snap); err != nil {
		return nil, fmt.Errorf("unmarshal env snapshot: %w", err)
	}
	return snap.Vars, nil
}

// allocatePort 分配 serve 端口（design.md §3：先试 last_port，被占则范围内轮转）。
// exclude：必须跳过的端口（0 = 不排除）；lastPort 快路径与轮转扫描均跳过。
// 返回可用端口；范围耗尽返回错误。轮转游标避免每次从头扫（B5）。
// portCursor 记录上次分配位置，下次从此处后轮转，降低并行 Activate 选同端口概率。
func (m *Manager) allocatePort(lastPort sql.NullInt64, exclude int) (int, error) {
	pr := m.cfg.ServePortRange
	// 先试 last_port（跳过 exclude）。
	if lastPort.Valid {
		p := int(lastPort.Int64)
		if p != exclude && p >= pr.Min && p <= pr.Max && isPortFree(p) {
			return p, nil
		}
	}
	// 轮转起点：从上次分配位置后一位开始扫描（避免每次从头，降低并行冲突）。
	m.portCursorMu.Lock()
	start := m.portCursor + 1
	if start < pr.Min || start > pr.Max {
		start = pr.Min
	}
	m.portCursorMu.Unlock()
	// 线性扫描 [start, Max] + [Min, start)。
	for off := 0; off <= (pr.Max - pr.Min); off++ {
		p := start + off
		if p > pr.Max {
			p -= (pr.Max - pr.Min + 1)
		}
		if p == exclude {
			continue
		}
		if isPortFree(p) {
			m.portCursorMu.Lock()
			m.portCursor = p
			m.portCursorMu.Unlock()
			return p, nil
		}
	}
	return 0, fmt.Errorf("serve port range %d-%d exhausted", pr.Min, pr.Max)
}

// portAllocationError 标记端口范围耗尽（G3-5：Recovery 分派为终态补偿，
// cause=分配错误；Activate 沿用 OpError code 映射，行为不变）。
type portAllocationError struct{ err error }

func (e *portAllocationError) Error() string { return e.err.Error() }
func (e *portAllocationError) Unwrap() error { return e.err }

// isPortFree 探测端口是否可用（bind 后立即释放）。
func isPortFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// Activate 激活任务（single-process-opencode D1/D5）。
// 前置检查 → 置 activating → 分配端口+合并 env → NewSession(runtime) → 健康检查+Probe（ready）
// → 锚定 bootstrap → token/group/watcher → SSE+对齐 → last_port → CAS active。
func (m *Manager) Activate(ctx context.Context, taskID string) error {
	unlock, err := m.tryLockTask(taskID)
	if err != nil {
		return err
	}
	defer unlock()

	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
	}
	// 前置检查：状态须为 suspended。
	if row.Status != StatusSuspended {
		return newOpErr(codeInvalidState, fmt.Errorf("activate requires suspended, got %s", row.Status))
	}
	// add-plain-dir-project D8：早期 kind 门禁——在任何状态修改/副作用前解析并校验项目 kind，
	// 未知值零副作用报错（MUST NOT 在 serve 启动后才发现未知 kind）。mode 显式传入后续 startSSE/alignSessions。
	proj, err := m.store.GetProject(ctx, row.ProjectID)
	if err != nil {
		return newOpErr(codeNotFound, fmt.Errorf("project gone: %w", err))
	}
	mode, err := alignModeForKind(proj.Kind)
	if err != nil {
		// 未知持久化 kind（DB 损坏值）→ internal（D1：区别于用户请求非法 kind 的 invalid_input）。
		return newOpErr(codeInternal, err)
	}
	// 前置检查：无未清理的旧代残留会话（tmux ls 中仍存在该任务会话则拒绝）。
	if err := m.checkNoResidualSessions(ctx, taskID); err != nil {
		return err
	}
	// 前置检查（G3-16 ABA 防护，首选方案）：任务存在进行中的 recovery incident 时
	// 拒绝激活。消除「旧 incident 被 Complete 唤醒 → 新 Activate 在 runtime 发布前
	// 重写 activating → 旧 incident 复核见 runtime=nil+activating 误判可继续」的
	// 重叠窗口——incident 注销（退出路径 defer）后 Activate 即恢复放行；incident
	// 退出是毫秒-秒级，用户重试即可。相比 activation/recovery epoch 显式代数，
	// 本方案零新增状态、单一拒绝点，简单可靠。
	if m.hasActiveRecoveryIncident(taskID) {
		return newOpErr(codeConflict, fmt.Errorf("task %s recovery in progress; retry after it settles", taskID))
	}
	// 前置检查：无未清理的 cleanup debt（任意 retryable=true notice 拒绝激活）。
	if hasRetryable, err := m.hasRetryableNotice(ctx, row); err != nil {
		return newOpErr(codeInternal, err)
	} else if hasRetryable {
		return newOpErr(codeConflict, fmt.Errorf("task %s has uncleaned cleanup debt; resolve or force-delete first", taskID))
	}

	// init_status 门禁（design.md §5，tasks 3.5）：none|succeeded → 放行；
	// pending|running → invalid_state "init in progress"；failed → invalid_state 含 init_error；
	// 未知/空值 → invalid_state fail-closed。
	//
	// guard 委托 domain/task.CanActivate（design D0 P1.4.2 strangler 第二步）：本处仅委托
	// init_status 五分支决策——status 已在上游校验为 suspended、notice 已在 hasRetryableNotice
	// 校验为无阻断，故 rehydrateGuardView(row) 构造的 domain 视图 notices 为空，
	// CanActivate 仅由 init_status 决定（fail-closed on 未知值）。
	// 委托前后行为 byte-equivalent：guard 拒绝时按现状分支模板生成错误消息。
	if !rehydrateGuardView(row).CanActivate() {
		switch row.InitStatus {
		case InitStatusPending, InitStatusRunning:
			return newOpErr(codeInvalidState, fmt.Errorf("task %s init in progress (init_status=%s)", taskID, row.InitStatus))
		case InitStatusFailed:
			msg := fmt.Sprintf("init failed: %s", row.InitError.String)
			if !row.InitError.Valid || row.InitError.String == "" {
				msg = "init failed"
			}
			msg += "；修复脚本后 Re-run"
			return newOpErr(codeInvalidState, errors.New(msg))
		default:
			return newOpErr(codeInvalidState, fmt.Errorf("task %s unknown init_status %q", taskID, row.InitStatus))
		}
	}

	// ① 置 activating。G3-18：激活准入原子拒绝未清 recovery debt（Complete 成功
	// 但删除失败遗留 / CAS mismatch 留存的旧 intent 不得被重放误伤本次激活）。
	updated, err := m.beginActivation(ctx, taskID, StatusSuspended)
	if err != nil {
		if errors.Is(err, store.ErrRecoveryDebtPresent) {
			return newOpErr(codeConflict, fmt.Errorf("task %s has uncleaned recovery debt; resolve (replay) before activate", taskID))
		}
		return newOpErr(codeInternal, err)
	}
	if !updated.Matched {
		return newOpErr(codeConflict, fmt.Errorf("task %s state changed before activate commit", taskID))
	}

	if err := m.activateRun(ctx, taskID, mode); err != nil {
		var handled *errActivateCommitHandled
		if errors.As(err, &handled) {
			m.replayHandledCleanup(ctx, taskID, err)
			return handled.err
		}
		// 失败：清理已建会话（runtime/shell）→ suspended + last_error（design.md §19 补偿）。
		// 补偿 MUST 用脱离调用方取消的 context：activateRun 可能在 probe 退避/健康检查等
		// 步骤因 ctx 取消返回，此时调用方 ctx 已取消，真实 store 用 ExecContext 会因 ctx 取消
		// 失败 → 清快照/状态回退/notice 持久化全部失败且错误被忽略 → 任务卡 activating。
		// 故补偿路径统一用 compCtx（WithoutCancel + 独立短超时），各步错误记录日志但不跳过后续步骤。
		m.runActivateFailureCompensation(ctx, taskID, err)
		return err
	}
	return nil
}

// errActivateCommitHandled 表示 CAS mismatch 反向清理已按本 attempt token 做完，
// MUST NOT 再走通用补偿（禁写 status/last_error/env_snapshot/anchor）。
type errActivateCommitHandled struct{ err error }

func (e *errActivateCommitHandled) Error() string { return e.err.Error() }
func (e *errActivateCommitHandled) Unwrap() error { return e.err }

func (m *Manager) replayHandledCleanup(reqCtx context.Context, taskID string, err error) {
	pendings := foldPendingCleanups(err, nil)
	if len(pendings) == 0 {
		return
	}
	replayCtx, cancel := context.WithTimeout(context.WithoutCancel(reqCtx), activateCompensationFinalizeTimeout)
	defer cancel()
	if rerr := m.replayPendingCleanups(replayCtx, taskID, pendings); rerr != nil {
		log.Printf("activate: CAS mismatch pending replay for task %s: %v", taskID, rerr)
	}
}

// activateCompensationTimeout 是 cleanup 阶段（kill 已建会话、枚举 shell、记 residual notice）
// 所用 context 的独立短超时。补偿 MUST 脱离调用方 ctx 的取消：调用方 ctx 可能已取消
// （probe 退避取消/健康检查超时），真实 store 用 ExecContext 会因 ctx 取消失败导致任务卡 activating。
// 用 context.WithoutCancel + 此超时保证 cleanup 有界完成。cleanup 内部 proc 调用不接收 compCtx、
// 各自用 context.Background() 超时（单次 KillSession 可达 30s，多会话串行更久），故此预算仅约束
// cleanup 内经 compCtx 的 store 写（notice 持久化）；超 30s 后 cleanup 内未完成的 store 写被取消，
// 但不影响后续 DB 收敛（见 activateCompensationFinalizeTimeout）。超时选 30s：与单次 KillSession 上限相当。
const activateCompensationTimeout = 30 * time.Second

// activateCompensationFinalizeTimeout 是 cleanup 结束后「最终 DB 收敛」的独立短超时
// （清 env 快照 + 状态回退 activating→suspended + last_error）。MUST 在 cleanup 结束后另起新 bounded
// context：无论 cleanup 耗时/成败，状态回退总有全新预算，避免 cleanup 耗尽 30s 预算后清快照/CAS
// 拿到 deadline exceeded 的 compCtx 导致任务卡 activating。超时选 10s：两次 DB 写足够。
const activateCompensationFinalizeTimeout = 10 * time.Second

// runActivateFailureCompensation 执行 Activate 失败的统一补偿（design.md §19 补偿）：
// kill 已建会话 → pending cleanup 回放 → 清 env 快照 → 状态回退 activating→suspended + last_error。
//
// 补偿用脱离调用方取消的 compCtx（WithoutCancel + activateCompensationTimeout），
// 避免调用方 ctx 已取消时真实 store 的 ExecContext 失败导致任务卡 activating。
// 预算拆分：cleanup 用 compCtx；回放用新 detached 有界 ctx（activateCompensationFinalizeTimeout）；
// 最终 DB 收敛另起新 bounded context，保证无论 cleanup 耗时/成败，状态回退总有全新预算。
// 各步错误 MUST 记录日志但不跳过后续补偿步骤（不静默吞错，但保证补偿尽量收敛）。
// last_error 聚合原始错误与 cleanup/回放 notice 错误（design.md §8）；MUST NOT 改变 Activate 返回错误。
func (m *Manager) runActivateFailureCompensation(reqCtx context.Context, taskID string, cause error) {
	compCtx, cancel := context.WithTimeout(context.WithoutCancel(reqCtx), activateCompensationTimeout)
	defer cancel()

	cleanupErr, observations := m.cleanupActivationRuntimeCollect(compCtx, taskID)
	if cleanupErr != nil {
		log.Printf("activate: cleanup runtime for task %s: %v", taskID, cleanupErr)
	}

	// 回放：cleanup 结束后、最终 DB 收敛前；单个新 detached 有界 ctx（非逐 pending、不复用 compCtx）。
	var replayErr error
	if pendings := foldPendingCleanups(cause, observations); len(pendings) > 0 {
		replayCtx, replayCancel := context.WithTimeout(context.WithoutCancel(reqCtx), activateCompensationFinalizeTimeout)
		replayErr = m.replayPendingCleanups(replayCtx, taskID, pendings)
		replayCancel()
		if replayErr != nil {
			log.Printf("activate: replay pending cleanup for task %s: %v", taskID, replayErr)
		}
	}

	// 最终 DB 收敛：另起全新 bounded context，不受 cleanup/回放耗时影响。
	finalCtx, finalCancel := context.WithTimeout(context.WithoutCancel(reqCtx), activateCompensationFinalizeTimeout)
	defer finalCancel()

	if _, err := m.writeEnvSnapshot(finalCtx, taskID, sql.NullString{}); err != nil {
		log.Printf("activate: clear env snapshot for task %s: %v", taskID, err)
	}
	le := sql.NullString{String: cause.Error(), Valid: true}
	agg := cause
	if cleanupErr != nil {
		agg = fmt.Errorf("%w; cleanup notice: %v", agg, cleanupErr)
	}
	if replayErr != nil {
		agg = fmt.Errorf("%w; replay notice: %v", agg, replayErr)
	}
	if agg != cause {
		le = sql.NullString{String: agg.Error(), Valid: true}
	}
	updated, err := m.writeStatusConditional(finalCtx, taskID, StatusActivating, StatusSuspended, le)
	if err != nil {
		log.Printf("activate: rollback activating→suspended for task %s: %v", taskID, err)
		return
	}
	if !updated.Matched {
		// 无 error 但 updated=false：状态已被并发改动（非 activating），便于诊断卡 activating 场景。
		log.Printf("activate: rollback activating→suspended for task %s: status changed concurrently (not activating)", taskID)
	}
}

// foldPendingCleanups 将 cause 路径 pending + cleanup observations 按发生顺序 fold：
// 同会话 reason/retryable 取最新 observation；tickets = 全部 persisted=false 的 union；
// 仅当同会话存在 persisted=false 时产生回放项；顺序 = 各会话首次出现顺序。
func foldPendingCleanups(cause error, observations []cleanupObservation) []pendingCleanup {
	type foldState struct {
		sessionName    string
		reason         string
		retryable      bool
		tickets        []string
		hasUnpersisted bool
	}
	byName := map[string]*foldState{}
	var order []string

	apply := func(name, reason string, retryable bool, tickets []string, persisted bool) {
		st, ok := byName[name]
		if !ok {
			st = &foldState{sessionName: name}
			byName[name] = st
			order = append(order, name)
		}
		// reason/retryable 始终取最新 observation（含已落库的更新终态）。
		st.reason = reason
		st.retryable = retryable
		if !persisted {
			st.hasUnpersisted = true
			st.tickets = unionTickets(st.tickets, tickets)
		}
	}

	// cause 路径 pending：按 errors.As 检出（发生在 cleanup observations 之前）。
	var pce *pendingCleanupError
	if errors.As(cause, &pce) && pce != nil {
		apply(pce.pending.sessionName, pce.pending.reason, pce.pending.retryable, pce.pending.cleanupTickets, false)
	}
	for _, obs := range observations {
		apply(obs.sessionName, obs.reason, obs.retryable, obs.cleanupTickets, obs.persisted)
	}

	var out []pendingCleanup
	for _, name := range order {
		st := byName[name]
		if !st.hasUnpersisted {
			continue
		}
		out = append(out, pendingCleanup{
			sessionName:    st.sessionName,
			cleanupTickets: st.tickets,
			reason:         st.reason,
			retryable:      st.retryable,
		})
	}
	return out
}

// replayPendingCleanups 对归并后的 pending 每项恰好一次 recordResidualNotice。
func (m *Manager) replayPendingCleanups(ctx context.Context, taskID string, pendings []pendingCleanup) error {
	var errs []error
	for _, p := range pendings {
		if err := m.recordResidualNotice(ctx, taskID, p.sessionName, p.cleanupTickets, p.reason, p.retryable); err != nil {
			errs = append(errs, fmt.Errorf("replay notice %s: %w", p.sessionName, err))
		}
	}
	return errors.Join(errs...)
}

// activateRun 执行激活的外部副作用序列（runtime → probe/ready → D5 bootstrap → SSE → last_port）。
// mode 由 Activate 早期 kind 门禁解析后传入（add-plain-dir-project D8）。
func (m *Manager) activateRun(ctx context.Context, taskID string, mode AlignMode) error {
	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}

	port, err := m.allocatePort(row.LastPort, 0)
	if err != nil {
		return &portAllocationError{err: newOpErr(codeConflict, err)}
	}
	env, err := m.mergeEnvSnapshot(ctx, row, port)
	if err != nil {
		return newOpErr(codeInternal, err)
	}

	runtimeName := runtimeSessionName(taskID)
	password := newRandomPassword()
	port, password, err = m.bootstrapRuntime(ctx, row, runtimeName, port, password, env, nil, false)
	if err != nil {
		return err
	}
	return m.commitRuntimeReady(ctx, taskID, row.WorktreePath, runtimeName, port, password, mode, nil)
}

// commitRuntimeReady 成功提交序列（D3/Phase 2）：token/group/watcher → SSE+align → final health → last_port → CAS active。
// onRegister（仅 Recovery 传入，G3-19）：runtime 发布前回调，把本 attempt 的新
// token 显式绑定到发起 incident（不经 setRuntime 通用挂钩，避免误绑新代 token）。
func (m *Manager) commitRuntimeReady(ctx context.Context, taskID, wtPath, runtimeName string, port int, password string, mode AlignMode, onRegister func(rt *taskRuntime)) error {
	if alive, _ := m.proc.HasSession(runtimeName); !alive {
		_, _ = m.proc.KillSession(runtimeName)
		return newOpErr(codeProcessError, fmt.Errorf("runtime session gone before runtime register"))
	}
	rt := m.newRuntime(taskID)
	if onRegister != nil {
		onRegister(rt)
	}
	m.setRuntime(taskID, rt)
	rt.registerGroup(roleRuntime, runtimeName)
	m.watchServeExit(taskID, runtimeName)
	if err := m.startSSE(ctx, rt, taskID, wtPath, port, password, mode); err != nil {
		return newOpErr(codeProcessError, fmt.Errorf("sse subscribe: %w", err))
	}
	oc := m.ocFactory(port, password, opencode.Options{HealthTimeout: 2 * time.Second, OpTimeout: 10 * time.Second})
	if _, herr := oc.Health(ctx); herr != nil {
		return newOpErr(codeProcessError, fmt.Errorf("final health: %w", herr))
	}
	if _, err := m.writeLastPort(ctx, taskID, port); err != nil {
		return newOpErr(codeInternal, &lastPortWriteError{err: fmt.Errorf("write last port: %w", err)})
	}
	// G3-1：CAS 前排空本 attempt runtime 的 SSE fatal——fatal 与 CAS 在同一同步域分派，
	// 杜绝「CAS 提交无 SSE 的 active」窗口（Recovery 无 incident 时此处恒为 nil，Activate 不受影响）。
	if fatalErr := m.incidentFatalFor(taskID, rt.instVersion); fatalErr != nil {
		return newOpErr(codeProcessError, fmt.Errorf("sse fatal before commit: %w", fatalErr))
	}
	cas, err := m.writeStatusConditional(ctx, taskID, StatusActivating, StatusActive, sql.NullString{})
	if err != nil {
		return newOpErr(codeInternal, fmt.Errorf("commit active: %w", err))
	}
	if cas.Matched {
		// G3-1：CAS 后 fatal → 已提交 active，经幂等 ensureRecovery 立即开新 incident
		//（same-token 状态/锁校验保证不产生并发双恢复）。
		if fatalErr := m.incidentFatalFor(taskID, rt.instVersion); fatalErr != nil {
			go m.ensureRecovery(taskID, rt.instVersion)
		}
		return nil
	}
	fresh, rerr := m.store.GetTask(ctx, taskID)
	if rerr != nil {
		rbErr := m.rollbackAttemptRuntime(ctx, taskID, runtimeName, rt.instVersion)
		cause := newOpErr(codeInternal, fmt.Errorf("commit active reread: %w", rerr))
		return wrapHandledRollback(cause, rbErr)
	}
	if fresh.Status == StatusActive {
		if cur := m.getRuntime(taskID); cur != nil && cur.instVersion == rt.instVersion {
			return nil
		}
	}
	rbErr := m.rollbackAttemptRuntime(ctx, taskID, runtimeName, rt.instVersion)
	cause := newOpErr(codeConflict, fmt.Errorf("task %s state changed before activate commit", taskID))
	return wrapHandledRollback(cause, rbErr)
}

func wrapHandledRollback(cause, rbErr error) error {
	if rbErr == nil {
		return &errActivateCommitHandled{err: cause}
	}
	var pce *pendingCleanupError
	if errors.As(rbErr, &pce) {
		return &errActivateCommitHandled{err: rbErr}
	}
	return &errActivateCommitHandled{err: fmt.Errorf("%w; rollback: %v", cause, rbErr)}
}

func (m *Manager) rollbackAttemptRuntime(ctx context.Context, taskID, runtimeName string, tok runtime.InstVersion) error {
	cur := m.getRuntime(taskID)
	if cur == nil || cur.instVersion != tok {
		return nil
	}
	cur.stopAllJoin()
	m.rtMu.Lock()
	replaced := m.runtimes[taskID] != nil && m.runtimes[taskID] != cur
	if m.runtimes[taskID] == cur {
		delete(m.runtimes, taskID)
	}
	m.rtMu.Unlock()
	if replaced {
		return nil
	}

	nctx, ncancel := withResidualNoticeCtx(ctx)
	defer ncancel()
	exists, herr := m.proc.HasSession(runtimeName)
	if herr != nil && !errors.Is(herr, process.ErrNoTmuxServer) {
		return m.rollbackNotice(nctx, taskID, runtimeName, nil, noticeReasonKillFailed, true, herr)
	}
	if !exists {
		return nil
	}
	if cur := m.getRuntime(taskID); cur != nil && cur.instVersion != tok {
		return nil
	}
	res, kerr := m.proc.KillSession(runtimeName)
	if kerr != nil {
		return m.rollbackNotice(nctx, taskID, runtimeName, res.CleanupTickets, noticeReasonKillFailed, true, kerr)
	}
	cls := classifyKillResult(res)
	if cls.action == "none" {
		return nil
	}
	return m.rollbackNotice(nctx, taskID, runtimeName, res.CleanupTickets, cls.reason, cls.retryable, fmt.Errorf("rollback disposition %s", res.Disposition))
}

func (m *Manager) rollbackNotice(ctx context.Context, taskID, sessionName string, tickets []string, reason string, retryable bool, cause error) error {
	nerr := m.recordResidualNotice(ctx, taskID, sessionName, tickets, reason, retryable)
	if nerr == nil {
		// G3-2/G3-5：notice 已落库的 retryable/infra 回退失败 → typed 终态
		//（Recovery MUST NOT 再创建进程；Activate 补偿行为不变）。
		return &retryableCleanupError{err: cause}
	}
	if perr := m.persistOrphanDebt(ctx, sessionName, tickets); perr != nil {
		log.Printf("activate: persist rollback debt %s: %v", sessionName, perr)
	}
	return recordNoticeOrPending(sessionName, tickets, reason, retryable, nerr, cause)
}

// 生产默认健康轮询预算（design.md：500ms 间隔、10s deadline）。
const (
	defaultServeReadyTimeout      = 10 * time.Second
	defaultServeReadyPollInterval = 500 * time.Millisecond
)

func (m *Manager) serveReadyWaitTimeout() time.Duration {
	if m != nil && m.serveReadyTimeout > 0 {
		return m.serveReadyTimeout
	}
	return defaultServeReadyTimeout
}

func (m *Manager) serveReadyWaitPollInterval() time.Duration {
	if m != nil && m.serveReadyPollInterval > 0 {
		return m.serveReadyPollInterval
	}
	return defaultServeReadyPollInterval
}

// waitServeReady 轮询 health 直到就绪或超时。
func (m *Manager) waitServeReady(ctx context.Context, oc OCClient) error {
	deadline := time.Now().Add(m.serveReadyWaitTimeout())
	poll := m.serveReadyWaitPollInterval()
	for time.Now().Before(deadline) {
		// 每轮先尊重取消：避免 Health 已因 ctx 失败后仍继续轮询到墙钟超时。
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := oc.Health(ctx)
		if err == nil && h.Healthy {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
	// 墙钟耗尽时若 ctx 已取消，优先返回取消（与 startServeWithPortRetry 短路语义一致）。
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("health check timeout")
}

// errServeSessionDied 标记健康轮询期间 serve 会话进程已不存在（与健康超时结构化区分）。
// 调用方用 errors.Is 判定：进程死亡 = 已确认终止，无需 KillSession。
var errServeSessionDied = errors.New("serve session died before ready")

// waitServeReadyOrDead 在 waitServeReady 基础上每轮先判定 serve 会话进程是否已死
// （design.md §3/§19 E3：serve 崩溃后不得等满超时）。仅用于 Activate 重试路径
// （delete.go 的临时 serve 仍用 waitServeReady，保持其调用不变）。
// 进程死亡分支包装 errServeSessionDied，调用方用 errors.Is 区分死亡与健康超时。
func (m *Manager) waitServeReadyOrDead(ctx context.Context, oc OCClient, serveName string) error {
	deadline := time.Now().Add(m.serveReadyWaitTimeout())
	poll := m.serveReadyWaitPollInterval()
	for time.Now().Before(deadline) {
		// 每轮先尊重取消：MUST 先于死亡/健康判定，避免取消窗口误走 kill/allocate。
		if err := ctx.Err(); err != nil {
			return err
		}
		// 先判进程死亡：已死则立即终止轮询（不等满超时）。
		// ErrNoTmuxServer 全仓惯例视为 absent（无 server = 会话不存在），
		// 其他 infra 错误不得伪装死亡（走墙钟超时 → kill 门禁，避免误判逃逸进程）。
		alive, derr := m.proc.HasSession(serveName)
		if derr == nil && !alive || errors.Is(derr, process.ErrNoTmuxServer) {
			return fmt.Errorf("%w: %s", errServeSessionDied, serveName)
		}
		h, err := oc.Health(ctx)
		if err == nil && h.Healthy {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
	// 墙钟耗尽时若 ctx 已取消，优先返回取消（调用方以 ctx.Err()/errors.Is 短路，零本地副作用）。
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("health check timeout")
}

// capabilityProbeError 标记能力探测失败（G3-5：Recovery 分派为终态补偿——「探测失败
// 不轮换」；Activate 路径沿用 OpError code 映射，行为不变）。
type capabilityProbeError struct{ err error }

func (e *capabilityProbeError) Error() string { return e.err.Error() }
func (e *capabilityProbeError) Unwrap() error { return e.err }

// probeErrToOpCode 按 design.md §11/§21 将 Probe 返回的 sentinel 错误映射为 OpError code：
//   - ErrServeNotReady（网络/超时/serve 未就绪）→ process_error（可重试）
//   - ErrCapabilityMismatch（结构不兼容）→ oc_incompatible（激活门禁拒绝）
//   - ErrUnauthorized（401，Basic Auth 凭据错误）→ internal（内部 bug）
//
// Probe 内部已用 classifyProbeErr 归类为这些 sentinel；此处用 errors.Is 兼容 wrap（%w）。
// 返回错误统一包 capabilityProbeError（typed 分派，G3-5；文本不变）。
func probeErrToOpCode(err error) (string, error) {
	switch {
	case errors.Is(err, opencode.ErrCapabilityMismatch):
		return codeOCIncompatible, &capabilityProbeError{err: fmt.Errorf("capability probe: %w", err)}
	case errors.Is(err, opencode.ErrUnauthorized):
		return codeInternal, &capabilityProbeError{err: fmt.Errorf("capability probe (unauthorized, internal bug): %w", err)}
	case errors.Is(err, opencode.ErrServeNotReady):
		return codeProcessError, &capabilityProbeError{err: fmt.Errorf("capability probe (serve not ready): %w", err)}
	default:
		// 未知错误保守按 process_error（可重试），避免误判为不可恢复。
		return codeProcessError, &capabilityProbeError{err: fmt.Errorf("capability probe: %w", err)}
	}
}

// servePortRetries 为总尝试次数（含首次），健康检查失败后换端口重试（design.md D2）。
const servePortRetries = 3

// residualNoticeWriteTimeout 是轮换路径本地 residual notice 写的有界预算
// （design.md D2：WithoutCancel + 10s；单个有界 CAS 收敛流程，不为热路径引入 30s 级延迟）。
const residualNoticeWriteTimeout = 10 * time.Second

// probeColdStartAttempts 是 capability probe 冷启动重试总次数（design.md D8）：
// 首次 + 2 次重试 = 3 次尝试。全新 worktree 是 opencode 冷项目，首次 /session/status
// 可超 10s（实测 7.3s+，负载下超时）归类 ErrServeNotReady；首次超时后服务端初始化已完成，
// 二次命中热路径，故只覆盖冷启动窗口。
const probeColdStartAttempts = 3

// defaultProbeColdStartBackoff 返回冷启动重试默认退避序列（design.md D8：2s、4s）。
// 用函数返回新 slice 避免包级变量被并发改写（未来 t.Parallel 下 race）；
// 调用方按需注入更短序列供测试。
func defaultProbeColdStartBackoff() []time.Duration {
	return []time.Duration{2 * time.Second, 4 * time.Second}
}

// probeWithColdStartRetry 执行 capability probe，遇 ErrServeNotReady 短退避重试（design.md D8）。
//
// 全新 worktree 是 opencode 冷项目，serve 启动后首次 /session/status 可超 10s → Probe 归类
// ErrServeNotReady。此时 serve 进程仍健康，仅能力端点未就绪，故重试期间会话保活（不在重试内 kill）。
// 共 probeColdStartAttempts 次尝试（首次 + 2 次），退避按 probeColdStartBackoff（2s、4s）。
// 退避用 timer + ctx.Done 尊重取消：ctx 取消时立即返回 ctx.Err()（已是最后一次尝试时不再 sleep）。
// 其他错误（ErrCapabilityMismatch / ErrUnauthorized / 未知）立即返回，不重试。
// 成功返回 nil。
func (m *Manager) probeWithColdStartRetry(ctx context.Context, oc OCClient) error {
	backoffFn := m.probeColdStartBackoffFn
	if backoffFn == nil {
		backoffFn = defaultProbeColdStartBackoff
	}
	return runProbeColdStartRetry(ctx, oc, backoffFn())
}

// runProbeColdStartRetry 执行 capability probe，遇 ErrServeNotReady 短退避重试（design.md D8）。
//
// 全新 worktree 是 opencode 冷项目，serve 启动后首次 /session/status 可超 10s → Probe 归类
// ErrServeNotReady。此时 serve 进程仍健康，仅能力端点未就绪，故重试期间会话保活（不在重试内 kill）。
// 共 probeColdStartAttempts 次尝试（首次 + 2 次），退避按 backoff（默认 2s、4s，测试可注入更短序列）。
// 退避用 timer + ctx.Done 尊重取消：ctx 取消时立即返回 ctx.Err()（已是最后一次尝试时不再 sleep）。
// 其他错误（ErrCapabilityMismatch / ErrUnauthorized / 未知）立即返回，不重试。
// 成功返回 nil。
func runProbeColdStartRetry(ctx context.Context, oc OCClient, backoff []time.Duration) error {
	var lastErr error
	for attempt := 0; attempt < probeColdStartAttempts; attempt++ {
		_, err := oc.Probe(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		if !errors.Is(err, opencode.ErrServeNotReady) {
			// 非冷启动错误：立即失败，不重试（mismatch/unauthorized/未知）。
			return err
		}
		// ErrServeNotReady：若非最后一次尝试，退避后重试。
		if attempt == probeColdStartAttempts-1 {
			break
		}
		wait := backoff[attempt]
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("after %d attempts: %w", probeColdStartAttempts, lastErr)
}

// pendingCleanup 是轮换路径 notice 本地写失败时移交外层补偿的持久化载荷
// （design.md D2：sessionName + tickets + reason + retryable）。
type pendingCleanup struct {
	sessionName    string
	cleanupTickets []string
	reason         string
	retryable      bool
}

// pendingCleanupError 包裹轮换路径终态 cause，并携带待回放的 pending cleanup。
// Unwrap 返回 cause；errors.As 可检出后由 runActivateFailureCompensation 回放。
type pendingCleanupError struct {
	pending   pendingCleanup
	noticeErr error
	cause     error
}

func (e *pendingCleanupError) Error() string {
	if e == nil {
		return ""
	}
	if e.noticeErr != nil {
		return fmt.Sprintf("%v; pending cleanup notice: %v", e.cause, e.noticeErr)
	}
	return e.cause.Error()
}

func (e *pendingCleanupError) Unwrap() error { return e.cause }

// cleanupObservation 记录 cleanup 路径一次 notice intent 的观察结果（design.md D2）。
// clean 不形成 observation；写成功 persisted=true 仍参与归并（最新 reason/retryable）。
type cleanupObservation struct {
	sessionName    string
	reason         string
	retryable      bool
	cleanupTickets []string
	persisted      bool
}

// killNoticeClass 是 KillResult 共享 fail-closed 分类结果（轮换 switch 与 cleanup collect 共用）。
// action:
//   - "none"：clean，无 notice
//   - "terminal"：记 notice 后终态（retryable debt 或未知矛盾）
//   - "continue"：记 notice 后可继续轮换（snapshot_missing_degraded）
type killNoticeClass struct {
	action    string // none | terminal | continue
	reason    string
	retryable bool
}

// classifyKillResult 将 KillResult 收敛为共享 fail-closed 分类。
// 未知 disposition 或 disposition 与 SessionKilled 矛盾 → kill_failed / retryable / terminal。
// MUST NOT 经 dispositionToNotice 静默忽略未知值。
func classifyKillResult(res process.KillResult) killNoticeClass {
	// 已知 disposition 的 SessionKilled 一致性（与 process.KillSession 语义对齐）。
	switch res.Disposition {
	case process.DispositionClean:
		if !res.SessionKilled {
			return killNoticeClass{action: "terminal", reason: noticeReasonKillFailed, retryable: true}
		}
		return killNoticeClass{action: "none"}
	case process.DispositionReapFailed:
		if !res.SessionKilled {
			return killNoticeClass{action: "terminal", reason: noticeReasonKillFailed, retryable: true}
		}
		return killNoticeClass{action: "terminal", reason: noticeReasonReapFailed, retryable: true}
	case process.DispositionSnapshotFailed:
		if res.SessionKilled {
			return killNoticeClass{action: "terminal", reason: noticeReasonKillFailed, retryable: true}
		}
		return killNoticeClass{action: "terminal", reason: noticeReasonSnapshotFailed, retryable: true}
	case process.DispositionKillFailed:
		if res.SessionKilled {
			return killNoticeClass{action: "terminal", reason: noticeReasonKillFailed, retryable: true}
		}
		return killNoticeClass{action: "terminal", reason: noticeReasonKillFailed, retryable: true}
	case process.DispositionSnapshotMissingDegraded:
		if res.SessionKilled {
			return killNoticeClass{action: "terminal", reason: noticeReasonKillFailed, retryable: true}
		}
		return killNoticeClass{action: "continue", reason: noticeReasonSnapshotMissing, retryable: false}
	default:
		return killNoticeClass{action: "terminal", reason: noticeReasonKillFailed, retryable: true}
	}
}

// withResidualNoticeCtx 返回脱离请求取消、有界 10s 的 notice 写 ctx。
func withResidualNoticeCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), residualNoticeWriteTimeout)
}

// recordNoticeOrPending 写 residual notice；成功返回 nil；失败返回包裹 pendingCleanupError 的终态错误。
func recordNoticeOrPending(sessionName string, tickets []string, reason string, retryable bool, noticeErr, cause error) error {
	if noticeErr == nil {
		return cause
	}
	return &pendingCleanupError{
		pending: pendingCleanup{
			sessionName:    sessionName,
			cleanupTickets: append([]string(nil), tickets...),
			reason:         reason,
			retryable:      retryable,
		},
		noticeErr: noticeErr,
		cause:     cause,
	}
}

// aggregateServeWaitErr 聚合轮换路径终态错误（wait + kill/disposition + notice 文本上下文）。
// 仅用于需要文本诊断、不要求保留底层 error chain 的路径（终态 disposition/末次预算等）。
// 需要保留 aerr/perr 可 errors.Is 的路径请用 wrapServeWaitCause。
func aggregateServeWaitErr(waitErr error, parts ...string) error {
	msg := fmt.Sprintf("serve not ready: %v", waitErr)
	for _, p := range parts {
		if p != "" {
			msg += "; " + p
		}
	}
	return errors.New(msg)
}

// wrapServeWaitCause 在保留 wait/disposition 文本上下文的同时，以 %w 包装 finalErr，
// 使 errors.Is/As 仍可达分配/持久化等底层错误（design/spec：分配失败 MUST 包装 aerr）。
func wrapServeWaitCause(waitErr error, finalErr error, parts ...string) error {
	msg := fmt.Sprintf("serve not ready: %v", waitErr)
	for _, p := range parts {
		if p != "" {
			msg += "; " + p
		}
	}
	if finalErr == nil {
		return errors.New(msg)
	}
	return fmt.Errorf("%s: %w", msg, finalErr)
}

// runtimeCmdArgv 构造单进程启动 argv：`opencode --port <p> --hostname 127.0.0.1`，
// 有锚定时追加 `--session <id>`（D1/D5）。密码经 env 注入，禁止进 argv。
func runtimeCmdArgv(port int, sessionID string) []string {
	argv := []string{"opencode", "--port", strconv.Itoa(port), "--hostname", "127.0.0.1"}
	if sessionID != "" {
		argv = append(argv, "--session", sessionID)
	}
	return argv
}

// startServeWithPortRetry 创建 runtime 会话并健康检查；未就绪时按既有门禁换端口重试
// （Activate 形态：无 permit、密码复用）。sessionID 非空时命令携带 `--session`。
// 返回最终可用端口；轮换路径终态错误可携带 pendingCleanupError。
func (m *Manager) startServeWithPortRetry(ctx context.Context, row TaskRow, serveName string, port int, password string, env map[string]string, sessionID string) (int, error) {
	port, _, err := m.startRuntimeWithPortRetry(ctx, row, serveName, port, password, env, sessionID, nil, false)
	return port, err
}

// startRuntimeWithPortRetry 单进程启动 + 健康检查 + 能力探测（Activate/Recovery 共用）。
// 每轮原子序列（G3-4）：permit（beforeCreate，仅 Recovery）→ 端口分配（轮换）→
// env 快照持久化 → NewSession（Recovery 每次重新生成密码）→ 健康轮询 → 能力探测。
// 端口变更时 MUST 同步三处 OCDECK_SERVE_PORT：内存 env map、持久化 tasks.env_snapshot、
// 新建 runtime 会话环境。Probe 失败 MUST NOT 本地 kill / 换端口；kill+notice 委托外层
// compensation。返回最终可用端口与本次创建实际使用的密码；轮换路径终态错误可携带
// pendingCleanupError。
func (m *Manager) startRuntimeWithPortRetry(ctx context.Context, row TaskRow, serveName string, port int, password string, env map[string]string, sessionID string, beforeCreate func() error, freshPassword bool) (int, string, error) {
	// prevWaitErr / prevRotateParts 承载上一轮健康失败上下文（G3-4：端口分配移至轮次
	// 顶部、permit 之后；轮间上下文经此传递，操作顺序与既有一致）。
	var prevWaitErr error
	var prevRotateParts []string
	for attempt := 0; attempt < servePortRetries; attempt++ {
		// (0) Recovery permit：MUST 先于端口分配与任何进程副作用（D3/G3-4）。
		if beforeCreate != nil {
			if err := beforeCreate(); err != nil {
				return port, password, err
			}
		}
		if attempt > 0 {
			// (1) 端口轮换（G3-4 移至轮次顶部）：排除刚失败端口后分配；失败 MUST 包装
			// aerr（%w）并保留上轮 wait 上下文。
			newPort, aerr := m.allocatePort(sql.NullInt64{Int64: int64(port), Valid: true}, port)
			if aerr != nil {
				return port, password, &portAllocationError{err: newOpErr(codeConflict, wrapServeWaitCause(prevWaitErr, aerr, prevRotateParts...))}
			}
			port = newPort
			// (2) persist 失败 → 终态，MUST NOT NewSession。保留 wait + disposition 上下文，%w 包装 perr。
			// env 可能为 nil（调用方/测试注入）；写入前初始化，语义仍是更新后 persist。
			if env == nil {
				env = make(map[string]string)
			}
			env["OCDECK_SERVE_PORT"] = strconv.Itoa(port)
			if perr := m.persistEnvSnapshot(ctx, row.ID, env); perr != nil {
				return port, password, newOpErr(codeInternal, &persistEnvSnapshotError{err: wrapServeWaitCause(prevWaitErr, perr, prevRotateParts...)})
			}
		}
		// Recovery 每次创建进程重新生成密码（G3-4：MUST NOT 循环复用同密码；
		// Activate 沿用传入密码不变）。
		if freshPassword {
			password = newRandomPassword()
		}
		serveEnv := copyMap(env)
		serveEnv["OPENCODE_SERVER_PASSWORD"] = password
		serveEnv["OCDECK_SERVE_PORT"] = strconv.Itoa(port)
		if err := m.proc.NewSession(process.SessionSpec{
			Name:    serveName,
			Dir:     row.WorktreePath,
			Env:     serveEnv,
			CmdArgv: runtimeCmdArgv(port, sessionID),
		}); err != nil {
			return port, password, newOpErr(codeProcessError, fmt.Errorf("runtime session: %w", err))
		}
		oc := m.ocFactory(port, password, opencode.Options{
			HealthTimeout: 2 * time.Second,
			OpTimeout:     10 * time.Second,
		})
		if err := m.waitServeReadyOrDead(ctx, oc, serveName); err != nil {
			// (a) ctx 取消：零副作用短路（MUST NOT kill/allocate/persist）。
			// 同时认 wait 返回的取消错误与调用方 ctx（墙钟竞态下 wait 可能先返回 timeout）。
			if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				if ctx.Err() != nil {
					return port, password, ctx.Err()
				}
				return port, password, err
			}
			// (b) 进程已死亡：无 KillSession / disposition / notice → 直接预算检查。
			canRotate := errors.Is(err, errServeSessionDied)
			// 本轮可带入终态上下文的额外片段（disposition 等）。
			var rotateParts []string
			if !canRotate {
				// (c) 健康超时：KillSession 门禁 + notice 写（WithoutCancel + 10s）。
				res, kerr := m.proc.KillSession(serveName)
				nctx, ncancel := withResidualNoticeCtx(ctx)
				if kerr != nil {
					nerr := m.recordResidualNotice(nctx, row.ID, serveName, res.CleanupTickets, noticeReasonKillFailed, true)
					ncancel()
					cause := newOpErr(codeProcessError, aggregateServeWaitErr(err, fmt.Sprintf("kill: %v", kerr)))
					if nerr != nil {
						return port, password, recordNoticeOrPending(serveName, res.CleanupTickets, noticeReasonKillFailed, true, nerr, cause)
					}
					// G3-2/G3-5：kill infra 错误（notice 已落库）→ retryable cleanup debt
					// 在位，Recovery MUST NOT 再创建进程（typed 终态；Activate 行为不变）。
					return port, password, &retryableCleanupError{err: cause}
				}
				cls := classifyKillResult(res)
				switch cls.action {
				case "none":
					ncancel()
					canRotate = true
				case "continue":
					nerr := m.recordResidualNotice(nctx, row.ID, serveName, res.CleanupTickets, cls.reason, cls.retryable)
					ncancel()
					if nerr != nil {
						cause := newOpErr(codeProcessError, aggregateServeWaitErr(err,
							fmt.Sprintf("disposition: %s", res.Disposition),
							fmt.Sprintf("notice: %v", nerr)))
						return port, password, recordNoticeOrPending(serveName, res.CleanupTickets, cls.reason, cls.retryable, nerr, cause)
					}
					// snapshot_missing_degraded 记 notice 后可继续；保留 disposition 供末次终态聚合。
					canRotate = true
					rotateParts = append(rotateParts, fmt.Sprintf("disposition: %s", res.Disposition))
				default: // terminal
					nerr := m.recordResidualNotice(nctx, row.ID, serveName, res.CleanupTickets, cls.reason, cls.retryable)
					ncancel()
					parts := []string{fmt.Sprintf("disposition: %s", res.Disposition)}
					if nerr != nil {
						parts = append(parts, fmt.Sprintf("notice: %v", nerr))
					}
					cause := newOpErr(codeProcessError, aggregateServeWaitErr(err, parts...))
					if nerr != nil {
						return port, password, recordNoticeOrPending(serveName, res.CleanupTickets, cls.reason, cls.retryable, nerr, cause)
					}
					// G3-2/G3-5：retryable/未知矛盾 disposition（notice 已落库）→
					// Recovery MUST NOT 再创建进程（typed 终态；Activate 行为不变）。
					return port, password, &retryableCleanupError{err: cause}
				}
			}
			// (d) 末次预算：MUST NOT allocate/persist。终态聚合 wait + 本轮可得 disposition。
			if attempt == servePortRetries-1 {
				return port, password, newOpErr(codeProcessError, aggregateServeWaitErr(err, rotateParts...))
			}
			// (e) 轮换上下文带入下一轮顶部（permit → allocate → persist → NewSession）。
			prevWaitErr = err
			prevRotateParts = rotateParts
			continue
		}
		// 健康就绪 → 能力探测。
		// D8：冷启动 ErrServeNotReady 短退避重试；任何 Probe 失败 MUST NOT 本地 kill / 换端口
		// （kill+notice 委托外层 runActivateFailureCompensation）。
		if err := m.probeWithColdStartRetry(ctx, oc); err != nil {
			code, ferr := probeErrToOpCode(err)
			return port, password, newOpErr(code, ferr)
		}
		return port, password, nil
	}
	return port, password, newOpErr(codeProcessError, fmt.Errorf("serve not ready after %d port retries", servePortRetries))
}

// persistEnvSnapshotError 标记 env 快照写失败（G3-5：Recovery 分派为 attempt 重试；
// Activate 仍走既有终态补偿）。
type persistEnvSnapshotError struct{ err error }

func (e *persistEnvSnapshotError) Error() string { return e.err.Error() }
func (e *persistEnvSnapshotError) Unwrap() error { return e.err }

// lastPortWriteError 标记 tasks.last_port 写失败（G3-5：Recovery 在提交回滚后重试；
// Activate 走既有补偿）。
type lastPortWriteError struct{ err error }

func (e *lastPortWriteError) Error() string { return e.err.Error() }
func (e *lastPortWriteError) Unwrap() error { return e.err }

// persistEnvSnapshot 将已合并的 env map 持久化为 tasks.env_snapshot（不含密码）。
// 端口变更重试时复用 mergeEnvSnapshot 的持久化路径，保证快照与新端口一致（E1）。
func (m *Manager) persistEnvSnapshot(ctx context.Context, taskID string, merged map[string]string) error {
	snap := envSnapshot{Vars: merged}
	b, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal env snapshot: %w", err)
	}
	if _, err := m.writeEnvSnapshot(ctx, taskID, sql.NullString{String: string(b), Valid: true}); err != nil {
		return fmt.Errorf("persist env snapshot: %w", err)
	}
	return nil
}

// startSSE 启动 SSE 订阅 goroutine（design.md §4）。
// SSE 生命周期挂 Manager 生命周期 context（非 HTTP request context，B5）。
// onReady 后先全量对齐再放行业务事件：对齐竞态期间 SSE 事件 MUST 缓冲，
// 对齐完成后按序重放（缓冲起止与对齐替换 MUST 原子，B5）。
// 对齐错误传播（返回 error，不吞，B5）。
func (m *Manager) startSSE(ctx context.Context, rt *taskRuntime, taskID, wtPath string, port int, password string, mode AlignMode) error {
	// SSE 挂 Manager 生命周期 context，非传入的 HTTP request ctx。
	sseCtx, cancel := context.WithCancel(m.lifecycleCtx())
	sseDone := make(chan struct{})
	rt.mu.Lock()
	rt.sseCancel = func() {
		// 阻塞式 cancel：取消 context 后 join SSE goroutine，避免 goroutine 在
		// cancel 后仍写已关资源（design.md §4 lifecycle 收敛，G）。
		cancel()
		<-sseDone
	}
	rt.sseDone = sseDone
	rt.mu.Unlock()

	readyCh := make(chan struct{}, 1)
	// 连接代跟踪（P1.8 复评阻塞 3）：每次连接建立（OnReady 首连/onReconnect 重连）
	// 记录当前连接代，断流回调捕获该值传入 apply——client 回调均在 SSE goroutine 内
	// 串行（断流先于下一连接建立），捕获值即刚断连接的代；旧连接的延迟回调经 apply
	// 的 epoch 匹配被拒，不得误伤新连接代。
	var connEpochMu sync.Mutex
	var connEpoch uint64
	ocWithReady := m.ocFactory(port, password, opencode.Options{
		HealthTimeout:    2 * time.Second,
		HeartbeatTimeout: 60 * time.Second,
		OnReady: func() {
			// P1.8：连接建立 → 新连接代 + aligning（epoch 单调，独立于激活代，design D4）。
			connEpochMu.Lock()
			connEpoch = rt.ensureAgentStatusState().apply(agentStatusOp{kind: agentOpConnect}).Epoch
			connEpochMu.Unlock()
			select {
			case readyCh <- struct{}{}:
			default:
			}
		},
		// P1.8.4：已建立连接终止（非主动取消）→ 重连退避前同步回调一次。task 层校验
		// runtime 激活代身份后经唯一 apply 使回调捕获的连接代失效（design D4 断流感知）。
		OnDisconnect: func() {
			connEpochMu.Lock()
			epoch := connEpoch
			connEpochMu.Unlock()
			m.handleAgentStatusDisconnect(taskID, rt, epoch)
		},
	})

	// 缓冲屏障：对齐完成前 SSE 事件缓冲；对齐完成后放行（design.md §4）。
	var bufMu sync.Mutex
	buffered := []opencode.Event{}
	buffering := true

	// align 串行化屏障（design.md §4）：任一时刻最多一个 align 在执行（首次 align + reconnect align 互斥）。
	// 首次 align 进行中若断流，onReconnect MUST 排队等待（合并为一次重对齐），MUST NOT 并发清空 buffered
	// 造成事件丢失/乱序。等待期间 buffering 保持 true，事件继续缓冲；待 align 释放后 reconnect 重新全量对齐
	//（丢弃半成品状态安全，AlignTaskSessions 幂等 upsert）。
	var alignMu sync.Mutex

	// drainAndRelease 排空全部缓冲事件再置 buffering=false 放行实时事件（design.md §4：
	// 缓冲起止与对齐替换 MUST 原子）。重放期间 buffering 仍为 true，新到达的实时事件继续
	// 缓冲，不得越过缓冲事件被先处理。逐批取出缓冲事件→处理→重读缓冲，直到某批处理前后
	// 均无新缓冲事件，才置 buffering=false。这保证所有缓冲事件（含重放期间新缓冲的）先于
	// 任何实时事件落库，实时事件不会越过缓冲事件越序。
	drainAndRelease := func() {
		for {
			bufMu.Lock()
			replay := buffered
			buffered = nil
			bufMu.Unlock()
			// 处理本批缓冲事件（buffering 仍为 true，期间到达的事件继续缓冲）。
			// R7 failpoint：session 落库错误 MUST 收敛运行时（design.md §4/§19）。
			for _, ev := range replay {
				if err := m.handleSSEEvent(sseCtx, taskID, wtPath, ev); err != nil {
					// G3-1：经统一 fatal 上报（incident 同步域 + 幂等 ensureRecovery），
					// 提交期 fatal 不得因 activating no-op 而丢失。
					m.reportRuntimeFatal(taskID, rt.instVersion, fmt.Errorf("sse buffered event: %w", err))
					return
				}
			}
			// 处理完后再取一批：若重放期间有新事件缓冲，继续处理；否则放行。
			bufMu.Lock()
			if len(buffered) == 0 {
				// 缓冲已彻底排空且无新增：放行实时事件。
				buffering = false
				bufMu.Unlock()
				return
			}
			// 仍有重放期间新缓冲的事件，继续排空（buffering 保持 true）。
			bufMu.Unlock()
		}
	}

	go func() {
		defer close(sseDone)
		onEvent := func(ev opencode.Event) {
			bufMu.Lock()
			if buffering {
				buffered = append(buffered, ev)
				bufMu.Unlock()
				return
			}
			bufMu.Unlock()
			// R7 failpoint：session 落库错误 MUST 收敛运行时（会话归属丢失 → 运行时不可确定，
			// design.md §4/§19）。经统一 fatal 上报（G3-1）。
			if err := m.handleSSEEvent(sseCtx, taskID, wtPath, ev); err != nil {
				m.reportRuntimeFatal(taskID, rt.instVersion, fmt.Errorf("sse event: %w", err))
				return
			}
		}
		onReconnect := func() {
			// 串行化：等待进行中的 align（首次 align 或上一轮 reconnect align）完成后再重对齐，
			// 不并发清空 buffered（design.md §4）。等待期间 buffering 仍为 true，事件继续缓冲；
			// 待 align 完成后重新进入缓冲模式 + 全量对齐（合并为一次重对齐，幂等 upsert 安全）。
			alignMu.Lock()
			defer alignMu.Unlock()
			// 重连后全量对齐屏障（design.md §4）：重新进入缓冲模式 + 全量对齐。
			bufMu.Lock()
			buffering = true
			buffered = nil
			bufMu.Unlock()
			// P1.8：重连建立 → 新连接代 + aligning（对齐串行域内，断流的旧 epoch 已失效）。
			connEpochMu.Lock()
			connEpoch = rt.ensureAgentStatusState().apply(agentStatusOp{kind: agentOpConnect}).Epoch
			connEpochMu.Unlock()
			if err := m.alignSessions(sseCtx, taskID, wtPath, ocWithReady, mode); err != nil {
				// 重连对齐失败 MUST 收敛任务状态（design.md §4）：不得只取消 SSE 留 active 假象。
				// serve 可能仍存活但无法追踪会话，视同运行时不可确定 → 经统一 fatal 上报
				//（G3-1：incident 同步域 + 幂等 ensureRecovery）。
				cancel()
				m.reportRuntimeFatal(taskID, rt.instVersion, fmt.Errorf("sse reconnect align: %w", err))
				return
			}
			// D6 注意力对账（align 路径）：session align 成功后、drainAndRelease 前。失败不影响任务状态机。
			m.reconcileTaskAttention(sseCtx, rt, ocWithReady, wtPath)
			// P1.8.3：agentStatus 对账（align 成功后、drainAndRelease 前；失败仅保持不可用，
			// 不影响任务生命周期）。
			m.reconcileAgentStatus(sseCtx, rt, taskID, wtPath, ocWithReady)
			// B4b：先排空全部缓冲事件再置 buffering=false 放行实时事件（design.md §4）。
			drainAndRelease()
		}
		// B4c：SubscribeEvents 永久返回 MUST 有处理路径（converge suspended + last_error）。
		// 返回非 nil 或正常返回（流结束）都意味着 SSE 不再托管 → 不得留 active 无 SSE 假象。
		sseErr := ocWithReady.SubscribeEvents(sseCtx, wtPath, onEvent, onReconnect)
		if sseErr != nil && sseErr != context.Canceled && !errors.Is(sseErr, context.Canceled) {
			// SSE 流异常结束（非主动 cancel）：经统一 fatal 上报（G3-1）。
			m.reportRuntimeFatal(taskID, rt.instVersion, fmt.Errorf("sse stream ended: %w", sseErr))
			return
		}
		// 正常返回（流结束/ctx 取消）：若 ctx 未被主动 cancel（即非 Activate/Shutdown 主动停 SSE），
		// serve 仍存活但 SSE 流终止 → 收敛。sseCtx 被 cancel 的情况由 Activate 返回路径/Shutdown 处理，不在此收敛。
		if sseCtx.Err() == nil {
			m.reportRuntimeFatal(taskID, rt.instVersion, errors.New("sse stream ended"))
		}
	}()

	// 等待 SSE 建立（onReady）后做首次全量对齐（屏障：对齐完成后放行缓冲）。
	// 串行化（alignMu）：首次 align 与 reconnect align 互斥，断流发生在首次 align 中途时 reconnect 排队等待，
	// 不并发清空 buffered（design.md §4）。
	select {
	case <-readyCh:
		alignMu.Lock()
		if err := m.alignSessions(sseCtx, taskID, wtPath, ocWithReady, mode); err != nil {
			alignMu.Unlock()
			cancel()
			return fmt.Errorf("sse initial align: %w", err)
		}
		// D6 注意力对账（align 路径）：首次 align 成功后、drainAndRelease 前。
		m.reconcileTaskAttention(sseCtx, rt, ocWithReady, wtPath)
		// P1.8.3：agentStatus 对账（首次 align 成功后、drainAndRelease 前）。
		m.reconcileAgentStatus(sseCtx, rt, taskID, wtPath, ocWithReady)
		// B4b：先排空全部缓冲事件再置 buffering=false 放行实时事件（design.md §4）。
		drainAndRelease()
		alignMu.Unlock()
	case <-time.After(15 * time.Second):
		cancel()
		return fmt.Errorf("sse ready timeout")
	}
	return nil
}

// handleSSEEvent 处理 SSE 事件：session.created/updated → upsert；session.deleted → 删行。
// R7 failpoint：store 落库错误 MUST 返回（不得 _ = 丢弃，design.md §19 failpoint 表）。
// 调用方据返回错误收敛运行时（convergeToSuspended）：会话归属丢失意味着运行时不可确定
// （无法追踪会话 → 可能误判状态），MUST NOT 继续正常处理后续事件（design.md §4/§8）。
//
// SSE directory 防线（design.md §4 补注 + VERIFICATION.md 实测）：
//   - created/updated/deleted 事件校验 properties.info.directory == 本任务 worktree（wtPath），
//     fail-closed 条件 = directory 明确等于 worktree；其余（不同/缺失）一律丢弃并告警，
//     不落库（防止跨任务/跨 worktree 串流污染本任务 session 表）。
//   - status/diff 事件无 properties.info、仅 properties.sessionID（VERIFICATION.md 实测）：
//     用 properties.sessionID 反查 task_sessions 归属，命中本任务才处理（SessionIDProp 优先
//     properties.sessionID 回退 properties.info.id）。
func (m *Manager) handleSSEEvent(ctx context.Context, taskID, wtPath string, ev opencode.Event) error {
	switch ev.Type {
	case "session.created", "session.updated":
		sid := ev.SessionID()
		if sid == "" {
			return nil
		}
		// B3a：fail-closed——directory MUST 明确等于本任务 worktree；不同或缺失均丢弃并告警。
		dir, ok := ev.Directory()
		if !ok || dir != wtPath {
			log.Printf("task %s: sse %s for session %s has directory %q (ok=%v) != worktree %q; dropping",
				taskID, ev.Type, sid, dir, ok, wtPath)
			return nil
		}
		if ev.Type == "session.updated" {
			// D8：session.updated 仅刷新本任务已归属行的 last_seen_at，绝不创建归属。
			// 未归属 session 的 updated 一律忽略（!Matched 正常路径，记 debug 不报错）。
			updatedTS, hasTS := ev.TimeUpdated()
			if !hasTS {
				return nil
			}
			if m.lifecycle != nil {
				res, uerr := m.lifecycle.TouchOwnedSession(ctx, taskID, ocdecksess.ID(sid), int64(updatedTS))
				if uerr != nil {
					return fmt.Errorf("touch owned session %s: %w", sid, uerr)
				}
				if !res.Matched {
					log.Printf("task %s: session.updated for unowned session %s; ignoring", taskID, sid)
				}
				return nil
			}
			res, uerr := m.store.TouchOwnedTaskSession(ctx, taskID, sid, int64(updatedTS))
			if uerr != nil {
				return fmt.Errorf("touch owned session %s: %w", sid, uerr)
			}
			if !res.Matched {
				log.Printf("task %s: session.updated for unowned session %s; ignoring", taskID, sid)
			}
			return nil
		}
		// session.created：原子 claim（D8）。冲突 → 忽略 + 记诊断日志（不阻断）。
		var createdAt, lastSeen int64
		if updated, ok := ev.TimeUpdated(); ok {
			createdAt = int64(updated)
			lastSeen = int64(updated)
		}
		if m.lifecycle != nil {
			obs := ocdecksess.Observation{
				ID: ocdecksess.ID(sid), ParentID: ev.ParentID(),
				CreatedAt: createdAt, UpdatedAt: lastSeen, FirstSeenAt: nowUnixI(),
			}
			cres, cerr := m.lifecycle.ClaimSession(ctx, taskID, obs)
			if cerr != nil {
				return fmt.Errorf("claim session %s: %w", sid, cerr)
			}
			if !cres.Claimed {
				log.Printf("task %s: sse session.created for session %s conflict (owned by task %s); skipping",
					taskID, sid, cres.OwnerTaskID)
				return nil
			}
			// P1.8.2：owned 成员变更经唯一 apply 维护（默认 idle、重聚合、可用性）。
			m.noteAgentSessionClaimed(taskID, sid)
			return nil
		}
		cres, cerr := m.store.ClaimTaskSession(ctx, taskID, sid, createdAt, nowUnixI(), lastSeen, ev.ParentID())
		if cerr != nil {
			return fmt.Errorf("claim session %s: %w", sid, cerr)
		}
		if !cres.Claimed {
			log.Printf("task %s: sse session.created for session %s conflict (owned by task %s); skipping",
				taskID, sid, cres.OwnerTaskID)
			return nil
		}
		m.noteAgentSessionClaimed(taskID, sid)
	case "session.deleted":
		sid := ev.SessionID()
		if sid == "" {
			return nil
		}
		// B3a：fail-closed——deleted 同源校验 directory；不同或缺失均丢弃（防止误删其他任务 session 行）。
		dir, ok := ev.Directory()
		if !ok || dir != wtPath {
			log.Printf("task %s: sse %s for session %s has directory %q (ok=%v) != worktree %q; dropping",
				taskID, ev.Type, sid, dir, ok, wtPath)
			return nil
		}
		if m.lifecycle != nil {
			if _, err := m.lifecycle.DeleteOwnedSession(ctx, taskID, ocdecksess.ID(sid)); err != nil {
				return fmt.Errorf("delete session %s: %w", sid, err)
			}
			// P1.8.2：owned 成员移除经同一 apply 维护（重聚合、可用性 1→0）。
			m.noteAgentSessionDeleted(taskID, sid)
			return nil
		}
		if _, err := m.store.DeleteTaskSession(ctx, taskID, sid); err != nil {
			return fmt.Errorf("delete session %s: %w", sid, err)
		}
		m.noteAgentSessionDeleted(taskID, sid)
	default:
		// D6 注意力事件：permission/question v1+v2 家族。ParseAttentionEvent 命中则应用，
		// 未知/缺字段静默忽略。注意力事件处理永不返回错误（不影响任务状态机）。
		// P1.4.5：apply 返回 changed，经 commit helper 发布 serve_runtime.attention_changed
		//（NoopPublisher 阶段调用位就绪无实际发布）。
		if aev, ok := opencode.ParseAttentionEvent(ev); ok {
			if rt := m.getRuntime(taskID); rt != nil {
				if rt.applyAttentionEvent(aev) {
					m.commitAttentionChanged(taskID, rt, true)
				}
			}
			return nil
		}
		// B3b：status/diff 等无 properties.info 的事件，sessionID 取 properties.sessionID
		//（VERIFICATION.md 实测，回退 properties.info.id），反查 task_sessions 归属，
		// 命中本任务才处理（design.md §4 补注）。未命中本任务的 session 不动本任务数据。
		// 历史重复归属 fail-closed：OwnerOf 返回 typed ambiguity error（design D0）。
		sid := ev.SessionIDProp()
		if sid == "" {
			return nil
		}
		if m.lifecycle != nil {
			owner, found, err := m.lifecycle.OwnerOf(ctx, ocdecksess.ID(sid))
			if err != nil {
				return fmt.Errorf("check session ownership %s: %w", sid, err)
			}
			if !found || owner != taskID {
				return nil
			}
		} else {
			owns, err := m.sessionBelongsToTask(ctx, taskID, sid)
			if err != nil {
				return fmt.Errorf("check session ownership %s: %w", sid, err)
			}
			if !owns {
				return nil
			}
		}
		// P1.8.2（模式 A）：session.status 事件经上方归属反查（fail-closed）后更新
		// agentStatus 内存态；解析失败/未知枚举静默忽略。模式 B 不解析不 apply
		//（design D4 模式执行矩阵；模式分支经参数化暴露给测试，无运行时切换）。
		m.applySessionStatusEvent(taskID, ev, agentStatusModeA)
	}
	return nil
}

// sessionBelongsToTask 反查 task_sessions 中 sessionID 是否归属于 taskID（design.md §4 补注：
// status/diff 事件无 directory，按 sessionID 反查归属，命中本任务才处理）。
func (m *Manager) sessionBelongsToTask(ctx context.Context, taskID, sid string) (bool, error) {
	rows, err := m.store.ListTaskSessions(ctx, taskID)
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		if r.SessionID == sid {
			return true, nil
		}
	}
	return false, nil
}

// alignSessions 全量对齐（design.md §4 + add-plain-dir-project D8）：
// GET /session?directory=<wt>&limit=1000。count<limit → complete=true（删 owned 缺席行）；
// count==limit → overflow，complete=false，先经事务外 CAS 写 session_overflow notice 再调对齐
// （对齐失败 notice 保留，B5）。
//
// mode=AlignModeRepo：listed 逐个原子 claim（单任务场景 guard 不命中，行为与既有 upsert 一致），
// 冲突 ID 经 align 编排上报日志（冲突 session 在事务内被跳过）。
// mode=AlignModeOwnedOnly（dir）：仅对 listed∩owned 刷新 last_seen_at，绝不 claim。
// complete/overflow 判定 MUST 基于原始目录列表（过滤之前，D8 算法第 1 步）。
//
// P1.4.5：注入 LifecycleService 时委托（overflow 前置 CAS + complete notice 决策随事务 +
// AlignConflict 有界重试统一在 application/task.RunAlign）；未注入时经 storeAlignPortsAdapter
// 共用同一编排（TaskStore 支撑的窄端口）。
//
// 返回 error（不吞，B5）：非 overflow 错误传播；AlignTaskSessions store 错误传播。
func (m *Manager) alignSessions(ctx context.Context, taskID, wtPath string, oc OCClient, mode AlignMode) error {
	sessions, err := oc.ListSessions(ctx, wtPath, 1000)
	complete := true
	if err != nil {
		// 溢出（count==limit）仅刷新不删缺席行，写 session_overflow notice（B5）。
		if isOverflow(err) {
			complete = false
		} else {
			return fmt.Errorf("list sessions: %w", err)
		}
	}
	obs := make([]ocdecksess.Observation, 0, len(sessions))
	for _, s := range sessions {
		obs = append(obs, ocdecksess.Observation{
			ID:        ocdecksess.ID(s.ID),
			ParentID:  s.ParentID,
			CreatedAt: int64(s.Time.Created),
			UpdatedAt: int64(s.Time.Updated),
		})
	}
	if m.lifecycle != nil {
		_, aerr := m.lifecycle.AlignSessions(ctx, taskID, toDomainAlignMode(mode), obs, complete)
		return aerr
	}
	_, aerr := apptask.RunAlign(ctx, storeAlignPortsAdapter{store: m.store}, taskID, toDomainAlignMode(mode), obs, complete)
	return aerr
}

// anchorStageError 标记 D5 锚定阶段的确定性失败（G3-5：claim 冲突/锚定写失败/条件清空
// store 错误/CAS 异常 → Recovery 终态补偿；Activate 沿用 OpError code 映射，行为不变）。
type anchorStageError struct{ err error }

func (e *anchorStageError) Error() string { return e.err.Error() }
func (e *anchorStageError) Unwrap() error { return e.err }

// bootstrapRuntime 执行 D5 确定性锚定协议。Activate（beforeCreate=nil，不耗恢复 permit）
// 与 Recovery（beforeCreate 非 nil：每次 NewSession 前先取 permit+退避；freshPassword：
// 每次创建重新生成密码，G3-4）共用。有锚定 → `--session` 启动，ready 后列表校验；
// 缺席则条件清空转无锚定。无锚定 → 不带 `--session` 启动，POST+claim 后确认 bootstrap
// 终止，再以 `--session` 双启动。返回最终端口与实际使用的密码。
func (m *Manager) bootstrapRuntime(ctx context.Context, row TaskRow, runtimeName string, port int, password string, env map[string]string, beforeCreate func() error, freshPassword bool) (int, string, error) {
	for attempt := 0; attempt < servePortRetries; attempt++ {
		fresh, rerr := m.store.GetTask(ctx, row.ID)
		if rerr != nil {
			return port, password, newOpErr(codeInternal, rerr)
		}
		row = fresh
		anchor := ""
		if row.AnchorSessionID.Valid {
			anchor = row.AnchorSessionID.String
		}
		if anchor != "" {
			var err error
			port, password, err = m.startRuntimeWithPortRetry(ctx, row, runtimeName, port, password, env, anchor, beforeCreate, freshPassword)
			if err != nil {
				return port, password, err
			}
			oc := m.ocFactory(port, password, opencode.Options{HealthTimeout: 2 * time.Second, OpTimeout: 10 * time.Second})
			ok, verr := m.anchorPresentInList(ctx, oc, row.WorktreePath, anchor)
			if verr != nil {
				return port, password, newOpErr(codeProcessError, verr)
			}
			if ok {
				return port, password, nil
			}
			cleared, cerr := m.store.ClearTaskAnchorConditional(ctx, row.ID, anchor)
			if cerr != nil {
				return port, password, &anchorStageError{err: newOpErr(codeInternal, fmt.Errorf("clear stale anchor: %w", cerr))}
			}
			if !cleared.Matched {
				reread, rrerr := m.store.GetTask(ctx, row.ID)
				if rrerr != nil {
					return port, password, newOpErr(codeInternal, rrerr)
				}
				if !reread.AnchorSessionID.Valid || reread.AnchorSessionID.String == "" {
					anchor = ""
				} else if reread.AnchorSessionID.String != anchor {
					if err := m.confirmRuntimeTerminated(ctx, row.ID, runtimeName); err != nil {
						return port, password, err
					}
					password = newRandomPassword()
					continue
				} else {
					return port, password, &anchorStageError{err: newOpErr(codeInternal, fmt.Errorf("clear stale anchor: CAS mismatch with unchanged anchor %s", anchor))}
				}
			}
			if err := m.confirmRuntimeTerminated(ctx, row.ID, runtimeName); err != nil {
				return port, password, err
			}
			password = newRandomPassword()
		}

		var err error
		port, password, err = m.startRuntimeWithPortRetry(ctx, row, runtimeName, port, password, env, "", beforeCreate, freshPassword)
		if err != nil {
			return port, password, err
		}
		oc := m.ocFactory(port, password, opencode.Options{HealthTimeout: 2 * time.Second, OpTimeout: 10 * time.Second})
		created, cerr := oc.CreateSession(ctx, row.WorktreePath, row.Name)
		if cerr != nil {
			return port, password, &anchorStageError{err: newOpErr(codeProcessError, fmt.Errorf("create anchor session: %w", cerr))}
		}
		cres, perr := m.store.ClaimTaskSessionAndSetAnchor(ctx, row.ID, created.ID,
			int64(created.Time.Created), int64(created.Time.Updated), int64(created.Time.Updated), "")
		if perr != nil {
			return port, password, &anchorStageError{err: newOpErr(codeInternal, fmt.Errorf("persist anchor session %s: %w", created.ID, perr))}
		}
		if !cres.Claimed {
			return port, password, &anchorStageError{err: newOpErr(codeProcessError, fmt.Errorf("anchor session %s conflict (owned by task %s); MUST NOT attach", created.ID, cres.OwnerTaskID))}
		}
		m.noteAgentSessionClaimed(row.ID, created.ID)
		if err := m.confirmRuntimeTerminated(ctx, row.ID, runtimeName); err != nil {
			return port, password, err
		}
		password = newRandomPassword()
		port, password, err = m.startRuntimeWithPortRetry(ctx, row, runtimeName, port, password, env, created.ID, beforeCreate, freshPassword)
		if err != nil {
			return port, password, err
		}
		oc = m.ocFactory(port, password, opencode.Options{HealthTimeout: 2 * time.Second, OpTimeout: 10 * time.Second})
		ok, verr := m.anchorPresentInList(ctx, oc, row.WorktreePath, created.ID)
		if verr != nil {
			return port, password, newOpErr(codeProcessError, verr)
		}
		if !ok {
			return port, password, &anchorStageError{err: newOpErr(codeProcessError, fmt.Errorf("anchor session %s missing after dual-start", created.ID))}
		}
		return port, password, nil
	}
	return port, password, newOpErr(codeProcessError, fmt.Errorf("anchor bootstrap exhausted after %d attempts", servePortRetries))
}

func (m *Manager) anchorPresentInList(ctx context.Context, oc OCClient, dir, sessionID string) (bool, error) {
	sessions, err := oc.ListSessions(ctx, dir, 1000)
	if err != nil {
		return false, fmt.Errorf("list sessions for anchor check: %w", err)
	}
	for _, s := range sessions {
		if s.ID == sessionID {
			return true, nil
		}
	}
	return false, nil
}

// confirmRuntimeTerminated 按 KillResult disposition 确认 runtime 已终止，才可复用会话名与端口。
func (m *Manager) confirmRuntimeTerminated(ctx context.Context, taskID, runtimeName string) error {
	exists, herr := m.proc.HasSession(runtimeName)
	if herr != nil && !errors.Is(herr, process.ErrNoTmuxServer) {
		// G3-15：HasSession infra 错误 = 未确认终止，fail-closed 按 kill_failed/
		// retryable 写 notice：成功 → typed cleanup error（Recovery 终态，MUST NOT
		// 复用固定会话名创建进程）；失败 → 完整 pending。
		cause := newOpErr(codeProcessError, fmt.Errorf("confirm runtime terminated: has session: %w", herr))
		nctx, ncancel := withResidualNoticeCtx(ctx)
		defer ncancel()
		nerr := m.recordResidualNotice(nctx, taskID, runtimeName, nil, noticeReasonKillFailed, true)
		if nerr != nil {
			return recordNoticeOrPending(runtimeName, nil, noticeReasonKillFailed, true, nerr, cause)
		}
		return &retryableCleanupError{err: cause}
	}
	if !exists {
		return nil
	}
	res, kerr := m.proc.KillSession(runtimeName)
	nctx, ncancel := withResidualNoticeCtx(ctx)
	defer ncancel()
	if kerr != nil {
		nerr := m.recordResidualNotice(nctx, taskID, runtimeName, res.CleanupTickets, noticeReasonKillFailed, true)
		cause := newOpErr(codeProcessError, fmt.Errorf("confirm runtime terminated: kill: %w", kerr))
		if nerr != nil {
			return recordNoticeOrPending(runtimeName, res.CleanupTickets, noticeReasonKillFailed, true, nerr, cause)
		}
		// G3-2/G3-5：kill infra 错误（notice 已落库）→ retryable cleanup debt 在位，
		// Recovery MUST NOT 再创建进程（typed 终态；Activate 行为不变）。
		return &retryableCleanupError{err: cause}
	}
	cls := classifyKillResult(res)
	switch cls.action {
	case "none":
		return nil
	case "continue":
		nerr := m.recordResidualNotice(nctx, taskID, runtimeName, res.CleanupTickets, cls.reason, cls.retryable)
		if nerr != nil {
			cause := newOpErr(codeProcessError, fmt.Errorf("confirm runtime terminated: disposition %s", res.Disposition))
			return recordNoticeOrPending(runtimeName, res.CleanupTickets, cls.reason, cls.retryable, nerr, cause)
		}
		return nil
	default:
		nerr := m.recordResidualNotice(nctx, taskID, runtimeName, res.CleanupTickets, cls.reason, cls.retryable)
		cause := newOpErr(codeProcessError, fmt.Errorf("confirm runtime terminated: disposition %s", res.Disposition))
		if nerr != nil {
			return recordNoticeOrPending(runtimeName, res.CleanupTickets, cls.reason, cls.retryable, nerr, cause)
		}
		// G3-2/G3-5：retryable/未知矛盾 disposition（notice 已落库）→ Recovery MUST
		// NOT 再创建进程（typed 终态；Activate 行为不变）。
		return &retryableCleanupError{err: cause}
	}
}

// watchServeExit 监视 serve 会话消失 → 完整清理运行时 → suspended + last_error（design.md §4）。
// 回调校验 (instVersion, sessionName) 仍匹配 Manager 当前注册表，否则忽略（B4 回调隔离：
// 旧实例回调不清理新实例）。校验针对 m.getRuntime 的当前 runtime，而非注册时捕获的 rt，
// 保证旧 runtime 被替换后回调即失效（C1：不捕获本地快照）。
// 事件类型分发（C1 typed RuntimeEvent，single-process D4 统一 runtime failure 分派）：
//   - WatchEventSessionExit → handleServeExit → ensureRecovery（幂等恢复入口）；
//   - WatchEventInfraError（tmux 持续故障）→ ensureRecovery（统一分派，不得静默；
//     恢复前序清理终止该 runtime）。
//
// P1.4.7（design.md D0:150）：注册时捕获触发令牌并贯穿传递给 converge——触发令牌是
// 注册/回调校验时刻的身份，不得在等锁后重读 runtime 顶替。
func (m *Manager) watchServeExit(taskID, serveName string) {
	tok := runtime.InstVersion("")
	if rt := m.getRuntime(taskID); rt != nil {
		tok = rt.instVersion
	}
	cancel, done := m.proc.WatchExit(serveName, func(ev process.WatchEvent) {
		// 校验当前 runtime 注册表（非注册时捕获的 rt）。
		cur := m.getRuntime(taskID)
		if cur == nil || !cur.matchesRegistry(tok, serveName) {
			return
		}
		switch ev.Type {
		case process.WatchEventSessionExit:
			m.handleServeExit(taskID, tok)
		case process.WatchEventInfraError:
			m.ensureRecovery(taskID, tok)
		}
	})
	if cur := m.getRuntime(taskID); cur != nil {
		cur.mu.Lock()
		cur.watchCancels[serveName] = cancel
		cur.watchDones[serveName] = done
		cur.mu.Unlock()
	}
}

// handleServeExit serve 异常消失（非挂起路径）→ 完整清理运行时 → suspended + last_error。
// 清除 env snapshot 与 shell（design.md §2/§4：serve 异常退出清快照）。
// P1.4.7：tok 为 watcher 注册时捕获的触发令牌（design.md D0:150 令牌贯穿）。
// single-process Phase 4：watchTUIExit 已随独立 TUI 会话模型删除（TUI 与任务进程
// 同体，-tui 遗留会话仅由 reconcile 清理路径处置）。
func (m *Manager) handleServeExit(taskID string, tok runtime.InstVersion) {
	m.ensureRecovery(taskID, tok)
}

// convergeToSuspended 收敛活跃任务到 suspended（design.md §4：不得留 active 但无 SSE 托管的运行时）。
// single-process D4 起 watcher/SSE 永久失败统一走幂等 ensureRecovery（不再直接落挂起），
// 本收敛族保留为显式收敛原语（D2 attention 失效矩阵入口；converge_debt 测试直接消费）——
// 令牌校验在 convergeToSuspendedChecked 内统一完成（拿锁后按触发令牌比对当前 runtime）。
func (m *Manager) convergeToSuspended(taskID, reason string, tok runtime.InstVersion) {
	m.convergeToSuspendedChecked(taskID, reason, tok)
}

// convergeToSuspendedForGen 同 convergeToSuspended，SSE 路径专用：以触发事件的
// instVersion 进入统一收敛入口（P1.4.9：原 (generation, instanceID) 双参数收敛）。
func (m *Manager) convergeToSuspendedForGen(taskID, reason string, instVersion runtime.InstVersion) {
	m.convergeToSuspendedChecked(taskID, reason, instVersion)
}

// convergeToSuspendedChecked 统一收敛入口（watcher/SSE 路径一致携带触发令牌，
// design.md D0:150）。拿锁后校验当前 runtime 令牌仍等于触发令牌——旧实例延迟回调
// （等锁期间任务被 Suspend→重新 Activate 换代）MUST NOT 清理新实例 runtime（design.md §2
// 令牌隔离）。锁等待超时不再无锁清理/CAS（design.md D0:151 替换行为）：仅按触发令牌
// 登记两阶段债务，由 backgroundLoop worker 持锁消化（converge_debt.go）。
func (m *Manager) convergeToSuspendedChecked(taskID, reason string, tok runtime.InstVersion) {
	unlock, err := m.lockTaskForConverge(taskID)
	if err != nil {
		m.onConvergeLockTimeout(taskID, reason, tok)
		return
	}
	defer unlock()
	// 触发令牌隔离（统一原 SSE checkGen 路径与 watcher 路径）：当前 runtime 为 nil 或令牌
	// 不匹配 → 旧实例延迟回调，跳过收敛（不清理新实例/已清 runtime）。
	rt := m.getRuntime(taskID)
	if rt == nil || rt.instVersion != tok {
		log.Printf("convergeToSuspended: stale token callback (task %s instVersion=%s) skipped", taskID, tok)
		return
	}
	// 清理前捕获 attention 外部可见快照存在性（design.md D2「清理前捕获外部可见状态」；
	// cleanup 会经 clearRuntime→clearAttention 清空快照，事后不可读），供 CAS 矩阵
	// ②/③a/③c 的 attention 失效发布判定。run_status 失效为「捕获即 apply」
	//（design.md:426）：单次唯一 apply 内读取投影、置失效并返回 typed delta（事件
	// from 与事实号只来自该 delta），供矩阵发布。发布决策在发布点原子 claim
	//（ClaimAttentionInvalidation/ClaimRunStatusInvalidation，per-aspect/per-fact marker）：
	// 本捕获与矩阵发布之间的长清理窗口内，无需任务锁的超时回调可能已认领发布同一
	// 事实（TOCTOU），claim 失败即被抑制。
	attentionVisible := m.attentionVisible(rt)
	runStatusInvalidation := rt.invalidateAgentStatus()
	ctx := context.Background()
	// 停 SSE + kill tui/shell 会话。notice 持久化失败聚合进 last_error（不静默，design.md §8）。
	// cleanupActivationRuntime 内部 fail-closed 记 retryable notice（P4 复评阻塞 5）。
	cleanupErr := m.cleanupActivationRuntime(ctx, taskID)
	// 清除 env 快照 + active→suspended CAS + D2 嵌套决策表（converge_debt.go）。
	// env 快照写回错误聚合进 last_error，不静默吞错（P4 复评阻塞 5）。
	m.convergeCommitCAS(ctx, taskID, reason, tok, attentionVisible, runStatusInvalidation, cleanupErr)
}

// cleanupActivationRuntime 清理激活过程中已建的会话（失败补偿）。
// 既有签名保留：返回聚合 error；其余三调用点（锁超时/主动收敛/reconcile）行为不变。
// Activate 失败路径用 cleanupActivationRuntimeCollect 收集 observations 供 pending 回放。
func (m *Manager) cleanupActivationRuntime(ctx context.Context, taskID string) error {
	err, _ := m.cleanupActivationRuntimeCollect(ctx, taskID)
	return err
}

// cleanupActivationRuntimeCollect 同 cleanupActivationRuntime，并返回每条 cleanup notice intent
// 的 observation（design.md D2）。clean 不形成 observation；未知/矛盾 disposition 经共享
// classifyKillResult fail-closed 记 kill_failed（MUST NOT 经 dispositionToNotice 静默忽略）。
func (m *Manager) cleanupActivationRuntimeCollect(ctx context.Context, taskID string) (error, []cleanupObservation) {
	rt := m.getRuntime(taskID)
	if rt != nil {
		rt.stopAll()
	}
	// kill 已存在的 serve/tui/shell 会话（best-effort，记录残留 notice）。
	// B7b：shell 枚举 ListSessions 错误 MUST 传播（不得静默吞错）。
	shellNames, err := m.listShellSessions(taskID)
	if err != nil {
		m.clearRuntime(taskID)
		return fmt.Errorf("enumerate shells for cleanup activation runtime: %w", err), nil
	}
	names := append([]string{runtimeSessionName(taskID), serveSessionName(taskID), tuiSessionName(taskID)}, shellNames...)
	var noticeErrs []error
	var observations []cleanupObservation
	recordObs := func(name, reason string, retryable bool, tickets []string, nerr error) {
		observations = append(observations, cleanupObservation{
			sessionName:    name,
			reason:         reason,
			retryable:      retryable,
			cleanupTickets: append([]string(nil), tickets...),
			persisted:      nerr == nil,
		})
	}
	for _, name := range names {
		// P4 复评阻塞 5：HasSession infra 错误 fail-closed——记 retryable notice（kill_failed），
		// 不得 _ = 吞错当 absent 跳过（残留会话下次 Activate 被门禁阻塞）。
		exists, herr := m.proc.HasSession(name)
		if herr != nil && !errors.Is(herr, process.ErrNoTmuxServer) {
			nerr := m.recordResidualNotice(ctx, taskID, name, nil, noticeReasonKillFailed, true)
			recordObs(name, noticeReasonKillFailed, true, nil, nerr)
			if nerr != nil {
				noticeErrs = append(noticeErrs, fmt.Errorf("has session %s: %w; record notice: %v", name, herr, nerr))
			} else {
				noticeErrs = append(noticeErrs, fmt.Errorf("has session %s: %w", name, herr))
			}
			continue
		}
		if !exists {
			continue
		}
		// KillSession infra 错误同样 fail-closed 记 retryable notice。
		res, kerr := m.proc.KillSession(name)
		if kerr != nil {
			nerr := m.recordResidualNotice(ctx, taskID, name, res.CleanupTickets, noticeReasonKillFailed, true)
			recordObs(name, noticeReasonKillFailed, true, res.CleanupTickets, nerr)
			if nerr != nil {
				noticeErrs = append(noticeErrs, fmt.Errorf("kill session %s: %w; record notice: %v", name, kerr, nerr))
			} else {
				noticeErrs = append(noticeErrs, fmt.Errorf("kill session %s: %w", name, kerr))
			}
			continue
		}
		cls := classifyKillResult(res)
		if cls.action == "none" {
			// clean：无 notice intent，不形成 observation、不取消既有 pending。
			continue
		}
		nerr := m.recordResidualNotice(ctx, taskID, name, res.CleanupTickets, cls.reason, cls.retryable)
		recordObs(name, cls.reason, cls.retryable, res.CleanupTickets, nerr)
		if nerr != nil {
			noticeErrs = append(noticeErrs, nerr)
		}
	}
	m.clearRuntime(taskID)
	return errors.Join(noticeErrs...), observations
}

// checkNoResidualSessions 检查 tmux ls 中是否仍有该任务会话（design.md §19 Activate 前置）。
func (m *Manager) checkNoResidualSessions(ctx context.Context, taskID string) error {
	sessions, err := m.proc.ListSessions()
	if err != nil {
		return newOpErr(codeInternal, err)
	}
	for _, s := range sessions {
		if taskIDFromSessionName(s) == taskID {
			return newOpErr(codeConflict, fmt.Errorf("task %s has residual sessions; clean up or force-delete first", taskID))
		}
	}
	return nil
}

// copyMap 复制 env map。
func copyMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// nowUnixI 返回当前 Unix 秒。
func nowUnixI() int64 { return time.Now().Unix() }

// isOverflow 判断是否为 session 列表溢出错误。
func isOverflow(err error) bool {
	return err != nil && err.Error() != "" && contains(err.Error(), "overflow")
}

// contains 简化 strings.Contains（避免多处 import）。
func contains(s, sub string) bool { return strings.Contains(s, sub) }

// isSessionNotFound 判断 occlient GetSession 是否返回 session 不存在。
func isSessionNotFound(err error) bool {
	return err != nil && contains(err.Error(), "session not found")
}
