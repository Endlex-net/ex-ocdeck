// main_test.go 验证 P1.6.5 bus wiring 集成（P1.6.6 生产侧）：真实 *eventbus.Bus 经
// eventSubscriberAdapter 同时接入生产侧与消费侧——LifecycleService{Publish: bus} 的
// commit helper 发布的事件被 Subscribe(TopicTask) 的订阅者按序收到。
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ocdeck/internal/application"
	appagent "ocdeck/internal/application/agentprovider"
	apptask "ocdeck/internal/application/task"
	"ocdeck/internal/config"
	ocdeckevent "ocdeck/internal/domain/event"
	bindomain "ocdeck/internal/domain/agentprovider"
	ocdecktask "ocdeck/internal/domain/task"
	"ocdeck/internal/infrastructure/ai"
	"ocdeck/internal/infrastructure/auditlog"
	"ocdeck/internal/infrastructure/eventbus"
	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
	"ocdeck/internal/infrastructure/store"
	"ocdeck/internal/task"
)

// stubTaskRepo 嵌入 application.TaskRepository，仅覆盖本测试触达的
// CreateTask/UpdateTaskStatus（其余方法测试不调用）。
type stubTaskRepo struct {
	application.TaskRepository
	transitionRes application.TransitionResult
}

func (r *stubTaskRepo) CreateTask(context.Context, application.TaskSnapshot) error { return nil }

func (r *stubTaskRepo) UpdateTaskStatus(context.Context, string, ocdecktask.Status, *string) (application.TransitionResult, error) {
	return r.transitionRes, nil
}

// stubReadRepo 嵌入 application.TaskReadRepository（本测试不触达读侧方法）。
type stubReadRepo struct {
	application.TaskReadRepository
}

