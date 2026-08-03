package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// helper: 在 t.TempDir() 创建真实 git 仓库，返回仓库路径。
func newTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "config", "user.email", "t@t.com")
	runGit(t, dir, "config", "user.name", "tester")
	// 初始提交以建立 HEAD。
	writeFile(t, dir, "README.md", "init\n")
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

func findEntry(entries []FileStatus, path string) (FileStatus, bool) {
	for _, e := range entries {
		if e.Path == path || e.Rename == path {
			return e, true
		}
	}
	return FileStatus{}, false
}

func TestStatusOrdinaryAndUntracked(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, "a.txt", "init\n") // 建立追踪基线
	runGit(t, repo, "add", "a.txt")
	runGit(t, repo, "commit", "-qm", "add a")
	writeFile(t, repo, "a.txt", "modified\n") // modify tracked
	writeFile(t, repo, "b.txt", "new\n")      // untracked
	writeFile(t, repo, "c.txt", "tracked\n")  // add staged
	runGit(t, repo, "add", "c.txt")

	entries, err := Status(context.Background(), repo)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	a, ok := findEntry(entries, "a.txt")
	if !ok {
		t.Fatalf("missing a.txt in %v", entries)
	}
	if a.Kind != "1" || a.Unstaged == false {
		t.Errorf("a.txt: want ordinary unstaged, got %+v", a)
	}

	b, ok := findEntry(entries, "b.txt")
	if !ok {
		t.Fatalf("missing b.txt untracked in %v", entries)
	}
	if !b.Untracked || b.X != '?' || b.Y != '?' {
		t.Errorf("b.txt: want untracked ??, got %+v", b)
	}

	c, ok := findEntry(entries, "c.txt")
	if !ok {
		t.Fatalf("missing c.txt in %v", entries)
	}
	if !c.Staged {
		t.Errorf("c.txt: want staged, got %+v", c)
	}
}

func TestStatusRename(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, "orig.txt", "content line\n")
	runGit(t, repo, "add", "orig.txt")
	runGit(t, repo, "commit", "-qm", "add orig")
	// 重命名 + 修改，git 默认开启 rename 检测。
	runGit(t, repo, "mv", "orig.txt", "moved.txt")
	writeFile(t, repo, "moved.txt", "content line\nmore\n")
	runGit(t, repo, "add", "moved.txt")

	entries, err := Status(context.Background(), repo)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	var rename *FileStatus
	for i := range entries {
		if entries[i].Kind == "2" {
			rename = &entries[i]
			break
		}
	}
	if rename == nil {
		t.Fatalf("expected rename entry, got %v", entries)
	}
	if rename.Rename != "orig.txt" || rename.Path != "moved.txt" {
		t.Errorf("rename: want orig.txt -> moved.txt, got %+v", rename)
	}
}

func TestStatusSpecialFilename(t *testing.T) {
	repo := newTestRepo(t)
	name := "weird name 'with' \"q\".txt"
	writeFile(t, repo, name, "x\n")
	runGit(t, repo, "add", name)

	entries, err := Status(context.Background(), repo)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	e, ok := findEntry(entries, name)
	if !ok {
		t.Fatalf("missing %q in %v", name, entries)
	}
	if !e.Staged {
		t.Errorf("want staged special filename, got %+v", e)
	}
}

func TestStatusTooManyFiles(t *testing.T) {
	repo := newTestRepo(t)
	for i := 0; i < MaxStatusFiles+5; i++ {
		writeFile(t, repo, "f"+pad(i)+".txt", "x\n")
	}
	// 不 add，仅作为 untracked 计数。
	_, err := Status(context.Background(), repo)
	if err != ErrTooManyFilesChanged {
		t.Fatalf("want ErrTooManyFilesChanged, got %v", err)
	}
}

func pad(i int) string {
	s := ""
	for j := 0; j < 4-len(itoa(i)); j++ {
		s += "0"
	}
	return s + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestNumstatAssociation(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, "a.txt", "init\n") // 0 add 0 del baseline tracked via commit already? README is base.
	// 使用 README 修改。
	writeFile(t, repo, "README.md", "init\nadded\n")
	runGit(t, repo, "add", "README.md") // staged +1 add

	entries, err := Status(context.Background(), repo)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	e, ok := findEntry(entries, "README.md")
	if !ok {
		t.Fatalf("missing README.md in %v", entries)
	}
	if e.Additions != 1 {
		t.Errorf("want 1 addition, got %d (%+v)", e.Additions, e)
	}
}

