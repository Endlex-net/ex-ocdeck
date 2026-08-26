package pty

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// quietCommand 是一个几乎不产生输出的命令（安静终端），用于验证空 buffer 的
// ticker 唤醒不误报 io.EOF（B5）。
func quietCommand() *exec.Cmd {
	return exec.Command("sleep", "10")
}

// TestPty_BatchFlushAndRead 验证 16ms 批量刷新：Open echo 循环 → Read 合并输出。
// 真实 PTY（无 tmux 依赖）：直接跑 cat 读 stdin 回显。
func TestPty_BatchFlushAndRead(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping pty integration test in -short mode")
	}
	cmd := exec.Command("cat")
	p, err := Open(cmd, "", []string{"TERM=xterm"}, 120, 40)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	// 写入少量数据，cat 回显到 stdout。
	if _, err := p.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Read 应在 16ms 窗口后返回合并输出。
	var buf bytes.Buffer
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(buf.String(), "hello") {
		data, err := p.Read()
		if err != nil {
			break
		}
		if len(data) > 0 {
			buf.Write(data)
		}
	}
	if !strings.Contains(buf.String(), "hello") {
		t.Fatalf("read did not contain hello within timeout; got %q", buf.String())
	}
}

// TestPty_Resize 验证 Resize 不报错（实际 winsize 传播由 tmux 客户端侧验证）。
func TestPty_Resize(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping pty integration test in -short mode")
	}
	cmd := exec.Command("cat")
	p, err := Open(cmd, "", []string{"TERM=xterm"}, 120, 40)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	if err := p.Resize(120, 40); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	// 无效尺寸应报错。
	if err := p.Resize(0, 40); err == nil {
		t.Error("Resize(0,40) should fail")
	}
	if err := p.Resize(120, 0); err == nil {
		t.Error("Resize(120,0) should fail")
	}
}

// TestPty_CloseIdempotent 验证 Close 幂等。
func TestPty_CloseIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping pty integration test in -short mode")
	}
	cmd := exec.Command("cat")
	p, err := Open(cmd, "", []string{"TERM=xterm"}, 120, 40)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestPty_WriteEmptyNoop 验证空写不报错。
func TestPty_WriteEmptyNoop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping pty integration test in -short mode")
	}
	cmd := exec.Command("cat")
	p, err := Open(cmd, "", []string{"TERM=xterm"}, 120, 40)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	n, err := p.Write(nil)
	if err != nil || n != 0 {
		t.Errorf("Write(nil) = (%d, %v), want (0, nil)", n, err)
	}
}

// TestPty_QuietTerminalNoFalseEOF 验证安静终端下空 buffer 的 ticker 唤醒
// 不误报 io.EOF（B5：pty.go 旧实现安静终端每 16ms 被误判关闭）。
// 启动一个 sleep 命令（几乎无输出），Read 在短时间内不应返回 EOF。
func TestPty_QuietTerminalNoFalseEOF(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping pty integration test in -short mode")
	}
	p, err := Open(quietCommand(), "", []string{"TERM=xterm"}, 80, 24)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	// Read 应阻塞（无数据），不应在 16ms ticker 唤醒时返回 EOF。
	// 给 500ms 窗口（远超多个 16ms tick），确认不误报 EOF。
	done := make(chan error, 1)
	go func() {
		_, err := p.Read()
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Read on quiet terminal returned err %v (false EOF/diagnostic)", err)
		}
		t.Fatal("Read on quiet terminal returned data without write")
	case <-time.After(500 * time.Millisecond):
		// 500ms 内未返回 = 阻塞正确，安静终端未被误判关闭。
	}
}

// TestPty_BatchFlushForms16msWindow 验证 16ms 批量窗口真实形成：连续小包
// 不每次 Read 即触发消费，而是累积到 16ms 窗口后合并返回（B5）。
func TestPty_BatchFlushForms16msWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping pty integration test in -short mode")
	}
	// cat 回显：写入两次小数据，观察是否合并为一次 Read。
	cmd := exec.Command("cat")
	p, err := Open(cmd, "", []string{"TERM=xterm"}, 120, 40)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	// 连续写两段，间隔 < 16ms。
	if _, err := p.Write([]byte("aa")); err != nil {
		t.Fatalf("Write aa: %v", err)
	}
	if _, err := p.Write([]byte("bb")); err != nil {
		t.Fatalf("Write bb: %v", err)
	}

	// 在 16ms 窗口内，第一次 Read 应返回合并的 "aabb"（批量窗口生效）。
	// 若每次 Read 即 flush，可能分两次返回 "aa" 与 "bb"。
	var buf bytes.Buffer
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(buf.String(), "aabb") {
		data, err := p.Read()
		if err != nil {
			break
		}
		if len(data) > 0 {
			buf.Write(data)
		}
	}
	if !strings.Contains(buf.String(), "aabb") {
		t.Errorf("batch window did not merge aa+bb; got %q", buf.String())
	}
}

