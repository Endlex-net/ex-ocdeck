// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act } from 'react';
import { GitPanel } from '../components/GitPanel';
import { api } from '../api';
import type { GitDiffResult, GitFileEntry } from '../types';
import { mount, stubMatchMedia } from './cm-test-env';
/* ============================ GitPanel × DiffViewer 集成（tasks 4.2/4.4，DR1） ============================
 * 真实经过 GitPanel 渲染 + React.lazy DiffViewer chunk 加载路径；api 层 mock。 */

vi.mock('../api', () => ({
  api: {
    gitStatus: vi.fn(),
    gitDiff: vi.fn(),
    gitCommit: vi.fn(),
    gitPush: vi.fn(),
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

function makeEntry(path: string, over: Partial<GitFileEntry>): GitFileEntry {
  return {
    path,
    x: 'M',
    y: ' ',
    staged: false,
    unstaged: true,
    untracked: false,
    additions: 1,
    deletions: 1,
    isBinary: false,
    ...over,
  };
}

function makeDiff(oldContent: string, newContent: string): GitDiffResult {
  return {
    oldContent,
    newContent,
    oldExists: oldContent !== '',
    newExists: newContent !== '',
    // 不存在侧的 mode 恒为空串（spec「文件 diff 查看」）
    oldMode: oldContent !== '' ? '100644' : '',
    newMode: newContent !== '' ? '100644' : '',
    isBinary: false,
    truncated: false,
  };
}

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

/** 面板 DOM 中点击指定文件路径打开 diff。 */
function clickFile(container: HTMLElement, path: string) {
  const el = [...container.querySelectorAll('.git-file-path')].find((n) =>
    n.textContent?.includes(path),
  );
  expect(el, `file entry ${path}`).toBeTruthy();
  act(() => el!.dispatchEvent(new MouseEvent('click', { bubbles: true })));
}

describe('GitPanel diff 区域（CodeMirror merge 集成）', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    stubMatchMedia(false); // 宽屏默认并排
    gitStatusMock.mockResolvedValue({
      branch: 'main',
      files: [
        makeEntry('src/a.txt', { staged: true, unstaged: false }),
        makeEntry('src/b.txt', {}),
      ],
    });
    gitDiffMock.mockImplementation((_taskID, _ref, path) => {
      if (path === 'src/a.txt') return Promise.resolve(makeDiff('a-old\n', 'a-new\n'));
      return Promise.resolve(makeDiff('b-old\n', 'b-new\n'));
    });
  });

  it('点击文件经真实 gitDiff 调用渲染 merge 视图（懒加载 chunk）', async () => {
    const { container, unmount } = mount(<GitPanel taskID="t1" active />);
    await until(() => container.querySelectorAll('.git-file-path').length === 2);
    clickFile(container, 'src/a.txt');
    await until(() => container.querySelectorAll('.cm-editor').length === 2);
    expect(gitDiffMock).toHaveBeenCalledWith('t1', 'HEAD', 'src/a.txt', false);
    expect(container.textContent).toContain('a-old');
    unmount();
  });

  it('modeOverride 由 GitPanel 持有：切换文件后保留手动形态选择，面板卸载后丢弃', async () => {
    const { container, unmount } = mount(<GitPanel taskID="t1" active />);
    await until(() => container.querySelectorAll('.git-file-path').length === 2);

    // 打开 a.txt（宽屏默认并排）→ 手动切到单列
    clickFile(container, 'src/a.txt');
    await until(() => container.querySelectorAll('.cm-editor').length === 2);
    const unifiedBtn = [...container.querySelectorAll<HTMLButtonElement>('.diff-toolbar button')].find(
      (b) => b.textContent?.includes('单列'),
    );
    act(() => unifiedBtn!.click());
    await until(() => container.querySelectorAll('.cm-editor').length === 1);

    // 切换到 b.txt：override 保留（仍单列），diff 内容更新
    clickFile(container, 'src/b.txt');
    await until(() => container.textContent?.includes('b-old') ?? false);
    expect(container.querySelectorAll('.cm-editor')).toHaveLength(1);

    // 卸载重挂：override 丢弃，回到宽屏默认并排
    unmount();
    const again = mount(<GitPanel taskID="t1" active />);
    await until(() => again.container.querySelectorAll('.git-file-path').length === 2);
    clickFile(again.container, 'src/a.txt');
    await until(() => again.container.querySelectorAll('.cm-editor').length === 2);
    again.unmount();
  });

  it('diff 加载失败展示错误提示，不渲染编辑器', async () => {
    gitDiffMock.mockRejectedValue(new Error('git boom'));
    const { container, unmount } = mount(<GitPanel taskID="t1" active />);
    await until(() => container.querySelectorAll('.git-file-path').length === 2);
    clickFile(container, 'src/b.txt');
    await until(() => container.querySelector('.git-error') !== null);
    expect(container.textContent).toContain('加载 diff 失败');
    expect(container.querySelectorAll('.cm-editor')).toHaveLength(0);
    unmount();
  });

  it('wrapOverride 由 GitPanel 持有：切换文件后保留换行选择（与 modeOverride 同生命周期）', async () => {
    const { container, unmount } = mount(<GitPanel taskID="t1" active />);
    await until(() => container.querySelectorAll('.git-file-path').length === 2);

    // 打开 a.txt（默认不折行）→ 开启换行
    clickFile(container, 'src/a.txt');
    await until(() => container.querySelectorAll('.cm-editor').length === 2);
    expect(container.querySelectorAll('.cm-content.cm-lineWrapping')).toHaveLength(0);
    const wrapBtn = [...container.querySelectorAll<HTMLButtonElement>('.diff-toolbar button')].find(
      (b) => b.textContent?.includes('换行'),
    );
    act(() => wrapBtn!.click());
    await until(() => container.querySelectorAll('.cm-content.cm-lineWrapping').length === 2);

    // 切换到 b.txt：换行选择保留（仍折行），diff 内容更新
    clickFile(container, 'src/b.txt');
    await until(() => container.textContent?.includes('b-old') ?? false);
    expect(container.querySelectorAll('.cm-content.cm-lineWrapping').length === 2);
    unmount();
  });

  it('请求乱序防护：晚到的旧响应不覆盖最新文件 diff（I2）', async () => {
    const dA = deferred<GitDiffResult>();
    const dB = deferred<GitDiffResult>();
    gitDiffMock.mockImplementation((_t, _r, path) => (path === 'src/a.txt' ? dA.promise : dB.promise));
    const { container, unmount } = mount(<GitPanel taskID="t1" active />);
    await until(() => container.querySelectorAll('.git-file-path').length === 2);

    // 先点 a（挂起）再点 b；b 先完成 → 展示 b
    clickFile(container, 'src/a.txt');
    clickFile(container, 'src/b.txt');
    await act(async () => {
      dB.resolve(makeDiff('b-old\n', 'b-new\n'));
    });
    await until(() => container.textContent?.includes('b-old') ?? false);
    expect(container.textContent).toContain('b-new');

    // a 晚到成功：不覆盖 b 的 diff
    await act(async () => {
      dA.resolve(makeDiff('a-old\n', 'a-new\n'));
    });
    for (let i = 0; i < 5; i++) {
      await act(async () => {
        await Promise.resolve();
      });
    }
    expect(container.textContent).toContain('b-old');
    expect(container.textContent).not.toContain('a-old');
    unmount();
  });

  it('请求乱序防护：晚到的旧失败不写 diffError（I2）', async () => {
    const dA = deferred<GitDiffResult>();
    const dB = deferred<GitDiffResult>();
    gitDiffMock.mockImplementation((_t, _r, path) => (path === 'src/a.txt' ? dA.promise : dB.promise));
    const { container, unmount } = mount(<GitPanel taskID="t1" active />);
    await until(() => container.querySelectorAll('.git-file-path').length === 2);

    clickFile(container, 'src/a.txt');
    clickFile(container, 'src/b.txt');
    await act(async () => {
      dB.resolve(makeDiff('b-old\n', 'b-new\n'));
    });
    await until(() => container.textContent?.includes('b-old') ?? false);

    // a 晚到失败：不产生错误提示，b 的 diff 保持
    await act(async () => {
      dA.reject(new Error('late failure'));
    });
    for (let i = 0; i < 5; i++) {
      await act(async () => {
        await Promise.resolve();
      });
    }
    expect(container.querySelector('.git-error')).toBeNull();
    expect(container.textContent).toContain('b-old');
    unmount();
  });
});
