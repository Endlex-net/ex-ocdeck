// diffreview_coverage_test.go 补齐 tasks.md 3.11 清单中 task 层覆盖项。
//
// 覆盖项（逐字对齐 3.11 清单）：
//   - 能力事件模型：首探/复探 singleflight 合并/instVersion 失效
//   - 双任务路由隔离（PromptPortAdapter 经 taskOcClient(taskID) 取各自 client+directory）
//   - lineEnding 冻结（CRLF 删除全部换行后新增换行仍按 crlf 重建、风格与冻结值不一致 409）
//   - 写回禁锢（中间级 symlink 逃逸零写盘）
//   - adapter 获取失败=pre_send_failure（taskOcClient ok=false）
package task

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ocdeck/internal/application/diffreview"
	"ocdeck/internal/infrastructure/opencode"
)

// --- 能力事件模型：首探/复探 singleflight 合并 ---

// TestCapabilityProbeSingleFlightConcurrentMerge 验证并发 ProbeCapability 经 singleflight
// 合并为一次 GET /doc 探测（ProbePromptAsyncCapability 仅调用一次）。
func TestCapabilityProbeSingleFlightConcurrentMerge(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	proc := newMockProc()
	setProcServeEnv(proc, "t1")
	// 计数 ProbePromptAsyncCapability 调用次数。
	var probeCalls int32
	oc := newMockOC(true)
	oc.probePromptAsyncResult = opencode.CapabilitySupported
	wrap := &countingProbeOC{inner: oc, probeCalls: &probeCalls}
	factory := func(port int, password string, opts opencode.Options) OCClient { return wrap }
	m := newTestManagerWithFactory(t, store, proc, newMockWorktree(), factory)
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)

	adapter := NewRuntimePortAdapter(m)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			adapter.ProbeCapability(context.Background(), "t1")
		}()
	}
	wg.Wait()
	// F15：singleflight 合并——稳定缓存检查与 inflight 注册在同一 reg.mu 临界区内完成，
	// 消除 TOCTOU 窗口。10 个并发请求 → 严格 1 次 GET /doc 探测（首个调用者执行，其余等待 inflight
	// 完成后命中缓存）。MUST NOT 出现 2 次以上（原允许 <=2 的宽松断言掩盖 TOCTOU）。
	n := atomic.LoadInt32(&probeCalls)
	if n != 1 {
		t.Errorf("F15 singleflight should merge concurrent probes to exactly 1, got %d probe calls", n)
	}
}

// TestCapabilityProbeSingleFlightBarrierStrictOneProbe 验证 F15：barrier 同步 10 个真重叠调用，
// 严格断言 probeCalls==1（首个调用者阻塞在探测中，其余 9 个在 inflight 等待，不绕过发起各自探测）。
func TestCapabilityProbeSingleFlightBarrierStrictOneProbe(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	proc := newMockProc()
	setProcServeEnv(proc, "t1")
	var probeCalls int32
	oc := newMockOC(true)
	oc.probePromptAsyncResult = opencode.CapabilitySupported
	// barrierProbeOC：首次 ProbePromptAsyncCapability 阻塞在 start 通道，等待所有 waiter 注册后释放。
	start := make(chan struct{})
	proceed := make(chan struct{})
	wrap := &barrierProbeOC{mockOC: oc, probeCalls: &probeCalls, start: start, proceed: proceed}
	factory := func(port int, password string, opts opencode.Options) OCClient { return wrap }
	m := newTestManagerWithFactory(t, store, proc, newMockWorktree(), factory)
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)

	adapter := NewRuntimePortAdapter(m)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			adapter.ProbeCapability(context.Background(), "t1")
		}()
	}
	// 等待首个探测开始（阻塞在 start），确保 inflight 已注册。
	<-start
	// 短暂等待让其余 9 个 waiter 进入 inflight 等待（它们不应发起新探测）。
	time.Sleep(50 * time.Millisecond)
	close(proceed) // 释放首个探测。
	wg.Wait()
	n := atomic.LoadInt32(&probeCalls)
	if n != 1 {
		t.Errorf("F15 barrier: strict singleflight should produce exactly 1 probe, got %d", n)
	}
}

// barrierProbeOC 包装 mockOC，首次 ProbePromptAsyncCapability 通知 start 并阻塞 proceed，
// 用于强制 10 个并发 ProbeCapability 真重叠（首个阻塞探测中，其余等待 inflight）。
// 嵌入 *mockOC 以满足 OCClient 接口（其余方法透传 mockOC）。
type barrierProbeOC struct {
	*mockOC
	probeCalls *int32
	start      chan<- struct{}
	proceed    <-chan struct{}
	called     bool
	mu         sync.Mutex
}

func (c *barrierProbeOC) ProbePromptAsyncCapability(ctx context.Context) opencode.CapabilityState {
	c.mu.Lock()
	if !c.called {
		c.called = true
		c.mu.Unlock()
		atomic.AddInt32(c.probeCalls, 1)
		close(c.start)
		<-c.proceed // 阻塞首个探测，确保 waiter 真重叠等待。
		return c.mockOC.ProbePromptAsyncCapability(ctx)
	}
	c.mu.Unlock()
	atomic.AddInt32(c.probeCalls, 1)
	return c.mockOC.ProbePromptAsyncCapability(ctx)
}

