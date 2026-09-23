// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act } from 'react';
import { GitPanel } from '../components/GitPanel';
import { api } from '../api';
import type { GitBranchDiffFilesResult } from '../types';
import { mount, rerender, stubMatchMedia } from './cm-test-env';

/* ============================ git-page-enhancements 3.5：baseRef 装配 ============================
 * base 输入 draft/applied 分离（失焦或 Enter 生效、输入过程零请求）、生效前 trim、
 * 空值回落任务 base_ref、无 base_ref 零请求留白、prop 不覆盖手工值、
 * generation/seq 不进请求参数。 */

vi.mock('../api', () => ({
  api: {
    gitStatus: vi.fn(),
    gitDiff: vi.fn(),
    gitCommit: vi.fn(),
    gitPush: vi.fn(),
    gitBranchDiffFiles: vi.fn(),
    gitBranchDiffFile: vi.fn(),
    listAnnotations: vi.fn(async () => ({
      annotations: [],
      submitCapability: { state: 'supported', reason: '' },
    })),
    listSubmissions: vi.fn(async () => ({ queue: [], history: [], failures: [] })),
    gitFileRead: vi.fn(),
    gitFileWrite: vi.fn(),
    createAnnotation: vi.fn(),
    reportActivity: vi.fn(),
  },
  ApiError: class ApiError extends Error {
    constructor(
      public readonly status: number,
      public readonly code: string,
      message: string,
    ) {
      super(message);
    }
  },
}));

const gitStatusMock = vi.mocked(api.gitStatus);
const gitBranchDiffFilesMock = vi.mocked(api.gitBranchDiffFiles);

function branchFiles(paths: string[]): GitBranchDiffFilesResult {
  return {
    baseRef: '',
    files: paths.map((path) => ({ path, additions: 1, deletions: 0, isBinary: false })),
  };
}

async function until(cond: () => boolean, timeoutMs = 4000) {
  const start = Date.now();
  while (!cond()) {
    if (Date.now() - start > timeoutMs) throw new Error('condition timeout');
    // eslint-disable-next-line no-await-in-loop
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 10));
    });
  }
}

async function flush() {
  for (let i = 0; i < 6; i++) {
    await act(async () => {
      await Promise.resolve();
    });
  }
}

function clickTab(container: HTMLElement, label: string) {
  const tab = [...container.querySelectorAll<HTMLButtonElement>('.git-view-tab')].find(
    (b) => b.textContent === label,
  );
  expect(tab, `视图页签 ${label}`).toBeTruthy();
  act(() => tab!.click());
}

function baseInput(container: HTMLElement): HTMLInputElement {
  const el = container.querySelector<HTMLInputElement>('.git-base-ref input');
  expect(el, '基础 ref 输入框').toBeTruthy();
  return el!;
}

/** React 受控输入写入（prototype value setter 规避 value tracker 去重）。 */
function setInput(el: HTMLInputElement, value: string) {
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
  act(() => {
    setter.call(el, value);
    el.dispatchEvent(new Event('input', { bubbles: true }));
  });
}

function pressEnter(el: HTMLInputElement) {
  act(() => {
    el.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
  });
}

function blurInput(el: HTMLInputElement) {
  act(() => {
    el.dispatchEvent(new FocusEvent('focusout', { bubbles: true }));
  });
}

beforeEach(() => {
  vi.clearAllMocks();
  stubMatchMedia(false);
  gitStatusMock.mockResolvedValue({ branch: 'main', files: [] });
  gitBranchDiffFilesMock.mockResolvedValue(branchFiles(['b.ts']));
});

