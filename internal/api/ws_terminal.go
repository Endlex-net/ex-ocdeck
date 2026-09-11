package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"ocdeck/internal/application"
	appuploads "ocdeck/internal/application/uploads"
	"ocdeck/internal/infrastructure/pty"
)

// bracketedPasteStart/End bracketed paste 起止标记（design D3：ESC[200~ + 绝对路径 +
// ESC[201~，单个完整消息，无 @、无回车）。
const (
	bracketedPasteStart = "\x1b[200~"
	bracketedPasteEnd   = "\x1b[201~"
)

// deliverWriteDeadline 生产默认投递单次 PTY 写入的服务端写 deadline（delivery spec
// bridge 收尾表：5s，覆盖底层写退出与后备终止）。s.deliverWriteDeadline 零值时使用；
// 测试注入短值，不改变生产语义。
const deliverWriteDeadline = 5 * time.Second

// deliverPTYWrite 投递单次可中断写入入口（与普通输入共用串行写路径）。方法表达式
// 便于故障注入测试替换（无错误短写/立即写错误）；生产恒为 (*pty.Pty).WriteTimeout。
var deliverPTYWrite = (*pty.Pty).WriteTimeout

// deliverOrchestrator 投递编排消费方窄接口（terminal-file-paste-drop 4.2：Lane A
// application 编排在组合根注入；api 层仅协议适配，准入决策归编排层）。
//
//   - inject 由编排层在 per-task 协调锁临界区内调用（写入退出契约见 delivery spec
//     「连接归属与投递准入」）；inject 仅完整写入返回 nil，其余统一由编排层映射
//     write_failed；
//   - 返回 code 为 deliver_result error 六枚举字面值（deliverResultErrorCodes）；
//     六枚举之外的返回值（含空串，Lane A 契约："" 表示基础设施故障）视为基础设施
//     故障：WS 适配层不回业务回执、以 1011 结束连接（D5：MUST NOT 伪造
//     not_found/task_inactive/write_failed 等业务码）；
//   - 签名钉死：Lane A 的 *uploads.Orchestrator.Deliver 返回 (bool, DeliverErrorCode)
//     （命名 string 类型，见 application/uploads/orchestrator.go），与本接口的
//     (bool, string) 非同一 Go 签名、不直接满足接口——组合根注入时以薄适配做
//     string(code) 转换（边界映射归组合根）。api 层仅 import application 包的
//     ctx helper（appuploads.InjectPathFrom）与数据类型；准入决策仍归编排层。
type deliverOrchestrator interface {
	Deliver(ctx context.Context, taskID, connID, uploadID string, inject func(ctx context.Context) error) (ok bool, code string)
}

// wsDeliverFrame client→server deliver 控制帧（terminal-streaming delta）。
type wsDeliverFrame struct {
	Type     string `json:"type"`
	UploadID string `json:"uploadId"`
}

