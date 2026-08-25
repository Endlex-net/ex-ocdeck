package runtime

import (
	"sync"
	"testing"
)

// TestNewInstVersion_Format 验证令牌格式 `<13 位 ms>-<hex 后缀>`（design.md:70）。
func TestNewInstVersion_Format(t *testing.T) {
	r := New()
	v := r.NewInstVersion("t1")
	s := string(v)
	if len(s) <= 14 || s[13] != '-' {
		t.Fatalf("token %q MUST be <13-digit ms>-<hex suffix>", s)
	}
	for i, c := range s[:13] {
		if c < '0' || c > '9' {
			t.Fatalf("token %q first 13 runes MUST be digits (pos %d = %q)", s, i, c)
		}
	}
	for _, c := range s[14:] {
		ok := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
		if !ok {
			t.Fatalf("token %q suffix MUST be lowercase hex, got %q", s, c)
		}
	}
}

// TestNewInstVersion_UniqueAcrossRapidSuccessiveAllocations 验证连续快速分配
//（含同毫秒）不撞值：issued 集合使唯一性成为确定性保证（design.md:70 随机后缀 +
// issued 复检，非概率保证），任意单次运行 MUST 通过。
func TestNewInstVersion_UniqueAcrossRapidSuccessiveAllocations(t *testing.T) {
	r := New()
	const n = 1000
	seen := make(map[InstVersion]bool, n)
	for i := 0; i < n; i++ {
		v := r.NewInstVersion("t1")
		if seen[v] {
			t.Fatalf("duplicate instVersion %q within %d rapid allocations", v, n)
		}
		seen[v] = true
	}
}

// TestNewInstVersion_TombstoneRetainsLastTokenAfterClearSim 验证 clear-sim（无 prevGen
// 参数，直接再分配）后 tombstone 仍为最近一次分配的令牌（B4：清理后保留）。
func TestNewInstVersion_TombstoneRetainsLastTokenAfterClearSim(t *testing.T) {
	r := New()
	r.NewInstVersion("t1")
	// 模拟 clearRuntime：runtime 移除。P1.4.9 后分配不依赖 prev 状态，直接再分配。
	v2 := r.NewInstVersion("t1")
	if v2 == "" {
		t.Fatal("allocated token must be non-empty")
	}
	tomb, ok := r.Tombstone("t1")
	if !ok || tomb != v2 {
		t.Fatalf("Tombstone = %q (ok=%v), want %q (last allocated)", tomb, ok, v2)
	}
}

// TestNewInstVersion_IndependentTasks 验证不同 taskID 的令牌互不影响（tombstone 按
// taskID 独立）。
func TestNewInstVersion_IndependentTasks(t *testing.T) {
	r := New()
	vA := r.NewInstVersion("a")
	vB := r.NewInstVersion("b")
	if vA == vB {
		t.Fatalf("independent tasks allocated identical tokens %q", vA)
	}
	vA2 := r.NewInstVersion("a")
	if vA2 == vA {
		t.Fatalf("re-allocation for task a must differ, both %q", vA)
	}
	if tomb, ok := r.Tombstone("b"); !ok || tomb != vB {
		t.Errorf("b tombstone = %q (ok=%v), want %q (independent)", tomb, ok, vB)
	}
}

// TestNewInstVersion_ConcurrentAllUnique 验证并发分配全部唯一且 genMu 并发安全
//（issued 集合确定性保证，非概率保证）。
func TestNewInstVersion_ConcurrentAllUnique(t *testing.T) {
	r := New()
	const goroutines = 50
	const perG = 20
	var mu sync.Mutex
	seen := make(map[InstVersion]bool, goroutines*perG)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				v := r.NewInstVersion("shared")
				mu.Lock()
				if seen[v] {
					t.Errorf("duplicate instVersion %q in concurrent allocation", v)
				}
				seen[v] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != goroutines*perG {
		t.Fatalf("allocated %d unique tokens, want %d", len(seen), goroutines*perG)
	}
	// tombstone 为最后一次分配的令牌之一（等值域内任一即可：必在集合中）。
	tomb, ok := r.Tombstone("shared")
	if !ok || !seen[tomb] {
		t.Fatalf("tombstone %q (ok=%v) must be one of allocated tokens", tomb, ok)
	}
}

// TestTombstone_NoTask 验证未分配 taskID 无 tombstone（found=false）。
func TestTombstone_NoTask(t *testing.T) {
	r := New()
	if _, ok := r.Tombstone("nonexistent"); ok {
		t.Error("Tombstone(nonexistent) must be found=false (never allocated)")
	}
}

// TestNewInstVersion_RejectsCandidateEqualToHistoricalToken 定向复现候选碰撞场景
//（oracle BLOCKED 根因）：分配 A → 分配 B（tombstone 推进到 B）→ 第三次分配的候选
// 先返回历史令牌 A。旧实现仅对照当前 tombstone 会误接受 A；issued 集合 MUST 拒绝 A
// 并重生成取新值 C。经 genValueFn 接缝脚本化候选序列，断言 NewInstVersion 的对外
// 行为（返回值与 tombstone），不测内部状态。
func TestNewInstVersion_RejectsCandidateEqualToHistoricalToken(t *testing.T) {
	r := New()
	const (
		tokA = InstVersion("01724000000123-aaaaaa")
		tokB = InstVersion("01724000000123-bbbbbb")
		tokC = InstVersion("01724000000123-cccccc")
	)
	// 扁平候选流：第 1 次分配取 A；第 2 次取 B；第 3 次先撞历史 A（须被拒绝）再取 C。
	queue := []InstVersion{tokA, tokB, tokA, tokC}
	r.genValueFn = func() InstVersion {
		v := queue[0]
		queue = queue[1:]
		return v
	}

	if v := r.NewInstVersion("t1"); v != tokA {
		t.Fatalf("first allocation = %q, want %q", v, tokA)
	}
	if v := r.NewInstVersion("t1"); v != tokB {
		t.Fatalf("second allocation = %q, want %q (tombstone moved)", v, tokB)
	}
	v3 := r.NewInstVersion("t1")
	if v3 != tokC {
		t.Fatalf("third allocation = %q, want %q (candidate equal to historical token A MUST be rejected and regenerated)", v3, tokC)
	}
	if tomb, ok := r.Tombstone("t1"); !ok || tomb != tokC {
		t.Fatalf("Tombstone = %q (ok=%v), want %q", tomb, ok, tokC)
	}
	if len(queue) != 0 {
		t.Fatalf("scripted candidates not fully consumed, leftover %v (regen loop consumed an unexpected candidate)", queue)
	}
}
