// Package uploads 编排受管上传的提交与投递（terminal-file-paste-drop 2.3）。
//
// 职责分层（design「职责分层与实施阶段」）：上传与投递的准入决策唯一归属本包；
// 流式落盘、rename、sidecar JSON 等文件系统细节由 internal/infrastructure/uploads
// 经 StorePort 实现（洋葱边界：本包只声明 consumer-owned 端口，不 import
// infrastructure）。per-task 协调锁串行化上传提交、投递、清理与任务离开 active 的
// 生命周期提交（design D5，锁顺序：Manager 任务锁 → 协调锁，禁止反向）。
//
// Lane B 契约（签名钉死）：
//   - type DeliverErrorCode string，六枚举 not_found/expired/forbidden/task_inactive/
//     write_failed/invalid_input；零值 "" 表示基础设施故障（任务查询失败/元数据读取
//     失败/文件 stat I/O 失败），非业务码，调用方按连接 1011 收尾。
//   - Deliver(ctx, taskID, connID, uploadID, inject) (ok, code)：在 per-task 协调锁
//     临界区内完成七步准入并调用 inject（inject 完整写入才返回 nil）。
//   - ContextWithInjectPath/InjectPathFrom：Deliver 步骤⑦将受管最终绝对路径
//     （<UploadDir>/<taskID>/<受管名>）包进 inject ctx，Lane B 注入函数经
//     InjectPathFrom 读取后写入 PTY。
package uploads

import (
	"context"
	"io"
	"time"

	ocdeckevent "ocdeck/internal/domain/event"
)

// Meta 受管上传元数据（应用层视图；infrastructure 落盘为六键 sidecar JSON，
// sidecar 为唯一持久真值）。UploadedAt/LastDeliveredAt 一律 UTC。
type Meta struct {
	UploadID        string     // 32hex，与最终文件名前缀一致
	TaskID          string     // 所属任务（== 存储目录名）
	ConnID          string     // 提交该上传的 TUI 连接
	Filename        string     // 最终落盘文件名（<32hex><可选 .ext>）
	UploadedAt      time.Time  // 提交完成时刻（UTC）
	LastDeliveredAt *time.Time // 最近一次成功投递时刻；nil=从未投递
}

// EntryKind 启动恢复/周期清理扫描的条目分类（design D4 分类表）。
type EntryKind string

const (
	// EntryValidUpload 有效配对（sidecar + 同名文件，全部不变量成立）→ 可投递索引。
	EntryValidUpload EntryKind = "valid_upload"
	// EntryOrphanFile 无 sidecar 的落盘文件 → 孤儿。
	EntryOrphanFile EntryKind = "orphan_file"
	// EntryOrphanSidecar 单边 sidecar（对应文件缺失）→ 孤儿。
	EntryOrphanSidecar EntryKind = "orphan_sidecar"
	// EntryOrphanCorrupt 损坏 sidecar（键缺失/格式非法/配对不变量破坏）及其对应文件 → 孤儿。
	EntryOrphanCorrupt EntryKind = "orphan_corrupt"
	// EntryStalePartial 遗留 .partial → 孤儿（Filename 为去掉 .partial 后的受管名）。
	EntryStalePartial EntryKind = "stale_partial"
	// EntryOrphanTmp 遗留 sidecar .tmp → 孤儿（Filename 为 .tmp 全名）。
	EntryOrphanTmp EntryKind = "orphan_tmp"
	// EntryReadFailed sidecar 读取失败（权限/I/O）：不归孤儿、不删、下轮重试。
	EntryReadFailed EntryKind = "read_failed"
)

// Entry 扫描条目。ModTime 为孤儿清理时间计算依据（文件 mtime 仅用于此，
// 不重建过期基准）；Meta 仅 EntryValidUpload 非空。
type Entry struct {
	Kind     EntryKind
	TaskID   string
	Filename string
	ModTime  time.Time
	Meta     *Meta
}

// StorePort 受管存储端口（internal/infrastructure/uploads 实现）。
// 命名与提交顺序（.partial → rename 最终名 → sidecar .tmp → rename .meta.json）
// 的语义约束见 design D4。
type StorePort interface {
	// NewUploadID 生成 32hex 上传 ID（crypto/rand 16B）。
	NewUploadID() (string, error)
	// ManagedName 计算落盘文件名：uploadID + 扩展名规则（原始名小写化匹配
	// ^\.[a-z0-9]{1,10}$，否则无扩展名）。客户端原始名不进入落盘路径。
	ManagedName(uploadID, originalName string) string
	// WritePartial 流式写 name 的 .partial（task 目录 0700、文件 0600）。
	// 每个非零字节块后调用 onProgress；失败时尽力清理残留并返回错误。
	WritePartial(ctx context.Context, taskID, name string, r io.Reader, onProgress func(n int64)) (int64, error)
	// CommitUpload 提交上传：.partial → 最终名 rename → sidecar .tmp → .meta.json。
	// sidecar 失败时尽力撤销已 rename 文件（撤销失败按孤儿处理）并返回错误。
	CommitUpload(taskID string, meta Meta) error
	// StatUpload 报告最终名文件是否存在；NotExist 归一化为 (false, nil)，
	// 其余 I/O 错误返回 err（基础设施故障，调用方不得伪造业务码）。
	StatUpload(taskID, filename string) (bool, error)
	// RefreshLastDelivered 以 meta + at 重写 sidecar 的 lastDeliveredAt
	//（.tmp → 原子 rename；失败保留磁盘既有 sidecar）。
	RefreshLastDelivered(taskID string, meta Meta, at time.Time) error
	// RemoveUpload 删除文件与其 sidecar（均容忍 NotExist）。
	RemoveUpload(taskID, filename string) error
	// RemovePartial 删除 name 的 .partial（容忍 NotExist）。
	RemovePartial(taskID, name string) error
	// RemovePath 删除 task 目录下精确相对路径（容忍 NotExist；孤儿清理通用）。
	RemovePath(taskID, filename string) error
	// RemoveTaskDir 整目录回收（task.deleted 事件触发；目录不存在视为成功）。
	RemoveTaskDir(taskID string) error
	// Scan 扫描根目录并按 D4 分类表分类（启动恢复与周期清理的唯一输入）。
	Scan() ([]Entry, error)
}

// TaskPort 任务存在/活跃查询端口（组合根注入 sqlite adapter 视图）。
// 仅 status==active 视为活跃（任务状态准入表共享：仅 active 通过）。
type TaskPort interface {
	TaskActive(ctx context.Context, taskID string) (exists, active bool, err error)
}

// ConnPort 当前 TUI 连接归属查询端口。组合根接线点：Lane B 接入前不注入（nil），
// 投递①与上传 connId 准入跳过判定。
type ConnPort interface {
	// CurrentTUIConn 返回任务当前 TUI 连接 ID；found=false 表示无当前 TUI 连接。
	CurrentTUIConn(ctx context.Context, taskID string) (connID string, found bool, err error)
}

// EventSubscription task 事件订阅窄接口（组合根包装 *eventbus.Sub，同
// application/notification ports 模式）。
type EventSubscription interface {
	C() <-chan ocdeckevent.Event
	Overflow() <-chan struct{}
	Close()
}

// EventSubscriber task 事件订阅端口（nil 时 task.deleted 回收不启用）。
type EventSubscriber interface {
	Subscribe(topic ocdeckevent.Topic) EventSubscription
}
