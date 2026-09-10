// activity.go 用户主动操作上报（idle-reminder-user-activity D5）。
//
// 上报是发布型副作用：仅发布一次 task.user_activity 事件（D1），供 Notifier
// 取消该任务已武装的 idle 计时。MUST NOT 触碰任务行：不推进 updated_at、
// 不产生 task.activity_changed（事件见 active-sessions-stream「内部事件总线」
// 发布前提例外的 delta）。
package task

import (
	"context"

	ocdeckevent "ocdeck/internal/domain/event"
)

// RecordUserActivity 记录一次任务的用户主动操作：发布 task.user_activity
//（RID=taskID）。Publish 未注入（测试构造）时 no-op。
func (m *Manager) RecordUserActivity(_ context.Context, taskID string) {
	if m.publish == nil {
		return
	}
	m.publish.Publish(ocdeckevent.NewTaskUserActivity(taskID))
}

// RecordShellUserActivity 记录一次 shell 终端的用户主动操作：tid 为 shell 终端 ID
//（shell tmux 会话名），经 taskIDFromSessionName 解析归属任务；解析失败（非法 tid）
// 静默忽略零发布。
func (m *Manager) RecordShellUserActivity(_ context.Context, tid string) {
	taskID := taskIDFromSessionName(tid)
	if taskID == "" || m.publish == nil {
		return
	}
	m.publish.Publish(ocdeckevent.NewTaskUserActivity(taskID))
}
