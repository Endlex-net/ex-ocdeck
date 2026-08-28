// Package event 定义领域事件契约与 typed payload 构造器。
//
// 本包只依赖 stdlib。它提供：
//   - Event{Topic, Type, RID, Payload} 信封类型
//   - 闭合的 Topic 常量（task / session / serve_runtime / control）
//   - 闭合的 Type 常量（事件类型目录，逐字对齐 design D0 事件类型目录；
//     task-notifications D2 显式增量：serve_runtime.session_error）
//   - 各 Type 对应的 typed payload 结构与构造器
//
// Bus（基础设施）不做 schema 校验；生产方使用本包的 typed 构造器组装 Event，
// 消费方按 Type 字面量匹配。Payload 仅承载小载荷，不含整表/整实体快照；
// RID 为主体实体自己的主键，关联实体 ID 转入 Payload。
package event

// Topic 为领域 topic，闭合枚举。
type Topic string

const (
	// TopicTask task 聚合相关事件。
	TopicTask = "task"
	// TopicSession session 聚合相关事件（含 sessions.aligned，主体为任务的会话集合）。
	TopicSession = "session"
	// TopicServeRuntime ServeRuntime 组件相关事件（attention/run_status/session_error）。
	TopicServeRuntime = "serve_runtime"
	// TopicControl 控制事件（resync.requested）。
	TopicControl = "control"
)

// Type 为具名领域事件类型，闭合枚举（逐字对齐 design D0 事件类型目录）。
type Type string

const (
	// TypeTaskCreated 任务行已创建（creating 状态入库）。RID=task 主键，Payload={}。
	TypeTaskCreated = "task.created"
	// TypeTaskStatusChanged 任务状态机发生真实迁移。RID=task 主键，Payload={from,to}。
	TypeTaskStatusChanged = "task.status_changed"
	// TypeTaskDeleted 任务行已删除。RID=task 主键，Payload={from}。
	TypeTaskDeleted = "task.deleted"
	// TypeTaskActivityChanged 未伴随 status 迁移的任务行非 status 真实变更（Changed=true）。
	// 不再要求 updated_at 跨秒推进。RID=task 主键，Payload={}。
	TypeTaskActivityChanged = "task.activity_changed"
	// TypeSessionClaimed 任务认领了 opencode 会话归属（或归属信息被推进）。
	// RID=session 主键，Payload={task_id}。
	TypeSessionClaimed = "session.claimed"
	// TypeSessionTouched owned 会话最近活跃时间被推进。RID=session 主键，Payload={task_id}。
	TypeSessionTouched = "session.touched"
	// TypeSessionDeleted 一条 owned 会话归属被移除。RID=session 主键，Payload={task_id}。
	TypeSessionDeleted = "session.deleted"
	// TypeSessionsAligned 全量对账使会话归属行或活动水位发生真实变化。
	// RID=task 主键（主体为任务的会话集合，持久侧无独立对象），
	// Payload={inserted, touched, deleted, session_ids}。
	TypeSessionsAligned = "sessions.aligned"
	// TypeServeRuntimeAttentionChanged 外部可见注意力快照发生变化。
	// RID=ServeRuntime 主键 instVersion，Payload={task_id}。
	TypeServeRuntimeAttentionChanged = "serve_runtime.attention_changed"
	// TypeServeRuntimeRunStatusChanged ServeRuntime 运行状态或可用性发生变化。
	// RID=ServeRuntime 主键 instVersion，Payload={task_id, from, to, available}。
	TypeServeRuntimeRunStatusChanged = "serve_runtime.run_status_changed"
	// TypeServeRuntimeSessionError 观察 opencode session.error 事件（task-notifications
	// design D2：对 D0 封闭事件目录的显式增量）。一次性错误事实（非状态投影）。
	// RID=ServeRuntime 主键 instVersion，
	// Payload={task_id, session_id, name, message, status_code *int, is_retryable *bool}。
	TypeServeRuntimeSessionError = "serve_runtime.session_error"
	// TypeResyncRequested 控制事件：要求订阅方重拉其场景全量。RID 允许空，Payload={}。
	TypeResyncRequested = "resync.requested"
)

// Event 领域事件信封。
//
// 字段语义：
//   - Topic 领域 topic（见 Topic 常量）
//   - Type  具名事件类型（见 Type 常量）
//   - RID   主体实体自己的主键 ID；resync.requested 无主体允许空
//   - Payload 小载荷；未知 Type 仍投递，不得使 Publish 失败
type Event struct {
	Topic   Topic
	Type    Type
	RID     string
	Payload any
}

// --- typed payloads ---

// TaskStatusChangedPayload 对应 TypeTaskStatusChanged 的 payload。
// From/To 均为 tasks.status 枚举值字符串（creating/creation_failed/activating/
// active/suspending/suspended/archived/deleting/deletion_failed）。
type TaskStatusChangedPayload struct {
	From string
	To   string
}

// TaskDeletedPayload 对应 TypeTaskDeleted 的 payload。
// From 为被删行原状态。
type TaskDeletedPayload struct {
	From string
}

// SessionOwnerPayload 对应 session 单条事件（claimed/touched/deleted）的 payload。
// TaskID 为 owning task 主键。
type SessionOwnerPayload struct {
	TaskID string
}

// SessionsAlignedPayload 对应 TypeSessionsAligned 的 payload。
// SessionIDs 为受影响的 session ID 集合（由对账事务统计得出）。
type SessionsAlignedPayload struct {
	Inserted   int
	Touched    int
	Deleted    int
	SessionIDs []string
}

// ServeRuntimeTaskPayload 对应 TypeServeRuntimeAttentionChanged 的 payload。
// TaskID 为 owning task 主键。
type ServeRuntimeTaskPayload struct {
	TaskID string
}

