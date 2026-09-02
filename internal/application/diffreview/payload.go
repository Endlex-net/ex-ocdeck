// payload.go 实现 3.6 提交 payload 组装（design.md D7 逐字公式）。
//
// 组装规则（D7 唯一）：
//   - fixedHeader / noteSection / annotationBlock / annotationSection / core / sourceSegment。
//   - 三元组排序 path→ref→untracked(false<true)。
//   - 批注排序 created_at 升序平局 id 字典序；i 从 1 连续编号。
//   - 动态 fence（最长反引号串+1，最小 3）。
//   - 八字段→组合映射（元数据控制行 + 恰好一个内容形态块）。
//   - go-difflib v1.0.0 wrapper 四规则 + EOF 控制行规则④。
//   - 65536 字节准入 core=Join([头,note?,批注节,相关diff标题],"\n\n")+markerSuffix。
//   - 有界单遍算法（逐来源读取→汇总→预算开放即格式化入有界 builder 读完即弃→预算耗尽停 formatter 继续读取→末断言）。
//   - 截断公式 sep/room/rune-safe prefix。
package diffreview

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
)

// payloadMaxBytes 体积阈值（design.md D7：65536 字节）。
const payloadMaxBytes = 65536

// markerSuffix 截断标记行（design.md D7："\n\n...[diff 已截断]..."）。
const markerSuffix = "\n\n...[diff 已截断]..."

// fixedHeader 固定指令头（design.md D7 逐字）。
const fixedHeader = "以下是代码 review 批注，请在当前 worktree 中逐条修复。修复前先阅读相关代码，保持最小改动。"

// sourceTriple 为 diff 来源三元组（path, ref, untracked），去重与排序依据（design.md D7）。
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

