import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

/**
 * 1013 恢复期终端会话测试（single-process D8 / G4-5⑥）：
 * - 1013 → recovering 状态（停止指数退避），3s 定时探测重连且保持「进程启动中」展示；
 * - 任务状态回 active（外部 connect，等价 SSE active 翻转驱动）→ 正常 connecting；
 * - dispose / 其他关闭码 → 探测 timer 取消（不再新建 WS）。
 *
 * 策略：与 session-adapter.test.ts 相同的 vi.mock 全家桶（xterm/addon/preferences/
 * lock/gestures/ime/coordination/api）+ stub WebSocket 构造记录 + vi.useFakeTimers
 * 控制 3s 探测时钟。末元素取值用索引（与 tsconfig 主 lib ES2020 兼容）。
 */

vi.mock('../terminal/session-coordination', () => ({
  createLockOrchestrator: vi.fn((deps: unknown) => {
    const d = deps as { lock(): void; blur(): void; unlockSilently(): void; focus(): void; attachGestures(): void; detachGestures(): void };
    return {
      onAuthOk: (_coarse: boolean, onAuthed: () => void) => onAuthed(),
      onPointerChange: () => {},
      lock: () => { d.lock(); d.blur(); },
      unlock: () => { d.unlockSilently(); d.focus(); },
      dispose: vi.fn(),
    };
  }),
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
vi.mock('../terminal/touch-gestures', () => ({ attachTouchGestures: vi.fn(() => ({ rebind: vi.fn(), dispose: vi.fn() })) }));

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
  element: null,
  textarea: undefined,
  options: {} as Record<string, unknown>,
};
vi.mock('@xterm/xterm', () => ({ Terminal: vi.fn(() => termInstance) }));
vi.mock('@xterm/addon-fit', () => ({ FitAddon: vi.fn(() => ({ fit: vi.fn() })) }));
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
vi.mock('../terminal/ime-compensator', () => ({
  createImeCompensator: vi.fn(() => ({
    handleKeyDown: vi.fn(), handleKeyUp: vi.fn(), handleCompositionStart: vi.fn(),
    handleCompositionEnd: vi.fn(), handleInput: vi.fn(), observeNative: vi.fn(), dispose: vi.fn(),
  })),
}));

// ---------- fake WebSocket ----------
interface FakeWS {
  readyState: number;
  binaryType: string;
  send: ReturnType<typeof vi.fn>;
  close: ReturnType<typeof vi.fn>;
  onopen?: () => void;
  onmessage?: (ev: MessageEvent) => void;
  onclose?: (ev: CloseEvent) => void;
  onerror?: () => void;
}
let wsInstances: FakeWS[];
let savedWebSocket: typeof WebSocket | undefined;

function fakeHost(): HTMLElement {
  return { clientWidth: 800, clientHeight: 600 } as unknown as HTMLElement;
}
function fakeWrap(): HTMLElement {
  return { style: {} } as unknown as HTMLElement;
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.clearAllMocks();
  wsInstances = [];
  savedWebSocket = (globalThis as { WebSocket?: typeof WebSocket }).WebSocket;
  (globalThis as { WebSocket: unknown }).WebSocket = vi.fn(function (this: unknown) {
    const ws: FakeWS = {
      readyState: 1,
      binaryType: 'arraybuffer',
      send: vi.fn(),
      close: vi.fn(),
    };
    wsInstances.push(ws);
    return ws as object;
  }) as unknown as typeof WebSocket;
  vi.stubGlobal('matchMedia', vi.fn(() => ({
    matches: false,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
  })));
  vi.stubGlobal('ResizeObserver', vi.fn(() => ({ observe: vi.fn(), disconnect: vi.fn() })));
  vi.stubGlobal('requestAnimationFrame', vi.fn((cb: FrameRequestCallback) => { cb(0); return 0; }));
  vi.stubGlobal('cancelAnimationFrame', vi.fn());
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  if (savedWebSocket) (globalThis as { WebSocket: unknown }).WebSocket = savedWebSocket;
});

async function newSession(onState: (s: string) => void) {
  const { TermSession } = await import('../terminal/session');
  return new TermSession(fakeHost(), fakeWrap(), '/ws/x', onState as never);
}

function lastState(states: string[]): string {
  return states[states.length - 1];
}

function serverClose(ws: FakeWS, code: number) {
  ws.readyState = 3; // CLOSED
  ws.onclose?.({ code, reason: '' } as CloseEvent);
}

describe('TermSession 1013 恢复期语义（D8 / G4-5⑥）', () => {
  it('1013 → recovering；3s 定时探测重连且状态保持「进程启动中」展示', async () => {
    const states: string[] = [];
    const session = await newSession((s) => states.push(s));
    session.connect();
    expect(wsInstances).toHaveLength(1);

    serverClose(wsInstances[0], 1013);
    expect(lastState(states)).toBe('recovering');
    vi.advanceTimersByTime(2999);
    expect(wsInstances).toHaveLength(1);
    vi.advanceTimersByTime(1);
    expect(wsInstances).toHaveLength(2);
    expect(lastState(states)).toBe('recovering');
    serverClose(wsInstances[1], 1013);
    vi.advanceTimersByTime(3000);
    expect(wsInstances).toHaveLength(3);
    session.dispose();
  });

  it('恢复期外部 connect（等价任务状态回 active 驱动）→ 正常 connecting 并清探测 timer', async () => {
    const states: string[] = [];
    const session = await newSession((s) => states.push(s));
    session.connect();
    serverClose(wsInstances[0], 1013);
    expect(lastState(states)).toBe('recovering');

    session.connect();
    expect(lastState(states)).toBe('connecting');
    expect(wsInstances).toHaveLength(2);
    vi.advanceTimersByTime(3000);
    expect(wsInstances).toHaveLength(2);
    session.dispose();
  });

  it('1013 后 dispose → 探测 timer 取消，不再新建 WS', async () => {
    const session = await newSession(() => {});
    session.connect();
    serverClose(wsInstances[0], 1013);
    session.dispose();
    vi.advanceTimersByTime(10_000);
    expect(wsInstances).toHaveLength(1);
  });

  it('4010 仍走 suspended（1013 不得改变既有非重试关闭码语义）', async () => {
    const states: string[] = [];
    const session = await newSession((s) => states.push(s));
    session.connect();
    serverClose(wsInstances[0], 4010);
    expect(lastState(states)).toBe('suspended');
    vi.advanceTimersByTime(3000);
    expect(wsInstances).toHaveLength(1);
    session.dispose();
  });
});

describe('TermSession 陈旧连接回调竞态（G4-8 socket identity guard）', () => {
  it('旧 socket 的延迟 onclose 不得清空新连接/改写状态/触发重连', async () => {
    const states: string[] = [];
    const session = await newSession((s) => states.push(s));
    session.connect();
    const stale = wsInstances[0];
    expect(stale.onclose).toBeTypeOf('function');

    // 模拟 SSE active 翻转驱动 connect()：closeSocket 关闭旧 socket 后建立新连接。
    session.connect();
    expect(wsInstances).toHaveLength(2);
    expect(lastState(states)).toBe('connecting');

    // 旧 socket 的异步 onclose 此时才到达（client close 竞态）：必须被代际 guard 丢弃。
    serverClose(stale, 1006);
    expect(lastState(states)).toBe('connecting'); // 未被改写为 reconnecting
    // 当前连接未被清空：session 内部 ws 仍指向新实例（通过新实例 onclose 生效验证）。
    vi.advanceTimersByTime(10_000);
    expect(wsInstances).toHaveLength(2); // 陈旧 1006 未触发指数退避重连

    // 当前代连接的 onclose 仍正常分派（1013 → recovering）。
    serverClose(wsInstances[1], 1013);
    expect(lastState(states)).toBe('recovering');
    session.dispose();
  });

  it('旧 socket 的延迟 onopen/onmessage 不得向陈旧连接发送/写终端', async () => {
    const states: string[] = [];
    const session = await newSession((s) => states.push(s));
    session.connect();
    const stale = wsInstances[0];
    session.connect();
    const current = wsInstances[1];

    // 陈旧 onopen 到达：guard 丢弃，不重复发送 auth 帧。
    stale.onopen?.();
    expect(stale.send).not.toHaveBeenCalled();
    // 陈旧 onmessage 到达：guard 丢弃，不写终端。
    stale.onmessage?.({ data: 'x' } as MessageEvent);
    expect(termInstance.write).not.toHaveBeenCalled();
    // 当前代 onopen 正常发送。
    current.onopen?.();
    expect(current.send).toHaveBeenCalledTimes(1);
    session.dispose();
  });
});
