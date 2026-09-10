// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act } from 'react';
import type { Root } from 'react-dom/client';
import { CommandCenterPage } from '../pages/CommandCenterPage';
import { api } from '../api';
import { emitPaletteFocus, __resetPaletteFocusForTest } from '../palette-focus';
import { mount, flushUI, stubMatchMedia } from './cm-test-env';
import type { Project } from '../types';

/* ==================== 新建任务面板权限模式（task-permission-mode tasks 5.2） ====================
 * 覆盖 spec「Web 新建任务权限模式选择」三个 scenario 与 design D8：
 * - 表单缺省 ask（三段 segmented control，工作空间控件同型，缺省选中「人工批准」）；
 * - 选择 all-approve / ai-auto 提交 → createTask 第 5 参透传；缺省 ask 不传（presence）；
 * - 重置规则与 runMode 逐字一致：项目 ID 变更（切换/清除/切到 dir）→ ask；同项目信号保持。 */

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

/** 权限模式 radiogroup（aria-labelledby 指向「权限模式」label）。 */
function permGroup(container: HTMLElement) {
  const label = [...container.querySelectorAll('#cc-new-task-panel label')].find(
    (l) => l.textContent === '权限模式',
  )!;
  return container.querySelector(`#cc-new-task-panel [aria-labelledby="${label.id}"]`)!;
}

function permRadio(container: HTMLElement, label: string) {
  return [...permGroup(container).querySelectorAll<HTMLButtonElement>('button[role="radio"]')].find(
    (b) => b.textContent === label,
  )!;
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

async function clickPerm(container: HTMLElement, label: string) {
  await act(async () => {
    permRadio(container, label).click();
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

describe('新建任务面板权限模式三档（task-permission-mode 5.2）', () => {
  it('表单缺省 ask：三段选择器渲染、缺省选中「人工批准」，提交不携带 permission_mode（presence）', async () => {
    const { container } = renderPage();
    await openWithProject('ocdeck', 'p1');

    // 三段选择器渲染，文案为定稿三档短标签
    expect(permRadio(container, '人工批准')).not.toBeNull();
    expect(permRadio(container, '全部批准')).not.toBeNull();
    expect(permRadio(container, 'AI 自动识别')).not.toBeNull();
    // 缺省选中 ask
    expect(permRadio(container, '人工批准').getAttribute('aria-checked')).toBe('true');
    expect(permRadio(container, '全部批准').getAttribute('aria-checked')).toBe('false');
    expect(permRadio(container, 'AI 自动识别').getAttribute('aria-checked')).toBe('false');

    await fillTaskAndSubmit(container);
    // 缺省 ask 不传：第 5 参为 undefined（api 层缺省不携带 permission_mode 字段）
    expect(api.createTask).toHaveBeenCalledWith('p1', 'task-a', 'main', undefined, undefined);
  });

  it('选择「全部批准」提交 → 第 5 参透传 all-approve', async () => {
    const { container } = renderPage();
    await openWithProject('ocdeck', 'p1');
    await clickPerm(container, '全部批准');

    expect(permRadio(container, '全部批准').getAttribute('aria-checked')).toBe('true');
    expect(permRadio(container, '人工批准').getAttribute('aria-checked')).toBe('false');

    await fillTaskAndSubmit(container);
    expect(api.createTask).toHaveBeenCalledWith('p1', 'task-a', 'main', undefined, 'all-approve');
  });

  it('选择「AI 自动识别」提交 → 第 5 参透传 ai-auto（local-path 路径同样透传）', async () => {
    const { container } = renderPage();
    await openWithProject('ocdeck', 'p1');
    await clickPerm(container, 'AI 自动识别');
    // 同项目切到 local 工作空间：权限模式选择保持
    await act(async () => {
      [...container.querySelectorAll<HTMLButtonElement>('#cc-new-task-panel button[role="radio"]')]
        .find((b) => b.textContent?.includes('local'))!
        .click();
    });
    await flushUI();
    expect(permRadio(container, 'AI 自动识别').getAttribute('aria-checked')).toBe('true');

    await fillTaskAndSubmit(container);
    expect(api.createTask).toHaveBeenCalledWith('p1', 'task-a', undefined, 'local-path', 'ai-auto');
  });

  it('选择后显式选回「人工批准」→ 按缺省处理，不携带 permission_mode', async () => {
    const { container } = renderPage();
    await openWithProject('ocdeck', 'p1');
    await clickPerm(container, '全部批准');
    await clickPerm(container, '人工批准');

    await fillTaskAndSubmit(container);
    expect(api.createTask).toHaveBeenCalledWith('p1', 'task-a', 'main', undefined, undefined);
  });

  it('dir 项目同样渲染权限模式控件并可透传选择', async () => {
    storeProjects = [proj('d1', 'plain', { kind: 'dir' })];
    const { container } = renderPage();
    await openWithProject('plain', 'd1');

    expect(permGroup(container)).not.toBeNull();
    await clickPerm(container, 'AI 自动识别');

    await fillTaskAndSubmit(container);
    expect(api.createTask).toHaveBeenCalledWith('d1', 'task-a', undefined, undefined, 'ai-auto');
  });
});

describe('权限模式选择器重置规则（与 runMode 逐字一致，task-permission-mode D8）', () => {
  it('项目 ID 变更（切换项目/切到 dir/清除选择）重置为「人工批准」', async () => {
    storeProjects = [proj('p1', 'ocdeck'), proj('p2', 'other'), proj('d1', 'plain', { kind: 'dir' })];
    const { container } = renderPage();
    await openWithProject('ocdeck', 'p1');
    await clickPerm(container, '全部批准');
    expect(permRadio(container, '全部批准').getAttribute('aria-checked')).toBe('true');

    // ① 切换项目：p1 → p2（repo → repo）
    await openWithProject('other', 'p2');
    expect(permRadio(container, '人工批准').getAttribute('aria-checked')).toBe('true');
    expect(permRadio(container, '全部批准').getAttribute('aria-checked')).toBe('false');

    // ② repo → dir：权限模式随项目 ID 变更重置；切回 repo 后验证未继承此前选择
    await clickPerm(container, 'AI 自动识别');
    await openWithProject('plain', 'd1');
    expect(permRadio(container, '人工批准').getAttribute('aria-checked')).toBe('true');
    await openWithProject('other', 'p2');
    expect(permRadio(container, '人工批准').getAttribute('aria-checked')).toBe('true');

    // ③ 清除项目选择（偏离已选输入）→ 重选另一 repo：仍缺省 ask
    await clickPerm(container, '全部批准');
    await act(async () => {
      setInput(projectInput(container), 'deviate');
    });
    await flushUI();
    await openWithProject('ocdeck', 'p1');
    expect(permRadio(container, '人工批准').getAttribute('aria-checked')).toBe('true');
    expect(permRadio(container, '全部批准').getAttribute('aria-checked')).toBe('false');
  });

  it('不改变项目的信号保持选择：无 payload new（keep）不重置权限模式', async () => {
    const { container } = renderPage();
    await openWithProject('ocdeck', 'p1');
    await clickPerm(container, 'AI 自动识别');

    act(() => {
      emitPaletteFocus('new-task-name');
    });
    await flushUI();

    expect(permRadio(container, 'AI 自动识别').getAttribute('aria-checked')).toBe('true');
    expect(permRadio(container, '人工批准').getAttribute('aria-checked')).toBe('false');
  });
});
