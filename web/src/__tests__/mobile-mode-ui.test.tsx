// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act } from 'react';
import { DEFAULT_PALETTE_CONFIG } from '../components/PaletteConfigPanel';
import { SettingsPage } from '../pages/SettingsPage';
import { TerminalView } from '../terminal/TerminalView';
import {
  FONT_FAMILY_KEY,
  FONT_SIZE_KEY,
  MOBILE_CAPS_KEY,
  MOBILE_MODE_KEY,
  TERM_PREFS_CHANGED,
} from '../terminal/preferences';
import { mount, stubMatchMedia } from './cm-test-env';

/* ============================ 移动端模式设置接线（mobile-terminal-mode-settings tasks 6.1-6.3 / 8.1） ============================
 * 真实经过 SettingsPage(appearance) 与 TerminalView 渲染路径；
 * preferences 仅对 loadMobileCaps 做透传 spy（验证 auto/off 判别式不读取 caps key）；
 * session mock 成假 TermSession（本 lane 只测视图接线，session 由并行 lane 负责）。 */

const prefsSpies = vi.hoisted(() => ({
  loadMobileCaps: vi.fn<() => unknown>(),
  actualLoadMobileCaps: undefined as undefined | (() => unknown),
}));

vi.mock('../terminal/preferences', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../terminal/preferences')>();
  prefsSpies.actualLoadMobileCaps = actual.loadMobileCaps;
  return { ...actual, loadMobileCaps: prefsSpies.loadMobileCaps };
});

const sessionMock = vi.hoisted(() => {
  const instances: FakeTermSession[] = [];
  class FakeTermSession {
    static instances = instances;
    connect = vi.fn();
    disconnect = vi.fn();
    dispose = vi.fn();
    applyPreferences = vi.fn();
    lock = vi.fn();
    unlock = vi.fn();
    private locked = false;
    private lockCbs = new Set<(locked: boolean) => void>();
    constructor(
      _host: HTMLElement,
      _wrap: HTMLElement,
      _wsPath: string,
      _onState: (s: string) => void,
    ) {
      instances.push(this);
    }
    isLocked(): boolean {
      return this.locked;
    }
    onLockChange(cb: (locked: boolean) => void): () => boolean {
      this.lockCbs.add(cb);
      return () => this.lockCbs.delete(cb);
    }
  }
  return FakeTermSession;
});

vi.mock('../terminal/session', () => ({ TermSession: sessionMock }));

vi.mock('../api', () => ({
  api: {},
  ApiError: class ApiError extends Error {},
}));

/* ---------- localStorage Map 桩（同 mobile-mode.test.ts 范式） ---------- */
const store = new Map<string, string>();
let savedStorage: Storage | undefined;
let termEvents = 0;
const countTermEvent = () => {
  termEvents += 1;
};

function seedCaps(lock: boolean, gestures: boolean, keyboardAvoid: boolean): string {
  return JSON.stringify({ version: 1, lock, gestures, keyboardAvoid });
}

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

function mobileSeg(container: HTMLElement): HTMLElement {
  return container.querySelector('[aria-labelledby="mobileModeSegLabel"]')!;
}

function pressedOption(container: HTMLElement): string | null {
  return mobileSeg(container).querySelector("button[aria-pressed='true']")?.textContent ?? null;
}

function capInput(container: HTMLElement, id: string): HTMLInputElement {
  return container.querySelector<HTMLInputElement>(`#mobile-cap-${id}`)!;
}

/* ---------- TerminalView 可控 matchMedia 桩（coarse 指针动态切换） ---------- */
let coarseMatches = false;
const coarseListeners = new Set<(e: MediaQueryListEvent) => void>();

function stubControllableMatchMedia() {
  Object.defineProperty(window, 'matchMedia', {
    configurable: true,
    writable: true,
    value: vi.fn((query: string) => ({
      matches: query === '(pointer: coarse)' ? coarseMatches : false,
      media: query,
      onchange: null,
      addEventListener: (_type: string, cb: (e: MediaQueryListEvent) => void) => {
        if (query === '(pointer: coarse)') coarseListeners.add(cb);
      },
      removeEventListener: (_type: string, cb: (e: MediaQueryListEvent) => void) => {
        coarseListeners.delete(cb);
      },
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    })),
  });
}

function setCoarse(matches: boolean) {
  coarseMatches = matches;
  for (const cb of [...coarseListeners]) cb({ matches } as MediaQueryListEvent);
}

function lockButton(container: HTMLElement): HTMLButtonElement | null {
  return container.querySelector<HTMLButtonElement>('.terminal-lock-button');
}

