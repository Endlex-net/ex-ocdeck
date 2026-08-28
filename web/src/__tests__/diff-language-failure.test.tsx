// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act } from 'react';
import DiffViewer from '../components/diff/DiffViewer';
import { loadLanguage } from '../components/editor/language';
import type { GitDiffResult } from '../types';
import { mount, stubMatchMedia } from './cm-test-env';

/* ============================ 语言 chunk 加载失败降级（tasks 6.4，I3） ============================
 * 模拟语言包动态加载失败（如网络中断）：loader rejection MUST 被捕获并降级纯文本，
 * 不产生未处理 rejection、不渲染空白 diff。 */

// 动态 import('@codemirror/lang-markdown') 在 factory 抛错时 reject
vi.mock('@codemirror/lang-markdown', () => {
  throw new Error('chunk load failed');
});

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

describe('语言 chunk 加载失败降级纯文本（I3）', () => {
  beforeEach(() => {
    stubMatchMedia(false);
  });

  it('loadLanguage 捕获 loader rejection 返回 null（不向上抛）', async () => {
    await expect(loadLanguage('a.md')).resolves.toBeNull();
  });

  it('DiffViewer 渲染路径：loader 失败仍渲染纯文本编辑器、内容可见', async () => {
    const onModeChange = vi.fn();
    const diff: GitDiffResult = {
      oldContent: '# old\n',
      newContent: '# new\n',
      oldExists: true,
      newExists: true,
      oldMode: '100644',
      newMode: '100644',
      isBinary: false,
      truncated: false,
    };
    const { container, unmount } = mount(
      <DiffViewer
        diff={diff}
        path="README.md"
        modeOverride="unified"
        onModeChange={onModeChange}
        wrapOverride={false}
        onWrapChange={vi.fn()}
      />,
    );
    await until(() => container.querySelectorAll('.cm-editor').length === 1);
    expect(container.textContent).toContain('# new');
    expect(container.querySelector('.tok-heading')).toBeNull(); // 未走高亮
    unmount();
  });
});
