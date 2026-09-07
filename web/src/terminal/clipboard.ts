/**
 * OSC 52 clipboard-write 解析 / 策略 / 写入排队（纯逻辑，不依赖 xterm / DOM）。
 *
 * OSC 52 形如 ESC]52;<selection>;<base64>(BEL|ST)；xterm parser 交给 handler 的是
 * `]52;` 与终止符之间的 payload，即 `<selection>;<base64>`。
 * 只接受 selection=`c` 的写入；`c;?` 为读请求，静默忽略且不得回写终端。
 *
 * 安全边界：
 * - 只持久化策略（ask/auto/off），剪贴板内容仅在内存短暂流转；
 * - 去重用有界近期内容集合（非仅相邻两条）+ 定时裁剪，防远程交替内容绕过、防敏感明文无限期滞留；
 * - auto 写入经单一队列串行（latest-wins 合并）+ 滑动窗口限速 + in-flight 超时，
 *   且启动排队项前复查策略——off 后排队项不得再启动；
 * - 同步抛错的 write 归一为 failed，队列不因异常卡死。
 */

import { TERM_PREFS_CHANGED } from './preferences';

export const OSC52_MAX_DECODED_BYTES = 1024 * 1024; // 1 MiB
/** 1 MiB 的 base64 上限（含 padding）：4 * ceil(1048576 / 3) = 1398104。 */
export const OSC52_MAX_BASE64_CHARS = Math.ceil(OSC52_MAX_DECODED_BYTES / 3) * 4;

export const DEDUPE_MS = 1000;
/** 近期内容集合容量：窗口内第 9 个新内容淘汰最旧一条。 */
export const DEDUPE_MAX_ENTRIES = 8;
/** 写入限速：滑动窗口内最多启动的写入次数。 */
export const WRITE_WINDOW_MS = 1000;
export const WRITE_MAX_PER_WINDOW = 5;
/** in-flight 写入超时：writeText Promise 永不结算时按 failed 放行队列。 */
export const WRITE_TIMEOUT_MS = 10_000;
export const TOAST_THROTTLE_MS = 1000;

export type ClipboardPolicy = 'ask' | 'auto' | 'off';
export const CLIPBOARD_POLICY_KEY = 'ocdeck.terminal.clipboardPolicy';
export const DEFAULT_CLIPBOARD_POLICY: ClipboardPolicy = 'ask';

export type ClipboardAction = 'drop' | 'auto' | 'ask';
/** requestWrite 结果：written=已写入；failed=被浏览器拒绝/超时/同步抛错；dropped=被合并/限速/off/已作废。 */
export type WriteOutcome = 'written' | 'failed' | 'dropped';

/** 合法 clipboard-write 返回明文；其余（读请求、非 c、非法数据）返回 null。 */
export function parseOsc52Payload(payload: string): string | null {
  const sep = payload.indexOf(';');
  if (sep < 0) return null;
  if (payload.slice(0, sep) !== 'c') return null;
  const encoded = payload.slice(sep + 1);
  if (encoded === '?' || encoded.length === 0) return null;
  if (encoded.length > OSC52_MAX_BASE64_CHARS) return null;

  let binary: string;
  try {
    binary = atob(encoded);
  } catch {
    return null;
  }

  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
  if (bytes.length > OSC52_MAX_DECODED_BYTES) return null;

  let text: string;
  try {
    text = new TextDecoder('utf-8', { fatal: true }).decode(bytes);
  } catch {
    return null;
  }
  if (text.length === 0 || text.includes('\0')) return null;
  return text;
}

export function loadClipboardPolicy(): ClipboardPolicy {
  try {
    const raw = localStorage.getItem(CLIPBOARD_POLICY_KEY);
    if (raw === 'ask' || raw === 'auto' || raw === 'off') return raw;
  } catch {
    /* localStorage 不可用时按默认处理 */
  }
  return DEFAULT_CLIPBOARD_POLICY;
}