func TestP165_BusWiring_LifecyclePublishesToSubscribers(t *testing.T) {
	repo := &stubTaskRepo{transitionRes: application.TransitionResult{
		MutationResult: application.MutationResult{Matched: true, Changed: true},
		StatusChanged:  true,
		From:           ocdecktask.StatusSuspended,
		To:             ocdecktask.StatusActivating,
	}}
	bus := eventbus.New()
	svc := apptask.New(apptask.Options{Tasks: repo, Read: &stubReadRepo{}, Publish: bus})
	sub := eventSubscriberAdapter{bus}.Subscribe(ocdeckevent.TopicTask)
	defer sub.Close()

	ctx := context.Background()
	if err := svc.CreateTask(ctx, application.TaskSnapshot{ID: "t1", Status: "creating"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateStatus(ctx, "t1", ocdecktask.StatusActivating, nil); err != nil {
		t.Fatal(err)
	}

	recv := func(want ocdeckevent.Type) ocdeckevent.Event {
		t.Helper()
		select {
		case ev := <-sub.C():
			if ev.Type != want {
				t.Fatalf("event type = %s, want %s", ev.Type, want)
			}
			return ev
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s", want)
			return ocdeckevent.Event{}
		}
	}
	if ev := recv(ocdeckevent.TypeTaskCreated); ev.RID != "t1" {
		t.Fatalf("task.created rid = %q, want t1", ev.RID)
	}
	ev := recv(ocdeckevent.TypeTaskStatusChanged)
	if ev.RID != "t1" {
		t.Fatalf("task.status_changed rid = %q, want t1", ev.RID)
	}
	if p, ok := ev.Payload.(ocdeckevent.TaskStatusChangedPayload); !ok ||
		p.From != string(ocdecktask.StatusSuspended) || p.To != string(ocdecktask.StatusActivating) {
		t.Fatalf("task.status_changed payload = %+v, want suspended→activating", ev.Payload)
	}

	// 同值 no-op（StatusChanged=false 且 Changed=false）MUST NOT 发布。
	repo.transitionRes = application.TransitionResult{MutationResult: application.MutationResult{Matched: true}}
	if _, err := svc.UpdateStatus(ctx, "t1", ocdecktask.StatusActivating, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-sub.C():
		t.Fatalf("same-value write should not publish, got %s", ev.Type)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestDiffReviewStartupFailClosed 验证生产接线的启动收敛 fail-closed 语义（F1/F12）：
// 构造 composition-root 等价的 diffreview service（SQLite adapter），ConvergeOnStartup
// 在正常 DB 上成功（sending→delivery_unknown）；断言启动序列在收敛失败时 MUST 不开放 API/调度器
// （run() 中 Reconcile 前调用 ConvergeDiffReviewOnStartup，返回 error 即拒绝启动）。
func TestDiffReviewStartupFailClosed(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	// composition-root 等价：构造 store adapter（与 main.go 同源）。
	repo := store.NewDiffReviewRepoAdapter(db.Queries)

	// 无 sending 行 → 收敛 0 行成功（正常启动路径）。
	n, err := repo.ConvergeDiffReviewOnStartup(ctx)
	if err != nil {
		t.Fatalf("converge on clean db should succeed, got %v", err)
	}
	if n != 0 {
		t.Errorf("clean db converge affected = %d, want 0", n)
	}

	// 构造一个 sending 行，验证收敛成功（sending→delivery_unknown）。
	if err := seedTaskForSubmissions(ctx, db); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if err := seedSendingSubmission(ctx, db, "s1", "t1"); err != nil {
		t.Fatalf("seed sending submission: %v", err)
	}
	n, err = repo.ConvergeDiffReviewOnStartup(ctx)
	if err != nil {
		t.Fatalf("converge with sending row should succeed, got %v", err)
	}
	if n != 1 {
		t.Errorf("converge affected = %d, want 1", n)
	}
}

// TestDiffReviewStartupFailClosed_OnWriteFailure 验证 F12①：收敛写库失败时启动编排 MUST fail-closed
// （不开放 API/调度器）。经生产 gate 函数 diffReviewStartupGate（与 main.go run() 共用同一函数）
// 断言编排层在收敛失败时不调用 openAPI（而非仅断言 converge 返回 error）。
func TestDiffReviewStartupFailClosed_OnWriteFailure(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// 先种 FK 父行 + 一条 sending 行，再关闭 DB 使收敛写失败。
	if err := seedTaskForSubmissions(ctx, db); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if err := seedSendingSubmission(ctx, db, "s1", "t1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	db.Close() // 关闭 DB，后续 ConvergeOnStartup 写库失败。

	repo := store.NewDiffReviewRepoAdapter(db.Queries)
	apiOpened := false
	openAPI := func() error { apiOpened = true; return nil }
	gateErr := diffReviewStartupGate(ctx, repo, openAPI)
	if gateErr == nil {
		t.Fatal("startup gate should return error on converge write failure (fail-closed)")
	}
	if apiOpened {
		t.Fatal("fail-closed violated: openAPI called despite converge write failure")
	}
}

// TestDiffReviewStartupFailClosed_OnSuccessOpensAPI 验证 F12①：收敛成功时启动编排开放 API。
func TestDiffReviewStartupFailClosed_OnSuccessOpensAPI(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	repo := store.NewDiffReviewRepoAdapter(db.Queries)
	apiOpened := false
	openAPI := func() error { apiOpened = true; return nil }
	if err := diffReviewStartupGate(ctx, repo, openAPI); err != nil {
		t.Fatalf("startup gate on clean db should succeed, got %v", err)
	}
	if !apiOpened {
		t.Fatal("converge success should open API")
	}
}

// seedTaskForSubmissions 种 diff_review_submissions 的 FK 父行（project + task；
// 与 store 包测试同款最小行，满足 submissions.task_id → tasks.id → projects.id）。
func seedTaskForSubmissions(ctx context.Context, db *store.DB) error {
	if err := db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main", "repo"); err != nil {
		return err
	}
	return db.CreateTask(ctx, store.TaskRow{
		ID: "t1", ProjectID: "p1", Name: "task", Branch: "b", Status: "suspended", WorktreePath: "/tmp/wt",
	})
}

// seedSendingSubmission 插入一条 sending 状态的 diff_review_submission 行（供启动收敛测试）。
func seedSendingSubmission(ctx context.Context, db *store.DB, id, taskID string) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO diff_review_submissions
		   (id, task_id, status, target_session_id, message_id, note, payload, truncated, error, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, taskID, "sending", "sess1", "msg_seed", "", "", 0, "", 1)
	return err
}

// --- task-permission-mode 装配测试（tasks 3.3 / F4.7）：Judge 注入路径 ---

// 编译期断言：composition root 的 permJudgeAdapter 满足 task.PermissionJudge 端口
// （ai.PermJudge 经适配注入 Manager 的路径与生产 main.go 逐字一致）。
var _ task.PermissionJudge = permJudgeAdapter{}

// TestPermJudgeAdapter_Unconfigured_UncertainErr 未配置 → uncertain + 非 nil error
// （D5/D11 返回语义接缝：adapter 原样透传 error，消费方审计记 FAILED 并转人工）。
func TestPermJudgeAdapter_Unconfigured_UncertainErr(t *testing.T) {
	adapter := permJudgeAdapter{inner: ai.NewPermJudge(ai.LoadStore(t.TempDir()))}
	v, err := adapter.Judge(context.Background(), task.PermissionJudgeInput{
		TaskName: "t", Permission: "bash", Patterns: []string{"rm"},
	})
	if v != task.PermissionVerdictUncertain {
		t.Errorf("verdict = %q, want uncertain", v)
	}
	if err == nil {
		t.Fatal("unconfigured MUST return uncertain + non-nil error (D5/D11)")
	}
}

// TestPermJudgeAdapter_WiredEndToEnd 装配路径端到端：Store 配置指向本地测试 server，
// LLM 输出 XML verdict → 端口适配后返回 PermissionVerdictApprove。
func TestPermJudgeAdapter_WiredEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"<verdict>APPROVE</verdict>"}}]}`)
	}))
	defer srv.Close()

	st := ai.LoadStore(t.TempDir())
	if err := st.Put(ai.ProviderConfig{
		Provider: ai.ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL,
	}); err != nil {
		t.Fatalf("store put: %v", err)
	}
	adapter := permJudgeAdapter{inner: ai.NewPermJudge(st)}
	v, err := adapter.Judge(context.Background(), task.PermissionJudgeInput{
		TaskName: "修复登录 bug", Permission: "bash", Patterns: []string{"rm"},
	})
	if err != nil {
		t.Fatalf("judge: %v", err)
	}
	if v != task.PermissionVerdictApprove {
		t.Errorf("verdict = %q, want approve (wired path)", v)
	}
}

// TestPermJudgeAdapter_FieldMapping 组合根 adapter 全字段映射验证（tasks 8.2：
// 漏映射会静默丢增强数据）——经 httptest 捕获真实 LLM 请求体，解析 user 消息三对象
// 并逐字段断言（平台语境 7 字段 / request / evidence 全字段含 Degraded 与 files）。
func TestPermJudgeAdapter_FieldMapping(t *testing.T) {
	var bodyMu sync.Mutex
	var reqBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyMu.Lock()
		reqBody = body
		bodyMu.Unlock()
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"<verdict>APPROVE</verdict>"}}]}`)
	}))
	defer srv.Close()

	st := ai.LoadStore(t.TempDir())
	if err := st.Put(ai.ProviderConfig{
		Provider: ai.ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL,
	}); err != nil {
		t.Fatalf("store put: %v", err)
	}
	adapter := permJudgeAdapter{inner: ai.NewPermJudge(st)}
	in := task.PermissionJudgeInput{
		TaskName: "my task", Permission: "bash", Patterns: []string{"rm", "ls"},
		ProjectName: "proj", ProjectKind: "repo", TaskMode: "worktree",
		TaskDir: "/wt/t1", ProjectDir: "/repo", Branch: "ocdeck/t1",
		Detail: task.RequestDetail{
			Command:  "rm -rf build",
			Filepath: "a.go", Diff: "-x\n+y",
			Files: []task.RequestFileChange{{Type: "update", RelativePath: "b.go", Patch: "p"}},
			URL:   "https://x", Description: "desc", SubagentType: "sub",
			Pattern: "pat", Path: "/p", Include: "*.go", ParentDir: "/pd",
			Directories: []string{"/d1", "/d2"},
		},
	}
	if _, err := adapter.Judge(context.Background(), in); err != nil {
		t.Fatalf("judge: %v", err)
	}
	bodyMu.Lock()
	body := reqBody
	bodyMu.Unlock()

	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal openai request: %v", err)
	}
	var userContent string
	for _, m := range req.Messages {
		if m.Role == "user" {
			userContent = m.Content
		}
	}
	if userContent == "" {
		t.Fatal("openai request missing user message")
	}
	var payload struct {
		PlatformContext struct {
			TaskName    string `json:"task_name"`
			ProjectName string `json:"project_name"`
			ProjectType string `json:"project_type"`
			TaskMode    string `json:"task_mode"`
			TaskDir     string `json:"task_dir"`
			ProjectDir  string `json:"project_dir"`
			Branch      string `json:"branch"`
		} `json:"platform_context"`
		Request struct {
			Permission string   `json:"permission"`
			Patterns   []string `json:"patterns"`
		} `json:"request"`
		Evidence ai.PermEvidence `json:"evidence"`
	}
	if err := json.Unmarshal([]byte(userContent), &payload); err != nil {
		t.Fatalf("user message not JSON: %v", err)
	}
	pc := payload.PlatformContext
	if pc.TaskName != "my task" || pc.ProjectName != "proj" || pc.ProjectType != "repo" ||
		pc.TaskMode != "worktree" || pc.TaskDir != "/wt/t1" || pc.ProjectDir != "/repo" || pc.Branch != "ocdeck/t1" {
		t.Errorf("platform_context = %+v", pc)
	}
	if payload.Request.Permission != "bash" || len(payload.Request.Patterns) != 2 {
		t.Errorf("request = %+v", payload.Request)
	}
	ev := payload.Evidence
	if ev.Degraded != "" || ev.Command != "rm -rf build" || ev.Filepath != "a.go" ||
		ev.Diff != "-x\n+y" || ev.URL != "https://x" || ev.Description != "desc" ||
		ev.SubagentType != "sub" || ev.Pattern != "pat" || ev.Path != "/p" ||
		ev.Include != "*.go" || ev.ParentDir != "/pd" {
		t.Errorf("evidence fields = %+v", ev)
	}
	if len(ev.Files) != 1 || ev.Files[0].Type != "update" || ev.Files[0].RelativePath != "b.go" || ev.Files[0].Patch != "p" {
		t.Errorf("evidence.files = %+v", ev.Files)
	}
	if len(ev.Directories) != 2 || ev.Directories[0] != "/d1" {
		t.Errorf("evidence.directories = %+v", ev.Directories)
	}
}

