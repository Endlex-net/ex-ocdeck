// @vitest-environment jsdom
import { afterAll, describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act, useEffect, useLayoutEffect } from 'react';
import { ResizeHandle, usePersistedSize, type ResizeMode } from '../components/resize';
import { mount, rerender } from './cm-test-env';

/* ============================ 共享 resize 原语（design D4 / tasks 1.4） ============================
 * 覆盖：pointerup→lostpointercapture 序列（不回退只写一次）、拖动取消恢复起始值、
 * 拖拽中失效（enabled 变 false / handle 卸载）按 cancelled 结束、禁用期对外返回默认值且不访问存储、
 * 存储读取异常回退各分支、px 取整 clamp / 比例换算 / 容器零宽不启动、键盘步进与保存、
 * 指针事务隔离（启动 pointerId）、capture 建立/保存/释放顺序。 */

/* ---------- pointer capture 记录型桩（jsdom 未实现；afterAll 恢复原状） ---------- */
const capturedPointers = new Set<number>();
const eventLog: string[] = [];
const nativeCapture = {
  set: Element.prototype.setPointerCapture,
  has: Element.prototype.hasPointerCapture,
  release: Element.prototype.releasePointerCapture,
};
Element.prototype.setPointerCapture = function setPointerCapture(pointerId: number) {
  capturedPointers.add(pointerId);
  eventLog.push(`capture:${pointerId}`);
};
Element.prototype.hasPointerCapture = function hasPointerCapture(pointerId: number) {
  return capturedPointers.has(pointerId);
};
Element.prototype.releasePointerCapture = function releasePointerCapture(pointerId: number) {
  capturedPointers.delete(pointerId);
  eventLog.push(`release:${pointerId}`);
};
afterAll(() => {
  Element.prototype.setPointerCapture = nativeCapture.set;
  Element.prototype.hasPointerCapture = nativeCapture.has;
  Element.prototype.releasePointerCapture = nativeCapture.release;
});

const store = new Map<string, string>();
let savedStorage: Storage | undefined;
let writeCount = 0;

const PX_KEY = 'ocdeck:sidebar-width';
const RATIO_KEY = 'ocdeck:diff-split-ratio';

