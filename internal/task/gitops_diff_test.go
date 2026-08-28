package task

// codemirror-git-diff 任务 2.6：Manager.GitDiff 六阶段编排与八字段契约的行为测试。
//
// 覆盖：
//   - 固定失败顺序（阶段①先于锁/任务查找；②先于③；④先于⑤；③先于⑤）。
//   - 八字段真值表（untracked/ref/index/删除/二进制清空两侧/truncated/isBinary+truncated共存/空文件/两侧不存在）。
//   - 错误矩阵（invalid_state/git_error stderr 透传/internal 含 path 与操作名/逃逸 invalid_input）。
//   - 词法校验零 git 调用五类（空 path/绝对路径/../NUL/untracked+ref），WorktreePath 指向不存在路径
//     断言 invalid_input 即证明在任何 git 命令前早退（git 被执行会返回 git_error 而非 invalid_input）。
//
// 每个新增行为测试在旧 unified-diff 实现下会失败：旧实现 path 空返回空 diff 文本（非 invalid_input）、
// 无八字段、unmerged 不返回 invalid_state、新侧逃逸不返回 invalid_input。

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ocdeck/internal/infrastructure/git"
)

// --- helpers ---

// seedRepoTask 在 mockStore 中创建 repo 项目与挂起任务，WorktreePath 指向真实 git 仓库 dir。
func seedRepoTask(t *testing.T, store *mockStore, taskID, projectID, dir string) TaskRow {
	t.Helper()
	store.seedProject(ProjectRow{ID: projectID, Name: "p", Path: dir, DefaultBranch: "main", Kind: ProjectKindRepo})
	row := TaskRow{ID: taskID, ProjectID: projectID, Name: "my task",
		Status: StatusSuspended, WorktreePath: dir, InitStatus: InitStatusNone}
	store.tasks[taskID] = row
	return row
}

// commitFile 写入并提交单文件，返回 HEAD。
func commitFile(t *testing.T, dir, path, content string) {
	t.Helper()
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitInit(t, dir, "add", path)
	runGitInit(t, dir, "commit", "-qm", "update "+path)
}

// gitOutput 执行 git 并返回 trim 后的 stdout（失败 fatal）。
func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return strings.TrimSpace(string(out))
}

// seedUnmergedPath 用 update-index --index-info 把 path 写入 stage 1 与 stage 2（无 stage 0），
// 模拟未解决冲突。
func seedUnmergedPath(t *testing.T, dir, path string) {
	t.Helper()
	// 用 hash-object 造一个合法 blob oid 作为冲突双方的占位内容。
	blobFile := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(blobFile, []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oid := gitOutput(t, dir, "hash-object", "-w", blobFile)
	// 先删除已有 stage-0 记录（若有）：--index-info 会追加而非替换已有 stage，
	// 不先删会残留 stage-0 导致冲突判定失效。--ignore-unmatch 容许 path 尚未在 index 中。
	runGitInit(t, dir, "rm", "--cached", "--quiet", "--ignore-unmatch", "--", path)
	info := "100644 " + oid + " 1\t" + path + "\n" +
		"100644 " + oid + " 2\t" + path + "\n"
	cmd := exec.Command("git", "update-index", "--index-info")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(info)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git update-index --index-info: %v\n%s", err, out)
	}
}

// --- 阶段① 词法校验先于锁/任务查找（零 git 调用） ---

// TestGitDiff_LexicalBeforeLock 验证词法校验先于任务锁：持锁任务 + 非法 path → invalid_input（非 conflict）。
func TestGitDiff_LexicalBeforeLock(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1") // WorktreePath 不存在
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	unlock, err := m.tryLockTask("t1")
	if err != nil {
		t.Fatalf("tryLockTask: %v", err)
	}
	defer unlock()

	// 持锁 + 空 path → invalid_input，而非 conflict（阶段①先于锁）。
	if _, err := m.GitDiff(context.Background(), "t1", "", "", false); !isOpErrCode(err, codeInvalidInput) {
		t.Fatalf("held lock + empty path: err = %v, want codeInvalidInput (lexical before lock)", err)
	}
}

// TestGitDiff_LexicalBeforeLookup 验证词法校验先于任务查找：不存在的任务 + 非法 path → invalid_input（非 not_found）。
func TestGitDiff_LexicalBeforeLookup(t *testing.T) {
	store := newMockStore()
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	cases := []struct {
		name string
		ref  string
		path string
	}{
		{"empty path", "", ""},
		{"absolute path", "", "/etc/hosts"},
		{"parent escape", "", "../x.txt"},
		{"NUL in path", "", "a\x00b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 任务 "nope" 不存在：若词法校验晚于任务查找会返回 not_found。
			_, err := m.GitDiff(context.Background(), "nope", c.ref, c.path, false)
			if !isOpErrCode(err, codeInvalidInput) {
				t.Fatalf("nonexistent task + illegal path %q: err = %v, want codeInvalidInput (lexical before lookup)", c.path, err)
			}
		})
	}
}

