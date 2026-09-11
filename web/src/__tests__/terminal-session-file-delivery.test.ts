import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { DeliverResult, TermConnEvent, TermSession } from '../terminal/session';
import { api } from '../api';
import { createFileDeliveryController, type FileDeliveryController } from '../terminal/file-delivery';

/**
 * TermSession 文件投递接线测试（terminal-file-paste-drop 5.2）：
 * - auth_ok connId 接收与归一（合法 uuid 保留；缺失/空串/非 uuid/非字符串 → null 能力降级）；
 * - sendDeliver 文本帧格式 + 门禁复查（锁定/未认证拦截，syntheticInFlight 固定 false）；
 * - deliver_result 帧解析转发（缺 uploadId/ok 的畸形帧忽略）；
 * - closed 事件、connId 清空与重连换代序列（陈旧连接 auth_ok/deliver_result 被代际守卫丢弃）；
 * - 真实 TermSession × FileDeliveryController 生命周期接线（W5）：上传中切换连接、
 *   锁定发生在上传与 deliver 之间、等待回执期间断线。
 *
 * 策略：与 term-recovering.test.ts 相同的 vi.mock 全家桶 + fake WebSocket。
 * fake 构造器补 static OPEN = 1——session.ts 门禁以 `WebSocket.OPEN` 比较 readyState
 * （term-recovering 未用到门禁故未补；本文件 fileGateOpen 依赖它）。
 */

