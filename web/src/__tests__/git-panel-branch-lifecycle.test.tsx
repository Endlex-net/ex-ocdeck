// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act } from 'react';
import { EditorView } from '@codemirror/view';
import { GitPanel, buildFileTree, compareBytewise, listFileRows } from '../components/GitPanel';
import { api, ApiError } from '../api';
import type { Annotation, GitBranchDiffFilesResult, GitDiffResult, GitFileEntry, GitStatus } from '../types';
import { flushUI, mount, stubMatchMedia } from './cm-test-env';

/* ============================ git-page-enhancements 3.6/3.7 + 6.3/6.4/6.5 ============================
 * 序号模型（组件级全局 requestSeq + latest 游标 + generation，作用于已提交请求）：同代乱序丢弃、
 * stale 失败不覆盖当前状态、失败手动重试（无自动重试）、列表重载清消失选中；
 * 混合单列表（已提交+未提交，固定次序）；按条目类型门控（已提交只读/未提交完整能力）；
 * 未提交状态跨视图与 base 变更共享保持；leave guard 拒绝停留；refresh/push 动作表。 */

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
const gitDiffMock = vi.mocked(api.gitDiff);
const gitPushMock = vi.mocked(api.gitPush);
const gitCommitMock = vi.mocked(api.gitCommit);
const gitBranchDiffFilesMock = vi.mocked(api.gitBranchDiffFiles);
const gitBranchDiffFileMock = vi.mocked(api.gitBranchDiffFile);
const gitFileReadMock = vi.mocked(api.gitFileRead);
const gitFileWriteMock = vi.mocked(api.gitFileWrite);

const entryA: GitFileEntry = {
  path: 'a.ts',
  x: 'M',
  y: ' ',
  staged: false,
  unstaged: true,
  untracked: false,
  additions: 1,
  deletions: 0,
  isBinary: false,
};

const entryB: GitFileEntry = { ...entryA, path: 'b.ts' };

const f = (path: string) => ({ path, additions: 1, deletions: 0, isBinary: false });

function branchFilesOf(...paths: string[]): GitBranchDiffFilesResult {
  return { baseRef: '', files: paths.map(f) };
}

function diffOf(x: string): GitDiffResult {
  return {
    oldContent: `${x}-old\n`,
    newContent: `${x}-new\n`,
    oldExists: true,
    newExists: true,
    oldMode: '100644',
    newMode: '100644',
    isBinary: false,
    truncated: false,
  };
}

const mkAnn = (id: string, path: string, ref: string, untracked: boolean): Annotation => ({
  id,
  path,
  side: 'new',
  ref,
  untracked,
  startLine: 1,
  endLine: 1,
  snapshotStartLine: 1,
  snapshotLineCount: 1,
  snapshot: 'x\n',
  comment: `批注-${id}`,
  revision: 1,
  stale: false,
  createdAt: 0,
  updatedAt: 0,
});

/** 手动控制的 deferred Promise（乱序完成测试用）。 */
function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: Error) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
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

function clickButton(container: HTMLElement, text: string) {
  const btn = [...container.querySelectorAll<HTMLButtonElement>('button')].find(
    (b) => b.textContent === text,
  );
  expect(btn, `按钮 ${text}`).toBeTruthy();
  act(() => btn!.click());
}

function clickFile(container: HTMLElement, path: string) {
  const el = [...container.querySelectorAll<HTMLElement>('.git-file-path')].find(
    (n) => n.textContent === path,
  );
  expect(el, `文件 ${path} 在列表中`).toBeTruthy();
  act(() => el!.click());
}

/** 点击指定类型的条目行（同路径已提交/未提交并存时按状态类区分）。 */
function clickRow(container: HTMLElement, statusClass: string, path: string) {
  const row = [...container.querySelectorAll<HTMLElement>('.git-file')].find(
    (n) =>
      n.querySelector(`.git-file-path.${statusClass}`) !== null &&
      n.querySelector('.git-file-path')?.textContent === path,
  );
  expect(row, `条目 ${statusClass} ${path} 在列表中`).toBeTruthy();
  act(() => (row!.querySelector('.git-file-path') as HTMLElement).click());
}

function rowPaths(container: HTMLElement): string[] {
  return [...container.querySelectorAll('.git-files .git-file-path')].map(
    (n) => n.textContent ?? '',
  );
}

function pathClasses(container: HTMLElement): string[] {
  return [...container.querySelectorAll('.git-files .git-file-path')].map((n) => n.className);
}

function headerText(container: HTMLElement): string {
  // 可见块的 header（W5 后 branch 视图可能同时挂载 hidden 的另一类块）
  const el = [...container.querySelectorAll('.git-diff-header')].find(
    (n) => n.closest('[hidden]') === null,
  );
  return el?.textContent ?? '';
}

function visibleDiffText(container: HTMLElement): string {
  return [...container.querySelectorAll('.git-diff > div')]
    .filter((n) => !(n as HTMLElement).hidden)
    .map((n) => n.textContent)
    .join('');
}

function hiddenDiffText(container: HTMLElement): string {
  return [...container.querySelectorAll('.git-diff > div')]
    .filter((n) => (n as HTMLElement).hidden)
    .map((n) => n.textContent)
    .join('');
}

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

function baseInput(container: HTMLElement): HTMLInputElement {
  return container.querySelector<HTMLInputElement>('.git-base-ref input')!;
}

const activeTabOf = (container: HTMLElement) =>
  container.querySelector('.git-view-tab-active')?.textContent ?? '';

/** 点击刷新并等待本次 status 请求完成。 */
async function clickRefreshAndWait(container: HTMLElement) {
  const calls = gitStatusMock.mock.calls.length;
  clickButton(container, '刷新');
  await until(() => gitStatusMock.mock.calls.length > calls);
  await flush();
}

/** 挂载并进入分支对比视图（默认 baseRef=main，列表默认解析 b.txt；beforeEach 的 status 含未提交 a.ts）。 */
async function mountPanel() {
  const ui = mount(<GitPanel taskID="t1" active baseRef="main" />);
  clickTab(ui.container, '分支对比');
  await until(() => ui.container.querySelector('.git-base-ref input') !== null);
  return ui;
}

function editorViewOf(container: HTMLElement): EditorView | null {
  for (const el of container.querySelectorAll('.cm-content')) {
    const v = EditorView.findFromDOM(el as HTMLElement);
    if (v) return v;
  }
  return null;
}

/** 打开文件 diff 并进入编辑模式（uncommitted 视图）。 */
async function enterEditMode(container: HTMLElement) {
  await until(() => container.querySelector('.git-file-path') !== null);
  act(() => {
    (container.querySelector('.git-file-path') as HTMLElement).click();
  });
  await until(() => container.querySelector('.cm-editor') !== null);
  act(() => {
    [...container.querySelectorAll<HTMLButtonElement>('button')]
      .find((b) => b.textContent === '单列')!
      .click();
  });
  await until(() => container.querySelectorAll('.cm-editor').length === 1);
  await until(
    () =>
      [...container.querySelectorAll<HTMLButtonElement>('button')].some(
        (b) => b.textContent === '编辑' && !b.disabled,
      ),
  );
  act(() => {
    [...container.querySelectorAll<HTMLButtonElement>('button')]
      .find((b) => b.textContent === '编辑')!
      .click();
  });
  await until(
    () =>
      [...container.querySelectorAll('.cm-content')].some(
        (el) => el.getAttribute('contenteditable') === 'true',
      ),
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  stubMatchMedia(false);
  gitStatusMock.mockResolvedValue({ branch: 'main', files: [entryA] });
  gitDiffMock.mockResolvedValue(diffOf('a'));
  gitPushMock.mockResolvedValue();
  gitBranchDiffFilesMock.mockResolvedValue(branchFilesOf('b.txt'));
  gitBranchDiffFileMock.mockResolvedValue(diffOf('b'));
  gitFileReadMock.mockResolvedValue({
    editable: true,
    content: 'a\nb\n',
    baseHash: 'h0',
    lineEnding: 'lf',
    hasBom: false,
    mode: '0644',
  });
  gitFileWriteMock.mockResolvedValue({ baseHash: 'h1' });
});

