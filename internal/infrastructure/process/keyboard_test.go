// design D1（fix-terminal-input-panel-resize）键盘配置行为测试：
// NewSession 前置链、new-session 失败后的 exit-empty 恢复、EnsureServerOptions
// 三步（剪贴板 / extended-keys / exit-empty 恢复）互不跳过与错误汇总、版本门禁、
// reconcile 幂等重放。全部经 execTmuxFn 注入记录命令序列 / 注入失败，不依赖真实
// tmux。断言有效性：凡失败注入场景，若被测路径未执行（如漏掉恢复调用、步骤被
// 跳过、顺序颠倒），对应断言必然不通过。
package process

import (
	"bytes"
	"context"
	"errors"
	"log"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// kbFailure 描述一次故障注入：head 非空按命令首词匹配（start-server/new-session 等）；
// opt 非空按 set-option {option, value} 匹配（flags 无关，复用 isSetOptionCall）；
// noServer 命中时返回"无 server"错误，否则返回 permission denied。
type kbFailure struct {
	head     string
	opt      []string
	noServer bool
}

// newKeyboardMockManager 构造记录全部 execTmux 调用的 Manager：-V 应答 version
// （空串表示 tmux -V 失败，版本不可解析）；命中 failures 的调用注入对应错误；
// 其余调用成功。调用序列完整记录进 *calls 供顺序断言。
func newKeyboardMockManager(version string, failures []kbFailure, calls *[][]string) *Manager {
	return &Manager{
		execTmuxFn: func(_ context.Context, args ...string) (string, string, error) {
			cp := append([]string(nil), args...)
			*calls = append(*calls, cp)
			if len(args) == 1 && args[0] == "-V" {
				if version == "" {
					return "", "boom", &tmuxCmdError{sub: args, stderr: "boom", err: errors.New("exit 1")}
				}
				return version + "\n", "", nil
			}
			head := ""
			if len(args) > 0 {
				head = args[0]
			}
			for _, f := range failures {
				matched := (f.head != "" && head == f.head) ||
					(len(f.opt) == 2 && isSetOptionCall(cp, f.opt))
				if !matched {
					continue
				}
				if f.noServer {
					return "", "no server running on /tmp/tmux-0/ocdeck", &tmuxCmdError{
						sub:    args,
						stderr: "no server running on /tmp/tmux-0/ocdeck",
						err:    errors.New("exit 1"),
					}
				}
				return "", "permission denied", &tmuxCmdError{sub: args, stderr: "permission denied", err: errors.New("exit 1")}
			}
			return "", "", nil
		},
	}
}

// indexOfCall 返回 calls 中首个与 want 完全相等的下标，找不到返回 -1。
func indexOfCall(calls [][]string, want []string) int {
	for i, c := range calls {
		if equalArgs(c, want) {
			return i
		}
	}
	return -1
}

// hasCall 判断 calls 中是否存在与 want 完全相等的一次调用。
func hasCall(calls [][]string, want []string) bool {
	return indexOfCall(calls, want) >= 0
}

func captureProcessLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	old := log.Writer()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(old)
		log.SetFlags(log.LstdFlags)
	})
	return buf
}