beforeEach(() => {
  store.clear();
  writeCount = 0;
  capturedPointers.clear();
  eventLog.length = 0;
  savedStorage = globalThis.localStorage;
  (globalThis as { localStorage: Storage }).localStorage = {
    getItem: (k: string) => (store.has(k) ? store.get(k)! : null),
    setItem: (k: string, v: string) => {
      writeCount += 1;
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
  vi.restoreAllMocks();
});

/* ---------- 测试挂载壳：usePersistedSize + ResizeHandle 组合 ---------- */

interface HarnessProps {
  mode: ResizeMode;
  storageKey: string;
  defaultValue: number;
  min: number;
  max: number;
  containerWidth?: number;
  enabled?: boolean;
  removeWhenDisabled?: boolean;
}

function Harness(props: HarnessProps) {
  const { mode, storageKey, defaultValue, min, max, containerWidth, enabled = true, removeWhenDisabled } = props;
  const size = usePersistedSize(storageKey, defaultValue, min, max, enabled);
  return (
    <div style={{ position: 'relative', width: 800, height: 40 }}>
      <span data-testid="size">{size.value}</span>
      {(!removeWhenDisabled || enabled) && (
        <ResizeHandle
          mode={mode}
          value={size.value}
          min={min}
          max={max}
          containerWidth={containerWidth}
          onDrag={size.setSize}
          onCommit={size.commitSize}
          enabled={enabled}
        />
      )}
    </div>
  );
}

/** 直接驱动 hook 的壳：按钮分别触发 commitSize / setSize（禁用契约用）。 */
function HookHarness({ enabled }: { enabled: boolean }) {
  const size = usePersistedSize(PX_KEY, 232, 180, 480, enabled);
  return (
    <div>
      <span data-testid="size">{size.value}</span>
      <button type="button" data-testid="commit-350" onClick={() => size.commitSize(350)} />
      <button type="button" data-testid="set-360" onClick={() => size.setSize(360)} />
    </div>
  );
}

/** 父级 layout 阶段壳：在禁用提交的 layout 阶段（子组件失效处理之后、被动 effect 之前）
 * 检查事务已清（capture 已释放）并注入迟到指针事件——区分 layout 取消与被动 effect 取消。 */
function LayoutPhaseHarness({ enabled, phaseLog }: { enabled: boolean; phaseLog: string[] }) {
  const size = usePersistedSize(PX_KEY, 232, 180, 480, enabled);
  useLayoutEffect(() => {
    if (enabled) return;
    // 子组件（ResizeHandle）的 layout 取消已执行；被动 effect 尚未运行
    phaseLog.push(`layout:captureReleased=${!capturedPointers.has(7)}`);
    const handle = document.querySelector<HTMLElement>('.od-resize-handle');
    handle?.dispatchEvent(pointer('pointerup', 150, { pointerId: 7 }));
    handle?.dispatchEvent(pointer('lostpointercapture', 150, { pointerId: 7 }));
    phaseLog.push('layout:lateEventsDispatched');
  }, [enabled, phaseLog]);
  useEffect(() => {
    phaseLog.push('passive'); // 被动 effect 标记（时序对照）
  }, [enabled, phaseLog]);
  return (
    <div style={{ position: 'relative', width: 800, height: 40 }}>
      <span data-testid="size">{size.value}</span>
      <ResizeHandle
        mode="px"
        value={size.value}
        min={180}
        max={480}
        onDrag={size.setSize}
        onCommit={size.commitSize}
        enabled={enabled}
      />
    </div>
  );
}

function pxHarness(enabled?: boolean) {
  return <Harness mode="px" storageKey={PX_KEY} defaultValue={232} min={180} max={480} enabled={enabled} />;
}

function ratioHarness(containerWidth: number, enabled?: boolean, removeWhenDisabled?: boolean) {
  return (
    <Harness
      mode="ratio"
      storageKey={RATIO_KEY}
      defaultValue={0.5}
      min={0.2}
      max={0.8}
      containerWidth={containerWidth}
      enabled={enabled}
      removeWhenDisabled={removeWhenDisabled}
    />
  );
}

/* ---------- 事件辅助：jsdom 无 PointerEvent，用 MouseEvent + 自定义属性合成 ---------- */

function pointer(
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

function handleOf(container: HTMLElement): HTMLElement {
  return container.querySelector<HTMLElement>('.od-resize-handle')!;
}

function shownSize(container: HTMLElement): string {
  return container.querySelector<HTMLElement>('[data-testid="size"]')!.textContent ?? '';
}

function keyDown(handle: HTMLElement, key: string) {
  act(() => handle.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true })));
}

/** down → 逐点 move → up/cancel/lost 收尾（up 与 cancel 后均补发 lostpointercapture）。 */
function drag(handle: HTMLElement, xs: number[], end: 'up' | 'cancel' | 'lost') {
  act(() => handle.dispatchEvent(pointer('pointerdown', xs[0])));
  for (const x of xs.slice(1)) {
    act(() => handle.dispatchEvent(pointer('pointermove', x)));
  }
  const last = xs[xs.length - 1];
  if (end === 'cancel') {
    act(() => handle.dispatchEvent(pointer('pointercancel', last)));
    act(() => handle.dispatchEvent(pointer('lostpointercapture', last)));
  } else if (end === 'lost') {
    act(() => handle.dispatchEvent(pointer('lostpointercapture', last)));
  } else {
    act(() => handle.dispatchEvent(pointer('pointerup', last)));
    act(() => handle.dispatchEvent(pointer('lostpointercapture', last)));
  }
}

describe('ResizeHandle 渲染语义', () => {
  it('6px 热区、separator/vertical、tabIndex、aria 值域反映当前尺寸', () => {
    const mounted = mount(pxHarness());
    const handle = handleOf(mounted.container);
    expect(handle.getAttribute('role')).toBe('separator');
    expect(handle.getAttribute('aria-orientation')).toBe('vertical');
    expect(handle.getAttribute('tabindex')).toBe('0');
    expect(handle.getAttribute('aria-valuenow')).toBe('232');
    expect(handle.getAttribute('aria-valuemin')).toBe('180');
    expect(handle.getAttribute('aria-valuemax')).toBe('480');
    expect(handle.style.width).toBe('6px');
    mounted.unmount();
  });
});

