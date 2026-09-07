package process

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestShellQuote(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"abc", "'abc'"},
		{"", "''"},
		{"a b", "'a b'"},
		{"a'b", "'a'\\''b'"},
		{"a\"b", "'a\"b'"},
		{`a\b`, `'a\b'`},
		{"$HOME", "'$HOME'"},
		{"`; rm -rf /", "'`; rm -rf /'"}, // backtick 原样保留，单引号内无注入
		{"$(whoami)", "'$(whoami)'"},
		{"a$b${c}", "'a$b${c}'"},
	}
	for _, c := range cases {
		got := shellQuote(c.in)
		if got != c.want {
			t.Errorf("shellQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBuildShellCommand(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"empty argv", []string{}, ""},
		{"single", []string{"opencode"}, "'opencode'"},
		{"with space arg", []string{"opencode", "serve"}, "'opencode' 'serve'"},
		{"quoted arg", []string{"sh", "-c", "echo hi"}, "'sh' '-c' 'echo hi'"},
		{"injection attempt", []string{"echo", "a'; rm -rf /; '"}, "'echo' 'a'\\''; rm -rf /; '\\'''"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildShellCommand(c.argv)
			if got != c.want {
				t.Errorf("buildShellCommand(%v) = %q, want %q", c.argv, got, c.want)
			}
		})
	}
}

func TestValidateSessionName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"ocdeck-task1-runtime", true},
		{"ocdeck-task1-serve", true}, // legacy：管理/清理路径仍接受
		{"ocdeck-task-1-tui", true},  // legacy
		{"ocdeck-my-task-shell-1", true},
		{"ocdeck-task1-shell-99", true},
		{"", false},
		{"ocdeck-", false},
		{"ocdeck-task", false}, // 无 role 段
		{"ocdeck-TASK-serve", false},
		{"ocdeck-task serve-serve", false}, // 空格
		{"ocdeck-task;rm-serve", false},
		{"foo-task-serve", false}, // 前缀错
		{"OCDECK-task-serve", false},
		{"ocdeck-task1-serve1", false},    // serve 不带后缀
		{"ocdeck-task1-tui-1", false},     // tui 不带后缀
		{"ocdeck-task1-shell", false},     // shell MUST 带 -<n>
		{"ocdeck-task1-shell-abc", false}, // shell-<n> 仅数字
		{"ocdeck-task1-shell-01", false},  // 前导零非法
		{"ocdeck-task1-shell-0", false},   // n 必须 > 0
		{"ocdeck-task1-worker", false},    // 非法 role
		{"ocdeck-task1-Serve", false},     // 大写
		{"ocdeck-task1-runtime-1", false}, // runtime 不带后缀
	}
	for _, c := range cases {
		err := ValidateSessionName(c.name)
		if (err == nil) != c.ok {
			t.Errorf("ValidateSessionName(%q) ok=%v, want %v (err=%v)", c.name, err == nil, c.ok, err)
		}
	}
}

func TestValidateNewSessionName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"ocdeck-task1-runtime", true},
		{"ocdeck-my-task-shell-1", true},
		{"ocdeck-task1-shell-99", true},
		{"ocdeck-task1-shell-01", false},
		{"ocdeck-task1-shell-0", false},
		{"ocdeck-task1-serve", false}, // legacy 不可新建
		{"ocdeck-task-1-tui", false},  // legacy 不可新建
		{"", false},
		{"ocdeck-task", false},
		{"ocdeck-task1-worker", false},
		{"ocdeck-task1-runtime-1", false},
		{"ocdeck-task1-shell", false},
	}
	for _, c := range cases {
		err := ValidateNewSessionName(c.name)
		if (err == nil) != c.ok {
			t.Errorf("ValidateNewSessionName(%q) ok=%v, want %v (err=%v)", c.name, err == nil, c.ok, err)
		}
	}
}

func TestNewSession_RejectsLegacySuffix(t *testing.T) {
	m := &Manager{
		execTmuxFn: func(context.Context, ...string) (string, string, error) {
			t.Fatal("execTmux must not run for rejected legacy names")
			return "", "", nil
		},
	}
	for _, name := range []string{"ocdeck-task1-serve", "ocdeck-task1-tui"} {
		err := m.NewSession(SessionSpec{
			Name:    name,
			Dir:     "/tmp",
			CmdArgv: []string{"sleep", "1"},
		})
		if err == nil {
			t.Errorf("NewSession(%q) err=nil, want reject legacy suffix", name)
		}
	}
}

