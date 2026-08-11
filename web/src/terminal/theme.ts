import type { ITheme } from '@xterm/xterm';

/**
 * 终端配色（OpenSpec web-ui-redesign 新增范围：终端配色跟随应用主题）。
 * 用户决策：暗色主题=深色终端、亮色主题=浅色终端，切换即时生效无需重连。
 *
 * 取值对齐 design-system.css 的 --term-* token（亮暗两组，token 为唯一事实来源）；
 * 此处常量化是因为 xterm ITheme 只接受 hex/rgb，无法直接消费 oklch/color-mix token。
 * 所有 hex 均由 token 定义经浏览器 canvas 精确换算（probed），改动 token 时需同步换算。
 */

/** 暗色终端（对齐 --term-* 暗色组：[data-theme="dark"]）。
 *  background  #090a0c ← --term-bg oklch(0.145 0.005 260)
 *  foreground  #e9e9e9 ← --term-fg color-mix(white 92%, --ink)
 *  selection   #313234 ← color-mix(white 20%, --term-bg)
 *  red/green   ← --term-red/--term-green 暗色组；brightBlack ← --term-muted
 *  blue/yellow/magenta/cyan 与青色光标沿用既有终端调色板（brand 未注册 ANSI 色相）。 */
export const XTERM_THEME_DARK: ITheme = {
  background: '#090a0c',
  foreground: '#e9e9e9',
  cursor: '#5ccfe6',
  cursorAccent: '#090a0c',
  selectionBackground: '#313234',
  black: '#1c2430',
  red: '#fd7468',
  green: '#4ed589',
  yellow: '#e5c07b',
  blue: '#61afef',
  magenta: '#c678dd',
  cyan: '#5ccfe6',
  white: '#e9e9e9',
  brightBlack: '#898989',
  brightRed: '#fd7468',
  brightGreen: '#4ed589',
  brightYellow: '#e5c07b',
  brightBlue: '#61afef',
  brightMagenta: '#c678dd',
  brightCyan: '#5ccfe6',
  brightWhite: '#ffffff',
};

/** 亮色终端（对齐 --term-* 亮色组：:root）。
 *  background  #f4f6f8 ← --term-bg oklch(0.972 0.004 260)
 *  foreground  #202020 ← --term-fg color-mix(--ink 92%, white)
 *  selection   #d9d9d9 ← --term-border color-mix(--ink 14%, white)
 *  red/green/yellow ← --term-red/--term-green 亮色组 + --warn；blue/cursor ← 品牌 --accent
 *  magenta/cyan 为 oklch 派生（0.55 0.18 305 / 0.60 0.11 215），亮底下对比度可读；
 *  bright* 与普通色同值（亮底下提亮度会降低对比度），brightBlack ← --term-muted。 */
export const XTERM_THEME_LIGHT: ITheme = {
  background: '#f4f6f8',
  foreground: '#202020',
  cursor: '#1677ff',
  cursorAccent: '#f4f6f8',
  selectionBackground: '#d9d9d9',
  black: '#202020',
  red: '#cc3430',
  green: '#0fa05c',
  yellow: '#ce871b',
  blue: '#1677ff',
  magenta: '#8b4ec4',
  cyan: '#0090a9',
  white: '#f4f6f8',
  brightBlack: '#717171',
  brightRed: '#cc3430',
  brightGreen: '#0fa05c',
  brightYellow: '#ce871b',
  brightBlue: '#1677ff',
  brightMagenta: '#8b4ec4',
  brightCyan: '#0090a9',
  brightWhite: '#ffffff',
};

/** 有效主题 → xterm 配色（纯函数，可测）。 */
export function resolveXtermTheme(theme: 'light' | 'dark'): ITheme {
  return theme === 'dark' ? XTERM_THEME_DARK : XTERM_THEME_LIGHT;
}

/** 读当前有效主题（<html data-theme>，与 useTheme/index.html 内联脚本同一 channel）。
 *  无 DOM 环境（node 测试/SSR）缺省 light——防御性降级，不抛错。 */
export function readCurrentTermTheme(): 'light' | 'dark' {
  if (typeof document === 'undefined' || !document.documentElement) return 'light';
  return document.documentElement.getAttribute('data-theme') === 'dark' ? 'dark' : 'light';
}

/** 监听应用主题切换（data-theme 属性变化），回调有效主题；返回退订函数。
 *  MutationObserver 与 React 解耦：useTheme 多消费者幂等写同一属性，仅值变化时触发。
 *  无 DOM/MutationObserver 环境降级为 no-op 退订（浏览器环境恒可用）。 */
export function watchTermTheme(cb: (theme: 'light' | 'dark') => void): () => void {
  if (typeof MutationObserver === 'undefined' || typeof document === 'undefined' || !document.documentElement) {
    return () => {};
  }
  const observer = new MutationObserver(() => cb(readCurrentTermTheme()));
  observer.observe(document.documentElement, {
    attributes: true,
    attributeFilter: ['data-theme'],
  });
  return () => observer.disconnect();
}
