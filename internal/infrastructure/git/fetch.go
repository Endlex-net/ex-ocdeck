package git

// fetch.go 实现远端分支刷新（add-plain-dir-project D10：branches refresh）。
//
// fetch 继承用户 git config/credential helper/SSH agent（本机 git CLI）；
// GIT_TERMINAL_PROMPT=0 禁止交互凭证（不可用即失败，避免 hang）；
// 30s 硬上限（父 ctx 更短 deadline 自然优先）；context 取消时按进程组终止，
// 避免遗留 ssh/credential-helper 子进程。
//
// 副作用边界（D10，按修订后设计文档）：逐 remote 显式 refspec fetch——
// `git fetch --no-tags --no-recurse-submodules --no-write-fetch-head --prune
//   --refmap='+refs/heads/*:refs/remotes/<remote>/*' <remote> '+refs/heads/*:refs/remotes/<remote>/*'`。
// --refmap 使命令行 refspec 完全取代 remote.*.fetch 配置（含恶意 +refs/heads/main:refs/heads/ocdeck/victim
// 与 mirror remote），保证仅写 refs/remotes/<remote>/*，不触碰 refs/heads/*；--no-tags 使 fetch.pruneTags 无从生效。
// 无 remote 的仓库 → 跳过 fetch，直接返回（调用方 ListBranches 仅本地枚举）。
//
// RefreshBranches 提供 fetch + ListBranches 的 singleflight 合并：同一 canonical repo
// 并发 refresh 合并为单次 fetch+枚举，等待者共享同一结果；不同 repo 并行。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// fetchTimeout 是 fetch 的硬上限（add-plain-dir-project D10：30s）。
const fetchTimeout = 30 * time.Second

// fetchTimeoutForTest 供测试缩短 fetch 超时上限，避免真实等待 30s（生产路径为 fetchTimeout）。
var fetchTimeoutForTest = fetchTimeout

// fetchRemoteHook 供测试注入确定性 fetch 行为（计数/阻塞/进程组终止）。
// 非 nil 时 FetchAll 对每个 remote 委托给它（跳过真实 git fetch）；生产路径为 nil。
var fetchRemoteHook func(ctx context.Context, repoPath, remote string) error

// ListRemotes 枚举 repoPath 配置的远端名称（`git remote`，每行一个）。
// 无 remote 返回空切片非错误（调用方据此跳过 fetch）。只读，不进仓库写锁。
func ListRemotes(ctx context.Context, repoPath string) ([]string, error) {
	out, _, err := run(ctx, repoPath, "remote")
	if err != nil {
		return nil, fmt.Errorf("git remote: %w", err)
	}
	var remotes []string
	for _, line := range strings.Split(out, "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		remotes = append(remotes, name)
	}
	return remotes, nil
}

// FetchAll 逐 remote 显式 refspec 刷新 repoPath 的远端跟踪分支（refs/remotes/*）。
//
// 副作用边界（D10）：--refmap 使命令行 refspec 取代 remote.*.fetch 配置，仅写
// refs/remotes/<remote>/*；--no-write-fetch-head 不覆盖用户 FETCH_HEAD；不移动本地分支与 worktree HEAD。
// 无 remote → 直接返回（无远端可拉，成功）。
//
// 失败语义：fetch 失败/超时/context 取消返回错误（含 git stderr，调用方映射 git_error），不返回伪成功。
// context 取消时按进程组终止子进程（不留 ssh/credential-helper 子进程）。
//
// 30s 硬上限：入口始终派生 context.WithTimeout(ctx, fetchTimeoutForTest)（父 ctx 更短 deadline 自然优先）。
//
// 调用方 MUST 已持有 repoPath 的仓库写锁（AcquireRepoLock），与 worktree.Add/Remove、GitPush 串行。
func FetchAll(ctx context.Context, repoPath string) error {
	// 30s 硬上限：始终派生（父 ctx 更短 deadline 自然优先；父无 deadline 时用 30s）。
	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeoutForTest)
	defer cancel()

	remotes, err := ListRemotes(fetchCtx, repoPath)
	if err != nil {
		return err
	}
	if len(remotes) == 0 {
		// 无远端可拉：视为成功（调用方 ListBranches 仅本地枚举）。
		return nil
	}
	for _, remote := range remotes {
		if fetchRemoteHook != nil {
			// 测试注入路径：跳过真实 git fetch，委托 hook（计数/阻塞/进程组终止确定性测试）。
			if err := fetchRemoteHook(fetchCtx, repoPath, remote); err != nil {
				return err
			}
			continue
		}
		if err := fetchRemote(fetchCtx, repoPath, remote); err != nil {
			return err
		}
	}
	return nil
}

