package uploads

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	ocdeckevent "ocdeck/internal/domain/event"
)

func TestStartupScan_RebuildsIndexAndCleanup(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	store := newFakeStore()
	// R5 兜底语义：任务不存在会整目录回收，故此处配置任务存在以走按条目清理路径。
	tasks := newFakeTasks(map[string]fakeTask{
		"t1": {exists: true, active: true},
		"t2": {exists: true, active: true},
		"t3": {exists: true, active: true},
	})

	validMeta := Meta{
		UploadID: testUploadID, TaskID: "t1", ConnID: testConnID,
		Filename: testUploadID + ".txt", UploadedAt: now.Add(-time.Minute),
	}
	expiredMeta := Meta{
		UploadID: "11111111111111111111111111111111", TaskID: "t1", ConnID: testConnID,
		Filename: "11111111111111111111111111111111.txt", UploadedAt: now.Add(-2 * time.Hour),
	}
	store.scanEntries = []Entry{
		{Kind: EntryValidUpload, TaskID: "t1", Filename: validMeta.Filename, Meta: &validMeta},
		{Kind: EntryValidUpload, TaskID: "t1", Filename: expiredMeta.Filename, Meta: &expiredMeta},
		{Kind: EntryOrphanFile, TaskID: "t2", Filename: "old.bin", ModTime: now.Add(-2 * orphanAge)},
		{Kind: EntryOrphanFile, TaskID: "t2", Filename: "fresh.bin", ModTime: now},
		{Kind: EntryReadFailed, TaskID: "t3", Filename: "x.meta.json", ModTime: now.Add(-2 * orphanAge)},
	}
	o := newTestOrchestrator(store, tasks, nil, func(o *Options) {
		o.Cfg.Retention = time.Hour
		o.Clock = func() time.Time { return now }
	})
	if err := o.StartupScan(ctx); err != nil {
		t.Fatalf("StartupScan: %v", err)
	}
	o.mu.Lock()
	indexed := len(o.index)
	_, hasValid := o.index[indexKey{taskID: "t1", uploadID: testUploadID}]
	o.mu.Unlock()
	if indexed != 1 || !hasValid {
		t.Fatalf("index size=%d hasValid=%v, want only valid entry indexed", indexed, hasValid)
	}
	_, _, _, removeUpload, removePath, _ := storeCounts(store)
	if removeUpload != 1 {
		t.Fatalf("expired upload removals = %d, want 1", removeUpload)
	}
	if removePath != 1 {
		t.Fatalf("orphan removals = %d, want 1 (old only, read_failed untouched)", removePath)
	}
	if got := store.removePathCalls[0]; got.name != "old.bin" {
		t.Fatalf("removed orphan = %s, want old.bin (fresh kept)", got.name)
	}
}

func TestCleanupOnce_TTLDisabledKeepsExpiredUpload(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	store := newFakeStore()
	meta := Meta{
		UploadID: testUploadID, TaskID: "t1", ConnID: testConnID,
		Filename: testUploadID + ".txt", UploadedAt: now.Add(-2 * time.Hour),
	}
	store.scanEntries = []Entry{
		{Kind: EntryValidUpload, TaskID: "t1", Filename: meta.Filename, Meta: &meta},
	}
	o := newTestOrchestrator(store, newFakeTasks(map[string]fakeTask{"t1": {exists: true, active: true}}), nil, func(o *Options) {
		o.Cfg.Retention = 0 // TTL 关闭
		o.Clock = func() time.Time { return now }
	})
	if err := o.CleanupOnce(ctx); err != nil {
		t.Fatalf("CleanupOnce: %v", err)
	}
	_, _, _, removeUpload, _, _ := storeCounts(store)
	if removeUpload != 0 {
		t.Fatalf("expired upload removals = %d, want 0 (TTL disabled)", removeUpload)
	}
}

