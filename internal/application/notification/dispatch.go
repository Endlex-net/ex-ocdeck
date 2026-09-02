package notification

import (
	"context"
	"log"
	"strings"
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
// 矩阵：web 启用即已配置；bark endpoint 与 token 均非空才算已配置；macos 仅
// darwin 且 notifier 可用才算可用，否则 skipped——运行时可用性经
// ChannelAvailability 可选接口，未实现视为可用）。真实 dispatch 与测试通知
// 共用 resolveOneChannel，保证 skipped 语义一致。
func resolveChannels(channels []notification.Channel, cfg notification.Config) []notification.ResolvedChannel {
	var out []notification.ResolvedChannel
	for _, ch := range channels {
		if rc, ok := resolveOneChannel(ch, cfg); ok {
			out = append(out, rc)
		}
	}
	return out
}

// resolveOneChannel 单渠道启用/配置/可用性判定。ok=false 表示 skipped。
func resolveOneChannel(ch notification.Channel, cfg notification.Config) (notification.ResolvedChannel, bool) {
	switch ch.Name() {
	case "web":
		if !cfg.Channels.Web.Enabled {
			return notification.ResolvedChannel{}, false
		}
		return notification.ResolvedChannel{Channel: ch}, true
	case "bark":
		if !cfg.Channels.Bark.Enabled || cfg.Channels.Bark.Endpoint == "" || cfg.Channels.Bark.Token == "" {
			return notification.ResolvedChannel{}, false
		}
		return notification.ResolvedChannel{
			Channel: ch,
			Config: notification.ChannelConfig{
				Endpoint: cfg.Channels.Bark.Endpoint,
				Token:    cfg.Channels.Bark.Token,
			},
		}, true
	case "macos":
		if !cfg.Channels.Macos.Enabled || !channelAvailable(ch) {
			return notification.ResolvedChannel{}, false
		}
		return notification.ResolvedChannel{Channel: ch}, true
	case "wecom":
		if !cfg.Channels.Wecom.Enabled || cfg.Channels.Wecom.URL == "" {
			return notification.ResolvedChannel{}, false
		}
		return notification.ResolvedChannel{
			Channel: ch,
			Config: notification.ChannelConfig{
				Endpoint: cfg.Channels.Wecom.URL,
			},
		}, true
	default:
		return notification.ResolvedChannel{}, false
	}
}

// channelAvailable 运行时可用性：实现 ChannelAvailability 且 Available()=false
// 时 skipped；未实现接口视为可用（web/bark 由配置判定）。
func channelAvailable(ch notification.Channel) bool {
	av, ok := ch.(notification.ChannelAvailability)
	return !ok || av.Available()
}

// dispatch 多渠道并行投递（design D4：单次投递全过程使用 DispatchPlan 固化配置；
// spec「通知渠道投递与降级」：单一渠道失败不影响其他渠道、不阻塞任务主流程、
// 失败记日志；Title 由内容组装统一携带任务名，本层不做标题降级）。异步
// goroutine 执行（run loop 先标记消费再投递；投递期间到达的新事件按 spec 已接受
// 竞态语义处理）。全渠道失败不自动重试（已消费语义由调用方保证）。
// LLM 停止原因总结（D9）在渠道投递前执行：LLM 副作用只发生在门禁全部通过之后
// （dispatch 调用点），预算内降级——失败/超时/未装配/空白/超长输出不延迟投递
// 超过上界、不因此失败或丢失通知。
func (n *Notifier) dispatch(ctx context.Context, plan *notification.DispatchPlan, in notification.Intent) {
	n.dispatchWG.Add(1)
	go func() {
		defer n.dispatchWG.Done()
		in = n.summarize(ctx, plan, in)
		deliverParallel(ctx, plan.Channels, in, func(name string, res notification.Result) {
			if !res.OK {
				log.Printf("notify: channel %s failed for task %s (%s): %s",
					name, in.TaskID, in.Category, res.Err)
			}
		})
	}()
}

// SendTestNotification 测试通知投递（spec「测试通知」；design D11
// SetNotificationTester 窄端口）。跳过 active/类别复验；总开关/URL 由调用方
// （api）拦截后传入已解析 baseURL。与真实 dispatch 共享渠道解析与并行 Send；
// MUST NOT 调用 LLM。返回注入渠道的逐渠道结果（保序）。
func (n *Notifier) SendTestNotification(ctx context.Context, cfg notification.Config, baseURL string) []notification.ChannelResult {
	intent := TestIntent(testURL(strings.TrimRight(baseURL, "/")))
	out := make([]notification.ChannelResult, len(n.opts.Channels))
	var deliver []notification.ResolvedChannel
	var deliverIdx []int
	for i, ch := range n.opts.Channels {
		rc, ok := resolveOneChannel(ch, cfg)
		if !ok {
			out[i] = notification.ChannelResult{
				Name:   ch.Name(),
				Status: notification.ChannelStatusSkipped,
			}
			continue
		}
		deliver = append(deliver, rc)
		deliverIdx = append(deliverIdx, i)
	}
	if len(deliver) == 0 {
		return out
	}
	results := deliverParallel(ctx, deliver, intent, nil)
	for j, rc := range deliver {
		status := notification.ChannelStatusFailed
		errMsg := results[j].Err
		if results[j].OK {
			status = notification.ChannelStatusSuccess
			errMsg = ""
		}
		out[deliverIdx[j]] = notification.ChannelResult{
			Name:   rc.Channel.Name(),
			Status: status,
			Error:  errMsg,
		}
	}
	return out
}

// deliverParallel 多渠道并行 Send（真实 dispatch 与测试通知共用）：Title 由
// 内容组装统一携带任务名，各渠道原样投递（无 CapGroup 渠道无标题降级——分组
// 缺失仅表现为通知中心不折叠，spec「通知渠道投递与降级」）。onResult 可选
// （真实路径记失败日志）。
func deliverParallel(ctx context.Context, channels []notification.ResolvedChannel, in notification.Intent, onResult func(name string, res notification.Result)) []notification.Result {
	out := make([]notification.Result, len(channels))
	var wg sync.WaitGroup
	for i, rc := range channels {
		wg.Add(1)
		go func(i int, rc notification.ResolvedChannel) {
			defer wg.Done()
			res := rc.Channel.Send(ctx, in, rc.Config)
			out[i] = res
			if onResult != nil {
				onResult(rc.Channel.Name(), res)
			}
		}(i, rc)
	}
	wg.Wait()
	return out
}
