package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// --- statusRecorder（sse-active-sessions P2.5） ---

// TestStatusRecorder_Passthrough200 验证非 404/405 状态码照常透传（handler 自定义
// 状态码与 body 不被吞）。
func TestStatusRecorder_Passthrough200(t *testing.T) {
	underlying := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: underlying, status: http.StatusOK}

	rec.WriteHeader(http.StatusOK)
	if _, err := rec.Write([]byte("ok")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if underlying.Code != http.StatusOK {
		t.Errorf("underlying code = %d, want 200", underlying.Code)
	}
	if underlying.Body.String() != "ok" {
		t.Errorf("underlying body = %q, want ok", underlying.Body.String())
	}
	if rec.status != http.StatusOK || !rec.wrote {
		t.Errorf("recorder status=%d wrote=%v, want 200/true", rec.status, rec.wrote)
	}
}

// TestStatusRecorder_Swallows404Body 验证 404/405 的 header/body 不转发（由
// jsonNotFoundHandler 统一重写）。
func TestStatusRecorder_Swallows404Body(t *testing.T) {
	underlying := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: underlying, status: http.StatusOK}

	rec.WriteHeader(http.StatusNotFound)
	if _, err := rec.Write([]byte("default mux text")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if rec.status != http.StatusNotFound {
		t.Errorf("recorder status = %d, want 404", rec.status)
	}
	if underlying.Body.Len() != 0 {
		t.Errorf("underlying body = %q, want swallowed", underlying.Body.String())
	}
}

// flushErrorWriter 注入 FlushError 的底层 ResponseWriter，返回 sentinel 错误。
type flushErrorWriter struct {
	header   http.Header
	flushed  bool
	flushErr error
}

func (f *flushErrorWriter) Header() http.Header { return f.header }

func (f *flushErrorWriter) Write(b []byte) (int, error) { return len(b), nil }

func (f *flushErrorWriter) WriteHeader(int) {}

func (f *flushErrorWriter) FlushError() error {
	f.flushed = true
	return f.flushErr
}

// TestStatusRecorder_FlushErrorPropagates 验证 FlushError 委托到底层连接且错误
// 可传播：直接调用与经 http.ResponseController（SSE handler 实际路径，经 Unwrap 链）
// 均返回底层 flush 错误。
func TestStatusRecorder_FlushErrorPropagates(t *testing.T) {
	sentinel := errors.New("flush failed: connection reset")
	fw := &flushErrorWriter{header: http.Header{}, flushErr: sentinel}
	rec := &statusRecorder{ResponseWriter: fw, status: http.StatusOK}

	// Unwrap 暴露底层 writer（ResponseController 依赖 Unwrap 链）。
	if rec.Unwrap() != fw {
		t.Fatal("Unwrap must expose underlying ResponseWriter")
	}

	// 直接调用 FlushError：错误传播。
	if err := rec.FlushError(); !errors.Is(err, sentinel) {
		t.Errorf("FlushError = %v, want %v", err, sentinel)
	}

	// SSE handler 实际路径：http.NewResponseController(w).Flush() 经中间件栈
	//（statusRecorder）到达底层并传播错误。
	fw.flushed = false
	rc := http.NewResponseController(rec)
	if err := rc.Flush(); !errors.Is(err, sentinel) {
		t.Errorf("ResponseController.Flush = %v, want %v (propagated)", err, sentinel)
	}
	if !fw.flushed {
		t.Error("underlying FlushError not reached via ResponseController")
	}
}

// TestStatusRecorder_FlushErrorDelegatesToPlainWriter 验证底层仅实现 Flush()
// （httptest.Recorder）时经 recorder/controller flush 无错误。
func TestStatusRecorder_FlushErrorDelegatesToPlainWriter(t *testing.T) {
	underlying := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: underlying, status: http.StatusOK}

	if err := rec.FlushError(); err != nil {
		t.Errorf("FlushError = %v, want nil (recorder implements Flush)", err)
	}
	rc := http.NewResponseController(rec)
	if err := rc.Flush(); err != nil {
		t.Errorf("ResponseController.Flush = %v, want nil", err)
	}
}

// --- Start BaseContext（sse-active-sessions P2.4） ---

// TestStartBaseContextCancelsRequestContext 验证 http.Server.BaseContext 设为服务
// 进程 ctx：进程取消先传导到 in-flight 请求的 r.Context()（handler 观测取消退出），
// Start 随后完成 5s 预算内 Shutdown 返回。
func TestStartBaseContextCancelsRequestContext(t *testing.T) {
	// 预占一个临时端口（bind :0 读端口后释放，Start 立即重绑）。
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	_, portStr, _ := net.SplitHostPort(probe.Addr().String())
	addr := probe.Addr().String()
	probe.Close()
	port, _ := strconv.Atoi(portStr)

	cfg := testConfig()
	cfg.ListenPort = port
	s := &Server{
		cfg:       cfg,
		mux:       http.NewServeMux(),
		auth:      NewTokenAuthenticator(cfg.Token),
		wsClients: newWSClientRegistry(),
	}
	handlerEntered := make(chan struct{})
	handlerDone := make(chan struct{})
	s.mux.HandleFunc("GET /api/v1/basectx-probe", func(w http.ResponseWriter, r *http.Request) {
		close(handlerEntered)
		<-r.Context().Done() // BaseContext 生效：进程 ctx 取消传导到请求 ctx
		close(handlerDone)
		w.WriteHeader(http.StatusOK)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startErr := make(chan error, 1)
	go func() { startErr <- s.Start(ctx) }()

	// 等待服务起来（TCP 拨号重试覆盖端口重绑窗口）。
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not come up at %s: %v", addr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 挂起请求异步发出（handler 等 r.Context().Done()，进程取消后才返回）。
	go func() {
		req, _ := http.NewRequest("GET", "http://"+addr+"/api/v1/basectx-probe", nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()

	<-handlerEntered
	// 进程取消 → handler 观测 r.Context() 取消退出。
	cancel()
	select {
	case <-handlerDone:
		// ok：BaseContext 派生的请求 ctx 因进程取消而取消。
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not observe request ctx cancellation within 3s")
	}
	// Shutdown 在 handler 退出后完成，Start 返回 nil。
	select {
	case err := <-startErr:
		if err != nil {
			t.Fatalf("Start returned %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return within 3s after cancel")
	}
}
