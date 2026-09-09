// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act } from 'react';
import { GitPanel } from '../components/GitPanel';
import { api } from '../api';
import { flushUI, mount } from './cm-test-env';
import { drag, installPointerCaptureStubs, stubStorage } from './dom-test-utils';

/* ============ D6 git 文件面板宽度持久化 + 收起态 + 断点（fix-terminal-input-panel-resize 5.3/5.4 + Gate5 L4） ============
 * 组件级接线验证：宽度经 --git-side-w 内联变量下发、拖拽提交写 ocdeck:git-side-width、刷新恢复、
 * 收起 = 36px 窄条（class 切换，会话内不持久化）→ 展开恢复收起前宽度、
 * ≤1024px 堆叠态强制完整显示（隐藏把手与收起按钮，可经 media change 在同一挂载实例上往返）。
 * 原语行为见 resize.test.tsx。 */

type MediaListener = (e: { matches: boolean }) => void;

/** 可控 media 状态 + 可发 change 的 matchMedia 桩（cm-test-env 的 stubMatchMedia 不支持触发 change）。 */
const media = { narrow: false };
const mediaListeners = new Set<MediaListener>();

function installControllableMatchMedia(): void {
  Object.defineProperty(window, 'matchMedia', {
    configurable: true,
    writable: true,
    value: (query: string) => ({
      matches: query.includes('1024') ? media.narrow : false,
      media: query,
      onchange: null,
      addEventListener: (_type: string, cb: MediaListener) => mediaListeners.add(cb),
      removeEventListener: (_type: string, cb: MediaListener) => mediaListeners.delete(cb),
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    }),
  });
}

function setNarrow(next: boolean): void {
  media.narrow = next;
  act(() => {
    for (const cb of [...mediaListeners]) cb({ matches: next });
  });
}

