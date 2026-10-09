package task

// charx_baseline_sse_test.go 落地阶段 0 基线组 fixture（tasks.md 1.2）：SSE 捕获与
// 并发 claim、session.updated 不创建归属、有效模式三分流、ownedOnly 对齐
// complete/overflow 前置判定与 session_overflow notice CAS、历史重复行 fail-closed。
//
// 有效性证据思路：事件经 fake server SSE → 真实 client 解析（envelope/字段校验）
// → startSSE 缓冲/实时派发 → handleSSEEvent → store 原子 claim/Touch。断言落在
// store 终态（归属集合/last_seen）与内存态（attention/agentStatus），能区分
// 「事件被解析并应用」与「静默忽略」两条路径的输出差异。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"ocdeck/internal/application"
)

// charxTimeout 是 fixture 轮询屏障的统一预算。
const charxTimeout = 5 * time.Second

// charxSessionLastSeen 持锁读取任务某 session 的 last_seen（SSE goroutine 并发写
// 安全）。
func (e *charxEnv) charxSessionLastSeen(taskID, sid string) (int64, bool) {
	rows, _ := e.store.ListTaskSessions(context.Background(), taskID)
	for _, r := range rows {
		if r.SessionID == sid {
			return r.LastSeenAt, true
		}
	}
	return 0, false
}

// --- 1.2 SSE 捕获与并发 claim ---

// TestCharx_SSEConcurrentClaim_SingleWinner 同一共享目录（dir 项目）两任务并发
// 订阅同一上游：session.created 广播后两任务并发原子 claim，仅一个成功。
//
// 覆盖边界（A4 说明）：本 fixture 覆盖的是 task 编排层——两任务的 SSE 管道并发
// 处理同一事件、冲突方按 SSE 语义忽略（不插入归属行、不影响状态与恢复预算）；
// mockStore 以全局 mutex 串行化 claim，其唯一性不能证明生产 SQLite 的并发原子性
// （后者由 store 包 TestClaimTaskSession_ConcurrentUniqueOwnership_RealSQLite
// 在真实 SQLite 上承担）。
//
// 完成屏障（A4）：S-race 之后广播两任务各自锚定的 session.updated 标记事件，
// 两标记均落库后，两任务的 SSE 管道必然都已处理完 S-race（单连接内事件有序）。
func TestCharx_SSEConcurrentClaim_SingleWinner(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{})
	charxSeedDirLocalPathTask(e.store, "t1", "pdir", "/shared/dir")
	charxSeedDirLocalPathTask(e.store, "t2", "pdir", "/shared/dir")

	if err := e.charxActivate("t1"); err != nil {
		t.Fatalf("Activate t1: %v", err)
	}
	if err := e.charxActivate("t2"); err != nil {
		t.Fatalf("Activate t2: %v", err)
	}
	anchor1 := e.charxTaskRow("t1").AnchorSessionID.String
	anchor2 := e.charxTaskRow("t2").AnchorSessionID.String
	e.srv.waitSubscribers(t, 2)

	// 广播 TUI 新建会话（directory 同源校验命中两任务的共享目录），
	// 随后广播两任务各自锚定的 updated 标记（1901/1902 > 锚定 last_seen 1001，
	// 保证单调守卫放行刷新）作为完成屏障。
	e.srv.broadcastEvent(t, charxSessionEvent("session.created", "S-race", "/shared/dir", 500, ""))
	e.srv.broadcastEvent(t, charxSessionEvent("session.updated", anchor1, "/shared/dir", 1901, ""))
	e.srv.broadcastEvent(t, charxSessionEvent("session.updated", anchor2, "/shared/dir", 1902, ""))
	charxWaitUntil(t, charxTimeout, "both tasks processed events past S-race (markers 1901/1902)", func() bool {
		s1, ok1 := e.charxSessionLastSeen("t1", anchor1)
		s2, ok2 := e.charxSessionLastSeen("t2", anchor2)
		return ok1 && ok2 && s1 == 1901 && s2 == 1902
	})

	// 恰好一个成功：获胜方持有 S-race，冲突方无行（忽略 + 诊断，不插入）。
	owned1, owned2 := e.charxOwnedSessions("t1"), e.charxOwnedSessions("t2")
	if owned1["S-race"] && owned2["S-race"] {
		t.Fatal("S-race claimed by BOTH tasks — uniqueness violated")
	}
	if !owned1["S-race"] && !owned2["S-race"] {
		t.Fatal("S-race claimed by NEITHER task")
	}
	// 双方状态、last_error 与恢复预算均不受冲突影响。
	for _, id := range []string{"t1", "t2"} {
		assertStatus(t, e.store, id, StatusActive)
		row := e.charxTaskRow(id)
		if row.LastError.Valid && row.LastError.String != "" {
			t.Errorf("%s last_error = %q, want empty", id, row.LastError.String)
		}
		if n := e.store.recoveryPermitCount(id); n != 0 {
			t.Errorf("%s recovery permits = %d, want 0", id, n)
		}
	}
}

