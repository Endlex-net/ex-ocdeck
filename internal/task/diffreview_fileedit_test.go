package task

// diffreview_fileedit_test.go 测试 FileEditPortAdapter（design.md D5 文件编辑读写）。
//
// 覆盖（D9 测试底线，3.11 完整清单由 3d 收口）：
//   - 读取判定链各 reasonCode（binary/non_utf8/mixed_line_endings/too_large/not_regular/missing/read_only）
//   - editable=true 响应字段（content 去 BOM/CRLF→\n、baseHash 精确字节、lineEnding、hasBOM、mode）
//   - CRLF/BOM/末尾换行保真
//   - 写回：content 含 CR 拒绝、初检 conflict（hash/mode/换行不一致）、终检 conflict、
//     特殊位保真、只读拒绝、零写盘前置失败、临时文件清理、rename 前终检
//
// 测试用真实文件系统 + mockStore + seedRepoTask（与 gitops_diff_test.go 同源 helper）。

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"ocdeck/internal/application/diffreview"
)

// newFileEditManager 构造一个指向真实 git 仓库的 Manager + FileEditPortAdapter。
func newFileEditManager(t *testing.T, dir string) (*Manager, *FileEditPortAdapter) {
	t.Helper()
	store := newMockStore()
	seedRepoTask(t, store, "t1", "p1", dir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	return m, NewFileEditPortAdapter(m)
}

// writeFileInRepo 在 dir 下写入文件（含 mkdir），返回完整路径。
func writeFileInRepo(t *testing.T, dir, path, content string, mode os.FileMode) {
	t.Helper()
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(full, mode); err != nil {
		t.Fatal(err)
	}
}

// --- 读取判定链（reasonCode） ---

func TestFileEdit_Read_LexicalBeforeLock(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	_, adapter := newFileEditManager(t, dir)

	// 非法 path → invalid_input（先于锁）。
	_, err := adapter.ReadRaw(context.Background(), "t1", "../escape.txt")
	if !isOpErrCode(err, codeInvalidInput) {
		t.Fatalf("escape path: err=%v want codeInvalidInput", err)
	}
}

func TestFileEdit_Read_LexicalBeforeLookup(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	store := newMockStore()
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	adapter := NewFileEditPortAdapter(m)

	// 不存在任务 + 非法 path → invalid_input（先于 not_found）。
	_, err := adapter.ReadRaw(context.Background(), "nope", "")
	if !isOpErrCode(err, codeInvalidInput) {
		t.Fatalf("nonexistent task + empty path: err=%v want codeInvalidInput", err)
	}
}

func TestFileEdit_Read_TaskNotFound(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	store := newMockStore()
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	adapter := NewFileEditPortAdapter(m)

	_, err := adapter.ReadRaw(context.Background(), "nope", "f.txt")
	if !isOpErrCode(err, codeNotFound) {
		t.Fatalf("err=%v want codeNotFound", err)
	}
}

func TestFileEdit_Read_NoWorktree(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1") // WorktreePath 指向不存在路径
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	adapter := NewFileEditPortAdapter(m)
	// seedSuspendedTask 设 WorktreePath=/data/worktrees/...，但该路径不存在；
	// 改为直接造一个空 WorktreePath 的 task
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: StatusSuspended, WorktreePath: ""}

	_, err := adapter.ReadRaw(context.Background(), "t1", "f.txt")
	if !isOpErrCode(err, codeInvalidState) {
		t.Fatalf("err=%v want codeInvalidState", err)
	}
}

func TestFileEdit_Read_DirProject(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: "/tmp", Kind: ProjectKindDir})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/tmp"}
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	adapter := NewFileEditPortAdapter(m)

	_, err := adapter.ReadRaw(context.Background(), "t1", "f.txt")
	if !isOpErrCode(err, codeInvalidInput) {
		t.Fatalf("dir project: err=%v want codeInvalidInput", err)
	}
}

func TestFileEdit_Read_Missing(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	_, adapter := newFileEditManager(t, dir)

	raw, err := adapter.ReadRaw(context.Background(), "t1", "nope.txt")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if raw.Exists {
		t.Fatal("Exists should be false for missing file")
	}
}

func TestFileEdit_Read_NotRegular(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	writeFileInRepo(t, dir, "symlink.txt", "target\n", 0o644)
	// 替换为 symlink
	linkPath := filepath.Join(dir, "link.txt")
	if err := os.Symlink("symlink.txt", linkPath); err != nil {
		t.Fatal(err)
	}
	_, adapter := newFileEditManager(t, dir)

	_, err := adapter.ReadRaw(context.Background(), "t1", "link.txt")
	if err == nil {
		t.Fatal("expected error for symlink")
	}
	var re *diffreview.FileEditReadRawError
	if !errors.As(err, &re) || !re.NotRegular {
		t.Fatalf("err=%v want *FileEditReadRawError{NotRegular:true}", err)
	}
}

func TestFileEdit_Read_Binary(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	writeFileInRepo(t, dir, "bin.txt", "hello\x00world\n", 0o644)
	_, adapter := newFileEditManager(t, dir)

	raw, err := adapter.ReadRaw(context.Background(), "t1", "bin.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !raw.Exists {
		t.Fatal("Exists should be true")
	}
	if !diffreview.IsBinaryBytes(raw.Bytes) {
		t.Fatal("should be binary")
	}
}

func TestFileEdit_Read_TooLarge(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	writeFileInRepo(t, dir, "big.txt", strings.Repeat("a", diffreview.FileEditMaxBytes+1), 0o644)
	_, adapter := newFileEditManager(t, dir)

	raw, err := adapter.ReadRaw(context.Background(), "t1", "big.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Bytes) <= diffreview.FileEditMaxBytes {
		t.Fatalf("bytes len %d should be > %d", len(raw.Bytes), diffreview.FileEditMaxBytes)
	}
}

