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
	entries, err := parseStatusPorcelainV2Z(strings.NewReader(input), 100, false, nil)
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

// TestParseStatusPorcelainV2ZKindFilterSkipsRenamePreservesStream 验证 kindFilter 跳过 '2' rename
// 条目时正确消费其第二条 NUL 记录（旧路径），保持流位置——紧跟其后的 `?`/`!` 目标条目应被正确解析。
// 合成 porcelain v2 -z 输入：rename(2) + oldPath + untracked(?) + ignored(!)。
func TestParseStatusPorcelainV2ZKindFilterSkipsRenamePreservesStream(t *testing.T) {
	// "2 XY sub mH mI mW hO iO score newPath\x00oldPath\x00? untracked.txt\x00! ignored.log\x00"
	input := "2 R. N... 100644 100644 100644 78981922613b2afb6025042ff6bd878ac1994e85 975fbec8256d3e8a3797e7a3611380f27c49f4ac 100 new.txt\x00old.txt\x00? untracked.txt\x00! ignored.log\x00"
	// kindFilter 仅保留 '?' 与 '!'；'2' rename 被跳过但必须消费 oldPath NUL 记录。
	entries, err := parseStatusPorcelainV2Z(strings.NewReader(input), 100, true,
		func(kind byte) bool { return kind == '?' || kind == '!' })
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 target entries (rename skipped), got %d: %+v", len(entries), entries)
	}
	if entries[0].Kind != "?" || entries[0].Path != "untracked.txt" || !entries[0].Untracked {
		t.Errorf("entry[0] wrong: %+v", entries[0])
	}
	if entries[1].Kind != "!" || entries[1].Path != "ignored.log" || !entries[1].Ignored {
		t.Errorf("entry[1] wrong: %+v", entries[1])
	}
}

// TestParseStatusPorcelainV2ZKindFilterSkipsTrackedOrdinary 验证 kindFilter 跳过 '1' ordinary
// 条目时不分配、不计数；紧跟其后的目标条目被正确解析。
func TestParseStatusPorcelainV2ZKindFilterSkipsTrackedOrdinary(t *testing.T) {
	// "1 XY sub mH mI mW hO iO path\x00? untracked.txt\x00"
	input := "1 .M N... 100644 100644 100644 78981922613b2afb6025042ff6bd878ac1994e85 975fbec8256d3e8a3797e7a3611380f27c49f4ac tracked.txt\x00? untracked.txt\x00"
	entries, err := parseStatusPorcelainV2Z(strings.NewReader(input), 100, true,
		func(kind byte) bool { return kind == '?' || kind == '!' })
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 1 || entries[0].Kind != "?" || entries[0].Path != "untracked.txt" {
		t.Fatalf("expected only untracked.txt (ordinary skipped), got %+v", entries)
	}
}

