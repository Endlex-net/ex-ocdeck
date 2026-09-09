// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act } from 'react';
import { GitPanel } from '../components/GitPanel';
import { api } from '../api';
import type { GitDiffResult, GitFileEntry } from '../types';
import { flushUI, mount, rerender, stubMatchMedia } from './cm-test-env';

/* ==================== idle-reminder-user-activity 6.1/6.2：git 区域主动交互上报 ====================
 * design D4：GitPanel 区域容器捕获阶段集中监听 click/wheel/touchstart/touchmove/keydown/input，
 * 归属取手势发生时的 taskID，fire-and-forget 上报；仅可信事件（isTrusted=true，真实用户
 * 输入）上报，程序派发事件、自动加载/刷新请求与程序滚动（scroll）不计。
 * 嵌套组件（ReviewPanel/DiffViewer）复用同一入口，不逐组件添加。
 * design D6（DR4）：上报入口 leading+trailing 节流（2s 窗口）——距上次发送满 2s 立即发送；
 * 冷却期内首个手势置 pending、按原定时刻补报一次（不被后续手势推迟，补后清空）；
 * 孤立手势只发一次；失败不重放仍计入窗口；切任务/卸载/hidden 时尽力补报 pending。 */

vi.mock('../api', () => ({
  api: {
    gitStatus: vi.fn(),
    gitDiff: vi.fn(),
    gitCommit: vi.fn(),
    gitPush: vi.fn(),
    listAnnotations: vi.fn(async () => ({
      annotations: [],
      submitCapability: { state: 'supported', reason: '' },
    })),
    listSubmissions: vi.fn(async () => ({ queue: [], history: [], failures: [] })),
    gitFileRead: vi.fn(),
    gitFileWrite: vi.fn(),
    reportActivity: vi.fn(),
  },
  ApiError: class ApiError extends Error {
    constructor(
      public readonly status: number,
      public readonly code: string,
      message: string,
    ) {
      super(message);
    }
  },
}));

const gitStatusMock = vi.mocked(api.gitStatus);
const gitDiffMock = vi.mocked(api.gitDiff);
// reportActivity 为本 change 新增端点封装，用 cast 兼容接线前的类型
const reportActivityMock = vi.mocked(
  (api as unknown as { reportActivity: (...args: unknown[]) => void }).reportActivity,
);

const entry: GitFileEntry = {
  path: 'a.ts',
  x: 'M',
  y: ' ',
  staged: false,
  unstaged: true,
  untracked: false,
  additions: 1,
  deletions: 0,
  isBinary: false,
};

const diff: GitDiffResult = {
  oldContent: 'a\n',
  newContent: 'a\nb\n',
  oldExists: true,
  newExists: true,
  oldMode: '100644',
  newMode: '100644',
  isBinary: false,
  truncated: false,
};

async function until(cond: () => boolean, timeoutMs = 4000) {
  const start = Date.now();
  while (!cond()) {
    if (Date.now() - start > timeoutMs) throw new Error('condition timeout');
    // eslint-disable-next-line no-await-in-loop
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 10));
    });
  }
}

function panelOf(container: HTMLElement): HTMLElement {
  return container.querySelector('.git-panel') as HTMLElement;
}

/* jsdom 限制：dispatchEvent() 在派发入口强制 isTrusted=false（EventTarget-impl.js 的
 * dispatchEvent 内 eventImpl.isTrusted = false），且 isTrusted 是实例上不可配置的
 * unforgeable 属性——经公开 API 无法构造可信事件。真实浏览器中用户输入产生的事件
 * isTrusted=true 且仅用户代理能产生。因此"可信"事件取 jsdom 内部 impl（own
 * Symbol(impl)）直接调用 _dispatch 派发：绕过强制重置、走完整捕获/冒泡传播，
 * 该模拟代表真实用户输入；找不到 impl 时显式抛错，不静默降级。 */
type JSDomEventImpl = { isTrusted: boolean; _dispatch: (event: object) => boolean };

function implOf(target: object): JSDomEventImpl {
  const sym = Object.getOwnPropertySymbols(target).find((s) => {
    const v = (target as Record<symbol, unknown>)[s];
    return !!v && typeof v === 'object';
  });
  if (!sym) throw new Error('jsdom impl object not found: 无法模拟事件派发');
  return (target as Record<symbol, unknown>)[sym] as JSDomEventImpl;
}

/** 派发冒泡的"可信"DOM 事件（模拟真实用户手势：真实浏览器中用户输入 isTrusted=true）。 */
function fire(el: Element, type: string) {
  const ev = new Event(type, { bubbles: true, cancelable: true });
  implOf(ev).isTrusted = true;
  act(() => {
    implOf(el)._dispatch(implOf(ev));
  });
}

