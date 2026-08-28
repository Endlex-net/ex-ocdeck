package task

// codemirror-git-diff 任务 6.2/6.5/6.7：I1 契约扩展（mode/symlink/gitlink）Manager 行为测试。
// 覆盖：chmod-only（内容相同 mode 不同）、symlink 目标变更（index blob vs 工作区 Readlink）、
// gitlink OID 变更（index 记录 OID vs 工作区 rev-parse HEAD）、未初始化子模块（toplevel 校验
// 拦截父仓库发现，I5）、dirty 子模块 -dirty 后缀（I6）、新侧 symlink/directory 分支禁锢越界
// → invalid_input（I7）、子模块 dirty 探测失败 → git_error 透传 stderr（I11）。
// 每个用例在旧实现下都会失败：旧契约无 mode 字段（DTO 恒空串），symlink/gitlink 两侧被
// 误判为不存在（内容为空串、exists=false）；未初始化子模块误返回 superproject HEAD、
// dirty 子模块无 -dirty 后缀、禁锢越界分支不报 invalid_input、dirty 探测失败被静默按
// clean 处理（无错误返回）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGitDiff_ChmodOnlyModeChange 验证仅 mode 变更（100644→100755）：两侧内容相同、
// mode 不同（旧实现误报「无变化」的边界场景，spec「权限位变更」scenario）。
func TestGitDiff_ChmodOnlyModeChange(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	commitFile(t, dir, "run.sh", "echo hi\n")
	// 工作区仅 chmod +x（内容不变）；index stage-0 仍为 100644。
	if err := os.Chmod(filepath.Join(dir, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	d, err := m.GitDiff(context.Background(), "t1", "", "run.sh", false)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !d.OldExists || !d.NewExists {
		t.Fatalf("exists = (old=%v,new=%v), want both true", d.OldExists, d.NewExists)
	}
	if d.OldContent != "echo hi\n" || d.NewContent != "echo hi\n" {
		t.Errorf("contents = (old=%q,new=%q), want identical", d.OldContent, d.NewContent)
	}
	if d.OldMode != "100644" || d.NewMode != "100755" {
		t.Errorf("modes = (old=%q,new=%q), want (100644,100755)", d.OldMode, d.NewMode)
	}
	if d.IsBinary || d.Truncated {
		t.Errorf("chmod-only: isBinary=%v truncated=%v, want both false", d.IsBinary, d.Truncated)
	}
}

// TestGitDiff_SymlinkTargetChange 验证 symlink 目标变更（spec「符号链接目标变更」scenario）：
// 旧侧=index stage-0 symlink blob（git show → 链接目标文本），新侧=工作区 Readlink 目标文本，
// 两侧 mode 均为 120000 且不参与二进制嗅探。
func TestGitDiff_SymlinkTargetChange(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; symlink permissions may differ")
	}
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	commitFile(t, dir, "target.txt", "t\n")
	if err := os.Symlink("v1.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	runGitInit(t, dir, "add", "link.txt")
	// 工作区重指 symlink（index 不变）。
	if err := os.Remove(filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("v2.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	d, err := m.GitDiff(context.Background(), "t1", "", "link.txt", false)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !d.OldExists || d.OldContent != "v1.txt" || d.OldMode != "120000" {
		t.Errorf("symlink old side: content=%q mode=%q exists=%v, want 'v1.txt'/120000/true", d.OldContent, d.OldMode, d.OldExists)
	}
	if !d.NewExists || d.NewContent != "v2.txt" || d.NewMode != "120000" {
		t.Errorf("symlink new side: content=%q mode=%q exists=%v, want 'v2.txt'/120000/true", d.NewContent, d.NewMode, d.NewExists)
	}
	if d.IsBinary || d.Truncated {
		t.Errorf("symlink: isBinary=%v truncated=%v, want both false", d.IsBinary, d.Truncated)
	}
}

// TestGitDiff_GitlinkOIDChange 验证 gitlink OID 变更（spec「子模块（gitlink）变更」scenario）：
// 旧侧=index gitlink 记录的 commit OID 文本，新侧=工作区子仓库 rev-parse HEAD 的 OID 文本，
// 两侧 mode 均为 160000 且不参与二进制嗅探。
func TestGitDiff_GitlinkOIDChange(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	commitFile(t, dir, "a.txt", "v1\n")
	// index 侧 gitlink 记录（update-index --cacheinfo 不要求 submodule 对象存在）。
	oid1 := strings.Repeat("1", 40)
	runGitInit(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+oid1+",subm")
	// 工作区 subm 为已初始化子仓库（HEAD=oid2 ≠ oid1）。
	subm := filepath.Join(dir, "subm")
	if err := os.MkdirAll(subm, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitInit(t, subm, "init", "-q")
	runGitInit(t, subm, "config", "user.email", "t@t.com")
	runGitInit(t, subm, "config", "user.name", "tester")
	commitFile(t, subm, "README.md", "init\n")
	oid2 := gitOutput(t, subm, "rev-parse", "HEAD")

	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	d, err := m.GitDiff(context.Background(), "t1", "", "subm", false)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !d.OldExists || d.OldContent != oid1 || d.OldMode != "160000" {
		t.Errorf("gitlink old side: content=%q mode=%q exists=%v, want oid1/160000/true", d.OldContent, d.OldMode, d.OldExists)
	}
	if !d.NewExists || d.NewContent != oid2 || d.NewMode != "160000" {
		t.Errorf("gitlink new side: content=%q mode=%q exists=%v, want oid2/160000/true", d.NewContent, d.NewMode, d.NewExists)
	}
	if d.IsBinary || d.Truncated {
		t.Errorf("gitlink: isBinary=%v truncated=%v, want both false", d.IsBinary, d.Truncated)
	}
}

// TestGitDiff_UninitializedSubmodule 验证真实未初始化子模块的新侧语义（design D4、I5）：
// index 有 gitlink 记录、工作区目录存在但无自身 .git——repo discovery 会向上发现父仓库，
// toplevel 校验 MUST 拦截（不得误返回 superproject HEAD）→ 新侧存在性=true、内容为空、
// mode=160000（非「文件已不存在」）。
func TestGitDiff_UninitializedSubmodule(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	commitFile(t, dir, "a.txt", "v1\n")
	oid1 := strings.Repeat("ab", 20)
	runGitInit(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+oid1+",subm")
	// 真实未初始化子模块形态：目录存在、无自身 .git（gitlink 仅注册在父仓库 index）。
	if err := os.MkdirAll(filepath.Join(dir, "subm"), 0o755); err != nil {
		t.Fatal(err)
	}
	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	d, err := m.GitDiff(context.Background(), "t1", "", "subm", false)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !d.OldExists || d.OldContent != oid1 || d.OldMode != "160000" {
		t.Errorf("uninitialized submodule old side: content=%q mode=%q exists=%v, want oid/160000/true", d.OldContent, d.OldMode, d.OldExists)
	}
	if !d.NewExists || d.NewContent != "" || d.NewMode != "160000" {
		t.Errorf("uninitialized submodule new side: content=%q mode=%q exists=%v, want empty/160000/true", d.NewContent, d.NewMode, d.NewExists)
	}
	// I5 回归锚点：新侧内容 MUST NOT 泄露 superproject HEAD。
	if parentHead := gitOutput(t, dir, "rev-parse", "HEAD"); d.NewContent == parentHead {
		t.Errorf("uninitialized submodule new side leaked superproject HEAD %q", d.NewContent)
	}
}

// TestGitDiff_DirtySubmodule 验证 dirty 子模块的新侧内容（design D4、I6）：tracked 修改使
// status --porcelain 非空 → commit OID 文本追加稳定 -dirty 后缀（对齐旧 unified diff 的
// `Subproject commit <OID>-dirty` 显示语义）。
func TestGitDiff_DirtySubmodule(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	commitFile(t, dir, "a.txt", "v1\n")
	subm := filepath.Join(dir, "subm")
	if err := os.MkdirAll(subm, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitInit(t, subm, "init", "-q")
	runGitInit(t, subm, "config", "user.email", "t@t.com")
	runGitInit(t, subm, "config", "user.name", "tester")
	commitFile(t, subm, "README.md", "init\n")
	head := gitOutput(t, subm, "rev-parse", "HEAD")
	// tracked 修改（未 add）→ dirty。
	if err := os.WriteFile(filepath.Join(subm, "README.md"), []byte("modified\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// index gitlink 记录 OID ≠ 工作区 HEAD（OID 变更 + dirty 同时成立）。
	oid1 := strings.Repeat("1", 40)
	runGitInit(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+oid1+",subm")

	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	d, err := m.GitDiff(context.Background(), "t1", "", "subm", false)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !d.OldExists || d.OldContent != oid1 || d.OldMode != "160000" {
		t.Errorf("dirty submodule old side: content=%q mode=%q exists=%v, want oid1/160000/true", d.OldContent, d.OldMode, d.OldExists)
	}
	if !d.NewExists || d.NewContent != head+"-dirty" || d.NewMode != "160000" {
		t.Errorf("dirty submodule new side: content=%q mode=%q exists=%v, want HEAD OID + '-dirty'/160000/true", d.NewContent, d.NewMode, d.NewExists)
	}
	if d.IsBinary || d.Truncated {
		t.Errorf("dirty submodule: isBinary=%v truncated=%v, want both false", d.IsBinary, d.Truncated)
	}
}

// TestGitDiff_NewSideEscapeBranches_InvalidInput 验证 I7 禁锢前置的用户可见语义：
// symlink 分支（resolved parent 越界）与 directory 分支（resolved target 指向外部仓库目录）
// 均在 Readlink/git 执行之前拒绝 → invalid_input。
func TestGitDiff_NewSideEscapeBranches_InvalidInput(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; symlink permissions may differ")
	}
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	commitFile(t, dir, "a.txt", "v1\n")
	outside := t.TempDir()
	// 外部 symlink（最终组件）+ worktree 内中间级 symlink 目录。
	if err := os.Symlink("legit", filepath.Join(outside, "link.txt")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "esc")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	// 外部仓库目录（directory 分支锚点：外部 repo directory）。
	nested := filepath.Join(outside, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitInit(t, nested, "init", "-q")
	runGitInit(t, nested, "config", "user.email", "t@t.com")
	runGitInit(t, nested, "config", "user.name", "tester")
	commitFile(t, nested, "README.md", "init\n")

	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	if _, err := m.GitDiff(context.Background(), "t1", "", "esc/link.txt", false); !isOpErrCode(err, codeInvalidInput) {
		t.Errorf("symlink parent escape: err = %v, want codeInvalidInput", err)
	}
	if _, err := m.GitDiff(context.Background(), "t1", "", "esc/nested", false); !isOpErrCode(err, codeInvalidInput) {
		t.Errorf("directory escape (external repo): err = %v, want codeInvalidInput", err)
	}
}

// TestGitDiff_SubmoduleDirtyProbe_GitError 验证 dirty 探测失败的用户可见语义（I11，
// 用户裁决）：损坏子模块 index（toplevel/HEAD 校验通过、status --porcelain 确定性失败）
// → git_error 且透传 git stderr，MUST NOT 静默按 clean 处理（丢失 -dirty 语义）。
func TestGitDiff_SubmoduleDirtyProbe_GitError(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	commitFile(t, dir, "a.txt", "v1\n")
	oid1 := strings.Repeat("1", 40)
	runGitInit(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+oid1+",subm")
	subm := filepath.Join(dir, "subm")
	if err := os.MkdirAll(subm, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitInit(t, subm, "init", "-q")
	runGitInit(t, subm, "config", "user.email", "t@t.com")
	runGitInit(t, subm, "config", "user.name", "tester")
	commitFile(t, subm, "README.md", "init\n")
	// 覆写 index 为垃圾数据：dirty 探测确定性失败（旧实现静默按 clean 返回无后缀 OID）。
	if err := os.WriteFile(filepath.Join(subm, ".git", "index"), []byte("garbage-not-an-index"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	_, err := m.GitDiff(context.Background(), "t1", "", "subm", false)
	if !isOpErrCode(err, codeGitError) {
		t.Fatalf("dirty probe failure: err = %v, want codeGitError", err)
	}
	if !strings.Contains(err.Error(), "index") {
		t.Errorf("err message = %q, want git stderr passthrough containing 'index'", err.Error())
	}
}
