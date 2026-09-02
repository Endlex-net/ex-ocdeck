// scheduler_test.go 测试 3.7 队列调度器（能力门禁/CAS/PromptOutcome状态映射/重试/404分流）。
package diffreview

import (
	"context"
	"sync"
	"testing"
)

// mockCallbacks 实现 SchedulerCallbacks。
type mockCallbacks struct {
	mu       sync.Mutex
	locked   bool
	unlocked bool
	failLock bool
}

func (c *mockCallbacks) LockTask(ctx context.Context, taskID string) (func(), error) {
	if c.failLock {
		return nil, errNotFound
	}
	c.mu.Lock()
	c.locked = true
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		c.unlocked = true
		c.mu.Unlock()
	}, nil
}

// newSchedulerTestService 构造带完整 mock 的 service + queue。
func newSchedulerTestService(repo *mockRepo, rt *mockRuntime, prompt *mockPrompt) *Service {
	return newTestService(repo, &mockScope{found: true, kind: "repo"}, &mockDiff{}, rt, prompt)
}

// TestSchedulerCapabilityUnsupportedDirectFailed 验证缓存 unsupported → 直接 queued→failed。
func TestSchedulerCapabilityUnsupportedDirectFailed(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "queued", TargetSessionID: "sess1"}
	rt := &mockRuntime{probeCap: CapabilityUnsupported}
	svc := newSchedulerTestService(repo, rt, &mockPrompt{})
	sch := NewScheduler(SchedulerOptions{Service: svc, Callbacks: &mockCallbacks{}, TaskID: "t1"})
	sch.tick(context.Background())
	if repo.submissions["s1"].Status != "failed" {
		t.Errorf("unsupported capability should directly fail, got %s", repo.submissions["s1"].Status)
	}
	if repo.submissions["s1"].Error != "capability unsupported: prompt_async" {
		t.Errorf("unsupported error text should be fixed, got %s", repo.submissions["s1"].Error)
	}
}

// TestSchedulerCapabilityUnknownKeepsQueued 验证 unknown → 保持 queued。
func TestSchedulerCapabilityUnknownKeepsQueued(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "queued", TargetSessionID: "sess1"}
	rt := &mockRuntime{probeCap: CapabilityUnknown}
	svc := newSchedulerTestService(repo, rt, &mockPrompt{})
	sch := NewScheduler(SchedulerOptions{Service: svc, Callbacks: &mockCallbacks{}, TaskID: "t1"})
	sch.tick(context.Background())
	if repo.submissions["s1"].Status != "queued" {
		t.Errorf("unknown capability should keep queued, got %s", repo.submissions["s1"].Status)
	}
}

// TestSchedulerAcceptedSentCleanup 验证 204 → sent 清理事务。
func TestSchedulerAcceptedSentCleanup(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "queued", TargetSessionID: "sess1"}
	repo.annotations["a1"] = DiffAnnotationRecord{ID: "a1", TaskID: "t1", Revision: 1}
	repo.items["s1"] = []DiffReviewSubmissionItemRecord{{AnnotationID: "a1", AnnotationRevision: 1}}
	rt := &mockRuntime{probeCap: CapabilitySupported, sessStat: SessionStatusIdle}
	prompt := &mockPrompt{outcome: PromptOutcome{Kind: PromptOutcomeAccepted, StatusCode: 204}}
	svc := newSchedulerTestService(repo, rt, prompt)
	sch := NewScheduler(SchedulerOptions{Service: svc, Callbacks: &mockCallbacks{}, TaskID: "t1"})
	sch.tick(context.Background())
	if repo.submissions["s1"].Status != "sent" {
		t.Errorf("accepted should become sent, got %s", repo.submissions["s1"].Status)
	}
	if _, exists := repo.annotations["a1"]; exists {
		t.Errorf("sent cleanup should delete annotation a1")
	}
}

// TestSchedulerTransportUnknownDeliveryUnknown 验证 transport_unknown → delivery_unknown。
func TestSchedulerTransportUnknownDeliveryUnknown(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "queued", TargetSessionID: "sess1"}
	rt := &mockRuntime{probeCap: CapabilitySupported, sessStat: SessionStatusIdle}
	prompt := &mockPrompt{outcome: PromptOutcome{Kind: PromptOutcomeTransportUnknown, Detail: "timeout"}}
	svc := newSchedulerTestService(repo, rt, prompt)
	sch := NewScheduler(SchedulerOptions{Service: svc, Callbacks: &mockCallbacks{}, TaskID: "t1"})
	sch.tick(context.Background())
	if repo.submissions["s1"].Status != "delivery_unknown" {
		t.Errorf("transport_unknown should become delivery_unknown, got %s", repo.submissions["s1"].Status)
	}
}

