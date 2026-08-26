import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// api 模块 mock：sse.ts 只依赖 getToken/clearToken/UNAUTHORIZED_EVENT（同 session-adapter.test.ts 模式），
// 避免 node 环境引入 localStorage。
vi.mock('../api', () => ({
  clearToken: vi.fn(),
  getToken: vi.fn(() => 'fake-token'),
  UNAUTHORIZED_EVENT: 'ocdeck:unauthorized-test',
}));

import { clearToken, getToken, UNAUTHORIZED_EVENT } from '../api';
import { subscribeActiveSessions, subscribeProjects } from '../sse';
import type { ActiveSessionItem } from '../types';

const clearTokenMock = vi.mocked(clearToken);
const getTokenMock = vi.mocked(getToken);

// window 垫片：仅需要 dispatchEvent（401 通道与 api.ts 同机制）
const dispatchedEvents: string[] = [];
vi.stubGlobal('window', {
  dispatchEvent: (e: Event) => {
    dispatchedEvents.push(e.type);
    return true;
  },
});

const fetchMock = vi.fn();
vi.stubGlobal('fetch', fetchMock);

const encoder = new TextEncoder();

function item(taskId: string): ActiveSessionItem {
  return {
    task_id: taskId,
    project_id: 'p1',
    project_name: 'proj',
    name: 't-' + taskId,
    branch: 'main',
    worktree_path: '/wt',
    last_active_at: 100,
  };
}

const frame = (event: string, data: string) => `event: ${event}\ndata: ${data}\n\n`;

/** 预置帧的 SSE 响应（流在 start 时全部入队并关闭）。 */
function sseResponse(frames: string[]): Response {
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const f of frames) controller.enqueue(encoder.encode(f));
      controller.close();
    },
  });
  return { status: 200, ok: true, body } as unknown as Response;
}

/** 手动可控流（跨 chunk 粘包测试：按任意切面 push）。 */
function controlledResponse(): {
  res: Response;
  push: (chunk: string) => void;
  close: () => void;
} {
  let streamCtrl: ReadableStreamDefaultController<Uint8Array> | null = null;
  const body = new ReadableStream<Uint8Array>({
    start(c) {
      streamCtrl = c;
    },
  });
  return {
    res: { status: 200, ok: true, body } as unknown as Response,
    push: (chunk: string) => streamCtrl?.enqueue(encoder.encode(chunk)),
    close: () => streamCtrl?.close(),
  };
}

/** HTTP 错误响应（401/500 等，正常 JSON 错误体，无 SSE 头）。 */
function errResponse(status: number, code: string, message: string): Response {
  return {
    status,
    ok: false,
    body: undefined,
    text: () => Promise.resolve(JSON.stringify({ error: { code, message } })),
  } as unknown as Response;
}

/** fetch 失败 reason（mockRejectedValue 每次调用才产生 rejection，避免预建 promise 未消费泄漏）。 */
const fetchFailed = () => new TypeError('fetch failed');

