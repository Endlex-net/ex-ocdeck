import { describe, it, expect } from 'vitest';
import { acronymOf, classifyMatch, foldForMatch, rankByQuery } from '../fuzzy-match';

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

describe('acronymOf', () => {
  it('splits on -, _, whitespace and camelCase boundaries (before folding)', () => {
    expect(acronymOf('go-ai-agent-app')).toBe('gaaa');
    expect(acronymOf('goAiAgentApp')).toBe('gaaa');
    expect(acronymOf('go_ai_agent_app')).toBe('gaaa');
    expect(acronymOf('go ai agent app')).toBe('gaaa');
  });

  it('consecutive uppercase has no boundary; digit segments take first char', () => {
    expect(acronymOf('HTTPServer')).toBe('h');
    expect(acronymOf('app2')).toBe('a');
    expect(acronymOf('2fa')).toBe('2');
  });

  it('skips empty segments; empty / pure-separator names give empty acronym', () => {
    expect(acronymOf('a--b')).toBe('ab');
    expect(acronymOf('')).toBe('');
    expect(acronymOf('- _ -')).toBe('');
  });

  it('folds segment initials (length may change, e.g. İ)', () => {
    expect(acronymOf('İstanbul')).toBe('i\u0307');
  });

  it('non-BMP segment initial is a full code point, not a lone surrogate', () => {
    // U+10400 (𐐀) 是 surrogate pair；段首必须取完整 code point 再 fold（U+10428 = 𐐨）
    expect(acronymOf('𐐀-alpha')).toBe('𐐨a');
    expect(classifyMatch('𐐀-alpha', '𐐀a')).toEqual({ kind: 'acronym', index: 0 });
    expect(classifyMatch('𐐀-alpha', '𐐨a')).toEqual({ kind: 'acronym', index: 0 });
    // 孤立 surrogate 不是合法 acronym：修复前会以 '\uD801' + 'a' 误命中
    expect(classifyMatch('𐐀-alpha', '\uD801a')).toBeNull();
  });

  it('non-ASCII uppercase has no camelCase boundary; İ followed by ASCII A does', () => {
    // goİstanbul：İ 非 ASCII A-Z，o→İ 不构成边界 → 单段取首字符 g
    expect(acronymOf('goİstanbul')).toBe('g');
    // İA：前一字符 İ 非 ASCII 大写、当前 A 是 ASCII 大写 → 有边界，拆两段
    expect(acronymOf('İA')).toBe('i\u0307a');
  });
});

describe('acronymOf separators', () => {
  it.each([
    ['U+0009 TAB', 0x0009],
    ['U+000A LF', 0x000a],
    ['U+000B VT', 0x000b],
    ['U+000C FF', 0x000c],
    ['U+000D CR', 0x000d],
    ['U+0020 SPACE', 0x0020],
    ['U+00A0 NBSP', 0x00a0],
    ['U+1680 OGHAM SPACE MARK', 0x1680],
    ['U+2000 EN QUAD', 0x2000],
    ['U+2001 EM QUAD', 0x2001],
    ['U+2002 EN SPACE', 0x2002],
    ['U+2003 EM SPACE', 0x2003],
    ['U+2004 THREE-PER-EM SPACE', 0x2004],
    ['U+2005 FOUR-PER-EM SPACE', 0x2005],
    ['U+2006 SIX-PER-EM SPACE', 0x2006],
    ['U+2007 FIGURE SPACE', 0x2007],
    ['U+2008 PUNCTUATION SPACE', 0x2008],
    ['U+2009 THIN SPACE', 0x2009],
    ['U+200A HAIR SPACE', 0x200a],
    ['U+2028 LINE SEPARATOR', 0x2028],
    ['U+2029 PARAGRAPH SEPARATOR', 0x2029],
    ['U+202F NARROW NBSP', 0x202f],
    ['U+205F MEDIUM MATH SPACE', 0x205f],
    ['U+3000 IDEOGRAPHIC SPACE', 0x3000],
    ['U+FEFF BOM', 0xfeff],
  ])('treats %s as a segment separator', (_, code) => {
    expect(acronymOf(`a${String.fromCodePoint(code)}b`)).toBe('ab');
  });

  it('U+0085 is not in the ECMAScript whitespace set (no split)', () => {
    expect(acronymOf('a\u0085b')).toBe('a');
  });
});

describe('classifyMatch acronym tier', () => {
  it('gaaa/ga hit as kind acronym index 0; aa and zzz miss', () => {
    expect(classifyMatch('go-ai-agent-app', 'gaaa')).toEqual({ kind: 'acronym', index: 0 });
    expect(classifyMatch('go-ai-agent-app', 'ga')).toEqual({ kind: 'acronym', index: 0 });
    expect(classifyMatch('go-ai-agent-app', 'aa')).toBeNull();
    expect(classifyMatch('go-ai-agent-app', 'zzz')).toBeNull();
  });

  it('exact/prefix take precedence; double hit (substring + acronym) counts as substring', () => {
    expect(classifyMatch('ga', 'ga')).toEqual({ kind: 'exact', index: 0 });
    expect(classifyMatch('go-ai', 'g')).toEqual({ kind: 'prefix', index: 0 });
    // 前导分隔符让 'g' 同时是 substring(-ga-b 的 index 1) 与 acronym(gab) 命中 → 取 substring
    expect(classifyMatch('-ga-b', 'g')).toEqual({ kind: 'substring', index: 1 });
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

  it('ranks exact > prefix > substring > acronym', () => {
    const items = [
      { name: 'xgaaa', id: 'sub' },
      { name: 'go-ai-agent-app', id: 'acro' },
      { name: 'gaaaX', id: 'pre' },
      { name: 'gaaa', id: 'exact' },
    ];
    expect(rankByQuery(items, 'gaaa').map((i) => i.id)).toEqual(['exact', 'pre', 'sub', 'acro']);
  });

  it('acronym tier has no position tie-break: falls to name order regardless of input order', () => {
    // 前导分隔符确保 'ga' 只构成 acronym 命中（无子串命中），档内按名称确定序
    const long = { name: '-go-ab', id: 'long' }; // acronym 'gab'
    const short = { name: '-go-a', id: 'short' }; // acronym 'ga'
    expect(rankByQuery([long, short], 'ga').map((i) => i.id)).toEqual(['short', 'long']);
    expect(rankByQuery([short, long], 'ga').map((i) => i.id)).toEqual(['short', 'long']);
  });

  it('acronym tier input-order fallback for identical folded names', () => {
    const a = { name: '-go-a', id: '1' };
    const b = { name: '-Go-A', id: '2' };
    expect(rankByQuery([a, b], 'ga').map((i) => i.id)).toEqual(['1', '2']);
    expect(rankByQuery([b, a], 'ga').map((i) => i.id)).toEqual(['2', '1']);
  });

  it('acronym hits are kept in nonempty query; empty query unchanged; input not mutated', () => {
    const items = [{ name: 'go-ai-agent-app' }, { name: 'zzz' }];
    const snapshot = [...items];
    expect(rankByQuery(items, 'gaaa').map((i) => i.name)).toEqual(['go-ai-agent-app']);
    expect(items).toEqual(snapshot);
    expect(rankByQuery(items, '').map((i) => i.name)).toEqual(['go-ai-agent-app', 'zzz']);
  });
});
