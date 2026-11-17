// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { readFileSync } from 'node:fs';
import { act } from 'react';
import type { Root } from 'react-dom/client';
import { CompactTaskNav } from '../components/CompactTaskNav';
import { taskStatusDotClass, taskStatusLabel } from '../components/task-status';
import { requestTerminalFocus } from '../terminal/focus-request';
import type { Project, TaskSummary } from '../types';
import { mount, rerender } from './cm-test-env';

vi.mock('../terminal/focus-request', () => ({ requestTerminalFocus: vi.fn() }));

type ObserverCallback = () => void;

class MutationObserverStub {
  static instances: MutationObserverStub[] = [];
  disconnected = false;
  private callback: ObserverCallback;

  constructor(callback: MutationCallback) {
    this.callback = callback as unknown as ObserverCallback;
    MutationObserverStub.instances.push(this);
  }

  observe() {}
  disconnect() {
    this.disconnected = true;
  }
  fire() {
    this.callback();
  }
}

class ResizeObserverStub {
  static instances: ResizeObserverStub[] = [];
  disconnected = false;
  private callback: ObserverCallback;

  constructor(callback: ResizeObserverCallback) {
    this.callback = callback as unknown as ObserverCallback;
    ResizeObserverStub.instances.push(this);
  }

  observe() {}
  unobserve() {}
  disconnect() {
    this.disconnected = true;
  }
  fire() {
    this.callback();
  }
}

class MediaQueryStub {
  private listeners = new Set<() => void>();
  matches = false;
  media = '(max-width: 767px)';

  addEventListener(type: string, listener: EventListener) {
    if (type === 'change') this.listeners.add(listener as unknown as () => void);
  }
  removeEventListener(type: string, listener: EventListener) {
    if (type === 'change') this.listeners.delete(listener as unknown as () => void);
  }
  fire() {
    for (const listener of this.listeners) listener();
  }
}

let mediaQuery: MediaQueryStub;
const cleanups: Array<() => void> = [];

function makeTask(id: string, name: string, over: Partial<TaskSummary> = {}): TaskSummary {
  return {
    id,
    name,
    status: 'active',
    init_status: 'none',
    branch: 'main',
    worktree_path: `/tmp/${id}`,
    mode: 'worktree',
    permission_mode: 'ask',
    updated_at: 1,
    agentStatus: 'idle',
    attention_count: 0,
    ...over,
  };
}

function makeProject(id: string, name: string, tasks: TaskSummary[]): Project {
  return {
    id,
    name,
    path: `/tmp/${id}`,
    kind: 'repo',
    default_branch: 'main',
    created_at: 1,
    task_count: tasks.length,
    tasks_by_status: {},
    tasks,
  };
}

function navUI(
  projects: Project[],
  options: { currentTaskID?: string; variant?: 'sidebar' | 'zen'; visible?: boolean } = {},
) {
  return (
    <CompactTaskNav
      projects={projects}
      currentTaskID={options.currentTaskID}
      variant={options.variant ?? 'zen'}
      visible={options.visible}
    />
  );
}

function renderNav(
  projects: Project[],
  options: { currentTaskID?: string; variant?: 'sidebar' | 'zen'; visible?: boolean } = {},
) {
  const mounted = mount(navUI(projects, options));
  let mountedNow = true;
  const unmount = () => {
    if (!mountedNow) return;
    mountedNow = false;
    mounted.unmount();
  };
  cleanups.push(unmount);
  return { ...mounted, unmount };
}

function update(root: Root, projects: Project[], options: Parameters<typeof navUI>[1] = {}) {
  rerender(root, navUI(projects, options));
}

function links(container: HTMLElement): HTMLAnchorElement[] {
  return [...container.querySelectorAll<HTMLAnchorElement>('.compact-task-item')];
}

function tooltip(): HTMLElement | null {
  return document.body.querySelector<HTMLElement>('[role="tooltip"]');
}

function mouseEnter(element: Element) {
  act(() => {
    element.dispatchEvent(new MouseEvent('mouseover', { bubbles: true }));
  });
}

function mouseLeave(element: Element) {
  act(() => {
    element.dispatchEvent(new MouseEvent('mouseout', { bubbles: true }));
  });
}

