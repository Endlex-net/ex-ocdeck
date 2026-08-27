// sessions_test.go 验证 P1.4.5 session 用例与 Align 编排的事务边界
// （design.md D0:80/86 + canonical opencode-orchestration spec overflow 语义）。
//
// 覆盖：
//   - RunAlign overflow（complete=false）：notice CAS 失败 MUST NOT 执行 Align；
//     Align 失败 MUST NOT 回滚已写入的 overflow notice；
//   - RunAlign complete：notice expected 失配返回 AlignConflict → 重读 Task 重新决策后
//     有界重试成功；
//   - claim/touch/delete/align 的 commit helper 发布条件（NoopPublisher 阶段以
//     recordingPublisher 断言调用位：仅真实变更发布）。
package task

import (
	"context"
	"errors"
	"strings"
	"testing"

	"ocdeck/internal/application"
	ocdeckevent "ocdeck/internal/domain/event"
	ocdecksess "ocdeck/internal/domain/session"
	ocdecktask "ocdeck/internal/domain/task"
)

// fakeSessionRepo 实现 application.SessionRepository（仅本测试所需子集，其余 panic）。
type fakeSessionRepo struct {
	claimRes    application.ClaimResult
	claimErr    error
	touchRes    application.MutationResult
	touchErr    error
	delAffected int
	delErr      error
	alignRes    application.AlignResult
	alignErr    error
	// alignCallCount 记录 Align 调用次数（断言 CAS 失败 MUST NOT 执行 Align）。
	alignCallCount int
}

func (r *fakeSessionRepo) Claim(ctx context.Context, taskID string, obs ocdecksess.Observation) (application.ClaimResult, error) {
	return r.claimRes, r.claimErr
}
func (r *fakeSessionRepo) TouchOwned(ctx context.Context, taskID string, sessionID ocdecksess.ID, lastSeenAt int64) (application.MutationResult, error) {
	return r.touchRes, r.touchErr
}
func (r *fakeSessionRepo) DeleteOwned(ctx context.Context, taskID string, sessionID ocdecksess.ID) (int, error) {
	return r.delAffected, r.delErr
}
func (r *fakeSessionRepo) Align(ctx context.Context, taskID string, mode ocdecksess.AlignMode, observed []ocdecksess.Observation, complete bool, notice application.NoticeMutation) (application.AlignResult, error) {
	r.alignCallCount++
	if r.alignErr != nil {
		return application.AlignResult{}, r.alignErr
	}
	return r.alignRes, nil
}
func (r *fakeSessionRepo) OwnedSessions(ctx context.Context, taskID string) ([]ocdecksess.ID, error) {
	panic("not used")
}
func (r *fakeSessionRepo) OwnerOf(ctx context.Context, sessionID ocdecksess.ID) (string, bool, error) {
	panic("not used")
}

// alignCall 记录一次 Align 调用的参数（供编排测试断言）。
type alignCall struct {
	complete bool
	notice   application.NoticeMutation
}

// fakeAlignPorts 实现 apptask.AlignPorts（overflow CAS / Align 冲突编排测试用）。
//
// notices 模拟 tasks.notice 列（nil=NULL）；alignCalls 记录调用序；alignErrs 按调用序
// 消费（空则返回 alignRes）。afterAlign 在每次 Align 返回前触发（模拟并发写 notice）。
type fakeAlignPorts struct {
	notices    map[string]*string
	casFail    bool
	alignErrs  []error
	alignRes   application.AlignResult
	alignCalls []alignCall
	afterAlign func()
}

func (p *fakeAlignPorts) Align(ctx context.Context, taskID string, mode ocdecksess.AlignMode, observed []ocdecksess.Observation, complete bool, notice application.NoticeMutation) (application.AlignResult, error) {
	call := alignCall{complete: complete, notice: notice}
	p.alignCalls = append(p.alignCalls, call)
	var err error
	if len(p.alignErrs) > 0 {
		err = p.alignErrs[0]
		p.alignErrs = p.alignErrs[1:]
	}
	if p.afterAlign != nil {
		p.afterAlign()
	}
	if err != nil {
		return application.AlignResult{}, err
	}
	return p.alignRes, nil
}

