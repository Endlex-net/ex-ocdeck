// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act } from 'react';
import { mount, flushUI, stubMatchMedia } from './cm-test-env';
import type * as FR from '../terminal/focus-request';
import type * as TFI from '../terminal/tab-focus-intent';
import type { Task } from '../types';

/* ======== tab-local 聚焦 intent：生成侧（fix-ai-auto-permission-and-tab-focus D3 / tasks 5.1） ========
 * TerminalView 换成探针组件记录 focusIntent prop——intent 是否生成只有 prop 可观测
 * （Git/设置 target 没有消费方，聚焦行为断言无法区分「生成了但无人消费」）。 */

const probe = vi.hoisted(() => {
  const records: Array<{ wsPath: string; focusIntent: unknown }> = [];
  return { records };
});

vi.mock('../terminal/TerminalView', () => ({
  TerminalView: (p: { wsPath: string; focusIntent: unknown }) => {
    probe.records.push({ wsPath: p.wsPath, focusIntent: p.focusIntent });
    return null;
  },
}));

type TaskSubOpts = {
  onData: (t: Task) => void;
  onError: (m: string) => void;
  onGone: () => void;
};
let taskSub: TaskSubOpts | null = null;
let shellIds: string[] = [];

vi.mock('../sse', () => ({
  subscribeTask: vi.fn((_id: string, opts: TaskSubOpts) => {
    taskSub = opts;
    return { close: vi.fn() };
  }),
  subscribeActiveSessions: vi.fn(() => ({ close: vi.fn() })),
}));

