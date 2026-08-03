/**
 * 终端外观偏好（design.md D2）：localStorage 持久化 + 校验。
 * 默认字体栈追加 CJK 回退，使中文在常见浏览器环境正常渲染。
 * 读取侧遇到损坏/非法数据只回退默认，MUST NOT 改写 localStorage。
 */

export interface TermPreferences {
  fontFamily?: string; // trim 后非空才存在
  fontSize?: number; // 整数，8–32
}

export const DEFAULT_FONT_FAMILY =
  '"JetBrains Mono", "SF Mono", ui-monospace, Menlo, Consolas, "PingFang SC", "Sarasa Mono SC", "Noto Sans Mono CJK SC", "Microsoft YaHei", monospace';
export const DEFAULT_FONT_SIZE = 13;

export const FONT_FAMILY_KEY = 'ocdeck.terminal.fontFamily';
export const FONT_SIZE_KEY = 'ocdeck.terminal.fontSize';
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

/** 清除两个偏好 key。 */
export function clearTermPrefs(): void {
  localStorage.removeItem(FONT_FAMILY_KEY);
  localStorage.removeItem(FONT_SIZE_KEY);
}