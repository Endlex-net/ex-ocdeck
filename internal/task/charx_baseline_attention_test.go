package task

// charx_baseline_attention_test.go 落地阶段 0 基线组 fixture（tasks.md 1.2）：
// attention 六家族事件（含缺字段畸形静默忽略）、diffreview 能力探测三态、
// 标题同步三态（成功 / 404 降级缓存 / 网络失败不缓存）。
//
// 有效性证据思路（A3：分段断言，杜绝可空过）：
//   - 阶段一先断言全部 asked 建立「完整」pending 集合（逐 ID + 实际 DTO JSON），
//     畸形 asked 若被错误接受会立刻在集合中出现坏 ID；
//   - 阶段二对仍存在的 pending 投递畸形 reply，用其后的有效 reply 作流处理屏障，
//     断言其余 pending 原样保留（畸形 reply 未删除任何条目）；
//   - 阶段三让每个了结家族（permission.replied / permission.v2.replied /
//     question.replied / question.v2.replied / question.rejected /
//     question.v2.rejected）唯一负责关闭一个 pending，终态断言空集。
//     停用任一家族处理 → 其负责的 pending 永不消失 → 终态空集断言稳定失败；
//     停用任一 asked 家族 → 阶段一完整集合断言稳定失败。

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"ocdeck/internal/application/diffreview"
	"ocdeck/internal/infrastructure/opencode"
)

// attentionIDs 返回当前 pending 集合（perm/quest 两类型 ID 集），fixture 断言 helper。
func (e *charxEnv) attentionIDs(taskID string) (map[string]bool, map[string]bool) {
	att, _ := e.m.Attention(taskID)
	perms, quests := map[string]bool{}, map[string]bool{}
	for _, p := range att.Permissions {
		perms[p.ID] = true
	}
	for _, q := range att.Questions {
		quests[q.ID] = true
	}
	return perms, quests
}

// charxAttentionMatches 断言 pending 集合精确等于 wantPerms/wantQuests。
func (e *charxEnv) charxAssertAttentionSet(taskID string, wantPerms, wantQuests []string) {
	e.t.Helper()
	charxWaitUntil(e.t, charxTimeout, fmt.Sprintf("attention set == perms%v quests%v", wantPerms, wantQuests), func() bool {
		perms, quests := e.attentionIDs(taskID)
		if len(perms) != len(wantPerms) || len(quests) != len(wantQuests) {
			return false
		}
		for _, id := range wantPerms {
			if !perms[id] {
				return false
			}
		}
		for _, id := range wantQuests {
			if !quests[id] {
				return false
			}
		}
		return true
	})
}

