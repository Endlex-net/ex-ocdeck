// payload_test.go 测试 D7 payload 组装逐字公式（design.md D7 + golden 固化）。
package diffreview

import (
	"strings"
	"testing"
)

// TestPayloadFixedHeaderAndAnnotationSection 验证核心区逐字拼接（fixedHeader/note/批注节/相关diff标题）。
func TestPayloadFixedHeaderAndAnnotationSection(t *testing.T) {
	anns := []DiffAnnotationRecord{
		{ID: "a1", Path: "foo.go", Side: "new", Ref: "", Untracked: false, StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "hello\n", Comment: "fix this", CreatedAt: 100},
	}
	result, err := assemblePayloadFromAnnotations(anns, "", func(src sourceTriple) (DiffSourceResult, error) {
		return DiffSourceResult{OldContent: "", NewContent: "hello\n", OldExists: false, NewExists: true, OldMode: "", NewMode: "100644"}, nil
	})
	if err != nil {
		t.Fatalf("assemblePayload: %v", err)
	}
	// fixedHeader 必须逐字出现。
	if !strings.Contains(result.Payload, fixedHeader) {
		t.Errorf("payload missing fixedHeader\ngot: %s", result.Payload)
	}
	// 批注节标题 "## 批注"。
	if !strings.Contains(result.Payload, "## 批注") {
		t.Errorf("payload missing annotation section header")
	}
	// 批注块标题格式：### 批注 1 — "foo.go" (new，来源 index，行 L1)
	if !strings.Contains(result.Payload, `### 批注 1 — "foo.go" (new，来源 index，行 L1)`) {
		t.Errorf("payload missing annotation block header\ngot: %s", result.Payload)
	}
	// 相关 diff 标题 "## 相关 diff"。
	if !strings.Contains(result.Payload, "## 相关 diff") {
		t.Errorf("payload missing related diff title")
	}
	// 来源段标题 ### "foo.go"（来源 index）
	if !strings.Contains(result.Payload, `### "foo.go"（来源 index）`) {
		t.Errorf("payload missing source segment header")
	}
}