// TestNewSession_PrechainBeforeNewSessionFullOrder 验证 design D1 三步顺序：
// 前置链（start-server \; exit-empty off \; extended-keys on，单次调用）先于
// new-session 完成；new-session 独立调用；随后 EnsureServerOptions 三步依次执行。
func TestNewSession_PrechainBeforeNewSessionFullOrder(t *testing.T) {
	var calls [][]string
	m := newKeyboardMockManager("tmux 3.7c", nil, &calls)
	err := m.NewSession(SessionSpec{
		Name:    "ocdeck-kb1-runtime",
		Dir:     "/tmp",
		CmdArgv: []string{"sleep", "1"},
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	prechain := []string{"start-server", ";", "set-option", "-s", "exit-empty", "off", ";", "set-option", "-s", "extended-keys", "on"}
	want := [][]string{
		{"-V"},
		prechain,
		{"new-session", "-d", "-s", "ocdeck-kb1-runtime", "-c", "/tmp", "--", "'sleep' '1'"},
		{"-V"},
		{"set-option", "-s", "get-clipboard", "off"},
		{"set-option", "-wg", "allow-passthrough", "off"},
		{"set-option", "-s", "set-clipboard", "on"},
		{"-V"},
		extendedKeysOn,
		exitEmptyOn,
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %d records in exact D1 order", calls, len(want))
	}
	for i, w := range want {
		if !equalArgs(calls[i], w) {
			t.Fatalf("calls[%d] = %v, want %v (full sequence=%v)", i, calls[i], w, calls)
		}
	}
}

// TestNewSession_PrechainFailureDegraded 验证前置链失败降级：new-session 照常执行，
// EnsureServerOptions 步骤②幂等重试 extended-keys。
func TestNewSession_PrechainFailureDegraded(t *testing.T) {
	var calls [][]string
	m := newKeyboardMockManager("tmux 3.7c", []kbFailure{{head: "start-server"}}, &calls)
	if err := m.NewSession(SessionSpec{
		Name:    "ocdeck-kb2-runtime",
		Dir:     "/tmp",
		CmdArgv: []string{"sleep", "1"},
	}); err != nil {
		t.Fatalf("NewSession must succeed after prechain failure (degraded): %v", err)
	}
	nsArgs := []string{"new-session", "-d", "-s", "ocdeck-kb2-runtime", "-c", "/tmp", "--", "'sleep' '1'"}
	nsIdx := indexOfCall(calls, nsArgs)
	if nsIdx < 0 {
		t.Fatalf("new-session must still run after prechain failure, calls=%v", calls)
	}
	if calls[1][0] != "start-server" {
		t.Fatalf("prechain must be attempted before new-session, calls[1]=%v", calls[1])
	}
	// 幂等重试：new-session 之后 extended-keys on 再次出现。
	ekIdx := indexOfCall(calls, extendedKeysOn)
	if ekIdx < 0 || ekIdx < nsIdx {
		t.Fatalf("extended-keys retry must follow new-session (idx=%d, ns=%d), calls=%v", ekIdx, nsIdx, calls)
	}
	if !hasCall(calls, exitEmptyOn) {
		t.Errorf("exit-empty restore missing, calls=%v", calls)
	}
}

// TestNewSession_CreateFailureRestoresExitEmpty 验证 new-session 失败后立即
// best-effort 恢复 exit-empty on：保留原创建错误、不 kill-server、不影响已有会话。
func TestNewSession_CreateFailureRestoresExitEmpty(t *testing.T) {
	var calls [][]string
	m := newKeyboardMockManager("tmux 3.7c", []kbFailure{{head: "new-session"}}, &calls)
	err := m.NewSession(SessionSpec{
		Name:    "ocdeck-kb3-runtime",
		Dir:     "/tmp",
		CmdArgv: []string{"sleep", "1"},
	})
	if err == nil {
		t.Fatal("want creation error")
	}
	if !strings.Contains(err.Error(), "new-session") {
		t.Errorf("err = %v, want original creation error mentioning new-session", err)
	}
	nsIdx := -1
	for i, c := range calls {
		if c[0] == "new-session" {
			nsIdx = i
			break
		}
	}
	if nsIdx < 0 {
		t.Fatalf("new-session must have been attempted, calls=%v", calls)
	}
	eeIdx := indexOfCall(calls, exitEmptyOn)
	if eeIdx < 0 || eeIdx < nsIdx {
		t.Fatalf("exit-empty on must run after failed new-session (ee=%d, ns=%d), calls=%v", eeIdx, nsIdx, calls)
	}
	for i, c := range calls {
		if c[0] == "kill-server" {
			t.Errorf("calls[%d] = kill-server must never be called on creation failure", i)
		}
	}
}

// TestNewSession_CreateAndRestoreFailureJoined 验证创建失败且恢复本身也失败时，
// 两条错误都汇总返回（errors.Join，不吞掉恢复失败）。
func TestNewSession_CreateAndRestoreFailureJoined(t *testing.T) {
	var calls [][]string
	m := newKeyboardMockManager("tmux 3.7c", []kbFailure{
		{head: "new-session"},
		{opt: []string{"exit-empty", "on"}},
	}, &calls)
	err := m.NewSession(SessionSpec{
		Name:    "ocdeck-kb4-runtime",
		Dir:     "/tmp",
		CmdArgv: []string{"sleep", "1"},
	})
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "new-session") || !strings.Contains(err.Error(), "exit-empty") {
		t.Errorf("err = %v, want joined creation and exit-empty restore failures", err)
	}
}

// kbCleanupCmds 会话清理/重建命令：键盘配置失败路径 MUST NOT 触发其中任何一个
// （会话保留，不影响已创建的 pane）。
var kbCleanupCmds = []string{"kill-session", "kill-server", "kill-pane", "respawn-pane", "respawn-window"}

// TestNewSession_KeyboardConfigFailureBestEffort 验证创建成功但键盘 set 失败：
// 仍返回创建成功、会话保留（不清理/不重建，new-session 恰好一次、无任何
// 清理/重建命令）、配置失败记录日志、exit-empty 恢复步骤照常执行。
func TestNewSession_KeyboardConfigFailureBestEffort(t *testing.T) {
	var calls [][]string
	logBuf := captureProcessLog(t)
	m := newKeyboardMockManager("tmux 3.7c", []kbFailure{{opt: []string{"extended-keys", "on"}}}, &calls)
	if err := m.NewSession(SessionSpec{
		Name:    "ocdeck-kb5-runtime",
		Dir:     "/tmp",
		CmdArgv: []string{"sleep", "1"},
	}); err != nil {
		t.Fatalf("NewSession must succeed despite config failure (session kept): %v", err)
	}
	nsCount := 0
	for _, c := range calls {
		if c[0] == "new-session" {
			nsCount++
		}
	}
	if nsCount != 1 {
		t.Errorf("new-session calls = %d, want 1 (no cleanup/recreate)", nsCount)
	}
	// 显式禁令：即使错误分支额外执行清理/重建，上述次数断言也可能漏检
	// （mock 对未匹配命令默认成功），这里逐条禁令兜底。
	for i, c := range calls {
		for _, cmd := range kbCleanupCmds {
			if c[0] == cmd {
				t.Errorf("calls[%d] = %s: config failure MUST NOT trigger cleanup/rebuild, calls=%v", i, cmd, calls)
			}
		}
	}
	ekIdx := indexOfCall(calls, extendedKeysOn)
	eeIdx := indexOfCall(calls, exitEmptyOn)
	if ekIdx < 0 || eeIdx <= ekIdx {
		t.Fatalf("exit-empty restore must follow failed extended-keys step (ek=%d, ee=%d), calls=%v", ekIdx, eeIdx, calls)
	}
	// 剪贴板步骤仍执行（在 extended-keys 步骤之前）。
	if idx := indexOfCall(calls, []string{"set-option", "-s", "get-clipboard", "off"}); idx < 0 || idx > ekIdx {
		t.Errorf("clipboard step must still run before extended-keys step, calls=%v", calls)
	}
	if !strings.Contains(logBuf.String(), "EnsureServerOptions") {
		t.Errorf("config failure must be logged, log=%q", logBuf.String())
	}
}

// TestNewSession_ExitEmptyRestoreFailureStillCreates 验证恢复本身失败时创建仍成功、
// 错误记录日志（恢复失败不阻断创建结果）。
func TestNewSession_ExitEmptyRestoreFailureStillCreates(t *testing.T) {
	var calls [][]string
	logBuf := captureProcessLog(t)
	m := newKeyboardMockManager("tmux 3.7c", []kbFailure{{opt: []string{"exit-empty", "on"}}}, &calls)
	if err := m.NewSession(SessionSpec{
		Name:    "ocdeck-kb6-runtime",
		Dir:     "/tmp",
		CmdArgv: []string{"sleep", "1"},
	}); err != nil {
		t.Fatalf("NewSession must succeed despite restore failure: %v", err)
	}
	nsCount := 0
	for _, c := range calls {
		if c[0] == "new-session" {
			nsCount++
		}
	}
	if nsCount != 1 {
		t.Errorf("new-session calls = %d, want 1", nsCount)
	}
	if !strings.Contains(logBuf.String(), "EnsureServerOptions") {
		t.Errorf("restore failure must be logged, log=%q", logBuf.String())
	}
}

// TestEnsureServerOptions_ClipboardFailureStillRestores 验证步骤①失败不跳过②③，
// 错误汇总返回。
func TestEnsureServerOptions_ClipboardFailureStillRestores(t *testing.T) {
	var calls [][]string
	m := newKeyboardMockManager("tmux 3.7c", []kbFailure{{opt: []string{"get-clipboard", "off"}}}, &calls)
	err := m.EnsureServerOptions()
	if err == nil {
		t.Fatal("want error from clipboard step")
	}
	if !strings.Contains(err.Error(), "get-clipboard") {
		t.Errorf("err = %v, want mention get-clipboard failure", err)
	}
	gcIdx := indexOfCall(calls, []string{"set-option", "-s", "get-clipboard", "off"})
	ekIdx := indexOfCall(calls, extendedKeysOn)
	eeIdx := indexOfCall(calls, exitEmptyOn)
	if gcIdx < 0 || ekIdx <= gcIdx || eeIdx <= ekIdx {
		t.Fatalf("steps must not be skipped after clipboard failure (gc=%d, ek=%d, ee=%d), calls=%v", gcIdx, ekIdx, eeIdx, calls)
	}
}

// TestEnsureServerOptions_ExtendedKeysFailureStillRestores 验证步骤②失败不跳过③，
// 错误汇总返回。
func TestEnsureServerOptions_ExtendedKeysFailureStillRestores(t *testing.T) {
	var calls [][]string
	m := newKeyboardMockManager("tmux 3.7c", []kbFailure{{opt: []string{"extended-keys", "on"}}}, &calls)
	err := m.EnsureServerOptions()
	if err == nil {
		t.Fatal("want error from extended-keys step")
	}
	if !strings.Contains(err.Error(), "extended-keys") {
		t.Errorf("err = %v, want mention extended-keys failure", err)
	}
	ekIdx := indexOfCall(calls, extendedKeysOn)
	eeIdx := indexOfCall(calls, exitEmptyOn)
	if ekIdx < 0 || eeIdx <= ekIdx {
		t.Fatalf("exit-empty restore must follow failed extended-keys step (ek=%d, ee=%d), calls=%v", ekIdx, eeIdx, calls)
	}
}

// TestEnsureServerOptions_ExitEmptyRestoreFailureSurfaces 验证恢复本身失败时错误
// 上抛（步骤①②已执行），不静默。
func TestEnsureServerOptions_ExitEmptyRestoreFailureSurfaces(t *testing.T) {
	var calls [][]string
	m := newKeyboardMockManager("tmux 3.7c", []kbFailure{{opt: []string{"exit-empty", "on"}}}, &calls)
	err := m.EnsureServerOptions()
	if err == nil {
		t.Fatal("want error from exit-empty restore step")
	}
	if !strings.Contains(err.Error(), "exit-empty") {
		t.Errorf("err = %v, want mention exit-empty restore failure", err)
	}
	if !hasCall(calls, extendedKeysOn) {
		t.Errorf("extended-keys step must still run, calls=%v", calls)
	}
}

// TestEnsureServerOptions_AllStepsFailJoined 验证三步同时失败：三条错误都经
// errors.Join 保留（不吞掉、不被后续步骤覆盖），且三步全部尝试执行。
func TestEnsureServerOptions_AllStepsFailJoined(t *testing.T) {
	var calls [][]string
	m := newKeyboardMockManager("tmux 3.7c", []kbFailure{
		{opt: []string{"get-clipboard", "off"}},
		{opt: []string{"extended-keys", "on"}},
		{opt: []string{"exit-empty", "on"}},
	}, &calls)
	err := m.EnsureServerOptions()
	if err == nil {
		t.Fatal("want error when all three steps fail")
	}
	for _, want := range []string{"get-clipboard", "extended-keys", "exit-empty"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want joined error mentioning %q", err, want)
		}
	}
	// errors.As 能穿透汇总错误找到原始 tmuxCmdError（一次 As 证明至少一条链可达）。
	var ce *tmuxCmdError
	if !errors.As(err, &ce) {
		t.Errorf("errors.As tmuxCmdError failed; joined unwrap chain lost (err=%v)", err)
	}
	gcIdx := indexOfCall(calls, []string{"set-option", "-s", "get-clipboard", "off"})
	ekIdx := indexOfCall(calls, extendedKeysOn)
	eeIdx := indexOfCall(calls, exitEmptyOn)
	if gcIdx < 0 || ekIdx <= gcIdx || eeIdx <= ekIdx {
		t.Fatalf("all three steps must still be attempted in order (gc=%d, ek=%d, ee=%d), calls=%v", gcIdx, ekIdx, eeIdx, calls)
	}
}

