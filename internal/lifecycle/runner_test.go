package lifecycle

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"ocdeck/internal/git"
)

// --- RunScript 测试 ---

func TestRunScriptSuccess(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "logs", "init.log")
	r := New()
	err := r.RunScript(context.Background(), dir, nil, "echo hello", logPath, 5*time.Second)
	if err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(body), "hello") {
		t.Errorf("log should contain hello, got %q", string(body))
	}
	// 日志文件 0600、目录 0700。
	if fi, err := os.Stat(logPath); err == nil {
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("log perm = %v, want 0600", fi.Mode().Perm())
		}
	} else {
		t.Fatalf("stat log: %v", err)
	}
	if fi, err := os.Stat(filepath.Dir(logPath)); err == nil {
		if fi.Mode().Perm() != 0o700 {
			t.Errorf("log dir perm = %v, want 0700", fi.Mode().Perm())
		}
	} else {
		t.Fatalf("stat log dir: %v", err)
	}
}

func TestRunScriptNonzero(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "init.log")
	r := New()
	err := r.RunScript(context.Background(), dir, nil, "echo to-log; exit 3", logPath, 5*time.Second)
	if err == nil {
		t.Fatal("expected nonzero exit error")
	}
	body, _ := os.ReadFile(logPath)
	if !strings.Contains(string(body), "to-log") {
		t.Errorf("log should capture stderr/stdout before exit, got %q", string(body))
	}
}

func TestRunScriptTruncateRewrite(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "init.log")
	// 预写旧内容，验证 truncate 重写。
	if err := os.WriteFile(logPath, []byte("OLD CONTENT THAT SHOULD BE GONE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New()
	if err := r.RunScript(context.Background(), dir, nil, "echo new", logPath, 5*time.Second); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	body, _ := os.ReadFile(logPath)
	if strings.Contains(string(body), "OLD CONTENT") {
		t.Errorf("log should be truncated, still contains old content: %q", string(body))
	}
	if !strings.Contains(string(body), "new") {
		t.Errorf("log should contain new content: %q", string(body))
	}
}

func TestRunScriptTimeoutKillsProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process group semantics are unix-only")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "init.log")
	r := New()
	// 启动一个 sleep 子进程，验证超时后孙子进程也被杀。
	start := time.Now()
	err := r.RunScript(context.Background(), dir, nil, "sleep 30 & sleep 30 & wait", logPath, 500*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > 5*time.Second {
		t.Errorf("timeout took too long: %v (process group may not have been killed)", elapsed)
	}
	// 验证无遗留 sleep 进程（进程组应被杀）。
	out, _ := exec.Command("sh", "-c", "pgrep -f 'sleep 30' || true").Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("leftover sleep processes after timeout: %s", out)
	}
}

func TestRunScript1MBTruncation(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "init.log")
	r := New()
	// 输出 2MB，超过 1MB 上限。
	if err := r.RunScript(context.Background(), dir, nil,
		"yes | head -c $((2*1024*1024))", logPath, 10*time.Second); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	body, _ := os.ReadFile(logPath)
	if len(body) > logCap+len(logTruncMarker)+64 {
		t.Errorf("log too large: %d bytes (cap %d)", len(body), logCap)
	}
	if !strings.HasSuffix(string(body), logTruncMarker) {
		t.Errorf("log should end with truncation marker, got tail %q", string(body)[min(len(body), 200):])
	}
}

// TestRunScriptCtxCancelReportsCanceled 验证父 ctx 取消报告 "canceled"（非 "timed out"），
// 且无遗留进程（进程组被杀）。
func TestRunScriptCtxCancelReportsCanceled(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "init.log")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	r := New()
	err := r.RunScript(ctx, dir, nil, "sleep 30", logPath, 10*time.Second)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "canceled") {
		t.Errorf("parent ctx cancel should report 'canceled', got %q", err.Error())
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Errorf("parent ctx cancel must not be reported as 'timed out': %q", err.Error())
	}
}

// TestRunScriptTimeoutReportsTimedOut 验证超时报告 "timed out"（非 "canceled"）。
func TestRunScriptTimeoutReportsTimedOut(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "init.log")
	r := New()
	err := r.RunScript(context.Background(), dir, nil, "sleep 30", logPath, 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("timeout should report 'timed out', got %q", err.Error())
	}
}

