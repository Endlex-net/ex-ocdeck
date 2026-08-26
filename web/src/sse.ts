import { clearToken, getToken, UNAUTHORIZED_EVENT } from './api';
import type { ActiveSessionItem } from './types';

/** 连接状态（连接状态 UI 用）：connecting = 连接/重连尝试中；open = 已收到有效帧。 */
export type ActiveSessionsConnState = 'connecting' | 'open';

export interface SubscribeActiveSessionsOptions {
  /** snapshot 与 update 同构（design D5）：data 均为 ActiveSessionItem 裸数组，整表替换。 */
  onData(sessions: ActiveSessionItem[]): void;
  /** 连接/协议错误。本层不回调 onData 即保留旧数据；401 不走此通道。 */
  onError(message: string): void;
  /** 可选：连接状态变化（connecting ↔ open）。 */
  onStateChange?(state: ActiveSessionsConnState): void;
}

const STREAM_PATH = '/api/v1/sessions/active/stream';
/** 指数退避（design D5）：1s 起步 ×2，上限 30s；首个有效帧到达后重置为初始值。 */
const BACKOFF_INITIAL_MS = 1000;
const BACKOFF_MAX_MS = 30000;

/**
 * 订阅 sessions/active SSE 流（design D5）。
 * fetch + Bearer（复用 api.ts 的 getToken/401 通道）+ ReadableStream 手动分帧。
 * 断线/非 401 错误后指数退避重连；协议错误（非法 JSON/非数组 data）保留旧数据、
 * 终止当前连接并退避重连且不重置退避。
 * close() 为永久终态：置 closed 标志、中断当前 fetch、取消在途退避计时器；
 * 此后所有恢复点（各 await 之后、退避 timer 回调、loop 入口、setState/onData/
 * onError 同步回调 close() 后的继续执行）都不再发起 fetch、创建新 timer、
 * 安排重连或重复派发回调。
 */
