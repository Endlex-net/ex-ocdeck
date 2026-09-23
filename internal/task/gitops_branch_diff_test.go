package task

// git-page-enhancements 任务 2.2-2.4/2.8：Manager.GitBranchDiffFiles / GitBranchDiffFile
// 与包级私有 helper resolveBranchDiffRefs 的行为测试。
//
// 覆盖（design.md D2/D3/D4 + tasks 2.3/2.4 错误矩阵）：
//   - 门禁链顺序（词法 → 锁 → GetTask → worktree 空 → assertGitRepoTask）：任务不存在/
//     worktree 空/project 不存在/dir 拒绝/非法组合 internal/任务忙 conflict/base 空 零 git 调用。
//   - helper 错误映射按子命令断言：base 非法 → invalid_input（ unborn/正常/损坏仓库三种 fixture
//     分别证明其后零 ResolveHeadOID/MergeBase 调用）；ErrUnbornHead/ErrNoMergeBase → invalid_state；
//     MergeBase 其他非零 exit（blob ref，/tmp 实测 exit 128）→ git_error 透传 stderr。
//   - files 行为：条目映射、BaseRef 原始透传、空结果、dirty worktree/index 不混入（真实仓库）、
//     超限 ErrTooManyFilesChanged → git_error（-short 跳过）。
//   - file 行为：修改/纯新增/纯删除/二进制/rename 新路径语义/归属拒绝零 blob 读/
//     损坏历史 numstat 失败透传与 OID 归因。
//   - local-path 验收（2.8）：repo+local-path 任务在项目目录当前 checkout 上可用；dir 拒绝
//     （门禁矩阵 dir 子用例）。既有 local-path 回归（status/diff/commit/push/编辑读写/
//     diff review/push -u 非 force/detached HEAD）由 local_path_mode_test.go 既有测试覆盖。
//
// 有效性证据：ResolveHeadOID 的 exit 128 类致命失败不可用真实仓库触达——git 对「HEAD
// symref 无法解析」（目录型 ref/自指循环，/tmp 实测）一律 exit 1（unborn 语义），exit 128
// 仅源于仓库整体损坏（如 .git/HEAD garbage），而损坏仓库下 git 视目录为非仓库，base 解析
// 必先失败（/tmp 实测：rev-parse --verify --quiet --end-of-options <完整OID> 在 .git/HEAD
// 为 garbage 时亦 exit 128 "fatal: not a git repository"，2026-09-22 探测），见
// TestGitBranchDiff_BrokenHead_BaseStepEarlyExit。该 fatal 分支现经 PATH-shim 故障注入在
// task 层直接覆盖：TestGitBranchDiff_HeadResolveFatal_GitErrorStderr（failOn 前缀
// rev-parse --verify --quiet 锁定 HEAD 端解析，注入 exit 128，断言 git_error + 注入
// stderr 透传 + 零 merge-base/numstat 后续调用，mutation 实证见该测试注释）；infra 层
// TestResolveHeadOIDBrokenRepo 另有直接覆盖（test 包外）。
//
// GitBranchDiffFile 侧读取失败分支：真实 fixture 下 numstat（归属校验步）对变更文件均读取
// 对应侧 blob 计行（/tmp 实测：损坏任一侧 blob 后 diff --numstat 先行 exit 128
// "unable to read <OID>"），同一批 blob 在 numstat 成功后真实读取不可能再失败——失败分支
// 经 PATH-shim 故障注入触达：「旧侧读取失败零新侧读」见
// TestGitBranchDiffFile_OldSideReadFailure_NoNewSideRead（新侧先读 mutation 实证见该测试
// 注释）；新侧失败分支（旧侧成功之后）无用例覆盖，属防御性分支，infra 层 readBlobSide
// 错误路径直接覆盖。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ocdeck/internal/infrastructure/git"
)

// initMainGitRepo 在 dir 初始化 main 分支 git 仓库并做初始提交（-b main 固定分支名，
// 与 initLocalPathGitRepo 同口径，供 base="main" 断言使用）。
func initMainGitRepo(t *testing.T, dir string) {
	t.Helper()
	runGitInit(t, dir, "init", "-q", "-b", "main")
	runGitInit(t, dir, "config", "user.email", "t@t.com")
	runGitInit(t, dir, "config", "user.name", "tester")
	readme := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readme, []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitInit(t, dir, "add", "README.md")
	runGitInit(t, dir, "commit", "-qm", "init")
}

// corruptLooseObject 把 dir 内 loose 对象 oid 截断为空文件（loose 对象为 0444 只读，先放开
// 写位），使读取该对象的 git 命令 exit 128（/tmp 实测 stderr 为 "error: object file ...
// is empty" + "fatal: unable to read ..."，错误消息携带对象 OID）。
func corruptLooseObject(t *testing.T, dir, oid string) {
	t.Helper()
	loose := filepath.Join(dir, ".git", "objects", oid[:2], oid[2:])
	if err := os.Chmod(loose, 0o644); err != nil {
		t.Fatalf("chmod loose object %s: %v", oid, err)
	}
	if err := os.Truncate(loose, 0); err != nil {
		t.Fatalf("truncate loose object %s: %v", oid, err)
	}
}

// --- 门禁链错误矩阵（两方法同链） ---