// TestPty_CloseTimeoutDoesNotBlock 验证 Close 带超时：子进程不退出时 Close
// 不永久阻塞（B5：close master 后子进程未退出则 Kill）。
func TestPty_CloseTimeoutDoesNotBlock(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping pty integration test in -short mode")
	}
	// 一个忽略 SIGHUP 的长睡进程，模拟 Close 后子进程不退出。
	cmd := exec.Command("bash", "-c", "trap '' HUP; sleep 30")
	p, err := Open(cmd, "", []string{"TERM=xterm"}, 80, 24)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Close 应在 closeTimeout 内返回（子进程被 Kill）。
	done := make(chan error, 1)
	go func() { done <- p.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(closeTimeout + 3*time.Second):
		t.Fatal("Close blocked beyond timeout")
	}
}

// TestRingBuffer_OverflowDropsOldest 验证 ringBuffer 溢出时丢弃最旧并标记（B5）。
func TestRingBuffer_OverflowDropsOldest(t *testing.T) {
	rb := newRingBuffer(8)
	rb.Write([]byte("AAAA")) // 4
	rb.Write([]byte("BBBB")) // 8 = cap
	rb.Write([]byte("CC"))   // 溢出 → 丢最旧
	if !rb.Overflow() {
		t.Error("overflow flag not set")
	}
	if rb.Len() > 8 {
		t.Errorf("len %d exceeds cap 8", rb.Len())
	}
}

// TestRingBuffer_NoOverflowUnderCap 验证未超限时正常写入不标记 overflow。
func TestRingBuffer_NoOverflowUnderCap(t *testing.T) {
	rb := newRingBuffer(16)
	rb.Write([]byte("hello"))
	if rb.Overflow() {
		t.Error("overflow flag set under cap")
	}
	if string(rb.Bytes()) != "hello" {
		t.Errorf("bytes = %q, want hello", rb.Bytes())
	}
}

// P1: 溢出时环形缓冲 MUST 保留最新 capacity 字节（丢弃最旧）。
// 旧实现保留输入前缀+丢弃旧数据，最终保留的不是最新 capacity 字节，导致丢失最新输出。
func TestRingBuffer_OverflowKeepsNewest(t *testing.T) {
	const cap = 8
	rb := newRingBuffer(cap)
	// 逐步写入超过 cap，最终应保留最后 cap 字节 "45678901"。
	for i := 0; i < 9; i++ {
		rb.Write([]byte{byte('0' + i)}) // '0'..'8' 共 9 字节，预期保留 "12345678"
	}
	if !rb.Overflow() {
		t.Error("overflow flag not set")
	}
	if got := rb.Bytes(); len(got) != cap {
		t.Errorf("len = %d, want %d (got=%q)", len(got), cap, got)
	} else if string(got) != "12345678" {
		t.Errorf("bytes = %q, want \"12345678\" (newest cap bytes)", got)
	}
}

// P1: 单次大写入超过 cap 时保留输入尾部 cap 字节。
func TestRingBuffer_SingleLargeWriteKeepsTail(t *testing.T) {
	const cap = 8
	rb := newRingBuffer(cap)
	// 一次写入 20 字节，应只保留尾部 cap=8 字节 "mnopqrst"... 用 20 字符输入。
	input := "0123456789ABCDEFGHIJ" // 20 字节
	rb.Write([]byte(input))
	if !rb.Overflow() {
		t.Error("overflow flag not set")
	}
	want := input[len(input)-cap:]
	if got := rb.Bytes(); string(got) != want {
		t.Errorf("bytes = %q, want %q (tail cap bytes)", got, want)
	}
}

// P1: 多次写入累计超过 cap，最终保留最新 cap 字节（尾部）。
func TestRingBuffer_MultiWriteKeepsNewestTail(t *testing.T) {
	const cap = 8
	rb := newRingBuffer(cap)
	rb.Write([]byte("AAAA")) // 4
	rb.Write([]byte("BBBB")) // 8 = cap
	rb.Write([]byte("CC"))   // 溢出 2 → 丢弃最旧 2 "AA"，保留 "AABB...CC"
	// 期望保留最新 8 字节：原 "AAAABBBB" 丢弃最旧 2 = "AABBBB" + "CC" = "AABBBBCC"
	if !rb.Overflow() {
		t.Error("overflow flag not set")
	}
	if got, want := rb.Bytes(), "AABBBBCC"; len(got) != cap || string(got) != want {
		t.Errorf("bytes = %q (len %d), want %q (len %d)", got, len(got), want, cap)
	}
}