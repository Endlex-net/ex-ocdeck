package task

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"ocdeck/internal/application/runtime"
	"ocdeck/internal/opencode"
	"ocdeck/internal/process"
	"ocdeck/internal/pty"
)

// ReopenAttach 重开 TUI attach 会话（design.md §18 ReopenAttach + §4 TUI 消失保持活跃）。
// WS 建连与 REST /tasks/:id/attach/reopen 并发幂等：复用同一新 TUI 会话（不产生 409）。
func (m *Manager) ReopenAttach(ctx context.Context, taskID string) (TerminalID, error) {
	unlock, err := m.lockTaskWait(ctx, taskID)
	if err != nil {
		// lockTaskWait 已返回语义化 OpError（conflict/invalid_state/not_found），直接透传，
		// 不再二次包装以免丢失原 code。
		return "", err
	}
	defer unlock()

	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return "", newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
	}
	// lockTaskWait 已在拿锁后复查 active；此处仅为防御性二次校验（持锁期间状态不变）。
	if row.Status != StatusActive {
		return "", newOpErr(codeInvalidState, fmt.Errorf("reopen attach requires active, got %s", row.Status))
	}
	// add-plain-dir-project D8：TUI 重开路径在任何状态修改/运行时副作用前校验项目 kind，
	// 未知值零副作用报错。kind 不改变对齐（ReopenAttach 无对齐），但满足四入口校验一致。
	proj, perr := m.store.GetProject(ctx, row.ProjectID)
	if perr != nil {
		return "", newOpErr(codeNotFound, fmt.Errorf("project gone: %w", perr))
	}
	if _, kerr := alignModeForKind(proj.Kind); kerr != nil {
		// 未知持久化 kind（DB 损坏值）→ internal（D1）。
		return "", newOpErr(codeInternal, kerr)
	}
	tuiName := tuiSessionName(taskID)
	// 已存在则复用（幂等）。HasSession 基础设施错误（非 ErrNoTmuxServer）MUST 传播
	//（不得吞错误当 absent 继续建 TUI，掩盖 infra 故障，design.md §8）。
	exists, herr := m.proc.HasSession(tuiName)
	if herr != nil && !errors.Is(herr, process.ErrNoTmuxServer) {
		return "", newOpErr(codeInternal, fmt.Errorf("reopen attach: has tui session: %w", herr))
	}
	if exists {
		return TerminalID(tuiName), nil
	}
	env, err := m.loadEnvSnapshot(row)
	if err != nil {
		return "", newOpErr(codeInternal, err)
	}
	password := m.recoverPassword(ctx, taskID)
	if password == "" {
		return "", newOpErr(codeProcessError, fmt.Errorf("cannot recover serve password for task %s", taskID))
	}
	// 端口以 serve 会话内 OCDECK_SERVE_PORT 为唯一权威来源（design.md §3/§5：
	// last_port 仅记录，可能写入失败或过期，MUST NOT 回退）。
	// 读回失败/空/不可解析 MUST 直接失败（记 last_error 供调用方感知），不得用 last_port 兜底
	// 致 attach 到错误端口。
	serveName := serveSessionName(taskID)
	portStr, perr := m.proc.ShowSessionEnv(serveName, "OCDECK_SERVE_PORT")
	if perr != nil || portStr == "" {
		return "", newOpErr(codeProcessError, fmt.Errorf("reopen attach: recover serve port: %w", perr))
	}
	port, ok := parsePort(portStr)
	if !ok {
		return "", newOpErr(codeProcessError, fmt.Errorf("reopen attach: invalid serve port %q", portStr))
	}
	tuiEnv := copyMap(env)
	tuiEnv["OPENCODE_SERVER_PASSWORD"] = password
	// 锚定确定 session（design.md §4：不使用 --continue，经 REST 预检/创建后 --session）。
	oc := m.ocFactory(port, password, opencode.Options{HealthTimeout: 2 * time.Second, OpTimeout: 5 * time.Second})
	sessionID, aerr := m.resolveAnchorSession(ctx, oc, row)
	if aerr != nil {
		// add-plain-dir-project D8：锚定 claim 冲突 → 记 last_error，任务保持 active 不收敛
		//（TUI 重开失败可重试），MUST NOT attach 不属本任务的 session。
		// UpdateTaskStatus 写 last_error 同时保持 status=active（不收敛状态）。
		le := sql.NullString{String: fmt.Sprintf("reopen attach: %v", aerr), Valid: true}
		if _, uerr := m.writeStatus(ctx, taskID, StatusActive, le); uerr != nil {
			log.Printf("reopen attach: record last_error for task %s: %v", taskID, uerr)
		}
		return "", newOpErr(codeProcessError, fmt.Errorf("reopen attach: %w", aerr))
	}
	if err := m.proc.NewSession(newSessionSpec(tuiName, row.WorktreePath, tuiEnv,
		[]string{"opencode", "attach", fmt.Sprintf("http://127.0.0.1:%d", port), "--session", sessionID})); err != nil {
		return "", newOpErr(codeProcessError, err)
	}
	// 注册 tui group（B4：groups 真实写入注册表）。
	if rt := m.getRuntime(taskID); rt != nil {
		rt.registerGroup("tui", tuiName)
	}
	m.watchTUIExit(taskID, tuiName)
	return TerminalID(tuiName), nil
}

