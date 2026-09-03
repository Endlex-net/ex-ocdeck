// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act, useState } from 'react';
import { readFileSync } from 'node:fs';
import { EditorView } from '@codemirror/view';
import DiffViewer from '../components/diff/DiffViewer';
import { ApiError } from '../api';
import type {
  Annotation,
  AnnotationCreateInput,
  FileEditRead,
  FileEditWriteInput,
  GitDiffResult,
} from '../types';
import { flushUI, mount, stubMatchMedia } from './cm-test-env';

/* ============================ DiffViewer 批注手势 + 编辑模式（tasks 5.2/5.3，5.6 交互测试） ============================
 * 真实 CodeMirror 实例（jsdom）：框选/点击手势、快照构造、标记聚合与三元组隔离、编辑门禁与写回。 */

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

function makeAnn(over: Partial<Annotation>): Annotation {
  return {
    id: 'ann-1',
    path: 'a.ts',
    side: 'new',
    ref: '',
    untracked: false,
    startLine: 2,
    endLine: 2,
    snapshotStartLine: 1,
    snapshotLineCount: 4,
    snapshot: '',
    comment: '评论一',
    revision: 1,
    stale: false,
    createdAt: 100,
    updatedAt: 100,
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

function editorViews(container: HTMLElement): EditorView[] {
  return [...container.querySelectorAll('.cm-content')]
    .map((el) => EditorView.findFromDOM(el as HTMLElement))
    .filter((v): v is EditorView => v !== null && v !== undefined);
}

function setNativeValue(el: HTMLTextAreaElement, value: string) {
  const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!;
  act(() => {
    setter.call(el, value);
    el.dispatchEvent(new Event('input', { bubbles: true }));
  });
}

function renderViewer(over: {
  diff?: GitDiffResult;
  annotations?: Annotation[];
  onCreateAnnotation?: ReturnType<typeof vi.fn>;
  onLocateAnnotations?: ReturnType<typeof vi.fn>;
  onRefreshDiff?: () => Promise<void>;
  editIO?: { read: ReturnType<typeof vi.fn>; write: ReturnType<typeof vi.fn> };
  agentBusy?: boolean;
  /** undefined = 测试缺省 unified；null = 无覆盖（走组件默认形态选择）；'side-by-side' = 并排。 */
  modeOverride?: 'unified' | 'side-by-side' | null;
}) {
  const onModeChange = vi.fn();
  const onWrapChange = vi.fn();
  const mounted = mount(
    <DiffViewer
      diff={over.diff ?? makeDiff({})}
      path="a.ts"
      sourceRef=""
      untracked={false}
      annotations={over.annotations ?? []}
      agentBusy={over.agentBusy ?? false}
      onCreateAnnotation={over.onCreateAnnotation ?? (async () => {})}
      onLocateAnnotations={over.onLocateAnnotations ?? (() => {})}
      onRefreshDiff={over.onRefreshDiff}
      editIO={over.editIO}
      modeOverride={over.modeOverride === undefined ? 'unified' : over.modeOverride}
      onModeChange={onModeChange}
      wrapOverride={false}
      onWrapChange={onWrapChange}
    />,
  );
  return { ...mounted, onModeChange, onWrapChange };
}

async function submitDraftWith(container: HTMLElement, comment: string) {
  const ta = container.querySelector<HTMLTextAreaElement>('.ann-inline-input');
  expect(ta, '内联批注区已打开').toBeTruthy();
  if (comment) setNativeValue(ta!, comment);
  const btn = [...container.querySelectorAll<HTMLButtonElement>('.ann-inline-actions button')].find(
    (b) => b.textContent?.includes('发布评论'),
  )!;
  await act(async () => {
    btn.click();
    await Promise.resolve();
  });
}

/** F7：等待资格预取完成、编辑命令出现（eligible）。 */
async function waitEditEntry(container: HTMLElement) {
  await until(() =>
    [...container.querySelectorAll<HTMLButtonElement>('button')].some(
      (b) => b.textContent === '编辑' && !b.disabled,
    ),
  );
}

/** 点击编辑命令进入编辑模式（含资格等待）。 */
async function clickEdit(container: HTMLElement) {
  await waitEditEntry(container);
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

describe('查看模式批注手势（框选/点击 + 快照构造）', () => {
  beforeEach(() => stubMatchMedia(false));

  it('unified 拖选新侧行 → 内联批注区 → 提交：side=new、行范围、快照取自原始侧内容并保留 \\r', async () => {
    const onCreateAnnotation = vi.fn(async (_input: AnnotationCreateInput) => {});
    const { container, unmount } = renderViewer({
      diff: makeDiff({
        newContent: 'l1\r\nl2\r\nl3\r\nl4\r\nl5\r\nl6\r\nl7\r\nl8\r\n',
      }),
      onCreateAnnotation,
    });
    await until(() => editorViews(container).length === 1);
    const view = editorViews(container)[0];
    // 拖选 L3..L4
    act(() => {
      view.dispatch({
        selection: { anchor: view.state.doc.line(3).from, head: view.state.doc.line(4).to },
      });
      // mouseup 落在真实内容行（F16 白名单）
      container
        .querySelectorAll('.cm-line')[3]
        .dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
    });
    expect(container.querySelector('.ann-inline')).toBeTruthy();
    // 批注 6：内联批注区嵌在编辑器内容里（block widget），不是悬浮浮层
    expect(container.querySelector('.cm-content .ann-inline')).toBeTruthy();
    expect(container.textContent).toContain('发布评论');
    expect(container.textContent).toContain('第 3-4 行');
    // 候选选区高亮
    expect(container.querySelector('.cm-annotationCandidate')).toBeTruthy();
    await submitDraftWith(container, '这里改成 22 有问题');
    expect(onCreateAnnotation).toHaveBeenCalledTimes(1);
    const input = onCreateAnnotation.mock.calls[0][0];
    expect(input).toMatchObject({
      path: 'a.ts',
      side: 'new',
      ref: '',
      untracked: false,
      startLine: 3,
      endLine: 4,
      comment: '这里改成 22 有问题',
    });
    // 窗口 ±3 行、保留 \r
    expect(input.snapshot).toBe('l1\r\nl2\r\nl3\r\nl4\r\nl5\r\nl6\r\nl7\r');
    expect(input.snapshotStartLine).toBe(1);
    expect(input.snapshotLineCount).toBe(7);
    // 提交后内联批注区移除、候选高亮清除
    expect(container.querySelector('.ann-inline')).toBeNull();
    expect(container.querySelector('.cm-annotationCandidate')).toBeNull();
    unmount();
  });

  it('空评论提交 = 丢弃，不调用创建', async () => {
    const onCreateAnnotation = vi.fn(async (_input: AnnotationCreateInput) => {});
    const { container, unmount } = renderViewer({ onCreateAnnotation });
    await until(() => editorViews(container).length === 1);
    const view = editorViews(container)[0];
    act(() => {
      view.dispatch({ selection: { anchor: 0, head: view.state.doc.line(1).to } });
      container
        .querySelector('.cm-line')!
        .dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
    });
    await submitDraftWith(container, '   ');
    expect(onCreateAnnotation).not.toHaveBeenCalled();
    expect(container.querySelector('.ann-inline')).toBeNull();
    unmount();
  });

  it('批注 4：内联批注区 ⌘/Ctrl+Enter 快捷提交；Esc 关闭；空评论快捷键同样丢弃', async () => {
    const onCreateAnnotation = vi.fn(async (_input: AnnotationCreateInput) => {});
    const { container, unmount } = renderViewer({ onCreateAnnotation });
    await until(() => editorViews(container).length === 1);
    const openDraft = () => {
      const view = editorViews(container)[0];
      act(() => {
        view.dispatch({ selection: { anchor: 0, head: view.state.doc.line(1).to } });
        container
          .querySelector('.cm-line')!
          .dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
      });
      expect(container.querySelector('.ann-inline')).toBeTruthy();
      return container.querySelector<HTMLTextAreaElement>('.ann-inline-input')!;
    };
    // 空评论 + ⌘Enter → 丢弃
    let ta = openDraft();
    act(() => {
      ta.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', metaKey: true, bubbles: true }));
    });
    expect(onCreateAnnotation).not.toHaveBeenCalled();
    expect(container.querySelector('.ann-inline')).toBeNull();
    // 有评论 + Esc → 关闭不提交
    ta = openDraft();
    setNativeValue(ta, '按 Esc');
    act(() => {
      ta.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    });
    expect(onCreateAnnotation).not.toHaveBeenCalled();
    expect(container.querySelector('.ann-inline')).toBeNull();
    // 有评论 + Ctrl+Enter → 提交（与点击同语义）
    ta = openDraft();
    setNativeValue(ta, '快捷键评论');
    await act(async () => {
      ta.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', ctrlKey: true, bubbles: true }));
      await Promise.resolve();
    });
    expect(onCreateAnnotation).toHaveBeenCalledTimes(1);
    expect(onCreateAnnotation.mock.calls[0][0].comment).toBe('快捷键评论');
    expect(container.querySelector('.ann-inline')).toBeNull();
    unmount();
  });

  it('并排 a 侧拖选 → side=old、快照取自 oldContent', async () => {
    const onCreateAnnotation = vi.fn(async (_input: AnnotationCreateInput) => {});
    const { container, unmount } = renderViewer({
      modeOverride: 'side-by-side',
      onCreateAnnotation,
    });
    await until(() => editorViews(container).length === 2);
    const aView = editorViews(container)[0];
    act(() => {
      aView.dispatch({
        selection: { anchor: aView.state.doc.line(2).from, head: aView.state.doc.line(2).to },
      });
      container
        .querySelectorAll('.cm-merge-a .cm-line')[1]
        .dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
    });
    expect(container.textContent).toContain('旧侧');
    expect(container.textContent).toContain('第 2 行');
    await submitDraftWith(container, '旧侧评论');
    expect(onCreateAnnotation.mock.calls[0][0]).toMatchObject({
      side: 'old',
      startLine: 2,
      endLine: 2,
    });
    expect(onCreateAnnotation.mock.calls[0][0].snapshot).toContain('const a = 1;');
    unmount();
  });

  it('unified 点击删除行 → side=old（删除行归属旧侧）', async () => {
    const onCreateAnnotation = vi.fn(async (_input: AnnotationCreateInput) => {});
    const { container, unmount } = renderViewer({
      diff: makeDiff({ oldContent: 'keep\ndel1\ndel2\nkeep2\n', newContent: 'keep\nkeep2\n' }),
      onCreateAnnotation,
    });
    await until(() => container.querySelector('.cm-deletedLine') !== null);
    const deleted = container.querySelectorAll('.cm-deletedLine')[1]; // del2
    act(() => {
      deleted.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }));
      deleted.dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
    });
    expect(container.querySelector('.ann-inline')).toBeTruthy();
    expect(container.textContent).toContain('第 3 行');
    unmount();
  });

  it('unified 删除侧拖选多行 → old 侧范围批注', async () => {
    const onCreateAnnotation = vi.fn(async (_input: AnnotationCreateInput) => {});
    const { container, unmount } = renderViewer({
      diff: makeDiff({ oldContent: 'keep\ndel1\ndel2\nkeep2\n', newContent: 'keep\nkeep2\n' }),
      onCreateAnnotation,
    });
    await until(() => container.querySelectorAll('.cm-deletedLine').length === 2);
    const dels = container.querySelectorAll('.cm-deletedLine');
    act(() => {
      dels[0].dispatchEvent(new MouseEvent('mousedown', { bubbles: true })); // del1
      dels[1].dispatchEvent(new MouseEvent('mouseup', { bubbles: true })); // del2
    });
    expect(container.textContent).toContain('第 2-3 行');
    await submitDraftWith(container, '删除的两行都有问题');
    expect(onCreateAnnotation.mock.calls[0][0]).toMatchObject({
      side: 'old',
      startLine: 2,
      endLine: 3,
    });
    unmount();
  });

  it('unified 混合侧选择拒绝：删除块 ↔ 普通内容拖拽，提示且不产生批注', async () => {
    const onCreateAnnotation = vi.fn(async (_input: AnnotationCreateInput) => {});
    const { container, unmount } = renderViewer({
      diff: makeDiff({ oldContent: 'keep\ndel1\nkeep2\n', newContent: 'keep\nkeep2\n' }),
      onCreateAnnotation,
    });
    await until(() => container.querySelector('.cm-deletedLine') !== null);
    const deleted = container.querySelector('.cm-deletedLine')!;
    const line = container.querySelector('.cm-line')!;
    // 旧侧拖到普通内容
    act(() => {
      deleted.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }));
      line.dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
    });
    expect(container.textContent).toContain('选区跨越两侧');
    expect(container.querySelector('.ann-inline')).toBeNull();
    // 普通内容拖到删除块
    act(() => {
      line.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }));
      deleted.dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
    });
    expect(onCreateAnnotation).not.toHaveBeenCalled();
    expect(container.querySelector('.ann-inline')).toBeNull();
    unmount();
  });

  it('单击普通代码行 → 单行批注（unified 新侧；spec 点击行）', async () => {
    const onCreateAnnotation = vi.fn(async (_input: AnnotationCreateInput) => {});
    const { container, unmount } = renderViewer({ onCreateAnnotation });
    await until(() => container.querySelector('.cm-line') !== null);
    const line = container.querySelectorAll('.cm-line')[1]; // 第 2 行
    act(() => {
      line.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }));
      line.dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
    });
    expect(container.querySelector('.ann-inline')).toBeTruthy();
    expect(container.textContent).toContain('第 2 行');
    await submitDraftWith(container, '单击行批注');
    expect(onCreateAnnotation.mock.calls[0][0]).toMatchObject({
      side: 'new',
      startLine: 2,
      endLine: 2,
    });
    unmount();
  });

  it('F14：保留旧选区点击行号 → mousedown 单行批注不被 mouseup 旧选区覆盖', async () => {
    const onCreateAnnotation = vi.fn(async (_input: AnnotationCreateInput) => {});
    const { container, unmount } = renderViewer({ onCreateAnnotation });
    await until(() => editorViews(container).length === 1);
    const view = editorViews(container)[0];
    // 先制造旧选区 L1-L3（不派发 mouseup，模拟历史残留选区）
    act(() => {
      view.dispatch({
        selection: { anchor: view.state.doc.line(1).from, head: view.state.doc.line(3).to },
      });
    });
    const gutterEl = [...container.querySelectorAll('.cm-lineNumbers .cm-gutterElement')].find(
      (el) => el.textContent === '4',
    )!;
    expect(gutterEl).toBeTruthy();
    act(() => {
      gutterEl.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }));
      gutterEl.dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
    });
    // 内联批注区必须是行号单击的 L4 单行，不是旧选区 L1-3/L1-4
    expect(container.textContent).toContain('第 4 行');
    expect(container.textContent).not.toContain('第 1-');
    unmount();
  });

  it('F14：点击批注标记（gutter widget）+ 旧选区 → 只定位不开新批注区', async () => {
    const onCreateAnnotation = vi.fn(async (_input: AnnotationCreateInput) => {});
    const onLocateAnnotations = vi.fn();
    const { container, unmount } = renderViewer({
      annotations: [makeAnn({})],
      onCreateAnnotation,
      onLocateAnnotations,
    });
    await until(() => container.querySelector('.cm-annotationDot') !== null);
    const view = editorViews(container)[0];
    act(() => {
      view.dispatch({
        selection: { anchor: view.state.doc.line(1).from, head: view.state.doc.line(2).to },
      });
    });
    const dot = container.querySelector('.cm-annotationDot')!;
    act(() => {
      dot.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }));
      dot.dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
    });
    expect(onLocateAnnotations).toHaveBeenCalledWith(['ann-1']);
    expect(container.querySelector('.ann-inline')).toBeNull(); // 未被旧选区误开批注
    unmount();
  });

  it('F15：并排 A 侧按下 B 侧释放 → 跨侧提示，不创建批注', async () => {
    const onCreateAnnotation = vi.fn(async (_input: AnnotationCreateInput) => {});
    const { container, unmount } = renderViewer({
      modeOverride: 'side-by-side',
      onCreateAnnotation,
    });
    await until(() => container.querySelectorAll('.cm-line').length > 0);
    const aLine = container.querySelector('.cm-merge-a .cm-line')!;
    const bLine = container.querySelector('.cm-merge-b .cm-line')!;
    expect(aLine).toBeTruthy();
    expect(bLine).toBeTruthy();
    act(() => {
      aLine.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }));
      bLine.dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
    });
    expect(container.textContent).toContain('选区跨越两侧');
    expect(container.querySelector('.ann-inline')).toBeNull();
    expect(onCreateAnnotation).not.toHaveBeenCalled();
    unmount();
  });

  it('F16：点击并排对齐空白区（.cm-mergeSpacer）+ 残留选区 → 零新批注区零创建', async () => {
    const onCreateAnnotation = vi.fn(async (_input: AnnotationCreateInput) => {});
    const { container, unmount } = renderViewer({
      modeOverride: 'side-by-side',
      onCreateAnnotation,
    });
    await until(() => editorViews(container).length === 2);
    await until(() => container.querySelector('.cm-mergeSpacer') !== null);
    const bView = editorViews(container)[1];
    // 制造残留选区
    act(() => {
      bView.dispatch({
        selection: { anchor: bView.state.doc.line(1).from, head: bView.state.doc.line(2).to },
      });
    });
    const spacer = container.querySelector('.cm-mergeSpacer')!;
    act(() => {
      spacer.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }));
      spacer.dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
    });
    expect(container.querySelector('.ann-inline')).toBeNull();
    expect(onCreateAnnotation).not.toHaveBeenCalled();
    unmount();
  });

  it('F16：点击非行内容目标（.cm-content 内非 .cm-line 区域）+ 残留选区 → 不回退', async () => {
    const onCreateAnnotation = vi.fn(async (_input: AnnotationCreateInput) => {});
    const { container, unmount } = renderViewer({ onCreateAnnotation });
    await until(() => editorViews(container).length === 1);
    const view = editorViews(container)[0];
    act(() => {
      view.dispatch({
        selection: { anchor: view.state.doc.line(1).from, head: view.state.doc.line(3).to },
      });
    });
    // contentDOM 本身（非 .cm-line 子元素）按下抬起
    act(() => {
      view.contentDOM.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }));
      view.contentDOM.dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
    });
    expect(container.querySelector('.ann-inline')).toBeNull();
    expect(onCreateAnnotation).not.toHaveBeenCalled();
    unmount();
  });

  it('行号点击 → 单行批注（unified 恒新侧；并排按指针所在侧）', async () => {
    const onCreateAnnotation = vi.fn(async (_input: AnnotationCreateInput) => {});
    const { container, unmount } = renderViewer({ onCreateAnnotation });
    await until(() => container.querySelector('.cm-lineNumbers') !== null);
    const gutterEl = [...container.querySelectorAll('.cm-lineNumbers .cm-gutterElement')].find(
      (el) => el.textContent === '2',
    )!;
    expect(gutterEl).toBeTruthy();
    act(() => {
      gutterEl.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }));
    });
    expect(container.textContent).toContain('第 2 行');
    await submitDraftWith(container, '行号批注');
    expect(onCreateAnnotation.mock.calls[0][0]).toMatchObject({
      side: 'new',
      startLine: 2,
      endLine: 2,
    });
    unmount();

    // 并排 a 侧行号 → side=old
    const onCreate2 = vi.fn(async (_input: AnnotationCreateInput) => {});
    const sb = renderViewer({ modeOverride: 'side-by-side', onCreateAnnotation: onCreate2 });
    await until(() => sb.container.querySelectorAll('.cm-lineNumbers').length === 2);
    const aGutterEl = [
      ...sb.container.querySelectorAll('.cm-merge-a .cm-lineNumbers .cm-gutterElement'),
    ].find((el) => el.textContent === '2')!;
    expect(aGutterEl).toBeTruthy();
    act(() => {
      aGutterEl.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }));
    });
    expect(sb.container.textContent).toContain('第 2 行');
    sb.unmount();
  });
});

