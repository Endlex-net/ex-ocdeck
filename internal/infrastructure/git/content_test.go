package git

// codemirror-git-diff 任务 1.5/6.2/6.5：按版本读取单文件内容（ref/index/工作区三分支）测试。
// 覆盖：存在性判定（目录/冲突 stage）、:(literal) 防 pathspec magic（含冒号/magic 字符 path、
// 目录 path）、底层 sentinel 错误、git index 不被修改、512KB 截断、单侧 NUL 嗅探、
// rev-parse option 注入拒绝；mode/symlink/gitlink 分支（任务 6.1/6.2 I1 契约扩展）：
// symlink（120000）内容为链接目标文本、gitlink（160000）内容为 commit OID 文本、
// mode 取自探测记录/工作区类型与权限位、mode 120000/160000 侧不参与 NUL 嗅探；
// 评审 Round 2 修复（任务 6.5）：toplevel 校验拦截父仓库发现（I5）、dirty 子模块 -dirty
// 后缀（I6）、symlink resolved parent / directory resolved target 禁锢前置（I7）、
// owner 执行位 0100 口径（I8）；评审 Round 3 修复（任务 6.7）：toplevel 比较仅去行尾换行
//（I10，尾空格目录）、dirty 探测失败返回错误（I11）。

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// --- ResolveRefOID ---

func TestResolveRefOID_HEAD(t *testing.T) {
	repo := newTestRepo(t)
	oid, err := ResolveRefOID(context.Background(), repo, "HEAD")
	if err != nil {
		t.Fatalf("ResolveRefOID: %v", err)
	}
	if oid != currentRef(t, repo) {
		t.Errorf("oid = %q, want current HEAD %q", oid, currentRef(t, repo))
	}
}

// TestResolveRefOID_RejectsOptionInjection 验证 ref option 注入拒绝（沿用旧 Diff 注入用例意图）：
// rev-parse --verify --end-of-options 必须把 "--output=" 当字面 ref 处理并失败，不得写出文件。
func TestResolveRefOID_RejectsOptionInjection(t *testing.T) {
	repo := newTestRepo(t)
	evilPath := filepath.Join(repo, "evil.txt")
	_, err := ResolveRefOID(context.Background(), repo, "--output="+evilPath)
	if err == nil {
		t.Fatal("expected ref injection to be rejected")
	}
	if _, statErr := os.Stat(evilPath); statErr == nil {
		t.Fatalf("evil file created by ref injection: %s", evilPath)
	}
}

func TestResolveRefOID_InvalidRefStderrPassthrough(t *testing.T) {
	repo := newTestRepo(t)
	_, err := ResolveRefOID(context.Background(), repo, "no-such-ref")
	if err == nil {
		t.Fatal("invalid ref should fail")
	}
	msg := StderrOf(err)
	if msg == "" {
		t.Errorf("StderrOf should surface git diagnostic, got empty (err=%v)", err)
	}
}

// --- ReadRefSideContent（ref 分支）---

func TestReadRefSideContent_BlobVersions(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, "README.md", "v1\n")
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-qm", "v1")
	base := currentRef(t, repo)
	writeFile(t, repo, "README.md", "v2\n")
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-qm", "v2")

	sc, err := ReadRefSideContent(context.Background(), repo, base, "README.md")
	if err != nil {
		t.Fatalf("ReadRefSideContent: %v", err)
	}
	if !sc.Exists || sc.Content != "v1\n" || sc.IsBinary || sc.Truncated {
		t.Errorf("old side at v1 commit: got %+v", sc)
	}
	if sc.Mode != "100644" {
		t.Errorf("old side mode = %q, want 100644 (from probe record)", sc.Mode)
	}
}

func TestReadRefSideContent_MissingPath(t *testing.T) {
	repo := newTestRepo(t)
	sc, err := ReadRefSideContent(context.Background(), repo, currentRef(t, repo), "nope.txt")
	if err != nil {
		t.Fatalf("missing path must be normal not-exists, got err=%v", err)
	}
	if sc.Exists || sc.Content != "" {
		t.Errorf("missing path: got %+v, want Exists=false", sc)
	}
}

