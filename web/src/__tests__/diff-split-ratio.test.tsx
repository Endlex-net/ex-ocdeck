// @vitest-environment jsdom
import { describe, it, expect, vi } from 'vitest';
import { act } from 'react';
import DiffViewer, { type DiffViewMode, type EditIO } from '../components/diff/DiffViewer';
import { MergeView } from '@codemirror/merge';
import { EditorView } from '@codemirror/view';
import type { FileEditRead, GitDiffResult } from '../types';
import { mount, rerender } from './cm-test-env';
import { drag, installPointerCaptureStubs, keyDown, pointer, stubStorage } from './dom-test-utils';

/* ============ D7 diff 并排分栏比例（fix-terminal-input-panel-resize 5.4/5.5 + Gate5 L1-L4） ============
 * 组件级接线验证（真实 CodeMirror 渲染路径）：异步 MergeView 创建后应用持久化比例、
 * 拖拽/键盘 → 布局即时应用 + pointerup/每次步进持久化、比例不进重建 deps（编辑器不重建）、
 * 单侧坍缩不应用/不显示/不访问存储且返回双侧恢复内存偏好、DOM 校验失败安全退出、
 * 仅存在性变化（不重建）清除残留内联比例（L1）、校验通过前 handle 不可交互（L2）、
 * 重建边界取消在途拖拽事务（L3）、真实编辑会话下 resize 无写回/重取/重建（L4）。 */

type ROCallback = (entries: ResizeObserverEntry[], observer: ResizeObserver) => void;

/** 可手动触发的 ResizeObserver 桩（覆盖 cm-test-env 的无操作桩；jsdom 无布局）。 */
class ROStub {
  static instances: ROStub[] = [];
  private cb: ROCallback;
  target?: Element;
  constructor(cb: ROCallback) {
    this.cb = cb;
  }
  observe(target: Element) {
    this.target = target;
    ROStub.instances.push(this);
  }
  unobserve() {}
  disconnect() {}
  fire() {
    this.cb([], this as unknown as ResizeObserver);
  }
}
(globalThis as { ResizeObserver: unknown }).ResizeObserver = ROStub;

installPointerCaptureStubs();
const { store, writes } = stubStorage();

const RATIO_KEY = 'ocdeck:diff-split-ratio';

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

interface DiffOpts {
  path?: string;
  mode?: DiffViewMode;
  wrap?: boolean;
  editIO?: EditIO;
  editModePreferred?: boolean;
}

function diffUI(diff: GitDiffResult, opts: DiffOpts = {}) {
  const onModeChange = vi.fn();
  const onWrapChange = vi.fn();
  const ui = (
    <DiffViewer
      diff={diff}
      path={opts.path ?? 'a.txt'}
      modeOverride={opts.mode ?? 'side-by-side'}
      onModeChange={onModeChange}
      wrapOverride={opts.wrap ?? false}
      onWrapChange={onWrapChange}
      editIO={opts.editIO}
      editModePreferred={opts.editModePreferred}
    />
  );
  return { ui, onModeChange, onWrapChange };
}

function mountDiff(diff: GitDiffResult, opts: DiffOpts = {}) {
  const { ui, onModeChange, onWrapChange } = diffUI(diff, opts);
  return { ...mount(ui), onModeChange, onWrapChange };
}

/** a/b 两侧的 .cm-mergeViewEditor wrapper（MergeView 固定 a→b 顺序 append）。 */
function wraps(container: HTMLElement): HTMLElement[] {
  return [...container.querySelectorAll<HTMLElement>('.cm-mergeViewEditor')];
}

/** 内联 style 属性原文。jsdom 的 style.flex getter 在 removeProperty 后返回陈旧值，不可靠。 */
function inlineStyleAttr(w: HTMLElement): string {
  return w.getAttribute('style') ?? '';
}

/** 挂载后给 .diff-editor 注入 clientWidth 并触发测量回调（jsdom 无布局，clientWidth 恒 0）。 */
function measureEditorWidth(container: HTMLElement, width: number): void {
  const editorEl = container.querySelector<HTMLElement>('.diff-editor')!;
  expect(editorEl, '.diff-editor 容器').toBeTruthy();
  Object.defineProperty(editorEl, 'clientWidth', { configurable: true, get: () => width });
  const inst = ROStub.instances.filter((o) => o.target === editorEl).pop();
  expect(inst, '容器宽度 observer').toBeTruthy();
  act(() => inst!.fire());
}