describe('拖拽状态机', () => {
  it('pointerup → lostpointercapture 序列：提交一次、不回退、不再写', () => {
    const mounted = mount(pxHarness());
    const handle = handleOf(mounted.container);
    act(() => handle.dispatchEvent(pointer('pointerdown', 100)));
    act(() => handle.dispatchEvent(pointer('pointermove', 130)));
    act(() => handle.dispatchEvent(pointer('pointermove', 150)));
    expect(shownSize(mounted.container)).toBe('282'); // 拖动中仅内存
    expect(store.has(PX_KEY)).toBe(false);
    expect(writeCount).toBe(0);
    act(() => handle.dispatchEvent(pointer('pointerup', 150)));
    expect(shownSize(mounted.container)).toBe('282');
    expect(store.get(PX_KEY)).toBe('282');
    expect(writeCount).toBe(1);
    // 此后到达的 lostpointercapture：仅清理，不回滚不再写
    act(() => handle.dispatchEvent(pointer('lostpointercapture', 150)));
    expect(shownSize(mounted.container)).toBe('282');
    expect(store.get(PX_KEY)).toBe('282');
    expect(writeCount).toBe(1);
    mounted.unmount();
  });

  it('pointercancel → cancelled：恢复起始值、不写存储', () => {
    const mounted = mount(pxHarness());
    drag(handleOf(mounted.container), [100, 150], 'cancel');
    expect(shownSize(mounted.container)).toBe('232');
    expect(store.has(PX_KEY)).toBe(false);
    expect(writeCount).toBe(0);
    mounted.unmount();
  });

  it('拖拽中意外 lostpointercapture → cancelled；迟到 pointerup 不提交', () => {
    const mounted = mount(pxHarness());
    const handle = handleOf(mounted.container);
    act(() => handle.dispatchEvent(pointer('pointerdown', 100)));
    act(() => handle.dispatchEvent(pointer('pointermove', 150)));
    act(() => handle.dispatchEvent(pointer('lostpointercapture', 150)));
    expect(shownSize(mounted.container)).toBe('232');
    expect(writeCount).toBe(0);
    act(() => handle.dispatchEvent(pointer('pointerup', 150))); // 迟到
    expect(shownSize(mounted.container)).toBe('232');
    expect(writeCount).toBe(0);
    mounted.unmount();
  });

  it('拖拽中 enabled 变 false（切 unified/跨断点等布局失效）→ cancelled；返回后显示起始值', () => {
    const mounted = mount(pxHarness(true));
    const handle = handleOf(mounted.container);
    act(() => handle.dispatchEvent(pointer('pointerdown', 100)));
    act(() => handle.dispatchEvent(pointer('pointermove', 150)));
    expect(shownSize(mounted.container)).toBe('282');
    rerender(mounted.root, pxHarness(false)); // handle 保留、disabled
    expect(shownSize(mounted.container)).toBe('232'); // 恢复起始值（此例起始值=默认值）
    expect(store.has(PX_KEY)).toBe(false);
    act(() => handle.dispatchEvent(pointer('pointerup', 150))); // 迟到不提交
    expect(writeCount).toBe(0);
    rerender(mounted.root, pxHarness(true)); // 返回
    expect(shownSize(mounted.container)).toBe('232');
    expect(writeCount).toBe(0);
    mounted.unmount();
  });

  it('非默认起始值拖动 → 失效 → 返回：恢复起始值（内存保留语义）', () => {
    store.set(PX_KEY, '400');
    const mounted = mount(pxHarness(true));
    expect(shownSize(mounted.container)).toBe('400');
    const handle = handleOf(mounted.container);
    act(() => handle.dispatchEvent(pointer('pointerdown', 100)));
    act(() => handle.dispatchEvent(pointer('pointermove', 150))); // 内存 450
    expect(shownSize(mounted.container)).toBe('450');
    rerender(mounted.root, pxHarness(false)); // 失效 → cancelled
    expect(shownSize(mounted.container)).toBe('232'); // 禁用对外返回默认值
    rerender(mounted.root, pxHarness(true)); // 返回
    expect(shownSize(mounted.container)).toBe('400'); // 恢复起始值
    expect(store.get(PX_KEY)).toBe('400'); // 种子未被改写
    expect(writeCount).toBe(0);
    mounted.unmount();
  });

  it('禁用提交的 layout 阶段：事务已清（capture 已释放）、迟到指针事件不提交、再启用恢复非默认起始值', () => {
    store.set(PX_KEY, '400');
    const phaseLog: string[] = [];
    const mounted = mount(<LayoutPhaseHarness enabled={true} phaseLog={phaseLog} />);
    const handle = handleOf(mounted.container);
    act(() => handle.dispatchEvent(pointer('pointerdown', 100, { pointerId: 7 })));
    act(() => handle.dispatchEvent(pointer('pointermove', 150, { pointerId: 7 })));
    expect(shownSize(mounted.container)).toBe('450'); // 非默认起始值 400 上拖动
    expect(capturedPointers.has(7)).toBe(true);

    rerender(mounted.root, <LayoutPhaseHarness enabled={false} phaseLog={phaseLog} />);
    // layout 阶段断言：失效已同步生效（capture 已释放），迟到事件发生在被动 effect 之前——
    // 若取消仍在被动 effect（旧实现），此阶段 capture 未释放，断言失败
    expect(phaseLog).toEqual([
      'passive',
      'layout:captureReleased=true',
      'layout:lateEventsDispatched',
      'passive',
    ]);
    expect(writeCount).toBe(0);
    expect(store.get(PX_KEY)).toBe('400'); // 迟到事件未提交，种子未被改写

    rerender(mounted.root, <LayoutPhaseHarness enabled={true} phaseLog={phaseLog} />);
    expect(shownSize(mounted.container)).toBe('400'); // 恢复非默认起始值
    mounted.unmount();
  });

  it('拖拽中禁用（保留 handle）→ cancelled 并清事务后释放 capture', () => {
    const mounted = mount(pxHarness(true));
    const handle = handleOf(mounted.container);
    act(() => handle.dispatchEvent(pointer('pointerdown', 100, { pointerId: 3 })));
    act(() => handle.dispatchEvent(pointer('pointermove', 150, { pointerId: 3 })));
    expect(capturedPointers.has(3)).toBe(true);
    rerender(mounted.root, pxHarness(false));
    expect(capturedPointers.has(3)).toBe(false); // 清事务后释放
    expect(shownSize(mounted.container)).toBe('232');
    expect(writeCount).toBe(0);
    mounted.unmount();
  });

  it('capture 顺序：down 捕获 → up 保存一次 → 最后释放（用启动 pointerId）', () => {
    const mounted = mount(pxHarness());
    const handle = handleOf(mounted.container);
    const storage = globalThis.localStorage;
    const origSetItem = storage.setItem;
    vi.spyOn(storage, 'setItem').mockImplementation((k: string, v: string) => {
      eventLog.push(`write:${k}`);
      origSetItem.call(storage, k, v);
    });
    act(() => handle.dispatchEvent(pointer('pointerdown', 100, { pointerId: 7 })));
    act(() => handle.dispatchEvent(pointer('pointermove', 150, { pointerId: 7 })));
    act(() => handle.dispatchEvent(pointer('pointerup', 150, { pointerId: 7 })));
    act(() => handle.dispatchEvent(pointer('lostpointercapture', 150, { pointerId: 7 })));
    expect(eventLog).toEqual(['capture:7', 'write:ocdeck:sidebar-width', 'release:7']);
    expect(shownSize(mounted.container)).toBe('282');
    mounted.unmount();
  });

  it('拖拽中 handle 卸载（实例销毁路径）→ cancelled；返回后显示起始值', () => {
    const mounted = mount(ratioHarness(800, true, true));
    const handle = handleOf(mounted.container);
    act(() => handle.dispatchEvent(pointer('pointerdown', 100)));
    act(() => handle.dispatchEvent(pointer('pointermove', 200)));
    expect(shownSize(mounted.container)).toBe('0.625');
    rerender(mounted.root, ratioHarness(800, false, true)); // handle 卸载
    expect(shownSize(mounted.container)).toBe('0.5');
    expect(store.has(RATIO_KEY)).toBe(false);
    expect(writeCount).toBe(0);
    rerender(mounted.root, ratioHarness(800, true, true)); // 返回
    expect(shownSize(mounted.container)).toBe('0.5');
    expect(writeCount).toBe(0);
    mounted.unmount();
  });

  it('其他指针不影响活动拖拽，原指针仍正常提交', () => {
    const mounted = mount(pxHarness());
    const handle = handleOf(mounted.container);
    act(() => handle.dispatchEvent(pointer('pointerdown', 100, { pointerId: 1 })));
    act(() => handle.dispatchEvent(pointer('pointermove', 200, { pointerId: 2 }))); // 第二根手指
    expect(shownSize(mounted.container)).toBe('232');
    act(() => handle.dispatchEvent(pointer('pointerup', 200, { pointerId: 2 }))); // 其他指针 up 不提交
    expect(writeCount).toBe(0);
    expect(capturedPointers.has(1)).toBe(true); // 事务仍在
    act(() => handle.dispatchEvent(pointer('pointermove', 150, { pointerId: 1 }))); // 原指针继续
    expect(shownSize(mounted.container)).toBe('282');
    act(() => handle.dispatchEvent(pointer('lostpointercapture', 150, { pointerId: 2 }))); // 其他指针 lost：活动事务不被取消
    expect(shownSize(mounted.container)).toBe('282');
    expect(capturedPointers.has(1)).toBe(true); // 事务仍在
    act(() => handle.dispatchEvent(pointer('pointerup', 150, { pointerId: 1 }))); // 原指针提交
    expect(shownSize(mounted.container)).toBe('282');
    expect(store.get(PX_KEY)).toBe('282');
    expect(writeCount).toBe(1);
    expect(capturedPointers.has(1)).toBe(false); // 释放用事务记录的 pointerId
    mounted.unmount();
  });

  it('其他指针的 pointercancel 不取消活动拖拽，原指针取消仍恢复', () => {
    const mounted = mount(pxHarness());
    const handle = handleOf(mounted.container);
    act(() => handle.dispatchEvent(pointer('pointerdown', 100, { pointerId: 1 })));
    act(() => handle.dispatchEvent(pointer('pointermove', 150, { pointerId: 1 })));
    act(() => handle.dispatchEvent(pointer('pointercancel', 150, { pointerId: 2 }))); // 其他指针
    expect(shownSize(mounted.container)).toBe('282'); // 仍在拖拽
    act(() => handle.dispatchEvent(pointer('pointermove', 160, { pointerId: 1 })));
    expect(shownSize(mounted.container)).toBe('292');
    act(() => handle.dispatchEvent(pointer('pointercancel', 160, { pointerId: 1 }))); // 原指针取消
    expect(shownSize(mounted.container)).toBe('232');
    expect(writeCount).toBe(0);
    mounted.unmount();
  });

  it('仅 isPrimary + 左键启动拖拽', () => {
    const mounted = mount(pxHarness());
    const handle = handleOf(mounted.container);
    act(() => handle.dispatchEvent(pointer('pointerdown', 100, { button: 2 })));
    act(() => handle.dispatchEvent(pointer('pointermove', 150)));
    expect(shownSize(mounted.container)).toBe('232');
    act(() => handle.dispatchEvent(pointer('pointerup', 150)));
    act(() => handle.dispatchEvent(pointer('pointerdown', 100, { isPrimary: false })));
    act(() => handle.dispatchEvent(pointer('pointermove', 150)));
    expect(shownSize(mounted.container)).toBe('232');
    expect(writeCount).toBe(0);
    mounted.unmount();
  });
});

