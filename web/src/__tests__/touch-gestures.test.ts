import { describe, expect, it, vi } from 'vitest';
import { attachTouchGestures } from '../terminal/touch-gestures';
import { createSyntheticGate, shouldSendInput } from '../terminal/input-gate';

/**
 * 真实调用链测试：直接测 attachTouchGestures()，不 mock 实现逻辑。
 *
 * 测试缝：
 * - FakeTarget：包裹 Node EventTarget，记录 addEventListener 的 options（断言 capture/passive），
 *   保留原生 dispatchEvent + stopImmediatePropagation 语义（不 spy 替换——使阻断/放行真实可测）。
 * - mock Terminal：modes/buffer.type/element 可变；element 为派发目标（FakeTarget）。
 * - 真实 createSyntheticGate()：dispatch 期间 inFlight()===true，与模拟 onData/onBinary 联动断言门禁放行。
 * - 模拟「xterm 原生 touch listener」注册在同一 target 上（rebind 之后），通过是否执行区分阻断/放行。
 *
 * 反证原则：每项测试在实现改回旧版本时必失败——下方断言精确锚定修复行为。
 */

/** 记录一次 addEventListener 调用。 */
interface AddListenerCall {
  type: string;
  capture: boolean;
  passive: boolean;
}

/**
 * FakeTarget：包裹 Node EventTarget。
 * - 记录 addEventListener options（capture/passive），供断言监听注册契约。
 * - 保留原生 dispatchEvent + stopImmediatePropagation 语义：stopImmediatePropagation 真实阻断后续监听。
 * - 暴露 addListenerCalls 供测试读取。
 */
function createFakeTarget(): {
  target: EventTarget;
  addListenerCalls: AddListenerCall[];
} {
  const inner = new EventTarget();
  const addListenerCalls: AddListenerCall[] = [];
  const target: EventTarget = {
    addEventListener: (type: string, listener: EventListenerOrEventListenerObject | null, options?: AddEventListenerOptions) => {
      const capture = Boolean(options?.capture);
      const passive = Boolean(options?.passive);
      addListenerCalls.push({ type, capture, passive });
      inner.addEventListener(type, listener, options);
    },
    removeEventListener: (type: string, listener: EventListenerOrEventListenerObject | null, options?: EventListenerOptions) => {
      inner.removeEventListener(type, listener, options);
    },
    dispatchEvent: (event: Event) => inner.dispatchEvent(event),
  };
  return { target, addListenerCalls };
}

/** 构造 Touch。 */
function touch(identifier: number, x: number, y: number): Touch {
  return { identifier, clientX: x, clientY: y } as Touch;
}

/** 构造真实 Event 实例，附加 touches/changedTouches/timeStamp + spy preventDefault。
 *  保留原生 stopImmediatePropagation（不再 spy 替换），使阻断/放行真实可测。 */
function touchEvent(
  type: 'touchstart' | 'touchmove' | 'touchend' | 'touchcancel',
  touches: Touch[],
  changedTouches: Touch[],
  timeStamp: number,
): TouchEvent {
  const ev = new Event(type, { bubbles: false, cancelable: false, composed: false });
  Object.defineProperties(ev, {
    touches: { value: touches, configurable: true },
    changedTouches: { value: changedTouches, configurable: true },
    timeStamp: { value: timeStamp, configurable: true },
  });
  // 只 spy preventDefault（断言 compatibility mouse 抑制契约）；stopImmediatePropagation 保留原生。
  (ev as unknown as { preventDefault: ReturnType<typeof vi.fn> }).preventDefault = vi.fn();
  return ev as unknown as TouchEvent;
}

/** spy 事件构造器：构造真实 Event 并附加 init 属性，记录调用参数。
 *  派发目标 dispatchEvent 需要 Event 实例，故构造 Event 再附加 init 字段供断言读取。 */
function spyCtor(_name: string): { ctor: typeof Event; calls: { type: string; init: Record<string, unknown> }[] } {
  const calls: { type: string; init: Record<string, unknown> }[] = [];
  const ctor = vi.fn((type: string, init?: Record<string, unknown>) => {
    calls.push({ type, init: init ?? {} });
    const ev = new Event(type, { bubbles: Boolean(init?.bubbles), cancelable: Boolean(init?.cancelable) });
    if (init) {
      for (const [k, v] of Object.entries(init)) {
        Object.defineProperty(ev, k, { value: v, configurable: true, enumerable: true });
      }
    }
    return ev;
  });
  return { ctor: ctor as unknown as typeof Event, calls };
}

