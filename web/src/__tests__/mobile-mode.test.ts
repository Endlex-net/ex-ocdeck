// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  DEFAULT_CAPS,
  resolveMobileCaps,
  type EffectiveCaps,
  type MobileCaps,
} from '../terminal/mobile-mode';
import {
  clearTermPrefs,
  FONT_FAMILY_KEY,
  FONT_SIZE_KEY,
  loadMobileCaps,
  loadMobileMode,
  MOBILE_CAPS_KEY,
  MOBILE_MODE_KEY,
  saveMobileCaps,
  saveMobileMode,
  TERM_PREFS_CHANGED,
} from '../terminal/preferences';

// localStorage stub：Map 后端 + 按需 spyOn 注入读写失败（沿 App.notification-stream.test.tsx 模式）。
const store = new Map<string, string>();
let savedLocalStorage: Storage | undefined;
let events = 0;
const countEvent = () => {
  events++;
};

function capsOf(lock: boolean, gestures: boolean, keyboardAvoid: boolean): MobileCaps {
  return { version: 1, lock, gestures, keyboardAvoid };
}

const ALL_SUB_SWITCH_COMBOS: MobileCaps[] = [
  capsOf(false, false, false),
  capsOf(false, false, true),
  capsOf(false, true, false),
  capsOf(false, true, true),
  capsOf(true, false, false),
  capsOf(true, false, true),
  capsOf(true, true, false),
  capsOf(true, true, true),
];

beforeEach(() => {
  store.clear();
  events = 0;
  savedLocalStorage = globalThis.localStorage;
  (globalThis as { localStorage: Storage }).localStorage = {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => void store.set(k, String(v)),
    removeItem: (k: string) => void store.delete(k),
    clear: () => store.clear(),
    key: (i: number) => Array.from(store.keys())[i] ?? null,
    get length() {
      return store.size;
    },
  } as Storage;
  window.addEventListener(TERM_PREFS_CHANGED, countEvent);
});

afterEach(() => {
  window.removeEventListener(TERM_PREFS_CHANGED, countEvent);
  vi.restoreAllMocks();
  if (savedLocalStorage === undefined) {
    delete (globalThis as { localStorage?: Storage }).localStorage;
  } else {
    (globalThis as { localStorage: Storage }).localStorage = savedLocalStorage;
  }
});

describe('resolveMobileCaps（启用判定唯一入口，design D2）', () => {
  it('off：无论 coarse 与子开关如何，三项能力全部停用', () => {
    for (const coarse of [false, true]) {
      for (const caps of ALL_SUB_SWITCH_COMBOS) {
        expect(resolveMobileCaps('off', caps, coarse)).toEqual({
          lock: false,
          gestures: false,
          keyboardAvoid: false,
        });
      }
    }
  });

  it('auto：锁定/手势跟随 coarse，避让恒启用且与 pointer 无关；子开关值不参与判定', () => {
    for (const caps of ALL_SUB_SWITCH_COMBOS) {
      expect(resolveMobileCaps('auto', caps, true)).toEqual({
        lock: true,
        gestures: true,
        keyboardAvoid: true,
      });
      expect(resolveMobileCaps('auto', caps, false)).toEqual({
        lock: false,
        gestures: false,
        keyboardAvoid: true,
      });
    }
  });

  it('on：子开关生效（8 组合全表），锁定开强制手势开，结果与 coarse 无关', () => {
    const table: Array<[MobileCaps, EffectiveCaps]> = [
      [capsOf(false, false, false), { lock: false, gestures: false, keyboardAvoid: false }],
      [capsOf(false, false, true), { lock: false, gestures: false, keyboardAvoid: true }],
      [capsOf(false, true, false), { lock: false, gestures: true, keyboardAvoid: false }],
      [capsOf(false, true, true), { lock: false, gestures: true, keyboardAvoid: true }],
      // 锁定开 → 手势强制开（避免只能看不能滚），即使存储的 gestures 为 false
      [capsOf(true, false, false), { lock: true, gestures: true, keyboardAvoid: false }],
      [capsOf(true, false, true), { lock: true, gestures: true, keyboardAvoid: true }],
      [capsOf(true, true, false), { lock: true, gestures: true, keyboardAvoid: false }],
      [capsOf(true, true, true), { lock: true, gestures: true, keyboardAvoid: true }],
    ];
    for (const coarse of [false, true]) {
      for (const [caps, want] of table) {
        expect(resolveMobileCaps('on', caps, coarse)).toEqual(want);
      }
    }
  });
});