// TestPermJudgeAdapter_DegradedReachesJudge Degraded 透传证明（tasks 8.2）：已配置
// store + Degraded 非空 → ai 判定器内短路零 LLM（server 零请求、uncertain + nil）——
// 若 adapter 漏映射 Degraded，已配置 store 必然发出 LLM 请求，本测试变红。
func TestPermJudgeAdapter_DegradedReachesJudge(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"<verdict>APPROVE</verdict>"}}]}`)
	}))
	defer srv.Close()

	st := ai.LoadStore(t.TempDir())
	if err := st.Put(ai.ProviderConfig{
		Provider: ai.ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL,
	}); err != nil {
		t.Fatalf("store put: %v", err)
	}
	adapter := permJudgeAdapter{inner: ai.NewPermJudge(st)}
	in := task.PermissionJudgeInput{
		TaskName: "t", Permission: "bash",
		Detail: task.RequestDetail{Degraded: task.DegradedMissing},
	}
	v, err := adapter.Judge(context.Background(), in)
	if v != task.PermissionVerdictUncertain || err != nil {
		t.Fatalf("got (%v, %v), want (uncertain, nil) — degraded short-circuit", v, err)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Errorf("degraded MUST short-circuit with zero LLM calls, hits = %d", hits)
	}
}

// TestPermJudgeAdapter_DegradedDirectoriesNull R1-F1：directories 字面 null 成员
// （直接构造 metadata RawMessage）→ 真实提取记 malformed_critical → 经真实 adapter 短路零 LLM
// （uncertain + nil），不得绕过降级自动回复。
func TestPermJudgeAdapter_DegradedDirectoriesNull(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"<verdict>APPROVE</verdict>"}}]}`)
	}))
	defer srv.Close()

	st := ai.LoadStore(t.TempDir())
	if err := st.Put(ai.ProviderConfig{
		Provider: ai.ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL,
	}); err != nil {
		t.Fatalf("store put: %v", err)
	}
	adapter := permJudgeAdapter{inner: ai.NewPermJudge(st)}
	detail := task.ExtractRequestDetail("external_directory", json.RawMessage(`{"command":"cat /outside/a","directories":[null]}`))
	if detail.Degraded != task.DegradedMalformed {
		t.Fatalf("degraded = %q, want malformed_critical", detail.Degraded)
	}
	in := task.PermissionJudgeInput{
		TaskName: "t", Permission: "external_directory", Detail: detail,
	}
	v, err := adapter.Judge(context.Background(), in)
	if v != task.PermissionVerdictUncertain || err != nil {
		t.Fatalf("got (%v, %v), want (uncertain, nil) — directories null member MUST degrade", v, err)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Errorf("degraded MUST short-circuit with zero LLM calls, hits = %d", hits)
	}
}