describe('3.6：序号模型（同代乱序 / stale 失败 / 手动重试，作用于已提交请求）', () => {
  it('base A→B：在途旧列表响应（generation 失效）一律丢弃，新值响应正常写入', async () => {
    const pMain = deferred<GitBranchDiffFilesResult>();
    const pDev = deferred<GitBranchDiffFilesResult>();
    gitBranchDiffFilesMock.mockImplementation((_t, base) =>
      base === 'main' ? pMain.promise : pDev.promise,
    );
    const { container, unmount } = await mountPanel();
    expect(gitBranchDiffFilesMock).toHaveBeenCalledWith('t1', 'main');
    setInput(baseInput(container), 'dev');
    pressEnter(baseInput(container));
    await until(
      () => gitBranchDiffFilesMock.mock.calls[gitBranchDiffFilesMock.mock.calls.length - 1]?.[1] === 'dev',
    );
    // 旧 base 响应晚到成功：丢弃
    await act(async () => {
      pMain.resolve(branchFilesOf('old.txt'));
    });
    await flush();
    expect(container.textContent).not.toContain('old.txt');
    // 新 base 响应到达：写入
    await act(async () => {
      pDev.resolve(branchFilesOf('new.txt'));
    });
    await until(() => container.textContent?.includes('new.txt') ?? false);
    expect(container.textContent).not.toContain('old.txt');
    expect(container.querySelector('.git-files-stale')).toBeNull();
    unmount();
  });

  it('快速切文件 A→B：晚到旧文件响应不覆盖最新 diff（成功不覆盖）', async () => {
    gitBranchDiffFilesMock.mockResolvedValue(branchFilesOf('a.txt', 'b.txt'));
    const dA = deferred<GitDiffResult>();
    const dB = deferred<GitDiffResult>();
    gitBranchDiffFileMock.mockImplementation((_t, _b, path) =>
      path === 'a.txt' ? dA.promise : dB.promise,
    );
    const { container, unmount } = await mountPanel();
    await until(() => rowPaths(container).includes('b.txt'));
    clickFile(container, 'a.txt');
    clickFile(container, 'b.txt');
    await act(async () => {
      dB.resolve(diffOf('b'));
    });
    await until(() => container.textContent?.includes('b-old') ?? false);
    // a 晚到成功：不覆盖 b
    await act(async () => {
      dA.resolve(diffOf('a'));
    });
    await flush();
    expect(container.textContent).toContain('b-old');
    expect(container.textContent).not.toContain('a-old');
    expect(headerText(container)).toContain('b.txt');
    unmount();
  });

  it('快速切文件 A→B：晚到旧文件失败不写错误、不动当前视图', async () => {
    gitBranchDiffFilesMock.mockResolvedValue(branchFilesOf('a.txt', 'b.txt'));
    const dA = deferred<GitDiffResult>();
    const dB = deferred<GitDiffResult>();
    gitBranchDiffFileMock.mockImplementation((_t, _b, path) =>
      path === 'a.txt' ? dA.promise : dB.promise,
    );
    const { container, unmount } = await mountPanel();
    await until(() => rowPaths(container).includes('b.txt'));
    clickFile(container, 'a.txt');
    clickFile(container, 'b.txt');
    await act(async () => {
      dB.resolve(diffOf('b'));
    });
    await until(() => container.textContent?.includes('b-old') ?? false);
    await act(async () => {
      dA.reject(new Error('late failure'));
    });
    await flush();
    expect(container.querySelector('.git-error')).toBeNull();
    expect(container.textContent).toContain('b-old');
    unmount();
  });

  it('刷新在途 + base 切换：旧序号/旧 generation 的列表响应（成功或失败）不覆盖新状态、不置 stale', async () => {
    // UI 上刷新按钮在加载中 disabled，同代并发列表请求不可达；可达路径为刷新在途时 base 变更
    gitBranchDiffFilesMock
      .mockResolvedValueOnce(branchFilesOf('v1.txt'))
      .mockImplementationOnce(() => d1.promise)
      .mockImplementationOnce(() => d2.promise);
    const d1 = deferred<GitBranchDiffFilesResult>();
    const d2 = deferred<GitBranchDiffFilesResult>();
    const { container, unmount } = await mountPanel();
    await until(() => rowPaths(container).includes('v1.txt'));
    clickButton(container, '刷新');
    // 刷新在途时切 base：generation++ 使旧响应失效，新值请求分配新序号
    setInput(baseInput(container), 'dev');
    pressEnter(baseInput(container));
    await until(
      () => gitBranchDiffFilesMock.mock.calls[gitBranchDiffFilesMock.mock.calls.length - 1]?.[1] === 'dev',
    );
    await act(async () => {
      d2.resolve(branchFilesOf('v2.txt'));
    });
    await until(() => rowPaths(container).includes('v2.txt'));
    // 刷新响应晚到失败：丢弃，不置 stale、不清列表
    await act(async () => {
      d1.reject(new Error('stale boom'));
    });
    await flush();
    expect(rowPaths(container)).toContain('v2.txt');
    expect(rowPaths(container)).not.toContain('v1.txt');
    expect(container.querySelector('.git-files-stale')).toBeNull();
    expect(container.querySelector('.git-error')).toBeNull();
    unmount();
  });

  it('列表失败：stale + 手动重载入口，无自动重试；重载成功恢复', async () => {
    gitBranchDiffFilesMock
      .mockRejectedValueOnce(new ApiError(500, 'internal', 'boom'))
      .mockResolvedValueOnce(branchFilesOf('ok.txt'));
    const { container, unmount } = await mountPanel();
    await until(() => container.textContent?.includes('boom') ?? false);
    expect(container.querySelector('.git-files-stale')).not.toBeNull();
    await flushUI();
    expect(gitBranchDiffFilesMock.mock.calls).toHaveLength(1); // 无自动重试
    clickButton(container, '重载');
    await until(() => rowPaths(container).includes('ok.txt'));
    expect(container.querySelector('.git-files-stale')).toBeNull();
    unmount();
  });

  it('文件 diff 失败：stale + 手动重试入口，无自动重试；重试成功渲染', async () => {
    gitBranchDiffFilesMock.mockResolvedValue(branchFilesOf('b.txt'));
    gitBranchDiffFileMock
      .mockRejectedValueOnce(new ApiError(404, 'not_found', 'nope'))
      .mockResolvedValueOnce(diffOf('b'));
    const { container, unmount } = await mountPanel();
    await until(() => rowPaths(container).includes('b.txt'));
    clickFile(container, 'b.txt');
    await until(() => container.textContent?.includes('nope') ?? false);
    expect(headerText(container)).toContain('b.txt');
    await flushUI();
    expect(gitBranchDiffFileMock.mock.calls).toHaveLength(1); // 无自动重试
    clickButton(container, '重试');
    await until(() => container.textContent?.includes('b-old') ?? false);
    expect(container.querySelector('.git-error')).toBeNull();
    unmount();
  });

  it('列表重载后已选已提交路径消失：清空其选中与 diff，不报错', async () => {
    gitBranchDiffFilesMock
      .mockResolvedValueOnce(branchFilesOf('a.txt', 'b.txt'))
      .mockResolvedValueOnce(branchFilesOf('a.txt'));
    const { container, unmount } = await mountPanel();
    await until(() => rowPaths(container).includes('b.txt'));
    clickFile(container, 'b.txt');
    await until(() => headerText(container).includes('b.txt'));
    clickButton(container, '刷新');
    await until(() => headerText(container) === '');
    expect(rowPaths(container)).toContain('a.txt');
    expect(rowPaths(container)).not.toContain('b.txt');
    expect(container.querySelector('.git-error')).toBeNull();
    expect(container.querySelector('.git-files-stale')).toBeNull();
    unmount();
  });

  it('同路径已提交/未提交交错选择：旧已提交响应不覆盖未提交 diff（tasks 6.5 全身份匹配）', async () => {
    gitStatusMock.mockResolvedValue({
      branch: 'main',
      files: [
        { path: 'x.ts', x: 'M', y: 'M', staged: true, unstaged: false, untracked: false, additions: 2, deletions: 1, isBinary: false },
      ],
    });
    gitBranchDiffFilesMock.mockResolvedValue(branchFilesOf('x.ts'));
    // 两次点击各用独立 deferred（W8）：复用同一已 resolved promise 会让第一次响应被
    // 错误丢弃时测试仍通过，无法证明「第一次响应已写入 hidden 已提交块」
    const d1 = deferred<GitDiffResult>();
    const d2 = deferred<GitDiffResult>();
    gitBranchDiffFileMock.mockReturnValueOnce(d1.promise).mockReturnValueOnce(d2.promise);
    gitDiffMock.mockResolvedValue(diffOf('u'));
    const { container, unmount } = await mountPanel();
    await until(() => rowPaths(container).includes('x.ts'));
    // 先点已提交 x.ts（请求 1 在途）
    clickRow(container, 'git-file-committed', 'x.ts');
    await until(() => gitBranchDiffFileMock.mock.calls.length === 1);
    expect(headerText(container)).toContain('x.ts');
    // 再点未提交（已暂存）x.ts：焦点切到未提交，u-old 写入
    clickRow(container, 'git-file-staged', 'x.ts');
    await until(() => visibleDiffText(container).includes('u-old'));
    expect(gitDiffMock).toHaveBeenCalledWith('t1', 'HEAD', 'x.ts', false);
    // 请求 1 晚到成功，但当前焦点是未提交块：响应丢弃（design D8:142，Y2）——
    // 不写入隐藏块、可见区不被覆盖、无错误、不自动重发
    await act(async () => {
      d1.resolve(diffOf('c'));
    });
    await flush();
    expect(visibleDiffText(container)).toContain('u-old');
    expect(visibleDiffText(container)).not.toContain('c-old');
    expect(headerText(container)).toContain('HEAD');
    expect(hiddenDiffText(container)).not.toContain('c-old');
    expect(gitBranchDiffFileMock.mock.calls).toHaveLength(1);
    // 同路径两类条目不同时高亮：焦点在未提交块时 committed 行不 active
    expect(
      [...container.querySelectorAll('.git-file')].some(
        (n) =>
          n.querySelector('.git-file-committed') !== null &&
          n.className.includes('git-file-active'),
      ),
    ).toBe(false);
    // 切回已提交焦点：重新点击发起新请求，写入可见区
    clickRow(container, 'git-file-committed', 'x.ts');
    await until(() => gitBranchDiffFileMock.mock.calls.length === 2);
    await act(async () => {
      d2.resolve(diffOf('c'));
    });
    await until(() => visibleDiffText(container).includes('c-old'));
    // 焦点在已提交块：未提交行不 active
    expect(
      [...container.querySelectorAll('.git-file')].some(
        (n) =>
          n.querySelector('.git-file-staged') !== null &&
          n.className.includes('git-file-active'),
      ),
    ).toBe(false);
    expect(container.querySelector('.git-error')).toBeNull();
    unmount();
  });

  it('branch 内焦点切换：未提交编辑会话保活不 dispose（W5）', async () => {
    const { container, unmount } = await mountPanel();
    await until(() => rowPaths(container).includes('b.txt'));
    // 打开未提交条目并进入编辑
    clickFile(container, 'a.ts');
    await until(() => container.querySelector('.cm-editor') !== null);
    act(() => {
      [...container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '单列')!
        .click();
    });
    await until(() => container.querySelectorAll('.cm-editor').length === 1);
    await until(
      () =>
        [...container.querySelectorAll<HTMLButtonElement>('button')].some(
          (b) => b.textContent === '编辑' && !b.disabled,
        ),
    );
    act(() => {
      [...container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '编辑')!
        .click();
    });
    await until(
      () =>
        [...container.querySelectorAll('.cm-content')].some(
          (el) => el.getAttribute('contenteditable') === 'true',
        ),
    );
    const viewBefore = editorViewOf(container)!;
    // 未保存修改（不等待 debounce 写回）
    act(() => {
      viewBefore.dispatch({
        changes: { from: viewBefore.state.doc.length, insert: 'LOCAL_EDIT\n' },
      });
    });
    // 焦点切到已提交条目
    clickFile(container, 'b.txt');
    await until(() => headerText(container).includes('b.txt'));
    // 未提交编辑器仍挂载（hidden）：DOM、编辑器实例、未保存文本全部保留
    const hiddenEditable = container.querySelector('.cm-content[contenteditable="true"]');
    expect(hiddenEditable, '未提交编辑器 DOM 保留').not.toBeNull();
    const viewHidden = EditorView.findFromDOM(hiddenEditable as HTMLElement)!;
    expect(viewHidden).toBe(viewBefore);
    expect(viewHidden.state.doc.toString()).toContain('LOCAL_EDIT');
    // 切回未提交焦点：同一实例继续编辑
    clickFile(container, 'a.ts');
    await until(() => headerText(container).includes('a.ts'));
    const viewAfter = editorViewOf(container)!;
    expect(viewAfter).toBe(viewBefore);
    expect(viewAfter.state.doc.toString()).toContain('LOCAL_EDIT');
    unmount();
  });

  it('branch 内刷新不切焦点（W6）：查看未提交条目时刷新重拉已提交列表/diff 与 status，但保持当前显示', async () => {
    const { container, unmount } = await mountPanel();
    await until(() => rowPaths(container).includes('b.txt'));
    clickFile(container, 'b.txt');
    await until(() => headerText(container).includes('b.txt'));
    clickFile(container, 'a.ts');
    await until(() => headerText(container).includes('a.ts'));
    const statusCalls = gitStatusMock.mock.calls.length;
    const listCalls = gitBranchDiffFilesMock.mock.calls.length;
    const fileCalls = gitBranchDiffFileMock.mock.calls.length;
    clickButton(container, '刷新');
    await until(() => gitBranchDiffFilesMock.mock.calls.length === listCalls + 1);
    await until(() => gitBranchDiffFileMock.mock.calls.length === fileCalls + 1);
    await until(() => gitStatusMock.mock.calls.length === statusCalls + 1);
    await flush();
    // 焦点保持未提交块：header 与内容不变
    expect(headerText(container)).toContain('a.ts');
    expect(visibleDiffText(container)).toContain('a-old');
    unmount();
  });

  it('status 消失清除与新 openDiff 选择交错：基于最新选择清除且引用不残留（X5）', async () => {
    // 首拉列表含 a.ts、b.ts
    gitStatusMock.mockResolvedValueOnce({ branch: 'main', files: [entryA, entryB] });
    const { container, unmount } = mount(<GitPanel taskID="t1" active baseRef="main" />);
    await until(() => rowPaths(container).includes('a.ts'));
    // 打开 a.ts
    clickFile(container, 'a.ts');
    await until(() => headerText(container).includes('a.ts'));
    // 第二次刷新挂起；挂起期间点 b.ts（openDiff 以 imperative 方式更新最新选择）
    const dStatus = deferred<GitStatus>();
    gitStatusMock.mockImplementation(() => dStatus.promise);
    clickButton(container, '刷新');
    await until(() => gitStatusMock.mock.calls.length === 2);
    gitDiffMock.mockResolvedValue(diffOf('b'));
    clickFile(container, 'b.ts');
    await until(() => headerText(container).includes('b.ts'));
    await until(() => visibleDiffText(container).includes('b-old'));
    // 放行 status v2=[]：消失检查必须基于最新选择（b.ts）清除 state 与引用
    await act(async () => {
      dStatus.resolve({ branch: 'main', files: [] });
    });
    await until(() => headerText(container) === '');
    expect(visibleDiffText(container)).not.toContain('b-old');
    expect(container.querySelector('.git-error')).toBeNull();
    // 交错后再次刷新：幂等无副作用（不因残留引用重复清除/报错）
    await clickRefreshAndWait(container);
    expect(headerText(container)).toBe('');
    expect(container.querySelector('.git-error')).toBeNull();
    unmount();
  });

  it('guard 拒绝时点击已提交条目：焦点与可见编辑器保持未提交块（W7 正向）', async () => {
    gitFileWriteMock.mockRejectedValue(new ApiError(409, 'conflict', 'hash 不一致'));
    const { container, unmount } = await mountPanel();
    await until(() => rowPaths(container).includes('b.txt'));
    // 未提交条目进入编辑并制造写回冲突
    clickFile(container, 'a.ts');
    await until(() => container.querySelector('.cm-editor') !== null);
    act(() => {
      [...container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '单列')!
        .click();
    });
    await until(() => container.querySelectorAll('.cm-editor').length === 1);
    await until(
      () =>
        [...container.querySelectorAll<HTMLButtonElement>('button')].some(
          (b) => b.textContent === '编辑' && !b.disabled,
        ),
    );
    act(() => {
      [...container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '编辑')!
        .click();
    });
    await until(
      () =>
        [...container.querySelectorAll('.cm-content')].some(
          (el) => el.getAttribute('contenteditable') === 'true',
        ),
    );
    const editorView = editorViewOf(container)!;
    act(() => {
      editorView.dispatch({ changes: { from: editorView.state.doc.length, insert: 'c\n' } });
    });
    await until(() => container.textContent?.includes('保存冲突') ?? false, 6000);
    // 点击已提交条目：guard 拒绝 → 焦点/可见块保持未提交编辑
    clickFile(container, 'b.txt');
    await flushUI();
    expect(headerText(container)).toContain('a.ts');
    const visibleEditable = [
      ...container.querySelectorAll('.cm-content[contenteditable="true"]'),
    ].find((el) => el.closest('[hidden]') === null);
    expect(visibleEditable, '未提交编辑器仍可见').toBeTruthy();
    expect(gitBranchDiffFileMock).not.toHaveBeenCalled();
    unmount();
  });

  it('guard 拒绝时点击未提交条目：焦点保持已提交块（W7 反向）', async () => {
    const { container, unmount } = await mountPanel();
    await until(() => rowPaths(container).includes('b.txt'));
    // 未提交条目进入编辑，写回成功后切到已提交焦点
    clickFile(container, 'a.ts');
    await until(() => container.querySelector('.cm-editor') !== null);
    act(() => {
      [...container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '单列')!
        .click();
    });
    await until(() => container.querySelectorAll('.cm-editor').length === 1);
    await until(
      () =>
        [...container.querySelectorAll<HTMLButtonElement>('button')].some(
          (b) => b.textContent === '编辑' && !b.disabled,
        ),
    );
    act(() => {
      [...container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '编辑')!
        .click();
    });
    await until(
      () =>
        [...container.querySelectorAll('.cm-content')].some(
          (el) => el.getAttribute('contenteditable') === 'true',
        ),
    );
    const editorView = editorViewOf(container)!;
    act(() => {
      editorView.dispatch({ changes: { from: editorView.state.doc.length, insert: 'c\n' } });
    });
    await until(() => gitFileWriteMock.mock.calls.length === 1); // 写回完成（clean）
    clickFile(container, 'b.txt');
    await until(() => headerText(container).includes('b.txt'));
    await until(() => visibleDiffText(container).includes('b-old'));
    // hidden 编辑会话继续修改 → 写回 409 → 阻塞
    gitFileWriteMock.mockRejectedValue(new ApiError(409, 'conflict', 'hash 不一致'));
    act(() => {
      editorView.dispatch({ changes: { from: editorView.state.doc.length, insert: 'd\n' } });
    });
    await until(() => container.textContent?.includes('保存冲突') ?? false, 6000);
    // 点击未提交条目：guard 拒绝 → 焦点保持已提交块
    clickFile(container, 'a.ts');
    await flushUI();
    expect(headerText(container)).toContain('b.txt');
    expect(visibleDiffText(container)).toContain('b-old');
    expect(gitDiffMock).toHaveBeenCalledTimes(1); // 打开 a.ts 的那一次，无新请求
    unmount();
  });

  it('guard 等待期间列表响应移除当前 path：不复活选中、不发请求（X4）', async () => {
    const dWrite = deferred<{ baseHash: string }>();
    const dList = deferred<GitBranchDiffFilesResult>();
    // 首拉列表含 b.txt；窗口内的刷新响应将其移除
    gitBranchDiffFilesMock
      .mockResolvedValueOnce(branchFilesOf('b.txt'))
      .mockImplementationOnce(() => dList.promise);
    const { container, unmount } = await mountPanel();
    await until(() => rowPaths(container).includes('b.txt'));
    // 未提交条目进入编辑
    clickFile(container, 'a.ts');
    await until(() => container.querySelector('.cm-editor') !== null);
    act(() => {
      [...container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '单列')!
        .click();
    });
    await until(() => container.querySelectorAll('.cm-editor').length === 1);
    await until(
      () =>
        [...container.querySelectorAll<HTMLButtonElement>('button')].some(
          (b) => b.textContent === '编辑' && !b.disabled,
        ),
    );
    act(() => {
      [...container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '编辑')!
        .click();
    });
    await until(
      () =>
        [...container.querySelectorAll('.cm-content')].some(
          (el) => el.getAttribute('contenteditable') === 'true',
        ),
    );
    const editorView = editorViewOf(container)!;
    // 在途写挂起：点击已提交条目的 guard flush 等待它（竞态窗口开启）
    gitFileWriteMock.mockImplementation(() => dWrite.promise);
    act(() => {
      editorView.dispatch({ changes: { from: editorView.state.doc.length, insert: 'c\n' } });
    });
    clickFile(container, 'b.txt');
    await until(() => gitFileWriteMock.mock.calls.length === 1);
    // 窗口内刷新列表：响应移除 b.txt
    clickButton(container, '刷新');
    await until(() => gitBranchDiffFilesMock.mock.calls.length === 2);
    // 不用 act 包裹两个 resolve（复现真实竞速）：列表续体先执行（同步 ref / 清选中），
    // guard 续体紧随——渲染提交晚于 guard 续体时 branchFilesRef 是否滞后即见分晓
    dList.resolve(branchFilesOf('a2.txt'));
    dWrite.resolve({ baseHash: 'h1' });
    await flushUI();
    expect(gitBranchDiffFileMock).not.toHaveBeenCalled();
    expect(headerText(container)).toContain('a.ts');
    unmount();
  });

  it('文件请求在途时列表重载移除该 path：选中/diff/error/stale/loading 全清，在途响应被丢弃（Z1）', async () => {
    const dList = deferred<GitBranchDiffFilesResult>();
    const dFile = deferred<GitDiffResult>();
    gitBranchDiffFilesMock
      .mockResolvedValueOnce(branchFilesOf('b.txt')) // 首拉
      .mockImplementationOnce(() => dList.promise); // 刷新：移除 b.txt
    gitBranchDiffFileMock
      .mockRejectedValueOnce(new ApiError(500, 'internal', 'boom')) // 首次点击失败：error+stale
      .mockImplementationOnce(() => dFile.promise); // 刷新触发的选中重开（W6）在途
    const { container, unmount } = await mountPanel();
    await until(() => rowPaths(container).includes('b.txt'));
    clickFile(container, 'b.txt');
    await until(() => container.textContent?.includes('boom') ?? false);
    expect(headerText(container)).toContain('b.txt');
    // 刷新：重拉列表（deferred）+ 重开选中已提交 diff（W6，deferred 在途）→ error/stale 清、loading 置 true
    clickButton(container, '刷新');
    await until(() => gitBranchDiffFilesMock.mock.calls.length === 2);
    await until(() => gitBranchDiffFileMock.mock.calls.length === 2);
    await until(() => container.textContent?.includes('加载 diff…') ?? false);
    // 列表响应移除 b.txt：消失分支须清齐选中/diff/error/stale/loading（Z1）
    await act(async () => {
      dList.resolve(branchFilesOf('a2.txt'));
    });
    await until(() => rowPaths(container).includes('a2.txt'));
    expect(headerText(container)).toBe('');
    expect(container.querySelector('.git-error')).toBeNull();
    expect(container.querySelector('.git-files-stale')).toBeNull();
    expect(container.textContent).not.toContain('加载 diff…');
    // 放行在途文件响应：选中已清 → 丢弃，不复活 diff、不报错、不重挂 loading
    await act(async () => {
      dFile.resolve(diffOf('b'));
    });
    await flush();
    expect(container.textContent).not.toContain('b-old');
    expect(headerText(container)).toBe('');
    expect(container.querySelector('.git-error')).toBeNull();
    expect(container.querySelector('.git-files-stale')).toBeNull();
    expect(container.textContent).not.toContain('加载 diff…');
    unmount();
  });

  it('guard 等待期间 base 变更：恢复后旧 base 的请求意图失效（X1）', async () => {
    const dWrite = deferred<{ baseHash: string }>();
    const { container, unmount } = await mountPanel();
    await until(() => rowPaths(container).includes('b.txt'));
    // 未提交条目进入编辑
    clickFile(container, 'a.ts');
    await until(() => container.querySelector('.cm-editor') !== null);
    act(() => {
      [...container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '单列')!
        .click();
    });
    await until(() => container.querySelectorAll('.cm-editor').length === 1);
    await until(
      () =>
        [...container.querySelectorAll<HTMLButtonElement>('button')].some(
          (b) => b.textContent === '编辑' && !b.disabled,
        ),
    );
    act(() => {
      [...container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '编辑')!
        .click();
    });
    await until(
      () =>
        [...container.querySelectorAll('.cm-content')].some(
          (el) => el.getAttribute('contenteditable') === 'true',
        ),
    );
    const editorView = editorViewOf(container)!;
    // 在途写挂起：点击已提交条目触发的 guard flush 会等待它（竞态窗口开启）
    gitFileWriteMock.mockImplementation(() => dWrite.promise);
    act(() => {
      editorView.dispatch({ changes: { from: editorView.state.doc.length, insert: 'c\n' } });
    });
    clickFile(container, 'b.txt');
    await until(() => gitFileWriteMock.mock.calls.length === 1);
    // 窗口内切换 base：generation 递增、已提交列表清空重拉
    setInput(baseInput(container), 'dev');
    pressEnter(baseInput(container));
    await until(() => gitBranchDiffFilesMock.mock.calls.some((c) => c[1] === 'dev'));
    // 放行写回 → guard true → 恢复后意图已失效：不得用旧 base 请求、不得建选中、不得切焦点
    await act(async () => {
      dWrite.resolve({ baseHash: 'h1' });
    });
    await flushUI();
    expect(gitBranchDiffFileMock).not.toHaveBeenCalled();
    expect(headerText(container)).toContain('a.ts');
    expect(hiddenDiffText(container)).not.toContain('b.txt（');
    unmount();
  });

  it('branch 内刷新不被隐藏编辑会话的阻塞 guard 拦截（X2-refresh）', async () => {
    const { container, unmount } = await mountPanel();
    await until(() => rowPaths(container).includes('b.txt'));
    // 未提交条目进入编辑，写回成功后切到已提交焦点
    clickFile(container, 'a.ts');
    await until(() => container.querySelector('.cm-editor') !== null);
    act(() => {
      [...container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '单列')!
        .click();
    });
    await until(() => container.querySelectorAll('.cm-editor').length === 1);
    await until(
      () =>
        [...container.querySelectorAll<HTMLButtonElement>('button')].some(
          (b) => b.textContent === '编辑' && !b.disabled,
        ),
    );
    act(() => {
      [...container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '编辑')!
        .click();
    });
    await until(
      () =>
        [...container.querySelectorAll('.cm-content')].some(
          (el) => el.getAttribute('contenteditable') === 'true',
        ),
    );
    const editorView = editorViewOf(container)!;
    act(() => {
      editorView.dispatch({ changes: { from: editorView.state.doc.length, insert: 'c\n' } });
    });
    await until(() => gitFileWriteMock.mock.calls.length === 1);
    clickFile(container, 'b.txt');
    await until(() => headerText(container).includes('b.txt'));
    await until(() => visibleDiffText(container).includes('b-old'));
    // hidden 编辑会话继续修改 → 写回 409 → guard 阻塞
    gitFileWriteMock.mockRejectedValue(new ApiError(409, 'conflict', 'hash 不一致'));
    act(() => {
      editorView.dispatch({ changes: { from: editorView.state.doc.length, insert: 'd\n' } });
    });
    await until(() => container.textContent?.includes('保存冲突') ?? false, 6000);
    // 刷新（focus:false）：不被无关编辑会话阻塞，三路请求照常发出，焦点保持
    const statusCalls = gitStatusMock.mock.calls.length;
    const listCalls = gitBranchDiffFilesMock.mock.calls.length;
    const fileCalls = gitBranchDiffFileMock.mock.calls.length;
    clickButton(container, '刷新');
    await until(() => gitBranchDiffFilesMock.mock.calls.length === listCalls + 1);
    await until(() => gitBranchDiffFileMock.mock.calls.length === fileCalls + 1);
    await until(() => gitStatusMock.mock.calls.length === statusCalls + 1);
    await flush();
    expect(headerText(container)).toContain('b.txt');
    unmount();
  });

  it('committed diff 重试不被隐藏编辑会话的阻塞 guard 拦截（X2-retry）', async () => {
    gitBranchDiffFileMock
      .mockRejectedValueOnce(new ApiError(500, 'internal', 'boom'))
      .mockResolvedValueOnce(diffOf('b'));
    const { container, unmount } = await mountPanel();
    await until(() => rowPaths(container).includes('b.txt'));
    clickFile(container, 'a.ts');
    await until(() => container.querySelector('.cm-editor') !== null);
    act(() => {
      [...container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '单列')!
        .click();
    });
    await until(() => container.querySelectorAll('.cm-editor').length === 1);
    await until(
      () =>
        [...container.querySelectorAll<HTMLButtonElement>('button')].some(
          (b) => b.textContent === '编辑' && !b.disabled,
        ),
    );
    act(() => {
      [...container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '编辑')!
        .click();
    });
    await until(
      () =>
        [...container.querySelectorAll('.cm-content')].some(
          (el) => el.getAttribute('contenteditable') === 'true',
        ),
    );
    const editorView = editorViewOf(container)!;
    act(() => {
      editorView.dispatch({ changes: { from: editorView.state.doc.length, insert: 'c\n' } });
    });
    await until(() => gitFileWriteMock.mock.calls.length === 1);
    // 切到已提交焦点：首次请求失败 → stale + 重试入口
    clickFile(container, 'b.txt');
    await until(() => container.textContent?.includes('boom') ?? false);
    // hidden 编辑会话阻塞（409）
    gitFileWriteMock.mockRejectedValue(new ApiError(409, 'conflict', 'hash 不一致'));
    act(() => {
      editorView.dispatch({ changes: { from: editorView.state.doc.length, insert: 'd\n' } });
    });
    await until(() => container.textContent?.includes('保存冲突') ?? false, 6000);
    // 重试（focus:false）：跳过 guard 照常发请求并渲染
    clickButton(container, '重试');
    await until(() => gitBranchDiffFileMock.mock.calls.length === 2);
    await until(() => visibleDiffText(container).includes('b-old'));
    expect(container.querySelector('.git-error')).toBeNull();
    unmount();
  });

  it('ReviewPanel 展示任务全部活动批注（Y4）：消失/已提交文件批注可见可操作', async () => {
    const listAnnotationsMock = vi.mocked(api.listAnnotations);
    listAnnotationsMock.mockResolvedValue({
      annotations: [
        mkAnn('ann-1', 'a.ts', '', false), // 未提交条目
        mkAnn('ann-2', 'gone.ts', '', false), // 文件已从 status 消失（stale 批注仍可见可提交）
        mkAnn('ann-3', 'b.txt', '', false), // 已提交路径
      ],
      submitCapability: { state: 'supported', reason: '' },
    });
    const { container, unmount } = await mountPanel();
    await until(() => rowPaths(container).includes('b.txt'));
    // 全部活动批注在 ReviewPanel 可见（不做 status 过滤）
    await until(() => container.textContent?.includes('批注-ann-1') ?? false);
    expect(container.textContent).toContain('批注-ann-2');
    expect(container.textContent).toContain('批注-ann-3');
    const items = [...container.querySelectorAll('.review-panel .ann-item')].map(
      (n) => n.textContent ?? '',
    );
    expect(items.some((t) => t.includes('gone.ts'))).toBe(true);
    expect(items.some((t) => t.includes('b.txt'))).toBe(true);
    // 已提交条目的 DiffViewer 仍不接批注（只读门控不变）：打开 b.txt 无批注手势入口
    clickFile(container, 'b.txt');
    await until(() => visibleDiffText(container).includes('b-old'));
    // ReviewPanel 三条批注不受 branch diff 切换影响
    expect(container.textContent).toContain('批注-ann-2');
    unmount();
  });

  it('branch 视图提交成功：已提交列表失效并自动重载、选中清空（Y3）', async () => {
    gitCommitMock.mockResolvedValue();
    const { container, unmount } = await mountPanel();
    await until(() => rowPaths(container).includes('b.txt'));
    clickFile(container, 'b.txt');
    await until(() => headerText(container).includes('b.txt'));
    const statusCalls = gitStatusMock.mock.calls.length;
    const listCalls = gitBranchDiffFilesMock.mock.calls.length;
    // 输入提交信息并提交（勾选默认覆盖全部未提交条目 a.ts）
    const textarea = container.querySelector<HTMLTextAreaElement>('.git-commit-msg')!;
    const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!;
    act(() => {
      setter.call(textarea, 'commit msg');
      textarea.dispatchEvent(new Event('input', { bubbles: true }));
    });
    clickButton(container, '提交（1）');
    await until(() => container.textContent?.includes('提交完成') ?? false);
    // 新提交改变 HEAD 对比基线：status 刷新 + 已提交列表自动重载
    await until(() => gitStatusMock.mock.calls.length === statusCalls + 1);
    await until(() => gitBranchDiffFilesMock.mock.calls.length === listCalls + 1);
    await flush();
    // 已提交选中失效：committed 块清空
    expect(headerText(container)).toBe('');
    unmount();
  });

  it('commit 在途期间切到 branch 视图：放行后按完成时视图重载已提交列表（Y6 交错一）', async () => {
    const dCommit = deferred<void>();
    gitCommitMock.mockImplementation(() => dCommit.promise);
    // 从 uncommitted 视图发起提交（baseRef=main 为 branch 列表 base）
    const { container, unmount } = mount(<GitPanel taskID="t1" active baseRef="main" />);
    await until(() => rowPaths(container).includes('a.ts'));
    const textarea = container.querySelector<HTMLTextAreaElement>('.git-commit-msg')!;
    const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!;
    act(() => {
      setter.call(textarea, 'commit msg');
      textarea.dispatchEvent(new Event('input', { bubbles: true }));
    });
    clickButton(container, '提交（1）');
    await until(() => gitCommitMock.mock.calls.length === 1); // 提交在途
    // 在途期间切换到 branch 视图（进入时按 baseRef 首拉列表）
    clickTab(container, '分支对比');
    await until(() => activeTabOf(container) === '分支对比');
    await until(() => gitBranchDiffFilesMock.mock.calls.length === 1);
    const listCalls = gitBranchDiffFilesMock.mock.calls.length;
    // 放行提交：完成时视图为 branch → 必须再次重载已提交列表
    await act(async () => {
      dCommit.resolve();
    });
    await until(() => gitBranchDiffFilesMock.mock.calls.length === listCalls + 1);
    await until(() => rowPaths(container).includes('b.txt'));
    await flush();
    expect(gitBranchDiffFileMock).not.toHaveBeenCalled(); // 无选中，不拉文件 diff
    unmount();
  });

  it('commit 在途期间 base A→B：放行后不得用旧 base 请求列表（Y6 交错二）', async () => {
    const dCommit = deferred<void>();
    gitCommitMock.mockImplementation(() => dCommit.promise);
    gitBranchDiffFilesMock.mockImplementation((_t, base) =>
      Promise.resolve(base === 'main' ? branchFilesOf('b.txt') : branchFilesOf('d.txt')),
    );
    const { container, unmount } = await mountPanel();
    await until(() => rowPaths(container).includes('b.txt'));
    // branch 视图内发起提交（发起时 base=main）
    const textarea = container.querySelector<HTMLTextAreaElement>('.git-commit-msg')!;
    const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!;
    act(() => {
      setter.call(textarea, 'commit msg');
      textarea.dispatchEvent(new Event('input', { bubbles: true }));
    });
    clickButton(container, '提交（1）');
    await until(() => gitCommitMock.mock.calls.length === 1);
    // 在途期间 base main→dev：applyBranchBase 已 generation 递增并重拉 dev 列表
    setInput(baseInput(container), 'dev');
    pressEnter(baseInput(container));
    await until(() => rowPaths(container).includes('d.txt'));
    // 放行提交：失效可幂等，但重载必须用完成时的当前 base（dev），不得用旧 base（main）
    await act(async () => {
      dCommit.resolve();
    });
    await until(() => container.textContent?.includes('提交完成') ?? false);
    await flush();
    const mainCalls = gitBranchDiffFilesMock.mock.calls.filter((c) => c[1] === 'main').length;
    expect(mainCalls).toBe(1); // 仅进入视图时的首拉，commit 放行不得再请求 main
    expect(rowPaths(container)).toContain('d.txt'); // dev 列表正常
    unmount();
  });

  it('旧已提交文件响应跨 generation 丢弃：成功（Y5①）', async () => {
    const dMain = deferred<GitDiffResult>();
    gitBranchDiffFileMock.mockImplementation((_t, base) =>
      base === 'main' ? dMain.promise : Promise.resolve(diffOf('d')),
    );
    gitBranchDiffFilesMock.mockImplementation((_t, base) =>
      Promise.resolve(base === 'main' ? branchFilesOf('b.txt') : branchFilesOf('d.txt')),
    );
    const { container, unmount } = await mountPanel();
    await until(() => rowPaths(container).includes('b.txt'));
    // 请求 1（base=main）在途
    clickFile(container, 'b.txt');
    await until(() => gitBranchDiffFileMock.mock.calls.length === 1);
    // 切 base：generation 递增、列表重拉 dev
    setInput(baseInput(container), 'dev');
    pressEnter(baseInput(container));
    await until(() => rowPaths(container).includes('d.txt'));
    // 请求 1 晚到成功：跨 generation 丢弃
    await act(async () => {
      dMain.resolve(diffOf('c'));
    });
    await flush();
    expect(visibleDiffText(container)).not.toContain('c-old');
    expect(container.querySelector('.git-error')).toBeNull();
    unmount();
  });

  it('旧已提交文件响应跨 generation 丢弃：失败（Y5①）', async () => {
    const dMain = deferred<GitDiffResult>();
    gitBranchDiffFileMock.mockImplementation((_t, base) =>
      base === 'main' ? dMain.promise : Promise.resolve(diffOf('d')),
    );
    gitBranchDiffFilesMock.mockImplementation((_t, base) =>
      Promise.resolve(base === 'main' ? branchFilesOf('b.txt') : branchFilesOf('d.txt')),
    );
    const { container, unmount } = await mountPanel();
    await until(() => rowPaths(container).includes('b.txt'));
    clickFile(container, 'b.txt');
    await until(() => gitBranchDiffFileMock.mock.calls.length === 1);
    setInput(baseInput(container), 'dev');
    pressEnter(baseInput(container));
    await until(() => rowPaths(container).includes('d.txt'));
    // 请求 1 晚到失败：跨 generation 丢弃，不得置 stale/error
    await act(async () => {
      dMain.reject(new ApiError(500, 'internal', 'stale boom'));
    });
    await flush();
    expect(container.querySelector('.git-error')).toBeNull();
    expect(container.querySelector('.git-files-stale')).toBeNull();
    expect(visibleDiffText(container)).not.toContain('stale boom');
    unmount();
  });

  it('连续重载：同代乱序下旧响应不覆盖新状态（Y5②）', async () => {
    const d1 = deferred<GitBranchDiffFilesResult>();
    const d2 = deferred<GitBranchDiffFilesResult>();
    gitBranchDiffFilesMock
      .mockRejectedValueOnce(new ApiError(500, 'internal', 'boom'))
      .mockImplementationOnce(() => d1.promise)
      .mockImplementationOnce(() => d2.promise);
    const { container, unmount } = await mountPanel();
    await until(() => container.textContent?.includes('boom') ?? false);
    // stale 区重载按钮无 disabled：连点两次，d1/d2 同时在途
    clickButton(container, '重载');
    clickButton(container, '重载');
    // 新序号响应先到
    await act(async () => {
      d2.resolve(branchFilesOf('ok2.txt'));
    });
    await until(() => rowPaths(container).includes('ok2.txt'));
    // 旧序号响应晚到失败：不得置 stale、不得清列表
    await act(async () => {
      d1.reject(new Error('late boom'));
    });
    await flush();
    expect(rowPaths(container)).toContain('ok2.txt');
    expect(container.querySelector('.git-files-stale')).toBeNull();
    expect(container.querySelector('.git-error')).toBeNull();
    unmount();
  });
});

