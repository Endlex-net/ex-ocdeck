// permission_mode_coordinator_test.go Manager.UpdateTaskPermissionMode 协调器
// （task-permission-mode tasks 4.2，design D4）：错误矩阵全行、同值零副作用、
// 转换矩阵全行经协调器（切入补判/切出 epoch+1/DR1 不动 epoch/无 runtime 仅持久化）、
// 事件时序（收敛先于发布）、PONR（请求取消不阻断收敛与发布）。
package task

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/application"
	ocdeckevent "ocdeck/internal/domain/event"
)

// --- 失败注入包装 store（透传其余方法给内嵌 mockStore） ---

// getTaskErrStore GetTask 注入错误（ErrNoRows 与其他读错误分流行）。
type getTaskErrStore struct {
	*mockStore
	err error
}

func (s *getTaskErrStore) GetTask(ctx context.Context, id string) (TaskRow, error) {
	if s.err != nil {
		return TaskRow{}, s.err
	}
	return s.mockStore.GetTask(ctx, id)
}

// updatePermModeStubStore UpdateTaskPermissionMode 注入错误/未命中并计数。
type updatePermModeStubStore struct {
	*mockStore
	err     error
	matched bool
	mu      sync.Mutex
	calls   int
}

func (s *updatePermModeStubStore) UpdateTaskPermissionMode(ctx context.Context, taskID, mode string) (application.MutationResult, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if s.err != nil {
		return application.MutationResult{}, s.err
	}
	if !s.matched {
		return application.MutationResult{}, nil
	}
	return s.mockStore.UpdateTaskPermissionMode(ctx, taskID, mode)
}

func (s *updatePermModeStubStore) updateCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func opCodeOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	var oe *application.OpError
	if !errors.As(err, &oe) {
		t.Fatalf("err %v is not *application.OpError", err)
	}
	return oe.Code
}

