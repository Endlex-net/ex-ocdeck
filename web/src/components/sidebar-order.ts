import type { Project, TaskSummary } from '../types';

/** 侧栏任务组收录的状态集合：active/suspended + 过渡态 creating/activating/suspending。
 *  deleting（正在消失）与 archived/失败态不显示（避免闪现即将消失的行）。 */
const SIDEBAR_STATUS = new Set([
  'active',
  'suspended',
  'creating',
  'activating',
  'suspending',
]);

/** 过渡态 tooltip 文案（平实中文）。 */
const TRANSITIONAL_LABEL: Record<string, string> = {
  creating: '创建中',
  activating: '激活中',
  suspending: '挂起中',
};

/** 过渡态文案；非过渡态返回空串。 */
export function transitionalLabel(status: string): string {
  return TRANSITIONAL_LABEL[status] ?? '';
}

/** 侧栏任务是否收录（纯函数）。 */
export function isSidebarTask(t: TaskSummary): boolean {
  return SIDEBAR_STATUS.has(t.status);
}

/**
 * 侧栏任务组排序（用户选定方案：活跃度优先）。纯函数，供 AppShell 与测试共用。
 *
 * 组内任务排序（降序优先级）：
 *   0. 有注意力标记（attention_count > 0）
 *   1. 活跃且 agent 工作中（busy）
 *   2. 活跃其余（idle / 无 agentStatus）
 *   3. 过渡态（creating/activating/suspending，按"正在变为活跃"处理）
 *   4. 挂起
 * 同级 updated_at 倒序，再相同以任务 id 升序 tie-break（稳定）。
 *
 * 组间项目排序：按组内最高优先级任务的排序键比较（有注意力/活跃任务的组自然排前，
 * 纯挂起组之间即"组内最近 updated_at"倒序）；无可见任务的项目组整体不展示（组头也不渲染）。
 */

/** 侧栏任务组：项目 + 组内已排序任务（仅侧栏收录状态入组；至少含一个可见任务）。 */
export interface SidebarGroup {
  project: Project;
  tasks: TaskSummary[];
}

/** 组内优先级档（数值越小越靠前）。 */
export function taskRank(t: TaskSummary): number {
  if ((t.attention_count ?? 0) > 0) return 0;
  if (t.status === 'active') return t.agentStatus === 'busy' ? 1 : 2;
  if (t.status === 'creating' || t.status === 'activating' || t.status === 'suspending') return 3;
  return 4; // 挂起
}

/** 组内任务比较器：优先级档 → updated_at 倒序 → id 升序（稳定 tie-break）。 */
export function compareSidebarTasks(a: TaskSummary, b: TaskSummary): number {
  const dr = taskRank(a) - taskRank(b);
  if (dr !== 0) return dr;
  const dt = (b.updated_at ?? 0) - (a.updated_at ?? 0);
  if (dt !== 0) return dt;
  return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
}

/** 侧栏任务分组 + 排序：组内 compareSidebarTasks 排序；组间按组内最优任务键；
 *  无可见任务的项目组整体过滤（含组头不渲染）。 */
export function orderSidebarGroups(projects: Project[]): SidebarGroup[] {
  const groups: SidebarGroup[] = [];
  for (const p of projects) {
    const tasks = (p.tasks ?? []).filter(isSidebarTask).sort(compareSidebarTasks);
    if (tasks.length === 0) continue; // 空组不展示（组头也不渲染）
    groups.push({ project: p, tasks });
  }
  // 组间键 = 组内最高优先级任务；键相同返回 0，Array.sort 稳定保持 store 原序
  return groups.sort((ga, gb) => compareSidebarTasks(ga.tasks[0], gb.tasks[0]));
}
