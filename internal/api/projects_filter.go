// projects_filter.go projects 场景（全任务树投影）的消费过滤（projects-stream
// design D4 消费过滤表）。纯函数：仅判定事件是否将 projects 快照标脏，不做增量合并——
// 标脏后重组装全量快照（buildProjectsSnapshot）重推。与 eventDirtiesActiveSessions
// （sessions_filter.go，active-only）同层并列，两过滤表各自独立定义、互不覆盖。
package api

import (
	ocdeckevent "ocdeck/internal/domain/event"
)

// eventDirtiesProjectsTaskTree 按消费过滤表判定领域事件是否标脏 projects 场景
// （projects-stream design D4，逐行对齐）。
//
// 与指挥中心 active-only 过滤的差异：侧栏/项目管理页呈现全部非删除态任务（含挂起、
// 归档、过渡与失败态），任一任务的进入/迁移/离开/字段变化都改变该投影——
//   - task.created：树新增行
//   - task.status_changed：任意 from/to（挂起↔归档、creating→creation_failed、
//     →deleting 等非 active 过渡/失败迁移同样改树呈现，无需判 active 边界）
//   - task.deleted：任意 from（删除挂起/归档任务也改树）
//   - task.activity_changed / session.* / serve_runtime.* / resync.requested：
//     notice/last_error/updated_at/last_active_at/agentStatus/attention_count 呈现变化
//   - 未知 Type：保守标脏，避免漏推
//
// 全部已知 Type 与未知 Type 均标脏，故不解读 Payload（active-only 过滤所需的
// status 枚举校验在此无增量价值），标脏后一律重组装全量快照重推。
func eventDirtiesProjectsTaskTree(ev ocdeckevent.Event) bool {
	return true
}