// --- 1.2 session.updated 不创建归属 ---

// TestCharx_SessionUpdated_NoOwnershipCreation 未归属 session 的 updated 事件被
// 忽略（条件更新未命中，不插入归属行、不报错）；已归属 session 的 updated 事件
// 仅刷新 last_seen_at。
func TestCharx_SessionUpdated_NoOwnershipCreation(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{})
	charxSeedRepoWorktreeTask(e.store, "t1", "p1")
	if err := e.charxActivate("t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	anchor := e.charxTaskRow("t1").AnchorSessionID.String
	wt := "/data/worktrees/p1/t1"
	e.srv.waitSubscribers(t, 1)

	// 先未归属 updated，再已归属 updated（作同步屏障：后者落库后前者必然已处理）。
	e.srv.broadcastEvent(t, charxSessionEvent("session.updated", "U-unknown", wt, 999, ""))
	e.srv.broadcastEvent(t, charxSessionEvent("session.updated", anchor, wt, 4321, ""))
	charxWaitUntil(t, charxTimeout, "anchor last_seen refreshed to 4321", func() bool {
		seen, ok := e.charxSessionLastSeen("t1", anchor)
		return ok && seen == 4321
	})

	if owned := e.charxOwnedSessions("t1"); owned["U-unknown"] {
		t.Errorf("U-unknown MUST NOT be claimed by session.updated: %v", owned)
	}
	// 六观察面完整快照（A2）：updated 仅刷新已归属行 last_seen；无新归属、无副作用。
	got := e.captureCharx("updated", "t1")
	assertCharxMatches(t, "updated", charxSnapshot{
		tasks: []string{fmt.Sprintf(
			"t1|name=task t1|active|worktree|anchor=%s|last_port=PT1|delete_mode=-|last_error=|notices=[]|"+
				"env={OCDECK_PROJECT_PATH=/repo-p1,OCDECK_SERVE_PORT=<PORT>,OCDECK_TASK_BASE_BRANCH=main,"+
				"OCDECK_TASK_HEAD_BRANCH=ocdeck/t1,OCDECK_TASK_ID=t1,OCDECK_TASK_NAME=task t1,"+
				"OCDECK_TASK_PATH=/data/worktrees/p1/t1}", anchor)},
		sessions: []string{
			fmt.Sprintf("t1|%s|created=1001|first_seen=<TS>|last_seen=4321|parent=-", anchor),
		},
		owner: []string{"t1 -> [" + anchor + "]"},
		proc: []string{
			"new ocdeck-t1-runtime cwd=/data/worktrees/p1/t1 auth=pw(64) argv=opencode --port PT1 --hostname 127.0.0.1",
			"kill ocdeck-t1-runtime",
			fmt.Sprintf("new ocdeck-t1-runtime cwd=/data/worktrees/p1/t1 auth=pw(64) argv=opencode --port PT1 --hostname 127.0.0.1 --session %s", anchor),
		},
		notices: nil,
		dto:     []string{`t1|attention={"Permissions":[],"Questions":[]}|agentStatus=idle`},
	}, got)
}

// --- 1.2 有效模式三分流 ---