// TestCharx_AttentionSixFamilies attention 六家族（asked×4 + 了结×6，v1/v2 别名全覆盖）
// + 缺字段畸形事件静默忽略。按事件语义分段断言（见文件头注释），终态做六观察面
// 完整快照比较。
func TestCharx_AttentionSixFamilies(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{})
	charxSeedRepoWorktreeTask(e.store, "t1", "p1")
	if err := e.charxActivate("t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	anchor := e.charxTaskRow("t1").AnchorSessionID.String
	e.srv.waitSubscribers(t, 1)

	// --- 阶段一：asked 家族建立完整 pending 集合（perm-1 带 metadata 完整对象）。 ---
	asked := []opencode.Event{
		charxPermAskedMeta("permission.asked", "perm-1", anchor, "bash", []string{"rm -rf /x"},
			map[string]interface{}{"path": "a.go", "line": 3}),
		charxPermAsked("permission.v2.asked", "perm-2", anchor, "edit", []string{"a.txt", "b.txt"}, ""),
		charxQuestAsked("question.asked", "q-1", anchor, "方案", "选哪个？", false),
		charxQuestAsked("question.v2.asked", "q-2", anchor, "确认", "继续吗？", false),
		charxQuestAsked("question.asked", "q-3", anchor, "范围", "全部删除？", false),
		charxQuestAsked("question.v2.asked", "q-4", anchor, "回滚", "执行回滚？", false),
	}
	for _, ev := range asked {
		e.srv.broadcastEvent(t, ev)
	}
	// 畸形 asked：缺 permission / 缺 id / 空 questions / question 缺 id——全部静默忽略。
	for _, ev := range []opencode.Event{
		charxPermAsked("permission.asked", "perm-bad-1", anchor, "", []string{"x"}, "permission"),
		charxPermAsked("permission.asked", "", anchor, "bash", []string{"x"}, "id"),
		charxQuestAsked("question.asked", "q-bad-1", anchor, "h", "q", true),
		charxQuestAsked("question.asked", "", anchor, "h", "q", false),
	} {
		e.srv.broadcastEvent(t, ev)
	}
	e.charxAssertAttentionSet("t1", []string{"perm-1", "perm-2"}, []string{"q-1", "q-2", "q-3", "q-4"})

	// 实际 DTO 全量断言（A1：字段名/独立字段/metadata 完整对象保留，Since 归一化）。
	att, _ := e.m.Attention("t1")
	if got := charxAttentionJSON(t, att); got != strings.Join([]string{
		`{"Permissions":[`,
		`{"Since":"<TS>","id":"perm-1","metadata":{"line":3,"path":"a.go"},"patterns":["rm -rf /x"],"permission":"bash","sessionID":"` + anchor + `"},`,
		`{"Since":"<TS>","id":"perm-2","patterns":["a.txt","b.txt"],"permission":"edit","sessionID":"` + anchor + `"}`,
		`],"Questions":[`,
		`{"Since":"<TS>","id":"q-1","questions":[{"header":"方案","question":"选哪个？"}],"sessionID":"` + anchor + `"},`,
		`{"Since":"<TS>","id":"q-2","questions":[{"header":"确认","question":"继续吗？"}],"sessionID":"` + anchor + `"},`,
		`{"Since":"<TS>","id":"q-3","questions":[{"header":"范围","question":"全部删除？"}],"sessionID":"` + anchor + `"},`,
		`{"Since":"<TS>","id":"q-4","questions":[{"header":"回滚","question":"执行回滚？"}],"sessionID":"` + anchor + `"}`,
		`]}`,
	}, "") {
		t.Errorf("阶段一 attention DTO =\n %s\nwant 完整集合（含 metadata 原对象）", got)
	}

	// --- 阶段二：畸形 reply 针对 still-pending；有效 marker reply 作流处理屏障。 ---
	e.srv.broadcastEvent(t, opencode.Event{Type: "question.replied", Properties: map[string]interface{}{
		"sessionID": anchor, // 缺 requestID：畸形，静默忽略，q-1 必须保留
	}})
	e.srv.broadcastEvent(t, opencode.Event{Type: "permission.replied", Properties: map[string]interface{}{
		"requestID": "perm-2", // 缺 sessionID：畸形，静默忽略，perm-2 必须保留
	}})
	e.srv.broadcastEvent(t, opencode.Event{Type: "question.v2.rejected", Properties: map[string]interface{}{
		"sessionID": anchor, // 缺 requestID：畸形，静默忽略
	}})
	// marker：有效 permission.replied 关闭 perm-1（其落库后，前面的畸形 reply 必然已处理）。
	e.srv.broadcastEvent(t, charxPermReplied("permission.replied", anchor, "perm-1"))
	e.charxAssertAttentionSet("t1", []string{"perm-2"}, []string{"q-1", "q-2", "q-3", "q-4"})

	// --- 阶段三：六了结家族各关闭一个 pending（每个家族唯一负责人，停用即残留）。 ---
	e.srv.broadcastEvent(t, charxPermReplied("permission.v2.replied", anchor, "perm-2"))
	e.srv.broadcastEvent(t, charxQuestReplied("question.replied", anchor, "q-1"))
	e.srv.broadcastEvent(t, charxQuestReplied("question.v2.replied", anchor, "q-2"))
	e.srv.broadcastEvent(t, charxQuestReplied("question.rejected", anchor, "q-3"))
	e.srv.broadcastEvent(t, charxQuestReplied("question.v2.rejected", anchor, "q-4"))
	e.charxAssertAttentionSet("t1", nil, nil)

	// 终态六观察面完整快照（A2：默认全面比较——六面 golden 全部落字面量）。
	got := e.captureCharx("attention", "t1")
	assertCharxMatches(t, "attention", charxSnapshot{
		tasks: []string{fmt.Sprintf(
			"t1|name=task t1|active|worktree|anchor=%s|last_port=PT1|delete_mode=-|last_error=|notices=[]|"+
				"env={OCDECK_PROJECT_PATH=/repo-p1,OCDECK_SERVE_PORT=<PORT>,OCDECK_TASK_BASE_BRANCH=main,"+
				"OCDECK_TASK_HEAD_BRANCH=ocdeck/t1,OCDECK_TASK_ID=t1,OCDECK_TASK_NAME=task t1,"+
				"OCDECK_TASK_PATH=/data/worktrees/p1/t1}", anchor)},
		sessions: []string{
			fmt.Sprintf("t1|%s|created=1001|first_seen=<TS>|last_seen=1001|parent=-", anchor),
		},
		owner: []string{"t1 -> [" + anchor + "]"},
		proc: []string{
			"new ocdeck-t1-runtime cwd=/data/worktrees/p1/t1 auth=derived/pw(32) argv=opencode --port PT1 --hostname 127.0.0.1",
			"kill ocdeck-t1-runtime",
			fmt.Sprintf("new ocdeck-t1-runtime cwd=/data/worktrees/p1/t1 auth=derived/pw(32) argv=opencode --port PT1 --hostname 127.0.0.1 --session %s", anchor),
		},
		notices: nil,
		dto:     []string{`t1|attention={"Permissions":[],"Questions":[]}|agentStatus=idle`},
	}, got)
}

