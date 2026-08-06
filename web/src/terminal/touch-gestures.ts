import type { Terminal } from '@xterm/xterm';

/**
 * 触控手势层（design D4/D8）。
 *
 * 路由原则：每次 touchstart 按 xterm 公开运行时状态（`term.modes.mouseTrackingMode`
 * 与 `term.buffer.active.type`）求值，MUST NOT 按 wsPath/终端类型静态区分。
 *
 * 监听目标 vs 派发目标分离：
 * - touch 监听目标 = touch 接收目标。LOCKED → lock.overlay（overlay 在锁定态接收手势）；
 *   UNLOCKED → .xterm 元素（capture 阶段先于 xterm 原生 target/bubble 监听）。
 * - 合成事件派发目标 **始终是 `.xterm` 元素**（`term.element`），即便锁定态手势监听挂在
 *   overlay 上。overlay 只负责接收手势，不接收合成 WheelEvent/MouseEvent——
 *   xterm wheel/mouse 监听挂在 `.xterm`，派发到 overlay 不会触发 xterm 协议路径。
 *
 * 监听阶段契约：capture 阶段注册 touch 监听。接管时 `stopImmediatePropagation()` 阻断
 * xterm 原生 touch 监听；放行路径（normal buffer + mouseTrackingMode none）不阻断。
 *
 * 合成事件契约（D4.2）：
 * - WheelEvent: clientX/Y、逐帧 deltaY = previousClientY - currentClientY、deltaMode:0、
 *   bubbles/cancelable:true（每帧无条件更新 lastY，保证逐帧 delta）
 * - MouseEvent: clientX/Y、button:0、buttons(down=1/up=0)、bubbles/cancelable:true；
 *   mouseup 必须 bubbles:true（xterm mouseup 监听挂在 document）
 * - 自定义 tap 对原 touch 序列 preventDefault 抑制 Safari compatibility mouse（防双确认）
 * - changedTouches + touch.identifier 追踪单指；touchcancel 复位
 *
 * 手势状态冻结（D4.1）：tracker 在 touchstart 冻结 {locked, route, mouseActive}，
 * 整段手势序列（move/end）用冻结值判定，避免手势中途运行时状态变化产生意外行为。
 *
 * 多指序列阻断：多指导致 tracker 清空后，该 touch 序列剩余事件（后续多指 move/end）
 * 仍须 stopImmediatePropagation 阻断 xterm 原生监听直到序列结束，不能落回原生。
 */

/** 锁定状态 + 合成事件同步标记，由 TermSession 提供。 */
export interface GestureContext {
  /** 当前是否锁定（决定 touch 监听目标）。 */
  isLocked(): boolean;
  /** 合成事件同步包裹：回调执行期间 syntheticEventInFlight=true，放行门禁。 */
  markSynthetic<T>(fn: () => T): T;
}

export interface AttachOptions {
  term: Terminal;
  /** 返回 touch 监听目标元素：LOCKED=lock.overlay / UNLOCKED=xterm 元素。锁状态变化时调用 rebind()。 */
  getTarget(): HTMLElement;
  ctx: GestureContext;
  /** DOM 事件构造器注入（默认全局；Vitest Node 无 DOM 时注入 mock）。 */
  wheelCtor?: typeof WheelEvent;
  mouseCtor?: typeof MouseEvent;
}

export interface GestureHandle {
  /** 锁状态变化时重新挂载监听到新目标。 */
  rebind(): void;
  dispose(): void;
}

// ---------- 阈值（纯常量，可单测引用） ----------

/** tap 判定：位移阈值。 */
export const TAP_DISTANCE_THRESHOLD = 8;
/** tap 判定：时长阈值。 */
export const TAP_TIME_THRESHOLD = 300;

// ---------- 纯逻辑：路由判定（可单测，不依赖 DOM） ----------

/** 手势路由结果。 */
export type GestureRoute = 'passthrough' | 'takeover';

/**
 * 按 D4.1 路由表判定。
 * - 锁定态（isLocked=true）→ 无条件 takeover（overlay 已遮挡终端，xterm 收不到事件，
 *   但锁定手势经 overlay 接管，合成 wheel 派发到 .xterm 实现锁定态滚动查看）。
 * - 解锁态：normal buffer + mouseTrackingMode none → 放行 xterm 原生 touch；
 *   否则（alt buffer 或 mouse reporting active）手势层接管。
 */
export function resolveGestureRoute(
  isLocked: boolean,
  mouseTrackingMode: 'none' | 'x10' | 'vt200' | 'drag' | 'any',
  bufferType: 'normal' | 'alternate',
): GestureRoute {
  if (isLocked) return 'takeover';
  if (bufferType === 'normal' && mouseTrackingMode === 'none') {
    return 'passthrough';
  }
  return 'takeover';
}

// ---------- 纯逻辑：合成事件参数构造（可单测） ----------

export interface WheelInit {
  clientX: number;
  clientY: number;
  deltaY: number;
  deltaMode: 0;
  bubbles: true;
  cancelable: true;
}

