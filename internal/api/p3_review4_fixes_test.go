package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"ocdeck/internal/pty"
	"ocdeck/internal/task"
)

// fakeTaskBackend 内存实现 TaskBackend，用于 API 层测试。
type fakeTaskBackend struct {
	reopenErr      error
	validateErr    error
	validateCalls  []string
	listShellsRes  []task.TerminalID
	attachPtyFn    func(sessionName string, cols, rows int) (*pty.Pty, error)
	attachPtyCalls []string
}

func (f *fakeTaskBackend) Create(ctx context.Context, projectID, taskName string) (task.TaskRow, error) {
	return task.TaskRow{}, nil
}
func (f *fakeTaskBackend) Activate(ctx context.Context, taskID string) error { return nil }
func (f *fakeTaskBackend) Suspend(ctx context.Context, taskID string) error  { return nil }
func (f *fakeTaskBackend) Archive(ctx context.Context, taskID string) error  { return nil }
func (f *fakeTaskBackend) Restore(ctx context.Context, taskID string) error  { return nil }
func (f *fakeTaskBackend) Delete(ctx context.Context, taskID string, mode task.DeleteMode, confirmDirty bool) error {
	return nil
}
func (f *fakeTaskBackend) Retry(ctx context.Context, taskID string, confirmDirty bool) error {
	return nil
}
func (f *fakeTaskBackend) ReopenAttach(ctx context.Context, taskID string) (task.TerminalID, error) {
	return "", f.reopenErr
}
func (f *fakeTaskBackend) CreateShell(ctx context.Context, taskID string) (task.TerminalID, error) {
	return "", nil
}
func (f *fakeTaskBackend) CloseShell(ctx context.Context, terminalID task.TerminalID) error {
	return nil
}
func (f *fakeTaskBackend) Get(ctx context.Context, taskID string) (task.TaskRow, error) {
	return task.TaskRow{}, nil
}
func (f *fakeTaskBackend) List(ctx context.Context, projectID string) ([]task.TaskRow, error) {
	return nil, nil
}
func (f *fakeTaskBackend) ListTaskSessions(ctx context.Context, taskID string) ([]task.SessionRow, error) {
	return nil, nil
}
func (f *fakeTaskBackend) ListShells(taskID string) ([]task.TerminalID, error) {
	return f.listShellsRes, nil
}
func (f *fakeTaskBackend) ValidateShellTerminal(tid string) error {
	f.validateCalls = append(f.validateCalls, tid)
	return f.validateErr
}
func (f *fakeTaskBackend) AttachPty(sessionName string, cols, rows int) (*pty.Pty, error) {
	f.attachPtyCalls = append(f.attachPtyCalls, sessionName)
	if f.attachPtyFn != nil {
		return f.attachPtyFn(sessionName, cols, rows)
	}
	return nil, errors.New("attach not implemented")
}

// AgentStatus 返回空串（2.8 默认降级，具体行为在 task 包测试覆盖）。
func (f *fakeTaskBackend) AgentStatus(ctx context.Context, taskID string) string { return "" }

// ListAllActiveTaskIDs 返回空切片（全局配置受影响任务提示的默认空响应）。
func (f *fakeTaskBackend) ListAllActiveTaskIDs(ctx context.Context) ([]string, error) {
	return []string{}, nil
}

// ListActiveTaskOverview 默认返回空切片（cross-project-active-sessions API 测试默认空响应）；
// 具体 store 聚合行为由 internal/store 测试覆盖，hydration 行为由 active_sessions_api_test 覆盖。
func (f *fakeTaskBackend) ListActiveTaskOverview(ctx context.Context) ([]task.ActiveTaskOverviewRow, error) {
	return []task.ActiveTaskOverviewRow{}, nil
}

// Git 默认实现（noop / 零值）：git API 测试在 gitTaskBackend / mockGitBackend 中覆盖。
func (f *fakeTaskBackend) GitStatus(ctx context.Context, taskID string) (task.GitStatusDTO, error) {
	return task.GitStatusDTO{}, nil
}
func (f *fakeTaskBackend) GitDiff(ctx context.Context, taskID, ref, path string) (task.GitDiffDTO, error) {
	return task.GitDiffDTO{}, nil
}
func (f *fakeTaskBackend) GitCommit(ctx context.Context, taskID, message string, paths []string) error {
	return nil
}
func (f *fakeTaskBackend) GitPush(ctx context.Context, taskID string) error { return nil }

// Lifecycle config / init rerun / logs 默认实现（noop / 空值）：
// 实际行为由 lifecycle_config_api_test.go 与具体场景注入 mock 覆盖。
func (f *fakeTaskBackend) RerunInit(ctx context.Context, taskID string) (task.TaskRow, error) {
	return task.TaskRow{}, nil
}
func (f *fakeTaskBackend) ReadInitLog(ctx context.Context, taskID string) (string, error) {
	return "", nil
}
func (f *fakeTaskBackend) ReadPreDeleteLog(ctx context.Context, taskID string) (string, error) {
	return "", nil
}

