package task

import "context"

// UploadCoordination 是 terminal-file-paste-drop 2.5 的 per-task 协调锁窄端口
//（design D5）：任务离开 active 的提交（suspend / delete intent / retry delete intent /
// 删行 / 异常收敛 active→suspended）与上传提交、投递、清理对同一 task 的操作
// MUST 串行化。方法名与 application/uploads.Coordination.Acquire 对齐，
// *appuploads.Coordination 结构性满足本接口，组合根直接注入（task 包不 import
// application/uploads）。
//
// 锁顺序固定：Manager 任务锁 → 协调锁，禁止反向——投递链路持协调锁时不得获取
// 任务锁（application/uploads 不依赖 Manager，结构性保证）。
// nil 时（测试构造未注入）提交直接执行，行为与既有 legacy 测试路径一致
//（与 Lifecycle/Namer 等可选端口同型）。
type UploadCoordination interface {
	Acquire(ctx context.Context, taskID string) (release func(), err error)
}

// inUploadCoordination 在 taskID 的 per-task 协调锁临界区内执行 commit。
// 调用方 MUST 已持有该任务的 Manager 锁（锁顺序：任务锁 → 协调锁）。
// Acquire 失败（ctx 取消）返回错误，commit 不执行。
func (m *Manager) inUploadCoordination(ctx context.Context, taskID string, commit func() error) error {
	if m.uploadCoordination == nil {
		return commit()
	}
	release, err := m.uploadCoordination.Acquire(ctx, taskID)
	if err != nil {
		return err
	}
	defer release()
	return commit()
}
