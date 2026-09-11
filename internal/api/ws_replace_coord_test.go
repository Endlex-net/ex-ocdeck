package api

// R1 屏障测试：TUI 注册表替换提交接入 per-task 协调锁（生产 handleWSTUI 注册路径 +
// 真实 *appuploads.Orchestrator 联动）。契约（delivery spec「连接归属与投递准入」/
// design D5 原子边界）：投递准入与 PTY 写入、finalize 复查提交、连接替换提交 MUST
// 在同一把 per-task 协调锁临界区内完成（线性化点 = 锁内校验+写入完成的时刻），
// 等待 4009 关闭握手不持锁。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	appuploads "ocdeck/internal/application/uploads"
	"ocdeck/internal/infrastructure/pty"
)

// coordTestStore StorePort 测试桩：记录 CommitUpload；StatUpload 恒存在。
type coordTestStore struct {
	mu        sync.Mutex
	committed []appuploads.Meta
}

func (s *coordTestStore) NewUploadID() (string, error)           { return strings.Repeat("ab", 16), nil }
func (s *coordTestStore) ManagedName(uploadID, _ string) string  { return uploadID }
func (s *coordTestStore) RemoveUpload(_ string, _ string) error  { return nil }
func (s *coordTestStore) RemovePartial(_ string, _ string) error { return nil }
func (s *coordTestStore) RemovePath(_ string, _ string) error    { return nil }
func (s *coordTestStore) RemoveTaskDir(_ string) error           { return nil }
func (s *coordTestStore) Scan() ([]appuploads.Entry, error)      { return nil, nil }
func (s *coordTestStore) StatUpload(_ string, _ string) (bool, error) {
	return true, nil
}

func (s *coordTestStore) WritePartial(_ context.Context, _ string, _ string, r io.Reader, _ func(int64)) (int64, error) {
	return io.Copy(io.Discard, r)
}

func (s *coordTestStore) CommitUpload(_ string, meta appuploads.Meta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.committed = append(s.committed, meta)
	return nil
}

func (s *coordTestStore) RefreshLastDelivered(_ string, _ appuploads.Meta, _ time.Time) error {
	return nil
}

func (s *coordTestStore) committedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.committed)
}

// coordTestTasks TaskPort 测试桩：任务存在且活跃。
type coordTestTasks struct{}

func (coordTestTasks) TaskActive(context.Context, string) (bool, bool, error) { return true, true, nil }

// registryConns ConnPort：直读生产注册表（与组合根 currentConnPort 同一数据源：
// Server.CurrentTUIConnID → wsClientRegistry）。
type registryConns struct{ s *Server }

func (c registryConns) CurrentTUIConn(_ context.Context, taskID string) (string, bool, error) {
	id, ok := c.s.wsClients.currentTUIConnID(taskID)
	return id, ok, nil
}

// coordDeliverAdapter 复刻组合根 deliverOrchAdapter：(bool, DeliverErrorCode) →
// api 层窄接口 (bool, string)。
type coordDeliverAdapter struct{ inner *appuploads.Orchestrator }

func (a coordDeliverAdapter) Deliver(ctx context.Context, taskID, connID, uploadID string, inject func(context.Context) error) (bool, string) {
	ok, code := a.inner.Deliver(ctx, taskID, connID, uploadID, inject)
	return ok, string(code)
}

// replaceCoordHarness 生产 handleWSTUI + 真实编排器 + 共享 per-task 协调锁的联动拓扑。
type replaceCoordHarness struct {
	srv    *httptest.Server
	s      *Server
	orch   *appuploads.Orchestrator
	store  *coordTestStore
	taskID string
	url    string
	done   <-chan struct{}
	// attachCalls PTY attach 信号（通道计数，无共享内存竞争）：第 n 个值代表第 n 条
	// 连接的 handler 已通过 AttachPty（registerTUIConn 之前一步）。
	attachCalls chan string
}

