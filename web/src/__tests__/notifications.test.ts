import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// api 模块 mock：notifications.ts 只依赖 getToken/clearToken/UNAUTHORIZED_EVENT
// （同 sse.test.ts 模式，避免 node 环境引入 localStorage）。
vi.mock('../api', () => ({
  clearToken: vi.fn(),
  getToken: vi.fn(() => 'fake-token'),
  UNAUTHORIZED_EVENT: 'ocdeck:unauthorized-test',
}));

// router.navigate mock：断言通知点击导航目标。
vi.mock('../router', () => ({
  navigate: vi.fn(),
}));

import { clearToken, UNAUTHORIZED_EVENT } from '../api';
import { navigate } from '../router';
import {
  hashRouteFromURL,
  isNotificationIntent,
  showNotification,
  subscribeNotifications,
  type NotificationEnv,
} from '../notifications';
import type { NotificationIntent } from '../types';

const clearTokenMock = vi.mocked(clearToken);
const navigateMock = vi.mocked(navigate);

const encoder = new TextEncoder();

// window 垫片：仅需要 dispatchEvent（401 通道与 api.ts 同机制）。
const dispatchedEvents: string[] = [];
vi.stubGlobal('window', {
  dispatchEvent: (e: Event) => {
    dispatchedEvents.push(e.type);
    return true;
  },
});

const fetchMock = vi.fn();
vi.stubGlobal('fetch', fetchMock);

function intentFixture(): NotificationIntent {
  return {
    task_id: 'task-42',
    task_name: '构建服务',
    category: 'question',
    level: 'timeSensitive',
    title: '[构建服务] 等待你的回答',
    body: '构建服务\n用哪个分支？',
    url: 'http://127.0.0.1:18080/#/task/task-42',
  };
}

const frame = (event: string, data: string) => `event: ${event}\ndata: ${data}\n\n`;

/** 预置帧的 SSE 响应（流在 start 时全部入队并关闭）。 */
function sseResponse(frames: string[], status = 200): Response {
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const f of frames) controller.enqueue(encoder.encode(f));
      controller.close();
    },
  });
  return { status, ok: status === 200, body } as unknown as Response;
}