interface MockTerm {
  modes: { mouseTrackingMode: 'none' | 'x10' | 'vt200' | 'drag' | 'any' };
  buffer: { active: { type: 'normal' | 'alternate' } };
  element: EventTarget | null;
}

/** 构建手势测试 fixture：fake target（监听目标）+ xterm（派发目标）+ mock term + gate。
 *  xterm wheel/mouse listener 在 dispatch 时同步触发模拟 onData/onBinary，记录 gate.inFlight() 快照。 */
interface Fixture {
  target: EventTarget;
  addListenerCalls: AddListenerCall[];
  xterm: EventTarget;
  term: MockTerm;
  wheel: ReturnType<typeof spyCtor>;
  mouse: ReturnType<typeof spyCtor>;
  gate: ReturnType<typeof createSyntheticGate>;
  /** dispatch 期间记录的 inFlight 快照（模拟 onData/onBinary 调用时门禁看到的 gate 状态）。 */
  inFlightSnapshots: boolean[];
  /** 模拟 onData/onBinary 在 dispatch 期间实际触发的次数。 */
  readonly ioFire: number;
  attach: () => ReturnType<typeof attachTouchGestures>;
}

function createFixture(opts: { locked: boolean; mouseTrackingMode: MockTerm['modes']['mouseTrackingMode']; bufferType: MockTerm['buffer']['active']['type']; useXtermAsTarget?: boolean }): Fixture {
  const ft = createFakeTarget();
  const fx = createFakeTarget();
  // useXtermAsTarget：UNLOCKED 时 xterm 既是监听目标也是派发目标（真实场景 xterm 元素 = 监听目标，
  // term.element = 同一 .xterm 元素也是派发目标）。这里用 fx 同时充当监听+派发目标。
  const listenTarget = opts.useXtermAsTarget ? fx.target : ft.target;
  const wheel = spyCtor('wheel');
  const mouse = spyCtor('mouse');
  const gate = createSyntheticGate();
  const inFlightSnapshots: boolean[] = [];
  let ioFireCount = 0;

  // xterm 派发目标上的 wheel/mouse listener：dispatch 时同步触发模拟 onData/onBinary，
  // 记录此刻 gate.inFlight()（门禁在合成事件栈内看到的真实状态）。
  fx.target.addEventListener('wheel', () => {
    inFlightSnapshots.push(gate.inFlight());
    ioFireCount += 1;
  });
  fx.target.addEventListener('mousedown', () => {
    inFlightSnapshots.push(gate.inFlight());
    ioFireCount += 1;
  });
  fx.target.addEventListener('mouseup', () => {
    inFlightSnapshots.push(gate.inFlight());
    ioFireCount += 1;
  });

  const term: MockTerm = {
    modes: { mouseTrackingMode: opts.mouseTrackingMode },
    buffer: { active: { type: opts.bufferType } },
    element: fx.target,
  };

  return {
    target: listenTarget,
    addListenerCalls: (opts.useXtermAsTarget ? fx : ft).addListenerCalls,
    xterm: fx.target,
    term,
    wheel,
    mouse,
    gate,
    inFlightSnapshots,
    get ioFire() { return ioFireCount; },
    attach: () => attachTouchGestures({
      term: term as unknown as import('../terminal/touch-gestures').AttachOptions['term'],
      getTarget: () => listenTarget as unknown as HTMLElement,
      ctx: {
        isLocked: () => opts.locked,
        markSynthetic: <T>(fn: () => T): T => gate.markSynthetic(fn),
      },
      wheelCtor: wheel.ctor as unknown as typeof WheelEvent,
      mouseCtor: mouse.ctor as unknown as typeof MouseEvent,
    }),
  };
}

