import { PERMISSION_MODE_LABELS, type Task } from '../types';

/**
 * 权限模式只读徽标（task-permission-mode D7/D8）：任务详情页头按 task.permission_mode
 * 展示任务当前保存的权限模式（人工批准 / 全部批准 / AI 自动识别）只读徽标；修改入口在任务信息卡。
 */
export function PermissionModeBadge({ task }: { task: Task }) {
  return (
    <span className="badge badge-muted" title={`权限模式：${task.permission_mode}`}>
      {PERMISSION_MODE_LABELS[task.permission_mode]}
    </span>
  );
}
