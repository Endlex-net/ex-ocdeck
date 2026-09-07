import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import { WebglAddon } from '@xterm/addon-webgl';
import { clearToken, getToken, wsURL, UNAUTHORIZED_EVENT } from '../api';
import {
  loadMobileCaps,
  loadMobileMode,
  loadTermPrefs,
  resolveFontFamily,
  resolveFontSize,
  type TermPreferences,
} from './preferences';
import { DEFAULT_CAPS, resolveMobileCaps, type EffectiveCaps } from './mobile-mode';
import { readCurrentTermTheme, resolveXtermTheme, watchTermTheme } from './theme';
import { createLockController, type LockController } from './lock';
import { attachTouchGestures, type GestureHandle } from './touch-gestures';
import { shouldSendInput, encodeBinaryInput, createSyntheticGate, type SyntheticGate } from './input-gate';
import { createLockOrchestrator, type LockOrchestrator } from './session-coordination';
import { createImeCompensator, isImeProcessKey, type ImeCompensator } from './ime-compensator';
import { parseOsc52Payload } from './clipboard';
import { debugMark } from '../debug';

export { shouldSendInput, encodeBinaryInput, createSyntheticGate, type SyntheticGate } from './input-gate';
export { createLockOrchestrator, type LockOrchestrator, type LockOrchestratorDeps } from './session-coordination';
export { createImeCompensator, type ImeCompensator } from './ime-compensator';

export type TermConnState =
  | 'idle'
  | 'connecting'
  | 'connected'
  | 'reconnecting'
  | 'recovering' // 1013：任务进程恢复中（Try Again Later），轮询任务状态后重连
  | 'suspended' // 4010：任务已挂起，需用户激活
  | 'closed' // 1000：对端正常关闭（如 shell 退出）
  | 'replaced' // 4009：被新连接替换（其他标签页已接管）
  | 'gone' // 4004：终端已不存在（如挂起后 shell 已消失）
  | 'auth_failed'; // 4001：token 失效

const encoder = new TextEncoder();

/** 键盘避让收缩阈值（CSS px，mobile-terminal-mode-settings design D4）：
 * iOS/iPadOS 虚拟键盘 250px+，Safari 工具栏/地址栏伸缩通常 <100px。写死常量，不做设置项。 */
const KEYBOARD_SHRINK_THRESHOLD = 100;

/**
 * TermSession 封装 xterm.js + 终端 WS 的生命周期：
 * - 首帧 auth+尺寸握手（服务端 5s 超时）；
 * - 二进制帧 <-> xterm IO，JSON resize 控制帧；
 * - 指数退避自动重连用于 1001（服务端重启）与 1006 等网络异常断线；
 *   4001/4004/4009/4010/1000 停止自动重连，由用户显式操作恢复
 *   （4009 提供"接管"，4004 提示终端已关闭，1000 提供"重新连接"）；
 * - 容器尺寸变化（ResizeObserver）时 fit 并同步尺寸到服务端。
 * 重连后屏幕由服务端 tmux 恢复，前端不做本地缓冲。
 *
 * 移动端适配（design D4/D5/D8 + mobile-terminal-mode-settings design D3/D4）：
 * - 统一输入门禁覆盖 onData + onBinary 双出口；
 * - 锁定/手势/键盘避让启用由本机「移动端模式」偏好驱动
 *   （auto=coarse 自适配、on=子开关、off=全停用），经 applyMobileCaps 做状态边沿迁移
 *   （能力未变 MUST NOT 触碰锁定状态与焦点；每次 WS auth_ok 为唯一强制回锁例外）；
 * - 锁定状态唯一所有者，对外暴露 lock()/unlock()/isLocked/onLockChange。
 */
export class TermSession {
  private term: Terminal;
  private fit = new FitAddon();
  private ws: WebSocket | null = null;
  /** 连接代（G4-8 socket identity guard）：每次 closeSocket/换代自增；陈旧连接的
   * 异步回调（onopen/onmessage/onclose）据此直接丢弃——不得改写当前连接、状态或重连 timer。 */
  private wsGen = 0;
  private authed = false;
  private disposed = false;
  private closedByUs = false;
  private retry = 0;
  private timer: ReturnType<typeof setTimeout> | undefined;
  private observer: ResizeObserver;
  private fitRaf = 0;

