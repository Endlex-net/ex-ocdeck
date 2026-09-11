// @vitest-environment jsdom
import { describe, it, expect, afterEach } from 'vitest';
import { act } from 'react';
import type { Root } from 'react-dom/client';
import { PermissionModeBadge } from '../components/PermissionModeBadge';
import { mount } from './cm-test-env';
import type { Task, TaskPermissionMode } from '../types';

/* ==================== 任务详情权限模式只读展示（task-permission-mode tasks 5.2 / D7） ====================
 * 详情页头按 task.permission_mode 只读展示定稿三档文案；创建后不可修改，无修改入口。 */

function makeTask(permission_mode: TaskPermissionMode): Task {
  return {
    id: 't1',
    project_id: 'p1',
    project_kind: 'repo',
    name: 'demo-task',
    branch: 'main',
    base_ref: '',
    status: 'active',
    worktree_path: '/tmp/wt',
    mode: 'worktree',
    permission_mode,
    init_status: 'none',
    created_at: 1,
    updated_at: 2,
  };
}

const roots: Root[] = [];

afterEach(async () => {
  while (roots.length) {
    const root = roots.pop()!;
    await act(async () => {
      root.unmount();
    });
  }
});

describe('PermissionModeBadge（task-permission-mode 5.2）', () => {
  it.each([
    ['ask', '人工批准'],
    ['all-approve', '全部批准'],
    ['ai-auto', 'AI 自动识别'],
  ] as const)('%s → 只读展示「%s」', (mode, label) => {
    const { container, root } = mount(<PermissionModeBadge task={makeTask(mode)} />);
    roots.push(root);
    const badge = container.querySelector('.badge')!;
    expect(badge.textContent).toBe(label);
    expect(badge.className).toContain('badge-muted');
    // 只读标记：非交互元素，title 透出原始枚举值与不可修改语义
    expect(badge.tagName).toBe('SPAN');
    expect(badge.getAttribute('title')).toBe(`权限模式（创建后不可修改）：${mode}`);
  });
});
