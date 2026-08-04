import type { Task } from '../types';

/**
 * init 状态徽标（tasks.md 5.3）：
 * - pending|running → "init 进行中"（spinner）
 * - failed → 失败徽标（title 为 init_error 权威信息）
 * - none|succeeded → 不显示
 */
export function InitStatusBadge({ task }: { task: Task }) {
  switch (task.init_status) {
    case 'pending':
    case 'running':
      return (
        <span className="badge badge-pending" title={`init_status: ${task.init_status}`}>
          <span className="spinner spinner-inline" aria-hidden />
          init 进行中
        </span>
      );
    case 'failed':
      return (
        <span className="badge badge-failed" title={task.init_error || 'init 失败'}>
          init failed
        </span>
      );
    default:
      return null;
  }
}
