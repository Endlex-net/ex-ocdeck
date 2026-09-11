// Package pty 实现 attach 客户端 PTY 池与 16ms 批量刷新（design.md §7/§18）。
//
// PTY 本体运行 `tmux -L ocdeck attach -t <session>`，仅作渲染客户端——
// 断开 WS/杀 PTY 只 detach 客户端，tmux 会话与任务进程不受影响。
// 16ms 批量合并 PTY 输出，削峰 WS 推送。
package pty

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// flushInterval 是 PTY 输出批量合并窗口（design.md §7：≤16ms）。
// 借鉴 emdash pty-session-registry.ts:43-64（buffer 累积 + setTimeout flush）。
const flushInterval = 16 * time.Millisecond

// readBufSize 单次 PTY 读循环的缓冲上限。PTY 输出量通常远小于此，
// 但留出足够空间避免高频小包读循环开销。
const readBufSize = 32 * 1024

// bufferCap PTY 累积 buffer 的上限（溢出策略：丢弃最旧并标记 overflow）。
// 防止慢消费客户端导致 buffer 无限增长。16MB 与 process 包一致量级。
const bufferCap = 16 * 1024 * 1024

// closeTimeout Close 等待子进程退出的超时（design.md §18：close master 后子进程
// 未退出则 Kill，不得永久阻塞）。
const closeTimeout = 5 * time.Second

// Pty 是单个 attach 客户端 PTY 句柄。
//
// 生命周期：Open 启动子进程 + 读循环 goroutine → Read 取合并后的输出 →
// Write 写入子进程 stdin → Resize 调整 PTY 尺寸（tmux 客户端自动传播到会话）→
// Close 终止子进程并释放资源。
//
// 并发安全：Read/Write/Resize/Close 可并发调用；Read 返回的数据是 16ms 窗口内
// 累积的合并输出，多次 Read 之间不重叠。
type Pty struct {
	cmd    *exec.Cmd
	ptmx   *os.File
	cancel context.CancelFunc

	// flushMu 保护 buffer 与 closed 与 overflow。readLoop 写 buffer，Read 消费 buffer。
	flushMu sync.Mutex
	buffer  *ringBuffer
	closed  bool

	// ioMu 写入/关闭生命周期协调：WriteTimeout 全程持有（底层写、deadline 清理、
	// 后备终止在同一临界区内完成，互不穿插）；Close 先获锁再关闭 ptmx/回收进程。
	// 由此建立顺序：Close 先发生则后续 WriteTimeout 立即失败；WriteTimeout 进行中
	// Close 有界等待（≤ 其写期限）后再关闭。
	ioMu sync.Mutex

	// flushCh 由读循环写入与 ticker 共同触发，通知 Read 可消费 buffer。
	// 只在读循环实际写入数据、或 ticker 到期且 buffer 非空时才发信号，
	// 保证空 buffer 的 ticker 唤醒不会误报 io.EOF（安静终端每 16ms 不被误判关闭）。
	// buffered(1) 使发信号非阻塞，避免丢 tick 时调用方卡死。
	flushCh chan struct{}

	// readerDone 在 readLoop 退出时关闭；readerErr 携带底层读错误（nil=EOF）。
	readerDone chan struct{}
	readerErr  error
}

// Open 启动 cmd（argv 数组，由调用方构造——不在此处做 shell 拼接），
// cwd 为工作目录（空表示继承），env 为子进程环境（nil 表示继承父进程）。
// cols/rows 为初始 PTY 尺寸（兑现 WS 首帧尺寸创建 PTY 契约，design.md §7）；
// ≤0 时默认 80×24。
//
// cmd 必须是已配置好 argv 的 exec.Cmd（不含 Stdin/Stdout/Stdout 设置——
// 本函数接管）。返回的 *Pty 调用方 MUST Close 释放子进程与 PTY fd。
func Open(cmd *exec.Cmd, cwd string, env []string, cols, rows int) (*Pty, error) {
	if cmd == nil {
		return nil, errors.New("pty: cmd is nil")
	}
	if cwd != "" {
		cmd.Dir = cwd
	}
	if env != nil {
		cmd.Env = env
	}
	if cols <= 0 || rows <= 0 {
		cols, rows = 80, 24
	}

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("pty: start %v: %w", cmd.Args, err)
	}
	// 首帧尺寸：在 PTY 创建后立即设置，兑现 WS 首帧 resize 契约。
	_ = pty.Setsize(ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})

	ctx, cancel := context.WithCancel(context.Background())
	p := &Pty{
		cmd:        cmd,
		ptmx:       ptmx,
		cancel:     cancel,
		buffer:     newRingBuffer(bufferCap),
		flushCh:    make(chan struct{}, 1),
		readerDone: make(chan struct{}),
	}
	go p.readLoop(ctx)
	go p.flushTickerLoop(ctx)
	return p, nil
}