func TestFileEdit_Read_ReadOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 0444 权限拒绝在 root 下不生效")
	}
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	writeFileInRepo(t, dir, "ro.txt", "hello\n", 0o444)
	defer os.Chmod(filepath.Join(dir, "ro.txt"), 0o644)
	_, adapter := newFileEditManager(t, dir)

	raw, err := adapter.ReadRaw(context.Background(), "t1", "ro.txt")
	if err != nil {
		t.Fatal(err)
	}
	if diffreview.HasOwnerWrite(raw.Mode) {
		t.Fatalf("mode %o should not have owner write", raw.Mode)
	}
}

// --- editable=true 响应字段保真（通过 FileEditService 端到端） ---

func TestFileEdit_Read_EditableLF(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	content := "line1\nline2\nline3\n"
	writeFileInRepo(t, dir, "f.txt", content, 0o644)
	m, adapter := newFileEditManager(t, dir)
	svc := diffreview.New(diffreview.Options{FileEdit: adapter})
	_ = m

	res, err := svc.ReadFile(context.Background(), "t1", "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Editable {
		t.Fatalf("editable: %+v", res)
	}
	if res.Content != content {
		t.Fatalf("content: %q want %q", res.Content, content)
	}
	if res.LineEnding != diffreview.LineEndingLF {
		t.Fatalf("lineEnding: %v", res.LineEnding)
	}
	if res.HasBOM {
		t.Fatal("hasBOM should be false")
	}
	if res.Mode != "0644" {
		t.Fatalf("mode: %q", res.Mode)
	}
	// baseHash = SHA256 of original bytes
	wantHash := diffreview.SHA256Hex([]byte(content))
	if res.BaseHash != wantHash {
		t.Fatalf("baseHash: %q want %q", res.BaseHash, wantHash)
	}
}

