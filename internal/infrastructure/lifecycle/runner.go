// Package lifecycle 实现项目生命周期配置的纯机制层（design.md §7.1）：
//   - RunScript：以 /bin/sh -c 非交互执行脚本，stdout/stderr 写入日志文件，
//     超时杀整个进程组；
//   - CopyInherited：将枚举出的 gitignored/untracked 文件按 glob 匹配从主仓库
//     复制进 worktree，保持相对路径与权限，no-clobber 原子发布。
//
// 本包不感知任务状态机，也不访问 DB；由 internal/task 编排层注入调用。
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"ocdeck/internal/infrastructure/git"
)

// LifecycleRunner 生命周期脚本与文件继承机制（design.md §7.1）。
// internal/infrastructure/lifecycle 实现，task 编排层注入 mock。
type LifecycleRunner interface {
	// RunScript 在 dir 下以 env 执行 script（/bin/sh -c），stdout+stderr 写入 logPath
	//（每次执行 truncate 重写；RunScript 是该日志文件的唯一写入者）。
	// 捕获输出上限 1MB，超限截断并追加 "[log truncated at 1MB]" 标记。
	// timeout 到期杀整个进程组（避免孙子进程泄漏）返回超时错误。exit 0 返回 nil。
	RunScript(ctx context.Context, dir string, env map[string]string, script, logPath string, timeout time.Duration) error

	// CopyInherited 将 entries（来自 internal/infrastructure/git 的文件级枚举）中匹配 patterns 的
	// 文件从 repoPath 复制进 wtPath，保持相对路径与权限；符号链接按链接复制；
	// 普通文件 MUST no-clobber 原子发布：同目录临时文件完整写入（fsync+chmod）后
	// link(2) 到目标（EEXIST → 目标已存在，跳过+警告），再 unlink 临时文件——
	// rename 会覆盖并发出现的目标，禁止直接 rename；destination 路径任一祖先
	// 为符号链接时 MUST 拒绝（防逃逸/防覆写他处）；
	// 路径 containment 校验（拒绝绝对路径/.. 逃逸）。
	// 匹配与复制机制失败一律降级为逐条警告返回，不返回阻断性 error。
	CopyInherited(ctx context.Context, repoPath, wtPath string, entries []git.FileStatus, patterns []string) (warnings []string)
}

// logCap 为单次脚本输出捕获上限（design.md §7.4：1MB）。
const logCap = 1 << 20

// logTruncMarker 超限截断时追加的标记。
const logTruncMarker = "[log truncated at 1MB]"

// runner 是 LifecycleRunner 的默认实现。
type runner struct{}

// New 构造默认 LifecycleRunner。
func New() LifecycleRunner { return runner{} }

// RunScript 实现 design.md §7.1。
func (runner) RunScript(ctx context.Context, dir string, env map[string]string, script, logPath string, timeout time.Duration) error {
	// 日志目录 0700、日志文件 0600（design.md §7.4：可能含敏感信息）。
	// 对已有目录/文件 MUST 显式 Chmod 收紧，rerun 前若过宽执行后仍需满足 MUST。
	dirPath := filepath.Dir(logPath)
	if err := os.MkdirAll(dirPath, 0o700); err != nil {
		return fmt.Errorf("lifecycle: create log dir: %w", err)
	}
	if err := os.Chmod(dirPath, 0o700); err != nil {
		return fmt.Errorf("lifecycle: chmod log dir: %w", err)
	}
	// 每次 truncate 重写；RunScript 是该文件的唯一写入者。
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("lifecycle: open log: %w", err)
	}
	if err := logFile.Chmod(0o600); err != nil {
		logFile.Close()
		return fmt.Errorf("lifecycle: chmod log: %w", err)
	}
	defer logFile.Close()

	// 超时 context：与传入 ctx 解耦，确保脚本按 timeout 终止（父 ctx 取消也终止）。
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Dir = dir
	cmd.Env = envSlice(env)
	// Setpgid 使脚本及其子进程成为独立进程组，超时/取消时可杀整个进程组（避免孙子进程泄漏）。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// 有界写入：超过 logCap 截断并追加标记。RunScript 是 logFile 唯一写入者，
	// 直接以 logFile 为底层 writer，截断标记在进程结束后写入。
	lw := &cappedWriter{w: logFile, max: logCap}
	cmd.Stdout = lw
	cmd.Stderr = lw

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("lifecycle: start script: %w", err)
	}

	// 主流程仲裁：waitCh 承载 cmd.Wait 的回收结果。命令完成优先于取消/超时报告——
	// 仅以 waitCh 已投递（可观察的完成结果）为准；否则立即 kill 进程组。
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	return arbitrate(waitCh, runCtx, func() { killProcessGroupFn(cmd.Process) },
		func(runErr error) error { return finishRun(runErr, lw, logFile) },
		timeout)
}

