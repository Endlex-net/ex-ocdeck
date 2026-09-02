// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act } from 'react';
import { CommandPalette, matchCommandTrigger } from '../components/CommandPalette';
import { navigate } from '../router';
import { DEFAULT_COMMAND_TRIGGERS, type PaletteCommandId } from '../palette-focus';
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

// navigate 为模块级 mock，调用记录跨测试累积会让 toHaveBeenCalledWith 误判（被先前测试的历史调用满足）
beforeEach(() => {
  vi.clearAllMocks();
});

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

  it('命中时默认高亮首个候选，Enter 携带 projectName + projectID', () => {
    const onNewTask = vi.fn<(payload?: PaletteFocusPayload) => void>();
    const { container, unmount } = mount(
      <CommandPalette open onClose={() => {}} onNewTask={onNewTask} />,
    );
    typeQuery(container, 'new alpha');
    expect(container.querySelector('.od-palette-item.current .od-palette-label')?.textContent).toBe('Alpha');
    expect(container.querySelector('.od-palette-item.current')?.getAttribute('aria-selected')).toBe('true');
    const input = container.querySelector('.od-palette-input') as HTMLInputElement;
    act(() => {
      input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    });
    expect(onNewTask).toHaveBeenCalledWith({ projectName: 'Alpha', projectID: 'p-alpha' });
    unmount();
  });

  it('零命中默认高亮置顶命令；余文命中↔零命中往复按规则重置', () => {
    const { container, unmount } = mount(<CommandPalette open onClose={() => {}} />);
    const currentLabel = () =>
      container.querySelector('.od-palette-item.current .od-palette-label')?.textContent;
    typeQuery(container, 'new zzzz');
    expect(labels(container)).toEqual(['新建任务', 'Alpha', 'Beta', 'foo bar', 'xxAlpha']);
    expect(currentLabel()).toBe('新建任务');

    typeQuery(container, 'new alpha');
    expect(currentLabel()).toBe('Alpha');
    typeQuery(container, 'new zzzz');
    expect(currentLabel()).toBe('新建任务');
    typeQuery(container, 'new alpha');
    expect(currentLabel()).toBe('Alpha');
    unmount();
  });

  it('ArrowUp 从首个候选回到置顶命令', () => {
    const { container, unmount } = mount(<CommandPalette open onClose={() => {}} />);
    typeQuery(container, 'new alpha');
    expect(container.querySelector('.od-palette-item.current .od-palette-label')?.textContent).toBe('Alpha');
    const input = container.querySelector('.od-palette-input') as HTMLInputElement;
    act(() => {
      input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowUp', bubbles: true }));
    });
    expect(container.querySelector('.od-palette-item.current .od-palette-label')?.textContent).toBe('新建任务');
    unmount();
  });

  it('非触发词模式默认高亮仍为第 0 项（回归）', () => {
    const { container, unmount } = mount(<CommandPalette open onClose={() => {}} />);
    typeQuery(container, '设置');
    const labs = labels(container);
    expect(labs.length).toBeGreaterThan(1);
    expect(labs[0]).toBe('设置 · 终端外观');
    expect(container.querySelector('.od-palette-item.current .od-palette-label')?.textContent).toBe(labs[0]);
    unmount();
  });

  it('acronym 命中进入候选，档位序 exact > acronym', () => {
    storeProjects = [
      project({ id: 'p-exact', name: 'gaaa', path: '/tmp/exact' }),
      project({ id: 'p-acro', name: 'go-ai-agent-app', path: '/tmp/acro' }),
    ];
    const { container, unmount } = mount(<CommandPalette open onClose={() => {}} />);
    typeQuery(container, 'new gaaa');
    expect(labels(container)).toEqual(['新建任务', 'gaaa', 'go-ai-agent-app']);
    // 默认高亮首个候选（exact 命中项目排在 acronym 命中之前）
    expect(container.querySelector('.od-palette-item.current .od-palette-label')?.textContent).toBe('gaaa');
    unmount();
  });

  it('仅 acronym 命中时计入命中集合：默认高亮该候选且 Enter 携带 projectID', () => {
    const onNewTask = vi.fn<(payload?: PaletteFocusPayload) => void>();
    storeProjects = [
      project({ id: 'p-zzz', name: 'zzz', path: '/tmp/zzz' }),
      project({ id: 'p-acro', name: 'go-ai-agent-app', path: '/tmp/acro' }),
    ];
    const { container, unmount } = mount(
      <CommandPalette open onClose={() => {}} onNewTask={onNewTask} />,
    );
    typeQuery(container, 'new ga');
    expect(labels(container)).toEqual(['新建任务', 'go-ai-agent-app']);
    expect(container.querySelector('.od-palette-item.current .od-palette-label')?.textContent).toBe('go-ai-agent-app');
    const input = container.querySelector('.od-palette-input') as HTMLInputElement;
    act(() => {
      input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    });
    expect(onNewTask).toHaveBeenCalledWith({ projectName: 'go-ai-agent-app', projectID: 'p-acro' });
    unmount();
  });

  it('acronym 非首字母串前缀（如 aa）走零命中 fallback', () => {
    storeProjects = [
      project({ id: 'p-zzz', name: 'zzz', path: '/tmp/zzz' }),
      project({ id: 'p-acro', name: 'go-ai-agent-app', path: '/tmp/acro' }),
    ];
    const { container, unmount } = mount(<CommandPalette open onClose={() => {}} />);
    typeQuery(container, 'new aa');
    expect(labels(container)).toEqual(['新建任务', 'go-ai-agent-app', 'zzz']);
    expect(container.querySelector('.od-palette-item.current .od-palette-label')?.textContent).toBe('新建任务');
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

  it('自由文本（零命中）Enter 只传 projectName', () => {
    const onNewTask = vi.fn();
    const { container, unmount } = mount(<CommandPalette open onClose={() => {}} onNewTask={onNewTask} />);
    // 'foo bar zzz' 零命中：默认高亮置顶命令，Enter 走自由文本路径（不含空格拆词）
    typeQuery(container, 'new foo bar zzz');
    const input = container.querySelector('.od-palette-input') as HTMLInputElement;
    act(() => {
      input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    });
    expect(onNewTask).toHaveBeenCalledWith({ projectName: 'foo bar zzz' });
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

describe('CommandPalette 指令触发词模式（4.11）', () => {
  beforeEach(() => {
    storeProjects = [
      project({ id: 'p-alpha', name: 'Alpha', path: '/tmp/alpha' }),
    ];
  });

  const currentLabel = (container: HTMLElement) =>
    container.querySelector('.od-palette-item.current .od-palette-label')?.textContent;

  function enter(container: HTMLElement) {
    act(() => {
      (container.querySelector('.od-palette-input') as HTMLInputElement).dispatchEvent(
        new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }),
      );
    });
  }

  it('cc 模式：唯一条目默认高亮，Enter 跳转指挥中心且余文被忽略', () => {
    const navigateMock = vi.mocked(navigate);
    const { container, unmount } = mount(<CommandPalette open onClose={() => {}} />);
    typeQuery(container, 'cc ');
    expect(labels(container)).toEqual(['指挥中心']);
    expect(currentLabel(container)).toBe('指挥中心');
    enter(container);
    expect(navigateMock).toHaveBeenCalledWith('/');
    unmount();
  });

  it('cc 带任意余文仍唯一条目（余文不参与过滤、不展示项目候选与任务条目）', () => {
    const { container, unmount } = mount(<CommandPalette open onClose={() => {}} />);
    typeQuery(container, 'cc 完全无关的余文 alpha');
    expect(labels(container)).toEqual(['指挥中心']);
    expect(currentLabel(container)).toBe('指挥中心');
    unmount();
  });

  it('pro（默认词表）模式 Enter 跳转项目管理；reg 模式 Enter 走 onRegisterProject 聚焦链路', () => {
    const navigateMock = vi.mocked(navigate);
    const onRegisterProject = vi.fn();
    const { container, unmount } = mount(
      <CommandPalette open onClose={() => {}} onRegisterProject={onRegisterProject} />,
    );
    // 4.12：pro（默认词表）空余文也展示项目候选（默认高亮置顶命令），Enter 跳转 /projects 不选中
    typeQuery(container, 'pro ');
    expect(labels(container)[0]).toBe('项目管理');
    expect(currentLabel(container)).toBe('项目管理');
    enter(container);
    expect(navigateMock).toHaveBeenCalledWith('/projects');

    typeQuery(container, 'reg 任意余文');
    expect(labels(container)).toEqual(['注册项目']);
    enter(container);
    expect(onRegisterProject).toHaveBeenCalledTimes(1);
    unmount();
  });

  it('最长前缀优先：p 与 pr 同启用时输入 pr 进入项目管理', () => {
    const triggers = {
      ...DEFAULT_COMMAND_TRIGGERS,
      projects: 'pr',
      'settings-appearance': 'p',
    };
    const { container, unmount } = mount(
      <CommandPalette open onClose={() => {}} commandTriggers={triggers} />,
    );
    typeQuery(container, 'pr ');
    expect(labels(container)[0]).toBe('项目管理');
    typeQuery(container, 'p ');
    expect(labels(container)).toEqual(['设置 · 终端外观']);
    unmount();
  });

  it('与 new 快速新建模式互不干扰：new x 仍快速新建、前缀指令词不吞 new', () => {
    const triggers = {
      ...DEFAULT_COMMAND_TRIGGERS,
      'settings-env': 'ne',
    };
    const { container, unmount } = mount(
      <CommandPalette open onClose={() => {}} commandTriggers={triggers} />,
    );
    // 'ne' + 'w' 非空白边界 → ne 不匹配，全局 new 胜出进入快速新建
    typeQuery(container, 'new alpha');
    expect(labels(container)[0]).toBe('新建任务');
    expect(labels(container)).toContain('Alpha');
    unmount();
  });

  it('仅指令词无尾随空白不进入模式（保持模糊匹配/零命中空态）', () => {
    const { container, unmount } = mount(<CommandPalette open onClose={() => {}} />);
    typeQuery(container, 'cc');
    expect(labels(container)).toEqual([]);
    expect(container.querySelector('.od-palette-empty')).not.toBeNull();
    unmount();
  });

  it('空值指令词不启用：cc 未配置时 cc 不进入指令模式', () => {
    const triggers = { ...DEFAULT_COMMAND_TRIGGERS, 'command-center': '' };
    const { container, unmount } = mount(
      <CommandPalette open onClose={() => {}} commandTriggers={triggers} />,
    );
    typeQuery(container, 'cc ');
    expect(labels(container)).toEqual([]);
    expect(container.querySelector('.od-palette-empty')).not.toBeNull();
    unmount();
  });
});

describe('CommandPalette projects 指令项目参数（4.12）', () => {
  const currentLabel = (container: HTMLElement) =>
    container.querySelector('.od-palette-item.current .od-palette-label')?.textContent;

  function enter(container: HTMLElement) {
    act(() => {
      (container.querySelector('.od-palette-input') as HTMLInputElement).dispatchEvent(
        new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }),
      );
    });
  }

  function topEnter(container: HTMLElement) {
    const input = container.querySelector('.od-palette-input') as HTMLInputElement;
    act(() => {
      input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowUp', bubbles: true }));
    });
    enter(container);
  }

  beforeEach(() => {
    storeProjects = [
      project({ id: 'p-xx', name: 'xxAlpha', path: '/tmp/xx' }),
      project({ id: 'p-beta', name: 'Beta', path: '/tmp/beta' }),
      project({ id: 'p-alpha', name: 'Alpha', path: '/tmp/alpha' }),
      project({ id: 'p-space', name: 'foo bar', path: '/tmp/space' }),
    ];
  });

  it('pro <项目名> 候选按排序展示、默认高亮首位、Enter 导航项目深链', () => {
    const navigateMock = vi.mocked(navigate);
    const { container, unmount } = mount(<CommandPalette open onClose={() => {}} />);
    typeQuery(container, 'pro alpha');
    expect(labels(container)).toEqual(['项目管理', 'Alpha', 'xxAlpha']);
    expect(currentLabel(container)).toBe('Alpha');
    enter(container);
    expect(navigateMock).toHaveBeenCalledWith('/projects#p-alpha');
    unmount();
  });

  it('候选点击同样导航项目深链', () => {
    const navigateMock = vi.mocked(navigate);
    const { container, unmount } = mount(<CommandPalette open onClose={() => {}} />);
    typeQuery(container, 'pro alpha');
    const second = [...container.querySelectorAll('.od-palette-item')].find(
      (el) => el.querySelector('.od-palette-hint')?.textContent === '/tmp/xx',
    )!;
    act(() => {
      (second as HTMLElement).click();
    });
    expect(navigateMock).toHaveBeenCalledWith('/projects#p-xx');
    unmount();
  });

  it('置顶命令唯一精确命中 → 导航该项目深链', () => {
    const navigateMock = vi.mocked(navigate);
    const { container, unmount } = mount(<CommandPalette open onClose={() => {}} />);
    typeQuery(container, 'pro alpha');
    topEnter(container);
    expect(navigateMock).toHaveBeenCalledWith('/projects#p-alpha');
    unmount();
  });

  it('matchMode=exact-then-substring 唯一子串命中 → 深链；exact 模式同输入不推断', () => {
    const navigateMock = vi.mocked(navigate);
    const { container, unmount } = mount(
      <CommandPalette open onClose={() => {}} matchMode="exact-then-substring" />,
    );
    typeQuery(container, 'pro eta');
    expect(labels(container)).toEqual(['项目管理', 'Beta']);
    topEnter(container);
    expect(navigateMock).toHaveBeenCalledWith('/projects#p-beta');
    unmount();

    const exact = mount(<CommandPalette open onClose={() => {}} matchMode="exact" />);
    typeQuery(exact.container, 'pro eta');
    topEnter(exact.container);
    expect(navigateMock).toHaveBeenCalledWith('/projects');
    exact.unmount();
  });

  it('零命中或多命中置顶 Enter → /projects 不选中', () => {
    const navigateMock = vi.mocked(navigate);
    const { container, unmount } = mount(<CommandPalette open onClose={() => {}} />);
    typeQuery(container, 'pro zzz');
    expect(labels(container)).toEqual(['项目管理', 'Alpha', 'Beta', 'foo bar', 'xxAlpha']);
    topEnter(container);
    expect(navigateMock).toHaveBeenCalledWith('/projects');

    // 多命中阶段：清零命中阶段的历史调用后断言，保持独立鉴别力
    navigateMock.mockClear();
    typeQuery(container, 'pro al');
    topEnter(container);
    expect(navigateMock).toHaveBeenCalledTimes(1);
    expect(navigateMock).toHaveBeenCalledWith('/projects');
    unmount();
  });

  it('缩写命中进入候选但 MUST NOT 参与置顶推断', () => {
    storeProjects = [project({ id: 'p-g', name: 'go-ai-agent-app', path: '/tmp/g' })];
    const navigateMock = vi.mocked(navigate);
    const { container, unmount } = mount(<CommandPalette open onClose={() => {}} />);
    typeQuery(container, 'pro gaaa');
    expect(labels(container)).toEqual(['项目管理', 'go-ai-agent-app']);
    expect(currentLabel(container)).toBe('go-ai-agent-app');
    topEnter(container);
    expect(navigateMock).toHaveBeenCalledWith('/projects');
    unmount();
  });

  it('空余文与零命中：全部候选按名称确定序、默认高亮置顶命令', () => {
    const { container, unmount } = mount(<CommandPalette open onClose={() => {}} />);
    typeQuery(container, 'pro ');
    expect(labels(container)).toEqual(['项目管理', 'Alpha', 'Beta', 'foo bar', 'xxAlpha']);
    expect(currentLabel(container)).toBe('项目管理');
    typeQuery(container, 'pro zzz');
    expect(labels(container)).toEqual(['项目管理', 'Alpha', 'Beta', 'foo bar', 'xxAlpha']);
    expect(currentLabel(container)).toBe('项目管理');
    unmount();
  });
});