// TestUpdateTaskPermissionMode_ErrorMatrix 错误矩阵全行（D4）。
func TestUpdateTaskPermissionMode_ErrorMatrix(t *testing.T) {
	t.Run("trim 后非三值→invalid_input 零副作用", func(t *testing.T) {
		m, store, _ := newPermModeEnv(t, "ask", "ask")
		rec := &recordingPublisher{}
		m.publish = rec
		if _, err := m.UpdateTaskPermissionMode(context.Background(), "t1", "bogus"); opCodeOf(t, err) != application.CodeInvalidInput {
			t.Fatalf("code = %v", err)
		}
		if row, _ := store.GetTask(context.Background(), "t1"); row.PermissionMode != "ask" {
			t.Fatalf("persisted mode = %q, want ask（零副作用）", row.PermissionMode)
		}
		if got := len(rec.snapshot()); got != 0 {
			t.Fatalf("events = %d, want 0", got)
		}
	})
	t.Run("per-task 互斥竞争→conflict", func(t *testing.T) {
		m, _, _ := newPermModeEnv(t, "ask", "ask")
		unlock, err := m.tryLockTask("t1")
		if err != nil {
			t.Fatalf("prereq lock: %v", err)
		}
		defer unlock()
		if _, err := m.UpdateTaskPermissionMode(context.Background(), "t1", "ai-auto"); opCodeOf(t, err) != application.CodeConflict {
			t.Fatalf("code = %v", err)
		}
	})
	t.Run("行不存在 ErrNoRows→not_found", func(t *testing.T) {
		m, _, _ := newPermModeEnv(t, "ask", "ask")
		m.store = &getTaskErrStore{mockStore: m.store.(*mockStore), err: sql.ErrNoRows}
		if _, err := m.UpdateTaskPermissionMode(context.Background(), "t1", "ai-auto"); opCodeOf(t, err) != application.CodeNotFound {
			t.Fatalf("code = %v", err)
		}
	})
	t.Run("其他读错误→internal 不误包 404", func(t *testing.T) {
		m, _, _ := newPermModeEnv(t, "ask", "ask")
		m.store = &getTaskErrStore{mockStore: m.store.(*mockStore), err: errors.New("db boom")}
		if _, err := m.UpdateTaskPermissionMode(context.Background(), "t1", "ai-auto"); opCodeOf(t, err) != application.CodeInternal {
			t.Fatalf("code = %v", err)
		}
	})
	t.Run("持久化损坏→internal fail-closed", func(t *testing.T) {
		m, store, _ := newPermModeEnv(t, "ask", "ask")
		m.clearRuntime("t1")
		store.mutTask("t1", func(r *TaskRow) { r.PermissionMode = "bogus" })
		if _, err := m.UpdateTaskPermissionMode(context.Background(), "t1", "ai-auto"); opCodeOf(t, err) != application.CodeInternal {
			t.Fatalf("code = %v", err)
		}
	})
	t.Run("DB 提交失败→internal 零 runtime 副作用零事件", func(t *testing.T) {
		m, _, rt := newPermModeEnv(t, "ask", "ask")
		rec := &recordingPublisher{}
		m.publish = rec
		stub := &updatePermModeStubStore{mockStore: m.store.(*mockStore), err: errors.New("commit boom"), matched: true}
		m.store = stub
		if _, err := m.UpdateTaskPermissionMode(context.Background(), "t1", "ai-auto"); opCodeOf(t, err) != application.CodeInternal {
			t.Fatalf("code = %v", err)
		}
		rt.mu.Lock()
		epoch, aiAuto := rt.permEpoch, rt.permState.AIAutoEnabled
		rt.mu.Unlock()
		if epoch != 0 || aiAuto {
			t.Fatalf("epoch/AIAuto = %d/%v, want 0/false（提交失败零收敛）", epoch, aiAuto)
		}
		if got := len(rec.snapshot()); got != 0 {
			t.Fatalf("events = %d, want 0", got)
		}
	})
	t.Run("UPDATE 未命中→not_found", func(t *testing.T) {
		m, _, _ := newPermModeEnv(t, "ask", "ask")
		m.store = &updatePermModeStubStore{mockStore: m.store.(*mockStore), matched: false}
		if _, err := m.UpdateTaskPermissionMode(context.Background(), "t1", "ai-auto"); opCodeOf(t, err) != application.CodeNotFound {
			t.Fatalf("code = %v", err)
		}
	})
}

// TestUpdateTaskPermissionMode_SameValueZeroSideEffect 同值保存零副作用：不写库、
// 不收敛、不发事件，返回当前视图。
func TestUpdateTaskPermissionMode_SameValueZeroSideEffect(t *testing.T) {
	m, store, rt := newPermModeEnv(t, "ai-auto", "ai-auto")
	rec := &recordingPublisher{}
	m.publish = rec
	stub := &updatePermModeStubStore{mockStore: store, matched: true}
	m.store = stub

	view, err := m.UpdateTaskPermissionMode(context.Background(), "t1", "ai-auto")
	if err != nil {
		t.Fatalf("same-value save: %v", err)
	}
	if view != (application.PermissionModeView{PermissionMode: "ai-auto", EffectivePermissionMode: "ai-auto"}) {
		t.Fatalf("view = %+v", view)
	}
	if got := stub.updateCalls(); got != 0 {
		t.Fatalf("update calls = %d, want 0（同值不落库）", got)
	}
	rt.mu.Lock()
	epoch := rt.permEpoch
	rt.mu.Unlock()
	if epoch != 0 {
		t.Fatalf("epoch = %d, want 0（同值不开 epoch）", epoch)
	}
	if got := len(rec.snapshot()); got != 0 {
		t.Fatalf("events = %d, want 0", got)
	}
}

