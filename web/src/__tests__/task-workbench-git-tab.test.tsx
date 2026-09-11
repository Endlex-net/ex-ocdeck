// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act } from 'react';
import { TaskWorkbenchPage } from '../pages/TaskWorkbenchPage';
import { subscribeTask } from '../sse';
import { mount, flushUI } from './cm-test-env';
import type { Project, Task } from '../types';

/* ==================== 工作台 Git 能力判定（add-local-path-task-mode 6.2/D8 反向） ====================
 * 判定拆分：Git tab/面板入口仅按 project_kind==='dir' 隐藏——repo local-path 任务显示 Git tab
 * （已驻留不回退 TUI）；workbench-base-ref-and-overflow D3 起，local-path 页头分支区改由
 * api.gitStatus 当前分支驱动（本测试 mock 返回空串——降级形态下不展示，聚焦 Git 能力判定；
 * task.branch 异常非空也不再成为页头数据源），非空分支展示见 task-workbench-header-branch.test.tsx。 */

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
  api: {
    listTerminals: vi.fn(async () => []),
    // 页头分支区数据源（workbench-base-ref-and-overflow D3）：空串 = 降级不展示
    gitStatus: vi.fn(async () => ({ branch: '', files: [] })),
  },
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
    base_ref: '',
    status: 'active',
    worktree_path: '/tmp/wt',
    mode: 'worktree',
    permission_mode: 'ask',
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

  it('repo local-path：Git tab 显示；页头分支区由 gitStatus 驱动（空串降级不展示）', () => {
    const { container, unmount } = mount(<TaskWorkbenchPage taskID="t1" />);
    // 现实帧：branch 恒为空（D3）
    act(() => taskSub!.onData(makeTask({ mode: 'local-path', branch: '' })));
    expect(gitTab(container)).not.toBeUndefined();
    expect(container.querySelector('.header-meta')).toBeNull();
    expect(activeTab(container)!.textContent).toBe('终端');

    // 防御：异常携带非空 branch 时页头仍不以其为数据源（按 kind+mode 判定），Git tab 不受影响
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
