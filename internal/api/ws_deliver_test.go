package api

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	appuploads "ocdeck/internal/application/uploads"
	"ocdeck/internal/infrastructure/pty"
)

// --- 测试基建 ---

// fakeDeliverCall 编排调用记录。
type fakeDeliverCall struct {
	taskID   string
	connID   string
	uploadID string
}

// fakeDeliverOrch deliverOrchestrator 内存 fake：模拟 Lane A application 编排
//（per-task 协调锁临界区内准入 + inject 写入，经通道同步模拟临界区时序）。
type fakeDeliverOrch struct {
	mu    sync.Mutex
	calls []fakeDeliverCall

	// result 非 nil 时代替默认编排（参数为 inject）。
	result func(inject func(context.Context) error) (bool, string)

	// injectRuns inject 进入次数（= 编排到达写入步的次数）。
	injectRuns int
	// injectErr 最近一次 inject 返回值。
	injectErr error

	// entered inject 进入信号（非 nil 时发一次）。
	entered chan struct{}
	// release 非 nil：inject 阻塞直到关闭或 ctx 取消。
	release chan struct{}
}

func (f *fakeDeliverOrch) Deliver(ctx context.Context, taskID, connID, uploadID string, inject func(context.Context) error) (bool, string) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeDeliverCall{taskID: taskID, connID: connID, uploadID: uploadID})
	f.mu.Unlock()
	if f.result != nil {
		return f.result(inject)
	}
	// 默认编排：临界区内调用 inject；完整成功 → ok，其余 → write_failed。
	err := f.runInject(ctx, inject)
	if err != nil {
		return false, "write_failed"
	}
	return true, ""
}

// runInject 模拟编排写入步：进入信号 → 可选阻塞 → 注入（编排解析受管最终绝对路径
// 后经 appuploads.ContextWithInjectPath 包入 ctx，与 Lane A Deliver 步骤⑦一致）。
func (f *fakeDeliverOrch) runInject(ctx context.Context, inject func(context.Context) error) error {
	f.mu.Lock()
	f.injectRuns++
	f.mu.Unlock()
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	err := inject(appuploads.ContextWithInjectPath(ctx, "/tmp/ocdeliver-test/t1/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.png"))
	f.mu.Lock()
	f.injectErr = err
	f.mu.Unlock()
	return err
}

func (f *fakeDeliverOrch) deliverCalls() []fakeDeliverCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeDeliverCall(nil), f.calls...)
}

func (f *fakeDeliverOrch) injectRunCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.injectRuns
}

func (f *fakeDeliverOrch) lastInjectErr() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.injectErr
}

// newDeliverTestServer 构造带投递编排的 Server（写 deadline/收尾预算注入短值；
// replaceCoord 为独立 per-task 协调锁，与 handleWSTUI 注册路径配套）。
func newDeliverTestServer(fake *fakeDeliverOrch, finishBudget time.Duration) *Server {
	cfg := testConfig()
	return &Server{
		cfg:                  cfg,
		auth:                 NewTokenAuthenticator(cfg.Token),
		tasks:                &fakeTaskBackend{},
		wsClients:            newWSClientRegistry(),
		deliverOrch:          fake,
		replaceCoord:         appuploads.NewCoordination(),
		deliverWriteDeadline: 200 * time.Millisecond,
		wsFinishBudget:       finishBudget,
	}
}

// newDeliverBridgeHandler 构造与 handleWSTUI/handleWSShell 注册段一致的 bridge handler
//（registry/guard/transport/deliver scope 接线 + auth_ok connId 下发；TUI 经
// registerTUIConn 走 per-task 协调锁，shell 直接注册），返回全部连接退出后的完成信号
//（替换拓扑下 handler 会被多条连接调用，以 Once 汇合）。
func newDeliverBridgeHandler(reg *wsClientRegistry, key string, s *Server, p *pty.Pty, taskID string, isShell bool) (http.HandlerFunc, <-chan struct{}) {
	done := make(chan struct{})
	var once sync.Once
	signalDone := func() { once.Do(func() { close(done) }) }
	return func(w http.ResponseWriter, r *http.Request) {
		c, transport, err := acceptWS(w, r)
		if err != nil {
			return
		}
		defer signalDone()
		connID := newTestConnID()
		guard := &wsCloseGuard{}
		var oldConn *websocket.Conn
		var oldGuard *wsCloseGuard
		var oldCancel context.CancelFunc
		var bridgeCtx context.Context
		if isShell {
			oldConn, oldGuard, oldCancel, bridgeCtx = reg.register(key, c, guard, connID)
		} else {
			oldConn, oldGuard, oldCancel, bridgeCtx, err = s.registerTUIConn(r.Context(), taskID, c, guard, connID)
			if err != nil {
				wsClose(context.Background(), c, wsCloseInternalError, "replace coordination failed")
				return
			}
		}
		if oldConn != nil {
			if oldGuard.commitReplace() {
				wsCloseReplacedWait(oldConn)
			}
			oldCancel()
		}
		defer reg.unregister(key, c)
		_ = c.Write(r.Context(), websocket.MessageText, mustJSON(wsAuthResp{Type: "auth_ok", ConnID: connID}))
		s.bridgeTerminal(bridgeCtx, c, p, wsBridgeDeps{
			onInput: func() {
				s.tasks.RecordUserActivity(r.Context(), taskID)
			},
			transport: transport,
			guard:     guard,
			deliver:   &wsDeliverScope{taskID: taskID, connID: connID},
		})
	}, done
}

// newTestConnID 测试用连接 ID（uuid v4 同形态：8-4-4-4-12 hex）。
func newTestConnID() string {
	return "0123abcd-00ab-4cd0-9ef0-0123456789ab"
}

// startDeliverBridge 启动带投递编排的 bridge httptest server，返回 ws URL 与退出信号。
func startDeliverBridge(t *testing.T, s *Server, p *pty.Pty, taskID string) (string, <-chan struct{}) {
	t.Helper()
	reg := newWSClientRegistry()
	h, done := newDeliverBridgeHandler(reg, terminalKey(taskID, false), s, p, taskID, false)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")+"/ws", done
}

// awaitHandlerDone 有界等待 bridge handler 退出。
func awaitHandlerDone(t *testing.T, done <-chan struct{}, timeout time.Duration) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Error("bridge handler did not exit within finish budget + slack")
	}
}

// mustDeliverFrame 构造合法 deliver 文本帧。
func mustDeliverFrame(t *testing.T, uploadID string) []byte {
	t.Helper()
	b, err := json.Marshal(wsDeliverFrame{Type: "deliver", UploadID: uploadID})
	if err != nil {
		t.Fatalf("marshal deliver frame: %v", err)
	}
	return b
}

