import { EditorState, type Extension, type Range } from '@codemirror/state';
import { getChunks, type Chunk } from '@codemirror/merge';
import {
  Decoration,
  ViewPlugin,
  type DecorationSet,
  type EditorView,
  type ViewUpdate,
} from '@codemirror/view';

/* ============================ 纯新增行标记（diff 配色风格） ============================
 * 行类无法区分「修改」与「纯新增」（b 侧同为 .cm-changedLine、unified 同为 .cm-inlineChangedLine），
 * 这里在组件层按 chunk 精确分类：纯插入块（fromA === toA，旧侧范围为空）覆盖的行打上行级类
 * cm-diff-added，供 CSS 的 goland 风格（html[data-diff-style="goland"]）标绿消费。
 * 仅挂载在只读 diff 视图的 b 侧/unified（a 侧无纯新增语义，且 chunk 偏移是 b 文档
 * 坐标，不得施到 a 文档），编辑模式不参与。 */

/** 取当前 merge chunks（无 merge 态/未完成初始化时为 null）；以数组引用判断是否需要重算装饰。 */
function chunksOf(state: EditorState): readonly Chunk[] | null {
  return getChunks(state)?.chunks ?? null;
}

function pureAddedDecorations(state: EditorState, chunks: readonly Chunk[] | null): DecorationSet {
  if (!chunks) return Decoration.none;
  const doc = state.doc;
  const ranges: Range<Decoration>[] = [];
  for (const chunk of chunks) {
    if (chunk.fromA !== chunk.toA) continue; // 修改/删除块不上类（goland 下按「修改」统一蓝）
    // chunk to 指向块末行的下一行行首（可能越过文档末尾），行范围用 endB 收口
    const toLine = doc.lineAt(Math.min(chunk.endB, doc.length)).number;
    for (let line = doc.lineAt(chunk.fromB).number; line <= toLine; line++) {
      ranges.push(Decoration.line({ class: 'cm-diff-added' }).range(doc.line(line).from));
    }
  }
  return Decoration.set(ranges, true);
}

export const pureAddedLineExtension: Extension = ViewPlugin.fromClass(
  class {
    decorations: DecorationSet;
    private chunks: readonly Chunk[] | null;

    constructor(view: EditorView) {
      this.chunks = chunksOf(view.state);
      this.decorations = pureAddedDecorations(view.state, this.chunks);
    }

    update(update: ViewUpdate) {
      const next = chunksOf(update.view.state);
      if (next === this.chunks) return;
      this.chunks = next;
      this.decorations = pureAddedDecorations(update.view.state, next);
    }
  },
  { decorations: (v) => v.decorations },
);