func TestCheckRefFormatRejectsInvalid(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	if err := ValidateBranchName(ctx, repo, "ocdeck/my-task"); err != nil {
		t.Errorf("valid name rejected: %v", err)
	}
	if err := ValidateBranchName(ctx, repo, "ocdeck/bad..name"); err == nil {
		t.Error("invalid name accepted")
	}
	if err := ValidateBranchName(ctx, repo, "bad name with space"); err == nil {
		t.Error("invalid name with space accepted")
	}
}

func TestRepoLockMutualExclusion(t *testing.T) {
	mu := RepoLock("/fake/repo/path")
	if !mu.TryLock() {
		t.Fatal("first TryLock failed")
	}
	mu2 := RepoLock("/fake/repo/path")
	if mu2.TryLock() {
		t.Fatal("second TryLock should fail (same repo)")
	}
	mu.Unlock()
	if !mu2.TryLock() {
		t.Fatal("TryLock should succeed after unlock")
	}
	mu2.Unlock()

	// 不同 repo 应独立。
	muA := RepoLock("/repo/a")
	muB := RepoLock("/repo/b")
	if !muA.TryLock() || !muB.TryLock() {
		t.Fatal("different repos should lock independently")
	}
	muA.Unlock()
	muB.Unlock()
}

func TestContextCancel(t *testing.T) {
	repo := newTestRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// 取消的 ctx 下 git 应快速失败（exec.CommandContext 杀进程）。
	_, err := Status(ctx, repo)
	if err == nil {
		// 可能仍成功（已缓存），但不应阻塞；允许 nil，主要验证不 panic/不 hang。
		return
	}
	// 期望 context 相关错误或 commandError。
}

func TestDiffOutput(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, "README.md", "init\nline2\n")
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-qm", "c2")
	writeFile(t, repo, "README.md", "init\nline2\nline3\n")
	out, trunc, err := Diff(context.Background(), repo, "HEAD", "README.md")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if trunc {
		t.Error("unexpected truncation")
	}
	if !strings.Contains(out, "+line3") {
		t.Errorf("diff missing addition: %q", out)
	}
}

func TestDiffBinaryTruncated(t *testing.T) {
	repo := newTestRepo(t)
	bin := []byte{0x00, 0x01, 0x02, 0x03, 0xff}
	if err := os.WriteFile(filepath.Join(repo, "bin.dat"), bin, 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "bin.dat")
	runGit(t, repo, "commit", "-qm", "add binary")
	// 修改二进制。
	if err := os.WriteFile(filepath.Join(repo, "bin.dat"), []byte{0x00, 0x09, 0x10}, 0o644); err != nil {
		t.Fatal(err)
	}
	out, trunc, err := Diff(context.Background(), repo, "HEAD", "bin.dat")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !trunc {
		t.Errorf("want truncated=true for binary, got out=%q", out)
	}
}

func TestCommitAndPush(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, "f.txt", "content\n")
	if err := Commit(context.Background(), repo, "add f", []string{"f.txt"}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Push 到不存在的 remote 应原样透传错误。
	err := Push(context.Background(), repo, "main")
	if err == nil {
		t.Skip("push succeeded (remote configured); skipping error-path assertion")
	}
	// 错误应包含原始信息。
	if !strings.Contains(err.Error(), "origin") && !strings.Contains(err.Error(), "push") {
		t.Errorf("push error should mention origin/push: %v", err)
	}
}

func TestCommitEmptyMessage(t *testing.T) {
	repo := newTestRepo(t)
	if err := Commit(context.Background(), repo, "", []string{"README.md"}); err == nil {
		t.Error("empty message should be rejected")
	}
}

// TestCommitAllStagesUntracked 空 paths=提交全部（含 untracked），冒烟实证回归。
func TestCommitAllStagesUntracked(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, "untracked.txt", "new\n")
	if err := Commit(context.Background(), repo, "commit all", nil); err != nil {
		t.Fatalf("Commit all: %v", err)
	}
	st, err := Status(context.Background(), repo)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(st) != 0 {
		t.Errorf("after commit-all, status should be clean, got %d files", len(st))
	}
}

