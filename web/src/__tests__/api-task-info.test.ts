// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { api, ApiError } from '../api';

/* ==================== task-info-editable API 包装（tasks.md 4.1） ====================
 * 走真实 api.updateTask / getBranchPrefix / putBranchPrefix / createTask（仅 mock fetch），
 * 直接保护 api.ts 的端点、方法与 JSON 字段映射（presence 语义）。 */

const fetchMock = vi.fn();
vi.stubGlobal('fetch', fetchMock);

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

function okJson(body: unknown) {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  });
}

function lastCall(): { url: string; init: RequestInit; body: Record<string, unknown> } {
  const call = fetchMock.mock.calls[fetchMock.mock.calls.length - 1] as [string, RequestInit];
  const [url, init] = call;
  return { url, init, body: init.body ? (JSON.parse(String(init.body)) as Record<string, unknown>) : {} };
}

beforeEach(() => {
  fetchMock.mockReset();
  // 工厂式响应：同一用例多次调用时 Response body 不可复用（Body has already been read）
  fetchMock.mockImplementation(() => Promise.resolve(okJson({ id: 't1' })));
});

describe('api.updateTask（PATCH /tasks/:id，presence 语义）', () => {
  it('name + branch_slug 同时提供 → 原样发送两字段', async () => {
    await api.updateTask('t1', { name: 'new-name', branch_slug: 'feat-x' });
    const { url, init, body } = lastCall();
    expect(url).toBe('/api/v1/tasks/t1');
    expect(init.method).toBe('PATCH');
    expect(body).toEqual({ name: 'new-name', branch_slug: 'feat-x' });
  });

  it('仅 name → 请求体无 branch_slug 字段', async () => {
    await api.updateTask('t1', { name: 'n' });
    const { body } = lastCall();
    expect(body).toEqual({ name: 'n' });
    expect('branch_slug' in body).toBe(false);
  });

  it('空 patch（{}）→ 发送空 JSON 对象（幂等同值读契约）', async () => {
    await api.updateTask('t1', {});
    const { init, body } = lastCall();
    expect(init.method).toBe('PATCH');
    expect(body).toEqual({});
  });

  it('成功响应解析为任务 DTO 返回', async () => {
    fetchMock.mockResolvedValue(okJson({ id: 't1', name: 'renamed', branch: 'ocdeck/feat-x' }));
    const t = await api.updateTask('t1', { name: 'renamed' });
    expect(t.name).toBe('renamed');
    expect(t.branch).toBe('ocdeck/feat-x');
  });

  it('服务端 422 invalid_input → ApiError 原样透出 code/message', async () => {
    fetchMock.mockResolvedValue(
      new Response(JSON.stringify({ error: { code: 'invalid_input', message: '任务名称不能为空' } }), {
        status: 422,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    const err = await api.updateTask('t1', { name: '  ' }).catch((e) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err.status).toBe(422);
    expect(err.code).toBe('invalid_input');
    expect(err.message).toBe('任务名称不能为空');
  });
});

describe('api 分支前缀（GET/PUT /config/branch-prefix）', () => {
  it('getBranchPrefix → GET 端点，返回 {prefix}', async () => {
    fetchMock.mockResolvedValue(okJson({ prefix: 'ocdeck' }));
    const c = await api.getBranchPrefix();
    const { url, init } = lastCall();
    expect(url).toBe('/api/v1/config/branch-prefix');
    expect(init.method).toBe('GET');
    expect(init.body).toBeUndefined();
    expect(c.prefix).toBe('ocdeck');
  });

  it('putBranchPrefix → PUT 同构 {prefix}，返回生效值', async () => {
    fetchMock.mockResolvedValue(okJson({ prefix: 'team-x' }));
    const c = await api.putBranchPrefix('team-x');
    const { url, init, body } = lastCall();
    expect(url).toBe('/api/v1/config/branch-prefix');
    expect(init.method).toBe('PUT');
    expect(body).toEqual({ prefix: 'team-x' });
    expect(c.prefix).toBe('team-x');
  });
});

describe('api.createTask branch_slug 映射（tasks.md 4.3）', () => {
  it('提供 branchSlug → 请求体携带 snake_case branch_slug', async () => {
    await api.createTask('p1', 'task-a', 'main', undefined, 'ai-auto', 'my-feature');
    const { body } = lastCall();
    expect(body).toEqual({
      name: 'task-a',
      base_ref: 'main',
      permission_mode: 'ai-auto',
      branch_slug: 'my-feature',
    });
  });

  it('缺省/空串 → 请求体无 branch_slug 字段（既有命名行为不变）', async () => {
    await api.createTask('p1', 'task-a', 'main', undefined, 'ai-auto');
    expect('branch_slug' in lastCall().body).toBe(false);
    await api.createTask('p1', 'task-a', 'main', undefined, 'ai-auto', '');
    expect('branch_slug' in lastCall().body).toBe(false);
  });
});
