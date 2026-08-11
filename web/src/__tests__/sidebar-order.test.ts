import { describe, it, expect } from 'vitest';
import {
  compareSidebarTasks,
  isSidebarTask,
  orderSidebarGroups,
  taskRank,
  transitionalLabel,
} from '../components/sidebar-order';
import type { Project, TaskSummary } from '../types';

/** TaskSummary 工厂：只关心排序相关字段，其余给缺省。 */
function task(over: Partial<TaskSummary> & { id: string }): TaskSummary {
  return {
    name: over.id,
    status: 'active',
    init_status: 'succeeded',
    branch: 'main',
    worktree_path: '/tmp/x',
    updated_at: 0,
    attention_count: 0,
    ...over,
  };
}

/** Project 工厂：tasks 之外字段给缺省。 */
function project(id: string, tasks: TaskSummary[]): Project {
  return {
    id,
    name: id,
    path: `/tmp/${id}`,
    kind: 'repo',
    default_branch: 'main',
    created_at: 0,
    task_count: tasks.length,
    tasks_by_status: {},
    tasks,
  };
}

describe('taskRank（组内优先级档）', () => {
  it('注意力 > busy > idle/无 agentStatus > 过渡态 > 挂起', () => {
    expect(taskRank(task({ id: 'a', attention_count: 2, agentStatus: 'idle' }))).toBe(0);
    expect(taskRank(task({ id: 'b', agentStatus: 'busy' }))).toBe(1);
    expect(taskRank(task({ id: 'c', agentStatus: 'idle' }))).toBe(2);
    expect(taskRank(task({ id: 'd' }))).toBe(2); // 无 agentStatus 与 idle 同档
    expect(taskRank(task({ id: 'e', status: 'activating' }))).toBe(3);
    expect(taskRank(task({ id: 'f', status: 'creating' }))).toBe(3);
    expect(taskRank(task({ id: 'g', status: 'suspending' }))).toBe(3);
    expect(taskRank(task({ id: 'h', status: 'suspended', agentStatus: 'busy' }))).toBe(4);
  });

  it('过渡态带注意力标记仍排最前', () => {
    expect(taskRank(task({ id: 'a', status: 'activating', attention_count: 1 }))).toBe(0);
  });
});

describe('侧栏收录状态（isSidebarTask / transitionalLabel）', () => {
  it('active/suspended/creating/activating/suspending 收录；deleting/archived/失败态不收', () => {
    expect(isSidebarTask(task({ id: 'a' }))).toBe(true); // active
    expect(isSidebarTask(task({ id: 'b', status: 'suspended' }))).toBe(true);
    expect(isSidebarTask(task({ id: 'c', status: 'creating' }))).toBe(true);
    expect(isSidebarTask(task({ id: 'd', status: 'activating' }))).toBe(true);
    expect(isSidebarTask(task({ id: 'e', status: 'suspending' }))).toBe(true);
    expect(isSidebarTask(task({ id: 'f', status: 'deleting' }))).toBe(false);
    expect(isSidebarTask(task({ id: 'g', status: 'archived' }))).toBe(false);
    expect(isSidebarTask(task({ id: 'h', status: 'creation_failed' }))).toBe(false);
    expect(isSidebarTask(task({ id: 'i', status: 'deletion_failed' }))).toBe(false);
  });

  it('过渡态文案', () => {
    expect(transitionalLabel('creating')).toBe('创建中');
    expect(transitionalLabel('activating')).toBe('激活中');
    expect(transitionalLabel('suspending')).toBe('挂起中');
    expect(transitionalLabel('active')).toBe('');
    expect(transitionalLabel('deleting')).toBe('');
  });
});

