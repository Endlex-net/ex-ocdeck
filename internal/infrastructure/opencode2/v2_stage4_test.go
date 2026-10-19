// v2_stage4_test.go 落实阶段 4 验收（opencode-v2-provider tasks 7.1 / design D9
// 验收矩阵）在 opencode2 侧的三项补测：
//   - 首建补偿场景①（POST 成功后配置失败）：candidate session 受限回收
//     （SessionControl.Delete）；DELETE 失败记清理债务且不误判「从未建立」（D3）
//   - ephemeral 完整链：AcquireEphemeral → EphemeralRuntimeEndpoint → Release
//     （零 session 副作用触达，D3/D9 完整链行）
//   - 任务隔离：同 server 两 session——A 挂起（Release）仅 interrupt A，
//     B 零触达、全程无 DELETE（D3 挂起停止只作用于目标任务 session）
package opencode2

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	domain "ocdeck/internal/domain/agentprovider"
)

// TestStage4_FirstBuildCompensation_ConfigFailAfterPOST 落实首建补偿场景①：
// POST 建 session 成功后配置应用失败 → attempt 失败（不返回 binding）+ candidate
// 经 DELETE 受限回收（仅本次新建 session id）；DELETE 失败 → 清理债务落实例记录
// last_error（含残留 session id——非「从未建立」表述），原错误照常返回。
func TestStage4_FirstBuildCompensation_ConfigFailAfterPOST(t *testing.T) {
	run := func(t *testing.T, deleteStatus int) (debt string) {
		var deletedID string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/environment"):
				w.WriteHeader(http.StatusInternalServerError) // 配置应用失败（场景①触发点）
			case r.Method == http.MethodPost && r.URL.Path == "/api/session":
				_, _ = w.Write(readFixture(t, "session_create.json"))
			case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/session/ses_"):
				deletedID = strings.TrimPrefix(r.URL.Path, "/api/session/")
				w.WriteHeader(deleteStatus)
				if deleteStatus != http.StatusNoContent {
					_, _ = w.Write([]byte(`{"_tag":"InternalError"}`))
				}
			default:
				_, _ = w.Write(readFixture(t, "session_list_page2.json"))
			}
		}))
		t.Cleanup(srv.Close)
		sup, _, recs := newTestSupervisor(t, srv.URL, nil)
		if _, _, err := sup.ensureServer(context.Background()); err != nil {
			t.Fatalf("init: %v", err)
		}
		sup.deps.Env = fakeEnvSeam(func(string, bool) (map[string]string, error) {
			return nil, errors.New("env snapshot unavailable")
		})
		b, err := sup.Acquire(context.Background(), appTaskRef("t1", "/wt"))
		if err == nil {
			t.Fatalf("config failure must fail the attempt (no binding), got %+v", b)
		}
		if b.UpstreamSessionID != "" {
			t.Fatalf("binding must not be returned on failed attempt: %+v", b)
		}
		if deletedID != "ses_fixture01a" {
			t.Fatalf("candidate must be recycled via DELETE, deleted=%q", deletedID)
		}
		recs.mu.Lock()
		if recs.rec.LastError.Valid {
			debt = recs.rec.LastError.String
		}
		recs.mu.Unlock()
		return debt
	}

	t.Run("delete ok", func(t *testing.T) {
		if debt := run(t, http.StatusNoContent); debt != "" {
			t.Fatalf("clean recycle must not record debt: %q", debt)
		}
	})
	t.Run("delete fail records cleanup debt", func(t *testing.T) {
		debt := run(t, http.StatusInternalServerError)
		if debt == "" {
			t.Fatalf("DELETE failure must record cleanup debt")
		}
		if !strings.Contains(debt, "ses_fixture01a") || !strings.Contains(debt, "残留") {
			t.Fatalf("debt must name leftover session (非「从未建立」): %q", debt)
		}
	})
}

// TestStage4_RecycleCandidateSession_Supervisor 落实任务层首建失败补偿的
// supervisor 回收入口（D3 受限回收例外；CandidateSessionRecycler 窄接口）：v2
// binding upstream → DELETE 回收；DELETE 失败记清理债务（含残留 session id，
// 非「从未建立」）并向调用方返回错误；无 upstream 幂等成功零触达。
func TestStage4_RecycleCandidateSession_Supervisor(t *testing.T) {
	run := func(t *testing.T, deleteStatus int) (deletedID, debt string, rerr error) {
		var mu sync.Mutex
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			mu.Lock()
			defer mu.Unlock()
			if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/session/ses_") {
				deletedID = strings.TrimPrefix(r.URL.Path, "/api/session/")
				w.WriteHeader(deleteStatus)
				if deleteStatus != http.StatusNoContent {
					_, _ = w.Write([]byte(`{"_tag":"InternalError"}`))
				}
				return
			}
			_, _ = w.Write(readFixture(t, "session_list_page2.json"))
		}))
		t.Cleanup(srv.Close)
		sup, _, recs := newTestSupervisor(t, srv.URL, nil)
		b := stage4LifecycleBinding(t, "t-rc", "ses_fixture01a")
		rerr = sup.RecycleCandidateSession(context.Background(), b)
		recs.mu.Lock()
		if recs.rec.LastError.Valid {
			debt = recs.rec.LastError.String
		}
		recs.mu.Unlock()
		mu.Lock()
		defer mu.Unlock()
		return deletedID, debt, rerr
	}

	t.Run("delete ok", func(t *testing.T) {
		id, debt, err := run(t, http.StatusNoContent)
		if err != nil {
			t.Fatalf("clean recycle must succeed: %v", err)
		}
		if debt != "" {
			t.Fatalf("clean recycle must not record debt: %q", debt)
		}
		if id != "ses_fixture01a" {
			t.Fatalf("DELETE must target candidate upstream, deleted=%q", id)
		}
	})
	t.Run("delete fail debt and error", func(t *testing.T) {
		_, debt, err := run(t, http.StatusInternalServerError)
		if err == nil {
			t.Fatalf("DELETE failure must surface to caller for compensation error chain")
		}
		if !strings.Contains(debt, "ses_fixture01a") || !strings.Contains(debt, "残留") {
			t.Fatalf("debt must name leftover session (非「从未建立」): %q", debt)
		}
	})
	t.Run("empty upstream idempotent", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("no session side effects expected, got %s %s", r.Method, r.URL.Path)
		}))
		t.Cleanup(srv.Close)
		sup, _, recs := newTestSupervisor(t, srv.URL, nil)
		b := stage4LifecycleBinding(t, "t-rc", "")
		if err := sup.RecycleCandidateSession(context.Background(), b); err != nil {
			t.Fatalf("empty upstream must be idempotent success: %v", err)
		}
		if recs.rec.LastError.Valid {
			t.Fatalf("no debt expected: %q", recs.rec.LastError.String)
		}
	})
}

