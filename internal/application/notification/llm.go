package notification

import (
	"context"
	"log"
	"strings"
	"time"

	"ocdeck/internal/domain/notification"
)

// LLM 停止原因总结（task-notifications design D9 + spec「LLM 停止原因总结
//（可选增强）」）：输入仅任务名/类别/类别详情（不拉取会话消息），固定 prompt、
// 预算与降级规则全部常量；LLM 副作用只发生在门禁全部通过之后（dispatch 调用点，
// spec「发送前门禁」），在途投递使用 DispatchPlan 固化的 LLMSummary 开关（D4）。

const (
	// defaultSummaryBudget LLM 总结总预算上界（design D9：整体 5s，含 AI 配置
	// 读取——配置读取在组合根适配方内完成，共享本 ctx）。测试经 Options.LLMBudget
	// 注入可测等价值。
	defaultSummaryBudget = 5 * time.Second

	// summaryMaxTokens prompt 的 max_tokens（design D9：200）。
	summaryMaxTokens = 200

	// summaryMaxOutputChars 输出上限（design D9：超 300 字符 → 降级）。
	summaryMaxOutputChars = 300
)

// summaryPromptTemplate 固定 prompt 模板（design D9 逐字）。
const summaryPromptTemplate = `你是通知摘要助手。根据以下任务运行信息，用一两句中文概括该任务停止或等待人工处理的原因，只基于给定信息，不要臆测。
任务：{{TaskName}}
类别：{{Category}}
详情：{{Detail}}`

// summaryBodyPrefix LLM 总结附加到确定性摘要正文的前缀（spec「通知正文 MUST
// 附带由 LLM 生成的停止原因总结」；artifacts 未规定拼接格式，实现取确定性前缀）。
const summaryBodyPrefix = "AI 总结："

// SummaryCompleter LLM 补全窄端口（design D9：复用 infrastructure/ai completer
// 与 aiStore——组合根适配装配，本包不感知 provider；参数形态与 ai.Request 对齐
// （system/user/max_tokens）。未配置/失败/超时由适配方返回错误或不予装配，本包
// 一律降级为确定性摘要，MUST NOT 因此失败或丢失通知）。
type SummaryCompleter interface {
	Complete(ctx context.Context, system, user string, maxTokens int) (string, error)
}

// categoryLabel 类别的人类可读名（复用 design D4 各类别 Title，不另造文案）。
func categoryLabel(cat notification.Category) string {
	switch cat {
	case notification.CategoryQuestion:
		return "等待你的回答"
	case notification.CategoryPermission:
		return "等待权限确认"
	case notification.CategoryIdle:
		return "任务已空闲"
	case notification.CategoryRetry:
		return "重试未恢复"
	case notification.CategoryError:
		return "运行出错"
	default: // test
		return "测试通知"
	}
}

// summaryDetail 从确定性意图正文提取类别详情（composeBody 的逆：Body = 任务名 +
// "\n" + 详情；形态不符时整段作为详情，不臆测）。
func summaryDetail(in notification.Intent) string {
	if prefix := in.TaskName + "\n"; strings.HasPrefix(in.Body, prefix) {
		return in.Body[len(prefix):]
	}
	return in.Body
}

// buildSummaryPrompt 组装固定 prompt（模板逐字替换，输入仅任务名/类别/详情）。
func buildSummaryPrompt(in notification.Intent, detail string) string {
	return strings.NewReplacer(
		"{{TaskName}}", in.TaskName,
		"{{Category}}", categoryLabel(in.Category),
		"{{Detail}}", detail,
	).Replace(summaryPromptTemplate)
}

// effectiveBudget 计算有效 LLM 预算：硬上界 5s，注入只允许缩短（测试用），
// 零值/负值/超限一律回落 5s 默认——不得放大（E1）。表驱动锁定见
// TestLLM_EffectiveBudget。
func effectiveBudget(injected time.Duration) time.Duration {
	if injected > 0 && injected < defaultSummaryBudget {
		return injected
	}
	return defaultSummaryBudget
}

// summarize 预算内生成停止原因总结并附加到正文：成功且输出有效（TrimSpace
// 非空、≤300 字符）时返回追加总结的意图；开关关闭（计划固化）、未装配、调用
// 失败/超时/空白/超长输出时原样返回（确定性摘要降级，MUST NOT 延迟投递超过
// 预算上界、不因此失败或丢失通知）。预算为硬上界：LLMBudget 注入只允许缩短
// （测试用），非正值用 5s 默认——不得放大（E1）。
func (n *Notifier) summarize(parent context.Context, plan *notification.DispatchPlan, in notification.Intent) notification.Intent {
	if !plan.LLMSummary || n.opts.Summarizer == nil {
		return in
	}
	ctx, cancel := context.WithTimeout(parent, effectiveBudget(n.opts.LLMBudget))
	defer cancel()

	text, err := n.opts.Summarizer.Complete(ctx, "", buildSummaryPrompt(in, summaryDetail(in)), summaryMaxTokens)
	if err != nil {
		log.Printf("notify: llm summary for task %s (%s) unavailable, fallback to deterministic detail: %v", in.TaskID, in.Category, err)
		return in
	}
	text = strings.TrimSpace(text)
	if text == "" || len([]rune(text)) > summaryMaxOutputChars {
		return in
	}
	in.Body += "\n" + summaryBodyPrefix + text
	return in
}