// TestGitBranchDiff_GateChain_ErrorMatrix 验证词法校验先于锁/任务查找/git 调用，
// 及锁后门禁链顺序（与 GitStatus/GitDiff 一致）。零 git 调用证明方式沿用 gitops_diff_test.go：
// WorktreePath 指向不存在路径时，git 被执行会返回 git_error 而非 invalid_input。
func TestGitBranchDiff_GateChain_ErrorMatrix(t *testing.T) {
	callBoth := func(t *testing.T, m *Manager, taskID, base, path string) []error {
		t.Helper()
		_, e1 := m.GitBranchDiffFiles(context.Background(), taskID, base)
		_, e2 := m.GitBranchDiffFile(context.Background(), taskID, base, path)
		return []error{e1, e2}
	}

	cases := []struct {
		name   string
		setup  func(t *testing.T, store *mockStore)
		taskID string
		base   string
		path   string
		want   string
	}{
		{"task not found", func(t *testing.T, store *mockStore) {}, "nope", "main", "a.txt", codeNotFound},
		{"empty base zero git calls", func(t *testing.T, store *mockStore) {
			seedSuspendedTask(store, "t1", "p1") // WorktreePath 不存在
		}, "t1", "", "a.txt", codeInvalidInput},
		{"empty worktree", func(t *testing.T, store *mockStore) {
			seedSuspendedTask(store, "t1", "p1")
			store.mutTask("t1", func(r *TaskRow) { r.WorktreePath = "" })
		}, "t1", "main", "a.txt", codeInvalidState},
		{"project gone", func(t *testing.T, store *mockStore) {
			dir := t.TempDir()
			initMainGitRepo(t, dir)
			seedRepoTask(t, store, "t1", "p1", dir)
			delete(store.projects, "p1")
		}, "t1", "main", "a.txt", codeNotFound},
		{"dir project rejected", func(t *testing.T, store *mockStore) {
			dir := t.TempDir()
			store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: dir, DefaultBranch: "main", Kind: ProjectKindDir})
			store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "task",
				Status: StatusSuspended, WorktreePath: dir, InitStatus: InitStatusNone, Mode: TaskModeLocalPath}
		}, "t1", "main", "a.txt", codeInvalidInput},
		{"illegal combo fail-closed", func(t *testing.T, store *mockStore) {
			dir := t.TempDir()
			store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: dir, DefaultBranch: "main", Kind: ProjectKindDir})
			store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "task",
				Status: StatusSuspended, WorktreePath: dir, InitStatus: InitStatusNone, Mode: TaskModeWorktree}
		}, "t1", "main", "a.txt", codeInternal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newMockStore()
			c.setup(t, store)
			m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
			for i, err := range callBoth(t, m, c.taskID, c.base, c.path) {
				if !isOpErrCode(err, c.want) {
					t.Fatalf("call[%d]: err = %v, want %s", i, err, c.want)
				}
			}
		})
	}

	// 任务忙：持锁后两方法均 conflict（先于 GetTask）。
	t.Run("task busy conflict", func(t *testing.T) {
		store := newMockStore()
		seedSuspendedTask(store, "t1", "p1")
		m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
		unlock, err := m.tryLockTask("t1")
		if err != nil {
			t.Fatalf("tryLockTask: %v", err)
		}
		defer unlock()
		if _, err := m.GitBranchDiffFiles(context.Background(), "t1", "main"); !isOpErrCode(err, codeConflict) {
			t.Fatalf("GitBranchDiffFiles busy: err = %v, want codeConflict", err)
		}
		if _, err := m.GitBranchDiffFile(context.Background(), "t1", "main", "a.txt"); !isOpErrCode(err, codeConflict) {
			t.Fatalf("GitBranchDiffFile busy: err = %v, want codeConflict", err)
		}
	})

	// file 端点专有词法校验：path 空/绝对路径/.. 逃逸/NUL → invalid_input，
	// 且先于任务查找（不存在任务 + 非法 path → invalid_input 而非 not_found）。
	t.Run("file path lexical before lookup", func(t *testing.T) {
		store := newMockStore()
		m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
		for _, path := range []string{"", "/etc/hosts", "../x.txt", "a\x00b"} {
			_, err := m.GitBranchDiffFile(context.Background(), "nope", "main", path)
			if !isOpErrCode(err, codeInvalidInput) {
				t.Fatalf("illegal path %q on nonexistent task: err = %v, want codeInvalidInput", path, err)
			}
		}
	})
}

// --- helper 错误映射（真实仓库 fixture，按子命令断言早退） ---

