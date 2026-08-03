// Package api 实现 HTTP/WS 端点、token 中间件与统一错误结构（design.md §14/§21）。
package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// TokenAuthenticator 提供 token 校验能力，REST 与 WS 共用（design.md §14/§21）。
//
// 日志红线：token 值不得入日志；校验使用常量时间比较避免时序泄露。
type TokenAuthenticator struct {
	token  string
	digest [sha256.Size]byte
}

func NewTokenAuthenticator(token string) *TokenAuthenticator {
	return &TokenAuthenticator{token: token, digest: sha256.Sum256([]byte(token))}
}

// ValidateToken 校验裸 token 是否匹配（常量时间比较）。供 WS 首消息认证复用。
// 双方先 SHA-256 到固定 32 字节摘要再 ConstantTimeCompare，避免不同长度提前返回
// 泄露 token 长度（design.md §14 安全边界）。
func (a *TokenAuthenticator) ValidateToken(token string) bool {
	if a == nil || a.token == "" || token == "" {
		return false
	}
	digest := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(a.digest[:], digest[:]) == 1
}

// extractBearer 从 Authorization 头提取 Bearer token。
func extractBearer(header string) string {
	const prefix = "Bearer "
	if len(header) <= len(prefix) {
		return ""
	}
	if !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// AuthMiddleware REST Bearer 认证中间件（design.md §14/§21）。
//
// staticExempt 返回 true 的路径豁免认证（用于内嵌静态资源，本任务留接口占位）。
// 认证失败返回统一 JSON 401 并补 WWW-Authenticate: Bearer（design.md §21 / RFC 6750）。
func (a *TokenAuthenticator) AuthMiddleware(staticExempt func(path string) bool, next http.Handler) http.Handler {
	if staticExempt == nil {
		staticExempt = func(string) bool { return false }
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if staticExempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		token := extractBearer(r.Header.Get("Authorization"))
		if !a.ValidateToken(token) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, CodeUnauthorized, "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}