// TestGitDiff_LexicalZeroGitCalls 验证五类词法非法在 WorktreePath 指向不存在路径时返回 invalid_input，
// 证明在任何 git 命令前早退（git 被执行会返回 git_error 而非 invalid_input）。
func TestGitDiff_LexicalZeroGitCalls(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1") // WorktreePath=/data/worktrees/... 不存在
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	cases := []struct {
		name      string
		ref       string
		path      string
		untracked bool
	}{
		{"empty path", "", "", false},
		{"absolute path", "", "/etc/hosts", false},
		{"parent escape", "", "../x.txt", false},
		{"NUL in path", "", "a\x00b", false},
		{"untracked+ref combination", "HEAD", "a.txt", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := m.GitDiff(context.Background(), "t1", c.ref, c.path, c.untracked)
			if !isOpErrCode(err, codeInvalidInput) {
				t.Fatalf("%s: err = %v, want codeInvalidInput (zero git calls)", c.name, err)
			}
		})
	}
}

// --- 阶段② 在阶段③之前 ---

// TestGitDiff_StateBeforeRefResolution 验证 WorktreePath 空 + ref 非空 → invalid_state（非 git_error）。
func TestGitDiff_StateBeforeRefResolution(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.WorktreePath = "" }) // 清空 worktree
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	_, err := m.GitDiff(context.Background(), "t1", "HEAD", "a.txt", false)
	if !isOpErrCode(err, codeInvalidState) {
		t.Fatalf("empty worktree + ref: err = %v, want codeInvalidState (stage② before ③)", err)
	}
}

// --- 阶段④ 在阶段⑤之前；阶段③ 在阶段⑤之前 ---

