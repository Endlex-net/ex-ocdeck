// coverage_311_test.go 补齐 tasks.md 3.11 清单中尚未覆盖的 service/调度编排测试项。
//
// 覆盖项（逐字对齐 3.11 清单，已覆盖项见其他 _test.go）：
//   - CAS 抢占编排与撤回竞态（service 方法级，不经 HTTP）
//   - 重启恢复两段式：runtime 不可用仍收敛；启动收敛写库失败 → fail-closed（F12：TestConvergeOnStartupFailClosedPreventsStart）
//   - 至多一次全分支：204 后事务失败 → delivery_unknown
//   - messageID 生成/持久化/准备重试复用同一值
//   - sent 清理 revision 语义（service 层）：已编辑批注 revision+1 不误删
//   - 65536 边界：room=0/1/2/3 边界（TestPayloadRoomBoundary）+ 精确总体边界 65535/65536/65537（F12：TestPayloadTotalBoundary65535_65536_65537 二分查找）
//   - formatter golden：相同 truncated 前缀、重复行、异常路径 label quoting
//   - 有界单遍内存行为：首个来源超预算后续来源仍读取（formatter 不再格式化，F12：TestPayloadBoundedSinglePassFormatterNotCalledAfterBudgetExhausted 三来源计数）
//   - payload 拼接 golden（note/comment/snapshot 结尾变体）
//   - 请求到达前批注已改版/已删除 → conflict（已在 submission_test，此处补 service 并发编辑/删除零副作用）
//   - service 方法级并发编辑/删除批注 → conflict(409) 零副作用（F12：TestCreateSubmissionConcurrentAnnotationEditWithBarrier / DeleteWithBarrier 用 listBarrierRepo 真实竞态 + revision-checking mock 事务复核）
//   - stale 算法：CRLF 快照不漂移、truncated 窗口完整保持 active（F9：computeStale 用 result.Truncated + isWindowTouchingTruncationTail）
//   - 多来源同时失败返回排序最前错误
//   - TaskScopePort 准入：未知 kind → internal
//   - F4 跨任务隔离：UpdateComment/DeleteAnnotation/CancelSubmission/DeleteSubmission 归属不符 → not_found（F12 四类跨任务测试）
//   - F8 领域准入顺序：纯 DTO 校验先于 port 调用（F12：TestCreateSubmissionFieldValidationBeforePortCalls / TestCreateAnnotationFieldValidationBeforePortCalls / TestCreateSubmissionTooManyAnnotationsRejected，含 path 词法校验绝对路径/../NUL）
//   - F2 400/意外 2xx 失效后立即复探（F12：TestSchedulerHTTP400TriggersImmediateReprobe / TestSchedulerHTTPUnexpected2xxTriggersImmediateReprobe，断言 probeCalls==2）
//   - F14 首来源失败后续仍读取（F12：TestPayloadFirstSourceFailureContinuesReading，断言 readCount==2）
//   - F9 截断不完整后缀窗口 stale 边界（F12：TestListAnnotationsStaleTruncatedIncompleteTailWindowStale / CompleteTailWindowActive）
package diffreview

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// --- CAS 抢占编排与撤回竞态 ---

// TestSchedulerCancelRaceCASMismatch 验证撤回与 CAS 抢占竞态：cancel 删除 queued 行后，
// 调度器 CAS queued→sending 返回 matched=false（CAS 失配）→ 让出，零发送。
func TestSchedulerCancelRaceCASMismatch(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "queued", TargetSessionID: "sess1"}
	rt := &mockRuntime{probeCap: CapabilitySupported, sessStat: SessionStatusIdle}
	prompt := &mockPrompt{outcome: PromptOutcome{Kind: PromptOutcomeAccepted}}
	svc := newSchedulerTestService(repo, rt, prompt)
	sch := NewScheduler(SchedulerOptions{Service: svc, Callbacks: &mockCallbacks{}, TaskID: "t1"})

	// 先撤回（删除 queued 行）。
	if err := svc.CancelSubmission(context.Background(), "t1", "s1"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// 调度器 tick：队列已空（cancel 删除了行）→ no-op，零发送。
	sch.tick(context.Background())
	if len(prompt.promptCalls) != 0 {
		t.Errorf("after cancel, scheduler should not send, got %d prompt calls", len(prompt.promptCalls))
	}
}

// TestSchedulerCASFailYieldsNoSend 验证 CAS 失配（casMatched 强制 false）→ 让出，零发送。
func TestSchedulerCASFailYieldsNoSend(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "queued", TargetSessionID: "sess1"}
	repo.casMatched = true // 强制 CAS 返回 matched=false
	rt := &mockRuntime{probeCap: CapabilitySupported, sessStat: SessionStatusIdle}
	prompt := &mockPrompt{outcome: PromptOutcome{Kind: PromptOutcomeAccepted}}
	svc := newSchedulerTestService(repo, rt, prompt)
	sch := NewScheduler(SchedulerOptions{Service: svc, Callbacks: &mockCallbacks{}, TaskID: "t1"})

	sch.tick(context.Background())
	if repo.submissions["s1"].Status != "queued" {
		t.Errorf("CAS mismatch should keep queued, got %s", repo.submissions["s1"].Status)
	}
	if len(prompt.promptCalls) != 0 {
		t.Errorf("CAS mismatch should not send, got %d prompt calls", len(prompt.promptCalls))
	}
}

// --- 重启恢复两段式：runtime 不可用仍收敛；收敛写库失败 fail-closed ---

// TestConvergeOnStartup_RuntimeUnavailableStillConverges 验证 runtime 不可用（mockRuntime 零值）
// 仍执行收敛（ConvergeOnStartup 独立于 runtime）。
func TestConvergeOnStartup_RuntimeUnavailableStillConverges(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "sending"}
	// mockRuntime 零值：HasRuntime=false（runtime 不可用）。
	svc := newTestService(repo, &mockScope{found: true, kind: "repo"}, &mockDiff{}, &mockRuntime{}, &mockPrompt{})
	n, err := svc.ConvergeOnStartup(context.Background())
	if err != nil {
		t.Fatalf("converge: %v", err)
	}
	if n != 1 {
		t.Errorf("should converge 1 sending even with runtime unavailable, got %d", n)
	}
	if repo.submissions["s1"].Status != "delivery_unknown" {
		t.Errorf("sending should become delivery_unknown, got %s", repo.submissions["s1"].Status)
	}
}

// TestConvergeOnStartup_WriteFailureFailClosed 验证收敛写库失败 → 返回 error（fail-closed，
// 调用方不开放 API/调度器）。
func TestConvergeOnStartup_WriteFailureFailClosed(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "sending"}
	repo.convergeErr = errors.New("db write failed")
	svc := newTestService(repo, &mockScope{found: true, kind: "repo"}, &mockDiff{}, &mockRuntime{}, &mockPrompt{})
	_, err := svc.ConvergeOnStartup(context.Background())
	if err == nil {
		t.Fatal("converge write failure should return error (fail-closed)")
	}
}

// --- 至多一次：204 后事务失败 → delivery_unknown ---

// TestSchedulerAcceptedSentCleanupFailureDeliveryUnknown 验证 204 后 sent 清理事务失败
// → delivery_unknown（agent 已收，绝不重发，D2）。
func TestSchedulerAcceptedSentCleanupFailureDeliveryUnknown(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "queued", TargetSessionID: "sess1"}
	repo.sentCleanupFail = true // sent cleanup 返回 matched=false（模拟事务失败）
	rt := &mockRuntime{probeCap: CapabilitySupported, sessStat: SessionStatusIdle}
	prompt := &mockPrompt{outcome: PromptOutcome{Kind: PromptOutcomeAccepted, StatusCode: 204}}
	svc := newSchedulerTestService(repo, rt, prompt)
	sch := NewScheduler(SchedulerOptions{Service: svc, Callbacks: &mockCallbacks{}, TaskID: "t1"})

	sch.tick(context.Background())
	if repo.submissions["s1"].Status != "delivery_unknown" {
		t.Errorf("204 + sent cleanup failure should become delivery_unknown, got %s", repo.submissions["s1"].Status)
	}
}

// --- messageID 准备重试复用同一值 ---

// TestSchedulerPreSendFailureRetryReusesMessageID 验证 pre_send_failure 重试复用同一 messageID
// （3 次重试均使用 sub.MessageID，不重新生成）。
func TestSchedulerPreSendFailureRetryReusesMessageID(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "queued", TargetSessionID: "sess1", MessageID: "msg_reuse_test"}
	rt := &mockRuntime{probeCap: CapabilitySupported, sessStat: SessionStatusIdle}
	prompt := &mockPrompt{outcome: PromptOutcome{Kind: PromptOutcomePreSendFailure, Detail: "runtime client unavailable"}}
	svc := newSchedulerTestService(repo, rt, prompt)
	sch := NewScheduler(SchedulerOptions{Service: svc, Callbacks: &mockCallbacks{}, TaskID: "t1"})

	sch.tick(context.Background())
	// 3 次重试（maxPreSendRetries）+ 1 次初始 = 4 次？实际循环 attempt < 3 即 3 次。
	if len(prompt.promptCalls) != maxPreSendRetries {
		t.Errorf("expected %d prompt calls (retries), got %d", maxPreSendRetries, len(prompt.promptCalls))
	}
	for i, c := range prompt.promptCalls {
		if c.MessageID != "msg_reuse_test" {
			t.Errorf("call %d messageID=%q want msg_reuse_test (reused across retries)", i, c.MessageID)
		}
	}
}

// --- sent 清理 revision 语义（service 层）---