// newAPITestServer 构造带 TaskBackend 的 Server。
func newAPITestServer(t *testing.T, tb TaskBackend) *Server {
	t.Helper()
	cfg := testConfig()
	s := &Server{
		cfg:       cfg,
		mux:       http.NewServeMux(),
		auth:      NewTokenAuthenticator(cfg.Token),
		wsClients: newWSClientRegistry(),
		tasks:     tb,
	}
	s.registerRoutes()
	return s
}

// --- Fix 3: task busy ReopenAttach 返回 409（非 500） ---

func TestAPI_ReopenAttach_BusyReturns409(t *testing.T) {
	tb := &fakeTaskBackend{
		// task.OpError codeConflict → api 映射 409。
		reopenErr: &task.OpError{Code: "conflict", Err: errors.New("task busy")},
	}
	s := newAPITestServer(t, tb)
	srv := httptest.NewServer(s.mux)
	defer srv.Close()

	req := httptest.NewRequest("POST", "/api/v1/tasks/t1/attach/reopen", nil)
	req.SetPathValue("id", "t1")
	// api 子 mux 经 auth 中间件；这里直接调 handler 验证映射。
	rec := httptest.NewRecorder()
	s.handleReopenAttach(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("busy ReopenAttach status=%d want 409 (not 500)", rec.Code)
	}
	var body errorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != CodeConflict {
		t.Errorf("code=%v want conflict", body.Error.Code)
	}
}

// --- Fix 5: shell WS 身份校验拒绝（4004） ---

func TestAPI_WSShell_RejectsInvalidTerminal_4004(t *testing.T) {
	cases := []struct {
		name        string
		tid         string
		validateErr error
	}{
		{"garbage tid", "not-a-session", &task.OpError{Code: "invalid_input", Err: errors.New("invalid")}},
		{"serve session tid", "ocdeck-t1-serve", &task.OpError{Code: "invalid_input", Err: errors.New("not shell")}},
		{"not found", "ocdeck-t1-shell-99", &task.OpError{Code: "not_found", Err: errors.New("not alive")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tb := &fakeTaskBackend{validateErr: c.validateErr}
			s := newAPITestServer(t, tb)
			srv := httptest.NewServer(s.mux)
			defer srv.Close()

			url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/terminal/shell/" + c.tid
			conn, _, err := websocket.Dial(context.Background(), url, nil)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.CloseNow()
			// 发送 auth 帧。
			authBody, _ := json.Marshal(wsAuthReq{Type: "auth", Token: "testtoken", Cols: 80, Rows: 24})
			if err := conn.Write(context.Background(), websocket.MessageText, authBody); err != nil {
				t.Fatalf("write auth: %v", err)
			}
			// 校验失败 → 4004 关闭。
			_, _, err = conn.Read(context.Background())
			if websocket.CloseStatus(err) != websocket.StatusCode(wsCloseTerminalNotFound) {
				t.Errorf("ValidateShellTerminal(%q) close=%v want %d (terminal not found)", c.tid, websocket.CloseStatus(err), wsCloseTerminalNotFound)
			}
			if len(tb.validateCalls) != 1 || tb.validateCalls[0] != c.tid {
				t.Errorf("ValidateShellTerminal must be called with %q, got %v", c.tid, tb.validateCalls)
			}
		})
	}
}

// --- 第三轮：shell WS infra 故障误发 4004 修复 ---

// TestAPI_WSShell_ValidateInfraError_1011 验证第三轮 B5：
// ValidateShellTerminal 返回 process_error（tmux 基础设施故障）MUST 以 1011（internal error）关闭，
// 不得误发 4004 致前端永久停止重连（terminal-streaming spec 契约：1011 走默认重连路径，临时可恢复）。
func TestAPI_WSShell_ValidateInfraError_1011(t *testing.T) {
	tb := &fakeTaskBackend{
		validateErr: &task.OpError{Code: "process_error", Err: errors.New("tmux protocol error")},
	}
	s := newAPITestServer(t, tb)
	srv := httptest.NewServer(s.mux)
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/terminal/shell/ocdeck-t1-shell-1"
	conn, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()
	authBody, _ := json.Marshal(wsAuthReq{Type: "auth", Token: "testtoken", Cols: 80, Rows: 24})
	if err := conn.Write(context.Background(), websocket.MessageText, authBody); err != nil {
		t.Fatalf("write auth: %v", err)
	}
	// infra 错误 → 1011 internal error（非 4004）。
	_, _, rerr := conn.Read(context.Background())
	if got := websocket.CloseStatus(rerr); got != websocket.StatusCode(wsCloseInternalError) {
		t.Errorf("process_error close=%v want %d (1011 internal error, not 4004)", got, wsCloseInternalError)
	}
}

