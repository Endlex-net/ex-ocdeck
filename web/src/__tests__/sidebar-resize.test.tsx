// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act } from 'react';
import { AppShell } from '../components/AppShell';
import { mount } from './cm-test-env';
import { drag, installPointerCaptureStubs, stubStorage } from './dom-test-utils';

/* ============ D5 侧栏宽度持久化接线（fix-terminal-input-panel-resize 5.1/5.4） ============
 * 组件级接线验证：宽度经 --sidebar-w 下发、拖拽提交写 ocdeck:sidebar-width、刷新恢复、
 * ⌘B 折叠（只切 class 不动 width state）→ 展开恢复自定义宽度、窄屏顶栏形态不渲染把手。
 * resize 原语自身行为见 resize.test.tsx。 */

const mocks = vi.hoisted(() => ({ isNarrow: false }));

vi.mock('../hooks', () => ({
  useMediaQuery: () => mocks.isNarrow,
  useProjects: () => ({ projects: [], initialized: true, error: '' }),
}));
vi.mock('../components/ServerStatusBanner', () => ({ ServerStatusBanner: () => null }));

installPointerCaptureStubs();
const { store, writes } = stubStorage();

const WIDTH_KEY = 'ocdeck:sidebar-width';

function mountShell() {
  return mount(
    <AppShell
      onOpenPalette={() => {}}
      onToggleTheme={() => {}}
      themePref="system"
      paletteHotkeyLabel="⌘K"
    >
      <div>page</div>
    </AppShell>,
  );
}

function sidebarVar(container: HTMLElement): string {
  return container.querySelector<HTMLElement>('.od-shell')!.style.getPropertyValue('--sidebar-w');
}

beforeEach(() => {
  mocks.isNarrow = false;
});

afterEach(() => {
  document.body.classList.remove('od-side-collapsed'); // AppShell 卸载不清理 body class
});

describe('D5 侧栏宽度持久化接线（AppShell）', () => {
  it('持久化宽度读取：--sidebar-w 下发 + 把手渲染在壳层 flex 链上', () => {
    store.set(WIDTH_KEY, '400');
    const { container, unmount } = mountShell();
    expect(sidebarVar(container)).toBe('400px');
    expect(container.querySelector('.od-shell > .od-resize-handle')).not.toBeNull();
    unmount();
  });

  it('拖拽提交写入存储一次；刷新（卸载重挂）恢复拖拽后宽度', () => {
    const first = mountShell();
    drag(first.container.querySelector<HTMLElement>('.od-resize-handle')!, [300, 382]); // 232 + 82
    expect(store.get(WIDTH_KEY)).toBe('314');
    expect(writes.count).toBe(1);
    expect(sidebarVar(first.container)).toBe('314px');
    first.unmount();

    const again = mountShell();
    expect(sidebarVar(again.container)).toBe('314px');
    again.unmount();
  });

  it('拖拽越界 clamp 到 [180,480]', () => {
    const low = mountShell();
    drag(low.container.querySelector<HTMLElement>('.od-resize-handle')!, [300, 0]); // 232 - 300
    expect(store.get(WIDTH_KEY)).toBe('180');
    low.unmount();

    const high = mountShell();
    drag(high.container.querySelector<HTMLElement>('.od-resize-handle')!, [300, 900]); // 232 + 600
    expect(store.get(WIDTH_KEY)).toBe('480');
    high.unmount();
  });

  it('⌘B 折叠隐藏把手但不动 width state；展开恢复自定义宽度，存储未被改写', () => {
    store.set(WIDTH_KEY, '400');
    const { container, unmount } = mountShell();
    expect(container.querySelector('.od-resize-handle')).not.toBeNull();
    const writesBefore = writes.count; // ⌘B 自身会持久化 ocdeck:side-collapsed，只看宽度键

    const cmdB = () =>
      act(() =>
        window.dispatchEvent(
          new KeyboardEvent('keydown', { key: 'b', metaKey: true, cancelable: true }),
        ),
      );
    cmdB();
    expect(document.body.classList.contains('od-side-collapsed')).toBe(true);
    expect(container.querySelector('.od-resize-handle')).toBeNull();
    expect(sidebarVar(container)).toBe('400px'); // 折叠只切 body class，width state 未动

    cmdB();
    expect(document.body.classList.contains('od-side-collapsed')).toBe(false);
    expect(container.querySelector('.od-resize-handle')).not.toBeNull();
    expect(sidebarVar(container)).toBe('400px'); // 展开恢复自定义宽度
    expect(store.get(WIDTH_KEY)).toBe('400');
    // 折叠/展开各持久化一次 ocdeck:side-collapsed；宽度键从未被写
    expect(writes.count - writesBefore).toBe(2);
    unmount();
  });

  it('≤767px 顶栏形态不渲染把手', () => {
    mocks.isNarrow = true;
    const { container, unmount } = mountShell();
    expect(container.querySelector('.od-resize-handle')).toBeNull();
    unmount();
  });
});
