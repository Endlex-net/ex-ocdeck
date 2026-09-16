// title_sync_test.go 覆盖会话标题同步 seam 生产实现（task-info-editable 3.2）：
// 无 runtime/锚定 no-op、成功清 notice、404 缓存降级、网络失败不缓存、instVersion 失效。
package task

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/infrastructure/opencode"
)

// seedTitleSyncTask 建 active 任务（锚定会话 ses_anchor）+ 活跃 runtime + serve 会话 env
// （taskOcClient 可用）。返回 (manager, runtime)。
func seedTitleSyncTask(t *testing.T, store *mockStore, proc *mockProc, oc *mockOC) (*Manager, *taskRuntime) {
	t.Helper()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main", Kind: ProjectKindRepo})
	store.tasks["t1"] = TaskRow{
		ID: "t1", ProjectID: "p1", Name: "Old", Branch: "ocdeck/old",
		Status: StatusActive, WorktreePath: "/wt/t1", BaseRef: "refs/heads/main",
		Mode: TaskModeWorktree, PermissionMode: PermissionModeAsk, CreatedAt: 1, UpdatedAt: 1,
		AnchorSessionID: sqlNullString("ses_anchor"),
	}
	serveName := runtimeSessionName("t1")
	if err := proc.NewSession(newSessionSpec(serveName, "/wt/t1", map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw",
		"OCDECK_SERVE_PORT":        "50001",
	}, []string{"sleep"})); err != nil {
		t.Fatal(err)
	}
	m := newTestManager(t, store, proc, newMockWorktree(), oc)
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	return m, rt
}

func TestTitleSync_NoRuntime_NoOp(t *testing.T) {
	store := newMockStore()
	proc := newMockProc()
	oc := newMockOC(true)
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main", Kind: ProjectKindRepo})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "Old", Status: StatusActive,
		WorktreePath: "/wt/t1", Mode: TaskModeWorktree, AnchorSessionID: sqlNullString("ses_anchor")}
	m := newTestManager(t, store, proc, newMockWorktree(), oc)

	m.syncSessionTitle(context.Background(), "t1", "New")
	if calls := oc.updateTitleCallsSnapshot(); len(calls) != 0 {
		t.Fatalf("no runtime must not PATCH, calls=%v", calls)
	}
	if _, ok := store.pendingOfNotice(noticeCodeTitleSync); ok {
		t.Fatal("no runtime must not write notice")
	}
}

func TestTitleSync_NoAnchor_NoOp(t *testing.T) {
	store := newMockStore()
	proc := newMockProc()
	oc := newMockOC(true)
	m, _ := seedTitleSyncTask(t, store, proc, oc)
	mut := store.tasks["t1"]
	mut.AnchorSessionID = sql.NullString{}
	store.tasks["t1"] = mut

	m.syncSessionTitle(context.Background(), "t1", "New")
	if calls := oc.updateTitleCallsSnapshot(); len(calls) != 0 {
		t.Fatalf("no anchor must not PATCH, calls=%v", calls)
	}
}

func TestTitleSync_Success_ClearsNotice(t *testing.T) {
	store := newMockStore()
	proc := newMockProc()
	oc := newMockOC(true)
	m, _ := seedTitleSyncTask(t, store, proc, oc)
	store.seedNotice("t1", noticeCodeTitleSync) // 预置既有失败提示

	m.syncSessionTitle(context.Background(), "t1", "New")
	calls := oc.updateTitleCallsSnapshot()
	if len(calls) != 1 || calls[0] != (updateTitleCall{Dir: "/wt/t1", ID: "ses_anchor", Title: "New"}) {
		t.Fatalf("calls = %v, want single PATCH to anchor with new title", calls)
	}
	if _, ok := store.pendingOfNotice(noticeCodeTitleSync); ok {
		t.Fatal("success must clear title sync notice")
	}
}