// TestStage4_Ephemeral_FullChain 落实 ephemeral 完整链（D9）：AcquireEphemeral →
// EphemeralRuntimeEndpoint（端点/派生凭据/目录解析）→ Release 仅回收连接——
// 全链零 session 副作用（除就绪判定外无任何 session API 触达）。
func TestStage4_Ephemeral_FullChain(t *testing.T) {
	var sessionSideEffects int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/api/session") {
			mu.Lock()
			sessionSideEffects++ // 非就绪判定的 session 侧副作用都算违规
			mu.Unlock()
		}
		_, _ = w.Write(readFixture(t, "session_list_page2.json"))
	}))
	t.Cleanup(srv.Close)
	sup, _, _ := newTestSupervisor(t, srv.URL, nil)
	b, err := sup.AcquireEphemeral(context.Background(), appTaskRef("t-eph", "/wt"))
	if err != nil {
		t.Fatalf("AcquireEphemeral: %v", err)
	}
	// 端点解析（provider 侧下沉；删除清理消费）。
	port, password, dir, err := sup.EphemeralRuntimeEndpoint(b)
	if err != nil {
		t.Fatalf("EphemeralRuntimeEndpoint: %v", err)
	}
	if port != 4096 || password == "" || dir != "/wt" {
		t.Fatalf("endpoint: port=%d dir=%q password-empty=%v", port, dir, password == "")
	}
	if err := sup.Release(context.Background(), b); err != nil {
		t.Fatalf("Release(ephemeral): %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if sessionSideEffects != 0 {
		t.Fatalf("ephemeral chain must not touch session side effects, hits=%d", sessionSideEffects)
	}
}

// TestStage4_Isolation_ReleaseA_LeavesB_Untouched 落实任务隔离（D9）：同 server
// 两任务 session（A/B）——A 挂起 Release 仅 interrupt A 恰一次；B 的 session API
// 零触达，全程无任何 DELETE（Release 保留上游 session）。
func TestStage4_Isolation_ReleaseA_LeavesB_Untouched(t *testing.T) {
	var interruptA, interruptsB, deletes int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/session/ses_a/interrupt"):
			interruptA++
			_, _ = w.Write([]byte(`{"interrupted":true}`))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/session/ses_b"):
			interruptsB++
			w.WriteHeader(500)
		case r.Method == http.MethodDelete:
			deletes++
			w.WriteHeader(204)
		default:
			_, _ = w.Write(readFixture(t, "session_list_page2.json"))
		}
	}))
	t.Cleanup(srv.Close)
	sup, _, _ := newTestSupervisor(t, srv.URL, nil)
	bindingA := stage4LifecycleBinding(t, "t-a", "ses_a")
	if err := sup.Release(context.Background(), bindingA); err != nil {
		t.Fatalf("Release(A): %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if interruptA != 1 {
		t.Fatalf("A interrupt hits=%d, want exactly 1", interruptA)
	}
	if interruptsB != 0 {
		t.Fatalf("task B must be untouched by A release, B hits=%d", interruptsB)
	}
	if deletes != 0 {
		t.Fatalf("release must never DELETE sessions, deletes=%d", deletes)
	}
}

// stage4LifecycleBinding 构造 port/directory 与实例一致的 v2 生命周期 binding。
func stage4LifecycleBinding(t *testing.T, taskID, upstream string) domain.TaskBinding {
	t.Helper()
	raw, err := encodeV2BindingData(v2BindingData{Port: 4096, Directory: "/wt", TaskID: taskID,
		Origin: originCreated, Auth: v2BindingAuth{Kind: "derived"}})
	if err != nil {
		t.Fatal(err)
	}
	return domain.TaskBinding{
		Version:            domain.TaskBindingVersion,
		ProviderID:         ProviderID,
		ProviderInstanceID: ProviderInstanceID,
		UpstreamSessionID:  upstream,
		TerminalRef:        v2TerminalRef(taskID),
		Data:               raw,
	}
}