/** 只持久化策略，永不写入剪贴板内容。失败向上抛出；成功派发变更事件（同页即时生效）。 */
export function saveClipboardPolicy(policy: ClipboardPolicy): void {
  localStorage.setItem(CLIPBOARD_POLICY_KEY, policy);
  window.dispatchEvent(new CustomEvent(TERM_PREFS_CHANGED));
}

export interface ClipboardControllerOptions {
  now?: () => number;
  /** 实际执行写入的函数（auto 路径专用；用户手势内的手动复制不走此队列）。 */
  write?: (text: string) => Promise<void>;
  /** pump 启动排队项前的策略复查（默认读 localStorage）。 */
  getPolicy?: () => ClipboardPolicy;
  /** in-flight 写入超时（默认 WRITE_TIMEOUT_MS）。 */
  writeTimeoutMs?: number;
  /** 定时器注入（超时与去重裁剪共用，测试可控时钟）。 */
  schedule?: (fn: () => void, delayMs: number) => { cancel(): void };
}

export interface ClipboardController {
  /** 已校验明文进入策略门（同步）。 */
  onValidatedWrite(text: string): ClipboardAction;
  /** auto 路径写入：排队串行执行，latest-wins 合并 + 限速 + 策略复查。 */
  requestWrite(text: string): Promise<WriteOutcome>;
  takeToastSlot(): boolean;
  /** 策略切到 off 时调用：排队项 dropped、in-flight 等待方立即按 dropped 结算
   *  （真实写入结果到达时不再二次结算）、清空去重集合。 */
  cancelPending(): void;
  /** 组件卸载：同 cancelPending，且之后不再启动任何新写入。 */
  dispose(): void;
}

