package task

import (
	"context"
	"os"
	"testing"

	"ocdeck/internal/process"
)

// unsetEnvForTest 在测试期间取消指定 env 变量并在 t.Cleanup 恢复原值。
// 用于 mergeEnvSnapshot 的 locale 兜底测试，避免宿主实际 locale 干扰断言。
func unsetEnvForTest(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		old, ok := os.LookupEnv(k)
		_ = os.Unsetenv(k)
		t.Cleanup(func() {
			if ok {
				_ = os.Setenv(k, old)
			} else {
				_ = os.Unsetenv(k)
			}
		})
	}
}

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

// TestMergeEnvSnapshot_LocaleDefault 验证 design.md D0 任务侧 locale 兜底（透传 + 非空语义）：
//   - (a) 宿主无 LANG/LC_ALL/LC_CTYPE → 注入 LANG=en_US.UTF-8；
//   - (b) 宿主显式 LANG=C → 原样透传不覆盖；
//   - (c) 无 LANG 但有 LC_ALL（或 LC_CTYPE）→ 透传高位变量原值且不注入默认；
//   - (d) 宿主 LANG= 空串且 LC_ALL/LC_CTYPE 未设 → 注入默认（空串视为未设置）；
//   - (e) 宿主 LC_ALL= 空串且 LANG 未设 → 注入默认（空串视为未设置）。
func TestMergeEnvSnapshot_LocaleDefault(t *testing.T) {
	const def = "en_US.UTF-8"

	// (a) 无 locale 变量 → 注入默认。
	unsetEnvForTest(t, "LANG", "LC_ALL", "LC_CTYPE")
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	merged, err := m.mergeEnvSnapshot(context.Background(), store.tasks["t1"], 50001)
	if err != nil {
		t.Fatalf("case a: mergeEnvSnapshot: %v", err)
	}
	if got := merged["LANG"]; got != def {
		t.Errorf("case a: LANG = %q, want %q (default injected)", got, def)
	}

	// (b) 显式 LANG=C → 透传不覆盖、不注入默认。
	unsetEnvForTest(t, "LANG", "LC_ALL", "LC_CTYPE")
	t.Setenv("LANG", "C")
	store2 := newMockStore()
	seedSuspendedTask(store2, "t1", "p1")
	m2 := newTestManager(t, store2, newMockProc(), newMockWorktree(), newMockOC(true))
	merged2, err := m2.mergeEnvSnapshot(context.Background(), store2.tasks["t1"], 50001)
	if err != nil {
		t.Fatalf("case b: mergeEnvSnapshot: %v", err)
	}
	if got := merged2["LANG"]; got != "C" {
		t.Errorf("case b: LANG = %q, want C (explicit value respected, not overwritten)", got)
	}

	// (c1) 无 LANG 但有 LC_ALL → 透传 LC_ALL 原值且不注入默认。
	unsetEnvForTest(t, "LANG", "LC_ALL", "LC_CTYPE")
	t.Setenv("LC_ALL", "en_US.UTF-8")
	store3 := newMockStore()
	seedSuspendedTask(store3, "t1", "p1")
	m3 := newTestManager(t, store3, newMockProc(), newMockWorktree(), newMockOC(true))
	merged3, err := m3.mergeEnvSnapshot(context.Background(), store3.tasks["t1"], 50001)
	if err != nil {
		t.Fatalf("case c1: mergeEnvSnapshot: %v", err)
	}
	if got, ok := merged3["LANG"]; ok && got == def {
		t.Errorf("case c1: default LANG should not be injected when LC_ALL set, got LANG=%q", got)
	}
	if got := merged3["LC_ALL"]; got != "en_US.UTF-8" {
		t.Errorf("case c1: LC_ALL = %q, want en_US.UTF-8 (passed through)", got)
	}

	// (c2) 无 LANG 但有 LC_CTYPE → 透传 LC_CTYPE 原值且不注入默认。
	unsetEnvForTest(t, "LANG", "LC_ALL", "LC_CTYPE")
	t.Setenv("LC_CTYPE", "en_US.UTF-8")
	store4 := newMockStore()
	seedSuspendedTask(store4, "t1", "p1")
	m4 := newTestManager(t, store4, newMockProc(), newMockWorktree(), newMockOC(true))
	merged4, err := m4.mergeEnvSnapshot(context.Background(), store4.tasks["t1"], 50001)
	if err != nil {
		t.Fatalf("case c2: mergeEnvSnapshot: %v", err)
	}
	if got, ok := merged4["LANG"]; ok && got == def {
		t.Errorf("case c2: default LANG should not be injected when LC_CTYPE set, got LANG=%q", got)
	}
	if got := merged4["LC_CTYPE"]; got != "en_US.UTF-8" {
		t.Errorf("case c2: LC_CTYPE = %q, want en_US.UTF-8 (passed through)", got)
	}

	// (d) LANG= 空串且 LC_ALL/LC_CTYPE 未设 → 注入默认（空串视为未设置）。
	unsetEnvForTest(t, "LANG", "LC_ALL", "LC_CTYPE")
	// t.Setenv 无法设空串后区分"未设"与"空串"语义，直接 os.Setenv 让 LookupEnv
	// 返回 ("", true)，与未设 (ok=false) 区分；空串被基础集循环 v != "" 跳过，
	// 且不抑制兜底注入。
	_ = os.Setenv("LANG", "")
	store5 := newMockStore()
	seedSuspendedTask(store5, "t1", "p1")
	m5 := newTestManager(t, store5, newMockProc(), newMockWorktree(), newMockOC(true))
	merged5, err := m5.mergeEnvSnapshot(context.Background(), store5.tasks["t1"], 50001)
	if err != nil {
		t.Fatalf("case d: mergeEnvSnapshot: %v", err)
	}
	if got := merged5["LANG"]; got != def {
		t.Errorf("case d: LANG = %q, want %q (empty string treated as unset, default injected)", got, def)
	}

	// (e) LC_ALL= 空串且 LANG 未设 → 注入默认（空串视为未设置）。
	unsetEnvForTest(t, "LANG", "LC_ALL", "LC_CTYPE")
	_ = os.Setenv("LC_ALL", "")
	store6 := newMockStore()
	seedSuspendedTask(store6, "t1", "p1")
	m6 := newTestManager(t, store6, newMockProc(), newMockWorktree(), newMockOC(true))
	merged6, err := m6.mergeEnvSnapshot(context.Background(), store6.tasks["t1"], 50001)
	if err != nil {
		t.Fatalf("case e: mergeEnvSnapshot: %v", err)
	}
	if got := merged6["LANG"]; got != def {
		t.Errorf("case e: LANG = %q, want %q (empty LC_ALL treated as unset, default injected)", got, def)
	}
}
