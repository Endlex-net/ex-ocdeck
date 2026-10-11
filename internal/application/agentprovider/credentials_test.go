package agentprovider

// credentials_test.go 覆盖 D3 派生公式固定测试向量（期望值由独立实现
// python hmac/sha256/base64url 预计算，非自证）与 master secret 解析顺序
//（env 优先 > 0600 文件 > 缺失 fail-closed；显式空 env 不回退文件）。

import (
	"os"
	"path/filepath"
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