beforeEach(() => {
  fetchMock.mockReset();
  dispatchedEvents.length = 0;
  clearTokenMock.mockClear();
  navigateMock.mockClear();
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

describe('hashRouteFromURL', () => {
  it('从帧 url 提取 hash 路由目标', () => {
    expect(hashRouteFromURL('http://127.0.0.1:18080/#/task/t1')).toBe('/task/t1');
    expect(hashRouteFromURL('https://example.com/#/configs#notifications')).toBe(
      '/configs#notifications',
    );
  });

  it('非 hash 深链返回 null', () => {
    expect(hashRouteFromURL('https://example.com/tasks')).toBeNull();
    expect(hashRouteFromURL('')).toBeNull();
  });
});

describe('isNotificationIntent', () => {
  it('七字段齐全且为字符串 → 合法', () => {
    expect(isNotificationIntent(intentFixture())).toBe(true);
  });

  it('缺字段/类型错误 → 非法', () => {
    expect(isNotificationIntent(null)).toBe(false);
    expect(isNotificationIntent('x')).toBe(false);
    const partial = { ...intentFixture() } as Record<string, unknown>;
    delete partial.url;
    expect(isNotificationIntent(partial)).toBe(false);
    expect(isNotificationIntent({ ...intentFixture(), task_id: 42 })).toBe(false);
  });
});

describe('showNotification', () => {
  it('tag 为任务标识；点击聚焦并导航到帧内 url 的目标页', () => {
    const fakeNote = { onclick: null as ((this: Notification) => void) | null, close: vi.fn() };
    const env: NotificationEnv = {
      create: (title, options) => {
        expect(title).toBe('[构建服务] 等待你的回答');
        expect(options.tag).toBe('task-42');
        expect(options.body).toBe('构建服务\n用哪个分支？');
        return fakeNote as unknown as Notification;
      },
      focus: vi.fn(),
      navigate: navigateMock,
    };
    showNotification(intentFixture(), env);
    expect(fakeNote.onclick).toBeTypeOf('function');
    fakeNote.onclick?.call(fakeNote as unknown as Notification);
    expect(env.focus).toHaveBeenCalled();
    expect(navigateMock).toHaveBeenCalledWith('/task/task-42');
  });

  it('url 无 hash 深链时仅聚焦不导航', () => {
    const fakeNote = { onclick: null as ((this: Notification) => void) | null, close: vi.fn() };
    const env: NotificationEnv = {
      create: () => fakeNote as unknown as Notification,
      focus: vi.fn(),
      navigate: navigateMock,
    };
    showNotification({ ...intentFixture(), url: 'https://example.com/x' }, env);
    fakeNote.onclick?.call(fakeNote as unknown as Notification);
    expect(env.focus).toHaveBeenCalled();
    expect(navigateMock).not.toHaveBeenCalled();
  });
});

describe('subscribeNotifications', () => {
  /** fetch resolve + ReadableStream 多段 await 链在 fake timers 下的推进：
   *  分多轮 advance 让微任务队列充分排空。 */
  const drain = async () => {
    for (let i = 0; i < 6; i++) await vi.advanceTimersByTimeAsync(50);
  };

  it('仅派发 event: notification 帧；snapshot/未知 event 忽略', async () => {
    const onIntent = vi.fn();
    fetchMock.mockResolvedValueOnce(
      sseResponse([
        ': ping\n\n',
        frame('notification', JSON.stringify(intentFixture())),
        frame('snapshot', '[1,2]'),
        frame('update', '[]'),
      ]),
    );
    const sub = subscribeNotifications({ onIntent });
    await drain();
    expect(onIntent).toHaveBeenCalledTimes(1);
    expect(onIntent).toHaveBeenCalledWith(intentFixture());
    sub.close();
  });

  it('非法 JSON / 校验失败帧静默忽略（不断连、不派发）', async () => {
    const onIntent = vi.fn();
    fetchMock.mockResolvedValueOnce(
      sseResponse([frame('notification', '{bad json'), frame('notification', '{"task_id":"only-one-field"}')]),
    );
    const sub = subscribeNotifications({ onIntent });
    await drain();
    expect(onIntent).not.toHaveBeenCalled();
    sub.close();
  });

  it('401 → 清 token + 全局事件，停止重连', async () => {
    const onIntent = vi.fn();
    fetchMock.mockResolvedValueOnce({ status: 401, ok: false } as unknown as Response);
    const sub = subscribeNotifications({ onIntent });
    await drain();
    expect(dispatchedEvents).toContain(UNAUTHORIZED_EVENT);
    expect(clearTokenMock).toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(fetchMock).toHaveBeenCalledTimes(1); // 不再重连
    sub.close();
  });

  it('断流退避重连（不重放——新连接只收新帧）', async () => {
    const onIntent = vi.fn();
    fetchMock
      .mockResolvedValueOnce(sseResponse([frame('notification', JSON.stringify(intentFixture()))]))
      .mockResolvedValueOnce(
        sseResponse([frame('notification', JSON.stringify({ ...intentFixture(), task_id: 'task-2' }))]),
      );
    const sub = subscribeNotifications({ onIntent });
    await drain();
    expect(onIntent).toHaveBeenCalledTimes(1);
    // 流结束后 1s 退避重连。
    await vi.advanceTimersByTimeAsync(1100);
    await drain();
    expect(onIntent).toHaveBeenCalledTimes(2);
    expect(onIntent).toHaveBeenLastCalledWith({ ...intentFixture(), task_id: 'task-2' });
    sub.close();
  });

  it('close() 后不再 fetch、不再派发', async () => {
    const onIntent = vi.fn();
    fetchMock.mockResolvedValueOnce(sseResponse([frame('notification', JSON.stringify(intentFixture()))]));
    const sub = subscribeNotifications({ onIntent });
    sub.close();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(onIntent).not.toHaveBeenCalled();
  });
});