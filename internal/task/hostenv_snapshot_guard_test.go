package task

import (
	"context"
	"os"
	"reflect"
	"testing"

	"ocdeck/internal/infrastructure/hostenv"
	"ocdeck/internal/infrastructure/process"
)

// TestRefresh_DoesNotTouchSnapshotsOrBaseEnv 快照与环境不变性（tasks 2.4）：
// hostenv.Refresh 成功/失败前后，已激活任务的持久化 env 快照与服务端基础环境
// （process.DefaultBaseEnv，进程 manager/watchdog 共用）逐项不变；
// refresh 成功后下一次 follow_host 解析使用新捕获缓存，失败保留旧缓存继续解析。
func TestRefresh_DoesNotTouchSnapshotsOrBaseEnv(t *testing.T) {
	// K4：显式重置到未加载态，保证基线 mergeEnvSnapshot 在 v1 缓存下执行
	//（-count=2 第二轮 / -shuffle 乱序时不残留上一轮缓存）。
	hostenv.ResetForTest()
	t.Cleanup(hostenv.ResetForTest)
	value := "v1"
	origCapture := hostenv.Capture
	hostenv.Capture = func() map[string]string {
		return map[string]string{"GUARD_FOLLOW": value}
	}
	t.Cleanup(func() { hostenv.Capture = origCapture })

	store := &envVarsStore{mockStore: newMockStore()}
	store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: "/repo", DefaultBranch: "main", Kind: ProjectKindRepo})
	store.globalVars = []GlobalEnvVarRow{{Key: "GUARD_FOLLOW", Mode: "follow_host", Value: ""}}
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "my task",
		Branch: "ocdeck/my-task", BaseRef: "refs/heads/main",
		Status: StatusSuspended, WorktreePath: "/wt", Mode: TaskModeWorktree}
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	ctx := context.Background()

	// 激活基线：v1 缓存下生成并持久化快照。
	merged, err := m.mergeEnvSnapshot(ctx, store.tasks["t1"], 50001)
	if err != nil {
		t.Fatalf("mergeEnvSnapshot: %v", err)
	}
	if merged["GUARD_FOLLOW"] != "v1" {
		t.Fatalf("baseline GUARD_FOLLOW = %q, want v1", merged["GUARD_FOLLOW"])
	}
	snapshotBefore := store.tasks["t1"].EnvSnapshot
	baseBefore := process.DefaultBaseEnv(os.LookupEnv)
	assertUnchanged := func(stage string) {
		t.Helper()
		if store.tasks["t1"].EnvSnapshot != snapshotBefore {
			t.Errorf("%s: persisted env snapshot changed: %q -> %q", stage, snapshotBefore.String, store.tasks["t1"].EnvSnapshot.String)
		}
		if !reflect.DeepEqual(baseBefore, process.DefaultBaseEnv(os.LookupEnv)) {
			t.Errorf("%s: DefaultBaseEnv changed (MUST NOT update process manager/watchdog base env)", stage)
		}
	}

	// Refresh 成功（v2）：快照与基础环境不变；下一次 follow_host 解析用新缓存。
	value = "v2"
	if err := hostenv.Refresh(); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	assertUnchanged("after successful refresh")
	merged2, err := m.layerEnvSnapshot(ctx, store.tasks["t1"])
	if err != nil {
		t.Fatalf("layerEnvSnapshot after refresh: %v", err)
	}
	if merged2["GUARD_FOLLOW"] != "v2" {
		t.Errorf("follow_host after refresh = %q, want v2 (下一次解析使用新捕获缓存)", merged2["GUARD_FOLLOW"])
	}

	// Refresh 失败：快照与基础环境不变；follow_host 解析仍用保留的旧成功缓存。
	hostenv.Capture = func() map[string]string { return nil }
	if err := hostenv.Refresh(); err == nil {
		t.Fatal("refresh with failing capture = nil error, want error")
	}
	assertUnchanged("after failed refresh")
	merged3, err := m.layerEnvSnapshot(ctx, store.tasks["t1"])
	if err != nil {
		t.Fatalf("layerEnvSnapshot after failed refresh: %v", err)
	}
	if merged3["GUARD_FOLLOW"] != "v2" {
		t.Errorf("follow_host after failed refresh = %q, want v2 (失败保留旧成功缓存)", merged3["GUARD_FOLLOW"])
	}
}
