// oc_spi_bridge.go 实现 C2/C6 残余修复的生产桥（registry/Provider 窄接口 →
// task.OCClient）：buildTaskOCClient 的主路径改为经 Provider.SessionControl/
// StatusReader/AttentionIO/AsyncPrompt/TitleUpdater 出口调用——能力健康观测
// （reportConversion）在 SPI 出口内置，生产调用不再绕过能力出口（C6）；
// 无 SPI 窄接口对应的方法（Health/Probe/raw-SubscribeEvents/ProbePromptAsync-
// Capability——探活与原始事件通道，非能力面）保留 OCClient fallback 工厂委托
// （OCClient 测试接缝过渡裁决保持）。任务书契约：OCClient 仅留显式测试接缝+
// 非能力面 fallback；生产分派经 registry.Get（fail-closed）。
package task

import (
	"context"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
	"ocdeck/internal/infrastructure/store"
)

// spiBridgeOCClient 把 provider 窄接口出口包装为 OCClient（task 层生产调用桥）。
// 有窄接口对应的方法经出口转发（出口自带 reportConversion 能力观测）；无对应的
// 方法（Health/Probe/SubscribeEvents/ProbePromptAsyncCapability）经 fallback
// （既有 OCClient 工厂——测试接缝与探活通道）。
type spiBridgeOCClient struct {
	p appagent.Provider
	// binding 持久化 binding 本体（provider 出口入参）。
	binding domain.TaskBinding
	// dir 是 binding 解出的 runtime directory（v1 data.directory）。
	dir string
	// taskID 是 binding 解出的观测身份键（v1 data.taskId）。
	taskID string
	// fallback 承接无窄接口对应的方法（不得为 nil——构造入口保证）。
	fallback OCClient
}

var _ OCClient = (*spiBridgeOCClient)(nil)

// bridgeSessionKey 按 binding 命名空间构造 SessionKey（opencode-v2-provider
// tasks 6.5，design D4 替换表：v2 任务 → (opencode-v2, default, upstreamID)；
// v1/未知命名空间回退 v1 常量——存量行为不变）。
func (c *spiBridgeOCClient) bridgeSessionKey(upstreamID string) domain.SessionKey {
	if c.binding.ProviderID != "" {
		return domain.SessionKey{
			ProviderID:         c.binding.ProviderID,
			ProviderInstanceID: c.binding.ProviderInstanceID,
			UpstreamID:         upstreamID,
		}
	}
	return domain.SessionKey{
		ProviderID:         store.ProviderIDV1,
		ProviderInstanceID: store.ProviderInstanceV1Default,
		UpstreamID:         upstreamID,
	}
}

// toBridgeSessions 把 domain.AgentSession 列表映射回 appagent.Session 形状
// （spi_provider.toAgentSession 的逆向；Raw 不透传——D8 DTO 不含 Raw 保留面）。
func toBridgeSessions(in []domain.AgentSession) []appagent.Session {
	out := make([]appagent.Session, 0, len(in))
	for _, s := range in {
		parentID := ""
		if s.Parent != nil {
			parentID = s.Parent.UpstreamID
		}
		out = append(out, appagent.Session{
			ID:       s.Key.UpstreamID,
			Title:    s.Title,
			Time:     appagent.SessionTime{Created: float64(s.CreatedAt), Updated: float64(s.UpdatedAt)},
			ParentID: parentID,
		})
	}
	return out
}

// sessionStatusMap 桥 StatusReader.Current → OCClient.SessionStatus 形状：
// 键抽 UpstreamID；unknown 条目跳过（SSE/REST 既有「未知枚举不应用」语义，
// 消费方对未记录 session 按 idle 参与聚合，行为一致）。
func sessionStatusMap(in map[domain.SessionKey]domain.AgentStatus) map[string]appagent.SessionStatus {
	out := make(map[string]appagent.SessionStatus, len(in))
	for k, st := range in {
		var t appagent.SessionStatusType
		switch st {
		case domain.StatusIdle:
			t = appagent.StatusIdle
		case domain.StatusBusy:
			t = appagent.StatusBusy
		case domain.StatusRetry:
			t = appagent.StatusRetry
		default:
			continue // unknown/未来新枚举：跳过不入 map（不参与聚合）。
		}
		out[k.UpstreamID] = appagent.SessionStatus{Type: t}
	}
	return out
}

func (c *spiBridgeOCClient) Health(ctx context.Context) (appagent.HealthResponse, error) {
	return c.fallback.Health(ctx)
}

func (c *spiBridgeOCClient) Probe(ctx context.Context) (string, error) {
	return c.fallback.Probe(ctx)
}

func (c *spiBridgeOCClient) ListSessions(ctx context.Context, dir string, limit int) ([]appagent.Session, error) {
	_ = dir
	res, err := c.p.Sessions().List(ctx, c.binding)
	if err != nil {
		return nil, err
	}
	// v1 List 内部固定 limit=1000；桥忽略调用方 limit（既有消费点不依赖 limit
	// 截断语义）。Overflow 信号携带为 ErrSessionOverflow（与 v1 client 同形）。
	if res.Overflow {
		return toBridgeSessions(res.Sessions), appagent.ErrSessionOverflow
	}
	return toBridgeSessions(res.Sessions), nil
}

func (c *spiBridgeOCClient) GetSession(ctx context.Context, dir, id string) (appagent.Session, error) {
	_ = dir
	got, err := c.p.Sessions().Get(ctx, c.binding, c.bridgeSessionKey(id))
	if err != nil {
		return appagent.Session{}, err
	}
	return toBridgeSessions([]domain.AgentSession{got})[0], nil
}

