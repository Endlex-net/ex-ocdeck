import { createSyntheticGate, type SyntheticGate } from './input-gate';

/**
 * 应用侧 IME 候选输入仲裁（design D7）。
 *
 * 纯逻辑模块：不 import xterm/DOM，时钟/调度器/emit 注入，vitest 在 Node 无 DOM 可直接单测。
 *
 * 模型：候选字符不立即补发，挂起一个宏任务 settle，观测 xterm 原生 onData 实际发出了什么，
 * 结算时只补「原生没发」的内容。fail-closed：原生覆盖因裁剪/容量溢出不可证明时丢弃候选，
 * 漏发优先于双发。
 *
 * 关键契约（design D7）：
 * - 候选资格判决：inputType==='insertText' && ev.data && ev.isTrusted && ev.composed===true
 *   && [...ev.data].length===1 && anyKeyDown && !nonModKeyDown && !ev.isComposing && !compositionActive
 *   任一不满足 → fail-closed 丢弃不进队列
 * - 镜像状态：anyKeyDown（任意 keydown 置 true/任意 keyup 清 false）；
 *   nonModKeyDown（修饰键 {Shift,Control,Alt,Meta,CapsLock} + IME 处理键 key==='Process' || keyCode===229
 *   双条件判定不置位，其余 keydown 置 true，任意 keyup 清 false）；
 *   compositionActive（compositionstart/end）
 * - settle：共享单定时器 setTimeout(0)；待结算批次在完成全部匹配前仍视为 pending（裁剪以完整 pending 集计算保护窗口）
 * - 覆盖判定：∃ e ∈ recentNative，e.at ∈ [c.at-100ms, settleNow] 且 e.remainingText 包含 c.text
 *   → 删除一次出现（occurrence 级），remainingText 耗尽立即移出 ring；未覆盖 → term.input(c.text, true) 补发
 * - 匹配顺序：多条可匹配取时间最早者（时间戳同取 ring 插入序最早者）
 * - 裁剪安全：惰性裁剪（observeNative/settle 时）只允许丢弃「早于 min(c.at)-100ms 且超 150ms」的记录，
 *   MUST NOT 丢弃落在 pending 候选回看窗口内的记录
 * - 容量：硬上限 32，溢出丢最旧；溢出落在 pending 窗口 → 该候选标记不可证明；溢出 MUST 记 lastCapacityEvictionAt=now()；
 *   新候选 lastCapacityEvictionAt >= c.at-100ms → 立即不可证明
 * - 自身发射排除：补偿器经 emit 发出的内容 MUST NOT 进 recentNative（复用 SyntheticGate 嵌套计数——
 *   emit 同步触发 onData tap，调用栈内标记排除）
 * - emit MUST 同步调 term.input()；不修改 textarea.value
 */

/** 修饰键集合（design D7）：按 KeyboardEvent.key 判定，keydown 不置位 nonModKeyDown。 */
const MODIFIER_KEYS = new Set(['Shift', 'Control', 'Alt', 'Meta', 'CapsLock']);

/** 判定 keydown 是否为 IME 处理键（key === 'Process' || keyCode === 229，双条件并集）。
 *  纯函数，可单测。用于 handleKeyDown 不置位判定与 settle 重排触发。 */
export function isImeProcessKey(key: string, keyCode: number | undefined): boolean {
  return key === 'Process' || keyCode === 229;
}

/** recentNative ring buffer 记录。 */
interface NativeRecord {
  text: string;
  at: number;
  /** 剩余可消耗文本（occurrence 级消耗模型）。 */
  remainingText: string;
}

/** pending 候选。 */
interface PendingCandidate {
  text: string;
  at: number;
  /** 是否不可证明（裁剪/容量溢出导致历史残缺）→ fail-closed 不补发。 */
  unprovable: boolean;
}

