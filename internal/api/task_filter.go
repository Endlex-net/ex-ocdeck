// task_filter.go 任务详情 SSE 场景的消费过滤（task-detail-stream D2 决策表）。
// 纯函数：仅判定事件是否将指定 taskID 的详情快照标脏，不做增量合并。
package api

import ocdeckevent "ocdeck/internal/domain/event"

// eventDirtiesTaskDetail 按 design D2 决策表判定领域事件是否标脏指定任务详情。
// 优先级自上而下：未知 Type / 已知 Type 但 Topic 或 Payload 类型不合法 → 脏；
// task.* 与 sessions.aligned 比 RID；session 单条与 serve_runtime 比 Payload TaskID；
// resync.requested 恒脏；其余合法且不关联事件不脏。
func eventDirtiesTaskDetail(taskID string) func(ocdeckevent.Event) bool {
	return func(ev ocdeckevent.Event) bool {
		switch ev.Type {
		case ocdeckevent.TypeTaskCreated, ocdeckevent.TypeTaskStatusChanged,
			ocdeckevent.TypeTaskDeleted, ocdeckevent.TypeTaskActivityChanged:
			if ev.Topic != ocdeckevent.TopicTask || !knownTaskPayload(ev) {
				return true
			}
			return ev.RID == taskID
		case ocdeckevent.TypeSessionsAligned:
			if ev.Topic != ocdeckevent.TopicSession || !knownTaskPayload(ev) {
				return true
			}
			return ev.RID == taskID
		case ocdeckevent.TypeSessionClaimed, ocdeckevent.TypeSessionTouched, ocdeckevent.TypeSessionDeleted:
			if ev.Topic != ocdeckevent.TopicSession {
				return true
			}
			p, ok := ev.Payload.(ocdeckevent.SessionOwnerPayload)
			if !ok {
				return true
			}
			return p.TaskID == taskID
		case ocdeckevent.TypeServeRuntimeAttentionChanged:
			if ev.Topic != ocdeckevent.TopicServeRuntime {
				return true
			}
			p, ok := ev.Payload.(ocdeckevent.ServeRuntimeTaskPayload)
			if !ok {
				return true
			}
			return p.TaskID == taskID
		case ocdeckevent.TypeServeRuntimeRunStatusChanged:
			if ev.Topic != ocdeckevent.TopicServeRuntime {
				return true
			}
			p, ok := ev.Payload.(ocdeckevent.ServeRuntimeRunStatusChangedPayload)
			if !ok {
				return true
			}
			return p.TaskID == taskID
		case ocdeckevent.TypeServeRuntimeSessionError:
			// task-notifications D2：一次性错误事实（通知触发输入），不改变任务详情
			// 投影——合法事件无论 TaskID 是否本任务均不标脏；Topic/payload 形状异常
			// 按本表惯例保守标脏。
			if ev.Topic != ocdeckevent.TopicServeRuntime {
				return true
			}
			if _, ok := ev.Payload.(ocdeckevent.ServeRuntimeSessionErrorPayload); !ok {
				return true
			}
			return false
		case ocdeckevent.TypeResyncRequested:
			if ev.Topic != ocdeckevent.TopicControl {
				return true
			}
			if _, ok := ev.Payload.(struct{}); !ok {
				return true
			}
			return true
		default:
			return true
		}
	}
}

func knownTaskPayload(ev ocdeckevent.Event) bool {
	switch ev.Type {
	case ocdeckevent.TypeTaskCreated, ocdeckevent.TypeTaskActivityChanged:
		_, ok := ev.Payload.(struct{})
		return ok
	case ocdeckevent.TypeTaskStatusChanged:
		_, ok := ev.Payload.(ocdeckevent.TaskStatusChangedPayload)
		return ok
	case ocdeckevent.TypeTaskDeleted:
		_, ok := ev.Payload.(ocdeckevent.TaskDeletedPayload)
		return ok
	case ocdeckevent.TypeSessionsAligned:
		_, ok := ev.Payload.(ocdeckevent.SessionsAlignedPayload)
		return ok
	default:
		return false
	}
}
