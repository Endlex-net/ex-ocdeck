// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act } from 'react';
import DiffViewer, { type DiffViewMode } from '../components/diff/DiffViewer';
import { MergeView } from '@codemirror/merge';
import { EditorView } from '@codemirror/view';
import type { GitDiffResult } from '../types';
import { flushUI, mount, rerender, stubMatchMedia } from './cm-test-env';

/* ============================ DiffViewer（tasks 4.4，design D6 / spec「diff 视图渲染」） ============================
 * 全部用例真实经过组件渲染路径（jsdom + 真实 CodeMirror 实例）。 */

function makeDiff(over: Partial<GitDiffResult>): GitDiffResult {
  return {
    oldContent: 'const a = 1;\n',
    newContent: 'const a = 2;\n',
    oldExists: true,
    newExists: true,
    oldMode: '100644',
    newMode: '100644',
    isBinary: false,
    truncated: false,
    ...over,
  };
}

/** 等待条件成立（编辑器在 async effect 中创建，动态 import 跨宏任务）。 */
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

function renderDiff(
  diff: GitDiffResult,
  modeOverride: DiffViewMode | null = 'unified',
  wrapOverride = false,
) {
  const onModeChange = vi.fn();
  const onWrapChange = vi.fn();
  const mounted = mount(
    <DiffViewer
      diff={diff}
      path="a.txt"
      modeOverride={modeOverride}
      onModeChange={onModeChange}
      wrapOverride={wrapOverride}
      onWrapChange={onWrapChange}
    />,
  );
  return { ...mounted, onModeChange, onWrapChange };
}