// TestReadRefSideContent_DirectoryPathNotExists 验证目录 path 按不存在处理：
// ls-tree 对目录返回其子路径记录，与请求 path 不相等 → 不存在。
func TestReadRefSideContent_DirectoryPathNotExists(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, "sub/a.txt", "a\n")
	writeFile(t, repo, "sub/b.txt", "b\n")
	runGit(t, repo, "add", "sub")
	runGit(t, repo, "commit", "-qm", "add sub")

	// 目录本身：记录路径为 sub/a.txt 等，均 != "sub" → 不存在。
	sc, err := ReadRefSideContent(context.Background(), repo, currentRef(t, repo), "sub")
	if err != nil {
		t.Fatalf("directory path: %v", err)
	}
	if sc.Exists {
		t.Errorf("directory path must not exist, got %+v", sc)
	}
	// 目录内文件正常读取。
	sc, err = ReadRefSideContent(context.Background(), repo, currentRef(t, repo), "sub/a.txt")
	if err != nil {
		t.Fatalf("file in dir: %v", err)
	}
	if !sc.Exists || sc.Content != "a\n" {
		t.Errorf("file in dir: got %+v", sc)
	}
}

// TestReadRefSideContent_ExecutableBitExists 验证 100755 regular blob 视为存在。
func TestReadRefSideContent_ExecutableBitExists(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, "run.sh", "echo hi\n")
	if err := os.Chmod(filepath.Join(repo, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "run.sh")
	runGit(t, repo, "commit", "-qm", "add exec")

	sc, err := ReadRefSideContent(context.Background(), repo, currentRef(t, repo), "run.sh")
	if err != nil {
		t.Fatalf("executable blob: %v", err)
	}
	if !sc.Exists || sc.Content != "echo hi\n" {
		t.Errorf("executable blob: got %+v", sc)
	}
	if sc.Mode != "100755" {
		t.Errorf("executable blob mode = %q, want 100755 (from probe record)", sc.Mode)
	}
}

// TestReadRefSideContent_SymlinkBlob 验证 symlink（120000）blob 按存在处理：
// 内容为链接目标文本（同经 git show <blobOID>），mode=120000，不参与二进制嗅探
//（旧实现按不存在处理，本用例锚定 I1 契约扩展后的语义）。
func TestReadRefSideContent_SymlinkBlob(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; symlink permissions may differ")
	}
	repo := newTestRepo(t)
	writeFile(t, repo, "target.txt", "target\n")
	if err := os.Symlink("target.txt", filepath.Join(repo, "link.txt")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	runGit(t, repo, "add", "link.txt")
	runGit(t, repo, "commit", "-qm", "add symlink")

	sc, err := ReadRefSideContent(context.Background(), repo, currentRef(t, repo), "link.txt")
	if err != nil {
		t.Fatalf("symlink probe: %v", err)
	}
	if !sc.Exists || sc.Content != "target.txt" || sc.Mode != "120000" || sc.IsBinary {
		t.Errorf("symlink ref side: got %+v, want exists + target text + mode 120000 + non-binary", sc)
	}
}

// TestReadRefSideContent_GitlinkOIDContent 验证 gitlink（160000，submodule 条目）按存在处理：
// 内容直接取记录 commit OID 文本（无需 git show），mode=160000。
// 用 update-index --cacheinfo 直接写入 gitlink 记录（不依赖真实 submodule 对象存在）。
func TestReadRefSideContent_GitlinkOIDContent(t *testing.T) {
	repo := newTestRepo(t)
	runGit(t, repo, "update-index", "--add", "--cacheinfo",
		"160000,1234567890123456789012345678901234567890,subm")
	runGit(t, repo, "commit", "-qm", "add gitlink")

	sc, err := ReadRefSideContent(context.Background(), repo, currentRef(t, repo), "subm")
	if err != nil {
		t.Fatalf("gitlink probe: %v", err)
	}
	if !sc.Exists || sc.Content != "1234567890123456789012345678901234567890" || sc.Mode != "160000" || sc.IsBinary {
		t.Errorf("gitlink ref side: got %+v, want exists + commit OID text + mode 160000", sc)
	}
}

// --- ReadIndexSideContent（index 分支）---

func TestReadIndexSideContent_Stage0Content(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, "a.txt", "index-version\n")
	runGit(t, repo, "add", "a.txt")
	// 工作区继续修改：index 侧必须返回暂存版本而非工作区版本。
	writeFile(t, repo, "a.txt", "worktree-version\n")

	sc, err := ReadIndexSideContent(context.Background(), repo, "a.txt")
	if err != nil {
		t.Fatalf("ReadIndexSideContent: %v", err)
	}
	if !sc.Exists || sc.Content != "index-version\n" {
		t.Errorf("index side: got %+v, want staged content", sc)
	}
	if sc.Mode != "100644" {
		t.Errorf("index side mode = %q, want 100644 (from probe record)", sc.Mode)
	}
}

func TestReadIndexSideContent_MissingPath(t *testing.T) {
	repo := newTestRepo(t)
	sc, err := ReadIndexSideContent(context.Background(), repo, "nope.txt")
	if err != nil {
		t.Fatalf("missing path must be normal not-exists, got err=%v", err)
	}
	if sc.Exists {
		t.Errorf("missing path: got %+v, want Exists=false", sc)
	}
}