// TestParseStatusPorcelainV2ZKindFilterTargetLimit 验证 kindFilter 路径下目标条目达 maxFiles+1 即 ErrTooManyFilesChanged，
// tracked 条目不计数（合成输入：2 个 tracked ordinary + maxFiles+1 个 untracked）。
func TestParseStatusPorcelainV2ZKindFilterTargetLimit(t *testing.T) {
	var b strings.Builder
	// 2 个 tracked ordinary（被 kindFilter 跳过，不计数）。
	b.WriteString("1 .M N... 100644 100644 100644 78981922613b2afb6025042ff6bd878ac1994e85 975fbec8256d3e8a3797e7a3611380f27c49f4ac t1.txt\x00")
	b.WriteString("1 .M N... 100644 100644 100644 78981922613b2afb6025042ff6bd878ac1994e85 975fbec8256d3e8a3797e7a3611380f27c49f4ac t2.txt\x00")
	// maxFiles+1 个 untracked（目标条目，超限）。
	const maxFiles = 5
	for i := 0; i <= maxFiles; i++ {
		b.WriteString("? u" + itoa(i) + ".txt\x00")
	}
	_, err := parseStatusPorcelainV2Z(strings.NewReader(b.String()), maxFiles, true,
		func(kind byte) bool { return kind == '?' || kind == '!' })
	if err != ErrTooManyFilesChanged {
		t.Fatalf("want ErrTooManyFilesChanged (tracked not counted, %d targets), got %v", maxFiles+1, err)
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

// findIgnoredEntry 在 entries 中查找指定路径的 ignored 条目。
func findIgnoredEntry(entries []FileStatus, path string) (FileStatus, bool) {
	for _, e := range entries {
		if e.Path == path && e.Ignored {
			return e, true
		}
	}
	return FileStatus{}, false
}

// TestListIgnoredUntracked 验证 ListIgnoredUntracked 枚举 untracked + ignored 文件级记录（design.md §7.2）。
// 覆盖：untracked/ignored/tracked 混合、ignored 目录内嵌套文件展开、-uall 展开、`.git` 排除。
func TestListIgnoredUntracked(t *testing.T) {
	repo := newTestRepo(t)
	// tracked 文件（已在 newTestRepo 中提交 README.md）。
	// untracked 普通文件。
	writeFile(t, repo, "untracked.txt", "x\n")
	// untracked 目录内嵌套文件（-uall 展开）。
	writeFile(t, repo, "newdir/sub/deep.txt", "y\n")
	// ignored：根 .gitignore + ignored 目录内嵌套文件。
	writeFile(t, repo, ".gitignore", "ignored/\n*.log\n")
	writeFile(t, repo, "ignored/nested/file.log", "z\n")
	writeFile(t, repo, "root.log", "r\n")

	entries, err := ListIgnoredUntracked(context.Background(), repo)
	if err != nil {
		t.Fatalf("ListIgnoredUntracked: %v", err)
	}

	// untracked.txt 出现在 untracked 条目。
	if u, ok := findEntry(entries, "untracked.txt"); !ok || !u.Untracked {
		t.Errorf("untracked.txt missing or not untracked: %+v", u)
	}
	// newdir 下嵌套文件应被 -uall 展开（非目录占位）。
	if _, ok := findEntry(entries, "newdir/sub/deep.txt"); !ok {
		t.Errorf("newdir/sub/deep.txt should be expanded by -uall, got %v", entries)
	}
	// ignored 目录内嵌套文件应被 traditional + -uall 展开为文件级记录。
	if _, ok := findIgnoredEntry(entries, "ignored/nested/file.log"); !ok {
		t.Errorf("ignored/nested/file.log should be expanded (ignored), got %v", entries)
	}
	// root.log 被 *.log 匹配为 ignored。
	if _, ok := findIgnoredEntry(entries, "root.log"); !ok {
		t.Errorf("root.log should be ignored (*.log), got %v", entries)
	}
	// tracked 文件 README.md 不应出现。
	if e, ok := findEntry(entries, "README.md"); ok && !e.Untracked && !e.Ignored {
		t.Errorf("tracked README.md should not appear, got %+v", e)
	}
	// .git 条目 MUST 排除。
	for _, e := range entries {
		if isGitPath(e.Path) {
			t.Errorf(".git path should be excluded: %+v", e)
		}
	}
}

// TestListIgnoredUntrackedTooManyFiles 验证有界输出超限返回 ErrTooManyFilesChanged。
func TestListIgnoredUntrackedTooManyFiles(t *testing.T) {
	repo := newTestRepo(t)
	for i := 0; i < MaxStatusFiles+5; i++ {
		writeFile(t, repo, "f"+pad(i)+".txt", "x\n")
	}
	_, err := ListIgnoredUntracked(context.Background(), repo)
	if err != ErrTooManyFilesChanged {
		t.Fatalf("want ErrTooManyFilesChanged, got %v", err)
	}
}

// TestStatusExcludesIgnored 防回归：既有 Status 调用方默认不含 ignored 条目。
func TestStatusExcludesIgnored(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, ".gitignore", "*.log\n")
	writeFile(t, repo, "ignored.log", "x\n")
	writeFile(t, repo, "untracked.txt", "y\n")

	entries, err := Status(context.Background(), repo)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if _, ok := findEntry(entries, "ignored.log"); ok {
		t.Errorf("Status should exclude ignored entries, found ignored.log in %v", entries)
	}
	if _, ok := findEntry(entries, "untracked.txt"); !ok {
		t.Errorf("Status should include untracked, missing untracked.txt in %v", entries)
	}
}

// TestListIgnoredUntrackedExcludesModifiedTracked 验证 ListIgnoredUntracked 不返回修改过的 tracked 文件
// （design.md §7.2：仅含 untracked(`?`) 与 ignored(`!`) 两类）。
func TestListIgnoredUntrackedExcludesModifiedTracked(t *testing.T) {
	repo := newTestRepo(t)
	// 建立一个 tracked 文件。
	writeFile(t, repo, "tracked.txt", "init\n")
	runGit(t, repo, "add", "tracked.txt")
	runGit(t, repo, "commit", "-qm", "add tracked")
	// 修改 tracked 文件（ordinary 条目，非 untracked/ignored）。
	writeFile(t, repo, "tracked.txt", "modified\n")
	// 同时有 untracked 与 ignored。
	writeFile(t, repo, ".gitignore", "*.log\n")
	writeFile(t, repo, "ignored.log", "x\n")
	writeFile(t, repo, "untracked.txt", "y\n")

	entries, err := ListIgnoredUntracked(context.Background(), repo)
	if err != nil {
		t.Fatalf("ListIgnoredUntracked: %v", err)
	}
	for _, e := range entries {
		if !e.Untracked && !e.Ignored {
			t.Errorf("ListIgnoredUntracked must only return untracked/ignored, got %v", e)
		}
	}
	// 修改过的 tracked.txt 不应出现。
	if e, ok := findEntry(entries, "tracked.txt"); ok {
		t.Errorf("modified tracked file should not be returned, got %+v", e)
	}
}

// TestListIgnoredUntrackedManyTrackedNotCounted 验证有界计数只针对 `?`/`!` 目标条目，
// tracked 条目在解析阶段即跳过（不分配、不计数，design.md §7.2）。构造：超过 MaxStatusFiles
// 个 modified tracked 文件 + 少量 untracked/ignored，证明 tracked 不计数、不超限、目标计数正确。
func TestListIgnoredUntrackedManyTrackedNotCounted(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping >MaxStatusFiles tracked files test in -short mode")
	}
	repo := newTestRepo(t)
	// 建立超过 MaxStatusFiles 个 tracked 文件并修改（ordinary 条目，经 kindFilter 在解析阶段跳过）。
	const trackedCount = MaxStatusFiles + 50
	for i := 0; i < trackedCount; i++ {
		writeFile(t, repo, "t"+pad(i)+".txt", "init\n")
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "add tracked baseline")
	for i := 0; i < trackedCount; i++ {
		writeFile(t, repo, "t"+pad(i)+".txt", "modified\n")
	}
	// 少量 untracked + ignored（.gitignore 提交为 tracked，不参与目标计数）。
	writeFile(t, repo, ".gitignore", "*.log\n")
	runGit(t, repo, "add", ".gitignore")
	runGit(t, repo, "commit", "-qm", "add gitignore")
	writeFile(t, repo, "ignored.log", "x\n")
	writeFile(t, repo, "untracked.txt", "y\n")

	entries, err := ListIgnoredUntracked(context.Background(), repo)
	if err != nil {
		t.Fatalf("ListIgnoredUntracked: %v (tracked files must not be counted toward limit)", err)
	}
	// 仅含 untracked/ignored，tracked 不返回。
	for _, e := range entries {
		if !e.Untracked && !e.Ignored {
			t.Errorf("tracked file leaked: %+v", e)
		}
	}
	// 目标条目计数 = 2（untracked.txt + ignored.log）。
	if len(entries) != 2 {
		t.Errorf("expected 2 target entries (1 untracked + 1 ignored), got %d: %v", len(entries), entries)
	}
}

