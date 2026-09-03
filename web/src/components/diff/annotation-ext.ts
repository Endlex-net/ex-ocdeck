import { StateEffect, StateField, type Extension } from '@codemirror/state';
import {
  Decoration,
  EditorView,
  GutterMarker,
  ViewPlugin,
  WidgetType,
  gutter,
  type DecorationSet,
} from '@codemirror/view';
import { getChunks, getOriginalDoc } from '@codemirror/merge';
import type { Annotation, DiffSide } from '../../types';

/* ============================ 批注 CM 扩展（diff-review-workbench tasks 5.2） ============================
 * 查看模式：gutter 弱标记（同行聚合带数量，悬停摘要，点击定位）+ 选区候选高亮 + 手势捕获。
 * 标记按 (path, ref, untracked) 三元组 + side 在上层过滤后传入；本模块只认行号映射。 */

/** 一次批注手势：单侧连续行范围（1-based 闭区间）。 */
export interface AnnotationGesture {
  side: DiffSide;
  startLine: number;
  endLine: number;
}

// ---------- 行号 → 批注聚合 ----------

export const setAnnotationsEffect = StateEffect.define<Map<number, Annotation[]>>();

export const annotationMapField = StateField.define<Map<number, Annotation[]>>({
  create: () => new Map(),
  update(value, tr) {
    for (const e of tr.effects) {
      if (e.is(setAnnotationsEffect)) value = e.value;
    }
    return value;
  },
});

export function pushLine(map: Map<number, Annotation[]>, line: number, a: Annotation): void {
  const arr = map.get(line);
  if (arr) arr.push(a);
  else map.set(line, [a]);
}

/** 侧-by-侧编辑器：批注按本侧行号直接落入（截到文档行数内）。 */
export function sideMarkerMap(annotations: Annotation[], docLines: number): Map<number, Annotation[]> {
  const map = new Map<number, Annotation[]>();
  for (const a of annotations) {
    const from = Math.max(1, a.startLine);
    const to = Math.min(a.endLine, docLines);
    for (let l = from; l <= to; l++) pushLine(map, l, a);
  }
  return map;
}

/** unified 编辑器：new 侧按文档行号；old 侧经 chunks 锚定到删除块所在文档行
 *  （找不到锚点=无法原位显示，仅列表展示，见 spec 批注锚定状态）。 */
export function unifiedMarkerMap(
  annotations: Annotation[],
  view: EditorView,
): Map<number, Annotation[]> {
  const map = new Map<number, Annotation[]>();
  const info = getChunks(view.state);
  const original = info ? getOriginalDoc(view.state) : null;
  for (const a of annotations) {
    if (a.side === 'new') {
      const to = Math.min(a.endLine, view.state.doc.lines);
      for (let l = Math.max(1, a.startLine); l <= to; l++) pushLine(map, l, a);
      continue;
    }
    if (!info || !original || a.startLine > original.lines) continue;
    // 注意 Text.line() 按行号取值；lineAt 按位置（误用会把行号当偏移）
    const pos = original.line(a.startLine).from;
    // endA 为块末行内容结尾（不含换行），用 <= 覆盖空删除行（pos==endA）
    const chunk = info.chunks.find((c) => c.fromA !== c.toA && pos >= c.fromA && pos <= c.endA);
    if (!chunk) continue;
    const docLine = view.state.doc.lineAt(Math.min(chunk.fromB, view.state.doc.length)).number;
    pushLine(map, docLine, a);
  }
  return map;
}

// ---------- gutter 标记 ----------

function markerTitle(items: Annotation[]): string {
  return items
    .map(
      (a) =>
        `${a.stale ? '[已漂移] ' : ''}${a.side === 'old' ? '旧侧' : '新侧'} L${a.startLine}${
          a.endLine > a.startLine ? `-${a.endLine}` : ''
        }：${a.comment}`,
    )
    .join('\n');
}

class AnnotationMarker extends GutterMarker {
  constructor(
    readonly items: Annotation[],
    private readonly onLocate: (ids: string[]) => void,
  ) {
    super();
  }
  override eq(other: AnnotationMarker): boolean {
    return (
      other.items.length === this.items.length &&
      other.items.every(
        (o, i) =>
          o.id === this.items[i].id &&
          o.revision === this.items[i].revision &&
          o.stale === this.items[i].stale &&
          o.comment === this.items[i].comment,
      )
    );
  }
  override toDOM(): Node {
    const span = document.createElement('span');
    const stale = this.items.some((a) => a.stale);
    span.className = `cm-annotationDot${stale ? ' cm-annotationDot-stale' : ''}`;
    if (this.items.length > 1) span.textContent = String(this.items.length);
    span.title = markerTitle(this.items);
    // 点击定位挂在标记自身：gutter 级 domEventHandlers 按高度反推行号（jsdom 无布局恒为行 1），不可用
    span.addEventListener('mousedown', (e) => {
      e.preventDefault();
      e.stopPropagation();
      this.onLocate(this.items.map((a) => a.id));
    });
    return span;
  }
}