// fetchRemote 对单个 remote 执行显式 refspec fetch，--refmap 取代 remote.*.fetch 配置。
func fetchRemote(ctx context.Context, repoPath, remote string) error {
	refspec := "+refs/heads/*:refs/remotes/" + remote + "/*"
	refmap := "+refs/heads/*:refs/remotes/" + remote + "/*"

	// GIT_TERMINAL_PROMPT=0：禁止交互凭证（不可用即失败，不 hang）。
	// 继承调用方环境（含用户 git config/credential helper/SSH agent）。
	cmd := exec.CommandContext(ctx, "git", "fetch",
		"--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", "--prune",
		"--refmap="+refmap, remote, refspec)
	cmd.Dir = repoPath
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")

	// exec.CommandContext 取消仅向主进程发 SIGKILL；fetch 可能 spawn ssh/credential-helper 子进程，
	// 需按进程组终止避免遗留。Setpgid 使子进程成为独立进程组，取消时 syscall.Kill(-pgid) 杀整组。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var out boundedBuffer
	var errBuf boundedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	cmd.Cancel = func() error {
		// 进程组终止：负 pgid 表示向整组发信号。
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return os.ErrProcessDone
	}

	if err := cmd.Run(); err != nil {
		stderr := errBuf.String()
		if stderr != "" {
			return fmt.Errorf("git fetch %s: %s", remote, stderr)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("git fetch %s: %w", remote, err)
		}
		return fmt.Errorf("git fetch %s: %w", remote, err)
	}
	return nil
}

// RefreshBranches 刷新 repoPath 远端分支并在锁内重新枚举，返回短名数组
// （add-plain-dir-project D10：POST /api/v1/projects/{id}/branches/refresh）。
//
// 单次执行：获取 canonical repo 写锁（AcquireRepoLock，与 worktree.Add/Remove、GitPush 串行）
// → FetchAll → ListBranches（同锁内枚举，保证 fetch 与枚举间无并发写）。
// 锁等待期间 ctx 取消 → 及时返回不执行 fetch。
//
// Singleflight 合并由 RefreshBranchesSingleflight 提供，本函数仅做单次 fetch+枚举。
func RefreshBranches(ctx context.Context, repoPath string) ([]string, error) {
	unlock, err := AcquireRepoLock(ctx, repoPath)
	if err != nil {
		return nil, err
	}
	defer unlock()

	if err := FetchAll(ctx, repoPath); err != nil {
		return nil, err
	}
	return ListBranches(ctx, repoPath)
}

// --- singleflight：同 canonical repo 并发 refresh 合并为单次 fetch+枚举 ---

// refreshCall 是一次合并的 refresh 执行（fetch + ListBranches）。
// done channel 供等待者响应自身 ctx 取消（select done|ctx.Done 即时返回）。
// 领跑者结果不被等待者取消破坏：等待者取消仅结束自身等待，MUST NOT 取消仍有有效领跑者的
// 共享 fetch（fetch 生命周期仅由领跑者 ctx 含 30s 硬上限治理，YAGNI 自愈）。
type refreshCall struct {
	done   chan struct{}
	result []string
	err    error
}

var (
	refreshMu    sync.Mutex
	refreshCalls = map[string]*refreshCall{}
)

// canonicalRepoPath 归一 repoPath 为 canonical path（与 RepoLock 同 key）。
func canonicalRepoPath(repoPath string) string {
	if resolved, err := filepath.EvalSymlinks(repoPath); err == nil && resolved != "" {
		return resolved
	}
	return repoPath
}

// RefreshBranchesSingleflight 刷新远端分支，同一 canonical repo 并发调用合并为单次 fetch+枚举，
// 等待者共享同一结果；不同 repo 并行。
//
// 等待者响应自身 ctx 取消：ctx 先到 → 返回 ctx 错误（select done|ctx.Done 即时返回，不阻塞到
// 领跑者结束）。等待者取消 MUST NOT 取消仍有有效领跑者的共享 fetch——底层 fetch 生命周期仅由
// 领跑者 ctx（含 FetchAll 30s 硬上限）治理；无等待者需要结果时也由 30s 自愈，不提前取消
//（YAGNI：简化语义，避免误取消有效领跑者）。
//
// 合并粒度为 canonical repo：调用方传入原始 repoPath，内部归一为 canonical path 作 singleflight key，
// 与 RepoLock 同 key 保证合并与锁串行一致。
func RefreshBranchesSingleflight(ctx context.Context, repoPath string) ([]string, error) {
	key := canonicalRepoPath(repoPath)

	refreshMu.Lock()
	if call, ok := refreshCalls[key]; ok {
		// 已有同 repo 在执行：登记为等待者，等共享结果或自身 ctx 取消。
		refreshMu.Unlock()

		select {
		case <-call.done:
			// 领跑者完成：共享结果。
			return call.result, call.err
		case <-ctx.Done():
			// 自身 ctx 取消：及时返回（仅结束自身等待，不取消领跑者 fetch）。
			return nil, ctx.Err()
		}
	}
	// 领跑者：执行 fetch+枚举，结果经 done channel 共享给等待者。
	call := &refreshCall{done: make(chan struct{})}
	refreshCalls[key] = call
	refreshMu.Unlock()

	// 执行完成后关闭 done 唤醒等待者、清理 singleflight 槽。
	defer func() {
		close(call.done)
		refreshMu.Lock()
		if refreshCalls[key] == call {
			delete(refreshCalls, key)
		}
		refreshMu.Unlock()
	}()

	call.result, call.err = RefreshBranches(ctx, repoPath)
	return call.result, call.err
}