// countingProbeOC 包装 mockOC，计数 ProbePromptAsyncCapability 调用。
type countingProbeOC struct {
	inner      OCClient
	probeCalls *int32
}

func (c *countingProbeOC) Health(ctx context.Context) (opencode.HealthResponse, error) {
	return c.inner.Health(ctx)
}
func (c *countingProbeOC) Probe(ctx context.Context) (string, error) { return c.inner.Probe(ctx) }
func (c *countingProbeOC) ListSessions(ctx context.Context, dir string, limit int) ([]opencode.Session, error) {
	return c.inner.ListSessions(ctx, dir, limit)
}
func (c *countingProbeOC) GetSession(ctx context.Context, dir, id string) (opencode.Session, error) {
	return c.inner.GetSession(ctx, dir, id)
}
func (c *countingProbeOC) CreateSession(ctx context.Context, dir, title string) (opencode.Session, error) {
	return c.inner.CreateSession(ctx, dir, title)
}
func (c *countingProbeOC) DeleteSession(ctx context.Context, dir, id string) error {
	return c.inner.DeleteSession(ctx, dir, id)
}
func (c *countingProbeOC) SessionStatus(ctx context.Context, dir string) (map[string]opencode.SessionStatus, error) {
	return c.inner.SessionStatus(ctx, dir)
}
func (c *countingProbeOC) SubscribeEvents(ctx context.Context, dir string, onEvent func(opencode.Event), onReconnect func()) error {
	return c.inner.SubscribeEvents(ctx, dir, onEvent, onReconnect)
}
func (c *countingProbeOC) ListPermissions(ctx context.Context, dir string) ([]opencode.PermissionRequest, error) {
	return c.inner.ListPermissions(ctx, dir)
}
func (c *countingProbeOC) ListQuestions(ctx context.Context, dir string) ([]opencode.QuestionRequest, error) {
	return c.inner.ListQuestions(ctx, dir)
}
func (c *countingProbeOC) PromptAsync(ctx context.Context, dir, sessionID, messageID, text string) opencode.PromptResult {
	return c.inner.PromptAsync(ctx, dir, sessionID, messageID, text)
}
func (c *countingProbeOC) ProbePromptAsyncCapability(ctx context.Context) opencode.CapabilityState {
	atomic.AddInt32(c.probeCalls, 1)
	return c.inner.ProbePromptAsyncCapability(ctx)
}

// --- 能力事件模型：instVersion 失效 ---

// TestCapabilityCacheInvalidatedOnInstVersionChange 验证 instVersion 变化（runtime 替换）后
// 能力缓存失效，重新探测。
func TestCapabilityCacheInvalidatedOnInstVersionChange(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	proc := newMockProc()
	setProcServeEnv(proc, "t1")
	var probeCalls int32
	oc := newMockOC(true)
	oc.probePromptAsyncResult = opencode.CapabilitySupported
	wrap := &countingProbeOC{inner: oc, probeCalls: &probeCalls}
	factory := func(port int, password string, opts opencode.Options) OCClient { return wrap }
	m := newTestManagerWithFactory(t, store, proc, newMockWorktree(), factory)

	rt1 := m.newRuntime("t1")
	m.setRuntime("t1", rt1)
	adapter := NewRuntimePortAdapter(m)
	st1, err := adapter.ProbeCapability(context.Background(), "t1")
	if err != nil || st1 != diffreview.CapabilitySupported {
		t.Fatalf("first probe: st=%v err=%v", st1, err)
	}
	firstCalls := atomic.LoadInt32(&probeCalls)

	// runtime 替换（新 instVersion）→ 缓存失效，重新探测。
	m.clearRuntime("t1")
	rt2 := m.newRuntime("t1")
	m.setRuntime("t1", rt2)
	st2, err := adapter.ProbeCapability(context.Background(), "t1")
	if err != nil || st2 != diffreview.CapabilitySupported {
		t.Fatalf("second probe after instVersion change: st=%v err=%v", st2, err)
	}
	secondCalls := atomic.LoadInt32(&probeCalls)
	if secondCalls <= firstCalls {
		t.Errorf("instVersion change should trigger re-probe, calls before=%d after=%d", firstCalls, secondCalls)
	}
}

// setProcServeEnv 在 mockProc 中设置 serve 会话的 password/port env（使 taskOcClient ok=true）。
func setProcServeEnv(proc *mockProc, taskID string) {
	serveName := runtimeSessionName(taskID)
	proc.mu.Lock()
	proc.envValues[serveName] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "secret",
		"OCDECK_SERVE_PORT":        "12345",
	}
	proc.mu.Unlock()
}

// --- 双任务路由隔离 ---