// TestCharx_AlignSplit_RepoWorktreeFullAlign repo+worktree：目录级全量对齐——
// 列表中全部 session（含非本任务创建的）被 claim 补记。
func TestCharx_AlignSplit_RepoWorktreeFullAlign(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{})
	charxSeedRepoWorktreeTask(e.store, "t1", "p1")
	wt := "/data/worktrees/p1/t1"
	e.srv.seedSession(wt, "S-other-1", 10, 100)
	e.srv.seedSession(wt, "S-other-2", 20, 200)

	if err := e.charxActivate("t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	anchor := e.charxTaskRow("t1").AnchorSessionID.String
	owned := e.charxOwnedSessions("t1")
	for _, want := range []string{"S-other-1", "S-other-2", anchor} {
		if !owned[want] {
			t.Errorf("worktree full align must claim %s: owned=%v", want, owned)
		}
	}
	// 六观察面完整快照（A2）。
	got := e.captureCharx("worktree-align", "t1")
	assertCharxMatches(t, "worktree-align", charxSnapshot{
		tasks: []string{fmt.Sprintf(
			"t1|name=task t1|active|worktree|anchor=%s|last_port=PT1|delete_mode=-|last_error=|notices=[]|"+
				"env={OCDECK_PROJECT_PATH=/repo-p1,OCDECK_SERVE_PORT=<PORT>,OCDECK_TASK_BASE_BRANCH=main,"+
				"OCDECK_TASK_HEAD_BRANCH=ocdeck/t1,OCDECK_TASK_ID=t1,OCDECK_TASK_NAME=task t1,"+
				"OCDECK_TASK_PATH=/data/worktrees/p1/t1}", anchor)},
		sessions: []string{
			"t1|S-other-1|created=10|first_seen=<TS>|last_seen=100|parent=-",
			"t1|S-other-2|created=20|first_seen=<TS>|last_seen=200|parent=-",
			fmt.Sprintf("t1|%s|created=1001|first_seen=<TS>|last_seen=1001|parent=-", anchor),
		},
		owner: []string{fmt.Sprintf("t1 -> [S-other-1 S-other-2 %s]", anchor)},
		proc: []string{
			"new ocdeck-t1-runtime cwd=/data/worktrees/p1/t1 auth=pw(64) argv=opencode --port PT1 --hostname 127.0.0.1",
			"kill ocdeck-t1-runtime",
			fmt.Sprintf("new ocdeck-t1-runtime cwd=/data/worktrees/p1/t1 auth=pw(64) argv=opencode --port PT1 --hostname 127.0.0.1 --session %s", anchor),
		},
		notices: nil,
		dto:     []string{`t1|attention={"Permissions":[],"Questions":[]}|agentStatus=idle`},
	}, got)
}

// TestCharx_AlignSplit_RepoLocalPathOwnedOnly repo+local-path：ownedOnly 对齐——
// 仅刷新 listed∩owned，绝不认领新 session；complete 时删除 owned 缺席行。
func TestCharx_AlignSplit_RepoLocalPathOwnedOnly(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{})
	charxSeedRepoLocalPathTask(e.store, "t1", "prep", "/shared/repo")
	dir := "/shared/repo"
	e.srv.seedSession(dir, "S-owned", 10, 700) // 本任务已归属
	e.srv.seedSession(dir, "S-other", 20, 800) // 他任务/手工 session（不可认领）
	// S-ghost：本任务归属但已不在上游（complete 时应删除）。
	e.store.sessions["t1"] = []SessionRow{
		{TaskID: "t1", SessionID: "S-owned", SessionCreatedAt: 10, FirstSeenAt: 1, LastSeenAt: 1},
		{TaskID: "t1", SessionID: "S-ghost", SessionCreatedAt: 30, FirstSeenAt: 1, LastSeenAt: 1},
	}

	if err := e.charxActivate("t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	anchor := e.charxTaskRow("t1").AnchorSessionID.String
	if owned := e.charxOwnedSessions("t1"); owned["S-other"] {
		t.Errorf("local-path ownedOnly align MUST NOT claim S-other: %v", owned)
	}
	// 六观察面完整快照（A2）：local-path 任务 env 无分支键（gitless），cwd=项目路径。
	got := e.captureCharx("localpath-align", "t1")
	assertCharxMatches(t, "localpath-align", charxSnapshot{
		tasks: []string{fmt.Sprintf(
			"t1|name=task t1|active|local-path|anchor=%s|last_port=PT1|delete_mode=-|last_error=|notices=[]|"+
				"env={OCDECK_PROJECT_PATH=/shared/repo,OCDECK_SERVE_PORT=<PORT>,OCDECK_TASK_ID=t1,"+
				"OCDECK_TASK_NAME=task t1,OCDECK_TASK_PATH=/shared/repo}", anchor)},
		sessions: []string{
			"t1|S-owned|created=10|first_seen=<TS>|last_seen=700|parent=-",
			fmt.Sprintf("t1|%s|created=1001|first_seen=<TS>|last_seen=1001|parent=-", anchor),
		},
		owner: []string{fmt.Sprintf("t1 -> [S-owned %s]", anchor)},
		proc: []string{
			"new ocdeck-t1-runtime cwd=/shared/repo auth=pw(64) argv=opencode --port PT1 --hostname 127.0.0.1",
			"kill ocdeck-t1-runtime",
			fmt.Sprintf("new ocdeck-t1-runtime cwd=/shared/repo auth=pw(64) argv=opencode --port PT1 --hostname 127.0.0.1 --session %s", anchor),
		},
		notices: nil,
		dto:     []string{`t1|attention={"Permissions":[],"Questions":[]}|agentStatus=idle`},
	}, got)
}

