// registry_dispatch.go 承载生产 OCClient 工厂的 registry 分派（tasks 3.6/3.7，
// 原 cmd/ocdeck-server 本地实现移入 task 包——生产装配形态测试与实现同包，
// 消除「测试接缝绕过生产工厂」的验证盲区）。
package task

import (
	"context"
	"fmt"

	appagent "ocdeck/internal/application/agentprovider"
	"ocdeck/internal/infrastructure/opencode"
)

// unregisteredOCClient 是 registry 分派失败的 fail-closed 占位 client：
// 工厂无法解析任务 provider 时返回此实现，全部方法返回 provider_not_found 包装
// 错误——零网络副作用（specs「Provider registry 分派」：未注册 fail-closed）。
// 该形态仅在 provider_id 持久化损坏时可达。
type unregisteredOCClient struct{ err *appagent.Error }

func newUnregisteredOCClient() *unregisteredOCClient {
	return &unregisteredOCClient{err: appagent.NewError("registry.get", appagent.ErrKindProviderNotFound,
		fmt.Errorf("task provider 未注册"))}
}

// NewRegistryDispatchFactory 构造经 registry 分派的 OCClient 工厂：
// C2 残余修复（ora-4 裁决 ②「生产 OCClient 固定查 v1+NewClient 不可接受」）：
// 按 providerID 分派经 registry.Get（fail-closed）；分派命中后的传输构造委托
// provider 包内出口（opencode.NewWireClient——「从端点三元组构造传输 client」
// 的调用点收敛进 provider 面，装配方不再直构）。未注册 → fail-closed 占位
// client（provider_not_found，零网络副作用——specs「Provider registry 分派」）。
// 生产能力调用（sessions/status/attention/prompts/titles）经 task 层能力出口桥
// （buildTaskOCClient/startSSE → spiBridgeOCClient）消费 Provider 窄接口，本
// 工厂仅承接无窄接口对应的探活/原始事件通道（Health/Probe/raw-SubscribeEvents/
// ProbePromptAsyncCapability——非能力面 fallback）。
func NewRegistryDispatchFactory(registry *appagent.Registry) OCClientFactory {
	return func(providerID string, port int, password string, opts appagent.Options) OCClient {
		// mutation 证据（已验证 → 红 → 还原）：分派判定旁路（Get 调用删除/恒真）
		// → factory 对未注册 provider 返回传输 client（零 fail-closed）→
		// TestRegistryDispatchFactory_RegisteredAndFailClosed 的 miss/empty 段红。
		if _, err := registry.Get(providerID); err != nil {
			return newUnregisteredOCClient()
		}
		return opencode.NewWireClient(port, password, opts)
	}
}

func (c *unregisteredOCClient) Health(context.Context) (appagent.HealthResponse, error) {
	return appagent.HealthResponse{}, c.err
}
func (c *unregisteredOCClient) Probe(context.Context) (string, error) { return "", c.err }
func (c *unregisteredOCClient) ListSessions(context.Context, string, int) ([]appagent.Session, error) {
	return nil, c.err
}
func (c *unregisteredOCClient) GetSession(context.Context, string, string) (appagent.Session, error) {
	return appagent.Session{}, c.err
}
func (c *unregisteredOCClient) CreateSession(context.Context, string, string) (appagent.Session, error) {
	return appagent.Session{}, c.err
}
func (c *unregisteredOCClient) DeleteSession(context.Context, string, string) error { return c.err }
func (c *unregisteredOCClient) SessionStatus(context.Context, string) (map[string]appagent.SessionStatus, error) {
	return nil, c.err
}
func (c *unregisteredOCClient) SubscribeEvents(context.Context, string, func(appagent.Event), func()) error {
	return c.err
}
func (c *unregisteredOCClient) ListPermissions(context.Context, string) ([]appagent.PermissionRequest, error) {
	return nil, c.err
}
func (c *unregisteredOCClient) ListQuestions(context.Context, string) ([]appagent.QuestionRequest, error) {
	return nil, c.err
}
func (c *unregisteredOCClient) ReplyPermission(context.Context, string, string, string) error {
	return c.err
}
func (c *unregisteredOCClient) PromptAsync(context.Context, string, string, string, string, []appagent.PromptFilePart) appagent.PromptTransportResult {
	return appagent.PromptTransportResult{Kind: appagent.ResultPreSendFailure, Detail: c.err.Error()}
}
func (c *unregisteredOCClient) ProbePromptAsyncCapability(context.Context) appagent.ProbeCapabilityState {
	return appagent.ProbeCapabilityState("unknown")
}
func (c *unregisteredOCClient) UpdateSessionTitle(context.Context, string, string, string) error {
	return c.err
}

var _ OCClient = (*unregisteredOCClient)(nil)