// TestEnsureServerOptions_NoServerDirectReturn 验证 ErrNoTmuxServer 直通返回：
// 任一步确定无 server 即返回，后续步骤不再执行（无 server 即无可配置对象）。
func TestEnsureServerOptions_NoServerDirectReturn(t *testing.T) {
	t.Run("first clipboard step", func(t *testing.T) {
		var calls [][]string
		m := newKeyboardMockManager("tmux 3.7c", []kbFailure{{noServer: true, opt: []string{"get-clipboard", "off"}}}, &calls)
		err := m.EnsureServerOptions()
		if !errors.Is(err, ErrNoTmuxServer) {
			t.Fatalf("err = %v, want ErrNoTmuxServer", err)
		}
		opts := optCalls(calls)
		if len(opts) != 1 || !equalArgs(opts[0], []string{"set-option", "-s", "get-clipboard", "off"}) {
			t.Fatalf("set-option calls = %v, want exactly [get-clipboard off] (no extended-keys/exit-empty after ErrNoTmuxServer)", opts)
		}
	})
	t.Run("extended-keys step", func(t *testing.T) {
		var calls [][]string
		m := newKeyboardMockManager("tmux 3.7c", []kbFailure{{noServer: true, opt: []string{"extended-keys", "on"}}}, &calls)
		err := m.EnsureServerOptions()
		if !errors.Is(err, ErrNoTmuxServer) {
			t.Fatalf("err = %v, want ErrNoTmuxServer", err)
		}
		// 剪贴板步骤已执行（server 在该步仍存在）；确定无 server 后 exit-empty 不再执行。
		if !hasCall(calls, []string{"set-option", "-s", "set-clipboard", "on"}) {
			t.Errorf("clipboard step must run before server disappears, calls=%v", calls)
		}
		if hasCall(calls, exitEmptyOn) {
			t.Errorf("exit-empty restore must not run after ErrNoTmuxServer, calls=%v", calls)
		}
	})
}