const testUploadID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1" // 32 hex

// wsSession 管理测试客户端连接与帧收集。
type wsSession struct {
	conn   *websocket.Conn
	mu     sync.Mutex
	text   []string // 收到的文本帧
	binary []byte   // 累计 binary 数据
	closed error    // 读结束错误（含关闭码）
}

// readFramesUntil 在 deadline 前循环读帧并回调判定（true = 停止读取）。
func (w *wsSession) readFramesUntil(deadline time.Duration, stop func(typ websocket.MessageType, payload []byte) bool) {
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	for {
		typ, payload, err := w.conn.Read(ctx)
		if err != nil {
			w.mu.Lock()
			w.closed = err
			w.mu.Unlock()
			return
		}
		w.mu.Lock()
		switch typ {
		case websocket.MessageText:
			w.text = append(w.text, string(payload))
		case websocket.MessageBinary:
			w.binary = append(w.binary, payload...)
		}
		w.mu.Unlock()
		if stop(typ, payload) {
			return
		}
	}
}

func (w *wsSession) textFrames() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.text...)
}

func (w *wsSession) binaryBytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.binary...)
}

func (w *wsSession) closeStatus() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return int(websocket.CloseStatus(w.closed))
}

// readInBackground 后台持续读帧直到连接关闭（配合 waitFor 轮询累计数据）。
func (w *wsSession) readInBackground() {
	go func() {
		for {
			typ, payload, err := w.conn.Read(context.Background())
			if err != nil {
				w.mu.Lock()
				w.closed = err
				w.mu.Unlock()
				return
			}
			w.mu.Lock()
			switch typ {
			case websocket.MessageText:
				w.text = append(w.text, string(payload))
			case websocket.MessageBinary:
				w.binary = append(w.binary, payload...)
			}
			w.mu.Unlock()
		}
	}()
}

// hasTextFrame 判断是否收到包含 sub 的文本帧。
func (w *wsSession) hasTextFrame(sub string) bool {
	for _, tf := range w.textFrames() {
		if strings.Contains(tf, sub) {
			return true
		}
	}
	return false
}

// readAuthOK 读到 auth_ok 帧并返回 connId。
func (w *wsSession) readAuthOK(t *testing.T) string {
	t.Helper()
	var connID string
	w.readFramesUntil(3*time.Second, func(typ websocket.MessageType, payload []byte) bool {
		if typ != websocket.MessageText {
			return false
		}
		var resp wsAuthResp
		if json.Unmarshal(payload, &resp) == nil && resp.Type == "auth_ok" {
			connID = resp.ConnID
			return true
		}
		return false
	})
	if connID == "" {
		t.Fatal("no auth_ok connId received")
	}
	return connID
}

// --- 4.1：auth_ok connId 与文本帧显式分发 ---

