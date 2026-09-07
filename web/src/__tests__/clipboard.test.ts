// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  CLIPBOARD_POLICY_KEY,
  DEDUPE_MAX_ENTRIES,
  DEDUPE_MS,
  OSC52_MAX_BASE64_CHARS,
  OSC52_MAX_DECODED_BYTES,
  WRITE_MAX_PER_WINDOW,
  WRITE_WINDOW_MS,
  createClipboardController,
  loadClipboardPolicy,
  parseOsc52Payload,
  saveClipboardPolicy,
  type ClipboardPolicy,
  type WriteOutcome,
} from '../terminal/clipboard';

function utf8B64(text: string): string {
  return b64OfBytes(new TextEncoder().encode(text));
}

/** 大 buffer 分块转 base64（避免 String.fromCharCode 一次展开爆栈）。 */
function b64OfBytes(bytes: Uint8Array): string {
  let bin = '';
  const chunk = 0x8000;
  for (let i = 0; i < bytes.length; i += chunk) {
    bin += String.fromCharCode(...bytes.subarray(i, i + chunk));
  }
  return btoa(bin);
}

function oscC(text: string): string {
  return `c;${utf8B64(text)}`;
}

const store = new Map<string, string>();
let savedLocalStorage: Storage | undefined;

beforeEach(() => {
  store.clear();
  savedLocalStorage = globalThis.localStorage;
  (globalThis as { localStorage: Storage }).localStorage = {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => {
      store.set(k, v);
    },
    removeItem: (k: string) => {
      store.delete(k);
    },
    clear: () => store.clear(),
    key: (i: number) => [...store.keys()][i] ?? null,
    get length() {
      return store.size;
    },
  };
});

afterEach(() => {
  if (savedLocalStorage) (globalThis as { localStorage: Storage }).localStorage = savedLocalStorage;
  vi.restoreAllMocks();
});

describe('parseOsc52Payload', () => {
  it('合法 c; payload 往返（含中文 / emoji / 多行）', () => {
    const text = 'hello\n中文🌈\r\nline2';
    expect(parseOsc52Payload(oscC(text))).toBe(text);
  });

  it('读请求 c;? 忽略', () => {
    expect(parseOsc52Payload('c;?')).toBeNull();
  });

  it('非 c selection 忽略', () => {
    expect(parseOsc52Payload(`p;${utf8B64('x')}`)).toBeNull();
    expect(parseOsc52Payload(`s;${utf8B64('x')}`)).toBeNull();
  });

  it('非法 base64 拒绝', () => {
    expect(parseOsc52Payload('c;!!!!')).toBeNull();
    expect(parseOsc52Payload('c;abc')).toBeNull();
  });

  it('非法 UTF-8 拒绝', () => {
    expect(parseOsc52Payload(`c;${btoa(String.fromCharCode(0xff))}`)).toBeNull();
  });

  it('超长 encoded 先拒绝，不解码（atob 不被调用）', () => {
    const atobSpy = vi.spyOn(globalThis, 'atob');
    expect(parseOsc52Payload(`c;${'A'.repeat(OSC52_MAX_BASE64_CHARS + 4)}`)).toBeNull();
    expect(atobSpy).not.toHaveBeenCalled();
  });

  it('恰好 1 MiB 解码结果接受', () => {
    const text = 'a'.repeat(OSC52_MAX_DECODED_BYTES);
    expect(parseOsc52Payload(oscC(text))).toBe(text);
  });

  it('encoded 长度等于上限但解码出 1MiB+1 / +2 字节 → 解码后二次检查拒绝', () => {
    for (const n of [OSC52_MAX_DECODED_BYTES + 1, OSC52_MAX_DECODED_BYTES + 2]) {
      const encoded = b64OfBytes(new Uint8Array(n).fill(0x61));
      expect(encoded.length).toBe(OSC52_MAX_BASE64_CHARS); // 与恰好 1MiB 同长（padding 差异）
      expect(parseOsc52Payload(`c;${encoded}`)).toBeNull();
    }
  });

  it('空 payload / 空内容拒绝', () => {
    expect(parseOsc52Payload('')).toBeNull();
    expect(parseOsc52Payload('c')).toBeNull();
    expect(parseOsc52Payload('c;')).toBeNull();
    expect(parseOsc52Payload(`c;${utf8B64('')}`)).toBeNull();
  });

  it('NUL 字节拒绝；换行与普通 Unicode 保留', () => {
    expect(parseOsc52Payload(oscC('a\0b'))).toBeNull();
    expect(parseOsc52Payload(oscC('a\nb\t$`'))).toBe('a\nb\t$`');
  });
});