// TestCharx_AlignSplit_DirLocalPathOwnedOnly dir+local-path：与 repo+local-path
// 同为 ownedOnly（目录可共享），行为等价。
func TestCharx_AlignSplit_DirLocalPathOwnedOnly(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{})
	charxSeedDirLocalPathTask(e.store, "t1", "pdir", "/plain/dir")
	dir := "/plain/dir"
	e.srv.seedSession(dir, "S-owned", 10, 300)
	e.srv.seedSession(dir, "S-stranger", 20, 400)
	e.store.sessions["t1"] = []SessionRow{
		{TaskID: "t1", SessionID: "S-owned", SessionCreatedAt: 10, FirstSeenAt: 1, LastSeenAt: 1},
	}

	if err := e.charxActivate("t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	anchor := e.charxTaskRow("t1").AnchorSessionID.String
	if owned := e.charxOwnedSessions("t1"); owned["S-stranger"] {
		t.Errorf("dir ownedOnly align MUST NOT claim S-stranger: %v", owned)
	}
	// 六观察面完整快照（A2）：dir+local-path 与 repo+local-path 同为 ownedOnly
	//（env 无分支键、cwd=项目路径），行为等价。
	got := e.captureCharx("dir-align", "t1")
	assertCharxMatches(t, "dir-align", charxSnapshot{
		tasks: []string{fmt.Sprintf(
			"t1|name=task t1|active|local-path|anchor=%s|last_port=PT1|delete_mode=-|last_error=|notices=[]|"+
				"env={OCDECK_PROJECT_PATH=/plain/dir,OCDECK_SERVE_PORT=<PORT>,OCDECK_TASK_ID=t1,"+
				"OCDECK_TASK_NAME=task t1,OCDECK_TASK_PATH=/plain/dir}", anchor)},
		sessions: []string{
			"t1|S-owned|created=10|first_seen=<TS>|last_seen=300|parent=-",
			fmt.Sprintf("t1|%s|created=1001|first_seen=<TS>|last_seen=1001|parent=-", anchor),
		},
		owner: []string{fmt.Sprintf("t1 -> [S-owned %s]", anchor)},
		proc: []string{
			"new ocdeck-t1-runtime cwd=/plain/dir auth=pw(64) argv=opencode --port PT1 --hostname 127.0.0.1",
			"kill ocdeck-t1-runtime",
			fmt.Sprintf("new ocdeck-t1-runtime cwd=/plain/dir auth=pw(64) argv=opencode --port PT1 --hostname 127.0.0.1 --session %s", anchor),
		},
		notices: nil,
		dto:     []string{`t1|attention={"Permissions":[],"Questions":[]}|agentStatus=idle`},
	}, got)
}

// --- 1.2 ownedOnly 对齐 complete/overflow 前置判定与 session_overflow notice CAS ---

