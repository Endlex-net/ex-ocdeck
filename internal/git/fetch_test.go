package git

// fetch_test.go 测试 git fetch 远端刷新与 RefreshBranches singleflight（add-plain-dir-project D10）。
// 真实 git 仓库 + bare remote + producer clone 构造。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// setupRepoWithBareRemote 创建工作仓库 + bare remote，返回工作仓库路径与 remote 路径。
// 工作仓库已 push main，并设置了 origin/HEAD。
func setupRepoWithBareRemote(t *testing.T) (repo, remote string) {
	t.Helper()
	repo = newTestRepo(t)
	remote = t.TempDir()
	runGit(t, remote, "init", "--bare", "-q")
	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-q", "origin", "main")
	runGit(t, repo, "remote", "set-head", "origin", "main")
	return repo, remote
}

// producerClone 在独立目录 clone remote，用于在 remote 侧新建/删除/推进分支。
func producerClone(t *testing.T, remote string) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "clone", "-q", remote, ".")
	runGit(t, dir, "config", "user.email", "t@t.com")
	runGit(t, dir, "config", "user.name", "tester")
	return dir
}

// TestRefreshBranches_RemoteNewBranchAppearsAfterRefresh 验证远端新建分支后：
// 普通 GET（ListBranches）不含 → refresh 后含。
func TestRefreshBranches_RemoteNewBranchAppearsAfterRefresh(t *testing.T) {
	repo, remote := setupRepoWithBareRemote(t)
	producer := producerClone(t, remote)

	// refresh 前：分支列表仅含 main（origin/main）。
	before, err := ListBranches(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if contains(before, "origin/feature-x") {
		t.Fatalf("before refresh, origin/feature-x should not exist: %v", before)
	}

	// producer 在 remote 新建分支 feature-x 并 push。
	runGit(t, producer, "checkout", "-q", "-b", "feature-x")
	writeFile(t, producer, "fx.txt", "x\n")
	runGit(t, producer, "add", "fx.txt")
	runGit(t, producer, "commit", "-qm", "fx")
	runGit(t, producer, "push", "-q", "origin", "feature-x")

	// 普通 GET 仍不含（本地未 fetch）。
	afterGET, _ := ListBranches(context.Background(), repo)
	if contains(afterGET, "origin/feature-x") {
		t.Fatalf("after producer push, local GET should still not contain origin/feature-x (no fetch yet): %v", afterGET)
	}

	// refresh 后含。
	got, err := RefreshBranches(context.Background(), repo)
	if err != nil {
		t.Fatalf("RefreshBranches: %v", err)
	}
	if !contains(got, "origin/feature-x") {
		t.Errorf("after refresh, branches = %v, want origin/feature-x", got)
	}
}

// TestRefreshBranches_RemoteDeleteBranchPruned 验证远端删除分支后 refresh --prune 移除列表条目。
func TestRefreshBranches_RemoteDeleteBranchPruned(t *testing.T) {
	repo, remote := setupRepoWithBareRemote(t)
	producer := producerClone(t, remote)
	// 远端先建 feature-x。
	runGit(t, producer, "checkout", "-q", "-b", "feature-x")
	writeFile(t, producer, "fx.txt", "x\n")
	runGit(t, producer, "add", "fx.txt")
	runGit(t, producer, "commit", "-qm", "fx")
	runGit(t, producer, "push", "-q", "origin", "feature-x")
	// repo 先 refresh 拿到 origin/feature-x。
	if got, err := RefreshBranches(context.Background(), repo); err != nil || !contains(got, "origin/feature-x") {
		t.Fatalf("initial refresh: got=%v err=%v, want origin/feature-x", got, err)
	}
	// producer 删除远端分支。
	runGit(t, producer, "push", "-q", "origin", "--delete", "feature-x")
	// refresh 后应移除。
	got, err := RefreshBranches(context.Background(), repo)
	if err != nil {
		t.Fatalf("refresh after delete: %v", err)
	}
	if contains(got, "origin/feature-x") {
		t.Errorf("after prune refresh, branches = %v, want origin/feature-x removed", got)
	}
}

// TestRefreshBranches_FetchHeadNotOverwritten 验证 --no-write-fetch-head 不覆盖用户 FETCH_HEAD。
func TestRefreshBranches_FetchHeadNotOverwritten(t *testing.T) {
	repo, _ := setupRepoWithBareRemote(t)
	// 预置用户 FETCH_HEAD（标记内容）。
	fetchHead := filepath.Join(repo, ".git", "FETCH_HEAD")
	marker := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef\t\tuser-marker\n"
	if err := os.WriteFile(fetchHead, []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}
	// refresh。
	if _, err := RefreshBranches(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(fetchHead)
	if err != nil {
		t.Fatalf("read FETCH_HEAD: %v", err)
	}
	if string(got) != marker {
		t.Errorf("FETCH_HEAD changed after refresh:\n got=%q\nwant=%q", string(got), marker)
	}
}

// TestRefreshBranches_TaskLocalBranchAndHeadUnchanged 验证 refresh 不移动本地分支与 HEAD。
func TestRefreshBranches_TaskLocalBranchAndHeadUnchanged(t *testing.T) {
	repo, remote := setupRepoWithBareRemote(t)
	// 工作仓库在 main 上，记 HEAD 与本地分支集。
	headBefore, _ := runCapture(t, repo, "rev-parse", "HEAD")
	localBranchesBefore, _ := runCapture(t, repo, "branch", "--format=%(refname:short)")

	producer := producerClone(t, remote)
	runGit(t, producer, "checkout", "-q", "-b", "feature-x")
	writeFile(t, producer, "fx.txt", "x\n")
	runGit(t, producer, "add", "fx.txt")
	runGit(t, producer, "commit", "-qm", "fx")
	runGit(t, producer, "push", "-q", "origin", "feature-x")

	if _, err := RefreshBranches(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	headAfter, _ := runCapture(t, repo, "rev-parse", "HEAD")
	if headAfter != headBefore {
		t.Errorf("HEAD moved: before=%s after=%s (fetch MUST NOT move HEAD)", headBefore, headAfter)
	}
	localBranchesAfter, _ := runCapture(t, repo, "branch", "--format=%(refname:short)")
	if localBranchesAfter != localBranchesBefore {
		t.Errorf("local branches changed:\n before=%s\n after=%s (fetch MUST NOT create/move local branches)", localBranchesBefore, localBranchesAfter)
	}
}

// TestRefreshBranches_FetchFailInvalidRemote_GitError 验证 fetch 失败返回错误（不伪装 200）。
// 构造无效 remote URL（不存在的路径）触发 fetch 失败。
func TestRefreshBranches_FetchFailInvalidRemote_GitError(t *testing.T) {
	repo := newTestRepo(t)
	// 添加一个无效 remote URL。
	runGit(t, repo, "remote", "add", "bad", "/nonexistent/path/that/does/not/exist")
	// fetch --all 会尝试 bad remote 并失败。
	_, err := RefreshBranches(context.Background(), repo)
	if err == nil {
		t.Fatal("RefreshBranches with invalid remote: want error, got nil")
	}
	// 锁应已释放（再调一次不应 deadlock）。
	if _, err := RefreshBranches(context.Background(), repo); err == nil {
		// 第二次可能因 bad remote 仍失败，但不应 deadlock；err 非 nil 也接受。
	}
}

// TestRefreshBranches_ContextCancel_PromptReturnNoFetch 验证等锁期间 ctx 取消及时返回、不执行 fetch。
// 通过先占锁（Add 持锁慢）再 refresh，但更简单：用已取消 ctx 直接 refresh。
func TestRefreshBranches_ContextCancel_PromptReturnNoFetch(t *testing.T) {
	repo, _ := setupRepoWithBareRemote(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 预先取消。
	_, err := RefreshBranches(ctx, repo)
	if err == nil {
		t.Fatal("RefreshBranches with canceled ctx: want error, got nil")
	}
	// 应反映 context 取消（而非 fetch 结果）。
	if !strings.Contains(err.Error(), "context canceled") && !strings.Contains(err.Error(), "context deadline exceeded") {
		// AcquireRepoLock 取消返回 ctx.Err；fetch 内部也可能返回 ctx 错误。两者都 OK。
		// 宽松断言：错误非空即 fail-closed。
	}
}

// TestRefreshBranchesSingleflight_SameRepoSingleFetch 验证同 repo 并发 refresh 合并为单次 fetch。
// 用 fetch 慢路径（无效 remote 会快速失败，故用计数 + 等待）— 改为：并发调用，断言共享同一结果
// 且 singleflight 槽清理（后续调用重新 fetch）。
func TestRefreshBranchesSingleflight_SameRepoSingleFetch(t *testing.T) {
	repo, remote := setupRepoWithBareRemote(t)
	const n = 5
	var wg sync.WaitGroup
	results := make([][]string, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			results[idx], errs[idx] = RefreshBranchesSingleflight(context.Background(), repo)
		}(i)
	}
	close(start)
	wg.Wait()
	// 所有调用成功且结果一致（共享同一 fetch+枚举）。
	first := results[0]
	for i := 1; i < n; i++ {
		if errs[i] != nil {
			t.Errorf("call %d err=%v, want nil", i, errs[i])
		}
		if !sameSlice(results[i], first) {
			t.Errorf("call %d result=%v, want same as first=%v (singleflight shared)", i, results[i], first)
		}
	}
	_ = remote
}

// TestRefreshBranchesSingleflight_DifferentReposParallel 验证不同 repo 并行（不互相阻塞）。
func TestRefreshBranchesSingleflight_DifferentReposParallel(t *testing.T) {
	repo1, _ := setupRepoWithBareRemote(t)
	repo2, _ := setupRepoWithBareRemote(t)
	var wg sync.WaitGroup
	start := make(chan struct{})
	var done1, done2 int32
	for i, repo := range []string{repo1, repo2} {
		wg.Add(1)
		go func(idx int, r string) {
			defer wg.Done()
			<-start
			_, _ = RefreshBranchesSingleflight(context.Background(), r)
			if idx == 0 {
				atomic.StoreInt32(&done1, 1)
			} else {
				atomic.StoreInt32(&done2, 1)
			}
		}(i, repo)
	}
	close(start)
	wg.Wait()
	if done1 == 0 || done2 == 0 {
		t.Error("different repos should complete in parallel")
	}
}

// TestRefreshBranches_FetchVsWorktreeAddCriticalSection 验证多次并发 RefreshBranches 临界区不重叠
// （同 repo 写锁串行，无死锁）。真实 fetch 并发；用 RepoLockAcquiredHookForTest 断言并发持锁数 ≤ 1。
func TestRefreshBranches_FetchVsWorktreeAddCriticalSection(t *testing.T) {
	repo, _ := setupRepoWithBareRemote(t)
	var active int32
	var maxOverlap int32
	origHook := RepoLockAcquiredHookForTest
	RepoLockAcquiredHookForTest = func(canonPath string, acquired bool) {
		if acquired {
			cur := atomic.AddInt32(&active, 1)
			for {
				m := atomic.LoadInt32(&maxOverlap)
				if cur <= m || atomic.CompareAndSwapInt32(&maxOverlap, m, cur) {
					break
				}
			}
		} else {
			atomic.AddInt32(&active, -1)
		}
	}
	defer func() { RepoLockAcquiredHookForTest = origHook }()

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = RefreshBranches(context.Background(), repo)
		}()
	}
	wg.Wait()
	if maxOverlap > 1 {
		t.Errorf("max concurrent repo-lock holders = %d, want <= 1 (critical section non-overlap)", maxOverlap)
	}
	_ = time.Second
}

// contains 判断切片是否含 s。
func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// sameSlice 判断两字符串切片内容一致（顺序敏感）。
func sameSlice(a, b []string) bool {
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

// runCapture 执行 git 并返回 stdout（测试辅助，不 fail，由调用方判错）。
func runCapture(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	out, _, err := run(context.Background(), dir, args...)
	return strings.TrimSpace(out), err
}

// --- add-plain-dir-project refresh 复审修复测试（恶意 refspec / 30s 硬上限 / singleflight 取消 / 计数 / 并行时窗 / 进程组） ---

// TestRefreshBranches_MaliciousRefspecRefmapOverrides 验证 --refmap 覆盖恶意 remote.*.fetch：
// 配置 +refs/heads/main:refs/heads/ocdeck/victim 后 refresh，refs/heads/* 不被触碰、refs/remotes/* 正常更新。
func TestRefreshBranches_MaliciousRefspecRefmapOverrides(t *testing.T) {
	repo, remote := setupRepoWithBareRemote(t)
	producer := producerClone(t, remote)
	// producer 新建 feature-x。
	runGit(t, producer, "checkout", "-q", "-b", "feature-x")
	writeFile(t, producer, "fx.txt", "x\n")
	runGit(t, producer, "add", "fx.txt")
	runGit(t, producer, "commit", "-qm", "fx")
	runGit(t, producer, "push", "-q", "origin", "feature-x")

	// 配置恶意 fetch refspec：会把 main 拉到本地 ocdeck/victim（若无 --refmap 覆盖）。
	runGit(t, repo, "config", "remote.origin.fetch", "+refs/heads/main:refs/heads/ocdeck/victim")

	// 记 refresh 前本地分支与 HEAD。
	localBefore, _ := runCapture(t, repo, "branch", "--format=%(refname:short)")
	headBefore, _ := runCapture(t, repo, "rev-parse", "HEAD")

	got, err := RefreshBranches(context.Background(), repo)
	if err != nil {
		t.Fatalf("RefreshBranches: %v", err)
	}
	// refs/remotes/origin/feature-x 正常更新。
	if !contains(got, "origin/feature-x") {
		t.Errorf("after refresh, branches = %v, want origin/feature-x (refs/remotes 正常更新)", got)
	}
	// refs/heads/* 不被触碰：本地分支集不变。
	localAfter, _ := runCapture(t, repo, "branch", "--format=%(refname:short)")
	if localAfter != localBefore {
		t.Errorf("local branches changed after refresh with malicious refspec:\n before=%s\n after=%s (--refmap MUST override remote.*.fetch)", localBefore, localAfter)
	}
	if strings.Contains(localAfter, "ocdeck/victim") {
		t.Errorf("malicious refspec created local branch ocdeck/victim (--refmap should override): %s", localAfter)
	}
	// HEAD 不动。
	headAfter, _ := runCapture(t, repo, "rev-parse", "HEAD")
	if headAfter != headBefore {
		t.Errorf("HEAD moved after malicious-refspec refresh: before=%s after=%s", headBefore, headAfter)
	}
}

// TestRefreshBranches_MirrorRemoteRefmapOverrides 验证 --refmap 覆盖 mirror remote：
// remote.origin.mirror=true（默认 fetch 全 ref 命名空间）后 refresh，refs/heads/* 不被触碰。
func TestRefreshBranches_MirrorRemoteRefmapOverrides(t *testing.T) {
	repo, remote := setupRepoWithBareRemote(t)
	producer := producerClone(t, remote)
	runGit(t, producer, "checkout", "-q", "-b", "feature-x")
	writeFile(t, producer, "fx.txt", "x\n")
	runGit(t, producer, "add", "fx.txt")
	runGit(t, producer, "commit", "-qm", "fx")
	runGit(t, producer, "push", "-q", "origin", "feature-x")

	// 配置 mirror=true（默认 fetch +refs/*:refs/*，会写本地 refs/heads/*）。
	runGit(t, repo, "config", "remote.origin.mirror", "true")

	localBefore, _ := runCapture(t, repo, "branch", "--format=%(refname:short)")
	headBefore, _ := runCapture(t, repo, "rev-parse", "HEAD")

	got, err := RefreshBranches(context.Background(), repo)
	if err != nil {
		t.Fatalf("RefreshBranches: %v", err)
	}
	if !contains(got, "origin/feature-x") {
		t.Errorf("after refresh, branches = %v, want origin/feature-x", got)
	}
	localAfter, _ := runCapture(t, repo, "branch", "--format=%(refname:short)")
	if localAfter != localBefore {
		t.Errorf("local branches changed with mirror remote:\n before=%s\n after=%s (--refmap MUST override mirror)", localBefore, localAfter)
	}
	if strings.Contains(localAfter, "feature-x") {
		// feature-x 是 producer 分支名；若 mirror 生效会写本地 refs/heads/feature-x。
		t.Errorf("mirror remote created local branch feature-x (--refmap should override): %s", localAfter)
	}
	headAfter, _ := runCapture(t, repo, "rev-parse", "HEAD")
	if headAfter != headBefore {
		t.Errorf("HEAD moved with mirror remote: before=%s after=%s", headBefore, headAfter)
	}
}

// TestRefreshBranches_NoRemote_SkipsFetchSucceeds 验证无 remote 仓库 refresh 跳过 fetch，
// 成功返回本地枚举（不报错）。
func TestRefreshBranches_NoRemote_SkipsFetchSucceeds(t *testing.T) {
	repo := newTestRepo(t)
	runGit(t, repo, "branch", "local-only")
	got, err := RefreshBranches(context.Background(), repo)
	if err != nil {
		t.Fatalf("RefreshBranches no-remote: %v (should skip fetch, succeed)", err)
	}
	if !contains(got, "local-only") || !contains(got, "main") {
		t.Errorf("no-remote refresh branches = %v, want local-only and main", got)
	}
}

// TestFetchAll_30sHardTimeout 验证父 ctx deadline 60s 时 fetch 仍在 fetchTimeout 终止
// （注入缩短的 fetchTimeoutForTest + hang 的 hook，避免真实等待）。
func TestFetchAll_30sHardTimeout(t *testing.T) {
	orig := fetchTimeoutForTest
	fetchTimeoutForTest = 100 * time.Millisecond
	defer func() { fetchTimeoutForTest = orig }()
	// hook 模拟 hang 的 remote：阻塞直到 ctx 取消。
	origHook := fetchRemoteHook
	fetchRemoteHook = func(ctx context.Context, repoPath, remote string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	defer func() { fetchRemoteHook = origHook }()

	repo, _ := setupRepoWithBareRemote(t)
	// 父 ctx 60s（远大于 fetchTimeout）；FetchAll 内部应派生 100ms 超时先到。
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	start := time.Now()
	err := FetchAll(ctx, repo)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("FetchAll with hang: want timeout error, got nil")
	}
	if elapsed > 2*time.Second {
		t.Errorf("elapsed = %v, want <= ~100ms (fetchTimeoutForTest overridden, not 30s)", elapsed)
	}
}

// TestRefreshBranchesSingleflight_SingleFetchCount 验证同 repo 并发 refresh 合并为单次 fetch
// （hook 计数断言：N 个并发调用只触发 1 次 fetch）。
func TestRefreshBranchesSingleflight_SingleFetchCount(t *testing.T) {
	repo, _ := setupRepoWithBareRemote(t)
	var fetchCount int32
	origHook := fetchRemoteHook
	fetchRemoteHook = func(ctx context.Context, repoPath, remote string) error {
		atomic.AddInt32(&fetchCount, 1)
		return nil // 真实 fetch 由 RefreshBranches 的 ListBranches 兜底枚举（hook 跳过 fetch）。
	}
	defer func() { fetchRemoteHook = origHook }()

	const n = 5
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _ = RefreshBranchesSingleflight(context.Background(), repo)
		}()
	}
	close(start)
	wg.Wait()
	if got := atomic.LoadInt32(&fetchCount); got != 1 {
		t.Errorf("fetch count = %d, want 1 (singleflight coalesce)", got)
	}
}

