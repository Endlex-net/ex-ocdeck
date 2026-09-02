import { describe, it, expect } from 'vitest';
import { PALETTE_CONFIG_CHANGED_EVENT, PALETTE_FOCUS_EVENT } from '../palette-focus';
import type { PaletteConfig, PaletteFocusDetail, PaletteMatchMode } from '../palette-focus';

describe('palette-focus Lane A types/constants', () => {
  it('exports config-changed event name without wrapping runtime behavior', () => {
    expect(PALETTE_CONFIG_CHANGED_EVENT).toBe('od:palette-config-changed');
    expect(PALETTE_FOCUS_EVENT).toBe('od:palette-focus');
    const mode: PaletteMatchMode = 'exact-then-substring';
    const cfg: PaletteConfig = { hotkey: 'mod+k', triggerWord: 'new', matchMode: mode };
    expect(cfg.matchMode).toBe('exact-then-substring');
  });

  it('accepts the three legal PaletteFocusDetail shapes', () => {
    const idOnly: PaletteFocusDetail = { id: 'new-task-name' };
    const withName: PaletteFocusDetail = { id: 'new-task-name', projectName: 'p' };
    const withBoth: PaletteFocusDetail = { id: 'new-task-name', projectName: 'p', projectID: '1' };
    expect(idOnly).toEqual({ id: 'new-task-name' });
    expect(withName.projectName).toBe('p');
    expect(withBoth.projectID).toBe('1');
  });

  it('rejects projectID-only and nested payload at compile time', () => {
    // @ts-expect-error projectID must not appear without projectName
    const illegalID: PaletteFocusDetail = { id: 'new-task-name', projectID: '1' };
    // @ts-expect-error MUST NOT nest {id, payload}
    const illegalNested: PaletteFocusDetail = { id: 'new-task-name', payload: { projectName: 'p' } };
    expect(illegalID).toBeDefined();
    expect(illegalNested).toBeDefined();
  });
});
