package git

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// newUnbornTestRepo 在 t.TempDir() 创建仅 init、零提交的 git 仓库（unborn HEAD fixture）。
func newUnbornTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.email", "t@t.com")
	runGit(t, dir, "config", "user.name", "tester")
	return dir
}

// TestMergeBaseCommonAncestor 验证 exit 0 分支：有共同祖先时返回祖先 OID（与 CLI 输出一致）。
func TestMergeBaseCommonAncestor(t *testing.T) {
	repo := newTestRepo(t)
	baseOID, err := runCapture(t, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	// 两侧分叉：feature 提交后回 main 提交，共同祖先为初始提交。
	runGit(t, repo, "checkout", "-qb", "feature")
	writeFile(t, repo, "feature.txt", "f\n")
	runGit(t, repo, "add", "feature.txt")
	runGit(t, repo, "commit", "-qm", "feature")
	runGit(t, repo, "checkout", "-q", "main")
	writeFile(t, repo, "main.txt", "m\n")
	runGit(t, repo, "add", "main.txt")
	runGit(t, repo, "commit", "-qm", "main")
	featureOID, err := runCapture(t, repo, "rev-parse", "feature")
	if err != nil {
		t.Fatalf("rev-parse feature: %v", err)
	}

	mb, err := MergeBase(context.Background(), repo, baseOID, featureOID)
	if err != nil {
		t.Fatalf("MergeBase: %v", err)
	}
	if mb != baseOID {
		t.Errorf("want merge-base = initial commit %s, got %s", baseOID, mb)
	}
	// 有效性：与 git CLI 实际输出一致。
	want, err := runCapture(t, repo, "merge-base", baseOID, featureOID)
	if err != nil || mb != want {
		t.Errorf("want CLI merge-base %q (err %v), got %q", want, err, mb)
	}
}

// TestMergeBaseNoCommonAncestor 验证 exit 1 分支：无共同祖先产生 typed ErrNoMergeBase
// （fixture 实测：--orphan 分支两侧历史独立，git merge-base exit 1）。
func TestMergeBaseNoCommonAncestor(t *testing.T) {
	repo := newTestRepo(t)
	mainOID, err := runCapture(t, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	runGit(t, repo, "switch", "-q", "--orphan", "other")
	writeFile(t, repo, "other.txt", "o\n")
	runGit(t, repo, "add", "other.txt")
	runGit(t, repo, "commit", "-qm", "other")
	otherOID, err := runCapture(t, repo, "rev-parse", "other")
	if err != nil {
		t.Fatalf("rev-parse other: %v", err)
	}

	mb, err := MergeBase(context.Background(), repo, mainOID, otherOID)
	if !errors.Is(err, ErrNoMergeBase) {
		t.Fatalf("want ErrNoMergeBase, got mb=%q err=%v", mb, err)
	}
	// 有效性反证：同一函数对有共同祖先的输入不产生 sentinel。
	if _, err := MergeBase(context.Background(), repo, mainOID, mainOID); err != nil {
		t.Errorf("MergeBase(oid, oid) should succeed, got %v", err)
	}
}

// TestMergeBaseInvalidOIDPassthrough 验证 exit 128 分支：OID 不存在透传 stderr，
// MUST NOT 误判为 ErrNoMergeBase（fixture 实测：git merge-base 对无效 OID exit 128）。
func TestMergeBaseInvalidOIDPassthrough(t *testing.T) {
	repo := newTestRepo(t)
	headOID, err := runCapture(t, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	_, err = MergeBase(context.Background(), repo, headOID, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	if err == nil {
		t.Fatal("want error for invalid OID, got nil")
	}
	if errors.Is(err, ErrNoMergeBase) {
		t.Fatalf("exit 128 MUST NOT map to ErrNoMergeBase, got %v", err)
	}
	// 透传证据：错误信息携带 git 原始诊断（含无效 OID 文本）。
	if stderr := StderrOf(err); !strings.Contains(stderr, "deadbeef") {
		t.Errorf("want raw git stderr passthrough, got %q", stderr)
	}
}

// TestResolveHeadOID 验证 exit 0 分支：有提交仓库返回 HEAD OID（与 rev-parse 一致）。
func TestResolveHeadOID(t *testing.T) {
	repo := newTestRepo(t)
	oid, err := ResolveHeadOID(context.Background(), repo)
	if err != nil {
		t.Fatalf("ResolveHeadOID: %v", err)
	}
	want, err := runCapture(t, repo, "rev-parse", "HEAD")
	if err != nil || oid != want {
		t.Errorf("want HEAD OID %q (err %v), got %q", want, err, oid)
	}
}

// TestResolveHeadOIDUnborn 验证 exit 1 分支：仓库有效但零提交 → ErrUnbornHead
// （/tmp 实测：`rev-parse --verify --quiet --end-of-options HEAD` 对 unborn HEAD exit 1 无输出）。
func TestResolveHeadOIDUnborn(t *testing.T) {
	repo := newUnbornTestRepo(t)
	oid, err := ResolveHeadOID(context.Background(), repo)
	if !errors.Is(err, ErrUnbornHead) {
		t.Fatalf("want ErrUnbornHead, got oid=%q err=%v", oid, err)
	}
	// 有效性反证：同一函数对有提交仓库不产生 sentinel（TestResolveHeadOID 覆盖）。
}

// TestResolveHeadOIDBrokenRepo 验证 exit 128 分支：损坏仓库（HEAD 内容非法 → git 视为
// 非仓库，/tmp 实测 exit 128 fatal）透传 stderr，MUST NOT 误判为 ErrUnbornHead。
func TestResolveHeadOIDBrokenRepo(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, ".git/HEAD", "garbage\n")
	_, err := ResolveHeadOID(context.Background(), repo)
	if err == nil {
		t.Fatal("want error for broken repo, got nil")
	}
	if errors.Is(err, ErrUnbornHead) {
		t.Fatalf("exit 128 MUST NOT map to ErrUnbornHead, got %v", err)
	}
	if stderr := StderrOf(err); stderr == "" {
		t.Errorf("want raw git stderr passthrough, got empty")
	}
}

// TestBranchDiffFilesParseAndByteWiseOrder 验证 numstat 解析与路径 byte-wise 字典序排序：
// '0'(0x30) < 'B'(0x42) < 'Z'(0x5A) < 'a'(0x61) < 'z'(0x7A) < 3 字节 UTF-8（中 0xE4/日 0xE6）
// < 4 字节 UTF-8（😀 0xF0），locale 序与此不同。parseNumstatZ 返回 map（遍历无序），
// 多次调用断言同一精确次序，使未排序实现必现乱序（单次调用小条目数可能偶然有序）。
func TestBranchDiffFilesParseAndByteWiseOrder(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, "a.txt", "1\n2\n3\n")
	runGit(t, repo, "add", "a.txt")
	runGit(t, repo, "commit", "-qm", "add a")
	base, err := runCapture(t, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}

	writeFile(t, repo, "a.txt", "1\nL2\n3\nl4\n") // +2 -1
	writeFile(t, repo, "z.txt", "z\n")
	writeFile(t, repo, "B.txt", "b\n")
	writeFile(t, repo, "m.txt", "m\n")
	writeFile(t, repo, "zz.txt", "zz\n")
	writeFile(t, repo, "0num.txt", "0\n")
	writeFile(t, repo, "Zed.txt", "Z\n")
	writeFile(t, repo, "中文/中文.txt", "c\n")
	writeFile(t, repo, "日本語.txt", "j\n")
	writeFile(t, repo, "😀emoji.txt", "e\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "head")
	head, err := runCapture(t, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}

	wantOrder := []string{
		"0num.txt", "B.txt", "Zed.txt", "a.txt", "m.txt",
		"z.txt", "zz.txt", "中文/中文.txt", "日本語.txt", "😀emoji.txt",
	}
	wantStats := map[string]BranchDiffFile{
		"0num.txt":   {Path: "0num.txt", Additions: 1, Deletions: 0},
		"B.txt":      {Path: "B.txt", Additions: 1, Deletions: 0},
		"Zed.txt":    {Path: "Zed.txt", Additions: 1, Deletions: 0},
		"a.txt":      {Path: "a.txt", Additions: 2, Deletions: 1},
		"m.txt":      {Path: "m.txt", Additions: 1, Deletions: 0},
		"z.txt":      {Path: "z.txt", Additions: 1, Deletions: 0},
		"zz.txt":     {Path: "zz.txt", Additions: 1, Deletions: 0},
		"中文/中文.txt":  {Path: "中文/中文.txt", Additions: 1, Deletions: 0},
		"日本語.txt":    {Path: "日本語.txt", Additions: 1, Deletions: 0},
		"😀emoji.txt": {Path: "😀emoji.txt", Additions: 1, Deletions: 0},
	}

	for round := 0; round < 20; round++ {
		files, err := BranchDiffFiles(context.Background(), repo, base, head)
		if err != nil {
			t.Fatalf("BranchDiffFiles: %v", err)
		}
		if len(files) != len(wantOrder) {
			t.Fatalf("want %d entries, got %d: %+v", len(wantOrder), len(files), files)
		}
		for i, want := range wantOrder {
			if files[i].Path != want {
				t.Fatalf("round %d: files[%d].Path = %q, want %q (got order %v)", round, i, files[i].Path, want, pathsOf(files))
			}
		}
		for _, f := range files {
			want := wantStats[f.Path]
			if f.Additions != want.Additions || f.Deletions != want.Deletions {
				t.Errorf("%q: want +%d -%d, got +%d -%d", f.Path, want.Additions, want.Deletions, f.Additions, f.Deletions)
			}
			if f.IsBinary {
				t.Errorf("%q: want IsBinary=false, got true", f.Path)
			}
		}
	}
}

// TestBranchDiffFilesRenameEntry 验证 rename 条目按新路径产出（design D4 归属判定依赖）：
// 纯 rename（git mv）列表恰为新路径条目、统计 +0 -0，旧路径不出现。
func TestBranchDiffFilesRenameEntry(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, "a.txt", "l1\nl2\nl3\n")
	runGit(t, repo, "add", "a.txt")
	runGit(t, repo, "commit", "-qm", "add a")
	base, err := runCapture(t, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}

	runGit(t, repo, "mv", "a.txt", "b.txt")
	runGit(t, repo, "commit", "-qm", "rename a to b")
	head, err := runCapture(t, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}

	files, err := BranchDiffFiles(context.Background(), repo, base, head)
	if err != nil {
		t.Fatalf("BranchDiffFiles: %v", err)
	}
	if len(files) != 1 || files[0].Path != "b.txt" {
		t.Fatalf("want exactly [b.txt], got %+v", files)
	}
	if files[0].Additions != 0 || files[0].Deletions != 0 {
		t.Errorf("pure rename: want +0 -0, got +%d -%d", files[0].Additions, files[0].Deletions)
	}
}

func pathsOf(files []BranchDiffFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

// TestBranchDiffFilesBinaryEntry 验证二进制条目：numstat 报 '-' '-' 时 IsBinary=true 且不计行。
func TestBranchDiffFilesBinaryEntry(t *testing.T) {
	repo := newTestRepo(t)
	writeFile(t, repo, "bin.dat", "text\n")
	runGit(t, repo, "add", "bin.dat")
	runGit(t, repo, "commit", "-qm", "text version")
	base, err := runCapture(t, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}

	writeFile(t, repo, "bin.dat", "BIN\x00ARY") // 含 NUL → git 判定二进制
	runGit(t, repo, "add", "bin.dat")
	runGit(t, repo, "commit", "-qm", "binary version")
	head, err := runCapture(t, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}

	files, err := BranchDiffFiles(context.Background(), repo, base, head)
	if err != nil {
		t.Fatalf("BranchDiffFiles: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 entry, got %+v", files)
	}
	f := files[0]
	if f.Path != "bin.dat" || !f.IsBinary {
		t.Fatalf("want bin.dat IsBinary=true, got %+v", f)
	}
	if f.Additions != 0 || f.Deletions != 0 {
		t.Errorf("binary entry MUST NOT count lines, got +%d -%d", f.Additions, f.Deletions)
	}
}

// TestBranchDiffFilesEmpty 验证空结果：mb == HEAD 时返回空列表（非错误）。
func TestBranchDiffFilesEmpty(t *testing.T) {
	repo := newTestRepo(t)
	oid, err := runCapture(t, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	files, err := BranchDiffFiles(context.Background(), repo, oid, oid)
	if err != nil {
		t.Fatalf("BranchDiffFiles: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("want empty list, got %+v", files)
	}
}

// TestBranchDiffFilesTooMany 验证超限：条目数超 MaxStatusFiles 返回 ErrTooManyFilesChanged
// （真实仓库提交 MaxStatusFiles+1 个文件，-short 跳过，与 git_test.go 既有超限用例同口径）。
func TestBranchDiffFilesTooMany(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping >MaxStatusFiles branch diff test in -short mode")
	}
	repo := newTestRepo(t)
	base, err := runCapture(t, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	for i := 0; i < MaxStatusFiles+1; i++ {
		writeFile(t, repo, fmt.Sprintf("f%05d.txt", i), "x\n")
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "many files")
	head, err := runCapture(t, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}

	files, err := BranchDiffFiles(context.Background(), repo, base, head)
	if !errors.Is(err, ErrTooManyFilesChanged) {
		t.Fatalf("want ErrTooManyFilesChanged for %d entries, got files=%d err=%v", MaxStatusFiles+1, len(files), err)
	}
}