describe('D7 并排分栏比例（DiffViewer）', () => {
  it('异步 MergeView 创建完成后应用持久化比例（0.3 → a 30% / b 70%）', async () => {
    store.set(RATIO_KEY, '0.3');
    const { container, unmount } = mountDiff(makeDiff({}));
    await until(() => container.querySelectorAll('.cm-editor').length === 2);
    expect(wraps(container)[0].style.flex).toBe('0 0 30%');
    expect(wraps(container)[1].style.flex).toBe('0 0 70%');
    unmount();
  });

  it('无持久化时默认对半分（50% / 50%）', async () => {
    const { container, unmount } = mountDiff(makeDiff({}));
    await until(() => container.querySelectorAll('.cm-editor').length === 2);
    expect(wraps(container)[0].style.flex).toBe('0 0 50%');
    expect(wraps(container)[1].style.flex).toBe('0 0 50%');
    unmount();
  });

  it('拖拽：布局即时应用、pointerup 持久化一次；编辑器不重建、形态/换行回调不触发；刷新恢复', async () => {
    const destroy = vi.spyOn(MergeView.prototype, 'destroy');
    const first = mountDiff(makeDiff({}));
    await until(() => first.container.querySelectorAll('.cm-editor').length === 2);
    measureEditorWidth(first.container, 800);
    const editorsBefore = [...first.container.querySelectorAll('.cm-editor')];
    const handle = first.container.querySelector<HTMLElement>('.od-resize-handle')!;
    expect(handle).not.toBeNull();

    drag(handle, [400, 480]); // +80/800 → 0.6
    // jsdom 序列化 flex 简写会去掉数值尾零（30.00% → 30%）
    await until(() => wraps(first.container)[0].style.flex === '0 0 60%');
    expect(store.get(RATIO_KEY)).toBe('0.6');
    expect(writes.count).toBe(1);
    expect(destroy).not.toHaveBeenCalled(); // 比例不进重建 deps
    const editorsAfter = [...first.container.querySelectorAll('.cm-editor')];
    expect(editorsAfter[0]).toBe(editorsBefore[0]);
    expect(editorsAfter[1]).toBe(editorsBefore[1]);
    expect(first.onModeChange).not.toHaveBeenCalled();
    expect(first.onWrapChange).not.toHaveBeenCalled();
    first.unmount();

    const again = mountDiff(makeDiff({}));
    await until(() => again.container.querySelectorAll('.cm-editor').length === 2);
    expect(wraps(again.container)[0].style.flex).toBe('0 0 60%'); // 刷新恢复
    again.unmount();
    destroy.mockRestore();
  });

  it('键盘步进按 10px/容器宽换算并立即持久化', async () => {
    const { container, unmount } = mountDiff(makeDiff({}));
    await until(() => container.querySelectorAll('.cm-editor').length === 2);
    measureEditorWidth(container, 800);
    keyDown(container.querySelector<HTMLElement>('.od-resize-handle')!, 'ArrowRight'); // +10/800
    expect(store.get(RATIO_KEY)).toBe('0.5125');
    expect(wraps(container)[0].style.flex).toBe('0 0 51.25%');
    unmount();
  });

  it('单侧坍缩（纯新增）：把手移除、重建后 wrapper 无内联比例、不访问存储；返回双侧恢复内存偏好', async () => {
    store.set(RATIO_KEY, '0.3');
    const getSpy = vi.spyOn(globalThis.localStorage, 'getItem');
    const first = mountDiff(makeDiff({}));
    await until(() => first.container.querySelectorAll('.cm-editor').length === 2);
    expect(wraps(first.container)[0].style.flex).toBe('0 0 30%');
    expect(getSpy.mock.calls.filter(([k]) => k === RATIO_KEY)).toHaveLength(1); // 首次启用读一次

    // 纯新增（旧侧不存在 → a 侧坍缩）
    rerender(first.root, diffUI(makeDiff({ oldExists: false, oldContent: '', oldMode: '' })).ui);
    await until(
      () =>
        first.container.querySelector('.diff-collapse-a') !== null &&
        first.container.querySelectorAll('.cm-editor').length === 2,
    );
    expect(first.container.querySelector('.od-resize-handle')).toBeNull();
    for (const w of wraps(first.container)) {
      expect(w.style.flex).toBe(''); // 坍缩走 .diff-collapse CSS，内联比例不产生不可见占位
    }
    expect(getSpy.mock.calls.filter(([k]) => k === RATIO_KEY)).toHaveLength(1); // 禁用期不访问存储
    expect(writes.count).toBe(0);

    // 返回双侧：内存偏好 0.3 恢复（不经存储往返）
    rerender(first.root, diffUI(makeDiff({})).ui);
    await until(() => first.container.querySelector('.diff-collapse-a') === null);
    expect(wraps(first.container)[0].style.flex).toBe('0 0 30%');
    expect(wraps(first.container)[1].style.flex).toBe('0 0 70%');
    expect(writes.count).toBe(0);
    first.unmount();
  });

  it('非并排形态与预览态不显示把手（enabled 谓词）', async () => {
    const first = mountDiff(makeDiff({}));
    await until(() => first.container.querySelectorAll('.cm-editor').length === 2);
    expect(first.container.querySelector('.od-resize-handle')).not.toBeNull();

    // 切单列（经 modeOverride 重渲染，形态切换走销毁-重建路径）：handle 消失
    rerender(first.root, diffUI(makeDiff({}), { mode: 'unified' }).ui);
    await until(() => first.container.querySelectorAll('.cm-editor').length === 1);
    expect(first.container.querySelector('.od-resize-handle')).toBeNull();
    first.unmount();

    // 预览态（markdown 才有预览开关）：handle 消失
    const md = mountDiff(makeDiff({ newContent: '# 标题\n\n正文\n' }), { path: 'a.md' });
    await until(() => md.container.querySelectorAll('.cm-editor').length === 2);
    expect(md.container.querySelector('.od-resize-handle')).not.toBeNull();
    const previewBtn = [
      ...md.container.querySelectorAll<HTMLButtonElement>('.diff-toolbar button'),
    ].find((b) => b.textContent?.includes('预览'));
    act(() => previewBtn!.click());
    await until(() => md.container.querySelectorAll('.cm-editor').length === 0);
    expect(md.container.querySelector('.od-resize-handle')).toBeNull();
    md.unmount();
  });

  it('DOM 校验失败安全退出：把手禁用移除、不写布局/比例、console.warn', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const { container, unmount } = mountDiff(makeDiff({}));
    await until(() => container.querySelectorAll('.cm-editor').length === 2);
    measureEditorWidth(container, 800);
    const wrapA = wraps(container)[0];
    wrapA.classList.remove('cm-mergeViewEditor'); // 破坏 D7 校验前提
    const handle = container.querySelector<HTMLElement>('.od-resize-handle')!;

    act(() => handle.dispatchEvent(pointer('pointerdown', 400)));
    act(() => handle.dispatchEvent(pointer('pointermove', 480))); // 0.6 → applySplitRatio 失败
    await until(() => container.querySelector('.od-resize-handle') === null);
    act(() => handle.dispatchEvent(pointer('pointerup', 480))); // 迟到提交：事务已随 handle 卸载取消
    expect(store.has(RATIO_KEY)).toBe(false);
    expect(writes.count).toBe(0);
    expect(wrapA.style.flex).toBe('0 0 50%'); // 布局未被改写（保持创建时默认）
    expect(warn).toHaveBeenCalled();
    warn.mockRestore();
    unmount();
  });

  // ---------- Gate5 L1：仅存在性变化（重建 deps 不含存在性 → 不重建） ----------

  it('L1 内容不变仅 oldExists 翻转（不重建）：清除残留内联比例；翻回双侧恢复内存偏好', async () => {
    const destroy = vi.spyOn(MergeView.prototype, 'destroy');
    store.set(RATIO_KEY, '0.3');
    const doubleSided = makeDiff({ oldContent: 'a\n', newContent: 'b\n' });
    const first = mountDiff(doubleSided);
    await until(() => first.container.querySelectorAll('.cm-editor').length === 2);
    expect(wraps(first.container)[0].style.flex).toBe('0 0 30%');

    // 同一内容串仅翻转存在性：不销毁重建，但残留的内联比例必须被清除（否则压过 .diff-collapse-a）
    rerender(
      first.root,
      diffUI(makeDiff({ oldContent: 'a\n', newContent: 'b\n', oldExists: false, oldMode: '' })).ui,
    );
    await until(() => first.container.querySelector('.diff-collapse-a') !== null);
    expect(destroy).not.toHaveBeenCalled(); // 存在性不进重建 deps
    for (const w of wraps(first.container)) expect(inlineStyleAttr(w)).toBe('');
    expect(first.container.querySelector('.od-resize-handle')).toBeNull();
    expect(writes.count).toBe(0);

    // 翻回双侧（仍未重建）：同一实例重新校验并应用内存偏好
    rerender(first.root, diffUI(doubleSided).ui);
    await until(() => first.container.querySelector('.diff-collapse-a') === null);
    expect(destroy).not.toHaveBeenCalled();
    expect(wraps(first.container)[0].style.flex).toBe('0 0 30%');
    expect(wraps(first.container)[1].style.flex).toBe('0 0 70%');
    expect(first.container.querySelector('.od-resize-handle')).not.toBeNull();
    first.unmount();
    destroy.mockRestore();
  });

  it('L1 内容不变仅 newExists 翻转（纯删除方向，不重建）：同样清除并恢复', async () => {
    const doubleSided = makeDiff({ oldContent: 'a\n', newContent: 'b\n' });
    const first = mountDiff(doubleSided);
    await until(() => first.container.querySelectorAll('.cm-editor').length === 2);
    rerender(
      first.root,
      diffUI(makeDiff({ oldContent: 'a\n', newContent: 'b\n', newExists: false, newMode: '' })).ui,
    );
    await until(() => first.container.querySelector('.diff-collapse-b') !== null);
    for (const w of wraps(first.container)) expect(inlineStyleAttr(w)).toBe('');
    rerender(first.root, diffUI(doubleSided).ui);
    await until(() => first.container.querySelector('.diff-collapse-b') === null);
    expect(wraps(first.container)[0].style.flex).toBe('0 0 50%');
    expect(wraps(first.container)[1].style.flex).toBe('0 0 50%');
    first.unmount();
  });

  it('L1 单侧直接挂载 → 翻回双侧（不重建）：异步创建使用最新存在性，翻回后比例经校验重新应用', async () => {
    store.set(RATIO_KEY, '0.3');
    const pureAdd = makeDiff({ oldExists: false, oldContent: '', oldMode: '', newContent: 'b\n' });
    const first = mountDiff(pureAdd);
    await until(() => first.container.querySelectorAll('.cm-editor').length === 2);
    expect(first.container.querySelector('.diff-collapse-a')).not.toBeNull();
    expect(wraps(first.container)[0].style.flex).toBe(''); // 单侧创建：不应用比例
    expect(first.container.querySelector('.od-resize-handle')).toBeNull();

    // oldContent/newContent 均不变（仅 oldExists 翻转）：同一实例重新校验 → 应用持久化比例
    rerender(
      first.root,
      diffUI(
        makeDiff({ oldExists: true, oldContent: '', oldMode: '100644', newContent: 'b\n' }),
      ).ui,
    );
    await until(() => first.container.querySelector('.diff-collapse-a') === null);
    expect(wraps(first.container)[0].style.flex).toBe('0 0 30%');
    expect(wraps(first.container)[1].style.flex).toBe('0 0 70%');
    expect(first.container.querySelector('.od-resize-handle')).not.toBeNull();
    expect(writes.count).toBe(0);
    first.unmount();
  });

  // ---------- Gate5 L2：校验通过前 handle 不可交互 ----------

  it('L2 异步创建等待期 handle 不渲染（未校验不可交互），就绪后出现', async () => {
    const first = mountDiff(makeDiff({}));
    expect(first.container.querySelector('.od-resize-handle')).toBeNull(); // seamReady 初始 false
    await until(() => first.container.querySelectorAll('.cm-editor').length === 2);
    expect(first.container.querySelector('.od-resize-handle')).not.toBeNull(); // 创建且校验通过
    first.unmount();
  });

  it('L2 校验失败时键盘提交被拒：不写存储/布局，handle 移除', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const { container, unmount } = mountDiff(makeDiff({}));
    await until(() => container.querySelectorAll('.cm-editor').length === 2);
    measureEditorWidth(container, 800);
    const wrapA = wraps(container)[0];
    wrapA.classList.remove('cm-mergeViewEditor'); // 创建成功后破坏 wrapper 关系
    keyDown(container.querySelector<HTMLElement>('.od-resize-handle')!, 'ArrowRight');
    expect(store.has(RATIO_KEY)).toBe(false); // 提交被拒
    expect(writes.count).toBe(0);
    expect(wrapA.style.flex).toBe('0 0 50%'); // 布局未被改写
    expect(container.querySelector('.od-resize-handle')).toBeNull(); // 校验失败 → 禁用移除
    expect(warn).toHaveBeenCalled();
    warn.mockRestore();
    unmount();
  });

  // ---------- Gate5 L5：代际 token 按对象身份比较，回切相同输入不复用旧就绪代际 ----------

  it('L5 回切到相同输入（A→unified→A）：新实例创建+校验完成前 handle 不提前挂载', async () => {
    const first = mountDiff(makeDiff({})); // 配置 A
    await until(() => first.container.querySelectorAll('.cm-editor').length === 2);
    expect(first.container.querySelector('.od-resize-handle')).not.toBeNull(); // A 就绪

    // 切 unified：代际 token 变化 → handle 卸载，旧实例销毁
    rerender(first.root, diffUI(makeDiff({}), { mode: 'unified' }).ui);
    await until(() => first.container.querySelectorAll('.cm-editor').length === 1);
    expect(first.container.querySelector('.od-resize-handle')).toBeNull();

    // 回切 A：输入值与第一轮完全相同，但代际是新 token——
    // 旧就绪代际不得因「值相等」复用（同步断言：异步创建+DOM 校验尚未完成，handle MUST NOT 存在）
    rerender(first.root, diffUI(makeDiff({}), { mode: 'side-by-side' }).ui);
    expect(first.container.querySelector('.od-resize-handle')).toBeNull();
    await until(() => first.container.querySelectorAll('.cm-editor').length === 2);
    expect(first.container.querySelector('.od-resize-handle')).not.toBeNull(); // 创建+校验完成后出现
    first.unmount();
  });

  // ---------- Gate5 L3：重建边界取消在途拖拽事务（失效先于承载实例销毁） ----------

  it('L3 拖动中换行切换触发重建：capture 释放（取消）先于旧实例 destroy、零存储写入、迟到 pointerup 无效', async () => {
    const first = mountDiff(makeDiff({}));
    await until(() => first.container.querySelectorAll('.cm-editor').length === 2);
    measureEditorWidth(first.container, 800);
    const oldHandle = first.container.querySelector<HTMLElement>('.od-resize-handle')!;

    // 顺序探针：destroy 入口检查旧 handle 的 pointer capture 是否已释放
    //（render 期失效判定 → handle 同提交卸载且 layout cleanup 取消事务 → 之后 passive cleanup 才 destroy）
    let captureAtDestroy: boolean | null = null;
    const realDestroy = MergeView.prototype.destroy;
    const destroy = vi
      .spyOn(MergeView.prototype, 'destroy')
      .mockImplementation(function (this: MergeView) {
        captureAtDestroy = oldHandle.hasPointerCapture(1);
        return realDestroy.call(this);
      });

    act(() => oldHandle.dispatchEvent(pointer('pointerdown', 400)));
    act(() => oldHandle.dispatchEvent(pointer('pointermove', 480))); // in-memory 0.6
    expect(wraps(first.container)[0].style.flex).toBe('0 0 60%');
    expect(store.has(RATIO_KEY)).toBe(false); // 拖动中仅内存
    expect(oldHandle.hasPointerCapture(1)).toBe(true); // 重建前事务活跃（capture 未释放）

    // 拖动中重建（wrapOverride 变化）：handle 在该次提交即卸载（layout cleanup 取消事务）
    rerender(first.root, diffUI(makeDiff({}), { wrap: true }).ui);
    expect(captureAtDestroy).toBe(false); // destroy 执行时事务已取消（capture 已释放）
    expect(destroy).toHaveBeenCalledTimes(1);
    expect(store.has(RATIO_KEY)).toBe(false);
    expect(writes.count).toBe(0);
    await until(() => first.container.querySelectorAll('.cm-content.cm-lineWrapping').length === 2);

    // 新实例创建并校验通过后恢复交互：应用恢复后的起始比例，而非拖拽中间值
    await until(() => first.container.querySelector('.od-resize-handle') !== null);
    expect(wraps(first.container)[0].style.flex).toBe('0 0 50%');

    // 迟到 pointerup 落在已卸载的旧节点：React 不派发，无提交
    act(() => oldHandle.dispatchEvent(pointer('pointerup', 480)));
    act(() => oldHandle.dispatchEvent(pointer('lostpointercapture', 480)));
    expect(store.has(RATIO_KEY)).toBe(false);
    expect(writes.count).toBe(0);
    first.unmount();
    destroy.mockRestore();
  });

  // ---------- Gate5 L4：真实编辑会话下的 resize ----------

  it('L4 编辑会话下拖拽：无写回/重取/重建，会话保留且拖后仍可编辑写回', async () => {
    const destroy = vi.spyOn(MergeView.prototype, 'destroy');
    const read = vi.fn().mockResolvedValue({
      editable: true,
      content: 'const a = 2;\n',
      baseHash: 'h0',
      lineEnding: 'lf',
      hasBom: false,
      mode: '100644',
    } satisfies FileEditRead);
    const write = vi.fn().mockResolvedValue({ baseHash: 'h1' });
    const first = mountDiff(makeDiff({}), {
      editIO: { read, write },
      editModePreferred: true, // 资格 eligible 后自动进入编辑
    });
    await until(() =>
      [...first.container.querySelectorAll('.cm-content')].some(
        (el) => el.getAttribute('contenteditable') === 'true',
      ),
    );
    const readCallsAtEntry = read.mock.calls.length;
    const destroyCallsAtEntry = destroy.mock.calls.length; // 进入编辑本身有一次合法重建
    measureEditorWidth(first.container, 800);
    const editorsBefore = [...first.container.querySelectorAll('.cm-editor')];

    drag(first.container.querySelector<HTMLElement>('.od-resize-handle')!, [400, 480]); // 0.6
    await until(() => wraps(first.container)[0].style.flex === '0 0 60%');

    // 无重取、无写回、无重建；编辑器 DOM 节点同一（编辑会话未被清除）
    expect(read.mock.calls.length).toBe(readCallsAtEntry);
    expect(write).not.toHaveBeenCalled();
    expect(destroy.mock.calls.length).toBe(destroyCallsAtEntry);
    const editorsAfter = [...first.container.querySelectorAll('.cm-editor')];
    expect(editorsAfter[0]).toBe(editorsBefore[0]);
    expect(editorsAfter[1]).toBe(editorsBefore[1]);

    // 独立排除「拖拽本身安排了防抖保存」：跨过保存防抖窗口后仍必须零写回
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 700));
    });
    expect(write).not.toHaveBeenCalled();

    // 会话存活的强断言：拖拽后 b 侧仍可编辑并触发一次防抖写回
    const editableContent = [
      ...first.container.querySelectorAll<HTMLElement>('.cm-content'),
    ].find((el) => el.getAttribute('contenteditable') === 'true');
    expect(editableContent).toBeTruthy();
    const view = EditorView.findFromDOM(editableContent!);
    expect(view).toBeTruthy();
    act(() => {
      view!.dispatch({ changes: { from: view!.state.doc.length, insert: '\nappended' } });
    });
    await until(() => write.mock.calls.length > 0);
    expect(write.mock.calls[0][0].content).toContain('appended');
    first.unmount();
    destroy.mockRestore();
  });
});
