package uploads

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	ocdeckevent "ocdeck/internal/domain/event"
)

const (
	testUploadID = "0123456789abcdef0123456789abcdef"
	testConnID   = "conn-1"
	testBody     = "payload"
)

// --- fakes ---

type indexRef struct{ taskID, name string }

// fakeStore 内存 StorePort：状态化模拟（partials/files/sidecars）+ 错误注入 + 调用记录。
type fakeStore struct {
	mu       sync.Mutex
	partials map[string]string          // "task/name" -> content
	files    map[string]string          // "task/name" -> content
	sidecars map[string]Meta // "task/name" -> meta

	scanEntries []Entry // 非 nil 时 Scan 返回它
	// scanQueue 非 nil 时按次弹出返回（耗尽后重复末值）：构造「锁外扫描旧值 /
	// 锁内重扫新值」的屏障时序。
	scanQueue [][]Entry

	errNewID   error
	errCommit  error
	errStat    error
	errRefresh error
	// errRemoveTaskDir 注入整目录回收失败（下轮重试语义）。
	errRemoveTaskDir error
	seq              int

	commitCalls        []indexRef
	refreshCalls       []indexRef
	removePartialCalls []indexRef
	removeUploadCalls  []indexRef
	removePathCalls    []indexRef
	removeTaskDirCalls []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		partials: map[string]string{},
		files:    map[string]string{},
		sidecars: map[string]Meta{},
	}
}

func key(taskID, name string) indexRef { return indexRef{taskID: taskID, name: name} }

