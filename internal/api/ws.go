package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// wsCloseCode WS 关闭码（design.md §7/§21）。
const (
	wsCloseAuthFailed       = 4001
	wsCloseReplaced         = 4009
	wsCloseTaskSuspended    = 4010
	wsCloseTerminalNotFound = 4004 // shell 终端身份校验失败：非法 tid / 非 shell 会话（design.md §21）
	wsCloseInternalError    = 1011
	wsCloseNormal           = 1000
	wsCloseRecovering       = 1013 // Try Again Later：任务进程恢复中（D8 recovering 契约，前端轮询状态后重连）
)

// wsMaxFrame WS 帧大小上限（design.md §7）。
const wsMaxFrame = 1 << 20

// wsAuthTimeout 首帧认证超时（design.md §7：5s）。
const wsAuthTimeout = 5 * time.Second

// wsWriteQueueCap 有界写队列容量（B10：慢客户端断开）。
// 队列满即认为客户端消费不及，主动断开避免无限背压堆积。
const wsWriteQueueCap = 64

// wsAuthReq 首帧认证请求（design.md §7/§21）。
type wsAuthReq struct {
	Type  string `json:"type"`
	Token string `json:"token"`
	Cols  int    `json:"cols"`
	Rows  int    `json:"rows"`
}

// wsCtrlFrame 控制帧（resize）。
type wsCtrlFrame struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

// wsAuthResp 认证结果控制帧。
type wsAuthResp struct {
	Type string `json:"type"`
	Code string `json:"code,omitempty"`
}

// acceptWS 升级 HTTP 连接为 WebSocket（基于 coder/websocket，design.md §7）。
// Origin 校验由调用方在调用前完成（checkWSOrigin），这里关闭库内置 origin 校验
// （InsecureSkipVerify）以使用自定义策略，保留 token/Origin 的显式控制流。
// 设置读上限为 wsMaxFrame，超过即断开（design.md §7：有界帧防 DoS）。
func acceptWS(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // Origin 由 checkWSOrigin 在调用前校验
	})
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(int64(wsMaxFrame))
	return c, nil
}

// checkWSOrigin 校验 Origin（design.md §7：默认 http://localhost:* / http://127.0.0.1:* + OCDECK_ALLOWED_ORIGINS，不信 X-Forwarded-*）。
// MUST 解析 URL 比较 hostname，不得用字符串前缀匹配（`http://localhost.evil` 可绕过 HasPrefix 前缀）。
// 默认仅允许 scheme=http 且 hostname ∈ {localhost, 127.0.0.1} 任意端口。
// https / ws 等非 http scheme 默认拒绝（反代 https 场景经 OCDECK_ALLOWED_ORIGINS 精确配置）。
// AllowedOrigins 列表按精确 origin 全串匹配（调用方保证不含通配），不受 scheme 限制。
func (s *Server) checkWSOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // 非浏览器客户端
	}
	// AllowedOrigins：精确全串匹配。
	if s.cfg != nil {
		for _, a := range s.cfg.AllowedOrigins {
			if a == origin {
				return true
			}
		}
	}
	// 默认 localhost / 127.0.0.1：解析 URL 比较 hostname（精确匹配，避免前缀绕过）。
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" {
		return false
	}
	// 仅允许 http scheme（design.md §7：http://localhost:*）；非 http（file/ws/...）默认拒绝。
	if u.Scheme != "http" {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1":
		return true
	}
	return false
}

// wsAuthHandshake 首帧认证：wsAuthTimeout 内读取 auth JSON，校验 token。
// 成功返回 auth 请求；失败返回 false（调用方负责写关闭码）。
// token 校验失败 MUST NOT 写日志或回显 close frame：失败仅返回 false，
// close reason 用固定泛化串（wsCloseAuthFailed + "auth failed"），不得携带 token 或校验细节，
// 避免任何回显路径泄露 token 推断信息。
func (s *Server) wsAuthHandshake(ctx context.Context, c *websocket.Conn) (wsAuthReq, bool) {
	ctx, cancel := context.WithTimeout(ctx, wsAuthTimeout)
	defer cancel()
	typ, payload, err := c.Read(ctx)
	if err != nil || typ != websocket.MessageText {
		return wsAuthReq{}, false
	}
	var req wsAuthReq
	if err := json.Unmarshal(payload, &req); err != nil {
		return wsAuthReq{}, false
	}
	if req.Type != "auth" || !s.auth.ValidateToken(req.Token) {
		return wsAuthReq{}, false
	}
	return req, true
}