// TestAPI_WSShell_ValidateNotFound_4004 验证第三轮：ValidateShellTerminal 返回 not_found → 4004。
func TestAPI_WSShell_ValidateNotFound_4004(t *testing.T) {
	tb := &fakeTaskBackend{
		validateErr: &task.OpError{Code: "not_found", Err: errors.New("shell not alive")},
	}
	s := newAPITestServer(t, tb)
	srv := httptest.NewServer(s.mux)
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/terminal/shell/ocdeck-t1-shell-99"
	conn, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()
	authBody, _ := json.Marshal(wsAuthReq{Type: "auth", Token: "testtoken", Cols: 80, Rows: 24})
	if err := conn.Write(context.Background(), websocket.MessageText, authBody); err != nil {
		t.Fatalf("write auth: %v", err)
	}
	_, _, rerr := conn.Read(context.Background())
	if got := websocket.CloseStatus(rerr); got != websocket.StatusCode(wsCloseTerminalNotFound) {
		t.Errorf("not_found close=%v want %d (4004 terminal not found)", got, wsCloseTerminalNotFound)
	}
}

// --- 测试升级：WS 4009 断言旧连接实际收到 4009 关闭码 ---
// 用直接 register + wsClose 模式（不启动 bridge，避免 bridge 关闭码竞争），
// 断言旧连接实际收到 4009 关闭码（而非 -1/normal）。

func TestWS_Replace4009_OldConnReceives4009(t *testing.T) {
	reg := newWSClientRegistry()
	key := terminalKey("t1", false)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := acceptWS(w, r)
		if err != nil {
			return
		}
		// 不启动 bridge，仅注册 + 在新连接注册时对旧连接发 4009 再 cancel。
		oldConn, oldCancel, bridgeCtx := reg.register(key, c)
		if oldConn != nil {
			wsCloseReplacedWait(oldConn)
			oldCancel()
		}
		defer reg.unregister(key, c)
		// 保持新连接存活（阻塞读，等待测试结束）。
		<-bridgeCtx.Done()
		c.CloseNow()
	}))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	c1, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial c1: %v", err)
	}
	defer c1.CloseNow()
	c1.SetReadLimit(wsMaxFrame)

	// 第二个连接替换同一终端。
	c2, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial c2: %v", err)
	}
	defer c2.CloseNow()

	// c1 应实际收到 4009 关闭码。
	done := make(chan error, 1)
	go func() {
		for {
			_, _, rerr := c1.Read(context.Background())
			if rerr != nil {
				done <- rerr
				return
			}
		}
	}()
	select {
	case rerr := <-done:
		if got := websocket.CloseStatus(rerr); got != websocket.StatusCode(wsCloseReplaced) {
			t.Errorf("old conn close=%v want %d (4009 replaced)", got, wsCloseReplaced)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old conn did not receive 4009 close")
	}
}

// --- 测试升级：WS 取消 PTY 读阻塞时 ctx 取消能退出 ---

func TestWSBridge_CtxCancel_PtyReadBlockingExits(t *testing.T) {
	// 用一个会阻塞 Read 的 PTY：起 sleep 命令（不产生输出，Read 阻塞）。
	// pty.Open 启动 /bin/sleep 100，Read 会阻塞直到 ctx 取消或 PTY 关闭。
	// 改用 /bin/cat 但不写入数据，Read 仍会阻塞（等待输入）——但 cat 在无输入时 Read 可能返回 EOF（非阻塞）。
	// 用 /bin/sleep 确保无输出，Read 阻塞。
	tb := &fakeTaskBackend{
		attachPtyFn: func(sessionName string, cols, rows int) (*pty.Pty, error) {
			return openSleepPty(t), nil
		},
	}
	_ = tb // 用 newBridgeHandler 更直接测试 bridge ctx 取消。

	// 直接复用 ws_bridge_test 的模式：bridge ctx 取消 → bridge 退出。
	p := openSleepPty(t)
	defer p.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := acceptWS(w, r)
		if err != nil {
			return
		}
		defer c.CloseNow()
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

	// bridge ctx 取消后 bridge MUST 退出（PTY Read 阻塞也被取消），WS 关闭。
	done := make(chan struct{})
	go func() {
		_, _, _ = c.Read(context.Background())
		close(done)
	}()
	select {
	case <-done:
		// bridge 退出，无挂起。
	case <-time.After(5 * time.Second):
		t.Fatal("bridge did not exit after ctx cancel while PTY read blocking (possible hang)")
	}
}

// openSleepPty 起一个 /bin/sleep 的 PTY（无输出，Read 阻塞），返回 *pty.Pty。
func openSleepPty(t *testing.T) *pty.Pty {
	t.Helper()
	// 使用 exec.Command 构造；与 openCatPty 同模式但用 sleep。
	cmd := exec.Command("/bin/sleep", "100")
	p, err := pty.Open(cmd, "", nil, 80, 24)
	if err != nil {
		t.Fatalf("pty.Open sleep: %v", err)
	}
	return p
}
