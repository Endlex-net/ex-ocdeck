import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';

/**
 * D1 链接接线测试（terminal-links-emoji-icons 2.2）：
 * - 门控纯函数三路径（metaKey/ctrlKey/无修饰，window.open 桩）：仅修饰键路径同步打开；
 *   被阻止（返回 null）不重试、不抛错；
 * - adapter 接线：Terminal 构造选项 linkHandler.activate 与 WebLinksAddon 构造入参
 *   复用同一 openLink 函数引用（两条入口独立接线、共享门控）；allowNonHttpProtocols
 *   不设置；UnicodeGraphemesAddon 已加载（D2）。
 *
 * 策略：与 session-adapter.test.ts 相同的 vi.mock 全家桶（Node 无 DOM 实例化 TermSession）。
 * 真实 Terminal 的 OSC 8 激活路径见 terminal-links-osc8.test.ts。
 */

// ---------- addon 构造捕获（vi.mock 工厂惰性执行，容器变量在文件求值期已初始化） ----------
const webLinksCtorArgs: unknown[][] = [];
const webLinksInstances: object[] = [];
const graphemesInstances: object[] = [];

vi.mock('@xterm/addon-web-links', () => ({
  WebLinksAddon: vi.fn(function WebLinksAddonMock(this: unknown, ...args: unknown[]) {
    webLinksCtorArgs.push(args);
    const instance = { activate: vi.fn(), dispose: vi.fn() };
    webLinksInstances.push(instance);
    return instance;
  }),
}));
vi.mock('@xterm/addon-unicode-graphemes', () => ({
  UnicodeGraphemesAddon: vi.fn(function UnicodeGraphemesAddonMock(this: unknown) {
    const instance = { activate: vi.fn(), dispose: vi.fn() };
    graphemesInstances.push(instance);
    return instance;
  }),
}));