// readLoop 持续从 PTY master fd 读取输出，累积到 buffer。
// ctx 取消或 ptmx 读错时退出；退出时强制 flush 信号确保 Read 能取走剩余数据。
//
// 重要：每次底层 Read 写入 buffer 后 NOT 立即 signalFlush——仅标记 "dirty"，
// 由 flushTickerLoop 每 16ms 唤醒一次 Read 消费，形成真正的 16ms 批量窗口
//（design.md §7）。若直接 signalFlush，每次 Read 即触发一次 Read 消费，
// 批量窗口退化为 0ms（pty.go 旧行为）。
func (p *Pty) readLoop(ctx context.Context) {
	defer func() {
		// 退出时标记 EOF 并发 flush 信号，确保 Read 能取走剩余数据。
		p.flushMu.Lock()
		p.readerErr = io.EOF
		p.flushMu.Unlock()
		p.signalFlush()
		close(p.readerDone)
	}()

	buf := make([]byte, readBufSize)
	for {
		// ctx 取消时通过关闭 ptmx（由 Close 负责）触发 read 返回错误退出，
		// 这里 select 仅作快速路径探测，真正退出依赖 ptmx 读错。
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := p.ptmx.Read(buf)
		if n > 0 {
			p.flushMu.Lock()
			p.buffer.Write(buf[:n])
			p.flushMu.Unlock()
			// 不立即 signalFlush：交给 flushTickerLoop 按 16ms 窗口批量合并。
		}
		if err != nil {
			p.flushMu.Lock()
			if !errors.Is(err, io.EOF) && p.readerErr == nil {
				// 非 EOF 的底层读错误记录，供 Read 返回。
				p.readerErr = err
			} else if p.readerErr == nil {
				p.readerErr = io.EOF
			}
			p.flushMu.Unlock()
			return
		}
	}
}

// flushTickerLoop 每 16ms 触发一次 flush 信号，实现批量合并窗口。
// 仅当 buffer 有数据或读循环已退出时才发信号——避免空 buffer 的 ticker 唤醒
// 误报 io.EOF（pty.go 旧实现安静终端每 16ms 被误判关闭）。
// ctx 取消即退出。
func (p *Pty) flushTickerLoop(ctx context.Context) {
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.flushMu.Lock()
			hasData := p.buffer.Len() > 0
			readerDone := p.readerErr != nil
			p.flushMu.Unlock()
			if hasData || readerDone {
				p.signalFlush()
			}
		}
	}
}

// signalFlush 非阻塞发送 flush 信号。buffered chan(1) 保证不阻塞，
// 已有 pending 信号时跳过（合并多次 tick/数据为一次 Read 唤醒）。
func (p *Pty) signalFlush() {
	select {
	case p.flushCh <- struct{}{}:
	default:
	}
}

// Read 返回自上次 Read 以来 16ms 窗口内累积的 PTY 输出。
// 阻塞直到 flush 信号到达或 PTY 关闭。返回 (data, nil) 有数据；
// (nil, io.EOF) 表示 PTY 已关闭；其他 error 表示底层读错误。
// 返回的数据切片是新分配的副本，调用方可持有。
//
// 关键：只有当读循环已退出（readerErr != nil）且 buffer 为空时才返回 EOF，
// 空 buffer 的 ticker 唤醒（buffer 为空、readerErr 为 nil）继续阻塞等待，
// 不误报 EOF。
//
// 不感知 ctx：调用方若需在 ctx 取消时退出（如 WS bridge），用 ReadCtx。
func (p *Pty) Read() ([]byte, error) {
	return p.read(context.Background())
}