describe('3.5：分支对比 baseRef 装配', () => {
  it('baseRef prop 默认值：首次进入分支对比视图填入输入框并加载对比结果（仅一次请求）', async () => {
    const { container, unmount } = mount(<GitPanel taskID="t1" active baseRef="main" />);
    clickTab(container, '分支对比');
    await until(() => baseInput(container).value === 'main');
    await until(() => gitBranchDiffFilesMock.mock.calls.length === 1);
    expect(gitBranchDiffFilesMock).toHaveBeenCalledWith('t1', 'main');
    await until(() => container.textContent?.includes('b.ts') ?? false);
    await flush();
    expect(gitBranchDiffFilesMock.mock.calls).toHaveLength(1);
    unmount();
  });

  it('无 base_ref：留白显示「请输入基础 ref」且零请求；手填生效；置空回落零请求留白', async () => {
    const { container, unmount } = mount(<GitPanel taskID="t1" active />);
    clickTab(container, '分支对比');
    await until(() => container.textContent?.includes('请输入基础 ref') ?? false);
    expect(baseInput(container).value).toBe('');
    await flush();
    expect(gitBranchDiffFilesMock).not.toHaveBeenCalled();

    // 手填生效
    setInput(baseInput(container), 'dev');
    await flush();
    expect(gitBranchDiffFilesMock).not.toHaveBeenCalled(); // 输入过程零请求
    pressEnter(baseInput(container));
    await until(() => gitBranchDiffFilesMock.mock.calls.length === 1);
    expect(gitBranchDiffFilesMock).toHaveBeenCalledWith('t1', 'dev');

    // 清空 + Enter：applied 置空 → 留白，不发起请求，在途旧响应失效
    setInput(baseInput(container), '');
    pressEnter(baseInput(container));
    await until(() => container.textContent?.includes('请输入基础 ref') ?? false);
    await flush();
    expect(gitBranchDiffFilesMock.mock.calls).toHaveLength(1);
    unmount();
  });

  it('生效时机与 trim：Enter 生效；失焦生效；带空白输入 trim 后生效；空输入回落任务 base_ref', async () => {
    const { container, unmount } = mount(<GitPanel taskID="t1" active baseRef="main" />);
    clickTab(container, '分支对比');
    await until(() => gitBranchDiffFilesMock.mock.calls.length === 1);

    // 输入过程零请求，Enter 生效（trim）
    setInput(baseInput(container), '  dev  ');
    await flush();
    expect(gitBranchDiffFilesMock.mock.calls).toHaveLength(1);
    pressEnter(baseInput(container));
    await until(() => gitBranchDiffFilesMock.mock.calls.length === 2);
    expect(gitBranchDiffFilesMock).toHaveBeenLastCalledWith('t1', 'dev');
    expect(baseInput(container).value).toBe('dev');

    // 失焦生效：空白输入 trim 为空 → 回落任务 base_ref 重载
    setInput(baseInput(container), '   ');
    blurInput(baseInput(container));
    await until(() => gitBranchDiffFilesMock.mock.calls.length === 3);
    expect(gitBranchDiffFilesMock).toHaveBeenLastCalledWith('t1', 'main');
    expect(baseInput(container).value).toBe('main');

    // 无变化失焦：不重复请求
    blurInput(baseInput(container));
    await flush();
    expect(gitBranchDiffFilesMock.mock.calls).toHaveLength(3);
    unmount();
  });

  it('同值应用不重复请求', async () => {
    const { container, unmount } = mount(<GitPanel taskID="t1" active baseRef="main" />);
    clickTab(container, '分支对比');
    await until(() => gitBranchDiffFilesMock.mock.calls.length === 1);
    setInput(baseInput(container), 'main');
    pressEnter(baseInput(container));
    await flush();
    expect(gitBranchDiffFilesMock.mock.calls).toHaveLength(1);
    unmount();
  });

  it('baseRef prop 后续变化：!manual 时同步并按新值加载；manual 手改后 prop 不覆盖', async () => {
    const { container, root, unmount } = mount(<GitPanel taskID="t1" active />);
    clickTab(container, '分支对比');
    await until(() => container.textContent?.includes('请输入基础 ref') ?? false);

    // 任务详情到达：prop '' → 'origin/main'，非 manual → 输入同步并加载
    rerender(root, <GitPanel taskID="t1" active baseRef="origin/main" />);
    await until(() => baseInput(container).value === 'origin/main');
    await until(() => gitBranchDiffFilesMock.mock.calls.length === 1);
    expect(gitBranchDiffFilesMock).toHaveBeenCalledWith('t1', 'origin/main');

    // 手改生效
    setInput(baseInput(container), 'dev');
    pressEnter(baseInput(container));
    await until(() => gitBranchDiffFilesMock.mock.calls.length === 2);

    // prop 再变（manual 已置位）：不覆盖 draft/applied、不发请求
    rerender(root, <GitPanel taskID="t1" active baseRef="develop" />);
    await flush();
    expect(baseInput(container).value).toBe('dev');
    expect(gitBranchDiffFilesMock.mock.calls).toHaveLength(2);
    unmount();
  });

  it('generation/requestSeq 不序列化进请求参数：列表 2 参、文件 3 参', async () => {
    const gitBranchDiffFileMock = vi.mocked(api.gitBranchDiffFile);
    gitBranchDiffFileMock.mockResolvedValue({
      oldContent: '',
      newContent: '',
      oldExists: false,
      newExists: true,
      oldMode: '',
      newMode: '100644',
      isBinary: false,
      truncated: false,
    });
    const { container, unmount } = mount(<GitPanel taskID="t1" active baseRef="main" />);
    clickTab(container, '分支对比');
    await until(() => container.textContent?.includes('b.ts') ?? false);
    // 触发多次 base 变更（generation 递增）后再拉文件
    setInput(baseInput(container), 'dev');
    pressEnter(baseInput(container));
    await until(() => container.textContent?.includes('b.ts') ?? false);
    const fileEl = [...container.querySelectorAll<HTMLElement>('.git-file-path')].find(
      (n) => n.textContent === 'b.ts',
    )!;
    act(() => fileEl.click());
    await until(() => gitBranchDiffFileMock.mock.calls.length === 1);
    await flush();
    expect(gitBranchDiffFileMock.mock.calls[0]).toEqual(['t1', 'dev', 'b.ts']);
    for (const call of gitBranchDiffFilesMock.mock.calls) expect(call).toHaveLength(2);
    unmount();
  });
});
