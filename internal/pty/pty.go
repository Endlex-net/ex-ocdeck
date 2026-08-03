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
func (p *Pty) Close() error {
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
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(os.Signal(syscall.SIGTERM))
	}
	// 关闭 ptmx 触发子进程 SIGHUP（attach 客户端退出 = detach），
	// 同时让 readLoop 的阻塞 Read 返回错误退出。
	_ = p.ptmx.Close()
	// 等待读循环退出。
	<-p.readerDone
	// 等待子进程回收，避免僵尸；超时则 Kill 防止永久阻塞。
	if p.cmd.Process != nil {
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