// TestReadIndexSideContent_DirectoryExactMatch 验证记录路径与请求 path 精确相等：
// :(literal) 目录 path 会返回其下全部子路径记录，必须按不存在处理（design D3 实测契约）。
func TestReadIndexSideContent_DirectoryExactMatch(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, "sub/a.txt", "a\n")
	writeFile(t, repo, "sub/b.txt", "b\n")
	runGit(t, repo, "add", "sub")

	sc, err := ReadIndexSideContent(context.Background(), repo, "sub")
	if err != nil {
		t.Fatalf("directory path: %v", err)
	}
	if sc.Exists {
		t.Errorf("directory path must not exist (records are sub/a.txt etc.), got %+v", sc)
	}
	// 子路径正常存在。
	sc, err = ReadIndexSideContent(context.Background(), repo, "sub/a.txt")
	if err != nil {
		t.Fatalf("file in dir: %v", err)
	}
	if !sc.Exists || sc.Content != "a\n" {
		t.Errorf("file in dir: got %+v", sc)
	}
}

// TestReadIndexSideContent_UnmergedConflict 验证冲突 stage 组合（无 stage-0、有其他 stage）
// 返回 ErrUnmergedPath 哨兵。用 update-index --index-info 直接构造 stage 1/2/3 记录
//（逗号 --cacheinfo 语法不支持 stage 字段）。
func TestReadIndexSideContent_UnmergedConflict(t *testing.T) {
	repo := newTestRepo(t)
	hashObj := func(content string) string {
		t.Helper()
		cmd := exec.Command("git", "hash-object", "-w", "--stdin")
		cmd.Dir = repo
		cmd.Stdin = strings.NewReader(content)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("hash-object: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	// --index-info 输入格式与 ls-files --stage 相同："<mode> <object> <stage>\t<path>"。
	stageInfo := strings.Join([]string{
		"100644 " + hashObj("base\n") + " 1\tf.txt",
		"100644 " + hashObj("ours\n") + " 2\tf.txt",
		"100644 " + hashObj("theirs\n") + " 3\tf.txt",
	}, "\n") + "\n"
	cmd := exec.Command("git", "update-index", "--index-info")
	cmd.Dir = repo
	cmd.Stdin = strings.NewReader(stageInfo)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("update-index --index-info: %v\n%s", err, out)
	}

	_, err := ReadIndexSideContent(context.Background(), repo, "f.txt")
	if !errors.Is(err, ErrUnmergedPath) {
		t.Fatalf("unmerged path: err = %v, want ErrUnmergedPath", err)
	}
}

// TestReadIndexSideContent_SymlinkStage0 验证 stage-0 symlink（120000）按存在处理：
// 内容经 git show <blobOID> 读取（即链接目标文本），mode=120000。
func TestReadIndexSideContent_SymlinkStage0(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; symlink permissions may differ")
	}
	repo := newTestRepo(t)
	writeFile(t, repo, "target.txt", "target\n")
	if err := os.Symlink("target.txt", filepath.Join(repo, "link.txt")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	runGit(t, repo, "add", "link.txt")

	sc, err := ReadIndexSideContent(context.Background(), repo, "link.txt")
	if err != nil {
		t.Fatalf("symlink stage-0 probe: %v", err)
	}
	if !sc.Exists || sc.Content != "target.txt" || sc.Mode != "120000" || sc.IsBinary {
		t.Errorf("symlink stage-0: got %+v, want exists + target text + mode 120000", sc)
	}
}

// TestReadIndexSideContent_GitlinkStage0 验证 stage-0 gitlink（160000）按存在处理：
// 内容直接取记录 OID 文本（无需 git show），mode=160000。
func TestReadIndexSideContent_GitlinkStage0(t *testing.T) {
	repo := newTestRepo(t)
	runGit(t, repo, "update-index", "--add", "--cacheinfo",
		"160000,1234567890123456789012345678901234567890,subm")

	sc, err := ReadIndexSideContent(context.Background(), repo, "subm")
	if err != nil {
		t.Fatalf("gitlink stage-0 probe: %v", err)
	}
	if !sc.Exists || sc.Content != "1234567890123456789012345678901234567890" || sc.Mode != "160000" {
		t.Errorf("gitlink stage-0: got %+v, want exists + OID text + mode 160000", sc)
	}
}

// --- literal pathspec 防 magic（探测分支共用语义）---

// TestSideContent_LiteralPathspecRejectsMagic 验证 :(literal) 包裹后 magic pathspec
// 仅匹配字面路径（不存在），不得扩大匹配范围（沿用旧 Diff magic 用例意图）。
func TestSideContent_LiteralPathspecRejectsMagic(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, "a.txt", "1\n")
	writeFile(t, repo, "b.txt", "2\n")
	runGit(t, repo, "add", "a.txt", "b.txt")
	runGit(t, repo, "commit", "-qm", "base")

	// ref 分支：literal ":(exclude)a.txt" 不存在，b.txt 不泄漏进结果。
	sc, err := ReadRefSideContent(context.Background(), repo, currentRef(t, repo), ":(exclude)a.txt")
	if err != nil {
		t.Fatalf("magic path ref branch: %v", err)
	}
	if sc.Exists {
		t.Errorf("magic pathspec must not match in ref branch, got %+v", sc)
	}
	// index 分支同语义。
	sc, err = ReadIndexSideContent(context.Background(), repo, ":(exclude)a.txt")
	if err != nil {
		t.Fatalf("magic path index branch: %v", err)
	}
	if sc.Exists {
		t.Errorf("magic pathspec must not match in index branch, got %+v", sc)
	}
}

