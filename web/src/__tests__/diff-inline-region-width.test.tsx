// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act } from 'react';
import { readFileSync } from 'node:fs';
import { EditorView } from '@codemirror/view';
import DiffViewer from '../components/diff/DiffViewer';
import { syncInlineHostWidth } from '../components/diff/annotation-ext';
import type { GitDiffResult } from '../types';
import { flushUI, mount, stubMatchMedia } from './cm-test-env';

/* ============================ 内联批注区不换行长行修复 ============================
 * 根因：.ann-inline-host 嵌在 .cm-content 内容流内，块级宽度跟随内容宽度（不换行 = 最长行），
 * 横向滚动把卡片右缘（取消/发布按钮）推出视口。
 * 修复：syncInlineHostWidth 把宿主宽度钉到 scroller 可视宽度 + CSS sticky left:0。
 * jsdom 无真实布局——这里做结构/量测逻辑断言；真实浏览器布局见 agent-browser 验证。 */

function makeDiff(over: Partial<GitDiffResult>): GitDiffResult {
  return {
    oldContent: 'const a = 1;\nconst b = 2;\nconst c = 3;\n',
    newContent: 'const a = 1;\nconst b = 22;\nconst c = 3;\nconst d = 4;\n',
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

/** 模拟横向滚动几何：scroller 可视宽 800，gutters 静态偏移 40，已向右滚 300。 */
function stubScrollerGeometry(view: EditorView) {
  const scroller = view.scrollDOM;
  const content = view.contentDOM;
  const rect = (left: number, width: number) =>
    ({
      left,
      top: 0,
      right: left + width,
      bottom: 600,
      width,
      height: 600,
      x: left,
      y: 0,
      toJSON: () => ({}),
    }) as DOMRect;
  const spies = [
    vi.spyOn(scroller, 'getBoundingClientRect').mockReturnValue(rect(0, 800)),
    // 渲染 rect 已含 scrollLeft 平移：静态 40 - 滚动 300 = -260
    vi.spyOn(content, 'getBoundingClientRect').mockReturnValue(rect(-260, 4000)),
  ];
  Object.defineProperty(scroller, 'clientWidth', { configurable: true, value: 800 });
  scroller.scrollLeft = 300;
  return () => spies.forEach((s) => s.mockRestore());
}

describe('syncInlineHostWidth（宿主宽度钉 scroller 可视宽）', () => {
  it('宽度 = scroller.clientWidth - content 静态左偏移（gutters）', () => {
    const view = new EditorView({ doc: 'l1\nl2\nl3\n' });
    const restore = stubScrollerGeometry(view);
    try {
      const host = document.createElement('div');
      syncInlineHostWidth(view, host);
      // 800 - (-260 - 0 + 300) = 800 - 40 = 760；sticky left = gutters 静态偏移 40
      // （钉在内容区左缘，避免横向滚动时卡片滑到 sticky gutters 底下）
      expect(host.style.width).toBe('760px');
      expect(host.style.left).toBe('40px');
    } finally {
      restore();
      view.destroy();
    }
  });

  it('jsdom 零布局（clientWidth=0）→ 不写内联宽度/偏移，回退默认块级行为', () => {
    const view = new EditorView({ doc: 'l1\nl2\n' });
    const host = document.createElement('div');
    host.style.width = '123px';
    host.style.left = '4px';
    syncInlineHostWidth(view, host);
    expect(host.style.width).toBe('');
    expect(host.style.left).toBe('');
    view.destroy();
  });
});

describe('内联批注区宿主样式（不换行长行）', () => {
  beforeEach(() => stubMatchMedia(false));

  it('CSS：.ann-inline-host 以 sticky left:0 钉住可视区左缘', () => {
    const css = readFileSync('src/legacy-components.css', 'utf8');
    const block = css.match(/\.ann-inline-host\s*\{([^}]*)\}/);
    expect(block, '.ann-inline-host 规则存在').toBeTruthy();
    expect(block![1]).toMatch(/position:\s*sticky/);
    expect(block![1]).toMatch(/left:\s*0/);
    expect(block![1]).toMatch(/box-sizing:\s*border-box/);
  });

  it('DiffViewer 集成：打开内联批注区 → 宿主写入可视宽度；关闭 → 清理内联宽度', async () => {
    const onCreateAnnotation = vi.fn(async () => {});
    const { container, unmount } = mount(
      <DiffViewer
        diff={makeDiff({})}
        path="a.ts"
        sourceRef=""
        untracked={false}
        annotations={[]}
        onCreateAnnotation={onCreateAnnotation}
        onLocateAnnotations={() => {}}
        modeOverride="unified"
        onModeChange={() => {}}
        wrapOverride={false}
        onWrapChange={() => {}}
      />,
    );
    await flushUI();
    await until(() => container.querySelectorAll('.cm-content').length === 1);
    const content = container.querySelector('.cm-content') as HTMLElement;
    const view = EditorView.findFromDOM(content)!;
    expect(view).toBeTruthy();

    const restore = stubScrollerGeometry(view);
    try {
      act(() => {
        view.dispatch({
          selection: { anchor: 0, head: view.state.doc.line(1).to },
        });
        container
          .querySelector('.cm-line')!
          .dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
      });
      const host = container.querySelector<HTMLElement>('.ann-inline-host');
      expect(host, 'block widget 宿主挂在 .cm-content 内').toBeTruthy();
      expect(host!.closest('.cm-content')).toBeTruthy();
      expect(host!.querySelector('.ann-inline')).toBeTruthy();
      // 量测生效：宿主宽度 = 可视宽 800 - gutters 40 = 760（内容宽 4000 下仍钉在可视宽），
      // sticky left = 40 钉在内容区左缘
      expect(host!.style.width).toBe('760px');
      expect(host!.style.left).toBe('40px');

      // 关闭（Esc）→ effect 清理内联宽度/偏移
      act(() => {
        host!
          .querySelector('.ann-inline')!
          .dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
      });
      expect(container.querySelector('.ann-inline')).toBeNull();
      expect(host!.style.width).toBe('');
      expect(host!.style.left).toBe('');
    } finally {
      restore();
      unmount();
    }
  });

  it('换行开启（wrapOverride）布局不变：宿主结构与 sticky 类名一致、零布局下无内联宽度', async () => {
    const onCreateAnnotation = vi.fn(async () => {});
    const { container, unmount } = mount(
      <DiffViewer
        diff={makeDiff({})}
        path="a.ts"
        sourceRef=""
        untracked={false}
        annotations={[]}
        onCreateAnnotation={onCreateAnnotation}
        onLocateAnnotations={() => {}}
        modeOverride="unified"
        onModeChange={() => {}}
        wrapOverride
        onWrapChange={() => {}}
      />,
    );
    await flushUI();
    await until(() => container.querySelectorAll('.cm-content.cm-lineWrapping').length === 1);
    const content = container.querySelector('.cm-content') as HTMLElement;
    const view = EditorView.findFromDOM(content)!;
    act(() => {
      view.dispatch({
        selection: { anchor: 0, head: view.state.doc.line(1).to },
      });
      container
        .querySelector('.cm-line')!
        .dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
    });
    const host = container.querySelector<HTMLElement>('.ann-inline-host');
    expect(host).toBeTruthy();
    expect(host!.querySelector('.ann-inline')).toBeTruthy();
    // jsdom 零布局：量到 0 不写内联宽度/偏移——换行场景维持默认块级宽度（= 可视宽）
    expect(host!.style.width).toBe('');
    expect(host!.style.left).toBe('');
    unmount();
  });
});