vi.mock('../api', () => ({
  api: {
    gitStatus: vi.fn(),
    gitDiff: vi.fn(),
    gitCommit: vi.fn(),
    gitPush: vi.fn(),
    listAnnotations: vi.fn(),
    createAnnotation: vi.fn(),
    gitFileRead: vi.fn(),
    gitFileWrite: vi.fn(),
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
const listAnnotationsMock = vi.mocked(api.listAnnotations);

installPointerCaptureStubs();
const { store, writes } = stubStorage();

const WIDTH_KEY = 'ocdeck:git-side-width';

function mountPanel() {
  return mount(<GitPanel taskID="t1" active />);
}

function gitSideVar(container: HTMLElement): string {
  return container.querySelector<HTMLElement>('.git-side')!.style.getPropertyValue('--git-side-w');
}

beforeEach(() => {
  vi.clearAllMocks();
  media.narrow = false;
  mediaListeners.clear();
  installControllableMatchMedia();
  gitStatusMock.mockResolvedValue({ branch: 'main', files: [] });
  listAnnotationsMock.mockResolvedValue({
    annotations: [],
    submitCapability: { state: 'unknown', reason: '' },
  });
});

describe('D6 git 文件面板宽度持久化接线（GitPanel）', () => {
  it('持久化宽度读取 + 拖拽提交写入一次 + 刷新恢复', async () => {
    store.set(WIDTH_KEY, '500');
    const first = mountPanel();
    await flushUI();
    expect(gitSideVar(first.container)).toBe('500px');
    drag(first.container.querySelector<HTMLElement>('.od-resize-handle')!, [300, 340]); // 500 + 40
    expect(store.get(WIDTH_KEY)).toBe('540');
    expect(writes.count).toBe(1);
    expect(gitSideVar(first.container)).toBe('540px');
    first.unmount();

    const again = mountPanel();
    await flushUI();
    expect(gitSideVar(again.container)).toBe('540px');
    again.unmount();
  });

  it('默认 340 起拖越界 clamp 到 600', async () => {
    const { container, unmount } = mountPanel();
    await flushUI();
    expect(gitSideVar(container)).toBe('340px');
    drag(container.querySelector<HTMLElement>('.od-resize-handle')!, [300, 900]); // 340 + 600
    expect(store.get(WIDTH_KEY)).toBe('600');
    unmount();
  });

  it('收起 → 36px 窄条（class 切换）+ 把手隐藏；展开恢复收起前宽度，全程不写存储', async () => {
    store.set(WIDTH_KEY, '500');
    const { container, unmount } = mountPanel();
    await flushUI();

    act(() => container.querySelector<HTMLButtonElement>('.git-side-collapse')!.click());
    const collapsedSide = container.querySelector<HTMLElement>('.git-side')!;
    expect(collapsedSide.className).toContain('git-side-collapsed');
    expect(container.querySelector('.od-resize-handle')).toBeNull(); // 收起态隐藏把手
    expect(gitSideVar(container)).toBe('500px'); // 宽度变量未动（36px 由 class 规则呈现）
    expect(container.querySelector<HTMLButtonElement>('.git-side-expand')).not.toBeNull();

    act(() => container.querySelector<HTMLButtonElement>('.git-side-expand')!.click());
    expect(container.querySelector<HTMLElement>('.git-side')!.className).not.toContain(
      'git-side-collapsed',
    );
    expect(gitSideVar(container)).toBe('500px'); // 恢复收起前宽度
    expect(container.querySelector('.od-resize-handle')).not.toBeNull();
    expect(store.get(WIDTH_KEY)).toBe('500');
    expect(writes.count).toBe(0);
    unmount();
  });

  it('收起态会话内不持久化：卸载重挂恢复展开', async () => {
    const first = mountPanel();
    await flushUI();
    act(() => first.container.querySelector<HTMLButtonElement>('.git-side-collapse')!.click());
    expect(first.container.querySelector<HTMLElement>('.git-side')!.className).toContain(
      'git-side-collapsed',
    );
    first.unmount();

    const again = mountPanel();
    await flushUI();
    expect(again.container.querySelector<HTMLElement>('.git-side')!.className).not.toContain(
      'git-side-collapsed',
    );
    expect(again.container.querySelector('.od-resize-handle')).not.toBeNull();
    again.unmount();
  });

  it('D6 断点状态（≤1024px 堆叠态）：隐藏把手与收起按钮，面板强制完整显示', () => {
    media.narrow = true;
    const { container, unmount } = mountPanel();
    expect(container.querySelector('.git-side-collapse')).toBeNull();
    expect(container.querySelector('.od-resize-handle')).toBeNull();
    expect(container.querySelector('.git-side-collapsed')).toBeNull(); // 不进入收起态
    expect(container.querySelector('.git-side')).not.toBeNull();
    unmount();
  });

  it('L4 断点往返（同一挂载实例）：桌面收起 → 窄屏完整显示 → 桌面恢复收起 → 展开恢复宽度', async () => {
    store.set(WIDTH_KEY, '500');
    const { container, unmount } = mountPanel();
    await flushUI();

    // 桌面：收起
    act(() => container.querySelector<HTMLButtonElement>('.git-side-collapse')!.click());
    expect(container.querySelector<HTMLElement>('.git-side')!.className).toContain(
      'git-side-collapsed',
    );
    expect(container.querySelector('.od-resize-handle')).toBeNull();

    // 切窄屏（media change，不重挂）：收起偏好不生效，强制完整显示
    setNarrow(true);
    expect(container.querySelector<HTMLElement>('.git-side')!.className).not.toContain(
      'git-side-collapsed',
    );
    expect(container.querySelector('.git-side-collapse')).toBeNull();
    expect(container.querySelector('.od-resize-handle')).toBeNull();

    // 回桌面：收起偏好保留，恢复窄条
    setNarrow(false);
    expect(container.querySelector<HTMLElement>('.git-side')!.className).toContain(
      'git-side-collapsed',
    );

    // 展开：恢复收起前宽度，把手回归
    act(() => container.querySelector<HTMLButtonElement>('.git-side-expand')!.click());
    expect(container.querySelector<HTMLElement>('.git-side')!.className).not.toContain(
      'git-side-collapsed',
    );
    expect(gitSideVar(container)).toBe('500px');
    expect(container.querySelector('.od-resize-handle')).not.toBeNull();
    expect(writes.count).toBe(0); // 收起/断点/展开全程不写宽度存储
    unmount();
  });
});
