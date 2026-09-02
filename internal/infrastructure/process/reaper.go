// Package process reaper：kill-session 前快照 pane 子孙进程，kill 后对幸存者
// 身份校验（pid+startTime）→ TERM 宽限 → KILL（design.md §2）。
//
// 快照失败规则（design.md §2）：会话仍存在而快照失败 → MUST NOT kill，返回
// retryable_snapshot_failed；会话已消失且快照失败 → snapshot_missing_degraded。
// 进程身份（pid+startTime+pgid）MUST NOT 出本包：对外用 opaque ticket 字符串。
package process

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// psProvider 抽象 ps 命令调用，便于测试注入 mock（design.md §10 v1 仅 Darwin）。
type psProvider interface {
	// AllProcTree 返回全系统进程的 (pid, ppid) 列表，用于递归收集 pane 子孙。
	AllProcTree(ctx context.Context) ([]procEntry, error)
	// ProcessIdentity 返回 (startTime, pgid)，用于快照身份捕获与校验。
	// pid 不存在返回 ("", 0, ErrProcNotFound)；ps 失败/超时/畸形输出返回其他 error
	// （调用方 MUST 与"PID 不存在"区分，避免误报 clean）。
	ProcessIdentity(ctx context.Context, pid int) (startTime string, pgid int, err error)
	// Kill 发送信号给 pid。PID 不存在视为幂等成功；EPERM 报错。
	Kill(pid int, sig string) error
	// Alive 探测 pid 是否存活。
	// zombie（已终止、未被父进程 reap）MUST 视为已死：SIGKILL 后被 init 收养
	// 的进程在 PID 1 不收割的环境（docker 容器、CI runner）下长期残留进程表
	// 条目，若按 kill -0 判活，reaper 会把已正确收割的进程永远误报为 survivor。
	Alive(pid int) bool
}

// ErrProcNotFound 表示 ProcessIdentity 查询的 pid 不存在（与 ps 失败/畸形输出区分）。
var ErrProcNotFound = errors.New("process: pid not found")

// procEntry 是 ps -A -o pid=,ppid= 的一行。
type procEntry struct {
	pid  int
	ppid int
}

// darwinPSProvider 是基于 POSIX ps(1)/kill(1) 的实现，macOS 与 Linux 通用
// （design.md §10 v1 以 Darwin 为主目标；ps/kill 语义差异见各方法注释）。
type darwinPSProvider struct{}

func (darwinPSProvider) AllProcTree(ctx context.Context) ([]procEntry, error) {
	cmd := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,ppid=")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ps -A -o pid=,ppid=: %w", err)
	}
	var entries []procEntry
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		entries = append(entries, procEntry{pid: pid, ppid: ppid})
	}
	return entries, nil
}

func (darwinPSProvider) ProcessIdentity(ctx context.Context, pid int) (string, int, error) {
	// lstart 格式如 "Thu Jul 30 13:07:37 2026"；pgid 经 pgid= 取。
	cmd := exec.CommandContext(ctx, "ps", "-o", "lstart=,pgid=", "-p", strconv.Itoa(pid))
	out, err := cmd.Output()
	if err != nil {
		// ps 对不存在的 pid 返回 exit 1（ExitError），与"ps 命令本身失败/超时"区分：
		// ExitError 视为 PID 不存在；其他错误（ctx 超时、二进制缺失等）向上传播，
		// 调用方据此判定 ps 失败而非 PID 不存在（避免误报 clean）。
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", 0, ErrProcNotFound
		}
		return "", 0, fmt.Errorf("ps identity pid=%d: %w", pid, err)
	}
	// 输出形如 "Thu Jul 30 13:07:37 2026 64663"——lstart 占前 24 字符左右，
	// 末尾是 pgid 数字。按字段分割：lstart 是 5 个 token（WEEK MON DAY TIME YEAR），
	// 之后是 pgid。
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) < 6 {
		// 畸形输出（字段不足）——视为 ps 失败而非 PID 不存在，避免误报 clean。
		return "", 0, fmt.Errorf("ps identity pid=%d: malformed output %q", pid, string(out))
	}
	pgid, err := strconv.Atoi(fields[len(fields)-1])
	if err != nil {
		// pgid 字段非数字——畸形输出，视为 ps 失败。
		return "", 0, fmt.Errorf("ps identity pid=%d: malformed pgid %q", pid, fields[len(fields)-1])
	}
	// lstart = 前 5 个字段用空格拼回原样。
	startTime := strings.Join(fields[:5], " ")
	return startTime, pgid, nil
}