beforeEach(() => {
  store.clear();
  termEvents = 0;
  sessionMock.instances.length = 0;
  prefsSpies.loadMobileCaps.mockReset();
  prefsSpies.loadMobileCaps.mockImplementation(prefsSpies.actualLoadMobileCaps!);
  coarseMatches = false;
  coarseListeners.clear();
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
  window.addEventListener(TERM_PREFS_CHANGED, countTermEvent);
});

afterEach(() => {
  window.removeEventListener(TERM_PREFS_CHANGED, countTermEvent);
  (globalThis as { localStorage: Storage }).localStorage = savedStorage!;
  vi.restoreAllMocks();
});

describe('AppearancePanel 移动端模式控件（task 6.1）', () => {
  it('auto（缺省）渲染分段控件且不渲染子开关、不读取 caps key', () => {
    const getSpy = vi.spyOn(globalThis.localStorage, 'getItem');
    const { container } = mountSettings();

    expect(mobileSeg(container).querySelectorAll('button')).toHaveLength(3);
    expect(pressedOption(container)).toBe('自动');
    expect(capInput(container, 'lock')).toBeNull();
    expect(capInput(container, 'gestures')).toBeNull();
    expect(capInput(container, 'avoid')).toBeNull();
    expect(prefsSpies.loadMobileCaps).not.toHaveBeenCalled();
    expect(getSpy.mock.calls.some((c) => c[0] === MOBILE_CAPS_KEY)).toBe(false);
  });

  it('off 模式不渲染子开关、不读取 caps key（即使 caps 存储存在）', () => {
    store.set(MOBILE_MODE_KEY, 'off');
    store.set(MOBILE_CAPS_KEY, seedCaps(false, true, true));
    const getSpy = vi.spyOn(globalThis.localStorage, 'getItem');
    const { container } = mountSettings();

    expect(pressedOption(container)).toBe('关闭');
    expect(capInput(container, 'lock')).toBeNull();
    expect(capInput(container, 'gestures')).toBeNull();
    expect(capInput(container, 'avoid')).toBeNull();
    expect(prefsSpies.loadMobileCaps).not.toHaveBeenCalled();
    expect(getSpy.mock.calls.some((c) => c[0] === MOBILE_CAPS_KEY)).toBe(false);
  });

  it('on 模式渲染三个子开关；锁定开 → 手势显示开且 disabled，hint 含外接键盘提示', () => {
    store.set(MOBILE_MODE_KEY, 'on');
    store.set(MOBILE_CAPS_KEY, seedCaps(true, true, false));
    const { container } = mountSettings();

    expect(pressedOption(container)).toBe('开启');
    expect(capInput(container, 'lock').checked).toBe(true);
    const gestures = capInput(container, 'gestures');
    expect(gestures.checked).toBe(true);
    expect(gestures.disabled).toBe(true);
    expect(capInput(container, 'avoid').checked).toBe(false);
    expect(container.textContent).toContain('接外接键盘时关闭可避免锁定遮挡输入');
  });

  it('锁定关时手势可编辑；勾选锁定且手势关 → 同一次写入置 gestures:true 并派发一次事件', async () => {
    store.set(MOBILE_MODE_KEY, 'on');
    store.set(MOBILE_CAPS_KEY, seedCaps(false, false, true));
    const { container } = mountSettings();

    const gestures = capInput(container, 'gestures');
    expect(gestures.disabled).toBe(false);
    expect(gestures.checked).toBe(false);

    const setItemSpy = vi.spyOn(globalThis.localStorage, 'setItem');
    await act(async () => {
      capInput(container, 'lock').click();
    });

    expect(setItemSpy).toHaveBeenCalledTimes(1);
    expect(setItemSpy.mock.calls[0][0]).toBe(MOBILE_CAPS_KEY);
    expect(JSON.parse(setItemSpy.mock.calls[0][1])).toEqual({
      version: 1,
      lock: true,
      gestures: true,
      keyboardAvoid: true,
    });
    expect(store.get(MOBILE_CAPS_KEY)).toBe(seedCaps(true, true, true));
    expect(capInput(container, 'gestures').checked).toBe(true);
    expect(capInput(container, 'gestures').disabled).toBe(true);
    expect(termEvents).toBe(1);
  });

  it('mode 写失败 → 显示错误行、分段控件保持旧值、不渲染子开关、不派发事件', async () => {
    const { container } = mountSettings();
    vi.spyOn(globalThis.localStorage, 'setItem').mockImplementation(() => {
      throw new Error('quota exceeded');
    });

    await act(async () => {
      [...mobileSeg(container).querySelectorAll('button')]
        .find((b) => b.textContent === '开启')!
        .click();
    });

    expect(container.querySelector('.error-line')?.textContent).toContain('quota exceeded');
    expect(pressedOption(container)).toBe('自动');
    expect(capInput(container, 'lock')).toBeNull();
    expect(termEvents).toBe(0);
  });

  it('caps 写失败 → 显示错误行、checkbox 保持旧值、存储不变、不派发事件', async () => {
    store.set(MOBILE_MODE_KEY, 'on');
    store.set(MOBILE_CAPS_KEY, seedCaps(false, true, true));
    const { container } = mountSettings();
    vi.spyOn(globalThis.localStorage, 'setItem').mockImplementation(() => {
      throw new Error('quota exceeded');
    });

    await act(async () => {
      capInput(container, 'avoid').click();
    });

    expect(container.querySelector('.error-line')?.textContent).toContain('quota exceeded');
    expect(capInput(container, 'avoid').checked).toBe(true);
    expect(store.get(MOBILE_CAPS_KEY)).toBe(seedCaps(false, true, true));
    expect(termEvents).toBe(0);
  });

  it('恢复默认全成功 → AppearancePanel 收敛为 auto、子开关消失、字体输入清空', async () => {
    store.set(MOBILE_MODE_KEY, 'on');
    store.set(MOBILE_CAPS_KEY, seedCaps(true, true, true));
    store.set(FONT_FAMILY_KEY, 'mono');
    store.set(FONT_SIZE_KEY, '14');
    const { container } = mountSettings();
    expect(capInput(container, 'lock')).not.toBeNull();

    const resetBtn = [...container.querySelectorAll('button')].find(
      (b) => b.textContent === '恢复默认',
    )!;
    await act(async () => {
      resetBtn.click();
    });

    expect(termEvents).toBe(1);
    expect(pressedOption(container)).toBe('自动');
    expect(capInput(container, 'lock')).toBeNull();
    expect(container.querySelector('.error-line')).toBeNull();
    const fontFamilyInput = container.querySelector<HTMLInputElement>('.term-appearance-row input')!;
    expect(fontFamilyInput.value).toBe('');
  });

  it('恢复默认部分失败 → 「部分偏好未清除」、UI 按实际存储收敛、恰好一次事件', async () => {
    store.set(MOBILE_MODE_KEY, 'on');
    store.set(MOBILE_CAPS_KEY, seedCaps(false, true, false));
    store.set(FONT_FAMILY_KEY, 'mono');
    const { container } = mountSettings();
    expect(capInput(container, 'avoid').checked).toBe(false);

    // 仅 mode key 删除失败：字体/caps key 清除成功 → 事件仍应恰好派发一次
    vi.spyOn(globalThis.localStorage, 'removeItem').mockImplementation((k: string) => {
      if (k === MOBILE_MODE_KEY) throw new Error('denied');
      store.delete(k);
    });

    const resetBtn = [...container.querySelectorAll('button')].find(
      (b) => b.textContent === '恢复默认',
    )!;
    await act(async () => {
      resetBtn.click();
    });

    expect(container.textContent).toContain('部分偏好未清除');
    expect(termEvents).toBe(1);
    // mode key 未清除 → 仍为 on；caps key 已清除 → 子开关按默认全开收敛
    expect(pressedOption(container)).toBe('开启');
    expect(capInput(container, 'lock')).not.toBeNull();
    expect(capInput(container, 'lock').checked).toBe(true);
    expect(capInput(container, 'gestures').checked).toBe(true);
    expect(capInput(container, 'avoid').checked).toBe(true);
    // 字体 key 清除成功 → 输入按实际存储清空
    const fontFamilyInput = container.querySelector<HTMLInputElement>('.term-appearance-row input')!;
    expect(fontFamilyInput.value).toBe('');
  });

  it('恢复默认全部失败 → 0 事件、UI 保持原值并提示；恢复删除能力后可重试收敛', async () => {
    store.set(MOBILE_MODE_KEY, 'on');
    store.set(MOBILE_CAPS_KEY, seedCaps(false, true, false));
    store.set(FONT_FAMILY_KEY, 'mono');
    const { container } = mountSettings();

    const removeItemSpy = vi.spyOn(globalThis.localStorage, 'removeItem').mockImplementation(() => {
      throw new Error('denied');
    });
    const resetBtn = () =>
      [...container.querySelectorAll('button')].find((b) => b.textContent === '恢复默认')!;

    await act(async () => {
      resetBtn().click();
    });

    expect(termEvents).toBe(0);
    expect(container.textContent).toContain('部分偏好未清除');
    // mode/caps/字体输入均保持原值
    expect(pressedOption(container)).toBe('开启');
    expect(capInput(container, 'lock').checked).toBe(false);
    expect(capInput(container, 'gestures').checked).toBe(true);
    expect(capInput(container, 'avoid').checked).toBe(false);
    expect(
      container.querySelector<HTMLInputElement>('.term-appearance-row input')!.value,
    ).toBe('mono');
    expect(store.get(MOBILE_CAPS_KEY)).toBe(seedCaps(false, true, false));

    // 恢复删除能力再点一次：可重试并按实际存储收敛
    removeItemSpy.mockRestore();
    await act(async () => {
      resetBtn().click();
    });

    expect(termEvents).toBe(1);
    expect(container.querySelector('.error-line')).toBeNull();
    expect(pressedOption(container)).toBe('自动');
    expect(capInput(container, 'lock')).toBeNull();
    expect(
      container.querySelector<HTMLInputElement>('.term-appearance-row input')!.value,
    ).toBe('');
  });
});