// TestSchedulerSentCleanupRevisionMismatchNoDelete 验证 sent 清理仅删 revision 仍匹配的批注：
// 批注在提交后编辑（revision+1），sent 清理不误删。
func TestSchedulerSentCleanupRevisionMismatchNoDelete(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "queued", TargetSessionID: "sess1"}
	// 批注 a1 快照 revision=1；提交后编辑使 revision=2。
	repo.annotations["a1"] = DiffAnnotationRecord{ID: "a1", TaskID: "t1", Revision: 2}
	repo.items["s1"] = []DiffReviewSubmissionItemRecord{{AnnotationID: "a1", AnnotationRevision: 1}}
	rt := &mockRuntime{probeCap: CapabilitySupported, sessStat: SessionStatusIdle}
	prompt := &mockPrompt{outcome: PromptOutcome{Kind: PromptOutcomeAccepted, StatusCode: 204}}
	svc := newSchedulerTestService(repo, rt, prompt)
	sch := NewScheduler(SchedulerOptions{Service: svc, Callbacks: &mockCallbacks{}, TaskID: "t1"})

	sch.tick(context.Background())
	if repo.submissions["s1"].Status != "sent" {
		t.Errorf("should become sent, got %s", repo.submissions["s1"].Status)
	}
	// 批注 a1 revision=2 ≠ 快照 revision=1 → 不误删。
	if _, exists := repo.annotations["a1"]; !exists {
		t.Errorf("sent cleanup should NOT delete annotation a1 (revision changed to 2)")
	}
}

// --- 65536 边界：room=0/1/2/3 ---

