import { clearToken, getToken, UNAUTHORIZED_EVENT } from './api';
import type { ActiveSessionItem, Project, Task } from './types';

/** 连接状态（连接状态 UI 用）：connecting = 连接/重连尝试中；open = 已收到有效帧。 */
export type StreamConnState = 'connecting' | 'open';
/** 兼容别名（tasks/active 订阅方沿用旧名）。 */
export type ActiveSessionsConnState = StreamConnState;

/** 帧 data 校验（JSON 解析后调用）：合法返回条目数组，协议错误返回 null。 */
export type StreamDataValidator<T> = (data: unknown) => T[] | null;

export interface SubscribeStreamOptions<T> {
  onData(items: T[]): void;
  /** 连接/协议错误。本层不回调 onData 即保留旧数据；401 不走此通道。 */
  onError(message: string): void;
  /** 可选：连接状态变化（connecting ↔ open）。 */
  onStateChange?(state: StreamConnState): void;
  /** 可选：HTTP 404 永久终态（任务详情 gone）。提供时不 onError、不重连。 */
  onGone?(): void;
  /** 可选：ReadableStream 正常 EOF 也走 onError（任务详情）。缺省 false，仅重连不报错。 */
  reportEndAsError?: boolean;
  /** 帧 data 校验：合法返回条目数组，协议错误返回 null。缺省仅要求 JSON 数组。 */
  validate?: StreamDataValidator<T>;
  /** 连接中断/帧格式错误文案的场景名词（如「活跃会话」「项目」）。 */
  errorLabel: string;
}

/** 缺省校验：data 为 JSON 数组即接受（snapshot/update 均为裸数组）。 */
const asArray = <T>(data: unknown): T[] | null => (Array.isArray(data) ? (data as T[]) : null);

/** 指数退避（design D5）：1s 起步 ×2，上限 30s；首个有效帧到达后重置为初始值。 */
const BACKOFF_INITIAL_MS = 1000;
const BACKOFF_MAX_MS = 30000;

/**
 * 通用 SSE 流订阅（fetch + Bearer + ReadableStream 手动分帧 + 指数退避重连；
 * projects-stream design D6 参数化：订阅路径 + 帧数据校验，多场景共用同一实现）。
 * 协议错误（非法 JSON / 校验失败）保留旧数据、终止当前连接并退避重连且不重置退避。
 * close() 为永久终态：置 closed 标志、中断当前 fetch、取消在途退避计时器；
 * 此后所有恢复点（各 await 之后、退避 timer 回调、loop 入口、setState/onData/
 * onError 同步回调 close() 后的继续执行）都不再发起 fetch、创建新 timer、
 * 安排重连或重复派发回调。
 */
export function subscribeStream<T>(
  path: string,
  opts: SubscribeStreamOptions<T>,
): { close(): void } {
  const validate = opts.validate ?? asArray<T>;
  let closed = false;
  let backoffMs = BACKOFF_INITIAL_MS;
  let abortCtrl: AbortController | null = null;
  let backoffTimer: ReturnType<typeof setTimeout> | null = null;
  let connState: StreamConnState | null = null;

  const setState = (s: StreamConnState) => {
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
      const items = validate(data);
      if (items === null) return false;
      backoffMs = BACKOFF_INITIAL_MS; // 首个有效帧到达 → 重置退避
      setState('open');
      opts.onData(items);
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
      opts.onError(`${opts.errorLabel}连接中断`);
      return 'read-error';
    }
    if (invalidFrame) {
      // 协议错误：保留旧数据（不回调 onData）、终止当前连接、退避重连且不重置退避
      opts.onError(`${opts.errorLabel}推送数据格式错误`);
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
      const res = await fetch(path, {
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
      if (res.status === 404 && opts.onGone) {
        // 永久终态（与 401 同级）：先置 closed，避免 onGone 抛错落入 catch 后重连
        closed = true;
        opts.onGone();
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
      if (outcome === 'ended' && opts.reportEndAsError) {
        opts.onError(`${opts.errorLabel}连接中断`);
      }
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

export interface SubscribeActiveSessionsOptions {
  /** snapshot 与 update 同构（design D5）：data 均为 ActiveSessionItem 裸数组，整表替换。 */
  onData(sessions: ActiveSessionItem[]): void;
  /** 连接/协议错误。本层不回调 onData 即保留旧数据；401 不走此通道。 */
  onError(message: string): void;
  /** 可选：连接状态变化（connecting ↔ open）。 */
  onStateChange?(state: ActiveSessionsConnState): void;
}

/** 订阅 tasks/active SSE 流（design D5；projects-stream 改名，旧 sessions/active 路径不留）。 */
export function subscribeActiveSessions(
  opts: SubscribeActiveSessionsOptions,
): { close(): void } {
  return subscribeStream<ActiveSessionItem>('/api/v1/tasks/active/stream', {
    ...opts,
    errorLabel: '活跃会话',
  });
}

export interface SubscribeProjectsOptions {
  /** snapshot 与 update 同构（projects-stream design D6）：data 均为 Project 裸数组，整表替换。 */
  onData(projects: Project[]): void;
  onError(message: string): void;
  onStateChange?(state: StreamConnState): void;
}

/** 订阅 projects SSE 流（projects-stream design D6）：侧栏/指挥中心/项目管理页共享 store 数据源。 */
export function subscribeProjects(opts: SubscribeProjectsOptions): { close(): void } {
  return subscribeStream<Project>('/api/v1/projects/stream', {
    ...opts,
    errorLabel: '项目',
  });
}

export interface SubscribeTaskOptions {
  /** snapshot 与 update 同构：data 为单个 Task 对象，整对象替换。 */
  onData(task: Task): void;
  onError(message: string): void;
  onStateChange?(state: StreamConnState): void;
  /** HTTP 404：任务不存在/已删除，永久终态。 */
  onGone?(): void;
}

/** 订阅任务详情 SSE 流（task-detail-stream D5）：单对象帧，validate 仅校验信封形状。 */
export function subscribeTask(taskID: string, opts: SubscribeTaskOptions): { close(): void } {
  return subscribeStream<Task>(`/api/v1/tasks/${taskID}/stream`, {
    onError: opts.onError,
    onStateChange: opts.onStateChange,
    onGone: opts.onGone,
    reportEndAsError: true,
    errorLabel: '任务详情',
    validate: (data) =>
      typeof data === 'object' && data !== null && !Array.isArray(data) ? [data as Task] : null,
    onData: (items) => opts.onData(items[0]),
  });
}
