package task

import (
	"context"
	"sync"
	"time"

	"ocdeck/internal/infrastructure/git"
)

// mockLifecycleRunner 记录 RunScript 调用并支持注入结果（design.md §7.1，Phase 3 测试用）。
// 非 goroutine-safe：测试串行使用；并发测试用独立实例或锁保护。
type mockLifecycleRunner struct {
	mu sync.Mutex
	// runScriptErr 控制 RunScript 返回（nil = 成功）。
	runScriptErr error
	// runScriptCalls 记录调用参数（dir/script/logPath/timeout）。
	runScriptCalls []mockRunScriptCall
	// copyInheritedWarnings 控制 CopyInherited 返回的警告。
	copyInheritedWarnings []string
	// copyInheritedCalls 记录调用参数。
	copyInheritedCalls []mockCopyCall
}

type mockRunScriptCall struct {
	dir      string
	env      map[string]string
	script   string
	logPath  string
	timeout  time.Duration
	ctxDone  bool // 记录 RunScript 调用时 ctx 是否已 Done（区分 runnerCtx / request ctx / signal ctx）
}

type mockCopyCall struct {
	repoPath string
	wtPath   string
	entries  []git.FileStatus
	patterns []string
}

func (m *mockLifecycleRunner) RunScript(ctx context.Context, dir string, env map[string]string, script, logPath string, timeout time.Duration) error {
	m.mu.Lock()
	m.runScriptCalls = append(m.runScriptCalls, mockRunScriptCall{
		dir: dir, env: env, script: script, logPath: logPath, timeout: timeout,
		ctxDone: ctx.Err() != nil, // 记录调用时 ctx 是否已取消（探针：区分 runnerCtx/request ctx/signal ctx）
	})
	err := m.runScriptErr
	m.mu.Unlock()
	return err
}

func (m *mockLifecycleRunner) CopyInherited(ctx context.Context, repoPath, wtPath string, entries []git.FileStatus, patterns []string) []string {
	m.mu.Lock()
	m.copyInheritedCalls = append(m.copyInheritedCalls, mockCopyCall{repoPath: repoPath, wtPath: wtPath, entries: entries, patterns: patterns})
	warnings := m.copyInheritedWarnings
	m.mu.Unlock()
	return warnings
}

func (m *mockLifecycleRunner) runScriptCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.runScriptCalls)
}

func (m *mockLifecycleRunner) copyCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.copyInheritedCalls)
}

// lastRunScriptCtxDone 返回最后一次 RunScript 调用时 ctx 是否已 Done。
// 用于断言脚本执行收到的是 runnerCtx（非取消）而非 request/signal ctx（已取消）。
func (m *mockLifecycleRunner) lastRunScriptCtxDone() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.runScriptCalls) == 0 {
		return false
	}
	return m.runScriptCalls[len(m.runScriptCalls)-1].ctxDone
}
