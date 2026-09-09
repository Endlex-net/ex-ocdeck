/**
 * 常用工具快捷打开偏好（add-frontend-tool-quick-open design.md D1/D3）：
 * localStorage 持久化 + 编辑器唤起 URI 构造，纯函数可测（参照 terminal/preferences.ts 模式）。
 * 读取侧遇到损坏/非法数据只回退默认，MUST NOT 改写 localStorage。
 * 写入侧 setItem 成功才派发 EDITOR_TOOLS_CHANGED，失败向上抛出、不派发。
 */

export type EditorTool = 'vscode' | 'goland';

export interface EditorTools {
  vscode: boolean;
  goland: boolean;
}

export const VSCODE_KEY = 'ocdeck.editorTools.vscode';
export const GOLAND_KEY = 'ocdeck.editorTools.goland';
/** 默认工具键唯一写入口 = saveDefaultTool（仅快捷打开下拉显式选择调用）。 */
export const DEFAULT_KEY = 'ocdeck.editorTools.defaultTool';
/** 自定义唤起 URI 模板键（可选，spec「自定义唤起 URI 模板」）。 */
export const VSCODE_URI_TEMPLATE_KEY = 'ocdeck.editorTools.uriTemplate.vscode';
export const GOLAND_URI_TEMPLATE_KEY = 'ocdeck.editorTools.uriTemplate.goland';
export const EDITOR_TOOLS_CHANGED = 'ocdeck-editor-tools-changed'; // window CustomEvent 名

/** 默认编辑器回退顺序（spec：VSCode → GoLand 取首个已启用）。 */
const TOOL_ORDER: readonly EditorTool[] = ['vscode', 'goland'];

/** 各工具允许的模板 scheme 前缀（白名单同时是安全边界：防 javascript: 等危险 scheme 经 assign 执行）。 */
const TOOL_TEMPLATE_SCHEMES: Record<EditorTool, readonly string[]> = {
  vscode: ['vscode://', 'vscode-insiders://'],
  goland: ['goland://', 'jetbrains://'],
};

function toolKey(tool: EditorTool): string {
  return tool === 'vscode' ? VSCODE_KEY : GOLAND_KEY;
}

function uriTemplateKey(tool: EditorTool): string {
  return tool === 'vscode' ? VSCODE_URI_TEMPLATE_KEY : GOLAND_URI_TEMPLATE_KEY;
}

/** 读取两个工具开关；仅 '1' 视为开启，其余/缺省/读取异常按关闭处理，不改写 localStorage。 */
export function loadEditorTools(): EditorTools {
  const tools: EditorTools = { vscode: false, goland: false };
  for (const tool of TOOL_ORDER) {
    try {
      tools[tool] = localStorage.getItem(toolKey(tool)) === '1';
    } catch {
      /* localStorage 不可用时按关闭处理 */
    }
  }
  return tools;
}

/** 保存开关：开写 '1' 关写 '0'；只写对应开关键，MUST NOT 触碰 DEFAULT_KEY（默认回退是派生状态）。失败向上抛出、不派发。 */
export function saveEditorTool(tool: EditorTool, enabled: boolean): void {
  localStorage.setItem(toolKey(tool), enabled ? '1' : '0');
  window.dispatchEvent(new CustomEvent(EDITOR_TOOLS_CHANGED));
}

/** 读取存储默认；非法值/读取异常返回 null 按无记录处理，不改写 localStorage、不抛至渲染层。 */
export function loadDefaultTool(): EditorTool | null {
  try {
    const raw = localStorage.getItem(DEFAULT_KEY);
    if (raw === 'vscode' || raw === 'goland') return raw;
  } catch {
    /* localStorage 不可用时按无记录处理 */
  }
  return null;
}

/** 写默认工具（DEFAULT_KEY 唯一写入口）：setItem 成功才派发，失败向上抛出、不派发。 */
export function saveDefaultTool(tool: EditorTool): void {
  localStorage.setItem(DEFAULT_KEY, tool);
  window.dispatchEvent(new CustomEvent(EDITOR_TOOLS_CHANGED));
}

