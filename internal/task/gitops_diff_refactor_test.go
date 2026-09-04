package task

// diff-review-workbench 3.1 守卫与规范化管线测试。
//
// 守卫测试先行确认现状（六阶段顺序/八字段不变量/truncated 真值表已在 gitops_diff_test.go 覆盖，
// 此处补 GitDiff 公共入口拆分后六阶段顺序不回归 + 核心 helper 可独立调用），
// 再加 UTF-8 规范化管线新行为测试（旧实现失败、新实现通过）。
//
// 规范化管线契约（specs/git-operations「文件 diff 查看」单侧内容处理管线唯一顺序）：
//   - raw bytes → NUL 嗅探（git 包已完成）→ ToValidUTF8（非法序列替换 U+FFFD）
//     → 规范化结果按 UTF-8 rune 边界限制至 524288 bytes。
//   - truncated=true iff 原始读取超限或规范化结果因上限被裁短（替换扩张导致的裁短同样置位）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"ocdeck/internal/infrastructure/git"
)

// isValidUTF8 透传 utf8.Valid（避免在测试断言里直接引 unicode/utf8 多处）。
func isValidUTF8(s string) bool { return utf8.ValidString(s) }

// TestGitDiff_RefactorSixStageOrder 验证拆分 gitDiffLocked 后六阶段失败顺序不回归：
// 阶段②（empty worktree）先于阶段③（invalid ref）—— WorktreePath 空 + ref 非空 → invalid_state。
func TestGitDiff_RefactorSixStageOrder(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.WorktreePath = "" })
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	_, err := m.GitDiff(context.Background(), "t1", "HEAD", "a.txt", false)
	if !isOpErrCode(err, codeInvalidState) {
		t.Fatalf("empty worktree + ref: err = %v, want codeInvalidState (stage② before ③ after refactor)", err)
	}
}

// TestGitDiff_RefactorEightFieldInvariant 验证拆分后八字段真值表关键分支不回归（ref vs worktree）。
func TestGitDiff_RefactorEightFieldInvariant(t *testing.T) {
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
	if d.IsBinary || d.Truncated {
		t.Errorf("refactor invariant: isBinary=%v truncated=%v, want both false", d.IsBinary, d.Truncated)
	}
}

// TestGitDiff_RefactorTruncatedTruthTable 验证拆分后 truncated 真值表不回归：
// 新侧 >512KB 纯文本 → truncated=true，内容为 512KB rune-safe 前缀。
func TestGitDiff_RefactorTruncatedTruthTable(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	commitFile(t, dir, "a.txt", "v1\n")
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
}

// --- UTF-8 规范化管线新行为测试 ---

// TestGitDiff_UTF8Normalization_ReplacesInvalidBytes 验证非法 UTF-8 字节替换为 U+FFFD。
// 旧实现直接透传 git.SideContent.Content（含非法字节），新实现经 ToValidUTF8 替换。
func TestGitDiff_UTF8Normalization_ReplacesInvalidBytes(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	commitFile(t, dir, "a.txt", "v1\n")
	// 新侧含非法 UTF-8 序列（无 NUL → 非 binary）。0xFF 为非法首字节，前后用合法 ASCII
	// 隔开使每个 0xFF 成为独立非法序列，各自替换为一个 U+FFFD（Go ToValidUTF8 把连续
	// 非法字节作为一段替换为单个 U+FFFD，故需隔开以验证逐段替换）。
	invalid := []byte("a\xffb\xffc\n")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), invalid, 0o644); err != nil {
		t.Fatal(err)
	}
	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	d, err := m.GitDiff(context.Background(), "t1", "", "a.txt", false)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	// 每个 0xFF 替换为 U+FFFD（UTF-8 编码 0xEF 0xBF 0xBD）。
	want := "a\uFFFDb\uFFFDc\n"
	if d.NewContent != want {
		t.Errorf("newContent = %q, want %q (invalid bytes replaced with U+FFFD)", d.NewContent, want)
	}
	if d.IsBinary {
		t.Errorf("invalid utf8: isBinary = true, want false (no NUL)")
	}
	if d.Truncated {
		t.Errorf("invalid utf8: truncated = true, want false (within limit)")
	}
}

