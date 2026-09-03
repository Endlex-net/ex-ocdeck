// payload_bench_test.go 非门禁诊断 benchmark（design.md D9 / tasks.md 6.3）。
//
// 不设耗时阈值：结果记入测试输出；实测明显异常时回设计评审。
package diffreview

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// benchSideMaxBytes 单侧内容规模（design.md D9：单侧可达 512KiB = 524288）。
const benchSideMaxBytes = 512 * 1024

// uniqueShortLineBytes 最坏输入每行字节数（8 位十进制 + '\n'）。
const uniqueShortLineBytes = 9

// multiSourceBenchCount 多来源内存诊断的来源数（tasks.md 6.3：如 50 来源）。
const multiSourceBenchCount = 50

var (
	worstCaseOnce sync.Once
	worstCaseOld  string
	worstCaseNew  string
)

// worstCaseUniqueSides 构造 SequenceMatcher 最坏输入：两侧均为大量唯一短行，
// 新侧为旧侧逆序（无连续匹配块，findLongestMatch 反复全区间扫描）。
func worstCaseUniqueSides() (oldContent, newContent string) {
	worstCaseOnce.Do(func() {
		n := benchSideMaxBytes / uniqueShortLineBytes
		worstCaseOld = uniqueShortLines(0, n)
		worstCaseNew = uniqueShortLinesReversed(n)
	})
	return worstCaseOld, worstCaseNew
}

func uniqueShortLines(start, n int) string {
	var b strings.Builder
	b.Grow(n * uniqueShortLineBytes)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "%08d\n", start+i)
	}
	return b.String()
}

func uniqueShortLinesReversed(n int) string {
	var b strings.Builder
	b.Grow(n * uniqueShortLineBytes)
	for i := n - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "%08d\n", i)
	}
	return b.String()
}

func benchAnnotation(id, path string, createdAt int64) DiffAnnotationRecord {
	return DiffAnnotationRecord{
		ID: id, Path: path, Side: "new",
		StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1,
		Snapshot: "x\n", Comment: "c", CreatedAt: createdAt,
	}
}

func benchDiffResult(oldContent, newContent string) DiffSourceResult {
	return DiffSourceResult{
		OldContent: oldContent,
		NewContent: newContent,
		OldExists:  true,
		NewExists:  true,
		OldMode:    "100644",
		NewMode:    "100644",
	}
}

// BenchmarkDiffPayloadWorstCase 单来源最坏输入组装耗时（design.md D9 命名）。
// 两侧各接近 512KiB 唯一短行，走真实 assemblePayloadFromAnnotations。
func BenchmarkDiffPayloadWorstCase(b *testing.B) {
	oldContent, newContent := worstCaseUniqueSides()
	anns := []DiffAnnotationRecord{benchAnnotation("a1", "worst.go", 1)}
	diffRead := func(sourceTriple) (DiffSourceResult, error) {
		return benchDiffResult(oldContent, newContent), nil
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := assemblePayloadFromAnnotations(anns, "", diffRead); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDiffPayloadMultiSourceMemory 多来源内存行为（D7 有界单遍 vs 旧两阶段全量峰值）。
// 50 来源各持接近 512KiB 双侧最坏输入；预算耗尽后停 formatter，继续读取汇总。
func BenchmarkDiffPayloadMultiSourceMemory(b *testing.B) {
	oldContent, newContent := worstCaseUniqueSides()
	anns := make([]DiffAnnotationRecord, multiSourceBenchCount)
	for i := 0; i < multiSourceBenchCount; i++ {
		anns[i] = benchAnnotation(fmt.Sprintf("a%d", i), fmt.Sprintf("f%02d.go", i), int64(i+1))
	}
	diffRead := func(sourceTriple) (DiffSourceResult, error) {
		return benchDiffResult(oldContent, newContent), nil
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := assemblePayloadFromAnnotations(anns, "", diffRead); err != nil {
			b.Fatal(err)
		}
	}
}