func TestParseSessionName(t *testing.T) {
	cases := []struct {
		name   string
		taskID string
		suffix string
		ok     bool
	}{
		{"ocdeck-abc123-runtime", "abc123", "runtime", true},
		{"ocdeck-abc123-serve", "abc123", "serve", true},
		{"ocdeck-abc123-tui", "abc123", "tui", true},
		{"ocdeck-abc123-shell-1", "abc123", "shell-1", true},
		{"ocdeck-abc123-shell-12", "abc123", "shell-12", true},
		{"ocdeck-abc123-shell-0", "", "", false},
		{"ocdeck-abc123-shell-00", "", "", false},
		{"ocdeck-abc123-shell-01", "", "", false},
		{"ocdeck-abc123-shell-", "", "", false},
		{"ocdeck-abc123-shell-abc", "", "", false},
		{"ocdeck-abc123-shell-18446744073709551616", "", "", false}, // overflow
		{"ocdeck-abc123-worker", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		taskID, suffix, err := ParseSessionName(c.name)
		if c.ok {
			if err != nil || taskID != c.taskID || suffix != c.suffix {
				t.Errorf("ParseSessionName(%q)=(%q,%q,%v) want (%q,%q,nil)", c.name, taskID, suffix, err, c.taskID, c.suffix)
			}
			continue
		}
		if err == nil {
			t.Errorf("ParseSessionName(%q) ok, want error", c.name)
		}
	}
}

func TestHasSession_AcceptsLegacySuffix(t *testing.T) {
	var sawName string
	m := &Manager{
		execTmuxFn: func(_ context.Context, args ...string) (string, string, error) {
			for i, a := range args {
				if a == "-t" && i+1 < len(args) {
					sawName = args[i+1]
				}
			}
			return "", "", nil
		},
	}
	ok, err := m.HasSession("ocdeck-task1-serve")
	if err != nil || !ok {
		t.Fatalf("HasSession(legacy serve) ok=%v err=%v, want true/nil", ok, err)
	}
	if sawName != "=ocdeck-task1-serve" {
		t.Errorf("HasSession target = %q, want =ocdeck-task1-serve", sawName)
	}
}

func TestValidateEnvKey(t *testing.T) {
	cases := []struct {
		key string
		ok  bool
	}{
		{"PATH", true},
		{"OPENCODE_SERVER_PASSWORD", true},
		{"OCDECK_SERVE_PORT", true},
		{"MY_VAR-1", true},
		{"a.b.c", true},
		{"", false},
		{"1abc", false}, // 数字开头
		{"A B", false},  // 空格
		{"A=B", false},  // = 非法
	}
	for _, c := range cases {
		err := validateEnvKey(c.key)
		if (err == nil) != c.ok {
			t.Errorf("validateEnvKey(%q) ok=%v, want %v", c.key, err == nil, c.ok)
		}
	}
}

func TestSortedKeys(t *testing.T) {
	m := map[string]string{"b": "2", "a": "1", "c": "3"}
	got := sortedKeys(m)
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("len=%d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("sortedKeys[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

func TestEncodeDecodeTicket(t *testing.T) {
	tp := ticketPayload{PID: 12345, StartTime: "Thu Jul 30 13:07:37 2026", PGID: 12340}
	enc := encodeTicket(tp)
	if enc == "" {
		t.Fatal("encodeTicket returned empty")
	}
	dec, ok := decodeTicket(enc)
	if !ok {
		t.Fatalf("decodeTicket(%q) failed", enc)
	}
	if dec.PID != tp.PID || dec.StartTime != tp.StartTime || dec.PGID != tp.PGID {
		t.Errorf("round-trip mismatch: got %+v, want %+v", dec, tp)
	}
	// 无效 base64。
	if _, ok := decodeTicket("!!!invalid"); ok {
		t.Error("decodeTicket should fail on invalid base64")
	}
}

func TestCollectDescendants(t *testing.T) {
	tree := []procEntry{
		{1, 0}, {100, 1}, {101, 100}, {102, 101}, {200, 1}, {201, 200},
	}
	got := collectDescendants(tree, []int{100})
	want := map[int]bool{100: true, 101: true, 102: true}
	if len(got) != len(want) {
		t.Fatalf("collectDescendants(100) len=%d, want %d", len(got), len(want))
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected pid %d in descendants of 100", p)
		}
	}
	// 多 root。
	got2 := collectDescendants(tree, []int{100, 200})
	want2 := map[int]bool{100: true, 101: true, 102: true, 200: true, 201: true}
	if len(got2) != len(want2) {
		t.Fatalf("collectDescendants(100,200) len=%d, want %d", len(got2), len(want2))
	}
	for _, p := range got2 {
		if !want2[p] {
			t.Errorf("unexpected pid %d in descendants", p)
		}
	}
}

// --- B1：错误对象不得泄露 -e 值 ---

// TestTmuxCmdError_StripsEnvValues 验证 tmuxCmdError.Error() 永不含 -e 值。
// -e KEY=VALUE argv 会暴露 OPENCODE_SERVER_PASSWORD 与任务 env，错误对象 MUST
// 只保留命令名 + exit code + 有界 stderr（design.md §2 日志红线）。
func TestTmuxCmdError_StripsEnvValues(t *testing.T) {
	cases := []struct {
		name string
		sub  []string
	}{
		{"new-session with -e", []string{"new-session", "-d", "-s", "ocdeck-task-serve", "-e", "OPENCODE_SERVER_PASSWORD=s3cr3t", "-e", "FOO=bar", "--", "'sleep' '30'"}},
		{"has-session no -e", []string{"has-session", "-t", "=ocdeck-task-serve"}},
		{"show-environment", []string{"show-environment", "-t", "=ocdeck-task-serve", "FOO"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ce := &tmuxCmdError{
				sub:    c.sub,
				stderr: "some bounded stderr",
				err:    &execExitError{code: 1},
			}
			msg := ce.Error()
			if strings.Contains(msg, "OPENCODE_SERVER_PASSWORD") {
				t.Errorf("error leaked secret: %q", msg)
			}
			if strings.Contains(msg, "s3cr3t") {
				t.Errorf("error leaked secret value: %q", msg)
			}
			if strings.Contains(msg, "FOO=bar") {
				t.Errorf("error leaked env value: %q", msg)
			}
			// summary 仍应含命令名。
			if !strings.Contains(msg, "tmux") {
				t.Errorf("error missing tmux command name: %q", msg)
			}
		})
	}
}

// execExitError 是测试用最小 ExitError 实现（避免依赖 os/exec 的私有字段）。
type execExitError struct{ code int }

func (e *execExitError) Error() string { return "exit error" }
func (e *execExitError) ExitCode() int { return e.code }

// TestBoundedStderr_Truncates 验证超长 stderr 被截断到 stderrShowLimit。
func TestBoundedStderr_Truncates(t *testing.T) {
	long := strings.Repeat("x", stderrShowLimit+100)
	got := boundedStderr(long)
	if !strings.HasSuffix(got, "...(truncated)") {
		t.Errorf("long stderr not truncated, suffix=%q", got[len(got)-20:])
	}
	if boundedStderr("short") != "short" {
		t.Error("short stderr should pass through")
	}
}

// --- B3：reaper 错误分类与 KILL 前身份复核 ---

// fakePSProvider 是测试用 psProvider 注入，可精确控制每个 PID 的行为。
type fakePSProvider struct {
	alive       map[int]bool
	identities  map[int]string // pid → startTime
	identityErr map[int]error  // pid → 显式 identity 错误（优先于 identities）
	killed      []int          // 记录被 Kill 的 (pid)
	killErr     map[int]error
	killHook    func(pid int, sig string) // 可选：Kill 时回调（改 identity 等）
}

func (f *fakePSProvider) AllProcTree(ctx context.Context) ([]procEntry, error) {
	return nil, nil // reaper 单测不依赖 tree
}
func (f *fakePSProvider) ProcessIdentity(ctx context.Context, pid int) (string, int, error) {
	if e, ok := f.identityErr[pid]; ok {
		return "", 0, e
	}
	st, ok := f.identities[pid]
	if !ok {
		return "", 0, ErrProcNotFound
	}
	return st, pid, nil
}
func (f *fakePSProvider) Kill(pid int, sig string) error {
	if e, ok := f.killErr[pid]; ok {
		return e
	}
	f.killed = append(f.killed, pid)
	if f.killHook != nil {
		f.killHook(pid, sig)
	}
	if f.alive != nil && sig == "KILL" {
		f.alive[pid] = false
	}
	return nil
}
func (f *fakePSProvider) Alive(pid int) bool {
	return f.alive[pid]
}

// TestReapSurvivors_KILLBeforeRecheck 验证 KILL 前再次身份校验：TERM 宽限期
// 内 PID 被复用（startTime 变）则跳过 KILL，不误杀新进程（B3）。
func TestReapSurvivors_KILLBeforeRecheck(t *testing.T) {
	ps := &fakePSProvider{
		alive:      map[int]bool{500: true},
		identities: map[int]string{500: "T0"},
	}
	// TERM 信号后，模拟 PID 被新进程复用，startTime 变为 T1。
	ps.killHook = func(pid int, sig string) {
		if sig == "TERM" {
			ps.identities[pid] = "T1"
		}
	}
	m := &Manager{psProvider: ps}
	snap := &procSnapshot{
		pidSet: map[int]ticketPayload{500: {PID: 500, StartTime: "T0"}},
	}
	remaining := m.reapSurvivors(context.Background(), snap)
	// PID 被复用 → 不应 KILL，remaining 为空（该 PID 不再是目标）。
	if len(remaining) != 0 {
		t.Errorf("expected no remaining (pid reused), got %d", len(remaining))
	}
	// 验证只发了 TERM，没发 KILL（KILL 前因 startTime 变跳过）。
	if len(ps.killed) != 1 {
		t.Errorf("expected 1 kill (TERM only), got %d", len(ps.killed))
	}
}

// TestReapSurvivors_PSFailsConservative 验证 ps 失败（非 ErrProcNotFound）时
// 保守保留，不误报 clean（B3）。
func TestReapSurvivors_PSFailsConservative(t *testing.T) {
	ps := &fakePSProvider{
		alive:       map[int]bool{600: true},
		identityErr: map[int]error{600: errors.New("ps timeout")},
	}
	m := &Manager{psProvider: ps}
	snap := &procSnapshot{
		pidSet: map[int]ticketPayload{600: {PID: 600, StartTime: "T0"}},
	}
	remaining := m.reapSurvivors(context.Background(), snap)
	if len(remaining) != 1 {
		t.Fatalf("expected 1 remaining (ps fail conservative), got %d", len(remaining))
	}
	if remaining[0].PID != 600 {
		t.Errorf("remaining pid = %d, want 600", remaining[0].PID)
	}
}

// TestReapSurvivors_ProcNotFoundSkips 验证 PID 不存在（ErrProcNotFound）视为已退出跳过。
func TestReapSurvivors_ProcNotFoundSkips(t *testing.T) {
	ps := &fakePSProvider{
		alive: map[int]bool{700: true},
		// identityErr 不设 → ProcessIdentity 返回 ErrProcNotFound（identities 无 700）
	}
	m := &Manager{psProvider: ps}
	snap := &procSnapshot{
		pidSet: map[int]ticketPayload{700: {PID: 700, StartTime: "T0"}},
	}
	remaining := m.reapSurvivors(context.Background(), snap)
	if len(remaining) != 0 {
		t.Errorf("ErrProcNotFound should skip, got %d remaining", len(remaining))
	}
}

// TestRetryReap_UndecodableTicketCounted 验证无法解码的 ticket 计入失败而非丢弃（B3）。
func TestRetryReap_UndecodableTicketCounted(t *testing.T) {
	m := &Manager{psProvider: &fakePSProvider{alive: map[int]bool{}}}
	remaining, err := m.RetryReap([]string{"!!!invalid-ticket", "also-bad"})
	if err != nil {
		t.Fatalf("RetryReap: %v", err)
	}
	if len(remaining) != 2 {
		t.Errorf("undecodable tickets must be counted as failed; got %d remaining, want 2", len(remaining))
	}
}

// TestRetryReap_InvalidPIDIgnored 验证 ticket payload 中 PID<=0 被拒绝（B3：安全正整数）。
func TestRetryReap_InvalidPIDIgnored(t *testing.T) {
	// 手工构造 PID=0 与 PID=-1 的 ticket（decodeTicket 应拒绝）。
	m := &Manager{psProvider: &fakePSProvider{alive: map[int]bool{}}}
	badTickets := []string{
		encodeTicket(ticketPayload{PID: 0, StartTime: "T0"}),
		encodeTicket(ticketPayload{PID: -5, StartTime: "T0"}),
		encodeTicket(ticketPayload{PID: 100, StartTime: ""}), // 空 startTime
	}
	remaining, err := m.RetryReap(badTickets)
	if err != nil {
		t.Fatalf("RetryReap: %v", err)
	}
	if len(remaining) != 3 {
		t.Errorf("invalid-pid tickets must be counted as failed; got %d, want 3", len(remaining))
	}
}

// --- B4：isSessionNotFoundExit / isNoServerExit 区分 ---

func TestIsSessionNotFoundExit(t *testing.T) {
	cases := []struct {
		stderr string
		want   bool
	}{
		{"can't find session: ocdeck-task-serve", true},
		{"session not found", true},
		{"no server running", false},
		{"some other error", false},
		{"", false},
	}
	for _, c := range cases {
		ce := &tmuxCmdError{stderr: c.stderr}
		if got := isSessionNotFoundExit(ce); got != c.want {
			t.Errorf("isSessionNotFoundExit(%q) = %v, want %v", c.stderr, got, c.want)
		}
	}
}

func TestIsNoServerExit(t *testing.T) {
	cases := []struct {
		stderr string
		want   bool
	}{
		{"no server running", true},
		{"error connecting to /tmp/x/tmux-501/ocdeck (No such file or directory)", true},
		{"can't find session: foo", false},
		{"permission denied", false},
		{"", false},
	}
	for _, c := range cases {
		ce := &tmuxCmdError{stderr: c.stderr}
		if got := isNoServerExit(ce); got != c.want {
			t.Errorf("isNoServerExit(%q) = %v, want %v", c.stderr, got, c.want)
		}
	}
}

// TestTmuxExecEnv_InjectsTMUX_TMPDIR 验证 tmuxExecEnv 注入 TMUX_TMPDIR 且覆盖 baseEnv 同名项（B1）。
func TestTmuxExecEnv_InjectsTMUX_TMPDIR(t *testing.T) {
	m := &Manager{
		tmpdir:  "/custom/tmux",
		baseEnv: []string{"PATH=/usr/bin", "TMPDIR=/tmp", "TMUX_TMPDIR=/old"},
	}
	env := m.tmuxExecEnv()
	found := false
	for _, e := range env {
		if e == "TMUX_TMPDIR=/custom/tmux" {
			found = true
		}
		if e == "TMUX_TMPDIR=/old" {
			t.Error("old TMUX_TMPDIR not overwritten")
		}
	}
	if !found {
		t.Error("TMUX_TMPDIR not injected")
	}
}

// TestTmuxExecEnv_EmptyTmpdirPassthrough 验证 tmpdir 为空时透传 baseEnv。
func TestTmuxExecEnv_EmptyTmpdirPassthrough(t *testing.T) {
	m := &Manager{
		tmpdir:  "",
		baseEnv: []string{"PATH=/usr/bin"},
	}
	env := m.tmuxExecEnv()
	if len(env) != 1 || env[0] != "PATH=/usr/bin" {
		t.Errorf("empty tmpdir should passthrough baseEnv, got %v", env)
	}
}

// TestDefaultBaseEnv_LocaleDefault 验证 design.md D0 locale 兜底（透传 + 非空语义）：
//   - (a) 宿主无 LANG/LC_ALL/LC_CTYPE → 注入 LANG=en_US.UTF-8；
//   - (b) 宿主显式 LANG（含非 UTF-8 如 C）→ 原样透传不覆盖；
//   - (c) 宿主无 LANG 但有 LC_ALL 或 LC_CTYPE → 不注入（高位变量透传且原值一致）；
//   - (d) 宿主 LANG= 空串且 LC_ALL/LC_CTYPE 未设 → 注入默认（空串视为未设置）；
//   - (e) 宿主 LC_ALL= 空串且 LANG 未设 → 注入默认（空串视为未设置）。
func TestDefaultBaseEnv_LocaleDefault(t *testing.T) {
	const def = "LANG=en_US.UTF-8"
	// 固定一组非 locale 基础集值，避免宿主环境干扰断言。
	baseHost := map[string]string{
		"HOME": "/home/h", "USER": "u", "PATH": "/usr/bin",
		"SHELL": "/bin/sh", "TMPDIR": "/tmp",
	}
	envOf := func(extra map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) {
			if v, ok := extra[k]; ok {
				return v, true
			}
			v, ok := baseHost[k]
			return v, ok
		}
	}
	has := func(env []string, kv string) bool {
		for _, e := range env {
			if e == kv {
				return true
			}
		}
		return false
	}

	// (a) 无 locale 变量 → 注入默认。
	got := DefaultBaseEnv(envOf(nil))
	if !has(got, def) {
		t.Errorf("case a: expected %q in env, got %v", def, got)
	}

	// (b) 显式 LANG=C → 透传不覆盖、不注入默认。
	got = DefaultBaseEnv(envOf(map[string]string{"LANG": "C"}))
	if has(got, def) {
		t.Errorf("case b: default LANG should not be injected when LANG=C set, got %v", got)
	}
	if !has(got, "LANG=C") {
		t.Errorf("case b: LANG=C should be passed through, got %v", got)
	}

	// (c1) 无 LANG 但有 LC_ALL → 透传 LC_ALL 原值且不注入默认。
	got = DefaultBaseEnv(envOf(map[string]string{"LC_ALL": "en_US.UTF-8"}))
	if has(got, def) {
		t.Errorf("case c1: default LANG should not be injected when LC_ALL set, got %v", got)
	}
	if !has(got, "LC_ALL=en_US.UTF-8") {
		t.Errorf("case c1: LC_ALL=en_US.UTF-8 should be passed through, got %v", got)
	}

	// (c2) 无 LANG 但有 LC_CTYPE → 透传 LC_CTYPE 原值且不注入默认。
	got = DefaultBaseEnv(envOf(map[string]string{"LC_CTYPE": "en_US.UTF-8"}))
	if has(got, def) {
		t.Errorf("case c2: default LANG should not be injected when LC_CTYPE set, got %v", got)
	}
	if !has(got, "LC_CTYPE=en_US.UTF-8") {
		t.Errorf("case c2: LC_CTYPE=en_US.UTF-8 should be passed through, got %v", got)
	}

	// (d) LANG= 空串且 LC_ALL/LC_CTYPE 未设 → 注入默认（空串视为未设置）。
	got = DefaultBaseEnv(envOf(map[string]string{"LANG": ""}))
	if !has(got, def) {
		t.Errorf("case d: expected %q injected when LANG is empty string, got %v", def, got)
	}
	if has(got, "LANG=") {
		t.Errorf("case d: empty LANG= should not be passed through, got %v", got)
	}

	// (e) LC_ALL= 空串且 LANG 未设 → 注入默认（空串视为未设置）。
	got = DefaultBaseEnv(envOf(map[string]string{"LC_ALL": ""}))
	if !has(got, def) {
		t.Errorf("case e: expected %q injected when LC_ALL is empty string, got %v", def, got)
	}
	if has(got, "LC_ALL=") {
		t.Errorf("case e: empty LC_ALL= should not be passed through, got %v", got)
	}
}

func TestParseTmuxVersion(t *testing.T) {
	cases := []struct {
		in        string
		major     int
		minor     int
		ok        bool
		atLeast37 bool
	}{
		{"tmux 3.4", 3, 4, true, false},
		{"tmux 3.5a", 3, 5, true, false},
		{"tmux 3.7", 3, 7, true, true},
		{"tmux 3.6a", 3, 6, true, false},
		{"tmux 4.0", 4, 0, true, true},
		{"tmux next-3.4", 0, 0, false, false},
		{"bogus", 0, 0, false, false},
		{"tmux", 0, 0, false, false},
	}
	for _, c := range cases {
		maj, min, ok := parseTmuxVersion(c.in)
		if maj != c.major || min != c.minor || ok != c.ok {
			t.Errorf("parseTmuxVersion(%q) = %d,%d,%v; want %d,%d,%v", c.in, maj, min, ok, c.major, c.minor, c.ok)
		}
		if got := tmuxVersionAtLeast(c.in, 3, 7); got != c.atLeast37 {
			t.Errorf("tmuxVersionAtLeast(%q, 3, 7) = %v, want %v", c.in, got, c.atLeast37)
		}
	}
}

// newClipboardMockManager 构造带记录 execTmuxFn 的 Manager：
// -V 应答 version（空串表示 tmux -V 失败）；failArgs 中列出的每个 set-option
// （如 {"get-clipboard","off"}）返回 permission denied 错误；invalidOptions 中
// 列出的每个 set-option 返回 "invalid option" 错误（模拟 tmux < 3.3 无
// allow-passthrough）；noServer 时 set-option 返回"无 server"错误；其余调用成功。
// failArgs/invalidOptions 以 {option, value} 描述并忽略 -g/-wg flags（故障注入与
// 作用域解耦），调用是否带正确 flags 由断言侧比对完整 recorded args 验证。
func newClipboardMockManager(version string, failArgs, invalidOptions [][]string, noServer bool, calls *[][]string) *Manager {
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
			for _, fa := range failArgs {
				if isSetOptionCall(cp, fa) {
					return "", "permission denied", &tmuxCmdError{sub: args, stderr: "permission denied", err: errors.New("exit 1")}
				}
			}
			for _, io := range invalidOptions {
				if isSetOptionCall(cp, io) {
					return "", "invalid option: " + io[0], &tmuxCmdError{sub: args, stderr: "invalid option: " + io[0], err: errors.New("exit 1")}
				}
			}
			if noServer && len(args) > 0 && args[0] == "set-option" {
				return "", "no server running", &tmuxCmdError{
					sub:    args,
					stderr: "no server running on /tmp/tmux-0/ocdeck",
					err:    errors.New("exit 1"),
				}
			}
			return "", "", nil
		},
	}
}