// assemblePayload 组装提交 payload（design.md D7 有界单遍算法）。
//
// 参数：
//   - items：批注快照条目（已按准入校验通过，含 id/revision/path/side/ref/untracked/行范围/快照/评论/created_at）。
//   - note：用户补充说明（可空）。
//   - diffReadAll：由调用方提供的批量 diff 读取闭包（F5：单次任务锁内逐来源读取）。
//     签名：传入排序后的来源列表 + 逐来源回调；adapter 在锁内逐来源读取并调用回调，
//     回调返回 error 立即中止（多源失败返回首个排序错误，D7）。读取错误透传（invalid_state/git_error/internal）。
//     调用方经 DiffSourcePort.ReadLocked 实现，单次 tryLockTask 全程持锁。
//
// 返回：
//   - payload 组装结果（payload 文本 + truncated 真值）。
//   - error：GitDiff 读取失败（整体失败零副作用）、核心区超阈值（ErrPayloadTooLarge）。
//
// 有界单遍算法（D7 唯一）：按三元组排序逐来源——读取→记录首个排序 error 及 truncated 汇总→
// 预算开放时立即格式化进入 ≤65536 有界 builder（读完即弃）→预算耗尽停 formatter 继续读取汇总→末断言。
//
// F7 字节契约修正：
//   - 分别计算逻辑完整候选与截断输出预算，不在非截断路径预留 markerSuffix。
//   - core 与首个 sourceRegion 间 MUST 有 \n\n 分隔（首个完整来源前补 sep）。
//   - 后续来源溢出时可用容量扣除 builder 当前已写长度（avail = budget - builder.Len()）。
//   - 预算 = payloadMaxBytes - len(core) - len(markerSuffix)（截断标记在截断时追加）。
func assemblePayload(items []DiffReviewSubmissionItemRecord, note string, diffReadAll func(srcs []sourceTriple, onSource func(src sourceTriple, result DiffSourceResult, err error) error) error) (payloadResult, error) {
	sources := collectSources(items)
	sortedSources := sortSources(sources)

	// 组装核心区（不含 sourceRegion，含固定头/note/批注节/相关diff标题）。
	annotationSection := buildAnnotationSection(items)
	relatedDiffTitle := ""
	if len(sortedSources) > 0 {
		relatedDiffTitle = "## 相关 diff"
	}
	core := buildCore(note, annotationSection, relatedDiffTitle)

	// 准入：len(core)+len(markerSuffix) > 65536 → invalid_input（零副作用，D7）。
	// 保证截断标记永远放得下。
	if len(core)+len(markerSuffix) > payloadMaxBytes {
		return payloadResult{}, ErrPayloadTooLarge
	}

	// 有界单遍：逐来源读取→汇总→格式化入有界 builder。
	// builder 仅含 sourceRegion（各 sourceSegment 及其分隔 \n\n），最终拼接 core + builder.String()。
	var builder strings.Builder
	builder.Grow(payloadMaxBytes)
	sep := "\n\n"
	budgetOpen := true // 预算开放时可格式化来源
	truncated := false // 预算截断标记
	anyGitTruncated := false
	var firstErr error
	var readErr error // diffReadAll 自身返回的锁/校验错误

	// F7/D7：逻辑候选预算 = payloadMaxBytes - len(core)（未超限则完整输出，MUST NOT 附 marker）。
	// 仅首次真实超限时收缩到截断预算 room = payloadMaxBytes - len(core) - len(markerSuffix)
	//（保留 markerSuffix 空间），追加 markerSuffix。
	candidateBudget := payloadMaxBytes - len(core)
	room := payloadMaxBytes - len(core) - len(markerSuffix)

	// onSource 为逐来源回调：在锁内由 adapter 逐来源读取后调用，执行格式化与汇总。
	onSource := func(src sourceTriple, result DiffSourceResult, err error) error {
		if err != nil {
			// 记录首个排序 error（多来源同时失败返回排序最前来源错误，D7）。
			if firstErr == nil {
				firstErr = err
			}
			// 读取失败仍继续下一来源汇总（D7：后续来源仍须读取以汇总 error/truncated）。
			return nil
		}
		if result.Truncated {
			anyGitTruncated = true
		}
		if !budgetOpen {
			// 预算耗尽：停止格式化，继续读取汇总（formatter 不调用）。
			return nil
		}
		seg := buildSourceSegment(src, result)
		// F7：用逻辑候选预算判定首次真实超限。未超限则完整写入（MUST NOT 附 marker）。
		if builder.Len()+len(sep)+len(seg) > candidateBudget {
			// 预算截断：收缩已有 sourceRegion + 当前来源到截断预算 room，追加 markerSuffix。
			// F18/D7：禁止完整物化 sourceRegion（builder.String() + sep + seg 可远超 65536，违反
			// D7 单个有界 builder）。改用「已有内容 + 当前 sep+seg」为逻辑前缀统一 rune-safe 收缩到 room，
			// 不构造 fullRegion 字符串。
			budgetOpen = false
			truncated = true
			// F7 边界：room <= len(sep) → 清空 source region（D7：仅 core+markerSuffix，不残留 \n）。
			if room <= len(sep) {
				builder.Reset()
				return nil
			}
			// 收缩到截断预算 room（core + room <= 65536 - marker，保证 markerSuffix 放得下）。
			// 已有内容 > room → rune-safe 原地缩短已有 builder（不追加 sep+seg）。
			// 已有内容 <= room → 追加当前 sep+seg 的可用 rune-safe 前缀（avail = room - builder.Len()）。
			existing := builder.Len()
			if existing > room {
				// 已有内容已超 room：原地缩短 builder 到 room 字节的 rune-safe 前缀。
				prefix := runeSafePrefix(builder.String(), room)
				builder.Reset()
				builder.WriteString(prefix)
			} else {
				// 已有内容 <= room：追加 sep + seg 的可用 rune-safe 前缀。
				avail := room - existing
				if avail >= len(sep) {
					// 先写 sep，再写 seg 的剩余可用 rune-safe 前缀。
					segAvail := avail - len(sep)
					segPrefix := runeSafePrefix(seg, segAvail)
					builder.WriteString(sep)
					builder.WriteString(segPrefix)
				} else if avail > 0 {
					// 不足以放完整 sep：写 sep 的 rune-safe 前缀（不写 seg）。
					builder.WriteString(runeSafePrefix(sep, avail))
				}
			}
			return nil
		}
		// 预算开放：写入 sep（core 与 sourceRegion 间及来源间分隔，F7：首个来源前也补 sep）+ 完整 sourceSegment。
		builder.WriteString(sep)
		builder.WriteString(seg)
		return nil
	}

	// 调用批量读取闭包（F5：单次任务锁内逐来源读取，onSource 回调完成格式化）。
	readErr = diffReadAll(sortedSources, onSource)

	// diffReadAll 自身的锁/校验错误（非来源读取错误）优先返回。
	if readErr != nil {
		return payloadResult{}, readErr
	}

	// 全部读取结束：存在任何来源 error → 丢弃候选 payload、整体失败（D7）。
	if firstErr != nil {
		return payloadResult{}, firstErr
	}

	// 组装最终 payload。
	var payload string
	if builder.Len() > 0 {
		// 有 sourceRegion → core + sourceRegion（+ markerSuffix 截断时）。
		payload = core + builder.String()
		if truncated {
			payload = payload + markerSuffix
		}
	} else {
		// 无 sourceRegion（无相关 diff 来源或首来源即超 room 且 room<=len(sep)）→ 仅 core。
		// 截断但无 sourceRegion 时仍需 markerSuffix。
		payload = core
		if truncated {
			payload = payload + markerSuffix
		}
	}

	// truncated 真值表（D7）：任一来源 GitDiff.truncated || payload 因 65536 预算被截断。
	truncated = truncated || anyGitTruncated

	// 组装完成 MUST 断言 payload ≤ 65536（D7）。
	if len(payload) > payloadMaxBytes {
		return payloadResult{}, fmt.Errorf("diffreview: payload assertion failed: %d > %d", len(payload), payloadMaxBytes)
	}
	return payloadResult{Payload: payload, Truncated: truncated}, nil
}

