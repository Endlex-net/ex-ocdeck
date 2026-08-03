package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"ocdeck/internal/config"
)

// writeMaskedFrame 写入一个客户端帧（带 mask，RFC 6455 客户端→服务端必须 mask）。
func writeMaskedFrame(conn net.Conn, opcode int, payload []byte) error {
	hdr := []byte{byte(0x80 | opcode), byte(0x80 | len(payload))}
	mask := []byte{1, 2, 3, 4}
	hdr = append(hdr, mask...)
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	hdr = append(hdr, masked...)
	_, err := conn.Write(hdr)
	return err
}

// readServerFrame 从服务端读取一帧（服务端不 mask）。
func readServerFrame(r *bufio.Reader) (int, []byte, error) {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return 0, nil, err
	}
	opcode := int(hdr[0] & 0x0F)
	length := int64(hdr[1] & 0x7F)
	if length == 126 {
		ext := make([]byte, 2)
		if _, err := io.ReadFull(r, ext); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext))
	} else if length == 127 {
		ext := make([]byte, 8)
		if _, err := io.ReadFull(r, ext); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(ext))
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return opcode, payload, nil
}

// TestWSClientRegistry_Replace4009 验证单交互客户端注册表：新连接替换旧连接并返回旧 cancel。
// register 不再自动取消旧连接（B4）：调用方须先发 4009 再调用 oldCancel。
func TestWSClientRegistry_Replace4009(t *testing.T) {
	reg := newWSClientRegistry()
	key := terminalKey("t1", false)
	c1, _ := net.Pipe()
	defer c1.Close()
	wsC1 := &websocket.Conn{} // 仅用于 key 标识，不实际使用
	old, oldCancel, ctx1 := reg.register(key, wsC1)
	if old != nil {
		t.Fatal("first register should have no old conn")
	}
	if oldCancel != nil {
		t.Fatal("first register should have nil oldCancel")
	}
	wsC2 := &websocket.Conn{}
	old2, old2Cancel, ctx2 := reg.register(key, wsC2)
	if old2 != wsC1 {
		t.Errorf("second register should return first conn as old")
	}
	if old2Cancel == nil {
		t.Fatal("second register should return non-nil oldCancel")
	}
	// register 不应自动取消旧连接 ctx（调用方负责在发 4009 后 cancel）。
	select {
	case <-ctx1.Done():
		t.Error("old conn ctx should NOT be cancelled by register (caller cancels after 4009)")
	default:
	}
	// 调用 oldCancel 后旧连接 ctx 应被取消。
	old2Cancel()
	select {
	case <-ctx1.Done():
	default:
		t.Error("old conn ctx should be cancelled after oldCancel()")
	}
	// 新连接 ctx 应活跃。
	select {
	case <-ctx2.Done():
		t.Error("new conn ctx should not be cancelled")
	default:
	}
	_ = c1
}

// TestWSClientRegistry_UnregisterMatch 验证 unregister 仅移除匹配的 conn（不误删新连接）。
func TestWSClientRegistry_UnregisterMatch(t *testing.T) {
	reg := newWSClientRegistry()
	key := terminalKey("s1", true)
	wsC1 := &websocket.Conn{}
	reg.register(key, wsC1)
	wsC2 := &websocket.Conn{}
	reg.register(key, wsC2) // wsC2 替换 wsC1
	// 用旧 conn unregister 不应移除当前项。
	reg.unregister(key, wsC1)
	reg.mu.Lock()
	_, ok := reg.clients[key]
	reg.mu.Unlock()
	if !ok {
		t.Error("unregister with old conn should not remove current entry")
	}
	// 用新 conn unregister 应移除。
	reg.unregister(key, wsC2)
	reg.mu.Lock()
	_, ok = reg.clients[key]
	reg.mu.Unlock()
	if ok {
		t.Error("unregister with current conn should remove entry")
	}
}