// arbitrate 仲裁命令完成与 ctx 取消的胜负。
// 完成优先：仅当 waitCh 已投递（可观察的完成结果）时命令结果胜出、不 kill；
// 否则（ctx 取消时 waitCh 仍空）立即 kill 进程组，等待 reap 后按 ctx.Err() 报告超时/取消。
//
// 残余风险（v1 明示接受）：cmd.Wait 已回收但 waiter goroutine 尚未完成 waitCh<-result
// 的纳秒级窗口内，非阻塞 default 检查可能判"未回收"而对已回收 PGID 发 SIGKILL。
// 无 OS 级同步可消除该窗口；SIGKILL 对已回收进程组返回 ESRCH（无效），PID 复用风险
// 为 v1 明示接受的残余风险——不得用延迟窗口掩盖，否则破坏 §7.1"超时到期 MUST 杀进程组"。
func arbitrate(waitCh <-chan error, runCtx context.Context, kill func(), finish func(error) error, timeout time.Duration) error {
	select {
	case runErr := <-waitCh:
		// 命令自然完成（exit 0 或非零）：直接返回其结果，绝不可能 kill。
		return finish(runErr)
	case <-runCtx.Done():
		// ctx 取消（超时或父取消）：完成优先——仅 waitCh 已投递（=已回收）才走完成路径，不 kill。
		select {
		case runErr := <-waitCh:
			return finish(runErr)
		default:
		}
		// 未回收：立即 kill 整个进程组（释放 pipe 让 Wait 返回），再等待 reap。
		kill()
		runErr := <-waitCh
		finish(runErr) // 仍写入截断标记（若触发上限）
		// 按 ctx.Err() 区分：DeadlineExceeded → "timed out"；否则 → "canceled"。
		if runCtx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("lifecycle: script timed out after %s", timeout)
		}
		return fmt.Errorf("lifecycle: script canceled: %w", runCtx.Err())
	}
}

// finishRun 写入截断标记（若触发上限）并返回命令结果的 error 映射。
// exit 0 → nil；非零 → "script exited"（日志已落盘，UI 据此查看）。
func finishRun(runErr error, lw *cappedWriter, logFile *os.File) error {
	if lw.truncated {
		_, _ = logFile.WriteString(logTruncMarker)
	}
	if runErr == nil {
		return nil
	}
	return fmt.Errorf("lifecycle: script exited: %w", runErr)
}

// killProcessGroupFn 向进程组发送 SIGKILL（design.md §7.1：超时杀整个进程组）。
// 测试钩子：包级变量便于测试注入验证仲裁语义；勿在并行测试中使用（非线程安全替换）。
var killProcessGroupFn = killProcessGroup

// killProcessGroup 向进程组发送 SIGKILL（design.md §7.1：超时杀整个进程组）。
func killProcessGroup(p *os.Process) {
	if p == nil {
		return
	}
	// Setpgid 下 pgid == pid；负 pid 表示向进程组发信号。
	_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
}

