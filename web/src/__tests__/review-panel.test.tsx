// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act } from 'react';
import { ReviewPanel } from '../components/ReviewPanel';
import { api, ApiError } from '../api';
import type { Annotation, Submission, SubmissionsListResponse, SubmitCapability } from '../types';
import { flushUI, mount } from './cm-test-env';

/* ============================ ReviewPanel 交互测试（diff-review-workbench tasks 5.4/5.6） ============================
 * 批注列表（编辑/删除/stale 徽标）、提交能力禁用、预览弹层（临时移除 + id+revision + 409 重确认）、
 * queue/history/failures 分区操作限制（撤回仅 queued、删除仅终态）。 */

vi.mock('../api', async (importOriginal) => {
  const orig = await importOriginal<typeof import('../api')>();
  return {
    ...orig,
    api: {
      listSubmissions: vi.fn(async () => ({ queue: [], history: [], failures: [] })),
      updateAnnotationComment: vi.fn(async () => ({})),
      deleteAnnotation: vi.fn(async () => undefined),
      createSubmission: vi.fn(async () => ({})),
      cancelSubmission: vi.fn(async () => undefined),
      deleteSubmission: vi.fn(async () => undefined),
    },
  };
});

const mocked = api as unknown as {
  listSubmissions: ReturnType<typeof vi.fn>;
  updateAnnotationComment: ReturnType<typeof vi.fn>;
  deleteAnnotation: ReturnType<typeof vi.fn>;
  createSubmission: ReturnType<typeof vi.fn>;
  cancelSubmission: ReturnType<typeof vi.fn>;
  deleteSubmission: ReturnType<typeof vi.fn>;
};

function makeAnn(over: Partial<Annotation>): Annotation {
  return {
    id: 'ann-1',
    path: 'a.ts',
    side: 'new',
    ref: '',
    untracked: false,
    startLine: 2,
    endLine: 3,
    snapshotStartLine: 1,
    snapshotLineCount: 6,
    snapshot: '',
    comment: '评论一',
    revision: 3,
    stale: false,
    createdAt: 100,
    updatedAt: 100,
    ...over,
  };
}

function makeSub(over: Partial<Submission>): Submission {
  return {
    id: 'sub-1',
    status: 'queued',
    note: '',
    payload: '',
    truncated: false,
    error: '',
    createdAt: 100,
    sentAt: null,
    items: [],
    ...over,
  };
}

const supported: SubmitCapability = { state: 'supported', reason: '' };

function renderPanel(over: {
  annotations?: Annotation[];
  capability?: SubmitCapability;
  onChanged?: () => void;
  highlightIDs?: Set<string>;
}) {
  return mount(
    <ReviewPanel
      taskID="t1"
      annotations={over.annotations ?? []}
      capability={over.capability ?? supported}
      onChanged={over.onChanged ?? (() => {})}
      highlightIDs={over.highlightIDs ?? new Set()}
    />,
  );
}

function setNativeValue(el: HTMLTextAreaElement, value: string) {
  const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!;
  act(() => {
    setter.call(el, value);
    el.dispatchEvent(new Event('input', { bubbles: true }));
  });
}

function findButton(container: HTMLElement, text: string): HTMLButtonElement {
  const btn = [...container.querySelectorAll<HTMLButtonElement>('button')].find(
    (b) => b.textContent === text,
  );
  expect(btn, `按钮「${text}」存在`).toBeTruthy();
  return btn!;
}

beforeEach(() => {
  vi.clearAllMocks();
  mocked.listSubmissions.mockResolvedValue({ queue: [], history: [], failures: [] });
});

describe('批注列表（编辑/删除/stale 徽标）', () => {
  it('stale 批注在列表带「已漂移」徽标（与行内标记构成双处展示）', async () => {
    const { container, unmount } = renderPanel({
      annotations: [makeAnn({ stale: true })],
    });
    await flushUI();
    const badge = container.querySelector('.ann-stale-badge');
    expect(badge?.textContent).toBe('已漂移');
    expect(container.textContent).toContain('评论一');
    unmount();
  });

  it('编辑评论：PATCH 携带新评论并触发刷新', async () => {
    const onChanged = vi.fn();
    const { container, unmount } = renderPanel({ annotations: [makeAnn({})], onChanged });
    await flushUI();
    act(() => findButton(container, '编辑评论').click());
    const ta = container.querySelector<HTMLTextAreaElement>('.ann-edit-input')!;
    setNativeValue(ta, '改成新评论');
    await act(async () => {
      findButton(container, '保存').click();
      await Promise.resolve();
    });
    expect(mocked.updateAnnotationComment).toHaveBeenCalledWith('t1', 'ann-1', '改成新评论');
    expect(onChanged).toHaveBeenCalled();
    unmount();
  });

  it('删除批注：调用删除端点并触发刷新', async () => {
    const onChanged = vi.fn();
    const { container, unmount } = renderPanel({ annotations: [makeAnn({})], onChanged });
    await flushUI();
    await act(async () => {
      findButton(container, '删除').click();
      await Promise.resolve();
    });
    expect(mocked.deleteAnnotation).toHaveBeenCalledWith('t1', 'ann-1');
    expect(onChanged).toHaveBeenCalled();
    unmount();
  });

  it('行内标记定位：highlightIDs 命中条目带高亮样式', async () => {
    const { container, unmount } = renderPanel({
      annotations: [makeAnn({ id: 'x1' }), makeAnn({ id: 'x2', comment: '评论二' })],
      highlightIDs: new Set(['x2']),
    });
    await flushUI();
    const highlighted = container.querySelectorAll('.ann-item-highlight');
    expect(highlighted).toHaveLength(1);
    expect(highlighted[0].textContent).toContain('评论二');
    unmount();
  });
});

