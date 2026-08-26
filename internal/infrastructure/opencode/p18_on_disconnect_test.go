// p18_on_disconnect_test.go 验证 P1.8.4 SSE 断流回调契约：
// 仅已建立连接终止（established 且非 ctx 主动取消）触发一次、先于重连事件派发；
// 从未建立的连接与主动 ctx 取消 MUST NOT 触发。
package opencode

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestP18_OnDisconnect_EstablishedDrop 断流（首连接建立后关闭）→ onDisconnect 触发一次，
// 且先于重连后的事件回调（退避前同步）。
func TestP18_OnDisconnect_EstablishedDrop(t *testing.T) {
	var connects int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		if atomic.AddInt32(&connects, 1) == 1 {
			// 首连接：建立（server.connected）后立即断流。
			_, _ = io.WriteString(w, "event: server.connected\ndata: {}\n\n")
			flusher.Flush()
			return
		}
		// 重连：保持连接，发送一个事件证明 onDisconnect 先于其回调。
		_, _ = io.WriteString(w, "event: session.created\ndata: {\"type\":\"session.created\",\"properties\":{\"info\":{\"id\":\"s1\",\"time\":{\"updated\":1.0}}}}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	var mu sync.Mutex
	disconnects := 0
	disconnectsAtFirstEvent := -1
	c.onDisconnect = func() {
		mu.Lock()
		defer mu.Unlock()
		disconnects++
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.SubscribeEvents(ctx, "/wt",
			func(Event) {
				mu.Lock()
				defer mu.Unlock()
				if disconnectsAtFirstEvent < 0 {
					disconnectsAtFirstEvent = disconnects
				}
				cancel()
			},
			func() {},
		)
	}()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("SubscribeEvents: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for SubscribeEvents to return")
	}

	mu.Lock()
	defer mu.Unlock()
	if disconnects != 1 {
		t.Fatalf("onDisconnect calls = %d, want 1", disconnects)
	}
	if disconnectsAtFirstEvent != 1 {
		t.Fatalf("disconnects observed at first reconnected event = %d, want 1 (callback after disconnect)", disconnectsAtFirstEvent)
	}
}

// TestP18_OnDisconnect_CtxCancelNotFired 主动 ctx 取消（连接已建立）MUST NOT 触发。
func TestP18_OnDisconnect_CtxCancelNotFired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: server.connected\ndata: {}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	var disconnects int32
	c.onDisconnect = func() { atomic.AddInt32(&disconnects, 1) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- c.SubscribeEvents(ctx, "/wt", func(Event) {}, func() {})
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("expected ctx cancel, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SubscribeEvents did not return on ctx cancel")
	}
	if n := atomic.LoadInt32(&disconnects); n != 0 {
		t.Fatalf("ctx cancel must not fire onDisconnect; fired %d", n)
	}
}

// TestP18_OnDisconnect_NeverEstablishedNotFired 从未建立的连接（永久 4xx）MUST NOT 触发。
func TestP18_OnDisconnect_NeverEstablishedNotFired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	var disconnects int32
	c.onDisconnect = func() { atomic.AddInt32(&disconnects, 1) }

	err := c.SubscribeEvents(context.Background(), "/wt", func(Event) {}, func() {})
	if err == nil {
		t.Fatal("expected permanent error from never-established connection")
	}
	if n := atomic.LoadInt32(&disconnects); n != 0 {
		t.Fatalf("never-established connection must not fire onDisconnect; fired %d", n)
	}
}
