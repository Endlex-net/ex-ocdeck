package pty

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

// blockedPayload 单次写入超过 os.Pipe 缓冲（Linux 64KB / darwin 16-64KB）的载荷：
// 底层写接受部分字节后阻塞（消费端不存在），确定性地复现「PTY 不消费写阻塞」，
// 与 goroutine 调度时序无关。
var blockedPayload = make([]byte, 128*1024)

// newPipeWritePty 构造 ptmx 为 os.Pipe 写端的 Pty：无读端时写入在缓冲填满后阻塞。
// os.Pipe 是可 poll 的 *os.File——原生写 deadline 生效；关闭读端解除阻塞（写返回
// broken pipe），等价于「终止 attach 客户端并关闭其 PTY」的后备终止解除路径。
// darwin 的真实 PTY 输入队列满时丢弃而非流控阻塞，无法构造确定性阻塞，故用管道。
// 读方向不存在（无 readLoop）：readerDone 预关闭、cancel/cmd 就绪，使 Close 可用。
func newPipeWritePty(t *testing.T) (*Pty, *os.File) {
	t.Helper()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})
	_, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	readerDone := make(chan struct{})
	close(readerDone)
	return &Pty{
		ptmx:       pw,
		cancel:     cancel,
		cmd:        exec.Command("cat"), // 未 Start：Process 为 nil，Close/abortWrite 跳过进程回收
		readerDone: readerDone,
	}, pr
}

// TestPtyWriteTimeout_CompletesNormally 验证正常完整写入：n == len && err == nil，
// 且 PTY 仍可用（写入退出未终止 attach 客户端）。
func TestPtyWriteTimeout_CompletesNormally(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping pty integration test in -short mode")
	}
	cmd := exec.Command("cat")
	p, err := Open(cmd, "", []string{"TERM=xterm"}, 120, 40)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	n, err := p.WriteTimeout(context.Background(), []byte("hello\n"), 2*time.Second)
	if err != nil || n != len("hello\n") {
		t.Fatalf("WriteTimeout = (%d, %v), want complete write", n, err)
	}
	// PTY 存活：后续写入仍成功（写退出路径不得误杀 attach 客户端）。
	if _, err := p.Write([]byte("again\n")); err != nil {
		t.Fatalf("PTY unusable after WriteTimeout: %v", err)
	}
}

// TestPtyWriteTimeout_BlockedWriteBoundedExit（「PTY 不消费写阻塞」infrastructure 层）：
// 底层写阻塞（消费端不存在）时 WriteTimeout 在 timeout + 宽限内有界返回（底层写已
// 确认结束、协调锁可释放），且结果为未完整成功（无错误短写或 deadline 错误）。
func TestPtyWriteTimeout_BlockedWriteBoundedExit(t *testing.T) {
	p, pr := newPipeWritePty(t)
	t.Cleanup(func() { _ = pr.Close() }) // 后备解除：关闭底层 PTY 读端解除阻塞

	const timeout = 300 * time.Millisecond
	start := time.Now()
	n, err := p.WriteTimeout(context.Background(), blockedPayload, timeout)
	elapsed := time.Since(start)

	// 有界退出：写尝试 timeout + 原生 deadline 宽限 + 后备终止，远小于 2s
	//（对应 5s 写期限覆盖底层写退出与锁释放的验收口径，测试按比例缩短）。
	if elapsed > 2*time.Second {
		t.Fatalf("WriteTimeout returned after %v, want bounded exit (~%v)", elapsed, timeout)
	}
	if err == nil && n == len(blockedPayload) {
		t.Fatalf("blocked write reported complete (%d, %v), want incomplete", n, err)
	}
}

// TestPtyWriteTimeout_CtxCancelTerminatesWrite（连接取消）：写入阻塞期间 ctx 取消，
// 写入终止且有界返回（不留下后台写入继续占用协调锁）。
func TestPtyWriteTimeout_CtxCancelTerminatesWrite(t *testing.T) {
	p, pr := newPipeWritePty(t)
	t.Cleanup(func() { _ = pr.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	n, err := p.WriteTimeout(ctx, blockedPayload, 30*time.Second)
	elapsed := time.Since(start)

	// ctx 取消后立即压缩 deadline / 后备终止，写入终止（错误或短写），不等到 30s。
	if elapsed > 2*time.Second {
		t.Fatalf("WriteTimeout returned after %v after ctx cancel, want prompt termination", elapsed)
	}
	if err == nil && n == len(blockedPayload) {
		t.Fatalf("canceled write reported complete (%d, %v), want incomplete", n, err)
	}
}

// TestPtyWriteTimeout_AbortTerminatesAttachClient（后备终止语义）：abortWrite 终止
// attach 客户端并关闭 PTY 解除阻塞；读方向随之 EOF（bridge 据此进入故障收尾）。
func TestPtyWriteTimeout_AbortTerminatesAttachClient(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping pty integration test in -short mode")
	}
	p, err := Open(exec.Command("sleep", "30"), "", []string{"TERM=xterm"}, 80, 24)
	if err != nil {
		t.Fatalf("pty.Open sleep: %v", err)
	}
	defer p.Close()

	p.abortWrite()
	// 子进程被终止（attach 客户端退出 = detach；任务本体 tmux 会话不随其终止）。
	dead := make(chan error, 1)
	go func() { dead <- p.cmd.Wait() }()
	select {
	case <-dead:
	case <-time.After(3 * time.Second):
		t.Fatal("attach client process not reaped after abortWrite")
	}
	// 读方向 EOF：readLoop 随 ptmx 关闭退出后 Read 返回 EOF。
	readCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		_, err := p.ReadCtx(readCtx)
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("Read after abort: got %v, want io.EOF", err)
		}
	}
}