// TestSideContent_ColonPathReadable 验证冒号开头路径可正常读取（literal 包裹正确性）。
func TestSideContent_ColonPathReadable(t *testing.T) {
	repo := newTestRepo(t)
	// 用 git add -A 暂存（直接传 :colon.txt 会被 git 当作 magic pathspec 拒绝）。
	writeFile(t, repo, ":colon.txt", "x\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "base")

	sc, err := ReadRefSideContent(context.Background(), repo, currentRef(t, repo), ":colon.txt")
	if err != nil {
		t.Fatalf("colon path ref branch: %v", err)
	}
	if !sc.Exists || sc.Content != "x\n" {
		t.Errorf("colon path ref branch: got %+v", sc)
	}
	sc, err = ReadIndexSideContent(context.Background(), repo, ":colon.txt")
	if err != nil {
		t.Fatalf("colon path index branch: %v", err)
	}
	if !sc.Exists || sc.Content != "x\n" {
		t.Errorf("colon path index branch: got %+v", sc)
	}
}

// --- ReadWorktreeSideContent（工作区分支）---

func TestReadWorktreeSideContent_Basic(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, "a.txt", "worktree\n")

	sc, err := ReadWorktreeSideContent(context.Background(), repo, "a.txt")
	if err != nil {
		t.Fatalf("ReadWorktreeSideContent: %v", err)
	}
	if !sc.Exists || sc.Content != "worktree\n" || sc.IsBinary || sc.Truncated {
		t.Errorf("worktree side: got %+v", sc)
	}
	if sc.Mode != "100644" {
		t.Errorf("worktree side mode = %q, want 100644 (from permission bits)", sc.Mode)
	}
}

func TestReadWorktreeSideContent_MissingFileNotExists(t *testing.T) {
	repo := newTestRepo(t)
	sc, err := ReadWorktreeSideContent(context.Background(), repo, "nope.txt")
	if err != nil {
		t.Fatalf("missing worktree file must be normal not-exists, got err=%v", err)
	}
	if sc.Exists || sc.Content != "" {
		t.Errorf("missing worktree file: got %+v, want Exists=false", sc)
	}
}

// TestReadWorktreeSideContent_Symlink 验证工作区 symlink 按存在处理（design D4）：
// 内容为 Readlink 目标文本（读链接文本而非跟随），mode=120000；链接目标文本本身不受禁锢
//（指向 worktree 外也不报 escape），中间级 symlink 逃逸由 resolved parent 校验拦截
//（见 TestReadWorktreeSideContent_SymlinkParentEscapeRejected）。
func TestReadWorktreeSideContent_Symlink(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; symlink permissions may differ")
	}
	repo := newTestRepo(t)
	if err := os.Symlink("target.txt", filepath.Join(repo, "link.txt")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	sc, err := ReadWorktreeSideContent(context.Background(), repo, "link.txt")
	if err != nil {
		t.Fatalf("worktree symlink: %v", err)
	}
	if !sc.Exists || sc.Content != "target.txt" || sc.Mode != "120000" || sc.IsBinary {
		t.Errorf("worktree symlink: got %+v, want exists + readlink text + mode 120000 + non-binary", sc)
	}

	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(repo, "outlink")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	sc, err = ReadWorktreeSideContent(context.Background(), repo, "outlink")
	if err != nil {
		t.Fatalf("worktree symlink to outside: %v", err)
	}
	if !sc.Exists || sc.Content != outside || sc.Mode != "120000" {
		t.Errorf("worktree symlink to outside: got %+v, want exists + target text (no containment check)", sc)
	}
}