// ReadCtx 同 Read，但额外感知 ctx 取消：ctx 取消时返回 (nil, ctx.Err())，
// 避免调用方 goroutine 在 PTY 无输出时永久阻塞（WS bridge ctx 取消后 MUST 退出，
// 不留泄漏 goroutine）。ctx 已取消时仍会先消费 buffer 内已累积的数据再返回错误，
// 不丢已读输出。
func (p *Pty) ReadCtx(ctx context.Context) ([]byte, error) {
	return p.read(ctx)
}

func (p *Pty) read(ctx context.Context) ([]byte, error) {
	for {
		// 等待 flush 信号、读循环退出或 ctx 取消。
		select {
		case <-p.flushCh:
		case <-p.readerDone:
		case <-ctx.Done():
			// ctx 取消：先消费 buffer 内已累积数据（不丢已读输出），再返回取消错误。
			p.flushMu.Lock()
			if p.buffer.Len() > 0 {
				out := make([]byte, p.buffer.Len())
				copy(out, p.buffer.Bytes())
				p.buffer.Reset()
				p.flushMu.Unlock()
				return out, nil
			}
			p.flushMu.Unlock()
			return nil, ctx.Err()
		}

		p.flushMu.Lock()
		if p.buffer.Len() > 0 {
			out := make([]byte, p.buffer.Len())
			copy(out, p.buffer.Bytes())
			p.buffer.Reset()
			p.flushMu.Unlock()
			return out, nil
		}
		// buffer 为空：判断读循环是否已退出。
		if p.readerErr != nil {
			err := p.readerErr
			p.flushMu.Unlock()
			if errors.Is(err, io.EOF) {
				return nil, io.EOF
			}
			return nil, err
		}
		p.flushMu.Unlock()
		// readerErr == nil 且 buffer 为空：spurious flush（ticker 在数据写入前到），
		// 继续阻塞等待下一次信号，不误报 EOF。
	}
}

// Write 向 PTY master 写入输入数据（转发到子进程 stdin）。
func (p *Pty) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	n, err := p.ptmx.Write(b)
	if err != nil {
		return n, fmt.Errorf("pty: write: %w", err)
	}
	return n, nil
}

// writeAbortGrace 原生写 deadline 相对统一绝对截止提前的量：为后备终止与底层写退出
// 确认在预算内预留的时间；也是取消路径等待压缩 deadline 生效使写自行退出的宽限上限。
const writeAbortGrace = 500 * time.Millisecond

// errWriteDeadline 统一绝对截止超期的终止原因（取消路径使用 ctx.Err()）。
var errWriteDeadline = errors.New("pty: write deadline exceeded")

// setPTMXWriteDeadline 原生写 deadline 设置入口（生产为 (*os.File).SetWriteDeadline）。
// 变量为注入「平台不支持原生写 deadline」的可控测试边界（与 api 层 deliverPTYWrite
// 同型）；生产路径恒为真实实现。
var setPTMXWriteDeadline = (*os.File).SetWriteDeadline