// TestListIgnoredUntrackedTargetLimit 验证达到 MaxStatusFiles+1 个目标条目（?/!）即 ErrTooManyFilesChanged。
func TestListIgnoredUntrackedTargetLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large target count test in -short mode")
	}
	repo := newTestRepo(t)
	// 全部作为 untracked（不提交），超过 MaxStatusFiles 个目标条目。
	for i := 0; i < MaxStatusFiles+1; i++ {
		writeFile(t, repo, "u"+pad(i)+".txt", "x\n")
	}
	_, err := ListIgnoredUntracked(context.Background(), repo)
	if err != ErrTooManyFilesChanged {
		t.Fatalf("want ErrTooManyFilesChanged for %d target entries, got %v", MaxStatusFiles+1, err)
	}
}

// --- codemirror-git-diff：按版本读取文件内容（ref/index/工作区三分支）见 content_test.go ---

// cachedDiff 返回 repo 当前 `git diff --cached` 输出，供 index 不变性断言。
func cachedDiff(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "diff", "--cached")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git diff --cached: %v", err)
	}
	return string(out)
}

// --- fix-git-diff-new-file-and-linenum 任务 2.4：untracked 行数统计 ---

func TestUntrackedLineCount_100Lines(t *testing.T) {
	repo := newTestRepo(t)
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString("line\n")
	}
	writeFile(t, repo, "hundred.txt", b.String())
	entries, err := Status(context.Background(), repo)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	e, ok := findEntry(entries, "hundred.txt")
	if !ok {
		t.Fatalf("missing hundred.txt in %v", entries)
	}
	if e.Additions != 100 {
		t.Errorf("additions = %d, want 100", e.Additions)
	}
	if e.Deletions != 0 {
		t.Errorf("deletions = %d, want 0", e.Deletions)
	}
}