func (f *fakeStore) NewUploadID() (string, error) {
	if f.errNewID != nil {
		return "", f.errNewID
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	// 与真实 uploadID 同型：恒定 32 字符（27 前缀 + 5 位序号），前缀提取语义一致。
	return testUploadID[:27] + fmt.Sprintf("%05d", f.seq), nil
}

func (f *fakeStore) ManagedName(uploadID, originalName string) string {
	if strings.HasSuffix(originalName, ".txt") {
		return uploadID + ".txt"
	}
	return uploadID
}

func (f *fakeStore) WritePartial(ctx context.Context, taskID, name string, r io.Reader, onProgress func(n int64)) (int64, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	f.mu.Lock()
	f.partials[taskID+"/"+name] = string(data)
	f.mu.Unlock()
	return int64(len(data)), nil
}

func (f *fakeStore) CommitUpload(taskID string, meta Meta) error {
	f.mu.Lock()
	f.commitCalls = append(f.commitCalls, key(taskID, meta.Filename))
	err := f.errCommit
	f.mu.Unlock()
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	k := taskID + "/" + meta.Filename
	content, ok := f.partials[k]
	if !ok {
		return errors.New("fake: partial missing")
	}
	delete(f.partials, k)
	f.files[k] = content
	f.sidecars[k] = meta
	return nil
}

func (f *fakeStore) StatUpload(taskID, filename string) (bool, error) {
	if f.errStat != nil {
		return false, f.errStat
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.files[taskID+"/"+filename]
	return ok, nil
}

func (f *fakeStore) RefreshLastDelivered(taskID string, meta Meta, at time.Time) error {
	f.mu.Lock()
	f.refreshCalls = append(f.refreshCalls, key(taskID, meta.Filename))
	err := f.errRefresh
	f.mu.Unlock()
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	updated := meta
	updated.LastDeliveredAt = &at
	f.sidecars[taskID+"/"+meta.Filename] = updated
	return nil
}

func (f *fakeStore) RemoveUpload(taskID, filename string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeUploadCalls = append(f.removeUploadCalls, key(taskID, filename))
	delete(f.files, taskID+"/"+filename)
	delete(f.sidecars, taskID+"/"+filename)
	return nil
}

func (f *fakeStore) RemovePartial(taskID, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removePartialCalls = append(f.removePartialCalls, key(taskID, name))
	delete(f.partials, taskID+"/"+name)
	return nil
}

func (f *fakeStore) RemovePath(taskID, filename string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removePathCalls = append(f.removePathCalls, key(taskID, filename))
	delete(f.files, taskID+"/"+filename)
	return nil
}

func (f *fakeStore) RemoveTaskDir(taskID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeTaskDirCalls = append(f.removeTaskDirCalls, taskID)
	return f.errRemoveTaskDir
}

func (f *fakeStore) Scan() ([]Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.scanQueue != nil {
		if len(f.scanQueue) > 1 {
			out := f.scanQueue[0]
			f.scanQueue = f.scanQueue[1:]
			return out, nil
		}
		return f.scanQueue[0], nil
	}
	return f.scanEntries, nil
}

func (f *fakeStore) hasPartial(taskID, name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.partials[taskID+"/"+name]
	return ok
}

// fakeTasks 任务端口：状态表 + 查询错误注入 + 首查后翻转（屏障/竞争语义）。
type fakeTasks struct {
	mu    sync.Mutex
	tasks map[string]fakeTask
	err   error
	// flipAfterFirst 首次查询后该任务翻转为 not-exists（模拟生命周期提交先行）。
	flipAfterFirst map[string]bool
	calls          map[string]int
}

type fakeTask struct {
	exists, active bool
}

func newFakeTasks(tasks map[string]fakeTask) *fakeTasks {
	return &fakeTasks{tasks: tasks, flipAfterFirst: map[string]bool{}, calls: map[string]int{}}
}

func (f *fakeTasks) TaskActive(ctx context.Context, taskID string) (bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[taskID]++
	if f.err != nil {
		return false, false, f.err
	}
	if f.flipAfterFirst[taskID] {
		if f.calls[taskID] > 1 {
			delete(f.tasks, taskID)
		}
	}
	t := f.tasks[taskID]
	return t.exists, t.exists && t.active, nil
}

// setActive 翻转任务活跃态（模拟 Suspend/删除提交后状态）。
func (f *fakeTasks) setActive(taskID string, active bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.tasks[taskID]
	t.active = active
	f.tasks[taskID] = t
}

// callsCount 返回该任务的查询次数（事件消费/复查时序等待用）。
func (f *fakeTasks) callsCount(taskID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[taskID]
}

type fakeConns struct {
	conns map[string]string // taskID -> current connID
	found bool
}

func (f *fakeConns) CurrentTUIConn(ctx context.Context, taskID string) (string, bool, error) {
	c, ok := f.conns[taskID]
	return c, ok && f.found, nil
}

// fakeEvents/fakeSub 事件订阅端口。
type fakeEvents struct {
	sub *fakeSub
}

func (f *fakeEvents) Subscribe(topic ocdeckevent.Topic) EventSubscription {
	f.sub.events = make(chan ocdeckevent.Event, 8)
	f.sub.overflow = make(chan struct{}, 1)
	return f.sub
}

type fakeSub struct {
	events   chan ocdeckevent.Event
	overflow chan struct{}
	closed   sync.Once
}

func (s *fakeSub) C() <-chan ocdeckevent.Event { return s.events }
func (s *fakeSub) Overflow() <-chan struct{}   { return s.overflow }
func (s *fakeSub) Close()                      { s.closed.Do(func() { close(s.events) }) }

// --- 构造辅助 ---

func newTestOrchestrator(store *fakeStore, tasks *fakeTasks, conns ConnPort, mutate func(*Options)) *Orchestrator {
	opts := Options{
		Cfg:   Config{MaxBytes: 1 << 20},
		Store: store,
		Tasks: tasks,
		Conns: conns,
		Logf:  func(string, ...any) {},
	}
	if mutate != nil {
		mutate(&opts)
	}
	return New(opts)
}

// commitUpload 走真实注册→绑定→写体→finalize 链路提交一个上传，返回 uploadID。
func commitUpload(t *testing.T, o *Orchestrator, taskID, connID string, body string) string {
	t.Helper()
	u, err := o.RegisterActiveUpload(taskID, connID)
	if err != nil {
		t.Fatalf("RegisterActiveUpload: %v", err)
	}
	o.BindOriginalName(u, "a.txt")
	if _, err := o.WriteUploadBody(context.Background(), u, strings.NewReader(body)); err != nil {
		t.Fatalf("WriteUploadBody: %v", err)
	}
	if err := o.FinalizeUpload(context.Background(), u); err != nil {
		t.Fatalf("FinalizeUpload: %v", err)
	}
	return u.ID
}

// --- Deliver 七步准入 ---

func TestDeliver_SevenSteps(t *testing.T) {
	ctx := context.Background()
	tasks := newFakeTasks(map[string]fakeTask{
		"t1": {exists: true, active: true},
		"t2": {exists: true, active: true},
	})
	conns := &fakeConns{conns: map[string]string{"t1": testConnID, "t2": testConnID}, found: true}

	t.Run("step7_success_and_refresh", func(t *testing.T) {
		store := newFakeStore()
		o := newTestOrchestrator(store, tasks, conns, func(o *Options) { o.Cfg.Retention = time.Hour })
		id := commitUpload(t, o, "t1", testConnID, testBody)
		injects := 0
		ok, code := o.Deliver(ctx, "t1", testConnID, id, func(ctx context.Context) error { injects++; return nil })
		if !ok || code != "" {
			t.Fatalf("Deliver = (%v, %q), want (true, \"\")", ok, code)
		}
		if injects != 1 {
			t.Errorf("injects = %d, want 1", injects)
		}
		if len(store.refreshCalls) != 1 {
			t.Errorf("refresh calls = %d, want 1 (TTL enabled)", len(store.refreshCalls))
		}
	})

	t.Run("step1_forbidden_conn_not_current", func(t *testing.T) {
		store := newFakeStore()
		o := newTestOrchestrator(store, tasks, conns, nil)
		id := commitUpload(t, o, "t1", testConnID, testBody)
		ok, code := o.Deliver(ctx, "t1", "other-conn", id, func(ctx context.Context) error {
			t.Fatal("inject must not run")
			return nil
		})
		_ = id
		if ok || code != CodeForbidden {
			t.Fatalf("Deliver = (%v, %q), want (false, forbidden)", ok, code)
		}
	})

	t.Run("step1_forbidden_no_current_tui_shell_frame", func(t *testing.T) {
		store := newFakeStore()
		noConn := &fakeConns{conns: map[string]string{}, found: false}
		o := newTestOrchestrator(store, tasks, noConn, nil)
		ok, code := o.Deliver(ctx, "t1", testConnID, "whatever", func(ctx context.Context) error {
			t.Fatal("inject must not run")
			return nil
		})
		if ok || code != CodeForbidden {
			t.Fatalf("Deliver = (%v, %q), want (false, forbidden)", ok, code)
		}
	})

	t.Run("step2_task_inactive_beats_step3", func(t *testing.T) {
		store := newFakeStore()
		o := newTestOrchestrator(store, tasks, conns, nil)
		id := commitUpload(t, o, "t2", testConnID, testBody)
		tasks.setActive("t2", false) // 生命周期提交先行：任务离开 active
		ok, code := o.Deliver(ctx, "t2", testConnID, id, func(ctx context.Context) error {
			t.Fatal("inject must not run")
			return nil
		})
		if ok || code != CodeTaskInactive {
			t.Fatalf("Deliver = (%v, %q), want (false, task_inactive)", ok, code)
		}
	})

	t.Run("step3_not_found", func(t *testing.T) {
		store := newFakeStore()
		o := newTestOrchestrator(store, tasks, conns, nil)
		ok, code := o.Deliver(ctx, "t1", testConnID, "nosuchid", func(ctx context.Context) error {
			t.Fatal("inject must not run")
			return nil
		})
		if ok || code != CodeNotFound {
			t.Fatalf("Deliver = (%v, %q), want (false, not_found)", ok, code)
		}
	})

	t.Run("step4_forbidden_ownership", func(t *testing.T) {
		store := newFakeStore()
		// conns=nil：跳过 step①，隔离校验 step④ 归属判定（Lane B 接入前的形态）。
		o := newTestOrchestrator(store, tasks, nil, nil)
		id := commitUpload(t, o, "t1", testConnID, testBody)
		ok, code := o.Deliver(ctx, "t1", "other-conn", id, func(ctx context.Context) error {
			t.Fatal("inject must not run")
			return nil
		})
		if ok || code != CodeForbidden {
			t.Fatalf("Deliver = (%v, %q), want (false, forbidden)", ok, code)
		}
	})

	t.Run("step5_expired_boundary_clock_injected", func(t *testing.T) {
		store := newFakeStore()
		retention := time.Hour
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		cur := now
		o := newTestOrchestrator(store, tasks, conns, func(o *Options) {
			o.Cfg.Retention = retention
			o.Clock = func() time.Time { return cur }
		})
		id := commitUpload(t, o, "t1", testConnID, testBody) // UploadedAt = now
		inject := func(ctx context.Context) error { return nil }

		// now < expiresAt → 可投递；成功后 lastDeliveredAt 刷新为该时刻并同步索引。
		cur = now.Add(retention - time.Nanosecond)
		ok, code := o.Deliver(ctx, "t1", testConnID, id, inject)
		if !ok || code != "" {
			t.Fatalf("Deliver before expiresAt = (%v, %q), want (true, \"\")", ok, code)
		}
		// 刷新后基准后移（R2）：now==原 expiresAt 仍可投递（旧实现沿用 uploadedAt
		// 基准会在此误判 expired——本断言固化修复后语义）。
		cur = now.Add(retention)
		ok, code = o.Deliver(ctx, "t1", testConnID, id, inject)
		if !ok || code != "" {
			t.Fatalf("Deliver at original expiresAt after refresh = (%v, %q), want (true, \"\")", ok, code)
		}
		// 新基准 + retention 到点：now==expiresAt → 过期。
		cur = now.Add(2 * retention)
		ok, code = o.Deliver(ctx, "t1", testConnID, id, inject)
		if ok || code != CodeExpired {
			t.Fatalf("Deliver at refreshed expiresAt = (%v, %q), want (false, expired)", ok, code)
		}
	})

	t.Run("step5_refresh_failure_keeps_last_successful_base", func(t *testing.T) {
		store := newFakeStore()
		retention := time.Hour
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		cur := now
		o := newTestOrchestrator(store, tasks, conns, func(o *Options) {
			o.Cfg.Retention = retention
			o.Clock = func() time.Time { return cur }
		})
		id := commitUpload(t, o, "t1", testConnID, testBody)
		inject := func(ctx context.Context) error { return nil }

		// 首次成功投递：基准推进到 T1（now+30m）。
		cur = now.Add(30 * time.Minute)
		if ok, code := o.Deliver(ctx, "t1", testConnID, id, inject); !ok || code != "" {
			t.Fatalf("first Deliver = (%v, %q), want success", ok, code)
		}
		// 后续刷新全部失败：索引必须沿用最近一次成功基准 T1，不得被失败尝试推进。
		store.errRefresh = errors.New("disk io")
		// T1+retention-1ns 仍可投递（基准 T1）；若失败尝试错误地推进了基准，此处会误判 expired。
		cur = now.Add(90*time.Minute - time.Nanosecond)
		ok, code := o.Deliver(ctx, "t1", testConnID, id, inject)
		if !ok || code != "" {
			t.Fatalf("Deliver with failing refresh = (%v, %q), want success (base must stay at last success)", ok, code)
		}
		// 到点过期：基准 T1 → expiresAt = T1+retention = now+90m。
		cur = now.Add(90 * time.Minute)
		ok, code = o.Deliver(ctx, "t1", testConnID, id, inject)
		if ok || code != CodeExpired {
			t.Fatalf("Deliver at refreshed-base expiry = (%v, %q), want (false, expired)", ok, code)
		}
	})

	t.Run("step6_file_missing_not_found", func(t *testing.T) {
		store := newFakeStore()
		o := newTestOrchestrator(store, tasks, conns, nil)
		id := commitUpload(t, o, "t1", testConnID, testBody)
		if err := store.RemoveUpload("t1", id+".txt"); err != nil {
			t.Fatal(err)
		}
		ok, code := o.Deliver(ctx, "t1", testConnID, id, func(ctx context.Context) error {
			t.Fatal("inject must not run")
			return nil
		})
		if ok || code != CodeNotFound {
			t.Fatalf("Deliver = (%v, %q), want (false, not_found)", ok, code)
		}
	})

	t.Run("step6_forbidden_runes_invalid_input", func(t *testing.T) {
		store := newFakeStore()
		o := newTestOrchestrator(store, tasks, conns, nil)
		id := commitUpload(t, o, "t1", testConnID, testBody)
		// 注入非法路径形态的索引项（真实索引项不可能出现；spec ⑥ 决策步骤仍必须拒绝）。
		o.mu.Lock()
		meta := o.index[indexKey{taskID: "t1", uploadID: id}]
		meta.Filename = "bad\\name.txt"
		o.index[indexKey{taskID: "t1", uploadID: id}] = meta
		o.mu.Unlock()
		ok, code := o.Deliver(ctx, "t1", testConnID, id, func(ctx context.Context) error {
			t.Fatal("inject must not run")
			return nil
		})
		if ok || code != CodeInvalidInput {
			t.Fatalf("Deliver = (%v, %q), want (false, invalid_input)", ok, code)
		}
	})

	t.Run("step7_write_failed", func(t *testing.T) {
		store := newFakeStore()
		o := newTestOrchestrator(store, tasks, conns, nil)
		id := commitUpload(t, o, "t1", testConnID, testBody)
		ok, code := o.Deliver(ctx, "t1", testConnID, id, func(ctx context.Context) error {
			return errors.New("pty write short")
		})
		if ok || code != CodeWriteFailed {
			t.Fatalf("Deliver = (%v, %q), want (false, write_failed)", ok, code)
		}
	})

	t.Run("infra_task_query_failure_zero_code", func(t *testing.T) {
		store := newFakeStore()
		asks := newFakeTasks(map[string]fakeTask{"t1": {exists: true, active: true}})
		o := newTestOrchestrator(store, asks, conns, nil)
		id := commitUpload(t, o, "t1", testConnID, testBody)
		asks.err = errors.New("db down") // 提交后查询故障：投递侧按基础设施故障收口
		ok, code := o.Deliver(ctx, "t1", testConnID, id, func(ctx context.Context) error {
			t.Fatal("inject must not run on infra failure")
			return nil
		})
		if ok || code != "" {
			t.Fatalf("Deliver = (%v, %q), want (false, \"\") infra", ok, code)
		}
	})

	t.Run("infra_stat_failure_zero_code", func(t *testing.T) {
		store := newFakeStore()
		o := newTestOrchestrator(store, tasks, conns, nil)
		id := commitUpload(t, o, "t1", testConnID, testBody)
		store.errStat = errors.New("stat io error")
		ok, code := o.Deliver(ctx, "t1", testConnID, id, func(ctx context.Context) error {
			t.Fatal("inject must not run on infra failure")
			return nil
		})
		if ok || code != "" {
			t.Fatalf("Deliver = (%v, %q), want (false, \"\") infra", ok, code)
		}
	})

	t.Run("refresh_failure_keeps_ok", func(t *testing.T) {
		store := newFakeStore()
		o := newTestOrchestrator(store, tasks, conns, func(o *Options) { o.Cfg.Retention = time.Hour })
		id := commitUpload(t, o, "t1", testConnID, testBody)
		store.errRefresh = errors.New("sidecar rewrite failed")
		ok, code := o.Deliver(ctx, "t1", testConnID, id, func(ctx context.Context) error { return nil })
		if !ok || code != "" {
			t.Fatalf("Deliver = (%v, %q), want (true, \"\") despite refresh failure", ok, code)
		}
	})
}

// --- Lane B 注入路径契约 ---

// TestDeliver_InjectCtxCarriesManagedAbsPath 成功路径：inject 收到的 ctx 经
// InjectPathFrom 返回受管最终绝对路径（<UploadDir>/<taskID>/<受管名>），且该路径
// 真实存在于磁盘（非客户端原始名）。
func TestDeliver_InjectCtxCarriesManagedAbsPath(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := newFakeStore()
	tasks := newFakeTasks(map[string]fakeTask{"t1": {exists: true, active: true}})
	conns := &fakeConns{conns: map[string]string{"t1": testConnID}, found: true}
	o := newTestOrchestrator(store, tasks, conns, func(o *Options) { o.Cfg.UploadDir = dir })
	id := commitUpload(t, o, "t1", testConnID, testBody) // 绑定名 <id>.txt

	// fakeStore 为内存态：按 infrastructure 物理布局在磁盘落最终文件，验证注入路径可 stat。
	wantPath := filepath.Join(dir, "t1", id+".txt")
	if err := os.MkdirAll(filepath.Dir(wantPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wantPath, []byte(testBody), 0o600); err != nil {
		t.Fatal(err)
	}

	gotPath, gotOK := "", false
	ok, code := o.Deliver(ctx, "t1", testConnID, id, func(ctx context.Context) error {
		gotPath, gotOK = InjectPathFrom(ctx)
		return nil
	})
	if !ok || code != "" {
		t.Fatalf("Deliver = (%v, %q), want (true, \"\")", ok, code)
	}
	if !gotOK {
		t.Fatal("inject ctx must carry inject path")
	}
	if gotPath != wantPath {
		t.Errorf("inject path = %s, want %s", gotPath, wantPath)
	}
	if _, err := os.Stat(gotPath); err != nil {
		t.Errorf("injected path must exist on disk: %v", err)
	}
}

// TestInjectPathFrom_NotSet 未注入路径的 ctx 报告 not-set（私有 key 防跨包伪造）。
func TestInjectPathFrom_NotSet(t *testing.T) {
	if _, ok := InjectPathFrom(context.Background()); ok {
		t.Fatal("InjectPathFrom must report not-set on plain ctx")
	}
}

// TestDeliver_InjectPathFinalCheck 验证步骤⑦对 filepath.Join 后的最终绝对路径
// 复查（R13）：配置根/taskID 组合引入的反斜杠/控制字符 → invalid_input、零注入
//（配置加载期校验不能替代投递时复查）。
func TestDeliver_InjectPathFinalCheck(t *testing.T) {
	ctx := context.Background()
	t.Run("upload_dir_with_control_char", func(t *testing.T) {
		store := newFakeStore()
		tasks := newFakeTasks(map[string]fakeTask{"t1": {exists: true, active: true}})
		o := newTestOrchestrator(store, tasks, nil, func(o *Options) {
			o.Cfg.UploadDir = "/data\x01uploads" // 异常配置直接注入 Orchestrator.Config
		})
		id := commitUpload(t, o, "t1", testConnID, testBody)
		ok, code := o.Deliver(ctx, "t1", testConnID, id, func(ctx context.Context) error {
			t.Fatal("inject must not run for rejected final path")
			return nil
		})
		if ok || code != CodeInvalidInput {
			t.Fatalf("Deliver = (%v, %q), want (false, invalid_input)", ok, code)
		}
	})

	t.Run("task_id_with_control_char", func(t *testing.T) {
		store := newFakeStore()
		tasks := newFakeTasks(map[string]fakeTask{"t\x01x": {exists: true, active: true}})
		o := newTestOrchestrator(store, tasks, nil, func(o *Options) {
			o.Cfg.UploadDir = filepath.Join(t.TempDir(), "uploads")
		})
		// 手工注入索引项（文件名合法、taskID 非法：组合路径必须在⑦被拒）。
		badID := strings.Repeat("9", 32)
		o.mu.Lock()
		o.index[indexKey{taskID: "t\x01x", uploadID: badID}] = Meta{
			UploadID: badID, TaskID: "t\x01x", ConnID: testConnID,
			Filename: badID + ".txt", UploadedAt: time.Now().UTC(),
		}
		o.mu.Unlock()
		store.files["t\x01x/"+badID+".txt"] = "x"

		ok, code := o.Deliver(ctx, "t\x01x", testConnID, badID, func(ctx context.Context) error {
			t.Fatal("inject must not run for rejected final path")
			return nil
		})
		if ok || code != CodeInvalidInput {
			t.Fatalf("Deliver = (%v, %q), want (false, invalid_input)", ok, code)
		}
	})
}

// TestDeliver_CrossTaskUploadID_Forbidden 验证七步顺序（R14）：任务 B 用任务 A 的
// uploadId 不在 ③ 直接 not_found，而是进入 ④ 归属判定返回 forbidden（七步顺序冻结，
// 跨任务引用不得绕过归属判定）。
func TestDeliver_CrossTaskUploadID_Forbidden(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	tasks := newFakeTasks(map[string]fakeTask{
		"tA": {exists: true, active: true},
		"tB": {exists: true, active: true},
	})
	o := newTestOrchestrator(store, tasks, nil, nil) // conns=nil：隔离 ④
	idA := commitUpload(t, o, "tA", testConnID, testBody)

	ok, code := o.Deliver(ctx, "tB", testConnID, idA, func(ctx context.Context) error {
		t.Fatal("inject must not run for cross-task uploadId")
		return nil
	})
	if ok || code != CodeForbidden {
		t.Fatalf("cross-task Deliver = (%v, %q), want (false, forbidden)", ok, code)
	}

	// 未知 uploadId 仍走 ③ not_found（查找语义本身不回归）。
	ok, code = o.Deliver(ctx, "tB", testConnID, strings.Repeat("e", 32), func(ctx context.Context) error {
		t.Fatal("inject must not run")
		return nil
	})
	if ok || code != CodeNotFound {
		t.Fatalf("unknown uploadId Deliver = (%v, %q), want (false, not_found)", ok, code)
	}
}

// --- 上传侧 ---

func TestWriteUploadBody_TooLarge(t *testing.T) {
	store := newFakeStore()
	tasks := newFakeTasks(map[string]fakeTask{"t1": {exists: true, active: true}})
	o := newTestOrchestrator(store, tasks, nil, func(o *Options) { o.Cfg.MaxBytes = 10 })
	u, err := o.RegisterActiveUpload("t1", testConnID)
	if err != nil {
		t.Fatal(err)
	}
	o.BindOriginalName(u, "a.txt")
	_, err = o.WriteUploadBody(context.Background(), u, strings.NewReader("0123456789"))
	if err != nil {
		t.Fatalf("10 bytes should fit: %v", err)
	}
	u2, err := o.RegisterActiveUpload("t1", testConnID)
	if err != nil {
		t.Fatal(err)
	}
	o.BindOriginalName(u2, "a.txt")
	if _, err := o.WriteUploadBody(context.Background(), u2, strings.NewReader("0123456789A")); !errors.Is(err, ErrUploadTooLarge) {
		t.Fatalf("11 bytes err = %v, want ErrUploadTooLarge", err)
	}
	o.AbortUpload(context.Background(), u2)
	o.AbortUpload(context.Background(), u)
}

func TestWriteUploadBody_NotBound(t *testing.T) {
	o := newTestOrchestrator(newFakeStore(), newFakeTasks(map[string]fakeTask{"t1": {exists: true, active: true}}), nil, nil)
	u, err := o.RegisterActiveUpload("t1", testConnID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.WriteUploadBody(context.Background(), u, strings.NewReader("x")); !errors.Is(err, ErrUploadNotBound) {
		t.Fatalf("err = %v, want ErrUploadNotBound", err)
	}
	o.AbortUpload(context.Background(), u)
}

func TestRegisterActiveUpload_NewIDError(t *testing.T) {
	store := newFakeStore()
	store.errNewID = errors.New("rand fail")
	o := newTestOrchestrator(store, newFakeTasks(map[string]fakeTask{}), nil, nil)
	if _, err := o.RegisterActiveUpload("t1", testConnID); !errors.Is(err, store.errNewID) {
		t.Fatalf("err = %v, want rand fail", err)
	}
}

// --- FinalizeUpload 复查与提交 ---

func TestFinalizeUpload_RecheckOrder(t *testing.T) {
	ctx := context.Background()
	tasks := newFakeTasks(map[string]fakeTask{
		"t1": {exists: true, active: true},
		"t2": {exists: true, active: false},
		"gone": {exists: false},
	})
	conns := &fakeConns{conns: map[string]string{"t1": testConnID, "t2": "other-conn"}, found: true}

	t.Run("success_publishes_index", func(t *testing.T) {
		store := newFakeStore()
		o := newTestOrchestrator(store, tasks, conns, nil)
		if err := o.FinalizeUpload(ctx, mustBoundUpload(t, o, "t1", testConnID)); err != nil {
			t.Fatalf("FinalizeUpload: %v", err)
		}
		o.mu.Lock()
		published := len(o.index)
		o.mu.Unlock()
		if published != 1 {
			t.Fatalf("index size = %d, want 1", published)
		}
	})

	t.Run("task_missing_beats_conn_mismatch", func(t *testing.T) {
		store := newFakeStore()
		o := newTestOrchestrator(store, tasks, conns, nil)
		u := mustBoundUpload(t, o, "gone", "other-conn") // conn 也不匹配，验证存在性先行
		if err := o.FinalizeUpload(ctx, u); !errors.Is(err, ErrUploadTaskNotFound) {
			t.Fatalf("err = %v, want ErrUploadTaskNotFound", err)
		}
	})

	t.Run("inactive_beats_conn_mismatch", func(t *testing.T) {
		store := newFakeStore()
		o := newTestOrchestrator(store, tasks, conns, nil)
		u := mustBoundUpload(t, o, "t2", testConnID)
		if err := o.FinalizeUpload(ctx, u); !errors.Is(err, ErrUploadTaskInactive) {
			t.Fatalf("err = %v, want ErrUploadTaskInactive", err)
		}
	})

	t.Run("conn_mismatch", func(t *testing.T) {
		store := newFakeStore()
		o := newTestOrchestrator(store, tasks, conns, nil)
		u := mustBoundUpload(t, o, "t1", "other-conn")
		if err := o.FinalizeUpload(ctx, u); !errors.Is(err, ErrConnNotCurrent) {
			t.Fatalf("err = %v, want ErrConnNotCurrent", err)
		}
	})

	t.Run("commit_error_no_publish_partial_kept", func(t *testing.T) {
		store := newFakeStore()
		store.errCommit = errors.New("disk full")
		o := newTestOrchestrator(store, tasks, conns, nil)
		u := mustBoundUpload(t, o, "t1", testConnID)
		if err := o.FinalizeUpload(ctx, u); !errors.Is(err, store.errCommit) {
			t.Fatalf("err = %v, want commit error", err)
		}
		o.mu.Lock()
		published := len(o.index)
		o.mu.Unlock()
		if published != 0 {
			t.Fatalf("index size = %d, want 0 (sidecar failed, no uploadId)", published)
		}
		if !store.hasPartial("t1", u.Filename()) {
			t.Fatal("partial must be kept for AbortUpload cleanup")
		}
		o.AbortUpload(ctx, u)
		if store.hasPartial("t1", u.Filename()) {
			t.Fatal("AbortUpload must remove partial after failed finalize")
		}
	})
}

// mustBoundUpload 构造已绑定文件名并写入 partial 的活跃上传（connID 注册期给定，
// 可传非当前连接模拟归属不匹配）。
func mustBoundUpload(t *testing.T, o *Orchestrator, taskID, connID string) *ActiveUpload {
	t.Helper()
	u, err := o.RegisterActiveUpload(taskID, connID)
	if err != nil {
		t.Fatal(err)
	}
	o.BindOriginalName(u, "a.txt")
	if _, err := o.WriteUploadBody(context.Background(), u, strings.NewReader(testBody)); err != nil {
		t.Fatal(err)
	}
	return u
}

// --- 停滞取消与 finalize 竞争 ---

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not reached before timeout")
}

func TestStallCancel_PreventsFinalizePublish(t *testing.T) {
	store := newFakeStore()
	tasks := newFakeTasks(map[string]fakeTask{"t1": {exists: true, active: true}})
	o := newTestOrchestrator(store, tasks, nil, func(o *Options) { o.StallTimeout = 20 * time.Millisecond })
	u := mustBoundUpload(t, o, "t1", testConnID)
	// 停滞计时器触发 → 取消收尾①（标记 + cancelCh）。
	<-u.CancelCh()
	if err := o.FinalizeUpload(context.Background(), u); !errors.Is(err, ErrUploadStalled) {
		t.Fatalf("finalize err = %v, want ErrUploadStalled", err)
	}
	o.mu.Lock()
	published := len(o.index)
	o.mu.Unlock()
	if published != 0 {
		t.Fatalf("index size = %d, want 0 (cancelled upload must not publish)", published)
	}
	// 收尾③在 finalize markDone 后删除 .partial。
	waitFor(t, 2*time.Second, func() bool { return !store.hasPartial("t1", u.Filename()) })
}

func TestStallCancel_FinalizeWins_NoRemoval(t *testing.T) {
	store := newFakeStore()
	tasks := newFakeTasks(map[string]fakeTask{"t1": {exists: true, active: true}})
	o := newTestOrchestrator(store, tasks, nil, func(o *Options) { o.StallTimeout = 50 * time.Millisecond })
	id := commitUpload(t, o, "t1", testConnID, testBody)
	time.Sleep(100 * time.Millisecond) // 计时器迟到触发
	commit, _, removePartial, _, _, _ := storeCounts(store)
	if commit != 1 || removePartial != 0 {
		t.Fatalf("commit=%d removePartial=%d, want committed upload and no partial removal", commit, removePartial)
	}
	if _, ok := o.index[indexKey{taskID: "t1", uploadID: id}]; !ok {
		t.Fatal("upload must stay published after late timer")
	}
}

// storeCounts 读取 fakeStore 计数（commit/refresh/removePartial/removeUpload/removePath/removeTaskDir）。
func storeCounts(f *fakeStore) (int, int, int, int, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.commitCalls), len(f.refreshCalls), len(f.removePartialCalls),
		len(f.removeUploadCalls), len(f.removePathCalls), len(f.removeTaskDirCalls)
}

func TestAbortUpload_Idempotent(t *testing.T) {
	store := newFakeStore()
	tasks := newFakeTasks(map[string]fakeTask{"t1": {exists: true, active: true}})
	o := newTestOrchestrator(store, tasks, nil, nil)
	u := mustBoundUpload(t, o, "t1", testConnID)
	o.AbortUpload(context.Background(), u)
	o.AbortUpload(context.Background(), u)
	_, _, removePartial, _, _, _ := storeCounts(store)
	if removePartial != 1 {
		t.Fatalf("removePartial calls = %d, want 1 (idempotent)", removePartial)
	}
	select {
	case <-u.DoneCh():
	default:
		t.Fatal("done channel must be closed after AbortUpload")
	}
}

// --- 生命周期提交并发屏障（2.5 语义在 application 侧的体现） ---

func TestBarrier_LifecycleCommitFirst_DeliverZeroInjectAndNoPublish(t *testing.T) {
	store := newFakeStore()
	tasks := newFakeTasks(map[string]fakeTask{"t1": {exists: true, active: true}})
	tasks.flipAfterFirst["t1"] = true // 首查（commitUpload 内）后任务消失 = 删除提交先行
	o := newTestOrchestrator(store, tasks, nil, nil)
	id := commitUpload(t, o, "t1", testConnID, testBody)

	// Deliver：任务查询见 not-exists → task_inactive，零注入。
	injects := 0
	ok, code := o.Deliver(context.Background(), "t1", testConnID, id, func(ctx context.Context) error {
		injects++
		return nil
	})
	if ok || code != CodeTaskInactive {
		t.Fatalf("Deliver = (%v, %q), want (false, task_inactive)", ok, code)
	}
	if injects != 0 {
		t.Fatalf("injects = %d, want 0 (提交先行时投递零注入)", injects)
	}

	// Finalize：同一复查路径，不发布 uploadId。
	u := mustBoundUpload(t, o, "t1", testConnID)
	if err := o.FinalizeUpload(context.Background(), u); !errors.Is(err, ErrUploadTaskNotFound) {
		t.Fatalf("finalize err = %v, want ErrUploadTaskNotFound", err)
	}
	o.mu.Lock()
	published := len(o.index)
	o.mu.Unlock()
	if published != 1 { // 仅 barrier 前已提交的那一条
		t.Fatalf("index size = %d, want 1 (finalize must not publish)", published)
	}
}

// --- 停滞取消与协调锁屏障：生命周期提交先行时 finalize 阻塞等锁、复查不发布 ---

func TestBarrier_FinalizeBlockedByCoordinationLock_RechecksTask(t *testing.T) {
	store := newFakeStore()
	tasks := newFakeTasks(map[string]fakeTask{"t1": {exists: true, active: true}})
	o := newTestOrchestrator(store, tasks, nil, nil)

	// 模拟 Manager 生命周期提交先行持锁（锁顺序：任务锁 → 协调锁，此处仅协调锁侧）。
	holdRelease, err := o.Coordination().Acquire(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	u := mustBoundUpload(t, o, "t1", testConnID)

	finalizeDone := make(chan error, 1)
	go func() { finalizeDone <- o.FinalizeUpload(context.Background(), u) }()

	// finalize 阻塞在协调锁上（未复查、未提交）。
	select {
	case err := <-finalizeDone:
		t.Fatalf("finalize returned before lock released: %v", err)
	case <-time.After(30 * time.Millisecond):
	}

	// 锁持有期间任务被删除（删除提交先行）。
	tasks.mu.Lock()
	delete(tasks.tasks, "t1")
	tasks.mu.Unlock()
	holdRelease()

	// finalize 拿锁后复查必见任务已删，不发布。
	select {
	case err := <-finalizeDone:
		if !errors.Is(err, ErrUploadTaskNotFound) {
			t.Fatalf("finalize err = %v, want ErrUploadTaskNotFound", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("finalize did not complete after lock release")
	}
	o.mu.Lock()
	published := len(o.index)
	o.mu.Unlock()
	if published != 0 {
		t.Fatalf("index size = %d, want 0", published)
	}
}

// --- 停滞进度覆盖全部请求体读取（R6：非零读取即刷新 + 超时回调复核重排） ---

// TestTouchProgress_ContinuousReadsAcrossStallTimeout_NoCancel：持续小块进度跨越
// 初始停滞阈值不误取消（每次非零读取刷新计时器），finalize 正常提交。
func TestTouchProgress_ContinuousReadsAcrossStallTimeout_NoCancel(t *testing.T) {
	store := newFakeStore()
	tasks := newFakeTasks(map[string]fakeTask{"t1": {exists: true, active: true}})
	o := newTestOrchestrator(store, tasks, nil, func(o *Options) { o.StallTimeout = 60 * time.Millisecond })
	u, err := o.RegisterActiveUpload("t1", testConnID)
	if err != nil {
		t.Fatal(err)
	}
	defer o.AbortUpload(context.Background(), u)
	// 模拟请求体逐块非零读取，总时长 200ms 跨越初始阈值（60ms）的 3 倍。
	for i := 0; i < 20; i++ {
		time.Sleep(10 * time.Millisecond)
		u.TouchProgress()
		select {
		case <-u.CancelCh():
			t.Fatal("continuous progress must not trigger stall cancel")
		default:
		}
	}
	// 未被标记不可提交：finalize 正常提交。
	o.BindOriginalName(u, "a.txt")
	if _, err := o.WriteUploadBody(context.Background(), u, strings.NewReader(testBody)); err != nil {
		t.Fatalf("WriteUploadBody: %v", err)
	}
	if err := o.FinalizeUpload(context.Background(), u); err != nil {
		t.Fatalf("FinalizeUpload: %v", err)
	}
}

// TestStallCancel_LockWaitNewProgress_RearmsTimer：计时器触发后回调等协调锁期间
// 收到新字节 → 取得锁后复核发现新进度，重新安排计时、不取消；重排后的计时器在
// 无后续进度时按新基准正常触发取消（重排生效，不是永久静默）。
func TestStallCancel_LockWaitNewProgress_RearmsTimer(t *testing.T) {
	store := newFakeStore()
	tasks := newFakeTasks(map[string]fakeTask{"t1": {exists: true, active: true}})
	o := newTestOrchestrator(store, tasks, nil, func(o *Options) { o.StallTimeout = 120 * time.Millisecond })
	// 先持协调锁：模拟生命周期提交先行持锁，令触发的停滞回调阻塞在锁上。
	release, err := o.Coordination().Acquire(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	u, err := o.RegisterActiveUpload("t1", testConnID)
	if err != nil {
		t.Fatal(err)
	}
	defer o.AbortUpload(context.Background(), u)
	time.Sleep(150 * time.Millisecond) // 计时器触发，回调阻塞在协调锁上
	u.TouchProgress()                  // 等锁期间收到新字节（真实进度）
	release()
	// 重排窗口（< 120ms）内不取消。
	time.Sleep(30 * time.Millisecond)
	select {
	case <-u.CancelCh():
		t.Fatal("new progress during lock wait must prevent stall cancel")
	default:
	}
	// 无后续进度：重排后的计时器按新基准触发取消。
	select {
	case <-u.CancelCh():
	case <-time.After(2 * time.Second):
		t.Fatal("re-armed stall timer did not fire")
	}
}