describe('渲染优先级完整链（spec「文件 diff 查看」派生规则，I1 修订）', () => {
  beforeEach(() => {
    stubMatchMedia(false); // 宽屏默认并排（避免 jsdom 无 matchMedia 报错）
  });

  const cases: Array<{
    name: string;
    diff: GitDiffResult;
    message?: string;
    merge?: boolean;
    gitlink?: boolean;
    modeBanner?: boolean;
    notText?: string;
  }> = [
    {
      name: 'isBinary 优先于一切：二进制提示、不渲染 merge 视图',
      diff: makeDiff({ isBinary: true, oldContent: '', newContent: '' }),
      message: '二进制文件，暂不支持查看 diff。',
    },
    {
      name: 'isBinary + truncated 共存：截断横幅与二进制提示同时出现',
      diff: makeDiff({ isBinary: true, truncated: true, oldContent: '', newContent: '' }),
      message: '二进制文件，暂不支持查看 diff。',
    },
    {
      name: 'isBinary + mode 变更共存：二进制提示与 mode 横幅同时出现',
      diff: makeDiff({ isBinary: true, oldContent: '', newContent: '', newMode: '100755' }),
      message: '二进制文件，暂不支持查看 diff。',
      modeBanner: true,
    },
    {
      name: 'gitlink OID 变更（两侧 160000）：子模块提示含两侧 OID 文本，不渲染 merge',
      diff: makeDiff({
        oldContent: 'aaaaaaaa11111111',
        newContent: 'bbbbbbbb22222222',
        oldMode: '160000',
        newMode: '160000',
      }),
      gitlink: true,
    },
    {
      name: 'gitlink dirty 子模块（新侧 OID 带 -dirty 后缀，I6）：提示原样展示 dirty OID 文本，不落无变更',
      diff: makeDiff({
        oldContent: 'aaaaaaaa11111111',
        newContent: 'aaaaaaaa11111111-dirty',
        oldMode: '160000',
        newMode: '160000',
      }),
      gitlink: true,
      notText: '内容无变化',
    },
    {
      name: 'gitlink 新增（旧侧不存在、新侧 160000）：子模块提示，不落「文件已不存在」',
      diff: makeDiff({
        oldExists: false,
        oldContent: '',
        oldMode: '',
        newContent: 'cccccccc33333333',
        newMode: '160000',
      }),
      gitlink: true,
      notText: '文件已不存在',
    },
    {
      name: '双侧不存在：文件已不存在',
      diff: makeDiff({ oldExists: false, newExists: false, oldContent: '', oldMode: '', newMode: '' }),
      message: '文件已不存在。',
    },
    {
      name: 'untracked 空文件（旧侧不存在 + 新侧为空）：空文件状态',
      diff: makeDiff({ oldExists: false, oldContent: '', oldMode: '', newContent: '' }),
      message: '空文件。',
    },
    {
      name: '已删除空文件（旧侧存在、内容为空）：空文件而非无变更',
      diff: makeDiff({ newExists: false, newContent: '', newMode: '', oldContent: '' }),
      message: '空文件。',
    },
    {
      name: '双侧均存在、内容与 mode 均相同、未截断：无变更',
      diff: makeDiff({ newContent: 'const a = 1;\n' }),
      message: '内容无变化。',
    },
    {
      name: 'chmod-only（内容相同、100644→100755）：mode 横幅 + merge 视图，MUST NOT 显示无变更',
      diff: makeDiff({ newContent: 'const a = 1;\n', newMode: '100755' }),
      merge: true,
      modeBanner: true,
      notText: '内容无变化',
    },
    {
      name: 'chmod 空内容（两侧存在、内容均空、mode 不同）：不落空文件，merge + 横幅',
      diff: makeDiff({ oldContent: '', newContent: '', newMode: '100755' }),
      merge: true,
      modeBanner: true,
      notText: '空文件',
    },
    {
      name: '截断前缀相同 + mode 相同：截断范围内无可见差异（不显示无变更）',
      diff: makeDiff({ newContent: 'const a = 1;\n', truncated: true }),
      message: '截断范围内无可见差异。',
    },
    {
      name: '截断前缀相同 + mode 不同：截断提示与 mode 横幅共存',
      diff: makeDiff({ newContent: 'const a = 1;\n', truncated: true, newMode: '100755' }),
      message: '截断范围内无可见差异。',
      modeBanner: true,
    },
    {
      name: 'symlink 目标变更（两侧 120000）：merge 视图渲染目标差异',
      diff: makeDiff({
        oldContent: '../old/target\n',
        newContent: '../new/target\n',
        oldMode: '120000',
        newMode: '120000',
      }),
      merge: true,
    },
    {
      name: '全部新增（旧侧不存在）:merge 视图',
      diff: makeDiff({ oldExists: false, oldContent: '', oldMode: '' }),
      merge: true,
    },
    {
      name: '全部删除（新侧不存在）:merge 视图',
      diff: makeDiff({ newExists: false, newContent: '', newMode: '' }),
      merge: true,
    },
    {
      name: '常规两侧差异：merge 视图',
      diff: makeDiff({}),
      merge: true,
    },
    {
      name: '截断 + merge 视图共存：横幅与编辑器同时出现',
      diff: makeDiff({ truncated: true, newContent: 'const a = 3;\n' }),
      merge: true,
    },
  ];

  for (const c of cases) {
    it(c.name, async () => {
      const { container, unmount } = renderDiff(c.diff);
      await flushUI();
      if (c.message) {
        expect(container.textContent).toContain(c.message);
        expect(container.querySelectorAll('.cm-editor')).toHaveLength(0);
      }
      if (c.gitlink) {
        expect(container.textContent).toContain('子模块（gitlink）变更');
        expect(container.querySelectorAll('.cm-editor')).toHaveLength(0);
        // 两侧 OID 文本均展示（spec：子模块提示含两侧 OID）
        if (c.diff.oldContent) expect(container.textContent).toContain(c.diff.oldContent);
        if (c.diff.newContent) expect(container.textContent).toContain(c.diff.newContent);
      }
      if (c.merge) {
        expect(container.querySelectorAll('.cm-editor').length).toBeGreaterThan(0);
      }
      if (c.modeBanner) {
        expect(container.textContent).toContain('权限/类型变更');
        expect(container.textContent).toContain(`${c.diff.oldMode} → ${c.diff.newMode}`);
      }
      if (c.notText) {
        expect(container.textContent).not.toContain(c.notText);
      }
      // 截断横幅独立显示，可与任一状态提示或 merge 视图同时出现
      expect(container.textContent?.includes('内容过大，已被服务端截断。')).toBe(c.diff.truncated);
      unmount();
    });
  }
});

describe('行尾方向性（CRLF↔LF 差异必须可见，spec「diff 视图渲染」）', () => {
  beforeEach(() => {
    stubMatchMedia(false);
  });

  const directionCases = [
    { name: 'CRLF→LF', old: 'a\r\nb\r\n', neu: 'a\nb\n' },
    { name: 'LF→CRLF', old: 'a\nb\n', neu: 'a\r\nb\r\n' },
    { name: '末尾换行变化', old: 'a\nb', neu: 'a\nb\n' },
  ];
  const modes: DiffViewMode[] = ['side-by-side', 'unified'];

  for (const m of modes) {
    for (const c of directionCases) {
      it(`${c.name}（${m}）呈现可见差异而非「无变更」`, async () => {
        const { container, unmount } = renderDiff(
          makeDiff({ oldContent: c.old, newContent: c.neu }),
          m,
        );
        await flushUI();
        expect(container.textContent).not.toContain('内容无变化');
        // 单列 string original 被 merge 规范化会吞掉 CRLF → 无任何增删标记（design D6 回归锚点）
        const changed =
          container.querySelector('.cm-changedLine') ??
          container.querySelector('.cm-insertedLine') ??
          container.querySelector('.cm-deletedChunk') ??
          container.querySelector('.cm-deletedLine');
        expect(changed).not.toBeNull();
        unmount();
      });
    }
  }
});

