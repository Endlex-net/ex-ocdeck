package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// terminal-file-paste-drop 3.1：上传接口新增错误码的 HTTP 状态映射
// （design.md D1 错误表：Origin/connId 准入拒绝 → 403，超限 → 413）。
func TestHTTPStatusForUploadCodes(t *testing.T) {
	tests := []struct {
		code ErrorCode
		want int
	}{
		{CodeForbidden, http.StatusForbidden},
		{CodePayloadTooLarge, http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		if got := httpStatusFor(tt.code); got != tt.want {
			t.Fatalf("httpStatusFor(%s)=%d want %d", tt.code, got, tt.want)
		}
	}
}

// writeError 按新错误码写入统一信封与状态（403/413 必须经 writeError 直接生效，
// 不依赖 invalid_state/invalid_input 的 422 默认映射）。
func TestWriteErrorUploadCodes(t *testing.T) {
	tests := []struct {
		code ErrorCode
		want int
	}{
		{CodeForbidden, http.StatusForbidden},
		{CodePayloadTooLarge, http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		writeError(rec, tt.code, "test message")
		if rec.Code != tt.want {
			t.Fatalf("writeError(%s) status=%d want %d", tt.code, rec.Code, tt.want)
		}
		var body errorBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("writeError(%s) body %q: %v", tt.code, rec.Body.String(), err)
		}
		if body.Error.Code != tt.code {
			t.Fatalf("writeError(%s) envelope code=%q", tt.code, body.Error.Code)
		}
	}
}