// TestPromptPortAdapterDualTaskRoutingIsolation 验证 PromptPortAdapter 经 taskOcClient(taskID)
// 取各自 client+directory，双任务投递互不串投（每个任务的 PromptAsync 用各自 OCClient）。
func TestPromptPortAdapterDualTaskRoutingIsolation(t *testing.T) {
	dir1 := t.TempDir()
	initTestGitRepo(t, dir1)
	dir2 := t.TempDir()
	initTestGitRepo(t, dir2)
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.WorktreePath = dir1 })
	seedActiveTask(store, "t2", "p2")
	store.mutTask("t2", func(r *TaskRow) { r.WorktreePath = dir2 })
	proc := newMockProc()
	setProcServeEnv(proc, "t1")
	setProcServeEnv(proc, "t2")

	// 用两个独立 mockOC 实例（经 factory 按 port 区分），记录各自 PromptAsync 的 dir。
	var dir1Calls, dir2Calls []string
	var mu sync.Mutex
	oc1 := newMockOC(true)
	oc1.promptAsyncResult = opencode.PromptResult{Kind: opencode.ResultAccepted}
	oc2 := newMockOC(true)
	oc2.promptAsyncResult = opencode.PromptResult{Kind: opencode.ResultAccepted}
	wrap1 := &dirCaptureOC{inner: oc1, dirs: &dir1Calls, mu: &mu}
	wrap2 := &dirCaptureOC{inner: oc2, dirs: &dir2Calls, mu: &mu}
	// factory 按 port 区分：t1 port=12345, t2 port=12345（同端口）——需按 password 或其他区分。
	// 实际 taskOcClient 按 taskID 取 env，两任务 port 可同。factory 需按 taskID 区分——但 factory
	// 签名只收 port+password。改用 password 区分：t1 password="secret1", t2 password="secret2"。
	proc2 := newMockProc()
	proc2.mu.Lock()
	proc2.envValues[runtimeSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "secret1", "OCDECK_SERVE_PORT": "11111",
	}
	proc2.envValues[runtimeSessionName("t2")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "secret2", "OCDECK_SERVE_PORT": "22222",
	}
	proc2.mu.Unlock()
	factory := func(port int, password string, opts opencode.Options) OCClient {
		if password == "secret1" {
			return wrap1
		}
		return wrap2
	}
	m := newTestManagerWithFactory(t, store, proc2, newMockWorktree(), factory)
	m.setRuntime("t1", m.newRuntime("t1"))
	m.setRuntime("t2", m.newRuntime("t2"))

	adapter := NewPromptPortAdapter(m)
	adapter.PromptAsync(context.Background(), "t1", "sess1", "msg1", "text1")
	adapter.PromptAsync(context.Background(), "t2", "sess2", "msg2", "text2")

	mu.Lock()
	defer mu.Unlock()
	if len(dir1Calls) != 1 || dir1Calls[0] != dir1 {
		t.Errorf("t1 should route to dir1=%q, got %v", dir1, dir1Calls)
	}
	if len(dir2Calls) != 1 || dir2Calls[0] != dir2 {
		t.Errorf("t2 should route to dir2=%q, got %v", dir2, dir2Calls)
	}
}

// dirCaptureOC 包装 mockOC，记录 PromptAsync 的 dir 参数。
type dirCaptureOC struct {
	inner OCClient
	dirs  *[]string
	mu    *sync.Mutex
}

func (c *dirCaptureOC) Health(ctx context.Context) (opencode.HealthResponse, error) {
	return c.inner.Health(ctx)
}
func (c *dirCaptureOC) Probe(ctx context.Context) (string, error) { return c.inner.Probe(ctx) }
func (c *dirCaptureOC) ListSessions(ctx context.Context, dir string, limit int) ([]opencode.Session, error) {
	return c.inner.ListSessions(ctx, dir, limit)
}
func (c *dirCaptureOC) GetSession(ctx context.Context, dir, id string) (opencode.Session, error) {
	return c.inner.GetSession(ctx, dir, id)
}
func (c *dirCaptureOC) CreateSession(ctx context.Context, dir, title string) (opencode.Session, error) {
	return c.inner.CreateSession(ctx, dir, title)
}
func (c *dirCaptureOC) DeleteSession(ctx context.Context, dir, id string) error {
	return c.inner.DeleteSession(ctx, dir, id)
}
func (c *dirCaptureOC) SessionStatus(ctx context.Context, dir string) (map[string]opencode.SessionStatus, error) {
	return c.inner.SessionStatus(ctx, dir)
}
func (c *dirCaptureOC) SubscribeEvents(ctx context.Context, dir string, onEvent func(opencode.Event), onReconnect func()) error {
	return c.inner.SubscribeEvents(ctx, dir, onEvent, onReconnect)
}
func (c *dirCaptureOC) ListPermissions(ctx context.Context, dir string) ([]opencode.PermissionRequest, error) {
	return c.inner.ListPermissions(ctx, dir)
}
func (c *dirCaptureOC) ListQuestions(ctx context.Context, dir string) ([]opencode.QuestionRequest, error) {
	return c.inner.ListQuestions(ctx, dir)
}
func (c *dirCaptureOC) PromptAsync(ctx context.Context, dir, sessionID, messageID, text string) opencode.PromptResult {
	c.mu.Lock()
	*c.dirs = append(*c.dirs, dir)
	c.mu.Unlock()
	return c.inner.PromptAsync(ctx, dir, sessionID, messageID, text)
}
func (c *dirCaptureOC) ProbePromptAsyncCapability(ctx context.Context) opencode.CapabilityState {
	return c.inner.ProbePromptAsyncCapability(ctx)
}