/** 默认编辑器解析（纯函数）：stored 合法且已启用 → stored；否则按 vscode → goland 取首个已启用；无则 null。 */
export function resolveDefaultTool(tools: EditorTools, stored: EditorTool | null): EditorTool | null {
  if (stored !== null && tools[stored]) return stored;
  for (const tool of TOOL_ORDER) {
    if (tools[tool]) return tool;
  }
  return null;
}

/**
 * 模板合法性判定（spec「自定义唤起 URI 模板」）：含至少一个 {path} 占位符，
 * 且以该工具允许的 scheme 前缀开头（vscode: vscode:// / vscode-insiders://，goland: goland:// / jetbrains://）。
 */
export function isValidUriTemplate(tool: EditorTool, template: string): boolean {
  return template.includes('{path}') && TOOL_TEMPLATE_SCHEMES[tool].some((s) => template.startsWith(s));
}

/** 读取自定义模板：缺省/空串/读取异常返回 null 按未设置处理，不改写 localStorage、不抛至渲染层。 */
export function loadEditorUriTemplate(tool: EditorTool): string | null {
  try {
    const raw = localStorage.getItem(uriTemplateKey(tool));
    if (raw !== null && raw !== '') return raw;
  } catch {
    /* localStorage 不可用时按未设置处理 */
  }
  return null;
}

/** 保存模板：非空写原文、空串清除（removeItem）；成功才派发，失败向上抛出、不派发。不校验合法性（非法模板读取时按未设置回退）。 */
export function saveEditorUriTemplate(tool: EditorTool, template: string): void {
  if (template === '') {
    localStorage.removeItem(uriTemplateKey(tool));
  } else {
    localStorage.setItem(uriTemplateKey(tool), template);
  }
  window.dispatchEvent(new CustomEvent(EDITOR_TOOLS_CHANGED));
}

/**
 * 模板路径注入值（design D7）：归一化路径按 / 分段逐段编码（Windows 盘符首段冒号保留字面）、
 * 以 / 连接、保留开头 /、不做尾斜杠收敛（与内置 VSCode 分支的差异仅在首尾斜杠处理）。
 */
function encodeTemplatePath(normalized: string): string {
  return normalized
    .split('/')
    .map((seg, i) => (i === 0 && /^[A-Za-z]:$/.test(seg) ? seg : encodeURIComponent(seg)))
    .join('/');
}

/**
 * 编辑器唤起 URI 构造（design.md D3/D7，spec 逐字同一契约）：
 * template 合法（含 {path} 且 scheme 前缀属于该工具族）→ 全部 {path} 替换为注入值，
 * 不再对替换结果做任何编码；非法/缺省 → 内置分支（行为不变）：
 * 公共归一化仅反斜杠转 '/'；
 * VSCode 分支：按 '/' 分段编码（盘符首段冒号保留字面），去开头 '/' 拼到 vscode://file/ 后，
 * 尾部 '/' 收敛为恰好一个（该收敛仅属于 VSCode 分支）；
 * GoLand 分支：对归一化路径整体一次 encodeURIComponent（不复用/不叠加分段编码）；
 * path 为空返回空串（调用侧不触发唤起）。
 */
export function buildEditorUri(tool: EditorTool, path: string, template?: string | null): string {
  if (path === '') return '';
  const normalized = path.replace(/\\/g, '/');
  if (template != null && isValidUriTemplate(tool, template)) {
    return template.split('{path}').join(encodeTemplatePath(normalized));
  }
  if (tool === 'goland') return `goland://open?file=${encodeURIComponent(normalized)}`;
  const encoded = normalized
    .split('/')
    .map((seg, i) => (i === 0 && /^[A-Za-z]:$/.test(seg) ? seg : encodeURIComponent(seg)))
    .join('/');
  return `vscode://file/${encoded.replace(/^\/+/, '').replace(/\/+$/, '')}/`;
}
