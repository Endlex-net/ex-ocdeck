// sessions_stream.go GET /api/v1/sessions/active/stream SSE 端点（sse-active-sessions
// P2.3；design.md D3 建连状态机与推送语义）。
//
// 建连状态机：先对 task/session/serve_runtime/control 四 topic 各 Subscribe 一次并
// fan-in，再组装初始快照——失败在写 SSE headers 前退订返 500（不留悬挂连接）；成功
// 则写 200+headers、发完整 snapshot 帧后 Flush（首帧立即可达），组装期间按消费过滤
// 表标脏的事件在 snapshot 后立即补一帧 update 收敛（消除订阅竞态）。
//
// 事件循环：过滤表命中置 dirty，固定 500ms 合并窗口到期重组装全量快照发 update
// （仅成功写出并 flush 后清 dirty；组装失败保持 dirty 由窗口/心跳 tick 重试，不闭连）；
// 任一路 Overflow() 置位先置 dirty 再窗口外立即重推（自愈）；心跳语义为「连续 25s
// 无业务帧」——任一成功写出的业务帧（snapshot/update/溢出重推，含心跳 tick 上的
// update）重新起算，静默期发出 `: ping` 注释行并兼作写错误探测（design D3）。
// 所有帧经统一 writeSSEFrame 写出；任何 Write/Flush 失败立即退订退出（客户端重连
// 经 snapshot 自愈）。推送路径纯读：不调用 opencode、不做 store 写。
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	ocdeckevent "ocdeck/internal/domain/event"
)

// 对外 SSE 帧事件名（design D3：仅 snapshot/update；心跳为注释行，不是 event）。
const (
	sseEventSnapshot    = "snapshot"
	sseEventUpdate      = "update"
	sseHeartbeatComment = "ping"
	sseDefaultCoalesce  = 500 * time.Millisecond
	sseDefaultHeartbeat = 25 * time.Second
)

// activeSessionsStreamTopics SSE 消费的四个领域 topic（design D3）；顺序固定，
// 订阅句柄按下标对应（事件循环的静态 select 逐路挂接）。
var activeSessionsStreamTopics = []ocdeckevent.Topic{
	ocdeckevent.TopicTask,
	ocdeckevent.TopicSession,
	ocdeckevent.TopicServeRuntime,
	ocdeckevent.TopicControl,
}

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

