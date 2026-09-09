import { afterAll, beforeEach, afterEach } from 'vitest';
import { act } from 'react';

/* ============================ 组件级 resize 接线测试共用桩 ============================
 * jsdom 缺口补齐：pointer capture API 与 localStorage，供 AppShell/GitPanel/DiffViewer
 * 的 resize 持久化接线测试复用（原语本身的行为覆盖见 resize.test.tsx，此处仅做接线桩）。 */

/* ---------- pointer capture 记录型桩（jsdom 未实现；afterAll 恢复原状） ---------- */
// node 环境无 Element：nativeCapture 置空，installPointerCaptureStubs 直接跳过
const nativeCapture =
  typeof Element !== 'undefined'
    ? {
        set: Element.prototype.setPointerCapture,
        has: Element.prototype.hasPointerCapture,
        release: Element.prototype.releasePointerCapture,
      }
    : { set: undefined, has: undefined, release: undefined };
const capturedPointers = new Set<number>();

/** 每个测试文件环境调用一次（模块顶层）；无 DOM 环境为 no-op。 */
export function installPointerCaptureStubs(): void {
  if (typeof Element === 'undefined') return;
  Element.prototype.setPointerCapture = function setPointerCapture(pointerId: number) {
    capturedPointers.add(pointerId);
  };
  Element.prototype.hasPointerCapture = function hasPointerCapture(pointerId: number) {
    return capturedPointers.has(pointerId);
  };
  Element.prototype.releasePointerCapture = function releasePointerCapture(pointerId: number) {
    capturedPointers.delete(pointerId);
  };
  afterAll(() => {
    Element.prototype.setPointerCapture = nativeCapture.set!;
    Element.prototype.hasPointerCapture = nativeCapture.has!;
    Element.prototype.releasePointerCapture = nativeCapture.release!;
  });
}

/* ---------- localStorage 桩（beforeEach 清空、afterEach 还原；writes 计写入次数） ---------- */
const store = new Map<string, string>();
const writes = { count: 0 };
let savedStorage: Storage | undefined;
let storageHooked = false;

export function stubStorage(): { store: Map<string, string>; writes: { count: number } } {
  if (!storageHooked) {
    storageHooked = true;
    beforeEach(() => {
      store.clear();
      writes.count = 0;
      capturedPointers.clear();
      savedStorage = globalThis.localStorage;
      (globalThis as { localStorage: Storage }).localStorage = {
        getItem: (k: string) => (store.has(k) ? store.get(k)! : null),
        setItem: (k: string, v: string) => {
          writes.count += 1;
          store.set(k, String(v));
        },
        removeItem: (k: string) => {
          store.delete(k);
        },
        clear: () => {
          store.clear();
        },
        key: (i: number) => Array.from(store.keys())[i] ?? null,
        get length() {
          return store.size;
        },
      } as Storage;
    });
    afterEach(() => {
      (globalThis as { localStorage: Storage }).localStorage = savedStorage!;
    });
  }
  return { store, writes };
}

/* ---------- 事件辅助：jsdom 无 PointerEvent，用 MouseEvent + 自定义属性合成 ---------- */

export function pointer(
  type: string,
  clientX: number,
  opts: { button?: number; isPrimary?: boolean; pointerId?: number } = {},
): Event {
  const ev = new MouseEvent(type, {
    bubbles: true,
    cancelable: true,
    clientX,
    clientY: 10,
    button: opts.button ?? 0,
  });
  Object.defineProperty(ev, 'pointerId', { value: opts.pointerId ?? 1 });
  Object.defineProperty(ev, 'isPrimary', { value: opts.isPrimary ?? true });
  return ev;
}

/** down → 逐点 move → up 收尾（补发 lostpointercapture）。 */
export function drag(handle: HTMLElement, xs: number[]): void {
  act(() => handle.dispatchEvent(pointer('pointerdown', xs[0])));
  for (const x of xs.slice(1)) {
    act(() => handle.dispatchEvent(pointer('pointermove', x)));
  }
  const last = xs[xs.length - 1];
  act(() => handle.dispatchEvent(pointer('pointerup', last)));
  act(() => handle.dispatchEvent(pointer('lostpointercapture', last)));
}

export function keyDown(handle: HTMLElement, key: string, mods: { metaKey?: boolean } = {}): void {
  act(() =>
    handle.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true, ...mods })),
  );
}