// assemblePayloadFromAnnotations 为提交用例的组装入口：接受批注记录（含 created_at）+ note + 单来源 diff 读取闭包。
// 批注排序（created_at 升序平局 id 字典序）在此完成，随后委托 assemblePayload。
// diffRead 为单来源读取闭包（兼容既有测试与不走 ReadLocked 的场景），内部包装为批量签名。
func assemblePayloadFromAnnotations(anns []DiffAnnotationRecord, note string, diffRead func(src sourceTriple) (DiffSourceResult, error)) (payloadResult, error) {
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
	diffReadAll := func(srcs []sourceTriple, onSource func(src sourceTriple, result DiffSourceResult, err error) error) error {
		for _, src := range srcs {
			result, err := diffRead(src)
			if cerr := onSource(src, result, err); cerr != nil {
				return cerr
			}
			// F14/D7：来源读取失败不短路——错误已交回调汇总，继续遍历剩余来源。
		}
		return nil
	}
	return assemblePayload(items, note, diffReadAll)
}

// assemblePayloadFromAnnotationsLocked 为 F5 提交用例的锁作用域组装入口：接受批量 diff 读取闭包。
// 与 assemblePayloadFromAnnotations 区别：diffReadAll 由调用方经 DiffSourcePort.ReadLocked 实现，
// 单次 tryLockTask 全程持锁，回调内逐来源格式化（禁止递归加锁）。
func assemblePayloadFromAnnotationsLocked(anns []DiffAnnotationRecord, note string, diffReadAll func(srcs []sourceTriple, onSource func(src sourceTriple, result DiffSourceResult, err error) error) error) (payloadResult, error) {
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
	return assemblePayload(items, note, diffReadAll)
}

// collectSources 从批注条目收集去重的来源三元组（design.md D7：以三元组去重）。
func collectSources(items []DiffReviewSubmissionItemRecord) []sourceTriple {
	seen := map[sourceTriple]bool{}
	var sources []sourceTriple
	for _, it := range items {
		t := sourceTriple{Path: it.Path, Ref: it.Ref, Untracked: it.Untracked}
		if !seen[t] {
			seen[t] = true
			sources = append(sources, t)
		}
	}
	return sources
}

// sortSources 按三元组字典序排序（path→ref→untracked(false<true)，D7）。
func sortSources(sources []sourceTriple) []sourceTriple {
	out := make([]sourceTriple, len(sources))
	copy(out, sources)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		if out[i].Ref != out[j].Ref {
			return out[i].Ref < out[j].Ref
		}
		// untracked: false < true
		return !out[i].Untracked && out[j].Untracked
	})
	return out
}

// sortAnnotations 按批注排序键排序（created_at 升序平局 id 字典序，D7）。
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

// buildCore 组装核心区（design.md D7：Join([fixedHeader, noteSection?, annotationSection, relatedDiffTitle?], "\n\n")）。
func buildCore(note, annotationSection, relatedDiffTitle string) string {
	parts := []string{fixedHeader}
	if note != "" {
		parts = append(parts, "## 补充说明"+"\n"+note)
	}
	parts = append(parts, annotationSection)
	if relatedDiffTitle != "" {
		parts = append(parts, relatedDiffTitle)
	}
	return strings.Join(parts, "\n\n")
}

