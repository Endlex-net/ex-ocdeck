// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act } from 'react';
import { EditorView } from '@codemirror/view';
import MarkdownPreview, {
  blockRange,
  blockRangeByIndex,
  buildLineIndex,
  lineAt,
  lineAtByIndex,
} from '../components/diff/MarkdownPreview';
import DiffViewer from '../components/diff/DiffViewer';
import type { Annotation, AnnotationCreateInput, GitDiffResult } from '../types';
import { mount, rerender, stubMatchMedia } from './cm-test-env';

/* ============================ 预览块级批注（tasks 5.1-5.5，design D8） ============================
 * 行号映射唯一口径：lineAt(o) = 1 + count('\n', content.slice(0, o))；end point 排他取前一字符所在行；
 * 可批注块集合 p/h1-h6/li/table/pre/blockquote；徽章精确范围匹配 + 最内层归属 + 聚合 + 漂移标识；
 * 草稿生命周期 owner 为 DiffViewer（复用既有批注数据契约与 ±3 快照窗口）。 */

/* ora-13 F2：解析调用计数——包装真实 react-markdown（行为不变，仅计数执行次数） */
const mockParseCount = { n: 0 };

vi.mock('react-markdown', async (importOriginal) => {
  const actual = await importOriginal<typeof import('react-markdown')>();
  const CountingMarkdown = (props: Parameters<typeof actual.default>[0]) => {
    mockParseCount.n += 1;
    return actual.default(props);
  };
  return { ...actual, default: CountingMarkdown };
});

function makeMdDiff(over: Partial<GitDiffResult> = {}): GitDiffResult {
  return {
    oldContent: '# 旧标题\n\n旧段落。\n',
    newContent: '# 标题\n\n段落A\n\n段落B\n\n段落C\n\n段落D\n',
    oldExists: true,
    newExists: true,
    oldMode: '100644',
    newMode: '100644',
    isBinary: false,
    truncated: false,
    ...over,
  };
}

