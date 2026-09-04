import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';

/**
 * Session adapter 接线测试：锁定 TermSession 实际委托 LockOrchestrator，
 * 并验证 deps 各槽位绑定到正确的方法（lockController.lock / term.blur / attachGestures 等）。
 *
 * 策略：vi.mock 替换 xterm/addon/preferences/lock/手势层/api，使 TermSession 可在 Node 无 DOM 实例化；
 * vi.mock('./session-coordination') 暴露 spy createLockOrchestrator，断言：
 * - 构造时按移动端模式偏好落地（auto+coarse → lock() + attach，门禁先置位）
 * - auth_ok → orchestrator.onAuthOk 被调用（入参为 appliedCaps.lock）
 * - pointer change → applyMobileCaps 边沿迁移（不再委托 orchestrator.onPointerChange）
 * - 公共 unlock → orchestrator.unlock + term.focus
 * - deps 槽位绑定正确（lock→lockController.lock、blur→term.blur、focus→term.focus、
 *   unlockSilently→lockController.unlock、attachGestures→attachTouchGestures、detachGestures→gestures.dispose）
 *
 * 反证：若 session.ts 不委托 orchestrator（直接内联逻辑），orchestrator spy 不会被调用 → 断言失败。
 */

// ---------- mocks ----------

// capture deps 注入到此对象，供断言绑定关系
let capturedDeps: import('../terminal/session-coordination').LockOrchestratorDeps | null = null;
let capturedHandle: { onAuthOk: ReturnType<typeof vi.fn>; onPointerChange: ReturnType<typeof vi.fn>; lock: ReturnType<typeof vi.fn>; unlock: ReturnType<typeof vi.fn>; dispose: ReturnType<typeof vi.fn> } | null = null;

vi.mock('../terminal/session-coordination', () => ({
  createLockOrchestrator: vi.fn((deps: unknown) => {
    const d = deps as import('../terminal/session-coordination').LockOrchestratorDeps;
    capturedDeps = d;
    capturedHandle = {
      // 复现真实协调器顺序（design D5），使 adapter 测试能验证 lock-before-blur 等：
      // lock = deps.lock + deps.blur；unlock = deps.unlockSilently + deps.focus；
      // onPointerChange(coarse) = deps.lock + deps.blur + deps.attachGestures；
      // onPointerChange(fine) = deps.detachGestures + deps.unlockSilently。
      // onAuthOk(lockEnabled) = 锁定能力启用时 deps.lock + deps.blur 再 onAuthed（lock 先于 authed 暴露）。
      onAuthOk: vi.fn((lockEnabled: boolean, onAuthed: () => void) => {
        if (lockEnabled) { d.lock(); d.blur(); }
        onAuthed();
      }),
      onPointerChange: vi.fn((matchesCoarse: boolean) => {
        if (matchesCoarse) { d.lock(); d.blur(); d.attachGestures(); }
        else { d.detachGestures(); d.unlockSilently(); }
      }),
      lock: vi.fn(() => { d.lock(); d.blur(); }),
      unlock: vi.fn(() => { d.unlockSilently(); d.focus(); }),
      dispose: vi.fn(),
    };
    return capturedHandle;
  }),
}));

const fakeOverlay = {} as HTMLElement;

/** fake wrap：带 style 对象（dispose 时设 maxHeight）+ getBoundingClientRect。
 * style 预置 maxHeight:'' 对齐真实 CSSStyleDeclaration 语义（未设置时读回 '' 而非 undefined）。 */
function fakeWrap(): HTMLElement {
  return { style: { maxHeight: '' }, getBoundingClientRect: () => ({ top: 0, bottom: 0, left: 0, right: 0, width: 0, height: 0, x: 0, y: 0, toJSON: () => ({}) }) } as unknown as HTMLElement;
}
/** fake host：带正 clientWidth/clientHeight（fitNow 提前 return 守卫需正尺寸，否则删除 this.fit.fit() 不失败）。 */
function fakeHost(): HTMLElement {
  return { clientWidth: 800, clientHeight: 600 } as unknown as HTMLElement;
}
// lockController mock：lock/unlock 切换 isLocked 并通知 onChange 订阅者（复现真实 createLockController 语义：同值 no-op 不通知）
const lockChangeListeners = new Set<(locked: boolean) => void>();
const lockControllerMock = {
  lock: vi.fn(() => { if (lockControllerMock.isLocked()) return; lockControllerMock.isLocked.mockReturnValue(true); for (const cb of lockChangeListeners) cb(true); }),
  unlock: vi.fn(() => { if (!lockControllerMock.isLocked()) return; lockControllerMock.isLocked.mockReturnValue(false); for (const cb of lockChangeListeners) cb(false); }),
  isLocked: vi.fn(() => false),
  onChange: vi.fn((cb: (locked: boolean) => void) => { lockChangeListeners.add(cb); return () => { lockChangeListeners.delete(cb); }; }),
  dispose: vi.fn(),
  overlay: fakeOverlay,
};
vi.mock('../terminal/lock', () => ({
  createLockController: vi.fn(() => lockControllerMock),
}));

const gesturesMock = { rebind: vi.fn(), dispose: vi.fn() };
vi.mock('../terminal/touch-gestures', () => ({
  attachTouchGestures: vi.fn(() => gesturesMock),
}));

// mock Terminal：实例方法记录调用，options 可写
// textarea 用 fake EventTarget 充当——每次测试重建（beforeEach），避免跨测试监听累积。
let textareaAddListenerCalls: { type: string; capture: boolean }[];
let fakeTextarea: HTMLTextAreaElement;
function createFakeTextarea(): { calls: { type: string; capture: boolean }[]; el: HTMLTextAreaElement } {
  const target = new EventTarget();
  const calls: { type: string; capture: boolean }[] = [];
  const el = {
    addEventListener: (type: string, l: EventListenerOrEventListenerObject | null, options?: AddEventListenerOptions) => {
      calls.push({ type, capture: Boolean(options?.capture) });
      target.addEventListener(type, l as EventListener, options);
    },
    removeEventListener: (type: string, l: EventListenerOrEventListenerObject | null, options?: EventListenerOptions) => {
      target.removeEventListener(type, l as EventListener, options);
    },
    dispatchEvent: (e: Event) => target.dispatchEvent(e),
  } as unknown as HTMLTextAreaElement;
  return { calls, el };
}
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
  cols: 80,
  rows: 24,
  element: null as HTMLElement | null,
  textarea: undefined as HTMLTextAreaElement | undefined,
  options: {} as Record<string, unknown>,
  modes: { mouseTrackingMode: 'none' as const },
  buffer: { active: { type: 'normal' as const } },
  parser: { registerOscHandler: vi.fn(() => ({ dispose: vi.fn() })) },
};
vi.mock('@xterm/xterm', () => ({
  Terminal: vi.fn(() => termInstance),
}));
// FitAddon mock：暴露稳定 fit spy 供 D6 测试断言 FitAddon 重排被调用
const fitAddonFitSpy = vi.fn();
vi.mock('@xterm/addon-fit', () => ({ FitAddon: vi.fn(() => ({ fit: fitAddonFitSpy })) }));
vi.mock('@xterm/addon-webgl', () => ({ WebglAddon: vi.fn(() => ({ dispose: vi.fn() })) }));

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
  UNAUTHORIZED_EVENT: 'unauthorized',
}));

vi.mock('../terminal/input-gate', async () => {
  const actual = await vi.importActual<typeof import('../terminal/input-gate')>('../terminal/input-gate');
  return { ...actual };
});

// IME compensator mock：捕获 createImeCompensator 入参 + 返回 handle spy
let capturedImeOpts: { emit: (data: string) => void; now: () => number; schedule: (fn: () => void, delayMs?: number) => { cancel(): void } } | null = null;
const imeCompensatorMock = {
  handleKeyDown: vi.fn(),
  handleKeyUp: vi.fn(),
  handleCompositionStart: vi.fn(),
  handleCompositionEnd: vi.fn(),
  handleInput: vi.fn(),
  observeNative: vi.fn(),
  settle: vi.fn(),
  dispose: vi.fn(),
};
vi.mock('../terminal/ime-compensator', () => ({
  createImeCompensator: vi.fn((opts: unknown) => {
    capturedImeOpts = opts as { emit: (data: string) => void; now: () => number; schedule: (fn: () => void, delayMs?: number) => { cancel(): void } };
    return imeCompensatorMock;
  }),
}));

// ---------- 全局 stubs ----------

interface TestEnv {
  matchMediaMatches: boolean;
  mqlListeners: ((e: MediaQueryListEvent) => void)[];
}

let env: TestEnv;
let savedMatchMedia: (q: string) => MediaQueryList;
let savedResizeObserver: typeof ResizeObserver;
let savedLocalStorage: Storage;
let savedRAF: typeof requestAnimationFrame;
let savedCancelRAF: typeof cancelAnimationFrame;
let windowDefined = false;
// visualViewport fake：可控 offsetTop/height + addEventListener/removeEventListener 记录
let vvListeners: { type: string; cb: () => void }[] = [];
let vvOffsetTop = 0;
let vvHeight = 600;
let savedVisualViewport: VisualViewport | undefined;
let savedDocument: Document | undefined;
let savedSetTimeout: typeof setTimeout;
let savedClearTimeout: typeof clearTimeout;
// document.activeElement 控制（D6 UNLOCKED+聚焦判定）
let activeElement: Element | null = null;

