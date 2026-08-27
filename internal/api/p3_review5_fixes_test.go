package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"ocdeck/internal/application"
)

// --- Fix 8: HasSession infra error → 1011（不得误判 4010 掩盖 infra 故障） ---

// TestAPI_WSTUI_ReopenAttachInfraError_1011 验证 B8：ReopenAttach 返回 infra/internal
// 错误（如 HasSession tmux 基础设施错误）MUST 以 1011（internal error）关闭 WS，
// 不得误判为 4010（task suspended）掩盖 infra 故障（design.md §8/§21）。
func TestAPI_WSTUI_ReopenAttachInfraError_1011(t *testing.T) {
	tb := &fakeTaskBackend{
		// infra 错误映射为 codeInternal（task 层 HasSession infra error → newOpErr(codeInternal,...)）。
		reopenErr: &application.OpError{Code: "internal", Err: errors.New("tmux infra: has session failed")},
	}
	s := newAPITestServer(t, tb)
	srv := httptest.NewServer(s.mux)
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/terminal/t1"
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
	// infra 错误 → 1011 internal error（非 4010 task suspended）。
	_, _, rerr := conn.Read(context.Background())
	if got := websocket.CloseStatus(rerr); got != websocket.StatusCode(wsCloseInternalError) {
		t.Errorf("infra error close=%v want %d (1011 internal error, not 4010 suspended)", got, wsCloseInternalError)
	}
}

// TestAPI_WSTUI_ReopenAttachSuspended_4010 验证 ReopenAttach 返回 invalid_state
// （任务确非活跃）→ 4010 task suspended，区分 infra 错误路径。
func TestAPI_WSTUI_ReopenAttachSuspended_4010(t *testing.T) {
	tb := &fakeTaskBackend{
		reopenErr: &application.OpError{Code: "invalid_state", Err: errors.New("task not active")},
	}
	s := newAPITestServer(t, tb)
	srv := httptest.NewServer(s.mux)
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/terminal/t1"
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
	if got := websocket.CloseStatus(rerr); got != websocket.StatusCode(wsCloseTaskSuspended) {
		t.Errorf("invalid_state close=%v want %d (4010 task suspended)", got, wsCloseTaskSuspended)
	}
}

// TestAPI_WSTUI_ReopenAttachRecovering_1013 验证 D8（Phase 4）：ReopenAttach 返回
// typed recovering（进程恢复中）→ WS close code 1013（Try Again Later），前端据此
// 轮询任务状态后重连；MUST NOT 映射为 4010 suspended 或其他非重试关闭码。
func TestAPI_WSTUI_ReopenAttachRecovering_1013(t *testing.T) {
	tb := &fakeTaskBackend{
		reopenErr: &application.OpError{Code: "recovering", Err: errors.New("task t1 process is starting")},
	}
	s := newAPITestServer(t, tb)
	srv := httptest.NewServer(s.mux)
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/terminal/t1"
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
	if got := websocket.CloseStatus(rerr); got != websocket.StatusCode(wsCloseRecovering) {
		t.Errorf("recovering close=%v want %d (1013 try again later, NOT 4010 suspended)", got, wsCloseRecovering)
	}
}

// TestAPI_WSTUI_ReopenAttachConflict_1011Not4010 验证 G4-3：锁竞争等 transient
// conflict（ReopenAttach 等锁超时后仍忙）→ 1011 可重试关闭，MUST NOT 进 4010
// 让前端误判挂起停止重连。
func TestAPI_WSTUI_ReopenAttachConflict_1011Not4010(t *testing.T) {
	tb := &fakeTaskBackend{
		reopenErr: &application.OpError{Code: "conflict", Err: errors.New("task busy")},
	}
	s := newAPITestServer(t, tb)
	srv := httptest.NewServer(s.mux)
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/terminal/t1"
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
	if got := websocket.CloseStatus(rerr); got != websocket.StatusCode(wsCloseInternalError) {
		t.Errorf("conflict close=%v want %d (1011 retryable, NOT 4010 suspended)", got, wsCloseInternalError)
	}
}