// buildAnnotationSection 组装批注节（design.md D7：annotationSection = "## 批注" + "\n\n" + Join(annotationBlocks, "\n\n")）。
// items 必须已按批注排序键排序；i 从 1 连续编号。
func buildAnnotationSection(items []DiffReviewSubmissionItemRecord) string {
	blocks := make([]string, len(items))
	for i, it := range items {
		blocks[i] = buildAnnotationBlock(i+1, it)
	}
	return "## 批注" + "\n\n" + strings.Join(blocks, "\n\n")
}

// buildAnnotationBlock 组装单条批注块（design.md D7 逐字公式）。
// annotationBlock(i) = "### 批注 " + i + " — " + Q(path) + " (" + side + "，来源 " + source + "，行 " + range + ")"
//
//   - "\n" + "评论：" + comment + "\n" + fence + "\n" + snapshot + "\n" + fence
func buildAnnotationBlock(i int, it DiffReviewSubmissionItemRecord) string {
	source := sourceLabel(it.Ref, it.Untracked)
	rng := rangeLabel(it.StartLine, it.EndLine)
	fence := dynamicFence(it.Snapshot)
	return "### 批注 " + strconv.Itoa(i) + " — " + strconv.Quote(it.Path) +
		" (" + it.Side + "，来源 " + source + "，行 " + rng + ")" +
		"\n" + "评论：" + it.Comment +
		"\n" + fence + "\n" + it.Snapshot + "\n" + fence
}

// buildSourceSegment 组装单来源段（design.md D7：sourceSegment = "### " + Q(path) + "（来源 " + source + "）" + "\n" + Join(bodyLines, "\n")）。
// bodyLines = 元数据控制行列表 + [内容形态块]。
func buildSourceSegment(src sourceTriple, result DiffSourceResult) string {
	source := sourceLabel(src.Ref, src.Untracked)
	header := "### " + strconv.Quote(src.Path) + "（来源 " + source + "）"

	var bodyLines []string
	// 元数据控制行①：双侧存在且 oldMode≠newMode → "# mode change: <oldMode> -> <newMode>"
	if result.OldExists && result.NewExists && result.OldMode != result.NewMode {
		bodyLines = append(bodyLines, "# mode change: "+result.OldMode+" -> "+result.NewMode)
	}
	// 元数据控制行②：聚合 truncated=true → "# truncated: content is a bounded prefix"
	if result.Truncated {
		bodyLines = append(bodyLines, "# truncated: content is a bounded prefix")
	}
	// 内容形态块（恰好一条）
	bodyLines = append(bodyLines, contentShapeBlock(src.Path, result))

	return header + "\n" + strings.Join(bodyLines, "\n")
}

// contentShapeBlock 返回内容形态块（design.md D7 八字段→组合映射，恰好命中一条）。
func contentShapeBlock(path string, r DiffSourceResult) string {
	// isBinary=true → "# binary: content unavailable"
	if r.IsBinary {
		return "# binary: content unavailable"
	}
	// 任一侧 mode=160000 → "# gitlink: <oldContent> -> <newContent>"
	if r.OldMode == "160000" || r.NewMode == "160000" {
		return "# gitlink: " + r.OldContent + " -> " + r.NewContent
	}
	// 双侧不存在 → "# file missing on both sides"
	if !r.OldExists && !r.NewExists {
		return "# file missing on both sides"
	}
	// !oldExists && newExists 且两侧内容均空 → "# empty file added"
	if !r.OldExists && r.NewExists && r.OldContent == "" && r.NewContent == "" {
		return "# empty file added"
	}
	// oldExists && !newExists 且两侧内容均空 → "# empty file deleted"
	if r.OldExists && !r.NewExists && r.OldContent == "" && r.NewContent == "" {
		return "# empty file deleted"
	}
	// 其余 → unified diff（formatter 规则）
	return unifiedDiffBlock(path, r)
}

