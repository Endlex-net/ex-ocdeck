package api

import (
	"context"

	"ocdeck/internal/infrastructure/store"
)

// storeProjectAdapter 包装 *store.DB 实现 ProjectStore，做 row 类型转换，
// 避免 api handler 直接依赖 store 包结构（design.md §18：api 不做业务编排）。
type storeProjectAdapter struct {
	db *store.DB
}

// NewProjectStoreAdapter 用 *store.DB 构造 ProjectStore 适配器。
func NewProjectStoreAdapter(db *store.DB) ProjectStore {
	return &storeProjectAdapter{db: db}
}

func (a *storeProjectAdapter) CreateProject(ctx context.Context, id, name, path, defaultBranch, kind string) error {
	return a.db.CreateProject(ctx, id, name, path, defaultBranch, kind)
}

func (a *storeProjectAdapter) GetProject(ctx context.Context, id string) (storeProjectRow, error) {
	p, err := a.db.GetProject(ctx, id)
	if err != nil {
		return storeProjectRow{}, err
	}
	return storeProjectRow{
		ID:            p.ID,
		Name:          p.Name,
		Path:          p.Path,
		DefaultBranch: p.DefaultBranch,
		Kind:          p.Kind,
		CreatedAt:     p.CreatedAt,
	}, nil
}

func (a *storeProjectAdapter) GetProjectByPath(ctx context.Context, path string) (storeProjectRow, error) {
	p, err := a.db.GetProjectByPath(ctx, path)
	if err != nil {
		return storeProjectRow{}, err
	}
	return storeProjectRow{
		ID:            p.ID,
		Name:          p.Name,
		Path:          p.Path,
		DefaultBranch: p.DefaultBranch,
		Kind:          p.Kind,
		CreatedAt:     p.CreatedAt,
	}, nil
}

func (a *storeProjectAdapter) ListProjects(ctx context.Context) ([]storeProjectRow, error) {
	rows, err := a.db.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]storeProjectRow, 0, len(rows))
	for _, p := range rows {
		out = append(out, storeProjectRow{
			ID:            p.ID,
			Name:          p.Name,
			Path:          p.Path,
			DefaultBranch: p.DefaultBranch,
			Kind:          p.Kind,
			CreatedAt:     p.CreatedAt,
		})
	}
	return out, nil
}

func (a *storeProjectAdapter) DeleteProjectIfEmpty(ctx context.Context, id string) (bool, error) {
	return a.db.DeleteProjectIfEmpty(ctx, id)
}

func (a *storeProjectAdapter) CountProjectTasks(ctx context.Context, projectID string) (storeTaskCounts, error) {
	c, err := a.db.CountProjectTasks(ctx, projectID)
	if err != nil {
		return storeTaskCounts{}, err
	}
	return storeTaskCounts{Total: c.Total, ByStatus: c.ByStatus}, nil
}

func (a *storeProjectAdapter) HasProjectTasks(ctx context.Context, projectID string) (bool, error) {
	return a.db.HasProjectTasks(ctx, projectID)
}