// TestWSAuthOK_ConnIDIssuedPerConnection 验证 auth_ok 携带 server 签发的 connId
//（每连接一个 uuid，terminal-streaming delta 认证成功帧契约）。
func TestWSAuthOK_ConnIDIssuedPerConnection(t *testing.T) {
	p := openCatPty(t)
	defer p.Close()
	tb := &fakeTaskBackend{
		attachPtyFn: func(string, int, int) (*pty.Pty, error) { return p, nil },
	}
	s := newDeliverTestServer(&fakeDeliverOrch{}, time.Second)
	s.tasks = tb
	srv := httptest.NewServer(http.HandlerFunc(s.handleWSTUI))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/terminal/t1"

	authFrame := mustJSON(wsAuthReq{Type: "auth", Token: "testtoken", Cols: 80, Rows: 24})
	readAuthOK := func() string {
		c, _, err := websocket.Dial(context.Background(), wsURL, nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer c.CloseNow()
		if err := c.Write(context.Background(), websocket.MessageText, authFrame); err != nil {
			t.Fatalf("write auth: %v", err)
		}
		_, payload, err := c.Read(context.Background())
		if err != nil {
			t.Fatalf("read auth_ok: %v", err)
		}
		var resp wsAuthResp
		if err := json.Unmarshal(payload, &resp); err != nil {
			t.Fatalf("unmarshal auth_ok: %v", err)
		}
		if resp.Type != "auth_ok" {
			t.Fatalf("type=%q want auth_ok", resp.Type)
		}
		return resp.ConnID
	}

	connID1 := readAuthOK()
	if !isUUIDShape(connID1) {
		t.Fatalf("auth_ok connId %q not a uuid", connID1)
	}
	connID2 := readAuthOK()
	if !isUUIDShape(connID2) {
		t.Fatalf("auth_ok connId %q not a uuid", connID2)
	}
	if connID1 == connID2 {
		t.Fatalf("connId must differ per connection, got %q twice", connID1)
	}
}

// isUUIDShape 校验 uuid 形态（8-4-4-4-12 hex）。
func isUUIDShape(s string) bool {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	hex := strings.ReplaceAll(s, "-", "")
	if len(hex) != 32 {
		return false
	}
	for i := 0; i < len(hex); i++ {
		c := hex[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// TestWSTextFrameDispatch_DropsUnrecognized（4.1）：非法 JSON / 未知 type / 非法
// uploadId 的 deliver 帧一律丢弃+日志：零 PTY 写入、零回执、零 onInput、零编排调用；
// resize 沿用既有处理（不校验 uploadId），后续帧继续处理。
func TestWSTextFrameDispatch_DropsUnrecognized(t *testing.T) {
	p := openCatPty(t)
	defer p.Close()
	fake := &fakeDeliverOrch{}
	s := newDeliverTestServer(fake, time.Second)
	url, done := startDeliverBridge(t, s, p, "t1")
	defer awaitHandlerDone(t, done, 3*time.Second)

	c, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	w := &wsSession{conn: c}
	defer c.CloseNow()

	ctx := context.Background()
	frames := [][]byte{
		[]byte("not-json"),                                                        // 非法 JSON
		[]byte(`{"type":"mystery"}`),                                              // 未知 type
		[]byte(`{"type":"deliver"}`),                                              // 缺 uploadId
		[]byte(`{"type":"deliver","uploadId":"short"}`),                           // 非 32 hex
		[]byte(`{"type":"deliver","uploadId":"` + strings.Repeat("g", 32) + `"}`), // 非 hex 字符
		[]byte(`{"type":"resize","cols":100,"rows":30}`),                          // resize 正常处理
	}
	for _, frame := range frames {
		if err := c.Write(ctx, websocket.MessageText, frame); err != nil {
			t.Fatalf("write frame: %v", err)
		}
	}
	// 屏障：END-MARK 出现在回显中即证明此前全部帧已串行处理完毕。
	if err := c.Write(ctx, websocket.MessageBinary, []byte("END-MARK\n")); err != nil {
		t.Fatalf("write barrier: %v", err)
	}
	w.readFramesUntil(3*time.Second, func(typ websocket.MessageType, payload []byte) bool {
		return typ == websocket.MessageBinary && strings.Contains(string(payload), "END-MARK")
	})

	if calls := fake.deliverCalls(); len(calls) != 0 {
		t.Errorf("deliver orchestrator called for invalid frames: %+v", calls)
	}
	if n := fake.injectRunCount(); n != 0 {
		t.Errorf("inject ran %d times for invalid frames, want 0", n)
	}
	// 屏障后的断言：onInput 恰为 1 次（屏障 END-MARK 为非空 binary 输入帧，自身上报；
	// 被丢弃的帧 MUST NOT 上报）。
	if acts := tbActivity(s); len(acts) != 1 {
		t.Errorf("activity calls = %v, want exactly 1 (barrier frame only)", acts)
	}
	if w.hasTextFrame("deliver_result") {
		t.Errorf("dropped frames must not get receipt, got %v", w.textFrames())
	}
	if bin := string(w.binaryBytes()); strings.Contains(bin, "not-json") || strings.Contains(bin, "mystery") {
		t.Errorf("dropped text frames leaked into PTY echo: %q", bin)
	}
}

// tbActivity 读取 fakeTaskBackend 活动上报快照。
func tbActivity(s *Server) []string {
	tb := s.tasks.(*fakeTaskBackend)
	return tb.recordedActivityCalls()
}

// --- 4.2：deliver 处理（WS 适配） ---

// TestWSDeliver_SuccessReceiptOnBoundedQueue（4.2）：合法 deliver → onInput → 编排
//（connId 绑定）→ bracketed paste 写入 PTY → deliver_result 经共享有界写队列回帧，
// 帧形状逐字符合同。
func TestWSDeliver_SuccessReceiptOnBoundedQueue(t *testing.T) {
	p := openCatPty(t)
	defer p.Close()
	fake := &fakeDeliverOrch{}
	s := newDeliverTestServer(fake, time.Second)
	// 本用例是唯一走真实 (*pty.Pty).WriteTimeout 的 deliver 用例：注入写期限必须大于
	// pty.WriteTimeout 的原生 deadline 预留（writeAbortGrace=500ms），否则原生写
	// deadline 落在过去、写立即以 i/o timeout 失败（Linux CI write_failed 0.00s 的
	// 根因：Linux ptmx 经 os.OpenFile 创建、受 runtime poller 管理，deadline 生效；
	// darwin ptmx 不受 poller 管理、SetWriteDeadline 被静默忽略，会掩盖该配置错误）。
	s.deliverWriteDeadline = 2 * time.Second
	url, done := startDeliverBridge(t, s, p, "t1")
	defer awaitHandlerDone(t, done, 3*time.Second)

	c, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	w := &wsSession{conn: c}
	defer c.CloseNow()

	connID := w.readAuthOK(t)
	if err := c.Write(context.Background(), websocket.MessageText, mustDeliverFrame(t, testUploadID)); err != nil {
		t.Fatalf("write deliver: %v", err)
	}

	// 回执帧形状逐字符合同（成功无 error 字段）；失败输出附最近一次注入错误，
	// 避免 receipt 断言先 fatal 掩盖真实写入失败原因。
	wantReceipt := fmt.Sprintf(`{"type":"deliver_result","uploadId":%q,"ok":true}`, testUploadID)
	w.readFramesUntil(3*time.Second, func(typ websocket.MessageType, payload []byte) bool {
		return typ == websocket.MessageText && strings.Contains(string(payload), "deliver_result")
	})
	if !w.hasTextFrame(wantReceipt) {
		t.Fatalf("receipt %v, want exact frame %q (last inject err = %v)", w.textFrames(), wantReceipt, fake.lastInjectErr())
	}

	// 编排调用绑定 (taskID, connId, uploadId)。
	calls := fake.deliverCalls()
	if len(calls) != 1 || calls[0].taskID != "t1" || calls[0].connID != connID || calls[0].uploadID != testUploadID {
		t.Fatalf("orchestrator calls = %+v, want one call bound to (t1, %s, %s)", calls, connID, testUploadID)
	}
	// 注入完整成功（inject 返回 nil）。
	if err := fake.lastInjectErr(); err != nil {
		t.Errorf("inject err = %v, want nil", err)
	}
	// 合法 deliver 视为用户活动（onInput，PTY 写入前）。
	if acts := tbActivity(s); len(acts) != 1 {
		t.Errorf("activity calls = %v, want exactly 1 for valid deliver", acts)
	}
	// bracketed paste 到达 PTY（cat 回显；ESC 序列经终端 echo 转义，以可打印路径断言
	// 载荷到达）。回执读取在 deliver_result 处停止，此后转入后台持续读帧等待回显。
	w.readInBackground()
	waitFor(t, 2*time.Second, "path echo in PTY output", func() bool {
		return strings.Contains(string(w.binaryBytes()), "/tmp/ocdeliver-test/t1/")
	})
}

// TestWSDeliver_AdmissionRejectContinuesConnection（4.2）：注入前准入拒绝
//（not_found 等确定零写入）→ 回执 ok:false + error=code，连接继续处理后续帧。
func TestWSDeliver_AdmissionRejectContinuesConnection(t *testing.T) {
	p := openCatPty(t)
	defer p.Close()
	fake := &fakeDeliverOrch{result: func(inject func(context.Context) error) (bool, string) {
		return false, "not_found"
	}}
	s := newDeliverTestServer(fake, time.Second)
	url, done := startDeliverBridge(t, s, p, "t1")
	defer awaitHandlerDone(t, done, 3*time.Second)

	c, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	w := &wsSession{conn: c}
	defer c.CloseNow()
	ctx := context.Background()

	if err := c.Write(ctx, websocket.MessageText, mustDeliverFrame(t, testUploadID)); err != nil {
		t.Fatalf("write deliver: %v", err)
	}
	wantReceipt := fmt.Sprintf(`{"type":"deliver_result","uploadId":%q,"ok":false,"error":"not_found"}`, testUploadID)
	w.readFramesUntil(3*time.Second, func(typ websocket.MessageType, payload []byte) bool {
		return typ == websocket.MessageText && strings.Contains(string(payload), "deliver_result")
	})
	if !w.hasTextFrame(wantReceipt) {
		t.Fatalf("receipt %v, want exact frame %q", w.textFrames(), wantReceipt)
	}
	if n := fake.injectRunCount(); n != 0 {
		t.Errorf("inject ran %d times for admission reject, want 0 (zero PTY write)", n)
	}
	// 连接继续：后续 binary 帧仍被处理（cat 回显）。
	if err := c.Write(ctx, websocket.MessageBinary, []byte("AFTER-REJECT\n")); err != nil {
		t.Fatalf("write after reject: %v", err)
	}
	w.readFramesUntil(3*time.Second, func(typ websocket.MessageType, payload []byte) bool {
		return typ == websocket.MessageBinary && strings.Contains(string(payload), "AFTER-REJECT")
	})
	if !strings.Contains(string(w.binaryBytes()), "AFTER-REJECT") {
		t.Error("connection should continue processing input after admission reject")
	}
}

// TestWSDeliver_ShellRejected（shell 连接 deliver 一律拒绝）：shell 连接经编排
// 第 1 步（终端类型/连接当前性）拒绝 → forbidden 回执、零 inject、零 PTY 写入。
func TestWSDeliver_ShellRejected(t *testing.T) {
	p := openCatPty(t)
	defer p.Close()
	fake := &fakeDeliverOrch{result: func(inject func(context.Context) error) (bool, string) {
		return false, "forbidden"
	}}
	s := newDeliverTestServer(fake, time.Second)
	reg := newWSClientRegistry()
	h, done := newDeliverBridgeHandler(reg, terminalKey("shell-t1", true), s, p, "shell-t1", true)
	srv := httptest.NewServer(h)
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	c, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	w := &wsSession{conn: c}
	// defer 顺序（LIFO）：先关连接再等待 handler 退出。
	defer awaitHandlerDone(t, done, 3*time.Second)
	defer c.CloseNow()

	if err := c.Write(context.Background(), websocket.MessageText, mustDeliverFrame(t, testUploadID)); err != nil {
		t.Fatalf("write deliver: %v", err)
	}
	wantReceipt := fmt.Sprintf(`{"type":"deliver_result","uploadId":%q,"ok":false,"error":"forbidden"}`, testUploadID)
	w.readFramesUntil(3*time.Second, func(typ websocket.MessageType, payload []byte) bool {
		return typ == websocket.MessageText && strings.Contains(string(payload), "deliver_result")
	})
	if !w.hasTextFrame(wantReceipt) {
		t.Fatalf("shell deliver receipt %v, want %q", w.textFrames(), wantReceipt)
	}
	if n := fake.injectRunCount(); n != 0 {
		t.Errorf("shell deliver inject ran %d times, want 0", n)
	}
	if strings.Contains(string(w.binaryBytes()), "/tmp/ocdeliver-test") {
		t.Error("shell deliver must not write PTY")
	}
}

// --- 4.3：bridge 收尾（故障注入） ---

// stubDeliverPTYWrite 替换投递写入入口（恢复句柄登记在 cleanup）。
func stubDeliverPTYWrite(t *testing.T, stub func(p *pty.Pty, ctx context.Context, data []byte, deadline time.Duration) (int, error)) {
	t.Helper()
	old := deliverPTYWrite
	deliverPTYWrite = stub
	t.Cleanup(func() { deliverPTYWrite = old })
}

// assertWriteFailureCloseout 「非超时写失败」三项 MUST 断言（tasks 4.3）：
// write_failed 回执尝试、关闭码 1011（故障先提交）、后续零 PTY 写入。
func assertWriteFailureCloseout(t *testing.T, w *wsSession, uploadID string) {
	t.Helper()
	// 回执尝试：write_failed 回执先于关闭帧。
	wantReceipt := fmt.Sprintf(`{"type":"deliver_result","uploadId":%q,"ok":false,"error":"write_failed"}`, uploadID)
	w.readFramesUntil(3*time.Second, func(typ websocket.MessageType, payload []byte) bool {
		return typ == websocket.MessageText && strings.Contains(string(payload), "deliver_result")
	})
	if !w.hasTextFrame(wantReceipt) {
		t.Fatalf("receipts %v, want %q", w.textFrames(), wantReceipt)
	}
	// 关闭码：故障先提交 → 1011。
	w.readFramesUntil(3*time.Second, func(typ websocket.MessageType, payload []byte) bool { return false })
	if got := w.closeStatus(); got != wsCloseInternalError {
		t.Fatalf("close status = %d, want %d (1011, fault committed first)", got, wsCloseInternalError)
	}
	// 后续零 PTY 写入：注入载荷未出现在 PTY 回显（不补写、不重写）。
	if strings.Contains(string(w.binaryBytes()), "/tmp/ocdeliver-test") {
		t.Error("PTY must not receive injection payload after write failure")
	}
}

// TestWSDeliver_ImmediateWriteError（「立即写错误」）：底层写立即失败（EIO）→
// write_failed 回执 + 关闭码 1011 + 后续零 PTY 写入 + 不补写（写入恰一次）。
func TestWSDeliver_ImmediateWriteError(t *testing.T) {
	p := openCatPty(t)
	defer p.Close()
	fake := &fakeDeliverOrch{}
	s := newDeliverTestServer(fake, time.Second)
	url, done := startDeliverBridge(t, s, p, "t1")
	defer awaitHandlerDone(t, done, 3*time.Second)

	stubDeliverPTYWrite(t, func(_ *pty.Pty, _ context.Context, _ []byte, _ time.Duration) (int, error) {
		return 0, errors.New("write /dev/ptmx: input/output error")
	})

	c, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	w := &wsSession{conn: c}
	defer c.CloseNow()

	if err := c.Write(context.Background(), websocket.MessageText, mustDeliverFrame(t, testUploadID)); err != nil {
		t.Fatalf("write deliver: %v", err)
	}
	assertWriteFailureCloseout(t, w, testUploadID)
	if n := fake.injectRunCount(); n != 1 {
		t.Errorf("inject ran %d times, want exactly 1 (no rewrite)", n)
	}
	// 写失败不撤销已观察到的用户活动（onInput 在 PTY 写入前）。
	if acts := tbActivity(s); len(acts) != 1 {
		t.Errorf("activity calls = %v, want 1 (reported before PTY write)", acts)
	}
}

// TestWSDeliver_ShortWriteNoError（「无错误短写」）：底层写返回 n < len 且 err == nil
// → 不视为成功，write_failed + 关闭码 1011 + 后续零 PTY 写入。
func TestWSDeliver_ShortWriteNoError(t *testing.T) {
	p := openCatPty(t)
	defer p.Close()
	fake := &fakeDeliverOrch{}
	s := newDeliverTestServer(fake, time.Second)
	url, done := startDeliverBridge(t, s, p, "t1")
	defer awaitHandlerDone(t, done, 3*time.Second)

	stubDeliverPTYWrite(t, func(_ *pty.Pty, _ context.Context, data []byte, _ time.Duration) (int, error) {
		return len(data) / 2, nil // 无错误短写
	})

	c, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	w := &wsSession{conn: c}
	defer c.CloseNow()

	if err := c.Write(context.Background(), websocket.MessageText, mustDeliverFrame(t, testUploadID)); err != nil {
		t.Fatalf("write deliver: %v", err)
	}
	assertWriteFailureCloseout(t, w, testUploadID)
	if n := fake.injectRunCount(); n != 1 {
		t.Errorf("inject ran %d times, want exactly 1 (no rewrite)", n)
	}
}

// TestWSDeliver_InfraFailureCloses1011NoReceipt（D5 基础设施错误语义）：编排返回
// 六枚举之外结果（基础设施故障）→ 不伪造业务回执、零 PTY 写入、1011 结束连接。
func TestWSDeliver_InfraFailureCloses1011NoReceipt(t *testing.T) {
	p := openCatPty(t)
	defer p.Close()
	fake := &fakeDeliverOrch{result: func(inject func(context.Context) error) (bool, string) {
		return false, "internal"
	}}
	s := newDeliverTestServer(fake, time.Second)
	url, done := startDeliverBridge(t, s, p, "t1")
	defer awaitHandlerDone(t, done, 3*time.Second)

	c, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	w := &wsSession{conn: c}
	defer c.CloseNow()

	if err := c.Write(context.Background(), websocket.MessageText, mustDeliverFrame(t, testUploadID)); err != nil {
		t.Fatalf("write deliver: %v", err)
	}
	w.readFramesUntil(3*time.Second, func(typ websocket.MessageType, payload []byte) bool { return false })
	if got := w.closeStatus(); got != wsCloseInternalError {
		t.Fatalf("close status = %d, want %d (1011 infra)", got, wsCloseInternalError)
	}
	if w.hasTextFrame("deliver_result") {
		t.Errorf("infra failure must not fabricate business receipt, got %v", w.textFrames())
	}
	if n := fake.injectRunCount(); n != 0 {
		t.Errorf("inject ran %d times on infra failure, want 0 (zero PTY call)", n)
	}
}

// TestWSDeliver_BlockedPTYWriteBoundedFail（「PTY 不消费写阻塞」，api 层）：写入无法
// 完成时写期限内有界返回，回执与故障收尾在收尾预算内完成（写期限/收尾预算注入短值，
// 模拟生产 5s/1s 比例；真实底层退出契约由 pty 包测试覆盖）。
func TestWSDeliver_BlockedPTYWriteBoundedFail(t *testing.T) {
	p := openCatPty(t)
	defer p.Close()
	fake := &fakeDeliverOrch{}
	s := newDeliverTestServer(fake, 400*time.Millisecond)
	url, done := startDeliverBridge(t, s, p, "t1")
	defer awaitHandlerDone(t, done, 2*time.Second)

	// 模拟不消费写的底层：阻塞至写期限后按短写返回（与 pty.WriteTimeout 的有界退出同语义）。
	stubDeliverPTYWrite(t, func(_ *pty.Pty, ctx context.Context, data []byte, deadline time.Duration) (int, error) {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(deadline):
			return len(data) / 3, nil
		}
	})

	c, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	w := &wsSession{conn: c}
	defer c.CloseNow()

	start := time.Now()
	if err := c.Write(context.Background(), websocket.MessageText, mustDeliverFrame(t, testUploadID)); err != nil {
		t.Fatalf("write deliver: %v", err)
	}
	assertWriteFailureCloseout(t, w, testUploadID)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("blocked write closeout took %v, want within write deadline + finish budget + slack", elapsed)
	}
}

// TestWSDeliver_EOFDoesNotCancelReceipt（「PTY EOF 先到」）：PTY EOF 与投递写入失败
// 并发时，EOF MUST NOT 抢先取消已入队回执——客户端仍收到 write_failed 回执，随后
// 故障收尾 1011。
func TestWSDeliver_EOFDoesNotCancelReceipt(t *testing.T) {
	p := openCatPty(t)
	defer p.Close()
	fake := &fakeDeliverOrch{entered: make(chan struct{}), release: make(chan struct{})}
	s := newDeliverTestServer(fake, time.Second)
	url, done := startDeliverBridge(t, s, p, "t1")
	defer awaitHandlerDone(t, done, 3*time.Second)

	c, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	w := &wsSession{conn: c}
	defer c.CloseNow()

	if err := c.Write(context.Background(), websocket.MessageText, mustDeliverFrame(t, testUploadID)); err != nil {
		t.Fatalf("write deliver: %v", err)
	}
	select {
	case <-fake.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("inject never entered critical section")
	}
	// PTY EOF 先到（attach 客户端被终止）。
	p.Close()

	// 编排在连接取消后写入终止 → write_failed；EOF 不得抢取消已入队回执。
	wantReceipt := fmt.Sprintf(`{"type":"deliver_result","uploadId":%q,"ok":false,"error":"write_failed"}`, testUploadID)
	w.readFramesUntil(3*time.Second, func(typ websocket.MessageType, payload []byte) bool {
		return typ == websocket.MessageText && strings.Contains(string(payload), "deliver_result")
	})
	if !w.hasTextFrame(wantReceipt) {
		t.Fatalf("receipts after PTY EOF = %v, want %q (EOF must not cancel queued receipt)", w.textFrames(), wantReceipt)
	}
	w.readFramesUntil(3*time.Second, func(typ websocket.MessageType, payload []byte) bool { return false })
	if got := w.closeStatus(); got != wsCloseInternalError {
		t.Fatalf("close status = %d, want %d (1011)", got, wsCloseInternalError)
	}
}

// TestWSDeliver_WSCancelTerminatesWrite（「WS 先取消」）：bridge ctx（连接被服务端
// 取消：替换/请求取消）先于写完成取消 → 写入终止 → write_failed 回执 + 1011 收尾，
// 全程有界不挂起。
func TestWSDeliver_WSCancelTerminatesWrite(t *testing.T) {
	p := openCatPty(t)
	defer p.Close()
	fake := &fakeDeliverOrch{entered: make(chan struct{})}
	s := newDeliverTestServer(fake, 400*time.Millisecond)
	// 写入在 ctx 取消前一直阻塞（写真实在途时取消连接），取消后终止 → write_failed。
	stubDeliverPTYWrite(t, func(_ *pty.Pty, ctx context.Context, _ []byte, deadline time.Duration) (int, error) {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(deadline):
			return 0, errors.New("write deadline exceeded")
		}
	})

	bridgeCtx, cancelBridge := context.WithCancel(context.Background())
	handlerDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		c, transport, err := acceptWS(w, r)
		if err != nil {
			return
		}
		connID := newTestConnID()
		guard := &wsCloseGuard{}
		_ = c.Write(r.Context(), websocket.MessageText, mustJSON(wsAuthResp{Type: "auth_ok", ConnID: connID}))
		s.bridgeTerminal(bridgeCtx, c, p, wsBridgeDeps{
			transport: transport,
			guard:     guard,
			deliver:   &wsDeliverScope{taskID: "t1", connID: connID},
		})
	}))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	c, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	w := &wsSession{conn: c}
	defer c.CloseNow()

	if err := c.Write(context.Background(), websocket.MessageText, mustDeliverFrame(t, testUploadID)); err != nil {
		t.Fatalf("write deliver: %v", err)
	}
	select {
	case <-fake.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("inject never entered critical section")
	}
	start := time.Now()
	cancelBridge() // 连接取消先于写完成

	wantReceipt := fmt.Sprintf(`{"type":"deliver_result","uploadId":%q,"ok":false,"error":"write_failed"}`, testUploadID)
	w.readFramesUntil(3*time.Second, func(typ websocket.MessageType, payload []byte) bool {
		return typ == websocket.MessageText && strings.Contains(string(payload), "deliver_result")
	})
	if !w.hasTextFrame(wantReceipt) {
		t.Fatalf("receipts after WS cancel = %v, want %q", w.textFrames(), wantReceipt)
	}
	w.readFramesUntil(3*time.Second, func(typ websocket.MessageType, payload []byte) bool { return false })
	if got := w.closeStatus(); got != wsCloseInternalError {
		t.Fatalf("close status = %d, want %d (1011, fault committed, no replacement)", got, wsCloseInternalError)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("closeout after cancel took %v, want bounded", elapsed)
	}
	awaitHandlerDone(t, handlerDone, 3*time.Second)
}

