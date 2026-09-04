// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act } from 'react';
import { TaskWorkbenchPage } from '../pages/TaskWorkbenchPage';
import { subscribeTask } from '../sse';
import { mount, flushUI } from './cm-test-env';
import type { Project, Task } from '../types';

/* ==================== 工作台 Git tab 降级（add-local-path-task-mode tasks 5.7） ====================
 * 覆盖 task-lifecycle delta：repo local-path 任务与 dir 任务同级降级——Git tab 隐藏
 * （含已驻留 Git tab 时回退 TUI）、页头不展示分支名；按 task.mode 判定，MUST NOT 判空推断。 */

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

describe('TaskWorkbenchPage Git tab 降级（repo local-path，add-local-path-task-mode 5.7）', () => {
  it('回归基线：worktree 任务页头展示分支名、Git tab 存在', () => {
    const { container, unmount } = mount(<TaskWorkbenchPage taskID="t1" />);
    act(() => taskSub!.onData(makeTask({})));
    expect(container.querySelector('.header-meta')?.textContent).toContain('ocdeck/demo');
    expect(gitTab(container)).not.toBeUndefined();
    unmount();
  });

  it('local-path 任务：Git tab 隐藏、页头不展示分支名（落库 branch 恒空亦然）', () => {
    const { container, unmount } = mount(<TaskWorkbenchPage taskID="t1" />);
    // 现实帧：branch 恒为空（D3）
    act(() => taskSub!.onData(makeTask({ mode: 'local-path', branch: '' })));
    expect(gitTab(container)).toBeUndefined();
    expect(container.querySelector('.header-meta')).toBeNull();
    expect(activeTab(container)!.textContent).toBe('终端');

    // 钉住 !isGitless 守卫本身：即使携带非空 branch 也不展示（数据不变量由后端保证）
    act(() => taskSub!.onData(makeTask({ mode: 'local-path', branch: 'ocdeck/demo' })));
    expect(gitTab(container)).toBeUndefined();
    expect(container.querySelector('.header-meta')).toBeNull();
    unmount();
  });

  it('已驻留 Git tab 时收到 local-path 帧回退 TUI', async () => {
    const { container, unmount } = mount(<TaskWorkbenchPage taskID="t1" />);
    act(() => taskSub!.onData(makeTask({ mode: 'worktree' })));
    expect(gitTab(container)).not.toBeUndefined();

    // 用户切到 Git tab
    await act(async () => {
      gitTab(container)!.click();
    });
    await flushUI();
    expect(activeTab(container)!.textContent).toBe('Git');

    // 后续帧按 mode 判定降级：Git tab 移除，回到 TUI tab
    act(() => taskSub!.onData(makeTask({ mode: 'local-path', branch: '' })));
    await flushUI();
    expect(gitTab(container)).toBeUndefined();
    expect(activeTab(container)!.textContent).toBe('终端');
    unmount();
  });

  it('dir 任务同样降级（project_kind=dir，既有行为回归）', () => {
    const { container, unmount } = mount(<TaskWorkbenchPage taskID="t1" />);
    act(() => taskSub!.onData(makeTask({ project_kind: 'dir', mode: 'local-path', branch: '' })));
    expect(gitTab(container)).toBeUndefined();
    expect(container.querySelector('.header-meta')).toBeNull();
    unmount();
  });
});