describe('行内标记（聚合、悬停摘要、点击定位、三元组隔离）', () => {
  beforeEach(() => stubMatchMedia(false));

  it('同一行多条批注聚合为带数量标记，悬停摘要按 (createdAt,id) 排序，点击定位全部', async () => {
    const onLocateAnnotations = vi.fn();
    const anns = [
      makeAnn({ id: 'b-id', comment: '晚创建', createdAt: 200 }),
      makeAnn({ id: 'a-id', comment: '早创建', createdAt: 100 }),
      // 其他三元组（staged 视图）：不得在本视图显示
      makeAnn({ id: 'other', ref: 'HEAD', startLine: 1, endLine: 1, comment: '串扰' }),
    ];
    const { container, unmount } = renderViewer({
      annotations: anns,
      onLocateAnnotations,
    });
    await until(() => container.querySelector('.cm-annotationDot') !== null);
    const dots = container.querySelectorAll('.cm-annotationDot');
    expect(dots).toHaveLength(1); // 同行聚合为一个标记
    expect(dots[0].textContent).toBe('2');
    const title = dots[0].getAttribute('title') ?? '';
    expect(title.indexOf('早创建')).toBeLessThan(title.indexOf('晚创建'));
    expect(title).not.toContain('串扰');
    act(() => {
      dots[0].dispatchEvent(new MouseEvent('mousedown', { bubbles: true }));
    });
    expect(onLocateAnnotations).toHaveBeenCalledWith(['a-id', 'b-id']);
    unmount();
  });

  it('stale 批注行内标记带漂移样式与悬停前缀', async () => {
    const { container, unmount } = renderViewer({
      annotations: [makeAnn({ stale: true, comment: '会漂移' })],
    });
    await until(() => container.querySelector('.cm-annotationDot') !== null);
    const dot = container.querySelector('.cm-annotationDot')!;
    expect(dot.className).toContain('cm-annotationDot-stale');
    expect(dot.getAttribute('title')).toContain('[已漂移]');
    unmount();
  });

  it('旧侧批注在 unified 视图锚定到删除块所在文档行', async () => {
    const { container, unmount } = renderViewer({
      diff: makeDiff({ oldContent: 'keep\ndel\nkeep2\n', newContent: 'keep\nkeep2\n' }),
      annotations: [makeAnn({ side: 'old', startLine: 2, endLine: 2, comment: '旧侧批注' })],
    });
    await until(() => container.querySelector('.cm-annotationDot') !== null);
    unmount();
  });
});

