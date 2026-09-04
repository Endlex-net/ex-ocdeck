// diffreview_adapters.go 实现 diffreview service 的 store 层 adapter（design.md D9）。
//
// 本文件适配已存在的 store 原语（diff_review_queries.go / queries.go），不改原语语义。
// 两类 adapter：
//   - DiffReviewRepoAdapter：实现 diffreview.DiffReviewRepository（批注/提交持久化，调 diff_review_queries.go 原语）
//   - TaskScopeAdapter：实现 diffreview.TaskScopePort（任务存在性 + 项目 kind，调 GetTask + GetProject）
//
// 编译期断言保证接口实现完整（design.md D9 五口全覆盖要求）。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"ocdeck/internal/application/diffreview"
)

// DiffReviewRepoAdapter 实现 diffreview.DiffReviewRepository（design.md D9，SQLite）。
//
// 适配 diff_review_queries.go 已存在的原语（CreateDiffAnnotation / UpdateDiffAnnotationComment /
// DeleteDiffAnnotation / ListDiffAnnotationsByTask / GetDiffAnnotation / CreateDiffReviewSubmission /
// ListDiffReviewQueue / ListDiffReviewHistory / ListDiffReviewFailures / ListDiffReviewSubmissionItems /
// GetDiffReviewSubmission / CASDiffReviewSubmission / CompleteDiffReviewSentCleanup /
// CancelDiffReviewSubmission / DeleteDiffReviewSubmission / ConvergeDiffReviewOnStartup）。
//
// 本阶段（3.3）DiffReviewRepository 接口为骨架占位（方法集待 3.5-3.7 补全），故 adapter
// 仅持有 *Queries 并就位编译期断言。用例方法随接口扩展在此添加，逐一调对应原语。
type DiffReviewRepoAdapter struct {
	q *Queries
}

// NewDiffReviewRepoAdapter 构造 SQLite DiffReviewRepository adapter。q 通常为 *DB.Queries。
func NewDiffReviewRepoAdapter(q *Queries) *DiffReviewRepoAdapter {
	return &DiffReviewRepoAdapter{q: q}
}

// 编译期断言：*DiffReviewRepoAdapter 实现 diffreview.DiffReviewRepository（design.md D9 五口全覆盖）。
// 接口当前为骨架（无方法），断言保证后续扩展方法集时 adapter 同步实现。
var _ diffreview.DiffReviewRepository = (*DiffReviewRepoAdapter)(nil)

// TaskScopeAdapter 实现 diffreview.TaskScopePort（design.md D9，SQLite）。
//
// 调 GetTask（存在性）+ GetProject（kind）。sql.ErrNoRow 归一化为 Found=false（非 error）；
// 其余底层 store 错误透传（service 按 internal 处理）。
type TaskScopeAdapter struct {
	q *Queries
}

// NewTaskScopeAdapter 构造 SQLite TaskScopePort adapter。
func NewTaskScopeAdapter(q *Queries) *TaskScopeAdapter {
	return &TaskScopeAdapter{q: q}
}

// 编译期断言：*TaskScopeAdapter 实现 diffreview.TaskScopePort（design.md D9 五口全覆盖）。
var _ diffreview.TaskScopePort = (*TaskScopeAdapter)(nil)

// Lookup 查询任务存在性与项目 kind（design.md D9 TaskScopePort）。
// 任务不存在（GetTask 返回 sql.ErrNoRow）→ Found=false, Kind=""，无 error。
// 任务存在但项目不存在（GetProject 返回 sql.ErrNoRow）→ 视为数据完整性错误，返回 error。
// 其余底层错误透传。
func (a *TaskScopeAdapter) Lookup(ctx context.Context, taskID string) (diffreview.TaskScopeResult, error) {
	task, err := a.q.GetTask(ctx, taskID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return diffreview.TaskScopeResult{Found: false, Kind: ""}, nil
		}
		return diffreview.TaskScopeResult{}, fmt.Errorf("diffreview scope: get task %s: %w", taskID, err)
	}
	proj, err := a.q.GetProject(ctx, task.ProjectID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return diffreview.TaskScopeResult{}, fmt.Errorf("diffreview scope: project %s missing for task %s", task.ProjectID, taskID)
		}
		return diffreview.TaskScopeResult{}, fmt.Errorf("diffreview scope: get project %s: %w", task.ProjectID, err)
	}
	return diffreview.TaskScopeResult{Found: true, Kind: proj.Kind}, nil
}