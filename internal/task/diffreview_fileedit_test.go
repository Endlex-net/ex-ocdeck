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
	"os"
	"path/filepath"
	"strings"
	"testing"

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