func (p *fakeAlignPorts) UpdateTaskNoticeCAS(ctx context.Context, id string, expected, newNotice *string) (application.MutationResult, error) {
	if p.casFail {
		return application.MutationResult{}, nil
	}
	cur := p.notices[id]
	if noticePtrEqual(cur, expected) {
		if noticePtrEqual(cur, newNotice) {
			return application.MutationResult{Matched: true}, nil
		}
		p.notices[id] = newNotice
		return application.MutationResult{Matched: true, Changed: true}, nil
	}
	return application.MutationResult{}, nil
}

func (p *fakeAlignPorts) GetTaskRow(ctx context.Context, id string) (application.TaskSnapshot, error) {
	return application.TaskSnapshot{ID: id, Status: "active", Notice: p.notices[id]}, nil
}

func noticePtrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func strPtr(s string) *string { return &s }

// TestP145_RunAlign_OverflowCASFailNoAlign 验证 overflow（complete=false）时 notice CAS
// 失败：返回错误且 MUST NOT 执行 Align（design.md D0:86）。
func TestP145_RunAlign_OverflowCASFailNoAlign(t *testing.T) {
	p := &fakeAlignPorts{notices: map[string]*string{}, casFail: true}

	_, err := RunAlign(context.Background(), p, "t1", ocdecksess.AlignModeRepo, nil, false)
	if err == nil || !strings.Contains(err.Error(), "session overflow notice") {
		t.Fatalf("err = %v, want session overflow notice error", err)
	}
	if len(p.alignCalls) != 0 {
		t.Fatalf("Align calls = %d, want 0 (CAS failure must not run Align)", len(p.alignCalls))
	}
}

// TestP145_RunAlign_OverflowAlignFailNoticePreserved 验证 overflow notice 事务外 CAS 成功后
// Align 失败：错误传播，已写入的 overflow notice MUST NOT 回滚（design.md D0:86）。
func TestP145_RunAlign_OverflowAlignFailNoticePreserved(t *testing.T) {
	p := &fakeAlignPorts{
		notices:   map[string]*string{},
		alignErrs: []error{errTestAlignStore},
	}

	_, err := RunAlign(context.Background(), p, "t1", ocdecksess.AlignModeRepo, nil, false)
	if err == nil || !strings.Contains(err.Error(), "align store error") {
		t.Fatalf("err = %v, want align error propagated", err)
	}
	// overflow notice 已写入且保留。
	raw := p.notices["t1"]
	if raw == nil {
		t.Fatal("overflow notice should be written and preserved after Align failure")
	}
	notices, perr := ocdecktask.ParseNoticesJSON(*raw)
	if perr != nil || len(notices) != 1 || notices[0].Code != ocdecktask.NoticeCodeSessionOverflow {
		t.Fatalf("notice = %v (err=%v), want single session_overflow", notices, perr)
	}
}