// TestGitDiff_OldSideBeforeNewSide 验证 index 未解决冲突（阶段④ invalid_state）先于新侧 IO 错误（阶段⑤ internal）。
// 同一 path 在 index 中处于 unmerged stage 1/2，worktree 对应目录 chmod 000 使新侧 Lstat 返回 EACCES（internal）。
// 期望 invalid_state（阶段④先返回）。非 root 才能触发权限拒绝。
func TestGitDiff_OldSideBeforeNewSide(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 000 权限拒绝在 root 下不生效")
	}
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	commitFile(t, dir, "a.txt", "v1\n") // 建立 HEAD，供后续 untracked-free 校验

	// 造 unmerged path "blocked/file.txt"：仅 stage 1/2，无 stage 0。
	blockedDir := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(blockedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seedUnmergedPath(t, dir, "blocked/file.txt")
	// chmod 000 让新侧 Lstat 返回 EACCES（非 ENOENT）→ internal。
	if err := os.Chmod(blockedDir, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(blockedDir, 0o755) // 恢复以便 t.TempDir 清理

	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	_, err := m.GitDiff(context.Background(), "t1", "", "blocked/file.txt", false)
	if !isOpErrCode(err, codeInvalidState) {
		t.Fatalf("unmerged+blocked new side: err = %v, want codeInvalidState (stage④ before ⑤)", err)
	}
}

// TestGitDiff_RefResolutionBeforeNewSide 验证 ref 无效（阶段③ git_error）先于新侧 IO 错误（阶段⑤ internal）。
func TestGitDiff_RefResolutionBeforeNewSide(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 000 权限拒绝在 root 下不生效")
	}
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	commitFile(t, dir, "a.txt", "v1\n")

	blockedDir := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(blockedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blockedDir, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(blockedDir, 0o755)

	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	_, err := m.GitDiff(context.Background(), "t1", "no-such-ref", "blocked/file.txt", false)
	if !isOpErrCode(err, codeGitError) {
		t.Fatalf("invalid ref+blocked new side: err = %v, want codeGitError (stage③ before ⑤)", err)
	}
}

// --- 八字段真值表 ---

// TestGitDiff_TruthTable 覆盖八字段契约各分支。每个子测试在旧实现下都会失败
//（旧实现返回 unified diff 文本或对 untracked 仅返回新文件 diff，无 old/new content 八字段）。
func TestGitDiff_TruthTable(t *testing.T) {
	t.Run("untracked_new_file", func(t *testing.T) {
		dir := t.TempDir()
		initTestGitRepo(t, dir)
		commitFile(t, dir, "a.txt", "v1\n")
		if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		store := newMockStore()
		seedRepoTask(t, store, "t1", "p1", dir)
		m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

		d, err := m.GitDiff(context.Background(), "t1", "", "notes.txt", true)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		assertOld(d, "", false, t)
		assertNew(d, "hello\n", true, t)
		if d.IsBinary || d.Truncated {
			t.Errorf("untracked: isBinary=%v truncated=%v, want both false", d.IsBinary, d.Truncated)
		}
	})

	t.Run("ref_vs_worktree", func(t *testing.T) {
		dir := t.TempDir()
		initTestGitRepo(t, dir)
		commitFile(t, dir, "a.txt", "v1\n")
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		store := newMockStore()
		seedRepoTask(t, store, "t1", "p1", dir)
		m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

		d, err := m.GitDiff(context.Background(), "t1", "HEAD", "a.txt", false)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		assertOld(d, "v1\n", true, t)
		assertNew(d, "v2\n", true, t)
	})

	t.Run("index_vs_worktree", func(t *testing.T) {
		dir := t.TempDir()
		initTestGitRepo(t, dir)
		commitFile(t, dir, "a.txt", "v1\n")
		// staged 内容为 v2，worktree 再改为 v3：旧侧=index(stage-0)=v2，新侧=worktree=v3。
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGitInit(t, dir, "add", "a.txt")
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v3\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		store := newMockStore()
		seedRepoTask(t, store, "t1", "p1", dir)
		m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

		d, err := m.GitDiff(context.Background(), "t1", "", "a.txt", false)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		assertOld(d, "v2\n", true, t)
		assertNew(d, "v3\n", true, t)
	})

	t.Run("deleted_in_worktree", func(t *testing.T) {
		dir := t.TempDir()
		initTestGitRepo(t, dir)
		commitFile(t, dir, "a.txt", "v1\n")
		if err := os.Remove(filepath.Join(dir, "a.txt")); err != nil {
			t.Fatal(err)
		}
		store := newMockStore()
		seedRepoTask(t, store, "t1", "p1", dir)
		m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

		d, err := m.GitDiff(context.Background(), "t1", "", "a.txt", false)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		assertOld(d, "v1\n", true, t)
		assertNew(d, "", false, t)
	})

	t.Run("binary_clears_both_sides", func(t *testing.T) {
		dir := t.TempDir()
		initTestGitRepo(t, dir)
		commitFile(t, dir, "a.txt", "text\n")
		// 新侧内容含 NUL（前 8000 字节内）→ isBinary=true，两侧内容清空。
		bin := append([]byte("x"), 0)
		bin = append(bin, []byte("y")...)
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), bin, 0o644); err != nil {
			t.Fatal(err)
		}
		store := newMockStore()
		seedRepoTask(t, store, "t1", "p1", dir)
		m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

		d, err := m.GitDiff(context.Background(), "t1", "", "a.txt", false)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !d.IsBinary {
			t.Fatalf("isBinary = false, want true")
		}
		if d.OldContent != "" || d.NewContent != "" {
			t.Errorf("binary: contents not cleared, old=%q new=%q", d.OldContent, d.NewContent)
		}
		if !d.OldExists || !d.NewExists {
			t.Errorf("binary: exists flags = (old=%v,new=%v), want both true", d.OldExists, d.NewExists)
		}
		if d.Truncated {
			t.Errorf("binary: truncated = true, want false")
		}
	})

	t.Run("truncated_over_512KB", func(t *testing.T) {
		dir := t.TempDir()
		initTestGitRepo(t, dir)
		commitFile(t, dir, "a.txt", "v1\n")
		// 新侧 513KB 纯文本（无 NUL）→ truncated=true，内容为 512KB 前缀。
		big := strings.Repeat("a", git.FileContentMaxBytes+1024)
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(big), 0o644); err != nil {
			t.Fatal(err)
		}
		store := newMockStore()
		seedRepoTask(t, store, "t1", "p1", dir)
		m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

		d, err := m.GitDiff(context.Background(), "t1", "", "a.txt", false)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !d.Truncated {
			t.Fatalf("truncated = false, want true")
		}
		if len(d.NewContent) != git.FileContentMaxBytes {
			t.Errorf("newContent len = %d, want %d", len(d.NewContent), git.FileContentMaxBytes)
		}
		if d.NewContent != big[:git.FileContentMaxBytes] {
			t.Errorf("newContent prefix mismatch")
		}
		if d.IsBinary {
			t.Errorf("truncated: isBinary = true, want false")
		}
	})

	t.Run("binary_and_truncated_coexist", func(t *testing.T) {
		dir := t.TempDir()
		initTestGitRepo(t, dir)
		commitFile(t, dir, "a.txt", "text\n")
		// 新侧 >512KB 且首字节 NUL → isBinary=true 且 truncated=true，两侧内容清空但 truncated 保留。
		bin := make([]byte, git.FileContentMaxBytes+1024)
		bin[0] = 0 // 前 8000 字节内含 NUL
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), bin, 0o644); err != nil {
			t.Fatal(err)
		}
		store := newMockStore()
		seedRepoTask(t, store, "t1", "p1", dir)
		m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

		d, err := m.GitDiff(context.Background(), "t1", "", "a.txt", false)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !d.IsBinary || !d.Truncated {
			t.Fatalf("isBinary=%v truncated=%v, want both true", d.IsBinary, d.Truncated)
		}
		if d.OldContent != "" || d.NewContent != "" {
			t.Errorf("binary+truncated: contents not cleared, old=%q new=%q", d.OldContent, d.NewContent)
		}
	})

	t.Run("empty_file_new_side", func(t *testing.T) {
		dir := t.TempDir()
		initTestGitRepo(t, dir)
		commitFile(t, dir, "a.txt", "v1\n")
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(""), 0o644); err != nil {
			t.Fatal(err)
		}
		store := newMockStore()
		seedRepoTask(t, store, "t1", "p1", dir)
		m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

		d, err := m.GitDiff(context.Background(), "t1", "", "a.txt", false)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		assertOld(d, "v1\n", true, t)
		assertNew(d, "", true, t)
		if d.IsBinary {
			t.Errorf("empty file: isBinary = true, want false")
		}
	})

	t.Run("untracked_both_not_exist", func(t *testing.T) {
		// untracked + 文件不存在 → 两侧均不存在，无错误（旧实现会返回 empty diff 或报错）。
		dir := t.TempDir()
		initTestGitRepo(t, dir)
		commitFile(t, dir, "a.txt", "v1\n")
		store := newMockStore()
		seedRepoTask(t, store, "t1", "p1", dir)
		m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

		d, err := m.GitDiff(context.Background(), "t1", "", "ghost.txt", true)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if d.OldExists || d.NewExists {
			t.Errorf("untracked missing: exists = (old=%v,new=%v), want both false", d.OldExists, d.NewExists)
		}
		if d.OldContent != "" || d.NewContent != "" {
			t.Errorf("untracked missing: contents not empty")
		}
		if d.IsBinary || d.Truncated {
			t.Errorf("untracked missing: isBinary=%v truncated=%v, want false", d.IsBinary, d.Truncated)
		}
	})

	t.Run("untracked_zero_git_calls", func(t *testing.T) {
		// WorktreePath 指向不存在路径：untracked 模式旧侧零 git 调用，新侧 Lstat ENOENT → 不存在。
		// 任何 git 命令执行都会因非 git 仓库返回 git_error，返回 DTO 即证明零 git 调用。
		store := newMockStore()
		seedSuspendedTask(store, "t1", "p1")
		m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

		d, err := m.GitDiff(context.Background(), "t1", "", "notes.txt", true)
		if err != nil {
			t.Fatalf("untracked nonexistent worktree: err = %v, want nil (zero git calls)", err)
		}
		if d.OldExists || d.NewExists {
			t.Errorf("exists = (old=%v,new=%v), want both false", d.OldExists, d.NewExists)
		}
	})
}