describe('clipboard policy', () => {
  it('默认 ask；非法值回退 ask', () => {
    expect(loadClipboardPolicy()).toBe('ask');
    store.set(CLIPBOARD_POLICY_KEY, 'maybe');
    expect(loadClipboardPolicy()).toBe('ask');
  });

  it('ask → auto 持久化后走 auto', () => {
    let now = 0;
    const ctl = createClipboardController({ now: () => now });
    expect(ctl.onValidatedWrite('hello')).toBe('ask');
    saveClipboardPolicy('auto');
    expect(loadClipboardPolicy()).toBe('auto');
    now += 2000;
    expect(ctl.onValidatedWrite('hello')).toBe('auto');
    expect(store.get(CLIPBOARD_POLICY_KEY)).toBe('auto');
  });

  it('off 一律 drop', () => {
    saveClipboardPolicy('off');
    const ctl = createClipboardController();
    expect(ctl.onValidatedWrite('hello')).toBe('drop');
    expect(ctl.onValidatedWrite('other')).toBe('drop');
  });
});

describe('有界近期内容去重', () => {
  it('交替内容不绕过：A→B→A 第三次 A 在窗口内被丢弃', () => {
    let now = 0;
    saveClipboardPolicy('auto');
    const ctl = createClipboardController({ now: () => now });
    expect(ctl.onValidatedWrite('A')).toBe('auto');
    expect(ctl.onValidatedWrite('B')).toBe('auto');
    expect(ctl.onValidatedWrite('A')).toBe('drop');
  });

  it(`集合有界（${DEDUPE_MAX_ENTRIES} 条）：第 9 个内容淘汰最旧，被淘汰者可再次通过`, () => {
    let now = 0;
    saveClipboardPolicy('auto');
    const ctl = createClipboardController({ now: () => now });
    for (let i = 0; i < DEDUPE_MAX_ENTRIES; i++) ctl.onValidatedWrite(`t${i}`);
    expect(ctl.onValidatedWrite('t8')).toBe('auto'); // 淘汰 t0
    expect(ctl.onValidatedWrite('t0')).toBe('auto'); // 已被淘汰 → 放行
    expect(ctl.onValidatedWrite('t7')).toBe('drop'); // 仍在集合内
  });

  it(`窗口（${DEDUPE_MS}ms）过期后相同内容放行`, () => {
    let now = 0;
    saveClipboardPolicy('auto');
    const ctl = createClipboardController({ now: () => now });
    expect(ctl.onValidatedWrite('same')).toBe('auto');
    now += DEDUPE_MS - 1;
    expect(ctl.onValidatedWrite('same')).toBe('drop');
    now += 1;
    expect(ctl.onValidatedWrite('same')).toBe('auto');
  });

  it('裁剪定时器按最早过期重排：A@0、B@900 均最终被裁剪（无新插入）', () => {
    saveClipboardPolicy('auto');
    let now = 0;
    const timers: { fn: () => void; delayMs: number; cancelled: boolean }[] = [];
    const schedule = (fn: () => void, delayMs: number) => {
      const handle = { fn, delayMs, cancelled: false, cancel() { this.cancelled = true; } };
      timers.push(handle);
      return handle;
    };
    const ctl = createClipboardController({ now: () => now, schedule });
    expect(ctl.onValidatedWrite('A')).toBe('auto');
    now = 900;
    expect(ctl.onValidatedWrite('B')).toBe('auto');
    expect(timers).toHaveLength(1); // 复用同一裁剪定时器

    now = 1000;
    timers[0].fn(); // 裁掉 A，保留 B
    expect(timers).toHaveLength(2);
    expect(timers[1].delayMs).toBe(900); // 按最早剩余条目（B@900）重排

    now = 1900;
    timers[1].fn(); // 裁掉 B
    expect(ctl.onValidatedWrite('A')).toBe('auto'); // A 早已过期
    expect(ctl.onValidatedWrite('B')).toBe('auto'); // B 也已过期
  });
});

