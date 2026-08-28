package notify

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"ocdeck/internal/domain/notification"
)

var _ notification.Channel = (*MacosChannel)(nil)

// fakeLookPath 返回仅探测到 found 中二进名的 lookPathFunc（未命中镜像
// exec.LookPath 的 *exec.Error{ErrNotFound}）。
func fakeLookPath(found ...string) lookPathFunc {
	return func(name string) (string, error) {
		for _, f := range found {
			if f == name {
				return "/fake/bin/" + f, nil
			}
		}
		return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
	}
}

// macosRunnerCall fake runner 记录的单次调用。
type macosRunnerCall struct {
	name    string
	args    []string
	timeout time.Duration
}

// macosRunnerFake 可注入 commandRunner fake：记录调用，err 非空时统一失败并
// 返回固定输出（验证失败 Err 携带截断诊断片段）。
type macosRunnerFake struct {
	calls []macosRunnerCall
	err   error
}

func (f *macosRunnerFake) run(_ context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	f.calls = append(f.calls, macosRunnerCall{name: name, args: append([]string(nil), args...), timeout: timeout})
	if f.err != nil {
		return "boom-output", f.err
	}
	return "", nil
}

// noopRunner 探测类测试用空 runner（不执行命令）。
func noopRunner(context.Context, time.Duration, string, ...string) (string, error) {
	return "", nil
}

func macosTestIntent() notification.Intent {
	return notification.Intent{
		TaskID:   "task-42",
		TaskName: "demo-task",
		Category: notification.CategoryIdle,
		Level:    notification.LevelActive,
		Title:    "任务已空闲",
		Body:     "demo-task 已空闲超过 60 秒",
		URL:      "http://127.0.0.1:18080/#/task/task-42",
	}
}

// TestMacosChannel_ProbeCapsMatrix 探测结果启动时缓存 + 能力位矩阵：
// terminal-notifier=Group|Replace、osascript=0、均无/非 darwin 不可用。
func TestMacosChannel_ProbeCapsMatrix(t *testing.T) {
	cases := []struct {
		name      string
		goos      string
		found     []string
		available bool
		caps      notification.Capability
	}{
		{"darwin both", "darwin", []string{"terminal-notifier", "osascript"}, true, notification.CapGroup | notification.CapReplace},
		{"darwin terminal-notifier only", "darwin", []string{"terminal-notifier"}, true, notification.CapGroup | notification.CapReplace},
		{"darwin osascript only", "darwin", []string{"osascript"}, true, 0},
		{"darwin none", "darwin", nil, false, 0},
		// 非 darwin 一律不可用（Caps 随探测到的实现、对不可用渠道无意义，不断言）。
		{"linux with both", "linux", []string{"terminal-notifier", "osascript"}, false, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newMacosChannel(tc.goos, fakeLookPath(tc.found...), noopRunner)
			if got := c.Available(); got != tc.available {
				t.Fatalf("Available = %v, want %v", got, tc.available)
			}
			if tc.caps >= 0 {
				if got := c.Caps(); got != tc.caps {
					t.Fatalf("Caps = %v, want %v", got, tc.caps)
				}
			}
			if c.Name() != "macos" {
				t.Fatalf("name = %q, want macos", c.Name())
			}
		})
	}
}

// TestMacosChannel_SendTerminalNotifier terminal-notifier 在场时经其投递，
// argv 逐字对齐 design D10（-group/-title/-message/-open/-sound），无 shell。
func TestMacosChannel_SendTerminalNotifier(t *testing.T) {
	runner := &macosRunnerFake{}
	c := newMacosChannel("darwin", fakeLookPath("terminal-notifier", "osascript"), runner.run)
	res := c.Send(context.Background(), macosTestIntent(), notification.ChannelConfig{})
	if !res.OK || res.Err != "" {
		t.Fatalf("expect success, got OK=%v Err=%q", res.OK, res.Err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(runner.calls))
	}
	call := runner.calls[0]
	if call.name != "/fake/bin/terminal-notifier" {
		t.Fatalf("command = %q, want resolved terminal-notifier path", call.name)
	}
	if call.timeout != 10*time.Second {
		t.Fatalf("timeout = %v, want 10s hard timeout", call.timeout)
	}
	in := macosTestIntent()
	wantArgs := []string{"-group", in.TaskID, "-title", in.Title, "-message", in.Body, "-open", in.URL, "-sound", "default"}
	if strings.Join(call.args, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Fatalf("args = %q, want %q", call.args, wantArgs)
	}
}

