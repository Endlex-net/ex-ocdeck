import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import { WebglAddon } from '@xterm/addon-webgl';
import { clearToken, getToken, wsURL, UNAUTHORIZED_EVENT } from '../api';
import {
  loadTermPrefs,
  resolveFontFamily,
  resolveFontSize,
  type TermPreferences,
} from './preferences';
import { createLockController, type LockController } from './lock';
import { attachTouchGestures, type GestureHandle } from './touch-gestures';
import { shouldSendInput, encodeBinaryInput, createSyntheticGate, type SyntheticGate } from './input-gate';
import { createLockOrchestrator, type LockOrchestrator } from './session-coordination';
import { createImeCompensator, type ImeCompensator } from './ime-compensator';

export { shouldSendInput, encodeBinaryInput, createSyntheticGate, type SyntheticGate } from './input-gate';
export { createLockOrchestrator, type LockOrchestrator, type LockOrchestratorDeps } from './session-coordination';
export { createImeCompensator, type ImeCompensator } from './ime-compensator';

export type TermConnState =
  | 'idle'
  | 'connecting'
  | 'connected'
  | 'reconnecting'
  | 'suspended' // 4010：任务已挂起，需用户激活
  | 'closed' // 1000：对端正常关闭（如 shell 退出）
  | 'replaced' // 4009：被新连接替换（其他标签页已接管）
  | 'gone' // 4004：终端已不存在（如挂起后 shell 已消失）
  | 'auth_failed'; // 4001：token 失效

const encoder = new TextEncoder();

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
 * 移动端适配（design D4/D5/D8）：
 * - 统一输入门禁覆盖 onData + onBinary 双出口；
 * - coarse 设备默认锁定（overlay 拦截 + term.blur），可显式解锁；
 * - pointer 类型动态变化重评估（fine 自动解锁，coarse 强制锁定）；
 * - 锁定状态唯一所有者，对外暴露 lock()/unlock()/isLocked/onLockChange。
 */
export class TermSession {
  private term: Terminal;
  private fit = new FitAddon();
  private ws: WebSocket | null = null;
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
  private readonly lockChangeCallbacks = new Set<(locked: boolean) => void>();

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
  ) {
    const prefs = loadTermPrefs();
    this.term = new Terminal({
      fontFamily: resolveFontFamily(prefs),
      fontSize: resolveFontSize(prefs),
      lineHeight: 1.15,
      cursorBlink: true,
      allowProposedApi: true,
      scrollback: 5000,
      theme: {
        background: '#0b0e14',
        foreground: '#d6deeb',
        cursor: '#5ccfe6',
        cursorAccent: '#0b0e14',
        selectionBackground: '#2a3345',
        black: '#1c2430',
        red: '#ef6b73',
        green: '#7fd88f',
        yellow: '#e5c07b',
        blue: '#61afef',
        magenta: '#c678dd',
        cyan: '#5ccfe6',
        white: '#d6deeb',
        brightBlack: '#5a6478',
        brightRed: '#ef6b73',
        brightGreen: '#7fd88f',
        brightYellow: '#e5c07b',
        brightBlue: '#61afef',
        brightMagenta: '#c678dd',
        brightCyan: '#5ccfe6',
        brightWhite: '#ffffff',
      },
    });
    this.term.loadAddon(this.fit);
    this.term.open(host);
    try {
      this.term.loadAddon(new WebglAddon());
    } catch {
      /* WebGL 不可用时回退 canvas 渲染 */
    }

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

    // pointer 类型检测：coarse → 默认锁定；fine → 恒解锁无锁定 UI。
    // 仅外接键盘不改变 pointer 语义（matchMedia('pointer') 描述主指针设备）。
    this.pointerCoarse = this.detectPointerCoarse();
    if (this.pointerCoarse) {
      // 初始无焦点，无需 blur；直接置门禁标志 + 挂手势层。
      this.lockController.lock();
      this.attachGestures();
    }
    if (typeof window !== 'undefined' && typeof window.matchMedia === 'function') {
      this.pointerMql = window.matchMedia('(pointer: coarse)');
      this.pointerMql.addEventListener('change', this.onPointerChange);
    }
  }

  connect(): void {
    if (this.disposed) return;
    this.clearTimer();
    this.closedByUs = false;
    this.closeSocket();
    this.setState(this.retry > 0 ? 'reconnecting' : 'connecting');

    const ws = new WebSocket(wsURL(this.wsPath));
    ws.binaryType = 'arraybuffer';
    this.ws = ws;
    this.authed = false;

    ws.onopen = () => {
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
      if (typeof ev.data === 'string') {
        try {
          const msg = JSON.parse(ev.data) as { type?: string };
          if (msg.type === 'auth_ok') {
            // 任何 WS 连接建立/auth_ok（含重连、Tab 切换）→ coarse 强制 LOCKED。
            // 必须先于 authed/connected 状态暴露与任何外部回调/fit，防门禁未就绪窗口泄漏。
            this.lockOrchestrator.onAuthOk(this.pointerCoarse, () => {
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
      this.term.write(new Uint8Array(ev.data as ArrayBuffer));
    };
    ws.onerror = () => {
      /* onclose 随后触发，统一处理 */
    };
    ws.onclose = (ev: CloseEvent) => {
      this.authed = false;
      this.ws = null;
      if (this.disposed || this.closedByUs) return;
      switch (ev.code) {
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

  /** attach 触控手势层（仅 coarse pointer 启用）。重复 attach 前先 dispose 旧实例。 */
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
   * visualViewport 键盘适配（design D6）：UNLOCKED 且 textarea 聚焦时监听 resize+scroll（rAF 去抖），
   * 按 max(0, vv.offsetTop + vv.height - wrap.getBoundingClientRect().top) 设 wrap maxHeight → fitNow → WS resize。
   * blur/锁定/卸载移除内联样式并 refit；visualViewport API 缺失跳过。
   */
  private updateVisualViewportListener(): void {
    const vv = typeof window !== 'undefined' ? window.visualViewport : undefined;
    if (!vv) return;
    const shouldListen =
      !this.disposed && !this.lockController.isLocked() && this.term.textarea === document.activeElement;
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

  private fitForViewport(): void {
    const vv = window.visualViewport;
    if (!vv || this.disposed) return;
    const top = this.wrap.getBoundingClientRect().top;
    const visibleBottom = vv.offsetTop + vv.height;
    this.wrap.style.maxHeight = Math.max(0, visibleBottom - top) + 'px';
    this.fitNow();
  }

  private detectPointerCoarse(): boolean {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return false;
    return window.matchMedia('(pointer: coarse)').matches;
  }

  /**
   * pointer 类型动态变化重评估（design D5）：委托 LockOrchestrator。
   * 转 coarse → lock + attach 手势层；转 fine → detach 手势层 + unlockSilently（不 focus）。
   * 仅外接键盘不改变 pointer，不触发本回调。
   */
  private onPointerChange = (ev: MediaQueryListEvent): void => {
    this.pointerCoarse = ev.matches;
    this.lockOrchestrator.onPointerChange(ev.matches);
  };
}