describe('单侧存在文件：并排空侧坍缩铺满（修正 1：语义仍是并排 MergeView，不是默认切单列）', () => {
  beforeEach(() => stubMatchMedia(false)); // 宽屏：无缺省 override 时默认并排

  const pureNew = () =>
    makeDiff({
      oldExists: false,
      oldContent: '',
      oldMode: '',
      newContent: 'fresh 1\nfresh 2\nfresh 3\n',
      newMode: '100644',
    });
  const pureGone = () =>
    makeDiff({
      oldExists: true,
      oldContent: 'gone 1\ngone 2\n',
      newExists: false,
      newContent: '',
      newMode: '',
    });

  it('纯新文件宽屏无 override：并排双实例 + a 侧坍缩标记（cm-merge-a 存在但被坍缩）', async () => {
    const { container, unmount } = renderViewer({ diff: pureNew(), modeOverride: null });
    // 语义仍是并排 MergeView：两个编辑器实例都建（空侧坍缩为 0 宽不可见，非不渲染）
    await until(() => editorViews(container).length === 2);
    expect(container.querySelector('.cm-merge-a')).not.toBeNull();
    expect(container.querySelector('.cm-merge-b')).not.toBeNull();
    expect(container.querySelector('.diff-collapse-a')).not.toBeNull();
    expect(container.querySelector('.diff-collapse-b')).toBeNull();
    // 空 a 侧文档为空；新侧内容正常呈现
    expect(container.textContent).toContain('fresh 1');
    unmount();
  });

  it('纯删除文件：对称坍缩（b 侧坍缩标记，a 侧铺满）', async () => {
    const { container, unmount } = renderViewer({ diff: pureGone(), modeOverride: null });
    await until(() => editorViews(container).length === 2);
    expect(container.querySelector('.diff-collapse-b')).not.toBeNull();
    expect(container.querySelector('.diff-collapse-a')).toBeNull();
    expect(container.textContent).toContain('gone 1');
    unmount();
  });

  it('CSS：坍缩侧 wrapper 0 宽隐藏（flex:0 0 0 + visibility:hidden），存在侧 flexGrow 铺满', () => {
    const css = readFileSync('src/legacy-components.css', 'utf8');
    const block = css.match(/\.diff-collapse-a[^{]*\{([^}]*)\}/);
    expect(block, 'diff-collapse 规则存在').toBeTruthy();
    expect(block![0]).toContain('.cm-mergeViewEditor');
    expect(block![1]).toMatch(/flex:\s*0 0 0/);
    expect(block![1]).toMatch(/visibility:\s*hidden/);
  });

  it('override 语义：手动切单列=unified 单编辑器无坍缩；手动切并排=坍缩铺满（override 恒优先）', async () => {
    // 用户手动切「单列」→ unifiedMergeView，无坍缩标记
    const uni = renderViewer({ diff: pureNew(), modeOverride: 'unified' });
    await until(() => editorViews(uni.container).length === 1);
    expect(uni.container.querySelector('.cm-merge-b')).not.toBeNull();
    expect(uni.container.querySelector('.cm-merge-a')).toBeNull();
    expect(uni.container.querySelector('.diff-collapse-a')).toBeNull();
    uni.unmount();
    // 用户手动切「并排」→ 单侧不存在时呈现坍缩铺满（不再渲染可见双栏空半）
    const side = renderViewer({ diff: pureNew(), modeOverride: 'side-by-side' });
    await until(() => editorViews(side.container).length === 2);
    expect(side.container.querySelector('.diff-collapse-a')).not.toBeNull();
    side.unmount();
  });

  it('坍缩铺满下批注手势可用（b 侧框选 → side=new、快照取自 newContent）', async () => {
    const onCreateAnnotation = vi.fn(async (_input: AnnotationCreateInput) => {});
    const { container, unmount } = renderViewer({
      diff: pureNew(),
      modeOverride: null,
      onCreateAnnotation,
    });
    await until(() => editorViews(container).length === 2);
    const view = EditorView.findFromDOM(
      container.querySelector<HTMLElement>('.cm-merge-b .cm-content')!,
    )!;
    expect(view).toBeTruthy();
    act(() => {
      view.dispatch({
        selection: { anchor: view.state.doc.line(2).from, head: view.state.doc.line(2).to },
      });
      container
        .querySelectorAll('.cm-merge-b .cm-line')[1]
        .dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
    });
    expect(container.querySelector('.ann-inline')).toBeTruthy();
    await submitDraftWith(container, '新文件批注');
    expect(onCreateAnnotation).toHaveBeenCalledTimes(1);
    expect(onCreateAnnotation.mock.calls[0][0]).toMatchObject({
      side: 'new',
      startLine: 2,
      endLine: 2,
      comment: '新文件批注',
    });
    expect(onCreateAnnotation.mock.calls[0][0].snapshot).toContain('fresh 2');
    unmount();
  });

  it('坍缩铺满下编辑模式可用（资格 eligible → 进入编辑、b 侧内容可写）', async () => {
    const io = { read: vi.fn(), write: vi.fn() };
    io.read.mockResolvedValue({
      editable: true,
      content: 'fresh 1\nfresh 2\nfresh 3\n',
      baseHash: 'h0',
      lineEnding: 'lf',
      hasBom: false,
      mode: '0644',
    } satisfies FileEditRead);
    io.write.mockResolvedValue({ baseHash: 'h1' });
    const { container, unmount } = renderViewer({
      diff: pureNew(),
      modeOverride: null,
      editIO: io,
    });
    await until(() => editorViews(container).length === 2);
    await clickEdit(container);
    // 编辑目标为可写编辑器（b 侧）；坍缩的 a 侧始终只读
    const view = editorViews(container).find(
      (v) => v.contentDOM.getAttribute('contenteditable') === 'true',
    )!;
    expect(view).toBeTruthy();
    act(() => {
      view.dispatch({ changes: { from: view.state.doc.length, insert: 'added\n' } });
    });
    await until(() => io.write.mock.calls.length > 0);
    expect(io.write.mock.calls[0][0].content).toContain('added');
    unmount();
  });
});

