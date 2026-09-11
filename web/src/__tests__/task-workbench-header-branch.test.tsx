// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act } from 'react';
import { TaskWorkbenchPage } from '../pages/TaskWorkbenchPage';
import { api } from '../api';
import { subscribeTask } from '../sse';
import { mount, flushUI, stubMatchMedia } from './cm-test-env';
import type { Task } from '../types';

/* ============ 工作台页头分支区与溢出菜单空态（workbench-base-ref-and-overflow tasks 3.x / 4.1） ============
 * 页头分支区渲染矩阵（design D3 判定顺序）：dir 不渲染 / local-path 经 api.gitStatus 一次性
 * 获取当前分支（请求条件严格 project_kind==='repo' && mode==='local-path'）/ worktree 保留
 * branch 非空外层条件再按 base_ref 分流；分支名 button 化点击复制（.od-toast 2000ms 重显、
 * 不节流）；溢出菜单可见项为空时整段 return null，"展开→隐藏→恢复"不自动展开不调回调。
 * oracle I3 补强：连续推送不重复获取、失败不重试、dir+local-path 合法组合零请求、
 * 任务切换 deferred 隔离（旧任务卸载后结果到达被丢弃）。 */

type TaskSubOpts = {
  onData: (t: Task) => void;
  onError: (m: string) => void;
  onGone: () => void;
};
const taskSubs = new Map<string, TaskSubOpts>();

vi.mock('../sse', () => ({
  subscribeTask: vi.fn((id: string, opts: TaskSubOpts) => {
    taskSubs.set(id, opts);
    return { close: vi.fn(() => taskSubs.delete(id)) };
  }),
}));

vi.mock('../api', () => ({
  api: {
    listTerminals: vi.fn(async () => []),
    gitStatus: vi.fn(async () => ({ branch: 'feature-y', files: [] })),
  },
  ApiError: class ApiError extends Error {
    constructor(
      public status: number,
      public code: string,
      message: string,
    ) {
      super(message);
    }
  },
}));

vi.mock('../hooks', () => ({
  useProjects: () => ({ projects: [], loading: false, initialized: true, error: '' }),
  useProjectsRefresh: () => vi.fn(async () => {}),
  useMediaQuery: () => false,
}));

/* 终端/面板/配置编辑器重依赖打桩：本测试只关心页头渲染与溢出菜单交互。 */
vi.mock('../terminal/TerminalView', () => ({ TerminalView: () => null }));
vi.mock('../components/GitPanel', () => ({ GitPanel: () => null }));
vi.mock('../components/EnvEditor', () => ({ EnvEditor: () => null }));

function makeTask(over: Partial<Task>): Task {
  return {
    id: 't1',
    project_id: 'p1',
    project_kind: 'repo',
    name: 'demo-task',
    branch: '',
    base_ref: '',
    status: 'active',
    worktree_path: '/tmp/wt',
    mode: 'worktree',
    permission_mode: 'ask',
    init_status: 'none',
    created_at: 1,
    updated_at: 2,
    ...over,
  };
}

const cleanups: Array<() => void> = [];

function renderWorkbench(taskID = 't1') {
  const utils = mount(<TaskWorkbenchPage taskID={taskID} />);
  // 幂等卸载：测试内手动卸载（任务切换场景）与 afterEach 兜底不重复 unmount
  let done = false;
  const unmount = () => {
    if (done) return;
    done = true;
    utils.unmount();
  };
  cleanups.push(unmount);
  return { container: utils.container, unmount };
}

async function pushTask(task: Task) {
  await act(async () => taskSubs.get(task.id)!.onData(task));
  await flushUI();
}

function headerMeta(container: HTMLElement): HTMLElement | null {
  return container.querySelector<HTMLElement>('.header-meta');
}

function branchButtons(container: HTMLElement): HTMLButtonElement[] {
  return [...container.querySelectorAll<HTMLButtonElement>('.header-meta .branch-copy')];
}

function toast(): HTMLElement | null {
  return document.querySelector<HTMLElement>('.od-toast');
}

