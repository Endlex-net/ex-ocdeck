import { describe, it, expect } from 'vitest';
import { parseQuickCreateQuery } from '../components/CommandPalette';
import { foldForMatch } from '../fuzzy-match';

describe('parseQuickCreateQuery', () => {
  it('非触发词前缀返回 null', () => {
    expect(parseQuickCreateQuery('open foo', 'new')).toBeNull();
    expect(parseQuickCreateQuery('newest', 'new')).toBeNull();
  });

  it('字面前缀非正则：点号等元字符按字面匹配', () => {
    expect(parseQuickCreateQuery('n.w foo', 'n.w')).toEqual({ projectQuery: 'foo' });
    expect(parseQuickCreateQuery('naw foo', 'n.w')).toBeNull();
  });

  it('大小写不敏感', () => {
    expect(parseQuickCreateQuery('NEW ocdeck', 'new')).toEqual({ projectQuery: 'ocdeck' });
    expect(parseQuickCreateQuery('New ocdeck', 'NEW')).toEqual({ projectQuery: 'ocdeck' });
  });

  it('triggerWord + 空白即进入模式，空余文 projectQuery 为空串', () => {
    expect(parseQuickCreateQuery('new ', 'new')).toEqual({ projectQuery: '' });
    expect(parseQuickCreateQuery('new\t', 'new')).toEqual({ projectQuery: '' });
    expect(parseQuickCreateQuery('new  \n', 'new')).toEqual({ projectQuery: '' });
  });

  it('仅触发词无尾随空白不进入模式', () => {
    expect(parseQuickCreateQuery('new', 'new')).toBeNull();
  });

  it('空白集合边界：NBSP / U+3000 / FEFF 可作为进入空白', () => {
    expect(parseQuickCreateQuery('new\u00a0foo', 'new')).toEqual({ projectQuery: 'foo' });
    expect(parseQuickCreateQuery('new\u3000foo', 'new')).toEqual({ projectQuery: 'foo' });
    expect(parseQuickCreateQuery('new\ufefffoo', 'new')).toEqual({ projectQuery: 'foo' });
  });

  it('余文整段 trim 保留内部空格', () => {
    expect(parseQuickCreateQuery('new  foo  bar  ', 'new')).toEqual({ projectQuery: 'foo  bar' });
  });

  it('İ fold 长度变化下原始 UTF-16 切片不越界', () => {
    const trigger = 'İ';
    expect(foldForMatch(trigger).length).toBeGreaterThan(trigger.length);
    expect(parseQuickCreateQuery('İ x', trigger)).toEqual({ projectQuery: 'x' });
    expect(parseQuickCreateQuery('İ', trigger)).toBeNull();
  });
});
