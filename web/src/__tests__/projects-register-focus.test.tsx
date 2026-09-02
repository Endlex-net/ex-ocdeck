// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act } from 'react';
import type { Root } from 'react-dom/client';
import { ProjectsManagePage } from '../pages/ProjectsManagePage';
import { emitPaletteFocus, __resetPaletteFocusForTest } from '../palette-focus';
import { mount, flushUI, stubMatchMedia } from './cm-test-env';
import type { Project } from '../types';

let storeProjects: Project[] = [];

vi.mock('../hooks', () => ({
  useProjects: () => ({ projects: storeProjects, initialized: true, loading: false, error: '' }),
  useProjectsRefresh: () => vi.fn(async () => {}),
  runProjectMutation: async (fn: () => Promise<unknown>) => fn(),
  createErrorMessage: (_prefix: string, err: unknown) =>
    err instanceof Error ? err.message : String(err),
  deleteErrorMessage: (err: unknown) => (err instanceof Error ? err.message : String(err)),
}));

vi.mock('../api', () => ({
  api: {
    getProject: vi.fn(async () => ({ id: 'p1', name: 'demo', path: '/p', kind: 'repo', default_branch: 'main', created_at: 1, task_count: 0, tasks_by_status: {}, tasks: [] })),
    listTasks: vi.fn(async () => []),
    listBranches: vi.fn(async () => ['main']),
    refreshBranches: vi.fn(async () => ['main']),
    createProject: vi.fn(),
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

const roots: Root[] = [];

beforeEach(() => {
  __resetPaletteFocusForTest();
  stubMatchMedia(false);
  storeProjects = [
    {
      id: 'p1',
      name: 'demo',
      path: '/p',
      kind: 'repo',
      default_branch: 'main',
      created_at: 1,
      task_count: 0,
      tasks_by_status: {},
      tasks: [],
    },
  ];
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

function registerInput(container: HTMLElement) {
  return container.querySelector<HTMLInputElement>('[data-od-id="register-project-name"]');
}

async function flushFocus() {
  await flushUI();
  await act(async () => {
    await new Promise<void>((resolve) => {
      requestAnimationFrame(() => resolve());
    });
  });
}

describe('ProjectsManagePage register-project-name 消费', () => {
  it('pending 兜底：挂载后展开注册表单并聚焦名称输入', async () => {
    emitPaletteFocus('register-project-name');
    const { container } = renderPage();
    await flushFocus();
    const input = registerInput(container);
    expect(input).not.toBeNull();
    expect(container.querySelector('[aria-expanded="true"]')).not.toBeNull();
    expect(document.activeElement).toBe(input);
  });

  it('实时事件：已挂载页面收到信号后展开并聚焦', async () => {
    const { container } = renderPage();
    await flushFocus();
    expect(registerInput(container)).toBeNull();
    act(() => {
      emitPaletteFocus('register-project-name');
    });
    await flushFocus();
    const input = registerInput(container);
    expect(input).not.toBeNull();
    expect(container.querySelector('[aria-expanded="true"]')).not.toBeNull();
    expect(document.activeElement).toBe(input);
  });
});