// TestPermJudgeAdapter_FiveStateMapping 判定器返回语义接缝五态映射验收（tasks 7.2，
// 真实 ai.PermJudge 经组合根 adapter，不得仅用 fake Judge 手工错误代替）：
// 未配置 / 非法输出 / 调用失败 / 超时 → uncertain + 非 nil error（task 审计记 FAILED）；
// 合法 UNCERTAIN → uncertain + nil（审计记 UNCERTAIN，转人工）。
func TestPermJudgeAdapter_FiveStateMapping(t *testing.T) {
	newAdapter := func(t *testing.T, body string, status int) task.PermissionJudge {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(srv.Close)
		st := ai.LoadStore(t.TempDir())
		if err := st.Put(ai.ProviderConfig{
			Provider: ai.ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL,
		}); err != nil {
			t.Fatalf("store put: %v", err)
		}
		return permJudgeAdapter{inner: ai.NewPermJudge(st)}
	}
	in := task.PermissionJudgeInput{TaskName: "t", Permission: "bash", Patterns: []string{"rm"}}

	t.Run("未配置 → uncertain + err", func(t *testing.T) {
		adapter := permJudgeAdapter{inner: ai.NewPermJudge(ai.LoadStore(t.TempDir()))}
		v, err := adapter.Judge(context.Background(), in)
		if v != task.PermissionVerdictUncertain || err == nil {
			t.Fatalf("got (%v, %v), want (uncertain, non-nil err)", v, err)
		}
	})
	t.Run("非法输出 → uncertain + err", func(t *testing.T) {
		v, err := newAdapter(t, `{"choices":[{"message":{"content":"approve"}}]}`, http.StatusOK).Judge(context.Background(), in)
		if v != task.PermissionVerdictUncertain || err == nil {
			t.Fatalf("got (%v, %v), want (uncertain, non-nil err)", v, err)
		}
	})
	t.Run("合法 UNCERTAIN → uncertain + nil", func(t *testing.T) {
		v, err := newAdapter(t, `{"choices":[{"message":{"content":"<verdict>UNCERTAIN</verdict>"}}]}`, http.StatusOK).Judge(context.Background(), in)
		if v != task.PermissionVerdictUncertain || err != nil {
			t.Fatalf("got (%v, %v), want (uncertain, nil)", v, err)
		}
	})
	t.Run("调用失败 500 → uncertain + err", func(t *testing.T) {
		v, err := newAdapter(t, `server error`, http.StatusInternalServerError).Judge(context.Background(), in)
		if v != task.PermissionVerdictUncertain || err == nil {
			t.Fatalf("got (%v, %v), want (uncertain, non-nil err)", v, err)
		}
	})
	t.Run("超时 → uncertain + err", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(300 * time.Millisecond)
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"<verdict>APPROVE</verdict>"}}]}`)
		}))
		defer srv.Close()
		st := ai.LoadStore(t.TempDir())
		if err := st.Put(ai.ProviderConfig{
			Provider: ai.ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL,
		}); err != nil {
			t.Fatalf("store put: %v", err)
		}
		adapter := permJudgeAdapter{inner: ai.NewPermJudge(st)}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		v, err := adapter.Judge(ctx, in)
		if v != task.PermissionVerdictUncertain || err == nil {
			t.Fatalf("got (%v, %v), want (uncertain, non-nil err)", v, err)
		}
		// R1-S1：超时原因可识别（completer 层以 %s 拼接错误文本，errors.Is 断链——
		// 以字符串包含 context deadline exceeded 判定）。
		if !strings.Contains(err.Error(), "context deadline exceeded") {
			t.Errorf("err = %v, want context deadline exceeded cause", err)
		}
	})
}

// TestPermAuditAdapter_RecordPassthrough 组合根审计适配器字段透传（tasks 7.1/D11）：
// task.PermAuditRecord 经 adapter 落入临时 JSONL 文件，逐字段断言行内容
// （task → auditlog.Entry → 单行 JSONL，含 ok 记录的 reply 字段与 patterns）。
func TestPermAuditAdapter_RecordPassthrough(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "ai-permission-audit.jsonl")
	al, err := auditlog.NewPermAuditLogger(path)
	if err != nil {
		t.Fatalf("NewPermAuditLogger: %v", err)
	}
	defer al.Close()

	adapter := permAuditAdapter{inner: al}
	rec := task.PermAuditRecord{
		Time:        "2026-09-10T12:00:00Z",
		TaskID:      "t1",
		TaskName:    "my task",
		RequestID:   "perm-1",
		Permission:  "bash",
		Patterns:    []string{"rm", "ls"},
		Verdict:     "APPROVE",
		ReplyResult: "ok",
		Reply:       "once",
		Detail:      "command=rm -rf build",
	}
	if err := adapter.Record(rec); err != nil {
		t.Fatalf("Record: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(lines))
	}
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("line not valid JSON: %v", err)
	}
	want := map[string]string{
		"time": "2026-09-10T12:00:00Z", "task_id": "t1", "task_name": "my task",
		"request_id": "perm-1", "permission": "bash", "verdict": "APPROVE",
		"reply_result": "ok", "reply": "once", "detail": "command=rm -rf build",
	}
	for k, v := range want {
		if fmt.Sprint(got[k]) != v {
			t.Errorf("field %q = %v, want %q", k, got[k], v)
		}
	}
	pats, ok := got["patterns"].([]interface{})
	if !ok || len(pats) != 2 || pats[0] != "rm" || pats[1] != "ls" {
		t.Errorf("patterns = %v, want [rm ls]", got["patterns"])
	}
}

// TestPermissionChain_RESTExtractToAdapter 整链验证（tasks 8.2/F8，REST 真调用）：
// httptest 返回真实 GET /permission 响应（含 metadata）→ 真实客户端 ListPermissions
// 解析归一 → task 提取 → 真实 adapter → LLM 请求体 evidence 逐字透传；降级形态走
// 短路（已配置 store 零 LLM）。
func TestPermissionChain_RESTExtractToAdapter(t *testing.T) {
	var bodyMu sync.Mutex
	var reqBody []byte
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/permission":
			// 真实 /permission 响应（opencode 1.18.18 契约，含 metadata）。
			_, _ = io.WriteString(w, `[{"id":"r1","sessionID":"s1","permission":"bash","patterns":["rm"],"metadata":{"command":"rm -rf build"}}]`)
		default:
			body, _ := io.ReadAll(r.Body)
			atomic.AddInt32(&hits, 1)
			bodyMu.Lock()
			reqBody = body
			bodyMu.Unlock()
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"<verdict>APPROVE</verdict>"}}]}`)
		}
	}))
	defer srv.Close()

	// 真实客户端解析 REST（归一化 metadata）。
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse srv url: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("srv port: %v", err)
	}
	oc := opencode.NewClient(port, "pw", opencode.Options{})
	perms, err := oc.ListPermissions(context.Background(), "/wt")
	if err != nil {
		t.Fatalf("ListPermissions: %v", err)
	}
	if len(perms) != 1 || len(perms[0].Metadata) == 0 {
		t.Fatalf("REST parse = %+v, want 1 request with metadata", perms)
	}

	st := ai.LoadStore(t.TempDir())
	if err := st.Put(ai.ProviderConfig{
		Provider: ai.ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL,
	}); err != nil {
		t.Fatalf("store put: %v", err)
	}
	adapter := permJudgeAdapter{inner: ai.NewPermJudge(st)}

	perm := perms[0]
	detail := task.ExtractRequestDetail(perm.Permission, perm.Metadata)
	in := task.PermissionJudgeInput{
		TaskName: "t", Permission: perm.Permission, Patterns: perm.Patterns, Detail: detail,
	}
	v, err := adapter.Judge(context.Background(), in)
	if err != nil || v != task.PermissionVerdictApprove {
		t.Fatalf("chain: verdict=%v err=%v, want approve", v, err)
	}
	bodyMu.Lock()
	body := reqBody
	bodyMu.Unlock()
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal LLM request: %v", err)
	}
	var evidence string
	for _, m := range req.Messages {
		if m.Role == "user" {
			var payload struct {
				Evidence struct {
					Command string `json:"command"`
				} `json:"evidence"`
			}
			if err := json.Unmarshal([]byte(m.Content), &payload); err == nil && payload.Evidence.Command != "" {
				evidence = payload.Evidence.Command
			}
		}
	}
	if evidence != "rm -rf build" {
		t.Errorf("evidence.command through chain = %q, want rm -rf build", evidence)
	}

	// 降级形态：metadata nil → 提取记 missing_critical（bash 关键类别）→
	// adapter 短路：已配置 store 但零 LLM 请求、uncertain + nil。
	perm.Metadata = nil
	detail = task.ExtractRequestDetail(perm.Permission, perm.Metadata)
	if detail.Degraded != task.DegradedMissing {
		t.Fatalf("degraded = %q, want missing_critical", detail.Degraded)
	}
	in.Detail = detail
	before := atomic.LoadInt32(&hits)
	v, err = adapter.Judge(context.Background(), in)
	if v != task.PermissionVerdictUncertain || err != nil {
		t.Fatalf("degraded chain: got (%v, %v), want (uncertain, nil)", v, err)
	}
	if atomic.LoadInt32(&hits) != before {
		t.Error("degraded chain MUST NOT call LLM (short-circuit)")
	}
}