// WriteTimeout 向 PTY master 写入单个完整消息，整个调用（写尝试 + 必要时的后备终止
// 与退出确认）受 timeout 与 ctx 双重约束（terminal-file-paste-drop 写入退出契约）：
//
//   - 从调用起点取统一绝对截止时刻 deadlineAt（5s 写期限覆盖底层写退出、后备终止与
//     锁释放，不得超支）；写尝试的原生 deadline 设为 deadlineAt-writeAbortGrace，为
//     终止与退出确认预留预算；平台不支持原生 deadline（SetWriteDeadline 报错）时
//     静默忽略，依赖后备终止在预算内解除阻塞；
//   - deadlineAt 到期或 ctx 取消（连接取消）时写入 MUST 终止：先压缩原生 deadline
//     促使写退出，宽限窗口（不超过剩余预算）内仍未退出则后备终止——SIGKILL attach
//     客户端并关闭 ptmx 解除阻塞（任务本体 tmux 会话不随 attach 客户端终止）；
//   - 取消或超期后的底层结果不得冒充按期成功（未在期限内完整成功统一未完整成功）；
//   - 返回前 MUST 确认底层写已结束（MUST NOT 让调用方带着仍在写 PTY 的后台 goroutine
//     返回，破坏串行写语义/协调锁线性化）；
//   - 全程持有 ioMu（写入/关闭生命周期协调）：Close 与本调用串行，见 ioMu 注释。
//     所有返回路径都先消费底层写结果再返回，故写 goroutine 必然在 ioMu 释放前结束。
//
// 仅 n == len(b) && err == nil 为完整成功；超时/取消/立即错误/短写均为未完整成功，
// 由调用方统一分类（write_failed）。与普通 Write 共用 pump goroutine 串行调用。
func (p *Pty) WriteTimeout(ctx context.Context, b []byte, timeout time.Duration) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	if timeout <= 0 {
		return 0, errors.New("pty: write timeout must be positive")
	}

	p.ioMu.Lock()
	defer p.ioMu.Unlock()

	// Close 已先执行：写入生命周期已终止，立即失败返回，不再触碰 ptmx。
	p.flushMu.Lock()
	closed := p.closed
	p.flushMu.Unlock()
	if closed {
		return 0, errors.New("pty: write on closed pty")
	}

	type writeResult struct {
		n   int
		err error
	}
	result := make(chan writeResult, 1)
	writeDone := make(chan struct{})
	deadlineAt := time.Now().Add(timeout)
	writeDeadline := deadlineAt.Add(-writeAbortGrace)
	go func() {
		defer close(writeDone)
		// 平台不支持原生 deadline（SetWriteDeadline 报错）时静默忽略，依赖后备终止。
		_ = setPTMXWriteDeadline(p.ptmx, writeDeadline)
		n, err := p.ptmx.Write(b)
		_ = setPTMXWriteDeadline(p.ptmx, time.Time{})
		result <- writeResult{n: n, err: err}
	}()

	select {
	case r := <-result:
		return finishWriteResult(r.n, r.err)
	case <-ctx.Done():
		// 连接取消：压缩 deadline 使可 poll fd 上的写立刻退出。
		_ = setPTMXWriteDeadline(p.ptmx, time.Now())
	case <-time.After(time.Until(deadlineAt)):
	}

	// 走到此处即已超期或取消（完整结果分支已在 select 内返回）：后续到达的底层
	// 结果不得再按按期成功返回。
	cause := ctx.Err()
	if cause == nil {
		cause = errWriteDeadline
	}
	// 宽限窗口（不超过剩余预算）内等原生/压缩 deadline 生效使写退出，仍未退出才
	// 执行后备终止——超期路径剩余预算为 0，立即后备终止，不得超支。
	grace := writeAbortGrace
	if remaining := time.Until(deadlineAt); remaining < grace {
		grace = remaining
	}
	if grace > 0 {
		graceTimer := time.NewTimer(grace)
		defer graceTimer.Stop()
		select {
		case r := <-result:
			return terminateWriteResult(r.n, r.err, cause)
		case <-graceTimer.C:
		}
	}
	p.abortWrite()
	// 确认底层写已结束才返回（per-task 协调锁释放前提）。abort 后 SIGKILL + 关闭
	// ptmx 必然解除阻塞，此处等待有界。
	<-writeDone
	r := <-result
	return terminateWriteResult(r.n, r.err, cause)
}

// finishWriteResult 归一化底层写结果（错误带上下文）。
func finishWriteResult(n int, err error) (int, error) {
	if err != nil {
		return n, fmt.Errorf("pty: write: %w", err)
	}
	return n, nil
}

// terminateWriteResult 归一化终止路径（取消/超期/后备终止）的写结果：终止后到达的
// 底层结果不得冒充按期成功——err 为空时以终止原因代替（调用方统一分类 write_failed）。
func terminateWriteResult(n int, err, cause error) (int, error) {
	if err == nil {
		err = cause
	}
	return finishWriteResult(n, err)
}

// abortWrite 后备终止：SIGKILL attach 客户端子进程并关闭 ptmx，解除阻塞中的底层写。
// 仅终止 attach 客户端（等于 detach），任务本体 tmux 会话不受影响。
// 生产调用方为 WriteTimeout 终止路径（已持有 ioMu，与 Close 有定义的顺序）；
// 测试可直接调用。
func (p *Pty) abortWrite() {
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	_ = p.ptmx.Close()
}