// TestGitDiff_UTF8Normalization_ReplacementExpansionTruncates 验证替换扩张导致规范化结果
// 超过 524288 bytes 时按 rune 边界裁短并置 truncated=true（specs scenario「非法 UTF-8 替换扩张裁短」）。
// 构造：原始读取未超上限，但大量独立非法单字节（用合法字节隔开）替换为 3 字节 U+FFFD 后超过上限。
func TestGitDiff_UTF8Normalization_ReplacementExpansionTruncates(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	commitFile(t, dir, "a.txt", "v1\n")
	// 用 "\xffa" 重复填充：每个 0xFF 独立替换为 3 字节 U+FFFD，'a' 为 1 字节合法。
	// 4 字节原始 → 4 字节规范化（3+1），无扩张。改用纯 0xFF 间隔合法多字节？
	// 实际扩张需：单字节非法(1) → U+FFFD(3)，扩张系数 3。用 "\xff" 重复但需各自独立：
	// 用 2 字节合法间隔会稀释。改用更密方案："\xff\xff" 连续 → 单个 U+FFFD（无扩张）。
	// 要稳定触发扩张且每字节独立：用 ASCII 字节 0x80（非法 continuation）间隔合法 1 字节。
	// 0x80 单字节非法（continuation 无首字节）→ U+FFFD(3)，扩张 3x。用 "x\x80" 重复：
	// 2 字节原始 → 4 字节规范化（1+3），扩张 2x。
	// 填充至 rawLen 使规范化结果（2x）超过 FileContentMaxBytes。
	rawLen := git.FileContentMaxBytes/2 + 1024
	raw := strings.Repeat("x\x80", rawLen/2)
	// 调整到接近 rawLen 字节（可能差 1）。
	for len(raw) < rawLen && len(raw) < git.FileContentMaxBytes {
		raw += "x"
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(raw), 0o644); err != nil {
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
		t.Fatalf("replacement expansion: truncated = false, want true (normalized result exceeds limit)")
	}
	// 规范化结果必须按 rune 边界截到 ≤ FileContentMaxBytes。
	if len(d.NewContent) > git.FileContentMaxBytes {
		t.Errorf("newContent len = %d, want <= %d (rune boundary truncation)", len(d.NewContent), git.FileContentMaxBytes)
	}
	// 截断后内容不应含原始非法字节 0x80。
	if strings.ContainsRune(d.NewContent, '\x80') {
		t.Errorf("newContent contains raw invalid byte 0x80, want fully normalized")
	}
	// 规范化结果为合法 UTF-8（utf8.Valid）。
	if !isValidUTF8(d.NewContent) {
		t.Errorf("newContent is not valid UTF-8 after normalization")
	}
}

// TestGitDiff_UTF8Normalization_RuneBoundaryTruncation 验证规范化结果超限时截到 rune 边界，
// 不会在多字节 rune 中间截断产生新的非法字节。用合法多字节 rune（"世"=3 字节）填充，
// 使规范化结果（无需替换）恰好超过上限，断言截断点为 rune 边界。
func TestGitDiff_UTF8Normalization_RuneBoundaryTruncation(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	commitFile(t, dir, "a.txt", "v1\n")
	// "世" 为 3 字节 rune。填充至字节数略超 FileContentMaxBytes（非 3 的倍数，强制边界回退）。
	rune3 := "世" // 0xE4 0xB8 0x96
	count := git.FileContentMaxBytes/3 + 10
	raw := strings.Repeat(rune3, count)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(raw), 0o644); err != nil {
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
		t.Fatalf("rune boundary: truncated = false, want true")
	}
	if len(d.NewContent) > git.FileContentMaxBytes {
		t.Errorf("newContent len = %d, want <= %d", len(d.NewContent), git.FileContentMaxBytes)
	}
	// 截断点必须为 rune 边界：长度是 3 的倍数。
	if len(d.NewContent)%3 != 0 {
		t.Errorf("newContent len = %d, want multiple of 3 (rune boundary, no mid-rune cut)", len(d.NewContent))
	}
	// 内容为合法 UTF-8（全 "世" rune），无残缺字节。
	for _, r := range d.NewContent {
		if r != '世' {
			t.Errorf("newContent contains rune %q, want all '世'", r)
			break
		}
	}
}