  private readonly wrap: HTMLElement;
  private readonly lockController: LockController;
  private readonly lockOrchestrator: LockOrchestrator;
  private gestures: GestureHandle | null = null;
  private readonly syntheticGate: SyntheticGate = createSyntheticGate();
  private pointerCoarse = false;
  private pointerMql: MediaQueryList | null = null;
  /** 当前生效能力（mobile-terminal-mode-settings design D3）：null=构造首调前。
   * 偏好不缓存字段，每次 applyMobileCaps 重新读取，仅缓存上一次生效结果用于边沿判定。 */
  private appliedCaps: EffectiveCaps | null = null;
  private readonly lockChangeCallbacks = new Set<(locked: boolean) => void>();
  /** 应用主题订阅退订（终端配色跟随主题，dispose 时退订防泄漏）。 */
  private unwatchTermTheme: (() => void) | null = null;
  /** OSC 52 handler 退订。 */
  private osc52Disposable: { dispose(): void } | null = null;

  // IME 补偿器 + 原生 onData tap（全平台启用，design D7）。
  private imeCompensator: ImeCompensator | null = null;
  // visualViewport 键盘适配（design D6）：UNLOCKED 且聚焦时监听。
  private vvResizeHandler: (() => void) | null = null;
  private vvScrollHandler: (() => void) | null = null;
  private vvFitRaf = 0;
  private imeListenersAttached = false;

  constructor(
    private host: HTMLElement,
    wrap: HTMLElement,
    private wsPath: string,
    private onState: (s: TermConnState) => void,
    /** 已校验的 OSC 52 clipboard-write 明文；同步回调，不得回写终端。 */
    private onClipboardWrite?: (text: string) => void,
  ) {
    const prefs = loadTermPrefs();
    this.term = new Terminal({
      fontFamily: resolveFontFamily(prefs),
      fontSize: resolveFontSize(prefs),
      lineHeight: 1.15,
      cursorBlink: true,
      allowProposedApi: true,
      scrollback: 5000,
      // 终端配色跟随应用主题（terminal/theme.ts，token 对齐常量）；
      // 切换由下方 watchTermTheme 订阅即时应用到已挂载终端，无需重连。
      theme: resolveXtermTheme(readCurrentTermTheme()),
    });
    this.term.loadAddon(this.fit);
    this.term.open(host);
    this.osc52Disposable = this.term.parser.registerOscHandler(52, (data) => {
      const text = parseOsc52Payload(data);
      if (text !== null) this.onClipboardWrite?.(text);
      return true;
    });
    // Shift+Enter → opencode input_newline：xterm 对 Enter/Shift+Enter 均发 \r，修饰信息丢失；
    // 经 custom key handler 翻译为 modifyOtherKeys 形态 CSI 27;2;13~（opentui parse.keypress.ts
    // 无条件解析为 {name:"return", shift:true}）。kitty CSI u 被 opencode useKittyKeyboard
    // 门控，本场景协商不成立，不可用。
    this.term.attachCustomKeyEventHandler((ev) => this.handleCustomKey(ev));
    try {
      this.term.loadAddon(new WebglAddon());
    } catch {
      /* WebGL 不可用时回退 canvas 渲染 */
    }

    // 应用主题切换 → 即时翻转终端配色（term.options.theme 运行时赋值，无需重连）；
    // dispose 时退订（每个已挂载 TermSession 各自订阅，全部即时生效）。
    this.unwatchTermTheme = watchTermTheme((t) => {
      if (this.disposed) return;
      this.term.options.theme = resolveXtermTheme(t);
    });

    this.wrap = wrap;

    // 统一输入门禁：onData + onBinary 双出口汇入同一门禁。
    // onData 同时作为 IME 补偿器的原生发射观测 tap（design D7）。
    this.term.onData((d) => {
      this.imeCompensator?.observeNative(d);
      this.sendInput(d, false);
    });
    this.term.onBinary((d) => this.sendInput(d, true));

    this.observer = new ResizeObserver(() => this.scheduleFit());
    this.observer.observe(host);

    // 锁定控制器（状态唯一所有者）。
    this.lockController = createLockController(wrap);
    // 锁状态变化时通知订阅者 + 重挂手势层（目标元素在 overlay/xterm 间切换）
    // + 切换 visualViewport 监听（仅 UNLOCKED 监听键盘视口）。
    this.lockController.onChange((locked) => {
      this.gestures?.rebind();
      this.updateVisualViewportListener();
      for (const cb of this.lockChangeCallbacks) cb(locked);
    });

    // IME 补偿器（全平台启用，design D7）：纯逻辑模块，emit 同步调 term.input，
    // 调用栈内经 syntheticGate 标记排除自身补发进 recentNative。
    this.imeCompensator = createImeCompensator({
      emit: (data) => this.term.input(data, true),
      now: () => (typeof performance !== 'undefined' ? performance.now() : Date.now()),
      schedule: (fn, delayMs) => {
        const id = setTimeout(fn, delayMs ?? 0);
        return { cancel: () => clearTimeout(id) };
      },
    });
    this.attachImeListeners();

    // 锁定状态机协调器：注入依赖（thin adapter），行为见 session-coordination.ts。
    this.lockOrchestrator = createLockOrchestrator({
      lock: () => this.lockController.lock(),
      blur: () => this.term.blur(),
      unlockSilently: () => this.lockController.unlock(),
      focus: () => this.term.focus(),
      attachGestures: () => this.attachGestures(),
      detachGestures: () => this.detachGestures(),
    });

    // pointer 类型检测：matchMedia('pointer') 描述主指针设备；
    // 仅外接键盘不改变 pointer 语义。
    this.pointerCoarse = this.detectPointerCoarse();
    // 移动端模式能力落地（design D3 构造首调：prev===null，按 next 落地初始状态）。
    this.applyMobileCaps();
    if (typeof window !== 'undefined' && typeof window.matchMedia === 'function') {
      this.pointerMql = window.matchMedia('(pointer: coarse)');
      this.pointerMql.addEventListener('change', this.onPointerChange);
    }
  }

