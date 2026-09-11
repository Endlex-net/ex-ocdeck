package uploads

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// --- AdmitUpload 任务初始准入（design D1 阶段①：404/409/500 语义三分支）---

func TestAdmitUpload(t *testing.T) {
	ctx := context.Background()

	t.Run("task_active_passes", func(t *testing.T) {
		tasks := newFakeTasks(map[string]fakeTask{"t1": {exists: true, active: true}})
		o := newTestOrchestrator(newFakeStore(), tasks, nil, nil)
		if err := o.AdmitUpload(ctx, "t1"); err != nil {
			t.Fatalf("AdmitUpload: %v", err)
		}
	})

	t.Run("task_missing_404", func(t *testing.T) {
		tasks := newFakeTasks(map[string]fakeTask{})
		o := newTestOrchestrator(newFakeStore(), tasks, nil, nil)
		if err := o.AdmitUpload(ctx, "gone"); !errors.Is(err, ErrUploadTaskNotFound) {
			t.Fatalf("err = %v, want ErrUploadTaskNotFound", err)
		}
	})

	t.Run("task_inactive_409", func(t *testing.T) {
		tasks := newFakeTasks(map[string]fakeTask{"t1": {exists: true, active: false}})
		o := newTestOrchestrator(newFakeStore(), tasks, nil, nil)
		if err := o.AdmitUpload(ctx, "t1"); !errors.Is(err, ErrUploadTaskInactive) {
			t.Fatalf("err = %v, want ErrUploadTaskInactive", err)
		}
	})

	t.Run("task_query_failure_infra_not_business", func(t *testing.T) {
		tasks := newFakeTasks(map[string]fakeTask{"t1": {exists: true, active: true}})
		tasks.err = errors.New("db down")
		o := newTestOrchestrator(newFakeStore(), tasks, nil, nil)
		err := o.AdmitUpload(ctx, "t1")
		if !errors.Is(err, errTaskQuery) {
			t.Fatalf("err = %v, want errTaskQuery (infra, 500 internal)", err)
		}
		if errors.Is(err, ErrUploadTaskNotFound) || errors.Is(err, ErrUploadTaskInactive) {
			t.Fatalf("infra failure must not map to business sentinel: %v", err)
		}
	})
}

// --- AdmitUploadConn connId 初始准入（design D1 阶段③：403 语义）---

func TestAdmitUploadConn(t *testing.T) {
	ctx := context.Background()
	tasks := newFakeTasks(map[string]fakeTask{"t1": {exists: true, active: true}})
	conns := &fakeConns{conns: map[string]string{"t1": testConnID}, found: true}

	t.Run("current_conn_passes", func(t *testing.T) {
		o := newTestOrchestrator(newFakeStore(), tasks, conns, nil)
		if err := o.AdmitUploadConn(ctx, "t1", testConnID); err != nil {
			t.Fatalf("AdmitUploadConn: %v", err)
		}
	})

	t.Run("stale_conn_forbidden", func(t *testing.T) {
		o := newTestOrchestrator(newFakeStore(), tasks, conns, nil)
		if err := o.AdmitUploadConn(ctx, "t1", "other-conn"); !errors.Is(err, ErrConnNotCurrent) {
			t.Fatalf("err = %v, want ErrConnNotCurrent", err)
		}
	})

	t.Run("no_current_tui_conn_forbidden", func(t *testing.T) {
		noConn := &fakeConns{conns: map[string]string{}, found: false}
		o := newTestOrchestrator(newFakeStore(), tasks, noConn, nil)
		if err := o.AdmitUploadConn(ctx, "t1", testConnID); !errors.Is(err, ErrConnNotCurrent) {
			t.Fatalf("err = %v, want ErrConnNotCurrent", err)
		}
	})

	t.Run("conns_port_nil_skips", func(t *testing.T) {
		o := newTestOrchestrator(newFakeStore(), tasks, nil, nil)
		if err := o.AdmitUploadConn(ctx, "t1", "any-conn"); err != nil {
			t.Fatalf("AdmitUploadConn with nil conns = %v, want nil (skip)", err)
		}
	})
}

// --- BindConnID 幂等绑定（仅允许从空值设置一次）---

func TestBindConnID(t *testing.T) {
	o := newTestOrchestrator(newFakeStore(), newFakeTasks(map[string]fakeTask{}), nil, nil)
	u, err := o.RegisterActiveUpload("t1", "")
	if err != nil {
		t.Fatal(err)
	}
	defer o.AbortUpload(context.Background(), u)

	if got := u.ConnID(); got != "" {
		t.Fatalf("ConnID after empty register = %q, want empty", got)
	}
	if err := o.BindConnID(u, testConnID); err != nil {
		t.Fatalf("BindConnID: %v", err)
	}
	if got := u.ConnID(); got != testConnID {
		t.Fatalf("ConnID = %q, want %q", got, testConnID)
	}
	if err := o.BindConnID(u, testConnID); err != nil {
		t.Fatalf("rebind same value must be idempotent, got %v", err)
	}
	if err := o.BindConnID(u, "other-conn"); !errors.Is(err, ErrConnAlreadyBound) {
		t.Fatalf("rebind different value err = %v, want ErrConnAlreadyBound", err)
	}
	if got := u.ConnID(); got != testConnID {
		t.Fatalf("ConnID after failed rebind = %q, want unchanged %q", got, testConnID)
	}
}

// --- 注册空 connID 的完整时序（design D1 阶段①→②→③：Register→读→Bind→准入）---

func TestAdmitSequence_RegisterEmptyConn_BindBeforeWrite(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	tasks := newFakeTasks(map[string]fakeTask{"t1": {exists: true, active: true}})
	conns := &fakeConns{conns: map[string]string{"t1": testConnID}, found: true}
	o := newTestOrchestrator(store, tasks, conns, nil)

	// ① 任务准入（先于请求体解析）。
	if err := o.AdmitUpload(ctx, "t1"); err != nil {
		t.Fatalf("AdmitUpload: %v", err)
	}
	// 登记（connId 未解析，空串）并模拟开始读请求体。
	u, err := o.RegisterActiveUpload("t1", "")
	if err != nil {
		t.Fatal(err)
	}
	defer o.AbortUpload(ctx, u)
	// ② 解析出首 part（connId + 文件名）。
	if err := o.BindConnID(u, testConnID); err != nil {
		t.Fatalf("BindConnID: %v", err)
	}
	o.BindOriginalName(u, "a.txt")
	// ③ connId 准入（先于文件落盘）。
	if err := o.AdmitUploadConn(ctx, "t1", u.ConnID()); err != nil {
		t.Fatalf("AdmitUploadConn: %v", err)
	}
	// 准入通过才写请求体；随后 finalize 全链路可用空注册登记走通。
	if _, err := o.WriteUploadBody(ctx, u, strings.NewReader(testBody)); err != nil {
		t.Fatalf("WriteUploadBody: %v", err)
	}
	if err := o.FinalizeUpload(ctx, u); err != nil {
		t.Fatalf("FinalizeUpload: %v", err)
	}
	o.mu.Lock()
	_, published := o.index[indexKey{taskID: "t1", uploadID: u.ID}]
	o.mu.Unlock()
	if !published {
		t.Fatal("upload must be published after full sequence")
	}
}