function makeBlockAnn(over: Partial<Annotation>): Annotation {
  return {
    id: 'b1',
    path: 'a.md',
    side: 'new',
    ref: '',
    untracked: false,
    startLine: 3,
    endLine: 3,
    snapshotStartLine: 1,
    snapshotLineCount: 5,
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

/** DiffViewer 级渲染：markdown diff + 批注回调，返回 rerender 所需回调。 */
function renderViewerWithAnnotations(over: {
  diff?: GitDiffResult;
  annotations?: Annotation[];
  onCreateAnnotation?: ReturnType<typeof vi.fn>;
  onLocateAnnotations?: ReturnType<typeof vi.fn>;
}) {
  const onModeChange = vi.fn();
  const onWrapChange = vi.fn();
  const mounted = mount(
    <DiffViewer
      diff={over.diff ?? makeMdDiff()}
      path="a.md"
      sourceRef=""
      untracked={false}
      annotations={over.annotations ?? []}
      onCreateAnnotation={over.onCreateAnnotation ?? (async () => {})}
      onLocateAnnotations={over.onLocateAnnotations ?? (() => {})}
      modeOverride="unified"
      onModeChange={onModeChange}
      wrapOverride={false}
      onWrapChange={onWrapChange}
    />,
  );
  return { ...mounted, onModeChange, onWrapChange };
}

function enterPreview(container: HTMLElement, expectedPanes = 2) {
  const btn = [...container.querySelectorAll<HTMLButtonElement>('.diff-toolbar button')].find(
    (b) => b.textContent?.trim() === '预览',
  );
  expect(btn, '预览开关存在').toBeTruthy();
  act(() => btn!.click());
  expect(container.querySelectorAll('.md-diff-pane')).toHaveLength(expectedPanes);
}

/** 在指定预览栏内打开块级草稿（默认新侧 pane 的第三个块入口 = 段落B：标题/段落A 之后）。 */
function openBlockDraftInPane(container: HTMLElement, paneIndex: 0 | 1 = 1, entryIndex = 2) {
  const pane = container.querySelectorAll('.md-diff-pane')[paneIndex];
  const entries = pane.querySelectorAll<HTMLButtonElement>('.md-block-annotate');
  expect(entries.length).toBeGreaterThanOrEqual(entryIndex + 1);
  act(() => entries[entryIndex].click());
}

const openBlockDraftOnParagraphB = (container: HTMLElement) =>
  openBlockDraftInPane(container, 1, 2);

/** 组件级：渲染 MarkdownPreview（启用块级批注），返回入口按钮列表与容器。 */
function renderPreviewBlocks(content: string, over: Partial<Parameters<typeof MarkdownPreview>[0]> = {}) {
  const onAnnotateBlock = vi.fn();
  const mounted = mount(
    <MarkdownPreview content={content} side="new" onAnnotateBlock={onAnnotateBlock} {...over} />,
  );
  return {
    ...mounted,
    onAnnotateBlock,
    entries: () => [...mounted.container.querySelectorAll<HTMLButtonElement>('.md-block-annotate')],
  };
}

describe('行号映射纯函数（D8 唯一口径，仅以 \\n 分行）', () => {
  it('lineAt：offset 直接传 slice；裸 \\r 不分行、CRLF 按 \\n 分行', () => {
    // 'a\nb\nc'：a=0 \n=1 b=2 \n=3 c=4（offset 为 0-based UTF-16 code unit）
    expect(lineAt('a\nb\nc', 0)).toBe(1);
    expect(lineAt('a\nb\nc', 1)).toBe(1); // 第一行（含 \n 处）
    expect(lineAt('a\nb\nc', 2)).toBe(2); // 第二行
    expect(lineAt('a\nb\nc', 4)).toBe(3); // 第三行
    // 裸 \r 是文档字符：'a\rb' 恒为一行
    expect(lineAt('a\rb', 2)).toBe(1);
    expect(lineAt('a\rb', 3)).toBe(1);
    // CRLF：\n 才产生新行，\r 计入本行（'甲\r\n乙\r\n丙'：甲0 \r1 \n2 乙3 \r4 \n5 丙6）
    expect(lineAt('甲\r\n乙\r\n丙', 2)).toBe(1); // 第一行的 \n 处：仍是第一行
    expect(lineAt('甲\r\n乙\r\n丙', 3)).toBe(2); // 已进入第二行
    expect(lineAt('甲\r\n乙\r\n丙', 7)).toBe(3); // 末尾（\n 之后）
    expect(lineAt('甲\r\n乙\r\n丙', 10)).toBe(3); // 超长 offset 截断为全串
  });

  it('blockRange：startLine/endLine 公式（end point 排他取前一字符所在行）', () => {
    // 单行块 '第一行'（offset 0..3）：end=3 排他 → 前一字符 offset 2 在第一行 → (1,1)
    expect(blockRange('第一行\n第二行\n', { start: { offset: 0 }, end: { offset: 3 } })).toEqual({
      startLine: 1,
      endLine: 1,
    });
    // 'p1\np2 内容\n'：块 [3, 8) 覆盖第二行 → (2,2)
    expect(blockRange('p1\np2 内容\n', { start: { offset: 3 }, end: { offset: 8 } })).toEqual({
      startLine: 2,
      endLine: 2,
    });
    // 裸 \r 内容：范围横跨裸 \r 仍按 \n 模型（'甲\r含裸CR' 为一行，end=7 排他）
    expect(blockRange('甲\r含裸CR\n乙\n', { start: { offset: 0 }, end: { offset: 7 } })).toEqual({
      startLine: 1,
      endLine: 1,
    });
  });

  it('blockRange：position/offset 缺失或范围零宽 → null（不提供批注入口）', () => {
    expect(blockRange('任意\n', undefined)).toBeNull();
    expect(blockRange('任意\n', { start: {}, end: {} })).toBeNull();
    expect(blockRange('任意\n', { start: { offset: 2 }, end: { offset: 2 } })).toBeNull(); // 零宽
    expect(blockRange('任意\n', { start: { offset: 3 }, end: { offset: 2 } })).toBeNull(); // 倒置
    expect(blockRange('任意\n', { start: { offset: null }, end: { offset: 4 } })).toBeNull();
  });
});

describe('块级入口与行范围映射（MarkdownPreview 组件级）', () => {
  beforeEach(() => stubMatchMedia(false));

  it('段落：入口锚定各自块 range（1-based 闭区间）', () => {
    const { container, entries, onAnnotateBlock, unmount } = renderPreviewBlocks('第一段\n\n第二段\n');
    expect(entries()).toHaveLength(2);
    act(() => entries()[0].click());
    expect(onAnnotateBlock).toHaveBeenLastCalledWith({ startLine: 1, endLine: 1 });
    act(() => entries()[1].click());
    expect(onAnnotateBlock).toHaveBeenLastCalledWith({ startLine: 3, endLine: 3 });
    expect(container.querySelectorAll('.md-annotatable')).toHaveLength(2);
    unmount();
  });

  it('表格整块：单元格内无入口，入口锚定 table 整块 range', () => {
    const { entries, onAnnotateBlock, unmount } = renderPreviewBlocks('| a | b |\n| - | - |\n| 1 | 2 |\n');
    expect(entries()).toHaveLength(1); // 仅 table 块入口
    act(() => entries()[0].click());
    expect(onAnnotateBlock).toHaveBeenLastCalledWith({ startLine: 1, endLine: 3 });
    unmount();
  });

  it('含 checkbox 的任务列表项：li 可批注，生成节点不影响 li 行范围', () => {
    const { entries, onAnnotateBlock, unmount } = renderPreviewBlocks('- [x] 已完成\n- [ ] 待办\n');
    // li 与其内段落的入口均存在；文档序第一个是列表项入口
    expect(entries().length).toBeGreaterThanOrEqual(2);
    act(() => entries()[0].click());
    expect(onAnnotateBlock).toHaveBeenLastCalledWith({ startLine: 1, endLine: 1 });
    unmount();
  });

  it('代码块：pre 整块 range；hr 与容器不可批注（无入口）', () => {
    const { container, entries, onAnnotateBlock, unmount } = renderPreviewBlocks(
      '```js\ncode1\ncode2\n```\n\n---\n\n正文\n',
    );
    // 入口：pre(1) + p(1)；hr 与 ul 容器无入口
    expect(entries()).toHaveLength(2);
    act(() => entries()[0].click());
    expect(onAnnotateBlock).toHaveBeenLastCalledWith({ startLine: 1, endLine: 4 });
    expect(container.querySelector('hr')!.querySelector('.md-block-annotate')).toBeNull();
    unmount();
  });

  it('裸 \\r 内容：行范围按 \\n 模型映射（不用解析器行号）', () => {
    const { entries, onAnnotateBlock, unmount } = renderPreviewBlocks('段落甲\r内含裸CR\n\n段落乙\n');
    act(() => entries()[0].click());
    expect(onAnnotateBlock).toHaveBeenLastCalledWith({ startLine: 1, endLine: 1 });
    act(() => entries()[1].click());
    expect(onAnnotateBlock).toHaveBeenLastCalledWith({ startLine: 3, endLine: 3 });
    unmount();
  });

  it('CRLF 内容：行范围按 \\n 模型映射', () => {
    const { entries, onAnnotateBlock, unmount } = renderPreviewBlocks('甲\r\n\r\n乙\r\n');
    act(() => entries()[0].click());
    expect(onAnnotateBlock).toHaveBeenLastCalledWith({ startLine: 1, endLine: 1 });
    act(() => entries()[1].click());
    expect(onAnnotateBlock).toHaveBeenLastCalledWith({ startLine: 3, endLine: 3 });
    unmount();
  });

  it('块级草稿态：目标块高亮 + 受控评论输入 + Esc/取消/提交回调', () => {
    const onDraftCommentChange = vi.fn();
    const onSubmitDraft = vi.fn();
    const onCancelDraft = vi.fn();
    const { container, unmount } = renderPreviewBlocks('第一段\n\n第二段\n', {
      draft: { startLine: 3, endLine: 3, comment: '草稿评论' },
      onDraftCommentChange,
      onSubmitDraft,
      onCancelDraft,
    });
    const draftBlocks = container.querySelectorAll('.md-block-draft');
    expect(draftBlocks).toHaveLength(1);
    expect(draftBlocks[0].textContent).toContain('第二段'); // 高亮锚定第二段
    const ta = container.querySelector<HTMLTextAreaElement>('.md-block-draft-input')!;
    expect(ta.value).toBe('草稿评论');
    act(() => {
      const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!;
      setter.call(ta, '改后的评论');
      ta.dispatchEvent(new Event('input', { bubbles: true }));
    });
    expect(onDraftCommentChange).toHaveBeenCalledWith('改后的评论');
    expect(container.textContent).toContain('第 3 行');
    // Esc 丢弃
    act(() => {
      ta.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    });
    expect(onCancelDraft).toHaveBeenCalledTimes(1);
    expect(onSubmitDraft).not.toHaveBeenCalled();
    // 提交与取消按钮
    const buttons = [...container.querySelectorAll<HTMLButtonElement>('.md-block-draft-actions button')];
    act(() => buttons.find((b) => b.textContent === '取消')!.click());
    expect(onCancelDraft).toHaveBeenCalledTimes(2);
    act(() => buttons.find((b) => b.textContent === '发布评论')!.click());
    expect(onSubmitDraft).toHaveBeenCalledTimes(1);
    unmount();
  });
});

describe('块级徽章（精确匹配/侧别/聚合/漂移/最内层归属）', () => {
  beforeEach(() => stubMatchMedia(false));

  function renderBadges(content: string, annotations: Annotation[]) {
    const onLocateAnnotations = vi.fn();
    const mounted = mount(
      <MarkdownPreview content={content} side="new" annotations={annotations} onLocateAnnotations={onLocateAnnotations} />,
    );
    return { ...mounted, onLocateAnnotations };
  }

  it('精确范围相等且侧别匹配才显示徽章；点击定位该条目', () => {
    const { container, onLocateAnnotations, unmount } = renderBadges('甲\n\n乙\n\n丙\n', [
      makeBlockAnn({ startLine: 3, endLine: 3 }),
      makeBlockAnn({ id: 'b2', startLine: 4, endLine: 4, comment: '无匹配块' }),
      makeBlockAnn({ id: 'b3', side: 'old', startLine: 1, endLine: 1, comment: '侧别不符' }),
    ]);
    const badges = container.querySelectorAll('.md-block-badge');
    expect(badges).toHaveLength(1);
    expect(badges[0].textContent).toContain('1');
    act(() => badges[0].dispatchEvent(new MouseEvent('click', { bubbles: true })));
    expect(onLocateAnnotations).toHaveBeenCalledWith(['b1']);
    unmount();
  });

  it('同块多条聚合：带数量徽章、悬停按（createdAt、id）顺序摘要、点击定位全部', () => {
    const { container, onLocateAnnotations, unmount } = renderBadges('甲\n\n乙\n\n丙\n', [
      makeBlockAnn({ id: 'later', createdAt: 200, comment: '后创建' }),
      makeBlockAnn({ id: 'earlier', createdAt: 90, comment: '先创建' }),
    ]);
    const badge = container.querySelector('.md-block-badge')!;
    expect(badge.textContent).toContain('2');
    const tips = [...badge.querySelectorAll('.md-block-badge-tip-item')].map((n) => n.textContent);
    expect(tips).toEqual(['先创建', '后创建']);
    act(() => badge.dispatchEvent(new MouseEvent('click', { bubbles: true })));
    expect(onLocateAnnotations).toHaveBeenCalledWith(['earlier', 'later']);
    unmount();
  });

  it('含已漂移批注的徽章显示漂移标识与摘要标识', () => {
    const { container, unmount } = renderBadges('甲\n\n乙\n\n丙\n', [
      makeBlockAnn({ stale: true, comment: '漂移评论' }),
    ]);
    expect(container.querySelector('.md-block-badge-stale')).not.toBeNull();
    expect(container.querySelector('.md-block-badge-tip')!.textContent).toContain('（已漂移）');
    unmount();
  });

  it('同范围嵌套块：徽章仅归属最内层（引用块与其内段落 range 相等时归段落）', () => {
    // '> 引用段'：blockquote 与其内 p 的行范围同为 (1,1)（tight list 不生成内层 p，故用引用块构造嵌套）
    const { container, unmount } = renderBadges('> 引用段落\n', [
      makeBlockAnn({ startLine: 1, endLine: 1 }),
    ]);
    const badges = container.querySelectorAll('.md-block-badge');
    expect(badges).toHaveLength(1);
    // 徽章直接挂在内层 p 上（最内层），blockquote 自身不重复显示
    expect(badges[0].parentElement!.tagName).toBe('P');
    const blockquote = container.querySelector('blockquote')!;
    expect(
      [...blockquote.children].some((el) => el.classList.contains('md-block-badge')),
    ).toBe(false);
    unmount();
  });

  it('范围不精确匹配任何可批注块的批注不显示标记（如跨块多行范围）', () => {
    const { container, unmount } = renderBadges('甲\n\n乙\n\n丙\n', [
      makeBlockAnn({ startLine: 1, endLine: 5 }),
      makeBlockAnn({ id: 'b2', startLine: 1, endLine: 2 }),
    ]);
    expect(container.querySelectorAll('.md-block-badge')).toHaveLength(0);
    unmount();
  });
});

describe('DiffViewer 草稿生命周期与数据契约（owner 为 DiffViewer）', () => {
  beforeEach(() => stubMatchMedia(false));

  it('提交创建：side=所在栏、闭区间行范围、±3 快照窗口照常构造', async () => {
    const onCreateAnnotation = vi.fn(async (_input: AnnotationCreateInput) => {});
    const { container, unmount } = renderViewerWithAnnotations({ onCreateAnnotation });
    await until(() => editorViews(container).length === 1);
    enterPreview(container);
    openBlockDraftOnParagraphB(container);
    const ta = container.querySelector<HTMLTextAreaElement>('.md-block-draft-input')!;
    act(() => {
      const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!;
      setter.call(ta, '给段落B的评论');
      ta.dispatchEvent(new Event('input', { bubbles: true }));
    });
    const publish = [...container.querySelectorAll<HTMLButtonElement>('.md-block-draft-actions button')].find(
      (b) => b.textContent === '发布评论',
    )!;
    await act(async () => {
      publish.click();
      await Promise.resolve();
    });
    await until(() => container.querySelector('.md-block-draft-box') === null);
    expect(onCreateAnnotation).toHaveBeenCalledTimes(1);
    const input = onCreateAnnotation.mock.calls[0][0] as AnnotationCreateInput;
    expect(input.side).toBe('new');
    expect(input.startLine).toBe(5);
    expect(input.endLine).toBe(5);
    expect(input.snapshotStartLine).toBe(2);
    expect(input.snapshotLineCount).toBe(7);
    expect(input.snapshot).toBe('\n段落A\n\n段落B\n\n段落C\n');
    expect(input.comment).toBe('给段落B的评论');
    unmount();
  });

  it('单侧预览：块级批注侧别按存在侧（纯新增 → new，纯删除 → old）', async () => {
    const onCreateAnnotation = vi.fn(async (_input: AnnotationCreateInput) => {});
    const added = renderViewerWithAnnotations({
      diff: makeMdDiff({ oldExists: false, oldContent: '', oldMode: '' }),
      onCreateAnnotation,
    });
    await until(() => editorViews(added.container).length === 1);
    enterPreview(added.container, 1);
    act(() => added.container.querySelector<HTMLButtonElement>('.md-block-annotate')!.click());
    const ta = added.container.querySelector<HTMLTextAreaElement>('.md-block-draft-input')!;
    act(() => {
      const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!;
      setter.call(ta, '新侧批注');
      ta.dispatchEvent(new Event('input', { bubbles: true }));
    });
    const publish = [...added.container.querySelectorAll<HTMLButtonElement>('.md-block-draft-actions button')].find(
      (b) => b.textContent === '发布评论',
    )!;
    await act(async () => {
      publish.click();
      await Promise.resolve();
    });
    await until(() => added.container.querySelector('.md-block-draft-box') === null);
    expect(onCreateAnnotation.mock.calls[0][0].side).toBe('new');
    added.unmount();

    const onCreateDeleted = vi.fn(async (_input: AnnotationCreateInput) => {});
    const deleted = renderViewerWithAnnotations({
      diff: makeMdDiff({ newExists: false, newContent: '', newMode: '' }),
      onCreateAnnotation: onCreateDeleted,
    });
    await until(() => editorViews(deleted.container).length === 1);
    enterPreview(deleted.container, 1);
    act(() => deleted.container.querySelector<HTMLButtonElement>('.md-block-annotate')!.click());
    const ta2 = deleted.container.querySelector<HTMLTextAreaElement>('.md-block-draft-input')!;
    act(() => {
      const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!;
      setter.call(ta2, '旧侧批注');
      ta2.dispatchEvent(new Event('input', { bubbles: true }));
    });
    const publish2 = [...deleted.container.querySelectorAll<HTMLButtonElement>('.md-block-draft-actions button')].find(
      (b) => b.textContent === '发布评论',
    )!;
    await act(async () => {
      publish2.click();
      await Promise.resolve();
    });
    await until(() => deleted.container.querySelector('.md-block-draft-box') === null);
    expect(onCreateDeleted.mock.calls[0][0].side).toBe('old');
    deleted.unmount();
  });

  it('空评论关闭即丢弃：不留存、不创建', async () => {
    const onCreateAnnotation = vi.fn(async (_input: AnnotationCreateInput) => {});
    const { container, unmount } = renderViewerWithAnnotations({ onCreateAnnotation });
    await until(() => editorViews(container).length === 1);
    enterPreview(container);
    openBlockDraftOnParagraphB(container);
    const publish = [...container.querySelectorAll<HTMLButtonElement>('.md-block-draft-actions button')].find(
      (b) => b.textContent === '发布评论',
    )!;
    await act(async () => {
      publish.click();
      await Promise.resolve();
    });
    await until(() => container.querySelector('.md-block-draft-box') === null);
    expect(onCreateAnnotation).not.toHaveBeenCalled();
    unmount();
  });

  it('Esc 关闭即丢弃；退出预览丢弃块级草稿', async () => {
    const onCreateAnnotation = vi.fn(async (_input: AnnotationCreateInput) => {});
    const { container, unmount } = renderViewerWithAnnotations({ onCreateAnnotation });
    await until(() => editorViews(container).length === 1);
    enterPreview(container);
    openBlockDraftOnParagraphB(container);
    const ta = container.querySelector<HTMLTextAreaElement>('.md-block-draft-input')!;
    act(() => {
      ta.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    });
    expect(container.querySelector('.md-block-draft-box')).toBeNull();

    // 重新打开草稿后退出预览：草稿被丢弃、源码模式无批注 UI 残留
    openBlockDraftOnParagraphB(container);
    expect(container.querySelector('.md-block-draft-box')).not.toBeNull();
    act(() =>
      [...container.querySelectorAll<HTMLButtonElement>('.diff-toolbar button')]
        .find((b) => b.textContent?.trim() === '预览')!
        .click(),
    );
    await until(() => editorViews(container).length === 1);
    expect(container.querySelector('.md-block-draft-box')).toBeNull();
    expect(container.querySelector('.ann-inline')).toBeNull(); // 源码草稿与块级草稿互斥
    expect(onCreateAnnotation).not.toHaveBeenCalled();
    unmount();
  });

  it('同 key 刷新：该侧内容变化清除该侧草稿；仅他侧变化时保留', async () => {
    const onCreateAnnotation = vi.fn(async (_input: AnnotationCreateInput) => {});
    const mounted = renderViewerWithAnnotations({ onCreateAnnotation });
    await until(() => editorViews(mounted.container).length === 1);
    enterPreview(mounted.container);
    openBlockDraftOnParagraphB(mounted.container);
    expect(mounted.container.querySelector('.md-block-draft-box')).not.toBeNull();

    // 仅旧侧内容变化：新侧草稿保留（该侧语义）
    rerender(
      mounted.root,
      <DiffViewer
        diff={makeMdDiff({ oldContent: '旧侧更新\n' })}
        path="a.md"
        annotations={[]}
        onCreateAnnotation={onCreateAnnotation}
        onLocateAnnotations={() => {}}
        modeOverride="unified"
        onModeChange={mounted.onModeChange}
        wrapOverride={false}
        onWrapChange={mounted.onWrapChange}
      />,
    );
    await until(() => mounted.container.textContent?.includes('段落B') ?? false);
    expect(mounted.container.querySelector('.md-block-draft-box')).not.toBeNull();

    // 新侧内容变化：草稿锚定失效 → 清除该侧草稿
    rerender(
      mounted.root,
      <DiffViewer
        diff={makeMdDiff({ oldContent: '旧侧更新\n', newContent: '# 标题\n\n段落A\n\n段落B改\n\n段落C\n\n段落D\n' })}
        path="a.md"
        annotations={[]}
        onCreateAnnotation={onCreateAnnotation}
        onLocateAnnotations={() => {}}
        modeOverride="unified"
        onModeChange={mounted.onModeChange}
        wrapOverride={false}
        onWrapChange={mounted.onWrapChange}
      />,
    );
    await until(() => mounted.container.textContent?.includes('段落B改') ?? false);
    expect(mounted.container.querySelector('.md-block-draft-box')).toBeNull();
    expect(onCreateAnnotation).not.toHaveBeenCalled();
    mounted.unmount();
  });

  it('徽章视图身份三元组隔离与点击定位（DiffViewer 层过滤 + 既有定位链路）', async () => {
    const onLocateAnnotations = vi.fn();
    const { container, unmount } = renderViewerWithAnnotations({
      annotations: [
        makeBlockAnn({ startLine: 3, endLine: 3 }), // ref='' 匹配当前视图 → 新侧栏徽章
        makeBlockAnn({
          id: 'other-ref',
          ref: 'HEAD',
          startLine: 3,
          endLine: 3,
          comment: 'HEAD来源批注',
        }), // 不同 ref：不匹配当前视图，任何栏都不显示
        makeBlockAnn({
          id: 'other-side',
          side: 'old',
          startLine: 3,
          endLine: 3,
          comment: '旧侧批注',
        }), // 他侧批注仅显示在他侧栏
      ],
      onLocateAnnotations,
    });
    await until(() => editorViews(container).length === 1);
    enterPreview(container);
    const panes = container.querySelectorAll('.md-diff-pane');
    // 新侧栏：仅当前视图的 b1；旧侧栏：仅他侧 other-side；HEAD 来源被三元组过滤
    expect(panes[1].querySelectorAll('.md-block-badge')).toHaveLength(1);
    expect(panes[0].querySelectorAll('.md-block-badge')).toHaveLength(1);
    expect(container.textContent).not.toContain('HEAD来源批注');
    // 点击新侧徽章 → 定位匹配条目（既有 onLocateAnnotations 链路）
    act(() =>
      panes[1]
        .querySelector('.md-block-badge')!
        .dispatchEvent(new MouseEvent('click', { bubbles: true })),
    );
    expect(onLocateAnnotations).toHaveBeenCalledWith(['b1']);
    unmount();
  });

  it('提交失败：错误提示显示且草稿保留（任何失败不呈现空白）', async () => {
    const onCreateAnnotation = vi.fn(async (_input: AnnotationCreateInput) => {
      throw new Error('创建失败');
    });
    const { container, unmount } = renderViewerWithAnnotations({ onCreateAnnotation });
    await until(() => editorViews(container).length === 1);
    enterPreview(container);
    openBlockDraftOnParagraphB(container);
    const ta = container.querySelector<HTMLTextAreaElement>('.md-block-draft-input')!;
    act(() => {
      const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!;
      setter.call(ta, '会失败的评论');
      ta.dispatchEvent(new Event('input', { bubbles: true }));
    });
    const publish = [...container.querySelectorAll<HTMLButtonElement>('.md-block-draft-actions button')].find(
      (b) => b.textContent === '发布评论',
    )!;
    await act(async () => {
      publish.click();
      await Promise.resolve();
    });
    await until(() => container.textContent?.includes('创建失败') ?? false);
    expect(container.querySelector('.md-block-draft-box')).not.toBeNull(); // 草稿保留可重试
    unmount();
  });
});

describe('Gate C 修复回归（ora-12 F1-F4）', () => {
  beforeEach(() => stubMatchMedia(false));

  it('F2 同一预览会话连续成功创建两条块级批注：busy 终态复位，第二次不被静默拒绝', async () => {
    const onCreateAnnotation = vi.fn(async (_input: AnnotationCreateInput) => {});
    const mounted = renderViewerWithAnnotations({ onCreateAnnotation });
    await until(() => editorViews(mounted.container).length === 1);
    enterPreview(mounted.container);

    const submitBlockComment = async (entryIndex: number, comment: string) => {
      const pane = mounted.container.querySelectorAll('.md-diff-pane')[1];
      const entries = pane.querySelectorAll<HTMLButtonElement>('.md-block-annotate');
      act(() => entries[entryIndex].click());
      const ta = mounted.container.querySelector<HTMLTextAreaElement>('.md-block-draft-input')!;
      act(() => {
        const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!;
        setter.call(ta, comment);
        ta.dispatchEvent(new Event('input', { bubbles: true }));
      });
      const publish = [
        ...mounted.container.querySelectorAll<HTMLButtonElement>('.md-block-draft-actions button'),
      ].find((b) => b.textContent === '发布评论')!;
      await act(async () => {
        publish.click();
        await Promise.resolve();
      });
    };

    await submitBlockComment(2, '第一条');
    await until(() => mounted.container.querySelector('.md-block-draft-box') === null);
    await submitBlockComment(3, '第二条');
    await until(() => onCreateAnnotation.mock.calls.length === 2);
    expect(onCreateAnnotation.mock.calls[0][0].startLine).toBe(5);
    expect(onCreateAnnotation.mock.calls[1][0].startLine).toBe(7);
    expect(mounted.container.querySelector('.md-block-draft-box')).toBeNull();
    mounted.unmount();
  });

  it('F3 他侧刷新不修改草稿状态：非空评论原样保留', async () => {
    const onCreateAnnotation = vi.fn(async (_input: AnnotationCreateInput) => {});
    const mounted = renderViewerWithAnnotations({ onCreateAnnotation });
    await until(() => editorViews(mounted.container).length === 1);
    enterPreview(mounted.container);
    openBlockDraftOnParagraphB(mounted.container);
    const before = mounted.container.querySelector<HTMLTextAreaElement>('.md-block-draft-input')!;
    act(() => {
      const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!;
      setter.call(before, '未提交的评论');
      before.dispatchEvent(new Event('input', { bubbles: true }));
    });

    // 仅旧侧内容变化：新侧草稿及其评论、错误态一律不动
    rerender(
      mounted.root,
      <DiffViewer
        diff={makeMdDiff({ oldContent: '旧侧更新\n' })}
        path="a.md"
        annotations={[]}
        onCreateAnnotation={onCreateAnnotation}
        onLocateAnnotations={() => {}}
        modeOverride="unified"
        onModeChange={mounted.onModeChange}
        wrapOverride={false}
        onWrapChange={mounted.onWrapChange}
      />,
    );
    await until(() => mounted.container.textContent?.includes('段落B') ?? false);
    const ta = mounted.container.querySelector<HTMLTextAreaElement>('.md-block-draft-input');
    expect(ta).not.toBeNull();
    expect(ta!.value).toBe('未提交的评论');
    mounted.unmount();
  });

  it('F3 同侧失败态刷新：草稿与关联错误提示一并清除', async () => {
    const onCreateAnnotation = vi.fn(async (_input: AnnotationCreateInput) => {
      throw new Error('创建失败');
    });
    const mounted = renderViewerWithAnnotations({ onCreateAnnotation });
    await until(() => editorViews(mounted.container).length === 1);
    enterPreview(mounted.container);
    openBlockDraftOnParagraphB(mounted.container);
    const ta = mounted.container.querySelector<HTMLTextAreaElement>('.md-block-draft-input')!;
    act(() => {
      const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!;
      setter.call(ta, '会失败的评论');
      ta.dispatchEvent(new Event('input', { bubbles: true }));
    });
    const publish = [...mounted.container.querySelectorAll<HTMLButtonElement>('.md-block-draft-actions button')].find(
      (b) => b.textContent === '发布评论',
    )!;
    await act(async () => {
      publish.click();
      await Promise.resolve();
    });
    await until(() => mounted.container.textContent?.includes('创建失败') ?? false);

    // 同侧（新侧）内容变化：草稿与 draftError 一并清除
    rerender(
      mounted.root,
      <DiffViewer
        diff={makeMdDiff({ newContent: '# 标题\n\n段落A\n\n段落B改\n\n段落C\n\n段落D\n' })}
        path="a.md"
        annotations={[]}
        onCreateAnnotation={onCreateAnnotation}
        onLocateAnnotations={() => {}}
        modeOverride="unified"
        onModeChange={mounted.onModeChange}
        wrapOverride={false}
        onWrapChange={mounted.onWrapChange}
      />,
    );
    await until(() => mounted.container.textContent?.includes('段落B改') ?? false);
    expect(mounted.container.querySelector('.md-block-draft-box')).toBeNull();
    expect(mounted.container.textContent).not.toContain('创建失败');
    mounted.unmount();
  });

  it('F4 段落草稿不产生 validateDOMNesting 警告（批注 UI 不作 p 的非法后代）', () => {
    const errors: string[] = [];
    const spy = vi
      .spyOn(console, 'error')
      .mockImplementation((...args: unknown[]) => {
        errors.push(args.map(String).join(' '));
      });
    try {
      const { container, unmount } = renderPreviewBlocks('第一段\n\n第二段\n', {
        draft: { startLine: 1, endLine: 1, comment: '草稿' },
        onDraftCommentChange: () => {},
        onSubmitDraft: () => {},
        onCancelDraft: () => {},
      });
      expect(container.querySelector('.md-block-draft-input')).not.toBeNull();
      // 高亮作用于合法包装层，语义块本身仍渲染
      expect(container.querySelectorAll('.md-block-draft')).toHaveLength(1);
      unmount();
    } finally {
      spy.mockRestore();
    }
    expect(errors.some((e) => e.includes('validateDOMNesting'))).toBe(false);
  });

  it('F5 同范围嵌套块的受控草稿：仅最内层渲染草稿 UI（单一高亮/输入框/草稿盒）', () => {
    // '> 引用段落'：blockquote 与内层 p 的 range 同为 (1,1)
    const { container, unmount } = renderPreviewBlocks('> 引用段落\n', {
      draft: { startLine: 1, endLine: 1, comment: '草稿' },
      onDraftCommentChange: () => {},
      onSubmitDraft: () => {},
      onCancelDraft: () => {},
    });
    expect(container.querySelectorAll('.md-block-draft-box')).toHaveLength(1);
    expect(container.querySelectorAll('.md-block-draft-input')).toHaveLength(1);
    expect(container.querySelectorAll('.md-block-draft')).toHaveLength(1);
    // 草稿盒归属内层 p 的包装层（p 是 phrasing-only，wrap 内含语义块与草稿盒）
    const wrap = container.querySelector('.md-block-draft')!;
    expect(wrap.querySelector('p')).not.toBeNull();
    expect(wrap.querySelector('.md-block-draft-box')).not.toBeNull();
    // blockquote 自身（外层）不渲染草稿盒/输入框（box 只属于内层 p 的包装层）
    const blockquote = container.querySelector('blockquote')!;
    expect(blockquote.querySelectorAll(':scope > .md-block-draft-box')).toHaveLength(0);
    expect(blockquote.querySelectorAll(':scope > .md-block-draft-input')).toHaveLength(0);
    unmount();
  });

  it('F6 大文档换行索引：显式构造一次，映射与朴素实现对拍（不经 React DOM、不计时钟）', () => {
    const filler = 'x'.repeat(3000);
    const parts: string[] = [];
    for (let i = 0; i < 1200; i++) {
      parts.push(`${filler}\n\n第${i}段\n`);
    }
    const content = parts.join('\n');
    const index = buildLineIndex(content); // 显式构造一次，后续全部复用
    // 对拍抽样：行首/行尾/换行符处/跨段边界/超长 offset
    const samples = [
      0, 1, 3000, 3001, 3016, 3017, 6033, 6034, content.length - 1, content.length, content.length + 99,
    ];
    for (const o of samples) {
      expect(lineAtByIndex(index, o)).toBe(lineAt(content, o));
    }
    // 块 position 走生产同一实现
    expect(blockRangeByIndex(index, { start: { offset: 3017 }, end: { offset: 6033 } })).toEqual(
      blockRange(content, { start: { offset: 3017 }, end: { offset: 6033 } }),
    );
  });

  it('F6 行映射复杂度与文档长度无关：大/小索引各 10k 次查询均为毫秒级数量级', () => {
    const big = buildLineIndex('x\n'.repeat(1000000)); // 100 万换行（~2MB 文档）
    const small = buildLineIndex('a\nb\nc\n');
    const bigLines = 1000000 + 1;
    const t1 = Date.now();
    let acc = 0;
    // 32749 与 1000001 互质：offset 均匀覆盖全文档（否则线性实现提前 break 无法区分复杂度）
    for (let i = 0; i < 30000; i++) acc += lineAtByIndex(big, (i * 32749) % bigLines);
    const bigMs = Date.now() - t1;
    const t2 = Date.now();
    for (let i = 0; i < 30000; i++) acc += lineAtByIndex(small, i % 6);
    const smallMs = Date.now() - t2;
    expect(acc).toBeGreaterThan(0); // 防止死代码消除
    // 二分：30000 次查询毫秒级；线性遍历实现在百万换行索引上退化到秒级（数量级断言，非窄墙钟）
    expect(bigMs).toBeLessThan(2000);
    expect(smallMs).toBeLessThan(2000);
  });

  it('F1 端到端 smoke：多块长文档渲染正确（不设墙钟阈值，复杂度回归见纯函数用例）', () => {
    const filler = 'x'.repeat(300);
    const parts: string[] = [];
    for (let i = 0; i < 200; i++) {
      parts.push(`${filler}\n\n第${i}段\n`);
    }
    const { entries, onAnnotateBlock, unmount } = renderPreviewBlocks(parts.join('\n'));
    const allEntries = entries();
    expect(allEntries.length).toBe(400);
    act(() => allEntries[0].click());
    expect(onAnnotateBlock).toHaveBeenLastCalledWith({ startLine: 1, endLine: 1 });
    act(() => allEntries[allEntries.length - 1].click());
    expect(onAnnotateBlock).toHaveBeenLastCalledWith({ startLine: 800 - 1, endLine: 800 - 1 });
    unmount();
  });

  it('提交代际：旧提交在换目标后完成，不得关闭或污染新草稿', async () => {
    let resolveInFlight!: () => void;
    const calls: AnnotationCreateInput[] = [];
    const onCreateAnnotation = vi.fn(
      (input: AnnotationCreateInput) =>
        new Promise<void>((resolve) => {
          calls.push(input);
          if (calls.length === 1) {
            resolveInFlight = resolve;
          } else {
            resolve();
          }
        }),
    );
    const mounted = renderViewerWithAnnotations({ onCreateAnnotation });
    await until(() => editorViews(mounted.container).length === 1);
    enterPreview(mounted.container);

    // 提交段落B（在途挂起）
    openBlockDraftOnParagraphB(mounted.container);
    const ta = mounted.container.querySelector<HTMLTextAreaElement>('.md-block-draft-input')!;
    act(() => {
      const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!;
      setter.call(ta, '第一条');
      ta.dispatchEvent(new Event('input', { bubbles: true }));
    });
    const publish = [...mounted.container.querySelectorAll<HTMLButtonElement>('.md-block-draft-actions button')].find(
      (b) => b.textContent === '发布评论',
    )!;
    await act(async () => {
      publish.click();
      await Promise.resolve();
    });
    expect(onCreateAnnotation).toHaveBeenCalledTimes(1);

    // 换目标：打开段落C 草稿并输入
    openBlockDraftInPane(mounted.container, 1, 3);
    const ta2 = mounted.container.querySelector<HTMLTextAreaElement>('.md-block-draft-input')!;
    expect(ta2.value).toBe('');
    expect(mounted.container.textContent).toContain('第 7 行'); // 新草稿目标为段落C
    act(() => {
      const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!;
      setter.call(ta2, '第二条');
      ta2.dispatchEvent(new Event('input', { bubbles: true }));
    });

    // 旧提交（段落B）此时完成：不得关闭新草稿、不得清空新评论
    await act(async () => {
      resolveInFlight();
      await Promise.resolve();
    });
    expect(mounted.container.querySelector('.md-block-draft-box')).not.toBeNull();
    expect(mounted.container.querySelector<HTMLTextAreaElement>('.md-block-draft-input')!.value).toBe('第二条');

    // 新草稿正常提交成功：两条批注创建、草稿关闭
    const publish2 = [...mounted.container.querySelectorAll<HTMLButtonElement>('.md-block-draft-actions button')].find(
      (b) => b.textContent === '发布评论',
    )!;
    await act(async () => {
      publish2.click();
      await Promise.resolve();
    });
    await until(() => mounted.container.querySelector('.md-block-draft-box') === null);
    expect(onCreateAnnotation).toHaveBeenCalledTimes(2);
    expect(calls[0].startLine).toBe(5);
    expect(calls[1].startLine).toBe(7);
    mounted.unmount();
  });

  it('徽章键盘可激活（Enter/Space）', () => {
    const onLocateAnnotations = vi.fn();
    const mounted = mount(
      <MarkdownPreview
        content="甲\n\n乙\n\n丙\n"
        side="new"
        annotations={[makeBlockAnn({ startLine: 1, endLine: 1 })]}
        onLocateAnnotations={onLocateAnnotations}
      />,
    );
    const badge = mounted.container.querySelector<HTMLElement>('.md-block-badge')!;
    act(() => {
      badge.dispatchEvent(
        new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }),
      );
    });
    expect(onLocateAnnotations).toHaveBeenCalledWith(['b1']);
    act(() => {
      badge.dispatchEvent(new KeyboardEvent('keydown', { key: ' ', bubbles: true }));
    });
    expect(onLocateAnnotations).toHaveBeenCalledTimes(2);
    mounted.unmount();
  });

  it('blockquote > p 嵌套：内层段落有独立批注入口，锚定自身 range', () => {
    const { entries, onAnnotateBlock, unmount } = renderPreviewBlocks('> 引用段落\n');
    expect(entries()).toHaveLength(2); // blockquote 容器块 + 内层 p
    act(() => entries()[1].click()); // 内层 p 入口
    expect(onAnnotateBlock).toHaveBeenLastCalledWith({ startLine: 1, endLine: 1 });
    unmount();
  });

  it('ora-13 F2: 评论按键不重新解析 markdown 子树；content 变化必须重新解析', () => {
    const mounted = mount(
      <MarkdownPreview
        content="甲\n\n乙\n"
        side="new"
        draft={{ startLine: 1, endLine: 1, comment: 'a' }}
        onDraftCommentChange={() => {}}
        onSubmitDraft={() => {}}
        onCancelDraft={() => {}}
      />,
    );
    const afterMount = mockParseCount.n;
    expect(afterMount).toBeGreaterThanOrEqual(1);

    // 按键（draft.comment 变化）：不得重新解析
    rerender(
      mounted.root,
      <MarkdownPreview
        content="甲\n\n乙\n"
        side="new"
        draft={{ startLine: 1, endLine: 1, comment: 'ab' }}
        onDraftCommentChange={() => {}}
        onSubmitDraft={() => {}}
        onCancelDraft={() => {}}
      />,
    );
    expect(mockParseCount.n).toBe(afterMount);
    // 受控输入仍实时更新（批注块经 Context 更新）
    expect(mounted.container.querySelector<HTMLTextAreaElement>('.md-block-draft-input')!.value).toBe('ab');

    // content 变化：必须重新解析
    rerender(
      mounted.root,
      <MarkdownPreview
        content="甲\n\n乙\n\n丙\n"
        side="new"
        draft={{ startLine: 1, endLine: 1, comment: 'ab' }}
        onDraftCommentChange={() => {}}
        onSubmitDraft={() => {}}
        onCancelDraft={() => {}}
      />,
    );
    expect(mockParseCount.n).toBe(afterMount + 1);
    mounted.unmount();
  });
});