describe('提交能力门禁', () => {
  it('capability 非 supported：提交按钮禁用并提示原因', async () => {
    const { container, unmount } = renderPanel({
      annotations: [makeAnn({})],
      capability: { state: 'unsupported', reason: 'prompt_async 不可用' },
    });
    await flushUI();
    const btn = findButton(container, '提交给 AI');
    expect(btn.disabled).toBe(true);
    expect(container.textContent).toContain('prompt_async 不可用');
    unmount();
  });

  it('无批注时提交按钮禁用', async () => {
    const { container, unmount } = renderPanel({ annotations: [] });
    await flushUI();
    expect(findButton(container, '提交给 AI').disabled).toBe(true);
    unmount();
  });
});

describe('提交预览弹层', () => {
  const twoAnns = () => [
    makeAnn({ id: 'a1', comment: '第一条', revision: 2, createdAt: 100 }),
    makeAnn({ id: 'a2', comment: '第二条', revision: 7, createdAt: 200 }),
  ];

  async function openPreview(container: HTMLElement) {
    await act(async () => {
      findButton(container, '提交给 AI').click();
    });
    expect(container.querySelector('.ann-preview')).toBeTruthy();
  }

  it('临时移除条目不进入提交，确认携带 id+revision 与补充说明', async () => {
    const { container, unmount } = renderPanel({ annotations: twoAnns() });
    await flushUI();
    await openPreview(container);
    // 移除第一条（仅本次不提交，保留为活动批注）
    await act(async () => {
      findButton(container, '本次不提交').click();
    });
    expect(container.textContent).toContain('保留在活动批注中');
    setNativeValue(container.querySelector<HTMLTextAreaElement>('.ann-note-input')!, '请逐条修复');
    await act(async () => {
      findButton(container, '确认提交（1）').click();
      await Promise.resolve();
    });
    expect(mocked.createSubmission).toHaveBeenCalledWith('t1', [{ id: 'a2', revision: 7 }], '请逐条修复');
    unmount();
  });

  it('临时移除后可恢复，恢复后回到提交集合', async () => {
    const { container, unmount } = renderPanel({ annotations: twoAnns() });
    await flushUI();
    await openPreview(container);
    await act(async () => {
      findButton(container, '本次不提交').click();
    });
    expect(container.querySelector('.ann-preview')!.textContent).toContain('1 条批注');
    await act(async () => {
      findButton(container, '恢复').click();
    });
    expect(container.querySelector('.ann-preview')!.textContent).toContain('2 条批注');
    unmount();
  });

  it('409 冲突：保留弹层、刷新批注、提示重新确认', async () => {
    mocked.createSubmission.mockRejectedValueOnce(new ApiError(409, 'conflict', 'revision 不符'));
    const onChanged = vi.fn();
    const { container, unmount } = renderPanel({ annotations: twoAnns(), onChanged });
    await flushUI();
    await openPreview(container);
    await act(async () => {
      findButton(container, '确认提交（2）').click();
      await Promise.resolve();
    });
    expect(container.querySelector('.ann-preview')).toBeTruthy(); // 弹层保留
    expect(container.textContent).toContain('请重新确认');
    expect(onChanged).toHaveBeenCalled(); // 批注列表已刷新
    unmount();
  });

  it('全部移除后确认按钮禁用', async () => {
    const { container, unmount } = renderPanel({ annotations: twoAnns() });
    await flushUI();
    await openPreview(container);
    for (const btn of [
      ...container.querySelectorAll<HTMLButtonElement>('.ann-preview-list button'),
    ].filter((b) => b.textContent === '本次不提交')) {
      // eslint-disable-next-line no-await-in-loop
      await act(async () => {
        btn.click();
      });
    }
    expect(findButton(container, '确认提交（0）').disabled).toBe(true);
    unmount();
  });
});