/** 资格判决输入（纯逻辑，可单测）。anyKeyDown/nonModKeyDown/compositionActive 由镜像状态提供。 */
export interface QualifyInput {
  inputType: string;
  data: string | null;
  isTrusted: boolean;
  composed: boolean;
  isComposing: boolean;
  anyKeyDown: boolean;
  nonModKeyDown: boolean;
  compositionActive: boolean;
}

/** handleInput 接收的 InputEvent 字段（镜像状态由补偿器内部维护，不在此传入）。 */
export interface InputEventFields {
  inputType: string;
  data: string | null;
  isTrusted: boolean;
  composed: boolean;
  isComposing: boolean;
}

/** 资格判决（纯函数，可单测）。任一不满足 → false（fail-closed 不进队列）。 */
export function qualifyCandidate(inp: QualifyInput): boolean {
  return (
    inp.inputType === 'insertText' &&
    Boolean(inp.data) &&
    inp.isTrusted === true &&
    inp.composed === true &&
    [...(inp.data ?? '')].length === 1 &&
    inp.anyKeyDown === true &&
    inp.nonModKeyDown === false &&
    inp.isComposing === false &&
    inp.compositionActive === false
  );
}

/** 判定 KeyboardEvent.key 是否为修饰键（纯函数，可单测）。 */
export function isModifierKey(key: string): boolean {
  return MODIFIER_KEYS.has(key);
}

/** 调度器注入：返回可取消 handle。 */
export interface ScheduleHandle {
  cancel(): void;
}

export interface ImeCompensatorOptions {
  /** 接 term.input——MUST 同步调用。调用栈内嵌套计数标记，排除自身补发进 recentNative。 */
  emit: (data: string) => void;
  /** 单调毫秒时钟注入（生产 performance.now()，测试 fake clock）。 */
  now: () => number;
  /** 调度器注入（生产同 Window setTimeout(fn, delayMs ?? 0)），返回可取消 handle。 */
  schedule: (fn: () => void, delayMs?: number) => ScheduleHandle;
}

export interface ImeCompensator {
  /** 镜像 keydown（任意 keydown 置 anyKeyDown=true；不在不置位集合内的键置 nonModKeyDown=true）。
   *  IME 处理键（key==='Process' || keyCode===229）不置位 nonModKeyDown，但若 pending 非空
   *  （settle 已调度）MUST 取消并重新调度 settle——重排的 settle 落在该 keydown 注册的
   *  xterm diff timer 之后结算，闭合 input-before-229 批量竞争窗口。 */
  handleKeyDown(e: { key: string; keyCode?: number }): void;
  /** 镜像 keyup（任意 keyup 清 anyKeyDown=false、nonModKeyDown=false）。 */
  handleKeyUp(): void;
  /** compositionstart → compositionActive=true。 */
  handleCompositionStart(): void;
  /** compositionend → compositionActive=false。 */
  handleCompositionEnd(): void;
  /** capture input → 资格判决（镜像状态由补偿器内部维护）→ 挂起候选 + 调度 settle。 */
  handleInput(e: InputEventFields): void;
  /** 接 session 的 onData tap，记录原生发射历史（自身补发经标记排除）。 */
  observeNative(text: string): void;
  /** 立即触发结算（测试用；生产由 schedule 调度）。 */
  settle(): void;
  /** 取消 pending settle、清空状态（session 卸载时调用）。 */
  dispose(): void;
}

const CAPACITY = 32;
const LOOKBACK_MS = 100;
const TRIM_LATENCY_MS = 150;