func startReplaceCoordHarness(t *testing.T) *replaceCoordHarness {
	t.Helper()
	const taskID = "t1"

	store := &coordTestStore{}
	h := &replaceCoordHarness{
		store:       store,
		taskID:      taskID,
		attachCalls: make(chan string, 8),
	}
	// 每连接一个独立 PTY（与生产 AttachPty 语义一致）：替换后旧 handler 的
	// defer p.Close() 不得殃及新连接的 bridge。
	var ptyMu sync.Mutex
	var attached []*pty.Pty
	h.s = &Server{
		cfg:  testConfig(),
		auth: NewTokenAuthenticator(testConfig().Token),
		tasks: &fakeTaskBackend{attachPtyFn: func(string, int, int) (*pty.Pty, error) {
			np, err := pty.Open(exec.Command("/bin/cat"), "", nil, 80, 24)
			if err != nil {
				return nil, err
			}
			ptyMu.Lock()
			attached = append(attached, np)
			ptyMu.Unlock()
			h.attachCalls <- taskID
			return np, nil
		}},
		wsClients: newWSClientRegistry(),
	}
	t.Cleanup(func() {
		ptyMu.Lock()
		defer ptyMu.Unlock()
		for _, np := range attached {
			_ = np.Close()
		}
	})
	// 真实编排器 + 生产注册表联动：Conns 端口直读注册表（与组合根 currentConnPort
	// 同源），替换提交共用编排器的 per-task 协调锁。
	orch := appuploads.New(appuploads.Options{
		Cfg:   appuploads.Config{UploadDir: t.TempDir(), MaxBytes: 1 << 20},
		Store: store,
		Tasks: coordTestTasks{},
		Conns: registryConns{s: h.s},
	})
	h.orch = orch
	h.s.deliverOrch = coordDeliverAdapter{inner: orch}
	h.s.replaceCoord = orch.Coordination()
	h.s.deliverWriteDeadline = 200 * time.Millisecond
	h.s.wsFinishBudget = time.Second
	done := make(chan struct{})
	var once sync.Once
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 直挂 handler（无路由 pattern）：回填路径参数（handleWSTUI 读 PathValue）。
		r.SetPathValue("taskID", taskID)
		defer once.Do(func() { close(done) })
		h.s.handleWSTUI(w, r)
	}))
	t.Cleanup(h.srv.Close)
	h.url = "ws" + strings.TrimPrefix(h.srv.URL, "http") + "/ws/terminal/" + taskID
	h.done = done
	return h
}