// --- 1.2 permission reply（A8：真实 client 契约覆盖） ---

// TestCharx_ReplyPermission_ProductionPath 经生产路径驱动一条真实 reply：
// ai-auto 任务 + approve judge → SSE permission.asked → judgeScan/judgeAndReply →
// taskOcClient（真实 *opencode.Client）POST /permission/{requestID}/reply。
// 断言精确匹配真实端点契约（permission_reply.go:52）：路径 requestID、显式
// directory、basic 用户名 opencode、body {"reply":"once"}、replyCalls 记录；
// 响应为 JSON true（client 解码成功 → ReplyPermission 返回 nil）。
func TestCharx_ReplyPermission_ProductionPath(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{})
	charxSeedRepoWorktreeTask(e.store, "t1", "p1")
	e.store.mutTask("t1", func(tr *TaskRow) { tr.PermissionMode = PermissionModeAIAuto })
	judge := newFakeJudge(PermissionVerdictApprove)
	e.m.judge = judge
	if err := e.charxActivate("t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	anchor := e.charxTaskRow("t1").AnchorSessionID.String
	e.srv.waitSubscribers(t, 1)

	e.srv.broadcastEvent(t, charxPermAsked("permission.asked", "perm-r1", anchor, "bash", []string{"x"}, ""))
	charxWaitUntil(t, charxTimeout, "ai-auto judge replied once", func() bool {
		return len(e.srv.replyCalls()) == 1
	})

	// 判定链路确实经过 judge（approve → once）。
	if calls := judge.judgeCalls(); len(calls) != 1 {
		t.Fatalf("judge calls = %d, want 1", len(calls))
	}
	// reply 记录：kind|requestID|body 精确匹配。
	if calls := e.srv.replyCalls(); !reflect.DeepEqual(calls, []string{`perm|perm-r1|{"reply":"once"}`}) {
		t.Errorf("replyCalls = %q, want [perm|perm-r1|{\"reply\":\"once\"}]", calls)
	}
	// 请求记录：路径 + 显式 directory + 认证形状。
	wantReq := "POST /permission/perm-r1/reply dir=/data/worktrees/p1/t1 user=opencode"
	found := false
	for _, req := range e.srv.requests() {
		if req == wantReq {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("请求记录缺少 %q，got %q", wantReq, e.srv.requests())
	}
}

// --- 1.2 diffreview 能力探测 ---

// TestCharx_DiffreviewCapability_ProbeStates GET /doc 三态：路径+operationId 匹配
// → supported；路径缺失/operationId 不符 → unsupported（fail-closed）；404/5xx/
// 畸形 → unknown。经 RuntimePortAdapter.ProbeCapability（生产 diffreview 调度消费口）。
func TestCharx_DiffreviewCapability_ProbeStates(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{})
	charxSeedRepoWorktreeTask(e.store, "t1", "p1")
	charxSeedRepoWorktreeTask(e.store, "t2", "p2")
	charxSeedRepoWorktreeTask(e.store, "t3", "p3")
	for _, id := range []string{"t1", "t2", "t3"} {
		if err := e.charxActivate(id); err != nil {
			t.Fatalf("Activate %s: %v", id, err)
		}
	}
	adapter := NewRuntimePortAdapter(e.m)
	ctx := context.Background()

	// 默认 /doc 含 prompt_async → supported（稳定缓存）。
	if st, err := adapter.ProbeCapability(ctx, "t1"); err != nil || st != diffreview.CapabilitySupported {
		t.Errorf("t1 capability = %v, err=%v; want supported", st, err)
	}
	// 500 → unknown（非稳定态，不缓存）。
	e.srv.mu.Lock()
	e.srv.docMode = fakeDocBroken
	e.srv.mu.Unlock()
	if st, err := adapter.ProbeCapability(ctx, "t2"); err != nil || st != diffreview.CapabilityUnknown {
		t.Errorf("t2 capability = %v, err=%v; want unknown (broken /doc)", st, err)
	}
	// 200 但路径缺失 → unsupported（fail-closed 稳定缓存）。
	e.srv.mu.Lock()
	e.srv.docMode = fakeDocNoPromptAsync
	e.srv.mu.Unlock()
	if st, err := adapter.ProbeCapability(ctx, "t3"); err != nil || st != diffreview.CapabilityUnsupported {
		t.Errorf("t3 capability = %v, err=%v; want unsupported (path missing)", st, err)
	}
	// 无运行时 → absent（不探测）。
	if st, err := adapter.ProbeCapability(ctx, "t4"); err != nil || st != diffreview.CapabilityAbsent {
		t.Errorf("t4 capability = %v, err=%v; want absent (no runtime)", st, err)
	}
}

