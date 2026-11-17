import type { TaskSummary } from '../types';
import { transitionalLabel } from './sidebar-order';

export type TaskStatusDot = 'idle' | 'busy' | 'retry' | 'attention' | 'idle-off';

type TaskDotStatus = Pick<TaskSummary, 'agentStatus'> &
  Partial<Pick<TaskSummary, 'attention_count'>>;

type TaskStatusText = Pick<TaskSummary, 'status' | 'agentStatus'> &
  Partial<Pick<TaskSummary, 'attention_count'>>;

/** compact 任务状态点：等待人工始终优先，未知运行态沿用既有空心点。 */
export function taskStatusDotClass(task: TaskDotStatus): TaskStatusDot {
  if ((task.attention_count ?? 0) > 0) return 'attention';
  if (task.agentStatus === 'busy') return 'busy';
  if (task.agentStatus === 'retry') return 'retry';
  if (task.agentStatus === 'idle') return 'idle';
  return 'idle-off';
}

/** compact 悬浮详情状态文案，过渡态与侧栏共用同一事实来源。 */
export function taskStatusLabel(task: TaskStatusText): string {
  const transitional = transitionalLabel(task.status);
  if (transitional) return transitional;
  if (task.status === 'suspended') return '任务已挂起';
  if (task.status === 'active') {
    if ((task.attention_count ?? 0) > 0) return '等待人工';
    if (task.agentStatus === 'busy') return '工作中';
    if (task.agentStatus === 'retry') return '重试中';
    if (task.agentStatus === 'idle') return '空闲';
    return 'agent状态未知';
  }
  return 'agent状态未知';
}
