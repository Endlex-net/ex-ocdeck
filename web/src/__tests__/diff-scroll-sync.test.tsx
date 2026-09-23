// @vitest-environment jsdom
import { describe, it, expect, vi } from 'vitest';
import { act } from 'react';
import DiffViewer, { type DiffViewMode } from '../components/diff/DiffViewer';
import { MergeView } from '@codemirror/merge';
import { EditorView } from '@codemirror/view';
import type { GitDiffResult } from '../types';
import { mount, rerender } from './cm-test-env';

/* ============ D1 并排 diff 横向滚动协同（git-page-enhancements 4.1–4.3） ============
 * jsdom 无布局：Object.defineProperty 注入 scrollLeft/scrollWidth/clientWidth，
 * 派发合成 scroll 事件驱动协同 handler（沿用 diff-split-ratio.test.tsx 的 DOM 检查风格）。
 * 必测矩阵（design「必测错误矩阵」前端行冻结）：clamp 写入目标侧且 echo 不回拉源侧、
 * 交错序列（A→B 程序写入 → B 在 echo 前被用户拖动 → B 旧 echo 迟到）收敛不回拉、
 * 双侧→单侧→双侧（无重建、单侧期间零滚动事件）恢复后首次滚动不消费旧 pending、
 * 同值竞态例外自愈、快速交替拖动收敛无振荡、换行/unified/单侧坍缩不启用。 */

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

function diffUI(diff: GitDiffResult, opts: { mode?: DiffViewMode; wrap?: boolean } = {}) {
  return (
    <DiffViewer
      diff={diff}
      path="a.txt"
      modeOverride={opts.mode ?? 'side-by-side'}
      onModeChange={vi.fn()}
      wrapOverride={opts.wrap ?? false}
      onWrapChange={vi.fn()}
    />
  );
}

/** 依 DOM 顺序取 a/b 两侧 EditorView（MergeView 固定 a→b 顺序 append）。 */
function sideViews(container: HTMLElement): [EditorView, EditorView] {
  const els = [...container.querySelectorAll<HTMLElement>('.cm-content')];
  expect(els).toHaveLength(2);
  const views = els.map((el) => EditorView.findFromDOM(el)!);
  expect(views[0]).toBeTruthy();
  expect(views[1]).toBeTruthy();
  return [views[0], views[1]];
}

/** 注入一侧横向滚动几何：clientWidth 固定 100，scrollWidth = 100 + max（最大可滚 max px）。 */
function stubScrollGeometry(view: EditorView, max: number): void {
  const dom = view.scrollDOM;
  let left = 0;
  Object.defineProperty(dom, 'scrollLeft', {
    configurable: true,
    get: () => left,
    set: (v: number) => {
      left = Math.max(0, Math.min(v, max));
    },
  });
  Object.defineProperty(dom, 'scrollWidth', { configurable: true, get: () => 100 + max });
  Object.defineProperty(dom, 'clientWidth', { configurable: true, get: () => 100 });
}

/** 模拟用户把一侧滚到 offset（写 scrollLeft）并派发浏览器 scroll 事件。 */
function userScroll(view: EditorView, offset: number): void {
  view.scrollDOM.scrollLeft = offset;
  view.scrollDOM.dispatchEvent(new Event('scroll'));
}