// ---------- xterm / lock / gestures / preferences / api mocks（对齐 session-adapter） ----------
const termInstance = {
  loadAddon: vi.fn(),
  open: vi.fn(),
  onData: vi.fn(),
  onBinary: vi.fn(),
  write: vi.fn(),
  dispose: vi.fn(),
  blur: vi.fn(),
  focus: vi.fn(),
  input: vi.fn(),
  refresh: vi.fn(),
  cols: 80,
  rows: 24,
  element: null as HTMLElement | null,
  textarea: undefined as HTMLTextAreaElement | undefined,
  options: {} as Record<string, unknown>,
  modes: { mouseTrackingMode: 'none' as const },
  buffer: { active: { type: 'normal' as const } },
  parser: { registerOscHandler: vi.fn(() => ({ dispose: vi.fn() })) },
  attachCustomKeyEventHandler: vi.fn(),
};
vi.mock('@xterm/xterm', () => ({ Terminal: vi.fn(() => termInstance) }));
vi.mock('@xterm/addon-fit', () => ({ FitAddon: vi.fn(() => ({ fit: vi.fn() })) }));
vi.mock('@xterm/addon-webgl', () => ({
  WebglAddon: vi.fn(() => ({ dispose: vi.fn(), clearTextureAtlas: vi.fn() })),
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

// ---------- 全局 stubs（Node 构造 TermSession 所需最小集；对齐 session-adapter） ----------
let windowDefined = false;
let savedMatchMedia: ((q: string) => MediaQueryList) | undefined;
let savedResizeObserver: typeof ResizeObserver | undefined;
let savedRAF: typeof requestAnimationFrame | undefined;
let savedLocalStorage: Storage;
let savedDocument: Document | undefined;
const fakeTextarea = {
  addEventListener: vi.fn(),
  removeEventListener: vi.fn(),
} as unknown as HTMLTextAreaElement;

beforeEach(() => {
  vi.clearAllMocks();
  webLinksCtorArgs.length = 0;
  webLinksInstances.length = 0;
  graphemesInstances.length = 0;
  termInstance.textarea = fakeTextarea;

  if (typeof globalThis.window === 'undefined') {
    windowDefined = true;
    (globalThis as { window: typeof globalThis }).window = globalThis as unknown as typeof globalThis & Window;
  }
  // window.open 桩（openLink 门控路径的唯一出口；返回 null 模拟被浏览器阻止）
  (globalThis.window as unknown as { open: ReturnType<typeof vi.fn> }).open = vi.fn(() => null);

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

  // rAF 桩（applyPreferences → scheduleFit；不执行回调即可）
  savedRAF = globalThis.requestAnimationFrame;
  (globalThis as { requestAnimationFrame: typeof requestAnimationFrame }).requestAnimationFrame = ((cb: () => void) => {
    void cb;
    return 0;
  }) as typeof requestAnimationFrame;

  savedLocalStorage = globalThis.localStorage;
  (globalThis as { localStorage: Storage }).localStorage = {
    getItem: () => null,
    setItem: vi.fn(),
    removeItem: () => {},
    clear: () => {},
    key: () => null,
    length: 0,
  } as unknown as Storage;

  savedDocument = (globalThis as { document?: Document }).document;
  // 无 document.fonts：D3 字体加载守卫应跳过（生命周期测试见 terminal-fonts.test.ts）
  (globalThis as { document: Document }).document = { activeElement: null } as unknown as Document;
});

afterEach(() => {
  (globalThis as { matchMedia: unknown }).matchMedia = savedMatchMedia;
  if (savedResizeObserver !== undefined) {
    (globalThis as { ResizeObserver: typeof ResizeObserver }).ResizeObserver = savedResizeObserver;
  }
  if (savedRAF !== undefined) {
    (globalThis as { requestAnimationFrame: typeof requestAnimationFrame }).requestAnimationFrame = savedRAF;
  }
  (globalThis as { localStorage: Storage }).localStorage = savedLocalStorage;
  if (savedDocument === undefined) delete (globalThis as { document?: Document }).document;
  else (globalThis as { document: Document }).document = savedDocument;
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

describe('D1 链接修饰键门控（openLink 纯函数三路径）', () => {
  it('metaKey → 同步 window.open(uri, "_blank", "noopener,noreferrer") 恰一次', async () => {
    const { openLink } = await import('../terminal/session');
    openLink({ metaKey: true, ctrlKey: false } as MouseEvent, 'https://example.com/path');
    expect(window.open).toHaveBeenCalledTimes(1);
    expect(window.open).toHaveBeenCalledWith('https://example.com/path', '_blank', 'noopener,noreferrer');
  });

  it('ctrlKey → 同步打开（非 macOS 平台语义）', async () => {
    const { openLink } = await import('../terminal/session');
    openLink({ metaKey: false, ctrlKey: true } as MouseEvent, 'https://example.com/path');
    expect(window.open).toHaveBeenCalledTimes(1);
    expect(window.open).toHaveBeenCalledWith('https://example.com/path', '_blank', 'noopener,noreferrer');
  });

  it('无修饰键 → 直接 return，不触发打开（普通点击保持既有点击/选择行为）', async () => {
    const { openLink } = await import('../terminal/session');
    openLink({ metaKey: false, ctrlKey: false } as MouseEvent, 'https://example.com/path');
    openLink({} as MouseEvent, 'https://example.com/path');
    expect(window.open).not.toHaveBeenCalled();
  });

  it('被浏览器阻止（返回 null）→ 不重试、不抛错', async () => {
    const { openLink } = await import('../terminal/session');
    expect(() => openLink({ metaKey: true } as MouseEvent, 'https://example.com/path')).not.toThrow();
    expect(window.open).toHaveBeenCalledTimes(1); // 单次尝试，无异步重试放大
  });
});

describe('D1 两条入口接线复用同一 openLink（adapter 层）', () => {
  it('Terminal 构造选项 linkHandler = { activate: openLink }（OSC 8 入口；allowNonHttpProtocols 不设）', async () => {
    const { TermSession, openLink } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    const { Terminal } = await import('@xterm/xterm');
    // Terminal mock 未声明参数 → calls 元组为空；显式类型化构造选项（仅断言所需形状）
    const ctorOptions = vi.mocked(Terminal).mock.calls[0]![0] as unknown as {
      linkHandler?: { activate: (event: MouseEvent, uri: string) => void } & Record<string, unknown>;
    };
    expect(ctorOptions.linkHandler).toBeDefined();
    // 反证：两条入口各自定义函数（而非复用同一 openLink）时引用相等断言失败
    expect(ctorOptions.linkHandler!.activate).toBe(openLink);
    expect(Object.keys(ctorOptions.linkHandler!)).toEqual(['activate']);
    session.dispose();
  });

  it('WebLinksAddon 以同一 openLink 构造并 loadAddon（纯文本 URL 入口）', async () => {
    const { TermSession, openLink } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    expect(webLinksCtorArgs).toHaveLength(1);
    expect(webLinksCtorArgs[0][0]).toBe(openLink);
    expect(termInstance.loadAddon).toHaveBeenCalledWith(webLinksInstances[0]);
    session.dispose();
  });

  it('UnicodeGraphemesAddon 已加载（D2：loadAddon 即激活 provider）', async () => {
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    expect(graphemesInstances).toHaveLength(1);
    expect(termInstance.loadAddon).toHaveBeenCalledWith(graphemesInstances[0]);
    session.dispose();
  });

  it('D3：初始化与 applyPreferences 共用同一 resolveFontFamily（有效栈经同一变换）', async () => {
    const prefsModule = await import('../terminal/preferences');
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    const resolveMock = vi.mocked(prefsModule.resolveFontFamily);
    // 初始化：构造期经 resolveFontFamily(loadTermPrefs()) 计算有效栈（mock Terminal 不回填 options，
    // 构造期断言以 resolve 调用为准；applyPreferences 路径断言有效栈写入 options）
    expect(resolveMock).toHaveBeenCalledWith({});
    // 偏好更新：applyPreferences({fontFamily}) → 同一 resolveFontFamily → 有效栈写入 options
    resolveMock.mockClear();
    resolveMock.mockReturnValue('Fira Code, "Symbols Nerd Font Mono"');
    session.applyPreferences({ fontFamily: 'Fira Code' });
    expect(resolveMock).toHaveBeenCalledWith({ fontFamily: 'Fira Code' });
    expect(termInstance.options.fontFamily).toBe('Fira Code, "Symbols Nerd Font Mono"');
    session.dispose();
  });
});