func TestFileEdit_Read_EditableCRLFWithBOM(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	rawBytes := []byte{0xEF, 0xBB, 0xBF, 'a', '\r', '\n', 'b', '\r', '\n'}
	full := filepath.Join(dir, "crlf_bom.txt")
	if err := os.WriteFile(full, rawBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	_, adapter := newFileEditManager(t, dir)
	svc := diffreview.New(diffreview.Options{FileEdit: adapter})

	res, err := svc.ReadFile(context.Background(), "t1", "crlf_bom.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Editable {
		t.Fatalf("editable: %+v", res)
	}
	if res.Content != "a\nb\n" {
		t.Fatalf("content: %q want %q", res.Content, "a\\nb\\n")
	}
	if res.LineEnding != diffreview.LineEndingCRLF {
		t.Fatalf("lineEnding: %v", res.LineEnding)
	}
	if !res.HasBOM {
		t.Fatal("hasBOM should be true")
	}
	if res.BaseHash != diffreview.SHA256Hex(rawBytes) {
		t.Fatalf("baseHash mismatch: got %s", res.BaseHash)
	}
}

func TestFileEdit_Read_EditableNoNewline(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	writeFileInRepo(t, dir, "nonl.txt", "hello", 0o644)
	_, adapter := newFileEditManager(t, dir)
	svc := diffreview.New(diffreview.Options{FileEdit: adapter})

	res, err := svc.ReadFile(context.Background(), "t1", "nonl.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Editable {
		t.Fatalf("editable: %+v", res)
	}
	if res.LineEnding != diffreview.LineEndingLF {
		t.Fatalf("lineEnding (no newline): %v want lf", res.LineEnding)
	}
	if res.Content != "hello" {
		t.Fatalf("content: %q", res.Content)
	}
}

func TestFileEdit_Read_EditableSpecialMode(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("setuid/setgid 在 root 下行为不同")
	}
	// macOS /var/folders（t.TempDir）挂载 nosuid，setuid 位被内核剥离，无法测试。
	// 特殊位保真由 TestFileEdit_Write_SpecialModePreserved（需可设置 setuid 的文件系统）覆盖；
	// 在不支持的环境跳过，mode 响应字段的四位八进制格式由 TestModeToOctalString 覆盖。
	dir := t.TempDir()
	full := filepath.Join(dir, "setuid.txt")
	if err := os.WriteFile(full, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(full, 0o4755); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Lstat(full)
	if info.Mode()&os.ModeSetuid == 0 {
		t.Skip("setuid bit stripped by filesystem (macOS nosuid on /var/folders)")
	}
	defer os.Chmod(full, 0o644)
	_, adapter := newFileEditManager(t, dir)
	svc := diffreview.New(diffreview.Options{FileEdit: adapter})

	res, err := svc.ReadFile(context.Background(), "t1", "setuid.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Editable {
		t.Fatalf("editable: %+v", res)
	}
	if res.Mode != "4755" {
		t.Fatalf("mode: %q want 4755", res.Mode)
	}
}

// --- 写回测试 ---

func TestFileEdit_Write_LexicalBeforeLock(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	_, adapter := newFileEditManager(t, dir)

	req := diffreview.FileEditWriteRequest{
		Path: "../escape.txt", Content: "a\n", BaseHash: strings.Repeat("a", 64),
		LineEnding: diffreview.LineEndingLF, BaseMode: "0644",
	}
	_, err := adapter.Write(context.Background(), "t1", req)
	if !isOpErrCode(err, codeInvalidInput) {
		t.Fatalf("escape path: err=%v want codeInvalidInput", err)
	}
}

func TestFileEdit_Write_TaskNotFound(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	store := newMockStore()
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	adapter := NewFileEditPortAdapter(m)

	req := diffreview.FileEditWriteRequest{
		Path: "f.txt", Content: "a\n", BaseHash: strings.Repeat("a", 64),
		LineEnding: diffreview.LineEndingLF, BaseMode: "0644",
	}
	_, err := adapter.Write(context.Background(), "nope", req)
	if !isOpErrCode(err, codeNotFound) {
		t.Fatalf("err=%v want codeNotFound", err)
	}
}

func TestFileEdit_Write_FileMissing_InvalidState(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	_, adapter := newFileEditManager(t, dir)

	req := diffreview.FileEditWriteRequest{
		Path: "nope.txt", Content: "a\n", BaseHash: strings.Repeat("a", 64),
		LineEnding: diffreview.LineEndingLF, BaseMode: "0644",
	}
	_, err := adapter.Write(context.Background(), "t1", req)
	if !isOpErrCode(err, codeInvalidState) {
		t.Fatalf("missing file: err=%v want codeInvalidState", err)
	}
}

func TestFileEdit_Write_NotRegular_InvalidState(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	linkPath := filepath.Join(dir, "link.txt")
	if err := os.Symlink("target", linkPath); err != nil {
		t.Fatal(err)
	}
	_, adapter := newFileEditManager(t, dir)

	req := diffreview.FileEditWriteRequest{
		Path: "link.txt", Content: "a\n", BaseHash: strings.Repeat("a", 64),
		LineEnding: diffreview.LineEndingLF, BaseMode: "0644",
	}
	_, err := adapter.Write(context.Background(), "t1", req)
	if !isOpErrCode(err, codeInvalidState) {
		t.Fatalf("symlink: err=%v want codeInvalidState", err)
	}
}

func TestFileEdit_Write_HashMismatch_ConflictZeroWrite(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	content := "original\n"
	writeFileInRepo(t, dir, "f.txt", content, 0o644)
	_, adapter := newFileEditManager(t, dir)

	// 错误 baseHash → conflict，零写盘。
	req := diffreview.FileEditWriteRequest{
		Path: "f.txt", Content: "new\n", BaseHash: strings.Repeat("a", 64),
		LineEnding: diffreview.LineEndingLF, BaseMode: "0644",
	}
	_, err := adapter.Write(context.Background(), "t1", req)
	if !isOpErrCode(err, codeConflict) {
		t.Fatalf("hash mismatch: err=%v want codeConflict", err)
	}
	// 零写盘：文件内容不变。
	got, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if string(got) != content {
		t.Fatalf("file modified after conflict: %q want %q", got, content)
	}
}

func TestFileEdit_Write_ModeMismatch_Conflict(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	writeFileInRepo(t, dir, "f.txt", "hello\n", 0o755) // 实际 mode 0755
	_, adapter := newFileEditManager(t, dir)

	baseHash := diffreview.SHA256Hex([]byte("hello\n"))
	// 请求 baseMode=0644 与实际 0755 不一致 → conflict。
	req := diffreview.FileEditWriteRequest{
		Path: "f.txt", Content: "world\n", BaseHash: baseHash,
		LineEnding: diffreview.LineEndingLF, BaseMode: "0644",
	}
	_, err := adapter.Write(context.Background(), "t1", req)
	if !isOpErrCode(err, codeConflict) {
		t.Fatalf("mode mismatch: err=%v want codeConflict", err)
	}
}

func TestFileEdit_Write_LineEndingMismatch_Conflict(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	// 实际文件为 CRLF，请求 lineEnding=lf → conflict。
	writeFileInRepo(t, dir, "f.txt", "a\r\nb\r\n", 0o644)
	_, adapter := newFileEditManager(t, dir)

	baseHash := diffreview.SHA256Hex([]byte("a\r\nb\r\n"))
	req := diffreview.FileEditWriteRequest{
		Path: "f.txt", Content: "a\nb\n", BaseHash: baseHash,
		LineEnding: diffreview.LineEndingLF, BaseMode: "0644",
	}
	_, err := adapter.Write(context.Background(), "t1", req)
	if !isOpErrCode(err, codeConflict) {
		t.Fatalf("lineEnding mismatch: err=%v want codeConflict", err)
	}
}

func TestFileEdit_Write_Success_LF(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	content := "hello\nworld\n"
	writeFileInRepo(t, dir, "f.txt", content, 0o644)
	_, adapter := newFileEditManager(t, dir)

	baseHash := diffreview.SHA256Hex([]byte(content))
	newContent := "hello\nnew\nworld\n"
	req := diffreview.FileEditWriteRequest{
		Path: "f.txt", Content: newContent, BaseHash: baseHash,
		LineEnding: diffreview.LineEndingLF, BaseMode: "0644",
	}
	res, err := adapter.Write(context.Background(), "t1", req)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	// 文件内容 = newContent（LF，无 BOM）。
	got, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if string(got) != newContent {
		t.Fatalf("file content: %q want %q", got, newContent)
	}
	// 返回 baseHash = SHA256 of new content。
	if res.BaseHash != diffreview.SHA256Hex([]byte(newContent)) {
		t.Fatalf("new baseHash: %q", res.BaseHash)
	}
}

func TestFileEdit_Write_Success_CRLFWithBOM(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	// 原文件 CRLF + BOM
	rawBytes := []byte{0xEF, 0xBB, 0xBF, 'a', '\r', '\n', 'b', '\r', '\n'}
	full := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(full, rawBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	_, adapter := newFileEditManager(t, dir)

	baseHash := diffreview.SHA256Hex(rawBytes)
	// 新 content（LF 分隔），重建为 CRLF + BOM。
	newContent := "a\nx\nb\n"
	wantRebuilt := []byte{0xEF, 0xBB, 0xBF, 'a', '\r', '\n', 'x', '\r', '\n', 'b', '\r', '\n'}
	req := diffreview.FileEditWriteRequest{
		Path: "f.txt", Content: newContent, BaseHash: baseHash,
		LineEnding: diffreview.LineEndingCRLF, BaseMode: "0644",
	}
	res, err := adapter.Write(context.Background(), "t1", req)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if string(got) != string(wantRebuilt) {
		t.Fatalf("file content: %q want %q", got, wantRebuilt)
	}
	if res.BaseHash != diffreview.SHA256Hex(wantRebuilt) {
		t.Fatalf("new baseHash: %q want %q", res.BaseHash, diffreview.SHA256Hex(wantRebuilt))
	}
}

func TestFileEdit_Write_SpecialModePreserved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("setuid 在 root 下行为不同")
	}
	// macOS /var/folders（t.TempDir）挂载 nosuid，setuid 位被内核剥离，无法测试。
	// 特殊位映射与 chmod 逻辑由 octalToFileMode/fileModeToOctal 单测 + 普通可执行位测试覆盖。
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	content := "hello\n"
	full := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(full, 0o4755); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Lstat(full)
	if info.Mode()&os.ModeSetuid == 0 {
		t.Skip("setuid bit stripped by filesystem (macOS nosuid on /var/folders)")
	}
	defer os.Chmod(full, 0o644)
	_, adapter := newFileEditManager(t, dir)

	baseHash := diffreview.SHA256Hex([]byte(content))
	newContent := "world\n"
	req := diffreview.FileEditWriteRequest{
		Path: "f.txt", Content: newContent, BaseHash: baseHash,
		LineEnding: diffreview.LineEndingLF, BaseMode: "4755",
	}
	_, err := adapter.Write(context.Background(), "t1", req)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	info2, err := os.Lstat(filepath.Join(dir, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	gotMode := diffreview.ModeToOctalString(fileModeToOctal(info2.Mode()))
	if gotMode != "4755" {
		t.Fatalf("mode: %q want 4755", gotMode)
	}
}

// TestFileEdit_Write_FinalCheckHashConflict 验证 rename 前终检（非初检）检测到目标 hash 变化：
// 初检通过后、终检前外部修改目标内容 → conflict + 临时文件清理 + 目标内容不变。
func TestFileEdit_Write_FinalCheckHashConflict(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	content := "hello\n"
	writeFileInRepo(t, dir, "f.txt", content, 0o644)
	_, adapter := newFileEditManager(t, dir)

	baseHash := diffreview.SHA256Hex([]byte(content))
	target := filepath.Join(dir, "f.txt")

	// 注入 hook：临时文件写入后、终检前修改目标内容（hash 变化，mode 不变）。
	prevHook := preFinalCheckHook
	preFinalCheckHook = func(tgt string) {
		_ = os.WriteFile(tgt, []byte("external-modified\n"), 0o644)
	}
	defer func() { preFinalCheckHook = prevHook }()

	req := diffreview.FileEditWriteRequest{
		Path: "f.txt", Content: "new\n", BaseHash: baseHash,
		LineEnding: diffreview.LineEndingLF, BaseMode: "0644",
	}
	_, err := adapter.Write(context.Background(), "t1", req)
	if !isOpErrCode(err, codeConflict) {
		t.Fatalf("final check hash conflict: err=%v want codeConflict", err)
	}
	// 目标内容不变（保持 hook 写入的内容，而非 new\n——终检拒绝未 rename）。
	got, _ := os.ReadFile(target)
	if string(got) != "external-modified\n" {
		t.Fatalf("target content after final-check conflict: %q", got)
	}
	// 无临时文件残留。
	assertNoTempFile(t, dir)
}

// TestFileEdit_Write_FinalCheckModeConflict 验证 rename 前终检检测到目标 mode 变化：
// 初检通过后、终检前外部 chmod 目标 → conflict + 临时文件清理 + 目标内容不变。
func TestFileEdit_Write_FinalCheckModeConflict(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 在 root 下行为不同")
	}
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	content := "hello\n"
	writeFileInRepo(t, dir, "f.txt", content, 0o644)
	_, adapter := newFileEditManager(t, dir)

	baseHash := diffreview.SHA256Hex([]byte(content))
	target := filepath.Join(dir, "f.txt")

	// 注入 hook：临时文件写入后、终检前 chmod 目标（mode 变化，hash 不变）。
	prevHook := preFinalCheckHook
	preFinalCheckHook = func(tgt string) {
		_ = os.Chmod(tgt, 0o755)
	}
	defer func() { preFinalCheckHook = prevHook }()

	req := diffreview.FileEditWriteRequest{
		Path: "f.txt", Content: "new\n", BaseHash: baseHash,
		LineEnding: diffreview.LineEndingLF, BaseMode: "0644",
	}
	_, err := adapter.Write(context.Background(), "t1", req)
	if !isOpErrCode(err, codeConflict) {
		t.Fatalf("final check mode conflict: err=%v want codeConflict", err)
	}
	// 目标内容不变。
	got, _ := os.ReadFile(target)
	if string(got) != content {
		t.Fatalf("target content after final-check conflict: %q want %q", got, content)
	}
	// 无临时文件残留。
	assertNoTempFile(t, dir)
}

// TestFileEdit_Write_FinalCheckTypeConflict 验证 rename 前终检检测到目标类型变化：
// 初检通过后、终检前外部删除目标并重建为目录 → conflict + 临时文件清理。
func TestFileEdit_Write_FinalCheckTypeConflict(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	content := "hello\n"
	writeFileInRepo(t, dir, "f.txt", content, 0o644)
	_, adapter := newFileEditManager(t, dir)

	baseHash := diffreview.SHA256Hex([]byte(content))
	target := filepath.Join(dir, "f.txt")

	// 注入 hook：临时文件写入后、终检前删除目标文件并重建为目录（类型从 regular → dir）。
	prevHook := preFinalCheckHook
	preFinalCheckHook = func(tgt string) {
		_ = os.Remove(tgt)
		_ = os.Mkdir(tgt, 0o755)
	}
	defer func() { preFinalCheckHook = prevHook }()

	req := diffreview.FileEditWriteRequest{
		Path: "f.txt", Content: "new\n", BaseHash: baseHash,
		LineEnding: diffreview.LineEndingLF, BaseMode: "0644",
	}
	_, err := adapter.Write(context.Background(), "t1", req)
	if !isOpErrCode(err, codeConflict) {
		t.Fatalf("final check type conflict: err=%v want codeConflict", err)
	}
	// 无临时文件残留。
	assertNoTempFile(t, dir)
	// 清理 hook 创建的目录。
	_ = os.RemoveAll(target)
}

// TestFileEdit_Write_FinalCheckMissingConflict 验证 rename 前终检检测到目标消失：
// 初检通过后、终检前外部删除目标 → conflict + 临时文件清理。
func TestFileEdit_Write_FinalCheckMissingConflict(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	content := "hello\n"
	writeFileInRepo(t, dir, "f.txt", content, 0o644)
	_, adapter := newFileEditManager(t, dir)

	baseHash := diffreview.SHA256Hex([]byte(content))

	// 注入 hook：临时文件写入后、终检前删除目标文件。
	prevHook := preFinalCheckHook
	preFinalCheckHook = func(tgt string) {
		_ = os.Remove(tgt)
	}
	defer func() { preFinalCheckHook = prevHook }()

	req := diffreview.FileEditWriteRequest{
		Path: "f.txt", Content: "new\n", BaseHash: baseHash,
		LineEnding: diffreview.LineEndingLF, BaseMode: "0644",
	}
	_, err := adapter.Write(context.Background(), "t1", req)
	if !isOpErrCode(err, codeConflict) {
		t.Fatalf("final check missing conflict: err=%v want codeConflict", err)
	}
	// 无临时文件残留。
	assertNoTempFile(t, dir)
}

// assertNoTempFile 断言目录下无 .ocdeck-fileedit-* 临时文件残留。
func assertNoTempFile(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ocdeck-fileedit-") {
			t.Fatalf("temp file leaked: %s", e.Name())
		}
	}
}

