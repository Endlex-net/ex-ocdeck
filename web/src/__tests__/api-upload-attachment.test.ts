// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { api, ApiError, UNAUTHORIZED_EVENT, UPLOAD_TARGET_UNAVAILABLE_MESSAGE } from '../api';

/* ==================== terminal-file-paste-drop 5.2：uploadAttachment 上传契约 ====================
 * delivery spec 上传接口 requirement：POST /api/v1/tasks/{taskID}/attachments，
 * multipart FormData 顺序固定——先 connId 后唯一 file part；201 → {"uploadId":"32hex"}；
 * 404（业务 404 与旧 server 无路由不可区分）→ 固定降级文案；错误信封 {"error":{code,message}}；
 * 401 → 清 token + 登出事件；fetch 拒绝中的文件读取失效原样上抛（不吞成网络错误，R12）。 */

type FetchLike = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function jsonResponse(status: number, body: unknown): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    text: async () => JSON.stringify(body),
  } as unknown as Response;
}

// localStorage stub：jsdom 在 vitest + Node 26 实验性 localStorage 下可能不暴露
// globalThis.localStorage（同 api-report-activity.test.ts 模式）。
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

describe('api.uploadAttachment（multipart 上传契约）', () => {
  it('POST /tasks/{id}/attachments；FormData 先 connId 后 file；不手动设 Content-Type；携带 Bearer token', async () => {
    const fetchMock = vi.fn<FetchLike>(async () => jsonResponse(201, { uploadId: 'a'.repeat(32) }));
    vi.stubGlobal('fetch', fetchMock);
    const file = new File([new Uint8Array([1, 2])], 'shot.png', { type: 'image/png' });

    const res = await api.uploadAttachment('t7', file, '11111111-1111-4111-8111-111111111111');

    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe('/api/v1/tasks/t7/attachments');
    expect(init?.method).toBe('POST');
    expect((init?.headers as Record<string, string>).Authorization).toBe('Bearer fake-token');
    expect((init?.headers as Record<string, string>)['Content-Type']).toBeUndefined(); // boundary 由浏览器生成

    const form = init?.body as FormData;
    expect([...form.keys()]).toEqual(['connId', 'file']); // 顺序契约：首 part MUST 为 connId
    expect(form.get('connId')).toBe('11111111-1111-4111-8111-111111111111');
    expect((form.get('file') as File).name).toBe('shot.png');

    expect(res).toEqual({ uploadId: 'a'.repeat(32) });
  });

  it('404 → ApiError(404) 携带固定降级文案（业务 404 与旧 server 无路由不可区分）', async () => {
    const fetchMock = vi.fn<FetchLike>(async () =>
      jsonResponse(404, { error: { code: 'not_found', message: '任务不存在' } }),
    );
    vi.stubGlobal('fetch', fetchMock);
    await expect(
      api.uploadAttachment('t7', new File(['x'], 'a.txt'), 'c1'),
    ).rejects.toMatchObject({ status: 404, message: UPLOAD_TARGET_UNAVAILABLE_MESSAGE });
  });

  it('其他非 2xx → 透传错误信封 code/message（如 408 上传停滞）', async () => {
    const fetchMock = vi.fn<FetchLike>(async () =>
      jsonResponse(408, { error: { code: 'invalid_input', message: 'upload stalled for 1 hour' } }),
    );
    vi.stubGlobal('fetch', fetchMock);
    await expect(
      api.uploadAttachment('t7', new File(['x'], 'a.txt'), 'c1'),
    ).rejects.toBeInstanceOf(ApiError);
    await expect(
      api.uploadAttachment('t7', new File(['x'], 'a.txt'), 'c1'),
    ).rejects.toMatchObject({ status: 408, code: 'invalid_input', message: 'upload stalled for 1 hour' });
  });

  it('401 → 清 token + 派发登出事件 + ApiError 401', async () => {
    const fetchMock = vi.fn<FetchLike>(async () =>
      jsonResponse(401, { error: { code: 'unauthorized', message: '认证失败' } }),
    );
    vi.stubGlobal('fetch', fetchMock);
    const unauthListener = vi.fn();
    window.addEventListener(UNAUTHORIZED_EVENT, unauthListener);
    try {
      await expect(
        api.uploadAttachment('t7', new File(['x'], 'a.txt'), 'c1'),
      ).rejects.toMatchObject({ status: 401 });
      expect(unauthListener).toHaveBeenCalledTimes(1);
      expect(store.has('ocdeck.token')).toBe(false);
    } finally {
      window.removeEventListener(UNAUTHORIZED_EVENT, unauthListener);
    }
  });

  it('fetch 拒绝为文件读取失效（NotReadableError）→ 原样上抛保留 name，不包装成网络错误（R12）', async () => {
    const readErr = new DOMException('The source file could not be read', 'NotReadableError');
    const fetchMock = vi.fn<FetchLike>(async () => {
      throw readErr;
    });
    vi.stubGlobal('fetch', fetchMock);
    await expect(
      api.uploadAttachment('t7', new File(['x'], 'a.txt'), 'c1'),
    ).rejects.toBe(readErr);
  });

  it('fetch 普通网络拒绝 → 仍包装为 ApiError(0, network_error)（普通失败提示不变）', async () => {
    const fetchMock = vi.fn<FetchLike>(async () => {
      throw new TypeError('Failed to fetch');
    });
    vi.stubGlobal('fetch', fetchMock);
    await expect(
      api.uploadAttachment('t7', new File(['x'], 'a.txt'), 'c1'),
    ).rejects.toMatchObject({ status: 0, code: 'network_error' });
  });
});