describe('单位适配', () => {
  it('px 拖拽取整后 clamp，提交保存一次', () => {
    const mounted = mount(pxHarness());
    const handle = handleOf(mounted.container);
    act(() => handle.dispatchEvent(pointer('pointerdown', 100)));
    act(() => handle.dispatchEvent(pointer('pointermove', 150.4))); // 282.4 → 282
    expect(shownSize(mounted.container)).toBe('282');
    act(() => handle.dispatchEvent(pointer('pointermove', 999))); // clamp 到 480
    expect(shownSize(mounted.container)).toBe('480');
    act(() => handle.dispatchEvent(pointer('pointermove', -600))); // clamp 到 180
    expect(shownSize(mounted.container)).toBe('180');
    act(() => handle.dispatchEvent(pointer('pointerup', -600)));
    expect(store.get(PX_KEY)).toBe('180');
    expect(writeCount).toBe(1);
    mounted.unmount();
  });

  it('比例拖拽 ratio = clamp(startRatio + deltaPx/containerWidth)', () => {
    const mounted = mount(ratioHarness(800));
    const handle = handleOf(mounted.container);
    act(() => handle.dispatchEvent(pointer('pointerdown', 100)));
    act(() => handle.dispatchEvent(pointer('pointermove', 200))); // +100/800 → 0.625
    expect(shownSize(mounted.container)).toBe('0.625');
    act(() => handle.dispatchEvent(pointer('pointermove', 999))); // clamp 0.8
    expect(shownSize(mounted.container)).toBe('0.8');
    act(() => handle.dispatchEvent(pointer('pointerup', 999)));
    expect(store.get(RATIO_KEY)).toBe('0.8');
    expect(writeCount).toBe(1);
    mounted.unmount();
  });

  it('比例模式容器零宽不启动拖拽', () => {
    const mounted = mount(ratioHarness(0));
    drag(handleOf(mounted.container), [100, 200], 'up');
    expect(shownSize(mounted.container)).toBe('0.5');
    expect(store.has(RATIO_KEY)).toBe(false);
    expect(writeCount).toBe(0);
    mounted.unmount();
  });
});

