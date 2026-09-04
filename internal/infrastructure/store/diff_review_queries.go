// diff_review_queries.go 实现 diff-review-workbench D2/D3 的查询层原语。
//
// 设计依据 design.md D2（提交队列 CAS 状态机 / 至多一次投递）与 D3（持久化 schema）。
// 本文件仅提供存储原语，不含调度/编排语义（归 lane 3）。
//
// 约定：
//   - 时间戳沿用秒级 Unix（nowUnix）；revision 为每次实变严格 +1 的整数，版本比对唯一依据。
//   - 状态字面值：queued|sending|sent|failed|delivery_unknown；撤回=DELETE 行不保留 cancelled 记录。
//   - CAS 转移用条件 UPDATE（WHERE status=expected），返回 affected 行数供调用方判 matched。
//   - sent 清理事务（sending→sent + 按 id+revision 删批注）为唯一不可拆分事务原语，
//     不存在独立的 MarkSent 方法（D2/D3）。
//   - 全局启动收敛：单事务批量 UPDATE sending→delivery_unknown + 固定 error（D2 重启恢复①）。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// DiffAnnotationStatus 是批注锚定状态的存储侧占位（active/stale 由后端计算不落库）。
// 此处仅保留语义说明，不引入类型，避免误用。

// DiffReviewSubmissionStatus 为 diff_review_submissions.status 的合法字面值（D2/D3）。
// 撤回 = DELETE 行，不保留 cancelled 记录（故无 cancelled 常量）。
const (
	DiffReviewStatusQueued          = "queued"
	DiffReviewStatusSending         = "sending"
	DiffReviewStatusSent            = "sent"
	DiffReviewStatusFailed          = "failed"
	DiffReviewStatusDeliveryUnknown = "delivery_unknown"
)

// diffReviewStartupConvergeError 为 D2 重启恢复①的固定 error 文案（唯一字面值）。
const diffReviewStartupConvergeError = "delivery unknown after restart"

// DiffAnnotationRow diff_annotations 表行映射（D3）。
// ref/untracked 为来源元组（ref 空表示 index/untracked；untracked=true 时 ref 必空、side 必 new，见 D4）。
// revision 为版本比对唯一依据（每次实变严格 +1）。
type DiffAnnotationRow struct {
	ID                string
	TaskID            string
	Path              string
	Side              string // 'old' | 'new'
	Ref               string
	Untracked         bool
	StartLine         int
	EndLine           int
	SnapshotStartLine int
	Snapshot          string
	SnapshotLineCount int
	Comment           string
	Revision          int
	CreatedAt         int64
	UpdatedAt         int64
}

// DiffReviewSubmissionRow diff_review_submissions 表行映射（D3）。
// Seq 为全局 AUTOINCREMENT 入队序（FIFO 唯一依据）；SentAt 仅 sent 时有效。
type DiffReviewSubmissionRow struct {
	Seq             int64
	ID              string
	TaskID          string
	Status          string
	TargetSessionID string
	MessageID       string
	Note            string
	Payload         string
	Truncated       bool
	Error           string
	CreatedAt       int64
	SentAt          sql.NullInt64
}

// DiffReviewSubmissionItemRow diff_review_submission_items 表行映射（D3 不可变快照）。
// AnnotationRevision 为快照时批注 revision（sent 清理比对用）。
type DiffReviewSubmissionItemRow struct {
	SubmissionID       string
	AnnotationID       string
	AnnotationRevision int
	Path               string
	Side               string
	Ref                string
	Untracked          bool
	StartLine          int
	EndLine            int
	SnapshotStartLine  int
	Snapshot           string
	Comment            string
}

// DiffReviewSubmissionWithItems 是分区快照中的一条提交及其 items。
// Items 始终非 nil（可为空切片）。
type DiffReviewSubmissionWithItems struct {
	Submission DiffReviewSubmissionRow
	Items      []DiffReviewSubmissionItemRow
}

// DiffReviewSubmissionPartitions 是任务三分区的一致快照（同一 SQLite 读事务）。
// 排序契约（design.md D8）：queue seq ASC；history sent_at DESC, seq DESC；
// failures created_at DESC, seq DESC。
type DiffReviewSubmissionPartitions struct {
	Queue    []DiffReviewSubmissionWithItems
	History  []DiffReviewSubmissionWithItems
	Failures []DiffReviewSubmissionWithItems
}

