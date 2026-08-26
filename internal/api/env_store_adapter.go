package api

import (
	"context"

	"ocdeck/internal/infrastructure/store"
)

// storeEnvAdapter 包装 *store.DB 实现 EnvStore，做 row 类型转换，
// 避免 api handler 直接依赖 store 包结构（design.md §18：api 不做业务编排）。
type storeEnvAdapter struct {
	db *store.DB
}

// NewEnvStoreAdapter 用 *store.DB 构造 EnvStore 适配器。
func NewEnvStoreAdapter(db *store.DB) EnvStore {
	return &storeEnvAdapter{db: db}
}

func (a *storeEnvAdapter) ListProjectEnvVars(ctx context.Context, projectID string) ([]envVarRow, error) {
	rows, err := a.db.ListProjectEnvVars(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]envVarRow, 0, len(rows))
	for _, e := range rows {
		out = append(out, envVarRow{Key: e.Key, Value: e.Value})
	}
	return out, nil
}

func (a *storeEnvAdapter) SetProjectEnvVar(ctx context.Context, projectID, key, value string) error {
	return a.db.SetProjectEnvVar(ctx, projectID, key, value)
}

func (a *storeEnvAdapter) DeleteProjectEnvVar(ctx context.Context, projectID, key string) error {
	return a.db.DeleteProjectEnvVar(ctx, projectID, key)
}

func (a *storeEnvAdapter) ListTaskEnvVars(ctx context.Context, taskID string) ([]envVarRow, error) {
	rows, err := a.db.ListTaskEnvVars(ctx, taskID)
	if err != nil {
		return nil, err
	}
	out := make([]envVarRow, 0, len(rows))
	for _, e := range rows {
		out = append(out, envVarRow{Key: e.Key, Value: e.Value})
	}
	return out, nil
}

func (a *storeEnvAdapter) SetTaskEnvVar(ctx context.Context, taskID, key, value string) error {
	return a.db.SetTaskEnvVar(ctx, taskID, key, value)
}

func (a *storeEnvAdapter) DeleteTaskEnvVar(ctx context.Context, taskID, key string) error {
	return a.db.DeleteTaskEnvVar(ctx, taskID, key)
}

func (a *storeEnvAdapter) ListGlobalEnvVars(ctx context.Context) ([]globalEnvVarRow, error) {
	rows, err := a.db.ListGlobalEnvVars(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]globalEnvVarRow, 0, len(rows))
	for _, e := range rows {
		out = append(out, globalEnvVarRow{Key: e.Key, Mode: e.Mode, Value: e.Value})
	}
	return out, nil
}

func (a *storeEnvAdapter) SetGlobalEnvVar(ctx context.Context, key, mode, value string) error {
	return a.db.SetGlobalEnvVar(ctx, key, mode, value)
}

func (a *storeEnvAdapter) DeleteGlobalEnvVar(ctx context.Context, key string) error {
	return a.db.DeleteGlobalEnvVar(ctx, key)
}