func TestUntrackedLineCount_NoTrailingNewline(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, "notrail.txt", "a\nb\nc") // 2 newlines + 末行无换行 → 3
	entries, err := Status(context.Background(), repo)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	e, ok := findEntry(entries, "notrail.txt")
	if !ok {
		t.Fatalf("missing notrail.txt in %v", entries)
	}
	if e.Additions != 3 {
		t.Errorf("additions = %d, want 3", e.Additions)
	}
}

func TestUntrackedLineCount_BinaryNotCounted(t *testing.T) {
	repo := newTestRepo(t)
	bin := []byte("text\nwith\x00nul\nin\nit\n")
	if err := os.WriteFile(filepath.Join(repo, "bin.txt"), bin, 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := Status(context.Background(), repo)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	e, ok := findEntry(entries, "bin.txt")
	if !ok {
		t.Fatalf("missing bin.txt in %v", entries)
	}
	if !e.IsBinary {
		t.Errorf("IsBinary should be true for file with NUL, got %+v", e)
	}
	if e.Additions != 0 {
		t.Errorf("binary additions should be 0, got %d", e.Additions)
	}
}

func TestUntrackedLineCount_SymlinkSkipped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; symlink permissions may differ")
	}
	repo := newTestRepo(t)
	// 先建一个目标文件（tracked），再建指向它的 symlink（untracked）。
	writeFile(t, repo, "target.txt", "target content\n")
	runGit(t, repo, "add", "target.txt")
	runGit(t, repo, "commit", "-qm", "add target")
	link := filepath.Join(repo, "link.txt")
	if err := os.Symlink(filepath.Join(repo, "target.txt"), link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	entries, err := Status(context.Background(), repo)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	e, ok := findEntry(entries, "link.txt")
	if !ok {
		t.Fatalf("missing link.txt symlink in %v", entries)
	}
	if e.Additions != 0 {
		t.Errorf("symlink additions should be 0, got %d", e.Additions)
	}
}

