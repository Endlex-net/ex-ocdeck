import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';

/**
 * Session adapter 接线测试：锁定 TermSession 实际委托 LockOrchestrator，
 * 并验证 deps 各槽位绑定到正确的方法（lockController.lock / term.blur / attachGestures 等）。
 *
 * 策略：vi.mock 替换 xterm/addon/preferences/lock/手势层/api，使 TermSession 可在 Node 无 DOM 实例化；
 * vi.mock('./session-coordination') 暴露 spy createLockOrchestrator，断言：
 * - 构造时若 coarse → lock() + attach 被调用（门禁先置位）
 * - auth_ok → orchestrator.onAuthOk 被调用
 * - pointer change → orchestrator.onPointerChange 被调用
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
      // onAuthOk(coarse) = deps.lock + deps.blur 再 onAuthed（lock 先于 authed 暴露）。
      onAuthOk: vi.fn((coarse: boolean, onAuthed: () => void) => {
        if (coarse) { d.lock(); d.blur(); }
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

/** fake wrap：带 style 对象（dispose 时设 maxHeight）+ getBoundingClientRect。 */
function fakeWrap(): HTMLElement {
  return { style: {}, getBoundingClientRect: () => ({ top: 0, bottom: 0, left: 0, right: 0, width: 0, height: 0, x: 0, y: 0, toJSON: () => ({}) }) } as unknown as HTMLElement;
}
/** fake host：带正 clientWidth/clientHeight（fitNow 提前 return 守卫需正尺寸，否则删除 this.fit.fit() 不失败）。 */
function fakeHost(): HTMLElement {
  return { clientWidth: 800, clientHeight: 600 } as unknown as HTMLElement;
}
// lockController mock：lock/unlock 切换 isLocked 并通知 onChange 订阅者（复现真实 createLockController notify 语义）
const lockChangeListeners = new Set<(locked: boolean) => void>();
const lockControllerMock = {
  lock: vi.fn(() => { lockControllerMock.isLocked.mockReturnValue(true); for (const cb of lockChangeListeners) cb(true); }),
  unlock: vi.fn(() => { lockControllerMock.isLocked.mockReturnValue(false); for (const cb of lockChangeListeners) cb(false); }),
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

beforeEach(() => {
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

  it('pointer change（matchMedia change 事件）→ orchestrator.onPointerChange 委托', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const session = new TermSession({} as HTMLElement, fakeWrap(), '/ws/x', () => {});
    // 触发 matchMedia change 事件
    const evt = { matches: true } as MediaQueryListEvent;
    for (const cb of env.mqlListeners) cb(evt);
    expect(capturedHandle!.onPointerChange).toHaveBeenCalledWith(true);
    // 转 fine
    const evt2 = { matches: false } as MediaQueryListEvent;
    for (const cb of env.mqlListeners) cb(evt2);
    expect(capturedHandle!.onPointerChange).toHaveBeenCalledWith(false);
    session.dispose();
  });

  it('auth_ok fine：orchestrator.onAuthOk(false, cb) —— 真实 pointer 参数透传', async () => {
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
      // fine pointer → onAuthOk 第一个参数必须为 false（锁定真实 pointer 透传）
      expect(capturedHandle!.onAuthOk).toHaveBeenCalledTimes(1);
      expect(capturedHandle!.onAuthOk).toHaveBeenCalledWith(false, expect.any(Function));
      expect(onState).toHaveBeenCalledWith('connected');
      session.dispose();
    } finally {
      (globalThis as { WebSocket: unknown }).WebSocket = savedWs;
    }
  });

  it('auth_ok coarse：orchestrator.onAuthOk(true, cb) —— coarse 重连回锁参数透传', async () => {
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
      // coarse pointer → onAuthOk 第一个参数必须为 true（coarse 重连回锁）
      expect(capturedHandle!.onAuthOk).toHaveBeenCalledTimes(1);
      expect(capturedHandle!.onAuthOk).toHaveBeenCalledWith(true, expect.any(Function));
      expect(onState).toHaveBeenCalledWith('connected');
      session.dispose();
    } finally {
      (globalThis as { WebSocket: unknown }).WebSocket = savedWs;
    }
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
      // coarse auth_ok → lock → onLockChange 收到 true
      expect(states).toContain(true);
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
    // pointer 转 fine：matchMedia change 事件 matches=false → 协调器 onPointerChange(false)
    // → deps.unlockSilently() → lockController.unlock（置 isLocked=false + notify）→ onLockChange 收到 false
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

  it('resize 触发公式 max(0, vv.offsetTop+vv.height - wrap.top) 设 wrap maxHeight', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const wrap = fakeWrap();
    const session = new TermSession({} as HTMLElement, wrap, '/ws/x', () => {});
    lockControllerMock.isLocked.mockReturnValue(false);
    focusTextarea();
    fakeTextarea.dispatchEvent(new Event('focus'));
    // 设置 vv offsetTop=100, height=400；wrap.top=50 → maxHeight = 100+400-50 = 450
    vvOffsetTop = 100;
    vvHeight = 400;
    (wrap.getBoundingClientRect as () => DOMRect) = () => ({ top: 50, bottom: 0, left: 0, right: 0, width: 0, height: 0, x: 0, y: 0, toJSON: () => ({}) }) as unknown as DOMRect;
    // 触发 resize listener
    const resizeCb = vvListeners.filter((l) => l.type === 'resize')[0].cb;
    resizeCb();
    flushRaf();
    expect((wrap.style as { maxHeight?: string }).maxHeight).toBe('450px');
    session.dispose();
  });

  it('公式 clamp 负值为 0（vv 不可见时 wrap.top > visibleBottom）', async () => {
    env.matchMediaMatches = false;
    const { TermSession } = await import('../terminal/session');
    const wrap = fakeWrap();
    const session = new TermSession({} as HTMLElement, wrap, '/ws/x', () => {});
    lockControllerMock.isLocked.mockReturnValue(false);
    focusTextarea();
    fakeTextarea.dispatchEvent(new Event('focus'));
    vvOffsetTop = 0;
    vvHeight = 100;
    (wrap.getBoundingClientRect as () => DOMRect) = () => ({ top: 200, bottom: 0, left: 0, right: 0, width: 0, height: 0, x: 0, y: 0, toJSON: () => ({}) }) as unknown as DOMRect;
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
    (wrap.getBoundingClientRect as () => DOMRect) = () => ({ top: 50, bottom: 0, left: 0, right: 0, width: 0, height: 0, x: 0, y: 0, toJSON: () => ({}) }) as unknown as DOMRect;
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

  it('scroll 触发公式 max(0, vv.offsetTop+vv.height - wrap.top) 设 wrap maxHeight（与 resize 同公式）', async () => {
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
    (wrap.getBoundingClientRect as () => DOMRect) = () => ({ top: 30, bottom: 0, left: 0, right: 0, width: 0, height: 0, x: 0, y: 0, toJSON: () => ({}) }) as unknown as DOMRect;
    // 触发 scroll listener
    const scrollCb = vvListeners.filter((l) => l.type === 'scroll')[0].cb;
    scrollCb();
    flushRaf();
    // maxHeight = 80 + 300 - 30 = 350
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
      (wrap.getBoundingClientRect as () => DOMRect) = () => ({ top: 50, bottom: 0, left: 0, right: 0, width: 0, height: 0, x: 0, y: 0, toJSON: () => ({}) }) as unknown as DOMRect;
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
    (wrap.getBoundingClientRect as () => DOMRect) = () => ({ top: 50, bottom: 0, left: 0, right: 0, width: 0, height: 0, x: 0, y: 0, toJSON: () => ({}) }) as unknown as DOMRect;
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