// submission.go 实现 3.6 提交用例（design.md D2 准入/落库 + D7 payload 组装 + spec 提交给 task agent 会话）。
//
// 用例：
//   - CreateSubmission：准入（任务运行中/锚定/能力 supported/批次 id 分类优先级）+
//     payload 组装（D7 逐字公式）+ 单事务落库（queued+items 快照+revision 复核）+ messageID 生成。
//   - ListSubmissions：分区列表（queue/history/failures）+ items。
//   - CancelSubmission：撤回（仅 queued）。
//   - DeleteSubmission：终态删除（仅 sent/failed/delivery_unknown）。
package diffreview

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
)

// SubmissionItemRequest 为提交请求中的单条批注引用（design.md D8：{id, revision}）。
type SubmissionItemRequest struct {
	ID       string
	Revision int
}

// CreateSubmissionRequest 为提交请求入参（design.md D8：{annotations: [{id, revision}], note}）。
type CreateSubmissionRequest struct {
	TaskID      string
	Annotations []SubmissionItemRequest
	Note        string
}

// SubmissionView 为提交分区列表项（含 items 快照）。
type SubmissionView struct {
	Submission DiffReviewSubmissionRecord
	Items      []DiffReviewSubmissionItemRecord
}

// CreateSubmission 创建提交（design.md D2 准入 + D7 组装 + 单事务落库）。
//
// 准入顺序（全部满足才落库，否则 invalid_state/invalid_input/conflict，零副作用）。
// F8/D8：纯 DTO/领域校验先于任何 port 调用（避免无效请求触发副作用端口）。
//  0. 纯领域校验（无副作用、无 port 调用）：annotations 非空 + id 不重复 + revision 合法(1..MaxInt64) +
//     annotations ≤500 项 + note ≤65536 UTF-8 bytes。
//  1. 任务作用域准入（scope.Lookup）。
//  2. 任务运行中（runtime 存在，Snapshot.HasRuntime）。
//  3. 锚定会话非空（Snapshot.HasAnchorSession）。
//  4. 能力 supported（EnsureCapabilitySupported）。
//  5. 批次 id 分类优先级（与数组顺序无关）：任一跨任务→invalid_input；否则任一缺失或 revision 不符→conflict。
//  6. payload 组装（D7）+ 核心区体积准入。
//  7. 单事务落库（queued + items 快照 + revision 复核）。
//
// messageID（design.md D1）= "msg_" + submission UUID 去连字符小写，生成一次持久化。
func (s *Service) CreateSubmission(ctx context.Context, req CreateSubmissionRequest) (DiffReviewSubmissionRecord, []DiffReviewSubmissionItemRecord, error) {
	// F8：纯领域校验先于任何 port 调用（D8 共用校验：纯 DTO 校验置于 port 调用前）。
	if err := validateSubmissionRequest(req); err != nil {
		return DiffReviewSubmissionRecord{}, nil, err
	}

	// 1. 任务作用域准入。
	if err := s.checkTaskScope(ctx, req.TaskID); err != nil {
		return DiffReviewSubmissionRecord{}, nil, err
	}

	// 2-3. runtime 快照（运行中 + 锚定会话）。
	snap, err := s.rt.Snapshot(ctx, req.TaskID)
	if err != nil {
		return DiffReviewSubmissionRecord{}, nil, err
	}
	if !snap.HasRuntime {
		return DiffReviewSubmissionRecord{}, nil, ErrTaskNotRunning
	}
	if !snap.HasAnchorSession {
		return DiffReviewSubmissionRecord{}, nil, ErrNoAnchorSession
	}

	// 4. 能力 supported（遇 absent/unknown 同步复探，D1 事件模型②）。
	if err := s.EnsureCapabilitySupported(ctx, req.TaskID); err != nil {
		return DiffReviewSubmissionRecord{}, nil, err
	}

	// 5. 批次 id 分类优先级（与数组顺序无关）。
	// 全部分类：任一跨任务→invalid_input；否则任一缺失或 revision 不符→conflict。
	existing, err := s.repo.ListDiffAnnotationsByTask(ctx, req.TaskID)
	if err != nil {
		return DiffReviewSubmissionRecord{}, nil, err
	}
	existingByID := make(map[string]DiffAnnotationRecord, len(existing))
	for _, a := range existing {
		existingByID[a.ID] = a
	}
	// 先检查跨任务（任一 id 存在但属其他任务）。
	// 注意：ListDiffAnnotationsByTask 仅返回本任务批注，跨任务 id 在此表现为"缺失"。
	// 跨任务判定需 GetDiffAnnotation（不限 task）——但 store 原语 GetDiffAnnotation 按 id 主键查，
	// 返回的行含 task_id，可用于跨任务判定。
	if err := s.classifyBatchIDs(ctx, req.TaskID, req.Annotations); err != nil {
		return DiffReviewSubmissionRecord{}, nil, err
	}
	// 本任务范围内：任一缺失或 revision 不符 → conflict。
	selected, err := s.selectBatchAnnotations(ctx, req.TaskID, req.Annotations, existingByID)
	if err != nil {
		return DiffReviewSubmissionRecord{}, nil, err
	}

	// 7. payload 组装（D7 逐字公式 + 有界单遍算法，F5：单次任务锁内逐来源读取）。
	// diffReadAll 经 DiffSourcePort.ReadLocked 在单个任务锁作用域内逐来源读取核心 helper gitDiffLocked，
	// 回调内完成格式化与有界 builder 写入（组装全程持锁，禁止递归加锁）。
	diffReadAll := func(srcs []sourceTriple, onSource func(src sourceTriple, result DiffSourceResult, err error) error) error {
		diffSrcs := make([]DiffSource, len(srcs))
		for i, s := range srcs {
			diffSrcs[i] = DiffSource{Ref: s.Ref, Path: s.Path, Untracked: s.Untracked}
		}
		cb := func(src DiffSource, result DiffSourceResult, err error) error {
			t := sourceTriple{Ref: src.Ref, Path: src.Path, Untracked: src.Untracked}
			return onSource(t, result, err)
		}
		return s.diff.ReadLocked(ctx, req.TaskID, diffSrcs, cb)
	}
	result, err := assemblePayloadFromAnnotationsLocked(selected, req.Note, diffReadAll)
	if err != nil {
		return DiffReviewSubmissionRecord{}, nil, err
	}

	// 8. 单事务落库（queued + items 快照 + revision 复核）。
	submissionID, err := newUUID()
	if err != nil {
		return DiffReviewSubmissionRecord{}, nil, fmt.Errorf("diffreview: generate submission id: %w", err)
	}
	messageID := newMessageID(submissionID)
	items := buildSubmissionItems(submissionID, selected)
	sub := DiffReviewSubmissionRecord{
		ID:              submissionID,
		TaskID:          req.TaskID,
		Status:          "queued",
		TargetSessionID: snap.AnchorSessionID,
		MessageID:       messageID,
		Note:            req.Note,
		Payload:         result.Payload,
		Truncated:       result.Truncated,
		Error:           "",
	}
	if err := s.repo.CreateDiffReviewSubmission(ctx, CreateDiffReviewSubmissionInput{
		Submission: sub,
		Items:      items,
	}); err != nil {
		return DiffReviewSubmissionRecord{}, nil, err
	}
	// 读回以获得 seq/created_at。
	stored, err := s.repo.GetDiffReviewSubmission(ctx, submissionID)
	if err != nil {
		return DiffReviewSubmissionRecord{}, nil, err
	}
	return stored, items, nil
}

