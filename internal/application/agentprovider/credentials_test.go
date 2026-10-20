package agentprovider

// credentials_test.go 覆盖 D3 派生公式固定测试向量（期望值由独立实现
// python hmac/sha256/base64url 预计算，非自证）与 master secret 解析顺序
//（env 优先 > 0600 文件 > 缺失 fail-closed；显式空 env 不回退文件）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeriveRuntimePassword_FixedVectors(t *testing.T) {
	// msg = "ocdeck-agent-v1" || 0x00 || provider || 0x00 || task，
	// base64url_nopad(HMAC-SHA256(master, msg))[:32]。
	vectors := []struct {
		master, provider, task, want string
	}{
		{"s3cret-master-key", "opencode-v1", "task-123", "45qTSfLIzff_whdwI-GhshlU88Iyu94t"},
		{"another-master", "opencode-v1", "t-abc", "oLbSNyvKSpLZGTYD6xdZ2G_S5nJcnost"},
		{"m", "opencode-v1", "x", "Md3DW4LIFrZEvIHRF-yYaSaZ9GFY1ICi"},
	}
	for _, v := range vectors {
		if got := DeriveRuntimePassword(v.master, v.provider, v.task); got != v.want {
			t.Errorf("Derive(%q,%q,%q) = %q, want %q", v.master, v.provider, v.task, got, v.want)
		}
	}
	// 同任务恒定；provider/task 输入改变输出。
	a := DeriveRuntimePassword("m", "p", "t1")
	if a != DeriveRuntimePassword("m", "p", "t1") {
		t.Error("派生必须确定性（同任务恒定）")
	}
	if a == DeriveRuntimePassword("m", "p2", "t1") || a == DeriveRuntimePassword("m", "p", "t2") {
		t.Error("provider/task 必须参与派生")
	}
	if len(a) != 32 {
		t.Errorf("长度 = %d, want 32", len(a))
	}
}

func TestResolveMasterSecret_OrderAndFailClosed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.Unsetenv(MasterSecretEnv)
	t.Cleanup(func() { os.Unsetenv(MasterSecretEnv) })

	secretFile := filepath.Join(home, masterSecretFile)
	if err := os.MkdirAll(filepath.Dir(secretFile), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(secretFile, []byte("from-file\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	// 环境变量优先（文件同时存在）。
	t.Setenv(MasterSecretEnv, "from-env")
	got, err := ResolveMasterSecret()
	if err != nil || got != "from-env" {
		t.Fatalf("env 优先 = (%q, %v)", got, err)
	}
	// 环境变量显式空串 → 空值 fail-closed（D3：不回退文件）。
	t.Setenv(MasterSecretEnv, "")
	if _, err := ResolveMasterSecret(); err == nil {
		t.Fatal("空 env 必须失败（空值 fail-closed，不回退文件）")
	}
	// 未设置 env → 文件回退（TrimSpace 读取）。
	os.Unsetenv(MasterSecretEnv)
	got, err = ResolveMasterSecret()
	if err != nil || got != "from-file" {
		t.Fatalf("file fallback = (%q, %v)", got, err)
	}
	// 文件内容空白 → fail-closed。
	if err := os.WriteFile(secretFile, []byte("   \n"), 0o600); err != nil {
		t.Fatalf("write blank: %v", err)
	}
	if _, err := ResolveMasterSecret(); err == nil {
		t.Fatal("空白文件必须失败")
	}
	// 缺失 → fail-closed。
	if err := os.Remove(secretFile); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := ResolveMasterSecret(); err == nil {
		t.Fatal("缺失必须 fail-closed 返回错误")
	}
}

// TestEnsureMasterSecret_AutoGenerateOnMissing D3 修订：文件缺失（且 env 未设置）
// → 首启自动生成（随机 32 字节 hex、0600），再次调用读到同一值（同任务恒定的
// 前提）； ResolveMasterSecret 本体语义不变（仍 fail-closed——Ensure 是唯一
// 自动供给入口）。
func TestEnsureMasterSecret_AutoGenerateOnMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	os.Unsetenv(MasterSecretEnv)
	t.Cleanup(func() { os.Unsetenv(MasterSecretEnv) })

	got, err := EnsureMasterSecret()
	if err != nil {
		t.Fatalf("EnsureMasterSecret(缺失): %v", err)
	}
	if len(got) != 64 {
		t.Fatalf("len = %d, want 64（32 字节 hex）", len(got))
	}
	for _, c := range got {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			t.Fatalf("非 hex 字符 %q in %q", c, got)
		}
	}
	// 文件落盘形态：0600 权限 + 内容与返回值一致（TrimSpace 语义）。
	home, herr := os.UserHomeDir()
	if herr != nil {
		t.Fatalf("home: %v", herr)
	}
	raw, rerr := os.ReadFile(filepath.Join(home, masterSecretFile))
	if rerr != nil {
		t.Fatalf("read generated file: %v", rerr)
	}
	if strings.TrimSpace(string(raw)) != got {
		t.Fatalf("file content %q != returned %q", strings.TrimSpace(string(raw)), got)
	}
	fi, serr := os.Stat(filepath.Join(home, masterSecretFile))
	if serr != nil {
		t.Fatalf("stat: %v", serr)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %v, want 0600", fi.Mode().Perm())
	}
	// 再次调用：读到同一值（不重复生成）。
	again, err := EnsureMasterSecret()
	if err != nil || again != got {
		t.Fatalf("second call = (%q, %v), want same %q", again, err, got)
	}
	// ResolveMasterSecret 本体语义不变：文件缺失时仍 fail-closed（自动供给仅经 Ensure）。
	t.Setenv("HOME", t.TempDir())
	if _, err := ResolveMasterSecret(); err == nil {
		t.Fatal("ResolveMasterSecret 对缺失文件必须仍 fail-closed（语义不变）")
	}
}

// TestEnsureMasterSecret_EnvExplicitEmptyStillFails：env 显式空串属显式配置错误，
// 不自动生成（维持 fail-closed，D3 原语义）。
func TestEnsureMasterSecret_EnvExplicitEmptyStillFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(MasterSecretEnv, "   ")
	if _, err := EnsureMasterSecret(); err == nil {
		t.Fatal("env 显式空必须维持 fail-closed（不自动生成）")
	}
}

// TestEnsureMasterSecret_BlankFileStillFails：文件存在但空白属配置错误，
// 不自动生成（维持 fail-closed）。
func TestEnsureMasterSecret_BlankFileStillFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.Unsetenv(MasterSecretEnv)
	t.Cleanup(func() { os.Unsetenv(MasterSecretEnv) })
	secretFile := filepath.Join(home, masterSecretFile)
	if err := os.MkdirAll(filepath.Dir(secretFile), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(secretFile, []byte("  \n"), 0o600); err != nil {
		t.Fatalf("write blank: %v", err)
	}
	if _, err := EnsureMasterSecret(); err == nil {
		t.Fatal("空白文件必须维持 fail-closed（不自动生成）")
	}
}