// charxNoticeConflictStore 是 notice CAS 冲突注入层（A5⑤）：首次携带
// session_overflow 的 CAS 写入尝试发生时，先向行内注入一条「并发 notice 写」
// （模拟读后写前的并发修改），再返回 CAS 失配——迫使 recordSessionOverflowNotice
// 走「重读 → 重新决策 → 追加」路径。若该路径覆盖而非追加并发 notice，断言即失败。
type charxNoticeConflictStore struct {
	*mockStore
	fired bool
}

func (s *charxNoticeConflictStore) UpdateTaskNoticeCAS(ctx context.Context, id string, expected, newNotice sql.NullString) (application.MutationResult, error) {
	if !s.fired && strings.Contains(newNotice.String, noticeCodeSessionOverflow) {
		s.fired = true
		competing := encodeNotices([]noticeEntry{{Code: "manual_concurrent", Message: "concurrent write", TS: 1}})
		_, _ = s.mockStore.UpdateTaskNotice(ctx, id, competing)
		// 返回失配（Matched=false）：expected 与被并发改写后的行值不符。
		return application.MutationResult{}, nil
	}
	return s.mockStore.UpdateTaskNoticeCAS(ctx, id, expected, newNotice)
}

// TestCharx_OwnedOnlyOverflow_NoticeWrittenThenCleared overflow（列表数==limit）：
// ① complete 判定基于原始列表数量（先于过滤）；② 事务外 CAS 写入 session_overflow
// notice；③ 不删缺席行（S-ghost 保留）、仅刷新候选集；④ 列表收缩后重连 complete
// 对齐在同一事务内删除缺席行并清除 notice。
//
// 注：锚定列表校验对 overflow 的既有失败语义保留（anchorPresentInList 将
// ErrSessionOverflow 当错误 → 激活期不允许列表==limit），故 overflow 经 SSE 断流
// 重连的对齐路径触发（锚定校验不参与）。
func TestCharx_OwnedOnlyOverflow_NoticeWrittenThenCleared(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{})
	charxSeedDirLocalPathTask(e.store, "t1", "pdir", "/big/dir")
	dir := "/big/dir"

	// 激活时上游仅锚定 session（count < limit，锚定校验通过、首次对齐 complete）。
	if err := e.charxActivate("t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	anchor := e.charxTaskRow("t1").AnchorSessionID.String

	// 激活后注入历史归属行（S-owned 候选 / S-ghost 缺席行），并把上游列表扩到
	// 999+锚定=1000 == limit → 重连对齐进入 overflow。
	e.store.mu.Lock()
	e.store.sessions["t1"] = append(e.store.sessions["t1"],
		SessionRow{TaskID: "t1", SessionID: "S0001", SessionCreatedAt: 1, FirstSeenAt: 1, LastSeenAt: 1},
		SessionRow{TaskID: "t1", SessionID: "S0500", SessionCreatedAt: 500, FirstSeenAt: 1, LastSeenAt: 1},
		SessionRow{TaskID: "t1", SessionID: "S-ghost", SessionCreatedAt: 9, FirstSeenAt: 1, LastSeenAt: 1})
	e.store.mu.Unlock()
	e.srv.mu.Lock()
	for i := 1; i <= 999; i++ {
		e.srv.sessions = append(e.srv.sessions, fakeSession{
			ID: fmt.Sprintf("S%04d", i), Directory: dir, Created: float64(i), Updated: 500,
		})
	}
	e.srv.mu.Unlock()

	// 断流 → 重连 → overflow 对齐。
	e.srv.breakStreams()
	charxWaitUntil(t, charxTimeout, "session_overflow notice written on reconnect align", func() bool {
		for _, ne := range e.charxNoticesOf("t1") {
			if ne.Code == noticeCodeSessionOverflow {
				return true
			}
		}
		return false
	})
	// overflow 不删缺席行；候选集（listed∩owned）刷新。
	if owned := e.charxOwnedSessions("t1"); !owned["S-ghost"] {
		t.Errorf("overflow align MUST NOT delete absent rows (S-ghost): %v", owned)
	}
	for _, sid := range []string{"S0001", "S0500"} {
		if seen, _ := e.charxSessionLastSeen("t1", sid); seen != 500 {
			t.Errorf("owned candidate %s last_seen = %d, want 500", sid, seen)
		}
	}
	for _, ne := range e.charxNoticesOf("t1") {
		if ne.Code == noticeCodeSessionOverflow && ne.Message != "session alignment reached limit; absent rows not pruned" {
			t.Errorf("overflow notice message = %q", ne.Message)
		}
	}

	// 上游列表收缩为 owned 子集 → 再次断流重连 → complete 对齐：删 S-ghost + 清 notice。
	e.srv.mu.Lock()
	e.srv.sessions = nil
	e.srv.sessions = append(e.srv.sessions,
		fakeSession{ID: "S0001", Directory: dir, Created: 1, Updated: 600},
		fakeSession{ID: "S0500", Directory: dir, Created: 500, Updated: 600},
		fakeSession{ID: anchor, Directory: dir, Created: 1001, Updated: 1001})
	e.srv.mu.Unlock()
	e.srv.breakStreams()

	charxWaitUntil(t, charxTimeout, "complete align clears session_overflow notice", func() bool {
		for _, ne := range e.charxNoticesOf("t1") {
			if ne.Code == noticeCodeSessionOverflow {
				return false
			}
		}
		return true
	})
	if owned := e.charxOwnedSessions("t1"); owned["S-ghost"] {
		t.Errorf("complete align must delete absent owned row S-ghost: %v", owned)
	}
	for _, sid := range []string{"S0001", "S0500"} {
		if seen, _ := e.charxSessionLastSeen("t1", sid); seen != 600 {
			t.Errorf("reconnect align must refresh %s to 600, got %d", sid, seen)
		}
	}
}