// CreateDiffAnnotation 插入批注行（revision 初始为 1）并以 RETURNING 返回完整行。
// created_at/updated_at 用 nowUnix。F12：INSERT 与行读取必须在同一语句完成——调用方
// 不得在写入提交后再做必需的二次读取（避免写成功但响应失败致客户端重试重复创建）。
func (q *Queries) CreateDiffAnnotation(ctx context.Context, r DiffAnnotationRow) (DiffAnnotationRow, error) {
	now := nowUnix()
	row := q.db.QueryRowContext(ctx,
		`INSERT INTO diff_annotations
		   (id, task_id, path, side, ref, untracked, start_line, end_line,
		    snapshot_start_line, snapshot, snapshot_line_count, comment, revision, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 RETURNING id, task_id, path, side, ref, untracked, start_line, end_line,
		           snapshot_start_line, snapshot, snapshot_line_count, comment, revision, created_at, updated_at`,
		r.ID, r.TaskID, r.Path, r.Side, r.Ref, boolToInt(r.Untracked),
		r.StartLine, r.EndLine, r.SnapshotStartLine, r.Snapshot, r.SnapshotLineCount,
		r.Comment, 1, now, now)
	return scanDiffAnnotationRow(row)
}

// DiffAnnotationCommentUpdate 表达编辑评论写入的三态结果。
//
//   - Matched: WHERE 命中行（id 存在）。
//   - Changed: 命中行且评论发生真实变更（同值命中时 Changed=false，revision 不递增）。
//   - Revision: 真实变更后的新 revision（>0）；同值命中时为命中行当前 revision；未命中时为 0。
//   - Row: 命中行的完整记录（F12：RETURNING/同事务 SELECT 取得，调用方不得再二次读取）。
//
// 同值原子 no-op：SQL WHERE 排除同值（comment != ?），仅真实变更才递增 revision 与 updated_at。
// 调用方据此区分「不存在」「同值无变化」「已更新」（D3：每次实变才 +1）。
type DiffAnnotationCommentUpdate struct {
	Matched  bool
	Changed  bool
	Revision int
	Row      DiffAnnotationRow
}

// UpdateDiffAnnotationComment 更新批注评论，仅真实变更（comment != 当前值）才 revision 严格 +1（D3：每次实变 +1）。
// 同值命中 → Matched=true, Changed=false, Revision=当前值（revision 不递增，updated_at 不推进）。
// 行不存在 → Matched=false, Changed=false, Revision=0（调用方按 not_found 处理）。
// updated_at 用 nowUnix（秒级，同秒实变不推进不影响 revision 比对）。
//
// 分类过程在 runTx 单事务内完成：真实变更用 UPDATE ... RETURNING 全列直接获取本次实变后的
// 完整行（F12：返回值属于本次写入，调用方不得在提交后再做必需的二次读取）；RETURNING 无行命中时
// 在同一事务内 SELECT 全列区分同值（行存在）与不存在。SQLite 单写者事务保证原子性。
func (q *Queries) UpdateDiffAnnotationComment(ctx context.Context, id, comment string) (DiffAnnotationCommentUpdate, error) {
	now := nowUnix()
	return runTx(ctx, q, func(qx *Queries) (DiffAnnotationCommentUpdate, error) {
		// 真实变更：WHERE 排除同值，RETURNING 直接返回本次实变后的完整行。
		row := qx.db.QueryRowContext(ctx,
			`UPDATE diff_annotations
			    SET comment = ?, revision = revision + 1, updated_at = ?
			 WHERE id = ? AND comment != ?
			 RETURNING id, task_id, path, side, ref, untracked, start_line, end_line,
			           snapshot_start_line, snapshot, snapshot_line_count, comment, revision, created_at, updated_at`,
			comment, now, id, comment)
		updated, err := scanDiffAnnotationRow(row)
		if err == nil {
			return DiffAnnotationCommentUpdate{Matched: true, Changed: true, Revision: updated.Revision, Row: updated}, nil
		}
		if err != sql.ErrNoRows {
			return DiffAnnotationCommentUpdate{}, err
		}
		// RETURNING 无行命中：行不存在 或 同值命中。在同一事务内 SELECT 全列区分。
		cur, serr := scanDiffAnnotationRow(qx.db.QueryRowContext(ctx,
			`SELECT id, task_id, path, side, ref, untracked, start_line, end_line,
			        snapshot_start_line, snapshot, snapshot_line_count, comment, revision, created_at, updated_at
			 FROM diff_annotations WHERE id = ?`, id))
		if serr == sql.ErrNoRows {
			return DiffAnnotationCommentUpdate{Matched: false, Changed: false, Revision: 0}, nil
		}
		if serr != nil {
			return DiffAnnotationCommentUpdate{}, serr
		}
		// 行存在但同值 → Matched=true, Changed=false, Revision=当前值。
		return DiffAnnotationCommentUpdate{Matched: true, Changed: false, Revision: cur.Revision, Row: cur}, nil
	})
}

