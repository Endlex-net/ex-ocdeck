// webhub.go 通知 WebHub 与 GET /api/v1/notifications/stream（design D7）。
//
// 不经进程内 bus：注册表 + 每连接带缓冲 channel（容量 16）+ 非阻塞 enqueue。
// 投递 Publish 遍历连接：缓冲满判定为慢客户端，断开并移除（前端自动重连，
// 通知不重放——spec「网页通知渠道」断线重连不重放）。accepted=true 当且仅当
// 至少一个连接本次 enqueue 成功；零连接或全部连接缓冲满均为 accepted=false。
// 帧格式 `event: notification` + 单行 data（snake_case 七字段 JSON）；无 snapshot/重放。
package api

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"ocdeck/internal/domain/notification"
)

const (
	webHubBufferPerConn       = 16
	webHubDefaultWriteTimeout = 2 * time.Second
)

// WebHub 已连接 SSE 前端注册表；实现 notify.WebPublisher 窄端口
// （Publish(Intent) bool，Lane C web.go 已定）。零值不可用，经 newWebHub 构造。
type WebHub struct {
	mu    sync.Mutex
	conns map[*webHubConn]struct{}
}

func newWebHub() *WebHub {
	return &WebHub{conns: map[*webHubConn]struct{}{}}
}

// NotificationHub 返回路由与 web 渠道共享的 WebHub 实例（design D11：
// `webHub := srv.NotificationHub()`，组合根装配 web 渠道适配器时注入同一实例）。
func (s *Server) NotificationHub() *WebHub {
	return s.webHub
}

// webHubConn 单连接：带缓冲投递 channel + 独立取消信号（慢客户端断开时唤醒
// 阻塞在 Write/Flush 的 handler，使其及时退出而不排空缓冲）。
type webHubConn struct {
	ch     chan notification.Intent
	done   chan struct{}
	closed bool // 仅 hub 锁内读写
}

func (h *WebHub) Publish(in notification.Intent) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	accepted := false
	for c := range h.conns {
		select {
		case c.ch <- in:
			accepted = true
		default:
			delete(h.conns, c)
			c.close()
		}
	}
	return accepted
}

func (c *webHubConn) close() {
	if !c.closed {
		c.closed = true
		close(c.done)
	}
}

func (h *WebHub) register() *webHubConn {
	c := &webHubConn{
		ch:   make(chan notification.Intent, webHubBufferPerConn),
		done: make(chan struct{}),
	}
	h.mu.Lock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()
	return c
}

func (h *WebHub) unregister(c *webHubConn) {
	h.mu.Lock()
	if _, ok := h.conns[c]; ok {
		delete(h.conns, c)
		c.close()
	}
	h.mu.Unlock()
}

type notificationFrameIntent struct {
	TaskID   string `json:"task_id"`
	TaskName string `json:"task_name"`
	Category string `json:"category"`
	Level    string `json:"level"`
	Title    string `json:"title"`
	Body     string `json:"body"`
	URL      string `json:"url"`
}

func (s *Server) handleNotificationsStream(w http.ResponseWriter, r *http.Request) {
	c := s.webHub.register()
	defer s.webHub.unregister(c)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	if err := s.writeNotificationSSEFrame(w, "", []byte(sseHeartbeatComment)); err != nil {
		return
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case in, ok := <-c.ch:
			if !ok {
				return
			}
			data, err := json.Marshal(notificationFrameIntent{
				TaskID:   in.TaskID,
				TaskName: in.TaskName,
				Category: string(in.Category),
				Level:    string(in.Level),
				Title:    in.Title,
				Body:     in.Body,
				URL:      in.URL,
			})
			if err != nil {
				continue
			}
			if err := s.writeNotificationSSEFrame(w, "notification", data); err != nil {
				return
			}
		}
	}
}

func (s *Server) writeNotificationSSEFrame(w http.ResponseWriter, event string, data []byte) error {
	timeout := s.webHubWriteTimeout
	if timeout <= 0 {
		timeout = webHubDefaultWriteTimeout
	}
	// 写期限必须生效后再同步 Write/Flush：不支持 deadline 则立即失败并关连接，
	// 禁止另起 goroutine（超时后写协程可能泄漏或在 ServeHTTP 返回后继续写）。
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	return writeSSEFrame(w, event, data)
}
