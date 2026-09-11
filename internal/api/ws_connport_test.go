package api

import (
	"testing"

	"github.com/coder/websocket"
)

// TestCurrentTUIConnID_ConnTracking 验证 currentTUIConnID 的连接当前性语义
//（terminal-file-paste-drop：deliver/上传准入步骤①的数据源）。
func TestCurrentTUIConnID_ConnTracking(t *testing.T) {
	t.Run("current_tui_conn_returned", func(t *testing.T) {
		reg := newWSClientRegistry()
		reg.register(terminalKey("t1", false), &websocket.Conn{}, &wsCloseGuard{}, "conn-1")
		id, ok := reg.currentTUIConnID("t1")
		if !ok || id != "conn-1" {
			t.Fatalf("currentTUIConnID = (%q, %v), want (\"conn-1\", true)", id, ok)
		}
	})

	t.Run("replacement_invalidates_old_conn", func(t *testing.T) {
		reg := newWSClientRegistry()
		reg.register(terminalKey("t1", false), &websocket.Conn{}, &wsCloseGuard{}, "conn-1")
		reg.register(terminalKey("t1", false), &websocket.Conn{}, &wsCloseGuard{}, "conn-2") // 换代
		id, ok := reg.currentTUIConnID("t1")
		if !ok || id != "conn-2" {
			t.Fatalf("after replace currentTUIConnID = (%q, %v), want (\"conn-2\", true)", id, ok)
		}
		if id == "conn-1" {
			t.Fatal("old connID must not be reported as current")
		}
	})

	t.Run("shell_conn_not_tui", func(t *testing.T) {
		reg := newWSClientRegistry()
		reg.register(terminalKey("t1", true), &websocket.Conn{}, &wsCloseGuard{}, "shell-conn")
		if id, ok := reg.currentTUIConnID("t1"); ok {
			t.Fatalf("shell conn reported as TUI: (%q, %v), want not found", id, ok)
		}
	})

	t.Run("unregistered_and_after_unregister", func(t *testing.T) {
		reg := newWSClientRegistry()
		if id, ok := reg.currentTUIConnID("missing"); ok {
			t.Fatalf("unregistered task reported: (%q, %v)", id, ok)
		}
		c := &websocket.Conn{}
		reg.register(terminalKey("t1", false), c, &wsCloseGuard{}, "conn-1")
		reg.unregister(terminalKey("t1", false), c)
		if id, ok := reg.currentTUIConnID("t1"); ok {
			t.Fatalf("after unregister reported: (%q, %v), want not found", id, ok)
		}
	})
}

// TestServer_CurrentTUIConnID 委托注册表（组合根 ConnPort 数据源）。
func TestServer_CurrentTUIConnID(t *testing.T) {
	s := &Server{wsClients: newWSClientRegistry()}
	if _, ok := s.CurrentTUIConnID("t1"); ok {
		t.Fatal("unregistered task should not be found")
	}
	s.wsClients.register(terminalKey("t1", false), &websocket.Conn{}, &wsCloseGuard{}, "conn-9")
	id, ok := s.CurrentTUIConnID("t1")
	if !ok || id != "conn-9" {
		t.Fatalf("Server.CurrentTUIConnID = (%q, %v), want (\"conn-9\", true)", id, ok)
	}
}
