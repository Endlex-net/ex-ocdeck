package notification

import (
	"time"

	ocdeckevent "ocdeck/internal/domain/event"
)

// run_status 聚合三态字面量（task 层 agentStatus 投影值；"" 表不可用）。
const (
	runStatusIdle  = "idle"
	runStatusBusy  = "busy"
	runStatusRetry = "retry"
)

// retryErrorWindow retry/error 计时窗口（spec「通知触发」：1 分钟固定，不可配置）。
const retryErrorWindow = 60 * time.Second

// taskState 每任务触发器内存态（design D3 字段表，逐字段对齐）。进程内存态、
// MUST NOT 持久化；仅 run loop goroutine 串行触达（无锁——事件处理、计时判定、
// 门禁复验与消费标记全部在同一串行化上下文，spec「通知抑制、启动基线与对账」）。
// instVersion 绑定 runtime 实例令牌（B3）：换代时整体重建（旧实例的计时/名额/
// 去重不延续到新实例），到期门禁复验实例一致才允许投递。
type taskState struct {
	instVersion     string
	idleSince       *time.Time // 进入 idle 的武装时刻；nil=未武装（扫描时以 idleSince+当前配置阈值判定，支持热更新）
	retryDeadline   *time.Time
	errorDeadline   *time.Time
	errorSeen       bool // 本 episode 首个 error 只武装一次；重复 error 仅更新 lastError；episode 结束复位
	episodeActive   bool
	episodeConsumed bool // episode 名额（spec「发送前门禁与投递原子性」仲裁表）
	idleSuppressed  bool // idle 周期以通知/消费结束；新 busy→idle 迁移时复位重新武装

	notifiedQuestions   map[string]struct{} //（注意力类型, request ID）去重；大小以当前 pending 集合为上界
	notifiedPermissions map[string]struct{} // 与 question 去重键相互独立

	lastError ocdeckevent.ServeRuntimeSessionErrorPayload // episode 内最新一条 session.error（重复不延长计时，仅更新详情）
}

func newTaskState(instVersion string) *taskState {
	return &taskState{
		instVersion:         instVersion,
		notifiedQuestions:   map[string]struct{}{},
		notifiedPermissions: map[string]struct{}{},
	}
}

// startEpisode 进入异常周期（迁移为 retry，或非 busy 时观察到 session.error）：
// episode 开启并取消 idle 计时（spec idle 取消条件全集含「进入异常周期」）。
// 计时由调用方按触发源分别武装（retryDeadline/errorDeadline）。
func (st *taskState) startEpisode() {
	st.episodeActive = true
	st.idleSince = nil
}

// endEpisode 聚合回到 busy：episode 关闭、名额释放、retry/error 计时取消、
// errorSeen 复位（已恢复语义，spec retry/error requirement）。
func (st *taskState) endEpisode() {
	st.episodeActive = false
	st.episodeConsumed = false
	st.errorSeen = false
	st.retryDeadline = nil
	st.errorDeadline = nil
}
