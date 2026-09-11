package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
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

// wsAuthResp 认证结果控制帧。connId 为 server 为该连接签发的连接 ID
//（terminal-file-paste-drop terminal-streaming delta：auth_ok MUST 携带 connId）。
type wsAuthResp struct {
	Type   string `json:"type"`
	Code   string `json:"code,omitempty"`
	ConnID string `json:"connId,omitempty"`
}

// acceptWS 升级 HTTP 连接为 WebSocket（基于 coder/websocket，design.md §7）。
// Origin 校验由调用方在调用前完成（checkWSOrigin），这里关闭库内置 origin 校验
// （InsecureSkipVerify）以使用自定义策略，保留 token/Origin 的显式控制流。
// 设置读上限为 wsMaxFrame，超过即断开（design.md §7：有界帧防 DoS）。
//
// 返回的 wsTransportHandle 保留底层连接句柄：bridge 收尾 1s 预算耗尽时经它强制关闭
// 底层 transport（coder/websocket 的 Close/CloseNow 不可互相抢占，见 wsFinishTimeout）。
func acceptWS(w http.ResponseWriter, r *http.Request) (*websocket.Conn, *wsTransportHandle, error) {
	handle := &wsTransportHandle{}
	c, err := websocket.Accept(&wsHijackRecorder{ResponseWriter: w, handle: handle}, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // Origin 由 checkWSOrigin 在调用前校验
	})
	if err != nil {
		return nil, nil, err
	}
	c.SetReadLimit(int64(wsMaxFrame))
	return c, handle, nil
}

// wsTransportHandle 保留 WS 连接底层 transport 句柄，支持独立于 Conn.Close 的强制关闭。
// coder/websocket（v1.8.15）Close 写关闭帧最多等 5s、CloseNow 不可抢占进行中的 Close
//（走 waitGoroutines 上限 15s），bridge 收尾 1s 预算经本适配边界兑现：预算耗尽直接关
// 底层连接，不依赖对端关闭握手（delivery spec bridge 收尾预算冻结段）。
type wsTransportHandle struct {
	mu   sync.Mutex
	conn net.Conn
}

func (h *wsTransportHandle) set(c net.Conn) {
	h.mu.Lock()
	h.conn = c
	h.mu.Unlock()
}

// clear 解除句柄（真实 Close 已发生，幂等强关不再动作）。
func (h *wsTransportHandle) clear(c net.Conn) {
	h.mu.Lock()
	if h.conn == c {
		h.conn = nil
	}
	h.mu.Unlock()
}

// forceClose 强制关闭底层 transport（幂等；与 Conn.Close 状态无关）。
func (h *wsTransportHandle) forceClose() {
	h.mu.Lock()
	c := h.conn
	h.conn = nil
	h.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

// wsHijackRecorder 拦截 Hijack：把 coder/websocket Accept 取得的 net.Conn 换成记录
// 句柄的包装（Accept 内部经 http.Hijacker 取底层连接，包装直接实现该接口）。
type wsHijackRecorder struct {
	http.ResponseWriter
	handle *wsTransportHandle
}

func (w *wsHijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response writer does not implement http.Hijacker")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, nil, err
	}
	tc := &trackedConn{Conn: conn, handle: w.handle}
	w.handle.set(tc)
	return tc, brw, nil
}

// trackedConn 记录到 wsTransportHandle 的底层连接；Close 时先解除句柄。
type trackedConn struct {
	net.Conn
	handle *wsTransportHandle
}

func (c *trackedConn) Close() error {
	c.handle.clear(c)
	return c.Conn.Close()
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

// closeOwner 关闭原因提交者（唯一关闭所有者，先提交者为准）。
type closeOwner int

const (
	closeOwnerNone    closeOwner = iota // 未提交
	closeOwnerReplace                   // 替换流程拥有关闭（4009）
	closeOwnerFault                     // bridge 故障收尾拥有关闭（1011）
)

// wsCloseGuard 每连接关闭原因仲裁（delivery spec bridge 收尾表「故障与替换并发」行）：
// 终止原因在 per-conn 提交点内提交，先提交者拥有关闭；后提交方 MUST NOT 另发关闭帧
// 覆盖关闭码（coder/websocket 仅第一次 Close 执行握手，第二次无法改写关闭码）。
type wsCloseGuard struct {
	mu    sync.Mutex
	owner closeOwner
}

// commitReplace 提交替换关闭意图。true=替换方拥有关闭（发送 4009）；false=故障已先
// 提交（bridge 完成 1011 收尾，替换方 MUST NOT 对该连接另发 4009）。
func (g *wsCloseGuard) commitReplace() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.owner != closeOwnerNone {
		return false
	}
	g.owner = closeOwnerReplace
	return true
}