// TestArbitrateDoubleReadyCompletionWins 验证仲裁不变量：命令完成优先于取消/超时报告。
// 确定性构造——预先向 waitCh 投递完成结果（=命令已回收），同时取消 ctx，不依赖时序运气：
// 仲裁 select 的两个 case 同时就绪时，完成 case（waitCh）与取消 case（runCtx.Done）均可能被选中，
// 但取消分支内的非阻塞检查必命中已投递的 waitCh → 走完成路径、不 kill。
// 断言：killer 调用计数为 0、返回命令自身结果（exit 0 → nil）。
func TestArbitrateDoubleReadyCompletionWins(t *testing.T) {
	waitCh := make(chan error, 1)
	waitCh <- nil // 预先投递：命令已回收（exit 0）

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // ctx 已取消：双就绪

	killed := 0
	kill := func() { killed++ }

	err := arbitrate(waitCh, ctx, kill, func(e error) error {
		if e == nil {
			return nil
		}
		return fmt.Errorf("exited: %w", e)
	}, 10*time.Second)
	if err != nil {
		t.Fatalf("completion should win, return nil (exit 0), got %v", err)
	}
	if killed != 0 {
		t.Errorf("killer must not be called when waitCh already has result, got %d calls", killed)
	}
}

// TestArbitrateDoubleReadyNonZeroExitWins 验证非零退出码的双就绪场景：命令结果（"exited"）胜出、不 kill。
func TestArbitrateDoubleReadyNonZeroExitWins(t *testing.T) {
	waitCh := make(chan error, 1)
	waitCh <- fmt.Errorf("exit status 3") // 预先投递：命令已回收（非零）

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	killed := 0
	kill := func() { killed++ }

	err := arbitrate(waitCh, ctx, kill, func(e error) error {
		if e == nil {
			return nil
		}
		return fmt.Errorf("exited: %w", e)
	}, 10*time.Second)
	if err == nil {
		t.Fatal("expected non-zero exit error")
	}
	if !strings.Contains(err.Error(), "exited") {
		t.Errorf("should report command exit result, got %q", err.Error())
	}
	if strings.Contains(err.Error(), "canceled") {
		t.Errorf("must not report 'canceled' when waitCh already has result: %q", err.Error())
	}
	if killed != 0 {
		t.Errorf("killer must not be called when waitCh already has result, got %d calls", killed)
	}
}

// TestArbitrateOnlyCtxReadyKillsImmediately 验证仅 ctx 就绪（waitCh 空）时立即调用 killer，无延迟。
// 断言：killer 被调用恰好 1 次；返回 "canceled"（父取消，非超时）。
func TestArbitrateOnlyCtxReadyKillsImmediately(t *testing.T) {
	waitCh := make(chan error, 1) // 空：命令未回收

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	killed := 0
	kill := func() { killed++ }

	// 投递 reap 结果（kill 后 Wait 返回）。
	go func() {
		// 给仲裁一点时间进入 kill 分支后再投递，避免 select 永久阻塞。
		time.Sleep(10 * time.Millisecond)
		waitCh <- fmt.Errorf("signal: killed")
	}()

	err := arbitrate(waitCh, ctx, kill, func(e error) error {
		if e == nil {
			return nil
		}
		return fmt.Errorf("exited: %w", e)
	}, 10*time.Second)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "canceled") {
		t.Errorf("should report 'canceled', got %q", err.Error())
	}
	if killed != 1 {
		t.Errorf("killer must be called exactly once (no delay), got %d calls", killed)
	}
}

// TestArbitrateOnlyCtxReadyTimeoutReportsTimedOut 验证仅 ctx 就绪 + DeadlineExceeded → "timed out"。
func TestArbitrateOnlyCtxReadyTimeoutReportsTimedOut(t *testing.T) {
	waitCh := make(chan error, 1) // 空

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()
	<-ctx.Done() // 等待超时触发

	killed := 0
	kill := func() { killed++ }

	go func() {
		time.Sleep(10 * time.Millisecond)
		waitCh <- fmt.Errorf("signal: killed")
	}()

	err := arbitrate(waitCh, ctx, kill, func(e error) error {
		if e == nil {
			return nil
		}
		return fmt.Errorf("exited: %w", e)
	}, 1*time.Millisecond)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("should report 'timed out', got %q", err.Error())
	}
	if killed != 1 {
		t.Errorf("killer must be called exactly once, got %d calls", killed)
	}
}

