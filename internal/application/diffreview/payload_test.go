// payload_test.go 测试 payload 组装逐字公式（无相关 diff 段，行号高效格式，golden 固化）。
package diffreview

import (
	"strings"
	"testing"
)

// TestPayloadFixedHeaderAndAnnotationSection 验证核心区逐字拼接（fixedHeader/批注节；无相关 diff 段）。
func TestPayloadFixedHeaderAndAnnotationSection(t *testing.T) {
	anns := []DiffAnnotationRecord{
		{ID: "a1", Path: "foo.go", Side: "new", Ref: "", Untracked: false, StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "hello\n", Comment: "fix this", CreatedAt: 100},
	}
	result, err := assemblePayloadFromAnnotations(anns, "")
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
	// 批注块标题格式（行号高效格式）：### 批注 1 — "foo.go":1 (new，来源 index)
	if !strings.Contains(result.Payload, `### 批注 1 — "foo.go":1 (new，来源 index)`) {
		t.Errorf("payload missing annotation block header\ngot: %s", result.Payload)
	}
	// MUST NOT 出现相关 diff 段（用户决策：不附加 Context 内容）。
	if strings.Contains(result.Payload, "## 相关 diff") {
		t.Errorf("payload must not contain related diff section")
	}
	// 不产生预算截断。
	if result.Truncated {
		t.Errorf("truncated must be false (no related diff)")
	}
}

// TestPayloadLineRangeFormat 验证行号高效格式（Q(path):start-end / 单行 Q(path):start）。
func TestPayloadLineRangeFormat(t *testing.T) {
	anns := []DiffAnnotationRecord{
		{ID: "a1", Path: "internal/task/diffreview_coverage_test.go", Side: "new", StartLine: 840, EndLine: 845, SnapshotStartLine: 837, SnapshotLineCount: 9, Snapshot: "x\n", Comment: "c1", CreatedAt: 1},
		{ID: "a2", Path: "f.go", Side: "old", Ref: "HEAD", StartLine: 7, EndLine: 7, SnapshotStartLine: 4, SnapshotLineCount: 7, Snapshot: "y\n", Comment: "c2", CreatedAt: 2},
	}
	result, err := assemblePayloadFromAnnotations(anns, "")
	if err != nil {
		t.Fatalf("assemblePayload: %v", err)
	}
	if !strings.Contains(result.Payload, `### 批注 1 — "internal/task/diffreview_coverage_test.go":840-845 (new，来源 index)`) {
		t.Errorf("multi-line range format wrong\ngot: %s", result.Payload)
	}
	if !strings.Contains(result.Payload, `### 批注 2 — "f.go":7 (old，来源 "HEAD")`) {
		t.Errorf("single-line range format wrong\ngot: %s", result.Payload)
	}
	// 旧格式 L840-L845 / 行 L840-L845 MUST NOT 出现。
	if strings.Contains(result.Payload, "L840") || strings.Contains(result.Payload, "行 ") {
		t.Errorf("legacy line-range label must not appear\ngot: %s", result.Payload)
	}
}