// optCalls 提取记录中的 set-option 调用序列。
func optCalls(calls [][]string) [][]string {
	var out [][]string
	for _, c := range calls {
		if len(c) > 0 && c[0] == "set-option" {
			out = append(out, c)
		}
	}
	return out
}

// clipboardExternal = set-option -s set-clipboard external（fail-closed 目标态；
// set-clipboard 是 server 选项，-s 写 server 表）。
var clipboardExternal = []string{"set-option", "-s", "set-clipboard", "external"}

// TestEnsureServerOptions_FailClosedMatrix 验证按版本分段的 fail-closed 语义：
// >=3.7 走 [get-clipboard off → allow-passthrough off → set-clipboard on] 转发 raw；
// 3.3–3.6 走 [set-clipboard external → allow-passthrough on] 透传 DCS；其余保持/
// 恢复 external（3.2–3.6 的 get-clipboard 默认 buffer，on + buffer 会跨任务泄露
// paste buffer），补救失败 MUST 作为错误返回。
func TestEnsureServerOptions_FailClosedMatrix(t *testing.T) {
	clipboardPassthroughOff := []string{"set-option", "-wg", "allow-passthrough", "off"}
	clipboardPassthroughOn := []string{"set-option", "-wg", "allow-passthrough", "on"}

	// <3.3 或版本无法解析 → 恢复 external + 尝试关 passthrough（防不可解析实为
	// ≥3.3 的遗留开启），恢复成功时返回 nil。
	for _, v := range []string{"tmux 3.2", "tmux next-3.4", "bogus", "tmux"} {
		t.Run("below3.3/unparsable "+v, func(t *testing.T) {
			var calls [][]string
			m := newClipboardMockManager(v, nil, nil, false, &calls)
			if err := m.EnsureServerOptions(); err != nil {
				t.Fatalf("err = %v, want nil (feature unavailable is not an error)", err)
			}
			opts := optCalls(calls)
			if len(opts) != 2 || !equalArgs(opts[0], clipboardExternal) || !equalArgs(opts[1], clipboardPassthroughOff) {
				t.Fatalf("set-option calls = %v, want exactly [set-clipboard external, allow-passthrough off]", opts)
			}
		})
	}

	t.Run("below3.3 passthrough off unknown option skipped", func(t *testing.T) {
		var calls [][]string
		// tmux 3.2 无 allow-passthrough（3.3 引入）：invalid option 属预期，best-effort 跳过。
		m := newClipboardMockManager("tmux 3.2", nil, [][]string{{"allow-passthrough", "off"}}, false, &calls)
		if err := m.EnsureServerOptions(); err != nil {
			t.Fatalf("err = %v, want nil (unknown option is not an error)", err)
		}
		opts := optCalls(calls)
		if len(opts) != 2 || !equalArgs(opts[0], clipboardExternal) || !equalArgs(opts[1], clipboardPassthroughOff) {
			t.Fatalf("set-option calls = %v, want [set-clipboard external, allow-passthrough off]", opts)
		}
	})

	// 3.3–3.6（含 3.5a）：先确保 external 再开 passthrough（DCS 透传路径）。
	for _, v := range []string{"tmux 3.3", "tmux 3.4", "tmux 3.5a", "tmux 3.6a"} {
		t.Run("passthrough segment "+v, func(t *testing.T) {
			var calls [][]string
			m := newClipboardMockManager(v, nil, nil, false, &calls)
			if err := m.EnsureServerOptions(); err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			opts := optCalls(calls)
			if len(opts) != 2 || !equalArgs(opts[0], clipboardExternal) || !equalArgs(opts[1], clipboardPassthroughOn) {
				t.Fatalf("set-option calls = %v, want exactly [set-clipboard external, allow-passthrough on]", opts)
			}
		})
	}

	t.Run("tmux -V error", func(t *testing.T) {
		var calls [][]string
		m := newClipboardMockManager("", nil, nil, false, &calls)
		if err := m.EnsureServerOptions(); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		opts := optCalls(calls)
		if len(opts) != 2 || !equalArgs(opts[0], clipboardExternal) || !equalArgs(opts[1], clipboardPassthroughOff) {
			t.Fatalf("set-option calls = %v, want [set-clipboard external, allow-passthrough off]", opts)
		}
	})

	t.Run("3.7 happy path order", func(t *testing.T) {
		var calls [][]string
		m := newClipboardMockManager("tmux 3.7c", nil, nil, false, &calls)
		if err := m.EnsureServerOptions(); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		opts := optCalls(calls)
		wantGetOff := []string{"set-option", "-s", "get-clipboard", "off"}
		wantClipOn := []string{"set-option", "-s", "set-clipboard", "on"}
		if len(opts) != 3 || !equalArgs(opts[0], wantGetOff) || !equalArgs(opts[1], clipboardPassthroughOff) || !equalArgs(opts[2], wantClipOn) {
			t.Fatalf("set-option calls = %v, want [get-clipboard off, allow-passthrough off, set-clipboard on] in that order", opts)
		}
	})

	t.Run("3.7 get-clipboard off fails restores external", func(t *testing.T) {
		var calls [][]string
		m := newClipboardMockManager("tmux 3.7", [][]string{{"get-clipboard", "off"}}, nil, false, &calls)
		err := m.EnsureServerOptions()
		if err == nil {
			t.Fatal("want error when get-clipboard off fails")
		}
		opts := optCalls(calls)
		// 统一恢复：external + 关 passthrough（防任何一段遗留 on）。
		if len(opts) != 3 || !equalArgs(opts[0], []string{"set-option", "-s", "get-clipboard", "off"}) || !equalArgs(opts[1], clipboardExternal) || !equalArgs(opts[2], clipboardPassthroughOff) {
			t.Fatalf("set-option calls = %v, want [get-clipboard off, set-clipboard external, allow-passthrough off]", opts)
		}
	})

	t.Run("3.7 allow-passthrough off fails restores external", func(t *testing.T) {
		var calls [][]string
		m := newClipboardMockManager("tmux 3.7", [][]string{{"allow-passthrough", "off"}}, nil, false, &calls)
		err := m.EnsureServerOptions()
		if err == nil {
			t.Fatal("want error when allow-passthrough off fails (would leave raw+DCS dual write)")
		}
		if !strings.Contains(err.Error(), "allow-passthrough off") {
			t.Errorf("err = %v, want mention allow-passthrough off failure", err)
		}
		opts := optCalls(calls)
		// 统一恢复独立重试两项：external 成功、passthrough off 再次失败并入错误。
		if len(opts) != 4 || !equalArgs(opts[0], []string{"set-option", "-s", "get-clipboard", "off"}) || !equalArgs(opts[1], clipboardPassthroughOff) || !equalArgs(opts[2], clipboardExternal) || !equalArgs(opts[3], clipboardPassthroughOff) {
			t.Fatalf("set-option calls = %v, want [get-clipboard off, allow-passthrough off, set-clipboard external, allow-passthrough off]", opts)
		}
	})

	t.Run("below3.3 restore external fails surfaces error", func(t *testing.T) {
		var calls [][]string
		m := newClipboardMockManager("tmux 3.2", [][]string{{"set-clipboard", "external"}}, nil, false, &calls)
		err := m.EnsureServerOptions()
		// 恢复失败 MUST NOT 被吞掉：server 可能遗留 on + get-clipboard buffer。
		if err == nil {
			t.Fatal("want error when restore set-clipboard external fails")
		}
		if !strings.Contains(err.Error(), "set-clipboard external") {
			t.Errorf("err = %v, want mention restore set-clipboard external", err)
		}
		opts := optCalls(calls)
		if len(opts) != 2 || !equalArgs(opts[0], clipboardExternal) || !equalArgs(opts[1], clipboardPassthroughOff) {
			t.Fatalf("set-option calls = %v, want [set-clipboard external, allow-passthrough off]", opts)
		}
	})

	t.Run("passthrough segment first step external fails restores fail-closed", func(t *testing.T) {
		var calls [][]string
		m := newClipboardMockManager("tmux 3.3", [][]string{{"set-clipboard", "external"}}, nil, false, &calls)
		err := m.EnsureServerOptions()
		if err == nil {
			t.Fatal("want error when first step set-clipboard external fails")
		}
		// 原错误与恢复段错误都要出现：首步失败无法证明 raw 已关，恢复须独立重试
		// external 并收回可能遗留的 passthrough。
		if !strings.Contains(err.Error(), "set-clipboard external") || !strings.Contains(err.Error(), "restore set-clipboard external") {
			t.Errorf("err = %v, want mention original and restore set-clipboard external failures", err)
		}
		opts := optCalls(calls)
		if len(opts) != 3 || !equalArgs(opts[0], clipboardExternal) || !equalArgs(opts[1], clipboardExternal) || !equalArgs(opts[2], clipboardPassthroughOff) {
			t.Fatalf("set-option calls = %v, want [set-clipboard external, set-clipboard external retry, allow-passthrough off]", opts)
		}
	})

	t.Run("passthrough segment passthrough on fails restores off", func(t *testing.T) {
		var calls [][]string
		m := newClipboardMockManager("tmux 3.4", [][]string{{"allow-passthrough", "on"}}, nil, false, &calls)
		err := m.EnsureServerOptions()
		if err == nil {
			t.Fatal("want error when allow-passthrough on fails")
		}
		if !strings.Contains(err.Error(), "allow-passthrough on") {
			t.Errorf("err = %v, want mention allow-passthrough on failure", err)
		}
		opts := optCalls(calls)
		// 统一恢复独立尝试两项：external 幂等重试 + 收回 passthrough off。
		if len(opts) != 4 || !equalArgs(opts[0], clipboardExternal) || !equalArgs(opts[1], clipboardPassthroughOn) || !equalArgs(opts[2], clipboardExternal) || !equalArgs(opts[3], clipboardPassthroughOff) {
			t.Fatalf("set-option calls = %v, want [set-clipboard external, allow-passthrough on, set-clipboard external, allow-passthrough off]", opts)
		}
	})

	t.Run("passthrough segment passthrough on and restore both fail combines errors", func(t *testing.T) {
		var calls [][]string
		m := newClipboardMockManager("tmux 3.4", [][]string{{"allow-passthrough", "on"}, {"allow-passthrough", "off"}}, nil, false, &calls)
		err := m.EnsureServerOptions()
		if err == nil {
			t.Fatal("want error when both passthrough on and restore off fail")
		}
		if !strings.Contains(err.Error(), "allow-passthrough on") || !strings.Contains(err.Error(), "restore allow-passthrough off") {
			t.Errorf("err = %v, want combined errors mentioning both failures", err)
		}
		opts := optCalls(calls)
		if len(opts) != 4 || !equalArgs(opts[0], clipboardExternal) || !equalArgs(opts[1], clipboardPassthroughOn) || !equalArgs(opts[2], clipboardExternal) || !equalArgs(opts[3], clipboardPassthroughOff) {
			t.Fatalf("set-option calls = %v, want [set-clipboard external, allow-passthrough on, set-clipboard external, allow-passthrough off]", opts)
		}
	})

	t.Run("passthrough segment passthrough on no server returns without restore", func(t *testing.T) {
		var calls [][]string
		m := &Manager{
			execTmuxFn: func(_ context.Context, args ...string) (string, string, error) {
				cp := append([]string(nil), args...)
				calls = append(calls, cp)
				if len(args) == 1 && args[0] == "-V" {
					return "tmux 3.4\n", "", nil
				}
				if isSetOptionCall(cp, []string{"allow-passthrough", "on"}) {
					return "", "no server running on /tmp/tmux-0/ocdeck", &tmuxCmdError{
						sub:    args,
						stderr: "no server running on /tmp/tmux-0/ocdeck",
						err:    errors.New("exit 1"),
					}
				}
				return "", "", nil
			},
		}
		err := m.EnsureServerOptions()
		// 无 server 即无遗留状态可补救（external 已确保），MUST NOT 再走恢复路径。
		if !errors.Is(err, ErrNoTmuxServer) {
			t.Fatalf("err = %v, want ErrNoTmuxServer", err)
		}
		opts := optCalls(calls)
		if len(opts) != 2 || !equalArgs(opts[0], clipboardExternal) || !equalArgs(opts[1], clipboardPassthroughOn) {
			t.Fatalf("set-option calls = %v, want exactly [set-clipboard external, allow-passthrough on]", opts)
		}
	})

	t.Run("3.7 get-clipboard and restore both fail combines errors", func(t *testing.T) {
		var calls [][]string
		m := newClipboardMockManager("tmux 3.7", [][]string{{"get-clipboard", "off"}, {"set-clipboard", "external"}}, nil, false, &calls)
		err := m.EnsureServerOptions()
		if err == nil {
			t.Fatal("want error when both get-clipboard off and restore fail")
		}
		// 两段失败都要出现在返回错误里，且 errors.Join 保留两条 unwrap 链
		// （errors.As 能穿透到原始 tmuxCmdError，而非只有 restore 段）。
		if !strings.Contains(err.Error(), "get-clipboard") || !strings.Contains(err.Error(), "restore set-clipboard external") {
			t.Errorf("err = %v, want mention both get-clipboard failure and restore failure", err)
		}
		var ce *tmuxCmdError
		if !errors.As(err, &ce) {
			t.Errorf("errors.As tmuxCmdError failed; original unwrap chain lost (err=%v)", err)
		}
		opts := optCalls(calls)
		// 统一恢复在 external 失败后仍独立尝试关 passthrough。
		if len(opts) != 3 || !equalArgs(opts[0], []string{"set-option", "-s", "get-clipboard", "off"}) || !equalArgs(opts[1], clipboardExternal) || !equalArgs(opts[2], clipboardPassthroughOff) {
			t.Fatalf("set-option calls = %v, want [get-clipboard off, set-clipboard external, allow-passthrough off]", opts)
		}
	})

	t.Run("3.7 set-clipboard on fails after get-clipboard off state safe", func(t *testing.T) {
		var calls [][]string
		m := newClipboardMockManager("tmux 3.7", [][]string{{"set-clipboard", "on"}}, nil, false, &calls)
		err := m.EnsureServerOptions()
		if err == nil {
			t.Fatal("want error when set-clipboard on fails")
		}
		opts := optCalls(calls)
		// 状态安全：get-clipboard off 与 allow-passthrough off 已生效，set-clipboard on
		// 失败不会留下 on+buffer 或双写，MUST NOT 额外触发 restore external。
		if len(opts) != 3 || !equalArgs(opts[0], []string{"set-option", "-s", "get-clipboard", "off"}) || !equalArgs(opts[1], clipboardPassthroughOff) || !equalArgs(opts[2], []string{"set-option", "-s", "set-clipboard", "on"}) {
			t.Fatalf("set-option calls = %v, want exactly [get-clipboard off, allow-passthrough off, set-clipboard on]", opts)
		}
	})

	// 版本检查耗尽共享 ctx deadline 的场景：tmux -V 阻塞到 ctx 到期，补救 set-option
	// 若复用该 ctx 必然带着已过期的 deadline；fresh ctx 应给出未来的 deadline。
	t.Run("remediation uses fresh context after version deadline exhausted", func(t *testing.T) {
		type callRec struct {
			head     string
			deadline time.Time
		}
		var recs []callRec
		m := &Manager{
			execTmuxFn: func(ctx context.Context, args ...string) (string, string, error) {
				rec := callRec{head: args[0]}
				if dl, ok := ctx.Deadline(); ok {
					rec.deadline = dl
				}
				recs = append(recs, rec)
				if len(args) == 1 && args[0] == "-V" {
					<-ctx.Done() // 模拟 tmux -V 吃满整个版本检查 ctx
					return "", "deadline exceeded", &tmuxCmdError{sub: args, stderr: "context deadline exceeded", err: ctx.Err()}
				}
				return "", "", nil
			},
		}
		if err := m.EnsureServerOptions(); err != nil {
			t.Fatalf("err = %v, want nil (remediation succeeds with fresh ctx)", err)
		}
		if len(recs) < 2 {
			t.Fatalf("calls = %v, want at least [-V, set-option]", recs)
		}
		versionDl := recs[0].deadline
		remediationDl := recs[len(recs)-1].deadline
		if remediationDl.IsZero() {
			t.Fatal("remediation call has no deadline")
		}
		// 复用旧 ctx 时 remediation deadline 已过期（== versionDl，且此刻已成过去）。
		if !remediationDl.After(time.Now()) {
			t.Fatalf("remediation deadline %v not in the future; version ctx deadline was %v (shared ctx?)", remediationDl, versionDl)
		}
		if !remediationDl.After(versionDl) {
			t.Fatalf("remediation deadline %v must be re-issued after version deadline %v", remediationDl, versionDl)
		}
	})

	// 版本探测返回有效版本但耗尽共享 ctx：3.3–3.6 段首步 set-clipboard external
	// 用该 ctx 必然失败，统一恢复路径必须换 fresh ctx 重试成功。
	t.Run("3.3-3.6 first step fails after version deadline exhausted uses fresh ctx restore", func(t *testing.T) {
		type callRec struct {
			args     []string
			deadline time.Time
		}
		var recs []callRec
		m := &Manager{
			execTmuxFn: func(ctx context.Context, args ...string) (string, string, error) {
				rec := callRec{args: append([]string(nil), args...)}
				if dl, ok := ctx.Deadline(); ok {
					rec.deadline = dl
				}
				recs = append(recs, rec)
				if len(args) == 1 && args[0] == "-V" {
					<-ctx.Done() // 版本探测吃满共享 ctx，但版本本身有效
					return "tmux 3.4\n", "", nil
				}
				if args[0] == "set-option" && ctx.Err() != nil {
					return "", "context deadline exceeded", &tmuxCmdError{sub: args, stderr: "context deadline exceeded", err: ctx.Err()}
				}
				return "", "", nil
			},
		}
		err := m.EnsureServerOptions()
		if err == nil {
			t.Fatal("want error from the exhausted-ctx first step")
		}
		if !strings.Contains(err.Error(), "set-clipboard external") {
			t.Errorf("err = %v, want mention first step set-clipboard external failure", err)
		}
		if len(recs) != 4 {
			t.Fatalf("calls = %v, want exactly [-V, set external fail, set external retry, pt off]", recs)
		}
		if !equalArgs(recs[1].args, []string{"set-option", "-s", "set-clipboard", "external"}) ||
			!equalArgs(recs[2].args, []string{"set-option", "-s", "set-clipboard", "external"}) ||
			!equalArgs(recs[3].args, []string{"set-option", "-wg", "allow-passthrough", "off"}) {
			t.Fatalf("calls = %v, want [set external, set external retry, pt off] after -V", recs)
		}
		versionDl := recs[0].deadline
		// 恢复段两次调用各自持有独立 fresh deadline，均须在未来且晚于 version deadline。
		for i := 2; i <= 3; i++ {
			if !recs[i].deadline.After(time.Now()) || !recs[i].deadline.After(versionDl) {
				t.Fatalf("restore call %d deadline %v must be fresh (future, after version deadline %v)", i, recs[i].deadline, versionDl)
			}
		}
	})

	t.Run("no server", func(t *testing.T) {
		var calls [][]string
		m := newClipboardMockManager("tmux 3.7", nil, nil, true, &calls)
		err := m.EnsureServerOptions()
		if !errors.Is(err, ErrNoTmuxServer) {
			t.Fatalf("err = %v, want ErrNoTmuxServer", err)
		}
		opts := optCalls(calls)
		// 无 server 时无遗留状态可补救，MUST NOT 再尝试恢复 external。
		if len(opts) != 1 || !equalArgs(opts[0], []string{"set-option", "-s", "get-clipboard", "off"}) {
			t.Fatalf("set-option calls = %v, want exactly [get-clipboard off]", opts)
		}
	})

	t.Run("no server below3.3 stops after first ErrNoTmuxServer", func(t *testing.T) {
		var calls [][]string
		m := newClipboardMockManager("tmux 3.2", nil, nil, true, &calls)
		err := m.EnsureServerOptions()
		if !errors.Is(err, ErrNoTmuxServer) {
			t.Fatalf("err = %v, want ErrNoTmuxServer", err)
		}
		opts := optCalls(calls)
		// 首个补救（external）已确认无 server，无遗留状态，MUST NOT 再打第二枪。
		if len(opts) != 1 || !equalArgs(opts[0], clipboardExternal) {
			t.Fatalf("set-option calls = %v, want exactly [set-clipboard external]", opts)
		}
	})
}