vi.mock('../api', () => ({
  api: {
    listTerminals: vi.fn(async () => shellIds.map((id) => ({ terminal_id: id }))),
    createTerminal: vi.fn(async () => ({ terminal_id: 'sh-new' })),
    closeTerminal: vi.fn(async () => {}),
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
  useMediaQuery: () => false,
  useProjects: () => ({ projects: [], initialized: true, error: '' }),
  useProjectsRefresh: () => vi.fn(async () => {}),
  createErrorMessage: (_prefix: string, err: unknown) =>
    err instanceof Error ? err.message : String(err),
}));

vi.mock('../components/GitPanel', () => ({ GitPanel: () => null }));
vi.mock('../components/EnvEditor', () => ({ EnvEditor: () => null }));
vi.mock('../components/TaskInfoCard', () => ({ TaskInfoCard: () => null }));

let TaskWorkbenchPage: typeof import('../pages/TaskWorkbenchPage')['TaskWorkbenchPage'];
let fr: typeof FR;
let tfi: typeof TFI;

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

/** 某 TerminalView 最近一次收到的 focusIntent prop（所有视图广播同一对象）。 */
function lastIntent(wsPath: string): TFI.TabFocusIntent | null {
  for (let i = probe.records.length - 1; i >= 0; i--) {
    if (probe.records[i].wsPath === wsPath) {
      return probe.records[i].focusIntent as TFI.TabFocusIntent | null;
    }
  }
  return null;
}

/** 全程出现过的 intent 是否只带有给定 target（null = 已取消/未生成，不算生成）。 */
function onlyTargets(...targets: string[]): boolean {
  return probe.records.every((r) => {
    const it = r.focusIntent as TFI.TabFocusIntent | null;
    return it === null || targets.includes(it.target);
  });
}

function tabButton(container: HTMLElement, text: string): HTMLButtonElement {
  const btn = [...container.querySelectorAll<HTMLButtonElement>('.tabstrip button')].find(
    (b) => b.textContent === text,
  );
  if (!btn) throw new Error(`tab not found: ${text}`);
  return btn;
}

/** shell tab 外层 span（label + close），按显示名定位（用户视角）。 */
function shellTabSpan(container: HTMLElement, display: string): HTMLElement {
  const label = [...container.querySelectorAll<HTMLButtonElement>('.tabstrip .tab-label')].find(
    (b) => b.textContent === display,
  );
  if (!label) throw new Error(`shell tab not found: ${display}`);
  return label.parentElement as HTMLElement;
}

function activeTab(container: HTMLElement): Element {
  const el = container.querySelector('.tabstrip .tab-active');
  if (!el) throw new Error('no active tab');
  return el;
}

async function renderWorkbench(shells: string[] = []) {
  shellIds = shells;
  const page = mount(<TaskWorkbenchPage taskID="t1" />);
  act(() => taskSub!.onData(makeTask({})));
  await flushUI();
  return page;
}

beforeEach(async () => {
  vi.resetModules();
  probe.records.length = 0;
  taskSub = null;
  shellIds = [];
  stubMatchMedia(false);
  fr = await import('../terminal/focus-request');
  tfi = await import('../terminal/tab-focus-intent');
  ({ TaskWorkbenchPage } = await import('../pages/TaskWorkbenchPage'));
});

describe('tabstrip 真实点击生成 intent（prop 下发）', () => {
  it('点击「终端」→ target=tui；重复点击 seq 单调递增（新覆盖旧）', async () => {
    const { container, unmount } = await renderWorkbench();
    await act(async () => {
      tabButton(container, '终端').click();
    });
    const i1 = lastIntent('/ws/terminal/t1');
    expect(i1).toMatchObject({ target: 'tui' });
    expect(i1!.seq).toBeGreaterThan(0);
    expect(typeof i1!.ts).toBe('number');
    await act(async () => {
      tabButton(container, '终端').click();
    });
    expect(lastIntent('/ws/terminal/t1')!.seq).toBe(i1!.seq + 1);
    unmount();
  });

  it('点击「shell 1」→ target=shell 稳定 id（非显示序号）；同一 intent 广播给 TUI 与对应 shell', async () => {
    const { container, unmount } = await renderWorkbench(['sh1']);
    await act(async () => {
      tabButton(container, 'shell 1').click();
    });
    const i = lastIntent('/ws/terminal/shell/sh1');
    expect(i!.target).toBe('sh1');
    expect(lastIntent('/ws/terminal/t1')).toBe(i);
    unmount();
  });
});

describe('Git/设置与程序性切换不生成 intent', () => {
  it('点击 Git / 设置：不生成 intent，且 pending intent 随切走取消（prop 变 null）', async () => {
    const { container, unmount } = await renderWorkbench(['sh1']);
    await act(async () => {
      tabButton(container, '终端').click();
    });
    expect(lastIntent('/ws/terminal/t1')).toMatchObject({ target: 'tui' });
    await act(async () => {
      tabButton(container, 'Git').click();
    });
    expect(lastIntent('/ws/terminal/t1')).toBeNull();
    expect(onlyTargets('tui', 'sh1')).toBe(true);
    await act(async () => {
      tabButton(container, '设置').click();
    });
    expect(lastIntent('/ws/terminal/t1')).toBeNull();
    expect(onlyTargets('tui', 'sh1')).toBe(true);
    unmount();
  });

  it('程序性切换全程零 intent：导航请求激活 TUI、addShell、closeShell 回退', async () => {
    const { container, unmount } = await renderWorkbench();
    // 程序性：全局导航请求 → 工作台订阅回调 switchTab(TUI)，只切标签不生成 intent
    await act(async () => {
      tabButton(container, 'Git').click();
    });
    act(() => fr.requestTerminalFocus('t1'));
    await flushUI();
    expect(activeTab(container).textContent).toBe('终端');
    // 程序性：addShell 成功后 setTab(新 shell)
    await act(async () => {
      container.querySelector<HTMLButtonElement>('.tab-add')!.click();
    });
    await flushUI();
    // 程序性：closeShell 关闭当前 shell → 回退 TUI
    await act(async () => {
      shellTabSpan(container, 'shell 1').querySelector<HTMLButtonElement>('.tab-close')!.click();
    });
    await flushUI();
    expect(activeTab(container).textContent).toBe('终端');
    // 从未点击终端类 tab：全程不应出现任何 intent
    expect(onlyTargets()).toBe(true);
    unmount();
  });
});

describe('模块契约（target 键 / TTL / 白名单）', () => {
  it('tabFocusTargetFromWsPath：TUI → tui、shell → 稳定 id、其他 → null', () => {
    expect(tfi.tabFocusTargetFromWsPath('/ws/terminal/t1')).toBe('tui');
    expect(tfi.tabFocusTargetFromWsPath('/ws/terminal/shell/s9')).toBe('s9');
    expect(tfi.tabFocusTargetFromWsPath('/ws/terminal/shell/')).toBeNull();
    expect(tfi.tabFocusTargetFromWsPath('/ws/other')).toBeNull();
  });

  it('isTabFocusTargetAllowed：body / tabstrip 内放行；输入元素与 tabstrip 外不放行（不放宽全局白名单）', () => {
    expect(tfi.isTabFocusTargetAllowed(document.body)).toBe(true);
    const strip = document.createElement('div');
    strip.className = 'tabstrip';
    const tabBtn = document.createElement('button');
    strip.appendChild(tabBtn);
    document.body.appendChild(strip);
    expect(tfi.isTabFocusTargetAllowed(tabBtn)).toBe(true);
    const input = document.createElement('input');
    document.body.appendChild(input);
    expect(tfi.isTabFocusTargetAllowed(input)).toBe(false);
    // 全局导航白名单放行 .od-sidebar，tab-local 白名单不放行
    const sidebar = document.createElement('div');
    sidebar.className = 'od-sidebar';
    const sidebarBtn = document.createElement('button');
    sidebar.appendChild(sidebarBtn);
    document.body.appendChild(sidebar);
    expect(tfi.isTabFocusTargetAllowed(sidebarBtn)).toBe(false);
    expect(tfi.isTabFocusTargetAllowed(null)).toBe(false);
    strip.remove();
    input.remove();
    sidebar.remove();
  });

  it('isTabFocusIntentExpired：沿用导航请求 5s TTL', () => {
    const now = 1_000_000;
    expect(tfi.isTabFocusIntentExpired({ seq: 1, ts: now - 4999, target: 'tui' }, now)).toBe(false);
    expect(tfi.isTabFocusIntentExpired({ seq: 1, ts: now - 5000, target: 'tui' }, now)).toBe(true);
  });
});
