// notifications.ts 通知 SSE 订阅与系统通知展示（task-notifications Lane D；
// spec「网页通知渠道」、design D7）。
//
// 独立轻量订阅：不复用 sse.ts 通用解析器（其只接受 snapshot/update 帧）。
// 仅在 web 启用且 Notification.permission === 'granted' 时连接（配置由调用方
// 拉取判定）；断线退避重连不重放（服务端语义——新连接只收到注册后的意图）。
// 收到帧 new Notification(title, {body, tag: task_id})，点击聚焦窗口并导航到帧内
// url 对应的目标页（hash 路由深链，仅取 #/ 起始部分）。

import { clearToken, getToken, UNAUTHORIZED_EVENT } from './api';
import { navigate } from './router';
import type { NotificationIntent } from './types';

/** 配置保存成功或浏览器通知权限授权成功后派发，App 根部据此重读配置并重建订阅。 */
export const NOTIFICATION_CONFIG_CHANGED_EVENT = 'notification-config-changed';

/** 跨标签同步 nonce（沿 hooks.ts 主题 storage 模式）：同页不触发 storage，异页触发。 */
export const NOTIFICATION_CONFIG_SYNC_KEY = 'ocdeck.notification-config-sync';

export function emitNotificationConfigChanged(): void {
  window.dispatchEvent(new Event(NOTIFICATION_CONFIG_CHANGED_EVENT));
  try {
    localStorage.setItem(NOTIFICATION_CONFIG_SYNC_KEY, String(Date.now()));
  } catch {
    /* localStorage 不可用时仍派发本页事件 */
  }
}

/** 本页自定义事件 + 异页 storage 事件。 */
export function subscribeNotificationConfigChanged(cb: () => void): () => void {
  const onLocal = () => cb();
  const onStorage = (e: StorageEvent) => {
    if (e.key === NOTIFICATION_CONFIG_SYNC_KEY) cb();
  };
  window.addEventListener(NOTIFICATION_CONFIG_CHANGED_EVENT, onLocal);
  window.addEventListener('storage', onStorage);
  return () => {
    window.removeEventListener(NOTIFICATION_CONFIG_CHANGED_EVENT, onLocal);
    window.removeEventListener('storage', onStorage);
  };
}

/** 重连退避：1s 起步 ×2，上限 30s。 */
const BACKOFF_INITIAL_MS = 1000;
const BACKOFF_MAX_MS = 30000;

/** 从帧 url 提取 hash 路由目标："<base>/#/task/x" → "/task/x"。
 *  非 hash 深链（缺 "#/"）返回 null（不导航，仍聚焦窗口）。 */
export function hashRouteFromURL(url: string): string | null {
  const idx = url.indexOf('#/');
  if (idx === -1) return null;
  return url.slice(idx + 1);
}

/** 校验帧意图：七字段齐全且均为字符串才派发（协议错误帧整体忽略，不断连）。 */
export function isNotificationIntent(data: unknown): data is NotificationIntent {
  if (typeof data !== 'object' || data === null) return false;
  const v = data as Record<string, unknown>;
  return (
    typeof v.task_id === 'string' &&
    typeof v.task_name === 'string' &&
    typeof v.category === 'string' &&
    typeof v.level === 'string' &&
    typeof v.title === 'string' &&
    typeof v.body === 'string' &&
    typeof v.url === 'string'
  );
}

/** 展示一条系统通知：tag 以任务标识收敛（支持 tag 的浏览器同任务替换）；
 *  点击聚焦窗口并导航到帧内 url 的目标页。 */
export function showNotification(in_: NotificationIntent, env?: NotificationEnv): void {
  const e = env ?? defaultNotificationEnv;
  const n = e.create(in_.title, { body: in_.body, tag: in_.task_id });
  n.onclick = () => {
    e.focus();
    const target = hashRouteFromURL(in_.url);
    if (target !== null) e.navigate(target);
    n.close();
  };
}