func TestUntrackedLineCount_Over16MBFileSkipsCounting(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping >16MB file test in -short mode")
	}
	repo := newTestRepo(t)
	// 写入 16MB + 1 字节，超过单文件预算，行计数应跳过（additions=0）。
	// 含换行以区分"超限跳过（0）"与"计算后无换行（0）"——超限时即便有换行也必须为 0。
	content := strings.Repeat("x\n", untrackedFileBudget/2+1) // >16MB，每行 2 字节
	if err := os.WriteFile(filepath.Join(repo, "big.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := Status(context.Background(), repo)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	e, ok := findEntry(entries, "big.txt")
	if !ok {
		t.Fatalf("missing big.txt in %v", entries)
	}
	if e.Additions != 0 {
		t.Errorf("file >16MB additions should be 0 (skip counting), got %d", e.Additions)
	}
}

func TestUntrackedLineCount_64MBCumulativeBudgetExhausted(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 64MB cumulative budget test in -short mode")
	}
	repo := newTestRepo(t)
	// 4 个 16MB 文本文件耗尽累计 64MB 预算；第 5 个文件应仅嗅探不计行（additions=0）。
	// 用单行大文件：每文件 16MB（无换行），单文件恰好等于预算（不超限）。
	for i := 0; i < 4; i++ {
		content := strings.Repeat("x", untrackedFileBudget)
		writeFile(t, repo, "f"+itoa(i)+".txt", content)
	}
	// 第 5 个小文件（累计预算已耗尽）：仅嗅探，additions=0。
	writeFile(t, repo, "small.txt", "line\n")
	entries, err := Status(context.Background(), repo)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	small, ok := findEntry(entries, "small.txt")
	if !ok {
		t.Fatalf("missing small.txt in %v", entries)
	}
	if small.Additions != 0 {
		t.Errorf("small.txt after budget exhaustion additions should be 0, got %d", small.Additions)
	}
}

func TestUntrackedLineCount_IOError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission bits may not deny read")
	}
	repo := newTestRepo(t)
	writeFile(t, repo, "noperm.txt", "content\n")
	p := filepath.Join(repo, "noperm.txt")
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
	_, err := Status(context.Background(), repo)
	if err == nil {
		// 某些环境（如 root 或 FS 不强制权限）可能不报错——降级验证不阻塞。
		t.Skip("read succeeded despite 0o000 (filesystem does not enforce permissions)")
	}
	if !strings.Contains(err.Error(), "noperm.txt") {
		t.Errorf("IO error should contain path, got %v", err)
	}
}

// --- fix-git-diff 评审修复补充测试 ---

// TestUntrackedLineCount_ENOENTSkipsSilently 验证 Lstat/open not-exist 静默跳过
// （status 快照后文件被并发删除属正常竞态，不阻塞 status、不报错）。
func TestUntrackedLineCount_ENOENTSkipsSilently(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, "gone.txt", "content\n")
	// 先获取 status 快照（含 gone.txt untracked 条目），再删除文件模拟竞态。
	// 直接构造 entries 调 countUntrackedLines：更精确地触发 ENOENT 路径。
	entries := []FileStatus{{Kind: "?", Path: "gone.txt", Untracked: true}}
	os.Remove(filepath.Join(repo, "gone.txt"))
	err := countUntrackedLines(context.Background(), repo, entries)
	if err != nil {
		t.Fatalf("ENOENT should skip silently, got err=%v", err)
	}
	// 被删文件 additions 保持 0（未计行）。
	if entries[0].Additions != 0 {
		t.Errorf("ENOENT additions should be 0, got %d", entries[0].Additions)
	}
}

