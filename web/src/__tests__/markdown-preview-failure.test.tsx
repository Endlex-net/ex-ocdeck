// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act, useState } from 'react';
import type { GitDiffResult } from '../types';
import { mount, rerender, stubMatchMedia } from './cm-test-env';

/* ============================ 预览逐侧失败隔离（tasks 3.5/3.7，design D6） ============================
 * mock MarkdownPreview 使指定侧渲染抛错，驱动 DiffViewer 的逐侧 ErrorBoundary：
 * 单侧失败仅替换该侧为「渲染失败，请切回源码」，另一侧继续正常显示（不空白）；
 * 三种重试条件（切回源码再进入预览 / 切换文件 / 该侧内容变化）分别重置重试，
 * 且未变化侧不因他侧内容变化被额外重试（Gate B F1：reset identity 逐侧收敛）。 */

const mockRenderLog: string[] = [];
/** useState initializer 仅在真实挂载时执行——精确区分「重挂」与「重渲染」。 */
const mockMountLog: string[] = [];

vi.mock('../components/diff/MarkdownPreview', () => ({
  default({ content }: { content: string }) {
    useState(() => {
      mockMountLog.push(content);
      return null;
    });
    mockRenderLog.push(content);
    if (content.includes('@FAIL_OLD@')) throw new Error('old render boom');
    if (content.includes('@FAIL_NEW@')) throw new Error('new render boom');
    return <div data-testid="mock-md">{content}</div>;
  },
}));

import DiffViewer from '../components/diff/DiffViewer';

