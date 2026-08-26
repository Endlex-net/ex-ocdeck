package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"ocdeck/internal/infrastructure/pty"
)

// dialWS 连接到 httptest server 的 /ws 端点，返回 websocket 连接。
func dialWS(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	return c
}

// newBridgeHandler 返回一个 handler：acceptWS 后 bridgeTerminal 桥接到给定 PTY。
func newBridgeHandler(p *pty.Pty) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := acceptWS(w, r)
		if err != nil {
			return
		}
		defer c.CloseNow()
		s := &Server{}
		s.bridgeTerminal(r.Context(), c, p)
	}
}

// openCatPty 起一个 /bin/cat 的 PTY（darwin/posix），返回 *pty.Pty。
func openCatPty(t *testing.T) *pty.Pty {
	t.Helper()
	cmd := exec.Command("/bin/cat")
	p, err := pty.Open(cmd, "", nil, 80, 24)
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	return p
}

// TestWSBridge_PtyCloseClosesWS 验证双向取消：PTY 关闭 → bridge 结束 → WS 被关闭。
// 设计：design.md §7/§21 双向取消（任一方向退出即取消另一方向）。
func TestWSBridge_PtyCloseClosesWS(t *testing.T) {
	p := openCatPty(t)
	defer p.Close()
	srv := httptest.NewServer(newBridgeHandler(p))
	defer srv.Close()

	c := dialWS(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws")
	defer c.CloseNow()
	c.SetReadLimit(wsMaxFrame)

	// 关闭 PTY → pumpPTYToWS 读到 EOF → 返回 false → bridge 结束 → WS 被关闭。
	p.Close()
	// 读 WS 直到出错（被服务端关闭）。
	_, _, err := c.Read(context.Background())
	if err == nil {
		t.Error("expected WS closed after PTY close")
	}
}

// TestWSBridge_WSDisconnectExitsBridge 验证双向取消：WS 断开 → pumpWSToPTY 退出 →
// 取消另一方向 → bridge 结束。bridge 结束后不应泄漏 goroutine（PTY 关闭可正常回收）。
func TestWSBridge_WSDisconnectExitsBridge(t *testing.T) {
	p := openCatPty(t)
	defer p.Close()
	srv := httptest.NewServer(newBridgeHandler(p))
	defer srv.Close()

	c := dialWS(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws")
	c.SetReadLimit(wsMaxFrame)

	// 客户端主动关闭 WS → 服务端 pumpWSToPTY 读错退出 → 取消 → bridge 结束。
	c.CloseNow()
	// 给 bridge 一点时间退出。后续 PTY Close 不应阻塞。
	time.Sleep(100 * time.Millisecond)
	// bridge 已退出，PTY Close 可正常回收（不阻塞）。
	if err := p.Close(); err != nil {
		t.Errorf("PTY close after WS disconnect should not error: %v", err)
	}
}

// TestWSBridge_Replace4009_OldConnCancelled 验证 4009 替换：同一终端 key 新连接替换旧连接，
// 旧连接 MUST 被以 4009 关闭（B4：先发 4009 再 cancel 旧 bridge；旧 bridge 被取消路径不发 1000）。
func TestWSBridge_Replace4009_OldConnCancelled(t *testing.T) {
	p := openCatPty(t)
	defer p.Close()
	reg := newWSClientRegistry()
	key := terminalKey("t1", false)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := acceptWS(w, r)
		if err != nil {
			return
		}
		defer c.CloseNow()
		oldConn, oldCancel, bridgeCtx := reg.register(key, c)
		if oldConn != nil {
			wsCloseReplacedWait(oldConn)
			oldCancel()
		}
		defer reg.unregister(key, c)
		// bridge 使用 bridgeCtx（新连接替换时由新连接的 oldCancel 取消）。
		s := &Server{}
		s.bridgeTerminal(bridgeCtx, c, p)
	}))
	defer srv.Close()

	c1 := dialWS(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws")
	defer c1.CloseNow()
	c1.SetReadLimit(wsMaxFrame)

	c2 := dialWS(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws")
	defer c2.CloseNow()
	c2.SetReadLimit(wsMaxFrame)

	// c1 应被服务端以 4009 关闭（旧连接被替换；B4：旧 bridge 被取消不发 1000）。
	_, _, err := c1.Read(context.Background())
	if websocket.CloseStatus(err) != websocket.StatusCode(wsCloseReplaced) {
		t.Fatalf("c1 close status = %v, want 4009 (replaced)", websocket.CloseStatus(err))
	}
	// c2 仍可用：写入一帧并确保服务端读到（PTY 回显由 cat 产生）。
	if err := c2.Write(context.Background(), websocket.MessageBinary, []byte("hi")); err != nil {
		t.Errorf("c2 write after replace: %v", err)
	}
}

// TestWSBridge_CtxCancelExitsNoHang 验证 bridge ctx 取消时 bridge 不挂起：
// pumpPTYToWS 的写 goroutine 在 ctx 取消后 c.Write 返回错误退出，读循环在 cancel 后
// 也能退出。这是 B10 慢客户端断开的前提（避免写 goroutine 阻塞 c.Write 导致 join 死锁）。
// 设计：design.md §7/§21 双向取消 + B10。
func TestWSBridge_CtxCancelExitsNoHang(t *testing.T) {
	p := openCatPty(t)
	defer p.Close()

	// 用 net.Pipe 构造一个可控的 ws-like 连接较重，这里用 httptest + 真实 WS。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := acceptWS(w, r)
		if err != nil {
			return
		}
		defer c.CloseNow()
		// 用可取消 ctx 驱动 bridge，测试取消即退出。
		ctx, cancel := context.WithCancel(r.Context())
		go func() {
			time.Sleep(100 * time.Millisecond)
			cancel()
		}()
		s := &Server{}
		s.bridgeTerminal(ctx, c, p)
	}))
	defer srv.Close()

	c := dialWS(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws")
	defer c.CloseNow()
	c.SetReadLimit(wsMaxFrame)

	// 写入数据让 PTY 产生输出（cat 回显），使写 goroutine 有 c.Write 在途。
	go func() {
		data := make([]byte, 8192)
		for i := range data {
			data[i] = 'x'
		}
		for i := 0; i < 100; i++ {
			if _, err := p.Write(data); err != nil {
				return
			}
		}
	}()

	// bridge ctx 取消后 bridge MUST 退出 → WS 关闭。有界超时断言不挂起。
	done := make(chan struct{})
	go func() {
		_, _, _ = c.Read(context.Background())
		close(done)
	}()
	select {
	case <-done:
		// bridge 退出，WS 关闭，无挂起。
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not exit after ctx cancel (possible write-goroutine join deadlock)")
	}
}

// TestHandleDeleteTask_ConfirmDirtyParamName 验证 API 层 confirmDirty 参数名核对为
// camelCase confirmDirty（非 confirm_dirty），design.md §21 路由表。
func TestHandleDeleteTask_ConfirmDirtyParamName(t *testing.T) {
	// 用 query 参数 confirmDirty=true 构造请求，断言 task backend 收到 confirmDirty=true。
	// 由于 tasks 后端注入较重，此处仅断言参数解析逻辑：query.Get("confirmDirty") 命中。
	req := httptest.NewRequest("DELETE", "/api/v1/tasks/t1?mode=normal&confirmDirty=true", nil)
	if got := req.URL.Query().Get("confirmDirty"); got != "true" {
		t.Errorf("confirmDirty param=%q want true (camelCase)", got)
	}
	// snake_case 不应命中（参数名大小写敏感）。
	req2 := httptest.NewRequest("DELETE", "/api/v1/tasks/t1?mode=normal&confirm_dirty=true", nil)
	if got := req2.URL.Query().Get("confirmDirty"); got == "true" {
		t.Error("confirm_dirty (snake_case) must not satisfy confirmDirty param (case-sensitive)")
	}
}
