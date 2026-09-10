// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act } from 'react';
import { mount, flushUI, stubMatchMedia } from './cm-test-env';
import type * as FR from '../terminal/focus-request';
import type { ActiveSessionItem, Project, Task } from '../types';

/* ============ 导航焦点请求：三个生产点 + 工作台标签激活（fix-terminal-input-panel-resize D3） ============
 * 导航本身仍走原生 href/navigate（jsdom 不真正路由），这里断言点击后单例收到请求信号；
 * 工作台用真实 TerminalView（session mock），覆盖「Git 标签 → 同任务请求 → TUI 激活 → connected 后聚焦」。 */

const sessionMock = vi.hoisted(() => {
  const instances: FakeTermSession[] = [];
  class FakeTermSession {
    static instances = instances;
    connect = vi.fn();
    disconnect = vi.fn();
    dispose = vi.fn();
    applyPreferences = vi.fn();
    active: unknown;
    focusCalls = 0;
    locked = false;
    private lockCbs = new Set<(v: boolean) => void>();
    private onStateCb: (s: string) => void;
    constructor(
      _host: HTMLElement,
      _wrap: HTMLElement,
      _wsPath: string,
      onState: (s: string) => void,
      _onClipboardWrite?: (text: string) => void,
    ) {
      this.onStateCb = onState;
      instances.push(this);
    }
    isLocked(): boolean {
      return this.locked;
    }
    onLockChange(cb: (v: boolean) => void): () => void {
      this.lockCbs.add(cb);
      return () => this.lockCbs.delete(cb);
    }
    focus(): void {
      this.focusCalls++;
    }
    emitState(s: string): void {
      this.onStateCb(s);
    }
    setLocked(v: boolean): void {
      this.locked = v;
      for (const cb of this.lockCbs) cb(v);
    }
  }
  return FakeTermSession;
});

vi.mock('../terminal/session', () => ({ TermSession: sessionMock }));

type TaskSubOpts = {
  onData: (t: Task) => void;
  onError: (m: string) => void;
  onGone: () => void;
};
type SessionsSubOpts = {
  onData: (items: ActiveSessionItem[]) => void;
  onError: (m: string) => void;
};
let taskSub: TaskSubOpts | null = null;
let sessionsSub: SessionsSubOpts | null = null;
let storeProjects: Project[] = [];
let storeIsMobile = false;

vi.mock('../sse', () => ({
  subscribeTask: vi.fn((_id: string, opts: TaskSubOpts) => {
    taskSub = opts;
    return { close: vi.fn() };
  }),
  subscribeActiveSessions: vi.fn((opts: SessionsSubOpts) => {
    sessionsSub = opts;
    return { close: vi.fn() };
  }),
}));

vi.mock('../api', () => ({
  api: {
    listTerminals: vi.fn(async () => []),
    refreshBranches: vi.fn(async () => []),
    createTask: vi.fn(),
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
  useMediaQuery: () => storeIsMobile,
  useProjects: () => ({ projects: storeProjects, initialized: true, error: '' }),
  useProjectsRefresh: () => vi.fn(async () => {}),
  createErrorMessage: (_prefix: string, err: unknown) =>
    err instanceof Error ? err.message : String(err),
}));

vi.mock('../components/ServerStatusBanner', () => ({ ServerStatusBanner: () => null }));
vi.mock('../components/GitPanel', () => ({ GitPanel: () => null }));
vi.mock('../components/EnvEditor', () => ({ EnvEditor: () => null }));

let fr: typeof FR;
let AppShell: typeof import('../components/AppShell')['AppShell'];
let CommandCenterPage: typeof import('../pages/CommandCenterPage')['CommandCenterPage'];
let TaskWorkbenchPage: typeof import('../pages/TaskWorkbenchPage')['TaskWorkbenchPage'];

function lastSession(): InstanceType<typeof sessionMock> {
  return sessionMock.instances[sessionMock.instances.length - 1];
}

function pendingReq(): FR.TerminalFocusRequest | null {
  const got: FR.TerminalFocusRequest[] = [];
  fr.subscribeTerminalFocus((r) => got.push(r));
  return got[0] ?? null;
}

function makeProjects(): Project[] {
  return [
    {
      id: 'p1',
      name: 'proj',
      path: '/tmp/proj',
      kind: 'repo',
      default_branch: 'main',
      created_at: 1,
      task_count: 3,
      tasks_by_status: { active: 2, suspended: 1 },
      tasks: [
        {
          id: 't1',
          name: 'attn-task',
          status: 'active',
          init_status: 'none',
          branch: 'main',
          worktree_path: '/tmp/wt1',
          mode: 'worktree',
          permission_mode: 'ask',
          updated_at: 2,
          agentStatus: 'busy',
          attention_count: 1,
        },
        {
          id: 't3',
          name: 'parked-task',
          status: 'suspended',
          init_status: 'none',
          branch: 'main',
          worktree_path: '/tmp/wt3',
          mode: 'worktree',
          permission_mode: 'ask',
          updated_at: 3,
          attention_count: 0,
        },
      ],
    },
  ];
}

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
    permission_mode: 'ask',
    init_status: 'none',
    created_at: 1,
    updated_at: 2,
    ...over,
  };
}

