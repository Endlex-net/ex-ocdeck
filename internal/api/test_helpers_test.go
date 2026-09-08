package api

import (
	"os"
	"testing"

	"ocdeck/internal/config"
	"ocdeck/internal/infrastructure/hostenv"
)

// TestMain 将 login shell 兜底捕获替换为空结果：resolvedValue 等测试只依赖进程环境，
// 不受运行机 shell 配置干扰（hostenv 兜底行为由 hostenv 包内单测覆盖）。
func TestMain(m *testing.M) {
	hostenv.Capture = func() map[string]string { return nil }
	os.Exit(m.Run())
}

func testConfig() *config.Config {
	return &config.Config{
		Token:           "testtoken",
		ListenAddr:      "127.0.0.1",
		ShutdownPolicy:  config.ShutdownPersist,
		OpenCodeVersion: "test-1.0.0",
		VersionVerified: false, // test-1.0.0 != baseline，默认不匹配
		TmuxVersion:     "tmux 3.4",
	}
}