func TestCleanupOnce_StalePartialSkipsActiveUpload(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	store := newFakeStore()
	o := newTestOrchestrator(store, newFakeTasks(map[string]fakeTask{"t1": {exists: true, active: true}}), nil, func(o *Options) {
		o.Clock = func() time.Time { return now }
	})
	// 活跃上传：partial 属于活跃登记，即使 modtime>1h 也由取消收尾负责，不删。
	u, err := o.RegisterActiveUpload("t1", testConnID)
	if err != nil {
		t.Fatal(err)
	}
	store.scanEntries = []Entry{
		{Kind: EntryStalePartial, TaskID: "t1", Filename: u.ID, ModTime: now.Add(-2 * orphanAge)},
	}
	if err := o.CleanupOnce(ctx); err != nil {
		t.Fatalf("CleanupOnce: %v", err)
	}
	_, _, removePartial, _, _, _ := storeCounts(store)
	if removePartial != 0 {
		t.Fatalf("stale partial removals = %d, want 0 (belongs to active upload)", removePartial)
	}
	o.AbortUpload(ctx, u)
}

func TestTaskDeletedEvent_ReclaimsDir(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newFakeStore()
	sub := &fakeSub{}
	o := newTestOrchestrator(store, newFakeTasks(map[string]fakeTask{}), nil, func(o *Options) {
		o.Events = &fakeEvents{sub: sub}
	})
	o.mu.Lock()
	o.index[indexKey{taskID: "t9", uploadID: testUploadID}] = Meta{UploadID: testUploadID, TaskID: "t9"}
	o.mu.Unlock()

	o.Start(ctx)
	defer o.Stop()
	sub.events <- ocdeckevent.NewTaskDeleted("t9", "active")
	// 无关事件忽略。
	sub.events <- ocdeckevent.Event{Topic: ocdeckevent.TopicTask, Type: ocdeckevent.TypeTaskStatusChanged, RID: "t9"}

	waitFor(t, 2*time.Second, func() bool {
		_, _, _, _, _, n := storeCounts(store)
		return n == 1
	})
	if got := store.removeTaskDirCalls[0]; got != "t9" {
		t.Fatalf("reclaimed dir = %s, want t9", got)
	}
	o.mu.Lock()
	_, stillIndexed := o.index[indexKey{taskID: "t9", uploadID: testUploadID}]
	o.mu.Unlock()
	if stillIndexed {
		t.Fatal("index entry must be purged on task.deleted")
	}
}

// TestTaskDeletedEvent_QueryFailureSkipsReclaim 验证回收前的存在性复查（R5 残留）：
// task.deleted 到达但任务表查询暂时失败 → 不误删、索引保留；查询恢复后重发事件
// 成功回收。
func TestTaskDeletedEvent_QueryFailureSkipsReclaim(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newFakeStore()
	sub := &fakeSub{}
	tasks := newFakeTasks(map[string]fakeTask{"t9": {exists: true, active: true}})
	tasks.err = errors.New("db unavailable")
	o := newTestOrchestrator(store, tasks, nil, func(o *Options) {
		o.Events = &fakeEvents{sub: sub}
	})
	o.mu.Lock()
	o.index[indexKey{taskID: "t9", uploadID: testUploadID}] = Meta{UploadID: testUploadID, TaskID: "t9"}
	o.mu.Unlock()

	o.Start(ctx)
	defer o.Stop()
	sub.events <- ocdeckevent.NewTaskDeleted("t9", "active")

	waitFor(t, 2*time.Second, func() bool {
		return tasks.callsCount("t9") >= 1 // 事件已被消费并进入复查
	})
	// 查询失败：零回收、索引保留。
	if n := len(store.removeTaskDirCalls); n != 0 {
		t.Fatalf("task dir reclaims on query failure = %d, want 0", n)
	}
	o.mu.Lock()
	_, still := o.index[indexKey{taskID: "t9", uploadID: testUploadID}]
	o.mu.Unlock()
	if !still {
		t.Fatal("index must survive query failure")
	}

	// 查询恢复（任务确已删除）后重试成功。
	tasks.err = nil
	delete(tasks.tasks, "t9")
	sub.events <- ocdeckevent.NewTaskDeleted("t9", "active")
	waitFor(t, 2*time.Second, func() bool {
		_, _, _, _, _, n := storeCounts(store)
		return n == 1
	})
	if got := store.removeTaskDirCalls[0]; got != "t9" {
		t.Fatalf("reclaimed dir = %s, want t9", got)
	}
}