function makeSession(
  id: string,
  name: string,
  attention: ActiveSessionItem['attention'],
): ActiveSessionItem {
  return {
    task_id: id,
    project_id: 'p1',
    project_name: 'proj',
    name,
    branch: 'main',
    worktree_path: `/tmp/${id}`,
    mode: 'worktree',
    permission_mode: 'ask',
    last_active_at: 100,
    agentStatus: 'busy',
    attention,
  };
}

function renderShell() {
  return mount(
    <AppShell
      onOpenPalette={() => {}}
      onToggleTheme={() => {}}
      themePref="system"
      paletteHotkeyLabel="⌘K / Ctrl+K"
    >
      <div />
    </AppShell>,
  );
}

function gitTab(container: HTMLElement) {
  return [...container.querySelectorAll<HTMLButtonElement>('.tabstrip button')].find(
    (b) => b.textContent === 'Git',
  );
}

function activeTab(container: HTMLElement) {
  return container.querySelector('.tabstrip .tab-active');
}

function rowContaining(container: HTMLElement, text: string) {
  return [...container.querySelectorAll<HTMLElement>('.od-row-clickable')].find((el) =>
    el.textContent?.includes(text),
  );
}

beforeEach(async () => {
  vi.resetModules();
  sessionMock.instances.length = 0;
  taskSub = null;
  sessionsSub = null;
  storeProjects = [];
  storeIsMobile = false;
  stubMatchMedia(false);
  fr = await import('../terminal/focus-request');
  ({ AppShell } = await import('../components/AppShell'));
  ({ CommandCenterPage } = await import('../pages/CommandCenterPage'));
  ({ TaskWorkbenchPage } = await import('../pages/TaskWorkbenchPage'));
});

describe('导航焦点请求生产点（点击 → 发布信号，导航照常）', () => {
  it('侧栏任务项点击 → 发布该任务请求', () => {
    storeProjects = makeProjects();
    const { container, unmount } = renderShell();
    const link = container.querySelector<HTMLElement>('.od-nav-task');
    expect(link).not.toBeNull();
    link!.click();
    expect(pendingReq()?.taskID).toBe('t1');
    unmount();
  });

  it('指挥中心任务行点击 → 发布请求（需要关注行 / 其余活跃任务行 / 挂起与归档行）', () => {
    storeProjects = makeProjects();
    const { container, unmount } = mount(<CommandCenterPage />);
    act(() =>
      sessionsSub!.onData([
        makeSession('t1', 'attn-task', {
          permissions: [],
          questions: [{ id: 'q1', questions: [{ header: 'h', question: 'w' }], since: 1 }],
        }),
        makeSession('t2', 'plain-task', { permissions: [], questions: [] }),
      ]),
    );
    const attnRow = container.querySelector<HTMLElement>('.cc-attention .od-row-clickable');
    expect(attnRow).not.toBeNull();
    attnRow!.click();
    expect(pendingReq()?.taskID).toBe('t1');

    rowContaining(container, 'plain-task')!.click();
    expect(pendingReq()?.taskID).toBe('t2');

    rowContaining(container, 'parked-task')!.click();
    expect(pendingReq()?.taskID).toBe('t3');
    unmount();
  });
});