// TestCheckWSOrigin 校验 Origin 白名单。
func TestCheckWSOrigin(t *testing.T) {
	s := &Server{}
	cases := []struct {
		origin string
		want   bool
	}{
		{"", true},                                  // 非浏览器客户端
		{"http://localhost:8080", true},             // 默认 localhost
		{"http://127.0.0.1:3000", true},             // 默认 127.0.0.1
		{"http://localhost", true},                  // 无端口也允许
		{"http://localhost.evil", false},            // hostname 前缀绕过（HasPrefix 会误放行，解析 URL 精确匹配拒绝）
		{"http://localhostevil", false},             // hostname 非精确匹配
		{"http://evil.com", false},                  // 非白名单
		{"http://evil.com/http://localhost", false}, // 路径含 localhost 字符串不绕过
		{"http://127.0.0.1.evil", false},            // 127.0.0.1 前缀绕过
		{"ws://localhost:8080", false},              // 非 http scheme 默认拒绝（design.md §7 仅 http://localhost:*）
		{"https://localhost:8080", false},           // https 经 OCDECK_ALLOWED_ORIGINS 配置，默认拒绝
		{"file://localhost", false},                 // 非 http scheme 默认拒绝
	}
	for i, c := range cases {
		req := httptest.NewRequest("GET", "/ws", nil)
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		if got := s.checkWSOrigin(req); got != c.want {
			t.Errorf("case %d origin=%q: got %v want %v", i, c.origin, got, c.want)
		}
	}
}

// TestCheckWSOrigin_AllowedList 验证 OCDECK_ALLOWED_ORIGINS 白名单生效。
func TestCheckWSOrigin_AllowedList(t *testing.T) {
	s := &Server{cfg: &config.Config{AllowedOrigins: []string{"http://myapp.local"}}}
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Origin", "http://myapp.local")
	if !s.checkWSOrigin(req) {
		t.Error("whitelisted origin should be allowed")
	}
	req2 := httptest.NewRequest("GET", "/ws", nil)
	req2.Header.Set("Origin", "http://other.local")
	if s.checkWSOrigin(req2) {
		t.Error("non-whitelisted origin should be rejected")
	}
}