describe('loadMobileMode / loadMobileCaps（独立解析与容错，design D1）', () => {
  it('缺省（无存储项）→ mode auto、caps DEFAULT_CAPS', () => {
    expect(loadMobileMode()).toBe('auto');
    expect(loadMobileCaps()).toEqual(DEFAULT_CAPS);
  });

  it('三个合法 mode 值都能原样读出', () => {
    for (const mode of ['auto', 'on', 'off'] as const) {
      store.set(MOBILE_MODE_KEY, mode);
      expect(loadMobileMode()).toBe(mode);
    }
  });

  it('合法 caps JSON 原样读出', () => {
    store.set(MOBILE_CAPS_KEY, JSON.stringify(capsOf(false, true, false)));
    expect(loadMobileCaps()).toEqual(capsOf(false, true, false));
  });

  it('非法 mode 值回退 auto，且不改写 localStorage', () => {
    store.set(MOBILE_MODE_KEY, 'always');
    expect(loadMobileMode()).toBe('auto');
    expect(store.get(MOBILE_MODE_KEY)).toBe('always');
  });

  it('caps JSON 解析失败回退 DEFAULT_CAPS，且不改写 localStorage', () => {
    store.set(MOBILE_CAPS_KEY, '{not-json');
    expect(loadMobileCaps()).toEqual(DEFAULT_CAPS);
    expect(store.get(MOBILE_CAPS_KEY)).toBe('{not-json');
  });

  it('caps 缺字段 / 字段类型错误 / version 未知 各自整项回退且不改写', () => {
    // version 异常用例取非默认字段值，确保 version 校验缺失时断言可区分
    const badValues = [
      JSON.stringify({ version: 1, lock: false, gestures: false }), // 缺 keyboardAvoid
      JSON.stringify({ lock: false, gestures: false, keyboardAvoid: false }), // 缺 version
      JSON.stringify({ version: 2, lock: false, gestures: false, keyboardAvoid: false }), // version 未知
      JSON.stringify({ version: 1, lock: 'yes', gestures: true, keyboardAvoid: true }), // 类型错误
      JSON.stringify({ version: 1, lock: true, gestures: 1, keyboardAvoid: true }),
      JSON.stringify({ version: 1, lock: true, gestures: true, keyboardAvoid: null }),
      JSON.stringify([true, true, true]), // 非对象
      JSON.stringify('on'), // 字符串
      'null',
    ];
    for (const raw of badValues) {
      store.set(MOBILE_CAPS_KEY, raw);
      expect(loadMobileCaps()).toEqual(DEFAULT_CAPS);
      expect(store.get(MOBILE_CAPS_KEY)).toBe(raw);
    }
  });

  it('getItem 抛异常（如 SecurityError）→ 两个 loader 均按默认返回', () => {
    vi.spyOn(localStorage, 'getItem').mockImplementation(() => {
      throw new Error('SecurityError');
    });
    expect(loadMobileMode()).toBe('auto');
    expect(loadMobileCaps()).toEqual(DEFAULT_CAPS);
  });

  it('两个 loader 各自只读自己的 key', () => {
    const getSpy = vi.spyOn(localStorage, 'getItem');
    loadMobileMode();
    expect(getSpy.mock.calls.map((c) => c[0])).toEqual([MOBILE_MODE_KEY]);
    getSpy.mockClear();
    loadMobileCaps();
    expect(getSpy.mock.calls.map((c) => c[0])).toEqual([MOBILE_CAPS_KEY]);
  });
});

describe('写入路径与恢复默认（design D1 写失败三互斥组）', () => {
  it('成功写入：mode 只写 mode key、caps 一次性写完整 JSON，各恰好派发一次', () => {
    saveMobileMode('off');
    saveMobileCaps(capsOf(true, true, false));
    expect(store.get(MOBILE_MODE_KEY)).toBe('off');
    expect(store.get(MOBILE_CAPS_KEY)).toBe(JSON.stringify(capsOf(true, true, false)));
    expect(events).toBe(2);
  });

  it('a. saveMobileMode setItem 失败 → 抛错且不派发', () => {
    vi.spyOn(localStorage, 'setItem').mockImplementation(() => {
      throw new Error('quota exceeded');
    });
    expect(() => saveMobileMode('on')).toThrow('quota exceeded');
    expect(events).toBe(0);
    expect(store.has(MOBILE_MODE_KEY)).toBe(false);
  });

  it('a. saveMobileCaps setItem 失败 → 抛错且不派发', () => {
    vi.spyOn(localStorage, 'setItem').mockImplementation(() => {
      throw new Error('quota exceeded');
    });
    expect(() => saveMobileCaps(DEFAULT_CAPS)).toThrow('quota exceeded');
    expect(events).toBe(0);
    expect(store.has(MOBILE_CAPS_KEY)).toBe(false);
  });

  it('b. 恢复默认四 key 全部删除失败 → 不派发、failedKeys 含全部四项', () => {
    store.set(FONT_FAMILY_KEY, 'x');
    store.set(MOBILE_MODE_KEY, 'on');
    vi.spyOn(localStorage, 'removeItem').mockImplementation(() => {
      throw new Error('denied');
    });
    const { failedKeys } = clearTermPrefs();
    expect(failedKeys).toEqual([FONT_FAMILY_KEY, FONT_SIZE_KEY, MOBILE_MODE_KEY, MOBILE_CAPS_KEY]);
    expect(events).toBe(0);
  });

  it('c. 恢复默认部分成功 → 恰好派发一次、failedKeys 仅含失败项', () => {
    store.set(FONT_FAMILY_KEY, 'mono');
    store.set(FONT_SIZE_KEY, '13');
    store.set(MOBILE_MODE_KEY, 'on');
    store.set(MOBILE_CAPS_KEY, '{}');
    vi.spyOn(localStorage, 'removeItem').mockImplementation((k: string) => {
      if (k === FONT_FAMILY_KEY || k === MOBILE_CAPS_KEY) throw new Error('denied');
      store.delete(k);
    });
    const { failedKeys } = clearTermPrefs();
    expect(failedKeys).toEqual([FONT_FAMILY_KEY, MOBILE_CAPS_KEY]);
    expect(events).toBe(1);
    expect(store.has(FONT_SIZE_KEY)).toBe(false);
    expect(store.has(MOBILE_MODE_KEY)).toBe(false);
    expect(store.get(FONT_FAMILY_KEY)).toBe('mono');
  });

  it('恢复默认全部成功 → failedKeys 空、恰好派发一次', () => {
    store.set(FONT_FAMILY_KEY, 'mono');
    store.set(MOBILE_CAPS_KEY, '{}');
    const { failedKeys } = clearTermPrefs();
    expect(failedKeys).toEqual([]);
    expect(events).toBe(1);
    expect(store.size).toBe(0);
  });
});