/** 程序派发事件（isTrusted=false）：模拟代码触发的 .click()/dispatchEvent，MUST NOT 上报。 */
function fireUntrusted(el: Element, type: string) {
  act(() => {
    el.dispatchEvent(new Event(type, { bubbles: true, cancelable: true }));
  });
}

/** 节流语义测试装载：完全 fake 的时钟（setTimeout+performance），listener 挂载不依赖
 *  status 渲染，mount 后即可直接派发手势，时间全部经 vi.advanceTimersByTime 推进。 */
function mountWithFakeClock(taskID = 't1') {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'performance'] });
  return mount(<GitPanel taskID={taskID} active />);
}

beforeEach(() => {
  vi.clearAllMocks();
  stubMatchMedia(false);
  gitStatusMock.mockResolvedValue({ branch: 'main', files: [entry] });
  gitDiffMock.mockResolvedValue(diff);
});

afterEach(() => {
  vi.useRealTimers();
});

describe('GitPanel 主动操作上报（D4 容器捕获监听）', () => {
  it('容器内各类手势均上报且 taskID 归属正确（逐一手势间隔满节流窗口）', () => {
    const { container, unmount } = mountWithFakeClock();
    const panel = panelOf(container);
    const types = ['click', 'keydown', 'input', 'wheel', 'touchstart', 'touchmove'];
    for (let i = 0; i < types.length; i++) {
      if (i > 0) vi.advanceTimersByTime(2000); // 跨出冷却窗口：每个手势均为 leading
      fire(panel, types[i]);
    }
    expect(reportActivityMock).toHaveBeenCalledTimes(6);
    for (const call of reportActivityMock.mock.calls) expect(call[0]).toBe('t1');
    unmount();
  });

  it('嵌套组件（DiffViewer/ReviewPanel）内手势复用同一上报入口', async () => {
    const { container, unmount } = mount(<GitPanel taskID="t1" active />);
    await until(() => container.querySelector('.git-file-path') !== null);
    // 打开文件 diff：懒加载的 DiffViewer 挂进 .git-diff 区域（程序 .click() 不可信、不启动节流窗口）
    act(() => {
      (container.querySelector('.git-file-path') as HTMLElement).click();
    });
    await until(() => container.querySelector('.cm-editor') !== null);
    reportActivityMock.mockClear();
    fire(container.querySelector('.cm-editor') as HTMLElement, 'click'); // 首个可信手势：leading
    fire(container.querySelector('.review-header') as HTMLElement, 'click'); // 冷却期内：pending
    expect(reportActivityMock).toHaveBeenCalledTimes(1);
    unmount(); // 清理时尽力补报 pending
    expect(reportActivityMock).toHaveBeenCalledTimes(2);
    for (const call of reportActivityMock.mock.calls) expect(call[0]).toBe('t1');
  });

  it('程序派发事件（isTrusted=false）不计为主动操作', async () => {
    const { container, unmount } = mount(<GitPanel taskID="t1" active />);
    await until(() => container.querySelector('.git-file-path') !== null);
    const panel = panelOf(container);
    // HTMLElement.click() 与裸 dispatchEvent 在真实浏览器中 isTrusted 均为 false
    act(() => {
      panel.click();
    });
    fireUntrusted(panel, 'click');
    fireUntrusted(panel, 'input');
    expect(reportActivityMock).not.toHaveBeenCalled();
    // 过滤的是事件来源而非类型：同一类型的事件一旦可信仍正常上报
    fire(panel, 'click');
    expect(reportActivityMock).toHaveBeenCalledTimes(1);
    unmount();
  });

  it('自动加载与程序触发刷新、scroll 事件不上报', async () => {
    const { container, root, unmount } = mount(<GitPanel taskID="t1" active />);
    await until(() => container.querySelector('.git-file-path') !== null);
    await flushUI();
    expect(gitStatusMock).toHaveBeenCalled(); // 自动加载确实发生了
    expect(reportActivityMock).not.toHaveBeenCalled();
    // 重挂激活触发程序刷新（无用户手势）
    rerender(root, <GitPanel taskID="t1" active={false} />);
    rerender(root, <GitPanel taskID="t1" active />);
    await until(() => gitStatusMock.mock.calls.length >= 2);
    await flushUI();
    expect(reportActivityMock).not.toHaveBeenCalled();
    // 程序滚动（scrollIntoView 引发的 scroll 事件）不算手势
    const files = container.querySelector('.git-files') as HTMLElement;
    fire(files, 'scroll');
    fire(panelOf(container), 'scroll');
    expect(reportActivityMock).not.toHaveBeenCalled();
    unmount();
  });

  it('手势监听以捕获阶段注册，wheel/touchmove 为 passive', async () => {
    const recorded: Array<{ el: EventTarget; type: string; options: AddEventListenerOptions }> = [];
    const orig = EventTarget.prototype.addEventListener;
    const spy = vi
      .spyOn(EventTarget.prototype, 'addEventListener')
      .mockImplementation(function (this: EventTarget, ...args: Parameters<typeof orig>) {
        const [type, , options] = args;
        recorded.push({
          el: this,
          type: String(type),
          options: (options ?? {}) as AddEventListenerOptions,
        });
        return orig.apply(this, args);
      });
    const { container, unmount } = mount(<GitPanel taskID="t1" active />);
    try {
      await until(() => container.querySelector('.git-file-path') !== null);
      const panel = panelOf(container);
      const own = recorded.filter((c) => c.el === panel);
      expect(new Set(own.map((c) => c.type))).toEqual(
        new Set(['click', 'keydown', 'input', 'touchstart', 'wheel', 'touchmove']),
      );
      for (const c of own) expect(c.options.capture).toBe(true);
      for (const c of own.filter((c) => c.type === 'wheel' || c.type === 'touchmove')) {
        expect(c.options.passive).toBe(true);
      }
    } finally {
      spy.mockRestore();
      unmount();
    }
  });

  it('卸载后移除监听，不再上报', async () => {
    const { container, unmount } = mount(<GitPanel taskID="t1" active />);
    await until(() => container.querySelector('.git-file-path') !== null);
    const panel = panelOf(container);
    fire(panel, 'click');
    expect(reportActivityMock).toHaveBeenCalledTimes(1);
    unmount();
    // 仍派发可信事件：证明不再上报是监听已移除，而非 isTrusted 过滤
    fire(panel, 'click');
    expect(reportActivityMock).toHaveBeenCalledTimes(1);
  });
});