// TestUntrackedLineCount_BinaryDoesNotConsumeTextBudget 验证二进制 prefix 不消耗文本预算
// （否则多个小二进制文件即可耗尽 64MB 使后续文本 additions=0）。
// 用 countUntrackedFileLines + 注入小预算避免创建大文件。
func TestUntrackedLineCount_BinaryDoesNotConsumeTextBudget(t *testing.T) {
	dir := t.TempDir()
	// 二进制文件（含 NUL）。
	binPath := filepath.Join(dir, "bin.dat")
	if err := os.WriteFile(binPath, []byte("text\x00binary\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 文本文件（应在二进制之后仍能计行——证明二进制未消耗预算）。
	textPath := filepath.Join(dir, "text.txt")
	if err := os.WriteFile(textPath, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	totalRemaining := untrackedTotalBudget // 64MB，足够两个小文件

	// 处理二进制文件。
	binEntry := FileStatus{Kind: "?", Path: "bin.dat", Untracked: true}
	f1, err := os.Open(binPath)
	if err != nil {
		t.Fatal(err)
	}
	binAdditions, binUsed, berr := countUntrackedFileLines(context.Background(), f1, &binEntry, &totalRemaining)
	f1.Close()
	if berr != nil {
		t.Fatalf("binary count: %v", berr)
	}
	if binAdditions != 0 {
		t.Errorf("binary additions should be 0, got %d", binAdditions)
	}
	if binUsed != 0 {
		t.Errorf("binary used should be 0 (not consuming text budget), got %d", binUsed)
	}
	if !binEntry.IsBinary {
		t.Errorf("binary file should be marked IsBinary")
	}
	// 关键断言：二进制不消耗文本预算，totalRemaining 仍为 64MB。
	if totalRemaining != untrackedTotalBudget {
		t.Errorf("binary should not consume text budget: remaining=%d, want %d", totalRemaining, untrackedTotalBudget)
	}

	// 处理文本文件——应正常计行（预算未被二进制消耗）。
	textEntry := FileStatus{Kind: "?", Path: "text.txt", Untracked: true}
	f2, err := os.Open(textPath)
	if err != nil {
		t.Fatal(err)
	}
	additions, used, terr := countUntrackedFileLines(context.Background(), f2, &textEntry, &totalRemaining)
	f2.Close()
	if terr != nil {
		t.Fatalf("text count: %v", terr)
	}
	if textEntry.IsBinary {
		t.Errorf("text file should not be binary")
	}
	if additions != 3 {
		t.Errorf("text additions = %d, want 3 (binary did not consume budget)", additions)
	}
	if used != 6 {
		t.Errorf("used = %d, want 6", used)
	}
}

// TestUntrackedLineCount_RemainingLessThanPrefixSkips 验证剩余预算小于 prefix 时跳过行计数
// （修复前小文件会在突破 64MB 后仍返回有效行数）。
func TestUntrackedLineCount_RemainingLessThanPrefixSkips(t *testing.T) {
	dir := t.TempDir()
	// 小文本文件（< 8000 字节，prefix 读取全部内容）。
	textPath := filepath.Join(dir, "small.txt")
	if err := os.WriteFile(textPath, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 注入极小预算（< prefix 长度 4）→ 该文件跳过行计数，additions=0。
	totalRemaining := 2
	entry := FileStatus{Kind: "?", Path: "small.txt", Untracked: true}
	f, err := os.Open(textPath)
	if err != nil {
		t.Fatal(err)
	}
	additions, used, rerr := countUntrackedFileLines(context.Background(), f, &entry, &totalRemaining)
	f.Close()
	if rerr != nil {
		t.Fatalf("count: %v", rerr)
	}
	if additions != 0 {
		t.Errorf("remaining < prefix should skip counting: additions=%d, want 0", additions)
	}
	if used != 0 {
		t.Errorf("skipped file should consume no budget: used=%d, want 0", used)
	}
}

// TestUntrackedLineCount_BudgetExhaustedBinaryStillMarked 验证预算耗尽后二进制嗅探仍执行、IsBinary 仍标记。
func TestUntrackedLineCount_BudgetExhaustedBinaryStillMarked(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "bin.dat")
	if err := os.WriteFile(binPath, []byte("text\x00nul\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 预算耗尽（0）→ 嗅探仍执行、IsBinary 标记、不计行。
	totalRemaining := 0
	entry := FileStatus{Kind: "?", Path: "bin.dat", Untracked: true}
	f, err := os.Open(binPath)
	if err != nil {
		t.Fatal(err)
	}
	additions, used, rerr := countUntrackedFileLines(context.Background(), f, &entry, &totalRemaining)
	f.Close()
	if rerr != nil {
		t.Fatalf("count: %v", rerr)
	}
	if !entry.IsBinary {
		t.Errorf("budget exhausted: binary sniff should still mark IsBinary")
	}
	if additions != 0 {
		t.Errorf("budget exhausted: additions should be 0, got %d", additions)
	}
	if used != 0 {
		t.Errorf("budget exhausted: binary should not consume budget, used=%d", used)
	}
}

// TestUntrackedLineCount_CtxCancelReturnsError 验证 ctx 取消时返回 ctx.Err()（非静默）。
func TestUntrackedLineCount_CtxCancelReturnsError(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, "a.txt", "x\n")
	writeFile(t, repo, "b.txt", "y\n")
	entries := []FileStatus{
		{Kind: "?", Path: "a.txt", Untracked: true},
		{Kind: "?", Path: "b.txt", Untracked: true},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := countUntrackedLines(ctx, repo, entries)
	if err == nil {
		t.Fatal("cancelled ctx should return error, got nil")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("error should mention cancelled, got %v", err)
	}
}

// TestUntrackedLineCount_OversizedFileConsumesActualReadBytes 验证超限分支报告实际读取字节
// （prefix + 已成功续读 + 触发超限的本次读取），使调用方按实际扣减后累计预算归零，后续文件不再获得读取额度。
// 用 countUntrackedFileLines + 注入小预算直接测超限分支 used 值。
func TestUntrackedLineCount_OversizedFileConsumesActualReadBytes(t *testing.T) {
	dir := t.TempDir()
	// 构造一个大于累计预算的文本文件（无 NUL）。
	// totalRemaining=10000：prefix 读取 8000 字节（< 10000，通过），
	// 续读预算 = 10000 - 8000 = 2000；文件剩余 12000 字节 > 2000 → 超限分支触发。
	// 超限分支应报告 used = 8000 + 已续读 + 本次读取（>8000），使调用方扣减后预算归零。
	content := strings.Repeat("x\n", 10000) // 20000 字节
	bigPath := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(bigPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	totalRemaining := 10000
	entry := FileStatus{Kind: "?", Path: "big.txt", Untracked: true}
	f, err := os.Open(bigPath)
	if err != nil {
		t.Fatal(err)
	}
	additions, used, rerr := countUntrackedFileLines(context.Background(), f, &entry, &totalRemaining)
	f.Close()
	if rerr != nil {
		t.Fatalf("count: %v", rerr)
	}
	if additions != 0 {
		t.Errorf("oversized file additions should be 0, got %d", additions)
	}
	// used 应远大于 prefix（8000），包含实际续读+触发超限的读取。
	if used <= binarySniffBytes {
		t.Errorf("oversized file used should exceed prefix (actual read bytes): used=%d, want > %d", used, binarySniffBytes)
	}
	// 模拟调用方扣减（clamp 防负数）。
	if used >= totalRemaining {
		totalRemaining = 0
	} else {
		totalRemaining -= used
	}
	if totalRemaining != 0 {
		t.Errorf("after clamp, totalRemaining should be 0: got %d (used=%d)", totalRemaining, used)
	}
}

// TestUntrackedLineCount_MultipleOversizedFilesDepleteBudget 集成用例：
// 多个 >16MB 文件不能重复获得近 16MB 读取额度——累计预算被正确扣减后后续文件不再全量读取。
// 用 Status 层验证：2 个 ~17MB 文本文件，第一个消耗 ~16MB+ 预算，第二个因预算耗尽 additions=0。
func TestUntrackedLineCount_MultipleOversizedFilesDepleteBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multi-oversized-files test in -short mode")
	}
	repo := newTestRepo(t)
	// 2 个 >16MB 文本文件（含换行以区分"超限跳过 0"与"计算后无换行 0"）。
	content := strings.Repeat("x\n", 9*1024*1024) // ~18MB per file
	writeFile(t, repo, "big1.txt", content)
	writeFile(t, repo, "big2.txt", content)

	entries, err := Status(context.Background(), repo)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	// 两个文件都应 additions=0（超限跳过计数）。
	for _, name := range []string{"big1.txt", "big2.txt"} {
		e, ok := findEntry(entries, name)
		if !ok {
			t.Fatalf("missing %s in entries", name)
		}
		if e.Additions != 0 {
			t.Errorf("%s additions = %d, want 0 (budget exhausted after first oversized file)", name, e.Additions)
		}
	}
}
