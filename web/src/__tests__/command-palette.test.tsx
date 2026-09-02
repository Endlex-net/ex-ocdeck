// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act } from 'react';
import { CommandPalette } from '../components/CommandPalette';
import { mount, rerender } from './cm-test-env';
import type { Project } from '../types';
import type { PaletteFocusPayload } from '../palette-focus';

let storeProjects: Project[] = [];

vi.mock('../hooks', () => ({
  useProjects: () => ({ projects: storeProjects, loading: false, initialized: true, error: '' }),
}));

vi.mock('../router', () => ({
  navigate: vi.fn(),
}));

function project(over: Partial<Project>): Project {
  return {
    id: over.id ?? 'p1',
    name: over.name ?? 'Alpha',
    path: over.path ?? '/tmp/alpha',
    kind: over.kind ?? 'repo',
    default_branch: over.default_branch ?? 'main',
    created_at: over.created_at ?? 1,
    task_count: over.task_count ?? 0,
    tasks_by_status: over.tasks_by_status ?? {},
    tasks: over.tasks ?? [],
  };
}

function labels(container: HTMLElement): string[] {
  return [...container.querySelectorAll('.od-palette-label')].map((el) => el.textContent ?? '');
}

function hints(container: HTMLElement): string[] {
  return [...container.querySelectorAll('.od-palette-hint')].map((el) => el.textContent ?? '');
}

function typeQuery(container: HTMLElement, value: string) {
  const input = container.querySelector('.od-palette-input') as HTMLInputElement;
  const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')?.set;
  act(() => {
    setter?.call(input, value);
    input.dispatchEvent(new Event('input', { bubbles: true }));
  });
}

