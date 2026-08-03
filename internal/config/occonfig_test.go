package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newOCConfigManager(t *testing.T) *OCConfigManager {
	t.Helper()
	dir := t.TempDir()
	return NewOCConfigManager(dir)
}

func TestOCConfig_List(t *testing.T) {
	m := newOCConfigManager(t)
	if err := os.WriteFile(filepath.Join(m.dir, "opencode.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.dir, "omo-slim.jsonc"), []byte(`{"a":1,}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.dir, "ignore.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	names, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, n := range names {
		got[n.Name] = true
	}
	if !got["opencode.json"] || !got["omo-slim.jsonc"] {
		t.Errorf("list missing configs: %+v", got)
	}
	if got["ignore.txt"] {
		t.Error("ignore.txt should be excluded")
	}
}

func TestOCConfig_Read(t *testing.T) {
	m := newOCConfigManager(t)
	if err := os.WriteFile(filepath.Join(m.dir, "opencode.json"), []byte(`{"k":"v"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := m.Read("opencode.json")
	if err != nil {
		t.Fatal(err)
	}
	if c.Content != `{"k":"v"}` {
		t.Errorf("content = %q", c.Content)
	}
	if c.Hash == "" || c.Mtime == 0 {
		t.Errorf("hash/mtime empty: hash=%q mtime=%d", c.Hash, c.Mtime)
	}
}

func TestOCConfig_Read_NotFound(t *testing.T) {
	m := newOCConfigManager(t)
	if _, err := m.Read("missing.json"); err != ErrConfigNotFound {
		t.Errorf("err = %v, want ErrConfigNotFound", err)
	}
}

func TestOCConfig_Read_SymlinkRejected(t *testing.T) {
	m := newOCConfigManager(t)
	target := t.TempDir()
	link := filepath.Join(m.dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := m.Read("link.json"); err == nil {
		t.Error("symlink read should fail")
	}
}

func TestOCConfig_Save_JSONSyntaxValid(t *testing.T) {
	m := newOCConfigManager(t)
	saved, err := m.Save("opencode.json", `{"a":1}`, 0, "")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if saved.Hash == "" || saved.Mtime == 0 {
		t.Error("saved hash/mtime empty")
	}
	data, _ := os.ReadFile(filepath.Join(m.dir, "opencode.json"))
	if string(data) != `{"a":1}` {
		t.Errorf("file content = %q", data)
	}
}

func TestOCConfig_Save_JSONSyntaxInvalid(t *testing.T) {
	m := newOCConfigManager(t)
	_, err := m.Save("opencode.json", `{invalid}`, 0, "")
	if err == nil || !strings.Contains(err.Error(), ErrInvalidSyntax.Error()) {
		t.Errorf("err = %v, want ErrInvalidSyntax", err)
	}
	// 文件不应被创建。
	if _, statErr := os.Stat(filepath.Join(m.dir, "opencode.json")); statErr == nil {
		t.Error("invalid file should not be written")
	}
}

func TestOCConfig_Save_JSONC_AllowsCommentsAndTrailingComma(t *testing.T) {
	m := newOCConfigManager(t)
	content := `{
  // a comment
  "a": 1,
  "b": 2,
}`
	_, err := m.Save("omo-slim.jsonc", content, 0, "")
	if err != nil {
		t.Fatalf("jsonc save should succeed: %v", err)
	}
	// 原文（含注释）应被保留。
	data, _ := os.ReadFile(filepath.Join(m.dir, "omo-slim.jsonc"))
	if !strings.Contains(string(data), "// a comment") {
		t.Errorf("jsonc comment should be preserved: %q", data)
	}
}

func TestOCConfig_Save_JSONC_SyntaxInvalidStillRejected(t *testing.T) {
	m := newOCConfigManager(t)
	// 注释剥离后仍非法（缺右括号）。
	_, err := m.Save("omo-slim.jsonc", `{"a":1 // c\n`, 0, "")
	if err == nil || !strings.Contains(err.Error(), ErrInvalidSyntax.Error()) {
		t.Errorf("err = %v, want ErrInvalidSyntax", err)
	}
}

func TestOCConfig_Save_OptimisticConcurrencyConflict(t *testing.T) {
	m := newOCConfigManager(t)
	// 首次保存。
	saved, err := m.Save("opencode.json", `{"a":1}`, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	// 外部修改文件（改 mtime + 内容）。
	if err := os.WriteFile(filepath.Join(m.dir, "opencode.json"), []byte(`{"a":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 用旧 mtime+hash 保存应冲突。
	_, err = m.Save("opencode.json", `{"a":3}`, saved.Mtime, saved.Hash)
	if err != ErrConfigConflict {
		t.Errorf("err = %v, want ErrConfigConflict", err)
	}
}

func TestOCConfig_Save_BakBackupCreated(t *testing.T) {
	m := newOCConfigManager(t)
	if _, err := m.Save("opencode.json", `{"a":1}`, 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Save("opencode.json", `{"a":2}`, mtimeOf(t, m, "opencode.json"), hashOfFile(t, m, "opencode.json")); err != nil {
		t.Fatal(err)
	}
	bak, err := os.ReadFile(filepath.Join(m.dir, "opencode.json.bak"))
	if err != nil {
		t.Fatalf("bak should exist: %v", err)
	}
	if string(bak) != `{"a":1}` {
		t.Errorf("bak content = %q, want {\"a\":1}", bak)
	}
}

func TestOCConfig_Save_AtomicWrite_PermPreserved(t *testing.T) {
	m := newOCConfigManager(t)
	// 首次保存权限 0644。
	path := filepath.Join(m.dir, "opencode.json")
	if _, err := m.Save("opencode.json", `{"a":1}`, 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	// 再次保存应保留 0644。
	if _, err := m.Save("opencode.json", `{"a":2}`, mtimeOf(t, m, "opencode.json"), hashOfFile(t, m, "opencode.json")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("perm = %o, want 0644", info.Mode().Perm())
	}
}

func TestOCConfig_Save_NameValidation_RejectsPathSeparator(t *testing.T) {
	m := newOCConfigManager(t)
	for _, name := range []string{"", "../x.json", "a/b.json", ".", ".."} {
		if _, err := m.Save(name, "{}", 0, ""); err == nil {
			t.Errorf("name %q should be rejected", name)
		}
	}
}

func TestOCConfig_Save_RejectsNonConfigExtension(t *testing.T) {
	m := newOCConfigManager(t)
	if _, err := m.Save("notes.txt", "x", 0, ""); err == nil {
		t.Error("non .json/.jsonc file should be rejected")
	}
}

func TestOCConfig_Save_SymlinkRejected(t *testing.T) {
	m := newOCConfigManager(t)
	target := t.TempDir()
	link := filepath.Join(m.dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := m.Save("link.json", `{}`, 0, ""); err == nil {
		t.Error("save to symlink should fail")
	}
}

// P1: .bak 若为 symlink MUST 拒绝跟随，防止覆盖任意同用户文件。
// 既存 symlink .bak 应被先 remove（不跟随 symlink）再经临时文件原子 rename 重建普通文件，
// 备份内容不被 symlink 目标污染，无 swap 窗口。新 .bak 为普通文件且权限继承主文件。
func TestOCConfig_Save_BakSymlinkRejected(t *testing.T) {
	m := newOCConfigManager(t)
	if _, err := m.Save("opencode.json", `{"a":1}`, 0, ""); err != nil {
		t.Fatal(err)
	}
	// 将主文件权限改为 0644，验证 .bak 重建后权限继承主文件当前权限。
	mainPath := filepath.Join(m.dir, "opencode.json")
	if err := os.Chmod(mainPath, 0o644); err != nil {
		t.Fatal(err)
	}
	// 在 .bak 位置放置 symlink 指向外部目录（模拟攻击）。
	evil := t.TempDir()
	bakLink := filepath.Join(m.dir, "opencode.json.bak")
	if err := os.Symlink(evil, bakLink); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	// 保存应成功（.bak symlink 被先删除再原子重建普通文件），不应写入 evil 目录。
	if _, err := m.Save("opencode.json", `{"a":2}`, mtimeOf(t, m, "opencode.json"), hashOfFile(t, m, "opencode.json")); err != nil {
		t.Fatalf("save with symlink .bak should succeed after removing symlink: %v", err)
	}
	// .bak 应为普通文件（非 symlink），内容为旧版本。
	info, err := os.Lstat(bakLink)
	if err != nil {
		t.Fatalf("bak should exist: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal(".bak is still a symlink after save")
	}
	data, _ := os.ReadFile(bakLink)
	if string(data) != `{"a":1}` {
		t.Errorf("bak content = %q, want {\"a\":1}", data)
	}
	// .bak 权限应继承主文件当前权限（0o644），与主文件写入策略一致。
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("bak perm = %o, want 0644 (inherited from main file)", got)
	}
	// evil 目录不应被写入任何文件。
	if ents, _ := os.ReadDir(evil); len(ents) != 0 {
		t.Errorf("evil dir should be empty, got %d entries", len(ents))
	}
}

// --- JSONC stripJSONC 状态机测试 ---

func TestStripJSONC_UnclosedBlockCommentRejected(t *testing.T) {
	// 未闭合块注释：stripJSONC 返回 ok=false → validateConfigSyntax 映射为 ErrInvalidSyntax。
	_, ok := stripJSONC(`{"a":1} /* unclosed`)
	if ok {
		t.Error("unclosed block comment should return ok=false")
	}
	if err := validateConfigSyntax("x.jsonc", `{"a":1} /* unclosed`); err == nil ||
		!strings.Contains(err.Error(), ErrInvalidSyntax.Error()) {
		t.Errorf("validateConfigSyntax unclosed block: err=%v want ErrInvalidSyntax", err)
	}
}

func TestStripJSONC_StringLiteralsPreserved(t *testing.T) {
	// 字符串内的 // 与 /* 必须原样保留，不被误剥离为注释。
	in := `{"a":"// not a comment","b":"/* also not */"}`
	out, ok := stripJSONC(in)
	if !ok {
		t.Fatal("stripJSONC returned not ok")
	}
	if !strings.Contains(out, "// not a comment") || !strings.Contains(out, "/* also not */") {
		t.Errorf("string literal content stripped: %q", out)
	}
	if err := jsonValid(out); err != nil {
		t.Errorf("stripped output not valid JSON: %v (out=%q)", err, out)
	}
}

func TestStripJSONC_TrailingCommaStripped(t *testing.T) {
	cases := []string{
		`{"a":1,}`,
		`{"a":1, "b":2,}`,
		`{"a":[1,2,3,]}`,
		"{\"a\":1,\n}", // 真实换行（逗号+空白+闭合括号）
	}
	for _, c := range cases {
		out, ok := stripJSONC(c)
		if !ok {
			t.Errorf("stripJSONC(%q) returned not ok", c)
			continue
		}
		if err := jsonValid(out); err != nil {
			t.Errorf("trailing comma case %q → invalid JSON %q: %v", c, out, err)
		}
	}
}

func TestStripJSONC_LinearTime(t *testing.T) {
	// 构造大文件（重复带注释与尾逗号的对象），验证单次扫描线性时间。
	// 用 2MB 输入，断言在合理时间内完成且结果合法。
	var sb strings.Builder
	sb.WriteString(`{`)
	for i := 0; i < 100000; i++ {
		sb.WriteString(`// comment` + "\n")
		sb.WriteString(fmt.Sprintf(`"k%d":"v%d",`, i, i))
	}
	sb.WriteString(`"end":1}`)
	const limit = 5 * time.Second
	start := time.Now()
	out, ok := stripJSONC(sb.String())
	elapsed := time.Since(start)
	if !ok {
		t.Fatal("stripJSONC large file returned not ok")
	}
	if elapsed > limit {
		t.Errorf("stripJSONC took %v, want < %v", elapsed, limit)
	}
	if err := jsonValid(out); err != nil {
		t.Errorf("large file stripped output invalid: %v", err)
	}
}

// jsonValid 轻量校验 JSON 合法性（复用 validateConfigSyntax 的 json 路径但无扩展名分流）。
func jsonValid(s string) error {
	var v any
	return json.Unmarshal([]byte(s), &v)
}

// --- helpers ---

func mtimeOf(t *testing.T, m *OCConfigManager, name string) int64 {
	t.Helper()
	info, err := os.Stat(filepath.Join(m.dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime().Unix()
}

func hashOfFile(t *testing.T, m *OCConfigManager, name string) string {
	t.Helper()
	c, err := m.Read(name)
	if err != nil {
		t.Fatal(err)
	}
	return c.Hash
}