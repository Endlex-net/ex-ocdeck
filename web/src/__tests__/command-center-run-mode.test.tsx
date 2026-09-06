// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act } from 'react';
import type { Root } from 'react-dom/client';
import { CommandCenterPage } from '../pages/CommandCenterPage';
import { api } from '../api';
import { emitPaletteFocus, __resetPaletteFocusForTest } from '../palette-focus';
import { mount, flushUI, stubMatchMedia } from './cm-test-env';
import type { Project } from '../types';

/* ==================== 新建任务面板工作空间（add-local-path-task-mode tasks 5.7） ====================
 * 覆盖 command-center delta「指挥中心内联新建任务」：
 * - 三态：默认 worktree（选择器+基准分支）/ local-path（隐藏基准分支、绕过 ready 门禁、
 *   逐字提醒、提交 mode=local-path 且无 base_ref）/ dir（不渲染选择器、保留色块警告、无 mode）
 * - 选择器状态重置规则四条：项目 ID 变更重置；同项目手动切换保留分支状态；不改变项目的信号
 *   （无 payload new / keep）保持 mode；repo 初次分支请求始终发起、模式切换不重发。 */

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

function hasBranchField(container: HTMLElement) {
  return [...container.querySelectorAll('#cc-new-task-panel label')].some(
    (l) => l.textContent === '基准分支',
  );
}

function radio(container: HTMLElement, label: string) {
  return [...container.querySelectorAll<HTMLButtonElement>('#cc-new-task-panel button[role="radio"]')].find(
    (b) => b.textContent?.includes(label),
  )!;
}

function modeHint(container: HTMLElement) {
  return container.querySelector('#cc-new-task-panel .cc-mode-hint');
}

function dirWarn(container: HTMLElement) {
  return container.querySelector('#cc-new-task-panel .cc-dir-warn');
}

function hintP(container: HTMLElement) {
  return container.querySelector('#cc-new-task-panel .od-hint');
}