// --- 1.2 标题同步三态 ---

// TestCharx_TitleSync_ThreeStates 成功 PATCH / 404 降级缓存（同一 runtime 的第二次
// 改名不再发 PATCH）/ 5xx 失败不缓存（重试再发）/ 网络失败不缓存（连接中断，非
// HTTP 状态码；重试再发）。notice code 固定 task_title_sync_failed。
// 404 缓存绑定 instVersion，故四态分别用独立任务隔离。
func TestCharx_TitleSync_ThreeStates(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{})
	charxSeedRepoWorktreeTask(e.store, "t1", "p1")
	charxSeedRepoWorktreeTask(e.store, "t2", "p2")
	charxSeedRepoWorktreeTask(e.store, "t3", "p3")
	charxSeedRepoWorktreeTask(e.store, "t4", "p4")
	for _, id := range []string{"t1", "t2", "t3", "t4"} {
		if err := e.charxActivate(id); err != nil {
			t.Fatalf("Activate %s: %v", id, err)
		}
	}
	anchorOf := func(taskID string) string {
		t.Helper()
		return e.charxTaskRow(taskID).AnchorSessionID.String
	}
	patchCountFor := func(anchor string) int {
		t.Helper()
		n := 0
		for _, c := range e.srv.patchCalls() {
			if strings.HasPrefix(c, anchor+"|") {
				n++
			}
		}
		return n
	}
	rename := func(taskID, name string) {
		t.Helper()
		if _, err := e.m.UpdateTaskInfo(context.Background(), taskID, UpdateTaskInfoOptions{Name: &name}); err != nil {
			t.Fatalf("UpdateTaskInfo(%s, %q): %v", taskID, name, err)
		}
	}
	waitNotice := func(taskID, substr string) {
		t.Helper()
		charxWaitUntil(t, charxTimeout, "title sync notice on "+taskID, func() bool {
			for _, ne := range e.charxNoticesOf(taskID) {
				if ne.Code == noticeCodeTitleSync && strings.Contains(ne.Message, substr) {
					return true
				}
			}
			return false
		})
	}

	// ① 成功：标题写入上游，无 notice。
	anchor1 := anchorOf("t1")
	rename("t1", "renamed-ok")
	charxWaitUntil(t, charxTimeout, "title synced to upstream", func() bool {
		sess, ok := e.srv.sessionByID(anchor1)
		return ok && sess.Title == "renamed-ok"
	})
	if entries := e.charxNoticesOf("t1"); len(entries) != 0 {
		t.Errorf("t1 notices after successful sync = %+v, want none", entries)
	}
	if n := patchCountFor(anchor1); n != 1 {
		t.Errorf("t1 PATCH calls = %d, want 1", n)
	}

	// ② 404：能力降级 notice + instVersion 缓存——同任务再次改名不再发 PATCH；
	// notice 保持单条（按任务去重）。
	e.srv.mu.Lock()
	e.srv.titleStatus = 404
	e.srv.mu.Unlock()
	anchor2 := anchorOf("t2")
	rename("t2", "renamed-404a")
	waitNotice("t2", "unsupported")
	if n := patchCountFor(anchor2); n != 1 {
		t.Errorf("t2 PATCH calls after 404 = %d, want 1", n)
	}
	rename("t2", "renamed-404b")
	charxWaitUntil(t, charxTimeout, "t2 name committed", func() bool {
		return e.charxTaskRow("t2").Name == "renamed-404b"
	})
	if n := patchCountFor(anchor2); n != 1 {
		t.Errorf("t2 PATCH calls after cached unsupported = %d, want 1 (缓存不再实测)", n)
	}
	titleNotices := 0
	for _, ne := range e.charxNoticesOf("t2") {
		if ne.Code == noticeCodeTitleSync {
			titleNotices++
			if ne.Message != "session title update unsupported" {
				t.Errorf("404 notice message = %q", ne.Message)
			}
		}
	}
	if titleNotices != 1 {
		t.Errorf("t2 title sync notices = %d, want 1 (dedup)", titleNotices)
	}

	// ③ 5xx：失败 notice，不缓存——同任务下次改名重发 PATCH。
	e.srv.mu.Lock()
	e.srv.titleStatus = 500
	e.srv.mu.Unlock()
	anchor3 := anchorOf("t3")
	rename("t3", "renamed-500a")
	waitNotice("t3", "500")
	rename("t3", "renamed-500b")
	charxWaitUntil(t, charxTimeout, "uncached 5xx retries PATCH", func() bool {
		return patchCountFor(anchor3) == 2
	})

	// ④ 网络失败（A5：非仅 HTTP 500）：连接在响应前中断 → 传输层错误 notice
	//（消息不含 "unsupported"、不含 "http"）；不缓存——下次改名重发 PATCH。
	e.srv.mu.Lock()
	e.srv.titleStatus = 0
	e.srv.titleNetFail = true
	e.srv.mu.Unlock()
	anchor4 := anchorOf("t4")
	rename("t4", "renamed-neta")
	charxWaitUntil(t, charxTimeout, "network failure notice on t4", func() bool {
		for _, ne := range e.charxNoticesOf("t4") {
			if ne.Code == noticeCodeTitleSync && ne.Message != "" &&
				!strings.Contains(ne.Message, "unsupported") && !strings.Contains(ne.Message, "http ") {
				return true
			}
		}
		return false
	})
	if n := patchCountFor(anchor4); n != 1 {
		t.Errorf("t4 PATCH calls after net fail = %d, want 1", n)
	}
	rename("t4", "renamed-netb")
	charxWaitUntil(t, charxTimeout, "uncached network failure retries PATCH", func() bool {
		return patchCountFor(anchor4) == 2
	})
}
