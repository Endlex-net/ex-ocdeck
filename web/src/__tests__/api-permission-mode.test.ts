// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { api } from '../api';

/* ==================== createTask 请求体 permission_mode 映射（task-permission-mode P3 gate F1） ====================
 * 走真实 api.createTask（仅 mock fetch），直接保护 api.ts 的 JSON 字段映射：
 * - 缺省（表单对 ask 映射为不传参）→ 请求体无 permission_mode 字段（后端 presence 语义）；
 * - all-approve / ai-auto → 以 snake_case permission_mode 原样发送；
 * - 既有 presence 契约不回退：base_ref/mode 缺省仍不携带。 */

const fetchMock = vi.fn();
vi.stubGlobal('fetch', fetchMock);

// localStorage stub：jsdom 在 vitest + Node 实验性 localStorage 下不暴露 globalThis.localStorage
// （同 server-status-banner.test.ts 模式）；api.getToken 仅读取，给空 Map 后端即可。
vi.stubGlobal('localStorage', {
  getItem: () => null,
  setItem: () => {},
  removeItem: () => {},
  clear: () => {},
  key: () => null,
  get length() {
    return 0;
  },
});

function okResponse() {
  return new Response(JSON.stringify({ id: 't1' }), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  });
}

/** 最近一次 fetch 调用的 [url, init] 与解析后的请求体。 */
function lastCall(): { url: string; init: RequestInit; body: Record<string, unknown> } {
  const call = fetchMock.mock.calls[fetchMock.mock.calls.length - 1] as [string, RequestInit];
  const [url, init] = call;
  return { url, init, body: JSON.parse(String(init.body)) as Record<string, unknown> };
}

beforeEach(() => {
  fetchMock.mockReset();
  fetchMock.mockResolvedValue(okResponse());
});

describe('api.createTask permission_mode 请求体映射（P3 gate F1）', () => {
  it('缺省（不传 permissionMode）→ 请求体无 permission_mode 字段，base_ref/mode 同缺省', async () => {
    await api.createTask('p1', 'task-a');
    const { url, init, body } = lastCall();
    expect(url).toBe('/api/v1/projects/p1/tasks');
    expect(init.method).toBe('POST');
    expect(body).toEqual({ name: 'task-a' });
    expect('permission_mode' in body).toBe(false);
    expect('base_ref' in body).toBe(false);
    expect('mode' in body).toBe(false);
  });

  it('worktree 缺省路径（仅 baseRef）→ 仍无 permission_mode 字段', async () => {
    await api.createTask('p1', 'task-a', 'main');
    const { body } = lastCall();
    expect(body).toEqual({ name: 'task-a', base_ref: 'main' });
    expect('permission_mode' in body).toBe(false);
  });

  it('all-approve → 以 snake_case permission_mode 原样发送', async () => {
    await api.createTask('p1', 'task-a', 'main', undefined, 'all-approve');
    const { body } = lastCall();
    expect(body.permission_mode).toBe('all-approve');
    expect(body).toEqual({ name: 'task-a', base_ref: 'main', permission_mode: 'all-approve' });
  });

  it('ai-auto → 以 snake_case permission_mode 原样发送（local-path 组合下 mode/base_ref 语义不回退）', async () => {
    await api.createTask('p1', 'task-a', undefined, 'local-path', 'ai-auto');
    const { body } = lastCall();
    expect(body).toEqual({ name: 'task-a', mode: 'local-path', permission_mode: 'ai-auto' });
    expect('base_ref' in body).toBe(false);
  });
});
