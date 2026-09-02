const MODIFIER_ORDER = ['mod', 'meta', 'ctrl', 'alt', 'shift'] as const;
const MODIFIER_SET = new Set<string>(MODIFIER_ORDER);

export function normalizeHotkey(raw: string): string | null {
  const parts = raw.split('+').map((p) => p.trim().replace(/[A-Z]/g, (ch) => String.fromCharCode(ch.charCodeAt(0) + 32)));
  if (parts.some((p) => p === '')) return null;
  const mods: string[] = [];
  const keys: string[] = [];
  for (const part of parts) {
    if (MODIFIER_SET.has(part)) mods.push(part);
    else keys.push(part);
  }
  if (keys.length !== 1) return null;
  const ordered: string[] = [];
  for (const name of MODIFIER_ORDER) {
    for (const m of mods) {
      if (m === name) ordered.push(m);
    }
  }
  return [...ordered, keys[0]].join('+');
}

export function validateCanonicalHotkey(canonical: string): string | null {
  if (!canonical) return 'hotkey must be a canonical combo';
  const parts = canonical.split('+');
  if (parts.length < 2) return `hotkey ${canonical} must include a modifier`;
  const mods = parts.slice(0, -1);
  const key = parts[parts.length - 1];
  if (!key || key.length !== 1 || !/^[a-z0-9]$/.test(key)) {
    return `hotkey ${canonical} key token must be a single [a-z0-9]`;
  }
  const seen = new Set<string>();
  let hasPrimary = false;
  let prevIdx = -1;
  for (const m of mods) {
    if (!MODIFIER_SET.has(m)) return `hotkey ${canonical} has unknown modifier ${m}`;
    if (seen.has(m)) return `hotkey ${canonical} repeats modifier ${m}`;
    seen.add(m);
    const idx = MODIFIER_ORDER.indexOf(m as (typeof MODIFIER_ORDER)[number]);
    if (idx < prevIdx) return `hotkey ${canonical} modifiers must be in order mod,meta,ctrl,alt,shift`;
    prevIdx = idx;
    if (m === 'mod' || m === 'meta' || m === 'ctrl' || m === 'alt') hasPrimary = true;
  }
  if (!hasPrimary) return `hotkey ${canonical} must include mod|meta|ctrl|alt`;
  if (seen.has('mod') && (seen.has('meta') || seen.has('ctrl'))) {
    return `hotkey ${canonical} must not combine mod with meta/ctrl`;
  }
  if (reservedCombo(seen, key)) return `hotkey ${canonical} is a reserved browser combo`;
  if (sidebarBConflict(seen, key)) return `hotkey ${canonical} conflicts with sidebar ⌘B`;
  return null;
}

function reservedCombo(seen: Set<string>, key: string): boolean {
  if (!'twnq'.includes(key)) return false;
  return seen.has('meta') || seen.has('ctrl') || seen.has('mod');
}

function sidebarBConflict(seen: Set<string>, key: string): boolean {
  if (key !== 'b') return false;
  if (seen.has('alt') || seen.has('shift')) return false;
  return seen.has('meta') || seen.has('ctrl') || seen.has('mod');
}

type ModifierMask = { meta: boolean; ctrl: boolean; alt: boolean; shift: boolean };

function expandMasks(seen: Set<string>): ModifierMask[] {
  const alt = seen.has('alt');
  const shift = seen.has('shift');
  if (seen.has('mod')) {
    return [
      { meta: true, ctrl: false, alt, shift },
      { meta: false, ctrl: true, alt, shift },
      { meta: true, ctrl: true, alt, shift },
    ];
  }
  return [{ meta: seen.has('meta'), ctrl: seen.has('ctrl'), alt, shift }];
}

function eventToken(e: Pick<KeyboardEvent, 'key' | 'code'>): string | null {
  const code = e.code;
  if (typeof code === 'string' && /^Digit[0-9]$/.test(code)) {
    return code.slice(-1);
  }
  const key = e.key;
  if (typeof key === 'string' && key.length === 1 && /[A-Za-z]/.test(key)) {
    return key.toLowerCase();
  }
  if (!code && typeof key === 'string' && key.length === 1 && /[0-9]/.test(key)) {
    return key;
  }
  return null;
}

export function matchHotkey(
  e: Pick<KeyboardEvent, 'key' | 'code' | 'metaKey' | 'ctrlKey' | 'altKey' | 'shiftKey'>,
  canonical: string,
): boolean {
  const parts = canonical.split('+');
  const key = parts[parts.length - 1];
  const seen = new Set(parts.slice(0, -1));
  const token = eventToken(e);
  if (token !== key) return false;
  const actual = { meta: e.metaKey, ctrl: e.ctrlKey, alt: e.altKey, shift: e.shiftKey };
  return expandMasks(seen).some(
    (mask) =>
      mask.meta === actual.meta &&
      mask.ctrl === actual.ctrl &&
      mask.alt === actual.alt &&
      mask.shift === actual.shift,
  );
}

const MAC_MOD: Record<string, string> = {
  mod: '⌘',
  meta: '⌘',
  ctrl: 'Ctrl',
  alt: '⌥',
  shift: '⇧',
};
const TEXT_MOD: Record<string, string> = {
  mod: 'Ctrl',
  meta: '⌘',
  ctrl: 'Ctrl',
  alt: 'Alt',
  shift: 'Shift',
};

export function formatHotkey(canonical: string): string {
  const parts = canonical.split('+');
  const key = parts[parts.length - 1].toUpperCase();
  const mods = parts.slice(0, -1);
  const hasMod = mods.includes('mod');
  const hasMeta = mods.includes('meta');
  if (hasMod) {
    const mac = `${mods.map((m) => MAC_MOD[m]).join('')}${key}`;
    const text = `${mods.map((m) => TEXT_MOD[m]).join('+')}+${key}`;
    return `${mac} / ${text}`;
  }
  if (hasMeta) {
    let out = '';
    let lastIsWord = false;
    for (const m of mods) {
      const token = MAC_MOD[m];
      lastIsWord = token.length > 1;
      out += token;
    }
    return lastIsWord ? `${out}+${key}` : `${out}${key}`;
  }
  return `${mods.map((m) => TEXT_MOD[m]).join('+')}+${key}`;
}
