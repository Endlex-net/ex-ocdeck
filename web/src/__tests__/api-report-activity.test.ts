// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { api } from '../api';

/* ==================== idle-reminder-user-activity 6.1：activity 上报端点封装 ====================
 * design D4：POST /api/v1/tasks/{id}/activity、空请求体；fire-and-forget——
 * 忽略响应与错误（含 401，不触发登出事件），不等待、不重试。 */

type FetchLike = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

// localStorage stub：jsdom 在 vitest + Node 26 实验性 localStorage 下可能不暴露
// globalThis.localStorage（同 App.notification-stream.test.tsx 模式）。
let savedLocalStorage: Storage | undefined;
const store = new Map<string, string>();

beforeEach(() => {
  savedLocalStorage = globalThis.localStorage;
  store.clear();
  store.set('ocdeck.token', 'fake-token');
  (globalThis as { localStorage: Storage }).localStorage = {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => void store.set(k, String(v)),
    removeItem: (k: string) => void store.delete(k),
    clear: () => store.clear(),
    key: (i: number) => Array.from(store.keys())[i] ?? null,
    get length() {
      return store.size;
    },
  } as Storage;
});

afterEach(() => {
  vi.unstubAllGlobals();
  if (savedLocalStorage === undefined) {
    delete (globalThis as { localStorage?: Storage }).localStorage;
  } else {
    (globalThis as { localStorage: Storage }).localStorage = savedLocalStorage;
  }
});

describe('api.reportActivity（fire-and-forget）', () => {
  it('POST /tasks/{id}/activity，空请求体，同步返回不等待', () => {
    const fetchMock = vi.fn<FetchLike>(async () => ({ ok: true, status: 204 }) as Response);
    vi.stubGlobal('fetch', fetchMock);
    const ret = api.reportActivity('t9');
    expect(ret).toBeUndefined(); // 同步返回，调用方无可等待句柄
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe('/api/v1/tasks/t9/activity');
    expect(init?.method).toBe('POST');
    expect(init?.body).toBeUndefined(); // 空请求体
    expect((init?.headers as Record<string, string>).Authorization).toBe('Bearer fake-token');
  });

  it('网络失败被吞掉：不抛错、无未处理 rejection、不重试', async () => {
    const fetchMock = vi.fn<FetchLike>(async () => {
      throw new TypeError('network down');
    });
    vi.stubGlobal('fetch', fetchMock);
    expect(() => api.reportActivity('t9')).not.toThrow();
    await new Promise((resolve) => setTimeout(resolve, 0)); // 让 rejection 进入 .catch
    expect(fetchMock).toHaveBeenCalledTimes(1); // 不重试
  });

  it.each([401, 500])(
    '非 2xx（%i）响应同样忽略：不触发登出副作用、不重试',
    async (status) => {
      const fetchMock = vi.fn<FetchLike>(async () => ({ ok: false, status }) as Response);
      vi.stubGlobal('fetch', fetchMock);
      const unauthListener = vi.fn();
      window.addEventListener('ocdeck:unauthorized', unauthListener);
      api.reportActivity('t9');
      await new Promise((resolve) => setTimeout(resolve, 0));
      expect(unauthListener).not.toHaveBeenCalled();
      expect(fetchMock).toHaveBeenCalledTimes(1); // 不重试
      window.removeEventListener('ocdeck:unauthorized', unauthListener);
    },
  );
});