// --- adapter 获取失败=pre_send_failure ---

// TestPromptPortAdapterRuntimeUnavailablePreSendFailure 验证 taskOcClient ok=false（任务非 active）
// → PromptOutcome{Kind: pre_send_failure, Detail: "runtime client unavailable"}。
func TestPromptPortAdapterRuntimeUnavailablePreSendFailure(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1") // 非 active → taskOcClient ok=false
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	adapter := NewPromptPortAdapter(m)
	outcome := adapter.PromptAsync(context.Background(), "t1", "sess1", "msg1", "text1")
	if outcome.Kind != diffreview.PromptOutcomePreSendFailure {
		t.Errorf("ok=false should be pre_send_failure, got %v", outcome.Kind)
	}
	if outcome.Detail != "runtime client unavailable" {
		t.Errorf("detail should be fixed text, got %q", outcome.Detail)
	}
}

// --- lineEnding 冻结：CRLF 删除全部换行后新增换行仍按 crlf 重建 ---

// TestFileEdit_Write_LineEndingFrozenCRLFRebuild 验证 lineEnding 冻结：
// 读取 CRLF 文件，删除全部换行后新增换行，写回携带 lineEnding=crlf → 重建为 \r\n（按冻结值）。
func TestFileEdit_Write_LineEndingFrozenCRLFRebuild(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	// 原文件 CRLF。
	rawBytes := []byte("a\r\nb\r\n")
	full := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(full, rawBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	_, adapter := newFileEditManager(t, dir)

	baseHash := diffreview.SHA256Hex(rawBytes)
	// 新 content：删除全部换行后新增一个换行（LF in content），但 lineEnding 冻结为 crlf → 重建为 \r\n。
	newContent := "ax\n"
	wantRebuilt := []byte("ax\r\n")
	req := diffreview.FileEditWriteRequest{
		Path: "f.txt", Content: newContent, BaseHash: baseHash,
		LineEnding: diffreview.LineEndingCRLF, BaseMode: "0644",
	}
	_, err := adapter.Write(context.Background(), "t1", req)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	got, _ := os.ReadFile(full)
	if string(got) != string(wantRebuilt) {
		t.Errorf("lineEnding frozen crlf: file=%q want %q (crlf rebuild)", got, wantRebuilt)
	}
}

// --- 写回禁锢：中间级 symlink 逃逸零写盘 ---

// TestFileEdit_Write_ContainmentEscapeZeroWrite 验证 path 经中间级 symlink 逃逸 worktree 根
// → invalid_input，零写盘（不创建任何文件）。
func TestFileEdit_Write_ContainmentEscapeZeroWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("symlink 创建在 root 外受限")
	}
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	writeFileInRepo(t, dir, "f.txt", "hello\n", 0o644)
	// 工作区 sub → 外部目录（symlink）；在 outside 下放一个目标文件使 symlink 解析命中。
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "f.txt"), []byte("escape\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "sub")); err != nil {
		t.Skipf("symlink creation failed: %v", err)
	}
	_, adapter := newFileEditManager(t, dir)

	baseHash := diffreview.SHA256Hex([]byte("escape\n")) // baseHash 取实际（逃逸后）文件内容
	// path="sub/f.txt" 经 sub symlink 指向 outside → 禁锢逃逸。
	req := diffreview.FileEditWriteRequest{
		Path: "sub/f.txt", Content: "new\n", BaseHash: baseHash,
		LineEnding: diffreview.LineEndingLF, BaseMode: "0644",
	}
	_, err := adapter.Write(context.Background(), "t1", req)
	if !isOpErrCode(err, codeInvalidInput) {
		t.Fatalf("containment escape: err=%v want codeInvalidInput", err)
	}
	// 零写盘：outside 下文件未变。
	got, _ := os.ReadFile(filepath.Join(outside, "f.txt"))
	if string(got) != "escape\n" {
		t.Errorf("containment escape should NOT write to outside target, got %q", got)
	}
	// 原工作区文件未修改。
	origGot, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if string(origGot) != "hello\n" {
		t.Errorf("original file modified on escape: %q", origGot)
	}
}

// --- lineEnding 冻结：风格与冻结值不一致 409 ---

