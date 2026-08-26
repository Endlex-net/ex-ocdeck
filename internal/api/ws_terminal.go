package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/coder/websocket"

	"ocdeck/internal/application"
	"ocdeck/internal/pty"
)

// handleWSTUI 处理 /ws/terminal/:taskID（TUI 终端，design.md §7/§21）。
// 首帧 auth（5s 超时、认证成功前不订阅 PTY）→ 以握手尺寸创建 attach 客户端 PTY
// → TUI 会话消失则自动 ReopenAttach → 二进制双向 IO + JSON resize。
// 单交互客户端：同一终端新连接替换旧连接（4009）。
func (s *Server) handleWSTUI(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("taskID")
	if s.tasks == nil {
		http.Error(w, "task backend not configured", http.StatusServiceUnavailable)
		return
	}
	if !s.checkWSOrigin(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	c, err := acceptWS(w, r)
	if err != nil {
		return
	}
	// 首帧认证（5s 超时）。
	auth, ok := s.wsAuthHandshake(r.Context(), c)
	if !ok {
		wsClose(context.Background(), c, wsCloseAuthFailed, "auth failed")
		return
	}

	// 任务须活跃；TUI 会话消失则自动 ReopenAttach（design.md §21）。
	// 错误按 code 区分关闭码：invalid_state/not_found/conflict（任务确非活跃/锁冲突）
	// → 4010 task suspended；internal/process_error（infra 故障，如 HasSession tmux 错误）
	// → 1011 internal error，不得误判为 4010 掩盖 infra 故障（design.md §8/§21）。
	tid, err := s.tasks.ReopenAttach(r.Context(), taskID)
	if err != nil {
		code := ErrorCode(application.OpErrorCode(err))
		switch code {
		case CodeInvalidState, CodeNotFound, CodeConflict:
			wsClose(context.Background(), c, wsCloseTaskSuspended, "task not active")
		default:
			wsClose(context.Background(), c, wsCloseInternalError, "reopen attach failed")
		}
		return
	}
	// 等待 attach 客户端 PTY 创建（process.AttachPty）。
	p, err := s.tasks.AttachPty(string(tid), auth.Cols, auth.Rows)
	if err != nil {
		wsClose(context.Background(), c, wsCloseInternalError, "attach failed")
		return
	}
	defer p.Close()

	// 单交互客户端注册：新连接替换旧连接（4009）。
	// B4：先向旧连接发 4009（等待写出或短超时），再 cancel 旧 bridge，保证旧连接稳定收到 4009，
	// 且旧 bridge 在被取消路径不发 1000（见 bridgeTerminal）。
	key := terminalKey(taskID, false)
	oldConn, oldCancel, bridgeCtx := s.wsClients.register(key, c)
	if oldConn != nil {
		wsCloseReplacedWait(oldConn)
		oldCancel()
	}
	defer s.wsClients.unregister(key, c)

	_ = c.Write(r.Context(), websocket.MessageText, mustJSON(wsAuthResp{Type: "auth_ok"}))
	s.bridgeTerminal(bridgeCtx, c, p)
}

// handleWSShell 处理 /ws/terminal/shell/:tid（shell 终端）。
func (s *Server) handleWSShell(w http.ResponseWriter, r *http.Request) {
	tid := r.PathValue("tid")
	if s.tasks == nil {
		http.Error(w, "task backend not configured", http.StatusServiceUnavailable)
		return
	}
	if !s.checkWSOrigin(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	c, err := acceptWS(w, r)
	if err != nil {
		return
	}
	auth, ok := s.wsAuthHandshake(r.Context(), c)
	if !ok {
		wsClose(context.Background(), c, wsCloseAuthFailed, "auth failed")
		return
	}
	// shell WS 身份校验（design.md §21）：terminalID MUST 对应该任务的一个 shell 终端，
	// 不得 attach 任意合法 tmux 会话名（如 serve/TUI/其他任务会话）。
	// 关闭码按错误类型分流（第三轮 terminal-streaming spec 契约）：
	//   - not_found/invalid_input（terminal 确非活跃/非法 tid）→ 4004（terminal not found），
	//     前端 4004 是"终端已关闭"永久停止重连；
	//   - process_error/internal（tmux 基础设施故障等服务端错误）→ 1011（internal error），
	//     走默认重连路径（临时故障可恢复），不得误发 4004 致前端永久停止。
	if err := s.tasks.ValidateShellTerminal(tid); err != nil {
		code := ErrorCode(application.OpErrorCode(err))
		switch code {
		case CodeNotFound, CodeInvalidInput, CodeConflict:
			wsClose(context.Background(), c, wsCloseTerminalNotFound, "terminal not found")
		default:
			// CodeProcessError / CodeInternal / 其他 → 1011 infra/服务端错误，前端可重连。
			wsClose(context.Background(), c, wsCloseInternalError, "terminal validation failed")
		}
		return
	}
	p, err := s.tasks.AttachPty(tid, auth.Cols, auth.Rows)
	if err != nil {
		wsClose(context.Background(), c, wsCloseInternalError, "attach failed")
		return
	}
	defer p.Close()

	// 单交互客户端注册：新连接替换旧连接（4009）。
	// B4：先向旧连接发 4009（等待写出或短超时），再 cancel 旧 bridge，保证旧连接稳定收到 4009，
	// 且旧 bridge 在被取消路径不发 1000（见 bridgeTerminal）。
	key := terminalKey(tid, true)
	oldConn, oldCancel, bridgeCtx := s.wsClients.register(key, c)
	if oldConn != nil {
		wsCloseReplacedWait(oldConn)
		oldCancel()
	}
	defer s.wsClients.unregister(key, c)

	_ = c.Write(r.Context(), websocket.MessageText, mustJSON(wsAuthResp{Type: "auth_ok"}))
	s.bridgeTerminal(bridgeCtx, c, p)
}

// mustJSON 序列化为 JSON，失败返回空（仅用于内部固定响应）。
func mustJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}

// bridgeTerminal 桥接 WS 与 PTY（design.md §7）：
//   - PTY→WS：有界写队列削峰，队列满（慢客户端）即断开；
//   - WS→PTY：二进制帧写入 PTY，JSON resize 控制帧调整尺寸；
//   - 双向取消：任一方向退出即取消另一方向（Wait 不永久挂）。
//
// ctx 为 bridge 的 replaceCtx：被新连接替换（wsClientRegistry.register 返回的 cancel）或
// HTTP 请求结束时会被取消。B4：ctx 取消路径 MUST NOT 抢先发 1000——被替换时由新连接负责
// 发送 4009，正常结束（PTY/WS EOF，ctx 未被取消）才发 1000。故内部用独立 loopCtx 驱动双向
// 退出，wg.Wait() 后按 ctx.Err() 区分：被取消→直接退出（不发 1000），否则发 1000。
func (s *Server) bridgeTerminal(ctx context.Context, c *websocket.Conn, p *pty.Pty) {
	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)

	// PTY → WS（有界写队列 + 慢客户端断开）。
	go func() {
		defer wg.Done()
		defer cancel() // PTY 退出即取消 WS→PTY 方向
		if !pumpPTYToWS(loopCtx, c, p) {
			// 读 PTY 结束或写队列满/失败 → 结束桥接。
			return
		}
	}()

	// WS → PTY（二进制 + resize 控制帧）。
	go func() {
		defer wg.Done()
		defer cancel() // WS 退出即取消 PTY→WS 方向
		pumpWSToPTY(loopCtx, c, p)
	}()

	wg.Wait()
	// B4：被替换/请求取消（ctx 取消）时不发 1000——由替换方发 4009；正常结束才发 1000。
	if ctx.Err() != nil {
		return
	}
	wsClose(context.Background(), c, wsCloseNormal, "")
}

