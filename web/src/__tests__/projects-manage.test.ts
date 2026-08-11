import { describe, it, expect } from 'vitest';
import {
  filterProjects,
  filterTasksByStatus,
  healthCounts,
  STATUS_FILTER_LABEL,
  toggleStatusFilter,
} from '../pages/ProjectsManagePage';
import type { Project, Task } from '../types';

/* ============================ 项目管理单页纯函数测试（tasks.md 6.x） ============================
 * 无 jsdom 渲染环境（design.md D11），可测逻辑抽为纯函数单测。
 * 路由解析/store/mutation 等契约测试归 4.9（shell-contracts.test.ts）。 */

function makeProject(p: Partial<Project>): Project {
  return {
    id: p.id ?? 'p1',
    name: p.name ?? 'demo',
    path: p.path ?? '/demo',
    kind: p.kind ?? 'repo',
    default_branch: p.default_branch ?? 'main',
    created_at: p.created_at ?? 0,
    task_count: p.task_count ?? 0,
    tasks_by_status: p.tasks_by_status ?? {},
    tasks: p.tasks ?? [],
  };
}

describe('healthCounts（健康摘要推导）', () => {
  it('空/缺省 tasks_by_status 全为 0', () => {
    expect(healthCounts(undefined)).toEqual({ active: 0, suspended: 0, archived: 0, failed: 0 });
    expect(healthCounts({})).toEqual({ active: 0, suspended: 0, archived: 0, failed: 0 });
  });

  it('各状态字段正确映射', () => {
    expect(
      healthCounts({ active: 1, suspended: 2, archived: 3 }),
    ).toEqual({ active: 1, suspended: 2, archived: 3, failed: 0 });
  });

  it('失败 = creation_failed + deletion_failed', () => {
    expect(
      healthCounts({ creation_failed: 1, deletion_failed: 2 }),
    ).toEqual({ active: 0, suspended: 0, archived: 0, failed: 3 });
  });

  it('缺省失败子键归 0', () => {
    expect(healthCounts({ creation_failed: 2 })).toEqual({
      active: 0,
      suspended: 0,
      archived: 0,
      failed: 2,
    });
    expect(healthCounts({ deletion_failed: 4 })).toEqual({
      active: 0,
      suspended: 0,
      archived: 0,
      failed: 4,
    });
  });

  it('全状态混合', () => {
    expect(
      healthCounts({ active: 5, suspended: 1, archived: 2, creation_failed: 3, deletion_failed: 1 }),
    ).toEqual({ active: 5, suspended: 1, archived: 2, failed: 4 });
  });
});

describe('filterProjects（轨道搜索）', () => {
  const projects: Project[] = [
    makeProject({ id: 'a', name: 'ocdeck', path: '/Users/x/code/ocdeck', kind: 'repo' }),
    makeProject({ id: 'b', name: 'blog-next', path: '/Users/x/code/blog-next', kind: 'repo' }),
    makeProject({ id: 'c', name: 'scratch-notes', path: '/Users/x/notes/scratch', kind: 'dir' }),
  ];

  it('空 query 返回全部', () => {
    expect(filterProjects(projects, '')).toHaveLength(3);
    expect(filterProjects(projects, '   ')).toHaveLength(3);
  });

  it('按名称匹配（大小写不敏感）', () => {
    expect(filterProjects(projects, 'ocdeck').map((p) => p.id)).toEqual(['a']);
    expect(filterProjects(projects, 'BLOG').map((p) => p.id)).toEqual(['b']);
  });

  it('按路径匹配', () => {
    expect(filterProjects(projects, '/notes/').map((p) => p.id)).toEqual(['c']);
    expect(filterProjects(projects, 'code').map((p) => p.id)).toEqual(['a', 'b']);
  });

  it('按类型匹配（纯目录/dir）', () => {
    expect(filterProjects(projects, '纯目录').map((p) => p.id)).toEqual(['c']);
    expect(filterProjects(projects, 'dir').map((p) => p.id)).toEqual(['c']);
    expect(filterProjects(projects, '仓库').map((p) => p.id)).toEqual(['a', 'b']);
    expect(filterProjects(projects, 'repo').map((p) => p.id)).toEqual(['a', 'b']);
  });

  it('无匹配返回空数组', () => {
    expect(filterProjects(projects, 'zzz不存在')).toEqual([]);
  });

  it('部分匹配返回子集', () => {
    expect(filterProjects(projects, 'next').map((p) => p.id)).toEqual(['b']);
  });

  it('空列表对任何 query 返回空', () => {
    expect(filterProjects([], 'ocdeck')).toEqual([]);
    expect(filterProjects([], '')).toEqual([]);
  });
});
/* ============================ 健康摘要 chip 状态过滤（用户已确认方案） ============================ */

function makeTask(id: string, status: string): Task {
  return {
    id,
    project_id: 'p1',
    project_kind: 'repo',
    name: id,
    branch: 'main',
    status,
    worktree_path: '/tmp/x',
    init_status: 'succeeded',
    created_at: 0,
    updated_at: 0,
  };
}

describe('filterTasksByStatus（状态过滤）', () => {
  const tasks = [
    makeTask('a1', 'active'),
    makeTask('s1', 'suspended'),
    makeTask('a2', 'active'),
    makeTask('r1', 'archived'),
  ];

  it('null/空串显示全量', () => {
    expect(filterTasksByStatus(tasks, null)).toBe(tasks);
    expect(filterTasksByStatus(tasks, '')).toBe(tasks);
  });

  it('选中状态只显示该状态任务', () => {
    expect(filterTasksByStatus(tasks, 'active').map((t) => t.id)).toEqual(['a1', 'a2']);
    expect(filterTasksByStatus(tasks, 'suspended').map((t) => t.id)).toEqual(['s1']);
    expect(filterTasksByStatus(tasks, 'archived').map((t) => t.id)).toEqual(['r1']);
  });

  it('无匹配返回空数组（对应"无活跃任务"空态文案由调用方按 label 拼）', () => {
    expect(filterTasksByStatus(tasks, 'archived').filter((t) => t.status === 'active')).toEqual([]);
    expect(filterTasksByStatus([], 'active')).toEqual([]);
    // 空态文案素材：label 覆盖三档
    expect(STATUS_FILTER_LABEL).toEqual({ active: '活跃', suspended: '挂起', archived: '归档' });
  });
});

describe('toggleStatusFilter（chip 单选切换）', () => {
  it('未选中时点选 → 选中', () => {
    expect(toggleStatusFilter(null, 'active')).toBe('active');
  });
  it('点当前选中项 → 取消（恢复全量）', () => {
    expect(toggleStatusFilter('active', 'active')).toBeNull();
  });
  it('点另一状态 → 切换选中', () => {
    expect(toggleStatusFilter('active', 'suspended')).toBe('suspended');
    expect(toggleStatusFilter('suspended', 'archived')).toBe('archived');
  });
});