// TestFileEdit_Write_LineEndingFrozenMismatchConflict 验证写回时当前文件换行风格与冻结 lineEnding
// 不一致 → conflict（409）零写盘。
func TestFileEdit_Write_LineEndingFrozenMismatchConflict(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	// 原文件 CRLF。
	rawBytes := []byte("a\r\nb\r\n")
	full := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(full, rawBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	_, adapter := newFileEditManager(t, dir)

	baseHash := diffreview.SHA256Hex(rawBytes)
	// 请求携带 lineEnding=lf，但当前文件为 crlf → 初检换行风格不一致 → conflict。
	req := diffreview.FileEditWriteRequest{
		Path: "f.txt", Content: "a\nb\n", BaseHash: baseHash,
		LineEnding: diffreview.LineEndingLF, BaseMode: "0644",
	}
	_, err := adapter.Write(context.Background(), "t1", req)
	if !isOpErrCode(err, codeConflict) {
		t.Fatalf("lineEnding mismatch: err=%v want codeConflict", err)
	}
	// 零写盘：原文件未变。
	got, _ := os.ReadFile(full)
	if string(got) != string(rawBytes) {
		t.Errorf("file modified on lineEnding mismatch: %q want %q", got, rawBytes)
	}
}

// TestDiffSourcePortAdapterReadLocked_MultiSourceSingleLock 验证 F14/F5 生产 adapter
// DiffSourcePortAdapter.ReadLocked：一次 tryLockTask + 批量来源经 gitDiffLocked（已持锁）读取，
// 首来源失败不短路（F14），后续来源仍读取。直接覆盖生产 adapter（非兼容 helper）。
func TestDiffSourcePortAdapterReadLocked_MultiSourceSingleLock(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	commitFile(t, dir, "a.txt", "v1\n")
	commitFile(t, dir, "b.txt", "v2\n")
	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	adapter := NewDiffSourcePortAdapter(m)

	srcs := []diffreview.DiffSource{
		{Path: "a.txt"},
		{Path: "b.txt"},
	}
	var seen []diffreview.DiffSource
	var results []diffreview.DiffSourceResult
	err := adapter.ReadLocked(context.Background(), "t1", srcs, func(src diffreview.DiffSource, result diffreview.DiffSourceResult, err error) error {
		seen = append(seen, src)
		if err != nil {
			return err
		}
		results = append(results, result)
		return nil
	})
	if err != nil {
		t.Fatalf("ReadLocked: %v", err)
	}
	if len(seen) != 2 {
		t.Errorf("should read all 2 sources, got %d", len(seen))
	}
	if len(results) != 2 {
		t.Fatalf("should get 2 results, got %d", len(results))
	}
	// a.txt 旧侧 = "v1\n"（HEAD），b.txt 旧侧 = "v2\n"（HEAD）。
	if results[0].OldContent != "v1\n" {
		t.Errorf("a.txt oldContent = %q, want v1\\n", results[0].OldContent)
	}
	if results[1].OldContent != "v2\n" {
		t.Errorf("b.txt oldContent = %q, want v2\\n", results[1].OldContent)
	}
}

// TestDiffSourcePortAdapterReadLocked_FirstSourceFailureContinues 验证 F14：首来源失败不短路，
// 后续来源仍读取（错误交回调汇总后继续遍历）。
func TestDiffSourcePortAdapterReadLocked_FirstSourceFailureContinues(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	commitFile(t, dir, "b.txt", "v2\n")
	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	adapter := NewDiffSourcePortAdapter(m)

	// a.txt 不存在 ref 侧（ref="nope" → git_error）→ 首来源失败；b.txt ref="" → index 侧。
	srcs := []diffreview.DiffSource{
		{Ref: "nope", Path: "a.txt"},
		{Path: "b.txt"},
	}
	var seen []diffreview.DiffSource
	var firstErr error
	err := adapter.ReadLocked(context.Background(), "t1", srcs, func(src diffreview.DiffSource, result diffreview.DiffSourceResult, err error) error {
		seen = append(seen, src)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		return nil // 不中止，继续遍历。
	})
	if err != nil {
		t.Fatalf("ReadLocked returned adapter-level error: %v", err)
	}
	if len(seen) != 2 {
		t.Errorf("F14: first source failure should NOT short-circuit, should read all 2 sources, got %d", len(seen))
	}
	if firstErr == nil {
		t.Errorf("first source error should be reported to callback")
	}
}

// --- F13：runtime 替换 barrier 测试（leader fence 失败不返回旧 supported；waiter 迭代重探）---

// TestCapabilityProbeLeaderFenceFailureRotatesToNewInstance 验证 F13①：leader 探测完成前 runtime
// 被替换 → writeCapCache fence 失败 → MUST NOT 返回旧 supported；有界迭代针对新实例重探并返回其结果。
func TestCapabilityProbeLeaderFenceFailureRotatesToNewInstance(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	proc := newMockProc()
	setProcServeEnv(proc, "t1")
	var probeCalls int32
	oc := newMockOC(true)
	oc.probePromptAsyncResult = opencode.CapabilitySupported
	// leaderBarrier：首次探测阻塞在 proceed，期间主测试替换 runtime。
	proceed := make(chan struct{})
	started := make(chan struct{})
	wrap := &barrierProbeOC{mockOC: oc, probeCalls: &probeCalls, start: started, proceed: proceed}
	factory := func(port int, password string, opts opencode.Options) OCClient { return wrap }
	m := newTestManagerWithFactory(t, store, proc, newMockWorktree(), factory)

	rt1 := m.newRuntime("t1")
	m.setRuntime("t1", rt1)
	adapter := NewRuntimePortAdapter(m)

	var wg sync.WaitGroup
	var st diffreview.CapabilityState
	var perr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		st, perr = adapter.ProbeCapability(context.Background(), "t1")
	}()
	// 等待 leader 探测开始（阻塞在 start，探测进行中）。
	<-started
	// 探测期间替换 runtime（新 instVersion）→ leader 的 writeCapCache fence 失败。
	m.clearRuntime("t1")
	rt2 := m.newRuntime("t1")
	m.setRuntime("t1", rt2)
	close(proceed) // 释放 leader 探测 → fence 失败 → 迭代重探新实例。

	wg.Wait()
	if perr != nil {
		t.Fatalf("ProbeCapability error: %v", perr)
	}
	// F13①：leader fence 失败后迭代重探新实例 → 返回新实例的 supported（不返回旧 supported）。
	if st != diffreview.CapabilitySupported {
		t.Errorf("F13①: after fence failure should rotate to new instance and return its state, got %v", st)
	}
	// 应有 2 次探测：首次（旧实例，fence 失败）+ 迭代重探（新实例）。
	n := atomic.LoadInt32(&probeCalls)
	if n < 2 {
		t.Errorf("F13①: fence failure should trigger re-probe on new instance, got %d probe calls (want >=2)", n)
	}
}