// TestWSDeliver_QueueFullNoReceiptBoundTeardown（「writer 队列满」）：慢客户端使共享
// 有界写队列满 → 回执不保证到达（丢弃+日志）、bridge 有界断开收尾，不挂起。
func TestWSDeliver_QueueFullNoReceiptBoundTeardown(t *testing.T) {
	p := openCatPty(t)
	defer p.Close()
	fake := &fakeDeliverOrch{entered: make(chan struct{}), release: make(chan struct{})}
	s := newDeliverTestServer(fake, 400*time.Millisecond)
	url, done := startDeliverBridge(t, s, p, "t1")
	defer awaitHandlerDone(t, done, 2*time.Second)

	c, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()
	// 客户端不读（慢客户端）。

	if err := c.Write(context.Background(), websocket.MessageText, mustDeliverFrame(t, testUploadID)); err != nil {
		t.Fatalf("write deliver: %v", err)
	}
	select {
	case <-fake.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("inject never entered critical section")
	}
	// 洪灌 PTY 输出（cat 回显）填满共享有界写队列并使 writer 阻塞在 c.Write。
	floodDone := make(chan struct{})
	go func() {
		defer close(floodDone)
		chunk := []byte(strings.Repeat("x\n", 16*1024))
		for i := 0; i < 128; i++ {
			if _, err := p.Write(chunk); err != nil {
				return
			}
		}
	}()
	<-floodDone

	// 队列满触发断开（queue-full 断开同时取消 loopCtx → inject 经 ctx 取消终止），
	// 回执入队时队列已满 → 丢弃（不保证回执）。断言收尾有界退出。
	start := time.Now()
	close(fake.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("bridge handler did not exit after queue-full disconnect within budget + slack")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("teardown after queue-full took %v, want within finish budget + slack", elapsed)
	}
}

