package task

// git_trace_test.go：测试期 git 子命令调用记录与故障注入（tasks 2.2/2.4「按子命令计数
// 断言副作用边界」）。
//
// 实现方式（零生产代码改动）：internal/infrastructure/git/exec.go 的 run 经
// exec.CommandContext(ctx, "git", ...)（:103）按 PATH 解析 git 可执行文件——测试在
// fixture 构建完成后把 shim 目录前置到 PATH，shim 先把 "$*" 追加到日志文件再 exec
// 真实 git，从而获得精确的调用次数与先后顺序。生产路径不受影响（t.Setenv 用后即恢复）。
//
// 故障注入：installGitCallRecorderFailing 可指定 argv 前缀（如 show <旧侧 blobOID>），
// shim 命中时记录后向 stderr 写入注入诊断并以 exit 128 失败（与 git 致命失败同型，
// 经 *exec.ExitError / commandError.stderr 透传），用于触达真实 fixture 不可达的
// git_error 分支（如「旧侧读取失败 MUST NOT 读新侧」）。

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// installGitCallRecorder 安装 PATH shim 开始记录 git 调用，返回 reads 函数读取
// 安装之后的调用序列（一行一次调用，argv 以空格连接，按发生顺序）。
// MUST 在 fixture 构建（runGitInit 等仓库操作）之后调用，使日志只含 Manager 发出的调用。
func installGitCallRecorder(t *testing.T) func() []string {
	t.Helper()
	return installGitCallRecorderFailing(t)
}

// installGitCallRecorderFailing 同 installGitCallRecorder，另按 failOn（argv 前缀，
// 如 "show", "<blobOID>"）注入故障：命中调用的 argv 以空格连接后以 failOn 序列开头时，
// shim 记录日志后向 stderr 写注入诊断并 exit 128，不透传真实 git。
func installGitCallRecorderFailing(t *testing.T, failOn ...string) func() []string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("lookpath git: %v", err)
	}
	shimDir := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "git-calls.log")

	var b strings.Builder
	fmt.Fprintf(&b, "#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %s\n", strconv.Quote(logFile))
	if len(failOn) > 0 {
		conds := make([]string, 0, len(failOn))
		injected := make([]string, 0, len(failOn))
		for i, a := range failOn {
			conds = append(conds, fmt.Sprintf(`[ "$%d" = %s ]`, i+1, strconv.Quote(a)))
			injected = append(injected, a)
		}
		injectedArgv := strings.Join(injected, " ")
		fmt.Fprintf(&b, "if %s; then\n  echo %s >&2\n  exit 128\nfi\n",
			strings.Join(conds, " && "), strconv.Quote("injected failure: "+injectedArgv))
	}
	fmt.Fprintf(&b, "exec %s \"$@\"\n", strconv.Quote(realGit))
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		data, err := os.ReadFile(logFile)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			t.Fatal(err)
		}
		var calls []string
		for _, line := range strings.Split(string(data), "\n") {
			if line != "" {
				calls = append(calls, line)
			}
		}
		return calls
	}
}
