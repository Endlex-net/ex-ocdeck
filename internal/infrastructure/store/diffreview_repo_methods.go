// diffreview_repo_methods.go 实现 DiffReviewRepoAdapter 的全部 DiffReviewRepository 方法
//（design.md D9，适配 diff_review_queries.go 已存在原语，不改原语语义）。
//
// store 行类型与 application domain 记录类型同形（字段逐字对应），adapter 做显式逐字段映射，
// 不用类型别名跨层共享（design.md D9 边界约束）。唯一差异：submission.SentAt 在 store 侧为
// sql.NullInt64（未发送时 Valid=false），domain 侧为 int64（0 表示未发送）。
package store

import (
	"context"
	"database/sql"
	"errors"

	"ocdeck/internal/application/diffreview"
)

// annotationRowToRecord 将 store 行映射为 domain 记录（字段逐字对应）。
func annotationRowToRecord(r DiffAnnotationRow) diffreview.DiffAnnotationRecord {
	return diffreview.DiffAnnotationRecord{
		ID:                r.ID,
		TaskID:            r.TaskID,
		Path:              r.Path,
		Side:              r.Side,
		Ref:               r.Ref,
		Untracked:         r.Untracked,
		StartLine:         r.StartLine,
		EndLine:           r.EndLine,
		SnapshotStartLine: r.SnapshotStartLine,
		Snapshot:          r.Snapshot,
		SnapshotLineCount: r.SnapshotLineCount,
		Comment:           r.Comment,
		Revision:          r.Revision,
		CreatedAt:         r.CreatedAt,
		UpdatedAt:         r.UpdatedAt,
	}
}

// submissionRowToRecord 将 store 行映射为 domain 记录。SentAt sql.NullInt64→int64（0=未发送）。
func submissionRowToRecord(r DiffReviewSubmissionRow) diffreview.DiffReviewSubmissionRecord {
	sentAt := int64(0)
	if r.SentAt.Valid {
		sentAt = r.SentAt.Int64
	}
	return diffreview.DiffReviewSubmissionRecord{
		Seq:             r.Seq,
		ID:              r.ID,
		TaskID:          r.TaskID,
		Status:          r.Status,
		TargetSessionID: r.TargetSessionID,
		MessageID:       r.MessageID,
		Note:            r.Note,
		Payload:         r.Payload,
		Truncated:       r.Truncated,
		Error:           r.Error,
		CreatedAt:       r.CreatedAt,
		SentAt:          sentAt,
	}
}

// itemRowToRecord 将 store 行映射为 domain 记录（字段逐字对应）。
func itemRowToRecord(r DiffReviewSubmissionItemRow) diffreview.DiffReviewSubmissionItemRecord {
	return diffreview.DiffReviewSubmissionItemRecord{
		SubmissionID:       r.SubmissionID,
		AnnotationID:       r.AnnotationID,
		AnnotationRevision: r.AnnotationRevision,
		Path:               r.Path,
		Side:               r.Side,
		Ref:                r.Ref,
		Untracked:          r.Untracked,
		StartLine:          r.StartLine,
		EndLine:            r.EndLine,
		SnapshotStartLine:  r.SnapshotStartLine,
		Snapshot:           r.Snapshot,
		Comment:            r.Comment,
	}
}

// CreateDiffAnnotation 调 store 原语 CreateDiffAnnotation（revision 初始 1，INSERT...RETURNING 返回完整行）。
func (a *DiffReviewRepoAdapter) CreateDiffAnnotation(ctx context.Context, in diffreview.CreateDiffAnnotationInput) (diffreview.DiffAnnotationRecord, error) {
	row, err := a.q.CreateDiffAnnotation(ctx, DiffAnnotationRow{
		ID:                in.ID,
		TaskID:            in.TaskID,
		Path:              in.Path,
		Side:              in.Side,
		Ref:               in.Ref,
		Untracked:         in.Untracked,
		StartLine:         in.StartLine,
		EndLine:           in.EndLine,
		SnapshotStartLine: in.SnapshotStartLine,
		Snapshot:          in.Snapshot,
		SnapshotLineCount: in.SnapshotLineCount,
		Comment:           in.Comment,
	})
	if err != nil {
		return diffreview.DiffAnnotationRecord{}, err
	}
	return annotationRowToRecord(row), nil
}

// UpdateDiffAnnotationComment 调 store 原语三态结果（含完整行），透传不改语义。
func (a *DiffReviewRepoAdapter) UpdateDiffAnnotationComment(ctx context.Context, id, comment string) (diffreview.CommentUpdateResult, error) {
	u, err := a.q.UpdateDiffAnnotationComment(ctx, id, comment)
	if err != nil {
		return diffreview.CommentUpdateResult{}, err
	}
	return diffreview.CommentUpdateResult{Matched: u.Matched, Changed: u.Changed, Revision: u.Revision, Record: annotationRowToRecord(u.Row)}, nil
}