// 编译期断言：store adapter 满足启动回填门禁端口（task-permission-mode D8/tasks 4.5）。
var _ permissionBackfiller = (*task.StoreAdapter)(nil)

// TestPermissionModeStartupGate 启动回填门禁（D8 顺序：SQL add column（Migrate，更早）→
// Backfill → Reconcile → HTTP open；本测试经生产 gate 函数验证 fail-closed 与回填语义，
// 与 main.go run() 共用同一函数）。
func TestPermissionModeStartupGate(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := seedTaskForSubmissions(ctx, db); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	// 存量 active 任务（启动事实 NULL、持久化模式 ai-auto）。
	if _, err := db.ExecContext(ctx,
		`UPDATE tasks SET status='active', permission_mode='ai-auto' WHERE id='t1'`); err != nil {
		t.Fatalf("seed active: %v", err)
	}
	adapter := task.NewStoreAdapter(db)

	// 成功路径：回填执行且 open 被调用。
	opened := false
	if err := permissionModeStartupGate(ctx, adapter, func() error { opened = true; return nil }); err != nil {
		t.Fatalf("gate: %v", err)
	}
	if !opened {
		t.Fatal("gate must call open on backfill success")
	}
	var mode string
	if err := db.QueryRowContext(ctx,
		`SELECT permission_mode_at_start FROM tasks WHERE id='t1'`).Scan(&mode); err != nil {
		t.Fatalf("read start fact: %v", err)
	}
	if mode != "ai-auto" {
		t.Fatalf("permission_mode_at_start = %q, want ai-auto（active 且 NULL 行回填）", mode)
	}

	// 失败路径：DB 关闭后回填报错 → fail-closed 不调用 open。
	db2, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store2: %v", err)
	}
	adapter2 := task.NewStoreAdapter(db2)
	db2.Close()
	opened2 := false
	gateErr := permissionModeStartupGate(ctx, adapter2, func() error { opened2 = true; return nil })
	if gateErr == nil {
		t.Fatal("gate must fail on backfill error (fail-closed)")
	}
	if opened2 {
		t.Fatal("gate MUST NOT open on backfill error")
	}
}

