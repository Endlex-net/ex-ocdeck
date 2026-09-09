import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { existsSync, readFileSync } from 'node:fs';

/**
 * D3 字体加载生命周期（terminal-links-emoji-icons 4.3/4.4，design D3 N5）：
 * - 终端打开后 `document.fonts.load('13px "Symbols Nerd Font Mono"', '\u{E0A0}')` 全局单次尝试；
 * - 成功 settle → 所有存活实例 clearTextureAtlas() + refresh(0, rows-1)（已销毁实例跳过）；
 * - 失败 → console.warn 降级，无 refresh，不重建终端（term.dispose 不被调）、不重连、
 *   不改偏好（localStorage 零写入）、不动锁定状态；
 * - 无 fonts API 环境跳过不崩溃；阻塞终/open 不成立（open 先于 load，load 不阻塞）。
 *
 * 策略：session-adapter 式 mock 全家桶 + vi.resetModules()（session.ts 的
 * aliveSessions/iconFontLoadAttempted 为模块级状态，须逐测试隔离）。
 */

// ---------- mocks ----------
const termInstances: FakeTerm[] = [];

class FakeTerm {
  loadAddon = vi.fn();
  open = vi.fn();
  onData = vi.fn();
  onBinary = vi.fn();
  write = vi.fn();
  dispose = vi.fn();
  blur = vi.fn();
  focus = vi.fn();
  input = vi.fn();
  refresh = vi.fn();
  cols = 80;
  rows = 24;
  element = null;
  textarea = { addEventListener: vi.fn(), removeEventListener: vi.fn() } as unknown as HTMLTextAreaElement;
  options: Record<string, unknown> = {};
  modes = { mouseTrackingMode: 'none' as const };
  buffer = { active: { type: 'normal' as const } };
  parser = { registerOscHandler: vi.fn(() => ({ dispose: vi.fn() })) };
  attachCustomKeyEventHandler = vi.fn();
  constructor() {
    termInstances.push(this);
  }
}
vi.mock('@xterm/xterm', () => ({ Terminal: vi.fn(() => new FakeTerm()) }));
vi.mock('@xterm/addon-fit', () => ({ FitAddon: vi.fn(() => ({ fit: vi.fn() })) }));

// webgl mock：可配置构造失败（「加载失败仍 refresh」用例，ora-15 非阻断项）
const harness = vi.hoisted(() => ({ webglThrowOnConstruct: false }));
const atlasClearSpy = vi.fn();
vi.mock('@xterm/addon-webgl', () => ({
  WebglAddon: vi.fn(function WebglAddonMock(this: unknown) {
    if (harness.webglThrowOnConstruct) throw new Error('WebGL unavailable');
    return { dispose: vi.fn(), clearTextureAtlas: atlasClearSpy };
  }),
}));
vi.mock('@xterm/addon-web-links', () => ({
  WebLinksAddon: vi.fn(() => ({ activate: vi.fn(), dispose: vi.fn() })),
}));
vi.mock('@xterm/addon-unicode-graphemes', () => ({
  UnicodeGraphemesAddon: vi.fn(() => ({ activate: vi.fn(), dispose: vi.fn() })),
}));

