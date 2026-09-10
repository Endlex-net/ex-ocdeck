// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act } from 'react';
import type { Root } from 'react-dom/client';
import { CommandCenterPage } from '../pages/CommandCenterPage';
import { ProjectsManagePage } from '../pages/ProjectsManagePage';
import { TaskWorkbenchPage } from '../pages/TaskWorkbenchPage';
import { api } from '../api';
import { subscribeTask } from '../sse';
import { mount, flushUI, stubMatchMedia } from './cm-test-env';
import type { Project, Task, TaskSummary } from '../types';

/* ==================== gitless 任务分支名防御（Z-F1，git-operations delta） ====================
 * 防御性契约：mode=local-path（或 dir）任务即使携带非空 branch（合法数据下恒空，此处钉住
 * 显式按 kind+mode 判定而非依赖持久化字段为空）也不渲染分支名/分支图标：
 * - 指挥中心三种任务行（需要关注/其余活跃/挂起归档）
 * - 项目页任务行（展示项目目录路径而非分支）
 * - 工作台移动任务切换器（gitless 行无分支名，worktree 行保留对照） */

type SessionsSubOpts = { onData: (items: never[]) => void; onError: (m: string) => void };
type TaskSubOpts = {
  onData: (t: Task) => void;
  onError: (m: string) => void;
  onGone: () => void;
};
let storeProjects: Project[] = [];
let taskSub: TaskSubOpts | null = null;
let mobile = false;

vi.mock('../sse', () => ({
  subscribeActiveSessions: vi.fn((_opts: SessionsSubOpts) => ({ close: vi.fn() })),
  subscribeTask: vi.fn((_id: string, opts: TaskSubOpts) => {
    taskSub = opts;
    return { close: vi.fn() };
  }),
}));

vi.mock('../api', () => ({
  api: {
    listBranches: vi.fn(async () => ['main']),
    refreshBranches: vi.fn(async () => ['main']),
    createTask: vi.fn(),
    listTerminals: vi.fn(async () => []),
    getProject: vi.fn(),
    listTasks: vi.fn(),
  },
  ApiError: class ApiError extends Error {
    constructor(
      public status: number,
      public code: string,
      message: string,
    ) {
      super(message);
    }
  },
}));

vi.mock('../hooks', () => ({
  useProjects: () => ({ projects: storeProjects, loading: false, initialized: true, error: '' }),
  useProjectsRefresh: () => vi.fn(async () => {}),
  useMediaQuery: (q: string) => (q.includes('767') ? mobile : false),
  runProjectMutation: vi.fn(),
  createErrorMessage: (err: unknown) => (err instanceof Error ? err.message : String(err)),
  deleteErrorMessage: (err: unknown) => (err instanceof Error ? err.message : String(err)),
}));

/* 终端/面板/配置编辑器重依赖打桩：本测试只关心任务行与切换器渲染。 */
vi.mock('../terminal/TerminalView', () => ({ TerminalView: () => null }));
vi.mock('../components/GitPanel', () => ({ GitPanel: () => null }));
vi.mock('../components/EnvEditor', () => ({ EnvEditor: () => null }));
vi.mock('../components/LifecycleConfigEditor', () => ({ LifecycleConfigEditor: () => null }));

function summary(id: string, over: Partial<TaskSummary>): TaskSummary {
  return {
    id,
    name: `task-${id}`,
    status: 'active',
    init_status: 'none',
    branch: '',
    worktree_path: '/tmp/wt',
    mode: 'worktree',
    permission_mode: 'ask',
    updated_at: 2,
    attention_count: 0,
    ...over,
  };
}