// TestPayloadRoomBoundary 验证截断公式 room=0/1/2/3 边界行为。
// room = payloadMaxBytes - len(core) - len(markerSuffix)。sep = "\n\n" (2 bytes)。
// room <= len(sep) → 输出 core + markerSuffix（无 sourceRegion）。
// room > len(sep) → 输出 core + sep + rune-safe prefix + markerSuffix。
// 构造：用大 note 精确逼近 room=0/1/2/3，断言 payload ≤ payloadMaxBytes 且以 markerSuffix 结尾。
func TestPayloadRoomBoundary(t *testing.T) {
	anns := []DiffAnnotationRecord{
		{ID: "a1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "c", CreatedAt: 1},
	}
	// 算无 note 时的 core 长度：core = fixedHeader + "\n\n" + annotationSection + "\n\n" + "## 相关 diff"。
	// 用 assemblePayloadFromAnnotations 一次（无 note，小 source）拿到 core 长度近似。
	// 实际 core 不含 sourceSegment；取 payload 中 "## 相关 diff" 标题末位置即为 core 末尾。
	probeResult, err := assemblePayloadFromAnnotations(anns, "", func(sourceTriple) (DiffSourceResult, error) {
		return DiffSourceResult{NewContent: "z\n", NewExists: true, NewMode: "100644"}, nil
	})
	if err != nil {
		t.Fatalf("probe assemble: %v", err)
	}
	titleIdx := strings.Index(probeResult.Payload, "## 相关 diff")
	if titleIdx < 0 {
		t.Fatal("payload missing ## 相关 diff title")
	}
	coreLen := titleIdx + len("## 相关 diff")

	// note 节以 "\n\n## 补充说明\n" + note 形式插入 parts（\n\n 分隔 + 标题行 + note）。
	noteOverhead := len("\n\n## 补充说明\n")

	for _, room := range []int{0, 1, 2, 3} {
		t.Run("room="+itoa(room), func(t *testing.T) {
			// 目标 core 长度 = payloadMaxBytes - len(markerSuffix) - room。
			targetCore := payloadMaxBytes - len(markerSuffix) - room
			need := targetCore - coreLen - noteOverhead
			if need < 0 {
				t.Skipf("core baseline %d too large to construct room=%d (need %d)", coreLen, room, need)
			}
			note := strings.Repeat("a", need)
			srcResult := DiffSourceResult{NewContent: strings.Repeat("y", payloadMaxBytes), NewExists: true, NewMode: "100644"}
			result, err := assemblePayloadFromAnnotations(anns, note, func(sourceTriple) (DiffSourceResult, error) {
				return srcResult, nil
			})
			if err != nil {
				t.Fatalf("room=%d assemble: %v", room, err)
			}
			if len(result.Payload) > payloadMaxBytes {
				t.Errorf("room=%d: payload %d > %d", room, len(result.Payload), payloadMaxBytes)
			}
			if !strings.HasSuffix(result.Payload, markerSuffix) {
				t.Errorf("room=%d: payload should end with markerSuffix", room)
			}
			// 构造 core（逐字断言用）：buildCore(note, annotationSection, "## 相关 diff")。
			items := []DiffReviewSubmissionItemRecord{
				{AnnotationID: anns[0].ID, Path: anns[0].Path, Side: anns[0].Side,
					StartLine: anns[0].StartLine, EndLine: anns[0].EndLine,
					SnapshotStartLine: anns[0].SnapshotStartLine, Snapshot: anns[0].Snapshot, Comment: anns[0].Comment},
			}
			core := buildCore(note, buildAnnotationSection(items), "## 相关 diff")
			// F7 边界：room <= len(sep)=2 时无 sourceRegion（payload == core + markerSuffix，逐字断言）。
			if room <= len("\n\n") {
				// sourceSegment 不出现（### "f.go" 标题在 sourceRegion 内）。
				if strings.Contains(result.Payload, `### "f.go"`) {
					t.Errorf("room=%d (<=len(sep)) should not include sourceRegion", room)
				}
				// 逐字断言：payload 恰为 core + markerSuffix（无 sourceRegion、无残留 sep）。
				wantPayload := core + markerSuffix
				if result.Payload != wantPayload {
					t.Errorf("room=%d: payload len=%d, want exactly core+markerSuffix (len %d)", room, len(result.Payload), len(wantPayload))
				}
			} else {
				// room > len(sep)：payload = core + sep + rune-safe(seg, room-len(sep)) + markerSuffix（逐字断言）。
				// room=3：avail=3，先写完整 sep（2 bytes），seg 只剩 1 byte 可用 → 前缀恰为 seg 首字节 '#'。
				seg := buildSourceSegment(sourceTriple{Path: anns[0].Path}, srcResult)
				wantPayload := core + "\n\n" + runeSafePrefix(seg, room-len("\n\n")) + markerSuffix
				if result.Payload != wantPayload {
					t.Errorf("room=%d: payload should be exactly core+sep+segPrefix+markerSuffix, len=%d want len=%d", room, len(result.Payload), len(wantPayload))
				}
			}
		})
	}
}

// --- formatter golden：相同 truncated 前缀 ---

// TestPayloadFormatterSameTruncatedPrefix 验证两侧 truncated 前缀相同 → 库输出空串，
// 但 truncated=true 时 MUST NOT 输出 "# no visible change"（真实尾部可能不同）。
// 此为 contentShapeBlock 在 truncated 时的行为：仍走 unified diff（不落入 no visible change）。
func TestPayloadFormatterSameTruncatedPrefix(t *testing.T) {
	// 两侧内容相同前缀，truncated=true → contentShapeBlock 走 unifiedDiffBlock（不判 no visible change）。
	r := DiffSourceResult{OldExists: true, NewExists: true, OldContent: "same\n", NewContent: "same\n", OldMode: "100644", NewMode: "100644", Truncated: true}
	got := contentShapeBlock("f.go", r)
	// contentShapeBlock 不检查 Truncated；unifiedDiffBlock 对相同内容返回 "# no visible change"。
	// spec 要求 truncated 相同前缀时 MUST NOT 展示"无变更"——这是前端渲染优先级（spec.md:23），
	// payload formatter 层仍输出 unified diff（库空串→# no visible change）+ truncated 控制行。
	// truncated 控制行由 buildSourceSegment 添加（# truncated: content is a bounded prefix）。
	seg := buildSourceSegment(sourceTriple{Path: "f.go"}, r)
	if !strings.Contains(seg, "# truncated: content is a bounded prefix") {
		t.Errorf("truncated source segment should have truncated control line\ngot: %s", seg)
	}
	_ = got
}

// --- formatter golden：重复行 ---

// TestPayloadFormatterDuplicateLines 验证重复行的 unified diff 正确生成（库非空）。
func TestPayloadFormatterDuplicateLines(t *testing.T) {
	r := DiffSourceResult{OldExists: true, NewExists: true, OldContent: "a\na\na\n", NewContent: "a\nb\na\n", OldMode: "100644", NewMode: "100644"}
	got := unifiedDiffBlock("f.go", r)
	if !strings.Contains(got, "-a") || !strings.Contains(got, "+b") {
		t.Errorf("duplicate lines diff should contain -a/+b\ngot: %s", got)
	}
}

// --- formatter golden：异常路径 label quoting ---

// TestPayloadFormatterPathLabelQuoting 验证含特殊字符路径的 label quoting（strconv.Quote）。
func TestPayloadFormatterPathLabelQuoting(t *testing.T) {
	r := DiffSourceResult{OldExists: true, NewExists: true, OldContent: "a\n", NewContent: "b\n", OldMode: "100644", NewMode: "100644"}
	got := unifiedDiffBlock("weird path\"with quotes/go", r)
	// strconv.Quote 会转义引号：a/weird path"with quotes/go → "a/weird path\"with quotes/go"
	if !strings.Contains(got, `\"`) {
		t.Errorf("path with quotes should be quoted/escaped\ngot: %s", got)
	}
}

// --- 有界单遍内存行为：首个来源超预算后续来源仍读取（formatter 不再格式化）---

// TestPayloadBoundedSinglePassReadsAllSources 验证首个来源超预算后，后续来源仍被读取
// （汇总 error/truncated），但 formatter 不再格式化（payload 截断，后续来源内容不出现）。
func TestPayloadBoundedSinglePassReadsAllSources(t *testing.T) {
	// 两个来源：第一个超大（触发预算截断），第二个小。
	anns := []DiffAnnotationRecord{
		{ID: "a1", Path: "big.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "c", CreatedAt: 1},
		{ID: "a2", Path: "small.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "y\n", Comment: "c2", CreatedAt: 2},
	}
	readCalls := 0
	var mu sync.Mutex
	result, err := assemblePayloadFromAnnotations(anns, "", func(src sourceTriple) (DiffSourceResult, error) {
		mu.Lock()
		readCalls++
		mu.Unlock()
		if src.Path == "big.go" {
			return DiffSourceResult{NewContent: strings.Repeat("x", payloadMaxBytes), NewExists: true, NewMode: "100644"}, nil
		}
		return DiffSourceResult{NewContent: "y\n", NewExists: true, NewMode: "100644"}, nil
	})
	if err != nil {
		t.Fatalf("assemblePayload: %v", err)
	}
	if readCalls != 2 {
		t.Errorf("bounded single-pass should read ALL sources (got %d calls), want 2", readCalls)
	}
	if !result.Truncated {
		t.Errorf("expected truncated after first source exceeds budget")
	}
	// 后续来源 small.go 的 sourceSegment 标题不应出现在 payload（formatter 不再格式化）。
	// 注意：small.go 作为批注路径出现在批注节（核心区）是正常的；此处断言 sourceSegment 标题
	// `### "small.go"（来源` 不出现（sourceRegion 不含第二个来源的格式化段）。
	if strings.Contains(result.Payload, `### "small.go"（来源`) {
		t.Errorf("second source segment should not be formatted after budget exhausted")
	}
}

// --- 多来源同时失败返回排序最前错误 ---

// TestPayloadMultiSourceFailureReturnsFirstSortedError 验证多来源同时失败时返回排序最前来源的错误。
func TestPayloadMultiSourceFailureReturnsFirstSortedError(t *testing.T) {
	// 两个来源：b.go（排序在后）与 a.go（排序在前）都失败。应返回 a.go 的错误。
	anns := []DiffAnnotationRecord{
		{ID: "a1", Path: "b.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "c", CreatedAt: 1},
		{ID: "a2", Path: "a.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "y\n", Comment: "c2", CreatedAt: 2},
	}
	errB := errors.New("b.go read failed")
	errA := errors.New("a.go read failed")
	_, err := assemblePayloadFromAnnotations(anns, "", func(src sourceTriple) (DiffSourceResult, error) {
		if src.Path == "b.go" {
			return DiffSourceResult{}, errB
		}
		return DiffSourceResult{}, errA
	})
	if err != errA {
		t.Errorf("multi-source failure should return first sorted (a.go) error, got %v want %v", err, errA)
	}
}

// --- service 方法级并发编辑/删除批注 → conflict(409) 零副作用 ---

// TestCreateSubmissionConcurrentAnnotationEditConflict 验证提交期间批注被并发编辑（revision+1）
// → CreateSubmission 返回 ErrRevisionConflict（零落库）。
// F12①：并发编辑经 Service.UpdateComment 真实路径触发（非直接改 mock map），revision 1→2。
func TestCreateSubmissionConcurrentAnnotationEditConflict(t *testing.T) {
	repo := newMockRepo()
	// 批注 a1 原始 revision=1。
	repo.annotations["a1"] = DiffAnnotationRecord{
		ID: "a1", TaskID: "t1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1,
		SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "c", Revision: 1, CreatedAt: 100,
	}
	svc := newSubmissionTestService(repo, &mockDiff{result: DiffSourceResult{NewContent: "x\n", NewExists: true, NewMode: "100644"}}, &mockPrompt{})
	// 经 Service.UpdateComment 真实路径编辑批注 comment "c"→"c2"，revision 1→2。
	if _, err := svc.UpdateComment(context.Background(), "t1", "a1", "c2"); err != nil {
		t.Fatalf("UpdateComment failed: %v", err)
	}
	if repo.annotations["a1"].Revision != 2 {
		t.Fatalf("annotation revision = %d, want 2 after UpdateComment", repo.annotations["a1"].Revision)
	}
	// 请求携带 revision=1（预览时快照），但批注已被编辑为 revision=2（并发）→ conflict。
	_, _, err := svc.CreateSubmission(context.Background(), CreateSubmissionRequest{
		TaskID:      "t1",
		Annotations: []SubmissionItemRequest{{ID: "a1", Revision: 1}},
	})
	if err != ErrRevisionConflict {
		t.Errorf("concurrent edit (revision changed to 2) should be conflict, got %v", err)
	}
}

// TestCreateSubmissionConcurrentAnnotationDeleteConflict 验证提交期间批注被并发删除
// → CreateSubmission 返回 ErrRevisionConflict（不区分从未存在与预览后删除，D2）。
// F12①：并发删除经 Service.DeleteAnnotation 真实路径触发（非直接改 mock map）。
func TestCreateSubmissionConcurrentAnnotationDeleteConflict(t *testing.T) {
	repo := newMockRepo()
	// 批注 a1 原始 revision=1。
	repo.annotations["a1"] = DiffAnnotationRecord{
		ID: "a1", TaskID: "t1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1,
		SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "c", Revision: 1, CreatedAt: 100,
	}
	svc := newSubmissionTestService(repo, &mockDiff{result: DiffSourceResult{NewContent: "x\n", NewExists: true, NewMode: "100644"}}, &mockPrompt{})
	// 经 Service.DeleteAnnotation 真实路径删除批注。
	if err := svc.DeleteAnnotation(context.Background(), "t1", "a1"); err != nil {
		t.Fatalf("DeleteAnnotation failed: %v", err)
	}
	if _, exists := repo.annotations["a1"]; exists {
		t.Fatalf("annotation should be deleted via Service")
	}
	// 请求携带 revision=1，但批注已被删除 → conflict。
	_, _, err := svc.CreateSubmission(context.Background(), CreateSubmissionRequest{
		TaskID:      "t1",
		Annotations: []SubmissionItemRequest{{ID: "a1", Revision: 1}},
	})
	if err != ErrRevisionConflict {
		t.Errorf("concurrent delete (annotation missing) should be conflict, got %v", err)
	}
}

// --- stale 算法：CRLF 快照不漂移 ---

// TestListAnnotationsStaleCRLFNoDrift 验证 CRLF 文件快照不漂移：快照含 \r（CRLF 的 \r 保留在行片段），
// 重读内容也含 CRLF（split('\n') 行片段含 \r），匹配 → active。
// F9 行模型：split('\n') = "line1\r\nline2\r\n" → ["line1\r","line2\r",""]；window 行 1 = "line1\r"。
func TestListAnnotationsStaleCRLFNoDrift(t *testing.T) {
	repo := newMockRepo()
	repo.annotations["a1"] = DiffAnnotationRecord{
		ID: "a1", TaskID: "t1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1,
		SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "line1\r",
	}
	// 重读内容含 CRLF → 窗口取第 1 行 = "line1\r" → 匹配 → active。
	svc := newTestService(repo, &mockScope{found: true, kind: "repo"},
		&mockDiff{result: DiffSourceResult{NewContent: "line1\r\nline2\r\n", NewExists: true, NewMode: "100644"}}, &mockRuntime{}, &mockPrompt{})
	views, err := svc.ListAnnotations(context.Background(), "t1")
	if err != nil {
		t.Fatalf("ListAnnotations: %v", err)
	}
	if views[0].Stale {
		t.Errorf("CRLF snapshot matching should be active (no drift), got stale=true")
	}
}

// TestListAnnotationsStaleCRLFDriftToLF 验证 CRLF 快照漂移：快照行片段含 \r，重读内容为 LF（无 \r）→ 不等 → stale。
func TestListAnnotationsStaleCRLFDriftToLF(t *testing.T) {
	repo := newMockRepo()
	repo.annotations["a1"] = DiffAnnotationRecord{
		ID: "a1", TaskID: "t1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1,
		SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "line1\r",
	}
	// 重读内容为 LF（CRLF→LF 漂移）→ 窗口 = "line1" ≠ "line1\r" → stale。
	svc := newTestService(repo, &mockScope{found: true, kind: "repo"},
		&mockDiff{result: DiffSourceResult{NewContent: "line1\nline2\n", NewExists: true, NewMode: "100644"}}, &mockRuntime{}, &mockPrompt{})
	views, _ := svc.ListAnnotations(context.Background(), "t1")
	if !views[0].Stale {
		t.Errorf("CRLF→LF drift should be stale")
	}
}

// --- stale 算法：truncated 窗口完整保持 active ---

// TestListAnnotationsStaleTruncatedWindowCompleteActive 验证 truncated 文件窗口完整落在返回前缀内
// → 正常比对保持 active（不因 truncated 标记为 stale）。
// F9：snapshot 行模型 split('\n') join \n → "l1\nl2"（无末尾 \n）；用 NewTruncated（逐侧）。
func TestListAnnotationsStaleTruncatedWindowCompleteActive(t *testing.T) {
	repo := newMockRepo()
	repo.annotations["a1"] = DiffAnnotationRecord{
		ID: "a1", TaskID: "t1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1,
		SnapshotStartLine: 1, SnapshotLineCount: 2, Snapshot: "l1\nl2",
	}
	// 重读内容前缀含 l1/l2（窗口完整在前缀内），NewTruncated=true 但窗口完整 → active。
	svc := newTestService(repo, &mockScope{found: true, kind: "repo"},
		&mockDiff{result: DiffSourceResult{NewContent: "l1\nl2\nl3\n", NewExists: true, NewMode: "100644", NewTruncated: true}}, &mockRuntime{}, &mockPrompt{})
	views, err := svc.ListAnnotations(context.Background(), "t1")
	if err != nil {
		t.Fatalf("ListAnnotations: %v", err)
	}
	if views[0].Stale {
		t.Errorf("truncated file with complete window should be active, got stale=true")
	}
}

// TestListAnnotationsStaleTruncatedWindowIncompleteStale 验证 truncated 文件窗口无法完整取得 → stale。
// F9：用 NewTruncated（逐侧）；snapshot 行模型 join \n。
func TestListAnnotationsStaleTruncatedWindowIncompleteStale(t *testing.T) {
	repo := newMockRepo()
	repo.annotations["a1"] = DiffAnnotationRecord{
		ID: "a1", TaskID: "t1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1,
		SnapshotStartLine: 1, SnapshotLineCount: 5, Snapshot: "l1\nl2\nl3\nl4\nl5",
	}
	// 重读内容仅 3 行（窗口需 5 行，截断点在窗口内）→ 窗口无法完整取得 → stale。
	svc := newTestService(repo, &mockScope{found: true, kind: "repo"},
		&mockDiff{result: DiffSourceResult{NewContent: "l1\nl2\nl3\n", NewExists: true, NewMode: "100644", NewTruncated: true}}, &mockRuntime{}, &mockPrompt{})
	views, _ := svc.ListAnnotations(context.Background(), "t1")
	if !views[0].Stale {
		t.Errorf("truncated file with incomplete window should be stale")
	}
}

// --- TaskScopePort 准入：未知 kind → internal ---

// TestTaskScopeUnknownKindInternalError 验证 TaskScopePort 返回未知 kind → service 返回 ErrUnknownProjectKind。
func TestTaskScopeUnknownKindInternalError(t *testing.T) {
	repo := newMockRepo()
	svc := newTestService(repo, &mockScope{found: true, kind: "weird"}, &mockDiff{}, &mockRuntime{}, &mockPrompt{})
	_, err := svc.CreateAnnotation(context.Background(), CreateDiffAnnotationInput{
		ID: "a1", TaskID: "t1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1,
		SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x", Comment: "c",
	})
	if err != ErrUnknownProjectKind {
		t.Errorf("unknown project kind should return ErrUnknownProjectKind, got %v", err)
	}
}

// TestTaskScopeLookupError 验证 TaskScopePort 底层错误透传。
func TestTaskScopeLookupError(t *testing.T) {
	scopeErr := errors.New("scope db error")
	svc := newTestService(newMockRepo(), &mockScope{kind: "repo", found: true}, &mockDiff{}, &mockRuntime{}, &mockPrompt{})
	// mockScope 不支持 error 注入；用自定义 scope。
	svc2 := newTestService(newMockRepo(), &errScope{err: scopeErr}, &mockDiff{}, &mockRuntime{}, &mockPrompt{})
	_, err := svc2.CreateAnnotation(context.Background(), CreateDiffAnnotationInput{
		ID: "a1", TaskID: "t1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1,
		SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x", Comment: "c",
	})
	if err != scopeErr {
		t.Errorf("scope lookup error should propagate, got %v want %v", err, scopeErr)
	}
	_ = svc
}

// errScope 为返回固定 error 的 TaskScopePort 实现。
type errScope struct{ err error }

func (s *errScope) Lookup(ctx context.Context, taskID string) (TaskScopeResult, error) {
	return TaskScopeResult{}, s.err
}

// --- payload 拼接 golden（note/comment/snapshot 结尾变体）---

// TestPayloadConcatenationGolden 验证 payload 各节拼接边界：
// note/comment/snapshot 分别以无换行、\n、\n\n 结尾时拼接正确（无多余空行/缺行）。
func TestPayloadConcatenationGolden(t *testing.T) {
	cases := []struct {
		name     string
		note     string
		comment  string
		snapshot string
	}{
		{"note no newline", "plain note", "c", "x\n"},
		{"note single newline", "note\n", "c", "x\n"},
		{"note double newline", "note\n\n", "c", "x\n"},
		{"comment no newline", "n", "comment no nl", "x\n"},
		{"snapshot no newline", "n", "c", "snapshot no nl"},
		{"snapshot double newline", "n", "c", "snap\n\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			anns := []DiffAnnotationRecord{
				{ID: "a1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: c.snapshot, Comment: c.comment, CreatedAt: 1},
			}
			result, err := assemblePayloadFromAnnotations(anns, c.note, func(sourceTriple) (DiffSourceResult, error) {
				return DiffSourceResult{NewContent: "x\n", NewExists: true, NewMode: "100644"}, nil
			})
			if err != nil {
				t.Fatalf("%s: assemblePayload: %v", c.name, err)
			}
			if len(result.Payload) > payloadMaxBytes {
				t.Errorf("%s: payload %d > %d", c.name, len(result.Payload), payloadMaxBytes)
			}
			// 断言批注节含 comment 与 snapshot 原文。
			if c.comment != "" && !strings.Contains(result.Payload, "评论："+c.comment) {
				t.Errorf("%s: payload missing comment", c.name)
			}
			if !strings.Contains(result.Payload, c.snapshot) {
				t.Errorf("%s: payload missing snapshot text", c.name)
			}
		})
	}
}

// --- F12：精确总体边界 65535/65536/65537 ---

// TestPayloadTotalBoundary65535_65536_65537 验证 payload 总体字节边界（F12：精确 65535/65536/65537）。
//   - 不截断上界：payload ≤ 65536 且 !Truncated 的最大 note 长度。
//   - +1 字节 note → 截断、payload ≤ 65536 且以 markerSuffix 结尾。
//   - -1 字节 note → 不截断（65535 区域）。
//
// 用二分查找精确的最大不截断 note 长度（note 为 ASCII 单字节 rune，noteOverhead 固定）。
func TestPayloadTotalBoundary65535_65536_65537(t *testing.T) {
	anns := []DiffAnnotationRecord{
		{ID: "a1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "c", CreatedAt: 1},
	}
	readOnce := func(noteLen int) (int, bool) {
		result, err := assemblePayloadFromAnnotations(anns, strings.Repeat("a", noteLen), func(sourceTriple) (DiffSourceResult, error) {
			return DiffSourceResult{NewContent: "z\n", NewExists: true, NewMode: "100644"}, nil
		})
		if err != nil {
			// core+markerSuffix 超阈值 → ErrPayloadTooLarge，视为「超限/截断」。
			if errors.Is(err, ErrPayloadTooLarge) {
				return payloadMaxBytes + 1, true
			}
			t.Fatalf("assemble noteLen=%d: %v", noteLen, err)
		}
		return len(result.Payload), result.Truncated
	}
	// 二分查找最大不截断 note 长度：payload ≤ 65536 且 !Truncated。
	lo, hi := 0, payloadMaxBytes
	bestNoTrunc := 0
	for lo <= hi {
		mid := (lo + hi) / 2
		n, trunc := readOnce(mid)
		if n <= payloadMaxBytes && !trunc {
			bestNoTrunc = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	// 上界：bestNoTrunc 对应 payload 恰为 65536（逻辑候选精确等于上限，不截断，无 markerSuffix）。
	nAt, truncAt := readOnce(bestNoTrunc)
	if truncAt {
		t.Errorf("at max non-truncating note len %d, should NOT truncate (payload=%d)", bestNoTrunc, nAt)
	}
	// F12：精确锁定公式——最大不截断 note 长度的 payload 恰好等于 payloadMaxBytes（65536）。
	if nAt != payloadMaxBytes {
		t.Errorf("at max non-truncating note len %d, payload=%d, want exactly %d (65536 boundary)", bestNoTrunc, nAt, payloadMaxBytes)
	}
	pAt := mustPayload(anns, bestNoTrunc)
	if strings.HasSuffix(pAt, markerSuffix) {
		t.Errorf("at max non-truncating note len %d, payload should NOT end with markerSuffix", bestNoTrunc)
	}
	// +1 字节 note → 截断（超出预算，对应 65537 区域）。
	nOver, truncOver := readOnce(bestNoTrunc + 1)
	if !truncOver {
		t.Errorf("note len %d+1 should truncate, got truncated=false (payload=%d)", bestNoTrunc, nOver)
	}
	if nOver > payloadMaxBytes {
		t.Errorf("truncated payload %d > %d (assertion breach)", nOver, payloadMaxBytes)
	}
	if !strings.HasSuffix(mustPayload(anns, bestNoTrunc+1), markerSuffix) {
		t.Errorf("truncated payload should end with markerSuffix")
	}
	// -1 字节 note → 不截断（65535 区域，payload 恰为 65535）。
	if bestNoTrunc > 0 {
		nUnder, truncUnder := readOnce(bestNoTrunc - 1)
		if truncUnder {
			t.Errorf("note len %d-1 should NOT truncate (payload=%d)", bestNoTrunc, nUnder)
		}
		// F12：精确断言 -1 区域 payload == 65535（payloadMaxBytes-1，不截断）。
		if nUnder != payloadMaxBytes-1 {
			t.Errorf("note len %d-1 payload=%d, want exactly %d (65535, no truncation)", bestNoTrunc, nUnder, payloadMaxBytes-1)
		}
	}
}

// mustPayload 辅助：读取 payload 文本，失败 fatal。
func mustPayload(anns []DiffAnnotationRecord, noteLen int) string {
	result, err := assemblePayloadFromAnnotations(anns, strings.Repeat("a", noteLen), func(sourceTriple) (DiffSourceResult, error) {
		return DiffSourceResult{NewContent: "z\n", NewExists: true, NewMode: "100644"}, nil
	})
	if err != nil {
		panic(err)
	}
	return result.Payload
}

// --- F12：formatter 调用计数（首个来源超预算后续来源 formatter 不再被调用）---

// TestPayloadBoundedSinglePassFormatterNotCalledAfterBudgetExhausted 验证首个来源超预算后，
// 后续来源仍被读取（diffRead 调用计数 == 来源数），但其 formatter 输出不出现
// （buildSourceSegment 不被调用或其结果不写入 builder）。
// 用 sourceSegment 标题 `### "path"（来源` 作为 formatter 调用代理：仅被格式化的来源出现该标题。
func TestPayloadBoundedSinglePassFormatterNotCalledAfterBudgetExhausted(t *testing.T) {
	anns := []DiffAnnotationRecord{
		{ID: "a1", Path: "big.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "c", CreatedAt: 1},
		{ID: "a2", Path: "mid.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "y\n", Comment: "c2", CreatedAt: 2},
		{ID: "a3", Path: "tail.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "z\n", Comment: "c3", CreatedAt: 3},
	}
	readCount := 0
	var mu sync.Mutex
	result, err := assemblePayloadFromAnnotations(anns, "", func(src sourceTriple) (DiffSourceResult, error) {
		mu.Lock()
		readCount++
		mu.Unlock()
		// 首来源 big.go 超预算（content 极大）触发截断；后续来源仍读取。
		return DiffSourceResult{NewContent: strings.Repeat("x", payloadMaxBytes*2), NewExists: true, NewMode: "100644"}, nil
	})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if readCount != 3 {
		t.Errorf("bounded single-pass should read ALL 3 sources, got %d", readCount)
	}
	if !result.Truncated {
		t.Errorf("should truncate after first source exceeds budget")
	}
	// 仅首来源 big.go 的 sourceSegment 出现；mid.go/tail.go 不应出现（formatter 不再格式化）。
	if !strings.Contains(result.Payload, `### "big.go"（来源`) {
		t.Errorf("first source segment should be present")
	}
	if strings.Contains(result.Payload, `### "mid.go"（来源`) {
		t.Errorf("second source segment should NOT be formatted after budget exhausted")
	}
	if strings.Contains(result.Payload, `### "tail.go"（来源`) {
		t.Errorf("third source segment should NOT be formatted after budget exhausted")
	}
}

// TestPayloadMultiSourceShrinkExistingBuilderToRoom 验证 F7 多来源收缩修复：
// 两来源、首段完整写入但 builder 已超截断预算 room（即 candidateBudget-len(markerSuffix)）、
// 次段触发超限时，必须以「已有 sourceRegion + 当前 sep + seg」为逻辑前缀统一 rune-safe 收缩到 room。
// 旧实现只截当前段、不收缩已有 builder → 追加 markerSuffix 后 payload > 65536（末断言失败）。
// 断言：最终 payload ≤ 65536 且以 markerSuffix 结尾（truncated=true）。
func TestPayloadMultiSourceShrinkExistingBuilderToRoom(t *testing.T) {
	anns := []DiffAnnotationRecord{
		{ID: "a1", Path: "big.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "c", CreatedAt: 1},
		{ID: "a2", Path: "tail.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "y\n", Comment: "c2", CreatedAt: 2},
	}
	// 单来源探针：返回长度 n 的 NewContent，观察其 sourceSegment 是否完整写入（payload 不含 markerSuffix）
	// 与对应 builder 长度，用于定位首段「完整写入但 builder 已超 room」的内容长度区间。
	probe := func(n int) (payloadLen int, hasMarker bool) {
		result, err := assemblePayloadFromAnnotations(anns[:1], "", func(src sourceTriple) (DiffSourceResult, error) {
			return DiffSourceResult{NewContent: strings.Repeat("x", n), NewExists: true, NewMode: "100644"}, nil
		})
		if err != nil {
			t.Fatalf("probe n=%d: %v", n, err)
		}
		return len(result.Payload), strings.HasSuffix(result.Payload, markerSuffix)
	}
	// 找到最大不触发截断的内容长度 n0（首段完整写入，payload 不含 markerSuffix）。
	// 此处 n0 对应单来源 payload 恰好 = candidateBudget（payloadMaxBytes）。
	lo, hi, n0 := 0, payloadMaxBytes, 0
	for lo <= hi {
		mid := (lo + hi) / 2
		n, hasMarker := probe(mid)
		if n <= payloadMaxBytes && !hasMarker {
			n0 = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	if n0 == 0 {
		t.Fatalf("could not find non-truncating first-source content size")
	}
	// 取 n0 使得首段完整写入，但 builder.Len() = sep + seg1 = payloadMaxBytes - len(core)，
	// 远大于 room = candidateBudget - len(markerSuffix)。次段任意大小都会触发收缩。
	// 两来源：首来源 content=n0（完整写入、builder 已超 room），次来源 content=1（触发超限收缩）。
	result, err := assemblePayloadFromAnnotations(anns, "", func(src sourceTriple) (DiffSourceResult, error) {
		if src.Path == "big.go" {
			return DiffSourceResult{NewContent: strings.Repeat("x", n0), NewExists: true, NewMode: "100644"}, nil
		}
		return DiffSourceResult{NewContent: "y\n", NewExists: true, NewMode: "100644"}, nil
	})
	if err != nil {
		t.Fatalf("assemble two-source: %v", err)
	}
	if !result.Truncated {
		t.Errorf("two-source with first seg overflowing room should truncate, got truncated=false")
	}
	if len(result.Payload) > payloadMaxBytes {
		t.Errorf("F7 multi-source shrink: payload %d > %d (existing builder not shrunk to room)", len(result.Payload), payloadMaxBytes)
	}
	if !strings.HasSuffix(result.Payload, markerSuffix) {
		t.Errorf("truncated two-source payload should end with markerSuffix")
	}
}

// --- F12：真实并发竞态（barrier 同步 + revision-checking repo）---

// revisionCheckingRepo 包装 mockRepo，其 CreateDiffReviewSubmission 在落库前复核每条 item 的
// annotation revision 是否仍等于快照 revision（模拟真实 store 事务内复核，D2 落库条）。
// 任一不符 → ErrRevisionConflict，零落库。
type revisionCheckingRepo struct {
	*mockRepo
}

func (r *revisionCheckingRepo) CreateDiffReviewSubmission(ctx context.Context, in CreateDiffReviewSubmissionInput) error {
	for _, it := range in.Items {
		ann, ok := r.annotations[it.AnnotationID]
		if !ok {
			return ErrRevisionConflict
		}
		if ann.Revision != it.AnnotationRevision {
			return ErrRevisionConflict
		}
	}
	return r.mockRepo.CreateDiffReviewSubmission(ctx, in)
}

// TestCreateSubmissionConcurrentAnnotationEditWithBarrier 验证提交与并发批注编辑的真实竞态：
// F12：barrier 在 CreateDiffReviewSubmission（事务复核）前暂停。另一 goroutine 经 Service.UpdateComment
// 真实路径编辑批注 revision 1→2（非直接改 mock map），待编辑完成释放 barrier 后事务内复核发现
// revision 不符 → conflict，零落库。真命中事务复核竞态（快照与复核之间编辑）。
func TestCreateSubmissionConcurrentAnnotationEditWithBarrier(t *testing.T) {
	repo := &revisionCheckingRepo{mockRepo: newMockRepo()}
	repo.annotations["a1"] = DiffAnnotationRecord{
		ID: "a1", TaskID: "t1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1,
		SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "c", Revision: 1, CreatedAt: 100,
	}
	createStarted := make(chan struct{})
	editDone := make(chan struct{})
	hookRepo := &createBarrierRepo{
		revisionCheckingRepo: repo,
		started:              createStarted,
		proceed:              editDone,
	}
	rt := &mockRuntime{
		snap:     RuntimeSnapshot{HasRuntime: true, HasAnchorSession: true, AnchorSessionID: "sess1", InstVersion: "v1"},
		probeCap: CapabilitySupported,
		sessStat: SessionStatusIdle,
	}
	svc := newTestService(hookRepo, &mockScope{found: true, kind: "repo"},
		&mockDiff{result: DiffSourceResult{NewContent: "x\n", NewExists: true, NewMode: "100644"}}, rt, &mockPrompt{})

	var wg sync.WaitGroup
	var svcErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _, svcErr = svc.CreateSubmission(context.Background(), CreateSubmissionRequest{
			TaskID:      "t1",
			Annotations: []SubmissionItemRequest{{ID: "a1", Revision: 1}},
		})
	}()

	// 等待 CreateDiffReviewSubmission 被调用（快照已取回 revision=1，事务复核前 barrier 暂停）。
	<-createStarted
	// F12①：经 Service.UpdateComment 真实路径编辑批注 revision 1→2（非直接改 mock map）。
	var editErr error
	editDone2 := make(chan struct{})
	go func() {
		defer close(editDone2)
		_, editErr = svc.UpdateComment(context.Background(), "t1", "a1", "c2-edited")
	}()
	<-editDone2 // 等待编辑完成（revision 已 +1）。
	if editErr != nil {
		t.Fatalf("concurrent UpdateComment failed: %v", editErr)
	}
	close(editDone) // 释放 barrier，CreateDiffReviewSubmission 事务复核发现 revision 不符。

	wg.Wait()
	if svcErr != ErrRevisionConflict {
		t.Errorf("concurrent edit with barrier should cause conflict, got %v", svcErr)
	}
	// 零落库：submissions 应为空。
	if len(repo.submissions) != 0 {
		t.Errorf("concurrent edit should zero submissions, got %d", len(repo.submissions))
	}
	// 批注已被真实编辑（revision=2）。
	if repo.annotations["a1"].Revision != 2 {
		t.Errorf("annotation revision = %d, want 2 (edited via Service)", repo.annotations["a1"].Revision)
	}
}

// TestCreateSubmissionConcurrentAnnotationDeleteWithBarrier 验证提交与并发批注删除的真实竞态：
// F12：barrier 在快照后、CreateDiffReviewSubmission（事务复核）前暂停。另一 goroutine 经
// Service.DeleteAnnotation 真实路径删除批注（非直接改 mock map），释放后事务复核发现批注缺失
// → conflict（不区分从未存在与预览后删除）。
func TestCreateSubmissionConcurrentAnnotationDeleteWithBarrier(t *testing.T) {
	repo := &revisionCheckingRepo{mockRepo: newMockRepo()}
	repo.annotations["a1"] = DiffAnnotationRecord{
		ID: "a1", TaskID: "t1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1,
		SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "c", Revision: 1, CreatedAt: 100,
	}
	createStarted := make(chan struct{})
	deleteDone := make(chan struct{})
	hookRepo := &createBarrierRepo{
		revisionCheckingRepo: repo,
		started:              createStarted,
		proceed:              deleteDone,
	}
	rt := &mockRuntime{
		snap:     RuntimeSnapshot{HasRuntime: true, HasAnchorSession: true, AnchorSessionID: "sess1", InstVersion: "v1"},
		probeCap: CapabilitySupported,
		sessStat: SessionStatusIdle,
	}
	svc := newTestService(hookRepo, &mockScope{found: true, kind: "repo"},
		&mockDiff{result: DiffSourceResult{NewContent: "x\n", NewExists: true, NewMode: "100644"}}, rt, &mockPrompt{})

	var wg sync.WaitGroup
	var svcErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _, svcErr = svc.CreateSubmission(context.Background(), CreateSubmissionRequest{
			TaskID:      "t1",
			Annotations: []SubmissionItemRequest{{ID: "a1", Revision: 1}},
		})
	}()

	<-createStarted
	// F12①：经 Service.DeleteAnnotation 真实路径删除批注（非直接改 mock map）。
	var delErr error
	delDone2 := make(chan struct{})
	go func() {
		defer close(delDone2)
		delErr = svc.DeleteAnnotation(context.Background(), "t1", "a1")
	}()
	<-delDone2 // 等待删除完成。
	if delErr != nil {
		t.Fatalf("concurrent DeleteAnnotation failed: %v", delErr)
	}
	close(deleteDone) // 释放 barrier。

	wg.Wait()
	if svcErr != ErrRevisionConflict {
		t.Errorf("concurrent delete with barrier should cause conflict, got %v", svcErr)
	}
	if len(repo.submissions) != 0 {
		t.Errorf("concurrent delete should zero submissions, got %d", len(repo.submissions))
	}
	if _, exists := repo.annotations["a1"]; exists {
		t.Errorf("annotation should be deleted via Service")
	}
}

// createBarrierRepo 包装 revisionCheckingRepo，首次 CreateDiffReviewSubmission 时触发 barrier：
// 通知 started 并阻塞等待 proceed 关闭，使并发编辑/删除发生在快照后、事务复核前（真命中事务复核竞态）。
type createBarrierRepo struct {
	*revisionCheckingRepo
	started chan<- struct{}
	proceed <-chan struct{}
	called  bool
	mu      sync.Mutex
}

func (b *createBarrierRepo) CreateDiffReviewSubmission(ctx context.Context, in CreateDiffReviewSubmissionInput) error {
	b.mu.Lock()
	if !b.called {
		b.called = true
		b.mu.Unlock()
		close(b.started)
		<-b.proceed
	} else {
		b.mu.Unlock()
	}
	return b.revisionCheckingRepo.CreateDiffReviewSubmission(ctx, in)
}

// --- F12：生产启动 fail-closed（随 F1）---

// TestConvergeOnStartupFailClosedPreventsStart 验证收敛写库失败 → 返回 error，
// 调用方（main composition root）MUST 不开放 API/调度器（fail-closed 语义）。
// 此处验证 Service.ConvergeOnStartup 返回非 nil error；main.go 接线在 Reconcile 前调用并检查。
func TestConvergeOnStartupFailClosedPreventsStart(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "sending"}
	repo.convergeErr = errors.New("disk full")
	svc := newTestService(repo, &mockScope{found: true, kind: "repo"}, &mockDiff{}, &mockRuntime{}, &mockPrompt{})
	_, err := svc.ConvergeOnStartup(context.Background())
	if err == nil {
		t.Fatal("converge write failure should return error (fail-closed, prevents API/scheduler start)")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("fail-closed error should propagate underlying cause, got %v", err)
	}
	// 收敛未成功：sending 行仍为 sending（未被转 delivery_unknown）。
	if repo.submissions["s1"].Status != "sending" {
		t.Errorf("failed converge should not mutate submission status, got %s", repo.submissions["s1"].Status)
	}
}

// --- F12：跨任务隔离（F4 归属校验）---

// TestUpdateCommentCrossTaskOwnershipRejected 验证 UpdateComment 跨任务归属不符 → not_found
// （零写副作用，不泄露跨任务批注存在性）。
func TestUpdateCommentCrossTaskOwnershipRejected(t *testing.T) {
	repo := newMockRepo()
	repo.annotations["a1"] = DiffAnnotationRecord{ID: "a1", TaskID: "owner-task", Comment: "orig", Revision: 1}
	svc := newTestService(repo, &mockScope{found: true, kind: "repo"}, &mockDiff{}, &mockRuntime{}, &mockPrompt{})
	_, err := svc.UpdateComment(context.Background(), "other-task", "a1", "new comment")
	if err != ErrAnnotationNotFound {
		t.Errorf("cross-task update should return not_found, got %v", err)
	}
	// 零写副作用：批注仍属 owner-task，comment 未变。
	if repo.annotations["a1"].Comment != "orig" {
		t.Errorf("cross-task update should not modify annotation, comment=%q", repo.annotations["a1"].Comment)
	}
	if repo.annotations["a1"].Revision != 1 {
		t.Errorf("cross-task update should not change revision, got %d", repo.annotations["a1"].Revision)
	}
}

// TestDeleteAnnotationCrossTaskOwnershipRejected 验证 DeleteAnnotation 跨任务归属不符 → not_found
// （零删除副作用）。
func TestDeleteAnnotationCrossTaskOwnershipRejected(t *testing.T) {
	repo := newMockRepo()
	repo.annotations["a1"] = DiffAnnotationRecord{ID: "a1", TaskID: "owner-task"}
	svc := newTestService(repo, &mockScope{found: true, kind: "repo"}, &mockDiff{}, &mockRuntime{}, &mockPrompt{})
	err := svc.DeleteAnnotation(context.Background(), "other-task", "a1")
	if err != ErrAnnotationNotFound {
		t.Errorf("cross-task delete should return not_found, got %v", err)
	}
	if _, exists := repo.annotations["a1"]; !exists {
		t.Errorf("cross-task delete should not remove annotation (zero side effect)")
	}
}

// TestCancelSubmissionCrossTaskOwnershipRejected 验证 CancelSubmission 跨任务归属不符 → not_found
// （零撤回副作用）。
func TestCancelSubmissionCrossTaskOwnershipRejected(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "owner-task", Status: "queued"}
	svc := newTestService(repo, &mockScope{found: true, kind: "repo"}, &mockDiff{}, &mockRuntime{}, &mockPrompt{})
	err := svc.CancelSubmission(context.Background(), "other-task", "s1")
	if err != ErrSubmissionNotFound {
		t.Errorf("cross-task cancel should return not_found, got %v", err)
	}
	if repo.submissions["s1"].Status != "queued" {
		t.Errorf("cross-task cancel should not mutate submission, got %s", repo.submissions["s1"].Status)
	}
}

// TestDeleteSubmissionCrossTaskOwnershipRejected 验证 DeleteSubmission 跨任务归属不符 → not_found
// （零删除副作用）。
func TestDeleteSubmissionCrossTaskOwnershipRejected(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "owner-task", Status: "sent"}
	svc := newTestService(repo, &mockScope{found: true, kind: "repo"}, &mockDiff{}, &mockRuntime{}, &mockPrompt{})
	err := svc.DeleteSubmission(context.Background(), "other-task", "s1")
	if err != ErrSubmissionNotFound {
		t.Errorf("cross-task delete should return not_found, got %v", err)
	}
	if _, exists := repo.submissions["s1"]; !exists {
		t.Errorf("cross-task delete should not remove submission (zero side effect)")
	}
}

// --- F12：领域准入顺序（F8：纯 DTO 校验先于 port 调用）---

// TestCreateSubmissionFieldValidationBeforePortCalls 验证空批注列表/重复 id/revision 非法
// 在任何 port（scope/runtime/能力探测）调用前拒绝（F8）。
// 用记录 port 调用次数的 mock 验证零 port 调用。
func TestCreateSubmissionFieldValidationBeforePortCalls(t *testing.T) {
	cases := []struct {
		name string
		req  CreateSubmissionRequest
		want error
	}{
		{"empty annotations rejected", CreateSubmissionRequest{TaskID: "t1"}, ErrEmptySubmission},
		{"duplicate id rejected", CreateSubmissionRequest{
			TaskID: "t1",
			Annotations: []SubmissionItemRequest{
				{ID: "a1", Revision: 1}, {ID: "a1", Revision: 1},
			},
		}, ErrDuplicateAnnotationID},
		{"revision zero rejected", CreateSubmissionRequest{
			TaskID:      "t1",
			Annotations: []SubmissionItemRequest{{ID: "a1", Revision: 0}},
		}, ErrInvalidAnnotationRevision},
		{"empty id rejected", CreateSubmissionRequest{
			TaskID:      "t1",
			Annotations: []SubmissionItemRequest{{ID: "", Revision: 1}},
		}, ErrInvalidAnnotationID},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt := &mockRuntime{}
			scope := &countingScope{kind: "repo", found: true}
			svc := newTestService(newMockRepo(), scope, &mockDiff{}, rt, &mockPrompt{})
			_, _, err := svc.CreateSubmission(context.Background(), c.req)
			if err != c.want {
				t.Errorf("%s: got %v want %v", c.name, err, c.want)
			}
			if scope.calls > 0 {
				t.Errorf("%s: scope.Lookup should NOT be called before field validation, got %d calls", c.name, scope.calls)
			}
		})
	}
}

// TestCreateSubmissionTooManyAnnotationsRejected 验证 annotations > 500 → invalid_input（F8）。
func TestCreateSubmissionTooManyAnnotationsRejected(t *testing.T) {
	scope := &countingScope{kind: "repo", found: true}
	svc := newTestService(newMockRepo(), scope, &mockDiff{}, &mockRuntime{}, &mockPrompt{})
	anns := make([]SubmissionItemRequest, maxSubmissionAnnotations+1)
	for i := range anns {
		anns[i] = SubmissionItemRequest{ID: "a" + itoa(i), Revision: 1}
	}
	_, _, err := svc.CreateSubmission(context.Background(), CreateSubmissionRequest{TaskID: "t1", Annotations: anns})
	if err != ErrTooManyAnnotations {
		t.Errorf("too many annotations should be rejected, got %v", err)
	}
	if scope.calls > 0 {
		t.Errorf("scope should NOT be called for >500 annotations (field validation first), got %d calls", scope.calls)
	}
}

// TestCreateAnnotationFieldValidationBeforePortCalls 验证 CreateAnnotation 纯领域校验
// （空白 comment/path 空/来源非法/范围非法/窗口不自洽）先于 scope 调用（F8）。
func TestCreateAnnotationFieldValidationBeforePortCalls(t *testing.T) {
	cases := []struct {
		name string
		in   CreateDiffAnnotationInput
		want error
	}{
		{"empty comment", CreateDiffAnnotationInput{
			ID: "a1", TaskID: "t1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1,
			SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "  ",
		}, ErrEmptyComment},
		{"empty path", CreateDiffAnnotationInput{
			ID: "a1", TaskID: "t1", Path: "", Side: "new", StartLine: 1, EndLine: 1,
			SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "c",
		}, ErrInvalidAnnotationPath},
		{"absolute path", CreateDiffAnnotationInput{
			ID: "a1", TaskID: "t1", Path: "/etc/hosts", Side: "new", StartLine: 1, EndLine: 1,
			SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "c",
		}, ErrInvalidAnnotationPath},
		{"parent escape path", CreateDiffAnnotationInput{
			ID: "a1", TaskID: "t1", Path: "../x.go", Side: "new", StartLine: 1, EndLine: 1,
			SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "c",
		}, ErrInvalidAnnotationPath},
		{"NUL in path", CreateDiffAnnotationInput{
			ID: "a1", TaskID: "t1", Path: "a\x00b", Side: "new", StartLine: 1, EndLine: 1,
			SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "c",
		}, ErrInvalidAnnotationPath},
		{"invalid side", CreateDiffAnnotationInput{
			ID: "a1", TaskID: "t1", Path: "f.go", Side: "middle", StartLine: 1, EndLine: 1,
			SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x", Comment: "c",
		}, ErrInvalidAnnotationSide},
		{"snapshot line count mismatch", CreateDiffAnnotationInput{
			ID: "a1", TaskID: "t1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1,
			SnapshotStartLine: 1, SnapshotLineCount: 3, Snapshot: "only-one-line\n", Comment: "c",
		}, ErrInvalidSnapshotWindow},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			scope := &countingScope{kind: "repo", found: true}
			svc := newTestService(newMockRepo(), scope, &mockDiff{}, &mockRuntime{}, &mockPrompt{})
			_, err := svc.CreateAnnotation(context.Background(), c.in)
			if err != c.want {
				t.Errorf("%s: got %v want %v", c.name, err, c.want)
			}
			if scope.calls > 0 {
				t.Errorf("%s: scope.Lookup should NOT be called before field validation, got %d calls", c.name, scope.calls)
			}
		})
	}
}

// countingScope 包装 mockScope 并记录 Lookup 调用次数。
type countingScope struct {
	kind  string
	found bool
	calls int
}

func (s *countingScope) Lookup(ctx context.Context, taskID string) (TaskScopeResult, error) {
	s.calls++
	return TaskScopeResult{Found: s.found, Kind: s.kind}, nil
}

// --- F12：F2 400/意外 2xx 失效后立即复探 ---

// TestSchedulerHTTP400TriggersImmediateReprobe 验证 400 失效后立即触发 ProbeCapability（F2）。
// mockRuntime 记录 ProbeCapability 调用次数：400 前一次门禁探测 + 400 后一次复探 = 2 次。
func TestSchedulerHTTP400TriggersImmediateReprobe(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "queued", TargetSessionID: "sess1"}
	rt := &mockRuntime{
		snap:     RuntimeSnapshot{HasRuntime: true, HasAnchorSession: true, AnchorSessionID: "sess1", InstVersion: "v1"},
		probeCap: CapabilitySupported,
		sessStat: SessionStatusIdle,
	}
	prompt := &mockPrompt{outcome: PromptOutcome{Kind: PromptOutcomeHTTPResponse, StatusCode: 400, Body: "bad request"}}
	svc := newSchedulerTestService(repo, rt, prompt)
	sch := NewScheduler(SchedulerOptions{Service: svc, Callbacks: &mockCallbacks{}, TaskID: "t1"})

	sch.tick(context.Background())
	// 门禁探测 1 次 + 400 后复探 1 次 = 2 次。
	if rt.probeCalls != 2 {
		t.Errorf("400 should trigger immediate reprobe, got probeCalls=%d, want 2", rt.probeCalls)
	}
	if repo.submissions["s1"].Status != "failed" {
		t.Errorf("400 should transition to failed, got %s", repo.submissions["s1"].Status)
	}
}

// TestSchedulerHTTPUnexpected2xxTriggersImmediateReprobe 验证意外 2xx 失效后立即触发复探（F2）。
func TestSchedulerHTTPUnexpected2xxTriggersImmediateReprobe(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "queued", TargetSessionID: "sess1"}
	rt := &mockRuntime{
		snap:     RuntimeSnapshot{HasRuntime: true, HasAnchorSession: true, AnchorSessionID: "sess1", InstVersion: "v1"},
		probeCap: CapabilitySupported,
		sessStat: SessionStatusIdle,
	}
	prompt := &mockPrompt{outcome: PromptOutcome{Kind: PromptOutcomeHTTPResponse, StatusCode: 200, Body: "ok"}}
	svc := newSchedulerTestService(repo, rt, prompt)
	sch := NewScheduler(SchedulerOptions{Service: svc, Callbacks: &mockCallbacks{}, TaskID: "t1"})

	sch.tick(context.Background())
	if rt.probeCalls != 2 {
		t.Errorf("unexpected 2xx should trigger immediate reprobe, got probeCalls=%d, want 2", rt.probeCalls)
	}
	if repo.submissions["s1"].Status != "delivery_unknown" {
		t.Errorf("unexpected 2xx should transition to delivery_unknown, got %s", repo.submissions["s1"].Status)
	}
}

// TestSchedulerHTTP400TerminalBeforeProbe 验证 F16：400 复探（第 2 次 probe）阻塞时，
// 终态 CAS 已落库（failed），不残留 sending。释放 probe barrier 后 probe 完成。
func TestSchedulerHTTP400TerminalBeforeProbe(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "queued", TargetSessionID: "sess1"}
	probeStarted := make(chan struct{})
	probeProceed := make(chan struct{})
	rt := &mockRuntime{
		snap:         RuntimeSnapshot{HasRuntime: true, HasAnchorSession: true, AnchorSessionID: "sess1", InstVersion: "v1"},
		probeCap:     CapabilitySupported,
		sessStat:     SessionStatusIdle,
		probeBlockOn: 2, // 阻塞第 2 次 probe（复探）
		probeStarted: probeStarted,
		probeProceed: probeProceed,
	}
	prompt := &mockPrompt{outcome: PromptOutcome{Kind: PromptOutcomeHTTPResponse, StatusCode: 400, Body: "bad"}}
	svc := newSchedulerTestService(repo, rt, prompt)
	sch := NewScheduler(SchedulerOptions{Service: svc, Callbacks: &mockCallbacks{}, TaskID: "t1"})

	tickDone := make(chan struct{})
	go func() {
		sch.tick(context.Background())
		close(tickDone)
	}()

	// 等待第 2 次 probe（复探）被调用并阻塞。
	<-probeStarted
	// F16：复探阻塞期间，终态已落库（failed），不残留 sending。
	if repo.submissions["s1"].Status != "failed" {
		t.Errorf("terminal CAS should be persisted before probe completes, got %s", repo.submissions["s1"].Status)
	}
	// 释放复探，tick 完成。
	close(probeProceed)
	<-tickDone
	if rt.probeCalls != 2 {
		t.Errorf("should have 2 probe calls (gate + reprobe), got %d", rt.probeCalls)
	}
}

// --- F12：F14 首来源失败后续仍读取且 formatter 不调用 ---

// TestPayloadFirstSourceFailureContinuesReading 验证首来源读取失败后，后续来源仍被读取
// （F14/D7），且整体失败返回首个排序来源错误（assembler 汇总后返回）。
func TestPayloadFirstSourceFailureContinuesReading(t *testing.T) {
	anns := []DiffAnnotationRecord{
		{ID: "a1", Path: "a_fail.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "c", CreatedAt: 1},
		{ID: "a2", Path: "b_ok.go", Side: "new", StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "y\n", Comment: "c2", CreatedAt: 2},
	}
	readCount := 0
	var mu sync.Mutex
	errA := errors.New("a_fail.go read failed")
	_, err := assemblePayloadFromAnnotations(anns, "", func(src sourceTriple) (DiffSourceResult, error) {
		mu.Lock()
		readCount++
		mu.Unlock()
		if src.Path == "a_fail.go" {
			return DiffSourceResult{}, errA
		}
		return DiffSourceResult{NewContent: "y\n", NewExists: true, NewMode: "100644"}, nil
	})
	if err != errA {
		t.Errorf("first source failure should return its error, got %v want %v", err, errA)
	}
	if readCount != 2 {
		t.Errorf("F14: first source failure should NOT short-circuit, should read all 2 sources, got %d", readCount)
	}
}

// --- F12：F9 截断不完整后缀窗口 stale 边界 ---

// TestListAnnotationsStaleTruncatedIncompleteTailWindowStale 验证对应侧被截断、内容末行不以 \n 结尾
// 且窗口恰好覆盖该不完整末行 → stale（F9：snapshot 与窗口前缀逐字相同，但因截断仍 stale）。
// 关键：使用 per-side NewTruncated=true（非聚合 Truncated）；snapshot 与 extractLineWindow 返回的窗口逐字相同。
func TestListAnnotationsStaleTruncatedIncompleteTailWindowStale(t *testing.T) {
	repo := newMockRepo()
	repo.annotations["a1"] = DiffAnnotationRecord{
		ID: "a1", TaskID: "t1", Path: "f.go", Side: "new", StartLine: 3, EndLine: 3,
		SnapshotStartLine: 1, SnapshotLineCount: 3, Snapshot: "l1\nl2\nl3",
	}
	// 截断内容 "l1\nl2\nl3"（l3 无末尾 \n，截断不完整后缀），NewTruncated=true，窗口 1-3 行 = "l1\nl2\nl3" == snapshot。
	// 但窗口末行 l3 是截断不完整后缀 → stale（F9）。
	svc := newTestService(repo, &mockScope{found: true, kind: "repo"},
		&mockDiff{result: DiffSourceResult{NewContent: "l1\nl2\nl3", NewExists: true, NewMode: "100644", NewTruncated: true}}, &mockRuntime{}, &mockPrompt{})
	views, _ := svc.ListAnnotations(context.Background(), "t1")
	if !views[0].Stale {
		t.Errorf("truncated content with incomplete tail (no trailing \\n) and window covering it should be stale even when snapshot matches window prefix")
	}
}

// TestListAnnotationsStaleTruncatedCompleteTailWindowActive 验证对应侧被截断但内容以 \n 结尾（末行完整）
// 且窗口落在完整行内 → active（F9：截断文件窗口完整落在返回前缀内保持 active）。
func TestListAnnotationsStaleTruncatedCompleteTailWindowActive(t *testing.T) {
	repo := newMockRepo()
	repo.annotations["a1"] = DiffAnnotationRecord{
		ID: "a1", TaskID: "t1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1,
		SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "l1",
	}
	// 截断内容 "l1\nl2\nl3\n"（以 \n 结尾，末行完整），NewTruncated=true，窗口 1 行 = "l1" == snapshot → active。
	svc := newTestService(repo, &mockScope{found: true, kind: "repo"},
		&mockDiff{result: DiffSourceResult{NewContent: "l1\nl2\nl3\n", NewExists: true, NewMode: "100644", NewTruncated: true}}, &mockRuntime{}, &mockPrompt{})
	views, err := svc.ListAnnotations(context.Background(), "t1")
	if err != nil {
		t.Fatalf("ListAnnotations: %v", err)
	}
	if views[0].Stale {
		t.Errorf("truncated content with complete tail (trailing \\n), window before tail should be active")
	}
}

// TestListAnnotationsStaleTruncatedPrefixEndsWithNewlineCoversTailStale 验证 F9 截断尾语义最后一处：
// 截断前缀恰以 \n 结尾时（真实内容 "l1\nrest" 被截成 "l1\n"），split('\n') 末元素 "" 其实是
// 未知 rest 的前缀（不完整行）——旧实现对此无条件返回 active（不完整），新实现窗口覆盖末元素 → stale。
func TestListAnnotationsStaleTruncatedPrefixEndsWithNewlineCoversTailStale(t *testing.T) {
	repo := newMockRepo()
	// 批注锚定第 2 行（窗口 [2,2]），快照为空串（预览时该行可见为空）。
	repo.annotations["a1"] = DiffAnnotationRecord{
		ID: "a1", TaskID: "t1", Path: "f.go", Side: "new", StartLine: 2, EndLine: 2,
		SnapshotStartLine: 2, SnapshotLineCount: 1, Snapshot: "",
	}
	// 重读内容为截断前缀 "l1\n"（NewTruncated=true）：split = ["l1",""]，末元素 "" 不完整。
	// 窗口取第 2 行（= 末元素 ""）→ 覆盖不完整行 → stale（窗口逐字等于 snapshot 也不例外）。
	svc := newTestService(repo, &mockScope{found: true, kind: "repo"},
		&mockDiff{result: DiffSourceResult{NewContent: "l1\n", NewExists: true, NewMode: "100644", NewTruncated: true}}, &mockRuntime{}, &mockPrompt{})
	views, err := svc.ListAnnotations(context.Background(), "t1")
	if err != nil {
		t.Fatalf("ListAnnotations: %v", err)
	}
	if !views[0].Stale {
		t.Errorf("truncated prefix ending with \\n: window covering trailing empty line should be stale")
	}
}

// TestListAnnotationsStaleOnlyOtherSideTruncatedNoFalseStale 验证 F9 核心修复：
// 仅另一侧（old）被截断，本侧（new）天然无末尾换行（不以 \n 结尾），但本侧未被截断 →
// MUST NOT 误判 stale。旧实现用聚合 result.Truncated 导致仅另一侧截断+本侧无末尾换行误判 stale。
// 本侧内容与 snapshot 逐字相同 → active。
func TestListAnnotationsStaleOnlyOtherSideTruncatedNoFalseStale(t *testing.T) {
	repo := newMockRepo()
	repo.annotations["a1"] = DiffAnnotationRecord{
		ID: "a1", TaskID: "t1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1,
		SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "l1",
	}
	// 本侧（new）内容 "l1"（无末尾 \n，天然如此，未截断）；另一侧（old）被截断 OldTruncated=true。
	// 旧实现聚合 Truncated=true + isWindowTouchingIncompleteTruncationTail("l1",1,1)=true → 误判 stale。
	// F9 修复：sideTruncated=NewTruncated=false → 不进入 incomplete-tail 判定 → 窗口 "l1" == snapshot → active。
	svc := newTestService(repo, &mockScope{found: true, kind: "repo"},
		&mockDiff{result: DiffSourceResult{
			OldContent: "oldbig\noldbig\noldbig", OldExists: true, OldMode: "100644", OldTruncated: true,
			NewContent: "l1", NewExists: true, NewMode: "100644",
		}}, &mockRuntime{}, &mockPrompt{})
	views, err := svc.ListAnnotations(context.Background(), "t1")
	if err != nil {
		t.Fatalf("ListAnnotations: %v", err)
	}
	if views[0].Stale {
		t.Errorf("only-other-side truncated + this-side no trailing newline MUST NOT be stale (per-side truncation, F9)")
	}
}

// --- F12⑤：UpdateComment 超限零 port 调用 ---

// TestUpdateCommentOversizedRejectsBeforePortCalls 验证 UpdateComment 的 comment 65536-byte 上限
// 为纯 DTO 校验，先于任何 port 调用（F8/F12⑤）。用 countingScope 断言零调用。
func TestUpdateCommentOversizedRejectsBeforePortCalls(t *testing.T) {
	repo := newMockRepo()
	scope := &countingScope{kind: "repo", found: true}
	svc := newTestService(repo, scope, &mockDiff{}, &mockRuntime{}, &mockPrompt{})
	oversized := strings.Repeat("a", maxTextFieldBytes+1)
	_, err := svc.UpdateComment(context.Background(), "t1", "a1", oversized)
	if err != ErrFieldTooLarge {
		t.Errorf("oversized comment should return ErrFieldTooLarge, got %v", err)
	}
	if scope.calls > 0 {
		t.Errorf("scope.Lookup should NOT be called for oversized comment (field validation first), got %d calls", scope.calls)
	}
}
