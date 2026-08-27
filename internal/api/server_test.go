package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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

// TestStatusRecorder_Buffers404UntilFlush 验证 404/405 缓冲 header/body，未 flush
// 前不转发到底层（由 jsonNotFoundHandler 决定转发或重写）。
func TestStatusRecorder_Buffers404UntilFlush(t *testing.T) {
	underlying := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: underlying, status: http.StatusOK}

	rec.Header().Set("Content-Type", "text/plain")
	rec.WriteHeader(http.StatusNotFound)
	if _, err := rec.Write([]byte("default mux text")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if rec.status != http.StatusNotFound {
		t.Errorf("recorder status = %d, want 404", rec.status)
	}
	if underlying.Body.Len() != 0 {
		t.Errorf("underlying body = %q, want buffered until flush", underlying.Body.String())
	}
	if rec.jsonEnvelope() {
		t.Fatal("plain-text 404 must not be treated as JSON envelope")
	}
}

func TestJSONNotFoundHandler_ForwardsJSONEnvelope(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, CodeNotFound, "task not found")
	})
	ts := httptest.NewServer(jsonNotFoundHandler(inner))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/tasks/missing")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	var eb errorBody
	if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
		t.Fatal(err)
	}
	if eb.Error.Code != CodeNotFound || eb.Error.Message != "task not found" {
		t.Errorf("envelope = %+v, want not_found/task not found", eb.Error)
	}
}

func TestJSONNotFoundHandler_Rewrites405KeepsAllow(t *testing.T) {
	inner := http.NewServeMux()
	inner.HandleFunc("GET /api/v1/server/status", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ts := httptest.NewServer(jsonNotFoundHandler(inner))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/server/status", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); !strings.Contains(allow, http.MethodGet) {
		t.Errorf("Allow = %q, want to keep GET from mux", allow)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json rewrite", ct)
	}
	var eb errorBody
	if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
		t.Fatal(err)
	}
	if eb.Error.Code != CodeNotFound {
		t.Errorf("code = %s, want not_found", eb.Error.Code)
	}
}

func TestJSONNotFoundHandler_HeaderOnly200Passthrough(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom", "keep-me")
		w.Header().Set("Content-Type", "text/plain")
	})
	ts := httptest.NewServer(jsonNotFoundHandler(inner))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/header-only")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Custom"); got != "keep-me" {
		t.Errorf("X-Custom = %q, want keep-me (header-only 200)", got)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain" {
		t.Errorf("content-type = %q, want text/plain passthrough", ct)
	}
}

func TestJSONNotFoundHandler_RewritesPlainText404(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "404 page not found", http.StatusNotFound)
	})
	ts := httptest.NewServer(jsonNotFoundHandler(inner))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"code":"not_found"`) {
		t.Errorf("body = %q, want rewritten JSON envelope", body)
	}
	if strings.Contains(string(body), "page not found") {
		t.Errorf("plain mux text leaked: %s", body)
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

func TestStatusRecorder_FlushErrorImplicit200CopiesBufferedHeaders(t *testing.T) {
	underlying := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: underlying, status: http.StatusOK}
	rec.Header().Set("X-Custom", "keep-me")
	rec.Header().Set("Content-Type", "text/plain")

	if err := rec.FlushError(); err != nil {
		t.Fatalf("FlushError: %v", err)
	}
	if !rec.wrote || rec.status != http.StatusOK {
		t.Errorf("recorder status=%d wrote=%v, want implicit 200", rec.status, rec.wrote)
	}
	if underlying.Code != http.StatusOK {
		t.Errorf("underlying code = %d, want 200", underlying.Code)
	}
	if got := underlying.Header().Get("X-Custom"); got != "keep-me" {
		t.Errorf("X-Custom = %q, want keep-me after implicit 200 flush", got)
	}
	if ct := underlying.Header().Get("Content-Type"); ct != "text/plain" {
		t.Errorf("content-type = %q, want text/plain passthrough", ct)
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