// TestWSAuthHandshake_TokenValidation 验证首帧认证 token 校验与超时拒绝。
func TestWSAuthHandshake_TokenValidation(t *testing.T) {
	// 用 httptest server + coder/websocket 客户端构造真实 WS 连接。
	auth := NewTokenAuthenticator("secret-token")
	s := &Server{auth: auth}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := acceptWS(w, r)
		if err != nil {
			return
		}
		_, ok := s.wsAuthHandshake(r.Context(), c)
		if ok {
			wsClose(context.Background(), c, wsCloseNormal, "ok")
		} else {
			wsClose(context.Background(), c, wsCloseAuthFailed, "bad")
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	// 正确 token → 正常关闭。
	t.Run("valid token", func(t *testing.T) {
		c, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer c.CloseNow()
		authBody, _ := json.Marshal(wsAuthReq{Type: "auth", Token: "secret-token", Cols: 80, Rows: 24})
		if err := c.Write(context.Background(), websocket.MessageText, authBody); err != nil {
			t.Fatal(err)
		}
		_, _, err = c.Read(context.Background())
		if websocket.CloseStatus(err) != websocket.StatusCode(wsCloseNormal) {
			t.Errorf("want close %d (normal), got %v", wsCloseNormal, websocket.CloseStatus(err))
		}
	})

	// 错误 token → auth failed 关闭。
	t.Run("invalid token", func(t *testing.T) {
		c, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer c.CloseNow()
		authBody, _ := json.Marshal(wsAuthReq{Type: "auth", Token: "wrong", Cols: 80, Rows: 24})
		_ = c.Write(context.Background(), websocket.MessageText, authBody)
		_, _, err = c.Read(context.Background())
		if websocket.CloseStatus(err) != websocket.StatusCode(wsCloseAuthFailed) {
			t.Errorf("want close %d (auth failed), got %v", wsCloseAuthFailed, websocket.CloseStatus(err))
		}
	})

	// 无首帧（超时）→ 非 normal 关闭（auth failed）。
	t.Run("no frame timeout", func(t *testing.T) {
		c, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer c.CloseNow()
		_, _, err = c.Read(context.Background())
		if websocket.CloseStatus(err) == websocket.StatusCode(wsCloseNormal) {
			t.Errorf("timeout should not close normal (1000), got normal")
		}
	})
}

// TestWSEcho_RoundTrip 验证 WS 二进制帧往返（基于 coder/websocket，验证 mask/FIN 由库处理）。
func TestWSEcho_RoundTrip(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := acceptWS(w, r)
		if err != nil {
			return
		}
		defer c.CloseNow()
		// 回显二进制帧。
		for {
			typ, payload, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if err := c.Write(r.Context(), typ, payload); err != nil {
				return
			}
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	c, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	c.SetReadLimit(wsMaxFrame)

	// 客户端 mask 帧（库自动处理）→ 服务端解 mask → 回显。
	msg := bytes.Repeat([]byte("x"), 200) // 跨多字节验证 mask 正确
	if err := c.Write(context.Background(), websocket.MessageBinary, msg); err != nil {
		t.Fatal(err)
	}
	typ, payload, err := c.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if typ != websocket.MessageBinary {
		t.Errorf("type=%v want binary", typ)
	}
	if !bytes.Equal(payload, msg) {
		t.Errorf("payload mismatch: got %d bytes, want %d", len(payload), len(msg))
	}
}

// TestWSUpgrade_RejectBadHandshake 验证非 WebSocket 请求被拒（库内置校验）。
func TestWSUpgrade_RejectBadHandshake(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := acceptWS(w, r)
		if err != nil {
			// 库会自行写 400 响应。
			return
		}
		c.CloseNow()
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	// 普通 GET 请求，无升级头 → 应返回 400。
	resp, err := http.Get(srv.URL + "/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// 非 WebSocket 请求：coder/websocket 返回 426 Upgrade Required。
	if resp.StatusCode != http.StatusUpgradeRequired && resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d want 426 or 400 (not a websocket request)", resp.StatusCode)
	}
}

// TestWSMask_LargePayload 验证大 payload（触发长度扩展 126/127 编码）mask 解码正确。
func TestWSMask_LargePayload(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := acceptWS(w, r)
		if err != nil {
			return
		}
		defer c.CloseNow()
		typ, payload, err := c.Read(r.Context())
		if err != nil {
			return
		}
		_ = c.Write(r.Context(), typ, payload)
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	c, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	c.SetReadLimit(wsMaxFrame)

	// 70000 字节 > 65535，触发 64 位长度编码。
	msg := bytes.Repeat([]byte("AB"), 35000)
	if err := c.Write(context.Background(), websocket.MessageBinary, msg); err != nil {
		t.Fatal(err)
	}
	_, payload, err := c.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, msg) {
		t.Errorf("large payload mismatch: got %d, want %d", len(payload), len(msg))
	}
}

// TestWSPingPong 验证 ping/pong 由库自动处理（不阻塞读循环）。
func TestWSPingPong(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := acceptWS(w, r)
		if err != nil {
			return
		}
		defer c.CloseNow()
		// 服务端发 ping，库自动等 pong。
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		_ = c.Ping(ctx)
		// 读一帧后关闭。
		_, _, _ = c.Read(r.Context())
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	c, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	c.SetReadLimit(wsMaxFrame)
	// 客户端发一帧触发服务端读退出（pong 由库自动回）。
	_ = c.Write(context.Background(), websocket.MessageText, []byte("hi"))
	// 客户端读服务端可能发的关闭或帧，不 panic 即通过。
	_, _, _ = c.Read(context.Background())
}