// wsDeliverResult server→client deliver_result 控制帧。成功无 error 字段；失败
// ok=false 且 error 为六枚举之一。
type wsDeliverResult struct {
	Type     string `json:"type"`
	UploadID string `json:"uploadId"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

// deliverResultErrorCodes deliver_result error 六枚举（terminal-streaming delta；
// 与 deliverOrchestrator 返回 code 字面值一致）。
var deliverResultErrorCodes = map[string]bool{
	"not_found":     true,
	"expired":       true,
	"forbidden":     true,
	"task_inactive": true,
	"write_failed":  true,
	"invalid_input": true,
}

// isUploadID 校验 uploadId 为合法 32 hex（D2：仅 deliver 帧校验，resize 不校验）。
func isUploadID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// handleWSTUI 处理 /ws/terminal/:taskID（TUI 终端，design.md §7/§21）。
// 首帧 auth（5s 超时、认证成功前不订阅 PTY）→ 以握手尺寸创建 attach 客户端 PTY
// → TUI 会话消失则自动 ReopenAttach → 二进制双向 IO + JSON 控制帧（resize/deliver）。
// 单交互客户端：同一终端新连接替换旧连接（4009）。
// 每连接签发 connId 随 auth_ok 下发（terminal-file-paste-drop D5）。
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
	c, transport, err := acceptWS(w, r)
	if err != nil {
		return
	}
	// 首帧认证（5s 超时）。
	auth, ok := s.wsAuthHandshake(r.Context(), c)
	if !ok {
		wsClose(context.Background(), c, wsCloseAuthFailed, "auth failed")
		return
	}

	// 任务须活跃；进程缺失时 ReopenAttach 触发幂等恢复并返回 typed recovering。
	// 错误按 code 区分关闭码（D8 固定契约）：
	//   - recovering → 1013 Try Again Later（前端轮询任务状态，回 active 后重连；
	//     MUST NOT 映射为 4010 suspended 或其他非重试关闭码）；
	//   - invalid_state/not_found（任务确非活跃）→ 4010 task suspended；
	//   - conflict（G4-3：锁竞争等 transient，ReopenAttach 等锁超时后仍忙）→ 1011
	//     可重试关闭，MUST NOT 进 4010 让前端误判挂起停止重连；
	//   - internal/process_error（infra 故障，如 HasSession tmux 错误）→ 1011 internal
	//     error，不得误判为 4010 掩盖 infra 故障（design.md §8/§21）。
	tid, err := s.tasks.ReopenAttach(r.Context(), taskID)
	if err != nil {
		code := ErrorCode(application.OpErrorCode(err))
		switch code {
		case CodeRecovering:
			wsClose(context.Background(), c, wsCloseRecovering, "task process starting")
		case CodeInvalidState, CodeNotFound:
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

	connID := uuid.NewString()
	// 单交互客户端注册：新连接替换旧连接（4009）。替换提交进 per-task 协调锁
	//（D5 原子边界：与投递准入/finalize 临界区线性化；等待 4009 关闭握手不持锁）。
	// B4：替换方经 oldGuard.commitReplace 赢得关闭所有权后向旧连接发 4009（等待写出
	// 或短超时），再 cancel 旧 bridge；故障先提交时由旧 bridge 完成 1011 收尾、替换方
	// 不另发 4009（wsCloseGuard）。旧 bridge 在被取消路径不发 1000（见 bridgeTerminal）。
	guard := &wsCloseGuard{}
	oldConn, oldGuard, oldCancel, bridgeCtx, err := s.registerTUIConn(r.Context(), taskID, c, guard, connID)
	if err != nil {
		// 替换提交无法进入协调锁（请求 ctx 已取消）：连接归属无法提交，1011 收尾。
		wsClose(context.Background(), c, wsCloseInternalError, "replace coordination failed")
		return
	}
	key := terminalKey(taskID, false)
	if oldConn != nil {
		if oldGuard.commitReplace() {
			wsCloseReplacedWait(oldConn)
		}
		oldCancel()
	}
	defer s.wsClients.unregister(key, c)

	_ = c.Write(r.Context(), websocket.MessageText, mustJSON(wsAuthResp{Type: "auth_ok", ConnID: connID}))
	// idle-reminder-user-activity D3：WS 输入帧即用户主动操作，上报 path 中的 taskID
	//（识别在 PTY 写入之前，不以写成功为前提）。
	s.bridgeTerminal(bridgeCtx, c, p, wsBridgeDeps{
		onInput: func() {
			s.tasks.RecordUserActivity(r.Context(), taskID)
		},
		transport: transport,
		guard:     guard,
		deliver: &wsDeliverScope{
			taskID: taskID,
			connID: connID,
		},
	})
}

// registerTUIConn 在 per-task 协调锁临界区内提交 TUI 连接替换（delivery spec
// 「连接归属与投递准入」：连接替换提交与投递准入/finalize MUST 在同一把锁内线性化，
// 线性化点 = 锁内注册表更新完成的时刻；等待 4009 关闭握手 MUST NOT 持有该锁，
// 4009/cancel 由调用方在锁释放后进行）。生产 TUI 注册唯一入口——通用
// wsClientRegistry.register 的其余调用方仅限 shell 连接与测试（见其注释）。
// 协调锁获取失败（请求 ctx 已取消）返回 err，调用方按 1011 收尾。shell 连接
// 不参与投递归属，不取锁（直接 register）。
func (s *Server) registerTUIConn(ctx context.Context, taskID string, c *websocket.Conn, guard *wsCloseGuard, connID string) (*websocket.Conn, *wsCloseGuard, context.CancelFunc, context.Context, error) {
	release, err := s.replaceCoord.Acquire(ctx, taskID)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	oldConn, oldGuard, oldCancel, bridgeCtx := s.wsClients.register(terminalKey(taskID, false), c, guard, connID)
	release()
	return oldConn, oldGuard, oldCancel, bridgeCtx, nil
}

// handleWSShell 处理 /ws/terminal/shell/:tid（shell 终端）。
// shell 连接同样签发 connId；deliver 帧经投递编排第 1 步（终端类型/连接当前性）拒绝
//（forbidden，terminal-file-paste-drop D5）。
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
	c, transport, err := acceptWS(w, r)
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

	connID := uuid.NewString()
	// 单交互客户端注册：新连接替换旧连接（4009）。仲裁规则同 handleWSTUI。
	key := terminalKey(tid, true)
	guard := &wsCloseGuard{}
	oldConn, oldGuard, oldCancel, bridgeCtx := s.wsClients.register(key, c, guard, connID)
	if oldConn != nil {
		if oldGuard.commitReplace() {
			wsCloseReplacedWait(oldConn)
		}
		oldCancel()
	}
	defer s.wsClients.unregister(key, c)

	_ = c.Write(r.Context(), websocket.MessageText, mustJSON(wsAuthResp{Type: "auth_ok", ConnID: connID}))
	// idle-reminder-user-activity D3：shell 输入帧上报 tid，由 task 层经
	// taskIDFromSessionName 解析归属任务（解析失败静默忽略）。
	s.bridgeTerminal(bridgeCtx, c, p, wsBridgeDeps{
		onInput: func() {
			s.tasks.RecordShellUserActivity(r.Context(), tid)
		},
		transport: transport,
		guard:     guard,
		deliver: &wsDeliverScope{
			taskID: tid,
			connID: connID,
		},
	})
}

// mustJSON 序列化为 JSON，失败返回空（仅用于内部固定响应）。
func mustJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}

// wsOutFrame 共享有界写队列的出站帧（PTY 输出为 binary，deliver_result 为 text）。
type wsOutFrame struct {
	typ  websocket.MessageType
	data []byte
}

// wsDeliverScope 单连接投递处理上下文（deliver 帧仅在它到达的那条 WS 连接的 pump 读
// 循环闭包内处理，D2/D5）。TUI 与 shell 共用；shell 连接的 connId 非该任务 TUI 当前
// 连接，经编排第 1 步拒绝（forbidden）。
type wsDeliverScope struct {
	taskID string
	connID string
}

// wsBridgeDeps bridgeTerminal 的连接级依赖（handler 构造，测试直接构造）。
type wsBridgeDeps struct {
	// onInput 用户输入观察回调（idle-reminder 上报，可 nil）。
	onInput func()
	// transport 底层 transport 句柄（收尾预算耗尽时强制关闭，必填）。
	transport *wsTransportHandle
	// guard 关闭原因仲裁（唯一关闭所有者，必填）。
	guard *wsCloseGuard
	// deliver 投递处理上下文；nil = 本连接无投递处理（未识别的 deliver 帧按丢弃+日志）。
	deliver *wsDeliverScope
}

// bridgeTerminal 桥接 WS 与 PTY（design.md §7 + terminal-file-paste-drop D2/D3）：
//   - PTY→WS 与 deliver_result 控制帧共用 bridge 级有界写队列（D2），独立写 goroutine
//     串行写出，队列满（慢客户端）即断开（B10）；
//   - WS→PTY：二进制帧写入 PTY，文本控制帧显式类型分发（resize/deliver，不回落写 PTY）；
//   - 双向取消：任一方向退出即取消另一方向（Wait 不永久挂）；
//   - 收尾（finishBridge）：排空回执 → 关闭码仲裁 → 独立 1s 预算内完成关闭握手与
//     transport 强制关闭。
//
// ctx 为 bridge 的 replaceCtx：被新连接替换（register 返回的 cancel）或 HTTP 请求结束
// 时会被取消。B4：ctx 取消路径 MUST NOT 抢先发 1000——被替换时关闭帧由替换方（4009，
// 经 guard 仲裁）负责，正常结束（PTY/WS EOF，ctx 未被取消）才发 1000。
func (s *Server) bridgeTerminal(ctx context.Context, c *websocket.Conn, p *pty.Pty, deps wsBridgeDeps) {
	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// bridge 级共享有界写队列（D2：现 pumpPTYToWS 内部队列上提）。writer 的写 ctx
	// 独立于 loopCtx——PTY 后备终止产生的 EOF MUST NOT 抢先取消已入队回执（bridge
	// 收尾表）；writer 只在收尾阶段被取消/强关。
	queue := newWSOutQueue()
	wctx, wcancel := context.WithCancel(context.Background())
	defer wcancel()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for frame := range queue {
			if err := c.Write(wctx, frame.typ, frame.data); err != nil {
				return
			}
		}
	}()

	var faultOwned atomic.Bool // 故障先提交（bridge 拥有 1011 收尾）
	var deliverFn func(uploadID string) bool
	if deps.deliver != nil {
		deliverFn = s.deliverFrameHandler(loopCtx, queue, p, deps.deliver, deps.guard, &faultOwned)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// PTY → WS（有界写队列投递；EOF/队列满返回即取消 WS→PTY 方向）。
	go func() {
		defer wg.Done()
		defer cancel()
		pumpPTYToWS(loopCtx, p, queue)
	}()

	// WS → PTY（二进制 + 显式类型分发控制帧）。
	go func() {
		defer wg.Done()
		defer cancel()
		pumpWSToPTY(loopCtx, c, p, queue, deliverFn, deps.onInput)
	}()

	wg.Wait()
	s.finishBridge(ctx, c, deps, queue, writerDone, wcancel, faultOwned.Load())
}

// newWSOutQueue bridge 级共享有界写队列构造（var 为故障注入测试的可控边界，与
// deliverPTYWrite 同型：注入「队列已满且 writer 阻塞」构造仅 deliver 回执入队失败的
// 慢客户端断开场景；生产恒为 wsWriteQueueCap 容量新队列）。
var newWSOutQueue = func() chan wsOutFrame {
	return make(chan wsOutFrame, wsWriteQueueCap)
}

// wsFinishTimeout 生产默认 writer/连接收尾预算（bridge 收尾两阶段第二阶段：释放协调
// 锁后独立 1s，与 5s 写期限互不借用；覆盖回执写出等待、关闭握手、transport 强制关闭
// 及本阶段退出）。s.wsFinishBudget 零值时使用；测试注入短值，不改变生产语义。
const wsFinishTimeout = time.Second

// finishBridge bridge 收尾（delivery spec bridge 收尾表 + 预算冻结）：
// ① 排空共享写队列（PTY EOF/读循环退出 MUST NOT 抢取消已入队回执）；
// ② 关闭码仲裁（faultOwned=故障先提交 1011；ctx 取消=替换方拥有关闭不发帧；否则正常 1000）；
// ③ 预算耗尽经 transport 句柄强制关闭底层连接，不依赖对端关闭帧。
func (s *Server) finishBridge(bridgeCtx context.Context, c *websocket.Conn, deps wsBridgeDeps, queue chan wsOutFrame, writerDone <-chan struct{}, wcancel context.CancelFunc, faultOwned bool) {
	budget := s.wsFinishBudget
	if budget <= 0 {
		budget = wsFinishTimeout
	}
	finishAt := time.Now().Add(budget)

	// ① 排空回执：入队方（双向 pump）均已退出，安全关闭队列。
	close(queue)
	select {
	case <-writerDone:
	case <-time.After(time.Until(finishAt)):
		wcancel() // 解除慢客户端 c.Write 阻塞，预算不无限等待
	}

	// ② 关闭码仲裁 + 有界关闭握手（code 0 = 不发关闭帧）。
	var code int
	switch {
	case faultOwned:
		code = wsCloseInternalError // 故障先提交：bridge 完成 1011 收尾
	case bridgeCtx.Err() != nil:
		// 被替换/请求取消：不发 1000（B4），4009 由替换方发出。
	default:
		code = wsCloseNormal // 正常结束（既有语义）
	}
	if code != 0 {
		closeDone := make(chan struct{})
		go func() {
			_ = c.Close(websocket.StatusCode(code), "")
			close(closeDone)
		}()
		select {
		case <-closeDone:
		case <-time.After(time.Until(finishAt)):
			// 预算耗尽（如对端不回关闭帧）：不再等待，直接强关底层连接。
		}
	}
	// ③ 收尾退出保证：强制关闭底层 transport（独立于 Conn.Close 的适配边界），
	// 并有界等待 writer 退出（transport 关闭后在途写立即失败）。
	deps.transport.forceClose()
	select {
	case <-writerDone:
	case <-time.After(time.Until(finishAt)):
	}
}

// deliverFrameHandler 构造 deliver 帧处理闭包（D2：deliver 帧在到达连接的 pump 读循环
// 闭包内处理）。返回 false 表示停止该连接继续处理输入并进入有界收尾：write_failed/
// 基础设施故障（bridge 收尾表「非超时写错误/短写」行），或回执入队失败（慢客户端，
// B10 统一断开）。
func (s *Server) deliverFrameHandler(connCtx context.Context, queue chan<- wsOutFrame, p *pty.Pty, scope *wsDeliverScope, guard *wsCloseGuard, faultOwned *atomic.Bool) func(string) bool {
	return func(uploadID string) bool {
		if s.deliverOrch == nil {
			// 编排未注入（组合根装配缺失）：投递无法执行，按基础设施故障收尾
			//（与 handler 层 tasks 未注入的处理模式一致）。
			log.Printf("ws: deliver %s infra failure (delivery orchestrator not configured), closing 1011", uploadID)
			faultOwned.Store(guard.commitFault())
			return false
		}
		inject := func(ctx context.Context) error {
			return s.injectDeliverPaste(ctx, p)
		}
		ok, code := s.deliverOrch.Deliver(connCtx, scope.taskID, scope.connID, uploadID, inject)
		if ok {
			if s.enqueueWSCtrl(queue, wsDeliverResult{Type: "deliver_result", UploadID: uploadID, OK: true}) {
				return true
			}
			// 回执无法入队：慢客户端，停止输入泵进入有界收尾（B10 统一断开语义，
			// 非故障收尾——关闭码走既有仲裁）。
			log.Printf("ws: deliver %s ok receipt dropped (write queue full), closing slow client", uploadID)
			return false
		}
		if !deliverResultErrorCodes[code] {
			// 基础设施故障（D5）：MUST NOT 伪造业务回执，1011 故障收尾；
			// 前端按断线进入"结果未知"。
			log.Printf("ws: deliver %s infra failure (code %q), closing 1011", uploadID, code)
			faultOwned.Store(guard.commitFault())
			return false
		}
		// 先生成并入队回执（bridge 收尾表：写队列满时不保证回执到达）。
		queued := s.enqueueWSCtrl(queue, wsDeliverResult{Type: "deliver_result", UploadID: uploadID, OK: false, Error: code})
		if code == "write_failed" {
			// 写失败：停止该连接继续处理输入，故障收尾（关闭码经仲裁，先提交者为准）。
			faultOwned.Store(guard.commitFault())
			return false
		}
		if !queued {
			// 准入拒绝但回执无法入队：慢客户端，停止输入泵进入有界收尾。
			log.Printf("ws: deliver %s %s receipt dropped (write queue full), closing slow client", uploadID, code)
			return false
		}
		return true
	}
}

// enqueueWSCtrl 控制帧经共享有界写队列回发，返回是否入队。队列满（慢客户端）返回
// false：回执不保证到达（bridge 收尾表），调用方停止该连接继续处理输入并进入有界
// 收尾（统一慢客户端断开，B10）。
func (s *Server) enqueueWSCtrl(queue chan<- wsOutFrame, v interface{}) bool {
	frame := wsOutFrame{typ: websocket.MessageText, data: mustJSON(v)}
	select {
	case queue <- frame:
		return true
	default:
		log.Printf("ws: control frame dropped, write queue full")
		return false
	}
}

// injectDeliverPaste bracketed paste 注入单一函数边界（design D3）：向 PTY 写入
// ESC[200~+最终绝对路径+ESC[201~ 单个完整消息（无 @、无回车），与普通输入共用串行写
// 路径（pump goroutine）。仅完整写入（n == len && err == nil）返回 nil；超时/取消/
// 立即错误/短写统一返回 error（编排层映射 write_failed，不补写）。
func (s *Server) injectDeliverPaste(ctx context.Context, p *pty.Pty) error {
	// 编排层在 per-task 协调锁临界区内解析的最终绝对路径（appuploads.Deliver 步骤⑦
	// 经 ContextWithInjectPath 包入，Lane A 权威 helper）。
	path, ok := appuploads.InjectPathFrom(ctx)
	if !ok {
		// 编排层未提供注入载荷：无法构造 bracketed paste，按写失败处理。
		return errors.New("deliver: inject path missing in ctx")
	}
	deadline := s.deliverWriteDeadline
	if deadline <= 0 {
		deadline = deliverWriteDeadline
	}
	data := make([]byte, 0, len(bracketedPasteStart)+len(path)+len(bracketedPasteEnd))
	data = append(data, bracketedPasteStart...)
	data = append(data, path...)
	data = append(data, bracketedPasteEnd...)
	n, err := deliverPTYWrite(p, ctx, data, deadline)
	if err == nil && n == len(data) {
		return nil
	}
	return fmt.Errorf("deliver: bracketed paste incomplete write (%d/%d bytes): %v", n, len(data), err)
}

// pumpPTYToWS 从 PTY 读取并投递到 bridge 级共享有界写队列。队列满即认为客户端消费
// 不及，返回触发断开（B10：慢客户端断开）。PTY 读结束（EOF/ctx 取消）也返回——
// 已入队帧（含 deliver_result 回执）由 bridge 收尾阶段排空写出，不被本方向退出取消。
func pumpPTYToWS(ctx context.Context, p *pty.Pty, queue chan<- wsOutFrame) {
	for {
		data, err := p.ReadCtx(ctx)
		if err != nil {
			return
		}
		select {
		case queue <- wsOutFrame{typ: websocket.MessageBinary, data: data}:
		default:
			// 队列满：慢客户端，断开。
			return
		}
	}
}

// pumpWSToPTY 从 WS 读取并写入 PTY。二进制帧写入 PTY；文本控制帧显式类型分发
//（D2，不再回落写 PTY）：
//   - 非法 JSON / 未知 type：丢弃 + 日志，不回帧、不调用 onInput；
//   - type=resize：沿用既有尺寸处理，不校验 uploadId；
//   - type=deliver：uploadId MUST 为 32 hex（不合法丢弃+日志），合法帧视为用户活动
//     （onInput 在 PTY 写入前）后经 deliverFn 进入投递编排（per-task 协调锁临界区内
//     准入+注入）。deliverFn 返回 false（write_failed/基础设施故障/回执入队失败的
//     慢客户端断开）即停止输入处理。
//
// ctx 取消或 WS 读错误即退出。
func pumpWSToPTY(ctx context.Context, c *websocket.Conn, p *pty.Pty, queue chan<- wsOutFrame, deliverFn func(uploadID string) bool, onInput func()) {
	for {
		typ, payload, err := c.Read(ctx)
		if err != nil {
			return
		}
		switch typ {
		case websocket.MessageBinary:
			if len(payload) > 0 && onInput != nil {
				onInput()
			}
			if _, err := p.Write(payload); err != nil {
				return
			}
		case websocket.MessageText:
			var probe wsCtrlFrame
			if err := json.Unmarshal(payload, &probe); err != nil {
				log.Printf("ws: drop malformed text frame (%d bytes): %v", len(payload), err)
				continue
			}
			switch probe.Type {
			case "resize":
				_ = p.Resize(probe.Cols, probe.Rows)
			case "deliver":
				var d wsDeliverFrame
				if err := json.Unmarshal(payload, &d); err != nil || !isUploadID(d.UploadID) {
					log.Printf("ws: drop deliver frame with invalid uploadId (%d bytes)", len(payload))
					continue
				}
				if deliverFn == nil {
					log.Printf("ws: drop deliver frame, delivery not available on this connection")
					continue
				}
				if onInput != nil {
					onInput() // 合法 deliver = 用户活动（PTY 写入前，D2）
				}
				if !deliverFn(d.UploadID) {
					return // 写失败/基础设施故障：停止该连接继续处理输入
				}
			default:
				log.Printf("ws: drop unknown text frame type %q", probe.Type)
			}
		}
	}
}