// dialAuthed 走完整生产入口：dial → auth 帧 → auth_ok（读取服务端签发的 connId）。
// 失败用 t.Errorf + (nil, "") 返回（可在 goroutine 中调用）；主线程调用方须检查 nil。
func (h *replaceCoordHarness) dialAuthed(t *testing.T) (*websocket.Conn, string) {
	t.Helper()
	c, _, err := websocket.Dial(context.Background(), h.url, nil)
	if err != nil {
		t.Errorf("dial: %v", err)
		return nil, ""
	}
	if err := c.Write(context.Background(), websocket.MessageText,
		mustJSON(wsAuthReq{Type: "auth", Token: testConfig().Token, Cols: 80, Rows: 24})); err != nil {
		t.Errorf("write auth: %v", err)
		_ = c.CloseNow()
		return nil, ""
	}
	var connID string
	w := &wsSession{conn: c}
	w.readFramesUntil(5*time.Second, func(typ websocket.MessageType, payload []byte) bool {
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
		t.Error("no auth_ok connId received")
		_ = c.CloseNow()
		return nil, ""
	}
	return c, connID
}

// waitAttach 等待下一条连接的 handler 到达 AttachPty（registerTUIConn 前一步），
// 每条连接恰好消费一个信号。
func (h *replaceCoordHarness) waitAttach(t *testing.T) {
	t.Helper()
	select {
	case <-h.attachCalls:
	case <-time.After(3 * time.Second):
		t.Fatal("handler never reached AttachPty")
	}
}

// waitCurrentConn 有界等待注册表当前 TUI connId 变为 want。
func (h *replaceCoordHarness) waitCurrentConn(t *testing.T, want string) {
	t.Helper()
	waitFor(t, 3*time.Second, fmt.Sprintf("current TUI conn == %q", want), func() bool {
		id, _ := h.s.wsClients.currentTUIConnID(h.taskID)
		return id == want
	})
}

// seedUpload 走真实提交链路播种可投递上传（绑定 connID，finalize 发布 uploadId）。
func (h *replaceCoordHarness) seedUpload(t *testing.T, connID string) *appuploads.ActiveUpload {
	t.Helper()
	u, err := h.orch.RegisterActiveUpload(h.taskID, connID)
	if err != nil {
		t.Fatalf("register active upload: %v", err)
	}
	h.orch.BindOriginalName(u, "a.png")
	if _, err := h.orch.WriteUploadBody(context.Background(), u, strings.NewReader("data")); err != nil {
		t.Fatalf("write upload body: %v", err)
	}
	if err := h.orch.FinalizeUpload(context.Background(), u); err != nil {
		t.Fatalf("finalize upload: %v", err)
	}
	return u
}

// TestReplaceCoord_DeliverFirstReplacementWaits（投递先行）：deliver 在协调锁临界区内
// 写入时，替换连接的注册表提交 MUST 等待同一把锁——注入完成前旧连接仍为当前连接
//（无「校验后插入」窗口）；注入完成后替换提交才生效，注入恰好一次且完整成功。
func TestReplaceCoord_DeliverFirstReplacementWaits(t *testing.T) {
	h := startReplaceCoordHarness(t)
	defer awaitHandlerDone(t, h.done, 5*time.Second)

	c1, connA := h.dialAuthed(t)
	if c1 == nil {
		t.Fatal("c1 dial failed")
	}
	defer c1.CloseNow()
	h.waitAttach(t)
	u := h.seedUpload(t, connA)

	// 注入写入阻塞在协调锁临界区内（持有锁）。
	entered := make(chan struct{})
	release := make(chan struct{})
	var injectRuns atomic.Int32
	stubDeliverPTYWrite(t, func(_ *pty.Pty, _ context.Context, data []byte, _ time.Duration) (int, error) {
		injectRuns.Add(1)
		entered <- struct{}{}
		<-release
		return len(data), nil
	})
	if err := c1.Write(context.Background(), websocket.MessageText, mustDeliverFrame(t, u.ID)); err != nil {
		t.Fatalf("c1 write deliver: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("inject never entered coordination critical section")
	}

	// 替换连接：handler 到达 AttachPty（registerTUIConn 前一步）后必然阻塞在协调锁上。
	type dialResult struct {
		c      *websocket.Conn
		connID string
	}
	c2ch := make(chan dialResult, 1)
	go func() {
		c2, connB := h.dialAuthed(t)
		c2ch <- dialResult{c: c2, connID: connB}
	}()
	h.waitAttach(t) // 屏障：c2 handler 已到 registerTUIConn 之前一步

	// 注入仍阻塞（未 release）：注册表当前连接 MUST 仍为 connA（替换提交被锁挡住）。
	if id, _ := h.s.wsClients.currentTUIConnID(h.taskID); id != connA {
		t.Fatalf("current conn = %q while deliver holds coordination lock, want %q (replacement must wait)", id, connA)
	}

	// 注入完成（deliver 临界区结束释放锁）→ 替换提交生效。
	close(release)
	res := <-c2ch
	if res.c == nil {
		t.Fatal("replacement connection failed to complete registration")
	}
	defer res.c.CloseNow()
	h.waitCurrentConn(t, res.connID)

	// 注入恰好一次且完整成功（未被打断、替换后无补写）。
	if n := injectRuns.Load(); n != 1 {
		t.Errorf("inject ran %d times, want exactly 1 (completed before replacement commit)", n)
	}
}

// TestReplaceCoord_ReplacementFirstStaleDeliverRejected（替换先行）：替换提交
//（经同一把协调锁）先于旧代次 deliver → 编排按唯一优先级拒绝（forbidden，零注入），
// 旧 uploadId 的投递不得再写入 PTY。
func TestReplaceCoord_ReplacementFirstStaleDeliverRejected(t *testing.T) {
	h := startReplaceCoordHarness(t)
	defer awaitHandlerDone(t, h.done, 5*time.Second)

	c1, connA := h.dialAuthed(t)
	if c1 == nil {
		t.Fatal("c1 dial failed")
	}
	defer c1.CloseNow()
	h.waitAttach(t)
	u := h.seedUpload(t, connA)

	// 替换先行：c2 经协调锁提交注册，c1 以 4009 关闭。
	c2, connB := h.dialAuthed(t)
	if c2 == nil {
		t.Fatal("c2 dial failed")
	}
	defer c2.CloseNow()
	h.waitAttach(t)
	h.waitCurrentConn(t, connB)

	// 屏障：c1 已被替换关闭（4009）。
	c1Err := make(chan error, 1)
	go func() {
		for {
			if _, _, rerr := c1.Read(context.Background()); rerr != nil {
				c1Err <- rerr
				return
			}
		}
	}()
	select {
	case rerr := <-c1Err:
		if got := websocket.CloseStatus(rerr); got != websocket.StatusCode(wsCloseReplaced) {
			t.Fatalf("c1 close = %v, want %d (4009, replaced)", got, wsCloseReplaced)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("c1 not closed after replacement")
	}

	// 旧代次 deliver（uploadId 绑定 connA，经 c2/connB 发送）：零注入。
	var injectRuns atomic.Int32
	stubDeliverPTYWrite(t, func(_ *pty.Pty, _ context.Context, _ []byte, _ time.Duration) (int, error) {
		injectRuns.Add(1)
		return 0, errors.New("stale generation deliver must not write PTY")
	})
	w := &wsSession{conn: c2}
	if err := c2.Write(context.Background(), websocket.MessageText, mustDeliverFrame(t, u.ID)); err != nil {
		t.Fatalf("c2 write stale deliver: %v", err)
	}
	wantReceipt := fmt.Sprintf(`{"type":"deliver_result","uploadId":%q,"ok":false,"error":"forbidden"}`, u.ID)
	w.readFramesUntil(3*time.Second, func(typ websocket.MessageType, payload []byte) bool {
		return typ == websocket.MessageText && strings.Contains(string(payload), "deliver_result")
	})
	if !w.hasTextFrame(wantReceipt) {
		t.Fatalf("stale deliver receipts = %v, want %q", w.textFrames(), wantReceipt)
	}
	if n := injectRuns.Load(); n != 0 {
		t.Errorf("inject ran %d times for stale generation deliver, want 0 (zero PTY write)", n)
	}
}

// TestReplaceCoord_FinalizeAfterReplacementRejected（finalize 与替换竞争：替换先提交）：
// 替换提交（经同一把协调锁）先于 finalize → 锁内复查当前 connId 已换代 →
// ErrConnNotCurrent，MUST NOT 提交/发布 uploadId（该 uploadId 不可投递）。
func TestReplaceCoord_FinalizeAfterReplacementRejected(t *testing.T) {
	h := startReplaceCoordHarness(t)
	defer awaitHandlerDone(t, h.done, 5*time.Second)

	c1, connA := h.dialAuthed(t)
	if c1 == nil {
		t.Fatal("c1 dial failed")
	}
	defer c1.CloseNow()
	h.waitAttach(t)

	// 在途上传：登记 + 绑定 connA + body 已写，未 finalize。
	u, err := h.orch.RegisterActiveUpload(h.taskID, connA)
	if err != nil {
		t.Fatalf("register active upload: %v", err)
	}
	h.orch.BindOriginalName(u, "a.png")
	if _, err := h.orch.WriteUploadBody(context.Background(), u, strings.NewReader("data")); err != nil {
		t.Fatalf("write upload body: %v", err)
	}

	// 替换提交先行（经同一把协调锁）。
	c2, connB := h.dialAuthed(t)
	if c2 == nil {
		t.Fatal("c2 dial failed")
	}
	defer c2.CloseNow()
	h.waitAttach(t)
	h.waitCurrentConn(t, connB)

	// finalize 复查（协调锁临界区内）：连接已换代 → 拒绝提交。
	if err := h.orch.FinalizeUpload(context.Background(), u); !errors.Is(err, appuploads.ErrConnNotCurrent) {
		t.Fatalf("finalize after replacement err = %v, want ErrConnNotCurrent", err)
	}
	if n := h.store.committedCount(); n != 0 {
		t.Errorf("finalize committed %d uploads after connection replacement, want 0 (no rename/commit)", n)
	}

	// 未发布：该 uploadId 不可投递（deliver → not_found，零注入）。
	var injectRuns atomic.Int32
	stubDeliverPTYWrite(t, func(_ *pty.Pty, _ context.Context, _ []byte, _ time.Duration) (int, error) {
		injectRuns.Add(1)
		return 0, errors.New("unpublished upload must not write PTY")
	})
	w := &wsSession{conn: c2}
	if err := c2.Write(context.Background(), websocket.MessageText, mustDeliverFrame(t, u.ID)); err != nil {
		t.Fatalf("c2 write deliver of unpublished upload: %v", err)
	}
	wantReceipt := fmt.Sprintf(`{"type":"deliver_result","uploadId":%q,"ok":false,"error":"not_found"}`, u.ID)
	w.readFramesUntil(3*time.Second, func(typ websocket.MessageType, payload []byte) bool {
		return typ == websocket.MessageText && strings.Contains(string(payload), "deliver_result")
	})
	if !w.hasTextFrame(wantReceipt) {
		t.Fatalf("unpublished upload receipts = %v, want %q", w.textFrames(), wantReceipt)
	}
	if n := injectRuns.Load(); n != 0 {
		t.Errorf("inject ran %d times for unpublished upload, want 0", n)
	}
}
