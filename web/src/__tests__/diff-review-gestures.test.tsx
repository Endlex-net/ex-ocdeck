// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act, useState } from 'react';
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
  modeOverride?: 'unified' | 'side-by-side';
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
      modeOverride={over.modeOverride ?? 'unified'}
      onModeChange={onModeChange}
      wrapOverride={false}
      onWrapChange={onWrapChange}
    />,
  );
  return { ...mounted, onModeChange, onWrapChange };
}

async function submitBubbleWith(container: HTMLElement, comment: string) {
  const ta = container.querySelector<HTMLTextAreaElement>('.ann-bubble-input');
  expect(ta, '批注气泡已打开').toBeTruthy();
  if (comment) setNativeValue(ta!, comment);
  const btn = [...container.querySelectorAll<HTMLButtonElement>('.ann-bubble-actions button')].find(
    (b) => b.textContent?.includes('添加批注'),
  )!;
  await act(async () => {
    btn.click();
    await Promise.resolve();
  });
}

describe('查看模式批注手势（框选/点击 + 快照构造）', () => {
  beforeEach(() => stubMatchMedia(false));

  it('unified 拖选新侧行 → 气泡 → 提交：side=new、行范围、快照取自原始侧内容并保留 \\r', async () => {
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
    expect(container.querySelector('.ann-bubble')).toBeTruthy();
    expect(container.textContent).toContain('新侧 L3-4');
    // 候选选区高亮
    expect(container.querySelector('.cm-annotationCandidate')).toBeTruthy();
    await submitBubbleWith(container, '这里改成 22 有问题');
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
    // 提交后气泡关闭、候选高亮清除
    expect(container.querySelector('.ann-bubble')).toBeNull();
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
    await submitBubbleWith(container, '   ');
    expect(onCreateAnnotation).not.toHaveBeenCalled();
    expect(container.querySelector('.ann-bubble')).toBeNull();
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
    expect(container.textContent).toContain('旧侧 L2');
    await submitBubbleWith(container, '旧侧评论');
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
    expect(container.querySelector('.ann-bubble')).toBeTruthy();
    expect(container.textContent).toContain('旧侧 L3');
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
    expect(container.textContent).toContain('旧侧 L2-3');
    await submitBubbleWith(container, '删除的两行都有问题');
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
    expect(container.querySelector('.ann-bubble')).toBeNull();
    // 普通内容拖到删除块
    act(() => {
      line.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }));
      deleted.dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
    });
    expect(onCreateAnnotation).not.toHaveBeenCalled();
    expect(container.querySelector('.ann-bubble')).toBeNull();
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
    expect(container.querySelector('.ann-bubble')).toBeTruthy();
    expect(container.textContent).toContain('新侧 L2');
    await submitBubbleWith(container, '单击行批注');
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
    // 气泡必须是行号单击的 L4 单行，不是旧选区 L1-3/L1-4
    expect(container.textContent).toContain('批注 · 新侧 L4');
    expect(container.textContent).not.toContain('L1-');
    unmount();
  });

  it('F14：点击批注标记（gutter widget）+ 旧选区 → 只定位不开新气泡', async () => {
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
    expect(container.querySelector('.ann-bubble')).toBeNull(); // 未被旧选区误开批注
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
    expect(container.querySelector('.ann-bubble')).toBeNull();
    expect(onCreateAnnotation).not.toHaveBeenCalled();
    unmount();
  });

  it('F16：点击并排对齐空白区（.cm-mergeSpacer）+ 残留选区 → 零新气泡零创建', async () => {
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
    expect(container.querySelector('.ann-bubble')).toBeNull();
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
    expect(container.querySelector('.ann-bubble')).toBeNull();
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
    expect(container.textContent).toContain('新侧 L2');
    await submitBubbleWith(container, '行号批注');
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
    expect(sb.container.textContent).toContain('旧侧 L2');
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

  it('GET editable=false：入口禁用并展示服务端原因（F6：不可无限重复点击）', async () => {
    const io = editIO();
    const notEditable: FileEditRead = {
      editable: false,
      reasonCode: 'mixed_line_endings',
      reason: '换行风格混杂',
    };
    io.read.mockResolvedValue(notEditable);
    const { container, unmount } = renderViewer({ editIO: io });
    await until(() => container.querySelectorAll('.cm-editor').length > 0);
    const btn = [...container.querySelectorAll<HTMLButtonElement>('button')].find(
      (b) => b.textContent === '编辑',
    )!;
    expect(btn.disabled).toBe(false);
    act(() => btn.click());
    await until(() => container.textContent?.includes('换行风格混杂') ?? false);
    // 资格拒绝后入口禁用，重复点击不再触发 GET（checking→view 往返重建按钮，需重新查询）
    const btn2 = [...container.querySelectorAll<HTMLButtonElement>('button')].find(
      (b) => b.textContent === '编辑',
    )!;
    expect(btn2.disabled).toBe(true);
    act(() => btn2.click());
    await flushUI();
    expect(io.read).toHaveBeenCalledTimes(1);
    // 仍在查看模式（只读）
    for (const el of container.querySelectorAll('.cm-content')) {
      expect(el.getAttribute('contenteditable')).toBe('false');
    }
    unmount();
  });

  it('F6：diff 刷新后资格拒绝作废，编辑入口重新开放', async () => {
    const io = editIO();
    io.read.mockResolvedValue({
      editable: false,
      reasonCode: 'read_only',
      reason: '文件只读',
    } satisfies FileEditRead);
    const { container, root, unmount } = renderViewer({ editIO: io });
    await until(() => container.querySelectorAll('.cm-editor').length > 0);
    const btn = [...container.querySelectorAll<HTMLButtonElement>('button')].find(
      (b) => b.textContent === '编辑',
    )!;
    act(() => btn.click());
    await until(() => container.textContent?.includes('文件只读') ?? false);
    const btn2 = [...container.querySelectorAll<HTMLButtonElement>('button')].find(
      (b) => b.textContent === '编辑',
    )!;
    expect(btn2.disabled).toBe(true);
    // diff prop 刷新（退出编辑/手动刷新路径）→ 拒绝理由重置，可重新尝试
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
    await flushUI();
    const btn3 = [...container.querySelectorAll<HTMLButtonElement>('button')].find(
      (b) => b.textContent === '编辑',
    )!;
    expect(btn3.disabled).toBe(false);
    expect(container.textContent).not.toContain('文件只读');
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
    act(() => {
      [...mounted.container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '编辑')!
        .click();
    });
    await until(
      () =>
        [...mounted.container.querySelectorAll('.cm-content')].some(
          (el) => el.getAttribute('contenteditable') === 'true',
        ),
    );
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
    act(() => {
      [...mounted.container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '编辑')!
        .click();
    });
    await until(
      () =>
        [...mounted.container.querySelectorAll('.cm-content')].some(
          (el) => el.getAttribute('contenteditable') === 'true',
        ),
    );
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
    act(() => {
      [...mounted.container.querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '编辑')!
        .click();
    });
    await until(
      () =>
        [...mounted.container.querySelectorAll('.cm-content')].some(
          (el) => el.getAttribute('contenteditable') === 'true',
        ),
    );
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
    await submitBubbleWith(container, '新内容批注');
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

  it('F8：进入编辑的瞬时请求错误可重试（不写资格态、不禁用入口）', async () => {
    const io = { read: vi.fn(), write: vi.fn() };
    io.read.mockRejectedValueOnce(new Error('network down')).mockResolvedValue(editableRead);
    io.write.mockResolvedValue({ baseHash: 'h1' });
    const { container, unmount } = renderViewer({ editIO: io });
    await until(() => container.querySelectorAll('.cm-editor').length > 0);
    const editBtn = () =>
      [...container.querySelectorAll<HTMLButtonElement>('button')].find(
        (b) => b.textContent === '编辑',
      )!;
    act(() => editBtn().click());
    await until(() => container.textContent?.includes('进入编辑失败') ?? false);
    // 入口保持可用，直接重试成功进入编辑
    expect(editBtn().disabled).toBe(false);
    act(() => editBtn().click());
    await until(
      () =>
        [...container.querySelectorAll('.cm-content')].some(
          (el) => el.getAttribute('contenteditable') === 'true',
        ),
    );
    expect(io.read).toHaveBeenCalledTimes(2);
    expect(container.textContent).not.toContain('进入编辑失败');
    unmount();
  });

  it('F7：先开批注气泡再进入编辑 → 气泡与候选高亮关闭，编辑模式无气泡渲染', async () => {
    const io = { read: vi.fn(), write: vi.fn() };
    const onCreateAnnotation = vi.fn(async (_input: AnnotationCreateInput) => {});
    io.read.mockResolvedValue(editableRead);
    io.write.mockResolvedValue({ baseHash: 'h1' });
    const { container, unmount } = renderViewer({ editIO: io, onCreateAnnotation });
    await until(() => editorViews(container).length > 0);
    const view = editorViews(container)[0];
    // 查看模式框选打开气泡
    act(() => {
      view.dispatch({ selection: { anchor: 0, head: view.state.doc.line(1).to } });
      container
        .querySelector('.cm-line')!
        .dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
    });
    expect(container.querySelector('.ann-bubble')).toBeTruthy();
    expect(container.querySelector('.cm-annotationCandidate')).toBeTruthy();
    // 不提交气泡，直接进入编辑
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
    expect(container.querySelector('.ann-bubble')).toBeNull();
    expect(container.querySelector('.cm-annotationCandidate')).toBeNull();
    expect(onCreateAnnotation).not.toHaveBeenCalled();
    unmount();
  });
});
