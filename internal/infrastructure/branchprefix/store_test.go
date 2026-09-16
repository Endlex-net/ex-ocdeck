// store_test.go 覆盖分支前缀配置存储（task-info-editable 3.1）：缺省/保存/非法拒绝/
// 损坏降级/原子写/持久化。
package branchprefix

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadStore_Default(t *testing.T) {
	s := LoadStore(t.TempDir())
	if got := s.Prefix(); got != DefaultPrefix {
		t.Fatalf("Prefix = %q, want %q", got, DefaultPrefix)
	}
	if s.LoadError() != nil {
		t.Fatalf("LoadError = %v, want nil", s.LoadError())
	}
}

func TestPut_PersistsAndReloads(t *testing.T) {
	dataDir := t.TempDir()
	s := LoadStore(dataDir)
	if err := s.Put("team-x"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := s.Prefix(); got != "team-x" {
		t.Fatalf("Prefix after Put = %q, want team-x", got)
	}
	// 原子写盘：新 Store 实例从文件读回同值。
	reloaded := LoadStore(dataDir)
	if got := reloaded.Prefix(); got != "team-x" {
		t.Fatalf("reloaded Prefix = %q, want team-x", got)
	}
	if reloaded.LoadError() != nil {
		t.Fatalf("reloaded LoadError = %v, want nil", reloaded.LoadError())
	}
}

func TestPut_Invalid_RejectedNoWrite(t *testing.T) {
	dataDir := t.TempDir()
	s := LoadStore(dataDir)
	if err := s.Put("team-x"); err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	for _, bad := range []string{
		"", "ocdeck/", "Team", "team x", " team", "team ", "-team", "team-", "oc/deck",
		strings.Repeat("a", 51), "中文",
	} {
		if err := s.Put(bad); err == nil {
			t.Errorf("Put(%q): want error, got nil", bad)
		}
	}
	// 拒绝后内存值与文件保持原样。
	if got := s.Prefix(); got != "team-x" {
		t.Fatalf("Prefix after rejects = %q, want team-x (unchanged)", got)
	}
	b, err := os.ReadFile(ConfigPath(dataDir))
	if err != nil || !strings.Contains(string(b), "team-x") {
		t.Fatalf("config file changed after rejects: %q, %v", string(b), err)
	}
}

func TestValidate_Boundary(t *testing.T) {
	if err := Validate("a"); err != nil {
		t.Errorf("single char: %v", err)
	}
	if err := Validate(strings.Repeat("a", 50)); err != nil {
		t.Errorf("50 chars: %v", err)
	}
	if err := Validate("a-b9"); err != nil {
		t.Errorf("mixed: %v", err)
	}
}

func TestLoadStore_CorruptedDegradesDefault(t *testing.T) {
	cases := map[string]string{
		"invalid json": "{not json",
		"missing key":  `{"other":"x"}`,
		"null value":   `{"prefix":null}`,
		"wrong type":   `{"prefix":123}`,
		"invalid char": `{"prefix":"Team"}`,
		"slash":        `{"prefix":"oc/deck"}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dataDir := t.TempDir()
			if err := os.WriteFile(ConfigPath(dataDir), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			s := LoadStore(dataDir)
			if got := s.Prefix(); got != DefaultPrefix {
				t.Fatalf("Prefix = %q, want default %q (degraded)", got, DefaultPrefix)
			}
			if s.LoadError() == nil {
				t.Fatal("LoadError = nil, want observable load error")
			}
		})
	}
}

func TestLoadStore_UnreadableFileDegrades(t *testing.T) {
	dataDir := t.TempDir()
	// 目录占位使 ReadFile 报非 IsNotExist 错误。
	if err := os.MkdirAll(filepath.Join(dataDir, configFileName), 0o700); err != nil {
		t.Fatal(err)
	}
	s := LoadStore(dataDir)
	if got := s.Prefix(); got != DefaultPrefix {
		t.Fatalf("Prefix = %q, want default", got)
	}
	if s.LoadError() == nil {
		t.Fatal("LoadError = nil, want error")
	}
}

func TestDecodeConfig_Strict(t *testing.T) {
	p, err := DecodeConfig([]byte(`{"prefix":"team"}`))
	if err != nil || p != "team" {
		t.Fatalf("DecodeConfig = %q, %v; want team/nil", p, err)
	}
	if _, err := DecodeConfig([]byte(`{"prefix":"team","unknown":1}`)); err != nil {
		t.Fatalf("unknown key must be ignored: %v", err)
	}
}
