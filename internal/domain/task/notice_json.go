// notice_json.go 实现 typed Notice 集合与持久化 JSON 形态的互转。
//
// JSON 结构逐字对齐 legacy internal/task.noticeEntry（design.md §8）：
//
//	[{code, message, ts, data{sessionName, cleanupTickets, reason, retryable}}]
//
// data 仅对 residual_processes 出现；session_overflow 无 data。此编解码是
// application（notice 决策）与 sqlite adapter（guard 视图重建）共用的单一映射，
// 避免两处各自维护平行的 JSON 形状。
package task

import (
	"encoding/json"
	"fmt"
)

// noticeJSON 为持久化 JSON 行形态（字段 tag 对齐 legacy noticeEntry）。
type noticeJSON struct {
	Code    string                 `json:"code"`
	Message string                 `json:"message"`
	TS      int64                  `json:"ts"`
	Data    map[string]interface{} `json:"data,omitempty"`
}

// ParseNoticesJSON 把 tasks.notice 列的 JSON 数组解析为 typed Notice 集合。
// 空串/NULL 语义（空集合）由调用方处理；此处空串返回空集合、无错误。
// 损坏 JSON 返回 error（fail-closed，对齐 legacy parseNotices）。
func ParseNoticesJSON(raw string) ([]Notice, error) {
	if raw == "" {
		return nil, nil
	}
	var entries []noticeJSON
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, fmt.Errorf("notice json corrupted: %w", err)
	}
	out := make([]Notice, 0, len(entries))
	for _, e := range entries {
		n := Notice{
			Code:    NoticeCode(e.Code),
			Message: e.Message,
			TS:      e.TS,
		}
		if e.Code == string(NoticeCodeResidualProcesses) {
			n.Data = NoticeData{
				SessionName:    noticeString(e.Data["sessionName"]),
				CleanupTickets: noticeStringSlice(e.Data["cleanupTickets"]),
				Reason:         noticeString(e.Data["reason"]),
				Retryable:      noticeBool(e.Data["retryable"]),
			}
		}
		out = append(out, n)
	}
	return out, nil
}

// EncodeNoticesJSON 把 typed Notice 集合编码为 tasks.notice 列的 JSON 字符串。
// 空集合编码为 nil（对应存储层 NULL，对齐 legacy encodeNotices 的 NullString{} 语义）。
func EncodeNoticesJSON(notices []Notice) *string {
	if len(notices) == 0 {
		return nil
	}
	entries := make([]noticeJSON, 0, len(notices))
	for _, n := range notices {
		e := noticeJSON{Code: string(n.Code), Message: n.Message, TS: n.TS}
		if n.Code == NoticeCodeResidualProcesses {
			e.Data = map[string]interface{}{
				"sessionName":    n.Data.SessionName,
				"cleanupTickets": n.Data.CleanupTickets,
				"reason":         n.Data.Reason,
				"retryable":      n.Data.Retryable,
			}
		}
		entries = append(entries, e)
	}
	b, err := json.Marshal(entries)
	if err != nil {
		// noticeJSON 字段均为基本类型/受控 slice，Marshal 不可失败；防御性返回空集编码。
		return nil
	}
	s := string(b)
	return &s
}

// noticeString 从解码后的 data 值提取 string，缺失/类型不符返回空串。
func noticeString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// noticeBool 从解码后的 data 值提取 bool，缺失/类型不符返回 false。
func noticeBool(v interface{}) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

// noticeStringSlice 从解码后的 data 值提取 []string，缺失/类型不符返回 nil。
func noticeStringSlice(v interface{}) []string {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
