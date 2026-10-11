package opencode

// spi_connection_test.go 覆盖 tasks 3.4 连接管理：NewClient 构造的 client 复用
// 共享 loopback transport（跨实例按 host:port 连接池复用）、空闲回收有界、
// 代理显式禁用。派生凭据原语测试已随原语迁至 application/agentprovider
//（credentials_test.go，C2c grep 口径要求）。

import (
	"testing"
	"time"
)

func TestConnection_SharedLoopbackTransport(t *testing.T) {
	c1 := NewClient(50001, "pw", Options{})
	c2 := NewClient(50002, "pw", Options{})
	// 3.4：新 client 实例共享同一 transport（跨实例按 host:port 连接池复用）。
	if c1.httpClient.Transport != c2.httpClient.Transport {
		t.Fatal("新 client 实例必须复用共享 loopback transport")
	}
	if c1.sseClient.Transport != c2.httpClient.Transport {
		t.Fatal("SSE client 同样复用共享 transport")
	}
	// orchestration spec「serve HTTP 客户端连接管理」：空闲连接受
	// MaxIdleConns/IdleConnTimeout 上限约束并被按期回收；代理显式禁用。
	if loopbackTransport.Proxy != nil {
		t.Fatal("loopback transport MUST 显式禁用代理（Proxy=nil）")
	}
	if loopbackTransport.MaxIdleConns <= 0 {
		t.Errorf("MaxIdleConns = %d, MUST 有界（>0）", loopbackTransport.MaxIdleConns)
	}
	if loopbackTransport.IdleConnTimeout <= 0 || loopbackTransport.IdleConnTimeout > 5*time.Minute {
		t.Errorf("IdleConnTimeout = %v, MUST 有界回收", loopbackTransport.IdleConnTimeout)
	}
}