function overflowEntry(container: HTMLElement): HTMLButtonElement | null {
  return container.querySelector<HTMLButtonElement>('button[aria-label="更多操作"]');
}

function setClipboardApi(writeText?: (t: string) => Promise<void>): void {
  if (writeText) {
    Object.defineProperty(window.navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
    });
  } else {
    delete (window.navigator as { clipboard?: unknown }).clipboard;
  }
}

beforeEach(() => {
  stubMatchMedia(false);
  taskSubs.clear();
  vi.mocked(subscribeTask).mockClear();
  vi.mocked(api.gitStatus).mockClear();
  vi.mocked(api.gitStatus).mockResolvedValue({ branch: 'feature-y', files: [] });
  setClipboardApi(() => Promise.resolve());
});

afterEach(() => {
  while (cleanups.length) cleanups.pop()!();
  delete (window.navigator as { clipboard?: unknown }).clipboard;
  vi.useRealTimers();
});

describe('页头分支区渲染矩阵（tasks 3.1 / design D3）', () => {
  it('worktree + base_ref=refs/heads/main：两行「上当前/下来源（↳ 图标）」，tooltip/aria 两态齐备，不发 gitStatus', async () => {
    const { container } = renderWorkbench();
    await flushUI();
    await pushTask(makeTask({ branch: 'feature-x', base_ref: 'refs/heads/main' }));

    const meta = headerMeta(container)!;
    expect(meta).not.toBeNull();
    // 两行布局 v3：.branch-rows 列容器，DOM 序 = 上行当前、下行来源行（↳ 静态图标 + 按钮）
    const rows = meta.querySelector('.branch-rows')!;
    expect(rows).not.toBeNull();
    const [cur, src] = branchButtons(container);
    expect(rows.children[0]).toBe(cur);
    const srcRow = meta.querySelector('.branch-src-row')!;
    expect(srcRow).not.toBeNull();
    expect(srcRow.querySelector('svg')).not.toBeNull();
    expect(srcRow.querySelector('.branch-copy')).toBe(src);
    expect(src.textContent).toBe('main');
    expect(src.className).toContain('branch-copy-src');
    expect(src.getAttribute('aria-label')).toBe('复制来源分支 main');
    expect(src.title).toBe('来源分支：main（refs/heads/main）（点击复制）');
    expect(cur.textContent).toBe('feature-x');
    expect(cur.className).toContain('branch-copy-cur');
    expect(cur.className).not.toContain('branch-copy-src');
    expect(cur.getAttribute('aria-label')).toBe('复制当前分支 feature-x');
    expect(cur.title).toBe('当前分支：feature-x（点击复制）');
    // 容器 span 不挂 title（tooltip 下沉到各 button）
    expect(meta.title).toBe('');
    expect(meta.className).toContain('header-meta-branches');
    expect(api.gitStatus).not.toHaveBeenCalled();
  });

  it('worktree + base_ref=refs/remotes/origin/main：来源保留 remote 段展示 origin/main', async () => {
    const { container } = renderWorkbench();
    await flushUI();
    await pushTask(makeTask({ branch: 'feature-x', base_ref: 'refs/remotes/origin/main' }));

    const src = branchButtons(container)[1]; // 两行 v3：DOM 序 = 当前、来源
    expect(src.textContent).toBe('origin/main');
    expect(src.title).toBe('来源分支：origin/main（refs/remotes/origin/main）（点击复制）');
  });

  it('worktree + base_ref 空串（历史任务）：仅当前分支，与现状一致，无占位', async () => {
    const { container } = renderWorkbench();
    await flushUI();
    await pushTask(makeTask({ branch: 'feature-x', base_ref: '' }));

    const meta = headerMeta(container)!;
    expect(meta.textContent).not.toContain('→');
    const btns = branchButtons(container);
    expect(btns).toHaveLength(1);
    expect(btns[0].getAttribute('aria-label')).toBe('复制当前分支 feature-x');
  });

  it('旧服务端缺 base_ref 字段：按空串降级，仅当前分支', async () => {
    const legacy = makeTask({ branch: 'feature-x' }) as Partial<Task>;
    delete legacy.base_ref;
    const { container } = renderWorkbench();
    await flushUI();
    await pushTask(legacy as Task);

    const btns = branchButtons(container);
    expect(btns).toHaveLength(1);
    expect(btns[0].textContent).toBe('feature-x');
  });

  it('worktree 但 branch 为空：即使 base_ref 非空整段不渲染', async () => {
    const { container } = renderWorkbench();
    await flushUI();
    await pushTask(makeTask({ branch: '', base_ref: 'refs/heads/main' }));

    expect(headerMeta(container)).toBeNull();
  });

  it('同名不去重：base_ref 短名与当前分支相同仍各自展示 main（两行），两按钮各自独立', async () => {
    const { container } = renderWorkbench();
    await flushUI();
    await pushTask(makeTask({ branch: 'main', base_ref: 'refs/heads/main' }));

    const [cur, src] = branchButtons(container); // 两行 v3：DOM 序 = 当前、来源
    expect(src.textContent).toBe('main');
    expect(cur.textContent).toBe('main');
    expect(src.getAttribute('aria-label')).toBe('复制来源分支 main');
    expect(cur.getAttribute('aria-label')).toBe('复制当前分支 main');
  });

  it('dir 任务（合法组合 dir + local-path）：整段不渲染，且 MUST NOT 发 gitStatus 请求', async () => {
    const { container } = renderWorkbench();
    await flushUI();
    await pushTask(makeTask({ project_kind: 'dir', mode: 'local-path', branch: '' }));

    expect(headerMeta(container)).toBeNull();
    expect(api.gitStatus).not.toHaveBeenCalled();
  });
});