beforeEach(async () => {
  env = { matchMediaMatches: false, mqlListeners: [] };
  capturedDeps = null;
  capturedHandle = null;
  capturedImeOpts = null;
  vvListeners = [];
  vvOffsetTop = 0;
  vvHeight = 600;
  activeElement = null;
  lockChangeListeners.clear();
  // 重置 mock 调用记录
  vi.clearAllMocks();
  lockControllerMock.isLocked.mockReturnValue(false);
  // 移动端偏好 mock 默认值重置（clearAllMocks 不清 mockReturnValue，须显式归位，防跨测试泄漏）
  const prefs = await import('../terminal/preferences');
  vi.mocked(prefs.loadMobileMode).mockReturnValue('auto');
  vi.mocked(prefs.loadMobileCaps).mockReturnValue({ version: 1, lock: true, gestures: true, keyboardAvoid: true });
  // 重建 fake textarea（避免跨测试监听累积）
  const ta = createFakeTextarea();
  textareaAddListenerCalls = ta.calls;
  fakeTextarea = ta.el;
  termInstance.textarea = fakeTextarea;

  // 定义 window 全局（Node 无 window；session.ts 用 `typeof window !== 'undefined'` 门禁）。
  // 使 globalThis.window = globalThis 自身，让 matchMedia/Event 等通过 window 访问生效。
  if (typeof globalThis.window === 'undefined') {
    windowDefined = true;
    (globalThis as { window: typeof globalThis }).window = globalThis as unknown as typeof globalThis & Window;
  }

  // window.matchMedia stub：返回可控 mql
  savedMatchMedia = globalThis.matchMedia!;
  (globalThis as { matchMedia: (q: string) => MediaQueryList }).matchMedia = (q: string) =>
    ({
      matches: env.matchMediaMatches,
      media: q,
      onchange: null,
      addListener: () => {},
      removeListener: () => {},
      addEventListener: (_t: string, cb: (e: MediaQueryListEvent) => void) => env.mqlListeners.push(cb),
      removeEventListener: () => {},
      dispatchEvent: () => false,
    }) as MediaQueryList;
  (globalThis.window as { matchMedia: (q: string) => MediaQueryList }).matchMedia = globalThis.matchMedia;

  // visualViewport stub
  savedVisualViewport = (globalThis.window as { visualViewport?: VisualViewport }).visualViewport;
  (globalThis.window as { visualViewport?: VisualViewport }).visualViewport = {
    offsetTop: 0,
    height: 600,
    width: 400,
    pageLeft: 0,
    pageTop: 0,
    scale: 1,
    onresize: null,
    onscroll: null,
    addEventListener: (type: string, cb: () => void) => { vvListeners.push({ type, cb }); },
    removeEventListener: (type: string, cb: () => void) => {
      vvListeners = vvListeners.filter((l) => !(l.type === type && l.cb === cb));
    },
    dispatchEvent: () => false,
  } as unknown as VisualViewport;
  // 暴露 vv offsetTop/height 控制 helper（通过 Object.defineProperty 使 getter 读变量）
  Object.defineProperty(globalThis.window, 'visualViewport', {
    get: () => ({
      get offsetTop() { return vvOffsetTop; },
      get height() { return vvHeight; },
      width: 400, pageLeft: 0, pageTop: 0, scale: 1, onresize: null, onscroll: null,
      addEventListener: (type: string, cb: () => void) => { vvListeners.push({ type, cb }); },
      removeEventListener: (type: string, cb: () => void) => {
        vvListeners = vvListeners.filter((l) => !(l.type === type && l.cb === cb));
      },
      dispatchEvent: () => false,
    }),
    configurable: true,
  });

  // document stub（document.activeElement 用于 D6 聚焦判定）
  savedDocument = (globalThis as { document?: Document }).document;
  (globalThis as { document: Document }).document = {
    activeElement: null,
  } as unknown as Document;
  Object.defineProperty(globalThis.document, 'activeElement', {
    get: () => activeElement,
    configurable: true,
  });

  // requestAnimationFrame / cancelAnimationFrame stub（同步执行 rAF callback，便于 D6 测试）
  savedRAF = globalThis.requestAnimationFrame!;
  savedCancelRAF = globalThis.cancelAnimationFrame!;
  let rafId = 0;
  const rafQueue = new Map<number, () => void>();
  (globalThis as { requestAnimationFrame: typeof requestAnimationFrame }).requestAnimationFrame = ((cb: () => void) => {
    rafId += 1;
    const id = rafId;
    rafQueue.set(id, cb);
    // 同步执行（测试可控；session.ts scheduleFit/scheduleVvFit 依赖此）
    // 这里不立即执行，暴露 flushRaf 供测试手动触发
    void id;
    return id;
  }) as typeof requestAnimationFrame;
  (globalThis as { cancelAnimationFrame: typeof cancelAnimationFrame }).cancelAnimationFrame = ((id: number) => {
    rafQueue.delete(id);
  }) as typeof cancelAnimationFrame;
  // 暴露 flushRaf 到 globalThis 供测试调用
  (globalThis as { __flushRaf?: () => void }).__flushRaf = () => {
    for (const [, cb] of rafQueue) cb();
    rafQueue.clear();
  };

  // setTimeout / clearTimeout stub（schedule=同 Window setTimeout；compensator dispose 用）
  savedSetTimeout = globalThis.setTimeout;
  savedClearTimeout = globalThis.clearTimeout;
  // 用真实 setTimeout（Node 已有），仅保存引用便于 restore

  // ResizeObserver stub：用具名 function（可作构造器）
  savedResizeObserver = globalThis.ResizeObserver!;
  function FakeResizeObserver(this: unknown, _cb: () => void) {
    return {
      observe: () => {},
      unobserve: () => {},
      disconnect: () => {},
    };
  }
  (globalThis as { ResizeObserver: typeof ResizeObserver }).ResizeObserver = FakeResizeObserver as unknown as typeof ResizeObserver;

  // localStorage stub
  savedLocalStorage = globalThis.localStorage!;
  (globalThis as { localStorage: Storage }).localStorage = {
    getItem: () => null,
    setItem: () => {},
    removeItem: () => {},
    clear: () => {},
    key: () => null,
    length: 0,
  } as Storage;
});

afterEach(() => {
  (globalThis as { matchMedia: (q: string) => MediaQueryList }).matchMedia = savedMatchMedia;
  (globalThis as { ResizeObserver: typeof ResizeObserver }).ResizeObserver = savedResizeObserver;
  (globalThis as { localStorage: Storage }).localStorage = savedLocalStorage;
  (globalThis as { requestAnimationFrame: typeof requestAnimationFrame }).requestAnimationFrame = savedRAF;
  (globalThis as { cancelAnimationFrame: typeof cancelAnimationFrame }).cancelAnimationFrame = savedCancelRAF;
  (globalThis as { setTimeout: typeof setTimeout }).setTimeout = savedSetTimeout;
  (globalThis as { clearTimeout: typeof clearTimeout }).clearTimeout = savedClearTimeout;
  delete (globalThis as { __flushRaf?: () => void }).__flushRaf;
  if (savedDocument === undefined) {
    delete (globalThis as { document?: Document }).document;
  } else {
    (globalThis as { document: Document }).document = savedDocument;
  }
  if (savedVisualViewport === undefined) {
    delete (globalThis.window as { visualViewport?: VisualViewport }).visualViewport;
  } else {
    (globalThis.window as { visualViewport?: VisualViewport }).visualViewport = savedVisualViewport;
  }
  if (windowDefined) {
    delete (globalThis as { window?: typeof globalThis }).window;
    windowDefined = false;
  }
});

/** helper：设置 document.activeElement 为 term.textarea（模拟聚焦）。 */
function focusTextarea(): void {
  activeElement = termInstance.textarea as unknown as Element;
}
/** helper：清除聚焦（document.activeElement=null）。 */
function blurTextarea(): void {
  activeElement = null;
}
/** flush rAF 队列（执行 scheduleVvFit/scheduleFit 注册的 rAF callback）。 */
function flushRaf(): void {
  (globalThis as { __flushRaf?: () => void }).__flushRaf?.();
}

/** helper：设置移动端模式偏好 mock（session 构造/重评估经 loadMobileMode/loadMobileCaps 读取）。 */
async function setMobilePrefs(
  mode: 'auto' | 'on' | 'off',
  caps?: { lock?: boolean; gestures?: boolean; keyboardAvoid?: boolean },
): Promise<void> {
  const prefs = await import('../terminal/preferences');
  vi.mocked(prefs.loadMobileMode).mockReturnValue(mode);
  vi.mocked(prefs.loadMobileCaps).mockReturnValue({
    version: 1,
    lock: caps?.lock ?? true,
    gestures: caps?.gestures ?? true,
    keyboardAvoid: caps?.keyboardAvoid ?? true,
  });
}