// ListQueue 列出任务队列（queued/sending 按 seq 升序）+ items。
func (s *Service) ListQueue(ctx context.Context, taskID string) ([]SubmissionView, error) {
	if err := s.checkTaskScope(ctx, taskID); err != nil {
		return nil, err
	}
	subs, err := s.repo.ListDiffReviewQueue(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return s.fillItems(ctx, subs)
}

// ListHistory 列出任务历史（sent 按 sent_at DESC seq DESC）+ items。
func (s *Service) ListHistory(ctx context.Context, taskID string) ([]SubmissionView, error) {
	if err := s.checkTaskScope(ctx, taskID); err != nil {
		return nil, err
	}
	subs, err := s.repo.ListDiffReviewHistory(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return s.fillItems(ctx, subs)
}

// ListFailures 列出任务失败（failed/delivery_unknown 按 created_at DESC seq DESC）+ items。
func (s *Service) ListFailures(ctx context.Context, taskID string) ([]SubmissionView, error) {
	if err := s.checkTaskScope(ctx, taskID); err != nil {
		return nil, err
	}
	subs, err := s.repo.ListDiffReviewFailures(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return s.fillItems(ctx, subs)
}

// CancelSubmission 撤回提交（仅 queued，design.md D2/spec 排队中撤回）。
// 非 queued → ErrInvalidState；不存在 → ErrSubmissionNotFound。
//
// F4：先做归属校验。GetDiffReviewSubmission 取行校验 rec.TaskID==taskID，
// 归属不符统一 not_found（零撤回副作用，不泄露跨任务提交存在性）。
func (s *Service) CancelSubmission(ctx context.Context, taskID, submissionID string) error {
	if err := s.checkTaskScope(ctx, taskID); err != nil {
		return err
	}
	// F4：归属校验。先取行确认属于本任务，归属不符统一 not_found（零撤回副作用）。
	rec, gerr := s.repo.GetDiffReviewSubmission(ctx, submissionID)
	if gerr != nil {
		if errors.Is(gerr, ErrSubmissionNotFound) {
			return ErrSubmissionNotFound
		}
		return gerr
	}
	if rec.TaskID != taskID {
		return ErrSubmissionNotFound
	}
	matched, err := s.repo.CancelDiffReviewSubmission(ctx, submissionID)
	if err != nil {
		return err
	}
	if !matched {
		// 非 queued（已 sending/sent/failed/delivery_unknown）→ invalid_state。
		return ErrInvalidState
	}
	return nil
}

// DeleteSubmission 终态删除（仅 sent/failed/delivery_unknown，design.md D2/spec 删除历史）。
// 非终态 → ErrInvalidState；不存在 → ErrSubmissionNotFound。
//
// F4：先做归属校验。GetDiffReviewSubmission 取行校验 rec.TaskID==taskID，
// 归属不符统一 not_found（零删除副作用，不泄露跨任务提交存在性）。
func (s *Service) DeleteSubmission(ctx context.Context, taskID, submissionID string) error {
	if err := s.checkTaskScope(ctx, taskID); err != nil {
		return err
	}
	// F4：归属校验。先取行确认属于本任务，归属不符统一 not_found（零删除副作用）。
	rec, gerr := s.repo.GetDiffReviewSubmission(ctx, submissionID)
	if gerr != nil {
		if errors.Is(gerr, ErrSubmissionNotFound) {
			return ErrSubmissionNotFound
		}
		return gerr
	}
	if rec.TaskID != taskID {
		return ErrSubmissionNotFound
	}
	matched, err := s.repo.DeleteDiffReviewSubmission(ctx, submissionID)
	if err != nil {
		return err
	}
	if !matched {
		// 非终态（queued/sending）→ invalid_state。
		return ErrInvalidState
	}
	return nil
}

// fillItems 为每个 submission 填充 items 快照。
func (s *Service) fillItems(ctx context.Context, subs []DiffReviewSubmissionRecord) ([]SubmissionView, error) {
	views := make([]SubmissionView, len(subs))
	for i, sub := range subs {
		items, err := s.repo.ListDiffReviewSubmissionItems(ctx, sub.ID)
		if err != nil {
			return nil, err
		}
		views[i] = SubmissionView{Submission: sub, Items: items}
	}
	return views, nil
}

// validateSubmissionRequest 执行 CreateSubmission 的纯领域校验（F8/D8：无副作用、无 port 调用，
// 先于任何 port 调用完成）。任一失败零副作用。
//   - annotations 非空（D2/D8）。
//   - annotations ≤500 项（D8）。
//   - id 不重复（D8，重复 MUST 在任何落库/组装前以 invalid_input 拒绝）。
//   - revision 1..MaxInt64（D8，JSON 整数范围失败在任何 task/store/diff 读取前返回 invalid_input）。
//   - note ≤65536 UTF-8 bytes（D8）。
func validateSubmissionRequest(req CreateSubmissionRequest) error {
	if len(req.Annotations) == 0 {
		return ErrEmptySubmission
	}
	if len(req.Annotations) > maxSubmissionAnnotations {
		return ErrTooManyAnnotations
	}
	if err := validateSubmissionItems(req.Annotations); err != nil {
		return err
	}
	if len(req.Note) > maxTextFieldBytes {
		return ErrNoteTooLarge
	}
	return nil
}

// maxSubmissionAnnotations 为提交批注条目上限（design.md D8 line 250：annotations ≤500）。
const maxSubmissionAnnotations = 500

// validateSubmissionItems 校验 id 不重复 + revision 合法（1..MaxInt64）。
func validateSubmissionItems(items []SubmissionItemRequest) error {
	seen := map[string]bool{}
	for _, it := range items {
		if it.ID == "" {
			return ErrInvalidAnnotationID
		}
		if seen[it.ID] {
			return ErrDuplicateAnnotationID
		}
		seen[it.ID] = true
		if it.Revision < 1 {
			return ErrInvalidAnnotationRevision
		}
		// D8：revision 1..MaxInt64（int 平台为 64 位时 math.MaxInt64）。
		if int64(it.Revision) > maxRevision {
			return ErrInvalidAnnotationRevision
		}
	}
	return nil
}

// classifyBatchIDs 执行批次 id 分类优先级（design.md D2：与数组顺序无关）。
// 任一 id 存在但属其他任务 → ErrCrossTaskAnnotation（invalid_input）。
// 本任务范围内缺失/revision 不符在此不判定（由 selectBatchAnnotations 判定 conflict）。
// F6：仅明确缺失（ErrAnnotationNotFound）归入「本任务范围内缺失」；DB 故障原样传播，
// 不吞成缺失（真实 DB 故障不应误判为用户端 conflict）。
func (s *Service) classifyBatchIDs(ctx context.Context, taskID string, items []SubmissionItemRequest) error {
	for _, it := range items {
		rec, err := s.repo.GetDiffAnnotation(ctx, it.ID)
		if err != nil {
			if errors.Is(err, ErrAnnotationNotFound) {
				// 明确缺失 → 本任务范围内缺失，由 selectBatchAnnotations 判定 conflict。
				continue
			}
			// DB 故障原样传播。
			return err
		}
		if rec.TaskID != taskID {
			return ErrCrossTaskAnnotation
		}
	}
	return nil
}

// selectBatchAnnotations 选出本任务范围内存在且 revision 匹配的批注（design.md D2）。
// 任一 id 在本任务范围内不存在或 revision 不符 → ErrRevisionConflict（conflict 409）。
func (s *Service) selectBatchAnnotations(ctx context.Context, taskID string, items []SubmissionItemRequest, existingByID map[string]DiffAnnotationRecord) ([]DiffAnnotationRecord, error) {
	selected := make([]DiffAnnotationRecord, len(items))
	for i, it := range items {
		rec, ok := existingByID[it.ID]
		if !ok {
			return nil, ErrRevisionConflict
		}
		if rec.Revision != it.Revision {
			return nil, ErrRevisionConflict
		}
		selected[i] = rec
	}
	return selected, nil
}

// buildSubmissionItems 从批注记录构建提交快照条目。
func buildSubmissionItems(submissionID string, anns []DiffAnnotationRecord) []DiffReviewSubmissionItemRecord {
	items := make([]DiffReviewSubmissionItemRecord, len(anns))
	for i, a := range anns {
		items[i] = DiffReviewSubmissionItemRecord{
			SubmissionID:       submissionID,
			AnnotationID:       a.ID,
			AnnotationRevision: a.Revision,
			Path:               a.Path,
			Side:               a.Side,
			Ref:                a.Ref,
			Untracked:          a.Untracked,
			StartLine:          a.StartLine,
			EndLine:            a.EndLine,
			SnapshotStartLine:  a.SnapshotStartLine,
			Snapshot:           a.Snapshot,
			Comment:            a.Comment,
		}
	}
	return items
}

// newMessageID 生成 messageID（design.md D1：msg_ + submission UUID 去连字符小写）。
func newMessageID(submissionID string) string {
	return "msg_" + strings.ReplaceAll(strings.ToLower(submissionID), "-", "")
}

// newUUID 生成随机 UUID v4（hex 32 字符，含连字符 36 字符）。
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	), nil
}

// --- 额外 domain 错误（提交用例） ---

// ErrInvalidState 操作状态非法（撤回非 queued / 删除非终态）。
var ErrInvalidState = errors.New("diffreview: invalid submission state for operation")

// ErrSubmissionNotFound 提交不存在。
var ErrSubmissionNotFound = errors.New("diffreview: submission not found")

// ErrInvalidAnnotationID 提交批次内 id 为空（D8）。
var ErrInvalidAnnotationID = errors.New("diffreview: empty annotation id in submission")

// ErrTooManyAnnotations 提交批注条目超 500 上限（design.md D8）。
var ErrTooManyAnnotations = errors.New("diffreview: submission annotations exceed 500 limit")

// ErrNoteTooLarge note 超 65536 UTF-8 bytes 上限（design.md D8）。
var ErrNoteTooLarge = errors.New("diffreview: submission note exceeds 65536 UTF-8 bytes limit")

// maxRevision 为 revision 上限（design.md D8：1..MaxInt64）。
const maxRevision = math.MaxInt64
