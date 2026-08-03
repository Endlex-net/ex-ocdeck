package api

import (
	"ocdeck/internal/config"
)

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