// TestNewSession_LowVersionSkipsExtendedKeysOnly 验证版本门禁仅作用于 extended-keys：
// < 3.2 / 版本不可解析时，前置链仍执行 start-server + exit-empty off（无 extended-keys），
// 创建后 exit-empty on 恢复照常；3.2 边界值包含 extended-keys。
func TestNewSession_LowVersionSkipsExtendedKeysOnly(t *testing.T) {
	for _, v := range []string{"tmux 3.1", "bogus", "tmux", "tmux next-3.4"} {
		t.Run(v, func(t *testing.T) {
			var calls [][]string
			m := newKeyboardMockManager(v, nil, &calls)
			if err := m.NewSession(SessionSpec{
				Name:    "ocdeck-kb7-runtime",
				Dir:     "/tmp",
				CmdArgv: []string{"sleep", "1"},
			}); err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			prechain := []string{"start-server", ";", "set-option", "-s", "exit-empty", "off"}
			if !equalArgs(calls[1], prechain) {
				t.Fatalf("prechain = %v, want %v (extended-keys gated off, start-server/exit-empty kept)", calls[1], prechain)
			}
			if hasCall(calls, extendedKeysOn) {
				t.Errorf("extended-keys must not appear anywhere for %q, calls=%v", v, calls)
			}
			if !hasCall(calls, exitEmptyOn) {
				t.Errorf("exit-empty restore must still run for %q, calls=%v", v, calls)
			}
		})
	}
	t.Run("boundary 3.2 keeps extended-keys", func(t *testing.T) {
		var calls [][]string
		m := newKeyboardMockManager("tmux 3.2", nil, &calls)
		if err := m.NewSession(SessionSpec{
			Name:    "ocdeck-kb7-runtime",
			Dir:     "/tmp",
			CmdArgv: []string{"sleep", "1"},
		}); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		if !hasCall(calls, extendedKeysOn) {
			t.Errorf("extended-keys on must be kept at boundary version 3.2, calls=%v", calls)
		}
	})
}

