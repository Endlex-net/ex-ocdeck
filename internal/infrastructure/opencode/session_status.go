package opencode

// SessionStatusEvent 是从 SSE session.status 事件解析出的结构化状态（P1.7.3 门禁
// 2026-08-26 实测固化，opencode 1.18.18）：envelope {type, properties}，sessionID 在
// properties.sessionID，状态在 properties.status.type ∈ {idle,busy,retry}；retry 额外
// 携带 properties.status.{attempt, message, next(epoch ms)}。
// 本类型只做事件解析，task 层 apply/聚合逻辑（P1.8.2）不在此处。
type SessionStatusEvent struct {
	SessionID string
	Status    SessionStatusType
	Attempt   int    // retry：当前重试序号
	Message   string // retry：失败摘要
	Next      int64  // retry：下次尝试时刻（epoch ms）
}

// ParseSessionStatusEvent 识别并解析 session.status 事件（fixture 见
// testdata/session_status_events.jsonl）。sessionID 提取沿用 SessionIDProp 语义
// （优先 properties.sessionID，回退 properties.info.id）。非该 type、缺 sessionID、
// 缺/非法 status.type → (zero, false)，调用方按 fail-closed 忽略，不中断流。
func ParseSessionStatusEvent(ev Event) (SessionStatusEvent, bool) {
	if ev.Type != "session.status" {
		return SessionStatusEvent{}, false
	}
	sid := ev.SessionIDProp()
	if sid == "" {
		return SessionStatusEvent{}, false
	}
	st, ok := ev.Properties["status"].(map[string]interface{})
	if !ok {
		return SessionStatusEvent{}, false
	}
	typ, ok := stringField(st, "type")
	if !ok {
		return SessionStatusEvent{}, false
	}
	var status SessionStatusType
	switch SessionStatusType(typ) {
	case StatusIdle, StatusBusy, StatusRetry:
		status = SessionStatusType(typ)
	default:
		// 枚举外取值视为契约漂移，fail-closed 忽略。
		return SessionStatusEvent{}, false
	}
	out := SessionStatusEvent{SessionID: sid, Status: status}
	if status == StatusRetry {
		if a, ok := floatNumber(st["attempt"]); ok {
			out.Attempt = int(a)
		}
		if m, ok := st["message"].(string); ok {
			out.Message = m
		}
		if n, ok := floatNumber(st["next"]); ok {
			out.Next = int64(n)
		}
	}
	return out, true
}