// TestUpdateTaskPermissionMode_TransitionMatrix 转换矩阵全行经协调器（D5 矩阵 +
// D7 事件职责）：收敛先于发布；切入补判；切出 epoch+1；DR1 不动 epoch 不补判；
// 无 runtime 仅持久化且只发 activity_changed。
func TestUpdateTaskPermissionMode_TransitionMatrix(t *testing.T) {
	t.Run("切入 ai-auto：epoch+1+补判+双事件", func(t *testing.T) {
		m, store, rt := newPermModeEnv(t, "ask", "ask")
		judge := newFakeJudge(PermissionVerdictApprove)
		m.judge = judge
		rt.setPermJudgeReady()
		// 预置 pending：切入补判经 judgeScan 纳入该请求。
		rt.ensureAttentionState().applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))
		rec := &recordingPublisher{}
		// 时序断言：activity_changed 发布时刻收敛（epoch 推进）必须已完成。
		convergedAtFirstEvent := false
		rec.onPublish = func(ev ocdeckevent.Event) {
			if ev.Type == ocdeckevent.TypeTaskActivityChanged {
				rt.mu.Lock()
				convergedAtFirstEvent = rt.permEpoch == 1 && rt.permState.AIAutoEnabled
				rt.mu.Unlock()
			}
		}
		m.publish = rec

		view, err := m.UpdateTaskPermissionMode(context.Background(), "t1", "ai-auto")
		if err != nil {
			t.Fatalf("switch-in: %v", err)
		}
		if view != (application.PermissionModeView{PermissionMode: "ai-auto", EffectivePermissionMode: "ai-auto"}) {
			t.Fatalf("view = %+v", view)
		}
		if !convergedAtFirstEvent {
			t.Fatal("runtime convergence must precede activity_changed publish")
		}
		events := rec.snapshot()
		if len(events) != 2 || events[0].Type != ocdeckevent.TypeTaskActivityChanged ||
			events[1].Type != ocdeckevent.TypeServeRuntimePermissionModeChanged {
			t.Fatalf("events = %+v, want [activity_changed, permission_mode_changed]", events)
		}
		if events[1].RID != string(rt.instVersion) {
			t.Fatalf("mode-changed RID = %q, want %q", events[1].RID, rt.instVersion)
		}
		eventually(t, 2*time.Second, func() bool { return len(judge.judgeCalls()) >= 1 })
		if row, _ := store.GetTask(context.Background(), "t1"); row.PermissionMode != "ai-auto" {
			t.Fatalf("persisted = %q, want ai-auto", row.PermissionMode)
		}
	})
	t.Run("切出：epoch+1 不补判", func(t *testing.T) {
		m, store, rt := newPermModeEnv(t, "ai-auto", "ai-auto")
		judge := newFakeJudge(PermissionVerdictApprove)
		m.judge = judge
		rt.setPermJudgeReady()
		rec := &recordingPublisher{}
		m.publish = rec

		view, err := m.UpdateTaskPermissionMode(context.Background(), "t1", "ask")
		if err != nil {
			t.Fatalf("switch-out: %v", err)
		}
		if view != (application.PermissionModeView{PermissionMode: "ask", EffectivePermissionMode: "ask"}) {
			t.Fatalf("view = %+v", view)
		}
		rt.mu.Lock()
		epoch := rt.permEpoch
		rt.mu.Unlock()
		if epoch != 1 {
			t.Fatalf("epoch = %d, want 1（切出推进）", epoch)
		}
		time.Sleep(50 * time.Millisecond)
		if got := len(judge.judgeCalls()); got != 0 {
			t.Fatalf("judge calls = %d, want 0（切出不补判）", got)
		}
		if got := len(rec.snapshot()); got != 2 {
			t.Fatalf("events = %d, want 2", got)
		}
		if row, _ := store.GetTask(context.Background(), "t1"); row.PermissionMode != "ask" {
			t.Fatalf("persisted = %q, want ask", row.PermissionMode)
		}
	})
	t.Run("DR1：--auto 进程保存 ai-auto 不动 epoch 不补判", func(t *testing.T) {
		m, store, rt := newPermModeEnv(t, "all-approve", "ask")
		judge := newFakeJudge(PermissionVerdictApprove)
		m.judge = judge
		rt.setPermJudgeReady()
		rec := &recordingPublisher{}
		m.publish = rec

		view, err := m.UpdateTaskPermissionMode(context.Background(), "t1", "ai-auto")
		if err != nil {
			t.Fatalf("DR1 save: %v", err)
		}
		if view != (application.PermissionModeView{PermissionMode: "ai-auto", EffectivePermissionMode: "all-approve"}) {
			t.Fatalf("view = %+v, want effective all-approve（DR1）", view)
		}
		rt.mu.Lock()
		epoch, aiAuto := rt.permEpoch, rt.permState.AIAutoEnabled
		rt.mu.Unlock()
		if epoch != 0 || aiAuto {
			t.Fatalf("epoch/AIAuto = %d/%v, want 0/false（DR1 不动 epoch）", epoch, aiAuto)
		}
		time.Sleep(50 * time.Millisecond)
		if got := len(judge.judgeCalls()); got != 0 {
			t.Fatalf("judge calls = %d, want 0（DR1 不补判）", got)
		}
		// 存在 runtime：真实变更双事件均发布（D7）。
		if got := len(rec.snapshot()); got != 2 {
			t.Fatalf("events = %d, want 2", got)
		}
		if row, _ := store.GetTask(context.Background(), "t1"); row.PermissionMode != "ai-auto" {
			t.Fatalf("persisted = %q, want ai-auto", row.PermissionMode)
		}
	})
	t.Run("无 runtime：仅持久化且只发 activity_changed", func(t *testing.T) {
		m, store, _ := newPermModeEnv(t, "ask", "ask")
		m.clearRuntime("t1")
		rec := &recordingPublisher{}
		m.publish = rec

		view, err := m.UpdateTaskPermissionMode(context.Background(), "t1", "ai-auto")
		if err != nil {
			t.Fatalf("no-runtime save: %v", err)
		}
		if view != (application.PermissionModeView{PermissionMode: "ai-auto", EffectivePermissionMode: "ai-auto"}) {
			t.Fatalf("view = %+v", view)
		}
		events := rec.snapshot()
		if len(events) != 1 || events[0].Type != ocdeckevent.TypeTaskActivityChanged {
			t.Fatalf("events = %+v, want only activity_changed（D7：无 runtime MUST NOT 发 mode-changed）", events)
		}
		if row, _ := store.GetTask(context.Background(), "t1"); row.PermissionMode != "ai-auto" {
			t.Fatalf("persisted = %q, want ai-auto", row.PermissionMode)
		}
	})
}