// TestEnsureServerOptions_LowVersionSkipsExtendedKeysOnly 验证 EnsureServerOptions
// 落点的版本门禁：低版本仅跳过 extended-keys，exit-empty on 恢复照常执行。
func TestEnsureServerOptions_LowVersionSkipsExtendedKeysOnly(t *testing.T) {
	var calls [][]string
	m := newKeyboardMockManager("tmux 3.1", nil, &calls)
	if err := m.EnsureServerOptions(); err != nil {
		t.Fatalf("EnsureServerOptions: %v", err)
	}
	want := [][]string{
		{"-V"},
		clipboardExternal,
		{"set-option", "-wg", "allow-passthrough", "off"},
		{"-V"},
		exitEmptyOn,
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %d records (no extended-keys, exit-empty kept)", calls, len(want))
	}
	for i, w := range want {
		if !equalArgs(calls[i], w) {
			t.Fatalf("calls[%d] = %v, want %v (full=%v)", i, calls[i], w, calls)
		}
	}
}

// TestEnsureServerOptions_ReconcileIdempotent 验证 reconcile 存量 server 场景的
// 幂等重放：连续两次 EnsureServerOptions 均成功且命令序列完全一致。
func TestEnsureServerOptions_ReconcileIdempotent(t *testing.T) {
	var calls [][]string
	m := newKeyboardMockManager("tmux 3.7c", nil, &calls)
	for i := 0; i < 2; i++ {
		if err := m.EnsureServerOptions(); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	const perRun = 7 // -V, 剪贴板三步, -V, extended-keys on, exit-empty on
	if len(calls) != 2*perRun {
		t.Fatalf("calls = %v, want %d records", calls, 2*perRun)
	}
	for i := 0; i < perRun; i++ {
		if !equalArgs(calls[i], calls[i+perRun]) {
			t.Fatalf("second run diverged at %d: %v vs %v", i, calls[i], calls[i+perRun])
		}
	}
}

// waitForChan 在 timeout 内等待 barrier 关闭，超时 Fatalf（失败路径不无限挂起）。
func waitForChan(t *testing.T, ch <-chan struct{}, timeout time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %s within %v", what, timeout)
	}
}

