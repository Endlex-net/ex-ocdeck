import { describe, expect, it } from 'vitest';
import {
  resolveGestureRoute,
  buildWheelInit,
  buildMouseInit,
  isTap,
  isVerticalDominant,
  TAP_DISTANCE_THRESHOLD,
  TAP_TIME_THRESHOLD,
} from '../terminal/touch-gestures';
import { shouldSendInput, encodeBinaryInput } from '../terminal/input-gate';

describe('resolveGestureRoute', () => {
  it('锁定态无条件 takeover（不论 buffer/mouse mode）', () => {
    expect(resolveGestureRoute(true, 'none', 'normal')).toBe('takeover');
    expect(resolveGestureRoute(true, 'none', 'alternate')).toBe('takeover');
    expect(resolveGestureRoute(true, 'any', 'normal')).toBe('takeover');
  });

  it('解锁态 normal buffer + mouseTracking none → passthrough', () => {
    expect(resolveGestureRoute(false, 'none', 'normal')).toBe('passthrough');
  });

  it('解锁态 alternate buffer → takeover（无 scrollback 可滚）', () => {
    expect(resolveGestureRoute(false, 'none', 'alternate')).toBe('takeover');
    expect(resolveGestureRoute(false, 'any', 'alternate')).toBe('takeover');
  });

  it('解锁态 normal buffer + mouse reporting active → takeover', () => {
    expect(resolveGestureRoute(false, 'vt200', 'normal')).toBe('takeover');
    expect(resolveGestureRoute(false, 'drag', 'normal')).toBe('takeover');
    expect(resolveGestureRoute(false, 'any', 'normal')).toBe('takeover');
  });
});

describe('isVerticalDominant（严格 |Δy| > |Δx|）', () => {
  it('垂直位移大 → true', () => {
    expect(isVerticalDominant(2, 10)).toBe(true);
    expect(isVerticalDominant(-2, -10)).toBe(true);
  });
  it('水平位移大 → false', () => {
    expect(isVerticalDominant(10, 2)).toBe(false);
  });
  it('相等位移 → false（不算垂直）', () => {
    expect(isVerticalDominant(5, 5)).toBe(false);
    expect(isVerticalDominant(-5, 5)).toBe(false);
  });
  it('均为 0 → false', () => {
    expect(isVerticalDominant(0, 0)).toBe(false);
  });
});

describe('buildWheelInit（逐帧 delta）', () => {
  it('deltaY = previousClientY - currentClientY（手指上滑 → 正值）', () => {
    const init = buildWheelInit(100, 200, 250);
    expect(init.deltaY).toBe(50);
    expect(init.deltaMode).toBe(0);
    expect(init.bubbles).toBe(true);
    expect(init.cancelable).toBe(true);
    expect(init.clientX).toBe(100);
    expect(init.clientY).toBe(200);
  });
  it('手指下滑 → 负值', () => {
    expect(buildWheelInit(0, 300, 200).deltaY).toBe(-100);
  });
});

describe('buildMouseInit', () => {
  it('mousedown: buttons=1', () => {
    const init = buildMouseInit(50, 60, 'down');
    expect(init.button).toBe(0);
    expect(init.buttons).toBe(1);
    expect(init.bubbles).toBe(true);
    expect(init.clientX).toBe(50);
    expect(init.clientY).toBe(60);
  });
  it('mouseup: buttons=0 且 bubbles:true（xterm mouseup 监听在 document）', () => {
    const init = buildMouseInit(50, 60, 'up');
    expect(init.buttons).toBe(0);
    expect(init.bubbles).toBe(true);
  });
});

describe('isTap（阈值判定）', () => {
  it('位移 <阈值 且 时长 <阈值 → tap', () => {
    expect(isTap(3, 4, 100)).toBe(true); // hypot=5 < 8, 100 < 300
  });
  it('位移超阈值 → 非 tap', () => {
    expect(isTap(10, 0, 100)).toBe(false); // hypot=10 >= 8
  });
  it('时长超阈值 → 非 tap', () => {
    expect(isTap(0, 0, TAP_TIME_THRESHOLD)).toBe(false);
  });
  it('位移恰好等于阈值 → 非 tap（严格小于）', () => {
    expect(isTap(TAP_DISTANCE_THRESHOLD, 0, 100)).toBe(false);
  });
});

describe('shouldSendInput（统一门禁）', () => {
  it('未认证 → 拒绝', () => {
    expect(
      shouldSendInput({ authed: false, wsOpen: true, locked: false, syntheticInFlight: false }),
    ).toBe(false);
  });
  it('WS 未 OPEN → 拒绝', () => {
    expect(
      shouldSendInput({ authed: true, wsOpen: false, locked: false, syntheticInFlight: false }),
    ).toBe(false);
  });
  it('锁定 + 非合成 → 拒绝', () => {
    expect(
      shouldSendInput({ authed: true, wsOpen: true, locked: true, syntheticInFlight: false }),
    ).toBe(false);
  });
  it('锁定 + 合成事件中 → 放行', () => {
    expect(
      shouldSendInput({ authed: true, wsOpen: true, locked: true, syntheticInFlight: true }),
    ).toBe(true);
  });
  it('解锁 + 已认证 + WS OPEN → 放行', () => {
    expect(
      shouldSendInput({ authed: true, wsOpen: true, locked: false, syntheticInFlight: false }),
    ).toBe(true);
  });
});

describe('encodeBinaryInput（charCode & 0xFF，MUST NOT TextEncoder）', () => {
  it('ASCII 逐字节保留', () => {
    const bytes = encodeBinaryInput('ABC');
    expect(Array.from(bytes)).toEqual([65, 66, 67]);
  });
  it('>127 字节按 charCode 取低 8 位，不被 UTF-8 破坏', () => {
    // 模拟鼠标控制序列字节（值 >127 的 code unit）。
    const bytes = encodeBinaryInput(String.fromCharCode(0xe1, 0x20, 0xff));
    expect(Array.from(bytes)).toEqual([0xe1, 0x20, 0xff]);
  });
});