// Resize 调整 PTY 窗口尺寸。tmux attach 客户端的 winsize 变化会自动
// 传播到会话窗口（design.md §7）。
func (p *Pty) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("pty: invalid size %dx%d", cols, rows)
	}
	if err := pty.Setsize(p.ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}); err != nil {
		return fmt.Errorf("pty: resize: %w", err)
	}
	return nil
}

// Close 终止子进程并释放 PTY fd。幂等。带超时（design.md §18：close master 后
// 子进程未退出则 Kill，不得永久阻塞）。
//
// 写入生命周期协调（ioMu）：与在途 WriteTimeout 串行——Close 先发生时后续
// WriteTimeout 立即失败返回；WriteTimeout 进行中 Close 有界等待（≤ 其写期限，
// 写自身按期限终止）后再执行关闭，与底层写、deadline 清理、abortWrite 之间
// 有定义的顺序，无并发关闭 ptmx / 回收进程的竞争。
func (p *Pty) Close() error {
	p.ioMu.Lock()
	defer p.ioMu.Unlock()

	p.flushMu.Lock()
	if p.closed {
		p.flushMu.Unlock()
		return nil
	}
	p.closed = true
	p.flushMu.Unlock()

	p.cancel()
	// 先终止子进程：子进程退出关闭 PTY slave 端，触发 master 端阻塞 Read
	// 返回 EOF，从而让 readLoop 退出。直接 Close ptmx 在子进程仍持有时
	// 不一定能唤醒阻塞 Read（平台差异），故先发 SIGHUP 再 Close。
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(os.Signal(syscall.SIGTERM))
	}
	// 关闭 ptmx 触发子进程 SIGHUP（attach 客户端退出 = detach），
	// 同时让 readLoop 的阻塞 Read 返回错误退出。
	_ = p.ptmx.Close()
	// 等待读循环退出。
	<-p.readerDone
	// 等待子进程回收，避免僵尸；超时则 Kill 防止永久阻塞。
	if p.cmd != nil && p.cmd.Process != nil {
		done := make(chan struct{})
		go func() {
			_ = p.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(closeTimeout):
			_ = p.cmd.Process.Kill()
			<-done
		}
	}
	return nil
}

// --- ringBuffer：带上限的环形 buffer，溢出丢弃最旧并标记 ---

// ringBuffer 封装 bytes.Buffer 并施加容量上限。溢出时丢弃最旧数据并记录 overflow。
type ringBuffer struct {
	buf      bytes.Buffer
	cap      int
	overflow bool
}

func newRingBuffer(capBytes int) *ringBuffer {
	return &ringBuffer{cap: capBytes}
}

func (b *ringBuffer) Write(p []byte) (int, error) {
	// 环形缓冲语义：溢出时丢弃最旧数据，保证保留最新 capacity 字节。
	// 直接计算保留窗口：若新数据本身 >= cap，只保留尾部 cap 字节；否则按需丢弃旧数据腾出空间。
	if len(p) >= b.cap {
		b.overflow = true
		b.buf.Reset()
		b.buf.Write(p[len(p)-b.cap:])
		return len(p), nil
	}
	over := b.buf.Len() + len(p) - b.cap
	if over > 0 {
		b.overflow = true
		b.discardOldest(over)
	}
	b.buf.Write(p)
	return len(p), nil
}

// discardOldest 丢弃 n 字节最旧数据（从头丢弃）。
func (b *ringBuffer) discardOldest(n int) {
	if n <= 0 {
		return
	}
	if n >= b.buf.Len() {
		b.buf.Reset()
		return
	}
	tail := append([]byte(nil), b.buf.Bytes()[n:]...)
	b.buf.Reset()
	b.buf.Write(tail)
}

func (b *ringBuffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *ringBuffer) Len() int      { return b.buf.Len() }
func (b *ringBuffer) Reset()        { b.buf.Reset(); b.overflow = false }
func (b *ringBuffer) Overflow() bool { return b.overflow }