// DeleteDiffAnnotation 调 store 原语，返回 affected（0=行不存在，幂等成功）。
func (a *DiffReviewRepoAdapter) DeleteDiffAnnotation(ctx context.Context, id string) (int, error) {
	return a.q.DeleteDiffAnnotation(ctx, id)
}

// ListDiffAnnotationsByTask 调 store 原语，映射行→domain 记录。
func (a *DiffReviewRepoAdapter) ListDiffAnnotationsByTask(ctx context.Context, taskID string) ([]diffreview.DiffAnnotationRecord, error) {
	rows, err := a.q.ListDiffAnnotationsByTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	out := make([]diffreview.DiffAnnotationRecord, len(rows))
	for i, r := range rows {
		out[i] = annotationRowToRecord(r)
	}
	return out, nil
}

// GetDiffAnnotation 调 store 原语。缺行 sql.ErrNoRows 映射为 diffreview.ErrAnnotationNotFound
//（F6：调用方据此区分「明确缺失」与 DB 故障，DB 故障原样传播不吞成缺失）。
func (a *DiffReviewRepoAdapter) GetDiffAnnotation(ctx context.Context, id string) (diffreview.DiffAnnotationRecord, error) {
	r, err := a.q.GetDiffAnnotation(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return diffreview.DiffAnnotationRecord{}, diffreview.ErrAnnotationNotFound
		}
		return diffreview.DiffAnnotationRecord{}, err
	}
	return annotationRowToRecord(r), nil
}

// CreateDiffReviewSubmission 调 store 原语（单事务 + items 快照 + revision 复核 + RETURNING 完整行）。
// F6：store 事务内 revision 复核 sentinel ErrDiffReviewRevisionConflict 映射为
// diffreview.ErrRevisionConflict（domain 错误），真实竞态返回 conflict(409)。
// G1：store 返回 INSERT...RETURNING 的完整行（含 seq/created_at），调用方不再二次读取。
func (a *DiffReviewRepoAdapter) CreateDiffReviewSubmission(ctx context.Context, in diffreview.CreateDiffReviewSubmissionInput) (diffreview.DiffReviewSubmissionRecord, error) {
	items := make([]DiffReviewSubmissionItemRow, len(in.Items))
	for i, it := range in.Items {
		items[i] = DiffReviewSubmissionItemRow{
			SubmissionID:       it.SubmissionID,
			AnnotationID:       it.AnnotationID,
			AnnotationRevision: it.AnnotationRevision,
			Path:               it.Path,
			Side:               it.Side,
			Ref:                it.Ref,
			Untracked:          it.Untracked,
			StartLine:          it.StartLine,
			EndLine:            it.EndLine,
			SnapshotStartLine:  it.SnapshotStartLine,
			Snapshot:           it.Snapshot,
			Comment:            it.Comment,
		}
	}
	stored, err := a.q.CreateDiffReviewSubmission(ctx, CreateDiffReviewSubmissionInput{
		Submission: DiffReviewSubmissionRow{
			Seq:             in.Submission.Seq,
			ID:              in.Submission.ID,
			TaskID:          in.Submission.TaskID,
			Status:          in.Submission.Status,
			TargetSessionID: in.Submission.TargetSessionID,
			MessageID:       in.Submission.MessageID,
			Note:            in.Submission.Note,
			Payload:         in.Submission.Payload,
			Truncated:       in.Submission.Truncated,
			Error:           in.Submission.Error,
			CreatedAt:       in.Submission.CreatedAt,
		},
		Items: items,
	})
	if err != nil {
		// F6：事务内 revision 复核 sentinel 映射为 domain ErrRevisionConflict。
		if errors.Is(err, ErrDiffReviewRevisionConflict) {
			return diffreview.DiffReviewSubmissionRecord{}, diffreview.ErrRevisionConflict
		}
		return diffreview.DiffReviewSubmissionRecord{}, err
	}
	return submissionRowToRecord(stored), nil
}

// ListDiffReviewQueue 调 store 原语，映射行→domain 记录。
func (a *DiffReviewRepoAdapter) ListDiffReviewQueue(ctx context.Context, taskID string) ([]diffreview.DiffReviewSubmissionRecord, error) {
	rows, err := a.q.ListDiffReviewQueue(ctx, taskID)
	if err != nil {
		return nil, err
	}
	out := make([]diffreview.DiffReviewSubmissionRecord, len(rows))
	for i, r := range rows {
		out[i] = submissionRowToRecord(r)
	}
	return out, nil
}

// ListDiffReviewHistory 调 store 原语，映射行→domain 记录。
func (a *DiffReviewRepoAdapter) ListDiffReviewHistory(ctx context.Context, taskID string) ([]diffreview.DiffReviewSubmissionRecord, error) {
	rows, err := a.q.ListDiffReviewHistory(ctx, taskID)
	if err != nil {
		return nil, err
	}
	out := make([]diffreview.DiffReviewSubmissionRecord, len(rows))
	for i, r := range rows {
		out[i] = submissionRowToRecord(r)
	}
	return out, nil
}