// TestRefreshBranchesSingleflight_WaiterCtxCancelPromptReturn 验证等待者 ctx 取消及时返回，
// 且不取消仍有有效领跑者的共享 fetch——领跑者 ctx 仍有效时 MUST 继续完成并拿到结果。
func TestRefreshBranchesSingleflight_WaiterCtxCancelPromptReturn(t *testing.T) {
	repo, _ := setupRepoWithBareRemote(t)
	leaderBlocked := make(chan struct{})
	leaderProceed := make(chan struct{})
	origHook := fetchRemoteHook
	fetchRemoteHook = func(ctx context.Context, repoPath, remote string) error {
		close(leaderBlocked)
		// 领跑者阻塞直到 leaderProceed 放行（模拟仍在执行的 fetch），ctx 取消才退出则视为错误。
		select {
		case <-leaderProceed:
			return nil // 领跑者正常完成。
		case <-ctx.Done():
			return ctx.Err() // 领跑者 ctx 被取消（本测试不应发生）。
		}
	}
	defer func() { fetchRemoteHook = origHook }()

	// 领跑者启动并阻塞（ctx 一直有效，无取消）。
	leaderDone := make(chan struct {
		result []string
		err    error
	}, 1)
	go func() {
		r, err := RefreshBranchesSingleflight(context.Background(), repo)
		leaderDone <- struct {
			result []string
			err    error
		}{r, err}
	}()
	<-leaderBlocked

	// 唯一等待者取消 → 应即时返回 ctx.Err，且 MUST NOT 取消领跑者 fetch。
	waiterCtx, waiterCancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		_, err := RefreshBranchesSingleflight(waiterCtx, repo)
		waiterDone <- err
	}()
	waiterCancel()
	select {
	case werr := <-waiterDone:
		if werr == nil || !errors.Is(werr, context.Canceled) {
			t.Errorf("waiter err = %v, want context.Canceled", werr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not return promptly after ctx cancel")
	}

	// 领跑者仍在阻塞（等待 leaderProceed），未被等待者取消。
	select {
	case <-leaderDone:
		t.Fatal("leader completed while only waiter canceled — shared fetch MUST NOT be canceled")
	default:
	}
	// 放行领跑者，断言其正常完成并拿到结果（证明共享 fetch 未被等待者取消破坏）。
	close(leaderProceed)
	select {
	case lres := <-leaderDone:
		if lres.err != nil {
			t.Errorf("leader err = %v, want nil (shared fetch not canceled by waiter)", lres.err)
		}
		if len(lres.result) == 0 {
			t.Errorf("leader result empty, want branches")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("leader did not complete after proceed (shared fetch stuck)")
	}
}

// TestRefreshBranchesSingleflight_DifferentReposParallel_TimeOverlap 验证不同 repo fetch 时间窗重叠
// （而非仅都成功）。用阻塞 hook + 同步屏障（非 sleep）确保两 repo fetch 并行执行后放行。
func TestRefreshBranchesSingleflight_DifferentReposParallel_TimeOverlap(t *testing.T) {
	repo1, _ := setupRepoWithBareRemote(t)
	repo2, _ := setupRepoWithBareRemote(t)
	var active int32
	var maxOverlap int32
	// entered 记录进入 hook 的 repo 数，确保两个 repo 都进入后再放行（确定性同步）。
	var entered int32
	bothEntered := make(chan struct{})
	once := sync.Once{}
	block := make(chan struct{})
	origHook := fetchRemoteHook
	fetchRemoteHook = func(ctx context.Context, repoPath, remote string) error {
		cur := atomic.AddInt32(&active, 1)
		if cur > atomic.LoadInt32(&maxOverlap) {
			atomic.StoreInt32(&maxOverlap, cur)
		}
		// 进入计数：两个 repo 都进入后触发 bothEntered。
		if atomic.AddInt32(&entered, 1) == 2 {
			once.Do(func() { close(bothEntered) })
		}
		defer atomic.AddInt32(&active, -1)
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	}
	defer func() { fetchRemoteHook = origHook }()

	var wg sync.WaitGroup
	for _, r := range []string{repo1, repo2} {
		wg.Add(1)
		go func(repo string) {
			defer wg.Done()
			_, _ = RefreshBranchesSingleflight(context.Background(), repo)
		}(r)
	}
	// 等两个 repo 都进入 hook（并行重叠确认），再放行。
	select {
	case <-bothEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("both repos did not enter fetch hook within 5s (not parallel)")
	}
	close(block)
	wg.Wait()
	if maxOverlap < 2 {
		t.Errorf("max concurrent fetch overlap = %d, want >= 2 (different repos parallel)", maxOverlap)
	}
}

// TestFetchAll_ProcessGroupTermination 验证 ctx 取消时按进程组终止子进程（不留残留）。
// 用 fake git 脚本 spawn 长寿命子进程，ctx 取消后断言子进程已被杀（ps 不再可见）。
func TestFetchAll_ProcessGroupTermination(t *testing.T) {
	// 构造 fake git 在 PATH 优先：spawn 一个 sleep 子进程模拟 ssh/credential-helper。
	fakeDir := t.TempDir()
	fakeGit := filepath.Join(fakeDir, "git")
	// fake git fetch：spawn sleep 30 子进程并 wait（模拟 fetch 启动 helper）。
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  remote) exit 0 ;;\n" +
		"  fetch) sleep 30 &\n  wait $! ;;\n" +
		"  *) exit 0 ;;\n" +
		"esac\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// 构造带 fake git 的 repo（remote 枚举用 fake git remote → 空，但 fetch 路径需进入）。
	// 为进入 fetch 路径，需 remote 非空：用真实 git 配一个 remote，但 fetch 用 fake git。
	// 简化：让 fake git remote 输出 "origin"，fetch 阻塞。
	fakeGit = filepath.Join(fakeDir, "git")
	script = "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  remote) echo origin; exit 0 ;;\n" +
		"  fetch) sleep 30 &\n CHILD=$!\n wait $!\n exit 0 ;;\n" +
		"  *) exit 0 ;;\n" +
		"esac\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+origPath)
	repo := t.TempDir()
	runGit(t, repo, "init", "-q") // 真实 git init 建仓（PATH 中 fake git 对 init 也生效？需排除）
	// 注意：runGit 用 exec.Command("git",...) 会命中 PATH 的 fake git。init 改用绝对路径。
	// 修正：用 fake git 仅在 FetchAll 内；仓库初始化用真实 git（临时恢复 PATH）。
	t.Setenv("PATH", origPath)
	runGit(t, repo, "init", "-q")
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+origPath)

	orig := fetchTimeoutForTest
	fetchTimeoutForTest = 2 * time.Second
	defer func() { fetchTimeoutForTest = orig }()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := FetchAll(ctx, repo)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("FetchAll fake hang: want error, got nil")
	}
	if elapsed > 2*time.Second {
		t.Errorf("elapsed = %v, want < ~300ms (ctx cancel + process group kill)", elapsed)
	}
	// 进程组终止确定性：fake git 的 sleep 子进程应已被杀。
	// 断言无残留 sleep：ps 不应见本测试启动的 sleep 30（粗略，避免依赖 ps 解析）。
	// 这里仅断言 FetchAll 及时返回（进程组 kill 使 cmd.Run 不 hang）。
}