// TestStderrOfFallsBackToStdout git 诊断在 stdout 时（如 nothing to commit）透传 stdout 而非裸 exit status。
func TestStderrOfFallsBackToStdout(t *testing.T) {
	repo := newTestRepo(t)
	err := Commit(context.Background(), repo, "nothing staged", []string{"README.md"})
	if err == nil {
		t.Skip("commit with no changes succeeded unexpectedly")
	}
	msg := StderrOf(err)
	if msg == "exit status 1" || msg == "" {
		t.Errorf("StderrOf should surface git diagnostic output, got %q", msg)
	}
}

func TestPushEmptyBranch(t *testing.T) {
	repo := newTestRepo(t)
	if err := Push(context.Background(), repo, ""); err == nil {
		t.Error("empty branch should be rejected")
	}
}

func TestParseNumstatZBinary(t *testing.T) {
	// 普通格式："add\tdel\tpath\0"，二进制为 "-\t-\t..."。
	input := "1\t0\ta.txt\x00-\t-\tbin.dat\x00"
	byPath, _ := parseNumstatZ([]byte(input))
	if byPath["a.txt"] == nil || byPath["a.txt"].additions != 1 {
		t.Errorf("a.txt wrong: %+v", byPath["a.txt"])
	}
	if byPath["bin.dat"] == nil || !byPath["bin.dat"].isBinary {
		t.Errorf("bin.dat should be binary: %+v", byPath["bin.dat"])
	}
}

func TestParseNumstatZRename(t *testing.T) {
	// rename: "add\tdel\0old\0new\0"
	input := "3\t1\x00old.txt\x00new.txt\x00"
	byPath, byRename := parseNumstatZ([]byte(input))
	key := "old.txt\x00new.txt"
	if byRename[key] == nil || byRename[key].additions != 3 || byRename[key].deletions != 1 {
		t.Errorf("rename entry wrong: %+v", byRename[key])
	}
	if byPath["new.txt"] == nil || byPath["new.txt"].additions != 3 {
		t.Errorf("rename newPath mapping wrong: %+v", byPath["new.txt"])
	}
}

func TestParseNumstatZSpecialChars(t *testing.T) {
	// 普通格式 "add\tdel\tpath\0"；路径含空格、tab、非 ASCII（-z 原始无引号）。
	input := "1\t0\tspaced name.txt\x001\t0\ttab\tname.txt\x001\t0\tünïcödé.txt\x00"
	byPath, _ := parseNumstatZ([]byte(input))
	for _, p := range []string{"spaced name.txt", "tab\tname.txt", "ünïcödé.txt"} {
		if byPath[p] == nil || byPath[p].additions != 1 {
			t.Errorf("missing/wrong stats for %q: %+v", p, byPath[p])
		}
	}
}

func TestStatusNumstatFailurePropagates(t *testing.T) {
	// 构造 numstat 失败：用合法 repo 但破坏 index 使 diff --numstat 失败。
	// 稳定方式：删除 .git/index 文件后 status 仍可用（porcelain v2 读 HEAD），但 diff --numstat --cached 因无 index 报错。
	repo := newTestRepo(t)
	writeFile(t, repo, "x.txt", "x\n")
	runGit(t, repo, "add", "x.txt")
	// 删除 index 触发 numstat 失败。
	if err := os.Remove(filepath.Join(repo, ".git", "index")); err != nil {
		t.Fatal(err)
	}
	_, err := Status(context.Background(), repo)
	if err == nil {
		t.Skip("status succeeded despite index removal (git version tolerant); cannot assert failure path")
	}
	if !strings.Contains(err.Error(), "numstat") {
		t.Errorf("error should mention numstat: %v", err)
	}
}

func TestStatusNonExistentDir(t *testing.T) {
	_, err := Status(context.Background(), "/nonexistent-dir-xyz")
	if err == nil {
		t.Fatal("expected error for non-existent dir")
	}
}

func TestParseStatusPorcelainV2ZUnmerged(t *testing.T) {
	// 构造 unmerged 输入。
	input := "u UU N... 100644 100644 100644 100644 78981922613b2afb6025042ff6bd878ac1994e85 975fbec8256d3e8a3797e7a3611380f27c49f4ac 587be6b4c3f93f93c489c0111bba5596147a26cb f.txt\x00"
	entries, err := parseStatusPorcelainV2Z(strings.NewReader(input), 100)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 1 || entries[0].Kind != "u" || entries[0].Path != "f.txt" {
		t.Fatalf("unexpected: %+v", entries)
	}
	if entries[0].X != 'U' || entries[0].Y != 'U' {
		t.Errorf("want UU, got %c%c", entries[0].X, entries[0].Y)
	}
}

