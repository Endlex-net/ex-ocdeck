// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act } from 'react';
import { EditorView } from '@codemirror/view';
import { GitPanel } from '../components/GitPanel';
import { api, ApiError } from '../api';
import type { GitDiffResult, GitFileEntry } from '../types';
import { flushUI, mount, stubMatchMedia } from './cm-test-env';

/* ============================ F11：视图移除路径必须过离开事务（flush + 等待在途） ============================
 * 工具栏刷新 / commit / push 都走 loadStatus；文件从 status 消失时 selFile 会被清除。
 * 清除前必须 flush 编辑会话，否则 debounce 内最新文本静默丢失；阻塞未解决时保留视图。 */

vi.mock('../api', () => ({
  api: {
    gitStatus: vi.fn(),
    gitDiff: vi.fn(),
    gitCommit: vi.fn(),
    gitPush: vi.fn(),
    listAnnotations: vi.fn(async () => ({
      annotations: [],
      submitCapability: { state: 'supported', reason: '' },
    })),
    listSubmissions: vi.fn(async () => ({ queue: [], history: [], failures: [] })),
    gitFileRead: vi.fn(),
    gitFileWrite: vi.fn(),
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
const gitFileReadMock = vi.mocked((api as unknown as { gitFileRead: ReturnType<typeof vi.fn> }).gitFileRead);
const gitFileWriteMock = vi.mocked(
  (api as unknown as { gitFileWrite: ReturnType<typeof vi.fn> }).gitFileWrite,
);

const entry: GitFileEntry = {
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

const diff: GitDiffResult = {
  oldContent: 'a\n',
  newContent: 'a\nb\n',
  oldExists: true,
  newExists: true,
  oldMode: '100644',
  newMode: '100644',
  isBinary: false,
  truncated: false,
};

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

function editorViewOf(container: HTMLElement): EditorView | null {
  for (const el of container.querySelectorAll('.cm-content')) {
    const v = EditorView.findFromDOM(el as HTMLElement);
    if (v) return v;
  }
  return null;
}

/** 打开文件 diff 并进入编辑模式。 */
async function enterEditMode(container: HTMLElement) {
  await until(() => container.querySelector('.git-file-path') !== null);
  act(() => {
    (container.querySelector('.git-file-path') as HTMLElement).click();
  });
  await until(() => container.querySelector('.cm-editor') !== null);
  // 宽屏默认为并排（a 侧只读）：切单列，编辑目标为唯一编辑器
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

async function clickRefresh(container: HTMLElement) {
  await act(async () => {
    [...container.querySelectorAll<HTMLButtonElement>('button')]
      .find((b) => b.textContent === '刷新')!
      .click();
    await Promise.resolve();
  });
}

beforeEach(() => {
  vi.clearAllMocks();
  stubMatchMedia(false);
  gitStatusMock.mockResolvedValue({ branch: 'main', files: [entry] });
  gitDiffMock.mockResolvedValue(diff);
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

describe('F11：loadStatus 清视图前过离开守卫', () => {
  it('刷新时文件消失：debounce 内的最新编辑先 flush 写出，再清除视图', async () => {
    const { container, unmount } = mount(<GitPanel taskID="t1" active />);
    await enterEditMode(container);
    const view = editorViewOf(container)!;
    // 第一次编辑：等待保存完成（clean）
    act(() => {
      view.dispatch({ changes: { from: view.state.doc.length, insert: 'c\n' } });
    });
    await until(() => gitFileWriteMock.mock.calls.length === 1);
    await flushUI();
    // 第二次编辑：debounce 500ms 窗口内立即刷新（文件从 status 消失）
    act(() => {
      view.dispatch({ changes: { from: view.state.doc.length, insert: 'd\n' } });
    });
    gitStatusMock.mockResolvedValue({ branch: 'main', files: [] });
    await clickRefresh(container);
    // 离开事务先 flush：最新文本必须写出
    await until(() => gitFileWriteMock.mock.calls.length >= 2);
    expect(gitFileWriteMock.mock.calls[1][1].content).toBe('a\nb\n' + 'c\n' + 'd\n');
    // 然后视图才被清除
    await until(() => container.querySelector('.git-diff-header') === null);
    unmount();
  });

  it('刷新时文件消失但写回 409 阻塞：保留视图与冲突横幅，不清 selFile', async () => {
    gitFileWriteMock.mockRejectedValue(new ApiError(409, 'conflict', 'hash 不一致'));
    const { container, unmount } = mount(<GitPanel taskID="t1" active />);
    await enterEditMode(container);
    const view = editorViewOf(container)!;
    act(() => {
      view.dispatch({ changes: { from: view.state.doc.length, insert: 'c\n' } });
    });
    // 写回 409 → 阻塞横幅
    await until(() => container.textContent?.includes('保存冲突') ?? false, 6000);
    // 文件从 status 消失 + 刷新：离开守卫拒绝（阻塞未解决）→ 视图保留
    gitStatusMock.mockResolvedValue({ branch: 'main', files: [] });
    await clickRefresh(container);
    await flushUI();
    expect(container.querySelector('.git-diff-header')).not.toBeNull();
    expect(container.textContent).toContain('保存冲突');
    unmount();
  });
});