describe('TermSession adapter → LockOrchestrator 接线', () => {
  it('构造 fine pointer：不 lock、不 attach 手势层、不调用 orchestrator.lock', async () => {
    env.matchMediaMatches = false; // fine
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    expect(capturedDeps).not.toBeNull();
    expect(lockControllerMock.lock).not.toHaveBeenCalled();
    // fine 不 attach 手势层
    const { attachTouchGestures } = await import('../terminal/touch-gestures');
    expect(attachTouchGestures).not.toHaveBeenCalled();
    session.dispose();
  });

  it('构造 coarse pointer：lockController.lock + attachTouchGestures 被调用（门禁先置位）', async () => {
    env.matchMediaMatches = true; // coarse
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    expect(lockControllerMock.lock).toHaveBeenCalled();
    const { attachTouchGestures } = await import('../terminal/touch-gestures');
    expect(attachTouchGestures).toHaveBeenCalled();
    session.dispose();
  });

  it('deps 槽位绑定：lock→lockController.lock、blur→term.blur、unlockSilently→lockController.unlock、focus→term.focus', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    expect(capturedDeps).not.toBeNull();
    const deps = capturedDeps!;

    // 触发各槽位验证绑定
    deps.lock();
    expect(lockControllerMock.lock).toHaveBeenCalled();
    deps.blur();
    expect(termInstance.blur).toHaveBeenCalled();
    deps.unlockSilently();
    expect(lockControllerMock.unlock).toHaveBeenCalled();
    deps.focus();
    expect(termInstance.focus).toHaveBeenCalled();
    session.dispose();
  });

  it('deps.attachGestures → attachTouchGestures 被调用；detachGestures → gestures.dispose', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    const deps = capturedDeps!;
    const attachTouchGesturesMock = vi.mocked(await import('../terminal/touch-gestures')).attachTouchGestures;
    attachTouchGesturesMock.mockClear();

    deps.attachGestures();
    expect(attachTouchGesturesMock).toHaveBeenCalledTimes(1);
    // 重复 attach 不再调（幂等 guard）
    deps.attachGestures();
    expect(attachTouchGesturesMock).toHaveBeenCalledTimes(1);

    deps.detachGestures();
    expect(gesturesMock.dispose).toHaveBeenCalled();

    session.dispose();
  });

  it('公共 lock() → orchestrator.lock 委托', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    session.lock();
    expect(capturedHandle!.lock).toHaveBeenCalledTimes(1);
    session.dispose();
  });

  it('公共 unlock() → orchestrator.unlock 委托（按钮可信手势栈）', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    session.unlock();
    expect(capturedHandle!.unlock).toHaveBeenCalledTimes(1);
    session.dispose();
  });

  it('pointer change → applyMobileCaps 边沿迁移（不再委托 orchestrator.onPointerChange）', async () => {
    env.matchMediaMatches = false; // fine + auto
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    const { attachTouchGestures } = await import('../terminal/touch-gestures');
    // fine → coarse：lock 边沿 false→true → lockOrchestrator.lock（门禁先置位再 blur）+ attach 手势层
    for (const cb of env.mqlListeners) cb({ matches: true } as MediaQueryListEvent);
    expect(lockControllerMock.lock).toHaveBeenCalledTimes(1);
    expect(termInstance.blur).toHaveBeenCalledTimes(1);
    expect(attachTouchGestures).toHaveBeenCalledTimes(1);
    // coarse → fine：lock 边沿 true→false → lockController.unlock（silent，不 focus）+ detach 手势层
    for (const cb of env.mqlListeners) cb({ matches: false } as MediaQueryListEvent);
    expect(lockControllerMock.unlock).toHaveBeenCalledTimes(1);
    expect(termInstance.focus).not.toHaveBeenCalled();
    expect(gesturesMock.dispose).toHaveBeenCalled();
    // 旧委托路径必须移除（两条迁移路径不得并存）：orchestrator.onPointerChange 不再被调用。
    // 反证：session.ts 恢复 onPointerChange 委托或并存时此断言失败。
    expect(capturedHandle!.onPointerChange).not.toHaveBeenCalled();
    session.dispose();
  });

  it('auth_ok fine（auto）：orchestrator.onAuthOk(false, cb) —— appliedCaps.lock=false（锁定能力未启用）', async () => {
    env.matchMediaMatches = false; // fine
    const savedWs = (globalThis as { WebSocket: unknown }).WebSocket;
    let wsInstance: { onopen?: () => void; onmessage?: (ev: MessageEvent) => void; onclose?: (ev: CloseEvent) => void; onerror?: () => void; readyState: number; binaryType: string; send: ReturnType<typeof vi.fn>; close: ReturnType<typeof vi.fn> } | null = null;
    (globalThis as { WebSocket: unknown }).WebSocket = vi.fn(function (this: unknown) {
      wsInstance = {
        readyState: 1, // OPEN
        binaryType: 'arraybuffer',
        send: vi.fn(),
        close: vi.fn(),
      };
      return wsInstance as object;
    }) as unknown as typeof WebSocket;

    try {
      const { TermSession } = await import('../terminal/session');
      const onState = vi.fn();
      const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', onState);
      session.connect();
      expect(wsInstance).not.toBeNull();
      wsInstance!.onmessage!({ data: JSON.stringify({ type: 'auth_ok' }) } as MessageEvent);
      // fine pointer + auto → appliedCaps.lock=false → onAuthOk 第一个参数必须为 false
      expect(capturedHandle!.onAuthOk).toHaveBeenCalledTimes(1);
      expect(capturedHandle!.onAuthOk).toHaveBeenCalledWith(false, expect.any(Function));
      expect(onState).toHaveBeenCalledWith('connected');
      session.dispose();
    } finally {
      (globalThis as { WebSocket: unknown }).WebSocket = savedWs;
    }
  });

  it('auth_ok coarse（auto）：orchestrator.onAuthOk(true, cb) —— appliedCaps.lock=true（重连回锁）', async () => {
    env.matchMediaMatches = true; // coarse
    const savedWs = (globalThis as { WebSocket: unknown }).WebSocket;
    let wsInstance: { onopen?: () => void; onmessage?: (ev: MessageEvent) => void; onclose?: (ev: CloseEvent) => void; onerror?: () => void; readyState: number; binaryType: string; send: ReturnType<typeof vi.fn>; close: ReturnType<typeof vi.fn> } | null = null;
    (globalThis as { WebSocket: unknown }).WebSocket = vi.fn(function (this: unknown) {
      wsInstance = {
        readyState: 1, // OPEN
        binaryType: 'arraybuffer',
        send: vi.fn(),
        close: vi.fn(),
      };
      return wsInstance as object;
    }) as unknown as typeof WebSocket;

    try {
      const { TermSession } = await import('../terminal/session');
      const onState = vi.fn();
      const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', onState);
      session.connect();
      expect(wsInstance).not.toBeNull();
      wsInstance!.onmessage!({ data: JSON.stringify({ type: 'auth_ok' }) } as MessageEvent);
      // coarse pointer + auto → appliedCaps.lock=true → onAuthOk 第一个参数必须为 true（重连回锁）
      expect(capturedHandle!.onAuthOk).toHaveBeenCalledTimes(1);
      expect(capturedHandle!.onAuthOk).toHaveBeenCalledWith(true, expect.any(Function));
      expect(onState).toHaveBeenCalledWith('connected');
      session.dispose();
    } finally {
      (globalThis as { WebSocket: unknown }).WebSocket = savedWs;
    }
  });
});