// commitFault 提交故障关闭意图。true=bridge 拥有关闭（1011 收尾）；false=替换已先
// 提交（4009），后续取消/超时产生的写失败 MUST NOT 覆盖该原因。
func (g *wsCloseGuard) commitFault() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.owner != closeOwnerNone {
		return false
	}
	g.owner = closeOwnerFault
	return true
}

// wsClientRegistry 维护按终端 key 的活跃 WS 连接，新连接替换旧连接（4009）。
// key = terminalKey(taskID/terminalID, isShell)。
//
// 写路径边界（刻意保留的内部边界）：TUI key（terminalKey(taskID, false)）的注册
// 唯一入口为 Server.registerTUIConn（per-task 协调锁内提交替换，与投递准入/finalize
// 线性化，见 ws_terminal.go）；本类型的通用 register 仅限 shell key 与测试直接构造
//（shell key 与 TUI key 不同空间，不影响 TUI 当前连接归属）。
type wsClientRegistry struct {
	mu      sync.Mutex
	clients map[string]*wsClientEntry
}

// wsClientEntry 记录活跃连接及其取消句柄与关闭仲裁 guard。
type wsClientEntry struct {
	conn   *websocket.Conn
	connID string // 服务端签发的连接 ID（auth_ok 下发；shell 与 TUI 均有但仅 TUI 参与 deliver 准入）
	guard  *wsCloseGuard
	cancel context.CancelFunc // 取消该连接桥接的 replaceCtx（触发旧连接 bridge 退出）
}

func newWSClientRegistry() *wsClientRegistry {
	return &wsClientRegistry{clients: map[string]*wsClientEntry{}}
}

// register 注册新连接，若 key 已有旧连接则返回旧 conn、旧 guard 与旧 bridge 的 cancel。
// 不在此取消旧 bridge：调用方 MUST 先经 oldGuard.commitReplace 赢得关闭所有权后发送
// 4009 close frame（等待写出或短超时），再调用返回的 oldCancel 取消旧 bridge——保证
// 旧连接稳定收到 4009，且故障先提交时由旧 bridge 完成 1011 收尾、替换方不另发 4009
//（见 wsCloseGuard）。
//
// 边界：生产 TUI 注册 MUST 经 Server.registerTUIConn（per-task 协调锁内提交，D5
// 原子边界）；本方法仅限 shell 连接（不同 key 空间）与测试直接构造。
//
// 返回：oldConn（可能 nil）、oldGuard（oldConn 非 nil 时非 nil）、oldCancel、新连接 bridge 用的 ctx。
func (r *wsClientRegistry) register(key string, conn *websocket.Conn, guard *wsCloseGuard, connID string) (*websocket.Conn, *wsCloseGuard, context.CancelFunc, context.Context) {
	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	old := r.clients[key]
	r.clients[key] = &wsClientEntry{conn: conn, connID: connID, guard: guard, cancel: cancel}
	var oldConn *websocket.Conn
	var oldGuard *wsCloseGuard
	var oldCancel context.CancelFunc
	if old != nil {
		oldConn = old.conn
		oldGuard = old.guard
		oldCancel = old.cancel
	}
	r.mu.Unlock()
	return oldConn, oldGuard, oldCancel, ctx
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

// currentTUIConnID 返回 task 当前 TUI 连接的 connID（换代即失效，shell 连接
// 不参与）。未注册返回 ("", false)。供 deliver/上传准入的连接当前性判定。
func (r *wsClientRegistry) currentTUIConnID(taskID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.clients[terminalKey(taskID, false)]
	if !ok {
		return "", false
	}
	return e.connID, true
}
