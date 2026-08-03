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

  constructor(
    private host: HTMLElement,
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
    this.term.onData((d) => {
      if (this.ws?.readyState === WebSocket.OPEN && this.authed) {
        this.ws.send(encoder.encode(d));
      }
    });

    this.observer = new ResizeObserver(() => this.scheduleFit());
    this.observer.observe(host);
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
            this.authed = true;
            this.retry = 0;
            this.setState('connected');
            this.fitNow(); // 以握手尺寸再校准一次
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
    this.term.dispose();
  }

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
}