// envSlice 将 env map 转为 os/exec 所需的 "KEY=VALUE" 切片。
func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// cappedWriter 在 max 字节后丢弃写入并置 truncated（design.md §7.4：1MB 上限）。
type cappedWriter struct {
	w         io.Writer
	max       int
	written   int
	truncated bool
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.truncated {
		return len(p), nil
	}
	remaining := c.max - c.written
	if remaining <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		n, err := c.w.Write(p[:remaining])
		c.written += n
		c.truncated = true
		// 声明消费全部字节，避免子进程因短写阻塞。
		return len(p), err
	}
	n, err := c.w.Write(p)
	c.written += n
	return n, err
}

// --- CopyInherited ---

// CopyInherited 实现 design.md §7.1。
func (runner) CopyInherited(ctx context.Context, repoPath, wtPath string, entries []git.FileStatus, patterns []string) []string {
	var warnings []string
	if len(patterns) == 0 || len(entries) == 0 {
		return warnings
	}
	// 预编译 glob 模式（含语法校验）；非法模式降级为逐条警告。
	matches := make([]func(string) bool, 0, len(patterns))
	for _, p := range patterns {
		if !doublestar.ValidatePattern(p) {
			warnings = append(warnings, fmt.Sprintf("invalid glob pattern %q", p))
			continue
		}
		matches = append(matches, func(name string) bool {
			m, merr := doublestar.Match(p, name)
			return merr == nil && m
		})
	}
	if len(matches) == 0 {
		return warnings
	}

	for _, e := range entries {
		rel := e.Path
		if rel == "" {
			continue
		}
		// 排除 .git（design.md §7.2：.git 条目 MUST 排除）。
		if isGitPath(rel) {
			continue
		}
		// containment 校验：拒绝绝对路径与 .. 逃逸。
		if !isContainedRel(rel) {
			warnings = append(warnings, fmt.Sprintf("skip non-contained path %q", rel))
			continue
		}
		// glob 匹配（patterns 任一命中即复制）。
		if !anyMatch(matches, rel) {
			continue
		}
		if w := copyOne(repoPath, wtPath, rel); w != "" {
			warnings = append(warnings, w)
		}
	}
	return warnings
}

// anyMatch 返回是否任一 matcher 命中。
func anyMatch(matches []func(string) bool, name string) bool {
	for _, m := range matches {
		if m(name) {
			return true
		}
	}
	return false
}

// copyOne 复制单个相对路径条目；失败返回警告字符串（成功返回空）。
func copyOne(repoPath, wtPath, rel string) string {
	src := filepath.Join(repoPath, rel)
	dst := filepath.Join(wtPath, rel)

	// destination 任一祖先为符号链接时拒绝（防逃逸/防覆写他处）。
	if violated, err := ancestorSymlink(wtPath, rel); err != nil {
		return fmt.Sprintf("skip %q: verify ancestors: %v", rel, err)
	} else if violated {
		return fmt.Sprintf("skip %q: destination ancestor is symlink", rel)
	}

	info, err := os.Lstat(src)
	if err != nil {
		return fmt.Sprintf("skip %q: stat source: %v", rel, err)
	}
	mode := info.Mode()

	switch {
	case mode&os.ModeSymlink != 0:
		return copySymlink(src, dst, rel)
	case mode.IsRegular():
		return copyRegular(src, dst, rel, mode)
	default:
		// §7.2 保证 entries 为文件级条目（-uall 展开）；非常规文件/符号链接（如目录占位）跳过。
		return fmt.Sprintf("skip %q: non-regular entry %v", rel, mode)
	}
}

// copySymlink 按链接复制（含 broken symlink）。
func copySymlink(src, dst, rel string) string {
	target, err := os.Readlink(src)
	if err != nil {
		return fmt.Sprintf("skip %q: readlink: %v", rel, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Sprintf("skip %q: mkdir dest parent: %v", rel, err)
	}
	// 目标已存在则跳过+警告（no-clobber）。
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Sprintf("skip %q: destination exists", rel)
	} else if !os.IsNotExist(err) {
		return fmt.Sprintf("skip %q: stat dest: %v", rel, err)
	}
	if err := os.Symlink(target, dst); err != nil {
		return fmt.Sprintf("skip %q: symlink: %v", rel, err)
	}
	return ""
}

