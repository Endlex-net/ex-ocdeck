// uploads_wiring_test.go 验证 terminal-file-paste-drop 2.4 组合根接线：经生产装配函数
// newUploadOrchestrator 构造的编排器（真实 infra uploads.Store + sqlite adapter TaskPort +
// eventbus 适配）在 TaskPort 映射、启动扫描、task.deleted 回收、TTL 过期清理与 Stop
// 幂等上的端到端行为。
package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appuploads "ocdeck/internal/application/uploads"
	"ocdeck/internal/config"
	ocdeckevent "ocdeck/internal/domain/event"
	ocdecktask "ocdeck/internal/domain/task"
	"ocdeck/internal/infrastructure/eventbus"
	"ocdeck/internal/infrastructure/sqlite"
	uploads "ocdeck/internal/infrastructure/uploads"
	"ocdeck/internal/infrastructure/store"
)

// wiringUploadConfig 构造仅含上传三字段的 config（组合根等价：字段经
// newUploadOrchestrator 注入编排器）。
func wiringUploadConfig(dir string, retention time.Duration) *config.Config {
	return &config.Config{
		UploadDir:       dir,
		UploadMaxBytes:  config.DefaultUploadMaxBytes,
		UploadRetention: retention,
	}
}

// commitWiredUpload 经真实 infra store 完成一次受管提交（.partial 流式写 → rename →
// sidecar），返回最终受管文件名。
func commitWiredUpload(t *testing.T, ctx context.Context, st *uploads.Store, taskID string) string {
	t.Helper()
	id, err := st.NewUploadID()
	if err != nil {
		t.Fatalf("new upload id: %v", err)
	}
	name := st.ManagedName(id, "报告.txt")
	if _, err := st.WritePartial(ctx, taskID, name, strings.NewReader("hello"), nil); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	if err := st.CommitUpload(taskID, appuploads.Meta{
		UploadID: id, TaskID: taskID, ConnID: "conn-1", Filename: name, UploadedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("commit upload: %v", err)
	}
	return name
}

// waitForDirGone 轮询等待目录被回收（事件消费是异步 goroutine）。
func waitForDirGone(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task dir not reclaimed within 2s: %s", path)
}

// TestUploadWiring_NullSidecarStartupRecovery 验证真实启动恢复链（G1）：磁盘上
// 手写合法 null sidecar（不经 CommitUpload，模拟升级前/手动布局）经 StartupScan
// 入索引后可投递。
func TestUploadWiring_NullSidecarStartupRecovery(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := seedTaskForSubmissions(ctx, db); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	repo := sqlite.New(db)
	if _, err := repo.UpdateTaskStatus(ctx, "t1", ocdecktask.StatusActive, nil); err != nil {
		t.Fatalf("activate task: %v", err)
	}

	// 纯手写磁盘布局：配对文件 + lastDeliveredAt 为 JSON null 的 sidecar。
	uploadID := strings.Repeat("ab", 16)
	name := uploadID + ".txt"
	taskDir := filepath.Join(dir, "t1")
	if err := os.MkdirAll(taskDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, name), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	sidecar := `{"uploadId":"` + uploadID + `","taskId":"t1","connId":"conn-1","filename":"` + name + `","uploadedAt":"2026-01-01T00:00:00Z","lastDeliveredAt":null}`
	if err := os.WriteFile(filepath.Join(taskDir, name+".meta.json"), []byte(sidecar), 0o600); err != nil {
		t.Fatal(err)
	}

	// Conns 数据源与 sidecar 的 connId 对齐，聚焦 G1 恢复链路。
	orch := newUploadOrchestrator(wiringUploadConfig(dir, 0), repo, eventbus.New(),
		&currentConnPort{src: &fakeCurrentConnSource{id: "conn-1", ok: true}})
	if err := orch.StartupScan(ctx); err != nil {
		t.Fatalf("startup scan: %v", err)
	}

	ok, code := orch.Deliver(ctx, "t1", "conn-1", uploadID, func(ctx context.Context) error { return nil })
	if !ok || code != "" {
		t.Fatalf("Deliver after startup recovery = (%v, %q), want (true, \"\") — null sidecar must not be treated as corrupt", ok, code)
	}
}

// fakeCurrentConnSource 是组合根 Conns 数据源的 fake（*api.Server 同型）。
type fakeCurrentConnSource struct {
	id    string
	ok    bool
	calls int
}

func (f *fakeCurrentConnSource) CurrentTUIConnID(taskID string) (string, bool) {
	f.calls++
	return f.id, f.ok
}

// TestUploadWiring_CurrentConnPort 验证 currentConnPort 适配映射：HTTP 服务构造前
//（未回填）按未注册处理 ("",false,nil)；setServer 回填后透传数据源结果。
func TestUploadWiring_CurrentConnPort(t *testing.T) {
	ctx := context.Background()
	p := &currentConnPort{}
	if id, ok, err := p.CurrentTUIConn(ctx, "t1"); id != "" || ok || err != nil {
		t.Fatalf("unbound port = (%q,%v,%v), want (\"\",false,nil)", id, ok, err)
	}

	src := &fakeCurrentConnSource{id: "conn-1", ok: true}
	p.setServer(src)
	id, ok, err := p.CurrentTUIConn(ctx, "t1")
	if err != nil || !ok || id != "conn-1" {
		t.Fatalf("bound port = (%q,%v,%v), want (\"conn-1\",true,nil)", id, ok, err)
	}
	if src.calls != 1 {
		t.Fatalf("source calls = %d, want 1", src.calls)
	}
}

// TestUploadWiring_DeliverConnCurrentity 验证接线后 Deliver ①连接当前性真实生效
//（不再跳过）：connID 与数据源当前值匹配才注入；不匹配返回 forbidden 且零注入。
func TestUploadWiring_DeliverConnCurrentity(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := seedTaskForSubmissions(ctx, db); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	repo := sqlite.New(db)
	if _, err := repo.UpdateTaskStatus(ctx, "t1", ocdecktask.StatusActive, nil); err != nil {
		t.Fatalf("activate task: %v", err)
	}

	st := uploads.NewStore(dir)
	name := commitWiredUpload(t, ctx, st, "t1")
	uploadID := name[:32] // 受管命名钉死：32hex uploadID 为文件名前缀

	// 数据源当前值与提交时 meta.ConnID（commitWiredUpload 固定 conn-1）一致：
	// ①连接当前性与 ④归属检查对同一请求 connID 串联放行。
	src := &fakeCurrentConnSource{id: "conn-1", ok: true}
	orch := newUploadOrchestrator(wiringUploadConfig(dir, 0), repo, eventbus.New(), &currentConnPort{src: src})
	if err := orch.StartupScan(ctx); err != nil {
		t.Fatalf("startup scan: %v", err)
	}

	injections := 0
	inject := func(ctx context.Context) error { injections++; return nil }

	ok, code := orch.Deliver(ctx, "t1", "conn-1", uploadID, inject)
	if !ok || code != "" || injections != 1 {
		t.Fatalf("matching conn Deliver = (%v,%q), injections=%d, want (true,\"\",1)", ok, code, injections)
	}
	if src.calls == 0 {
		t.Fatal("Conns port must be consulted for deliver admission")
	}

	ok, code = orch.Deliver(ctx, "t1", "conn-stale", uploadID, inject)
	if ok || code != appuploads.CodeForbidden || injections != 1 {
		t.Fatalf("stale conn Deliver = (%v,%q), injections=%d, want (false,forbidden,1)", ok, code, injections)
	}
}

// TestUploadWiring_TaskViewPortAdapter 验证组合根 TaskPort 适配的状态准入映射：
// 仅 status==active 视为活跃；ErrTaskNotFound 归一化为 (false,false,nil)。
func TestUploadWiring_TaskViewPortAdapter(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := seedTaskForSubmissions(ctx, db); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	p := uploadTaskViewPort{inner: sqlite.New(db)}

	// seeded 行 status=suspended：存在但不活跃。
	exists, active, err := p.TaskActive(ctx, "t1")
	if err != nil || !exists || active {
		t.Fatalf("suspended task = (%v,%v,%v), want (true,false,nil)", exists, active, err)
	}

	// 未命中任务归一化为 (false,false,nil)，不返回 ErrTaskNotFound。
	exists, active, err = p.TaskActive(ctx, "nope")
	if err != nil || exists || active {
		t.Fatalf("missing task = (%v,%v,%v), want (false,false,nil)", exists, active, err)
	}

	// 推进到 active 后视为活跃（uploadTaskViewPort 的唯一放行态）。
	repo := sqlite.New(db)
	if _, err := repo.UpdateTaskStatus(ctx, "t1", ocdecktask.StatusActive, nil); err != nil {
		t.Fatalf("activate task: %v", err)
	}
	exists, active, err = p.TaskActive(ctx, "t1")
	if err != nil || !exists || !active {
		t.Fatalf("active task = (%v,%v,%v), want (true,true,nil)", exists, active, err)
	}
}

// TestUploadWiring_StartupScanEmptyRoot 验证生产装配在 uploads 根目录缺失/为空时
// 启动扫描无错（首次启动常规路径）。
func TestUploadWiring_StartupScanEmptyRoot(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	// UploadDir 指向尚未创建的目录（config.Load 之外不预设 MkdirAll）。
	orch := newUploadOrchestrator(wiringUploadConfig(filepath.Join(t.TempDir(), "uploads"), 0),
		sqlite.New(db), eventbus.New(), &currentConnPort{})
	if err := orch.StartupScan(ctx); err != nil {
		t.Fatalf("startup scan on missing root: %v", err)
	}
}

// TestUploadWiring_ReclaimOnTaskDeleted 验证端到端回收链路：真实提交的上传经
// StartupScan 入索引后，bus 发布 task.deleted 事件触发订阅协程回收整个任务目录。
func TestUploadWiring_ReclaimOnTaskDeleted(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := seedTaskForSubmissions(ctx, db); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	st := uploads.NewStore(dir)
	name := commitWiredUpload(t, ctx, st, "t1")
	taskDir := filepath.Join(dir, "t1")

	bus := eventbus.New()
	adapter := sqlite.New(db)
	orch := newUploadOrchestrator(wiringUploadConfig(dir, 0), adapter, bus, &currentConnPort{})
	orch.Start(ctx)

	if err := orch.StartupScan(ctx); err != nil {
		t.Fatalf("startup scan: %v", err)
	}
	// 扫描后未过期且任务未删除：上传保留。
	if _, err := os.Stat(filepath.Join(taskDir, name)); err != nil {
		t.Fatalf("upload should survive startup scan: %v", err)
	}

	// 生产不变量：task.deleted 仅在 DB 删除提交成功后发布
	//（internal/application/task/delete_reconcile.go）。先真实删除任务行，
	// 回收路径的 TaskActive 复查才会放行。
	if _, err := adapter.DeleteTask(ctx, "t1"); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	// task.deleted 事件仅触发回收：订阅协程经协调锁删除整任务目录并清索引。
	bus.Publish(ocdeckevent.NewTaskDeleted("t1", "active"))
	waitForDirGone(t, taskDir)

	// Stop 幂等（shutdownRuntime 仅保证调用一次，但编排器语义须容忍重复）。
	orch.Stop()
	orch.Stop()
}

// TestUploadWiring_TTLExpiryCleanup 验证 cfg.UploadRetention 经组合根传入编排器：
// 过期上传在周期清理（CleanupOnce）中被删除（文件 + sidecar 成组）。
func TestUploadWiring_TTLExpiryCleanup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := seedTaskForSubmissions(ctx, db); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	st := uploads.NewStore(dir)
	name := commitWiredUpload(t, ctx, st, "t1")
	finalPath := filepath.Join(dir, "t1", name)
	sidecarPath := finalPath + ".meta.json"

	orch := newUploadOrchestrator(wiringUploadConfig(dir, 2*time.Millisecond), sqlite.New(db), eventbus.New(), &currentConnPort{})
	if err := orch.StartupScan(ctx); err != nil {
		t.Fatalf("startup scan: %v", err)
	}

	// 等待超过 retention 窗口后周期清理触发删除。
	time.Sleep(5 * time.Millisecond)
	if err := orch.CleanupOnce(ctx); err != nil {
		t.Fatalf("cleanup once: %v", err)
	}
	for _, p := range []string{finalPath, sidecarPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("expired upload still present: %s", p)
		}
	}
}
