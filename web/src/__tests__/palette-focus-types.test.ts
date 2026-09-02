import { describe, it, expect } from 'vitest';
import {
  DEFAULT_COMMAND_TRIGGERS,
  PALETTE_CONFIG_CHANGED_EVENT,
  PALETTE_FOCUS_EVENT,
} from '../palette-focus';
import type { PaletteConfig, PaletteFocusDetail, PaletteMatchMode } from '../palette-focus';

describe('palette-focus Lane A types/constants', () => {
  it('exports config-changed event name without wrapping runtime behavior', () => {
    expect(PALETTE_CONFIG_CHANGED_EVENT).toBe('od:palette-config-changed');
    expect(PALETTE_FOCUS_EVENT).toBe('od:palette-focus');
    const mode: PaletteMatchMode = 'exact-then-substring';
    const cfg: PaletteConfig = {
      hotkey: 'mod+k',
      triggerWord: 'new',
      matchMode: mode,
      commandTriggers: DEFAULT_COMMAND_TRIGGERS,
    };
    expect(cfg.matchMode).toBe('exact-then-substring');
  });

  it('PaletteConfig 为四键形状；DEFAULT_COMMAND_TRIGGERS 恰 8 键（cc/pro/reg + 5 空）', () => {
    expect(Object.keys(DEFAULT_COMMAND_TRIGGERS).sort()).toEqual([
      'command-center',
      'projects',
      'register-project',
      'settings-ai',
      'settings-appearance',
      'settings-env',
      'settings-opencode',
      'settings-palette',
    ]);
    expect(DEFAULT_COMMAND_TRIGGERS['command-center']).toBe('cc');
    expect(DEFAULT_COMMAND_TRIGGERS.projects).toBe('pro');
    expect(DEFAULT_COMMAND_TRIGGERS['register-project']).toBe('reg');
    for (const id of ['settings-appearance', 'settings-env', 'settings-opencode', 'settings-ai', 'settings-palette'] as const) {
      expect(DEFAULT_COMMAND_TRIGGERS[id]).toBe('');
    }
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