describe('compareSidebarTasks（组内排序）', () => {
  it('按优先级档降序排列', () => {
    const sorted = [
      task({ id: 'susp', status: 'suspended', updated_at: 99 }),
      task({ id: 'idle', agentStatus: 'idle', updated_at: 50 }),
      task({ id: 'att', attention_count: 1, updated_at: 10 }),
      task({ id: 'busy', agentStatus: 'busy', updated_at: 5 }),
    ].sort(compareSidebarTasks);
    expect(sorted.map((t) => t.id)).toEqual(['att', 'busy', 'idle', 'susp']);
  });

  it('过渡态排在活跃之后、挂起之前', () => {
    const sorted = [
      task({ id: 'susp', status: 'suspended', updated_at: 99 }),
      task({ id: 'act', status: 'activating', updated_at: 1 }),
      task({ id: 'idle', agentStatus: 'idle', updated_at: 50 }),
    ].sort(compareSidebarTasks);
    expect(sorted.map((t) => t.id)).toEqual(['idle', 'act', 'susp']);
  });

  it('同级 updated_at 倒序', () => {
    const sorted = [
      task({ id: 'old', status: 'suspended', updated_at: 100 }),
      task({ id: 'new', status: 'suspended', updated_at: 300 }),
      task({ id: 'mid', status: 'suspended', updated_at: 200 }),
    ].sort(compareSidebarTasks);
    expect(sorted.map((t) => t.id)).toEqual(['new', 'mid', 'old']);
  });

  it('updated_at 相同以 id 升序 tie-break（稳定）', () => {
    const sorted = [
      task({ id: 'b', agentStatus: 'busy', updated_at: 7 }),
      task({ id: 'a', agentStatus: 'busy', updated_at: 7 }),
      task({ id: 'c', agentStatus: 'busy', updated_at: 7 }),
    ].sort(compareSidebarTasks);
    expect(sorted.map((t) => t.id)).toEqual(['a', 'b', 'c']);
  });
});

describe('orderSidebarGroups（组间排序）', () => {
  it('按组内最高优先级任务键排序：注意力组 > busy 组 > 纯挂起组（按最近 updated_at）', () => {
    const groups = orderSidebarGroups([
      project('empty', []),
      project('susp-old', [task({ id: 't1', status: 'suspended', updated_at: 100 })]),
      project('busy', [task({ id: 't2', agentStatus: 'busy', updated_at: 10 })]),
      project('susp-new', [task({ id: 't3', status: 'suspended', updated_at: 200 })]),
      project('att', [task({ id: 't4', attention_count: 3, updated_at: 1 })]),
    ]);
    expect(groups.map((g) => g.project.id)).toEqual([
      'att',
      'busy',
      'susp-new',
      'susp-old',
    ]);
  });

  it('空组整体不展示：全空不显示、仅归档不显示、有过渡态显示', () => {
    // 全空（无任务 + 仅归档/失败/deleting）→ 空数组
    expect(
      orderSidebarGroups([
        project('no-tasks', []),
        project('archived-only', [task({ id: 't', status: 'archived' })]),
        project('deleting-only', [task({ id: 't', status: 'deleting' })]),
        project('failed-only', [task({ id: 't', status: 'creation_failed' })]),
      ]),
    ).toEqual([]);
    // 有过渡态任务的组显示；空组（含组头）不渲染
    const groups = orderSidebarGroups([
      project('empty-1', []),
      project('activating', [task({ id: 't', status: 'activating' })]),
      project('archived-only', [task({ id: 't', status: 'archived' })]),
    ]);
    expect(groups.map((g) => g.project.id)).toEqual(['activating']);
  });

  it('组间键相同（同档同 updated_at）以最优任务 id 升序 tie-break', () => {
    const groups = orderSidebarGroups([
      project('pb', [task({ id: 'b', agentStatus: 'busy', updated_at: 5 })]),
      project('pa', [task({ id: 'a', agentStatus: 'busy', updated_at: 5 })]),
    ]);
    expect(groups.map((g) => g.project.id)).toEqual(['pa', 'pb']);
  });

  it('组内任务同步排序；归档/失败/deleting 任务不入组，过渡态入组', () => {
    const [g] = orderSidebarGroups([
      project('p', [
        task({ id: 'archived', status: 'archived' }),
        task({ id: 'idle', agentStatus: 'idle' }),
        task({ id: 'failed', status: 'creation_failed' }),
        task({ id: 'del', status: 'deleting' }),
        task({ id: 'act', status: 'activating' }),
        task({ id: 'att', attention_count: 1 }),
        task({ id: 'susp', status: 'suspended' }),
      ]),
    ]);
    expect(g.tasks.map((t) => t.id)).toEqual(['att', 'idle', 'act', 'susp']);
  });
});
