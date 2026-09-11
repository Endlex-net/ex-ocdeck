package uploads

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appuploads "ocdeck/internal/application/uploads"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(t.TempDir())
}

// mustCommit 向 store 提交一个完整上传（经 WritePartial + CommitUpload 真实路径）。
func mustCommit(t *testing.T, s *Store, taskID, uploadID, connID, originalName, content string, uploadedAt time.Time) string {
	t.Helper()
	name := s.ManagedName(uploadID, originalName)
	if _, err := s.WritePartial(context.Background(), taskID, name, strings.NewReader(content), nil); err != nil {
		t.Fatalf("WritePartial: %v", err)
	}
	err := s.CommitUpload(taskID, appuploads.Meta{
		UploadID:   uploadID,
		TaskID:     taskID,
		ConnID:     connID,
		Filename:   name,
		UploadedAt: uploadedAt,
	})
	if err != nil {
		t.Fatalf("CommitUpload: %v", err)
	}
	return name
}

const (
	testUploadID = "0123456789abcdef0123456789abcdef"
	testConnID   = "11111111-2222-3333-4444-555555555555"
)

func TestManagedName_ExtensionRule(t *testing.T) {
	s := newTestStore(t)
	cases := []struct {
		original string
		want     string
	}{
		{"Report.PDF", testUploadID + ".pdf"},
		{"archive.tar.gz", testUploadID + ".gz"},
		{"noext", testUploadID},
		{".bashrc", testUploadID + ".bashrc"},
		{"a.verylongextension", testUploadID},
		{"bad ext!.png", testUploadID + ".png"},
	}
	for _, tc := range cases {
		if got := s.ManagedName(testUploadID, tc.original); got != tc.want {
			t.Errorf("ManagedName(%q) = %q, want %q", tc.original, got, tc.want)
		}
	}
}

func TestNewUploadID_Format(t *testing.T) {
	s := newTestStore(t)
	id, err := s.NewUploadID()
	if err != nil {
		t.Fatalf("NewUploadID: %v", err)
	}
	if !appuploads.IsUploadID(id) {
		t.Errorf("upload id = %q, want 32hex", id)
	}
}

func TestWritePartial_CommitUpload_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	name := mustCommit(t, s, "task1", testUploadID, testConnID, "notes.txt", "hello upload", time.Now())

	data, err := os.ReadFile(filepath.Join(s.Root(), "task1", name))
	if err != nil {
		t.Fatalf("read committed file: %v", err)
	}
	if string(data) != "hello upload" {
		t.Errorf("content = %q", string(data))
	}
	// sidecar 存在且 .partial 已消失。
	if _, err := os.Stat(filepath.Join(s.Root(), "task1", name+metaSuffix)); err != nil {
		t.Fatalf("sidecar missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "task1", name+partialSuffix)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("partial still present: %v", err)
	}
	// 权限：task 目录 0700、文件/sidecar 0600。
	if fi, err := os.Stat(filepath.Join(s.Root(), "task1")); err != nil {
		t.Fatalf("stat task dir: %v", err)
	} else if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("task dir perm = %o, want 700", perm)
	}
	if fi, err := os.Stat(filepath.Join(s.Root(), "task1", name)); err != nil {
		t.Fatalf("stat file: %v", err)
	} else if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("file perm = %o, want 600", perm)
	}
}

func TestCommitUpload_SidecarFailure_UndoFile(t *testing.T) {
	s := newTestStore(t)
	name := s.ManagedName(testUploadID, "a.txt")
	if _, err := s.WritePartial(context.Background(), "task1", name, strings.NewReader("x"), nil); err != nil {
		t.Fatalf("WritePartial: %v", err)
	}
	// 注入 sidecar 写失败：.meta.json.tmp 位置替换为目录 → WriteFile 失败 → EISDIR。
	tmpPath := filepath.Join(s.Root(), "task1", name+metaTmpSuffix)
	if err := os.MkdirAll(tmpPath, 0o700); err != nil {
		t.Fatalf("mkdir tmp dir: %v", err)
	}
	err := s.CommitUpload("task1", appuploads.Meta{
		UploadID: testUploadID, TaskID: "task1", ConnID: testConnID,
		Filename: name, UploadedAt: time.Now(),
	})
	if err == nil {
		t.Fatal("expected sidecar write failure")
	}
	// 已 rename 的最终文件被撤销；.partial 不再存在（已 rename）；无半提交状态。
	if _, statErr := os.Stat(filepath.Join(s.Root(), "task1", name)); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("committed file not undone: %v", statErr)
	}
}