const lockControllerMock = {
  lock: vi.fn(),
  unlock: vi.fn(),
  isLocked: vi.fn(() => false),
  onChange: vi.fn(() => () => {}),
  dispose: vi.fn(),
  overlay: {} as HTMLElement,
};
vi.mock('../terminal/lock', () => ({ createLockController: vi.fn(() => lockControllerMock) }));
vi.mock('../terminal/touch-gestures', () => ({
  attachTouchGestures: vi.fn(() => ({ rebind: vi.fn(), dispose: vi.fn() })),
}));
vi.mock('../terminal/session-coordination', () => ({
  createLockOrchestrator: vi.fn(() => ({
    onAuthOk: vi.fn((_lockEnabled: boolean, onAuthed: () => void) => onAuthed()),
    onPointerChange: vi.fn(),
    lock: vi.fn(),
    unlock: vi.fn(),
    dispose: vi.fn(),
  })),
}));
vi.mock('../terminal/preferences', () => ({
  loadTermPrefs: vi.fn(() => ({})),
  resolveFontFamily: vi.fn(() => 'monospace'),
  resolveFontSize: vi.fn(() => 13),
  loadMobileMode: vi.fn(() => 'auto'),
  loadMobileCaps: vi.fn(() => ({ version: 1, lock: true, gestures: true, keyboardAvoid: true })),
  TERM_PREFS_CHANGED: 'ocdeck-term-prefs-changed',
}));
vi.mock('../api', () => ({
  clearToken: vi.fn(),
  getToken: vi.fn(() => 'fake-token'),
  wsURL: vi.fn(() => 'ws://fake/terminal'),
  UNAUTHORIZED_EVENT: 'ocdeck:unauthorized',
}));

// ---------- document.fonts 可控桩（deferred） ----------
type Deferred = { promise: Promise<unknown>; resolve: () => void; reject: (e: Error) => void };
let fontLoadDeferred: Deferred | null = null;
let fontLoadCalls: { font: string; sample: string }[] = [];

function stubDocumentFonts(withFonts: boolean): void {
  fontLoadCalls = [];
  fontLoadDeferred = null;
  const fonts = withFonts
    ? {
        load: vi.fn((font: string, sample: string) => {
          fontLoadCalls.push({ font, sample });
          return new Promise((resolve, reject) => {
            fontLoadDeferred = {
              promise: new Promise(() => {}), // 占位：真实 settle 由测试驱动
              resolve: () => resolve([]),
              reject: (e: Error) => reject(e),
            };
          });
        }),
      }
    : undefined;
  (globalThis.document as { fonts?: unknown }).fonts = fonts;
}

function settleFontLoadOk(): void {
  fontLoadDeferred?.resolve();
}
function settleFontLoadFail(): void {
  fontLoadDeferred?.reject(new Error('font fetch failed'));
}

// ---------- 全局 stubs ----------
let windowDefined = false;
let savedMatchMedia: ((q: string) => MediaQueryList) | undefined;
let savedResizeObserver: typeof ResizeObserver | undefined;
let savedLocalStorage: Storage;
let savedDocument: Document | undefined;

beforeEach(async () => {
  vi.clearAllMocks();
  atlasClearSpy.mockClear();
  vi.resetModules(); // session.ts 模块级 aliveSessions/iconFontLoadAttempted 逐测试隔离
  termInstances.length = 0;
  harness.webglThrowOnConstruct = false;
  // resetModules 后需重新挂置 localStorage stub（其他 stub 为全局对象引用，不随模块缓存失效）
  termInstances[0]?.textarea;

  if (typeof globalThis.window === 'undefined') {
    windowDefined = true;
    (globalThis as { window: typeof globalThis }).window = globalThis as unknown as typeof globalThis & Window;
  }
  savedMatchMedia = globalThis.matchMedia;
  (globalThis as { matchMedia: (q: string) => MediaQueryList }).matchMedia = (q: string) =>
    ({
      matches: false,
      media: q,
      onchange: null,
      addListener: () => {},
      removeListener: () => {},
      addEventListener: () => {},
      removeEventListener: () => {},
      dispatchEvent: () => false,
    }) as MediaQueryList;
  (globalThis.window as { matchMedia: unknown }).matchMedia = globalThis.matchMedia;

  savedResizeObserver = globalThis.ResizeObserver;
  function FakeResizeObserver(this: unknown) {
    return { observe: () => {}, unobserve: () => {}, disconnect: () => {} };
  }
  (globalThis as { ResizeObserver: typeof ResizeObserver }).ResizeObserver =
    FakeResizeObserver as unknown as typeof ResizeObserver;

  savedLocalStorage = globalThis.localStorage;
  const storageSetItem = vi.fn();
  (globalThis as { localStorage: Storage }).localStorage = {
    getItem: () => null,
    setItem: storageSetItem,
    removeItem: vi.fn(),
    clear: () => {},
    key: () => null,
    length: 0,
  } as unknown as Storage;
  (globalThis as { __storageSetItem?: unknown }).__storageSetItem = storageSetItem;

  savedDocument = (globalThis as { document?: Document }).document;
  (globalThis as { document: Document }).document = { activeElement: null } as unknown as Document;
  stubDocumentFonts(true);
});