describe('sessions/active SSE 订阅（sse.ts，design D5）', () => {
  let onData: ReturnType<typeof vi.fn>;
  let onError: ReturnType<typeof vi.fn>;
  let onStateChange: ReturnType<typeof vi.fn>;

  const subscribe = () => subscribeActiveSessions({ onData, onError, onStateChange });

  const settle = () => vi.advanceTimersByTimeAsync(0);

  beforeEach(() => {
    vi.useFakeTimers();
    onData = vi.fn();
    onError = vi.fn();
    onStateChange = vi.fn();
    fetchMock.mockReset().mockRejectedValue(fetchFailed());
    clearTokenMock.mockClear();
    getTokenMock.mockClear();
    dispatchedEvents.length = 0;
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  describe('分帧解析', () => {
    it('跨 chunk 粘包：帧内容分片到达仍正确组帧', async () => {
      const ctl = controlledResponse();
      fetchMock.mockReset().mockResolvedValueOnce(ctl.res);
      const sub = subscribe();
      await settle();
      // 同一帧切成 3 片（event 名、data 中间、帧边界分隔各占一片）
      const data = JSON.stringify([item('a')]);
      ctl.push('event: snap');
      ctl.push(`shot\nda`);
      ctl.push(`ta: ${data}\n\n`);
      ctl.close();
      await settle();
      expect(onData).toHaveBeenCalledTimes(1);
      expect(onData).toHaveBeenCalledWith([item('a')]);
      sub.close();
    });

    it('注释行（`: ping` 心跳）与未知 event 名忽略，仅 snapshot/update 生效', async () => {
      fetchMock.mockReset().mockResolvedValueOnce(
        sseResponse([
          ': ping\n\n',
          'event: whatever\ndata: {"x":1}\n\n',
          'event: snapshot\ndata: ' + JSON.stringify([item('s')]) + '\n\n',
          'event: update\ndata: []\n\n',
        ]),
      );
      const sub = subscribe();
      await settle();
      expect(onData).toHaveBeenCalledTimes(2);
      expect(onData).toHaveBeenNthCalledWith(1, [item('s')]);
      expect(onData).toHaveBeenNthCalledWith(2, []);
      expect(onError).not.toHaveBeenCalled();
      sub.close();
    });

    it('携带 Bearer 头（与 request() 同一 token 源）', async () => {
      fetchMock.mockReset().mockResolvedValueOnce(sseResponse([frame('snapshot', '[]')]));
      const sub = subscribe();
      await settle();
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/v1/tasks/active/stream',
        expect.objectContaining({
          headers: { Authorization: 'Bearer fake-token' },
        }),
      );
      sub.close();
    });

    it('非法 JSON data → onError + 不回调 onData（保留旧数据）+ 退避重连且不重置退避', async () => {
      // 两次网络失败把退避抬到 4s；第三次连接收到非法帧 → 下一轮仍是 4s（未重置为 1s）
      fetchMock
        .mockRejectedValueOnce(fetchFailed())
        .mockRejectedValueOnce(fetchFailed())
        .mockResolvedValueOnce(sseResponse([frame('snapshot', 'not-json')]))
        .mockRejectedValue(fetchFailed());
      const sub = subscribe();
      await settle(); // fetch1 失败 → 1s 后重连（退避抬到 2s）
      await vi.advanceTimersByTimeAsync(1000); // fetch2 失败 → 2s 后重连（退避抬到 4s）
      await vi.advanceTimersByTimeAsync(2000); // fetch3 收到非法帧 → onError，按 4s 排程
      expect(onError).toHaveBeenCalledTimes(3);
      expect(onError).toHaveBeenLastCalledWith('活跃会话推送数据格式错误');
      expect(onData).not.toHaveBeenCalled();
      await vi.advanceTimersByTimeAsync(3000); // t=6s：若退避被重置为 1s，此处已发起第 4 次 fetch
      expect(fetchMock).toHaveBeenCalledTimes(3);
      await vi.advanceTimersByTimeAsync(1000); // t=7s：4s 退避到期
      expect(fetchMock).toHaveBeenCalledTimes(4);
      sub.close();
    });

    it('非数组 data → onError + 保留旧数据（此前有效帧不再被覆盖回调）', async () => {
      fetchMock.mockReset().mockResolvedValueOnce(
        sseResponse([
          frame('snapshot', JSON.stringify([item('a')])),
          frame('update', '{"not":"array"}'),
        ]),
      );
      const sub = subscribe();
      await settle();
      expect(onData).toHaveBeenCalledTimes(1); // 仅首个有效帧；坏帧不回调（调用方保留旧数据）
      expect(onError).toHaveBeenCalledTimes(1);
      sub.close();
    });
  });

  describe('401 处理（与 request() 同路）', () => {
    it('401 → clearToken + UNAUTHORIZED_EVENT，且永久停止重连', async () => {
      fetchMock.mockReset().mockResolvedValue(errResponse(401, 'unauthorized', '认证失败'));
      const sub = subscribe();
      await settle();
      expect(clearTokenMock).toHaveBeenCalledTimes(1);
      expect(dispatchedEvents).toEqual([UNAUTHORIZED_EVENT]);
      expect(onError).not.toHaveBeenCalled();
      expect(onData).not.toHaveBeenCalled();
      await vi.advanceTimersByTimeAsync(60000);
      expect(fetchMock).toHaveBeenCalledTimes(1); // 不再重连
      sub.close();
    });
  });

  describe('指数退避重连', () => {
    it('断线重连起步 1s；首个有效帧到达后重置退避', async () => {
      fetchMock
        .mockRejectedValueOnce(fetchFailed()) // t=0：失败 → 1s 后重连（退避抬到 2s）
        .mockRejectedValueOnce(fetchFailed()) // t=1s：失败 → 2s 后重连（退避抬到 4s）
        .mockResolvedValueOnce(sseResponse([frame('snapshot', '[]')])) // t=3s：有效帧 → 重置 1s；流结束 → 1s 后重连
        .mockResolvedValue(sseResponse([frame('update', '[]')]));
      const sub = subscribe();
      await settle();
      await vi.advanceTimersByTimeAsync(3000); // t=3s：第 3 次连接收到有效帧
      expect(fetchMock).toHaveBeenCalledTimes(3);
      expect(onData).toHaveBeenCalledTimes(1);
      await vi.advanceTimersByTimeAsync(1000); // t=4s：重置后 1s 到期即重连（未重置则需等到 t=7s）
      expect(fetchMock).toHaveBeenCalledTimes(4);
      sub.close();
    });

    it('连续失败退避翻倍并封顶 30s', async () => {
      const sub = subscribe();
      await settle(); // t=0 失败 → 1s
      await vi.advanceTimersByTimeAsync(1000); // t=1s 失败 → 2s
      await vi.advanceTimersByTimeAsync(2000); // t=3s 失败 → 4s
      await vi.advanceTimersByTimeAsync(4000); // t=7s 失败 → 8s
      await vi.advanceTimersByTimeAsync(8000); // t=15s 失败 → 16s
      await vi.advanceTimersByTimeAsync(16000); // t=31s 失败 → min(32,30)=30s
      expect(fetchMock).toHaveBeenCalledTimes(6);
      await vi.advanceTimersByTimeAsync(29000); // t=60s：30s 未到期
      expect(fetchMock).toHaveBeenCalledTimes(6);
      await vi.advanceTimersByTimeAsync(1000); // t=61s：封顶值到期
      expect(fetchMock).toHaveBeenCalledTimes(7);
      sub.close();
    });

    it('close() 在退避等待期间调用 → 不再发起 fetch', async () => {
      const sub = subscribe();
      await settle(); // 失败 → 1s 后重连
      await vi.advanceTimersByTimeAsync(500);
      sub.close();
      await vi.advanceTimersByTimeAsync(10000);
      expect(fetchMock).toHaveBeenCalledTimes(1);
      expect(onError).toHaveBeenCalledTimes(1);
    });

    it('close() 在 HTTP 错误体读取期间调用 → 不回调 onError、不建重连 timer、无后续 fetch', async () => {
      // 非延迟版 errResponse 的 text() 立即 resolve，构造 pending text() 才能覆盖
      // 「await httpErrorMessage 期间 close()」的恢复点竞态
      let resolveText!: (v: string) => void;
      const textPromise = new Promise<string>((r) => {
        resolveText = r;
      });
      fetchMock
        .mockReset()
        .mockResolvedValueOnce({
          status: 500,
          ok: false,
          body: undefined,
          text: () => textPromise,
        } as unknown as Response)
        .mockImplementation(() => Promise.reject(fetchFailed()));
      const sub = subscribe();
      await settle(); // loop 挂起在 httpErrorMessage 的 text() await
      sub.close();
      resolveText(JSON.stringify({ error: { code: 'internal', message: 'boom' } }));
      await settle();
      expect(onError).not.toHaveBeenCalled();
      // 覆盖 2-3 个退避间隔（1s/2s/4s）：fetch 次数保持初始 1 次
      await vi.advanceTimersByTimeAsync(7000);
      expect(fetchMock).toHaveBeenCalledTimes(1);
    });

    it('onError 同步调用 close() → 不再发起后续 fetch', async () => {
      fetchMock
        .mockReset()
        .mockRejectedValueOnce(fetchFailed())
        .mockImplementation(() => Promise.reject(fetchFailed()));
      let sub!: { close(): void };
      const syncCloseOnError = vi.fn(() => sub.close());
      sub = subscribeActiveSessions({ onData, onError: syncCloseOnError, onStateChange });
      await settle(); // 首次 fetch 失败 → onError 同步 close
      expect(syncCloseOnError).toHaveBeenCalledTimes(1);
      await vi.advanceTimersByTimeAsync(10000);
      expect(fetchMock).toHaveBeenCalledTimes(1);
    });

    it('onStateChange(connecting) 同步调用 close() → 不发起任何 fetch', async () => {
      // 可达路径：首次 connecting 发生在 subscribe 返回前（handle 不可用），
      // 重连时的 connecting 回调才是用户能同步 close() 的恢复点
      fetchMock
        .mockReset()
        .mockResolvedValueOnce(sseResponse([frame('snapshot', '[]')]))
        .mockImplementation(() => Promise.reject(fetchFailed()));
      let sub!: { close(): void };
      const states: string[] = [];
      let connectingCount = 0;
      sub = subscribeActiveSessions({
        onData,
        onError,
        onStateChange: (state) => {
          states.push(state);
          if (state === 'connecting') {
            connectingCount += 1;
            if (connectingCount > 1) sub.close(); // 仅重连的 connecting 同步关闭
          }
        },
      });
      await settle(); // 连接 1：connecting → open（首帧）→ 流结束 → 1s 退避
      expect(states).toEqual(['connecting', 'open']);
      await vi.advanceTimersByTimeAsync(1000); // timer → loop → setState('connecting') 回调内同步 close
      expect(states).toEqual(['connecting', 'open', 'connecting']);
      expect(connectingCount).toBe(2);
      // 覆盖 2 个退避间隔（2s/4s）：重连 fetch 从未发起
      await vi.advanceTimersByTimeAsync(6000);
      expect(fetchMock).toHaveBeenCalledTimes(1);
      expect(onError).not.toHaveBeenCalled();
    });

    it('close() 中断当前 fetch（AbortController abort）且不再重连', async () => {
      const ctl = controlledResponse();
      fetchMock.mockReset().mockResolvedValueOnce(ctl.res);
      const sub = subscribe();
      await settle();
      const signal = (fetchMock.mock.calls[0][1] as RequestInit).signal as AbortSignal;
      ctl.push(frame('snapshot', JSON.stringify([item('a')])));
      await settle();
      expect(onData).toHaveBeenCalledTimes(1);
      sub.close();
      await settle();
      expect(signal.aborted).toBe(true);
      await vi.advanceTimersByTimeAsync(5000);
      expect(fetchMock).toHaveBeenCalledTimes(1); // 卸载后不再发起新连接
    });
  });

  describe('projects preset（subscribeStream 参数化，projects-stream D6）', () => {
    it('请求 /api/v1/projects/stream 并携带 Bearer 头（与 tasks/active 同一核心）', async () => {
      fetchMock.mockReset().mockResolvedValueOnce(sseResponse([frame('snapshot', '[]')]));
      const sub = subscribeProjects({ onData, onError });
      await settle();
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/v1/projects/stream',
        expect.objectContaining({
          headers: { Authorization: 'Bearer fake-token' },
        }),
      );
      sub.close();
    });

    it('非数组 data → onError（errorLabel=项目 前缀文案）且不回调 onData', async () => {
      fetchMock.mockReset().mockResolvedValueOnce(sseResponse([frame('snapshot', '{"x":1}')]));
      const sub = subscribeProjects({ onData, onError });
      await settle();
      expect(onError).toHaveBeenCalledWith('项目推送数据格式错误');
      expect(onData).not.toHaveBeenCalled();
      sub.close();
    });
  });

  describe('连接状态回调', () => {
    it('connecting → open，重连后再经历 connecting → open', async () => {
      fetchMock
        .mockResolvedValueOnce(sseResponse([frame('snapshot', '[]')]))
        .mockResolvedValueOnce(sseResponse([frame('update', '[]')]));
      const sub = subscribe();
      await settle(); // 连接 1：connecting → open（首帧）→ 流结束 → 1s 退避
      await vi.advanceTimersByTimeAsync(1000); // 连接 2：connecting → open
      expect(onStateChange.mock.calls.map((c) => c[0])).toEqual([
        'connecting',
        'open',
        'connecting',
        'open',
      ]);
      sub.close();
    });
  });
});
