/**
 * 命令面板 → 目标页「展开折叠面板并聚焦」信号。
 * 对齐 design 源 docs/design/assets/ocdeck-palette.js 的 `od:palette-focus` 事件。
 *
 * 跨路由时 navigate 后目标页可能尚未挂载 listener，故同时写入 pending；
 * 目标页 mount 时 consumePendingPaletteFocus 兜底消费。
 * pending 与实时事件共用同一 payload 归一函数。
 */

export const PALETTE_FOCUS_EVENT = 'od:palette-focus';
export const PALETTE_CONFIG_CHANGED_EVENT = 'od:palette-config-changed';

export type PaletteFocusId = 'new-task-name' | 'register-project-name';

export type PaletteFocusPayload =
  | { projectName?: undefined; projectID?: undefined }
  | { projectName: string; projectID?: string };

export type PaletteFocusDetail = { id: PaletteFocusId } & PaletteFocusPayload;

export type PaletteMatchMode = 'exact' | 'exact-then-substring';

/** 指令触发词可绑定的面板指令 ID（恰 8 个，与静态入口 + 注册项目操作一一对应）。 */
export type PaletteCommandId =
  | 'command-center'
  | 'projects'
  | 'settings-appearance'
  | 'settings-env'
  | 'settings-opencode'
  | 'settings-ai'
  | 'settings-palette'
  | 'register-project';

export type PaletteConfig = {
  hotkey: string;
  triggerWord: string;
  matchMode: PaletteMatchMode;
  commandTriggers: Record<PaletteCommandId, string>;
};

/** 指令触发词默认词表：cc/pro/reg 默认启用，设置类 5 键默认空字符串（空 = 未启用）。 */
export const DEFAULT_COMMAND_TRIGGERS: Record<PaletteCommandId, string> = {
  'command-center': 'cc',
  projects: 'pro',
  'settings-appearance': '',
  'settings-env': '',
  'settings-opencode': '',
  'settings-ai': '',
  'settings-palette': '',
  'register-project': 'reg',
};

type Pending = { id: PaletteFocusId; payload: PaletteFocusPayload };

let pending: Pending | null = null;

/** 非法 `{projectID}`（无 projectName）与非对象归一为 `{}`。 */
export function normalizePaletteFocusPayload(raw: unknown): PaletteFocusPayload {
  if (raw == null || typeof raw !== 'object' || Array.isArray(raw)) return {};
  const rec = raw as Record<string, unknown>;
  if (typeof rec.projectName !== 'string') return {};
  if (typeof rec.projectID === 'string') {
    return { projectName: rec.projectName, projectID: rec.projectID };
  }
  return { projectName: rec.projectName };
}

export function readPaletteFocusDetail(detail: unknown): { id: PaletteFocusId; payload: PaletteFocusPayload } | null {
  if (detail == null || typeof detail !== 'object' || Array.isArray(detail)) return null;
  const id = (detail as { id?: unknown }).id;
  if (id !== 'new-task-name' && id !== 'register-project-name') return null;
  return { id, payload: normalizePaletteFocusPayload(detail) };
}

/** 派发 focus 意图（事件 + pending 兜底）。
 *  document 缺失时（Node 测试）仅写 pending，不抛。 */
export function emitPaletteFocus(id: PaletteFocusId, payload?: PaletteFocusPayload): void {
  const normalized = normalizePaletteFocusPayload(payload);
  pending = { id, payload: normalized };
  if (typeof document === 'undefined') return;
  const detail = { id, ...normalized } as PaletteFocusDetail;
  document.dispatchEvent(new CustomEvent(PALETTE_FOCUS_EVENT, { detail }));
}

/** 页面 mount / 监听时消费在途 focus；匹配无 payload 返回 `{}`，无匹配返回 null。 */
export function consumePendingPaletteFocus(expected: PaletteFocusId): PaletteFocusPayload | null {
  if (!pending || pending.id !== expected) return null;
  const payload = pending.payload;
  pending = null;
  return payload;
}

/** 事件已被实时 listener 处理后清 pending，避免 mount 再消费一次。 */
export function clearPendingPaletteFocus(id?: PaletteFocusId): void {
  if (id === undefined || pending?.id === id) pending = null;
}

/** 仅供测试：重置 pending 状态。 */
export function __resetPaletteFocusForTest(): void {
  pending = null;
}
