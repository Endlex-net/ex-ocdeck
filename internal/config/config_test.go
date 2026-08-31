package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_RejectsMissingToken(t *testing.T) {
	dir := t.TempDir()
	opts := Options{
		EnvLookup:     emptyEnv(),
		OS:            OSInfo{GOOS: "darwin"},
		OpenCodeProbe: func() (string, error) { return ContractBaseline, nil },
		TmuxProbe:     func() (string, error) { return "tmux 3.4", nil },
	}
	_ = opts
	// 注入 data dir 避免污染 home。
	opts.EnvLookup = func(key string) (string, bool) {
		if key == "OCDECK_DATA_DIR" {
			return dir, true
		}
		return "", false
	}
	_, _, err := Load(opts)
	if err == nil {
		t.Fatal("expected error for missing OCDECK_TOKEN")
	}
}

func TestLoad_RejectsEmptyToken(t *testing.T) {
	dir := t.TempDir()
	_, _, err := Load(Options{
		EnvLookup: func(key string) (string, bool) {
			switch key {
			case "OCDECK_TOKEN":
				return "", true
			case "OCDECK_DATA_DIR":
				return dir, true
			}
			return "", false
		},
		OS:            OSInfo{GOOS: "darwin"},
		OpenCodeProbe: func() (string, error) { return ContractBaseline, nil },
		TmuxProbe:     func() (string, error) { return "tmux 3.4", nil },
	})
	if err == nil {
		t.Fatal("expected error for empty OCDECK_TOKEN")
	}
}