var errTestAlignStore = &testError{"align store error"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

// TestP162_OverflowCASRealChange_PublishesActivityChangedEvenIfAlignFails 验证 P1.6.2
// overflow 分支（LifecycleService 路径）：notice CAS 命中且真实变更（Changed=true）
// 时发布 task.activity_changed；发布先于 Align 提交，
// 随后 Align 失败 MUST NOT 回滚已发布事件（事务外独立提交，design.md D0:86）。
func TestP162_OverflowCASRealChange_PublishesActivityChangedEvenIfAlignFails(t *testing.T) {
	repo := &fakeTaskRepo{mutationRes: application.MutationResult{Matched: true, Changed: true, UpdatedAtAdvanced: true}}
	sessions := &fakeSessionRepo{alignErr: errTestAlignStore}
	pub := &recordingPublisher{}
	svc := New(Options{Tasks: repo, Read: &fakeReadRepo{}, Sessions: sessions, Publish: pub})

	_, err := svc.AlignSessions(context.Background(), "t1", ocdecksess.AlignModeRepo, nil, false)
	if err == nil || !strings.Contains(err.Error(), "align store error") {
		t.Fatalf("err = %v, want align error propagated", err)
	}
	if len(pub.events) != 1 || pub.events[0] != string(ocdeckevent.TypeTaskActivityChanged) {
		t.Fatalf("events = %v, want [task.activity_changed] published and not rolled back", pub.events)
	}
}

// TestP162_OverflowCASFail_NoAlignNoPublish 验证 overflow notice CAS 未收敛
// （!Matched 重试耗尽 / GetTaskRow 错误）：返回错误且 MUST NOT 执行 Align、不发布任何事件。
func TestP162_OverflowCASFail_NoAlignNoPublish(t *testing.T) {
	newSvc := func(repo *fakeTaskRepo, read *fakeReadRepo) (*LifecycleService, *fakeSessionRepo, *recordingPublisher) {
		sessions := &fakeSessionRepo{}
		pub := &recordingPublisher{}
		return New(Options{Tasks: repo, Read: read, Sessions: sessions, Publish: pub}), sessions, pub
	}

	t.Run("CAS mismatch exhausts", func(t *testing.T) {
		svc, sessions, pub := newSvc(&fakeTaskRepo{mutationRes: application.MutationResult{}}, &fakeReadRepo{})
		_, err := svc.AlignSessions(context.Background(), "t1", ocdecksess.AlignModeRepo, nil, false)
		if err == nil || !strings.Contains(err.Error(), "did not converge") {
			t.Fatalf("err = %v, want CAS convergence error", err)
		}
		if sessions.alignCallCount != 0 {
			t.Fatalf("Align calls = %d, want 0", sessions.alignCallCount)
		}
		if len(pub.events) != 0 {
			t.Fatalf("events = %v, want none", pub.events)
		}
	})
	t.Run("get task error", func(t *testing.T) {
		svc, sessions, pub := newSvc(&fakeTaskRepo{}, &fakeReadRepo{getErr: errors.New("db down")})
		_, err := svc.AlignSessions(context.Background(), "t1", ocdecksess.AlignModeRepo, nil, false)
		if err == nil || !strings.Contains(err.Error(), "get task") {
			t.Fatalf("err = %v, want get task error", err)
		}
		if sessions.alignCallCount != 0 {
			t.Fatalf("Align calls = %d, want 0", sessions.alignCallCount)
		}
		if len(pub.events) != 0 {
			t.Fatalf("events = %v, want none", pub.events)
		}
	})
}

// TestP145_RunAlign_CompleteConflictRetries 验证 complete 路径 notice expected 失配：
// Align 返回 AlignConflict → 重读 Task、重新经 domain 决策后有界重试成功；
// 重试的 NoticeMutation.Expected 基于重读后的 notice。
func TestP145_RunAlign_CompleteConflictRetries(t *testing.T) {
	p := &fakeAlignPorts{
		notices: map[string]*string{
			"t1": strPtr(`[{"code":"session_overflow","message":"m","ts":1}]`),
		},
		// 第一次 Align 返回 conflict（事务内 notice expected 失配，整事务回滚）。
		alignErrs: []error{&application.AlignConflict{TaskID: "t1"}},
	}
	// 第一次 Align 后模拟并发方清除了 notice：重读得到 NULL，重新决策 Expected=nil。
	p.afterAlign = func() { p.notices["t1"] = nil }

	if _, err := RunAlign(context.Background(), p, "t1", ocdecksess.AlignModeRepo, nil, true); err != nil {
		t.Fatalf("RunAlign: %v (want retry success)", err)
	}
	if len(p.alignCalls) != 2 {
		t.Fatalf("Align calls = %d, want 2 (conflict + retry)", len(p.alignCalls))
	}
	if p.alignCalls[1].notice.Expected != nil {
		t.Fatalf("retry Expected = %v, want nil (notice re-read after conflict)", p.alignCalls[1].notice.Expected)
	}
	if p.alignCalls[1].notice.New != nil {
		t.Fatalf("retry New = %v, want nil (no overflow left to clear)", p.alignCalls[1].notice.New)
	}
}

// TestP145_RunAlign_CompleteConflictExhausts 验证 conflict 持续不收敛时有界重试耗尽返回错误。
func TestP145_RunAlign_CompleteConflictExhausts(t *testing.T) {
	conflict := &application.AlignConflict{TaskID: "t1"}
	errs := make([]error, 32) // 远超 8 次上限。
	for i := range errs {
		errs[i] = conflict
	}
	p := &fakeAlignPorts{notices: map[string]*string{}, alignErrs: errs}

	_, err := RunAlign(context.Background(), p, "t1", ocdecksess.AlignModeRepo, nil, true)
	if err == nil || !strings.Contains(err.Error(), "did not converge") {
		t.Fatalf("err = %v, want convergence exhaustion error", err)
	}
}

// TestP145_ClaimTouchDelete_CommitHelpers 验证 claim/touch/delete 的发布条件：
// 仅真实变更（Changed/affected>0）发布对应 session 事件；未命中/同值/冲突不发布。
func TestP145_ClaimTouchDelete_CommitHelpers(t *testing.T) {
	newSvcWith := func(sessions application.SessionRepository) (*LifecycleService, *recordingPublisher) {
		pub := &recordingPublisher{}
		svc := New(Options{Tasks: &fakeTaskRepo{}, Read: &fakeReadRepo{}, Sessions: sessions, Publish: pub})
		return svc, pub
	}

	t.Run("claim changed publishes", func(t *testing.T) {
		repo := &fakeSessionRepo{claimRes: application.ClaimResult{Claimed: true, Changed: true}}
		svc, pub := newSvcWith(repo)
		if _, err := svc.ClaimSession(context.Background(), "t1", ocdecksess.Observation{ID: "s1"}); err != nil {
			t.Fatal(err)
		}
		if len(pub.events) != 1 || pub.events[0] != string(ocdeckevent.TypeSessionClaimed) {
			t.Fatalf("events = %v, want [session.claimed]", pub.events)
		}
	})
	t.Run("claim same-value no-op not publishes", func(t *testing.T) {
		repo := &fakeSessionRepo{claimRes: application.ClaimResult{Claimed: true}}
		svc, pub := newSvcWith(repo)
		if _, err := svc.ClaimSession(context.Background(), "t1", ocdecksess.Observation{ID: "s1"}); err != nil {
			t.Fatal(err)
		}
		if len(pub.events) != 0 {
			t.Fatalf("same-value claim should not publish, got %v", pub.events)
		}
	})
	t.Run("claim conflict not publishes", func(t *testing.T) {
		repo := &fakeSessionRepo{claimRes: application.ClaimResult{Claimed: false, OwnerTaskID: "t2"}}
		svc, pub := newSvcWith(repo)
		if _, err := svc.ClaimSession(context.Background(), "t1", ocdecksess.Observation{ID: "s1"}); err != nil {
			t.Fatal(err)
		}
		if len(pub.events) != 0 {
			t.Fatalf("conflict claim should not publish, got %v", pub.events)
		}
	})
	t.Run("touch changed publishes", func(t *testing.T) {
		repo := &fakeSessionRepo{touchRes: application.MutationResult{Matched: true, Changed: true}}
		svc, pub := newSvcWith(repo)
		if _, err := svc.TouchOwnedSession(context.Background(), "t1", "s1", 99); err != nil {
			t.Fatal(err)
		}
		if len(pub.events) != 1 || pub.events[0] != string(ocdeckevent.TypeSessionTouched) {
			t.Fatalf("events = %v, want [session.touched]", pub.events)
		}
	})
	t.Run("touch same-value not publishes", func(t *testing.T) {
		repo := &fakeSessionRepo{touchRes: application.MutationResult{Matched: true}}
		svc, pub := newSvcWith(repo)
		if _, err := svc.TouchOwnedSession(context.Background(), "t1", "s1", 99); err != nil {
			t.Fatal(err)
		}
		if len(pub.events) != 0 {
			t.Fatalf("same-value touch should not publish, got %v", pub.events)
		}
	})
	t.Run("delete affected publishes", func(t *testing.T) {
		repo := &fakeSessionRepo{delAffected: 1}
		svc, pub := newSvcWith(repo)
		if n, err := svc.DeleteOwnedSession(context.Background(), "t1", "s1"); err != nil || n != 1 {
			t.Fatalf("delete: n=%d err=%v", n, err)
		}
		if len(pub.events) != 1 || pub.events[0] != string(ocdeckevent.TypeSessionDeleted) {
			t.Fatalf("events = %v, want [session.deleted]", pub.events)
		}
	})
	t.Run("delete miss not publishes", func(t *testing.T) {
		repo := &fakeSessionRepo{delAffected: 0}
		svc, pub := newSvcWith(repo)
		if n, err := svc.DeleteOwnedSession(context.Background(), "t1", "s1"); err != nil || n != 0 {
			t.Fatalf("delete: n=%d err=%v", n, err)
		}
		if len(pub.events) != 0 {
			t.Fatalf("missed delete should not publish, got %v", pub.events)
		}
	})
}

// TestP145_AlignCommitHelpers 验证 align 提交点：session 行计数>0 发布一次 sessions.aligned；
// 全量无变化不发布；notice 真实变更（Changed=true，含同秒）另发 task.activity_changed。
func TestP145_AlignCommitHelpers(t *testing.T) {
	newPub := func() (*LifecycleService, *recordingPublisher) {
		pub := &recordingPublisher{}
		return New(Options{Tasks: &fakeTaskRepo{}, Read: &fakeReadRepo{}, Sessions: &fakeSessionRepo{}, Publish: pub}), pub
	}

	t.Run("row changes publish sessions.aligned", func(t *testing.T) {
		svc, pub := newPub()
		svc.commitSessionsAligned("t1", application.AlignResult{
			Inserted: 1, Touched: 1, Deleted: 1,
			AffectedSessionIDs: []ocdecksess.ID{"a", "b", "c"},
			OwnedSessionIDs:    []ocdecksess.ID{"a", "b", "c"},
		})
		if len(pub.events) != 1 || pub.events[0] != string(ocdeckevent.TypeSessionsAligned) {
			t.Fatalf("events = %v, want [sessions.aligned]", pub.events)
		}
	})
	t.Run("no change publishes nothing", func(t *testing.T) {
		svc, pub := newPub()
		svc.commitSessionsAligned("t1", application.AlignResult{})
		if len(pub.events) != 0 {
			t.Fatalf("no-change align should not publish, got %v", pub.events)
		}
	})
	t.Run("notice-only align publishes task.activity_changed", func(t *testing.T) {
		svc, pub := newPub()
		svc.commitSessionsAligned("t1", application.AlignResult{
			TaskMutation: application.MutationResult{Matched: true, Changed: true, UpdatedAtAdvanced: true},
		})
		if len(pub.events) != 1 || pub.events[0] != string(ocdeckevent.TypeTaskActivityChanged) {
			t.Fatalf("events = %v, want [task.activity_changed]", pub.events)
		}
	})
	t.Run("same-second notice change publishes task.activity_changed", func(t *testing.T) {
		svc, pub := newPub()
		svc.commitSessionsAligned("t1", application.AlignResult{
			TaskMutation: application.MutationResult{Matched: true, Changed: true},
		})
		if len(pub.events) != 1 || pub.events[0] != string(ocdeckevent.TypeTaskActivityChanged) {
			t.Fatalf("events = %v, want [task.activity_changed]", pub.events)
		}
	})
	t.Run("notice unchanged does not publish", func(t *testing.T) {
		svc, pub := newPub()
		svc.commitSessionsAligned("t1", application.AlignResult{
			TaskMutation: application.MutationResult{Matched: true, Changed: false},
		})
		if len(pub.events) != 0 {
			t.Fatalf("unchanged notice should not publish, got %v", pub.events)
		}
	})
}

// TestP145_CommitAttentionChange 验证 attention 提交位：发布 serve_runtime.attention_changed
// （NoopPublisher 阶段由 recordingPublisher 断言调用位；真实事件生产在 P1.6）。
func TestP145_CommitAttentionChange(t *testing.T) {
	pub := &recordingPublisher{}
	svc := New(Options{Tasks: &fakeTaskRepo{}, Read: &fakeReadRepo{}, Sessions: &fakeSessionRepo{}, Publish: pub})
	svc.CommitAttentionChange("t1", "inst-1")
	if len(pub.events) != 1 || pub.events[0] != string(ocdeckevent.TypeServeRuntimeAttentionChanged) {
		t.Fatalf("events = %v, want [serve_runtime.attention_changed]", pub.events)
	}
}

// TestP145_NoopPublisherSessionUseCases 验证 NoopPublisher 注入下 session 用例无事件泄漏、
// 无 panic（调用位就绪）。
func TestP145_NoopPublisherSessionUseCases(t *testing.T) {
	repo := &fakeSessionRepo{
		claimRes:    application.ClaimResult{Claimed: true, Changed: true},
		touchRes:    application.MutationResult{Matched: true, Changed: true},
		delAffected: 1,
	}
	svc := New(Options{Tasks: &fakeTaskRepo{}, Read: &fakeReadRepo{}, Sessions: repo}) // Publish nil → NoopPublisher
	if _, err := svc.ClaimSession(context.Background(), "t1", ocdecksess.Observation{ID: "s1", UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.TouchOwnedSession(context.Background(), "t1", "s1", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeleteOwnedSession(context.Background(), "t1", "s1"); err != nil {
		t.Fatal(err)
	}
}