export function createClipboardController(opts?: ClipboardControllerOptions): ClipboardController {
  const now = opts?.now ?? (() => Date.now());
  const write = opts?.write;
  const getPolicy = opts?.getPolicy ?? loadClipboardPolicy;
  const writeTimeoutMs = opts?.writeTimeoutMs ?? WRITE_TIMEOUT_MS;
  const schedule =
    opts?.schedule ??
    ((fn: () => void, delayMs: number) => {
      const id = setTimeout(fn, delayMs);
      return { cancel: () => clearTimeout(id) };
    });

  const recent = new Map<string, number>(); // 内容 -> 进入时间（插入序即时间序）
  const writeTimes: number[] = [];
  let lastToastAt = Number.NEGATIVE_INFINITY;
  let disposed = false;
  let pruneTimer: { cancel(): void } | null = null;

  // 一次 in-flight 写入对应一个 batch：等待方列表 + 已结算标记（cancelPending 作废后，
  // 真实写入结果到达时不再二次结算）。
  interface Batch {
    resolvers: ((o: WriteOutcome) => void)[];
    settled: boolean;
  }
  let active: Batch | null = null;
  let activeTimeout: { cancel(): void } | null = null;
  let queuedText: string | null = null;
  let queuedResolvers: ((o: WriteOutcome) => void)[] = [];

  function pruneRecent(t: number): void {
    for (const [k, at] of recent) {
      if (t - at >= DEDUPE_MS) recent.delete(k);
    }
  }

  function rateAllows(t: number): boolean {
    while (writeTimes.length > 0 && t - writeTimes[0] >= WRITE_WINDOW_MS) writeTimes.shift();
    return writeTimes.length < WRITE_MAX_PER_WINDOW;
  }

  function finishActive(batch: Batch, outcome: WriteOutcome): void {
    // 身份校验：超时/迟到回调只允许结算自己的 batch。
    // 反例（无校验时）：A 超时 → B 启动 → A 迟到成功会把 B 结算成 written 并取消 B 的
    // 超时定时器，破坏单 in-flight 约束并产生幻影成功。
    if (active !== batch) return;
    activeTimeout?.cancel();
    activeTimeout = null;
    active = null;
    if (!batch.settled) {
      batch.settled = true;
      for (const r of batch.resolvers) r(outcome);
    }
    pump();
  }

  function pump(): void {
    if (disposed || active || queuedText === null) return;
    const text = queuedText;
    const resolvers = queuedResolvers;
    queuedText = null;
    queuedResolvers = [];
    // 启动前复查策略：排队项只在仍是 auto 授权时启动——auto→ask / off 都是撤销，
    // 未启动的排队项不得再自动写入。
    if (getPolicy() !== 'auto' || !write || !rateAllows(now())) {
      for (const r of resolvers) r('dropped');
      return;
    }
    const batch: Batch = { resolvers, settled: false };
    active = batch;
    // 超时/结算回调各自闭包自己的 batch（配合 finishActive 身份校验）。
    activeTimeout = schedule(() => finishActive(batch, 'failed'), writeTimeoutMs);
    writeTimes.push(now());
    let pending: Promise<void>;
    try {
      pending = write(text);
    } catch {
      // 同步抛错归一为 failed：inFlight 必须复位，否则队列永久卡死 + unhandled rejection。
      finishActive(batch, 'failed');
      return;
    }
    pending.then(
      () => finishActive(batch, 'written'),
      () => finishActive(batch, 'failed'),
    );
  }

  function cancelPending(): void {
    // 排队项：直接按丢弃结算。
    if (queuedText !== null) {
      const resolvers = queuedResolvers;
      queuedText = null;
      queuedResolvers = [];
      for (const r of resolvers) r('dropped');
    }
    // in-flight：等待方立即按 dropped 结算；真实写入结果到达时 finishActive 跳过已结算
    // batch（不得重开 popover / 更新 UI），队列照常复位。
    if (active && !active.settled) {
      active.settled = true;
      for (const r of active.resolvers) r('dropped');
    }
    // off 必须立即遗忘近期明文（不能等下一次插入才淘汰），裁剪定时器一并取消。
    recent.clear();
    pruneTimer?.cancel();
    pruneTimer = null;
  }

  /** 按最早剩余条目的过期时刻排裁剪定时器；裁剪后若仍有未过期条目则继续重排，
   *  保证条目过期不依赖后续插入。 */
  function schedulePrune(): void {
    if (disposed || pruneTimer) return;
    let oldest: number | undefined;
    for (const at of recent.values()) {
      if (oldest === undefined || at < oldest) oldest = at;
    }
    if (oldest === undefined) return;
    const delay = Math.max(0, oldest + DEDUPE_MS - now());
    pruneTimer = schedule(() => {
      pruneTimer = null;
      pruneRecent(now());
      schedulePrune();
    }, delay);
  }

  return {
    onValidatedWrite(text: string): ClipboardAction {
      if (disposed) return 'drop';
      const policy = getPolicy();
      if (policy === 'off') return 'drop';
      const t = now();
      pruneRecent(t);
      if (recent.has(text)) return 'drop';
      recent.set(text, t);
      if (recent.size > DEDUPE_MAX_ENTRIES) {
        const oldest = recent.keys().next().value;
        if (oldest !== undefined) recent.delete(oldest);
      }
      schedulePrune();
      return policy === 'auto' ? 'auto' : 'ask';
    },
    requestWrite(text: string): Promise<WriteOutcome> {
      if (disposed) return Promise.resolve('dropped');
      return new Promise((resolve) => {
        // latest-wins：排队中的旧内容被不同新内容取代时，其等待方立即按丢弃结算。
        if (queuedText !== null && queuedText !== text) {
          for (const r of queuedResolvers) r('dropped');
          queuedResolvers = [];
        }
        queuedText = text;
        queuedResolvers.push(resolve);
        pump();
      });
    },
    takeToastSlot(): boolean {
      const t = now();
      if (t - lastToastAt < TOAST_THROTTLE_MS) return false;
      lastToastAt = t;
      return true;
    },
    cancelPending,
    dispose(): void {
      disposed = true;
      cancelPending();
      activeTimeout?.cancel();
      activeTimeout = null;
      pruneTimer?.cancel();
      pruneTimer = null;
    },
  };
}
