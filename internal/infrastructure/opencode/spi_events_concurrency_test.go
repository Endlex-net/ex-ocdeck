package opencode

// spi_events_concurrency_test.go 锚定 C15 BLOCKING 修复（第四轮复审）：
// 布尔 pending + 锁外补发窗口的竞态——补发窗口内 markConnected 可重读已注册回调
// 直调 → 重复 connected / connected-disconnected 次序交错；布尔还会合并多个未消费
// 信号。修复：统一 FIFO 队列（connMu 内原子入队/领取）+ dispMu 串行分派器——
// 注册与补发共享同一分派域，队首未就绪即停（跨注册窗口保序）。
//
// 本测试以并发形态驱动（同包直调私有面）：
//   - 多生产者并发 mark*（入队序源头不唯一 → 断言「同类事件相对序稳定 + 总数
//     恰等 + 每事件恰一次」）；
//   - Set*/SetDisconnectCallback 重复注入与派发并发（注册窗口竞态正是修复面）；
//   - 回调内微延迟放大 pop→cb 交叠窗口（无串行化时并发 dispatch 在 cb 区交错；
//     dispMu 串行下仅放大时长，序不变）。
//
// mutation 证据（破坏 → 红 → 还原）：破坏点 = dispatchLifecycle 的「领取」挪出
// connMu 临界区（pop 与调用分离——两个并发 dispatch 都可领取到已 pop 事件并各自
// 调用 → 重复/交错）。单串行分派器被破坏后重复/丢失计数即红；还原后绿。

import (
	"sync"
	"testing"
	"time"
)

// TestSPI_Events_LifecycleConcurrentExactlyOnceAllOrder（C15）：mark 与 Set* 并发
// 时每个生命周期事件恰一次派发、每生产者段内 FIFO 相对序稳定。
func TestSPI_Events_LifecycleConcurrentExactlyOnceAllOrder(t *testing.T) {
	es := &spiEventStream{} // events 通道不参与生命周期分派（仅 Subscribe goroutine 用）

	const (
		producers   = 4
		perProducer = 250
		total       = producers * perProducer // 1000（一半 connected 一半 disconnected）
	)

	var mu sync.Mutex
	var got []string
	// 并发回调重叠检测：回调内微延迟 + 进入/离开计数——同一时刻两个回调在执行
	//（重叠）= 分派域未串行化（C15 修复的 dispMu 面）。正确实现下重叠恒为 0。
	inCallbacks := 0
	overlap := 0
	record := func(kind string) {
		mu.Lock()
		inCallbacks++
		if inCallbacks > 1 {
			overlap++
		}
		mu.Unlock()
		time.Sleep(50 * time.Microsecond)
		mu.Lock()
		got = append(got, kind)
		inCallbacks--
		mu.Unlock()
	}

	// 注入回调；随后持续重复注入（注册窗口竞态面）。
	es.SetCallbacks(func() { record("connected") }, nil)
	es.SetDisconnectCallback(func() { record("disconnected") })
	var injWG sync.WaitGroup
	stop := make(chan struct{})
	injWG.Add(1)
	go func() {
		defer injWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			es.SetCallbacks(func() { record("connected") }, nil)
			es.SetDisconnectCallback(func() { record("disconnected") })
			time.Sleep(50 * time.Microsecond)
		}
	}()

	// 多生产者并发 mark：每生产者 C,D,C,D,... 严格交替（段内相对序可断言）。
	var prodWG sync.WaitGroup
	results := make([][]string, producers)
	for p := 0; p < producers; p++ {
		prodWG.Add(1)
		go func(p int) {
			defer prodWG.Done()
			for i := 0; i < perProducer; i++ {
				if i%2 == 0 {
					es.markConnected()
				} else {
					es.markDisconnected()
				}
			}
			// 本生产者段落检查：其入队的 C,D 交替必须以 C→D 相邻序出现在全局派发序——
			// 通过子序列扫描核对（段内事件相对序稳定 = 单 FIFO 队列未被交错破坏）。
			var sub []string
			for i := 0; i < perProducer; i++ {
				if i%2 == 0 {
					sub = append(sub, "connected")
				} else {
					sub = append(sub, "disconnected")
				}
			}
			results[p] = sub
		}(p)
	}
	prodWG.Wait()

	// 等队列排空。
	deadline := time.Now().Add(10 * time.Second)
	for {
		es.connMu.Lock()
		queued := len(es.lifecycleEvents)
		es.connMu.Unlock()
		if queued == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue never drained: %d events left", queued)
		}
		time.Sleep(time.Millisecond)
	}
	close(stop)
	injWG.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != total {
		t.Fatalf("dispatched %d callbacks, want exactly %d（恰一次：布尔合并丢失/重复派发倍增面）",
			len(got), total)
	}
	// 每生产者的 C,D 交替子序列必须以原序出现在派发序中（FIFO 相对序保持）。
	for p := 0; p < producers; p++ {
		sub := results[p]
		idx := 0
		for _, kind := range got {
			if kind == sub[idx] {
				idx++
				if idx == len(sub) {
					break
				}
			}
		}
		if idx != len(sub) {
			t.Fatalf("producer %d interleaving broken: matched %d/%d（全局序中找不到完整 C,D 交替段——次序交错面）",
				p, idx, len(sub))
		}
	}
	// 并发回调重叠检测（分派域串行性）：正确实现 dispMu 串行下重叠恒为 0。
	if overlap > 0 {
		t.Fatalf("callback executions overlapped %d times（分派域未串行化：实时触发与补发交错执行——C15）",
			overlap)
	}
}