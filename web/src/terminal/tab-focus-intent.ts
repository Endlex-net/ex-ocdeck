import { FOCUS_REQUEST_TTL_MS, isInputElementTarget } from './focus-request';

/**
 * Tab-local 聚焦 intent（fix-ai-auto-permission-and-tab-focus design D3）：任务详情页
 * tabstrip 对「终端」/「shell N」的真实点击生成，与全局导航协议（focus-request.ts，
 * shell 消费导航请求禁令不变）完全分离。intent 由 TaskWorkbenchPage 持有（新覆盖旧）
 * 并作为 prop 传给各 TerminalView；TerminalView 仅当 target 匹配自身标识且
 * active && connected 时消费（门禁内核与导航请求同构：未锁定 + 未过期 + 白名单）。
 *
 * - target 闭合定义：'tui'（终端 tab）或 shell tab 稳定 id——与 tabstrip
 *   active={tab===id} 判定同一键，非显示序号、非数组索引；
 * - seq 单调递增：重复点击已激活终端 tab 生成新 seq，必须仍聚焦；
 * - 取消：切走（当前 tab ≠ target，页面层清除）、卸载、过期、用户焦点已进入输入区。
 */

/** 「终端」tab 的 intent target 键（TaskWorkbenchPage 的 TUI_TAB 与此同源）。 */
export const TAB_FOCUS_TARGET_TUI = 'tui';

export interface TabFocusIntent {
  /** 单调递增；新 intent 覆盖旧 intent。 */
  seq: number;
  /** 生成时刻（ms，Date.now），过期判定沿用导航请求 TTL。 */
  ts: number;
  /** 目标键：'tui' 或 shell tab 稳定 id。 */
  target: string;
}

let nextSeq = 0;

/** tabstrip 终端类 tab 真实点击时生成新 intent（seq 单调递增，新覆盖旧）。 */
export function nextTabFocusIntent(target: string): TabFocusIntent {
  return { seq: ++nextSeq, ts: Date.now(), target };
}

/** TerminalView 自身 tab 标识（design D3 target 键）：TUI wsPath → 'tui'，shell
 * wsPath → shell tab 稳定 id；空段/外部路径返回 null，不参与 tab-local 聚焦。 */
export function tabFocusTargetFromWsPath(wsPath: string): string | null {
  const prefix = '/ws/terminal/';
  if (!wsPath.startsWith(prefix)) return null;
  const rest = wsPath.slice(prefix.length);
  if (rest.startsWith('shell/')) return rest.slice('shell/'.length) || null;
  return rest === '' ? null : TAB_FOCUS_TARGET_TUI;
}

/** 门禁②（过期，design D3「等待必须有界」）：沿用导航请求 5s TTL。 */
export function isTabFocusIntentExpired(intent: TabFocusIntent, now = Date.now()): boolean {
  return now - intent.ts >= FOCUS_REQUEST_TTL_MS;
}

/**
 * 门禁④⑤（tab-local 白名单，design D3）：仅允许 body 或 tabstrip 点击目标（真实
 * 浏览器点击 tab 后焦点落在 tab 按钮上），不放宽全局导航白名单（.od-sidebar /
 * .wb-switcher / .cc-page 任务行不放行）；输入元素排除优先——用户焦点已进入输入区
 * 即取消。宁可不聚焦不抢焦点。
 */
export function isTabFocusTargetAllowed(el: Element | null): boolean {
  if (!el || isInputElementTarget(el)) return false;
  if (el === document.body) return true;
  return el.closest('.tabstrip') !== null;
}