describe('D1 并排横向滚动协同（DiffViewer）', () => {
  it('两侧最大可滚动距离不同：clamp 写入目标侧，目标侧 echo 不回拉源侧', async () => {
    const { container, unmount } = mount(diffUI(makeDiff({})));
    await until(() => container.querySelectorAll('.cm-editor').length === 2);
    const [a, b] = sideViews(container);
    stubScrollGeometry(a, 1000);
    stubScrollGeometry(b, 500);

    userScroll(a, 900); // 用户滚旧侧至 900
    expect(b.scrollDOM.scrollLeft).toBe(500); // clamp 到目标侧最大可行偏移
    expect(a.scrollDOM.scrollLeft).toBe(900); // 源侧保持用户偏移

    b.scrollDOM.dispatchEvent(new Event('scroll')); // b 的程序性 echo 到达
    expect(a.scrollDOM.scrollLeft).toBe(900); // echo 消费，MUST NOT 回拉源侧
    expect(b.scrollDOM.scrollLeft).toBe(500);
    unmount();
  });

  it('交错序列：A→B 程序写入 → B 在 echo 前被用户拖动 → B 旧 echo 迟到，源侧不回拉、最终收敛', async () => {
    const { container, unmount } = mount(diffUI(makeDiff({})));
    await until(() => container.querySelectorAll('.cm-editor').length === 2);
    const [a, b] = sideViews(container);
    stubScrollGeometry(a, 1000);
    stubScrollGeometry(b, 500);

    userScroll(a, 900); // A 用户滚动 → B 程序写入 500（echo 在途）
    expect(b.scrollDOM.scrollLeft).toBe(500);
    userScroll(b, 300); // B 在 echo 到达前被用户拖到 300 → A 程序写入 300
    expect(a.scrollDOM.scrollLeft).toBe(300);

    b.scrollDOM.dispatchEvent(new Event('scroll')); // B 旧 echo（值 500）迟到：读当前值 300 不匹配 → 用户事件分支
    expect(a.scrollDOM.scrollLeft).toBe(300); // 经相等短路收敛，源侧不回拉
    expect(b.scrollDOM.scrollLeft).toBe(300);

    a.scrollDOM.dispatchEvent(new Event('scroll')); // A 的 echo（300）到达 → 消费
    expect(a.scrollDOM.scrollLeft).toBe(300); // 稳定收敛，无振荡
    expect(b.scrollDOM.scrollLeft).toBe(300);
    unmount();
  });

  it('双侧→单侧→双侧（无重建、单侧期间零滚动事件）：恢复后首次滚动不消费旧 pending', async () => {
    const destroy = vi.spyOn(MergeView.prototype, 'destroy');
    const first = mount(diffUI(makeDiff({})));
    await until(() => first.container.querySelectorAll('.cm-editor').length === 2);
    const [a, b] = sideViews(first.container);
    stubScrollGeometry(a, 1000);
    stubScrollGeometry(b, 1000);

    userScroll(a, 600); // A 用户滚动 → B 程序写入 600（B 的 echo 不派发，留在途）
    expect(b.scrollDOM.scrollLeft).toBe(600);

    // 翻单侧（仅翻转存在性、内容串不变 → 不重建编辑器）
    rerender(first.root, diffUI(makeDiff({ oldExists: false, oldMode: '' })));
    await until(() => first.container.querySelector('.diff-collapse-a') !== null);
    expect(destroy).not.toHaveBeenCalled(); // 存在性不进重建 deps

    // 单侧期间零滚动事件（冻结用例前提）；翻回双侧
    rerender(first.root, diffUI(makeDiff({})));
    await until(() => first.container.querySelector('.diff-collapse-a') === null);
    expect(destroy).not.toHaveBeenCalled();

    // 恢复后首次滚动：若旧 pending(b) 未被启用谓词 effect 清理，此事件会被误消费为 echo、跳过同步
    a.scrollDOM.scrollLeft = 0; // 模拟坍缩窗口内的布局位移（无事件派发）
    userScroll(b, 600);
    expect(a.scrollDOM.scrollLeft).toBe(600); // 首次滚动必须作为用户事件执行同步
    first.unmount();
    destroy.mockRestore();
  });

  it('同值竞态例外：pending 含当前偏移的同值事件按 echo 消费，下一事件另一侧立即追上且无循环', async () => {
    const { container, unmount } = mount(diffUI(makeDiff({})));
    await until(() => container.querySelectorAll('.cm-editor').length === 2);
    const [a, b] = sideViews(container);
    stubScrollGeometry(a, 1000);
    stubScrollGeometry(b, 1000);

    userScroll(a, 300); // A 用户滚动 → B 程序写入 300，echo 在途
    expect(b.scrollDOM.scrollLeft).toBe(300);

    b.scrollDOM.scrollLeft = 300; // 用户恰在 echo 在途期间把 B 滚至同值偏移（同值竞态）
    b.scrollDOM.dispatchEvent(new Event('scroll'));
    expect(a.scrollDOM.scrollLeft).toBe(300); // 同值例外：允许本次不同步（按 echo 消费）

    userScroll(b, 450); // 下一事件：B 用户滚动到 450
    expect(a.scrollDOM.scrollLeft).toBe(450); // 另一侧立即追上（自愈）

    a.scrollDOM.dispatchEvent(new Event('scroll')); // A 的 echo（450）到达 → 消费
    b.scrollDOM.dispatchEvent(new Event('scroll')); // B 迟到旧 echo（450 已被消费）：相等短路
    expect(a.scrollDOM.scrollLeft).toBe(450); // 收敛，无持续事件循环
    expect(b.scrollDOM.scrollLeft).toBe(450);
    unmount();
  });

  it('快速交替拖动：迟到 echo 不误判、最终收敛无振荡（两侧最大可滚动距离不同）', async () => {
    const { container, unmount } = mount(diffUI(makeDiff({})));
    await until(() => container.querySelectorAll('.cm-editor').length === 2);
    const [a, b] = sideViews(container);
    stubScrollGeometry(a, 1000);
    stubScrollGeometry(b, 500);

    // 交替拖动（echo 滞后于下一次用户事件——浏览器滚动事件迟到/合并的真实形态）
    userScroll(a, 900); // A 用户 → B 程序写入 500（clamp）
    userScroll(b, 100); // B 用户（B 旧 echo 未到）→ A 程序写入 100
    b.scrollDOM.dispatchEvent(new Event('scroll')); // B 迟到旧 echo（500）：当前值 100 不匹配 → 相等短路收敛
    a.scrollDOM.dispatchEvent(new Event('scroll')); // A echo（100）消费
    userScroll(a, 700); // A 用户 → B 程序写入 500（clamp）
    b.scrollDOM.dispatchEvent(new Event('scroll')); // B echo（500）消费

    expect(a.scrollDOM.scrollLeft).toBe(700); // 源侧保持用户偏移不被回拉
    expect(b.scrollDOM.scrollLeft).toBe(500); // 目标侧停在其最大可行偏移
    unmount();
  });

  it('换行 / unified / 单侧坍缩不启用协同', async () => {
    // 换行：滚动事件不联动另一侧
    const wrapped = mount(diffUI(makeDiff({}), { wrap: true }));
    await until(() => wrapped.container.querySelectorAll('.cm-editor').length === 2);
    const [wa, wb] = sideViews(wrapped.container);
    stubScrollGeometry(wa, 1000);
    stubScrollGeometry(wb, 1000);
    userScroll(wa, 300);
    expect(wb.scrollDOM.scrollLeft).toBe(0);
    wrapped.unmount();

    // unified：单列形态无 a/b 双侧
    const unified = mount(diffUI(makeDiff({}), { mode: 'unified' }));
    await until(() => unified.container.querySelectorAll('.cm-editor').length === 1);
    expect(unified.container.querySelectorAll('.cm-mergeViewEditor')).toHaveLength(0);
    unified.unmount();

    // 单侧坍缩：存在内容侧独立横向滚动，空侧不被同步写入
    const single = mount(
      diffUI(makeDiff({ oldExists: false, oldContent: '', oldMode: '', newContent: 'b\n' })),
    );
    await until(() => single.container.querySelectorAll('.cm-editor').length === 2);
    const [sa, sb] = sideViews(single.container);
    stubScrollGeometry(sa, 1000);
    stubScrollGeometry(sb, 1000);
    userScroll(sb, 400); // 内容侧（b）用户滚动
    expect(sa.scrollDOM.scrollLeft).toBe(0); // 空侧（a）不被写入
    expect(sb.scrollDOM.scrollLeft).toBe(400); // 内容侧保持独立滚动（不被回拉）
    single.unmount();
  });
});