// replaceRaceHarness 双连接替换拓扑（「故障/替换并发」仲裁测试用）。
type replaceRaceHarness struct {
	srv  *httptest.Server
	c1   *websocket.Conn
	fake *fakeDeliverOrch
	done <-chan struct{}
}

// startReplaceHarness 启动单注册表双连接替换 handler，建立第一条连接 c1。
func startReplaceHarness(t *testing.T, fake *fakeDeliverOrch, finishBudget time.Duration) *replaceRaceHarness {
	p := openCatPty(t)
	t.Cleanup(func() { p.Close() })
	s := newDeliverTestServer(fake, finishBudget)
	reg := newWSClientRegistry()
	h, done := newDeliverBridgeHandler(reg, terminalKey("t1", false), s, p, "t1", false)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	c1, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial c1: %v", err)
	}
	t.Cleanup(func() { c1.CloseNow() })
	return &replaceRaceHarness{srv: srv, c1: c1, fake: fake, done: done}
}

func (h *replaceRaceHarness) url() string {
	return "ws" + strings.TrimPrefix(h.srv.URL, "http") + "/ws"
}

// readC1Close 有界读取 c1 直到连接关闭，返回关闭错误。
func (h *replaceRaceHarness) readC1Close(t *testing.T) error {
	t.Helper()
	errCh := make(chan error, 1)
	go func() {
		for {
			_, _, rerr := h.c1.Read(context.Background())
			if rerr != nil {
				errCh <- rerr
				return
			}
		}
	}()
	select {
	case err := <-errCh:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("c1 did not receive close")
		return nil
	}
}

