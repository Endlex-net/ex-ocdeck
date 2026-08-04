package task

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/config"
	"ocdeck/internal/git"
)

// === 反证实验：阻塞 1（splitInheritPatterns 逐行解析） ===
//
// 旧实现按所有空白拆分，会把 `# *.pem` 拆成 `#` 和 `*.pem` 两个 pattern，
// 导致用户本想注释掉的 *.pem 仍参与匹配/复制；含空格的单行 glob 也被拆碎。
// 修复后逐行解析：TrimSpace、忽略空行与 # 开头注释行、保留行内空格。
// 以下测试在修复前应 FAIL（反证），修复后 PASS。

// TestSplitInheritPatterns_LineBasedParsing_IgnoresCommentAndBlankLines：
// 注释行与空行不产出 pattern；合法行保留（含行内空格）。
func TestSplitInheritPatterns_LineBasedParsing_IgnoresCommentAndBlankLines(t *testing.T) {
	in := "# *.pem\n\n  # leading spaces comment\n*.env\nsecrets/*.key\n"
	got := splitInheritPatterns(in)
	want := []string{"*.env", "secrets/*.key"}
	if len(got) != len(want) {
		t.Fatalf("splitInheritPatterns(%q) = %v, want %v (comment/blank lines ignored)", in, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitInheritPatterns(%q)[%d] = %q, want %q", in, i, got[i], want[i])
		}
	}
}

// TestSplitInheritPatterns_PreservesInLineSpaces：单行含空格的 pattern 不被拆分
// （一行 = 一个 pattern，保留行内空格）。反证：旧空白拆分会把 "my dir/*.txt" 拆成
// "my" "dir/*.txt" 两个 pattern。
func TestSplitInheritPatterns_PreservesInLineSpaces(t *testing.T) {
	in := "my dir/*.txt"
	got := splitInheritPatterns(in)
	if len(got) != 1 || got[0] != "my dir/*.txt" {
		t.Fatalf("splitInheritPatterns(%q) = %v, want single [\"my dir/*.txt\"] (in-line spaces preserved)", in, got)
	}
}

// TestSplitInheritPatterns_TrimsEachLine：每行 TrimSpace，行首/行尾空白被去除但行内空格保留。
func TestSplitInheritPatterns_TrimsEachLine(t *testing.T) {
	in := "  *.env  \n\t*.txt\t\n"
	got := splitInheritPatterns(in)
	want := []string{"*.env", "*.txt"}
	if len(got) != len(want) {
		t.Fatalf("splitInheritPatterns(%q) = %v, want %v", in, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitInheritPatterns(%q)[%d] = %q, want %q", in, i, got[i], want[i])
		}
	}
}

// TestSplitInheritPatterns_EmptyAndCommentOnly：仅含空行/注释 → 无 pattern。
func TestSplitInheritPatterns_EmptyAndCommentOnly(t *testing.T) {
	for _, in := range []string{"", "\n\n", "# comment\n# another\n", "  \n\t\n"} {
		got := splitInheritPatterns(in)
		if len(got) != 0 {
			t.Fatalf("splitInheritPatterns(%q) = %v, want empty", in, got)
		}
	}
}

