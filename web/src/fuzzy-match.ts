export type MatchKind = 'exact' | 'prefix' | 'substring';
export type MatchResult = { kind: MatchKind; index: number };

export function foldForMatch(s: string): string {
  return String.prototype.toLowerCase.call(s);
}

export function classifyMatch(name: string, query: string): MatchResult | null {
  if (query === '') return null;
  const foldedName = foldForMatch(name);
  const foldedQuery = foldForMatch(query);
  const index = foldedName.indexOf(foldedQuery);
  if (index < 0) return null;
  if (foldedName === foldedQuery) return { kind: 'exact', index: 0 };
  if (foldedName.startsWith(foldedQuery)) return { kind: 'prefix', index: 0 };
  return { kind: 'substring', index };
}

function kindRank(kind: MatchKind): number {
  if (kind === 'exact') return 0;
  if (kind === 'prefix') return 1;
  return 2;
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