describe('local-path 当前分支获取（tasks 3.2 / design D3）', () => {
  it('repo local-path：经 api.gitStatus 一次性获取并展示单分支（仅当前行）', async () => {
    const { container } = renderWorkbench();
    await flushUI();
    await pushTask(makeTask({ mode: 'local-path', branch: '' }));

    expect(api.gitStatus).toHaveBeenCalledTimes(1);
    expect(api.gitStatus).toHaveBeenCalledWith('t1');
    const meta = headerMeta(container)!;
    // 两行结构下仅一行：.branch-rows 内只有当前分支按钮
    expect(meta.className).toContain('header-meta-branches');
    expect(meta.querySelectorAll('.branch-rows > .branch-copy')).toHaveLength(1);
    const btns = branchButtons(container);
    expect(btns).toHaveLength(1);
    expect(btns[0].textContent).toBe('feature-y');
    expect(btns[0].getAttribute('aria-label')).toBe('复制当前分支 feature-y');
    expect(btns[0].title).toBe('当前分支：feature-y（点击复制）');
  });

  it('gitStatus 非 401 失败：静默降级不展示，不设置页面业务错误', async () => {
    const { ApiError } = await import('../api');
    vi.mocked(api.gitStatus).mockRejectedValue(new ApiError(500, 'git_error', 'git 失败'));
    const { container } = renderWorkbench();
    await flushUI();
    await pushTask(makeTask({ mode: 'local-path', branch: '' }));

    expect(headerMeta(container)).toBeNull();
    expect(container.querySelector('.header-error')).toBeNull();
    expect(container.textContent).not.toContain('git 失败');
  });

  it('gitStatus 返回空串（detached HEAD）：静默降级不展示', async () => {
    vi.mocked(api.gitStatus).mockResolvedValue({ branch: '', files: [] });
    const { container } = renderWorkbench();
    await flushUI();
    await pushTask(makeTask({ mode: 'local-path', branch: '' }));

    expect(headerMeta(container)).toBeNull();
  });

  it('dir local-path 之外的谓词边界：repo worktree 不发 gitStatus', async () => {
    const { container } = renderWorkbench();
    await flushUI();
    await pushTask(makeTask({ mode: 'worktree', branch: 'feature-x' }));

    expect(api.gitStatus).not.toHaveBeenCalled();
    expect(headerMeta(container)).not.toBeNull();
  });

  it('同一任务连续多个 local-path SSE 帧：gitStatus 仍只请求一次（一次性获取，不重复）', async () => {
    const { container } = renderWorkbench();
    await flushUI();
    await pushTask(makeTask({ mode: 'local-path', branch: '' }));
    await pushTask(makeTask({ mode: 'local-path', branch: '', status: 'suspended' }));
    await pushTask(makeTask({ mode: 'local-path', branch: '', name: 'demo-task-renamed' }));

    expect(api.gitStatus).toHaveBeenCalledTimes(1);
    expect(api.gitStatus).toHaveBeenCalledWith('t1');
    expect(branchButtons(container)[0].textContent).toBe('feature-y');
  });

  it('获取失败后继续推送 SSE 帧：不重试，保持不展示', async () => {
    const { ApiError } = await import('../api');
    vi.mocked(api.gitStatus).mockRejectedValue(new ApiError(409, 'locked', '任务锁竞争'));
    const { container } = renderWorkbench();
    await flushUI();
    await pushTask(makeTask({ mode: 'local-path', branch: '' }));
    expect(headerMeta(container)).toBeNull();

    await pushTask(makeTask({ mode: 'local-path', branch: '', status: 'suspended' }));
    expect(api.gitStatus).toHaveBeenCalledTimes(1);
    expect(headerMeta(container)).toBeNull();
  });

  it('任务切换（路由重挂载隔离）：旧实例卸载后迟到结果不影响新实例，新实例仅显示自身结果', async () => {
    const pending = new Map<string, (v: { branch: string; files: never[] }) => void>();
    vi.mocked(api.gitStatus).mockImplementation(
      (id: string) => new Promise((resolve) => pending.set(id, resolve)),
    );

    // 旧任务 local-path：请求已发出、结果未到达
    const w1 = renderWorkbench('t1');
    await flushUI();
    await pushTask(makeTask({ id: 't1', mode: 'local-path', branch: '' }));
    expect(api.gitStatus).toHaveBeenCalledWith('t1');

    // 结果未到达即离开（组件卸载）
    w1.unmount();

    // 新任务：按任务身份独立获取
    const w2 = renderWorkbench('t2');
    await flushUI();
    await pushTask(makeTask({ id: 't2', mode: 'local-path', branch: '' }));
    expect(api.gitStatus).toHaveBeenCalledWith('t2');
    pending.get('t2')!({ branch: 'new-branch', files: [] });
    await flushUI();
    expect(headerMeta(w2.container)!.textContent).toContain('new-branch');

    // 旧任务迟到结果到达：被卸载守卫丢弃，不得影响新任务页头
    pending.get('t1')!({ branch: 'old-branch', files: [] });
    await flushUI();
    expect(headerMeta(w2.container)!.textContent).not.toContain('old-branch');
    expect(headerMeta(w2.container)!.textContent).toContain('new-branch');
  });

  // oracle I7：上一用例经"卸载旧实例"观察，删除 cancelled 守卫仍能通过；
  // 本用例在同一 root 内使谓词失效再恢复，断言旧请求结果被 cancelled 守卫丢弃、
  // 不覆盖当前有效结果（移除 effect 内 cancelled 检查时本用例应变红）。
  it('谓词失效再恢复（同一 root）：旧请求迟到结果被 cancelled 守卫丢弃，不覆盖当前有效结果', async () => {
    const resolvers: Array<(v: { branch: string; files: never[] }) => void> = [];
    vi.mocked(api.gitStatus).mockImplementation(
      () =>
        new Promise<{ branch: string; files: never[] }>((resolve) => resolvers.push(resolve)),
    );

    const { container } = renderWorkbench();
    await flushUI();
    // ① local-path 帧 → 请求 A 在途
    await pushTask(makeTask({ mode: 'local-path', branch: '' }));
    expect(api.gitStatus).toHaveBeenCalledTimes(1);

    // ② 谓词失效（同任务转为 dir 帧）→ effect 清理：重置 localPathBranch 并作废在途请求
    await pushTask(makeTask({ mode: 'local-path', branch: '', project_kind: 'dir' }));
    expect(headerMeta(container)).toBeNull();
    expect(api.gitStatus).toHaveBeenCalledTimes(1); // 谓词失效不发新请求

    // ③ 谓词恢复（repo local-path 帧）→ 重新请求 B
    await pushTask(makeTask({ mode: 'local-path', branch: '' }));
    expect(api.gitStatus).toHaveBeenCalledTimes(2);

    // ④ 当前有效请求 B 先到达
    resolvers[1]({ branch: 'new-branch', files: [] });
    await flushUI();
    expect(headerMeta(container)!.textContent).toContain('new-branch');

    // ⑤ 已作废的旧请求 A 迟到到达：被 cancelled 守卫丢弃，不得覆盖当前有效结果
    resolvers[0]({ branch: 'stale-branch', files: [] });
    await flushUI();
    expect(headerMeta(container)!.textContent).toContain('new-branch');
    expect(headerMeta(container)!.textContent).not.toContain('stale-branch');
  });
});