// AttachPty 为 WS 终端构造 attach 客户端 PTY（design.md §18 AttachPty）。
// 由 WS 层调用：根据终端类型（TUI/sessionName 或 shell/terminalID）创建 PTY。
func (m *Manager) AttachPty(sessionName string, cols, rows int) (*pty.Pty, error) {
	return m.proc.AttachPty(sessionName, cols, rows)
}

// --- shell 终端（design.md §18 CreateShell/CloseShell + §2 shell 会话序号） ---

// shellCounter 任务代内 shell 序号分配（in-memory，persist 恢复时从存活会话名解析继续）。
// 存储在 taskRuntime 上以便代内递增。
type shellCounter struct {
	n int64
}

// CreateShell 为活跃任务新建 shell 终端（design.md §18）。
// 返回 TerminalID（shell 会话名）。cwd=worktree，注入任务 env（复用 env 快照，不含密码）。
func (m *Manager) CreateShell(ctx context.Context, taskID string) (TerminalID, error) {
	unlock, err := m.tryLockTask(taskID)
	if err != nil {
		return "", err
	}
	defer unlock()

	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return "", newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
	}
	if row.Status != StatusActive {
		return "", newOpErr(codeInvalidState, fmt.Errorf("create shell requires active, got %s", row.Status))
	}
	rt := m.getRuntime(taskID)
	if rt == nil {
		return "", newOpErr(codeInvalidState, fmt.Errorf("no runtime for active task %s", taskID))
	}
	// 从存活 shell 会话名解析已用最大序号，继续递增。
	// B7b：ListSessions infra 错误 MUST 传播（不得静默吞错，design.md §8）。
	maxN := 0
	shellNames, err := m.listShellSessions(taskID)
	if err != nil {
		return "", newOpErr(codeProcessError, fmt.Errorf("enumerate shells for create shell: %w", err))
	}
	for _, s := range shellNames {
		n := shellNumberOf(taskID, s)
		if n > maxN {
			maxN = n
		}
	}
	n := maxN + 1
	_ = atomic.AddInt64 // 占位避免未用导入（实际用 maxN+1 简化）

	shellName := shellSessionName(taskID, n)
	env, err := m.loadEnvSnapshot(row)
	if err != nil {
		return "", newOpErr(codeInternal, err)
	}
	if err := m.proc.NewSession(newSessionSpec(shellName, row.WorktreePath, env,
		[]string{userShell()})); err != nil {
		return "", newOpErr(codeProcessError, err)
	}
	// 注册 shell group（B4：groups 真实写入注册表）。
	if rt := m.getRuntime(taskID); rt != nil {
		rt.registerGroup("shell", shellName)
	}
	m.watchShellExit(taskID, shellName)
	return TerminalID(shellName), nil
}

