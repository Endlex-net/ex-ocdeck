// Package auditlog 实现 ai-auto 判定审计日志（task-permission-mode D11）：
// 追加式单行 JSONL 旁路文件（<数据目录>/logs/ai-permission-audit.jsonl）。
//
// 机制（design.md D11 / tasks 7.1）：O_APPEND|O_CREATE|O_WRONLY 0600 打开
//（MkdirAll 父目录）+ 打开后 Chmod(0600) 收紧既有文件权限（OpenFile 创建权限参数
// 不修正既有文件，lifecycle/runner.go 先例；Chmod 失败关闭文件返回装配错误）；
// mutex 串行化，每条记录 marshal 为单行 JSON 后一次 Write 落盘（并发 goroutine 下
// 行不交错）；写入失败返回错误，由 task 消费者降级为普通日志（旁路，不影响判定/回复）。
// 单文件持续追加、不做滚动（量极小，清理由用户手动删除文件）。
//
// 本包不 import internal/task（SlugNamer 同族约束）：记录结构用本包 Entry，
// 组合根（main.go）适配 task.PermAuditRecord。
package auditlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Entry 单条审计记录（字段 JSON tag 与 spec「ai-auto 权限判定审计日志」逐字对应）。
type Entry struct {
	// Time RFC3339 时间戳（由调用方在判定终结时刻生成）。
	Time string `json:"time"`
	// TaskID / TaskName 任务标识与名称。
	TaskID   string `json:"task_id"`
	TaskName string `json:"task_name"`
	// RequestID 权限请求 ID；Permission 工具名；Patterns 请求的模式列表（与观察一致）。
	RequestID  string   `json:"request_id"`
	Permission string   `json:"permission"`
	Patterns   []string `json:"patterns"`
	// Verdict 判定结论：APPROVE|REJECT|UNCERTAIN|FAILED。
	Verdict string `json:"verdict"`
	// ReplyResult 回复结果：ok|gone|unknown|unsupported|not_applicable。
	ReplyResult string `json:"reply_result"`
	// Reply 实际发送的回复（once/reject）；仅 verdict 为 APPROVE/REJECT 且
	// reply_result 为 ok 时由调用方设置（条件字段）。
	Reply string `json:"reply,omitempty"`
	// Detail 判定依据摘要（task-permission-mode D11：≤1024B 含降级原因前缀与截断
	// 标记；由调用方从同一份 RequestDetail 统一生成，本层仅透传；无详情为空串）。
	Detail string `json:"detail"`
}

// Logger 追加式 JSONL 审计日志器（mutex 串行化）。
type Logger struct {
	mu sync.Mutex
	f  *os.File
}

// NewPermAuditLogger 打开（必要时创建）审计日志文件并收紧权限为 0600。
// 既有文件过宽（如 0644）时构造后即被收紧且内容保留；任一步失败关闭句柄并返回错误。
func NewPermAuditLogger(path string) (*Logger, error) {
	dir := filepath.Dir(path)
	// 目录 0700（可能含敏感判定信息，同 lifecycle runner 日志目录语义）。
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("auditlog: create dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("auditlog: chmod dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("auditlog: open: %w", err)
	}
	// OpenFile 的创建权限参数不修正既有文件：显式 Chmod 收紧（runner.go 先例）。
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("auditlog: chmod: %w", err)
	}
	return &Logger{f: f}, nil
}

// Record 追加一条审计记录：marshal 为单行 JSON 后一次 Write 落盘
//（并发 goroutine 下行不交错）。写入失败返回错误（含已关闭句柄等场景），
// 由调用方降级为普通日志。
func (l *Logger) Record(e Entry) error {
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("auditlog: marshal: %w", err)
	}
	line = append(line, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.f.Write(line); err != nil {
		return fmt.Errorf("auditlog: write: %w", err)
	}
	return nil
}

// Close 关闭底层文件（进程退出/测试收尾用）。
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