function detailValue(card: HTMLElement, label: string): string | undefined {
  const row = [...card.querySelectorAll('.task-hover-card-row')].find(
    (candidate) => candidate.querySelector('dt')?.textContent === label,
  );
  return row?.querySelector('dd')?.textContent ?? undefined;
}

beforeEach(() => {
  vi.useFakeTimers();
  MutationObserverStub.instances = [];
  ResizeObserverStub.instances = [];
  mediaQuery = new MediaQueryStub();
  Object.defineProperty(window, 'matchMedia', {
    configurable: true,
    writable: true,
    value: vi.fn(() => mediaQuery),
  });
  Object.defineProperty(document, 'hidden', { configurable: true, value: false });
  vi.stubGlobal('MutationObserver', MutationObserverStub);
  vi.stubGlobal('ResizeObserver', ResizeObserverStub);
});

afterEach(() => {
  for (const cleanup of cleanups.splice(0).reverse()) cleanup();
  vi.clearAllMocks();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  document.body.innerHTML = '';
});

describe('compact 任务排序、状态与导航', () => {
  it('仅使用侧栏收录与排序，保留当前项、可访问名称、原生导航和焦点请求', () => {
    const projects = [
      makeProject('p1', '项目甲', [
        makeTask('idle', '空闲任务', { updated_at: 10 }),
        makeTask('archive', '归档任务', { status: 'archived', updated_at: 1000 }),
        makeTask('busy', '工作任务', { agentStatus: 'busy', updated_at: 2 }),
        makeTask('attention', '人工任务', {
          agentStatus: 'busy',
          attention_count: 2,
          updated_at: 1,
        }),
        makeTask('creating', '创建任务', { status: 'creating', agentStatus: undefined }),
        makeTask('suspended', '挂起任务', {
          status: 'suspended',
          agentStatus: undefined,
          updated_at: 20,
        }),
      ]),
    ];
    const { container, unmount } = renderNav(projects, { currentTaskID: 'idle' });
    const items = links(container);

    expect(items.map((item) => item.getAttribute('aria-label'))).toEqual([
      '人工任务，项目甲',
      '工作任务，项目甲',
      '空闲任务，项目甲',
      '创建任务，项目甲',
      '挂起任务，项目甲',
    ]);
    expect(items.map((item) => item.getAttribute('href'))).toContain('#/task/attention');
    expect(items[2].getAttribute('aria-current')).toBe('page');
    expect(items[2].classList.contains('compact-task-item--current')).toBe(true);
    expect(container.querySelector('.od-agent-attention')).not.toBeNull();
    expect(container.querySelector('.od-agent-busy')).not.toBeNull();
    expect(container.querySelector('.compact-task-spinner')).not.toBeNull();
    expect(container.querySelector('.compact-task-project')).toBeNull();
    expect(items.every((item) => !item.hasAttribute('title'))).toBe(true);
    expect(container.querySelector('.compact-task-nav [title]')).toBeNull();

    act(() => items[0].click());
    expect(requestTerminalFocus).toHaveBeenCalledWith('attention');
    unmount();
  });

  it('collapsed 和 Zen 均不显示项目分组标题，保留无视觉输出的组语义', () => {
    const projects = [makeProject('p1', '项目甲', [makeTask('t1', '任务甲')])];
    const { container, root } = renderNav(projects, { variant: 'sidebar' });
    expect(container.querySelector('.compact-task-project')).toBeNull();
    expect(container.querySelector('.compact-task-nav')?.textContent).toBe('');
    expect(container.querySelector('[role="group"]')?.getAttribute('aria-label')).toBe('项目甲');
    expect(links(container)[0].getAttribute('aria-label')).toBe('任务甲，项目甲');
    update(root, projects, { variant: 'zen' });
    expect(container.querySelector('.compact-task-project')).toBeNull();
    expect(container.querySelector('.compact-task-nav')?.textContent).toBe('');
  });

  it('两种 variant 的 current 仅使用中性背景，无 accent 装饰且保留键盘焦点环', () => {
    const projects = [makeProject('p1', '项目甲', [makeTask('t1', '任务甲')])];
    const { container, root } = renderNav(projects, { currentTaskID: 't1', variant: 'sidebar' });
    for (const variant of ['sidebar', 'zen'] as const) {
      if (variant === 'zen') update(root, projects, { currentTaskID: 't1', variant });
      const current = container.querySelector(`.compact-task-nav--${variant} .compact-task-item--current`);
      expect(current?.getAttribute('aria-current')).toBe('page');
    }

    const css = readFileSync('src/design-system.css', 'utf8');
    const currentRule = css.match(/\.compact-task-item--current,\s*\.compact-task-item\[aria-current="page"\]\s*\{([^}]*)\}/)?.[1];
    expect(currentRule).toContain('background: var(--surface);');
    expect(currentRule).not.toContain('accent');
    expect(currentRule).not.toContain('border-color');
    expect(css).not.toContain('.compact-task-item--current::after');
    const baseRule = css.match(/\.compact-task-item\s*\{([^}]*)\}/)?.[1];
    expect(baseRule).toContain('width: 32px;');
    expect(baseRule).toContain('min-height: 32px;');
    expect(baseRule).toContain('padding: 2px 0;');
    const listRule = css.match(/\.compact-task-list\s*\{([^}]*)\}/)?.[1];
    expect(listRule).toContain('padding: 4px 1px;');
    expect(listRule).toContain('scroll-padding: 4px 1px;');
    const focusRule = css.match(/\.compact-task-item:focus-visible\s*\{([^}]*)\}/)?.[1];
    expect(focusRule).toContain('outline: 2px solid var(--accent);');
    expect(focusRule).toContain('outline-offset: 2px;');
  });

  it('状态点 attention 优先，未知状态降级为 idle-off', () => {
    expect(taskStatusDotClass({ agentStatus: 'busy', attention_count: 1 })).toBe('attention');
    expect(taskStatusDotClass({ agentStatus: 'busy', attention_count: 0 })).toBe('busy');
    expect(taskStatusDotClass({ agentStatus: 'retry', attention_count: 0 })).toBe('retry');
    expect(taskStatusDotClass({ agentStatus: 'idle', attention_count: 0 })).toBe('idle');
    expect(taskStatusDotClass({ agentStatus: 'other', attention_count: 0 })).toBe('idle-off');
    expect(taskStatusDotClass({})).toBe('idle-off');
  });

  it('状态文案覆盖过渡、挂起和 active 优先级', () => {
    expect(taskStatusLabel(makeTask('a', 'a', { status: 'creating' }))).toBe('创建中');
    expect(taskStatusLabel(makeTask('a', 'a', { status: 'activating' }))).toBe('激活中');
    expect(taskStatusLabel(makeTask('a', 'a', { status: 'suspending' }))).toBe('挂起中');
    expect(taskStatusLabel(makeTask('a', 'a', { status: 'suspended' }))).toBe('任务已挂起');
    expect(
      taskStatusLabel(makeTask('a', 'a', { agentStatus: 'busy', attention_count: 1 })),
    ).toBe('等待人工');
    expect(taskStatusLabel(makeTask('a', 'a', { agentStatus: 'busy' }))).toBe('工作中');
    expect(taskStatusLabel(makeTask('a', 'a', { agentStatus: 'retry' }))).toBe('重试中');
    expect(taskStatusLabel(makeTask('a', 'a', { agentStatus: 'idle' }))).toBe('空闲');
    expect(taskStatusLabel(makeTask('a', 'a', { agentStatus: undefined }))).toBe('agent状态未知');
  });
});