// TestSplitInheritPatterns_MatchesAPISemantics：执行侧与 API 校验侧
// （lifecycle_config.go validate）对同一份配置解析出相同 pattern 集合——
// 均忽略空行与 # 注释行、保留行内空格。此处用 doublestar 校验合法的 pattern 集合
// 验证执行侧输出均为 API 侧会接受的合法行。
func TestSplitInheritPatterns_MatchesAPISemantics(t *testing.T) {
	// 与 API validate 一致：按 \n 分割、TrimSpace、忽略空行与 # 行。
	in := "# *.pem\n*.env\nmy configs/*.yml\n\n# trailing comment\n"
	got := splitInheritPatterns(in)
	want := []string{"*.env", "my configs/*.yml"}
	if len(got) != len(want) {
		t.Fatalf("execution-side patterns %v != API-side expected %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("execution-side patterns[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestRunInherit_CommentLineNotCopied：真实 runInherit 流程中，含 `# *.pem` 注释行
// 的配置不会把 .pem 文件纳入 CopyInherited 的 patterns。用记录 patterns 的 runner 桩
// 断言传入 CopyInherited 的 patterns 不含 `#` 也不含 `*.pem`。需真实 git repo 使
// ListIgnoredUntracked 枚举成功（否则枚举失败降级，CopyInherited 不被调用）。
// 反证：旧 splitInheritPatterns 会把 `# *.pem` 拆成 `#`、`*.pem`，runner 收到的 patterns
// 含 `*.pem`，断言失败。
func TestRunInherit_CommentLineNotCopied(t *testing.T) {
	resetLifecycleCfgMock()
	repoPath := newLifecycleTestRepo(t)
	store := newMockStore()
	// 配置含注释行 `# *.pem` 与合法行 `*.env`。
	seedLifecycleConfig(store, "p1", "# *.pem\n*.env\n", "", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: repoPath, DefaultBranch: "main"})
	runner := &patternsCaptureRunner{}
	m := newLifecycleTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true), runner)
	_, _, err := m.runInherit(context.Background(), repoPath, "/wt", "p1")
	if err != nil {
		t.Fatalf("runInherit: %v", err)
	}
	runner.mu.Lock()
	got := runner.capturedPatterns
	runner.mu.Unlock()
	if len(got) != 1 || got[0] != "*.env" {
		t.Fatalf("patterns passed to CopyInherited = %v, want only [\"*.env\"] (comment line `# *.pem` must not produce a pattern)", got)
	}
}

// patternsCaptureRunner 记录 CopyInherited 收到的 patterns（用于断言执行侧解析结果）。
type patternsCaptureRunner struct {
	mu               sync.Mutex
	capturedPatterns []string
}

func (r *patternsCaptureRunner) RunScript(ctx context.Context, dir string, env map[string]string, script, logPath string, timeout time.Duration) error {
	return nil
}

func (r *patternsCaptureRunner) CopyInherited(ctx context.Context, repoPath, wtPath string, entries []git.FileStatus, patterns []string) []string {
	r.mu.Lock()
	r.capturedPatterns = append([]string(nil), patterns...)
	r.mu.Unlock()
	return nil
}

// === ReadInitLog Manager 级测试（阻塞 3 补 Manager 级测试） ===

// newReadInitLogManager 构造带真实 logDir 与 mockStore 的 Manager（Manager 级日志读取测试用）。
func newReadInitLogManager(t *testing.T, store TaskStore, logDir string) *Manager {
	t.Helper()
	cfg := &config.Config{
		DataDir:        t.TempDir(),
		ServePortRange: config.PortRange{Min: 50000, Max: 50999},
		ShutdownPolicy: config.ShutdownPersist,
	}
	return New(Options{Cfg: cfg, Store: store, LogDir: logDir, LifecycleRunner: &mockLifecycleRunner{}})
}

// TestReadInitLog_InheritConcatenated：init.log + inherit.log 拼接，inherit 节冠以
// `[inherit warnings]` 标题（design.md §7.4）。任务存在时两者均读出并拼接。
// 对完整响应做逐字节等值断言，避免实现恢复未约定的 `== init log ==` 节时仍通过。
func TestReadInitLog_InheritConcatenated(t *testing.T) {
	resetLifecycleCfgMock()
	logDir := t.TempDir()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: StatusSuspended, InitStatus: InitStatusNone}
	m := newReadInitLogManager(t, store, logDir)
	taskDir := filepath.Join(logDir, "t1")
	if err := os.MkdirAll(taskDir, 0o700); err != nil {
		t.Fatal(err)
	}
	inheritContent := "warn: missing .env\n"
	initContent := "installing deps\n"
	if err := os.WriteFile(filepath.Join(taskDir, "inherit.log"), []byte(inheritContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "init.log"), []byte(initContent), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := m.ReadInitLog(context.Background(), "t1")
	if err != nil {
		t.Fatalf("ReadInitLog: %v", err)
	}
	// 契约格式：`[inherit warnings]\n<inherit 内容(若不以 \n 结尾则补 \n)><init 内容>`。
	// inheritContent 已以 \n 结尾，无需补行。
	want := "[inherit warnings]\n" + inheritContent + initContent
	if got != want {
		t.Fatalf("ReadInitLog response mismatch:\n got = %q\n want = %q", got, want)
	}
}

// TestReadInitLog_Tail64KB：整体 >64KB 时 tail ≤64KB，保留末尾（init.log 尾部优先）。
// 构造 inherit.log 小内容 + init.log 远超 64KB，断言结果 ≤64KB 且含 init.log 末尾标记。
func TestReadInitLog_Tail64KB(t *testing.T) {
	resetLifecycleCfgMock()
	logDir := t.TempDir()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: StatusSuspended, InitStatus: InitStatusNone}
	m := newReadInitLogManager(t, store, logDir)
	taskDir := filepath.Join(logDir, "t1")
	if err := os.MkdirAll(taskDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// inherit.log 小内容（确保拼接后仍 >64KB）。
	if err := os.WriteFile(filepath.Join(taskDir, "inherit.log"), []byte("small inherit warning\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// init.log ~128KB，末尾放唯一标记。
	big := strings.Repeat("x", 128*1024)
	marker := "\nEND-MARKER\n"
	if err := os.WriteFile(filepath.Join(taskDir, "init.log"), []byte(big+marker), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := m.ReadInitLog(context.Background(), "t1")
	if err != nil {
		t.Fatalf("ReadInitLog: %v", err)
	}
	if len(got) > lifecycleLogTailLimit {
		t.Fatalf("response len = %d, must be tail ≤ %d", len(got), lifecycleLogTailLimit)
	}
	if !strings.Contains(got, "END-MARKER") {
		t.Fatalf("tail must preserve end of init.log (END-MARKER), got len=%d", len(got))
	}
	// tail 后 inherit 节（位于开头）被截断丢弃——断言不含 inherit 小内容
	// （128KB init.log + inherit 超过 64KB，tail 保留末尾即 init.log 尾部）。
	if strings.Contains(got, "small inherit warning") {
		t.Fatalf("tail must drop leading inherit content when total >64KB, got %q", got[:min(len(got), 80)])
	}
}

// TestReadInitLog_InheritOnly_NoInitLog：init.log 缺失时 inherit 节仍返回
// （design.md §7.4/§9 UI：init=none 时 inherit.log 是唯一可见性渠道）。
func TestReadInitLog_InheritOnly_NoInitLog(t *testing.T) {
	resetLifecycleCfgMock()
	logDir := t.TempDir()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: StatusSuspended, InitStatus: InitStatusNone}
	m := newReadInitLogManager(t, store, logDir)
	taskDir := filepath.Join(logDir, "t1")
	if err := os.MkdirAll(taskDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "inherit.log"), []byte("only inherit warning\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 不创建 init.log。
	got, err := m.ReadInitLog(context.Background(), "t1")
	if err != nil {
		t.Fatalf("ReadInitLog: %v", err)
	}
	if !strings.Contains(got, "[inherit warnings]") {
		t.Fatalf("response must contain inherit warnings header, got %q", got)
	}
	if !strings.Contains(got, "only inherit warning") {
		t.Fatalf("response must contain inherit.log content, got %q", got)
	}
}

// TestReadInitLog_TaskNotFound_NotFound：任务不存在 → not_found（路径构造前先验任务存在）。
// design.md §8：先验证任务存在，再用可信 taskID 构造路径（防路径注入）。
// 陷阱：在不存在的 taskID 的日志目录路径放一个无读权限文件——若实现先读日志再查任务，
// readLifecycleLog 会因权限失败返回 internal；先验任务存在则返回 not_found。
func TestReadInitLog_TaskNotFound_NotFound(t *testing.T) {
	resetLifecycleCfgMock()
	logDir := t.TempDir()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	m := newReadInitLogManager(t, store, logDir)
	// 陷阱：在 inherit.log/init.log 路径位置放无读权限文件。实现若先读日志再查任务，
	// ReadFile 会因权限拒绝（非 ErrNotExist）返回 internal 错误，测试将 FAIL。
	taskDir := filepath.Join(logDir, "nonexistent-task")
	// 目录陷阱：在两个日志路径位置创建目录，提前 os.ReadFile 会在任何权限环境下
	// 稳定失败（EISDIR，非 ErrNotExist）→ 返回 internal 错误而非 not_found，测试将 FAIL。
	for _, name := range []string{"inherit.log", "init.log"} {
		if err := os.MkdirAll(filepath.Join(taskDir, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_, err := m.ReadInitLog(context.Background(), "nonexistent-task")
	if err == nil {
		t.Fatalf("ReadInitLog must error for nonexistent task")
	}
	if OpErrorCode(err) != codeNotFound {
		t.Fatalf("ReadInitLog error code = %q, want %q (task existence must be verified before path access)", OpErrorCode(err), codeNotFound)
	}
}

// TestReadPreDeleteLog_Tail64KB：pre-delete.log >64KB 时 tail ≤64KB，保留末尾。
func TestReadPreDeleteLog_Tail64KB(t *testing.T) {
	resetLifecycleCfgMock()
	logDir := t.TempDir()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: StatusSuspended, InitStatus: InitStatusNone}
	m := newReadInitLogManager(t, store, logDir)
	taskDir := filepath.Join(logDir, "t1")
	if err := os.MkdirAll(taskDir, 0o700); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("y", 128*1024)
	marker := "\nPREDELETE-END\n"
	if err := os.WriteFile(filepath.Join(taskDir, "pre-delete.log"), []byte(big+marker), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := m.ReadPreDeleteLog(context.Background(), "t1")
	if err != nil {
		t.Fatalf("ReadPreDeleteLog: %v", err)
	}
	if len(got) > lifecycleLogTailLimit {
		t.Fatalf("response len = %d, must be tail ≤ %d", len(got), lifecycleLogTailLimit)
	}
	if !strings.Contains(got, "PREDELETE-END") {
		t.Fatalf("tail must preserve end of pre-delete.log, got len=%d", len(got))
	}
}

// TestReadPreDeleteLog_TaskNotFound_NotFound：任务不存在 → not_found。
// 陷阱：在不存在的 taskID 的 pre-delete.log 路径放无读权限文件——若实现先读日志再查任务，
// 会返回 internal；先验任务存在则返回 not_found。
func TestReadPreDeleteLog_TaskNotFound_NotFound(t *testing.T) {
	resetLifecycleCfgMock()
	logDir := t.TempDir()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	m := newReadInitLogManager(t, store, logDir)
	taskDir := filepath.Join(logDir, "nonexistent-task")
	// 目录陷阱（同上，全权限环境稳定失败）。
	trapPath := filepath.Join(taskDir, "pre-delete.log")
	if err := os.MkdirAll(trapPath, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := m.ReadPreDeleteLog(context.Background(), "nonexistent-task")
	if err == nil {
		t.Fatalf("ReadPreDeleteLog must error for nonexistent task")
	}
	if OpErrorCode(err) != codeNotFound {
		t.Fatalf("ReadPreDeleteLog error code = %q, want %q (task existence must be verified before path access)", OpErrorCode(err), codeNotFound)
	}
}