// TestSchedulerPreSendFailureRetryThenFailed 验证 pre_send_failure 重试耗尽 → failed。
func TestSchedulerPreSendFailureRetryThenFailed(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "queued", TargetSessionID: "sess1"}
	rt := &mockRuntime{probeCap: CapabilitySupported, sessStat: SessionStatusIdle}
	prompt := &mockPrompt{outcome: PromptOutcome{Kind: PromptOutcomePreSendFailure, Detail: "runtime client unavailable"}}
	svc := newSchedulerTestService(repo, rt, prompt)
	sch := NewScheduler(SchedulerOptions{Service: svc, Callbacks: &mockCallbacks{}, TaskID: "t1"})
	sch.tick(context.Background())
	if repo.submissions["s1"].Status != "failed" {
		t.Errorf("pre_send_failure exhausted should become failed, got %s", repo.submissions["s1"].Status)
	}
}

// TestSchedulerHTTP400FailedAndInvalidate 验证 400 → failed + 能力置 unknown 复探。
func TestSchedulerHTTP400FailedAndInvalidate(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "queued", TargetSessionID: "sess1"}
	rt := &mockRuntime{probeCap: CapabilitySupported, sessStat: SessionStatusIdle}
	prompt := &mockPrompt{outcome: PromptOutcome{Kind: PromptOutcomeHTTPResponse, StatusCode: 400, Body: "bad request"}}
	svc := newSchedulerTestService(repo, rt, prompt)
	sch := NewScheduler(SchedulerOptions{Service: svc, Callbacks: &mockCallbacks{}, TaskID: "t1"})
	sch.tick(context.Background())
	if repo.submissions["s1"].Status != "failed" {
		t.Errorf("400 should become failed, got %s", repo.submissions["s1"].Status)
	}
	if !rt.invalidateCalled {
		t.Errorf("400 should invalidate capability (trigger re-probe)")
	}
}

// TestSchedulerHTTP401FailedNoRetry 验证 401 → failed 不重试。
func TestSchedulerHTTP401FailedNoRetry(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "queued", TargetSessionID: "sess1"}
	rt := &mockRuntime{probeCap: CapabilitySupported, sessStat: SessionStatusIdle}
	prompt := &mockPrompt{outcome: PromptOutcome{Kind: PromptOutcomeHTTPResponse, StatusCode: 401, Body: "unauthorized"}}
	svc := newSchedulerTestService(repo, rt, prompt)
	sch := NewScheduler(SchedulerOptions{Service: svc, Callbacks: &mockCallbacks{}, TaskID: "t1"})
	sch.tick(context.Background())
	if repo.submissions["s1"].Status != "failed" {
		t.Errorf("401 should become failed, got %s", repo.submissions["s1"].Status)
	}
}

// TestSchedulerHTTP404EndpointUnsupported 验证 404 会话存在 → 端点不支持 → failed + 缓存 unsupported（零重投）。
func TestSchedulerHTTP404EndpointUnsupported(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "queued", TargetSessionID: "sess1"}
	// F3：404 分流经 GetSession 结构化结果；found → 端点不支持。
	rt := &mockRuntime{probeCap: CapabilitySupported, sessStat: SessionStatusIdle, getSessionResult: GetSessionResult{Status: GetSessionFound}}
	prompt := &mockPrompt{outcome: PromptOutcome{Kind: PromptOutcomeHTTPResponse, StatusCode: 404}}
	svc := newSchedulerTestService(repo, rt, prompt)
	sch := NewScheduler(SchedulerOptions{Service: svc, Callbacks: &mockCallbacks{}, TaskID: "t1"})
	sch.tick(context.Background())
	if repo.submissions["s1"].Status != "failed" {
		t.Errorf("404 endpoint unsupported should become failed, got %s", repo.submissions["s1"].Status)
	}
	if repo.submissions["s1"].Error != "capability unsupported: prompt_async" {
		t.Errorf("404 endpoint unsupported error should be fixed text, got %s", repo.submissions["s1"].Error)
	}
	// F3：端点不支持分支直接写 unsupported（SetCapabilityUnsupported），MUST NOT 调 InvalidateCapability。
	if !rt.setUnsupportedCalled {
		t.Errorf("404 endpoint unsupported should SetCapabilityUnsupported (cache unsupported, zero-redelivery)")
	}
	if rt.invalidateCalled {
		t.Errorf("404 endpoint unsupported must not InvalidateCapability (should set unsupported directly)")
	}
}

