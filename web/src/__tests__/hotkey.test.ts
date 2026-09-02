import { describe, it, expect } from 'vitest';
import { formatHotkey, matchHotkey, normalizeHotkey, validateCanonicalHotkey } from '../hotkey';

function evt(partial: Partial<KeyboardEvent>): KeyboardEvent {
  return {
    key: '',
    code: '',
    metaKey: false,
    ctrlKey: false,
    altKey: false,
    shiftKey: false,
    ...partial,
  } as KeyboardEvent;
}

/** 与 Go TestHotkeyValidateMatrix 同组命名（wantErr=true 表示 validate 拒绝）。 */
const hotkeyValidateCases: { name: string; hotkey: string; wantErr: boolean }[] = [
  { name: 'mod+k', hotkey: 'mod+k', wantErr: false },
  { name: 'mod+shift+1', hotkey: 'mod+shift+1', wantErr: false },
  { name: 'meta+alt+k', hotkey: 'meta+alt+k', wantErr: false },
  { name: 'ctrl+alt+k', hotkey: 'ctrl+alt+k', wantErr: false },
  { name: 'alt+k', hotkey: 'alt+k', wantErr: false },
  { name: 'no modifier', hotkey: 'k', wantErr: true },
  { name: 'shift only', hotkey: 'shift+k', wantErr: true },
  { name: 'mod+banana', hotkey: 'mod+banana', wantErr: true },
  { name: 'mod++', hotkey: 'mod++', wantErr: true },
  { name: 'mod+K uppercase', hotkey: 'mod+K', wantErr: true },
  { name: 'duplicate modifier', hotkey: 'mod+mod+k', wantErr: true },
  { name: 'mod+meta+k', hotkey: 'mod+meta+k', wantErr: true },
  { name: 'mod+ctrl+k', hotkey: 'mod+ctrl+k', wantErr: true },
  { name: 'wrong order', hotkey: 'shift+mod+k', wantErr: true },
  { name: 'mod+t reserved', hotkey: 'mod+t', wantErr: true },
  { name: 'mod+w reserved', hotkey: 'mod+w', wantErr: true },
  { name: 'mod+n reserved', hotkey: 'mod+n', wantErr: true },
  { name: 'mod+q reserved', hotkey: 'mod+q', wantErr: true },
  { name: 'ctrl+t reserved', hotkey: 'ctrl+t', wantErr: true },
  { name: 'meta+ctrl+t reserved', hotkey: 'meta+ctrl+t', wantErr: true },
  { name: 'meta+shift+t reserved', hotkey: 'meta+shift+t', wantErr: true },
  { name: 'alt+t allowed', hotkey: 'alt+t', wantErr: false },
  { name: 'mod+b sidebar', hotkey: 'mod+b', wantErr: true },
  { name: 'ctrl+b sidebar', hotkey: 'ctrl+b', wantErr: true },
  { name: 'meta+ctrl+b sidebar', hotkey: 'meta+ctrl+b', wantErr: true },
  { name: 'alt+b allowed', hotkey: 'alt+b', wantErr: false },
  { name: 'meta+shift+b allowed', hotkey: 'meta+shift+b', wantErr: false },
];

describe('normalizeHotkey', () => {
  it.each([
    { raw: 'K+Shift+Mod', canonical: 'mod+shift+k' },
    { raw: '  K + Shift + Mod  ', canonical: 'mod+shift+k' },
    { raw: 'mod+K', canonical: 'mod+k' },
    { raw: 'mod+banana', canonical: 'mod+banana' },
    { raw: 'mod+mod+k', canonical: 'mod+mod+k' },
    { raw: 'mod++k', canonical: null },
    { raw: 'mod+', canonical: null },
    { raw: 'k+x', canonical: null },
  ])('normalize $raw', ({ raw, canonical }) => {
    expect(normalizeHotkey(raw)).toBe(canonical);
  });
});

describe('validateCanonicalHotkey', () => {
  it.each(hotkeyValidateCases)('$name', ({ hotkey, wantErr }) => {
    const err = validateCanonicalHotkey(hotkey);
    expect(err !== null).toBe(wantErr);
  });
});

describe('matchHotkey', () => {
  it('expands mod into three masks', () => {
    expect(matchHotkey(evt({ key: 'k', metaKey: true }), 'mod+k')).toBe(true);
    expect(matchHotkey(evt({ key: 'k', ctrlKey: true }), 'mod+k')).toBe(true);
    expect(matchHotkey(evt({ key: 'k', metaKey: true, ctrlKey: true }), 'mod+k')).toBe(true);
    expect(matchHotkey(evt({ key: 'k', metaKey: true, altKey: true }), 'mod+k')).toBe(false);
    expect(matchHotkey(evt({ key: 'k', metaKey: true, shiftKey: true }), 'mod+k')).toBe(false);
  });

  it('digits prefer event.code; Numpad1 does not match 1', () => {
    expect(matchHotkey(evt({ key: '!', code: 'Digit1', metaKey: true, shiftKey: true }), 'mod+shift+1')).toBe(true);
    expect(matchHotkey(evt({ key: '1', code: 'Numpad1', metaKey: true }), 'mod+1')).toBe(false);
    expect(matchHotkey(evt({ key: '1', metaKey: true }), 'mod+1')).toBe(true);
  });

  it('Digit code wins over letter key', () => {
    expect(matchHotkey(evt({ key: 'A', code: 'Digit1', metaKey: true }), 'mod+1')).toBe(true);
    expect(matchHotkey(evt({ key: 'A', code: 'Digit1', metaKey: true }), 'mod+a')).toBe(false);
  });

  it('letters use key.toLowerCase', () => {
    expect(matchHotkey(evt({ key: 'K', metaKey: true }), 'mod+k')).toBe(true);
  });
});

describe('formatHotkey', () => {
  it('maps canonical examples', () => {
    expect(formatHotkey('mod+k')).toBe('⌘K / Ctrl+K');
    expect(formatHotkey('mod+shift+1')).toBe('⌘⇧1 / Ctrl+Shift+1');
    expect(formatHotkey('meta+alt+k')).toBe('⌘⌥K');
    expect(formatHotkey('alt+k')).toBe('Alt+K');
    expect(formatHotkey('ctrl+alt+k')).toBe('Ctrl+Alt+K');
    expect(formatHotkey('mod+alt+shift+k')).toBe('⌘⌥⇧K / Ctrl+Alt+Shift+K');
    expect(formatHotkey('meta+ctrl+k')).toBe('⌘Ctrl+K');
  });
});
