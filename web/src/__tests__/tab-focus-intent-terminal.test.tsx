// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act } from 'react';
import { mount, flushUI, rerender, stubMatchMedia } from './cm-test-env';
import type * as TFI from '../terminal/tab-focus-intent';
import type { TerminalView as TerminalViewType } from '../terminal/TerminalView';
import type { Task } from '../types';

/* ======== tab-local 聚焦 intent：消费侧（fix-ai-auto-permission-and-tab-focus D3 / tasks 5.2） ========
 * 直挂 TerminalView（session mock、focusIntent prop 手工构造）覆盖门禁矩阵；
 * 工作台集成段用真实 tabstrip 点击驱动（生成 → prop → 消费全链路）。
 * 全局导航协议回归由 navigation-focus.test.tsx 守护。 */

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
    /** 测试驱动：模拟连接状态迁移（真实由 WS 事件驱动） */
    emitState(s: string): void {
      this.onStateCb(s);
    }
    setLocked(v: boolean): void {
      this.locked = v;
      for (const cb of this.lockCbs) cb(v);
    }
    // 文件投递端口（TerminalView 挂载 TUI 实例时会订阅；本套件不触发投递流程）
    getConnId(): string | null {
      return null;
    }
    fileGateOpen(): boolean {
      return false;
    }
    fileGateMessage(): string {
      return '';
    }
    sendDeliver(_uploadId: string): boolean {
      return false;
    }
    onConnEvent(_cb: (ev: unknown) => void): () => void {
      return () => {};
    }
    onDeliverResult(_cb: (r: unknown) => void): () => void {
      return () => {};
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

let TerminalView: typeof TerminalViewType;
let TaskWorkbenchPage: typeof import('../pages/TaskWorkbenchPage')['TaskWorkbenchPage'];

let seqCounter = 0;
function mkIntent(target: string, over: Partial<TFI.TabFocusIntent> = {}): TFI.TabFocusIntent {
  return { seq: ++seqCounter, ts: Date.now(), target, ...over };
}

function lastSession(): InstanceType<typeof sessionMock> {
  return sessionMock.instances[sessionMock.instances.length - 1];
}

function sess(i: number): InstanceType<typeof sessionMock> {
  return sessionMock.instances[i];
}

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

function tabButton(container: HTMLElement, text: string): HTMLButtonElement {
  const btn = [...container.querySelectorAll<HTMLButtonElement>('.tabstrip button')].find(
    (b) => b.textContent === text,
  );
  if (!btn) throw new Error(`tab not found: ${text}`);
  return btn;
}

function shellTabSpan(container: HTMLElement, display: string): HTMLElement {
  const label = [...container.querySelectorAll<HTMLButtonElement>('.tabstrip .tab-label')].find(
    (b) => b.textContent === display,
  );
  if (!label) throw new Error(`shell tab not found: ${display}`);
  return label.parentElement as HTMLElement;
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
  sessionMock.instances.length = 0;
  taskSub = null;
  shellIds = [];
  seqCounter = 0;
  stubMatchMedia(false);
  ({ TerminalView } = await import('../terminal/TerminalView'));
  ({ TaskWorkbenchPage } = await import('../pages/TaskWorkbenchPage'));
});