  connect(): void {
    // recoveryProbe（1013 定时探测）：保持「进程启动中」展示不闪「连接中」；
    // 外部 connect（任务状态回 active 驱动 / 手动重连）按正常状态展示。
    this.connectInternal(false);
  }

  private connectInternal(recoveryProbe: boolean): void {
    if (this.disposed) return;
    this.clearTimer();
    this.closedByUs = false;
    this.closeSocket();
    const gen = this.wsGen; // G4-8：本连接代（closeSocket 已完成换代）
    this.setState(recoveryProbe ? 'recovering' : this.retry > 0 ? 'reconnecting' : 'connecting');

    debugMark('odterm:ws-create');
    const ws = new WebSocket(wsURL(this.wsPath));
    ws.binaryType = 'arraybuffer';
    this.ws = ws;
    this.authed = false;

    ws.onopen = () => {
      if (gen !== this.wsGen) return; // 陈旧连接回调：新连接已建立，不得发送
      debugMark('odterm:ws-open');
      // 首帧：auth + 初始尺寸握手合一。
      this.fitNow();
      ws.send(
        JSON.stringify({
          type: 'auth',
          token: getToken(),
          cols: this.term.cols,
          rows: this.term.rows,
        }),
      );
    };
    ws.onmessage = (ev: MessageEvent) => {
      if (gen !== this.wsGen) return; // 陈旧连接回调：不得写入终端/状态
      if (typeof ev.data === 'string') {
        try {
          const msg = JSON.parse(ev.data) as { type?: string };
          if (msg.type === 'auth_ok') {
            debugMark('odterm:auth-ok');
            // 任何 WS 连接建立/auth_ok（含重连、Tab 切换）→ 锁定能力启用即强制 LOCKED
            // （design D3：边沿保护的唯一例外，入参为 appliedCaps.lock 而非 pointerCoarse）。
            // 必须先于 authed/connected 状态暴露与任何外部回调/fit，防门禁未就绪窗口泄漏。
            this.lockOrchestrator.onAuthOk(this.appliedCaps?.lock === true, () => {
              this.authed = true;
              this.retry = 0;
              this.setState('connected');
              this.fitNow(); // 以握手尺寸再校准一次
            });
          }
          // type=error 的帧等待随后的关闭帧统一处理
        } catch {
          /* 非 JSON 文本帧忽略 */
        }
        return;
      }
      debugMark('odterm:first-data');
      this.term.write(new Uint8Array(ev.data as ArrayBuffer));
    };
    ws.onerror = () => {
      /* onclose 随后触发，统一处理 */
    };
    ws.onclose = (ev: CloseEvent) => {
      // G4-8 socket identity guard：仅当前代连接的 onclose 允许改写状态/timer/
      // this.ws——陈旧连接（connect 重入后 closeSocket 关闭的旧 socket）的延迟
      // onclose 不得清空新连接、不得触发误重连。
      if (gen !== this.wsGen) return;
      this.authed = false;
      this.ws = null;
      if (this.disposed || this.closedByUs) return;
      switch (ev.code) {
        case 1013:
          // 任务进程恢复中（Try Again Later）：停止指数退避重连，展示「进程启动中」。
          // 主路径：任务状态流（SSE）驱动外层 active prop → 回到 active 即重连；
          // 兜底：定时探测重连（任务状态流断连错过翻转时不卡死；服务端
          // ensureRecovery 幂等，重连即探测）。
          this.retry = 0;
          this.setState('recovering');
          this.timer = setTimeout(() => this.connectInternal(true), 3000);
          return;
        case 4001: // 未认证：token 失效，回 token 输入页
          clearToken();
          window.dispatchEvent(new Event(UNAUTHORIZED_EVENT));
          this.setState('auth_failed');
          return;
        case 4010: // 任务已挂起：停止自动重连，提示用户激活
          this.retry = 0;
          this.setState('suspended');
          return;
        case 4009: // 被新连接替换：MUST NOT 自动重连（否则两标签页互相替换死循环）
          this.retry = 0;
          this.setState('replaced');
          return;
        case 4004: // 终端不存在（如挂起后 shell 已消失）：重连无意义
          this.retry = 0;
          this.setState('gone');
          return;
        case 1000: // 服务端正常关闭（如 shell 退出）：不自动重连
          this.retry = 0;
          this.setState('closed');
          return;
        default:
          // 1001（服务端重启）、1006 等异常断线（无 close frame）、1011 内部错误：
          // 指数退避静默重连（persist 模式服务端重启后自动恢复）
          this.retry += 1;
          this.setState('reconnecting');
          const delay = Math.min(500 * 2 ** this.retry, 8000);
          this.timer = setTimeout(() => this.connect(), delay);
      }
    };
  }