// TestWSDeliver_ReplaceCommitsBeforeFault（「替换先提交」）：替换先提交关闭原因
//（4009），后续写失败 MUST NOT 覆盖——旧连接关闭码保持 4009。
func TestWSDeliver_ReplaceCommitsBeforeFault(t *testing.T) {
	fake := &fakeDeliverOrch{entered: make(chan struct{}), release: make(chan struct{})}
	hn := startReplaceHarness(t, fake, time.Second)
	defer awaitHandlerDone(t, hn.done, 3*time.Second)
	// 写失败（非超时立即错误）：替换提交后的写失败 MUST NOT 覆盖关闭原因。
	stubDeliverPTYWrite(t, func(_ *pty.Pty, _ context.Context, _ []byte, _ time.Duration) (int, error) {
		return 0, errors.New("write /dev/ptmx: input/output error")
	})

	if err := hn.c1.Write(context.Background(), websocket.MessageText, mustDeliverFrame(t, testUploadID)); err != nil {
		t.Fatalf("c1 write deliver: %v", err)
	}
	select {
	case <-fake.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("inject never entered critical section")
	}

	// 替换先提交：第二条连接注册 → commitReplace 赢得关闭 → 4009。
	c2, _, err := websocket.Dial(context.Background(), hn.url(), nil)
	if err != nil {
		t.Fatalf("dial c2: %v", err)
	}
	defer c2.CloseNow()

	if got := int(websocket.CloseStatus(hn.readC1Close(t))); got != wsCloseReplaced {
		t.Fatalf("c1 close = %d, want %d (4009, replace committed first)", got, wsCloseReplaced)
	}

	// 替换提交后写失败：MUST NOT 覆盖关闭原因（回执入队尝试允许）。
	close(fake.release)
}