func (darwinPSProvider) Kill(pid int, sig string) error {
	cmd := exec.Command("kill", "-"+sig, strconv.Itoa(pid))
	if err := cmd.Run(); err != nil {
		// kill 对不存在 PID 返回 "No such process"（exit 1）——视为幂等成功；
		// EPERM（"Operation not permitted"）报错。通过 stderr 文本区分。
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.Stderr != nil {
			low := strings.ToLower(string(ee.Stderr))
			if strings.Contains(low, "no such process") {
				return nil
			}
		}
		return fmt.Errorf("kill -%s %d: %w", sig, pid, err)
	}
	return nil
}

func (darwinPSProvider) Alive(pid int) bool {
	// 以 ps stat= 判存活而非 kill -0：kill -0 对 zombie（已终止、未被父进程
	// reap）返回成功，会把已收割进程误判为存活——SIGKILL 后被 init 收养的
	// 进程在 PID 1 不收割的环境（docker 容器、CI runner）下长期保持 zombie，
	// zombie 对信号无反应，MUST 视为已死。
	// 进程不存在时 ps 退出非零；ps stat 列对他户进程可见（不受信号权限影响，
	// 原 kill -0 的 EPERM 判活场景照常覆盖）。探测命令失败视为存活失败
	//（与原 kill -0 缺命令行为一致）。
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	stat := strings.TrimSpace(string(out))
	return stat != "" && !strings.HasPrefix(stat, "Z")
}

// ticketPayload 是 cleanup ticket 的包内编码结构（MUST NOT 出本包）。
// 身份 = pid + startTime；pgid 附加供诊断。
type ticketPayload struct {
	PID       int    `json:"p"`
	StartTime string `json:"s"`
	PGID      int    `json:"g,omitempty"`
}

// encodeTicket 将进程身份编码为 opaque base64 字符串。
func encodeTicket(t ticketPayload) string {
	b, _ := json.Marshal(t)
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeTicket 反向解码（包内使用）。MUST 校验 PID 为安全正整数（>0），
// startTime 非空——畸形 payload 视为无效（不可解析身份），调用方计入失败。
func decodeTicket(s string) (ticketPayload, bool) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return ticketPayload{}, false
	}
	var t ticketPayload
	if err := json.Unmarshal(b, &t); err != nil {
		return ticketPayload{}, false
	}
	// PID 校验：安全正整数，避免 0/负值被传入 kill 造成误杀或语义错误。
	if t.PID <= 0 {
		return ticketPayload{}, false
	}
	if t.StartTime == "" {
		return ticketPayload{}, false
	}
	return t, true
}

// procSnapshot 是一次 pane 的子孙进程快照。
type procSnapshot struct {
	tickets []ticketPayload
	// pidSet 用于 kill 后查幸存者。
	pidSet map[int]ticketPayload
}