afterEach(() => {
  (globalThis as { matchMedia: unknown }).matchMedia = savedMatchMedia;
  if (savedResizeObserver !== undefined) {
    (globalThis as { ResizeObserver: typeof ResizeObserver }).ResizeObserver = savedResizeObserver;
  }
  (globalThis as { localStorage: Storage }).localStorage = savedLocalStorage;
  if (savedDocument === undefined) delete (globalThis as { document?: Document }).document;
  else (globalThis as { document: Document }).document = savedDocument;
  delete (globalThis as { __storageSetItem?: unknown }).__storageSetItem;
  if (windowDefined) {
    delete (globalThis as { window?: typeof globalThis }).window;
    windowDefined = false;
  }
});

function fakeWrap(): HTMLElement {
  return {
    style: { maxHeight: '' },
    getBoundingClientRect: () =>
      ({ top: 0, bottom: 0, left: 0, right: 0, width: 0, height: 0, x: 0, y: 0, toJSON: () => ({}) }) as unknown as DOMRect,
  } as unknown as HTMLElement;
}

async function makeSession(): Promise<import('../terminal/session').TermSession> {
  const { TermSession } = await import('../terminal/session');
  return new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
}

function storageSetItemSpy(): ReturnType<typeof vi.fn> {
  return (globalThis as { __storageSetItem?: unknown }).__storageSetItem as ReturnType<typeof vi.fn>;
}

