import { vi } from 'vitest';
import { act } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import type { ReactNode } from 'react';

/* ============================ CodeMirror 组件测试环境（tasks 4.4） ============================
 * jsdom 缺少 CM6 依赖的 ResizeObserver / requestAnimationFrame / matchMedia，
 * 这里安装最小桩；组件渲染统一走 mount()（createRoot + React.act）。 */

(globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;

if (typeof globalThis.ResizeObserver === 'undefined') {
  globalThis.ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  };
}
if (typeof globalThis.requestAnimationFrame === 'undefined') {
  globalThis.requestAnimationFrame = (cb: (t: number) => void) =>
    setTimeout(() => cb(Date.now()), 0) as unknown as number;
  globalThis.cancelAnimationFrame = (id: number) =>
    clearTimeout(id as unknown as ReturnType<typeof setTimeout>);
}
// jsdom 的 Range 缺几何 API；CM 鼠标选区路径（MouseSelection.isInPrimarySelection）会调用
if (typeof Range !== 'undefined' && !Range.prototype.getClientRects) {
  Range.prototype.getClientRects = () => [] as unknown as DOMRectList;
  Range.prototype.getBoundingClientRect = () => ({}) as DOMRect;
}

/** 覆盖 matchMedia（DiffViewer 形态默认值依赖）：matches=true 模拟 ≤1024px 窄屏。 */
export function stubMatchMedia(matches: boolean) {
  const mql = {
    matches,
    media: '(max-width: 1024px)',
    onchange: null,
    addEventListener: () => {},
    removeEventListener: () => {},
    addListener: () => {},
    removeListener: () => {},
    dispatchEvent: () => false,
  };
  Object.defineProperty(window, 'matchMedia', {
    configurable: true,
    writable: true,
    value: vi.fn(() => mql),
  });
}

/** 挂载组件并返回容器；unmount 触发清理（destroy 生命周期断言用）。 */
export function mount(ui: ReactNode): { container: HTMLElement; root: Root; unmount: () => void } {
  const container = document.createElement('div');
  document.body.appendChild(container);
  const root = createRoot(container);
  act(() => root.render(ui));
  return {
    container,
    root,
    unmount: () => {
      act(() => root.unmount());
      container.remove();
    },
  };
}

/** 重渲染（保持同一 root，验证状态保留与编辑器重建）。 */
export function rerender(root: Root, ui: ReactNode) {
  act(() => root.render(ui));
}

/** 等待懒加载 chunk 与组件内 async effect（动态 import + 编辑器创建）排空。 */
export async function flushUI() {
  for (let i = 0; i < 8; i++) {
    await act(async () => {
      await Promise.resolve();
    });
  }
  // 动态 import 的模块加载跨宏任务边界，补一个 setTimeout 轮次
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
  for (let i = 0; i < 8; i++) {
    await act(async () => {
      await Promise.resolve();
    });
  }
}
