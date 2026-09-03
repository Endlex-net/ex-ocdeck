import { afterEach, describe, expect, it, vi } from 'vitest';
import { rankBranchOptions } from '../pages/command-center-branch-rank';

afterEach(() => {
  vi.restoreAllMocks();
});

describe('rankBranchOptions（D2 过滤后排序元组）', () => {
  it('输入 master 时 origin/master 排第一（同名 + 远端双加分）', () => {
    expect(rankBranchOptions(['main', 'master', 'origin/master'], 'master')).toEqual([
      'origin/master',
      'master',
      'main',
    ]);
  });

  it('同名命中大小写不敏感（本地名与 q 比较）', () => {
    // 'MASTER' 本地名 'master' === q → 同名命中；'origin/MASTER' 额外远端加分故在前
    expect(rankBranchOptions(['MASTER', 'origin/MASTER'], 'master')).toEqual(['origin/MASTER', 'MASTER']);
    // 无 origin/ 前缀时整名参与同名判定
    expect(rankBranchOptions(['Master'], 'master')).toEqual(['Master']);
  });

  it('同键候选用过滤前原顺序稳定 tie-break', () => {
    expect(rankBranchOptions(['feature-b', 'feature-a', 'origin/feature-c'], 'feature')).toEqual([
      'origin/feature-c',
      'feature-b',
      'feature-a',
    ]);
    expect(rankBranchOptions(['origin/y', 'origin/x'], 'zzz')).toEqual(['origin/y', 'origin/x']);
  });

  it('upstream/* 等其它远端名不享受 origin/ 远端加分', () => {
    expect(rankBranchOptions(['upstream/main', 'main'], 'main')).toEqual(['main', 'upstream/main']);
    expect(rankBranchOptions(['upstream/main', 'origin/main'], 'main')).toEqual(['origin/main', 'upstream/main']);
  });

  it('空 query 不过滤不重排，保持原顺序', () => {
    expect(rankBranchOptions(['b', 'a'], '')).toEqual(['b', 'a']);
  });
});