// handleActiveSessionsStream GET /api/v1/sessions/active/stream（design D3）。
func (s *Server) handleActiveSessionsStream(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	coalesce, heartbeat := s.sseIntervals()

	// 先订阅再组装（design D3）：订阅与首次组装之间的变更经 dirty 标记在 snapshot
	// 后补一帧 update 收敛，杜绝"查询与订阅之间的变更永久漏掉"。退出路径统一经
	// defer 全量退订（写失败/客户端断开/进程 ctx 取消）。
	subs := make([]EventSubscription, len(activeSessionsStreamTopics))
	for i, topic := range activeSessionsStreamTopics {
		subs[i] = s.eventSubscriber.Subscribe(topic)
	}
	defer func() {
		for _, sub := range subs {
			sub.Close()
		}
	}()

	// 初始组装（写 SSE headers 前）：失败 → 退订 + 500 标准错误信封。
	snap, err := s.buildActiveSessionsSnapshot(ctx)
	if err != nil {
		writeError(w, CodeInternal, "list active sessions failed")
		return
	}
	data, err := json.Marshal(snap)
	if err != nil {
		writeError(w, CodeInternal, "list active sessions failed")
		return
	}

	// 组装期间到达的事件已在订阅缓冲中：按消费过滤表判脏（session.* 等标脏、
	// task.created/两端非 active 不标脏），snapshot 后补一帧 update。
	dirty := false
	for _, sub := range subs {
	drain:
		for {
			select {
			case ev, ok := <-sub.C():
				if !ok {
					break drain
				}
				if eventDirtiesActiveSessions(ev) {
					dirty = true
				}
			default:
				break drain
			}
		}
	}

	// 写 200 + SSE headers，发完整 snapshot 帧后 Flush：首帧立即可达。
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	if err := writeSSEFrame(w, sseEventSnapshot, data); err != nil {
		return // 连接已坏：defer 退订退出
	}

	// 心跳语义为「连续 heartbeat 间隔无业务帧」（design D3）：任一成功写出的业务帧
	// （snapshot/update/溢出重推，含心跳 tick 上写出的 update）都重新起算心跳。
	// 心跳 tick 仍兼作 dirty 重试点。本 handler 单 goroutine 持有 hb；Go 1.23+
	// timer channel 无缓冲，Reset 后不会送达陈旧到期值，单 goroutine Reset 安全。
	hb := time.NewTimer(heartbeat)
	defer hb.Stop()
	window := time.NewTicker(coalesce)
	defer window.Stop()

	// pushUpdate 写一帧 update（组装失败保持 *dirty、不写帧、不重置心跳）。
	// 成功写出并 flush 后清除 *dirty 并重置心跳；写/flush 失败返回 error，
	// 调用方立即退订退出。
	pushUpdate := func() error {
		if err := s.pushActiveSessionsUpdate(ctx, w, &dirty); err != nil {
			return err
		}
		if !dirty {
			hb.Reset(heartbeat)
		}
		return nil
	}

	// 组装期间标脏 → 立即补一帧 update（组装失败保持 dirty 由窗口/心跳 tick 重试）。
	if dirty {
		if err := pushUpdate(); err != nil {
			return
		}
	}

	for {
		select {
		case <-ctx.Done():
			// 客户端断开或进程 ctx 取消（P2.4 BaseContext）：退订退出。
			return
		case ev := <-subs[0].C():
			if eventDirtiesActiveSessions(ev) {
				dirty = true
			}
		case ev := <-subs[1].C():
			if eventDirtiesActiveSessions(ev) {
				dirty = true
			}
		case ev := <-subs[2].C():
			if eventDirtiesActiveSessions(ev) {
				dirty = true
			}
		case ev := <-subs[3].C():
			if eventDirtiesActiveSessions(ev) {
				dirty = true
			}
		case <-subs[0].Overflow():
			// 溢出自愈：先置 dirty 再窗口外立即重推（design D3）；仅成功写出并
			// flush 后清除，组装失败保持 dirty 由窗口/心跳 tick 继续重试。
			dirty = true
			if err := pushUpdate(); err != nil {
				return
			}
		case <-subs[1].Overflow():
			dirty = true
			if err := pushUpdate(); err != nil {
				return
			}
		case <-subs[2].Overflow():
			dirty = true
			if err := pushUpdate(); err != nil {
				return
			}
		case <-subs[3].Overflow():
			dirty = true
			if err := pushUpdate(); err != nil {
				return
			}
		case <-window.C:
			if dirty {
				if err := pushUpdate(); err != nil {
					return
				}
			}
		case <-hb.C:
			if dirty {
				// 心跳 tick 兼作组装失败的重试点（design D3）。
				if err := pushUpdate(); err != nil {
					return
				}
				if !dirty {
					// 本次 tick 写出 update：业务帧重新起算心跳，跳过 ping。
					continue
				}
			}
			if err := writeSSEFrame(w, "", []byte(sseHeartbeatComment)); err != nil {
				return
			}
			hb.Reset(heartbeat)
		}
	}
}

// pushActiveSessionsUpdate 重组装全量快照并写 update 帧（design D3：update 为全量
// 裸数组而非增量 diff）。仅成功写出并 flush 后清除 *dirty；组装/序列化失败保持
// *dirty 并记日志（连接保留，由窗口/心跳 tick 或溢出重推重试）；写/flush 失败返回
// error，调用方立即退订退出。
func (s *Server) pushActiveSessionsUpdate(ctx context.Context, w http.ResponseWriter, dirty *bool) error {
	snap, err := s.buildActiveSessionsSnapshot(ctx)
	if err != nil {
		log.Printf("active sessions stream: reassemble snapshot failed, keep dirty for retry: %v", err)
		return nil
	}
	data, err := json.Marshal(snap)
	if err != nil {
		log.Printf("active sessions stream: marshal snapshot failed, keep dirty for retry: %v", err)
		return nil
	}
	if err := writeSSEFrame(w, sseEventUpdate, data); err != nil {
		return err
	}
	*dirty = false
	return nil
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