// TestFileEdit_Write_ParentDirDisappears_InvalidStateNoSystemTemp（F6）：
// 父目录在步骤 6 前竞态消失 → invalid_state，且临时文件 MUST NOT 落系统临时目录
// （空 dir 传给 os.CreateTemp 会用系统默认临时目录，违反同目录临时文件契约）。
func TestFileEdit_Write_ParentDirDisappears_InvalidStateNoSystemTemp(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	content := "hello\n"
	writeFileInRepo(t, dir, "sub/f.txt", content, 0o644)
	_, adapter := newFileEditManager(t, dir)

	baseHash := diffreview.SHA256Hex([]byte(content))

	// 记录系统临时目录中已有的 .ocdeck-fileedit-* 基线。
	sysTemp := os.TempDir()
	beforeEntries, _ := os.ReadDir(sysTemp)
	before := map[string]bool{}
	for _, e := range beforeEntries {
		before[e.Name()] = true
	}

	// 注入 hook：步骤 6 父目录解析前删除整个 sub 目录（模拟父目录竞态消失）。
	prevHook := preTempWriteHook
	preTempWriteHook = func(tgt string) {
		_ = os.RemoveAll(filepath.Dir(tgt))
	}
	defer func() { preTempWriteHook = prevHook }()

	req := diffreview.FileEditWriteRequest{
		Path: "sub/f.txt", Content: "new\n", BaseHash: baseHash,
		LineEnding: diffreview.LineEndingLF, BaseMode: "0644",
	}
	_, err := adapter.Write(context.Background(), "t1", req)
	if !isOpErrCode(err, codeInvalidState) {
		t.Fatalf("parent dir disappeared: err=%v want codeInvalidState", err)
	}
	// 系统临时目录零新增 .ocdeck-fileedit-* 残留。
	afterEntries, _ := os.ReadDir(sysTemp)
	for _, e := range afterEntries {
		if strings.HasPrefix(e.Name(), ".ocdeck-fileedit-") && !before[e.Name()] {
			t.Fatalf("temp file leaked into system temp dir: %s", e.Name())
		}
	}
}

