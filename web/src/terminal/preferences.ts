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
  '"JetBrains Mono", "SF Mono", ui-monospace, Menlo, Consolas, "Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", "PingFang SC", "Sarasa Mono SC", "Noto Sans Mono CJK SC", "Microsoft YaHei", monospace, "Symbols Nerd Font Mono"';
export const DEFAULT_FONT_SIZE = 13;

/** emoji 系统字体族（design D3 E1 彩色优先；声明式引用，不下载不打包）。 */
const EMOJI_FONT_FAMILIES = ['Apple Color Emoji', 'Segoe UI Emoji', 'Noto Color Emoji'] as const;
/** Nerd Font 图标族（@font-face 见 terminal/fonts.css；unicode-range 限定图标码点）。 */
const ICON_FONT_FAMILY = 'Symbols Nerd Font Mono';

/** CSS 空白集合（空格 / \t / \n / \r / \f）。NBSP（U+00A0）等不属于 CSS 空白，
 *  MUST NOT 被当作转义终止空白消费（`\53` + NBSP + … 是不同的族名）。 */
function isCssWhitespace(ch: string): boolean {
  return ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r' || ch === '\f';
}

interface EscapeScan {
  /** 转义序列结束索引（不含）。 */
  end: number;
  /** hex 形式的解析码点；简单转义（\X）为 null。 */
  code: number | null;
}

/** 扫描 input 中 start（必为 '\\'）起始的转义序列：hex 形式含 1–6 位 hex 与可选终止空白
 *  （单个 CSS 空白，CRLF 整体）；简单形式 2 字符；尾随孤立反斜杠 1 字符。 */
function scanEscape(input: string, start: number): EscapeScan {
  const next = input[start + 1];
  if (next === undefined) return { end: start + 1, code: null };
  if (/[0-9A-Fa-f]/.test(next)) {
    let j = start + 1;
    let digits = '';
    while (j < input.length && digits.length < 6 && /[0-9A-Fa-f]/.test(input[j])) {
      digits += input[j];
      j++;
    }
    if (j < input.length && isCssWhitespace(input[j])) {
      j++;
      if (input[j - 1] === '\r' && input[j] === '\n') j++; // CRLF 终止符整体消费
    }
    return { end: j, code: parseInt(digits, 16) };
  }
  return { end: start + 2, code: null };
}

/**
 * CSS 转义解码（输入已经 CSS 预处理语义对待：CRLF 规范化为一个换行）。
 * ① 十六进制转义 `\` + 1–6 位 hex + 可选终止空白（单个 CSS 空白被消费；CRLF 整体消费）；
 *    解析值校验：零码点、代理项（D800–DFFF）、超 U+10FFFF 统一替换 U+FFFD（不抛 RangeError）；
 * ② 引号字符串内（inString=true）转义续行：`\` + 换行（CRLF/CR/LF/FF）→ 两者皆删除；
 * ③ 其余 `\X` → X；尾随孤立反斜杠丢弃。
 */
function decodeCssEscapes(input: string, inString: boolean): string {
  let out = '';
  let i = 0;
  while (i < input.length) {
    const ch = input[i];
    if (ch !== '\\') {
      out += ch;
      i++;
      continue;
    }
    const next = input[i + 1];
    if (next === undefined) break;
    // 字符串转义续行：\ + 换行（CRLF/CR/LF/FF）→ 整体删除
    if (inString && (next === '\n' || next === '\r' || next === '\f')) {
      i++;
      if (next === '\r' && input[i + 1] === '\n') i++; // CRLF 规范化为一个换行
      i++;
      continue;
    }
    const scan = scanEscape(input, i);
    out +=
      scan.code === null
        ? input.slice(i + 1, scan.end) // 简单转义：\X → X
        : scan.code === 0 || (scan.code >= 0xd800 && scan.code <= 0xdfff) || scan.code > 0x10ffff
          ? '\uFFFD' // 零码点/代理项/超范围 → U+FFFD（不抛 RangeError）
          : String.fromCodePoint(scan.code);
    i = scan.end;
  }
  return out;
}

/**
 * 未引号族名解码（P8 根因方案）：在原始文本上按 CSS 标识符规则切词——转义序列属于
 * 标识符内容（转义产生的空白原样保留，`\ ` 完整存活），未转义的 CSS 空白是语法分隔符；
 * 各标识符分别解码后以单个普通空格连接。语法分隔空白天然被丢弃，无需事后 trim/折叠。
 */
function decodeUnquotedFamily(raw: string): string {
  const identifiers: string[] = [];
  let current = '';
  let i = 0;
  while (i < raw.length) {
    const ch = raw[i];
    if (ch === '\\' && i + 1 < raw.length) {
      const scan = scanEscape(raw, i);
      current += decodeCssEscapes(raw.slice(i, scan.end), false);
      i = scan.end;
      continue;
    }
    if (isCssWhitespace(ch)) {
      if (current !== '') {
        identifiers.push(current);
        current = '';
      }
      i++;
      continue;
    }
    current += ch;
    i++;
  }
  if (current !== '') identifiers.push(current);
  return identifiers.join(' ');
}

/**
 * family 名规范化比较键（P7：两种语境空白语义不同）：
 * - 引号字符串：解码后仅小写——内容空白（首尾/连续）是字体名的一部分，MUST NOT trim/折叠
 *   （`"Symbols  Nerd Font Mono"`、`" Apple Color Emoji"` 是不同于目标族的名字）；
 * - 未加引号：标识符切词解码（decodeUnquotedFamily）——语法分隔空白丢弃、转义内容空白保留。
 * 引号形态/转义/大小写仅影响比较，非目标项的原文输出不受影响。
 */