describe('工作台标签激活（只激活不消费）', () => {
  it('已驻留 TUI 时收到匹配请求 → 保持 TUI（不抖动）', () => {
    const { container, unmount } = mount(<TaskWorkbenchPage taskID="t1" />);
    act(() => taskSub!.onData(makeTask({})));
    act(() => fr.requestTerminalFocus('t1'));
    expect(activeTab(container)!.textContent).toBe('终端');
    unmount();
  });

  it('移动端任务切换器真实点击发信号（.wb-switcher-btn → .wb-sw-item）', async () => {
    storeIsMobile = true;
    storeProjects = makeProjects();
    const { container, unmount } = mount(<TaskWorkbenchPage taskID="t1" />);
    // 切换器随 task 渲染（加载阶段仅 wb-not-found）
    act(() => taskSub!.onData(makeTask({})));
    await act(async () => {
      container.querySelector<HTMLButtonElement>('.wb-switcher-btn')!.click();
    });
    const item = container.querySelector<HTMLButtonElement>('.wb-sw-item');
    expect(item).not.toBeNull();
    await act(async () => {
      item!.click();
    });
    // 第一个切换项是 t1（active 任务排前）
    expect(pendingReq()?.taskID).toBe('t1');
    unmount();
  });

  it('其他任务的请求不切标签（从 Git tab 出发断言不切走）', async () => {
    const { container, unmount } = mount(<TaskWorkbenchPage taskID="t1" />);
    act(() => taskSub!.onData(makeTask({})));
    await act(async () => {
      gitTab(container)!.click();
    });
    await flushUI();
    expect(activeTab(container)!.textContent).toBe('Git');
    // 其他任务的显式请求不影响本页标签
    act(() => fr.requestTerminalFocus('t-other'));
    await flushUI();
    expect(activeTab(container)!.textContent).toBe('Git');
    unmount();
  });

  it('加载阶段（task 未返回）离开目标任务 → 请求作废；返回后无新请求不聚焦', () => {
    act(() => fr.requestTerminalFocus('t1'));
    // 进入 t1 工作台：加载阶段无 TerminalView，仅工作台订阅存活
    const w1 = mount(<TaskWorkbenchPage taskID="t1" />);
    // 未等数据返回即离开
    w1.unmount();
    expect(pendingReq()).toBeNull();
    // 5s 内返回 t1，无新显式请求
    const w2 = mount(<TaskWorkbenchPage taskID="t1" />);
    act(() => taskSub!.onData(makeTask({})));
    act(() => lastSession().emitState('connected'));
    expect(lastSession().focusCalls).toBe(0);
    w2.unmount();
  });

  it('Git 标签 → 点击同任务 → TUI 激活 → connected 后聚焦（真实 TerminalView 消费）', async () => {
    const { container, unmount } = mount(<TaskWorkbenchPage taskID="t1" />);
    act(() => taskSub!.onData(makeTask({})));
    await act(async () => {
      gitTab(container)!.click();
    });
    await flushUI();
    expect(activeTab(container)!.textContent).toBe('Git');
    expect(lastSession().focusCalls).toBe(0);

    // 模拟点击侧栏/切换器/指挥中心同任务后的导航焦点请求
    act(() => fr.requestTerminalFocus('t1'));
    await flushUI();
    expect(activeTab(container)!.textContent).toBe('终端');

    // 标签激活只切 tab；聚焦仍等 TUI session connected（门禁③）
    expect(lastSession().focusCalls).toBe(0);
    act(() => lastSession().emitState('connected'));
    expect(lastSession().focusCalls).toBe(1);
    unmount();
    expect(pendingReq()).toBeNull();
  });
});