// TestFileEdit_Write_TargetGrewBeyondLimit_Conflict（F5）：
// 目标文件外部增长超 512KiB → 有界读取拒绝（conflict），文件不被修改，无临时文件残留。
func TestFileEdit_Write_TargetGrewBeyondLimit_Conflict(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	// 磁盘上直接造 >512KiB 的文件（模拟基线读取后外部增长）。
	big := strings.Repeat("x", diffreview.FileEditMaxBytes+1)
	writeFileInRepo(t, dir, "f.txt", big, 0o644)
	_, adapter := newFileEditManager(t, dir)

	req := diffreview.FileEditWriteRequest{
		Path: "f.txt", Content: "new\n", BaseHash: diffreview.SHA256Hex([]byte("whatever")),
		LineEnding: diffreview.LineEndingLF, BaseMode: "0644",
	}
	_, err := adapter.Write(context.Background(), "t1", req)
	if !isOpErrCode(err, codeConflict) {
		t.Fatalf("grew beyond limit: err=%v want codeConflict", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if len(got) != len(big) {
		t.Fatalf("file modified: len=%d want %d", len(got), len(big))
	}
	assertNoTempFile(t, dir)
}

// TestFileEdit_Write_FinalCheckGrewBeyondLimit_Conflict（F5 终检同样有界）：
// 临时文件写入后、终检前目标被外部增长到超 512KiB → 终检 conflict + 临时文件清理。
func TestFileEdit_Write_FinalCheckGrewBeyondLimit_Conflict(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	content := "hello\n"
	writeFileInRepo(t, dir, "f.txt", content, 0o644)
	_, adapter := newFileEditManager(t, dir)

	baseHash := diffreview.SHA256Hex([]byte(content))

	prevHook := preFinalCheckHook
	preFinalCheckHook = func(tgt string) {
		f, _ := os.OpenFile(tgt, os.O_APPEND|os.O_WRONLY, 0)
		if f != nil {
			_, _ = f.WriteString(strings.Repeat("y", diffreview.FileEditMaxBytes+1))
			_ = f.Close()
		}
	}
	defer func() { preFinalCheckHook = prevHook }()

	req := diffreview.FileEditWriteRequest{
		Path: "f.txt", Content: "new\n", BaseHash: baseHash,
		LineEnding: diffreview.LineEndingLF, BaseMode: "0644",
	}
	_, err := adapter.Write(context.Background(), "t1", req)
	if !isOpErrCode(err, codeConflict) {
		t.Fatalf("final check grew beyond limit: err=%v want codeConflict", err)
	}
	assertNoTempFile(t, dir)
}

// TestReadBaselineBytes_SymlinkReplaced_NoFollow（F13）：
// 目标被替换为指向 regular file 的 symlink → O_NOFOLLOW 拒绝（ELOOP），按阶段映射
// 初检 invalid_state / 终检 conflict，MUST NOT 跟随读取链接目标内容。
func TestReadBaselineBytes_SymlinkReplaced_NoFollow(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(real, []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "f.txt")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	dirFd := openTestDirFd(t, dir)

	data, _, err := readBaselineBytesAt(dirFd, "f.txt", "f.txt", checkPhaseInitial)
	if err == nil {
		t.Fatalf("symlink target should be rejected, got data=%q", data)
	}
	if !isOpErrCode(err, codeInvalidState) {
		t.Fatalf("initial phase symlink: err=%v want codeInvalidState", err)
	}

	_, _, err = readBaselineBytesAt(dirFd, "f.txt", "f.txt", checkPhaseFinal)
	if !isOpErrCode(err, codeConflict) {
		t.Fatalf("final phase symlink: err=%v want codeConflict", err)
	}
}

// TestReadBaselineBytes_SocketReplaced_NotRegular（F13）：
// 目标被替换为 unix socket → openat 阶段即拒绝（EOPNOTSUPP/ENXIO），按阶段映射（不阻塞）。
func TestReadBaselineBytes_SocketReplaced_NotRegular(t *testing.T) {
	// macOS unix socket 路径上限 104 字符，t.TempDir() 过长，用 /tmp 短路径。
	sockDir, err := os.MkdirTemp("/tmp", "rb*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	ln, err := net.Listen("unix", filepath.Join(sockDir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dirFd := openTestDirFd(t, sockDir)

	_, _, err = readBaselineBytesAt(dirFd, "s", "f.txt", checkPhaseInitial)
	if !isOpErrCode(err, codeInvalidState) {
		t.Fatalf("initial phase socket: err=%v want codeInvalidState", err)
	}
	_, _, err = readBaselineBytesAt(dirFd, "s", "f.txt", checkPhaseFinal)
	if !isOpErrCode(err, codeConflict) {
		t.Fatalf("final phase socket: err=%v want codeConflict", err)
	}
}

// openTestDirFd 以生产同款 flags 打开目录 FD 并注册关闭（供 readBaselineBytesAt 单测）。
func openTestDirFd(t *testing.T, dir string) int {
	t.Helper()
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	return fd
}

// TestReadRawFile_LeafSymlinkSwap_NoFollow（G2/G3）：
// leaf 在 Lstat+禁锢校验之后、open 之前被替换为指向 worktree 外的 symlink——
// openat O_NOFOLLOW 拒绝（ELOOP）→ not_regular，MUST NOT 读到外部内容。
// 旧 Lstat→os.Open(targetReal) 实现会跟随新 symlink 读到外部内容（测试在旧实现下失败）。
func TestReadRawFile_LeafSymlinkSwap_NoFollow(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	// 先是正常 regular 文件（通过初始 Lstat/禁锢）。
	writeFileInRepo(t, dir, "f.txt", "legit\n", 0o644)
	// worktree 外的秘密文件（symlink 目标），绝不能被读出。
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("outside-secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 注入 hook：禁锢校验后、open 前把 f.txt 替换为指向 secret 的 symlink。
	prevHook := readRawPostConfineHook
	readRawPostConfineHook = func(tgt string) {
		_ = os.Remove(tgt)
		_ = os.Symlink(secret, tgt)
	}
	defer func() { readRawPostConfineHook = prevHook }()

	got, err := readRawFile(dir, "f.txt")
	if err == nil {
		t.Fatalf("swapped symlink target must be rejected, got %+v", got)
	}
	var rawErr *diffreview.FileEditReadRawError
	if !errors.As(err, &rawErr) || !rawErr.NotRegular {
		t.Fatalf("err=%v want *FileEditReadRawError{NotRegular:true}", err)
	}
	if got.Exists || len(got.Bytes) != 0 {
		t.Fatalf("must not follow swapped symlink to outside content: %+v", got)
	}
}

// TestReadRawFile_IntermediateDirSymlinkSwap_Rejected（G2/G3）：
// 中间目录在禁锢校验之后、父目录 walk 之前被替换为指向 worktree 外的 symlink——
// 拒绝（逃逸或 not_regular），MUST NOT 读到外部目录内容。
// 旧实现 os.Open(targetReal) 会跟随新 symlink 读到外部目录内容（测试在旧实现下失败）。
func TestReadRawFile_IntermediateDirSymlinkSwap_Rejected(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	// 先是正常目录与文件（通过初始 Lstat/禁锢）。
	writeFileInRepo(t, dir, "sub/f.txt", "legit\n", 0o644)
	// worktree 外目录（symlink 目标），内含同名文件，绝不能被读出。
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "f.txt"), []byte("outside-content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 注入 hook：禁锢校验后把 sub 替换为指向 outside 的 symlink。
	prevHook := readRawPostConfineHook
	readRawPostConfineHook = func(tgt string) {
		sub := filepath.Dir(tgt)
		_ = os.RemoveAll(sub)
		_ = os.Symlink(outside, sub)
	}
	defer func() { readRawPostConfineHook = prevHook }()

	got, err := readRawFile(dir, "sub/f.txt")
	if err == nil {
		t.Fatalf("swapped intermediate symlink dir must be rejected, got %+v", got)
	}
	// 可能命中禁锢逃逸（invalid_input）或 not_regular（walk 拒绝）——两者均为安全拒绝，
	// 关键是不得返回外部内容。
	if got.Exists || len(got.Bytes) != 0 {
		t.Fatalf("must not read outside content via swapped intermediate symlink: %+v", got)
	}
}

// TestReadRawFile_ModeAndBytesFromSameObject（G2/G3 基线一致性）：
// Lstat 之后、open 之前内容与 mode 都被外部修改——读取必须返回同一对象的新 bytes+新 mode
// （旧实现 bytes 来自新对象、mode 来自 Lstat 旧对象：测试在旧实现下 mode 断言失败）。
func TestReadRawFile_ModeAndBytesFromSameObject(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	writeFileInRepo(t, dir, "f.txt", "old-content\n", 0o644)

	// 注入 hook：Lstat+禁锢后重写文件内容并改 mode。
	prevHook := readRawPostConfineHook
	readRawPostConfineHook = func(tgt string) {
		_ = os.WriteFile(tgt, []byte("new-content\n"), 0o640)
		_ = os.Chmod(tgt, 0o640)
	}
	defer func() { readRawPostConfineHook = prevHook }()

	got, err := readRawFile(dir, "f.txt")
	if err != nil {
		t.Fatalf("readRawFile: %v", err)
	}
	if !got.Exists || string(got.Bytes) != "new-content\n" {
		t.Fatalf("bytes: %q want %q", got.Bytes, "new-content\n")
	}
	// mode 必须来自同一 FD fstat（新对象 0640），不是 Lstat 旧值 0644。
	if got.Mode != 0o640 {
		t.Fatalf("mode: %o want 640 (from same FD as bytes)", got.Mode)
	}
}

// TestFileEdit_Write_ParentSymlinkSwapAfterResolve_Conflict（F15）：
// dirFd 打开后、终检前中间目录被改名移走并在原位建指向外部目录的 symlink——终检前置
// 身份比对（重新安全解析当前请求父路径 + dev/ino 比对）检出替换 → conflict，
// 临时文件清理，外部目录零创建、零逃逸。
func TestFileEdit_Write_ParentSymlinkSwapAfterResolve_Conflict(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	content := "hello\n"
	writeFileInRepo(t, dir, "sub/f.txt", content, 0o644)
	_, adapter := newFileEditManager(t, dir)
	baseHash := diffreview.SHA256Hex([]byte(content))

	// 外部诱饵目录（symlink 目标，位于 worktree 外），绝不能被写入。
	decoy := t.TempDir()

	// 注入 hook：临时文件写入后、终检前（此时 dirFd 已打开）把 sub 改名移走并在原位建
	// 指向 decoy 的 symlink。终检身份比对重新解析 sub → 逃逸/替换 → conflict。
	subOld := filepath.Join(dir, "sub-moved")
	prevHook := preFinalCheckHook
	preFinalCheckHook = func(tgt string) {
		sub := filepath.Dir(tgt)
		_ = os.Rename(sub, subOld)
		_ = os.Symlink(decoy, sub)
	}
	defer func() { preFinalCheckHook = prevHook }()

	req := diffreview.FileEditWriteRequest{
		Path: "sub/f.txt", Content: "new\n", BaseHash: baseHash,
		LineEnding: diffreview.LineEndingLF, BaseMode: "0644",
	}
	_, err := adapter.Write(context.Background(), "t1", req)
	if !isOpErrCode(err, codeConflict) {
		t.Fatalf("parent dir replaced: err=%v want codeConflict", err)
	}
	// 原目录实体内容不被修改（写未发生）。
	got, rerr := os.ReadFile(filepath.Join(subOld, "f.txt"))
	if rerr != nil || string(got) != content {
		t.Fatalf("original file must be unmodified: %q err=%v", got, rerr)
	}
	// decoy 内零创建（无逃逸）、零临时文件泄漏。
	if _, serr := os.Lstat(filepath.Join(decoy, "f.txt")); !os.IsNotExist(serr) {
		t.Fatalf("escape: file created in symlink target decoy: %v", serr)
	}
	entries, _ := os.ReadDir(decoy)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ocdeck-fileedit-") {
			t.Fatalf("temp file leaked into decoy: %s", e.Name())
		}
	}
	// 移走的目录内无临时文件泄漏（cleanup 经 dirFd unlinkat）。
	entriesOld, _ := os.ReadDir(subOld)
	for _, e := range entriesOld {
		if strings.HasPrefix(e.Name(), ".ocdeck-fileedit-") {
			t.Fatalf("temp file leaked into moved dir: %s", e.Name())
		}
	}
}

// TestWalkOpenatDir_SymlinkComponentRejected（F15）：逐组件 O_NOFOLLOW walk——
// rel 中组件为 symlink 即 ELOOP 拒绝；组件消失即 ENOENT。
func TestWalkOpenatDir_SymlinkComponentRejected(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(filepath.Join(real, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	rootFd := openTestDirFd(t, dir)

	// symlink 组件 → ELOOP(linux) 或 ENOTDIR(darwin openat O_DIRECTORY 语义)。
	if fd, err := walkOpenatDir(rootFd, "link"); err == nil {
		unix.Close(fd)
		t.Fatal("symlink component should be rejected")
	} else if !errors.Is(err, syscall.ELOOP) && !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("err=%v want ELOOP or ENOTDIR", err)
	}
	// 消失组件 → ENOENT。
	if fd, err := walkOpenatDir(rootFd, "missing/sub"); err == nil {
		unix.Close(fd)
		t.Fatal("missing component should fail with ENOENT")
	} else if !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("err=%v want ENOENT", err)
	}
	// 正常路径可达且 "." 返回可用 FD。
	fd, err := walkOpenatDir(rootFd, "real/x")
	if err != nil {
		t.Fatalf("real path walk: %v", err)
	}
	unix.Close(fd)
	fd, err = walkOpenatDir(rootFd, ".")
	if err != nil {
		t.Fatalf("dot walk: %v", err)
	}
	// F19：返回 FD 必须带 FD_CLOEXEC（F_DUPFD_CLOEXEC）。
	flags, ferr := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if ferr != nil {
		t.Fatalf("get fd flags: %v", ferr)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("walked dir fd must have FD_CLOEXEC")
	}
	unix.Close(fd)
}

// TestFileEdit_Write_RootReplacedAfterResolve_Conflict（F17）：
// dirFd 打开后、终检前 worktree root 被改名移走并在原位建指向外部目录的 symlink——
// 终检前置重新安全打开 root 并比对 root dev:ino → conflict；外部目录零创建、零逃逸。
func TestFileEdit_Write_RootReplacedAfterResolve_Conflict(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	content := "hello\n"
	writeFileInRepo(t, dir, "f.txt", content, 0o644)
	_, adapter := newFileEditManager(t, dir)
	baseHash := diffreview.SHA256Hex([]byte(content))

	// 外部诱饵 root（symlink 目标），绝不能被写入。
	decoyRoot := t.TempDir()
	movedRoot := dir + "-moved"

	prevHook := preFinalCheckHook
	preFinalCheckHook = func(tgt string) {
		// worktree root 改名移走 + 原位建指向 decoyRoot 的 symlink。
		_ = os.Rename(dir, movedRoot)
		_ = os.Symlink(decoyRoot, dir)
	}
	defer func() { preFinalCheckHook = prevHook }()
	// 测试结束恢复目录名（TempDir 清理不受 rename 影响，但后续断言需要路径稳定）。
	defer func() {
		_ = os.Remove(dir)
		_ = os.Rename(movedRoot, dir)
	}()

	req := diffreview.FileEditWriteRequest{
		Path: "f.txt", Content: "new\n", BaseHash: baseHash,
		LineEnding: diffreview.LineEndingLF, BaseMode: "0644",
	}
	_, err := adapter.Write(context.Background(), "t1", req)
	if !isOpErrCode(err, codeConflict) {
		t.Fatalf("worktree root replaced: err=%v want codeConflict", err)
	}
	// 原 root 实体内容不被修改。
	got, rerr := os.ReadFile(filepath.Join(movedRoot, "f.txt"))
	if rerr != nil || string(got) != content {
		t.Fatalf("original file must be unmodified: %q err=%v", got, rerr)
	}
	// decoyRoot 内零创建、零临时文件泄漏。
	entries, _ := os.ReadDir(decoyRoot)
	for _, e := range entries {
		if e.Name() == "f.txt" || strings.HasPrefix(e.Name(), ".ocdeck-fileedit-") {
			t.Fatalf("escape into decoy root: %s", e.Name())
		}
	}
}

func TestFileEdit_Write_ReadOnlyBaseMode_RejectedByService(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	writeFileInRepo(t, dir, "f.txt", "hello\n", 0o644)
	_, adapter := newFileEditManager(t, dir)
	svc := diffreview.New(diffreview.Options{FileEdit: adapter})

	baseHash := diffreview.SHA256Hex([]byte("hello\n"))
	req := diffreview.FileEditWriteRequest{
		Path: "f.txt", Content: "new\n", BaseHash: baseHash,
		LineEnding: diffreview.LineEndingLF, BaseMode: "0444", // 无 owner 写位
	}
	_, err := svc.WriteFile(context.Background(), "t1", req)
	if err == nil {
		t.Fatal("expected error for read-only baseMode")
	}
	var fe *diffreview.FileEditErr
	if !errors.As(err, &fe) || fe.ReasonCode != diffreview.ReasonInvalidInput {
		t.Fatalf("err=%v want *FileEditErr{ReasonInvalidInput}", err)
	}
	// 零写盘。
	got, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if string(got) != "hello\n" {
		t.Fatalf("file modified: %q", got)
	}
}

func TestFileEdit_Write_ContentCR_RejectedByService(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	writeFileInRepo(t, dir, "f.txt", "hello\n", 0o644)
	_, adapter := newFileEditManager(t, dir)
	svc := diffreview.New(diffreview.Options{FileEdit: adapter})

	baseHash := diffreview.SHA256Hex([]byte("hello\n"))
	req := diffreview.FileEditWriteRequest{
		Path: "f.txt", Content: "he\rllo\n", BaseHash: baseHash,
		LineEnding: diffreview.LineEndingLF, BaseMode: "0644",
	}
	_, err := svc.WriteFile(context.Background(), "t1", req)
	if err == nil {
		t.Fatal("expected error for content with CR")
	}
	var fe *diffreview.FileEditErr
	if !errors.As(err, &fe) || fe.ReasonCode != diffreview.ReasonInvalidInput {
		t.Fatalf("err=%v want *FileEditErr{ReasonInvalidInput}", err)
	}
	// 零写盘。
	got, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if string(got) != "hello\n" {
		t.Fatalf("file modified: %q", got)
	}
}

func TestFileEdit_Write_RebuiltTooLarge_InvalidInput(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	// 原文件略小，但 rebuild 后超过 512KiB。
	content := strings.Repeat("a", diffreview.FileEditMaxBytes-10) + "\n"
	writeFileInRepo(t, dir, "f.txt", content, 0o644)
	_, adapter := newFileEditManager(t, dir)

	baseHash := diffreview.SHA256Hex([]byte(content))
	// 新 content 比原文件大，rebuild 后超过 512KiB。
	newContent := strings.Repeat("a", diffreview.FileEditMaxBytes) + "\n"
	req := diffreview.FileEditWriteRequest{
		Path: "f.txt", Content: newContent, BaseHash: baseHash,
		LineEnding: diffreview.LineEndingLF, BaseMode: "0644",
	}
	_, err := adapter.Write(context.Background(), "t1", req)
	if !isOpErrCode(err, codeInvalidInput) {
		t.Fatalf("rebuilt too large: err=%v want codeInvalidInput", err)
	}
	// 零写盘（临时文件清理）。
	got, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if string(got) != content {
		t.Fatalf("file modified: len %d want len %d", len(got), len(content))
	}
	// 无临时文件残留。
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ocdeck-fileedit-") {
			t.Fatalf("temp file leaked: %s", e.Name())
		}
	}
}