// CloseShell 关闭 shell 终端（design.md §18 CloseShell）。
// B10：校验 terminalID 必须是 shell 会话（不得关 serve/TUI）。
func (m *Manager) CloseShell(ctx context.Context, terminalID TerminalID) error {
	name := string(terminalID)
	if name == "" {
		return newOpErr(codeInvalidInput, fmt.Errorf("empty terminal id"))
	}
	taskID := taskIDFromSessionName(name)
	if taskID == "" {
		return newOpErr(codeInvalidInput, fmt.Errorf("invalid terminal id %s", name))
	}
	// 仅允许关闭 shell 会话（roleFromSessionName 以 shell- 前缀），拒绝 serve/tui。
	if role := roleFromSessionName(name); !strings.HasPrefix(role, "shell-") {
		return newOpErr(codeInvalidInput, fmt.Errorf("terminal %s is not a shell session", name))
	}
	unlock, err := m.tryLockTask(taskID)
	if err != nil {
		return err
	}
	defer unlock()
	// P1：CloseShell 的 HasSession/KillSession infra 错误 MUST 返回错误（不得一律成功）。
	// 区分 ErrNoTmuxServer（无 server → 视为不存在，幂等成功）与其他 infra 错误（fail-closed）。
	exists, herr := m.proc.HasSession(name)
	if herr != nil && !errors.Is(herr, process.ErrNoTmuxServer) {
		return newOpErr(codeProcessError, fmt.Errorf("close shell: has session %s: %w", name, herr))
	}
	if !exists {
		return nil // 幂等
	}
	res, kerr := m.proc.KillSession(name)
	if kerr != nil {
		// infra 错误 MUST 返回，不得吞错一律成功（design.md §8）。
		return newOpErr(codeProcessError, fmt.Errorf("close shell: kill session %s: %w", name, kerr))
	}
	if nerr := m.recordResidualNoticeFromDisposition(ctx, taskID, name, res); nerr != nil {
		// notice 写入错误 MUST 返回（不静默，design.md §8）。
		return newOpErr(codeInternal, fmt.Errorf("close shell: record notice %s: %w", name, nerr))
	}
	if rt := m.getRuntime(taskID); rt != nil {
		rt.removeGroup(name)
	}
	return nil
}

// watchShellExit 监视 shell 会话退出（记录日志级事件；shell 退出不改变任务状态）。
// 回调校验（B4 回调隔离，针对当前 runtime 注册表，C1：不捕获本地快照）。
// 事件类型分发（C1 typed RuntimeEvent）：
//   - WatchEventSessionExit → shell_exit：从注册表与 watchCancels 移除自身；
//   - WatchEventInfraError → shell infra 错误：记录日志 + 移除 group（shell 不收敛任务运行时，
//     与 serve/tui infra 错误不同，shell infra 不影响任务活跃状态）。
func (m *Manager) watchShellExit(taskID, shellName string) {
	tok := runtime.InstVersion("")
	if rt := m.getRuntime(taskID); rt != nil {
		tok = rt.instVersion
	}
	cancel, done := m.proc.WatchExit(shellName, func(ev process.WatchEvent) {
		cur := m.getRuntime(taskID)
		if cur == nil || !cur.matchesRegistry(tok, shellName) {
			return
		}
		switch ev.Type {
		case process.WatchEventSessionExit:
			// shell 退出：从注册表与 watchCancels 移除自身（best-effort）。
			cur.removeGroup(shellName)
			cur.mu.Lock()
			delete(cur.watchCancels, shellName)
			delete(cur.watchDones, shellName)
			cur.mu.Unlock()
		case process.WatchEventInfraError:
			// shell infra 错误：记录日志，从注册表移除（best-effort，不改变任务状态）。
			log.Printf("task %s: tmux infra error watching shell %s: %v", taskID, shellName, ev.Err)
			cur.removeGroup(shellName)
		}
	})
	if cur := m.getRuntime(taskID); cur != nil {
		cur.mu.Lock()
		cur.watchCancels[shellName] = cancel
		cur.watchDones[shellName] = done
		cur.mu.Unlock()
	}
}

// shellNumberOf 从会话名 ocdeck-<taskID>-shell-<n> 解析 n。
func shellNumberOf(taskID, name string) int {
	prefix := "ocdeck-" + taskID + "-shell-"
	if len(name) <= len(prefix) {
		return 0
	}
	n, _ := strconv.Atoi(name[len(prefix):])
	return n
}

// userShell 返回用户默认 shell（SHELL env 或 /bin/bash）。
func userShell() string {
	if s, ok := hostEnv("SHELL"); ok && s != "" {
		return s
	}
	return "/bin/bash"
}

// ListTaskSessions 返回任务的会话归属列表（design.md §21 GET /tasks/:id）。
func (m *Manager) ListTaskSessions(ctx context.Context, taskID string) ([]SessionRow, error) {
	return m.store.ListTaskSessions(ctx, taskID)
}