// copyRegular 普通文件 no-clobber 原子发布（design.md §7.1）。
// 目标是否已存在的唯一判定点是 link(2) 的 EEXIST——不前置 Lstat 预检（TOCTOU：预检后、link 前
// 目标可能被并发创建）。临时文件完整写入（fsync+chmod）后 link(2) 到目标：
//   - 成功 → 首次发布；
//   - EEXIST → 目标已存在（并发或重复），跳过+警告；
//   - 其他错误 → 跳过+警告。
func copyRegular(src, dst, rel string, mode os.FileMode) string {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Sprintf("skip %q: mkdir dest parent: %v", rel, err)
	}

	// 同目录临时文件：完整写入 → fsync+chmod → link(2) 到目标 → unlink 临时文件。
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".ocdeck-inherit-*")
	if err != nil {
		return fmt.Sprintf("skip %q: create temp: %v", rel, err)
	}
	tmpName := tmp.Name()
	// 失败时清理临时文件。
	defer os.Remove(tmpName)

	srcF, err := os.Open(src)
	if err != nil {
		tmp.Close()
		return fmt.Sprintf("skip %q: open source: %v", rel, err)
	}
	if _, err := io.Copy(tmp, srcF); err != nil {
		srcF.Close()
		tmp.Close()
		return fmt.Sprintf("skip %q: copy: %v", rel, err)
	}
	srcF.Close()
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Sprintf("skip %q: fsync: %v", rel, err)
	}
	if err := tmp.Chmod(mode.Perm()); err != nil {
		tmp.Close()
		return fmt.Sprintf("skip %q: chmod: %v", rel, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Sprintf("skip %q: close temp: %v", rel, err)
	}
	// link(2) 到目标：EEXIST → 目标已存在（并发出现），跳过+警告。
	if err := os.Link(tmpName, dst); err != nil {
		if errors.Is(err, os.ErrExist) || errors.Is(err, syscall.EEXIST) {
			return fmt.Sprintf("skip %q: destination exists (link EEXIST)", rel)
		}
		return fmt.Sprintf("skip %q: link: %v", rel, err)
	}
	return ""
}

// ancestorSymlink 检查 wtPath 下 rel 的任一祖先（含 wtPath 本身）是否为符号链接。
// 返回 (violated, error)：violated=true 表示存在符号链接祖先，拒绝复制。
func ancestorSymlink(wtPath, rel string) (bool, error) {
	// 从 wtPath 向下逐级检查：wtPath、wtPath/seg1、...、wtPath/seg(n-1)（不含最终目标）。
	cur := wtPath
	// 先验证 wtPath 本身不是符号链接。
	if info, err := os.Lstat(cur); err != nil {
		return false, err
	} else if info.Mode()&os.ModeSymlink != 0 {
		return true, nil
	}
	segs := strings.Split(filepath.ToSlash(rel), "/")
	for i := 0; i < len(segs)-1; i++ {
		cur = filepath.Join(cur, segs[i])
		if info, err := os.Lstat(cur); err != nil {
			if os.IsNotExist(err) {
				// 祖先尚不存在（后续 MkdirAll 会创建为目录），非符号链接。
				return false, nil
			}
			return false, err
		} else if info.Mode()&os.ModeSymlink != 0 {
			return true, nil
		}
	}
	return false, nil
}

// isContainedRel 校验相对路径不逃逸（拒绝绝对路径与 .. 逃逸）。
func isContainedRel(rel string) bool {
	if rel == "" || filepath.IsAbs(rel) {
		return false
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return false
	}
	// 拒绝任何 .. 段（如 a/../../b）。
	for _, seg := range strings.Split(filepath.ToSlash(clean), "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// isGitPath 判断相对路径是否位于 .git 下（与 internal/infrastructure/git 同语义，避免跨包依赖）。
func isGitPath(p string) bool {
	if p == ".git" {
		return true
	}
	rest := p
	for {
		if rest == ".git" {
			return true
		}
		i := strings.IndexByte(rest, '/')
		if i < 0 {
			return false
		}
		if rest[:i] == ".git" {
			return true
		}
		rest = rest[i+1:]
	}
}
