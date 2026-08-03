package process

import (
	"context"
	"errors"
	"strings"
	"testing"
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
		{"ocdeck-task1-serve", true},
		{"ocdeck-task-1-tui", true},
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
		// role 段收紧（design.md §2：role ∈ {serve, tui, shell-<n>}）
		{"ocdeck-task1-serve1", false},     // serve 不带后缀
		{"ocdeck-task1-tui-1", false},      // tui 不带后缀
		{"ocdeck-task1-shell", false},      // shell MUST 带 -<n>
		{"ocdeck-task1-shell-abc", false},  // shell-<n> 仅数字
		{"ocdeck-task1-shell-01", true},   // 0 开头仍为合法数字（shell-<n>）
		{"ocdeck-task1-worker", false},     // 非法 role
		{"ocdeck-task1-Serve", false},      // 大写
	}
	for _, c := range cases {
		err := ValidateSessionName(c.name)
		if (err == nil) != c.ok {
			t.Errorf("ValidateSessionName(%q) ok=%v, want %v (err=%v)", c.name, err == nil, c.ok, err)
		}
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
		{"A=B", false},   // = 非法
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