// stubSetWriteDeadline 注入「平台不支持原生写 deadline」故障（可控测试边界：生产为
// (*os.File).SetWriteDeadline，os.Pipe 支持原生 deadline，无法自然复现不支持平台）。
func stubSetWriteDeadline(t *testing.T, ferr error) {
	t.Helper()
	old := setPTMXWriteDeadline
	setPTMXWriteDeadline = func(*os.File, time.Time) error { return ferr }
	t.Cleanup(func() { setPTMXWriteDeadline = old })
}

// TestPtyWriteTimeout_NoNativeDeadlineBoundedFallback（「平台不支持原生 deadline」
// 后备路径）：写阻塞 + SetWriteDeadline 注入失败时，仍从调用起点按统一绝对截止预算
// 执行——到期即后备终止（不得超期后再借 writeAbortGrace 等待），返回前确认底层写
// 已结束（ptmx 已被后备关闭），协调锁可释放（WriteTimeout 已返回）。
func TestPtyWriteTimeout_NoNativeDeadlineBoundedFallback(t *testing.T) {
	p, pr := newPipeWritePty(t)
	t.Cleanup(func() { _ = pr.Close() })
	stubSetWriteDeadline(t, errors.New("setWriteDeadline: platform does not support deadlines"))

	const timeout = 300 * time.Millisecond
	start := time.Now()
	n, err := p.WriteTimeout(context.Background(), blockedPayload, timeout)
	elapsed := time.Since(start)

	// 绝对截止预算内退出：后备终止不得在超期后额外等待宽限（旧实现至少
	// timeout+writeAbortGrace 才开始终止，生产即 5.5s 超出 5s 写期限）。
	if elapsed >= timeout+writeAbortGrace {
		t.Fatalf("WriteTimeout without native deadline returned after %v, want within absolute deadline budget (< %v)", elapsed, timeout+writeAbortGrace)
	}
	if err == nil && n == len(blockedPayload) {
		t.Fatalf("blocked write reported complete (%d, %v), want incomplete", n, err)
	}
	// 底层写确认结束：后备终止已关闭 ptmx，后续写必败（不存在仍在写 PTY 的后台写入）。
	if _, werr := p.Write([]byte("x")); werr == nil {
		t.Error("ptmx must be closed by fallback abort (underlying write confirmed ended)")
	}
}

// TestPtyWriteTimeout_NoNativeDeadlineCtxCancelTerminates（原生 deadline 不可用 +
// 连接取消）：宽限窗口内压缩 deadline 无法生效即后备终止解除阻塞，写入终止且返回前
// 底层写确认结束（协调锁可释放），不等到 timeout。
func TestPtyWriteTimeout_NoNativeDeadlineCtxCancelTerminates(t *testing.T) {
	p, pr := newPipeWritePty(t)
	t.Cleanup(func() { _ = pr.Close() })
	stubSetWriteDeadline(t, errors.New("setWriteDeadline: platform does not support deadlines"))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	n, err := p.WriteTimeout(ctx, blockedPayload, 30*time.Second)
	elapsed := time.Since(start)

	// 取消 → 宽限窗口（≤writeAbortGrace）内写未退出即后备终止：远小于 30s timeout。
	if elapsed > 2*time.Second {
		t.Fatalf("WriteTimeout returned after %v after ctx cancel, want prompt termination", elapsed)
	}
	if err == nil && n == len(blockedPayload) {
		t.Fatalf("canceled write reported complete (%d, %v), want incomplete", n, err)
	}
	if _, werr := p.Write([]byte("x")); werr == nil {
		t.Error("ptmx must be closed by fallback abort after cancel (underlying write confirmed ended)")
	}
}

// --- R15：WriteTimeout 写 goroutine 与 Close 的生命周期协调（ioMu） ---

// waitWriteLockHeld 轮询等待 WriteTimeout 已进入 ioMu 临界区（TryLock 失败即锁被其
// 持有）：确定性屏障，证明后续 Close 与取消都发生在写阻塞期间，非 sleep 猜运气。
func waitWriteLockHeld(t *testing.T, p *Pty) {
	t.Helper()
	for i := 0; i < 2000; i++ {
		if !p.ioMu.TryLock() {
			return
		}
		p.ioMu.Unlock()
		time.Sleep(500 * time.Microsecond)
	}
	t.Fatal("WriteTimeout never entered ioMu critical section")
}