describe('编辑入口门禁（tasks 5.3）', () => {
  beforeEach(() => stubMatchMedia(false));

  const editIO = () => ({ read: vi.fn(), write: vi.fn() });

  const gateCases: Array<{ name: string; diff: GitDiffResult; reason: string }> = [
    {
      name: 'truncated',
      diff: makeDiff({ truncated: true, newContent: 'const a = 9;\n' }),
      reason: '截断',
    },
    {
      name: 'symlink',
      diff: makeDiff({ newMode: '120000' }),
      reason: '符号链接',
    },
    {
      name: '新侧缺失（已删除）',
      diff: makeDiff({ newExists: false, newContent: '', newMode: '' }),
      reason: '已删除',
    },
  ];
  for (const c of gateCases) {
    it(`${c.name}：编辑按钮禁用并显示原因`, async () => {
      const io = editIO();
      const { container, unmount } = renderViewer({ diff: c.diff, editIO: io });
      await until(() => container.querySelectorAll('.cm-editor').length > 0);
      const btn = [...container.querySelectorAll<HTMLButtonElement>('button')].find(
        (b) => b.textContent === '编辑',
      );
      expect(btn).toBeTruthy();
      expect(btn!.disabled).toBe(true);
      expect(container.textContent).toContain(c.reason);
      act(() => btn!.click());
      await flushUI();
      expect(io.read).not.toHaveBeenCalled();
      unmount();
    });
  }

  it('binary/gitlink：无 merge 视图，状态提示附带不可编辑原因', async () => {
    const io = editIO();
    const b = renderViewer({ diff: makeDiff({ isBinary: true, oldContent: '', newContent: '' }), editIO: io });
    await flushUI();
    expect(b.container.textContent).toContain('二进制文件');
    expect(b.container.textContent).toContain('不可编辑');
    b.unmount();
    const g = renderViewer({
      diff: makeDiff({ oldMode: '160000', newMode: '160000', oldContent: 'aaaa', newContent: 'bbbb' }),
      editIO: io,
    });
    await flushUI();
    expect(g.container.textContent).toContain('gitlink');
    expect(g.container.textContent).toContain('不可编辑');
    g.unmount();
  });

  it('GET editable=false：预取即判定，不提供编辑命令并展示服务端原因（F7）', async () => {
    const io = editIO();
    const notEditable: FileEditRead = {
      editable: false,
      reasonCode: 'mixed_line_endings',
      reason: '换行风格混杂',
    };
    io.read.mockResolvedValue(notEditable);
    const { container, unmount } = renderViewer({ editIO: io });
    await until(() => container.querySelectorAll('.cm-editor').length > 0);
    // 资格预取自动执行，无需点击即展示原因
    await until(() => container.textContent?.includes('换行风格混杂') ?? false);
    expect(io.read).toHaveBeenCalledTimes(1);
    // 不提供可用的编辑命令
    const hasEnabledEdit = [...container.querySelectorAll<HTMLButtonElement>('button')].some(
      (b) => b.textContent === '编辑' && !b.disabled,
    );
    expect(hasEnabledEdit).toBe(false);
    await flushUI();
    expect(io.read).toHaveBeenCalledTimes(1); // 无重复 GET
    // 仍在查看模式（只读）
    for (const el of container.querySelectorAll('.cm-content')) {
      expect(el.getAttribute('contenteditable')).toBe('false');
    }
    unmount();
  });

  it('F10：资格请求乱序——慢 edible 旧响应不得覆盖较新的 denied', async () => {
    const io = editIO();
    let resolveA!: (r: FileEditRead) => void;
    io.read
      .mockImplementationOnce(
        () =>
          new Promise<FileEditRead>((res) => {
            resolveA = res;
          }),
      )
      .mockResolvedValue({
        editable: false,
        reasonCode: 'mixed_line_endings',
        reason: '换行风格混杂',
      } satisfies FileEditRead);
    const { container, root, unmount } = renderViewer({ editIO: io });
    await until(() => container.querySelectorAll('.cm-editor').length > 0);
    expect(io.read).toHaveBeenCalledTimes(1); // 请求 A 在途
    // diff 变化触发请求 B（快，denied）
    const onModeChange = vi.fn();
    const onWrapChange = vi.fn();
    act(() =>
      root.render(
        <DiffViewer
          diff={makeDiff({ newContent: 'const a = 1;\nconst b = 222;\nconst c = 3;\nconst d = 4;\n' })}
          path="a.ts"
          sourceRef=""
          untracked={false}
          annotations={[]}
          onCreateAnnotation={async () => {}}
          editIO={io}
          modeOverride="unified"
          onModeChange={onModeChange}
          wrapOverride={false}
          onWrapChange={onWrapChange}
        />,
      ),
    );
    await until(() => container.textContent?.includes('换行风格混杂') ?? false);
    // 旧代际 A 迟到（editable=true）——必须被丢弃，状态保持 denied
    await act(async () => {
      resolveA({
        editable: true,
        content: 'x\n',
        baseHash: 'h0',
        lineEnding: 'lf',
        hasBom: false,
        mode: '0644',
      } satisfies FileEditRead);
    });
    await flushUI();
    expect(container.textContent).toContain('换行风格混杂');
    expect(
      [...container.querySelectorAll<HTMLButtonElement>('button')].some(
        (b) => b.textContent === '编辑' && !b.disabled,
      ),
    ).toBe(false);
    unmount();
  });

  it('F7：diff 刷新后自动重新判定资格，入口重新开放', async () => {
    const io = editIO();
    io.read.mockResolvedValue({
      editable: false,
      reasonCode: 'read_only',
      reason: '文件只读',
    } satisfies FileEditRead);
    const { container, root, unmount } = renderViewer({ editIO: io });
    await until(() => container.querySelectorAll('.cm-editor').length > 0);
    await until(() => container.textContent?.includes('文件只读') ?? false);
    // diff prop 刷新（退出编辑/手动刷新路径）→ 自动重新 GET 判定
    io.read.mockResolvedValue({
      editable: true,
      content: 'x\n',
      baseHash: 'h0',
      lineEnding: 'lf',
      hasBom: false,
      mode: '0644',
    } satisfies FileEditRead);
    const onModeChange = vi.fn();
    const onWrapChange = vi.fn();
    act(() =>
      root.render(
        <DiffViewer
          diff={makeDiff({ newContent: 'const a = 1;\nconst b = 222;\nconst c = 3;\nconst d = 4;\n' })}
          path="a.ts"
          sourceRef=""
          untracked={false}
          annotations={[]}
          onCreateAnnotation={async () => {}}
          editIO={io}
          modeOverride="unified"
          onModeChange={onModeChange}
          wrapOverride={false}
          onWrapChange={onWrapChange}
        />,
      ),
    );
    // 资格转 eligible：编辑命令出现，拒绝理由清除
    await until(() =>
      [...container.querySelectorAll<HTMLButtonElement>('button')].some(
        (b) => b.textContent === '编辑' && !b.disabled,
      ),
    );
    expect(container.textContent).not.toContain('文件只读');
    expect(io.read).toHaveBeenCalledTimes(2);
    unmount();
  });
});