/** 批注 gutter（行号右侧弱标记；点击定位列表）。 */
export function annotationGutter(onLocate: (ids: string[]) => void): Extension {
  return [
    annotationMapField,
    gutter({
      class: 'cm-annotationGutter',
      lineMarker(view, line) {
        const items = view.state.field(annotationMapField).get(view.state.doc.lineAt(line.from).number);
        return items && items.length > 0 ? new AnnotationMarker(items, onLocate) : null;
      },
      lineMarkerChange(update) {
        return update.transactions.some((tr) => tr.effects.some((e) => e.is(setAnnotationsEffect)));
      },
    }),
  ];
}

// ---------- 选区候选高亮 ----------

export const setCandidateEffect = StateEffect.define<{ from: number; to: number } | null>();

const candidateField = StateField.define<DecorationSet>({
  create: () => Decoration.none,
  update(deco, tr) {
    deco = deco.map(tr.changes);
    for (const e of tr.effects) {
      if (e.is(setCandidateEffect)) {
        deco = e.value
          ? Decoration.set([
              Decoration.mark({ class: 'cm-annotationCandidate' }).range(e.value.from, e.value.to),
            ])
          : Decoration.none;
      }
    }
    return deco;
  },
  provide: (f) => EditorView.decorations.from(f),
});

export const annotationCandidateExtension: Extension = candidateField;

// ---------- 内联批注区（批注 6：参考 GitLab，在最后选中行下方切开内联卡片，替代悬浮气泡） ----------

/** 打开/关闭内联批注区：line=1-based 目标行（区域插入该行下方）；host 由调用方持有（React portal 挂载点）。 */
export const setInlineRegionEffect = StateEffect.define<{ line: number; host: HTMLElement } | null>();

class InlineRegionWidget extends WidgetType {
  constructor(readonly host: HTMLElement) {
    super();
  }
  override eq(other: InlineRegionWidget): boolean {
    return other.host === this.host;
  }
  override toDOM(): HTMLElement {
    this.host.classList.add('ann-inline-host');
    return this.host;
  }
  override get estimatedHeight(): number {
    return 140;
  }
  // 不覆盖 ignoreEvent：默认全部忽略 = 编辑器不拦截 widget 内事件，
  // textarea/按钮等表单控件获得原生交互（覆盖为 false 反而会让编辑器抢事件）
}

const inlineRegionField = StateField.define<DecorationSet>({
  create: () => Decoration.none,
  update(deco, tr) {
    deco = deco.map(tr.changes);
    for (const e of tr.effects) {
      if (e.is(setInlineRegionEffect)) {
        if (e.value) {
          const line = Math.min(Math.max(1, e.value.line), tr.state.doc.lines);
          deco = Decoration.set([
            Decoration.widget({
              widget: new InlineRegionWidget(e.value.host),
              side: 1, // 目标行之后
              block: true,
            }).range(tr.state.doc.line(line).to),
          ]);
        } else {
          deco = Decoration.none;
        }
      }
    }
    return deco;
  },
  provide: (f) => EditorView.decorations.from(f),
});

export const inlineRegionExtension: Extension = inlineRegionField;

/**
 * 不换行长行修复：内联批注区宿主嵌在 .cm-content 内容流内，块级宽度跟随内容宽度
 * （不换行时 = 最长行，远超可视宽度），横向滚动会把卡片右缘（取消/发布按钮）推出视口。
 * 这里把宿主宽度钉到 scroller 可视宽度（clientWidth 减去 gutters 等左侧静态偏移），
 * 配合 CSS `position: sticky; left: 0` 让卡片横向滚动时整体钉在可视区内。
 * jsdom 等无真实布局环境量到 0 → 清除内联宽度，回退默认块级行为（换行场景不受影响）。
 */
export function syncInlineHostWidth(view: EditorView, host: HTMLElement): void {
  const scroller = view.scrollDOM;
  // content 相对 scroller 内容原点的静态左偏移（gutters 宽度）：
  // 渲染 rect 已含 scrollLeft 平移，加回即得静态偏移
  const left =
    view.contentDOM.getBoundingClientRect().left -
    scroller.getBoundingClientRect().left +
    scroller.scrollLeft -
    scroller.clientLeft;
  const width = scroller.clientWidth - Math.max(0, left);
  host.style.width = width > 0 ? `${width}px` : '';
  // sticky 偏移 = gutters 静态宽度：卡片钉在内容区左缘（而非滚到 scroller 左缘被
  // sticky gutters 遮住左边条/输入区）
  host.style.left = width > 0 && left > 0 ? `${left}px` : '';
}