  /** 手动重连（重置退避）。 */
  reconnect(): void {
    this.retry = 0;
    this.connect();
  }

  disconnect(): void {
    this.closedByUs = true;
    this.clearTimer();
    this.closeSocket();
    this.setState('idle');
  }

  dispose(): void {
    this.disposed = true;
    this.clearTimer();
    this.closeSocket();
    this.observer.disconnect();
    this.unwatchTermTheme?.();
    this.unwatchTermTheme = null;
    if (this.fitRaf) cancelAnimationFrame(this.fitRaf);
    // visualViewport 监听清理：先移除 resize/scroll listener（防全局 visualViewport 持续引用已销毁 session），再清引用、取消 pending rAF、清除 wrap maxHeight 内联样式。
    this.detachVisualViewportListener();
    this.wrap.style.maxHeight = '';
    this.gestures?.dispose();
    this.gestures = null;
    this.imeCompensator?.dispose();
    this.imeCompensator = null;
    this.pointerMql?.removeEventListener('change', this.onPointerChange);
    this.pointerMql = null;
    this.lockOrchestrator.dispose();
    this.lockChangeCallbacks.clear();
    this.lockController.dispose();
    this.osc52Disposable?.dispose();
    this.osc52Disposable = null;
    this.term.dispose();
  }

  // ---------- 锁定对外接口（design D8） ----------

  /**
   * 锁定终端。委托 LockOrchestrator：先置门禁标志再 term.blur()，
   * 防 blur 发出的 \x1b[O focus-loss 序列在门禁生效前泄漏（design D5）。
   */
  lock(): void {
    this.lockOrchestrator.lock();
  }

  /**
   * 解锁终端。必须在可信用户手势（解锁按钮 click）同步调用栈内调用，
   * 以移除 overlay + term.focus() 唤起虚拟键盘（iOS Safari 限制）。
   */
  unlock(): void {
    this.lockOrchestrator.unlock();
  }

  isLocked(): boolean {
    return this.lockController.isLocked();
  }

  onLockChange(cb: (locked: boolean) => void): () => void {
    this.lockChangeCallbacks.add(cb);
    return () => this.lockChangeCallbacks.delete(cb);
  }

