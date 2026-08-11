/**
 * 命令面板 → 目标页「展开折叠面板并聚焦」信号。
 * 对齐 design 源 docs/design/assets/ocdeck-palette.js 的 `od:palette-focus` 事件。
 *
 * 跨路由时 navigate 后目标页可能尚未挂载 listener，故同时写入 pendingId；
 * 目标页 mount 时 consumePendingPaletteFocus 兜底消费。
 */

export const PALETTE_FOCUS_EVENT = 'od:palette-focus';

export type PaletteFocusId = 'new-task-name' | 'register-project-name';

let pendingId: string | null = null;

/** 派发 focus 意图（事件 + pending 兜底）。
 *  document 缺失时（Node 测试）仅写 pending，不抛。 */
export function emitPaletteFocus(id: PaletteFocusId): void {
  pendingId = id;
  if (typeof document === 'undefined') return;
  document.dispatchEvent(new CustomEvent(PALETTE_FOCUS_EVENT, { detail: { id } }));
}

/** 页面 mount / 监听时消费在途 focus；匹配则清 pending 并返回 true。 */
export function consumePendingPaletteFocus(expected: PaletteFocusId): boolean {
  if (pendingId !== expected) return false;
  pendingId = null;
  return true;
}

/** 事件已被实时 listener 处理后清 pending，避免 mount 再消费一次。 */
export function clearPendingPaletteFocus(id?: PaletteFocusId): void {
  if (id === undefined || pendingId === id) pendingId = null;
}

/** 仅供测试：重置 pending 状态。 */
export function __resetPaletteFocusForTest(): void {
  pendingId = null;
}