// collectDescendants 从 roots 出发，在 procTree 中递归收集所有后代 pid。
// 返回包含 roots 自身与全部后代的集合。
func collectDescendants(tree []procEntry, roots []int) []int {
	// 构建 ppid → children 索引。
	children := make(map[int][]int)
	for _, e := range tree {
		children[e.ppid] = append(children[e.ppid], e.pid)
	}
	seen := make(map[int]struct{})
	var out []int
	var walk func(pid int)
	walk = func(pid int) {
		if _, ok := seen[pid]; ok {
			return
		}
		seen[pid] = struct{}{}
		out = append(out, pid)
		for _, c := range children[pid] {
			walk(c)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	return out
}

// snapshotSession 收集会话全部 pane 的子孙进程快照（design.md §2）。
// 流程：list-panes -s -t =<name> -F '#{pane_pid}' 取 pane pid →
// ps -A -o pid=,ppid= 全量进程树 → 递归收集 pane 子孙 → 逐个取 startTime+pgid。
// 会话不存在返回 (nil, errSessionGone)——调用方据此转 snapshot_missing_degraded。
func (m *Manager) snapshotSession(ctx context.Context, name string) (*procSnapshot, error) {
	// 1. 取 pane pid 列表。
	stdout, _, err := m.execTmux(ctx, "list-panes", "-s", "-t", "="+name, "-F", "#{pane_pid}")
	if err != nil {
		var ce *tmuxCmdError
		if errors.As(err, &ce) && (isSessionNotFoundExit(ce) || isNoServerExit(ce)) {
			// 无 server 与单会话不存在都表示目标会话不可达——视为会话已消失，
			// 转 snapshot_missing_degraded（不可重试）。
			return nil, errSessionGone
		}
		return nil, fmt.Errorf("list-panes %s: %w", name, err)
	}
	var panePids []int
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err != nil {
			continue
		}
		panePids = append(panePids, pid)
	}
	if len(panePids) == 0 {
		// 无 pane（空会话）——快照为空视为有效（无子孙可收割）。
		return &procSnapshot{tickets: nil, pidSet: map[int]ticketPayload{}}, nil
	}

	// 2. 取全量进程树，递归收集后代。
	tree, err := m.psProvider.AllProcTree(ctx)
	if err != nil {
		return nil, fmt.Errorf("ps tree: %w", err)
	}
	descs := collectDescendants(tree, panePids)

	// 3. 逐个取 startTime+pgid 形成 ticket。
	snap := &procSnapshot{pidSet: make(map[int]ticketPayload, len(descs))}
	for _, pid := range descs {
		st, pgid, err := m.psProvider.ProcessIdentity(ctx, pid)
		if err != nil {
			// 区分"PID 不存在"（进程在取身份间已退出，跳过）与 ps 失败/畸形输出
			// （快照失败，MUST NOT 静默当作 clean——会丢失逃逸子孙身份）。
			if errors.Is(err, ErrProcNotFound) {
				continue
			}
			return nil, fmt.Errorf("ps identity pid=%d: %w", pid, err)
		}
		if st == "" {
			// 进程在取身份间已退出——跳过（不进快照）。
			continue
		}
		tp := ticketPayload{PID: pid, StartTime: st, PGID: pgid}
		snap.tickets = append(snap.tickets, tp)
		snap.pidSet[pid] = tp
	}
	return snap, nil
}

// errSessionGone 标记会话在快照时已不存在（转 snapshot_missing_degraded）。
var errSessionGone = fmt.Errorf("session gone during snapshot")

// KillSession 终止会话并收割逃逸子孙（design.md §18）。
// 流程：① reaper 快照 ② kill-session ③ 对幸存者身份校验后 TERM 宽限 KILL。
// 调用方 MUST 只对已确认存在的会话调用（absent-at-entry 由上层处理）。
func (m *Manager) KillSession(name string) (KillResult, error) {
	if err := ValidateSessionName(name); err != nil {
		return KillResult{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// ① 快照（会话不存在转 snapshot_missing_degraded，不报错）。
	snap, err := m.snapshotSession(ctx, name)
	if err != nil {
		if err == errSessionGone {
			// 会话已消失 + 快照"缺失"（无法定位 pane）→ degraded，不可重试。
			return KillResult{
				SessionKilled: false,
				Disposition:  DispositionSnapshotMissingDegraded,
			}, nil
		}
		// 会话仍存在但快照失败 → MUST NOT kill，返回 retryable_snapshot_failed。
		return KillResult{
			SessionKilled: false,
			Disposition:   DispositionSnapshotFailed,
		}, nil
	}

	// ② kill-session。
	killCtx, killCancel := context.WithTimeout(context.Background(), 10*time.Second)
	_, _, killErr := m.execTmux(killCtx, "kill-session", "-t", "="+name)
	killCancel()
	if killErr != nil {
		// kill 失败：会话可能仍在——返回 retryable_kill_failed，tickets 携带快照身份。
		tickets := encodeTickets(snap.tickets)
		return KillResult{
			SessionKilled: false,
			Disposition:   DispositionKillFailed,
			CleanupTickets: tickets,
		}, nil
	}

	// ③ 收割幸存者：kill-session 发 SIGHUP，忽略 HUP 的逃逸子孙 reparent 到 init，
	// 仍保 startTime/pgid——身份校验后 TERM 宽限 KILL。
	remaining := m.reapSurvivors(ctx, snap)
	if len(remaining) == 0 {
		return KillResult{
			SessionKilled: true,
			Disposition:   DispositionClean,
		}, nil
	}
	// 有幸存者未净 → retryable_reap_failed，tickets 携带剩余身份。
	return KillResult{
		SessionKilled: true,
		Disposition:   DispositionReapFailed,
		CleanupTickets: encodeTickets(remaining),
	}, nil
}

// reapSurvivors 对快照中仍存活且身份匹配的进程先 TERM 后 KILL（design.md §2）。
// 返回未能收割的进程身份列表（供 RetryReap）。
//
// 关键（B3）：
//   - TERM 宽限对全部快照 PID 并发等待一次 termGrace（而非每 PID 串行 N×2s），
//     宽限期可被 ctx 取消（time.Sleep 改 select）。
//   - KILL 前 MUST 再次身份校验：TERM 后 2s 宽限期内 PID 可能被复用，
//     若 startTime 已变则跳过（不可误杀复用 PID 的新进程）。
//   - ProcessIdentity 错误分类：ps 失败/畸形输出（非 ErrProcNotFound）→ 保守保留
//     （不可误杀、不可当作已收割）；PID 不存在 → 视为已退出，跳过。
func (m *Manager) reapSurvivors(ctx context.Context, snap *procSnapshot) []ticketPayload {
	// 第一阶段：筛选存活且身份匹配的目标，发 TERM。
	type target struct {
		pid int
		tp  ticketPayload
	}
	var targets []target
	var remaining []ticketPayload
	for pid, tp := range snap.pidSet {
		if !m.psProvider.Alive(pid) {
			continue
		}
		st, _, err := m.psProvider.ProcessIdentity(ctx, pid)
		if err != nil {
			if errors.Is(err, ErrProcNotFound) {
				// PID 已退出——跳过。
				continue
			}
			// ps 失败/畸形输出——保守保留（不可误杀、不可当作已收割）。
			remaining = append(remaining, tp)
			continue
		}
		if st != tp.StartTime {
			// startTime 变 = 进程已退出并被复用 pid——非目标，跳过。
			continue
		}
		_ = m.psProvider.Kill(pid, "TERM")
		targets = append(targets, target{pid: pid, tp: tp})
	}

	// 第二阶段：对全部已 TERM 的 PID 并发等待一次 termGrace（可被 ctx 取消）。
	if len(targets) > 0 {
		termTimer := time.NewTimer(termGrace)
		select {
		case <-termTimer.C:
		case <-ctx.Done():
			termTimer.Stop()
		}
	}

	// 第三阶段：TERM 宽限后仍存活者，再次身份校验后 KILL。
	for _, t := range targets {
		if !m.psProvider.Alive(t.pid) {
			continue
		}
		// KILL 前再次身份校验：宽限期内 PID 可能被复用。
		st, _, err := m.psProvider.ProcessIdentity(ctx, t.pid)
		if err != nil {
			if errors.Is(err, ErrProcNotFound) {
				continue
			}
			// ps 失败——保守保留（不确定是否为同一进程，不可误杀）。
			remaining = append(remaining, t.tp)
			continue
		}
		if st != t.tp.StartTime {
			// PID 已被复用——非目标，跳过（不可误杀新进程）。
			continue
		}
		_ = m.psProvider.Kill(t.pid, "KILL")
		// 再探一次，仍活则保留（KILL 不可杀的进程如 D-state）。
		if m.psProvider.Alive(t.pid) {
			remaining = append(remaining, t.tp)
		}
	}
	return remaining
}

// RetryReap 按 opaque ticket 重试收割（design.md §18，供残留 notice 后台重试）。
// 返回仍未能收割的 ticket 列表（含无法解码的 ticket——MUST 计入失败，不得静默丢弃）。
func (m *Manager) RetryReap(tickets []string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var remaining []string
	type target struct {
		t   string
		pid int
		tp  ticketPayload
	}
	var targets []target
	for _, t := range tickets {
		tp, ok := decodeTicket(t)
		if !ok {
			// 无法解码的 ticket MUST 计入失败（不得静默丢弃）。
			remaining = append(remaining, t)
			continue
		}
		if !m.psProvider.Alive(tp.PID) {
			continue
		}
		st, _, err := m.psProvider.ProcessIdentity(ctx, tp.PID)
		if err != nil {
			if errors.Is(err, ErrProcNotFound) {
				// PID 已退出——视为已收割，跳过。
				continue
			}
			// ps 失败——保守保留。
			remaining = append(remaining, t)
			continue
		}
		if st != tp.StartTime {
			// pid 已被复用——非目标，跳过（视为已收割）。
			continue
		}
		_ = m.psProvider.Kill(tp.PID, "TERM")
		targets = append(targets, target{t: t, pid: tp.PID, tp: tp})
	}

	// 并发等待一次 termGrace（可被 ctx 取消）。
	if len(targets) > 0 {
		termTimer := time.NewTimer(termGrace)
		select {
		case <-termTimer.C:
		case <-ctx.Done():
			termTimer.Stop()
		}
	}

	for _, tg := range targets {
		if !m.psProvider.Alive(tg.pid) {
			continue
		}
		// KILL 前再次身份校验：宽限期内 PID 可能被复用。
		st, _, err := m.psProvider.ProcessIdentity(ctx, tg.pid)
		if err != nil {
			if errors.Is(err, ErrProcNotFound) {
				continue
			}
			remaining = append(remaining, tg.t)
			continue
		}
		if st != tg.tp.StartTime {
			continue
		}
		_ = m.psProvider.Kill(tg.pid, "KILL")
		if m.psProvider.Alive(tg.pid) {
			remaining = append(remaining, tg.t)
		}
	}
	return remaining, nil
}

// encodeTickets 批量编码 ticketPayload 为 opaque 字符串。
func encodeTickets(tps []ticketPayload) []string {
	out := make([]string, len(tps))
	for i, tp := range tps {
		out[i] = encodeTicket(tp)
	}
	return out
}