describe('键盘步进', () => {
  it('px 方向键步进 10px 立即保存；到界后无效调整不保存；非方向键不响应', () => {
    store.set(PX_KEY, '480');
    const mounted = mount(pxHarness());
    expect(shownSize(mounted.container)).toBe('480');
    const handle = handleOf(mounted.container);
    keyDown(handle, 'ArrowRight'); // 已到上限：无效调整
    expect(shownSize(mounted.container)).toBe('480');
    expect(writeCount).toBe(0);
    keyDown(handle, 'ArrowLeft');
    expect(shownSize(mounted.container)).toBe('470');
    keyDown(handle, 'ArrowLeft');
    expect(shownSize(mounted.container)).toBe('460');
    keyDown(handle, 'PageDown');
    expect(shownSize(mounted.container)).toBe('460');
    expect(writeCount).toBe(2);
    mounted.unmount();
  });

  it('比例模式键盘按 10px/容器宽度换算并保存', () => {
    const mounted = mount(ratioHarness(1000));
    const handle = handleOf(mounted.container);
    keyDown(handle, 'ArrowRight'); // +10/1000
    expect(Number(shownSize(mounted.container))).toBeCloseTo(0.51, 10);
    expect(Number(store.get(RATIO_KEY))).toBeCloseTo(0.51, 10);
    keyDown(handle, 'ArrowLeft');
    expect(Number(shownSize(mounted.container))).toBeCloseTo(0.5, 10);
    expect(writeCount).toBe(2);
    mounted.unmount();
  });
});

