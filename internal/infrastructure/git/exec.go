package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sync"
)

// 允许的 git 子命令白名单。任何调用 MUST 命中白名单的首参，禁止 shell 拼接。
var allowedSubcommands = map[string]struct{}{
	"status":           {},
	"diff":             {},
	"numstat":          {},
	"commit":           {},
	"add":              {},
	"push":             {},
	"check-ref-format": {},
	"rev-parse":        {},
	"symbolic-ref":     {},
	"ls-files":         {},
	"merge-base":       {},
	"worktree":          {},
	"branch":            {},
	"fetch":             {},
	"remote":            {},
	// codemirror-git-diff design D3：旧侧 blob 内容读取（ls-tree 探测 + git show <blobOID>）。
	"show":     {},
	"ls-tree":  {},
	"show-ref": {},
}

// execOutputLimit 是单次命令 stdout/stderr 的硬上限。
const execOutputLimit = 16 * 1024 * 1024

// ErrOutputTruncated 表示命令输出超过 execOutputLimit。
var ErrOutputTruncated = errors.New("git output truncated: exceeded limit")

// commandError 原样透传 git 的 stderr，不做自动补救。
type commandError struct {
	cmd    []string
	stdout string
	stderr string
	err    error
}

func (e *commandError) Error() string {
	if e.stderr != "" {
		return fmt.Sprintf("git %v: %s", e.cmd, e.stderr)
	}
	return fmt.Sprintf("git %v: %v", e.cmd, e.err)
}

func (e *commandError) Unwrap() error { return e.err }

// gitExitCode 从 git 命令错误中提取进程退出码。commandError 经 Unwrap 暴露 *exec.ExitError。
func gitExitCode(err error) (int, bool) {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), true
	}
	return 0, false
}

// StderrOf 从 git 命令错误中提取原始 stderr 文本（design.md §9/§21：git 错误原样透传 stderr）。
// 非 commandError（如参数校验错误、context 取消）返回 err.Error() 作为兜底，避免泄露空信息。
func StderrOf(err error) string {
	if err == nil {
		return ""
	}
	var ce *commandError
	if errors.As(err, &ce) {
		if ce.stderr != "" {
			return ce.stderr
		}
		// stderr 为空时 git 的诊断常在 stdout（如 "nothing to commit" 系列）——回退 stdout。
		if ce.stdout != "" {
			return ce.stdout
		}
		// stdout 也为空（如 exec 入口错误）回退到底层 err 文本。
		if ce.err != nil {
			return ce.err.Error()
		}
		return err.Error()
	}
	return err.Error()
}

// run 执行 git 命令（argv 数组调用），支持 context 取消、有界读取 stdout/stderr。
// dir 为 worktree 路径（可为空表示默认）。args 首参 MUST 命中白名单。
// 当 stdout 或 stderr 超过 execOutputLimit 时返回 ErrOutputTruncated（命令已结束）。
func run(ctx context.Context, dir string, args ...string) (stdout, stderr string, err error) {
	if len(args) == 0 {
		return "", "", errors.New("git: empty args")
	}
	if _, ok := allowedSubcommands[args[0]]; !ok {
		return "", "", fmt.Errorf("git: subcommand %q not in whitelist", args[0])
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}

	var outBuf, errBuf boundedBuffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()
	if runErr != nil {
		// 命令失败：优先返回 commandError，但若同时 overflow 则保留 overflow 事实于 error 链。
		ce := &commandError{
			cmd:    args,
			stdout: outBuf.String(),
			stderr: errBuf.String(),
			err:    runErr,
		}
		if outBuf.overflow || errBuf.overflow {
			return outBuf.String(), errBuf.String(), fmt.Errorf("%w (%v)", ErrOutputTruncated, ce)
		}
		return outBuf.String(), errBuf.String(), ce
	}
	if outBuf.overflow || errBuf.overflow {
		return outBuf.String(), errBuf.String(), ErrOutputTruncated
	}
	return outBuf.String(), errBuf.String(), nil
}