describe('语法高亮（syntaxHighlighting(classHighlighter) 生效）', () => {
  beforeEach(() => {
    stubMatchMedia(false);
  });

  it('.ts 文件关键字 token 类与增删 decoration 共存（单列）', async () => {
    const onModeChange = vi.fn();
    const { container, unmount } = mount(
      <DiffViewer
        diff={makeDiff({
          oldContent: 'const a = 1;\nlet b = 2;\n',
          newContent: 'const a = 1;\nlet c = 3;\n',
        })}
        path="src/main.ts"
        modeOverride="unified"
        onModeChange={onModeChange}
        wrapOverride={false}
        onWrapChange={vi.fn()}
      />,
    );
    await until(() => container.querySelector('.tok-keyword') !== null);
    expect(container.querySelector('.cm-insertedLine, .cm-deletedChunk')).not.toBeNull();
    unmount();
  });

  it('未识别扩展名纯文本渲染、无报错（.txt 无 token 类）', async () => {
    const { container, unmount } = renderDiff(makeDiff({}), 'unified');
    await flushUI();
    expect(container.querySelectorAll('.cm-editor').length).toBeGreaterThan(0);
    expect(container.querySelector('.tok-keyword')).toBeNull();
    unmount();
  });
});

describe('形态默认值与 modeOverride（design D6/DR1）', () => {
  it('宽屏（>1024px）默认并排：两个编辑器', async () => {
    stubMatchMedia(false);
    const { container, unmount } = renderDiff(makeDiff({}), null);
    await until(() => container.querySelectorAll('.cm-editor').length === 2);
    unmount();
  });

  it('窄屏（≤1024px）默认单列：一个编辑器', async () => {
    stubMatchMedia(true);
    const { container, unmount } = renderDiff(makeDiff({}), null);
    await until(() => container.querySelectorAll('.cm-editor').length === 1);
    unmount();
  });

  it('modeOverride 优先于视口默认（窄屏 + override=side-by-side → 并排）', async () => {
    stubMatchMedia(true);
    const { container, unmount } = renderDiff(makeDiff({}), 'side-by-side');
    await until(() => container.querySelectorAll('.cm-editor').length === 2);
    unmount();
  });

  it('形态切换控件：aria-pressed 反映当前形态，点击回调目标形态', async () => {
    stubMatchMedia(false);
    const { container, onModeChange, unmount } = renderDiff(makeDiff({}), null);
    await until(() => container.querySelectorAll('.cm-editor').length === 2);
    const unifiedBtn = [...container.querySelectorAll('button')].find((b) =>
      b.textContent?.includes('单列'),
    );
    const sideBtn = [...container.querySelectorAll('button')].find((b) =>
      b.textContent?.includes('并排'),
    );
    expect(unifiedBtn?.getAttribute('aria-pressed')).toBe('false');
    expect(sideBtn?.getAttribute('aria-pressed')).toBe('true');
    act(() => unifiedBtn?.click());
    expect(onModeChange).toHaveBeenCalledWith('unified');
    act(() => sideBtn?.click());
    expect(onModeChange).toHaveBeenCalledWith('side-by-side');
    unmount();
  });

  it('状态提示分支不渲染形态切换控件（无变更时无按钮）', async () => {
    stubMatchMedia(false);
    const { container, unmount } = renderDiff(
      makeDiff({ newContent: 'const a = 1;\n' }),
      null,
    );
    await flushUI();
    expect(container.querySelector('button')).toBeNull();
    unmount();
  });
});

