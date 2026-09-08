// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act } from 'react';
import type { Root } from 'react-dom/client';
import { ProjectsManagePage } from '../pages/ProjectsManagePage';
import { emitPaletteFocus, __resetPaletteFocusForTest } from '../palette-focus';
import { mount, flushUI, stubMatchMedia } from './cm-test-env';
import { api } from '../api';
import type { Project } from '../types';

// 注册表单重复路径提醒（allow-duplicate-project-path tasks 3.3）：
// 输入重复 path 即时提醒（含项目名）、修正即消失、不阻塞提交、trim 命中、仅空白不提醒。

let storeProjects: Project[] = [];

vi.mock('../hooks', () => ({
  useProjects: () => ({ projects: storeProjects, initialized: true, loading: false, error: '' }),
  useProjectsRefresh: () => vi.fn(async () => {}),
  runProjectMutation: async (opts: {
    mutate: () => Promise<unknown>;
    refresh: () => Promise<void>;
    onSuccess: () => void;
    onError: (err: unknown) => void;
  }) => {
    try {
      await opts.mutate();
    } catch (err) {
      opts.onError(err);
      return false;
    }
    opts.onSuccess();
    return true;
  },
  createErrorMessage: (err: unknown) => (err instanceof Error ? err.message : String(err)),
  deleteErrorMessage: (err: unknown) => (err instanceof Error ? err.message : String(err)),
}));

vi.mock('../api', () => ({
  api: {
    getProject: vi.fn(async () => ({ id: 'p1', name: 'demo', path: '/p', kind: 'repo', default_branch: 'main', created_at: 1, task_count: 0, tasks_by_status: {}, tasks: [] })),
    listTasks: vi.fn(async () => []),
    listBranches: vi.fn(async () => ['main']),
    refreshBranches: vi.fn(async () => ['main']),
    createProject: vi.fn(async () => ({ id: 'p-new', name: 'new-name', path: '/p', kind: 'repo', default_branch: 'main', created_at: 2, task_count: 0, tasks_by_status: {}, tasks: [] })),
    deleteProject: vi.fn(),
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

vi.mock('../router', () => ({
  navigate: vi.fn(),
  useHashRoute: () => '/projects',
}));

function project(id: string, name: string, path: string, kind: Project['kind'] = 'repo'): Project {
  return { id, name, path, kind, default_branch: 'main', created_at: 1, task_count: 0, tasks_by_status: {}, tasks: [] };
}

const roots: Root[] = [];

beforeEach(() => {
  __resetPaletteFocusForTest();
  stubMatchMedia(false);
  storeProjects = [project('p1', 'demo', '/p')];
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

function renderPage() {
  const utils = mount(<ProjectsManagePage />);
  roots.push(utils.root);
  return utils;
}

async function openRegisterForm() {
  emitPaletteFocus('register-project-name');
  const { container } = renderPage();
  await flushUI();
  return container;
}

function setInputValue(el: HTMLInputElement, value: string) {
  act(() => {
    const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
    setter.call(el, value);
    el.dispatchEvent(new Event('input', { bubbles: true }));
  });
}

function pathInput(container: HTMLElement) {
  return container.querySelector<HTMLInputElement>('#pjm-reg-path')!;
}

function dupAlert(container: HTMLElement) {
  return container.querySelector('[data-od-id="register-path-duplicate"]');
}

async function submitForm(container: HTMLElement) {
  await act(async () => {
    container
      .querySelector('form')!
      .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  });
  await flushUI();
}

describe('ProjectsManagePage 注册表单重复路径提醒', () => {
  it('输入重复 path 即时提醒，列出全部已注册该路径的项目名，提交按钮保持可用', async () => {
    storeProjects.push(project('p2', 'demo2', '/p'));
    const container = await openRegisterForm();
    setInputValue(pathInput(container), '/p');

    const alert = dupAlert(container);
    expect(alert).not.toBeNull();
    expect(alert!.textContent).toContain('该路径已注册项目');
    expect(alert!.textContent).toContain('「demo」');
    expect(alert!.textContent).toContain('「demo2」');
    expect(alert!.textContent).toContain('将继续创建独立的新项目');

    const submitBtn = container.querySelector<HTMLButtonElement>('button[type="submit"]')!;
    expect(submitBtn.disabled).toBe(false);
  });

  it('路径修改为不重复值后提醒消失', async () => {
    const container = await openRegisterForm();
    setInputValue(pathInput(container), '/p');
    expect(dupAlert(container)).not.toBeNull();

    setInputValue(pathInput(container), '/other');
    expect(dupAlert(container)).toBeNull();
  });

  it('提醒展示中提交正常创建：createProject 被调用且参数正确', async () => {
    const container = await openRegisterForm();
    setInputValue(container.querySelector<HTMLInputElement>('[data-od-id="register-project-name"]')!, 'new-name');
    setInputValue(pathInput(container), '/p');
    expect(dupAlert(container)).not.toBeNull();

    await submitForm(container);

    expect(api.createProject).toHaveBeenCalledWith('new-name', '/p', 'repo');
  });

  it('输入带前后空白 trim 后命中已注册路径', async () => {
    const container = await openRegisterForm();
    setInputValue(pathInput(container), '  /p  ');

    const alert = dupAlert(container);
    expect(alert).not.toBeNull();
    expect(alert!.textContent).toContain('「demo」');
  });

  it('输入仅空白（trim 后为空）不提醒', async () => {
    const container = await openRegisterForm();
    setInputValue(pathInput(container), '   ');

    expect(dupAlert(container)).toBeNull();
  });

  it('跨 kind 命中：已注册 dir 项目，repo 表单输入同 path 仍提醒（不按 kind 过滤）', async () => {
    storeProjects.push(project('p2', 'demo-dir', '/p', 'dir'));
    const container = await openRegisterForm();
    setInputValue(pathInput(container), '/p');

    const alert = dupAlert(container);
    expect(alert).not.toBeNull();
    expect(alert!.textContent).toContain('「demo-dir」');
  });

  it('尾斜杠不命中：输入 /p/ 不额外归一化、不提醒', async () => {
    const container = await openRegisterForm();
    setInputValue(pathInput(container), '/p/');

    expect(dupAlert(container)).toBeNull();
  });
});