function familyKey(raw: string): string {
  const quote = raw[0];
  const quoted = (quote === '"' || quote === "'") && raw.length >= 2 && raw.endsWith(quote);
  if (quoted) return decodeCssEscapes(raw.slice(1, -1), true).toLowerCase();
  return decodeUnquotedFamily(raw).toLowerCase();
}

/** CSS 语义 trim（转义感知，P8）：仅裁掉首尾「未转义的」CSS 空白——被转义的内容空白
 *  （如 `Fira\ ` 尾部的 `\ `、hex 转义的终止空白）属于族名内容，MUST NOT 被裁掉。 */
function trimCssWhitespace(s: string): string {
  const escaped = new Set<number>();
  for (let i = 0; i < s.length; i++) {
    if (s[i] === '\\' && i + 1 < s.length) {
      const scan = scanEscape(s, i);
      for (let k = i; k < scan.end; k++) escaped.add(k);
      i = scan.end - 1;
    }
  }
  let start = 0;
  let end = s.length;
  while (start < end && isCssWhitespace(s[start]) && !escaped.has(start)) start++;
  while (end > start && isCssWhitespace(s[end - 1]) && !escaped.has(end - 1)) end--;
  return s.slice(start, end);
}

/**
 * 按 CSS family 列表语义切分：逗号只在引号外分隔，反斜杠转义下一字符（名字含逗号的
 * 合法 family 如 `"Example, Mono"` 不会被拆散）。返回每项 CSS trim 后的原文切片，
 * 保留非目标 family 的引号形态与含义；空项（尾逗号/连续逗号）丢弃。
 */
function splitFontFamilyList(input: string): string[] {
  const slices: string[] = [];
  let start = 0;
  let quote: string | null = null;
  for (let i = 0; i < input.length; i++) {
    const ch = input[i];
    if (ch === '\\' && i + 1 < input.length) {
      i++; // 转义：下一字符（含引号/逗号）按字面处理
      continue;
    }
    if (quote !== null) {
      if (ch === quote) quote = null;
      continue;
    }
    if (ch === '"' || ch === "'") {
      quote = ch;
    } else if (ch === ',') {
      slices.push(trimCssWhitespace(input.slice(start, i)));
      start = i + 1;
    }
  }
  slices.push(trimCssWhitespace(input.slice(start)));
  return slices.filter((family) => family !== '');
}

/**
 * 有效字体栈回退变换（terminal-links-emoji-icons design D3 唯一规则，幂等）：
 * ① emoji 族（三类系统族）已存在保留原位置不动，缺失按固定顺序追加到末尾；
 * ② 图标族 `Symbols Nerd Font Mono` 移除全部已存在项（无论位置/引号形态）后，
 *    统一追加一次到绝对末尾（仅此前全部字体缺字时命中，配合 @font-face unicode-range）。
 * 非目标 family 的原文（含引号形态、转义、大小写）与相对顺序保持不变。
 * 只变换运行时有效栈，MUST NOT 写回用户偏好存储。初始化与 applyPreferences 共用本函数。
 */
function applyFontFallbacks(input: string): string {
  if (input.trim() === '') return DEFAULT_FONT_FAMILY;
  const iconKey = ICON_FONT_FAMILY.toLowerCase();
  const kept: string[] = [];
  for (const family of splitFontFamilyList(input)) {
    const key = familyKey(family);
    if (key === iconKey) continue; // ② 图标族：移除全部已存在项
    kept.push(family); // ① 其余（含已存在的 emoji 族）保留原位
  }
  const presentEmoji = new Set(kept.map((family) => familyKey(family)));
  const appended = EMOJI_FONT_FAMILIES.filter((family) => !presentEmoji.has(family.toLowerCase())).map(
    (family) => `"${family}"`,
  );
  return [...kept, ...appended, `"${ICON_FONT_FAMILY}"`].join(', ');
}

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

/** 字体族保存校验（terminal-links-emoji-icons design D3 输入域约束，P9）：
 *  解析器输入域 = 无注释的 CSS family 列表——含 CSS 注释开始/结束标记（斜杠星号、星号斜杠）
 *  的字符串拒绝保存。纯函数，可独立测试（不直接读 localStorage）。 */
export function validateFontFamily(v: string): boolean {
  return !v.includes('/*') && !v.includes('*/');
}

/** 有效字体栈（terminal-links-emoji-icons design D3）：未设置回默认栈；
 *  任意输入（含默认与用户自定义）统一经 applyFontFallbacks 幂等变换（emoji 回退 + 图标族归末尾）。 */
export function resolveFontFamily(p: TermPreferences): string {
  return applyFontFallbacks(p.fontFamily ?? DEFAULT_FONT_FAMILY);
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
  // 先完整校验 fontFamily（若提供；P9 输入域约束：拒绝 CSS 注释符，解析器输入域 = 无注释列表）；
  // 非法则整体不写入（与 fontSize 非法同等语义）。
  const fontFamilyTrim = p.fontFamily?.trim();
  const hasFontFamily = fontFamilyTrim !== undefined && fontFamilyTrim !== '';
  if (hasFontFamily && !validateFontFamily(fontFamilyTrim!)) {
    throw new Error('字体族不能包含 CSS 注释符');
  }

  // 写入 fontFamily：trim 后为空视为未设置（删除 key 回默认栈）。
  if (hasFontFamily) {
    localStorage.setItem(FONT_FAMILY_KEY, fontFamilyTrim!);
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