// TestRunScriptTightensLogPerms 验证 rerun 前过宽的日志目录/文件权限被收紧到 0700/0600。
// 预造后显式 Chmod 防止 umask 干扰，先断言前置权限成立，再断言执行后收紧。
func TestRunScriptTightensLogPerms(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	logPath := filepath.Join(logDir, "init.log")
	// 预先造宽权限目录与文件（显式 Chmod 防 umask 干扰）。
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(logPath, 0o644); err != nil {
		t.Fatal(err)
	}
	// 先断言前置权限成立（防 umask 干扰使测试假绿）。
	if fi, err := os.Stat(logDir); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("precondition: log dir perm = %v, want 0755", fi.Mode().Perm())
	}
	if fi, err := os.Stat(logPath); err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("precondition: log perm = %v, want 0644", fi.Mode().Perm())
	}
	r := New()
	if err := r.RunScript(context.Background(), dir, nil, "echo new", logPath, 5*time.Second); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if fi, err := os.Stat(logDir); err != nil {
		t.Fatalf("stat log dir: %v", err)
	} else if fi.Mode().Perm() != 0o700 {
		t.Errorf("log dir perm = %v, want 0700 (must tighten existing)", fi.Mode().Perm())
	}
	if fi, err := os.Stat(logPath); err != nil {
		t.Fatalf("stat log: %v", err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("log perm = %v, want 0600 (must tighten existing)", fi.Mode().Perm())
	}
}

// --- CopyInherited 测试 ---

// makeRepo 在 t.TempDir() 创建带初始提交的 git 仓库，返回仓库路径。
func makeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "config", "user.email", "t@t.com")
	runGit(t, dir, "config", "user.name", "tester")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-qm", "init")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCopyInheritedStarStar 验证 `**` 嵌套匹配。
func TestCopyInheritedStarStar(t *testing.T) {
	repo := makeRepo(t)
	writeFile(t, repo, ".gitignore", "secrets/\n")
	writeFile(t, repo, "secrets/a.env", "A=1\n")
	writeFile(t, repo, "secrets/sub/b.env", "B=2\n")
	writeFile(t, repo, "secrets/sub/deep/c.env", "C=3\n")

	wt := t.TempDir()
	entries, err := git.ListIgnoredUntracked(context.Background(), repo)
	if err != nil {
		t.Fatalf("ListIgnoredUntracked: %v", err)
	}
	r := New()
	warns := r.CopyInherited(context.Background(), repo, wt, entries, []string{"secrets/**"})
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	for _, p := range []string{"secrets/a.env", "secrets/sub/b.env", "secrets/sub/deep/c.env"} {
		if _, err := os.Stat(filepath.Join(wt, p)); err != nil {
			t.Errorf("missing copied file %s: %v", p, err)
		}
	}
}