// TestGitBranchDiff_BaseInvalid_InvalidInputZeroFurtherGitCalls 验证 base 无法 rev-parse →
// invalid_input 且其后零 git 调用（/tmp 实测各 fixture 的变异后行为）：
//   - 正常仓库：若错误地继续，ResolveHeadOID 成功、MergeBase("",...) exit 128 → git_error；
//     断言 invalid_input 即证明早退。
//   - unborn 仓库（仅 init）：若错误地继续，ResolveHeadOID → ErrUnbornHead → invalid_state；
//     断言 invalid_input（而非 invalid_state）按子命令证明 ResolveHeadOID 未被调用。
//   - 损坏仓库（.git/HEAD garbage）：若错误地继续，ResolveHeadOID exit 128 → git_error；
//     断言 invalid_input 即证明早退。
func TestGitBranchDiff_BaseInvalid_InvalidInputZeroFurtherGitCalls(t *testing.T) {
	cases := []struct {
		name  string
		repo  func(t *testing.T) string
		byRef func(t *testing.T, dir string) string
	}{
		{"normal repo", func(t *testing.T) string {
			dir := t.TempDir()
			initMainGitRepo(t, dir)
			return dir
		}, func(t *testing.T, dir string) string { return "no-such-ref" }},
		{"unborn repo", func(t *testing.T) string {
			dir := t.TempDir()
			runGitInit(t, dir, "init", "-q", "-b", "main")
			runGitInit(t, dir, "config", "user.email", "t@t.com")
			runGitInit(t, dir, "config", "user.name", "tester")
			return dir
		}, func(t *testing.T, dir string) string { return "no-such-ref" }},
		{"broken repo", func(t *testing.T) string {
			dir := t.TempDir()
			initMainGitRepo(t, dir)
			if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("garbage\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return dir
		}, func(t *testing.T, dir string) string { return "main" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := c.repo(t)
			store := newMockStore()
			seedRepoTask(t, store, "t1", "p1", dir)
			m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
			// 子命令计数证据（tasks 2.2）：两方法各一次失败 rev-parse 后零后续 git 调用。
			reads := installGitCallRecorder(t)

			base := c.byRef(t, dir)
			if _, err := m.GitBranchDiffFiles(context.Background(), "t1", base); !isOpErrCode(err, codeInvalidInput) {
				t.Fatalf("files: base=%q err = %v, want codeInvalidInput (zero further git calls)", base, err)
			}
			if _, err := m.GitBranchDiffFile(context.Background(), "t1", base, "a.txt"); !isOpErrCode(err, codeInvalidInput) {
				t.Fatalf("file: base=%q err = %v, want codeInvalidInput (zero further git calls)", base, err)
			}

			// 副作用边界（精确 argv 序列与次数，tasks 2.2）：resolveBranchDiffRefs 第一步为
			// ResolveRefOID(base)，即 `rev-parse --verify --end-of-options <base>`；base 解析
			// 失败即早退 → 两方法（Files/File 各一次 helper）日志恰为 2 条 base 端 rev-parse。
			// 注意 HEAD 端解析（git.ResolveHeadOID）同为 rev-parse 前缀：
			// `rev-parse --verify --quiet --end-of-options HEAD`——仅按 rev-parse 前缀断言
			// 无法区分两端，故按完整 argv 精确断言；若实现先解析 HEAD 或 base 失败后继续，
			// 将出现 HEAD 端 rev-parse 或 merge-base/diff --numstat/ls-tree/show，此处红。
			wantCall := "rev-parse --verify --end-of-options " + base
			calls := reads()
			if len(calls) != 2 {
				t.Errorf("git call count = %d, calls = %v, want exactly 2（两方法各 1 次 base 解析即早退）", len(calls), calls)
			}
			for _, call := range calls {
				if call != wantCall {
					t.Errorf("git call = %q, want exactly %q（仅 base 端解析；HEAD 端带 --quiet 且末参为 HEAD）", call, wantCall)
				}
			}
		})
	}
}

// TestGitBranchDiff_BrokenHead_BaseStepEarlyExit 经真实 Manager 路径验证「有效完整 base OID
// + 损坏 HEAD」fixture：损坏 .git/HEAD 使 git 视目录为非仓库，base（完整 OID，无需读对象）
// 解析亦 exit 128 "fatal: not a git repository"（/tmp 实测）→ invalid_input。
// 早退证据：若实现未在 base 步早退，下一步 ResolveHeadOID 在损坏仓库下 exit 128（非
// ErrUnbornHead）→ 将映射 git_error 而非 invalid_state/invalid_input——观察到 invalid_input
// 即证明 git 调用恰为 ResolveRefOID 一次。HEAD 步 exit 128 分支本身由 infra 层
// TestResolveHeadOIDBrokenRepo 直接覆盖（见文件头有效性证据说明）。
func TestGitBranchDiff_BrokenHead_BaseStepEarlyExit(t *testing.T) {
	dir := t.TempDir()
	initMainGitRepo(t, dir)
	baseOID := gitOutput(t, dir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	_, err := m.GitBranchDiffFiles(context.Background(), "t1", baseOID)
	if !isOpErrCode(err, codeInvalidInput) {
		t.Fatalf("files err = %v, want codeInvalidInput（base 步早退，零后续 git 调用）", err)
	}
	// stderr 透传证据：错误携带 git 原始诊断（非仓库语义），而非笼统包装。
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("files err message = %q, want raw git stderr passthrough", err.Error())
	}
	if _, err := m.GitBranchDiffFile(context.Background(), "t1", baseOID, "README.md"); !isOpErrCode(err, codeInvalidInput) {
		t.Fatalf("file err = %v, want codeInvalidInput（base 步早退，零后续 git 调用）", err)
	}
}

// TestGitBranchDiff_UnbornHead_InvalidState 验证 ErrUnbornHead → invalid_state：
// main 已提交但 HEAD 为 orphan（--orphan 切换后未提交）→ HEAD 无提交而 base 可解析
//（/tmp 实测：`rev-parse --verify --quiet --end-of-options HEAD` exit 1）。
func TestGitBranchDiff_UnbornHead_InvalidState(t *testing.T) {
	dir := t.TempDir()
	initMainGitRepo(t, dir)
	runGitInit(t, dir, "checkout", "-q", "--orphan", "unbornb")

	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	if _, err := m.GitBranchDiffFiles(context.Background(), "t1", "main"); !isOpErrCode(err, codeInvalidState) {
		t.Fatalf("files: err = %v, want codeInvalidState (ErrUnbornHead)", err)
	}
	// 消息携带 typed sentinel 文本（invalid_state 判定来源唯一：ErrUnbornHead，非 stderr 文案）。
	if _, err := m.GitBranchDiffFiles(context.Background(), "t1", "main"); !strings.Contains(err.Error(), git.ErrUnbornHead.Error()) {
		t.Errorf("err message missing ErrUnbornHead sentinel text; got %q", err.Error())
	}
	if _, err := m.GitBranchDiffFile(context.Background(), "t1", "main", "README.md"); !isOpErrCode(err, codeInvalidState) {
		t.Fatalf("file: err = %v, want codeInvalidState (ErrUnbornHead)", err)
	}
}

// TestGitBranchDiff_NoMergeBase_InvalidState 验证 ErrNoMergeBase → invalid_state：
// main 与 orphan 分支两段独立历史（branch_diff_test.go 同款 fixture，git merge-base exit 1）。
func TestGitBranchDiff_NoMergeBase_InvalidState(t *testing.T) {
	dir := t.TempDir()
	initMainGitRepo(t, dir)
	runGitInit(t, dir, "switch", "-q", "--orphan", "other")
	if err := os.WriteFile(filepath.Join(dir, "other.txt"), []byte("o\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitInit(t, dir, "add", "other.txt")
	runGitInit(t, dir, "commit", "-qm", "other")

	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	if _, err := m.GitBranchDiffFiles(context.Background(), "t1", "main"); !isOpErrCode(err, codeInvalidState) {
		t.Fatalf("files: err = %v, want codeInvalidState (ErrNoMergeBase)", err)
	}
	if _, err := m.GitBranchDiffFile(context.Background(), "t1", "main", "other.txt"); !isOpErrCode(err, codeInvalidState) {
		t.Fatalf("file: err = %v, want codeInvalidState (ErrNoMergeBase)", err)
	}
}

// TestGitBranchDiff_MergeBaseFatal_GitErrorStderr 验证 MergeBase 其他非零 exit →
// git_error 透传 stderr：base 指向 blob（rev-parse 可解析、merge-base 报
// "Not a valid commit name" exit 128，/tmp 实测），MUST NOT 误判 invalid_state。
func TestGitBranchDiff_MergeBaseFatal_GitErrorStderr(t *testing.T) {
	dir := t.TempDir()
	initMainGitRepo(t, dir)
	blob := gitOutput(t, dir, "hash-object", "-w", filepath.Join(dir, "README.md"))
	runGitInit(t, dir, "update-ref", "refs/tags/blobref", blob)

	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	_, err := m.GitBranchDiffFiles(context.Background(), "t1", "refs/tags/blobref")
	if !isOpErrCode(err, codeGitError) {
		t.Fatalf("files: err = %v, want codeGitError (merge-base exit 128 passthrough)", err)
	}
	if !strings.Contains(err.Error(), "Not a valid commit name") {
		t.Errorf("err message = %q, want git stderr passthrough 'Not a valid commit name'", err.Error())
	}
}

// TestGitBranchDiff_HeadResolveFatal_GitErrorStderr 验证 ResolveHeadOID 致命失败（exit 128，
// 非 ErrUnbornHead）→ git_error 透传 stderr，其后 MUST NOT 执行 merge-base/numstat
//（tasks 2.2 声明的 HEAD fatal 分支）。真实 fixture 无法触达（HEAD symref 无法解析的形态
// git 一律 exit 1 unborn 语义，exit 128 仅源于仓库整体损坏而损坏仓库下 base 解析必先失败，
// 见文件头），故经 PATH-shim 故障注入：failOn 前缀 rev-parse --verify --quiet 恰好锁定
// HEAD 端解析（base 端为 rev-parse --verify --end-of-options，第三段不同，不误伤），
// 注入 exit 128，其余调用透传。
func TestGitBranchDiff_HeadResolveFatal_GitErrorStderr(t *testing.T) {
	dir := t.TempDir()
	initMainGitRepo(t, dir)

	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	reads := installGitCallRecorderFailing(t, "rev-parse", "--verify", "--quiet")

	_, filesErr := m.GitBranchDiffFiles(context.Background(), "t1", "main")
	if !isOpErrCode(filesErr, codeGitError) {
		t.Fatalf("files: err = %v, want codeGitError（HEAD 解析 exit 128 非 unborn）", filesErr)
	}
	_, fileErr := m.GitBranchDiffFile(context.Background(), "t1", "main", "README.md")
	if !isOpErrCode(fileErr, codeGitError) {
		t.Fatalf("file: err = %v, want codeGitError", fileErr)
	}
	// 注入 stderr 保留：shim 注入文本（"injected failure: <failOn argv>"）经
	// commandError.stderr → StderrOf 原样透传，证明非笼统包装。
	injected := "injected failure: rev-parse --verify --quiet"
	for name, e := range map[string]error{"files": filesErr, "file": fileErr} {
		if !strings.Contains(e.Error(), injected) {
			t.Errorf("%s: err message = %q, want injected stderr %q passthrough", name, e.Error(), injected)
		}
	}

	// 副作用边界（tasks 2.2）：base 端 rev-parse 恰 2 次（两方法各 1 次且成功）、HEAD 端
	// 注入失败恰 2 次、零 merge-base/零 diff（numstat，BranchDiffFiles 未被调用）——若
	// 实现跳过 HEAD fatal 出口继续执行，将出现 merge-base 且其错误透传丢失注入 stderr，
	// 下列断言红（mutation 实证）。
	var baseRev, headRev, mergeBases, diffs int
	for _, call := range reads() {
		switch {
		case call == "rev-parse --verify --end-of-options main":
			baseRev++
		case call == "rev-parse --verify --quiet --end-of-options HEAD":
			headRev++
		case strings.HasPrefix(call, "merge-base"):
			mergeBases++
		case strings.HasPrefix(call, "diff"):
			diffs++
		}
	}
	if baseRev != 2 || headRev != 2 {
		t.Errorf("rev-parse counts = (base %d, head %d), want (2, 2)（顺序 base→HEAD，两方法各一轮）", baseRev, headRev)
	}
	if mergeBases != 0 || diffs != 0 {
		t.Errorf("post-fatal calls: merge-base=%d diff=%d, want 0/0（HEAD fatal 后 MUST NOT 继续）", mergeBases, diffs)
	}
}

// --- files 端点行为 ---

// TestGitBranchDiffFiles_HappyPath 验证条目逐字段映射、byte-wise 排序、BaseRef 精确等于
// 用户输入原始 base（text ref 不规范化）、base==HEAD 空结果非错误。
func TestGitBranchDiffFiles_HappyPath(t *testing.T) {
	dir := t.TempDir()
	initMainGitRepo(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "mod.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitInit(t, dir, "add", "mod.txt")
	runGitInit(t, dir, "commit", "-qm", "add mod")
	runGitInit(t, dir, "checkout", "-qb", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feat.txt"), []byte("f1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mod.txt"), []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitInit(t, dir, "add", "feat.txt", "mod.txt")
	runGitInit(t, dir, "commit", "-qm", "feature changes")

	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	res, err := m.GitBranchDiffFiles(context.Background(), "t1", "main")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	// BaseRef MUST 写入用户输入原始值（不规范化为 OID 或全限定 ref）。
	if res.BaseRef != "main" {
		t.Errorf("BaseRef = %q, want exactly raw input %q", res.BaseRef, "main")
	}
	if len(res.Files) != 2 {
		t.Fatalf("files = %+v, want 2 entries", res.Files)
	}
	if res.Files[0].Path != "feat.txt" || res.Files[1].Path != "mod.txt" {
		t.Errorf("order = [%s %s], want byte-wise [feat.txt mod.txt]", res.Files[0].Path, res.Files[1].Path)
	}
	if res.Files[0].Additions != 1 || res.Files[0].Deletions != 0 || res.Files[0].IsBinary {
		t.Errorf("feat.txt entry = %+v, want +1 -0 text", res.Files[0])
	}
	if res.Files[1].Additions != 3 || res.Files[1].Deletions != 1 || res.Files[1].IsBinary {
		t.Errorf("mod.txt entry = %+v, want +3 -1 text", res.Files[1])
	}

	// base == HEAD → 空列表（200 语义，非错误）。
	empty, eerr := m.GitBranchDiffFiles(context.Background(), "t1", "feature")
	if eerr != nil {
		t.Fatalf("base==HEAD: err = %v, want nil", eerr)
	}
	if len(empty.Files) != 0 {
		t.Errorf("base==HEAD files = %+v, want empty", empty.Files)
	}
	if empty.BaseRef != "feature" {
		t.Errorf("base==HEAD BaseRef = %q, want feature", empty.BaseRef)
	}
}

// TestGitBranchDiffFiles_DirtyWorktreeNotIncluded 任务 2.3 验证：dirty worktree/index fixture
// 下分支列表不混入 staged/unstaged/untracked 变更（`git diff --numstat -z` 只读两棵 tree）。
func TestGitBranchDiffFiles_DirtyWorktreeNotIncluded(t *testing.T) {
	dir := t.TempDir()
	initMainGitRepo(t, dir)
	runGitInit(t, dir, "checkout", "-qb", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feat.txt"), []byte("feat\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitInit(t, dir, "add", "feat.txt")
	runGitInit(t, dir, "commit", "-qm", "feature changes")

	// dirty fixture：unstaged 修改 README.md + staged 新增 staged.txt + untracked notes.txt。
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitInit(t, dir, "add", "staged.txt")
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("n\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	res, err := m.GitBranchDiffFiles(context.Background(), "t1", "main")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(res.Files) != 1 || res.Files[0].Path != "feat.txt" {
		t.Fatalf("files = %+v, want exactly [feat.txt]（不混入 staged/unstaged/untracked）", res.Files)
	}
}

// TestGitBranchDiffFiles_TooManyFiles_GitError 验证超限 ErrTooManyFilesChanged →
// git_error 透传（既有 status 口径）。真实仓库提交 MaxStatusFiles+1 个文件，-short 跳过
//（与 infra 层 TestBranchDiffFilesTooMany 同口径；映射分支与 numstat 执行失败共用）。
func TestGitBranchDiffFiles_TooManyFiles_GitError(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping >MaxStatusFiles branch diff test in -short mode")
	}
	dir := t.TempDir()
	initMainGitRepo(t, dir)
	runGitInit(t, dir, "checkout", "-qb", "feature")
	for i := 0; i < git.MaxStatusFiles+1; i++ {
		p := filepath.Join(dir, fmt.Sprintf("f%05d.txt", i))
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGitInit(t, dir, "add", "-A")
	runGitInit(t, dir, "commit", "-qm", "many files")

	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	_, err := m.GitBranchDiffFiles(context.Background(), "t1", "main")
	if !isOpErrCode(err, codeGitError) {
		t.Fatalf("err = %v, want codeGitError (ErrTooManyFilesChanged passthrough)", err)
	}
	if !strings.Contains(err.Error(), "too many changed files") {
		t.Errorf("err message = %q, want passthrough 'too many changed files'", err.Error())
	}
}

// TestGitBranchDiffFiles_NumstatFailure_GitError 验证 BranchDiffFiles numstat 命令执行失败
//（非超限）→ git_error 透传：损坏 HEAD commit 的 root tree loose 对象——helper 三步只读
// commit 对象（rev-parse/rev-parse HEAD/merge-base /tmp 实测均 exit 0），numstat 读 tree
// 必失败（exit 128 "unable to read tree"）。stderr 透传证据：错误消息携带 tree OID；
// 非 invalid_input/invalid_state 证明 helper 已通过；非 "too many changed files" 证明
// 非超限路径。GitBranchDiffFile 归属校验同经 BranchDiffFiles，同断言 git_error。
func TestGitBranchDiffFiles_NumstatFailure_GitError(t *testing.T) {
	dir := t.TempDir()
	initMainGitRepo(t, dir)
	runGitInit(t, dir, "checkout", "-qb", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feat.txt"), []byte("feat\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitInit(t, dir, "add", "feat.txt")
	runGitInit(t, dir, "commit", "-qm", "feature changes")
	treeOID := gitOutput(t, dir, "rev-parse", "HEAD^{tree}")
	corruptLooseObject(t, dir, treeOID)

	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	_, err := m.GitBranchDiffFiles(context.Background(), "t1", "main")
	if !isOpErrCode(err, codeGitError) {
		t.Fatalf("files err = %v, want codeGitError（numstat 失败透传）", err)
	}
	if !strings.Contains(err.Error(), treeOID) {
		t.Errorf("files err message = %q, want contains corrupted tree OID %s（stderr 透传）", err.Error(), treeOID)
	}
	if strings.Contains(err.Error(), "too many changed files") {
		t.Errorf("files err message = %q, MUST NOT be the too-many-files path", err.Error())
	}
	if _, err := m.GitBranchDiffFile(context.Background(), "t1", "main", "feat.txt"); !isOpErrCode(err, codeGitError) {
		t.Fatalf("file err = %v, want codeGitError（归属校验同经 numstat）", err)
	}
}

// --- file 端点行为 ---

// TestGitBranchDiffFile_SideExistence 验证修改/纯新增/纯删除/二进制四类存在性真值表
//（两侧 ReadRefSideContent + normalizeDiffSideContent + 既有组装规则）。
func TestGitBranchDiffFile_SideExistence(t *testing.T) {
	setup := func(t *testing.T) (*Manager, string) {
		t.Helper()
		dir := t.TempDir()
		initMainGitRepo(t, dir)
		// main：mod.txt/rm.txt/bin.txt 基线（bin.txt 为文本版）。
		if err := os.WriteFile(filepath.Join(dir, "mod.txt"), []byte("v1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "rm.txt"), []byte("bye\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		bin := append([]byte("x"), 0)
		bin = append(bin, 'y')
		if err := os.WriteFile(filepath.Join(dir, "bin.txt"), bin, 0o644); err != nil {
			t.Fatal(err)
		}
		runGitInit(t, dir, "add", "-A")
		runGitInit(t, dir, "commit", "-qm", "base files")
		runGitInit(t, dir, "checkout", "-qb", "feature")
		// feature：mod.txt 修改、mk.txt 新增、rm.txt 删除、bin.txt 文本→二进制。
		if err := os.WriteFile(filepath.Join(dir, "mod.txt"), []byte("v2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "mk.txt"), []byte("new\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(dir, "rm.txt")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "bin.txt"), []byte("BIN\x00ARY"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGitInit(t, dir, "add", "-A")
		runGitInit(t, dir, "commit", "-qm", "feature changes")

		store := newMockStore()
		seedRepoTask(t, store, "t1", "p1", dir)
		m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
		return m, dir
	}

	t.Run("modified", func(t *testing.T) {
		m, _ := setup(t)
		d, err := m.GitBranchDiffFile(context.Background(), "t1", "main", "mod.txt")
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		assertOld(d, "v1\n", true, t)
		assertNew(d, "v2\n", true, t)
		if d.OldMode != "100644" || d.NewMode != "100644" {
			t.Errorf("modes = (%q, %q), want 100644/100644", d.OldMode, d.NewMode)
		}
		if d.IsBinary || d.Truncated {
			t.Errorf("isBinary=%v truncated=%v, want both false", d.IsBinary, d.Truncated)
		}
	})
	t.Run("pure addition", func(t *testing.T) {
		m, _ := setup(t)
		d, err := m.GitBranchDiffFile(context.Background(), "t1", "main", "mk.txt")
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		assertOld(d, "", false, t)
		assertNew(d, "new\n", true, t)
	})
	t.Run("pure deletion", func(t *testing.T) {
		m, _ := setup(t)
		d, err := m.GitBranchDiffFile(context.Background(), "t1", "main", "rm.txt")
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		assertOld(d, "bye\n", true, t)
		assertNew(d, "", false, t)
	})
	t.Run("binary clears both", func(t *testing.T) {
		m, _ := setup(t)
		d, err := m.GitBranchDiffFile(context.Background(), "t1", "main", "bin.txt")
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !d.IsBinary {
			t.Fatalf("isBinary = false, want true")
		}
		if d.OldContent != "" || d.NewContent != "" {
			t.Errorf("contents not cleared: %q / %q", d.OldContent, d.NewContent)
		}
		if !d.OldExists || !d.NewExists {
			t.Errorf("exists = (old=%v,new=%v), want both true", d.OldExists, d.NewExists)
		}
	})
}

// TestGitBranchDiffFile_RenameNewPathSemantics 验证 rename 语义（D4 用户裁决）：
// 纯 rename（git mv）在列表中仅按新路径登记（parseNumstatZ rename 记 newPath）；
// file(newPath) 两侧均按新路径读取——旧侧（merge-base）该新路径不存在 → 纯新增展示；
// file(oldPath) → 旧路径不在变更集合 → invalid_input；MUST NOT 传递 oldPath。
//
// 有效性证据：归属拒绝子用例在「跳过归属校验」的实现下会读到旧侧 blob 返回 DTO 而失败；
// 新路径子用例在「按 oldPath 读旧侧」的实现下 OldExists=true 而失败。
func TestGitBranchDiffFile_RenameNewPathSemantics(t *testing.T) {
	dir := t.TempDir()
	initMainGitRepo(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("l1\nl2\nl3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitInit(t, dir, "add", "a.txt")
	runGitInit(t, dir, "commit", "-qm", "add a")
	runGitInit(t, dir, "checkout", "-qb", "feature")
	runGitInit(t, dir, "mv", "a.txt", "b.txt")
	runGitInit(t, dir, "commit", "-qm", "rename a to b")

	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	list, err := m.GitBranchDiffFiles(context.Background(), "t1", "main")
	if err != nil {
		t.Fatalf("list err = %v", err)
	}
	if len(list.Files) != 1 || list.Files[0].Path != "b.txt" {
		t.Fatalf("list = %+v, want exactly [b.txt]（rename 按新路径登记）", list.Files)
	}

	// 新路径两侧均按新路径读：旧侧 b.txt 不存在 → 纯新增展示。
	d, err := m.GitBranchDiffFile(context.Background(), "t1", "main", "b.txt")
	if err != nil {
		t.Fatalf("file(b.txt) err = %v", err)
	}
	assertOld(d, "", false, t)
	assertNew(d, "l1\nl2\nl3\n", true, t)

	// 旧路径不在变更集合 → invalid_input。
	if _, err := m.GitBranchDiffFile(context.Background(), "t1", "main", "a.txt"); !isOpErrCode(err, codeInvalidInput) {
		t.Fatalf("file(a.txt): err = %v, want codeInvalidInput（旧路径不在变更集合）", err)
	}
}

// TestGitBranchDiffFile_OwnershipRejectZeroBlobRead 验证归属校验：path 在两侧 tree 中均存在
// 且可读（未变更的 tracked 文件）但不在变更集合 → invalid_input 且零 blob 读。
// 有效性证据：若实现跳过归属校验，ReadRefSideContent 两侧均成功并返回 DTO，本测试失败。
func TestGitBranchDiffFile_OwnershipRejectZeroBlobRead(t *testing.T) {
	dir := t.TempDir()
	initMainGitRepo(t, dir)
	runGitInit(t, dir, "checkout", "-qb", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feat.txt"), []byte("feat\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitInit(t, dir, "add", "feat.txt")
	runGitInit(t, dir, "commit", "-qm", "feature changes")

	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	// 子命令计数证据（tasks 2.4）：归属拒绝后零 blob 读（ls-tree/show 不得出现）。
	reads := installGitCallRecorder(t)

	// README.md 在 main 与 feature 均未变更（blob 两侧可读）但不在变更集合。
	if _, err := m.GitBranchDiffFile(context.Background(), "t1", "main", "README.md"); !isOpErrCode(err, codeInvalidInput) {
		t.Fatalf("unchanged tracked file: err = %v, want codeInvalidInput（零 blob 读）", err)
	}
	// 从未存在的路径同样 invalid_input。
	if _, err := m.GitBranchDiffFile(context.Background(), "t1", "main", "ghost.txt"); !isOpErrCode(err, codeInvalidInput) {
		t.Fatalf("nonexistent path: err = %v, want codeInvalidInput", err)
	}

	// 副作用边界：仅 ref 解析 + numstat（归属集合计算），无 ls-tree/show——
	// 若实现跳过归属校验直接读取，此断言红。
	for _, call := range reads() {
		if strings.HasPrefix(call, "ls-tree") || strings.HasPrefix(call, "show") {
			t.Errorf("git blob read after ownership reject: %q, want zero blob reads", call)
		}
	}
}

// TestGitBranchDiffFile_CorruptOldBlob_GitErrorOidAttribution 验证损坏历史下
// GitBranchDiffFile 的 git_error 透传与错误归因：旧侧 blob loose 对象删除后，
// numstat（归属校验步，需读取全部变更 blob 计行）先行失败 exit 128 "unable to read
// <oldBlob>"，stderr 恰含旧侧 OID。
//
// 范围说明：blob 损坏时执行流在归属校验步（numstat）即失败，到不了侧读取；本测试验证的
// 是失败归因正确：错误消息恰含旧侧 OID、MUST NOT 含新侧 OID。「旧侧读取失败零新侧读」
// 的顺序契约不在本测试范围——numstat 成功前提无法用真实 fixture 触达，经 PATH-shim
// 故障注入触达，见 TestGitBranchDiffFile_OldSideReadFailure_NoNewSideRead。
func TestGitBranchDiffFile_CorruptOldBlob_GitErrorOidAttribution(t *testing.T) {
	dir := t.TempDir()
	initMainGitRepo(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitInit(t, dir, "add", "a.txt")
	runGitInit(t, dir, "commit", "-qm", "v1")
	oldBlob := gitOutput(t, dir, "rev-parse", "HEAD:a.txt")
	runGitInit(t, dir, "checkout", "-qb", "feature")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitInit(t, dir, "add", "a.txt")
	runGitInit(t, dir, "commit", "-qm", "v2")
	newBlob := gitOutput(t, dir, "rev-parse", "HEAD:a.txt")
	// 旧侧 loose 对象删除 + 新侧 blob 截断损坏（双标记，见函数注释）。
	loose := filepath.Join(dir, ".git", "objects", oldBlob[:2], oldBlob[2:])
	if err := os.Remove(loose); err != nil {
		t.Fatalf("remove loose object: %v", err)
	}
	corruptLooseObject(t, dir, newBlob)

	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	// 子命令计数证据（tasks 2.4）：「零新侧读」的可观测形式——失败止于 numstat，
	// 零 ls-tree/show（见函数注释：侧读分支在 numstat 成功后不可达，此处正向证明）。
	reads := installGitCallRecorder(t)

	_, err := m.GitBranchDiffFile(context.Background(), "t1", "main", "a.txt")
	if !isOpErrCode(err, codeGitError) {
		t.Fatalf("err = %v, want codeGitError（损坏历史透传）", err)
	}
	if !strings.Contains(err.Error(), oldBlob) {
		t.Errorf("err message = %q, want contains corrupt blob OID %s（归因旧侧）", err.Error(), oldBlob)
	}
	if strings.Contains(err.Error(), newBlob) {
		t.Errorf("err message = %q, MUST NOT contain new side blob OID %s", err.Error(), newBlob)
	}

	// 副作用边界：3 次 ref 解析 + 1 次失败的 numstat，无任何侧读取（ls-tree/show）——
	// 若实现绕过归属校验的 numstat 失败继续读侧，此断言红。
	for _, call := range reads() {
		if strings.HasPrefix(call, "ls-tree") || strings.HasPrefix(call, "show") {
			t.Errorf("git blob read after numstat failure: %q, want zero side reads", call)
		}
	}
}

// TestGitBranchDiffFile_OldSideReadFailure_NoNewSideRead 验证「旧侧先读：失败 git_error
// 透传且 MUST NOT 读新侧」（D2 错误出口，tasks 2.4）。真实 fixture 无法触达该分支
// （numstat 成功则同一批 blob 真实读取不可能失败，见文件头），故经 PATH-shim 故障注入：
// `git show <旧侧 blobOID>` 记录后 exit 128（其余调用透传），numstat 因此成功、归属校验
// 通过、旧侧 show 注入失败。断言四点：git_error、错误消息归因旧侧 OID、日志中新侧零
// ls-tree 探测且零 show（旧侧 show 恰 1 次）——ReadRefSideContent = ls-tree 存在性探测 +
// show blob 读取（content.go:99/:108），「零新侧读」需两者皆零，仅断 show 会漏掉新侧
// ls-tree 探测（content.go:109，目标为新侧 headOID）。
// 有效性证据（mutation 实证）：对调侧读取顺序（新侧先读）后，新侧 ls-tree/show 先成功
// 出现在日志、旧侧才注入失败——错误码断言仍过，但零新侧 ls-tree/show 断言红。
func TestGitBranchDiffFile_OldSideReadFailure_NoNewSideRead(t *testing.T) {
	dir := t.TempDir()
	initMainGitRepo(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitInit(t, dir, "add", "a.txt")
	runGitInit(t, dir, "commit", "-qm", "v1")
	oldBlob := gitOutput(t, dir, "rev-parse", "HEAD:a.txt")
	runGitInit(t, dir, "checkout", "-qb", "feature")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitInit(t, dir, "add", "a.txt")
	runGitInit(t, dir, "commit", "-qm", "v2")
	newBlob := gitOutput(t, dir, "rev-parse", "HEAD:a.txt")
	headOID := gitOutput(t, dir, "rev-parse", "HEAD") // 新侧 commit OID（新侧 ls-tree 探测目标）

	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	// 子命令计数 + 故障注入（tasks 2.4）：仅旧侧 show 失败，rev-parse/merge-base/numstat/
	// ls-tree/新侧 show 均透传真实 git。
	reads := installGitCallRecorderFailing(t, "show", oldBlob)

	_, err := m.GitBranchDiffFile(context.Background(), "t1", "main", "a.txt")
	if !isOpErrCode(err, codeGitError) {
		t.Fatalf("err = %v, want codeGitError（旧侧 show 注入失败透传）", err)
	}
	if !strings.Contains(err.Error(), oldBlob) {
		t.Errorf("err message = %q, want contains old side blob OID %s（归因旧侧）", err.Error(), oldBlob)
	}

	// 副作用边界：归属校验（numstat）成功后旧侧 show 恰 1 次且失败即止，新侧零读取——
	// ReadRefSideContent = ls-tree 探测 + show 读取，零新侧读需两者皆零：ls-tree 按新侧
	// headOID 匹配（merge-base/numstat 等 argv 同样含 headOID，故限定 ls-tree 前缀），
	// show 按新侧 blobOID 精确匹配。若实现对调顺序（新侧先读），新侧 ls-tree/show 先
	// 成功出现在日志，两处断言红。
	var oldShows, newShows, newLsTrees int
	for _, call := range reads() {
		switch {
		case call == "show "+oldBlob:
			oldShows++
		case call == "show "+newBlob:
			newShows++
		case strings.HasPrefix(call, "ls-tree") && strings.Contains(call, headOID):
			newLsTrees++
		}
	}
	if oldShows != 1 {
		t.Errorf("old side show count = %d, want exactly 1（旧侧先读且失败即止）", oldShows)
	}
	if newShows != 0 {
		t.Errorf("new side show count = %d, want 0（旧侧失败 MUST NOT 读新侧，D2）", newShows)
	}
	if newLsTrees != 0 {
		t.Errorf("new side ls-tree count = %d, want 0（新侧 ls-tree 探测同属读新侧，D2）", newLsTrees)
	}
}

// TestGitBranchDiff_DivergedBase_MergeBaseSemantics 验证 three-dot（merge-base）语义
//（design.md D3/D4：BranchDiffFiles 与单文件旧侧均以 mbOID 为基）：base 分支在分叉后
// 追加独有提交时，列表与归属校验 MUST 只针对 mbOID..HEAD。
//
// fixture：mb 上 a.txt/b.txt 均为 "v0"；feature（HEAD）将两者改为 "v1"；main（base）
// 分叉后新增 base-only.txt。
//   - three-dot 正确结果：list 恰为 [a.txt, b.txt]（base-only.txt 不出现）；
//     file(b.txt) 旧侧 "v0"/新侧 "v1"。
//   - 若实现误传 baseOID（two-dot base 尖端..HEAD）：base-only.txt 以删除条目混入列表，
//     且 file(base-only.txt) 因归属集合误含该路径而不再被拒——两个断言分别击杀两处调用点。
func TestGitBranchDiff_DivergedBase_MergeBaseSemantics(t *testing.T) {
	dir := t.TempDir()
	initMainGitRepo(t, dir)
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("v0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGitInit(t, dir, "add", "a.txt", "b.txt")
	runGitInit(t, dir, "commit", "-qm", "base files")
	runGitInit(t, dir, "checkout", "-qb", "feature")
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("v1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGitInit(t, dir, "add", "a.txt", "b.txt")
	runGitInit(t, dir, "commit", "-qm", "feature changes")
	// base（main）分叉后前进：追加独有文件。
	runGitInit(t, dir, "checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(dir, "base-only.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitInit(t, dir, "add", "base-only.txt")
	runGitInit(t, dir, "commit", "-qm", "base-only")
	runGitInit(t, dir, "checkout", "-q", "feature")

	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	res, err := m.GitBranchDiffFiles(context.Background(), "t1", "main")
	if err != nil {
		t.Fatalf("list err = %v", err)
	}
	var got []string
	for _, f := range res.Files {
		got = append(got, f.Path)
	}
	if len(got) != 2 || got[0] != "a.txt" || got[1] != "b.txt" {
		t.Fatalf("files = %v, want exactly [a.txt b.txt]（mbOID..HEAD，base 尖端独有文件不出现）", got)
	}

	// b.txt 在 mb..HEAD 已变更（归属必通过），且旧侧内容取 merge-base 版本 "v0"。
	d, err := m.GitBranchDiffFile(context.Background(), "t1", "main", "b.txt")
	if err != nil {
		t.Fatalf("file(b.txt) err = %v, want nil（归属按 mbOID..HEAD 判定）", err)
	}
	assertOld(d, "v0\n", true, t)
	assertNew(d, "v1\n", true, t)

	// base-only.txt 仅存在于 base 尖端（mb..HEAD 无变更）→ 归属拒绝 invalid_input。
	// 锁定第二处调用点：若 GitBranchDiffFile 的归属集合误用 baseOID（two-dot），
	// base-only.txt 以删除条目进入集合，将走到侧读取并返回成功 DTO，本断言红。
	if _, err := m.GitBranchDiffFile(context.Background(), "t1", "main", "base-only.txt"); !isOpErrCode(err, codeInvalidInput) {
		t.Fatalf("file(base-only.txt): err = %v, want codeInvalidInput（归属集合必须按 mbOID..HEAD）", err)
	}
}

// --- local-path 验收（任务 2.8） ---

// TestGitBranchDiff_LocalPath_OperatesOnProjectDir repo+local-path 任务可调用两个新方法，
// 操作对象为项目目录当前 checkout（worktree_path 即项目路径）。dir 项目拒绝已在
// TestGitBranchDiff_GateChain_ErrorMatrix 的 dir 子用例覆盖（两方法均 invalid_input）。
// 既有 local-path 行为回归（status/diff/commit/push/编辑读写/diff review 放行与落点、
// push -u 非 force、detached HEAD）由 local_path_mode_test.go 既有测试覆盖，无缺口。
func TestGitBranchDiff_LocalPath_OperatesOnProjectDir(t *testing.T) {
	projDir := t.TempDir()
	initMainGitRepo(t, projDir)
	runGitInit(t, projDir, "checkout", "-qb", "feature")
	if err := os.WriteFile(filepath.Join(projDir, "feat.txt"), []byte("feat\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitInit(t, projDir, "add", "feat.txt")
	runGitInit(t, projDir, "commit", "-qm", "feature changes")

	store := newMockStore()
	seedLocalPathRepoTask(store, "t1", "p1", projDir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	res, err := m.GitBranchDiffFiles(context.Background(), "t1", "main")
	if err != nil {
		t.Fatalf("GitBranchDiffFiles local-path: %v, want nil（门禁放行）", err)
	}
	if res.BaseRef != "main" || len(res.Files) != 1 || res.Files[0].Path != "feat.txt" {
		t.Fatalf("files = (baseRef=%q, %+v), want (main, [feat.txt])", res.BaseRef, res.Files)
	}

	d, err := m.GitBranchDiffFile(context.Background(), "t1", "main", "feat.txt")
	if err != nil {
		t.Fatalf("GitBranchDiffFile local-path: %v, want nil（门禁放行）", err)
	}
	assertOld(d, "", false, t)
	assertNew(d, "feat\n", true, t)
}