// TestUpdateTaskPermissionMode_PONR DB 提交后请求取消不阻断收敛与事件发布
// （收敛与最终视图读取均随 Manager 生命周期 ctx，D4 PONR）。
func TestUpdateTaskPermissionMode_PONR(t *testing.T) {
	m, store, rt := newPermModeEnv(t, "ask", "ask")
	rec := &recordingPublisher{}
	m.publish = rec

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 请求已取消：持久化原语不感知 ctx（秒级本地 SQLite），后续收敛 MUST 照常
	view, err := m.UpdateTaskPermissionMode(ctx, "t1", "ai-auto")
	if err != nil {
		t.Fatalf("PONR save: %v", err)
	}
	if view.EffectivePermissionMode != "ai-auto" {
		t.Fatalf("view = %+v", view)
	}
	rt.mu.Lock()
	epoch, aiAuto := rt.permEpoch, rt.permState.AIAutoEnabled
	rt.mu.Unlock()
	if epoch != 1 || !aiAuto {
		t.Fatalf("epoch/AIAuto = %d/%v, want 1/true（取消不阻断收敛）", epoch, aiAuto)
	}
	if got := len(rec.snapshot()); got != 2 {
		t.Fatalf("events = %d, want 2（取消不阻断发布）", got)
	}
	if row, _ := store.GetTask(context.Background(), "t1"); row.PermissionMode != "ai-auto" {
		t.Fatalf("persisted = %q, want ai-auto", row.PermissionMode)
	}
}