// TestReadWorktreeSideContent_DirectoryGitlink 验证工作区 directory 按 gitlink 处理（design D4）：
// 已初始化子仓库 → 内容为 rev-parse HEAD 的 commit OID 文本，dirty（tracked 修改或 untracked
// 文件）时追加 -dirty 后缀；真实未初始化子模块（普通目录，toplevel 校验拦截父仓库发现，
// I5）与损坏 .git（rev-parse 失败）→ 存在性=true、内容为空、mode=160000（正常结果，非错误）。
func TestReadWorktreeSideContent_DirectoryGitlink(t *testing.T) {
	repo := newTestRepo(t)
	subm := filepath.Join(repo, "subm")
	if err := os.MkdirAll(subm, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, subm, "init", "-q")
	runGit(t, subm, "config", "user.email", "t@t.com")
	runGit(t, subm, "config", "user.name", "tester")
	writeFile(t, subm, "README.md", "init\n")
	runGit(t, subm, "add", "README.md")
	runGit(t, subm, "commit", "-qm", "init")
	head := currentRef(t, subm)

	sc, err := ReadWorktreeSideContent(context.Background(), repo, "subm")
	if err != nil {
		t.Fatalf("clean submodule dir: %v", err)
	}
	if !sc.Exists || sc.Content != head || sc.Mode != "160000" || sc.IsBinary {
		t.Errorf("clean submodule dir: got %+v, want exists + HEAD OID + mode 160000", sc)
	}

	// tracked 修改：status --porcelain 非空 → OID 追加稳定 -dirty 后缀（I6）。
	writeFile(t, subm, "README.md", "modified\n")
	sc, err = ReadWorktreeSideContent(context.Background(), repo, "subm")
	if err != nil {
		t.Fatalf("dirty (tracked) submodule dir: %v", err)
	}
	if !sc.Exists || sc.Content != head+"-dirty" || sc.Mode != "160000" {
		t.Errorf("dirty (tracked) submodule dir: got %+v, want HEAD OID + '-dirty'", sc)
	}

	// untracked 文件同样构成 dirty；恢复 tracked 内容排除干扰。
	writeFile(t, subm, "README.md", "init\n")
	writeFile(t, subm, "untracked.txt", "u\n")
	sc, err = ReadWorktreeSideContent(context.Background(), repo, "subm")
	if err != nil {
		t.Fatalf("dirty (untracked) submodule dir: %v", err)
	}
	if !sc.Exists || sc.Content != head+"-dirty" {
		t.Errorf("dirty (untracked) submodule dir: got %+v, want HEAD OID + '-dirty'", sc)
	}
	os.Remove(filepath.Join(subm, "untracked.txt"))

	// 真实未初始化子模块：目录存在但无自身 .git（gitlink 注册在父仓库 index）——repo
	// discovery 会向上发现父仓库并返回 superproject HEAD，toplevel 校验 MUST 拦截（I5）。
	if err := os.MkdirAll(filepath.Join(repo, "plain-subm"), 0o755); err != nil {
		t.Fatal(err)
	}
	sc, err = ReadWorktreeSideContent(context.Background(), repo, "plain-subm")
	if err != nil {
		t.Fatalf("uninitialized submodule dir: %v", err)
	}
	if !sc.Exists || sc.Content != "" || sc.Mode != "160000" {
		t.Errorf("uninitialized submodule dir: got %+v, want exists + empty content + mode 160000", sc)
	}

	// 损坏 .git（gitdir 指向不存在路径，deinit/损坏子模块形态）→ rev-parse 失败，
	// 同样按未初始化处理。
	brokenSubm := filepath.Join(repo, "broken-subm")
	if err := os.MkdirAll(brokenSubm, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brokenSubm, ".git"),
		[]byte("gitdir: /nonexistent/modules/broken-subm\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sc, err = ReadWorktreeSideContent(context.Background(), repo, "broken-subm")
	if err != nil {
		t.Fatalf("broken submodule dir: %v", err)
	}
	if !sc.Exists || sc.Content != "" || sc.Mode != "160000" {
		t.Errorf("broken submodule dir: got %+v, want exists + empty content + mode 160000", sc)
	}
}

// TestReadWorktreeSideContent_TrailingSpaceSubmoduleDir 验证 toplevel 比较仅去除 git 追加的
// 行尾换行（I10）：尾空格目录的已初始化子模块不被误判未初始化（旧实现整体 TrimSpace 会
// 删去路径自身的尾空格导致 mismatch → 空内容）。
func TestReadWorktreeSideContent_TrailingSpaceSubmoduleDir(t *testing.T) {
	repo := newTestRepo(t)
	subm := filepath.Join(repo, "subm ") // 目录名含尾空格（合法路径）
	if err := os.MkdirAll(subm, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, subm, "init", "-q")
	runGit(t, subm, "config", "user.email", "t@t.com")
	runGit(t, subm, "config", "user.name", "tester")
	writeFile(t, subm, "README.md", "init\n")
	runGit(t, subm, "add", "README.md")
	runGit(t, subm, "commit", "-qm", "init")
	head := currentRef(t, subm)

	sc, err := ReadWorktreeSideContent(context.Background(), repo, "subm ")
	if err != nil {
		t.Fatalf("trailing-space submodule dir: %v", err)
	}
	if !sc.Exists || sc.Content != head || sc.Mode != "160000" {
		t.Errorf("trailing-space submodule dir: got %+v, want exists + HEAD OID + mode 160000", sc)
	}
}

// TestReadWorktreeSideContent_DirtyProbeFailure 验证 dirty 探测失败 MUST NOT 静默按 clean
// 处理（I11，用户裁决）：损坏子模块 index 使 status --porcelain 确定性失败（toplevel/HEAD
// 两条 rev-parse 不读 index、仍成功）→ 返回 ErrSubmoduleDirtyProbe 且 stderr 可透传，
// 而非返回无 -dirty 的 OID。
func TestReadWorktreeSideContent_DirtyProbeFailure(t *testing.T) {
	repo := newTestRepo(t)
	subm := filepath.Join(repo, "subm")
	if err := os.MkdirAll(subm, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, subm, "init", "-q")
	runGit(t, subm, "config", "user.email", "t@t.com")
	runGit(t, subm, "config", "user.name", "tester")
	writeFile(t, subm, "README.md", "init\n")
	runGit(t, subm, "add", "README.md")
	runGit(t, subm, "commit", "-qm", "init")
	// 覆写 index 为垃圾数据：rev-parse --show-toplevel/HEAD 不受影响，status 必然失败。
	if err := os.WriteFile(filepath.Join(subm, ".git", "index"), []byte("garbage-not-an-index"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := ReadWorktreeSideContent(context.Background(), repo, "subm")
	if !errors.Is(err, ErrSubmoduleDirtyProbe) {
		t.Fatalf("dirty probe failure: err = %v, want ErrSubmoduleDirtyProbe", err)
	}
	if msg := StderrOf(err); !strings.Contains(msg, "index") {
		t.Errorf("dirty probe failure: stderr passthrough missing git diagnostic, got %q", msg)
	}
}

// TestReadWorktreeSideContent_ExecutableMode 验证 regular file mode 依 owner 执行位映射
//（0100 → 100755；group/other 执行位不参与判定，如 0654 → 100644）。
func TestReadWorktreeSideContent_ExecutableMode(t *testing.T) {
	repo := newTestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "run.sh"), []byte("echo hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sc, err := ReadWorktreeSideContent(context.Background(), repo, "run.sh")
	if err != nil {
		t.Fatalf("executable worktree file: %v", err)
	}
	if !sc.Exists || sc.Mode != "100755" {
		t.Errorf("executable worktree file: got %+v, want mode 100755", sc)
	}

	// group 执行位但 owner 无执行位（0654）：MUST 仍为 100644（不得用任一 0111 位判定，I8）。
	if err := os.WriteFile(filepath.Join(repo, "groupexec.sh"), []byte("x\n"), 0o654); err != nil {
		t.Fatal(err)
	}
	sc, err = ReadWorktreeSideContent(context.Background(), repo, "groupexec.sh")
	if err != nil {
		t.Fatalf("group-exec worktree file: %v", err)
	}
	if sc.Mode != "100644" {
		t.Errorf("group-exec (0654) worktree file: mode = %q, want 100644 (owner exec bit only)", sc.Mode)
	}
}

func TestReadWorktreeSideContent_RejectsLexicallyInvalidPaths(t *testing.T) {
	repo := newTestRepo(t)
	for _, p := range []string{"", "/abs/path", "../x", "a/../../b", "a\x00b"} {
		_, err := ReadWorktreeSideContent(context.Background(), repo, p)
		if !errors.Is(err, ErrInvalidDiffPath) {
			t.Errorf("path %q: err = %v, want ErrInvalidDiffPath", p, err)
		}
	}
}

// TestReadWorktreeSideContent_SymlinkEscapeRejected 验证中间级 symlink 逃逸（regular 分支）：
// worktree 内 symlink 目录指向外部，path 经中间级 symlink 指向的 regular file 在 resolve 后
// 越出 worktree 根 → ErrWorktreeEscape。（末级 symlink / directory 分支的禁锢见
// TestReadWorktreeSideContent_SymlinkParentEscapeRejected / DirectoryEscapeRejected。）
func TestReadWorktreeSideContent_SymlinkEscapeRejected(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; symlink permissions may differ")
	}
	repo := newTestRepo(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "escape")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	_, err := ReadWorktreeSideContent(context.Background(), repo, "escape/secret.txt")
	if !errors.Is(err, ErrWorktreeEscape) {
		t.Fatalf("symlink escape: err = %v, want ErrWorktreeEscape", err)
	}
}

// TestReadWorktreeSideContent_SymlinkParentEscapeRejected 验证 symlink 分支的禁锢校验前置
//（I7）：path 的中间级 symlink 目录指向 worktree 外、最终组件为外部 symlink——resolved
// parent 越出 worktree 根 → ErrWorktreeEscape（在 Readlink 之前拒绝，不泄露外部链接目标）。
func TestReadWorktreeSideContent_SymlinkParentEscapeRejected(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; symlink permissions may differ")
	}
	repo := newTestRepo(t)
	outside := t.TempDir()
	if err := os.Symlink("legit", filepath.Join(outside, "link.txt")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "esc")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	_, err := ReadWorktreeSideContent(context.Background(), repo, "esc/link.txt")
	if !errors.Is(err, ErrWorktreeEscape) {
		t.Fatalf("symlink parent escape: err = %v, want ErrWorktreeEscape", err)
	}
}

// TestReadWorktreeSideContent_DirectoryEscapeRejected 验证 directory 分支的禁锢校验前置
//（I7）：path 经中间级 symlink 指向 worktree 外的仓库目录（外部 repo directory 锚点）——
// resolved target 越界 → ErrWorktreeEscape（在任何 git 命令之前拒绝，不泄露外部仓库 HEAD）。
func TestReadWorktreeSideContent_DirectoryEscapeRejected(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; symlink permissions may differ")
	}
	repo := newTestRepo(t)
	outside := newTestRepo(t)
	nested := filepath.Join(outside, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, nested, "init", "-q")
	runGit(t, nested, "config", "user.email", "t@t.com")
	runGit(t, nested, "config", "user.name", "tester")
	writeFile(t, nested, "README.md", "init\n")
	runGit(t, nested, "add", "README.md")
	runGit(t, nested, "commit", "-qm", "init")
	if err := os.Symlink(outside, filepath.Join(repo, "escdir")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	_, err := ReadWorktreeSideContent(context.Background(), repo, "escdir/nested")
	if !errors.Is(err, ErrWorktreeEscape) {
		t.Fatalf("directory escape: err = %v, want ErrWorktreeEscape", err)
	}
}

// TestReadWorktreeSideContent_BoundedPrefix512KB 验证 512KB+1 有界读取判定截断。
func TestReadWorktreeSideContent_BoundedPrefix512KB(t *testing.T) {
	repo := newTestRepo(t)
	content := strings.Repeat("x", FileContentMaxBytes+10)
	if err := os.WriteFile(filepath.Join(repo, "big.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	sc, err := ReadWorktreeSideContent(context.Background(), repo, "big.txt")
	if err != nil {
		t.Fatalf("big file: %v", err)
	}
	if !sc.Exists || !sc.Truncated {
		t.Fatalf("big file: got %+v, want Exists+Truncated", sc)
	}
	if len(sc.Content) != FileContentMaxBytes {
		t.Errorf("truncated content len = %d, want %d", len(sc.Content), FileContentMaxBytes)
	}
	if sc.Content != content[:FileContentMaxBytes] {
		t.Error("truncated content must be the bounded prefix")
	}

	// 恰好 512KB：不截断。
	if err := os.WriteFile(filepath.Join(repo, "exact.txt"), []byte(strings.Repeat("y", FileContentMaxBytes)), 0o644); err != nil {
		t.Fatal(err)
	}
	sc, err = ReadWorktreeSideContent(context.Background(), repo, "exact.txt")
	if err != nil {
		t.Fatalf("exact file: %v", err)
	}
	if sc.Truncated || len(sc.Content) != FileContentMaxBytes {
		t.Errorf("exact 512KB file: got truncated=%v len=%d, want false/%d", sc.Truncated, len(sc.Content), FileContentMaxBytes)
	}
}

// TestReadWorktreeSideContent_NULSniff 验证前 8000 字节 NUL 嗅探：
// NUL 在窗口内 → IsBinary；NUL 超出窗口（>8000 字节）→ 非 binary。
func TestReadWorktreeSideContent_NULSniff(t *testing.T) {
	repo := newTestRepo(t)
	binPath := filepath.Join(repo, "sniff-bin.dat")
	bin := append([]byte(strings.Repeat("a", binarySniffBytes-1)), 0x00)
	if err := os.WriteFile(binPath, bin, 0o644); err != nil {
		t.Fatal(err)
	}
	sc, err := ReadWorktreeSideContent(context.Background(), repo, "sniff-bin.dat")
	if err != nil {
		t.Fatalf("binary sniff: %v", err)
	}
	if !sc.Exists || !sc.IsBinary {
		t.Errorf("NUL within sniff window: got %+v, want IsBinary", sc)
	}

	latePath := filepath.Join(repo, "sniff-late.dat")
	late := append([]byte(strings.Repeat("a", binarySniffBytes)), 0x00)
	if err := os.WriteFile(latePath, late, 0o644); err != nil {
		t.Fatal(err)
	}
	sc, err = ReadWorktreeSideContent(context.Background(), repo, "sniff-late.dat")
	if err != nil {
		t.Fatalf("late NUL sniff: %v", err)
	}
	if sc.IsBinary {
		t.Errorf("NUL beyond sniff window must not be binary, got %+v", sc)
	}
}

// TestReadWorktreeSideContent_IOErrorContainsPathAndOp 验证非 ENOENT IO 错误
// 返回明确错误（消息含相对 path 与操作名）。
func TestReadWorktreeSideContent_IOErrorContainsPathAndOp(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission bits may not deny read")
	}
	repo := newTestRepo(t)
	p := filepath.Join(repo, "noperm.txt")
	if err := os.WriteFile(p, []byte("content\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })

	_, err := ReadWorktreeSideContent(context.Background(), repo, "noperm.txt")
	if err == nil {
		t.Skip("read succeeded despite 0o000 (filesystem does not enforce permissions)")
	}
	if !strings.Contains(err.Error(), "noperm.txt") {
		t.Errorf("IO error must contain relative path, got %v", err)
	}
	if !strings.Contains(err.Error(), "open") {
		t.Errorf("IO error must contain op name (open), got %v", err)
	}
}

// --- 容量/嗅探与 index 不变性 ---

// TestReadSideContent_IndexInvariant 验证三分支读取均不修改 git index（只读契约）。
// 含 directory gitlink 分支（工作区侧执行 git rev-parse，同为只读）。
func TestReadSideContent_IndexInvariant(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, "tracked.txt", "init\n")
	runGit(t, repo, "add", "tracked.txt")
	writeFile(t, repo, "new.txt", "new content\n")
	if err := os.MkdirAll(filepath.Join(repo, "plaindir"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := cachedDiff(t, repo)

	oid := currentRef(t, repo)
	if _, err := ReadRefSideContent(context.Background(), repo, oid, "tracked.txt"); err != nil {
		t.Fatalf("ref branch: %v", err)
	}
	if _, err := ReadIndexSideContent(context.Background(), repo, "tracked.txt"); err != nil {
		t.Fatalf("index branch: %v", err)
	}
	if _, err := ReadWorktreeSideContent(context.Background(), repo, "new.txt"); err != nil {
		t.Fatalf("worktree branch: %v", err)
	}
	if _, err := ReadWorktreeSideContent(context.Background(), repo, "plaindir"); err != nil {
		t.Fatalf("worktree directory branch: %v", err)
	}

	after := cachedDiff(t, repo)
	if before != after {
		t.Errorf("index changed after side content reads: before=%q after=%q", before, after)
	}
}

// TestReadRefSideContent_OverflowTruncated 验证 git show 超 16MB exec 上限的
// ErrOutputTruncated 真值表：stdout 非空且 stderr 空 → 512KB 有界前缀 + Truncated，不报错。
func TestReadRefSideContent_OverflowTruncated(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping >16MB blob test in -short mode")
	}
	repo := newTestRepo(t)
	big := strings.Repeat("x", 17*1024*1024) // 17MB > 16MB exec 上限
	if err := os.WriteFile(filepath.Join(repo, "big.blob"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "big.blob")
	runGit(t, repo, "commit", "-qm", "add big blob")

	sc, err := ReadRefSideContent(context.Background(), repo, currentRef(t, repo), "big.blob")
	if err != nil {
		t.Fatalf("overflow should be handled as truncated, got err=%v", err)
	}
	if !sc.Truncated {
		t.Error("overflow should return Truncated=true")
	}
	if len(sc.Content) != FileContentMaxBytes {
		t.Errorf("overflow content len = %d, want %d", len(sc.Content), FileContentMaxBytes)
	}
}

// TestReadIndexSideContent_BinarySniff 验证 index 侧二进制嗅探（git show 输出路径）。
func TestReadIndexSideContent_BinarySniff(t *testing.T) {
	repo := newTestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "bin.dat"), []byte{0x00, 0x01, 0x02}, 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "bin.dat")

	sc, err := ReadIndexSideContent(context.Background(), repo, "bin.dat")
	if err != nil {
		t.Fatalf("binary index side: %v", err)
	}
	if !sc.Exists || !sc.IsBinary {
		t.Errorf("binary index side: got %+v, want Exists+IsBinary", sc)
	}
}
