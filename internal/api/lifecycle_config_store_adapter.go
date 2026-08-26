package api

import (
	"context"

	"ocdeck/internal/infrastructure/store"
)

// storeLifecycleConfigAdapter 包装 *store.DB 实现 LifecycleConfigStore，做 row 类型转换，
// 避免 api handler 直接依赖 store 包结构（design.md §8：api 不做业务编排）。
type storeLifecycleConfigAdapter struct {
	db *store.DB
}

// NewLifecycleConfigStoreAdapter 用 *store.DB 构造 LifecycleConfigStore 适配器。
func NewLifecycleConfigStoreAdapter(db *store.DB) LifecycleConfigStore {
	return &storeLifecycleConfigAdapter{db: db}
}

func (a *storeLifecycleConfigAdapter) GetLifecycleConfig(ctx context.Context, projectID string) (lifecycleConfigRow, error) {
	c, err := a.db.GetLifecycleConfig(ctx, projectID)
	if err != nil {
		return lifecycleConfigRow{}, err
	}
	return lifecycleConfigRow{
		ProjectID:       c.ProjectID,
		InheritPatterns: c.InheritPatterns,
		InitScript:      c.InitScript,
		PreDeleteScript: c.PreDeleteScript,
		UpdatedAt:       c.UpdatedAt,
	}, nil
}

func (a *storeLifecycleConfigAdapter) UpsertLifecycleConfig(ctx context.Context, projectID, inheritPatterns, initScript, preDeleteScript string) error {
	return a.db.UpsertLifecycleConfig(ctx, projectID, inheritPatterns, initScript, preDeleteScript)
}