describe('TermSession adapter → 移动端模式边沿迁移（mobile-terminal-mode-settings 3.x / 5.1）', () => {
  /**
   * 边沿迁移表断言（design D3）。mutation 式自检说明：
   * - 构造/迁移断言：旧实现（coarse 直判、无偏好重评估）下，on+fine、off 等初态断言与
   *   「不重锁不 blur」防护断言失败；
   * - no-op 断言（true→true 不重锁）针对的是「每次重评估无条件重锁」的坏实现；
   * - 「旧委托路径移除」断言在 session.ts 恢复 onPointerChange 委托时失败。
   */

  /** stub WebSocket（auth_ok 触发用），返回当前实例句柄与 restore。 */
  function stubWs(): { current: () => { onmessage?: (ev: MessageEvent) => void } | null; restore: () => void } {
    const saved = (globalThis as { WebSocket: unknown }).WebSocket;
    let current: { readyState: number; binaryType: string; send: ReturnType<typeof vi.fn>; close: ReturnType<typeof vi.fn>; onmessage?: (ev: MessageEvent) => void } | null = null;
    (globalThis as { WebSocket: unknown }).WebSocket = vi.fn(function (this: unknown) {
      const ws = {
        readyState: 1, // OPEN
        binaryType: 'arraybuffer',
        send: vi.fn(),
        close: vi.fn(),
      };
      current = ws;
      return ws as object;
    }) as unknown as typeof WebSocket;
    return {
      current: () => current,
      restore: () => {
        (globalThis as { WebSocket: unknown }).WebSocket = saved;
      },
    };
  }

  it('构造四初态：auto+coarse 全落地（orchestrator lock 路径 + blur + attach 手势层）', async () => {
    env.matchMediaMatches = true; // coarse
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    const { attachTouchGestures } = await import('../terminal/touch-gestures');
    // lock 经 lockOrchestrator.lock（门禁先置位再 blur）——旧实现直接 lockController.lock（无 blur、不经 orchestrator）时失败
    expect(capturedHandle!.lock).toHaveBeenCalledTimes(1);
    expect(lockControllerMock.lock).toHaveBeenCalledTimes(1);
    expect(termInstance.blur).toHaveBeenCalledTimes(1);
    expect(attachTouchGestures).toHaveBeenCalledTimes(1);
    session.dispose();
  });

  it('构造四初态：auto+fine 全 no-op（不 lock 不 blur 不 attach）', async () => {
    env.matchMediaMatches = false; // fine
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    const { attachTouchGestures } = await import('../terminal/touch-gestures');
    expect(capturedHandle!.lock).not.toHaveBeenCalled();
    expect(lockControllerMock.lock).not.toHaveBeenCalled();
    expect(termInstance.blur).not.toHaveBeenCalled();
    expect(attachTouchGestures).not.toHaveBeenCalled();
    expect(capturedHandle!.onPointerChange).not.toHaveBeenCalled();
    session.dispose();
  });

  it('构造四初态：on+lock off+gestures on → 只挂手势（不 lock 不 blur，与 pointer 无关）', async () => {
    await setMobilePrefs('on', { lock: false, gestures: true, keyboardAvoid: true });
    env.matchMediaMatches = false; // fine：on 模式与 pointer 无关
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    const { attachTouchGestures } = await import('../terminal/touch-gestures');
    expect(attachTouchGestures).toHaveBeenCalledTimes(1);
    expect(lockControllerMock.lock).not.toHaveBeenCalled();
    expect(termInstance.blur).not.toHaveBeenCalled();
    session.dispose();
  });

  it('构造四初态：off 全 no-op（coarse 也停用锁定/手势，避让关闭不监听 vv）', async () => {
    await setMobilePrefs('off');
    env.matchMediaMatches = true; // coarse：off 模式与 pointer 无关
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    const { attachTouchGestures } = await import('../terminal/touch-gestures');
    expect(lockControllerMock.lock).not.toHaveBeenCalled();
    expect(termInstance.blur).not.toHaveBeenCalled();
    expect(attachTouchGestures).not.toHaveBeenCalled();
    // 避让关闭：聚焦也不注册 vv 监听（shouldListen 追加 keyboardAvoid 条件）
    focusTextarea();
    fakeTextarea.dispatchEvent(new Event('focus'));
    expect(vvListeners.filter((l) => l.type === 'resize')).toHaveLength(0);
    session.dispose();
  });

  it('lock true→true：偏好重评估（sameCaps）不重锁不 blur，手动解锁状态保持', async () => {
    env.matchMediaMatches = true; // auto+coarse 构造锁定
    const prefs = await import('../terminal/preferences');
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    expect(lockControllerMock.lock).toHaveBeenCalledTimes(1);
    expect(termInstance.blur).toHaveBeenCalledTimes(1);
    lockControllerMock.unlock(); // 用户手动解锁
    expect(lockControllerMock.isLocked()).toBe(false);
    vi.mocked(prefs.loadMobileMode).mockClear();
    // 偏好重评估（TERM_PREFS_CHANGED → applyPreferences → applyMobileCaps）
    session.applyPreferences({});
    // 触发证据：applyPreferences 确实重新读取移动端偏好（旧实现不做移动端重评估 → 失败）
    expect(prefs.loadMobileMode).toHaveBeenCalled();
    // sameCaps：MUST NOT 重锁/blur（无条件重锁的坏实现 → 失败），手动解锁状态保持
    expect(lockControllerMock.lock).toHaveBeenCalledTimes(1);
    expect(termInstance.blur).toHaveBeenCalledTimes(1);
    expect(lockControllerMock.isLocked()).toBe(false);
    session.dispose();
  });

  it('修改非锁定子项（keyboardAvoid）：lock 边沿不变 → 不重锁不 blur 不 unlock', async () => {
    await setMobilePrefs('on', { lock: true, gestures: true, keyboardAvoid: true });
    env.matchMediaMatches = false; // fine：on 模式与 pointer 无关
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    // on+lock on：构造即锁定（旧实现按 pointer 判定 fine 不锁 → 失败）
    expect(lockControllerMock.lock).toHaveBeenCalledTimes(1);
    // 改非锁定子项，lock 子开关保持开（注：on 模式下 lock 开强制 gestures 开，gestures 无独立边沿；
    // gestures true→false 边沿仅在 lock 同步关闭的迁移中出现，由 off/lock-off 用例覆盖）
    await setMobilePrefs('on', { lock: true, gestures: true, keyboardAvoid: false });
    session.applyPreferences({});
    expect(lockControllerMock.lock).toHaveBeenCalledTimes(1); // MUST NOT 重锁
    expect(termInstance.blur).toHaveBeenCalledTimes(1); // MUST NOT blur（不动焦点）
    expect(lockControllerMock.unlock).not.toHaveBeenCalled();
    session.dispose();
  });

  it('lock true→false：lockController.unlock（silent unlock）不 focus，不经 orchestrator.unlock', async () => {
    env.matchMediaMatches = true; // auto+coarse 构造锁定
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    await setMobilePrefs('off');
    session.applyPreferences({});
    // silent unlock：仅移除锁（旧实现无迁移，unlock 不被调 → 失败）
    expect(lockControllerMock.unlock).toHaveBeenCalledTimes(1);
    // 非 orchestrator.unlock 路径（该路径会 focus 唤起键盘，MUST NOT 发生）
    expect(capturedHandle!.unlock).not.toHaveBeenCalled();
    expect(termInstance.focus).not.toHaveBeenCalled();
    expect(gesturesMock.dispose).toHaveBeenCalledTimes(1); // 手势层随 off 一并 detach
    session.dispose();
  });

  it('lock false→true：lockOrchestrator.lock（门禁先置位再 blur）+ attach 手势层', async () => {
    env.matchMediaMatches = false; // auto+fine 构造不锁
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    expect(lockControllerMock.lock).not.toHaveBeenCalled();
    await setMobilePrefs('on', { lock: true, gestures: true, keyboardAvoid: true });
    let gateAtBlur: boolean | null = null;
    termInstance.blur.mockImplementationOnce(() => {
      gateAtBlur = lockControllerMock.isLocked();
    });
    session.applyPreferences({});
    // lock 边沿 false→true → orchestrator lock：门禁（lockController.lock）先置位再 blur
    expect(lockControllerMock.lock).toHaveBeenCalledTimes(1);
    expect(termInstance.blur).toHaveBeenCalledTimes(1);
    expect(gateAtBlur).toBe(true);
    const { attachTouchGestures } = await import('../terminal/touch-gestures');
    expect(attachTouchGestures).toHaveBeenCalledTimes(1); // gestures 边沿 false→true
    session.dispose();
  });

  it('判别式加载：auto/off 下构造与偏好重评估对 loadMobileCaps 零调用，仅 on 模式读取', async () => {
    env.matchMediaMatches = true; // coarse
    const prefs = await import('../terminal/preferences');
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    // auto：构造与重评估均 MUST NOT 读 caps key（非判别式的坏实现 → 失败）
    expect(prefs.loadMobileCaps).not.toHaveBeenCalled();
    session.applyPreferences({});
    expect(prefs.loadMobileCaps).not.toHaveBeenCalled();
    // off：仍零调用
    await setMobilePrefs('off');
    session.applyPreferences({});
    expect(prefs.loadMobileCaps).not.toHaveBeenCalled();
    // 对照：on 模式才发起读取（旧实现从不读取 → 失败）
    await setMobilePrefs('on', { lock: false, gestures: true, keyboardAvoid: true });
    session.applyPreferences({});
    expect(prefs.loadMobileCaps).toHaveBeenCalledTimes(1);
    session.dispose();
  });

  it('auth_ok 在锁定能力启用时无条件回锁（on+fine 也回锁——边沿保护唯一例外）', async () => {
    await setMobilePrefs('on', { lock: true, gestures: true, keyboardAvoid: true });
    env.matchMediaMatches = false; // fine：on 模式与 pointer 无关
    const wsStub = stubWs();
    try {
      const { TermSession } = await import('../terminal/session');
      const onState = vi.fn();
      const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', onState);
      expect(lockControllerMock.lock).toHaveBeenCalledTimes(1); // 构造即锁
      lockControllerMock.unlock(); // 用户手动解锁
      expect(lockControllerMock.isLocked()).toBe(false);
      session.connect();
      wsStub.current()!.onmessage!({ data: JSON.stringify({ type: 'auth_ok' }) } as MessageEvent);
      // appliedCaps.lock=true → onAuthOk(true)（旧实现传 pointerCoarse=false → 失败）
      expect(capturedHandle!.onAuthOk).toHaveBeenCalledWith(true, expect.any(Function));
      expect(lockControllerMock.lock).toHaveBeenCalledTimes(2); // 构造 1 + auth_ok 强制回锁 1
      expect(termInstance.blur).toHaveBeenCalledTimes(2);
      expect(onState).toHaveBeenCalledWith('connected');
      session.dispose();
    } finally {
      wsStub.restore();
    }
  });

  it('auth_ok 在锁定能力关闭时不回锁（on+lock off，即使 coarse）', async () => {
    await setMobilePrefs('on', { lock: false, gestures: true, keyboardAvoid: true });
    env.matchMediaMatches = true; // coarse：on+lock off 与 pointer 无关
    const wsStub = stubWs();
    try {
      const { TermSession } = await import('../terminal/session');
      const onState = vi.fn();
      const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', onState);
      expect(lockControllerMock.lock).not.toHaveBeenCalled();
      session.connect();
      wsStub.current()!.onmessage!({ data: JSON.stringify({ type: 'auth_ok' }) } as MessageEvent);
      // appliedCaps.lock=false → onAuthOk(false)（旧实现传 pointerCoarse=true 回锁 → 失败）
      expect(capturedHandle!.onAuthOk).toHaveBeenCalledWith(false, expect.any(Function));
      expect(lockControllerMock.lock).not.toHaveBeenCalled();
      expect(onState).toHaveBeenCalledWith('connected');
      session.dispose();
    } finally {
      wsStub.restore();
    }
  });

  it('gestures 独立边沿（lock 恒 false）：false→true attach、true→false detach，全程不触碰锁定与焦点', async () => {
    // F-03 补充：既有迁移断言均伴随 lock 边沿变化，此处隔离验证 gestures 自身边沿表。
    // mutation 自检：gestures 迁移被限制为仅 lock 同时变化（删除独立分支）时本用例失败。
    await setMobilePrefs('on', { lock: false, gestures: false, keyboardAvoid: true });
    env.matchMediaMatches = false; // fine
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    const { attachTouchGestures } = await import('../terminal/touch-gestures');
    // 构造：on+lock off+gestures off → 全不启用
    expect(attachTouchGestures).not.toHaveBeenCalled();
    expect(lockControllerMock.lock).not.toHaveBeenCalled();
    expect(termInstance.blur).not.toHaveBeenCalled();
    // 开启手势子开关（lock 保持关）→ gestures 边沿 false→true → attach
    await setMobilePrefs('on', { lock: false, gestures: true, keyboardAvoid: true });
    session.applyPreferences({});
    expect(attachTouchGestures).toHaveBeenCalledTimes(1);
    expect(lockControllerMock.lock).not.toHaveBeenCalled(); // 不重锁
    expect(termInstance.blur).not.toHaveBeenCalled();
    expect(termInstance.focus).not.toHaveBeenCalled();
    // 关闭手势子开关（lock 保持关）→ gestures 边沿 true→false → detach
    await setMobilePrefs('on', { lock: false, gestures: false, keyboardAvoid: true });
    session.applyPreferences({});
    expect(gesturesMock.dispose).toHaveBeenCalledTimes(1);
    expect(lockControllerMock.unlock).not.toHaveBeenCalled(); // 无 lock 边沿，不触碰锁定状态
    session.dispose();
  });

  it('keyboardAvoid false→true 边沿：聚焦态注册 resize/scroll listener，不 lock 不 blur 不 focus', async () => {
    // F-03 补充：keyboardAvoid 既有用例仅覆盖 true→false，此处补 false→true 注册路径。
    // mutation 自检：删除 keyboardAvoid false→true 分支时本用例失败。
    await setMobilePrefs('on', { lock: false, gestures: false, keyboardAvoid: false });
    env.matchMediaMatches = false; // fine
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    // 构造：避让关闭 → 聚焦也不监听
    focusTextarea();
    fakeTextarea.dispatchEvent(new Event('focus'));
    expect(vvListeners).toHaveLength(0);
    // 开启避让子开关（lock 保持关、textarea 已聚焦）→ keyboardAvoid 边沿 false→true
    // → 走既有 shouldListen 判定（unlocked+focused 满足）→ 注册 resize+scroll
    await setMobilePrefs('on', { lock: false, gestures: false, keyboardAvoid: true });
    session.applyPreferences({});
    expect(vvListeners.filter((l) => l.type === 'resize')).toHaveLength(1);
    expect(vvListeners.filter((l) => l.type === 'scroll')).toHaveLength(1);
    // 边沿迁移 MUST NOT 触碰锁定状态与焦点
    expect(lockControllerMock.lock).not.toHaveBeenCalled();
    expect(lockControllerMock.unlock).not.toHaveBeenCalled();
    expect(termInstance.blur).not.toHaveBeenCalled();
    expect(termInstance.focus).not.toHaveBeenCalled();
    session.dispose();
  });
});

