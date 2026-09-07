// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act } from 'react';
import { DEFAULT_PALETTE_CONFIG } from '../components/PaletteConfigPanel';
import { SettingsPage } from '../pages/SettingsPage';
import { CLIPBOARD_POLICY_KEY } from '../terminal/clipboard';
import { TERM_PREFS_CHANGED } from '../terminal/preferences';
import { mount, stubMatchMedia } from './cm-test-env';

/* ============================ 远程剪贴板策略设置控件（SettingsPage appearance） ============================
 * 持久 auto 授权是源级、跨任务的，必须在设置里可撤销（改回询问/关闭）。
 * 真实经过 SettingsPage(appearance) 渲染路径；api mock；沿用 mobile-mode-ui.test 范式。 */

vi.mock('../api', () => ({
  api: {},
  ApiError: class ApiError extends Error {},
}));

const store = new Map<string, string>();
let savedStorage: Storage | undefined;

function mountSettings() {
  return mount(
    <SettingsPage
      tab="appearance"
      paletteConfig={DEFAULT_PALETTE_CONFIG}
      paletteLoadState="ready"
      paletteLoadError=""
    />,
  );
}

function clipSeg(container: HTMLElement): HTMLElement {
  return container.querySelector('[aria-labelledby="clipPolicySegLabel"]')!;
}

function pressedOption(container: HTMLElement): string | null {
  return clipSeg(container).querySelector("button[aria-pressed='true']")?.textContent ?? null;
}

function clickOption(container: HTMLElement, label: string) {
  const btn = [...clipSeg(container).querySelectorAll<HTMLButtonElement>('button')].find(
    (b) => b.textContent === label,
  )!;
  return act(async () => {
    btn.click();
  });
}

beforeEach(() => {
  store.clear();
  savedStorage = globalThis.localStorage;
  (globalThis as { localStorage: Storage }).localStorage = {
    getItem: (k: string) => (store.has(k) ? store.get(k)! : null),
    setItem: (k: string, v: string) => {
      store.set(k, String(v));
    },
    removeItem: (k: string) => {
      store.delete(k);
    },
    clear: () => {
      store.clear();
    },
    key: (i: number) => Array.from(store.keys())[i] ?? null,
    get length() {
      return store.size;
    },
  } as Storage;
  stubMatchMedia(false);
});

afterEach(() => {
  (globalThis as { localStorage: Storage }).localStorage = savedStorage!;
  vi.restoreAllMocks();
});

describe('远程剪贴板策略控件', () => {
  it('默认询问；渲染三档并说明作用范围（本设备所有终端）', () => {
    const { container, unmount } = mountSettings();
    expect(clipSeg(container).querySelectorAll('button')).toHaveLength(3);
    expect(pressedOption(container)).toBe('询问');
    expect(container.textContent).toContain('对本设备的所有终端生效');
    unmount();
  });

  it('点「关闭」→ 持久化 off、选中态收敛、恰好派发一次变更事件', async () => {
    const { container, unmount } = mountSettings();
    let events = 0;
    const count = () => {
      events += 1;
    };
    window.addEventListener(TERM_PREFS_CHANGED, count);
    try {
      await clickOption(container, '关闭');
      expect(store.get(CLIPBOARD_POLICY_KEY)).toBe('off');
      expect(pressedOption(container)).toBe('关闭');
      expect(events).toBe(1);
    } finally {
      window.removeEventListener(TERM_PREFS_CHANGED, count);
      unmount();
    }
  });

  it('auto 授权可撤销：自动允许 → 询问，存储回到 ask', async () => {
    store.set(CLIPBOARD_POLICY_KEY, 'auto');
    const { container, unmount } = mountSettings();
    expect(pressedOption(container)).toBe('自动允许');
    await clickOption(container, '询问');
    expect(store.get(CLIPBOARD_POLICY_KEY)).toBe('ask');
    expect(pressedOption(container)).toBe('询问');
    unmount();
  });
});
