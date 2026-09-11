package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// uploadEnv 构造最小可启动 EnvLookup（token + dataDir + 覆盖项）。
func uploadEnv(dataDir string, overrides map[string]string) EnvLookup {
	return func(key string) (string, bool) {
		if v, ok := overrides[key]; ok {
			return v, true
		}
		switch key {
		case "OCDECK_TOKEN":
			return "secret", true
		case "OCDECK_DATA_DIR":
			return dataDir, true
		}
		return "", false
	}
}

func uploadLoadOptions(dataDir string, overrides map[string]string) Options {
	return Options{
		EnvLookup:     uploadEnv(dataDir, overrides),
		OS:            OSInfo{GOOS: "darwin"},
		OpenCodeProbe: func() (string, error) { return ContractBaseline, nil },
		TmuxProbe:     func() (string, error) { return "tmux 3.4", nil },
	}
}

func TestLoad_UploadDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg, release, err := Load(uploadLoadOptions(dir, nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer release()
	if cfg.UploadDir != filepath.Join(dir, "uploads") {
		t.Errorf("UploadDir = %s, want %s", cfg.UploadDir, filepath.Join(dir, "uploads"))
	}
	if cfg.UploadMaxBytes != DefaultUploadMaxBytes {
		t.Errorf("UploadMaxBytes = %d, want %d", cfg.UploadMaxBytes, DefaultUploadMaxBytes)
	}
	if cfg.UploadRetention != 0 {
		t.Errorf("UploadRetention = %s, want 0 (TTL off)", cfg.UploadRetention)
	}
}

func TestLoad_UploadDirRelativeAbsolutized(t *testing.T) {
	dir := t.TempDir()
	cfg, release, err := Load(uploadLoadOptions(dir, map[string]string{
		"OCDECK_UPLOAD_DIR": "uploads-rel/sub",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer release()
	// 相对路径按进程 CWD 绝对化（与 OCDECK_DATA_DIR 同型语义），不随运行期 CWD 漂移。
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if want := filepath.Clean(filepath.Join(wd, "uploads-rel/sub")); cfg.UploadDir != want {
		t.Errorf("UploadDir = %s, want %s", cfg.UploadDir, want)
	}
}

func TestLoad_UploadMaxBytesRange(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		value   string
		want    int64
		wantErr bool
	}{
		{"min bound", "1048576", 1 << 20, false},
		{"max bound", "104857600", 100 << 20, false},
		{"below min", "1048575", 0, true},
		{"above max", "104857601", 0, true},
		{"not a number", "abc", 0, true},
		{"zero", "0", 0, true},
		{"negative", "-1", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, release, err := Load(uploadLoadOptions(dir, map[string]string{
				"OCDECK_UPLOAD_MAX_BYTES": tc.value,
			}))
			if tc.wantErr {
				if err == nil {
					release()
					t.Fatalf("expected error for OCDECK_UPLOAD_MAX_BYTES=%q", tc.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			defer release()
			if cfg.UploadMaxBytes != tc.want {
				t.Errorf("UploadMaxBytes = %d, want %d", cfg.UploadMaxBytes, tc.want)
			}
		})
	}
}

func TestLoad_UploadRetention(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{"enabled", "24h", 24 * time.Hour, false},
		{"zero disables", "0", 0, false},
		{"negative rejected", "-5m", 0, true},
		{"invalid", "soon", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, release, err := Load(uploadLoadOptions(dir, map[string]string{
				"OCDECK_UPLOAD_RETENTION": tc.value,
			}))
			if tc.wantErr {
				if err == nil {
					release()
					t.Fatalf("expected error for OCDECK_UPLOAD_RETENTION=%q", tc.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			defer release()
			if cfg.UploadRetention != tc.want {
				t.Errorf("UploadRetention = %s, want %s", cfg.UploadRetention, tc.want)
			}
		})
	}
}

func TestLoad_UploadDirRejectedRunes(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name  string
		value string
	}{
		{"backslash", `srv/uploads` + "\\"},
		{"control char", "uploads\x01"},
		{"del char", "uploads\x7f"},
		{"newline", "uploads\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Load(uploadLoadOptions(dir, map[string]string{
				"OCDECK_UPLOAD_DIR": tc.value,
			}))
			if err == nil {
				t.Fatalf("expected error for OCDECK_UPLOAD_DIR=%q", tc.value)
			}
			if !strings.Contains(err.Error(), "OCDECK_UPLOAD_DIR") {
				t.Errorf("error %v should mention OCDECK_UPLOAD_DIR", err)
			}
		})
	}
}
