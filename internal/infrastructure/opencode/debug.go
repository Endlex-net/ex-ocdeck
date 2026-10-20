// debug.go 承载 opencode（v1 client）[debug] 诊断日志（OCDECK_DEBUG 生产装配
// 注入；默认关）。传输层 debugTransport 统一输出每次请求的 method/path/status
// 与错误响应 body 前 200 字符。脱敏硬约束：Basic Auth（Authorization header）、
// 密码、派生凭据永不落日志——只输出 method/URI/status 与错误 body。
package opencode

import (
	"bytes"
	"io"
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

// debugBodyCap 错误响应 body 落日志的上限（诊断）。
const debugBodyCap = 200

// debugTransport 包装既有 Transport：请求出站即记录 method/URI/status；
// 非 2xx 响应 body tee 缓存前 200 字节、Close 时输出（错误分类消费完整 body
// 不受影响）。仅在 debug 开启时由 NewWireClient 安装。
type debugTransport struct{ base http.RoundTripper }

func (d debugTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := d.base.RoundTrip(req)
	if err != nil {
		debugf("http %s %s: transport error: %v", req.Method, req.URL.RequestURI(), err)
		return resp, err
	}
	debugf("http %s %s -> %d", req.Method, req.URL.RequestURI(), resp.StatusCode)
	if resp.StatusCode >= 400 {
		resp.Body = &debugErrorBody{ReadCloser: resp.Body}
	}
	return resp, nil
}

// debugErrorBody tee 错误响应 body：读取时截留前 debugBodyCap 字节，Close 时
// 一次性输出（截断以 … 标记）。
type debugErrorBody struct {
	io.ReadCloser
	buf  bytes.Buffer
	full bool
}

func (b *debugErrorBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 && b.buf.Len() < debugBodyCap {
		keep := debugBodyCap - b.buf.Len()
		if keep > n {
			keep = n
		}
		b.buf.Write(p[:keep])
		if keep < n {
			b.full = true
		}
	}
	return n, err
}

func (b *debugErrorBody) Close() error {
	err := b.ReadCloser.Close()
	if b.buf.Len() > 0 {
		s := b.buf.String()
		if b.full {
			s += "…"
		}
		debugf("http error body: %s", s)
	}
	return err
}
