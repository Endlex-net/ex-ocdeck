// debug.go 承载 opencode（v1 client）[debug] 诊断日志（OCDECK_DEBUG 生产装配
// 注入；默认关）。传输层 debugTransport 只输出白名单字段：method/URI/status。
// 脱敏硬约束：响应 body 一律不落日志——服务器错误响应可能回显请求敏感
// 信息（Authorization/密码/env 值），截断≠脱敏；Basic Auth header、密码、
// 派生凭据同样永不落日志。
package opencode

import (
	"log"
	"net/http"
	"sync/atomic"
)

var debugLogEnabled atomic.Bool

// SetDebugLogging 开启/关闭本包 debug 日志（生产装配在启动期按 config 注入；
// MUST 先于任何 Client 构造生效——debugTransport 在 NewWireClient 装配）。
func SetDebugLogging(on bool) { debugLogEnabled.Store(on) }

func debugEnabled() bool { return debugLogEnabled.Load() }

func debugf(format string, args ...any) {
	if debugLogEnabled.Load() {
		log.Printf("[debug] opencode: "+format, args...)
	}
}

// debugTransport 包装既有 Transport：请求出站即记录 method/URI/status。
// 不读不改响应 body（错误分类消费完整 body 不受影响）。仅在 debug 开启时
// 由 NewWireClient 安装。
type debugTransport struct{ base http.RoundTripper }

func (d debugTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := d.base.RoundTrip(req)
	if err != nil {
		// 传输层错误（dial/超时）文本来自 net/http，不含响应 body；URL 不含
		// 凭据（Basic Auth 在 header）。可安全记录。
		debugf("http %s %s: transport error: %v", req.Method, req.URL.RequestURI(), err)
		return resp, err
	}
	debugf("http %s %s -> %d", req.Method, req.URL.RequestURI(), resp.StatusCode)
	return resp, nil
}
