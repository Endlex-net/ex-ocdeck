package task

import (
	"context"
	"sync/atomic"
	"testing"

	"ocdeck/internal/infrastructure/hostenv"
)

// stubCaptureCount 临时替换 hostenv.Capture 为计数 stub（返回 nil，不影响行为），
// 测试结束恢复。返回计数供断言捕获是否被触发。
func stubCaptureCount(t *testing.T) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	orig := hostenv.Capture
	hostenv.Capture = func() map[string]string {
		calls.Add(1)
		return nil
	}
	t.Cleanup(func() { hostenv.Capture = orig })
	return &calls
}

// TestLayerEnvSnapshot_IllegalRow_NoCapture 非法任务（kind/mode/base_ref/branch 形态
// 校验失败）拒绝路径 MUST NOT 触发 login shell 捕获（host-env-sync：校验 MUST 先于
// 任何 hostEnv 调用，被拒绝路径禁止懒加载捕获副作用）。
func TestLayerEnvSnapshot_IllegalRow_NoCapture(t *testing.T) {
	cases := []struct {
		name    string
		kind    string
		mode    string
		branch  string
		baseRef string
	}{
		{"dir+worktree", ProjectKindDir, TaskModeWorktree, "ocdeck/a", "refs/heads/main"},
		{"repo+unknown mode", ProjectKindRepo, "bogus", "ocdeck/a", "refs/heads/main"},
		{"worktree missing branch", ProjectKindRepo, TaskModeWorktree, "", "refs/heads/main"},
		{"worktree bad base_ref", ProjectKindRepo, TaskModeWorktree, "ocdeck/a", "refs/heads/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// K4：零捕获断言只有在未加载态下才有意义——若缓存已处于失败态，
			// 即使校验未前移（错误路径提前解析）计数也为 0，会假绿。
			hostenv.ResetForTest()
			t.Cleanup(hostenv.ResetForTest)
			calls := stubCaptureCount(t)
			store := &envVarsStore{mockStore: newMockStore()}
			defaultBranch := ""
			if tc.kind == ProjectKindRepo {
				defaultBranch = "main"
			}
			store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: "/repo", DefaultBranch: defaultBranch, Kind: tc.kind})
			// 配置一个进程环境未设置的 follow_host 全局变量：若校验未前移，
			// 全局层解析会在形态校验前触发 login shell 捕获（计数 >0，测试失败）。
			store.globalVars = []GlobalEnvVarRow{{Key: "GUARD_FOLLOW", Mode: "follow_host", Value: ""}}
			store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "task",
				Branch: tc.branch, BaseRef: tc.baseRef,
				Status: StatusSuspended, WorktreePath: "/wt", Mode: tc.mode}
			m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

			if _, err := m.layerEnvSnapshot(context.Background(), store.tasks["t1"]); err == nil {
				t.Fatal("layerEnvSnapshot with illegal row: want error, got nil")
			}
			if calls.Load() != 0 {
				t.Fatalf("capture calls = %d, want 0 (rejected path MUST NOT capture login shell env)", calls.Load())
			}
		})
	}
}