func TestLoad_DefaultPersist(t *testing.T) {
	dir := t.TempDir()
	cfg, release, err := Load(Options{
		EnvLookup: func(key string) (string, bool) {
			switch key {
			case "OCDECK_TOKEN":
				return "secret", true
			case "OCDECK_DATA_DIR":
				return dir, true
			}
			return "", false
		},
		OS:            OSInfo{GOOS: "darwin"},
		OpenCodeProbe: func() (string, error) { return ContractBaseline, nil },
		TmuxProbe:     func() (string, error) { return "tmux 3.4", nil },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer release()
	if cfg.ShutdownPolicy != ShutdownPersist {
		t.Errorf("policy = %s, want persist", cfg.ShutdownPolicy)
	}
	if cfg.ListenAddr != "127.0.0.1" {
		t.Errorf("listen = %s, want 127.0.0.1", cfg.ListenAddr)
	}
	if cfg.ServePortRange != DefaultPortRange {
		t.Errorf("port range = %+v, want default", cfg.ServePortRange)
	}
	if cfg.Token != "secret" {
		t.Errorf("token mismatch")
	}
}

func TestLoad_GOOSNonDarwinRejected(t *testing.T) {
	_, _, err := Load(Options{
		EnvLookup: func(key string) (string, bool) {
			if key == "OCDECK_TOKEN" {
				return "secret", true
			}
			return "", false
		},
		OS:            OSInfo{GOOS: "linux"},
		OpenCodeProbe: func() (string, error) { return ContractBaseline, nil },
		TmuxProbe:     func() (string, error) { return "tmux 3.4", nil },
	})
	if err == nil {
		t.Fatal("expected error for non-darwin GOOS")
	}
}

func TestLoad_InvalidShutdownPolicy(t *testing.T) {
	dir := t.TempDir()
	_, _, err := Load(Options{
		EnvLookup: func(key string) (string, bool) {
			switch key {
			case "OCDECK_TOKEN":
				return "secret", true
			case "OCDECK_DATA_DIR":
				return dir, true
			case "OCDECK_SHUTDOWN_POLICY":
				return "bogus", true
			}
			return "", false
		},
		OS:            OSInfo{GOOS: "darwin"},
		OpenCodeProbe: func() (string, error) { return ContractBaseline, nil },
		TmuxProbe:     func() (string, error) { return "tmux 3.4", nil },
	})
	if err == nil {
		t.Fatal("expected error for invalid shutdown policy")
	}
}

func TestLoad_OpenCodeMissingRejected(t *testing.T) {
	dir := t.TempDir()
	_, _, err := Load(Options{
		EnvLookup: func(key string) (string, bool) {
			switch key {
			case "OCDECK_TOKEN":
				return "secret", true
			case "OCDECK_DATA_DIR":
				return dir, true
			}
			return "", false
		},
		OS:            OSInfo{GOOS: "darwin"},
		OpenCodeProbe: func() (string, error) { return "", os.ErrNotExist },
		TmuxProbe:     func() (string, error) { return "tmux 3.4", nil },
	})
	if err == nil {
		t.Fatal("expected error for missing opencode binary")
	}
}

func TestLoad_TmuxTooOldRejected(t *testing.T) {
	dir := t.TempDir()
	_, _, err := Load(Options{
		EnvLookup: func(key string) (string, bool) {
			switch key {
			case "OCDECK_TOKEN":
				return "secret", true
			case "OCDECK_DATA_DIR":
				return dir, true
			}
			return "", false
		},
		OS:            OSInfo{GOOS: "darwin"},
		OpenCodeProbe: func() (string, error) { return ContractBaseline, nil },
		TmuxProbe:     func() (string, error) { return "tmux 3.1", nil },
	})
	if err == nil {
		t.Fatal("expected error for tmux < 3.2")
	}
}

func TestLoad_SingleInstanceLock(t *testing.T) {
	dir := t.TempDir()
	opts := Options{
		EnvLookup: func(key string) (string, bool) {
			switch key {
			case "OCDECK_TOKEN":
				return "secret", true
			case "OCDECK_DATA_DIR":
				return dir, true
			}
			return "", false
		},
		OS:            OSInfo{GOOS: "darwin"},
		OpenCodeProbe: func() (string, error) { return ContractBaseline, nil },
		TmuxProbe:     func() (string, error) { return "tmux 3.4", nil },
	}
	_, release, err := Load(opts)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	defer release()
	// 第二次加载同 dataDir 应因 flock 失败。
	_, _, err2 := Load(opts)
	if err2 == nil {
		t.Fatal("expected single-instance lock to block second load")
	}
}

func TestParseTmuxVersion(t *testing.T) {
	cases := []struct {
		in  string
		maj int
		min int
		ok  bool
	}{
		{"tmux 3.6a", 3, 6, true},
		{"tmux 3.2", 3, 2, true},
		{"tmux 4.0", 4, 0, true},
		{"bogus", 0, 0, false},
		{"tmux", 0, 0, false},
	}
	for _, c := range cases {
		maj, min, ok := parseTmuxSemver(c.in)
		if maj != c.maj || min != c.min || ok != c.ok {
			t.Errorf("parseTmuxSemver(%q) = %d,%d,%v; want %d,%d,%v", c.in, maj, min, ok, c.maj, c.min, c.ok)
		}
	}
}

func TestValidateTmuxVersion(t *testing.T) {
	if err := validateTmuxVersion("tmux 3.2"); err != nil {
		t.Errorf("3.2 should pass: %v", err)
	}
	if err := validateTmuxVersion("tmux 3.4"); err != nil {
		t.Errorf("3.4 should pass: %v", err)
	}
	if err := validateTmuxVersion("tmux 3.1"); err == nil {
		t.Error("3.1 should fail")
	}
	if err := validateTmuxVersion("garbage"); err == nil {
		t.Error("garbage should fail")
	}
}

func TestAcquireInstanceLock_SameDirFails(t *testing.T) {
	dir := t.TempDir()
	r1, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	defer r1()
	if _, err := acquireInstanceLock(dir); err == nil {
		t.Fatal("second lock on same dir should fail")
	}
}

// TestLoad_LockBeforeProbe 验证单实例 flock 在版本探测之前获取（B）：
// 锁被占用时，第二次 Load 应在探测前失败，探测函数 MUST NOT 被调用。
func TestLoad_LockBeforeProbe(t *testing.T) {
	dir := t.TempDir()
	// 先占用 flock。
	r1, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	defer r1()

	probeCalled := false
	_, _, err = Load(Options{
		EnvLookup: func(key string) (string, bool) {
			switch key {
			case "OCDECK_TOKEN":
				return "secret", true
			case "OCDECK_DATA_DIR":
				return dir, true
			}
			return "", false
		},
		OS: OSInfo{GOOS: "darwin"},
		OpenCodeProbe: func() (string, error) {
			probeCalled = true
			return ContractBaseline, nil
		},
		TmuxProbe: func() (string, error) { return "tmux 3.4", nil },
	})
	if err == nil {
		t.Fatal("expected Load to fail when flock is held by another instance")
	}
	if probeCalled {
		t.Error("OpenCodeProbe must not run when flock acquisition fails (lock-before-probe order)")
	}
}

func TestLoad_DataDirCreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "ocdeck")
	_, release, err := Load(Options{
		EnvLookup: func(key string) (string, bool) {
			switch key {
			case "OCDECK_TOKEN":
				return "secret", true
			case "OCDECK_DATA_DIR":
				return dir, true
			}
			return "", false
		},
		OS:            OSInfo{GOOS: "darwin"},
		OpenCodeProbe: func() (string, error) { return ContractBaseline, nil },
		TmuxProbe:     func() (string, error) { return "tmux 3.4", nil },
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer release()
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("data dir not created at %s", dir)
	}
}

func emptyEnv() EnvLookup {
	return func(string) (string, bool) { return "", false }
}

func TestVersionSupported(t *testing.T) {
	cases := []struct {
		detected string
		want     bool
	}{
		{"1.18.14", true},
		{"1.18.16", true},
		{"1.18.18", true},
		{"opencode 1.18.18", true},
		{"v1.18.14", true},
		{"1.18.13", false},
		{"1.18.19", true},
		{"1.18.25", true},
		{"1.18.26", false},
		{"1.19.0", false},
		{"test-1.0.0", false},
		{"1.18", false},
		{"1x.18.14", false},
		{"1.18x.14", false},
		{"1.18.14-rc.1", false},
		{"opencode 1.18.18 garbage", false},
		{"01.18.14", false},
	}
	for _, c := range cases {
		got := VersionSupported(c.detected)
		if got != c.want {
			t.Errorf("VersionSupported(%q) = %v, want %v", c.detected, got, c.want)
		}
	}
	if !VersionSupported(ContractMinVersion) {
		t.Errorf("VersionSupported(ContractMinVersion=%q) = false, want true", ContractMinVersion)
	}
	if !VersionSupported(ContractBaseline) {
		t.Errorf("VersionSupported(ContractBaseline=%q) = false, want true", ContractBaseline)
	}
}

func TestLoad_VersionVerifiedMismatch(t *testing.T) {
	dir := t.TempDir()
	cfg, release, err := Load(Options{
		EnvLookup: func(key string) (string, bool) {
			switch key {
			case "OCDECK_TOKEN":
				return "secret", true
			case "OCDECK_DATA_DIR":
				return dir, true
			}
			return "", false
		},
		OS:            OSInfo{GOOS: "darwin"},
		OpenCodeProbe: func() (string, error) { return "1.19.0", nil },
		TmuxProbe:     func() (string, error) { return "tmux 3.4", nil },
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer release()
	if cfg.VersionVerified {
		t.Error("VersionVerified = true, want false for mismatch")
	}
}

func TestLoad_DataDirAbsolutized(t *testing.T) {
	rel := "ocdeck-rel-dir"
	abs, err := filepath.Abs(filepath.Join(t.TempDir(), rel))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	cfg, release, err := Load(Options{
		EnvLookup: func(key string) (string, bool) {
			switch key {
			case "OCDECK_TOKEN":
				return "secret", true
			case "OCDECK_DATA_DIR":
				return abs, true
			}
			return "", false
		},
		OS:            OSInfo{GOOS: "darwin"},
		OpenCodeProbe: func() (string, error) { return ContractBaseline, nil },
		TmuxProbe:     func() (string, error) { return "tmux 3.4", nil },
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer release()
	if cfg.DataDir != abs {
		t.Errorf("DataDir = %q, want abs %q", cfg.DataDir, abs)
	}
}