describe('TerminalView tab-local intent 消费（直挂）', () => {
  it('target 不匹配不消费；匹配后（新 seq）消费聚焦', () => {
    const root = mount(
      <TerminalView wsPath="/ws/terminal/t1" active focusIntent={mkIntent('sh1')} />,
    );
    act(() => lastSession().emitState('connected'));
    expect(lastSession().focusCalls).toBe(0);
    rerender(
      root.root,
      <TerminalView wsPath="/ws/terminal/t1" active focusIntent={mkIntent('tui')} />,
    );
    expect(lastSession().focusCalls).toBe(1);
    root.unmount();
  });

  it('active=false 不消费；active 恢复后消费（同一 intent）', () => {
    const it = mkIntent('tui');
    const root = mount(<TerminalView wsPath="/ws/terminal/t1" active={false} focusIntent={it} />);
    act(() => lastSession().emitState('connected'));
    expect(lastSession().focusCalls).toBe(0);
    rerender(root.root, <TerminalView wsPath="/ws/terminal/t1" active focusIntent={it} />);
    expect(lastSession().focusCalls).toBe(1);
    root.unmount();
  });

  it('未就绪挂起等待，connected 后聚焦', () => {
    const root = mount(
      <TerminalView wsPath="/ws/terminal/shell/s1" active focusIntent={mkIntent('s1')} />,
    );
    expect(lastSession().focusCalls).toBe(0);
    act(() => lastSession().emitState('connected'));
    expect(lastSession().focusCalls).toBe(1);
    root.unmount();
  });

  it('重复点击（新 seq）仍聚焦；同一 seq 不重复聚焦、重连不补聚焦', () => {
    const root = mount(<TerminalView wsPath="/ws/terminal/t1" active focusIntent={null} />);
    act(() => lastSession().emitState('connected'));
    expect(lastSession().focusCalls).toBe(0);
    rerender(
      root.root,
      <TerminalView wsPath="/ws/terminal/t1" active focusIntent={mkIntent('tui')} />,
    );
    expect(lastSession().focusCalls).toBe(1);
    // 重复点击 → 新 seq → 再聚焦
    rerender(
      root.root,
      <TerminalView wsPath="/ws/terminal/t1" active focusIntent={mkIntent('tui')} />,
    );
    expect(lastSession().focusCalls).toBe(2);
    // 同一 intent 不因状态迁移重复生效；普通重连不再聚焦
    act(() => lastSession().emitState('connecting'));
    act(() => lastSession().emitState('connected'));
    expect(lastSession().focusCalls).toBe(2);
    root.unmount();
  });

  it('锁定：消费但不聚焦，解锁不补抢；后续新 intent 正常聚焦', () => {
    const root = mount(<TerminalView wsPath="/ws/terminal/t1" active focusIntent={null} />);
    act(() => lastSession().emitState('connected'));
    act(() => lastSession().setLocked(true));
    rerender(
      root.root,
      <TerminalView wsPath="/ws/terminal/t1" active focusIntent={mkIntent('tui')} />,
    );
    expect(lastSession().focusCalls).toBe(0);
    act(() => lastSession().setLocked(false));
    expect(lastSession().focusCalls).toBe(0);
    // 再次点击（新 seq）→ 已解锁 → 聚焦
    rerender(
      root.root,
      <TerminalView wsPath="/ws/terminal/t1" active focusIntent={mkIntent('tui')} />,
    );
    expect(lastSession().focusCalls).toBe(1);
    root.unmount();
  });

  it('过期（5s TTL）：不聚焦、取消', () => {
    const expired = mkIntent('tui', { ts: Date.now() - 5000 });
    const root = mount(<TerminalView wsPath="/ws/terminal/t1" active focusIntent={expired} />);
    act(() => lastSession().emitState('connected'));
    expect(lastSession().focusCalls).toBe(0);
    root.unmount();
  });

  it('用户焦点已进入输入区：取消，不抢占', () => {
    const root = mount(<TerminalView wsPath="/ws/terminal/t1" active focusIntent={null} />);
    const input = document.createElement('input');
    document.body.appendChild(input);
    input.focus();
    rerender(
      root.root,
      <TerminalView wsPath="/ws/terminal/t1" active focusIntent={mkIntent('tui')} />,
    );
    act(() => lastSession().emitState('connected'));
    expect(lastSession().focusCalls).toBe(0);
    root.unmount();
    input.remove();
  });

  it('tab-local 白名单：焦点在 .od-sidebar（全局白名单成员）不放行；回到 body 后新 intent 聚焦', () => {
    const root = mount(<TerminalView wsPath="/ws/terminal/t1" active focusIntent={null} />);
    act(() => lastSession().emitState('connected'));
    const sidebar = document.createElement('div');
    sidebar.className = 'od-sidebar';
    const sidebarBtn = document.createElement('button');
    sidebar.appendChild(sidebarBtn);
    document.body.appendChild(sidebar);
    sidebarBtn.focus();
    rerender(
      root.root,
      <TerminalView wsPath="/ws/terminal/t1" active focusIntent={mkIntent('tui')} />,
    );
    expect(lastSession().focusCalls).toBe(0);
    sidebarBtn.remove(); // activeElement 回到 body
    rerender(
      root.root,
      <TerminalView wsPath="/ws/terminal/t1" active focusIntent={mkIntent('tui')} />,
    );
    expect(lastSession().focusCalls).toBe(1);
    root.unmount();
    sidebar.remove();
  });
});

describe('工作台集成（tabstrip 点击 → 生成 → 消费全链路）', () => {
  it('点击已激活已连接「终端」→ 聚焦；重复点击仍聚焦', async () => {
    const { container, unmount } = await renderWorkbench();
    act(() => sess(0).emitState('connected'));
    await act(async () => {
      tabButton(container, '终端').click();
    });
    expect(sess(0).focusCalls).toBe(1);
    await act(async () => {
      tabButton(container, '终端').click();
    });
    expect(sess(0).focusCalls).toBe(2);
    unmount();
  });

  it('点击「shell 1」（未就绪）等待，connected 后聚焦该 shell', async () => {
    const { container, unmount } = await renderWorkbench(['sh1']);
    await act(async () => {
      tabButton(container, 'shell 1').click();
    });
    expect(sess(1).focusCalls).toBe(0);
    act(() => sess(1).emitState('connected'));
    expect(sess(1).focusCalls).toBe(1);
    unmount();
  });

  it('切走取消：等待期间切到 Git，随后 connected 不聚焦；切回后新 intent 正常聚焦', async () => {
    const { container, unmount } = await renderWorkbench(['sh1']);
    await act(async () => {
      tabButton(container, 'shell 1').click();
    });
    await act(async () => {
      tabButton(container, 'Git').click();
    });
    act(() => sess(1).emitState('connected'));
    expect(sess(1).focusCalls).toBe(0);
    await act(async () => {
      tabButton(container, 'shell 1').click();
    });
    expect(sess(1).focusCalls).toBe(1);
    unmount();
  });

  it('目标销毁不误投：关闭 pending 的 shell 后，其他终端不消费其 intent', async () => {
    const { container, unmount } = await renderWorkbench(['sh1', 'sh2']);
    // sh1 的 intent 尚未消费（未 connected）即关闭该 shell
    await act(async () => {
      tabButton(container, 'shell 1').click();
    });
    await act(async () => {
      shellTabSpan(container, 'shell 1').querySelector<HTMLButtonElement>('.tab-close')!.click();
    });
    await flushUI();
    // 关闭后回到 TUI；再点 sh2（显示名重排为 shell 1）→ 仅 sh2 自己的 intent 生效一次
    await act(async () => {
      tabButton(container, 'shell 1').click();
    });
    act(() => sess(2).emitState('connected'));
    expect(sess(2).focusCalls).toBe(1);
    expect(sess(0).focusCalls).toBe(0);
    unmount();
  });

  it('Git / 设置点击后无任何终端聚焦（含后续 connected）', async () => {
    const { container, unmount } = await renderWorkbench(['sh1']);
    act(() => sess(0).emitState('connected'));
    act(() => sess(1).emitState('connected'));
    await act(async () => {
      tabButton(container, 'Git').click();
    });
    await act(async () => {
      tabButton(container, '设置').click();
    });
    expect(sess(0).focusCalls).toBe(0);
    expect(sess(1).focusCalls).toBe(0);
    // 终端点击仍正常生成 + 消费（机制未被破坏）
    await act(async () => {
      tabButton(container, '终端').click();
    });
    expect(sess(0).focusCalls).toBe(1);
    unmount();
  });
});
