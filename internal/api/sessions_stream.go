// sessions_stream.go GET /api/v1/tasks/active/stream SSE 端点（sse-active-sessions
// P2.3；design.md D3）。
//
// projects-stream design D5 起，SSE 循环核心（四 topic 订阅 fan-in、初始组装失败
// 500（写头前）、组装期 drain 判脏、snapshot 首帧立即 flush、合并窗口、溢出重推、
// 心跳、断连/关停退出）抽取至 read_model_stream.go 由各读模型流共享（MUST NOT
// 平行复制）；本端点为 active sessions 场景薄绑定：组装器 buildActiveSessionsSnapshot
// （sessions_snapshot.go）+ 消费过滤 eventDirtiesActiveSessions（sessions_filter.go）。
// writeSSEFrame/sseIntervals 为共享件保留在本文件。
package api

import (
	"bytes"
	"context"
	"net/http"
	"time"
)

// 对外 SSE 帧事件名（design D3：仅 snapshot/update；心跳为注释行，不是 event）。
const (
	sseEventSnapshot    = "snapshot"
	sseEventUpdate      = "update"
	sseHeartbeatComment = "ping"
	sseDefaultCoalesce  = 500 * time.Millisecond
	sseDefaultHeartbeat = 25 * time.Second
)

// sseIntervals 返回 SSE 流合并窗口与心跳间隔；注入值（同包测试用）为零/负时回落
// 生产默认 500ms/25s。
func (s *Server) sseIntervals() (coalesce, heartbeat time.Duration) {
	coalesce = s.sseCoalesce
	if coalesce <= 0 {
		coalesce = sseDefaultCoalesce
	}
	heartbeat = s.sseHeartbeat
	if heartbeat <= 0 {
		heartbeat = sseDefaultHeartbeat
	}
	return coalesce, heartbeat
}

// handleActiveSessionsStream GET /api/v1/tasks/active/stream（design D3；design D5
// 薄封装：循环核心 + 场景组装器/过滤表注入）。
func (s *Server) handleActiveSessionsStream(w http.ResponseWriter, r *http.Request) {
	s.runReadModelStream(w, r, readModelStreamConfig{
		assemble: func(ctx context.Context) (any, error) {
			return s.buildActiveSessionsSnapshot(ctx)
		},
		eventDirty: eventDirtiesActiveSessions,
		logPrefix:  "active sessions stream",
		errCopy:    "list active sessions failed",
	})
}

// writeSSEFrame SSE 统一写路径（design D3）：写完整帧后经 http.NewResponseController
// Flush（经 statusRecorder 的 Unwrap/FlushError 链到达底层连接）。event 非空 →
// `event: <event>\ndata: <data>\n\n`；event 空 → 注释行 `: <data>\n\n`（心跳）。
// data 为单行（json.Marshal 不产生裸换行）。任何 Write/Flush 错误返回 error，
// 调用方 MUST 立即退订退出。
func writeSSEFrame(w http.ResponseWriter, event string, data []byte) error {
	var frame bytes.Buffer
	if event == "" {
		frame.WriteString(": ")
	} else {
		frame.WriteString("event: ")
		frame.WriteString(event)
		frame.WriteString("\ndata: ")
	}
	frame.Write(data)
	frame.WriteString("\n\n")
	if _, err := w.Write(frame.Bytes()); err != nil {
		return err
	}
	return http.NewResponseController(w).Flush()
}
