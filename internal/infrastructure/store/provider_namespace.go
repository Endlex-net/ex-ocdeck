// provider_namespace.go 实现命名空间防御性补齐（opencode-v1-spi-provider
// tasks 2.2 / design D6 阶段 1）：每次启动、在任何命名空间消费者运行前执行，
// 幂等、单事务。
//
// 覆盖范围（design D6）：tasks.provider_id、task_sessions 两列、非空锚定的
// anchor 命名空间列中 NULL 或空串的行——含旧二进制（回滚窗口）显式写空值的
// 边界情形；已有合法值保留（CASE 逐列守卫，不整行覆盖）。与 active 任务的
// binding 导入 runner（阶段 3）相互独立、先后无依赖。不推进 updated_at、不发事件。
package store

import (
	"context"
	"fmt"
)

// ProviderIDV1 与 ProviderInstanceV1Default 是本期 v1 统一命名空间常量
// （design D2：整个既有 v1 数据域一个命名空间，取值固定；归属判定冲突集合
// 与迁移前逐点一致）。ProviderIDV2/ProviderInstanceV2Default 是 v2 命名空间
// 常量（opencode-v2-provider design D2；承接原子转换的目标命名空间）。
const (
	ProviderIDV1              = "opencode-v1"
	ProviderInstanceV1Default = "default"
	ProviderIDV2              = "opencode-v2"
	ProviderInstanceV2Default = "default"
)

// BackfillProviderNamespaces 命名空间防御性补齐：单事务内执行四组 UPDATE，
// 任一失败整体回滚（幂等——WHERE 守卫仅命中 NULL/空串行）。
func (d *DB) BackfillProviderNamespaces(ctx context.Context) error {
	err := d.WithTx(ctx, func(q *Queries) error {
		// tasks.provider_id：NULL/空串 → v1。
		if _, err := q.db.ExecContext(ctx,
			`UPDATE tasks SET provider_id = ? WHERE provider_id IS NULL OR provider_id = ''`,
			ProviderIDV1); err != nil {
			return fmt.Errorf("backfill tasks.provider_id: %w", err)
		}
		// 锚定命名空间：仅非空锚定行补齐（无锚定行 anchor 列保持 NULL 语义）；
		// 逐列 CASE 守卫——一列空串、另一列合法时只补空列。
		if _, err := q.db.ExecContext(ctx,
			`UPDATE tasks SET
			   anchor_provider_id = CASE
			     WHEN anchor_provider_id IS NULL OR anchor_provider_id = '' THEN ? ELSE anchor_provider_id END,
			   anchor_provider_instance_id = CASE
			     WHEN anchor_provider_instance_id IS NULL OR anchor_provider_instance_id = '' THEN ? ELSE anchor_provider_instance_id END
			 WHERE anchor_session_id IS NOT NULL AND anchor_session_id != ''
			   AND (anchor_provider_id IS NULL OR anchor_provider_id = ''
			     OR anchor_provider_instance_id IS NULL OR anchor_provider_instance_id = '')`,
			ProviderIDV1, ProviderInstanceV1Default); err != nil {
			return fmt.Errorf("backfill tasks.anchor namespace: %w", err)
		}
		// task_sessions 两列：同样逐列守卫。
		if _, err := q.db.ExecContext(ctx,
			`UPDATE task_sessions SET
			   provider_id = CASE
			     WHEN provider_id IS NULL OR provider_id = '' THEN ? ELSE provider_id END,
			   provider_instance_id = CASE
			     WHEN provider_instance_id IS NULL OR provider_instance_id = '' THEN ? ELSE provider_instance_id END
			 WHERE provider_id IS NULL OR provider_id = ''
			    OR provider_instance_id IS NULL OR provider_instance_id = ''`,
			ProviderIDV1, ProviderInstanceV1Default); err != nil {
			return fmt.Errorf("backfill task_sessions namespace: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("backfill provider namespaces: %w", err)
	}
	return nil
}