func TestStop_Idempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fe := &fakeEvents{sub: &fakeSub{}}
	o := newTestOrchestrator(newFakeStore(), newFakeTasks(map[string]fakeTask{}), nil, func(o *Options) {
		o.Events = fe
	})
	o.Start(ctx)
	o.Stop()
	o.Stop() // 幂等
}

// TestCleanupOnce_NonHexPartialNameNotActive 验证非受管 32 字符 .partial 名
//（前 32 字符非 hex）不误判为活跃 upload ID：仍按孤儿龄期删除（S2）。
func TestCleanupOnce_NonHexPartialNameNotActive(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	store := newFakeStore()
	o := newTestOrchestrator(store, newFakeTasks(map[string]fakeTask{"t1": {exists: true, active: true}}), nil, func(o *Options) {
		o.Clock = func() time.Time { return now }
	})
	// 存在活跃登记，但扫描到的 partial 名非受管 32hex 形态，二者无关。
	u, err := o.RegisterActiveUpload("t1", testConnID)
	if err != nil {
		t.Fatal(err)
	}
	store.scanEntries = []Entry{
		{Kind: EntryStalePartial, TaskID: "t1", Filename: strings.Repeat("Z", 32), ModTime: now.Add(-2 * orphanAge)},
	}
	if err := o.CleanupOnce(ctx); err != nil {
		t.Fatalf("CleanupOnce: %v", err)
	}
	_, _, removePartial, _, _, _ := storeCounts(store)
	if removePartial != 1 {
		t.Fatalf("stale partial removals = %d, want 1 (non-hex name must not count as active)", removePartial)
	}
	o.AbortUpload(ctx, u)
}