describe('tasks 6.3：混合列表（已提交+未提交）排序与固定次序', () => {
  const committedFiles = [f('x.ts')];
  // staged/unstaged 与 untracked 需分开两个 DTO（D5：untracked 文件无「未暂存」语义）
  const statusFiles = (): GitFileEntry[] => [
    { path: 'x.ts', x: 'M', y: 'M', staged: true, unstaged: true, untracked: false, additions: 2, deletions: 1, isBinary: false },
    { path: 'x.ts', x: '?', y: '?', staged: false, unstaged: false, untracked: true, additions: 0, deletions: 0, isBinary: false },
  ];

  it('纯函数：同路径多条目固定次序 已提交→已暂存→未暂存→未跟踪；不同路径 byte-wise', () => {
    const rows = listFileRows(statusFiles(), committedFiles);
    expect(rows.map((r) => `${r.path}|${r.suffix}`)).toEqual([
      'x.ts|branch',
      'x.ts|staged',
      'x.ts|unstaged',
      'x.ts|untracked',
    ]);
    const mixed = listFileRows(
      [{ path: 'y.ts', x: 'M', y: ' ', staged: false, unstaged: true, untracked: false, additions: 0, deletions: 0, isBinary: false }],
      [f('a.ts')],
    );
    expect(mixed.map((r) => `${r.path}|${r.suffix}`)).toEqual(['a.ts|branch', 'y.ts|unstaged']);
  });

  it('tree 投影：同路径多条目独立不去重，同级固定次序与 flat 一致', () => {
    const tree = buildFileTree(listFileRows(statusFiles(), committedFiles));
    expect(tree.map((n) => n.key)).toEqual([
      'x.ts\0branch',
      'x.ts\0staged',
      'x.ts\0unstaged',
      'x.ts\0untracked',
    ]);
  });

  it('组件渲染：branch 视图混合列表已提交条目置前并带 badge（flat 与 tree 一致）', async () => {
    gitStatusMock.mockResolvedValue({ branch: 'main', files: statusFiles() });
    gitBranchDiffFilesMock.mockResolvedValue({ baseRef: 'main', files: committedFiles });
    const { container, unmount } = await mountPanel();
    await until(() => rowPaths(container).length === 4);
    const classes = pathClasses(container);
    expect(classes[0]).toContain('git-file-committed');
    expect(classes[1]).toContain('git-file-staged');
    expect(classes[2]).toContain('git-file-unstaged');
    expect(classes[3]).toContain('git-file-untracked');
    expect(container.querySelectorAll('.git-file-badge')).toHaveLength(1);
    clickButton(container, '目录树');
    expect(pathClasses(container)[0]).toContain('git-file-committed');
    expect(container.querySelectorAll('.git-files .git-file')).toHaveLength(4); // 不去重
    unmount();
  });

  it('compareBytewise：增补平面字符按 UTF-8 字节序（W4：JS 字符串 `<` 的 UTF-16 序在此相反）', () => {
    // \uE000 UTF-8 = EE 80 80；\u{10000} UTF-8 = F0 90 80 80 → 前者字节序小；
    // JS `<` 按 UTF-16 code unit：'\u{10000}' 首单元 0xD800 < 0xE000，结果相反
    expect(compareBytewise('\uE000', '\u{10000}')).toBe(-1);
    expect(compareBytewise('\u{10000}', '\uE000')).toBe(1);
    expect(compareBytewise('\uE000', '\uE000')).toBe(0);
  });

  it('base 为空：已提交部分显示「请输入基础 ref」且零请求，未提交条目照常展示', async () => {
    const { container, unmount } = mount(<GitPanel taskID="t1" active />);
    await until(() => rowPaths(container).includes('a.ts'));
    clickTab(container, '分支对比');
    await until(() => container.textContent?.includes('请输入基础 ref') ?? false);
    expect(rowPaths(container)).toEqual(['a.ts']);
    await flush();
    expect(gitBranchDiffFilesMock).not.toHaveBeenCalled();
    unmount();
  });
});