export function createImeCompensator(opts: ImeCompensatorOptions): ImeCompensator {
  let anyKeyDown = false;
  let nonModKeyDown = false;
  let compositionActive = false;

  const recentNative: NativeRecord[] = [];
  const pending: PendingCandidate[] = [];
  let lastCapacityEvictionAt: number | undefined = undefined;

  // 共享单定时器（setTimeout 0）。
  let settleHandle: ScheduleHandle | null = null;

  // 自身发射排除：emit 同步触发 onData tap，调用栈内嵌套计数标记排除。
  const gate: SyntheticGate = createSyntheticGate();

  function scheduleSettle(): void {
    if (settleHandle) return; // 已有 pending settle
    settleHandle = opts.schedule(() => {
      settleHandle = null;
      settle();
    }, 0);
  }

  /** 惰性裁剪：仅在 observeNative/settle 时执行。MUST NOT 丢弃落在 pending 候选回看窗口内的记录。 */
  function trim(): void {
    if (recentNative.length === 0) return;
    // 待结算批次在完成全部匹配前仍视为 pending——裁剪以完整 pending 集计算保护窗口。
    if (pending.length === 0) {
      // 无 pending：可丢弃所有超过 TRIM_LATENCY_MS 的记录。
      const now = opts.now();
      while (recentNative.length > 0 && now - recentNative[0].at > TRIM_LATENCY_MS) {
        recentNative.shift();
      }
      return;
    }
    // 有 pending：保护窗口起点 = min(c.at) - LOOKBACK_MS；只允许丢弃早于该起点且超 TRIM_LATENCY_MS 的记录。
    const minCandidateAt = Math.min(...pending.map((c) => c.at));
    const protectBefore = minCandidateAt - LOOKBACK_MS;
    // ring 按插入序（时间递增）；从头丢弃早于 protectBefore 且超 TRIM_LATENCY_MS 的记录。
    while (recentNative.length > 0) {
      const r = recentNative[0];
      if (r.at >= protectBefore) break; // 落在保护窗口内，停止
      if (opts.now() - r.at <= TRIM_LATENCY_MS) break; // 未超延迟阈值，停止
      recentNative.shift();
    }
  }

  /** 容量溢出处理：丢最旧，记水位，标记 pending 中窗口被覆盖的候选为不可证明。 */
  function evictIfOverflow(): void {
    while (recentNative.length > CAPACITY) {
      const evicted = recentNative.shift();
      if (!evicted) break;
      lastCapacityEvictionAt = opts.now();
      // 溢出记录落在某 pending 候选回看窗口内 → 该候选标记不可证明。
      for (const c of pending) {
        if (c.unprovable) continue;
        if (evicted.at >= c.at - LOOKBACK_MS) {
          c.unprovable = true;
        }
      }
    }
  }

  /**
   * 匹配候选 c：在 recentNative 中找时间最早（时间戳同取插入序）的覆盖记录，
   * 删除一次出现（occurrence 级），耗尽立即移出 ring。返回是否覆盖。
   */
  function tryConsume(c: PendingCandidate, settleNow: number): boolean {
    let bestIdx = -1;
    let bestAt = Infinity;
    for (let i = 0; i < recentNative.length; i++) {
      const r = recentNative[i];
      if (r.at < c.at - LOOKBACK_MS) continue;
      if (r.at > settleNow) continue;
      if (!r.remainingText.includes(c.text)) continue;
      // 时间最早者；时间戳同取插入序最早（i 递增，`<` 保留先遇到的）。
      if (r.at < bestAt) {
        bestAt = r.at;
        bestIdx = i;
      }
    }
    if (bestIdx === -1) return false;
    const r = recentNative[bestIdx];
    // 删除一次出现。
    const idx = r.remainingText.indexOf(c.text);
    r.remainingText = r.remainingText.slice(0, idx) + r.remainingText.slice(idx + c.text.length);
    // 耗尽立即移出 ring。
    if (r.remainingText.length === 0) recentNative.splice(bestIdx, 1);
    return true;
  }

  function settle(): void {
    // 结算前惰性裁剪（以完整 pending 集计算保护窗口——MUST NOT 先排出再裁剪）。
    trim();
    const settleNow = opts.now();
    // 待结算批次在完成全部匹配前仍视为 pending：遍历匹配读 pending 原地元素。
    // 异常路径（emit 抛错可沿 term.input() 栈传播）MUST NOT 损坏状态——
    // try/finally 保证批次在匹配结束（正常或异常）后整体移出 pending，
    // 已消费的 occurrence 不会因 pending 残留而在下次 settle 重放双发。
    // 用 batchSize 快照：settle 期间若重入追加新候选（如 emit 同步触发 observeNative→
    // 但 gate 标记排除自身发射，理论不重入；防御性保留 settle 期间追加的新候选）。
    const batchSize = pending.length;
    try {
      for (let i = 0; i < batchSize; i++) {
        const c = pending[i];
        if (c.unprovable) continue; // 不可证明 → fail-closed 不补发
        const covered = tryConsume(c, settleNow);
        if (!covered) {
          // 未覆盖 → term.input(c.text, true) 补发（经 gate 标记排除自身进 recentNative）。
          gate.markSynthetic(() => {
            opts.emit(c.text);
          });
        }
      }
    } finally {
      // 整批移出（保留 settle 期间重入追加的新候选——splice(0, batchSize) 只移前 batchSize 条）。
      pending.splice(0, batchSize);
    }
  }

  return {
    handleKeyDown(e) {
      anyKeyDown = true;
      // nonModKeyDown 不置位集合 = 修饰键 + IME 处理键（'Process'/229 双条件）。
      const imeKey = isImeProcessKey(e.key, e.keyCode);
      if (!MODIFIER_KEYS.has(e.key) && !imeKey) nonModKeyDown = true;
      // 候选 pending 期间收到 229 keydown → MUST 取消并重新调度 settle（design D7 不双发论证第 3 条
      // Safari 时序分支）：重排的 settle 落在该 keydown 注册的 xterm diff timer 之后结算，
      // diff 先结算原生发出进 recentNative → settle 观测覆盖只补未覆盖候选。
      // pending 为空时无需重排（无行为差异，diff 空读不写）。
      if (imeKey && settleHandle) {
        settleHandle.cancel();
        settleHandle = null;
        scheduleSettle();
      }
    },
    handleKeyUp() {
      anyKeyDown = false;
      nonModKeyDown = false;
    },
    handleCompositionStart() {
      compositionActive = true;
    },
    handleCompositionEnd() {
      compositionActive = false;
    },
    handleInput(e) {
      if (!qualifyCandidate({
        inputType: e.inputType,
        data: e.data,
        isTrusted: e.isTrusted,
        composed: e.composed,
        isComposing: e.isComposing,
        anyKeyDown,
        nonModKeyDown,
        compositionActive,
      })) {
        return; // fail-closed 丢弃不进队列
      }
      const at = opts.now();
      // 新候选回看窗口与历史缺口重叠 → 立即不可证明。
      const unprovable = lastCapacityEvictionAt !== undefined && lastCapacityEvictionAt >= at - LOOKBACK_MS;
      pending.push({ text: e.data!, at, unprovable });
      scheduleSettle();
    },
    observeNative(text) {
      // 自身发射排除：emit 在 gate 栈内同步触发 onData tap → 跳过记录。
      if (gate.inFlight()) return;
      if (!text) return; // 空文本忽略（避免空 exhausted record 占位）
      recentNative.push({ text, at: opts.now(), remainingText: text });
      // 先惰性裁剪（清掉 stale 记录），再判容量溢出——避免 stale ring 撑爆容量产生不必要的
      // lastCapacityEvictionAt，导致随后合法标点被误 fail-closed。
      trim();
      evictIfOverflow();
    },
    settle,
    dispose() {
      if (settleHandle) {
        settleHandle.cancel();
        settleHandle = null;
      }
      pending.length = 0;
      recentNative.length = 0;
      anyKeyDown = false;
      nonModKeyDown = false;
      compositionActive = false;
      lastCapacityEvictionAt = undefined;
    },
  };
}