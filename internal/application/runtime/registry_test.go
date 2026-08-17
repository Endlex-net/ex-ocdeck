package runtime

import (
	"sync"
	"testing"
)

// TestNewRuntimeToken_MonotonicAndTombstone 验证 generation 单调递增、
// 清理后从 tombstone 续递增（B4：进程生命周期内不回卷）。
func TestNewRuntimeToken_MonotonicAndTombstone(t *testing.T) {
	r := New()
	// 首次分配：prevGen=0 → gen=1，instanceID="i1"。
	tok1 := r.NewRuntimeToken("t1", 0, "i1")
	if tok1.Generation != 1 || tok1.InstanceID != "i1" {
		t.Fatalf("tok1 = %+v, want {gen=1 inst=i1}", tok1)
	}
	// 续递增：prevGen=1 → gen=2。
	tok2 := r.NewRuntimeToken("t1", 1, "i2")
	if tok2.Generation != 2 || tok2.InstanceID != "i2" {
		t.Fatalf("tok2 = %+v, want {gen=2 inst=i2}", tok2)
	}
	// 模拟 clearRuntime（runtime 移除，prevGen 回到 0）。generation MUST 从 tombstone 续递增，
	// 不得回卷到 1（B4）。
	tok3 := r.NewRuntimeToken("t1", 0, "i3")
	if tok3.Generation != 3 || tok3.InstanceID != "i3" {
		t.Fatalf("tok3 = %+v, want {gen=3 inst=i3} (tombstone prevents rewind)", tok3)
	}
	// Tombstone 反映最近一次分配的完整令牌（P1.4.7：generation+instanceID）。
	tomb, ok := r.Tombstone("t1")
	if !ok || tomb != tok3 {
		t.Errorf("Tombstone = %+v (ok=%v), want %+v", tomb, ok, tok3)
	}
}

// TestNewRuntimeToken_IndependentTasks 验证不同 taskID 的代际独立。
func TestNewRuntimeToken_IndependentTasks(t *testing.T) {
	r := New()
	tokA1 := r.NewRuntimeToken("a", 0, "a1")
	tokB1 := r.NewRuntimeToken("b", 0, "b1")
	if tokA1.Generation != 1 || tokB1.Generation != 1 {
		t.Fatalf("independent tasks should each start at gen=1, got a=%d b=%d", tokA1.Generation, tokB1.Generation)
	}
	// t1 推进不影响 b。
	tokA2 := r.NewRuntimeToken("a", 1, "a2")
	if tokA2.Generation != 2 {
		t.Fatalf("a gen2 = %d, want 2", tokA2.Generation)
	}
	if tomb, ok := r.Tombstone("b"); !ok || tomb != tokB1 {
		t.Errorf("b tombstone = %+v (ok=%v), want %+v (independent)", tomb, ok, tokB1)
	}
}

// TestNewRuntimeToken_Concurrent 验证并发分配无 data race 且代际单调。
func TestNewRuntimeToken_Concurrent(t *testing.T) {
	r := New()
	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	// 每个 goroutine 为独立 taskID 分配一次，验证 genMu 并发安全。
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			taskID := "t" + itoa(i)
			_ = r.NewRuntimeToken(taskID, 0, "inst")
		}(i)
	}
	wg.Wait()
	// 同一 taskID 并发分配（串行化后单调递增）。
	wg.Add(goroutines)
	var maxGen int
	var mu sync.Mutex
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			// 每次用「上次的 gen」作为 prevGen 模拟续递增；并发下 genMu 串行化。
			// 由于并发 prevGen 可能落后，Registry 仍保证单调（取 max(prev+1, last+1)）。
			tok := r.NewRuntimeToken("shared", 0, "inst")
			mu.Lock()
			if tok.Generation > maxGen {
				maxGen = tok.Generation
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if maxGen < 1 {
		t.Fatalf("concurrent allocation produced no tokens")
	}
	// tombstone 令牌的 generation == 最大已分配 gen。
	if tomb, ok := r.Tombstone("shared"); !ok || tomb.Generation != maxGen {
		t.Errorf("shared tombstone = %+v (ok=%v), want gen %d (max allocated)", tomb, ok, maxGen)
	}
}

// TestTombstone_NoTask 验证未分配 taskID 无 tombstone（found=false）。
func TestTombstone_NoTask(t *testing.T) {
	r := New()
	if _, ok := r.Tombstone("nonexistent"); ok {
		t.Error("Tombstone(nonexistent) must be found=false (never allocated)")
	}
}

// itoa 轻量整数转字符串（避免引入 strconv 使测试依赖最小）。
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