// waitForGoroutineParkedAt 轮询全量 goroutine 栈，直到出现「同一栈块同时含
// callerFrame 与 mutexFrame」（即对方 goroutine 真实到达加锁点并被互斥锁阻塞），
// 超时 Fatalf 并附栈快照。作为确定性 barrier 替代纯时间窗口：被阻塞方在锁释放前
// 不可能执行任何命令；若锁被移除（mutation），被阻塞方直接跑完、该等待必然超时变红。
func waitForGoroutineParkedAt(t *testing.T, callerFrame, mutexFrame string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		for _, block := range strings.Split(string(buf[:n]), "\n\n") {
			if strings.Contains(block, callerFrame) && strings.Contains(block, mutexFrame) {
				return
			}
		}
		if time.Now().After(deadline) {
			dump := n
			if dump > 64<<10 {
				dump = 64 << 10
			}
			t.Fatalf("no goroutine parked at %s (mutex frame %q) within %v; stacks:\n%s", callerFrame, mutexFrame, timeout, buf[:dump])
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitGroupWithTimeout 在 timeout 内等待 wg 完成，超时返回 false（调用方 Fatalf
// 并附诊断），不阻塞测试进程。
func waitGroupWithTimeout(wg *sync.WaitGroup, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// kbPrechain 完整前置链（tmux >= 3.2，含 extended-keys）。
var kbPrechain = []string{"start-server", ";", "set-option", "-s", "exit-empty", "off", ";", "set-option", "-s", "extended-keys", "on"}

// TestNewSession_ConcurrentTransactionsSerialized 验证 design D1 并发约束（I1）：
// 同一 Manager 上两个并发 NewSession 的「前置配置—new-session—失败恢复
// exit-empty」事务窗口经互斥锁串行化。barrier 控制交错：A 完成前置配置并阻塞在
// new-session；以 goroutine 栈证明 B 真实到达 NewSession 加锁点且被阻塞（而非
// 时间窗口推断），此时 B 未发出任何命令；放行 A 后完整事务落定，B 的前置链才
// 执行——恢复不使另一方失去预配置 server，最终 extended-keys on 在 B 侧生效、
// 全程无 kill/respawn 清理命令。移除锁的 mutation 举证见变更记录。
func TestNewSession_ConcurrentTransactionsSerialized(t *testing.T) {
	const (
		nameA = "ocdeck-ic1-runtime"
		nameB = "ocdeck-ic2-runtime"
	)
	var (
		mu    sync.Mutex
		calls [][]string
		// aAtNewSession：A 的 new-session 已到达（A 前置配置已完成）。
		aAtNewSession = make(chan struct{})
		// releaseA 幂等放行：正常由测试主体触发；FailNow 提前退出时经 Cleanup
		// 兜底关闭，避免 A 阻塞在 mock 等待点泄漏。
		releaseA = make(chan struct{})
	)
	release := func() {
		select {
		case <-releaseA:
		default:
			close(releaseA)
		}
	}
	t.Cleanup(release)

	m := &Manager{
		execTmuxFn: func(ctx context.Context, args ...string) (string, string, error) {
			mu.Lock()
			calls = append(calls, append([]string(nil), args...))
			mu.Unlock()
			if len(args) == 1 && args[0] == "-V" {
				return "tmux 3.7c\n", "", nil
			}
			if len(args) > 0 && args[0] == "new-session" {
				for _, a := range args {
					if a == nameA {
						// A 阻塞在 new-session：模拟「已完成前置配置、尚未创建」。
						// 等待响应 ctx 取消/超时（NewSession 的 15s ctx）与 Cleanup
						// 兜底，失败路径也能退出。
						close(aAtNewSession)
						select {
						case <-releaseA:
						case <-ctx.Done():
						}
						return "", "deliberate failure", &tmuxCmdError{
							sub:    args,
							stderr: "deliberate failure",
							err:    errors.New("exit 1"),
						}
					}
				}
			}
			return "", "", nil
		},
	}

	var wg sync.WaitGroup
	var errA, errB error
	wg.Add(2)
	go func() {
		defer wg.Done()
		errA = m.NewSession(SessionSpec{Name: nameA, Dir: "/tmp", CmdArgv: []string{"sleep", "1"}})
	}()
	waitForChan(t, aAtNewSession, 5*time.Second, "A's new-session arrival barrier")
	go func() {
		defer wg.Done()
		errB = m.NewSession(SessionSpec{Name: nameB, Dir: "/tmp", CmdArgv: []string{"sleep", "1"}})
	}()

	// 确定性 barrier：等待 B 真实到达 NewSession 的加锁点并被互斥锁阻塞——
	// 此后到放行 A 之前，B 不可能执行任何命令；期间命令流必须恰好是 A 的前三条
	// 记录（-V、前置链、new-session）。锁缺失时 B 直接跑完、该等待必然超时。
	waitForGoroutineParkedAt(t, "process.(*Manager).NewSession", "sync.(*Mutex)", 5*time.Second)
	mu.Lock()
	snap := append([][]string(nil), calls...)
	mu.Unlock()
	wantFrozen := [][]string{
		{"-V"},
		kbPrechain,
		{"new-session", "-d", "-s", nameA, "-c", "/tmp", "--", "'sleep' '1'"},
	}
	if len(snap) != len(wantFrozen) {
		t.Fatalf("calls = %v, want exactly %d records (A: -V, prechain, new-session) while B parked at the lock", snap, len(wantFrozen))
	}
	for i, w := range wantFrozen {
		if !equalArgs(snap[i], w) {
			t.Fatalf("calls[%d] = %v, want %v (B must not issue any command during A's transaction window)", i, snap[i], w)
		}
	}

	release()
	if !waitGroupWithTimeout(&wg, 10*time.Second) {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("NewSession goroutines did not finish within 10s; stacks:\n%s", buf[:n])
	}

	if errA == nil || !strings.Contains(errA.Error(), "new-session") {
		t.Errorf("errA = %v, want original creation error mentioning new-session", errA)
	}
	if errB != nil {
		t.Errorf("errB = %v, want nil (B must succeed on the preconfigured server)", errB)
	}

	mu.Lock()
	recs := append([][]string(nil), calls...)
	mu.Unlock()

	// 串行化后的完整命令序列是确定的：A 前置链 → A new-session(失败) → A 恢复
	// exit-empty on → B 前置链 → B new-session → B 尾部 EnsureServerOptions 三步。
	nsA := []string{"new-session", "-d", "-s", nameA, "-c", "/tmp", "--", "'sleep' '1'"}
	nsB := []string{"new-session", "-d", "-s", nameB, "-c", "/tmp", "--", "'sleep' '1'"}
	want := [][]string{
		{"-V"},
		kbPrechain,
		nsA,
		exitEmptyOn,
		{"-V"},
		kbPrechain,
		nsB,
		{"-V"},
		{"set-option", "-s", "get-clipboard", "off"},
		{"set-option", "-wg", "allow-passthrough", "off"},
		{"set-option", "-s", "set-clipboard", "on"},
		{"-V"},
		extendedKeysOn,
		exitEmptyOn,
	}
	if len(recs) != len(want) {
		t.Fatalf("calls = %v, want %d records in serialized order", recs, len(want))
	}
	for i, w := range want {
		if !equalArgs(recs[i], w) {
			t.Fatalf("recs[%d] = %v, want %v (A's failure recovery must land before B's prechain; no interleaving)", i, recs[i], w)
		}
	}
	// 恢复不错杀：全程无清理/重建命令（A 的失败恢复只恢复 exit-empty）。
	for i, c := range recs {
		for _, cmd := range kbCleanupCmds {
			if c[0] == cmd {
				t.Errorf("recs[%d] = %s: concurrent failure recovery MUST NOT cleanup/rebuild, calls=%v", i, cmd, recs)
			}
		}
	}
}

// TestNewSession_ConcurrentPublicEnsureServerOptionsSerialized 验证 design D1
// 并发约束（I1）的另一半：独立 EnsureServerOptions（reconcile 落点同路径）与
// NewSession 事务窗口也互斥——A 完成前置配置（exit-empty off）并阻塞在
// new-session 时，并发的公开 EnsureServerOptions MUST 被互斥锁阻塞，其恢复
// exit-empty on 不允许落在 A 前置链与 new-session 之间（否则空 server 提前退出、
// A 拉起未配置默认 server）。以 goroutine 栈证明 Ensure 真实阻塞在锁上，放行 A
// 后完整事务落定，Ensure 的三步才整体执行。
func TestNewSession_ConcurrentPublicEnsureServerOptionsSerialized(t *testing.T) {
	const nameA = "ocdeck-ic3-runtime"
	var (
		mu    sync.Mutex
		calls [][]string
		// aAtNewSession：A 的 new-session 已到达（A 前置配置已完成）。
		aAtNewSession = make(chan struct{})
		// releaseA 幂等放行（Cleanup 兜底，防 FailNow 提前退出泄漏 A）。
		releaseA = make(chan struct{})
	)
	release := func() {
		select {
		case <-releaseA:
		default:
			close(releaseA)
		}
	}
	t.Cleanup(release)

	m := &Manager{
		execTmuxFn: func(ctx context.Context, args ...string) (string, string, error) {
			mu.Lock()
			calls = append(calls, append([]string(nil), args...))
			mu.Unlock()
			if len(args) == 1 && args[0] == "-V" {
				return "tmux 3.7c\n", "", nil
			}
			if len(args) > 0 && args[0] == "new-session" {
				for _, a := range args {
					if a == nameA {
						close(aAtNewSession)
						select {
						case <-releaseA:
						case <-ctx.Done():
						}
						return "", "deliberate failure", &tmuxCmdError{
							sub:    args,
							stderr: "deliberate failure",
							err:    errors.New("exit 1"),
						}
					}
				}
			}
			return "", "", nil
		},
	}

	var wg sync.WaitGroup
	var errA, ensureErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		errA = m.NewSession(SessionSpec{Name: nameA, Dir: "/tmp", CmdArgv: []string{"sleep", "1"}})
	}()
	waitForChan(t, aAtNewSession, 5*time.Second, "A's new-session arrival barrier")
	go func() {
		defer wg.Done()
		ensureErr = m.EnsureServerOptions()
	}()

	// 确定性 barrier：Ensure 必须真实阻塞在 Manager 互斥锁上（而非已混入 A 的
	// 事务窗口发命令）。锁缺失时 Ensure 立即跑完三步、该等待必然超时。
	waitForGoroutineParkedAt(t, "process.(*Manager).EnsureServerOptions", "sync.(*Mutex)", 5*time.Second)
	mu.Lock()
	snap := append([][]string(nil), calls...)
	mu.Unlock()
	// 阻塞期间命令流必须恰好是 A 的前三条记录（Ensure 的一条命令都未出现）。
	wantFrozen := [][]string{
		{"-V"},
		kbPrechain,
		{"new-session", "-d", "-s", nameA, "-c", "/tmp", "--", "'sleep' '1'"},
	}
	if len(snap) != len(wantFrozen) {
		t.Fatalf("calls = %v, want exactly %d records (A only) while EnsureServerOptions parked at the lock", snap, len(wantFrozen))
	}
	for i, w := range wantFrozen {
		if !equalArgs(snap[i], w) {
			t.Fatalf("calls[%d] = %v, want %v (EnsureServerOptions must not issue any command during A's transaction window)", i, snap[i], w)
		}
	}

	release()
	if !waitGroupWithTimeout(&wg, 10*time.Second) {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("goroutines did not finish within 10s; stacks:\n%s", buf[:n])
	}

	if errA == nil || !strings.Contains(errA.Error(), "new-session") {
		t.Errorf("errA = %v, want original creation error mentioning new-session", errA)
	}
	if ensureErr != nil {
		t.Errorf("ensureErr = %v, want nil", ensureErr)
	}

	// 串行化后的完整序列确定：A 前置链 → A new-session(失败) → A 恢复 exit-empty
	// on → EnsureServerOptions 三步（剪贴板 / extended-keys / exit-empty）整体在其后。
	nsA := []string{"new-session", "-d", "-s", nameA, "-c", "/tmp", "--", "'sleep' '1'"}
	want := [][]string{
		{"-V"},
		kbPrechain,
		nsA,
		exitEmptyOn,
		{"-V"},
		{"set-option", "-s", "get-clipboard", "off"},
		{"set-option", "-wg", "allow-passthrough", "off"},
		{"set-option", "-s", "set-clipboard", "on"},
		{"-V"},
		extendedKeysOn,
		exitEmptyOn,
	}
	recs := recsSnap(&mu, &calls)
	if len(recs) != len(want) {
		t.Fatalf("calls = %v, want %d records in serialized order", recs, len(want))
	}
	for i, w := range want {
		if !equalArgs(recs[i], w) {
			t.Fatalf("recs[%d] = %v, want %v (EnsureServerOptions steps must run after A's full transaction)", i, recs[i], w)
		}
	}
}

// recsSnap 返回调用记录的快照（测试内部小 helper，统一加锁取值）。
func recsSnap(mu *sync.Mutex, calls *[][]string) [][]string {
	mu.Lock()
	defer mu.Unlock()
	return append([][]string(nil), *calls...)
}