  // ---------- 内部 ----------

  private closeSocket(): void {
    this.wsGen += 1; // G4-8：换代——在途旧连接的全部异步回调即刻失效
    if (this.ws) {
      try {
        this.ws.close();
      } catch {
        /* ignore */
      }
      this.ws = null;
    }
    this.authed = false;
  }

  private clearTimer(): void {
    if (this.timer !== undefined) {
      clearTimeout(this.timer);
      this.timer = undefined;
    }
  }

  private setState(s: TermConnState): void {
    if (!this.disposed) this.onState(s);
  }

  /** 就地应用终端外观偏好（不重建实例、不断 WS）。下一帧 fit 并同步 winsize。 */
  applyPreferences(prefs: TermPreferences): void {
    if (this.disposed) return;
    this.term.options.fontFamily = resolveFontFamily(prefs);
    this.term.options.fontSize = resolveFontSize(prefs);
    this.scheduleFit();
    // 偏好变更触发移动端能力重评估（TERM_PREFS_CHANGED 监听方 TerminalView 调用本方法，
    // mobile-terminal-mode-settings design D3 触发点之一）。
    this.applyMobileCaps();
  }

  /**
   * 移动端模式能力边沿迁移（mobile-terminal-mode-settings design D3，唯一迁移入口）。
   * 每次判定重新读取偏好（不缓存）；仅 mode==='on' 才读子开关（判别式加载，auto/off
   * 传 DEFAULT_CAPS 占位、不发起 caps 读取）。
   * - prev===null（构造首调）：按 next 落地初始状态；
   * - sameCaps：no-op，MUST NOT 触碰锁定状态与焦点（保护用户手动解锁）；
   * - lock false→true：lockOrchestrator.lock()（门禁先置位再 blur）；
   *   true→false：lockController.unlock()（silent unlock，MUST NOT focus）；
   * - gestures 边沿 attach/detach；keyboardAvoid false→true 走既有 shouldListen 判定，
   *   true→false：detach listener + 清 wrap maxHeight + refit。
   */
  private applyMobileCaps(): void {
    const mode = loadMobileMode();
    const caps = mode === 'on' ? loadMobileCaps() : DEFAULT_CAPS;
    const next = resolveMobileCaps(mode, caps, this.pointerCoarse);
    const prev = this.appliedCaps;
    this.appliedCaps = next;
    if (prev === null) {
      // 构造首调：按 next 落地（等价原 coarse 直判构造，统一走 orchestrator lock 路径）。
      if (next.lock) this.lockOrchestrator.lock(); // 门禁先置位再 blur
      if (next.gestures) this.attachGestures();
      this.updateVisualViewportListener(); // shouldListen 含 keyboardAvoid 判定
      return;
    }
    if (prev.lock === next.lock && prev.gestures === next.gestures && prev.keyboardAvoid === next.keyboardAvoid) {
      return;
    }
    if (next.lock !== prev.lock) {
      if (next.lock) {
        this.lockOrchestrator.lock();
      } else {
        // 系统级解锁：仅移除锁，MUST NOT focus（防 \x1b[I focus-in 序列注入）。
        this.lockController.unlock();
      }
    }
    if (next.gestures !== prev.gestures) {
      if (next.gestures) this.attachGestures();
      else this.detachGestures();
    }
    if (next.keyboardAvoid !== prev.keyboardAvoid) {
      if (next.keyboardAvoid) {
        this.updateVisualViewportListener();
      } else {
        this.detachVisualViewportListener();
        this.wrap.style.maxHeight = '';
        this.fitNow();
      }
    }
  }

  private scheduleFit(): void {
    if (this.fitRaf) return;
    this.fitRaf = requestAnimationFrame(() => {
      this.fitRaf = 0;
      this.fitNow();
    });
  }

  private fitNow(): void {
    // 隐藏（display:none）时尺寸为 0，fit 会算出 1x1，跳过。
    if (this.host.clientWidth === 0 || this.host.clientHeight === 0) return;
    try {
      this.fit.fit();
    } catch {
      return;
    }
    if (this.ws?.readyState === WebSocket.OPEN && this.authed) {
      this.ws.send(JSON.stringify({ type: 'resize', cols: this.term.cols, rows: this.term.rows }));
    }
  }

