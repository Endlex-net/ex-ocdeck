package notification

import (
	"context"
	"log"
	"sync"

	"ocdeck/internal/application"
	"ocdeck/internal/domain/notification"
)

// gateStage 门禁复验失败点（spec「发送前门禁与投递原子性」依次复验序列：
// 触发条件 → 总开关 → 类别开关 → URL → 渠道）。任一失败零副作用。
type gateStage int

const (
	gatePass            gateStage = iota
	gateConditionFailed           // 触发条件失效或快照读取失败
	gateMasterOff                 // 总开关关闭
	gateCategoryOff               // 类别开关关闭
	gateURLUnavailable            // 跳转链接 URL 不可用
	gateNoChannel                 // 无启用且已配置渠道
)

// occupiesEpisode 该失败点是否占用 episode 名额（spec 仲裁表：URL 不可用、
// 无启用渠道与已发起投递占名额；总开关/类别开关/触发条件失效不占）。
func (s gateStage) occupiesEpisode() bool {
	return s == gateURLUnavailable || s == gateNoChannel
}

// evaluation 单次候选判定结果：stage==gatePass 时 plan 携带固化投递计划。
type evaluation struct {
	stage gateStage
	plan  *notification.DispatchPlan
}

// evaluate 门禁复验 + DispatchPlan 构造（design D4：门禁复验、URL 推导、LLM
// 开关与 ResolvedChannel 全部从单次候选判定的 cfg 派生；snap 为单次组合快照）。
// condition 为类别特定的触发条件复验（任务仍 active 由本方法统一检查）。无副作用：
// 投递与消费标记由调用方在 gatePass 后执行（先标记已消费再投递，spec）。
func (n *Notifier) evaluate(taskID string, cat notification.Category, cfg notification.Config, snap TaskSnapshot, condition func(TaskSnapshot) bool) evaluation {
	if snap.Task.Status != application.StatusActive || !condition(snap) {
		return evaluation{stage: gateConditionFailed}
	}
	if !cfg.Enabled {
		return evaluation{stage: gateMasterOff}
	}
	if !categoryEnabled(cfg, cat) {
		return evaluation{stage: gateCategoryOff}
	}
	base, err := n.resolveBase(cfg)
	if err != nil {
		log.Printf("notify: task %s (%s): base url unavailable: %v", taskID, cat, err)
		return evaluation{stage: gateURLUnavailable}
	}
	url := taskURL(base, taskID)
	if cat == notification.CategoryTest {
		url = testURL(base)
	}
	channels := resolveChannels(n.opts.Channels, cfg)
	if len(channels) == 0 {
		log.Printf("notify: task %s (%s): no enabled and configured channel", taskID, cat)
		return evaluation{stage: gateNoChannel}
	}
	return evaluation{stage: gatePass, plan: &notification.DispatchPlan{
		URL:        url,
		LLMSummary: cfg.LLMSummary,
		Channels:   channels,
	}}
}

// categoryEnabled 类别开关（spec「配置运行时生效」）；test 跳过类别开关
// （spec「测试通知」：仅检查总开关/URL/渠道）。
func categoryEnabled(cfg notification.Config, cat notification.Category) bool {
	switch cat {
	case notification.CategoryQuestion:
		return cfg.Categories.Question
	case notification.CategoryPermission:
		return cfg.Categories.Permission
	case notification.CategoryIdle:
		return cfg.Categories.Idle
	case notification.CategoryRetry:
		return cfg.Categories.Retry
	case notification.CategoryError:
		return cfg.Categories.Error
	default: // test
		return true
	}
}

// resolveChannels 「已启用且已配置」渠道固化（spec「通知渠道投递与降级」能力
// 矩阵：web 启用即已配置；bark endpoint 与 token 均非空才算已配置；macos 启用即
// 可用——运行环境探测在渠道构造侧（Lane C BuildChannels 按平台构建），本层只按
// 配置开关与参数固化）。
func resolveChannels(channels []notification.Channel, cfg notification.Config) []notification.ResolvedChannel {
	var out []notification.ResolvedChannel
	for _, ch := range channels {
		switch ch.Name() {
		case "web":
			if cfg.Channels.Web.Enabled {
				out = append(out, notification.ResolvedChannel{Channel: ch})
			}
		case "bark":
			if cfg.Channels.Bark.Enabled && cfg.Channels.Bark.Endpoint != "" && cfg.Channels.Bark.Token != "" {
				out = append(out, notification.ResolvedChannel{
					Channel: ch,
					Config: notification.ChannelConfig{
						Endpoint: cfg.Channels.Bark.Endpoint,
						Token:    cfg.Channels.Bark.Token,
					},
				})
			}
		case "macos":
			if cfg.Channels.Macos.Enabled {
				out = append(out, notification.ResolvedChannel{Channel: ch})
			}
		}
	}
	return out
}

// dispatch 多渠道并行投递（design D4：单次投递全过程使用 DispatchPlan 固化配置；
// spec「通知渠道投递与降级」：单一渠道失败不影响其他渠道、不阻塞任务主流程、
// 失败记日志；无 CapGroup 的渠道由本层给标题加 [<TaskName>] 前缀降级）。异步
// goroutine 执行（run loop 先标记消费再投递；投递期间到达的新事件按 spec 已接受
// 竞态语义处理）。全渠道失败不自动重试（已消费语义由调用方保证）。
func (n *Notifier) dispatch(ctx context.Context, plan *notification.DispatchPlan, in notification.Intent) {
	n.dispatchWG.Add(1)
	go func() {
		defer n.dispatchWG.Done()
		var wg sync.WaitGroup
		for _, rc := range plan.Channels {
			wg.Add(1)
			go func(rc notification.ResolvedChannel) {
				defer wg.Done()
				intent := in
				if rc.Channel.Caps()&notification.CapGroup == 0 {
					intent.Title = "[" + in.TaskName + "] " + in.Title
				}
				if res := rc.Channel.Send(ctx, intent, rc.Config); !res.OK {
					log.Printf("notify: channel %s failed for task %s (%s): %s",
						rc.Channel.Name(), in.TaskID, in.Category, res.Err)
				}
			}(rc)
		}
		wg.Wait()
	}()
}