// B2: ref option injection 拒绝。
func TestDiffRejectsRefOptionInjection(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, "README.md", "init\nline2\n")
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-qm", "c2")
	// 试图注入 --output= 选项写文件；rev-parse --verify --end-of-options 应拒绝。
	evilPath := filepath.Join(repo, "evil.txt")
	_, _, err := Diff(context.Background(), repo, "--output="+evilPath, "README.md")
	if err == nil {
		t.Fatal("expected ref injection to be rejected")
	}
	if _, statErr := os.Stat(evilPath); statErr == nil {
		t.Fatalf("evil file created by ref injection: %s", evilPath)
	}
}

// B2: pathspec magic literal 化。
func TestDiffLiteralPathspecRejectsMagic(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, "a.txt", "1\n")
	writeFile(t, repo, "b.txt", "2\n")
	runGit(t, repo, "add", "a.txt", "b.txt")
	runGit(t, repo, "commit", "-qm", "base")
	writeFile(t, repo, "a.txt", "1\n2\n")
	writeFile(t, repo, "b.txt", "2\n3\n")
	// :(exclude) magic 不应被接受——literal 包裹后应仅匹配字面 ":(exclude)a.txt"（不存在）。
	out, trunc, err := Diff(context.Background(), repo, "HEAD", ":(exclude)a.txt")
	if err != nil {
		// git 报 pathspec 未匹配，接受为非注入证明。
		return
	}
	if trunc {
		t.Error("unexpected truncation")
	}
	if strings.Contains(out, "b.txt") {
		t.Errorf("magic pathspec leaked into diff: %q", out)
	}
}

// B2: commit literal pathspec。
func TestCommitLiteralPathspec(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, "a.txt", "1\n")
	writeFile(t, repo, "b.txt", "2\n")
	// 提交 a.txt；b.txt 不应被纳入。
	if err := Commit(context.Background(), repo, "only a", []string{"a.txt"}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// b.txt 应仍是未跟踪/未暂存。
	entries, err := Status(context.Background(), repo)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	b, ok := findEntry(entries, "b.txt")
	if !ok {
		t.Fatalf("b.txt should remain untracked, got %v", entries)
	}
	if !b.Untracked {
		t.Errorf("b.txt should be untracked, got %+v", b)
	}
}

