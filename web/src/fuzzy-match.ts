export type MatchKind = 'exact' | 'prefix' | 'substring' | 'acronym';
export type MatchResult = { kind: MatchKind; index: number };

export function foldForMatch(s: string): string {
  return String.prototype.toLowerCase.call(s);
}

/** ECMAScript WhiteSpace + LineTerminator（与 CommandPalette / Go isECMAScriptSpace 同源，不得用 /\s/）。 */
const ECMA_SCRIPT_SPACE_CODES = new Set([
  0x0009, 0x000a, 0x000b, 0x000c, 0x000d, 0x0020, 0x00a0, 0x1680, 0x2000, 0x2001, 0x2002, 0x2003,
  0x2004, 0x2005, 0x2006, 0x2007, 0x2008, 0x2009, 0x200a, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000,
  0xfeff,
]);

function isECMAScriptSpaceCode(code: number): boolean {
  return ECMA_SCRIPT_SPACE_CODES.has(code);
}

/** 项目名缩写串（tasks 4.8）：原始名称按 `-`/`_`/空白拆段（跳过空段），段内按 camelCase
 *  边界（前一字符非 ASCII A-Z 且当前字符为 ASCII A-Z）拆子段，每非空子段首字符逐个 fold 拼接。
 *  分段必须在原始名称上进行：先 fold 会消灭 camelCase 边界（goAi → goai）。 */
export function acronymOf(name: string): string {
  let out = '';
  let segStart = -1; // 当前子段起始下标；-1 = 尚未积累
  const endSegment = () => {
    if (segStart >= 0) {
      // 段首按完整 code point 取（非 BMP 字符是 surrogate pair，单 code unit 会产生孤立代理）
      out += foldForMatch(String.fromCodePoint(name.codePointAt(segStart)!));
      segStart = -1;
    }
  };
  for (let i = 0; i < name.length; i++) {
    const code = name.charCodeAt(i);
    if (code === 0x2d /* - */ || code === 0x5f /* _ */ || isECMAScriptSpaceCode(code)) {
      endSegment();
      continue;
    }
    if (segStart >= 0) {
      const prev = name.charCodeAt(i - 1);
      const isUpper = code >= 0x41 && code <= 0x5a;
      const prevIsUpper = prev >= 0x41 && prev <= 0x5a;
      if (isUpper && !prevIsUpper) endSegment(); // camelCase 边界：lower→upper
    }
    if (segStart < 0) segStart = i;
  }
  endSegment();
  return out;
}

export function classifyMatch(name: string, query: string): MatchResult | null {
  if (query === '') return null;
  const foldedName = foldForMatch(name);
  const foldedQuery = foldForMatch(query);
  const index = foldedName.indexOf(foldedQuery);
  if (index >= 0) {
    if (foldedName === foldedQuery) return { kind: 'exact', index: 0 };
    if (foldedName.startsWith(foldedQuery)) return { kind: 'prefix', index: 0 };
    return { kind: 'substring', index };
  }
  // 缩写档位（第四档）：非空 acronym 的前缀命中；index 固定 0，不参与位置比较。
  // exact/prefix/substring 已在上方返回，双命中自然按更高档计。
  const acronym = acronymOf(name);
  if (acronym !== '' && acronym.startsWith(foldedQuery)) return { kind: 'acronym', index: 0 };
  return null;
}

function kindRank(kind: MatchKind): number {
  if (kind === 'exact') return 0;
  if (kind === 'prefix') return 1;
  if (kind === 'substring') return 2;
  return 3;
}

function compareFoldedNames(a: string, b: string): number {
  const fa = foldForMatch(a);
  const fb = foldForMatch(b);
  const n = Math.min(fa.length, fb.length);
  for (let i = 0; i < n; i++) {
    const ca = fa.charCodeAt(i);
    const cb = fb.charCodeAt(i);
    if (ca !== cb) return ca - cb;
  }
  return fa.length - fb.length;
}

export function rankByQuery<T extends { name: string }>(items: readonly T[], query: string): T[] {
  if (query === '') {
    return items
      .map((item, inputIndex) => ({ item, inputIndex }))
      .sort((a, b) => {
        const byName = compareFoldedNames(a.item.name, b.item.name);
        if (byName !== 0) return byName;
        return a.inputIndex - b.inputIndex;
      })
      .map((row) => row.item);
  }

  const ranked: { item: T; inputIndex: number; kind: MatchKind; index: number }[] = [];
  for (let inputIndex = 0; inputIndex < items.length; inputIndex++) {
    const item = items[inputIndex];
    const hit = classifyMatch(item.name, query);
    if (!hit) continue;
    ranked.push({ item, inputIndex, kind: hit.kind, index: hit.index });
  }
  ranked.sort((a, b) => {
    const byKind = kindRank(a.kind) - kindRank(b.kind);
    if (byKind !== 0) return byKind;
    if (a.index !== b.index) return a.index - b.index;
    const byName = compareFoldedNames(a.item.name, b.item.name);
    if (byName !== 0) return byName;
    return a.inputIndex - b.inputIndex;
  });
  return ranked.map((row) => row.item);
}