// ListDiffReviewFailures 调 store 原语，映射行→domain 记录。
func (a *DiffReviewRepoAdapter) ListDiffReviewFailures(ctx context.Context, taskID string) ([]diffreview.DiffReviewSubmissionRecord, error) {
	rows, err := a.q.ListDiffReviewFailures(ctx, taskID)
	if err != nil {
		return nil, err
	}
	out := make([]diffreview.DiffReviewSubmissionRecord, len(rows))
	for i, r := range rows {
		out[i] = submissionRowToRecord(r)
	}
	return out, nil
}

// ListDiffReviewSubmissionPartitions 调 store 原语（同一 SQLite 读事务的一致快照），
// 映射行→domain 记录；排序契约（queue seq ASC / history sent_at DESC,seq DESC /
// failures created_at DESC,seq DESC）由 store 查询保证，adapter 不重排。
func (a *DiffReviewRepoAdapter) ListDiffReviewSubmissionPartitions(ctx context.Context, taskID string) (diffreview.SubmissionPartitions, error) {
	parts, err := a.q.ListDiffReviewSubmissionPartitions(ctx, taskID)
	if err != nil {
		return diffreview.SubmissionPartitions{}, err
	}
	mapViews := func(rows []DiffReviewSubmissionWithItems) []diffreview.SubmissionView {
		out := make([]diffreview.SubmissionView, len(rows))
		for i, r := range rows {
			items := make([]diffreview.DiffReviewSubmissionItemRecord, len(r.Items))
			for j, it := range r.Items {
				items[j] = itemRowToRecord(it)
			}
			out[i] = diffreview.SubmissionView{Submission: submissionRowToRecord(r.Submission), Items: items}
		}
		return out
	}
	return diffreview.SubmissionPartitions{
		Queue:    mapViews(parts.Queue),
		History:  mapViews(parts.History),
		Failures: mapViews(parts.Failures),
	}, nil
}

// ListDiffReviewSubmissionItems 调 store 原语，映射行→domain 记录。
func (a *DiffReviewRepoAdapter) ListDiffReviewSubmissionItems(ctx context.Context, submissionID string) ([]diffreview.DiffReviewSubmissionItemRecord, error) {
	rows, err := a.q.ListDiffReviewSubmissionItems(ctx, submissionID)
	if err != nil {
		return nil, err
	}
	out := make([]diffreview.DiffReviewSubmissionItemRecord, len(rows))
	for i, r := range rows {
		out[i] = itemRowToRecord(r)
	}
	return out, nil
}

// GetDiffReviewSubmission 调 store 原语。缺行 sql.ErrNoRows 映射为 diffreview.ErrSubmissionNotFound
//（F6：调用方 errors.Is 区分缺失与 DB 故障，DB 故障原样传播）。
func (a *DiffReviewRepoAdapter) GetDiffReviewSubmission(ctx context.Context, id string) (diffreview.DiffReviewSubmissionRecord, error) {
	r, err := a.q.GetDiffReviewSubmission(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return diffreview.DiffReviewSubmissionRecord{}, diffreview.ErrSubmissionNotFound
		}
		return diffreview.DiffReviewSubmissionRecord{}, err
	}
	return submissionRowToRecord(r), nil
}

// CASDiffReviewSubmission 调 store 原语。matched bool + ErrDiffReviewIllegalCAS 透传。
func (a *DiffReviewRepoAdapter) CASDiffReviewSubmission(ctx context.Context, id, from, to, errorText string) (bool, error) {
	return a.q.CASDiffReviewSubmission(ctx, id, from, to, errorText)
}

// CompleteDiffReviewSentCleanup 调 store 原语（sending→sent + 删批注同事务）。
func (a *DiffReviewRepoAdapter) CompleteDiffReviewSentCleanup(ctx context.Context, submissionID string) (bool, error) {
	return a.q.CompleteDiffReviewSentCleanup(ctx, submissionID)
}

// CancelDiffReviewSubmission 调 store 原语（仅 queued DELETE）。
func (a *DiffReviewRepoAdapter) CancelDiffReviewSubmission(ctx context.Context, id string) (bool, error) {
	return a.q.CancelDiffReviewSubmission(ctx, id)
}

// DeleteDiffReviewSubmission 调 store 原语（仅终态 DELETE）。
func (a *DiffReviewRepoAdapter) DeleteDiffReviewSubmission(ctx context.Context, id string) (bool, error) {
	return a.q.DeleteDiffReviewSubmission(ctx, id)
}

// ConvergeDiffReviewOnStartup 调 store 原语（单事务 sending→delivery_unknown + 固定 error）。
func (a *DiffReviewRepoAdapter) ConvergeDiffReviewOnStartup(ctx context.Context) (int64, error) {
	return a.q.ConvergeDiffReviewOnStartup(ctx)
}