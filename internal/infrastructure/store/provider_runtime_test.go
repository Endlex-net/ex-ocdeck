// provider_runtime_test.go 落实 tasks 5.1：实例记录生命周期（pid 可空启动中态
// 先于启动写入、步骤⑤落 pid、端口固定不轮换、启动失败记 last_error）。
package store

import (
	"context"
	"database/sql"
	"testing"
)

func TestProviderRuntime_Lifecycle(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	q := db.Queries

	// 无记录 → false。
	if _, ok, err := q.GetProviderRuntime(ctx, ProviderIDV2, ProviderInstanceV2Default); err != nil || ok {
		t.Fatalf("empty: ok=%v err=%v", ok, err)
	}
	// 写序①：启动中态（pid=NULL）。
	if err := q.PutProviderRuntimeStart(ctx, ProviderIDV2, ProviderInstanceV2Default, 4096); err != nil {
		t.Fatalf("start: %v", err)
	}
	rec, ok, err := q.GetProviderRuntime(ctx, ProviderIDV2, ProviderInstanceV2Default)
	if err != nil || !ok || rec.Port != 4096 || rec.PID.Valid {
		t.Fatalf("record: %+v ok=%v err=%v", rec, ok, err)
	}
	// 步骤⑤：落 pid（可用）。
	if err := q.SetProviderRuntimePID(ctx, ProviderIDV2, ProviderInstanceV2Default, 1234); err != nil {
		t.Fatalf("set pid: %v", err)
	}
	rec, _, _ = q.GetProviderRuntime(ctx, ProviderIDV2, ProviderInstanceV2Default)
	if !rec.PID.Valid || rec.PID.Int64 != 1234 {
		t.Fatalf("pid: %+v", rec.PID)
	}
	// 端口固定不轮换：同记录不同端口 → 拒绝（原记录不动）。
	if err := q.PutProviderRuntimeStart(ctx, ProviderIDV2, ProviderInstanceV2Default, 5050); err == nil {
		t.Fatalf("port rotation must be rejected")
	}
	rec, _, _ = q.GetProviderRuntime(ctx, ProviderIDV2, ProviderInstanceV2Default)
	if rec.Port != 4096 || rec.PID.Int64 != 1234 {
		t.Fatalf("record after rejection: %+v", rec)
	}
	// 崩溃窗口：重置为启动中态（pid=NULL）同端口合法。
	if err := q.PutProviderRuntimeStart(ctx, ProviderIDV2, ProviderInstanceV2Default, 4096); err != nil {
		t.Fatalf("crash-window reset: %v", err)
	}
	rec, _, _ = q.GetProviderRuntime(ctx, ProviderIDV2, ProviderInstanceV2Default)
	if rec.PID.Valid {
		t.Fatalf("crash-window must clear pid: %+v", rec)
	}
	// 启动失败：保留记录 + last_error。
	if err := q.SetProviderRuntimeError(ctx, ProviderIDV2, ProviderInstanceV2Default, "spawn boom"); err != nil {
		t.Fatalf("set error: %v", err)
	}
	rec, _, _ = q.GetProviderRuntime(ctx, ProviderIDV2, ProviderInstanceV2Default)
	if rec.PID.Valid || !rec.LastError.Valid || rec.LastError.String != "spawn boom" {
		t.Fatalf("failed-start record: %+v", rec)
	}
	// 步骤⑤成功清除 last_error。
	if err := q.SetProviderRuntimePID(ctx, ProviderIDV2, ProviderInstanceV2Default, 9); err != nil {
		t.Fatal(err)
	}
	rec, _, _ = q.GetProviderRuntime(ctx, ProviderIDV2, ProviderInstanceV2Default)
	if rec.LastError.Valid {
		t.Fatalf("last_error must clear on ready: %+v", rec)
	}
	// 独立实例隔离。
	if err := q.PutProviderRuntimeStart(ctx, "opencode-v1", "default", 5000); err != nil {
		t.Fatal(err)
	}
	rec2, ok2, _ := q.GetProviderRuntime(ctx, "opencode-v1", "default")
	if !ok2 || rec2.Port != 5000 {
		t.Fatalf("instance isolation: %+v ok=%v", rec2, ok2)
	}
	var _ = sql.NullInt64{}
}