export function subscribeActiveSessions(
  opts: SubscribeActiveSessionsOptions,
): { close(): void } {
  let closed = false;
  let backoffMs = BACKOFF_INITIAL_MS;
  let abortCtrl: AbortController | null = null;
  let backoffTimer: ReturnType<typeof setTimeout> | null = null;
  let connState: ActiveSessionsConnState | null = null;

  const setState = (s: ActiveSessionsConnState) => {
    if (s === connState) return;
    connState = s;
    opts.onStateChange?.(s);
  };

  /** 用当前退避值安排重连，并为下一次失败翻倍（首个有效帧到达时重置）。
   *  永久关闭契约：closed 后不再创建任何新 timer——含 onError 同步调用 close() 的场景
   *  （onError 返回后本函数立即被调，入口 guard 拦截）。 */
  const scheduleReconnect = () => {
    if (closed) return;
    const delay = backoffMs;
    backoffMs = Math.min(backoffMs * 2, BACKOFF_MAX_MS);
    backoffTimer = setTimeout(() => {
      backoffTimer = null;
      if (closed) return; // close() 发生在退避等待期间：不再发起下一轮 fetch
      void loop();
    }, delay);
  };

  const httpErrorMessage = async (res: Response): Promise<string> => {
    const text = await res.text().catch(() => '');
    try {
      const errObj = (JSON.parse(text) as { error?: { code?: string; message?: string } })
        .error;
      if (errObj?.message) return `[${errObj.code ?? 'unknown'}] ${errObj.message}`;
    } catch {
      /* 非 JSON 错误体 */
    }
    return `请求失败（HTTP ${res.status}）`;
  };

  type StreamOutcome = 'ended' | 'invalid' | 'read-error' | 'aborted';

  /** 分帧解析（event:/data:/注释行；跨 chunk 粘包按行缓冲，空行分帧）。
   *  协议错误时已 onError + abort，返回 'invalid'（不重置退避——由调用处沿用当前值）。 */
  const readStream = async (
    body: ReadableStream<Uint8Array>,
    ctrl: AbortController,
  ): Promise<StreamOutcome> => {
    const reader = body.getReader();
    const decoder = new TextDecoder();
    let buf = '';
    let eventName = '';
    const dataLines: string[] = [];
    let invalidFrame = false;

    const dispatchFrame = (): boolean => {
      if (closed) return true; // 回调内 close() 后同 chunk 剩余帧不再派发（onData/onStateChange 静默）
      if (eventName !== 'snapshot' && eventName !== 'update') return true; // 未知 event 忽略
      if (dataLines.length === 0) return true;
      let data: unknown;
      try {
        data = JSON.parse(dataLines.join('\n'));
      } catch {
        return false;
      }
      if (!Array.isArray(data)) return false;
      backoffMs = BACKOFF_INITIAL_MS; // 首个有效帧到达 → 重置退避
      setState('open');
      opts.onData(data as ActiveSessionItem[]);
      return true;
    };

    const consumeLine = (line: string): boolean => {
      if (line === '') {
        // 空行 = 帧边界：派发后清空累积（无论派发成败都复位解析状态）
        const ok = dispatchFrame();
        eventName = '';
        dataLines.length = 0;
        return ok;
      }
      if (line.startsWith(':')) return true; // 注释行（`: ping` 心跳）
      if (line.startsWith('event:')) {
        eventName = line.slice('event:'.length).replace(/^ /, '');
      } else if (line.startsWith('data:')) {
        dataLines.push(line.slice('data:'.length).replace(/^ /, ''));
      }
      return true; // 其余未知字段行忽略
    };

    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });
        let nl = buf.indexOf('\n');
        while (nl !== -1) {
          const line = buf.slice(0, nl).replace(/\r$/, '');
          buf = buf.slice(nl + 1);
          nl = buf.indexOf('\n');
          if (!consumeLine(line)) {
            invalidFrame = true;
            break;
          }
        }
        if (invalidFrame) break;
      }
    } catch {
      if (ctrl.signal.aborted) return 'aborted';
      opts.onError('活跃会话连接中断');
      return 'read-error';
    }
    if (invalidFrame) {
      // 协议错误：保留旧数据（不回调 onData）、终止当前连接、退避重连且不重置退避
      opts.onError('活跃会话推送数据格式错误');
      ctrl.abort();
      return 'invalid';
    }
    return 'ended';
  };

  const loop = async (): Promise<void> => {
    if (closed) return;
    setState('connecting');
    // onStateChange 回调可能同步调用 close()：恢复后不得继续创建 controller / 发起 fetch
    if (closed) return;
    const ctrl = new AbortController();
    abortCtrl = ctrl;
    try {
      const res = await fetch(STREAM_PATH, {
        headers: { Authorization: `Bearer ${getToken()}` },
        signal: ctrl.signal,
      });
      if (closed) return;
      if (res.status === 401) {
        // 与 request() 401 同路（api.ts）：清 token + 全局事件，停止重连
        clearToken();
        window.dispatchEvent(new Event(UNAUTHORIZED_EVENT));
        return;
      }
      if (!res.ok || !res.body) {
        const message = await httpErrorMessage(res); // close() 可能发生在此 await 期间
        if (closed) return; // 永久关闭：不回调 onError、不安排重连
        opts.onError(message);
        scheduleReconnect();
        return;
      }
      const outcome = await readStream(res.body, ctrl);
      if (closed || outcome === 'aborted') return;
      // 正常结束（服务端关停/断流）与协议错误都退避重连
      scheduleReconnect();
    } catch {
      if (closed) return;
      opts.onError('无法连接服务端（ocdeck-server 未运行？）');
      scheduleReconnect();
    }
  };

  void loop();

  return {
    close() {
      closed = true;
      if (backoffTimer !== null) {
        clearTimeout(backoffTimer);
        backoffTimer = null;
      }
      abortCtrl?.abort();
    },
  };
}