// TestCapabilityProbeWaiterInstanceChangeIteratesNotUnboundedRecursion 验证 F13③：真实 leader+waiter——
// waiter 在 leader inflight 上等待，唤醒后 instVersion 已变化 → 有界迭代重探新实例（非无界递归）。
// 全程经 probe-round + flightJoined 栅栏串行化（无 Sleep 推测挂入）：
//  1. r1：leader 首探阻塞（flight v1 开放）→ 替换 rt1→rt2 → 释放 → fence 失败 → 迭代 probe#2 阻塞（r2）。
//  2. r2 确认时 flight v1 已清理、flight v2 已注册——此刻启动 waiter，并经 flightJoined 确认
//     waiter 已挂入 flight v2 后才替换 rt2→rt3（waiter 唤醒分支确定命中，不可被绕过）。
//  3. 释放 r2 → leader 二次 fence 失败迭代 probe#3（r3）；waiter 唤醒后 captured v2 ≠ v3 →
//     迭代挂入 flight v3 → 释放 r3，双方经缓存命中（写先于 flight 关闭）返回 supported。
//
// 每实例恰好一次探测：probeCalls == 3，waiter 不自行发起探测（确定性断言）。
func TestCapabilityProbeWaiterInstanceChangeIteratesNotUnboundedRecursion(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	proc := newMockProc()
	setProcServeEnv(proc, "t1")
	var probeCalls int32
	oc := newMockOC(true)
	oc.probePromptAsyncResult = opencode.CapabilitySupported
	mb := newMultiBarrierProbeOC(oc, &probeCalls)
	factory := func(port int, password string, opts opencode.Options) OCClient { return mb }
	m := newTestManagerWithFactory(t, store, proc, newMockWorktree(), factory)

	rt1 := m.newRuntime("t1")
	m.setRuntime("t1", rt1)
	adapter := NewRuntimePortAdapter(m)
	// 预取注册表：flightJoined 在有调用者挂入既有 flight 时非阻塞通知（F12③ 确定性栅栏）。
	reg := adapter.getCapRegistry()

	// swapRuntime 原子替换 runtime（同包直改注册表）。MUST 用单临界区替换：
	// clearRuntime+setRuntime 两步之间存在 getRuntime==nil 窗口，并发 probe 会误判 absent。
	swapRuntime := func() {
		m.rtMu.Lock()
		m.runtimes["t1"] = m.newRuntime("t1")
		m.rtMu.Unlock()
	}

	// leader：首个 ProbeCapability（阻塞在探测中，flight v1 注册）。
	var wg sync.WaitGroup
	var leaderSt diffreview.CapabilityState
	var leaderErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		leaderSt, leaderErr = adapter.ProbeCapability(context.Background(), "t1")
	}()
	r1 := mb.waitNext() // leader probe#1 阻塞中；flight v1 已注册。

	// leader 探测期间替换 runtime（v2）→ 释放后 leader fence 必失败 → 迭代 probe#2（flight v2）。
	swapRuntime()
	r1.release()
	r2 := mb.waitNext() // leader probe#2 阻塞中；flight v1 已清理、flight v2 已注册。

	// waiter：此刻启动只能捕获 v2；挂入 flight v2 由 flightJoined 确认（确定性，非推测）。
	var waiterSt diffreview.CapabilityState
	var waiterErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		waiterSt, waiterErr = adapter.ProbeCapability(context.Background(), "t1")
	}()
	<-reg.flightJoined // waiter 已挂入 flight v2（此后唤醒必走 instVersion 变化的迭代分支）。

	// 释放前再替换（v3）→ 释放 r2：leader 二次 fence 失败迭代 probe#3（flight v3）；
	// waiter 唤醒后 captured v2 ≠ v3 → 迭代挂入 flight v3（F13③ 核心路径，确定命中）。
	swapRuntime()
	r2.release()
	r3 := mb.waitNext() // probe#3 阻塞中（leader 或 waiter 镜像发起，恰一次）。
	r3.release()

	wg.Wait()
	if leaderErr != nil {
		t.Fatalf("leader ProbeCapability error: %v", leaderErr)
	}
	if waiterErr != nil {
		t.Fatalf("waiter ProbeCapability error: %v", waiterErr)
	}
	// leader 两次 fence 失败 → 迭代至当前实例返回 supported（不返回旧实例结论）。
	if leaderSt != diffreview.CapabilitySupported {
		t.Errorf("F13①: leader after instance changes should iterate to current instance, got %v", leaderSt)
	}
	// waiter 唤醒后 instVersion 变化 → 有界迭代重探当前实例 → supported。
	if waiterSt != diffreview.CapabilitySupported {
		t.Errorf("F13③: waiter after instance change should iterate and return current state, got %v", waiterSt)
	}
	// 每实例恰一次探测（v1/v2/v3）；waiter 挂入或缓存命中，不重复探测（singleflight 合并）。
	if n := atomic.LoadInt32(&probeCalls); n != 3 {
		t.Errorf("F13③: should probe exactly once per instance (3 total), got %d probe calls", n)
	}
}