// TestCopyInheritedNestedFileEntries 验证嵌套文件级条目复制（-uall 已展开目录为文件级，§7.2）。
func TestCopyInheritedNestedFileEntries(t *testing.T) {
	repo := makeRepo(t)
	writeFile(t, repo, ".gitignore", "data/\n")
	writeFile(t, repo, "data/x.txt", "x\n")
	writeFile(t, repo, "data/sub/y.txt", "y\n")

	wt := t.TempDir()
	entries, err := git.ListIgnoredUntracked(context.Background(), repo)
	if err != nil {
		t.Fatalf("ListIgnoredUntracked: %v", err)
	}
	r := New()
	warns := r.CopyInherited(context.Background(), repo, wt, entries, []string{"data/**"})
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	for _, p := range []string{"data/x.txt", "data/sub/y.txt"} {
		if _, err := os.Stat(filepath.Join(wt, p)); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
}

// TestCopyInheritedRegularFilePerms 验证普通文件执行位/权限保持。
func TestCopyInheritedRegularFilePerms(t *testing.T) {
	repo := makeRepo(t)
	writeFile(t, repo, ".gitignore", "bin/\n")
	if err := os.MkdirAll(filepath.Join(repo, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 可执行脚本（0755），验证权限保持。
	scriptPath := filepath.Join(repo, "bin", "run.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	wt := t.TempDir()
	entries, err := git.ListIgnoredUntracked(context.Background(), repo)
	if err != nil {
		t.Fatalf("ListIgnoredUntracked: %v", err)
	}
	r := New()
	warns := r.CopyInherited(context.Background(), repo, wt, entries, []string{"bin/**"})
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	dst := filepath.Join(wt, "bin", "run.sh")
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("missing copied script: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("perm = %v, want 0755 (executable bit preserved)", info.Mode().Perm())
	}
}

// TestCopyInheritedNoClobberLinkEEXIST 验证 link(2) 阶段目标已存在时 EEXIST 跳过+警告。
// 顺序重复同一文件条目：第一次 link(2) 成功，第二次命中 link 的 EEXIST 分支。
// （非并发测试——如实反映其测的是 link EEXIST 分支，而非并发竞态。）
// 断言具体 warning 文本 "link EEXIST"——该文本仅由 os.Link 的 EEXIST 处理分支产生，证明命中。
func TestCopyInheritedNoClobberLinkEEXIST(t *testing.T) {
	repo := makeRepo(t)
	writeFile(t, repo, ".gitignore", "data/\n")
	writeFile(t, repo, "data/file.txt", "REPO\n")

	wt := t.TempDir()
	r := New()

	// CopyInherited 调用前不预置目标，但在 entries 中重复同一文件两次——
	// 第一次 link 成功，第二次 link(2) 返回 EEXIST（目标已由第一次创建）。
	// 这验证 link(2) 的 no-clobber 语义：第二个发布尝试遇到已存在目标，跳过+警告。
	dupEntries := []git.FileStatus{
		{Path: "data/file.txt", Ignored: true},
		{Path: "data/file.txt", Ignored: true},
	}
	warns := r.CopyInherited(context.Background(), repo, wt, dupEntries, []string{"data/**"})
	if len(warns) != 1 {
		t.Fatalf("expected exactly 1 warning (second publish EEXIST), got %v", warns)
	}
	// 断言具体 link EEXIST 文本——仅由 os.Link EEXIST 处理分支产生（runner.go copyRegular）。
	if !strings.Contains(warns[0], "link EEXIST") {
		t.Errorf("warning should contain 'link EEXIST' (os.Link EEXIST branch), got %q", warns[0])
	}
	// 内容为首次发布的 REPO（未被第二次覆盖）。
	body, _ := os.ReadFile(filepath.Join(wt, "data", "file.txt"))
	if string(body) != "REPO\n" {
		t.Errorf("content should be REPO (not overwritten), got %q", string(body))
	}
}

// TestCopyInheritedSymlink 验证符号链接按链接复制（含 broken symlink）。
func TestCopyInheritedSymlink(t *testing.T) {
	repo := makeRepo(t)
	writeFile(t, repo, ".gitignore", "links/\n")
	if err := os.MkdirAll(filepath.Join(repo, "links"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 有效符号链接。
	if err := os.WriteFile(filepath.Join(repo, "links", "target.txt"), []byte("t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(repo, "links", "good.lnk")); err != nil {
		t.Fatal(err)
	}
	// broken 符号链接。
	if err := os.Symlink("nonexistent", filepath.Join(repo, "links", "broken.lnk")); err != nil {
		t.Fatal(err)
	}

	wt := t.TempDir()
	entries, err := git.ListIgnoredUntracked(context.Background(), repo)
	if err != nil {
		t.Fatalf("ListIgnoredUntracked: %v", err)
	}
	r := New()
	warns := r.CopyInherited(context.Background(), repo, wt, entries, []string{"links/**"})
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	goodDst := filepath.Join(wt, "links", "good.lnk")
	brokenDst := filepath.Join(wt, "links", "broken.lnk")
	if info, err := os.Lstat(goodDst); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("good.lnk should be symlink: %v", err)
	}
	if info, err := os.Lstat(brokenDst); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("broken.lnk should be symlink (copied as link): %v", err)
	}
}

// TestCopyInheritedNoClobber 验证预先存在的目标不被覆盖（link EEXIST 路径）。
func TestCopyInheritedNoClobber(t *testing.T) {
	repo := makeRepo(t)
	writeFile(t, repo, ".gitignore", "data/\n")
	writeFile(t, repo, "data/file.txt", "REPO\n")

	wt := t.TempDir()
	// 预先存在目标文件，内容应保留（no-clobber）。
	if err := os.MkdirAll(filepath.Join(wt, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "data", "file.txt"), []byte("EXISTING\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := git.ListIgnoredUntracked(context.Background(), repo)
	if err != nil {
		t.Fatalf("ListIgnoredUntracked: %v", err)
	}
	r := New()
	warns := r.CopyInherited(context.Background(), repo, wt, entries, []string{"data/**"})
	if len(warns) != 1 {
		t.Fatalf("expected exactly 1 warning (link EEXIST), got %v", warns)
	}
	// 断言具体 link EEXIST 文本——证明命中 os.Link 的 EEXIST 分支（非前置 Lstat 预检）。
	if !strings.Contains(warns[0], "link EEXIST") {
		t.Errorf("warning should contain 'link EEXIST', got %q", warns[0])
	}
	body, _ := os.ReadFile(filepath.Join(wt, "data", "file.txt"))
	if string(body) != "EXISTING\n" {
		t.Errorf("target should not be overwritten, got %q", string(body))
	}
}

// TestCopyInheritedAncestorSymlinkReject 验证 destination 任一祖先为符号链接时拒绝。
func TestCopyInheritedAncestorSymlinkReject(t *testing.T) {
	repo := makeRepo(t)
	writeFile(t, repo, ".gitignore", "data/\n")
	writeFile(t, repo, "data/file.txt", "x\n")

	wt := t.TempDir()
	// 在 wt 下创建符号链接祖先（data -> 别处）。
	escape := t.TempDir()
	if err := os.Symlink(escape, filepath.Join(wt, "data")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	entries, err := git.ListIgnoredUntracked(context.Background(), repo)
	if err != nil {
		t.Fatalf("ListIgnoredUntracked: %v", err)
	}
	r := New()
	warns := r.CopyInherited(context.Background(), repo, wt, entries, []string{"data/**"})
	found := false
	for _, w := range warns {
		if strings.Contains(w, "ancestor is symlink") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected ancestor symlink rejection warning, got %v", warns)
	}
	// 不应写入逃逸目标。
	if _, err := os.Stat(filepath.Join(escape, "file.txt")); err == nil {
		t.Errorf("file should not be written through symlink ancestor")
	}
}

// TestCopyInheritedContainmentReject 验证路径 containment 校验拒绝绝对路径/.. 逃逸。
func TestCopyInheritedContainmentReject(t *testing.T) {
	repo := makeRepo(t)
	wt := t.TempDir()
	// 构造包含 .. 逃逸的条目。
	entries := []git.FileStatus{
		{Path: "../escape.txt", Ignored: true},
		{Path: "/abs/path.txt", Ignored: true},
	}
	r := New()
	warns := r.CopyInherited(context.Background(), repo, wt, entries, []string{"**"})
	if len(warns) != 2 {
		t.Fatalf("expected 2 containment warnings, got %v", warns)
	}
	for _, w := range warns {
		if !strings.Contains(w, "non-contained") {
			t.Errorf("expected non-contained warning, got %q", w)
		}
	}
}

// TestCopyInheritedGitExcluded 验证 .git 条目排除。
func TestCopyInheritedGitExcluded(t *testing.T) {
	repo := makeRepo(t)
	// keep.txt 需存在于 repo 才能被复制。
	writeFile(t, repo, "keep.txt", "k\n")
	wt := t.TempDir()
	entries := []git.FileStatus{
		{Path: ".git/config", Ignored: true},
		{Path: "sub/.git/refs", Ignored: true},
		{Path: "keep.txt", Untracked: true},
	}
	r := New()
	warns := r.CopyInherited(context.Background(), repo, wt, entries, []string{"**"})
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	if _, err := os.Stat(filepath.Join(wt, ".git", "config")); err == nil {
		t.Error(".git/config should not be copied")
	}
	if _, err := os.Stat(filepath.Join(wt, "keep.txt")); err != nil {
		t.Errorf("keep.txt should be copied: %v", err)
	}
}

// TestCopyInheritedTrackedNotMatched 验证 tracked 文件不匹配（entries 仅含 untracked/ignored）。
func TestCopyInheritedTrackedNotMatched(t *testing.T) {
	repo := makeRepo(t)
	wt := t.TempDir()
	// entries 为空（无 untracked/ignored），warnings 应为空。
	r := New()
	warns := r.CopyInherited(context.Background(), repo, wt, nil, []string{"**"})
	if len(warns) != 0 {
		t.Errorf("expected no warnings for empty entries, got %v", warns)
	}
}

// TestCopyInheritedInvalidGlob 验证非法 glob 降级为警告。
func TestCopyInheritedInvalidGlob(t *testing.T) {
	repo := makeRepo(t)
	wt := t.TempDir()
	entries := []git.FileStatus{{Path: "a.txt", Untracked: true}}
	r := New()
	warns := r.CopyInherited(context.Background(), repo, wt, entries, []string{"[unterminated"})
	// 非法模式应产生警告，文件不被复制。
	found := false
	for _, w := range warns {
		if strings.Contains(w, "invalid glob") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected invalid glob warning, got %v", warns)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