describe('tasks 6.4/6.5：按条目类型门控、共享状态与视图动作表', () => {
  it('已提交条目只读（无勾选/无编辑入口/零资格预取）；未提交条目完整能力（编辑/批注可用）', async () => {
    const { container, unmount } = await mountPanel();
    await until(() => rowPaths(container).includes('b.txt'));
    // 已提交行：无勾选、有「已提交」badge
    const committedRow = [...container.querySelectorAll<HTMLElement>('.git-file')].find(
      (n) => n.querySelector('.git-file-committed') !== null,
    );
    expect(committedRow).toBeTruthy();
    expect(committedRow!.querySelector('input[type=checkbox]')).toBeNull();
    expect(committedRow!.textContent).toContain('已提交');
    // 未提交行：勾选在场
    const uncommittedRow = [...container.querySelectorAll<HTMLElement>('.git-file')].find(
      (n) => n.querySelector('.git-file-path')?.textContent === 'a.ts',
    );
    expect(uncommittedRow!.querySelector('input[type=checkbox]')).not.toBeNull();
    // 提交输入区与批注面板在 branch 视图可用（作用于未提交条目）
    expect(container.querySelector('.git-commit-box')).not.toBeNull();
    expect(container.querySelector('.review-panel')).not.toBeNull();
    // 点开已提交条目：只读 viewer，无编辑入口、无资格预取
    clickFile(container, 'b.txt');
    await until(() => container.querySelectorAll('.cm-editor').length >= 1);
    await flushUI();
    expect(gitFileReadMock).not.toHaveBeenCalled();
    expect(gitFileWriteMock).not.toHaveBeenCalled();
    expect(
      [...container.querySelectorAll<HTMLButtonElement>('button')].filter(
        (b) => b.textContent === '编辑' && !b.disabled,
      ),
    ).toHaveLength(0);
    expect(container.textContent).not.toContain('退出编辑');
    // 点开未提交条目：完整能力（编辑资格预取 + 编辑入口）
    clickFile(container, 'a.ts');
    await until(() => headerText(container).includes('a.ts'));
    await until(() => gitFileReadMock.mock.calls.some((c) => c[1] === 'a.ts'));
    await until(
      () =>
        [...container.querySelectorAll<HTMLButtonElement>('button')].some(
          (b) => b.textContent === '编辑' && !b.disabled,
        ),
    );
    expect(container.textContent).toContain('批注（');
    unmount();
  });

  it('uncommitted→branch：离开守卫拒绝（编辑冲突未解决）时停留', async () => {
    gitFileWriteMock.mockRejectedValue(new ApiError(409, 'conflict', 'hash 不一致'));
    const { container, unmount } = mount(<GitPanel taskID="t1" active baseRef="main" />);
    await enterEditMode(container);
    const view = editorViewOf(container)!;
    act(() => {
      view.dispatch({ changes: { from: view.state.doc.length, insert: 'c\n' } });
    });
    await until(() => container.textContent?.includes('保存冲突') ?? false, 6000);
    clickTab(container, '分支对比');
    await flushUI();
    expect(container.querySelector('.git-base-ref')).toBeNull();
    expect(container.querySelector('.git-view-tab-active')?.textContent).toBe('未提交变更');
    unmount();
  });

  it('视图动作分发：branch 刷新 = 已提交列表+选中已提交 diff+loadStatus；push 成功刷新 status 且已提交列表/选中不变', async () => {
    const { container, unmount } = await mountPanel();
    await until(() => rowPaths(container).includes('b.txt'));
    clickFile(container, 'b.txt');
    await until(() => headerText(container).includes('b.txt'));
    const statusCalls = gitStatusMock.mock.calls.length; // mount 激活后 = 1
    const listCalls = gitBranchDiffFilesMock.mock.calls.length;
    const fileCalls = gitBranchDiffFileMock.mock.calls.length;
    // 刷新（branch 分发：列表 + 选中 diff + 共享 loadStatus）
    clickButton(container, '刷新');
    await until(() => gitBranchDiffFilesMock.mock.calls.length === listCalls + 1);
    await until(() => gitBranchDiffFileMock.mock.calls.length === fileCalls + 1);
    await until(() => gitStatusMock.mock.calls.length === statusCalls + 1);
    await flush();
    expect(headerText(container)).toContain('b.txt');
    // 推送成功：loadStatus 刷新，已提交列表/选中保持
    clickButton(container, '推送');
    await until(() => container.textContent?.includes('推送完成') ?? false);
    await flush();
    expect(gitStatusMock.mock.calls.length).toBe(statusCalls + 2);
    expect(gitBranchDiffFilesMock.mock.calls.length).toBe(listCalls + 1);
    expect(gitBranchDiffFileMock.mock.calls.length).toBe(fileCalls + 1);
    expect(rowPaths(container)).toContain('b.txt');
    expect(headerText(container)).toContain('b.txt');
    unmount();
  });

  it('两视图各自保持选中：branch 操作不清未提交选中，branch 内可打开两类 diff（W1/W2）', async () => {
    const { container, unmount } = mount(<GitPanel taskID="t1" active baseRef="main" />);
    // uncommitted 视图打开 a.ts
    await until(() => rowPaths(container).includes('a.ts'));
    clickFile(container, 'a.ts');
    await until(() => headerText(container).includes('a.ts'));
    // 切 branch：branch 无已提交选中 → diff 区留空（各自保持，不带出未提交选中显示）
    clickTab(container, '分支对比');
    await until(() => activeTabOf(container) === '分支对比');
    await until(() => rowPaths(container).includes('b.txt'));
    expect(headerText(container)).toBe('');
    // branch 内点已提交 b.txt
    clickFile(container, 'b.txt');
    await until(() => headerText(container).includes('b.txt'));
    // base 变更只清已提交选中与 diff；未提交选中保持
    setInput(baseInput(container), 'dev');
    pressEnter(baseInput(container));
    await until(() => gitBranchDiffFilesMock.mock.calls.some((c) => c[1] === 'dev'));
    await flush();
    expect(headerText(container)).toBe('');
    expect(rowPaths(container)).toContain('a.ts');
    // branch 内点未提交 a.ts：焦点切到未提交块并显示其 diff
    clickFile(container, 'a.ts');
    await until(() => headerText(container).includes('a.ts'));
    await until(() => visibleDiffText(container).includes('a-old'));
    // 切回 uncommitted：未提交选中与 diff 保留（已提交操作未触碰共享状态）
    clickTab(container, '未提交变更');
    await until(() => activeTabOf(container) === '未提交变更');
    await until(() => headerText(container).includes('a.ts'));
    expect(container.textContent).toContain('a-old');
    // branch 内点回已提交 b.txt 后切走再切回：两类选中各自保留
    clickTab(container, '分支对比');
    await until(() => activeTabOf(container) === '分支对比');
    clickFile(container, 'b.txt');
    await until(() => headerText(container).includes('b.txt'));
    clickTab(container, '未提交变更');
    await until(() => activeTabOf(container) === '未提交变更');
    await until(() => headerText(container).includes('a.ts'));
    clickTab(container, '分支对比');
    await until(() => activeTabOf(container) === '分支对比');
    await until(() => headerText(container).includes('b.txt'));
    unmount();
  });

  it('branch→uncommitted：离开守卫拒绝（branch 内编辑未提交条目冲突）时停留 branch（W3）', async () => {
    gitFileWriteMock.mockRejectedValue(new ApiError(409, 'conflict', 'hash 不一致'));
    const { container, unmount } = await mountPanel();
    await until(() => rowPaths(container).includes('b.txt'));
    // branch 视图内打开未提交条目并进入编辑
    clickFile(container, 'a.ts');
    await until(() => container.querySelector('.cm-editor') !== null);
    act(() => {
      [...container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '单列')!
        .click();
    });
    await until(() => container.querySelectorAll('.cm-editor').length === 1);
    await until(
      () =>
        [...container.querySelectorAll<HTMLButtonElement>('button')].some(
          (b) => b.textContent === '编辑' && !b.disabled,
        ),
    );
    act(() => {
      [...container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '编辑')!
        .click();
    });
    await until(
      () =>
        [...container.querySelectorAll('.cm-content')].some(
          (el) => el.getAttribute('contenteditable') === 'true',
        ),
    );
    const editorView = editorViewOf(container)!;
    act(() => {
      editorView.dispatch({ changes: { from: editorView.state.doc.length, insert: 'c\n' } });
    });
    await until(() => container.textContent?.includes('保存冲突') ?? false, 6000);
    clickTab(container, '未提交变更');
    await flushUI();
    expect(container.querySelector('.git-view-tab-active')?.textContent).toBe('分支对比');
    unmount();
  });
});