function makeMdDiff(over: Partial<GitDiffResult>): GitDiffResult {
  return {
    oldContent: '旧侧内容\n',
    newContent: '新侧内容\n',
    oldExists: true,
    newExists: true,
    oldMode: '100644',
    newMode: '100644',
    isBinary: false,
    truncated: false,
    ...over,
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

function renderViewer(diff: GitDiffResult) {
  const onModeChange = vi.fn();
  const onWrapChange = vi.fn();
  const mounted = mount(
    <DiffViewer
      diff={diff}
      path="a.md"
      modeOverride="unified"
      onModeChange={onModeChange}
      wrapOverride={false}
      onWrapChange={onWrapChange}
    />,
  );
  return { ...mounted, onModeChange, onWrapChange };
}

function togglePreview(container: HTMLElement) {
  const btn = [...container.querySelectorAll<HTMLButtonElement>('.diff-toolbar button')].find(
    (b) => b.textContent?.trim() === '预览',
  );
  expect(btn, '预览开关存在').toBeTruthy();
  act(() => btn!.click());
}

function enterPreview(container: HTMLElement) {
  togglePreview(container);
  expect(container.querySelectorAll('.md-diff-pane')).toHaveLength(2);
}

function paneFailed(container: HTMLElement, index: 0 | 1): boolean {
  return container.querySelectorAll('.md-diff-pane')[index].querySelector('.md-preview-failed') !== null;
}

function paneContent(container: HTMLElement, index: 0 | 1): string | null {
  return container
    .querySelectorAll('.md-diff-pane')
    [index].querySelector('[data-testid=mock-md]')?.textContent ?? null;
}

describe('预览逐侧失败隔离与重试（design D6）', () => {
  // F4：渲染抛错会触发 React console.error（预期路径），拦截避免 stderr 噪音并断言其发生
  let consoleErrorSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    stubMatchMedia(false);
    mockRenderLog.length = 0;
    mockMountLog.length = 0;
    consoleErrorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
  });

  afterEach(() => {
    consoleErrorSpy.mockRestore();
  });

  it('单侧渲染失败仅替换该侧为固定提示，另一侧继续正常显示（不空白）', async () => {
    const { container, unmount } = renderViewer(
      makeMdDiff({ oldContent: '@FAIL_OLD@\n', newContent: '新侧正常\n' }),
    );
    enterPreview(container);
    await until(() => paneFailed(container, 0));
    expect(consoleErrorSpy).toHaveBeenCalled(); // 渲染异常路径真实发生（React 记录错误）
    expect(paneFailed(container, 0)).toBe(true);
    expect(container.querySelectorAll('.md-diff-pane')[0].textContent).toContain(
      '渲染失败，请切回源码',
    );
    expect(paneFailed(container, 1)).toBe(false);
    expect(paneContent(container, 1)).toBe('新侧正常\n');
    unmount();
  });

  it('该侧内容变化后重置并重试渲染（同 key 刷新）', async () => {
    const failed = makeMdDiff({ oldContent: '@FAIL_OLD@\n' });
    const mounted = renderViewer(failed);
    enterPreview(mounted.container);
    await until(() => paneFailed(mounted.container, 0));

    // 内容变化（旧侧修复）：换 key 重挂边界 → 重试渲染成功
    rerender(mounted.root, renderViewerProps(makeMdDiff({ oldContent: '旧侧修复\n' }), mounted));
    await until(() => paneContent(mounted.container, 0) === '旧侧修复\n');
    expect(paneFailed(mounted.container, 0)).toBe(false);
    expect(mounted.container.querySelectorAll('.md-preview-failed')).toHaveLength(0);
    mounted.unmount();
  });

  it('切回源码再进入预览：失败边界重新挂载，该侧重试渲染（渲染尝试记录为证）', async () => {
    const diff = makeMdDiff({ oldContent: '@FAIL_OLD@\n' });
    const { container, unmount } = renderViewer(diff);
    enterPreview(container);
    await until(() => paneFailed(container, 0));
    const attemptsAfterFirstEnter = mockRenderLog.filter((c) => c.includes('@FAIL_OLD@')).length;
    expect(attemptsAfterFirstEnter).toBeGreaterThanOrEqual(1);

    // 退出预览（源码渲染正常）再进入：旧侧重新尝试渲染（仍失败 → 提示重现，但这是新一次尝试）
    togglePreview(container); // 退出
    await until(() => container.querySelector('.cm-editor') !== null);
    enterPreview(container); // 再次进入
    await until(
      () => mockRenderLog.filter((c) => c.includes('@FAIL_OLD@')).length > attemptsAfterFirstEnter,
    );
    expect(paneFailed(container, 0)).toBe(true);
    unmount();
  });

  it('切换文件（重挂载）后失败状态不残留', async () => {
    const first = renderViewer(makeMdDiff({ oldContent: '@FAIL_OLD@\n' }));
    enterPreview(first.container);
    await until(() => paneFailed(first.container, 0));
    first.unmount();

    // 切换文件 = DiffViewer 以新 key 重挂载：无失败残留，正常渲染
    const second = renderViewer(makeMdDiff({}));
    enterPreview(second.container);
    expect(second.container.querySelectorAll('.md-preview-failed')).toHaveLength(0);
    expect(paneContent(second.container, 0)).toBe('旧侧内容\n');
    second.unmount();
  });

  it('仅新侧内容变化：旧侧失败边界不重挂、不重试（逐侧 reset identity）', async () => {
    const oldContent = '@FAIL_OLD@\n';
    const mounted = renderViewer(makeMdDiff({ oldContent, newContent: '新侧初版\n' }));
    enterPreview(mounted.container);
    await until(() => paneFailed(mounted.container, 0));
    const oldMounts = mockMountLog.filter((c) => c === oldContent).length;
    expect(oldMounts).toBeGreaterThanOrEqual(1);

    // 仅新侧内容变化：旧侧不重挂（mount 次数不变）、失败提示保持；新侧正常更新
    rerender(
      mounted.root,
      renderViewerProps(
        makeMdDiff({ oldContent, newContent: '新侧更新\n' }),
        mounted,
      ),
    );
    await until(() => paneContent(mounted.container, 1) === '新侧更新\n');
    expect(mockMountLog.filter((c) => c === oldContent).length).toBe(oldMounts);
    expect(paneFailed(mounted.container, 0)).toBe(true);
    mounted.unmount();
  });

  it('双侧同时失败互不影响，内容修复后各自恢复', async () => {
    const mounted = renderViewer(
      makeMdDiff({ oldContent: '@FAIL_OLD@\n', newContent: '@FAIL_NEW@\n' }),
    );
    enterPreview(mounted.container);
    await until(() => paneFailed(mounted.container, 0) && paneFailed(mounted.container, 1));
    expect(mounted.container.querySelectorAll('.md-preview-failed')).toHaveLength(2);

    // 仅新侧修复：新侧恢复渲染，旧侧保持失败提示（逐侧隔离）
    rerender(
      mounted.root,
      renderViewerProps(makeMdDiff({ oldContent: '@FAIL_OLD@\n', newContent: '新侧好了\n' }), mounted),
    );
    await until(() => paneContent(mounted.container, 1) === '新侧好了\n');
    expect(paneFailed(mounted.container, 0)).toBe(true);
    expect(paneFailed(mounted.container, 1)).toBe(false);
    mounted.unmount();
  });
});

/** 与 renderViewer 相同的 props（rerender 保持同一组件实例，仅 diff 变化）。 */
function renderViewerProps(
  diff: GitDiffResult,
  mounted: { onModeChange: ReturnType<typeof vi.fn>; onWrapChange: ReturnType<typeof vi.fn> },
) {
  return (
    <DiffViewer
      diff={diff}
      path="a.md"
      modeOverride="unified"
      onModeChange={mounted.onModeChange}
      wrapOverride={false}
      onWrapChange={mounted.onWrapChange}
    />
  );
}