// TestCapabilityProbeWaiterContinuousRotationReturnsUnknown 验证 F13③：连续 runtime 替换超上限
// （maxInstanceRotations=8）→ 降级返回 unknown（非无界递归）。
func TestCapabilityProbeWaiterContinuousRotationReturnsUnknown(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	proc := newMockProc()
	setProcServeEnv(proc, "t1")
	var probeCalls int32
	oc := newMockOC(true)
	oc.probePromptAsyncResult = opencode.CapabilitySupported
	// multiBarrierProbeOC：每次 ProbePromptAsyncCapability 阻塞（供连续替换测试每轮 barrier）。
	mb := newMultiBarrierProbeOC(oc, &probeCalls)
	factory := func(port int, password string, opts opencode.Options) OCClient { return mb }
	m := newTestManagerWithFactory(t, store, proc, newMockWorktree(), factory)

	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	adapter := NewRuntimePortAdapter(m)

	var wg sync.WaitGroup
	var st diffreview.CapabilityState
	var perr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		st, perr = adapter.ProbeCapability(context.Background(), "t1")
	}()
	// 每轮：等待 probe 开始（阻塞），替换实例，释放 probe（fence 失败 → 迭代下一轮）。
	// maxInstanceRotations=8：8 轮 fence 失败后 ProbeCapability 退出返回 unknown。
	for i := 0; i < 8; i++ {
		round := mb.waitNext() // 等待本轮 probe 开始并阻塞。
		m.clearRuntime("t1")
		m.setRuntime("t1", m.newRuntime("t1"))
		round.release() // 释放本轮 probe（fence 失败 → 迭代下一轮）。
	}
	// 8 轮替换后迭代超上限，ProbeCapability 退出返回 unknown（无第 9 次 probe）。

	wg.Wait()
	if perr != nil {
		t.Fatalf("ProbeCapability error: %v", perr)
	}
	// 连续替换超上限后降级 unknown（非无界递归 panic）。
	if st != diffreview.CapabilityUnknown {
		t.Errorf("continuous rotation over maxInstanceRotations should return unknown, got %v", st)
	}
}

// multiBarrierProbeOC 包装 mockOC，每次 ProbePromptAsyncCapability 阻塞（供连续替换/waiter 栅栏测试）。
// 每轮 probe 先经 rounds channel 交付 round handle 再阻塞：waitNext 返回时该轮探测已确定进入
// 阻塞段（确定性 channel 同步，无 Sleep 轮询），调用方 release 放行。
type multiBarrierProbeOC struct {
	*mockOC
	probeCalls *int32
	rounds     chan *probeRound
}

func newMultiBarrierProbeOC(oc *mockOC, probeCalls *int32) *multiBarrierProbeOC {
	return &multiBarrierProbeOC{mockOC: oc, probeCalls: probeCalls, rounds: make(chan *probeRound, 8)}
}

type probeRound struct {
	proceed chan struct{}
}

func (c *multiBarrierProbeOC) ProbePromptAsyncCapability(ctx context.Context) opencode.CapabilityState {
	atomic.AddInt32(c.probeCalls, 1)
	r := &probeRound{proceed: make(chan struct{})}
	c.rounds <- r
	<-r.proceed
	return c.mockOC.ProbePromptAsyncCapability(ctx)
}

// waitNext 等待下一轮 probe 交付（阻塞中），返回 round handle 供 release。
func (c *multiBarrierProbeOC) waitNext() *probeRound { return <-c.rounds }

func (r *probeRound) release() { close(r.proceed) }