// boundedBuffer 是有界写入缓冲。始终满足 io.Writer 契约：返回 len(p), nil。
// 上限内写入保留，超出部分丢弃并置 overflow 标记，避免子进程因 pipe 阻塞而 hang。
type boundedBuffer struct {
	buf      bytes.Buffer
	overflow bool
	max      int
	written  int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.max == 0 {
		b.max = execOutputLimit
	}
	// 始终声明消费全部字节，满足 io.Writer 契约，避免 io.ErrShortWrite 重试。
	defer func() { b.written += len(p) }()
	if b.overflow {
		return len(p), nil
	}
	remaining := b.max - b.written
	if remaining <= 0 {
		b.overflow = true
		return len(p), nil
	}
	if len(p) > remaining {
		b.buf.Write(p[:remaining])
		b.overflow = true
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *boundedBuffer) String() string { return b.buf.String() }

// repoLocks 维护按 canonical repo path 的写锁，串行化仓库级写操作（worktree add/remove 等）。
// status/diff 只读不进锁。
var (
	repoLocksMu sync.Mutex
	repoLocks   = map[string]*sync.Mutex{}
)

// RepoLock 返回针对 repoPath 的写锁句柄，供仓库级写操作串行化。
// repoPath 经 EvalSymlinks 归一为 canonical path，避免同仓库不同路径字符串得到不同锁。
// 调用方在执行写操作期间持有该锁。
func RepoLock(repoPath string) *sync.Mutex {
	key := repoPath
	if resolved, err := filepath.EvalSymlinks(repoPath); err == nil && resolved != "" {
		key = resolved
	}
	repoLocksMu.Lock()
	defer repoLocksMu.Unlock()
	mu, ok := repoLocks[key]
	if !ok {
		mu = &sync.Mutex{}
		repoLocks[key] = mu
	}
	return mu
}

// repoLockAcquiredHook 供测试观察锁获取/释放，断言临界区不重叠（add-plain-dir-project D10）。
// 签名：hook(canonicalPath, acquired bool)。生产路径为 nil。
var repoLockAcquiredHook func(canonicalPath string, acquired bool)

// RepoLockAcquiredHookForTest 是 repoLockAcquiredHook 的导出别名，供跨包测试
// （internal/infrastructure/worktree）注入观察 hook。仅测试使用，生产路径保持 nil。
var RepoLockAcquiredHookForTest func(canonicalPath string, acquired bool)

// canonicalLockKey 归一 repoPath 为 canonical path（与 RepoLock 同 key）。
func canonicalLockKey(repoPath string) string {
	if resolved, err := filepath.EvalSymlinks(repoPath); err == nil && resolved != "" {
		return resolved
	}
	return repoPath
}

// AcquireRepoLock 获取 repoPath 的仓库级写锁，支持 context 取消（add-plain-dir-project D10
// refresh fetch 等待锁时可取消）。repoPath 经 EvalSymlinks 归一为 canonical path，与 RepoLock
// 共享同一锁池，故与 worktree.Add/Remove、GitPush(-u) 串行。
//
// 成功返回 unlock 函数（MUST 调用释放）；ctx 在等锁期间取消返回 ctx.Err()，未获取锁、不执行任何操作。
// 锁内操作仍按既有语义串行（sync.Mutex，不可取消执行中操作）。
func AcquireRepoLock(ctx context.Context, repoPath string) (unlock func(), err error) {
	mu := RepoLock(repoPath)
	// sync.Mutex 无 context 通道；用 done 信号 + goroutine 等待，ctx 先到则放弃。
	done := make(chan struct{})
	go func() {
		mu.Lock()
		close(done)
	}()
	select {
	case <-done:
		hook := repoLockAcquiredHook
		if hook == nil {
			hook = RepoLockAcquiredHookForTest
		}
		if hook != nil {
			canon := canonicalLockKey(repoPath)
			hook(canon, true)
			return func() {
				hook(canon, false)
				mu.Unlock()
			}, nil
		}
		return mu.Unlock, nil
	case <-ctx.Done():
		// ctx 先到：goroutine 仍会随后取锁（竞争极小），用额外 goroutine 在 done 后释放，
		// 避免锁泄漏。此处不等待 goroutine，它会在取锁后经下方释放路径释放。
		go func() {
			<-done
			mu.Unlock()
		}()
		return nil, ctx.Err()
	}
}