// TestGitDiff_UTF8Normalization_FourByteRuneSpanningBoundary（F10）验证四字节 rune 跨
// 读取边界时，正确管线（raw→NUL嗅探→ToValidUTF8→rune边界524288）整体移除越界 rune，
// 不产生预截断残片替换的 U+FFFD。
//
// 构造：524285 字节 ASCII + 1 个四字节 rune（U+1F600 😀 = F0 9F 98 80），总 524289 字节
// 恰为工作区有界读取上限（FileContentMaxBytes+1）。读取返回完整 524289 字节，rune 完整。
// 新管线：ToValidUTF8 无变化 → rune 边界 524288 回退到 524286（完整 rune 起始）→
// 输出 524286 字节（524285 ASCII + 完整 😀 rune），无 U+FFFD。
// 旧管线（预截断 524288 字节）：切出 524286-524288 的 3 字节残片 → ToValidUTF8 替换 U+FFFD
// → 输出 524288 字节含 U+FFFD（正确管线不会出现）。
func TestGitDiff_UTF8Normalization_FourByteRuneSpanningBoundary(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	commitFile(t, dir, "a.txt", "v1\n")
	rune4 := "😀" // U+1F600, 4 字节 UTF-8: F0 9F 98 80
	// 524285 字节 ASCII + 1 个四字节 rune = 524289 字节（= 工作区有界读取上限）。
	prefix := strings.Repeat("a", git.FileContentMaxBytes-3) // 524285
	raw := prefix + rune4                                      // 524289
	if len(raw) != git.FileContentMaxBytes+1 {
		t.Fatalf("setup: raw len = %d, want %d", len(raw), git.FileContentMaxBytes+1)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(raw), 0o644); err != nil {
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
		t.Fatalf("four-byte spanning: truncated = false, want true (raw read at limit)")
	}
	// 正确管线：rune 边界 524288 回退越过完整四字节 rune（其末字节恰为 524288），
	// 整体移除越界 rune，输出 524285 字节全 ASCII，无 U+FFFD、无残片。
	if len(d.NewContent) != git.FileContentMaxBytes-3 {
		t.Errorf("newContent len = %d, want %d (rune boundary retreat past full 4-byte rune)", len(d.NewContent), git.FileContentMaxBytes-3)
	}
	if !utf8.ValidString(d.NewContent) {
		t.Errorf("newContent is not valid UTF-8 after normalization")
	}
	// 不应出现 U+FFFD（正确管线整体移除越界 rune，非残片替换）。
	if strings.Contains(d.NewContent, "\uFFFD") {
		t.Errorf("newContent contains U+FFFD, want none (4-byte rune removed wholesale, not fragment-replaced)")
	}
	// 末尾应为 ASCII（越界 rune 被整体移除）。
	if !strings.HasSuffix(d.NewContent, "a") {
		t.Errorf("newContent should end with ASCII 'a' (4-byte rune removed), got suffix %q", d.NewContent[len(d.NewContent)-4:])
	}
}

// TestNormalizeDiffSideContent_TruthTable 直接测规范化 helper 的真值表（不依赖 git 仓库）。
func TestNormalizeDiffSideContent_TruthTable(t *testing.T) {
	cases := []struct {
		name        string
		content     string
		rawTrunc    bool
		wantNorm    string
		wantTrunc   bool
	}{
		{"empty passthrough", "", false, "", false},
		{"valid utf8 unchanged", "hello\n", false, "hello\n", false},
		{"invalid bytes replaced", "a\xffb\xffc", false, "a\uFFFDb\uFFFDc", false},
		{"raw truncated flag preserved", "abc", true, "abc", true},
		{"replacement expansion truncates",
			strings.Repeat("x\x80", git.FileContentMaxBytes/4+10), false,
			strings.Repeat("x\uFFFD", git.FileContentMaxBytes/4), true},
		{"rune boundary truncation on valid multibyte",
			strings.Repeat("世", git.FileContentMaxBytes/3+5), false,
			strings.Repeat("世", git.FileContentMaxBytes/3), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, gotTrunc := normalizeDiffSideContent(c.content, c.rawTrunc)
			if got != c.wantNorm {
				t.Errorf("normalized = %q, want %q", got, c.wantNorm)
			}
			if gotTrunc != c.wantTrunc {
				t.Errorf("truncated = %v, want %v", gotTrunc, c.wantTrunc)
			}
			if len(got) > git.FileContentMaxBytes {
				t.Errorf("normalized len = %d, want <= %d", len(got), git.FileContentMaxBytes)
			}
		})
	}
}

// TestTruncateAtRuneBoundary 验证 rune 边界截断不产生残缺字节。
func TestTruncateAtRuneBoundary(t *testing.T) {
	cases := []struct {
		s       string
		max     int
		want    string
	}{
		{"abc", 10, "abc"},          // 未超上限原样返回
		{"abc", 3, "abc"},           // 恰好等于上限
		{"abcdef", 4, "abcd"},       // 单字节 rune 边界
		{"世世世", 4, "世"},          // 3 字节 rune，max=4 回退到 3（第一个完整 rune）
		{"世世世", 6, "世世"},        // 恰好两个 rune
		{"世世世", 7, "世世"},        // max=7 回退到 6
		{"a世b", 2, "a"},            // max=2 在 "世" 之前停止（"a"=1 字节，下一 rune=3 字节超限）
		{"", 5, ""},                 // 空串
	}
	for _, c := range cases {
		got := truncateAtRuneBoundary(c.s, c.max)
		if got != c.want {
			t.Errorf("truncateAtRuneBoundary(%q, %d) = %q, want %q", c.s, c.max, got, c.want)
		}
	}
}