func TestStatUpload(t *testing.T) {
	s := newTestStore(t)
	ok, err := s.StatUpload("task1", "missing")
	if err != nil || ok {
		t.Fatalf("StatUpload missing = (%v, %v), want (false, nil)", ok, err)
	}
	name := mustCommit(t, s, "task1", testUploadID, testConnID, "a.txt", "x", time.Now())
	ok, err = s.StatUpload("task1", name)
	if err != nil || !ok {
		t.Fatalf("StatUpload existing = (%v, %v), want (true, nil)", ok, err)
	}
}

func TestRefreshLastDelivered_UpdatesSidecar(t *testing.T) {
	s := newTestStore(t)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	name := mustCommit(t, s, "task1", testUploadID, testConnID, "a.txt", "x", at)

	at2 := at.Add(time.Hour)
	if err := s.RefreshLastDelivered("task1", appuploads.Meta{
		UploadID: testUploadID, TaskID: "task1", ConnID: testConnID,
		Filename: name, UploadedAt: at,
	}, at2); err != nil {
		t.Fatalf("RefreshLastDelivered: %v", err)
	}
	meta, err := s.readSidecar(filepath.Join(s.Root(), "task1", name+metaSuffix))
	if err != nil {
		t.Fatalf("readSidecar: %v", err)
	}
	if meta.LastDeliveredAt == nil || !meta.LastDeliveredAt.Equal(at2) {
		t.Errorf("lastDeliveredAt = %v, want %v", meta.LastDeliveredAt, at2)
	}
	if !meta.UploadedAt.Equal(at) {
		t.Errorf("uploadedAt = %v, want %v (not overwritten)", meta.UploadedAt, at)
	}
}

func TestSidecar_SchemaRoundTrip(t *testing.T) {
	s := newTestStore(t)
	at := time.Date(2026, 6, 1, 12, 0, 0, 123000000, time.UTC)
	name := mustCommit(t, s, "task1", testUploadID, testConnID, "a.txt", "x", at)
	raw, err := os.ReadFile(filepath.Join(s.Root(), "task1", name+metaSuffix))
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	// 六键 schema：全部键存在且键序固定；未投递 lastDeliveredAt 为 JSON null
	//（schema 唯一合法未投递形态）。
	want := `{"uploadId":"` + testUploadID + `","taskId":"task1","connId":"` + testConnID + `","filename":"` + name + `","uploadedAt":"` + at.Format(time.RFC3339Nano) + `","lastDeliveredAt":null}`
	if string(raw) != want {
		t.Errorf("sidecar = %s, want %s", raw, want)
	}
}

