package task

import (
	"context"
	"testing"

	"ocdeck/internal/infrastructure/opencode"
)

// TestOCClient_PromptAsync_InterfaceCompliance 验证（design.md D1 / tasks.md 2.4/2.5）：
//   - *opencode.Client 实现 OCClient（编译期断言在 types.go，此处运行时零值检查）
//   - mockOC/readyOC/readyOCWrap/portHealthOC/alwaysUnhealthyOC/healthThenProbeOC/
//     overflowOC/blockingPermOC/blockingBothOC 均 OCClient 兼容（编译保证，此处仅冒烟）
//   - mockOC.PromptAsync 默认返回 pre_send_failure（newMockOC 初始化）
func TestOCClient_PromptAsync_InterfaceCompliance(t *testing.T) {
	// 编译期断言 var _ OCClient = (*opencode.Client)(nil) 在 types.go。
	// 此处构造一个 mockOC 并断言 PromptAsync 可调用、返回预置结果。
	oc := newMockOC(true)
	oc.promptAsyncResult = opencode.PromptResult{
		Kind:       opencode.ResultAccepted,
		StatusCode: 204,
	}
	got := oc.PromptAsync(context.Background(), "/wt", "sess-1", "msg_x", "t")
	if got.Kind != opencode.ResultAccepted || got.StatusCode != 204 {
		t.Fatalf("PromptAsync: %+v", got)
	}

	// readyOC 委托：确认透传到 inner。
	wrap := &readyOC{inner: oc}
	got2 := wrap.PromptAsync(context.Background(), "/wt", "sess-1", "msg_x", "t")
	if got2.Kind != opencode.ResultAccepted {
		t.Fatalf("readyOC delegate: %+v", got2)
	}
}

// TestOCClient_PromptAsync_MockDefaults 验证显式实现的 stub 与 mockOC 默认返回 pre_send_failure
//（design.md D1：stub 不依赖错误字符串匹配，返回结构化 Kind）。
func TestOCClient_PromptAsync_MockDefaults(t *testing.T) {
	stubs := []OCClient{
		newMockOC(true), // newMockOC 初始化 pre_send_failure
		&portHealthOC{},
		&alwaysUnhealthyOC{},
		&healthThenProbeOC{},
		&overflowOC{},
	}
	for i, s := range stubs {
		got := s.PromptAsync(context.Background(), "/wt", "s", "msg_x", "t")
		if got.Kind != opencode.ResultPreSendFailure {
			t.Fatalf("stub[%d] (%T): kind %v want pre_send_failure", i, s, got.Kind)
		}
		if got.Detail == "" {
			t.Fatalf("stub[%d] (%T): detail should be non-empty", i, s)
		}
	}
}

// TestOCClient_PromptAsync_BlockingDelegates 验证 attention 测试 stub 委托到 inner mockOC。
func TestOCClient_PromptAsync_BlockingDelegates(t *testing.T) {
	inner := newMockOC(true)
	inner.promptAsyncResult = opencode.PromptResult{Kind: opencode.ResultHTTPResponse, StatusCode: 400, Body: "bad"}

	perm := &blockingPermOC{inner: inner}
	got := perm.PromptAsync(context.Background(), "/wt", "s", "msg_x", "t")
	if got.Kind != opencode.ResultHTTPResponse || got.StatusCode != 400 {
		t.Fatalf("blockingPermOC delegate: %+v", got)
	}

	both := &blockingBothOC{inner: inner}
	got2 := both.PromptAsync(context.Background(), "/wt", "s", "msg_x", "t")
	if got2.Kind != opencode.ResultHTTPResponse {
		t.Fatalf("blockingBothOC delegate: %+v", got2)
	}
}