export interface MouseInit {
  clientX: number;
  clientY: number;
  button: 0;
  buttons: number; // down=1, up=0
  bubbles: true;
  cancelable: true;
}

/** 构造 WheelEvent init。deltaY = previousClientY - currentClientY（手指上滑 → 正值）。 */
export function buildWheelInit(
  currentClientX: number,
  currentClientY: number,
  previousClientY: number,
): WheelInit {
  return {
    clientX: currentClientX,
    clientY: currentClientY,
    deltaY: previousClientY - currentClientY,
    deltaMode: 0,
    bubbles: true,
    cancelable: true,
  };
}

/** 构造 mousedown/mouseup init。down 时 buttons=1，up 时 buttons=0。 */
export function buildMouseInit(
  clientX: number,
  clientY: number,
  phase: 'down' | 'up',
): MouseInit {
  return {
    clientX,
    clientY,
    button: 0,
    buttons: phase === 'down' ? 1 : 0,
    bubbles: true,
    cancelable: true,
  };
}

/** tap 判定纯函数：位移 <阈值 且 时长 <阈值 → tap。 */
export function isTap(dx: number, dy: number, dtMs: number): boolean {
  return Math.hypot(dx, dy) < TAP_DISTANCE_THRESHOLD && dtMs < TAP_TIME_THRESHOLD;
}

/** 垂直占优判定纯函数：严格 |Δy| > |Δx|。相等不算垂直。 */
export function isVerticalDominant(dx: number, dy: number): boolean {
  return Math.abs(dy) > Math.abs(dx);
}

// ---------- 手势状态（touchstart 冻结） ----------

interface TouchTracker {
  identifier: number;
  startX: number;
  startY: number;
  startT: number;
  lastY: number;
  /** 冻结的路由：整段手势序列使用，避免中途运行时状态变化产生意外行为。 */
  route: GestureRoute;
  /** 冻结的锁定态：锁定态 tap 永远 no-op（锁定时不合成点击）。 */
  locked: boolean;
  /** 冻结的 mouse reporting 是否 active：tap 合成点击的判定依据。 */
  mouseActive: boolean;
  /** 粘性 tap/拖拽互斥状态（design D4.2 阈值 8px）：
   *  - tapEligible：累计位移 <8px 阈值前为 true，此期间 MUST NOT 派发 wheel；
   *    一旦越过阈值置 false（进入 dragging），后续即使回到起点也 MUST NOT 合成点击。
   *  - maxDistance：累计最大位移（touchmove 持续更新），用于判定何时越过阈值。
   *  - dragging：越过阈值后置 true，touchend 时据此拒绝 tap。 */
  tapEligible: boolean;
  maxDistance: number;
  dragging: boolean;
}