/** Notification 构造/窗口聚焦/导航的可注入环境（测试用）。 */
export interface NotificationEnv {
  create(title: string, options: NotificationOptions): Notification;
  focus(): void;
  navigate(path: string): void;
}

const defaultNotificationEnv: NotificationEnv = {
  create: (title, options) => new Notification(title, options),
  focus: () => window.focus(),
  navigate: (path) => navigate(path),
};

/** 订阅选项。 */
export interface SubscribeNotificationsOptions {
  onIntent(intent: NotificationIntent): void;
  /** 连接/协议错误（可选；通知流静默重连，默认不报错不打扰）。 */
  onError?(message: string): void;
}

/** 订阅通知 SSE 流：fetch + Bearer + ReadableStream 手动分帧（仅 event:
 *  notification 帧），指数退避重连。close() 为永久终态。返回 close 句柄。 */
export function subscribeNotifications(
  opts: SubscribeNotificationsOptions,
): { close(): void } {
  let closed = false;
  let backoffMs = BACKOFF_INITIAL_MS;
  let abortCtrl: AbortController | null = null;
  let backoffTimer: ReturnType<typeof setTimeout> | null = null;

  const scheduleReconnect = () => {
    if (closed) return;
    const delay = backoffMs;
    backoffMs = Math.min(backoffMs * 2, BACKOFF_MAX_MS);
    backoffTimer = setTimeout(() => {
      backoffTimer = null;
      if (closed) return;
      void loop();
    }, delay);
  };

  /** 分帧读取：空行分帧、event: notification 的 data 行解析派发；
   *  非 JSON/校验失败帧静默忽略（不断连——与通用流不同，通知流不重置退避策略）。 */
  const readStream = async (body: ReadableStream<Uint8Array>): Promise<void> => {
    const reader = body.getReader();
    const decoder = new TextDecoder();
    let buf = '';
    let eventName = '';
    const dataLines: string[] = [];

    const dispatchFrame = () => {
      if (eventName === 'notification' && dataLines.length > 0) {
        let data: unknown;
        try {
          data = JSON.parse(dataLines.join('\n'));
        } catch {
          return; // 非法 JSON 帧忽略
        }
        if (isNotificationIntent(data)) {
          backoffMs = BACKOFF_INITIAL_MS; // 有效帧到达 → 重置退避
          opts.onIntent(data);
        }
      }
    };

    const consumeLine = (line: string) => {
      if (line === '') {
        dispatchFrame();
        eventName = '';
        dataLines.length = 0;
        return;
      }
      if (line.startsWith(':')) return; // 注释行（建连确认/心跳）
      if (line.startsWith('event:')) {
        eventName = line.slice('event:'.length).replace(/^ /, '');
      } else if (line.startsWith('data:')) {
        dataLines.push(line.slice('data:'.length).replace(/^ /, ''));
      }
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
          consumeLine(line);
          nl = buf.indexOf('\n');
        }
      }
    } catch {
      /* 读错误走统一重连 */
    }
  };

  const loop = async (): Promise<void> => {
    if (closed) return;
    const ctrl = new AbortController();
    abortCtrl = ctrl;
    try {
      const res = await fetch('/api/v1/notifications/stream', {
        headers: { Authorization: `Bearer ${getToken()}` },
        signal: ctrl.signal,
      });
      if (closed) return;
      if (res.status === 401) {
        // 与 request() 401 同路：清 token + 全局事件，停止重连。
        clearToken();
        window.dispatchEvent(new Event(UNAUTHORIZED_EVENT));
        return;
      }
      if (!res.ok || !res.body) {
        opts.onError?.(`通知流连接失败（HTTP ${res.status}）`);
        scheduleReconnect();
        return;
      }
      await readStream(res.body);
      if (closed) return;
      // 正常结束（服务端关停/断流）→ 退避重连。
      scheduleReconnect();
    } catch {
      if (closed) return;
      opts.onError?.('通知流连接中断');
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