// TestPayloadNoteSection 验证 note 非空时补充说明节出现，空时不出现。
func TestPayloadNoteSection(t *testing.T) {
	anns := []DiffAnnotationRecord{
		{ID: "a1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "c", CreatedAt: 1},
	}
	withNote, _ := assemblePayloadFromAnnotations(anns, "please review", func(sourceTriple) (DiffSourceResult, error) {
		return DiffSourceResult{NewContent: "x\n", NewExists: true, NewMode: "100644"}, nil
	})
	if !strings.Contains(withNote.Payload, "## 补充说明\nplease review") {
		t.Errorf("note section missing when note non-empty\ngot: %s", withNote.Payload)
	}
	withoutNote, _ := assemblePayloadFromAnnotations(anns, "", func(sourceTriple) (DiffSourceResult, error) {
		return DiffSourceResult{NewContent: "x\n", NewExists: true, NewMode: "100644"}, nil
	})
	if strings.Contains(withoutNote.Payload, "## 补充说明") {
		t.Errorf("note section present when note empty")
	}
}

// TestPayloadAnnotationSorting 验证批注排序 created_at 升序平局 id 字典序。
func TestPayloadAnnotationSorting(t *testing.T) {
	anns := []DiffAnnotationRecord{
		{ID: "b", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "b", CreatedAt: 200},
		{ID: "a", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "a", CreatedAt: 100},
		{ID: "c", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "c", CreatedAt: 100},
	}
	result, _ := assemblePayloadFromAnnotations(anns, "", func(sourceTriple) (DiffSourceResult, error) {
		return DiffSourceResult{NewContent: "x\n", NewExists: true, NewMode: "100644"}, nil
	})
	// 顺序应为 a(100), c(100), b(200) → 批注 1=a, 2=c, 3=b
	idxA := strings.Index(result.Payload, "评论：a")
	idxC := strings.Index(result.Payload, "评论：c")
	idxB := strings.Index(result.Payload, "评论：b")
	if !(idxA < idxC && idxC < idxB) {
		t.Errorf("annotation sort wrong: a=%d c=%d b=%d", idxA, idxC, idxB)
	}
}

// TestPayloadDynamicFence 验证动态 fence（含反引号内容时 fence 长度 = 最长反引号串+1）。
func TestPayloadDynamicFence(t *testing.T) {
	snapshot := "code with `backticks` and ```triple```"
	anns := []DiffAnnotationRecord{
		{ID: "a1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: snapshot, Comment: "c", CreatedAt: 1},
	}
	result, _ := assemblePayloadFromAnnotations(anns, "", func(sourceTriple) (DiffSourceResult, error) {
		return DiffSourceResult{NewContent: snapshot, NewExists: true, NewMode: "100644"}, nil
	})
	// 最长反引号串 = 3，fence = 4 个反引号 "````"
	if !strings.Contains(result.Payload, "````\n"+snapshot+"\n````") {
		t.Errorf("dynamic fence wrong for triple backtick content\ngot: %s", result.Payload)
	}
}

// TestPayloadDynamicFenceMinThree 验证 fence 最小 3（无反引号内容）。
func TestPayloadDynamicFenceMinThree(t *testing.T) {
	snapshot := "plain code"
	anns := []DiffAnnotationRecord{
		{ID: "a1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: snapshot, Comment: "c", CreatedAt: 1},
	}
	result, _ := assemblePayloadFromAnnotations(anns, "", func(sourceTriple) (DiffSourceResult, error) {
		return DiffSourceResult{NewContent: snapshot, NewExists: true, NewMode: "100644"}, nil
	})
	if !strings.Contains(result.Payload, "```\n"+snapshot+"\n```") {
		t.Errorf("min fence should be 3 backticks\ngot: %s", result.Payload)
	}
}

// TestPayloadSourceLabel 验证 source 标签（untracked→untracked; ref非空→Q(ref); 否则→index）。
func TestPayloadSourceLabel(t *testing.T) {
	cases := []struct {
		ref       string
		untracked bool
		want      string
	}{
		{"", false, "index"},
		{"HEAD", false, `"HEAD"`},
		{"", true, "untracked"},
	}
	for _, c := range cases {
		got := sourceLabel(c.ref, c.untracked)
		if got != c.want {
			t.Errorf("sourceLabel(%q,%v)=%q want %q", c.ref, c.untracked, got, c.want)
		}
	}
}

// TestPayloadRangeLabel 验证 range 标签（单行 L<n>，多行 L<start>-L<end>）。
func TestPayloadRangeLabel(t *testing.T) {
	if got := rangeLabel(5, 5); got != "L5" {
		t.Errorf("rangeLabel(5,5)=%q want L5", got)
	}
	if got := rangeLabel(3, 7); got != "L3-L7" {
		t.Errorf("rangeLabel(3,7)=%q want L3-L7", got)
	}
}

// TestPayloadContentShapeBinary 验证 isBinary → "# binary: content unavailable"。
func TestPayloadContentShapeBinary(t *testing.T) {
	r := DiffSourceResult{IsBinary: true, OldExists: true, NewExists: true}
	got := contentShapeBlock("f.go", r)
	if got != "# binary: content unavailable" {
		t.Errorf("binary shape = %q", got)
	}
}

// TestPayloadContentShapeGitlink 验证 mode=160000 → gitlink 块。
func TestPayloadContentShapeGitlink(t *testing.T) {
	r := DiffSourceResult{OldMode: "160000", NewMode: "160000", OldContent: "abc", NewContent: "def", OldExists: true, NewExists: true}
	got := contentShapeBlock("subm", r)
	if got != "# gitlink: abc -> def" {
		t.Errorf("gitlink shape = %q", got)
	}
}

// TestPayloadContentShapeMissingBoth 验证双侧不存在 → "# file missing on both sides"。
func TestPayloadContentShapeMissingBoth(t *testing.T) {
	r := DiffSourceResult{OldExists: false, NewExists: false}
	got := contentShapeBlock("f.go", r)
	if got != "# file missing on both sides" {
		t.Errorf("missing both shape = %q", got)
	}
}

// TestPayloadContentShapeEmptyFileAdded 验证空文件新增。
func TestPayloadContentShapeEmptyFileAdded(t *testing.T) {
	r := DiffSourceResult{OldExists: false, NewExists: true, OldContent: "", NewContent: ""}
	got := contentShapeBlock("f.go", r)
	if got != "# empty file added" {
		t.Errorf("empty added shape = %q", got)
	}
}

// TestPayloadContentShapeEmptyFileDeleted 验证空文件删除。
func TestPayloadContentShapeEmptyFileDeleted(t *testing.T) {
	r := DiffSourceResult{OldExists: true, NewExists: false, OldContent: "", NewContent: ""}
	got := contentShapeBlock("f.go", r)
	if got != "# empty file deleted" {
		t.Errorf("empty deleted shape = %q", got)
	}
}

// TestPayloadModeChangeControlLine 验证元数据控制行① mode change。
func TestPayloadModeChangeControlLine(t *testing.T) {
	r := DiffSourceResult{OldExists: true, NewExists: true, OldMode: "100644", NewMode: "100755", OldContent: "x\n", NewContent: "x\n"}
	seg := buildSourceSegment(sourceTriple{Path: "f.go"}, r)
	if !strings.Contains(seg, "# mode change: 100644 -> 100755") {
		t.Errorf("mode change control line missing\ngot: %s", seg)
	}
}

// TestPayloadTruncatedControlLine 验证元数据控制行② truncated。
func TestPayloadTruncatedControlLine(t *testing.T) {
	r := DiffSourceResult{Truncated: true, OldExists: true, NewExists: true, OldMode: "100644", NewMode: "100644", OldContent: "x\n", NewContent: "x\n"}
	seg := buildSourceSegment(sourceTriple{Path: "f.go"}, r)
	if !strings.Contains(seg, "# truncated: content is a bounded prefix") {
		t.Errorf("truncated control line missing\ngot: %s", seg)
	}
}

// TestPayloadFormatterNoVisibleChange 验证相同内容 → "# no visible change"。
func TestPayloadFormatterNoVisibleChange(t *testing.T) {
	r := DiffSourceResult{OldExists: true, NewExists: true, OldContent: "same\n", NewContent: "same\n", OldMode: "100644", NewMode: "100644"}
	got := unifiedDiffBlock("f.go", r)
	if got != "# no visible change" {
		t.Errorf("identical content should be no visible change, got: %q", got)
	}
}

// TestPayloadFormatterActualDiff 验证有差异时生成 unified diff。
func TestPayloadFormatterActualDiff(t *testing.T) {
	r := DiffSourceResult{OldExists: true, NewExists: true, OldContent: "a\nb\nc\n", NewContent: "a\nB\nc\n", OldMode: "100644", NewMode: "100644"}
	got := unifiedDiffBlock("f.go", r)
	if !strings.Contains(got, "-b") || !strings.Contains(got, "+B") {
		t.Errorf("unified diff missing -b/+B\ngot: %s", got)
	}
}

// TestPayloadFormatterMissingFinalNewline 验证 EOF 控制行规则④（库输出非空时追加 EOF 行）。
func TestPayloadFormatterMissingFinalNewline(t *testing.T) {
	// old="a" (no final \n), new="b" (no final \n) → 两侧 missingFinalNewline=true
	r := DiffSourceResult{OldExists: true, NewExists: true, OldContent: "a", NewContent: "b", OldMode: "100644", NewMode: "100644"}
	got := unifiedDiffBlock("f.go", r)
	if !strings.Contains(got, `\ No newline at end of file (a side)`) {
		t.Errorf("missing EOF marker for a side\ngot: %s", got)
	}
	if !strings.Contains(got, `\ No newline at end of file (b side)`) {
		t.Errorf("missing EOF marker for b side\ngot: %s", got)
	}
}

// TestPayloadFormatterEOFOnlyDifference 验证仅 EOF 差异（双侧 flag 不同 → synthetic headers + EOF 行）。
func TestPayloadFormatterEOFOnlyDifference(t *testing.T) {
	// old="a\n", new="a" → 内容相同但 new 缺末尾换行 → 库空串，flag 不同 → synthetic headers + b side EOF
	r := DiffSourceResult{OldExists: true, NewExists: true, OldContent: "a\n", NewContent: "a", OldMode: "100644", NewMode: "100644"}
	got := unifiedDiffBlock("f.go", r)
	if !strings.Contains(got, `\ No newline at end of file (b side)`) {
		t.Errorf("EOF-only diff (flags differ) should have b side EOF marker\ngot: %s", got)
	}
}

// TestPayloadTruncatedByBudget 验证 65536 预算截断（来源段超预算 → 截断标记）。
func TestPayloadTruncatedByBudget(t *testing.T) {
	// 构造大量批注使 sourceRegion 超预算。
	bigContent := strings.Repeat("x", 70000) + "\n"
	anns := []DiffAnnotationRecord{
		{ID: "a1", Path: "big.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "c", CreatedAt: 1},
	}
	result, err := assemblePayloadFromAnnotations(anns, "", func(sourceTriple) (DiffSourceResult, error) {
		return DiffSourceResult{OldContent: "", NewContent: bigContent, OldExists: false, NewExists: true, NewMode: "100644"}, nil
	})
	if err != nil {
		t.Fatalf("assemblePayload: %v", err)
	}
	if len(result.Payload) > payloadMaxBytes {
		t.Errorf("payload %d > %d", len(result.Payload), payloadMaxBytes)
	}
	if !result.Truncated {
		t.Errorf("expected truncated=true for oversized source")
	}
	if !strings.Contains(result.Payload, "...[diff 已截断]...") {
		t.Errorf("truncated payload missing marker")
	}
}

// TestPayloadCoreTooLarge 验证核心区超阈值 → ErrPayloadTooLarge。
func TestPayloadCoreTooLarge(t *testing.T) {
	bigNote := strings.Repeat("x", 66000)
	anns := []DiffAnnotationRecord{
		{ID: "a1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "c", CreatedAt: 1},
	}
	_, err := assemblePayloadFromAnnotations(anns, bigNote, func(sourceTriple) (DiffSourceResult, error) {
		return DiffSourceResult{NewContent: "x\n", NewExists: true, NewMode: "100644"}, nil
	})
	if err != ErrPayloadTooLarge {
		t.Errorf("expected ErrPayloadTooLarge, got %v", err)
	}
}

// TestPayloadGitDiffError 验证 GitDiff 读取失败 → 整体失败零副作用。
func TestPayloadGitDiffError(t *testing.T) {
	anns := []DiffAnnotationRecord{
		{ID: "a1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "c", CreatedAt: 1},
	}
	gitErr := errGitReadFailure
	_, err := assemblePayloadFromAnnotations(anns, "", func(sourceTriple) (DiffSourceResult, error) {
		return DiffSourceResult{}, gitErr
	})
	if err != gitErr {
		t.Errorf("expected git error propagation, got %v", err)
	}
}

// TestPayloadNoTruncationMarkerWhenUnderBudget 验证未超预算时 MUST NOT 附 marker。
func TestPayloadNoTruncationMarkerWhenUnderBudget(t *testing.T) {
	anns := []DiffAnnotationRecord{
		{ID: "a1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "c", CreatedAt: 1},
	}
	result, _ := assemblePayloadFromAnnotations(anns, "", func(sourceTriple) (DiffSourceResult, error) {
		return DiffSourceResult{NewContent: "x\n", NewExists: true, NewMode: "100644"}, nil
	})
	if result.Truncated {
		t.Errorf("should not be truncated when under budget")
	}
	if strings.Contains(result.Payload, "...[diff 已截断]...") {
		t.Errorf("truncation marker present when under budget")
	}
}

// TestPayloadGitTruncatedFlag 验证 GitDiff.truncated 标志汇总到 truncated 真值。
func TestPayloadGitTruncatedFlag(t *testing.T) {
	anns := []DiffAnnotationRecord{
		{ID: "a1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "c", CreatedAt: 1},
	}
	result, _ := assemblePayloadFromAnnotations(anns, "", func(sourceTriple) (DiffSourceResult, error) {
		return DiffSourceResult{NewContent: "x\n", NewExists: true, NewMode: "100644", Truncated: true}, nil
	})
	if !result.Truncated {
		t.Errorf("GitDiff.truncated should propagate to submission.truncated")
	}
}

// TestRuneSafePrefix 验证 rune-safe 截断（不在 UTF-8 rune 中间截断）。
func TestRuneSafePrefix(t *testing.T) {
	// "你好" = 6 bytes (3 bytes per rune)
	s := "你好world"
	// 截到 4 bytes → 应截到 3 bytes（第一个 rune 完整）
	got := runeSafePrefix(s, 4)
	if got != "你" {
		t.Errorf("runeSafePrefix(s,4)=%q want %q", got, "你")
	}
}

// errGitReadFailure 为测试用的 GitDiff 读取错误。
var errGitReadFailure = &testError{"git read failed"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }
