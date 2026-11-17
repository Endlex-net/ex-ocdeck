// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { readFileSync } from 'node:fs';
import { StrictMode, act } from 'react';
import { mount, flushUI, rerender, stubMatchMedia } from './cm-test-env';
import { TaskWorkbenchPage } from '../pages/TaskWorkbenchPage';
import type { Project, Task } from '../types';

/* ============================ 网页全屏（Zen）工作台生命周期 ============================
 * 终端与 Git 面板换成探针：记录挂载/卸载与 props，并渲染真实 textarea/input 作为键盘事件源。
 * 任务轨使用真实 CompactTaskNav（经 projects 数据），不打桩。 */

const env = vi.hoisted(() => ({
  mobile: false,
  projects: [] as Project[],
  shells: [] as string[],
  taskSub: null as null | { onData: (t: Task) => void; onError: (m: string) => void; onGone: () => void },
  lifecycle: [] as string[],
  terminalProps: [] as Array<{ wsPath: string; active: boolean }>,
}));

vi.mock('../terminal/TerminalView', async () => {
  const { createElement, useEffect } = await import('react');
  return {
    TerminalView: (p: { wsPath: string; active: boolean }) => {
      useEffect(() => {
        env.lifecycle.push(`mount:${p.wsPath}`);
        return () => {
          env.lifecycle.push(`unmount:${p.wsPath}`);
        };
      }, []);
      env.terminalProps.push({ wsPath: p.wsPath, active: p.active });
      return createElement('textarea', { 'data-ws': p.wsPath });
    },
  };
});

vi.mock('../components/GitPanel', async () => {
  const { createElement, useEffect } = await import('react');
  return {
    GitPanel: () => {
      useEffect(() => {
        env.lifecycle.push('mount:git');
        return () => {
          env.lifecycle.push('unmount:git');
        };
      }, []);
      return createElement('input', { 'data-testid': 'git-control' });
    },
  };
});

vi.mock('../components/EnvEditor', () => ({ EnvEditor: () => null }));
vi.mock('../components/TaskInfoCard', () => ({ TaskInfoCard: () => null }));

vi.mock('../sse', () => ({
  subscribeTask: vi.fn((_id: string, opts: NonNullable<typeof env.taskSub>) => {
    env.taskSub = opts;
    return { close: vi.fn() };
  }),
  subscribeActiveSessions: vi.fn(() => ({ close: vi.fn() })),
}));