describe('TaskHoverCard 即时交互与内容', () => {
  it('hover/focus 零延迟打开，A→B 立即替换且只有一个 portal 卡片', () => {
    const projects = [
      makeProject('p1', '项目甲', [
        makeTask('t1', '任务甲'),
        makeTask('t2', '任务乙', { agentStatus: 'busy', updated_at: 2 }),
      ]),
    ];
    const { container } = renderNav(projects);
    const first = links(container).find((item) => item.getAttribute('href') === '#/task/t1')!;
    const second = links(container).find((item) => item.getAttribute('href') === '#/task/t2')!;

    mouseEnter(first);
    expect(tooltip()?.dataset.taskId).toBe('t1');
    expect(first.getAttribute('aria-describedby')).toBe(tooltip()?.id);
    expect(document.querySelectorAll('[role="tooltip"]')).toHaveLength(1);

    mouseEnter(second);
    expect(tooltip()?.dataset.taskId).toBe('t2');
    expect(tooltip()?.textContent).toContain('任务乙');
    expect(tooltip()?.textContent).not.toContain('任务甲');
    expect(first.hasAttribute('aria-describedby')).toBe(false);
    expect(second.getAttribute('aria-describedby')).toBe(tooltip()?.id);
    expect(document.querySelectorAll('[role="tooltip"]')).toHaveLength(1);

    mouseLeave(second);
    act(() => first.focus());
    expect(document.activeElement).toBe(first);
    expect(tooltip()?.dataset.taskId).toBe('t1');
  });

  it('展示任务名、项目、状态、缺省 0 和当前任务标识，不含按钮或原生 title', () => {
    const projects = [makeProject('p1', '项目甲', [makeTask('t1', '任务甲')])];
    const { container } = renderNav(projects, { currentTaskID: 't1' });
    const trigger = links(container)[0];
    act(() => trigger.focus());
    const card = tooltip();
    expect(card).not.toBeNull();
    expect(card?.querySelector('.task-hover-card-name')?.textContent).toBe('任务甲');
    expect(detailValue(card!, '项目')).toBe('项目甲');
    expect(detailValue(card!, '当前状态')).toBe('空闲');
    expect(detailValue(card!, '待处理')).toBe('0');
    expect(card?.querySelector('.task-hover-card-current')?.textContent).toBe('当前任务');
    expect(card?.querySelector('button')).toBeNull();
    expect(card?.hasAttribute('title')).toBe(false);
    expect(document.activeElement).toBe(trigger);
    expect(trigger.getAttribute('aria-describedby')).toBe(card?.id);
  });

  it('trigger/card 跨越只给关闭 100ms 宽限，进入任一区域取消旧 timer', () => {
    const projects = [makeProject('p1', '项目甲', [makeTask('t1', '任务甲')])];
    const { container } = renderNav(projects);
    const trigger = links(container)[0];
    mouseEnter(trigger);
    const card = tooltip()!;

    mouseLeave(trigger);
    act(() => vi.advanceTimersByTime(99));
    expect(tooltip()).not.toBeNull();
    mouseEnter(card);
    act(() => vi.advanceTimersByTime(1000));
    expect(tooltip()).not.toBeNull();

    mouseLeave(card);
    act(() => vi.advanceTimersByTime(99));
    expect(tooltip()).not.toBeNull();
    act(() => vi.advanceTimersByTime(1));
    expect(tooltip()).toBeNull();
    expect(vi.getTimerCount()).toBe(0);
  });

  it('焦点离开同样只延迟关闭，重新聚焦立即取消关闭', () => {
    const projects = [makeProject('p1', '项目甲', [makeTask('t1', '任务甲')])];
    const { container } = renderNav(projects);
    const trigger = links(container)[0];
    act(() => trigger.focus());
    expect(tooltip()).not.toBeNull();
    act(() => trigger.blur());
    act(() => vi.advanceTimersByTime(50));
    act(() => trigger.focus());
    act(() => vi.advanceTimersByTime(1000));
    expect(tooltip()).not.toBeNull();
  });

  it('悬浮详情层级高于 workbench overflow 且低于 modal 与命令面板', () => {
    const designCSS = readFileSync('src/design-system.css', 'utf8');
    const legacyCSS = readFileSync('src/legacy-components.css', 'utf8');
    const zIndex = (source: string, selector: string) => {
      const block = source.match(new RegExp(`\\.${selector}\\s*\\{([^}]*)\\}`))?.[1];
      return Number(block?.match(/\bz-index:\s*(\d+)/)?.[1]);
    };

    const hoverZ = zIndex(designCSS, 'task-hover-card');
    expect(hoverZ).toBe(45);
    expect(hoverZ).toBeGreaterThan(zIndex(legacyCSS, 'overflow-menu'));
    expect(zIndex(legacyCSS, 'modal-backdrop')).toBe(50);
    expect(hoverZ).toBeLessThan(zIndex(legacyCSS, 'modal-backdrop'));
    expect(zIndex(designCSS, 'od-palette-overlay')).toBe(90);
    expect(hoverZ).toBeLessThan(zIndex(designCSS, 'od-palette-overlay'));
  });
});