func TestReadSidecar_CorruptVariants(t *testing.T) {
	s := newTestStore(t)
	dir := filepath.Join(s.Root(), "task1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"missing uploadId": `{"taskId":"task1","connId":"c","filename":"f","uploadedAt":"2026-01-01T00:00:00Z","lastDeliveredAt":null}`,
		"bad time":         `{"uploadId":"` + testUploadID + `","taskId":"task1","connId":"c","filename":"f","uploadedAt":"yesterday","lastDeliveredAt":null}`,
		"empty connId":     `{"uploadId":"` + testUploadID + `","taskId":"task1","connId":"","filename":"f","uploadedAt":"2026-01-01T00:00:00Z","lastDeliveredAt":null}`,
		"wrong type":       `{"uploadId":42,"taskId":"task1","connId":"c","filename":"f","uploadedAt":"2026-01-01T00:00:00Z","lastDeliveredAt":null}`,
		// lastDeliveredAt：六键之一，缺失即损坏（delta spec:180）；字符串必须
		// RFC3339Nano；非字符串类型损坏。
		"missing lastDeliveredAt":    `{"uploadId":"` + testUploadID + `","taskId":"task1","connId":"c","filename":"f","uploadedAt":"2026-01-01T00:00:00Z"}`,
		"bad lastDeliveredAt":        `{"uploadId":"` + testUploadID + `","taskId":"task1","connId":"c","filename":"f","uploadedAt":"2026-01-01T00:00:00Z","lastDeliveredAt":"yesterday"}`,
		"wrong lastDeliveredAt type": `{"uploadId":"` + testUploadID + `","taskId":"task1","connId":"c","filename":"f","uploadedAt":"2026-01-01T00:00:00Z","lastDeliveredAt":42}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, "x.meta.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.readSidecar(path); !errors.Is(err, errSidecarCorrupt) {
				t.Errorf("readSidecar err = %v, want errSidecarCorrupt", err)
			}
		})
	}
}

func TestScan_Classification(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// 有效配对。
	validName := mustCommit(t, s, "task-valid", testUploadID, testConnID, "a.txt", "x", time.Now())
	// 无 sidecar 文件。
	if err := os.MkdirAll(filepath.Join(s.Root(), "task-mixed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root(), "task-mixed", "stray.bin"), []byte("z"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 损坏 sidecar + 配对文件（uploadId 与文件名前缀不一致）。
	if err := os.WriteFile(filepath.Join(s.Root(), "task-mixed", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.txt"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root(), "task-mixed", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.txt.meta.json"), []byte(`{"uploadId":"`+testUploadID+`","taskId":"task-mixed","connId":"c","filename":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.txt","uploadedAt":"2026-01-01T00:00:00Z","lastDeliveredAt":null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 单边 sidecar（对应文件缺失）。
	if err := os.WriteFile(filepath.Join(s.Root(), "task-mixed", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.png.meta.json"), []byte(`{"uploadId":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","taskId":"task-mixed","connId":"c","filename":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.png","uploadedAt":"2026-01-01T00:00:00Z","lastDeliveredAt":null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 遗留 .partial 与 .tmp。
	partialName := s.ManagedName("cccccccccccccccccccccccccccccccc", "p.bin")
	if _, err := s.WritePartial(ctx, "task-mixed", partialName, strings.NewReader("pp"), nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root(), "task-mixed", partialName+metaTmpSuffix), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, err := s.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	byKind := map[appuploads.EntryKind][]string{}
	for _, e := range entries {
		byKind[e.Kind] = append(byKind[e.Kind], e.TaskID+"/"+e.Filename)
	}
	if got := byKind[appuploads.EntryValidUpload]; len(got) != 1 || got[0] != "task-valid/"+validName {
		t.Errorf("valid entries = %v, want [task-valid/%s]", got, validName)
	}
	if got := byKind[appuploads.EntryOrphanFile]; len(got) != 1 || got[0] != "task-mixed/stray.bin" {
		t.Errorf("orphan file entries = %v, want [task-mixed/stray.bin]", got)
	}
	if got := byKind[appuploads.EntryOrphanCorrupt]; len(got) != 1 || got[0] != "task-mixed/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.txt" {
		t.Errorf("corrupt entries = %v, want paired file name", got)
	}
	if got := byKind[appuploads.EntryOrphanSidecar]; len(got) != 1 || !strings.HasSuffix(got[0], ".png.meta.json") {
		t.Errorf("orphan sidecar entries = %v", got)
	}
	if got := byKind[appuploads.EntryStalePartial]; len(got) != 1 || got[0] != "task-mixed/"+partialName {
		t.Errorf("stale partial entries = %v, want trimmed managed name", got)
	}
	if got := byKind[appuploads.EntryOrphanTmp]; len(got) != 1 || got[0] != "task-mixed/"+partialName+metaTmpSuffix {
		t.Errorf("orphan tmp entries = %v", got)
	}
}

func TestScan_SidecarReadFailure_NotOrphan(t *testing.T) {
	s := newTestStore(t)
	// 注入读取失败：sidecar 无读权限 → ReadFile EACCES（spec：不归孤儿、不删、下轮重试）。
	dir := filepath.Join(s.Root(), "task1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := testUploadID + ".txt"
	paired := filepath.Join(dir, name)
	sidecar := filepath.Join(dir, name+metaSuffix)
	if err := os.WriteFile(sidecar, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 真实配对文件：读取失败时 MUST 与 sidecar 一起保留（R4——未 claim 时会被
	// 第二遍归 OrphanFile 超龄删除）。
	if err := os.WriteFile(paired, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sidecar, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sidecar, 0o600) })
	entries, err := s.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(entries) != 1 || entries[0].Kind != appuploads.EntryReadFailed {
		t.Fatalf("entries = %+v, want single read_failed (paired file must not be orphaned)", entries)
	}
	// 完整清理链验证零删除：超龄时钟下若配对文件被误归孤儿必删。
	orch := appuploads.New(appuploads.Options{
		Store: s,
		Clock: func() time.Time { return time.Now().Add(2 * time.Hour) },
	})
	if err := orch.CleanupOnce(context.Background()); err != nil {
		t.Fatalf("CleanupOnce: %v", err)
	}
	for _, p := range []string{paired, sidecar} {
		if _, statErr := os.Stat(p); statErr != nil {
			t.Fatalf("read-failed entry must survive cleanup: %s: %v", p, statErr)
		}
	}
}

func TestScan_MissingRoot_Empty(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "not-created"))
	entries, err := s.Scan()
	if err != nil || len(entries) != 0 {
		t.Fatalf("Scan missing root = (%v, %v), want (empty, nil)", entries, err)
	}
}

func TestWritePartial_CtxCancel_CleansPartial(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	name := s.ManagedName(testUploadID, "big.bin")
	_, err := s.WritePartial(ctx, "task1", name, io.Reader(strings.NewReader(strings.Repeat("x", 100*1024))), nil)
	if err == nil {
		t.Fatal("expected ctx cancel error")
	}
	if _, statErr := os.Stat(filepath.Join(s.Root(), "task1", name+partialSuffix)); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("partial not cleaned after ctx cancel: %v", statErr)
	}
}

// TestScan_NullLastDeliveredAt_ValidUpload 验证合法 null sidecar（从未投递）
// 启动扫描恢复为可投递条目，不误判损坏（G1）。
func TestScan_NullLastDeliveredAt_ValidUpload(t *testing.T) {
	s := newTestStore(t)
	dir := filepath.Join(s.Root(), "task1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := testUploadID + ".txt"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	uploadedAt := "2026-01-01T00:00:00Z"
	sidecar := `{"uploadId":"` + testUploadID + `","taskId":"task1","connId":"` + testConnID + `","filename":"` + name + `","uploadedAt":"` + uploadedAt + `","lastDeliveredAt":null}`
	if err := os.WriteFile(filepath.Join(dir, name+metaSuffix), []byte(sidecar), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, err := s.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(entries) != 1 || entries[0].Kind != appuploads.EntryValidUpload {
		t.Fatalf("entries = %+v, want single valid upload (null lastDeliveredAt is legal)", entries)
	}
	meta := entries[0].Meta
	if meta == nil || meta.UploadID != testUploadID || meta.TaskID != "task1" ||
		meta.ConnID != testConnID || meta.Filename != name || meta.LastDeliveredAt != nil {
		t.Fatalf("meta = %+v, want null lastDeliveredAt preserved as nil", meta)
	}
}

// TestScan_MissingLastDeliveredAt_Corrupt 验证缺 lastDeliveredAt 键的 sidecar
// 按损坏分类（六键任一缺失=损坏），不进入可投递索引（G1 残留）。
func TestScan_MissingLastDeliveredAt_Corrupt(t *testing.T) {
	s := newTestStore(t)
	dir := filepath.Join(s.Root(), "task1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := testUploadID + ".txt"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sidecar := `{"uploadId":"` + testUploadID + `","taskId":"task1","connId":"` + testConnID + `","filename":"` + name + `","uploadedAt":"2026-01-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(dir, name+metaSuffix), []byte(sidecar), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, err := s.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(entries) != 1 || entries[0].Kind != appuploads.EntryOrphanCorrupt {
		t.Fatalf("entries = %+v, want single orphan_corrupt (missing key is corrupt)", entries)
	}
}

// TestScan_PairedStatFailure_ReadFailedThenRetry 验证配对文件 Stat 失败（非 NotExist，
// 自指 symlink → ELOOP）不归孤儿（G2）：分类为 ReadFailed、CleanupOnce 零删除、
// 修复后下一轮扫描恢复为有效配对。
func TestScan_PairedStatFailure_ReadFailedThenRetry(t *testing.T) {
	s := newTestStore(t)
	dir := filepath.Join(s.Root(), "task1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := testUploadID + ".txt"
	sidecarPath := filepath.Join(dir, name+metaSuffix)
	pairedPath := filepath.Join(dir, name)
	if err := os.WriteFile(sidecarPath, []byte(`{"uploadId":"`+testUploadID+`","taskId":"task1","connId":"`+testConnID+`","filename":"`+name+`","uploadedAt":"2026-01-01T00:00:00Z","lastDeliveredAt":null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 注入 Stat 失败：自指 symlink（ELOOP，非 NotExist）。
	if err := os.Symlink(name, pairedPath); err != nil {
		t.Fatal(err)
	}

	entries, err := s.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(entries) != 1 || entries[0].Kind != appuploads.EntryReadFailed {
		t.Fatalf("entries = %+v, want single read_failed (stat error must not be orphan)", entries)
	}

	// CleanupOnce 对 ReadFailed 条目零删除（sidecar 与配对文件都不动）。
	// Clock 注入条目 mtime +2h：即使被误归孤儿也已超 1h 龄期必删，断言才有效。
	orch := appuploads.New(appuploads.Options{
		Store: s,
		Clock: func() time.Time { return time.Now().Add(2 * time.Hour) },
	})
	if err := orch.CleanupOnce(context.Background()); err != nil {
		t.Fatalf("CleanupOnce: %v", err)
	}
	if _, statErr := os.Stat(sidecarPath); statErr != nil {
		t.Fatalf("sidecar must survive read-failed cleanup: %v", statErr)
	}
	if _, statErr := os.Lstat(pairedPath); statErr != nil {
		t.Fatalf("paired path must survive read-failed cleanup: %v", statErr)
	}

	// 修复注入（换真实文件）后下一轮扫描恢复为有效配对（重试语义）。
	if err := os.Remove(pairedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pairedPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err = s.Scan()
	if err != nil {
		t.Fatalf("re-scan: %v", err)
	}
	if len(entries) != 1 || entries[0].Kind != appuploads.EntryValidUpload {
		t.Fatalf("re-scan entries = %+v, want single valid upload (retry recovers)", entries)
	}
}
