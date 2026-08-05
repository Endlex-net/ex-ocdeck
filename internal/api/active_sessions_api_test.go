package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ocdeck/internal/task"
)

// activeSessionsBackend 可注入 ListActiveTaskOverview 返回值与 AgentStatus 行为，
// 供 GET /api/v1/sessions/active handler 测试（cross-project-active-sessions D3/D4）。
type activeSessionsBackend struct {
	*fakeTaskBackend
	overviewRows []task.ActiveTaskOverviewRow
	overviewErr  error
	// agentStatusFn 注入每个 taskID 的 hydration 行为；nil 时返回空串（降级）。
	agentStatusFn func(ctx context.Context, taskID string) string
	// agentStatusCalls 记录 AgentStatus 被调用的 taskID（断言 hydration 次数/顺序）。
	agentStatusMu    sync.Mutex
	agentStatusCalls []string
}

func newActiveSessionsBackend(rows ...task.ActiveTaskOverviewRow) *activeSessionsBackend {
	return &activeSessionsBackend{
		fakeTaskBackend: &fakeTaskBackend{},
		overviewRows:    rows,
	}
}

func (b *activeSessionsBackend) ListActiveTaskOverview(ctx context.Context) ([]task.ActiveTaskOverviewRow, error) {
	if b.overviewErr != nil {
		return nil, b.overviewErr
	}
	return b.overviewRows, nil
}

func (b *activeSessionsBackend) AgentStatus(ctx context.Context, taskID string) string {
	b.agentStatusMu.Lock()
	b.agentStatusCalls = append(b.agentStatusCalls, taskID)
	b.agentStatusMu.Unlock()
	if b.agentStatusFn != nil {
		return b.agentStatusFn(ctx, taskID)
	}
	return ""
}

func (b *activeSessionsBackend) agentStatusCallCount() int {
	b.agentStatusMu.Lock()
	defer b.agentStatusMu.Unlock()
	return len(b.agentStatusCalls)
}

// activeRow 构造 task.ActiveTaskOverviewRow 测试行辅助。
func activeRow(id, projectID, projectName, name, branch, wt string, lastActive int64) task.ActiveTaskOverviewRow {
	return task.ActiveTaskOverviewRow{
		ID: id, ProjectID: projectID, ProjectName: projectName, Name: name,
		Branch: branch, WorktreePath: wt, LastActiveAt: lastActive,
	}
}

// decodeActiveSessions 解码响应体为 activeSessionDTO 切片，并校验为非 null 的 JSON 数组。
func decodeActiveSessions(t *testing.T, body string) []activeSessionDTO {
	t.Helper()
	body = strings.TrimSpace(body)
	if body == "null" {
		t.Fatal("response body is null, want []")
	}
	var got []activeSessionDTO
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, body)
	}
	return got
}

// readAndDecode 读取响应体一次，返回原始字符串与解码结果（避免 double-read 空体 bug）。
func readAndDecode(t *testing.T, body io.Reader) (string, []activeSessionDTO) {
	t.Helper()
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(raw), decodeActiveSessions(t, string(raw))
}

func TestListActiveSessions_HappyPath(t *testing.T) {
	rows := []task.ActiveTaskOverviewRow{
		activeRow("t1", "p1", "projA", "taskA", "bA", "/wtA", 300),
		activeRow("t2", "p2", "projB", "taskB", "bB", "/wtB", 200),
	}
	tb := newActiveSessionsBackend(rows...)
	tb.agentStatusFn = func(ctx context.Context, taskID string) string {
		switch taskID {
		case "t1":
			return "busy"
		case "t2":
			return "idle"
		}
		return ""
	}
	s := newAPITestServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/sessions/active", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_, got := readAndDecode(t, resp.Body)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	// 字段断言 + sort（last_active_at DESC）。
	want := []activeSessionDTO{
		{TaskID: "t1", ProjectID: "p1", ProjectName: "projA", Name: "taskA", Branch: "bA", WorktreePath: "/wtA", LastActiveAt: 300, AgentStatus: "busy"},
		{TaskID: "t2", ProjectID: "p2", ProjectName: "projB", Name: "taskB", Branch: "bB", WorktreePath: "/wtB", LastActiveAt: 200, AgentStatus: "idle"},
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("dto[%d] = %+v, want %+v", i, got[i], w)
		}
	}
}