describe('TermSession adapter → 键盘避让阈值启发式（mobile-terminal-mode-settings 4.1/4.2 / 5.2）', () => {
  /** 避让用例基座：auto+fine（keyboardAvoid=true）+ 解锁 + 聚焦 → vv 监听已挂。 */
  async function makeAvoidanceSession(): Promise<{ session: import('../terminal/session').TermSession; wrap: HTMLElement }> {
    const { TermSession } = await import('../terminal/session');
    const wrap = fakeWrap();
    const session = new TermSession(fakeHost(), wrap, '/ws/x', () => {});
    lockControllerMock.isLocked.mockReturnValue(false);
    focusTextarea();
    fakeTextarea.dispatchEvent(new Event('focus'));
    return { session, wrap };
  }

  function setWrapRect(wrap: HTMLElement, top: number, bottom: number): void {
    (wrap.getBoundingClientRect as () => DOMRect) = () =>
      ({ top, bottom, left: 0, right: 0, width: 0, height: 0, x: 0, y: 0, toJSON: () => ({}) }) as unknown as DOMRect;
  }

  it('shrink 99px（< 阈值）不收缩、不 refit（工具栏抖动不误伤）', async () => {
    const { session, wrap } = await makeAvoidanceSession();
    setWrapRect(wrap, 0, 599);
    vvOffsetTop = 0;
    vvHeight = 500; // visibleBottom=500 → shrink=599-500=99
    fitAddonFitSpy.mockClear();
    vvListeners.filter((l) => l.type === 'resize')[0].cb();
    flushRaf();
    expect((wrap.style as { maxHeight?: string }).maxHeight).toBe(''); // 保持全高
    expect(fitAddonFitSpy).not.toHaveBeenCalled(); // 目标值未变化不 refit（旧实现无条件收缩+refit → 失败）
    session.dispose();
  });

  it('shrink 100px（恰达阈值）收缩：maxHeight = visibleBottom - wrap.top + refit', async () => {
    const { session, wrap } = await makeAvoidanceSession();
    setWrapRect(wrap, 0, 600);
    vvOffsetTop = 0;
    vvHeight = 500; // shrink=600-500=100（边界含等号）
    fitAddonFitSpy.mockClear();
    vvListeners.filter((l) => l.type === 'resize')[0].cb();
    flushRaf();
    expect((wrap.style as { maxHeight?: string }).maxHeight).toBe('500px');
    expect(fitAddonFitSpy).toHaveBeenCalledTimes(1);
    session.dispose();
  });

  it('已收缩后重复 resize 不翻转、不重复 refit（基线取自然布局，不被自身 maxHeight 污染）', async () => {
    const { session, wrap } = await makeAvoidanceSession();
    const style = wrap.style as { maxHeight?: string };
    // 模拟真实布局：maxHeight 生效时 wrap 底边被抬到 600，未受限自然底边 750。
    // 若实现未先清 inline maxHeight 再测量（坏实现），第二次事件读到收缩态底边 600
    // → shrink=0 <100 → 翻转回全高并重复 refit → 下方断言失败。
    (wrap.getBoundingClientRect as () => DOMRect) = () =>
      ({ top: 50, bottom: style.maxHeight ? 600 : 750, left: 0, right: 0, width: 0, height: 0, x: 0, y: 0, toJSON: () => ({}) }) as unknown as DOMRect;
    vvOffsetTop = 0;
    vvHeight = 500; // visibleBottom=500；自然 shrink=750-500=250
    fitAddonFitSpy.mockClear();
    const resizeCb = vvListeners.filter((l) => l.type === 'resize')[0].cb;
    resizeCb();
    flushRaf();
    expect(style.maxHeight).toBe('450px'); // 500-50
    expect(fitAddonFitSpy).toHaveBeenCalledTimes(1);
    resizeCb();
    flushRaf();
    // 目标值不变：不翻转回全高、不重复 refit
    expect(style.maxHeight).toBe('450px');
    expect(fitAddonFitSpy).toHaveBeenCalledTimes(1);
    session.dispose();
  });

  it('offsetTop 变化（Safari 平移）→ 目标高度随之更新并 refit', async () => {
    const { session, wrap } = await makeAvoidanceSession();
    const style = wrap.style as { maxHeight?: string };
    (wrap.getBoundingClientRect as () => DOMRect) = () =>
      ({ top: 50, bottom: style.maxHeight ? 600 : 750, left: 0, right: 0, width: 0, height: 0, x: 0, y: 0, toJSON: () => ({}) }) as unknown as DOMRect;
    vvOffsetTop = 0;
    vvHeight = 500; // visibleBottom=500 → 收缩 '450px'
    fitAddonFitSpy.mockClear();
    const resizeCb = vvListeners.filter((l) => l.type === 'resize')[0].cb;
    resizeCb();
    flushRaf();
    expect(style.maxHeight).toBe('450px');
    // offsetTop 变化：visibleBottom=600，自然 shrink=750-600=150 ≥100 → 目标 550px
    vvOffsetTop = 100;
    resizeCb();
    flushRaf();
    expect(style.maxHeight).toBe('550px');
    expect(fitAddonFitSpy).toHaveBeenCalledTimes(2); // 目标值变化才 refit
    session.dispose();
  });

  it('键盘收起（shrink < 阈值）恢复全高布局并 refit', async () => {
    const { session, wrap } = await makeAvoidanceSession();
    setWrapRect(wrap, 0, 650);
    const style = wrap.style as { maxHeight?: string };
    vvOffsetTop = 0;
    vvHeight = 500; // shrink=150 → 收缩
    const resizeCb = vvListeners.filter((l) => l.type === 'resize')[0].cb;
    resizeCb();
    flushRaf();
    expect(style.maxHeight).toBe('500px');
    // 键盘收起：vv 恢复 → shrink=650-600=50 <100 → 恢复全高（与工具栏抖动共用同一路径）
    vvOffsetTop = 0;
    vvHeight = 600;
    fitAddonFitSpy.mockClear();
    resizeCb();
    flushRaf();
    expect(style.maxHeight).toBe('');
    expect(fitAddonFitSpy).toHaveBeenCalledTimes(1); // 恢复时 refit
    session.dispose();
  });

  it('keyboardAvoid true→false 边沿：detach listener + 清 maxHeight + refit', async () => {
    const { session, wrap } = await makeAvoidanceSession(); // auto+fine → keyboardAvoid=true
    setWrapRect(wrap, 0, 650);
    vvOffsetTop = 0;
    vvHeight = 500;
    vvListeners.filter((l) => l.type === 'resize')[0].cb();
    flushRaf();
    const style = wrap.style as { maxHeight?: string };
    expect(style.maxHeight).toBe('500px');
    // 设置页关闭避让子开关 → TERM_PREFS_CHANGED → applyPreferences → 边沿 true→false
    await setMobilePrefs('on', { lock: false, gestures: true, keyboardAvoid: false });
    fitAddonFitSpy.mockClear();
    session.applyPreferences({});
    expect(vvListeners.filter((l) => l.type === 'resize')).toHaveLength(0); // detach
    expect(vvListeners.filter((l) => l.type === 'scroll')).toHaveLength(0);
    expect(style.maxHeight).toBe(''); // 清 wrap maxHeight
    expect(fitAddonFitSpy).toHaveBeenCalledTimes(1); // refit（旧实现无边沿迁移 → 失败）
    session.dispose();
  });

  it('避让关闭（on+keyboardAvoid off）：聚焦也不注册 vv 监听、无 maxHeight', async () => {
    await setMobilePrefs('on', { lock: false, gestures: true, keyboardAvoid: false });
    const { TermSession } = await import('../terminal/session');
    const wrap = fakeWrap();
    const session = new TermSession(fakeHost(), wrap, '/ws/x', () => {});
    lockControllerMock.isLocked.mockReturnValue(false);
    focusTextarea();
    fakeTextarea.dispatchEvent(new Event('focus'));
    expect(vvListeners.filter((l) => l.type === 'resize')).toHaveLength(0);
    expect(vvListeners.filter((l) => l.type === 'scroll')).toHaveLength(0);
    expect((wrap.style as { maxHeight?: string }).maxHeight).toBeFalsy();
    session.dispose();
  });
});

describe('TermSession adapter → 跨标签页偏好迁移（mobile-terminal-mode-settings 5.3）', () => {
  it('storage 事件 → TERM_PREFS_CHANGED 通道 → 已打开终端按新偏好迁移（silent unlock，不 focus）', async () => {
    env.matchMediaMatches = true; // auto+coarse 构造锁定
    const prefs = await import('../terminal/preferences');
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    expect(lockControllerMock.isLocked()).toBe(true);
    // 复用 TerminalView 的接线：TERM_PREFS_CHANGED → session.applyPreferences(...)
    // （Node 无 window 事件总线，用 EventTarget 承载同名通道，接线语义一致）
    const bus = new EventTarget();
    const apply = () => session.applyPreferences({});
    bus.addEventListener(prefs.TERM_PREFS_CHANGED, apply);
    // 模拟另一标签页写入 off 后本页收到 storage 事件：session 不缓存偏好，迁移按新鲜读取生效
    vi.mocked(prefs.loadMobileMode).mockReturnValue('off');
    bus.dispatchEvent(new CustomEvent(prefs.TERM_PREFS_CHANGED));
    // lock 边沿 true→false：silent unlock（不 focus）+ 手势层 detach
    expect(lockControllerMock.isLocked()).toBe(false);
    expect(termInstance.focus).not.toHaveBeenCalled();
    expect(gesturesMock.dispose).toHaveBeenCalledTimes(1);
    bus.removeEventListener(prefs.TERM_PREFS_CHANGED, apply);
    session.dispose();
  });
});