// TestPtyWriteTimeout_CloseWaitsForInFlightWrite（写阻塞时 Close 并发）：WriteTimeout
// 阻塞于底层写期间调用 Close——Close 有界等待在途写按写期限终止后才执行关闭
//（Close 返回蕴含写已结束，ioMu happens-before；且等待时长为写期限量级，证明是
// 等待而非并发抢关）；关闭后再 WriteTimeout 立即失败。
func TestPtyWriteTimeout_CloseWaitsForInFlightWrite(t *testing.T) {
	p, pr := newPipeWritePty(t)
	t.Cleanup(func() { _ = pr.Close() })

	type wres struct {
		n   int
		err error
	}
	res := make(chan wres, 1)
	// timeout=800ms：原生 deadline = timeout-writeAbortGrace = 300ms，写真实阻塞
	// 300ms 后按 deadline 终止（timeout < grace 时 deadline 落在过去、写立即失败，
	// 无法构造在途阻塞）。
	const effectiveDeadline = 300 * time.Millisecond
	go func() {
		n, err := p.WriteTimeout(context.Background(), blockedPayload, effectiveDeadline+writeAbortGrace)
		res <- wres{n, err}
	}()
	waitWriteLockHeld(t, p) // 屏障：写已在 ioMu 临界区内（阻塞于底层写）

	closeStart := time.Now()
	closeErrCh := make(chan error, 1)
	go func() { closeErrCh <- p.Close() }()

	select {
	case err := <-closeErrCh:
		if err != nil {
			t.Errorf("Close err = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return after in-flight write deadline")
	}
	if elapsed := time.Since(closeStart); elapsed < effectiveDeadline-100*time.Millisecond {
		t.Errorf("Close returned after %v, want it to wait for in-flight write (~%v native deadline)", elapsed, effectiveDeadline)
	}
	// Close 返回时在途写必已结束（ioMu happens-before），且为未完整成功。
	select {
	case r := <-res:
		if r.err == nil && r.n == len(blockedPayload) {
			t.Fatalf("blocked write reported complete (%d, %v), want incomplete", r.n, r.err)
		}
	default:
		t.Fatal("Close returned before in-flight write finished (lifecycle ordering broken)")
	}

	// Close 先完成后的 WriteTimeout：立即失败，不触碰 ptmx。
	start := time.Now()
	if _, err := p.WriteTimeout(context.Background(), []byte("x"), time.Second); err == nil {
		t.Error("WriteTimeout after Close must fail immediately")
	} else if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("WriteTimeout after Close took %v, want immediate failure", elapsed)
	}
}

// TestPtyWriteTimeout_WriteAfterCloseFailsFast（Close 先发生）：Close 完成后
// WriteTimeout 立即失败返回（写入生命周期已终止，不等待、不阻塞）。
func TestPtyWriteTimeout_WriteAfterCloseFailsFast(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping pty integration test in -short mode")
	}
	p, err := Open(exec.Command("cat"), "", []string{"TERM=xterm"}, 80, 24)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	start := time.Now()
	n, err := p.WriteTimeout(context.Background(), []byte("hello"), time.Second)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("WriteTimeout after Close = (%d, nil), want immediate failure", n)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("WriteTimeout after Close took %v, want immediate failure", elapsed)
	}
}

// TestPtyWriteTimeout_CancelAndCloseConcurrent（取消与 Close 同时发生）：写阻塞期间
// ctx 取消与 Close 并发——写入按取消路径有界终止（不得按完整成功返回），Close
// 有界等待写结束后正常关闭，全部有界返回不挂起。
func TestPtyWriteTimeout_CancelAndCloseConcurrent(t *testing.T) {
	p, pr := newPipeWritePty(t)
	t.Cleanup(func() { _ = pr.Close() })
	ctx, cancel := context.WithCancel(context.Background())

	type wres struct {
		n   int
		err error
	}
	res := make(chan wres, 1)
	go func() {
		n, err := p.WriteTimeout(ctx, blockedPayload, 30*time.Second)
		res <- wres{n, err}
	}()
	waitWriteLockHeld(t, p) // 屏障：写已在临界区内阻塞，取消与 Close 并发发生

	go cancel()
	closeErrCh := make(chan error, 1)
	go func() { closeErrCh <- p.Close() }()

	select {
	case r := <-res:
		if r.err == nil && r.n == len(blockedPayload) {
			t.Fatalf("canceled write reported complete (%d, %v), want incomplete", r.n, r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("write did not terminate promptly after cancel")
	}
	select {
	case err := <-closeErrCh:
		if err != nil {
			t.Errorf("Close err = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not complete after canceled write finished")
	}
}