describe('CommandPalette 快速新建', () => {
  beforeEach(() => {
    storeProjects = [
      project({ id: 'p-xx', name: 'xxAlpha', path: '/tmp/xx' }),
      project({ id: 'p-beta', name: 'Beta', path: '/tmp/beta' }),
      project({ id: 'p-alpha', name: 'Alpha', path: '/tmp/alpha' }),
      project({ id: 'p-space', name: 'foo bar', path: '/tmp/space' }),
    ];
  });

  it('静态入口共 7 项，含设置 · 命令面板', () => {
    const { container, unmount } = mount(<CommandPalette open onClose={() => {}} />);
    const pageItems = [...container.querySelectorAll('.od-palette-item')].filter((el) =>
      ['指挥中心', '项目管理', '设置 · 终端外观', '设置 · 环境变量', '设置 · opencode 配置', '设置 · AI 配置', '设置 · 命令面板'].includes(
        el.querySelector('.od-palette-label')?.textContent ?? '',
      ),
    );
    expect(pageItems).toHaveLength(7);
    expect(labels(container)).toContain('设置 · 命令面板');
    unmount();
  });

  it('仅触发词不进入快速新建模式', () => {
    const { container, unmount } = mount(<CommandPalette open onClose={() => {}} />);
    typeQuery(container, 'new');
    const labs = labels(container);
    expect(labs).not.toContain('Alpha');
    expect(labs).not.toContain('xxAlpha');
    expect(container.querySelectorAll('.od-palette-group')).not.toHaveLength(0);
    expect([...container.querySelectorAll('.od-palette-group')].map((el) => el.textContent)).not.toContain('项目');
    unmount();
  });

  it('触发词模式置顶新建任务，候选按 rank 排序且副文案为路径', () => {
    const { container, unmount } = mount(<CommandPalette open onClose={() => {}} />);
    typeQuery(container, 'new alpha');
    const labs = labels(container);
    expect(labs[0]).toBe('新建任务');
    expect(labs.slice(1)).toEqual(['Alpha', 'xxAlpha']);
    expect(hints(container)).toContain('/tmp/alpha');
    expect(hints(container)).toContain('/tmp/xx');
    unmount();
  });

  it('空余文展示全部项目候选且不自动预选（cursor 停在置顶命令）', () => {
    const { container, unmount } = mount(<CommandPalette open onClose={() => {}} />);
    typeQuery(container, 'new ');
    const labs = labels(container);
    expect(labs[0]).toBe('新建任务');
    expect(labs.slice(1)).toEqual(['Alpha', 'Beta', 'foo bar', 'xxAlpha']);
    const current = container.querySelector('.od-palette-item.current .od-palette-label')?.textContent;
    expect(current).toBe('新建任务');
    unmount();
  });

  it('零命中 fallback 展示全部项目按名称确定序', () => {
    const { container, unmount } = mount(<CommandPalette open onClose={() => {}} />);
    typeQuery(container, 'new zzzz');
    expect(labels(container)).toEqual(['新建任务', 'Alpha', 'Beta', 'foo bar', 'xxAlpha']);
    unmount();
  });

  it('Enter 置顶命令只传 { projectName }；空余文传空串', () => {
    const onNewTask = vi.fn();
    const { container, unmount } = mount(<CommandPalette open onClose={() => {}} onNewTask={onNewTask} />);
    typeQuery(container, 'new ');
    const input = container.querySelector('.od-palette-input') as HTMLInputElement;
    act(() => {
      input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    });
    expect(onNewTask).toHaveBeenCalledWith({ projectName: '' });
    unmount();
  });

  it('自由文本 Enter 只传 projectName', () => {
    const onNewTask = vi.fn();
    const { container, unmount } = mount(<CommandPalette open onClose={() => {}} onNewTask={onNewTask} />);
    typeQuery(container, 'new foo bar');
    const input = container.querySelector('.od-palette-input') as HTMLInputElement;
    act(() => {
      input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    });
    expect(onNewTask).toHaveBeenCalledWith({ projectName: 'foo bar' });
    unmount();
  });

  it('候选点击携带 projectID + projectName；同名项目靠 id 区分', () => {
    storeProjects = [
      project({ id: 'id-a', name: 'Twin', path: '/a' }),
      project({ id: 'id-b', name: 'Twin', path: '/b' }),
    ];
    const onNewTask = vi.fn<(payload?: PaletteFocusPayload) => void>();
    const { container, unmount } = mount(<CommandPalette open onClose={() => {}} onNewTask={onNewTask} />);
    typeQuery(container, 'new Twin');
    const items = [...container.querySelectorAll('.od-palette-item')];
    const second = items.find((el) => el.querySelector('.od-palette-hint')?.textContent === '/b');
    expect(second).toBeTruthy();
    act(() => {
      (second as HTMLElement).click();
    });
    expect(onNewTask).toHaveBeenCalledWith({ projectName: 'Twin', projectID: 'id-b' });
    unmount();
  });

  it('首次无快照仅显示置顶命令', () => {
    storeProjects = [];
    const { container, unmount } = mount(<CommandPalette open onClose={() => {}} />);
    typeQuery(container, 'new ');
    expect(labels(container)).toEqual(['新建任务']);
    unmount();
  });

  it('候选收缩后 cursor 越界被 clamp，Enter 执行仍可见的末尾项', () => {
    const onNewTask = vi.fn<(payload?: PaletteFocusPayload) => void>();
    const { container, root, unmount } = mount(
      <CommandPalette open onClose={() => {}} onNewTask={onNewTask} />,
    );
    typeQuery(container, 'new ');
    const input = container.querySelector('.od-palette-input') as HTMLInputElement;
    // [新建任务, Alpha, Beta, foo bar, xxAlpha]：ArrowDown 到末尾 xxAlpha
    for (let i = 0; i < 4; i++) {
      act(() => {
        input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
      });
    }
    expect(container.querySelector('.od-palette-item.current .od-palette-label')?.textContent).toBe('xxAlpha');

    // 共享项目快照收缩为 2 项：cursor(4) 越界，须 clamp 到末尾 Beta
    storeProjects = [
      project({ id: 'p-beta', name: 'Beta', path: '/tmp/beta' }),
      project({ id: 'p-alpha', name: 'Alpha', path: '/tmp/alpha' }),
    ];
    rerender(root, <CommandPalette open onClose={() => {}} onNewTask={onNewTask} />);
    expect(labels(container)).toEqual(['新建任务', 'Alpha', 'Beta']);
    expect(container.querySelector('.od-palette-item.current .od-palette-label')?.textContent).toBe('Beta');
    act(() => {
      input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    });
    expect(onNewTask).toHaveBeenCalledWith({ projectName: 'Beta', projectID: 'p-beta' });
    unmount();
  });

  it('候选清空后 Enter 仍执行置顶快速新建', () => {
    const onNewTask = vi.fn<(payload?: PaletteFocusPayload) => void>();
    const { container, root, unmount } = mount(
      <CommandPalette open onClose={() => {}} onNewTask={onNewTask} />,
    );
    typeQuery(container, 'new ');
    const input = container.querySelector('.od-palette-input') as HTMLInputElement;
    act(() => {
      input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
    });
    expect(container.querySelector('.od-palette-item.current .od-palette-label')?.textContent).toBe('Alpha');

    // 共享项目快照清空：仅剩置顶命令，cursor 须回到 0
    storeProjects = [];
    rerender(root, <CommandPalette open onClose={() => {}} onNewTask={onNewTask} />);
    expect(labels(container)).toEqual(['新建任务']);
    expect(container.querySelector('.od-palette-item.current .od-palette-label')?.textContent).toBe('新建任务');
    act(() => {
      input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    });
    expect(onNewTask).toHaveBeenCalledWith({ projectName: '' });
    unmount();
  });
});