describe('TaskHoverCard 关闭与资源清理', () => {
  function openSingle() {
    const projects = [makeProject('p1', '项目甲', [makeTask('t1', '任务甲')])];
    const mounted = renderNav(projects);
    mouseEnter(links(mounted.container)[0]);
    expect(tooltip()).not.toBeNull();
    return { ...mounted, projects };
  }

  it('卡片内部 scroll 保持打开，任务列表 scroll 立即关闭', () => {
    const { container } = openSingle();
    const card = tooltip()!;

    act(() => card.dispatchEvent(new Event('scroll')));
    expect(tooltip()).toBe(card);

    const list = container.querySelector('.compact-task-list')!;
    act(() => list.dispatchEvent(new Event('scroll')));
    expect(tooltip()).toBeNull();
    expect(vi.getTimerCount()).toBe(0);
  });

  it('列表滚动、窗口 resize/blur 与断点变化立即关闭并清 timer', () => {
    const { container } = openSingle();
    const list = container.querySelector('.compact-task-list')!;
    mouseLeave(links(container)[0]);
    act(() => list.dispatchEvent(new Event('scroll')));
    expect(tooltip()).toBeNull();
    expect(vi.getTimerCount()).toBe(0);

    mouseEnter(links(container)[0]);
    act(() => window.dispatchEvent(new Event('resize')));
    expect(tooltip()).toBeNull();

    mouseEnter(links(container)[0]);
    act(() => window.dispatchEvent(new Event('blur')));
    expect(tooltip()).toBeNull();

    mouseEnter(links(container)[0]);
    act(() => mediaQuery.fire());
    expect(tooltip()).toBeNull();
    expect(vi.getTimerCount()).toBe(0);
  });

  it('页面隐藏与 visible=false 立即关闭，不遗留卡片或 timer', () => {
    const { container, root, projects } = openSingle();
    Object.defineProperty(document, 'hidden', { configurable: true, value: true });
    act(() => document.dispatchEvent(new Event('visibilitychange')));
    expect(tooltip()).toBeNull();

    mouseEnter(links(container)[0]);
    mouseLeave(links(container)[0]);
    update(root, projects, { visible: false });
    expect(container.querySelector('.compact-task-nav')).toBeNull();
    expect(tooltip()).toBeNull();
    expect(vi.getTimerCount()).toBe(0);
  });

  it('owner display/visibility 隐藏立即关闭，ResizeObserver 也执行同一检查', () => {
    const { container } = openSingle();
    const nav = container.querySelector<HTMLElement>('.compact-task-nav')!;
    nav.style.display = 'none';
    act(() => MutationObserverStub.instances[0].fire());
    expect(tooltip()).toBeNull();

    nav.style.display = '';
    mouseEnter(links(container)[0]);
    nav.style.visibility = 'hidden';
    act(() => ResizeObserverStub.instances[0].fire());
    expect(tooltip()).toBeNull();
    expect(vi.getTimerCount()).toBe(0);
  });

  it('任务从最新 projects 快照消失时立即关闭', () => {
    const { root } = openSingle();
    update(root, []);
    expect(tooltip()).toBeNull();
    expect(vi.getTimerCount()).toBe(0);
  });

  it('卸载移除 portal、observer、监听器并清理在途关闭 timer', () => {
    const { container, unmount } = openSingle();
    mouseLeave(links(container)[0]);
    expect(vi.getTimerCount()).toBe(1);
    const mutations = MutationObserverStub.instances[0];
    const resizes = ResizeObserverStub.instances[0];
    unmount();
    expect(tooltip()).toBeNull();
    expect(mutations.disconnected).toBe(true);
    expect(resizes.disconnected).toBe(true);
    expect(vi.getTimerCount()).toBe(0);
    act(() => vi.runAllTimers());
    expect(tooltip()).toBeNull();
  });
});

describe('projects 数据边界', () => {
  it('渲染、hover 与快照更新不新增 fetch 或 EventSource 请求', () => {
    const fetchSpy = vi.fn();
    const eventSourceSpy = vi.fn();
    vi.stubGlobal('fetch', fetchSpy);
    vi.stubGlobal('EventSource', eventSourceSpy);
    const projects = [makeProject('p1', '项目甲', [makeTask('t1', '任务甲')])];
    const { container, root } = renderNav(projects);
    mouseEnter(links(container)[0]);
    update(root, [
      makeProject('p1', '项目甲', [makeTask('t1', '任务甲更新', { agentStatus: 'busy' })]),
    ]);
    expect(tooltip()?.textContent).toContain('任务甲更新');
    expect(tooltip()?.textContent).toContain('工作中');
    expect(fetchSpy).not.toHaveBeenCalled();
    expect(eventSourceSpy).not.toHaveBeenCalled();
  });
});