// DeleteDiffAnnotation 按 id 删除批注。返回 affected（0=行不存在，幂等成功）。
func (q *Queries) DeleteDiffAnnotation(ctx context.Context, id string) (int, error) {
	res, err := q.db.ExecContext(ctx, `DELETE FROM diff_annotations WHERE id = ?`, id)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// ListDiffAnnotationsByTask 列出任务下全部活动批注（按 created_at ASC、id ASC 稳定排序）。
// stale 由后端列表读取时计算，不落库（D4）；此处仅返回存储行。
func (q *Queries) ListDiffAnnotationsByTask(ctx context.Context, taskID string) ([]DiffAnnotationRow, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT id, task_id, path, side, ref, untracked, start_line, end_line,
		        snapshot_start_line, snapshot, snapshot_line_count, comment, revision, created_at, updated_at
		 FROM diff_annotations WHERE task_id = ?
		 ORDER BY created_at ASC, id ASC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDiffAnnotationRows(rows)
}

// GetDiffAnnotation 按 id 读取单条批注。缺行返回 sql.ErrNoRows。
func (q *Queries) GetDiffAnnotation(ctx context.Context, id string) (DiffAnnotationRow, error) {
	row := q.db.QueryRowContext(ctx,
		`SELECT id, task_id, path, side, ref, untracked, start_line, end_line,
		        snapshot_start_line, snapshot, snapshot_line_count, comment, revision, created_at, updated_at
		 FROM diff_annotations WHERE id = ?`, id)
	return scanDiffAnnotationRow(row)
}

// CreateDiffReviewSubmissionInput 为创建提交的入参（单事务写 submission + items + revision 复核）。
// Items 为不可变快照（annotation_id + annotation_revision 为复核/清理比对用）。
// revision 复核直接从每个 item 的 AnnotationID/AnnotationRevision 派生，事务内逐条 SELECT 校验
// 当前活动批注的 revision 仍等于快照 revision（与准入同一判定，D2 落库条）。任一不符 → 返回
// ErrDiffReviewRevisionConflict，整事务回滚（零落库、零清理、零调度）。
// submission_id 由函数内部按 Submission.ID 写入每个 item，调用方无需在 item 上设置 SubmissionID。
type CreateDiffReviewSubmissionInput struct {
	Submission DiffReviewSubmissionRow
	Items      []DiffReviewSubmissionItemRow
}

// ErrDiffReviewRevisionConflict 复核失败（事务内批注 revision 已变化或被删除），
// 调用方统一映射 conflict(409)（不区分「从未存在」与「预览后删除」，D2）。
var ErrDiffReviewRevisionConflict = errors.New("store: diff review submission revision conflict")

// CreateDiffReviewSubmission 单事务创建提交：写 submissions 行（status=queued）+ items 快照，
// 并在事务内按每个 item 的 annotation_id+revision 复核活动批注版本（D2 落库条）。
// 复核失败 → ErrDiffReviewRevisionConflict，整事务回滚（零落库）。
// message_id UNIQUE 碰撞 → 底层 sqlite 错误透传（调用方按约束冲突处理）。
// G1：INSERT...RETURNING seq, created_at 返回完整行——调用方不得在事务提交后再做必需的
// 二次读取（避免写成功但响应失败致客户端重试产生第二条 submission/重复投递）。
func (q *Queries) CreateDiffReviewSubmission(ctx context.Context, in CreateDiffReviewSubmissionInput) (DiffReviewSubmissionRow, error) {
	if in.Submission.Status != DiffReviewStatusQueued {
		return DiffReviewSubmissionRow{}, fmt.Errorf("store: create diff review submission requires status=%q, got %q",
			DiffReviewStatusQueued, in.Submission.Status)
	}
	if in.Submission.ID == "" || in.Submission.TaskID == "" || in.Submission.MessageID == "" {
		return DiffReviewSubmissionRow{}, fmt.Errorf("store: create diff review submission requires id/task_id/message_id")
	}
	var stored DiffReviewSubmissionRow
	txErr := withTxQueries(ctx, q.db, func(qx *Queries) error {
		// 事务内复核：逐条 SELECT 当前活动批注 revision，与 item 快照 revision 比对。
		// 不区分「不存在」与「revision 不符」——统一视为 conflict（D2）。
		for _, it := range in.Items {
			var curRev int
			err := qx.db.QueryRowContext(ctx,
				`SELECT revision FROM diff_annotations WHERE id = ?`, it.AnnotationID).Scan(&curRev)
			if err != nil {
				if err == sql.ErrNoRows {
					return ErrDiffReviewRevisionConflict
				}
				return err
			}
			if curRev != it.AnnotationRevision {
				return ErrDiffReviewRevisionConflict
			}
		}
		now := nowUnix()
		if err := qx.db.QueryRowContext(ctx,
			`INSERT INTO diff_review_submissions
			   (id, task_id, status, target_session_id, message_id, note, payload, truncated, error, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 RETURNING seq, created_at`,
			in.Submission.ID, in.Submission.TaskID, in.Submission.Status,
			in.Submission.TargetSessionID, in.Submission.MessageID, in.Submission.Note,
			in.Submission.Payload, boolToInt(in.Submission.Truncated), "", now).Scan(&stored.Seq, &stored.CreatedAt); err != nil {
			return err
		}
		for _, it := range in.Items {
			if _, err := qx.db.ExecContext(ctx,
				`INSERT INTO diff_review_submission_items
				   (submission_id, annotation_id, annotation_revision, path, side, ref, untracked,
				    start_line, end_line, snapshot_start_line, snapshot, comment)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				in.Submission.ID, it.AnnotationID, it.AnnotationRevision, it.Path, it.Side, it.Ref,
				boolToInt(it.Untracked), it.StartLine, it.EndLine, it.SnapshotStartLine, it.Snapshot,
				it.Comment); err != nil {
				return err
			}
		}
		return nil
	})
	if txErr != nil {
		return DiffReviewSubmissionRow{}, txErr
	}
	// RETURNING 已取 seq/created_at；其余字段与输入一致（status=queued、error=""、sent_at NULL）。
	stored.ID = in.Submission.ID
	stored.TaskID = in.Submission.TaskID
	stored.Status = in.Submission.Status
	stored.TargetSessionID = in.Submission.TargetSessionID
	stored.MessageID = in.Submission.MessageID
	stored.Note = in.Submission.Note
	stored.Payload = in.Submission.Payload
	stored.Truncated = in.Submission.Truncated
	stored.Error = ""
	return stored, nil
}

// ListDiffReviewQueue 列出任务下队列中的提交（queued + sending），按 seq ASC（FIFO 唯一依据，D2）。
func (q *Queries) ListDiffReviewQueue(ctx context.Context, taskID string) ([]DiffReviewSubmissionRow, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT seq, id, task_id, status, target_session_id, message_id, note, payload, truncated, error, created_at, sent_at
		 FROM diff_review_submissions
		 WHERE task_id = ? AND status IN (?, ?)
		 ORDER BY seq ASC`,
		taskID, DiffReviewStatusQueued, DiffReviewStatusSending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDiffReviewSubmissionRows(rows)
}

// ListDiffReviewHistory 列出任务下已发送提交（sent），按 sent_at DESC, seq DESC（D2/D8）。
// 秒级时间戳同秒时以 seq 决胜，排序稳定。
func (q *Queries) ListDiffReviewHistory(ctx context.Context, taskID string) ([]DiffReviewSubmissionRow, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT seq, id, task_id, status, target_session_id, message_id, note, payload, truncated, error, created_at, sent_at
		 FROM diff_review_submissions
		 WHERE task_id = ? AND status = ?
		 ORDER BY sent_at DESC, seq DESC`,
		taskID, DiffReviewStatusSent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDiffReviewSubmissionRows(rows)
}

// ListDiffReviewFailures 列出任务下失败提交（failed + delivery_unknown），
// 按 created_at DESC, seq DESC（D2/D8）。秒级时间戳同秒时以 seq 决胜。
func (q *Queries) ListDiffReviewFailures(ctx context.Context, taskID string) ([]DiffReviewSubmissionRow, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT seq, id, task_id, status, target_session_id, message_id, note, payload, truncated, error, created_at, sent_at
		 FROM diff_review_submissions
		 WHERE task_id = ? AND status IN (?, ?)
		 ORDER BY created_at DESC, seq DESC`,
		taskID, DiffReviewStatusFailed, DiffReviewStatusDeliveryUnknown)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDiffReviewSubmissionRows(rows)
}

// ListDiffReviewSubmissionPartitions 在同一 SQLite 读事务内读取任务三分区 submissions 与 items。
// 保证同一 submission 不会同时出现在多个分区，且每条提交带该快照时刻的 items。
func (q *Queries) ListDiffReviewSubmissionPartitions(ctx context.Context, taskID string) (DiffReviewSubmissionPartitions, error) {
	return runTx(ctx, q, func(qx *Queries) (DiffReviewSubmissionPartitions, error) {
		subs, err := qx.listDiffReviewSubmissionsByTask(ctx, taskID)
		if err != nil {
			return DiffReviewSubmissionPartitions{}, err
		}
		itemsBySub, err := qx.listDiffReviewSubmissionItemsByTask(ctx, taskID)
		if err != nil {
			return DiffReviewSubmissionPartitions{}, err
		}
		out := DiffReviewSubmissionPartitions{
			Queue:    make([]DiffReviewSubmissionWithItems, 0),
			History:  make([]DiffReviewSubmissionWithItems, 0),
			Failures: make([]DiffReviewSubmissionWithItems, 0),
		}
		for _, sub := range subs {
			items := itemsBySub[sub.ID]
			if items == nil {
				items = []DiffReviewSubmissionItemRow{}
			}
			view := DiffReviewSubmissionWithItems{Submission: sub, Items: items}
			switch sub.Status {
			case DiffReviewStatusQueued, DiffReviewStatusSending:
				out.Queue = append(out.Queue, view)
			case DiffReviewStatusSent:
				out.History = append(out.History, view)
			case DiffReviewStatusFailed, DiffReviewStatusDeliveryUnknown:
				out.Failures = append(out.Failures, view)
			}
		}
		return out, nil
	})
}

// listDiffReviewSubmissionsByTask 读取任务全部提交，按分区排序键一次排好：
// queue（queued/sending）seq ASC；history（sent）sent_at DESC, seq DESC；
// failures（failed/delivery_unknown）created_at DESC, seq DESC。
func (q *Queries) listDiffReviewSubmissionsByTask(ctx context.Context, taskID string) ([]DiffReviewSubmissionRow, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT seq, id, task_id, status, target_session_id, message_id, note, payload, truncated, error, created_at, sent_at
		 FROM diff_review_submissions
		 WHERE task_id = ?
		 ORDER BY CASE status
		            WHEN ? THEN 0
		            WHEN ? THEN 0
		            WHEN ? THEN 1
		            WHEN ? THEN 2
		            WHEN ? THEN 2
		            ELSE 3
		          END,
		          CASE WHEN status IN (?, ?) THEN seq END ASC,
		          CASE WHEN status = ? THEN sent_at END DESC,
		          CASE WHEN status = ? THEN seq END DESC,
		          CASE WHEN status IN (?, ?) THEN created_at END DESC,
		          CASE WHEN status IN (?, ?) THEN seq END DESC`,
		taskID,
		DiffReviewStatusQueued, DiffReviewStatusSending,
		DiffReviewStatusSent,
		DiffReviewStatusFailed, DiffReviewStatusDeliveryUnknown,
		DiffReviewStatusQueued, DiffReviewStatusSending,
		DiffReviewStatusSent, DiffReviewStatusSent,
		DiffReviewStatusFailed, DiffReviewStatusDeliveryUnknown,
		DiffReviewStatusFailed, DiffReviewStatusDeliveryUnknown)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDiffReviewSubmissionRows(rows)
}

// listDiffReviewSubmissionItemsByTask 读取任务全部提交快照条目，按 submission_id、annotation_id ASC。
func (q *Queries) listDiffReviewSubmissionItemsByTask(ctx context.Context, taskID string) (map[string][]DiffReviewSubmissionItemRow, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT i.submission_id, i.annotation_id, i.annotation_revision, i.path, i.side, i.ref, i.untracked,
		        i.start_line, i.end_line, i.snapshot_start_line, i.snapshot, i.comment
		 FROM diff_review_submission_items i
		 INNER JOIN diff_review_submissions s ON s.id = i.submission_id
		 WHERE s.task_id = ?
		 ORDER BY i.submission_id ASC, i.annotation_id ASC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]DiffReviewSubmissionItemRow{}
	for rows.Next() {
		var r DiffReviewSubmissionItemRow
		var untracked int
		if err := rows.Scan(&r.SubmissionID, &r.AnnotationID, &r.AnnotationRevision, &r.Path, &r.Side,
			&r.Ref, &untracked, &r.StartLine, &r.EndLine, &r.SnapshotStartLine, &r.Snapshot, &r.Comment); err != nil {
			return nil, err
		}
		r.Untracked = untracked != 0
		out[r.SubmissionID] = append(out[r.SubmissionID], r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ListDiffReviewSubmissionItems 列出某提交的全部不可变快照条目（按 annotation_id ASC 稳定排序）。
func (q *Queries) ListDiffReviewSubmissionItems(ctx context.Context, submissionID string) ([]DiffReviewSubmissionItemRow, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT submission_id, annotation_id, annotation_revision, path, side, ref, untracked,
		        start_line, end_line, snapshot_start_line, snapshot, comment
		 FROM diff_review_submission_items
		 WHERE submission_id = ?
		 ORDER BY annotation_id ASC`, submissionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DiffReviewSubmissionItemRow
	for rows.Next() {
		var r DiffReviewSubmissionItemRow
		var untracked int
		if err := rows.Scan(&r.SubmissionID, &r.AnnotationID, &r.AnnotationRevision, &r.Path, &r.Side,
			&r.Ref, &untracked, &r.StartLine, &r.EndLine, &r.SnapshotStartLine, &r.Snapshot, &r.Comment); err != nil {
			return nil, err
		}
		r.Untracked = untracked != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetDiffReviewSubmission 按 id 读取单条提交。缺行返回 sql.ErrNoRows。
func (q *Queries) GetDiffReviewSubmission(ctx context.Context, id string) (DiffReviewSubmissionRow, error) {
	row := q.db.QueryRowContext(ctx,
		`SELECT seq, id, task_id, status, target_session_id, message_id, note, payload, truncated, error, created_at, sent_at
		 FROM diff_review_submissions WHERE id = ?`, id)
	return scanDiffReviewSubmissionRow(row)
}

// diffReviewAllowedCASTransitions 为 D2 状态机合法 CAS 转移白名单（唯一真值来源）。
// 永久拒绝以 sent 为目标的转移：sent 只能经 sent 清理事务（CompleteDiffReviewSentCleanup）达成，
// 不得经 CAS 绕过批注清理（不存在独立 MarkSent 路径，D2/D3）。
var diffReviewAllowedCASTransitions = map[[2]string]struct{}{
	{DiffReviewStatusQueued, DiffReviewStatusSending}:         {},
	{DiffReviewStatusQueued, DiffReviewStatusFailed}:          {},
	{DiffReviewStatusSending, DiffReviewStatusFailed}:         {},
	{DiffReviewStatusSending, DiffReviewStatusDeliveryUnknown}: {},
}

// isAllowedDiffReviewCAS 判定 from→to 是否为 D2 合法状态机转移。
func isAllowedDiffReviewCAS(from, to string) bool {
	_, ok := diffReviewAllowedCASTransitions[[2]string{from, to}]
	return ok
}

// ErrDiffReviewIllegalCAS 表示 CAS 转移不在 D2 状态机白名单内（非法边或未知状态）。
// 调用方应视为编程错误而非冲突；返回此错误时数据库零修改。
var ErrDiffReviewIllegalCAS = errors.New("store: diff review submission CAS transition not allowed")

// CASDiffReviewSubmission 条件状态转移：仅当当前 status==from 且 from→to ∈ D2 状态机白名单时更新为 to。
// 返回 (matched bool, err)：
//   - matched=true：from 匹配且转移成功（1 行）。
//   - matched=false, err=nil：from 不匹配（CAS 失配，0 行，状态不变）。
//   - matched=false, err=ErrDiffReviewIllegalCAS：from→to 不在白名单（非法边 / 未知状态 / 目标为 sent），零修改。
//
// 白名单四条边：queued→sending、queued→failed、sending→failed、sending→delivery_unknown。
// 永久拒绝以 sent 为目标的转移（sent 仅由 sent 清理事务达成，D2/D3）。
// errorText 非空时写入 error 列（失败转移用），空时保持原值。
func (q *Queries) CASDiffReviewSubmission(ctx context.Context, id, from, to, errorText string) (bool, error) {
	if !isAllowedDiffReviewCAS(from, to) {
		return false, ErrDiffReviewIllegalCAS
	}
	var qry string
	var args []any
	if errorText == "" {
		qry = `UPDATE diff_review_submissions SET status = ? WHERE id = ? AND status = ?`
		args = []any{to, id, from}
	} else {
		qry = `UPDATE diff_review_submissions SET status = ?, error = ? WHERE id = ? AND status = ?`
		args = []any{to, errorText, id, from}
	}
	res, err := q.db.ExecContext(ctx, qry, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// CompleteDiffReviewSentCleanup 是 sent 清理事务唯一路径（D2/D3，不可拆分）：
// 在同一 SQLite 事务内完成 ① UPDATE submission sending→sent + 写 sent_at；
// ② 按 submission_items 的 annotation_id+annotation_revision 删除活动批注（revision 仍匹配才删，
// 已被编辑 revision+1 的批注保留）。
//
// 不存在独立的 MarkSent 方法：sent 必伴随批注清理同事务完成。
// 任一步失败整体回滚（原子性：不会出现 sent 而批注未删，或批注已删而 submission 未 sent）。
//
// 返回 (matched bool, err)：matched=false 表示 submission 当前非 sending（CAS 失配，零修改）；
// matched=true 表示已 sent 并完成清理。
func (q *Queries) CompleteDiffReviewSentCleanup(ctx context.Context, submissionID string) (bool, error) {
	return runTx(ctx, q, func(qx *Queries) (bool, error) {
		// 1) CAS：sending→sent + 写 sent_at。先校验当前为 sending，避免误改其他状态。
		now := nowUnix()
		res, err := qx.db.ExecContext(ctx,
			`UPDATE diff_review_submissions
			    SET status = ?, sent_at = ?
			 WHERE id = ? AND status = ?`,
			DiffReviewStatusSent, now, submissionID, DiffReviewStatusSending)
		if err != nil {
			return false, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return false, err
		}
		if n == 0 {
			// CAS 失配（非 sending 或行不存在）→ 零修改。
			return false, nil
		}
		// 2) 按 submission_items 删除活动批注：仅在 (annotation_id, annotation_revision) 元组
		// 逐条相关匹配时删除（D2/D3）。相关 EXISTS 子查询在同一 item 上同时约束
		// i.annotation_id = id AND i.annotation_revision = revision，避免两个独立 IN 集合
		// 造成的跨行误匹配（例如快照 (a1,1),(a2,2)、当前 (a1,2),(a2,3) 时 a1 不应被删）。
		// 已被编辑（revision+1）或已删除的批注不命中，自然保留。
		if _, err := qx.db.ExecContext(ctx,
			`DELETE FROM diff_annotations
			  WHERE EXISTS (
			      SELECT 1 FROM diff_review_submission_items i
			       WHERE i.submission_id = ?
			         AND i.annotation_id = diff_annotations.id
			         AND i.annotation_revision = diff_annotations.revision
			  )`,
			submissionID); err != nil {
			return false, err
		}
		return true, nil
	})
}

// CancelDiffReviewSubmission 撤回提交：仅当 status=queued 时 DELETE 行（D2：不保留 cancelled 记录）。
// 返回 (matched bool, err)：matched=false 表示非 queued（不能撤回）；true 表示已删除。
func (q *Queries) CancelDiffReviewSubmission(ctx context.Context, id string) (bool, error) {
	res, err := q.db.ExecContext(ctx,
		`DELETE FROM diff_review_submissions WHERE id = ? AND status = ?`,
		id, DiffReviewStatusQueued)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// DeleteDiffReviewSubmission 终态删除：仅当 status ∈ {sent, failed, delivery_unknown} 时删除（D2/D8）。
// 返回 (matched bool, err)：matched=false 表示非终态（不能删）；true 表示已删除。
func (q *Queries) DeleteDiffReviewSubmission(ctx context.Context, id string) (bool, error) {
	res, err := q.db.ExecContext(ctx,
		`DELETE FROM diff_review_submissions
		  WHERE id = ? AND status IN (?, ?, ?)`,
		id, DiffReviewStatusSent, DiffReviewStatusFailed, DiffReviewStatusDeliveryUnknown)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// ConvergeDiffReviewOnStartup 全局启动收敛（D2 重启恢复①）：单事务批量将所有 sending 行
// 转 delivery_unknown 并写入固定非空 error（"delivery unknown after restart"）。
// 独立于任何 runtime；收敛写库失败 MUST fail-closed（返回错误，调用方 lane 3 不开放 API/调度器）。
// 返回受影响行数。
func (q *Queries) ConvergeDiffReviewOnStartup(ctx context.Context) (int64, error) {
	var affected int64
	txErr := withTxQueries(ctx, q.db, func(qtx *Queries) error {
		res, err := qtx.db.ExecContext(ctx,
			`UPDATE diff_review_submissions
			    SET status = ?, error = ?
			 WHERE status = ?`,
			DiffReviewStatusDeliveryUnknown, diffReviewStartupConvergeError, DiffReviewStatusSending)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		affected = n
		return nil
	})
	return affected, txErr
}

// --- 扫描辅助 ---

func scanDiffAnnotationRow(row rowScanner) (DiffAnnotationRow, error) {
	var r DiffAnnotationRow
	var untracked int
	err := row.Scan(&r.ID, &r.TaskID, &r.Path, &r.Side, &r.Ref, &untracked,
		&r.StartLine, &r.EndLine, &r.SnapshotStartLine, &r.Snapshot, &r.SnapshotLineCount,
		&r.Comment, &r.Revision, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return DiffAnnotationRow{}, err
	}
	r.Untracked = untracked != 0
	return r, nil
}

func scanDiffAnnotationRows(rows *sql.Rows) ([]DiffAnnotationRow, error) {
	var out []DiffAnnotationRow
	for rows.Next() {
		r, err := scanDiffAnnotationRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanDiffReviewSubmissionRow(row rowScanner) (DiffReviewSubmissionRow, error) {
	var r DiffReviewSubmissionRow
	var truncated int
	err := row.Scan(&r.Seq, &r.ID, &r.TaskID, &r.Status, &r.TargetSessionID, &r.MessageID,
		&r.Note, &r.Payload, &truncated, &r.Error, &r.CreatedAt, &r.SentAt)
	if err != nil {
		return DiffReviewSubmissionRow{}, err
	}
	r.Truncated = truncated != 0
	return r, nil
}

func scanDiffReviewSubmissionRows(rows *sql.Rows) ([]DiffReviewSubmissionRow, error) {
	var out []DiffReviewSubmissionRow
	for rows.Next() {
		r, err := scanDiffReviewSubmissionRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// boolToInt 将 bool 映射为 SQLite INTEGER（0/1）。
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}