func (c *spiBridgeOCClient) CreateSession(ctx context.Context, dir, title string) (appagent.Session, error) {
	_ = dir
	// SPI 形状无 title 参数（标题经 TitleUpdater 单独承载，D7）；既有锚定创建
	// 消费点 title=""（空串），桥内丢弃 title 保持等价。
	created, err := c.p.Sessions().Create(ctx, c.binding)
	if err != nil {
		return appagent.Session{}, err
	}
	return toBridgeSessions([]domain.AgentSession{created})[0], nil
}

func (c *spiBridgeOCClient) DeleteSession(ctx context.Context, dir, id string) error {
	_ = dir
	return c.p.Sessions().Delete(ctx, c.binding, c.bridgeSessionKey(id))
}

func (c *spiBridgeOCClient) SessionStatus(ctx context.Context, dir string) (map[string]appagent.SessionStatus, error) {
	_ = dir
	cur, err := c.p.Status().Current(ctx, c.binding)
	if err != nil {
		return nil, err
	}
	return sessionStatusMap(cur), nil
}

func (c *spiBridgeOCClient) SubscribeEvents(ctx context.Context, dir string, onEvent func(appagent.Event), onReconnect func()) error {
	_ = dir
	// SPI EventSource 是 typed 流（D8 映射后 domain.AgentEvent）——与 raw
	// appagent.Event 回调形态不同：typed 流的原始投影面不经本桥（raw 订阅保留
	// fallback——startSSE 的原始回退通道既有形态）。
	return c.fallback.SubscribeEvents(ctx, c.dir, onEvent, onReconnect)
}

func (c *spiBridgeOCClient) ListPermissions(ctx context.Context, dir string) ([]appagent.PermissionRequest, error) {
	_ = dir
	// scope 通道：Session 字段仅参与健康观测归属（REST 传输是全目录快照），
	// key 的 UpstreamID 留空（runtime scope 观测面）。
	out, err := c.p.Attention().ListPermissions(ctx, c.binding, c.bridgeSessionKey(""))
	if err != nil {
		return nil, err
	}
	return out, nil // appagent.PermissionRequest = domain.PermissionPayload（alias）。
}

func (c *spiBridgeOCClient) ListQuestions(ctx context.Context, dir string) ([]appagent.QuestionRequest, error) {
	_ = dir
	out, err := c.p.Attention().ListQuestions(ctx, c.binding, c.bridgeSessionKey(""))
	if err != nil {
		return nil, err
	}
	return out, nil // appagent.QuestionRequest = domain.QuestionPayload（alias）。
}

func (c *spiBridgeOCClient) ReplyPermission(ctx context.Context, dir, requestID, reply string) error {
	_ = dir
	at := domain.Attention{ID: requestID, Kind: domain.AttentionPermission, Session: c.bridgeSessionKey(requestID)}
	return c.p.Attention().Reply(ctx, c.binding, at, appagent.AttentionReply{Action: reply})
}

func (c *spiBridgeOCClient) PromptAsync(ctx context.Context, dir, sessionID, messageID, text string, files []appagent.PromptFilePart) appagent.PromptTransportResult {
	_ = dir
	req := appagent.PromptRequest{MessageID: messageID, Text: text, Files: nil}
	if len(files) > 0 {
		req.Files = make([]appagent.PromptFile, 0, len(files))
		for _, f := range files {
			req.Files = append(req.Files, appagent.PromptFile{URL: f.URL, Mime: f.Mime, Filename: f.Filename})
		}
	}
	res, _ := c.p.Prompts().Send(ctx, c.binding, c.bridgeSessionKey(sessionID), req)
	// Outcome→Kind 一一对应（字面值同形：accepted/http_response/transport_
	// unknown/pre_send_failure——agentprovider.PromptOutcome 常量表）。
	return appagent.PromptTransportResult{
		Kind:       appagent.PromptTransportResultKind(res.Outcome),
		StatusCode: res.StatusCode,
		Body:       res.Body,
		Detail:     res.Detail,
	}
}

func (c *spiBridgeOCClient) ProbePromptAsyncCapability(ctx context.Context) appagent.ProbeCapabilityState {
	// 无 SPI 窄接口对应（D7 能力探测经 GET /doc 传输细节；SPI 层能力状态经
	// reportConversion 承载）——fallback 既有通道。
	return c.fallback.ProbePromptAsyncCapability(ctx)
}

func (c *spiBridgeOCClient) UpdateSessionTitle(ctx context.Context, dir, id, title string) error {
	_ = dir
	return c.p.Titles().Sync(ctx, c.binding, c.bridgeSessionKey(id), title)
}

// bridgeClientFor 在 provider 实现可承载能力出口（非测试接缝）时构造桥；
// 返回 nil 表示维持既有 OCClient 工厂路径（测试接缝/过渡形态）。
// 判定：provider.Events() 非 nil 窄接口可用（测试接缝 minimalProvider 的窄
// 接口为 nil，C1 形态）——以 Sessions() 出口探针（能力调用主面）。
func (m *Manager) bridgeClientFor(data *opencodeBindingData, binding domain.TaskBinding, fallback OCClient) OCClient {
	p, err := m.providerFor(data.ProviderID)
	if err != nil || p == nil {
		return nil
	}
	if p.Sessions() == nil {
		// 测试接缝形态（minimalProvider：窄接口 nil）——既有 ocFactory 路径。
		return nil
	}
	return &spiBridgeOCClient{p: p, binding: binding, dir: data.Directory, taskID: data.TaskID, fallback: fallback}
}