// TestEnsureServerOptions_AllowPassthroughUnknownOptionVariants 验证 <3.3 上关闭
// allow-passthrough 遇到两种 "选项不存在" stderr 措辞都被 best-effort 跳过：
// "invalid option"（选项名匹配失败）与 "unknown option"（scope 解析失败）——
// allow-passthrough 于 3.3 引入，更老版本上 set 该选项必然报其一。
func TestEnsureServerOptions_AllowPassthroughUnknownOptionVariants(t *testing.T) {
	for _, stderr := range []string{"invalid option: allow-passthrough", "unknown option: allow-passthrough"} {
		t.Run(stderr, func(t *testing.T) {
			var calls [][]string
			m := &Manager{
				execTmuxFn: func(_ context.Context, args ...string) (string, string, error) {
					cp := append([]string(nil), args...)
					calls = append(calls, cp)
					if len(args) == 1 && args[0] == "-V" {
						return "tmux 3.2\n", "", nil
					}
					if isSetOptionCall(cp, []string{"allow-passthrough", "off"}) {
						return "", stderr, &tmuxCmdError{sub: args, stderr: stderr, err: errors.New("exit 1")}
					}
					return "", "", nil
				},
			}
			if err := m.EnsureServerOptions(); err != nil {
				t.Fatalf("err = %v, want nil (%q is not a real failure)", err, stderr)
			}
			opts := optCalls(calls)
			if len(opts) != 2 || !equalArgs(opts[0], clipboardExternal) || !equalArgs(opts[1], []string{"set-option", "-wg", "allow-passthrough", "off"}) {
				t.Fatalf("set-option calls = %v, want [set-clipboard external, allow-passthrough off]", opts)
			}
		})
	}
}