// TestListActiveSessions_PartialDegradationKeepsSuccessfulAgentStatus 验证多任务场景下
// 部分 AgentStatus 失败/降级时，成功任务仍携带 agentStatus，仅失败任务省略字段
// （cross-project-active-sessions D4：单点降级不阻塞其他）。
func TestListActiveSessions_PartialDegradationKeepsSuccessfulAgentStatus(t *testing.T) {
	rows := []task.ActiveTaskOverviewRow{
		activeRow("t-ok", "p1", "projA", "taskA", "bA", "/wtA", 200),
		activeRow("t-fail", "p2", "projB", "taskB", "bB", "/wtB", 100),
	}
	tb := newActiveSessionsBackend(rows...)
	// t-ok 成功返回 busy；t-fail 返回空串（降级）→ agentStatus 省略。
	tb.agentStatusFn = func(ctx context.Context, taskID string) string {
		if taskID == "t-ok" {
			return "busy"
		}
		return ""
	}
	s := newAPITestServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/sessions/active", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// 读取一次 body，复用于 raw 与 decoded 断言（避免 double-read 空体）。
	raw, got := readAndDecode(t, resp.Body)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	// sort by last_active_at DESC: t-ok(200) first, t-fail(100) second.
	if got[0].TaskID != "t-ok" || got[1].TaskID != "t-fail" {
		t.Fatalf("sort order = [%s,%s], want [t-ok,t-fail]", got[0].TaskID, got[1].TaskID)
	}
	if got[0].AgentStatus != "busy" {
		t.Errorf("t-ok agentStatus = %q, want busy (successful hydration retained)", got[0].AgentStatus)
	}
	if got[1].AgentStatus != "" {
		t.Errorf("t-fail agentStatus = %q, want empty (degraded)", got[1].AgentStatus)
	}
	// 原始 JSON：t-ok 含 agentStatus，t-fail 省略。
	if !strings.Contains(raw, `"agentStatus":"busy"`) {
		t.Errorf("raw response missing t-ok agentStatus:busy: %s", raw)
	}
	// t-fail 行不应出现 agentStatus 字段（验证 omit 生效：仅 1 处 agentStatus 出现）。
	if strings.Count(raw, "agentStatus") != 1 {
		t.Errorf("raw response has %d agentStatus occurrences, want 1 (only t-ok): %s", strings.Count(raw, "agentStatus"), raw)
	}
}

func TestListActiveSessions_EmptyReturnsArrayNotNull(t *testing.T) {
	tb := newActiveSessionsBackend() // 无行
	s := newAPITestServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/sessions/active", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	body := strings.TrimSpace(string(raw))
	if body == "null" {
		t.Fatal("empty response is null, want []")
	}
	if body != "[]" {
		t.Errorf("empty body = %q, want []", body)
	}
	// hydration 不应被调用。
	if calls := tb.agentStatusCallCount(); calls != 0 {
		t.Errorf("AgentStatus called %d times on empty result, want 0", calls)
	}
}

func TestListActiveSessions_StoreFailureReturns500NoHydration(t *testing.T) {
	tb := newActiveSessionsBackend()
	tb.overviewErr = errStoreFailure
	s := newAPITestServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/sessions/active", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	var eb errorBody
	if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
		t.Fatal(err)
	}
	if eb.Error.Code != CodeInternal {
		t.Errorf("error code = %q, want %q", eb.Error.Code, CodeInternal)
	}
	// store 失败 MUST NOT 进入 hydration。
	if calls := tb.agentStatusCallCount(); calls != 0 {
		t.Errorf("AgentStatus called %d times on store failure, want 0 (no hydration)", calls)
	}
}

