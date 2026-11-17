// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { readFileSync } from 'node:fs';
import { act } from 'react';
import { AppShell } from '../components/AppShell';
import { mount } from './cm-test-env';
import { stubStorage } from './dom-test-utils';
import type { Project, TaskSummary } from '../types';

/* ============ 桌面折叠侧栏接入 CompactTaskNav（workbench-web-fullscreen 3.3）============
 * 折叠（桌面 ≥768px）走 CompactTaskNav；展开态保持 SidebarTaskGroups；⌘B 切换形态；
 * 两处同一任务的状态点语义一致。 */

const mocks = vi.hoisted(() => ({ isNarrow: false, projects: [] as Project[] }));

vi.mock('../hooks', () => ({
  useMediaQuery: () => mocks.isNarrow,
  useProjects: () => ({ projects: mocks.projects, initialized: true, error: '' }),
}));
vi.mock('../components/ServerStatusBanner', () => ({ ServerStatusBanner: () => null }));

const { store } = stubStorage();

const COLLAPSE_KEY = 'ocdeck:side-collapsed';

function task(over: Partial<TaskSummary>): TaskSummary {
  return {
    id: 't1',
    name: 'demo-task',
    status: 'active',
    init_status: 'none',
    branch: 'main',
    worktree_path: '/tmp/wt',
    mode: 'worktree',
    permission_mode: 'ask',
    updated_at: 2,
    agentStatus: 'busy',
    attention_count: 0,
    ...over,
  };
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
      task_count: 2,
      tasks_by_status: { active: 2 },
      tasks: [task({ id: 't1', name: 'demo-task' }), task({ id: 't2', name: 'idle-task', agentStatus: 'idle' })],
    },
  ];
}

function mountShell() {
  return mount(
    <AppShell onOpenPalette={() => {}} onToggleTheme={() => {}} themePref="system" paletteHotkeyLabel="⌘K">
      <div>page</div>
    </AppShell>,
  );
}

const cmdB = () =>
  act(() => {
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'b', metaKey: true, cancelable: true }));
  });

beforeEach(() => {
  store.clear();
  window.location.hash = '#/';
  mocks.isNarrow = false;
  mocks.projects = makeProjects();
});

afterEach(() => {
  document.body.classList.remove('od-side-collapsed');
});

describe('桌面折叠侧栏接入 CompactTaskNav', () => {
  it('折叠态渲染 compact 任务点：无原生 title，保留 current 语义与可访问名称', () => {
    window.location.hash = '#/task/t1';
    store.set(COLLAPSE_KEY, '1');
    const { container, unmount } = mountShell();
    expect(container.querySelector('.compact-task-nav--sidebar')).not.toBeNull();
    expect(container.querySelector('.compact-task-project')).toBeNull();
    expect(container.querySelector('.compact-task-nav--sidebar')?.textContent).toBe('');
    expect(container.querySelector('.od-nav-task')).toBeNull();
    const item = container.querySelector<HTMLAnchorElement>('a.compact-task-item[href="#/task/t1"]');
    expect(item).not.toBeNull();
    expect(item!.hasAttribute('title')).toBe(false);
    expect(item!.getAttribute('aria-label')).toBe('demo-task，proj');
    expect(item!.getAttribute('aria-current')).toBe('page');
    expect(item!.classList.contains('compact-task-item--current')).toBe(true);
    unmount();
  });

  it('桌面 collapsed 使用 44px 侧栏、零横向内边距与 32px 图标项', () => {
    const css = readFileSync('src/design-system.css', 'utf8');
    expect(css).toContain('body.od-side-collapsed .od-shell > .od-sidebar { width: 44px; flex-basis: 44px; }');
    expect(css).toContain('body.od-side-collapsed .od-nav { padding: 10px 0; align-items: center; }');
    expect(css.match(/body\.od-side-collapsed \.od-nav-item\s*\{([^}]*)\}/)?.[1]).toContain('width: 32px;');
    expect(css.match(/body\.od-side-collapsed \.od-sidebar-cmdk\s*\{([^}]*)\}/)?.[1]).toContain('width: 32px;');
  });

  it('展开态不渲染 compact 导航，任务组保持原渲染与项目标题', () => {
    const { container, unmount } = mountShell();
    expect(container.querySelector('.compact-task-nav')).toBeNull();
    expect(container.querySelector('.od-nav-task')).not.toBeNull();
    expect(container.querySelector('.od-nav-proj')?.textContent).toBe('proj');
    unmount();
  });

  it('两处状态点同源：同一 busy 任务在折叠与展开态均为 od-agent-busy', () => {
    const expanded = mountShell();
    expect(expanded.container.querySelector('.od-nav-task .od-agent-busy')).not.toBeNull();
    expanded.unmount();

    store.set(COLLAPSE_KEY, '1');
    const collapsed = mountShell();
    expect(collapsed.container.querySelector('a.compact-task-item[href="#/task/t1"] .od-agent-busy')).not.toBeNull();
    collapsed.unmount();
  });

  it('⌘B 在展开与折叠形态间切换，偏好仍按既有键持久化', () => {
    const { container, unmount } = mountShell();
    expect(container.querySelector('.compact-task-nav')).toBeNull();
    cmdB();
    expect(store.get(COLLAPSE_KEY)).toBe('1');
    expect(container.querySelector('.compact-task-nav--sidebar')).not.toBeNull();
    cmdB();
    expect(store.get(COLLAPSE_KEY)).toBe('0');
    expect(container.querySelector('.compact-task-nav')).toBeNull();
    unmount();
  });

  it('点击 compact 任务仍为原生 href 导航（不拦截默认行为）', () => {
    store.set(COLLAPSE_KEY, '1');
    const { container, unmount } = mountShell();
    const item = container.querySelector<HTMLAnchorElement>('a.compact-task-item[href="#/task/t2"]')!;
    const ev = new MouseEvent('click', { bubbles: true, cancelable: true });
    act(() => {
      item.dispatchEvent(ev);
    });
    expect(ev.defaultPrevented).toBe(false);
    unmount();
  });

  it('移动端（≤767px）即使持久化为折叠也不走 compact，保持既有顶栏任务组', () => {
    store.set(COLLAPSE_KEY, '1');
    mocks.isNarrow = true;
    const { container, unmount } = mountShell();
    expect(container.querySelector('.compact-task-nav')).toBeNull();
    expect(container.querySelector('.od-nav-task')).not.toBeNull();
    unmount();
  });
});