async function fillTaskName(container: HTMLElement, name = 'task-a') {
  await act(async () => {
    setInput(taskInput(container), name);
  });
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

async function fillTaskAndSubmit(container: HTMLElement, name = 'task-a') {
  vi.mocked(api.createTask).mockResolvedValue({ id: 't1' } as never);
  await fillTaskName(container, name);
  expect(submitBtn(container).disabled).toBe(false);
  await dispatchSubmit(container);
}

/** 打开面板并选中指定项目（palette 信号 projectID 直选路径）。 */
async function openWithProject(name: string, id: string) {
  act(() => {
    emitPaletteFocus('new-task-name', { projectName: name, projectID: id });
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

describe('新建任务面板工作空间三态（add-local-path-task-mode 5.7）', () => {
  it('默认态：repo 项目缺省「worktree」，渲染基准分支，提交体不含 mode 字段（presence）', async () => {
    const { container } = renderPage();
    await openWithProject('ocdeck', 'p1');

    // 双段选择器渲染，缺省选中「worktree」
    expect(container.querySelector('#cc-new-task-panel .cc-segment')).not.toBeNull();
    expect(radio(container, 'worktree').getAttribute('aria-checked')).toBe('true');
    expect(radio(container, 'local').getAttribute('aria-checked')).toBe('false');
    expect(hasBranchField(container)).toBe(true);
    // 非 local-path 提醒、非 dir 色块警告
    expect(modeHint(container)).toBeNull();
    expect(dirWarn(container)).toBeNull();
    expect(hintP(container)!.textContent).toBe(
      '创建后自动切出独立分支与 worktree，并进入工作台；任务名将生成英文分支 slug。',
    );

    vi.mocked(api.listBranches).mockResolvedValue(['main']);
    await fillTaskAndSubmit(container);
    // 三参调用：mode 字段缺席（presence 契约，worktree 走既有缺省语义）
    expect(api.createTask).toHaveBeenCalledWith('p1', 'task-a', 'main');
    expect(vi.mocked(api.createTask).mock.calls[0]).toHaveLength(3);
  });

  it('local-path 态：隐藏基准分支、逐字灰字提醒与 od-hint、提交 mode=local-path 且无 base_ref', async () => {
    const { container } = renderPage();
    await openWithProject('ocdeck', 'p1');
    await act(async () => {
      radio(container, 'local').click();
    });
    await flushUI();

    expect(radio(container, 'local').getAttribute('aria-checked')).toBe('true');
    expect(radio(container, 'worktree').getAttribute('aria-checked')).toBe('false');
    // 基准分支字段整体隐藏（非禁用）
    expect(hasBranchField(container)).toBe(false);
    // 灰字提醒逐字文案（command-center delta「运行模式选择器（repo）」）
    expect(modeHint(container)!.textContent).toContain(
      '直接在项目目录里跑，改动就地生效。多任务共享同一目录，并行与否自己把握。',
    );
    // 底部 od-hint 切换为不承诺分支/worktree 的版本
    expect(hintP(container)!.textContent).toBe(
      '创建后直接在当前目录运行并进入工作台，不切分支。',
    );

    vi.mocked(api.createTask).mockResolvedValue({ id: 't1' } as never);
    await fillTaskAndSubmit(container);
    // 携带 mode=local-path 且 MUST NOT 携带 base_ref
    expect(api.createTask).toHaveBeenCalledWith('p1', 'task-a', undefined, 'local-path');
  });

  it('local-path 绕过 ready 门禁：分支列表在途（loading）仍可提交', async () => {
    let resolveBranches!: (v: string[]) => void;
    vi.mocked(api.listBranches).mockImplementation(
      () =>
        new Promise<string[]>((res) => {
          resolveBranches = res;
        }),
    );
    const { container } = renderPage();
    await openWithProject('ocdeck', 'p1');
    await fillTaskName(container);

    // worktree 模式：loading 门禁生效，禁止提交
    expect(submitBtn(container).disabled).toBe(true);
    await dispatchSubmit(container);
    expect(api.createTask).not.toHaveBeenCalled();

    // 切到 local：不再等待分支列表 ready，loading 在途仍可提交
    await act(async () => {
      radio(container, 'local').click();
    });
    await flushUI();
    expect(submitBtn(container).disabled).toBe(false);
    vi.mocked(api.createTask).mockResolvedValue({ id: 't1' } as never);
    await dispatchSubmit(container);
    expect(api.createTask).toHaveBeenCalledWith('p1', 'task-a', undefined, 'local-path');

    // 在途请求最终完成不破坏断言（防未处理 rejection/泄漏）
    await act(async () => {
      resolveBranches(['main']);
    });
    await flushUI();
  });

  it('dir 态：不渲染选择器、保留色块警告、不出现灰字提醒、提交不带 mode 字段', async () => {
    storeProjects = [proj('d1', 'plain', { kind: 'dir' })];
    const { container } = renderPage();
    await openWithProject('plain', 'd1');

    expect(container.querySelector('#cc-new-task-panel .cc-segment')).toBeNull();
    expect(container.textContent).not.toContain('local');
    // 现有色块警告原样保留，与 local-path 灰字提醒互斥
    expect(dirWarn(container)).not.toBeNull();
    expect(dirWarn(container)!.textContent).toContain('纯目录项目无文件隔离');
    expect(modeHint(container)).toBeNull();
    expect(hasBranchField(container)).toBe(false);

    vi.mocked(api.createTask).mockResolvedValue({ id: 't1' } as never);
    await fillTaskAndSubmit(container);
    expect(api.createTask).toHaveBeenCalledWith('d1', 'task-a', undefined);
    expect(vi.mocked(api.createTask).mock.calls[0]).toHaveLength(3);
  });
});

describe('选择器状态重置规则（add-local-path-task-mode 5.7）', () => {
  it('项目 ID 变更（切换项目/切到 dir/清除选择）重置为「worktree」', async () => {
    storeProjects = [proj('p1', 'ocdeck'), proj('p2', 'other'), proj('d1', 'plain', { kind: 'dir' })];
    const { container } = renderPage();
    await openWithProject('ocdeck', 'p1');
    await act(async () => {
      radio(container, 'local').click();
    });
    await flushUI();
    expect(radio(container, 'local').getAttribute('aria-checked')).toBe('true');

    // ① 切换项目：p1 → p2（repo → repo）
    await openWithProject('other', 'p2');
    expect(radio(container, 'worktree').getAttribute('aria-checked')).toBe('true');
    expect(radio(container, 'local').getAttribute('aria-checked')).toBe('false');

    // ② repo → dir：选择器不渲染（dir）；切回 repo 后验证未继承就地选择
    await act(async () => {
      radio(container, 'local').click();
    });
    await flushUI();
    await openWithProject('plain', 'd1');
    expect(container.querySelector('#cc-new-task-panel .cc-segment')).toBeNull();
    await openWithProject('other', 'p2');
    expect(radio(container, 'worktree').getAttribute('aria-checked')).toBe('true');

    // ③ 清除项目选择（偏离已选输入）→ 重选另一 repo：仍缺省 worktree
    await act(async () => {
      radio(container, 'local').click();
    });
    await flushUI();
    await act(async () => {
      setInput(projectInput(container), 'deviate');
    });
    await flushUI();
    expect(container.querySelector('#cc-new-task-panel .cc-segment')).toBeNull();
    await openWithProject('ocdeck', 'p1');
    expect(radio(container, 'worktree').getAttribute('aria-checked')).toBe('true');
    expect(radio(container, 'local').getAttribute('aria-checked')).toBe('false');
  });

  it('同项目手动切换保留已选分支与分支状态，不重新请求分支列表', async () => {
    vi.mocked(api.listBranches).mockResolvedValue(['main', 'origin/main', 'develop']);
    const { container } = renderPage();
    await openWithProject('ocdeck', 'p1');
    expect(api.listBranches).toHaveBeenCalledTimes(1);

    await act(async () => {
      setInput(branchInput(container), 'develop');
    });
    await act(async () => {
      radio(container, 'local').click();
    });
    await flushUI();
    expect(hasBranchField(container)).toBe(false);
    expect(api.listBranches).toHaveBeenCalledTimes(1);

    // 切回 worktree：基准分支字段恢复展示、已选分支保留、分支列表状态保持原样
    await act(async () => {
      radio(container, 'worktree').click();
    });
    await flushUI();
    expect(hasBranchField(container)).toBe(true);
    expect(branchInput(container).value).toBe('develop');
    expect(api.listBranches).toHaveBeenCalledTimes(1);

    // 保留的已选分支仍驱动提交（base_ref = 过滤排序首项 = develop）
    await fillTaskAndSubmit(container);
    expect(api.createTask).toHaveBeenCalledWith('p1', 'task-a', 'develop');
  });

  it('不改变项目的信号保持 mode：无 payload new（keep）不重置 local 选择', async () => {
    const { container } = renderPage();
    await openWithProject('ocdeck', 'p1');
    await act(async () => {
      radio(container, 'local').click();
    });
    await flushUI();

    act(() => {
      emitPaletteFocus('new-task-name');
    });
    await flushUI();

    expect(radio(container, 'local').getAttribute('aria-checked')).toBe('true');
    expect(modeHint(container)).not.toBeNull();
    expect(hasBranchField(container)).toBe(false);
  });

  it('repo 初次分支请求始终发起（与模式无关），模式切换不重发', async () => {
    const { container } = renderPage();
    await openWithProject('ocdeck', 'p1');
    expect(api.listBranches).toHaveBeenCalledTimes(1);

    // 同项目反复切换模式：不重新请求分支列表
    await act(async () => {
      radio(container, 'local').click();
    });
    await flushUI();
    await act(async () => {
      radio(container, 'worktree').click();
    });
    await flushUI();
    expect(api.listBranches).toHaveBeenCalledTimes(1);

    // 切到 p2：初次请求再次发起（项目选中即发起，与当前模式无关）
    await openWithProject('other', 'p2');
    expect(api.listBranches).toHaveBeenCalledTimes(2);
    expect(vi.mocked(api.listBranches).mock.calls[1][0]).toBe('p2');
  });
});