// unifiedDiffBlock 生成 unified diff 文本（design.md D7 formatter 契约 + wrapper 四规则）。
func unifiedDiffBlock(path string, r DiffSourceResult) string {
	a := splitForDiff(r.OldContent, r.OldExists)
	b := splitForDiff(r.NewContent, r.NewExists)
	aMissingFinal := missingFinalNewline(r.OldContent, r.OldExists)
	bMissingFinal := missingFinalNewline(r.NewContent, r.NewExists)

	// 规则③：仅对库输入给最后一行补 \n（missingFinalNewline 侧）。
	if aMissingFinal && len(a) > 0 {
		a[len(a)-1] = a[len(a)-1] + "\n"
	}
	if bMissingFinal && len(b) > 0 {
		b[len(b)-1] = b[len(b)-1] + "\n"
	}

	diff := difflib.UnifiedDiff{
		A:        a,
		B:        b,
		FromFile: strconv.Quote("a/" + path),
		ToFile:   strconv.Quote("b/" + path),
		Context:  3,
	}
	out, err := difflib.GetUnifiedDiffString(diff)
	if err != nil {
		// difflib GetUnifiedDiffString 仅在 writer 写入失败时返回 error（bytes.Buffer 不会失败）。
		// 不该发生；若发生视为无可见变更。
		return "# no visible change"
	}

	// 规则④：库输出处理。
	if out != "" {
		// 库输出非空 → 每个 true 侧在 unified 段末尾各追加一条 EOF 控制行。
		var suffix strings.Builder
		if aMissingFinal {
			suffix.WriteString("\n\\ No newline at end of file (a side)")
		}
		if bMissingFinal {
			suffix.WriteString("\n\\ No newline at end of file (b side)")
		}
		return out + suffix.String()
	}
	// 库输出空串（无 diff group）→ 仅当两侧 flag 不同才生成 quoted headers + true 侧 EOF 行。
	if aMissingFinal != bMissingFinal {
		var sb strings.Builder
		sb.WriteString("--- " + strconv.Quote("a/"+path) + "\n")
		sb.WriteString("+++ " + strconv.Quote("b/"+path) + "\n")
		if aMissingFinal {
			sb.WriteString("\\ No newline at end of file (a side)")
		}
		if bMissingFinal {
			sb.WriteString("\\ No newline at end of file (b side)")
		}
		return sb.String()
	}
	// 库空串且两侧 flag 相同 → "# no visible change"
	return "# no visible change"
}

// splitForDiff 为 difflib 输入分行（design.md D7 wrapper 规则①②）。
// 空串/不存在侧 → 空 slice。SplitAfter 后丢弃末尾空元素（D7 line 228 唯一规则）。
// 注意：D7 formatter 的 SplitAfter 规则与 D4 stale 行模型（CM split('\n')）不同——
// D7 只管 payload 格式化（difflib 输入），D4 管快照/stale 行计数。
func splitForDiff(content string, exists bool) []string {
	if !exists || content == "" {
		return nil
	}
	return splitAfterDropTrailingEmpty(content)
}

// splitAfterDropTrailingEmpty 为 D7 formatter 行模型：SplitAfter(s, "\n") 后丢弃末尾空元素。
//   - "a\n" → ["a\n"]（SplitAfter ["a\n",""] 丢弃末 ""）
//   - "a"   → ["a"]
//   - "a\nb" → ["a\n","b"]
func splitAfterDropTrailingEmpty(s string) []string {
	parts := strings.SplitAfter(s, "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// missingFinalNewline 判断侧是否缺末尾换行（design.md D7 wrapper 规则③）。
// missingFinalNewline = content != "" && !HasSuffix(content, "\n")（空串/不存在侧恒 false）。
func missingFinalNewline(content string, exists bool) bool {
	return exists && content != "" && !strings.HasSuffix(content, "\n")
}

// sourceLabel 返回来源标签（design.md D7：untracked→"untracked"; ref非空→Q(ref); 否则→"index"）。
func sourceLabel(ref string, untracked bool) string {
	if untracked {
		return "untracked"
	}
	if ref != "" {
		return strconv.Quote(ref)
	}
	return "index"
}

// rangeLabel 返回行范围标签（design.md D7：单行 L<n>，多行 L<start>-L<end>）。
func rangeLabel(start, end int) string {
	if start == end {
		return "L" + strconv.Itoa(start)
	}
	return "L" + strconv.Itoa(start) + "-L" + strconv.Itoa(end)
}

// dynamicFence 计算动态反引号 fence（design.md D7 fence 规则）。
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

// runeSafePrefix 返回 s 的不超过 maxBytes 字节的最大 rune-safe 前缀（design.md D7 截断公式）。
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