describe('上报节流（D6/DR4：2s leading+trailing）', () => {
  it('首次手势立即上报（leading）', () => {
    const { container, unmount } = mountWithFakeClock();
    fire(panelOf(container), 'click');
    expect(reportActivityMock).toHaveBeenCalledTimes(1);
    expect(reportActivityMock).toHaveBeenCalledWith('t1');
    unmount(); // 无 pending：清理不补发
    expect(reportActivityMock).toHaveBeenCalledTimes(1);
  });

  it('冷却期内首个手势到点补报一次；后续手势不推迟该时刻', () => {
    const { container, unmount } = mountWithFakeClock();
    const panel = panelOf(container);
    fire(panel, 'click'); // t=0 leading
    vi.advanceTimersByTime(700);
    fire(panel, 'click'); // 冷却期内：置 pending，补报时刻仍为 t=2000
    vi.advanceTimersByTime(500); // t=1200
    fire(panel, 'click'); // pending 已存在：不推迟
    fire(panel, 'click');
    expect(reportActivityMock).toHaveBeenCalledTimes(1); // 冷却期内不立即发送
    vi.advanceTimersByTime(800); // t=2000：trailing 补报
    expect(reportActivityMock).toHaveBeenCalledTimes(2);
    expect(reportActivityMock).toHaveBeenLastCalledWith('t1');
    vi.advanceTimersByTime(4000); // 无新手势：不再发送
    expect(reportActivityMock).toHaveBeenCalledTimes(2);
    unmount();
  });

  it('孤立手势只发一次，不凭空生成补报', () => {
    const { container, unmount } = mountWithFakeClock();
    fire(panelOf(container), 'keydown');
    vi.advanceTimersByTime(5000);
    expect(reportActivityMock).toHaveBeenCalledTimes(1);
    unmount();
  });

  it('跨武装边界的最后手势经 trailing 补报不丢失', () => {
    const { container, unmount } = mountWithFakeClock();
    const panel = panelOf(container);
    fire(panel, 'wheel'); // t=0 leading
    vi.advanceTimersByTime(1900);
    fire(panel, 'touchmove'); // 冷却期内最后手势：置 pending
    vi.advanceTimersByTime(100); // t=2000：到点补报
    expect(reportActivityMock).toHaveBeenCalledTimes(2);
    expect(reportActivityMock).toHaveBeenLastCalledWith('t1');
    unmount();
  });

  it('持续输入每窗口至多一条且持续有报（不饿死）', () => {
    const { container, unmount } = mountWithFakeClock();
    const panel = panelOf(container);
    fire(panel, 'touchstart'); // t=0 leading
    expect(reportActivityMock).toHaveBeenCalledTimes(1);
    for (let t = 500; t <= 6000; t += 500) {
      vi.advanceTimersByTime(500);
      fire(panel, 'touchmove');
    }
    // t=0 leading + t=2/4/6s trailing：持续手势每窗口至多一条、且不因持续冷却而饿死
    expect(reportActivityMock).toHaveBeenCalledTimes(4);
    for (const call of reportActivityMock.mock.calls) expect(call[0]).toBe('t1');
    unmount();
  });

  it('isTrusted=false 不发送且不改变节流状态', () => {
    const { container, unmount } = mountWithFakeClock();
    const panel = panelOf(container);
    fire(panel, 'click'); // t=0 leading
    expect(reportActivityMock).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(500);
    fireUntrusted(panel, 'click'); // 不可信：不置 pending、不启动补报
    fireUntrusted(panel, 'input');
    act(() => {
      panel.click(); // 程序点击（jsdom 中亦不可信）
    });
    vi.advanceTimersByTime(1500); // t=2000：若不可信手势改变了节流状态，此处会多出补报
    expect(reportActivityMock).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(500); // t=2500：窗口已过期
    fireUntrusted(panel, 'click'); // 不可信：不触发 leading
    expect(reportActivityMock).toHaveBeenCalledTimes(1);
    fire(panel, 'click'); // 可信手势：立即 leading
    expect(reportActivityMock).toHaveBeenCalledTimes(2);
    expect(reportActivityMock).toHaveBeenLastCalledWith('t1');
    unmount();
  });

  it('切任务不串报：pending 归属旧 taskID', () => {
    const { container, root, unmount } = mountWithFakeClock('t1');
    const panel = panelOf(container);
    fire(panel, 'click'); // t1 leading
    vi.advanceTimersByTime(1000);
    fire(panel, 'keydown'); // t1 冷却期内：pending 绑定 't1'
    expect(reportActivityMock.mock.calls.map((c) => c[0])).toEqual(['t1']);
    rerender(root, <GitPanel taskID="t2" active />); // 切任务：flush pending（'t1'）并清理定时器
    expect(reportActivityMock.mock.calls.map((c) => c[0])).toEqual(['t1', 't1']);
    fire(panelOf(container), 'click'); // 新任务入口节流状态独立：立即 leading
    expect(reportActivityMock.mock.calls.map((c) => c[0])).toEqual(['t1', 't1', 't2']);
    unmount();
  });

  it('卸载时尽力补报 pending', () => {
    const { container, unmount } = mountWithFakeClock();
    const panel = panelOf(container);
    fire(panel, 'click'); // leading
    vi.advanceTimersByTime(1000);
    fire(panel, 'input'); // 冷却期内：pending='t1'
    unmount(); // 清理：补报 pending 后移除监听
    expect(reportActivityMock.mock.calls.map((c) => c[0])).toEqual(['t1', 't1']);
    fire(panel, 'click'); // 已卸载：监听已移除，不再上报
    expect(reportActivityMock).toHaveBeenCalledTimes(2);
  });

  it('页面 hidden 时尽力补报 pending；无 pending 不发送', () => {
    const { container, unmount } = mountWithFakeClock();
    const panel = panelOf(container);
    const setHidden = (hidden: boolean) => {
      Object.defineProperty(document, 'visibilityState', {
        value: hidden ? 'hidden' : 'visible',
        configurable: true,
      });
    };
    try {
      fire(panel, 'click'); // t=0 leading
      expect(reportActivityMock).toHaveBeenCalledTimes(1);
      setHidden(true); // 无 pending：hidden 不发送
      act(() => {
        document.dispatchEvent(new Event('visibilitychange'));
      });
      expect(reportActivityMock).toHaveBeenCalledTimes(1);
      setHidden(false);
      vi.advanceTimersByTime(1000);
      fire(panel, 'wheel'); // 冷却期内：pending='t1'
      setHidden(true);
      act(() => {
        document.dispatchEvent(new Event('visibilitychange'));
      }); // 尽力补报
      expect(reportActivityMock.mock.calls.map((c) => c[0])).toEqual(['t1', 't1']);
      // 补报后 pending 清空、定时器清理：原定时刻不再重复发送
      setHidden(false);
      vi.advanceTimersByTime(1000); // t=2000
      expect(reportActivityMock).toHaveBeenCalledTimes(2);
    } finally {
      delete (document as unknown as { visibilityState?: string }).visibilityState;
      unmount();
    }
  });
});