  /**
   * Shift+Enter 拦截翻译：keydown 命中即 preventDefault（阻止浏览器派生 keypress，
   * 否则 xterm _keyPress 会把 charCode 13 再发为 \r，造成「先换行后提交」），
   * 经统一输入门禁发送 CSI 27;2;13~ 并 return false 阻止 xterm 默认 \r。
   * 防御：残留的 Shift+Enter keypress 直接吞掉（return false），不重复发送。
   * 其余键一律 return true 不干预。
   * IME 门禁：isComposing 之外复用 isImeProcessKey（key==='Process' || keyCode===229，
   * ime-compensator 既有契约）覆盖 composition 边界上 isComposing=false 的事件。
   */
  private handleCustomKey(ev: KeyboardEvent): boolean {
    const isShiftEnter =
      ev.key === 'Enter' && ev.shiftKey && !ev.ctrlKey && !ev.altKey && !ev.metaKey;
    if (ev.type === 'keydown') {
      if (isShiftEnter && !ev.isComposing && !isImeProcessKey(ev.key, ev.keyCode)) {
        ev.preventDefault();
        this.sendInput('\x1b[27;2;13~', false);
        return false;
      }
      return true;
    }
    if (ev.type === 'keypress' && isShiftEnter) {
      return false;
    }
    return true;
  }

  /** 统一输入门禁（design D5）：onData + onBinary 双出口。 */
  private sendInput(d: string, binary: boolean): void {
    if (
      !shouldSendInput({
        authed: this.authed,
        wsOpen: this.ws?.readyState === WebSocket.OPEN,
        locked: this.lockController.isLocked(),
        syntheticInFlight: this.syntheticGate.inFlight(),
      })
    ) {
      return;
    }
    if (binary) {
      this.ws!.send(encodeBinaryInput(d));
    } else {
      this.ws!.send(encoder.encode(d));
    }
  }

  private markSynthetic<T>(fn: () => T): T {
    return this.syntheticGate.markSynthetic(fn);
  }

  private xtermElement(): HTMLElement {
    // 优先用 term.element 公开 API（term.open 后挂载的 .xterm 元素），做存在性检查；
    // fallback 仅在公开 API 不可用时兜底。
    return this.term.element ?? (this.host.querySelector('.xterm') as HTMLElement);
  }

  /** attach 触控手势层（由移动端模式偏好驱动，mobile-terminal-mode-settings D2/D3）。
   * 重复 attach 前先 dispose 旧实例。 */
  private attachGestures(): void {
    if (this.gestures) return;
    this.gestures = attachTouchGestures({
      term: this.term,
      getTarget: () =>
        this.lockController.isLocked() ? this.lockController.overlay : this.xtermElement(),
      ctx: {
        isLocked: () => this.lockController.isLocked(),
        markSynthetic: <T>(fn: () => T): T => this.markSynthetic(fn),
      },
    });
  }

  private detachGestures(): void {
    this.gestures?.dispose();
    this.gestures = null;
  }

  /**
   * IME 补偿器 textarea 监听接线（design D7/D8）：capture 阶段镜像 keydown/keyup/composition/input。
   * 全平台启用；不修改 textarea.value。
   */
  private attachImeListeners(): void {
    if (this.imeListenersAttached) return;
    const ta = this.term.textarea;
    if (!ta) return;
    this.imeListenersAttached = true;
    ta.addEventListener('keydown', (e) => this.imeCompensator?.handleKeyDown(e), { capture: true });
    ta.addEventListener('keyup', () => this.imeCompensator?.handleKeyUp(), { capture: true });
    ta.addEventListener('compositionstart', () => this.imeCompensator?.handleCompositionStart(), { capture: true });
    ta.addEventListener('compositionend', () => this.imeCompensator?.handleCompositionEnd(), { capture: true });
    ta.addEventListener('input', (e) => {
      const ie = e as InputEvent;
      this.imeCompensator?.handleInput({
        inputType: ie.inputType,
        data: ie.data,
        isTrusted: ie.isTrusted,
        composed: ie.composed,
        isComposing: ie.isComposing,
      });
    }, { capture: true });
    // focus/blur 触发 visualViewport 监听切换。
    ta.addEventListener('focus', () => this.updateVisualViewportListener());
    ta.addEventListener('blur', () => this.updateVisualViewportListener());
  }

