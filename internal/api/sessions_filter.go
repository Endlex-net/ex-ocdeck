// sessions_filter.go SSE 场景适配器的消费过滤（sse-active-sessions P2.1；design.md
// 消费过滤表）。纯函数：仅判定事件是否将 active sessions 场景标脏，不做增量合并——
// 标脏后按 D3 重推全量快照（buildActiveSessionsSnapshot）。
package api

import (
	ocdeckevent "ocdeck/internal/domain/event"

	"ocdeck/internal/application"
)

// knownTaskStatuses 生命周期 status 枚举闭集（design.md D0 九值），值直接取自
// application 包的 canonical 常量（不本地复制字面量）。
var knownTaskStatuses = map[string]struct{}{
	application.StatusSuspended:      {},
	application.StatusActive:         {},
	application.StatusArchived:       {},
	application.StatusCreating:       {},
	application.StatusCreationFailed: {},
	application.StatusActivating:     {},
	application.StatusSuspending:     {},
	application.StatusDeleting:       {},
	application.StatusDeletionFailed: {},
}

// eventDirtiesActiveSessions 按消费过滤表判定领域事件是否标脏 active sessions 场景
// （design.md 消费过滤表，逐行对齐）：
//   - 标脏：全部 session.*（claimed/touched/deleted）、sessions.aligned、
//     serve_runtime.attention_changed / run_status_changed、resync.requested、
//     task.activity_changed、未知 Type（保守标脏，避免漏推）
//   - task.status_changed：仅 (from==active) != (to==active) 标脏（进出 active 集合）；
//     两端都非 active 的迁移（suspend/archive/restore 等中间态）不标脏
//   - task.deleted：仅 from==active 标脏
//   - task.created：不标脏（只影响 projects 树，active 集合不变）
//
// 已知 Type 的 payload 无法按 typed payload 解读时保守标脏：bus 不做 schema 校验，
// 解读失败的 status/delete 若按零值判非 active 会漏推。类型断言通过但 From/To
// 不在 status 枚举闭集内（空串/未知值——语义畸形，typed 构造器不会产出，仅出现在
// 绕过构造器的事件里）同样保守标脏：按零值判非 active 会把畸形迁移漏成"无变化"。
func eventDirtiesActiveSessions(ev ocdeckevent.Event) bool {
	switch ev.Type {
	case ocdeckevent.TypeTaskCreated:
		return false
	case ocdeckevent.TypeTaskStatusChanged:
		p, ok := ev.Payload.(ocdeckevent.TaskStatusChangedPayload)
		if !ok {
			return true
		}
		if _, ok := knownTaskStatuses[p.From]; !ok {
			return true
		}
		if _, ok := knownTaskStatuses[p.To]; !ok {
			return true
		}
		return (p.From == application.StatusActive) != (p.To == application.StatusActive)
	case ocdeckevent.TypeTaskDeleted:
		p, ok := ev.Payload.(ocdeckevent.TaskDeletedPayload)
		if !ok {
			return true
		}
		if _, ok := knownTaskStatuses[p.From]; !ok {
			return true
		}
		return p.From == application.StatusActive
	default:
		// session.* / sessions.aligned / serve_runtime.* / resync.requested /
		// task.activity_changed / 未知 Type：全部标脏。
		return true
	}
}