// assertOld 断言 DTO 旧侧 content/exists。
func assertOld(d GitDiffDTO, wantContent string, wantExists bool, t *testing.T) {
	t.Helper()
	if d.OldContent != wantContent {
		t.Errorf("oldContent = %q, want %q", d.OldContent, wantContent)
	}
	if d.OldExists != wantExists {
		t.Errorf("oldExists = %v, want %v", d.OldExists, wantExists)
	}
}

// assertNew 断言 DTO 新侧 content/exists。
func assertNew(d GitDiffDTO, wantContent string, wantExists bool, t *testing.T) {
	t.Helper()
	if d.NewContent != wantContent {
		t.Errorf("newContent = %q, want %q", d.NewContent, wantContent)
	}
	if d.NewExists != wantExists {
		t.Errorf("newExists = %v, want %v", d.NewExists, wantExists)
	}
}

// --- 错误矩阵 ---

// TestGitDiff_UnmergedConflict_InvalidState 验证 index 未解决冲突 → invalid_state。
// 同时验证 ref 非空时读取 ref 侧（绕过 index 冲突）不报错：旧侧=HEAD blob。
func TestGitDiff_UnmergedConflict_InvalidState(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	commitFile(t, dir, "a.txt", "v1\n")
	// 把 a.txt 置为 unmerged（stage 1/2，无 stage 0）。
	seedUnmergedPath(t, dir, "a.txt")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("wt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	// ref 空 → index 侧 → invalid_state。
	_, err := m.GitDiff(context.Background(), "t1", "", "a.txt", false)
	if !isOpErrCode(err, codeInvalidState) {
		t.Fatalf("unmerged index (ref empty): err = %v, want codeInvalidState", err)
	}
	if !strings.Contains(err.Error(), git.ErrUnmergedPath.Error()) {
		t.Errorf("err message missing sentinel text; got %q", err.Error())
	}

	// ref=HEAD → 读 ref 侧 blob，绕过 index 冲突，不报错。
	d, err := m.GitDiff(context.Background(), "t1", "HEAD", "a.txt", false)
	if err != nil {
		t.Fatalf("unmerged index (ref=HEAD): err = %v, want nil (ref side bypasses index)", err)
	}
	if !d.OldExists || d.OldContent != "v1\n" {
		t.Errorf("ref side content = %q exists=%v, want 'v1\\n' true", d.OldContent, d.OldExists)
	}
}

// TestGitDiff_InvalidRef_GitErrorStderr 验证 ref 无效 → git_error 且透传 git stderr。
func TestGitDiff_InvalidRef_GitErrorStderr(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	commitFile(t, dir, "a.txt", "v1\n")
	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	_, err := m.GitDiff(context.Background(), "t1", "no-such-ref", "a.txt", false)
	if !isOpErrCode(err, codeGitError) {
		t.Fatalf("invalid ref: err = %v, want codeGitError", err)
	}
	// git rev-parse --verify 对不存在的 ref 输出 "fatal: Needed a single revision"。
	if !strings.Contains(err.Error(), "Needed a single revision") {
		t.Errorf("err message = %q, want git stderr passthrough containing 'Needed a single revision'", err.Error())
	}
}

// TestGitDiff_WorktreeIOError_Internal 验证新侧非 ENOENT IO 错误 → internal，
// 且消息含相对 path 与操作名。
func TestGitDiff_WorktreeIOError_Internal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 000 权限拒绝在 root 下不生效")
	}
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	commitFile(t, dir, "a.txt", "v1\n")
	// blocked 目录 chmod 000 → Lstat("blocked/file.txt") 返回 EACCES（非 ENOENT）→ internal。
	blockedDir := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(blockedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blockedDir, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(blockedDir, 0o755)

	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	_, err := m.GitDiff(context.Background(), "t1", "", "blocked/file.txt", false)
	if !isOpErrCode(err, codeInternal) {
		t.Fatalf("blocked new side: err = %v, want codeInternal", err)
	}
	// 消息含相对 path 与操作名（stat）。
	if !strings.Contains(err.Error(), "blocked/file.txt") {
		t.Errorf("err message = %q, want contains relative path", err.Error())
	}
	if !strings.Contains(err.Error(), "stat") {
		t.Errorf("err message = %q, want contains operation name 'stat'", err.Error())
	}
}

// TestGitDiff_WorktreeEscape_InvalidInput 验证新侧真实路径逃逸 → invalid_input。
// 工作区 sub 目录为指向外部目录的 symlink，path="sub/file.txt" 触发禁锢校验。
func TestGitDiff_WorktreeEscape_InvalidInput(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("symlink 创建在 root 外受限，root 下行为不同")
	}
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	commitFile(t, dir, "a.txt", "v1\n")
	// 外部目录 + 目标文件。
	outside := t.TempDir()
	target := filepath.Join(outside, "file.txt")
	if err := os.WriteFile(target, []byte("escape\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 工作区 sub → 外部目录（symlink）。
	if err := os.Symlink(outside, filepath.Join(dir, "sub")); err != nil {
		t.Skipf("symlink creation failed (restricted mode): %v", err)
	}
	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	_, err := m.GitDiff(context.Background(), "t1", "", "sub/file.txt", false)
	if !isOpErrCode(err, codeInvalidInput) {
		t.Fatalf("worktree escape: err = %v, want codeInvalidInput", err)
	}
	if !strings.Contains(err.Error(), git.ErrWorktreeEscape.Error()) {
		t.Errorf("err message = %q, want contains ErrWorktreeEscape", err.Error())
	}
}