function makeTask(over: Partial<Task>): Task {
  return {
    id: 't1',
    project_id: 'p1',
    project_kind: 'repo',
    name: 'demo-task',
    branch: '',
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

function makeProject(id: string, kind: 'repo' | 'dir', tasks: TaskSummary[]): Project {
  return {
    id,
    name: 'proj',
    path: '/abs/proj',
    kind,
    default_branch: 'main',
    created_at: 1,
    task_count: tasks.length,
    tasks_by_status: {},
    tasks,
  };
}

/** 任务行副标题断言：gitless 行只含项目名、无 svg（分支图标整段移除）。 */
function expectGitlessRowSub(sub: Element, projectName: string) {
  expect(sub.textContent).toBe(projectName);
  expect(sub.querySelectorAll('svg')).toHaveLength(0);
}

const roots: Root[] = [];

beforeEach(() => {
  stubMatchMedia(false);
  storeProjects = [];
  taskSub = null;
  mobile = false;
  vi.mocked(subscribeTask).mockClear();
});

afterEach(async () => {
  while (roots.length) {
    const root = roots.pop()!;
    await act(async () => {
      root.unmount();
    });
  }
});

function renderPage(ui: React.ReactElement) {
  const utils = mount(ui);
  roots.push(utils.root);
  return utils;
}

describe('指挥中心任务行（Z-F1）', () => {
  it('local-path 任务携带非空 branch：三种任务行均不渲染分支名/分支图标', async () => {
    // 需要关注（creation_failed）/其余活跃/挂起 三区各一行 local-path 任务
    storeProjects = [
      makeProject('p1', 'repo', [
        summary('t-fail', { status: 'creation_failed', mode: 'local-path', branch: 'ocdeck/failed' }),
        summary('t-live', { status: 'active', mode: 'local-path', branch: 'ocdeck/live' }),
        summary('t-park', { status: 'suspended', mode: 'local-path', branch: 'ocdeck/parked' }),
      ]),
    ];
    const { container } = renderPage(<CommandCenterPage />);
    await flushUI();

    expect(container.querySelectorAll('.od-row-sub')).toHaveLength(3);
    for (const sub of container.querySelectorAll('.od-row-sub')) {
      expectGitlessRowSub(sub, 'proj');
    }
    expect(container.textContent).not.toContain('ocdeck/');
  });

  it('对照：worktree 任务行保持「项目名 · ⎇ 分支」结构', async () => {
    storeProjects = [
      makeProject('p1', 'repo', [summary('t-live', { status: 'active', branch: 'ocdeck/wt' })]),
    ];
    const { container } = renderPage(<CommandCenterPage />);
    await flushUI();

    const sub = container.querySelector('.od-row-sub')!;
    expect(sub.textContent).toContain('proj');
    expect(sub.textContent).toContain('ocdeck/wt');
    expect(sub.querySelectorAll('svg')).toHaveLength(1);
  });
});

describe('项目页任务行（Z-F1）', () => {
  it('local-path 任务携带非空 branch：展示项目目录路径而非分支名', async () => {
    window.location.hash = '#/projects#p1';
    const localPath = makeTask({
      id: 't-lp',
      name: 'task-t-lp',
      mode: 'local-path',
      branch: 'ocdeck/live',
      worktree_path: '/abs/proj',
    });
    const worktree = makeTask({
      id: 't-wt',
      name: 'task-t-wt',
      mode: 'worktree',
      branch: 'ocdeck/wt',
      worktree_path: '/abs/wt2',
    });
    storeProjects = [
      makeProject('p1', 'repo', [
        summary('t-lp', { mode: 'local-path', branch: 'ocdeck/live' }),
        summary('t-wt', { branch: 'ocdeck/wt' }),
      ]),
    ];
    vi.mocked(api.getProject).mockResolvedValue(makeProject('p1', 'repo', []));
    vi.mocked(api.listTasks).mockResolvedValue([localPath, worktree]);

    const { container } = renderPage(<ProjectsManagePage />);
    await flushUI();

    const subs = [...container.querySelectorAll('.od-row-sub .mono')].map((s) => s.textContent);
    expect(subs).toContain('/abs/proj'); // local-path → 项目目录路径
    expect(subs).toContain('ocdeck/wt'); // worktree 对照 → 分支名
    expect(container.textContent).not.toContain('ocdeck/live');
  });
});

describe('工作台移动任务切换器（Z-F1）', () => {
  it('local-path 任务携带非空 branch：切换器行不渲染分支名（worktree 行对照保留）', async () => {
    mobile = true;
    storeProjects = [
      makeProject('p1', 'repo', [
        summary('t-lp', { status: 'active', mode: 'local-path', branch: 'ocdeck/live' }),
        summary('t-wt', { status: 'active', branch: 'ocdeck/wt' }),
      ]),
    ];
    const { container } = renderPage(<TaskWorkbenchPage taskID="t-wt" />);
    await flushUI();
    act(() => taskSub!.onData(makeTask({ id: 't-wt' })));

    // 打开切换器（≤767px 页头任务切换器）
    await act(async () => {
      container.querySelector<HTMLButtonElement>('.wb-switcher-btn')!.click();
    });
    await flushUI();

    const items = [...container.querySelectorAll('.wb-sw-item')];
    expect(items).toHaveLength(2);
    const byName = (name: string) => items.find((i) => i.textContent?.includes(name))!;
    // gitless 行：无分支名
    expect(byName('task-t-lp').querySelector('.mono')).toBeNull();
    expect(byName('task-t-lp').textContent).not.toContain('ocdeck/live');
    // worktree 行对照：分支名保留
    expect(byName('task-t-wt').querySelector('.mono')!.textContent).toBe('ocdeck/wt');
  });
});