describe('TerminalView 锁定按钮可见性（task 6.3）', () => {
  beforeEach(() => {
    stubControllableMatchMedia();
  });

  it('auto：coarse 指针渲染、fine 指针不渲染', () => {
    const first = mount(<TerminalView wsPath="/ws/terminal/t1" active={false} />);
    expect(lockButton(first.container)).toBeNull();
    first.unmount();

    setCoarse(true);
    const second = mount(<TerminalView wsPath="/ws/terminal/t1" active={false} />);
    expect(lockButton(second.container)).not.toBeNull();
    second.unmount();
  });

  it('off：即使 coarse 指针也不渲染', () => {
    store.set(MOBILE_MODE_KEY, 'off');
    setCoarse(true);
    const { container, unmount } = mount(<TerminalView wsPath="/ws/terminal/t1" active={false} />);
    expect(lockButton(container)).toBeNull();
    unmount();
  });

  it('on：按子开关判定——lock 开在 fine 指针也渲染，lock 关在 coarse 指针也不渲染', () => {
    store.set(MOBILE_MODE_KEY, 'on');
    store.set(MOBILE_CAPS_KEY, seedCaps(true, true, true));
    const first = mount(<TerminalView wsPath="/ws/terminal/t1" active={false} />);
    expect(lockButton(first.container)).not.toBeNull();
    first.unmount();

    store.set(MOBILE_CAPS_KEY, seedCaps(false, true, true));
    setCoarse(true);
    const second = mount(<TerminalView wsPath="/ws/terminal/t1" active={false} />);
    expect(lockButton(second.container)).toBeNull();
    second.unmount();
  });

  it('pointer media query 变化即时刷新（auto）', () => {
    const { container, unmount } = mount(<TerminalView wsPath="/ws/terminal/t1" active={false} />);
    expect(lockButton(container)).toBeNull();

    act(() => setCoarse(true));
    expect(lockButton(container)).not.toBeNull();

    act(() => setCoarse(false));
    expect(lockButton(container)).toBeNull();
    unmount();
  });

  it('TERM_PREFS_CHANGED 后按新 mode/caps 刷新', () => {
    setCoarse(true);
    const { container, unmount } = mount(<TerminalView wsPath="/ws/terminal/t1" active={false} />);
    expect(lockButton(container)).not.toBeNull();

    store.set(MOBILE_MODE_KEY, 'off');
    act(() => {
      window.dispatchEvent(new CustomEvent(TERM_PREFS_CHANGED));
    });
    expect(lockButton(container)).toBeNull();

    store.set(MOBILE_MODE_KEY, 'on');
    store.set(MOBILE_CAPS_KEY, seedCaps(true, true, true));
    act(() => {
      window.dispatchEvent(new CustomEvent(TERM_PREFS_CHANGED));
    });
    expect(lockButton(container)).not.toBeNull();
    unmount();
  });

  it('同源 storage 事件刷新', () => {
    setCoarse(true);
    const { container, unmount } = mount(<TerminalView wsPath="/ws/terminal/t1" active={false} />);
    expect(lockButton(container)).not.toBeNull();

    store.set(MOBILE_MODE_KEY, 'off');
    act(() => {
      window.dispatchEvent(new StorageEvent('storage', { key: MOBILE_MODE_KEY }));
    });
    expect(lockButton(container)).toBeNull();
    unmount();
  });

  it('storage 事件接线到 session.applyPreferences（真实 StorageEvent）', () => {
    const { unmount } = mount(<TerminalView wsPath="/ws/terminal/t1" active={false} />);
    const session = sessionMock.instances[0];
    expect(session).toBeDefined();

    vi.mocked(session.applyPreferences).mockClear();
    act(() => {
      window.dispatchEvent(new StorageEvent('storage', { key: FONT_SIZE_KEY, newValue: '15' }));
    });
    expect(session.applyPreferences).toHaveBeenCalledTimes(1);
    unmount();
  });
});