// ListShells 返回该任务当前存活的 shell 终端列表（terminalID/会话名，design.md §21 GET /tasks/:id/terminals）。
// 从 RuntimeGroup 注册表（role=shell）枚举 + HasSession 存活过滤，
// 保证仅返回本进程代内注册且仍存活的 shell（不返回 serve/TUI/其他任务的会话）。
// B5-backend：HasSession tmux 基础设施故障（非 ErrNoTmuxServer）MUST 返回错误（process_error），
// 不得映射为空列表成功——区分 ErrNoTmuxServer（无 server → 空列表合理）与会话不存在（空列表合理）
// vs 其他 infra 故障（返回 error，让 api 层映射 process_error，main 据此感知）。
func (m *Manager) ListShells(taskID string) ([]TerminalID, error) {
	rt := m.getRuntime(taskID)
	if rt == nil {
		return nil, nil
	}
	rt.mu.Lock()
	var names []string
	for sessionName, g := range rt.groups {
		if g.Role != "shell" {
			continue
		}
		names = append(names, sessionName)
	}
	rt.mu.Unlock()
	// 存活过滤：注册表可能滞后于 tmux 实际状态（shell 退出回调未及时移除）。
	var out []TerminalID
	for _, name := range names {
		alive, herr := m.proc.HasSession(name)
		if herr != nil && !errors.Is(herr, process.ErrNoTmuxServer) {
			// B5-backend：infra 故障（非 ErrNoTmuxServer）MUST 返回错误，不得静默映射为空列表。
			return nil, newOpErr(codeProcessError, fmt.Errorf("list shells: has session %s: %w", name, herr))
		}
		if alive {
			out = append(out, TerminalID(name))
		}
	}
	return out, nil
}

// ValidateShellTerminal 校验 terminalID 对应一个存活的 shell 终端（design.md §21 shell WS 身份校验）。
// 防止 shell WS attach 任意合法 tmux 会话名（如 serve/TUI/其他任务会话）。
// 校验：tid 解析出 taskID、role 为 shell-<n>、会话存活、且在该任务运行时注册表中注册为 shell。
// P1：HasSession infra 错误（非 ErrNoTmuxServer）MUST NOT 映射为"不存在"——返回 process_error，
// 区分 ErrNoTmuxServer/会话不存在 vs 其他 infra 故障。
// 返回 OpError（invalid_input/not_found/conflict/process_error），供 api/WS 层映射关闭码。
func (m *Manager) ValidateShellTerminal(tid string) error {
	if tid == "" {
		return newOpErr(codeInvalidInput, fmt.Errorf("empty terminal id"))
	}
	taskID := taskIDFromSessionName(tid)
	if taskID == "" {
		return newOpErr(codeInvalidInput, fmt.Errorf("invalid terminal id %s", tid))
	}
	if role := roleFromSessionName(tid); !strings.HasPrefix(role, "shell-") {
		// 指向 serve/TUI 或非 shell 会话 → 拒绝。
		return newOpErr(codeInvalidInput, fmt.Errorf("terminal %s is not a shell session", tid))
	}
	alive, herr := m.proc.HasSession(tid)
	if herr != nil && !errors.Is(herr, process.ErrNoTmuxServer) {
		// infra 故障：MUST NOT 映射为"不存在"，返回 process_error（fail-closed）。
		return newOpErr(codeProcessError, fmt.Errorf("validate shell %s: has session: %w", tid, herr))
	}
	if !alive {
		return newOpErr(codeNotFound, fmt.Errorf("shell terminal %s not found (not alive)", tid))
	}
	// 运行时注册表校验：必须属于该任务的活跃运行时且注册为 shell。
	// 防止 attach 其他进程代/会话名碰巧匹配但未托管的情况。
	rt := m.getRuntime(taskID)
	if rt == nil {
		return newOpErr(codeNotFound, fmt.Errorf("shell terminal %s not found (no runtime)", tid))
	}
	rt.mu.Lock()
	g, ok := rt.groups[tid]
	rt.mu.Unlock()
	if !ok || g == nil || g.Role != "shell" {
		return newOpErr(codeNotFound, fmt.Errorf("shell terminal %s not found (not registered as shell)", tid))
	}
	return nil
}