export function attachTouchGestures(opts: AttachOptions): GestureHandle {
  const WheelCtor = opts.wheelCtor ?? WheelEvent;
  const MouseCtor = opts.mouseCtor ?? MouseEvent;

  let target: HTMLElement | null = null;
  let tracker: TouchTracker | null = null;
  /** 多指序列阻断标记：多指出现后置 true，序列结束（touches.length===0）复位。
   *  期间所有 touch 事件继续 stopImmediatePropagation 阻断 xterm 原生监听。 */
  let suppressNativeUntilEnd = false;

  /** 取合成事件派发目标：始终是 term.element（.xterm）。 */
  function dispatchTarget(): HTMLElement | null {
    return opts.term.element ?? null;
  }

  function onTouchStart(ev: TouchEvent): void {
    if (ev.touches.length !== 1) {
      // 多指：清空 tracker，但继续阻断原生监听直到序列结束。
      tracker = null;
      suppressNativeUntilEnd = true;
      ev.stopImmediatePropagation();
      ev.preventDefault();
      return;
    }
    const t = ev.changedTouches[0];
    if (!t) return;

    const mouseTrackingMode = opts.term.modes.mouseTrackingMode;
    const bufferType = opts.term.buffer.active.type;
    const locked = opts.ctx.isLocked();
    const route = resolveGestureRoute(locked, mouseTrackingMode, bufferType);

    if (route === 'passthrough') {
      // 放行 xterm 原生 touch 监听（target/bubble 阶段），不阻断。
      tracker = null;
      suppressNativeUntilEnd = false;
      return;
    }

    // 接管：阻断 xterm 原生 touch 监听。
    ev.stopImmediatePropagation();
    // 抑制 Safari compatibility mouse（防 tap 双确认）。
    ev.preventDefault();

    tracker = {
      identifier: t.identifier,
      startX: t.clientX,
      startY: t.clientY,
      startT: ev.timeStamp,
      lastY: t.clientY,
      route,
      locked,
      mouseActive: mouseTrackingMode !== 'none',
      tapEligible: true,
      maxDistance: 0,
      dragging: false,
    };
    suppressNativeUntilEnd = false;
  }

  function findTouch(list: TouchList, identifier: number): Touch | undefined {
    for (let i = 0; i < list.length; i++) {
      const t = list[i];
      if (t.identifier === identifier) return t;
    }
    return undefined;
  }

  function onTouchMove(ev: TouchEvent): void {
    if (suppressNativeUntilEnd) {
      ev.stopImmediatePropagation();
      ev.preventDefault();
      return;
    }
    if (!tracker) return;
    if (ev.touches.length !== 1) {
      tracker = null;
      suppressNativeUntilEnd = true;
      ev.stopImmediatePropagation();
      ev.preventDefault();
      return;
    }
    const t = findTouch(ev.changedTouches, tracker.identifier);
    if (!t) return;

    ev.stopImmediatePropagation();
    ev.preventDefault();

    const currentY = t.clientY;
    const currentX = t.clientX;
    const dx = currentX - tracker.startX;
    const dy = currentY - tracker.startY;

    // 粘性 tap/拖拽互斥（design D4.2 阈值 8px）：累计位移达到阈值前 MUST NOT 派发 wheel。
    // 更新累计最大位移；一旦越过阈值进入 dragging，后续即使回到起点也 MUST NOT 合成点击。
    const dist = Math.hypot(dx, dy);
    if (dist > tracker.maxDistance) tracker.maxDistance = dist;
    if (tracker.tapEligible && tracker.maxDistance >= TAP_DISTANCE_THRESHOLD) {
      // 越过阈值：进入 dragging，tap 不再 eligible。
      tracker.tapEligible = false;
      tracker.dragging = true;
    }

    // 逐帧无条件更新 lastY（保证下一帧 delta 为单帧差值，不累计）。
    const wheelInit = buildWheelInit(currentX, currentY, tracker.lastY);
    // 仅 dragging（已越过阈值）且垂直占优时合成 wheel；阈值前/水平/相等时不滚（仍更新 lastY 防累计）。
    if (tracker.dragging && isVerticalDominant(dx, dy)) {
      const el = dispatchTarget();
      if (el) {
        opts.ctx.markSynthetic(() => {
          el.dispatchEvent(new WheelCtor('wheel', wheelInit));
        });
      }
    }
    tracker.lastY = currentY;
  }

  function endTracker(ev: TouchEvent): void {
    if (suppressNativeUntilEnd) {
      // 序列结束（touches.length===0）时复位阻断标记。
      if (ev.touches.length === 0) suppressNativeUntilEnd = false;
      ev.stopImmediatePropagation();
      ev.preventDefault();
      return;
    }
    if (!tracker) return;
    const t = findTouch(ev.changedTouches, tracker.identifier);
    if (!t) {
      tracker = null;
      return;
    }

    ev.stopImmediatePropagation();
    ev.preventDefault();

    const dx = t.clientX - tracker.startX;
    const dy = t.clientY - tracker.startY;
    const dt = ev.timeStamp - tracker.startT;

    // tap 判定：未进入 dragging（累计位移 <8px）且 isTap（位移 <阈值 + 时长 <阈值）。锁定态 tap 永远 no-op。
    // 一旦 dragging（阈值已越过），即使最终位移 <8px 也 MUST NOT 合成点击。
    if (!tracker.dragging && !tracker.locked && isTap(dx, dy, dt) && tracker.mouseActive) {
      const downInit = buildMouseInit(t.clientX, t.clientY, 'down');
      const upInit = buildMouseInit(t.clientX, t.clientY, 'up');
      const el = dispatchTarget();
      if (el) {
        opts.ctx.markSynthetic(() => {
          el.dispatchEvent(new MouseCtor('mousedown', downInit));
          // mouseup 必须 bubbles:true（xterm mouseup 监听挂在 document）。
          el.dispatchEvent(new MouseCtor('mouseup', upInit));
        });
      }
    }
    tracker = null;
  }

  function onTouchEnd(ev: TouchEvent): void {
    endTracker(ev);
  }

  function onTouchCancel(_: TouchEvent): void {
    tracker = null;
    suppressNativeUntilEnd = false;
  }

  function bind(el: HTMLElement): void {
    el.addEventListener('touchstart', onTouchStart, { capture: true, passive: false });
    el.addEventListener('touchmove', onTouchMove, { capture: true, passive: false });
    el.addEventListener('touchend', onTouchEnd, { capture: true, passive: false });
    el.addEventListener('touchcancel', onTouchCancel, { capture: true, passive: false });
  }

  function unbind(el: HTMLElement): void {
    el.removeEventListener('touchstart', onTouchStart, { capture: true });
    el.removeEventListener('touchmove', onTouchMove, { capture: true });
    el.removeEventListener('touchend', onTouchEnd, { capture: true });
    el.removeEventListener('touchcancel', onTouchCancel, { capture: true });
  }

  function rebind(): void {
    if (target) unbind(target);
    tracker = null;
    suppressNativeUntilEnd = false;
    target = opts.getTarget();
    bind(target);
  }

  // 初始挂载。
  rebind();

  return {
    rebind,
    dispose() {
      if (target) unbind(target);
      target = null;
      tracker = null;
      suppressNativeUntilEnd = false;
    },
  };
}
