package opencode

// spi_events_lifecycle_test.go 覆盖门禁第三轮 OPEN 项的连接生命周期契约测试：
//   - C15：连接建立先于 SetCallbacks 注册（确定性调度）→ ready 信号不丢（补发恰一次）；
//   - C16：断流回调纳入 EventStream 生命周期（已建立连接终止 → markDisconnected →
//     注册后补发 → SetDisconnectCallback 注册的断流回调被调用）；「连接已建立→断流→
//     重连前」时序中 connected/disconnected 补发次序与真实事件序一致；
//   - C10：真实永久 SSE 错误（401）→ stream.Err() 保留实际返回错误（非 sctx.Err()/
//     nil），恢复 last_error 消费面可读。
//
// mutation 证据逐测试注释。

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// TestSPI_Events_ConnectedBeforeSetCallbacks_Replayed（C15）：连接建立先于回调
// 注册——server 首连即推事件（markConnected 由 onReady 在 SSE goroutine 触发），
// Subscribe 返回后 sleep 确认连接已建立，再 SetCallbacks。断言 onConnected 被补发。
//
// mutation 证据（已验证 → 红 → 还原）：markConnected 未注入回调时置可重放信号的
// 分支删除（直接丢弃）→ pendingConnected 不产生 → SetCallbacks 不补发 → 本测试红
//（connected 超时 0 次）。
func TestSPI_Events_ConnectedBeforeSetCallbacks_Replayed(t *testing.T) {
	ts := sseScriptServer(t, nil) // 首连 server.connected 后挂起（连接已建立）
	p := NewSPIProvider()
	stream, err := p.Events().Subscribe(context.Background(), ts.testBinding())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer stream.Close()

	// 确定性调度：等连接信号先入队（connMu 下 FIFO 长度>0，C15 队列形态）再注册回调。
	var es *spiEventStream
	switch s := stream.(type) {
	case *spiEventStream:
		es = s
	default:
		t.Fatalf("stream type = %T, want *spiEventStream", stream)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		es.connMu.Lock()
		queued := len(es.lifecycleEvents)
		es.connMu.Unlock()
		if queued > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("connection signal must be observed before SetCallbacks (C15 window)")
		}
		time.Sleep(2 * time.Millisecond)
	}

	var mu sync.Mutex
	connected := 0
	es.SetCallbacks(func() {
		mu.Lock()
		connected++
		mu.Unlock()
	}, nil)
	mu.Lock()
	if connected != 1 {
		mu.Unlock()
		t.Fatalf("onConnected replays = %d, want exactly 1（补发恰一次）", connected)
	}
	mu.Unlock()
}

// TestSPI_Events_DisconnectCallbackLifecycle（C16）：
// server 首连建立后立即断流（连接终止；SSE client 重连前 onDisconnect 同步触发）。
// 流程：Subscribe → 等断流信号 pending → SetCallbacks（connected replay）→
// SetDisconnectCallback（disconnected replay）。断言「连接已建立→断流→重连前」：
// connected 与 disconnected 各补发一次、次序 connected 先于 disconnected。
//
// mutation 证据（已验证 → 红 → 还原）：markConnected/markDisconnected 未注入回调
// 时置可重放信号的分支删除 → 两个补发均为 0 → 本测试红「want exactly once」；
// Subscribe 的 Options 移除 OnDisconnect → realDisconnects=0 → 断流信号不产生。
func TestSPI_Events_DisconnectCallbackLifecycle(t *testing.T) {
	// 首连接：server.connected → 立即关闭（established 后断流，连接已建立）；
	// 后续重连挂起（「重连前」窗口不被打断）。
	var connects int
	var mu sync.Mutex
	srv := newSPITestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/event" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		mu.Lock()
		connects++
		first := connects == 1
		mu.Unlock()
		_, _ = fmt.Fprint(w, "data: {\"type\":\"server.connected\",\"properties\":{}}\n\n")
		fl.Flush()
		if first {
			return // handler 返回 → 连接终止（established=true → onDisconnect）
		}
		<-r.Context().Done()
	}))

	p := NewSPIProvider()
	stream, err := p.Events().Subscribe(context.Background(), srv.testBinding())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer stream.Close()

	var es *spiEventStream
	switch s := stream.(type) {
	case *spiEventStream:
		es = s
	default:
		t.Fatalf("stream type = %T, want *spiEventStream", stream)
	}
	// 等断流信号先于注册入队（C16 窗口：连接已建立→断流→重连前；C15 队列形态）。
	deadline := time.Now().Add(3 * time.Second)
	for {
		es.connMu.Lock()
		queued := len(es.lifecycleEvents)
		es.connMu.Unlock()
		if queued >= 2 { // connected + disconnected 都已入队。
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("disconnect signal must be observed before callbacks registered (C16 window)")
		}
		time.Sleep(2 * time.Millisecond)
	}

	var cbMu sync.Mutex
	var order []string
	es.SetCallbacks(func() {
		cbMu.Lock()
		order = append(order, "connected")
		cbMu.Unlock()
	}, nil)
	es.SetDisconnectCallback(func() {
		cbMu.Lock()
		order = append(order, "disconnected")
		cbMu.Unlock()
	})
	cbMu.Lock()
	defer cbMu.Unlock()
	if len(order) != 2 {
		t.Fatalf("callbacks = %v, want [connected disconnected]（各补发恰一次，次序=真实事件序）", order)
	}
	if order[0] != "connected" || order[1] != "disconnected" {
		t.Fatalf("callback order = %v, want connected→disconnected", order)
	}
}

// TestSPI_Events_StreamErrCarriesRealError（C10）：真实永久 SSE 错误（401）→
// SubscribeEvents 返回 ErrUnauthorized（非 ctx 取消）→ 流退出先存 Err 再关 events
// → Err() 携带实际返回错误；消费面（恢复 last_error）可读。
//
// mutation 证据（已验证 → 红 → 还原）：defer 内恢复 setErr(streamErr) 为
// setErr(sctx.Err())（sctx 未取消 → nil）→ Err()=nil → 本测试红「Err = nil, want
// unauthorized err」。
func TestSPI_Events_StreamErrCarriesRealError(t *testing.T) {
	// 401 永久失败：isPermanentSSEError → SubscribeEvents 返回 ErrUnauthorized
	//（sctx 未取消——ctx 错误未发生）。
	srv := newSPITestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	p := NewSPIProvider()
	stream, err := p.Events().Subscribe(context.Background(), srv.testBinding())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer stream.Close()

	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-stream.Events():
			if ok {
				t.Fatalf("unexpected event from 401 server: %+v", ev)
			}
			// events 关闭（流退出）：Err 必须携带实际返回错误（先存再关）。
			got := stream.Err()
			if got == nil {
				t.Fatal("Err = nil, want real SubscribeEvents return error（C10：终止原因可判定）")
			}
			return
		case <-deadline:
			t.Fatal("timeout waiting for stream termination")
		}
	}
}