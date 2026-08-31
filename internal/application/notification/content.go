package notification

import (
	"fmt"
	"strings"

	"ocdeck/internal/application"
	ocdeckevent "ocdeck/internal/domain/event"
	"ocdeck/internal/domain/notification"
)

// 内容组装模板（design D4 逐字模板；降级文案规则唯一表述在 spec「通知内容与
// 跳转链接」）。Body 均含任务名；单字段截 200 字符（rune）、拼接详情整体截 500。

const (
	maxDetailFieldLen = 200
	maxDetailTotalLen = 500
)

// titleSourcePrefix Title 来源前缀（spec「通知内容与跳转链接」：通知可能经
// Bark 等通用渠道与其他应用的通知混合呈现，标题 MUST 以 OC 标识来源）。
const titleSourcePrefix = "OC"

// titleNameMaxLen Title 中任务名段的截断上界（spec「通知内容与跳转链接」：
// 任务名截断至 12 字符，过短时原样使用）。
const titleNameMaxLen = 12

// titleFor Title 统一格式 `OC [<类别枚举值>] <任务名>`（spec「通知内容与
// 跳转链接」为唯一表述；类别用枚举原值，任务名截 12 rune）。空任务名省略
// 任务名段——spec 未规定，取确定性处理。
func titleFor(cat notification.Category, taskName string) string {
	title := titleSourcePrefix + " [" + string(cat) + "]"
	name := truncate(taskName, titleNameMaxLen)
	if name == "" {
		return title
	}
	return title + " " + name
}

// levelFor 级别映射（spec「通知渠道投递与降级」映射表：question/permission →
// timeSensitive；error → critical；retry → timeSensitive；idle/test → active）。
func levelFor(cat notification.Category) notification.Level {
	switch cat {
	case notification.CategoryQuestion, notification.CategoryPermission, notification.CategoryRetry:
		return notification.LevelTimeSensitive
	case notification.CategoryError:
		return notification.LevelCritical
	default: // idle / test
		return notification.LevelActive
	}
}

// truncate 按字符（rune）截断。
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

func newIntent(snap TaskSnapshot, cat notification.Category, body, url string) notification.Intent {
	return notification.Intent{
		TaskID:   snap.Task.ID,
		TaskName: snap.Task.Name,
		Category: cat,
		Level:    levelFor(cat),
		Title:    titleFor(cat, snap.Task.Name),
		Body:     body,
		URL:      url,
	}
}

// questionIntent question 内容组装：Title「OC [question] <任务名>」（统一格式
// 见 titleFor），Body = 提问内容（正文不重复任务名，spec「通知内容与跳转链接」；
// 多条 \n 拼接，单字段截 200、整体截 500）。B14：空白提问字段省略，全部为空白
// 时整体使用固定降级文案（MUST NOT 出现空白详情）。
func questionIntent(snap TaskSnapshot, pq application.PendingQuestion, url string) notification.Intent {
	fields := make([]string, 0, len(pq.Questions))
	for _, q := range pq.Questions {
		if strings.TrimSpace(q.Question) == "" {
			continue // 空白详情字段省略
		}
		fields = append(fields, truncate(q.Question, maxDetailFieldLen))
	}
	detail := truncate(strings.Join(fields, "\n"), maxDetailTotalLen)
	if strings.TrimSpace(detail) == "" {
		detail = questionDetailFallback
	}
	return newIntent(snap, notification.CategoryQuestion, detail, url)
}

// permissionIntent permission 内容组装：Title「OC [permission] <任务名>」，
// Body = 权限名 + patterns（`, ` 拼接，同截断；正文不重复任务名）。B14：空白
// 权限名/patterns 条目省略，详情整体为空白时使用固定降级文案。
func permissionIntent(snap TaskSnapshot, pp application.PendingPermission, url string) notification.Intent {
	patterns := make([]string, 0, len(pp.Patterns))
	for _, p := range pp.Patterns {
		if strings.TrimSpace(p) == "" {
			continue // 空白详情字段省略
		}
		patterns = append(patterns, truncate(p, maxDetailFieldLen))
	}
	detail := ""
	if name := strings.TrimSpace(pp.Permission); name != "" {
		detail = truncate(name, maxDetailFieldLen)
	}
	if len(patterns) > 0 {
		if detail != "" {
			detail += ": "
		}
		detail += strings.Join(patterns, ", ")
	}
	detail = truncate(detail, maxDetailTotalLen)
	if strings.TrimSpace(detail) == "" {
		detail = permissionDetailFallback
	}
	return newIntent(snap, notification.CategoryPermission, detail, url)
}