describe('换行开关（design D6 换行开关 bullet，spec「切换换行展示」）', () => {
  beforeEach(() => {
    stubMatchMedia(false);
  });

  it('默认关：两种形态均无 .cm-lineWrapping（横向滚动）', async () => {
    for (const mode of ['side-by-side', 'unified'] as const) {
      const { container, unmount } = renderDiff(makeDiff({}), mode);
      await until(() => container.querySelectorAll('.cm-editor').length > 0);
      expect(container.querySelectorAll('.cm-content.cm-lineWrapping')).toHaveLength(0);
      unmount();
    }
  });

  it('开启后单列编辑器折行：.cm-lineWrapping 存在', async () => {
    const { container, unmount } = renderDiff(makeDiff({}), 'unified', true);
    await until(() => container.querySelectorAll('.cm-editor').length === 1);
    expect(container.querySelectorAll('.cm-content.cm-lineWrapping')).toHaveLength(1);
    unmount();
  });

  it('开启后并排 a/b 两侧编辑器均折行', async () => {
    const { container, unmount } = renderDiff(makeDiff({}), 'side-by-side', true);
    await until(() => container.querySelectorAll('.cm-editor').length === 2);
    expect(container.querySelectorAll('.cm-content.cm-lineWrapping')).toHaveLength(2);
    unmount();
  });

  it('换行按钮 aria-pressed 反映开关状态，点击回调目标值', async () => {
    const { container, onWrapChange, unmount } = renderDiff(makeDiff({}), 'unified');
    await until(() => container.querySelectorAll('.cm-editor').length === 1);
    const wrapBtn = [...container.querySelectorAll<HTMLButtonElement>('.diff-toolbar button')].find(
      (b) => b.textContent?.includes('换行'),
    );
    expect(wrapBtn?.getAttribute('aria-pressed')).toBe('false');
    act(() => wrapBtn!.click());
    expect(onWrapChange).toHaveBeenCalledWith(true);
    unmount();
  });

  it('切换走销毁-重建路径：wrap 变化触发 destroy 并按新值重建', async () => {
    const mergeDestroy = vi.spyOn(MergeView.prototype, 'destroy');
    const diff = makeDiff({});
    const onModeChange = vi.fn();
    const { container, root, unmount } = mount(
      <DiffViewer
        diff={diff}
        path="a.txt"
        modeOverride="side-by-side"
        onModeChange={onModeChange}
        wrapOverride={false}
        onWrapChange={vi.fn()}
      />,
    );
    await until(() => container.querySelectorAll('.cm-editor').length === 2);
    expect(container.querySelectorAll('.cm-content.cm-lineWrapping')).toHaveLength(0);
    rerender(
      root,
      <DiffViewer
        diff={diff}
        path="a.txt"
        modeOverride="side-by-side"
        onModeChange={onModeChange}
        wrapOverride
        onWrapChange={vi.fn()}
      />,
    );
    await until(() => container.querySelectorAll('.cm-content.cm-lineWrapping').length === 2);
    expect(mergeDestroy).toHaveBeenCalledTimes(1); // 与形态切换同一销毁-重建路径
    mergeDestroy.mockRestore();
    unmount();
  });
});

describe('挂载 / 只读 / destroy 生命周期', () => {
  beforeEach(() => {
    stubMatchMedia(false);
  });

  it('挂载成功且只读：contenteditable=false（两个形态）', async () => {
    for (const mode of ['side-by-side', 'unified'] as const) {
      const { container, unmount } = renderDiff(makeDiff({}), mode);
      await until(() => container.querySelectorAll('.cm-content').length > 0);
      for (const content of container.querySelectorAll('.cm-content')) {
        expect(content.getAttribute('contenteditable')).toBe('false');
      }
      unmount();
    }
  });

  it('卸载调用 destroy() 并移除编辑器 DOM', async () => {
    const mergeDestroy = vi.spyOn(MergeView.prototype, 'destroy');
    const editorDestroy = vi.spyOn(EditorView.prototype, 'destroy');
    const { container, unmount } = renderDiff(makeDiff({}), 'side-by-side');
    await until(() => container.querySelectorAll('.cm-editor').length === 2);
    expect(mergeDestroy).not.toHaveBeenCalled();
    unmount();
    expect(mergeDestroy).toHaveBeenCalledTimes(1);
    expect(editorDestroy).toHaveBeenCalled(); // MergeView.destroy 释放两侧编辑器
    expect(container.querySelector('.cm-editor')).toBeNull();
    mergeDestroy.mockRestore();
    editorDestroy.mockRestore();
  });

  it('形态切换销毁旧实例并重建（并排 → 单列）', async () => {
    stubMatchMedia(false);
    const mergeDestroy = vi.spyOn(MergeView.prototype, 'destroy');
    const onModeChange = vi.fn();
    const diff = makeDiff({});
    const { container, root, unmount } = mount(
      <DiffViewer
        diff={diff}
        path="a.txt"
        modeOverride={null}
        onModeChange={onModeChange}
        wrapOverride={false}
        onWrapChange={vi.fn()}
      />,
    );
    await until(() => container.querySelectorAll('.cm-editor').length === 2);
    rerender(
      root,
      <DiffViewer
        diff={diff}
        path="a.txt"
        modeOverride="unified"
        onModeChange={onModeChange}
        wrapOverride={false}
        onWrapChange={vi.fn()}
      />,
    );
    await until(() => container.querySelectorAll('.cm-editor').length === 1);
    expect(mergeDestroy).toHaveBeenCalledTimes(1);
    expect(container.querySelector('.cm-mergeView')).toBeNull();
    mergeDestroy.mockRestore();
    unmount();
  });
});