// ServeRuntimeRunStatusChangedPayload 对应 TypeServeRuntimeRunStatusChanged 的 payload。
// From/To 为聚合三态聚合值（idle/busy/retry）或 "" 表不可用；Available 为变化后外部是否可用。
type ServeRuntimeRunStatusChangedPayload struct {
	TaskID    string
	From      string
	To        string
	Available bool
}

// ServeRuntimeSessionErrorPayload 对应 TypeServeRuntimeSessionError 的 payload
//（task-notifications D2）。Name/Message 保留原文（TrimSpace 仅用于解析有效性判定）；
// StatusCode/IsRetryable 可空（解析降级语义见 spec「通知触发——错误未恢复」）。
type ServeRuntimeSessionErrorPayload struct {
	TaskID      string
	SessionID   string
	Name        string
	Message     string
	StatusCode  *int
	IsRetryable *bool
}

// --- typed constructors ---

// NewTaskCreated 构造 task.created 事件。
func NewTaskCreated(taskID string) Event {
	return Event{Topic: TopicTask, Type: TypeTaskCreated, RID: taskID, Payload: struct{}{}}
}

// NewTaskStatusChanged 构造 task.status_changed 事件。
func NewTaskStatusChanged(taskID, from, to string) Event {
	return Event{
		Topic:   TopicTask,
		Type:    TypeTaskStatusChanged,
		RID:     taskID,
		Payload: TaskStatusChangedPayload{From: from, To: to},
	}
}

// NewTaskDeleted 构造 task.deleted 事件。
func NewTaskDeleted(taskID, from string) Event {
	return Event{
		Topic:   TopicTask,
		Type:    TypeTaskDeleted,
		RID:     taskID,
		Payload: TaskDeletedPayload{From: from},
	}
}

// NewTaskActivityChanged 构造 task.activity_changed 事件。
func NewTaskActivityChanged(taskID string) Event {
	return Event{Topic: TopicTask, Type: TypeTaskActivityChanged, RID: taskID, Payload: struct{}{}}
}

// NewSessionClaimed 构造 session.claimed 事件。
func NewSessionClaimed(sessionID, taskID string) Event {
	return Event{
		Topic:   TopicSession,
		Type:    TypeSessionClaimed,
		RID:     sessionID,
		Payload: SessionOwnerPayload{TaskID: taskID},
	}
}

// NewSessionTouched 构造 session.touched 事件。
func NewSessionTouched(sessionID, taskID string) Event {
	return Event{
		Topic:   TopicSession,
		Type:    TypeSessionTouched,
		RID:     sessionID,
		Payload: SessionOwnerPayload{TaskID: taskID},
	}
}

// NewSessionDeleted 构造 session.deleted 事件。
func NewSessionDeleted(sessionID, taskID string) Event {
	return Event{
		Topic:   TopicSession,
		Type:    TypeSessionDeleted,
		RID:     sessionID,
		Payload: SessionOwnerPayload{TaskID: taskID},
	}
}

// NewSessionsAligned 构造 sessions.aligned 事件。
func NewSessionsAligned(taskID string, inserted, touched, deleted int, sessionIDs []string) Event {
	return Event{
		Topic: TopicSession,
		Type:  TypeSessionsAligned,
		RID:   taskID,
		Payload: SessionsAlignedPayload{
			Inserted:   inserted,
			Touched:    touched,
			Deleted:    deleted,
			SessionIDs: sessionIDs,
		},
	}
}

// NewServeRuntimeAttentionChanged 构造 serve_runtime.attention_changed 事件。
// RID 为 ServeRuntime 主键 instVersion（P1.4.9：单字符串实例令牌）。
func NewServeRuntimeAttentionChanged(instVersion, taskID string) Event {
	return Event{
		Topic:   TopicServeRuntime,
		Type:    TypeServeRuntimeAttentionChanged,
		RID:     instVersion,
		Payload: ServeRuntimeTaskPayload{TaskID: taskID},
	}
}

// NewServeRuntimeRunStatusChanged 构造 serve_runtime.run_status_changed 事件。
// RID 为 ServeRuntime 主键 instVersion（P1.4.9：单字符串实例令牌）。
func NewServeRuntimeRunStatusChanged(instVersion, taskID, from, to string, available bool) Event {
	return Event{
		Topic: TopicServeRuntime,
		Type:  TypeServeRuntimeRunStatusChanged,
		RID:   instVersion,
		Payload: ServeRuntimeRunStatusChangedPayload{
			TaskID:    taskID,
			From:      from,
			To:        to,
			Available: available,
		},
	}
}

// NewServeRuntimeSessionError 构造 serve_runtime.session_error 事件（task-notifications
// D2）。RID 为 ServeRuntime 主键 instVersion（P1.4.9：单字符串实例令牌）；statusCode/
// isRetryable 可空（nil 表缺失）。
func NewServeRuntimeSessionError(instVersion, taskID, sessionID, name, message string, statusCode *int, isRetryable *bool) Event {
	return Event{
		Topic: TopicServeRuntime,
		Type:  TypeServeRuntimeSessionError,
		RID:   instVersion,
		Payload: ServeRuntimeSessionErrorPayload{
			TaskID:      taskID,
			SessionID:   sessionID,
			Name:        name,
			Message:     message,
			StatusCode:  statusCode,
			IsRetryable: isRetryable,
		},
	}
}

// NewResyncRequested 构造 resync.requested 控制事件。
// 事件无主体，RID 固定为空字符串；诊断 triggerID 不进入本事件模型（不另加字段）。
func NewResyncRequested() Event {
	return Event{Topic: TopicControl, Type: TypeResyncRequested, RID: "", Payload: struct{}{}}
}