// TestSchedulerHTTP404SessionMissing 验证 404 会话不存在 → failed invalid_state。
func TestSchedulerHTTP404SessionMissing(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "queued", TargetSessionID: "sess1"}
	// F3：404 分流经 GetSession 结构化结果；missing → failed invalid_state。
	rt := &mockRuntime{probeCap: CapabilitySupported, sessStat: SessionStatusIdle, getSessionResult: GetSessionResult{Status: GetSessionMissing}}
	prompt := &mockPrompt{outcome: PromptOutcome{Kind: PromptOutcomeHTTPResponse, StatusCode: 404}}
	svc := newSchedulerTestService(repo, rt, prompt)
	sch := NewScheduler(SchedulerOptions{Service: svc, Callbacks: &mockCallbacks{}, TaskID: "t1"})
	sch.tick(context.Background())
	if repo.submissions["s1"].Status != "failed" {
		t.Errorf("404 session missing should become failed, got %s", repo.submissions["s1"].Status)
	}
}

// TestSchedulerHTTP404UnknownProbe 验证 404 GetSession 结果未知 → failed + 能力置 unknown 触发复探（F3）。
func TestSchedulerHTTP404UnknownProbe(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "queued", TargetSessionID: "sess1"}
	rt := &mockRuntime{probeCap: CapabilitySupported, sessStat: SessionStatusIdle, getSessionResult: GetSessionResult{Status: GetSessionUnknown, Detail: "boom"}}
	prompt := &mockPrompt{outcome: PromptOutcome{Kind: PromptOutcomeHTTPResponse, StatusCode: 404}}
	svc := newSchedulerTestService(repo, rt, prompt)
	sch := NewScheduler(SchedulerOptions{Service: svc, Callbacks: &mockCallbacks{}, TaskID: "t1"})
	sch.tick(context.Background())
	if repo.submissions["s1"].Status != "failed" {
		t.Errorf("404 unknown probe should become failed, got %s", repo.submissions["s1"].Status)
	}
	if !rt.invalidateCalled {
		t.Errorf("404 unknown probe should InvalidateCapability (trigger reprobe)")
	}
	if rt.setUnsupportedCalled {
		t.Errorf("404 unknown probe must not SetCapabilityUnsupported (should invalidate unknown)")
	}
}

// TestSchedulerUnexpected2xxDeliveryUnknown 验证意外 2xx → delivery_unknown + 能力置 unknown。
func TestSchedulerUnexpected2xxDeliveryUnknown(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "queued", TargetSessionID: "sess1"}
	rt := &mockRuntime{probeCap: CapabilitySupported, sessStat: SessionStatusIdle}
	prompt := &mockPrompt{outcome: PromptOutcome{Kind: PromptOutcomeHTTPResponse, StatusCode: 200}}
	svc := newSchedulerTestService(repo, rt, prompt)
	sch := NewScheduler(SchedulerOptions{Service: svc, Callbacks: &mockCallbacks{}, TaskID: "t1"})
	sch.tick(context.Background())
	if repo.submissions["s1"].Status != "delivery_unknown" {
		t.Errorf("unexpected 2xx should become delivery_unknown, got %s", repo.submissions["s1"].Status)
	}
	if !rt.invalidateCalled {
		t.Errorf("unexpected 2xx should invalidate capability")
	}
}

// TestSchedulerBusyWaits 验证 busy → 保持 queued（不发送）。
func TestSchedulerBusyWaits(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "queued", TargetSessionID: "sess1"}
	rt := &mockRuntime{probeCap: CapabilitySupported, sessStat: SessionStatusBusy}
	svc := newSchedulerTestService(repo, rt, &mockPrompt{})
	sch := NewScheduler(SchedulerOptions{Service: svc, Callbacks: &mockCallbacks{}, TaskID: "t1"})
	sch.tick(context.Background())
	if repo.submissions["s1"].Status != "queued" {
		t.Errorf("busy should keep queued, got %s", repo.submissions["s1"].Status)
	}
}

// TestSchedulerEmptyQueueNoop 验证空队列 no-op。
func TestSchedulerEmptyQueueNoop(t *testing.T) {
	repo := newMockRepo()
	rt := &mockRuntime{probeCap: CapabilitySupported}
	svc := newSchedulerTestService(repo, rt, &mockPrompt{})
	sch := NewScheduler(SchedulerOptions{Service: svc, Callbacks: &mockCallbacks{}, TaskID: "t1"})
	sch.tick(context.Background()) // should not panic or error
}

// TestSchedulerLockConflictYields 验证任务锁冲突 → 让出。
func TestSchedulerLockConflictYields(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "queued", TargetSessionID: "sess1"}
	rt := &mockRuntime{probeCap: CapabilitySupported, sessStat: SessionStatusIdle}
	svc := newSchedulerTestService(repo, rt, &mockPrompt{})
	cb := &mockCallbacks{failLock: true}
	sch := NewScheduler(SchedulerOptions{Service: svc, Callbacks: cb, TaskID: "t1"})
	sch.tick(context.Background())
	if repo.submissions["s1"].Status != "queued" {
		t.Errorf("lock conflict should yield (keep queued), got %s", repo.submissions["s1"].Status)
	}
}