// ---------- 手势捕获 ----------

/** unified 视图删除行（.cm-deletedLine widget）→ 旧侧行号（经 chunks 映射，spec side 映射规则）。 */
export function oldLineForDeleted(view: EditorView, el: Element): number | null {
  const info = getChunks(view.state);
  if (!info) return null;
  const original = getOriginalDoc(view.state);
  let pos: number;
  try {
    pos = view.posAtDOM(el, 0);
  } catch {
    return null;
  }
  for (const chunk of info.chunks) {
    if (chunk.fromA === chunk.toA) continue; // 纯插入块无删除行
    if (pos < chunk.fromB || pos > chunk.endB) continue;
    const firstLine = original.lineAt(chunk.fromA).number;
    const lastLine = original.lineAt(chunk.endA).number;
    return Math.min(firstLine + deletedLineIndex(el), lastLine);
  }
  return null;
}

function deletedLineIndex(el: Element): number {
  const parent = el.parentElement;
  if (!parent) return 0;
  let i = 0;
  for (const child of Array.from(parent.children)) {
    if (!child.classList.contains('cm-deletedLine')) continue;
    if (child === el) return i;
    i++;
  }
  return 0;
}

/** F15：跨编辑器拖拽标记——document 级捕获监听先于目标编辑器的 view.dom mouseup 触发，
 *  标记后目标编辑器跳过本次 mouseup（含其旧选区回退）。 */
const crossSideMarked = new WeakSet<MouseEvent>();

/**
 * 查看模式批注手势（tasks 5.2，spec「框选段落+点击行双支持」）：
 * - 行号 gutter 点击 → 单行批注（unified 恒 new 侧；并排按编辑器所在侧）；
 * - 普通代码行单击 → 单行批注；拖选（非空选区）→ 行范围批注；
 * - unified 删除行（.cm-deletedLine）→ old 侧：单击单行、拖拽多行范围（块内行序）；
 * - 混合侧选择（删除块 ↔ 普通内容互相拖拽、并排跨编辑器 A 按下 B 释放）→ onCrossSide
 *   拒绝提示，不产生批注（spec「跨侧不创建并提示」）。
 *
 * 注意：EditorView.domEventHandlers 挂在 contentDOM 上，gutter 在其之外、删除块是
 * DeletionWidget（ignoreEvent 吞掉事件）——这两类事件永远到不了编辑器级 handler。
 * 因此手势统一用 view.dom 捕获阶段监听（ViewPlugin）+ 拖拽起点跟踪；
 * 跨编辑器释放经 ownerDocument 级监听协调（F15：CM 自身也把拖选 mouseup 挂在 ownerDocument）。
 */
