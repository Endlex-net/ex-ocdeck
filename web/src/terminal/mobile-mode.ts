/**
 * 移动端模式判定（mobile-terminal-mode-settings design.md D2）：三态模式 + 子开关 → 实际生效能力。
 * 纯函数模块，不 import xterm/DOM（对齐 session-coordination 的测试缝风格）。
 */

export type MobileMode = 'auto' | 'on' | 'off';

export interface MobileCaps {
  version: 1;
  lock: boolean;
  gestures: boolean;
  keyboardAvoid: boolean;
}

export interface EffectiveCaps {
  lock: boolean;
  gestures: boolean;
  keyboardAvoid: boolean;
}

export const DEFAULT_CAPS: MobileCaps = { version: 1, lock: true, gestures: true, keyboardAvoid: true };

/**
 * 启用判定唯一入口。调用方约定：caps 仅在 mode === 'on' 时来自存储读取；
 * auto/off 传 DEFAULT_CAPS 占位（本函数不读它），调用方负责不发起读取。
 */
export function resolveMobileCaps(mode: MobileMode, caps: MobileCaps, coarse: boolean): EffectiveCaps {
  if (mode === 'off') return { lock: false, gestures: false, keyboardAvoid: false };
  if (mode === 'auto') {
    // 锁定/手势沿用 coarse 自适配；避让保持全平台既有现状（与 pointer 无关）
    return { lock: coarse, gestures: coarse, keyboardAvoid: true };
  }
  // on：子开关生效；锁定开 → 手势强制开（避免只能看不能滚）
  return { lock: caps.lock, gestures: caps.lock || caps.gestures, keyboardAvoid: caps.keyboardAvoid };
}
