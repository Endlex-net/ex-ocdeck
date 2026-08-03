package task

import (
	"context"
	"testing"

	"ocdeck/internal/process"
)

// TestMergeEnvSnapshot_ForcesTerm 防回归：宿主 TERM 为 terminfo 不认识的值
// （如 xterm-ghostty）时，基础集 MUST 强制 xterm-256color（tmux 报
// "missing or unsuitable terminal" 的根因，m0579 真实用户环境复现）。
func TestMergeEnvSnapshot_ForcesTerm(t *testing.T) {
	t.Setenv("TERM", "xterm-ghostty")

	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	merged, err := m.mergeEnvSnapshot(context.Background(), store.tasks["t1"], 50001)
	if err != nil {
		t.Fatalf("mergeEnvSnapshot: %v", err)
	}
	if got := merged["TERM"]; got != "xterm-256color" {
		t.Errorf("TERM = %q, want xterm-256color（不得继承宿主 xterm-ghostty）", got)
	}
}

// TestDefaultBaseEnv_ForcesTerm 防回归：process 层基础 env 同样强制 TERM。
func TestDefaultBaseEnv_ForcesTerm(t *testing.T) {
	env := process.DefaultBaseEnv(func(k string) (string, bool) {
		if k == "TERM" {
			return "xterm-ghostty", true
		}
		return "", false
	})
	found := ""
	for _, kv := range env {
		if len(kv) > 5 && kv[:5] == "TERM=" {
			found = kv[5:]
		}
	}
	if found != "xterm-256color" {
		t.Errorf("DefaultBaseEnv TERM = %q, want xterm-256color", found)
	}
}
