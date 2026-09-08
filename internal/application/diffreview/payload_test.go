// payload_test.go 测试 payload 组装逐字公式（无相关 diff 段，行号高效格式，golden 固化）。
package diffreview

import (
	"errors"
	"strconv"
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
// a1 为 6 行范围（> 5 行，D9 省略分支）：窗口自洽（837..845 共 9 行）且输出无 fence。
func TestPayloadLineRangeFormat(t *testing.T) {
	anns := []DiffAnnotationRecord{
		{ID: "a1", Path: "internal/task/diffreview_coverage_test.go", Side: "new", StartLine: 840, EndLine: 845, SnapshotStartLine: 837, SnapshotLineCount: 9, Snapshot: "l1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\nl9", Comment: "c1", CreatedAt: 1},
		{ID: "a2", Path: "f.go", Side: "old", Ref: "HEAD", StartLine: 7, EndLine: 7, SnapshotStartLine: 7, SnapshotLineCount: 2, Snapshot: "y\n", Comment: "c2", CreatedAt: 2},
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
	// D9：6 行范围（> 5 行）→ 省略分支，批注 1 块仅头与评论（逐字），其后直接跟批注 2 块（无 fence 段）。
	omittedBlock := `### 批注 1 — "internal/task/diffreview_coverage_test.go":840-845 (new，来源 index)` + "\n评论：c1"
	if !strings.Contains(result.Payload, omittedBlock+"\n\n### 批注 2") {
		t.Errorf("6-line range block must omit fence\ngot: %s", result.Payload)
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
		// a2 窗口自洽（3..4 覆盖行 3）：仅调整 SnapshotStartLine/SnapshotLineCount，golden 字节不变。
		{ID: "a2", Path: "b.go", Side: "old", Ref: "", StartLine: 3, EndLine: 3, SnapshotStartLine: 3, SnapshotLineCount: 2, Snapshot: "s1\n", Comment: "c2", CreatedAt: 2},
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

// --- D9 统一省略规则（tasks 6.3） ---

// TestPayloadOmissionLineCountBoundary 验证引用范围行数恰 5 保留 fence、恰 6 省略
// （D9：行数 > 5 时仅输出批注头与评论，逐字不变）。
func TestPayloadOmissionLineCountBoundary(t *testing.T) {
	build := func(n int) DiffAnnotationRecord {
		lines := make([]string, n)
		for i := range lines {
			lines[i] = "l" + strconv.Itoa(i+1)
		}
		return DiffAnnotationRecord{
			ID: "a1", Path: "f.go", Side: "new", StartLine: 1, EndLine: n,
			SnapshotStartLine: 1, SnapshotLineCount: n,
			Snapshot: strings.Join(lines, "\n"), Comment: "c", CreatedAt: 1,
		}
	}
	// 行数恰 5 → 未省略，fence 照旧。
	kept, err := assemblePayloadFromAnnotations([]DiffAnnotationRecord{build(5)}, "")
	if err != nil {
		t.Fatalf("assemblePayload(5 lines): %v", err)
	}
	if !strings.Contains(kept.Payload, "```\nl1\nl2\nl3\nl4\nl5\n```") {
		t.Errorf("5-line range must keep fence\ngot: %s", kept.Payload)
	}
	// 行数恰 6 → 省略，仅批注头与评论（逐字）。
	omitted, err := assemblePayloadFromAnnotations([]DiffAnnotationRecord{build(6)}, "")
	if err != nil {
		t.Fatalf("assemblePayload(6 lines): %v", err)
	}
	want := fixedHeader + "\n\n## 批注\n\n" +
		`### 批注 1 — "f.go":1-6 (new，来源 index)` + "\n评论：c"
	if omitted.Payload != want {
		t.Errorf("omitted payload mismatch:\n--- got ---\n%s\n--- want ---\n%s", omitted.Payload, want)
	}
}

// TestPayloadOmissionRuneCountBoundary 验证范围文本 rune 数恰 300 保留、恰 301 省略
// （D9：字符数 > 300 时省略）。
func TestPayloadOmissionRuneCountBoundary(t *testing.T) {
	build := func(n int) DiffAnnotationRecord {
		return DiffAnnotationRecord{
			ID: "a1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1,
			SnapshotStartLine: 1, SnapshotLineCount: 1,
			Snapshot: strings.Repeat("a", n), Comment: "c", CreatedAt: 1,
		}
	}
	kept, err := assemblePayloadFromAnnotations([]DiffAnnotationRecord{build(300)}, "")
	if err != nil {
		t.Fatalf("assemblePayload(300 runes): %v", err)
	}
	if !strings.Contains(kept.Payload, "```\n"+strings.Repeat("a", 300)+"\n```") {
		t.Errorf("300-rune range must keep fence\ngot: %s", kept.Payload)
	}
	omitted, err := assemblePayloadFromAnnotations([]DiffAnnotationRecord{build(301)}, "")
	if err != nil {
		t.Fatalf("assemblePayload(301 runes): %v", err)
	}
	want := fixedHeader + "\n\n## 批注\n\n" +
		`### 批注 1 — "f.go":1 (new，来源 index)` + "\n评论：c"
	if omitted.Payload != want {
		t.Errorf("omitted payload mismatch:\n--- got ---\n%s\n--- want ---\n%s", omitted.Payload, want)
	}
}

// TestPayloadOmissionRuneCountMultibyteBoundary 验证 rune 计数按字符而非字节：
// 全角字符每 rune 3 bytes，300 runes（900 bytes）保留、301 runes 省略——若按 len() 字节计数，
// 300-rune 用例（900 bytes）将被误判省略。
func TestPayloadOmissionRuneCountMultibyteBoundary(t *testing.T) {
	build := func(n int) DiffAnnotationRecord {
		return DiffAnnotationRecord{
			ID: "a1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1,
			SnapshotStartLine: 1, SnapshotLineCount: 1,
			Snapshot: strings.Repeat("中", n), Comment: "c", CreatedAt: 1,
		}
	}
	kept, err := assemblePayloadFromAnnotations([]DiffAnnotationRecord{build(300)}, "")
	if err != nil {
		t.Fatalf("assemblePayload(300 runes): %v", err)
	}
	if !strings.Contains(kept.Payload, "```\n"+strings.Repeat("中", 300)+"\n```") {
		t.Errorf("300-rune multibyte range must keep fence (rune counting, not bytes)\ngot: %s", kept.Payload)
	}
	omitted, err := assemblePayloadFromAnnotations([]DiffAnnotationRecord{build(301)}, "")
	if err != nil {
		t.Fatalf("assemblePayload(301 runes): %v", err)
	}
	want := fixedHeader + "\n\n## 批注\n\n" +
		`### 批注 1 — "f.go":1 (new，来源 index)` + "\n评论：c"
	if omitted.Payload != want {
		t.Errorf("omitted payload mismatch:\n--- got ---\n%s\n--- want ---\n%s", omitted.Payload, want)
	}
}

// TestPayloadOmissionCountsSlicedRangeOnly 验证 rune 计数对象是切取后的引用范围文本，
// 而非整个快照窗口：窗口上下文合计 603 runes（> 300），引用切片（行 2-3）合计 201 runes
// （≤ 300）→ 保留 fence 与快照全文。
func TestPayloadOmissionCountsSlicedRangeOnly(t *testing.T) {
	snapshot := strings.Repeat("c", 200) + "\n" +
		strings.Repeat("a", 100) + "\n" +
		strings.Repeat("b", 100) + "\n" +
		strings.Repeat("d", 200)
	ann := DiffAnnotationRecord{
		ID: "a1", Path: "f.go", Side: "new", StartLine: 2, EndLine: 3,
		SnapshotStartLine: 1, SnapshotLineCount: 4, Snapshot: snapshot, Comment: "c", CreatedAt: 1,
	}
	result, err := assemblePayloadFromAnnotations([]DiffAnnotationRecord{ann}, "")
	if err != nil {
		t.Fatalf("assemblePayload: %v", err)
	}
	if !strings.Contains(result.Payload, "```") {
		t.Errorf("sliced range (201 runes) within window context (603 runes) must keep fence\ngot: %s", result.Payload)
	}
	if !strings.Contains(result.Payload, snapshot) {
		t.Errorf("unomitted block must keep full snapshot window\ngot: %s", result.Payload)
	}
}

// TestPayloadRangeTextCountsCRLF 验证范围文本切取保留 CRLF 行尾且 \r 计入 rune 数：
// 同为 2 行范围，LF 行尾合计 300 runes 保留；CRLF 行尾（\r 计入）为 301 runes 省略。
func TestPayloadRangeTextCountsCRLF(t *testing.T) {
	build := func(snapshot string) DiffAnnotationRecord {
		return DiffAnnotationRecord{
			ID: "a1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 2,
			SnapshotStartLine: 1, SnapshotLineCount: 2, Snapshot: snapshot, Comment: "c", CreatedAt: 1,
		}
	}
	lf, err := assemblePayloadFromAnnotations([]DiffAnnotationRecord{
		build(strings.Repeat("a", 150) + "\n" + strings.Repeat("a", 149)),
	}, "")
	if err != nil {
		t.Fatalf("assemblePayload(LF): %v", err)
	}
	if !strings.Contains(lf.Payload, "```") {
		t.Errorf("300-rune LF range must keep fence\ngot: %s", lf.Payload)
	}
	crlf, err := assemblePayloadFromAnnotations([]DiffAnnotationRecord{
		build(strings.Repeat("a", 150) + "\r\n" + strings.Repeat("a", 149)),
	}, "")
	if err != nil {
		t.Fatalf("assemblePayload(CRLF): %v", err)
	}
	if strings.Contains(crlf.Payload, "```") {
		t.Errorf("\\r must count toward the 300-rune limit (CRLF range omitted)\ngot: %s", crlf.Payload)
	}
}

// TestPayloadInvalidSnapshotWindowRejected 验证行偏移越界（损坏窗口）统一返回
// ErrInvalidSnapshotWindow（API 映射 invalid_input；发生在 core 大小准入之前）。
func TestPayloadInvalidSnapshotWindowRejected(t *testing.T) {
	cases := []struct {
		name string
		ann  DiffAnnotationRecord
	}{
		{"1-line range window corrupted", DiffAnnotationRecord{
			ID: "a1", Path: "f.go", Side: "new", StartLine: 5, EndLine: 5,
			SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x", Comment: "c", CreatedAt: 1,
		}},
		{"6-line range window corrupted", DiffAnnotationRecord{
			ID: "a1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 6,
			SnapshotStartLine: 1, SnapshotLineCount: 3, Snapshot: "l1\nl2\nl3", Comment: "c", CreatedAt: 1,
		}},
		{"negative offset", DiffAnnotationRecord{
			ID: "a1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1,
			SnapshotStartLine: 2, SnapshotLineCount: 1, Snapshot: "x", Comment: "c", CreatedAt: 1,
		}},
		// 窗口损坏且最终 core 同时超限：窗口校验 MUST 先于 core 大小准入，
		// 返回 ErrInvalidSnapshotWindow 而非 ErrPayloadTooLarge。
		{"corrupted window beats oversized core", DiffAnnotationRecord{
			ID: "a1", Path: "f.go", Side: "new", StartLine: 5, EndLine: 5,
			SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x",
			Comment: strings.Repeat("x", payloadMaxBytes+100), CreatedAt: 1,
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := assemblePayloadFromAnnotations([]DiffAnnotationRecord{c.ann}, "")
			if !errors.Is(err, ErrInvalidSnapshotWindow) {
				t.Fatalf("err=%v want ErrInvalidSnapshotWindow", err)
			}
		})
	}
}