export function annotationGestures(opts: {
  unified: boolean;
  side: DiffSide;
  onGesture: (g: AnnotationGesture) => void;
  /** 混合侧选区拒绝提示（不产生批注）。 */
  onCrossSide?: () => void;
}): Extension {
  const captureGestures = ViewPlugin.define((view) => {
    /** 拖拽起点：old=删除块内按下（记旧侧行号）；内容侧按下记行号（无则 null 回退选区）。 */
    let dragStart: { kind: 'old' | 'content'; line: number | null } | null = null;
    /** F14：mousedown 已触发批注（行号单击）的序列，mouseup 不再回退旧选区。 */
    let firedOnDown = false;
    const contentSide: DiffSide = opts.unified ? 'new' : opts.side;

    const fire = (side: DiffSide, startLine: number, endLine: number) =>
      opts.onGesture({ side, startLine, endLine });

    const deletedLineOf = (target: HTMLElement): { el: Element; line: number } | null => {
      const el = target.closest('.cm-deletedLine');
      if (!el) return null;
      const line = oldLineForDeleted(view, el);
      return line === null ? null : { el, line };
    };

    /** 指针所在内容行行号（DOM 反推，选区在 jsdom/怪异指针路径下不可靠时的主依据）。 */
    const contentLineOf = (target: HTMLElement): number | null => {
      const lineEl = target.closest('.cm-line');
      if (!lineEl) return null;
      try {
        return view.state.doc.lineAt(view.posAtDOM(lineEl, 0)).number;
      } catch {
        return null; // 目标不在文档内（装饰/widget）
      }
    };

    const onMouseDown = (event: MouseEvent) => {
      const target = event.target as HTMLElement | null;
      dragStart = null;
      firedOnDown = false;
      if (!target) return;
      if (target.closest('.cm-annotationGutter')) return; // 标记 gutter 自有定位逻辑
      if (opts.unified) {
        const del = deletedLineOf(target);
        if (del) {
          dragStart = { kind: 'old', line: del.line };
          event.preventDefault(); // 删除块是 widget，阻止其内文本选择
          return;
        }
      }
      const gutterEl = target.closest('.cm-lineNumbers .cm-gutterElement');
      if (gutterEl) {
        // 行号元素文本即行号（默认 formatNumber=String），不依赖布局几何（jsdom 友好）
        const ln = Number.parseInt((gutterEl.textContent ?? '').trim(), 10);
        if (Number.isInteger(ln) && ln >= 1 && ln <= view.state.doc.lines) {
          fire(contentSide, ln, ln);
          firedOnDown = true;
          event.preventDefault();
        }
        return;
      }
      if (target.closest('.cm-content')) {
        dragStart = { kind: 'content', line: contentLineOf(target) };
      }
    };

    const onMouseUp = (event: MouseEvent) => {
      if (crossSideMarked.has(event)) return; // F15：已被起点编辑器的 document 级监听判为跨侧
      const start = dragStart;
      dragStart = null;
      const fired = firedOnDown;
      firedOnDown = false;
      const target = event.target as HTMLElement | null;
      if (!target) return;
      const upDel = opts.unified ? deletedLineOf(target) : null;
      if (start?.kind === 'old' && start.line !== null) {
        // 旧侧起手：落在删除块 → 单行或范围；落在普通内容 → 混合侧拒绝
        if (upDel) {
          fire('old', Math.min(start.line, upDel.line), Math.max(start.line, upDel.line));
        } else if (target.closest('.cm-content')) {
          opts.onCrossSide?.();
        }
        return;
      }
      if (upDel) {
        // 内容侧起手（或无起手）落在删除块 → 混合侧拒绝
        if (start?.kind === 'content') opts.onCrossSide?.();
        return;
      }
      // F14：mousedown 已开批注的序列、gutter/标记等已明确处理的目标——不回退旧选区
      if (fired) return;
      if (target.closest('.cm-gutters')) return;
      // F16：合法目标白名单——mouseup 必须落在真实内容行才允许回退/指针映射；
      // .cm-mergeSpacer/.cm-gap/.cm-collapsedLines 等非行 widget 一律不产生批注（不再枚举黑名单）
      if (!target.closest('.cm-line')) return;
      // 内容侧：按下/抬起行号都可得时以指针真实路径为准（单击=同行单行，跨行=范围）
      if (start?.kind === 'content') {
        if (start.line === null) return; // 在非行内容（对齐空白/间隙 widget）按下 → 不回退
        const upLine = contentLineOf(target);
        if (upLine !== null) {
          fire(contentSide, Math.min(start.line, upLine), Math.max(start.line, upLine));
        }
        return; // 抬起行不可判定 → 不回退
      }
      // 无 mousedown 起手（键盘/程序化选区）且落在真实行 → 回退到 CM 选区
      const sel = view.state.selection.main;
      if (!sel.empty) {
        const from = view.state.doc.lineAt(sel.from);
        const to = view.state.doc.lineAt(sel.to);
        fire(contentSide, from.number, to.number);
      }
    };

    /** F15：document 级 mouseup——释放在本编辑器之外时收尾：落在另一编辑器 = 跨侧拒绝并标记事件。 */
    const onDocMouseUp = (event: MouseEvent) => {
      if (!dragStart) return; // 无进行中拖拽
      const target = event.target as HTMLElement | null;
      if (target && view.dom.contains(target)) return; // 本编辑器释放，由 onMouseUp 处理
      dragStart = null;
      firedOnDown = false;
      if (target?.closest('.cm-editor')) {
        crossSideMarked.add(event);
        opts.onCrossSide?.();
      }
    };

    const doc = view.dom.ownerDocument;
    view.dom.addEventListener('mousedown', onMouseDown, true);
    view.dom.addEventListener('mouseup', onMouseUp, true);
    doc.addEventListener('mouseup', onDocMouseUp, true);
    return {
      destroy() {
        view.dom.removeEventListener('mousedown', onMouseDown, true);
        view.dom.removeEventListener('mouseup', onMouseUp, true);
        doc.removeEventListener('mouseup', onDocMouseUp, true);
      },
    };
  });

  return [captureGestures];
}
