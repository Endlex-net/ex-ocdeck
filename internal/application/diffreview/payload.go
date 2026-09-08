// payload.go 实现 3.6 提交 payload 组装。
//
// 组装规则（唯一）：
//   - fixedHeader / noteSection / annotationBlock / annotationSection / core。
//   - 批注排序 created_at 升序平局 id 字典序；i 从 1 连续编号。
//   - 动态 fence（最长反引号串+1，最小 3）。
//   - 行号表述高效格式：Q(path):start-end（单行 Q(path):start）。
//   - D9 统一省略规则：窗口索引越界 → ErrInvalidSnapshotWindow；引用范围 > 5 行或 > 300 runes
//     仅输出批注头与评论（仅删 fence 段），对所有文件类型生效。
//   - 不附加相关 diff/上下文段（用户决策：批注快照自带窗口即足够，不再附加 Context 内容）。
//   - 65536 字节准入：len(core) > 65536 → ErrPayloadTooLarge（零副作用）。
package diffreview

import (
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// payloadMaxBytes 体积阈值（65536 字节）。
const payloadMaxBytes = 65536

// fixedHeader 固定指令头（逐字）。
const fixedHeader = "以下是代码 review 批注，请在当前 worktree 中逐条修复。修复前先阅读相关代码，保持最小改动。"

// sourceTriple 为 diff 来源三元组（path, ref, untracked），来源标识依据。
type sourceTriple struct {
	Path      string
	Ref       string
	Untracked bool
}

// payloadResult 为组装结果。
type payloadResult struct {
	Payload   string
	Truncated bool
}

// assemblePayloadFromAnnotations 为提交用例的组装入口：接受批注记录（含 created_at）+ note。
// 批注排序（created_at 升序平局 id 字典序）在此完成。不读取 diff、不附加相关 diff 段。
func assemblePayloadFromAnnotations(anns []DiffAnnotationRecord, note string) (payloadResult, error) {
	sorted := sortAnnotations(anns)
	items := make([]DiffReviewSubmissionItemRecord, len(sorted))
	for i, a := range sorted {
		items[i] = DiffReviewSubmissionItemRecord{
			AnnotationID:       a.ID,
			AnnotationRevision: a.Revision,
			Path:               a.Path,
			Side:               a.Side,
			Ref:                a.Ref,
			Untracked:          a.Untracked,
			StartLine:          a.StartLine,
			EndLine:            a.EndLine,
			SnapshotStartLine:  a.SnapshotStartLine,
			Snapshot:           a.Snapshot,
			Comment:            a.Comment,
		}
	}
	return assemblePayload(items, note)
}

// assemblePayload 组装提交 payload（唯一规则：core = Join([fixedHeader, noteSection?, annotationSection], "\n\n")）。
// D9：批注窗口校验（ErrInvalidSnapshotWindow）先于 core 大小准入。准入：len(core) > payloadMaxBytes
// → ErrPayloadTooLarge（零副作用）。Truncated 恒 false（无相关 diff 段，不产生预算截断；
// DTO 字段保留 wire 兼容）。
func assemblePayload(items []DiffReviewSubmissionItemRecord, note string) (payloadResult, error) {
	section, err := buildAnnotationSection(items)
	if err != nil {
		return payloadResult{}, err
	}
	core := buildCore(note, section)
	if len(core) > payloadMaxBytes {
		return payloadResult{}, ErrPayloadTooLarge
	}
	return payloadResult{Payload: core, Truncated: false}, nil
}

// sortAnnotations 按批注排序键排序（created_at 升序平局 id 字典序）。
func sortAnnotations(anns []DiffAnnotationRecord) []DiffAnnotationRecord {
	out := make([]DiffAnnotationRecord, len(anns))
	copy(out, anns)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// buildCore 组装核心区（Join([fixedHeader, noteSection?, annotationSection], "\n\n")）。
func buildCore(note, annotationSection string) string {
	parts := []string{fixedHeader}
	if note != "" {
		parts = append(parts, "## 补充说明"+"\n"+note)
	}
	parts = append(parts, annotationSection)
	return strings.Join(parts, "\n\n")
}

// buildAnnotationSection 组装批注节（annotationSection = "## 批注" + "\n\n" + Join(annotationBlocks, "\n\n")）。
// items 必须已按批注排序键排序；i 从 1 连续编号。任一批注块组装失败（窗口不自洽）即整体失败。
func buildAnnotationSection(items []DiffReviewSubmissionItemRecord) (string, error) {
	blocks := make([]string, len(items))
	for i, it := range items {
		b, err := buildAnnotationBlock(i+1, it)
		if err != nil {
			return "", err
		}
		blocks[i] = b
	}
	return "## 批注" + "\n\n" + strings.Join(blocks, "\n\n"), nil
}

// buildAnnotationBlock 组装单条批注块（逐字公式）。
// annotationBlock(i) = "### 批注 " + i + " — " + Q(path) + ":" + range + " (" + side + "，来源 " + source + ")"
//
//   - "\n" + "评论：" + comment + "\n" + fence + "\n" + snapshot + "\n" + fence
//
// 行号表述（用户决策，高效格式）：Q(path):start-end；单行 Q(path):start。
// D9 统一省略规则：每条批注先无条件校验窗口索引 lo = startLine - snapshotStartLine、
// hi = endLine - snapshotStartLine + 1（0 <= lo <= hi <= len(Split(snapshot, "\n"))），
// 越界返回 ErrInvalidSnapshotWindow；引用范围行数 > 5 或范围文本 rune 数 > 300
// 时仅输出批注头与评论（仅删 fence 段，头与评论逐字不变）。
func buildAnnotationBlock(i int, it DiffReviewSubmissionItemRecord) (string, error) {
	lines := strings.Split(it.Snapshot, "\n")
	lo := it.StartLine - it.SnapshotStartLine
	hi := it.EndLine - it.SnapshotStartLine + 1
	if lo < 0 || lo > hi || hi > len(lines) {
		return "", ErrInvalidSnapshotWindow
	}
	source := sourceLabel(it.Ref, it.Untracked)
	rng := rangeLabel(it.StartLine, it.EndLine)
	head := "### 批注 " + strconv.Itoa(i) + " — " + strconv.Quote(it.Path) + ":" + rng +
		" (" + it.Side + "，来源 " + source + ")" +
		"\n" + "评论：" + it.Comment
	// 引用范围行数（1-based 闭区间）；范围文本按窗口切取后以 "\n" Join 计 rune
	// （内部 \n 与 CRLF 的 \r 均计入，末行后不额外计分隔符）。
	if it.EndLine-it.StartLine+1 > 5 ||
		utf8.RuneCountInString(strings.Join(lines[lo:hi], "\n")) > 300 {
		return head, nil
	}
	fence := dynamicFence(it.Snapshot)
	return head + "\n" + fence + "\n" + it.Snapshot + "\n" + fence, nil
}

// sourceLabel 返回来源标签（untracked→"untracked"; ref非空→Q(ref); 否则→"index"）。
func sourceLabel(ref string, untracked bool) string {
	if untracked {
		return "untracked"
	}
	if ref != "" {
		return strconv.Quote(ref)
	}
	return "index"
}

// rangeLabel 返回行范围标签（高效格式：单行 "start"，多行 "start-end"）。
func rangeLabel(start, end int) string {
	if start == end {
		return strconv.Itoa(start)
	}
	return strconv.Itoa(start) + "-" + strconv.Itoa(end)
}

// dynamicFence 计算动态反引号 fence（fence 规则）。
// 长度 = 被包围内容中最长反引号串 + 1（最小 3）。
func dynamicFence(content string) string {
	maxRun := 0
	run := 0
	for _, c := range content {
		if c == '`' {
			run++
			if run > maxRun {
				maxRun = run
			}
		} else {
			run = 0
		}
	}
	n := maxRun + 1
	if n < 3 {
		n = 3
	}
	return strings.Repeat("`", n)
}

// runeSafePrefix 返回 s 的不超过 maxBytes 字节的最大 rune-safe 前缀（scheduler 错误截断复用）。
func runeSafePrefix(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// 截断到不超过 maxBytes 字节的 rune 边界。
	end := maxBytes
	for end > 0 && !isRuneStart(s, end) {
		end--
	}
	return s[:end]
}

// isRuneStart 判断 s[pos] 是否为 UTF-8 rune 起始字节（非 continuation byte 0b10xxxxxx）。
func isRuneStart(s string, pos int) bool {
	if pos < 0 || pos >= len(s) {
		return false
	}
	return s[pos]&0xC0 != 0x80
}