describe('分支名点击复制（tasks 3.3 / design D3）', () => {
  it('复制来源分支：写入显示短名，toast「已复制 main」', async () => {
    const writeText = vi.fn(() => Promise.resolve());
    setClipboardApi(writeText);
    const { container } = renderWorkbench();
    await flushUI();
    await pushTask(makeTask({ branch: 'feature-x', base_ref: 'refs/heads/main' }));

    const src = branchButtons(container)[1]; // 两行 v3：DOM 序 = 当前、来源
    await act(async () => {
      src.click();
    });

    expect(writeText).toHaveBeenCalledWith('main');
    expect(toast()!.textContent).toBe('已复制 main');
  });

  it('复制当前分支：写入 feature-x，toast「已复制 feature-x」', async () => {
    const writeText = vi.fn(() => Promise.resolve());
    setClipboardApi(writeText);
    const { container } = renderWorkbench();
    await flushUI();
    await pushTask(makeTask({ branch: 'feature-x', base_ref: 'refs/heads/main' }));

    const [cur] = branchButtons(container); // 当前分支为首个按钮（两行 v3）
    await act(async () => {
      cur.click();
    });

    expect(writeText).toHaveBeenCalledWith('feature-x');
    expect(toast()!.textContent).toBe('已复制 feature-x');
  });

  it('复制失败（Clipboard API 拒绝）：toast 指引悬浮提示兜底', async () => {
    setClipboardApi(() => Promise.reject(new Error('denied')));
    const { container } = renderWorkbench();
    await flushUI();
    await pushTask(makeTask({ branch: 'feature-x', base_ref: 'refs/heads/main' }));

    const src = branchButtons(container)[1]; // 两行 v3：DOM 序 = 当前、来源
    await act(async () => {
      src.click();
    });

    expect(toast()!.textContent).toBe('复制失败，完整分支名见悬浮提示');
    // tooltip 是真实兜底出口（完整文本可达）
    expect(src.title).toContain('refs/heads/main');
  });

  it('local-path 单分支点击复制：toast 反馈与双分支态一致', async () => {
    const writeText = vi.fn(() => Promise.resolve());
    setClipboardApi(writeText);
    const { container } = renderWorkbench();
    await flushUI();
    await pushTask(makeTask({ mode: 'local-path', branch: '' }));

    const [cur] = branchButtons(container);
    await act(async () => {
      cur.click();
    });

    expect(writeText).toHaveBeenCalledWith('feature-y');
    expect(toast()!.textContent).toBe('已复制 feature-y');
  });

  it('同名双分支各自点击均复制成功；连续点击重置 toast 计时器重显（不节流不叠加）', async () => {
    const writeText = vi.fn(() => Promise.resolve());
    setClipboardApi(writeText);
    const { container } = renderWorkbench();
    await flushUI();
    await pushTask(makeTask({ branch: 'main', base_ref: 'refs/heads/main' }));
    vi.useFakeTimers();

    const [cur, src] = branchButtons(container); // 两行 v3：DOM 序 = 当前、来源
    await act(async () => {
      src.click();
    });
    expect(writeText).toHaveBeenNthCalledWith(1, 'main');
    expect(toast()!.textContent).toBe('已复制 main');
    expect(document.querySelectorAll('.od-toast')).toHaveLength(1);

    // 距首次点击 1500ms 后点当前分支：反馈计时重置（再 2000ms 才隐藏）
    act(() => {
      vi.advanceTimersByTime(1500);
    });
    await act(async () => {
      cur.click();
    });
    expect(writeText).toHaveBeenNthCalledWith(2, 'main');
    expect(toast()!.textContent).toBe('已复制 main');
    expect(document.querySelectorAll('.od-toast')).toHaveLength(1);

    // 第二次点击后 1500ms（距首次 3000ms）：toast 仍在——证明计时被重置而非叠加
    act(() => {
      vi.advanceTimersByTime(1500);
    });
    expect(toast()).not.toBeNull();
    act(() => {
      vi.advanceTimersByTime(600);
    });
    expect(toast()).toBeNull();
  });
});