// TestWSDeliver_FaultCommitsBeforeReplace（「故障先提交」）：故障收尾先提交 1011，
// 替换 handler MUST NOT 另发 4009——旧连接关闭码恒为 1011。
func TestWSDeliver_FaultCommitsBeforeReplace(t *testing.T) {
	fake := &fakeDeliverOrch{entered: make(chan struct{}), release: make(chan struct{})}
	hn := startReplaceHarness(t, fake, time.Second)
	defer awaitHandlerDone(t, hn.done, 3*time.Second)
	// 写失败（非超时立即错误）：故障收尾先提交 1011。
	stubDeliverPTYWrite(t, func(_ *pty.Pty, _ context.Context, _ []byte, _ time.Duration) (int, error) {
		return 0, errors.New("write /dev/ptmx: input/output error")
	})

	if err := hn.c1.Write(context.Background(), websocket.MessageText, mustDeliverFrame(t, testUploadID)); err != nil {
		t.Fatalf("c1 write deliver: %v", err)
	}
	select {
	case <-fake.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("inject never entered critical section")
	}

	// 故障先提交：写失败完成故障收尾（1011）。
	close(fake.release)
	if got := int(websocket.CloseStatus(hn.readC1Close(t))); got != wsCloseInternalError {
		t.Fatalf("c1 close = %d, want %d (1011, fault committed first)", got, wsCloseInternalError)
	}

	// 故障提交后的替换：新连接照常建立；旧连接保持已关闭（不再收到 4009）。
	c2, _, err := websocket.Dial(context.Background(), hn.url(), nil)
	if err != nil {
		t.Fatalf("dial c2 after fault: %v", err)
	}
	defer c2.CloseNow()
	if _, _, err := hn.c1.Read(context.Background()); err == nil {
		t.Fatal("c1 should stay closed after fault close")
	}
}

// TestWSDeliver_NonTimeoutFailureReplaceRace（「非超时失败与连接替换竞争」）：立即写
// 错误与连接替换并发——关闭码线性化为先提交者（4009/1011 二选一），收尾有界退出。
func TestWSDeliver_NonTimeoutFailureReplaceRace(t *testing.T) {
	fake := &fakeDeliverOrch{}
	hn := startReplaceHarness(t, fake, 400*time.Millisecond)
	defer awaitHandlerDone(t, hn.done, 3*time.Second)
	stubDeliverPTYWrite(t, func(_ *pty.Pty, _ context.Context, _ []byte, _ time.Duration) (int, error) {
		return 0, errors.New("write /dev/ptmx: input/output error")
	})

	if err := hn.c1.Write(context.Background(), websocket.MessageText, mustDeliverFrame(t, testUploadID)); err != nil {
		t.Fatalf("c1 write deliver: %v", err)
	}
	// 与写失败并发发起替换。
	c2, _, err := websocket.Dial(context.Background(), hn.url(), nil)
	if err != nil {
		t.Fatalf("dial c2: %v", err)
	}
	defer c2.CloseNow()

	got := int(websocket.CloseStatus(hn.readC1Close(t)))
	if got != wsCloseReplaced && got != wsCloseInternalError {
		t.Fatalf("c1 close = %d, want exactly one of 4009/1011 (first committer wins)", got)
	}
}

// --- 「回执已写出但对端不回关闭帧」 ---

// dialRawWS 手工完成 WS 升级（绕过 coder 客户端的关闭握手自动应答），返回原始连接，
// 用于模拟"收到关闭帧但不回应"的对端。
func dialRawWS(t *testing.T, rawURL string) net.Conn {
	t.Helper()
	host := strings.TrimPrefix(strings.TrimPrefix(rawURL, "http://"), "ws://")
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	conn, err := net.DialTimeout("tcp", host, 3*time.Second)
	if err != nil {
		t.Fatalf("dial tcp: %v", err)
	}
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	req := "GET /ws HTTP/1.1\r\nHost: " + host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write upgrade: %v", err)
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read upgrade response: %v", err)
	}
	if !strings.Contains(line, "101") {
		t.Fatalf("upgrade response = %q, want 101", line)
	}
	for {
		l, err := br.ReadString('\n')
		if err != nil || l == "\r\n" {
			break
		}
	}
	return conn
}

// TestWSDeliver_ReceiptWrittenPeerIgnoresCloseFrame（delivery spec:311）：write_failed
// 回执已成功写出后发起故障收尾，对端收到关闭帧但不回应——实际连接关闭与收尾退出
// 均在独立收尾预算内完成（预算耗尽经 transport 适配边界直接关底层连接）。
func TestWSDeliver_ReceiptWrittenPeerIgnoresCloseFrame(t *testing.T) {
	p := openCatPty(t)
	defer p.Close()
	const finishBudget = 400 * time.Millisecond
	fake := &fakeDeliverOrch{}
	s := newDeliverTestServer(fake, finishBudget)
	stubDeliverPTYWrite(t, func(_ *pty.Pty, _ context.Context, data []byte, _ time.Duration) (int, error) {
		return len(data) / 2, nil // 无错误短写 → write_failed
	})

	reg := newWSClientRegistry()
	h, done := newDeliverBridgeHandler(reg, terminalKey("t1", false), s, p, "t1", false)
	srv := httptest.NewServer(h)
	defer srv.Close()

	conn := dialRawWS(t, srv.URL+"/ws")
	defer conn.Close()

	// 发送 deliver 帧（客户端帧必须 mask）。
	if err := writeMaskedFrame(conn, 0x1, mustDeliverFrame(t, testUploadID)); err != nil {
		t.Fatalf("write deliver: %v", err)
	}

	// 读回执（已写出），随后读服务端关闭帧但不回应。
	br := bufio.NewReader(conn)
	gotReceipt := false
	gotClose := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		opcode, payload, err := readServerFrame(br)
		if err != nil {
			break
		}
		if opcode == 0x1 && strings.Contains(string(payload), "deliver_result") {
			gotReceipt = true
		}
		if opcode == 0x8 {
			gotClose = true
			break // 收到关闭帧：不回应，保持 TCP 打开
		}
	}
	if !gotReceipt {
		t.Fatal("write_failed receipt not written before fault close")
	}
	if !gotClose {
		t.Fatal("server close frame not received")
	}

	// 对端不回关闭帧：连接关闭与 handler 退出必须在收尾预算内完成。
	select {
	case <-done:
	case <-time.After(finishBudget + time.Second):
		t.Fatalf("handler did not exit within finish budget (%v)", finishBudget)
	}
	// 底层 transport 已被强制关闭：对端随后的读出错（EOF/连接复位）。
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Error("peer connection should be force-closed after finish budget")
	}
}