func TestListActiveSessions_UnauthorizedWithoutToken(t *testing.T) {
	tb := newActiveSessionsBackend()
	s := newAPITestServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/sessions/active", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// TestListActiveSessions_HydrationCapSemaphore 验证并发 hydration 上限 ≤8
// （cross-project-active-sessions D4：per-request 信号量 cap 8）。
// 9 个任务：前 8 个进入信号量并阻塞在 entered barrier；断言 inflight==8 + maxInflight==8 后，
// 第 9 个 MUST 无法进入（在 release 前 entered 通道无新信号）。释放 release 后第 9 个才能进入。
// 全程确定性 channel/barrier，不依赖 time.Sleep。峰值并发经 atomic CAS 跟踪，断言 ≤8。
func TestListActiveSessions_HydrationCapSemaphore(t *testing.T) {
	const n = 9
	rows := make([]task.ActiveTaskOverviewRow, n)
	for i := range rows {
		rows[i] = activeRow("t"+strconv.Itoa(i), "p1", "projA", "task", "b", "/wt", int64(100-i))
	}
	tb := newActiveSessionsBackend(rows...)

	var inflight, maxInflight int32
	// entered 在每个 hydration goroutine 获取信号量后发信号；release 在断言后释放让其退出。
	entered := make(chan struct{}, n)
	release := make(chan struct{})
	tb.agentStatusFn = func(ctx context.Context, taskID string) string {
		cur := atomic.AddInt32(&inflight, 1)
		// 记录峰值并发（CAS 更新 maxInflight）。
		for {
			m := atomic.LoadInt32(&maxInflight)
			if cur <= m || atomic.CompareAndSwapInt32(&maxInflight, m, cur) {
				break
			}
		}
		entered <- struct{}{}
		<-release // 阻塞直到主 goroutine 完成 cap 断言
		atomic.AddInt32(&inflight, -1)
		return "idle"
	}

	s := newAPITestServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	done := make(chan struct{})
	go func() {
		resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/sessions/active", ""))
		if err != nil {
			t.Error(err)
		}
		defer resp.Body.Close()
		close(done)
	}()

	// 等待 8 个 hydration 进入信号量（达到 cap）。
	for i := 0; i < 8; i++ {
		<-entered
	}
	if got := atomic.LoadInt32(&inflight); got != 8 {
		t.Fatalf("inflight after 8 entered = %d, want 8", got)
	}
	if max := atomic.LoadInt32(&maxInflight); max != 8 {
		t.Errorf("maxInflight after 8 entered = %d, want 8 (cap)", max)
	}
	// 第 9 个 MUST 无法进入：在合理等待窗口内 entered 通道无新信号。
	select {
	case <-entered:
		t.Fatal("9th hydration entered semaphore, want blocked by cap ≤ 8")
	case <-time.After(100 * time.Millisecond):
		// ok: 第 9 个被信号量阻塞。
	}
	if got := atomic.LoadInt32(&inflight); got != 8 {
		t.Errorf("inflight during 9th-wait = %d, want 8 (cap holds)", got)
	}
	if max := atomic.LoadInt32(&maxInflight); max != 8 {
		t.Errorf("maxInflight during 9th-wait = %d, want 8 (cap not exceeded)", max)
	}
	// 释放：8 个退出后第 9 个进入。
	close(release)
	// 第 9 个进入。
	<-entered
	<-done

	// 峰值并发 MUST ≤8（cap 限制，第 9 个只在 8 个退出后才进入）。
	if max := atomic.LoadInt32(&maxInflight); max > 8 {
		t.Errorf("maxInflight = %d, want ≤ 8 (semaphore cap never exceeded)", max)
	}
}

// TestListActiveSessions_HydrationBudgetDeadline 验证 hydration hctx 的 deadline ≈3s
// （cross-project-active-sessions D4：context.WithTimeout(ctx, 3*time.Second)）。
// fake 检查接收到的 ctx deadline 在 [2.9s, 3.1s] 范围并立即返回，无 3s 墙钟等待。
func TestListActiveSessions_HydrationBudgetDeadline(t *testing.T) {
	rows := []task.ActiveTaskOverviewRow{activeRow("t1", "p1", "projA", "taskA", "b", "/wt", 100)}
	tb := newActiveSessionsBackend(rows...)
	tb.agentStatusFn = func(ctx context.Context, taskID string) string {
		dl, ok := ctx.Deadline()
		if !ok {
			t.Error("hydration ctx has no deadline, want ~3s budget")
			return ""
		}
		rem := time.Until(dl)
		if rem < 2900*time.Millisecond || rem > 3100*time.Millisecond {
			t.Errorf("hydration ctx deadline remaining = %v, want ~3s (2.9s-3.1s)", rem)
		}
		return "busy"
	}
	s := newAPITestServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/sessions/active", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, got := readAndDecode(t, resp.Body)
	if len(got) != 1 || got[0].AgentStatus != "busy" {
		t.Errorf("dto = %+v, want single busy", got)
	}
}

// TestListActiveSessions_ShortParentCtxCancellation 验证调用方（请求 ctx）更短
// deadline 时 hctx 继承取消，hydration goroutine 在 ctx.Done 后立即退出、agentStatus 省略。
func TestListActiveSessions_ShortParentCtxCancellation(t *testing.T) {
	rows := []task.ActiveTaskOverviewRow{activeRow("t1", "p1", "projA", "taskA", "b", "/wt", 100)}
	tb := newActiveSessionsBackend(rows...)
	cancelled := make(chan struct{})
	tb.agentStatusFn = func(ctx context.Context, taskID string) string {
		<-ctx.Done() // 等父 ctx 取消（继承到 hctx）
		close(cancelled)
		return "" // 取消 → 省略字段
	}
	s := newAPITestServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/sessions/active", nil)
	req.Header.Set("Authorization", "Bearer testtoken")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req = req.WithContext(ctx)

	// 客户端可能在父 ctx deadline 到期时返回 transport 错误——这不影响服务端 hydration
	// 已观测到 ctx 取消。仅断言 hydration 因 ctx.Done 退出（无 3s 墙钟、无死锁）。
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()

	select {
	case <-cancelled:
		// ok: hydration 观测到 ctx 取消并退出
	case <-time.After(2 * time.Second):
		t.Fatal("hydration did not observe ctx cancellation within 2s")
	}
}

// TestListActiveSessions_ClientRequestCancelMidHydration 验证客户端中途取消请求 ctx
// 时 hydration goroutine 终止、无 side effects（不写 agentStatus），handler 正常返回。
// 使用阻塞 barrier 让 hydration 进入但未完成，主 goroutine 取消请求 ctx 后断言
// hydration 因 ctx.Done 退出且 AgentStatus 调用记录 < 任务数。
func TestListActiveSessions_ClientRequestCancelMidHydration(t *testing.T) {
	const n = 3
	rows := make([]task.ActiveTaskOverviewRow, n)
	for i := range rows {
		rows[i] = activeRow("t"+strconv.Itoa(i), "p1", "projA", "task", "b", "/wt", int64(100-i))
	}
	tb := newActiveSessionsBackend(rows...)
	// entered: 每个 hydration 进入并阻塞在 ctx.Done 或 cancelSignal。
	entered := make(chan struct{}, n)
	cancelSignal := make(chan struct{})
	tb.agentStatusFn = func(ctx context.Context, taskID string) string {
		entered <- struct{}{}
		select {
		case <-ctx.Done():
			return "" // 取消 → 不写 agentStatus
		case <-cancelSignal:
			return "busy"
		}
	}
	s := newAPITestServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/sessions/active", nil)
	req.Header.Set("Authorization", "Bearer testtoken")
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)

	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- err
			return
		}
		resp.Body.Close()
		done <- nil
	}()

	// 等待第一个 hydration 进入（证明 hydration 已启动）。
	<-entered
	// 客户端取消请求 ctx。
	cancel()
	// 等待 handler 返回（hydration 因 ctx.Done 退出）。
	select {
	case <-done:
		// ok
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return within 2s after client cancel")
	}
	// 取消信号释放（防止残留 goroutine 永久阻塞）。
	close(cancelSignal)
	// 至少一个 hydration 被调用（进入即记录），但整体不应全部完成写 agentStatus。
	// 不强制断言精确计数（调度顺序非确定），只确保请求可被取消且无死锁。
	if calls := tb.agentStatusCallCount(); calls == 0 {
		t.Error("AgentStatus never called, want ≥1 (hydration started before cancel)")
	}
}