describe('提交分区（queue/history/failures）操作限制', () => {
  it('撤回仅 queued；删除仅终态（sent/failed/delivery_unknown）', async () => {
    const subs: SubmissionsListResponse = {
      queue: [makeSub({ id: 'q1', status: 'queued' }), makeSub({ id: 'q2', status: 'sending' })],
      history: [makeSub({ id: 'h1', status: 'sent', sentAt: 200 })],
      failures: [makeSub({ id: 'f1', status: 'failed', error: 'boom' })],
    };
    mocked.listSubmissions.mockResolvedValue(subs);
    const { container, unmount } = renderPanel({ annotations: [] });
    await flushUI();

    // 渲染顺序固定：queue（seq 升序）→ failures → history
    const items = [...container.querySelectorAll<HTMLElement>('.subs-item')];
    expect(items).toHaveLength(4);
    const btnTexts = (el: HTMLElement) =>
      [...el.querySelectorAll('button')].map((b) => b.textContent);
    const texts = items.map((x) => x.textContent ?? '');
    // queued：有撤回、无删除
    expect(texts[0]).toContain('排队中');
    expect(btnTexts(items[0])).toEqual(['撤回']);
    // sending：既无撤回也无删除
    expect(texts[1]).toContain('发送中');
    expect(btnTexts(items[1])).toEqual([]);
    // failed（终态）：有详情+删除、无撤回，展示 error
    expect(texts[2]).toContain('boom');
    expect(btnTexts(items[2])).toEqual(['详情', '删除']);
    // sent（终态）：有详情+删除、无撤回
    expect(texts[3]).toContain('已发送');
    expect(btnTexts(items[3])).toEqual(['详情', '删除']);

    // 撤回调用
    await act(async () => {
      [...items[0].querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '撤回')!
        .click();
      await Promise.resolve();
    });
    expect(mocked.cancelSubmission).toHaveBeenCalledWith('t1', 'q1');
    // 删除调用（failures 区）
    await act(async () => {
      [...items[2].querySelectorAll<HTMLButtonElement>('button')]
        .find((b) => b.textContent === '删除')!
        .click();
      await Promise.resolve();
    });
    expect(mocked.deleteSubmission).toHaveBeenCalledWith('t1', 'f1');
    unmount();
  });

  it('非 queued 撤回被服务端拒绝时展示错误', async () => {
    mocked.listSubmissions.mockResolvedValue({
      queue: [makeSub({ id: 'q1', status: 'queued' })],
      history: [],
      failures: [],
    });
    mocked.cancelSubmission.mockRejectedValueOnce(new ApiError(422, 'invalid_state', '非 queued'));
    const { container, unmount } = renderPanel({ annotations: [] });
    await flushUI();
    await act(async () => {
      findButton(container, '撤回').click();
      await Promise.resolve();
    });
    expect(container.textContent).toContain('非 queued');
    unmount();
  });
});

describe('提交只读详情（F5：历史快照 / 失败 payload）', () => {
  const item = {
    annotationId: 'ann-1',
    path: 'a.ts',
    side: 'new' as const,
    ref: '',
    untracked: false,
    startLine: 2,
    endLine: 3,
    snapshotStartLine: 1,
    snapshot: 'l1\nl2\nl3',
    comment: '快照评论',
  };

  it('历史记录展开详情：展示批注条目与提交时刻快照（只读）', async () => {
    mocked.listSubmissions.mockResolvedValue({
      queue: [],
      history: [makeSub({ id: 'h1', status: 'sent', sentAt: 200, note: '补充说明', items: [item] })],
      failures: [],
    });
    const { container, unmount } = renderPanel({ annotations: [] });
    await flushUI();
    expect(container.textContent).toContain('补充说明');
    expect(container.textContent).not.toContain('快照评论'); // 默认折叠
    await act(async () => {
      findButton(container, '详情').click();
    });
    expect(container.textContent).toContain('快照评论');
    expect(container.textContent).toContain('a.ts');
    expect(container.querySelector('.subs-snapshot')?.textContent).toBe('l1\nl2\nl3');
    // 历史无 payload 展示（仅失败区）
    expect(container.querySelector('.subs-payload')).toBeNull();
    unmount();
  });

  it('失败/投递未知展开详情：展示可复制 payload，无自动重发入口', async () => {
    mocked.listSubmissions.mockResolvedValue({
      queue: [],
      history: [],
      failures: [
        makeSub({
          id: 'f1',
          status: 'delivery_unknown',
          error: 'delivery unknown after restart',
          payload: '以下是代码 review 批注…',
          items: [item],
        }),
      ],
    });
    const { container, unmount } = renderPanel({ annotations: [] });
    await flushUI();
    await act(async () => {
      findButton(container, '详情').click();
    });
    expect(container.querySelector('.subs-payload')?.textContent).toBe('以下是代码 review 批注…');
    expect(findButton(container, '复制 payload')).toBeTruthy();
    // 无重发入口
    expect(container.textContent).not.toContain('重发');
    unmount();
  });
});