// pumpPTYToWS 从 PTY 读取并写入 WS。写操作经有界队列削峰：队列满即认为客户端
// 消费不及，返回 false 触发断开（B10：慢客户端断开）。
// 返回 false 表示应结束桥接（PTY 读结束或写失败/背压）。
//
// 写 goroutine 使用独立可取消子 ctx：PTY 读结束或队列满时 MUST 先取消该子 ctx 再等
// 写 goroutine 退出（join）。否则慢客户端下写 goroutine 阻塞在 c.Write（客户端不读），
// 而 pumpPTYToWS 阻塞在 <-done 等写 goroutine → 桥接尚未收到 false 返回值、未触发
// defer cancel() → 死锁。取消子 ctx 使写 goroutine 的 c.Write 立即返回错误退出。
func pumpPTYToWS(ctx context.Context, c *websocket.Conn, p *pty.Pty) bool {
	// 写 goroutine 子 ctx：返回前取消以解除慢客户端下 c.Write 阻塞。
	wctx, wcancel := context.WithCancel(ctx)
	// 有界写队列：PTY 读循环将数据投递到队列，独立写 goroutine 串行写 WS。
	queue := make(chan []byte, wsWriteQueueCap)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for data := range queue {
			if err := c.Write(wctx, websocket.MessageBinary, data); err != nil {
				return
			}
		}
	}()
	for {
		data, err := p.ReadCtx(ctx)
		if err != nil {
			// PTY 读结束或 ctx 取消：取消写 goroutine 的写（解除慢客户端 c.Write 阻塞）再 join。
			wcancel()
			close(queue)
			<-done
			return false
		}
		select {
		case queue <- data:
		default:
			// 队列满：慢客户端，断开。取消写 goroutine 的写（解除 c.Write 阻塞）再 join。
			wcancel()
			close(queue)
			<-done
			return false
		}
	}
}

// pumpWSToPTY 从 WS 读取并写入 PTY。二进制帧写入 PTY；JSON resize 控制帧调整尺寸。
// ctx 取消或 WS 读错误即退出。
func pumpWSToPTY(ctx context.Context, c *websocket.Conn, p *pty.Pty) {
	for {
		typ, payload, err := c.Read(ctx)
		if err != nil {
			return
		}
		switch typ {
		case websocket.MessageBinary:
			if _, err := p.Write(payload); err != nil {
				return
			}
		case websocket.MessageText:
			// 可能是 resize 控制帧（JSON）。
			var ctrl wsCtrlFrame
			if json.Unmarshal(payload, &ctrl) == nil && ctrl.Type == "resize" {
				_ = p.Resize(ctrl.Cols, ctrl.Rows)
			} else {
				// 非控制帧按二进制输入处理。
				if _, err := p.Write(payload); err != nil {
					return
				}
			}
		}
	}
}
