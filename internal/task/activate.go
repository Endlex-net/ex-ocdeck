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

	"ocdeck/internal/opencode"
	"ocdeck/internal/process"
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
		return nil, err
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
// 返回可用端口；范围耗尽返回错误。轮转游标避免每次从头扫（B5）。
// portCursor 记录上次分配位置，下次从此处后轮转，降低并行 Activate 选同端口概率。
func (m *Manager) allocatePort(lastPort sql.NullInt64) (int, error) {
	pr := m.cfg.ServePortRange
	// 先试 last_port。
	if lastPort.Valid {
		p := int(lastPort.Int64)
		if p >= pr.Min && p <= pr.Max && isPortFree(p) {
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
		if isPortFree(p) {
			m.portCursorMu.Lock()
			m.portCursor = p
			m.portCursorMu.Unlock()
			return p, nil
		}
	}
	return 0, fmt.Errorf("serve port range %d-%d exhausted", pr.Min, pr.Max)
}

// isPortFree 探测端口是否可用（bind 后立即释放）。
func isPortFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// Activate 激活任务（design.md §19 Activate 行 + §2/§3/§4/§11）。
// 前置检查 → 置 activating → 分配端口+合并 env → NewSession(serve) → 健康检查+Probe
// → SSE 订阅+全量对齐 → NewSession(tui) → active。
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
	// 前置检查：无未清理的 cleanup debt（任意 retryable=true notice 拒绝激活）。
	if hasRetryable, err := m.hasRetryableNotice(ctx, row); err != nil {
		return newOpErr(codeInternal, err)
	} else if hasRetryable {
		return newOpErr(codeConflict, fmt.Errorf("task %s has uncleaned cleanup debt; resolve or force-delete first", taskID))
	}

	// init_status 门禁（design.md §5，tasks 3.5）：none|succeeded → 放行；
	// pending|running → invalid_state "init in progress"；failed → invalid_state 含 init_error；
	// 未知/空值 → invalid_state fail-closed。
	switch row.InitStatus {
	case InitStatusNone, InitStatusSucceeded:
		// 放行。
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

	// ① 置 activating。
	updated, err := m.store.UpdateTaskStatusConditional(ctx, taskID, StatusSuspended, StatusActivating, sql.NullString{})
	if err != nil {
		return newOpErr(codeInternal, err)
	}
	if !updated {
		return newOpErr(codeConflict, fmt.Errorf("task %s state changed before activate commit", taskID))
	}

	if err := m.activateRun(ctx, taskID, mode); err != nil {
		// 失败：清理已建会话（serve/tui/shell）→ suspended + last_error（design.md §19 补偿）。
		// 补偿 MUST 用脱离调用方取消的 context：activateRun 可能在 probe 退避/健康检查等
		// 步骤因 ctx 取消返回，此时调用方 ctx 已取消，真实 store 用 ExecContext 会因 ctx 取消
		// 失败 → 清快照/状态回退/notice 持久化全部失败且错误被忽略 → 任务卡 activating。
		// 故补偿路径统一用 compCtx（WithoutCancel + 独立短超时），各步错误记录日志但不跳过后续步骤。
		m.runActivateFailureCompensation(ctx, taskID, err)
		return err
	}
	// 提交点：active。提交失败补偿（杀已建会话回 suspended，B5）。
	if err := m.store.UpdateTaskStatus(ctx, taskID, StatusActive, sql.NullString{}); err != nil {
		commitErr := fmt.Errorf("commit active: %w", err)
		m.runActivateFailureCompensation(ctx, taskID, commitErr)
		return newOpErr(codeInternal, err)
	}
	return nil
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
// kill 已建会话 → 清 env 快照 → 状态回退 activating→suspended + last_error。
//
// 补偿用脱离调用方取消的 compCtx（WithoutCancel + activateCompensationTimeout），
// 避免调用方 ctx 已取消时真实 store 的 ExecContext 失败导致任务卡 activating。
// 预算拆分：cleanup 用 compCtx；最终 DB 收敛（清快照 + 状态回退 + last_error）在 cleanup 结束后
// 另起新 bounded context（activateCompensationFinalizeTimeout），保证无论 cleanup 耗时/成败，
// 状态回退总有全新预算。
// 各步错误 MUST 记录日志但不跳过后续补偿步骤（不静默吞错，但保证补偿尽量收敛）。
// last_error 聚合原始错误与 cleanup notice 错误（design.md §8：notice 持久化失败不静默）。
func (m *Manager) runActivateFailureCompensation(reqCtx context.Context, taskID string, cause error) {
	compCtx, cancel := context.WithTimeout(context.WithoutCancel(reqCtx), activateCompensationTimeout)
	defer cancel()

	cleanupErr := m.cleanupActivationRuntime(compCtx, taskID)
	if cleanupErr != nil {
		log.Printf("activate: cleanup runtime for task %s: %v", taskID, cleanupErr)
	}

	// 最终 DB 收敛：另起全新 bounded context，不受 cleanup 耗时影响。
	finalCtx, finalCancel := context.WithTimeout(context.WithoutCancel(reqCtx), activateCompensationFinalizeTimeout)
	defer finalCancel()

	if err := m.store.UpdateTaskEnvSnapshot(finalCtx, taskID, sql.NullString{}); err != nil {
		log.Printf("activate: clear env snapshot for task %s: %v", taskID, err)
	}
	le := sql.NullString{String: cause.Error(), Valid: true}
	if cleanupErr != nil {
		le = sql.NullString{String: fmt.Errorf("%w; cleanup notice: %v", cause, cleanupErr).Error(), Valid: true}
	}
	updated, err := m.store.UpdateTaskStatusConditional(finalCtx, taskID, StatusActivating, StatusSuspended, le)
	if err != nil {
		log.Printf("activate: rollback activating→suspended for task %s: %v", taskID, err)
		return
	}
	if !updated {
		// 无 error 但 updated=false：状态已被并发改动（非 activating），便于诊断卡 activating 场景。
		log.Printf("activate: rollback activating→suspended for task %s: status changed concurrently (not activating)", taskID)
	}
}

// activateRun 执行激活的外部副作用序列（serve → probe → SSE → tui）。
// mode 由 Activate 早期 kind 门禁解析后传入（add-plain-dir-project D8）。
func (m *Manager) activateRun(ctx context.Context, taskID string, mode AlignMode) error {
	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}

	// ② 分配端口、合并 env 快照并持久化。
	port, err := m.allocatePort(row.LastPort)
	if err != nil {
		return newOpErr(codeConflict, err)
	}
	env, err := m.mergeEnvSnapshot(ctx, row, port)
	if err != nil {
		return newOpErr(codeInternal, err)
	}

	// ③ NewSession(serve) + 健康检查：EADDRINUSE 自动换端口重试（design.md §3，B5）。
	// serve 在 tmux 内运行 opencode serve --port；端口被占时进程启动但健康检查不就绪。
	// 重试：换新端口 + 重新合并 env（OCDECK_SERVE_PORT 变）+ 重建 serve 会话。
	password := newRandomPassword()
	serveName := serveSessionName(taskID)
	port, err = m.startServeWithPortRetry(ctx, row, serveName, port, password, env)
	if err != nil {
		return err
	}
	// 端口写回 DB（仅记录，非事实来源，design.md §3）。B7b：写入错误不得忽略。
	if err := m.store.UpdateTaskLastPort(ctx, taskID, port); err != nil {
		// 端口写回失败非致命（last_port 仅交叉校验），但不得静默吞错。
		// serve 已起在 port，继续后续流程；记录日志供运维感知。
		log.Printf("activate: update last port for task %s: %v (serve running on %d)", taskID, err, port)
	}

	// ⑤ SSE 订阅（onReady 等待建立 → 全量对齐 → 事件 upsert）。
	// 注册前校验 serve 会话仍存活（C2：避免注册已被 tmux 回收的会话造成孤儿注册表；
	// serve 在 Probe 后、注册前可能崩溃，此时清已建会话回 suspended）。
	if alive, _ := m.proc.HasSession(serveName); !alive {
		_, _ = m.proc.KillSession(serveName) // best-effort 清理
		return newOpErr(codeProcessError, fmt.Errorf("serve session gone before runtime register"))
	}
	rt := m.newRuntime(taskID)
	m.setRuntime(taskID, rt)
	// 注册 serve group（B4：groups 真实写入注册表）。
	rt.registerGroup("serve", serveName)
	if err := m.startSSE(ctx, rt, taskID, row.WorktreePath, port, password, mode); err != nil {
		return newOpErr(codeProcessError, fmt.Errorf("sse subscribe: %w", err))
	}

	// ⑥ NewSession(tui)：opencode attach --session（无记录或 404 时先经 REST 创建会话锚定，§4）。
	if err := m.startTUI(ctx, row, port, password, env); err != nil {
		return newOpErr(codeProcessError, fmt.Errorf("tui session: %w", err))
	}
	// 注册 tui group（B4）。
	rt.registerGroup("tui", tuiSessionName(taskID))

	// 退出监视：serve 异常消失 → 完整清理 → suspended（design.md §4）。
	m.watchServeExit(taskID, serveName)
	// TUI 消失 serve 存活 → 标记可重开（保持活跃）。
	m.watchTUIExit(taskID, tuiSessionName(taskID))
	return nil
}

// waitServeReady 轮询 health 直到就绪或超时。
func (m *Manager) waitServeReady(ctx context.Context, oc OCClient) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		h, err := oc.Health(ctx)
		if err == nil && h.Healthy {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("health check timeout")
}

// waitServeReadyOrDead 在 waitServeReady 基础上每轮先判定 serve 会话进程是否已死
// （design.md §3/§19 E3：serve 崩溃后不得等满超时）。仅用于 Activate 重试路径
// （delete.go 的临时 serve 仍用 waitServeReady，保持其调用不变）。
func (m *Manager) waitServeReadyOrDead(ctx context.Context, oc OCClient, serveName string) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		// 先判进程死亡：已死则立即终止轮询（不等满超时）。
		alive, derr := m.proc.HasSession(serveName)
		if derr == nil && !alive {
			return fmt.Errorf("serve session died before ready: %s", serveName)
		}
		h, err := oc.Health(ctx)
		if err == nil && h.Healthy {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("health check timeout")
}

// probeErrToOpCode 按 design.md §11/§21 将 Probe 返回的 sentinel 错误映射为 OpError code：
//   - ErrServeNotReady（网络/超时/serve 未就绪）→ process_error（可重试）
//   - ErrCapabilityMismatch（结构不兼容）→ oc_incompatible（激活门禁拒绝）
//   - ErrUnauthorized（401，Basic Auth 凭据错误）→ internal（内部 bug）
//
// Probe 内部已用 classifyProbeErr 归类为这些 sentinel；此处用 errors.Is 兼容 wrap（%w）。
func probeErrToOpCode(err error) (string, error) {
	switch {
	case errors.Is(err, opencode.ErrCapabilityMismatch):
		return codeOCIncompatible, fmt.Errorf("capability probe: %w", err)
	case errors.Is(err, opencode.ErrUnauthorized):
		return codeInternal, fmt.Errorf("capability probe (unauthorized, internal bug): %w", err)
	case errors.Is(err, opencode.ErrServeNotReady):
		return codeProcessError, fmt.Errorf("capability probe (serve not ready): %w", err)
	default:
		// 未知错误保守按 process_error（可重试），避免误判为不可恢复。
		return codeProcessError, fmt.Errorf("capability probe: %w", err)
	}
}

// servePortRetries EADDRINUSE 换端口重试上限（design.md §3，B5）。
const servePortRetries = 3

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

// startServeWithPortRetry 创建 serve 会话并健康检查；EADDRINUSE 自动换端口重试（design.md §3，B5）。
// 端口被占时 serve 进程启动但健康检查不就绪 → 换新端口重新合并 env + 重建 serve 会话。
// 端口变更时 MUST 同步三处 OCDECK_SERVE_PORT（design.md §3 E1，§2 line 68）：
//   - 内存 env map（传给后续 startTUI）
//   - 持久化 tasks.env_snapshot（UpdateTaskEnvSnapshot）
//   - 新建 serve 会话环境（serveEnv）
//
// 重建 serve 会话前 MUST 先 kill 旧会话（不允许 "serve 旧端口 / TUI 新端口"）。
// Probe 错误按 §11 分类（probeErrToOpCode），非全部 oc_incompatible。
// 返回最终可用端口。
func (m *Manager) startServeWithPortRetry(ctx context.Context, row TaskRow, serveName string, port int, password string, env map[string]string) (int, error) {
	for attempt := 0; attempt < servePortRetries; attempt++ {
		serveEnv := copyMap(env)
		serveEnv["OPENCODE_SERVER_PASSWORD"] = password
		serveEnv["OCDECK_SERVE_PORT"] = strconv.Itoa(port)
		if err := m.proc.NewSession(process.SessionSpec{
			Name:    serveName,
			Dir:     row.WorktreePath,
			Env:     serveEnv,
			CmdArgv: []string{"opencode", "serve", "--port", strconv.Itoa(port), "--hostname", "127.0.0.1"},
		}); err != nil {
			return port, newOpErr(codeProcessError, fmt.Errorf("serve session: %w", err))
		}
		oc := m.ocFactory(port, password, opencode.Options{
			HealthTimeout: 2 * time.Second,
			OpTimeout:     10 * time.Second,
		})
		// E3：健康轮询前判进程死亡，serve 崩溃立即失败（不等满超时）。
		if err := m.waitServeReadyOrDead(ctx, oc, serveName); err != nil {
			// E1：先 kill 旧 serve 会话（不允许 serve 旧端口 / TUI 新端口），再换端口。
			_, _ = m.proc.KillSession(serveName)
			newPort, aerr := m.allocatePort(sql.NullInt64{Int64: int64(port), Valid: true})
			if aerr != nil {
				return port, newOpErr(codeConflict, fmt.Errorf("serve not ready and no free port: %w", err))
			}
			if newPort == port {
				// 无新端口可换 → 真正的就绪失败。
				return port, newOpErr(codeProcessError, fmt.Errorf("serve not ready: %w", err))
			}
			port = newPort
			// E1：同步三处 OCDECK_SERVE_PORT（内存 env + 持久化快照，design.md §2 line 68）。
			env["OCDECK_SERVE_PORT"] = strconv.Itoa(port)
			if perr := m.persistEnvSnapshot(ctx, row.ID, env); perr != nil {
				return port, newOpErr(codeInternal, fmt.Errorf("persist env snapshot on port change: %w", perr))
			}
			continue
		}
		// 健康就绪 → 能力探测。
		// D8：全新 worktree 是 opencode 冷项目，serve 启动后首次 /session/status 可超 10s
		//（实测 7.3s+，负载下超时）→ Probe 归类 ErrServeNotReady。此时 serve 进程仍健康，
		// 仅能力端点未就绪，故不 kill 会话，短退避重试（首次 + 2 次，退避 2s/4s）。
		// 首次超时后服务端初始化已完成，二次命中热路径。其他错误（mismatch/unauthorized/未知）
		// 立即按 probeErrToOpCode 失败并 kill 会话。
		if err := m.probeWithColdStartRetry(ctx, oc); err != nil {
			_, _ = m.proc.KillSession(serveName)
			code, ferr := probeErrToOpCode(err)
			return port, newOpErr(code, ferr)
		}
		return port, nil
	}
	return port, newOpErr(codeProcessError, fmt.Errorf("serve not ready after %d port retries", servePortRetries))
}

// persistEnvSnapshot 将已合并的 env map 持久化为 tasks.env_snapshot（不含密码）。
// 端口变更重试时复用 mergeEnvSnapshot 的持久化路径，保证快照与新端口一致（E1）。
func (m *Manager) persistEnvSnapshot(ctx context.Context, taskID string, merged map[string]string) error {
	snap := envSnapshot{Vars: merged}
	b, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal env snapshot: %w", err)
	}
	if err := m.store.UpdateTaskEnvSnapshot(ctx, taskID, sql.NullString{String: string(b), Valid: true}); err != nil {
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
	ocWithReady := m.ocFactory(port, password, opencode.Options{
		HealthTimeout:    2 * time.Second,
		HeartbeatTimeout: 60 * time.Second,
		OnReady: func() {
			select {
			case readyCh <- struct{}{}:
			default:
			}
		},
	})

	// 缓冲屏障：对齐完成前 SSE 事件缓冲；对齐完成后放行（design.md §4）。
	var bufMu sync.Mutex
	buffered := []opencode.Event{}
	buffering := true

	// align 串行化屏障（design.md §4）：任一时刻最多一个 align 在执行（首次 align + reconnect align 互斥）。
	// 首次 align 进行中若断流，onReconnect MUST 排队等待（合并为一次重对齐），MUST NOT 并发清空 buffered
	// 造成事件丢失/乱序。等待期间 buffering 保持 true，事件继续缓冲；待 align 释放后 reconnect 重新全量对齐
	//（丢弃半成品状态安全，AlignSessions 幂等 upsert）。
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
					go m.convergeToSuspendedForGen(taskID, "sse replay session store error: "+err.Error(), rt.generation, rt.instanceID)
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
			// design.md §4/§19）。在独立 goroutine 收敛：cleanup 会 kill serve 结束本 ctx，
			// 需避免在 SSE goroutine 内 join/cancel 造成死锁。
			if err := m.handleSSEEvent(sseCtx, taskID, wtPath, ev); err != nil {
				go m.convergeToSuspendedForGen(taskID, "sse session store error: "+err.Error(), rt.generation, rt.instanceID)
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
			if err := m.alignSessions(sseCtx, taskID, wtPath, ocWithReady, mode); err != nil {
				// 重连对齐失败 MUST 收敛任务状态（design.md §4）：不得只取消 SSE 留 active 假象。
				// serve 可能仍存活但无法追踪会话，视同运行时不可确定 → cleanup runtime + suspended + last_error。
				// 在新 goroutine 收敛：onReconnect 在 SSE goroutine 内，cleanup 会 kill serve（结束本 goroutine ctx），
				// 需避免在自身 goroutine 内 join/cancel 造成死锁。
				cancel()
				go m.convergeToSuspendedForGen(taskID, "sse reconnect align failed: "+err.Error(), rt.generation, rt.instanceID)
				return
			}
			// D6 注意力对账（align 路径）：session align 成功后、drainAndRelease 前。失败不影响任务状态机。
			m.reconcileTaskAttention(sseCtx, rt, ocWithReady, wtPath)
			// B4b：先排空全部缓冲事件再置 buffering=false 放行实时事件（design.md §4）。
			drainAndRelease()
		}
		// B4c：SubscribeEvents 永久返回 MUST 有处理路径（converge suspended + last_error）。
		// 返回非 nil 或正常返回（流结束）都意味着 SSE 不再托管 → 不得留 active 无 SSE 假象。
		sseErr := ocWithReady.SubscribeEvents(sseCtx, wtPath, onEvent, onReconnect)
		if sseErr != nil && sseErr != context.Canceled && !errors.Is(sseErr, context.Canceled) {
			// SSE 流异常结束（非主动 cancel）：在新 goroutine 收敛（cleanup 会 kill serve 结束本 ctx）。
			go m.convergeToSuspendedForGen(taskID, "sse stream ended: "+sseErr.Error(), rt.generation, rt.instanceID)
			return
		}
		// 正常返回（流结束/ctx 取消）：若 ctx 未被主动 cancel（即非 Activate/Shutdown 主动停 SSE），
		// serve 仍存活但 SSE 流终止 → 收敛。sseCtx 被 cancel 的情况由 Activate 返回路径/Shutdown 处理，不在此收敛。
		if sseCtx.Err() == nil {
			go m.convergeToSuspendedForGen(taskID, "sse stream ended (serve may still be alive)", rt.generation, rt.instanceID)
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
			// 未归属 session 的 updated 一律忽略（updated=false 正常路径，记 debug 不报错）。
			updatedTS, hasTS := ev.TimeUpdated()
			if !hasTS {
				return nil
			}
			updated, uerr := m.store.TouchOwnedTaskSession(ctx, taskID, sid, int64(updatedTS))
			if uerr != nil {
				return fmt.Errorf("touch owned session %s: %w", sid, uerr)
			}
			if !updated {
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
		claimed, owner, cerr := m.store.ClaimTaskSession(ctx, taskID, sid, createdAt, nowUnixI(), lastSeen, ev.ParentID())
		if cerr != nil {
			return fmt.Errorf("claim session %s: %w", sid, cerr)
		}
		if !claimed {
			log.Printf("task %s: sse session.created for session %s conflict (owned by task %s); skipping",
				taskID, sid, owner)
		}
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
		if err := m.store.DeleteTaskSession(ctx, taskID, sid); err != nil {
			return fmt.Errorf("delete session %s: %w", sid, err)
		}
	default:
		// D6 注意力事件：permission/question v1+v2 家族。ParseAttentionEvent 命中则应用，
		// 未知/缺字段静默忽略。注意力事件处理永不返回错误（不影响任务状态机）。
		if aev, ok := opencode.ParseAttentionEvent(ev); ok {
			if rt := m.getRuntime(taskID); rt != nil {
				rt.applyAttentionEvent(aev)
			}
			return nil
		}
		// B3b：status/diff 等无 properties.info 的事件，sessionID 取 properties.sessionID
		//（VERIFICATION.md 实测，回退 properties.info.id），反查 task_sessions 归属，
		// 命中本任务才处理（design.md §4 补注）。未命中本任务的 session 不动本任务数据。
		sid := ev.SessionIDProp()
		if sid == "" {
			return nil
		}
		owns, err := m.sessionBelongsToTask(ctx, taskID, sid)
		if err != nil {
			return fmt.Errorf("check session ownership %s: %w", sid, err)
		}
		if !owns {
			return nil
		}
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
// 冲突 ID 经 store 层上报（此处仅传播 error，冲突 session 在 store 内被跳过）。
// mode=AlignModeOwnedOnly（dir）：仅对 listed∩owned 刷新 last_seen_at（store 层处理），绝不 claim。
// complete/overflow 判定 MUST 基于原始目录列表（过滤之前，D8 算法第 1 步）。
//
// 返回 error（不吞，B5）：非 overflow 错误传播；AlignTaskSessions store 错误传播。
func (m *Manager) alignSessions(ctx context.Context, taskID, wtPath string, oc OCClient, mode AlignMode) error {
	sessions, err := oc.ListSessions(ctx, wtPath, 1000)
	complete := true
	if err != nil {
		// 溢出（count==limit）仅刷新不删缺席行，写 session_overflow notice（B5）。
		if isOverflow(err) {
			complete = false
			if oerr := m.recordSessionOverflowNotice(ctx, taskID); oerr != nil {
				// notice 写入错误 MUST 处理（聚合，不静默，design.md §8）。
				return fmt.Errorf("session overflow notice: %w", oerr)
			}
		} else {
			return fmt.Errorf("list sessions: %w", err)
		}
	}
	obs := make([]SessionObservation, 0, len(sessions))
	for _, s := range sessions {
		obs = append(obs, SessionObservation{
			SessionID: s.ID,
			CreatedAt: int64(s.Time.Created),
			UpdatedAt: int64(s.Time.Updated),
			ParentID:  s.ParentID,
		})
	}
	// 完整对齐时清除 session_overflow notice（design.md §4）。
	var noticeFn func(sql.NullString) sql.NullString
	if complete {
		noticeFn = func(cur sql.NullString) sql.NullString {
			entries, perr := parseNotices(cur)
			if perr != nil {
				return cur // 损坏不动
			}
			out := entries[:0]
			for _, e := range entries {
				if e.Code != noticeCodeSessionOverflow {
					out = append(out, e)
				}
			}
			return encodeNotices(out)
		}
	}
	conflicts, aerr := m.store.AlignTaskSessions(ctx, taskID, mode, obs, complete, noticeFn)
	if aerr != nil {
		return aerr
	}
	// repo/dir 对齐路径冲突 → 忽略 + 记诊断日志（不阻断，D8）。dir 模式无 claim 故无冲突；
	// repo 单任务场景 guard 不命中，conflicts 为空。
	for _, sid := range conflicts {
		log.Printf("task %s: align session %s conflict (owned by other task); skipping", taskID, sid)
	}
	return nil
}

// startTUI 锚定确定 session 并创建 TUI 会话（design.md §4 恢复与锚定）。
// 激活 MUST 立即锚定确定 session，不使用 --continue（其"目录最近会话"语义不等于本任务会话）。
//  1. 有记录（task_sessions 最近 sessionID）→ GetSession 预检：存在 → attach --session <id>；
//     404 → 走创建路径；其他错误 → 激活失败。
//  2. 无记录或预检 404 → CreateSession(dir, title=任务名) → 持久化 task_sessions
//     （upsert：session_id、session_created_at=time.created、first_seen_at/last_seen_at=time.updated，
//     与 SSE 对齐语义一致）→ attach --session <newID>。
//  3. 持久化失败（CreateSession 已成功但 task_sessions 写入失败）→ 聚合错误并激活失败
//     （不留"session 已建但无归属记录"的不一致；已建 session 可经后续全量对齐补记，不算泄漏）。
func (m *Manager) startTUI(ctx context.Context, row TaskRow, port int, password string, env map[string]string) error {
	tuiEnv := copyMap(env)
	tuiEnv["OPENCODE_SERVER_PASSWORD"] = password
	tuiName := tuiSessionName(row.ID)

	oc := m.ocFactory(port, password, opencode.Options{HealthTimeout: 2 * time.Second, OpTimeout: 5 * time.Second})

	// 锚定 sessionID：有记录 → 预检存在；无记录或 404 → 创建并持久化。
	sessionID, err := m.resolveAnchorSession(ctx, oc, row)
	if err != nil {
		return err
	}

	cmdArgv := []string{"opencode", "attach", "http://127.0.0.1:" + strconv.Itoa(port), "--session", sessionID}
	return m.proc.NewSession(process.SessionSpec{
		Name:    tuiName,
		Dir:     row.WorktreePath,
		Env:     tuiEnv,
		CmdArgv: cmdArgv,
	})
}

// resolveAnchorSession 解析 TUI attach 锚定的 sessionID（design.md §4 锚定）。
// 有记录 → GetSession 预检存在；无记录或预检 404 → CreateSession 并持久化 task_sessions。
// 预检/创建的其他错误、持久化失败均返回错误（激活失败，不回退）。
//
// B3 锚定隔离：候选 MUST 仅取顶层会话（ListTopLevelTaskSessions，parent_id 为空），
// 杜绝锚定到 background subagent 子会话（子 session last_seen 更晚时会排到首项，
// 导致 attach 锚定到子会话而非用户主会话）。
func (m *Manager) resolveAnchorSession(ctx context.Context, oc OCClient, row TaskRow) (string, error) {
	// R7 failpoint：ListTopLevelTaskSessions 错误 MUST 传播（不得当空集继续，
	// 否则丢失既有 session 归属 → 用户会话状态错误恢复，design.md §19 failpoint 表）。
	sessions, serr := m.store.ListTopLevelTaskSessions(ctx, row.ID)
	if serr != nil {
		return "", fmt.Errorf("start tui: list top-level sessions: %w", serr)
	}

	if len(sessions) > 0 {
		recentID := sessions[0].SessionID
		_, gerr := oc.GetSession(ctx, row.WorktreePath, recentID)
		if gerr == nil {
			return recentID, nil
		}
		if isSessionNotFound(gerr) {
			// 404 → 走创建路径。
		} else {
			// 其他错误 → 激活失败（不回退）。
			return "", fmt.Errorf("session precheck: %w", gerr)
		}
	}

	// 无记录或预检 404 → 创建新会话并锚定（design.md §4）。新建的是顶层会话（无 parent）。
	created, cerr := oc.CreateSession(ctx, row.WorktreePath, row.Name)
	if cerr != nil {
		return "", fmt.Errorf("create anchor session: %w", cerr)
	}
	// D8：原子 claim 归属（锚定创建路径冲突 → 激活失败，MUST NOT attach 不属本任务的 session）。
	// 新建 session 理论上必不冲突，但 claim 冲突（边界）时返回错误以避免归属不一致。
	claimed, owner, perr := m.store.ClaimTaskSession(ctx, row.ID, created.ID,
		int64(created.Time.Created), int64(created.Time.Updated), int64(created.Time.Updated), "")
	if perr != nil {
		// store 错误：已建 session 可经后续全量对齐补记，但当前激活 MUST 失败以避免归属不一致。
		return "", fmt.Errorf("persist anchor session %s: %w", created.ID, perr)
	}
	if !claimed {
		return "", fmt.Errorf("anchor session %s conflict (owned by task %s); MUST NOT attach", created.ID, owner)
	}
	return created.ID, nil
}

// watchServeExit 监视 serve 会话消失 → 完整清理运行时 → suspended + last_error（design.md §4）。
// 回调校验三元组 (generation, sessionName, InstanceID) 仍匹配 Manager 当前注册表，
// 否则忽略（B4 回调隔离：旧代回调不清理新代）。校验针对 m.getRuntime 的当前 runtime，
// 而非注册时捕获的 rt，保证旧代 runtime 被替换后回调即失效（C1：不捕获本地快照）。
// 事件类型分发（C1 typed RuntimeEvent）：
//   - WatchEventSessionExit → handleServeExit（serve_exit 语义）；
//   - WatchEventInfraError（tmux 持续故障）→ handleInfraError（记录 last_error + notice + 收敛运行时，
//     不得静默）。
func (m *Manager) watchServeExit(taskID, serveName string) {
	gen := 0
	inst := ""
	if rt := m.getRuntime(taskID); rt != nil {
		gen = rt.generation
		inst = rt.instanceID
	}
	cancel, done := m.proc.WatchExit(serveName, func(ev process.WatchEvent) {
		// 校验当前 runtime 注册表（非注册时捕获的 rt）。
		cur := m.getRuntime(taskID)
		if cur == nil || !cur.matchesRegistry(gen, serveName, inst) {
			return
		}
		switch ev.Type {
		case process.WatchEventSessionExit:
			m.handleServeExit(taskID)
		case process.WatchEventInfraError:
			m.handleInfraError(taskID, serveName, ev.Err)
		}
	})
	if cur := m.getRuntime(taskID); cur != nil {
		cur.mu.Lock()
		cur.watchCancels[serveName] = cancel
		cur.watchDones[serveName] = done
		cur.mu.Unlock()
	}
}

// watchTUIExit 监视 TUI 会话消失 → 标记可重开（保持活跃，design.md §4）。
// 回调校验三元组（B4 回调隔离，针对当前 runtime 注册表，C1：不捕获本地快照）。
// 事件类型分发（C1 typed RuntimeEvent）：
//   - WatchEventSessionExit → tui_exit 语义：标记可重开（保持活跃）；
//   - WatchEventInfraError → handleInfraError（TUI 监视 infra 错误同样收敛运行时）。
func (m *Manager) watchTUIExit(taskID, tuiName string) {
	gen := 0
	inst := ""
	if rt := m.getRuntime(taskID); rt != nil {
		gen = rt.generation
		inst = rt.instanceID
	}
	cancel, done := m.proc.WatchExit(tuiName, func(ev process.WatchEvent) {
		cur := m.getRuntime(taskID)
		if cur == nil || !cur.matchesRegistry(gen, tuiName, inst) {
			return
		}
		switch ev.Type {
		case process.WatchEventSessionExit:
			// TUI 消失但 serve 存活 → 标记可重开，保持活跃。
			// 从注册表移除 TUI group；ReopenAttach 由 WS/REST 触发重建。
			cur.removeGroup(tuiName)
		case process.WatchEventInfraError:
			m.handleInfraError(taskID, tuiName, ev.Err)
		}
	})
	if cur := m.getRuntime(taskID); cur != nil {
		cur.mu.Lock()
		cur.watchCancels[tuiName] = cancel
		cur.watchDones[tuiName] = done
		cur.mu.Unlock()
	}
}

// handleServeExit serve 异常消失（非挂起路径）→ 完整清理运行时 → suspended + last_error。
// 清除 env snapshot 与 shell（design.md §2/§4：serve 异常退出清快照）。
func (m *Manager) handleServeExit(taskID string) {
	m.convergeToSuspended(taskID, "serve session exited unexpectedly")
}

// convergeToSuspended 收敛活跃任务到 suspended（design.md §4：不得留 active 但无 SSE 托管的运行时）。
// 非 SSE 路径（serve_exit watcher、handleInfraError）—— gen 校验已由调用方 matchesRegistry 完成，
// 此处不做 gen 隔离校验。
func (m *Manager) convergeToSuspended(taskID, reason string) {
	m.convergeToSuspendedChecked(taskID, reason, 0, "", false)
}

// convergeToSuspendedForGen 同 convergeToSuspended，但携带触发事件的 (generation, instanceID)
// 并做 gen 隔离校验（SSE 路径专用）。拿锁后 MUST 校验与当前 runtime 注册表匹配：旧代延迟错误
// （SSE goroutine 排队等锁期间任务被 Suspend→重新 Activate 换代）MUST NOT 清理新代 runtime
// （design.md §2 三元组隔离）。校验不通过时记录日志并返回（新代 runtime 不受旧代延迟错误影响）。
func (m *Manager) convergeToSuspendedForGen(taskID, reason string, gen int, instID string) {
	m.convergeToSuspendedChecked(taskID, reason, gen, instID, true)
}

func (m *Manager) convergeToSuspendedChecked(taskID, reason string, gen int, instID string, checkGen bool) {
	unlock, err := m.lockTaskForConverge(taskID)
	if err != nil {
		// 锁等待超时：尽力 best-effort 清理（不持锁，仍清残留 + 落 last_error），不静默。
		// gen 校验在无锁路径无法可靠执行（runtime 可能已被清理/替换），降级为 best-effort 收敛。
		ctx := context.Background()
		_ = m.cleanupActivationRuntime(ctx, taskID)
		_ = m.store.UpdateTaskEnvSnapshot(ctx, taskID, sql.NullString{})
		le := sql.NullString{String: fmt.Sprintf("%s; converge lock wait timed out: %v", reason, err), Valid: true}
		_, _ = m.store.UpdateTaskStatusConditional(ctx, taskID, StatusActive, StatusSuspended, le)
		return
	}
	defer unlock()
	// P4 复评阻塞 4：SSE 路径（checkGen=true）校验当前 runtime 仍为触发代（三元组隔离）。
	// 旧代延迟回调（等锁期间 Suspend→重新 Activate）MUST NOT 清理新代 runtime。
	// gen=0 是首代合法值，需与当前 runtime 的 gen 匹配；instID 为本代唯一标识，不匹配即旧代。
	if checkGen {
		rt := m.getRuntime(taskID)
		curGen, curInst := -1, "<nil>"
		if rt != nil {
			curGen, curInst = rt.generation, rt.instanceID
		}
		if rt == nil || curGen != gen || curInst != instID {
			log.Printf("convergeToSuspended: stale gen callback (task %s gen=%d inst=%s) skipped; current gen=%d inst=%s",
				taskID, gen, instID, curGen, curInst)
			return
		}
	}
	ctx := context.Background()
	// 停 SSE + kill tui/shell 会话。notice 持久化失败聚合进 last_error（不静默，design.md §8）。
	// cleanupActivationRuntime 内部 fail-closed 记 retryable notice（P4 复评阻塞 5）。
	cleanupErr := m.cleanupActivationRuntime(ctx, taskID)
	// 清除 env 快照（design.md §2：运行时不可恢复时清快照）。
	// P4 复评阻塞 5：env 快照写回错误聚合进 last_error，不静默吞错。
	envErr := m.store.UpdateTaskEnvSnapshot(ctx, taskID, sql.NullString{})
	le := sql.NullString{String: reason, Valid: true}
	if cleanupErr != nil {
		le = sql.NullString{String: fmt.Sprintf("%s; cleanup notice: %v", reason, cleanupErr), Valid: true}
	}
	if envErr != nil {
		le = sql.NullString{String: fmt.Sprintf("%s; clear env snapshot: %v", le.String, envErr), Valid: true}
	}
	// P4 复评阻塞 5：状态提交失败 MUST NOT 静默——runtime 注册表已由 cleanupActivationRuntime
	// 清除（停 SSE/watcher），但状态未变意味着 DB 仍 active 但无 runtime。记 last_error 让用户/
	// 后台感知；DB 故障下保持 active+last_error（下次 reconcile/converge 再试），不得移除既有 notice。
	committed, statusErr := m.store.UpdateTaskStatusConditional(ctx, taskID, StatusActive, StatusSuspended, le)
	if statusErr != nil {
		log.Printf("convergeToSuspended: commit suspended failed (task %s): %v; last_error=%s", taskID, statusErr, le.String)
	}
	if !committed && statusErr == nil {
		// 状态已非 active（并发 Suspend/Delete 等），无需再落 suspended。
		log.Printf("convergeToSuspended: task %s no longer active (concurrent transition)", taskID)
	}
}

// handleInfraError 处理 tmux 持续基础设施故障（C1：infra_error 明确处理路径，不得静默）。
// 触发来源：WatchExit 连续 tmux 命令失败达到退避上限 → WatchEventInfraError。
// 处理：取得任务锁 → 完整清理运行时（停 SSE/watcher + kill 残余会话，记 residual notice）
// → 记 last_error（含底层 infra 错误）→ 落 suspended。
// 不静默：last_error 与 notice 均落库，供用户与后台周期感知。
func (m *Manager) handleInfraError(taskID, sessionName string, infraErr error) {
	reason := fmt.Sprintf("tmux infra error watching %s: %v", sessionName, infraErr)
	m.convergeToSuspended(taskID, reason)
}

// cleanupActivationRuntime 清理激活过程中已建的会话（失败补偿）。
// kill serve/tui/shell（best-effort，记录残留 notice），停 SSE 与退出监视。
// 返回聚合 error：notice 持久化失败（CAS 不收敛/store 不可达）MUST 传播/聚合，
// 不静默吞错（design.md §8）。调用方（Activate 失败路径）将其纳入 last_error。
// P4 复评阻塞 5：HasSession/KillSession infra 错误 MUST fail-closed 收集为 retryable notice
// （不得 _ = 吞错——残留进程下次 Activate 被门禁永久阻塞，design.md §5/§8）。
func (m *Manager) cleanupActivationRuntime(ctx context.Context, taskID string) error {
	rt := m.getRuntime(taskID)
	if rt != nil {
		rt.stopAll()
	}
	// kill 已存在的 serve/tui/shell 会话（best-effort，记录残留 notice）。
	// B7b：shell 枚举 ListSessions 错误 MUST 传播（不得静默吞错）。
	shellNames, err := m.listShellSessions(taskID)
	if err != nil {
		m.clearRuntime(taskID)
		return fmt.Errorf("enumerate shells for cleanup activation runtime: %w", err)
	}
	names := append([]string{serveSessionName(taskID), tuiSessionName(taskID)}, shellNames...)
	var noticeErrs []error
	for _, name := range names {
		// P4 复评阻塞 5：HasSession infra 错误 fail-closed——记 retryable notice（kill_failed），
		// 不得 _ = 吞错当 absent 跳过（残留会话下次 Activate 被门禁阻塞）。
		exists, herr := m.proc.HasSession(name)
		if herr != nil && !errors.Is(herr, process.ErrNoTmuxServer) {
			if nerr := m.recordResidualNotice(ctx, taskID, name, nil, noticeReasonKillFailed, true); nerr != nil {
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
			if nerr := m.recordResidualNotice(ctx, taskID, name, res.CleanupTickets, noticeReasonKillFailed, true); nerr != nil {
				noticeErrs = append(noticeErrs, fmt.Errorf("kill session %s: %w; record notice: %v", name, kerr, nerr))
			} else {
				noticeErrs = append(noticeErrs, fmt.Errorf("kill session %s: %w", name, kerr))
			}
			continue
		}
		if err := m.recordResidualNoticeFromDisposition(ctx, taskID, name, res); err != nil {
			noticeErrs = append(noticeErrs, err)
		}
	}
	m.clearRuntime(taskID)
	return errors.Join(noticeErrs...)
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