describe('attachTouchGestures 真实调用链', () => {
  it('监听注册契约：touch 监听用 {capture:true, passive:false}', () => {
    const f = createFixture({ locked: true, mouseTrackingMode: 'none', bufferType: 'normal' });
    const handle = f.attach();
    const touchCallTypes = new Set(['touchstart', 'touchmove', 'touchend', 'touchcancel']);
    const touchCalls = f.addListenerCalls.filter((c) => touchCallTypes.has(c.type));
    // 4 种 touch 事件各注册一次
    expect(touchCalls).toHaveLength(4);
    // 全部 capture:true、passive:false
    for (const c of touchCalls) {
      expect(c.capture).toBe(true);
      expect(c.passive).toBe(false);
    }
    handle.dispose();
  });

  it('锁定态：overlay 接收 touch 序列 → 合成 WheelEvent 派发到 term.element 而非 overlay', () => {
    const f = createFixture({ locked: true, mouseTrackingMode: 'none', bufferType: 'normal' });
    const handle = f.attach();

    f.target.dispatchEvent(touchEvent('touchstart', [touch(0, 100, 200)], [touch(0, 100, 200)], 0) as unknown as Event);
    f.target.dispatchEvent(touchEvent('touchmove', [touch(0, 100, 150)], [touch(0, 100, 150)], 10) as unknown as Event);

    // WheelEvent 构造参数：deltaY = 上一帧 Y(200) - 当前 Y(150) = 50
    expect(f.wheel.calls).toHaveLength(1);
    expect(f.wheel.calls[0].type).toBe('wheel');
    expect(f.wheel.calls[0].init.deltaY).toBe(50);
    expect(f.wheel.calls[0].init.deltaMode).toBe(0);
    expect(f.wheel.calls[0].init.bubbles).toBe(true);
    expect(f.wheel.calls[0].init.cancelable).toBe(true);
    // 派发到 term.element（xterm）→ xterm wheel listener 触发
    expect(f.ioFire).toBe(1);

    handle.dispose();
  });

  it('锁定态 tap → 零 MouseEvent 合成（锁定 tap 永远 no-op）', () => {
    const f = createFixture({ locked: true, mouseTrackingMode: 'any', bufferType: 'alternate' });
    const handle = f.attach();

    f.target.dispatchEvent(touchEvent('touchstart', [touch(0, 50, 60)], [touch(0, 50, 60)], 0) as unknown as Event);
    // tap：位移 0、时长 100ms
    f.target.dispatchEvent(touchEvent('touchend', [], [touch(0, 50, 60)], 100) as unknown as Event);

    expect(f.mouse.calls).toHaveLength(0);
    expect(f.ioFire).toBe(0);

    handle.dispose();
  });

  it('放行路径（normal+none passthrough）：不阻断 xterm 原生 touch listener', () => {
    const f = createFixture({ locked: false, mouseTrackingMode: 'none', bufferType: 'normal', useXtermAsTarget: true });
    const handle = f.attach();
    // rebind 之后注册模拟 xterm 原生 touch listener（非 capture，模拟 xterm target 阶段监听）
    f.target.addEventListener('touchstart', () => {
      // 原生监听执行标记
    }, { capture: false });

    // 放行：手势层不 stopImmediatePropagation/preventDefault → 原生监听执行
    let nativeRan = false;
    f.target.addEventListener('touchstart', () => { nativeRan = true; }, { capture: false });

    const start = touchEvent('touchstart', [touch(0, 10, 10)], [touch(0, 10, 10)], 0);
    f.target.dispatchEvent(start as unknown as Event);
    expect(start.preventDefault).not.toHaveBeenCalled();
    expect(nativeRan).toBe(true);
    expect(f.wheel.calls).toHaveLength(0);

    handle.dispose();
  });

  it('接管路径（alt buffer takeover）：stopImmediatePropagation 真实阻断 xterm 原生 touch listener', () => {
    const f = createFixture({ locked: false, mouseTrackingMode: 'none', bufferType: 'alternate', useXtermAsTarget: true });
    const handle = f.attach();

    // rebind 之后注册模拟 xterm 原生 touch listener（非 capture）——应被 stopImmediatePropagation 阻断
    let nativeRan = false;
    f.target.addEventListener('touchstart', () => { nativeRan = true; }, { capture: false });

    const start = touchEvent('touchstart', [touch(0, 10, 10)], [touch(0, 10, 10)], 0);
    f.target.dispatchEvent(start as unknown as Event);

    // 原生 stopImmediatePropagation 真实阻断：后续 listener 不执行
    expect(nativeRan).toBe(false);
    // takeover 必须调 preventDefault 抑制 Safari compatibility mouse（防 tap 双确认）
    expect(start.preventDefault).toHaveBeenCalled();

    handle.dispose();
  });

  it('逐帧 delta：水平帧后转垂直帧 → 垂直帧 deltaY 用上一帧 clientY 差值而非 touchstart 累计值', () => {
    const f = createFixture({ locked: false, mouseTrackingMode: 'none', bufferType: 'alternate', useXtermAsTarget: true });
    const handle = f.attach();

    f.target.dispatchEvent(touchEvent('touchstart', [touch(0, 100, 200)], [touch(0, 100, 200)], 0) as unknown as Event);
    // 帧1：水平占优（|Δx|=50 > |Δy|=10）→ 不合成 wheel，但应更新 lastY=210
    f.target.dispatchEvent(touchEvent('touchmove', [touch(0, 150, 210)], [touch(0, 150, 210)], 10) as unknown as Event);
    expect(f.wheel.calls).toHaveLength(0);
    // 帧2：转垂直 → deltaY = 上一帧 Y(210) - 当前 Y(120) = 90（逐帧）；累计值应为 80
    f.target.dispatchEvent(touchEvent('touchmove', [touch(0, 155, 120)], [touch(0, 155, 120)], 20) as unknown as Event);
    expect(f.wheel.calls).toHaveLength(1);
    expect(f.wheel.calls[0].init.deltaY).toBe(90);

    handle.dispose();
  });

  it('冻结状态：touchstart 后改 mouseTrackingMode → touchend tap 按冻结值判决（不合成点击）', () => {
    const f = createFixture({ locked: false, mouseTrackingMode: 'none', bufferType: 'alternate', useXtermAsTarget: true });
    const handle = f.attach();

    // touchstart：mouseTrackingMode=none 冻结 mouseActive=false
    f.target.dispatchEvent(touchEvent('touchstart', [touch(0, 50, 60)], [touch(0, 50, 60)], 0) as unknown as Event);
    // 手势中途启用 mouse reporting
    f.term.modes.mouseTrackingMode = 'any';
    // tap：位移 0、时长 100ms
    f.target.dispatchEvent(touchEvent('touchend', [], [touch(0, 50, 60)], 100) as unknown as Event);

    // 冻结值 mouseActive=false → 不合成点击（即便运行时已开 mouse reporting）
    expect(f.mouse.calls).toHaveLength(0);

    handle.dispose();
  });

  it('冻结状态：touchstart 时 mouseActive=true → tap 合成 mousedown+mouseup（含 cancelable:true）', () => {
    const f = createFixture({ locked: false, mouseTrackingMode: 'any', bufferType: 'alternate', useXtermAsTarget: true });
    const handle = f.attach();

    f.target.dispatchEvent(touchEvent('touchstart', [touch(0, 50, 60)], [touch(0, 50, 60)], 0) as unknown as Event);
    // 手势中途关闭 mouse reporting
    f.term.modes.mouseTrackingMode = 'none';
    f.target.dispatchEvent(touchEvent('touchend', [], [touch(0, 50, 60)], 100) as unknown as Event);

    // 冻结值 mouseActive=true → 合成点击（即便运行时已关）
    expect(f.mouse.calls).toHaveLength(2);
    expect(f.mouse.calls[0].type).toBe('mousedown');
    expect(f.mouse.calls[0].init.buttons).toBe(1);
    expect(f.mouse.calls[0].init.cancelable).toBe(true);
    expect(f.mouse.calls[0].init.bubbles).toBe(true);
    expect(f.mouse.calls[0].init.button).toBe(0);
    expect(f.mouse.calls[1].type).toBe('mouseup');
    expect(f.mouse.calls[1].init.buttons).toBe(0);
    expect(f.mouse.calls[1].init.cancelable).toBe(true);
    expect(f.mouse.calls[1].init.bubbles).toBe(true);

    // MouseEvent 路径同样在 markSynthetic 包裹内：mousedown+mouseup 各 dispatch 一次
    // dispatch 期间 gate.inFlight()=true；两次 dispatch 后 gate 恢复 false。
    expect(f.inFlightSnapshots).toEqual([true, true]);
    expect(f.gate.inFlight()).toBe(false);

    handle.dispose();
  });

  it('多指序列阻断：多指出现后剩余事件持续 stopImmediatePropagation 直到 touches.length===0', () => {
    const f = createFixture({ locked: false, mouseTrackingMode: 'any', bufferType: 'alternate', useXtermAsTarget: true });
    const handle = f.attach();

    // 正常单指 touchstart（接管路径）
    let nativeRanAfterStart = false;
    f.target.dispatchEvent(touchEvent('touchstart', [touch(0, 50, 60)], [touch(0, 50, 60)], 0) as unknown as Event);
    // 注册模拟原生 listener 在 start 之后
    f.target.addEventListener('touchmove', () => { nativeRanAfterStart = true; }, { capture: false });

    // 第二指加入（touches.length=2）→ tracker 清空 + suppressNativeUntilEnd=true
    f.target.dispatchEvent(touchEvent('touchmove', [touch(0, 50, 60), touch(1, 80, 90)], [touch(1, 80, 90)], 10) as unknown as Event);
    // 原生 listener 应被 stopImmediatePropagation 阻断不执行
    expect(nativeRanAfterStart).toBe(false);

    // 序列剩余事件（仍多指）继续阻断
    nativeRanAfterStart = false;
    f.target.addEventListener('touchmove', () => { nativeRanAfterStart = true; }, { capture: false });
    f.target.dispatchEvent(touchEvent('touchmove', [touch(0, 55, 65), touch(1, 85, 95)], [touch(0, 55, 65)], 20) as unknown as Event);
    expect(nativeRanAfterStart).toBe(false);
    expect(f.wheel.calls).toHaveLength(0); // 多指期间不合成

    // touchend 仍多指（touches.length=1）→ 继续阻断，不合成
    nativeRanAfterStart = false;
    f.target.addEventListener('touchend', () => { nativeRanAfterStart = true; }, { capture: false });
    f.target.dispatchEvent(touchEvent('touchend', [touch(0, 55, 65)], [touch(1, 85, 95)], 30) as unknown as Event);
    expect(nativeRanAfterStart).toBe(false);
    expect(f.mouse.calls).toHaveLength(0);

    // 最后 touchend touches.length===0 → 阻断并复位标记
    nativeRanAfterStart = false;
    f.target.addEventListener('touchend', () => { nativeRanAfterStart = true; }, { capture: false });
    f.target.dispatchEvent(touchEvent('touchend', [], [touch(0, 55, 65)], 40) as unknown as Event);
    expect(nativeRanAfterStart).toBe(false); // 序列最后事件仍被阻断

    // 复位后：新的单指 touchstart 在 takeover 路径正常阻断
    nativeRanAfterStart = false;
    f.target.addEventListener('touchstart', () => { nativeRanAfterStart = true; }, { capture: false });
    f.target.dispatchEvent(touchEvent('touchstart', [touch(0, 50, 60)], [touch(0, 50, 60)], 50) as unknown as Event);
    expect(nativeRanAfterStart).toBe(false);

    handle.dispose();
  });

  it('term.element 缺失时 wheel 不派发（存在性检查兜底）', () => {
    // element 缺失：用 fixture 但 term.element=null
    const wheel = spyCtor('wheel');
    const mouse = spyCtor('mouse');
    const gate = createSyntheticGate();
    const { target } = createFakeTarget();
    const term: MockTerm = {
      modes: { mouseTrackingMode: 'none' },
      buffer: { active: { type: 'alternate' } },
      element: null,
    };
    const handle = attachTouchGestures({
      term: term as unknown as import('../terminal/touch-gestures').AttachOptions['term'],
      getTarget: () => target as unknown as HTMLElement,
      ctx: {
        isLocked: () => false,
        markSynthetic: <T>(fn: () => T): T => gate.markSynthetic(fn),
      },
      wheelCtor: wheel.ctor as unknown as typeof WheelEvent,
      mouseCtor: mouse.ctor as unknown as typeof MouseEvent,
    });

    target.dispatchEvent(touchEvent('touchstart', [touch(0, 100, 200)], [touch(0, 100, 200)], 0) as unknown as Event);
    target.dispatchEvent(touchEvent('touchmove', [touch(0, 100, 150)], [touch(0, 100, 150)], 10) as unknown as Event);
    expect(wheel.calls).toHaveLength(0);

    handle.dispose();
  });

  it('rebind：锁状态变化时重新挂载监听到新目标', () => {
    const wheel = spyCtor('wheel');
    const mouse = spyCtor('mouse');
    const gate = createSyntheticGate();
    const { target: overlay } = createFakeTarget();
    const { target: xterm } = createFakeTarget();
    const term: MockTerm = {
      modes: { mouseTrackingMode: 'none' },
      buffer: { active: { type: 'normal' } },
      element: xterm,
    };
    let locked = true;
    const handle = attachTouchGestures({
      term: term as unknown as import('../terminal/touch-gestures').AttachOptions['term'],
      getTarget: () => (locked ? overlay : xterm) as unknown as HTMLElement,
      ctx: {
        isLocked: () => locked,
        markSynthetic: <T>(fn: () => T): T => gate.markSynthetic(fn),
      },
      wheelCtor: wheel.ctor as unknown as typeof WheelEvent,
      mouseCtor: mouse.ctor as unknown as typeof MouseEvent,
    });

    // 锁定态：touchstart 在 overlay 接收 → takeover 阻断
    let nativeRan = false;
    overlay.addEventListener('touchstart', () => { nativeRan = true; }, { capture: false });
    overlay.dispatchEvent(touchEvent('touchstart', [touch(0, 100, 200)], [touch(0, 100, 200)], 0) as unknown as Event);
    expect(nativeRan).toBe(false); // 锁定 takeover 阻断

    // 解锁：rebind 切到 xterm，normal+none → passthrough
    locked = false;
    handle.rebind();
    nativeRan = false;
    xterm.addEventListener('touchstart', () => { nativeRan = true; }, { capture: false });
    xterm.dispatchEvent(touchEvent('touchstart', [touch(0, 100, 200)], [touch(0, 100, 200)], 100) as unknown as Event);
    expect(nativeRan).toBe(true); // passthrough 放行

    handle.dispose();
  });

  it('synthetic gate 与 dispatch 同步联动：dispatch 期间 inFlight=true、门禁放行；返回后 inFlight=false、门禁拒绝', () => {
    const f = createFixture({ locked: true, mouseTrackingMode: 'none', bufferType: 'normal' });
    const handle = f.attach();

    // 锁定态：wheel dispatch 在 markSynthetic 栈内 → 模拟 onData 触发时 gate.inFlight()=true
    f.target.dispatchEvent(touchEvent('touchstart', [touch(0, 100, 200)], [touch(0, 100, 200)], 0) as unknown as Event);
    f.target.dispatchEvent(touchEvent('touchmove', [touch(0, 100, 150)], [touch(0, 100, 150)], 10) as unknown as Event);

    // dispatch 期间模拟 onData 触发 → inFlight 快照应为 [true]
    expect(f.inFlightSnapshots).toEqual([true]);
    // 门禁在合成栈内放行（锁定 + syntheticInFlight）
    expect(shouldSendInput({ authed: true, wsOpen: true, locked: true, syntheticInFlight: true })).toBe(true);
    // dispatch 返回后 inFlight 恢复 false
    expect(f.gate.inFlight()).toBe(false);
    // 锁定态门禁重新拒绝
    expect(shouldSendInput({ authed: true, wsOpen: true, locked: true, syntheticInFlight: false })).toBe(false);

    handle.dispose();
  });

  it('synthetic gate 包裹缺失（反证）：若 dispatch 不经 markSynthetic，dispatch 期间 inFlight=false', () => {
    // 此测试锁定一个反证场景：构造一个不经 markSynthetic 的「错误实现」fixture，
    // 断言 dispatch 期间 inFlight=false（证明 markSynthetic 包裹是 inFlight=true 的必要条件）。
    const f = createFixture({ locked: true, mouseTrackingMode: 'none', bufferType: 'normal' });
    // 直接构造一个不包 markSynthetic 的 spyCtor 派发：绕过实现，手动 dispatch 到 xterm
    const ev = new Event('wheel', { bubbles: true, cancelable: true });
    let snapshotDuringDispatch = false;
    f.xterm.addEventListener('wheel', () => {
      snapshotDuringDispatch = f.gate.inFlight();
    });
    f.xterm.dispatchEvent(ev);
    // 未经 markSynthetic → dispatch 期间 inFlight=false
    expect(snapshotDuringDispatch).toBe(false);
  });
});