func TestNewSession_EnsureServerOptionsBestEffort(t *testing.T) {
	var calls [][]string
	// get-clipboard off 失败 → EnsureServerOptions 返回错误；NewSession 只记日志不失败。
	m := newClipboardMockManager("tmux 3.7", [][]string{{"get-clipboard", "off"}}, nil, false, &calls)
	err := m.NewSession(SessionSpec{
		Name:    "ocdeck-task1-runtime",
		Dir:     "/tmp",
		CmdArgv: []string{"sleep", "1"},
	})
	if err != nil {
		t.Fatalf("NewSession should succeed when EnsureServerOptions fails: %v", err)
	}
	if len(calls) < 2 || calls[0][0] != "new-session" {
		t.Fatalf("expected new-session then clipboard options, got %v", calls)
	}
	opts := optCalls(calls)
	if len(opts) == 0 || !equalArgs(opts[0], []string{"set-option", "-s", "get-clipboard", "off"}) {
		t.Errorf("first set-option = %v, want get-clipboard off (fail-closed order), all=%v", opts, calls)
	}
}

// isSetOptionCall 判断 args 是否为对 {option, value} 的 set-option 调用（flags 任意）。
func isSetOptionCall(args, optionValue []string) bool {
	return len(args) == 4 && args[0] == "set-option" && equalArgs(args[2:], optionValue)
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
