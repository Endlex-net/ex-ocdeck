// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act } from 'react';
import type { Root } from 'react-dom/client';
import { CommandCenterPage } from '../pages/CommandCenterPage';
import { api } from '../api';
import {
  emitPaletteFocus,
  __resetPaletteFocusForTest,
  PALETTE_FOCUS_EVENT,
} from '../palette-focus';
import { mount, flushUI, stubMatchMedia, rerender } from './cm-test-env';
import type { Project } from '../types';

type SessionsSubOpts = {
  onData: (items: never[]) => void;
  onError: (m: string) => void;
};

let storeProjects: Project[] = [];

vi.mock('../sse', () => ({
  subscribeActiveSessions: vi.fn((_opts: SessionsSubOpts) => ({ close: vi.fn() })),
}));

vi.mock('../api', () => ({
  api: {
    listBranches: vi.fn(async () => ['main']),
    refreshBranches: vi.fn(async () => ['main']),
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
  useProjects: () => ({ projects: storeProjects, initialized: true, error: '' }),
  useProjectsRefresh: () => vi.fn(async () => {}),
  createErrorMessage: (_prefix: string, err: unknown) =>
    err instanceof Error ? err.message : String(err),
}));

function proj(id: string, name: string, over: Partial<Project> = {}): Project {
  return {
    id,
    name,
    path: `/p/${id}`,
    kind: 'repo',
    default_branch: 'main',
    created_at: 1,
    task_count: 0,
    tasks_by_status: {},
    tasks: [],
    ...over,
  };
}

function projectInput(container: HTMLElement) {
  return container.querySelector<HTMLInputElement>('input[role="combobox"]')!;
}

function taskInput(container: HTMLElement) {
  return container.querySelector<HTMLInputElement>('#cc-task-name')!;
}

function submitBtn(container: HTMLElement) {
  return [...container.querySelectorAll('button')].find((b) =>
    b.textContent?.includes('创建并进入工作台'),
  )!;
}

function setInput(el: HTMLInputElement, value: string) {
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
  setter.call(el, value);
  el.dispatchEvent(new Event('input', { bubbles: true }));
}

async function fillTaskAndSubmit(container: HTMLElement, name = 'task-a') {
  vi.mocked(api.createTask).mockResolvedValue({ id: 't1' } as never);
  await act(async () => {
    setInput(taskInput(container), name);
  });
  expect(submitBtn(container).disabled).toBe(false);
  await act(async () => {
    container
      .querySelector('#cc-new-task-panel form')!
      .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  });
  await flushUI();
}

const roots: Root[] = [];

beforeEach(() => {
  __resetPaletteFocusForTest();
  stubMatchMedia(false);
  storeProjects = [proj('p1', 'ocdeck'), proj('p2', 'other')];
  vi.mocked(api.createTask).mockReset();
  vi.mocked(api.listBranches).mockReset();
  vi.mocked(api.listBranches).mockResolvedValue(['main']);
});

afterEach(async () => {
  __resetPaletteFocusForTest();
  while (roots.length) {
    const root = roots.pop()!;
    await act(async () => {
      root.unmount();
    });
  }
});

function renderPage(ui: React.ReactElement = <CommandCenterPage />) {
  const utils = mount(ui);
  roots.push(utils.root);
  return utils;
}

describe('CommandCenterPage 快速新建初始化', () => {
  it('唯一精确匹配预选项目并聚焦任务名', async () => {
    const { container } = renderPage(<CommandCenterPage matchMode="exact" />);
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'ocdeck' });
    });
    await flushUI();
    expect(container.querySelector('#cc-new-task-panel')).not.toBeNull();
    expect(projectInput(container).value).toBe('ocdeck');
    expect(document.activeElement).toBe(taskInput(container));
    await fillTaskAndSubmit(container);
    expect(api.createTask).toHaveBeenCalledWith('p1', 'task-a', 'main');
  });

  it('唯一子串匹配在 exact-then-substring 预选', async () => {
    const { container } = renderPage(<CommandCenterPage matchMode="exact-then-substring" />);
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'deck' });
    });
    await flushUI();
    expect(projectInput(container).value).toBe('ocdeck');
    await fillTaskAndSubmit(container);
    expect(api.createTask).toHaveBeenCalledWith('p1', 'task-a', 'main');
  });

  it('matchMode=exact 时子串不预选，过滤框填文本', async () => {
    const { container } = renderPage(<CommandCenterPage matchMode="exact" />);
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'deck' });
    });
    await flushUI();
    expect(projectInput(container).value).toBe('deck');
  });

  it('零命中填过滤词不预选', async () => {
    const { container } = renderPage();
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'zzzz' });
    });
    await flushUI();
    expect(projectInput(container).value).toBe('zzzz');
  });

  it('同名多命中不预选填过滤词；有效 projectID 直接选中对应项', async () => {
    storeProjects = [proj('p1', 'ocdeck'), proj('p3', 'ocdeck')];
    const { container } = renderPage();
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'ocdeck' });
    });
    await flushUI();
    expect(projectInput(container).value).toBe('ocdeck');
    expect(submitBtn(container).disabled).toBe(true);

    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'ocdeck', projectID: 'p3' });
    });
    await flushUI();
    expect(projectInput(container).value).toBe('ocdeck');
    await fillTaskAndSubmit(container);
    expect(api.createTask).toHaveBeenCalledWith('p3', 'task-a', 'main');
  });

  it('失效 projectID 回退文本匹配', async () => {
    const { container } = renderPage();
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'other', projectID: 'gone' });
    });
    await flushUI();
    expect(projectInput(container).value).toBe('other');
    await fillTaskAndSubmit(container);
    expect(api.createTask).toHaveBeenCalledWith('p2', 'task-a', 'main');
  });

  it('acronym 命中不触发预选，按填过滤词处理（MUST NOT 参与预选推断）', async () => {
    storeProjects = [proj('g1', 'go-ai-agent-app')];
    const { container } = renderPage(<CommandCenterPage matchMode="exact-then-substring" />);
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'gaaa' });
    });
    await flushUI();
    // 不预选：输入框保持原余文（预选会显示项目名），且无 repo 基准分支字段
    expect(projectInput(container).value).toBe('gaaa');
    expect(
      [...container.querySelectorAll('#cc-new-task-panel label')].some((l) => l.textContent === '基准分支'),
    ).toBe(false);
    expect(submitBtn(container).disabled).toBe(true);
  });

  it('prefix 命中仍预选（exact-then-substring）', async () => {
    const { container } = renderPage(<CommandCenterPage matchMode="exact-then-substring" />);
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'oc' });
    });
    await flushUI();
    expect(projectInput(container).value).toBe('ocdeck');
    await fillTaskAndSubmit(container);
    expect(api.createTask).toHaveBeenCalledWith('p1', 'task-a', 'main');
  });

  it('预选后任务名为空，提交按钮禁用且不发起创建', async () => {
    const { container } = renderPage();
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'ocdeck' });
    });
    await flushUI();
    expect(submitBtn(container).disabled).toBe(true);
    await act(async () => {
      container
        .querySelector('#cc-new-task-panel form')!
        .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    });
    expect(api.createTask).not.toHaveBeenCalled();
  });

  it('空字符串 payload 清空已选项目但保留 taskName', async () => {
    const { container } = renderPage();
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'ocdeck' });
    });
    await flushUI();
    await act(async () => {
      setInput(taskInput(container), 'keep-me');
    });
    expect(submitBtn(container).disabled).toBe(false);
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: '' });
    });
    await flushUI();
    expect(taskInput(container).value).toBe('keep-me');
    expect(projectInput(container).value).toBe('');
    expect(submitBtn(container).disabled).toBe(true);
    await act(async () => {
      container
        .querySelector('#cc-new-task-panel form')!
        .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    });
    expect(api.createTask).not.toHaveBeenCalled();
  });

  it('无 payload 只展开聚焦，保持全部表单状态', async () => {
    const { container } = renderPage();
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'ocdeck' });
    });
    await flushUI();
    await act(async () => {
      setInput(taskInput(container), 'keep-me');
    });
    act(() => {
      emitPaletteFocus('new-task-name');
    });
    await flushUI();
    expect(taskInput(container).value).toBe('keep-me');
    expect(projectInput(container).value).toBe('ocdeck');
    await fillTaskAndSubmit(container, 'keep-me');
    expect(api.createTask).toHaveBeenCalledWith('p1', 'keep-me', 'main');
  });

  it('非法仅 projectID 的 detail 归一为无 payload', async () => {
    const { container } = renderPage();
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'ocdeck' });
    });
    await flushUI();
    await act(async () => {
      setInput(taskInput(container), 'keep-me');
    });
    act(() => {
      document.dispatchEvent(
        new CustomEvent(PALETTE_FOCUS_EVENT, { detail: { id: 'new-task-name', projectID: 'p1' } }),
      );
    });
    await flushUI();
    expect(taskInput(container).value).toBe('keep-me');
    expect(projectInput(container).value).toBe('ocdeck');
  });

  it('已展开面板连续信号按新 nonce 应用项目并保留 taskName', async () => {
    const { container } = renderPage();
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'ocdeck' });
    });
    await flushUI();
    await act(async () => {
      setInput(taskInput(container), 'keep-me');
    });
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'other' });
    });
    await flushUI();
    expect(taskInput(container).value).toBe('keep-me');
    expect(projectInput(container).value).toBe('other');
    expect(document.activeElement).toBe(taskInput(container));
    await fillTaskAndSubmit(container, 'keep-me');
    expect(api.createTask).toHaveBeenCalledWith('p2', 'keep-me', 'main');
  });

  it('pending 跨路由：挂载前发出的信号被消费', async () => {
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'ocdeck' });
    });
    const { container } = renderPage();
    await flushUI();
    expect(container.querySelector('#cc-new-task-panel')).not.toBeNull();
    expect(projectInput(container).value).toBe('ocdeck');
    expect(document.activeElement).toBe(taskInput(container));
    await fillTaskAndSubmit(container);
    expect(api.createTask).toHaveBeenCalledWith('p1', 'task-a', 'main');
  });

  it('只按到达时快照判定，后续项目加载不自动重试预选', async () => {
    storeProjects = [];
    const { container, root } = renderPage();
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'ocdeck' });
    });
    await flushUI();
    expect(projectInput(container).value).toBe('ocdeck');
    expect(submitBtn(container).disabled).toBe(true);

    storeProjects = [proj('p1', 'ocdeck')];
    rerender(root, <CommandCenterPage />);
    await flushUI();
    expect(projectInput(container).value).toBe('ocdeck');
    expect(submitBtn(container).disabled).toBe(true);
  });

  it('同一批次内 projects 更新不改变信号到达时快照', async () => {
    storeProjects = [];
    const { container, root } = renderPage();
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'ocdeck' });
      storeProjects = [proj('p1', 'ocdeck')];
      rerender(root, <CommandCenterPage />);
    });
    await flushUI();
    // 信号到达时项目列表为空：仅填过滤词，不因同批次项目出现而预选（无基准分支字段）
    expect(projectInput(container).value).toBe('ocdeck');
    expect(
      [...container.querySelectorAll('#cc-new-task-panel label')].some((l) => l.textContent === '基准分支'),
    ).toBe(false);
  });

  it('nonce=0 页头打开面板时聚焦任务名', async () => {
    const { container } = renderPage();
    await act(async () => {
      [...container.querySelectorAll('button')].find((b) => b.textContent?.includes('新建任务'))!.click();
    });
    await flushUI();
    expect(container.querySelector('#cc-new-task-panel')).not.toBeNull();
    expect(document.activeElement).toBe(taskInput(container));
  });

  it('每个 nonce 恰好聚焦一次；手动改项目不抢焦', async () => {
    const focusSpy = vi.spyOn(HTMLElement.prototype, 'focus');
    const { container } = renderPage();
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'ocdeck' });
    });
    await flushUI();
    const taskEl = taskInput(container);
    const focusesOnTask = () =>
      focusSpy.mock.contexts.filter((el) => el instanceof HTMLElement && el.id === 'cc-task-name').length;
    const firstCount = focusesOnTask();
    expect(firstCount).toBe(1);
    expect(document.activeElement).toBe(taskEl);

    await act(async () => {
      projectInput(container).focus();
      setInput(projectInput(container), 'typed');
    });
    await flushUI();
    expect(focusesOnTask()).toBe(firstCount);
    expect(document.activeElement).not.toBe(taskEl);

    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'other' });
    });
    await flushUI();
    expect(focusesOnTask()).toBe(firstCount + 1);
    expect(document.activeElement).toBe(taskInput(container));
    focusSpy.mockRestore();
  });

  it('相同 payload 的新 nonce 仍恰好新增一次聚焦', async () => {
    const focusSpy = vi.spyOn(HTMLElement.prototype, 'focus');
    const { container } = renderPage();
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'ocdeck' });
    });
    await flushUI();
    const focusesOnTask = () =>
      focusSpy.mock.contexts.filter((el) => el instanceof HTMLElement && el.id === 'cc-task-name').length;
    const firstCount = focusesOnTask();
    expect(firstCount).toBe(1);

    await act(async () => {
      projectInput(container).focus();
    });
    await flushUI();
    expect(focusesOnTask()).toBe(firstCount);
    expect(document.activeElement).not.toBe(taskInput(container));

    // 相同项目同值 setState 被 React 跳过，但 nonce 新 → 仍应恰好新增一次任务名聚焦
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'ocdeck' });
    });
    await flushUI();
    expect(focusesOnTask()).toBe(firstCount + 1);
    expect(document.activeElement).toBe(taskInput(container));
    focusSpy.mockRestore();
  });

  it('dir 项目与 repo 空分支仍各聚焦一次', async () => {
    storeProjects = [proj('d1', 'plain', { kind: 'dir' }), proj('r1', 'emptyrepo')];
    vi.mocked(api.listBranches).mockResolvedValue([]);
    const focusSpy = vi.spyOn(HTMLElement.prototype, 'focus');
    const { container } = renderPage();
    const taskFocuses = () =>
      focusSpy.mock.contexts.filter((el) => el instanceof HTMLElement && el.id === 'cc-task-name').length;

    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'plain', projectID: 'd1' });
    });
    await flushUI();
    expect(taskFocuses()).toBe(1);
    expect(document.activeElement).toBe(taskInput(container));

    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'emptyrepo', projectID: 'r1' });
    });
    await flushUI();
    expect(taskFocuses()).toBe(2);
    expect(document.activeElement).toBe(taskInput(container));
    focusSpy.mockRestore();
  });
});
