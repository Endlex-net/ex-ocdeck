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
// Bark 等通用渠道与其他应用的通知混合呈现，标题 MUST 以 [ocdeck] 标识来源）。
const titleSourcePrefix = "[ocdeck] "

// titleNameMaxLen Title 中任务名段的截断上界（spec「通知内容与跳转链接」：
// 任务名截断至 12 字符，过短时原样使用）。
const titleNameMaxLen = 12

// titleFor Title 统一格式 `[ocdeck] [<任务名>] <类别标题>`（spec「通知内容与
// 跳转链接」为唯一表述；任务名截 12 rune）。空任务名省略任务名段——spec 未
// 规定，取确定性处理（不输出空 []）。
func titleFor(taskName, categoryTitle string) string {
	name := truncate(taskName, titleNameMaxLen)
	if name == "" {
		return titleSourcePrefix + categoryTitle
	}
	return titleSourcePrefix + "[" + name + "] " + categoryTitle
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

// composeBody Body = 任务名 + 换行 + 截断后的详情。
func composeBody(taskName, detail string) string {
	return taskName + "\n" + detail
}

func newIntent(snap TaskSnapshot, cat notification.Category, title, body, url string) notification.Intent {
	return notification.Intent{
		TaskID:   snap.Task.ID,
		TaskName: snap.Task.Name,
		Category: cat,
		Level:    levelFor(cat),
		Title:    titleFor(snap.Task.Name, title),
		Body:     body,
		URL:      url,
	}
}

// questionIntent question 内容组装：Title「[ocdeck] [<任务名>] 等待你的回答」，
// Body = 任务名 + 提问内容（多条 \n 拼接，单字段截 200、整体截 500）。B14：空白
// 提问字段省略，全部为空白时整体使用固定降级文案（spec「通知内容与跳转链接」
// MUST NOT 出现空白详情）。
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
	return newIntent(snap, notification.CategoryQuestion, "等待你的回答", composeBody(snap.Task.Name, detail), url)
}

// permissionIntent permission 内容组装：Title「[ocdeck] [<任务名>] 等待权限确认」，
// Body = 任务名 + 权限名 + patterns（`, ` 拼接，同截断）。B14：空白权限名/patterns
// 条目省略，详情整体为空白时使用固定降级文案（spec 同上）。
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
	return newIntent(snap, notification.CategoryPermission, "等待权限确认",
		composeBody(snap.Task.Name, detail), url)
}

// question/permission 空白详情的固定降级文案（B14；对齐 retry 降级文案的
// 确定性常量风格）。
const (
	questionDetailFallback   = "有新提问等待回答"
	permissionDetailFallback = "有新权限请求等待确认"
)

// errorIntent error 内容组装：Title「[ocdeck] [<任务名>] 运行出错」，Body =
// 任务名 + message（+ ` (HTTP <statusCode>)`，若有）。
func errorIntent(snap TaskSnapshot, last ocdeckevent.ServeRuntimeSessionErrorPayload, url string) notification.Intent {
	detail := last.Message
	if last.StatusCode != nil {
		detail += fmt.Sprintf(" (HTTP %d)", *last.StatusCode)
	}
	return newIntent(snap, notification.CategoryError, "运行出错", composeBody(snap.Task.Name, detail), url)
}

// retryIntent retry 内容组装：Title「[ocdeck] [<任务名>] 重试未恢复」，详情
// 不可得 → 固定降级文案「任务持续处于重试状态」（不放弃投递）。
func retryIntent(snap TaskSnapshot, url string) notification.Intent {
	detail := "任务持续处于重试状态"
	if snap.HasRetryDetail {
		detail = fmt.Sprintf("第 %d 次重试：%s", snap.RetryDetail.Attempt, snap.RetryDetail.Message)
	}
	return newIntent(snap, notification.CategoryRetry, "重试未恢复", composeBody(snap.Task.Name, detail), url)
}

// idleIntent idle 内容组装：Title「[ocdeck] [<任务名>] 任务已空闲」，阈值取
// 触发判定所用配置快照。
func idleIntent(snap TaskSnapshot, idleTimeoutSeconds int, url string) notification.Intent {
	detail := fmt.Sprintf("已空闲超过 %d 秒", idleTimeoutSeconds)
	return newIntent(snap, notification.CategoryIdle, "任务已空闲", composeBody(snap.Task.Name, detail), url)
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
		Title:    titleFor("ocdeck", "测试通知"),
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