vi.mock('../api', () => ({
  api: {
    listTerminals: vi.fn(async () => env.shells.map((id) => ({ terminal_id: id }))),
    createTerminal: vi.fn(async () => ({ terminal_id: 'sh-new' })),
    closeTerminal: vi.fn(async () => {}),
    gitStatus: vi.fn(async () => ({ branch: 'ocdeck/demo' })),
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
  useMediaQuery: (q: string) => (q === '(max-width: 767px)' ? env.mobile : false),
  useProjects: () => ({ projects: env.projects, initialized: true, error: '' }),
  useProjectsRefresh: () => vi.fn(async () => {}),
  createErrorMessage: (_prefix: string, err: unknown) =>
    err instanceof Error ? err.message : String(err),
}));

const HOTKEY = 'mod+shift+f';

function makeTask(over: Partial<Task> = {}): Task {
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

function makeProjects(): Project[] {
  const task = (id: string, name: string) => ({
    id,
    name,
    status: 'active',
    init_status: 'none',
    branch: 'main',
    worktree_path: '/tmp/wt',
    mode: 'worktree' as const,
    permission_mode: 'ask' as const,
    updated_at: 2,
    agentStatus: 'idle',
    attention_count: 0,
  });
  return [
    {
      id: 'p1',
      name: 'proj',
      path: '/tmp/proj',
      kind: 'repo',
      default_branch: 'main',
      created_at: 1,
      task_count: 2,
      tasks_by_status: { active: 2 },
      tasks: [task('t1', 'demo-task'), task('t2', 'other-task')],
    },
  ];
}

async function renderPage(hotkey = HOTKEY) {
  const page = mount(<TaskWorkbenchPage taskID="t1" workbenchFullscreenHotkey={hotkey} />);
  act(() => env.taskSub!.onData(makeTask({})));
  await flushUI();
  return page;
}

/** 派发 keydown（cancelable + bubbles，window capture 先于目标处理）；返回事件以断言是否被消费。 */
function press(target: EventTarget, init: KeyboardEventInit & { keyCode?: number } = {}): KeyboardEvent {
  const { keyCode, ...rest } = init;
  const ev = new KeyboardEvent('keydown', {
    key: 'f',
    code: 'KeyF',
    metaKey: true,
    shiftKey: true,
    bubbles: true,
    cancelable: true,
    ...rest,
  });
  if (keyCode !== undefined) Object.defineProperty(ev, 'keyCode', { value: keyCode });
  act(() => {
    target.dispatchEvent(ev);
  });
  return ev;
}

function tuiInput(container: HTMLElement): HTMLElement {
  return container.querySelector<HTMLElement>('textarea[data-ws="/ws/terminal/t1"]')!;
}

function tabButton(container: HTMLElement, text: string): HTMLButtonElement {
  const btn = [...container.querySelectorAll<HTMLButtonElement>('.tabstrip-scroll button')].find(
    (b) => b.textContent === text,
  );
  if (!btn) throw new Error(`tab not found: ${text}`);
  return btn;
}

function zenButton(container: HTMLElement): HTMLButtonElement | null {
  return container.querySelector<HTMLButtonElement>('.tabstrip-actions button');
}

function click(el: HTMLElement) {
  act(() => {
    el.click();
  });
}

const isZen = (c: HTMLElement) => c.querySelector('.workbench-zen') !== null;
const bodyZen = () => document.body.classList.contains('od-workbench-zen');
const rail = (c: HTMLElement) => c.querySelector<HTMLElement>('.zen-task-rail');

beforeEach(() => {
  stubMatchMedia(false);
  env.mobile = false;
  env.projects = makeProjects();
  env.shells = [];
  env.taskSub = null;
  env.lifecycle.length = 0;
  env.terminalProps.length = 0;
});

afterEach(() => {
  document.body.classList.remove('od-workbench-zen');
});

describe('进入与退出（tabstrip 操作格）', () => {
  it('进入后 body 标记与任务轨同步，退出后全部撤销；pane 与 workbench-main 父节点不变', async () => {
    const { container, unmount } = await renderPage();
    const main = container.querySelector('.workbench-main');
    const pane = container.querySelector('.pane');
    expect(zenButton(container)!.textContent).toBe('zen');

    click(zenButton(container)!);
    expect(isZen(container)).toBe(true);
    expect(bodyZen()).toBe(true);
    expect(rail(container)).not.toBeNull();
    expect(zenButton(container)!.textContent).toBe('out zen');
    expect(container.querySelector('.workbench-main')).toBe(main);
    expect(container.querySelector('.pane')).toBe(pane);

    click(zenButton(container)!);
    expect(isZen(container)).toBe(false);
    expect(bodyZen()).toBe(false);
    expect(rail(container)).toBeNull();
    expect(container.querySelector('.workbench-main')).toBe(main);
    unmount();
  });

  it('不调用浏览器原生 Fullscreen API', async () => {
    const request = vi.fn();
    Object.defineProperty(document.documentElement, 'requestFullscreen', {
      value: request,
      configurable: true,
    });
    const { container, unmount } = await renderPage();
    click(zenButton(container)!);
    expect(request).not.toHaveBeenCalled();
    unmount();
    delete (document.documentElement as { requestFullscreen?: unknown }).requestFullscreen;
  });

  it('普通设置无 zen 入口且热键不消费、不切换', async () => {
    const { container, unmount } = await renderPage();
    click(tabButton(container, '设置'));
    expect(zenButton(container)).toBeNull();
    const ev = press(tabButton(container, '设置'));
    expect(ev.defaultPrevented).toBe(false);
    expect(isZen(container)).toBe(false);
    expect(bodyZen()).toBe(false);
    unmount();
  });

  it('设置保持 Zen 与 rail 显隐，设置热键放行；out zen 退出并重置 rail', async () => {
    const { container, unmount } = await renderPage();
    click(zenButton(container)!);
    click(rail(container)!.querySelector<HTMLButtonElement>('button[aria-label="隐藏任务轨"]')!);
    click(tabButton(container, '设置'));

    expect(isZen(container)).toBe(true);
    expect(bodyZen()).toBe(true);
    expect(rail(container)).toBeNull();
    expect(container.querySelector('button[aria-label="展开任务轨"]')).not.toBeNull();
    expect(zenButton(container)!.textContent).toBe('out zen');
    const ev = press(tabButton(container, '设置'));
    expect(ev.defaultPrevented).toBe(false);
    expect(isZen(container)).toBe(true);

    click(tabButton(container, '终端'));
    expect(isZen(container)).toBe(true);
    expect(rail(container)).toBeNull();
    expect(container.querySelector('button[aria-label="展开任务轨"]')).not.toBeNull();
    click(tabButton(container, '设置'));
    click(zenButton(container)!);
    expect(isZen(container)).toBe(false);
    expect(bodyZen()).toBe(false);
    expect(rail(container)).toBeNull();
    expect(container.querySelector('button[aria-label="展开任务轨"]')).toBeNull();
    expect(zenButton(container)).toBeNull();

    click(tabButton(container, '终端'));
    expect(zenButton(container)!.textContent).toBe('zen');
    click(zenButton(container)!);
    expect(rail(container)).not.toBeNull();
    unmount();
  });

  it('普通流错误不退出 Zen，错误改由整宽提示条承载', async () => {
    const { container, unmount } = await renderPage();
    click(zenButton(container)!);
    act(() => env.taskSub!.onError('网络抖动'));
    await flushUI();
    expect(isZen(container)).toBe(true);
    expect(bodyZen()).toBe(true);
    expect(container.querySelector('.alert-bar.alert-error')?.textContent).toContain('网络抖动');
    unmount();
  });
});

describe('工作台热键（window capture）', () => {
  it('默认热键在终端 textarea 上精确切换，并阻止后续处理', async () => {
    const { container, unmount } = await renderPage();
    const first = press(tuiInput(container));
    expect(first.defaultPrevented).toBe(true);
    expect(isZen(container)).toBe(true);
    press(tuiInput(container));
    expect(isZen(container)).toBe(false);
    unmount();
  });

  it('下游监听不会收到已消费的热键', async () => {
    const downstream = vi.fn();
    document.addEventListener('keydown', downstream);
    const { container, unmount } = await renderPage();
    press(tuiInput(container));
    expect(downstream).not.toHaveBeenCalled();
    document.removeEventListener('keydown', downstream);
    unmount();
  });

  it('repeat 只消费不切换，非 repeat 才切换', async () => {
    const { container, unmount } = await renderPage();
    const ev = press(tuiInput(container), { repeat: true });
    expect(ev.defaultPrevented).toBe(true);
    expect(isZen(container)).toBe(false);
    press(tuiInput(container));
    expect(isZen(container)).toBe(true);
    press(tuiInput(container), { repeat: true });
    expect(isZen(container)).toBe(true);
    unmount();
  });

  it('IME 组合与 keyCode 229 放行，不切换也不消费', async () => {
    const { container, unmount } = await renderPage();
    const composing = press(tuiInput(container), { isComposing: true });
    const process = press(tuiInput(container), { key: 'Process', keyCode: 229 });
    expect(composing.defaultPrevented).toBe(false);
    expect(process.defaultPrevented).toBe(false);
    expect(isZen(container)).toBe(false);
    unmount();
  });

  it('裸 Esc 不进入热键分支，且不退出已进入的 Zen', async () => {
    const { container, unmount } = await renderPage();
    const esc = press(tuiInput(container), { key: 'Escape', code: 'Escape', metaKey: false, shiftKey: false });
    expect(esc.defaultPrevented).toBe(false);
    click(zenButton(container)!);
    press(tuiInput(container), { key: 'Escape', code: 'Escape', metaKey: false, shiftKey: false });
    expect(isZen(container)).toBe(true);
    unmount();
  });

  it('Git 控件内同样生效，切换 Git 标签后全屏保持', async () => {
    const { container, unmount } = await renderPage();
    click(tabButton(container, 'Git'));
    const gitControl = container.querySelector<HTMLElement>('[data-testid="git-control"]')!;
    press(gitControl);
    expect(isZen(container)).toBe(true);
    click(tabButton(container, '终端'));
    expect(isZen(container)).toBe(true);
    expect(bodyZen()).toBe(true);
    unmount();
  });

  it('自定义热键运行时生效，旧热键不再切换', async () => {
    const { container, root, unmount } = await renderPage();
    rerender(root, <TaskWorkbenchPage taskID="t1" workbenchFullscreenHotkey="ctrl+alt+g" />);
    const old = press(tuiInput(container));
    expect(old.defaultPrevented).toBe(false);
    expect(isZen(container)).toBe(false);
    press(tuiInput(container), { key: 'g', code: 'KeyG', metaKey: false, shiftKey: false, ctrlKey: true, altKey: true });
    expect(isZen(container)).toBe(true);
    unmount();
  });

  it('loading 与 404 不消费热键；404 后 Zen 统一退出', async () => {
    const page = mount(<TaskWorkbenchPage taskID="t1" workbenchFullscreenHotkey={HOTKEY} />);
    const loading = press(document.body);
    expect(loading.defaultPrevented).toBe(false);
    expect(isZen(page.container)).toBe(false);

    act(() => env.taskSub!.onData(makeTask({})));
    await flushUI();
    click(zenButton(page.container)!);
    expect(bodyZen()).toBe(true);
    act(() => env.taskSub!.onGone());
    await flushUI();
    expect(page.container.querySelector('.wb-not-found')).not.toBeNull();
    expect(bodyZen()).toBe(false);
    expect(page.container.querySelector('.workbench-zen')).toBeNull();
    const after = press(document.body);
    expect(after.defaultPrevented).toBe(false);
    page.unmount();
  });

  it('卸载后移除监听，且清理 body 标记', async () => {
    const { container, unmount } = await renderPage();
    click(zenButton(container)!);
    expect(bodyZen()).toBe(true);
    unmount();
    expect(bodyZen()).toBe(false);
    const ev = new KeyboardEvent('keydown', { key: 'f', code: 'KeyF', metaKey: true, shiftKey: true, cancelable: true });
    window.dispatchEvent(ev);
    expect(ev.defaultPrevented).toBe(false);
  });

  it('StrictMode 下进入、卸载清理与监听计数正常', async () => {
    const page = mount(
      <StrictMode>
        <TaskWorkbenchPage taskID="t1" workbenchFullscreenHotkey={HOTKEY} />
      </StrictMode>,
    );
    act(() => env.taskSub!.onData(makeTask({})));
    await flushUI();
    const ev = press(tuiInput(page.container));
    expect(ev.defaultPrevented).toBe(true);
    expect(isZen(page.container)).toBe(true);
    expect(bodyZen()).toBe(true);
    page.unmount();
    expect(bodyZen()).toBe(false);
  });
});

describe('桌面任务轨与移动端边界', () => {
  it('≥768px 默认展示任务轨；隐藏后仅保留直接浮动展开按钮并转移焦点', async () => {
    const { container, unmount } = await renderPage();
    click(zenButton(container)!);
    expect(rail(container)!.querySelector('a[href="#/task/t2"]')).not.toBeNull();
    expect(rail(container)!.querySelector('[aria-current="page"]')).not.toBeNull();
    expect(rail(container)!.querySelector('.compact-task-item--current[aria-current="page"]')).not.toBeNull();

    click(rail(container)!.querySelector<HTMLButtonElement>('button[aria-label="隐藏任务轨"]')!);
    expect(rail(container)).toBeNull();
    const expand = container.querySelector<HTMLButtonElement>('button[aria-label="展开任务轨"]')!;
    expect(expand.classList.contains('zen-rail-expand-floating')).toBe(true);
    expect(expand.parentElement?.classList.contains('workbench')).toBe(true);
    expect(document.activeElement).toBe(expand);

    click(expand);
    expect(rail(container)).not.toBeNull();
    expect(container.querySelector('button[aria-label="展开任务轨"]')).toBeNull();
    expect(document.activeElement).toBe(
      rail(container)!.querySelector('button[aria-label="隐藏任务轨"]'),
    );
    unmount();
  });

  it('展开任务轨为 44px，隐藏态浮动按钮仍为 28px 左下定位且高于 pane、低于 modal', () => {
    const css = readFileSync('src/legacy-components.css', 'utf8');
    const railRule = css.match(/\.zen-task-rail\s*\{([^}]*)\}/)?.[1];
    expect(railRule).toContain('flex: 0 0 44px;');
    expect(railRule).toContain('width: 44px;');
    expect(css.match(/\.zen-task-rail-list\s*\{([^}]*)\}/)?.[1]).toContain('scroll-padding: 4px 1px;');
    expect(css.match(/\.zen-rail-toggle\s*\{([^}]*)\}/)?.[1]).toContain('width: 28px;');
    expect(css.match(/\.zen-rail-toggle\s*\{([^}]*)\}/)?.[1]).toContain('height: 28px;');
    expect(css).not.toContain('.zen-task-rail-collapsed');
    expect(css.match(/\.workbench\s*\{([^}]*)\}/)?.[1]).toContain('position: relative;');
    const rule = css.match(/\.zen-rail-expand-floating\s*\{([^}]*)\}/)?.[1];
    expect(rule).toContain('position: absolute;');
    expect(rule).toContain('left: 8px;');
    expect(rule).toContain('bottom: 8px;');
    expect(rule).toContain('z-index: 45;');
  });

  it('Zen current 与 collapsed 共用中性背景，不保留 accent 边框或竖条', () => {
    const css = readFileSync('src/design-system.css', 'utf8');
    const currentRule = css.match(/\.compact-task-item--current,\s*\.compact-task-item\[aria-current="page"\]\s*\{([^}]*)\}/)?.[1];
    expect(currentRule).toContain('background: var(--surface);');
    expect(currentRule).not.toContain('accent');
    expect(currentRule).not.toContain('border-color');
    expect(css).not.toContain('.compact-task-item--current::after');
  });

  it('任务轨显隐跨 TUI/Git 切换保持；退出后重置为默认展示', async () => {
    const { container, unmount } = await renderPage();
    click(zenButton(container)!);
    click(rail(container)!.querySelector<HTMLButtonElement>('button[aria-label="隐藏任务轨"]')!);
    click(tabButton(container, 'Git'));
    expect(rail(container)).toBeNull();
    expect(container.querySelector('button[aria-label="展开任务轨"]')).not.toBeNull();
    click(tabButton(container, '终端'));
    expect(rail(container)).toBeNull();
    expect(container.querySelector('button[aria-label="展开任务轨"]')).not.toBeNull();

    click(zenButton(container)!);
    expect(rail(container)).toBeNull();
    expect(container.querySelector('button[aria-label="展开任务轨"]')).toBeNull();
    click(zenButton(container)!);
    expect(rail(container)).not.toBeNull();
    unmount();
  });

  it('≤767px 不渲染任务轨，退出入口仍可见', async () => {
    env.mobile = true;
    const { container, unmount } = await renderPage();
    click(zenButton(container)!);
    expect(isZen(container)).toBe(true);
    expect(rail(container)).toBeNull();
    expect(container.querySelector('button[aria-label="展开任务轨"]')).toBeNull();
    expect(zenButton(container)!.textContent).toBe('out zen');
    unmount();
  });

  it('768px 起恢复桌面任务轨', async () => {
    env.mobile = false;
    const { container, unmount } = await renderPage();
    click(zenButton(container)!);
    expect(rail(container)).not.toBeNull();
    unmount();
  });

  it('离页（卸载后重新挂载）不继承 Zen 与任务轨状态', async () => {
    const first = await renderPage();
    click(zenButton(first.container)!);
    click(rail(first.container)!.querySelector<HTMLButtonElement>('button[aria-label="隐藏任务轨"]')!);
    first.unmount();
    const second = await renderPage();
    expect(isZen(second.container)).toBe(false);
    expect(bodyZen()).toBe(false);
    second.unmount();
  });
});