describe('TermSession adapter → IME compensator 接线（4.6）', () => {
  it('构造时 createImeCompensator 被调用，emit=term.input(data,true)、now/schedule 注入', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    expect(capturedImeOpts).not.toBeNull();
    // emit 同步调 term.input——验证绑定（emit 调用 → term.input 被调）
    capturedImeOpts!.emit('？');
    expect(termInstance.input).toHaveBeenCalledWith('？', true);
    expect(typeof capturedImeOpts!.now).toBe('function');
    expect(typeof capturedImeOpts!.schedule).toBe('function');
    session.dispose();
  });

  it('IME capture 五类事件注册在 textarea（keydown/keyup/compositionstart/compositionend/input）均 capture:true + focus/blur 注册', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    // 五类 IME 事件必须 capture:true（design D7 capture input）
    const captureEvents = ['keydown', 'keyup', 'compositionstart', 'compositionend', 'input'];
    for (const type of captureEvents) {
      const calls = textareaAddListenerCalls.filter((c) => c.type === type);
      expect(calls.length).toBeGreaterThanOrEqual(1);
      expect(calls[0].capture).toBe(true);
    }
    // focus/blur 注册（触发 visualViewport 切换，capture 非强制）
    for (const type of ['focus', 'blur']) {
      const calls = textareaAddListenerCalls.filter((c) => c.type === type);
      expect(calls.length).toBeGreaterThanOrEqual(1);
    }
    session.dispose();
  });

  it('onData 顺序：observeNative 先于 sendInput（WS send）', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const onState = vi.fn();
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', onState);
    // stub WebSocket 捕获 send（带 OPEN 常量）
    const savedWs = (globalThis as { WebSocket: unknown }).WebSocket;
    let wsInstance: { readyState: number; binaryType: string; send: ReturnType<typeof vi.fn>; close: ReturnType<typeof vi.fn>; onmessage?: (ev: MessageEvent) => void } | null = null;
    const WsCtor = vi.fn(function (this: unknown) {
      wsInstance = { readyState: 1, binaryType: 'arraybuffer', send: vi.fn(), close: vi.fn() };
      return wsInstance as object;
    }) as unknown as typeof WebSocket;
    (WsCtor as unknown as { OPEN: number }).OPEN = 1;
    (globalThis as { WebSocket: unknown }).WebSocket = WsCtor;
    try {
      session.connect();
      wsInstance!.onmessage!({ data: JSON.stringify({ type: 'auth_ok' }) } as MessageEvent);
      // auth_ok 后 authed=true；模拟 xterm onData 触发
      const onDataCallback = termInstance.onData.mock.calls[0][0] as (d: string) => void;
      const order: string[] = [];
      imeCompensatorMock.observeNative.mockImplementationOnce(() => { order.push('observeNative'); });
      const ws = wsInstance!;
      const origSend = ws.send;
      ws.send = vi.fn((d: unknown) => { order.push('sendInput'); origSend(d); }) as unknown as typeof ws.send;
      onDataCallback('x');
      expect(order).toEqual(['observeNative', 'sendInput']);
      session.dispose();
    } finally {
      (globalThis as { WebSocket: unknown }).WebSocket = savedWs;
    }
  });

  it('textarea input 事件 → compensator.handleInput 被调（InputEvent 字段透传）', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    // 找到 input 监听并触发
    const inputCalls = textareaAddListenerCalls.filter((c) => c.type === 'input');
    expect(inputCalls.length).toBeGreaterThanOrEqual(1);
    // 构造 InputEvent 派发到 fake textarea
    const ev = new Event('input');
    Object.defineProperties(ev, {
      inputType: { value: 'insertText', configurable: true },
      data: { value: '？', configurable: true },
      isTrusted: { value: true, configurable: true },
      composed: { value: true, configurable: true },
      isComposing: { value: false, configurable: true },
    });
    fakeTextarea.dispatchEvent(ev);
    expect(imeCompensatorMock.handleInput).toHaveBeenCalledTimes(1);
    const arg = imeCompensatorMock.handleInput.mock.calls[0][0];
    expect(arg.inputType).toBe('insertText');
    expect(arg.data).toBe('？');
    expect(arg.isTrusted).toBe(true);
    expect(arg.composed).toBe(true);
    expect(arg.isComposing).toBe(false);
    session.dispose();
  });

  it('dispose 调 compensator.dispose', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    session.dispose();
    expect(imeCompensatorMock.dispose).toHaveBeenCalledTimes(1);
  });

  it('keydown/keyup/compositionstart/compositionend 四类 listener 派发验证绑定到正确 handler', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    // keydown 派发（带 key 字段）
    const kd = new Event('keydown');
    Object.defineProperty(kd, 'key', { value: 'Shift', configurable: true });
    fakeTextarea.dispatchEvent(kd);
    expect(imeCompensatorMock.handleKeyDown).toHaveBeenCalledTimes(1);
    // keyup 派发
    fakeTextarea.dispatchEvent(new Event('keyup'));
    expect(imeCompensatorMock.handleKeyUp).toHaveBeenCalledTimes(1);
    // compositionstart 派发
    fakeTextarea.dispatchEvent(new Event('compositionstart'));
    expect(imeCompensatorMock.handleCompositionStart).toHaveBeenCalledTimes(1);
    // compositionend 派发
    fakeTextarea.dispatchEvent(new Event('compositionend'));
    expect(imeCompensatorMock.handleCompositionEnd).toHaveBeenCalledTimes(1);
    session.dispose();
  });

  it('生产 scheduler 接线：schedule(fn) 走全局 setTimeout(fn,0)、返回 handle.cancel() 调 clearTimeout', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    expect(capturedImeOpts).not.toBeNull();
    // 用 fake timers 观测 setTimeout/clearTimeout（生产 schedule = 同 Window setTimeout(fn, delayMs ?? 0)）
    vi.useFakeTimers();
    try {
      let ran = false;
      capturedImeOpts!.schedule(() => { ran = true; });
      // 未 advance 0ms 前 callback 不执行
      expect(ran).toBe(false);
      // advance 0ms → setTimeout(0) 触发
      vi.advanceTimersByTime(0);
      expect(ran).toBe(true);
      // cancel 调 clearTimeout：新 schedule + cancel 不执行
      let ran2 = false;
      const handle2 = capturedImeOpts!.schedule(() => { ran2 = true; });
      handle2.cancel();
      vi.advanceTimersByTime(10);
      expect(ran2).toBe(false); // cancel 后不执行
      // 反证：session.ts 改 queueMicrotask 时 schedule 不走 setTimeout → fake timers 无法观测 → ran 永远 false → 失败
    } finally {
      vi.useRealTimers();
    }
    session.dispose();
  });
});

describe('TermSession adapter → onBinary 双出口门禁（4.5 真实调用链）', () => {
  it('onBinary 注册回调经统一门禁 sendInput、按 charCode 原始字节发送（含 >127）', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    // stub WebSocket 捕获 send（带 OPEN 常量）
    const savedWs = (globalThis as { WebSocket: unknown }).WebSocket;
    let wsInstance: { readyState: number; binaryType: string; send: ReturnType<typeof vi.fn>; close: ReturnType<typeof vi.fn>; onmessage?: (ev: MessageEvent) => void } | null = null;
    const WsCtor = vi.fn(function (this: unknown) {
      wsInstance = { readyState: 1, binaryType: 'arraybuffer', send: vi.fn(), close: vi.fn() };
      return wsInstance as object;
    }) as unknown as typeof WebSocket;
    (WsCtor as unknown as { OPEN: number }).OPEN = 1;
    (globalThis as { WebSocket: unknown }).WebSocket = WsCtor;
    try {
      session.connect();
      wsInstance!.onmessage!({ data: JSON.stringify({ type: 'auth_ok' }) } as MessageEvent);
      // 捕获 onBinary 注册回调（构造时 term.onBinary 注册）
      const onBinaryCallback = termInstance.onBinary.mock.calls[0][0] as (d: string) => void;
      wsInstance!.send.mockClear();
      // 构造含 >127 字节的 binary string（鼠标控制序列编码）
      const binStr = String.fromCharCode(0xe1, 0x20, 0xff);
      onBinaryCallback(binStr);
      // 经统一门禁 sendInput → encodeBinaryInput（charCode & 0xFF）→ ws.send(Uint8Array)
      const sentArgs = wsInstance!.send.mock.calls.map((c: unknown[]) => c[0]);
      const binSend = sentArgs.find((a) => a instanceof Uint8Array) as Uint8Array | undefined;
      expect(binSend).toBeDefined();
      expect(Array.from(binSend!)).toEqual([0xe1, 0x20, 0xff]); // >127 字节按 charCode 原始值，不被 UTF-8 破坏
      session.dispose();
    } finally {
      (globalThis as { WebSocket: unknown }).WebSocket = savedWs;
    }
  });

  it('onBinary locked 拒绝：锁定态门禁拦截 onBinary 不发送', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    const savedWs = (globalThis as { WebSocket: unknown }).WebSocket;
    let wsInstance: { readyState: number; binaryType: string; send: ReturnType<typeof vi.fn>; close: ReturnType<typeof vi.fn>; onmessage?: (ev: MessageEvent) => void } | null = null;
    const WsCtor = vi.fn(function (this: unknown) {
      wsInstance = { readyState: 1, binaryType: 'arraybuffer', send: vi.fn(), close: vi.fn() };
      return wsInstance as object;
    }) as unknown as typeof WebSocket;
    (WsCtor as unknown as { OPEN: number }).OPEN = 1;
    (globalThis as { WebSocket: unknown }).WebSocket = WsCtor;
    try {
      session.connect();
      wsInstance!.onmessage!({ data: JSON.stringify({ type: 'auth_ok' }) } as MessageEvent);
      const onBinaryCallback = termInstance.onBinary.mock.calls[0][0] as (d: string) => void;
      wsInstance!.send.mockClear();
      // 锁定态：门禁拒绝 onBinary
      lockControllerMock.lock(); // 置 isLocked=true
      onBinaryCallback(String.fromCharCode(0xe1));
      // 锁定态门禁拦截 → 无 Uint8Array 发送
      const sentArgs = wsInstance!.send.mock.calls.map((c: unknown[]) => c[0]);
      const binSend = sentArgs.find((a) => a instanceof Uint8Array);
      expect(binSend).toBeUndefined();
      session.dispose();
    } finally {
      (globalThis as { WebSocket: unknown }).WebSocket = savedWs;
    }
  });
});

