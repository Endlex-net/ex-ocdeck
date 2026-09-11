package uploads

import "errors"

// 准入/取消语义 sentinel（application err-first：不引入 Code/Msg 字段；
// HTTP 状态映射归 API 层，408 message "upload stalled for 1 hour" 由 Lane B 负责）。
var (
	// ErrUploadTaskNotFound 任务不存在（上传 finalize 复查 404）。
	ErrUploadTaskNotFound = errors.New("uploads: task not found")
	// ErrUploadTaskInactive 任务非 active（挂起/删除中/已归档等）。
	ErrUploadTaskInactive = errors.New("uploads: task not active")
	// ErrConnNotCurrent connId 非任务当前 TUI 连接。
	ErrConnNotCurrent = errors.New("uploads: connection is not the current TUI connection")
	// ErrConnAlreadyBound 上传的 connId 已绑定且与本次绑定值不同（BindConnID 仅允许从空值设置一次）。
	ErrConnAlreadyBound = errors.New("uploads: upload connection already bound")
	// ErrUploadStalled 上传停滞超时已被取消收尾；finalize 不得再发布 uploadId。
	ErrUploadStalled = errors.New("uploads: upload stalled")
	// ErrUploadNotBound 尚未绑定原始文件名（未过首 part 解析）即尝试写请求体。
	ErrUploadNotBound = errors.New("uploads: upload filename not bound")
	// ErrUploadTooLarge 上传超过 OCDECK_UPLOAD_MAX_BYTES（读满 M+1 字节判定）。
	ErrUploadTooLarge = errors.New("uploads: upload exceeds size limit")
)
