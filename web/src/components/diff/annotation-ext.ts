import { StateEffect, StateField, type Extension } from '@codemirror/state';
import {
  Decoration,
  EditorView,
  GutterMarker,
  ViewPlugin,
  gutter,
  type DecorationSet,
} from '@codemirror/view';
import { getChunks, getOriginalDoc } from '@codemirror/merge';
import type { Annotation, DiffSide } from '../../types';

/* ============================ 批注 CM 扩展（diff-review-workbench tasks 5.2） ============================
 * 查看模式：gutter 弱标记（同行聚合带数量，悬停摘要，点击定位）+ 选区候选高亮 + 手势捕获。
 * 标记按 (path, ref, untracked) 三元组 + side 在上层过滤后传入；本模块只认行号映射。 */

/** 一次批注手势：单侧连续行范围（1-based 闭区间）+ 指针坐标（气泡锚点）。 */
export interface AnnotationGesture {
  side: DiffSide;
  startLine: number;
  endLine: number;
  x: number;
  y: number;
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

/**
 * 查看模式批注手势（tasks 5.2）：
 * - 行号 gutter 点击 → 单行批注（unified 恒 new 侧；并排按编辑器所在侧）；
 * - 拖选（非空选区）→ 行范围批注；选区天然不跨侧（每编辑器独立），跨侧映射在上层防御；
 * - unified 下点击 .cm-deletedLine → 旧侧单行批注。
 *
 * 注意：EditorView.domEventHandlers 挂在 contentDOM 上，gutter 在其之外、删除块是
 * DeletionWidget（ignoreEvent 吞掉事件）——这两类点击永远到不了编辑器级 handler。
 * 因此 mousedown 手势用 view.dom 捕获阶段监听（ViewPlugin），mouseup 拖选仍走编辑器级。
 */
export function annotationGestures(opts: {
  unified: boolean;
  side: DiffSide;
  onGesture: (g: AnnotationGesture) => void;
}): Extension {
  const captureClicks = ViewPlugin.define((view) => {
    const onMouseDown = (event: MouseEvent) => {
      const target = event.target as HTMLElement | null;
      if (!target) return;
      if (target.closest('.cm-annotationGutter')) return; // 标记 gutter 自有定位逻辑
      if (opts.unified) {
        const deleted = target.closest('.cm-deletedLine');
        if (deleted) {
          const ln = oldLineForDeleted(view, deleted);
          if (ln !== null) {
            opts.onGesture({
              side: 'old',
              startLine: ln,
              endLine: ln,
              x: event.clientX,
              y: event.clientY,
            });
            event.preventDefault();
          }
          return;
        }
      }
      const gutterEl = target.closest('.cm-lineNumbers .cm-gutterElement');
      if (gutterEl) {
        // 行号元素文本即行号（默认 formatNumber=String），不依赖布局几何（jsdom 友好）
        const ln = Number.parseInt((gutterEl.textContent ?? '').trim(), 10);
        if (Number.isInteger(ln) && ln >= 1 && ln <= view.state.doc.lines) {
          opts.onGesture({
            side: opts.unified ? 'new' : opts.side,
            startLine: ln,
            endLine: ln,
            x: event.clientX,
            y: event.clientY,
          });
          event.preventDefault();
        }
      }
    };
    view.dom.addEventListener('mousedown', onMouseDown, true);
    return {
      destroy() {
        view.dom.removeEventListener('mousedown', onMouseDown, true);
      },
    };
  });

  return [
    captureClicks,
    EditorView.domEventHandlers({
      mouseup(event, view) {
        const sel = view.state.selection.main;
        if (sel.empty) return false;
        const from = view.state.doc.lineAt(sel.from);
        const to = view.state.doc.lineAt(sel.to);
        opts.onGesture({
          side: opts.unified ? 'new' : opts.side,
          startLine: from.number,
          endLine: to.number,
          x: event.clientX,
          y: event.clientY,
        });
        return false; // 不拦截默认选区行为
      },
    }),
  ];
}
