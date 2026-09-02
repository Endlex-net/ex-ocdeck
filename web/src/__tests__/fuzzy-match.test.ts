import { describe, it, expect } from 'vitest';
import { classifyMatch, foldForMatch, rankByQuery } from '../fuzzy-match';

type Item = { name: string; id?: string };

describe('foldForMatch', () => {
  it('uses toLowerCase, not locale/normalization', () => {
    expect(foldForMatch('Ä')).toBe('ä');
    expect(foldForMatch('中')).toBe('中');
    expect(foldForMatch('İ').length).toBe(2);
    expect(foldForMatch('İ')).toBe('i\u0307');
  });
});

describe('classifyMatch', () => {
  it('empty query returns null', () => {
    expect(classifyMatch('Alpha', '')).toBeNull();
  });

  it('exact/prefix index is 0; substring is first hit', () => {
    expect(classifyMatch('Alpha', 'alpha')).toEqual({ kind: 'exact', index: 0 });
    expect(classifyMatch('Alphabet', 'alpha')).toEqual({ kind: 'prefix', index: 0 });
    expect(classifyMatch('xxAlpha', 'alpha')).toEqual({ kind: 'substring', index: 2 });
  });

  it('miss returns null', () => {
    expect(classifyMatch('Alpha', 'zzz')).toBeNull();
  });

  it('İ fold length change does not overflow indexOf', () => {
    const folded = foldForMatch('İ');
    expect(folded.length).toBeGreaterThan('İ'.length);
    expect(classifyMatch('İstanbul', 'İ')).toEqual({ kind: 'prefix', index: 0 });
    expect(classifyMatch('x', 'İ')).toBeNull();
    expect(foldForMatch('İx')).toBe('i\u0307x');
    expect(classifyMatch('İx', 'x')).toEqual({ kind: 'substring', index: 2 });
  });
});

describe('rankByQuery', () => {
  const alpha = { name: 'Alpha', id: 'a' };
  const alphabet = { name: 'Alphabet', id: 'b' };
  const xx = { name: 'xxAlpha', id: 'c' };
  const beta = { name: 'Beta', id: 'd' };

  it('does not mutate input and returns a new array (empty query)', () => {
    const items = [beta, alpha];
    const snapshot = [items[0], items[1]];
    const out = rankByQuery(items, '');
    expect(items[0]).toBe(beta);
    expect(items[1]).toBe(alpha);
    expect(items).toEqual(snapshot);
    expect(out).not.toBe(items);
    expect(out[0]).toBe(alpha);
    expect(out[1]).toBe(beta);
  });

  it('does not mutate input and returns a new array (nonempty query)', () => {
    const items = [xx, alphabet, alpha];
    const snapshot = [items[0], items[1], items[2]];
    const out = rankByQuery(items, 'alpha');
    expect(items[0]).toBe(xx);
    expect(items[1]).toBe(alphabet);
    expect(items[2]).toBe(alpha);
    expect(items).toEqual(snapshot);
    expect(out).not.toBe(items);
    expect(out[0]).toBe(alpha);
    expect(out[1]).toBe(alphabet);
    expect(out[2]).toBe(xx);
  });

  it('ranks exact > prefix > substring and excludes misses', () => {
    expect(rankByQuery([xx, alphabet, alpha, beta], 'alpha').map((i) => i.name)).toEqual([
      'Alpha',
      'Alphabet',
      'xxAlpha',
    ]);
  });

  it('empty query returns all items in name order', () => {
    const items = [beta, alpha];
    expect(rankByQuery(items, '').map((i) => i.name)).toEqual(['Alpha', 'Beta']);
  });

  it('same comparator for rank, empty query, and zero-hit fallback (order and reverse)', () => {
    const afoo = { name: 'Afoo' };
    const abar = { name: 'aBar' };
    const forward = [afoo, abar];
    const reverse = [abar, afoo];
    const names = (items: Item[]) => items.map((i) => i.name);
    const fallback = (items: Item[], query: string) => {
      const ranked = rankByQuery(items, query);
      return ranked.length === 0 ? rankByQuery(items, '') : ranked;
    };
    expect(names(rankByQuery(forward, 'a'))).toEqual(['aBar', 'Afoo']);
    expect(names(rankByQuery(reverse, 'a'))).toEqual(['aBar', 'Afoo']);
    expect(names(rankByQuery(forward, ''))).toEqual(['aBar', 'Afoo']);
    expect(names(rankByQuery(reverse, ''))).toEqual(['aBar', 'Afoo']);
    expect(rankByQuery(forward, 'zzzz')).toEqual([]);
    expect(names(fallback(forward, 'zzzz'))).toEqual(['aBar', 'Afoo']);
    expect(names(fallback(reverse, 'zzzz'))).toEqual(['aBar', 'Afoo']);
  });

  it('non-ASCII UTF-16 order ä < 中 < 😀', () => {
    const items: Item[] = [{ name: '😀' }, { name: '中' }, { name: 'ä' }];
    expect(rankByQuery(items, '').map((i) => i.name)).toEqual(['ä', '中', '😀']);
  });

  it('prefix length: foo before foobar regardless of input order', () => {
    const foo = { name: 'foo' };
    const foobar = { name: 'foobar' };
    expect(rankByQuery([foobar, foo], '').map((i) => i.name)).toEqual(['foo', 'foobar']);
    expect(rankByQuery([foo, foobar], '').map((i) => i.name)).toEqual(['foo', 'foobar']);
  });

  it('Afoo vs aBar uses fold result', () => {
    const afoo = { name: 'Afoo' };
    const abar = { name: 'aBar' };
    expect(rankByQuery([afoo, abar], '').map((i) => i.name)).toEqual(['aBar', 'Afoo']);
    expect(rankByQuery([abar, afoo], '').map((i) => i.name)).toEqual(['aBar', 'Afoo']);
  });

  it('same folded name keeps input order', () => {
    const a = { name: 'Alpha', id: '1' };
    const b = { name: 'alpha', id: '2' };
    expect(rankByQuery([b, a], '').map((i) => i.id)).toEqual(['2', '1']);
  });

  it('ranking ignores matchMode because rankByQuery has no matchMode argument', () => {
    const items = [xx, alphabet, alpha];
    expect(rankByQuery(items, 'alpha').map((i) => i.name)).toEqual(['Alpha', 'Alphabet', 'xxAlpha']);
  });
});