// --- 端到端：读取 → 写回 → 读取 ---

func TestFileEdit_ReadWriteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	content := "line1\nline2\n"
	writeFileInRepo(t, dir, "f.txt", content, 0o644)
	_, adapter := newFileEditManager(t, dir)
	svc := diffreview.New(diffreview.Options{FileEdit: adapter})

	// 读取。
	res, err := svc.ReadFile(context.Background(), "t1", "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Editable {
		t.Fatalf("read: %+v", res)
	}

	// 写回新内容。
	newContent := "line1\nline2\nline3\n"
	writeReq := diffreview.FileEditWriteRequest{
		Path: "f.txt", Content: newContent, BaseHash: res.BaseHash,
		LineEnding: res.LineEnding, BaseMode: res.Mode,
	}
	writeRes, err := svc.WriteFile(context.Background(), "t1", writeReq)
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	// 再次读取，验证新内容。
	res2, err := svc.ReadFile(context.Background(), "t1", "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if res2.Content != newContent {
		t.Fatalf("round-trip content: %q want %q", res2.Content, newContent)
	}
	if res2.BaseHash != writeRes.BaseHash {
		t.Fatalf("round-trip baseHash: %q want %q", res2.BaseHash, writeRes.BaseHash)
	}
}

// --- 编译期断言 ---

func TestFileEditPortAdapter_ImplementsInterface(t *testing.T) {
	var _ diffreview.FileEditPort = (*FileEditPortAdapter)(nil)
}