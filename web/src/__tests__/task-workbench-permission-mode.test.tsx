// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act } from 'react';
import { TaskWorkbenchPage } from '../pages/TaskWorkbenchPage';
import { subscribeTask } from '../sse';
import { mount } from './cm-test-env';
import type { Task, TaskPermissionMode } from '../types';

/* ==================== TaskWorkbenchPage 权限模式徽标接线（task-permission-mode P3 gate F1） ====================
 * 走真实页面渲染路径（详情流 onData → 页头徽标组），直接保护 TaskWorkbenchPage 的
 * PermissionModeBadge 接线：非缺省 permission_mode 必须在页头徽标组显示对应定稿文案。 */

type TaskSubOpts = {
  onData: (t: Task) => void;
  onError: (m: string) => void;
  onGone: () => void;
};
let taskSub: TaskSubOpts | null = null;

vi.mock('../sse', () => ({
  subscribeTask: vi.fn((_id: string, opts: TaskSubOpts) => {
    taskSub = opts;
    return { close: vi.fn() };
  }),
}));

vi.mock('../api', () => ({
  api: { listTerminals: vi.fn(async () => []) },
  ApiError: class ApiError extends Error {
    constructor(
      public code: string,
      message: string,
    ) {
      super(message);
    }
  },
}));

vi.mock('../hooks', () => ({
  useMediaQuery: () => false,
  useProjects: () => ({ projects: [] }),
  useProjectsRefresh: () => vi.fn(async () => {}),
}));

/* 终端/面板重依赖打桩：本测试只关心页头徽标组，不加载 xterm/git 面板。 */
vi.mock('../terminal/TerminalView', () => ({ TerminalView: () => null }));
vi.mock('../components/GitPanel', () => ({ GitPanel: () => null }));
vi.mock('../components/EnvEditor', () => ({ EnvEditor: () => null }));

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

/** 页头徽标组内按文案定位徽标（与状态/init/agent 徽标同组，按定稿文案区分）。
 *  未命中归一为 null（find 的 undefined 会被 not.toBeNull 放过）。 */
function headerBadge(container: HTMLElement, label: string) {
  return (
    [...container.querySelectorAll('.page-header .badge')].find((b) => b.textContent === label) ??
    null
  );
}

beforeEach(() => {
  taskSub = null;
  vi.mocked(subscribeTask).mockClear();
});

describe('TaskWorkbenchPage 权限模式徽标接线（P3 gate F1）', () => {
  it.each([
    ['all-approve', '全部批准'],
    ['ai-auto', 'AI 自动识别'],
    ['ask', '人工批准'],
  ] as const)('详情流 task.permission_mode=%s → 页头徽标组显示「%s」', (mode, label) => {
    const { container, unmount } = mount(<TaskWorkbenchPage taskID="t1" />);
    expect(taskSub).not.toBeNull();
    act(() => taskSub!.onData(makeTask(mode)));
    const badge = headerBadge(container, label);
    expect(badge).not.toBeNull();
    expect(badge!.className).toContain('badge-muted');
    expect(badge!.getAttribute('title')).toBe(`权限模式（创建后不可修改）：${mode}`);
    unmount();
  });

  it('流推送不变更权限模式（创建后不可修改）：连续两帧徽标文案稳定', () => {
    const { container, unmount } = mount(<TaskWorkbenchPage taskID="t1" />);
    act(() => taskSub!.onData(makeTask('all-approve')));
    expect(headerBadge(container, '全部批准')).not.toBeNull();
    act(() => taskSub!.onData(makeTask('all-approve')));
    expect(headerBadge(container, '全部批准')).not.toBeNull();
    unmount();
  });
});