// question/permission 空白详情的固定降级文案（B14；对齐 retry 降级文案的
// 确定性常量风格）。
const (
	questionDetailFallback   = "有新提问等待回答"
	permissionDetailFallback = "有新权限请求等待确认"
)

// errorIntent error 内容组装：Title「OC [error] <任务名>」，Body = message
// （+ ` (HTTP <statusCode>)`，若有；正文不重复任务名）。
func errorIntent(snap TaskSnapshot, last ocdeckevent.ServeRuntimeSessionErrorPayload, url string) notification.Intent {
	detail := last.Message
	if last.StatusCode != nil {
		detail += fmt.Sprintf(" (HTTP %d)", *last.StatusCode)
	}
	return newIntent(snap, notification.CategoryError, detail, url)
}

// retryIntent retry 内容组装：Title「OC [retry] <任务名>」，Body=`第 <attempt>
// 次重试：<message>`（正文不重复任务名）；详情不可得 → 固定降级文案「任务持续
// 处于重试状态」（不放弃投递）。
func retryIntent(snap TaskSnapshot, url string) notification.Intent {
	detail := "任务持续处于重试状态"
	if snap.HasRetryDetail {
		detail = fmt.Sprintf("第 %d 次重试：%s", snap.RetryDetail.Attempt, snap.RetryDetail.Message)
	}
	return newIntent(snap, notification.CategoryRetry, detail, url)
}

// idleIntent idle 内容组装：Title「OC [idle] <任务名>」，Body=`已空闲超过
// <阈值> 秒`（正文不重复任务名），阈值取触发判定所用配置快照。
func idleIntent(snap TaskSnapshot, idleTimeoutSeconds int, url string) notification.Intent {
	detail := fmt.Sprintf("已空闲超过 %d 秒", idleTimeoutSeconds)
	return newIntent(snap, notification.CategoryIdle, detail, url)
}

// TestIntent 测试通知专用变体（spec「测试通知」：TaskID=notification-test、
// 任务名 ocdeck、固定文案「ocdeck 通知链路测试」、设置页深链；Title 走统一
// 格式，任务名恒为 ocdeck）。
func TestIntent(url string) notification.Intent {
	return notification.Intent{
		TaskID:   "notification-test",
		TaskName: "ocdeck",
		Category: notification.CategoryTest,
		Level:    levelFor(notification.CategoryTest),
		Title:    titleFor(notification.CategoryTest, "ocdeck"),
		Body:     "ocdeck 通知链路测试",
		URL:      url,
	}
}

// --- 跳转 URL 推导（design D8） ---

// resolveBase base 推导顺序：配置 base_url 非空 → 剔除尾部 '/' 后用之；否则
// resolver 从实际监听地址推导（host wildcard 映射 127.0.0.1 在组合根闭包内）。
// 不可用返回 error（门禁 URL 复验失败依据）。
func (n *Notifier) resolveBase(cfg notification.Config) (string, error) {
	if cfg.BaseURL != "" {
		return strings.TrimRight(cfg.BaseURL, "/"), nil
	}
	if n.opts.ResolveBaseURL == nil {
		return "", fmt.Errorf("base url resolver not configured")
	}
	return n.opts.ResolveBaseURL("")
}

// taskURL 真实类别目标页深链（spec「通知内容与跳转链接」）。
func taskURL(base, taskID string) string { return base + "/#/task/" + taskID }

// testURL test 类别设置页深链。
func testURL(base string) string { return base + "/#/configs#notifications" }
