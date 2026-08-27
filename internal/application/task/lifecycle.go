// Package task 实现 LifecycleService：任务生命周期用例的单一协调器（design.md D0:44/135）。
//
// LifecycleService 持 consumer-owned ports（TaskRepository/TaskReadRepository/SessionRepository
// 等读侧与写侧端口）与 Publisher（窄接口），按用例分文件组织。本包仅依赖 domain + application
// + stdlib，MUST NOT import infrastructure 或 api（import-graph 不变量，design.md D0:55）。
//
// 事件发布责任（design.md D0:133）：domain 决定合法变化（guard），Repository 返回结构化事实，
// application 在提交成功后经集中 commit helper 同步调用非阻塞 Publisher。P1.4.4 阶段注入
// NoopPublisher（不发布任何事件），commit helper 的 publish-after-commit 调用位就绪但本步
// 无实际发布；真实事件生产挂接在 Phase C/P1.6。
package task

import (
	"context"

	"ocdeck/internal/application"
	ocdeckevent "ocdeck/internal/domain/event"
)

// LifecycleService 是任务生命周期用例的单一协调器（design.md D0:44/135）。
//
// 持 TaskRepository（写侧 CAS + guard 视图读）、TaskReadRepository（全行读，供 Get/List）、
// SessionRepository（claim/touch/delete/align，P1.4.5）与 Publisher（事件窄接口，本阶段
// NoopPublisher）。保持单一 service，不拆成每动作一 service（design.md D0:160 反模式约束）。
type LifecycleService struct {
	tasks    application.TaskRepository
	read     application.TaskReadRepository
	sessions application.SessionRepository
	publish  application.Publisher
}

// Options 构造 LifecycleService 的依赖注入。
type Options struct {
	Tasks application.TaskRepository
	Read  application.TaskReadRepository
	// Sessions 为会话归属端口（P1.4.5 claim/touch/delete/align 用例所需）。
	// 调用 session 用例的前提是完整注入（与 Tasks/Read 同为构造期合同）。
	Sessions application.SessionRepository
	Publish  application.Publisher
}

// New 构造 LifecycleService。Publish 为 nil 时注入 NoopPublisher（design.md D0:133）。
func New(opts Options) *LifecycleService {
	publish := opts.Publish
	if publish == nil {
		publish = NoopPublisher{}
	}
	return &LifecycleService{
		tasks:    opts.Tasks,
		read:     opts.Read,
		sessions: opts.Sessions,
		publish:  publish,
	}
}

// NoopPublisher 为 P1.4.4 阶段的占位 Publisher（design.md D0:133）。
//
// 不发布任何事件——事件生产挂接是 Phase C/P1.6 的事。helper 的 publish-after-commit
// 调用位就绪但本步无实际发布，保证 commit helper 形态与真实发布路径一致，便于后续无缝替换。
type NoopPublisher struct{}

// Publish 实现 application.Publisher，丢弃所有事件（P1.4.4 NoopPublisher 阶段）。
func (NoopPublisher) Publish(ocdeckevent.Event) {}

// --- commit helper（design.md D0:133） ---
//
// 集中提交点：在 Repository CAS/事务成功且 Changed=true 后同步调用非阻塞 Publisher。
// guard 拒绝叶节点零副作用（design.md D0:156 决策先于副作用），故 commit helper 仅在
// guard 通过且 Repository 返回 Changed 时才到达。本阶段 Publisher 为 NoopPublisher，
// publish 调用位就绪但不产生实际事件。

// commitTransition 提交状态迁移并在真实变更时发布 task.status_changed（design.md D0:133）。
//
// 仅当 res.StatusChanged=true 时发布（同值 no-op 不发布）；Publish 溢出不回滚业务提交
// （design.md D0:133）。本阶段 Publisher 为 NoopPublisher，调用位就绪无实际发布。
func (s *LifecycleService) commitTransition(ctx context.Context, taskID string, res application.TransitionResult) {
	if !res.StatusChanged {
		return
	}
	s.publish.Publish(ocdeckevent.NewTaskStatusChanged(taskID, string(res.From), string(res.To)))
}

// commitTaskMutation 提交非状态迁移的列写入并在真实变更时发布
// task.activity_changed（task-detail-stream D0：Changed=true 即发，不再要求
// updated_at 跨秒）。同值 no-op（!Changed）不发布；Publish 溢出不回滚业务提交。
func (s *LifecycleService) commitTaskMutation(ctx context.Context, taskID string, res application.MutationResult) {
	if !res.Changed {
		return
	}
	s.publish.Publish(ocdeckevent.NewTaskActivityChanged(taskID))
}

// RequestResync 发布 resync.requested 控制事件（design.md D2 收敛矩阵：提交结果不确定时
// 仅发 resync.requested，订阅方全量重建兜底；事件无主体，RID 固定空串）。
// NoopPublisher 阶段调用位就绪无实际发布（P1.6 挂接真实事件生产）。
func (s *LifecycleService) RequestResync() {
	s.publish.Publish(ocdeckevent.NewResyncRequested())
}
