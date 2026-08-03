// Package api 实现 HTTP/WS 端点、token 中间件与统一错误结构（design.md §14/§21）。
//
// 本文件仅提供统一错误结构与辅助函数；路由与中间件见 server.go / middleware.go。
package api

import (
	"encoding/json"
	"net/http"
)

// ErrorCode 错误码枚举（design.md §21）。
type ErrorCode string

const (
	CodeUnauthorized   ErrorCode = "unauthorized"
	CodeNotFound       ErrorCode = "not_found"
	CodeConflict       ErrorCode = "conflict"
	CodeInvalidState   ErrorCode = "invalid_state"
	CodeInvalidInput   ErrorCode = "invalid_input"
	CodeOCIncompatible ErrorCode = "oc_incompatible"
	CodeGitError       ErrorCode = "git_error"
	CodeProcessError   ErrorCode = "process_error"
	CodeInternal       ErrorCode = "internal"
)

// errorBody 统一错误响应体 `{"error":{"code","message"}}`（design.md §21）。
type errorBody struct {
	Error errorBodyDetail `json:"error"`
}

type errorBodyDetail struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

// httpStatusFor 按 code 映射 HTTP 状态码（design.md §21：401/404/409/422/500）。
func httpStatusFor(code ErrorCode) int {
	switch code {
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict:
		return http.StatusConflict
	case CodeInvalidState:
		return http.StatusUnprocessableEntity
	case CodeInvalidInput:
		return http.StatusUnprocessableEntity
	case CodeOCIncompatible:
		return http.StatusUnprocessableEntity
	case CodeGitError:
		return http.StatusUnprocessableEntity
	case CodeProcessError:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

// writeError 写入统一错误响应。message 不得包含 token/env 值（日志红线）。
func writeError(w http.ResponseWriter, code ErrorCode, message string) {
	writeJSONError(w, httpStatusFor(code), code, message)
}

// writeJSONError 以指定 HTTP 状态码写入统一错误响应（用于保留 405 等状态码时）。
func writeJSONError(w http.ResponseWriter, status int, code ErrorCode, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: errorBodyDetail{Code: code, Message: message}})
}

// Errorf 构造 ApiError 便于 handler 返回。
type ApiError struct {
	Code    ErrorCode
	Message string
}

func (e *ApiError) Error() string { return string(e.Code) + ": " + e.Message }

func NewError(code ErrorCode, message string) *ApiError {
	return &ApiError{Code: code, Message: message}
}

// writeApiError 将 *ApiError 写入响应。
func writeApiError(w http.ResponseWriter, err error) {
	if ae, ok := err.(*ApiError); ok {
		writeError(w, ae.Code, ae.Message)
		return
	}
	// 非 ApiError 视为内部错误，不泄露原始错误细节到响应体。
	writeError(w, CodeInternal, "internal server error")
}
