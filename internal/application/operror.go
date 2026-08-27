// operror.go 定义跨边界错误语义（design.md §21 code 枚举）。
//
// sse-active-sessions P1.9a：自 internal/task 迁移至此（import 方向 api → application，
// design.md D0:55）；internal/task 保留别名与薄包装，既有引用零改动。错误码字面量、
// Error/Unwrap 行为与迁移前逐字一致（零 wire/行为变更）。
package application

import "errors"

// OpError 携带语义化错误码，供 api 层映射 HTTP code/msg（design.md §21）。
// 内部流转 err-first；仅在跨边界返回时附带 code。
type OpError struct {
	Code string // 对应 api.ErrorCode 枚举（conflict/not_found/invalid_state/...）
	Err  error
}

func (e *OpError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Code
}

func (e *OpError) Unwrap() error { return e.Err }

// CodeOf 返回错误码（供 api 边界映射）。
func (e *OpError) CodeOf() string { return e.Code }

// OpErrorCode 返回 OpError 的 code（若 err 为 *OpError，否则空串）。
func OpErrorCode(err error) string {
	var oe *OpError
	if errors.As(err, &oe) {
		return oe.Code
	}
	return ""
}

// NewOpErr 构造 OpError。
func NewOpErr(code string, err error) *OpError { return &OpError{Code: code, Err: err} }

// 错误码常量（与 api.ErrorCode 字面量一致，避免循环引用）。
const (
	CodeConflict       = "conflict"
	CodeNotFound       = "not_found"
	CodeInvalidState   = "invalid_state"
	CodeInvalidInput   = "invalid_input"
	CodeInternal       = "internal"
	CodeProcessError   = "process_error"
	CodeGitError       = "git_error"
	CodeOCIncompatible = "oc_incompatible"
	CodeRecovering     = "recovering"
)