func TestTitleSync_Unsupported404_CachedDegrade(t *testing.T) {
	store := newMockStore()
	proc := newMockProc()
	oc := newMockOC(true)
	m, rt := seedTitleSyncTask(t, store, proc, oc)
	oc.updateTitleErrByID["ses_anchor"] = opencode.ErrSessionTitleUnsupported

	m.syncSessionTitle(context.Background(), "t1", "New")
	if _, ok := store.pendingOfNotice(noticeCodeTitleSync); !ok {
		t.Fatal("404 must write degrade notice")
	}
	if !m.titleCapabilityUnsupported("t1", string(rt.instVersion)) {
		t.Fatal("404 must cache unsupported for taskID+instVersion")
	}
	// 缓存命中：再次同步不重发 PATCH（能力降级直接记 notice）。
	m.syncSessionTitle(context.Background(), "t1", "New")
	if calls := oc.updateTitleCallsSnapshot(); len(calls) != 1 {
		t.Fatalf("cached unsupported must not re-PATCH, calls=%d", len(calls))
	}
}

func TestTitleSync_NetworkError_NotCached(t *testing.T) {
	store := newMockStore()
	proc := newMockProc()
	oc := newMockOC(true)
	m, rt := seedTitleSyncTask(t, store, proc, oc)
	oc.updateTitleErr = errors.New("connection reset")

	m.syncSessionTitle(context.Background(), "t1", "New")
	if _, ok := store.pendingOfNotice(noticeCodeTitleSync); !ok {
		t.Fatal("network failure must write notice")
	}
	if m.titleCapabilityUnsupported("t1", string(rt.instVersion)) {
		t.Fatal("network failure MUST NOT be cached")
	}
	// 未缓存：下次重试再次 PATCH；notice 按任务去重仍单条。
	m.syncSessionTitle(context.Background(), "t1", "New")
	if calls := oc.updateTitleCallsSnapshot(); len(calls) != 2 {
		t.Fatalf("calls = %d, want 2 (network error retried)", len(calls))
	}
	if n := store.countNotices(noticeCodeTitleSync); n != 1 {
		t.Fatalf("notice entries = %d, want 1 (deduped by task)", n)
	}
}

func TestTitleSync_InstVersionChange_InvalidatesCache(t *testing.T) {
	store := newMockStore()
	proc := newMockProc()
	oc := newMockOC(true)
	m, rt := seedTitleSyncTask(t, store, proc, oc)
	oc.updateTitleErrByID["ses_anchor"] = opencode.ErrSessionTitleUnsupported

	m.syncSessionTitle(context.Background(), "t1", "New")
	if calls := oc.updateTitleCallsSnapshot(); len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	// runtime 替换（instVersion 变化）→ 缓存失效，重新实测。
	newRt := m.newRuntime("t1")
	m.setRuntime("t1", newRt)
	if m.titleCapabilityUnsupported("t1", string(newRt.instVersion)) {
		t.Fatal("new instVersion must not inherit old cache")
	}
	m.syncSessionTitle(context.Background(), "t1", "New")
	if calls := oc.updateTitleCallsSnapshot(); len(calls) != 2 {
		t.Fatalf("calls = %d, want 2 (cache invalidated by instVersion change, old=%s new=%s)",
			len(calls), rt.instVersion, newRt.instVersion)
	}
}

// TestTitleSync_ConcurrentFirstProbe_SinglePATCH 并发首次能力判定（D3 singleflight 同
// registry 先例）：同任务同实例版本并发触发多次首次标题同步，leader 阻塞在 PATCH 内放大
// 并发窗口——等待者经 inflight 共享首测结果、迟到者命中首测后的稳定缓存，底层 client
// PATCH 恰好一次。
func TestTitleSync_ConcurrentFirstProbe_SinglePATCH(t *testing.T) {
	store := newMockStore()
	proc := newMockProc()
	oc := newMockOC(true)
	m, _ := seedTitleSyncTask(t, store, proc, oc)
	oc.updateTitleErrByID["ses_anchor"] = opencode.ErrSessionTitleUnsupported
	gate := make(chan struct{})
	oc.updateTitleBlock = gate

	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.syncSessionTitle(context.Background(), "t1", "New")
		}()
	}
	// 等 leader 已进入 PATCH（gate 内，计数已落）再放开，保证其余 goroutine 与首测并发重叠。
	deadline := time.Now().Add(2 * time.Second)
	for oc.updateTitleCountLoad() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(gate)
	wg.Wait()

	if got := oc.updateTitleCountLoad(); got != 1 {
		t.Fatalf("PATCH calls = %d, want 1 (inflight merge + first-probe stable cache)", got)
	}
	if n := store.countNotices(noticeCodeTitleSync); n != 1 {
		t.Fatalf("notice entries = %d, want 1 (all callers degrade, deduped by task)", n)
	}
}
