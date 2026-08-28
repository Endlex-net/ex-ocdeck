package opencode

import (
	"encoding/json"
	"strconv"
	"strings"
)

// SessionErrorEvent 是从 SSE session.error 事件解析出的结构化错误观察
//（task-notifications design D2；真实样本 testdata/session_status_events.jsonl
// 第 6 行，opencode 1.18.18 实测固化）：envelope {type, properties}，sessionID 在
// properties.sessionID，错误体在 properties.error.{name, data.{message, statusCode,
// isRetryable}}。本类型只做事件解析；触发语义（error 计时/episode）归
// application/notification，不在此处。
type SessionErrorEvent struct {
	SessionID   string
	Name        string
	Message     string
	StatusCode  *int  // 可空：仅可无损表示为 Go int 的 JSON 整数
	IsRetryable *bool // 可空：仅 JSON 布尔
}

// ParseSessionErrorEvent 识别并解析 session.error 事件（字段规则唯一表述：spec
//「通知触发——错误未恢复」）。必填 sessionID/error.name/error.data.message 三者
// MUST 为 TrimSpace 后非空的字符串（详情保留原文）；缺失、空白或类型非法 →
// (zero, false) 忽略整个事件（fail-closed，不中断流）。可空字段 statusCode/
// isRetryable 缺失或 null 合法；存在但类型非法（statusCode 非可无损表示为 Go int
// 的 JSON 整数、isRetryable 非布尔）仅降级该字段为缺失，不拒绝整个事件。
//
// sessionID 直接读取 properties["sessionID"]，MUST NOT 走 SessionIDProp 的
// info.id 回退（那是 session.created/status 家族的契约；session.error 的必填键
// 是 properties.sessionID 本身，畸形事件不得借 info.id 混入派发链）。
func ParseSessionErrorEvent(ev Event) (SessionErrorEvent, bool) {
	if ev.Type != "session.error" {
		return SessionErrorEvent{}, false
	}
	sid, ok := ev.Properties["sessionID"].(string)
	if !ok || strings.TrimSpace(sid) == "" {
		return SessionErrorEvent{}, false
	}
	errObj, ok := ev.Properties["error"].(map[string]interface{})
	if !ok {
		return SessionErrorEvent{}, false
	}
	name, ok := errObj["name"].(string)
	if !ok || strings.TrimSpace(name) == "" {
		return SessionErrorEvent{}, false
	}
	data, ok := errObj["data"].(map[string]interface{})
	if !ok {
		return SessionErrorEvent{}, false
	}
	msg, ok := data["message"].(string)
	if !ok || strings.TrimSpace(msg) == "" {
		return SessionErrorEvent{}, false
	}
	out := SessionErrorEvent{SessionID: sid, Name: name, Message: msg}
	// 可空字段：null 与缺失等价；类型非法仅降级该字段。
	if v := data["statusCode"]; v != nil {
		if code, ok := losslessJSONInt(v); ok {
			out.StatusCode = &code
		}
	}
	if v := data["isRetryable"]; v != nil {
		if b, ok := v.(bool); ok {
			out.IsRetryable = &b
		}
	}
	return out, true
}

// losslessJSONInt 判断 v 是否为可无损表示为 Go int 的 JSON 整数：仅接受生产
// SSE 解码路径（parseEvent UseNumber）保留的 json.Number 词法值，经
// strconv.ParseInt 精确判定——小数（含 429.0）、指数记法（1e3）、越界一律否，
// 不经 float64 往返（2^53+1 等奇数大整数不会被舍入破坏）。
func losslessJSONInt(v interface{}) (int, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := strconv.ParseInt(n.String(), 10, strconv.IntSize)
	if err != nil {
		return 0, false
	}
	return int(i), true
}