describe('编辑模式（进入/写回/横幅）', () => {
  beforeEach(() => stubMatchMedia(false));

  // GET 编辑读取契约（D5）：content 已去除 BOM、CRLF 归一为 \n；crlf 仅体现在 lineEnding
  const editableRead: FileEditRead = {
    editable: true,
    content: 'x\ny\n',
    baseHash: 'h0',
    lineEnding: 'crlf',
    hasBom: false,
    mode: '0644',
  };

  async function enterEdit(io: { read: ReturnType<typeof vi.fn>; write: ReturnType<typeof vi.fn> }, agentBusy = false) {
    io.read.mockResolvedValue(editableRead);
    io.write.mockResolvedValue({ baseHash: 'h1' });
    const mounted = renderViewer({ editIO: io, agentBusy });
    await until(() => editorViews(mounted.container).length > 0);
    // F7：资格预取完成后才提供编辑命令
    await until(() =>
      [...mounted.container.querySelectorAll<HTMLButtonElement>('button')].some(
        (b) => b.textContent === '编辑' && !b.disabled,
      ),
    );
    const btn = [...mounted.container.querySelectorAll<HTMLButtonElement>('button')].find(
      (b) => b.textContent === '编辑',
    )!;
    act(() => btn.click());
    await until(
      () =>
        [...mounted.container.querySelectorAll('.cm-content')].some(
          (el) => el.getAttribute('contenteditable') === 'true',
        ),
    );
    return mounted;
  }

  it('进入编辑：新侧可编辑、无批注手势 gutter、编辑实时写回携带冻结 crlf/mode', async () => {
    const io = { read: vi.fn(), write: vi.fn() };
    const { container, unmount } = await enterEdit(io);
    // 编辑模式无批注标记 gutter
    expect(container.querySelector('.cm-annotationGutter')).toBeNull();
    // 编辑新侧内容
    const view = editorViews(container)[0];
    act(() => {
      view.dispatch({ changes: { from: view.state.doc.length, insert: 'z\n' } });
    });
    await until(() => io.write.mock.calls.length > 0);
    const input = io.write.mock.calls[0][0] as FileEditWriteInput;
    expect(input.path).toBe('a.ts');
    expect(input.content).toBe('x\ny\nz\n'); // CM 文档以 \n 为换行
    expect(input.content).not.toContain('\r');
    expect(input.lineEnding).toBe('crlf'); // 冻结携带
    expect(input.baseMode).toBe('0644');
    expect(input.baseHash).toBe('h0');
    await until(() => container.textContent?.includes('已保存') ?? false);
    unmount();
  });

  it('busy 时编辑带醒目警告横幅（D6）', async () => {
    const io = { read: vi.fn(), write: vi.fn() };
    const { container, unmount } = await enterEdit(io, true);
    expect(container.textContent).toContain('agent 正在修改代码');
    unmount();
  });

  it('409 → 冲突横幅保留内容、暂停写回；放弃本地改动后重读服务端内容', async () => {
    const io = { read: vi.fn(), write: vi.fn() };
    const { container, unmount } = await enterEdit(io);
    const conflict = new ApiError(409, 'conflict', '冲突');
    io.write.mockRejectedValueOnce(conflict);
    io.read.mockResolvedValueOnce({
      editable: true,
      content: 'server\n',
      baseHash: 'hS',
      lineEnding: 'crlf',
      hasBom: false,
      mode: '0644',
    } satisfies FileEditRead);
    const view = editorViews(container)[0];
    act(() => {
      view.dispatch({ changes: { from: 0, insert: 'mine\n' } });
    });
    await until(() => container.querySelector('.edit-conflict-bar') !== null, 6000);
    expect(container.textContent).toContain('保存冲突');
    // 内容保留（编辑器未重置）
    expect(view.state.doc.toString()).toContain('mine');
    // 放弃本地改动 → 重读并重建编辑器
    const discardBtn = [...container.querySelectorAll<HTMLButtonElement>('button')].find(
      (b) => b.textContent === '放弃本地改动',
    )!;
    await act(async () => {
      discardBtn.click();
    });
    await until(() => editorViews(container)[0].state.doc.toString() === 'server\n');
    expect(container.querySelector('.edit-conflict-bar')).toBeNull();
    unmount();
  });

  it('F8：外部元数据变化后放弃 → 新 session 携带新冻结 mode，后续写用新基线', async () => {
    const io = { read: vi.fn(), write: vi.fn() };
    const { container, unmount } = await enterEdit(io);
    io.write.mockRejectedValueOnce(new ApiError(409, 'conflict', '冲突'));
    // 外部 chmod：重读返回新 mode/基线
    io.read.mockResolvedValueOnce({
      editable: true,
      content: 'srv\n',
      baseHash: 'hS',
      lineEnding: 'lf',
      hasBom: false,
      mode: '0755',
    } satisfies FileEditRead);
    const view = editorViews(container)[0];
    act(() => {
      view.dispatch({ changes: { from: 0, insert: 'mine\n' } });
    });
    await until(() => container.querySelector('.edit-conflict-bar') !== null, 6000);
    await act(async () => {
      [...container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '放弃本地改动')!
        .click();
    });
    // 新 session 重建编辑器为服务端内容，冲突解除
    await until(() => editorViews(container)[0].state.doc.toString() === 'srv\n');
    expect(container.querySelector('.edit-conflict-bar')).toBeNull();
    // 后续编辑的写回携带新冻结元数据与新基线
    const v2 = editorViews(container)[0];
    act(() => {
      v2.dispatch({ changes: { from: v2.state.doc.length, insert: 'x\n' } });
    });
    await until(() => io.write.mock.calls.length >= 2, 6000);
    const last = io.write.mock.calls[io.write.mock.calls.length - 1][0] as FileEditWriteInput;
    expect(last).toMatchObject({ content: 'srv\nx\n', baseHash: 'hS', baseMode: '0755' });
    unmount();
  });

  it('F8：放弃时文件已不可编辑 → 安全退出编辑态并显示原因（不被困住）', async () => {
    const io = { read: vi.fn(), write: vi.fn() };
    const { container, unmount } = await enterEdit(io);
    io.write.mockRejectedValueOnce(new ApiError(409, 'conflict', '冲突'));
    io.read.mockResolvedValueOnce({
      editable: false,
      reasonCode: 'read_only',
      reason: '文件只读',
    } satisfies FileEditRead);
    const view = editorViews(container)[0];
    act(() => {
      view.dispatch({ changes: { from: 0, insert: 'mine\n' } });
    });
    await until(() => container.querySelector('.edit-conflict-bar') !== null, 6000);
    await act(async () => {
      [...container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '放弃本地改动')!
        .click();
    });
    // 安全退出：回查看模式（只读）、冲突横幅消失、显示原因、无编辑命令
    await until(() => container.textContent?.includes('文件只读') ?? false);
    expect(container.querySelector('.edit-conflict-bar')).toBeNull();
    expect(container.textContent).not.toContain('退出编辑');
    for (const el of container.querySelectorAll('.cm-content')) {
      expect(el.getAttribute('contenteditable')).toBe('false');
    }
    expect(
      [...container.querySelectorAll<HTMLButtonElement>('button')].some(
        (b) => b.textContent === '编辑' && !b.disabled,
      ),
    ).toBe(false);
    unmount();
  });

  it('F11：discard 在途期间重入被拒——迟到第二次不得清掉新 session', async () => {
    const io = { read: vi.fn(), write: vi.fn() };
    const { container, unmount } = await enterEdit(io);
    io.write.mockRejectedValueOnce(new ApiError(409, 'conflict', '冲突'));
    const view = editorViews(container)[0];
    act(() => {
      view.dispatch({ changes: { from: 0, insert: 'mine\n' } });
    });
    await until(() => container.querySelector('.edit-conflict-bar') !== null, 6000);
    // 第一次 discard：重读挂起（在途）
    let resolveDiscard!: (r: FileEditRead) => void;
    io.read.mockImplementationOnce(
      () =>
        new Promise<FileEditRead>((res) => {
          resolveDiscard = res;
        }),
    );
    const readsBefore = io.read.mock.calls.length;
    act(() => {
      [...container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '放弃本地改动')!
        .click();
    });
    // 在途期间按钮禁用，第二次点击被拒绝（不产生第二次重读）
    await until(() => container.textContent?.includes('放弃中…') ?? false);
    act(() => {
      [...container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '放弃中…')!
        .click();
    });
    expect(io.read.mock.calls.length).toBe(readsBefore + 1);
    // 第一次完成：新 session 建立
    await act(async () => {
      resolveDiscard({
        editable: true,
        content: 'srv\n',
        baseHash: 'hS',
        lineEnding: 'crlf',
        hasBom: false,
        mode: '0644',
      } satisfies FileEditRead);
    });
    await until(() => editorViews(container)[0].state.doc.toString() === 'srv\n');
    expect(container.querySelector('.edit-conflict-bar')).toBeNull();
    // 新 session 的编辑不丢失：写回以新基线正常进行
    const v2 = editorViews(container)[0];
    act(() => {
      v2.dispatch({ changes: { from: v2.state.doc.length, insert: 'n\n' } });
    });
    await until(() => io.write.mock.calls.length >= 2, 6000);
    const last = io.write.mock.calls[io.write.mock.calls.length - 1][0] as FileEditWriteInput;
    expect(last).toMatchObject({ content: 'srv\nn\n', baseHash: 'hS' });
    expect(
      [...container.querySelectorAll('.cm-content')].some(
        (el) => el.getAttribute('contenteditable') === 'true',
      ),
    ).toBe(true);
    unmount();
  });

  it('F11：延迟 discard 期间继续输入 → 输入不被覆盖，恰好一次补发且携带新 metadata', async () => {
    const io = { read: vi.fn(), write: vi.fn() };
    const { container, unmount } = await enterEdit(io); // 旧 session：crlf/0644/h0
    io.write.mockRejectedValueOnce(new ApiError(409, 'conflict', '冲突'));
    io.write.mockResolvedValue({ baseHash: 'h2' });
    const view = editorViews(container)[0];
    act(() => {
      view.dispatch({ changes: { from: 0, insert: 'mine\n' } });
    });
    await until(() => container.querySelector('.edit-conflict-bar') !== null, 6000);
    let resolveDiscard!: (r: FileEditRead) => void;
    io.read.mockImplementationOnce(
      () =>
        new Promise<FileEditRead>((res) => {
          resolveDiscard = res;
        }),
    );
    act(() => {
      [...container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '放弃本地改动')!
        .click();
    });
    await until(() => container.textContent?.includes('放弃中…') ?? false);
    // discard 在途：编辑器已锁只读过渡态
    for (const el of container.querySelectorAll('.cm-content')) {
      expect(el.getAttribute('contenteditable')).toBe('false');
    }
    // 代际栅栏路径（程序化事务绕过 UI 锁的残余窗口）：GET 返回前又有新输入
    act(() => {
      view.dispatch({ changes: { from: view.state.doc.length, insert: 'late\n' } });
    });
    await act(async () => {
      resolveDiscard({
        editable: true,
        content: 'srv\n',
        baseHash: 'hS',
        lineEnding: 'lf',
        hasBom: false,
        mode: '0755',
      } satisfies FileEditRead);
    });
    // 补发唯一 owner 是新 session：总写数恰好 2（冲突写 + 一次补发），旧 session 零额外写
    await until(() => io.write.mock.calls.length === 2, 6000);
    await flushUI();
    expect(io.write.mock.calls.length).toBe(2);
    const patch = io.write.mock.calls[1][0] as FileEditWriteInput;
    expect(patch.content).toContain('late\n'); // 用户输入保留，不被 'srv\n' 覆盖
    expect(patch).toMatchObject({ baseHash: 'hS', baseMode: '0755', lineEnding: 'lf' }); // 新冻结 metadata
    expect(editorViews(container)[0].state.doc.toString()).toContain('late\n');
    // 下一次编辑使用补发响应返回的新 hash
    const v2 = editorViews(container)[0];
    act(() => {
      v2.dispatch({ changes: { from: v2.state.doc.length, insert: 'next\n' } });
    });
    await until(() => io.write.mock.calls.length === 3, 6000);
    expect(io.write.mock.calls[2][0]).toMatchObject({ baseHash: 'h2', baseMode: '0755' });
    unmount();
  });

  it('F18：discard 在途禁用形态/换行控件，强制重建后编辑器仍为只读锁态', async () => {
    const io = { read: vi.fn(), write: vi.fn() };
    io.read.mockResolvedValue(editableRead);
    io.write.mockResolvedValue({ baseHash: 'h1' });
    let setWrapExt: (b: boolean) => void = () => {};
    function StatefulViewer() {
      const [wrap, setWrap] = useState(false);
      setWrapExt = setWrap;
      return (
        <DiffViewer
          diff={makeDiff({})}
          path="a.ts"
          sourceRef=""
          untracked={false}
          annotations={[]}
          onCreateAnnotation={async () => {}}
          editIO={io}
          modeOverride="unified"
          onModeChange={() => {}}
          wrapOverride={wrap}
          onWrapChange={setWrap}
        />
      );
    }
    const { container, unmount } = mount(<StatefulViewer />);
    await until(() => editorViews(container).length > 0);
    await clickEdit(container);
    // 制造 409 阻塞后发起 discard（重读挂起）
    io.write.mockRejectedValueOnce(new ApiError(409, 'conflict', '冲突'));
    act(() => {
      editorViews(container)[0].dispatch({ changes: { from: 0, insert: 'mine\n' } });
    });
    await until(() => container.querySelector('.edit-conflict-bar') !== null, 6000);
    io.read.mockImplementationOnce(() => new Promise<FileEditRead>(() => {}));
    act(() => {
      [...container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '放弃本地改动')!
        .click();
    });
    await until(() => container.textContent?.includes('放弃中…') ?? false);
    // 三控件禁用
    for (const label of ['单列', '并排', '换行']) {
      const btn = [...container.querySelectorAll<HTMLButtonElement>('button')].find(
        (b) => b.textContent === label,
      )!;
      expect(btn.disabled).toBe(true);
    }
    // 外部强制触发重建（换行 prop 变化）：新编辑器仍须为只读锁态
    act(() => setWrapExt(true));
    await until(() => editorViews(container).length > 0);
    for (const el of container.querySelectorAll('.cm-content')) {
      expect(el.getAttribute('contenteditable')).toBe('false');
    }
    unmount();
  });

  it('F11：discard 与 exit 并发——discard 在途时退出被拒，无双 session 无丢写', async () => {
    const io = { read: vi.fn(), write: vi.fn() };
    const { container, unmount } = await enterEdit(io);
    io.write.mockRejectedValueOnce(new ApiError(409, 'conflict', '冲突'));
    const view = editorViews(container)[0];
    act(() => {
      view.dispatch({ changes: { from: 0, insert: 'mine\n' } });
    });
    await until(() => container.querySelector('.edit-conflict-bar') !== null, 6000);
    let resolveDiscard!: (r: FileEditRead) => void;
    io.read.mockImplementationOnce(
      () =>
        new Promise<FileEditRead>((res) => {
          resolveDiscard = res;
        }),
    );
    act(() => {
      [...container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '放弃本地改动')!
        .click();
    });
    await until(() => container.textContent?.includes('放弃中…') ?? false);
    // discard 在途：退出按钮禁用，点击无效（仍处编辑模式）
    const exitBtn = [...container.querySelectorAll<HTMLButtonElement>('button')].find(
      (b) => b.textContent === '退出编辑',
    )!;
    expect(exitBtn.disabled).toBe(true);
    act(() => exitBtn.click());
    await flushUI();
    expect(container.textContent).toContain('放弃中…'); // 仍在 discard 流程，未退出
    // discard 完成：唯一新 session 建立，可继续编辑写回
    await act(async () => {
      resolveDiscard({
        editable: true,
        content: 'srv\n',
        baseHash: 'hS',
        lineEnding: 'crlf',
        hasBom: false,
        mode: '0644',
      } satisfies FileEditRead);
    });
    await until(() => editorViews(container)[0].state.doc.toString() === 'srv\n');
    expect(editorViews(container).length).toBe(1);
    const v2 = editorViews(container)[0];
    act(() => {
      v2.dispatch({ changes: { from: v2.state.doc.length, insert: 'n\n' } });
    });
    await until(() => io.write.mock.calls.length >= 2, 6000);
    unmount();
  });

  it('还原入口：确认后以快照写回', async () => {
    const io = { read: vi.fn(), write: vi.fn() };
    const { container, unmount } = await enterEdit(io);
    const view = editorViews(container)[0];
    act(() => {
      view.dispatch({ changes: { from: 0, insert: 'z\n' } });
    });
    await until(() => io.write.mock.calls.length > 0, 6000);
    const restoreBtn = [...container.querySelectorAll<HTMLButtonElement>('button')].find(
      (b) => b.textContent === '还原我的改动',
    )!;
    act(() => restoreBtn.click());
    expect(container.textContent).toContain('还原到进入编辑前的内容？');
    const confirmBtn = [...container.querySelectorAll<HTMLButtonElement>('button')].find(
      (b) => b.textContent === '确认还原',
    )!;
    await act(async () => {
      confirmBtn.click();
    });
    await until(() => io.write.mock.calls.length >= 2);
    const last = io.write.mock.calls[io.write.mock.calls.length - 1][0] as FileEditWriteInput;
    expect(last.content).toBe('x\ny\n'); // 会话快照
    await until(() => editorViews(container)[0].state.doc.toString() === 'x\ny\n');
    unmount();
  });

  it('F4：粘贴 CRLF/裸 CR 被归一为 \\n，写回 content 不含 \\r', async () => {
    const io = { read: vi.fn(), write: vi.fn() };
    const { container, unmount } = await enterEdit(io);
    const view = editorViews(container)[0];
    act(() => {
      view.dispatch({
        changes: { from: view.state.doc.length, insert: 'p\r\nq\rr\n' },
      });
    });
    await until(() => io.write.mock.calls.length > 0);
    const input = io.write.mock.calls[0][0] as FileEditWriteInput;
    expect(input.content).not.toContain('\r');
    expect(input.content).toBe('x\ny\np\nq\nr\n');
    unmount();
  });

  it('F3：退出编辑前调用 onRefreshDiff 刷新原始 diff，成功后才回查看模式', async () => {
    const io = { read: vi.fn(), write: vi.fn() };
    const first = await enterEdit(io);
    // 重新挂载带 onRefreshDiff 的实例（enterEdit 帮助函数未传该 prop）
    first.unmount();
    const onRefreshDiff = vi.fn(async () => {});
    const mounted = renderViewer({ editIO: io, onRefreshDiff });
    await until(() => editorViews(mounted.container).length > 0);
    await clickEdit(mounted.container);
    await act(async () => {
      [...mounted.container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '退出编辑')!
        .click();
      await Promise.resolve();
    });
    expect(onRefreshDiff).toHaveBeenCalledTimes(1);
    // 回到只读查看模式
    await until(
      () =>
        [...mounted.container.querySelectorAll('.cm-content')].every(
          (el) => el.getAttribute('contenteditable') === 'false',
        ),
    );
    mounted.unmount();
  });

  it('F3：刷新等待窗口编辑器只读锁定（deferred），输入不会静默丢失', async () => {
    const io = { read: vi.fn(), write: vi.fn() };
    io.read.mockResolvedValue(editableRead);
    io.write.mockResolvedValue({ baseHash: 'h1' });
    let resolveRefresh!: () => void;
    const onRefreshDiff = vi.fn(
      () =>
        new Promise<void>((res) => {
          resolveRefresh = res;
        }),
    );
    const mounted = renderViewer({ editIO: io, onRefreshDiff });
    await until(() => editorViews(mounted.container).length > 0);
    await clickEdit(mounted.container);
    // 点击进入编辑退出事务但不释放刷新
    act(() => {
      [...mounted.container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '退出编辑')!
        .click();
    });
    await until(() => mounted.container.textContent?.includes('退出中…') ?? false);
    await until(() => onRefreshDiff.mock.calls.length === 1);
    // 等待窗口：编辑器已锁为只读过渡态，仍处编辑模式
    for (const el of mounted.container.querySelectorAll('.cm-content')) {
      expect(el.getAttribute('contenteditable')).toBe('false');
    }
    // 刷新完成后才回查看模式
    await act(async () => {
      resolveRefresh();
      await Promise.resolve();
    });
    await until(() => mounted.container.textContent?.includes('编辑') ?? false);
    expect(mounted.container.textContent).not.toContain('退出中…');
    mounted.unmount();
  });

  it('F3：刷新失败保持编辑模式 + 明确错误，重试退出成功', async () => {
    const io = { read: vi.fn(), write: vi.fn() };
    io.read.mockResolvedValue(editableRead);
    io.write.mockResolvedValue({ baseHash: 'h1' });
    const onRefreshDiff = vi
      .fn()
      .mockRejectedValueOnce(new Error('network down'))
      .mockResolvedValue(undefined);
    const mounted = renderViewer({ editIO: io, onRefreshDiff });
    await until(() => editorViews(mounted.container).length > 0);
    await clickEdit(mounted.container);
    await act(async () => {
      [...mounted.container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '退出编辑')!
        .click();
      await Promise.resolve();
    });
    // 失败：仍在编辑模式（解除过渡锁定可继续编辑）、错误可见
    await until(() => mounted.container.textContent?.includes('刷新 diff 失败') ?? false);
    await until(
      () =>
        [...mounted.container.querySelectorAll('.cm-content')].some(
          (el) => el.getAttribute('contenteditable') === 'true',
        ),
    );
    // 重试退出成功
    await act(async () => {
      [...mounted.container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '退出编辑')!
        .click();
      await Promise.resolve();
    });
    await until(() => !(mounted.container.textContent?.includes('退出编辑') ?? true));
    expect(onRefreshDiff).toHaveBeenCalledTimes(2);
    expect(mounted.container.textContent).not.toContain('刷新 diff 失败');
    mounted.unmount();
  });

  it('F3：退出后查看模式展示刷新后的 diff，新批注快照取自最新原始侧内容', async () => {
    const io = { read: vi.fn(), write: vi.fn() };
    io.read.mockResolvedValue(editableRead);
    io.write.mockResolvedValue({ baseHash: 'h1' });
    const onCreateAnnotation = vi.fn(async (_input: AnnotationCreateInput) => {});
    // 有状态的父组件：onRefreshDiff 真实更新 diff prop
    function StatefulViewer() {
      const [d, setD] = useState(
        makeDiff({ newContent: 'old-line1\nold-line2\nold-line3\n' }),
      );
      return (
        <DiffViewer
          diff={d}
          path="a.ts"
          sourceRef=""
          untracked={false}
          annotations={[]}
          onCreateAnnotation={onCreateAnnotation}
          editIO={io}
          onRefreshDiff={async () => {
            setD(makeDiff({ newContent: 'fresh-line1\nfresh-line2\n' }));
          }}
          modeOverride="unified"
          onModeChange={() => {}}
          wrapOverride={false}
          onWrapChange={() => {}}
        />
      );
    }
    const { container, unmount } = mount(<StatefulViewer />);
    await until(() => editorViews(container).length > 0);
    await clickEdit(container);
    await act(async () => {
      [...container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '退出编辑')!
        .click();
      await Promise.resolve();
    });
    // 查看模式编辑器显示刷新后的内容（不是编辑前旧 diff，也不是编辑 GET 的规范化文本）
    await until(
      () => editorViews(container)[0]?.state.doc.toString() === 'fresh-line1\nfresh-line2\n',
    );
    // 基于最新内容创建批注：快照含 fresh 行
    const view = editorViews(container)[0];
    act(() => {
      view.dispatch({ selection: { anchor: 0, head: view.state.doc.line(1).to } });
      container
        .querySelector('.cm-line')!
        .dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
    });
    await submitDraftWith(container, '新内容批注');
    expect(onCreateAnnotation).toHaveBeenCalledTimes(1);
    expect(onCreateAnnotation.mock.calls[0][0].snapshot).toContain('fresh-line1');
    expect(onCreateAnnotation.mock.calls[0][0].snapshot).not.toContain('old-line1');
    unmount();
  });

  it('F9：退出期间禁用形态/换行控件，强制重建后编辑器仍为只读锁态', async () => {
    const io = { read: vi.fn(), write: vi.fn() };
    io.read.mockResolvedValue(editableRead);
    io.write.mockResolvedValue({ baseHash: 'h1' });
    let resolveRefresh!: () => void;
    // 有状态父组件：换行开关真实驱动编辑器重建
    let setWrapExt: (b: boolean) => void = () => {};
    function StatefulViewer() {
      const [wrap, setWrap] = useState(false);
      setWrapExt = setWrap;
      return (
        <DiffViewer
          diff={makeDiff({})}
          path="a.ts"
          sourceRef=""
          untracked={false}
          annotations={[]}
          onCreateAnnotation={async () => {}}
          editIO={io}
          onRefreshDiff={() =>
            new Promise<void>((res) => {
              resolveRefresh = res;
            })
          }
          modeOverride="unified"
          onModeChange={() => {}}
          wrapOverride={wrap}
          onWrapChange={setWrap}
        />
      );
    }
    const { container, unmount } = mount(<StatefulViewer />);
    await until(() => editorViews(container).length > 0);
    await clickEdit(container);
    // 进入退出事务，刷新挂起
    act(() => {
      [...container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '退出编辑')!
        .click();
    });
    await until(() => container.textContent?.includes('退出中…') ?? false);
    // 形态/换行控件在退出期间禁用
    for (const label of ['单列', '并排', '换行']) {
      const btn = [...container.querySelectorAll<HTMLButtonElement>('button')].find(
        (b) => b.textContent === label,
      )!;
      expect(btn.disabled).toBe(true);
    }
    // 外部强制触发重建（换行 prop 变化）：新编辑器仍须为只读锁态
    act(() => setWrapExt(true));
    await until(() => editorViews(container).length > 0);
    for (const el of container.querySelectorAll('.cm-content')) {
      expect(el.getAttribute('contenteditable')).toBe('false');
    }
    // 不存在未 flush 输入：写请求数与进入退出前一致（零）
    expect(io.write).not.toHaveBeenCalled();
    // 刷新完成后回查看模式
    await act(async () => {
      resolveRefresh();
      await Promise.resolve();
    });
    await until(
      () =>
        [...container.querySelectorAll<HTMLButtonElement>('button')].some(
          (b) => b.textContent === '编辑',
        ),
    );
    unmount();
  });

  it('F8：资格预取的瞬时请求错误可重试（不写资格态、提示 + 重试入口）', async () => {
    const io = { read: vi.fn(), write: vi.fn() };
    io.read.mockRejectedValueOnce(new Error('network down')).mockResolvedValue(editableRead);
    io.write.mockResolvedValue({ baseHash: 'h1' });
    const { container, unmount } = renderViewer({ editIO: io });
    await until(() => container.querySelectorAll('.cm-editor').length > 0);
    // 预取失败：提示 + 重试入口，无编辑命令
    await until(() => container.textContent?.includes('进入编辑失败') ?? false);
    expect(
      [...container.querySelectorAll<HTMLButtonElement>('button')].some(
        (b) => b.textContent === '编辑' && !b.disabled,
      ),
    ).toBe(false);
    // 重试成功 → eligible，编辑命令出现
    await act(async () => {
      [...container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '重试')!
        .click();
      await Promise.resolve();
    });
    await until(() =>
      [...container.querySelectorAll<HTMLButtonElement>('button')].some(
        (b) => b.textContent === '编辑' && !b.disabled,
      ),
    );
    expect(io.read).toHaveBeenCalledTimes(2);
    expect(container.textContent).not.toContain('进入编辑失败');
    // 点击进入编辑（复用预取结果，不再 GET）
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
    expect(io.read).toHaveBeenCalledTimes(2);
    unmount();
  });

  it('F7：先开内联批注区再进入编辑 → 批注区与候选高亮关闭，编辑模式无批注区渲染', async () => {
    const io = { read: vi.fn(), write: vi.fn() };
    const onCreateAnnotation = vi.fn(async (_input: AnnotationCreateInput) => {});
    io.read.mockResolvedValue(editableRead);
    io.write.mockResolvedValue({ baseHash: 'h1' });
    const { container, unmount } = renderViewer({ editIO: io, onCreateAnnotation });
    await until(() => editorViews(container).length > 0);
    const view = editorViews(container)[0];
    // 查看模式框选打开内联批注区
    act(() => {
      view.dispatch({ selection: { anchor: 0, head: view.state.doc.line(1).to } });
      container
        .querySelector('.cm-line')!
        .dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
    });
    expect(container.querySelector('.ann-inline')).toBeTruthy();
    expect(container.querySelector('.cm-annotationCandidate')).toBeTruthy();
    // 不提交草稿，直接进入编辑（先等资格预取完成）
    await until(() =>
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
    expect(container.querySelector('.ann-inline')).toBeNull();
    expect(container.querySelector('.cm-annotationCandidate')).toBeNull();
    expect(onCreateAnnotation).not.toHaveBeenCalled();
    unmount();
  });
});