describe('终端与 Git 工作区连续性', () => {
  it('进入、rail 显隐与退出不卸载 TUI/shell/Git，TUI 的 wsPath 与 active 不变', async () => {
    env.shells = ['sh1'];
    const { container, unmount } = await renderPage();
    click(tabButton(container, 'Git'));
    click(tabButton(container, '终端'));
    const panes = [...container.querySelectorAll('.pane')];
    const mountedBefore = env.lifecycle.length;

    click(zenButton(container)!);
    click(rail(container)!.querySelector<HTMLButtonElement>('button[aria-label="隐藏任务轨"]')!);
    click(container.querySelector<HTMLButtonElement>('button[aria-label="展开任务轨"]')!);
    click(zenButton(container)!);

    expect(env.lifecycle.slice(mountedBefore).filter((e) => e.startsWith('unmount'))).toEqual([]);
    expect([...container.querySelectorAll('.pane')]).toEqual(panes);
    const tuiProps = env.terminalProps.filter((p) => p.wsPath === '/ws/terminal/t1');
    expect(tuiProps.every((p) => p.wsPath === '/ws/terminal/t1')).toBe(true);
    expect(tuiProps[tuiProps.length - 1].active).toBe(true);
    unmount();
  });

  it('Zen 中 SSE 转 404：统一退出后 body 标记与任务轨无残留', async () => {
    const { container, unmount } = await renderPage();
    click(zenButton(container)!);
    act(() => env.taskSub!.onGone());
    await flushUI();
    expect(bodyZen()).toBe(false);
    expect(rail(container)).toBeNull();
    expect(container.querySelector('.wb-not-found')).not.toBeNull();
    unmount();
  });
});