describe('D3 字体加载生命周期（design D3 N5）', () => {
  it('终端打开后 fonts.load 单次触发、参数精确（字体 + U+E0A0 采样）；第二实例不重复触发', async () => {
    const s1 = await makeSession();
    expect(fontLoadCalls).toEqual([{ font: '13px "Symbols Nerd Font Mono"', sample: '\u{E0A0}' }]);
    const s2 = await makeSession();
    expect(fontLoadCalls).toHaveLength(1); // 全局单次尝试（iconFontLoadAttempted）
    s1.dispose();
    s2.dispose();
  });

  it('加载成功：全部存活实例 clearTextureAtlas + refresh(0, rows-1)；已销毁实例跳过', async () => {
    const s1 = await makeSession();
    const s2 = await makeSession();
    s2.dispose(); // settle 前销毁
    const s2Term = termInstances[1];
    settleFontLoadOk();
    await vi.waitFor(() => {
      expect(termInstances[0].refresh).toHaveBeenCalledWith(0, 23);
    });
    expect(atlasClearSpy).toHaveBeenCalledTimes(1); // 仅存活实例（webgl atlas）
    expect(termInstances[0].refresh).toHaveBeenCalledTimes(1);
    expect(s2Term.refresh).not.toHaveBeenCalled(); // 已销毁实例跳过
    // 不重建终端、不改偏好
    expect(termInstances[0].dispose).not.toHaveBeenCalled();
    expect(storageSetItemSpy()).not.toHaveBeenCalled();
    s1.dispose();
  });

  it('加载失败：console.warn 降级、无 refresh、不改偏好、不动锁定状态', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const s = await makeSession();
    settleFontLoadFail();
    await vi.waitFor(() => {
      expect(warn).toHaveBeenCalled();
    });
    expect(termInstances[0].refresh).not.toHaveBeenCalled();
    expect(atlasClearSpy).not.toHaveBeenCalled();
    expect(termInstances[0].dispose).not.toHaveBeenCalled(); // 不重建终端
    expect(storageSetItemSpy()).not.toHaveBeenCalled(); // 不改偏好
    expect(lockControllerMock.lock).not.toHaveBeenCalled(); // 不动锁定状态
    expect(lockControllerMock.unlock).not.toHaveBeenCalled();
    warn.mockRestore();
    s.dispose();
  });

  it('无 document.fonts API 环境：跳过加载、不崩溃、终端正常构造', async () => {
    stubDocumentFonts(false);
    const s = await makeSession();
    expect(fontLoadCalls).toHaveLength(0);
    expect(termInstances[0].open).toHaveBeenCalledTimes(1);
    s.dispose();
  });

  it('加载触发不阻塞终端 open（open 先于异步 load；阻塞式实现会让 open 延迟）', async () => {
    // fonts.load 返回永不 settle 的 promise：open 仍应同步完成
    await makeSession();
    expect(termInstances[0].open).toHaveBeenCalledTimes(1);
    expect(fontLoadCalls).toHaveLength(1);
  });

  it('两个存活实例同时刷新（字体成功 settle → 全部存活实例 atlas+refresh）', async () => {
    const s1 = await makeSession();
    const s2 = await makeSession();
    settleFontLoadOk();
    await vi.waitFor(() => {
      expect(termInstances[1].refresh).toHaveBeenCalledWith(0, 23);
    });
    expect(termInstances[0].refresh).toHaveBeenCalledTimes(1);
    expect(termInstances[1].refresh).toHaveBeenCalledTimes(1);
    expect(atlasClearSpy).toHaveBeenCalledTimes(2); // 每个存活实例各清一次 atlas
    s1.dispose();
    s2.dispose();
  });

  it('webgl 加载失败（构造抛错）仍 refresh：DOM renderer 路径无 atlas、不阻断补渲染', async () => {
    harness.webglThrowOnConstruct = true;
    const s = await makeSession();
    settleFontLoadOk();
    await vi.waitFor(() => {
      expect(termInstances[0].refresh).toHaveBeenCalledWith(0, 23);
    });
    expect(atlasClearSpy).not.toHaveBeenCalled(); // 无 webgl atlas 可清
    expect(termInstances[0].refresh).toHaveBeenCalledTimes(1); // refresh 不被阻断
    s.dispose();
  });

  it('@font-face 声明契约完整、字体资产与 LICENSE 就位、TerminalView 已接线（静态断言）', async () => {
    const { fileURLToPath } = await import('node:url');
    const read = (rel: string): string => readFileSync(fileURLToPath(new URL(rel, import.meta.url)), 'utf8');
    const exists = (rel: string): boolean => existsSync(fileURLToPath(new URL(rel, import.meta.url)));

    const css = read('../terminal/fonts.css');
    expect(css).toContain('@font-face');
    expect(css).toContain("font-family: 'Symbols Nerd Font Mono'");
    expect(css).toContain("url('../assets/fonts/SymbolsNerdFontMono-Regular.ttf') format('truetype')");
    expect(css).toContain('font-display: block');
    // N3：unicode-range 限定图标码点范围，杜绝正文误用
    expect(css).toContain('unicode-range: U+E000-F8FF, U+F0000-FFFFD, U+100000-10FFFD');
    // 字体资产 + MIT LICENSE 就位（design D3 资产链）
    expect(exists('../assets/fonts/SymbolsNerdFontMono-Regular.ttf')).toBe(true);
    expect(read('../assets/fonts/LICENSE')).toContain('The MIT License');
    // ora-15 P2：完整许可文本经 vite publicDir（默认 public/）随 dist 分发（MIT 要求随副本分发）
    const licenseText = read('../../public/licenses/nerd-fonts-SymbolsNerdFontMono.txt');
    expect(licenseText).toContain('The MIT License');
    expect(licenseText).toContain('Copyright (c) 2014 Ryan L McIntyre');
    // TerminalView import fonts.css（与 mobile.css 同模式）
    expect(read('../terminal/TerminalView.tsx')).toContain("./fonts.css");
  });
});