describe('usePersistedSize 存储契约', () => {
  it.each([
    ['解析失败', 'abc'],
    ['空串', ''],
    ['null 字面量', 'null'],
    ['字符串型数字', '"340"'],
    ['非有限 number', '1e999'],
    ['非整数 px（不取整回退）', '340.5'],
  ])('px 存储非法值回退默认：%s', (_name, raw) => {
    store.set(PX_KEY, raw);
    const { container, unmount } = mount(pxHarness());
    expect(shownSize(container)).toBe('232');
    expect(writeCount).toBe(0); // 读不写存储
    unmount();
  });

  it.each([
    ['越界上限 clamp', '999', '480'],
    ['越界下限 clamp', '50', '180'],
    ['合法值原样', '400', '400'],
  ])('px 存储越界 clamp：%s', (_name, raw, expected) => {
    store.set(PX_KEY, raw);
    const { container, unmount } = mount(pxHarness());
    expect(shownSize(container)).toBe(expected);
    expect(writeCount).toBe(0);
    unmount();
  });

  it.each([
    ['合法比例', '0.7', '0.7'],
    ['越界下限 clamp', '0.1', '0.2'],
    ['越界上限 clamp', '0.9', '0.8'],
    ['字符串型数字回退', '"0.7"', '0.5'],
    ['解析失败回退', 'oops', '0.5'],
    ['非有限 number 回退', '1e999', '0.5'],
  ])('比例存储读取：%s', (_name, raw, expected) => {
    store.set(RATIO_KEY, raw);
    const { container, unmount } = mount(ratioHarness(800));
    expect(shownSize(container)).toBe(expected);
    expect(writeCount).toBe(0);
    unmount();
  });

  it('enabled=false 不读存储返回默认；首次翻 true 读一次；禁用返回默认、内存保留、再启用恢复', () => {
    store.set(PX_KEY, '400');
    const getSpy = vi.spyOn(globalThis.localStorage, 'getItem');
    const mounted = mount(pxHarness(false));
    expect(shownSize(mounted.container)).toBe('232');
    expect(getSpy).not.toHaveBeenCalled();

    rerender(mounted.root, pxHarness(true));
    expect(shownSize(mounted.container)).toBe('400');
    expect(getSpy).toHaveBeenCalledTimes(1);

    // 禁用：对外返回默认值（内存保留）；再启用恢复；不重复读
    rerender(mounted.root, pxHarness(false));
    expect(shownSize(mounted.container)).toBe('232');
    rerender(mounted.root, pxHarness(true));
    expect(shownSize(mounted.container)).toBe('400');
    expect(getSpy).toHaveBeenCalledTimes(1);
    expect(writeCount).toBe(0);
    mounted.unmount();
  });

  it('禁用期间直接驱动 hook：commitSize 不访问存储、对外返回默认、内存保留再启用恢复', () => {
    const getSpy = vi.spyOn(globalThis.localStorage, 'getItem');
    const mounted = mount(<HookHarness enabled={false} />);
    const click = (id: string) =>
      act(() =>
        mounted.container
          .querySelector<HTMLButtonElement>(`[data-testid="${id}"]`)!
          .dispatchEvent(new MouseEvent('click', { bubbles: true })),
      );
    expect(shownSize(mounted.container)).toBe('232');

    click('commit-350'); // 禁用提交：只保留内存，不访问存储
    expect(writeCount).toBe(0);
    expect(store.has(PX_KEY)).toBe(false);
    expect(shownSize(mounted.container)).toBe('232'); // 对外返回默认值

    rerender(mounted.root, <HookHarness enabled={true} />);
    expect(shownSize(mounted.container)).toBe('350'); // 内存保留，再启用恢复
    expect(getSpy).toHaveBeenCalledTimes(1); // 仅首次启用读一次

    rerender(mounted.root, <HookHarness enabled={false} />);
    click('set-360'); // setSize 更新保留内存
    expect(shownSize(mounted.container)).toBe('232');

    rerender(mounted.root, <HookHarness enabled={true} />);
    expect(shownSize(mounted.container)).toBe('360');
    expect(writeCount).toBe(0);
    mounted.unmount();
  });

  it('getItem 抛异常不崩溃，回退默认', () => {
    store.set(PX_KEY, '400');
    vi.spyOn(globalThis.localStorage, 'getItem').mockImplementation(() => {
      throw new DOMException('blocked', 'SecurityError');
    });
    const { container, unmount } = mount(pxHarness());
    expect(shownSize(container)).toBe('232');
    expect(writeCount).toBe(0);
    unmount();
  });

  it('写失败（QuotaExceeded）捕获，保留内存布局', () => {
    const mounted = mount(pxHarness());
    vi.spyOn(globalThis.localStorage, 'setItem').mockImplementation(() => {
      throw new DOMException('quota exceeded', 'QuotaExceededError');
    });
    keyDown(handleOf(mounted.container), 'ArrowRight');
    expect(shownSize(mounted.container)).toBe('242');
    expect(store.has(PX_KEY)).toBe(false);
    mounted.unmount();
  });
});