describe('溢出菜单空态隐藏（tasks 4.1 / design D4）', () => {
  it('active + init 正常：整个 .header-overflow 不渲染', async () => {
    const { container } = renderWorkbench();
    await flushUI();
    await pushTask(makeTask({ status: 'active', init_status: 'none' }));

    expect(overflowEntry(container)).toBeNull();
    expect(container.querySelector('.header-overflow')).toBeNull();
  });

  it('init failed（active）：入口出现，菜单仅含「查看 init 日志」', async () => {
    const { container } = renderWorkbench();
    await flushUI();
    await pushTask(makeTask({ status: 'active', init_status: 'failed' }));

    const trigger = overflowEntry(container)!;
    expect(trigger).not.toBeNull();
    await act(async () => {
      trigger.click();
    });
    const items = [...container.querySelectorAll<HTMLButtonElement>('.overflow-item')];
    expect(items.map((b) => b.textContent)).toEqual(['查看 init 日志']);
  });

  it('suspended：入口出现，删除项可见且未禁用', async () => {
    const { container } = renderWorkbench();
    await flushUI();
    await pushTask(makeTask({ status: 'suspended' }));

    await act(async () => {
      overflowEntry(container)!.click();
    });
    const items = [...container.querySelectorAll<HTMLButtonElement>('.overflow-item')];
    expect(items.map((b) => b.textContent)).toEqual(['删除任务']);
    expect(items[0].disabled).toBe(false);
  });

  it('过渡状态（creating）：删除项禁用但入口保留', async () => {
    const { container } = renderWorkbench();
    await flushUI();
    await pushTask(makeTask({ status: 'creating' }));

    await act(async () => {
      overflowEntry(container)!.click();
    });
    const del = container.querySelector<HTMLButtonElement>('.overflow-item-danger')!;
    expect(del).not.toBeNull();
    expect(del.disabled).toBe(true);
  });

  it('展开 → 可见项变空（入口隐藏）→ 恢复：不自动展开、不调任何操作回调', async () => {
    const { container } = renderWorkbench();
    await flushUI();
    await pushTask(makeTask({ status: 'suspended' }));

    // 展开菜单
    await act(async () => {
      overflowEntry(container)!.click();
    });
    expect(container.querySelector('.overflow-menu')).not.toBeNull();

    // 可见项变空：active + init 正常 → 入口整段隐藏
    await pushTask(makeTask({ status: 'active', init_status: 'none' }));
    expect(container.querySelector('.header-overflow')).toBeNull();

    // 恢复可见项：入口重新出现但为关闭状态，不自动展开、未调 onDelete（无删除弹窗）
    await pushTask(makeTask({ status: 'suspended' }));
    expect(overflowEntry(container)).not.toBeNull();
    expect(container.querySelector('.overflow-menu')).toBeNull();
    expect(container.querySelector('.modal-backdrop')).toBeNull();
  });

  it('入口显隐随订阅数据动态更新（无需手动刷新）', async () => {
    const { container } = renderWorkbench();
    await flushUI();
    await pushTask(makeTask({ status: 'active', init_status: 'none' }));
    expect(overflowEntry(container)).toBeNull();

    await pushTask(makeTask({ status: 'suspended' }));
    expect(overflowEntry(container)).not.toBeNull();
  });
});