  /**
   * visualViewport 键盘适配（design D6 + mobile-terminal-mode-settings D4）：
   * 键盘避让启用（appliedCaps.keyboardAvoid）且 UNLOCKED 且 textarea 聚焦时监听 resize+scroll（rAF 去抖），
   * 仅视口明显压缩（shrink ≥ KEYBOARD_SHRINK_THRESHOLD）才按
   * max(0, vv.offsetTop + vv.height - wrap top) 设 wrap maxHeight → fitNow → WS resize。
   * blur/锁定/避让关闭/卸载移除内联样式并 refit；visualViewport API 缺失跳过。
   */
  private updateVisualViewportListener(): void {
    const vv = typeof window !== 'undefined' ? window.visualViewport : undefined;
    if (!vv) return;
    const shouldListen =
      !this.disposed &&
      this.appliedCaps?.keyboardAvoid === true &&
      !this.lockController.isLocked() &&
      this.term.textarea === document.activeElement;
    if (shouldListen && !this.vvResizeHandler) {
      this.vvResizeHandler = () => this.scheduleVvFit();
      this.vvScrollHandler = () => this.scheduleVvFit();
      vv.addEventListener('resize', this.vvResizeHandler);
      vv.addEventListener('scroll', this.vvScrollHandler);
      // 立即应用一次以对齐当前键盘状态。
      this.scheduleVvFit();
    } else if (!shouldListen && this.vvResizeHandler) {
      this.detachVisualViewportListener();
      // 移除内联 maxHeight 并 refit（恢复全高布局）。
      this.wrap.style.maxHeight = '';
      this.fitNow();
    }
  }

  /** 移除 visualViewport resize/scroll listener、取消 pending rAF、清引用。不清 maxHeight（由调用方决定是否 refit）。 */
  private detachVisualViewportListener(): void {
    const vv = typeof window !== 'undefined' ? window.visualViewport : undefined;
    if (vv && this.vvResizeHandler) {
      vv.removeEventListener('resize', this.vvResizeHandler);
    }
    if (vv && this.vvScrollHandler) {
      vv.removeEventListener('scroll', this.vvScrollHandler);
    }
    this.vvResizeHandler = null;
    this.vvScrollHandler = null;
    if (this.vvFitRaf) {
      cancelAnimationFrame(this.vvFitRaf);
      this.vvFitRaf = 0;
    }
  }

  private scheduleVvFit(): void {
    if (this.vvFitRaf) return;
    this.vvFitRaf = requestAnimationFrame(() => {
      this.vvFitRaf = 0;
      this.fitForViewport();
    });
  }

  /**
   * 键盘避让阈值启发式（mobile-terminal-mode-settings design D4）：
   * 每次先清 inline maxHeight 测自然布局（基线 MUST NOT 被自身写入的高度污染），
   * shrink = rect.bottom - (vv.offsetTop + vv.height)；达阈值才写回目标 maxHeight，
   * 否则恢复全高（工具栏抖动/键盘收起共用同一路径）；仅目标值变化时 fitNow。
   */
  private fitForViewport(): void {
    const vv = window.visualViewport;
    if (!vv || this.disposed) return;
    const prevMax = this.wrap.style.maxHeight;
    this.wrap.style.maxHeight = '';
    const rect = this.wrap.getBoundingClientRect();
    const visibleBottom = vv.offsetTop + vv.height;
    const shrink = rect.bottom - visibleBottom;
    const target = shrink >= KEYBOARD_SHRINK_THRESHOLD ? Math.max(0, visibleBottom - rect.top) + 'px' : '';
    this.wrap.style.maxHeight = target;
    if (target !== prevMax) this.fitNow();
  }

  private detectPointerCoarse(): boolean {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return false;
    return window.matchMedia('(pointer: coarse)').matches;
  }

  /**
   * pointer 类型动态变化（mobile-terminal-mode-settings design D3 触发点）：
   * 更新 pointerCoarse 后走 applyMobileCaps 边沿迁移，**替换**原 lockOrchestrator.onPointerChange
   * 委托（两条迁移路径不得并存）。auto 模式迁移结果与既有语义逐点等价
   * （coarse→lock+手势，fine→detach 手势+silent unlock）；on/off 模式 caps 与 coarse 无关，天然幂等。
   */
  private onPointerChange = (ev: MediaQueryListEvent): void => {
    this.pointerCoarse = ev.matches;
    this.applyMobileCaps();
  };
}