describe('TermSession adapter → 锁状态订阅投影 onLockChange（4.5）', () => {
  it('lock()/unlock() 后 onLockChange 回调收到正确 locked 值', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    const states: boolean[] = [];
    const unsub = session.onLockChange((locked) => states.push(locked));
    // lock() → 协调器 lock → lockController.lock（置 isLocked=true）→ onChange 触发回调
    session.lock();
    expect(states).toEqual([true]);
    // unlock() → 协调器 unlock → lockController.unlock（置 isLocked=false）→ onChange 触发回调
    session.unlock();
    expect(states).toEqual([true, false]);
    unsub();
    session.dispose();
  });

  it('auth_ok coarse → onLockChange 收到 true（重连回锁投影）', async () => {
    env.matchMediaMatches = true; // coarse
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    const states: boolean[] = [];
    session.onLockChange((locked) => states.push(locked));
    // 构造已按 auto+coarse 落地锁定；生产 lock() 同值 no-op 不通知（lock.ts:62-69），
    // 先解锁制造 false 边沿，使 auth_ok 强制回锁产生真实 true 投影。
    session.unlock();
    expect(states).toEqual([false]);
    const savedWs = (globalThis as { WebSocket: unknown }).WebSocket;
    let wsInstance: { readyState: number; binaryType: string; send: ReturnType<typeof vi.fn>; close: ReturnType<typeof vi.fn>; onmessage?: (ev: MessageEvent) => void } | null = null;
    const WsCtor = vi.fn(function (this: unknown) {
      wsInstance = { readyState: 1, binaryType: 'arraybuffer', send: vi.fn(), close: vi.fn() };
      return wsInstance as object;
    }) as unknown as typeof WebSocket;
    (WsCtor as unknown as { OPEN: number }).OPEN = 1;
    (globalThis as { WebSocket: unknown }).WebSocket = WsCtor;
    try {
      session.connect();
      wsInstance!.onmessage!({ data: JSON.stringify({ type: 'auth_ok' }) } as MessageEvent);
      // 锁定能力启用 → auth_ok 强制回锁 → onLockChange 投影 true
      expect(states).toEqual([false, true]);
      session.dispose();
    } finally {
      (globalThis as { WebSocket: unknown }).WebSocket = savedWs;
    }
  });

  it('pointer 转 fine → onLockChange 收到 false（自动解锁投影）', async () => {
    env.matchMediaMatches = true; // 初始 coarse
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    // 构造时 coarse 已 lock（onLockChange 订阅在构造后，错过初始 true，符合订阅语义）
    const states: boolean[] = [];
    session.onLockChange((locked) => states.push(locked));
    // pointer 转 fine：matchMedia change 事件 matches=false → applyMobileCaps 边沿迁移
    // （auto+coarse→fine：lock 边沿 true→false）→ lockController.unlock（置 isLocked=false + notify）
    // → onLockChange 收到 false（不再经协调器 onPointerChange 委托）
    const evt = { matches: false } as MediaQueryListEvent;
    for (const cb of env.mqlListeners) cb(evt);
    expect(states).toEqual([false]);
    session.dispose();
  });
});

describe('TermSession adapter → visualViewport 生命周期（D6, 4.1）', () => {
  it('UNLOCKED 且 textarea 聚焦 → 注册 resize+scroll listener（2 个）', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    // 模拟 unlock（orchestrator.unlock 不实际改 lockController，mock isLocked=false）+ 聚焦
    lockControllerMock.isLocked.mockReturnValue(false);
    focusTextarea();
    // 触发 focus 事件 → updateVisualViewportListener
    fakeTextarea.dispatchEvent(new Event('focus'));
    const resizeListeners = vvListeners.filter((l) => l.type === 'resize');
    const scrollListeners = vvListeners.filter((l) => l.type === 'scroll');
    expect(resizeListeners).toHaveLength(1);
    expect(scrollListeners).toHaveLength(1);
    session.dispose();
  });

  it('resize 达阈值（shrink≥100）→ 按 max(0, vv.offsetTop+vv.height - wrap.top) 设 wrap maxHeight', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const wrap = fakeWrap();
    const session = new TermSession({} as HTMLElement, wrap, '/ws/x', () => {});
    lockControllerMock.isLocked.mockReturnValue(false);
    focusTextarea();
    fakeTextarea.dispatchEvent(new Event('focus'));
    // 设置 vv offsetTop=100, height=400 → visibleBottom=500；自然布局 rect.top=50、rect.bottom=650
    // → shrink = 650-500 = 150 ≥ 100 → maxHeight = 500-50 = 450
    vvOffsetTop = 100;
    vvHeight = 400;
    (wrap.getBoundingClientRect as () => DOMRect) = () => ({ top: 50, bottom: 650, left: 0, right: 0, width: 0, height: 0, x: 0, y: 0, toJSON: () => ({}) }) as unknown as DOMRect;
    // 触发 resize listener
    const resizeCb = vvListeners.filter((l) => l.type === 'resize')[0].cb;
    resizeCb();
    flushRaf();
    expect((wrap.style as { maxHeight?: string }).maxHeight).toBe('450px');
    session.dispose();
  });

  it('目标高度 clamp 负值为 0（vv 不可见时 wrap.top > visibleBottom，shrink 达阈值）', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const wrap = fakeWrap();
    const session = new TermSession({} as HTMLElement, wrap, '/ws/x', () => {});
    lockControllerMock.isLocked.mockReturnValue(false);
    focusTextarea();
    fakeTextarea.dispatchEvent(new Event('focus'));
    vvOffsetTop = 0;
    vvHeight = 100;
    (wrap.getBoundingClientRect as () => DOMRect) = () => ({ top: 200, bottom: 800, left: 0, right: 0, width: 0, height: 0, x: 0, y: 0, toJSON: () => ({}) }) as unknown as DOMRect;
    vvListeners.filter((l) => l.type === 'resize')[0].cb();
    flushRaf();
    expect((wrap.style as { maxHeight?: string }).maxHeight).toBe('0px');
    session.dispose();
  });

  it('blur → 移除 resize/scroll listener + 清除 maxHeight 内联样式', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const wrap = fakeWrap();
    const session = new TermSession({} as HTMLElement, wrap, '/ws/x', () => {});
    lockControllerMock.isLocked.mockReturnValue(false);
    focusTextarea();
    fakeTextarea.dispatchEvent(new Event('focus'));
    // 设 maxHeight
    vvOffsetTop = 100; vvHeight = 400;
    (wrap.getBoundingClientRect as () => DOMRect) = () => ({ top: 50, bottom: 650, left: 0, right: 0, width: 0, height: 0, x: 0, y: 0, toJSON: () => ({}) }) as unknown as DOMRect;
    vvListeners.filter((l) => l.type === 'resize')[0].cb();
    flushRaf();
    expect((wrap.style as { maxHeight?: string }).maxHeight).toBe('450px');
    // blur
    blurTextarea();
    fakeTextarea.dispatchEvent(new Event('blur'));
    expect(vvListeners.filter((l) => l.type === 'resize')).toHaveLength(0);
    expect(vvListeners.filter((l) => l.type === 'scroll')).toHaveLength(0);
    expect((wrap.style as { maxHeight?: string }).maxHeight).toBe('');
    session.dispose();
  });

  it('lock → 移除 visualViewport listener（锁定不监听键盘视口）', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    lockControllerMock.isLocked.mockReturnValue(false);
    focusTextarea();
    fakeTextarea.dispatchEvent(new Event('focus'));
    expect(vvListeners.filter((l) => l.type === 'resize')).toHaveLength(1);
    // lock：lockController.onChange 触发 updateVisualViewportListener；isLocked=true → shouldListen=false
    lockControllerMock.isLocked.mockReturnValue(true);
    // 触发 onChange（lockController mock 的 onChange 在构造时注册了 cb）
    const onChangeCb = (lockControllerMock.onChange.mock.calls[0] as unknown as [(locked: boolean) => void])[0];
    onChangeCb(true);
    expect(vvListeners.filter((l) => l.type === 'resize')).toHaveLength(0);
    expect(vvListeners.filter((l) => l.type === 'scroll')).toHaveLength(0);
    session.dispose();
  });

  it('visualViewport API 缺失 → 跳过（不注册 listener，不报错）', async () => {
    env.matchMediaMatches = false;
    // 删除 visualViewport
    delete (globalThis.window as { visualViewport?: VisualViewport }).visualViewport;
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    lockControllerMock.isLocked.mockReturnValue(false);
    focusTextarea();
    fakeTextarea.dispatchEvent(new Event('focus'));
    // 无 vv → 不注册
    expect(vvListeners).toHaveLength(0);
    session.dispose();
  });

  it('dispose 移除 visualViewport resize/scroll listener（防全局 vv 持续引用已销毁 session）', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    lockControllerMock.isLocked.mockReturnValue(false);
    focusTextarea();
    fakeTextarea.dispatchEvent(new Event('focus'));
    expect(vvListeners.filter((l) => l.type === 'resize')).toHaveLength(1);
    expect(vvListeners.filter((l) => l.type === 'scroll')).toHaveLength(1);
    session.dispose();
    // dispose 后 listener 全部移除
    expect(vvListeners.filter((l) => l.type === 'resize')).toHaveLength(0);
    expect(vvListeners.filter((l) => l.type === 'scroll')).toHaveLength(0);
  });

  it('scroll 达阈值同公式收缩（Safari 平移 visual viewport，vv.offsetTop 变化）', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const wrap = fakeWrap();
    const session = new TermSession({} as HTMLElement, wrap, '/ws/x', () => {});
    lockControllerMock.isLocked.mockReturnValue(false);
    focusTextarea();
    fakeTextarea.dispatchEvent(new Event('focus'));
    // scroll 平移：vv.offsetTop 变化（Safari 为保持输入点可见平移 visual viewport）
    vvOffsetTop = 80;
    vvHeight = 300;
    (wrap.getBoundingClientRect as () => DOMRect) = () => ({ top: 30, bottom: 530, left: 0, right: 0, width: 0, height: 0, x: 0, y: 0, toJSON: () => ({}) }) as unknown as DOMRect;
    // 触发 scroll listener：visibleBottom=380，shrink=530-380=150 ≥ 100 → maxHeight = 380-30 = 350
    const scrollCb = vvListeners.filter((l) => l.type === 'scroll')[0].cb;
    scrollCb();
    flushRaf();
    expect((wrap.style as { maxHeight?: string }).maxHeight).toBe('350px');
    session.dispose();
  });

  it('键盘展开后 fitNow 触发 → FitAddon 重排 + WS resize 帧（D6 核心结果）', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const wrap = fakeWrap();
    const host = fakeHost(); // 正尺寸，否则 fitNow 提前 return（删除 this.fit.fit() 也不失败）
    const session = new TermSession(host, wrap, '/ws/x', () => {});
    // stub WebSocket 捕获 resize 帧（带 OPEN 常量）
    const savedWs = (globalThis as { WebSocket: unknown }).WebSocket;
    let wsInstance: { readyState: number; binaryType: string; send: ReturnType<typeof vi.fn>; close: ReturnType<typeof vi.fn>; onmessage?: (ev: MessageEvent) => void } | null = null;
    const WsCtor = vi.fn(function (this: unknown) {
      wsInstance = { readyState: 1, binaryType: 'arraybuffer', send: vi.fn(), close: vi.fn() };
      return wsInstance as object;
    }) as unknown as typeof WebSocket;
    (WsCtor as unknown as { OPEN: number }).OPEN = 1;
    (globalThis as { WebSocket: unknown }).WebSocket = WsCtor;
    try {
      session.connect();
      wsInstance!.onmessage!({ data: JSON.stringify({ type: 'auth_ok' }) } as MessageEvent);
      // auth_ok 后 authed=true；注册 vv listener
      lockControllerMock.isLocked.mockReturnValue(false);
      focusTextarea();
      fakeTextarea.dispatchEvent(new Event('focus'));
      vvOffsetTop = 100; vvHeight = 400;
      (wrap.getBoundingClientRect as () => DOMRect) = () => ({ top: 50, bottom: 650, left: 0, right: 0, width: 0, height: 0, x: 0, y: 0, toJSON: () => ({}) }) as unknown as DOMRect;
      // resize 触发 → scheduleVvFit → fitForViewport → fitNow → FitAddon.fit() + WS resize 帧
      wsInstance!.send.mockClear(); // 清掉 auth_ok 后 fitNow 的 resize 帧
      fitAddonFitSpy.mockClear(); // 清掉构造/auth_ok 期间的 fit 调用
      vvListeners.filter((l) => l.type === 'resize')[0].cb();
      flushRaf();
      // FitAddon 重排真实断言：fit() 被调用（删除生产 this.fit.fit() 时此项失败）
      expect(fitAddonFitSpy).toHaveBeenCalled();
      // fitNow 发送 resize JSON 帧（cols/rows）
      const resizeCall = wsInstance!.send.mock.calls.find((c: unknown[]) => {
        const arg = c[0];
        return typeof arg === 'string' && arg.includes('"type":"resize"');
      });
      expect(resizeCall).toBeDefined();
      expect((wrap.style as { maxHeight?: string }).maxHeight).toBe('450px');
      session.dispose();
    } finally {
      (globalThis as { WebSocket: unknown }).WebSocket = savedWs;
    }
  });

  it('事件入队 rAF 后先 blur/dispose、再 flush rAF 不得恢复 maxHeight（pending rAF 取消）', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const wrap = fakeWrap();
    const session = new TermSession({} as HTMLElement, wrap, '/ws/x', () => {});
    lockControllerMock.isLocked.mockReturnValue(false);
    focusTextarea();
    fakeTextarea.dispatchEvent(new Event('focus'));
    // 触发 resize 入队 rAF（不 flush）
    vvOffsetTop = 100; vvHeight = 400;
    (wrap.getBoundingClientRect as () => DOMRect) = () => ({ top: 50, bottom: 650, left: 0, right: 0, width: 0, height: 0, x: 0, y: 0, toJSON: () => ({}) }) as unknown as DOMRect;
    vvListeners.filter((l) => l.type === 'resize')[0].cb();
    // pending rAF 已入队（未 flush）
    // blur 先于 rAF flush：移除 listener + 清 maxHeight
    blurTextarea();
    fakeTextarea.dispatchEvent(new Event('blur'));
    expect((wrap.style as { maxHeight?: string }).maxHeight).toBe('');
    // flush rAF 后不得恢复 maxHeight（detachVisualViewportListener 取消了 pending rAF）
    flushRaf();
    expect((wrap.style as { maxHeight?: string }).maxHeight).toBe('');
    session.dispose();
  });

  it('locked+focused 不注册 visualViewport listener（负向测试）', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    // 锁定 + 聚焦 → shouldListen=false（!locked 条件不满足）
    lockControllerMock.lock(); // 置 isLocked=true
    focusTextarea();
    fakeTextarea.dispatchEvent(new Event('focus'));
    expect(vvListeners.filter((l) => l.type === 'resize')).toHaveLength(0);
    expect(vvListeners.filter((l) => l.type === 'scroll')).toHaveLength(0);
    session.dispose();
  });
});

