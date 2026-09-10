import { PERMISSION_MODE_LABELS, type Task } from '../types';

/**
 * 权限模式只读徽标（task-permission-mode D7/D8）：任务详情页头按 task.permission_mode
 * 展示创建时选定的模式（人工批准 / 全部批准 / AI 自动识别）；创建后不可修改，无修改入口。
 */
export function PermissionModeBadge({ task }: { task: Task }) {
  return (
    <span className="badge badge-muted" title={`权限模式（创建后不可修改）：${task.permission_mode}`}>
      {PERMISSION_MODE_LABELS[task.permission_mode]}
    </span>
  );
}