// TestUploadIDFromPartialName 直接表驱动 uploadIDFromPartialName 的受管形态判定
//（S2）：仅前 32 字符为合法 32hex 才返回 ID，非 hex 前缀返回空（不误判活跃）。
func TestUploadIDFromPartialName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"hex32 exact", testUploadID, testUploadID},
		{"hex32 with partial suffix", testUploadID + ".partial", testUploadID},
		{"hex32 with ext", testUploadID + ".txt", testUploadID},
		{"non-hex 32 chars", strings.Repeat("Z", 32), ""},
		{"mixed hex and non-hex", testUploadID[:31] + "Z", ""},
		{"too short", testUploadID[:31], ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := uploadIDFromPartialName(tc.in); got != tc.want {
				t.Fatalf("uploadIDFromPartialName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCleanupOnce_RescansUnderLock_AfterRenewal 屏障测试（R3）：锁外扫描到过期旧值 →
// 投递续期改写元数据（与 Deliver 成功后效果一致：磁盘 sidecar 与索引同步新
// lastDeliveredAt）→ 清理取得任务锁后必须重扫并按新值判断 → 断言文件保留。
// 若实现回退为「锁外快照直接决策」，旧值过期 → 删除 → 本测试变红。
func TestCleanupOnce_RescansUnderLock_AfterRenewal(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	store := newFakeStore()
	tasks := newFakeTasks(map[string]fakeTask{"t1": {exists: true, active: true}})
	renewed := now.Add(-30 * time.Minute)
	stale := Meta{
		UploadID: testUploadID, TaskID: "t1", ConnID: testConnID,
		Filename: testUploadID + ".txt", UploadedAt: now.Add(-2 * time.Hour),
	}
	fresh := stale
	fresh.LastDeliveredAt = &renewed
	store.scanQueue = [][]Entry{
		{{Kind: EntryValidUpload, TaskID: "t1", Filename: stale.Filename, Meta: &stale}}, // 锁外快照（旧）
		{{Kind: EntryValidUpload, TaskID: "t1", Filename: fresh.Filename, Meta: &fresh}}, // 锁内重扫（新）
	}
	o := newTestOrchestrator(store, tasks, nil, func(o *Options) {
		o.Cfg.Retention = time.Hour
		o.Clock = func() time.Time { return now }
	})

	// 先持锁模拟「投递续期先行」：Deliver 成功后的效果即 sidecar 新基准 + 索引同步。
	release, err := o.coord.Acquire(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	// 模拟 Deliver 续期效果（R2：刷新成功同步索引）。
	o.mu.Lock()
	o.index[indexKey{taskID: "t1", uploadID: testUploadID}] = fresh
	o.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := o.CleanupOnce(ctx); err != nil {
			t.Errorf("CleanupOnce: %v", err)
		}
	}()
	release() // 释放后清理 goroutine 才能取得锁并重扫
	<-done

	_, _, _, removeUpload, _, _ := storeCounts(store)
	if removeUpload != 0 {
		t.Fatalf("expired-check removals = %d, want 0 (renewal must win over stale snapshot)", removeUpload)
	}
	o.mu.Lock()
	_, still := o.index[indexKey{taskID: "t1", uploadID: testUploadID}]
	o.mu.Unlock()
	if !still {
		t.Fatal("index entry must survive (file kept, renewal effective)")
	}
}

// TestCleanupOnce_ReclaimsDeletedTaskDir 兜底回收（R5）：task.deleted 事件漏收/
// 重启残留时，清理确认任务已不存在即整目录回收并清索引（不逐条目删除）。
func TestCleanupOnce_ReclaimsDeletedTaskDir(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	tasks := newFakeTasks(map[string]fakeTask{}) // t1 不存在
	meta := Meta{
		UploadID: testUploadID, TaskID: "t1", ConnID: testConnID,
		Filename: testUploadID + ".txt", UploadedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	}
	store.scanEntries = []Entry{
		{Kind: EntryValidUpload, TaskID: "t1", Filename: meta.Filename, Meta: &meta},
	}
	o := newTestOrchestrator(store, tasks, nil, func(o *Options) {
		o.Cfg.Retention = time.Hour
		o.Clock = func() time.Time { return meta.UploadedAt.Add(time.Minute) }
	})
	if err := o.CleanupOnce(ctx); err != nil {
		t.Fatalf("CleanupOnce: %v", err)
	}
	if len(store.removeTaskDirCalls) != 1 || store.removeTaskDirCalls[0] != "t1" {
		t.Fatalf("task dir reclaims = %v, want [t1]", store.removeTaskDirCalls)
	}
	_, _, _, removeUpload, _, _ := storeCounts(store)
	if removeUpload != 0 {
		t.Fatalf("per-entry removals = %d, want 0 (whole-dir reclaim)", removeUpload)
	}
	o.mu.Lock()
	_, still := o.index[indexKey{taskID: "t1", uploadID: testUploadID}]
	o.mu.Unlock()
	if still {
		t.Fatal("index must be purged on whole-dir reclaim")
	}
}

// TestStartupScan_ReclaimsResidualDir 重启残留（R5）：StartupScan 走同一兜底，
// 任务已删除的目录被回收。
func TestStartupScan_ReclaimsResidualDir(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	tasks := newFakeTasks(map[string]fakeTask{}) // t1 重启前已删除
	meta := Meta{
		UploadID: testUploadID, TaskID: "t1", ConnID: testConnID,
		Filename: testUploadID + ".txt", UploadedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	}
	store.scanEntries = []Entry{
		{Kind: EntryValidUpload, TaskID: "t1", Filename: meta.Filename, Meta: &meta},
	}
	o := newTestOrchestrator(store, tasks, nil, func(o *Options) {
		o.Cfg.Retention = time.Hour
		o.Clock = func() time.Time { return meta.UploadedAt.Add(time.Minute) }
	})
	if err := o.StartupScan(ctx); err != nil {
		t.Fatalf("StartupScan: %v", err)
	}
	if len(store.removeTaskDirCalls) != 1 || store.removeTaskDirCalls[0] != "t1" {
		t.Fatalf("task dir reclaims = %v, want [t1]", store.removeTaskDirCalls)
	}
	o.mu.Lock()
	_, still := o.index[indexKey{taskID: "t1", uploadID: testUploadID}]
	o.mu.Unlock()
	if still {
		t.Fatal("index must not retain entries for reclaimed dir")
	}
}

// TestCleanupOnce_TaskQueryFailure_Skips 查询故障（R5）：任务存在性查询失败不得当
// 不存在误删——目录保留、零删除、索引不动。
func TestCleanupOnce_TaskQueryFailure_Skips(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	tasks := newFakeTasks(map[string]fakeTask{"t1": {exists: true, active: true}})
	tasks.err = errors.New("db unavailable")
	meta := Meta{
		UploadID: testUploadID, TaskID: "t1", ConnID: testConnID,
		Filename: testUploadID + ".txt", UploadedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	}
	store.scanEntries = []Entry{
		{Kind: EntryValidUpload, TaskID: "t1", Filename: meta.Filename, Meta: &meta},
	}
	o := newTestOrchestrator(store, tasks, nil, func(o *Options) {
		o.Cfg.Retention = time.Hour
		o.Clock = func() time.Time { return meta.UploadedAt.Add(time.Minute) }
	})
	// CleanupOnce 不重建索引：预填充该 upload 的索引项以验证「查询失败不误清」。
	o.mu.Lock()
	o.index[indexKey{taskID: "t1", uploadID: testUploadID}] = meta
	o.mu.Unlock()
	if err := o.CleanupOnce(ctx); err != nil {
		t.Fatalf("CleanupOnce: %v", err)
	}
	if len(store.removeTaskDirCalls) != 0 {
		t.Fatalf("task dir reclaims = %v, want none on query failure", store.removeTaskDirCalls)
	}
	_, _, _, removeUpload, _, _ := storeCounts(store)
	if removeUpload != 0 {
		t.Fatalf("removals = %d, want 0 on query failure", removeUpload)
	}
	o.mu.Lock()
	_, still := o.index[indexKey{taskID: "t1", uploadID: testUploadID}]
	o.mu.Unlock()
	if !still {
		t.Fatal("index must survive query failure")
	}
}

// TestCleanupOnce_RemoveTaskDirFailureRetries 首次目录删除失败（R5）：目录残留、
// 索引保留；下轮（注入解除后）重试成功并清索引。
func TestCleanupOnce_RemoveTaskDirFailureRetries(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	tasks := newFakeTasks(map[string]fakeTask{}) // t1 不存在
	meta := Meta{
		UploadID: testUploadID, TaskID: "t1", ConnID: testConnID,
		Filename: testUploadID + ".txt", UploadedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	}
	store.scanEntries = []Entry{
		{Kind: EntryValidUpload, TaskID: "t1", Filename: meta.Filename, Meta: &meta},
	}
	o := newTestOrchestrator(store, tasks, nil, func(o *Options) {
		o.Cfg.Retention = time.Hour
		o.Clock = func() time.Time { return meta.UploadedAt.Add(time.Minute) }
	})
	o.mu.Lock()
	o.index[indexKey{taskID: "t1", uploadID: testUploadID}] = meta
	o.mu.Unlock()

	store.errRemoveTaskDir = errors.New("ebusy")
	if err := o.CleanupOnce(ctx); err != nil {
		t.Fatalf("CleanupOnce: %v", err)
	}
	o.mu.Lock()
	_, still := o.index[indexKey{taskID: "t1", uploadID: testUploadID}]
	o.mu.Unlock()
	if !still {
		t.Fatal("index must survive failed reclaim (dir not removed)")
	}

	// 下轮：删除成功 → 索引清理。
	store.errRemoveTaskDir = nil
	if err := o.CleanupOnce(ctx); err != nil {
		t.Fatalf("CleanupOnce retry: %v", err)
	}
	if n := len(store.removeTaskDirCalls); n != 2 {
		t.Fatalf("removeTaskDir calls = %d, want 2 (retry next cycle)", n)
	}
	o.mu.Lock()
	_, still = o.index[indexKey{taskID: "t1", uploadID: testUploadID}]
	o.mu.Unlock()
	if still {
		t.Fatal("index must be purged after successful reclaim")
	}
}
