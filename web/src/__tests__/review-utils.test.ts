// @vitest-environment jsdom
import { describe, it, expect } from 'vitest';
import {
  buildSnapshot,
  editGateFor,
  filterByTriple,
  lineCountOf,
  sortAnnotations,
} from '../components/diff/review-utils';
import type { Annotation, GitDiffResult } from '../types';

/* ============================ review-utils 纯函数（diff-review-workbench D4/D8，tasks 5.6） ============================ */

function makeAnn(over: Partial<Annotation>): Annotation {
  return {
    id: 'a1',
    path: 'f.txt',
    side: 'new',
    ref: '',
    untracked: false,
    startLine: 1,
    endLine: 1,
    snapshotStartLine: 1,
    snapshotLineCount: 1,
    snapshot: '',
    comment: 'c',
    revision: 1,
    stale: false,
    createdAt: 1,
    updatedAt: 1,
    ...over,
  };
}

describe('buildSnapshot（快照构造自原始侧内容，保留行尾字符）', () => {
  it('选中段前后各 3 行窗口', () => {
    const content = '1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n';
    const w = buildSnapshot(content, 5, 6);
    expect(w.snapshotStartLine).toBe(2);
    expect(w.snapshotLineCount).toBe(8); // L2..L9
    expect(w.snapshot).toBe('2\n3\n4\n5\n6\n7\n8\n9');
  });

  it('文件边界裁短：起点靠前/结尾靠后', () => {
    const content = 'a\nb\nc\n';
    const w0 = buildSnapshot(content, 1, 1);
    expect(w0.snapshotStartLine).toBe(1);
    expect(w0.snapshot).toBe('a\nb\nc\n'); // L1..L4（末行为空行）
    expect(w0.snapshotLineCount).toBe(4);
  });

  it('CRLF 内容快照保留 \\r（禁止取自 CM state.doc）', () => {
    const content = '1\r\n2\r\n3\r\n4\r\n5\r\n6\r\n7\r\n8\r\n9\r\n10\r\n';
    const w = buildSnapshot(content, 5, 5); // 窗口 L2..L8
    expect(w.snapshotStartLine).toBe(2);
    expect(w.snapshotLineCount).toBe(7);
    expect(w.snapshot).toBe('2\r\n3\r\n4\r\n5\r\n6\r\n7\r\n8\r');
  });

  it('行号越界收敛到文件范围', () => {
    const w = buildSnapshot('only\n', 10, 20);
    expect(w.snapshotStartLine).toBe(1);
    expect(w.snapshot).toBe('only\n');
  });

  it('空内容按单行处理', () => {
    expect(lineCountOf('')).toBe(1);
    const w = buildSnapshot('', 1, 1);
    expect(w.snapshot).toBe('');
    expect(w.snapshotLineCount).toBe(1);
  });
});

describe('sortAnnotations（createdAt 升序，平局 id 字典序）', () => {
  it('按 (createdAt, id) 稳定排序', () => {
    const list = [
      makeAnn({ id: 'b', createdAt: 2 }),
      makeAnn({ id: 'z', createdAt: 1 }),
      makeAnn({ id: 'a', createdAt: 2 }),
    ];
    expect(sortAnnotations(list).map((a) => a.id)).toEqual(['z', 'a', 'b']);
  });
});

describe('filterByTriple（视图三元组隔离）', () => {
  it('同路径 staged/unstaged/untracked 互不串扰', () => {
    const staged = makeAnn({ id: 's', ref: 'HEAD' });
    const unstaged = makeAnn({ id: 'u', ref: '' });
    const untracked = makeAnn({ id: 't', ref: '', untracked: true });
    const other = makeAnn({ id: 'o', path: 'other.txt' });
    const all = [staged, unstaged, untracked, other];
    expect(filterByTriple(all, { path: 'f.txt', ref: 'HEAD', untracked: false })).toEqual([staged]);
    expect(filterByTriple(all, { path: 'f.txt', ref: '', untracked: false })).toEqual([unstaged]);
    expect(filterByTriple(all, { path: 'f.txt', ref: '', untracked: true })).toEqual([untracked]);
  });
});

describe('editGateFor（编辑入口门禁，tasks 5.3）', () => {
  const base: GitDiffResult = {
    oldContent: 'a\n',
    newContent: 'b\n',
    oldExists: true,
    newExists: true,
    oldMode: '100644',
    newMode: '100644',
    isBinary: false,
    truncated: false,
  };

  const cases: Array<{ name: string; diff: GitDiffResult; merge: boolean; reason: string }> = [
    { name: 'binary', diff: { ...base, isBinary: true }, merge: false, reason: '二进制' },
    {
      name: 'gitlink',
      diff: { ...base, oldMode: '160000', newMode: '160000' },
      merge: false,
      reason: 'gitlink',
    },
    {
      name: '新侧缺失',
      diff: { ...base, newExists: false, newContent: '', newMode: '' },
      merge: true,
      reason: '已删除',
    },
    {
      name: 'symlink',
      diff: { ...base, newMode: '120000' },
      merge: true,
      reason: '符号链接',
    },
    { name: 'truncated', diff: { ...base, truncated: true }, merge: true, reason: '截断' },
    { name: '未渲染 merge 视图', diff: base, merge: false, reason: '不可编辑' },
  ];

  for (const c of cases) {
    it(`${c.name} → 拒绝并给出原因`, () => {
      const g = editGateFor(c.diff, c.merge, null);
      expect(g.ok).toBe(false);
      expect(g.reason).toContain(c.reason);
    });
  }

  it('全部满足 → 允许进入', () => {
    expect(editGateFor(base, true, null).ok).toBe(true);
  });
});