// TestCapabilityProbeWaiterCacheReadRaceWithInvalidation 验证 F17：waiter 从 inflight 唤醒后
// readCapCache 与并发的 InvalidateCapability 真实写不竞态（旧实现无锁直读 reg.cache，-race 可检出
// DATA RACE）。修复后 waiter 经持锁 readCapCache 读，无竞态。
// 真实重叠确定性构造（F21，无 Sleep 推测、无 no-op 栅栏）：
//  1. 先完成一次成功探测，cache[t1] 落入真实条目 {v1, supported}——后续 invalidator 的失效写
//     不再是「条目不存在直接返回」的 no-op。
//  2. 原子替换 runtime（v2）→ leader 注册 flight v2 阻塞探测；flightJoined 确认 waiter 已挂入。
//  3. invalidator 对真实条目执行失效写（v1/v2 双版本交替，任一时刻至少一半命中当前条目；
//     首写针对 v1 条目必为真实写），首写完成经 started 上报；waiter 挂入 + invalidator 已写
//     双确认后才释放 leader——leader 写缓存、关 flight、waiter 唤醒 readCapCache 与 invalidator
//     写的并发重叠确定成立。
func TestCapabilityProbeWaiterCacheReadRaceWithInvalidation(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	proc := newMockProc()
	setProcServeEnv(proc, "t1")
	var probeCalls int32
	oc := newMockOC(true)
	oc.probePromptAsyncResult = opencode.CapabilitySupported
	mb := newMultiBarrierProbeOC(oc, &probeCalls)
	factory := func(port int, password string, opts opencode.Options) OCClient { return mb }
	m := newTestManagerWithFactory(t, store, proc, newMockWorktree(), factory)
	rt1 := m.newRuntime("t1")
	m.setRuntime("t1", rt1)
	adapter := NewRuntimePortAdapter(m)
	instVerV1 := string(rt1.instVersion)
	reg := adapter.getCapRegistry()

	// Phase A：完成一次成功探测 → cache[t1] 落入真实条目 {v1, supported}（fence 成功，先写后关 flight）。
	var seedWG sync.WaitGroup
	seedWG.Add(1)
	go func() {
		defer seedWG.Done()
		if _, err := adapter.ProbeCapability(context.Background(), "t1"); err != nil {
			t.Errorf("seed probe: %v", err)
		}
	}()
	mb.waitNext().release()
	seedWG.Wait()

	// swapRuntime 原子替换 runtime（同包直改注册表），返回新 runtime 供取 instVersion。
	swapRuntime := func() *taskRuntime {
		m.rtMu.Lock()
		rt := m.newRuntime("t1")
		m.runtimes["t1"] = rt
		m.rtMu.Unlock()
		return rt
	}

	// Phase B：替换 runtime（v2）→ leader 注册 flight v2 并阻塞探测（cache 条目仍为 v1 版）。
	rt2 := swapRuntime()
	instVerV2 := string(rt2.instVersion)
	var leaderWG sync.WaitGroup
	var leaderErr error
	leaderWG.Add(1)
	go func() {
		defer leaderWG.Done()
		_, leaderErr = adapter.ProbeCapability(context.Background(), "t1")
	}()
	r2 := mb.waitNext()

	// Phase C：waiter 挂入 flight v2，flightJoined 确认（确定性，非推测）。
	var waiterWG sync.WaitGroup
	var waiterErr error
	waiterWG.Add(1)
	go func() {
		defer waiterWG.Done()
		_, waiterErr = adapter.ProbeCapability(context.Background(), "t1")
	}()
	<-reg.flightJoined

	// Phase D：50 个 invalidator 对真实条目执行失效写——首写针对 v1 条目（必为真实写），
	// 完成后经 started 上报；循环内 v1/v2 双版本交替，leader 覆写为 {v2} 后 v2 版失效写仍真实。
	const numInvalidators = 50
	invalidateStarted := make(chan struct{}, numInvalidators)
	stop := make(chan struct{})
	var invalidateWG sync.WaitGroup
	for i := 0; i < numInvalidators; i++ {
		invalidateWG.Add(1)
		go func() {
			defer invalidateWG.Done()
			adapter.InvalidateCapability(context.Background(), "t1", instVerV1) // 首写：真实失效 v1 条目。
			invalidateStarted <- struct{}{}
			for {
				select {
				case <-stop:
					return
				default:
				}
				adapter.InvalidateCapability(context.Background(), "t1", instVerV1)
				adapter.InvalidateCapability(context.Background(), "t1", instVerV2)
			}
		}()
	}
	for i := 0; i < numInvalidators; i++ {
		<-invalidateStarted // 确认全部 invalidator 已发生真实写。
	}
	// 释放 leader → leader 真实写缓存（覆写为 {v2}）+ 关 flight；waiter 唤醒后 readCapCache
	// 与 invalidator 持续失效写并发重叠（-race 检测持锁读写不竞态）。
	r2.release()
	leaderWG.Wait()
	waiterWG.Wait()
	close(stop)
	invalidateWG.Wait()
	if leaderErr != nil {
		t.Fatalf("leader ProbeCapability error: %v", leaderErr)
	}
	if waiterErr != nil {
		t.Fatalf("waiter ProbeCapability error: %v", waiterErr)
	}
	// 恰两次探测（seed v1 + leader v2）；waiter 挂入已由 flightJoined 证明，不自行探测。
	if n := atomic.LoadInt32(&probeCalls); n != 2 {
		t.Errorf("F17: should probe exactly twice (seed + leader), got %d probe calls", n)
	}
	// -race 下无 DATA RACE 即通过（F17 修复后 waiter 经持锁 readCapCache 读）。
}

// --- errScope / 辅助：确保 errors import 被使用 ---
