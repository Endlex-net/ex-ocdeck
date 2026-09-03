// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act } from 'react';
import type { Root } from 'react-dom/client';
import { CommandCenterPage } from '../pages/CommandCenterPage';
import { api, ApiError } from '../api';
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
  // 与 api.ts 真实签名同形（status, code, message），避免测试替身参数错位
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
  useProjects: () => ({ projects: storeProjects, initialized: true, error: '' }),
  useProjectsRefresh: () => vi.fn(async () => {}),
  // 与 hooks.ts createErrorMessage(err) 同签名：非 ApiError 落入通用分支
  createErrorMessage: (err: unknown) => (err instanceof Error ? err.message : String(err)),
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

function branchInput(container: HTMLElement) {
  return container.querySelectorAll<HTMLInputElement>('input[role="combobox"]')[1]!;
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
  await dispatchSubmit(container);
}

// 浏览器中任务名框 Enter 与创建按钮点击都触发 form submit；jsdom 以 submit 事件等价驱动
async function dispatchSubmit(container: HTMLElement) {
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
  vi.mocked(api.refreshBranches).mockReset();
  vi.mocked(api.refreshBranches).mockResolvedValue(['main']);
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

describe('CommandCenterPage 基准分支排序与分支列表状态机（task-base-branch-context）', () => {
  it('预填 main 且存在 origin/main 时点创建提交 origin/main（远端同名优先）', async () => {
    vi.mocked(api.listBranches).mockResolvedValue(['main', 'origin/main']);
    const { container } = renderPage();
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'ocdeck', projectID: 'p1' });
    });
    await flushUI();
    expect(branchInput(container).value).toBe('main');
    await fillTaskAndSubmit(container);
    expect(api.createTask).toHaveBeenCalledWith('p1', 'task-a', 'origin/main');
  });

  it('任务名框 Enter 与创建按钮同路径：提交过滤首项', async () => {
    vi.mocked(api.listBranches).mockResolvedValue(['main', 'origin/main']);
    const { container } = renderPage();
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'ocdeck', projectID: 'p1' });
    });
    await flushUI();
    await act(async () => {
      setInput(taskInput(container), 'task-a');
    });
    await dispatchSubmit(container);
    expect(api.createTask).toHaveBeenCalledWith('p1', 'task-a', 'origin/main');
  });

  it('synthetic 候选排第一时提交 normalizedInput（trim 首尾空白）', async () => {
    vi.mocked(api.listBranches).mockResolvedValue(['main', 'develop']);
    const { container } = renderPage();
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'ocdeck', projectID: 'p1' });
    });
    await flushUI();
    await act(async () => {
      setInput(branchInput(container), '  feature-x  ');
    });
    await fillTaskAndSubmit(container);
    expect(api.createTask).toHaveBeenCalledWith('p1', 'task-a', 'feature-x');
  });

  it('synthetic 只参与排序不保证第一：输入 main 时提交 origin/main', async () => {
    vi.mocked(api.listBranches).mockResolvedValue(['origin/main']);
    const { container } = renderPage();
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'ocdeck', projectID: 'p1' });
    });
    await flushUI();
    await act(async () => {
      setInput(branchInput(container), 'main');
    });
    await fillTaskAndSubmit(container);
    expect(api.createTask).toHaveBeenCalledWith('p1', 'task-a', 'origin/main');
  });

  it('dir 项目提交不携带 base_ref', async () => {
    storeProjects = [proj('d1', 'plain', { kind: 'dir' })];
    const { container } = renderPage();
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'plain', projectID: 'd1' });
    });
    await flushUI();
    // dir 无基准分支字段（仅项目一个 combobox）
    expect(container.querySelectorAll('input[role="combobox"]').length).toBe(1);
    await fillTaskAndSubmit(container);
    expect(api.createTask).toHaveBeenCalledWith('d1', 'task-a', undefined);
  });

  it('初次加载在途：提交禁用且不发起 POST；ready 后提交过滤首项', async () => {
    let resolveBranches!: (v: string[]) => void;
    vi.mocked(api.listBranches).mockImplementation(
      () =>
        new Promise<string[]>((res) => {
          resolveBranches = res;
        }),
    );
    const { container } = renderPage();
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'ocdeck', projectID: 'p1' });
    });
    await flushUI(); // 初次 GET 仍在途
    await act(async () => {
      setInput(taskInput(container), 'task-a');
    });
    expect(submitBtn(container).disabled).toBe(true);
    await dispatchSubmit(container);
    expect(api.createTask).not.toHaveBeenCalled();

    await act(async () => {
      resolveBranches(['main', 'origin/main']);
    });
    await flushUI();
    expect(submitBtn(container).disabled).toBe(false);
    await dispatchSubmit(container);
    expect(api.createTask).toHaveBeenCalledWith('p1', 'task-a', 'origin/main');
  });

  it('初次加载失败：列表为空、禁止提交、不发起 POST', async () => {
    vi.mocked(api.listBranches).mockRejectedValue(new Error('boom'));
    const { container } = renderPage();
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'ocdeck', projectID: 'p1' });
    });
    await flushUI();
    expect(container.textContent).toContain('获取分支列表失败');
    // 打开分支下拉确认列表为空（error 且无历史 → 无选项，仅刷新入口）
    await act(async () => {
      branchInput(container).focus();
    });
    expect(container.querySelectorAll('.cc-combo-item').length).toBe(0);
    await act(async () => {
      setInput(taskInput(container), 'task-a');
    });
    expect(submitBtn(container).disabled).toBe(true);
    await dispatchSubmit(container);
    expect(api.createTask).not.toHaveBeenCalled();
  });

  it('refresh 失败保留 stale 列表并标注「本地快照未刷新」且禁止提交；重试成功恢复提交', async () => {
    vi.mocked(api.listBranches).mockResolvedValue(['main', 'origin/main']);
    const { container } = renderPage();
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'ocdeck', projectID: 'p1' });
    });
    await flushUI();
    await act(async () => {
      setInput(taskInput(container), 'task-a');
    });
    expect(submitBtn(container).disabled).toBe(false);

    const refreshBtn = () =>
      [...container.querySelectorAll('button')].find((b) => b.textContent?.includes('刷新远端分支'))!;
    await act(async () => {
      branchInput(container).focus();
    });
    expect(refreshBtn()).toBeTruthy();

    // 刷新失败：stale 列表仍展示 + 「本地快照未刷新」 + 禁止提交、不发起 POST
    vi.mocked(api.refreshBranches).mockRejectedValue(new Error('boom'));
    await act(async () => {
      refreshBtn().click();
    });
    await flushUI();
    expect(container.textContent).toContain('本地快照未刷新');
    expect(
      [...container.querySelectorAll('.cc-combo-item')].some((b) => b.textContent === 'origin/main'),
    ).toBe(true);
    expect(submitBtn(container).disabled).toBe(true);
    await dispatchSubmit(container);
    expect(api.createTask).not.toHaveBeenCalled();

    // 重试成功：ready 恢复提交，且使用刷新后的过滤首项
    vi.mocked(api.refreshBranches).mockResolvedValue(['develop', 'main', 'origin/main']);
    await act(async () => {
      refreshBtn().click();
    });
    await flushUI();
    expect(submitBtn(container).disabled).toBe(false);
    await dispatchSubmit(container);
    expect(api.createTask).toHaveBeenCalledWith('p1', 'task-a', 'origin/main');
  });

  it('成功空列表回退 default_branch：提交 base_ref=main', async () => {
    vi.mocked(api.listBranches).mockResolvedValue([]);
    const { container } = renderPage();
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'ocdeck', projectID: 'p1' });
    });
    await flushUI();
    await fillTaskAndSubmit(container);
    expect(api.createTask).toHaveBeenCalledWith('p1', 'task-a', 'main');
  });

  it('候选全空时省略 base_ref：服务端 invalid_input 后页面展示创建失败', async () => {
    storeProjects = [proj('p1', 'ocdeck', { default_branch: '' })];
    vi.mocked(api.listBranches).mockResolvedValue([]);
    vi.mocked(api.createTask).mockRejectedValue(new ApiError(400, 'invalid_input', '基准分支不能为空'));
    const { container } = renderPage();
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'ocdeck', projectID: 'p1' });
    });
    await flushUI();
    // ready 可提交，但候选全空 → base_ref 省略（api 层 falsy 省略字段）
    await act(async () => {
      setInput(taskInput(container), 'task-a');
    });
    expect(submitBtn(container).disabled).toBe(false);
    await dispatchSubmit(container);
    expect(api.createTask).toHaveBeenCalledWith('p1', 'task-a', undefined);
    expect(container.textContent).toContain('基准分支不能为空');
  });

  it('跨项目刷新竞态：A 旧刷新晚于 B 新刷新完成，不清 B 的刷新指示也不释放单飞锁', async () => {
    vi.mocked(api.listBranches).mockResolvedValue(['main']);
    const { container } = renderPage();
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'ocdeck', projectID: 'p1' });
    });
    await flushUI();

    const refreshBtn = () =>
      [...container.querySelectorAll('button')].find((b) => b.textContent?.includes('刷新远端分支'))!;
    let resolveA!: (v: string[]) => void;
    vi.mocked(api.refreshBranches).mockImplementationOnce(
      () =>
        new Promise<string[]>((res) => {
          resolveA = res;
        }),
    );
    await act(async () => {
      branchInput(container).focus();
    });
    await act(async () => {
      refreshBtn().click();
    });
    await flushUI();
    expect(api.refreshBranches).toHaveBeenCalledTimes(1);

    // 切到 B（effect 重置刷新状态与所有权）并发起 B 的刷新（在途）
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'other', projectID: 'p2' });
    });
    await flushUI();
    let resolveB!: (v: string[]) => void;
    vi.mocked(api.refreshBranches).mockImplementationOnce(
      () =>
        new Promise<string[]>((res) => {
          resolveB = res;
        }),
    );
    await act(async () => {
      branchInput(container).focus();
    });
    await act(async () => {
      refreshBtn().click();
    });
    await flushUI();
    expect(api.refreshBranches).toHaveBeenCalledTimes(2);
    expect(container.textContent).toContain('刷新中…');

    // A 旧请求此时才完成：不得清 B 的刷新指示、不得释放 B 的单飞锁
    await act(async () => {
      resolveA(['main', 'origin/main']);
    });
    await flushUI();
    expect(container.textContent).toContain('刷新中…');
    await act(async () => {
      refreshBtn().click();
    });
    await flushUI();
    expect(api.refreshBranches).toHaveBeenCalledTimes(2); // B 仍在途，未被旧 finally 释放

    // B 完成：ready + 刷新指示清除 + 提交恢复
    await act(async () => {
      resolveB(['develop', 'main']);
    });
    await flushUI();
    expect(container.textContent).not.toContain('刷新中…');
    await act(async () => {
      setInput(taskInput(container), 'task-a');
    });
    await dispatchSubmit(container);
    expect(api.createTask).toHaveBeenCalledWith('p2', 'task-a', 'main');
  });

  it('下拉高亮过滤排序首项：输入 main 时高亮 origin/main 而非输入精确等值项', async () => {
    vi.mocked(api.listBranches).mockResolvedValue(['main', 'origin/main']);
    const { container } = renderPage();
    act(() => {
      emitPaletteFocus('new-task-name', { projectName: 'ocdeck', projectID: 'p1' });
    });
    await flushUI();
    // 预填 main，打开下拉：D2 排序后首项为 origin/main，高亮应随首项而非 baseRef 精确匹配
    await act(async () => {
      branchInput(container).focus();
    });
    const hl = () => container.querySelector('.cc-combo-item.hl');
    expect(hl()).not.toBeNull();
    expect(hl()!.textContent).toBe('origin/main');
  });
});