describe('ora-13 F1: 源码/块级批注提交状态解耦（deferred 竞态）', () => {
  beforeEach(() => stubMatchMedia(false));

  function renderForRace() {
    // deferred 同时保存 resolve/reject：成功/失败竞态各走真实终态分支
    const deferred: Array<{ resolve: () => void; reject: (err: Error) => void }> = [];
    const onCreateAnnotation = vi.fn(
      (_input: AnnotationCreateInput) =>
        new Promise<void>((resolve, reject) => {
          deferred.push({ resolve, reject });
        }),
    );
    const mounted = renderViewerWithAnnotations({ onCreateAnnotation });
    return { ...mounted, onCreateAnnotation, deferred };
  }

  function openSourceDraft(container: HTMLElement) {
    const view = editorViews(container)[0];
    act(() => {
      view.dispatch({
        selection: { anchor: view.state.doc.line(1).from, head: view.state.doc.line(1).to },
      });
      container
        .querySelectorAll('.cm-line')[0]
        .dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
    });
    expect(container.querySelector('.ann-inline')).toBeTruthy();
  }

  function typeInto(ta: HTMLTextAreaElement, value: string) {
    act(() => {
      const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!;
      setter.call(ta, value);
      ta.dispatchEvent(new Event('input', { bubbles: true }));
    });
  }

  function clickPublish(scope: ParentNode) {
    const btn = [...scope.querySelectorAll<HTMLButtonElement>('button')].find(
      (b) => b.textContent === '发布评论',
    )!;
    act(() => {
      btn.click();
    });
    return btn;
  }

  it('旧源码提交成功不得清空预览中块级草稿的评论', async () => {
    const { container, deferred, onCreateAnnotation, unmount } = renderForRace();
    await until(() => editorViews(container).length === 1);

    // 源码 inline 草稿提交（在途挂起）
    openSourceDraft(container);
    typeInto(container.querySelector<HTMLTextAreaElement>('.ann-inline-input')!, '源码评论');
    clickPublish(container.querySelector('.ann-inline')!);
    expect(onCreateAnnotation).toHaveBeenCalledTimes(1);

    // 进入预览，打开块级草稿并输入
    enterPreview(container);
    openBlockDraftOnParagraphB(container);
    typeInto(container.querySelector<HTMLTextAreaElement>('.md-block-draft-input')!, '块级评论');

    // 旧源码提交成功完成：块级草稿与其评论不受影响
    await act(async () => {
      deferred[0]?.resolve();
      await Promise.resolve();
    });
    expect(container.querySelector('.md-block-draft-box')).not.toBeNull();
    expect(container.querySelector<HTMLTextAreaElement>('.md-block-draft-input')!.value).toBe('块级评论');
    unmount();
  });

  it('旧源码提交失败不得把错误污染为块级批注错误', async () => {
    const { container, deferred, onCreateAnnotation, unmount } = renderForRace();
    await until(() => editorViews(container).length === 1);

    // 源码 inline 草稿提交（在途挂起）→ 进入预览（源码草稿关闭）→ 开块级草稿并输入
    openSourceDraft(container);
    typeInto(container.querySelector<HTMLTextAreaElement>('.ann-inline-input')!, '源码评论');
    clickPublish(container.querySelector('.ann-inline')!);
    expect(onCreateAnnotation).toHaveBeenCalledTimes(1);
    enterPreview(container);
    openBlockDraftOnParagraphB(container);
    typeInto(container.querySelector<HTMLTextAreaElement>('.md-block-draft-input')!, '块级评论');

    // 此时旧源码提交失败（走 submitDraft catch 分支）：错误不得污染预览（错误态按草稿种类隔离）
    await act(async () => {
      deferred[0]?.reject(new Error('源码提交失败'));
      await Promise.resolve();
    });
    expect(container.querySelector('.md-block-draft-box')).not.toBeNull();
    expect(container.querySelector<HTMLTextAreaElement>('.md-block-draft-input')!.value).toBe('块级评论');
    expect(container.textContent).not.toContain('源码提交失败');
    unmount();
  });
});