// TestPayloadNoteSection 验证 note 非空时补充说明节出现，空时不出现。
func TestPayloadNoteSection(t *testing.T) {
	anns := []DiffAnnotationRecord{
		{ID: "a1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "c", CreatedAt: 1},
	}
	withNote, _ := assemblePayloadFromAnnotations(anns, "please review")
	if !strings.Contains(withNote.Payload, "## 补充说明\nplease review") {
		t.Errorf("note section missing when note non-empty\ngot: %s", withNote.Payload)
	}
	withoutNote, _ := assemblePayloadFromAnnotations(anns, "")
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
	result, _ := assemblePayloadFromAnnotations(anns, "")
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
	result, _ := assemblePayloadFromAnnotations(anns, "")
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
	result, _ := assemblePayloadFromAnnotations(anns, "")
	if !strings.Contains(result.Payload, "```\n"+snapshot+"\n```") {
		t.Errorf("min fence wrong\ngot: %s", result.Payload)
	}
}

// TestPayloadSourceLabel 验证来源标签（untracked→untracked；ref 非空→Q(ref)；否则→index）。
func TestPayloadSourceLabel(t *testing.T) {
	cases := []struct {
		name      string
		rec       DiffAnnotationRecord
		wantLabel string
	}{
		{"untracked", DiffAnnotationRecord{ID: "a", Path: "f.go", Side: "new", Untracked: true, StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x", Comment: "c", CreatedAt: 1}, "untracked"},
		{"ref", DiffAnnotationRecord{ID: "b", Path: "f.go", Side: "old", Ref: "HEAD", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x", Comment: "c", CreatedAt: 1}, `"HEAD"`},
		{"index", DiffAnnotationRecord{ID: "c", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x", Comment: "c", CreatedAt: 1}, "index"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, _ := assemblePayloadFromAnnotations([]DiffAnnotationRecord{tc.rec}, "")
			if !strings.Contains(result.Payload, "来源 "+tc.wantLabel) {
				t.Errorf("source label wrong, want %s\ngot: %s", tc.wantLabel, result.Payload)
			}
		})
	}
}

// TestPayloadCoreTooLarge 验证核心内容超 65536 字节 → ErrPayloadTooLarge（零副作用）。
func TestPayloadCoreTooLarge(t *testing.T) {
	// 构造批注使 core 超阈值（大 comment）。
	big := strings.Repeat("x", payloadMaxBytes)
	anns := []DiffAnnotationRecord{
		{ID: "a1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x", Comment: big, CreatedAt: 1},
	}
	_, err := assemblePayloadFromAnnotations(anns, "")
	if err != ErrPayloadTooLarge {
		t.Fatalf("err=%v want ErrPayloadTooLarge", err)
	}
}

// TestPayloadBoundary65536 验证 65536 字节精确边界：恰好 65536 接受，+1 拒绝。
func TestPayloadBoundary65536(t *testing.T) {
	anns := []DiffAnnotationRecord{
		{ID: "a1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x", Comment: "", CreatedAt: 1},
	}
	// 二分找到恰好不超界的 comment 长度。
	lo, hi := 0, payloadMaxBytes
	for lo < hi {
		mid := (lo + hi + 1) / 2
		anns[0].Comment = strings.Repeat("c", mid)
		if _, err := assemblePayloadFromAnnotations(anns, ""); err == nil {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	anns[0].Comment = strings.Repeat("c", lo)
	res, err := assemblePayloadFromAnnotations(anns, "")
	if err != nil {
		t.Fatalf("max fitting comment should be accepted: %v", err)
	}
	if len(res.Payload) > payloadMaxBytes {
		t.Fatalf("payload %d exceeds %d", len(res.Payload), payloadMaxBytes)
	}
	anns[0].Comment = strings.Repeat("c", lo+1)
	if _, err := assemblePayloadFromAnnotations(anns, ""); err != ErrPayloadTooLarge {
		t.Fatalf("+1 byte should be rejected: %v", err)
	}
}

// TestRuneSafePrefix 验证 rune 边界截断。
func TestRuneSafePrefix(t *testing.T) {
	s := "ab中cd"
	if got := runeSafePrefix(s, 3); got != "ab" {
		t.Errorf("runeSafePrefix mid-rune: %q want %q", got, "ab")
	}
	if got := runeSafePrefix(s, 100); got != s {
		t.Errorf("runeSafePrefix over-long: %q want %q", got, s)
	}
}

// TestPayloadConcatenationGolden 验证多批注逐字拼接 golden（字段原样插入 MUST NOT trim）。
func TestPayloadConcatenationGolden(t *testing.T) {
	anns := []DiffAnnotationRecord{
		{ID: "a1", Path: "a.go", Side: "new", StartLine: 1, EndLine: 2, SnapshotStartLine: 1, SnapshotLineCount: 2, Snapshot: "l1\nl2\n", Comment: "c1\n", CreatedAt: 1},
		{ID: "a2", Path: "b.go", Side: "old", Ref: "", StartLine: 3, EndLine: 3, SnapshotStartLine: 1, SnapshotLineCount: 3, Snapshot: "s1\n", Comment: "c2", CreatedAt: 2},
	}
	result, err := assemblePayloadFromAnnotations(anns, "n1")
	if err != nil {
		t.Fatalf("assemblePayload: %v", err)
	}
	want := fixedHeader +
		"\n\n## 补充说明\nn1" +
		"\n\n## 批注" +
		"\n\n### 批注 1 — \"a.go\":1-2 (new，来源 index)" +
		"\n评论：c1\n" +
		"\n```\nl1\nl2\n\n```" +
		"\n\n### 批注 2 — \"b.go\":3 (old，来源 index)" +
		"\n评论：c2" +
		"\n```\ns1\n\n```"
	if result.Payload != want {
		t.Errorf("payload golden mismatch:\n--- got ---\n%s\n--- want ---\n%s", result.Payload, want)
	}
}