describe('TermSession adapter → blur-DEL 组合（lock 先门禁后 blur）', () => {
  it('调真实 session.lock()（协调器 lock→blur 顺序）→ term.blur 同步触发 onData DEL 时 lock 已先生效、WS 未发送 DEL', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const onState = vi.fn();
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', onState);
    // stub WebSocket 捕获 send（带 OPEN 常量，证明 wsOpen=true 下仍因 locked 拒绝）
    const savedWs = (globalThis as { WebSocket: unknown }).WebSocket;
    let wsInstance: { readyState: number; binaryType: string; send: ReturnType<typeof vi.fn>; close: ReturnType<typeof vi.fn>; onmessage?: (ev: MessageEvent) => void } | null = null;
    const WsCtor = vi.fn(function (this: unknown) {
      wsInstance = { readyState: 1, binaryType: 'arraybuffer', send: vi.fn(), close: vi.fn() };
      return wsInstance as object;
    }) as unknown as typeof WebSocket;
    (WsCtor as unknown as { OPEN: number }).OPEN = 1;
    (globalThis as { WebSocket: unknown }).WebSocket = WsCtor;
    try {
      session.connect();
      wsInstance!.onmessage!({ data: JSON.stringify({ type: 'auth_ok' }) } as MessageEvent);
      // 捕获 onData callback（构造时 term.onData 注册）
      const onDataCallback = termInstance.onData.mock.calls[0][0] as (d: string) => void;
      // 令 mock term.blur() 同步触发 onData DEL（模拟真实 blur 清空 textarea 触发 pending deferred-diff）
      termInstance.blur.mockImplementationOnce(() => {
        onDataCallback('\x7f'); // DEL
      });
      // 调真实 session.lock()：协调器执行 lock→blur 顺序（lock 先置门禁、blur 后触发 DEL）
      session.lock();
      // lock 先于 blur：lockController.lock 已调（门禁生效）+ term.blur 已调
      expect(lockControllerMock.lock).toHaveBeenCalledTimes(1);
      expect(termInstance.blur).toHaveBeenCalledTimes(1);
      // 锁定态门禁拒绝（authed=true + wsOpen=true 但 locked && !syntheticInFlight）→ DEL 不发送。
      // 注：auth_ok 后 fitNow 会发 resize JSON 帧（合法），需区分——DEL 经 sendInput 走 encoder.encode 为 Uint8Array。
      const sentArgs = wsInstance!.send.mock.calls.map((c: unknown[]) => c[0]);
      const hasDel = sentArgs.some((a) => a instanceof Uint8Array && a.length === 1 && a[0] === 0x7f);
      expect(hasDel).toBe(false);
      session.dispose();
    } finally {
      (globalThis as { WebSocket: unknown }).WebSocket = savedWs;
    }
  });

  it('反证：删除 lock-before-blur 顺序（blur 先于 lock）→ DEL 在门禁生效前泄漏 → 测试失败', async () => {
    // 此测试锁定正确顺序：若 blur 先于 lock 执行，DEL 会在门禁生效前经 onData 发送。
    // 通过令 term.blur 在 lockController.lock 之前触发 DEL，验证正常实现（lock 先）不会泄漏。
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    const savedWs = (globalThis as { WebSocket: unknown }).WebSocket;
    let wsInstance: { readyState: number; binaryType: string; send: ReturnType<typeof vi.fn>; close: ReturnType<typeof vi.fn>; onmessage?: (ev: MessageEvent) => void } | null = null;
    const WsCtor = vi.fn(function (this: unknown) {
      wsInstance = { readyState: 1, binaryType: 'arraybuffer', send: vi.fn(), close: vi.fn() };
      return wsInstance as object;
    }) as unknown as typeof WebSocket;
    (WsCtor as unknown as { OPEN: number }).OPEN = 1;
    (globalThis as { WebSocket: unknown }).WebSocket = WsCtor;
    try {
      session.connect();
      wsInstance!.onmessage!({ data: JSON.stringify({ type: 'auth_ok' }) } as MessageEvent);
      const onDataCallback = termInstance.onData.mock.calls[0][0] as (d: string) => void;
      // 模拟错误实现：blur 先于 lock 触发 DEL（在 lockController.lock 之前调）
      // 正确实现 lock() = deps.lock() 先、deps.blur() 后——所以 blur mock 触发 DEL 时 lock 已生效。
      // 这里验证正确实现下 DEL 不泄漏：blur mock 触发 DEL，此时 lockControllerMock.isLocked 应为 true。
      termInstance.blur.mockImplementationOnce(() => {
        // blur 触发 DEL——此时若 lock 先执行，isLocked 已 true → DEL 被门禁拒绝
        const isLockedNow = lockControllerMock.isLocked();
        // lock 已先于 blur 执行 → isLockedNow 应为 true
        expect(isLockedNow).toBe(true);
        onDataCallback('\x7f');
      });
      session.lock();
      const sentArgs = wsInstance!.send.mock.calls.map((c: unknown[]) => c[0]);
      const hasDel = sentArgs.some((a) => a instanceof Uint8Array && a.length === 1 && a[0] === 0x7f);
      expect(hasDel).toBe(false);
      session.dispose();
    } finally {
      (globalThis as { WebSocket: unknown }).WebSocket = savedWs;
    }
  });
});

/* ============================ OSC 52 clipboard 接线 ============================ */

describe('OSC 52 clipboard 接线', () => {
  it('构造时注册 osc 52 handler；handler 同步返回 true 并把合法 payload 转发为明文', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const forwarded: string[] = [];
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {}, (t) => forwarded.push(t));

    expect(termInstance.parser.registerOscHandler).toHaveBeenCalledWith(52, expect.any(Function));
    // vi.fn 无参签名，元组取值需经 unknown 断言（handler 实际为 (data) => boolean）
    const handler = (termInstance.parser.registerOscHandler.mock.calls as unknown as [number, (data: string) => boolean][])[0][1];

    // 合法 c; payload → 转发解码明文，handler 同步返回 true（不返回 Promise）
    expect(handler('c;SGVsbG8=')).toBe(true);
    expect(forwarded).toEqual(['Hello']);
    session.dispose();
  });

  it('读请求 c;? / 非 c / 非法 payload：消费但不转发、绝不回写终端', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const forwarded: string[] = [];
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {}, (t) => forwarded.push(t));
    const handler = (termInstance.parser.registerOscHandler.mock.calls as unknown as [number, (data: string) => boolean][])[0][1];

    expect(handler('c;?')).toBe(true);
    expect(handler('p;SGVsbG8=')).toBe(true);
    expect(handler('c;!!!!')).toBe(true);
    expect(handler('c;')).toBe(true);
    expect(forwarded).toEqual([]);
    // 防外泄关键：读请求不得触发任何 term.input / 数据回写
    expect(termInstance.input).not.toHaveBeenCalled();
    session.dispose();
  });

  it('dispose 时注销 osc 52 handler', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    const disposable = termInstance.parser.registerOscHandler.mock.results[0].value as {
      dispose: ReturnType<typeof vi.fn>;
    };
    expect(disposable.dispose).not.toHaveBeenCalled();
    session.dispose();
    expect(disposable.dispose).toHaveBeenCalledTimes(1);
  });
});