package task

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	"ocdeck/internal/application"
)

// 本文件为 Phase 3 生命周期配置测试提供 mockStore 扩展（design.md §2.1/§3），
// 不修改 mock_test.go（另一 lane 独占）。mockStore 是具体类型（非嵌入 TaskStore 接口），
// 新增 TaskStore 方法必须在此实现以满足接口，否则包测试无法编译。

// lifecycleCfgRows 按 mockStore 实例存储项目生命周期配置（按 projectID 索引）。
// 按 store 实例隔离，避免不同测试函数共用同一个 mockStore 时配置互相污染
// （如 B2 测试用 newMockStore 不应读到 Phase 3 测试 seed 的 p1 init 配置）。
var (
	lifecycleCfgMu   sync.Mutex
	lifecycleCfgRows = map[*mockStore]map[string]LifecycleConfigRow{}
)

func resetLifecycleCfgMock() {
	lifecycleCfgMu.Lock()
	lifecycleCfgRows = map[*mockStore]map[string]LifecycleConfigRow{}
	lifecycleCfgMu.Unlock()
}

func (s *mockStore) cfgRows() map[string]LifecycleConfigRow {
	lifecycleCfgMu.Lock()
	rows, ok := lifecycleCfgRows[s]
	if !ok {
		rows = map[string]LifecycleConfigRow{}
		lifecycleCfgRows[s] = rows
	}
	lifecycleCfgMu.Unlock()
	return rows
}

func (s *mockStore) GetLifecycleConfig(ctx context.Context, projectID string) (LifecycleConfigRow, error) {
	lifecycleCfgMu.Lock()
	rows, ok := lifecycleCfgRows[s]
	lifecycleCfgMu.Unlock()
	if ok {
		if c, ok2 := rows[projectID]; ok2 {
			return c, nil
		}
	}
	// 缺行 = 空配置（非错误），与 store.Queries 语义一致。
	return LifecycleConfigRow{ProjectID: projectID}, nil
}

func (s *mockStore) UpsertLifecycleConfig(ctx context.Context, projectID, inheritPatterns, initScript, preDeleteScript string) error {
	lifecycleCfgMu.Lock()
	rows, ok := lifecycleCfgRows[s]
	if !ok {
		rows = map[string]LifecycleConfigRow{}
		lifecycleCfgRows[s] = rows
	}
	rows[projectID] = LifecycleConfigRow{
		ProjectID: projectID, InheritPatterns: inheritPatterns,
		InitScript: initScript, PreDeleteScript: preDeleteScript,
	}
	lifecycleCfgMu.Unlock()
	return nil
}

func (s *mockStore) CommitCreated(ctx context.Context, taskID, expectedStatus, initStatus string) (application.TransitionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[taskID]
	if !ok {
		return application.TransitionResult{}, fmt.Errorf("not found")
	}
	if t.Status != expectedStatus {
		return application.TransitionResult{}, nil
	}
	t.Status = StatusSuspended
	t.InitStatus = initStatus
	t.LastError = sql.NullString{}
	t.UpdatedAt = 10
	s.tasks[taskID] = t
	return application.TransitionResult{MutationResult: application.MutationResult{Matched: true, Changed: true}}, nil
}

func (s *mockStore) ClaimInitRun(ctx context.Context, taskID string) (application.MutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[taskID]
	if !ok {
		return application.MutationResult{}, fmt.Errorf("not found")
	}
	if t.Status != StatusSuspended || t.InitStatus != InitStatusPending {
		return application.MutationResult{}, nil
	}
	t.InitStatus = InitStatusRunning
	t.UpdatedAt = 11
	s.tasks[taskID] = t
	return application.MutationResult{Matched: true, Changed: true}, nil
}

func (s *mockStore) ClaimInitRerun(ctx context.Context, taskID string) (application.MutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[taskID]
	if !ok {
		return application.MutationResult{}, fmt.Errorf("not found")
	}
	if t.Status != StatusSuspended || (t.InitStatus != InitStatusFailed && t.InitStatus != InitStatusSucceeded) {
		return application.MutationResult{}, nil
	}
	t.InitStatus = InitStatusRunning
	t.InitError = sql.NullString{}
	t.UpdatedAt = 12
	s.tasks[taskID] = t
	return application.MutationResult{Matched: true, Changed: true}, nil
}

func (s *mockStore) FinishInitRun(ctx context.Context, taskID, status string, initError sql.NullString) (application.MutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[taskID]
	if !ok {
		return application.MutationResult{}, fmt.Errorf("not found")
	}
	if t.InitStatus != InitStatusRunning {
		return application.MutationResult{}, nil
	}
	t.InitStatus = status
	t.InitError = initError
	t.UpdatedAt = 13
	s.tasks[taskID] = t
	return application.MutationResult{Matched: true, Changed: true}, nil
}

func (s *mockStore) ConvergeInterruptedInitRuns(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for id, t := range s.tasks {
		if t.InitStatus == InitStatusPending || t.InitStatus == InitStatusRunning {
			t.InitStatus = InitStatusFailed
			t.InitError = sql.NullString{String: "interrupted by server restart", Valid: true}
			t.UpdatedAt = 14
			s.tasks[id] = t
			n++
		}
	}
	return n, nil
}