describe('matchCommandTrigger 纯函数（4.11）', () => {
  const T = DEFAULT_COMMAND_TRIGGERS;

  it('全局词与指令词按词义分流，余文语义各自正确', () => {
    expect(matchCommandTrigger('new my project', 'new', T)).toEqual({
      kind: 'quick-create',
      projectQuery: 'my project',
    });
    expect(matchCommandTrigger('cc ', 'new', T)).toEqual({ kind: 'command', commandId: 'command-center', rest: '' });
    // projects 指令的 rest 去首尾空白保留内部空白，作为项目名查询（4.12）
    expect(matchCommandTrigger('pro  my cool  project ', 'new', T)).toEqual({
      kind: 'command',
      commandId: 'projects',
      rest: 'my cool  project',
    });
  });

  it('fold 比较（大小写不敏感）与 ECMAScript 空白边界', () => {
    expect(matchCommandTrigger('CC x', 'new', T)).toEqual({ kind: 'command', commandId: 'command-center', rest: 'x' });
    expect(matchCommandTrigger('cc\u00a0x', 'new', T)).toEqual({
      kind: 'command',
      commandId: 'command-center',
      rest: 'x',
    });
  });

  it('仅词无尾随空白返回 null；空值词不参与；词长于输入返回 null', () => {
    expect(matchCommandTrigger('cc', 'new', T)).toBeNull();
    expect(matchCommandTrigger('ccc', 'new', T)).toBeNull();
    expect(matchCommandTrigger('cc', 'new', { ...T, 'command-center': '' })).toBeNull();
    expect(matchCommandTrigger('ne', 'new', T)).toBeNull();
  });

  it('最长前缀优先（指令词之间与全局词之间统一比较）', () => {
    const triggers: Record<PaletteCommandId, string> = {
      ...T,
      'settings-appearance': 'p',
      'command-center': 'newtask',
    };
    expect(matchCommandTrigger('pro ', 'new', triggers)).toEqual({ kind: 'command', commandId: 'projects', rest: '' });
    expect(matchCommandTrigger('newtask ', 'new', triggers)).toEqual({
      kind: 'command',
      commandId: 'command-center',
      rest: '',
    });
    expect(matchCommandTrigger('new x', 'new', T)).toEqual({ kind: 'quick-create', projectQuery: 'x' });
  });
});