// TestCharx_OwnedOnlyOverflow_NoticeCASConflictPreservesConcurrentWrite（A5⑤）
// overflow notice 写入的 CAS 冲突输入：读后写前出现并发 notice 修改时，
// recordSessionOverflowNotice MUST 重读并以「追加」收敛（不得覆盖并发 notice）。
// 经 charxNoticeConflictStore 注入：首次 overflow CAS 尝试前写入并发 notice 并
// 返回失配；断言终态同时保留并发 notice 与 session_overflow（各一条）。
func TestCharx_OwnedOnlyOverflow_NoticeCASConflictPreservesConcurrentWrite(t *testing.T) {
	srv := newFakeOCServer(t)
	ms := newMockStore()
	ts := &charxNoticeConflictStore{mockStore: ms}
	e := newCharxEnvOnTaskStore(t, srv, ts, ms, newCharxProc(), charxEnvOpts{})
	charxSeedDirLocalPathTask(ms, "t1", "pdir", "/big/dir")
	dir := "/big/dir"

	if err := e.charxActivate("t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	// 上游扩到 1000 == limit → 重连对齐 overflow → 触发冲突注入。
	e.srv.mu.Lock()
	for i := 1; i <= 999; i++ {
		e.srv.sessions = append(e.srv.sessions, fakeSession{
			ID: fmt.Sprintf("S%04d", i), Directory: dir, Created: float64(i), Updated: 500,
		})
	}
	e.srv.mu.Unlock()
	e.srv.breakStreams()

	charxWaitUntil(t, charxTimeout, "conflict-injected overflow notice converges", func() bool {
		hasManual, hasOverflow := false, false
		for _, ne := range e.charxNoticesOf("t1") {
			if ne.Code == "manual_concurrent" {
				hasManual = true
			}
			if ne.Code == noticeCodeSessionOverflow {
				hasOverflow = true
			}
		}
		return hasManual && hasOverflow
	})
	if !ts.fired {
		t.Fatal("冲突注入未触发——fixture 未覆盖 CAS 失配路径")
	}
	// 终态计数：并发 notice 与 overflow 各恰好一条（追加收敛，非覆盖、非重复膨胀）。
	counts := map[string]int{}
	for _, ne := range e.charxNoticesOf("t1") {
		counts[ne.Code]++
	}
	if counts["manual_concurrent"] != 1 || counts[noticeCodeSessionOverflow] != 1 {
		t.Errorf("notices = %v, want manual_concurrent×1 + session_overflow×1 (追加收敛)", counts)
	}
}

// --- 1.2 历史重复行 fail-closed ---

// TestCharx_DuplicateOwnerRows_FailClosedOnStatusEvent 历史重复归属行（同一
// sessionID 挂两个任务）：session.status 事件经 lifecycle.OwnerOf 反查命中歧义 →
// fail-closed（handleSSEEvent 返回错误 → 统一 fatal 上报 → ensureRecovery 收敛）。
// 断言：① 歧义事件不推进归属行；② 运行时经自动重拉恢复（预算计数 ≥1）；
// ③ 恢复后新 SSE 流健康（后续有效事件照常应用）；④ 重复归属行不被修复。
func TestCharx_DuplicateOwnerRows_FailClosedOnStatusEvent(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{})
	charxSeedRepoWorktreeTask(e.store, "t1", "p1")
	charxSeedRepoWorktreeTask(e.store, "t2", "p2")
	if err := e.charxActivate("t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	wt := "/data/worktrees/p1/t1"
	// 预置历史重复归属：s-amb 同时挂 t1/t2（迁移前数据，不做启动修复）。上游列表
	// 同步放入 s-amb——避免恢复重连的 complete 对齐把「缺席 owned 行」当作删除目标
	//（对齐删缺席行是既有语义，与重复行 fail-closed 正交）。
	e.srv.seedSession(wt, "s-amb", 1, 500)
	e.store.mu.Lock()
	e.store.sessions["t1"] = append(e.store.sessions["t1"], SessionRow{TaskID: "t1", SessionID: "s-amb", SessionCreatedAt: 1, FirstSeenAt: 1, LastSeenAt: 1})
	e.store.sessions["t2"] = append(e.store.sessions["t2"], SessionRow{TaskID: "t2", SessionID: "s-amb", SessionCreatedAt: 1, FirstSeenAt: 1, LastSeenAt: 1})
	e.store.mu.Unlock()
	e.srv.waitSubscribers(t, 1)
	newServeBefore := countNewServe(e.proc.mockProc, "t1")

	// 歧义归属状态事件：OwnerOf typed ambiguity → handleSSEEvent 错误 → runtime
	// fatal 收敛 → ensureRecovery 自动重拉（fake server 健康 → 恢复成功回 active）。
	e.srv.broadcastEvent(t, charxStatusEvent("s-amb", "busy"))
	charxWaitUntil(t, charxTimeout, "ambiguous status event triggers recovery (permit acquired)", func() bool {
		return e.store.recoveryPermitCount("t1") >= 1
	})
	charxWaitUntil(t, charxTimeout, "task recovered back to active", func() bool {
		return e.charxTaskRow("t1").Status == StatusActive
	})
	if n := countNewServe(e.proc.mockProc, "t1"); n <= newServeBefore {
		t.Errorf("recovery must recreate the runtime process (new %d, before %d)", n, newServeBefore)
	}

	// 恢复后事件流健康：新 SSE 订阅上有效 created 事件照常应用。
	e.srv.waitSubscribers(t, 1)
	e.srv.broadcastEvent(t, charxSessionEvent("session.created", "S-after", wt, 777, ""))
	charxWaitUntil(t, charxTimeout, "valid event after recovery is applied", func() bool {
		return e.charxOwnedSessions("t1")["S-after"]
	})

	// 重复归属行保持存在（fail-closed 不修复历史）。t1 行的 last_seen 不被推进：
	// 歧义事件本身零写；恢复后的全量对齐尝试 claim s-amb 撞 t2 冲突 → 按对齐语义
	// 跳过（不刷新、不删除），t2 行同样不动。
	if !e.charxOwnedSessions("t1")["s-amb"] || !e.charxOwnedSessions("t2")["s-amb"] {
		t.Error("duplicate ownership rows must be preserved (no startup repair)")
	}
	for _, taskID := range []string{"t1", "t2"} {
		if seen, _ := e.charxSessionLastSeen(taskID, "s-amb"); seen != 1 {
			t.Errorf("%s s-amb last_seen = %d, want 1 (untouched)", taskID, seen)
		}
	}
}