vi.mock('../terminal/session-coordination', () => ({
  createLockOrchestrator: vi.fn((deps: unknown) => {
    const d = deps as { lock(): void; blur(): void; unlockSilently(): void; focus(): void; attachGestures(): void; detachGestures(): void };
    return {
      onAuthOk: (_lockEnabled: boolean, onAuthed: () => void) => onAuthed(),
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
  parser: { registerOscHandler: vi.fn(() => ({ dispose: vi.fn() })) },
  attachCustomKeyEventHandler: vi.fn(),
};
vi.mock('@xterm/xterm', () => ({ Terminal: vi.fn(() => termInstance) }));
vi.mock('@xterm/addon-fit', () => ({ FitAddon: vi.fn(() => ({ fit: vi.fn() })) }));
vi.mock('@xterm/addon-webgl', () => ({ WebglAddon: vi.fn(() => ({ dispose: vi.fn() })) }));
vi.mock('../terminal/preferences', () => ({
  loadTermPrefs: vi.fn(() => ({})),
  resolveFontFamily: vi.fn(() => 'monospace'),
  resolveFontSize: vi.fn(() => 13),
  loadMobileMode: vi.fn(() => 'auto'),
  loadMobileCaps: vi.fn(() => ({ version: 1, lock: true, gestures: true, keyboardAvoid: true })),
  TERM_PREFS_CHANGED: 'ocdeck-term-prefs-changed',
}));
vi.mock('../api', async () => {
  // actual 展开：file-delivery.ts 依赖真实 ApiError / UPLOAD_TARGET_UNAVAILABLE_MESSAGE；
  // token/wsURL 换成可控 fake（本文件无网络）。
  const actual = await vi.importActual<typeof import('../api')>('../api');
  return {
    ...actual,
    clearToken: vi.fn(),
    getToken: vi.fn(() => 'fake-token'),
    wsURL: vi.fn(() => 'ws://fake/terminal'),
  };
});
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
  sent: string[];
  send: (data: string | ArrayBuffer | Uint8Array) => void;
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
  lockControllerMock.isLocked.mockReturnValue(false); // clearAllMocks 不重置 mockReturnValue
  wsInstances = [];
  savedWebSocket = (globalThis as { WebSocket?: typeof WebSocket }).WebSocket;
  const fakeCtor = vi.fn(function (this: unknown) {
    const ws: FakeWS = {
      readyState: 1,
      binaryType: 'arraybuffer',
      sent: [],
      send(data) {
        ws.sent.push(typeof data === 'string' ? data : '[binary]');
      },
      close: vi.fn(() => {
        ws.readyState = 3;
      }),
    };
    wsInstances.push(ws);
    return ws as object;
  }) as unknown as typeof WebSocket & { OPEN: number };
  fakeCtor.OPEN = 1;
  (globalThis as { WebSocket: unknown }).WebSocket = fakeCtor;
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

async function newSession(): Promise<TermSession> {
  const { TermSession } = await import('../terminal/session');
  return new TermSession(fakeHost(), fakeWrap(), '/ws/x', () => {});
}

function authOk(ws: FakeWS, connId: unknown): void {
  const frame: Record<string, unknown> = { type: 'auth_ok' };
  if (connId !== undefined) frame.connId = connId;
  ws.onmessage?.({ data: JSON.stringify(frame) } as MessageEvent);
}

function connectAndAuth(session: TermSession, connId: unknown): FakeWS {
  session.connect();
  const ws = wsInstances[wsInstances.length - 1];
  ws.onopen?.();
  authOk(ws, connId);
  return ws;
}

const UUID_A = '11111111-1111-4111-8111-111111111111';
const UUID_B = '22222222-2222-4222-8222-222222222222';
const UPLOAD_A = 'a'.repeat(32);

describe('auth_ok connId 接收（terminal-streaming delta）', () => {
  it('合法 uuid connId → getConnId 返回且 onConnEvent 收到 auth_ok 事件', async () => {
    const session = await newSession();
    const events: TermConnEvent[] = [];
    session.onConnEvent((ev) => events.push(ev));
    connectAndAuth(session, UUID_A);
    expect(session.getConnId()).toBe(UUID_A);
    expect(events).toEqual([{ type: 'auth_ok', connId: UUID_A }]);
    session.dispose();
  });

  it('缺失/空串/非 uuid/非字符串 connId → 归一为 null（文件能力降级），事件携带 null', async () => {
    for (const bad of [undefined, '', 'not-a-uuid', 123, null, '11111111-1111-1111-1111-111111111']) {
      const session = await newSession();
      const events: TermConnEvent[] = [];
      session.onConnEvent((ev) => events.push(ev));
      connectAndAuth(session, bad);
      expect(session.getConnId()).toBeNull();
      expect(events[events.length - 1]).toEqual({ type: 'auth_ok', connId: null });
      session.dispose();
    }
  });
});

describe('sendDeliver（deliver 文本帧 + 门禁复查，D6）', () => {
  it('门禁通过 → 发送 {"type":"deliver","uploadId"} 文本帧并返回 true', async () => {
    const session = await newSession();
    const ws = connectAndAuth(session, UUID_A);
    expect(session.fileGateOpen()).toBe(true);
    expect(session.sendDeliver(UPLOAD_A)).toBe(true);
    expect(ws.sent[ws.sent.length - 1]).toBe(JSON.stringify({ type: 'deliver', uploadId: UPLOAD_A }));
    session.dispose();
  });

  it('锁定 → 拦截且不发送；fileGateMessage 提示解锁（syntheticInFlight 固定 false，锁定例外不适用）', async () => {
    const session = await newSession();
    const ws = connectAndAuth(session, UUID_A);
    lockControllerMock.isLocked.mockReturnValue(true);
    expect(session.fileGateOpen()).toBe(false);
    expect(session.sendDeliver(UPLOAD_A)).toBe(false);
    expect(ws.sent.some((f) => f.includes('deliver'))).toBe(false);
    expect(session.fileGateMessage()).toBe('终端已锁定，请先解锁后重试');
    session.dispose();
  });

  it('未认证（auth_ok 前）→ 拦截；fileGateMessage 提示连接', async () => {
    const session = await newSession();
    session.connect();
    const ws = wsInstances[wsInstances.length - 1];
    ws.onopen?.(); // 尚未 auth_ok
    expect(session.fileGateOpen()).toBe(false);
    expect(session.sendDeliver(UPLOAD_A)).toBe(false);
    expect(ws.sent.some((f) => f.includes('deliver'))).toBe(false);
    expect(session.fileGateMessage()).toBe('终端未连接，请连接后重试');
    session.dispose();
  });
});

describe('deliver_result 帧解析转发', () => {
  it('合法帧转发 {uploadId, ok, error?}；缺 uploadId/ok 的畸形帧忽略', async () => {
    const session = await newSession();
    const ws = connectAndAuth(session, UUID_A);
    const results: DeliverResult[] = [];
    session.onDeliverResult((r) => results.push(r));

    ws.onmessage?.({ data: JSON.stringify({ type: 'deliver_result', uploadId: UPLOAD_A, ok: true }) } as MessageEvent);
    expect(results).toEqual([{ uploadId: UPLOAD_A, ok: true }]);

    ws.onmessage?.({ data: JSON.stringify({ type: 'deliver_result', uploadId: 'b'.repeat(32), ok: false, error: 'write_failed' }) } as MessageEvent);
    expect(results[1]).toEqual({ uploadId: 'b'.repeat(32), ok: false, error: 'write_failed' });

    // 畸形帧：缺 ok / 缺 uploadId → 不转发
    ws.onmessage?.({ data: JSON.stringify({ type: 'deliver_result', uploadId: UPLOAD_A }) } as MessageEvent);
    ws.onmessage?.({ data: JSON.stringify({ type: 'deliver_result', ok: true }) } as MessageEvent);
    expect(results).toHaveLength(2);
    session.dispose();
  });
});

describe('closed 事件、connId 清空与重连换代（D5 代次绑定依据）', () => {
  it('disconnect → closed 事件、getConnId 归 null、门禁关闭', async () => {
    const session = await newSession();
    const events: TermConnEvent[] = [];
    session.onConnEvent((ev) => events.push(ev));
    connectAndAuth(session, UUID_A);
    expect(session.getConnId()).toBe(UUID_A);

    session.disconnect();
    expect(events[events.length - 1]).toEqual({ type: 'closed' });
    expect(session.getConnId()).toBeNull();
    expect(session.fileGateOpen()).toBe(false);
    session.dispose();
  });

  it('重连换代：closed 后新连接 auth_ok 新 connId；事件序列完整', async () => {
    const session = await newSession();
    const events: TermConnEvent[] = [];
    session.onConnEvent((ev) => events.push(ev));
    connectAndAuth(session, UUID_A);

    session.connect(); // 换代：closeSocket（closed）→ 新 socket
    const ws2 = wsInstances[wsInstances.length - 1];
    ws2.onopen?.();
    authOk(ws2, UUID_B);

    expect(session.getConnId()).toBe(UUID_B);
    expect(events).toEqual([
      { type: 'auth_ok', connId: UUID_A },
      { type: 'closed' },
      { type: 'auth_ok', connId: UUID_B },
    ]);
    session.dispose();
  });

  it('陈旧连接的 auth_ok 被代际守卫丢弃，不改写 connId / 不产生事件', async () => {
    const session = await newSession();
    const events: TermConnEvent[] = [];
    session.onConnEvent((ev) => events.push(ev));
    connectAndAuth(session, UUID_A);
    const stale = wsInstances[0];

    session.connect(); // 换代后 stale 失效
    authOk(stale, '99999999-9999-4999-8999-999999999999');
    expect(session.getConnId()).toBeNull(); // 未被陈旧帧改写

    const current = wsInstances[wsInstances.length - 1];
    current.onopen?.();
    authOk(current, UUID_B);
    expect(session.getConnId()).toBe(UUID_B);
    expect(events).toEqual([
      { type: 'auth_ok', connId: UUID_A },
      { type: 'closed' },
      { type: 'auth_ok', connId: UUID_B },
    ]);
    session.dispose();
  });

  it('旧 socket 迟到 deliver_result 被代际守卫丢弃，不进入回执订阅（W6：connId 归属由连接代保证）', async () => {
    const session = await newSession();
    const ws1 = connectAndAuth(session, UUID_A);
    const results: DeliverResult[] = [];
    session.onDeliverResult((r) => results.push(r));

    session.connect(); // 换代后 ws1 失效
    const current = wsInstances[wsInstances.length - 1];
    current.onopen?.();
    authOk(current, UUID_B);

    // 旧连接的迟到回执：不得进入当前回执订阅（controller 层由此保证 uploadId 归属判定）
    ws1.onmessage?.({ data: JSON.stringify({ type: 'deliver_result', uploadId: UPLOAD_A, ok: true }) } as MessageEvent);
    ws1.onmessage?.({ data: JSON.stringify({ type: 'deliver_result', uploadId: UPLOAD_A, ok: false, error: 'write_failed' }) } as MessageEvent);
    expect(results).toEqual([]);

    // 当前连接的回执正常转发
    current.onmessage?.({ data: JSON.stringify({ type: 'deliver_result', uploadId: UPLOAD_A, ok: true }) } as MessageEvent);
    expect(results).toEqual([{ uploadId: UPLOAD_A, ok: true }]);
    session.dispose();
  });
});

/* ==================== 自然断线（真实 socket onclose）与文件状态机衔接（R11） ==================== */

describe('自然断线（真实 socket 的 onclose，非主动 disconnect）', () => {
  /** 触发当前代 socket 的自然断线（不经 closeSocket/disconnect）。 */
  function triggerClose(ws: FakeWS, code: number): void {
    ws.onclose?.({ code } as unknown as CloseEvent);
  }

  it('1011（server 内部错误）→ 恰好一次 closed、connId 立即清空、门禁关闭、指数退避重连', async () => {
    const session = await newSession();
    const events: TermConnEvent[] = [];
    session.onConnEvent((ev) => events.push(ev));
    const ws = connectAndAuth(session, UUID_A);
    expect(session.getConnId()).toBe(UUID_A);

    triggerClose(ws, 1011);
    expect(events).toEqual([{ type: 'auth_ok', connId: UUID_A }, { type: 'closed' }]);
    expect(session.getConnId()).toBeNull();
    expect(session.fileGateOpen()).toBe(false);

    await vi.advanceTimersByTimeAsync(1000); // retry=1 → delay = 500 * 2^1
    expect(wsInstances).toHaveLength(2); // 自动重连已发生
    session.dispose();
  });

  it('1006（异常断线，无 close frame）→ 同款收尾：closed + connId 立即清空', async () => {
    const session = await newSession();
    const events: TermConnEvent[] = [];
    session.onConnEvent((ev) => events.push(ev));
    const ws = connectAndAuth(session, UUID_A);

    triggerClose(ws, 1006);
    expect(events).toEqual([{ type: 'auth_ok', connId: UUID_A }, { type: 'closed' }]);
    expect(session.getConnId()).toBeNull();
    expect(session.fileGateOpen()).toBe(false);
    session.dispose();
  });

  it('4009（被替换）→ closed + connId 清空，MUST NOT 自动重连', async () => {
    const session = await newSession();
    const events: TermConnEvent[] = [];
    session.onConnEvent((ev) => events.push(ev));
    const ws = connectAndAuth(session, UUID_A);

    triggerClose(ws, 4009);
    expect(events).toEqual([{ type: 'auth_ok', connId: UUID_A }, { type: 'closed' }]);
    expect(session.getConnId()).toBeNull();
    expect(session.fileGateOpen()).toBe(false);

    await vi.advanceTimersByTimeAsync(10_000);
    expect(wsInstances).toHaveLength(1); // 停止自动重连
    session.dispose();
  });
});

/* ==================== 真实 TermSession × FileDeliveryController 生命周期接线（W5） ==================== */

function makeFile(name: string): File {
  return new File(['x'], name, { type: 'application/octet-stream' });
}

function itemOf(ctl: FileDeliveryController, name: string) {
  const item = ctl.snapshot().items.find((i) => i.name === name);
  if (!item) throw new Error(`item ${name} not found`);
  return item;
}

describe('TermSession × FileDeliveryController 接线（真实会话生命周期，W5）', () => {
  /** 可控上传的 controller（port = 真实 TermSession）。 */
  function newController(session: TermSession) {
    const uploads: {
      taskID: string;
      file: File;
      connId: string;
      resolve: (r: { uploadId: string }) => void;
      reject: (e: unknown) => void;
    }[] = [];
    const ctl: FileDeliveryController = createFileDeliveryController({
      taskID: 'task1',
      port: session,
      upload: (taskID, file, connId) =>
        new Promise((resolve, reject) => {
          uploads.push({ taskID, file, connId, resolve, reject });
        }),
      openFilePicker: vi.fn(),
    });
    return { ctl, uploads };
  }

  const flush = () => vi.advanceTimersByTimeAsync(0);

  it('auth_ok → 捕获 → 上传 → deliver（经真实 sendDeliver）→ 回执 → 已发送', async () => {
    const session = await newSession();
    const { ctl, uploads } = newController(session);
    connectAndAuth(session, UUID_A);

    ctl.handleCapturedFiles([makeFile('a.png')]);
    await flush();
    expect(uploads).toHaveLength(1);
    expect(uploads[0].connId).toBe(UUID_A); // 捕获真实会话的 connId

    uploads[0].resolve({ uploadId: UPLOAD_A });
    await flush();
    const ws = wsInstances[wsInstances.length - 1];
    expect(ws.sent[ws.sent.length - 1]).toBe(JSON.stringify({ type: 'deliver', uploadId: UPLOAD_A }));

    ws.onmessage?.({ data: JSON.stringify({ type: 'deliver_result', uploadId: UPLOAD_A, ok: true }) } as MessageEvent);
    await flush();
    expect(itemOf(ctl, 'a.png').status).toBe('sent');
    expect(ctl.snapshot().paused).toBe(false);
    ctl.dispose();
    session.dispose();
  });

  it('锁定发生在上传与 deliver 之间 → 明确失败、不发送 deliver', async () => {
    const session = await newSession();
    const { ctl, uploads } = newController(session);
    connectAndAuth(session, UUID_A);

    ctl.handleCapturedFiles([makeFile('a.png')]);
    lockControllerMock.isLocked.mockReturnValue(true); // 上传在途期间锁定
    uploads[0].resolve({ uploadId: UPLOAD_A });
    await flush();
    expect(itemOf(ctl, 'a.png').status).toBe('failed');
    expect(itemOf(ctl, 'a.png').detail).toBe('终端已锁定，请先解锁后重试');
    expect(wsInstances[wsInstances.length - 1].sent.some((f) => f.includes('deliver'))).toBe(false);
    ctl.dispose();
    session.dispose();
  });

  it('上传中切换连接（closeSocket 清空 connId）→ 明确失败、不向新连接投递', async () => {
    const session = await newSession();
    const { ctl, uploads } = newController(session);
    connectAndAuth(session, UUID_A);

    ctl.handleCapturedFiles([makeFile('a.png')]);
    await flush(); // uploading
    session.connect(); // 换代：connId → null
    uploads[0].resolve({ uploadId: UPLOAD_A });
    await flush();
    expect(itemOf(ctl, 'a.png').status).toBe('failed');
    expect(itemOf(ctl, 'a.png').detail).toBe('连接已更换，请重试');
    expect(wsInstances[wsInstances.length - 1].sent.some((f) => f.includes('deliver'))).toBe(false);
    ctl.dispose();
    session.dispose();
  });

  it('等待回执期间断线（真实 closed 事件）→ 结果未知 + 队列暂停', async () => {
    const session = await newSession();
    const { ctl, uploads } = newController(session);
    connectAndAuth(session, UUID_A);

    ctl.handleCapturedFiles([makeFile('a.png')]);
    uploads[0].resolve({ uploadId: UPLOAD_A });
    await flush(); // awaiting
    expect(itemOf(ctl, 'a.png').status).toBe('awaiting');

    session.disconnect(); // 等待回执期间断线
    await flush();
    expect(itemOf(ctl, 'a.png').status).toBe('unknown');
    expect(itemOf(ctl, 'a.png').detail).toBe('连接已断开');
    expect(ctl.snapshot().paused).toBe(true);
    ctl.dispose();
    session.dispose();
  });

  it('等待回执期间自然断线（真实 socket onclose 1011）→ 即时结果未知，不等 10s 回执超时（R11）', async () => {
    const session = await newSession();
    const { ctl, uploads } = newController(session);
    const ws = connectAndAuth(session, UUID_A);

    ctl.handleCapturedFiles([makeFile('a.png')]);
    uploads[0].resolve({ uploadId: UPLOAD_A });
    await flush(); // awaiting
    expect(itemOf(ctl, 'a.png').status).toBe('awaiting');

    // 自然断线：不经 disconnect/closeSocket，直接触发当前代 socket 的 onclose
    ws.onclose?.({ code: 1011 } as unknown as CloseEvent);
    await flush(); // 未推进 10s 回执超时
    expect(itemOf(ctl, 'a.png').status).toBe('unknown');
    expect(itemOf(ctl, 'a.png').detail).toBe('连接已断开');
    expect(ctl.snapshot().paused).toBe(true);
    ctl.dispose();
    session.dispose();
  });

  it('真实 api.uploadAttachment：文件读取失效（NotReadableError）→ 明确失败；点击重试进入绑定该项的重选流程（R12）', async () => {
    // 真实 api.uploadAttachment 闭包内的 getToken 读 localStorage（Node 测试环境补 stub）
    vi.stubGlobal('localStorage', {
      getItem: () => 'fake-token',
      setItem: vi.fn(),
      removeItem: vi.fn(),
      clear: vi.fn(),
    });
    // 首次 fetch 拒绝 = FormData 序列化时读取 File 失败；二次返回 201 供重选后的新尝试
    const fetchMock = vi
      .fn<() => Promise<Response>>()
      .mockImplementationOnce(async () => {
        throw new DOMException('The source file could not be read', 'NotReadableError');
      })
      .mockImplementationOnce(async () =>
        ({ ok: true, status: 201, text: async () => JSON.stringify({ uploadId: UPLOAD_A }) }) as unknown as Response,
      );
    vi.stubGlobal('fetch', fetchMock);

    const session = await newSession();
    const openPicker = vi.fn();
    const ctl: FileDeliveryController = createFileDeliveryController({
      taskID: 'task1',
      port: session,
      upload: api.uploadAttachment, // 真实 api 路径（非注入 fake）
      openFilePicker: openPicker,
    });
    connectAndAuth(session, UUID_A);

    ctl.handleCapturedFiles([makeFile('dead.png')]);
    await flush();
    const item = itemOf(ctl, 'dead.png');
    expect(item.status).toBe('failed');
    expect(item.detail).toBe('文件不可读取，请重新选择'); // 不被 api 包装吞成网络错误

    // 点击重试：原 File 已不可用 → 打开绑定该项重试意图的选择器，不再重试读取同一 File
    ctl.retryItem(item.id);
    expect(openPicker).toHaveBeenCalledTimes(1);
    expect(ctl.snapshot().pickerPendingItem).toBe(item.id);
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // 选定新文件 → 该项创建新尝试，经真实上传 + 真实 sendDeliver 走完整链路
    ctl.handlePickerResult([makeFile('fresh.png')]);
    await flush();
    expect(ctl.snapshot().pickerPendingItem).toBeNull();
    expect(fetchMock).toHaveBeenCalledTimes(2);
    const ws = wsInstances[wsInstances.length - 1];
    expect(ws.sent[ws.sent.length - 1]).toBe(JSON.stringify({ type: 'deliver', uploadId: UPLOAD_A }));

    ws.onmessage?.({ data: JSON.stringify({ type: 'deliver_result', uploadId: UPLOAD_A, ok: true }) } as MessageEvent);
    await flush();
    expect(itemOf(ctl, 'fresh.png').status).toBe('sent');
    ctl.dispose();
    session.dispose();
  });
});