// TestMacosChannel_SendOsascript 仅未安装 terminal-notifier 时用 osascript：
// 固定 on run argv 脚本模板，title/body 经 argv 直传（不进脚本字符串）。
func TestMacosChannel_SendOsascript(t *testing.T) {
	runner := &macosRunnerFake{}
	c := newMacosChannel("darwin", fakeLookPath("osascript"), runner.run)
	res := c.Send(context.Background(), macosTestIntent(), notification.ChannelConfig{})
	if !res.OK || res.Err != "" {
		t.Fatalf("expect success, got OK=%v Err=%q", res.OK, res.Err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(runner.calls))
	}
	call := runner.calls[0]
	if call.name != "/fake/bin/osascript" {
		t.Fatalf("command = %q, want resolved osascript path", call.name)
	}
	if call.timeout != 10*time.Second {
		t.Fatalf("timeout = %v, want 10s hard timeout", call.timeout)
	}
	if len(call.args) != 4 || call.args[0] != "-e" || call.args[1] != osascriptNotifyScript {
		t.Fatalf("args = %q, want [-e <fixed script> title body]", call.args)
	}
	in := macosTestIntent()
	if call.args[2] != in.Title || call.args[3] != in.Body {
		t.Fatalf("title/body must be passed via argv, got %q", call.args[2:])
	}
	// 脚本模板契约：on run argv 形式，标题/正文按 argv 序号读取。
	if !strings.Contains(osascriptNotifyScript, "on run argv") {
		t.Fatalf("script must use `on run argv` form, got %q", osascriptNotifyScript)
	}
}

// TestMacosChannel_TerminalNotifierFailureNoFallback terminal-notifier 存在但
// 执行失败：仅记失败，MUST NOT 降级 osascript。
func TestMacosChannel_TerminalNotifierFailureNoFallback(t *testing.T) {
	runner := &macosRunnerFake{err: errors.New("exit status 1")}
	c := newMacosChannel("darwin", fakeLookPath("terminal-notifier", "osascript"), runner.run)
	res := c.Send(context.Background(), macosTestIntent(), notification.ChannelConfig{})
	if res.OK {
		t.Fatal("execution failure must fail")
	}
	if len(runner.calls) != 1 || runner.calls[0].name != "/fake/bin/terminal-notifier" {
		t.Fatalf("runner calls = %+v, want exactly one terminal-notifier call (no osascript fallback)", runner.calls)
	}
	if !strings.Contains(res.Err, "boom-output") {
		t.Fatalf("Err should carry truncated output snippet for diagnostics, got %q", res.Err)
	}
}

// TestMacosChannel_SendUnavailable 渠道不可用（非 darwin 或均未安装）时 Send
// 直接失败、不执行任何命令（skipped 判定属上层）。
func TestMacosChannel_SendUnavailable(t *testing.T) {
	cases := []struct {
		name  string
		goos  string
		found []string
	}{
		{"linux", "linux", []string{"terminal-notifier"}},
		{"darwin none installed", "darwin", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &macosRunnerFake{}
			c := newMacosChannel(tc.goos, fakeLookPath(tc.found...), runner.run)
			res := c.Send(context.Background(), macosTestIntent(), notification.ChannelConfig{})
			if res.OK {
				t.Fatal("unavailable channel must fail")
			}
			if len(runner.calls) != 0 {
				t.Fatalf("runner calls = %+v, want none", runner.calls)
			}
		})
	}
}

// TestRunCommand_HardTimeout 真实 runner 的统一硬超时：进程到时被杀。
func TestRunCommand_HardTimeout(t *testing.T) {
	start := time.Now()
	_, err := runCommand(context.Background(), 100*time.Millisecond, "sleep", "2")
	if err == nil {
		t.Fatal("slow command must be killed at timeout")
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("hard timeout enforced too late: %v", el)
	}
}

// TestRunCommand_OutputCap 输出读取上限 4KB：超出部分丢弃，保留前 4KB。
func TestRunCommand_OutputCap(t *testing.T) {
	out, err := runCommand(context.Background(), 5*time.Second, "head", "-c", "8192", "/dev/zero")
	if err != nil {
		t.Fatalf("head must succeed: %v", err)
	}
	if len(out) != macosMaxOutputBytes {
		t.Fatalf("output len = %d, want capped at %d", len(out), macosMaxOutputBytes)
	}
}

// TestRunCommand_OverrunKeepsDraining 持续超量输出（无限写）不阻塞收尾：
// 超时被杀后返回，输出截断在 4KB 内。
func TestRunCommand_OverrunKeepsDraining(t *testing.T) {
	start := time.Now()
	out, err := runCommand(context.Background(), 300*time.Millisecond, "yes")
	if err == nil {
		t.Fatal("yes must exit via timeout kill")
	}
	if len(out) > macosMaxOutputBytes {
		t.Fatalf("output len = %d, want <= %d", len(out), macosMaxOutputBytes)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("overrunning command must not block past timeout: %v", el)
	}
}