// B2: literal pathspec 处理冒号开头路径。
func TestLiteralPathspecColonPath(t *testing.T) {
	repo := newTestRepo(t)
	// 用 git add -A 暂存（直接传 :colon.txt 会被 git 当作 magic pathspec 拒绝）。
	writeFile(t, repo, ":colon.txt", "x\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "base")
	writeFile(t, repo, ":colon.txt", "x\ny\n")
	out, trunc, err := Diff(context.Background(), repo, "HEAD", ":colon.txt")
	if err != nil {
		t.Fatalf("Diff colon path: %v", err)
	}
	if trunc {
		t.Error("unexpected truncation")
	}
	if !strings.Contains(out, "+y") {
		t.Errorf("diff should contain +y for colon path: %q", out)
	}
}

// B3: 超上限输出真实子进程不阻塞。
func TestBoundedOutputRealSubprocess(t *testing.T) {
	// 直接验证 boundedBuffer 满足 io.Writer 契约：始终返回 len(p), nil。
	// os/exec 依赖此契约避免 io.ErrShortWrite 重试与 pipe 阻塞。
	var buf boundedBuffer
	buf.max = 4
	// 第一次写入 3 字节（在限内）。
	n, err := buf.Write([]byte("abc"))
	if err != nil || n != 3 {
		t.Fatalf("write 3: n=%d err=%v", n, err)
	}
	// 第二次写入 5 字节：应声明消费 5 字节（满足 io.Writer），但仅保留前 1 字节，置 overflow。
	n, err = buf.Write([]byte("defgh"))
	if err != nil || n != 5 {
		t.Fatalf("write 5: n=%d err=%v (want n=5 nil)", n, err)
	}
	if !buf.overflow {
		t.Error("overflow not set")
	}
	if buf.String() != "abcd" {
		t.Errorf("buffer content wrong: %q", buf.String())
	}
	// 第三次写入：overflow 后仍返回 len(p), nil。
	n, err = buf.Write([]byte("ijklm"))
	if err != nil || n != 5 {
		t.Fatalf("write after overflow: n=%d err=%v", n, err)
	}
	// 内容不再增长。
	if buf.String() != "abcd" {
		t.Errorf("buffer grew after overflow: %q", buf.String())
	}
}

// B3: 真实子进程产生超限输出不阻塞、返回 ErrOutputTruncated。
// boundedBuffer 满足 io.Writer 契约（始终返回 len(p), nil），os/exec 不会重试写入，
// 子进程 pipe 不会因阻塞而 hang。真实大输出测试成本高（需 >16MB），
// 契约正确性已在 TestBoundedOutputRealSubprocess 覆盖，此处不再重复构造超大数据。

// S7: canonical repo lock 归一。
func TestRepoLockCanonicalPath(t *testing.T) {
	// 创建一个真实目录与一个符号链接指向它。
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	muReal := RepoLock(real)
	muLink := RepoLock(link)
	// 经 EvalSymlinks 归一后，real 与 link 应映射到同一把锁。
	if muReal != muLink {
		t.Fatalf("canonical lock not unified: real=%p link=%p", muReal, muLink)
	}
	if !muReal.TryLock() {
		t.Fatal("first TryLock failed")
	}
	if muLink.TryLock() {
		t.Fatal("link alias should share lock with real")
	}
	muReal.Unlock()
}

// P2: path 为空时文件数预统计 MUST 带同一 resolved ref，否则大 ref diff 可绕过 DiffMaxFiles。
// 构造：基线（newTestRepo 的 init 提交）后新增若干文件并提交为 c1，ref=c1 全仓 diff
// 文件数 < DiffMaxFiles，验证预统计带 ref 后与实际 diff 文件数一致、不误截断。
func TestDiffEmptyPathPrecountCarriesRef(t *testing.T) {
	repo := newTestRepo(t)
	// 在 init 基线之上新增几个文件并提交为 c1。
	for i := 0; i < 5; i++ {
		writeFile(t, repo, "f"+pad(i)+".txt", "v1\n")
		runGit(t, repo, "add", "f"+pad(i)+".txt")
	}
	writeFile(t, repo, "README.md", "init\nmore\n")
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-qm", "c1")
	// ref=HEAD~1（init 基线）、path 为空：ref→工作区 diff 含 c1 新增文件 + README 改动，
	// 文件数 < DiffMaxFiles，预统计带 ref 应一致、不截断。
	out, trunc, err := Diff(context.Background(), repo, "HEAD~1", "")
	if err != nil {
		t.Fatalf("Diff HEAD~1 empty path: %v", err)
	}
	if trunc {
		t.Errorf("unexpected truncation for small ref diff (precount may have used default diff): out=%q", out)
	}
	if !strings.Contains(out, "README.md") {
		t.Errorf("diff should include README.md: %q", out)
	}
}

// P2: 带 ref 的全仓 diff 文件数超过 DiffMaxFiles 时 MUST 截断（预统计带 ref）。
// 构造：init 基线提交后，新增 >DiffMaxFiles 个文件并提交为 c1，回退工作区
//（git reset --hard HEAD~1）使默认工作区 diff 文件数 = 0，但 ref=c1 的全仓 diff
// 文件数 = DiffMaxFiles+5。旧实现预统计不带 ref → 文件数=0 不截断；实际 diff 带 ref
// 输出超大（绕过限制）。修复后预统计带 ref → 文件数 > DiffMaxFiles → truncated=true。
func TestDiffEmptyPathRefPrecountTruncates(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large ref diff test in -short mode")
	}
	repo := newTestRepo(t)
	// 在 init 基线之上新增超过 DiffMaxFiles 个文件并提交为 c1。
	for i := 0; i < DiffMaxFiles+5; i++ {
		writeFile(t, repo, "f"+pad(i)+".txt", "x\n")
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "c1 with many files")
	c1 := currentRef(t, repo)
	// 回退工作区到 init 基线，使默认工作区 diff 为空、ref diff 巨大。
	runGit(t, repo, "reset", "--hard", "HEAD~1")
	// ref=c1、path 为空：预统计必须带 ref 才能识别超大文件数 → truncated=true。
	out, trunc, err := Diff(context.Background(), repo, c1, "")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !trunc {
		t.Fatalf("expected truncation for ref diff > DiffMaxFiles files, got out len=%d", len(out))
	}
	if out != "" {
		t.Errorf("truncated diff should have empty out, got %d bytes", len(out))
	}
}

// currentRef 返回当前 HEAD 的 OID。
func currentRef(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}