// 编译期断言：*store.DB 满足命名空间补齐门禁端口（opencode-v1-spi-provider tasks 2.2）。
var _ namespaceBackfiller = (*store.DB)(nil)

// TestNamespaceBackfillGate 命名空间防御性补齐启动门禁直测（B3；形态参照
// TestPermissionModeStartupGate，但被测函数是 namespaceBackfillGate）：
//   - 成功路径：open 回调内断言命名空间已补齐（补齐先于任何命名空间消费者）；
//   - 失败路径（DB 关闭）：gate 错误返回且 open 调用次数为零（fail-closed）；
//   - open 错误原样透传。
func TestNamespaceBackfillGate(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := seedTaskForSubmissions(ctx, db); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	// 预置待补齐行：tasks.provider_id/anchor 两列空串 + 非空锚定 + session 两列空串。
	if _, err := db.ExecContext(ctx,
		`UPDATE tasks SET provider_id = '', anchor_session_id = 'sess-g', anchor_provider_id = '', anchor_provider_instance_id = ''
		 WHERE id = 't1'`); err != nil {
		t.Fatalf("seed broken namespaces: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO task_sessions (task_id, session_id, session_created_at, first_seen_at, last_seen_at, provider_id, provider_instance_id)
		 VALUES ('t1', 'sess-g', 1, 1, 1, '', '')`); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// 成功路径：open 回调运行时命名空间已补齐（补齐先于 open 内的任何消费者）。
	opened := 0
	if err := namespaceBackfillGate(ctx, db, func() error {
		opened++
		var pid, apid, aiid string
		if err := db.QueryRowContext(ctx,
			`SELECT provider_id, anchor_provider_id, anchor_provider_instance_id FROM tasks WHERE id='t1'`).
			Scan(&pid, &apid, &aiid); err != nil {
			return fmt.Errorf("read task namespaces in open: %w", err)
		}
		if pid != store.ProviderIDV1 || apid != store.ProviderIDV1 || aiid != store.ProviderInstanceV1Default {
			return fmt.Errorf("task namespaces in open = (%q,%q,%q), want backfilled defaults", pid, apid, aiid)
		}
		var spid, siid string
		if err := db.QueryRowContext(ctx,
			`SELECT provider_id, provider_instance_id FROM task_sessions WHERE task_id='t1' AND session_id='sess-g'`).
			Scan(&spid, &siid); err != nil {
			return fmt.Errorf("read session namespaces in open: %w", err)
		}
		if spid != store.ProviderIDV1 || siid != store.ProviderInstanceV1Default {
			return fmt.Errorf("session namespaces in open = (%q,%q), want backfilled defaults", spid, siid)
		}
		return nil
	}); err != nil {
		t.Fatalf("gate success path: %v", err)
	}
	if opened != 1 {
		t.Fatalf("open called %d times, want 1", opened)
	}

	// 失败路径：DB 关闭 → 补齐报错 → fail-closed（open 调用次数为零）。
	db2, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store2: %v", err)
	}
	db2.Close()
	opened2 := 0
	gateErr := namespaceBackfillGate(ctx, db2, func() error { opened2++; return nil })
	if gateErr == nil || !strings.Contains(gateErr.Error(), "backfill provider namespaces") {
		t.Fatalf("gate error = %v, want wrapped backfill failure", gateErr)
	}
	if opened2 != 0 {
		t.Fatal("gate MUST NOT call open on backfill error")
	}

	// 透传路径：open 的错误原样返回（不被 gate 吞掉或改写语义）。
	openErr := fmt.Errorf("open-boom-sentinel")
	if got := namespaceBackfillGate(ctx, okNamespaceBackfiller{}, func() error { return openErr }); got != openErr {
		t.Fatalf("gate must pass through open error, got %v", got)
	}
}

// okNamespaceBackfiller 始终成功的补齐桩（透传路径用）。
type okNamespaceBackfiller struct{}

func (okNamespaceBackfiller) BackfillProviderNamespaces(ctx context.Context) error { return nil }

// TestRegistryDispatchFactory_RegisteredAndFailClosed（tasks 3.7 验证）：
//   - v1 已注册 → 工厂返回共享 loopback client（*opencode.Client）；
//   - 空 registry（未注册）→ fail-closed 占位 client：全部方法 errors.Is
//     provider_not_found、PromptAsync 走 pre_send_failure（零网络副作用）。
func TestRegistryDispatchFactory_RegisteredAndFailClosed(t *testing.T) {
	// 正常路径：v1 注册。
	reg := appagent.NewRegistry()
	if err := reg.Register(opencode.NewSPIProvider()); err != nil {
		t.Fatalf("register: %v", err)
	}
	factory := newRegistryDispatchFactory(reg)
	// C2 门禁修复：分派按任务 binding 的 providerID（不再固定查 v1）。
	oc := factory(opencode.ProviderID, 50001, "pw", appagent.Options{})
	if _, ok := oc.(*opencode.Client); !ok {
		t.Fatalf("已注册工厂产物类型 = %T, want *opencode.Client", oc)
	}
	// 未注册 provider 标识 → fail-closed 占位（按标识分派语义）。
	miss := factory("other-v9", 50001, "pw", appagent.Options{})
	if _, err := miss.Health(context.Background()); !errors.Is(err, appagent.ErrProviderNotFound) {
		t.Errorf("未注册 provider 分派 Health = %v, want provider_not_found", err)
	}

	// fail-closed 路径：空 registry。
	empty := appagent.NewRegistry()
	fc := newRegistryDispatchFactory(empty)
	stub := fc(opencode.ProviderID, 50001, "pw", appagent.Options{})
	ctx := context.Background()
	// 代表性方法：REST/SSE/回复路径全部 provider_not_found。
	if _, err := stub.Health(ctx); !errors.Is(err, appagent.ErrProviderNotFound) {
		t.Errorf("Health = %v, want provider_not_found", err)
	}
	if _, err := stub.Probe(ctx); !errors.Is(err, appagent.ErrProviderNotFound) {
		t.Errorf("Probe = %v, want provider_not_found", err)
	}
	if err := stub.SubscribeEvents(ctx, "/d", nil, nil); !errors.Is(err, appagent.ErrProviderNotFound) {
		t.Errorf("SubscribeEvents = %v, want provider_not_found", err)
	}
	// PromptAsync 四分类通道：unregistered 形态为 pre_send_failure（进入
	// provider 前的准入失败经 PromptResult 表达）。
	if res := stub.PromptAsync(ctx, "/d", "ses", "msg", "text", nil); res.Kind != appagent.ResultPreSendFailure {
		t.Errorf("PromptAsync kind = %v, want pre_send_failure", res.Kind)
	}
}

// --- 迁移导入启动门禁（opencode-v1-spi-provider tasks 4.2；生产 gate 直测） ---

// --- C6 门禁修复：能力健康生产装配接线直测 ---

// TestHealthWiring_ProviderReporterAndReleaseReset 生产装配语义直测：
//   - WithHealthReporter 注入后，真实能力调用的观测经报告端口进 tracker
//     （ProviderID 由装配方补齐；epoch 读取 RuntimeEpoch）；
//   - OnRuntimeReady→BumpRuntimeEpoch（换代递增）；OnRuntimeRelease→
//     ResetRuntime（释放侧重置代际与 runtime/session 观测）。
func TestHealthWiring_ProviderReporterAndReleaseReset(t *testing.T) {
	tr := appagent.NewHealthTracker()
	tr.SetProviderID(opencode.ProviderID)
	p := opencode.NewSPIProvider()
	p.WithHealthReporter(func(h appagent.CapabilityHealth) {
		if h.ProviderID == "" {
			h.ProviderID = opencode.ProviderID
		}
		tr.Transition(h)
	}, tr.RuntimeEpoch)

	// 真实能力调用（binding 无效→degraded 观测进 tracker）。data 缺 port（协议
	// 违约形态）但携带 taskId=t1（C6 身份键修复：观测按真实 task ID，与
	// UpstreamSessionID=ses_a 区分）。
	bad := bindomain.TaskBinding{Version: 1, ProviderID: opencode.ProviderID,
		ProviderInstanceID: opencode.ProviderInstanceID, UpstreamSessionID: "ses_a",
		TerminalRef: "t", Data: json.RawMessage(`{"taskId":"t1","directory":"/d"}`)}
	_, _ = p.Events().Subscribe(context.Background(), bad)
	var sawDegraded bool
	for _, h := range tr.Snapshot() {
		if h.TaskID == "t1" && h.State == appagent.CapDegraded {
			sawDegraded = true
		}
	}
	if !sawDegraded {
		t.Error("能力调用失败未产生 degraded 观测（健康接线断裂）")
	}

	// 换代/释放生命周期：Bump→观测 epoch 跟随；Reset→runtime 观测清空。
	if e := tr.BumpRuntimeEpoch("t1"); e != 1 {
		t.Fatalf("epoch = %d, want 1", e)
	}
	tr.Transition(appagent.CapabilityHealth{ProviderID: opencode.ProviderID, TaskID: "t1",
		Primitive: appagent.PrimitiveSessionControl, Scope: appagent.ScopeRuntime, State: appagent.CapAvailable})
	if e := tr.RuntimeEpoch("t1"); e != 1 {
		t.Errorf("RuntimeEpoch = %d, want 1", e)
	}
	// 释放侧：模拟 Release 成功终止 → OnRuntimeRelease→ResetRuntime。
	tr.ResetRuntime("t1")
	if e := tr.RuntimeEpoch("t1"); e != 0 {
		t.Errorf("ResetRuntime 后 epoch = %d, want 0", e)
	}
	for _, h := range tr.Snapshot() {
		if h.TaskID == "t1" && h.Scope != appagent.ScopeProvider {
			t.Errorf("ResetRuntime 后残留 runtime 观测: %+v", h)
		}
	}
}

// TestMigrationImportGate 生产 migrationImportGate 直测（形态随
// TestPermissionModeStartupGate）：
//   - 成功路径：runner 完成（含单任务失败落账）→ open 恰好调用一次；
//   - master 准入门失败（env 未设置）：fail-closed 拒绝启动、open 调用次数为
//     零、任务状态零修改（候选屏障：零 env 读取先于一切结论）；
//   - open 错误原样透传。
func TestMigrationImportGate(t *testing.T) {
	ctx := context.Background()
	okEnv := func() {
		t.Setenv(appagent.MasterSecretEnv, "gate-test-master")
	}

	// supProcBackend 构造真实 process.Manager（无 tmux server 的环境安全：
	// ShowSessionEnv/HasSession 返回空/错误 → runner 按标记缺失/不可读处置，
	// 无 panic——nil procMgr 会 panic，见 process.go）。
	supProcBackend := func(t *testing.T) *process.Manager {
		t.Helper()
		tmpdir, err := process.EnsureTmpDir(t.TempDir())
		if err != nil {
			t.Fatalf("ensure tmpdir: %v", err)
		}
		return process.New(process.Options{SocketName: "ocdeck-test", Tmpdir: tmpdir,
			BaseEnv: process.DefaultBaseEnv(os.LookupEnv)})
	}

	// ① 成功路径：单任务失败（无 runtime env 可读）不影响 gate 通过。
	okEnv()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := seedTaskForSubmissions(ctx, db); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET status='active' WHERE id='t1'`); err != nil {
		t.Fatalf("seed active: %v", err)
	}
	adapter := task.NewStoreAdapter(db)
	tm := task.New(task.Options{
		Cfg:       &config.Config{DataDir: t.TempDir(), ServePortRange: config.PortRange{Min: 50000, Max: 50099}},
		Store:     adapter,
		Proc:      supProcBackend(t),
		Worktree:  task.NewWorktreeAdapter(nil),
		OCFactory: func(_ string, port int, password string, opts appagent.Options) task.OCClient { return nil },
	})
	opened := 0
	if err := migrationImportGate(ctx, tm, func() error { opened++; return nil }); err != nil {
		t.Fatalf("gate success path: %v", err)
	}
	if opened != 1 {
		t.Fatalf("open called %d times, want 1", opened)
	}
	var lastErr sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT last_error FROM tasks WHERE id='t1'`).Scan(&lastErr); err != nil {
		t.Fatalf("read last_error: %v", err)
	}
	if !lastErr.Valid {
		t.Error("single-task failure must be recorded via last_error")
	}

	// ② master 准入门失败：fail-closed + open 不被调用 + 任务状态零修改。
	db2, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store2: %v", err)
	}
	defer db2.Close()
	if err := seedTaskForSubmissions(ctx, db2); err != nil {
		t.Fatalf("seed task2: %v", err)
	}
	if _, err := db2.ExecContext(ctx, `UPDATE tasks SET status='active', last_error=NULL WHERE id='t1'`); err != nil {
		t.Fatalf("seed active2: %v", err)
	}
	// D3 修订后 gate fail-closed 形态 = env 显式空串（显式配置错误）；文件缺失已
	// 改为首启自动供给（EnsureMasterSecret），不再触发 gate 失败。
	t.Setenv(appagent.MasterSecretEnv, "")
	tm2 := task.New(task.Options{
		Cfg:       &config.Config{DataDir: t.TempDir(), ServePortRange: config.PortRange{Min: 50000, Max: 50099}},
		Store:     task.NewStoreAdapter(db2),
		Proc:      task.NewProcessAdapter(supProcBackend(t)),
		Worktree:  task.NewWorktreeAdapter(nil),
		OCFactory: func(_ string, port int, password string, opts appagent.Options) task.OCClient { return nil },
	})
	opened2 := 0
	gateErr := migrationImportGate(ctx, tm2, func() error { opened2++; return nil })
	if gateErr == nil || !strings.Contains(gateErr.Error(), "master secret") {
		t.Fatalf("gate error = %v, want master secret gate failure", gateErr)
	}
	if opened2 != 0 {
		t.Fatal("gate MUST NOT open on master secret failure")
	}
	var status string
	var le sql.NullString
	if err := db2.QueryRowContext(ctx, `SELECT status, last_error FROM tasks WHERE id='t1'`).Scan(&status, &le); err != nil {
		t.Fatalf("read task2: %v", err)
	}
	if status != "active" || le.Valid {
		t.Errorf("候选屏障：任务状态/last_error 零修改，got (%s, %+v)", status, le)
	}

	// ③ open 错误原样透传。
	okEnv()
	openErr := fmt.Errorf("open-boom-sentinel")
	tm3 := task.New(task.Options{
		Cfg:       &config.Config{DataDir: t.TempDir(), ServePortRange: config.PortRange{Min: 50000, Max: 50099}},
		Store:     task.NewStoreAdapter(db),
		Proc:      task.NewProcessAdapter(supProcBackend(t)),
		Worktree:  task.NewWorktreeAdapter(nil),
		OCFactory: func(_ string, port int, password string, opts appagent.Options) task.OCClient { return nil },
	})
	if got := migrationImportGate(ctx, tm3, func() error { return openErr }); got != openErr {
		t.Fatalf("gate must pass through open error, got %v", got)
	}
}
