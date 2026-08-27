// read_model_stream.go SSE 读模型流共享循环核心（projects-stream design D5；自
// sse-active-sessions P2.3 的 sessions_stream.go handler 循环抽取，供
// /api/v1/tasks/active/stream 与 /api/v1/projects/stream 等读模型投影流共用，
// MUST NOT 平行复制）。
//
// 建连状态机：先对 task/session/serve_runtime/control 四 topic 各 Subscribe 一次并
// fan-in，再组装场景初始快照——失败在写 SSE headers 前退订返 500（不留悬挂连接）；
// 成功则写 200+headers、发完整 snapshot 帧后 Flush（首帧立即可达），组装期间按场景
// 消费过滤表标脏的事件在 snapshot 后立即补一帧 update 收敛（消除订阅竞态）。
//
// 事件循环：过滤表命中置 dirty，固定 500ms 合并窗口到期重组装全量快照发 update
// （仅成功写出并 flush 后清 dirty；组装/序列化失败保持 dirty 记日志不闭连，由窗口/
// 心跳 tick 重试）；任一路 Overflow() 置位先置 dirty 再窗口外立即重推（自愈）；心跳
// 语义为「连续 25s 无业务帧」——任一成功写出的业务帧（snapshot/update/溢出重推，
// 含心跳 tick 上的 update）重新起算，静默期发出 `: ping` 注释行并兼作写错误探测。
// 所有帧经统一 writeSSEFrame 写出；任何 Write/Flush 失败立即退订退出（客户端重连
// 经 snapshot 自愈）。场景差异仅经 readModelStreamConfig 注入（组装器/过滤表/文案），
// 循环纪律不注入行为；推送路径纯读（不调用 opencode、不做 store 写）由场景组装器
// 保证。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	ocdeckevent "ocdeck/internal/domain/event"
)

// errStreamGone 推送路径 assembleGone 命中后的包级 sentinel：调用方按写失败同等
// 处理（退订退出 handler），但不记错误日志（正常业务终态，task-detail-stream D3）。
var errStreamGone = errors.New("read model stream gone")

// readModelStreamTopics SSE 消费的四个领域 topic（sse-active-sessions design D3）；
// 顺序固定，订阅句柄按下标对应（事件循环的静态 select 逐路挂接）。
var readModelStreamTopics = []ocdeckevent.Topic{
	ocdeckevent.TopicTask,
	ocdeckevent.TopicSession,
	ocdeckevent.TopicServeRuntime,
	ocdeckevent.TopicControl,
}

// readModelStreamConfig 场景注入点（design D5）：仅快照组装函数、判脏函数与呈现
// 参数（日志前缀/500 文案）；四 topic 订阅、合并窗口、溢出重推、心跳与退出纪律
// 全部固定在核心内，不注入行为。
type readModelStreamConfig struct {
	// assemble 组装场景完整快照，与对应 REST 响应同构（裸数组或单对象）；store 失败
	// 返回 error（初始组装经 errCopy 返 500，重推保持 dirty 重试）。
	assemble func(ctx context.Context) (any, error)
	// eventDirty 场景消费过滤表（如 eventDirtiesActiveSessions）：标脏后重组装
	// 全量快照重推，不做增量合并。
	eventDirty func(ev ocdeckevent.Event) bool
	// assembleGone 可选：判定组装错误是否表示主体已消失。nil 时全部路径保持现有行为。
	// 初始组装 gone → JSON 404（不写 SSE 头）；pushUpdate gone → 返回 errStreamGone。
	assembleGone func(error) bool
	// logPrefix 重推组装/序列化失败日志的前缀（如 "active sessions stream"）。
	logPrefix string
	// errCopy 初始组装失败 500 错误信封文案（如 "list active sessions failed"）。
	errCopy string
}

// runReadModelStream SSE 读模型流共享循环核心（design D5）：建连状态机与事件循环
// 全量固定，场景差异收敛为 cfg 注入点。
func (s *Server) runReadModelStream(w http.ResponseWriter, r *http.Request, cfg readModelStreamConfig) {
	ctx := r.Context()
	coalesce, heartbeat := s.sseIntervals()

	// 先订阅再组装（design D3/D5）：订阅与首次组装之间的变更经 dirty 标记在 snapshot
	// 后补一帧 update 收敛，杜绝"查询与订阅之间的变更永久漏掉"。退出路径统一经
	// defer 全量退订（写失败/客户端断开/进程 ctx 取消）。
	subs := make([]EventSubscription, len(readModelStreamTopics))
	for i, topic := range readModelStreamTopics {
		subs[i] = s.eventSubscriber.Subscribe(topic)
	}
	defer func() {
		for _, sub := range subs {
			sub.Close()
		}
	}()

	// 初始组装（写 SSE headers 前）：失败 → 退订 + 500 标准错误信封；
	// assembleGone 命中 → JSON 404（不写 SSE 头，订阅经 defer 退订）。
	snap, err := cfg.assemble(ctx)
	if err != nil {
		if cfg.assembleGone != nil && cfg.assembleGone(err) {
			writeError(w, CodeNotFound, "task not found")
			return
		}
		writeError(w, CodeInternal, cfg.errCopy)
		return
	}
	data, err := json.Marshal(snap)
	if err != nil {
		writeError(w, CodeInternal, cfg.errCopy)
		return
	}

	// 组装期间到达的事件已在订阅缓冲中：按场景消费过滤表判脏，snapshot 后补一帧
	// update。
	dirty := false
	for _, sub := range subs {
	drain:
		for {
			select {
			case ev, ok := <-sub.C():
				if !ok {
					break drain
				}
				if cfg.eventDirty(ev) {
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
	// 心跳 tick 仍兼作 dirty 重试点。本循环单 goroutine 持有 hb；Go 1.23+ timer
	// channel 无缓冲，Reset 后不会送达陈旧到期值，单 goroutine Reset 安全。
	hb := time.NewTimer(heartbeat)
	defer hb.Stop()
	window := time.NewTicker(coalesce)
	defer window.Stop()

	// pushUpdate 重组装全量快照并写一帧 update（design D3：update 为场景完整快照、
	// 与对应 REST 响应同构，而非增量 diff）。组装/序列化失败保持 dirty、记日志、不写帧、
	// 不重置心跳（连接保留，由窗口/心跳 tick 或溢出重推重试）；assembleGone 命中返回
	// errStreamGone（调用方退订退出，不记错误日志）；仅成功写出并 flush 后清除 dirty
	// 并重置心跳；写/flush 失败返回 error，调用方立即退订退出。
	pushUpdate := func() error {
		snap, err := cfg.assemble(ctx)
		if err != nil {
			if cfg.assembleGone != nil && cfg.assembleGone(err) {
				return errStreamGone
			}
			log.Printf("%s: reassemble snapshot failed, keep dirty for retry: %v", cfg.logPrefix, err)
			return nil
		}
		data, err := json.Marshal(snap)
		if err != nil {
			log.Printf("%s: marshal snapshot failed, keep dirty for retry: %v", cfg.logPrefix, err)
			return nil
		}
		if err := writeSSEFrame(w, sseEventUpdate, data); err != nil {
			return err
		}
		dirty = false
		hb.Reset(heartbeat)
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
			if cfg.eventDirty(ev) {
				dirty = true
			}
		case ev := <-subs[1].C():
			if cfg.eventDirty(ev) {
				dirty = true
			}
		case ev := <-subs[2].C():
			if cfg.eventDirty(ev) {
				dirty = true
			}
		case ev := <-subs[3].C():
			if cfg.eventDirty(ev) {
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
