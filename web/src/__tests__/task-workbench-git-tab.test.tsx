// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act } from 'react';
import { TaskWorkbenchPage } from '../pages/TaskWorkbenchPage';
import { subscribeTask } from '../sse';
import { mount, flushUI } from './cm-test-env';
import type { Project, Task } from '../types';

/* ==================== 工作台 Git 能力判定（add-local-path-task-mode 6.2/D8 反向） ====================
 * 判定拆分：Git tab/面板入口仅按 project_kind==='dir' 隐藏——repo local-path 任务显示 Git tab
 * （已驻留不回退 TUI）；页头分支名按分支展示判定 isGitlessTask（kind+mode）隐藏——local-path
 * 任务即使异常携带非空 branch 也不展示（D3 落库恒空），实时分支由 GitPanel 内提供。 */

type TaskSubOpts = {
  onData: (t: Task) => void;
  onError: (m: string) => void;
  onGone: () => void;
};
let taskSub: TaskSubOpts | null = null;
let storeProjects: Project[] = [];

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
  useProjects: () => ({ projects: storeProjects }),
  useProjectsRefresh: () => vi.fn(async () => {}),
}));

/* 终端/面板重依赖打桩：本测试只关心 tabstrip 与页头，不加载 xterm/git 面板。 */
vi.mock('../terminal/TerminalView', () => ({ TerminalView: () => null }));
vi.mock('../components/GitPanel', () => ({ GitPanel: () => null }));
vi.mock('../components/EnvEditor', () => ({ EnvEditor: () => null }));

function makeTask(over: Partial<Task>): Task {
  return {
    id: 't1',
    project_id: 'p1',
    project_kind: 'repo',
    name: 'demo-task',
    branch: 'ocdeck/demo',
    status: 'active',
    worktree_path: '/tmp/wt',
    mode: 'worktree',
    init_status: 'none',
    created_at: 1,
    updated_at: 2,
    ...over,
  };
}

function gitTab(container: HTMLElement) {
  return [...container.querySelectorAll<HTMLButtonElement>('.tabstrip button')].find(
    (b) => b.textContent === 'Git',
  );
}

function activeTab(container: HTMLElement) {
  return container.querySelector('.tabstrip .tab-active');
}

beforeEach(() => {
  taskSub = null;
  storeProjects = [];
  vi.mocked(subscribeTask).mockClear();
});

describe('TaskWorkbenchPage Git 能力判定（add-local-path-task-mode 6.2）', () => {
  it('repo worktree：Git tab 显示、页头展示分支名（对照基线）', () => {
    const { container, unmount } = mount(<TaskWorkbenchPage taskID="t1" />);
    act(() => taskSub!.onData(makeTask({})));
    expect(container.querySelector('.header-meta')?.textContent).toContain('ocdeck/demo');
    expect(gitTab(container)).not.toBeUndefined();
    unmount();
  });

  it('repo local-path：Git tab 显示、页头不展示分支名（落库 branch 恒空，GitPanel 内实时分支）', () => {
    const { container, unmount } = mount(<TaskWorkbenchPage taskID="t1" />);
    // 现实帧：branch 恒为空（D3）
    act(() => taskSub!.onData(makeTask({ mode: 'local-path', branch: '' })));
    expect(gitTab(container)).not.toBeUndefined();
    expect(container.querySelector('.header-meta')).toBeNull();
    expect(activeTab(container)!.textContent).toBe('终端');

    // 防御：异常携带非空 branch 时页头仍隐藏（分支展示按 isGitlessTask），Git tab 不受影响
    act(() => taskSub!.onData(makeTask({ mode: 'local-path', branch: 'ocdeck/demo' })));
    expect(container.querySelector('.header-meta')).toBeNull();
    expect(gitTab(container)).not.toBeUndefined();
    unmount();
  });

  it('已驻留 Git tab 时收到 local-path 帧不回退（Git 能力不再按 mode 判定）', async () => {
    const { container, unmount } = mount(<TaskWorkbenchPage taskID="t1" />);
    act(() => taskSub!.onData(makeTask({ mode: 'worktree' })));
    expect(gitTab(container)).not.toBeUndefined();

    // 用户切到 Git tab
    await act(async () => {
      gitTab(container)!.click();
    });
    await flushUI();
    expect(activeTab(container)!.textContent).toBe('Git');

    // 后续 local-path 帧不降级：Git tab 与面板保留
    act(() => taskSub!.onData(makeTask({ mode: 'local-path', branch: '' })));
    await flushUI();
    expect(gitTab(container)).not.toBeUndefined();
    expect(activeTab(container)!.textContent).toBe('Git');
    unmount();
  });

  it('dir 任务仍隐藏 Git tab 与分支名；已驻留 Git tab 时回退 TUI（既有行为回归）', async () => {
    const { container, unmount } = mount(<TaskWorkbenchPage taskID="t1" />);
    // 先以 repo worktree 帧驻留 Git tab，再收到 dir 帧 → 回退 TUI
    act(() => taskSub!.onData(makeTask({ mode: 'worktree' })));
    await act(async () => {
      gitTab(container)!.click();
    });
    await flushUI();
    expect(activeTab(container)!.textContent).toBe('Git');

    act(() => taskSub!.onData(makeTask({ project_kind: 'dir', mode: 'local-path', branch: '' })));
    await flushUI();
    expect(gitTab(container)).toBeUndefined();
    expect(container.querySelector('.header-meta')).toBeNull();
    expect(activeTab(container)!.textContent).toBe('终端');
    unmount();
  });
});