// wsClose 写入关闭帧（design.md §7 关闭码）。
func wsClose(ctx context.Context, c *websocket.Conn, code int, reason string) {
	_ = c.Close(websocket.StatusCode(code), reason)
}

// wsCloseReplacedWait 向被替换的旧连接发送 4009 close frame，最多等待 closeCloseTimeout。
// coder/websocket 的 Conn.Close 先写出 close frame（writeControl，受库内部 writeFrameMu
// 串行化，与旧 bridge 的数据 Write 互斥不损坏）再等待握手；closeCloseTimeout 作为短超时
// 上限，保证即使旧 bridge 持锁也不长时间阻塞新连接的替换流程。超时后调用方仍 cancel 旧
// bridge，旧 bridge 在被取消路径不发 1000（见 bridgeTerminal），旧连接已收到 4009。
func wsCloseReplacedWait(c *websocket.Conn) {
	done := make(chan struct{})
	go func() {
		_ = c.Close(websocket.StatusCode(wsCloseReplaced), "replaced by new connection")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(wsCloseReplacedTimeout):
	}
}

// wsCloseReplacedTimeout 4009 close frame 的等待上限（B4：旧连接稳定收到 4009）。
// 1s 足够 coder/websocket 写出 close frame（writeControl 经 writeFrameMu 串行化）。
const wsCloseReplacedTimeout = time.Second

// --- 单交互客户端注册表（design.md §21：同一终端新连接替换旧连接，4009） ---

// wsClientRegistry 维护按终端 key 的活跃 WS 连接，新连接替换旧连接（4009）。
// key = terminalKey(taskID/terminalID, isShell)。
type wsClientRegistry struct {
	mu      sync.Mutex
	clients map[string]*wsClientEntry
}

// wsClientEntry 记录活跃连接及其取消句柄。
type wsClientEntry struct {
	conn   *websocket.Conn
	cancel context.CancelFunc // 取消该连接桥接的 replaceCtx（触发旧连接 bridge 退出）
}

func newWSClientRegistry() *wsClientRegistry {
	return &wsClientRegistry{clients: map[string]*wsClientEntry{}}
}

// register 注册新连接，若 key 已有旧连接则返回旧 conn 与旧 bridge 的 cancel。
// 不在此取消旧 bridge：调用方 MUST 先向旧连接发送 4009 close frame（等待写出或短超时），
// 再调用返回的 oldCancel 取消旧 bridge——保证旧连接稳定收到 4009，且旧 bridge 在被取消
// 路径不抢先发 1000（区分"被替换"与"正常结束"，见 bridgeTerminal）。
//
// 返回：oldConn（可能 nil）、oldCancel（oldConn 非 nil 时非 nil，否则 nil）、新连接 bridge 用的 ctx。
func (r *wsClientRegistry) register(key string, conn *websocket.Conn) (*websocket.Conn, context.CancelFunc, context.Context) {
	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	old := r.clients[key]
	r.clients[key] = &wsClientEntry{conn: conn, cancel: cancel}
	var oldConn *websocket.Conn
	var oldCancel context.CancelFunc
	if old != nil {
		oldConn = old.conn
		oldCancel = old.cancel
	}
	r.mu.Unlock()
	return oldConn, oldCancel, ctx
}

// unregister 移除 key 对应连接（仅当 conn 匹配，避免误删新连接）。
func (r *wsClientRegistry) unregister(key string, conn *websocket.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.clients[key]; ok && e.conn == conn {
		delete(r.clients, key)
	}
}

// terminalKey 构造终端注册 key。
func terminalKey(id string, isShell bool) string {
	if isShell {
		return "shell:" + id
	}
	return "tui:" + id
}