// TestInjectDeliverPaste_PayloadAndClassification（注入单一函数边界）：载荷构造
//（ESC[200~+路径+ESC[201~，单个完整消息、无回车）与完整成功分类（仅 n == len &&
// err == nil 返回 nil；短写/立即错误/缺路径均失败且不触达 PTY）。
func TestInjectDeliverPaste_PayloadAndClassification(t *testing.T) {
	s := newDeliverTestServer(&fakeDeliverOrch{}, 200*time.Millisecond)
	const path = "/tmp/ocdeliver-test/t1/file.png"
	want := bracketedPasteStart + path + bracketedPasteEnd
	ctx := appuploads.ContextWithInjectPath(context.Background(), path)

	cases := []struct {
		name    string
		n       int
		err     error
		ctxPath string
		wantErr bool
	}{
		{"complete write", len(want), nil, path, false},
		{"short write no error", len(want) / 2, nil, path, true},
		{"immediate error", 0, errors.New("EIO"), path, true},
		{"partial write with error", 3, errors.New("EIO"), path, true},
		{"missing ctx path", len(want), nil, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []byte
			var calls int
			old := deliverPTYWrite
			deliverPTYWrite = func(_ *pty.Pty, _ context.Context, data []byte, deadline time.Duration) (int, error) {
				calls++
				got = data
				if deadline <= 0 {
					t.Error("inject must pass positive write deadline")
				}
				return tc.n, tc.err
			}
			defer func() { deliverPTYWrite = old }()

			callCtx := ctx
			if tc.ctxPath == "" {
				callCtx = context.Background()
			}
			err := s.injectDeliverPaste(callCtx, nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("injectDeliverPaste err = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.ctxPath != "" && string(got) != want {
				t.Errorf("payload = %q, want %q", got, want)
			}
			if tc.ctxPath == "" && calls != 0 {
				t.Errorf("missing ctx path must not reach PTY write, calls = %d", calls)
			}
		})
	}
}

// --- R10：控制回执入队失败 → 停止输入泵 + 有界收尾（慢客户端统一断开） ---

// TestDeliverFrameHandler_QueueFullReceiptStopsInput（单元边界）：共享写队列已满时，
// ok/准入拒绝回执入队失败 → deliverFn 返回 false 停止输入泵且不提交故障收尾（慢客户端
// 断开非故障）；write_failed 仍提交故障收尾（1011 语义不受回执可达性影响）。
func TestDeliverFrameHandler_QueueFullReceiptStopsInput(t *testing.T) {
	newFullQueue := func() chan wsOutFrame {
		q := make(chan wsOutFrame, wsWriteQueueCap)
		for i := 0; i < wsWriteQueueCap; i++ {
			q <- wsOutFrame{typ: websocket.MessageBinary, data: []byte("pad")}
		}
		return q
	}
	cases := []struct {
		name      string
		result    func(func(context.Context) error) (bool, string)
		wantFault bool
	}{
		{"ok receipt", func(func(context.Context) error) (bool, string) { return true, "" }, false},
		{"admission reject", func(func(context.Context) error) (bool, string) { return false, "not_found" }, false},
		{"write_failed", func(func(context.Context) error) (bool, string) { return false, "write_failed" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := openCatPty(t)
			t.Cleanup(func() { p.Close() })
			s := newDeliverTestServer(&fakeDeliverOrch{result: tc.result}, time.Second)
			guard := &wsCloseGuard{}
			var faultOwned atomic.Bool
			handler := s.deliverFrameHandler(context.Background(), newFullQueue(), p,
				&wsDeliverScope{taskID: "t1", connID: newTestConnID()}, guard, &faultOwned)
			if handler(testUploadID) {
				t.Error("deliverFn must return false when receipt enqueue fails (queue full)")
			}
			if faultOwned.Load() != tc.wantFault {
				t.Errorf("faultOwned = %v, want %v", faultOwned.Load(), tc.wantFault)
			}
		})
	}
}

// TestWSDeliver_QueueFullEnqueueDisconnectsSlowClient（端到端）：共享写队列已满且
// writer 阻塞（newWSOutQueue 注入预填队列 + 2MB 大帧楔住 writer；客户端不读），
// 仅发生 deliver、无任何 PTY 输出 → deliver 回执入队失败 → 停止输入泵 → 连接被
// 有界收尾断开（不依赖 PTY 输出方向的队列满断开）。writer 让出最后空位的时序不定：
// 断开由首个或第二个 deliver 的回执入队失败触发，两者皆为被测契约。
func TestWSDeliver_QueueFullEnqueueDisconnectsSlowClient(t *testing.T) {
	p := openCatPty(t)
	defer p.Close()
	fake := &fakeDeliverOrch{result: func(func(context.Context) error) (bool, string) {
		return false, "not_found" // 准入拒绝：零 PTY 写入，回执是唯一出站帧
	}}
	s := newDeliverTestServer(fake, 400*time.Millisecond)

	old := newWSOutQueue
	newWSOutQueue = func() chan wsOutFrame {
		q := make(chan wsOutFrame, wsWriteQueueCap)
		q <- wsOutFrame{typ: websocket.MessageBinary, data: make([]byte, 2<<20)}
		q <- wsOutFrame{typ: websocket.MessageBinary, data: make([]byte, 2<<20)}
		for len(q) < wsWriteQueueCap {
			q <- wsOutFrame{typ: websocket.MessageBinary, data: []byte("pad")}
		}
		return q
	}
	t.Cleanup(func() { newWSOutQueue = old })

	url, done := startDeliverBridge(t, s, p, "t1")
	defer awaitHandlerDone(t, done, 3*time.Second)

	c, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()
	// 客户端不读（慢客户端）。

	for i := 0; i < 2; i++ {
		if err := c.Write(context.Background(), websocket.MessageText, mustDeliverFrame(t, testUploadID)); err != nil {
			t.Fatalf("write deliver #%d: %v", i+1, err)
		}
	}

	// 连接被收尾断开（回执入队失败 → 停止输入泵 → 有界收尾），全程无 PTY 输出。
	start := time.Now()
	readErr := make(chan error, 1)
	go func() {
		for {
			if _, _, rerr := c.Read(context.Background()); rerr != nil {
				readErr <- rerr
				return
			}
		}
	}()
	select {
	case <-readErr:
	case <-time.After(2 * time.Second):
		t.Fatal("connection not closed after receipt enqueue failure (slow client)")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("teardown took %v, want within finish budget + slack", elapsed)
	}
	// 至少一次编排调用（writer 让位时序不定：首个回执即失败的场合恰 1 次，
	// 首个回执占用空位、第二次失败的场合为 2 次）；断开后不再处理后续帧。
	if n := len(fake.deliverCalls()); n < 1 || n > 2 {
		t.Errorf("deliver calls = %d, want 1 or 2", n)
	}
}