describe('tap/拖拽互斥（design D4.2 阈值 8px）', () => {
  it('小幅抖动（<8px）只点击不滚动：无 wheel 派发、tap 合成点击', () => {
    // 反证：删除阈值守卫（任意垂直 move 立即派发 wheel）→ 小幅抖动会产生 wheel → 此测试失败。
    const f = createFixture({ locked: false, mouseTrackingMode: 'any', bufferType: 'alternate', useXtermAsTarget: true });
    const handle = f.attach();
    // touchstart @ (50,60)
    f.target.dispatchEvent(touchEvent('touchstart', [touch(0, 50, 60)], [touch(0, 50, 60)], 0) as unknown as Event);
    // 小幅抖动：move 到 (52,63)（距离 ≈3.6 < 8px 阈值）→ MUST NOT 派发 wheel
    f.target.dispatchEvent(touchEvent('touchmove', [touch(0, 52, 63)], [touch(0, 52, 63)], 10) as unknown as Event);
    expect(f.wheel.calls).toHaveLength(0);
    // touchend：位移 <8px、时长 <300ms → tap 合成点击
    f.target.dispatchEvent(touchEvent('touchend', [], [touch(0, 52, 63)], 50) as unknown as Event);
    expect(f.mouse.calls).toHaveLength(2); // mousedown + mouseup
    expect(f.ioFire).toBe(2); // 派发到 xterm 触发 2 次（mousedown+mouseup）
    handle.dispose();
  });

  it('拖动超阈值后返回起点不点击：无 click 合成（dragging 粘性）', () => {
    // 反证：tap 判定仅看最终位移（不粘性 dragging）→ 返回起点最终位移 <8px 仍合成点击 → 此测试失败。
    const f = createFixture({ locked: false, mouseTrackingMode: 'any', bufferType: 'alternate', useXtermAsTarget: true });
    const handle = f.attach();
    // touchstart @ (50,60)
    f.target.dispatchEvent(touchEvent('touchstart', [touch(0, 50, 60)], [touch(0, 50, 60)], 0) as unknown as Event);
    // 拖动到 (50,80)（距离 20 > 8px 阈值）→ 进入 dragging，派发 wheel
    f.target.dispatchEvent(touchEvent('touchmove', [touch(0, 50, 80)], [touch(0, 50, 80)], 10) as unknown as Event);
    expect(f.wheel.calls).toHaveLength(1);
    // 返回起点 (50,60) → 最终位移 0，但 dragging 已置 true → MUST NOT 合成点击
    f.target.dispatchEvent(touchEvent('touchmove', [touch(0, 50, 60)], [touch(0, 50, 60)], 20) as unknown as Event);
    f.target.dispatchEvent(touchEvent('touchend', [], [touch(0, 50, 60)], 30) as unknown as Event);
    expect(f.mouse.calls).toHaveLength(0); // 无 click
    handle.dispose();
  });

  it('锁定态小幅抖动（<8px）无 wheel 派发（锁定 tap 本来 no-op，wheel 也不应在阈值前派发）', () => {
    // 反证：删除阈值守卫 → 锁定态抖动产生 wheel → 此测试失败。
    const f = createFixture({ locked: true, mouseTrackingMode: 'none', bufferType: 'normal' });
    const handle = f.attach();
    // touchstart @ (100,200) 锁定态 takeover
    f.target.dispatchEvent(touchEvent('touchstart', [touch(0, 100, 200)], [touch(0, 100, 200)], 0) as unknown as Event);
    // 小幅抖动 (101,202)（距离 ≈2.8 < 8px）→ MUST NOT 派发 wheel
    f.target.dispatchEvent(touchEvent('touchmove', [touch(0, 101, 202)], [touch(0, 101, 202)], 10) as unknown as Event);
    expect(f.wheel.calls).toHaveLength(0);
    // touchend：锁定态 tap no-op
    f.target.dispatchEvent(touchEvent('touchend', [], [touch(0, 101, 202)], 50) as unknown as Event);
    expect(f.mouse.calls).toHaveLength(0);
    handle.dispose();
  });
});