describe('写入队列（串行 / latest-wins / 限速）', () => {
  interface PendingWrite {
    text: string;
    resolve: () => void;
  }

  /** 可控 write：记录启动顺序，手动放行完成。 */
  function gatedWrite() {
    const started: string[] = [];
    const pending: PendingWrite[] = [];
    const write = (text: string): Promise<void> =>
      new Promise((resolve) => {
        started.push(text);
        pending.push({ text, resolve });
      });
    return { started, pending, write };
  }

  it('同时只有一个 in-flight；排队期间旧内容被新内容取代（latest-wins）', async () => {
    saveClipboardPolicy('auto');
    let now = 0;
    const gate = gatedWrite();
    const ctl = createClipboardController({ now: () => now, write: gate.write });

    const pa = ctl.requestWrite('A');
    const pb = ctl.requestWrite('B');
    const pc = ctl.requestWrite('C');

    expect(gate.started).toEqual(['A']); // 仅 A in-flight，B/C 排队
    expect(await pb).toBe('dropped'); // B 被 C 取代，立即按丢弃结算

    gate.pending[0].resolve(); // A 完成
    expect(await pa).toBe('written');
    expect(gate.started).toEqual(['A', 'C']); // B 被跳过，只补写最新 C
    gate.pending[1].resolve();
    expect(await pc).toBe('written');
  });

  it(`限速：${WRITE_WINDOW_MS}ms 窗口内最多 ${WRITE_MAX_PER_WINDOW} 次写入，超出 dropped；窗口滑出后恢复`, async () => {
    saveClipboardPolicy('auto');
    let now = 0;
    const ctl = createClipboardController({ now: () => now, write: () => Promise.resolve() });
    const outcomes: WriteOutcome[] = [];
    for (let i = 0; i < WRITE_MAX_PER_WINDOW + 2; i++) {
      outcomes.push(await ctl.requestWrite(`w${i}`));
    }
    expect(outcomes).toEqual([
      ...Array<string>(WRITE_MAX_PER_WINDOW).fill('written'),
      'dropped',
      'dropped',
    ]);
    now += WRITE_WINDOW_MS;
    expect(await ctl.requestWrite('next')).toBe('written');
  });

  it('写入被拒绝 → failed（调用方回退 popover）', async () => {
    saveClipboardPolicy('auto');
    const ctl = createClipboardController({
      now: () => 0,
      write: () => Promise.reject(new Error('denied')),
    });
    expect(await ctl.requestWrite('x')).toBe('failed');
  });

  it('未注入 write 时 dropped（不抛错）', async () => {
    const ctl = createClipboardController({ now: () => 0 });
    expect(await ctl.requestWrite('x')).toBe('dropped');
  });

  it('成功 toast 节流约 1s', () => {
    let now = 0;
    const ctl = createClipboardController({ now: () => now });
    expect(ctl.takeToastSlot()).toBe(true);
    now += 999;
    expect(ctl.takeToastSlot()).toBe(false);
    now += 1;
    expect(ctl.takeToastSlot()).toBe(true);
  });

  it('write 同步抛错 → 按 failed 结算且队列恢复（无 unhandled rejection）', async () => {
    saveClipboardPolicy('auto');
    let calls = 0;
    const ctl = createClipboardController({
      now: () => 0,
      write: () => {
        calls += 1;
        if (calls === 1) throw new Error('sync boom');
        return Promise.resolve();
      },
    });
    expect(await ctl.requestWrite('a')).toBe('failed');
    expect(await ctl.requestWrite('b')).toBe('written');
  });

  it('in-flight 写入超时 → 按 failed 结算、队列继续；写完后超时定时器被取消', async () => {
    saveClipboardPolicy('auto');
    const timers: { fn: () => void; delayMs: number; cancelled: boolean }[] = [];
    const schedule = (fn: () => void, delayMs: number) => {
      const handle = { fn, delayMs, cancelled: false, cancel() { this.cancelled = true; } };
      timers.push(handle);
      return handle;
    };
    let calls = 0;
    const ctl = createClipboardController({
      now: () => 0,
      schedule,
      writeTimeoutMs: 5000,
      write: () => {
        calls += 1;
        return calls === 1 ? new Promise<void>(() => {}) : Promise.resolve();
      },
    });
    const pa = ctl.requestWrite('stuck');
    expect(timers[0].delayMs).toBe(5000);
    timers[0].fn(); // 超时触发
    expect(await pa).toBe('failed');
    expect(await ctl.requestWrite('next')).toBe('written'); // 队列恢复
  });

  it('排队项启动前复查策略：off 后排队项不再启动', async () => {
    saveClipboardPolicy('auto');
    let policy: ClipboardPolicy = 'auto';
    const gate = gatedWrite();
    const ctl = createClipboardController({ now: () => 0, write: gate.write, getPolicy: () => policy });

    const pa = ctl.requestWrite('A');
    const pb = ctl.requestWrite('B');
    expect(gate.started).toEqual(['A']); // A 已启动（启动时仍是 auto）

    policy = 'off';
    gate.pending[0].resolve();
    expect(await pa).toBe('written');
    expect(await pb).toBe('dropped'); // 复查 off → B 不启动
    expect(gate.started).toEqual(['A']);
  });

  it('auto→ask 撤销授权：排队项不再以 auto 身份启动（按 dropped 结算）', async () => {
    saveClipboardPolicy('auto');
    let policy: ClipboardPolicy = 'auto';
    const gate = gatedWrite();
    const ctl = createClipboardController({ now: () => 0, write: gate.write, getPolicy: () => policy });

    const pa = ctl.requestWrite('A');
    const pb = ctl.requestWrite('B');
    expect(gate.started).toEqual(['A']);

    policy = 'ask'; // 撤销 auto 授权（未到 off）
    gate.pending[0].resolve();
    expect(await pa).toBe('written');
    expect(await pb).toBe('dropped'); // 启动条件要求 === 'auto'
    expect(gate.started).toEqual(['A']); // B 从未启动
  });

  it('超时后旧写入迟到成功 → 不得结算/取消当前 batch（batch 身份校验）', async () => {
    saveClipboardPolicy('auto');
    const timers: { fn: () => void; delayMs: number; cancelled: boolean }[] = [];
    const schedule = (fn: () => void, delayMs: number) => {
      const handle = { fn, delayMs, cancelled: false, cancel() { this.cancelled = true; } };
      timers.push(handle);
      return handle;
    };
    const resolvers: (() => void)[] = [];
    const ctl = createClipboardController({
      now: () => 0,
      schedule,
      writeTimeoutMs: 5000,
      write: () => new Promise<void>((resolve) => resolvers.push(resolve)),
    });

    const pa = ctl.requestWrite('A');
    timers[0].fn(); // A 超时 → failed
    expect(await pa).toBe('failed');

    const pb = ctl.requestWrite('B'); // B 启动（pending）
    resolvers[0](); // A 迟到成功
    await Promise.resolve();

    expect(timers[1].cancelled).toBe(false); // B 的超时未被 A 的迟到回调取消
    expect(await Promise.race([pb, Promise.resolve('pending')])).toBe('pending'); // B 未被幻影结算为 written
    resolvers[1]();
    expect(await pb).toBe('written'); // B 仍走自己的路径结算
  });

  it('cancelPending：排队项 dropped；in-flight 等待方立即 dropped，真实结果不再二次结算，队列照常复位', async () => {
    saveClipboardPolicy('auto');
    const gate = gatedWrite();
    const ctl = createClipboardController({ now: () => 0, write: gate.write });

    const pa = ctl.requestWrite('A');
    const pb = ctl.requestWrite('B');
    expect(gate.started).toEqual(['A']);

    ctl.cancelPending();
    expect(await pa).toBe('dropped');
    expect(await pb).toBe('dropped');

    const pc = ctl.requestWrite('C'); // A 的 batch 尚未清理 → C 排队
    gate.pending[0].resolve(); // A 真实完成（迟到）
    await Promise.resolve(); // 让 finishActive 跑完 → C 启动
    expect(gate.started).toEqual(['A', 'C']);
    gate.pending[1].resolve();
    expect(await pc).toBe('written'); // 队列可用且无重复结算副作用
  });

  it('cancelPending 清空去重集合（off 立即遗忘近期明文）', () => {
    saveClipboardPolicy('auto');
    let now = 0;
    const ctl = createClipboardController({ now: () => now });
    expect(ctl.onValidatedWrite('x')).toBe('auto');
    expect(ctl.onValidatedWrite('x')).toBe('drop');
    ctl.cancelPending();
    expect(ctl.onValidatedWrite('x')).toBe('auto');
  });

  it('去重集合时间驱动裁剪：不依赖下一次插入', () => {
    saveClipboardPolicy('auto');
    let now = 0;
    const timers: { fn: () => void; cancelled: boolean }[] = [];
    const schedule = (fn: () => void) => {
      const handle = { fn, cancelled: false, cancel() { this.cancelled = true; } };
      timers.push(handle);
      return handle;
    };
    const ctl = createClipboardController({ now: () => now, schedule });
    expect(ctl.onValidatedWrite('stale')).toBe('auto');
    now += DEDUPE_MS;
    timers[0].fn(); // 定时裁剪触发（期间无任何新插入）
    expect(ctl.onValidatedWrite('stale')).toBe('auto');
  });

  it('dispose：队列/等待方清空、裁剪定时器取消、之后所有入口全量 no-op', async () => {
    saveClipboardPolicy('auto');
    const timers: { fn: () => void; delayMs: number; cancelled: boolean }[] = [];
    const schedule = (fn: () => void, delayMs: number) => {
      const handle = { fn, delayMs, cancelled: false, cancel() { this.cancelled = true; } };
      timers.push(handle);
      return handle;
    };
    const gate = gatedWrite();
    const ctl = createClipboardController({ now: () => 0, write: gate.write, schedule });

    ctl.onValidatedWrite('seed'); // 排入裁剪定时器
    expect(timers).toHaveLength(1);
    const pa = ctl.requestWrite('A');
    ctl.dispose();
    expect(await pa).toBe('dropped');
    expect(timers[0].cancelled).toBe(true); // 裁剪定时器已取消
    expect(timers[1].cancelled).toBe(true); // in-flight 超时定时器已取消
    expect(ctl.onValidatedWrite('fresh')).toBe('drop'); // disposed → 全量 no-op
    expect(timers).toHaveLength(2); // 未新排任何定时器
    expect(await ctl.requestWrite('B')).toBe('dropped'); // disposed → 不再启动
    expect(gate.started).toEqual(['A']);
  });
});