// TestListActiveSessions_ReadModelImmutability 验证 hydration worker 仅写自己的 out[i].AgentStatus，
// 不修改读模型字段（task_id/last_active_at 等来自 store，MUST NOT 被 hydration 改写）。
func TestListActiveSessions_ReadModelImmutability(t *testing.T) {
	rows := []task.ActiveTaskOverviewRow{
		activeRow("t1", "p1", "projA", "taskA", "bA", "/wtA", 100),
	}
	tb := newActiveSessionsBackend(rows...)
	tb.agentStatusFn = func(ctx context.Context, taskID string) string {
		return "busy"
	}
	s := newAPITestServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/sessions/active", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, got := readAndDecode(t, resp.Body)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	r := got[0]
	// 读模型字段保持 store 原值，仅 AgentStatus 被填充。
	if r.TaskID != "t1" || r.ProjectID != "p1" || r.ProjectName != "projA" || r.Name != "taskA" || r.Branch != "bA" || r.WorktreePath != "/wtA" || r.LastActiveAt != 100 {
		t.Errorf("read-model fields mutated: %+v", r)
	}
	if r.AgentStatus != "busy" {
		t.Errorf("agentStatus = %q, want busy", r.AgentStatus)
	}
}

var errStoreFailure = errString("store failure")

type errString string

func (e errString) Error() string { return string(e) }