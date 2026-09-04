import type { Annotation, DiffSide, GitDiffResult } from '../../types';

/* ============================ diff review 纯函数助手（diff-review-workbench D4/D8） ============================
 * 快照构造、视图三元组匹配、批注排序——全部不依赖 CodeMirror / React，便于单测。 */

/** 视图身份三元组（design D8 前端落点）：同路径 staged/unstaged/untracked 是不同视图。 */
export interface ViewTriple {
  path: string;
  ref: string;
  untracked: boolean;
}

export function sameTriple(a: ViewTriple, b: ViewTriple): boolean {
  return a.path === b.path && a.ref === b.ref && a.untracked === b.untracked;
}

/** 批注按视图三元组过滤：同路径不同来源的批注互不串标记。 */
export function filterByTriple(annotations: Annotation[], view: ViewTriple): Annotation[] {
  return annotations.filter(
    (a) => a.path === view.path && a.ref === view.ref && a.untracked === view.untracked,
  );
}

/** 批注排序键唯一（design D7 / spec 行内标记）：（createdAt 升序，平局 id 字典序）。 */
export function sortAnnotations(list: Annotation[]): Annotation[] {
  return [...list].sort((a, b) =>
    a.createdAt !== b.createdAt ? a.createdAt - b.createdAt : a.id < b.id ? -1 : a.id > b.id ? 1 : 0,
  );
}

/** 侧内容来源：old → oldContent，new → newContent（原始 GitDiffResult 字符串，含行尾 \r）。 */
export function sideContent(diff: GitDiffResult, side: DiffSide): string {
  return side === 'old' ? diff.oldContent : diff.newContent;
}

/** 与 CM `Text.of(content.split('\n'))` 一致的行数（空串 = 1 行；'a\n' = 2 行，末行为空行）。 */
export function lineCountOf(content: string): number {
  return content === '' ? 1 : content.split('\n').length;
}

export interface SnapshotWindow {
  snapshot: string;
  snapshotStartLine: number;
  snapshotLineCount: number;
}

/**
 * 快照窗口构造（design D4，唯一来源）：
 * 从原始侧内容按 1-based 闭区间行号切取，窗口 = 选中段前后各 3 行（文件边界裁短），
 * 按 '\n' 切分保留行尾 '\r'，MUST NOT 取自 CM state.doc（会吞掉 CRLF）。
 */
export function buildSnapshot(content: string, startLine: number, endLine: number): SnapshotWindow {
  const lines = content.split('\n');
  const total = lineCountOf(content);
  const s = Math.min(Math.max(1, startLine), total);
  const e = Math.min(Math.max(s, endLine), total);
  const w0 = Math.max(1, s - 3);
  const w1 = Math.min(total, e + 3);
  return {
    snapshot: lines.slice(w0 - 1, w1).join('\n'),
    snapshotStartLine: w0,
    snapshotLineCount: w1 - w0 + 1,
  };
}

/** 编辑入口本地门禁（design D5/spec「编辑的特殊文件边界」）。
 *  仅本地可判定的条件；GET 编辑读取 editable=false 的原因在点击后展示。 */
export interface EditGate {
  ok: boolean;
  reason: string;
}

const MODE_SYMLINK = '120000';
const MODE_GITLINK = '160000';

export function editGateFor(
  diff: GitDiffResult,
  renderedMerge: boolean,
  stateText: string | null,
): EditGate {
  if (diff.isBinary) return { ok: false, reason: '二进制文件不可编辑' };
  if (diff.oldMode === MODE_GITLINK || diff.newMode === MODE_GITLINK) {
    return { ok: false, reason: '子模块（gitlink）不可编辑' };
  }
  if (!diff.newExists) return { ok: false, reason: '新侧不存在（文件已删除），不可编辑' };
  if (diff.newMode === MODE_SYMLINK) return { ok: false, reason: '符号链接不可编辑' };
  if (diff.truncated) return { ok: false, reason: '内容过大已被截断，不可编辑' };
  if (!renderedMerge) {
    return { ok: false, reason: stateText ? `${stateText.replace(/。$/, '')}，不可编辑` : '当前视图不可编辑' };
  }
  return { ok: true, reason: '' };
}
