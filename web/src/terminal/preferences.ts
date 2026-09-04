/**
 * 终端外观偏好（design.md D2）与移动端模式偏好（mobile-terminal-mode-settings design.md D1）：
 * localStorage 持久化 + 校验。
 * 默认字体栈追加 CJK 回退，使中文在常见浏览器环境正常渲染。
 * 读取侧遇到损坏/非法数据只回退默认，MUST NOT 改写 localStorage。
 * 写入侧 setItem 全成功才派发 TERM_PREFS_CHANGED，失败向上抛出、不派发。
 */

import { DEFAULT_CAPS, type MobileCaps, type MobileMode } from './mobile-mode';

export interface TermPreferences {
  fontFamily?: string; // trim 后非空才存在
  fontSize?: number; // 整数，8–32
}

export const DEFAULT_FONT_FAMILY =
  '"JetBrains Mono", "SF Mono", ui-monospace, Menlo, Consolas, "PingFang SC", "Sarasa Mono SC", "Noto Sans Mono CJK SC", "Microsoft YaHei", monospace';
export const DEFAULT_FONT_SIZE = 13;

export const FONT_FAMILY_KEY = 'ocdeck.terminal.fontFamily';
export const FONT_SIZE_KEY = 'ocdeck.terminal.fontSize';
export const MOBILE_MODE_KEY = 'ocdeck.terminal.mobileMode';
export const MOBILE_CAPS_KEY = 'ocdeck.terminal.mobileCaps';
export const TERM_PREFS_CHANGED = 'ocdeck-term-prefs-changed'; // window CustomEvent 名

/** 整数且 8<=v<=32，否则 null；禁止 parseInt（会接受 "13px"）。 */
export function validateFontSize(v: string): number | null {
  const n = Number(v);
  if (!Number.isInteger(n) || n < 8 || n > 32) return null;
  return n;
}

export function resolveFontFamily(p: TermPreferences): string {
  return p.fontFamily ?? DEFAULT_FONT_FAMILY;
}

export function resolveFontSize(p: TermPreferences): number {
  return p.fontSize ?? DEFAULT_FONT_SIZE;
}

/** 读取偏好；损坏/非法只回退默认，MUST NOT 改写 localStorage。 */
export function loadTermPrefs(): TermPreferences {
  const prefs: TermPreferences = {};
  try {
    const ff = localStorage.getItem(FONT_FAMILY_KEY);
    if (ff !== null && ff.trim() !== '') prefs.fontFamily = ff;
  } catch {
    /* localStorage 不可用时按无偏好处理 */
  }
  try {
    const fs = localStorage.getItem(FONT_SIZE_KEY);
    if (fs !== null) {
      const n = Number(fs);
      if (Number.isInteger(n) && n >= 8 && n <= 32) prefs.fontSize = n;
    }
  } catch {
    /* ignore */
  }
  return prefs;
}

/**
 * 保存偏好：两字段先完整校验，全部合法才写 localStorage。
 * fontFamily trim 后为空 → 删除该 key（允许单独保存合法 fontSize）。
 * 存储异常向上抛出、不派发事件。
 */
export function saveTermPrefs(p: TermPreferences): void {
  // 先完整校验 fontSize（若提供）；非法则整体不写入。
  let fontSize: number | undefined;
  if (p.fontSize !== undefined) {
    const v = validateFontSize(String(p.fontSize));
    if (v === null) throw new Error('字号必须为 8–32 之间的整数');
    fontSize = v;
  }

  // 写入 fontFamily：trim 后为空视为未设置（删除 key 回默认栈）。
  const fontFamilyTrim = p.fontFamily?.trim();
  if (fontFamilyTrim && fontFamilyTrim !== '') {
    localStorage.setItem(FONT_FAMILY_KEY, fontFamilyTrim);
  } else {
    localStorage.removeItem(FONT_FAMILY_KEY);
  }
  // 写入 fontSize：未提供则删除 key（该项回默认）。
  if (fontSize !== undefined) {
    localStorage.setItem(FONT_SIZE_KEY, String(fontSize));
  } else {
    localStorage.removeItem(FONT_SIZE_KEY);
  }
}

/** 清除全部终端偏好 key（多 key 删除无法原子化）。 */
export function clearTermPrefs(): { failedKeys: string[] } {
  const failedKeys: string[] = [];
  let removedCount = 0;
  for (const key of [FONT_FAMILY_KEY, FONT_SIZE_KEY, MOBILE_MODE_KEY, MOBILE_CAPS_KEY]) {
    try {
      localStorage.removeItem(key);
      removedCount++;
    } catch {
      failedKeys.push(key);
    }
  }
  // 至少一项删除成功才派发，恰好一次；全部失败不派发。
  if (removedCount > 0) window.dispatchEvent(new CustomEvent(TERM_PREFS_CHANGED));
  return { failedKeys };
}

/**
 * 读取移动端模式（只读 mode key；「auto/off 不得读 caps key」是调用方约束）。
 * 非法值/读取异常回退 auto，不改写 localStorage。
 */
export function loadMobileMode(): MobileMode {
  try {
    const raw = localStorage.getItem(MOBILE_MODE_KEY);
    if (raw === 'auto' || raw === 'on' || raw === 'off') return raw;
  } catch {
    /* localStorage 不可用时按默认处理 */
  }
  return 'auto';
}

/**
 * 读取子开关记录（只读 caps key）。
 * JSON 损坏/缺字段/字段类型错误/version 未知/读取异常 → 整项回默认，不改写 localStorage。
 */
export function loadMobileCaps(): MobileCaps {
  try {
    const raw = localStorage.getItem(MOBILE_CAPS_KEY);
    if (raw !== null) {
      const parsed: unknown = JSON.parse(raw);
      if (
        typeof parsed === 'object' &&
        parsed !== null &&
        (parsed as { version?: unknown }).version === 1 &&
        typeof (parsed as { lock?: unknown }).lock === 'boolean' &&
        typeof (parsed as { gestures?: unknown }).gestures === 'boolean' &&
        typeof (parsed as { keyboardAvoid?: unknown }).keyboardAvoid === 'boolean'
      ) {
        const caps = parsed as { lock: boolean; gestures: boolean; keyboardAvoid: boolean };
        return { version: 1, lock: caps.lock, gestures: caps.gestures, keyboardAvoid: caps.keyboardAvoid };
      }
    }
  } catch {
    /* JSON 损坏/localStorage 不可用：整项回默认 */
  }
  return DEFAULT_CAPS;
}

/** 保存移动端模式：只写 mode key（模式切换不触碰 caps）。失败向上抛出、不派发。 */
export function saveMobileMode(mode: MobileMode): void {
  localStorage.setItem(MOBILE_MODE_KEY, mode);
  window.dispatchEvent(new CustomEvent(TERM_PREFS_CHANGED));
}

/** 保存子开关：一次性写完整 caps JSON（含「锁定开 → 手势强制开」的同事务提交）。失败向上抛出、不派发。 */
export function saveMobileCaps(caps: MobileCaps): void {
  localStorage.setItem(MOBILE_CAPS_KEY, JSON.stringify(caps));
  window.dispatchEvent(new CustomEvent(TERM_PREFS_CHANGED));
}